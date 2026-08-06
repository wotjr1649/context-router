package cli

import (
	"bytes"
	"slices"
	"strconv"
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
		t.Run(c.name, func(t *testing.T) {
			res := installFixture(c.existing)
			if res.State != c.wantState {
				t.Fatalf("state=%d want %d", res.State, c.wantState)
			}
			wantOut := c.wantOut
			if wantOut == "" {
				wantOut = c.existing
			}
			if string(res.Out) != wantOut {
				t.Fatalf("got=%q\nwant=%q", res.Out, wantOut)
			}
		})
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

// TestCodexMigrationDropsBothMarkers — 구 블록이 두 테이블을 모두 감쌌고 env가 마지막이면
// 종료 마커가 env 구간 안에 들어 보존 라인으로 옮겨진다. 짝 없는 마커가 남으면 이후
// 마커 분류가 이상으로 떨어져 그 파일의 마이그레이션 경로가 닫힌다.
func TestCodexMigrationDropsBothMarkers(t *testing.T) {
	// keep — **env 구간 안**의 사용자 엔트리. 마커와 표식뿐인 픽스처였다면 제외 조건을 넓혀도
	// (예: 마커 두 줄 사이를 통째로) 마커·파스·멱등 셋이 모두 통과해, 사용자 설정을 지우는
	// 방향이 무방비가 된다. **여러 줄 값**으로 둔 것은 파스 단정에도 이빨을 주기 위해서다 —
	// 남은 마커는 TOML 주석이라 그것만으로는 파스가 깨지지 않지만, 제외가 엔트리 경계를 자르면
	// 삼중 따옴표 잔해가 남아 그때 깨진다.
	const keep = "CTR_KEEP = \"\"\"\nkeep-me\n\"\"\"\n"
	for _, name := range []string{"정방향", "거울"} {
		t.Run(name, func(t *testing.T) {
			// 정방향 — 종료 마커가 env 구간 안(env가 마지막 테이블)
			src := codexBlockBegin + "\n[mcp_servers.ctr]\ncommand = \"context-router\"\n\n[mcp_servers.ctr.env]\nCTR_MANAGED = \"context-router/0.15.0\"\n" + keep + codexBlockEnd + "\n"
			if name == "거울" {
				// **시작 마커가 env 구간 안**이어야 한다. 두 테이블 순서만 바꾸면 시작 마커가
				// 첫 헤더보다 앞에 남아 drop 맵이 이미 지우므로 수정 전에도 통과한다.
				src = "[mcp_servers.ctr.env]\n" + codexBlockBegin + "\nCTR_MANAGED = \"context-router/0.15.0\"\n" + keep + "\n[mcp_servers.ctr]\ncommand = \"context-router\"\n" + codexBlockEnd + "\n"
			}
			res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: hookMarker("0.17.0")})
			if strings.Contains(string(res.Out), codexBlockBegin) || strings.Contains(string(res.Out), codexBlockEnd) {
				t.Errorf("마커가 남았다:\n%s", res.Out)
			}
			if !strings.Contains(string(res.Out), keep) {
				t.Errorf("마커를 빼면서 env 구간의 사용자 엔트리까지 지웠다:\n%s", res.Out)
			}
			if !codexTOMLParses(res.Out) {
				t.Errorf("산출물이 파스되지 않는다:\n%s", res.Out)
			}
			// 멱등 — 바이트를 바꾸는 변환이므로 재실행이 무변경이어야 D84 단일 백업 슬롯이
			// 2회차에 원본을 잃지 않는다.
			again := installCodexConfigBlock(res.Out, codexInstallRequest{Marker: hookMarker("0.17.0")})
			if !bytes.Equal(res.Out, again.Out) || again.Changed {
				t.Errorf("마이그레이션 결과가 멱등이 아니다(changed=%v):\n1: %s\n2: %s", again.Changed, res.Out, again.Out)
			}
		})
	}
}

// TestCodexEnvBodyKeepsUserComment — 무효값을 넘기는 호출부에서 구간 첫 줄이 지워지면 안 된다.
func TestCodexEnvBodyKeepsUserComment(t *testing.T) {
	src := "[mcp_servers.ctr]\ncommand = \"context-router\"\n\n[mcp_servers.ctr.env]\n# 사용자 주석\nUSER = \"keep\"\n"
	res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: hookMarker("0.17.0")})
	if !strings.Contains(string(res.Out), "# 사용자 주석") || !strings.Contains(string(res.Out), "USER = \"keep\"") {
		t.Errorf("보존 라인이 사라졌다:\n%s", res.Out)
	}
	// 이 픽스처도 바이트를 바꾼다(표식이 없어 CTR_MANAGED·enabled_tools가 새로 붙는다) —
	// D84 단일 백업 슬롯이 2회차 무변경 위에 서므로 같은 게이트를 건다.
	again := installCodexConfigBlock(res.Out, codexInstallRequest{Marker: hookMarker("0.17.0")})
	if !bytes.Equal(res.Out, again.Out) || again.Changed {
		t.Errorf("보존 갈래 재실행이 멱등이 아니다(changed=%v):\n1: %s\n2: %s", again.Changed, res.Out, again.Out)
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
		t.Run(c.name, func(t *testing.T) {
			out, changed, _ := uninstallCodexConfigBlock([]byte(c.existing))
			if changed != c.wantChanged || string(out) != c.wantOut {
				t.Fatalf("changed=%v out=%q (want %v %q)", changed, out, c.wantChanged, c.wantOut)
			}
		})
	}
}

// TestProbeCodexMCPBlock — doctor [16] 존재 판별(D52 승계). 우선순위는 install과 1:1이다:
// 중복 정의 > 구간 밖 충돌 > 테이블 존재. 마커 배치 자체는 더 이상 이상이 아니다(D80).
func TestProbeCodexMCPBlock(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		present bool
		anomaly codexAnomaly
	}{
		{"빈 파일 — 부재", "", false, anomalyNone},
		{"관리 테이블 존재", ctrTableFixture, true, anomalyNone},
		{"표식 없는 우리 테이블도 존재", "[mcp_servers.ctr]\ncommand = \"context-router\"\n", true, anomalyNone},
		{"사용자 소유 테이블도 존재로 본다", "[mcp_servers.ctr]\ncommand = \"old\"\n", true, anomalyNone},
		{"무관 내용 — 부재", "[model]\nname = \"gpt\"\n", false, anomalyNone},
		{"관리 테이블 중복 정의 — 이상", "[mcp_servers.ctr]\n[x]\n[mcp_servers.ctr]\n", false, anomalyDupHeader},
		// 충돌 줄은 픽스처 **앞**에 둔다 — 뒤에 두면 [mcp_servers.ctr.env] 구간이 다음 테이블
		// 헤더가 없어 EOF까지 뻗으므로 그 줄이 우리 구간 안이 되고, scanOutsideSpans가 건너뛰어
		// (true,anomalyNone)이 나온다. 구간 밖 신호를 재려면 구간 밖에 두어야 한다.
		{"구간 밖 충돌 — 구간 판정 사유가 아닌 별개 상태", "mcp_servers = { ctr = { command = \"other\" } }\n" + ctrTableFixture, false, anomalyOutsideConflict},
		{"ctr-exec만 있으면 부재", "[mcp_servers.ctr-exec]\ncommand = \"context-router\"\n", false, anomalyNone},
		// codexManagedSpans의 **우선순위 절** 감시선(이월 T1 리뷰 Minor): 중복 헤더와 EOF 스캐너
		// 열림을 한 입력에 담는다. 사유가 문면에 실리는 지금부터 뒤바뀐 사유는 사용자에게 틀린
		// 조치를 지시하므로, sc.open() 갈래의 `out.anomaly == anomalyNone` 절을 지우면 여기서 물린다.
		{"중복 헤더와 스캐너 열림 — 앞 사유를 유지한다", "[mcp_servers.ctr]\n[x]\n[mcp_servers.ctr]\nk = \"\"\"\nunclosed\n", false, anomalyDupHeader},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, a := probeCodexMCPBlock([]byte(c.in))
			if p != c.present || a != c.anomaly {
				t.Errorf("present=%v anomaly=%d want %v/%d", p, a, c.present, c.anomaly)
			}
		})
	}
}

// TestCodexConfigBlockRoundTrip — 왕복 f_uninstall(f_install(x)): EOF 개행 정규화 제외 바이트
// 동일. oracle의 EOF 개행은 파일 지배 개행을 따른다("\n" 하드코딩이면 CRLF 파일에 LF를 붙이는
// 혼합-EOL 구현이 통과한다).
func TestCodexConfigBlockRoundTrip(t *testing.T) {
	for i, orig := range []string{"", "a = 1\n", "a = 1", "a = 1\r\n", "a = 1\r\nb = 2"} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			res := installFixture(orig)
			if res.State != mcpWritten {
				t.Fatalf("install(%q) state=%d", orig, res.State)
			}
			back, _, _ := uninstallCodexConfigBlock(res.Out)
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
		})
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
	if sp.anomaly != anomalyNone {
		t.Errorf("anomaly=%d인데 이상이 없다", sp.anomaly)
	}
	// EOF 종료
	sp2 := codexManagedSpans(splitLinesKeepEnds([]byte("[mcp_servers.ctr]\na = 1\nb = 2\n")))
	if sp2.table.end != 3 {
		t.Errorf("EOF 종료 end=%d want 3", sp2.table.end)
	}
	// 중복 정의
	sp3 := codexManagedSpans(splitLinesKeepEnds([]byte("[mcp_servers.ctr]\n[x]\n[mcp_servers.ctr]\n")))
	if sp3.anomaly != anomalyDupHeader {
		t.Errorf("같은 이름 헤더 둘인데 anomaly=%d want %d", sp3.anomaly, anomalyDupHeader)
	}
	// [mcp_servers.ctr-exec]는 관리 대상이 아니다(D80 — 사용자가 만든 별도 등록)
	sp4 := codexManagedSpans(splitLinesKeepEnds([]byte("[mcp_servers.ctr-exec]\ncommand = \"context-router\"\n")))
	if sp4.table.found || sp4.env.found {
		t.Errorf("ctr-exec을 관리 테이블로 잡았다: %+v", sp4)
	}
	// EOF에서 스캐너가 열린 파일 — 사유가 중복 헤더와 구별돼야 한다
	sp5 := codexManagedSpans(splitLinesKeepEnds([]byte("[mcp_servers.ctr]\nk = \"\"\"\nunclosed\n")))
	if sp5.anomaly != anomalyScannerOpen {
		t.Errorf("EOF 스캐너 열림 anomaly=%d want %d", sp5.anomaly, anomalyScannerOpen)
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
		t.Run(c.name, func(t *testing.T) {
			lines, sp, view := codexLinesOf(t, c.src)
			marker, found := codexMarkerValue(lines, sp, view)
			if got := codexOwnership(marker, found, view.command, false); got != c.wantOwned {
				t.Errorf("owned=%v want %v (marker=%q found=%v command=%q)", got, c.wantOwned, marker, found, view.command)
			}
		})
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
	ebody := string(codexEnvBody(elines, esp.env, "context-router/0.15.0", "\n", -1, -1))
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
	febody := string(codexEnvBody(felines, fesp.env, "context-router/0.15.0", "\n", -1, -1))
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
// 이미 무효 TOML이므로 tomlStringList와 같은 "다루지 않는 형태"로 보낸다 — D92 전환 뒤에는
// tomlScanInline이 그 판정을 내린다: 미종료 값은 닫는 중괄호를 만나지 못해 ok=false이고,
// 계약 4대로 부분 산출 없이 entries가 빈 채로 돌아온다.
func TestCodexUnterminatedInlineMarkerNoPanic(t *testing.T) {
	cfg := []byte("[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { " + codexMarkerKey + " = \"\n")
	// 세 진입점이 모두 codexInlineMarker → tomlScanInline을 탄다 — 하나라도 패닉하면 이 테스트가 죽는다.
	res := installCodexConfigBlock(cfg, codexInstallRequest{
		Profiles: defaultMCPProfiles, SetProfile: true, Marker: hookMarker("0.15.0"),
	})
	if res.Out == nil {
		t.Error("install 산출이 nil")
	}
	uninstallCodexConfigBlock(cfg)
	probeCodexMCPBlock(cfg)
	// 이 형태의 판독 계약은 D92 전환에서 좁아졌다: 종전에는 키를 텍스트로 찾아 ("", true)
	// (키는 있다)였고, 지금은 열거가 ok=false로 빠져 ("", false)(키가 없다)다 — 구조가
	// 확정되지 않은 입력에서 부분 산출을 내지 않는다는 계약 4의 귀결이다. 어느 쪽이든
	// **요점은 같고 이 단정이 재는 것도 그것뿐이다**: 그 값이 소유로 읽히지 않는다.
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
	out, changed, _ := uninstallCodexConfigBlock(cfg)
	if changed {
		t.Errorf("uninstall이 미소유 테이블을 건드렸다:\n%s", out)
	}
	if !bytes.Contains(out, []byte("[mcp_servers.ctr]")) {
		t.Errorf("uninstall이 미소유 테이블을 지웠다:\n%s", out)
	}
}

// TestCodexEscapedManagedKey — D87(§2-6). 관리 키의 유니코드 이스케이프 표기는 TOML이 우리
// 키와 **같은 키**로 읽으므로, 알아보지 못한 채 정규 키를 새로 기입하면 같은 논리 키가 두 번
// 정의된다. 무변경으로 닫는다. 대상은 **키 토큰**뿐이다 — 값의 역슬래시(Windows 경로)가
// 걸리면 그 사용자의 install이 영구 동결된다.
func TestCodexEscapedManagedKey(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want codexAnomaly
	}{
		{
			name: "서브테이블 키의 이스케이프 표기 — 이상",
			src:  "[mcp_servers.ctr]\n\"comm\\u0061nd\" = \"other\"\n",
			want: anomalyEscapedKey,
		},
		{
			name: "env 서브테이블 표식 키의 이스케이프 표기 — 이상",
			src:  "[mcp_servers.ctr]\ncommand = \"context-router\"\n[mcp_servers.ctr.env]\n\"CTR_MAN\\u0041GED\" = \"context-router\"\n",
			want: anomalyEscapedKey,
		},
		{
			name: "인라인 env 안의 표식 키 이스케이프 표기 — 이상",
			src:  "[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { \"CTR_MAN\\u0041GED\" = \"context-router\" }\n",
			want: anomalyEscapedKey,
		},
		{
			name: "값의 역슬래시(Windows 경로) — 이상 아님",
			src:  "[mcp_servers.ctr]\ncommand = \"C:\\\\bin\\\\context-router.exe\"\n",
			want: anomalyNone,
		},
		{
			name: "인라인 env 값의 역슬래시 — 이상 아님",
			src:  "[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { CTR_STORE_ROOT = \"C:\\\\ctr\\\\store\" }\n",
			want: anomalyNone,
		},
		// tomlKeyTokenHasEscape의 **닫는 따옴표 뒤 '=' 확인** 전용 감시선(태스크 검증에서 추가).
		// 나머지 픽스처는 그 확인 없이도 엔트리 첫 토큰·env 한정 둘 중 하나에 걸려 통과하므로
		// 그 절만 지웠을 때 아무도 물지 않았다. env 인라인의 **리터럴 값** 안에 따옴표로 감싼
		// 역슬래시 토큰이 있고 그 뒤가 '='가 아닌 형태가 그 절이 유일한 감시자다.
		{
			name: "인라인 env 리터럴 값 안의 따옴표 경로 — 이상 아님",
			src:  "[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { FLAGS = '--a, \"C:\\t\" z' }\n",
			want: anomalyNone,
		},
		// 아래 셋은 **env 엔트리의 후행 주석** 축이다(리뷰 F1). 인라인 키 토큰 검사가 주석까지
		// 훑으면 정상 파일이 이상으로 판정돼 그 사용자의 install·uninstall·--fix가 영구 무변경으로
		// 굳는다 — 주석은 TOML 데이터가 아니다. 위 "후행 주석 안이 키 모양" 픽스처는 그 엔트리의
		// 키가 x라 env 한정에 걸려 인라인 루프가 아예 돌지 않으므로 이 축을 재지 못한다.
		{
			name: "env 엔트리의 후행 주석 안이 키 모양 — 이상 아님",
			src:  "[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { A = \"1\" } # TODO: , \"C:\\t\" = 2\n",
			want: anomalyNone,
		},
		{
			name: "표식 키 이스케이프 + 후행 주석 — 주석 절단이 판정을 죽이지 않는다",
			src:  "[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { \"CTR_MAN\\u0041GED\" = \"x\" } # note\n",
			want: anomalyEscapedKey,
		},
		{
			// 절단은 **문자열 밖** '#'에서만 한다 — 홑따옴표 리터럴 안의 '#'은 주석이 아니므로
			// 거기서 자르면 그 뒤의 진짜 이스케이프 표식 키를 놓친다(미탐 축).
			name: "홑따옴표 값 안의 # 뒤 표식 키 이스케이프 — 이상",
			src:  "[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { A = 'x#y', \"CTR_MAN\\u0041GED\" = \"z\" }\n",
			want: anomalyEscapedKey,
		},
		{
			name: "홑따옴표 키 — 이스케이프가 없으므로 이상 아님",
			src:  "[mcp_servers.ctr]\n'command' = \"context-router\"\n",
			want: anomalyNone,
		},
		{
			name: "우리 구간 밖의 이스케이프 키 — 이상 아님",
			src:  "[other]\n\"comm\\u0061nd\" = \"x\"\n[mcp_servers.ctr]\ncommand = \"context-router\"\n",
			want: anomalyNone,
		},
		// 아래 넷은 오탐 감시선이다. 판정이 라인을 문맥 없이 훑거나 닫는 따옴표 뒤 '='를
		// 확인하지 않으면 이 파일들이 이상으로 판정되어 install·uninstall·--fix가 영구
		// 무변경으로 굳는다. 셋은 `,` 직후에 키 모양을 두었다 — 그 위치가 초안의 인라인
		// 루프가 보는 자리이므로 그렇게 두어야 감시선이 실제로 물린다.
		{
			name: "배열 원소 값의 역슬래시 — 이상 아님",
			src:  "[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = [\"--store-root\", \"C:\\\\ctr\\\\store\"]\n",
			want: anomalyNone,
		},
		{
			name: "여러 줄 문자열 내용이 키 모양 — 이상 아님",
			src:  "[mcp_servers.ctr]\ncommand = \"context-router\"\nnote = \"\"\"\n\"comm\\u0061nd\" = \"x\"\n\"\"\"\n",
			want: anomalyNone,
		},
		{
			name: "후행 주석 안이 키 모양 — 이상 아님",
			src:  "[mcp_servers.ctr]\ncommand = \"context-router\"\nx = 1 # , \"comm\\u0061nd\" = 2\n",
			want: anomalyNone,
		},
		{
			name: "홑따옴표 값 내부가 키 모양 — 이상 아님",
			src:  "[mcp_servers.ctr]\ncommand = \"context-router\"\nk = ', \"comm\\u0061nd\" = 1'\n",
			want: anomalyNone,
		},
		// 오탐 감시선이 아니라 **우선순위 절**의 감시선이다(태스크 검증에서 추가): 중복 헤더와
		// 이스케이프 키를 한 입력에 담아, 앞 사유가 이미 잡혔으면 D87 검사가 그것을 덮지 않는지
		// 본다. 이스케이프 키는 **마지막** 관리 테이블 구간 안에 두어야 한다 — codexManagedSpans가
		// 뒤 헤더로 구간을 갈아치우므로 앞 구간에 두면 검사가 아예 닿지 않아 절을 재지 못한다.
		{
			name: "중복 헤더와 이스케이프 키가 함께 — 앞 사유를 유지한다",
			src:  "[mcp_servers.ctr]\nx = 1\n[other]\n[mcp_servers.ctr]\n\"comm\\u0061nd\" = \"y\"\n",
			want: anomalyDupHeader,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sp := codexManagedSpans(splitLinesKeepEnds([]byte(c.src)))
			if sp.anomaly != c.want {
				t.Errorf("anomaly=%d want %d", sp.anomaly, c.want)
			}
			// 이상이면 네 경로가 모두 무변경이어야 한다.
			if c.want == anomalyNone {
				return
			}
			res := installCodexConfigBlock([]byte(c.src), codexInstallRequest{Marker: hookMarker("0.16.0")})
			if res.State != mcpMarkerAnomaly || res.Changed || !bytes.Equal(res.Out, []byte(c.src)) {
				t.Errorf("install이 무변경이 아니다: state=%d changed=%v", res.State, res.Changed)
			}
			// uninstall도 사유를 **그대로** 넘긴다 — 무변경만으로는 호출자가 "관리 테이블이
			// 남았다"를 알릴 수 없어 제거 성공 문면만 보인다. 기대값은 probe와 같이 c.want다.
			if out, changed, anomaly := uninstallCodexConfigBlock([]byte(c.src)); changed || !bytes.Equal(out, []byte(c.src)) || anomaly != c.want {
				t.Errorf("uninstall이 무변경·사유 전파가 아니다: changed=%v anomaly=%d want %d", changed, anomaly, c.want)
			}
			// probe는 구간 사유를 **그대로** 넘긴다(D85) — 기대값을 c.want로 쓴다. 여기에 특정
			// 사유를 하드코딩하면 "앞 사유를 유지한다" 케이스(중복 헤더)가 물지 않는다.
			if present, anomaly := probeCodexMCPBlock([]byte(c.src)); present || anomaly != c.want {
				t.Errorf("probe present=%v anomaly=%d want false/%d", present, anomaly, c.want)
			}
			if _, _, found := codexConfigMarker([]byte(c.src)); found {
				t.Errorf("codexConfigMarker found=true — 이상 파일에서 판독이 성립했다")
			}
		})
	}
}

// TestCodexAnomalyReason — 사유마다 서로 다른 문면을 준다(§2-7). 같은 문면이면 사용자는
// install이 영구 무변경인 이유를 구별할 수 없다.
// **사유를 더하면 이 목록에도 더한다.** reason()은 default에서 ""를 돌려주므로 case를 빠뜨리면
// 사용자에게 빈 문면이 나가는데, 목록이 뒤처지면 그 무성 실패를 잡는 단정이 하나도 없다.
// anomalyNone은 의도적으로 빈 문자열이라 이 목록의 대상이 아니다 — 아래에서 따로 단정한다.
func TestCodexAnomalyReason(t *testing.T) {
	seen := map[string]codexAnomaly{}
	for _, a := range []codexAnomaly{
		anomalyDupHeader, anomalyScannerOpen, anomalyEscapedKey,
		anomalyOutsideConflict, anomalyOutputInvalid, anomalyDottedEnv,
		anomalyEnvNotTable, anomalyNotOwned,
	} {
		r := a.reason()
		if r == "" {
			t.Errorf("anomaly=%d의 사유 문면이 비었다", a)
		}
		if prev, dup := seen[r]; dup {
			t.Errorf("anomaly=%d와 %d의 사유 문면이 같다: %q", prev, a, r)
		}
		seen[r] = a
	}
	if anomalyNone.reason() != "" {
		t.Errorf("anomalyNone의 사유 문면이 비어 있지 않다: %q", anomalyNone.reason())
	}
}

// TestCodexInstallAnomalyChannel — D89 사유 전달 채널. install이 세운 사유는 probe가 세운
// 사유보다 **우선한다**. 우선순위가 없으면 install만 아는 이탈 사유가 probe의 anomalyNone에
// 덮여 빈 문자열로 인쇄된다.
func TestCodexInstallAnomalyChannel(t *testing.T) {
	// 구간 판정 이상이 있는 입력 — install이 sp.anomaly를 그대로 실어 낸다.
	dup := "[mcp_servers.ctr]\ncommand = \"context-router\"\n\n[mcp_servers.ctr]\ncommand = \"x\"\n"
	res := installCodexConfigBlock([]byte(dup), codexInstallRequest{Marker: hookMarker("0.17.0")})
	if res.State != mcpMarkerAnomaly {
		t.Fatalf("state=%d want mcpMarkerAnomaly", res.State)
	}
	if res.Anomaly != anomalyDupHeader {
		t.Errorf("Anomaly=%d want anomalyDupHeader — 결과가 사유를 싣지 않으면 채널이 없다", res.Anomaly)
	}
	// 정상 입력에서는 사유가 없다.
	ok := "[mcp_servers.ctr]\ncommand = \"context-router\"\n"
	if r := installCodexConfigBlock([]byte(ok), codexInstallRequest{Marker: hookMarker("0.17.0")}); r.Anomaly != anomalyNone {
		t.Errorf("정상 입력에 Anomaly=%d", r.Anomaly)
	}
	// 폴백 갈래 — install이 사유를 세우지 않는 구간 밖 충돌에서는 probe의 사유가 그대로 남아야
	// 한다. 우선순위를 "결과만 본다"로 접으면 D85가 세운 사유 하나(구간 밖 충돌)가 판정값에서
	// 사라지고, 그 사유는 install 쪽에 대응하는 값이 없어 어디서도 복원되지 않는다.
	conflict := "mcp_servers = { ctr = { command = \"other\" } }\n" + ctrTableFixture
	if v := codexRegistrationVerdict([]byte(conflict), "0.17.0"); v.State != mcpConflict || v.Anomaly != anomalyOutsideConflict {
		t.Errorf("State=%d Anomaly=%d want mcpConflict/anomalyOutsideConflict — 폴백 갈래가 막혔다", v.State, v.Anomaly)
	}
}

// codexGateFixture — 게이트가 실제로 물리는 **정본 형태**. 이스케이프를 담은 헤더 이름은
// TOML이 mcp_servers.ctr로 읽지만 우리 스캐너는 리터럴로 비교해 우리 구간으로 잡지 못하고,
// ctrKeySignal의 리터럴 여섯에도 걸리지 않아 충돌로도 서지 않는다. 그래서 install이
// [mcp_servers.ctr]를 append하고 같은 논리 테이블이 두 번 정의된다.
// **입력 자체는 유효 TOML이다**(r는 TOML 1.0의 유효 이스케이프) — 게이트의 비대칭
// 계약이 성립하는 형태다. 관리 테이블이 잡히지 않으므로 append 경로로 나간다.
const codexGateFixture = "[mcp_servers.\"ct\\u0072\"]\ncommand = \"other\"\n"

// TestCodexInstallOutputInvalidGate — D89. 입력은 파스되는데 산출물이 파스되지 않으면
// 무변경으로 이탈하고 새 상태·고정 사유를 낸다.
func TestCodexInstallOutputInvalidGate(t *testing.T) {
	if !codexTOMLParses([]byte(codexGateFixture)) {
		t.Fatalf("픽스처 입력이 파스되지 않는다 — 비대칭 계약을 잴 수 없다")
	}
	res := installCodexConfigBlock([]byte(codexGateFixture), codexInstallRequest{Marker: hookMarker("0.17.0")})
	if res.State != mcpOutputInvalid {
		t.Fatalf("state=%d want mcpOutputInvalid", res.State)
	}
	// 스펙 §3 표4 — 그 상태의 결과 필드값
	if string(res.Out) != codexGateFixture {
		t.Errorf("산출이 입력과 다르다 — 무변경 이탈이어야 한다:\n%s", res.Out)
	}
	if res.Changed {
		t.Errorf("Changed=true — 쓰기·백업을 함께 생략해야 한다")
	}
	if res.TableFound {
		t.Errorf("TableFound=true — 이 입력에는 우리 구간이 없다")
	}
	if res.Profiles != nil || res.ArgsKept || res.ExecExposed {
		t.Errorf("기입하지 않았는데 기입 결과 필드가 찼다: profiles=%v argsKept=%v exec=%v", res.Profiles, res.ArgsKept, res.ExecExposed)
	}
	if res.Anomaly != anomalyOutputInvalid {
		t.Errorf("Anomaly=%d want anomalyOutputInvalid", res.Anomaly)
	}
}

// TestCodexInstallGateAsymmetry — 게이트의 **비대칭 절**. 입력이 이미 무효면 게이트는
// 작동하지 않는다 — 산출물의 무효는 우리가 들인 것이 아니므로 되돌려도 이득이 없다.
// (`bytes.Equal` 단축은 파스 횟수 전용이라 반환값으로 관측할 수 없다 — 단정 대상이 아니다.)
func TestCodexInstallGateAsymmetry(t *testing.T) {
	// 입력이 이미 무효(같은 테이블 안 키 두 번)이지만 우리 구간이 없어 append 갈래로 나간다.
	src := "[other]\nx = 1\nx = 2\n"
	if codexTOMLParses([]byte(src)) {
		t.Fatalf("픽스처 입력이 파스된다 — 비대칭 절을 잴 수 없다")
	}
	res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: hookMarker("0.17.0")})
	if res.State != mcpWritten || !res.Changed {
		t.Errorf("state=%d changed=%v — 무효 입력에서 게이트가 작동했다", res.State, res.Changed)
	}
}

// TestCodexInstallMarkerOnly — D86(§2-3·§2-4·§2-5·§2-8). 표식 전용 갈래는 args·enabled_tools를
// 원문으로 보존하고 표식과 command만 맞춘다. **args는 우리 형식이어야 한다** — 우리 형식이
// 아니면 profilesFromArgs가 ok=false를 내 D81의 되읽기 실패 경로가 이미 두 값을 보존하므로,
// 그 픽스처로는 이 갈래를 되돌려도 통과한다(물지 않는 검사).
func TestCodexInstallMarkerOnly(t *testing.T) {
	// 우리 형식 args + 사용자가 손으로 넓힌 enabled_tools(프로필이 켜지 않는 ctr_execute를 더했다)
	src := "[mcp_servers.ctr]\n" +
		"command = \"context-router\"\n" +
		"args = [\"--enable\", \"ingest\"]\n" +
		"enabled_tools = [\"ctr_search\", \"ctr_index\", \"ctr_execute\"]\n" +
		"[mcp_servers.ctr.env]\n" +
		codexMarkerKey + " = \"context-router/0.15.0\"\n"

	res := installCodexConfigBlock([]byte(src), codexInstallRequest{
		Marker: hookMarker("0.16.0"), MarkerOnly: true,
	})
	if res.State != mcpWritten || !res.Changed {
		t.Fatalf("state=%d changed=%v want mcpWritten/true", res.State, res.Changed)
	}
	out := string(res.Out)
	// 사용자가 넓힌 목록이 바이트 그대로 남는다
	if !strings.Contains(out, "enabled_tools = [\"ctr_search\", \"ctr_index\", \"ctr_execute\"]") {
		t.Errorf("enabled_tools가 보존되지 않았다:\n%s", out)
	}
	if !strings.Contains(out, "args = [\"--enable\", \"ingest\"]") {
		t.Errorf("args가 보존되지 않았다:\n%s", out)
	}
	// 표식은 현재 버전이 된다
	if !strings.Contains(out, codexMarkerKey+" = \""+hookMarker("0.16.0")+"\"") {
		t.Errorf("표식이 현재 버전이 아니다:\n%s", out)
	}
	// 이 갈래는 프로필을 기입하지 않으므로 "실제로 기입한 프로필"이 없다
	if res.Profiles != nil {
		t.Errorf("MarkerOnly인데 Profiles=%v", res.Profiles)
	}
	// ArgsKept는 되읽기 실패 전용이다 — 이 픽스처는 되읽기에 성공한다
	if res.ArgsKept {
		t.Errorf("ArgsKept=true — 되읽기에 성공한 픽스처인데 의미가 오염됐다")
	}
	// 산출물이 실제로 노출하는 것을 답해야 한다(보존된 목록에 exec가 있다)
	if !res.ExecExposed {
		t.Errorf("ExecExposed=false — 보존된 enabled_tools에 ctr_execute가 있다")
	}

	// 멱등: 산출물에 같은 요청을 다시 걸면 무변경이다(D84 단일 백업 슬롯의 전제)
	res2 := installCodexConfigBlock(res.Out, codexInstallRequest{
		Marker: hookMarker("0.16.0"), MarkerOnly: true,
	})
	if res2.Changed {
		t.Errorf("2회차가 무변경이 아니다:\n%s", res2.Out)
	}

	// **호스트 재직렬화 형태에서의 무변경** — 이 단정이 ownedSame의 keepArgs 절을 재는 것이다.
	// 위 멱등 단정만으로는 부족하다: 산출물은 우리 형식이라 바이트 비교로 이미 무변경이 나오므로
	// keepArgs 절을 되돌려도 통과한다(물지 않는 검사). 대입 공백이 우리 형식과 다른 파일은
	// 바이트 비교가 성립하지 않아 키 단위 동치가 판정해야 하고, 보존하는 값을 비교하면
	// enabled_tools가 프로필 조립값과 달라 매 실행 재기입된다(D84 단일 슬롯 무의미화).
	reser := "[mcp_servers.ctr]\n" +
		"command=\"context-router\"\n" +
		"args=[\"--enable\",\"ingest\"]\n" +
		"enabled_tools=[\"ctr_search\",\"ctr_index\",\"ctr_execute\"]\n" +
		"[mcp_servers.ctr.env]\n" +
		codexMarkerKey + "=\"" + hookMarker("0.16.0") + "\"\n"
	if r := installCodexConfigBlock([]byte(reser), codexInstallRequest{
		Marker: hookMarker("0.16.0"), MarkerOnly: true,
	}); r.Changed {
		t.Errorf("재직렬화 형태에서 무변경이 아니다 — ownedSame이 keepArgs를 보지 않는다:\n%s", r.Out)
	}

	// install(MarkerOnly 없음)은 종전대로 프로필에서 재조립한다 — D81 불변(§2-8)
	res3 := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: hookMarker("0.16.0")})
	if strings.Contains(string(res3.Out), "ctr_execute") {
		t.Errorf("install이 사용자 확장을 보존했다 — D81 재조립이 깨졌다:\n%s", res3.Out)
	}
}

// TestCodexInstallMarkerOnlyRewritesCommand — D86(§2-4). 표식은 우리 것이고 command가
// 절대경로인 등록물에서 command는 우리 값으로 맞춰지고 args·enabled_tools는 보존된다.
// "표식만 갱신한다"가 아니라 "표식과 command를 맞춘다"가 정확한 계약이다.
func TestCodexInstallMarkerOnlyRewritesCommand(t *testing.T) {
	src := "[mcp_servers.ctr]\n" +
		"command = \"C:\\\\bin\\\\context-router.exe\"\n" +
		"args = [\"--enable\", \"ingest,net\"]\n" +
		"enabled_tools = [\"ctr_search\"]\n" +
		"[mcp_servers.ctr.env]\n" +
		codexMarkerKey + " = \"context-router/0.15.0\"\n"

	res := installCodexConfigBlock([]byte(src), codexInstallRequest{
		Marker: hookMarker("0.16.0"), MarkerOnly: true,
	})
	if res.State != mcpWritten || !res.Changed {
		t.Fatalf("state=%d changed=%v", res.State, res.Changed)
	}
	out := string(res.Out)
	if !strings.Contains(out, "command = \"context-router\"") {
		t.Errorf("command가 우리 값으로 맞춰지지 않았다:\n%s", out)
	}
	if !strings.Contains(out, "enabled_tools = [\"ctr_search\"]") {
		t.Errorf("enabled_tools가 보존되지 않았다:\n%s", out)
	}
}

// TestCodexInlineMarkerNonStringValue — 인라인 env의 표식 값이 문자열이 아니면 그 **값 토큰만**
// 관리 표식 문자열로 갈아 끼운다(삽입이 아니라 치환이라 중복 키가 생기지 않는다). 종전에는
// 원문을 그대로 두어 표식이 영영 현재 값이 되지 못했고, 그 상태가 doctor에서 "경고 없는
// 표식없음 + 무변경 보고"라는 서로 모순된 두 줄로 새어 나왔다. **값이 물리 라인 둘에 걸쳐도
// 같다**(재기준선 행 6) — 되쓰기가 논리 엔트리를 받고 spliceInlineSpan이 파일 좌표로 치환하므로
// 값 구간이 줄을 넘어도 제자리다. 종전에는 그 형태에서 값 토큰의 끝을 그 줄에서 찾지 못해
// 포기했다. 무변경으로 남는 것은 **구조가 확정되지 않는 형태**뿐이고 마지막 케이스가 그 경계다.
func TestCodexInlineMarkerNonStringValue(t *testing.T) {
	const head = "[mcp_servers.ctr]\ncommand = \"context-router\"\n"
	marker := hookMarker("0.16.0")
	cases := []struct {
		name, src, want string
		wantState       codexMCPState
	}{
		{
			"정수 값 — 표식 문자열로 교체",
			head + "env = { " + codexMarkerKey + " = 0 }\n",
			head + "env = { " + codexMarkerKey + " = \"" + marker + "\" }\n",
			mcpWritten,
		},
		{
			"뒤 키는 원문 그대로",
			head + "env = { " + codexMarkerKey + " = 0, PATH = \"/x\" }\n",
			head + "env = { " + codexMarkerKey + " = \"" + marker + "\", PATH = \"/x\" }\n",
			mcpWritten,
		},
		{
			// 값 안의 쉼표·중괄호는 종결자가 아니다 — 부분 문자열로 끝을 찾으면 여기서 사용자
			// 인라인 테이블이 깨진다.
			"홑따옴표 값 안의 쉼표·중괄호",
			head + "env = { " + codexMarkerKey + " = 'a,b}', X = 1 }\n",
			head + "env = { " + codexMarkerKey + " = \"" + marker + "\", X = 1 }\n",
			mcpWritten,
		},
		{
			// 재기준선 행 6. 값 구간이 물리 라인 둘에 걸쳐도 제자리에서 갈아 끼운다 — 되쓰기가
			// 논리 엔트리를 받고 치환 지점이 파일 좌표(줄, 열)이기 때문이다. 값 구간이 사라지면서
			// 두 물리 라인이 한 줄로 접히는 것은 치환의 정의다(구간 안쪽 줄은 통째로 사라진다).
			"여러 줄에 걸친 값 — 제자리에서 표식 문자열로 교체",
			head + "env = { " + codexMarkerKey + " = [1,\n2] }\n",
			head + "env = { " + codexMarkerKey + " = \"" + marker + "\" }\n",
			mcpWritten,
		},
		{
			// 후행 주석 안의 쉼표는 값 토큰의 종결자가 아니다 — codexEntryRaw가 주석을 먼저
			// 잘라 내므로 열거형이 그 쉼표를 보지 못한다. 잘리지 않으면 값 구간이 주석 안까지
			// 늘어나 주석 절반이 표식 문자열로 갈린다.
			// 닫는 중괄호가 주석에 먹혀 이미 무효 TOML인 줄이므로 무변경이 계약이다. 여는
			// 중괄호가 EOF까지 닫히지 않으므로(T2) 스캐너가 열린 채 끝나 anomalyScannerOpen이다.
			"주석 안의 쉼표 — 종결자가 아니다(무변경)",
			head + "env = { " + codexMarkerKey + " = 0 # a, b\n",
			head + "env = { " + codexMarkerKey + " = 0 # a, b\n",
			mcpMarkerAnomaly,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := installCodexConfigBlock([]byte(c.src), codexInstallRequest{Marker: marker, MarkerOnly: true})
			if res.State != c.wantState {
				t.Fatalf("state=%d want %d", res.State, c.wantState)
			}
			if string(res.Out) != c.want {
				t.Fatalf("got=%q\nwant=%q", res.Out, c.want)
			}
			if res.Changed != (c.src != c.want) {
				t.Errorf("Changed=%v — 산출이 원문과 다른가와 어긋난다", res.Changed)
			}
			// 멱등 — 교체한 뒤 재실행이 또 바꾸면 D84 단일 백업 슬롯이 2회차에 원본을 잃는다.
			if again := installCodexConfigBlock(res.Out, codexInstallRequest{Marker: marker, MarkerOnly: true}); again.Changed {
				t.Errorf("2회차가 무변경이 아니다:\n%s", again.Out)
			}
		})
	}
}

// TestCodexInlineMarkerInsideStringValue — 인라인 env의 **다른 키의 문자열 값 안**에 표식 키와
// 같은 모양이 들어 있어도 그 자리는 **값이지 키가 아니다**(재기준선 행 7). 그러므로 표식은
// 부재이고 여는 중괄호 뒤에 삽입되며, 사용자 값 바이트는 하나도 바뀌지 않는다.
//
// 종전의 무변경은 안전이 아니라 **두 판독기가 같은 바이트를 다르게 본 결과**였다. 판독
// (tomlInlineValue)은 문자열 토큰을 건너뛰지 않아 값 안의 `, CTR_MANAGED = …`를 키가 있는
// 것으로 읽고, 되쓰기(inlineMarkerSpan)는 건너뛰므로 그 자리를 찾지 못해 포기했다 — 그래서
// 이런 파일에는 표식이 **영영** 서지 않았고 doctor는 계속 표식없음을 냈다. 열거형은 문자열을
// 값으로 통째 잡으므로 두 자리가 함께 "부재"로 옳아진다.
//
// 값 한가운데를 치환하는 오답도 여전히 배제한다: 큰따옴표 값 안이면 넣은 따옴표가 값을
// 조기에 닫아 파스가 깨지고, 홑따옴표 값 안이면 파스는 되지만 사용자 환경변수가 조용히
// 바뀐다 — 사용자 바이트 보존과 파스 단정이 그 둘을 함께 잡는다.
func TestCodexInlineMarkerInsideStringValue(t *testing.T) {
	const head = "[mcp_servers.ctr]\ncommand = \"context-router\"\n"
	marker := hookMarker("0.16.0")
	// keep — 사용자가 쓴 바이트 그대로. 산출에 이 부분 문자열이 남아야 값 한가운데가 갈리지
	// 않은 것이다.
	cases := []struct{ name, src, keep string }{
		{
			"큰따옴표 값 안 — 값이지 키가 아니다",
			head + "env = { A = \"x, " + codexMarkerKey + " = 0, y\", B = 1 }\n",
			"A = \"x, " + codexMarkerKey + " = 0, y\", B = 1 }",
		},
		{
			"홑따옴표 값 안 — 값이지 키가 아니다",
			head + "env = { A = 'x, " + codexMarkerKey + " = \"0\", y', B = 1 }\n",
			"A = 'x, " + codexMarkerKey + " = \"0\", y', B = 1 }",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := installCodexConfigBlock([]byte(c.src), codexInstallRequest{Marker: marker, MarkerOnly: true})
			if res.State != mcpWritten {
				t.Fatalf("state=%d want mcpWritten", res.State)
			}
			if !strings.Contains(string(res.Out), c.keep) {
				t.Fatalf("사용자 값 바이트가 갈렸다:\ngot =%q\nkeep=%q", res.Out, c.keep)
			}
			if !strings.Contains(string(res.Out), codexMarkerKey+` = "`+marker+`"`) {
				t.Fatalf("표식이 삽입되지 않았다:\n%s", res.Out)
			}
			// 파스가 중복 키도 함께 잡는다 — 값 안의 모양을 키로 세어 한 번 더 넣었다면
			// 같은 인라인 테이블에 CTR_MANAGED가 둘이 되어 지정 파서가 거부한다.
			if !codexTOMLParses(res.Out) {
				t.Fatalf("산출이 파스되지 않는다:\n%s", res.Out)
			}
			// 멱등 — 2회차가 또 바꾸면 D84 단일 백업 슬롯이 2회차에 원본을 잃는다.
			again := installCodexConfigBlock(res.Out, codexInstallRequest{Marker: marker, MarkerOnly: true})
			if again.Changed || !bytes.Equal(again.Out, res.Out) {
				t.Errorf("2회차가 무변경이 아니다: changed=%v\n%s", again.Changed, again.Out)
			}
		})
	}
}

// TestSpliceInlineSpanRejectsOutOfRangeSpan — 구간이 **어느 차원으로든** 범위 밖이면 갈아
// 끼우지 않고 논리 엔트리 원문을 그대로 돌려준다. 줄만 검사하던 종전 가드에서 세 케이스가
// 각각 이렇게 깨졌다(실측): 열이 줄 길이를 넘으면 slice bounds 패닉이고 — internal/cli에
// recover가 없으므로 사용자 config를 쓰는 도중의 프로세스 종료다 — 같은 줄에서 시작이 끝보다
// 뒤면 패닉 없이 바이트를 복제해 조용히 파일을 깨뜨린다.
// 오늘의 두 생산자는 이런 좌표를 내지 않는다. 이 테스트가 재는 것은 **원시 자체의 전역성**이며,
// 소비자가 더 붙을 때 그 불변식이 깨지는 것을 여기서 잡는다.
func TestSpliceInlineSpanRejectsOutOfRangeSpan(t *testing.T) {
	lines := splitLinesKeepEnds([]byte("[mcp_servers.ctr]\nenv = { A = \"1\" }\n"))
	e := [2]int{1, 1}
	want := string(lines[1])
	pt := func(l, c int) tomlPoint { return tomlPoint{line: l, col: c} }
	for _, c := range []struct {
		name string
		sp   tomlSpan
	}{
		{"두 열 모두 줄 길이를 넘음", tomlSpan{start: pt(1, 999), end: pt(1, 999)}},
		{"끝 열만 줄 길이를 넘음", tomlSpan{start: pt(1, 8), end: pt(1, 999)}},
		{"같은 줄에서 시작이 끝보다 뒤", tomlSpan{start: pt(1, 12), end: pt(1, 8)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := string(spliceInlineSpan(lines, e, c.sp, "REPL")); got != want {
				t.Errorf("원문이 보존되지 않았다:\ngot =%q\nwant=%q", got, want)
			}
		})
	}
}

// TestCodexNestedInlineNotOverwritten — 스펙 §0 관측 ①. 중첩 인라인 안쪽의 CTR_MANAGED는
// 우리 표식이 아니다. 종전에는 그것을 바깥 표식으로 읽어 **사용자 값을 조용히 덮어썼고**
// 산출이 유효 TOML이라 게이트도 통과했다.
func TestCodexNestedInlineNotOverwritten(t *testing.T) {
	src := "[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { A = { CTR_MANAGED = \"inner\" }, B = \"1\" }\n"
	res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: hookMarker("0.18.0")})
	if !strings.Contains(string(res.Out), `"inner"`) {
		t.Errorf("사용자 값 inner가 사라졌다:\n%s", res.Out)
	}
	if !codexTOMLParses(res.Out) {
		t.Errorf("산출이 파스되지 않는다:\n%s", res.Out)
	}
	// 멱등(스펙 §2.1 P4)
	again := installCodexConfigBlock(res.Out, codexInstallRequest{Marker: hookMarker("0.18.0")})
	if !bytes.Equal(again.Out, res.Out) {
		t.Errorf("멱등이 아니다:\n1: %s\n2: %s", res.Out, again.Out)
	}
}

// TestCodexDottedInlineKeyExits — 스펙 §0 관측 ②. 인라인 테이블 안의 점 표기 키는 중첩이
// 아니라 깊이 1 마디이므로 깊이 1 한정이 걸러 내지 못한다. 첫 마디가 표식 키면 삽입이 중복
// 정의를 만들고, 표식으로 읽히지도 않아 갱신 갈래도 설 수 없다 — 무변경 + 수행 가능한 사유.
func TestCodexDottedInlineKeyExits(t *testing.T) {
	src := "[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { CTR_MANAGED.sub = \"x\" }\n"
	res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: hookMarker("0.18.0")})
	if res.State != mcpMarkerAnomaly {
		t.Fatalf("state=%d want mcpMarkerAnomaly", res.State)
	}
	if res.Anomaly != anomalyDottedEnv {
		t.Errorf("anomaly=%d want anomalyDottedEnv", res.Anomaly)
	}
	if string(res.Out) != src {
		t.Errorf("산출이 입력과 다르다 — 무변경 이탈이어야 한다:\n%s", res.Out)
	}
	if res.Changed {
		t.Errorf("Changed가 참이다")
	}
}

// TestCodexEnvNotInlineTable — env 우변이 인라인 테이블이 아니면 헤더도 표식도 없이 무변경으로
// 굳고 게이트도 잡지 않아, 사용자는 install이 왜 아무것도 하지 않는지 알 수 없었다.
func TestCodexEnvNotInlineTable(t *testing.T) {
	for _, rhs := range []string{`[]`, `"x"`, `1`} {
		src := "[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = " + rhs + "\n"
		res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: hookMarker("0.18.0")})
		if res.State != mcpMarkerAnomaly || res.Anomaly != anomalyEnvNotTable {
			t.Errorf("env = %s: state=%d anomaly=%d want mcpMarkerAnomaly/anomalyEnvNotTable", rhs, res.State, res.Anomaly)
		}
		if string(res.Out) != src {
			t.Errorf("env = %s: 무변경 이탈이 아니다:\n%s", rhs, res.Out)
		}
	}
}

// TestAnomalyEnvNotTableReasonUnique — 사유 문면은 서로 달라야 한다. 같으면 사용자가
// 무엇을 고쳐야 하는지 갈리지 않는다.
func TestAnomalyEnvNotTableReasonUnique(t *testing.T) {
	r := anomalyEnvNotTable.reason()
	if r == "" {
		t.Fatal("사유 문면이 비어 있다")
	}
	for _, other := range []codexAnomaly{
		anomalyDupHeader, anomalyScannerOpen, anomalyEscapedKey,
		anomalyOutsideConflict, anomalyOutputInvalid, anomalyDottedEnv, anomalyNotOwned,
	} {
		if other.reason() == r {
			t.Errorf("사유가 %d와 같다: %q", other, r)
		}
	}
}

// TestCodexDottedInlineNonMarkerPasses — 표식과 무관한 점 표기 마디는 막지 않는다.
// 무변경 집합을 불필요하게 넓히지 않는 것이 계약이다.
func TestCodexDottedInlineNonMarkerPasses(t *testing.T) {
	src := "[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { A.B = \"1\" }\n"
	res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: hookMarker("0.18.0")})
	if res.State == mcpMarkerAnomaly {
		t.Errorf("표식과 무관한 점 표기를 막았다: anomaly=%d", res.Anomaly)
	}
	if !codexTOMLParses(res.Out) {
		t.Errorf("산출이 파스되지 않는다:\n%s", res.Out)
	}
}

// TestCodexUnquoteChokepointHoldsEverywhere — 키 비교는 tomlUnquoteKey **하나**를 지난다.
// 이 자리는 그 계약이 실제로 지켜지는가를 판정 수준에서 잰다: 같은 릴리스 안에서 두 자리가
// strings.Trim으로 남아 같은 바이트를 다르게 읽었고, 그 둘이 각각 다른 오판을 냈다.
//
//   - `'"CTR_MANAGED"'.x` — TOML이 읽는 키 이름은 따옴표를 **포함한** "CTR_MANAGED"라 우리
//     표식 키가 아니다. codexInlineMarkerBlocked가 Trim으로 벗겨 게이트를 세우면 install이
//     무변경 이상으로 이탈해 MCP가 확정되지 않고 가드 등록이 빠지며, doctor는 점 표기 env가
//     없는 파일에 그 진단을 낸다. **점 없는 형제**는 codexInlineMarker가 이미 하나를 지나
//     남의 키로 통과시킨다 — 두 자리의 답이 갈리지 않아야 한다는 것이 이 짝의 뜻이다.
//   - `'"env"'.CTR_MANAGED` — 마찬가지로 우리 env가 아니다. tomlDottedEnvKey가 Trim으로
//     벗기면 남의 키에 install이 얼어붙고, 그 사용자 값이 소유 판정의 근거가 되어 남의
//     테이블을 되쓰고 지운다.
//   - `env.'"CTR_MANAGED"'` — env는 맞지만 표식 이름이 아니다. 꼬리 마디도 같은 자리를 지나야
//     그 사용자 값이 우리 표식으로 읽히지 않는다.
func TestCodexUnquoteChokepointHoldsEverywhere(t *testing.T) {
	cur := hookMarker("0.18.0")
	const head = "[mcp_servers.ctr]\n"
	ours := head + "command = \"context-router\"\n"
	theirs := head + "command = \"/usr/bin/someone-else\"\n"

	// 점 표기와 그 형제는 같은 답이어야 한다 — 둘 다 남의 키이므로 게이트가 서지 않는다.
	for _, env := range []string{
		"env = { '\"CTR_MANAGED\"'.x = 1, A = \"y\" }\n",
		"env = { '\"CTR_MANAGED\"' = 1, A = \"y\" }\n",
	} {
		src := ours + env
		res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: cur})
		if res.State != mcpWritten {
			t.Errorf("%q: state=%d anomaly=%d want mcpWritten — 남의 키에 게이트가 섰다", env, res.State, res.Anomaly)
		}
		if !strings.Contains(string(res.Out), cur) {
			t.Errorf("%q: 표식이 기입되지 않았다:\n%s", env, res.Out)
		}
		if !codexTOMLParses(res.Out) {
			t.Errorf("%q: 산출이 파스되지 않는다:\n%s", env, res.Out)
		}
	}
	// 점 표기 머리·꼬리 — 남의 키는 소유 근거가 아니다. command도 남의 것이라 표식만이
	// 눈금이며, 되읽기가 그 값을 우리 표식으로 읽으면 install이 되쓰고 uninstall이 지운다.
	for _, env := range []string{
		"'\"env\"'.CTR_MANAGED = \"" + cur + "\"\n",
		"env.'\"CTR_MANAGED\"' = \"" + cur + "\"\n",
	} {
		src := theirs + env
		if m, _, _ := codexConfigMarker([]byte(src)); m != "" {
			t.Errorf("%q: 남의 키를 표식으로 읽었다: %q", env, m)
		}
		if res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: cur}); res.State != mcpExistingHeader {
			t.Errorf("%q: install state=%d want mcpExistingHeader — 남의 테이블이다", env, res.State)
		}
		if out, changed, anomaly := uninstallCodexConfigBlock([]byte(src)); changed || anomaly != anomalyNotOwned {
			t.Errorf("%q: 남의 테이블을 건드렸다: changed=%v anomaly=%d\n%s", env, changed, anomaly, out)
		}
	}
}

// TestCodexUnscannableInlineEnvIsUnknownNotForeign — 인라인 env의 구조를 확정하지 못하면
// 소유는 **모르는** 것이지 "아니다"가 아니다. codexInlineMarker가 그 상태에서 부재를 내므로
// 표식이 문자열 그대로 적힌 파일이 남의 것으로 내려왔다: install은 mcpExistingHeader로 물러나
// 재실행으로도 고칠 수 없고, uninstall은 "사용자 등록이니 직접 정리하라"를 내며 종료코드 0으로
// 끝나 사용자는 제거가 끝난 줄 아는데 Codex는 그 MCP 서버를 계속 띄운다.
//
// **표식을 텍스트로 다시 찾아 고치지 않는다** — 판독기 둘이 같은 바이트를 다르게 읽는 것이
// 이 릴리스가 닫은 결함이다. 모른다는 것을 사유 있는 무변경으로 보고한다.
func TestCodexUnscannableInlineEnvIsUnknownNotForeign(t *testing.T) {
	// 절대경로 command라 인수 절(D80)이 서지 않는다 — 표식만이 소유 근거인 눈금이다.
	src := "[mcp_servers.ctr]\ncommand = \"/opt/bin/context-router\"\n" +
		"env = { CTR_MANAGED = \"context-router/0.17.2\", A = }\n"
	res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: hookMarker("0.18.0")})
	if res.State != mcpMarkerAnomaly || res.Anomaly != anomalyEnvNotTable {
		t.Errorf("install state=%d anomaly=%d want mcpMarkerAnomaly/anomalyEnvNotTable", res.State, res.Anomaly)
	}
	if string(res.Out) != src || res.Changed {
		t.Errorf("무변경 이탈이 아니다: changed=%v\n%s", res.Changed, res.Out)
	}
	out, changed, anomaly := uninstallCodexConfigBlock([]byte(src))
	if changed || anomaly != anomalyEnvNotTable {
		t.Errorf("uninstall changed=%v anomaly=%d want false/anomalyEnvNotTable — anomalyNotOwned는 우리 표식이 적힌 파일에 나갈 문면이 아니다", changed, anomaly)
	}
	if string(out) != src {
		t.Errorf("uninstall이 바이트를 바꿨다:\n%s", out)
	}
	// **남의 테이블은 여전히 남의 테이블이다.** 우변이 인라인 테이블이 아니면 마디가 있을 수
	// 없으므로 표식의 부재는 아는 것이다 — 그 형태까지 이상으로 올리면 남의 등록물에 우리
	// 파일을 고치라는 안내가 나간다.
	for _, rhs := range []string{"[]", "\"x\"", "1"} {
		foreign := "[mcp_servers.ctr]\ncommand = \"/usr/bin/someone-else\"\nenv = " + rhs + "\n"
		if r := installCodexConfigBlock([]byte(foreign), codexInstallRequest{Marker: hookMarker("0.18.0")}); r.State != mcpExistingHeader {
			t.Errorf("env = %s: install state=%d want mcpExistingHeader", rhs, r.State)
		}
		if _, ch, an := uninstallCodexConfigBlock([]byte(foreign)); ch || an != anomalyNotOwned {
			t.Errorf("env = %s: uninstall changed=%v anomaly=%d want false/anomalyNotOwned", rhs, ch, an)
		}
	}
}

// TestCodexMultilineInlineMarkerUpdates — 여러 줄로 이어진 인라인 env에서도 표식이 현재
// 값으로 갱신되고 재실행이 무변경이다. 한 줄 픽스처로는 물리 라인 한정 구현이 통과한다.
func TestCodexMultilineInlineMarkerUpdates(t *testing.T) {
	src := "[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { A = \"a\",\n  CTR_MANAGED = \"context-router/0.15.0\" }\n"
	res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: hookMarker("0.18.0")})
	if !strings.Contains(string(res.Out), hookMarker("0.18.0")) {
		t.Errorf("표식이 갱신되지 않았다:\n%s", res.Out)
	}
	if !strings.Contains(string(res.Out), `"a"`) {
		t.Errorf("사용자 값 a가 사라졌다:\n%s", res.Out)
	}
	again := installCodexConfigBlock(res.Out, codexInstallRequest{Marker: hookMarker("0.18.0")})
	if !bytes.Equal(again.Out, res.Out) {
		t.Errorf("멱등이 아니다:\n1: %s\n2: %s", res.Out, again.Out)
	}
}

// TestCodexInstallOutputAlwaysParses — §1.3 사후 게이트 5. 우리 정상 산출물이 지정 파서를
// 통과해야 한다. 새 파서는 종전 후보보다 엄격하므로 산출물 거부 방향의 회귀가 새로 열리고,
// 그 회귀는 install을 영구 무변경으로 만든다.
// **목록은 손으로 고른 다섯이 아니다** — 이 파일의 표 주도 테스트(TestInstallCodexConfigBlock·
// TestUninstallCodexConfigBlock·TestProbeCodexMCPBlock·TestCodexEscapedManagedKey·
// TestCodexInlineMarker* 계열)와 그 지역 픽스처를 훑어 합쳤다. 같은 갈래를 두 번 재는 행은
// 접었지만 **우리가 사용자 줄 안의 바이트를 되쓰는 행은 접지 않는다** — 게이트가 물 위험이
// 실제로 있는 자리가 거기이고, 파일 수준에서 append만 하는 행들과 달리 산출물의 유효성이
// 보존 바이트에 달려 있다.
func TestCodexInstallOutputAlwaysParses(t *testing.T) {
	srcs := []string{
		// ① 브리프의 기본 다섯
		"",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\n",
		"[other]\nx = 1\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { CTR_MANAGED = \"context-router/0.15.0\" }\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\n\n[mcp_servers.ctr.env]\nCTR_MANAGED = \"context-router/0.15.0\"\nUSER = \"keep\"\n",
		// ② TestInstallCodexConfigBlock의 existing — append 갈래의 파일 수준 형태
		"a = 1\n",
		"a = 1",     // 무개행 EOF
		"a = 1\r\n", // CRLF 지배
		"a = 1\n\n" + ctrTableFixture,
		"[mcp_servers]\nfoo = { command = \"x\" }\n",                          // 이미 정의된 부모 아래 서브테이블
		"[mcp_servers.chrome]\ncommand = \"x\"\n# electron debugging notes\n", // 오탐 회피 대표
		"[mcp_servers.ctr-exec]\ncommand = \"context-router\"\nargs = [\"--enable\", \"exec\"]\nenabled_tools = [\"ctr_execute\", \"ctr_execute_file\"]\ndefault_tools_approval_mode = \"prompt\"\n",
		"[mcp_servers.ctr.env]\nCTR_MANAGED = \"context-router/0.14.0\"\n",                                     // 부모 없는 env — 산출이 순서 역전
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = []\n",                                         // 인수(표식 없는 우리 테이블)
		"[mcp_servers.ctr]\ncommand = \"old\"\n",                                                               // mcpExistingHeader 무변경
		"[mcp_servers.\"ctr\"]\ncommand = \"x\"\n",                                                             // 충돌 — 따옴표 키
		"mcp_servers.ctr = { command = \"x\" }\n",                                                              // 충돌 — 인라인 대입
		"[mcp_servers]\nctr.command = \"x\"\n",                                                                 // 충돌 — 점표기
		"mcp_servers = { foo = { command = \"x\" } }\n",                                                        // 충돌 — 루트 인라인
		"[mcp_servers.ctr]\ncommand = \"context-router\"\n[x]\n[mcp_servers.ctr]\n",                            // 무효 입력 — 중복 헤더
		"[mcp_servers.ctr]\ncommand = \"context-router\"\n[mcp_servers.ctr.env]\n[y]\n[mcp_servers.ctr.env]\n", // 무효 입력 — env 중복
		// ③ TestUninstallCodexConfigBlock의 existing
		"a = 1\n" + ctrTableFixture + "[b]\nx = 1\n",
		"a = 1\n\n\n" + ctrTableFixture,
		"[mcp_servers.ctr]\ncommand = \"other\"\n[mcp_servers.ctr.env]\nCTR_MANAGED = \"context-router/0.14.0\"\n",
		// 구 마커가 우리 관리 테이블을 감싼 **마이그레이션 입력**. 우리가 실제로 내보내는 산출물
		// 중 눈금 테스트가 덮지 않은 유일한 형태다 — 마커 두 줄을 지우고 되쓰는 갈래가 여기서만
		// 산출을 만든다.
		codexBlockBegin + "\n[mcp_servers.ctr]\ncommand = \"context-router\"\n[hooks.state]\ntrust = \"abc\"\n" + codexBlockEnd + "\n",
		// 블록이 **우리 테이블만** 감싼 형태 — END가 우리 구간 안이라 keep에서 빼는 갈래를 탄다.
		codexBlockBegin + "\n[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = []\nenabled_tools = [\"ctr_search\"]\n" + codexBlockEnd + "\n[tui]\ntheme = \"dark\"\n",
		// ④ TestProbeCodexMCPBlock의 in
		ctrTableFixture,
		"[model]\nname = \"gpt\"\n",
		"mcp_servers = { ctr = { command = \"other\" } }\n" + ctrTableFixture, // 구간 밖 충돌
		"[mcp_servers.ctr-exec]\ncommand = \"context-router\"\n",
		"[mcp_servers.ctr]\n[x]\n[mcp_servers.ctr]\nk = \"\"\"\nunclosed\n", // 무효 입력 — 스캐너 열림
		// ⑤ TestCodexEscapedManagedKey의 src — 우리 구간 **안**의 보존 바이트. 그 표는
		// anomalyNone 행에서 install을 부르지 않으므로 이 행들이 산출을 만드는 것은 여기가 처음이다.
		"[mcp_servers.ctr]\ncommand = \"C:\\\\bin\\\\context-router.exe\"\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { CTR_STORE_ROOT = \"C:\\\\ctr\\\\store\" }\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { FLAGS = '--a, \"C:\\t\" z' }\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { A = \"1\" } # TODO: , \"C:\\t\" = 2\n",
		"[mcp_servers.ctr]\n'command' = \"context-router\"\n",
		"[other]\n\"comm\\u0061nd\" = \"x\"\n[mcp_servers.ctr]\ncommand = \"context-router\"\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = [\"--store-root\", \"C:\\\\ctr\\\\store\"]\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nnote = \"\"\"\n\"comm\\u0061nd\" = \"x\"\n\"\"\"\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nx = 1 # , \"comm\\u0061nd\" = 2\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nk = ', \"comm\\u0061nd\" = 1'\n",
		"[mcp_servers.ctr]\n\"comm\\u0061nd\" = \"other\"\n", // 이상 — 무변경
		// ⑥ 인라인 표식 되쓰기 — 우리가 **사용자 줄 안의 바이트**를 치환하는 갈래.
		// TestCodexInlineMarkerNonStringValue·TestCodexInlineMarkerInsideStringValue의 src와
		// TestCodexTableBodyPreservesUnownedKeys의 지역 픽스처.
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { CTR_MANAGED = 0 }\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { CTR_MANAGED = 0, PATH = \"/x\" }\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { CTR_MANAGED = 'a,b}', X = 1 }\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { CTR_MANAGED = [1,\n2] }\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { CTR_MANAGED = 0 # a, b\n", // 무효 입력 — 닫히지 않은 인라인
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { A = \"x, CTR_MANAGED = 0, y\", B = 1 }\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { A = 'x, CTR_MANAGED = \"0\", y', B = 1 }\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { CTR_STORE_ROOT = \"context-router/0.14.0\", CTR_MANAGED = \"context-router/0.14.0\" }\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = {} # }\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { \"CTR_MANAGED\" = \"context-router/0.14.0\" }\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { CTR_MANAGED = \"\n", // 무효 입력 — 미종료 문자열
		// ⑦ 재직렬화·보존 갈래(TestCodexInstallMarkerOnly 계열·TestCodexInstallScopeAndMigration·
		// TestCodexInstallProfileReadback·TestCodexTableBodyPreservesUnownedKeys의 지역 픽스처).
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = [\"--enable\", \"ingest\"]\nenabled_tools = [\"ctr_search\", \"ctr_index\", \"ctr_execute\"]\n[mcp_servers.ctr.env]\nCTR_MANAGED = \"context-router/0.15.0\"\n",
		"[mcp_servers.ctr]\ncommand=\"context-router\"\nargs=[\"--enable\",\"ingest\"]\nenabled_tools=[\"ctr_search\",\"ctr_index\"]\n[mcp_servers.ctr.env]\nCTR_MANAGED=\"context-router/0.16.0\"\n",
		"[mcp_servers.ctr]\ncommand = \"C:\\\\bin\\\\context-router.exe\"\nargs = [\"--enable\", \"ingest,net\"]\nenabled_tools = [\"ctr_search\"]\n[mcp_servers.ctr.env]\nCTR_MANAGED = \"context-router/0.15.0\"\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = [\"--profile\", \"global-search\"]\nenabled_tools = [\"custom\"]\n", // 되읽기 실패 — keepArgs 보존
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = []\nenabled_tools = [\"ctr_search\"]\ndefault_tools_approval_mode = \"prompt\"\nstartup_timeout_sec = 30\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\n\"args=x\" = \"y\"\nreal_user_key = 1\n",          // T3-F1 따옴표 키
		"[mcp_servers.ctr.env]\n\"CTR_MANAGED=x\" = \"y\"\nCTR_MANAGED = \"context-router/0.14.0\"\n",       // T3-F1 넷째 경로
		"[mcp_servers.ctr]\ncommand = \"other\"\n[mcp_servers.ctr.env]\nCTR_MANAGED = \"other-tool/1.0\"\n", // 사용자 소유
		"[mcp_servers.ctr]\ncommand = \"other\"\n[mcp_servers.ctr.env]\nCTR_MANAGED = \"\"\n",
		"[mcp_servers.ctr]\ncommand = \"other\"\nenv = { CTR_MANAGED = 0, X = \"context-router/0.15.0\" }\n",
		// 우리 테이블 안·남의 테이블 안 여러 줄 값(§2-2 경계 오인 양방향)
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nnote = \"\"\"\n[mcp_servers.fake]\n\"\"\"\n[mcp_servers.user]\ncommand = \"user-cmd\"\nblob = '''\n[mcp_servers.also_fake]\n'''\n",
		// 두 관리 테이블 사이에 사용자 테이블이 낀 재직렬화 형태
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = [\"--enable\", \"ingest,net\"]\nenabled_tools = [\"ctr_search\"]\ndefault_tools_approval_mode = \"prompt\"\n[mcp_servers.between]\ncommand = \"between-cmd\"\n[mcp_servers.ctr.env]\nCTR_MANAGED = \"context-router/0.14.0\"\nCTR_STORE_ROOT = \"D:/ctr\"\n[hooks.state]\ntrust = \"abc123\"\n[tui]\ntheme = \"dark\"\n",
		// 여러 줄 문자열 **내용**이 마커 줄 — 소유 오판 방지(C1)
		"[history]\nnotes = \"\"\"\n" + codexBlockBegin + "\n\"\"\"\n\n[mcp_servers.ctr]\ncommand = \"/opt/mine/my-wrapper\"\nargs = [\"--serve\"]\n" + codexBlockEnd + "\n",
		// 우리 구간 안에 닫히지 않은 '[' — 무효 입력(A2)
		"[mcp_servers.ctr.env]\nCTR_MANAGED = \"context-router/0.15.0\"\nLIST = [\n",
		// ⑧ v0.18이 더한 형태 — D92 열거형이 갈래를 새로 가르는 자리(재기준선 1·2·4·6·7).
		// 초록 스위트는 "우리가 픽스처로 고른 것"만 말하므로 새 갈래를 여기에 합류시켜야
		// 오발화 축이 함께 넓어진다. 순서: 여러 줄 인라인 env(갱신 갈래 · 삽입 갈래) ·
		// 중첩 인라인 env · 점 표기 표식 키 · env 우변 비-인라인 셋 ·
		// 인라인 env + 여러 줄 기본 문자열 값.
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { A = \"a\",\n  CTR_MANAGED = \"context-router/0.15.0\" }\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { A = \"1\",\n  B = \"2\" }\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { A = { CTR_MANAGED = \"inner\" }, B = \"1\" }\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { CTR_MANAGED.sub = \"x\" }\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = []\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = \"x\"\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = 1\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { A = \"\"\"\nx # y, z}\n\"\"\", B = \"b\" }\n",
	}
	for i, src := range srcs {
		res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: hookMarker("0.17.0")})
		if res.State == mcpOutputInvalid {
			t.Errorf("[%d] 정상 입력에서 게이트가 오발화했다:\n%s", i, src)
			continue
		}
		// 입력이 이미 무효인 행(중복 헤더·닫히지 않은 값)에는 **산출물 파스만** 요구하지 않는다 —
		// 그 무효는 우리가 들인 것이 아니고 게이트의 비대칭 절이 그 행을 통과시킨다. 위 오발화
		// 단정은 그 행에도 그대로 걸리므로 비대칭 절을 되돌리면 여기서 물린다.
		// **아래 멱등·제거 단정은 그 행에도 그대로 건다**: 무효 입력이면서 바이트를 바꾸는 행이
		// 실재하고(인라인 표식 되쓰기 둘), 2회차가 무변경이 아니면 D84 단일 백업 슬롯이 2회차에
		// 원본을 잃는다 — 입력의 유효성과 무관한 계약이다.
		if codexTOMLParses([]byte(src)) && !codexTOMLParses(res.Out) {
			t.Errorf("[%d] 산출물이 파스되지 않는다:\n%s", i, res.Out)
		}
		// 멱등(Global Constraints) — 바이트를 바꾸는 픽스처는 2회차가 무변경이어야 한다.
		if res.Changed {
			if again := installCodexConfigBlock(res.Out, codexInstallRequest{Marker: hookMarker("0.17.0")}); again.Changed {
				t.Errorf("[%d] 2회차가 무변경이 아니다:\n%s", i, again.Out)
			}
		}
		// 제거 산출물도 파스돼야 한다 — 스펙 §1.3 사후 5는 install·uninstall 둘 다 요구한다.
		if out, changed, _ := uninstallCodexConfigBlock([]byte(src)); changed && !codexTOMLParses(out) {
			t.Errorf("[%d] 제거 산출물이 파스되지 않는다:\n%s", i, out)
		}
	}
}

// TestCodexDottedEnvNoHeaderAppend — D90. 점 표기 env 키가 있으면 헤더를 붙이지 않는다.
// 붙이면 같은 논리 테이블이 두 번 정의돼 사용자의 Codex가 그 파일 전체를 읽지 못한다.
func TestCodexDottedEnvNoHeaderAppend(t *testing.T) {
	src := "[mcp_servers.ctr]\ncommand = \"context-router\"\nenv.FOO = \"bar\"\n"
	res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: hookMarker("0.17.0")})
	if strings.Contains(string(res.Out), "[mcp_servers.ctr.env]") {
		t.Fatalf("env 헤더를 덧붙였다 — 중복 정의:\n%s", res.Out)
	}
	// 표식을 쓸 자리가 없으므로 무변경 + 사유
	if res.State != mcpMarkerAnomaly || res.Anomaly != anomalyDottedEnv {
		t.Errorf("state=%d anomaly=%d want mcpMarkerAnomaly/anomalyDottedEnv", res.State, res.Anomaly)
	}
	if string(res.Out) != src {
		t.Errorf("산출이 입력과 다르다:\n%s", res.Out)
	}
	// 게이트가 아니라 D90이 잡았는지 — 게이트가 잡았다면 상태가 달랐을 것이다
	if res.State == mcpOutputInvalid {
		t.Errorf("게이트가 먼저 잡았다 — D90 단정이 게이트에 가려진다")
	}
}

// TestCodexDottedEnvCurrentMarkerWrites — 표식이 이미 현재 값이면 이탈하지 않는다. 이탈
// 조건을 "점 표기가 있으면"으로 넓히면 고칠 것이 없는 파일에 사유를 내는 오경보가 된다.
func TestCodexDottedEnvCurrentMarkerWrites(t *testing.T) {
	// **D90 개정 뒤 점 표기 표식은 소유 근거다** — codexMarkerValue가 그 형태를 셋째 경로로
	// 읽고 codexOwnership이 그 값을 본다. 그래서 이 픽스처의 소유는 표식 하나로 서며 command가
	// 우리 값인 것은 소유의 조건이 아니다. 이 자리가 재는 것도 소유가 아니라 **이탈 조건의
	// 한정**이다: 점 표기가 있어도 표식이 이미 현재 값이면 쓸 자리가 필요 없으므로 이탈하지
	// 않는다. 소유가 표식 하나로 서는 것을 되돌렸을 때 물리는 자리는
	// TestCodexDottedEnvJudgmentUnified다 — 그쪽 픽스처의 command는 우리 것이 아니다.
	src := "[mcp_servers.ctr]\ncommand = \"context-router\"\nenv.CTR_MANAGED = \"" + hookMarker("0.17.0") + "\"\nenv.FOO = \"bar\"\n"
	res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: hookMarker("0.17.0")})
	if res.State != mcpWritten {
		t.Fatalf("state=%d want mcpWritten — 표식이 현재 값이면 평소대로 기입한다", res.State)
	}
	if strings.Contains(string(res.Out), "[mcp_servers.ctr.env]") {
		t.Errorf("env 헤더를 덧붙였다:\n%s", res.Out)
	}
	if !strings.Contains(string(res.Out), "env.FOO = \"bar\"") {
		t.Errorf("사용자의 점 표기 줄이 사라졌다:\n%s", res.Out)
	}
	if !codexTOMLParses(res.Out) {
		t.Errorf("산출물이 파스되지 않는다:\n%s", res.Out)
	}
	// 멱등
	if again := installCodexConfigBlock(res.Out, codexInstallRequest{Marker: hookMarker("0.17.0")}); again.Changed {
		t.Errorf("2회차가 무변경이 아니다:\n%s", again.Out)
	}
}

// TestCodexDottedEnvQuotedSpaceIsNotEnv — 술어에 넘기는 인자가 **원문**인지를 호출부에서 재는
// 자리(D90 판정 술어 첫 항). `"e n v"`는 env가 아니라 남의 서브테이블이므로 우리는 평소대로
// [mcp_servers.ctr.env]를 만들어야 한다. codexReadTable이 정규화(joined)를 넘기면 stripLine이
// 따옴표 안 공백을 지워 그 줄이 env 정의로 읽히고, 헤더가 서지 않아 우리 표식이 남의 테이블에
// 있는 것처럼 취급된다 — 헤더 실존을 **긍정으로** 단정해야 그 되돌림이 여기서 물린다.
// (술어 단위의 `TestCodexDottedHead`는 인자를 재지 못한다 — 그쪽만으로는 joined로 바꿔도 녹색이다.)
func TestCodexDottedEnvQuotedSpaceIsNotEnv(t *testing.T) {
	src := "[mcp_servers.ctr]\ncommand = \"context-router\"\n\"e n v\".CTR_MANAGED = \"" + hookMarker("0.17.0") + "\"\n"
	res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: hookMarker("0.17.0")})
	if res.State != mcpWritten {
		t.Fatalf("state=%d want mcpWritten", res.State)
	}
	if !strings.Contains(string(res.Out), "[mcp_servers.ctr.env]") {
		t.Errorf("env 헤더가 서지 않았다 — \"e n v\"를 env로 읽었다:\n%s", res.Out)
	}
	if !strings.Contains(string(res.Out), "\"e n v\".CTR_MANAGED") {
		t.Errorf("남의 점 표기 줄이 사라졌다:\n%s", res.Out)
	}
	if !codexTOMLParses(res.Out) {
		t.Errorf("산출물이 파스되지 않는다:\n%s", res.Out)
	}
	if again := installCodexConfigBlock(res.Out, codexInstallRequest{Marker: hookMarker("0.17.0")}); again.Changed {
		t.Errorf("2회차가 무변경이 아니다:\n%s", again.Out)
	}
}

// TestCodexDottedEnvJudgmentUnified — D90 개정. 이 자리는 원래 "D90은 정보만 더한다 —
// 판정을 흔들지 않는다"를 쟀고, 그 픽스처의 command가 우리 것이라 판독기를 되돌려도 물리지
// 않았다. 전제가 뒤집혔다: 판독기와 판정이 갈리면 **정상 등록물**(호스트 재직렬화가 단일 키
// 서브테이블을 점 표기로 접은 형태)이 남의 것으로 읽혀 제거가 무성으로 실패한다. 이제 판독기
// 하나가 점 표기를 읽고 그 값이 곧 소유 판정이다.
//
// 그러므로 이 자리가 고정하는 것은 **바뀐 판정이 정확히 무엇인가**다. 픽스처의 command는
// 우리 것이 아니다 — 표식만이 소유 근거인 형태라야 판독기를 되돌렸을 때 물린다.
//   - 우리 표식이 박힌 점 표기 등록물은 uninstall이 제거한다(우리 것이므로 옳다).
//   - 남의 값이 든 같은 이름의 키는 여전히 남의 것이다 — 소유는 키 이름이 아니라 값이 정한다.
//   - 이상 판정(probe)은 불변이다 — 점 표기는 유효 TOML이라 사유가 아니다.
func TestCodexDottedEnvJudgmentUnified(t *testing.T) {
	ours := "[mcp_servers.ctr]\ncommand = \"/opt/bin/context-router\"\nenv.CTR_MANAGED = \"context-router/0.15.0\"\n"
	out, changed, anomaly := uninstallCodexConfigBlock([]byte(ours))
	if !changed || anomaly != anomalyNone {
		t.Errorf("우리 표식이 박힌 등록물을 남겼다: changed=%v anomaly=%d", changed, anomaly)
	}
	if strings.Contains(string(out), "mcp_servers.ctr") {
		t.Errorf("제거가 불완전하다:\n%s", out)
	}
	theirs := "[mcp_servers.ctr]\ncommand = \"/opt/bin/other\"\nenv.CTR_MANAGED = \"someone-else/1.0\"\n"
	if _, changed, anomaly := uninstallCodexConfigBlock([]byte(theirs)); changed || anomaly != anomalyNotOwned {
		t.Errorf("남의 테이블을 건드렸다: changed=%v anomaly=%d", changed, anomaly)
	}
	for _, src := range []string{ours, theirs} {
		if present, a := probeCodexMCPBlock([]byte(src)); !present || a != anomalyNone {
			t.Errorf("probe가 바뀌었다: present=%v anomaly=%d", present, a)
		}
	}
}

// TestCodexDottedHead — 술어. 입력은 **문자열 밖 공백만 무시한 원문 LHS**다. 전면 정규화된
// 라인에 걸면 따옴표 안 공백까지 지워져 "e n v"가 env로 읽히고 타인 테이블에 소유가 선다.
func TestCodexDottedHead(t *testing.T) {
	cases := []struct{ in, head, rest string }{
		{`env.FOO = "bar"`, "env", "FOO"},
		{`env."CTR_MANAGED" = "x"`, "env", "CTR_MANAGED"},
		{`"env".FOO = "bar"`, "env", "FOO"},
		{`'env'.FOO = "bar"`, "env", "FOO"},               // 홑따옴표 한 쌍도 같은 키다
		{`env.'CTR_MANAGED' = "x"`, "env", "CTR_MANAGED"}, // 꼬리도 마찬가지
		{`"env.FOO" = "bar"`, "", ""},                     // 단일 키 — 점 경로가 아니다
		{`"e n v".FOO = "bar"`, "", ""},                   // 따옴표 안 공백은 이름의 일부다
		{`env.A.B = 1`, "env", ""},                        // 세 마디 — env 정의로는 세되 표식 자리가 아니다
		{`command = "x"`, "", ""},
		// 따옴표를 **한 쌍만** 벗긴다(tomlUnquoteKey). 두 겹은 TOML이 별개 키로 읽으므로
		// 우리 env도 우리 표식도 아니다 — strings.Trim이면 둘 다 우리 것으로 읽혔다.
		{`'"env"'.CTR_MANAGED = "x"`, "", ""},
		{`env.'"CTR_MANAGED"' = "x"`, "env", `"CTR_MANAGED"`},
	}
	for _, c := range cases {
		head, rest := tomlDottedEnvKey(c.in)
		if head != c.head || rest != c.rest {
			t.Errorf("tomlDottedEnvKey(%q) = (%q,%q), want (%q,%q)", c.in, head, rest, c.head, c.rest)
		}
	}
	// 첫 마디가 이스케이프 표기면 이 술어는 인식하지 않는다 — 그 형태가 남기는 헤더 중복을
	// 실제로 받아 내는 것은 D89 게이트다. tomlDottedEnvKey 주석이 펴는 그 주장을 여기서
	// 고정한다: 단정이 없으면 게이트가 그 경로에서 빠져도 아무것도 물지 않는다.
	if head, _ := tomlDottedEnvKey("\"\\u0065nv\".CTR_MANAGED = \"x\""); head != "" {
		t.Errorf("이스케이프 마디를 env로 인식했다: head=%q", head)
	}
	esc := "[mcp_servers.ctr]\ncommand = \"context-router\"\n\"\\u0065nv\".CTR_MANAGED = \"x\"\n"
	if res := installCodexConfigBlock([]byte(esc), codexInstallRequest{Marker: hookMarker("0.17.0")}); res.State != mcpOutputInvalid {
		t.Errorf("state=%d want mcpOutputInvalid — 게이트가 받지 않으면 헤더 중복이 그대로 나간다", res.State)
	}
}

// TestCodexDottedMarkerIsRead — D90 개정. 점 표기 `env.CTR_MANAGED`는 우리가 기입한 표식과
// **같은 키**이고, 호스트가 단일 키 서브테이블을 그 형태로 접는 것은 정상 설치 뒤 정상 사용으로
// 도달하는 상태다. 판독기 하나가 그것을 읽지 않으면 그 정상 등록물에 잘못된 조치가 나간다:
// install이 "사용자 소유"로 물러나고, uninstall이 우리 등록물을 남긴 채 직접 정리하라 하며,
// 버전 라벨이 표식 없는 것으로 굳는다.
// **command가 우리 것이 아닌 행이 이 계약의 눈금이다** — 그 행에서는 표식만이 소유 근거이므로
// 판독기를 되돌리면 반드시 물린다.
func TestCodexDottedMarkerIsRead(t *testing.T) {
	cur := hookMarker("0.17.0")
	for _, command := range []string{"context-router", "/opt/bin/context-router"} {
		src := "[mcp_servers.ctr]\ncommand = \"" + command + "\"\nenv.CTR_MANAGED = \"" + cur + "\"\n"
		marker, gotCmd, found := codexConfigMarker([]byte(src))
		if !found || marker != cur || gotCmd != command {
			t.Errorf("[%s] codexConfigMarker = (%q,%q,%v), want (%q,%q,true)", command, marker, gotCmd, found, cur, command)
		}
		res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: cur})
		if res.State != mcpWritten {
			t.Errorf("[%s] install state=%d want mcpWritten — 우리 표식이 박힌 등록물이다", command, res.State)
		}
		if strings.Contains(string(res.Out), "[mcp_servers.ctr.env]") {
			t.Errorf("[%s] env 헤더를 덧붙였다 — 중복 정의:\n%s", command, res.Out)
		}
		if !codexTOMLParses(res.Out) {
			t.Errorf("[%s] 산출물이 파스되지 않는다:\n%s", command, res.Out)
		}
		if again := installCodexConfigBlock(res.Out, codexInstallRequest{Marker: cur}); again.Changed {
			t.Errorf("[%s] 2회차가 무변경이 아니다:\n%s", command, again.Out)
		}
		out, changed, anomaly := uninstallCodexConfigBlock([]byte(src))
		if !changed || anomaly != anomalyNone {
			t.Errorf("[%s] uninstall changed=%v anomaly=%d, want true/anomalyNone", command, changed, anomaly)
		}
		if strings.Contains(string(out), "mcp_servers.ctr") {
			t.Errorf("[%s] 제거가 불완전하다:\n%s", command, out)
		}
	}
	// 판독 순서의 근거 — 세 형태는 같은 논리 테이블 mcp_servers.ctr.env를 **정의**하므로 유효
	// TOML에 공존할 수 없다. 공존할 수 있다면 codexMarkerValue의 배치(새 경로를 맨 뒤)가
	// 관측 가능한 선택이 되고 "기존 두 경로의 답이 그대로다"라는 근거가 무너진다.
	for _, src := range []string{
		"[mcp_servers.ctr]\nenv.CTR_MANAGED = \"a\"\n[mcp_servers.ctr.env]\nCTR_MANAGED = \"b\"\n",
		"[mcp_servers.ctr]\nenv = { CTR_MANAGED = \"a\" }\nenv.CTR_MANAGED = \"b\"\n",
		"[mcp_servers.ctr]\nenv = { CTR_MANAGED = \"a\" }\n[mcp_servers.ctr.env]\nCTR_MANAGED = \"b\"\n",
	} {
		if codexTOMLParses([]byte(src)) {
			t.Errorf("두 형태가 공존하는 입력이 파스된다 — 판독 순서가 관측 가능해진다:\n%s", src)
		}
	}
}

// TestCodexValueReadStopsAtComment — 값 판독은 **문자열 밖 '#'에서 멈춘다**. 주석은 TOML
// 데이터가 아니므로 그것을 지나 스캔하면 후행 주석에 적은 문자열이 값으로 읽힌다. 표식과
// command가 그렇게 읽히면 **후행 주석 한 줄로 소유를 위조**할 수 있고, 판독 통일이 그 값을
// 소유 판정에 그대로 쓰므로 위조된 소유가 곧 남의 테이블을 되쓰는 권한이 된다.
// 표식 판독 경로 셋(env 서브테이블·인라인 env·점 표기)을 모두 잰다 — 한 경로만 고치면 나머지
// 둘이 같은 파일을 다른 방식으로 읽는다.
func TestCodexValueReadStopsAtComment(t *testing.T) {
	cur := hookMarker("0.17.0")
	head := "[mcp_servers.ctr]\ncommand = \"/opt/bin/context-router\"\n"
	cases := []struct{ name, src string }{
		{"점 표기", head + "env.CTR_MANAGED = 1 # \"" + cur + "\"\n"},
		{"env 서브테이블", head + "[mcp_servers.ctr.env]\nCTR_MANAGED = 1 # \"" + cur + "\"\n"},
		{"인라인 env", head + "env = { FOO = \"1\" } # , CTR_MANAGED = \"" + cur + "\"\n"},
		{"command", "[mcp_servers.ctr]\ncommand = 1 # \"context-router\"\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !codexTOMLParses([]byte(c.src)) {
				t.Fatalf("픽스처가 유효 TOML이 아니다 — 위조 경로를 재지 못한다:\n%s", c.src)
			}
			marker, command, _ := codexConfigMarker([]byte(c.src))
			if marker == cur {
				t.Errorf("표식 값을 후행 주석에서 읽었다: %q", marker)
			}
			if command == hookBinaryName {
				t.Errorf("command 값을 후행 주석에서 읽었다: %q", command)
			}
			res := installCodexConfigBlock([]byte(c.src), codexInstallRequest{Marker: cur})
			if res.State != mcpExistingHeader || res.Changed {
				t.Errorf("남의 테이블을 소유로 읽었다: state=%d changed=%v", res.State, res.Changed)
			}
		})
	}
	// 점 표기 값 판독은 소유 판정에 얹기 전부터 install의 D90 이탈을 가르는 데 쓰였다 —
	// 그 자리의 판독도 같은 기준이어야 한다.
	lines := splitLinesKeepEnds([]byte(head + "env.CTR_MANAGED = 1 # \"" + cur + "\"\n"))
	if v := codexReadTable(lines, codexManagedSpans(lines).table); v.dottedMarker == cur {
		t.Errorf("점 표기 표식 값을 후행 주석에서 읽었다: %q", v.dottedMarker)
	}
}

// TestCodexEnabledToolsStopsAtComment — 배열 안의 주석 처리된 원소는 값이 아니다(D91).
// 엔트리 판독이 여러 줄을 이어 붙인 뒤 한 번에 훑으므로 '#'을 **줄 단위로** 자르지 않으면
// 사용자가 꺼 둔 도구가 목록에 남는다: doctor는 부족을 알리지 않는데 Codex는 그 도구를 등록하지
// 않고, 주석 처리한 exec 도구는 설치기에 "이미 노출됨"을 말하게 한다.
// 이은 문자열에서 한 번만 자르면 반대 방향으로 틀린다 — 첫 주석 뒤의 **진짜 원소**가 사라진다.
func TestCodexEnabledToolsStopsAtComment(t *testing.T) {
	cur := hookMarker("0.17.0")
	head := "[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = [\"--enable\", \"ingest\"]\n"
	tail := "[mcp_servers.ctr.env]\nCTR_MANAGED = \"" + cur + "\"\n"
	req := codexInstallRequest{Marker: cur, MarkerOnly: true}

	src := head + "enabled_tools = [\n  \"ctr_search\", \"ctr_fetch\", \"ctr_transform\",\n" +
		"  \"ctr_record_event\", \"ctr_session_summary\", \"ctr_export_events\",\n" +
		"  # \"ctr_index\",\n]\n" + tail
	res := installCodexConfigBlock([]byte(src), req)
	if slices.Contains(res.Tools, "ctr_index") {
		t.Errorf("주석 처리한 도구가 목록에 들었다: %v", res.Tools)
	}
	if toolsCover(res.Tools, res.WantTools) {
		t.Errorf("부족을 알리지 않는다: have=%v want=%v", res.Tools, res.WantTools)
	}
	// 주석 **뒤**의 원소는 살아 있어야 한다 — 줄 단위 절단의 눈금이다.
	after := head + "enabled_tools = [\n  \"ctr_search\",\n  # \"ctr_index\",\n  \"ctr_fetch\",\n]\n" + tail
	if got := installCodexConfigBlock([]byte(after), req).Tools; !slices.Equal(got, []string{"ctr_search", "ctr_fetch"}) {
		t.Errorf("tools=%v, want [ctr_search ctr_fetch]", got)
	}
	// exec 노출 안내도 같은 목록을 본다.
	execSrc := head + "enabled_tools = [\n  \"ctr_search\",\n  # \"ctr_execute\",\n]\n" + tail
	if got := installCodexConfigBlock([]byte(execSrc), req); got.ExecExposed {
		t.Errorf("주석 처리한 exec 도구를 노출로 읽었다: tools=%v", got.Tools)
	}
}

// TestCodexBOMOnManagedHeader — BOM이 **우리 테이블 헤더 줄**에 붙은 파일. `hook install --codex`가
// 만드는 파일의 첫 줄이 그 헤더라 도달 가능하다. Go의 strings.Fields는 U+FEFF를 공백으로 보지
// 않으므로 판정 정규화가 그 세 바이트를 통과시켰고, 그러면 우리 구간을 잃어 그 구간의 키가
// "구간 밖 ctr 정의"로 잡혀 재등록이 충돌로 막혔다 — 우리가 만든 파일에 편집기가 세 바이트를
// 붙였을 뿐인데.
func TestCodexBOMOnManagedHeader(t *testing.T) {
	src := codexBOM + ctrTableFixture
	// 입력 자체는 우리 파서가 받는다(v0.17이 파스 판정 앞에서 BOM을 뗀다) — 그래서 이 픽스처가
	// 재는 것은 파스 축이 아니라 **라인 정규화 축** 하나다. 파스 축이 대신 물면 무엇을 쟀는지
	// 알 수 없어진다.
	if !codexTOMLParses([]byte(src)) {
		t.Fatalf("픽스처 입력이 파스되지 않는다 — 라인 정규화 축을 홀로 잴 수 없다")
	}
	sp := codexManagedSpans(splitLinesKeepEnds([]byte(src)))
	if !sp.table.found || sp.table.start != 0 {
		t.Fatalf("BOM 붙은 헤더를 구간으로 잡지 못했다: found=%v start=%d", sp.table.found, sp.table.start)
	}
	res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: hookMarker("0.17.2")})
	if res.State != mcpWritten || !res.TableFound {
		t.Fatalf("state=%d tableFound=%v want mcpWritten/true", res.State, res.TableFound)
	}
	// **세 바이트가 살아남아야 한다.** 판정에서만 떼고 되쓰기는 헤더 라인을 원문 그대로
	// 옮기므로, 이 단정이 깨지면 우리가 사용자 파일의 인코딩을 조용히 바꾼 것이다.
	if !bytes.HasPrefix(res.Out, []byte(codexBOM)) {
		t.Errorf("산출이 선두 BOM을 잃었다:\n%q", string(res.Out))
	}
	if !codexTOMLParses(res.Out) {
		t.Errorf("산출이 파스되지 않는다:\n%s", res.Out)
	}
	// 멱등(스펙 §2.2) — D84의 단일 백업 슬롯이 2회차 무변경 위에 서 있다.
	again := installCodexConfigBlock(res.Out, codexInstallRequest{Marker: hookMarker("0.17.2")})
	if !bytes.Equal(again.Out, res.Out) {
		t.Errorf("멱등이 아니다:\n1차:\n%s\n2차:\n%s", res.Out, again.Out)
	}
}

// TestCodexBOMNotStrippedMidLine — BOM 제거는 **선두 한정**이다. 줄 안쪽의 U+FEFF까지 지우면
// 사용자 값 안의 그 문자가 판정에서 사라져 다른 키·다른 이름으로 읽힌다.
func TestCodexBOMNotStrippedMidLine(t *testing.T) {
	if got := stripLine([]byte("[mcp_servers." + codexBOM + "ctr]\n")); got == "[mcp_servers.ctr]" {
		t.Errorf("줄 안쪽 U+FEFF까지 지웠다: %q", got)
	}
}

// TestCodexBlockOwnsTrailingComment — 구 블록의 소유 판정이 헤더를 정규화 문자열 == 로
// 비교해, 헤더에 후행 주석이 붙은 파일에서 마이그레이션이 닫혔다. 방향은 fail-safe이지만
// 그 파일은 영영 구형식으로 남는다 — 값 판독이 v0.17에서 주석을 인지하게 된 것과 같은 부류의
// 자리이고, 헤더 이름 추출은 그때 함께 오지 않았다.
func TestCodexBlockOwnsTrailingComment(t *testing.T) {
	src := codexBlockBegin + "\n[mcp_servers.ctr] # 우리 등록물\ncommand = \"context-router\"\n" + codexBlockEnd + "\n"
	class, begin, end := classifyMarkers(splitLinesKeepEnds([]byte(src)))
	if class != classReplace {
		t.Fatalf("class=%d begin=%d end=%d want classReplace — 헤더 후행 주석에서 소유 판정이 닫혔다", class, begin, end)
	}
}

// TestCodexOldBlockBOMMigrates — 구 블록의 BEGIN 마커가 파일 첫 줄이면 BOM이 그 줄에 붙는다.
// 마커 정확 매치가 그 세 바이트를 보면 분류가 classAppend로 떨어지고, install이 관리 테이블을
// 새로 append해 같은 논리 테이블을 두 번 정의한다 — 게이트가 기입을 막지만 그 파일은 어떤
// 명령으로도 풀리지 않는 막다른 갈래가 된다.
func TestCodexOldBlockBOMMigrates(t *testing.T) {
	src := codexBOM + codexBlockBegin + "\n[mcp_servers.ctr]\ncommand = \"context-router\"\n" + codexBlockEnd + "\n"
	class, begin, _ := classifyMarkers(splitLinesKeepEnds([]byte(src)))
	if class != classReplace || begin != 0 {
		t.Fatalf("class=%d begin=%d want classReplace/0 — BOM이 마커 분류를 뒤집었다", class, begin)
	}
}

// TestTomlPointInvalid — 무효 지점은 (-1, -1)이다(스펙 §0 D92 계약 4). 0을 무효로 쓰면
// "없다"와 "0행 0열이다"가 같은 값이 되고, v0.17 §1.4-라가 마커 인덱스에서 같은 이유로
// (0,0)을 버렸다.
func TestTomlPointInvalid(t *testing.T) {
	if p := tomlNoPoint(); p.valid() {
		t.Errorf("tomlNoPoint()가 유효하다: %+v", p)
	}
	if p := (tomlPoint{line: 0, col: 0}); !p.valid() {
		t.Errorf("(0,0)이 무효로 판정된다 — 그것은 실재하는 지점이다")
	}
	if p := (tomlPoint{line: 3, col: 7}); !p.valid() {
		t.Errorf("(3,7)이 무효로 판정된다")
	}
	// 세 타입에 소비자가 생기는 것은 T5이므로 이 블록이 없으면 T1~T4 구간 내내
	// golangci-lint(unused)가 적색이다(리허설 실측: "type tomlSpan is unused" 외 2건, 3 issues).
	// **복합 리터럴에 필드명을 적어야** 사용으로 센다 — 영값 tomlSpan{}으로 바꾸면
	// "field start is unused"로 다시 걸린다.
	zero := tomlSpan{start: tomlNoPoint(), end: tomlNoPoint()}
	sc := tomlInlineScan{open: tomlNoPoint(), close: tomlNoPoint()}
	sc.entries = append(sc.entries, tomlInlineEntry{key: zero, value: zero, segs: 1, escaped: false})
	if sc.ok || len(sc.entries) != 1 || sc.entries[0].escaped {
		t.Errorf("영값 열거 결과가 예상과 다르다: %+v", sc)
	}
	if sc.entries[0].key.start.valid() || sc.entries[0].value.end.valid() {
		t.Errorf("무효 지점으로 세운 구간이 유효하다: %+v", sc.entries[0])
	}
}

// TestCodexEntriesMultilineInline — 중괄호로 이어진 인라인 테이블은 한 논리 엔트리다.
// 지정 파서가 그 형태를 받으므로(TOML 1.1.0) 게이트의 비대칭 계약이 성립하는 유효 입력이고,
// 갈라 잡으면 그 뒤 모든 판독이 반쪽 문자열을 본다.
func TestCodexEntriesMultilineInline(t *testing.T) {
	src := "[mcp_servers.ctr]\nenv = { A = \"a\",\n  CTR_MANAGED = \"m\" }\ncommand = \"x\"\n"
	if !codexTOMLParses([]byte(src)) {
		t.Fatalf("픽스처가 파스되지 않는다 — 유효 입력에서 재는 축이 아니게 된다")
	}
	lines := splitLinesKeepEnds([]byte(src))
	sp := codexManagedSpans(lines)
	got := codexEntries(lines, sp.table)
	want := [][2]int{{1, 2}, {3, 3}}
	if len(got) != len(want) {
		t.Fatalf("엔트리 수=%d want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("엔트리[%d]=%v want %v", i, got[i], want[i])
		}
	}
}

// TestTomlScannerInlineOpenAtEOF — 닫히지 않은 인라인 테이블은 EOF에서 열림이며 사유
// 문면이 그 형태를 말해야 한다. 문면이 "문자열 또는 배열"만 말하면 사용자가 자기 파일에서
// 무엇을 고쳐야 하는지 알 수 없다.
func TestTomlScannerInlineOpenAtEOF(t *testing.T) {
	var sc tomlLineScanner
	sc.step([]byte("env = { A = \"a\",\n"))
	if !sc.open() {
		t.Errorf("여는 중괄호 뒤에도 스캐너가 닫혀 있다")
	}
	sc.step([]byte("  B = \"b\" }\n"))
	if sc.open() {
		t.Errorf("닫는 중괄호 뒤에도 스캐너가 열려 있다")
	}
	if r := anomalyScannerOpen.reason(); !strings.Contains(r, "인라인 테이블") {
		t.Errorf("사유 문면이 인라인 테이블을 말하지 않는다: %q", r)
	}
}

// TestCodexEntryRawRoundTrip — 되사상은 모든 줄 종결자 조합에서 옳아야 한다. 각 라인의
// 보존 구간이 [0, 주석위치)라는 접두 슬라이스이므로 라인당 시작 오프셋 하나면 충분하고,
// CRLF·마지막 줄 종결자 없음이 전부 접미 절단이라 자동으로 옳다(스펙 §0 D92 계약 2).
// **픽스처는 전부 헤더 줄로 시작한다** — 생산 경로에서 codexEntries가 sp.start+1부터 세므로
// 엔트리 첫 줄은 파일 0행이 될 수 없고, 그래서 codexEntryRaw가 접두를 떼지 않아도 옳다.
// 선두 BOM 케이스가 그 사실을 잰다(BOM은 0행 선두에만 존재하고 그 줄은 헤더다).
// 스펙 §1.3 선행 게이트 1이 요구하는 축이 이 표다: 줄 종결자 조합 · 선두 BOM 유무 ·
// 들여쓰기 유무.
func TestCodexEntryRawRoundTrip(t *testing.T) {
	const head = "[mcp_servers.ctr]\n"
	for _, c := range []struct{ name, src string }{
		{"LF", head + "env = { A = \"a b\",\n  B = \"c\" }\n"},
		{"CRLF", "[mcp_servers.ctr]\r\nenv = { A = \"a b\",\r\n  B = \"c\" }\r\n"},
		{"마지막 줄 종결자 없음", head + "env = { A = \"a b\",\n  B = \"c\" }"},
		{"후행 주석", head + "env = { A = \"a b\", # 메모\n  B = \"c\" } # 끝\n"},
		{"선두 BOM", "\xEF\xBB\xBF" + head + "env = { A = \"a b\",\n  B = \"c\" }\n"},
		{"들여쓰기", head + "  env = { A = \"a b\",\n\tB = \"c\" }\n"},
		{"BOM + CRLF + 들여쓰기", "\xEF\xBB\xBF[mcp_servers.ctr]\r\n  env = { A = \"a b\",\r\n\tB = \"c\" }\r\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			lines := splitLinesKeepEnds([]byte(c.src))
			e := [2]int{1, len(lines) - 1} // 0행은 헤더다 — 엔트리에 들지 않는다
			joined, at := codexEntryRaw(lines, e)
			if len(at) != e[1]-e[0]+1 {
				t.Fatalf("at 길이=%d want %d", len(at), e[1]-e[0]+1)
			}
			// 되사상: joined의 각 오프셋이 원문의 그 바이트를 가리켜야 한다.
			for off := 0; off < len(joined); off++ {
				p := codexPointAt(e, at, len(joined), off)
				if !p.valid() {
					t.Fatalf("off=%d에서 무효 지점", off)
				}
				if got := lines[p.line][p.col]; got != joined[off] {
					t.Fatalf("off=%d → (%d,%d): 원문 %q != joined %q", off, p.line, p.col, got, joined[off])
				}
			}
			// 범위 밖 오프셋은 무효 지점이다 — 주석이 그렇게 적혀 있는데 상한 검사가 없으면
			// 임의의 큰 오프셋이 유효 좌표가 되고 소비자가 그 값으로 스플라이스한다.
			for _, bad := range []int{-1, len(joined) + 1, 9999} {
				if p := codexPointAt(e, at, len(joined), bad); p.valid() {
					t.Errorf("범위 밖 off=%d가 유효 좌표다: %+v", bad, p)
				}
			}
			// 따옴표 안 공백이 살아 있어야 한다 — 잔여 ②가 닫히는 근거다.
			if !strings.Contains(joined, "a b") {
				t.Errorf("따옴표 안 공백이 지워졌다: %q", joined)
			}
			// 주석은 잘려야 한다.
			if strings.Contains(joined, "메모") || strings.Contains(joined, "끝") {
				t.Errorf("주석이 남았다: %q", joined)
			}
		})
	}
}

// TestCodexEntryRawCarriesStringState — 주석 절단이 **여러 줄 문자열 상태를 이월**해야 한다
// (스펙 §0 D92 계약 6). 무상태 stripTrailingComment를 라인마다 부르면 여러 줄 기본 문자열
// 안의 '#'을 주석으로 잘라 그 뒤 바이트를 잃는다 — 실측: 아래 픽스처가 '# def'와 그 뒤를
// 통째로 잃어 되사상이 그 자리에서 어긋난다.
func TestCodexEntryRawCarriesStringState(t *testing.T) {
	src := "[mcp_servers.ctr]\nenv = { A = \"\"\"\nabc # def\n\"\"\" }\n"
	lines := splitLinesKeepEnds([]byte(src))
	joined, _ := codexEntryRaw(lines, [2]int{1, 3})
	if !strings.Contains(joined, "# def") {
		t.Errorf("여러 줄 문자열 안의 주석 모양을 잘라 냈다: %q", joined)
	}
	if !strings.Contains(joined, `}`) {
		t.Errorf("문자열 뒤 닫는 중괄호가 사라졌다: %q", joined)
	}
}

// TestCodexKeyNameIgnoresOutsideSpace — 키 추출은 문자열 밖 공백을 무시한다. 이 성질이
// 없으면 원문 전환이 조용히 깨진다 — bare 키마다 후행 공백이 붙어 codexReadTable의 키
// 분기가 전부 미스하고 codexEnvBody의 표식 줄 제외도 서지 않는다.
func TestCodexKeyNameIgnoresOutsideSpace(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`env = { A = "1" }`, "env"},
		{`env={ A = "1" }`, "env"},
		{`CTR_MANAGED = "x"`, "CTR_MANAGED"},
		{`command = "ctr"`, "command"},
		{`  args = ["x"]`, "args"},
		{`"e n v" = { A = "1" }`, "e n v"}, // 따옴표 **안** 공백은 지우지 않는다(잔여 ②)
		{`"args=x" = "y"`, "args=x"},       // 따옴표 키 안의 '='가 이름을 자르지 않는다
		{`# 주석`, ""},
		{`값만 있고 등호가 없다`, ""},
	} {
		if got := codexKeyName(c.in); got != c.want {
			t.Errorf("codexKeyName(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestTomlKeyTokenHasEscapeIgnoresSpace — 이스케이프 키 검사도 같은 규칙을 쓴다.
// 공백을 건너뛰지 않으면 D87의 이탈이 fail-open으로 뒤집혀, 정규화 불가 키를 담은 파일에
// 우리가 기입하게 된다.
func TestTomlKeyTokenHasEscapeIgnoresSpace(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{`"C:\t"=2`, true},
		{`"C:\t" = 2`, true},  // 닫는 따옴표 **뒤** 공백
		{` "C:\t" = 2`, true}, // **선행** 공백 — 원문 전환 뒤 인라인 마디는 ',' 다음에 공백이 온다
		{"\t\"C:\\t\"\t=\t2", true},
		{`"plain" = 2`, false},
		{` bare = 2`, false},
		{`  `, false},
	} {
		if got := tomlKeyTokenHasEscape(c.in); got != c.want {
			t.Errorf("tomlKeyTokenHasEscape(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

// TestCodexInlineQuotedSpaceKeyIsNotEnv — 재기준선 행 3의 **설치 수준** 결속. 헬퍼 단위
// 테스트만 두면 호출부가 정규화에 남아 있어도 초록이라, 잔여 ②가 배송되지 않은 채 통과한다
// (실측: 헬퍼만 고친 상태에서 아래 픽스처에 우리 표식이 기입된다). 점 표기 형태는
// TestCodexDottedEnvQuotedSpaceIsNotEnv가 이미 잡지만 인라인 형태에는 결속이 없었다.
func TestCodexInlineQuotedSpaceKeyIsNotEnv(t *testing.T) {
	src := "[mcp_servers.ctr]\ncommand = \"context-router\"\n\"e n v\" = { A = \"1\" }\n"
	res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: hookMarker("0.18.0")})
	// 그 줄이 **바이트 동일**하게 살아 있어야 한다 — 표식이 기입되면 줄이 길어져 물린다.
	if !strings.Contains(string(res.Out), "\n\"e n v\" = { A = \"1\" }\n") {
		t.Errorf("남의 테이블 대입이 원문 그대로가 아니다:\n%s", res.Out)
	}
	// 표식은 우리 env 서브테이블에 새로 서야 한다 — "아무것도 안 한다"로 접히지 않게 잡는다.
	if !strings.Contains(string(res.Out), "["+codexManagedEnv+"]") {
		t.Errorf("우리 env 서브테이블이 서지 않았다:\n%s", res.Out)
	}
	if !codexTOMLParses(res.Out) {
		t.Errorf("산출이 파스되지 않는다:\n%s", res.Out)
	}
}

// TestTomlScanInline — 스펙 §0 D92 계약 1·4·5.
// **키 텍스트와 segs만 단정하면 부족하다**: escaped·open·close와 값 구간이 상수이거나
// 오프바이원이어도 통과한다(리뷰 실측). 그래서 열마다 기대값을 적고, P0 오라클을 케이스마다
// 함께 건다.
func TestTomlScanInline(t *testing.T) {
	for _, c := range []struct {
		name        string
		src         string
		wantOK      bool
		wantKeys    []string // 깊이 1 마디의 키 텍스트(원문 그대로)
		wantVals    []string // 같은 마디의 값 텍스트(원문 그대로)
		wantSegs    []int
		wantEscaped []bool
	}{
		{"단순", `env = { A = "1", B = "2" }`, true, []string{"A", "B"}, []string{`"1"`, `"2"`}, []int{1, 1}, []bool{false, false}},
		{"중첩은 값으로 통째", `env = { A = { CTR_MANAGED = "in" }, B = "1" }`, true, []string{"A", "B"}, []string{`{ CTR_MANAGED = "in" }`, `"1"`}, []int{1, 1}, []bool{false, false}},
		{"점 표기는 깊이 1", `env = { CTR_MANAGED.sub = "x" }`, true, []string{"CTR_MANAGED.sub"}, []string{`"x"`}, []int{2}, []bool{false}},
		{"후행 쉼표에 유령 없음", `env = { A = "1", }`, true, []string{"A"}, []string{`"1"`}, []int{1}, []bool{false}},
		{"빈 테이블", `env = {}`, true, nil, nil, nil, nil},
		{"빈 테이블 + 공백", `env = {   }`, true, nil, nil, nil, nil},
		{"값 안 쉼표", `env = { A = "x,y", B = "2" }`, true, []string{"A", "B"}, []string{`"x,y"`, `"2"`}, []int{1, 1}, []bool{false, false}},
		{"값 안 중괄호", `env = { A = "}", B = "2" }`, true, []string{"A", "B"}, []string{`"}"`, `"2"`}, []int{1, 1}, []bool{false, false}},
		{"값이 배열", `env = { A = ["x", "y"], B = "2" }`, true, []string{"A", "B"}, []string{`["x", "y"]`, `"2"`}, []int{1, 1}, []bool{false, false}},
		{"값이 정수", `env = { A = 1, B = "2" }`, true, []string{"A", "B"}, []string{`1`, `"2"`}, []int{1, 1}, []bool{false, false}},
		// escaped — 단순 키와 점 표기 키 각각 한 줄. **표시만 하고 소비하지 않는다**(계약 3).
		{"이스케이프 단순 키", `env = { "C:\t" = "1" }`, true, []string{`"C:\t"`}, []string{`"1"`}, []int{1}, []bool{true}},
		{"이스케이프 점 표기 키", `env = { "C:\t".sub = "x" }`, true, []string{`"C:\t".sub`}, []string{`"x"`}, []int{2}, []bool{true}},
		{"이스케이프 없는 따옴표 키", `env = { "plain" = "1" }`, true, []string{`"plain"`}, []string{`"1"`}, []int{1}, []bool{false}},
		// ok=false — 구조가 확정되지 않은 형태. entries가 **비어야** 한다(계약 4).
		{"닫히지 않음", `env = { A = "1"`, false, nil, nil, nil, nil},
		{"인라인 아님", `env = []`, false, nil, nil, nil, nil},
		{"값이 빈 구간", `env = { A = }`, false, nil, nil, nil, nil},
		{"쉼표 둘", `env = { A = "1",, B = "2" }`, false, nil, nil, nil, nil},
		{"키가 빈 구간", `env = { = "1" }`, false, nil, nil, nil, nil},
		{"짝 어긋난 괄호", `env = { A = [1} }`, false, nil, nil, nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			lines := splitLinesKeepEnds([]byte("[mcp_servers.ctr]\n" + c.src + "\n"))
			e := [2]int{1, len(lines) - 1} // 0행은 헤더 — 생산 경로와 같은 형태다
			sc := tomlScanInline(lines, e)
			if sc.ok != c.wantOK {
				t.Fatalf("ok=%v want %v", sc.ok, c.wantOK)
			}
			assertInlineScanTiles(t, lines, e, sc)
			if !c.wantOK {
				if sc.open.valid() || sc.close.valid() {
					t.Errorf("실패 결과가 유효 지점을 낸다: open=%+v close=%+v", sc.open, sc.close)
				}
				return
			}
			joined, at := codexEntryRaw(lines, e)
			// open·close는 실제로 그 중괄호를 가리켜야 한다 — 상수여도 통과하지 않게 잰다.
			if o := at[sc.open.line-e[0]] + sc.open.col; joined[o] != '{' {
				t.Errorf("open이 %q를 가리킨다", joined[o])
			}
			if o := at[sc.close.line-e[0]] + sc.close.col; joined[o] != '}' {
				t.Errorf("close가 %q를 가리킨다", joined[o])
			}
			if len(sc.entries) != len(c.wantKeys) {
				t.Fatalf("마디 수=%d want %d", len(sc.entries), len(c.wantKeys))
			}
			for i, en := range sc.entries {
				if got := tomlSpanText(joined, at, e, en.key); got != c.wantKeys[i] {
					t.Errorf("마디[%d] 키=%q want %q", i, got, c.wantKeys[i])
				}
				if got := tomlSpanText(joined, at, e, en.value); got != c.wantVals[i] {
					t.Errorf("마디[%d] 값=%q want %q", i, got, c.wantVals[i])
				}
				if en.segs != c.wantSegs[i] {
					t.Errorf("마디[%d] segs=%d want %d", i, en.segs, c.wantSegs[i])
				}
				if en.escaped != c.wantEscaped[i] {
					t.Errorf("마디[%d] escaped=%v want %v", i, en.escaped, c.wantEscaped[i])
				}
			}
		})
	}
}

// TestTomlScanInlineMultiline — 여러 줄 논리 엔트리에서도 구간이 파일 좌표로 옳고, 여러 줄
// 기본 문자열 안의 '#'·','·'}'가 구조 문자로 잡히지 않는다(계약 6 + 상태 이월).
func TestTomlScanInlineMultiline(t *testing.T) {
	for _, c := range []struct {
		name, src string
		wantKeys  []string
	}{
		{"두 줄", "[mcp_servers.ctr]\nenv = { A = \"a\",\n  B = \"b\" }\n", []string{"A", "B"}},
		{"여러 줄 기본 문자열 값", "[mcp_servers.ctr]\nenv = { A = \"\"\"\nx # y, z}\n\"\"\", B = \"b\" }\n", []string{"A", "B"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			lines := splitLinesKeepEnds([]byte(c.src))
			e := [2]int{1, len(lines) - 1}
			sc := tomlScanInline(lines, e)
			if !sc.ok {
				t.Fatalf("ok=false — 여러 줄 엔트리를 열거하지 못했다")
			}
			assertInlineScanTiles(t, lines, e, sc)
			joined, at := codexEntryRaw(lines, e)
			if len(sc.entries) != len(c.wantKeys) {
				t.Fatalf("마디 수=%d want %d", len(sc.entries), len(c.wantKeys))
			}
			for i, en := range sc.entries {
				if got := tomlSpanText(joined, at, e, en.key); got != c.wantKeys[i] {
					t.Errorf("마디[%d] 키=%q want %q", i, got, c.wantKeys[i])
				}
			}
		})
	}
}

// TestCodexInlineInsertMultiline — 여러 줄로 이어진 인라인 env에 표식이 **없으면** 여는
// 중괄호 뒤에 삽입한다. 현행은 라인 하나만 받으므로 이 형태에서 여는 중괄호가 있는 줄만 보고
// 나머지 줄을 잃거나(줄 단위 되쓰기) 원문 보존으로 빠져 표식이 영영 서지 않았다.
func TestCodexInlineInsertMultiline(t *testing.T) {
	src := "[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { A = \"1\",\n  B = \"2\" }\n"
	res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: hookMarker("0.18.0")})
	if !strings.Contains(string(res.Out), hookMarker("0.18.0")) {
		t.Errorf("표식이 삽입되지 않았다:\n%s", res.Out)
	}
	if strings.Count(string(res.Out), codexMarkerKey) != 1 {
		t.Errorf("표식이 %d번 들어갔다:\n%s", strings.Count(string(res.Out), codexMarkerKey), res.Out)
	}
	for _, keep := range []string{`A = "1"`, `B = "2"`} {
		if !strings.Contains(string(res.Out), keep) {
			t.Errorf("사용자 값 %s가 사라졌다:\n%s", keep, res.Out)
		}
	}
	if !codexTOMLParses(res.Out) {
		t.Errorf("산출이 파스되지 않는다:\n%s", res.Out)
	}
	again := installCodexConfigBlock(res.Out, codexInstallRequest{Marker: hookMarker("0.18.0")})
	if !bytes.Equal(again.Out, res.Out) {
		t.Errorf("멱등이 아니다:\n1: %s\n2: %s", res.Out, again.Out)
	}
}

// TestSetInlineEnvMarkerPreservesOnFail — 구조가 확정되지 않으면 **논리 엔트리 바이트를 그대로**
// 돌려준다. nil을 돌려주면 호출자의 b = append(b, setInlineEnvMarker(...)...)가 그 줄들을
// 산출에서 **없앤다** — 사용자 값이 조용히 사라지고 남은 파일은 파스되지 않는다.
// 새 시그니처를 직접 부른다: install 경로로만 재면 앞선 이탈 갈래에 가려 이 계약이 안 보인다.
func TestSetInlineEnvMarkerPreservesOnFail(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"닫히지 않음", "[mcp_servers.ctr]\nenv = { A = \"1\"\n"},
		{"인라인 아님", "[mcp_servers.ctr]\nenv = []\n"},
		{"쉼표 둘", "[mcp_servers.ctr]\nenv = { A = \"1\",, B = \"2\" }\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			lines := splitLinesKeepEnds([]byte(c.src))
			e := [2]int{1, len(lines) - 1}
			var want []byte
			for i := e[0]; i <= e[1]; i++ {
				want = append(want, lines[i]...)
			}
			if got := setInlineEnvMarker(lines, e, hookMarker("0.18.0")); !bytes.Equal(got, want) {
				t.Errorf("원문이 보존되지 않았다:\ngot =%q\nwant=%q", got, want)
			}
		})
	}
}

// TestCodexInlineInsertEmptyNoComma — 빈 인라인 테이블에는 쉼표를 붙이지 않는다(TOML 1.0.0이
// 후행 쉼표를 금지한다). 빈 여부를 이제 열거 결과가 정하므로, 종전 "여는 중괄호 뒤 첫 비공백
// 토큰" 판정이 지키던 계약이 전환 뒤에도 서는지 확인하는 회귀다.
func TestCodexInlineInsertEmptyNoComma(t *testing.T) {
	for _, src := range []string{
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = {}\n",
		"[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = {} # }\n",
	} {
		res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: hookMarker("0.18.0")})
		if !codexTOMLParses(res.Out) {
			t.Errorf("산출이 파스되지 않는다:\n%s", res.Out)
		}
		if strings.Contains(string(res.Out), ",}") || strings.Contains(string(res.Out), ", }") {
			t.Errorf("빈 테이블에 후행 쉼표를 붙였다:\n%s", res.Out)
		}
	}
}

// TestCodexInlineMarkerSpliceCRLFPreserved — CRLF 보존은 지금까지 codexEntryRaw 층
// (TestCodexEntryRawRoundTrip)에서만 재고 있었다 — installCodexConfigBlock을 거치는 갱신·삽입
// 두 갈래에는 결속이 없었다. spliceInlineSpan은 원문 라인을 종결자째 그대로 옮기므로("줄
// 종결자를 새로 만들지 않는다") CRLF도 자동으로 옳아야 한다는 것이 계약이다. 종결자를 합성하는
// 회귀가 들어오면 "\r\n" 쌍을 전부 지운 나머지에 홑 \n(합성된 개행) 또는 홑 \r(짝을 잃은 개행)이
// 남는다 — 그 잔여로 계약 위반을 잡는다.
func TestCodexInlineMarkerSpliceCRLFPreserved(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		// 갱신 갈래 — 표식이 이미 있고 옛 버전이다. 값 구간만 갈아 끼운다(여러 줄 인라인).
		{"갱신", "[mcp_servers.ctr]\r\ncommand = \"context-router\"\r\nenv = { A = \"1\",\r\n  CTR_MANAGED = \"context-router/0.17.0\" }\r\n"},
		// 삽입 갈래 — 표식이 없다. 여는 중괄호 뒤에 끼운다(여러 줄 인라인).
		{"삽입", "[mcp_servers.ctr]\r\ncommand = \"context-router\"\r\nenv = { A = \"1\",\r\n  B = \"2\" }\r\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := installCodexConfigBlock([]byte(c.src), codexInstallRequest{Marker: hookMarker("0.18.0")})
			if !strings.Contains(string(res.Out), hookMarker("0.18.0")) {
				t.Errorf("표식이 서지 않았다:\n%q", res.Out)
			}
			if !strings.Contains(string(res.Out), `A = "1"`) {
				t.Errorf("사용자 값이 사라졌다:\n%q", res.Out)
			}
			if rest := bytes.ReplaceAll(res.Out, []byte("\r\n"), nil); bytes.ContainsAny(rest, "\r\n") {
				t.Errorf("종결자가 CRLF로 통일되지 않았다(홑 \\n 또는 짝 없는 \\r):\n%q", res.Out)
			}
			if !codexTOMLParses(res.Out) {
				t.Errorf("산출이 파스되지 않는다:\n%s", res.Out)
			}
			again := installCodexConfigBlock(res.Out, codexInstallRequest{Marker: hookMarker("0.18.0")})
			if !bytes.Equal(again.Out, res.Out) {
				t.Errorf("2회차가 바이트 동일하지 않다:\n1: %q\n2: %q", res.Out, again.Out)
			}
		})
	}
}

// TestCodexInlineOneLineStringStaysInLine — 한 줄 문자열 토큰은 codexEntryRaw의 **조각 경계를
// 넘지 못한다.** 넘게 두면 줄 끝까지 닫히지 않은 문자열이 다음 줄의 따옴표와 한 토큰으로
// 융합되고, 그 값 구간을 스플라이스하면 **사이의 물리 라인이 통째로 지워진다.**
// 실측(교차모델 C1): 아래 첫 픽스처에서 이 릴리스는 KEEPME 줄을 지웠고 base(128a727)는
// 보존했다 — 이 릴리스가 들인 회귀다. D89 게이트는 구조적으로 못 잡는다: 입력이 무효 TOML이라
// 비대칭 계약이 통과시키고 오히려 산출 쪽이 유효해진다.
//
// TOML은 한 줄 문자열 안의 줄바꿈을 금지하므로 그 융합은 언제나 오독이고, 열거가 실패하면
// 소비자가 원문 바이트를 그대로 옮긴다. **여러 줄 문자열은 넘어도 된다** — 재기준선 행 6이
// 여러 줄 값의 제자리 갱신을 요구하며 TestCodexMultilineInlineMarkerUpdates가 그것을 잡는다.
func TestCodexInlineOneLineStringStaysInLine(t *testing.T) {
	const head = "[mcp_servers.ctr]\ncommand = \"context-router\"\n"
	for _, c := range []struct{ name, src string }{
		{"값 자리 큰따옴표", head + "env = { CTR_MANAGED = \"old\nKEEPME\"\n}\n"},
		{"값 자리 홑따옴표", head + "env = { CTR_MANAGED = 'old\nKEEPME'\n}\n"},
		{"키 자리", head + "env = { \"old\nKEEPME\" = 1\n}\n"},
		{"표식이 아닌 키", head + "env = { OTHER = \"old\nKEEPME\"\n}\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := []byte(c.src)
			res := installCodexConfigBlock(in, codexInstallRequest{Marker: hookMarker("0.18.0")})
			// 사용자 바이트가 하나도 바뀌지 않아야 한다 — 사라진 줄을 부분 검사로 찾으면
			// "표식만 끼워 넣고 나머지는 그대로"인 회귀가 통과한다.
			if !bytes.Equal(res.Out, in) {
				t.Errorf("사용자 바이트가 바뀌었다:\n입력: %q\n산출: %q", in, res.Out)
			}
			// 무변경으로 굳는 이유를 사용자에게 알려야 한다(D85).
			if res.Anomaly == anomalyNone {
				t.Errorf("무변경인데 사유가 없다: state=%v", res.State)
			}
		})
	}
}

// TestCodexEntryRawCutsCommentAfterMultilineClose — 여러 줄 문자열이 **닫히는 그 줄**의 후행
// 주석은 진짜 주석이므로 잘라야 한다. 줄 머리 상태만 보면 그 주석이 joined에 남아 값 구간
// 안으로 들어오고, 스플라이스가 그 주석과 뒤따르는 줄바꿈을 함께 먹는다.
// 실측(교차모델 C2): 아래 픽스처는 **유효 TOML**이라 게이트 대상이 아니며, base(128a727)는
// 그 줄을 원문 그대로 보존했다 — 이 릴리스가 들인 회귀다. codexEntryRaw의 종전 주석이 적던
// "그 자리는 언제나 닫는 중괄호 뒤"라는 단정이 이 입력으로 반증된다.
func TestCodexEntryRawCutsCommentAfterMultilineClose(t *testing.T) {
	src := "[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { A = \"\"\"\nx\n\"\"\", CTR_MANAGED = \"old\" # keep\n}\n"
	lines := splitLinesKeepEnds([]byte(src))
	joined, _ := codexEntryRaw(lines, [2]int{2, 5})
	if strings.Contains(joined, "# keep") {
		t.Errorf("닫는 그 줄의 진짜 주석이 값으로 남았다: %q", joined)
	}
	if !strings.Contains(joined, "x") {
		t.Errorf("여러 줄 문자열 내용이 사라졌다: %q", joined)
	}
	res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: hookMarker("0.18.0")})
	if !strings.Contains(string(res.Out), "# keep\n}") {
		t.Errorf("주석과 그 줄의 줄바꿈이 지워졌다:\n%q", res.Out)
	}
	if !strings.Contains(string(res.Out), `CTR_MANAGED = "`+hookMarker("0.18.0")+`"`) {
		t.Errorf("표식이 현재 값으로 갱신되지 않았다:\n%q", res.Out)
	}
	if !codexTOMLParses(res.Out) {
		t.Errorf("산출이 파스되지 않는다:\n%s", res.Out)
	}
}

// TestTomlUnquoteKeyOnePair — 키 표기의 따옴표는 **한 쌍만** 벗긴다. 양끝을 전부 벗기면
// TOML이 별개 키로 읽는 표기가 우리 표식과 같아진다.
func TestTomlUnquoteKeyOnePair(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"CTR_MANAGED", "CTR_MANAGED"},
		{`"CTR_MANAGED"`, "CTR_MANAGED"},
		{`'CTR_MANAGED'`, "CTR_MANAGED"},
		{`'"CTR_MANAGED"'`, `"CTR_MANAGED"`}, // 별개 키다 — 한 쌍만 벗긴다
		{`"'CTR_MANAGED'"`, `'CTR_MANAGED'`},
		{`""CTR_MANAGED""`, `"CTR_MANAGED"`},
		{`"CTR_MANAGED'`, `"CTR_MANAGED'`}, // 짝이 다르면 벗기지 않는다
		{`"`, `"`},
		{"", ""},
	} {
		if got := tomlUnquoteKey(c.in); got != c.want {
			t.Errorf("tomlUnquoteKey(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestCodexInlineDoubleQuotedKeyIsNotMarker — 단위 테스트만 두면 호출부가 strings.Trim에 남아
// 있어도 초록이므로 **설치 수준**으로 결속한다. 홑따옴표 안의 큰따옴표 표기는 TOML에서
// 따옴표까지 포함한 별개 키이고, 그것을 우리 표식으로 읽으면 사용자 값을 덮어쓴다.
// 실측(교차모델 C3): HEAD는 userval을 표식으로 덮어썼고 base(128a727)는 표식을 따로 삽입하고
// 값을 보존했다 — 새 호출부 둘이 들인 회귀다.
func TestCodexInlineDoubleQuotedKeyIsNotMarker(t *testing.T) {
	src := "[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { '\"CTR_MANAGED\"' = \"userval\" }\n"
	res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: hookMarker("0.18.0")})
	if !strings.Contains(string(res.Out), `'"CTR_MANAGED"' = "userval"`) {
		t.Errorf("사용자 값이 표식으로 덮어써졌다:\n%q", res.Out)
	}
	if !strings.Contains(string(res.Out), `CTR_MANAGED = "`+hookMarker("0.18.0")+`"`) {
		t.Errorf("우리 표식이 따로 서지 않았다:\n%q", res.Out)
	}
	if !codexTOMLParses(res.Out) {
		t.Errorf("산출이 파스되지 않는다:\n%s", res.Out)
	}
	again := installCodexConfigBlock(res.Out, codexInstallRequest{Marker: hookMarker("0.18.0")})
	if !bytes.Equal(again.Out, res.Out) {
		t.Errorf("2회차가 바이트 동일하지 않다:\n1: %q\n2: %q", res.Out, again.Out)
	}
}

// TestCodexInlineValueSpanCommentIsRemoved — **계약 고정(회귀 방지가 아니라 표류 방지)**.
// 표식 값이 여러 물리 라인에 걸치면 그 사이의 후행 주석은 값과 함께 사라진다. 소유자 판정:
// 관리 키의 값에 붙은 주석이므로 D88이 이미 보존 대상에서 뺀 것과 같은 부류이고, 고치지
// 않는다(스펙 §2.1 P3 예외 항 · codex_toml.go 파일 머리 주석).
//
// base(128a727)는 그 줄을 통째로 보존했으므로 이 픽스처는 base에서 적색이다 — 무는 단정이다.
// **손실의 폭도 함께 못박는다**: 값 구간 밖의 주석(앞줄 독립 · 엔트리 뒤 후행)은 살아야 한다.
// 그 둘이 없으면 "주석을 더 넓게 먹는" 회귀가 이 테스트를 통과한다.
func TestCodexInlineValueSpanCommentIsRemoved(t *testing.T) {
	src := "[mcp_servers.ctr]\ncommand = \"context-router\"\n# 앞줄\nenv = { CTR_MANAGED = [1, # note\n2] } # 뒤\n"
	res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: hookMarker("0.18.0")})
	out := string(res.Out)
	if strings.Contains(out, "# note") {
		t.Errorf("값 구간 안의 주석이 남았다 — 계약이 바뀌었다면 스펙 §2.1 P3와 파일 머리 주석도 함께 고쳐라:\n%q", out)
	}
	for _, keep := range []string{"# 앞줄", "# 뒤"} {
		if !strings.Contains(out, keep) {
			t.Errorf("값 구간 **밖**의 주석 %q가 사라졌다 — 손실 폭이 넓어졌다:\n%q", keep, out)
		}
	}
	if !strings.Contains(out, `CTR_MANAGED = "`+hookMarker("0.18.0")+`"`) {
		t.Errorf("표식이 현재 값으로 갱신되지 않았다:\n%q", out)
	}
	if !codexTOMLParses(res.Out) {
		t.Errorf("산출이 파스되지 않는다:\n%s", res.Out)
	}
	again := installCodexConfigBlock(res.Out, codexInstallRequest{Marker: hookMarker("0.18.0")})
	if !bytes.Equal(again.Out, res.Out) {
		t.Errorf("2회차가 바이트 동일하지 않다:\n1: %q\n2: %q", res.Out, again.Out)
	}
}

// TestTomlTripleCloseRunLength — TOML의 실제 닫기 규칙을 실행 길이별로 잰다. 여러 줄 문자열을
// 닫는 것은 "첫 삼중 따옴표"가 아니라 **뒤에 같은 따옴표가 더 붙지 않은 첫 삼중 따옴표**다:
// 따옴표 넷은 내용이 따옴표 하나인 8바이트 토큰이고, 다섯이면 둘을 남긴다. 홑따옴표 갈래도 같다.
//
// 두 자리를 함께 잰다. `advance`는 **이어지는 줄에서** 닫는 갈래(inBasic·inLiteral)를 지나야
// 이 규칙이 사는 분기에 닿으므로 스캐너를 열린 상태로 놓고 시작한다 — 한 줄 안에서 열고 닫는
// 형태는 여는 갈래가 먼저 걸린다. 빈 여러 줄 문자열(따옴표 여섯)은 그 형태로만 표현되므로
// 열림 없이 잰다. 관측점은 **닫기 뒤 바이트가 구조로 보이는가**다: 첫 삼중에서 닫으면 따옴표
// 하나가 문자열 밖에 남아 한 줄 문자열을 열고 줄 뒤쪽을 삼켜, 닫는 중괄호도 후행 주석도
// 값 안으로 들어간다.
func TestTomlTripleCloseRunLength(t *testing.T) {
	for _, c := range []struct {
		name string
		tok  string
		want int
	}{
		{"큰따옴표 셋으로 닫기", `"""x"""`, 7},
		{"큰따옴표 넷으로 닫기", `"""x""""`, 8},
		{"큰따옴표 다섯으로 닫기", `"""x"""""`, 9},
		{"빈 여러 줄 기본 문자열", `""""""`, 6},
		{"홑따옴표 셋으로 닫기", "'''x'''", 7},
		{"홑따옴표 넷으로 닫기", "'''x''''", 8},
		{"홑따옴표 다섯으로 닫기", "'''x'''''", 9},
		{"빈 여러 줄 리터럴 문자열", "''''''", 6},
	} {
		if got := tomlTripleLen(c.tok); got != c.want {
			t.Errorf("%s: tomlTripleLen(%q)=%d want %d", c.name, c.tok, got, c.want)
		}
	}
	for _, c := range []struct {
		name        string
		line        string
		basic       bool // 줄 머리에서 여러 줄 기본 문자열이 열려 있는가
		literal     bool // 줄 머리에서 여러 줄 리터럴 문자열이 열려 있는가
		wantComment int
	}{
		{"셋으로 닫는 줄", `x""" } # c`, true, false, 7},
		{"넷으로 닫는 줄", `x"""" } # c`, true, false, 8},
		{"다섯으로 닫는 줄", `x""""" } # c`, true, false, 9},
		{"한 줄 안의 빈 기본 문자열", `a = """""" } # c`, false, false, 13},
		{"홑따옴표 셋으로 닫는 줄", "x''' } # c", false, true, 7},
		{"홑따옴표 넷으로 닫는 줄", "x'''' } # c", false, true, 8},
		{"홑따옴표 다섯으로 닫는 줄", "x''''' } # c", false, true, 9},
		{"한 줄 안의 빈 리터럴 문자열", "a = '''''' } # c", false, false, 13},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc := tomlLineScanner{inBasic: c.basic, inLiteral: c.literal, depth: 1}
			if got := sc.advance(c.line, false); got != c.wantComment {
				t.Errorf("주석 자리=%d want %d — 닫기 뒤 바이트가 문자열로 먹혔다", got, c.wantComment)
			}
			if sc.open() {
				t.Errorf("닫는 줄 뒤에도 스캐너가 열려 있다: inBasic=%v inLiteral=%v depth=%d",
					sc.inBasic, sc.inLiteral, sc.depth)
			}
		})
	}
}

// TestCodexUninstallQuadQuoteKeepsFollowingTable — **회귀 잠금**. 따옴표 넷으로 닫는 여러 줄
// 문자열이 인라인 env 안에 있으면 uninstall이 사용자의 `config.toml`을 통째로 비웠다.
//
// 기제: 탐욕적 닫기가 남긴 따옴표 하나가 한 줄 문자열을 열어 인라인 테이블의 닫는 중괄호를
// 삼키고, 그러면 depth가 1에 머물러 뒤따르는 `[other]`가 테이블 경계로 잡히지 않는다 — 우리
// 구간이 EOF까지 늘어난다. 뒤쪽의 짝 없는 `}`가 depth를 도로 0으로 내리므로 EOF 열림
// 백스톱(anomalyScannerOpen)도 서지 않아 **사유 없이 changed=true**로 빠져나갔다.
// 실측: base(128a727)는 `[other]`를 그대로 돌려준다 — 이 단정의 기대값이 그 산출이다.
func TestCodexUninstallQuadQuoteKeepsFollowingTable(t *testing.T) {
	const tail = "[other]\ncommand = \"user-thing\"\nz = 1 }\n"
	for _, c := range []struct{ name, env string }{
		{"큰따옴표 넷", "env = { A = \"\"\"x\"\"\"\" }\n"},
		{"홑따옴표 넷", "env = { A = '''x'''' }\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := "[mcp_servers.ctr]\ncommand = \"context-router\"\n" + c.env + tail
			out, changed, an := uninstallCodexConfigBlock([]byte(src))
			if !changed || an != anomalyNone {
				t.Fatalf("changed=%v anomaly=%d — 우리 소유 테이블이므로 제거 대상이다", changed, an)
			}
			if string(out) != tail {
				t.Errorf("사용자 테이블이 살아남지 않았다(base 128a727 산출과 다르다):\ngot  %q\nwant %q", out, tail)
			}
		})
	}
}

// TestCodexInstallQuadQuoteInlineEnv — **회귀 잠금**. 따옴표 넷으로 닫는 여러 줄 문자열을 담은
// 인라인 env는 유효 TOML인데(우리 게이트도 유효로 잰다) install이 `anomalyEnvNotTable`로
// 이탈했다 — 인라인 테이블을 인라인 테이블로 바꾸라는 수행 불가능한 사유였고, MCP가 확정되지
// 않아 가드 등록도 함께 빠졌다.
//
// 기제: `tomlTripleLen`이 토큰 길이를 하나 짧게 내 남은 따옴표가 값 스캔의 한 줄 문자열
// 갈래로 흘러가고, 그 토큰이 조각 경계를 넘어 `tomlScanInline`이 fail로 빠진다.
// 실측: base(128a727)는 이 파일을 기입했다. 상태·changed가 base와 같고, 표식이 서는 것은
// 이 릴리스가 의도한 차이다(D92 — base는 같은 오독 때문에 표식을 넣지 못했다).
func TestCodexInstallQuadQuoteInlineEnv(t *testing.T) {
	marker := hookMarker("0.18.0")
	for _, c := range []struct{ name, env, keepA, keepB string }{
		{"큰따옴표 넷", "env = { A = \"\"\"x\"\"\"\",\n  B = \"1\" }\n", "A = \"\"\"x\"\"\"\"", "B = \"1\""},
		{"홑따옴표 넷", "env = { A = '''x'''',\n  B = '1' }\n", "A = '''x''''", "B = '1'"},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := "[mcp_servers.ctr]\ncommand = \"context-router\"\n" + c.env
			if !codexTOMLParses([]byte(src)) {
				t.Fatalf("픽스처가 파스되지 않는다 — 유효 입력에서 재는 축이 아니게 된다")
			}
			res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: marker})
			if res.State != mcpWritten || !res.Changed {
				t.Fatalf("state=%d changed=%v 사유=%q — base(128a727)는 이 파일을 기입했다",
					res.State, res.Changed, res.Anomaly.reason())
			}
			out := string(res.Out)
			if !strings.Contains(out, codexMarkerKey+` = "`+marker+`"`) {
				t.Errorf("표식이 서지 않았다:\n%q", out)
			}
			for _, keep := range []string{c.keepA, c.keepB} {
				if !strings.Contains(out, keep) {
					t.Errorf("사용자 값 %q가 바이트 그대로 남지 않았다:\n%q", keep, out)
				}
			}
			if !codexTOMLParses(res.Out) {
				t.Errorf("산출이 파스되지 않는다:\n%s", out)
			}
			again := installCodexConfigBlock(res.Out, codexInstallRequest{Marker: marker})
			if !bytes.Equal(again.Out, res.Out) {
				t.Errorf("2회차가 바이트 동일하지 않다:\n1: %q\n2: %q", res.Out, again.Out)
			}
		})
	}
}

// TestCodexInlineMarkerEndOnFragmentBoundary — **회귀 잠금**. 값 구간의 **배타적 끝**이 조각
// 경계에 정확히 놓이면 되쓰기가 그 경계 앞의 물리 라인들을 통째로 먹었다 — 사용자의 주석
// 줄과 줄바꿈이 사라진다(실측: 첫 픽스처가 `env = { CTR_MANAGED = "…/0.18.0"      , KEEP =
// "keepme" }` 한 줄로 접혔고 state=mcpWritten·changed=true로 조용히 빠져나갔다). 네 픽스처는
// 전부 **유효 TOML**이라 D89 게이트의 대상이 아니며, base(128a727)는 넷 모두 주석·빈 줄·줄
// 종결자를 원문 그대로 보존했다 — 이 릴리스가 들인 회귀다.
//
// 기제: 값 끝이 후행 공백 절단으로 at[k]까지 당겨지면 codexPointAt이 그것을 "조각 k의 첫
// 바이트"로 풀어 (k, 0)을 낸다. 배타적 끝은 "조각 k-1의 마지막 바이트 **바로 뒤**"라는 뜻
// 이므로 앞 조각의 끝으로 풀어야 그 줄의 꼬리(잘린 주석 + 줄 종결자)가 남는다. 삽입 갈래는
// open에서 산술로 구간을 세우므로 이 경로를 타지 않는다.
//
// 단정은 **엔트리 바이트 동일**이다 — 표식 값 하나만 바뀌고 나머지 바이트는 입력 그대로여야
// 한다. 주석 문자열만 부분 검사하면 "주석은 남기고 줄바꿈은 먹는" 회귀가 통과한다.
func TestCodexInlineMarkerEndOnFragmentBoundary(t *testing.T) {
	const old, cur = "0.17.2", "0.18.0"
	for _, c := range []struct{ name, mid, eol string }{
		{"주석 줄", "# 살아남아야 하는 사용자 메모", "\n"},
		{"빈 줄", "", "\n"},
		{"주석 두 줄", "# 첫째 메모\n# 둘째 메모", "\n"},
		{"CRLF", "# 살아남아야 하는 사용자 메모", "\r\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			mid := strings.ReplaceAll(c.mid, "\n", c.eol)
			entry := func(v string) string {
				return `env = { ` + codexMarkerKey + ` = "` + hookMarker(v) + `"` + c.eol +
					mid + c.eol + `      , KEEP = "keepme" }` + c.eol
			}
			src := "[mcp_servers.ctr]" + c.eol + `command = "context-router"` + c.eol + entry(old)
			if !codexTOMLParses([]byte(src)) {
				t.Fatalf("픽스처가 파스되지 않는다 — 유효 입력에서 재는 축이 아니게 된다:\n%q", src)
			}
			res := installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: hookMarker(cur)})
			if res.State != mcpWritten || !res.Changed {
				t.Fatalf("state=%d changed=%v 사유=%q — 표식만 갱신하면 되는 형태다",
					res.State, res.Changed, res.Anomaly.reason())
			}
			if !strings.Contains(string(res.Out), entry(cur)) {
				t.Errorf("엔트리 바이트가 보존되지 않았다(주석 줄·빈 줄·줄 종결자가 먹혔다):\n want %q\n got  %q",
					entry(cur), res.Out)
			}
			if !codexTOMLParses(res.Out) {
				t.Errorf("산출이 파스되지 않는다:\n%s", res.Out)
			}
			again := installCodexConfigBlock(res.Out, codexInstallRequest{Marker: hookMarker(cur)})
			if !bytes.Equal(again.Out, res.Out) {
				t.Errorf("2회차가 바이트 동일하지 않다:\n1: %q\n2: %q", res.Out, again.Out)
			}
			// **왕복은 흔들리지 않는다** — 두 해석 모두 at[k]+col로 같은 오프셋에 돌아오므로
			// tomlSpanText와 타일링 오라클은 이 변경의 대상이 아니다. 그 사실을 이 픽스처
			// 위에서 잰다: 오라클이 적색이 되면 오라클이 아니라 이 해석이 틀린 것이다.
			lines := splitLinesKeepEnds([]byte(src))
			e := [2]int{2, len(lines) - 1} // 0행 헤더 · 1행 command
			sc := tomlScanInline(lines, e)
			if !sc.ok {
				t.Fatalf("열거가 실패했다 — 유효 인라인 테이블이다: %+v", sc)
			}
			assertInlineScanTiles(t, lines, e, sc)
			joined, at := codexEntryRaw(lines, e)
			wantVals := []string{`"` + hookMarker(old) + `"`, `"keepme"`}
			if len(sc.entries) != len(wantVals) {
				t.Fatalf("마디 수=%d want %d", len(sc.entries), len(wantVals))
			}
			for i, en := range sc.entries {
				if got := tomlSpanText(joined, at, e, en.value); got != wantVals[i] {
					t.Errorf("마디[%d] 값 왕복=%q want %q — 배타적 끝의 새 해석이 오프셋을 옮겼다", i, got, wantVals[i])
				}
			}
		})
	}
}
