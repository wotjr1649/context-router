package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// mcpServersOf — 결과 JSON에서 mcpServers 맵을 뽑는 테스트 공용 헬퍼.
func mcpServersOf(t *testing.T, b []byte) map[string]mcpServerEntry {
	t.Helper()
	var got struct {
		Servers map[string]mcpServerEntry `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("결과 파싱: %v (%s)", err, b)
	}
	return got.Servers
}

// TestMergeMCPServersIdempotent: 두 번 병합해도 바이트가 같고, 남의 서버 항목은 보존된다.
func TestMergeMCPServersIdempotent(t *testing.T) {
	existing := []byte(`{"mcpServers":{"other":{"command":"x","args":[]}}}`)
	entry := mcpServerEntry{
		Command: hookBinaryName, Args: []string{"--enable", "exec"},
		AlwaysLoad: true, Managed: hookMarker("0.12.0"),
	}

	first, _, err := mergeMCPServers(existing, ctrMCPServerName, entry, true, true)
	if err != nil {
		t.Fatalf("merge 1: %v", err)
	}
	second, _, err := mergeMCPServers(first, ctrMCPServerName, entry, true, true)
	if err != nil {
		t.Fatalf("merge 2: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("멱등 위반:\n1: %s\n2: %s", first, second)
	}

	servers := mcpServersOf(t, first)
	if _, ok := servers["other"]; !ok {
		t.Errorf("남의 서버 항목이 사라졌다: %s", first)
	}
	ours, ok := servers[ctrMCPServerName]
	if !ok {
		t.Fatalf("우리 항목이 없다: %s", first)
	}
	if ours.Command != hookBinaryName || !ours.AlwaysLoad || ours.Managed != hookMarker("0.12.0") {
		t.Errorf("우리 항목 내용 불일치: %+v", ours)
	}
}

// TestMergeMCPServersHealsStaleMarker: 오래된 버전 마커를 단 우리 항목은 재설치에서 현재
// 마커로 갱신되고(self-heal), setProfile=false면 기존 args는 그대로 남는다 — 플래그 없이
// 실행한 재설치가 이미 켜둔 exec 프로필을 끄지 않는다(설계 v0.12 D64).
func TestMergeMCPServersHealsStaleMarker(t *testing.T) {
	existing := []byte(`{"mcpServers":{"` + ctrMCPServerName + `":{"command":"context-router",` +
		`"args":["--enable","exec"],"alwaysLoad":true,"__ctrManaged":"context-router/0.11.0"}}}`)
	entry := mcpServerEntry{Command: hookBinaryName, AlwaysLoad: true, Managed: hookMarker("0.12.0")}

	out, _, err := mergeMCPServers(existing, ctrMCPServerName, entry, true, false) // setProfile=false
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	ours := mcpServersOf(t, out)[ctrMCPServerName]
	if ours.Managed != hookMarker("0.12.0") {
		t.Errorf("마커가 갱신되지 않았다: %q", ours.Managed)
	}
	if len(ours.Args) != 2 || ours.Args[0] != "--enable" || ours.Args[1] != "exec" {
		t.Errorf("기존 프로필 args가 보존되지 않았다: %v", ours.Args)
	}
}

// TestMergeMCPServersRetiresSuperseded: 단일 서버 표준(D63 ②)에 따라 우리 명령을 가리키는
// 과거 등록(ctr)은 제거하고, 같은 이름이라도 남의 명령이면 두며, 다른 프로필(ctr-global)은
// 대체 대상이 아니라 보존한다.
func TestMergeMCPServersRetiresSuperseded(t *testing.T) {
	existing := []byte(`{"mcpServers":{` +
		`"ctr":{"command":"context-router","args":[]},` +
		`"ctr-global":{"command":"context-router","args":["--profile","global-search"]}}}`)
	entry := mcpServerEntry{Command: hookBinaryName, Args: []string{}, AlwaysLoad: true, Managed: hookMarker("0.12.0")}

	out, _, err := mergeMCPServers(existing, ctrMCPServerName, entry, true, true)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	servers := mcpServersOf(t, out)
	if _, ok := servers["ctr"]; ok {
		t.Errorf("대체된 ctr 항목이 남아 있다: %s", out)
	}
	if _, ok := servers["ctr-global"]; !ok {
		t.Errorf("다른 프로필(ctr-global)이 지워졌다: %s", out)
	}
	if _, ok := servers[ctrMCPServerName]; !ok {
		t.Errorf("우리 항목이 없다: %s", out)
	}

	// 같은 이름이라도 남의 명령이면 건드리지 않는다.
	foreign := []byte(`{"mcpServers":{"ctr":{"command":"someone-else","args":[]}}}`)
	out2, _, err := mergeMCPServers(foreign, ctrMCPServerName, entry, true, true)
	if err != nil {
		t.Fatalf("merge foreign: %v", err)
	}
	if _, ok := mcpServersOf(t, out2)["ctr"]; !ok {
		t.Errorf("남의 ctr 항목을 지웠다: %s", out2)
	}

	// 제거 경로도 같은 기준으로 은퇴시킨다(R2) — 우리 현재 이름이 애초에 없어도 은퇴 항목을 지웠으면
	// changed는 참이어야 한다. 거짓이면 호출자가 쓰기와 문면을 함께 건너뛰어 그 항목이 영구 잔존한다.
	out3, changed, err := mergeMCPServers(existing, ctrMCPServerName, mcpServerEntry{}, false, true)
	if err != nil {
		t.Fatalf("merge remove: %v", err)
	}
	if _, ok := mcpServersOf(t, out3)["ctr"]; ok {
		t.Errorf("제거 경로가 대체된 ctr 항목을 남겼다: %s", out3)
	}
	if !changed {
		t.Errorf("은퇴 항목을 지웠는데 changed가 거짓이다 — 호출자가 쓰기를 건너뛴다: %s", out3)
	}
	// 남의 명령이면 제거 경로에서도 두고, 지운 것이 없으므로 changed도 거짓이다.
	out4, changed4, err := mergeMCPServers(foreign, ctrMCPServerName, mcpServerEntry{}, false, true)
	if err != nil {
		t.Fatalf("merge remove foreign: %v", err)
	}
	if _, ok := mcpServersOf(t, out4)["ctr"]; !ok {
		t.Errorf("제거 경로가 남의 ctr 항목을 지웠다: %s", out4)
	}
	if changed4 {
		t.Errorf("아무것도 지우지 않았는데 changed가 참이다 — 남의 파일을 다시 쓰게 된다: %s", out4)
	}
}

// TestMergeMCPServersCarriesSupersededProfile: 대체된 과거 등록(ctr)이 들고 있던 프로필은 우리
// 이름에 기존 항목이 없을 때 이월한다 — 첫 등록에서는 그 항목이 사용자가 켜 둔 exec의 유일한
// 근거이고, 이월 없이 지우면 재설치가 도구를 조용히 줄인 뒤 "병합 완료"만 보고한다(G9). 명시
// 플래그(setProfile)가 있으면 플래그가 이기고, 우리 이름에 기존 항목이 있으면 그쪽이 근거다 —
// mergeMCPServers:133의 프로필 유지 규칙과 같은 우선순위다.
func TestMergeMCPServersCarriesSupersededProfile(t *testing.T) {
	retired := `"ctr":{"command":"context-router","args":["--enable","exec"]}`
	entry := mcpServerEntry{
		Command: hookBinaryName, Args: mcpArgsForProfiles(nil),
		AlwaysLoad: true, Managed: hookMarker("0.12.0"),
	}

	out, _, err := mergeMCPServers([]byte(`{"mcpServers":{`+retired+`}}`), ctrMCPServerName, entry, true, false)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if ours := mcpServersOf(t, out)[ctrMCPServerName]; !slices.Equal(ours.Args, []string{"--enable", "exec"}) {
		t.Errorf("대체된 등록의 프로필이 이월되지 않았다: %v", ours.Args)
	}
	// 명시 플래그가 있으면 플래그가 이긴다 — 프로필을 끄는 정식 경로가 이월에 막히면 안 된다.
	out2, _, err := mergeMCPServers([]byte(`{"mcpServers":{`+retired+`}}`), ctrMCPServerName, entry, true, true)
	if err != nil {
		t.Fatalf("merge(setProfile): %v", err)
	}
	if ours := mcpServersOf(t, out2)[ctrMCPServerName]; len(ours.Args) != 0 {
		t.Errorf("명시 플래그를 이월이 덮었다: %v", ours.Args)
	}
	// 우리 이름에 기존 항목이 있으면 그쪽이 근거다(이월하지 않는다).
	both := `{"mcpServers":{` + retired + `,"` + ctrMCPServerName + `":{"command":"context-router","args":[]}}}`
	out3, _, err := mergeMCPServers([]byte(both), ctrMCPServerName, entry, true, false)
	if err != nil {
		t.Fatalf("merge(prev): %v", err)
	}
	if ours := mcpServersOf(t, out3)[ctrMCPServerName]; len(ours.Args) != 0 {
		t.Errorf("우리 이름의 기존 항목보다 대체 항목을 우선했다: %v", ours.Args)
	}

	// 은퇴 항목의 args가 **비어 있어도** 그 항목의 존재가 이월 근거다(D81). 길이로 재면 빈
	// 프로필이 기본 프로필로 넓어져 "기존 항목도 은퇴 항목도 없는 첫 설치에서만 기본 프로필"
	// 이라는 우선순위가 깨진다 — 재설치가 사용자가 끈 프로필을 조용히 다시 켜는 경로다.
	// def는 무플래그 설치가 실제로 넘기는 entry다(기본 프로필이 실려 있다).
	def := mcpServerEntry{
		Command: hookBinaryName, Args: mcpArgsForProfiles(defaultMCPProfiles),
		AlwaysLoad: true, Managed: hookMarker("0.15.0"),
	}
	emptyRetired := `{"mcpServers":{"ctr":{"command":"context-router","args":[]}}}`
	out4, _, err := mergeMCPServers([]byte(emptyRetired), ctrMCPServerName, def, true, false)
	if err != nil {
		t.Fatalf("merge(빈 은퇴 args): %v", err)
	}
	if ours := mcpServersOf(t, out4)[ctrMCPServerName]; len(ours.Args) != 0 {
		t.Errorf("빈 프로필의 은퇴 항목을 기본 프로필로 넓혔다: %v", ours.Args)
	}
}

// TestMergeMCPServersKeepsExplicitAlwaysLoad: 항목에 명시된 alwaysLoad는 재설치가 덮지 않는다 —
// 이 키는 서버가 연결될 때까지 세션 시작을 막으므로(호스트 5초 상한) 사용자가 false로 끌 이유가
// 실재한다. alwaysLoad는 우리 소유 키라 keepUnownedEntryKeys가 되돌려 주지도 않는다(G11). 명시된
// false는 값째로 남아야 한다 — 키가 사라지면 다음 재설치가 "명시 없음"으로 읽어 true로 되살려
// 재설치마다 값이 진동한다. 키가 없으면 우리 기본값 true를 쓴다(첫 등록·마커 이전 등록).
func TestMergeMCPServersKeepsExplicitAlwaysLoad(t *testing.T) {
	existing := []byte(`{"mcpServers":{"` + ctrMCPServerName + `":{"command":"context-router",` +
		`"args":[],"alwaysLoad":false,"__ctrManaged":"context-router/0.11.0"}}}`)
	entry := mcpServerEntry{Command: hookBinaryName, Args: []string{}, AlwaysLoad: true, Managed: hookMarker("0.12.0")}

	first, _, err := mergeMCPServers(existing, ctrMCPServerName, entry, true, false)
	if err != nil {
		t.Fatalf("merge 1: %v", err)
	}
	if raw := entryKeyOf(t, first, ctrMCPServerName, "alwaysLoad"); string(raw) != "false" {
		t.Errorf("명시된 alwaysLoad가 보존되지 않았다: %q", raw)
	}
	second, _, err := mergeMCPServers(first, ctrMCPServerName, entry, true, false)
	if err != nil {
		t.Fatalf("merge 2: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("재설치 멱등 위반:\n1: %s\n2: %s", first, second)
	}
	fresh, _, err := mergeMCPServers([]byte(`{}`), ctrMCPServerName, entry, true, false)
	if err != nil {
		t.Fatalf("merge fresh: %v", err)
	}
	if raw := entryKeyOf(t, fresh, ctrMCPServerName, "alwaysLoad"); string(raw) != "true" {
		t.Errorf("기존 항목이 없을 때의 기본값이 true가 아니다: %q", raw)
	}
}

// entryKeyOf — 결과 JSON에서 서버 항목의 키 원문을 뽑는다. 구조체 디코드로는 "키가 없다"와
// "명시된 false"를 가릴 수 없어(둘 다 false로 읽힌다) 원문이 필요하다.
func entryKeyOf(t *testing.T, b []byte, server, key string) json.RawMessage {
	t.Helper()
	var got struct {
		Servers map[string]map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("결과 파싱: %v (%s)", err, b)
	}
	return got.Servers[server][key]
}

// TestMergeMCPServersPreservesUserKeys: 사용자가 우리 항목에 직접 넣은 키("env"·"cwd"·"type")는
// 재설치에서 왕복 보존되고, 우리 소유 4키는 현재 값으로 갱신된다. 4필드 구조체로 재마샬링만 하던
// 이전 형태는 그 키들을 매 hook install마다 조용히 버렸다 — hook install은 이제 모두에게 재실행을
// 권하는 경로라 그 유실이 멱등성 주장 자체를 침식한다(최종 리뷰 F4). 보존 경로에서도 두 번 병합한
// 바이트가 같아야 한다(키 정렬이 결정적이라는 근거).
func TestMergeMCPServersPreservesUserKeys(t *testing.T) {
	existing := []byte(`{"mcpServers":{"` + ctrMCPServerName + `":{"command":"context-router",` +
		`"args":["--enable","exec"],"alwaysLoad":true,"__ctrManaged":"context-router/0.11.0",` +
		`"env":{"CTR_SHADOW_RETENTION":"24h"},"cwd":"w","type":"stdio"}}}`)
	entry := mcpServerEntry{
		Command: hookBinaryName, Args: []string{}, AlwaysLoad: true, Managed: hookMarker("0.12.0"),
	}

	first, _, err := mergeMCPServers(existing, ctrMCPServerName, entry, true, true)
	if err != nil {
		t.Fatalf("merge 1: %v", err)
	}
	var got struct {
		Servers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Managed string            `json:"__ctrManaged"`
			Env     map[string]string `json:"env"`
			Cwd     string            `json:"cwd"`
			Type    string            `json:"type"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(first, &got); err != nil {
		t.Fatalf("결과 파싱: %v (%s)", err, first)
	}
	ours := got.Servers[ctrMCPServerName]
	if ours.Env["CTR_SHADOW_RETENTION"] != "24h" || ours.Cwd != "w" || ours.Type != "stdio" {
		t.Errorf("사용자 키가 유실됐다: env=%v cwd=%q type=%q", ours.Env, ours.Cwd, ours.Type)
	}
	// 우리 소유 키는 새 값이 이긴다 — setProfile=true라 args가 교체되고 마커는 self-heal 한다.
	if ours.Managed != hookMarker("0.12.0") || len(ours.Args) != 0 || ours.Command != hookBinaryName {
		t.Errorf("우리 키가 갱신되지 않았다: %+v", ours)
	}

	second, _, err := mergeMCPServers(first, ctrMCPServerName, entry, true, true)
	if err != nil {
		t.Fatalf("merge 2: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("보존 경로 멱등 위반:\n1: %s\n2: %s", first, second)
	}
}

// TestMergeMCPServersUninstallKeepsOthers: install=false면 우리 항목만 지운다.
func TestMergeMCPServersUninstallKeepsOthers(t *testing.T) {
	existing := []byte(`{"mcpServers":{"other":{"command":"x"},"` + ctrMCPServerName + `":{"command":"context-router"}}}`)
	out, _, err := mergeMCPServers(existing, ctrMCPServerName, mcpServerEntry{}, false, true)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var got struct {
		Servers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("파싱: %v", err)
	}
	if _, ok := got.Servers[ctrMCPServerName]; ok {
		t.Errorf("우리 항목이 남아 있다: %s", out)
	}
	if _, ok := got.Servers["other"]; !ok {
		t.Errorf("남의 항목이 지워졌다: %s", out)
	}
}

// TestMergeMCPServersRejectsForeignSameName: 우리 이름 자리에 마커도 우리 명령도 아닌 항목이
// 있으면 install(교체)·uninstall(삭제) 어느 쪽도 손대지 않고 충돌 오류를 낸다 — 사용자가 직접
// 넣은 남의 항목이 한 번의 설치/제거로 유실되지 않게 하는 관문이다(isOurHookGroup의 보존 철학).
func TestMergeMCPServersRejectsForeignSameName(t *testing.T) {
	existing := []byte(`{"mcpServers":{"` + ctrMCPServerName + `":{"command":"someone-else","args":["--x"]}}}`)
	entry := mcpServerEntry{Command: hookBinaryName, AlwaysLoad: true, Managed: hookMarker("0.12.0")}
	for _, install := range []bool{true, false} {
		out, _, err := mergeMCPServers(existing, ctrMCPServerName, entry, install, true)
		if err == nil {
			t.Errorf("install=%v: 남의 항목을 충돌 없이 처리했다: %s", install, out)
		}
	}
	// 마커 없이 명령만 우리 것인 항목(마커 도입 전 등록)은 우리 것이라 계속 갱신·제거된다.
	ours := []byte(`{"mcpServers":{"` + ctrMCPServerName + `":{"command":"context-router","args":["--x"]}}}`)
	if _, _, err := mergeMCPServers(ours, ctrMCPServerName, entry, true, true); err != nil {
		t.Errorf("마커 없는 우리 항목을 남의 것으로 봤다: %v", err)
	}
	// T3-F5: 무버전 정확 일치 마커("context-router", D82 이후 형태)만으로도 우리 것으로 인정돼
	// 갱신이 거부되지 않는다 — isOurMarkerValue가 옛 strings.HasPrefix(prev.Managed,
	// hookMarkerPrefix())보다 넓힌 지점의 감시선. command는 일부러 남의 것으로 둬 마커 단독으로
	// 소유가 인정되는지를 가른다.
	versionless := []byte(`{"mcpServers":{"` + ctrMCPServerName + `":{"command":"someone-else","args":["--x"],"__ctrManaged":"context-router"}}}`)
	if _, _, err := mergeMCPServers(versionless, ctrMCPServerName, entry, true, true); err != nil {
		t.Errorf("무버전 정확 일치 마커를 남의 것으로 봤다: %v", err)
	}
}

// TestMergeMCPServersEmptyOrNullTolerant: 공백뿐인 파일과 JSON `null`·`{"mcpServers":null}`
// (모두 구문상 유효하거나 사실상 빈 파일)에서도 install이 패닉·오류 없이 병합한다. null은
// Unmarshal이 맵을 nil로 설정해 뒤이은 할당이 패닉하던 경로다 — mergeHookSettings·
// mergeCodexHooks가 같은 함정을 같은 방식으로 이미 막아 두었다.
func TestMergeMCPServersEmptyOrNullTolerant(t *testing.T) {
	entry := mcpServerEntry{Command: hookBinaryName, AlwaysLoad: true, Managed: hookMarker("0.12.0")}
	for _, existing := range []string{" \n\t", "null", `{"mcpServers":null}`} {
		out, _, err := mergeMCPServers([]byte(existing), ctrMCPServerName, entry, true, true)
		if err != nil {
			t.Fatalf("existing=%q merge err: %v", existing, err)
		}
		if _, ok := mcpServersOf(t, out)[ctrMCPServerName]; !ok {
			t.Fatalf("existing=%q 병합 결과에 우리 항목 없음: %s", existing, out)
		}
	}
}

// TestMergeMCPServersRejectsMalformed: 깨진 JSON은 오류이며 메시지에 경로가 없다.
func TestMergeMCPServersRejectsMalformed(t *testing.T) {
	_, _, err := mergeMCPServers([]byte(`{"mcpServers":`), ctrMCPServerName, mcpServerEntry{Command: "x"}, true, true)
	if err == nil {
		t.Fatal("깨진 JSON에 오류가 없다")
	}
}

// scopeKeyForTest — 주입된 readFile이 받은 경로를 스코프 라벨로 바꾼다. 판별 순서가
// 중요하다: local도 projectRoot 하위이므로 파일명 검사가 먼저다.
func scopeKeyForTest(projectRoot, p string) string {
	sp, root := filepath.ToSlash(p), filepath.ToSlash(projectRoot)
	switch {
	case strings.HasSuffix(sp, "/settings.local.json"):
		return "LOCAL"
	case strings.HasPrefix(sp, root+"/"):
		return "PROJECT"
	case strings.HasSuffix(sp, "/.claude/settings.json"):
		return "USER"
	}
	return ""
}

// TestEnabledServersScopePicksHighest: 여러 스코프가 키를 정의하면 우선순위가 가장 높은
// 파일이 winner가 되고, 정의한 모든 경로가 defined에 담긴다. 우선순위는 좁은 스코프가
// 높다(local > project > user). 여기서 LOCAL은 파일은 있으나 키가 없어 정의로 세지 않으므로
// 키를 정의한 최고 스코프는 PROJECT이고, defined에는 PROJECT·USER가 담긴다.
func TestEnabledServersScopePicksHighest(t *testing.T) {
	proj := t.TempDir()
	files := map[string][]byte{
		"USER":    []byte(`{"enabledMcpjsonServers":["ctr-exec"]}`),
		"PROJECT": []byte(`{"enabledMcpjsonServers":["other"]}`),
		"LOCAL":   []byte(`{}`),
	}
	read := func(p string) ([]byte, error) {
		if b, ok := files[scopeKeyForTest(proj, p)]; ok {
			return b, nil
		}
		return nil, os.ErrNotExist
	}
	winner, defined, err := enabledServersScope(proj, read)
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	if scopeKeyForTest(proj, winner) != "PROJECT" {
		t.Errorf("winner=%q, project 스코프여야 한다(local은 키를 정의하지 않았다)", winner)
	}
	if len(defined) != 2 {
		t.Errorf("defined=%v, 2개여야 한다(PROJECT·USER)", defined)
	}
}

// TestEnabledServersScopeNoneDefined: 아무 스코프도 정의하지 않으면 winner가 비고
// defined도 빈다 — 이 경우 설치기가 최고 우선순위 스코프에 직접 쓴다(T6).
func TestEnabledServersScopeNoneDefined(t *testing.T) {
	read := func(string) ([]byte, error) { return nil, os.ErrNotExist }
	winner, defined, err := enabledServersScope(t.TempDir(), read)
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	if winner != "" || len(defined) != 0 {
		t.Errorf("winner=%q defined=%v, 둘 다 비어야 한다", winner, defined)
	}
}

// TestEnabledServersScopeReportsUnreadableScope: 확인하지 못한 스코프가 있으면 판정 불가로 올린다.
// 미존재(os.ErrNotExist)만 "그 스코프에 정의 없음"으로 확인된 상태다 — 읽기·파싱 실패를 정의 없음으로
// 세면 설치가 상위 스코프가 통째로 override 하는 자리에 승인 키를 쓰고 "기록했습니다"까지 찍는다(G6).
// askShadowedAllows가 거짓 clean을 막으려 이미 채택한 규칙과 같다. 오류 문면에는 경로가 없다(§12).
func TestEnabledServersScopeReportsUnreadableScope(t *testing.T) {
	proj := t.TempDir()
	for _, c := range []struct {
		name string
		read func(string) ([]byte, error)
	}{
		{"읽기 실패", func(p string) ([]byte, error) {
			if scopeKeyForTest(proj, p) == "LOCAL" {
				return nil, errors.New("읽기 거부") // 미존재가 아닌 오류
			}
			return nil, os.ErrNotExist
		}},
		{"파싱 실패", func(p string) ([]byte, error) {
			if scopeKeyForTest(proj, p) == "LOCAL" {
				return []byte(`{"enabledMcpjsonServers":`), nil
			}
			return nil, os.ErrNotExist
		}},
	} {
		winner, defined, err := enabledServersScope(proj, c.read)
		if err == nil {
			t.Errorf("%s: 오류가 없다(winner=%q defined=%v) — 확인 못 한 스코프를 정의 없음으로 세면 안 된다",
				c.name, winner, defined)
			continue
		}
		if strings.Contains(err.Error(), proj) {
			t.Errorf("%s: 오류 문면에 경로가 새어나온다", c.name)
		}
	}
}

// TestScopeKeyForTestSeparatesUserAndProject: 스텁 라벨러 자체의 회귀 방지. Windows에서
// t.TempDir()이 %USERPROFILE%\AppData\Local\Temp 하위라, 홈 접두로 가르면 project 경로까지
// USER로 찍혀 T5의 ask/allow 테스트가 빈 목록을 읽는다.
func TestScopeKeyForTestSeparatesUserAndProject(t *testing.T) {
	proj := t.TempDir()
	userPath, err := hookSettingsPath(true, proj)
	if err != nil {
		t.Fatalf("hookSettingsPath(user): %v", err)
	}
	projectPath, err := hookSettingsPath(false, proj)
	if err != nil {
		t.Fatalf("hookSettingsPath(project): %v", err)
	}
	localPath := filepath.Join(proj, ".claude", "settings.local.json")
	for _, c := range []struct{ path, want string }{
		{userPath, "USER"}, {projectPath, "PROJECT"}, {localPath, "LOCAL"},
	} {
		if got := scopeKeyForTest(proj, c.path); got != c.want {
			t.Errorf("scopeKeyForTest(%q)=%q want %q", c.path, got, c.want)
		}
	}
}

// TestAskShadowedAllows: ask와 allow가 같은 도구를 가리키면 그 도구를 보고한다.
// 평가 순서가 deny→ask→allow라 이 조합에서 allow는 효력이 없다.
func TestAskShadowedAllows(t *testing.T) {
	proj := t.TempDir()
	files := map[string]string{
		"PROJECT": `{"permissions":{"ask":["mcp__ctr-exec__ctr_execute","mcp__ctr-exec__ctr_execute_file"]}}`,
		"LOCAL":   `{"permissions":{"allow":["mcp__ctr-exec__ctr_execute"]}}`,
		"USER":    `{}`,
	}
	read := func(p string) ([]byte, error) {
		if s, ok := files[scopeKeyForTest(proj, p)]; ok {
			return []byte(s), nil
		}
		return nil, os.ErrNotExist
	}
	got, err := askShadowedAllows(proj, read)
	if err != nil {
		t.Fatalf("askShadowedAllows: %v", err)
	}
	if len(got) != 1 || got[0] != "mcp__ctr-exec__ctr_execute" {
		t.Errorf("got=%v, [mcp__ctr-exec__ctr_execute] 여야 한다", got)
	}
}

// TestAskShadowedAllowsGlob: ask의 도구 위치 glob이 allow의 리터럴을 덮는 경우도 잡는다.
func TestAskShadowedAllowsGlob(t *testing.T) {
	proj := t.TempDir()
	files := map[string]string{
		"PROJECT": `{"permissions":{"ask":["mcp__ctr-exec__ctr_*"]}}`,
		"LOCAL":   `{"permissions":{"allow":["mcp__ctr-exec__ctr_execute"]}}`,
	}
	read := func(p string) ([]byte, error) {
		if s, ok := files[scopeKeyForTest(proj, p)]; ok {
			return []byte(s), nil
		}
		return nil, os.ErrNotExist
	}
	got, err := askShadowedAllows(proj, read)
	if err != nil {
		t.Fatalf("askShadowedAllows: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got=%v, glob이 리터럴을 덮는 조합을 잡아야 한다", got)
	}
}

// TestAskShadowedAllowsServerWideAsk: 서버 단위 ask 규칙("mcp__ctr-exec" — 그 서버의 전 도구를 덮는
// 문서화된 형태)이 그 서버 도구의 allow를 가리는 조합도 잡는다. 이 형태를 놓치면 doctor [19]가
// "충돌 없음"이라는 거짓 clean을 낸다(최종 리뷰 F5). 이름이 접두로 겹치는 **다른** 서버(ctr-exec2)와
// 무관한 서버(ctr-global)의 allow는 보고 대상이 아니다 — 구분자 "__" 없이 접두만 보면 전자를
// 덮는다고 오판하므로 그 케이스를 함께 넣는다. 그래서 이 테스트는 "1건"으로만 통과한다.
func TestAskShadowedAllowsServerWideAsk(t *testing.T) {
	proj := t.TempDir()
	files := map[string]string{
		"PROJECT": `{"permissions":{"ask":["mcp__ctr-exec"]}}`,
		"LOCAL": `{"permissions":{"allow":["mcp__ctr-exec__ctr_execute",` +
			`"mcp__ctr-exec2__ctr_execute","mcp__ctr-global__ctr_search"]}}`,
	}
	read := func(p string) ([]byte, error) {
		if s, ok := files[scopeKeyForTest(proj, p)]; ok {
			return []byte(s), nil
		}
		return nil, os.ErrNotExist
	}
	got, err := askShadowedAllows(proj, read)
	if err != nil {
		t.Fatalf("askShadowedAllows: %v", err)
	}
	if len(got) != 1 || got[0] != "mcp__ctr-exec__ctr_execute" {
		t.Errorf("got=%v, 서버 단위 ask가 덮는 그 서버 도구 1건만이어야 한다", got)
	}
}

// TestAskShadowedAllowsWidenedAllowForms: 형태 확장(서버 단위·접미 glob)은 두 인자에 대칭으로
// 적용된다 — 판정은 "두 규칙의 도구 집합이 겹치는가"이므로 allow가 넓은 형태여도 그 안의 도구를
// 가리키는 ask는 여전히 프롬프트를 강제한다. 한쪽에만 확장을 적용하면 서버 단위 allow와 와일드카드
// allow가 진단에서 거짓 clean으로 나온다(G5). 겹치지 않는 형태를 같은 픽스처에 넣어 접두 비교가
// 리터럴끼리를 잡아먹지 않게(ctr_index vs ctr_indexer) 함께 고정한다 — 그래서 "2건"으로만 통과한다.
func TestAskShadowedAllowsWidenedAllowForms(t *testing.T) {
	proj := t.TempDir()
	files := map[string]string{
		"PROJECT": `{"permissions":{"ask":["mcp__ctr-exec__ctr_index"]}}`,
		"LOCAL": `{"permissions":{"allow":["mcp__ctr-exec","mcp__ctr-exec__*",` +
			`"mcp__ctr-exec2","mcp__ctr-exec__ctr_indexer","mcp__ctr-global__*"]}}`,
	}
	read := func(p string) ([]byte, error) {
		if s, ok := files[scopeKeyForTest(proj, p)]; ok {
			return []byte(s), nil
		}
		return nil, os.ErrNotExist
	}
	got, err := askShadowedAllows(proj, read)
	if err != nil {
		t.Fatalf("askShadowedAllows: %v", err)
	}
	if !slices.Equal(got, []string{"mcp__ctr-exec", "mcp__ctr-exec__*"}) {
		t.Errorf("got=%v, 서버 단위·접미 glob allow 2건만이어야 한다(다른 서버·다른 도구는 제외)", got)
	}
}

// TestAskShadowedAllowsClean: 겹치지 않으면 빈 목록이다.
func TestAskShadowedAllowsClean(t *testing.T) {
	proj := t.TempDir()
	files := map[string]string{
		"LOCAL": `{"permissions":{"allow":["mcp__ctr-exec__ctr_execute"]}}`,
	}
	read := func(p string) ([]byte, error) {
		if s, ok := files[scopeKeyForTest(proj, p)]; ok {
			return []byte(s), nil
		}
		return nil, os.ErrNotExist
	}
	got, err := askShadowedAllows(proj, read)
	if err != nil {
		t.Fatalf("askShadowedAllows: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got=%v, 비어야 한다", got)
	}
}

// TestAskShadowedAllowsReportsUnreadableScope: 확인하지 못한 스코프가 있으면 오류를 반환한다 —
// 조용히 건너뛰면 doctor가 "충돌 없음"이라는 거짓 clean을 찍는다(리뷰 F1). 미존재(os.ErrNotExist)만
// "그 스코프에 규칙 없음"으로 확인된 상태다. 오류 문면에는 경로가 들어가지 않는다(§12).
func TestAskShadowedAllowsReportsUnreadableScope(t *testing.T) {
	proj := t.TempDir()
	for _, c := range []struct {
		name string
		read func(string) ([]byte, error)
	}{
		{"읽기 실패", func(p string) ([]byte, error) {
			if scopeKeyForTest(proj, p) == "LOCAL" {
				return nil, errors.New("읽기 거부") // 미존재가 아닌 오류
			}
			return nil, os.ErrNotExist
		}},
		{"파싱 실패", func(p string) ([]byte, error) {
			if scopeKeyForTest(proj, p) == "LOCAL" {
				return []byte(`{"permissions":`), nil
			}
			return nil, os.ErrNotExist
		}},
	} {
		got, err := askShadowedAllows(proj, c.read)
		if err == nil {
			t.Errorf("%s: 오류가 없다(got=%v) — 확인 못 한 스코프를 충돌 없음으로 세면 안 된다", c.name, got)
			continue
		}
		if strings.Contains(err.Error(), proj) {
			t.Errorf("%s: 오류 문면에 경로가 새어나온다", c.name)
		}
	}
}

// TestAskShadowedAllowsIgnoresNonMCPRules: 비-MCP 규칙(Read/Edit 형태)은 인자에 절대경로를 담을
// 수 있어 비교·출력 범위 밖이다(리뷰 F5 — 진단 라인은 도구 이름만 낸다). 같은 픽스처의 mcp__
// 규칙은 그대로 잡아야 하므로 이 테스트는 "빈 목록"으로 통과할 수 없다.
func TestAskShadowedAllowsIgnoresNonMCPRules(t *testing.T) {
	proj := t.TempDir()
	files := map[string]string{
		"PROJECT": `{"permissions":{"ask":["Read(/abs/path/x.txt)","mcp__ctr-exec__ctr_execute"]}}`,
		"LOCAL":   `{"permissions":{"allow":["Read(/abs/path/x.txt)","mcp__ctr-exec__ctr_execute"]}}`,
	}
	read := func(p string) ([]byte, error) {
		if s, ok := files[scopeKeyForTest(proj, p)]; ok {
			return []byte(s), nil
		}
		return nil, os.ErrNotExist
	}
	got, err := askShadowedAllows(proj, read)
	if err != nil {
		t.Fatalf("askShadowedAllows: %v", err)
	}
	if len(got) != 1 || got[0] != "mcp__ctr-exec__ctr_execute" {
		t.Errorf("got=%v, mcp__ 규칙 1건만이어야 한다(비-MCP 규칙은 경로를 담을 수 있어 출력 금지)", got)
	}
}

// TestHookInstallWritesMCPConfig: hook install이 .mcp.json에 우리 서버를 멱등하게 쓰고,
// 승인 키를 아무도 정의하지 않았으면 설치 스코프에 직접 쓴다(설계 D64 스코프 규칙).
func TestHookInstallWritesMCPConfig(t *testing.T) {
	proj := t.TempDir()
	var out bytes.Buffer
	if err := runHookInstall(nil, t.TempDir(), "", false, proj, "0.12.0", &out); err != nil {
		t.Fatalf("install: %v", err)
	}
	b1, err := os.ReadFile(mcpConfigPath(proj))
	if err != nil {
		t.Fatalf(".mcp.json 미생성: %v", err)
	}
	ours, ok := mcpServersOf(t, b1)[ctrMCPServerName]
	if !ok {
		t.Fatalf("우리 서버 항목이 없다: %s", b1)
	}
	if ours.Managed != hookMarker("0.12.0") || !ours.AlwaysLoad {
		t.Errorf("마커·상시 로드 불일치: %+v", ours)
	}
	// D81 — 플래그 없는 첫 설치는 기본 프로필 ingest,net을 싣는다(B1이 잡은 미등록의 원인이
	// 기본값이었다). exec는 명시 opt-in이라 여기 들지 않는다(D58·D59·D64).
	// exec 부재는 **원소 일치가 아니라 부분 문자열**로 본다 — args는 항상
	// ["--enable", "<쉼표 목록>"] 형태라 exec가 단독 원소로 나올 수 없고,
	// slices.Contains(ours.Args, "exec")는 어떤 회귀에도 걸리지 않는다.
	if !slices.Equal(ours.Args, []string{"--enable", "ingest,net"}) {
		t.Errorf("기본 프로필이 실리지 않았다: %v", ours.Args)
	}
	if strings.Contains(strings.Join(ours.Args, ","), "exec") {
		t.Errorf("무플래그 설치에 exec가 실렸다: %v", ours.Args)
	}

	// 아무 스코프도 enabledMcpjsonServers를 정의하지 않았으므로 설치 스코프에 직접 써야 한다.
	settingsPath, err := hookSettingsPath(false, proj)
	if err != nil {
		t.Fatalf("hookSettingsPath: %v", err)
	}
	sb, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("settings 미생성: %v", err)
	}
	var settings struct {
		Enabled []string `json:"enabledMcpjsonServers"`
	}
	if err := json.Unmarshal(sb, &settings); err != nil {
		t.Fatalf("settings 파싱: %v", err)
	}
	if !slices.Contains(settings.Enabled, ctrMCPServerName) {
		t.Errorf("승인 키가 기록되지 않았다: %s", sb)
	}

	// 두 번째 설치가 바이트를 바꾸지 않는다 — .mcp.json과 settings 양쪽 모두. settings는 한 번의
	// 설치 안에서 훅 병합과 승인 키 기록이 차례로 쓰는 파일이라, 두 병합의 직렬화 형식이 서로
	// 왕복 안정임을 여기서 함께 고정한다(형식이 갈리면 재설치마다 바이트가 진동한다).
	if err := runHookInstall(nil, t.TempDir(), "", false, proj, "0.12.0", &out); err != nil {
		t.Fatalf("install 2: %v", err)
	}
	b2, _ := os.ReadFile(mcpConfigPath(proj))
	if !bytes.Equal(b1, b2) {
		t.Errorf("멱등 위반:\n1: %s\n2: %s", b1, b2)
	}
	sb2, _ := os.ReadFile(settingsPath)
	if !bytes.Equal(sb, sb2) {
		t.Errorf("settings 멱등 위반:\n1: %s\n2: %s", sb, sb2)
	}
}

// TestHookInstallWritesNoAskRule: 설치 결과 settings에 permissions.ask 항목이 생기지 않는다
// (설계 §2 D64 승인 정책 — 승인 강도는 호스트 권한 모드가 정하고, ask는 무프롬프트 모드에서도
// 프롬프트를 강제하며 더 구체적인 allow도 이긴다). 기존 가드는 doctor 안내 문면만 검사해
// (TestHostSnippetNoExecAskRule) 설치 결과 자체는 무단정이었다 — 설계가 테스트보다 강하게
// 주장하던 지점을 여기서 닫는다(최종 리뷰 F7).
func TestHookInstallWritesNoAskRule(t *testing.T) {
	proj := t.TempDir()
	var out bytes.Buffer
	if err := runHookInstall(nil, t.TempDir(), "", false, proj, "0.12.0", &out); err != nil {
		t.Fatalf("install: %v", err)
	}
	settingsPath, err := hookSettingsPath(false, proj)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("settings 미생성: %v", err)
	}
	// 설치가 이 파일을 실제로 썼는지 먼저 확인한다 — 빈 파일로 조용히 통과하면 아래 단정이 무의미하다.
	if !bytes.Contains(sb, []byte("hooks")) {
		t.Fatalf("설치가 settings에 훅을 쓰지 않았다: %s", sb)
	}
	var doc permissionRules
	if err := json.Unmarshal(sb, &doc); err != nil {
		t.Fatalf("settings 파싱: %v (%s)", err, sb)
	}
	if len(doc.Permissions.Ask) != 0 {
		t.Errorf("설치가 permissions.ask를 만들었다: %v", doc.Permissions.Ask)
	}
}

// TestHookInstallKeepsExecProfileWithoutFlag: --enable-exec으로 켠 프로필이, 플래그 없는
// 재설치에서 살아남는다. 이미 설정된 머신에서 재설치가 exec를 끄면 안 된다.
func TestHookInstallKeepsExecProfileWithoutFlag(t *testing.T) {
	proj := t.TempDir()
	var out bytes.Buffer
	if err := runHookInstall([]string{"--enable-exec"}, t.TempDir(), "", false, proj, "0.12.0", &out); err != nil {
		t.Fatalf("install exec: %v", err)
	}
	if err := runHookInstall(nil, t.TempDir(), "", false, proj, "0.12.0", &out); err != nil {
		t.Fatalf("install again: %v", err)
	}
	b, err := os.ReadFile(mcpConfigPath(proj))
	if err != nil {
		t.Fatalf("읽기: %v", err)
	}
	ours := mcpServersOf(t, b)[ctrMCPServerName]
	if !slices.Equal(ours.Args, []string{"--enable", "exec"}) {
		t.Errorf("exec 프로필이 재설치에서 꺼졌다: %v", ours.Args)
	}
}

// TestHookInstallEnableFlag — D81 프로필 입력 플래그. --enable은 쉼표 구분 목록을 받고,
// --enable-exec과 함께 지정하면 결과는 **합집합**이다(지정 순서는 결과를 바꾸지 않는다).
// 모르는 이름은 조용히 떨어뜨리지 않고 오류로 낸다 — 조용히 무시하면 사용자가 프로필이
// 켜졌다고 오인한다(제거된 상호 배제 검사가 들던 사유와 같다).
func TestHookInstallEnableFlag(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"--enable 단독", []string{"--enable", "ingest"}, []string{"--enable", "ingest"}},
		{"합집합", []string{"--enable", "ingest", "--enable-exec"}, []string{"--enable", "ingest,exec"}},
		{"순서 무관", []string{"--enable-exec", "--enable", "net"}, []string{"--enable", "net,exec"}},
		{"쉼표 목록", []string{"--enable", "net,ingest"}, []string{"--enable", "ingest,net"}},
		// 공백뿐인 값은 진짜 빈 값과 같게 읽혀야 한다 — 그래야 첫 설치에서 기본 프로필이 선다.
		{"공백뿐인 --enable", []string{"--enable", "   "}, []string{"--enable", "ingest,net"}},
	}
	for _, c := range cases {
		proj := t.TempDir()
		var out bytes.Buffer
		if err := runHookInstall(c.args, t.TempDir(), "", false, proj, "0.15.0", &out); err != nil {
			t.Fatalf("%s: install: %v", c.name, err)
		}
		b, _ := os.ReadFile(mcpConfigPath(proj))
		if ours := mcpServersOf(t, b)[ctrMCPServerName]; !slices.Equal(ours.Args, c.want) {
			t.Errorf("%s: args=%v want %v", c.name, ours.Args, c.want)
		}
	}
	// parseEnableProfiles는 TrimSpace로 공백뿐인 값을 비어있음 취급하는데 setProfile 판정이
	// 원시 문자열을 보면 두 입력이 갈린다 — 명시 프로필이 없는데 setProfile=true가 되어
	// 재설치가 이미 켜둔 프로필을 빈 집합으로 덮는다(리뷰 T7-F1). 그 피해를 직접 잰다.
	reProj := t.TempDir()
	var first, second bytes.Buffer
	if err := runHookInstall([]string{"--enable", "exec"}, t.TempDir(), "", false, reProj, "0.15.0", &first); err != nil {
		t.Fatalf("1차 install: %v", err)
	}
	if err := runHookInstall([]string{"--enable", "   "}, t.TempDir(), "", false, reProj, "0.15.0", &second); err != nil {
		t.Fatalf("2차 install: %v", err)
	}
	reBytes, _ := os.ReadFile(mcpConfigPath(reProj))
	if ours := mcpServersOf(t, reBytes)[ctrMCPServerName]; !slices.Equal(ours.Args, []string{"--enable", "exec"}) {
		t.Errorf("공백뿐인 --enable이 기존 프로필을 덮었다: args=%v want [--enable exec]", ours.Args)
	}

	// P4(PR 상세 리뷰) — v0.14 이전 등록물은 --enable이 없어 빈 프로필이고, 무플래그 재설치는
	// 보존 규칙이 이겨 기본 프로필로 넓히지 않는다(설계 D81 축자). 그러면 릴리스 동기였던
	// ctr_index·ctr_fetch_and_index 미등록이 그대로 남으므로 명시 경로를 안내한다.
	oldProj := t.TempDir()
	seed := []byte(`{"mcpServers":{"ctr-exec":{"command":"context-router","args":[],"alwaysLoad":true}}}`)
	if err := os.WriteFile(mcpConfigPath(oldProj), seed, 0o600); err != nil {
		t.Fatal(err)
	}
	var oldOut bytes.Buffer
	if err := runHookInstall(nil, t.TempDir(), "", false, oldProj, "0.15.0", &oldOut); err != nil {
		t.Fatalf("v0.14 형태 재설치: %v", err)
	}
	if !strings.Contains(oldOut.String(), "--enable ingest,net") {
		t.Errorf("빈 프로필 등록물에 명시 경로 안내가 없다:\n%s", oldOut.String())
	}
	// 안내는 내지만 보존 규칙은 그대로다 — 조용히 프로필을 넓히면 쓰기·네트워크 도구가
	// 사용자 동의 없이 노출된다.
	oldBytes, _ := os.ReadFile(mcpConfigPath(oldProj))
	if ours := mcpServersOf(t, oldBytes)[ctrMCPServerName]; len(ours.Args) != 0 {
		t.Errorf("안내 대신 프로필을 넓혔다: args=%v", ours.Args)
	}
	// 프로필이 있는 등록물에는 거짓 경보를 내지 않는다.
	newProj := t.TempDir()
	var mk, again bytes.Buffer
	if err := runHookInstall([]string{"--enable", "ingest"}, t.TempDir(), "", false, newProj, "0.15.0", &mk); err != nil {
		t.Fatalf("1차 install: %v", err)
	}
	if err := runHookInstall(nil, t.TempDir(), "", false, newProj, "0.15.0", &again); err != nil {
		t.Fatalf("무플래그 재설치: %v", err)
	}
	if strings.Contains(again.String(), "--enable ingest,net") {
		t.Errorf("프로필이 있는 등록물에 거짓 안내를 냈다:\n%s", again.String())
	}

	var out bytes.Buffer
	err := runHookInstall([]string{"--enable", "bogus"}, t.TempDir(), "", false, t.TempDir(), "0.15.0", &out)
	if err == nil {
		t.Fatalf("모르는 프로필 이름을 받아들였다: %s", out.String())
	}
	if strings.Contains(err.Error(), "bogus") {
		t.Errorf("오류가 사용자 입력을 에코했다: %v", err)
	}
}

// TestHookInstallReportsExistingApprovalScope: 이미 정의된 스코프가 있으면 쓰지 않고
// 보고만 한다 — 최상위 정의가 통째로 override 하므로 다른 곳에 쓰면 무시된다.
func TestHookInstallReportsExistingApprovalScope(t *testing.T) {
	proj := t.TempDir()
	local := filepath.Join(proj, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte(`{"enabledMcpjsonServers":["other"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHookInstall(nil, t.TempDir(), "", false, proj, "0.12.0", &out); err != nil {
		t.Fatalf("install: %v", err)
	}
	settingsPath, err := hookSettingsPath(false, proj)
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := os.ReadFile(settingsPath)
	if bytes.Contains(sb, []byte("enabledMcpjsonServers")) {
		t.Errorf("이미 정의가 있는데 다른 스코프에 썼다: %s", sb)
	}
	if !strings.Contains(out.String(), "enabledMcpjsonServers") {
		t.Errorf("보고 문면이 없다:\n%s", out.String())
	}
	// 적용되는 스코프를 라벨로 알려야 한다 — project와 user는 파일명이 둘 다 settings.json이라
	// 파일명으로는 사용자가 어느 파일을 손댈지 가릴 수 없다(리뷰 T6-7).
	if !strings.Contains(out.String(), "local 스코프") {
		t.Errorf("적용 스코프 라벨(local)이 없다:\n%s", out.String())
	}
	if strings.Contains(out.String(), "settings.json") {
		t.Errorf("모호한 파일명으로 알린다:\n%s", out.String())
	}
}

// TestHookInstallSkipsApprovalKeyOnIndeterminateScope: 스코프 판정이 실패하면 승인 키를 쓰지 않고
// 사유를 보고한다 — 깨진 settings.local.json은 "정의 없음"이 아니라 "확인 못 함"이고, 그 파일이
// 실제로 이 키를 정의하고 있으면 우선순위가 낮은 project 스코프에 쓴 값은 조용히 무시된다(local >
// project). 그 상태에서 "기록했습니다"를 찍으면 사용자는 승인이 끝났다고 읽는다(G6).
func TestHookInstallSkipsApprovalKeyOnIndeterminateScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows os.UserHomeDir 이음새
	proj := t.TempDir()
	local := filepath.Join(proj, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte(`{"enabledMcpjsonServers":`), 0o600); err != nil { // 깨진 JSON
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHookInstall(nil, t.TempDir(), "", false, proj, "0.12.0", &out); err != nil {
		t.Fatalf("install: %v", err) // 훅·.mcp.json 설치 자체는 성공한다
	}
	settingsPath, err := hookSettingsPath(false, proj)
	if err != nil {
		t.Fatal(err)
	}
	if sb, _ := os.ReadFile(settingsPath); bytes.Contains(sb, []byte("enabledMcpjsonServers")) {
		t.Errorf("판정 불가인데 승인 키를 썼다: %s", sb)
	}
	if !strings.Contains(out.String(), "승인 키 스코프를 판정하지 못해") {
		t.Errorf("건너뛴 사유를 알리지 않았다:\n%s", out.String())
	}
}

// TestHookInstallUserScopeSkipsApprovalKey: --user 설치는 승인 키를 어느 파일에도 쓰지 않고 사유를
// 보고한다. 이 키는 프로젝트 .mcp.json 등록을 이름으로 승인하는 장치라, 사용자 스코프에 쓰면 이
// 머신 모든 프로젝트의 동명 항목까지(소유 검증 없이) 승인하게 된다 — 사용자 스코프 서버는
// ~/.claude.json에 살고 승인 키가 필요 없다(리뷰 T6-1). .mcp.json 등록 자체는 그대로 수행한다.
func TestHookInstallUserScopeSkipsApprovalKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows os.UserHomeDir 이음새
	proj := t.TempDir()
	var out bytes.Buffer
	if err := runHookInstall([]string{"--user"}, t.TempDir(), "", false, proj, "0.12.0", &out); err != nil {
		t.Fatalf("install: %v", err)
	}
	userPath, err := hookSettingsPath(true, proj)
	if err != nil {
		t.Fatal(err)
	}
	projectPath, err := hookSettingsPath(false, proj)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{userPath, projectPath} {
		if b, readErr := os.ReadFile(p); readErr == nil && bytes.Contains(b, []byte("enabledMcpjsonServers")) {
			t.Errorf("--user 설치가 승인 키를 썼다(%s): %s", filepath.Base(p), b)
		}
	}
	if !strings.Contains(out.String(), "--user") {
		t.Errorf("건너뛴 사유를 알리지 않았다:\n%s", out.String())
	}
	// .mcp.json 등록은 --user에서도 그대로 수행한다(프로젝트 스코프 파일이라 대안이 없다).
	if _, err := os.Stat(mcpConfigPath(proj)); err != nil {
		t.Errorf(".mcp.json 등록까지 건너뛰었다: %v", err)
	}
}

// TestHookInstallSkipsApprovalKeyOnMCPConflict: .mcp.json 등록이 멈추면 승인 키도 쓰지 않는다.
// 이 키는 이름으로 항목을 미리 승인하므로, 우리 이름 자리에 남의 항목이 남아 있는 채로 이름만
// 승인 목록에 넣으면 그 남의 항목을 대신 자동 승인해 주는 셈이다(소유 관문의 연장).
func TestHookInstallSkipsApprovalKeyOnMCPConflict(t *testing.T) {
	proj := t.TempDir()
	foreign := []byte(`{"mcpServers":{"` + ctrMCPServerName + `":{"command":"someone-else","args":["--x"]}}}`)
	if err := os.WriteFile(mcpConfigPath(proj), foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHookInstall(nil, t.TempDir(), "", false, proj, "0.12.0", &out); err != nil {
		t.Fatalf("install: %v", err) // 훅 설치 자체는 성공한다(부분 성공을 오류로 승격하지 않는다)
	}
	after, err := os.ReadFile(mcpConfigPath(proj))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, foreign) {
		t.Errorf("남의 .mcp.json 항목을 고쳤다: %s", after)
	}
	settingsPath, err := hookSettingsPath(false, proj)
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := os.ReadFile(settingsPath)
	if bytes.Contains(sb, []byte("enabledMcpjsonServers")) {
		t.Errorf("등록이 멈췄는데 승인 키를 썼다: %s", sb)
	}
	// 정지 사실과 조치 안내가 출력돼야 한다 — 문면이 사라지면 사용자는 부분 성공을 성공으로 읽는다
	// (해상도 5, 리뷰 T6-4).
	for _, want := range []string{"멈췄습니다", "다시 실행"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("정지·조치 문면(%q)이 없다:\n%s", want, out.String())
		}
	}
}

// TestHookInstallAcceptsExecWithCodex — 계약 반전(§2-15). v0.14의
// TestHookInstallRejectsExecWithCodex가 있던 자리다: 그때는 --codex + --enable-exec이 오류를
// 내야 통과했는데, D81이 프로필을 Codex 관리 테이블에도 실으므로 그 상호 배제가 사라졌다.
// 지우기만 하면 그 조합이 무검사 구간이 되므로 "오류 없이 exec 프로필이 관리 테이블의
// args·enabled_tools에 반영된다"를 단정한다. t.Setenv 사용 → t.Parallel 금지.
func TestHookInstallAcceptsExecWithCodex(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	var out bytes.Buffer
	if err := runHookInstall([]string{"--codex", "--user", "--enable-exec"}, "", "", false, t.TempDir(), "0.15.0", &out); err != nil {
		t.Fatalf("--codex + --enable-exec을 거부했다: %v", err)
	}
	cfg, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatalf("config.toml 미생성: %v", err)
	}
	if !strings.Contains(string(cfg), "args = [\"--enable\", \"exec\"]") {
		t.Errorf("exec 프로필이 args에 반영되지 않았다:\n%s", cfg)
	}
	for _, tool := range []string{"ctr_execute", "ctr_execute_file"} {
		if !strings.Contains(string(cfg), "\""+tool+"\"") {
			t.Errorf("%s가 enabled_tools에 없다 — args와 함께 조립되지 않았다:\n%s", tool, cfg)
		}
	}
}

// TestHostSnippetSingleServerRegistration: 안내의 .mcp.json 예시가 단일 서버다. 설치기가
// ctr을 은퇴시키는데 안내가 3개를 계속 제시하면 사용자가 이중 등록을 되살린다(D63 ②).
func TestHostSnippetSingleServerRegistration(t *testing.T) {
	if strings.Contains(hostSnippet, `"ctr": {`) {
		t.Errorf("대체된 ctr 등록 예시가 남아 있다:\n%s", hostSnippet)
	}
	if !strings.Contains(hostSnippet, `"`+ctrMCPServerName+`": {`) {
		t.Errorf("단일 서버 등록 예시가 없다:\n%s", hostSnippet)
	}
	// 전 프로젝트 사용의 공식 경로는 사용자 스코프 등록이다 — 그쪽은 승인 키가 필요 없으므로
	// --user 설치가 승인 키를 건너뛴 사용자가 갈 곳이 안내에 있어야 한다(리뷰 T6-1).
	if !strings.Contains(hostSnippet, "claude mcp add --scope user") {
		t.Errorf("사용자 스코프 등록 안내가 없다:\n%s", hostSnippet)
	}
}

// TestHostSnippetUsesCurrentServerPrefix: permission 예시가 은퇴한 서버 이름을 가리키지 않는다.
// 단일 서버 표준(D63 ②)에서 ingest 2종은 ctr-exec 아래 노출되므로, mcp__ctr__ 접두 규칙을 그대로
// 복사한 사용자는 아무것도 매칭하지 않는 ask 규칙을 갖게 되고 ingest가 무보호로 남는다.
// ctr-global은 설치기가 만들지도 지우지도 않는 별개 프로필이라 그 접두는 유효하다.
func TestHostSnippetUsesCurrentServerPrefix(t *testing.T) {
	if strings.Contains(hostSnippet, "mcp__ctr__") {
		t.Errorf("은퇴한 ctr 서버 접두가 남아 있다:\n%s", hostSnippet)
	}
	for _, want := range []string{
		"mcp__" + ctrMCPServerName + "__ctr_index",
		"mcp__" + ctrMCPServerName + "__ctr_fetch_and_index",
		"mcp__ctr-global__*",
	} {
		if !strings.Contains(hostSnippet, want) {
			t.Errorf("%q 규칙이 안내에서 사라졌다", want)
		}
	}
}

// TestMergeEnabledServersRemove: 제거는 우리 이름만 빼고, 우리가 배열을 비웠으면 키째로 지운다
// (mergeHookSettings:167·177의 빈 컨테이너 제거 규칙과 같은 규칙). 우리 이름이 없으면 배열을
// 손대지 않는다 — 사용자가 의도적으로 비워 둔 []를 지우면 그 스코프가 "정의됨"에서 "미정의"로
// 바뀌어 하위 스코프를 덮으려던 의도가 사라진다. keepEmpty는 같은 뒤집힘이 "우리가 비운 배열"에도
// 성립하는 경우(하위 스코프가 그 키를 정의)를 위한 것이고, 그때는 빈 배열을 남긴다.
func TestMergeEnabledServersRemove(t *testing.T) {
	for _, c := range []struct {
		name, existing, want string
		keepEmpty            bool
	}{
		{"단독 원소는 키째로 제거", `{"enabledMcpjsonServers":["` + ctrMCPServerName + `"]}`, `{}`, false},
		{"다른 원소는 보존", `{"enabledMcpjsonServers":["other","` + ctrMCPServerName + `"]}`, `{"enabledMcpjsonServers":["other"]}`, false},
		{"의도적 빈 배열은 보존", `{"enabledMcpjsonServers":[]}`, `{"enabledMcpjsonServers":[]}`, false},
		{"키가 없으면 무변", `{"other":1}`, `{"other":1}`, false},
		{"하위 스코프가 정의하면 빈 배열을 남긴다", `{"enabledMcpjsonServers":["` + ctrMCPServerName + `"]}`, `{"enabledMcpjsonServers":[]}`, true},
		{"남는 원소가 있으면 keepEmpty와 무관", `{"enabledMcpjsonServers":["other","` + ctrMCPServerName + `"]}`, `{"enabledMcpjsonServers":["other"]}`, true},
	} {
		got, err := mergeEnabledServers([]byte(c.existing), ctrMCPServerName, false, c.keepEmpty)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, got); err != nil {
			t.Errorf("%s: compact: %v", c.name, err)
			continue
		}
		if buf.String() != c.want {
			t.Errorf("%s: got %s want %s", c.name, buf.String(), c.want)
		}
	}
}

// TestHookUninstallRemovesMCPConfigAndApprovalKey: install의 대칭 — uninstall이 .mcp.json 항목과
// 승인 키 원소를 되돌린다. 우리가 비운 컨테이너는 키째로 지우고, 파일 자체는 지우지 않는다.
func TestHookUninstallRemovesMCPConfigAndApprovalKey(t *testing.T) {
	proj := t.TempDir()
	var out bytes.Buffer
	if err := runHookInstall(nil, t.TempDir(), "", false, proj, "0.12.0", &out); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := runHookUninstall(nil, proj, &out); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	b, err := os.ReadFile(mcpConfigPath(proj))
	if err != nil {
		t.Fatalf(".mcp.json 파일이 사라졌다(파일은 지우지 않는다): %v", err)
	}
	if bytes.Contains(b, []byte(ctrMCPServerName)) {
		t.Errorf("우리 항목이 남아 있다: %s", b)
	}
	if bytes.Contains(b, []byte("mcpServers")) {
		t.Errorf("우리가 비운 컨테이너가 남아 있다: %s", b)
	}
	settingsPath, err := hookSettingsPath(false, proj)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sb, []byte("enabledMcpjsonServers")) {
		t.Errorf("승인 키가 남아 있다: %s", sb)
	}
}

// TestHookUninstallPreservesForeignEntriesAndKeys: uninstall은 우리 것만 되돌린다 — 남의 서버
// 항목이 있으면 mcpServers 컨테이너를 유지하고, settings의 다른 최상위 키도 보존한다.
func TestHookUninstallPreservesForeignEntriesAndKeys(t *testing.T) {
	proj := t.TempDir()
	settingsPath, err := hookSettingsPath(false, proj)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, []byte(`{"otherTool":{"keep":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mcpConfigPath(proj), []byte(`{"mcpServers":{"other":{"command":"x","args":[]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHookInstall(nil, t.TempDir(), "", false, proj, "0.12.0", &out); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := runHookUninstall(nil, proj, &out); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	servers := mcpServersOf(t, mustReadFile(t, mcpConfigPath(proj)))
	if _, ok := servers["other"]; !ok {
		t.Errorf("남의 서버 항목이 사라졌다: %s", mustReadFile(t, mcpConfigPath(proj)))
	}
	if _, ok := servers[ctrMCPServerName]; ok {
		t.Errorf("우리 항목이 남아 있다: %s", mustReadFile(t, mcpConfigPath(proj)))
	}
	sb := mustReadFile(t, settingsPath)
	if !bytes.Contains(sb, []byte("otherTool")) {
		t.Errorf("settings의 다른 최상위 키가 사라졌다: %s", sb)
	}
	if bytes.Contains(sb, []byte("enabledMcpjsonServers")) {
		t.Errorf("승인 키가 남아 있다: %s", sb)
	}
}

// TestHookUninstallIdempotentBytes: uninstall을 두 번 돌려도 두 파일의 바이트가 같다.
func TestHookUninstallIdempotentBytes(t *testing.T) {
	proj := t.TempDir()
	settingsPath, err := hookSettingsPath(false, proj)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHookInstall(nil, t.TempDir(), "", false, proj, "0.12.0", &out); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := runHookUninstall(nil, proj, &out); err != nil {
		t.Fatalf("uninstall 1: %v", err)
	}
	mcp1, set1 := mustReadFile(t, mcpConfigPath(proj)), mustReadFile(t, settingsPath)
	if err := runHookUninstall(nil, proj, &out); err != nil {
		t.Fatalf("uninstall 2: %v", err)
	}
	if mcp2 := mustReadFile(t, mcpConfigPath(proj)); !bytes.Equal(mcp1, mcp2) {
		t.Errorf(".mcp.json 멱등 위반:\n1: %s\n2: %s", mcp1, mcp2)
	}
	if set2 := mustReadFile(t, settingsPath); !bytes.Equal(set1, set2) {
		t.Errorf("settings 멱등 위반:\n1: %s\n2: %s", set1, set2)
	}
}

// TestHookUninstallKeepsForeignEntryUnderOurName: 우리 이름 자리에 남의 항목이 있으면 uninstall도
// 손대지 않는다 — T2의 소유 관문은 install·uninstall 양쪽에 대칭으로 적용된다. settings.json이
// 없어도 .mcp.json 정리는 시도한다(부분 설치에서 우리 항목이 영구 잔존하는 것을 막는다 —
// runHookUninstallCodex가 config.toml에 대해 이미 채택한 규칙).
func TestHookUninstallKeepsForeignEntryUnderOurName(t *testing.T) {
	proj := t.TempDir()
	foreign := []byte(`{"mcpServers":{"` + ctrMCPServerName + `":{"command":"someone-else","args":["--x"]}}}`)
	if err := os.WriteFile(mcpConfigPath(proj), foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHookUninstall(nil, proj, &out); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if after := mustReadFile(t, mcpConfigPath(proj)); !bytes.Equal(after, foreign) {
		t.Errorf("남의 항목을 고쳤다: %s", after)
	}
	settingsPath, err := hookSettingsPath(false, proj)
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(settingsPath); statErr == nil {
		t.Error("uninstall이 없던 settings 파일을 만들었다")
	}
}

// TestHookUninstallCleansMCPWhenSettingsUnparsable: 훅 설정이 깨져 정리에 실패해도 .mcp.json
// 정리는 진행하고 오류는 마지막에 반환한다(종료코드 유지). 실패 자리에서 즉시 반환하면 우리 등록이
// 영구 잔존해, 같은 블록이 선언한 "부분 설치에서 잔존 방지" 규칙이 미존재 경우에만 적용된다(T6-2).
func TestHookUninstallCleansMCPWhenSettingsUnparsable(t *testing.T) {
	proj := t.TempDir()
	settingsPath, err := hookSettingsPath(false, proj)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, []byte(`{"hooks":`), 0o600); err != nil { // 깨진 JSON
		t.Fatal(err)
	}
	ours := `{"mcpServers":{"` + ctrMCPServerName + `":{"command":"context-router","args":[],` +
		`"__ctrManaged":"context-router/0.12.0"}}}`
	if err := os.WriteFile(mcpConfigPath(proj), []byte(ours), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if uninstallErr := runHookUninstall(nil, proj, &out); uninstallErr == nil {
		t.Error("훅 설정 정리 실패가 오류로 반환되지 않았다 — 종료코드가 실패를 반영해야 한다")
	}
	if b := mustReadFile(t, mcpConfigPath(proj)); bytes.Contains(b, []byte(ctrMCPServerName)) {
		t.Errorf(".mcp.json 정리가 시도되지 않았다: %s", b)
	}
}

// TestHookUninstallUserScopeKeepsApprovalKey: `uninstall --user`는 승인 키를 건드리지 않는다 —
// `install --user`가 그 키를 쓰지 않으므로(hook_install.go의 --user 분기) 사용자 스코프 목록은
// 사용자가 직접 넣은 것이다. 제거는 설치가 쓸 수 있었던 스코프만 되돌린다(G7). 훅 항목 제거는
// 그대로 수행하므로 이 테스트는 "아무 일도 하지 않음"으로 통과할 수 없다.
func TestHookUninstallUserScopeKeepsApprovalKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows os.UserHomeDir 이음새
	proj := t.TempDir()
	userPath, err := hookSettingsPath(true, proj)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(userPath), 0o700); err != nil {
		t.Fatal(err)
	}
	// 사용자가 직접 넣은 목록 — 우리 이름이 들어 있어도 설치가 쓴 것이 아니다.
	hand := `{"enabledMcpjsonServers":["` + ctrMCPServerName + `","other"]}`
	if err := os.WriteFile(userPath, []byte(hand), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHookUninstall([]string{"--user"}, proj, &out); err != nil {
		t.Fatalf("uninstall --user: %v", err)
	}
	var doc struct {
		Enabled []string `json:"enabledMcpjsonServers"`
	}
	if err := json.Unmarshal(mustReadFile(t, userPath), &doc); err != nil {
		t.Fatalf("settings 파싱: %v", err)
	}
	if !slices.Contains(doc.Enabled, ctrMCPServerName) {
		t.Errorf("사용자가 직접 넣은 승인 키 원소를 지웠다: %v", doc.Enabled)
	}
	if !strings.Contains(out.String(), "--user") {
		t.Errorf("건너뛴 사유를 알리지 않았다:\n%s", out.String())
	}
}

// TestHookUninstallKeepsEmptyApprovalListWhenLowerScopeDefines: 우리 이름이 마지막 원소여도, 더 낮은
// 스코프(user)가 같은 키를 정의하면 키째로 지우지 않고 빈 배열을 남긴다 — 이 키는 스코프 간 병합되지
// 않고 최상위 정의가 통째로 override 하므로, project 정의가 사라지는 순간 user 목록이 살아나 사용자가
// 이 프로젝트에 넣지 않은 이름이 승인된다(G8). 아무도 정의하지 않은 흔한 경로에서는 기존 규칙대로
// 키를 지운다(TestHookUninstallRemovesMCPConfigAndApprovalKey가 그쪽을 고정한다).
func TestHookUninstallKeepsEmptyApprovalListWhenLowerScopeDefines(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	proj := t.TempDir()
	var out bytes.Buffer
	if err := runHookInstall(nil, t.TempDir(), "", false, proj, "0.12.0", &out); err != nil {
		t.Fatalf("install: %v", err)
	}
	// 설치 뒤에 사용자가 user 스코프 목록을 만든 상태 — 설치 시점에는 아무도 정의하지 않았다.
	userPath, err := hookSettingsPath(true, proj)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(userPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userPath, []byte(`{"enabledMcpjsonServers":["other"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runHookUninstall(nil, proj, &out); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	projectPath, err := hookSettingsPath(false, proj)
	if err != nil {
		t.Fatal(err)
	}
	sb := mustReadFile(t, projectPath)
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(sb, &doc); err != nil {
		t.Fatalf("settings 파싱: %v (%s)", err, sb)
	}
	raw, ok := doc["enabledMcpjsonServers"]
	if !ok {
		t.Fatalf("키가 사라져 하위 스코프 목록이 살아난다: %s", sb)
	}
	if string(raw) != "[]" {
		t.Errorf("우리 이름을 뺀 빈 배열이 아니다: %s", raw)
	}
}

// TestHookUninstallLeavesForeignOnlyMCPFileIntact: 우리 항목이 없는 .mcp.json은 바이트 그대로 두고
// "제거 완료"를 찍지 않는다 — 무변경 재기록도 사용자 파일을 바꾼다(json.Marshal은 &를 유니코드
// 이스케이프로 바꾸고 키를 정렬한다). 하지 않은 일을 했다고 말하는 문면은 사용자가 원인을 다른 데서
// 찾게 만든다(G10). 형제 승인 키 문면은 이미 "없었으면 무변"으로 유보한다.
func TestHookUninstallLeavesForeignOnlyMCPFileIntact(t *testing.T) {
	proj := t.TempDir()
	original := []byte("{\n  \"mcpServers\": {\n    \"other\": {\n      \"command\": \"x\",\n" +
		"      \"args\": [\"a&b\"]\n    }\n  }\n}\n")
	if err := os.WriteFile(mcpConfigPath(proj), original, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHookUninstall(nil, proj, &out); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if after := mustReadFile(t, mcpConfigPath(proj)); !bytes.Equal(after, original) {
		t.Errorf("우리 항목이 없는데 파일을 다시 썼다:\n원본: %s\n결과: %s", original, after)
	}
	if strings.Contains(out.String(), "mcp: .mcp.json 항목 제거 완료") {
		t.Errorf("하지 않은 제거를 완료라고 알린다:\n%s", out.String())
	}
}

// TestHookInstallRestoresApprovalNameAfterReinstall: uninstall이 하위 스코프 보호를 위해 남긴 빈
// 승인 배열도 "정의됨"으로 세어지지만, 그 정의를 가진 최상위 스코프가 이번 설치가 쓰는 바로 그
// 파일이면 보고가 아니라 병합이다 — 설계 D64 스코프 규칙의 "사용 중인 최고 우선순위 스코프에 쓴다"가
// 그 경우를 이미 지시한다. 보고로 넘기면 같은 프로젝트의 재설치가 .mcp.json 등록만 되돌려 놓고 승인
// 이름은 빼놓은 상태로 끝나, 사용자는 "설치 완료"를 읽고도 그 서버가 자동 승인되지 않는다(R1).
// 실사용 순서를 그대로 걷는다: 설치 → 사용자가 user 스코프에 다른 이름 추가 → uninstall(빈 배열
// 잔존) → 재설치. 같은 상태는 판정 실패 편향(lowerScopeDefinesEnabled의 오류→true)으로도 만들어지므로
// 하위 스코프 정의가 없는 환경에서도 도달한다.
func TestHookInstallRestoresApprovalNameAfterReinstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows os.UserHomeDir 이음새
	proj := t.TempDir()
	var out bytes.Buffer
	if err := runHookInstall(nil, t.TempDir(), "", false, proj, "0.12.0", &out); err != nil {
		t.Fatalf("install: %v", err)
	}
	userPath, err := hookSettingsPath(true, proj)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(userPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userPath, []byte(`{"enabledMcpjsonServers":["other"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runHookUninstall(nil, proj, &out); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	projectPath, err := hookSettingsPath(false, proj)
	if err != nil {
		t.Fatal(err)
	}
	var afterUninstall map[string]json.RawMessage
	if err := json.Unmarshal(mustReadFile(t, projectPath), &afterUninstall); err != nil {
		t.Fatalf("settings 파싱: %v", err)
	}
	if raw := string(afterUninstall["enabledMcpjsonServers"]); raw != "[]" {
		t.Fatalf("전제 불성립 — uninstall이 빈 배열을 남기지 않았다: %q", raw)
	}
	out.Reset()
	if err := runHookInstall(nil, t.TempDir(), "", false, proj, "0.12.0", &out); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	var reinstalled struct {
		Enabled []string `json:"enabledMcpjsonServers"`
	}
	if err := json.Unmarshal(mustReadFile(t, projectPath), &reinstalled); err != nil {
		t.Fatalf("settings 파싱: %v", err)
	}
	if !slices.Contains(reinstalled.Enabled, ctrMCPServerName) {
		t.Errorf("재설치가 승인 이름을 되돌리지 않았다 — 등록만 되고 자동 승인은 안 된 상태로 남는다: %v\n%s",
			reinstalled.Enabled, out.String())
	}
	// 하위 스코프(user) 목록은 손대지 않는다 — 우리 project 정의가 그 목록을 덮는 것은 이 프로젝트에
	// 한정된 효과이고, 그 파일 자체를 고칠 권한은 설치기에 없다(--user 분기와 같은 규율).
	if ub := mustReadFile(t, userPath); !bytes.Contains(ub, []byte(`"other"`)) {
		t.Errorf("user 스코프 목록을 손댔다: %s", ub)
	}
}

// TestHookUninstallRetiresSupersededEntry: 대체된 과거 등록 이름(D63 ②)도 제거 대상이다 — 정리가
// install 분기 안에만 있으면 uninstall 뒤에 우리 바이너리를 가리키는 옛 항목이 남아 호스트가 그
// 이름으로 우리를 계속 띄우고, "install의 대칭"(D64) 주장이 깨진다(R2). 소유 기준은 install과 같다
// (command가 우리 바이너리). 우리 현재 이름이 없어도 지운 것이 있으면 파일을 쓰고, "그대로 두었습니다"
// 문면은 아무것도 지우지 않았을 때만 나온다.
func TestHookUninstallRetiresSupersededEntry(t *testing.T) {
	proj := t.TempDir()
	old := supersededMCPServerNames[0]
	existing := []byte(`{"mcpServers":{` +
		`"` + old + `":{"command":"` + hookBinaryName + `","args":[]},` +
		`"other":{"command":"x","args":[]}}}`)
	if err := os.WriteFile(mcpConfigPath(proj), existing, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHookUninstall(nil, proj, &out); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	servers := mcpServersOf(t, mustReadFile(t, mcpConfigPath(proj)))
	if _, ok := servers[old]; ok {
		t.Errorf("대체된 과거 등록 %q가 남았다 — 우리 바이너리를 가리키는 항목이 잔존한다: %s",
			old, mustReadFile(t, mcpConfigPath(proj)))
	}
	if _, ok := servers["other"]; !ok {
		t.Errorf("남의 서버 항목이 사라졌다: %s", mustReadFile(t, mcpConfigPath(proj)))
	}
	if strings.Contains(out.String(), "그대로 두었습니다") {
		t.Errorf("지운 것이 있는데 무변경이라고 알린다:\n%s", out.String())
	}
}

// mustReadFile — 테스트 공용 읽기(실패 즉시 중단).
func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("읽기 실패: %v", err)
	}
	return b
}

// TestMCPProfileHelpers — D81 프로필 정적 대응(§1.3-3). 설치기가 쓰는 목록은 런타임 등록
// 결과가 아니라 이 순수 함수들이 소유한다 — enabled_tools를 손으로 나열하면 프로필과
// allowlist가 갈라지는 자리가 새로 생긴다. args와 enabled_tools가 **같은 입력**에서
// 도출되는지도 여기서 고정한다.
func TestMCPProfileHelpers(t *testing.T) {
	if !slices.Equal(defaultMCPProfiles, []string{"ingest", "net"}) {
		t.Fatalf("기본 프로필=%v want [ingest net] (D81 개정)", defaultMCPProfiles)
	}
	base := []string{
		"ctr_search", "ctr_fetch", "ctr_transform",
		"ctr_record_event", "ctr_session_summary", "ctr_export_events",
	}
	cases := []struct {
		name     string
		in       []string
		wantArgs []string
		wantAdd  []string
	}{
		{"빈 집합은 args 키 자체가 없다", nil, nil, nil},
		{"기본", defaultMCPProfiles, []string{"--enable", "ingest,net"}, []string{"ctr_index", "ctr_fetch_and_index"}},
		{"exec만", []string{"exec"}, []string{"--enable", "exec"}, []string{"ctr_execute", "ctr_execute_file"}},
		{
			"최대",
			[]string{"exec", "net", "ingest"},
			[]string{"--enable", "ingest,net,exec"},
			[]string{"ctr_index", "ctr_fetch_and_index", "ctr_execute", "ctr_execute_file"},
		},
		{"모르는 이름은 떨어진다", []string{"ingest", "bogus"}, []string{"--enable", "ingest"}, []string{"ctr_index"}},
		{"중복은 한 번만", []string{"net", "net"}, []string{"--enable", "net"}, []string{"ctr_fetch_and_index"}},
	}
	for _, c := range cases {
		if got := mcpArgsForProfiles(c.in); !slices.Equal(got, c.wantArgs) {
			t.Errorf("%s: args=%v want %v", c.name, got, c.wantArgs)
		}
		want := append(append([]string{}, base...), c.wantAdd...)
		if got := enabledToolsForProfiles(c.in); !slices.Equal(got, want) {
			t.Errorf("%s: enabled_tools=%v want %v", c.name, got, want)
		}
	}
	// slices.Equal은 nil과 []를 같다고 본다 — 위 "빈 집합" 케이스는 그 둘을 가르지 못하므로
	// 여기서 따로 고정한다. 가르는 것이 계약이다: Codex 갈래는 nil일 때 args 키 자체를 쓰지
	// 않는데 []를 쓰면 재직렬화가 그 줄을 지우고(§3 표1) 매 실행이 같은 줄을 되써 D84의
	// 무변경 판정이 성립하지 않는다(.mcp.json 갈래는 mergeMCPServers가 nil을 []로 정규화하므로
	// 어느 쪽이든 안전하다 — 이 계약이 걸리는 곳은 Codex 갈래뿐이다).
	if got := mcpArgsForProfiles(nil); got != nil {
		t.Errorf("빈 프로필의 args가 nil이 아니다: %#v", got)
	}
	// 되읽기: 우리가 쓰는 형태와 아는 이름만 인식한다. 부재·[]는 빈 집합(D80 동치 규칙).
	readback := []struct {
		name string
		in   []string
		want []string
		ok   bool
	}{
		{"부재", nil, nil, true},
		{"빈 배열", []string{}, nil, true},
		{"기본", []string{"--enable", "ingest,net"}, []string{"ingest", "net"}, true},
		{"순서 무관 정규화", []string{"--enable", "exec,ingest"}, []string{"ingest", "exec"}, true},
		{"모르는 이름", []string{"--enable", "ingest,bogus"}, nil, false},
		{"우리가 쓰지 않는 형태", []string{"--profile", "global-search"}, nil, false},
		{"토큰 수 불일치", []string{"--enable"}, nil, false},
	}
	for _, c := range readback {
		got, ok := profilesFromArgs(c.in)
		if ok != c.ok || !slices.Equal(got, c.want) {
			t.Errorf("%s: profilesFromArgs=%v,%v want %v,%v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// mcpOriginal — 세 테스트가 공유하는 "사용자가 손으로 쓴 원본". 우리 항목이 없어야 install이
// 실제로 바이트를 바꾼다.
const mcpOriginal = "{\n  \"mcpServers\": {\n    \"other\": {\"command\": \"x\"}\n  }\n}\n"

// TestMCPBackupSingleSlot — D95. 기입한 실행에만 .bak이 생기고, 2회차 무변경 install이
// 그 슬롯을 소모하지 않는다. 긍정 단정만 두면 매 실행 백업하는 구현이 통과해 단일 슬롯이
// 원본 대신 "직전에 우리가 쓴 것"으로 채워진다.
func TestMCPBackupSingleSlot(t *testing.T) {
	proj := t.TempDir()
	mcpPath := mcpConfigPath(proj)
	if err := os.WriteFile(mcpPath, []byte(mcpOriginal), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	// 실제 시그니처는 7인자다(hook_install.go:547). 기존 호출부와 같은 형태.
	if err := runHookInstall(nil, t.TempDir(), "", false, proj, "0.18.0", &out); err != nil {
		t.Fatalf("1차 install: %v", err)
	}
	bak, err := os.ReadFile(mcpPath + ".bak")
	if err != nil {
		t.Fatalf("1차 install이 백업을 남기지 않았다: %v", err)
	}
	if string(bak) != mcpOriginal {
		t.Errorf("백업이 원본이 아니다:\n%s", bak)
	}
	after1, _ := os.ReadFile(mcpPath)
	out.Reset()
	if err := runHookInstall(nil, t.TempDir(), "", false, proj, "0.18.0", &out); err != nil {
		t.Fatalf("2차 install: %v", err)
	}
	after2, _ := os.ReadFile(mcpPath)
	if !bytes.Equal(after1, after2) {
		t.Fatalf("2차 install이 파일을 바꿨다 — 멱등 전제가 무너졌다")
	}
	bak2, _ := os.ReadFile(mcpPath + ".bak")
	if string(bak2) != mcpOriginal {
		t.Errorf("2차 무변경 install이 백업 슬롯을 소모했다 — 원본을 잃는다:\n%s", bak2)
	}
}

// TestMCPBackupDoctorFix — 기입 자리 둘째(cli.go). --fix도 같은 슬롯을 지나야 한다.
// 여기가 빠지면 "우리 판정이 틀렸을 때의 복구 수단"이 --fix 경로에서만 없다.
func TestMCPBackupDoctorFix(t *testing.T) {
	isolateCodexHome(t) // config.toml 갈래가 호스트를 보지 않게 격리한다
	proj := t.TempDir()
	mcpPath := mcpConfigPath(proj)
	// --fix는 등록을 만들지 않는다 — 우리 소유 표식이 있는 옛 버전 등록물이 있어야 기입한다.
	original := "{\n  \"mcpServers\": {\n    \"" + ctrMCPServerName + "\": {\"command\": \"" +
		hookBinaryName + "\", \"__ctrManaged\": \"" + hookMarker("0.17.0") + "\"}\n  }\n}\n"
	if err := os.WriteFile(mcpPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := doctorOut(t, proj, true); err != nil {
		t.Fatalf("doctor --fix: %v", err)
	}
	bak, err := os.ReadFile(mcpPath + ".bak")
	if err != nil {
		t.Fatalf("--fix가 백업을 남기지 않았다: %v", err)
	}
	if string(bak) != original {
		t.Errorf("백업이 원본이 아니다:\n%s", bak)
	}
	after, _ := os.ReadFile(mcpPath)
	if string(after) == original {
		t.Errorf("--fix가 기입하지 않았다 — 이 픽스처가 기입 갈래를 타지 않는다:\n%s", after)
	}
}

// TestMCPBackupFailureBlocksWrite — 백업이 실패하면 **기입하지 않는다**(config.toml 갈래와 같은
// 순서). .bak 자리에 디렉터리를 놓아 atomicWriteFile을 실패시킨다 — 파일 권한은 Windows에서
// 신뢰할 수 없지만 "같은 이름의 디렉터리"는 어느 OS에서도 쓰기가 실패한다.
func TestMCPBackupFailureBlocksWrite(t *testing.T) {
	proj := t.TempDir()
	mcpPath := mcpConfigPath(proj)
	if err := os.WriteFile(mcpPath, []byte(mcpOriginal), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mcpPath+".bak", 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHookInstall(nil, t.TempDir(), "", false, proj, "0.18.0", &out); err != nil {
		t.Fatalf("install: %v", err) // 백업 실패는 훅 설치를 중단시키지 않는다
	}
	after, _ := os.ReadFile(mcpPath)
	if string(after) != mcpOriginal {
		t.Errorf("백업이 실패했는데 기입했다:\n%s", after)
	}
	if !strings.Contains(out.String(), "백업") {
		t.Errorf("백업 실패 사유를 내지 않았다:\n%s", out.String())
	}
}

// TestMCPUninstallDoesNotBackup — 스펙 §0 D95: uninstall 자리는 백업 대상이 **아니다**
// (제거는 되돌릴 대상이 아니라 되돌림 자체다). 대칭으로 오해해 여기에도 백업을 걸면 install이
// 남긴 원본 슬롯을 제거가 덮어 원본을 잃는다 — 만들지도, 덮지도 않는다는 것을 양쪽으로 잰다.
func TestMCPUninstallDoesNotBackup(t *testing.T) {
	proj := t.TempDir()
	mcpPath := mcpConfigPath(proj)
	if err := os.WriteFile(mcpPath, []byte(mcpOriginal), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHookInstall(nil, t.TempDir(), "", false, proj, "0.18.0", &out); err != nil {
		t.Fatalf("install: %v", err)
	}
	bakBefore, err := os.ReadFile(mcpPath + ".bak")
	if err != nil {
		t.Fatalf("install이 백업을 남기지 않았다: %v", err)
	}
	out.Reset()
	// runHookUninstall(args []string, projectRoot string, stdout io.Writer) error — 3인자다
	// (hook_install.go). args는 그 함수가 직접 파싱하므로 nil이 기본 스코프다.
	if err := runHookUninstall(nil, proj, &out); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	after, _ := os.ReadFile(mcpPath)
	if bytes.Contains(after, []byte(ctrMCPServerName)) {
		t.Fatalf("uninstall이 우리 항목을 지우지 않았다 — 기입 갈래를 타지 않는다:\n%s", after)
	}
	bakAfter, err := os.ReadFile(mcpPath + ".bak")
	if err != nil {
		t.Fatalf("uninstall이 기존 .bak을 지웠다: %v", err)
	}
	if !bytes.Equal(bakAfter, bakBefore) {
		t.Errorf("uninstall이 백업 슬롯을 덮었다 — 원본을 잃는다:\n%s", bakAfter)
	}
}
