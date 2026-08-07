// D97 계약 2 — 기입 경로(codex_toml.go)를 지운 뒤에도 doctor는 기존 손편집 등록물을 감지해서
// 알리고 어느 줄인지 짚는다. 후보였던 `codex mcp get`은 무효 TOML에서 실패한다([실측] —
// 도움이 가장 필요한 코호트에서 정확히 실패한다는 뜻이다) 그리고 `--json`은 env 값을 평문으로
// 낸다. 그러므로 doctor는 외부 명령을 부르지 않고 이 파일의 순수 스캐너로 직접 찾는다.
//
// TOML 파서를 쓰지 않는다 — 이것은 판정이 아니라 보고이고, 파서라면 실패할 무효 입력에서도
// 동작하는 것이 존재 이유다(D97). 그래서 줄 단위 문자열 매치만 하고, codex_toml.go의 상태
// 추적 스캐너(여러 줄 문자열·배열의 열림 상태를 좇는 쪽)는 가져오지 않는다 — 그 정교함이
// 유효 입력에서 사용자 파일을 다섯 번 파괴한 원인이었다(D97 근거).
package cli

import "strings"

// codexHeaderHit — 감지된 [mcp_servers.<이름>] 테이블 헤더 한 줄. Line은 1-기반이다 — 사용자가
// 에디터에서 그대로 찾아갈 줄 번호이지 배열 인덱스가 아니다.
type codexHeaderHit struct {
	Name string
	Line int
}

// codexServerHeaders — b에서 [mcp_servers.<이름>] 형태의 테이블 헤더 줄을 전부 찾는다. 순수
// 함수다(파일 IO·시간·난수 없음) — 읽기 전용 doctor가 부르는 경로라 D85의 순수성 계약을 진다.
//
// 이름은 "mcp_servers." 다음부터 닫는 대괄호 앞까지를 그대로 떼어 낸 값이다(따옴표 한 겹만
// 벗기고 TOML 이스케이프는 해석하지 않는다) — 그래서 [mcp_servers.ctr.env]처럼 점이 더 있는
// 헤더도 이름 "ctr.env"로 잡힌다. 세그먼트를 나누어 상위 서버 이름만 남기지 않는 것은 의도다:
// 무효 TOML을 손으로 고치는 사용자에게는 mcp_servers로 시작하는 헤더 줄 전부가 "지워야 할지
// 볼 자리"이고, .env 서브테이블 헤더만 홀로 남아도 TOML 점 표기 규칙상 부모 mcp_servers.ctr
// 테이블을 암묵적으로 되살린다 — 상위 이름만 보고하면 그 줄의 존재를 사용자가 놓친다.
//
// 알려진 한계 — 여러 줄 문자열이나 배열 안의 줄이 우연히 헤더 모양이면 오탐한다. 이 함수가
// 그 열림 상태를 추적하지 않기 때문이다. 받아들이는 이유는 비용과 편익이 비대칭이라서다:
// 보고 전용이라 오탐의 비용은 doctor 출력에 있지도 않은 줄 번호 하나가 더 얹히는 것뿐이고,
// 그 비용을 없애는 값은 여러 줄 문자열·배열의 열림 상태를 좇는 파서 수준 스캐너 전체다. 그
// 스캐너가 정확히 codex_toml.go였고, 유효 입력에서마저 사용자 파일을 다섯 번 파괴했다 — 무효
// 입력에서도 죽지 않는 값이 그 정교함의 값보다 크다는 것이 D97의 판정이고, 이 함수는 그
// 판정을 그대로 구현한다.
func codexServerHeaders(b []byte) []codexHeaderHit {
	b = trimBOM(b)
	var hits []codexHeaderHit
	for i, line := range strings.Split(string(b), "\n") {
		name, ok := codexHeaderName(line)
		if !ok {
			continue
		}
		hits = append(hits, codexHeaderHit{Name: name, Line: i + 1})
	}
	return hits
}

// codexHeaderName — 한 줄이 [mcp_servers.<이름>] 테이블 헤더이면 이름과 true를, 아니면
// ("", false)를 돌려준다. 줄 앞뒤 공백과 대괄호 안쪽 공백(`[ mcp_servers . foo ]`)을 허용한다.
// [[mcp_servers.foo]](배열 테이블)는 안쪽에 대괄호가 그대로 남아 "mcp_servers" 접두 매치가
// 실패하므로 별도 분기 없이 걸러진다. 하위 이름이 없는 [mcp_servers] 자체도 마침표가 없어
// 같은 이유로 걸러진다.
func codexHeaderName(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		return "", false
	}
	inner := strings.TrimSpace(trimmed[1 : len(trimmed)-1])

	rest := strings.TrimPrefix(inner, "mcp_servers")
	if rest == inner {
		return "", false // "mcp_servers" 접두가 아니다
	}
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, ".") {
		return "", false // 마침표가 없다 — [mcp_servers] 자체나 mcp_servers2 같은 다른 테이블
	}
	name := strings.TrimSpace(rest[1:])
	if name == "" {
		return "", false
	}
	return unquoteHeaderName(name), true
}

// unquoteHeaderName — 양끝이 같은 따옴표(" 또는 ')로 둘러싸여 있으면 그 한 겹만 벗긴다. TOML
// 이스케이프 시퀀스는 해석하지 않는다 — codexHeaderName과 같은 이유로, 판정이 아니라 보고라
// 그 값까지 구현할 필요가 없다.
func unquoteHeaderName(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
