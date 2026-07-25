// Package-level: .mcp.json(MCP 서버 등록) 병합. hook_install.go의 멱등 병합·소유 판정
// 철학을 따르되 대상 파일·스키마가 달라 분리한다(설계 v0.12 D64).
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ctrMCPServerName — .mcp.json에 등록하는 서버 이름. 설계 D63 ②가 단일 서버 등록을
// 표준으로 고정했다. 이름이 바뀌면 permission 규칙 접두와 doctor 안내 문면이 함께
// 움직이므로 이 상수 한 곳에서만 정한다.
const ctrMCPServerName = "ctr-exec"

// supersededMCPServerNames — 단일 서버 등록(D63 ②)이 대체하는 과거 등록 이름. ctr의 6도구는
// ctr-exec의 8도구에 완전히 포함되므로 둘을 함께 두면 6도구가 중복 노출된다. ctr-global은 다른
// 프로필(global-search)이라 대체 대상이 아니다 — 설치기가 만들지도, 지우지도 않는다.
var supersededMCPServerNames = []string{"ctr"}

// mcpServerEntry — .mcp.json의 stdio 서버 항목. alwaysLoad는 호스트가 도구를 세션
// 시작 시 상주시키게 한다(지연 로드 면제). Managed는 훅 설정과 같은 버전 마커
// (hookMarker: "context-router/<version>")로, 소유 판정과 self-heal의 근거다.
type mcpServerEntry struct {
	Command    string   `json:"command"`
	Args       []string `json:"args"`
	AlwaysLoad bool     `json:"alwaysLoad,omitempty"`
	Managed    string   `json:"__ctrManaged,omitempty"`
}

// mcpConfigPath — 프로젝트 루트의 .mcp.json 경로.
func mcpConfigPath(projectRoot string) string {
	return filepath.Join(projectRoot, ".mcp.json")
}

// mergeMCPServers — existing(빈 슬라이스 허용)에 name 항목을 install 여부에 따라
// 반영한 JSON을 반환한다. 다른 서버 항목은 원문 그대로 보존한다(json.RawMessage).
// 키 순서가 결정적이도록 map을 그대로 마샬링한다(encoding/json이 키를 정렬한다) —
// 멱등 비교가 바이트 단위로 성립하는 근거다.
//
// setProfile=false면 기존 우리 항목의 args를 그대로 유지한다 — 플래그 없이 실행한 재설치가
// 이미 켜둔 exec 프로필을 끄지 않게 하는 지점이다(마커는 setProfile과 무관하게 항상 현재 값으로
// 덮어쓴다 — self-heal). install은 대체된 과거 등록도 함께 정리한다(D63 ② 단일 서버).
func mergeMCPServers(existing []byte, name string, entry mcpServerEntry, install, setProfile bool) ([]byte, error) {
	doc := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(existing)) > 0 { // 공백뿐인 파일은 빈 병합 기반(mergeHookSettings:125 형제)
		if err := json.Unmarshal(existing, &doc); err != nil {
			return nil, errors.New("mcp: 설정 파싱 실패") // 경로·원문 미포함
		}
		if doc == nil { // JSON `null` → Unmarshal이 맵을 nil로 설정(할당 시 패닉 — 최종 리뷰 C5 형제)
			doc = map[string]json.RawMessage{}
		}
	}
	servers := map[string]json.RawMessage{}
	if raw, ok := doc["mcpServers"]; ok {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return nil, errors.New("mcp: mcpServers 파싱 실패")
		}
		if servers == nil { // `{"mcpServers":null}` 동일 경로
			servers = map[string]json.RawMessage{}
		}
	}
	// 우리 이름 자리의 기존 항목은 소유가 확인될 때만 교체·삭제한다 — 마커 접두(hookMarkerPrefix)
	// 또는 우리 명령 중 하나면 우리 것으로 본다. 둘 다 아니면 사용자가 직접 넣은 남의 항목이라
	// install·uninstall 모두 손대지 않고 충돌로 알린다(isOurHookGroup:95의 "마커 없는 수동 항목은
	// 보존" 철학 — 파손 금지 > 멱등 완전성). 훅과 달리 AND가 아니라 OR인 이유는 마커 이전 버전이
	// 남긴 우리 항목(마커 없음·명령 일치)도 우리 것이라 대칭 제거 대상이기 때문이다.
	var prev mcpServerEntry
	var prevExists bool
	if raw, ok := servers[name]; ok {
		if err := json.Unmarshal(raw, &prev); err != nil {
			return nil, errors.New("mcp: 기존 항목 파싱 실패")
		}
		if !strings.HasPrefix(prev.Managed, hookMarkerPrefix()) && prev.Command != hookBinaryName {
			return nil, errors.New("mcp: 같은 이름의 다른 서버 항목이 있어 갱신을 멈춘다")
		}
		prevExists = true
	}
	if install {
		if prevExists && !setProfile {
			entry.Args = prev.Args // 프로필 유지 — 명시 플래그 없이는 profile을 바꾸지 않는다
		}
		if entry.Args == nil {
			entry.Args = []string{} // "args": [] 고정 — nil은 null로 나가 멱등 비교가 흔들린다
		}
		// 대체된 과거 등록 정리 — 우리 명령을 가리키는 것만 지운다(같은 이름의 남의 서버 보호).
		for _, old := range supersededMCPServerNames {
			if old == name {
				continue
			}
			raw, ok := servers[old]
			if !ok {
				continue
			}
			var prev mcpServerEntry
			if err := json.Unmarshal(raw, &prev); err == nil && prev.Command == hookBinaryName {
				delete(servers, old)
			}
		}
		b, err := json.Marshal(entry)
		if err != nil {
			return nil, errors.New("mcp: 항목 직렬화 실패")
		}
		servers[name] = b
	} else {
		delete(servers, name)
	}
	sb, err := json.Marshal(servers)
	if err != nil {
		return nil, errors.New("mcp: mcpServers 직렬화 실패")
	}
	doc["mcpServers"] = sb
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, errors.New("mcp: 설정 직렬화 실패")
	}
	return append(out, '\n'), nil
}

// enabledServersScope — enabledMcpjsonServers 키를 정의한 스코프를 우선순위 순으로 조사한다.
// 이 키는 permission 규칙과 달리 스코프 간 병합되지 않고 최상위 정의가 하위를 덮으므로,
// 낮은 스코프에 쓰면 조용히 무시된다(설계 D64). readFile은 테스트 주입 seam이다.
func enabledServersScope(projectRoot string, readFile func(string) ([]byte, error)) (string, []string, error) {
	userPath, err := hookSettingsPath(true, projectRoot)
	if err != nil {
		return "", nil, err
	}
	projectPath, err := hookSettingsPath(false, projectRoot)
	if err != nil {
		return "", nil, err
	}
	localPath := filepath.Join(projectRoot, ".claude", "settings.local.json")

	var defined []string
	winner := ""
	// 높은 우선순위부터 — local(가장 좁음) > project > user. 관리자 정책·CLI 인자 스코프는
	// local보다 높지만 단일 사용자 로컬 도구의 판정 범위 밖이라 보지 않는다.
	for _, p := range []string{localPath, projectPath, userPath} {
		b, err := readFile(p)
		if err != nil {
			continue // 미존재·읽기 실패는 "정의 없음"으로 본다
		}
		var doc struct {
			Enabled []string `json:"enabledMcpjsonServers"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			continue // 깨진 파일은 이 판정에서 무시한다(설치기가 건드리지 않는다)
		}
		if doc.Enabled == nil {
			continue
		}
		defined = append(defined, p)
		if winner == "" {
			winner = p
		}
	}
	return winner, defined, nil
}

// permissionRules — settings 파일에서 읽는 규칙 배열.
type permissionRules struct {
	Permissions struct {
		Ask   []string `json:"ask"`
		Allow []string `json:"allow"`
	} `json:"permissions"`
}

// ruleMatches — ask 규칙 r이 allow 규칙 a가 가리키는 도구를 덮는가. 리터럴 완전 일치와
// 도구 위치 접미 glob("mcp__server__prefix_*")만 다룬다. 서버 세그먼트에는 glob이 오지 않는다.
// 판정은 mcp__ 접두 규칙에 한정한다: 매칭 규칙이 그 형태에만 정의돼 있고, 비-MCP 규칙(Read/Edit
// 형태)은 인자에 절대경로를 담을 수 있어 진단 라인에 그대로 실리면 안 된다(리뷰 F5, §12).
// 두 인자 모두를 걸러 여기 한 곳에서 비교 범위와 출력 범위가 함께 좁혀진다.
func ruleMatches(r, a string) bool {
	if !strings.HasPrefix(r, "mcp__") || !strings.HasPrefix(a, "mcp__") {
		return false
	}
	if r == a {
		return true
	}
	if !strings.HasSuffix(r, "*") {
		return false
	}
	return strings.HasPrefix(a, strings.TrimSuffix(r, "*"))
}

// askShadowedAllows — 모든 스코프의 permissions를 모아, ask가 덮는 allow 항목을 보고한다.
// 규칙 평가가 deny→ask→allow 순이라 이 조합에서 allow는 효력이 없다(설계 v0.12 D64).
// permission 규칙은 스코프 간 concat+dedup으로 병합되므로 전 스코프를 합쳐 판정한다 —
// enabledServersScope와 달리 덮어쓰기가 아니라 합집합이라 순회 순서가 결과를 바꾸지 않는다.
func askShadowedAllows(projectRoot string, readFile func(string) ([]byte, error)) ([]string, error) {
	userPath, err := hookSettingsPath(true, projectRoot)
	if err != nil {
		return nil, err
	}
	projectPath, err := hookSettingsPath(false, projectRoot)
	if err != nil {
		return nil, err
	}
	localPath := filepath.Join(projectRoot, ".claude", "settings.local.json")

	// 확인하지 못한 스코프는 "규칙 없음"이 아니다 — 조용히 건너뛰면 doctor가 거짓 clean을
	// 찍는다(리뷰 F1). 미존재만 확인된 상태("그 스코프에 규칙 없음")로 보고, 그 밖의 읽기·파싱
	// 실패는 판정 불가로 올린다. 오류 문면에는 경로·원문을 담지 않는다(§12).
	var asks, allows []string
	for _, p := range []string{userPath, projectPath, localPath} {
		b, err := readFile(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, errors.New("permissions: 설정 파일 읽기 실패")
		}
		var doc permissionRules
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, errors.New("permissions: 설정 파싱 실패")
		}
		asks = append(asks, doc.Permissions.Ask...)
		allows = append(allows, doc.Permissions.Allow...)
	}
	var shadowed []string
	seen := map[string]bool{}
	for _, a := range allows {
		for _, r := range asks {
			if ruleMatches(r, a) && !seen[a] {
				seen[a] = true
				shadowed = append(shadowed, a)
				break
			}
		}
	}
	return shadowed, nil
}
