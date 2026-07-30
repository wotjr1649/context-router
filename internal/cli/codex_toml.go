package cli

// D48→D80 — config.toml 관리 단위를 주석 마커 블록에서 **TOML 테이블 경계**로 옮긴다(스펙
// v0.15 §0 D80·D84). 주석은 TOML 데이터가 아니라 호스트 재직렬화가 지우므로(§3 표1) 마커로는
// 관리 단위를 유지할 수 없다. TOML 파서 비의존(신규 의존 금지): 라인 스캐너가 여러 줄
// 문자열·배열의 열림 상태를 추적해 테이블 헤더만 경계로 잡고, 그 경계 안에서만 바이트를
// 바꾼다 — TOML 문법상 헤더와 다음 헤더 사이에는 그 테이블의 키만 올 수 있으므로 blast
// radius가 우리 테이블 안으로 구조적으로 묶인다. 순수 바이트 변환 — IO는 호출자.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	codexBlockBegin = "# BEGIN context-router"
	codexBlockEnd   = "# END context-router"
)

type codexMCPState int

const (
	mcpWritten        codexMCPState = iota // 블록 기입/갱신 성공 — MCP 확정
	mcpExistingHeader                      // 표식도 없고 command도 우리 것이 아닌 [mcp_servers.ctr] 실존 — 기입 생략·MCP 확정
	mcpConflict                            // 우리 두 구간 **밖**의 중복 정의 신호 — 기입 생략·MCP 미확정
	mcpMarkerAnomaly                       // 구간 판정 불가(codexAnomaly의 어느 사유든) — 무변경·MCP 미확정
)

type markerClass int

const (
	classAppend  markerClass = iota // 마커 0쌍 — append 후보. **반환 전용**(비교 소비자 없음)
	classReplace                    // 소유 블록 1쌍 — 교체 후보. inOldBlock 판정이 이 값을 비교한다
	classAnomaly                    // 그 외 마커 배치 — 무변경. **반환 전용**(비교 소비자 없음)
)

// codexAnomaly — 구간 판정을 신뢰할 수 없는 사유(D85·D87). 불리언이 아니라 열거형인 이유는
// 사용자에게 **사유를 알려야** 하기 때문이다: [16]의 기존 안내는 중복 헤더 정리를 지시하므로,
// 사유가 다른 파일에서 그 안내만 보이면 install이 영구히 무변경인 이유를 알 수 없다.
// 어느 사유든 결과는 같다 — 무변경 경로로 빠지고 --fix를 권하지 않는다.
type codexAnomaly int

const (
	anomalyNone        codexAnomaly = iota // 이상 없음
	anomalyDupHeader                       // 같은 이름의 관리 테이블 헤더가 둘 이상(그 자체가 TOML 중복 정의)
	anomalyScannerOpen                     // EOF에서 스캐너가 열림(닫히지 않은 문자열·배열)
	anomalyEscapedKey                      // 우리 구간 안에 정규화 불가 키 표기(D87) — 이스케이프를 해석하지 않으므로 무변경
	// anomalyOutsideConflict — 우리 두 구간 **밖**의 중복 정의 신호. 구간 판정은 **성공**했으므로
	// 이것은 구간 판정 사유가 아니라 별개 상태다: codexSpans.anomaly에는 들어가지 않고
	// probeCodexMCPBlock만 만든다.
	anomalyOutsideConflict
)

// reason — 사용자 문면에 실을 사유(D85). 사유마다 **다른 조치**가 필요하므로 문면이 달라야
// 한다: 중복 헤더는 사용자가 하나를 지우면 되고, 스캐너 열림은 닫히지 않은 문자열·배열을
// 닫아야 하며, 정규화 불가 키는 이스케이프 표기를 보통 표기로 바꿔야 한다.
// anomalyNone은 빈 문자열이다 — 호출자가 그 갈래에서 사유를 인쇄하지 않는다.
func (a codexAnomaly) reason() string {
	switch a {
	case anomalyDupHeader:
		return "관리 테이블 헤더가 둘 이상입니다(TOML 중복 정의) — 하나만 남기고 지우세요"
	case anomalyScannerOpen:
		return "파일 끝에서 여러 줄 문자열 또는 배열이 닫히지 않았습니다 — 닫은 뒤 재실행하세요"
	case anomalyEscapedKey:
		return "관리 테이블 안의 키가 이스케이프 표기로 적혀 있습니다 — 보통 표기(예: command)로 바꾸세요"
	case anomalyOutsideConflict:
		return "관리 테이블 밖에 ctr 관련 정의가 있습니다 — doctor 스니펫으로 수동 정리한 뒤 재실행하세요"
	}
	return ""
}

// splitLinesKeepEnds — 종결자 보존 라인 분해(CRLF 보존 계약). 각 라인은 자신의 "\n"/"\r\n"을
// 포함하며, 마지막 라인은 종결자 없이 끝날 수 있다.
func splitLinesKeepEnds(b []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			lines = append(lines, b[start:i+1])
			start = i + 1
		}
	}
	if start < len(b) {
		lines = append(lines, b[start:])
	}
	return lines
}

// trimEOL — 라인 종결자("\r\n"/"\n")만 제거(마커 정확 매치용, 그 외 트림 없음).
func trimEOL(line []byte) string {
	s := strings.TrimSuffix(string(line), "\n")
	return strings.TrimSuffix(s, "\r")
}

// stripLine — 소유·충돌 판정용 정규화: 종결자 포함 공백 전부 제거.
func stripLine(line []byte) string {
	return strings.Join(strings.Fields(string(line)), "")
}

const (
	// codexManagedTable — Codex 관리 테이블 이름. .mcp.json의 ctrMCPServerName(ctr-exec)과
	// 다르다(D80·§1.2) — 사용자 파일의 [mcp_servers.ctr-exec]는 exec 2종만 노출하고 승인
	// 모드가 걸린 **별도 등록**이라 install도 uninstall도 읽지도 쓰지도 않는다.
	codexManagedTable = "mcp_servers.ctr"
	// codexManagedEnv — 소유 표식을 싣는 서브테이블. 관리 테이블과 **독립 구간**이다.
	codexManagedEnv = "mcp_servers.ctr.env"
	// codexMarkerKey — 소유 표식 키. 재직렬화를 견디면서 사용자 의미를 갖지 않는 유일한
	// 보존 필드다. 우리 바이너리는 이 환경변수를 읽지 않는다 — 표식일 뿐이며, 읽지
	// 않는다는 사실 자체가 계약이다(D80).
	codexMarkerKey = "CTR_MANAGED"
)

// tomlLineScanner — 라인 기반 테이블 헤더 판정기(D80 헤더 판정 규칙). TOML 파서가 아니다 —
// 여러 줄 문자열(삼중 큰따옴표·삼중 홑따옴표)과 여러 줄 배열의 열림 상태만 추적해 그 안의 [ … ] 모양 줄을
// 헤더로 보지 않는다. 상태 추적을 빼면 범위가 그 줄에서 잘려 우리 테이블의 잔여 키가 남거나
// 사용자 값 안의 한 줄이 경계가 되고, 그러면 D80의 blast radius 논거가 함께 무너진다.
type tomlLineScanner struct {
	inBasic   bool // """ 열림
	inLiteral bool // ''' 열림
	depth     int  // 여러 줄 배열 [ ] 균형
}

// open — 이 줄이 앞 줄에서 시작한 여러 줄 값 안인가(헤더·키 판정을 하면 안 되는 상태).
func (s *tomlLineScanner) open() bool { return s.inBasic || s.inLiteral || s.depth > 0 }

// step — 라인 하나를 소비한다. boundary는 이 줄이 테이블 경계인지, name은 단일 대괄호
// 헤더일 때의 정규화된 이름이다([[배열 테이블]]도 경계이지만 이름은 비운다 — 우리 이름이
// 될 수 없고, 경계로는 세야 앞 테이블의 구간이 거기서 끝난다).
func (s *tomlLineScanner) step(line []byte) (boundary bool, name string) {
	if !s.open() {
		t := stripLine(line)
		if strings.HasPrefix(t, "[") {
			boundary = true
			if !strings.HasPrefix(t, "[[") {
				if i := strings.Index(t, "]"); i > 1 {
					name = t[1:i]
				}
			}
		}
	}
	s.advance(string(line), boundary)
	return boundary, name
}

// advance — 라인 내용을 훑어 여러 줄 상태를 갱신한다. 헤더 줄은 자기완결이라 배열 균형을
// 세지 않는다 — 헤더의 대괄호가 배열 열림으로 잡히면 그 뒤 파일 전체가 값 안으로 들어간다.
func (s *tomlLineScanner) advance(line string, header bool) {
	for i := 0; i < len(line); {
		switch {
		case s.inBasic:
			// 닫기 탐색은 basicStringLen과 **같은 기준**으로 이스케이프를 인지한다 —
			// strings.Index로 찾으면 \""" 를 닫기로 오인해 그 뒤 파일 전체의 열림 상태가
			// 뒤집힌다. 두 자리가 다른 기준을 쓰면 같은 파일을 두 방식으로 읽는 셈이다.
			j := i
			for j < len(line) {
				if line[j] == '\\' {
					j += 2
					continue
				}
				if strings.HasPrefix(line[j:], `"""`) {
					break
				}
				j++
			}
			if j >= len(line) {
				return
			}
			s.inBasic, i = false, j+3
		case s.inLiteral:
			j := strings.Index(line[i:], "'''")
			if j < 0 {
				return
			}
			s.inLiteral, i = false, i+j+3
		case strings.HasPrefix(line[i:], `"""`):
			s.inBasic, i = true, i+3
		case strings.HasPrefix(line[i:], "'''"):
			s.inLiteral, i = true, i+3
		case line[i] == '#':
			return // 문자열 밖 주석 — 줄의 나머지는 값이 아니다
		case line[i] == '"':
			i += basicStringLen(line[i:])
		case line[i] == '\'':
			i += literalStringLen(line[i:])
		case line[i] == '[' && !header:
			s.depth++
			i++
		case line[i] == ']' && !header:
			if s.depth > 0 {
				s.depth--
			}
			i++
		default:
			i++
		}
	}
}

// basicStringLen — s[0]이 여는 큰따옴표일 때 닫는 따옴표까지의 길이(백슬래시 이스케이프
// 반영). 닫히지 않으면 줄 끝까지다 — TOML은 한 줄 기본 문자열의 줄바꿈을 허용하지 않으므로
// 미닫힘은 문법 오류이고, 우리는 그 줄에서 멈추면 된다.
func basicStringLen(s string) int {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return len(s)
}

// literalStringLen — 홑따옴표 문자열에는 이스케이프가 없다.
func literalStringLen(s string) int {
	if j := strings.Index(s[1:], "'"); j >= 0 {
		return j + 2
	}
	return len(s)
}

// codexSpan — 관리 테이블 한 구간의 라인 범위 [start, end) — 헤더 라인부터 다음 테이블 헤더
// 직전 또는 EOF까지.
type codexSpan struct {
	start, end int
	found      bool
}

// codexSpans — 두 관리 테이블의 구간. **서로 독립 구간**이다(D80): 인접해 있지 않아도 되고,
// 사이에 사용자 테이블이 오면 그 테이블은 어느 구간에도 들지 않는다 — 두 테이블을 하나의
// 연속 구간으로 다루면 그 사이 테이블이 삭제 범위에 든다.
// anomaly는 "구간 판정을 신뢰할 수 없다"는 뜻이며 **그 사유를 담는다**(D85가 사유를 인쇄한다).
// 어느 사유든 이미 무효 TOML이거나 우리가 다루지 않는 형태이므로 무변경으로 뺀다.
type codexSpans struct {
	table   codexSpan
	env     codexSpan
	anomaly codexAnomaly
}

// codexManagedSpans — 라인 목록에서 두 관리 테이블의 구간을 잡는다.
func codexManagedSpans(lines [][]byte) codexSpans {
	var out codexSpans
	var sc tomlLineScanner
	cur := -1 // 0=table, 1=env, -1=없음
	for i, ln := range lines {
		boundary, name := sc.step(ln)
		if !boundary {
			continue
		}
		switch cur {
		case 0:
			out.table.end = i
		case 1:
			out.env.end = i
		}
		cur = -1
		switch name {
		case codexManagedTable:
			if out.table.found {
				out.anomaly = anomalyDupHeader
			}
			out.table, cur = codexSpan{start: i, end: len(lines), found: true}, 0
		case codexManagedEnv:
			if out.env.found {
				out.anomaly = anomalyDupHeader
			}
			out.env, cur = codexSpan{start: i, end: len(lines), found: true}, 1
		}
	}
	// 유효 TOML이면 EOF에서 스캐너가 닫힌다 — 열려 있으면 닫히지 않은 문자열·배열이 있어 그 뒤
	// 헤더가 경계로 잡히지 않았고 우리 구간이 EOF까지 늘어난 상태다. 그대로 기입하면 append한
	// 헤더까지 그 구간에 삼켜져 재실행마다 테이블이 하나씩 늘고, 매 실행 Changed=true라 D84
	// 단일 백업 슬롯이 2회차에 원본을 잃는다(적대적 리뷰 A2). 무변경 경로로 뺀다.
	// 중복 헤더가 이미 잡혔으면 그 사유를 유지한다 — 구조적으로 더 근본이고, 사용자가 먼저
	// 고쳐야 하는 것이 그쪽이다.
	if sc.open() && out.anomaly == anomalyNone {
		out.anomaly = anomalyScannerOpen
	}
	// 우리 구간 안의 정규화 불가 키(D87) — 구간이 확정된 뒤에 본다. 앞의 두 사유가 이미
	// 잡혔으면 유지한다(그쪽이 구조적으로 더 근본이다).
	if out.anomaly == anomalyNone && codexEscapedKeyInSpans(lines, out) {
		out.anomaly = anomalyEscapedKey
	}
	return out
}

// codexEntries — 구간 안의 논리 엔트리를 (첫 줄, 마지막 줄) 인덱스 쌍으로 나눈다. 여러 줄
// 값은 한 엔트리다 — 소유 키의 한 줄만 지우면 남은 줄이 보존 라인으로 되살아나 문법이 깨진다.
func codexEntries(lines [][]byte, sp codexSpan) [][2]int {
	var out [][2]int
	var sc tomlLineScanner
	start := -1
	for i := sp.start + 1; i < sp.end; i++ {
		if start < 0 {
			start = i
		}
		sc.step(lines[i])
		if !sc.open() {
			out = append(out, [2]int{start, i})
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, [2]int{start, sp.end - 1})
	}
	return out
}

// codexKeyName — 정규화 라인에서 대입 키 이름을 뽑는다(따옴표 키도 벗긴다). 따옴표로 시작하는
// 키는 첫 '='가 아니라 **닫는 따옴표 뒤**에서 '='를 찾는다(T3-F1) — 첫 '=' 무조건 분리로는
// 따옴표 키 안의 '='가 이름을 조기 절단해 사용자 키 `"args=x" = "y"`가 예약어 "args"로
// 오분류되고, codexReadTable의 continue 세 곳과 codexEnvBody의 표식 건너뛰기가 그 줄을 보존
// 목록에서 빼 재기입 때 조용히 지운다. basicStringLen·literalStringLen로 닫는 따옴표를 찾는다 —
// tomlKeyLen과 같은 이스케이프 인지 기준을 공유한다. '='가 없거나 앞부분이 비면 ""(키 줄이
// 아니다). 주석 줄은 '#'로 시작하므로 우리 키 이름과 절대 같아지지 않는다 — 주석은 언제나
// 보존 라인으로 간다.
func codexKeyName(s string) string {
	if s == "" {
		return ""
	}
	i := strings.Index(s, "=")
	switch s[0] {
	case '"':
		i = basicStringLen(s)
	case '\'':
		i = literalStringLen(s)
	}
	if i <= 0 || i >= len(s) || s[i] != '=' {
		return ""
	}
	return strings.Trim(s[:i], `"'`)
}

// tomlKeyTokenHasEscape — 정규화 문자열 s의 **맨 앞이 따옴표 키 표기**이고 그 안에 역슬래시가
// 있는가(D87). TOML 기본 문자열 키는 이스케이프를 디코드하므로 그런 표기는 우리 키와 **같은
// 키**가 될 수 있는데 codexKeyName은 이스케이프를 해석하지 않아 알아보지 못한다.
// 홑따옴표(리터럴) 키에는 이스케이프가 없으므로 대상이 아니다.
func tomlKeyTokenHasEscape(s string) bool {
	if s == "" || s[0] != '"' {
		return false
	}
	n := basicStringLen(s)
	if n < 2 || s[n-1] != '"' {
		return false // 미종료 문자열 — 우리가 다루지 않는 형태(다른 사유가 잡는다)
	}
	// 닫는 따옴표 뒤에 '='가 와야 **키**다. 이 검사가 없으면 값이 키로 오인된다 — 배열 원소의
	// Windows 경로(args = ["--store-root", "C:\ctr"])가 대표 형태이고, 그러면 그 사용자의
	// install·uninstall·--fix가 영구 무변경으로 굳는다. 정규화(stripLine)가 공백을 지우므로
	// 키와 '=' 사이에 공백이 없다 — tomlKeyLen·tomlInlineValue가 쓰는 것과 같은 전제다.
	if n >= len(s) || s[n] != '=' {
		return false
	}
	return strings.Contains(s[1:n-1], `\`)
}

// stripTrailingComment — 정규화 문자열에서 **문자열 밖** '#'부터를 잘라 낸다(D87 오탐 방지).
// 후행 주석은 TOML 데이터가 아니므로 그 안의 `"이름" = 값` 모양이 인라인 테이블의 키로 잡히면
// 정상 파일이 이상으로 판정되고 그 사용자의 install·uninstall·--fix가 영구 무변경으로 굳는다.
// 따옴표 건너뛰기 걸음은 tomlStringList·tomlLineScanner.advance가 쓰는 것과 **같은 기준**이다 —
// 홑따옴표 리터럴 안의 '#'은 주석이 아니므로 거기서 자르면 그 뒤의 진짜 키를 놓친다.
func stripTrailingComment(s string) string {
	for i := 0; i < len(s); {
		switch s[i] {
		case '"':
			i += basicStringLen(s[i:])
		case '\'':
			i += literalStringLen(s[i:])
		case '#':
			return s[:i]
		default:
			i++
		}
	}
	return s
}

// codexEscapedKeyInSpans — 우리 두 구간 안에 정규화 불가 키 표기가 있는가(D87).
// **엔트리 단위로 본다.** codexEntries가 여러 줄 값을 한 엔트리로 묶고 그 엔트리에서 키는
// 맨 앞 토큰뿐이므로, 문자열 값 안이나 후행 주석 안의 `"이름" = 값` 모양이 키로 오인되지
// 않는다 — codexReadTable이 키를 읽는 경로와 **같은 기준**이며, 두 경로가 다른 기준을 쓰면
// 같은 파일을 두 방식으로 읽는 셈이다(라인을 문맥 없이 훑으면 여러 줄 문자열의 내용 줄·값 뒤
// 후행 주석·홑따옴표 값 내부가 모두 오탐이 된다).
// 인라인 테이블의 키 토큰은 **env 엔트리에서만** 본다 — tomlKeyLen·inlineMarkerSpan이 그 줄에만
// 적용되는 것과 같은 한정이고, 그것이 없으면 다른 키의 값 안에 든 쉼표 뒤 텍스트가 키로 잡힌다.
// **알려진 한계**: env 인라인 값 안에 따옴표·역슬래시·등호를 함께 담은 문자열은 여전히 키로
// 보인다. tomlInlineValue·inlineMarkerSpan이 이미 같은 한계를 갖는 형태이며(인라인 테이블의
// 구조를 따라가지 않는다) D80의 파서 비의존 원칙 아래에서는 그 한계가 계약이다.
func codexEscapedKeyInSpans(lines [][]byte, sp codexSpans) bool {
	for _, span := range []codexSpan{sp.table, sp.env} {
		if !span.found {
			continue
		}
		for _, e := range codexEntries(lines, span) {
			joined := ""
			for i := e[0]; i <= e[1]; i++ {
				joined += stripLine(lines[i])
			}
			if tomlKeyTokenHasEscape(joined) {
				return true
			}
			if codexKeyName(joined) != "env" {
				continue
			}
			// 인라인 키 토큰은 **후행 주석을 뺀** 부분에서만 본다 — 주석 안의 키 모양까지 잡으면
			// `env = { A = "1" } # TODO: , "C:\t" = 2` 같은 정상 파일이 이상이 된다.
			inline := stripTrailingComment(joined)
			for j := 0; j < len(inline); j++ {
				if (inline[j] == '{' || inline[j] == ',') && tomlKeyTokenHasEscape(inline[j+1:]) {
					return true
				}
			}
		}
	}
	return false
}

// tomlStringList — 정규화 문자열에서 따옴표 안 값을 순서대로 뽑는다. 배열의 줄바꿈·공백·
// 후행 쉼표가 달라도 값 동치를 판정할 수 있게 하는 최소 정규화이며 **비교 전용**이다 —
// stripLine이 값 안의 공백까지 지우므로 되쓰기에는 쓰지 않는다.
func tomlStringList(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		var n int
		switch s[i] {
		case '"':
			n = basicStringLen(s[i:])
		case '\'':
			n = literalStringLen(s[i:])
		default:
			i++
			continue
		}
		if n < 2 {
			i++
			continue
		}
		out = append(out, s[i+1:i+n-1])
		i += n
	}
	return out
}

// isBlankLine — 종결자 제외 내용이 공백뿐이면 빈 줄(uninstall 직전-빈줄 판정).
func isBlankLine(line []byte) bool {
	return strings.TrimSpace(string(line)) == ""
}

// classifyMarkers — 마커 정확 라인 매치로 배치 분류(계약 2). 소유 replace는 begin/end 인덱스도 반환.
func classifyMarkers(lines [][]byte) (class markerClass, begin, end int) {
	var begins, ends []int
	var sc tomlLineScanner
	for i, ln := range lines {
		// 여러 줄 문자열·배열 **안**의 줄은 마커가 아니다 — 그 내용이 마커 텍스트와 같으면
		// 마커 쌍으로 세어져 사이의 **미소유** 테이블이 소유로 판정되고, install이 사용자
		// command를 덮고 uninstall이 그 테이블을 통째로 지웠다(Codex 교차 리뷰 C1).
		// codexManagedSpans가 경계를 잡는 것과 같은 기준을 공유한다.
		inString := sc.open()
		sc.step(ln)
		if inString {
			continue
		}
		switch trimEOL(ln) {
		case codexBlockBegin:
			begins = append(begins, i)
		case codexBlockEnd:
			ends = append(ends, i)
		}
	}
	if len(begins) == 0 && len(ends) == 0 {
		return classAppend, 0, 0
	}
	if len(begins) == 1 && len(ends) == 1 && begins[0] < ends[0] && blockOwns(lines, begins[0], ends[0]) {
		return classReplace, begins[0], ends[0]
	}
	return classAnomaly, 0, 0
}

// blockOwns — 블록 내부에 정규화 "[mcp_servers.ctr]" 라인이 실존해야 소유(본문 검증=소유).
func blockOwns(lines [][]byte, begin, end int) bool {
	for i := begin + 1; i < end; i++ {
		if stripLine(lines[i]) == "[mcp_servers.ctr]" {
			return true
		}
	}
	return false
}

// ctrKeySignal — 정규화 라인의 ctr 키-경계 신호(계약 3b). 오탐("electron"·"spectra") 회피용.
func ctrKeySignal(s string) bool {
	return strings.Contains(s, "ctr]") ||
		strings.Contains(s, "ctr\"") ||
		strings.Contains(s, "ctr'") ||
		strings.Contains(s, "ctr=") ||
		strings.HasPrefix(s, "ctr.") ||
		strings.Contains(s, ".ctr.")
}

// mcpServersAssign — 정규화 라인이 루트 mcp_servers 대입 정의인지(계약 3b 보강, Codex P1). 인라인
// 테이블 대입은 자기완결이라 뒤에 [mcp_servers.ctr] 서브테이블 헤더를 붙이면 중복 정의 파스
// 에러 → 사용자 Codex 전체 파손. 대입 자체가 정의이므로 ctr 신호 없이 단독 충돌이다. 헤더
// ([mcp_servers])는 정규화가 "["로 시작해 여기 걸리지 않으며 [mcp_servers.ctr] 확장이 유효하다.
func mcpServersAssign(s string) bool {
	return strings.HasPrefix(s, "mcp_servers=") ||
		strings.HasPrefix(s, "\"mcp_servers\"=") ||
		strings.HasPrefix(s, "'mcp_servers'=")
}

// scanOutsideSpans — 우리 두 구간 **밖**의 중복 정의 신호(D80 — D48의 두 검사를 경계 근거만
// "마커 블록 밖"에서 "우리 관리 테이블 구간 밖"으로 바꿔 승계한다).
// ① mcpServersAssign — 루트 mcp_servers 인라인 대입은 그 대입 자체가 정의라, 뒤에
// [mcp_servers.ctr] 헤더를 붙이는 순간 중복 정의 파스 에러가 되어 사용자 Codex 전체가 깨진다.
// ② ctrKeySignal — 우리 구간 밖의 ctr 키-경계 신호. 관리 테이블 이름이 ctr 그대로이므로 그
// 리터럴은 새 경계 방식에서 다시 표현할 필요가 없다.
// 구간은 **이름**으로 잡으므로 소유 여부와 무관하게 제외된다 — 우리 이름 자리에 남의 항목이
// 있는 상태는 충돌이 아니라 mcpExistingHeader로 보고한다(v0.14의 hasHeader > conflict 우선순위 승계).
func scanOutsideSpans(lines [][]byte, sp codexSpans) bool {
	var hasMcp, hasSignal, assign bool
	for i, ln := range lines {
		if inCodexSpan(sp.table, i) || inCodexSpan(sp.env, i) {
			continue
		}
		s := stripLine(ln)
		if strings.Contains(s, "mcp_servers") {
			hasMcp = true
		}
		if ctrKeySignal(s) {
			hasSignal = true
		}
		if mcpServersAssign(s) {
			assign = true
		}
	}
	return (hasMcp && hasSignal) || assign
}

func inCodexSpan(sp codexSpan, i int) bool { return sp.found && i >= sp.start && i < sp.end }

// codexSplice — [start, end) 구간을 body로 갈아 끼우는 편집.
type codexSplice struct {
	start, end int
	body       []byte
}

// spliceCodexLines — 구간들을 새 바이트로 갈아 끼우고 drop에 든 라인을 지운다. 구간은 겹치지
// 않으며 start 오름차순으로 적용한다. 그 밖의 라인은 **바이트 그대로** 옮긴다.
func spliceCodexLines(lines [][]byte, edits []codexSplice, drop map[int]bool) []byte {
	slices.SortFunc(edits, func(a, b codexSplice) int { return a.start - b.start })
	var out []byte
	i := 0
	for _, e := range edits {
		for ; i < e.start; i++ {
			if !drop[i] {
				out = append(out, lines[i]...)
			}
		}
		out = append(out, e.body...)
		i = e.end
	}
	for ; i < len(lines); i++ {
		if !drop[i] {
			out = append(out, lines[i]...)
		}
	}
	return out
}

// codexInstallRequest — 관리 테이블 기입 요청(D80·D81·D86).
type codexInstallRequest struct {
	Profiles   []string // 명시 플래그로 정해진 프로필(SetProfile=false면 쓰이지 않는다)
	SetProfile bool     // --enable/--enable-exec 중 하나라도 있었는가
	Marker     string   // env.CTR_MANAGED 값(hookMarker(version))
	// MarkerOnly — 표식과 command만 맞추고 args·enabled_tools는 원문을 보존한다(D86).
	// doctor --fix가 세우며 install은 세우지 않는다: install은 "등록을 원하는 상태로 만든다"이고
	// --fix는 "표식을 현재 버전으로"라, 같은 함수를 쓰면서 그 차이를 인자로 표현하지 않으면
	// --fix가 사용자가 손으로 넓힌 enabled_tools를 프로필 기본값으로 되돌린다.
	// **첫 기입 경로에서는 무시된다** — 보존할 원문이 없고, --fix는 등록을 만들지 않는다.
	MarkerOnly bool
}

// codexInstallResult — 기입 결과. Changed=false면 Out은 existing과 같고, 호출자는 쓰기와
// 백업을 **모두** 생략한다(D84 단일 슬롯 계약).
type codexInstallResult struct {
	Out      []byte
	State    codexMCPState
	Changed  bool
	Profiles []string // 실제로 기입한 프로필(설치기 안내용) — ArgsKept면 되읽기 실패 값(nil)이고, MarkerOnly면 기입하지 않으므로 nil이다.
	// "산출물이 exec를 노출하는가"를 물으려면 Profiles가 아니라 ExecExposed를 봐야 한다.
	ArgsKept bool // 되읽지 못해 args·enabled_tools를 손대지 않았다(D81)
	// ExecExposed — 이 실행이 남기는 산출물(Out)의 enabled_tools가 exec 도구를 담는가(D81,
	// 리뷰 승격 — 이월 T4-F3의 근본 픽스). ArgsKept일 때도 실제 값을 낸다: 그때 Profiles는
	// 되읽기 실패로 nil이라 "요청 프로필에 exec가 있는가"를 답하지 못하지만, ExecExposed는
	// 보존되는 enabled_tools를 직접 봐서 "산출물이 실제로 노출하는가"를 답한다.
	ExecExposed bool
	// TableFound — 관리 테이블이 구간으로 잡혔는가(D85). doctor가 "등록물이 있는가"를 묻는
	// 판정을 이 값으로 답한다 — 별도 판독기(codexConfigMarker)에서 얻으면 판정원이 둘이 되고
	// 그 둘이 갈리는 것이 이 릴리스가 닫는 어긋남이다. mcpWritten이면서 이 값이 거짓인 상태가
	// "append하면 된다"이며, --fix는 등록을 만들지 않으므로 그 상태에서 기입하지 않는다.
	TableFound bool
}

// installCodexConfigBlock — 관리 테이블 병합(스펙 v0.15 §0 D80·D81·D84). 순수 변환: 파일 IO 없음.
// 판정 순서는 probeCodexMCPBlock과 1:1로 유지한다 — 중복 정의 > 구간 밖 충돌 > 소유.
// **읽기 전용 경로가 이 함수를 부른다**(D85 — doctor [20]의 감지원). 그러므로 파일 IO·시간·난수를
// 들이면 진단이 파일을 쓰거나 결정적이지 않게 된다. 프로필 헬퍼를 포함한 호출 트리 전체가 순수해야
// 하며, 그 조건은 스펙 v0.16 §1.3 게이트 1과 §3 표4에 있다.
func installCodexConfigBlock(existing []byte, req codexInstallRequest) codexInstallResult {
	lines := splitLinesKeepEnds(existing)
	sp := codexManagedSpans(lines)
	if sp.anomaly != anomalyNone {
		return codexInstallResult{Out: existing, State: mcpMarkerAnomaly, TableFound: sp.table.found}
	}
	if scanOutsideSpans(lines, sp) {
		return codexInstallResult{Out: existing, State: mcpConflict, TableFound: sp.table.found}
	}
	view := codexReadTable(lines, sp.table)
	marker, markerFound := codexMarkerValue(lines, sp, view)
	class, begin, end := classifyMarkers(lines)
	inOldBlock := class == classReplace && sp.table.found && sp.table.start > begin && sp.table.start < end
	if sp.table.found && !codexOwnership(marker, markerFound, view.command, inOldBlock) {
		// 판정 근거는 "블록 밖에 있음"이 아니라 "표식이 없고 명령도 우리 것이 아님"이다(D80).
		return codexInstallResult{Out: existing, State: mcpExistingHeader, TableFound: sp.table.found}
	}
	// 프로필 우선순위(D81): 명시 플래그 > 우리 소유 테이블의 기존 args > 기본 프로필.
	// Codex 갈래에는 은퇴 이름이 없어 .mcp.json의 셋째 항이 없다.
	profiles, argsKept := canonicalProfiles(req.Profiles), false
	if !req.SetProfile {
		if sp.table.found {
			p, ok := profilesFromArgs(view.args)
			profiles, argsKept = p, !ok
		} else {
			profiles = defaultMCPProfiles
		}
	}
	// keepArgs — 몸통 조립에서 args·enabled_tools를 보존 라인으로 되돌리는가. 두 사유가 여기서
	// 합류한다: 되읽기 실패(D81 argsKept)와 표식 전용 요청(D86 MarkerOnly). 결과 필드 ArgsKept는
	// **앞의 것만** 담는다 — 합치면 설치기 안내가 "프로필을 해석하지 못했다"를 잘못 말한다.
	keepArgs := argsKept || req.MarkerOnly
	// execExposed — 아래 세 mcpWritten 반환 모두가 공유하는 산출물 exec 노출 판정(리뷰 승격 —
	// 이월 T4-F3의 근본 픽스). **기존 테이블이 있고** 보존 갈래일 때만 view.tools가 최종값이다.
	// 테이블이 없으면 첫 기입 경로가 몸통을 profiles로 조립하므로(MarkerOnly는 그 경로에서
	// 무시된다) 보존 목록을 보면 결과 필드가 산출물과 어긋난다 — view가 비어 있어 늘 false가
	// 된다. 아니면(첫 기입 포함) enabledToolsForProfiles(profiles)가 최종값이다 —
	// codexTableBody의 keepArgs 인자가 정확히 이 두 갈래로 몸통을 고르는 것과 같은 분기다.
	execExposed := enabledToolsExposeExec(enabledToolsForProfiles(profiles))
	if keepArgs && sp.table.found {
		execExposed = enabledToolsExposeExec(view.tools)
	}
	// resultProfiles — 설치기 안내가 읽는 "실제로 기입한 프로필". 표식 전용 갈래는 프로필을
	// 기입하지 않으므로 그 값이 없다(D86) — 되읽은 값을 실으면 그 필드의 의미가 성립하지 않는다.
	resultProfiles := profiles
	if req.MarkerOnly {
		resultProfiles = nil
	}
	crlf := bytes.Contains(existing, []byte("\r\n"))
	eol := "\n"
	if crlf {
		eol = "\r\n"
	}
	if !sp.table.found { // 첫 기입 — 관리 테이블을 EOF 뒤에 잇는다
		// MarkerOnly는 여기서 무시한다 — 보존할 원문이 없고 --fix는 등록을 만들지 않는다(D86).
		body := ensureEOL(codexTableBody(lines, codexSpan{}, view, profiles, false, req.Marker, eol), eol)
		base := existing
		if sp.env.found {
			// 부모 테이블 없이 [mcp_servers.ctr.env]만 있는 파일 — 새 env 헤더를 덧붙이면 같은
			// 헤더가 두 번 정의돼 D80이 막으려는 파스 에러가 난다(scanOutsideSpans는 그 구간을
			// 제외하므로 걸러 내지 못한다). 기존 구간을 갈아 끼우고 헤더는 붙이지 않는다.
			// 산출은 env 서브테이블이 부모보다 앞서는 형태인데 TOML은 그 순서를 허용한다.
			base = spliceCodexLines(lines,
				[]codexSplice{{sp.env.start, sp.env.end, codexEnvBody(lines, sp.env, req.Marker, eol)}}, nil)
		} else {
			body = append(body, codexEnvBody(lines, codexSpan{}, req.Marker, eol)...)
		}
		return codexInstallResult{Out: appendBlock(base, body, crlf), State: mcpWritten, Changed: true, Profiles: resultProfiles, ExecExposed: execExposed, TableFound: sp.table.found}
	}
	// 무변경 판정(D84): 우리 소유 키 넷의 값이 모두 같고, 새로 만들 테이블도 지울 마커 줄도
	// 없으면 쓰기와 백업을 생략한다. **키 단위 동치는 바이트 동일을 포함**하므로 호스트가 우리
	// 테이블을 다른 형태(키 순서·인용·공백·env 표기)로 되썼을 때에도 무변경 재실행마다 .bak이
	// 생기지 않는다 — 스펙 §1.3-1 ② 게이트의 두 갈래를 한 경로가 함께 만족한다.
	envMissing := !sp.env.found && view.inlineEnv < 0
	// keepArgs 갈래에서는 args·enabled_tools를 비교하지 않는다 — 보존하는 값을 비교하면 사용자가
	// 넓힌 목록이 매 실행 "다르다"로 읽혀 .bak이 매번 새로 생기고 D84 단일 슬롯 계약이
	// 무의미해진다.
	ownedSame := view.command == hookBinaryName && marker == req.Marker &&
		(keepArgs || (slices.Equal(view.args, mcpArgsForProfiles(profiles)) &&
			slices.Equal(view.tools, enabledToolsForProfiles(profiles))))
	if !envMissing && !inOldBlock && ownedSame {
		return codexInstallResult{Out: existing, State: mcpWritten, Profiles: resultProfiles, ArgsKept: argsKept, ExecExposed: execExposed, TableFound: sp.table.found}
	}
	if inOldBlock {
		// D84 마이그레이션 — 마커 두 줄이 **우리 구간 안**에 들어와 있으면(블록이 우리 테이블만
		// 감싼 형태, §3 표4) 아래 drop 맵으로는 지워지지 않는다: spliceCodexLines는 편집 구간
		// 안에서 drop을 보지 않고 body를 통째로 얹기 때문이다. 그래서 keep에서 먼저 뺀다.
		// 구간 밖에 있는 마커는 여전히 drop이 지운다 — 두 배치가 서로를 대신하지 않는다.
		view.keep = slices.DeleteFunc(view.keep, func(i int) bool { return i == begin || i == end })
	}
	body := codexTableBody(lines, sp.table, view, profiles, keepArgs, req.Marker, eol)
	if envMissing { // env 서브테이블을 우리 구간 끝에 잇는다(다음 테이블 헤더 앞이라 안전하다)
		body = append(ensureEOL(body, eol), codexEnvBody(lines, codexSpan{}, req.Marker, eol)...)
	}
	edits := []codexSplice{{sp.table.start, sp.table.end, body}}
	if sp.env.found {
		edits = append(edits, codexSplice{sp.env.start, sp.env.end, codexEnvBody(lines, sp.env, req.Marker, eol)})
	}
	drop := map[int]bool{}
	if inOldBlock { // D84 마이그레이션 — 추가로 지우는 것은 **마커 두 줄뿐**이다
		drop[begin], drop[end] = true, true
	}
	// Changed는 **기입 바이트와 기존 바이트의 비교**로 정한다 — D84의 1차 기준이 그것이고,
	// 위 ownedSame(키 단위 동치)은 재직렬화 때문에 바이트 비교가 성립하지 않을 때의 하강
	// 경로다. 여기를 참으로 고정하면 ownedSame이 접지 못하는 상태가 그대로 새어 나간다:
	// 표식 키는 있는데 값이 문자열이 아니면 setInlineEnvMarker가 그 줄을 보존하므로 marker가
	// 영영 req.Marker와 같아지지 않고, 그런 파일은 무변경 재실행마다 .bak이 다시 생겨
	// 단일 슬롯 계약이 무의미해진다.
	out := spliceCodexLines(lines, edits, drop)
	return codexInstallResult{
		Out: out, State: mcpWritten,
		Changed: !bytes.Equal(out, existing), Profiles: resultProfiles, ArgsKept: argsKept, ExecExposed: execExposed,
		TableFound: sp.table.found,
	}
}

// probeCodexMCPBlock — doctor [16] 존재 판별(D52 승계). install 상태기계는 "무엇을 쓸지"의
// 분류라 존재/부재 판별에 부적합하므로 같은 순수 헬퍼를 재사용해 읽기 전용으로 판정한다.
// 우선순위는 installCodexConfigBlock과 1:1이다 — 중복 정의 > 구간 밖 충돌 > 테이블 존재.
// 소유 여부는 보지 않는다: 우리 이름 자리에 남의 항목이 있어도 "그 이름의 등록은 있다"가
// 맞고, install은 그 상태를 mcpExistingHeader로 따로 보고한다.
// anomaly는 불리언이 아니라 **사유**다(D85) — [16]이 사용자에게 조치를 지목해야 한다. 구간 밖
// 충돌은 사유가 아니라 별개 상태이므로 anomalyDupHeader로 접지 않고 그대로 둔다: 접으면 헤더가
// 하나뿐인 파일에 "하나만 남기고 지우세요"라는 존재하지 않는 조치를 지시한다.
func probeCodexMCPBlock(existing []byte) (present bool, anomaly codexAnomaly) {
	lines := splitLinesKeepEnds(existing)
	sp := codexManagedSpans(lines)
	if sp.anomaly != anomalyNone {
		return false, sp.anomaly
	}
	if scanOutsideSpans(lines, sp) {
		return false, anomalyOutsideConflict
	}
	return sp.table.found, anomalyNone
}

// appendBlock — 계약 4: 빈 파일은 블록만; 그 외 EOF 개행 정규화 후 구분 빈 줄 1개+블록.
func appendBlock(existing, block []byte, crlf bool) []byte {
	if len(existing) == 0 {
		return block
	}
	eol := []byte("\n")
	if crlf {
		eol = []byte("\r\n")
	}
	out := append([]byte{}, existing...)
	if out[len(out)-1] != '\n' {
		out = append(out, eol...) // 무개행 EOF 정규화(유일 허용 블록 밖 변경)
	}
	out = append(out, eol...) // 구분 빈 줄
	return append(out, block...)
}

// uninstallCodexConfigBlock — 소유한 관리 테이블 **두 구간만** 제거한다(D80). 구 형식
// 파일에서는 마커 두 줄도 함께 지우되 **블록 통째가 아니라 그 안의 우리 테이블만** 지운다 —
// 그래서 밀림 파일에서 블록 사이에 들어온 사용자 테이블이 살아남는다(D84). 구간 직전의 빈 줄
// 1개는 함께 지운다(append가 넣은 구분 줄의 대칭).
func uninstallCodexConfigBlock(existing []byte) (out []byte, changed bool) {
	lines := splitLinesKeepEnds(existing)
	sp := codexManagedSpans(lines)
	if sp.anomaly != anomalyNone || !sp.table.found {
		return existing, false
	}
	view := codexReadTable(lines, sp.table)
	marker, markerFound := codexMarkerValue(lines, sp, view)
	class, begin, end := classifyMarkers(lines)
	inOldBlock := class == classReplace && sp.table.start > begin && sp.table.start < end
	if !codexOwnership(marker, markerFound, view.command, inOldBlock) {
		return existing, false
	}
	drop := map[int]bool{}
	for i := sp.table.start; i < sp.table.end; i++ {
		drop[i] = true
	}
	for i := sp.env.start; sp.env.found && i < sp.env.end; i++ {
		drop[i] = true
	}
	if inOldBlock {
		drop[begin], drop[end] = true, true
	}
	if sp.table.start > 0 && !drop[sp.table.start-1] && isBlankLine(lines[sp.table.start-1]) {
		drop[sp.table.start-1] = true
	}
	for i, ln := range lines {
		if !drop[i] {
			out = append(out, ln...)
		}
	}
	return out, true
}

// codexConfigPath — $CODEX_HOME/config.toml, 미설정 시 ~/.codex/config.toml
// (codexHooksPath의 CODEX_HOME 규칙 승계 — 무성 오설치 방지 P2 교훈).
func codexConfigPath() (string, error) {
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		return filepath.Join(codexHome, "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("codex: 홈 디렉터리 해석 실패")
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

// codexConfigMarker — config.toml 관리 테이블의 소유 표식과 command를 읽는다(D83 감지원).
// probe는 존재·부재·이상만 보므로 버전 비교에 쓸 수 없어 별도 판독기가 필요하다.
// found는 **관리 테이블이 중복 없이 하나 있다**는 뜻이고, 표식이 없으면 marker는 ""다.
func codexConfigMarker(existing []byte) (marker, command string, found bool) {
	lines := splitLinesKeepEnds(existing)
	sp := codexManagedSpans(lines)
	if sp.anomaly != anomalyNone || !sp.table.found {
		return "", "", false
	}
	view := codexReadTable(lines, sp.table)
	m, _ := codexMarkerValue(lines, sp, view)
	return m, view.command, true
}

// codexTableView — 관리 테이블 구간을 소유 키와 보존 라인으로 가른 결과.
type codexTableView struct {
	command string   // command 키의 문자열 값(부재면 "")
	args    []string // args 배열의 문자열 값. **부재와 [] 모두 nil**이다 — D80이 둘을 동치로 본다
	tools   []string // enabled_tools 배열의 문자열 값
	// inlineEnv — 인라인 env 대입 줄 인덱스(-1이면 없음). 이 줄이 있으면 [mcp_servers.ctr.env]
	// 헤더를 새로 붙이지 않는다(중복 정의 금지) — 표식은 그 줄 안에서 갈아 끼운다.
	inlineEnv int
	// keep — 우리 소유가 아닌 라인. 원문 그대로 되돌린다.
	// argsLines·toolsLines — 되읽기 실패 시 keep에 합류해 args·enabled_tools도 원문으로 돌아간다.
	keep, argsLines, toolsLines []int
}

// codexReadTable — 관리 테이블 구간을 훑어 소유 키 값과 보존 라인을 가른다.
func codexReadTable(lines [][]byte, sp codexSpan) codexTableView {
	view := codexTableView{inlineEnv: -1}
	if !sp.found {
		return view
	}
	for _, e := range codexEntries(lines, sp) {
		joined := ""
		for i := e[0]; i <= e[1]; i++ {
			joined += stripLine(lines[i])
		}
		values := []string(nil)
		if eq := strings.Index(joined, "="); eq >= 0 {
			values = tomlStringList(joined[eq+1:])
		}
		switch codexKeyName(joined) {
		case "command":
			if len(values) > 0 {
				view.command = values[0]
			}
			continue
		case "args":
			view.args = values
			for i := e[0]; i <= e[1]; i++ {
				view.argsLines = append(view.argsLines, i)
			}
			continue
		case "enabled_tools":
			view.tools = values
			for i := e[0]; i <= e[1]; i++ {
				view.toolsLines = append(view.toolsLines, i)
			}
			continue
		case "env":
			// 인라인 env 대입은 보존 라인으로 두고 표식만 갈아 끼운다 — 이 줄이 있는데
			// [mcp_servers.ctr.env] 헤더를 새로 붙이면 중복 정의로 사용자 Codex가 깨진다.
			view.inlineEnv = e[0]
		}
		for i := e[0]; i <= e[1]; i++ {
			view.keep = append(view.keep, i)
		}
	}
	return view
}

// tomlKeyLen — s의 맨 앞이 key인가. TOML은 `CTR_MANAGED`와 `"CTR_MANAGED"`·`'CTR_MANAGED'`를
// **같은 키**로 읽으므로 세 표기를 모두 그 키로 본다 — 맨 키만 보면 따옴표 표기를 부재로 읽고
// 표식을 한 번 더 넣어 같은 인라인 테이블에 중복 키가 생긴다(D80이 막으려는 파스 에러 방향).
// 맞으면 그 키 표기의 길이(따옴표 포함), 아니면 -1이다. 서브테이블 경로의 codexKeyName이
// 따옴표를 벗기는 것과 **같은 기준**이며, 두 경로가 다른 기준을 쓰면 같은 파일을 두 방식으로
// 읽는 셈이다.
func tomlKeyLen(s, key string) int {
	for _, q := range []string{"", `"`, "'"} {
		if strings.HasPrefix(s, q+key+q) {
			return 2*len(q) + len(key)
		}
	}
	return -1
}

// tomlInlineValue — 인라인 테이블 대입 줄(정규화)에서 key의 문자열 값을 뽑는다. 키 경계는
// '{' 또는 ',' 직후로 한정한다 — 부분 문자열로 찾으면 다른 키의 값 안에 든 같은 이름까지 잡는다.
// 값은 '=' 바로 다음(정규화라 공백 없음)이 큰따옴표일 때만 읽는다 — inlineMarkerSpan과 같은
// 기준이다(T3-F2). '=' 뒤 나머지 전체를 tomlStringList에 넘기면 값이 문자열이 아닐 때 스캔이
// 다음 키의 문자열 값까지 집어삼켜 소유를 오판한다(예: `CTR_MANAGED = 0, X = "context-router/…"`
// 를 CTR_MANAGED의 값으로 오독). found는 키가 텍스트로 있는가이고, 값이 문자열이 아니면
// ("", true)다.
func tomlInlineValue(s, key string) (value string, found bool) {
	for i := 0; i < len(s); i++ {
		if s[i] != '{' && s[i] != ',' {
			continue
		}
		n := tomlKeyLen(s[i+1:], key)
		if n < 0 || i+1+n >= len(s) || s[i+1+n] != '=' {
			continue
		}
		v := i + 2 + n
		if v >= len(s) || s[v] != '"' {
			return "", true
		}
		// 미종료 문자열이면 basicStringLen이 len(s[v:])를 돌려주므로 하한과 닫힘을 함께 본다 —
		// 없으면 s[v+1:v+len-1]이 역순 슬라이스로 패닉하고 internal/cli에 recover가 없어
		// 프로세스가 죽는다(적대적 리뷰 A1). 이미 무효 TOML이니 tomlStringList·inlineMarkerSpan과
		// 같은 "다루지 않는 형태"로 뺀다.
		if vl := basicStringLen(s[v:]); vl >= 2 && s[v+vl-1] == '"' {
			return s[v+1 : v+vl-1], true
		}
		return "", true
	}
	return "", false
}

// codexMarkerValue — 소유 표식 값을 읽는다. [mcp_servers.ctr.env] 서브테이블과 관리 테이블
// 안의 인라인 env 대입 **두 형태를 모두** 인식한다(D80). found는 키가 있는가이고, 소유
// 판정은 값 기준(isOurMarkerValue)이라 키만 있고 값이 비면 소유가 아니다.
func codexMarkerValue(lines [][]byte, sp codexSpans, view codexTableView) (string, bool) {
	if sp.env.found {
		for _, e := range codexEntries(lines, sp.env) {
			joined := ""
			for i := e[0]; i <= e[1]; i++ {
				joined += stripLine(lines[i])
			}
			if codexKeyName(joined) != codexMarkerKey {
				continue
			}
			if v := tomlStringList(joined[strings.Index(joined, "=")+1:]); len(v) > 0 {
				return v[0], true
			}
			return "", true
		}
	}
	if view.inlineEnv >= 0 {
		return tomlInlineValue(stripLine(lines[view.inlineEnv]), codexMarkerKey)
	}
	return "", false
}

// codexOwnership — 관리 테이블의 소유 판정. D84가 **한 절로 격리**한 형태다:
//
//	소유 = env.CTR_MANAGED 값이 소유 기준을 만족(D82)
//	     || command가 hookBinaryName (D80 인수 절 — 재직렬화가 표식을 지운 파일의 복귀 경로)
//	     || 구 BEGIN/END 블록 안에 있는 우리 테이블 (D84 — v1.0에서 **이 절만** 지운다)
//
// 셋 다 **테이블 한정**이다: 테이블 한정 없는 위치 술어를 쓰면 밀림 파일에서 블록 안에 들어온
// 사용자 테이블까지 소유가 되고, 이름 한정 없는 명령 술어를 쓰면 [mcp_servers.ctr-exec]까지
// 소유가 된다. 앞의 두 절은 제거 대상이 아니다 — 첫 절의 정확 일치는 D82의 영구 본절이고,
// 둘째 절은 호스트 재직렬화가 표식을 다시 지울 때의 복귀 경로다.
func codexOwnership(marker string, markerFound bool, command string, inOldBlock bool) bool {
	if markerFound && isOurMarkerValue(marker) {
		return true
	}
	if command == hookBinaryName {
		return true
	}
	return inOldBlock
}

// tomlStringArray — 문자열 슬라이스를 TOML 배열 리터럴로 쓴다(우리가 기입하는 유일한 형태).
func tomlStringArray(v []string) string {
	q := make([]string, len(v))
	for i, s := range v {
		q[i] = `"` + s + `"`
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// ensureEOL — 바이트가 개행으로 끝나지 않으면 지배 개행을 붙인다(구간 뒤에 다른 테이블을
// 이어 붙일 때 두 줄이 붙는 것을 막는다).
func ensureEOL(b []byte, eol string) []byte {
	if len(b) > 0 && b[len(b)-1] != '\n' {
		return append(b, eol...)
	}
	return b
}

// inlineMarkerSpan — 인라인 env 대입 줄(**원문**)에서 key의 문자열 값 구간 [start,end)를
// 찾는다. 키 경계는 '{' 또는 ',' 다음의 첫 비공백 토큰으로 한정하고 따옴표 표기도 같은 키로
// 본다(tomlKeyLen) — 읽기(tomlInlineValue)와 되쓰기가 같은 키 기준을 써야 "부재로 읽고 다시
// 넣는" 중복 키가 생기지 않는다. 부분 문자열로 찾으면 다른 키의 값 안에 든 같은 이름까지
// 잡는다. 없거나 값이 문자열이 아니면 (-1,-1)이다. tomlInlineValue와 달리 정규화 문자열이
// 아니라 원문을 받는다 — 되쓰기는 원문 바이트를 보존해야 하므로 stripLine이 지운 공백 위치를
// 쓸 수 없다.
func inlineMarkerSpan(s, key string) (start, end int) {
	skipSpace := func(i int) int {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		return i
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '{' && s[i] != ',' {
			continue
		}
		j := skipSpace(i + 1)
		n := tomlKeyLen(s[j:], key)
		if n < 0 {
			continue
		}
		j = skipSpace(j + n)
		if j >= len(s) || s[j] != '=' {
			continue
		}
		j = skipSpace(j + 1)
		if j >= len(s) || s[j] != '"' {
			return -1, -1 // 키는 있으나 문자열 값이 아니다 — 우리가 다루지 않는 형태
		}
		return j, j + basicStringLen(s[j:])
	}
	return -1, -1
}

// setInlineEnvMarker — 인라인 env 대입 줄에 소유 표식을 심는다. 이미 있으면 **그 키의 값
// 구간만** 바꾸고, 없으면 여는 중괄호 뒤에 더한다(내부가 비면 쉼표를 붙이지 않는다 — TOML은
// 인라인 테이블의 후행 쉼표를 허용하지 않는다). 그 밖의 키는 원문 그대로 남는다. 키는 있는데
// 값이 문자열이 아니면 우리가 다루지 않는 형태이므로 원문을 그대로 둔다(중복 키 생성 금지) —
// 그 판정은 inlineMarkerSpan이 돌려주는 (-1,-1)이 내린다. **빈 문자열 값은 그 부류가 아니다**:
// 값 구간이 있으므로 제자리에서 현재 값으로 갱신한다 — 갱신하지 않으면 표식이 영영 현재 값이
// 되지 못하고, 그 상태가 D84 무변경 판정을 매번 어긋나게 한다.
// 값으로 첫 일치를 치환하지 않는 이유: 표식과 **값이 같은** 사용자 키가 앞서 있으면 그 값이
// 바뀌고 CTR_MANAGED는 옛 값으로 남는다 — 사용자 환경변수를 조용히 고치는 경로다.
func setInlineEnvMarker(line []byte, old string, oldFound bool, marker, eol string) []byte {
	s := trimEOL(line)
	if oldFound {
		if old == marker {
			return line
		}
		start, end := inlineMarkerSpan(s, codexMarkerKey)
		if start < 0 {
			return line
		}
		return []byte(s[:start] + `"` + marker + `"` + s[end:] + eol)
	}
	open := strings.Index(s, "{")
	last := strings.LastIndex(s, "}")
	if open < 0 || last < open {
		return line // 여러 줄 인라인 등 우리가 다루지 않는 형태 — 원문 보존
	}
	// 빈 여부는 여는 중괄호 뒤 첫 비공백 토큰으로 판정한다(T3-F3) — LastIndex(s, "}")를 경계로
	// 쓰면 후행 주석 안의 '}'(예: `env = {} # }`)를 닫는 중괄호로 오인해, 실제로는 빈 테이블인데
	// 내용이 있다고 보고 쉼표를 붙인다 — `{ CTR_MANAGED = "…",}`는 TOML이 금지하는 후행 쉼표다.
	sep := ","
	j := open + 1
	for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
		j++
	}
	if j < len(s) && s[j] == '}' {
		sep = ""
	}
	return []byte(s[:open+1] + " " + codexMarkerKey + ` = "` + marker + `"` + sep + s[open+1:] + eol)
}

// codexTableBody — 관리 테이블 구간의 새 내용(소유 키 + 보존 라인). 기존 테이블이면 헤더
// 라인을 원문 그대로 옮긴다 — 호스트가 헤더 공백을 바꿔 놓아도 그것만으로 재기입이 나지
// 않게 하는 지점이다. keepArgs면 args·enabled_tools를 보존 라인으로 되돌린다 — 호출자가 그
// 인자에 두 사유를 합류시킨다(D81 되읽기 실패·D86 표식 전용).
func codexTableBody(lines [][]byte, sp codexSpan, view codexTableView, profiles []string, keepArgs bool, marker, eol string) []byte {
	var b []byte
	if sp.found {
		b = append(b, lines[sp.start]...)
		b = ensureEOL(b, eol)
	} else {
		b = append(b, "["+codexManagedTable+"]"+eol...)
	}
	b = append(b, `command = "`+hookBinaryName+`"`+eol...)
	if !keepArgs {
		if a := mcpArgsForProfiles(profiles); len(a) > 0 {
			b = append(b, "args = "+tomlStringArray(a)+eol...)
		}
		b = append(b, "enabled_tools = "+tomlStringArray(enabledToolsForProfiles(profiles))+eol...)
	}
	keep := view.keep
	if keepArgs {
		keep = append(append(append([]int{}, keep...), view.argsLines...), view.toolsLines...)
		slices.Sort(keep)
	}
	for _, i := range keep {
		if i == view.inlineEnv {
			old, found := tomlInlineValue(stripLine(lines[i]), codexMarkerKey)
			b = append(b, setInlineEnvMarker(lines[i], old, found, marker, eol)...)
			continue
		}
		b = append(b, lines[i]...)
	}
	return b
}

// codexEnvBody — [mcp_servers.ctr.env] 구간의 새 내용. CTR_MANAGED만 우리 것이고 나머지
// 환경변수는 보존한다(D80).
func codexEnvBody(lines [][]byte, sp codexSpan, marker, eol string) []byte {
	var b []byte
	if sp.found {
		b = append(b, lines[sp.start]...)
		b = ensureEOL(b, eol)
	} else {
		b = append(b, "["+codexManagedEnv+"]"+eol...)
	}
	b = append(b, codexMarkerKey+` = "`+marker+`"`+eol...)
	if !sp.found {
		return b
	}
	for _, e := range codexEntries(lines, sp) {
		joined := ""
		for i := e[0]; i <= e[1]; i++ {
			joined += stripLine(lines[i])
		}
		if codexKeyName(joined) == codexMarkerKey {
			continue
		}
		for i := e[0]; i <= e[1]; i++ {
			b = append(b, lines[i]...)
		}
	}
	return b
}
