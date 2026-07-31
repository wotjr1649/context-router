package cli

import (
	"strings"
	"testing"
)

// TestCodexTOMLParsesMesh — 스펙 §3 표2(그물의 눈금)를 고정한다. 이 표가 게이트가 무엇을
// 잡고 무엇을 못 잡는지의 계약이며, 어긋나면 파서 선택을 다시 여는 것이 §1.3 선행 게이트 2다.
// **못 잡는 행도 함께 고정한다** — 그 행이 조용히 "잡는다"로 바뀌면 게이트가 구문만 본다는
// 계약이 깨진 것이고, 반대로 잡던 행이 통과로 바뀌면 파손 경로가 다시 열린다.
func TestCodexTOMLParsesMesh(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		valid bool // true면 파스된다(게이트가 못 잡는다)
	}{
		{"점 표기 테이블을 뒤 헤더가 재정의", "[mcp_servers.ctr]\nenv.FOO = \"bar\"\n\n[mcp_servers.ctr.env]\nCTR_MANAGED = \"x\"\n", false},
		{"TOML 명세의 무효 예제", "[fruit]\napple.color = \"red\"\n\n[fruit.apple]\n", false},
		{"같은 테이블 헤더 두 번", "[a]\nx = 1\n\n[a]\ny = 2\n", false},
		{"같은 테이블 안 키 두 번", "[a]\nx = 1\nx = 2\n", false},
		{"점 표기 같은 키 두 번", "[a]\nenv.FOO = \"1\"\nenv.FOO = \"2\"\n", false},
		{"같은 인라인 테이블 키 두 번", "[a]\nenv = { K = \"1\", K = \"2\" }\n", false},
		{"이스케이프 헤더 이름 중복", "[mcp_servers.ctr]\nx = 1\n\n[mcp_servers.\"ct\\u0072\"]\ny = 2\n", false},
		{"닫히지 않은 문자열", "[a]\nx = \"abc\n", false},
		{"닫히지 않은 인라인 테이블", "[a]\nenv = { K = \"x\"\n", false},
		{"큰따옴표 값 안 텍스트 치환", "[a]\nenv = { A = \"pre\"context-router/0.17.0\"post\" }\n", false},

		// 아래는 **못 잡는다**. 게이트가 구문만 본다는 계약의 반대편이다.
		{"인라인 테이블 후행 쉼표(파서 관용)", "[a]\nenv = { K = \"x\",}\n", true},
		{"홑따옴표 값 안 텍스트 치환", "[a]\nnote = 'CTR_MANAGED = \"x\"'\n", true},
		{"주석 안 표식", "[a]\nCTR_MANAGED = 1 # \"context-router/0.16.0\"\n", true},
		{"따옴표 안 공백 키", "[a]\n\"e n v\" = { K = \"x\" }\n", true},
		{"짝 없는 종료 마커 잔여", "# END context-router\n[a]\nx = 1\n", true},
		{"env 우변이 인라인 테이블이 아님", "[a]\nenv = []\n", true},
		{"점 표기 env + 헤더 없음(우리 정상 산출)", "[mcp_servers.ctr]\ncommand = \"context-router\"\nenv.CTR_MANAGED = \"context-router/0.17.0\"\n", true},
		{"유효한 대조군", "[mcp_servers.ctr]\ncommand = \"context-router\"\n\n[mcp_servers.ctr.env]\nCTR_MANAGED = \"x\"\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := codexTOMLParses([]byte(c.src)); got != c.valid {
				t.Errorf("codexTOMLParses = %v, want %v", got, c.valid)
			}
		})
	}
}

// FuzzCodexTOMLParsesNoPanic — §1.3 선행 게이트 1. 게이트는 읽기 전용 doctor가 매 실행
// 부르는 경로에 놓이고 이 패키지에는 recover가 없으므로, 파서가 임의 바이트에 패닉하면
// 진단이 함께 죽는다.
func FuzzCodexTOMLParsesNoPanic(f *testing.F) {
	f.Add([]byte("[a]\nx = 1\n"))
	f.Add([]byte("[a]\nenv = { K = \"x\"\n"))
	f.Add([]byte("\x00\xff[[[\n\"\"\"\n"))
	f.Add([]byte(strings.Repeat("[a.b.c]\n", 64)))
	f.Fuzz(func(t *testing.T, b []byte) {
		_ = codexTOMLParses(b)
	})
}
