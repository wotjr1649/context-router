package cli

import (
	"slices"
	"testing"
)

// TestCodexServerHeaders — codexServerHeaders가 잡아야 할 형태와 일부러 잡지 않는 형태를
// 함께 표로 둔다(D97 계약 2). 무효 TOML·BOM·CRLF는 실제로 이 함수가 존재하는 이유이므로
// 각각 별도 케이스로 둔다.
func TestCodexServerHeaders(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []codexHeaderHit
	}{
		{
			"헤더 하나",
			"[mcp_servers.ctr]\ncommand = \"context-router\"\n",
			[]codexHeaderHit{{Name: "ctr", Line: 1}},
		},
		{
			"헤더 없음",
			"a = 1\n[other_table]\nb = 2\n",
			nil,
		},
		{
			"들여쓰기 + 줄 끝 공백",
			"   [mcp_servers.ctr]   \ncommand = \"context-router\"\n",
			[]codexHeaderHit{{Name: "ctr", Line: 1}},
		},
		{
			"헤더 안 공백",
			"[ mcp_servers . foo ]\n",
			[]codexHeaderHit{{Name: "foo", Line: 1}},
		},
		{
			"따옴표 있는 이름 — 이중",
			`[mcp_servers."my-server"]` + "\n",
			[]codexHeaderHit{{Name: "my-server", Line: 1}},
		},
		{
			"따옴표 있는 이름 — 단일",
			"[mcp_servers.'my-server']\n",
			[]codexHeaderHit{{Name: "my-server", Line: 1}},
		},
		{
			"mcp_servers 자체는 잡지 않는다(하위 이름 없음)",
			"[mcp_servers]\nfoo = 1\n",
			nil,
		},
		{
			"mcp_servers로 시작하지만 다른 테이블은 잡지 않는다",
			"[mcp_servers2.ctr]\ncommand = \"x\"\n",
			nil,
		},
		{
			"배열 테이블([[ ]])은 잡지 않는다",
			"[[mcp_servers.ctr]]\ncommand = \"x\"\n",
			nil,
		},
		{
			"무효 TOML에서도 헤더를 찾는다 — 닫히지 않은 배열 뒤에도 계속 스캔한다",
			"[mcp_servers.foo]\ncommand = \"x\"\nbad = [1, 2,\n[mcp_servers.bar]\ncommand = \"y\"\n",
			[]codexHeaderHit{{Name: "foo", Line: 1}, {Name: "bar", Line: 4}},
		},
		{
			"BOM이 붙은 입력",
			"\xEF\xBB\xBF[mcp_servers.ctr]\ncommand = \"context-router\"\n",
			[]codexHeaderHit{{Name: "ctr", Line: 1}},
		},
		{
			"CRLF 입력",
			"[mcp_servers.foo]\r\ncommand = \"x\"\r\n[mcp_servers.bar]\r\n",
			[]codexHeaderHit{{Name: "foo", Line: 1}, {Name: "bar", Line: 3}},
		},
		{
			"줄 번호 1-기반 — 헤더가 흩어진 입력 하나",
			"# comment\n[mcp_servers.foo]\ncommand = \"x\"\n\n[mcp_servers.bar]\ncommand = \"y\"\n",
			[]codexHeaderHit{{Name: "foo", Line: 2}, {Name: "bar", Line: 5}},
		},
		{
			// 실측 형태(session progress.md) — 실제 ~/.codex/config.toml에 이 모양의 서버 테이블이
			// 둘 있었다. env 값은 비밀일 수 있어 여기 재현하지 않는다(그 줄은 헤더가 아니라 이
			// 함수가 애초에 보지 않는다).
			"실측 형태 — 서버 둘 + 각자의 env 서브테이블",
			"[mcp_servers.ctr]\ncommand = \"context-router\"\n[mcp_servers.ctr.env]\n" +
				"CTR_MANAGED = \"context-router/0.17.0\"\n\n" +
				"[mcp_servers.ctr-exec]\ncommand = \"context-router\"\n[mcp_servers.ctr-exec.env]\n" +
				"CTR_MANAGED = \"context-router/0.17.0\"\n",
			[]codexHeaderHit{
				{Name: "ctr", Line: 1},
				{Name: "ctr.env", Line: 3},
				{Name: "ctr-exec", Line: 6},
				{Name: "ctr-exec.env", Line: 8},
			},
		},
		{
			"닫는 대괄호 뒤 주석 — # 앞에 공백 하나",
			"[mcp_servers.ctr] # hand-added 2026-05\n",
			[]codexHeaderHit{{Name: "ctr", Line: 1}},
		},
		{
			"닫는 대괄호 뒤 주석 — # 뒤에 공백 없음",
			"[mcp_servers.ctr]  #comment with no space\n",
			[]codexHeaderHit{{Name: "ctr", Line: 1}},
		},
		{
			"닫는 대괄호 뒤 주석 — 주석 안에 #과 ]가 또 있다",
			"[mcp_servers.ctr] # see #2 and [docs]\n",
			[]codexHeaderHit{{Name: "ctr", Line: 1}},
		},
		{
			"따옴표 안의 # — 주석 시작이 아니라 이름의 일부",
			`[mcp_servers."a#b"]` + "\n",
			[]codexHeaderHit{{Name: "a#b", Line: 1}},
		},
		{
			"따옴표 안의 ] — 닫는 대괄호로 오인하지 않는다",
			`[mcp_servers."a]b"]` + "\n",
			[]codexHeaderHit{{Name: "a]b", Line: 1}},
		},
		{
			// codexHeaderClose의 이스케이프 건너뛰기 회귀 방지 — \"가 문자열을 조기 종료하면
			// 진짜 닫는 대괄호를 못 찾아 줄 전체를 놓친다. 이스케이프 자체는 해석하지 않으므로
			// (unquoteHeaderName과 같은 비범위) 이름에는 \" 두 글자가 그대로 남는다.
			"따옴표 안의 이스케이프된 큰따옴표 — 조기 종료로 줄 전체를 놓치지 않는다",
			`[mcp_servers."a\"b"]` + "\n",
			[]codexHeaderHit{{Name: `a\"b`, Line: 1}},
		},
		{
			"대괄호 뒤가 주석도 빈 것도 아니면 잡지 않는다(두 번째 알려진 한계)",
			"[mcp_servers.ctr] not a comment\n",
			nil,
		},
		{
			// 안 닫힌 큰따옴표(닫는 따옴표를 빠뜨린 오타) — codexHeaderClose가 최선-노력으로
			// 마지막 ']'를 닫는 자리로 쓴다(세 번째 알려진 한계). 판정은 살고 이름만
			// 지저분하다 — unquoteHeaderName이 양끝 짝을 못 채워 앞 따옴표가 그대로 남는다.
			"안 닫힌 큰따옴표 — 헤더로는 잡히고 이름만 지저분하다",
			`[mcp_servers."my-server]` + "\n",
			[]codexHeaderHit{{Name: `"my-server`, Line: 1}},
		},
		{
			"안 닫힌 작은따옴표 — 헤더로는 잡히고 이름만 지저분하다",
			`[mcp_servers.'my-server]` + "\n",
			[]codexHeaderHit{{Name: `'my-server`, Line: 1}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := codexServerHeaders([]byte(c.input))
			if !slices.Equal(got, c.want) {
				t.Fatalf("codexServerHeaders() = %+v, want %+v", got, c.want)
			}
		})
	}
}
