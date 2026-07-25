package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
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

	first, err := mergeMCPServers(existing, ctrMCPServerName, entry, true, true)
	if err != nil {
		t.Fatalf("merge 1: %v", err)
	}
	second, err := mergeMCPServers(first, ctrMCPServerName, entry, true, true)
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

	out, err := mergeMCPServers(existing, ctrMCPServerName, entry, true, false) // setProfile=false
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

	out, err := mergeMCPServers(existing, ctrMCPServerName, entry, true, true)
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
	out2, err := mergeMCPServers(foreign, ctrMCPServerName, entry, true, true)
	if err != nil {
		t.Fatalf("merge foreign: %v", err)
	}
	if _, ok := mcpServersOf(t, out2)["ctr"]; !ok {
		t.Errorf("남의 ctr 항목을 지웠다: %s", out2)
	}
}

// TestMergeMCPServersUninstallKeepsOthers: install=false면 우리 항목만 지운다.
func TestMergeMCPServersUninstallKeepsOthers(t *testing.T) {
	existing := []byte(`{"mcpServers":{"other":{"command":"x"},"` + ctrMCPServerName + `":{"command":"context-router"}}}`)
	out, err := mergeMCPServers(existing, ctrMCPServerName, mcpServerEntry{}, false, true)
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
		out, err := mergeMCPServers(existing, ctrMCPServerName, entry, install, true)
		if err == nil {
			t.Errorf("install=%v: 남의 항목을 충돌 없이 처리했다: %s", install, out)
		}
	}
	// 마커 없이 명령만 우리 것인 항목(마커 도입 전 등록)은 우리 것이라 계속 갱신·제거된다.
	ours := []byte(`{"mcpServers":{"` + ctrMCPServerName + `":{"command":"context-router","args":["--x"]}}}`)
	if _, err := mergeMCPServers(ours, ctrMCPServerName, entry, true, true); err != nil {
		t.Errorf("마커 없는 우리 항목을 남의 것으로 봤다: %v", err)
	}
}

// TestMergeMCPServersEmptyOrNullTolerant: 공백뿐인 파일과 JSON `null`·`{"mcpServers":null}`
// (모두 구문상 유효하거나 사실상 빈 파일)에서도 install이 패닉·오류 없이 병합한다. null은
// Unmarshal이 맵을 nil로 설정해 뒤이은 할당이 패닉하던 경로다 — mergeHookSettings·
// mergeCodexHooks가 같은 함정을 같은 방식으로 이미 막아 두었다(hook_install.go:125·129).
func TestMergeMCPServersEmptyOrNullTolerant(t *testing.T) {
	entry := mcpServerEntry{Command: hookBinaryName, AlwaysLoad: true, Managed: hookMarker("0.12.0")}
	for _, existing := range []string{" \n\t", "null", `{"mcpServers":null}`} {
		out, err := mergeMCPServers([]byte(existing), ctrMCPServerName, entry, true, true)
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
	_, err := mergeMCPServers([]byte(`{"mcpServers":`), ctrMCPServerName, mcpServerEntry{Command: "x"}, true, true)
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
