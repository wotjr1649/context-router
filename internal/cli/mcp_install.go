// Package-level: .mcp.json(MCP 서버 등록) 병합. hook_install.go의 멱등 병합·소유 판정
// 철학을 따르되 대상 파일·스키마가 달라 분리한다(설계 v0.12 D64).
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
// 이 4필드가 install이 소유하는 전부다 — 사용자가 우리 항목에 직접 넣은 그 밖의 키는
// ctrMCPEntryKeys 기준으로 왕복 보존한다(mergeMCPServers).
// alwaysLoad에 omitempty를 달지 않는다 — 사용자가 명시한 false를 그대로 내보내야 한다.
// 지우고 나면 다음 재설치가 "명시 없음"으로 읽어 기본값 true로 되살리므로 재설치마다 값이
// 진동한다(mergeMCPServers의 alwaysLoad 유지 규칙과 한 쌍).
type mcpServerEntry struct {
	Command    string   `json:"command"`
	Args       []string `json:"args"`
	AlwaysLoad bool     `json:"alwaysLoad"`
	Managed    string   `json:"__ctrManaged,omitempty"`
}

// ctrMCPEntryKeys — install이 소유하는(재설치마다 현재 값으로 덮어쓰는) 항목 키. mcpServerEntry의
// json 태그와 1:1이다. 여기 없는 키는 사용자 것이라 원문 그대로 되돌린다 — 4필드 구조체로 재마샬링만
// 하던 이전 형태는 "env"(CTR_SHADOW_RETENTION·CTR_STORE_ROOT처럼 현실적인 설정)·"cwd"·"type"을 매
// hook install마다 조용히 버렸다(최종 리뷰 F4).
var ctrMCPEntryKeys = []string{"command", "args", "alwaysLoad", "__ctrManaged"}

// keepUnownedEntryKeys — 새 항목(ours)에 기존 항목 원문(prev)의 우리 소유가 아닌 키를 되돌린다.
// 되돌릴 키가 없으면 ours를 그대로 준다 — 흔한 경로의 출력 형태(구조체 필드 순서)를 바꾸지 않는다.
// 되돌릴 키가 있으면 map 마샬링이라 키가 정렬되는데, 그 형태도 같은 입력에 같은 출력이므로 재실행
// 바이트 동일성(TestMergeMCPServersIdempotent)은 두 경로 모두에서 성립한다.
func keepUnownedEntryKeys(prev map[string]json.RawMessage, ours []byte) ([]byte, error) {
	merged := map[string]json.RawMessage{}
	for k, v := range prev {
		if !slices.Contains(ctrMCPEntryKeys, k) {
			merged[k] = v
		}
	}
	if len(merged) == 0 {
		return ours, nil
	}
	// 우리 4키를 사용자 키 위에 올린다 — Unmarshal은 비어 있지 않은 맵의 기존 항목을 보존한다.
	if err := json.Unmarshal(ours, &merged); err != nil {
		return nil, errors.New("mcp: 항목 직렬화 실패")
	}
	b, err := json.Marshal(merged)
	if err != nil {
		return nil, errors.New("mcp: 항목 직렬화 실패")
	}
	return b, nil
}

// isOurMarkerValue — 소유 표식 값의 기준(D82·D84). **정확 일치 `context-router`**(무버전 —
// D82 이후 훅 등록물과 hostSnippet이 쓰는 값)이거나 **`context-router/` 접두**(버전 있는 값)일
// 때만 소유다. 키의 존재만으로는 소유가 아니다. 정확 일치 절은 과도기 장치가 아니라 영구
// 본절이며 D84의 v1.0 제거 대상에서 제외한다 — 지우면 v0.15 이후 설치본이 소유 판정에서
// 탈락해 대칭 제거가 깨진다.
func isOurMarkerValue(v string) bool {
	return v == hookBinaryName || strings.HasPrefix(v, hookMarkerPrefix())
}

// mcpConfigPath — 프로젝트 루트의 .mcp.json 경로.
func mcpConfigPath(projectRoot string) string {
	return filepath.Join(projectRoot, ".mcp.json")
}

// mcpProfileNames — 설치기가 아는 프로필 이름(D81). 이 순서가 args의 쉼표 목록과
// enabled_tools의 추가 순서를 함께 정한다 — 두 값이 같은 입력에서 같은 순서로 도출되므로
// 골든이 입력 순서에 흔들리지 않는다. 서버 쪽 NewServer의 Enable 분기는 손대지 않는다.
var mcpProfileNames = []string{"ingest", "net", "exec"}

// defaultMCPProfiles — 플래그 없는 **첫** 설치가 쓰는 기본 프로필(D81 개정 — 앞선 초안의
// "기본값은 프로필 없음"을 대체한다). ctr_index·ctr_fetch_and_index 미등록이 세 세션 이월된
// 원인이 이 기본값이었다. exec는 --enable-exec 명시 opt-in을 유지한다(D58·D59·D64).
var defaultMCPProfiles = []string{"ingest", "net"}

// canonicalProfiles — 아는 이름만 mcpProfileNames 순서로 중복 없이 남긴다(정규화 단일 지점).
func canonicalProfiles(profiles []string) []string {
	var out []string
	for _, name := range mcpProfileNames {
		if slices.Contains(profiles, name) {
			out = append(out, name)
		}
	}
	return out
}

// mcpArgsForProfiles — 등록물 args. 프로필이 비면 nil을 준다 — Codex 갈래는 그 경우 args 키
// 자체를 쓰지 않고(재직렬화가 args = []를 지우므로 매 실행이 같은 줄을 되쓴다, D80),
// .mcp.json 갈래는 mergeMCPServers가 nil을 []로 정규화한다(그쪽은 키가 사라지면 멱등
// 비교가 흔들린다).
func mcpArgsForProfiles(profiles []string) []string {
	list := canonicalProfiles(profiles)
	if len(list) == 0 {
		return nil
	}
	return []string{"--enable", strings.Join(list, ",")}
}

// enabledToolsForProfiles — Codex enabled_tools의 **정적** 목록(D81·§1.3-3). 런타임 등록
// 결과가 아니다 — ctr_transform은 transform.ProbeIsolation, exec 2종은 sandbox.Probe 통과
// 시에만 실제로 등록되고 세션 3종은 cfg.Session이 있을 때만 등록되므로, 런타임을 기준으로
// 삼으면 등록물이 호스트마다 갈린다. args와 이 목록이 같은 입력에서 도출되는 것이 계약이다 —
// 도구가 늘어나는데 allowlist가 그대로면 프로필을 켜도 도구가 보이지 않는다.
func enabledToolsForProfiles(profiles []string) []string {
	tools := []string{
		"ctr_search", "ctr_fetch", "ctr_transform",
		"ctr_record_event", "ctr_session_summary", "ctr_export_events",
	}
	for _, name := range canonicalProfiles(profiles) {
		switch name {
		case "ingest":
			tools = append(tools, "ctr_index")
		case "net":
			tools = append(tools, "ctr_fetch_and_index")
		case "exec":
			tools = append(tools, "ctr_execute", "ctr_execute_file")
		}
	}
	return tools
}

// enabledToolsExposeExec — 최종 enabled_tools 목록이 exec 도구(ctr_execute·ctr_execute_file) 중
// 하나라도 담고 있는가(D81 설치기 안내용, 리뷰 승격 — 이월 T4-F3의 근본 픽스). Codex 갈래는
// 되읽기 실패 시(D81 ArgsKept) 이 목록을 프로필에서 다시 계산하지 않고 원문을 그대로 보존하므로
// "요청한 프로필에 exec가 있는가"와 "산출물이 실제로 노출하는가"가 갈릴 수 있다 — 안내 소비자는
// 후자를 봐야 한다.
func enabledToolsExposeExec(tools []string) bool {
	return slices.Contains(tools, "ctr_execute") || slices.Contains(tools, "ctr_execute_file")
}

// profilesFromArgs — 등록물의 args를 프로필 집합으로 되읽는다(D81 Codex 갈래). 우리가 쓰는
// 형태(["--enable", "<쉼표 목록>"])와 아는 이름만 인식하고, **부재와 []는 빈 프로필 집합**으로
// 되읽는다(D80 동치 규칙 — 현재 사용자 파일이 그 상태다). 그 밖의 형태는 ok=false이며,
// 호출자는 그때 args와 enabled_tools를 **둘 다 손대지 않는다** — 해석하지 못한 값을 기본
// 프로필로 덮으면 사용자가 켜 둔 프로필이 조용히 바뀐다.
func profilesFromArgs(args []string) (profiles []string, ok bool) {
	if len(args) == 0 {
		return nil, true
	}
	if len(args) != 2 || args[0] != "--enable" {
		return nil, false
	}
	for _, name := range strings.Split(args[1], ",") {
		if !slices.Contains(mcpProfileNames, strings.TrimSpace(name)) {
			return nil, false
		}
		profiles = append(profiles, strings.TrimSpace(name))
	}
	return canonicalProfiles(profiles), true
}

// mergeMCPServers — existing(빈 슬라이스 허용)에 name 항목을 install 여부에 따라
// 반영한 JSON을 반환한다. 다른 서버 항목은 원문 그대로 보존하고(json.RawMessage), 우리 항목
// 안에서도 우리가 소유하지 않은 키는 왕복 보존한다(ctrMCPEntryKeys·keepUnownedEntryKeys).
// 키 순서가 결정적이도록 map을 그대로 마샬링한다(encoding/json이 키를 정렬한다) —
// 멱등 비교가 바이트 단위로 성립하는 근거다.
//
// setProfile=false면 기존 우리 항목의 args를 그대로 유지한다 — 플래그 없이 실행한 재설치가
// 이미 켜둔 exec 프로필을 끄지 않게 하는 지점이다(마커는 setProfile과 무관하게 항상 현재 값으로
// 덮어쓴다 — self-heal). install은 대체된 과거 등록도 함께 정리한다(D63 ② 단일 서버).
//
// changed=false는 제거 경로에서 우리 항목이 애초에 없었다는 뜻이다 — 호출자가 쓰기와 "제거 완료"
// 문면을 함께 건너뛰게 하는 신호다(uninstallCodexConfigBlock의 changed와 같은 역할). 무변경 재기록은
// 바이트 중립이 아니다: 재마샬링이 키를 정렬하고 &를 유니코드 이스케이프로 바꾸므로, 우리 항목이
// 없는 남의 파일을 손대지 않으려면 이 신호가 필요하다. install 경로는 항상 true다 — 마커 self-heal이
// 있어 "무변경"이 성립하지 않는다.
func mergeMCPServers(existing []byte, name string, entry mcpServerEntry, install, setProfile bool) ([]byte, bool, error) {
	doc := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(existing)) > 0 { // 공백뿐인 파일은 빈 병합 기반(mergeHookSettings:125 형제)
		if err := json.Unmarshal(existing, &doc); err != nil {
			return nil, false, errors.New("mcp: 설정 파싱 실패") // 경로·원문 미포함
		}
		if doc == nil { // JSON `null` → Unmarshal이 맵을 nil로 설정(할당 시 패닉 — 최종 리뷰 C5 형제)
			doc = map[string]json.RawMessage{}
		}
	}
	servers := map[string]json.RawMessage{}
	if raw, ok := doc["mcpServers"]; ok {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return nil, false, errors.New("mcp: mcpServers 파싱 실패")
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
	var prevRaw map[string]json.RawMessage // 우리 소유가 아닌 키의 왕복 보존용 원문
	var prevExists bool
	if raw, ok := servers[name]; ok {
		if err := json.Unmarshal(raw, &prev); err != nil {
			return nil, false, errors.New("mcp: 기존 항목 파싱 실패")
		}
		if err := json.Unmarshal(raw, &prevRaw); err != nil {
			return nil, false, errors.New("mcp: 기존 항목 파싱 실패")
		}
		if !isOurMarkerValue(prev.Managed) && prev.Command != hookBinaryName {
			return nil, false, errors.New("mcp: 같은 이름의 다른 서버 항목이 있어 갱신을 멈춘다")
		}
		prevExists = true
	}
	changed := prevExists // 제거 경로의 신호 — install 경로는 아래에서 무조건 true로 올린다
	if install {
		changed = true
		if prevExists && !setProfile {
			entry.Args = prev.Args // 프로필 유지 — 명시 플래그 없이는 profile을 바꾸지 않는다
		}
		// 명시된 alwaysLoad도 args와 같은 규칙으로 유지한다 — 이 키는 서버가 연결될 때까지 세션
		// 시작을 막으므로(호스트 5초 상한) 사용자가 false로 끌 이유가 실재하고, 우리 소유 키라
		// keepUnownedEntryKeys가 되돌려 주지 않는다. 켜고 끄는 플래그가 없어 "명시돼 있으면 유지"가
		// 전부다. 키가 없으면(첫 등록·마커 이전 등록) 우리 기본값 true를 쓴다 — self-heal이다.
		if _, ok := prevRaw["alwaysLoad"]; ok {
			entry.AlwaysLoad = prev.AlwaysLoad
		}
		if entry.Args == nil {
			entry.Args = []string{} // "args": [] 고정 — nil은 null로 나가 멱등 비교가 흔들린다
		}
		retiredArgs, retired := retireSupersededMCPServers(servers, name)
		// 은퇴시키는 항목의 프로필은 우리 이름으로 이월한다 — 우리 이름에 기존 항목이 없으면
		// 그 항목이 사용자가 켜 둔 프로필의 유일한 근거이고, 지우면서 이월하지 않으면 재설치가
		// 도구를 조용히 줄인 뒤 "병합 완료"만 보고한다. 우선순위는 위 args 유지 규칙과 같다:
		// 명시 플래그(setProfile) > 우리 이름의 기존 항목(prevExists) > 은퇴 항목.
		// 조건은 **args 길이가 아니라 항목의 존재**다(D81) — 은퇴 항목의 args가 비어 있으면
		// "빈 프로필"이 사용자의 상태이고, 길이로 재면 그 상태가 기본 프로필로 넓어져
		// "기존 항목도 은퇴 항목도 없는 첫 설치에서만 기본 프로필"이라는 규칙이 깨진다.
		if !prevExists && !setProfile && retired {
			entry.Args = retiredArgs
			if entry.Args == nil {
				entry.Args = []string{} // "args": [] 고정 — 위 nil 정규화와 같은 이유
			}
		}
		b, err := json.Marshal(entry)
		if err != nil {
			return nil, false, errors.New("mcp: 항목 직렬화 실패")
		}
		if b, err = keepUnownedEntryKeys(prevRaw, b); err != nil {
			return nil, false, err
		}
		servers[name] = b
	} else {
		delete(servers, name)
		// 제거도 install의 대칭이다 — 은퇴 이름을 install만 정리하면 uninstall 뒤에 우리 명령을 가리키는
		// 옛 항목이 남아 호스트가 그 이름으로 우리를 계속 띄운다. 우리 이름이 애초에 없었어도 은퇴 항목을
		// 지웠으면 changed는 참이어야 한다 — 거짓이면 호출자가 쓰기와 문면을 함께 건너뛰어 그 항목이
		// 영구 잔존한다. 반대로 아무것도 지우지 않은 경우의 거짓은 그대로다(남의 파일 재기록 금지).
		if _, retired := retireSupersededMCPServers(servers, name); retired {
			changed = true
		}
	}
	if len(servers) == 0 {
		// 빈 컨테이너는 키째로 지운다 — mergeHookSettings(:167·:177)와 같은 무조건 형태다(T2 리뷰 F3).
		// {"mcpServers":{}}와 키 부재는 "등록된 서버 없음"으로 의미가 같으므로 보존할 사용자 의도가
		// 없고, 조건을 달면 제거 후 재실행이 빈 컨테이너를 다시 써 멱등이 깨진다(실측).
		// install에서는 우리 항목이 항상 들어가 len>=1이라 이 분기에 오지 않는다.
		delete(doc, "mcpServers")
	} else {
		sb, err := json.Marshal(servers)
		if err != nil {
			return nil, false, errors.New("mcp: mcpServers 직렬화 실패")
		}
		doc["mcpServers"] = sb
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, false, errors.New("mcp: 설정 직렬화 실패")
	}
	return append(out, '\n'), changed, nil
}

// retireSupersededMCPServers — 대체된 과거 등록 이름(D63 ② 단일 서버)을 servers에서 지운다. 우리
// 명령을 가리키는 항목만 지운다 — 같은 이름의 남의 서버는 보호한다(우리 이름 자리의 소유 관문과 같은
// 기준이고, 마커 없는 옛 항목도 우리 것이라 명령 일치 하나로 판정한다). install과 uninstall이 같은
// 함수를 쓴다: 정리가 install에만 있으면 제거는 install의 대칭이 아니게 되고, uninstall 뒤에도 우리
// 바이너리를 가리키는 옛 항목이 남아 호스트가 그 이름으로 우리를 계속 띄운다.
// 반환값은 지운 항목이 들고 있던 args(프로필 이월 후보 — install만 쓴다)와 실제로 지웠는지 여부다.
func retireSupersededMCPServers(servers map[string]json.RawMessage, name string) (args []string, removed bool) {
	for _, old := range supersededMCPServerNames {
		if old == name {
			continue
		}
		raw, ok := servers[old]
		if !ok {
			continue
		}
		var retired mcpServerEntry
		if err := json.Unmarshal(raw, &retired); err != nil || retired.Command != hookBinaryName {
			continue
		}
		if len(retired.Args) > 0 {
			args = retired.Args
		}
		delete(servers, old)
		removed = true
	}
	return args, removed
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
	// 확인하지 못한 스코프는 "정의 없음"이 아니다 — 조용히 건너뛰면 설치가 상위 스코프가 통째로
	// override 하는 자리에 승인 키를 쓰고 "기록했습니다"까지 찍는다. 미존재만 확인된 상태로 보고,
	// 그 밖의 읽기·파싱 실패는 판정 불가로 올려 호출자가 쓰기를 건너뛰게 한다(askShadowedAllows가
	// 거짓 clean을 막으려 이미 쓰는 규칙). 오류 문면에는 경로·원문을 담지 않는다(§12).
	for _, p := range []string{localPath, projectPath, userPath} {
		b, err := readFile(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return "", nil, errors.New("mcp: settings 읽기 실패")
		}
		var doc struct {
			Enabled []string `json:"enabledMcpjsonServers"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			return "", nil, errors.New("mcp: settings 파싱 실패")
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

// enabledServersScopeLabel — enabledServersScope가 돌려준 경로를 스코프 라벨로 바꾼다. 진단 라인은
// 파일명이 아니라 이 라벨을 낸다 — project와 user는 파일명이 둘 다 settings.json이어서 파일명만으로는
// 사용자가 어느 파일을 손댈지 가릴 수 없고, 절대경로는 실을 수 없다(§12 canary, 리뷰 T6-7).
func enabledServersScopeLabel(projectRoot, path string) string {
	projectPath, _ := hookSettingsPath(false, projectRoot) // user=false에는 오류 경로가 없다
	switch path {
	case filepath.Join(projectRoot, ".claude", "settings.local.json"):
		return "local"
	case projectPath:
		return "project"
	}
	return "user" // enabledServersScope가 조사하는 세 경로 중 남는 하나
}

// lowerScopeDefinesEnabled — 설치가 쓰는 project 스코프보다 **낮은** 스코프가 승인 키를 정의하는가.
// uninstall이 우리 이름을 뺀 배열이 비었을 때 키를 지워도 되는지를 가르는 판정이다 — 이 키는 스코프
// 간 병합되지 않고 최상위 정의가 통째로 override 하므로, 키를 지우면 그 아래 정의가 살아난다.
// project 아래는 user뿐이다(local은 project보다 높아 project 키를 지워도 효력이 바뀌지 않는다).
// 판정하지 못하면 "정의됨"을 유지하는 쪽(true)으로 기운다 — 뒤집는 쪽만 사용자가 이 프로젝트에 넣지
// 않은 이름을 승인하는 결과를 만든다. 스코프 순회는 enabledServersScope 하나로 통일한다(같은 질문에
// 두 번째 순회를 두지 않는다 — D13).
func lowerScopeDefinesEnabled(projectRoot string, readFile func(string) ([]byte, error)) bool {
	_, defined, err := enabledServersScope(projectRoot, readFile)
	if err != nil {
		return true
	}
	return slices.ContainsFunc(defined, func(p string) bool {
		return enabledServersScopeLabel(projectRoot, p) == "user"
	})
}

// mergeEnabledServers — settings 문서(existing, 빈 슬라이스 허용)의 enabledMcpjsonServers에
// add면 name을 더하고 아니면 name을 뺀 JSON을 반환한다. 다른 키·다른 원소는 원문 그대로
// 보존한다(json.RawMessage). 이미 있으면(제거 시엔 이미 없으면) 배열을 그대로 둔다 — 재실행이
// 파일 바이트를 흔들지 않게 하는 멱등 조건이다.
// 직렬화 형식(2-space MarshalIndent + 개행)은 mergeHookSettings와 같아야 한다 — 같은 파일을
// 한 번의 설치에서 차례로 쓰므로 형식이 갈리면 재설치마다 바이트가 진동한다.
// keepEmpty는 제거 경로에서만 뜻이 있다(add 경로는 목록이 비지 않는다): 우리가 비운 배열을 키째로
// 지우지 말고 빈 배열로 남기라는 지시다 — 판정은 호출자가 lowerScopeDefinesEnabled로 한다.
func mergeEnabledServers(existing []byte, name string, add, keepEmpty bool) ([]byte, error) {
	doc := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(existing)) > 0 { // 공백뿐인 파일은 빈 병합 기반(mergeMCPServers 형제)
		if err := json.Unmarshal(existing, &doc); err != nil {
			return nil, errors.New("mcp: settings 파싱 실패") // 경로·원문 미포함
		}
		if doc == nil { // JSON `null` → Unmarshal이 맵을 nil로 설정(할당 시 패닉 — mergeMCPServers:53 형제)
			doc = map[string]json.RawMessage{}
		}
	}
	var list []string
	if raw, ok := doc["enabledMcpjsonServers"]; ok {
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, errors.New("mcp: enabledMcpjsonServers 파싱 실패")
		}
	}
	before := len(list)
	if add {
		if !slices.Contains(list, name) {
			list = append(list, name)
		}
	} else {
		list = slices.DeleteFunc(list, func(s string) bool { return s == name })
	}
	switch {
	case len(list) == 0 && before > 0 && keepEmpty:
		// 같은 "정의됨→미정의" 위험이 우리가 비운 배열에도 있다 — 하위 스코프가 이 키를 정의하면
		// 키를 지우는 순간 그 목록이 살아나, 사용자가 이 프로젝트에 넣지 않은 이름이 승인된다.
		// 그 경우에는 빈 배열을 남겨 스코프의 정의 상태를 그대로 둔다. 재실행은 before==0 경로로
		// 들어가 이 []를 손대지 않으므로 바이트 멱등도 유지된다.
		doc["enabledMcpjsonServers"] = json.RawMessage("[]")
	case len(list) == 0 && before > 0:
		// 우리가 비운 배열만 키째로 지운다 — mergeHookSettings(:167·:177)의 빈 컨테이너 제거 규칙과
		// 같은 규칙이다(제거 뒤 흔적을 남기지 않는다). 원래 비어 있던 []는 손대지 않는다: 그것은
		// 하위 스코프를 덮으려는 사용자 의도이고, 키를 지우면 "정의됨"이 "미정의"로 뒤집힌다.
		delete(doc, "enabledMcpjsonServers")
	case len(list) > 0:
		b, err := json.Marshal(list)
		if err != nil {
			return nil, errors.New("mcp: enabledMcpjsonServers 직렬화 실패")
		}
		doc["enabledMcpjsonServers"] = b
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, errors.New("mcp: settings 직렬화 실패")
	}
	return append(out, '\n'), nil
}

// permissionRules — settings 파일에서 읽는 규칙 배열.
type permissionRules struct {
	Permissions struct {
		Ask   []string `json:"ask"`
		Allow []string `json:"allow"`
	} `json:"permissions"`
}

// ruleMatches — ask 규칙 r과 allow 규칙 a가 가리키는 도구 집합이 겹치는가. 겹치면 그 교집합의
// 도구에서 allow는 효력이 없다. 세 형태를 다룬다: 리터럴 완전 일치, **서버 단위 규칙("mcp__server" —
// 그 서버의 전 도구를 덮는 문서화된 형태)**, 도구 위치 접미 glob("mcp__server__prefix_*"). 서버
// 세그먼트에는 glob이 오지 않는다.
// 형태 확장은 두 인자에 대칭으로 적용한다 — 한쪽(ask)에만 넓히면 서버 단위·와일드카드 **allow**가
// 진단에서 거짓 clean으로 나온다. ask가 그 집합 안의 도구를 가리키면 프롬프트는 그대로 강제된다
// (최종 리뷰 F5의 근거를 allow 쪽까지 적용한 것이다).
// 판정은 mcp__ 접두 규칙에 한정한다: 매칭 규칙이 그 형태에만 정의돼 있고, 비-MCP 규칙(Read/Edit
// 형태)은 인자에 절대경로를 담을 수 있어 진단 라인에 그대로 실리면 안 된다(리뷰 F5, §12).
// 두 인자 모두를 걸러 여기 한 곳에서 비교 범위와 출력 범위가 함께 좁혀진다 — 아래 형태 확장은
// 모두 이 관문 뒤에 있다.
func ruleMatches(r, a string) bool {
	if !strings.HasPrefix(r, "mcp__") || !strings.HasPrefix(a, "mcp__") {
		return false
	}
	rp, rLiteral := ruleToolSet(r)
	ap, aLiteral := ruleToolSet(a)
	switch {
	case rLiteral && aLiteral:
		// 리터럴끼리는 완전 일치만이다 — 접두로 비교하면 ctr_index가 ctr_indexer를 덮는다고 오판한다.
		return rp == ap
	case rLiteral:
		return strings.HasPrefix(rp, ap)
	case aLiteral:
		return strings.HasPrefix(ap, rp)
	}
	// 둘 다 집합이면 한쪽 접두가 다른 쪽 접두를 포함할 때만 겹친다(좁은 쪽이 곧 교집합).
	return strings.HasPrefix(rp, ap) || strings.HasPrefix(ap, rp)
}

// ruleToolSet — 규칙 하나가 가리키는 도구 집합을 "접두 + 리터럴 여부"로 환원한다. 서버 단위 규칙은
// 구분자까지 붙여 접두로 만든다 — 구분자 없이 이름 접두만 보면 이름이 그 규칙으로 시작하는 **다른**
// 서버(mcp__ctr-exec2__…)의 도구까지 집합에 든다고 오판한다. 서버 단위 판정은 "mcp__" 뒤에 구분자가
// 더 없는가로 한다(도구 자리가 비어 있다는 뜻).
func ruleToolSet(rule string) (prefix string, literal bool) {
	if strings.HasSuffix(rule, "*") {
		return strings.TrimSuffix(rule, "*"), false
	}
	if !strings.Contains(strings.TrimPrefix(rule, "mcp__"), "__") {
		return rule + "__", false
	}
	return rule, true
}

// askShadowedAllows — 모든 스코프의 permissions를 모아, ask와 도구가 겹치는 allow 항목을 보고한다.
// 규칙 평가가 deny→ask→allow 순이라 그 교집합의 도구에서 allow는 효력이 없다(설계 v0.12 D64).
// 교집합이 allow의 전부가 아닐 수 있으므로(부분 겹침) 보고 문면은 "덮는다"가 아니라 "겹친다"다.
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
