package cli

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// assertInlineScanTiles — P0. 열거 결과가 논리 엔트리를 **구조 문자만 사이에 두고** 덮는가.
// 종전 본문("구간을 사이 바이트와 함께 다시 이으면 원문이다")은 무는 불변식이 아니다:
// joined[prev:s] + joined[s:x] == joined[prev:x]이므로 재구성은 **어떤 구간 열에 대해서도
// 항상 joined와 같고**, 의도적 +1이 아홉 케이스 전부 통과한다(session-49 리허설 실측).
//
// 진짜 불변식은 rowan의 "자식 폭의 합 = 부모 폭"을 이 문법에 맞게 옮긴 것 —
// **구간 사이의 바이트가 그 자리에 올 수 있는 구조 문자뿐인가**이다.
//
//	{ ␣ 키 ␣=␣ 값 ␣,␣ 키 ␣=␣ 값 ␣[,]␣ }
//
// 여는 중괄호와 첫 키 사이는 공백뿐 · 키와 값 사이는 '=' 정확히 하나 · 엔트리 사이는 ','
// 정확히 하나 · 마지막 값과 닫는 중괄호 사이는 ',' 0개나 1개(TOML 1.1.0의 후행 쉼표).
// 구간 자체는 비지 않고 공백·','·'='로 시작하거나 끝나지 않는다 — TOML의 어떤 값도 ','로
// 끝나지 않는다(문자열은 '"', 배열은 ']', 중첩 인라인은 '}').
//
// 실측(돌연변이 아홉 × 픽스처 여덟): value.end±1 · value.start+1 · key.end+1 · key.start-1 ·
// 엔트리 누락 · 후행 쉼표 유령 엔트리 · close+1을 **전부** 문다. 종전 본문은 그중 하나도
// 물지 않았다. 임포트에 fmt를 더한다.
func assertInlineScanTiles(t *testing.T, lines [][]byte, e [2]int, sc tomlInlineScan) {
	t.Helper()
	if !sc.ok {
		if len(sc.entries) != 0 {
			t.Errorf("ok=false인데 entries가 %d개다 — 부분 산출 금지(계약 4)", len(sc.entries))
		}
		return
	}
	joined, at := codexEntryRaw(lines, e)

	// off — 파일 좌표를 joined 오프셋으로. 무효 지점·범위 밖을 여기서 잡는다: 방어가 없으면
	// 틀린 좌표가 slice bounds out of range 패닉이 되어 실패 메시지 대신 스택이 나온다(실측).
	off := func(what string, p tomlPoint) int {
		if !p.valid() {
			t.Fatalf("%s가 무효 지점이다: %+v", what, p)
		}
		k := p.line - e[0]
		if k < 0 || k >= len(at) {
			t.Fatalf("%s의 라인 %d이 엔트리 %v 밖이다", what, p.line, e)
		}
		o := at[k] + p.col
		if o < 0 || o > len(joined) {
			t.Fatalf("%s의 오프셋 %d이 joined 길이 %d 밖이다", what, o, len(joined))
		}
		return o
	}

	// gap — [lo,hi)가 공백과 sep뿐이고 sep을 [minN,maxN]개 담는가. sep=0이면 공백만 허용한다.
	gap := func(what string, lo, hi int, sep byte, minN, maxN int) {
		if lo > hi {
			t.Fatalf("%s: 구간이 뒤집혔다 [%d,%d)", what, lo, hi)
		}
		n := 0
		for i := lo; i < hi; i++ {
			c := joined[i]
			switch {
			case c == ' ' || c == '\t':
			case sep != 0 && c == sep:
				n++
			default:
				t.Fatalf("%s: 구간 사이에 구조 문자가 아닌 %q가 있다 — %q", what, c, joined[lo:hi])
			}
		}
		if n < minN || n > maxN {
			t.Fatalf("%s: 구분자 %q가 %d개다 want [%d,%d] — %q", what, sep, n, minN, maxN, joined[lo:hi])
		}
	}

	// tight — 구간은 비지 않고 공백·','·'='를 물지 않는다.
	tight := func(what string, lo, hi int) {
		if lo >= hi {
			t.Fatalf("%s: 구간이 비었다 [%d,%d)", what, lo, hi)
		}
		if strings.IndexByte(" \t,=", joined[lo]) >= 0 {
			t.Fatalf("%s: 구간이 선행 구조 문자를 물었다 — %q", what, joined[lo:hi])
		}
		if strings.IndexByte(" \t,=", joined[hi-1]) >= 0 {
			t.Fatalf("%s: 구간이 후행 구조 문자를 물었다 — %q", what, joined[lo:hi])
		}
	}

	o, c := off("open", sc.open), off("close", sc.close)
	if o >= len(joined) || joined[o] != '{' {
		t.Fatalf("open이 여는 중괄호를 가리키지 않는다: off=%d %q", o, joined)
	}
	if c >= len(joined) || joined[c] != '}' {
		t.Fatalf("close가 닫는 중괄호를 가리키지 않는다: off=%d %q", c, joined)
	}
	if len(sc.entries) == 0 {
		gap("빈 테이블", o+1, c, 0, 0, 0)
		return
	}
	prev := o + 1 // 여는 중괄호 **뒤**부터가 내용이다
	for i, en := range sc.entries {
		ks, ke := off("키 시작", en.key.start), off("키 끝", en.key.end)
		vs, ve := off("값 시작", en.value.start), off("값 끝", en.value.end)
		tight(fmt.Sprintf("엔트리[%d] 키", i), ks, ke)
		tight(fmt.Sprintf("엔트리[%d] 값", i), vs, ve)
		if i == 0 {
			gap("여는 중괄호와 첫 키 사이", prev, ks, 0, 0, 0)
		} else {
			gap(fmt.Sprintf("엔트리[%d]와 [%d] 사이", i-1, i), prev, ks, ',', 1, 1)
		}
		gap(fmt.Sprintf("엔트리[%d]의 키와 값 사이", i), ke, vs, '=', 1, 1)
		prev = ve
	}
	gap("마지막 값과 닫는 중괄호 사이", prev, c, ',', 0, 1)
}

// tomlFlatten — 점 경로 → 잎 값의 정본 문자열. 헤더·점 표기·인라인 세 형태가 **같은 경로로
// 접힌다** — 형태가 달라도 의미가 같다는 것을 재는 오라클이다. 지정 파서는 우리 판독기가
// 아닌 독립 구현이라 차등 오라클로서 정당하고, D89가 검증 전용 사용을 이미 허용한다.
// 값은 %v로 적는다: 표기 차이(1_000 vs 1000, 'x' vs "x")를 파서가 이미 흡수하므로 P1은
// 의미만 보고, 표기는 P3가 원문 라인 생존으로 본다.
func tomlFlatten(b []byte) (map[string]string, error) {
	var doc map[string]any
	if err := toml.Unmarshal(trimBOM(b), &doc); err != nil {
		return nil, err
	}
	out := map[string]string{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		m, ok := v.(map[string]any)
		if !ok {
			out[prefix] = fmt.Sprintf("%v", v)
			return
		}
		if len(m) == 0 {
			out[prefix] = "{}" // 빈 테이블도 실재하는 정의다 — 소실을 보려면 잎이어야 한다
			return
		}
		for k, sub := range m {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			walk(p, sub)
		}
	}
	walk("", doc)
	return out, nil
}

// assertSemanticPreserved — P1. 입력과 산출의 평탄화 차집합이 허용 경로 안에 드는가.
// **입력이 파스되지 않으면 건너뛴다** — D89의 비대칭 계약과 같은 형태다. 산출이 파스되지
// 않으면 그것 자체가 실패다(입력이 유효했으므로 우리가 깬 것이다).
func assertSemanticPreserved(t *testing.T, in, out []byte, allowed ...string) {
	t.Helper()
	before, err := tomlFlatten(in)
	if err != nil {
		return // 무효 입력 — 비대칭 계약(D89)
	}
	after, err := tomlFlatten(out)
	if err != nil {
		t.Fatalf("유효 입력의 산출이 파스되지 않는다: %v\n%s", err, out)
	}
	ok := func(path string) bool { return slices.Contains(allowed, path) }
	for k, v := range before {
		if w, present := after[k]; !present {
			if !ok(k) {
				t.Errorf("경로가 사라졌다: %s = %q", k, v)
			}
		} else if w != v && !ok(k) {
			t.Errorf("값이 바뀌었다: %s: %q → %q", k, v, w)
		}
	}
	for k, w := range after {
		if _, present := before[k]; !present && !ok(k) {
			t.Errorf("경로가 새로 생겼다: %s = %q", k, w)
		}
	}
}

// tomlComments — P2. 문자열 밖 '#'부터 줄 끝까지를 모은다(여러 줄 문자열 상태를 이월한다).
// 정렬해 비교하므로 위치는 바뀌어도 내용은 안 바뀐다. **파서를 쓰지 않는다** — 게이트와
// 오라클이 같은 파서라 맹점이 동시에 꺼지는 함정을 이 한 겹이 해소한다.
// tomlLineScanner를 재사용하는 것은 codexEntryRaw와 같은 이유다: 새 상태 기계를 세우면
// 오라클이 판독기와 다른 문법을 갖게 되어 무엇이 틀렸는지 갈린다.
//
// 술어는 open()이 아니라 **inString()**이다(스펙 §0 D92 계약 6, codexEntryRaw와 같은 기준) —
// 인라인 깊이가 depth에 든 뒤로 open()은 여러 줄 배열·인라인 테이블의 이어지는 줄에서도
// 참이고, 그 줄의 후행 주석은 진짜 주석이라 여기서 세어야 한다.
func tomlComments(b []byte) []string {
	var out []string
	var sc tomlLineScanner
	for _, line := range splitLinesKeepEnds(b) {
		s := trimEOL(line)
		if !sc.inString() {
			if cut := stripTrailingComment(s); len(cut) < len(s) {
				out = append(out, s[len(cut):])
			}
		}
		sc.step(line)
	}
	slices.Sort(out)
	return out
}

// assertCommentsPreserved — P2. 주석 다중집합이 보존되는가. D88의 예외 하나만 허용한다:
// **우리가 재생성하는 관리 키 줄의 후행 주석은 보존 대상이 아니다.** 그래서 "사라진 것만"
// 본다 — 우리는 주석을 만들지 않으므로 새로 생기는 쪽은 곧 결함이고 그것도 함께 잡는다.
func assertCommentsPreserved(t *testing.T, in, out []byte, allowedLost ...string) {
	t.Helper()
	before, after := tomlComments(in), tomlComments(out)
	lost := map[string]int{}
	for _, c := range before {
		lost[c]++
	}
	for _, c := range after {
		lost[c]--
	}
	for c, n := range lost {
		switch {
		case n > 0 && !slices.Contains(allowedLost, c):
			t.Errorf("주석이 %d개 사라졌다: %q", n, c)
		case n < 0:
			t.Errorf("주석이 %d개 새로 생겼다: %q", -n, c)
		}
	}
}

// assertLineSurvival — P3. 우리 관리 키가 아닌 물리 라인은 산출에 **바이트 동일**하게 있어야
// 한다. P1이 통과시키는 표기 변화(1_000→1000, 'x'→"x", 들여쓰기·후행 주석 소실)를 여기서
// 잡는다.
//
// **예외는 인라인 env 논리 엔트리 하나다** — 그 엔트리는 보존 라인이면서 표식이 제자리
// 치환되는 유일한 자리다(codexTableBody의 keep 루프가 view.inlineEnv에서 setInlineEnvMarker를
// 지난다). Task 6 이후 그 엔트리는 **여러 줄일 수 있으므로 예외를 라인 하나로 받지 않고
// 구간 [2]int로 받는다.** 예외를 적지 않으면 P3가 늘 실패하거나, 반대로 느슨해져 다른 줄의
// 변조까지 통과시킨다. 무효값은 (-1,-1)이다.
func assertLineSurvival(t *testing.T, in, out []byte, envEntry [2]int) {
	t.Helper()
	inLines, outLines := splitLinesKeepEnds(in), splitLinesKeepEnds(out)
	managed := func(line []byte) bool {
		switch codexKeyName(trimEOL(line)) {
		case "command", "args", "enabled_tools", codexMarkerKey:
			return true
		}
		return strings.HasPrefix(strings.TrimSpace(trimEOL(line)), "[") // 헤더는 재생성 대상이다
	}
	for i, line := range inLines {
		if managed(line) {
			continue
		}
		if envEntry[0] >= 0 && i >= envEntry[0] && i <= envEntry[1] {
			continue // 인라인 env 엔트리 — 치환 구간을 담으므로 바이트 동일을 요구하지 않는다
		}
		if !slices.ContainsFunc(outLines, func(o []byte) bool { return bytes.Equal(o, line) }) {
			t.Errorf("보존 라인 %d이 산출에서 바이트 동일하게 살아 있지 않다: %q", i, line)
		}
	}
}

// codexEnvEntryOf — 입력에서 인라인 env **논리 엔트리** 구간을 찾는다. 없으면 (-1,-1)이다.
// P0와 P3가 그 구간을 각각 검사 대상·예외로 받으므로 격자 테스트가 케이스마다 부른다.
func codexEnvEntryOf(b []byte) (lines [][]byte, e [2]int) {
	lines = splitLinesKeepEnds(b)
	sp := codexManagedSpans(lines)
	if sp.anomaly != anomalyNone || !sp.table.found {
		return lines, [2]int{-1, -1}
	}
	return lines, codexReadTable(lines, sp.table).inlineEnvEntry
}

// codexLattice — 축의 곱. property-based 라이브러리를 새로 들이지 않는다(D8 의존 예산).
// 재현성이 공짜이고 실패 케이스가 곧 최소 케이스라 shrink가 필요 없다.
// 축은 스펙 §2.3 다섯 줄과 1:1이다 — env 형태 · 사용자 키(표기 포함) · 값 종류 · 주석 ·
// 바이트(줄 종결자 · 마지막 줄 종결자 유무 · 선두 BOM · 들여쓰기).
func codexLattice() []string {
	const marker = "context-router/0.15.0"
	envForms := []string{
		"[mcp_servers.ctr.env]\nCTR_MANAGED = \"" + marker + "\"\n",
		"env = { CTR_MANAGED = \"" + marker + "\" }\n",
		"env = { CTR_MANAGED = \"" + marker + "\",\n  U = \"1\" }\n",
		"env.CTR_MANAGED = \"" + marker + "\"\n",
		"env = []\n",
	}
	// 키 표기 — bare · "basic" · 'literal' · A.B · 공백 담은 따옴표(스펙 §2.3 둘째 줄)
	userKeys := []string{
		"",
		"U1 = \"v\"\n",
		"\"U2\" = \"v\"\n'U3' = \"v\"\n",
		"A.B = \"v\"\n\"u v\" = \"w\"\n",
	}
	// 값 종류 — '#' 포함 · ',' 포함 · 정수 · 여러 줄 배열 · 중첩 인라인(스펙 §2.3 셋째 줄)
	userValues := []string{
		"",
		"V1 = \"a#b\"\nV2 = \"x,y\"\n",
		"V3 = 1000\nV4 = [\n  \"p\",\n  \"q\",\n]\n",
		"V5 = { inner = \"z\" }\n",
	}
	// 주석 — 없음 · 후행 · 앞줄 독립 · **둘 다**(스펙 §2.3 넷째 줄)
	comments := [][2]string{{"", ""}, {"", " # 후행"}, {"# 앞줄\n", ""}, {"# 앞줄\n", " # 후행"}}
	eols := []string{"\n", "\r\n"}
	boms := []string{"", "\xEF\xBB\xBF"}
	finalEOL := []bool{true, false} // 마지막 줄 종결자 유무
	indents := []string{"", "  "}   // 들여쓰기 유무

	var out []string
	for _, env := range envForms {
		for _, keys := range userKeys {
			for _, vals := range userValues {
				for _, cm := range comments {
					for _, eol := range eols {
						for _, bom := range boms {
							for _, fin := range finalEOL {
								for _, ind := range indents {
									body := cm[0] + "[mcp_servers.ctr]\ncommand = \"context-router\"" + cm[1] + "\n" + keys + vals + env
									// 들여쓰기는 헤더가 아닌 키 줄에만 준다 — 헤더를 들여쓰면
									// 그 자체가 다른 축(헤더 판정)이고 이 격자의 대상이 아니다.
									if ind != "" {
										var b strings.Builder
										for _, ln := range strings.SplitAfter(body, "\n") {
											if ln != "" && !strings.HasPrefix(ln, "[") && !strings.HasPrefix(ln, "#") {
												b.WriteString(ind)
											}
											b.WriteString(ln)
										}
										body = b.String()
									}
									if eol == "\r\n" {
										body = strings.ReplaceAll(body, "\n", "\r\n")
									}
									if !fin {
										body = strings.TrimSuffix(body, eol)
									}
									out = append(out, bom+body)
								}
							}
						}
					}
				}
			}
		}
	}
	return out
}

// TestCodexLatticeSize — 축의 곱이 스펙 §2.3과 같은 폭인가. 5·4·4·4·2·2·2·2 = 5120이다.
func TestCodexLatticeSize(t *testing.T) {
	if got, want := len(codexLattice()), 5*4*4*4*2*2*2*2; got != want {
		t.Errorf("격자 케이스 수=%d want %d — 축이 줄었다", got, want)
	}
}

// TestCodexLatticeOracles — 격자의 모든 케이스에 P0·P1·P2·P3·P4를 **다섯 겹 모두** 건다.
// P0와 P3를 부르지 않으면 그 두 겹이 격자 위에서 한 번도 실행되지 않는다(리뷰 실측).
// 입력이 파스되지 않으면 P1만 건너뛴다(D89 비대칭). 실패 시 케이스 원문을 인쇄해 최소
// 재현을 즉시 얻는다.
func TestCodexLatticeOracles(t *testing.T) {
	marker := hookMarker("0.18.0")
	for i, src := range codexLattice() {
		t.Run(fmt.Sprintf("case%04d", i), func(t *testing.T) {
			in := []byte(src)
			res := installCodexConfigBlock(in, codexInstallRequest{Marker: marker})
			// P0 — 인라인 env 엔트리가 있으면 열거 결과가 그 엔트리를 구조 문자만 사이에 두고
			// 덮는다. 입력과 산출 **양쪽**에 건다: 산출에만 걸면 우리가 만든 형태만 보고,
			// 입력에만 걸면 우리 되쓰기가 만든 어긋남을 못 본다.
			for _, b := range [][]byte{in, res.Out} {
				if lines, e := codexEnvEntryOf(b); e[0] >= 0 {
					assertInlineScanTiles(t, lines, e, tomlScanInline(lines, e))
				}
			}
			// P1 — 허용 경로는 우리가 기입하는 넷뿐이다.
			assertSemanticPreserved(t, in, res.Out, "mcp_servers.ctr.env."+codexMarkerKey,
				"mcp_servers.ctr.command", "mcp_servers.ctr.args", "mcp_servers.ctr.enabled_tools")
			// P2 — 주석 다중집합. D88의 예외(우리가 재생성하는 관리 키 줄의 후행 주석)를
			// 허용 목록으로 넘긴다: 격자의 후행 주석은 command 줄에 붙어 있다.
			// tomlComments가 '#'부터 잘라 내므로 앞 공백은 들어가지 않는다.
			assertCommentsPreserved(t, in, res.Out, "# 후행")
			// P3 — 우리 관리 키가 아닌 물리 라인의 바이트 생존. 예외는 인라인 env 엔트리다.
			_, envEntry := codexEnvEntryOf(in)
			assertLineSurvival(t, in, res.Out, envEntry)
			// P4 — 멱등.
			again := installCodexConfigBlock(res.Out, codexInstallRequest{Marker: marker})
			if !bytes.Equal(again.Out, res.Out) {
				t.Errorf("멱등이 아니다:\n입력: %q\n1: %s\n2: %s", src, res.Out, again.Out)
			}
		})
	}
}
