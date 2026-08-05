package cli

import (
	"fmt"
	"strings"
	"testing"
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
