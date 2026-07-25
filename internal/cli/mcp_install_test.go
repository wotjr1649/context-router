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
}
