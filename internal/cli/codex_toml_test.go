package cli

import (
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
