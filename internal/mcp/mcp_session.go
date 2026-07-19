// mcp_session.go — 세션 이벤트 도구 2종(ctr_record_event·ctr_session_summary, 설계 §3.1·§3.2).
// mcp.go에서 분리(코드-아키텍처 §"선호 밴드 300~1,000줄" — 세션 도구가 2종으로 늘며 mcp.go가
// 밴드를 넘어 응집된 이음새를 따라 분리, 사전 승인 3경우 중 ②).
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wotjr1649/context-router/internal/ingest"
	"github.com/wotjr1649/context-router/internal/search"
	"github.com/wotjr1649/context-router/internal/session"
	"github.com/wotjr1649/context-router/internal/store"
)

// --- ctr_record_event (설계 §3.1) ---

const (
	maxEventTypeBytes   = 64
	maxSummaryBytes     = 2048
	maxAttributesBytes  = 4096
	maxRefsOrRelated    = 16 // artifact_refs·related_resources 공용 개수 상한
	maxRelatedItemBytes = 512
	maxEventTotalBytes  = 8192
	redactedFieldMarker = "«REDACTED»" // attributes·related가 redaction 후 파싱 불가할 때의 강등 마커(평문)
)

var eventTypeRe = regexp.MustCompile(`^[a-z0-9_]+$`)

// RecordEventInput.Attributes는 map[string]any다(json.RawMessage가 아님) — jsonschema-go의
// 타입 추론이 []byte 기반 json.RawMessage를 "byte 배열"로 잘못 유추해 object 입력을 거부하는
// 문제를 피한다(실측: mcp.AddTool 반사 스키마가 attributes를 array로 선언해 정상 JSON 객체
// 호출이 클라이언트 측 스키마 검증에서 거부됨). map[string]any는 jsonschema-go에서 object로
// 정확히 추론된다.
type RecordEventInput struct {
	EventType        string         `json:"event_type" jsonschema:"이벤트 타입, [a-z0-9_]+, 최대 64바이트"`
	Summary          string         `json:"summary" jsonschema:"요약, 1~2048바이트"`
	Attributes       map[string]any `json:"attributes,omitempty" jsonschema:"JSON 객체, 직렬화 최대 4096바이트"`
	ArtifactRefs     []int64        `json:"artifact_refs,omitempty" jsonschema:"참조할 artifact ID, 최대 16개"`
	RelatedResources []string       `json:"related_resources,omitempty" jsonschema:"관련 리소스 URI(스킴 필수), 최대 16개·항목당 512바이트"`
	Supersedes       string         `json:"supersedes,omitempty" jsonschema:"교정 대상 event_id(기록 시점 존재 검증)"`
}

type RecordEventOutput struct {
	EventID   string `json:"event_id"`
	SessionID string `json:"session_id"`
	Ts        int64  `json:"ts"`
}

// validateRecordEventInput: 형식·상한 검증(설계 §3.1 Global Constraints) — event_type·summary·
// attributes(직렬화된 attrBytes)·artifact_refs 개수를 먼저 확인한 뒤, related_resources 각
// 항목의 길이·URI 스킴을 검사하며 이벤트 직렬화 총합(≤8KB)을 누적한다.
func validateRecordEventInput(in RecordEventInput, attrBytes []byte) error {
	switch {
	case len(in.EventType) > maxEventTypeBytes || !eventTypeRe.MatchString(in.EventType):
		return toolErr(codeInvalidArgument, "event_type은 [a-z0-9_]+이고 64바이트 이하여야 합니다")
	case len(in.Summary) == 0 || len(in.Summary) > maxSummaryBytes:
		return toolErr(codeInvalidArgument, "summary는 1~2048바이트여야 합니다")
	case len(attrBytes) > maxAttributesBytes:
		return toolErr(codeInvalidArgument, "attributes는 4096바이트 이하여야 합니다")
	case len(in.ArtifactRefs) > maxRefsOrRelated:
		return toolErr(codeInvalidArgument, "artifact_refs는 16개 이하여야 합니다")
	case len(in.RelatedResources) > maxRefsOrRelated:
		return toolErr(codeInvalidArgument, "related_resources는 16개 이하여야 합니다")
	}
	total := len(in.EventType) + len(in.Summary) + len(attrBytes) + len(in.Supersedes)
	for _, r := range in.RelatedResources {
		if len(r) > maxRelatedItemBytes {
			return toolErr(codeInvalidArgument, "related_resources 항목은 512바이트 이하여야 합니다")
		}
		if u, err := url.Parse(r); err != nil || u.Scheme == "" {
			return toolErr(codeInvalidArgument, "related_resources 항목은 스킴을 포함한 URI여야 합니다")
		}
		total += len(r)
	}
	if total > maxEventTotalBytes {
		return toolErr(codeInvalidArgument, "이벤트 직렬화 총합이 8192바이트를 초과했습니다")
	}
	return nil
}

// resolveArtifactRefs: artifact_id → content_hash(store.ArtifactHashByID) → 정본 URI
// `artifact://<session_id>/sha256-<hash>`(설계 §3.1 — 세션 성분은 출처 표시일 뿐, 해석 시에는
// hash만 쓴다). 미존재 id는 INVALID_ARGUMENT(store.ErrNotFound를 NOT_FOUND로 흘려보내지 않음
// — supersedes의 NOT_FOUND와 의미가 다르다).
func resolveArtifactRefs(ctx context.Context, st *store.Store, sessionID string, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	uris := make([]string, len(ids))
	for i, id := range ids {
		hash, err := st.ArtifactHashByID(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return nil, toolErr(codeInvalidArgument, fmt.Sprintf("artifact_refs[%d]=%d 없음", i, id))
		}
		if err != nil {
			return nil, toToolError(err)
		}
		uris[i] = fmt.Sprintf("artifact://%s/sha256-%s", sessionID, hash)
	}
	return uris, nil
}

// redactEventFields: summary(원문)·attributes(직렬화된 JSON)·related_resources(직렬화된 JSON
// 배열)에 각각 ingest.Redact를 적용한다(설계 §3.1·§4). attributes는 redaction 후 json.Valid로
// 재검증해 실패 시 필드 전체를 단일 redacted 문자열로 강등한다(브리프 T3 이관 계약). spans
// 합계가 하나라도 있으면 전체 이벤트 redaction을 "spans"로 표시한다.
func redactEventFields(summary string, attributes []byte, related []string) (redSummary string, redAttrs json.RawMessage, redRelated []string, redaction string) {
	sOut, spans := ingest.Redact([]byte(summary))
	redSummary = string(sOut)

	if len(attributes) > 0 {
		aOut, n := ingest.Redact(attributes)
		spans += n
		if !json.Valid(aOut) {
			aOut, _ = json.Marshal(redactedFieldMarker) // 강등 — 유효 JSON 문자열 보장
		}
		redAttrs = aOut
	}

	if len(related) > 0 {
		raw, _ := json.Marshal(related)
		rOut, n := ingest.Redact(raw)
		spans += n
		var parsed []string
		if json.Unmarshal(rOut, &parsed) == nil {
			redRelated = parsed
		} else {
			redRelated = []string{redactedFieldMarker}
		}
	}

	redaction = "none"
	if spans > 0 {
		redaction = "spans"
	}
	return redSummary, redAttrs, redRelated, redaction
}

// recordEventFromInput: attributes(map) 직렬화→검증→artifact_refs 해석→redaction까지 마친
// session.Event를 만든다(registerRecordEvent 핸들러의 ≤2 구체 호출 규약 — 여기 1개 +
// sess.Append 1개). attributes가 비어 있으면 attrBytes는 nil(직렬화 생략, "attributes 없음"과
// "attributes={}"를 구분하지 않는다 — YAGNI).
func recordEventFromInput(ctx context.Context, st *store.Store, sessionID string, in RecordEventInput) (session.Event, error) {
	var attrBytes []byte
	if len(in.Attributes) > 0 {
		b, err := json.Marshal(in.Attributes)
		if err != nil {
			return session.Event{}, toolErr(codeInvalidArgument, "attributes 직렬화에 실패했습니다")
		}
		attrBytes = b
	}
	if err := validateRecordEventInput(in, attrBytes); err != nil {
		return session.Event{}, err
	}
	refs, err := resolveArtifactRefs(ctx, st, sessionID, in.ArtifactRefs)
	if err != nil {
		return session.Event{}, err
	}
	summary, attrs, related, redaction := redactEventFields(in.Summary, attrBytes, in.RelatedResources)
	return session.Event{
		Type: in.EventType, Summary: summary, Attributes: attrs,
		ArtifactRefs: refs, Related: related, Supersedes: in.Supersedes,
		Redaction: redaction,
	}, nil
}

func registerRecordEvent(srv *mcp.Server, st *store.Store, sess *session.DB) {
	destructive := false
	mcp.AddTool(srv, &mcp.Tool{
		Name: "ctr_record_event",
		Description: "세션 이벤트 1건을 기록한다 — 이벤트는 요약+포인터다: 대용량은 ctr_index로 " +
			"저장하고 artifact_refs로 가리킨다(이벤트 직렬화 총합 8192바이트 이하).",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &destructive},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in RecordEventInput) (*mcp.CallToolResult, RecordEventOutput, error) {
		start := time.Now()
		ev, err := recordEventFromInput(ctx, st, sess.SessionID(), in)
		if err != nil {
			return nil, RecordEventOutput{}, err
		}
		_, eventID, ts, err := sess.Append(ev)
		if err != nil {
			return nil, RecordEventOutput{}, toToolError(err)
		}
		out := RecordEventOutput{EventID: eventID, SessionID: sess.SessionID(), Ts: ts}
		st.LedgerAppend("ctr_record_event", 0, jsonLen(out), time.Since(start).Milliseconds())
		return nil, out, nil
	})
}

// --- ctr_session_summary (설계 §3.2) ---

const (
	defaultSummaryLimit    = 5
	maxSummaryLimit        = 20
	defaultSummaryMaxBytes = 8192
)

type SessionSummaryInput struct {
	SessionID      string `json:"session_id,omitempty" jsonschema:"세션 ID 필터, 생략 시 worktree 전체(§2.4 기본 범위)"`
	Limit          int    `json:"limit,omitempty" jsonschema:"타입별 최대 이벤트 수, 기본 5, 최대 20"`
	MaxReturnBytes int    `json:"max_return_bytes,omitempty" jsonschema:"응답 바이트 예산, 기본 8192"`
}

type summaryArtifactRef struct {
	URI     string `json:"uri"`
	Missing bool   `json:"missing,omitempty"`
}

type summaryEvent struct {
	EventID      string               `json:"event_id"`
	SessionID    string               `json:"session_id"`
	Ts           int64                `json:"ts"`
	Summary      string               `json:"summary"`
	ArtifactRefs []summaryArtifactRef `json:"artifact_refs,omitempty"`
}

type summaryGroup struct {
	EventType string         `json:"event_type"`
	Events    []summaryEvent `json:"events"`
	Truncated bool           `json:"truncated"`
}

type SessionSummaryOutput struct {
	Checkpoint          *summaryEvent  `json:"checkpoint,omitempty"`
	CheckpointTruncated bool           `json:"checkpoint_truncated,omitempty"`
	Groups              []summaryGroup `json:"groups"`
	Untrusted           bool           `json:"untrusted"`
}

// clampSummaryLimit: 기본 5·최대 20(초과 클램프), 0 이하는 기본값.
func clampSummaryLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultSummaryLimit
	case limit > maxSummaryLimit:
		return maxSummaryLimit
	default:
		return limit
	}
}

// hashFromArtifactURI: 정본 URI `artifact://<session_id>/sha256-<hash>`(설계 §3.1)에서 hash
// 성분만 뽑는다. 형식이 다르면(방어적 — T3가 정본 URI만 저장하므로 정상 경로에선 항상 매치)
// ok=false — missing 판정 대상에서 제외한다(오탐 방지, 새 오류 코드를 만들지 않는다).
func hashFromArtifactURI(uri string) (hash string, ok bool) {
	const prefix = "sha256-"
	i := strings.LastIndex(uri, "/")
	if i < 0 || !strings.HasPrefix(uri[i+1:], prefix) {
		return "", false
	}
	return uri[i+1+len(prefix):], true
}

// toSummaryEvent: session.SummaryEvent → wire 타입(D16) 변환 + artifact_refs의 missing
// 힌트(설계 §3.2, D15 — content.db에 hash가 없으면 오류가 아니라 missing:true) 부가.
func toSummaryEvent(ctx context.Context, st *store.Store, ev session.SummaryEvent) (summaryEvent, error) {
	refs := make([]summaryArtifactRef, len(ev.ArtifactRefs))
	for i, uri := range ev.ArtifactRefs {
		missing := false
		if hash, ok := hashFromArtifactURI(uri); ok {
			exists, err := search.ArtifactHashExists(ctx, st, hash)
			if err != nil {
				return summaryEvent{}, toToolError(err)
			}
			missing = !exists
		}
		refs[i] = summaryArtifactRef{URI: uri, Missing: missing}
	}
	return summaryEvent{EventID: ev.EventID, SessionID: ev.SessionID, Ts: ev.Ts, Summary: ev.Summary, ArtifactRefs: refs}, nil
}

// buildSessionSummaryOutput: session.Summarize의 결과(개수 상한까지만 적용됨)에 바이트 예산
// 배분·checkpoint 우선순위·missing 힌트를 적용한다(순수 Go 구조체 후처리 — applyFetchBudget과
// 동형, SQL 지식 불필요). 예산 측정은 이벤트 summary 텍스트 길이만 쓴다(applyBudget의
// snippet-only 관례 승계) — ponytail: 필드별 정밀 JSON 바이트 계산은 하지 않는다, 정밀도가
// 실제로 필요해지면 그때 확장.
//
// 규칙(설계 §3.2): checkpoint가 전체 예산을 단독으로 넘으면 생략 + CheckpointTruncated=true.
// 그렇지 않으면 checkpoint를 먼저 배정하고 남은 예산을 그룹 순서대로(타입 오름차순, 그룹 내
// 시간 역순) 소진한다 — 이벤트 1건이라도 남은 예산을 넘으면 그 그룹을 truncated=true로 표시하고
// 이후 그룹은 전혀 싣지 않는다(hard cap 유지, 예산 초과 금지).
func buildSessionSummaryOutput(ctx context.Context, st *store.Store, sum session.Summary, maxReturnBytes int) (SessionSummaryOutput, error) {
	budget := maxReturnBytes
	if budget <= 0 {
		budget = defaultSummaryMaxBytes
	}
	out := SessionSummaryOutput{Untrusted: true, Groups: []summaryGroup{}}
	remaining := budget

	if sum.Checkpoint != nil {
		if ckSize := len(sum.Checkpoint.Summary); ckSize > budget {
			out.CheckpointTruncated = true
		} else {
			ck, err := toSummaryEvent(ctx, st, *sum.Checkpoint)
			if err != nil {
				return SessionSummaryOutput{}, err
			}
			out.Checkpoint = &ck
			remaining -= ckSize
		}
	}

	for _, g := range sum.Groups {
		kept := make([]summaryEvent, 0, len(g.Events))
		truncated := false
		for _, ev := range g.Events {
			if len(ev.Summary) > remaining {
				truncated = true
				break
			}
			se, err := toSummaryEvent(ctx, st, ev)
			if err != nil {
				return SessionSummaryOutput{}, err
			}
			kept = append(kept, se)
			remaining -= len(ev.Summary)
		}
		out.Groups = append(out.Groups, summaryGroup{EventType: g.EventType, Events: kept, Truncated: truncated})
		if truncated {
			break // 공유 예산 소진 — 이후 그룹은 시도하지 않는다(hard cap).
		}
	}
	return out, nil
}

func registerSessionSummary(srv *mcp.Server, st *store.Store, sess *session.DB) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ctr_session_summary",
		Description: "세션 이벤트를 타입별로 그룹핑해 요약한다 — 최신 checkpoint 전문 + 타입별 최근 이벤트(시간 역순).",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SessionSummaryInput) (*mcp.CallToolResult, SessionSummaryOutput, error) {
		start := time.Now()
		sum, err := session.Summarize(ctx, sess.Reader(), in.SessionID, clampSummaryLimit(in.Limit))
		if err != nil {
			return nil, SessionSummaryOutput{}, toToolError(err)
		}
		out, err := buildSessionSummaryOutput(ctx, st, sum, in.MaxReturnBytes)
		if err != nil {
			return nil, SessionSummaryOutput{}, err
		}
		st.LedgerAppend("ctr_session_summary", 0, jsonLen(out), time.Since(start).Milliseconds())
		return nil, out, nil
	})
}
