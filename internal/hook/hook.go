// Package hook — Claude Code 훅 서브프로세스(`context-router hook`) 진입점: stdin 이벤트 1건을
// 호스트 접두(cc:/cx:) 세션에 append하거나 fail-open으로 drop한다. 설계서 §2(훅 아키텍처·세션 식별·fail-open·
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
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/ingest"
	"github.com/wotjr1649/context-router/internal/session"
	"github.com/wotjr1649/context-router/internal/store"
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
	AgentID       json.RawMessage `json:"agent_id"`      // D53 — RawMessage 지연 관대 파싱(스펙 v0.10 §0: string 필드 금지)
	AgentType     json.RawMessage `json:"agent_type"`    // D53 — 〃
}

// canonicalUUIDRe — session_id의 canonical(8-4-4-4-12 하이픈) UUID 형식 검증(설계 §2.2).
// google/uuid.Parse는 중괄호·urn·32-hex 변형까지 수용해 "canonical" 계약보다 느슨하므로 쓰지
// 않는다 — cc:<uuid> 세션 식별자의 형태 안정성을 위해 정확히 canonical 형태만 채택한다.
var canonicalUUIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Host — 훅 이벤트의 발신 호스트(설계 v0.4 §2 D35). 값이 곧 세션 네임스페이스 접두다.
// 진입점은 명시적 호스트 인자로 분기한다 — 암묵 재사용으로 인한 cc/cx 오귀속 금지.
type Host string

const (
	HostClaude Host = "cc" // Claude Code
	HostCodex  Host = "cx" // Codex CLI (§11.1 G1 — Claude 동형 훅 페이로드)
)

const (
	defaultDeadlineMS   = 2000
	defaultRetentionSec = 2592000 // 30일 — 훅 세션 기본 retention(설계 §2.2)
	dropsLogName        = "session.drops.log"
	maxStdinBytes       = 8 << 20       // stdin 읽기 상한(fail-open 봉인) — CTR_SHADOW_MAX 1MiB + JSON 이스케이프·대형 Write tool_input 여유
	maxSourceBytes      = 64            // SessionStart source 길이 봉인(최종 리뷰 C4) — 문서 enum 최대 7B, 여유 포함
	sourceFirstEvent    = "first-event" // D51 합성 등록 마커 — session_start payload source(스펙 v0.9 §0)
)

// Run — 훅 이벤트 1건을 처리한다(설계 §2). **항상 0을 반환한다**(fail-open §2.3): 어떤 실패도
// exit 0 + drops 1줄로 흡수하고 호스트에 오류를 전파하지 않는다. 순서: CTR_HOOKS_OFF → host 검증 →
// stdin 드레인 → 파싱 → session_id canonical 검증 → 호스트 접두(cc:/cx:) 조립 → cwd로 session dir
// 도출 → deadline ctx → SessionStart=EnsureSession / 그 외=SessionExists 판정. host는 명시적 발신
// 호스트(D35) — 세션 네임스페이스 접두라 미지 값은 오귀속 대신 drop한다. stdout은 guard(T7)의
// permissionDecision JSON 전용이라 골격에서는 미사용. getenv는 테스트 주입점.
func Run(ctx context.Context, stdin io.Reader, stdout io.Writer, storeRoot, version string, host Host, getenv func(string) string) int {
	if getenv("CTR_HOOKS_OFF") == "1" {
		_, _ = io.Copy(io.Discard, stdin) // 소비 후 exit — broken pipe 방지(설계 §2.3)
		return 0
	}
	if host != HostClaude && host != HostCodex {
		_, _ = io.Copy(io.Discard, stdin)             // drain — broken pipe 방지
		appendDrop(storeRoot, "bad-host", "", "", "") // 오귀속 대신 drop(D35 격리)
		return 0
	}
	data, err := io.ReadAll(io.LimitReader(stdin, maxStdinBytes+1)) // 상한+1로 초과 감지, 정상 크기는 EOF까지 소비
	if err != nil {
		appendDrop(storeRoot, "bad-input", "", "", "")
		return 0
	}
	if len(data) > maxStdinBytes { // deadline·파싱 이전에 봉인 — 거대 payload OOM 방지(fail-open)
		_, _ = io.Copy(io.Discard, stdin) // 남은 stdin drain → broken pipe 방지(HOOKS_OFF와 동형)
		appendDrop(storeRoot, "stdin-oversize", "", "", "")
		return 0
	}
	var in hookInput
	if json.Unmarshal(data, &in) != nil {
		appendDrop(storeRoot, "bad-input", "", "", "")
		return 0
	}
	if !canonicalUUIDRe.MatchString(in.SessionID) {
		appendDrop(storeRoot, "bad-session-id", "", in.HookEventName, in.ToolName) // 식별 전 단계 — storeRoot 레벨 사이드카
		return 0
	}
	canon, err := ident.Canonicalize(in.CWD)
	if err != nil {
		appendDrop(storeRoot, "bad-cwd", "", in.HookEventName, in.ToolName) // worktree 미식별 — storeRoot 레벨 사이드카
		return 0
	}
	dir := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)
	// content store dir는 프로젝트 레벨(worktree 하위 아님) — main의 content.db join과 동일(설계 §5).
	contentDir := filepath.Join(storeRoot, "projects", canon.ProjectID)

	ctx, cancel := context.WithTimeout(ctx, deadline(getenv))
	defer cancel()
	dispatch(ctx, in, dir, contentDir, canon.WorktreeRoot, host, version, getenv, stdout)
	return 0
}

// dispatch — 세션 식별 완료 후의 open·branch 경로(설계 §2.2). Run에서 분리해 핸들러 길이 규율
// (≤50줄, D13)을 지키고 T5~T7이 합류할 단일 이음새를 만든다. 모든 실패는 drop 후 조용히 반환한다
// (Run이 항상 0을 반환하는 fail-open 계약의 연장).
func dispatch(ctx context.Context, in hookInput, dir, contentDir, worktreeRoot string, host Host, version string, getenv func(string) string, stdout io.Writer) {
	external := string(host) + ":" + in.SessionID // 세션 네임스페이스 접두(D35) — host는 이제 명시 파라미터
	ad, err := session.OpenAppend(ctx, dir, session.AppendOptions{
		ExternalSessionID: external,
		Producer:          fmt.Sprintf("context-router/%s", version),
		RetentionSec:      retentionSec(getenv),
	})
	if err != nil {
		appendDrop(dir, openErrReason(err), external, in.HookEventName, in.ToolName)
		return
	}
	defer func() { _ = ad.Close() }()

	if in.HookEventName == "SessionStart" {
		// source는 신뢰 불가 stdin 문자열(최종 리뷰 C4) — EnsureSession은 서버 발행 고정 이벤트라
		// ValidateEvent를 우회하므로 여기서 길이만 봉인한다(거대 source의 payload 상한 우회 차단).
		// enum 강제는 안 한다: 호스트가 신형 source 값을 추가하면 세션 기록 전체가 사장된다(전방 호환).
		src := truncateUTF8(in.Source, maxSourceBytes) // C4: rune 경계 절단(byte-slice는 멀티바이트 source를 깨뜨림)
		if _, err := ad.EnsureSession(ctx, src, worktreeRoot); err != nil {
			appendDrop(dir, "ensure-failed", external, in.HookEventName, in.ToolName)
		}
		return
	}

	exists, err := ad.SessionExists(ctx)
	if err != nil {
		appendDrop(dir, "session-check-failed", external, in.HookEventName, in.ToolName)
		return
	}
	if !exists {
		// D51 register-on-first-event(v0.9 §0): 미지 세션은 drop 대신 합성 등록 후 그대로 계속
		// 처리한다(트리거 이벤트 포함). EnsureSession은 INSERT OR IGNORE 멱등이라 동시 경쟁 무해.
		// 등록 커밋~append 사이 실패는 기존 append-failed 계약과 동일(원자 결합 비도입 — §5).
		if _, ensErr := ad.EnsureSession(ctx, sourceFirstEvent, worktreeRoot); ensErr != nil {
			appendDrop(dir, "ensure-failed", external, in.HookEventName, in.ToolName)
			return
		}
	}
	// PreToolUse는 T7 large-read/dump guard 몫 — tool_call로 중복 계상하지 않고 tool_name으로
	// 분기한다(설계 §4 D25·D32·v0.4 D36). matcher가 Read|Bash|PowerShell라 여기 오는 건 사실상
	// 이 셋뿐이고, 각 가드가 자체 정적 판정으로 그 외를 통과시킨다.
	if in.HookEventName == "PreToolUse" {
		switch in.ToolName {
		case "Read":
			guardRead(ctx, ad, in, dir, contentDir, worktreeRoot, getenv, stdout)
		case "Bash":
			guardBash(ctx, ad, in, dir, contentDir, worktreeRoot, host, getenv, stdout)
		case "PowerShell":
			guardPowerShell(ctx, ad, in, dir, contentDir, worktreeRoot, getenv, stdout)
		}
		return
	}
	// D53 — 서브에이전트 생애주기(스펙 v0.10 §0): buildEvent→Append 경유, 1 호출 = 1 이벤트.
	if in.HookEventName == "SubagentStart" || in.HookEventName == "SubagentStop" {
		if ev, ok := buildEvent(in); ok {
			if _, _, _, err := ad.Append(ctx, ev); err != nil {
				appendDrop(dir, "append-failed", external, in.HookEventName, in.ToolName)
			}
		}
		return
	}
	// PostToolUse/PostToolUseFailure만 기본 이벤트 1건으로 기록한다(1 호출 = 1 이벤트).
	// 설계 §3·§2.2 "true → 처리".
	if in.HookEventName == "PostToolUse" || in.HookEventName == "PostToolUseFailure" {
		if ev, ok := buildEvent(in); ok {
			if _, _, _, err := ad.Append(ctx, ev); err != nil {
				appendDrop(dir, "append-failed", external, in.HookEventName, in.ToolName)
			}
		}
		// T6 Shadow Recall — 성공 이벤트만(PostToolUseFailure는 tool_response가 없다). 기본 이벤트에
		// 더해 조건부 artifact_created·tool_result_summary를 append한다(설계 §5). T7 guard도 합류 예정.
		if in.HookEventName == "PostToolUse" {
			shadowCapture(ctx, ad, in, dir, contentDir, external, getenv)
		}
	}
}

const defaultGuardReadMax = 262144 // 256KiB — large-read guard 임계 기본값(CTR_GUARD_READ_MAX, 설계 §4)

// readGuardInput — Read tool_input 중 가드가 보는 필드. offset/limit은 *포인터*라 "부재"와
// "명시적 0"을 구분한다(설계 §4 ② — offset/limit이 존재하면 부분 읽기로 보고 통과).
type readGuardInput struct {
	FilePath string `json:"file_path"`
	Offset   *int64 `json:"offset"`
	Limit    *int64 `json:"limit"`
}

// guardRead — PreToolUse(Read) large-read guard(설계 §4 D25). **deny는 4조건 전부 성립 시만**:
// ① 대상이 워크스페이스 경계 내(경계 밖은 ingest가 ErrWorkspace로 색인 불가 → 통과), ② 전체-파일
// 읽기(offset/limit 존재 = 부분 읽기 → 통과), ③ 크기 > 임계(기본 256KiB, CTR_GUARD_READ_MAX),
// ④ 현장 인덱싱 성공 확인 Indexed==1(denylist·oversize는 무오류 Skipped라 err 검사만으론 부족).
// 하나라도 불성립 → allow(stdout 무출력·이벤트 없음). worktreeRoot가 경계 기준이다 — 설계 §4의
// "projectRoot만"은 서버의 allow-path를 제외한다는 뜻이고, 경계 식별자는 ingest 관례(mcp.go)와
// 동일하게 WorktreeRoot다(ProjectRoot를 쓰면 linked worktree의 현재 파일이 경계 밖으로 오판됨).
// content store 열기 실패(락 경합 등) = 인덱싱 불가 → allow + drops(가드 판정은 색인 성공에만 발화).
func guardRead(ctx context.Context, ad *session.AppendDB, in hookInput, dir, contentDir, worktreeRoot string, getenv func(string) string, stdout io.Writer) {
	if in.ToolName != "Read" {
		return // matcher는 Read지만 stdin은 비신뢰 — 방어적 재확인, 그 외 도구는 통과
	}
	var f readGuardInput
	if json.Unmarshal(in.ToolInput, &f) != nil || f.FilePath == "" {
		return // 파싱 불가·경로 부재 → 통과
	}
	if f.Offset != nil || f.Limit != nil {
		return // ② 부분 읽기 → 통과
	}
	info, err := os.Stat(f.FilePath)
	if err != nil || info.IsDir() || info.Size() <= guardReadMax(getenv) {
		return // ③ 임계 이하·stat 불가·디렉터리 → 통과
	}
	st, err := store.OpenContext(ctx, contentDir, false)
	if err != nil {
		appendDrop(dir, "guard-store", "", in.HookEventName, in.ToolName) // 인덱싱 불가(락 경합·손상) — deadline 예산 안에서 포기
		return
	}
	defer func() { _ = st.Close() }()

	// 현장 인덱싱 — 전체 파이프라인(denylist·캡·경계 검증 포함). 경계 밖은 ErrWorkspace(err!=nil),
	// denylist·oversize는 err==nil·Indexed==0(Skipped)이라 반드시 Indexed==1까지 확인한다(④).
	rep, err := ingest.Run(ctx, st, worktreeRoot, nil, ingest.Request{Path: f.FilePath})
	if err != nil || rep.Indexed != 1 {
		return // ①④ 경계 밖·denylist·oversize·색인 실패 → 통과
	}
	denyTool(ctx, ad, in, dir, "Read", workspaceRel(in.CWD, f.FilePath)+" "+strconv.FormatInt(info.Size(), 10)+"B", stdout)
}

// guardBash — D32 Bash 단일파일 덤프 가드(guardRead의 형제, 설계 v0.3 §4·v0.7 D47). 정적
// 판정(guardDumpPath — host×GOOS 단독 게이트: cx:+Windows는 PS 게이트, 그 외는 bash 게이트)
// 성립 시에만 D25 4조건(임계 초과·경계 내·denylist 아님·현장 인덱싱 성공)을 guardRead와 동일
// 경로로 판정하고 deny한다. 그 외 전부 통과.
func guardBash(ctx context.Context, ad *session.AppendDB, in hookInput, dir, contentDir, worktreeRoot string, host Host, getenv func(string) string, stdout io.Writer) {
	var f struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(in.ToolInput, &f) != nil {
		return
	}
	path := guardDumpPath(host, runtime.GOOS, f.Command)
	if path == "" {
		return // 정적 판정 불가·비덤프·비절대 — 통과
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() <= guardReadMax(getenv) {
		return // 임계 이하·stat 불가·디렉터리 — 통과
	}
	st, err := store.OpenContext(ctx, contentDir, false)
	if err != nil {
		appendDrop(dir, "guard-store", "", in.HookEventName, in.ToolName)
		return
	}
	defer func() { _ = st.Close() }()
	rep, err := ingest.Run(ctx, st, worktreeRoot, nil, ingest.Request{Path: path})
	if err != nil || rep.Indexed != 1 {
		return // 경계 밖·denylist·oversize·색인 실패 — 통과
	}
	// warning detail: 명령·상대 파일·크기·안내 요지(설계 §4). workspaceRel이 상대화하므로
	// 절대경로는 이벤트에 실리지 않는다. deny detail의 명령 토큰: PS 게이트(cx:+Windows) 성립 시
	// 화이트리스트 4토큰 중 하나라 원문 운반이 안전(psDumpArg 계약), bash 게이트는 cat 고정(bashDumpArg 계약).
	token := "cat"
	if host == HostCodex && runtime.GOOS == "windows" {
		token = strings.Fields(f.Command)[0]
	}
	denyTool(ctx, ad, in, dir, "Bash", token+" "+workspaceRel(in.CWD, path)+" "+strconv.FormatInt(info.Size(), 10)+"B — ctr_search/ctr_fetch", stdout)
}

// guardPowerShell — D36 PowerShell 단일파일 덤프 가드(guardBash의 형제, 설계 v0.4 §3). 정적
// 판정(psDumpArg 어휘 + psAbsPath 절대경로) 성립 시에만 D25 4조건(임계 초과·경계 내·denylist
// 아님·현장 인덱싱 성공)을 guardBash와 동일 경로로 판정하고 deny한다. 그 외 전부 통과.
func guardPowerShell(ctx context.Context, ad *session.AppendDB, in hookInput, dir, contentDir, worktreeRoot string, getenv func(string) string, stdout io.Writer) {
	var f struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(in.ToolInput, &f) != nil {
		return
	}
	path := psAbsPath(runtime.GOOS, psDumpArg(f.Command))
	if path == "" {
		return // 정적 판정 불가·비덤프·비절대 — 통과
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() <= guardReadMax(getenv) {
		return // 임계 이하·stat 불가·디렉터리 — 통과
	}
	st, err := store.OpenContext(ctx, contentDir, false)
	if err != nil {
		appendDrop(dir, "guard-store", "", in.HookEventName, in.ToolName)
		return
	}
	defer func() { _ = st.Close() }()
	rep, err := ingest.Run(ctx, st, worktreeRoot, nil, ingest.Request{Path: path})
	if err != nil || rep.Indexed != 1 {
		return // 경계 밖·denylist·oversize·색인 실패 — 통과
	}
	// detail의 명령 토큰은 psDumpArg 성립 시 화이트리스트 4토큰 중 하나라 원문 운반이 안전하다.
	cmdToken := strings.Fields(f.Command)[0]
	denyTool(ctx, ad, in, dir, "PowerShell", cmdToken+" "+workspaceRel(in.CWD, path)+" "+strconv.FormatInt(info.Size(), 10)+"B — ctr_search/ctr_fetch", stdout)
}

// denyTool — 4조건 성립 시 deny 출력 헬퍼(설계 §4). stdout에 permissionDecision JSON(T0 검증
// 스키마)을 **먼저** 쓰고 그다음 warning 이벤트(호출자 조립 detail — 상대 경로·크기 등)를
// best-effort로 append한다 — 이벤트 기록 실패는 deny 판정에 영향 없다(fail-open은 기록 경로에만;
// 가드 판정은 DB 없이 성립, §4). stdout은 deny JSON 전용이라(Claude Code가 exit 0 stdout을 파싱)
// 그 외 바이트는 쓰지 않는다.
func denyTool(ctx context.Context, ad *session.AppendDB, in hookInput, dir, toolName, detail string, stdout io.Writer) {
	out := map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": "이미 인덱스됨 — ctr_search로 검색, ctr_fetch로 바이트 정확 조회",
	}}
	if b, err := json.Marshal(out); err == nil {
		_, _ = stdout.Write(b)
	}
	// summary는 allowlist 조립(도구명 + 호출자 조립 detail) — 원문 미운반. detail은 이미 워크스페이스
	// 상대화된 요소만 담는다(절대경로 미운반, §3·§5.5). 조립 후 Redact 2차 방어 + 상한 절단.
	summary := summaryLine(toolName, detail)
	if red, spans := ingest.Redact([]byte(summary)); spans > 0 {
		summary = string(red)
	}
	summary = truncateUTF8(summary, session.MaxSummaryBytes)
	if _, _, _, err := ad.Append(ctx, session.Event{Type: "warning", Summary: summary}); err != nil {
		appendDrop(dir, "guard-append", "", in.HookEventName, in.ToolName) // 기록 실패 — deny는 이미 확정, drops 1줄만
	}
}

// guardReadMax — 가드 임계(설계 §4, CTR_GUARD_READ_MAX 기본 256KiB). 양수만 채택(deadline과 동형).
func guardReadMax(getenv func(string) string) int64 {
	if v, err := strconv.ParseInt(getenv("CTR_GUARD_READ_MAX"), 10, 64); err == nil && v > 0 {
		return v
	}
	return defaultGuardReadMax
}

// bashDumpArg — D32 어휘 판정: 명령이 "단일 단순 `cat <경로>`"일 때만 경로 인자를
// 반환한다(그 외 전부 "" = allow). 비ASCII·제어문자는 bash IFS와 strings.Fields의
// 분할 규칙이 달라(NBSP 등) 오판 여지가 있으므로 전면 판정 포기. 파서는 확신이
// 있을 때만 deny하고, 오동작의 최대 피해는 "가드 미발화"다(설계 v0.3 §4·§7).
// ponytail: ~·# 전면 배제는 경로 내 정당한 문자까지 놓치는 의도적 미탐(allow 편향)
// — 실측에서 미탐이 문제되면 위치 인지 파서로 승급.
func bashDumpArg(command string) string {
	for i := 0; i < len(command); i++ {
		if command[i] < 0x20 || command[i] > 0x7e {
			return ""
		}
	}
	if strings.ContainsAny(command, "|&;<>`$(){}*?[]'\"\\~#") {
		return ""
	}
	fields := strings.Fields(command)
	if len(fields) != 2 || fields[0] != "cat" || strings.HasPrefix(fields[1], "-") {
		return ""
	}
	return fields[1]
}

// dumpAbsPath — OS 절대경로 정규화(goos는 테스트 주입점, 실호출은 runtime.GOOS).
// Windows: MSYS 형태 /x/...를 x:/... 드라이브형으로 변환 후 드라이브형만 절대로
// 인정(Go의 경로 의미론에서 /c/x는 현재 드라이브 상대 — 잘못 stat하면 오파일
// 판정이라 제외). Unix: /-접두만 절대. 상대·불명은 전부 ""(allow).
func dumpAbsPath(goos, arg string) string {
	if goos == "windows" {
		if len(arg) >= 3 && arg[0] == '/' && arg[2] == '/' &&
			((arg[1] >= 'a' && arg[1] <= 'z') || (arg[1] >= 'A' && arg[1] <= 'Z')) {
			arg = string(arg[1]) + ":" + arg[2:]
		}
		if len(arg) >= 3 && arg[1] == ':' && arg[2] == '/' &&
			((arg[0] >= 'a' && arg[0] <= 'z') || (arg[0] >= 'A' && arg[0] <= 'Z')) {
			return arg
		}
		return ""
	}
	if strings.HasPrefix(arg, "/") {
		return arg
	}
	return ""
}

// psDumpArg — D36 어휘 판정(bashDumpArg 자매): 명령이 "단일 단순 <덤프 토큰> <경로>"일 때만
// 경로 인자를 반환한다(그 외 전부 "" = allow). 덤프 토큰은 대소문자 무시 Get-Content·gc·cat·
// type. PS 메타문자(파이프·리다이렉트·서브식 $()·변수 $·배열 콤마·스플래팅 @·백틱 이스케이프·
// 주석 #·인용·와일드카드·괄호·세미콜론·~)와 비ASCII는 전면 판정 포기 — 파서는 확신이 있을
// 때만 deny하고 오동작의 최대 피해는 "가드 미발화"다(설계 v0.4 §3, bashDumpArg 원칙 승계).
// bash와 달리 백슬래시는 Windows 경로 구분자라 허용한다. 부분 읽기 플래그(-TotalCount·-Head·
// -Tail)와 명명 파라미터(-Raw·-Path 등)는 "인자 정확히 1개 + 대시 토큰 배제"에 이미 걸러진다.
// 덤프 토큰의 별칭 재정의·프로필 함수 shadow로 인한 오탐 deny는 D32 bash `cat` 셰도잉과 동일
// 클래스로 수용(§11.2 F1) — deny 시에도 대상 파일이 현장 색인돼 ctr_search/ctr_fetch로 복구
// 가능하다(비가역 아님).
func psDumpArg(command string) string {
	for i := 0; i < len(command); i++ {
		if command[i] < 0x20 || command[i] > 0x7e {
			return ""
		}
	}
	if strings.ContainsAny(command, "|&;<>`$(){}*?[]'\"~#@,") {
		return ""
	}
	fields := strings.Fields(command)
	if len(fields) != 2 || strings.HasPrefix(fields[1], "-") {
		return ""
	}
	switch strings.ToLower(fields[0]) {
	case "get-content", "gc", "cat", "type":
		return fields[1]
	}
	return ""
}

// psAbsPath — psDumpArg 인자의 절대경로 판정(dumpAbsPath 자매). PS에서 `/c/x`는 MSYS가 아니라
// "현재 드라이브 루트 상대"라 bash용 MSYS 변환을 승계하면 오파일 stat 위험(설계 §11.1 파생 ②)
// — Windows는 드라이브형(`X:\`·`X:/`)만 절대로 인정하고 goos 인자 기준 백슬래시→슬래시 치환으로
// 정규화한다(호스트 무관 — filepath.ToSlash는 호스트 OS 구분자만 바꿔 비-Windows 호스트에서 실패;
// 테스트 이식성 — 3-OS 게이트). Unix(pwsh)는 `/`-접두만 절대. 그 외 전부 ""(allow).
func psAbsPath(goos, arg string) string {
	if goos == "windows" {
		arg = strings.ReplaceAll(arg, "\\", "/")
		if len(arg) >= 3 && arg[1] == ':' && arg[2] == '/' &&
			((arg[0] >= 'a' && arg[0] <= 'z') || (arg[0] >= 'A' && arg[0] <= 'Z')) {
			return arg
		}
		return ""
	}
	if strings.HasPrefix(arg, "/") {
		return arg
	}
	return ""
}

// guardDumpPath — D47 호스트×GOOS 단독 게이트 선택(스펙 v0.7 §2). 게이트는 어휘 술어+
// 절대경로 해석이 묶인 완결 정책이라 직렬·혼합 금지: cx:+Windows는 PS 게이트(실측 §7 —
// Codex Windows exec는 raw PS 구문), 그 외(cx:+비Windows·cc: 전체)는 bash 게이트.
// 반대 방언 덤프는 miss=allow(fail-open by-design, 스펙 §2 셸 방언 한계).
func guardDumpPath(host Host, goos, command string) string {
	if host == HostCodex && goos == "windows" {
		return psAbsPath(goos, psDumpArg(command))
	}
	return dumpAbsPath(goos, bashDumpArg(command))
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
// build/test는 명령 시작(^) 또는 셸 구분자(;&|·개행) 직후에만 매치한다 — `grep "go test"`·
// `echo npm run build` 같은 인자 속 부분열 오분류를 막아 §6 사용량 계측 오염을 차단한다.
var bashClassifiers = []struct {
	re        *regexp.Regexp
	eventType string
}{
	{regexp.MustCompile(`^git (diff|commit|merge|rebase|log|status)`), "git_diff"},
	{regexp.MustCompile(`(?:^|[;&|\n]\s*)(?:go build|dotnet build|npm run build|msbuild|make(\s|$))`), "build_run"},
	{regexp.MustCompile(`(?:^|[;&|\n]\s*)(?:go test|dotnet test|pytest|vitest|npm test)`), "test_run"},
}

// bashTokenRe — Bash 첫 토큰이 명령 단어 형태([A-Za-z0-9_./-]+)인지. env 할당 `KEY=값`·비정형
// 토큰은 불일치 → `<arg>`로 마스킹(설계 §3 allowlist ①).
var bashTokenRe = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

// errCodeRe — PostToolUseFailure `error` 문자열에서 종료 코드 숫자만 추출한다(원문 미수용 §3 —
// 추출한 숫자 외 어떤 바이트도 요약에 복사하지 않는다).
var errCodeRe = regexp.MustCompile(`(?i)(?:status code|exit code|code)\s+(\d+)`)

// agentStrings — D53 관대 추출: RawMessage에서 문자열 언마샬 성공분만, 실패·부재는 빈 문자열.
// 생애주기 이벤트의 attrs는 빈 값도 기록하므로 ok 게이트가 없다(표식 게이트는 agentFields — T2).
func agentStrings(in hookInput) (id, typ string) {
	_ = json.Unmarshal(in.AgentID, &id)
	_ = json.Unmarshal(in.AgentType, &typ)
	return id, typ
}

// agentFields — D53 표식 게이트(스펙 v0.10 §0): agent_id가 비어있지 않을 때만 ok. 결손·빈 값·
// 타입 이상은 표식 생략일 뿐 기본 이벤트 처리 불변(best-effort — 재검수 P2 기전).
func agentFields(in hookInput) (id, typ string, ok bool) {
	id, typ = agentStrings(in)
	if id == "" {
		return "", "", false
	}
	return id, typ, true
}

// classify — 훅 이벤트 1건을 (event_type, summary, attrs)로 매핑한다(설계 §3). 우선순위:
// error(PostToolUseFailure 이벤트명 기준 — 응답 파싱 아님, T0) > git_diff/build_run/test_run
// (Bash 패턴표) > file_edit(Write/Edit/NotebookEdit) > tool_call. summary는 `<도구명>: <허용
// 요소>`로만 조립하고 원시 인자·오류 전문·응답 본문은 넣지 않는다(summary는 FTS 색인 대상 —
// 비밀 운반 차단 1차 방어). 순수 함수(테이블 테스트가 계약)이며 파일 상대 경로 기준은 in.CWD다
// (worktreeRoot와 동일 워크스페이스 디렉터리 — canon Fold/RealPath는 store-id 안정화 전용이라
// 표시 경로에는 불필요). attrs는 allowlist 필드만(exit_code·is_interrupt·matched_pattern) 채운다.
func classify(in hookInput) (eventType, summary string, attrs map[string]any) {
	if in.HookEventName == "SubagentStart" || in.HookEventName == "SubagentStop" {
		et, verb := "subagent_start", "started"
		if in.HookEventName == "SubagentStop" {
			et, verb = "subagent_stop", "stopped"
		}
		id, typ := agentStrings(in)
		summary := "subagent " + verb
		if typ != "" {
			summary += ": " + typ // 빈 type = 접미 생략(§3 실측 — 빈 summary 드롭 회피)
		}
		return et, summary, map[string]any{"agent_id": id, "agent_type": typ}
	}
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
		if et != "tool_call" { // T5: 매치한 패턴명(안정 enum)을 attr로 방출(설계 §3 "매치 패턴명" allowlist)
			attrs = map[string]any{"matched_pattern": et}
		}
		return et, summaryLine("Bash", bashFirstToken(f.Command)), attrs
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
	if id, typ, ok := agentFields(in); ok { // D53 표식 — PostToolUse/Failure·생애주기 공통(동일 키 덮어쓰기 무해)
		if attrs == nil {
			attrs = map[string]any{} // classify 반환 타입과 동일(검수 P1)
		}
		attrs["agent_id"], attrs["agent_type"] = id, typ
	}
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

// appendDrop — dir/session.drops.log에 진단 5필드 "<unix-ts>\t<reason>\t<sid8>\t<hook_event>\t
// <tool>\n" 1줄을 O_APPEND한다(D43, 설계 §2.3 fail-open 사이드카). sid8은 세션ID(호스트 접두 포함)
// 앞 8자, 미상 필드는 "-". sanitize: 탭·개행 제거 + 각 필드 64자 상한(파서 오염 방지, 설계 §5).
// dir이 아직 없을 수 있어 MkdirAll로 보강한다(best-effort). 자체 실패는 stderr slog에만 남긴다
// (이중 실패 무시, 설계 §2.3 한계) — 절대경로·비밀은 남기지 않는다(§5.5).
func appendDrop(dir, reason, sessionID, hookEvent, tool string) {
	san := func(s string) string {
		if s == "" {
			return "-"
		}
		s = strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(s)
		if len(s) > 64 {
			s = s[:64]
		}
		return s
	}
	sid8 := "-"
	if sessionID != "" {
		sid8 = sessionID
		if len(sid8) > 8 {
			sid8 = sid8[:8]
		}
	}
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
	if _, err := fmt.Fprintf(f, "%d\t%s\t%s\t%s\t%s\n",
		time.Now().Unix(), san(reason), san(sid8), san(hookEvent), san(tool)); err != nil {
		slog.Warn("hook drop 기록 실패", "stage", "write", "reason", reason)
	}
}
