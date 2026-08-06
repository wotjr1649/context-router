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

// tomlEmptyTableLeaf — tomlFlatten이 **빈 테이블**을 적는 잎 값. 문면을 두 자리에 적지 않는다:
// assertCodexOracles가 "입력의 env가 빈 테이블인가"를 이 값으로 판정하므로, 문면이 갈리면 그
// 판정이 조용히 거짓이 되고 빈 인라인 env가 다시 그물 밖으로 나간다.
var tomlEmptyTableLeaf = fmt.Sprintf("%T:{}", map[string]any{})

// tomlFlatten — 점 경로 → 잎 값의 정본 문자열. 헤더·점 표기·인라인 세 형태가 **같은 경로로
// 접힌다** — 형태가 달라도 의미가 같다는 것을 재는 오라클이다. 지정 파서는 우리 판독기가
// 아닌 독립 구현이라 차등 오라클로서 정당하고, D89가 검증 전용 사용을 이미 허용한다.
//
// 값은 **`%T:%v`**로 적는다 — 표기가 아니라 **타입과 값**이다. `%v`만 적으면 정수 1과
// 문자열 "1"이 같은 `1`로 접혀 타입 변조가 P1을 통과하는데, 그 자리는 P3의 인라인 env
// 예외 구간 안에서 **P1이 유일한 겹**이라 그물 전체에 구멍이 난다(리뷰 실측: 실제 격자
// 산출의 `U = "1"`을 `U = 1`로 바꾸면 다섯 겹과 산출물 유효성 게이트를 모두 통과했다).
// 같은 충돌이 `a=1`과 `a="1"`과 `a=1.0` 사이, `a=true`와 `a="true"` 사이에도 있었다.
//
// **표기는 여전히 P3의 몫이다**: 1_000과 1000은 둘 다 `int64:1000`, 'x'와 "x"는 둘 다
// `string:x`로 접히므로 타입 구분만 더해질 뿐 표기 차이는 그대로 흡수된다.
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
			out[prefix] = fmt.Sprintf("%T:%v", v, v)
			return
		}
		if len(m) == 0 {
			// 빈 테이블도 실재하는 정의다 — 소실을 보려면 잎이어야 한다. 타입 접두를 잎과
			// **같은 규칙**으로 붙인다: 맨 문자열 "{}"로 적으면 값이 문자열 "{}"인 키와
			// 겹쳐 빈 테이블과 문자열 사이의 변조가 통과한다.
			out[prefix] = tomlEmptyTableLeaf
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
//
// **상태는 줄 머리가 아니라 줄 안에서 본다**(session-53 정정, codexEntryRaw와 같은 정정).
// 종전은 `sc.inString()`을 **걸음 전에** 물어 참이면 그 물리 라인을 통째로 건너뛰었다 —
// 여러 줄 문자열이 **닫히는 그 줄**은 줄 머리가 아직 문자열 안이므로 통째로 빠졌고, 그 줄의
// 후행 주석이 입력에서도 산출에서도 세어지지 않았다. 실측: `env = { M = """`⏎`line`⏎
// `""" } # keep this note`에서 P2는 주석 0개를 보고, 그 주석을 산출에서 지워도 여전히 0개다 —
// 이 겹의 존재 이유인 손실이 이 겹에 보이지 않았다.
//
// 고치는 기제는 새로 세우지 않는다: `tomlLineScanner.advance`가 이미 **문자열 밖 '#' 자리**를
// 돌려주므로(이 릴리스가 codexEntryRaw 때문에 들인 반환값), 상태를 버리는 복사본에 그 줄을
// 걸려 자리만 받고 상태 갱신은 종전대로 step이 한다. 오라클이 판독기와 다른 문법을 갖지
// 않는다는 이 파일의 기준이 그대로 유지된다.
func tomlComments(b []byte) []string {
	var out []string
	var sc tomlLineScanner
	for _, line := range splitLinesKeepEnds(b) {
		s := trimEOL(line)
		head := sc // 줄 머리 상태의 복사본 — 주석 자리만 받고 상태 갱신은 step이 한다
		if at := head.advance(s, false); at < len(s) {
			out = append(out, s[at:])
		}
		sc.step(line)
	}
	slices.Sort(out)
	return out
}

// TestCommentsOnMultilineStringCloseLine — **P2 자신을 재는 자리.** 여러 줄 문자열이 닫히는
// 물리 라인의 후행 주석을 이 겹이 세는가. 종전 판정(줄 머리 상태로 줄을 통째로 건너뜀)은 그
// 주석을 입력에서도 산출에서도 세지 않아, 산출에서 지워져도 다중집합 차이가 0이었다 —
// **주석 손실을 잡으라고 있는 겹이 바로 그 손실에 눈이 없었다**(session-53 실측).
// 다른 네 겹도 이 손실에 눈이 없다: P1은 주석을 모델하지 않고, P0는 인라인 env 엔트리의
// 구조만 보며, P3는 그 엔트리를 통째로 면제하고, P4는 주석이 빠진 산출 위에서도 성립한다.
func TestCommentsOnMultilineStringCloseLine(t *testing.T) {
	const note = "# keep this note"
	in := []byte("[mcp_servers.ctr]\ncommand = \"context-router\"\n" +
		"env = { " + codexMarkerKey + " = \"context-router/0.17.2\", M = \"\"\"\nline\n\"\"\" } " + note + "\n")
	before := tomlComments(in)
	if len(before) != 1 || before[0] != note {
		t.Fatalf("닫는 줄의 후행 주석=%q want [%q] — 그 물리 라인이 통째로 건너뛰어졌다", before, note)
	}
	// 그 주석만 지운 산출 — P2가 이 차이를 보아야 한다(assertCommentsPreserved는 이 두
	// 다중집합의 차집합을 그대로 읽는다).
	after := tomlComments(bytes.ReplaceAll(in, []byte(" "+note), nil))
	if len(after) != 0 {
		t.Fatalf("주석을 지운 산출에서 %q가 여전히 세어진다", after)
	}
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

// lineSurvivalLosses — P3의 판정부. **t를 받지 않는다**: t.Errorf를 부르는 함수는 자기 실패를
// 관측할 수 없어 오라클 자신을 재는 테스트가 설 수 없는데, P3는 이 릴리스가 들인 줄 건너뛰기
// 논리 바로 위에 서 있어 그 겹 자체가 무는지 확인할 자리가 필요하다. 돌려주는 것은 산출에서
// 살아남지 못한 **입력 라인 인덱스**다.
//
// **개수까지 세는 다중집합이다.** 종전은 "산출 어딘가에 바이트 동일한 줄이 하나라도 있는가"라
// 바이트가 같은 보존 라인 둘 중 하나가 사라져도 남은 하나가 둘을 모두 통과시켰다(빈 줄 둘,
// 같은 주석 두 줄이 실제 형태다). 다른 네 겹도 그 손실을 못 본다 — P1은 빈 줄을 모델하지
// 않고, P2는 '#' 줄만 세며, P0는 인라인 env 엔트리만 보고, P4는 이미 상한 산출 위에서도
// 성립한다. **물리 라인당 바이트 동일이 계약인 겹은 P3뿐이다.** 개수 맵은 조회가 O(1)이라
// 계획이 경고한 O(n²) 스캔도 함께 없어진다.
func lineSurvivalLosses(in, out []byte, envEntry [2]int) []int {
	inLines, outLines := splitLinesKeepEnds(in), splitLinesKeepEnds(out)
	avail := make(map[string]int, len(outLines))
	for _, o := range outLines {
		avail[string(o)]++
	}
	// 면제는 **우리 두 구간 안에서만** 준다 — 키 이름도, 헤더도. 파일 전역으로 주면 남의
	// `[mcp_servers.other]`에 있는 command·args·enabled_tools·표식 키 줄이 바이트 생존
	// 검사에서 통째로 빠진다 — 실제 Codex config는 등록물마다 command를 하나씩 가지므로
	// 좁지만 실재하는 구멍이고, 격자는 둘째 테이블이 없어 이것을 구조적으로 못 본다.
	// 헤더 면제도 같은 이유로 같은 폭이다: 파일 전역이면 사용자의 `[model]` 헤더도,
	// `["a", "b"],` 같은 배열의 이어지는 줄도 어디서 사라지든 이 겹이 보지 못한다.
	//
	// 구간의 end는 **배타적**이다(spliceCodexLines가 `i = e.end`에서 원문 복사를 재개한다).
	// `<=`로 받으면 우리 구간 **다음** 테이블의 헤더 줄이 면제에 들어가 바로 그 `[model]`
	// 구멍이 남는다. 키 이름 갈래는 이 정정에 영향을 받지 않는다 — 그 인덱스의 줄은 헤더이고
	// 헤더는 codexKeyName이 늘 빈 이름으로 낸다.
	sp := codexManagedSpans(inLines)
	inSpan := func(s codexSpan, i int) bool { return s.found && i >= s.start && i < s.end }
	managed := func(i int, line []byte) bool {
		if !inSpan(sp.table, i) && !inSpan(sp.env, i) {
			return false
		}
		// 헤더 면제는 **우리 이름의 헤더**에만 준다(session-53). 종전은 "구간 안에서 `[`로
		// 시작하는 줄"이었는데, 우리 구간은 판독기가 인라인 테이블을 닫지 못하면 EOF까지
		// 늘어나므로 그 넓힘이 **남의 `[other]` 헤더까지 재생성 대상으로 면제한다** —
		// 바로 그 넓힘이 남의 테이블을 삼키는 결함의 형태이고, 이 겹이 그것을 못 보게 된다.
		// 이름 판독은 codexManagedSpans가 쓰는 것과 **같은 자리**(stripLine 뒤 tomlHeaderName)다 —
		// 갈리면 오라클과 판독기가 같은 파일을 두 기준으로 읽는다.
		switch tomlHeaderName(stripLine(line)) {
		case codexManagedTable, codexManagedEnv:
			return true // 우리 구간의 헤더는 재생성 대상이다
		}
		switch codexKeyName(trimEOL(line)) {
		case "command", "args", "enabled_tools", codexMarkerKey:
			return true
		}
		return false
	}
	// 면제의 끝은 논리 엔트리의 끝이 아니라 **엔트리가 삼킨 첫 테이블 헤더의 앞**이다
	// (session-53). 면제의 근거는 "표식 splice가 이 엔트리 안을 정당하게 되쓴다"인데, 테이블
	// 헤더 줄을 담은 엔트리는 정당한 엔트리가 아니다 — 판독기가 인라인 테이블을 닫지 못해 그
	// 뒤 사용자 테이블을 엔트리 안으로 끌어들인 상태이고, 면제가 그 사용자 라인까지 덮으면
	// **바이트 손실을 잡으라고 있는 겹이 바로 그 손실에 침묵한다.** 실측: `env = { X = "v"`⏎⏎
	// `[other]`⏎`uKEY = "uV" }`에서 산출이 `[other]`와 그 키를 통째로 잃는데 손실 목록이 비었다.
	exemptEnd := envEntry[1]
	if h := codexEntryHeaderLine(inLines, envEntry); h >= 0 {
		exemptEnd = h - 1
	}
	var lost []int
	for i, line := range inLines {
		if managed(i, line) {
			continue
		}
		if envEntry[0] >= 0 && i >= envEntry[0] && i <= exemptEnd {
			continue // 인라인 env 엔트리 — 치환 구간을 담으므로 바이트 동일을 요구하지 않는다
		}
		if avail[string(line)] == 0 {
			lost = append(lost, i)
			continue
		}
		avail[string(line)]--
	}
	return lost
}

// codexEntryHeaderLine — 논리 엔트리 [e0,e1]이 삼킨 **테이블 헤더 줄**의 첫 인덱스(없으면 -1).
// 엔트리 **둘째 줄부터** 본다: 첫 줄은 `env = {` 대입 줄이고 그 앞의 헤더는 엔트리 밖이다.
//
// 판정은 "공백을 지운 줄이 `[`로 시작해 `]`로 끝난다"이다(후행 주석은 먼저 뗀다) — `[other]`도
// `[[a.b]]`도 `[other] # 메모`도 물고, 인라인 값 안의 배열 이어지는 줄 `["a", "b"],`는 쉼표로
// 끝나 물지 않는다. **`tomlScanInline`으로 판정하지 않는다**: 이 형태에서 그 열거기가 open을
// 유효로 내는 것이 바로 다음 태스크가 고칠 결함이고, 판정을 거기 걸면 게이트와 오라클이 같은
// 눈을 공유해 맹점이 동시에 꺼진다(이 파일이 P2에서 파서를 피하는 것과 같은 이유).
//
// ponytail: 값 하나가 통째로 `[1, 2]`인 줄이 엔트리 안에 홀로 놓이면 오경보다. env 값은
// 환경변수 문자열이라 실재하지 않는 모양이고, 오경보는 적색 + 메시지로 드러난다 — 필요해지면
// 그때 "헤더 이름이 정규화 가능한 키 경로인가"로 좁힌다.
func codexEntryHeaderLine(lines [][]byte, e [2]int) int {
	if e[0] < 0 {
		return -1
	}
	for i := e[0] + 1; i <= e[1] && i < len(lines); i++ {
		t := stripTrailingComment(stripLine(lines[i]))
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			return i
		}
	}
	return -1
}

// TestLineSurvivalEntryExemptionStopsAtHeader — **P3 자신을 재는 자리.** 인라인 env 엔트리
// 면제가 테이블 헤더 줄 앞에서 끊기는가. 입력·산출을 손으로 적어 두는 것은 `installCodexConfigBlock`
// 을 부르지 않기 위해서다 — 이 테스트는 **오라클의 계약**을 잠그는 것이고, 다음 태스크가
// 생산 코드에서 이 소실을 고쳐도 그대로 초록이어야 한다(형제인 TestLineSurvivalCountsDuplicates·
// TestLineSurvivalHeaderExemptionIsScoped와 같은 형태).
func TestLineSurvivalEntryExemptionStopsAtHeader(t *testing.T) {
	in := []byte("[mcp_servers.ctr]\ncommand = \"context-router\"\n" +
		"env = { " + codexMarkerKey + " = \"context-router/0.17.2\"\n\n[other]\nuKEY = \"uV\" }\n")
	// **생산 쪽은 이제 이 형태를 아예 읽지 않는다**(session-54): 인라인 테이블이 열린 채
	// 헤더 모양의 줄이 오면 구간의 끝을 확정할 수 없으므로 codexManagedSpans가 사유를 세우고
	// 논리 엔트리가 서지 않는다. 그래도 이 겹의 좁힘은 **남긴다** — 면제가 헤더 앞에서 끊기지
	// 않으면, 판독기가 다시 그런 엔트리를 내는 날 P3가 바로 그 손실에 눈을 감는다.
	if _, e := codexEnvEntryOf(in); e != [2]int{-1, -1} {
		t.Fatalf("논리 엔트리=%v want [-1 -1] — 구간을 확정할 수 없는 형태를 판독기가 다시 읽고 있다", e)
	}
	// 사용자 테이블을 통째로 잃은 산출. 면제가 엔트리 전체면 손실 0건이다.
	out := []byte("[mcp_servers.ctr]\ncommand = \"context-router\"\n" +
		"env = { " + codexMarkerKey + " = \"context-router/0.18.0\" }\n")
	lost := lineSurvivalLosses(in, out, [2]int{2, 5})
	if !slices.Equal(lost, []int{4, 5}) {
		t.Errorf("잃은 라인=%v want [4 5] — 면제가 헤더 줄과 그 뒤까지 덮고 있다", lost)
	}
	// 반대쪽도 잰다: 헤더를 삼키지 않은 정상 엔트리의 면제는 그대로 남아야 한다.
	ok := []byte("[mcp_servers.ctr]\ncommand = \"context-router\"\n" +
		"env = { " + codexMarkerKey + " = \"context-router/0.17.2\",\n  U = \"1\" }\n")
	okOut := []byte("[mcp_servers.ctr]\ncommand = \"context-router\"\n" +
		"env = { " + codexMarkerKey + " = \"context-router/0.18.0\", U = \"1\" }\n")
	if lost := lineSurvivalLosses(ok, okOut, [2]int{2, 3}); len(lost) != 0 {
		t.Errorf("정상 엔트리의 면제가 사라졌다: %v", lost)
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
	inLines := splitLinesKeepEnds(in)
	for _, i := range lineSurvivalLosses(in, out, envEntry) {
		t.Errorf("보존 라인 %d이 산출에서 바이트 동일하게 살아 있지 않다(같은 바이트의 줄은 개수까지 센다): %q", i, inLines[i])
	}
}

// TestLineSurvivalCountsDuplicates — **오라클 자신을 재는 자리.** 바이트가 같은 보존 라인이
// 둘일 때 하나가 사라지면 P3가 물어야 한다. 종전 판정("산출 어딘가에 바이트 동일한 줄이
// 하나라도 있는가")은 남은 하나가 둘을 모두 통과시켜 이 손실이 보이지 않았고, 다른 네 겹도
// 같은 손실에 눈이 없다(P1은 빈 줄을 모델하지 않고, P2는 '#' 줄만 세며, P0는 인라인 env
// 엔트리만 보고, P4는 이미 상한 산출 위에서도 성립한다).
func TestLineSurvivalCountsDuplicates(t *testing.T) {
	const dup = "# 같은 줄\n"
	in := []byte(dup + dup + "[mcp_servers.ctr]\ncommand = \"context-router\"\n")
	out := []byte(dup + "[mcp_servers.ctr]\ncommand = \"context-router\"\n")
	if lost := lineSurvivalLosses(in, out, [2]int{-1, -1}); len(lost) != 1 {
		t.Errorf("잃은 라인=%v — 바이트 같은 두 줄 중 하나가 사라졌는데 개수를 세지 않는다", lost)
	}
	// 둘 다 살아 있으면 오경보가 없어야 한다 — 다중집합이 개수를 더 요구하면 정상 산출이 적색이 된다.
	if lost := lineSurvivalLosses(in, in, [2]int{-1, -1}); len(lost) != 0 {
		t.Errorf("무변경 산출에 오경보가 났다: %v", lost)
	}
}

// TestLineSurvivalHeaderExemptionIsScoped — 헤더 면제는 **우리 두 구간 안에서만** 준다.
// 파일 전역이면 사용자의 `[model]` 헤더도, 배열의 이어지는 `["a", "b"],` 줄도 어디서 사라지든
// 이 겹이 보지 못한다 — 형제인 키 이름 면제가 같은 이유로 이미 구간 한정이고, 격자는 둘째
// 테이블이 없어 이 넓힘을 구조적으로 못 본다.
func TestLineSurvivalHeaderExemptionIsScoped(t *testing.T) {
	const head = "[mcp_servers.ctr]\ncommand = \"context-router\"\n"
	const rest = "[model]\nopts = [\n  [\"a\", \"b\"],\n]\n"
	in := []byte(head + rest)
	// 구간 밖의 `[` 시작 줄 둘(남의 헤더 · 배열 이어지는 줄)을 지운 산출
	out := []byte(head + "opts = [\n]\n")
	if lost := lineSurvivalLosses(in, out, [2]int{-1, -1}); len(lost) != 2 {
		t.Errorf("잃은 라인=%v want 2개 — 구간 밖의 '[' 시작 줄이 면제되고 있다", lost)
	}
	// 반대쪽도 잰다: 우리 구간의 헤더는 재생성 대상이라 면제가 남아야 한다.
	if lost := lineSurvivalLosses(in, []byte("command = \"context-router\"\n"+rest), [2]int{-1, -1}); len(lost) != 0 {
		t.Errorf("우리 구간 헤더의 면제가 사라졌다: %v", lost)
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

// TestCodexLatticeWritingBranchCount — 격자의 **깊이**를 잰다. TestCodexLatticeSize가 5120이라는
// 폭을 고정하는 것과 짝이며, 둘 다 있어야 그물이 실재한다.
//
// **폭만 고정하면 절반이 공회전해도 초록이다**(session-53 실측). HEAD에서 5120 중 3072만 기입
// 갈래를 타고, 나머지 2048은 이상 판정으로 무변경으로 빠진다 — 점 표기 축 1024(anomalyDottedEnv)와
// 우변 비-인라인 축 1024(anomalyEnvNotTable). 그 2048에서는 다섯 겹이 파일을 자기 자신과
// 비교하므로 아무것도 검증하지 않는다: 스펙 §2.3이 세는 다섯 env 형태 중 **둘이 되쓰기를 한
// 번도 지나지 않는다.**
//
// 개수를 고정하는 이유는 **집합 사이의 이주**다. 케이스별 "기입했거나, 아니면 바이트 동일"
// (assertCodexOracles)은 필요조건일 뿐이다 — 이상 판정이 넓어져 **유효한** 케이스가 기입 집합에서
// 공회전 집합으로 옮겨가도 그 단정은 그대로 통과하고 5120이라는 폭도 그대로다. 개수만이 그
// 이주를 문다. 이 브랜치가 이미 그 실패를 냈다(과발화하는 이상 판정).
//
// **이 수는 현재 트리에 대한 진술이다.** 다음 태스크가 과발화하는 이상 판정을 고치면 케이스가
// 기입 집합으로 **되돌아오므로 이 수는 올라간다** — 그때 값을 올리는 것은 예정된 작업이고
// 단정을 느슨하게 하는 것이 아니다. 반대로 이 수가 **내려가면** 그것은 회귀다: 되쓰기를 지나던
// 케이스가 무변경으로 빠졌다는 뜻이고, 그물이 그만큼 눈을 감았다는 뜻이다. 값을 고칠 때는
// 무엇이 왜 옮겨갔는지를 함께 적어라.
func TestCodexLatticeWritingBranchCount(t *testing.T) {
	marker := hookMarker("0.18.0")
	wrote := 0
	for _, src := range codexLattice() {
		if installCodexConfigBlock([]byte(src), codexInstallRequest{Marker: marker}).Changed {
			wrote++
		}
	}
	// 3072 = 5 형태 중 되쓰기를 지나는 셋 × 1024. 나머지 둘(점 표기 · 우변 비-인라인)이 2048이다.
	if want := 3 * 1024; wrote != want {
		t.Errorf("기입 갈래를 탄 격자 케이스=%d want %d(전체 %d) — 케이스가 기입 집합과 공회전 집합 사이를 옮겨 갔다",
			wrote, want, len(codexLattice()))
	}
}

// TestCodexInTableCommentsPreserved — P2를 **관리 테이블 안**의 주석에 건다. 격자는 이 자리를
// 덮지 않는다(리뷰 실측): 격자의 독립 주석 cm[0]은 `[mcp_servers.ctr]` **앞**에 붙어 테이블
// 안에 들어가지 않고, 테이블 안의 유일한 주석인 command 줄 후행은 D88 예외라 allowedLost에
// 있다. 그래서 가장 위험한 자리 — keep 루프가 다시 잇는 구간 안의 주석 — 이 5120 케이스
// 전부에서 비어 있었다.
//
// **격자에 축을 더하지 않는다**: 케이스 수가 바뀌면 폭 고정 단정(5120)이 무의미해진다.
// 대신 그 자리만 겨냥한 픽스처를 둔다. allowedLost를 **비운다** — 여기 주석은 전부 사용자
// 소유 라인에 있어 keep 루프가 원문 그대로 되돌려야 하고, 하나라도 잃으면 결함이다.
func TestCodexInTableCommentsPreserved(t *testing.T) {
	marker := hookMarker("0.18.0")
	const head = "[mcp_servers.ctr]\ncommand = \"context-router\"\n"
	const env = "env = { CTR_MANAGED = \"context-router/0.15.0\" }\n"
	for _, c := range []struct{ name, src string }{
		{"테이블 안 독립 주석", head + "# 테이블 안 독립 주석\nU1 = \"v\"\n" + env},
		{"사용자 키의 후행 주석", head + "U1 = \"v\" # 사용자 키 후행\n" + env},
		{"여러 줄 값의 이어지는 줄 주석", head + "V4 = [\n  \"p\", # 첫 원소\n  \"q\", # 둘째 원소\n]\n" + env},
		{"셋 다", head + "# 독립\nU1 = \"v\" # 후행\nV4 = [\n  \"p\", # 이어지는 줄\n]\n" + env},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := []byte(c.src)
			res := installCodexConfigBlock(in, codexInstallRequest{Marker: marker})
			if res.State != mcpWritten {
				t.Fatalf("픽스처가 기입 갈래로 가지 않는다: state=%v — 이 테스트는 되쓰기를 재려는 것이다", res.State)
			}
			assertCommentsPreserved(t, in, res.Out)
		})
	}
}

// assertCodexOracles — 입력 하나에 P0·P1·P2·P3·P4를 **다섯 겹 모두** 건다. 격자와 겨냥
// 픽스처가 **같은 그물**을 지나게 하는 단일 지점이다: 겹 목록을 두 자리에 적으면 겨냥
// 픽스처만 한 겹 빠진 그물을 지나고, 그 빠짐은 초록으로 보인다.
// allowedLostComments는 P2의 D88 예외(우리가 재생성하는 관리 키 줄의 후행 주석)다.
//
// **돌려주는 wrote는 "이 케이스가 기입 갈래를 탔는가"다**(session-53). 무변경으로 빠진 입력에서는
// 다섯 겹이 전부 공회전한다 — P0는 같은 바이트를 두 번 훑고, P1·P2·P3는 파일을 자기 자신과
// 비교하며, P4는 자명하게 성립한다. 그런 케이스가 몇 개인지 **호출자가 세지 않으면** 이상 판정이
// 넓어져 유효 입력이 기입 집합에서 공회전 집합으로 옮겨가도 그물 전체가 초록으로 남는다.
// 여기서는 "무변경이면 바이트 동일"만 잰다 — 필요조건이지 충분조건이 아니고, 개수는 호출자 몫이다.
func assertCodexOracles(t *testing.T, marker string, in []byte, allowedLostComments ...string) (wrote bool) {
	t.Helper()
	res := installCodexConfigBlock(in, codexInstallRequest{Marker: marker})
	// 기입 갈래를 타지 않았으면 산출은 입력과 **바이트 동일**해야 한다(D84·D85 무변경 계약).
	// 이 단정 없이는 "Changed=false인데 산출이 다르다"가 다섯 겹 전부를 무의미하게 만든다.
	if !res.Changed && !bytes.Equal(res.Out, in) {
		t.Errorf("무변경 결과인데 산출이 입력과 다르다: state=%v anomaly=%v\n입력: %q\n산출: %q",
			res.State, res.Anomaly, in, res.Out)
	}
	// P0 — 인라인 env 엔트리가 있으면 열거 결과가 그 엔트리를 구조 문자만 사이에 두고
	// 덮는다. 입력과 산출 **양쪽**에 건다: 산출에만 걸면 우리가 만든 형태만 보고,
	// 입력에만 걸면 우리 되쓰기가 만든 어긋남을 못 본다.
	for _, b := range [][]byte{in, res.Out} {
		if lines, e := codexEnvEntryOf(b); e[0] >= 0 {
			assertInlineScanTiles(t, lines, e, tomlScanInline(lines, e))
		}
	}
	// P1 — 허용 경로는 우리가 기입하는 넷뿐이다.
	allowed := []string{
		"mcp_servers.ctr.env." + codexMarkerKey,
		"mcp_servers.ctr.command", "mcp_servers.ctr.args", "mcp_servers.ctr.enabled_tools",
	}
	// **빈 인라인 env는 부모 경로도 허용한다**(session-53). tomlFlatten은 빈 테이블을 그 경로의
	// 잎으로 적으므로(소실을 보려면 잎이어야 한다) `env = {   }`에 표식을 채우면 잎 경로
	// `mcp_servers.ctr.env`가 사라진 것으로 읽혀 P1이 **오경보**를 낸다. 그 결과 후행 쉼표 금지
	// 때문에 전용 생산 로직을 가진 빈 인라인 갈래를 격자에도 어떤 오라클 픽스처에도 넣을 수
	// 없었다 — 다섯 겹 전부에서 빠져 있었다.
	// **입력의 그 경로가 실제로 빈 테이블일 때만** 허용한다: `env = []` 같은 비-테이블 우변도
	// 같은 경로의 잎이라, 무조건 허용하면 그 값의 변조가 P1에 보이지 않게 된다.
	if before, err := tomlFlatten(in); err == nil && before["mcp_servers.ctr.env"] == tomlEmptyTableLeaf {
		allowed = append(allowed, "mcp_servers.ctr.env")
	}
	assertSemanticPreserved(t, in, res.Out, allowed...)
	// P2 — 주석 다중집합. tomlComments가 '#'부터 잘라 내므로 앞 공백은 들어가지 않는다.
	assertCommentsPreserved(t, in, res.Out, allowedLostComments...)
	// P3 — 우리 관리 키가 아닌 물리 라인의 바이트 생존. 예외는 인라인 env 엔트리다.
	_, envEntry := codexEnvEntryOf(in)
	assertLineSurvival(t, in, res.Out, envEntry)
	// P4 — 멱등.
	again := installCodexConfigBlock(res.Out, codexInstallRequest{Marker: marker})
	if !bytes.Equal(again.Out, res.Out) {
		t.Errorf("멱등이 아니다:\n입력: %q\n1: %s\n2: %s", in, res.Out, again.Out)
	}
	return res.Changed
}

// TestCodexEmptyInlineEnvOracles — 빈 인라인 `env`에 다섯 겹을 건다. **이 형태는 P1의 오경보
// 때문에 그물 밖에 있었다**(session-53 실측: `env = {   }`가 `경로가 사라졌다:
// mcp_servers.ctr.env`로 적색). 전용 생산 로직 — TOML의 후행 쉼표 금지 때문에 빈 테이블에는
// 쉼표 없이 삽입해야 한다 — 이 다섯 겹 중 어디에도 걸리지 않던 자리다.
// **격자에 축을 더하지 않는다**: 케이스 수가 바뀌면 폭 고정 단정(5120)이 무의미해진다.
func TestCodexEmptyInlineEnvOracles(t *testing.T) {
	marker := hookMarker("0.18.0")
	const head = "[mcp_servers.ctr]\ncommand = \"context-router\"\n"
	for _, c := range []struct{ name, src string }{
		{"빈 인라인", head + "env = {}\n"},
		{"공백만 담은 인라인", head + "env = {   }\n"},
		{"닫는 중괄호 뒤 주석", head + "env = {} # 사용자 메모\n"},
		{"사용자 키 뒤의 빈 인라인", head + "U1 = \"v\"\nenv = {  }\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := []byte(c.src)
			if !codexTOMLParses(in) {
				t.Fatalf("픽스처가 파스되지 않는다 — 유효 입력에서 재는 축이 아니게 된다:\n%q", c.src)
			}
			if !assertCodexOracles(t, marker, in) {
				t.Fatalf("픽스처가 기입 갈래로 가지 않는다 — 이 테스트는 되쓰기를 재려는 것이다")
			}
		})
	}
}

// TestCodexMultilineStringCloseOracles — 여러 줄 문자열이 인라인 `env` 안에서 닫히고 **그 물리
// 라인에 후행 주석이 붙는** 형태에 다섯 겹을 건다. P2가 그 줄을 통째로 건너뛰던 동안(위
// tomlComments의 정정) 이 자리의 주석 손실은 어느 겹에도 보이지 않았다.
// **격자에 축을 더하지 않는다** — 형제 픽스처들과 같은 선택이다.
func TestCodexMultilineStringCloseOracles(t *testing.T) {
	marker := hookMarker("0.18.0")
	const head = "[mcp_servers.ctr]\ncommand = \"context-router\"\n"
	for _, c := range []struct{ name, src string }{
		{"닫는 줄 후행 주석", head + "env = { " + codexMarkerKey + " = \"context-router/0.17.2\", M = \"\"\"\nline\n\"\"\" } # keep this note\n"},
		{"닫는 줄 뒤 사용자 키", head + "env = { " + codexMarkerKey + " = \"context-router/0.17.2\", M = \"\"\"\nline\n\"\"\" } # note\nU1 = \"v\"\n"},
		{"홑따옴표 삼중", head + "env = { " + codexMarkerKey + " = \"context-router/0.17.2\", M = '''\nline\n''' } # note\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := []byte(c.src)
			if !codexTOMLParses(in) {
				t.Fatalf("픽스처가 파스되지 않는다 — 유효 입력에서 재는 축이 아니게 된다:\n%q", c.src)
			}
			if !assertCodexOracles(t, marker, in) {
				t.Fatalf("픽스처가 기입 갈래로 가지 않는다 — 이 테스트는 되쓰기를 재려는 것이다")
			}
		})
	}
}

// TestCodexHeaderInsideInlineEnvOracles — **회귀 잠금 + 오라클 픽스처.** 인라인 `env`가 닫히지
// 않은 채 뒤따르는 `[other]` 헤더가 오는 형태. 그 헤더는 스캐너가 열려 있어 경계로 잡히지
// 않으므로 우리 구간이 EOF까지 늘어나고, **헤더 자신의 대괄호가 깊이를 도로 맞춰** EOF 열림
// 백스톱마저 서지 않았다 — 되쓰기가 사용자의 `[other]`와 그 키를 통째로 먹고(실측: `install`이
// state=mcpWritten·changed=true·사유 없음), `uninstall`은 파일을 통째로 비웠다. base(128a727)는
// 둘 다 보존한다.
//
// **이 형태는 앞 태스크가 고친 그물이 잡을 수 있게 된 바로 그 형태다** — P3의 인라인 env 면제가
// 엔트리가 삼킨 첫 테이블 헤더 앞에서 끊기므로 라인 4·5의 소실이 손실 목록에 오른다. 그런데
// 그 모양을 먹이는 픽스처가 커밋되어 있지 않아 그물은 초록이었다. 이 픽스처가 그 자리를 채운다:
// 픽스 전에는 P3가 적색이고, 픽스 뒤에는 무변경이라 다섯 겹이 모두 성립한다.
//
// 계약은 **되쓰지 않는 것**이다. 구간의 끝을 확정할 수 없으면 사유 있는 무변경으로 빠진다 —
// base처럼 원문을 보존하며 표식만 갱신하는 것이 아니라, 아예 손대지 않는다.
func TestCodexHeaderInsideInlineEnvOracles(t *testing.T) {
	marker := hookMarker("0.18.0")
	const head = "[mcp_servers.ctr]\ncommand = \"context-router\"\n"
	const tail = "[other]\nuKEY = \"uV\" }\n"
	for _, c := range []struct{ name, src string }{
		{"빈 줄 뒤 헤더", head + "env = { " + codexMarkerKey + " = \"context-router/0.17.2\"\n\n" + tail},
		{"바로 다음 줄이 헤더", head + "env = { " + codexMarkerKey + " = \"context-router/0.17.2\",\n" + tail},
		{"표식 없는 인라인 뒤 헤더", head + "env = { X = \"v\"\n\n" + tail},
		{"배열 테이블 헤더", head + "env = { X = \"v\"\n[[a.b]]\nuKEY = \"uV\" }\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := []byte(c.src)
			if assertCodexOracles(t, marker, in) {
				t.Fatalf("구간의 끝을 확정할 수 없는 형태를 되썼다 — 무변경이 계약이다")
			}
			res := installCodexConfigBlock(in, codexInstallRequest{Marker: marker})
			if res.State != mcpMarkerAnomaly || res.Anomaly != anomalyScannerOpen {
				t.Errorf("install state=%d anomaly=%d want mcpMarkerAnomaly/anomalyScannerOpen", res.State, res.Anomaly)
			}
			// 사유가 그 파일에 대해 참이어야 한다 — 이 형태는 파일 **끝**이 아니라 다음 테이블
			// 헤더 앞에서 닫히지 않았고, 문면이 둘을 함께 댄다.
			if r := res.Anomaly.reason(); !strings.Contains(r, "다음 테이블 헤더 앞") {
				t.Errorf("사유가 이 파일의 자리를 대지 않는다: %q", r)
			}
			out, changed, an := uninstallCodexConfigBlock(in)
			if changed || an != anomalyScannerOpen || !bytes.Equal(out, in) {
				t.Errorf("uninstall changed=%v anomaly=%d — 사용자 테이블을 지웠다:\n%q", changed, an, out)
			}
			if present, pan := probeCodexMCPBlock(in); present || pan != anomalyScannerOpen {
				t.Errorf("probe present=%v anomaly=%d want false/anomalyScannerOpen", present, pan)
			}
		})
	}
}

// TestCodexHeaderShapeInValueStillWrites — 위 좁힘의 **오경보 감시선**. "스캐너가 열렸는데 이 줄이
// 헤더 모양"만 보면 여러 줄 값의 정상적인 이어지는 줄이 전부 걸린다: 배열 원소가 배열인 형태
// (`args = [`⏎`  ["nested"],`⏎`]`)와 인라인 테이블 **안**의 중첩 배열, 그리고 여러 줄 문자열의
// 내용 줄이 그것이다. 셋 다 유효 TOML이고 base·출하 HEAD가 모두 기입하던 파일이라, 여기서
// 얼어붙으면 사유가 참이어도 새 결함이다. 좁힘의 근거는 "**열린 괄호가 전부 중괄호**일 때만"
// 이고 이 셋은 열린 배열이나 문자열을 갖는다.
func TestCodexHeaderShapeInValueStillWrites(t *testing.T) {
	marker := hookMarker("0.18.0")
	const head = "[mcp_servers.ctr]\ncommand = \"context-router\"\n"
	const env = "env = { " + codexMarkerKey + " = \"context-router/0.17.2\" }\n"
	for _, c := range []struct{ name, src string }{
		{"배열 안의 배열 원소", head + "args = [\n  [\"nested\"],\n]\n" + env},
		{"배열 마지막 원소가 배열(쉼표 없음)", head + "V4 = [\n  [\"a\"]\n]\n" + env},
		{"인라인 테이블 안의 중첩 배열", head + "env = { " + codexMarkerKey + " = \"context-router/0.17.2\", A = [\n  [\"x\"]\n] }\n"},
		{"인라인 테이블 안의 정수 배열", head + "env = { " + codexMarkerKey + " = \"context-router/0.17.2\", A = [\n  [1, 2]\n] }\n"},
		{"여러 줄 문자열 내용이 헤더 모양", head + "env = { " + codexMarkerKey + " = \"context-router/0.17.2\", A = \"\"\"\n[not a header]\n\"\"\" }\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := []byte(c.src)
			if !codexTOMLParses(in) {
				t.Fatalf("픽스처가 파스되지 않는다 — 유효 입력에서 재는 축이 아니게 된다:\n%q", c.src)
			}
			if !assertCodexOracles(t, marker, in) {
				t.Fatalf("정상 파일이 무변경으로 얼어붙었다: anomaly=%q",
					installCodexConfigBlock(in, codexInstallRequest{Marker: marker}).Anomaly.reason())
			}
		})
	}
}

// TestCodexLatticeOracles — 격자의 모든 케이스에 다섯 겹을 건다. P0와 P3를 부르지 않으면 그 두
// 겹이 격자 위에서 한 번도 실행되지 않는다(리뷰 실측). 입력이 파스되지 않으면 P1만 건너뛴다
// (D89 비대칭). 실패 시 케이스 원문을 인쇄해 최소 재현을 즉시 얻는다.
// 격자의 후행 주석은 command 줄에 붙어 있어 D88 예외다.
//
// **반환값을 여기서 세지 않는다** — `-run`으로 부분 집합만 돌리면 그 수가 틀려지므로 개수는
// 자기 루프를 가진 TestCodexLatticeWritingBranchCount가 잰다. 여기서는 케이스마다 "기입했거나,
// 아니면 바이트 동일"이 assertCodexOracles 안에서 걸린다.
func TestCodexLatticeOracles(t *testing.T) {
	marker := hookMarker("0.18.0")
	for i, src := range codexLattice() {
		t.Run(fmt.Sprintf("case%04d", i), func(t *testing.T) {
			assertCodexOracles(t, marker, []byte(src), "# 후행")
		})
	}
}

// TestCodexValueEndOnFragmentBoundaryOracles — 격자가 **구조적으로 못 만드는 한 형태**에 다섯
// 겹을 건다: 인라인 env 값 토큰의 **배타적 끝이 조각 경계에 정확히 놓이는** 모양. 값이 줄 끝에서
// 끝나고 다음 물리 라인이 쉼표로 시작해야 후행 공백 절단이 valEnd를 at[k]까지 되당기는데,
// 격자의 여러 줄 인라인 축은 `CTR_MANAGED = "…",`⏎`  U = "1" }` 하나뿐이라 쉼표가 **앞** 줄에
// 붙어 그 자리에 닿지 않는다. 그래서 이 클래스의 손실(경계 앞 물리 라인을 통째로 먹어 사용자의
// 주석 줄·빈 줄이 사라진다)이 5120 케이스 전부에서 관측될 수 없었고, 다섯 겹을 모두 통과했다.
//
// **왕복 오라클로는 영영 볼 수 없는 클래스다**(스펙 §1.3 선행 게이트 1의 맹점): 틀린 해석
// (뒤 조각의 시작)과 옳은 해석(앞 조각의 끝)이 **같은 오프셋으로 왕복**하고, 타일링(P0)도 같은
// 이유로 눈이 없다. 무는 것은 P2(주석 다중집합)와 P4다 — 그물의 어느 겹이 무는지가 이 형태에서만
// 달라지므로 다섯 겹을 통째로 건다.
//
// **격자에 축을 더하지 않는다**: 케이스 수가 바뀌면 폭 고정 단정(5120)이 무의미해진다 —
// TestCodexInTableCommentsPreserved가 같은 이유로 같은 선택을 했다. 대신 그 자리만 겨냥한
// 픽스처를 둔다. 주석·빈 줄·CRLF·후행 사용자 키를 축으로 섞어 손실의 폭을 함께 잰다.
func TestCodexValueEndOnFragmentBoundaryOracles(t *testing.T) {
	marker := hookMarker("0.18.0")
	for _, c := range []struct{ name, mid, tail, eol string }{
		{"주석 줄", "# 사용자 메모", "", "\n"},
		{"빈 줄", "", "", "\n"},
		{"주석 두 줄", "# 첫째 메모\n# 둘째 메모", "", "\n"},
		{"바이트 같은 빈 줄 둘", "\n", "", "\n"},
		{"CRLF", "# 사용자 메모", "", "\r\n"},
		{"뒤에 사용자 키", "# 사용자 메모", "U1 = \"v\"\n", "\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			eol := c.eol
			nl := func(s string) string { return strings.ReplaceAll(s, "\n", eol) }
			src := nl("[mcp_servers.ctr]\ncommand = \"context-router\"\n") +
				`env = { ` + codexMarkerKey + ` = "` + hookMarker("0.17.2") + `"` + eol +
				nl(c.mid) + eol + `      , KEEP = "keepme" }` + eol + nl(c.tail)
			if !codexTOMLParses([]byte(src)) {
				t.Fatalf("픽스처가 파스되지 않는다 — 유효 입력에서 재는 축이 아니게 된다:\n%q", src)
			}
			if !assertCodexOracles(t, marker, []byte(src)) {
				t.Fatalf("픽스처가 기입 갈래로 가지 않는다 — 이 테스트는 되쓰기를 재려는 것이다")
			}
		})
	}
}
