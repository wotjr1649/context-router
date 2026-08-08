package cli

import (
	"strings"
	"testing"
)

// codexBOM — 선두 UTF-8 BOM. 리터럴 세 바이트로 쓴다: go-toml이 내는 거부 사유가
// "U+00EF로 시작하는 키"라 이 결함은 코드포인트가 아니라 바이트 셋의 문제로 드러난다.
const codexBOM = "\xEF\xBB\xBF"

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

// TestCodexTOMLParsesLeadingBOM — 선두 UTF-8 BOM은 판정 전에 무시한다. go-toml은 그 세 바이트를
// 키의 첫 글자로 읽어 거부하는데, Windows 편집기(PowerShell 5.1의 `Out-File -Encoding utf8`,
// 구버전 메모장)가 붙이는 바이트라 사용자 config.toml에 실재한다. 판정이 거부하면 그 파일에서
// **입력이 무효로 서고 게이트의 비대칭 절이 통째로 꺼진다**(D89) — 되돌림이 가장 필요한 자리에서
// 되돌림이 사라진다.
// 무효 판정이 그대로 남는 것도 함께 고정한다: BOM을 떼는 것이 "판정을 무르게 하는 것"으로
// 바뀌면 그 파일에서는 게이트가 무엇이든 통과시킨다.
func TestCodexTOMLParsesLeadingBOM(t *testing.T) {
	if !codexTOMLParses([]byte(codexBOM + "[mcp_servers.ctr]\ncommand = \"context-router\"\n")) {
		t.Errorf("선두 BOM이 붙은 정상 파일을 무효로 판정한다")
	}
	if codexTOMLParses([]byte(codexBOM + "[a]\nx = 1\nx = 2\n")) {
		t.Errorf("BOM을 뗀 뒤에도 무효는 무효여야 한다")
	}
	// 선두가 아닌 BOM은 떼지 않는다 — 키 자리의 그 바이트는 실제로 무효이고, 값 안의 것은
	// 사용자 바이트라 우리가 해석을 바꿀 자리가 아니다.
	if codexTOMLParses([]byte("[a]\n" + codexBOM + "x = 1\n")) {
		t.Errorf("선두가 아닌 BOM까지 떼어 무효를 유효로 읽는다")
	}
}

// TestDoctorCodexBOMNoFalseAlarm — D97 계약 2 재기준선(Task 4가 doctor의 [16]을 codex_toml.go
// 판정에서 codexServerHeaders 줄 스캔으로 갈아 끼웠다). BOM은 codexServerHeaders가 trimBOM으로
// 먼저 벗기므로 줄 번호를 밀어내지 않는다 — codex_scan_test.go의 "BOM이 붙은 입력" 케이스가
// 탐지기 자체를 이미 재지만, 이 테스트는 doctor 배선(os.ReadFile → codexServerHeaders → 보고
// 줄)이 그 값을 그대로 실어 나르는지를 잰다 — 배선이 깨지면 탐지기가 맞아도 doctor 출력이
// 틀릴 수 있다.
func TestDoctorCodexBOMNoFalseAlarm(t *testing.T) {
	home := isolateCodexHome(t)
	writeCodexConfig(t, home, codexBOM+"model = \"gpt-5\"\n\n[mcp_servers.ctr]\ncommand = \"context-router\"\n")
	out, _ := doctorOut(t, t.TempDir())
	if !strings.Contains(out, "플러그인 이전 방식의 등록물이 남아 있다") {
		t.Fatalf("BOM 파일의 등록물을 doctor가 놓쳤다:\n%s", out)
	}
	// 재기준선(릴리스 리뷰 F1): 보고 줄이 이제 서버 이름을 함께 든다 — 거름이 생겨 그 이름이
	// 알려진 리터럴이기 때문이다. 줄 번호 단정의 취지는 그대로다.
	if !strings.Contains(out, ":3 (ctr)\n") {
		t.Errorf("BOM이 줄 번호를 밀어냈다 — 헤더는 3번째 줄이어야 한다:\n%s", out)
	}
}
