package cli

import (
	"slices"
	"strings"
	"testing"
)

func TestInstallCodexConfigBlock(t *testing.T) {
	block := "# BEGIN context-router\n[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = []\nenabled_tools = [\"ctr_search\", \"ctr_fetch\", \"ctr_transform\", \"ctr_record_event\", \"ctr_session_summary\", \"ctr_export_events\"]\n# ingest/net 활성화 시 권장: default_tools_approval_mode = \"prompt\"\n# END context-router\n"
	cases := []struct {
		name      string
		existing  string
		wantState codexMCPState
		wantOut   string // "" = existing 무변경 기대
	}{
		{"빈 파일 append", "", mcpWritten, block},
		{"기존 내용 뒤 append(개행 있음)", "a = 1\n", mcpWritten, "a = 1\n\n" + block},
		{"무개행 EOF — 개행 1개 추가 후 append", "a = 1", mcpWritten, "a = 1\n\n" + block},
		{"동일 버전 멱등(f(f(x))==f(x))", block, mcpWritten, block},
		{"기존 소유 블록 교체(내용 갱신)", "x = 1\n\n# BEGIN context-router\n[mcp_servers.ctr]\nold = true\n# END context-router\n", mcpWritten, "x = 1\n\n" + block},
		{"충돌: canonical 헤더 → 생략·MCP확정", "[mcp_servers.ctr]\ncommand = \"old\"\n", mcpExistingHeader, ""},
		{"충돌: quoted key", "[mcp_servers.\"ctr\"]\ncommand = \"x\"\n", mcpConflict, ""},
		{"충돌: 인라인 테이블", "mcp_servers.ctr = { command = \"x\" }\n", mcpConflict, ""},
		{"충돌: 부모 테이블+점표기", "[mcp_servers]\nctr.command = \"x\"\n", mcpConflict, ""},
		{"충돌: 루트 완전-점표기(.ctr. 신호)", "mcp_servers.ctr.command = \"x\"\n", mcpConflict, ""},
		{"충돌: 루트 인라인 mcp_servers 전체(ctr 신호 무)", "mcp_servers = { foo = { command = \"x\" } }\n", mcpConflict, ""},
		{"충돌: 루트 인라인 quoted 키", "\"mcp_servers\" = { foo = { command = \"x\" } }\n", mcpConflict, ""},
		{"비충돌: 헤더 정의 + 타 서버 인라인 값", "[mcp_servers]\nfoo = { command = \"x\" }\n", mcpWritten, "[mcp_servers]\nfoo = { command = \"x\" }\n\n" + block},
		{"오탐 회피: electron+타 서버", "[mcp_servers.chrome]\ncommand = \"x\"\n# electron debugging notes\n", mcpWritten, "[mcp_servers.chrome]\ncommand = \"x\"\n# electron debugging notes\n\n" + block},
		{"오탐 회피: spectra 언급", "[mcp_servers.foo]\n# spectra analysis\n", mcpWritten, "[mcp_servers.foo]\n# spectra analysis\n\n" + block},
		{"블록 밖 보존: hooks.state·주석·미지 키 바이트 그대로", "[hooks.state]\ntrust = \"abc123\"\n# user comment\nunknown_key = 1\n", mcpWritten, "[hooks.state]\ntrust = \"abc123\"\n# user comment\nunknown_key = 1\n\n" + block},
		{"마커 이상: END 단독", "# END context-router\n", mcpMarkerAnomaly, ""},
		{"마커 이상: END 부재", "# BEGIN context-router\n[mcp_servers.ctr]\n", mcpMarkerAnomaly, ""},
		{"마커 이상: 역순", "# END context-router\n# BEGIN context-router\n", mcpMarkerAnomaly, ""},
		{"마커 이상: 중복 쌍", "# BEGIN context-router\n# END context-router\n# BEGIN context-router\n# END context-router\n", mcpMarkerAnomaly, ""},
		{"소유권: 유사 마커(자유 접미)는 미소유=마커 0개 취급 append", "# BEGIN context-router migration\nuser = 1\n# END context-router migration\n", mcpWritten, "# BEGIN context-router migration\nuser = 1\n# END context-router migration\n\n" + block},
		{"소유권: 정확 마커+본문 불일치 → 무변경", "# BEGIN context-router\nuser = 1\n# END context-router\n", mcpMarkerAnomaly, ""},
		{"CRLF 보존 + CRLF 블록 기입", "a = 1\r\n", mcpWritten, "a = 1\r\n\r\n" + strings.ReplaceAll(block, "\n", "\r\n")},
		{"CRLF+무개행 EOF — EOF도 CRLF로 정규화", "a = 1\r\nb = 2", mcpWritten, "a = 1\r\nb = 2\r\n\r\n" + strings.ReplaceAll(block, "\n", "\r\n")},
		{"멱등: 접두 존재 재설치 f(f(x))==f(x)", "a = 1\n\n" + block, mcpWritten, "a = 1\n\n" + block},
	}
	for _, c := range cases {
		out, state := installCodexConfigBlock([]byte(c.existing))
		if state != c.wantState {
			t.Fatalf("%s: state=%d want %d", c.name, state, c.wantState)
		}
		wantOut := c.wantOut
		if wantOut == "" {
			wantOut = c.existing
		}
		if string(out) != wantOut {
			t.Fatalf("%s:\n got=%q\nwant=%q", c.name, out, wantOut)
		}
	}
}

func TestUninstallCodexConfigBlock(t *testing.T) {
	block := "# BEGIN context-router\n[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = []\nenabled_tools = [\"ctr_search\", \"ctr_fetch\", \"ctr_transform\", \"ctr_record_event\", \"ctr_session_summary\", \"ctr_export_events\"]\n# ingest/net 활성화 시 권장: default_tools_approval_mode = \"prompt\"\n# END context-router\n"
	cases := []struct {
		name        string
		existing    string
		wantChanged bool
		wantOut     string
	}{
		{"왕복: append 산출물 → 원본+EOF개행", "a = 1\n\n" + block, true, "a = 1\n"},
		{"블록 부재 무변경", "a = 1\n", false, "a = 1\n"},
		{"중간 블록 — 직전이 비-빈줄이면 빈줄 미삭제", "a = 1\n" + block + "b = 2\n", true, "a = 1\nb = 2\n"},
		{"연속 빈 줄 2개 — 정확히 1개만 제거·1개 보존", "a = 1\n\n\n" + block, true, "a = 1\n\n"},
		{"본문 불일치는 미소유 — 무변경", "# BEGIN context-router\nuser = 1\n# END context-router\n", false, "# BEGIN context-router\nuser = 1\n# END context-router\n"},
	}
	for _, c := range cases {
		out, changed := uninstallCodexConfigBlock([]byte(c.existing))
		if changed != c.wantChanged || string(out) != c.wantOut {
			t.Fatalf("%s: changed=%v out=%q (want %v %q)", c.name, changed, out, c.wantChanged, c.wantOut)
		}
	}
}

// D52 — doctor [16] 존재 판별(v0.9 §0): install 상태기계(classReplace/classAppend→동일
// mcpWritten)는 존재/부재를 구분하지 못하므로 별도 판별 헬퍼가 필요하다(적대 검수 P1).
func TestProbeCodexMCPBlock(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		present bool
		anomaly bool
	}{
		{"빈 파일 — 부재", "", false, false},
		{"관리 블록 존재", codexBlockBody, true, false},
		{"블록 밖 맨 헤더", "[mcp_servers.ctr]\ncommand = \"x\"\n", true, false},
		{"마커 이상 — BEGIN만", codexBlockBegin + "\n[mcp_servers.ctr]\n", false, true},
		{"무관 내용 — 부재", "[model]\nname = \"gpt\"\n", false, false},
		{"소유 블록 + 외부 충돌 — 이상", codexBlockBody + "\nmcp_servers = { ctr = { command = \"other\" } }\n", false, true},
	}
	for _, c := range cases {
		p, a := probeCodexMCPBlock([]byte(c.in))
		if p != c.present || a != c.anomaly {
			t.Errorf("%s: present=%v anomaly=%v want %v/%v", c.name, p, a, c.present, c.anomaly)
		}
	}
}

// 왕복 f_uninstall(f_install(x)) — EOF 개행 정규화 제외 바이트 동일(스펙 §0 D48).
// oracle의 EOF 개행은 파일 지배 개행을 따른다(Codex 검수 — "\n" 하드코딩이면 CRLF
// 파일에 LF를 붙이는 혼합-EOL 구현이 통과).
func TestCodexConfigBlockRoundTrip(t *testing.T) {
	for _, orig := range []string{"", "a = 1\n", "a = 1", "a = 1\r\n", "a = 1\r\nb = 2"} {
		installed, state := installCodexConfigBlock([]byte(orig))
		if state != mcpWritten {
			t.Fatalf("install(%q) state=%d", orig, state)
		}
		back, _ := uninstallCodexConfigBlock(installed)
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
