// Package cli — hook uninstall(자기 훅 그룹 제거 한 방향·원자 쓰기·타 도구 보존) +
// running-hook 위임(--no-shadow). 설계서 §7(D28). 러닝 훅은 fail-open(§2.3)이라 인자 오류로
// 호스트를 막지 않는다.
//
// **이 프로그램이 호스트 소유 파일에 쓰는 자리는 여기 하나다**(D96 계약 1의 유일한 예외):
// `hook uninstall`이 `.claude/settings.json`과 Codex `hooks.json`에서 우리 훅 그룹을 지우는
// 방향. 등록 방향은 호스트가 플러그인 매니페스트를 읽어 스스로 한다(D96·D97) — 기입 경로가
// 파일 파괴 결함 다섯을 낳았고 `[문서]`(v0.18 §3.5·§3.7·§3.8) 그 부산물이 v0.19에서 함께
// 사라졌다.
package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wotjr1649/context-router/internal/hook"
)

const (
	hookBinaryName = "context-router"    // PATH 실행 파일명(=훅 명령 첫 토큰) — ctr은 MCP 등록 키일 뿐(설계 §7)
	dropsFileName  = "session.drops.log" // internal/hook dropsLogName(unexported) 미러 — doctor drops 집계용
)

// hookRegistrations — 제거 대상 이벤트 6개(T0-amended 설계 §7·v0.10 D53 서브에이전트 2항목).
// 옛 설치기가 이 여섯 이벤트에 우리 그룹을 하나씩 남겼으므로 uninstall은 같은 여섯을 훑는다.
// matcher는 더는 들지 않는다 — 제거는 이벤트 키로만 찾고 그룹 소유 판정은 isOurHookGroup이
// 하므로, matcher를 남기면 아무도 읽지 않는 값이 계약처럼 보인다. 등록 쪽 matcher는 이제
// 플러그인 매니페스트(hooks/hooks.json)가 든다(D96).
var hookRegistrations = []string{
	"SessionStart",
	"PreToolUse",
	"PostToolUse",
	"PostToolUseFailure",
	"SubagentStart",
	"SubagentStop",
}

func hookMarkerPrefix() string { return hookBinaryName + "/" }

// markerVersion — 소유 표식 값에서 버전을 뽑는다. **무버전 마커는 ""를 돌려준다** — 훅
// 스코프의 버전 비교가 그 값을 불일치로 읽지 않게 하는 지점이다(D82). strings.TrimPrefix는
// 무버전 마커에서 무동작이라 "context-router"를 그대로 돌려주고, doctor의
// `marker != "" && marker != version` 분기가 항상 참이 되어 상시 불일치 경고를 낸다.
func markerVersion(marker string) string {
	if marker == hookBinaryName {
		return ""
	}
	if v, ok := strings.CutPrefix(marker, hookMarkerPrefix()); ok {
		return v
	}
	return ""
}

// ownedRegistration — 등록물이 우리 소유인가. 표식 값이 소유 기준을 만족하거나 command가
// hookBinaryName이면 참이다. doctor [20]이 `.mcp.json`의 옛 등록물을 이 술어로 가려낸다 —
// 표식이 없어도 command가 우리 것이면 우리가 남긴 것이므로 잔존 보고 대상이다.
func ownedRegistration(marker, command string, found bool) bool {
	return found && (isOurMarkerValue(marker) || command == hookBinaryName)
}

// hookGroupProbe — 병합 시 그룹이 "자기 것"인지 판정하는 데 필요한 필드만 읽는다(나머지는
// 손대지 않고 raw로 보존).
type hookGroupProbe struct {
	Managed string `json:"__ctrManaged"`
	Hooks   []struct {
		Command string `json:"command"`
	} `json:"hooks"`
}

// isOurHookGroup — 자기 소유 그룹 판정: 소유권 마커(정확 일치 또는 접두, isOurMarkerValue —
// D82) AND 명령 토큰 정확 일치를 모두 요구한다(결합, 설계 §7). 마커 없는 수동 항목·접두사만
// 닮은 `context-router hook-wrapper`는 어느 한 조건에서 탈락해 보존된다(오삭제 방지).
func isOurHookGroup(raw json.RawMessage) bool {
	var p hookGroupProbe
	if json.Unmarshal(raw, &p) != nil {
		return false
	}
	if !isOurMarkerValue(p.Managed) {
		return false // 마커 없음 → 수동 항목, 보존
	}
	for _, h := range p.Hooks {
		if isHookCommandToken(h.Command) {
			return true
		}
	}
	return false
}

// isHookCommandToken — 명령을 공백 토큰화해 `context-router hook`과 정확 일치하는지(접두사
// 매칭 금지 — `context-router hook-wrapper`는 두 번째 토큰이 다르므로 불일치). 뒤에 --no-shadow·
// --store-root 등 추가 토큰이 붙어도 첫 두 토큰이 일치하면 우리 명령이다.
func isHookCommandToken(cmd string) bool {
	f := strings.Fields(cmd)
	return len(f) >= 2 && f[0] == hookBinaryName && f[1] == "hook"
}

// removeHookSettings — 기존 settings JSON을 map[string]json.RawMessage 보존 패턴으로 파싱해,
// 관리 대상 이벤트 배열에서 **자기 항목만 제거**한 뒤 재직렬화한다(설계 §7). 미지 최상위 키·타
// 도구의 훅 항목·관리 대상 아닌 이벤트는 전부 raw로 왕복 보존된다. 빈 배열이 된 이벤트는
// 제거하고, hooks 자체가 비면 최상위에서 제거한다.
//
// 방향이 하나뿐인 것이 D96 계약 1이다 — 등록(append) 갈래와 그것이 요구하던 그룹 타입·명령
// 조립·마커 값이 이 릴리스에서 함께 사라졌다.
func removeHookSettings(existing []byte) ([]byte, error) {
	settings := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := json.Unmarshal(existing, &settings); err != nil {
			return nil, err
		}
		if settings == nil { // JSON `null` → Unmarshal이 맵을 nil로 설정(할당 시 패닉, 최종 리뷰 C5)
			settings = map[string]json.RawMessage{}
		}
	}
	hooks := map[string]json.RawMessage{}
	if raw, ok := settings["hooks"]; ok {
		if err := json.Unmarshal(raw, &hooks); err != nil {
			return nil, err
		}
		if hooks == nil { // `{"hooks":null}` 동일 경로(최종 리뷰 C5)
			hooks = map[string]json.RawMessage{}
		}
	}
	for _, event := range hookRegistrations {
		var arr []json.RawMessage
		if raw, ok := hooks[event]; ok {
			if err := json.Unmarshal(raw, &arr); err != nil {
				return nil, err
			}
		}
		kept := make([]json.RawMessage, 0, len(arr))
		for _, el := range arr {
			if isOurHookGroup(el) {
				continue // 자기 항목 제거
			}
			kept = append(kept, el)
		}
		if len(kept) == 0 {
			delete(hooks, event)
			continue
		}
		b, err := json.Marshal(kept)
		if err != nil {
			return nil, err
		}
		hooks[event] = b
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		b, err := json.Marshal(hooks)
		if err != nil {
			return nil, err
		}
		settings["hooks"] = b
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil // UTF-8 no BOM, LF + 끝 개행
}

// codexRegistrations — Codex 쪽 제거 대상 이벤트 3개. 캡처 둘(D35, §11.1 G1 행 1)에 D47 가드
// 그룹의 PreToolUse를 더한 집합이다 — 옛 설치기는 가드를 MCP 확정 시에만 등록했지만 제거는
// 언제나 무조건이었으므로(제거 대칭) 셋을 한 목록으로 든다. matcher는 hookRegistrations와 같은
// 이유로 들지 않는다: 제거는 이벤트 키로 찾고 소유 판정은 isOurCodexGroup이 한다.
var codexRegistrations = []string{
	"SessionStart",
	"PostToolUse",
	"PreToolUse",
}

// codexGroupProbe — 소유 판정에 필요한 필드만(나머지는 raw 왕복 보존).
type codexGroupProbe struct {
	Hooks []struct {
		Command       string `json:"command"`
		StatusMessage string `json:"statusMessage"`
	} `json:"hooks"`
}

// isCodexHookCommandToken — `context-router codex-hook` 정확 일치(접두사 매칭 금지 —
// isHookCommandToken의 형제, 러닝 서브커맨드가 달라 별도 함수).
func isCodexHookCommandToken(cmd string) bool {
	f := strings.Fields(cmd)
	return len(f) >= 2 && f[0] == hookBinaryName && f[1] == "codex-hook"
}

// isOurCodexGroup — 그룹의 **모든** 훅 항목이 자기 것(command 토큰 정확 일치 AND statusMessage
// 마커 값 일치)일 때만 자기 그룹으로 판정한다(전건 판정 — §11.2 F4). Claude 쪽 isOurHookGroup은
// 그룹 레벨 __ctrManaged 마커가 소유를 표시하지만 Codex hooks.json은 미지 필드 금지라 항목
// 레벨 추론뿐이므로, any-판정이면 사용자가 항목을 추가한 혼합 그룹까지 통째로 지워진다 —
// 혼합 그룹은 불가침(파손 금지 > 멱등 완전성, 잔존 정리는 사용자 /hooks 몫). 마커 판정은
// 정확 일치 context-router 또는 접두 context-router/다(isOurMarkerValue) — D82가 받아들이는
// 값 집합을 넓힌 것이지 검사 종류를 바꾼 것이 아니다.
func isOurCodexGroup(raw json.RawMessage) bool {
	var p codexGroupProbe
	if json.Unmarshal(raw, &p) != nil {
		return false
	}
	if len(p.Hooks) == 0 {
		return false
	}
	for _, h := range p.Hooks {
		if !isCodexHookCommandToken(h.Command) || !isOurMarkerValue(h.StatusMessage) {
			return false
		}
	}
	return true
}

// codexHooksPath — 설치 대상 hooks.json 경로(§11.1 G3). 기본 프로젝트 `<root>/.codex/hooks.json`,
// --user는 `$CODEX_HOME/hooks.json`(CODEX_HOME 미설정 시 `~/.codex/hooks.json`). Codex는 CODEX_HOME
// 환경변수로 상태 루트(config·auth·logs·sessions·skills, 기본 ~/.codex)를 재지정하고 hooks.json은 그
// 활성 config 계층 옆에서 탐색되므로(공식 env-vars 문서), CODEX_HOME이 설정된 사용자에게 ~/.codex에
// 쓰면 Codex가 읽지 않는 파일을 만드는 무성 오설치가 된다(최종 리뷰 Codex P2). 빈 문자열=미설정으로
// 폴백. config.toml·[hooks.state]는 절대 건드리지 않는다(신뢰 승인 우회 금지).
func codexHooksPath(user bool, projectRoot string) (string, error) {
	if user {
		if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
			return filepath.Join(codexHome, "hooks.json"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("hook: 홈 디렉터리 해석 실패")
		}
		return filepath.Join(home, ".codex", "hooks.json"), nil
	}
	return filepath.Join(projectRoot, ".codex", "hooks.json"), nil
}

// removeCodexHooks — removeHookSettings의 Codex 형제(D28 원칙 승계: 멱등·타 항목/미지 키 raw
// 보존·빈 컨테이너 정리). 이벤트 집합과 소유 판정만 다르다.
func removeCodexHooks(existing []byte) ([]byte, error) {
	settings := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := json.Unmarshal(existing, &settings); err != nil {
			return nil, err
		}
		if settings == nil {
			settings = map[string]json.RawMessage{}
		}
	}
	hooks := map[string]json.RawMessage{}
	if raw, ok := settings["hooks"]; ok {
		if err := json.Unmarshal(raw, &hooks); err != nil {
			return nil, err
		}
		if hooks == nil {
			hooks = map[string]json.RawMessage{}
		}
	}
	for _, event := range codexRegistrations {
		var arr []json.RawMessage
		if raw, ok := hooks[event]; ok {
			if err := json.Unmarshal(raw, &arr); err != nil {
				return nil, err
			}
		}
		kept := make([]json.RawMessage, 0, len(arr))
		for _, el := range arr {
			if isOurCodexGroup(el) {
				continue
			}
			kept = append(kept, el)
		}
		if len(kept) == 0 {
			delete(hooks, event)
			continue
		}
		b, err := json.Marshal(kept)
		if err != nil {
			return nil, err
		}
		hooks[event] = b
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		b, err := json.Marshal(hooks)
		if err != nil {
			return nil, err
		}
		settings["hooks"] = b
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// atomicWriteFile — temp 파일 + rename 원자 쓰기(설계 §7). dir을 MkdirAll로 보강하고 같은 dir에
// 임시 파일을 만들어 rename한다(같은 볼륨 rename만 원자적). 실패 시 임시 파일을 정리한다.
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".ctr-settings-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmpName)
		if werr != nil {
			return werr
		}
		return cerr
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// hookSettingsPath — 대상 settings.json 경로(설계 §7). 기본 프로젝트 `.claude/settings.json`,
// --user는 `~/.claude/settings.json`.
func hookSettingsPath(user bool, projectRoot string) (string, error) {
	if user {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("hook: 홈 디렉터리 해석 실패")
		}
		return filepath.Join(home, ".claude", "settings.json"), nil
	}
	return filepath.Join(projectRoot, ".claude", "settings.json"), nil
}

// removeSettingsFile — path의 기존 settings를 읽어 removeHookSettings를 적용한 결과 바이트를
// 반환한다(쓰기는 호출자). 미존재 파일은 빈 기반으로 취급한다. 읽기 실패·병합 실패 오류에는
// 절대경로를 담지 않는다(§12 canary — os.ReadFile의 *PathError는 경로 포함).
func removeSettingsFile(path string) ([]byte, error) {
	existing, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("hook: 설정 파일 읽기 실패")
		}
		existing = nil
	}
	merged, err := removeHookSettings(existing)
	if err != nil {
		return nil, fmt.Errorf("hook: 설정 병합 실패: %w", err) // JSON 오류는 경로 미포함
	}
	return merged, nil
}

// runHookInstallGuidance — `hook install`이 내는 안내(D96·D97). 이 서브커맨드는 마이그레이션
// 사용자가 가장 먼저 치는 명령이라 이름 없는 오류로 떨어뜨리지 않는다 — 등록이 이제 어디서
// 일어나는지 짚고 비-0으로 끝난다. **어떤 파일도 만들지 않는다.**
//
// 절차 본문은 hostSnippet과 같은 상수(hostInstallSteps)를 읽는다 — 두 자리에 같은 절차를
// 따로 적으면 한쪽만 고쳐지고, 그 어긋남이 정확히 이 릴리스가 닫는 형태다.
func runHookInstallGuidance(stdout io.Writer) error {
	fmt.Fprintln(stdout, "hook install: 등록은 호스트 플러그인 설치가 맡습니다 — 아래 절차를 따르세요.")
	fmt.Fprintln(stdout)
	fmt.Fprint(stdout, hostInstallSteps)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "우리 훅 그룹 제거는 hook uninstall에 그대로 있습니다(--user·--codex 포함).")
	return errors.New("hook install: 등록은 호스트 플러그인 설치가 맡습니다 — 위 절차를 따르세요")
}

// runHook — "hook" 서브커맨드 내부 디스패치(설계 §2·§7). args[0]이 install/uninstall이면 그
// 명령을, 아니면 러닝 훅(Claude Code가 호출)이다. 러닝 훅은 --no-shadow만 인식하고 그 외 인자는
// fail-open으로 무시한다(D23 — 호스트를 절대 막지 않는다). hook.Run은 항상 exit 0이라 반환 int을
// 버리고 nil을 돌려준다.
func runHook(ctx context.Context, args []string, storeRoot, projectRoot, version string, stdout io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "install":
			return runHookInstallGuidance(stdout)
		case "uninstall":
			return runHookUninstall(args[1:], projectRoot, stdout)
		}
	}
	getenv := os.Getenv
	for _, a := range args {
		if a == "--no-shadow" {
			getenv = shadowOffGetenv
			break
		}
	}
	hook.Run(ctx, os.Stdin, stdout, storeRoot, version, hook.HostClaude, getenv)
	return nil
}

// runCodexHook — Codex 러닝 훅 전용 서브커맨드(설계 v0.4 §2 D35, §11.2 F3). install/uninstall
// 하위 없음 — 모든 인자는 러닝 훅 인자(--no-shadow만 인식, 그 외 fail-open 무시 D23). 전용
// 서브커맨드인 이유: v0.3 러닝 훅은 미지 인자를 무시하므로 플래그로 호스트를 구분하면 구버전
// 바이너리가 Codex 이벤트를 cc:로 오귀속시킨다 — 미지 서브커맨드는 v0.3이 exit 1로 거부해
// 오귀속이 구조적으로 불가능하다(버전 게이트).
func runCodexHook(ctx context.Context, args []string, storeRoot, version string, stdout io.Writer) error {
	getenv := os.Getenv
	for _, a := range args {
		if a == "--no-shadow" {
			getenv = shadowOffGetenv
			break
		}
	}
	hook.Run(ctx, os.Stdin, stdout, storeRoot, version, hook.HostCodex, getenv)
	return nil
}

// shadowOffGetenv — --no-shadow를 CTR_SHADOW_OFF=1로 반영하는 getenv 래퍼(settings command 훅에
// env 기재 수단 부재 → args 플래그로 통일, T0 §4).
func shadowOffGetenv(k string) string {
	if k == "CTR_SHADOW_OFF" {
		return "1"
	}
	return os.Getenv(k)
}

// runHookUninstall — uninstall 서브커맨드(설계 §7). 자기 훅 그룹(마커+명령 정확 일치)만
// 제거한다. 설정 파일이 없으면 no-op.
//
// **범위는 훅 그룹 하나다**(D96 계약 1의 유일한 예외). `.mcp.json` 항목과
// enabledMcpjsonServers 승인 키 정리는 v0.19에서 함께 사라졌다 — 그 둘을 지우던 코드가
// 기입 코드와 같은 병합기를 공유했고, 그 병합기가 파일 파괴 결함 다섯의 자리였다 `[문서]`.
// 남은 잔존물은 doctor [20]이 두 절로 읽기 전용 보고하고 `claude mcp remove`가 등록물을
// 지운다.
func runHookUninstall(args []string, projectRoot string, stdout io.Writer) error {
	fs := flag.NewFlagSet("hook uninstall", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	user := fs.Bool("user", false, "~/.claude/settings.json에서 제거(기본: 프로젝트)")
	codex := fs.Bool("codex", false, "Codex CLI hooks.json에서 제거(기본: Claude settings.json)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("hook uninstall: 플래그 파싱 실패: %w", err)
	}
	if rest := fs.Args(); len(rest) > 0 {
		return fmt.Errorf("hook uninstall: 예상치 않은 인자 %d개", len(rest))
	}
	if *codex {
		return runHookUninstallCodex(*user, projectRoot, stdout)
	}
	path, err := hookSettingsPath(*user, projectRoot)
	if err != nil {
		return err
	}
	// 설정 파일이 없으면 no-op으로 알린다(만들지 않는다). 정리 실패는 문면으로 알리고 오류를
	// 반환해 종료코드가 실패를 반영하게 한다.
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		fmt.Fprintln(stdout, "hook uninstall: 설정 파일 없음 — 제거할 항목 없음")
		return nil
	}
	merged, mergeErr := removeSettingsFile(path)
	if mergeErr != nil {
		fmt.Fprintln(stdout, "hook uninstall: 훅 설정 정리 실패")
		return mergeErr
	}
	if writeErr := atomicWriteFile(path, merged); writeErr != nil {
		fmt.Fprintln(stdout, "hook uninstall: 훅 설정 쓰기 실패")
		return errors.New("hook uninstall: 설정 쓰기 실패")
	}
	fmt.Fprintln(stdout, "hook uninstall: 훅 항목 제거 완료")
	return nil
}

// runHookUninstallCodex — --codex 제거(스펙 v0.7 §3, D47+D48). hooks.json에서 자기 그룹(D47
// 가드 PreToolUse 포함)을 소거한다. 파일 미존재는 "제거할 항목 없음"만 알리는 no-op이다.
//
// **config.toml은 건드리지 않는다**(D96 계약 1). 관리 블록을 지우던 경로가 v0.19에서 함께
// 사라졌다 — 그 경로에서 배열이 헤더를 삼켜 uninstall이 사용자 config.toml을 비우는 형태가
// 나왔다 `[문서]`(v0.18 §3.7). 남은 등록물은 doctor [16]이 파일:줄로 짚고
// `codex mcp remove`가 지운다.
func runHookUninstallCodex(user bool, projectRoot string, stdout io.Writer) error {
	path, err := codexHooksPath(user, projectRoot)
	if err != nil {
		return err
	}
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("hook: 설정 파일 읽기 실패")
	}
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(stdout, "hook uninstall (codex): 설정 파일 없음 — 제거할 항목 없음")
		return nil
	}
	merged, mErr := removeCodexHooks(existing)
	if mErr != nil {
		return fmt.Errorf("hook: 설정 병합 실패: %w", mErr)
	}
	if wErr := atomicWriteFile(path, merged); wErr != nil {
		return errors.New("hook uninstall: 설정 쓰기 실패")
	}
	fmt.Fprintln(stdout, "hook uninstall (codex): 훅 항목 제거 완료")
	return nil
}

// scanRegisteredHooks — path의 settings에서 자기 소유 훅 그룹 수와 마커 버전을 함께 수집한다
// (doctor [9] 등록 상태 + 버전 불일치 감지). 마커 버전은 첫 자기 그룹의 __ctrManaged에서
// hookMarkerPrefix 뒤 부분이다 — 한 번의 install이 6개 그룹을 동일 버전 마커로 쓰므로(§7) 어느
// 그룹에서 취하든 값이 같아 map 순회 순서와 무관하게 결정적이다. 파일 미존재·hooks 부재는 (0,""),
// 파싱 실패만 오류.
func scanRegisteredHooks(path string) (count int, marker string, err error) {
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		if errors.Is(rerr, os.ErrNotExist) {
			return 0, "", nil
		}
		return 0, "", errors.New("hook: 설정 파일 읽기 실패")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return 0, "", nil
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(data, &settings); err != nil {
		return 0, "", err
	}
	raw, ok := settings["hooks"]
	if !ok {
		return 0, "", nil
	}
	var hooks map[string]json.RawMessage
	if err := json.Unmarshal(raw, &hooks); err != nil {
		return 0, "", err
	}
	for _, arr := range hooks {
		var groups []json.RawMessage
		if json.Unmarshal(arr, &groups) != nil {
			continue // 배열이 아닌 이벤트는 우리 소유가 아님 — 건너뜀
		}
		for _, g := range groups {
			if !isOurHookGroup(g) {
				continue
			}
			count++
			if marker == "" { // 첫 자기 그룹의 마커 버전만(모두 동일 — install 계약)
				var p hookGroupProbe
				if json.Unmarshal(g, &p) == nil {
					marker = markerVersion(p.Managed)
				}
			}
		}
	}
	return count, marker, nil
}

// scanCodexRegisteredHooks — scanRegisteredHooks의 Codex 형제(D52, 스펙 v0.9 §0): hooks.json
// 자기 그룹 수와 marker 버전(statusMessage의 hookMarkerPrefix 뒤)을 읽는다. 최상위는
// removeCodexHooks와 동일한 {"hooks":{event:[group...]}} 래퍼로 파싱한다(형제 구조 미러링).
// 파일 부재·hooks 부재는 (0,"",nil) — 미등록 정보 분기. isOurCodexGroup(전건 판정)을 그대로
// 재사용한다. 읽기 실패 오류에는 절대경로를 담지 않는다(§12 canary — *PathError는 경로 포함).
func scanCodexRegisteredHooks(path string) (count int, marker string, err error) {
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		if errors.Is(rerr, os.ErrNotExist) {
			return 0, "", nil
		}
		return 0, "", errors.New("hook: 설정 파일 읽기 실패")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return 0, "", nil
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(data, &settings); err != nil {
		return 0, "", err
	}
	raw, ok := settings["hooks"]
	if !ok {
		return 0, "", nil
	}
	var hooks map[string]json.RawMessage
	if err := json.Unmarshal(raw, &hooks); err != nil {
		return 0, "", err
	}
	for _, arr := range hooks {
		var groups []json.RawMessage
		if json.Unmarshal(arr, &groups) != nil {
			continue // 배열이 아닌 이벤트는 우리 소유 아님 — 건너뜀
		}
		for _, g := range groups {
			if !isOurCodexGroup(g) {
				continue
			}
			count++
			if marker == "" { // 첫 자기 그룹의 마커만(install이 모든 그룹에 동일 버전 마커를 씀)
				var p codexGroupProbe
				if json.Unmarshal(g, &p) == nil && len(p.Hooks) > 0 {
					marker = markerVersion(p.Hooks[0].StatusMessage)
				}
			}
		}
	}
	return count, marker, nil
}

// countRegisteredHooks — scanRegisteredHooks의 개수 부분만(기존 호출부·테스트 호환 얇은 래퍼).
func countRegisteredHooks(path string) (int, error) {
	n, _, err := scanRegisteredHooks(path)
	return n, err
}

// dropsByReason — drops.log을 사유별로 집계한다(doctor [12]). appendDrop 계약은 정확히 2필드
// "<unix초>\t<사유>"(구) 또는 5필드 "<unix초>\t<사유>\t<sid8>\t<hook_event>\t<tool>"(신, D43) —
// ts가 1+ 숫자이고 사유(필드[1])가 비지 않는 그 두 필드 수의 줄만 사유로 센다. 그 외 필드 수·비숫자
// ts·사유 없음은 "unparsed"(느슨 수용 금지, 설계 §5) — 진단은 절대 중단하지 않고, total은 빈 줄
// 포함 모든 줄을 센다(줄 수 계약). 셋째 반환값 lastSeen은 사유별 ts 최댓값(D71) — isUnixTS를
// 통과해도 int64 변환이 실패하면(자릿수 초과) 그 줄은 reasons에만 집계되고 lastSeen 키는 만들지
// 않는다. 파일 부재·읽기 실패는 (0, nil, nil) — countDropsLog와 동일 fail-soft.
func dropsByReason(path string) (int, map[string]int, map[string]int64) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, nil
	}
	defer func() { _ = f.Close() }()
	total, reasons, lastSeen := 0, map[string]int{}, map[string]int64{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20) // 긴 줄에도 스캔 중단 방지(countDropsLog 관례 보존)
	for sc.Scan() {
		total++ // 빈 줄도 센다 — 기존 countDropsLog의 total 의미 보존(줄 수 계약)
		fields := strings.Split(sc.Text(), "\t")
		// D43: 정확 2필드(구) 또는 정확 5필드(신)만 수용 — 그 외 필드 수는 unparsed(느슨 수용 금지).
		if (len(fields) == 2 || len(fields) == 5) && fields[1] != "" && isUnixTS(fields[0]) {
			reasons[fields[1]]++
			// D71: 사유별 ts 최댓값. isUnixTS는 자릿수 상한을 보지 않으므로 int64 변환이 따로
			// 실패할 수 있다 — 그 줄은 집계에만 남기고 병기는 생략한다(키를 만들지 않는다).
			if ts, convErr := strconv.ParseInt(fields[0], 10, 64); convErr == nil {
				if prev, ok := lastSeen[fields[1]]; !ok || ts > prev {
					lastSeen[fields[1]] = ts
				}
			}
		} else {
			reasons["unparsed"]++ // 빈 줄·비숫자 ts·필드 수 불일치·사유 없음 전부 unparsed
		}
	}
	return total, reasons, lastSeen
}

// isUnixTS — appendDrop이 쓰는 ts(time.Now().Unix()의 "%d")는 1+ ASCII 숫자다. 그 형식만 인정한다
// (부호·공백·빈 문자열 거부) — 실제 writer 포맷에 맞춰 검증한다.
func isUnixTS(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// formatDropCount — "N(사유=n,...)" 렌더(사유 알파벳순, 결정적). N==0이면 "0". lastSeen[사유]가
// 있으면 "사유=n@YYYY-MM-DD"(D71, UTC 날짜)로 마지막 발생 시각을 병기하고, 키가 없으면
// (unparsed·ts 변환 실패) 기존처럼 건수만 낸다.
func formatDropCount(total int, reasons map[string]int, lastSeen map[string]int64) string {
	if total == 0 {
		return "0"
	}
	keys := make([]string, 0, len(reasons))
	for k := range reasons {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		// D71: 마지막 발생 시각을 UTC 날짜로 병기한다 — 로컬 타임존이면 골든이 3-OS CI의
		// 타임존에 따라 갈린다. ts가 없는 사유(unparsed·변환 실패)는 건수만 낸다.
		if ts, ok := lastSeen[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%d@%s", k, reasons[k], time.Unix(ts, 0).UTC().Format("2006-01-02")))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%d", k, reasons[k]))
	}
	return fmt.Sprintf("%d(%s)", total, strings.Join(parts, ","))
}
