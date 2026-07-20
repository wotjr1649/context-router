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
	"time"

	"github.com/wotjr1649/context-router/internal/ident"
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

	ctx, cancel := context.WithTimeout(ctx, deadline(getenv))
	defer cancel()
	dispatch(ctx, in, dir, canon.WorktreeRoot, "cc:"+in.SessionID, version, getenv)
	return 0
}

// dispatch — 세션 식별 완료 후의 open·branch 경로(설계 §2.2). Run에서 분리해 핸들러 길이 규율
// (≤50줄, D13)을 지키고 T5~T7이 합류할 단일 이음새를 만든다. 모든 실패는 drop 후 조용히 반환한다
// (Run이 항상 0을 반환하는 fail-open 계약의 연장).
func dispatch(ctx context.Context, in hookInput, dir, worktreeRoot, external, version string, getenv func(string) string) {
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
	// T5(계측 매핑)·T6(Shadow Recall)·T7(guard) 이음새 — 골격은 알려진 세션 non-SessionStart
	// 이벤트를 no-op으로 통과시킨다(설계 §2.2 "true → 처리").
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
