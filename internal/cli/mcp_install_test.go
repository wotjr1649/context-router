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

	first, err := mergeMCPServers(existing, ctrMCPServerName, entry, true, true)
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

	second, err := mergeMCPServers(first, ctrMCPServerName, entry, true, true)
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
	if len(ours.Args) != 0 {
		t.Errorf("플래그 없는 설치인데 프로필이 붙었다: %v", ours.Args)
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

// TestHookInstallRejectsExecWithCodex: --codex는 config.toml 경로라 .mcp.json을 만들지 않고,
// 관리 블록의 도구 목록도 고정이라 --enable-exec이 반영될 자리가 없다. 조용히 무시하면 사용자가
// exec가 켜졌다고 오인하므로 조합 자체를 거부한다.
func TestHookInstallRejectsExecWithCodex(t *testing.T) {
	var out bytes.Buffer
	err := runHookInstall([]string{"--codex", "--enable-exec"}, "", "", false, t.TempDir(), "0.12.0", &out)
	if err == nil {
		t.Fatalf("--codex와 --enable-exec 조합을 거부하지 않았다: %s", out.String())
	}
	// 플래그가 아예 없어서 나는 파싱 오류로는 통과하지 못한다 — 조합 판정이 실제로 존재해야 한다.
	if strings.Contains(err.Error(), "플래그 파싱") {
		t.Errorf("조합 판정이 아니라 파싱 오류다: %v", err)
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
// 바뀌어 하위 스코프를 덮으려던 의도가 사라진다.
func TestMergeEnabledServersRemove(t *testing.T) {
	for _, c := range []struct{ name, existing, want string }{
		{"단독 원소는 키째로 제거", `{"enabledMcpjsonServers":["` + ctrMCPServerName + `"]}`, `{}`},
		{"다른 원소는 보존", `{"enabledMcpjsonServers":["other","` + ctrMCPServerName + `"]}`, `{"enabledMcpjsonServers":["other"]}`},
		{"의도적 빈 배열은 보존", `{"enabledMcpjsonServers":[]}`, `{"enabledMcpjsonServers":[]}`},
		{"키가 없으면 무변", `{"other":1}`, `{"other":1}`},
	} {
		got, err := mergeEnabledServers([]byte(c.existing), ctrMCPServerName, false)
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

// mustReadFile — 테스트 공용 읽기(실패 즉시 중단).
func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("읽기 실패: %v", err)
	}
	return b
}
