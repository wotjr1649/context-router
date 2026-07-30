package cli

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

// ctrTableFixture — v0.15 형식으로 기입된 두 관리 테이블(LF). 골든 비교의 기준 문자열이다.
const ctrTableFixture = "[mcp_servers.ctr]\n" +
	"command = \"context-router\"\n" +
	"args = [\"--enable\", \"ingest,net\"]\n" +
	"enabled_tools = [\"ctr_search\", \"ctr_fetch\", \"ctr_transform\", \"ctr_record_event\", \"ctr_session_summary\", \"ctr_export_events\", \"ctr_index\", \"ctr_fetch_and_index\"]\n" +
	"[mcp_servers.ctr.env]\n" +
	"CTR_MANAGED = \"context-router/0.15.0\"\n"

// baseToolsLine — 프로필이 빈 등록물이 쓰는 enabled_tools 줄(기본 6종). keepArgs=false면
// 프로필이 비어도 이 줄은 **무조건** 나간다 — enabledToolsForProfiles(nil)이 기본 6종을
// 돌려주기 때문이다. 빈 프로필 골든에서 이 줄을 빠뜨리면 골든이 실제 산출과 어긋난다.
const baseToolsLine = "enabled_tools = [\"ctr_search\", \"ctr_fetch\", \"ctr_transform\", " +
	"\"ctr_record_event\", \"ctr_session_summary\", \"ctr_export_events\"]\n"

// installFixture — 기본 요청(명시 플래그 없음, 0.15.0 표식).
func installFixture(existing string) codexInstallResult {
	return installCodexConfigBlock([]byte(existing), codexInstallRequest{Marker: "context-router/0.15.0"})
}

// TestInstallCodexConfigBlock — D80 기입 계약. v0.14의 마커 리터럴 표를 테이블 경계로 이관한다.
//   - append/CRLF/무개행 EOF/멱등: 그대로 지킨다(형태만 새 형식).
//   - 충돌 케이스(quoted key·인라인 대입·점표기·오탐 회피): scanOutsideSpans가 그대로 승계한다.
//   - 마커 배치 이상 케이스: 마커가 관리 단위가 아니므로 이상이 아니다 — 관리 테이블 **중복
//     정의**가 그 자리를 잇는다(TOML 파스 에러 방향을 막는 것이 원래 목적이었다).
func TestInstallCodexConfigBlock(t *testing.T) {
	cases := []struct {
		name      string
		existing  string
		wantState codexMCPState
		wantOut   string // "" = existing 무변경 기대
	}{
		{"빈 파일 append", "", mcpWritten, ctrTableFixture},
		{"기존 내용 뒤 append", "a = 1\n", mcpWritten, "a = 1\n\n" + ctrTableFixture},
		{"무개행 EOF — 개행 1개 추가 후 append", "a = 1", mcpWritten, "a = 1\n\n" + ctrTableFixture},
		{"멱등 f(f(x))==f(x)", "a = 1\n\n" + ctrTableFixture, mcpWritten, "a = 1\n\n" + ctrTableFixture},
		{
			"인수 — 표식 없는 우리 테이블", "[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = []\n", mcpWritten,
			"[mcp_servers.ctr]\ncommand = \"context-router\"\n" + baseToolsLine +
				"[mcp_servers.ctr.env]\nCTR_MANAGED = \"context-router/0.15.0\"\n",
		},
		{
			"인라인 env 표기 — 서브테이블 헤더를 붙이지 않는다(D80 기입 규칙)",
			"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { CTR_MANAGED = \"context-router/0.14.0\" }\n", mcpWritten,
			"[mcp_servers.ctr]\ncommand = \"context-router\"\n" + baseToolsLine +
				"env = { CTR_MANAGED = \"context-router/0.15.0\" }\n",
		},
		{
			"부모 테이블 없이 env만 — 새 env 헤더를 붙이지 않는다(중복 정의 금지)",
			"[mcp_servers.ctr.env]\nCTR_MANAGED = \"context-router/0.14.0\"\n", mcpWritten,
			"[mcp_servers.ctr.env]\nCTR_MANAGED = \"context-router/0.15.0\"\n\n" +
				"[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = [\"--enable\", \"ingest,net\"]\n" +
				"enabled_tools = [\"ctr_search\", \"ctr_fetch\", \"ctr_transform\", \"ctr_record_event\", \"ctr_session_summary\", \"ctr_export_events\", \"ctr_index\", \"ctr_fetch_and_index\"]\n",
		},
		{"사용자 소유 — 표식도 command도 남의 것", "[mcp_servers.ctr]\ncommand = \"old\"\n", mcpExistingHeader, ""},
		{"충돌: quoted key", "[mcp_servers.\"ctr\"]\ncommand = \"x\"\n", mcpConflict, ""},
		{"충돌: 인라인 테이블", "mcp_servers.ctr = { command = \"x\" }\n", mcpConflict, ""},
		{"충돌: 부모 테이블+점표기", "[mcp_servers]\nctr.command = \"x\"\n", mcpConflict, ""},
		{"충돌: 루트 완전-점표기(.ctr. 신호)", "mcp_servers.ctr.command = \"x\"\n", mcpConflict, ""},
		{"충돌: 루트 인라인 mcp_servers 전체(ctr 신호 무)", "mcp_servers = { foo = { command = \"x\" } }\n", mcpConflict, ""},
		{"충돌: 루트 인라인 quoted 키", "\"mcp_servers\" = { foo = { command = \"x\" } }\n", mcpConflict, ""},
		{"비충돌: 헤더 정의 + 타 서버 인라인 값", "[mcp_servers]\nfoo = { command = \"x\" }\n", mcpWritten, "[mcp_servers]\nfoo = { command = \"x\" }\n\n" + ctrTableFixture},
		{"오탐 회피: electron+타 서버", "[mcp_servers.chrome]\ncommand = \"x\"\n# electron debugging notes\n", mcpWritten, "[mcp_servers.chrome]\ncommand = \"x\"\n# electron debugging notes\n\n" + ctrTableFixture},
		{"오탐 회피: spectra 언급", "[mcp_servers.foo]\n# spectra analysis\n", mcpWritten, "[mcp_servers.foo]\n# spectra analysis\n\n" + ctrTableFixture},
		{
			"관리 대상 밖: ctr-exec은 무변경 통과", "[mcp_servers.ctr-exec]\ncommand = \"context-router\"\nargs = [\"--enable\", \"exec\"]\nenabled_tools = [\"ctr_execute\", \"ctr_execute_file\"]\ndefault_tools_approval_mode = \"prompt\"\n", mcpWritten,
			"[mcp_servers.ctr-exec]\ncommand = \"context-router\"\nargs = [\"--enable\", \"exec\"]\nenabled_tools = [\"ctr_execute\", \"ctr_execute_file\"]\ndefault_tools_approval_mode = \"prompt\"\n\n" + ctrTableFixture,
		},
		{"이상: 관리 테이블 중복 정의", "[mcp_servers.ctr]\ncommand = \"context-router\"\n[x]\n[mcp_servers.ctr]\n", mcpMarkerAnomaly, ""},
		{"이상: env 서브테이블 중복 정의", "[mcp_servers.ctr]\ncommand = \"context-router\"\n[mcp_servers.ctr.env]\n[y]\n[mcp_servers.ctr.env]\n", mcpMarkerAnomaly, ""},
		{"CRLF 보존 + CRLF 기입", "a = 1\r\n", mcpWritten, "a = 1\r\n\r\n" + strings.ReplaceAll(ctrTableFixture, "\n", "\r\n")},
	}
	for _, c := range cases {
		res := installFixture(c.existing)
		if res.State != c.wantState {
			t.Fatalf("%s: state=%d want %d", c.name, res.State, c.wantState)
		}
		wantOut := c.wantOut
		if wantOut == "" {
			wantOut = c.existing
		}
		if string(res.Out) != wantOut {
			t.Fatalf("%s:\n got=%q\nwant=%q", c.name, res.Out, wantOut)
		}
	}
}

// TestCodexInstallScopeAndMigration — D80 범위 제한(§2-1) · 밀림 시나리오(§2-4) · 중복 정의
// 회피(§2-6) · D84 마이그레이션(§2-11)을 재직렬화된 형태의 픽스처 하나로 함께 본다.
func TestCodexInstallScopeAndMigration(t *testing.T) {
	// 재직렬화 형태: 주석 없음 · env 표식 있음 · 우리 테이블 뒤에 사용자 테이블 여럿 ·
	// **두 관리 테이블 사이에도 사용자 테이블 하나** · 우리 테이블 안에 사용자 키 하나 ·
	// env 안에 CTR_MANAGED 외 환경변수 하나.
	src := "[mcp_servers.ctr]\n" +
		"command = \"context-router\"\n" +
		"args = [\"--enable\", \"ingest,net\"]\n" +
		"enabled_tools = [\"ctr_search\"]\n" +
		"default_tools_approval_mode = \"prompt\"\n" +
		"[mcp_servers.between]\n" +
		"command = \"between-cmd\"\n" +
		"[mcp_servers.ctr.env]\n" +
		"CTR_MANAGED = \"context-router/0.14.0\"\n" +
		"CTR_STORE_ROOT = \"D:/ctr\"\n" +
		"[hooks.state]\n" +
		"trust = \"abc123\"\n" +
		"[tui]\n" +
		"theme = \"dark\"\n"
	res := installFixture(src)
	if res.State != mcpWritten {
		t.Fatalf("state=%d want mcpWritten", res.State)
	}
	got := string(res.Out)
	for _, want := range []string{
		"[mcp_servers.between]\ncommand = \"between-cmd\"\n", // 두 구간 사이 사용자 테이블
		"[hooks.state]\ntrust = \"abc123\"\n",                // 뒤 테이블
		"[tui]\ntheme = \"dark\"\n",
		"default_tools_approval_mode = \"prompt\"\n", // 우리 테이블 안 사용자 키
		"CTR_STORE_ROOT = \"D:/ctr\"\n",              // env 안 사용자 환경변수
		"CTR_MANAGED = \"context-router/0.15.0\"\n",  // 표식은 현재 값으로 self-heal
		"\"ctr_index\"", // enabled_tools는 프로필에서 다시 도출
	} {
		if !strings.Contains(got, want) {
			t.Errorf("범위 밖/보존 대상이 사라졌다 — %q 없음:\n%s", want, got)
		}
	}
	// 재설치 왕복: 두 사용자 키가 값째로 남고 바이트가 흔들리지 않는다.
	again := installCodexConfigBlock(res.Out, codexInstallRequest{Marker: "context-router/0.15.0"})
	if !bytes.Equal(res.Out, again.Out) {
		t.Errorf("재설치 멱등 위반:\n1: %s\n2: %s", res.Out, again.Out)
	}
	if again.Changed {
		t.Errorf("무변경 재설치인데 Changed=true")
	}
	// 표식을 **갱신할 수 없는** 인라인 형태(값이 문자열이 아니다): 소유는 command로 인수하고
	// 표식 줄은 원문 그대로 두되, 산출 바이트가 같으면 Changed=false로 접는다. 접지 않으면
	// 무변경 재실행마다 config.toml을 다시 쓰고 .bak을 다시 남겨 D84 단일 슬롯이 무의미해진다.
	odd := "[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { CTR_MANAGED = 1 }\n"
	ores := installFixture(odd)
	oagain := installCodexConfigBlock(ores.Out, codexInstallRequest{Marker: "context-router/0.15.0"})
	if !bytes.Equal(ores.Out, oagain.Out) || oagain.Changed {
		t.Errorf("갱신할 수 없는 인라인 표식에서 무변경 재실행이 접히지 않았다(changed=%v):\n1: %s\n2: %s", oagain.Changed, ores.Out, oagain.Out)
	}

	// 밀림 시나리오(§2-4): BEGIN과 END 사이에 사용자 테이블이 들어간 파일.
	pushed := "# BEGIN context-router\n" +
		"[mcp_servers.ctr]\n" +
		"command = \"context-router\"\n" +
		"[hooks.state.'a.json:SessionStart:0:0']\n" +
		"trusted_hash = \"sha256:aaa\"\n" +
		"[hooks.state.'a.json:PostToolUse:0:0']\n" +
		"trusted_hash = \"sha256:bbb\"\n" +
		"# END context-router\n"
	pres := installFixture(pushed)
	for _, want := range []string{"sha256:aaa", "sha256:bbb", "[hooks.state.'a.json:SessionStart:0:0']"} {
		if !strings.Contains(string(pres.Out), want) {
			t.Errorf("밀림 파일의 사용자 테이블이 사라졌다 — %q 없음:\n%s", want, pres.Out)
		}
	}
	// D84 마이그레이션이 추가로 지우는 것은 **마커 두 줄뿐**이다.
	if strings.Contains(string(pres.Out), codexBlockBegin) || strings.Contains(string(pres.Out), codexBlockEnd) {
		t.Errorf("구 마커 두 줄이 남았다:\n%s", pres.Out)
	}
	if !strings.Contains(string(pres.Out), "CTR_MANAGED = \"context-router/0.15.0\"") {
		t.Errorf("마이그레이션이 표식을 기입하지 않았다:\n%s", pres.Out)
	}
	// 변환 결과에 대한 재실행은 바이트 무변경이다.
	pagain := installCodexConfigBlock(pres.Out, codexInstallRequest{Marker: "context-router/0.15.0"})
	if !bytes.Equal(pres.Out, pagain.Out) || pagain.Changed {
		t.Errorf("마이그레이션 결과가 멱등이 아니다(changed=%v):\n1: %s\n2: %s", pagain.Changed, pres.Out, pagain.Out)
	}

	// 밀림의 반대 형태(§2-11): 블록이 **우리 테이블만** 감싼 파일(§3 표4의 현재 사용자 파일).
	// END 줄이 우리 구간 **안**으로 들어오므로 drop 맵으로는 지워지지 않는다 — splice가
	// 편집 구간 안에서는 drop을 보지 않기 때문이다. keep에서 빼는 경로가 그것을 닫는다.
	wrapped := "# BEGIN context-router\n" +
		"[mcp_servers.ctr]\n" +
		"command = \"context-router\"\n" +
		"args = []\n" +
		"enabled_tools = [\"ctr_search\"]\n" +
		"# ingest/net 활성화 시 권장: default_tools_approval_mode = \"prompt\"\n" +
		"# END context-router\n" +
		"[tui]\n" +
		"theme = \"dark\"\n"
	wres := installFixture(wrapped)
	if strings.Contains(string(wres.Out), codexBlockBegin) || strings.Contains(string(wres.Out), codexBlockEnd) {
		t.Errorf("구 마커가 관리 테이블 안에 남았다:\n%s", wres.Out)
	}
	if !strings.Contains(string(wres.Out), "[tui]\ntheme = \"dark\"\n") {
		t.Errorf("감싼 형태에서 뒤 테이블이 사라졌다:\n%s", wres.Out)
	}
	wagain := installCodexConfigBlock(wres.Out, codexInstallRequest{Marker: "context-router/0.15.0"})
	if !bytes.Equal(wres.Out, wagain.Out) || wagain.Changed {
		t.Errorf("감싼 형태의 변환 결과가 멱등이 아니다(changed=%v):\n1: %s\n2: %s", wagain.Changed, wres.Out, wagain.Out)
	}

	// §2-2 install 수준 배치: 여러 줄 값을 **우리 관리 테이블 안**에 둔다 — 경계를 잘못 잡으면
	// 범위가 그 줄에서 잘려 잔여 키가 우리 테이블 밖으로 새어 나온다. 같은 모양을 **사용자
	// 테이블 안**에도 둬 우리 범위가 남의 테이블로 번지지 않는지(경계 오인 방향)를 함께 본다.
	multi := "[mcp_servers.ctr]\n" +
		"command = \"context-router\"\n" +
		"note = \"\"\"\n" +
		"[mcp_servers.fake]\n" +
		"\"\"\"\n" +
		"[mcp_servers.user]\n" +
		"command = \"user-cmd\"\n" +
		"blob = '''\n" +
		"[mcp_servers.also_fake]\n" +
		"'''\n"
	mres := installFixture(multi)
	if mres.State != mcpWritten {
		t.Fatalf("여러 줄 값 픽스처: state=%d want mcpWritten", mres.State)
	}
	for _, want := range []string{
		"note = \"\"\"\n[mcp_servers.fake]\n\"\"\"\n",  // 우리 테이블 안 여러 줄 값이 한 엔트리로 보존
		"[mcp_servers.user]\ncommand = \"user-cmd\"\n", // 남의 테이블은 손대지 않는다
		"blob = '''\n[mcp_servers.also_fake]\n'''\n",   // 남의 테이블 안 여러 줄 값도 그대로
	} {
		if !strings.Contains(string(mres.Out), want) {
			t.Errorf("여러 줄 값에서 경계를 잘못 잡았다 — %q 없음:\n%s", want, mres.Out)
		}
	}

	// 중복 정의 회피(§2-6) ①: 루트 mcp_servers 인라인 대입 + **우리 테이블이 없는** 파일.
	// 검사를 우회하면 산출 바이트에 [mcp_servers.ctr] 헤더가 붙는다 — 인라인 대입과 공존하는
	// 그 헤더가 중복 정의의 전제 조건이다. 아래 dup 픽스처는 이미 헤더를 갖고 있어 이 증거를
	// 낼 수 없으므로(우회해도 개수가 1로 같다) 헤더 추가 감시선은 이쪽이 맡는다.
	assign := "mcp_servers = { foo = { command = \"x\" } }\n[tui]\ntheme = \"dark\"\n"
	ares := installFixture(assign)
	if ares.State != mcpConflict || string(ares.Out) != assign {
		t.Errorf("루트 인라인 대입에서 무변경으로 빠지지 않았다: state=%d\n%s", ares.State, ares.Out)
	}
	if strings.Contains(string(ares.Out), "[mcp_servers.ctr]") {
		t.Errorf("[mcp_servers.ctr] 헤더가 새로 붙었다 — 인라인 대입과 공존하면 중복 정의다:\n%s", ares.Out)
	}

	// 중복 정의 회피(§2-6) ②: 같은 조건에서 D84의 마이그레이션도 중단된다.
	dup := "mcp_servers = { foo = { command = \"x\" } }\n" + pushed
	dres := installFixture(dup)
	if dres.State != mcpConflict || string(dres.Out) != dup {
		t.Errorf("중복 정의 조건에서 무변경으로 빠지지 않았다: state=%d\n%s", dres.State, dres.Out)
	}
	if !strings.Contains(string(dres.Out), codexBlockBegin) {
		t.Errorf("충돌 조건인데 마이그레이션이 마커를 지웠다:\n%s", dres.Out)
	}
}

// TestCodexInstallProfileReadback — D81 Codex 갈래의 되읽기(§2-7). 무플래그 재설치는 기존
// args를 프로필 집합으로 되읽어 args와 enabled_tools를 **함께** 재조립하고, 되읽지 못하는
// args에서는 두 키를 **둘 다** 손대지 않는다.
func TestCodexInstallProfileReadback(t *testing.T) {
	marker := "context-router/0.15.0"
	// 우선순위 ①: 명시 플래그가 이긴다.
	exp := installCodexConfigBlock([]byte(ctrTableFixture),
		codexInstallRequest{Profiles: []string{"ingest", "net", "exec"}, SetProfile: true, Marker: marker})
	if !strings.Contains(string(exp.Out), "args = [\"--enable\", \"ingest,net,exec\"]") ||
		!strings.Contains(string(exp.Out), "\"ctr_execute_file\"") {
		t.Errorf("명시 프로필이 반영되지 않았다:\n%s", exp.Out)
	}
	// 우선순위 ②: 기존 테이블의 args를 되읽는다(기본 프로필로 넓히지 않는다).
	prev := "[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = [\"--enable\", \"exec\"]\nenabled_tools = [\"stale\"]\n"
	got := installFixture(prev)
	if !strings.Contains(string(got.Out), "args = [\"--enable\", \"exec\"]") {
		t.Errorf("기존 프로필이 보존되지 않았다:\n%s", got.Out)
	}
	if strings.Contains(string(got.Out), "\"stale\"") || !strings.Contains(string(got.Out), "\"ctr_execute\"") {
		t.Errorf("enabled_tools가 args와 함께 재조립되지 않았다:\n%s", got.Out)
	}
	// **args 부재/[]는 빈 프로필**로 되읽고 기본 프로필로 넓히지 않는다(§3 표4 현재 사용자 파일).
	empty := installFixture("[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = []\n")
	if strings.Contains(string(empty.Out), "args = ") {
		t.Errorf("빈 프로필을 기본 프로필로 넓혔다:\n%s", empty.Out)
	}
	if strings.Contains(string(empty.Out), "ctr_index") {
		t.Errorf("빈 프로필인데 ingest 도구가 실렸다:\n%s", empty.Out)
	}
	// 되읽지 못하는 args — 두 키를 둘 다 손대지 않고 command·표식만 self-heal.
	odd := "[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = [\"--profile\", \"global-search\"]\nenabled_tools = [\"custom\"]\n"
	kept := installFixture(odd)
	if !kept.ArgsKept {
		t.Errorf("ArgsKept=false — 되읽기 실패를 알리지 않았다")
	}
	if !strings.Contains(string(kept.Out), "args = [\"--profile\", \"global-search\"]") ||
		!strings.Contains(string(kept.Out), "enabled_tools = [\"custom\"]") {
		t.Errorf("해석하지 못한 값을 덮어썼다:\n%s", kept.Out)
	}
	if !strings.Contains(string(kept.Out), "CTR_MANAGED = \""+marker+"\"") {
		t.Errorf("표식 self-heal이 생략됐다:\n%s", kept.Out)
	}
	// 첫 설치(기존 테이블 없음)만 기본 프로필을 쓴다.
	fresh := installFixture("")
	if !strings.Contains(string(fresh.Out), "args = [\"--enable\", \"ingest,net\"]") {
		t.Errorf("첫 설치가 기본 프로필을 쓰지 않았다:\n%s", fresh.Out)
	}
	// exec 부재 — D58·D59·D64가 기록한 opt-in 계약의 직접 감시선(§2-7 exec 부재)
	for _, tool := range []string{"ctr_execute", "ctr_execute_file"} {
		if strings.Contains(string(fresh.Out), tool) {
			t.Errorf("무플래그 설치에 %s가 실렸다:\n%s", tool, fresh.Out)
		}
	}
	// 승인 모드 키는 기입하지 않는다(D81).
	if strings.Contains(string(fresh.Out), "approval_mode") {
		t.Errorf("설치기가 승인 모드 키를 기입했다:\n%s", fresh.Out)
	}
}

// TestUninstallCodexConfigBlock — D80 제거 계약(§2-5). 표식이 있는 [mcp_servers.ctr]와 그
// [mcp_servers.ctr.env] **두 구간만** 제거하고 그 밖의 바이트는 보존한다. 이 계약이 없으면
// 신규 형식으로 설치된 두 테이블이 hook uninstall --codex 뒤에도 남는다.
func TestUninstallCodexConfigBlock(t *testing.T) {
	cases := []struct {
		name        string
		existing    string
		wantChanged bool
		wantOut     string
	}{
		{"왕복: append 산출물 → 원본+EOF개행", "a = 1\n\n" + ctrTableFixture, true, "a = 1\n"},
		{"테이블 부재 무변경", "a = 1\n", false, "a = 1\n"},
		{"중간 구간 — 직전이 비-빈줄이면 빈줄 미삭제", "a = 1\n" + ctrTableFixture + "[b]\nx = 1\n", true, "a = 1\n[b]\nx = 1\n"},
		{"연속 빈 줄 2개 — 정확히 1개만 제거", "a = 1\n\n\n" + ctrTableFixture, true, "a = 1\n\n"},
		{"사용자 소유 — 무변경", "[mcp_servers.ctr]\ncommand = \"old\"\n", false, "[mcp_servers.ctr]\ncommand = \"old\"\n"},
		{
			"관리 범위 밖 ctr-exec은 무변경", "[mcp_servers.ctr-exec]\ncommand = \"context-router\"\nargs = [\"--enable\", \"exec\"]\n\n" + ctrTableFixture, true,
			"[mcp_servers.ctr-exec]\ncommand = \"context-router\"\nargs = [\"--enable\", \"exec\"]\n",
		},
		{
			"밀림 파일 — 블록 안 사용자 테이블 보존(§2-4 uninstall)",
			"# BEGIN context-router\n[mcp_servers.ctr]\ncommand = \"context-router\"\n[hooks.state]\ntrust = \"abc\"\n# END context-router\n", true,
			"[hooks.state]\ntrust = \"abc\"\n",
		},
		{
			"구 버전 표식도 소유로 인정(§2-10)",
			"[mcp_servers.ctr]\ncommand = \"other\"\n[mcp_servers.ctr.env]\nCTR_MANAGED = \"context-router/0.14.0\"\n", true, "",
		},
	}
	for _, c := range cases {
		out, changed := uninstallCodexConfigBlock([]byte(c.existing))
		if changed != c.wantChanged || string(out) != c.wantOut {
			t.Fatalf("%s: changed=%v out=%q (want %v %q)", c.name, changed, out, c.wantChanged, c.wantOut)
		}
	}
}

// TestProbeCodexMCPBlock — doctor [16] 존재 판별(D52 승계). 우선순위는 install과 1:1이다:
// 중복 정의 > 구간 밖 충돌 > 테이블 존재. 마커 배치 자체는 더 이상 이상이 아니다(D80).
func TestProbeCodexMCPBlock(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		present bool
		anomaly bool
	}{
		{"빈 파일 — 부재", "", false, false},
		{"관리 테이블 존재", ctrTableFixture, true, false},
		{"표식 없는 우리 테이블도 존재", "[mcp_servers.ctr]\ncommand = \"context-router\"\n", true, false},
		{"사용자 소유 테이블도 존재로 본다", "[mcp_servers.ctr]\ncommand = \"old\"\n", true, false},
		{"무관 내용 — 부재", "[model]\nname = \"gpt\"\n", false, false},
		{"관리 테이블 중복 정의 — 이상", "[mcp_servers.ctr]\n[x]\n[mcp_servers.ctr]\n", false, true},
		// 충돌 줄은 픽스처 **앞**에 둔다 — 뒤에 두면 [mcp_servers.ctr.env] 구간이 다음 테이블
		// 헤더가 없어 EOF까지 뻗으므로 그 줄이 우리 구간 안이 되고, scanOutsideSpans가 건너뛰어
		// (true,false)가 나온다. 구간 밖 신호를 재려면 구간 밖에 두어야 한다.
		{"구간 밖 충돌 — 이상", "mcp_servers = { ctr = { command = \"other\" } }\n" + ctrTableFixture, false, true},
		{"ctr-exec만 있으면 부재", "[mcp_servers.ctr-exec]\ncommand = \"context-router\"\n", false, false},
	}
	for _, c := range cases {
		p, a := probeCodexMCPBlock([]byte(c.in))
		if p != c.present || a != c.anomaly {
			t.Errorf("%s: present=%v anomaly=%v want %v/%v", c.name, p, a, c.present, c.anomaly)
		}
	}
}

// TestCodexConfigBlockRoundTrip — 왕복 f_uninstall(f_install(x)): EOF 개행 정규화 제외 바이트
// 동일. oracle의 EOF 개행은 파일 지배 개행을 따른다("\n" 하드코딩이면 CRLF 파일에 LF를 붙이는
// 혼합-EOL 구현이 통과한다).
func TestCodexConfigBlockRoundTrip(t *testing.T) {
	for _, orig := range []string{"", "a = 1\n", "a = 1", "a = 1\r\n", "a = 1\r\nb = 2"} {
		res := installFixture(orig)
		if res.State != mcpWritten {
			t.Fatalf("install(%q) state=%d", orig, res.State)
		}
		back, _ := uninstallCodexConfigBlock(res.Out)
		want := orig
		if want != "" && !strings.HasSuffix(want, "\n") {
			eol := "\n"
			if strings.Contains(want, "\r\n") {
				eol = "\r\n"
			}
			want += eol // 명문 한계: EOF 개행 정규화만 왕복 예외(지배 개행 형식으로)
		}
		if string(back) != want {
			t.Fatalf("왕복(%q): got=%q want=%q", orig, back, want)
		}
	}
}

// TestTOMLLineScannerHeaderBoundary — D80 헤더 판정 규칙(§2-2). 여러 줄 문자열(삼중 큰따옴표·삼중 홑따옴표)과
// 여러 줄 배열 안에 있는 [ … ] 모양 줄은 테이블 경계가 아니다. 상태 추적 없이 라인 모양만
// 보면 범위가 그 줄에서 잘려 우리 테이블의 잔여 키가 남거나 사용자 값 안의 한 줄이 경계가
// 된다. [[배열 테이블]]은 경계이지만 이름 있는 헤더는 아니다.
// **배열 안 원소는 대괄호로 시작하는 형태로 둔다** — 따옴표로 시작하는 원소는 stripLine
// 결과가 '"'로 시작해 상태 추적이 있든 없든 경계가 아니므로 depth 추적에 물리는 단정이
// 되지 못한다. 한 줄 문자열·주석 줄(line 11)은 어느 구현에서도 경계가 아니라 감시선이
// 아니지만, '['를 포함하기만 하면 경계로 보는 구현을 배제하는 자리로 남긴다.
// **line 2는 이스케이프 인지 감시선이다(리뷰 F3)**: 여러 줄 기본 문자열 본문에 \""" 를 넣는다 —
// 첫 따옴표만 백슬래시로 이스케이프되고 나머지 둘은 평범한 문자라 실제로는 닫지 않는다.
// strings.Index 기반으로 닫기를 찾으면 이 세 따옴표를 닫기로 오인해 그 뒤로 열림 상태가
// 뒤집히고, 그 오염이 line 12·13의 경계 판정까지 전파된다 — 두 줄의 단정이 이 감시선의 게이트다.
func TestTOMLLineScannerHeaderBoundary(t *testing.T) {
	src := "" +
		"[a]\n" + // 0 경계 a
		"k = \"\"\"\n" + // 1
		"\\\"\"\"\n" + // 2 이스케이프된 \""" — 닫기 오인 감시선(F3), 실제로는 닫지 않는다
		"[mcp_servers.x]\n" + // 3 여러 줄 기본 문자열 안 — 경계 아님
		"\"\"\"\n" + // 4
		"m = '''\n" + // 5
		"[mcp_servers.y]\n" + // 6 여러 줄 리터럴 안 — 경계 아님
		"'''\n" + // 7
		"arr = [\n" + // 8
		"  [1, 2],\n" + // 9 여러 줄 배열 안의 중첩 배열 — 경계 아님(depth 추적의 감시선)
		"]\n" + // 10
		"s = \"[not.a.header]\"  # [also.not]\n" + // 11 한 줄 문자열·주석 — 경계 아님
		"[[b]]\n" + // 12 배열 테이블 — 경계이나 이름 없음(F3 감시선 게이트 1)
		"[c] # trailing\n" // 13 경계 c(F3 감시선 게이트 2)
	lines := splitLinesKeepEnds([]byte(src))
	var sc tomlLineScanner
	type got struct {
		boundary bool
		name     string
	}
	want := map[int]got{
		0: {true, "a"}, 3: {false, ""}, 6: {false, ""}, 9: {false, ""},
		11: {false, ""}, 12: {true, ""}, 13: {true, "c"},
	}
	for i, ln := range lines {
		b, n := sc.step(ln)
		if w, ok := want[i]; ok && (b != w.boundary || n != w.name) {
			t.Errorf("line %d: boundary=%v name=%q want %v/%q", i, b, n, w.boundary, w.name)
		}
	}
}

// TestCodexManagedSpans — D80 관리 범위. 각 테이블의 구간은 헤더 라인부터 **다음 테이블 헤더
// 직전 또는 EOF**까지이고 **두 테이블은 서로 독립 구간**이다 — 사이에 사용자 테이블이 와도
// 그 테이블은 어느 구간에도 들지 않는다. 같은 이름의 헤더가 둘이면 TOML 중복 정의라 dup이다.
func TestCodexManagedSpans(t *testing.T) {
	src := "" +
		"model = \"gpt\"\n" + // 0
		"[mcp_servers.ctr]\n" + // 1  table.start
		"command = \"context-router\"\n" + // 2
		"[mcp_servers.between]\n" + // 3  table.end
		"x = 1\n" + // 4
		"[mcp_servers.ctr.env]\n" + // 5  env.start
		codexMarkerKey + " = \"context-router/0.15.0\"\n" + // 6
		"[after]\n" + // 7  env.end
		"y = 2\n" // 8
	sp := codexManagedSpans(splitLinesKeepEnds([]byte(src)))
	if !sp.table.found || sp.table.start != 1 || sp.table.end != 3 {
		t.Errorf("table=%+v want {1 3 true}", sp.table)
	}
	if !sp.env.found || sp.env.start != 5 || sp.env.end != 7 {
		t.Errorf("env=%+v want {5 7 true}", sp.env)
	}
	if sp.dup {
		t.Errorf("dup=true인데 중복 헤더가 없다")
	}
	// EOF 종료
	sp2 := codexManagedSpans(splitLinesKeepEnds([]byte("[mcp_servers.ctr]\na = 1\nb = 2\n")))
	if sp2.table.end != 3 {
		t.Errorf("EOF 종료 end=%d want 3", sp2.table.end)
	}
	// 중복 정의
	sp3 := codexManagedSpans(splitLinesKeepEnds([]byte("[mcp_servers.ctr]\n[x]\n[mcp_servers.ctr]\n")))
	if !sp3.dup {
		t.Errorf("같은 이름 헤더 둘인데 dup=false")
	}
	// [mcp_servers.ctr-exec]는 관리 대상이 아니다(D80 — 사용자가 만든 별도 등록)
	sp4 := codexManagedSpans(splitLinesKeepEnds([]byte("[mcp_servers.ctr-exec]\ncommand = \"context-router\"\n")))
	if sp4.table.found || sp4.env.found {
		t.Errorf("ctr-exec을 관리 테이블로 잡았다: %+v", sp4)
	}
	// 논리 엔트리 — 여러 줄 값은 한 엔트리다
	lines := splitLinesKeepEnds([]byte("[mcp_servers.ctr]\nenabled_tools = [\n \"a\",\n]\nk = 1\n"))
	got := codexEntries(lines, codexManagedSpans(lines).table)
	if len(got) != 2 || got[0] != [2]int{1, 3} || got[1] != [2]int{4, 4} {
		t.Errorf("entries=%v want [[1 3] [4 4]]", got)
	}
	if k := codexKeyName("enabled_tools=[\"a\"]"); k != "enabled_tools" {
		t.Errorf("codexKeyName=%q want enabled_tools", k)
	}
	if v := tomlStringList("[\"a\",'b']"); !slices.Equal(v, []string{"a", "b"}) {
		t.Errorf("tomlStringList=%v want [a b]", v)
	}
}

// codexLinesOf — 픽스처 문자열을 라인 목록과 구간으로 함께 푸는 테스트 헬퍼.
func codexLinesOf(t *testing.T, src string) ([][]byte, codexSpans, codexTableView) {
	t.Helper()
	lines := splitLinesKeepEnds([]byte(src))
	sp := codexManagedSpans(lines)
	return lines, sp, codexReadTable(lines, sp.table)
}

// TestCodexTableOwnershipAndAdoption — D80 소유 판정과 인수(§2-3). 세 갈래를 한 표로 본다.
// ① 표식 있고 값이 소유 기준을 만족 → 소유 ② 표식 없고 command가 hookBinaryName → **인수**
// ③ 표식 값이 기준을 벗어나거나 표식도 없고 command도 다른 값 → 사용자 소유.
// 표식은 서브테이블과 인라인 대입 두 형태를 모두 인식한다(D80 — 호스트가 어느 형태로
// 되쓰는지는 미검증이라 스캔이 둘 다 본다). 인라인 대입의 키는 맨 키와 따옴표 키가 TOML에서
// 같은 키이므로 둘 다 인식한다 — 따옴표 키를 부재로 읽으면 소유를 놓치고 표식을 다시 넣는다.
func TestCodexTableOwnershipAndAdoption(t *testing.T) {
	cases := []struct {
		name      string
		src       string
		wantOwned bool
	}{
		{"① 표식 버전 있음", "[mcp_servers.ctr]\ncommand = \"other\"\n[mcp_servers.ctr.env]\nCTR_MANAGED = \"context-router/0.14.0\"\n", true},
		{"① 표식 무버전(D82 정확 일치)", "[mcp_servers.ctr]\ncommand = \"other\"\n[mcp_servers.ctr.env]\nCTR_MANAGED = \"context-router\"\n", true},
		{"① 인라인 env 표식", "[mcp_servers.ctr]\ncommand = \"other\"\nenv = { CTR_MANAGED = \"context-router/0.15.0\" }\n", true},
		{"① 인라인 env 표식(따옴표 키)", "[mcp_servers.ctr]\ncommand = \"other\"\nenv = { \"CTR_MANAGED\" = \"context-router/0.15.0\" }\n", true},
		{"② 인수 — 표식 없고 command 일치", "[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = []\n", true},
		{"③ 표식 값이 남의 것", "[mcp_servers.ctr]\ncommand = \"other\"\n[mcp_servers.ctr.env]\nCTR_MANAGED = \"other-tool/1.0\"\n", false},
		{"③ 표식도 없고 command도 다르다", "[mcp_servers.ctr]\ncommand = \"other\"\n", false},
		{"③ 키만 있고 값이 비었다", "[mcp_servers.ctr]\ncommand = \"other\"\n[mcp_servers.ctr.env]\nCTR_MANAGED = \"\"\n", false},
		// T3-F2: 인라인 표식 값이 문자열이 아니면(0) 뒤 키의 문자열 값("context-router/…")까지
		// 스캔이 집어삼켜 소유로 오판했다 — tomlInlineValue가 '=' 바로 다음 토큰만 보게 고쳤다.
		{"③ 인라인 표식 값이 문자열 아님(뒤 키로 오염 배제)", "[mcp_servers.ctr]\ncommand = \"other\"\nenv = { CTR_MANAGED = 0, X = \"context-router/0.15.0\" }\n", false},
	}
	for _, c := range cases {
		lines, sp, view := codexLinesOf(t, c.src)
		marker, found := codexMarkerValue(lines, sp, view)
		if got := codexOwnership(marker, found, view.command, false); got != c.wantOwned {
			t.Errorf("%s: owned=%v want %v (marker=%q found=%v command=%q)", c.name, got, c.wantOwned, marker, found, view.command)
		}
	}
	// 인수 범위는 관리 테이블 이름 하나다 — ctr-exec은 command가 같아도 관리 대상이 아니다.
	_, sp, _ := codexLinesOf(t, "[mcp_servers.ctr-exec]\ncommand = \"context-router\"\n")
	if sp.table.found {
		t.Errorf("ctr-exec이 관리 테이블로 잡혔다")
	}
	// D84 마지막 절: 구 블록 안이면 표식도 command도 없어도 소유다(v1.0에서 이 절만 지운다).
	if !codexOwnership("", false, "/abs/path/context-router", true) {
		t.Errorf("구 BEGIN/END 블록 안의 우리 테이블이 소유로 인정되지 않았다")
	}
}

// TestCodexTableBodyPreservesUnownedKeys — D80 왕복 보존(§2-1 키 보존). 관리 테이블 안에서
// install이 소유하는 키는 command·args·enabled_tools 셋(+ env.CTR_MANAGED)이고, 그 밖의 키는
// 사용자 것이라 **원문 그대로** 되돌린다. [mcp_servers.ctr.env]에서도 CTR_MANAGED만 우리
// 것이다. D81이 승인 모드 키를 기입하지 않기로 했고 사용자가 그 키를 놓을 자리는 이 테이블
// 안뿐이라, 이 보존은 그 결정의 전제다.
// 같은 테스트가 **인라인 env 표기 파일의 기입 규칙**(D80)도 본다 — 서브테이블 헤더를 붙이지
// 않고 그 줄 안에서 표식만 갈아 끼우며, 값이 같은 앞선 사용자 키를 대신 바꾸지 않는다.
func TestCodexTableBodyPreservesUnownedKeys(t *testing.T) {
	src := "[mcp_servers.ctr]\n" +
		"command = \"context-router\"\n" +
		"args = []\n" +
		"enabled_tools = [\"ctr_search\"]\n" +
		"default_tools_approval_mode = \"prompt\"\n" +
		"startup_timeout_sec = 30\n"
	lines, sp, view := codexLinesOf(t, src)
	got := string(codexTableBody(lines, sp.table, view, []string{"ingest", "net"}, false, "context-router/0.15.0", "\n"))
	for _, want := range []string{
		"[mcp_servers.ctr]\n",
		"command = \"context-router\"\n",
		"args = [\"--enable\", \"ingest,net\"]\n",
		"\"ctr_index\"",
		"\"ctr_fetch_and_index\"",
		"default_tools_approval_mode = \"prompt\"\n",
		"startup_timeout_sec = 30\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("본문에 %q 없음:\n%s", want, got)
		}
	}
	// 프로필이 비면 args 키 자체를 쓰지 않는다(재직렬화가 args = []를 지운다, D80).
	empty := string(codexTableBody(lines, sp.table, view, nil, false, "context-router/0.15.0", "\n"))
	if strings.Contains(empty, "args = ") {
		t.Errorf("빈 프로필인데 args 줄을 썼다:\n%s", empty)
	}
	// env 서브테이블: CTR_MANAGED만 우리 것이고 나머지 환경변수는 보존한다.
	esrc := "[mcp_servers.ctr.env]\nCTR_MANAGED = \"context-router/0.14.0\"\nCTR_STORE_ROOT = \"D:/ctr\"\n"
	elines := splitLinesKeepEnds([]byte(esrc))
	esp := codexManagedSpans(elines)
	ebody := string(codexEnvBody(elines, esp.env, "context-router/0.15.0", "\n"))
	if !strings.Contains(ebody, "CTR_MANAGED = \"context-router/0.15.0\"\n") {
		t.Errorf("표식이 현재 값으로 갱신되지 않았다:\n%s", ebody)
	}
	if !strings.Contains(ebody, "CTR_STORE_ROOT = \"D:/ctr\"\n") {
		t.Errorf("사용자 환경변수가 사라졌다:\n%s", ebody)
	}
	if strings.Count(ebody, "CTR_MANAGED") != 1 {
		t.Errorf("표식이 중복 기입됐다:\n%s", ebody)
	}
	// 인라인 env 표기(D80 기입 규칙): 서브테이블 헤더를 붙이지 않고 그 줄 안에서 표식만
	// 갈아 끼운다. 표식과 **값이 같은** 사용자 키를 앞에 둬, 첫 일치 치환이 그 키를 대신
	// 바꾸는 구현을 배제한다 — 그러면 사용자 환경변수가 조용히 바뀌고 표식은 옛 값으로 남는다.
	isrc := "[mcp_servers.ctr]\n" +
		"command = \"context-router\"\n" +
		"env = { CTR_STORE_ROOT = \"context-router/0.14.0\", CTR_MANAGED = \"context-router/0.14.0\" }\n"
	ilines, isp, iview := codexLinesOf(t, isrc)
	ibody := string(codexTableBody(ilines, isp.table, iview, nil, false, "context-router/0.15.0", "\n"))
	if !strings.Contains(ibody, "CTR_MANAGED = \"context-router/0.15.0\"") {
		t.Errorf("인라인 표식이 갱신되지 않았다:\n%s", ibody)
	}
	if !strings.Contains(ibody, "CTR_STORE_ROOT = \"context-router/0.14.0\"") {
		t.Errorf("값이 같은 앞선 사용자 키가 대신 바뀌었다:\n%s", ibody)
	}
	if strings.Contains(ibody, "["+codexManagedEnv+"]") {
		t.Errorf("인라인 표기 파일에 env 서브테이블 헤더를 붙였다 — 중복 정의다:\n%s", ibody)
	}
	// 표식 키가 없는 인라인 대입에는 여는 중괄호 뒤에 더한다(그 밖의 키는 그대로).
	nsrc := "[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { CTR_STORE_ROOT = \"D:/ctr\" }\n"
	nlines, nsp, nview := codexLinesOf(t, nsrc)
	nbody := string(codexTableBody(nlines, nsp.table, nview, nil, false, "context-router/0.15.0", "\n"))
	if !strings.Contains(nbody, codexMarkerKey+" = \"context-router/0.15.0\"") ||
		!strings.Contains(nbody, "CTR_STORE_ROOT = \"D:/ctr\"") {
		t.Errorf("인라인 대입에 표식을 더하면서 기존 키를 보존하지 못했다:\n%s", nbody)
	}
	// 따옴표 키 인라인 표식: TOML이 같은 키로 읽으므로 값 구간만 갈아 끼우고 키를 다시 더하지
	// 않는다 — 더하면 같은 인라인 테이블에 중복 키가 생겨 사용자 Codex 전체가 깨진다.
	qsrc := "[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { \"CTR_MANAGED\" = \"context-router/0.14.0\" }\n"
	qlines, qsp, qview := codexLinesOf(t, qsrc)
	qbody := string(codexTableBody(qlines, qsp.table, qview, nil, false, "context-router/0.15.0", "\n"))
	if !strings.Contains(qbody, "\"CTR_MANAGED\" = \"context-router/0.15.0\"") {
		t.Errorf("따옴표 키 인라인 표식이 제자리에서 갱신되지 않았다:\n%s", qbody)
	}
	if strings.Count(qbody, codexMarkerKey) != 1 {
		t.Errorf("따옴표 키를 부재로 읽고 표식을 다시 더했다 — 중복 키다:\n%s", qbody)
	}
	// keepArgs=true면 args·enabled_tools를 손대지 않고 원문으로 되돌린다(D81 되읽기 실패).
	ksrc := "[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = [\"--profile\", \"global-search\"]\nenabled_tools = [\"ctr_search\"]\n"
	klines, ksp, kview := codexLinesOf(t, ksrc)
	kbody := string(codexTableBody(klines, ksp.table, kview, defaultMCPProfiles, true, "context-router/0.15.0", "\n"))
	if !strings.Contains(kbody, "args = [\"--profile\", \"global-search\"]\n") || strings.Contains(kbody, "ctr_index") {
		t.Errorf("되읽지 못한 args·enabled_tools를 손댔다:\n%s", kbody)
	}
	// T3-F1: 따옴표 키 안의 '='가 첫 '=' 기준 분리로 조기 절단되면 사용자 키 "args=x"가 예약어
	// "args"로 오분류돼 codexReadTable의 continue가 그 줄을 보존 목록에서 빼 재기입 때 지운다 —
	// codexKeyName이 닫는 따옴표 뒤에서 '='를 찾아야 막힌다.
	fsrc := "[mcp_servers.ctr]\ncommand = \"context-router\"\n\"args=x\" = \"y\"\nreal_user_key = 1\n"
	flines, fsp, fview := codexLinesOf(t, fsrc)
	fbody := string(codexTableBody(flines, fsp.table, fview, nil, false, "context-router/0.15.0", "\n"))
	if !strings.Contains(fbody, "\"args=x\" = \"y\"\n") {
		t.Errorf("따옴표 키 안의 '='가 조기 절단돼 사용자 줄이 사라졌다:\n%s", fbody)
	}
	// 같은 오분류가 codexEnvBody의 표식 건너뛰기에도 있었다(T3-F1의 넷째 손실 경로) —
	// "CTR_MANAGED=x"가 표식 키로 오인되면 그 줄이 건너뛰어져 사라진다.
	fesrc := "[mcp_servers.ctr.env]\n\"CTR_MANAGED=x\" = \"y\"\nCTR_MANAGED = \"context-router/0.14.0\"\n"
	felines := splitLinesKeepEnds([]byte(fesrc))
	fesp := codexManagedSpans(felines)
	febody := string(codexEnvBody(felines, fesp.env, "context-router/0.15.0", "\n"))
	if !strings.Contains(febody, "\"CTR_MANAGED=x\" = \"y\"\n") {
		t.Errorf("codexEnvBody에서 따옴표 키 오분류로 사용자 줄이 사라졌다:\n%s", febody)
	}
	// T3-F3: 빈 인라인 env 뒤에 '}'를 담은 주석이 있으면(예: "env = {} # }") LastIndex 기반 빈
	// 판정이 그 주석의 '}'를 진짜 닫는 중괄호로 오인해 후행 쉼표(TOML 금지)를 만든다.
	csrc := "[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = {} # }\n"
	clines, csp, cview := codexLinesOf(t, csrc)
	cbody := string(codexTableBody(clines, csp.table, cview, nil, false, "context-router/0.15.0", "\n"))
	if !strings.Contains(cbody, codexMarkerKey+` = "context-router/0.15.0"} # }`) {
		t.Errorf("빈 인라인 env + 후행 주석 처리가 후행 쉼표를 만들었다(TOML 금지):\n%s", cbody)
	}
}

// A1(적대적 리뷰) — 인라인 env 대입이 표식 키의 여는 큰따옴표에서 끝나면 basicStringLen이
// len(s)를 돌려주고 tomlInlineValue가 s[v+1:v+len-1]로 역순 슬라이스해 패닉했다. internal/cli에
// recover가 없어 프로세스가 죽고, **읽기 전용인 doctor [20]까지 함께 죽었다.** 미종료 문자열은
// 이미 무효 TOML이므로 tomlStringList·inlineMarkerSpan과 같은 "다루지 않는 형태"로 보낸다.
func TestCodexUnterminatedInlineMarkerNoPanic(t *testing.T) {
	cfg := []byte("[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { " + codexMarkerKey + " = \"\n")
	// 세 진입점이 모두 tomlInlineValue를 탄다 — 하나라도 패닉하면 이 테스트가 죽는다.
	res := installCodexConfigBlock(cfg, codexInstallRequest{
		Profiles: defaultMCPProfiles, SetProfile: true, Marker: hookMarker("0.15.0"),
	})
	if res.Out == nil {
		t.Error("install 산출이 nil")
	}
	uninstallCodexConfigBlock(cfg)
	probeCodexMCPBlock(cfg)
	// 키는 있고 값이 다루지 않는 형태이면 ("", true)가 기존 계약이다(비문자열 표식과 같다) —
	// 요점은 그 값이 소유로 읽히지 않는 것이다.
	if marker, _, found := codexConfigMarker(cfg); found && isOurMarkerValue(marker) {
		t.Errorf("미종료 문자열을 우리 표식으로 읽었다: %q", marker)
	}
}

// A2(적대적 리뷰) — 우리 구간 안에 닫히지 않은 '['가 있으면 스캐너 열림이 EOF까지 유지돼
// 그 뒤 헤더가 경계로 잡히지 않고, 우리가 append한 [mcp_servers.ctr] 헤더까지 그 구간에
// 삼켜져 재실행마다 같은 테이블이 하나씩 붙었다(실측 1→2→3). 매 실행 Changed=true라
// D84 단일 백업 슬롯이 2회차에 원본을 잃는다. 입력이 이미 무효 TOML이므로 무변경으로 닫는다.
func TestCodexUnclosedBracketFailsClosed(t *testing.T) {
	cfg := []byte("[mcp_servers.ctr.env]\n" + codexMarkerKey + " = \"context-router/0.15.0\"\nLIST = [\n")
	req := codexInstallRequest{Profiles: defaultMCPProfiles, SetProfile: true, Marker: hookMarker("0.15.0")}
	res := installCodexConfigBlock(cfg, req)
	if res.Changed {
		t.Errorf("닫히지 않은 대괄호 파일을 바꿨다(state=%d):\n%s", res.State, res.Out)
	}
	if n := bytes.Count(res.Out, []byte("[mcp_servers.ctr]")); n != 0 {
		t.Errorf("무효 TOML에 관리 테이블 헤더를 붙였다: %d개\n%s", n, res.Out)
	}
	if !bytes.Equal(res.Out, cfg) {
		t.Errorf("무변경이어야 하는데 바이트가 달라졌다:\n%s", res.Out)
	}
}

// C1(Codex 교차 리뷰) — classifyMarkers가 tomlLineScanner 상태를 보지 않아 여러 줄 문자열
// **내용**인 마커 줄이 마커 쌍으로 세어졌다. 그러면 그 사이의 **미소유** [mcp_servers.ctr]가
// 소유로 판정돼 install이 사용자 command를 덮고 uninstall이 그 테이블을 통째로 지웠다.
// 입력·산출 모두 유효 TOML이라 눈에 띄지 않는다. 이 릴리스가 문서로 주장하는 "소유 판정이
// 파괴적 계산보다 앞이고 미소유 파일은 그대로 반환된다"의 반례였다.
func TestCodexMarkerInsideMultilineStringNotOwned(t *testing.T) {
	cfg := []byte("[history]\nnotes = \"\"\"\n" + codexBlockBegin + "\n\"\"\"\n\n" +
		"[mcp_servers.ctr]\ncommand = \"/opt/mine/my-wrapper\"\nargs = [\"--serve\"]\n" + codexBlockEnd + "\n")
	res := installCodexConfigBlock(cfg, codexInstallRequest{
		Profiles: defaultMCPProfiles, SetProfile: true, Marker: hookMarker("0.15.0"),
	})
	if res.Changed {
		t.Errorf("문자열 내용인 마커로 미소유 테이블을 덮었다(state=%d):\n%s", res.State, res.Out)
	}
	if !bytes.Contains(res.Out, []byte(`command = "/opt/mine/my-wrapper"`)) {
		t.Errorf("사용자 command가 사라졌다:\n%s", res.Out)
	}
	if !bytes.Contains(res.Out, []byte(codexBlockBegin)) {
		t.Errorf("문자열 내용 줄이 지워졌다:\n%s", res.Out)
	}
	out, changed := uninstallCodexConfigBlock(cfg)
	if changed {
		t.Errorf("uninstall이 미소유 테이블을 건드렸다:\n%s", out)
	}
	if !bytes.Contains(out, []byte("[mcp_servers.ctr]")) {
		t.Errorf("uninstall이 미소유 테이블을 지웠다:\n%s", out)
	}
}
