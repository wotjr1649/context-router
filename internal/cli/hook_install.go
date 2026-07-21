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
	"sort"
	"strings"

	"github.com/wotjr1649/context-router/internal/hook"
)

const (
	hookBinaryName = "context-router"    // PATH 실행 파일명(=훅 명령 첫 토큰) — ctr은 MCP 등록 키일 뿐(설계 §7)
	hookTimeoutSec = 10                  // 등록 timeout(단위 초 — T0 확인, command 기본 600초)
	dropsFileName  = "session.drops.log" // internal/hook dropsLogName(unexported) 미러 — doctor drops 집계용
)

// hookRegistrations — 설치 대상 4항목(T0-amended 설계 §7). matcher가 빈 문자열이면 전체 매칭
// (SessionStart=시작 방식 전체, Pre/PostToolUse(Failure)=전 도구 — T0 §4). PreToolUse는
// Read|Bash|PowerShell 정규식 매칭(large-read/dump guard 대상 — v0.4 D36) — 관리 그룹 1개 유지가
// merge의 동일-이벤트 상호 제거 함정을 회피한다(설계 v0.3 §4).
var hookRegistrations = []struct {
	event   string
	matcher string
}{
	{"SessionStart", ""},
	{"PreToolUse", "Read|Bash|PowerShell"},
	{"PostToolUse", ""},
	{"PostToolUseFailure", ""},
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

// isOurHookGroup — 자기 소유 그룹 판정: 소유권 마커(접두사) AND 명령 토큰 정확 일치를 모두
// 요구한다(결합, 설계 §7). 마커 없는 수동 항목·접두사만 닮은 `context-router hook-wrapper`는
// 어느 한 조건에서 탈락해 보존된다(오삭제 방지).
func isOurHookGroup(raw json.RawMessage) bool {
	var p hookGroupProbe
	if json.Unmarshal(raw, &p) != nil {
		return false
	}
	if !strings.HasPrefix(p.Managed, hookMarkerPrefix()) {
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
// 마커 접두)일 때만 자기 그룹으로 판정한다(전건 판정 — §11.2 F4). Claude 쪽 isOurHookGroup은
// 그룹 레벨 __ctrManaged 마커가 소유를 표시하지만 Codex hooks.json은 미지 필드 금지라 항목
// 레벨 추론뿐이므로, any-판정이면 사용자가 항목을 추가한 혼합 그룹까지 통째로 지워진다 —
// 혼합 그룹은 불가침(파손 금지 > 멱등 완전성, 잔존 정리는 사용자 /hooks 몫).
func isOurCodexGroup(raw json.RawMessage) bool {
	var p codexGroupProbe
	if json.Unmarshal(raw, &p) != nil {
		return false
	}
	if len(p.Hooks) == 0 {
		return false
	}
	for _, h := range p.Hooks {
		if !isCodexHookCommandToken(h.Command) || !strings.HasPrefix(h.StatusMessage, hookMarkerPrefix()) {
			return false
		}
	}
	return true
}

// buildCodexHookCommand — buildHookCommand의 Codex 형제: 러닝 서브커맨드가 `codex-hook`이다
// (D35 호스트 경계 + §11.2 F3 버전 게이트). 인용 규칙은 T11 관례 승계(가정 — Codex 훅 명령
// 파싱 규칙은 실측 전, 도그푸딩은 --store-root 미명시 경로만 사용).
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

// mergeCodexHooks — mergeHookSettings의 Codex 형제(D28 원칙 승계: 멱등·타 항목/미지 키 raw
// 보존·제거 대칭·빈 컨테이너 정리). 등록 집합·그룹 타입·소유 판정만 다르다.
func mergeCodexHooks(existing []byte, command, marker string, install bool) ([]byte, error) {
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
	for _, reg := range codexRegistrations {
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

// runHookInstall — install 서브커맨드(설계 §7). --user/--no-shadow만 자체 flagset으로 파싱한다
// (--root/--store-root는 main의 prescanRootFlags가 이미 소비·전달 — storeRootExplicit/Raw).
func runHookInstall(args []string, storeRoot, storeRootRaw string, storeRootExplicit bool, projectRoot, version string, stdout io.Writer) error {
	fs := flag.NewFlagSet("hook install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	user := fs.Bool("user", false, "~/.claude/settings.json에 등록(기본: 프로젝트)")
	noShadow := fs.Bool("no-shadow", false, "Shadow Recall 비활성(훅 명령에 --no-shadow 주입)")
	codex := fs.Bool("codex", false, "Codex CLI hooks.json에 등록(기본: Claude settings.json)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("hook install: 플래그 파싱 실패: %w", err)
	}
	if rest := fs.Args(); len(rest) > 0 {
		return fmt.Errorf("hook install: 예상치 않은 인자 %d개", len(rest))
	}
	if *codex {
		path, err := codexHooksPath(*user, projectRoot)
		if err != nil {
			return err
		}
		if storeRootExplicit && storeRootRaw != "" {
			if abs, absErr := filepath.Abs(storeRootRaw); absErr == nil {
				storeRootRaw = abs
			}
		}
		command := buildCodexHookCommand(storeRootExplicit, storeRootRaw, *noShadow)
		existing, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("hook: 설정 파일 읽기 실패")
		}
		merged, err := mergeCodexHooks(existing, command, hookMarker(version), true)
		if err != nil {
			return fmt.Errorf("hook: 설정 병합 실패: %w", err)
		}
		if err := atomicWriteFile(path, merged); err != nil {
			return errors.New("hook install: 설정 쓰기 실패")
		}
		fmt.Fprintf(stdout, "hook install (codex): %d개 이벤트 등록 완료 — Codex에서 /hooks로 훅을 리뷰·신뢰해야 실행됩니다\n", len(codexRegistrations))
		return nil
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
	merged, err := mergeSettingsFile(path, command, hookMarker(version), true)
	if err != nil {
		return err
	}
	if err := atomicWriteFile(path, merged); err != nil {
		return errors.New("hook install: 설정 쓰기 실패")
	}
	fmt.Fprintf(stdout, "hook install: %d개 이벤트 등록 완료\n", len(hookRegistrations))
	return nil
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
		path, err := codexHooksPath(*user, projectRoot)
		if err != nil {
			return err
		}
		existing, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintln(stdout, "hook uninstall (codex): 설정 파일 없음 — 제거할 항목 없음")
				return nil
			}
			return errors.New("hook: 설정 파일 읽기 실패")
		}
		merged, err := mergeCodexHooks(existing, "", "", false)
		if err != nil {
			return fmt.Errorf("hook: 설정 병합 실패: %w", err)
		}
		if err := atomicWriteFile(path, merged); err != nil {
			return errors.New("hook uninstall: 설정 쓰기 실패")
		}
		fmt.Fprintln(stdout, "hook uninstall (codex): 훅 항목 제거 완료")
		return nil
	}
	path, err := hookSettingsPath(*user, projectRoot)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		fmt.Fprintln(stdout, "hook uninstall: 설정 파일 없음 — 제거할 항목 없음")
		return nil
	}
	merged, err := mergeSettingsFile(path, "", "", false) // command/marker는 제거 경로에서 미사용
	if err != nil {
		return err
	}
	if err := atomicWriteFile(path, merged); err != nil {
		return errors.New("hook uninstall: 설정 쓰기 실패")
	}
	fmt.Fprintln(stdout, "hook uninstall: 훅 항목 제거 완료")
	return nil
}

// scanRegisteredHooks — path의 settings에서 자기 소유 훅 그룹 수와 마커 버전을 함께 수집한다
// (doctor [9] 등록 상태 + 버전 불일치 감지). 마커 버전은 첫 자기 그룹의 __ctrManaged에서
// hookMarkerPrefix 뒤 부분이다 — 한 번의 install이 4개 그룹을 동일 버전 마커로 쓰므로(§7) 어느
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
					marker = strings.TrimPrefix(p.Managed, hookMarkerPrefix())
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

// dropsByReason — drops.log을 사유별로 집계한다(doctor [12]). appendDrop 계약은 정확히
// "<unix초>\t<사유>"(fmt "%d\t%s") — ts가 1+ 숫자이고 사유가 비지 않으며 탭을 포함하지 않는(정확히
// 2필드) 줄만 그 사유로 센다. 어긋나면(비숫자 ts·탭 초과 필드로 사유에 TAB 혼입 등) "unparsed" —
// 진단은 절대 중단하지 않고(설계 v0.3 §5), total은 빈 줄 포함 모든 줄을 센다(줄 수 계약). 파일 부재·
// 읽기 실패는 (0, nil) — 기존 countDropsLog와 동일한 fail-soft.
func dropsByReason(path string) (int, map[string]int) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil
	}
	defer func() { _ = f.Close() }()
	total, reasons := 0, map[string]int{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20) // 긴 줄에도 스캔 중단 방지(countDropsLog 관례 보존)
	for sc.Scan() {
		total++ // 빈 줄도 센다 — 기존 countDropsLog의 total 의미 보존(줄 수 계약)
		ts, reason, ok := strings.Cut(sc.Text(), "\t")
		if ok && reason != "" && !strings.Contains(reason, "\t") && isUnixTS(ts) {
			reasons[reason]++
		} else {
			reasons["unparsed"]++ // 빈 줄·비숫자 ts·탭 초과 필드·사유 없음 전부 unparsed
		}
	}
	return total, reasons
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

// formatDropCount — "N(사유=n,...)" 렌더(사유 알파벳순, 결정적). N==0이면 "0".
func formatDropCount(total int, reasons map[string]int) string {
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
		parts = append(parts, fmt.Sprintf("%s=%d", k, reasons[k]))
	}
	return fmt.Sprintf("%d(%s)", total, strings.Join(parts, ","))
}
