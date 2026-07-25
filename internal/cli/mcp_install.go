// Package-level: .mcp.json(MCP 서버 등록) 병합. hook_install.go의 멱등 병합·소유 판정
// 철학을 따르되 대상 파일·스키마가 달라 분리한다(설계 v0.12 D64).
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
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
	if install {
		if raw, ok := servers[name]; ok {
			var prev mcpServerEntry
			if err := json.Unmarshal(raw, &prev); err != nil {
				return nil, errors.New("mcp: 기존 항목 파싱 실패")
			}
			if !setProfile {
				entry.Args = prev.Args // 프로필 유지 — 명시 플래그 없이는 profile을 바꾸지 않는다
			}
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
