// Package cli — hook install/uninstall(settings.json 멱등 병합·원자 쓰기·타 도구 보존) +
// running-hook 위임(--no-shadow). 설계서 §7(D28). 러닝 훅은 fail-open(§2.3)이라 인자 오류로
// 호스트를 막지 않는다.
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
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wotjr1649/context-router/internal/hook"
)

const (
	hookBinaryName = "context-router"    // PATH 실행 파일명(=훅 명령 첫 토큰) — ctr은 MCP 등록 키일 뿐(설계 §7)
	hookTimeoutSec = 10                  // 등록 timeout(단위 초 — T0 확인, command 기본 600초)
	dropsFileName  = "session.drops.log" // internal/hook dropsLogName(unexported) 미러 — doctor drops 집계용
)

// hookRegistrations — 설치 대상 6항목(T0-amended 설계 §7·v0.10 D53 서브에이전트 2항목). matcher가 빈 문자열이면 전체 매칭
// (SessionStart=시작 방식 전체, Pre/PostToolUse(Failure)=전 도구 — T0 §4). PreToolUse는
// Read|Bash|PowerShell|Grep 정규식 매칭(large-read/dump guard 대상 — v0.4 D36·v0.10 D54) — 관리
// 그룹 1개 유지가 merge의 동일-이벤트 상호 제거 함정을 회피한다(설계 v0.3 §4).
var hookRegistrations = []struct {
	event   string
	matcher string
}{
	{"SessionStart", ""},
	{"PreToolUse", "Read|Bash|PowerShell|Grep"},
	{"PostToolUse", ""},
	{"PostToolUseFailure", ""},
	{"SubagentStart", ""},
	{"SubagentStop", ""},
}

// ownedHookGroup — 설치가 쓰는 자기 소유 그룹(버전 마커 필드 __ctrManaged 포함, 설계 §7). 타
// 도구 그룹은 json.RawMessage로 그대로 왕복 보존하고 이 타입은 자기 항목에만 쓴다.
type ownedHookGroup struct {
	Matcher string    `json:"matcher"`
	Hooks   []hookCmd `json:"hooks"`
	Managed string    `json:"__ctrManaged"`
}

type hookCmd struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

// hookMarker — 소유권/버전 마커 값. 판정은 접두사(hookBinaryName+"/")로 하므로 install 버전과
// uninstall 버전이 달라도 대칭 제거된다.
func hookMarker(version string) string { return hookBinaryName + "/" + version }

func hookMarkerPrefix() string { return hookBinaryName + "/" }

// hookGroupMarker — **훅 등록물**(.claude/settings.json 그룹의 __ctrManaged, Codex hooks.json의
// statusMessage)이 쓰는 무버전 소유 표식(D82). 버전은 MCP 등록물에만 둔다 — 이득 둘이다:
// ① D64 드리프트가 훅 등록물에서 구조적으로 사라진다(버전이 없으니 뒤처질 것이 없다)
// ② 훅 정의가 릴리스 간 바이트 동일해져 Codex 재신뢰가 릴리스마다 강요되지 않는다.
// ②는 필요조건까지만 주장한다 — trusted_hash가 무엇을 해싱하는지는 Codex 내부이며 검증되지
// 않았고, 바이트 동일도 **같은 설치 옵션·같은 MCP 상태 안에서만** 성립한다(buildHookCommand의
// --store-root·--no-shadow, runHookInstallCodex가 넘기는 mcpConfirmed의 등록 그룹 2개/3개 분기).
const hookGroupMarker = hookBinaryName

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

// markerDrift — MCP 등록물 표식이 현재 버전과 어긋나는가(D83). 우리 표식이 아니면 고칠 대상이
// 아니다. **무버전 표식(context-router)은 "표식 있음·버전 미상"이라 드리프트로 본다** —
// hostSnippet이 인쇄하는 값이 그 형태이고, --fix가 현재 버전 값으로 채운다(D80).
func markerDrift(marker, version string) bool {
	return isOurMarkerValue(marker) && markerVersion(marker) != version
}

// ownedRegistration — 등록물이 우리 소유인가(D83 --fix의 대상 조건). 표식 값이 소유 기준을
// 만족하거나 command가 hookBinaryName이면 참이다 — D80의 인수 절과 같은 기준이다.
// 거짓이면 --fix는 병합하지 않고 hook install만 안내한다: 부재 파일뿐 아니라 **부재 등록물도
// 만들지 않는 것**이 no-create 원칙의 범위이고, 등록 생성은 hook install의 일이다.
// [20]의 "미등록" 표시가 이 술어의 거짓과 같은 조건이라 감지와 고침의 대상이 일치한다.
func ownedRegistration(marker, command string, found bool) bool {
	return found && (isOurMarkerValue(marker) || command == hookBinaryName)
}

// buildHookCommand — 등록할 훅 명령 문자열을 조립한다(설계 §7). --store-root는 명시된 경우에만
// 원시값을 주입한다(기본 절대경로의 불필요한 영구 기입 방지 — 미명시 시 훅은 실행 시점에
// CTR_STORE_ROOT>OS 기본으로 해석). --no-shadow는 러닝 훅이 CTR_SHADOW_OFF로 반영한다(settings
// command 훅에 env 기재 수단이 없어 args 플래그로 통일 — T0 §4 스키마).
func buildHookCommand(storeRootExplicit bool, storeRootRaw string, noShadow bool) string {
	cmd := hookBinaryName + " hook"
	if storeRootExplicit && storeRootRaw != "" {
		// T11 실측: 훅 명령은 Windows에서도 POSIX sh 규칙으로 파싱된다 — 비인용 값은 백슬래시가
		// 소실되고(C:\tmp\ctr→C:tmpctr) 공백에서 분할된다. 홑따옴표 인용만 무변형 전달(캡처 argv
		// 실측). 내부 홑따옴표는 표준 '\'' 이스케이프.
		cmd += " --store-root '" + strings.ReplaceAll(storeRootRaw, "'", `'\''`) + "'"
	}
	if noShadow {
		cmd += " --no-shadow"
	}
	return cmd
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

// mergeHookSettings — 기존 settings JSON을 map[string]json.RawMessage 보존 패턴으로 파싱해,
// 관리 대상 이벤트 배열에서 자기 항목만 제거(install/uninstall 공통)하고 install이면 새 그룹을
// append한 뒤 재직렬화한다(설계 §7). 미지 최상위 키·타 도구의 훅 항목·관리 대상 아닌 이벤트는
// 전부 raw로 왕복 보존된다. 빈 배열이 된 이벤트는 제거하고, hooks 자체가 비면 최상위에서 제거한다.
func mergeHookSettings(existing []byte, command, marker string, install bool) ([]byte, error) {
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
	for _, reg := range hookRegistrations {
		var arr []json.RawMessage
		if raw, ok := hooks[reg.event]; ok {
			if err := json.Unmarshal(raw, &arr); err != nil {
				return nil, err
			}
		}
		kept := make([]json.RawMessage, 0, len(arr)+1)
		for _, el := range arr {
			if isOurHookGroup(el) {
				continue // 자기 항목 제거(멱등 재설치 / uninstall 대칭)
			}
			kept = append(kept, el)
		}
		if install {
			b, err := json.Marshal(ownedHookGroup{
				Matcher: reg.matcher,
				Hooks:   []hookCmd{{Type: "command", Command: command, Timeout: hookTimeoutSec}},
				Managed: marker,
			})
			if err != nil {
				return nil, err
			}
			kept = append(kept, b)
		}
		if len(kept) == 0 {
			delete(hooks, reg.event)
			continue
		}
		b, err := json.Marshal(kept)
		if err != nil {
			return nil, err
		}
		hooks[reg.event] = b
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

// codexRegistrations — Codex 설치 대상 2항목(D35 캐프처 전용, §11.1 G1 행 1 채택 — PreToolUse
// 미등록·거부 표면 없음 §7). matcher 빈 문자열 = 전체 매칭.
var codexRegistrations = []struct {
	event   string
	matcher string
}{
	{"SessionStart", ""},
	{"PostToolUse", ""},
}

// codexHookCmd — Codex hooks.json 훅 항목. statusMessage가 소유/버전 마커를 겸한다(§11.1 G3
// — 미지 필드의 스키마 관용성이 미보증이라 공식 필드에 탑재; Codex UI에 노출되는 것은 의도).
type codexHookCmd struct {
	Type          string `json:"type"`
	Command       string `json:"command"`
	Timeout       int    `json:"timeout"`
	StatusMessage string `json:"statusMessage"`
}

type codexOwnedGroup struct {
	Matcher string         `json:"matcher"`
	Hooks   []codexHookCmd `json:"hooks"`
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

// buildCodexHookCommand — buildHookCommand의 Codex 형제: 러닝 서브커맨드가 `codex-hook`이다
// (D35 호스트 경계 + §11.2 F3 버전 게이트). 인용 규칙은 T11 관례 승계 — 2026-07-22 실측:
// 공백 포함 --store-root가 홑따옴표 인용으로 무변형 전달됨(실 codex exec, Windows).
func buildCodexHookCommand(storeRootExplicit bool, storeRootRaw string, noShadow bool) string {
	cmd := hookBinaryName + " codex-hook"
	if storeRootExplicit && storeRootRaw != "" {
		cmd += " --store-root '" + strings.ReplaceAll(storeRootRaw, "'", `'\''`) + "'"
	}
	if noShadow {
		cmd += " --no-shadow"
	}
	return cmd
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

// codexGuardRegistration — D47 가드 그룹(스펙 §3). 설치는 MCP 확정 시에만 포함하고(설치 결합 —
// 거부 표면과 안내 도구가 함께 실려 D32 위반 봉쇄), 제거는 install=false라 무조건 소거 대상(제거 대칭).
var codexGuardRegistration = struct {
	event   string
	matcher string
}{"PreToolUse", "Bash"}

// mergeCodexHooks — mergeHookSettings의 Codex 형제(D28 원칙 승계: 멱등·타 항목/미지 키 raw
// 보존·제거 대칭·빈 컨테이너 정리). 등록 집합·그룹 타입·소유 판정만 다르다. withGuard=true거나
// install=false면 codexGuardRegistration(PreToolUse matcher Bash)을 등록/제거 대상에 포함한다.
func mergeCodexHooks(existing []byte, command, marker string, install, withGuard bool) ([]byte, error) {
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
	regs := codexRegistrations
	if withGuard || !install { // 설치는 MCP 확정 시에만, 제거는 무조건(codexRegistrations 원본 보존 위해 fresh copy)
		regs = append(append([]struct{ event, matcher string }{}, codexRegistrations...), codexGuardRegistration)
	}
	for _, reg := range regs {
		var arr []json.RawMessage
		if raw, ok := hooks[reg.event]; ok {
			if err := json.Unmarshal(raw, &arr); err != nil {
				return nil, err
			}
		}
		kept := make([]json.RawMessage, 0, len(arr)+1)
		for _, el := range arr {
			if isOurCodexGroup(el) {
				continue
			}
			kept = append(kept, el)
		}
		if install {
			b, err := json.Marshal(codexOwnedGroup{
				Matcher: reg.matcher,
				Hooks:   []codexHookCmd{{Type: "command", Command: command, Timeout: hookTimeoutSec, StatusMessage: marker}},
			})
			if err != nil {
				return nil, err
			}
			kept = append(kept, b)
		}
		if len(kept) == 0 {
			delete(hooks, reg.event)
			continue
		}
		b, err := json.Marshal(kept)
		if err != nil {
			return nil, err
		}
		hooks[reg.event] = b
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

// mergeSettingsFile — path의 기존 settings를 읽어 mergeHookSettings를 적용한 결과 바이트를
// 반환한다(쓰기는 호출자). 미존재 파일은 빈 병합 기반으로 취급한다. 읽기 실패·병합 실패 오류에는
// 절대경로를 담지 않는다(§12 canary — os.ReadFile의 *PathError는 경로 포함).
func mergeSettingsFile(path, command, marker string, install bool) ([]byte, error) {
	existing, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("hook: 설정 파일 읽기 실패")
		}
		existing = nil
	}
	merged, err := mergeHookSettings(existing, command, marker, install)
	if err != nil {
		return nil, fmt.Errorf("hook: 설정 병합 실패: %w", err) // JSON 오류는 경로 미포함
	}
	return merged, nil
}

// runHook — "hook" 서브커맨드 내부 디스패치(설계 §2·§7). args[0]이 install/uninstall이면 그
// 명령을, 아니면 러닝 훅(Claude Code가 호출)이다. 러닝 훅은 --no-shadow만 인식하고 그 외 인자는
// fail-open으로 무시한다(D23 — 호스트를 절대 막지 않는다). hook.Run은 항상 exit 0이라 반환 int을
// 버리고 nil을 돌려준다.
func runHook(ctx context.Context, args []string, storeRoot, storeRootRaw string, storeRootExplicit bool, projectRoot, version string, stdout io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "install":
			return runHookInstall(args[1:], storeRoot, storeRootRaw, storeRootExplicit, projectRoot, version, stdout)
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

// parseEnableProfiles — --enable의 쉼표 구분 목록을 프로필 집합으로 판다(D81). 모르는 이름은
// 오류다 — 조용히 떨어뜨리면 사용자가 프로필이 켜졌다고 오인한다. 오류 문면에 입력 원문을
// 담지 않는다(규약 §6 — 사용자 입력 에코 금지).
func parseEnableProfiles(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []string
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if !slices.Contains(mcpProfileNames, name) {
			return nil, fmt.Errorf("hook install: --enable에 모르는 프로필 이름이 있습니다(가능: %s)", strings.Join(mcpProfileNames, ","))
		}
		out = append(out, name)
	}
	return out, nil
}

// runHookInstall — install 서브커맨드(설계 §7). --user/--no-shadow/--enable-exec/--codex를 자체
// flagset으로 파싱한다(--root/--store-root는 main의 prescanRootFlags가 이미 소비·전달 —
// storeRootExplicit/Raw). Claude 경로는 훅 병합에 이어 .mcp.json 등록과 승인 키까지 다룬다(D64) —
// 어느 하나가 실패해도 앞선 성공은 유지하고 사유만 보고한다(부분 성공을 감추지 않는다).
func runHookInstall(args []string, storeRoot, storeRootRaw string, storeRootExplicit bool, projectRoot, version string, stdout io.Writer) error {
	fs := flag.NewFlagSet("hook install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	user := fs.Bool("user", false, "~/.claude/settings.json에 등록(기본: 프로젝트)")
	noShadow := fs.Bool("no-shadow", false, "Shadow Recall 비활성(훅 명령에 --no-shadow 주입)")
	enableExec := fs.Bool("enable-exec", false, "exec 프로필을 켠다(--enable exec와 같다)")
	enable := fs.String("enable", "", "등록물에 실을 프로필 목록(쉼표 구분: ingest,net,exec)")
	codex := fs.Bool("codex", false, "Codex CLI hooks.json에 등록(기본: Claude settings.json)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("hook install: 플래그 파싱 실패: %w", err)
	}
	if rest := fs.Args(); len(rest) > 0 {
		return fmt.Errorf("hook install: 예상치 않은 인자 %d개", len(rest))
	}
	// 프로필 입력(D81): --enable과 --enable-exec을 함께 지정하면 결과는 합집합이고 지정
	// 순서는 결과를 바꾸지 않는다(canonicalProfiles가 mcpProfileNames 순서로 정규화한다).
	// setProfile은 "명시 플래그가 있었는가"이며, 없으면 mergeMCPServers·installCodexConfigBlock이
	// 기존 항목의 프로필을 그대로 유지한다(재설치가 이미 켠 프로필을 끄지 않는다).
	installProfiles, enableErr := parseEnableProfiles(*enable)
	if enableErr != nil {
		return enableErr
	}
	if *enableExec {
		installProfiles = append(installProfiles, "exec")
	}
	// 공백뿐인 --enable은 parseEnableProfiles와 같게 비어있음으로 본다 — 원시 문자열을 비교하면
	// 프로필이 빈 집합인데 setProfile=true가 되어 기존·은퇴 프로필을 덮는다(리뷰 T7-F1).
	setProfile := strings.TrimSpace(*enable) != "" || *enableExec
	if !setProfile {
		installProfiles = defaultMCPProfiles // 기존 항목도 은퇴 항목도 없는 첫 설치에서만 쓰인다
	}
	installProfiles = canonicalProfiles(installProfiles)
	// D81 — --codex × --enable-exec 상호 배제를 제거한다. 그 검사의 사유 둘이 모두 해소됐다:
	// ① --codex가 .mcp.json을 만들지 않는다 → 프로필이 Codex 관리 테이블에도 실리므로 반영될
	// 자리가 생겼다. ② 관리 블록의 도구 목록이 고정이다 → enabled_tools가 args와 함께 조립된다.
	if *codex {
		return runHookInstallCodex(*user, *noShadow, storeRootExplicit, storeRootRaw, projectRoot, version, installProfiles, setProfile, stdout)
	}
	path, err := hookSettingsPath(*user, projectRoot)
	if err != nil {
		return err
	}
	// F3: 명시된 상대 --store-root는 절대화한다 — 상대 경로를 그대로 훅 명령에 박으면 프로젝트마다
	// 훅 실행 시점 cwd 기준으로 서로 다른 store로 갈라진다(store 파편화). 절대화 실패 시 원시값 유지.
	if storeRootExplicit && storeRootRaw != "" {
		if abs, absErr := filepath.Abs(storeRootRaw); absErr == nil {
			storeRootRaw = abs
		}
	}
	command := buildHookCommand(storeRootExplicit, storeRootRaw, *noShadow)
	merged, err := mergeSettingsFile(path, command, hookGroupMarker, true)
	if err != nil {
		return err
	}
	if err := atomicWriteFile(path, merged); err != nil {
		return errors.New("hook install: 설정 쓰기 실패")
	}
	fmt.Fprintf(stdout, "hook install: %d개 이벤트 등록 완료\n", len(hookRegistrations))

	// .mcp.json 병합 — 훅과 같은 멱등 계약(설계 v0.12 D64). 실패해도 훅 설치 결과는 유지하고
	// 사유만 보고한다(설치가 부분 성공임을 감춘 채 성공으로 보이지 않게 한다).
	mcpRegistered := false
	mcpPath := mcpConfigPath(projectRoot)
	existing, readErr := os.ReadFile(mcpPath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		fmt.Fprintln(stdout, "mcp: 기존 설정을 읽지 못해 .mcp.json 병합을 건너뜁니다")
	} else {
		entry := mcpServerEntry{
			Command: hookBinaryName, Args: mcpArgsForProfiles(installProfiles),
			AlwaysLoad: true, Managed: hookMarker(version),
		}
		// setProfile=명시 플래그 유무: 플래그가 없으면 기존 항목의 args를 보존하고(재설치가
		// 이미 켠 프로필을 끄지 않는다), 우리 이름에 기존 항목이 없을 때만 은퇴 항목의 것을
		// 이월한다. 둘 다 없는 첫 설치에서만 위 기본 프로필이 실린다(D81 우선순위).
		mcpMerged, _, mergeErr := mergeMCPServers(existing, ctrMCPServerName, entry, true, setProfile)
		if mergeErr != nil {
			// 실패 사유는 둘뿐이다 — 우리 이름 자리에 우리 소유가 아닌 항목이 있거나(소유 관문),
			// 파일이 해석되지 않거나. 어느 쪽이든 사용자가 손댈 대상을 알려야 조치할 수 있다.
			fmt.Fprintf(stdout, "mcp: .mcp.json 병합을 멈췄습니다(훅 설치는 완료) — %q 이름에 우리가 소유하지 않은 항목이 있거나 파일을 해석할 수 없습니다. 그 항목을 정리하거나 파일을 고친 뒤 다시 실행하세요\n", ctrMCPServerName)
		} else if writeErr := atomicWriteFile(mcpPath, mcpMerged); writeErr != nil {
			fmt.Fprintln(stdout, "mcp: .mcp.json 기록 실패 — 훅 설치는 완료되었습니다")
		} else {
			mcpRegistered = true
			fmt.Fprintf(stdout, "mcp: .mcp.json 병합 완료(서버 %s)\n", ctrMCPServerName)
			// 빈 프로필로 남은 기존 등록물은 보존 규칙이 이겨 기본 프로필을 얻지 못한다(D81) —
			// 명시 경로를 알려야 사용자가 D81의 동기였던 두 도구를 켤 수 있다(리뷰 P4).
			if !setProfile && emptyProfileRegistration(existing, ctrMCPServerName) {
				fmt.Fprintln(stdout, "mcp: 기존 등록물에 프로필이 없어 그대로 유지했습니다 — ctr_index·ctr_fetch_and_index가 필요하면 hook install --enable ingest,net으로 명시하세요(빈 프로필은 기본값으로 넓히지 않습니다)")
			}
		}
	}

	// 승인 키 — 아무도 정의하지 않았거나 최상위 정의가 이번 설치 스코프 자신이면 그 파일에 쓰고,
	// 다른 스코프가 이기고 있으면 보고만 한다.
	// 이 키는 스코프 간 병합되지 않고 최상위 정의가 통째로 override 하므로, 남의 스코프가 이기는 상태에서
	// 우리 스코프에 쓰면 조용히 무시될 값을 남기는 셈이다(설계 D64 스코프 규칙).
	// 등록이 확정되지 않은 상태에서는 승인 키도 건드리지 않는다 — 이 키는 이름으로 .mcp.json 항목을
	// 미리 승인하므로, 우리 이름 자리에 남의 항목이 남아 있는 채로 이름을 넣으면 그 남의 항목을
	// 대신 자동 승인해 주는 셈이 된다(소유 관문의 연장).
	if !mcpRegistered {
		fmt.Fprintln(stdout, "mcp: .mcp.json 등록이 확정되지 않아 승인 키는 건드리지 않았습니다")
	} else if *user {
		// 이 키는 프로젝트 .mcp.json 등록을 "이름으로" 승인하는 장치다. 사용자 스코프에 쓰면 이 머신
		// 모든 프로젝트의 동명 항목까지 소유 검증 없이 승인하게 되므로 쓰지 않는다(리뷰 T6-1).
		// 전 프로젝트 사용의 정식 경로는 사용자 스코프 서버 등록이고, 그쪽은 이 키가 필요 없다.
		fmt.Fprintln(stdout, "mcp: --user 설치는 승인 키를 쓰지 않았습니다 — enabledMcpjsonServers는 프로젝트 .mcp.json 등록에만 적용됩니다. 모든 프로젝트에서 쓰려면 doctor 안내의 사용자 스코프 등록을 쓰세요(승인 키가 필요 없습니다)")
	} else if winner, defined, scopeErr := enabledServersScope(projectRoot, os.ReadFile); scopeErr != nil {
		fmt.Fprintln(stdout, "mcp: 승인 키 스코프를 판정하지 못해 건너뜁니다")
	} else if len(defined) > 0 && winner != path {
		// 실제로 적용되는 스코프를 라벨로 알린다 — 파일명은 project와 user가 같아 구분되지 않고,
		// 절대경로는 싣지 않는다(§12 canary 규율). 여기 오는 것은 이기는 정의가 이번 설치가 쓰는
		// 파일이 아닌 경우뿐이다: 더 좁은 스코프(local)는 사용자가 그 스코프로 관리하기로 정한 것이고,
		// 더 넓은 스코프(user)만 정의한 경우에는 project에 쓰는 순간 그 목록을 통째로 덮는다 — 어느
		// 쪽도 우리가 대신 고칠 자리가 아니라 보고가 맞다. "추가하세요"가 아니라 "있는지 확인하세요"인
		// 이유는 그 파일의 목록에 사용자가 이미 이름을 넣어 뒀을 수 있어서다(그러면 거짓 경보가 된다).
		fmt.Fprintf(stdout, "mcp: enabledMcpjsonServers는 이미 %d개 스코프에 정의돼 있어 이번 설치가 쓰지 않았습니다(적용되는 것은 최상위 %s 스코프 1개) — 자동 승인이 안 되면 그 파일의 목록에 %q가 있는지 확인하세요\n",
			len(defined), enabledServersScopeLabel(projectRoot, winner), ctrMCPServerName)
	} else {
		// 아무도 정의하지 않았거나(흔한 첫 설치) 이기는 정의가 곧 이 파일이다. 후자는 uninstall이
		// 하위 스코프 보호를 위해 남긴 빈 배열([])로 만들어지는 흔한 상태다 — 그 상태를 "이미 정의됨"으로
		// 보고로 넘기면 같은 프로젝트의 재설치가 .mcp.json 등록만 되돌려 놓고 승인 이름은 빼놓아, 등록된
		// 서버가 자동 승인되지 않은 채 "설치 완료"만 남는다. 설계 D64 스코프 규칙의 "사용 중인 최고
		// 우선순위 스코프에 쓴다"가 이 경우를 지시한다(무시될 값을 쓰는 것이 아니다 — 이기는 파일이다).
		// path는 위쪽 훅 병합이 이미 쓴 파일이다 — 그 쓰기가 끝난 뒤 다시 읽어 병합해야 한다.
		// 순서가 뒤집히면 훅 쓰기가 승인 키를 덮는다(둘 다 성공하고 결과만 조용히 사라진다).
		// mergeEnabledServers는 이미 든 이름을 다시 넣지 않으므로, 이름이 있는 목록에는 같은 바이트가
		// 나온다 — 이 분기가 재설치마다 도는 것이 파일을 흔들지 않는 근거다.
		prev, prevErr := os.ReadFile(path)
		if prevErr != nil && !errors.Is(prevErr, os.ErrNotExist) {
			fmt.Fprintln(stdout, "mcp: 기존 settings를 읽지 못해 승인 키 기록을 건너뜁니다")
		} else if next, mergeErr := mergeEnabledServers(prev, ctrMCPServerName, true, false); mergeErr != nil {
			fmt.Fprintln(stdout, "mcp: 승인 키 병합 실패 — 훅 설치는 완료되었습니다")
		} else if writeErr := atomicWriteFile(path, next); writeErr != nil {
			fmt.Fprintln(stdout, "mcp: 승인 키 기록 실패 — 훅 설치는 완료되었습니다")
		} else {
			fmt.Fprintf(stdout, "mcp: enabledMcpjsonServers에 %q를 기록했습니다\n", ctrMCPServerName)
		}
	}
	return nil
}

// backupCodexConfig — 내용을 바꾸기 직전 단일 슬롯 백업(D84). config.toml.bak을 **매번
// 덮어쓰며 누적하지 않는다**. 호출자는 기입 바이트가 기존과 다를 때만 부른다 — 무변경
// 재실행마다 .bak이 생기면 단일 슬롯 계약이 무의미해진다. 재직렬화는 호스트가 일으키므로
// 우리 설계가 막을 수 없고, 이 백업은 **우리 쪽 판정이 틀렸을 때의 복구 수단**이다.
// 새로 만드는 파일(existing 없음)에는 되돌릴 내용이 없어 백업하지 않는다.
func backupCodexConfig(path string, existing []byte) error {
	if len(existing) == 0 {
		return nil
	}
	return atomicWriteFile(path+".bak", existing)
}

// runHookInstallCodex — --codex 설치 결합(스펙 v0.7 §3, D47+D48). config.toml 관리 블록 병합을
// 먼저 시도해 MCP 확정 여부를 판정하고(가드 등록의 전제), hooks.json에는 MCP 확정 시에만
// PreToolUse(Bash) 가드 그룹을 포함한다 — "거부 표면 존재 + 안내 도구 부재"(D32)를 구조적으로 봉쇄.
func runHookInstallCodex(user, noShadow, storeRootExplicit bool, storeRootRaw, projectRoot, version string, profiles []string, setProfile bool, stdout io.Writer) error {
	cfgPath, err := codexConfigPath()
	if err != nil {
		return err
	}
	cfgExisting, err := os.ReadFile(cfgPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("hook: config.toml 읽기 실패")
	}
	res := installCodexConfigBlock(cfgExisting, codexInstallRequest{
		Profiles: profiles, SetProfile: setProfile, Marker: hookMarker(version),
	})
	if res.State == mcpWritten && res.Changed {
		if bErr := backupCodexConfig(cfgPath, cfgExisting); bErr != nil {
			return errors.New("hook install: config.toml 백업 실패")
		}
		if wErr := atomicWriteFile(cfgPath, res.Out); wErr != nil {
			return errors.New("hook install: config.toml 쓰기 실패")
		}
	}
	mcpConfirmed := res.State == mcpWritten || res.State == mcpExistingHeader
	path, err := codexHooksPath(user, projectRoot)
	if err != nil {
		return err
	}
	if storeRootExplicit && storeRootRaw != "" {
		if abs, absErr := filepath.Abs(storeRootRaw); absErr == nil {
			storeRootRaw = abs
		}
	}
	command := buildCodexHookCommand(storeRootExplicit, storeRootRaw, noShadow)
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("hook: 설정 파일 읽기 실패")
	}
	merged, err := mergeCodexHooks(existing, command, hookGroupMarker, true, mcpConfirmed)
	if err != nil {
		return fmt.Errorf("hook: 설정 병합 실패: %w", err)
	}
	if err := atomicWriteFile(path, merged); err != nil {
		return errors.New("hook install: 설정 쓰기 실패")
	}
	reportCodexMCPState(stdout, res.State)
	if res.State == mcpWritten {
		// D81 — 승인 모드 키는 기입하지 않는다. 예전에는 관리 블록의 주석 한 줄로 권했으나
		// 재직렬화가 주석을 지우므로(§3 표1) 파일에 남지 않는 안내는 안내가 아니다. 기입이
		// 확정된 경우에만 낸다.
		fmt.Fprintln(stdout, "hook install (codex): 승인 프롬프트가 필요하면 [mcp_servers.ctr]에 default_tools_approval_mode = \"prompt\"를 직접 넣으세요 — 설치기는 그 키를 쓰지 않고, 넣어 둔 키는 재설치가 보존합니다")
		if res.ExecExposed {
			// D81 — exec 노출 경로 하나를 명시한다. 승인 모드 키는 서버 테이블 단위라 별도
			// [mcp_servers.ctr-exec]에 걸어 둔 게이트가 관리 테이블 쪽 exec에는 걸리지 않는다.
			// 우리는 그 테이블을 고치지 않는다 — D80이 관리 대상 밖으로 두었다.
			// 두 톤을 가른다(리뷰 승격 — 이월 T4-F3): Profiles에 exec가 있으면 이번 실행이 실제로
			// 켠 것이고, 없으면(ArgsKept로 되읽기에 실패해 손대지 않은 값이 이미 그 상태였던 것)
			// "켰다"고 말하면 부정확하다 — ExecExposed가 그 값을 손대지 않은 경우도 잡아낸다.
			if slices.Contains(res.Profiles, "exec") {
				fmt.Fprintln(stdout, "hook install (codex): exec 프로필을 [mcp_servers.ctr]에 켰습니다 — 별도 [mcp_servers.ctr-exec]에 승인 모드를 걸어 두었다면 그 키는 서버 테이블 단위라 이 경로에는 걸리지 않습니다(같은 두 도구에 게이트 없는 두 번째 경로가 생깁니다)")
			} else {
				fmt.Fprintln(stdout, "hook install (codex): [mcp_servers.ctr]의 enabled_tools에 exec 도구가 이미 포함돼 있습니다(이번 실행이 켠 것이 아닙니다) — 별도 [mcp_servers.ctr-exec]에 승인 모드를 걸어 두었다면 그 키는 서버 테이블 단위라 이 경로에는 걸리지 않습니다(같은 두 도구에 게이트 없는 두 번째 경로가 있습니다)")
			}
		}
		if res.ArgsKept {
			fmt.Fprintln(stdout, "hook install (codex): 기존 args를 프로필로 해석하지 못해 args·enabled_tools를 그대로 두었습니다(command와 소유 표식만 갱신) — 프로필을 바꾸려면 --enable로 명시하세요")
		}
	}
	n := len(codexRegistrations)
	if mcpConfirmed {
		n++
	}
	fmt.Fprintf(stdout, "hook install (codex): %d개 이벤트 등록 완료 — Codex에서 /hooks로 훅을 리뷰·신뢰해야 실행됩니다(정의 변경 시 재신뢰)\n", n)
	return nil
}

// reportCodexMCPState — config.toml 병합 결과 상태별 안내(스펙 §3). 확정(written/existingHeader)은
// 정보성, 미확정(conflict/markerAnomaly)은 수동 조치 안내 — 가드 등록 보류 사유를 명시한다.
func reportCodexMCPState(stdout io.Writer, state codexMCPState) {
	switch state {
	case mcpWritten:
		fmt.Fprintln(stdout, "hook install (codex): MCP 관리 테이블 기입 완료 — Codex 재시작 시 반영")
	case mcpExistingHeader:
		fmt.Fprintln(stdout, "hook install (codex): 표식도 없고 command도 우리 것이 아닌 [mcp_servers.ctr] 감지 — 기입 생략(사용자 등록으로 봅니다. doctor [10]과 대조해 정리한 뒤 재실행하세요)")
	case mcpConflict:
		fmt.Fprintln(stdout, "hook install (codex): config.toml에 ctr 관련 흔적 감지 — MCP 기입·가드 등록 보류. doctor 스니펫으로 수동 등록 후 재실행하세요")
	case mcpMarkerAnomaly:
		fmt.Fprintln(stdout, "hook install (codex): 관리 테이블 중복 정의 — config.toml 무변경·가드 등록 보류. 중복 헤더 정리 후 재실행하세요")
	}
}

// runHookUninstall — uninstall 서브커맨드(설계 §7). 자기 항목(마커+명령 정확 일치)만 대칭
// 제거한다. 설정 파일이 없으면 no-op.
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
	// settings.json 미존재는 no-op으로 알리되 .mcp.json 정리는 그와 무관하게 시도한다 — 부분 설치
	// (settings만 수동 삭제)에서 우리 등록이 영구 잔존하는 것을 막는다. runHookUninstallCodex가
	// config.toml에 대해 이미 채택한 규칙(Codex P2)과 같다.
	// 훅 설정 정리 실패도 그 자리에서 반환하지 않는다 — 미존재와 같은 이유다(리뷰 T6-2). 실패를
	// 보고하고 아래 정리를 마친 뒤 마지막에 hookErr을 반환해 종료코드가 실패를 반영하게 한다.
	var hookErr error
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		fmt.Fprintln(stdout, "hook uninstall: 설정 파일 없음 — 제거할 항목 없음")
	} else if merged, mergeErr := mergeSettingsFile(path, "", "", false); mergeErr != nil { // command/marker는 제거 경로에서 미사용
		fmt.Fprintln(stdout, "hook uninstall: 훅 설정 정리 실패 — .mcp.json 정리는 계속합니다")
		hookErr = mergeErr
	} else if writeErr := atomicWriteFile(path, merged); writeErr != nil {
		fmt.Fprintln(stdout, "hook uninstall: 훅 설정 쓰기 실패 — .mcp.json 정리는 계속합니다")
		hookErr = errors.New("hook uninstall: 설정 쓰기 실패")
	} else {
		fmt.Fprintln(stdout, "hook uninstall: 훅 항목 제거 완료")
	}

	// .mcp.json 항목 제거 — install의 대칭(D64). 소유 관문은 양방향이라 우리 이름 자리에 남의
	// 항목이 있으면 install과 똑같이 손대지 않고 사유만 보고한다. 파일은 지우지 않는다.
	// 대체된 과거 등록 이름(D63 ②)도 install과 같은 소유 기준으로 함께 지운다 — 그쪽 정리가 install
	// 분기에만 있으면 대칭이 아니고, 우리 명령을 가리키는 옛 항목이 제거 뒤에도 영구 잔존한다.
	mcpPath := mcpConfigPath(projectRoot)
	if existing, readErr := os.ReadFile(mcpPath); readErr == nil {
		if mcpMerged, changed, mergeErr := mergeMCPServers(existing, ctrMCPServerName, mcpServerEntry{}, false, true); mergeErr != nil {
			fmt.Fprintf(stdout, "mcp: .mcp.json 항목 제거를 멈췄습니다 — %q 이름에 우리가 소유하지 않은 항목이 있거나 파일을 해석할 수 없습니다. 그 항목은 그대로 두었습니다\n", ctrMCPServerName)
		} else if !changed {
			// 우리 항목이 하나도 없으면 쓰지 않는다 — 재기록은 바이트 중립이 아니어서(키 정렬·유니코드
			// 이스케이프) 남의 서버만 담긴 손으로 쓴 파일을 우리가 고쳐 놓게 된다. 문면도 하지 않은
			// 일을 했다고 말하지 않는다(형제 승인 키 문면이 이미 "없었으면 무변"으로 유보한다).
			// "하나도"인 이유: changed는 현재 이름과 대체된 과거 등록 이름 중 어느 것을 지워도 참이라,
			// 이 분기는 둘 다 없었을 때만 온다.
			fmt.Fprintf(stdout, "mcp: .mcp.json에 우리 항목(%q·대체된 과거 등록 이름)이 없어 파일을 그대로 두었습니다\n", ctrMCPServerName)
		} else if writeErr := atomicWriteFile(mcpPath, mcpMerged); writeErr != nil {
			fmt.Fprintln(stdout, "mcp: .mcp.json 기록 실패 — 훅 항목 제거는 완료되었습니다")
		} else {
			// 지운 것이 현재 이름일 수도, 대체된 과거 등록 이름일 수도 있다 — 둘 다 changed를 올리므로
			// 없던 이름까지 지웠다고 말하지 않는다(위 "없었으면 무변"과 같은 유보다).
			fmt.Fprintf(stdout, "mcp: .mcp.json 항목 제거 완료 — %q와 대체된 과거 등록 이름 중 있던 것만 지웠습니다\n", ctrMCPServerName)
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		fmt.Fprintln(stdout, "mcp: 기존 설정을 읽지 못해 .mcp.json 정리를 건너뜁니다")
	}

	// 승인 키 — 우리가 쓴 스코프(이번 uninstall이 겨냥한 그 파일)에서만 우리 이름을 뺀다. 다른
	// 스코프에는 애초에 쓰지 않았으므로 건드리지 않는다. 파일이 없으면 지울 것도 없다(만들지 않는다).
	// 훅 쓰기가 끝난 뒤 다시 읽는다 — 순서가 뒤집히면 훅 쓰기가 이 편집을 덮는다.
	// --user는 install이 이 키를 쓰지 않는 스코프다(위 --user 분기) — 그 파일의 목록은 사용자가 직접
	// 넣은 것이므로 제거도 하지 않는다. "설치가 쓸 수 있었던 스코프만 되돌린다"의 대칭이다.
	if *user {
		fmt.Fprintln(stdout, "mcp: --user 제거는 승인 키를 건드리지 않았습니다 — --user 설치가 그 키를 쓰지 않으므로 사용자 스코프의 enabledMcpjsonServers는 직접 넣은 목록입니다")
	} else if prev, prevErr := os.ReadFile(path); prevErr == nil {
		// keepEmpty: 우리 이름을 빼서 배열이 비더라도 하위 스코프가 같은 키를 정의하면 키를 지우지
		// 않는다 — 지우는 순간 그 목록이 살아나 이 프로젝트에 넣지 않은 이름이 승인된다.
		if next, mergeErr := mergeEnabledServers(prev, ctrMCPServerName, false, lowerScopeDefinesEnabled(projectRoot, os.ReadFile)); mergeErr != nil {
			fmt.Fprintln(stdout, "mcp: 승인 키 병합 실패 — 훅 항목 제거는 완료되었습니다")
		} else if writeErr := atomicWriteFile(path, next); writeErr != nil {
			fmt.Fprintln(stdout, "mcp: 승인 키 기록 실패 — 훅 항목 제거는 완료되었습니다")
		} else {
			fmt.Fprintf(stdout, "mcp: enabledMcpjsonServers에서 %q를 제거했습니다(없었으면 무변)\n", ctrMCPServerName)
		}
	} else if !errors.Is(prevErr, os.ErrNotExist) {
		fmt.Fprintln(stdout, "mcp: 기존 settings를 읽지 못해 승인 키 정리를 건너뜁니다")
	}
	return hookErr
}

// runHookUninstallCodex — --codex 제거 대칭(스펙 v0.7 §3, D47+D48). hooks.json 자기 그룹(가드
// PreToolUse 포함)을 소거한 뒤 config.toml 관리 블록을 제거한다. hooks.json 미존재는 "제거할 항목
// 없음"만 알리고 오류 없이 넘어가되(no-op), config.toml 정리는 그와 무관하게 항상 시도한다 —
// 부분 설치(config만 쓰이고 hooks.json은 실패·수동 삭제)에서 관리 블록이 영구 잔존하는 것을
// 막는다(Codex P2). config 제거는 소유 블록이 있을 때만 관용적으로 수행.
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
	} else {
		merged, mErr := mergeCodexHooks(existing, "", "", false, false)
		if mErr != nil {
			return fmt.Errorf("hook: 설정 병합 실패: %w", mErr)
		}
		if wErr := atomicWriteFile(path, merged); wErr != nil {
			return errors.New("hook uninstall: 설정 쓰기 실패")
		}
		fmt.Fprintln(stdout, "hook uninstall (codex): 훅 항목 제거 완료")
	}
	// config.toml 관리 블록 제거는 hooks.json 유무와 무관하게 항상 시도한다(부분 설치 잔존 방지, P2).
	if cfgPath, cErr := codexConfigPath(); cErr == nil {
		if cfgExisting, rErr := os.ReadFile(cfgPath); rErr == nil {
			if cfgOut, changed := uninstallCodexConfigBlock(cfgExisting); changed {
				// D84 — "내용을 바꾸기 직전 config.toml.bak 단일 슬롯"은 제거 경로에도 선다
				// (리뷰 P1). install·doctor --fix와 같은 규칙이어야 잘못된 제거를 되돌릴 수 있고,
				// changed 판정 뒤에 두므로 무변경 재실행이 단일 슬롯을 덮지 않는다.
				if bErr := backupCodexConfig(cfgPath, cfgExisting); bErr != nil {
					return errors.New("hook uninstall: config.toml 백업 실패")
				}
				if wErr := atomicWriteFile(cfgPath, cfgOut); wErr != nil {
					return errors.New("hook uninstall: config.toml 쓰기 실패")
				}
				fmt.Fprintln(stdout, "hook uninstall (codex): MCP 등록 블록 제거 완료 — 다른 프로젝트가 Codex 가드를 계속 사용하면 그 프로젝트에서 hook install --codex 재실행으로 재기입하세요")
			}
		}
	}
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
// mergeCodexHooks와 동일한 {"hooks":{event:[group...]}} 래퍼로 파싱한다(형제 구조 미러링).
// 파일 부재·hooks 부재는 (0,"",nil) — 미설치 정보 분기. isOurCodexGroup(전건 판정)을 그대로
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
