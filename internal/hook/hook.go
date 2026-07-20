// Package hook — Claude Code 훅 서브프로세스(`context-router hook`) 진입점: stdin 이벤트 1건을
// cc: 세션에 append하거나 fail-open으로 drop한다. 설계서 §2(훅 아키텍처·세션 식별·fail-open·
// deadline). MCP 서버 경유 없음 — cli 평면에서 session/ident를 직접 소비한다(계측 매핑은 T5).
package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/ingest"
	"github.com/wotjr1649/context-router/internal/session"
)

// hookInput — Claude Code 훅 stdin JSON(설계 §2, T0 골든 픽스처 형태 그대로). tool_input·
// tool_response는 도구별 중첩 객체라 RawMessage로 지연 파싱한다(계측 매핑은 T5). 도구 실패는
// 별도 이벤트 PostToolUseFailure로 오며 tool_response 대신 error/is_interrupt를 담는다(T0 README).
type hookInput struct {
	HookEventName string          `json:"hook_event_name"`
	SessionID     string          `json:"session_id"`
	CWD           string          `json:"cwd"`
	Source        string          `json:"source"`        // SessionStart 전용
	ToolName      string          `json:"tool_name"`     // Pre/PostToolUse(Failure)
	ToolInput     json.RawMessage `json:"tool_input"`    // 도구별 하위 필드(T5)
	ToolResponse  json.RawMessage `json:"tool_response"` // PostToolUse(성공)만
	Error         string          `json:"error"`         // PostToolUseFailure만
	IsInterrupt   bool            `json:"is_interrupt"`  // PostToolUseFailure(선택)
}

// canonicalUUIDRe — session_id의 canonical(8-4-4-4-12 하이픈) UUID 형식 검증(설계 §2.2).
// google/uuid.Parse는 중괄호·urn·32-hex 변형까지 수용해 "canonical" 계약보다 느슨하므로 쓰지
// 않는다 — cc:<uuid> 세션 식별자의 형태 안정성을 위해 정확히 canonical 형태만 채택한다.
var canonicalUUIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

const (
	defaultDeadlineMS   = 2000
	defaultRetentionSec = 2592000 // 30일 — 훅 세션 기본 retention(설계 §2.2)
	dropsLogName        = "session.drops.log"
)

// Run — 훅 이벤트 1건을 처리한다(설계 §2). **항상 0을 반환한다**(fail-open §2.3): 어떤 실패도
// exit 0 + drops 1줄로 흡수하고 호스트에 오류를 전파하지 않는다. 순서: CTR_HOOKS_OFF → stdin
// 드레인 → 파싱 → session_id canonical 검증 → cc: 조립 → cwd로 session dir 도출 → deadline ctx →
// SessionStart=EnsureSession / 그 외=SessionExists 판정. stdout은 guard(T7)의 permissionDecision
// JSON 전용이라 골격에서는 미사용. getenv는 테스트 주입점.
func Run(ctx context.Context, stdin io.Reader, stdout io.Writer, storeRoot, version string, getenv func(string) string) int {
	if getenv("CTR_HOOKS_OFF") == "1" {
		_, _ = io.Copy(io.Discard, stdin) // 소비 후 exit — broken pipe 방지(설계 §2.3)
		return 0
	}
	data, err := io.ReadAll(stdin) // ReadAll이 EOF까지 소비 → broken pipe 방지 + 파싱 입력 확보
	if err != nil {
		appendDrop(storeRoot, "bad-input")
		return 0
	}
	var in hookInput
	if json.Unmarshal(data, &in) != nil {
		appendDrop(storeRoot, "bad-input")
		return 0
	}
	if !canonicalUUIDRe.MatchString(in.SessionID) {
		appendDrop(storeRoot, "bad-session-id") // 식별 전 단계 — storeRoot 레벨 사이드카
		return 0
	}
	canon, err := ident.Canonicalize(in.CWD)
	if err != nil {
		appendDrop(storeRoot, "bad-cwd") // worktree 미식별 — storeRoot 레벨 사이드카
		return 0
	}
	dir := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)
	// content store dir는 프로젝트 레벨(worktree 하위 아님) — main의 content.db join과 동일(설계 §5).
	contentDir := filepath.Join(storeRoot, "projects", canon.ProjectID)

	ctx, cancel := context.WithTimeout(ctx, deadline(getenv))
	defer cancel()
	dispatch(ctx, in, dir, contentDir, canon.WorktreeRoot, "cc:"+in.SessionID, version, getenv)
	return 0
}

// dispatch — 세션 식별 완료 후의 open·branch 경로(설계 §2.2). Run에서 분리해 핸들러 길이 규율
// (≤50줄, D13)을 지키고 T5~T7이 합류할 단일 이음새를 만든다. 모든 실패는 drop 후 조용히 반환한다
// (Run이 항상 0을 반환하는 fail-open 계약의 연장).
func dispatch(ctx context.Context, in hookInput, dir, contentDir, worktreeRoot, external, version string, getenv func(string) string) {
	ad, err := session.OpenAppend(ctx, dir, session.AppendOptions{
		ExternalSessionID: external,
		Producer:          fmt.Sprintf("context-router/%s", version),
		RetentionSec:      retentionSec(getenv),
	})
	if err != nil {
		appendDrop(dir, openErrReason(err))
		return
	}
	defer func() { _ = ad.Close() }()

	if in.HookEventName == "SessionStart" {
		if _, err := ad.EnsureSession(ctx, in.Source, worktreeRoot); err != nil {
			appendDrop(dir, "ensure-failed")
		}
		return
	}

	exists, err := ad.SessionExists(ctx)
	if err != nil {
		appendDrop(dir, "session-check-failed")
		return
	}
	if !exists {
		appendDrop(dir, "unknown-session") // 미지 세션의 후속 이벤트는 drop(설계 §2.2)
		return
	}
	// PostToolUse/PostToolUseFailure만 기본 이벤트 1건으로 기록한다(1 호출 = 1 이벤트; PreToolUse는
	// T7 guard 몫이라 여기서 tool_call로 중복 계상하지 않는다). 설계 §3·§2.2 "true → 처리".
	if in.HookEventName == "PostToolUse" || in.HookEventName == "PostToolUseFailure" {
		if ev, ok := buildEvent(in); ok {
			if _, _, _, err := ad.Append(ctx, ev); err != nil {
				appendDrop(dir, "append-failed")
			}
		}
		// T6 Shadow Recall — 성공 이벤트만(PostToolUseFailure는 tool_response가 없다). 기본 이벤트에
		// 더해 조건부 artifact_created·tool_result_summary를 append한다(설계 §5). T7 guard도 합류 예정.
		if in.HookEventName == "PostToolUse" {
			shadowCapture(ctx, ad, in, dir, contentDir, external, getenv)
		}
	}
}

// toolInputFields — classify가 소비하는 tool_input 하위 필드만(allowlist). command는 Bash,
// file_path/notebook_path는 파일 편집 도구 경로. 나머지(content 등 원문)는 파싱하지 않는다(§3 원문 미수용).
type toolInputFields struct {
	Command      string `json:"command"`
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
}

// bashClassifiers — Bash 명령 → event_type 분류 패턴표(설계 §3, 테이블 테스트가 계약). 슬라이스
// 순서가 우선순위다(git > build > test). 미매치는 tool_call. RE2 컴파일이라 ReDoS 없음.
var bashClassifiers = []struct {
	re        *regexp.Regexp
	eventType string
}{
	{regexp.MustCompile(`^git (diff|commit|merge|rebase|log|status)`), "git_diff"},
	{regexp.MustCompile(`go build|dotnet build|npm run build|msbuild|make(\s|$)`), "build_run"},
	{regexp.MustCompile(`go test|dotnet test|pytest|vitest|npm test`), "test_run"},
}

// bashTokenRe — Bash 첫 토큰이 명령 단어 형태([A-Za-z0-9_./-]+)인지. env 할당 `KEY=값`·비정형
// 토큰은 불일치 → `<arg>`로 마스킹(설계 §3 allowlist ①).
var bashTokenRe = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

// errCodeRe — PostToolUseFailure `error` 문자열에서 종료 코드 숫자만 추출한다(원문 미수용 §3 —
// 추출한 숫자 외 어떤 바이트도 요약에 복사하지 않는다).
var errCodeRe = regexp.MustCompile(`(?i)(?:status code|exit code|code)\s+(\d+)`)

// classify — 훅 이벤트 1건을 (event_type, summary, attrs)로 매핑한다(설계 §3). 우선순위:
// error(PostToolUseFailure 이벤트명 기준 — 응답 파싱 아님, T0) > git_diff/build_run/test_run
// (Bash 패턴표) > file_edit(Write/Edit/NotebookEdit) > tool_call. summary는 `<도구명>: <허용
// 요소>`로만 조립하고 원시 인자·오류 전문·응답 본문은 넣지 않는다(summary는 FTS 색인 대상 —
// 비밀 운반 차단 1차 방어). 순수 함수(테이블 테스트가 계약)이며 파일 상대 경로 기준은 in.CWD다
// (worktreeRoot와 동일 워크스페이스 디렉터리 — canon Fold/RealPath는 store-id 안정화 전용이라
// 표시 경로에는 불필요). attrs는 allowlist 필드만(exit_code·is_interrupt) 채운다.
func classify(in hookInput) (eventType, summary string, attrs map[string]any) {
	if in.HookEventName == "PostToolUseFailure" {
		element, a := classifyError(in.Error, in.IsInterrupt)
		return "error", summaryLine(in.ToolName, element), a
	}
	switch in.ToolName {
	case "Bash":
		var f toolInputFields
		_ = json.Unmarshal(in.ToolInput, &f) // 파싱 실패 시 빈 command → 첫 토큰 <arg>·tool_call
		et := "tool_call"
		for _, c := range bashClassifiers {
			if c.re.MatchString(f.Command) {
				et = c.eventType
				break
			}
		}
		return et, summaryLine("Bash", bashFirstToken(f.Command)), nil
	case "Write", "Edit", "NotebookEdit":
		var f toolInputFields
		_ = json.Unmarshal(in.ToolInput, &f)
		path := f.FilePath
		if path == "" {
			path = f.NotebookPath
		}
		return "file_edit", summaryLine(in.ToolName, workspaceRel(in.CWD, path)), nil
	default:
		return "tool_call", summaryLine(in.ToolName, ""), nil
	}
}

// classifyError — `error` 문자열·is_interrupt를 정규화 분류·코드로 매핑한다(설계 §3, 원문 미수용).
func classifyError(errStr string, isInterrupt bool) (element string, attrs map[string]any) {
	if isInterrupt {
		return "interrupted", map[string]any{"is_interrupt": true}
	}
	if m := errCodeRe.FindStringSubmatch(errStr); m != nil {
		n, _ := strconv.Atoi(m[1])
		return "exit " + m[1], map[string]any{"exit_code": n}
	}
	return "failed", nil
}

// bashFirstToken — 명령 첫 토큰을 원문(명령 단어 형태일 때만) 또는 `<arg>`로 돌려준다(설계 §3 ①).
func bashFirstToken(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) == 0 || !bashTokenRe.MatchString(fields[0]) {
		return "<arg>"
	}
	return fields[0]
}

// workspaceRel — path를 base 기준 상대 경로(슬래시)로 만든다(설계 §3 ②, §5.5 절대경로 미노출).
// 상대화 불가(다른 드라이브 등)면 base name만 남긴다.
func workspaceRel(base, path string) string {
	if path == "" {
		return ""
	}
	rel, err := filepath.Rel(base, path)
	if err != nil {
		rel = filepath.Base(path)
	}
	return filepath.ToSlash(rel)
}

// summaryLine — `<도구명>: <요소>` 조립(설계 §3). 도구명/요소가 비면 존재하는 쪽만 남긴다.
func summaryLine(tool, element string) string {
	switch {
	case tool == "":
		return element
	case element == "":
		return tool
	default:
		return tool + ": " + element
	}
}

// buildEvent — classify 결과에 2차 방어 ingest.Redact와 상한 절단을 적용해 저장용 Event를 만든다
// (설계 §3). Redact가 스팬을 가리면 redaction="spans"로 기록한다. ok=false면 기록하지 않는다
// (빈 요약 = ValidateEvent 거부 예상 케이스 — fail-open으로 무시).
func buildEvent(in hookInput) (session.Event, bool) {
	eventType, summary, attrs := classify(in)
	redaction := "none"
	if red, spans := ingest.Redact([]byte(summary)); spans > 0 {
		summary, redaction = string(red), "spans"
	}
	summary = truncateUTF8(summary, session.MaxSummaryBytes)
	if summary == "" {
		return session.Event{}, false
	}
	var attrsJSON json.RawMessage
	if len(attrs) > 0 {
		if b, err := json.Marshal(attrs); err == nil {
			attrsJSON = b
		}
	}
	return session.Event{Type: eventType, Summary: summary, Attributes: attrsJSON, Redaction: redaction}, true
}

// truncateUTF8 — s를 max바이트 이하로 rune 경계에서 절단한다(설계 §3 "기존 상한 안에서 절단").
func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// deadline — 훅 총 예산(설계 §2.3, CTR_HOOK_DEADLINE_MS 기본 2000ms). 파싱 불가·비양수는 기본값.
func deadline(getenv func(string) string) time.Duration {
	ms := defaultDeadlineMS
	if v, err := strconv.Atoi(getenv("CTR_HOOK_DEADLINE_MS")); err == nil && v > 0 {
		ms = v
	}
	return time.Duration(ms) * time.Millisecond
}

// retentionSec — 훅 세션 기본 retention(설계 §2.2, CTR_HOOK_RETENTION_SEC 기본 30일). 0=무기한은
// 명시 허용, 음수·파싱 불가는 기본값으로 복귀한다.
func retentionSec(getenv func(string) string) int64 {
	if v, err := strconv.ParseInt(getenv("CTR_HOOK_RETENTION_SEC"), 10, 64); err == nil && v >= 0 {
		return v
	}
	return defaultRetentionSec
}

// openErrReason — OpenAppend 실패 sentinel → drops 사유(설계 §2.3 fail-open). lease 경합·복구
// 대기·그 외 불용(손상·deadline 초과·스키마 불일치)을 구분해 doctor 진단에 남긴다.
func openErrReason(err error) string {
	switch {
	case errors.Is(err, session.ErrLeaseHeld):
		return "lease-held"
	case errors.Is(err, session.ErrRecoverPending):
		return "recover-pending"
	default:
		return "open-failed"
	}
}

// appendDrop — dir/session.drops.log에 "<unix-ts>\t<reason>\n" 1줄을 O_APPEND한다(설계 §2.3
// fail-open 사이드카). dir이 아직 없을 수 있어 MkdirAll로 보강한다(best-effort). 자체 실패는
// stderr slog에만 남긴다(이중 실패 무시, 설계 §2.3 한계) — 절대경로·비밀은 남기지 않는다(§5.5).
func appendDrop(dir, reason string) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn("hook drop 기록 실패", "stage", "mkdir", "reason", reason)
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, dropsLogName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		slog.Warn("hook drop 기록 실패", "stage", "open", "reason", reason)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := fmt.Fprintf(f, "%d\t%s\n", time.Now().Unix(), reason); err != nil {
		slog.Warn("hook drop 기록 실패", "stage", "write", "reason", reason)
	}
}
