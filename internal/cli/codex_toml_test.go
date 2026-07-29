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
// 되지 못한다. 한 줄 문자열·주석 줄(line 10)은 어느 구현에서도 경계가 아니라 감시선이
// 아니지만, '['를 포함하기만 하면 경계로 보는 구현을 배제하는 자리로 남긴다.
func TestTOMLLineScannerHeaderBoundary(t *testing.T) {
	src := "" +
		"[a]\n" + // 0 경계 a
		"k = \"\"\"\n" + // 1
		"[mcp_servers.x]\n" + // 2 여러 줄 기본 문자열 안 — 경계 아님
		"\"\"\"\n" + // 3
		"m = '''\n" + // 4
		"[mcp_servers.y]\n" + // 5 여러 줄 리터럴 안 — 경계 아님
		"'''\n" + // 6
		"arr = [\n" + // 7
		"  [1, 2],\n" + // 8 여러 줄 배열 안의 중첩 배열 — 경계 아님(depth 추적의 감시선)
		"]\n" + // 9
		"s = \"[not.a.header]\"  # [also.not]\n" + // 10 한 줄 문자열·주석 — 경계 아님
		"[[b]]\n" + // 11 배열 테이블 — 경계이나 이름 없음
		"[c] # trailing\n" // 12 경계 c
	lines := splitLinesKeepEnds([]byte(src))
	var sc tomlLineScanner
	type got struct {
		boundary bool
		name     string
	}
	want := map[int]got{
		0: {true, "a"}, 2: {false, ""}, 5: {false, ""}, 8: {false, ""},
		10: {false, ""}, 11: {true, ""}, 12: {true, "c"},
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
		"CTR_MANAGED = \"context-router/0.15.0\"\n" + // 6
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
