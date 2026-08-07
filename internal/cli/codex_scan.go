// D97 계약 2 — 기입 경로(codex_toml.go)를 지운 뒤에도 doctor는 기존 손편집 등록물을 감지해서
// 알리고 어느 줄인지 짚는다. 후보였던 `codex mcp get`은 무효 TOML에서 실패한다([실측] —
// 도움이 가장 필요한 코호트에서 정확히 실패한다는 뜻이다) 그리고 `--json`은 env 값을 평문으로
// 낸다. 그러므로 doctor는 외부 명령을 부르지 않고 이 파일의 순수 스캐너로 직접 찾는다.
//
// TOML 파서를 쓰지 않는다 — 이것은 판정이 아니라 보고이고, 파서라면 실패할 무효 입력에서도
// 동작하는 것이 존재 이유다(D97). 그래서 줄 단위 문자열 매치만 하고, codex_toml.go의 상태
// 추적 스캐너(여러 줄 문자열·배열의 열림 상태를 좇는 쪽)는 가져오지 않는다 — 그 정교함이
// 유효 입력에서 사용자 파일을 다섯 번 파괴한 원인이었다([문서] — D97 근거).
package cli

import (
	"bytes"
	"strings"
)

// trimBOM — 선두 UTF-8 BOM 세 바이트를 뗀다. **읽기 전용 판정 경로 전용이다** — 되쓰기
// 바이트를 만드는 자리에서 부르면 우리가 사용자 파일의 인코딩을 조용히 바꾼다.
//
// Windows 편집기(PowerShell 5.1의 `Out-File -Encoding utf8`, 구버전 메모장)가 파일 첫 줄에
// 붙이는 바이트인데 U+FEFF는 이 파일의 정규화(`strings.TrimSpace`, codexHeaderName)가 보는
// 공백이 아니라 그대로 통과한다 `[실측]`(설치된 Go의 unicode/tables.go를 직접 열어 확인 —
// _White_Space 레인지 테이블에 U+FEFF가 없다).
// 떼지 않으면 파일 첫 줄이 테이블 헤더일 때 그 헤더가 인식되지 않는다. Codex 자신은 BOM이
// 붙은 config.toml을 정상으로 읽으므로 `[실측]` 갈리는 것은 우리 판정뿐이고, 그래서 doctor가
// 멀쩡한 파일에 오보를 내지 않으려면 판정 전에 떼야 한다.
//
// 이 자리에 사는 이유: 소비처 둘(codexServerHeaders·codexTOMLParses)이 모두 읽기 전용
// 스캐너이고, 원 소유자였던 codex_toml.go는 기입 경로와 함께 지워졌다(D97 계약 1).
func trimBOM(b []byte) []byte { return bytes.TrimPrefix(b, []byte("\xEF\xBB\xBF")) }

// codexHeaderHit — 감지된 [mcp_servers.<이름>] 테이블 헤더 한 줄. Line은 1-기반이다 — 사용자가
// 에디터에서 그대로 찾아갈 줄 번호이지 배열 인덱스가 아니다.
type codexHeaderHit struct {
	// Name — 세그먼트를 나누지 않고 그대로 뗀 값이다(mcp_servers.ctr.env → "ctr.env"일
	// 수 있다, codexServerHeaders 주석 참고) — 서버 이름 그 자체라고 단정하지 않는다.
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
// 스캐너가 정확히 codex_toml.go였고, 유효 입력에서마저 사용자 파일을 다섯 번
// 파괴했다([문서] — v0.18 §3.5·§3.7·§3.8, D97 근거) — 무효 입력에서도 죽지 않는 값이 그
// 정교함의 값보다 크다는 것이 D97의 판정이고, 이 함수는 그 판정을 그대로 구현한다.
//
// 두 번째 한계 — 닫는 대괄호 뒤는 비어 있거나 `#` 주석이어야 헤더로 잡힌다. 손으로 등록물을
// 고친 사람이 그 줄에 사유를 남기는 것(`[mcp_servers.ctr] # hand-added 2026-05`)은 흔한
// 스타일이고 이 스캐너가 찾는 대상이 정확히 그 사람이라 주석은 잡아야 하지만, `#` 없이 다른
// 문자가 대괄호 바로 뒤에 곧장 붙는 줄은 잡지 않는다 — 그 형태는 애초에 유효 TOML 헤더 줄이
// 아니고, 뒷내용을 검사 없이 다 통과시키면 대괄호 뒤에 우연히 다른 무효 조각이 붙은 줄까지
// 헤더로 잘못 보고해 doctor 출력의 신뢰도를 깎는다. 대괄호 자체는 따옴표 안쪽을 피해서
// 찾으므로(codexHeaderClose) 이름 안의 `#`·`]`는 주석이나 종료로 오인되지 않는다.
//
// 세 번째 한계 — 줄 끝까지 따옴표가 안 닫히면(닫는 따옴표를 빠뜨린 흔한 오타, 예:
// `[mcp_servers."my-server]`) codexHeaderClose가 따옴표 추적을 포기하고 그 줄의 마지막
// `]`를 최선-노력으로 닫는 자리로 쓴다 — 헤더로 잡는다는 판정은 살리고 이름만 보장하지
// 않는다(안 닫힌 따옴표가 unquoteHeaderName의 양끝 짝 조건을 못 채워 그대로 이름에 남을 수
// 있다). 이 순서를 고르는 이유는 첫 번째 한계와 같은 비대칭이다: 오타 줄을 조용히 넘기면
// 오검출은 없지만 그 줄이 정확히 이 스캐너가 찾아야 하는 손편집 오타이고, 그러면 사용자는
// 자기 파일이 깨끗하다고 믿는다 — 지저분한 이름이라도 줄 번호가 doctor 출력에 뜨는 값이
// 조용한 통과보다 크다.
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
// 닫는 대괄호는 codexHeaderClose가 따옴표 밖에서 찾으므로 이름 안의 `#`·`]`는 종료로 오인되지
// 않는다(`[mcp_servers."a#b"]`·`[mcp_servers."a]b"]` 모두 이름을 그대로 잡는다). 그 대괄호
// 뒤는 비어 있거나 `#` 주석이어야 헤더로 인정한다 — 대가는 codexServerHeaders의 두 번째
// 알려진 한계에 적는다. [[mcp_servers.foo]](배열 테이블)는 codexHeaderClose가 이중
// 대괄호의 첫 ']'에서 멈추므로 둘째 ']'가 대괄호 뒤에 남아 그 규칙에 걸려 걸러진다(별도
// 분기 없음). 하위 이름이 없는 [mcp_servers] 자체는 대괄호 뒤가 비어 그 규칙은 통과하지만
// "mcp_servers." 뒤에 와야 할 마침표가 없어 다음 단계에서 걸러진다.
func codexHeaderName(line string) (string, bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "[") {
		return "", false
	}
	closeAt, ok := codexHeaderClose(s)
	if !ok {
		return "", false
	}
	tail := strings.TrimSpace(s[closeAt+1:])
	if tail != "" && !strings.HasPrefix(tail, "#") {
		return "", false // 대괄호 뒤가 비어 있지도 주석도 아니다 — codexServerHeaders 두 번째 한계
	}
	inner := strings.TrimSpace(s[1:closeAt])

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

// codexHeaderClose — s[0]=='['을 전제로, 따옴표 밖에서 처음 나오는 ']'의 인덱스를 찾는다.
// 큰따옴표 안에서는 `\`가 다음 한 글자를 이스케이프로 건너뛴다(`\"`에 문자열이 조기 종료되지
// 않도록) — 그 한 글자만 건너뛸 뿐 TOML 이스케이프를 실제로 해석하지는 않는다
// (unquoteHeaderName과 같은 이유). 작은따옴표(TOML 리터럴 문자열)는 이스케이프가 없어 그대로
// 토글한다.
//
// 줄 끝까지 따옴표가 안 닫히면(닫는 따옴표를 빠뜨린 흔한 오타) 따옴표 추적을 포기하고 그
// 줄의 마지막 ']'를 최선-노력으로 닫는 자리로 쓴다 — codexServerHeaders 세 번째 알려진
// 한계가 이 대가를 적는다. ']' 자체가 줄에 없으면 그마저 없어 (-1, false) — 여러 줄에 걸쳐
// 닫히는 실제 문자열은 애초에 이 함수의 대상이 아니다(codexServerHeaders 첫 번째 알려진
// 한계).
func codexHeaderClose(s string) (int, bool) {
	var quote byte
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '"' && c == '\\':
			i++
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == ']':
			return i, true
		}
	}
	i := strings.LastIndexByte(s, ']')
	return i, i >= 0
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
