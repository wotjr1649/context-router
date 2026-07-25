package cli

import (
	"encoding/json"
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
