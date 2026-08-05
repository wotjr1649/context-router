package cli

// D48→D80 — config.toml 관리 단위를 주석 마커 블록에서 **TOML 테이블 경계**로 옮긴다(스펙
// v0.15 §0 D80·D84). 주석은 TOML 데이터가 아니라 호스트 재직렬화가 지우므로(§3 표1) 마커로는
// 관리 단위를 유지할 수 없다. TOML 파서 비의존(신규 의존 금지): 라인 스캐너가 여러 줄
// 문자열·배열의 열림 상태를 추적해 테이블 헤더만 경계로 잡고, 그 경계 안에서만 바이트를
// 바꾼다 — TOML 문법상 헤더와 다음 헤더 사이에는 그 테이블의 키만 올 수 있으므로 blast
// radius가 우리 테이블 안으로 구조적으로 묶인다. 순수 바이트 변환 — IO는 호출자.
//
// D88 — 왕복 보존의 예외: **우리가 기입하는 키 줄의 후행 주석은 보존 대상이 아니다.** 보존은
// 키 단위 약속이고 관리 키 넷의 물리 라인은 통째로 재생성되므로 그 줄의 주석은 사라진다 —
// keepArgs 갈래(D81 되읽기 실패·D86 표식 전용)에서는 args·enabled_tools가 재생성되지 않고
// 보존 라인(keep)으로 원문 그대로(주석 포함) 옮겨진다(codexTableBody). 인라인 env 경로는
// 별개 예외다 — setInlineEnvMarker는 값 구간을 갱신하거나 표식을 삽입하며
// 줄을 재생성하지 않으므로 그 줄의 주석이 남는다. 키별 값 스팬 판독기로 닫지 않는 이유는
// 파서 비의존 원칙 아래에서 그 판독기의 정확성이 새 위험이 되기 때문이다.

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
	mcpOutputInvalid                       // 산출물이 파스되지 않아 기입하지 않았다(D89) — 무변경·MCP 미확정
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
	// anomalyOutputInvalid — 우리가 만든 산출물이 파스되지 않았다(D89). 다른 사유와 성격이
	// 반대다: 나머지는 사용자 파일을 고치라는 지시이고 이것은 **우리 결함의 자수**다.
	anomalyOutputInvalid
	anomalyDottedEnv // env 키가 점 표기로 적혀 표식을 쓸 자리가 없다(D90)
	// anomalyNotOwned — 관리 테이블이 있으나 우리 소유가 아니다(**제거 경로 전용**). install은
	// 같은 상태를 mcpExistingHeader로 보고하므로 거기서 세우면 그 갈래의 문면이 뒤바뀐다.
	anomalyNotOwned
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
		return "파일 끝에서 여러 줄 문자열·배열 또는 인라인 테이블이 닫히지 않았습니다 — 닫은 뒤 재실행하세요"
	case anomalyEscapedKey:
		return "관리 테이블 안의 키가 이스케이프 표기로 적혀 있습니다 — 보통 표기(예: command)로 바꾸세요"
	case anomalyOutsideConflict:
		return "관리 테이블 밖에 ctr 관련 정의가 있습니다 — doctor 스니펫으로 수동 정리한 뒤 재실행하세요"
	case anomalyOutputInvalid:
		return "이 도구가 만든 결과가 유효한 TOML이 아니어서 기입하지 않았습니다 — 제품 결함이니 파일 형태와 함께 알려 주세요"
	case anomalyDottedEnv:
		return "env 키가 점 표기(env.NAME)로 적혀 있습니다 — [mcp_servers.ctr.env] 테이블로 옮긴 뒤 재실행하세요"
	case anomalyNotOwned:
		return "관리 테이블이 남아 있으나 표식도 command도 우리 것이 아닙니다 — 사용자 등록으로 보고 건드리지 않았습니다. 직접 정리하세요"
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

// trimBOM — 선두 UTF-8 BOM 세 바이트를 뗀다. **판정 경로에서만 쓴다.**
//
// Windows 편집기(PowerShell 5.1의 `Out-File -Encoding utf8`, 구버전 메모장)가 파일 첫 줄에
// 붙이는 바이트인데 U+FEFF는 Go의 `strings.Fields`가 보는 공백이 아니라 정규화가 그대로
// 통과시킨다. 그러면 **파일 첫 줄이 우리 테이블 헤더일 때**(`hook install --codex`가 만드는
// 파일이 그 형태다) 헤더로 인식되지 않아 우리 구간을 잃고, 그 구간의 키가 "구간 밖 ctr 정의"가
// 되어 재등록이 충돌로 막힌다 — 우리가 만든 파일에 편집기가 세 바이트를 붙였을 뿐인데.
// Codex 자신은 그 파일을 정상으로 읽으므로(v0.17 D89 개정 실측) 갈리는 것은 우리 판정뿐이다.
//
// **`trimEOL`에는 넣지 않는다.** 그 함수의 산출이 `setInlineEnvMarker`의 되쓰기 바이트가 되므로
// 거기서 떼면 우리가 사용자 파일의 인코딩을 조용히 바꾼다. 판정에서만 떼고 되쓰기는 원문
// 라인을 그대로 옮기는 것이 이 규칙의 형태다.
func trimBOM(b []byte) []byte { return bytes.TrimPrefix(b, []byte("\xEF\xBB\xBF")) }

// stripLine — 소유·충돌 판정용 정규화: 종결자 포함 공백 전부 제거. 선두 BOM도 뗀다(trimBOM).
func stripLine(line []byte) string {
	return strings.Join(strings.Fields(string(trimBOM(line))), "")
}

// tomlHeaderName — 정규화 라인이 단일 대괄호 테이블 헤더면 그 이름을, 아니면 빈 문자열을
// 돌려준다. **헤더 이름을 읽는 자리가 이 하나여야 한다** — 정규화 문자열을 `==`로 비교하면
// 후행 주석이 붙은 헤더에서 이름이 달라지고, 그 자리마다 갈린다(구 블록 소유 판정이 그래서
// 주석 한 줄에 닫혔다). `[[배열 테이블]]`은 우리 이름이 될 수 없으므로 이름을 비운다.
func tomlHeaderName(t string) string {
	if !strings.HasPrefix(t, "[") || strings.HasPrefix(t, "[[") {
		return ""
	}
	if i := strings.Index(t, "]"); i > 1 {
		return t[1:i]
	}
	return ""
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
	depth     int  // 여러 줄 배열 [ ] · 인라인 테이블 { } 균형
}

// inString — 앞 줄에서 시작한 여러 줄 문자열 안인가. open()과 달리 괄호 깊이를 보지
// 않는다 — 주석 절단을 결정하는 것은 문자열 상태뿐이고(D92 계약 6), 인라인 깊이가
// depth에 든 뒤로 open()은 여러 줄 배열·인라인 테이블의 이어지는 줄에서도 참이다.
// 그 줄의 후행 주석은 진짜 주석이므로 잘라야 한다.
func (s *tomlLineScanner) inString() bool { return s.inBasic || s.inLiteral }

// open — 이 줄이 앞 줄에서 시작한 여러 줄 값 안인가(헤더·키 판정을 하면 안 되는 상태).
func (s *tomlLineScanner) open() bool { return s.inString() || s.depth > 0 }

// step — 라인 하나를 소비한다. boundary는 이 줄이 테이블 경계인지, name은 단일 대괄호
// 헤더일 때의 정규화된 이름이다([[배열 테이블]]도 경계이지만 이름은 비운다 — 우리 이름이
// 될 수 없고, 경계로는 세야 앞 테이블의 구간이 거기서 끝난다).
func (s *tomlLineScanner) step(line []byte) (boundary bool, name string) {
	if !s.open() {
		t := stripLine(line)
		if strings.HasPrefix(t, "[") {
			boundary, name = true, tomlHeaderName(t)
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
		case (line[i] == '[' || line[i] == '{') && !header:
			s.depth++
			i++
		case (line[i] == ']' || line[i] == '}') && !header:
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

// codexKeyName — 대입 키 이름을 뽑는다(따옴표 키도 벗긴다). 문자열 밖 공백을 무시하므로
// 정규화 입력에서의 답이 전과 같고, 원문 입력에서도 같다. 따옴표로 시작하는 키는 첫 '='가
// 아니라 **닫는 따옴표 뒤**에서 '='를 찾는다(T3-F1) — 첫 '=' 무조건 분리로는
// 따옴표 키 안의 '='가 이름을 조기 절단해 사용자 키 `"args=x" = "y"`가 예약어 "args"로
// 오분류되고, codexReadTable의 continue 세 곳과 codexEnvBody의 표식 건너뛰기가 그 줄을 보존
// 목록에서 빼 재기입 때 조용히 지운다. basicStringLen·literalStringLen로 닫는 따옴표를 찾는다 —
// tomlScanInline의 키 토큰 건너뛰기와 같은 이스케이프 인지 기준을 공유한다. '='가 없거나
// 앞부분이 비면 ""(키 줄이
// 아니다). 주석 줄은 '#'로 시작하므로 우리 키 이름과 절대 같아지지 않는다 — 주석은 언제나
// 보존 라인으로 간다.
func codexKeyName(s string) string {
	t := strings.TrimLeft(s, " \t")
	if t == "" {
		return ""
	}
	i := strings.Index(t, "=")
	switch t[0] {
	case '"':
		i = basicStringLen(t)
	case '\'':
		i = literalStringLen(t)
	}
	// 닫는 따옴표 뒤의 공백을 건너뛴 자리에 '='가 있어야 키 줄이다.
	for i > 0 && i < len(t) && (t[i] == ' ' || t[i] == '\t') {
		i++
	}
	if i <= 0 || i >= len(t) || t[i] != '=' {
		return ""
	}
	return strings.Trim(strings.TrimSpace(t[:i]), `"'`)
}

// tomlDottedEnvKey — 원문 LHS가 `env.<둘째 마디>` 점 경로인가(D90). head는 첫 마디가 env일 때
// "env", 아니면 ""다. rest는 마디가 정확히 둘일 때 둘째 마디의 따옴표를 벗긴 이름이고, 마디가
// 셋 이상이면 ""다 — 그 형태도 env 서브테이블을 정의하지만 표식 자리는 아니다.
//
// **입력은 정규화 라인이 아니다.** stripLine은 따옴표 안 공백까지 지우므로 그 결과에 걸면
// `"e n v".CTR_MANAGED`가 env로 읽히고, 타인 테이블에 우리 표식이 선다. 문자열 밖 공백만
// 무시한 원문 LHS를 받는다.
func tomlDottedEnvKey(s string) (head, rest string) {
	i := tomlTopLevelEq(s)
	if i < 0 {
		return "", ""
	}
	segs := tomlSplitDotted(s[:i])
	// 따옴표 표기(`"env".FOO`)는 벗기면 같은 키이므로 여기서 인식한다. 반면 첫 마디가
	// 이스케이프를 담으면 벗겨도 env와 같아지지 않아 아래 비교가 그대로 배제한다 — 이 술어는
	// codexKeyName과 같이 이스케이프를 해석하지 않으며, 그 형태가 남기는 헤더 중복은 산출물
	// 유효성 게이트(D89)가 무변경으로 받아 낸다 — TestCodexDottedHead가 그 둘을 고정한다.
	if len(segs) < 2 || strings.Trim(segs[0], `"'`) != "env" {
		return "", ""
	}
	if len(segs) != 2 {
		return "env", ""
	}
	return "env", strings.Trim(segs[1], `"'`)
}

// tomlSplitDotted — LHS를 **따옴표 밖의 '.'**에서 마디로 가른다. 따옴표 안의 '.'는 이름의
// 일부이므로 단일 키 "env.FOO"가 점 경로로 오인되지 않는다. 각 마디의 앞뒤 공백은 지운다.
func tomlSplitDotted(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		switch s[i] {
		case '"':
			i += basicStringLen(s[i:])
		case '\'':
			i += literalStringLen(s[i:])
		case '.':
			out = append(out, strings.TrimSpace(s[start:i]))
			i++
			start = i
		default:
			i++
		}
	}
	return append(out, strings.TrimSpace(s[start:]))
}

// tomlTopLevelEq — 따옴표 밖 첫 '='의 위치(없으면 -1). LHS를 자르는 데만 쓴다.
func tomlTopLevelEq(s string) int {
	for i := 0; i < len(s); {
		switch s[i] {
		case '"':
			i += basicStringLen(s[i:])
		case '\'':
			i += literalStringLen(s[i:])
		case '=':
			return i
		default:
			i++
		}
	}
	return -1
}

// tomlKeyTokenHasEscape — 문자열 s의 **맨 앞이 따옴표 키 표기**이고 그 안에 역슬래시가
// 있는가(D87). 문자열 밖 공백은 codexKeyName과 **같은 규칙**으로 무시한다 — 정규화가 하던
// 일을 그 규칙이 그대로 하므로 정규화 입력에서의 답이 전과 같고, 원문 입력에서도 같다.
// 홑따옴표(리터럴) 키에는 이스케이프가 없으므로 대상이 아니다.
func tomlKeyTokenHasEscape(s string) bool {
	s = strings.TrimLeft(s, " \t")
	if s == "" || s[0] != '"' {
		return false
	}
	n := basicStringLen(s)
	if n < 2 || s[n-1] != '"' {
		return false // 미종료 문자열 — 우리가 다루지 않는 형태(다른 사유가 잡는다)
	}
	q := n // 닫는 따옴표 **뒤** 인덱스를 고정한다 — 마지막 슬라이스가 이 전제 위에 선다
	for n < len(s) && (s[n] == ' ' || s[n] == '\t') {
		n++
	}
	// 닫는 따옴표 뒤에 '='가 와야 **키**다. 이 검사가 없으면 값이 키로 오인된다 — 배열 원소의
	// Windows 경로(args = ["--store-root", "C:\ctr"])가 대표 형태이고, 그러면 그 사용자의
	// install·uninstall·--fix가 영구 무변경으로 굳는다.
	if n >= len(s) || s[n] != '=' {
		return false
	}
	return strings.Contains(s[1:q-1], `\`)
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

// codexValueText — **값 판독용 정규화**: 공백을 지우고(stripLine) 문자열 밖 주석을 잘라 낸다.
// 주석은 TOML 데이터가 아니므로 값 판독이 그것을 지나 스캔하면 후행 주석에 적은 문자열이 값이
// 된다 — 표식과 command가 그렇게 읽히면 주석 한 줄로 소유가 위조되고(그 값이 곧 소유 판정이다),
// enabled_tools가 그렇게 읽히면 주석으로 꺼 둔 도구가 목록에 남아 D91의 부족 감지와 exec 노출
// 안내가 함께 사실과 어긋난다. 판독 전용이다 — 되쓰기는 원문 바이트를 옮긴다(codexTableBody).
func codexValueText(line []byte) string {
	return stripTrailingComment(stripLine(line))
}

// codexEntryText — 논리 엔트리의 라인들을 값 판독용 정규화로 잇는다. **줄마다** 자르는 것이
// 계약이다 — 주석은 그 물리 라인의 끝까지이므로, 이은 문자열에서 한 번만 자르면 여러 줄 배열의
// 첫 주석 뒤에 있는 진짜 원소가 통째로 사라진다. **값 판독 전용 경로**다 — 남은 두 호출부
// (codexReadTable·codexMarkerValue)가 값만 여기서 읽고, 키는 codexEntryRaw가 읽는다. 둘이
// 갈리면 같은 엔트리를 두 기준으로 읽는 셈이다.
func codexEntryText(lines [][]byte, e [2]int) string {
	joined := ""
	for i := e[0]; i <= e[1]; i++ {
		joined += codexValueText(lines[i])
	}
	return joined
}

// codexEntryRaw — 논리 엔트리를 **원문 바이트로** 잇고, 각 물리 라인 조각의 시작 오프셋을
// 함께 낸다. `codexEntryText`와 달리 공백을 지우지 않는다 — 그 산출 위의 오프셋은 파일의
// 어느 바이트도 가리키지 않으므로 되쓰기에 쓸 수 없고, 같은 정규화가 따옴표 안 공백을 지워
// `"e n v"`를 `env`로 읽게 만든다(스펙 §1.2 잔여 ②).
//
// 자르는 것은 **문자열 밖 주석뿐**이며 물리 라인마다 자른다(codexEntryText와 같은 계약) —
// 이은 문자열에서 한 번만 자르면 여러 줄 배열의 첫 주석 뒤에 있는 진짜 원소가 사라진다.
// 종결자도 뗀다: 라인 조각은 `[0, 주석위치)`의 **접두 슬라이스**이므로 CRLF도 마지막 줄
// 종결자 없음도 접미 절단으로 함께 처리된다.
//
// **접두는 떼지 않는다.** trimBOM은 접두 절단이라 되사상을 깨는데(실측: off=0 → (0,0)에서
// 원문 0xEF와 joined 'e'가 어긋난다), 엔트리 첫 줄은 codexEntries가 sp.start+1부터 세므로
// 파일 0행이 될 수 없고 BOM은 0행 선두에만 있다 — 뗄 이유가 없다.
//
// **주석 절단은 이월된 문자열 상태가 결정한다**(스펙 §0 D92 계약 6). 무상태
// stripTrailingComment를 라인마다 부르면 여러 줄 기본 문자열 안의 '#'을 주석으로 잘라 그 뒤
// 바이트를 잃는다(실측: env = { A = """\nabc # def\n""" }가 '# def'와 그 뒤를 잃는다).
// tomlLineScanner가 이미 inBasic·inLiteral을 들고 있으므로 **그 걸음을 재사용한다** — 새
// 상태 기계를 세우면 같은 파일을 두 방식으로 읽는 셈이다.
// 알려진 한계: 상태는 **줄 머리에서만** 본다. 여러 줄 문자열이 닫히는 그 줄의 후행 주석은
// 잘리지 않는다 — 그 자리는 언제나 닫는 중괄호 뒤이므로 열거형이 보는 구간 밖이다.
func codexEntryRaw(lines [][]byte, e [2]int) (string, []int) {
	var b strings.Builder
	var sc tomlLineScanner // """·''' 열림을 라인 사이로 이월한다
	at := make([]int, 0, e[1]-e[0]+1)
	for i := e[0]; i <= e[1]; i++ {
		at = append(at, b.Len())
		s := trimEOL(lines[i])
		if sc.inString() { // 여러 줄 문자열 안이면 이 줄에 주석이 없다
			b.WriteString(s)
		} else {
			b.WriteString(stripTrailingComment(s))
		}
		sc.step(lines[i])
	}
	return b.String(), at
}

// codexPointAt — codexEntryRaw가 낸 오프셋을 파일 좌표로 되돌린다. 라인 조각이 접두
// 슬라이스이므로 `col = off - at[k]`가 곧 그 줄 안의 바이트 오프셋이다.
// 범위 밖 오프셋은 무효 지점이다 — 소비자가 그 값으로 스플라이스하지 않게 한다.
// **상한을 joinedLen으로 받는다**: 하한만 보면 임의의 큰 오프셋이 유효 좌표가 되고(실측:
// off=9999가 {line:0 col:9999} valid=true), 그 좌표로 스플라이스하면 패닉이다.
func codexPointAt(e [2]int, at []int, joinedLen, off int) tomlPoint {
	if off < 0 || off > joinedLen || len(at) == 0 || off < at[0] {
		return tomlNoPoint()
	}
	k := len(at) - 1
	for k > 0 && at[k] > off {
		k--
	}
	return tomlPoint{line: e[0] + k, col: off - at[k]}
}

// codexEscapedKeyInSpans — 우리 두 구간 안에 정규화 불가 키 표기가 있는가(D87).
// **엔트리 단위로 본다.** codexEntries가 여러 줄 값을 한 엔트리로 묶고 그 엔트리에서 키는
// 맨 앞 토큰뿐이므로, 문자열 값 안이나 후행 주석 안의 `"이름" = 값` 모양이 키로 오인되지
// 않는다 — codexReadTable이 키를 읽는 경로와 **같은 기준**이며, 두 경로가 다른 기준을 쓰면
// 같은 파일을 두 방식으로 읽는 셈이다(라인을 문맥 없이 훑으면 여러 줄 문자열의 내용 줄·값 뒤
// 후행 주석·홑따옴표 값 내부가 모두 오탐이 된다).
// 인라인 테이블의 키 토큰은 **env 엔트리에서만** 본다 — 판독·되쓰기가 그 엔트리에만 적용되는
// 것과 같은 한정이고, 그것이 없으면 다른 키의 값 안에 든 쉼표 뒤 텍스트가 키로 잡힌다.
// **알려진 한계**: env 인라인 값 안에 따옴표·역슬래시·등호를 함께 담은 문자열은 여전히 키로
// 보인다. 이 검사는 라인을 훑을 뿐 인라인 테이블의 구조를 따라가지 않으며, D80의 파서 비의존
// 원칙 아래에서는 그 한계가 계약이다. 판독·되쓰기 쪽은 tomlScanInline이 구조를 따라가므로 값
// 안을 키로 잡지 않는다 — 그쪽에서 잘못 잡으면 사용자 파일이 깨진다.
func codexEscapedKeyInSpans(lines [][]byte, sp codexSpans) bool {
	for _, span := range []codexSpan{sp.table, sp.env} {
		if !span.found {
			continue
		}
		for _, e := range codexEntries(lines, span) {
			// 주석은 codexEntryRaw가 이미 잘라 냈다 — 주석 안의 키 모양까지 잡으면
			// `env = { A = "1" } # TODO: , "C:\t" = 2` 같은 정상 파일이 이상이 된다.
			raw, _ := codexEntryRaw(lines, e)
			if tomlKeyTokenHasEscape(raw) {
				return true
			}
			if codexKeyName(raw) != "env" {
				continue
			}
			for j := 0; j < len(raw); j++ {
				if (raw[j] == '{' || raw[j] == ',') && tomlKeyTokenHasEscape(raw[j+1:]) {
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
		// 선두 BOM은 판정에서 뗀다 — 구 블록의 BEGIN 마커가 파일 첫 줄이면 그 세 바이트가
		// 거기 붙고, 마커가 인식되지 않으면 분류가 classAppend로 떨어져 install이 관리
		// 테이블을 새로 append한다(같은 논리 테이블 두 번 정의 — 게이트가 막는 막다른 갈래).
		switch trimEOL(trimBOM(ln)) {
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

// blockOwns — 블록 내부에 우리 관리 테이블 헤더가 실존해야 소유(본문 검증=소유).
// 이름 추출은 tomlHeaderName이 소유한다 — 정규화 문자열을 `==`로 비교하면 헤더에 후행 주석이
// 붙은 파일에서 소유가 서지 않아 그 파일의 마이그레이션이 영영 닫힌다.
func blockOwns(lines [][]byte, begin, end int) bool {
	for i := begin + 1; i < end; i++ {
		if tomlHeaderName(stripLine(lines[i])) == codexManagedTable {
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
	// Anomaly — 이 함수가 판정한 이탈 사유(D89 사유 전달 채널). 값이 있으면 호출자가
	// probeCodexMCPBlock의 사유보다 **우선해서** 읽는다. install만 아는 이탈(게이트·점 표기)이
	// 있고 사유를 인쇄하는 두 자리는 probe에서 사유를 받으므로, 이 필드가 없으면 그 이탈이
	// 빈 사유로 나간다. anomalyNone이면 호출자가 종전대로 probe의 사유를 쓴다.
	Anomaly codexAnomaly
	// InputParses — 입력 바이트가 우리 파서로 파스되는가(D89 부수 결정 ②). **라벨 전용이며
	// 어떤 기입도 가르지 않는다** — 기입 정책은 이 릴리스에서 무변경이고, 게이트가 계약상
	// 작동하지 않는 입력임을 사용자에게 알리기만 한다. 상태 값이 아니라 필드인 근거가 그것이다.
	InputParses bool
	// D91 진단 전용 필드 넷. 별도 판독기를 만들면 같은 바이트를 읽는 네 번째 판정원이 되므로
	// 이미 판독한 값을 그대로 실어 낸다. **모든 반환 갈래에서 정의된 값을 갖는다** — 기입 없이
	// 빠지는 반환에서도 채워야 [20]의 판정 대상 한정이 성립한다.
	Tools        []string // 등록물의 enabled_tools 값
	ToolsPresent bool     // 그 키의 물리 라인이 실재하는가(부재와 []를 가른다)
	WantTools    []string // 등록물의 args가 요구하는 도구 집합
	ArgsReadable bool     // args를 프로필로 되읽었는가
}

// gateCodexOutput — D89 산출물 유효성 게이트. **비대칭**이다: 우리 파서 기준으로 입력이
// 파스되고 산출물이 파스되지 않을 때만 무변경으로 되돌린다. 입력이 이미 무효면 산출물의
// 무효는 우리가 들인 것이 아니므로 되돌려도 사용자에게 이득이 없다.
// 구문 유효성만 본다 — 파스되면서 사용자 값이 바뀌는 갈래는 이 게이트의 대상이 아니다(§1.2).
// 바이트가 같으면 건너뛴다: 그때는 산출물이 곧 입력이라 파스 결과가 이미 res.InputParses다.
// 입력 파스는 **결과에 실려 온 값을 읽는다** — 여기서 다시 부르면 D89의 "입력은 호출마다
// 정확히 한 번" 계약이 깨지고 같은 바이트를 두 번 파스한다.
// 되돌린 결과에도 InputParses를 그대로 싣는다 — 라벨 전용 필드라 이 갈래에서 잃으면 [16]이
// 파스되지 않는 입력을 파스된다고 말한다. D91 진단 필드 넷도 같은 이유로 그대로 옮긴다 —
// 판독은 되돌리기 전에 이미 끝났고, 여기서 영값으로 떨어뜨리면 그 상태의 진단이 사라진다.
func gateCodexOutput(existing []byte, res codexInstallResult) codexInstallResult {
	if bytes.Equal(res.Out, existing) || codexTOMLParses(res.Out) || !res.InputParses {
		return res
	}
	return codexInstallResult{
		Out: existing, State: mcpOutputInvalid,
		TableFound: res.TableFound, Anomaly: anomalyOutputInvalid,
		InputParses:  res.InputParses,
		Tools:        res.Tools,
		ToolsPresent: res.ToolsPresent,
		WantTools:    res.WantTools,
		ArgsReadable: res.ArgsReadable,
	}
}

// installCodexConfigBlock — 관리 테이블 병합(스펙 v0.15 §0 D80·D81·D84). 순수 변환: 파일 IO 없음.
// 판정 순서는 probeCodexMCPBlock과 1:1로 유지한다 — 중복 정의 > 구간 밖 충돌 > 소유.
// **읽기 전용 경로가 이 함수를 부른다**(D85 — doctor [20]의 감지원). 그러므로 파일 IO·시간·난수를
// 들이면 진단이 파일을 쓰거나 결정적이지 않게 된다. 프로필 헬퍼를 포함한 호출 트리 전체가 순수해야
// 하며, 그 조건은 스펙 v0.16 §1.3 게이트 1과 §3 표4에 있다.
func installCodexConfigBlock(existing []byte, req codexInstallRequest) codexInstallResult {
	// 입력 파스는 여기서 **한 번만** 한다(D89) — 모든 반환이 이 값을 실어 나르고 게이트도 그것을
	// 읽는다. 이탈 갈래에서 빼면 그 상태의 [16]이 입력 파스 실패를 알지 못한다.
	inputParses := codexTOMLParses(existing)
	lines := splitLinesKeepEnds(existing)
	sp := codexManagedSpans(lines)
	if sp.anomaly != anomalyNone {
		return codexInstallResult{Out: existing, State: mcpMarkerAnomaly, TableFound: sp.table.found, Anomaly: sp.anomaly, InputParses: inputParses}
	}
	if scanOutsideSpans(lines, sp) {
		return codexInstallResult{Out: existing, State: mcpConflict, TableFound: sp.table.found, InputParses: inputParses}
	}
	view := codexReadTable(lines, sp.table)
	// D91 — 되읽기에 실패하면 프로필 무관 기본 집합만으로 판정한다. 통째로 유보하면 도구 0개
	// allowlist 형태가 되읽기 실패 파일에서 그대로 열린다.
	diagProfiles, argsReadable := profilesFromArgs(view.args)
	if !argsReadable {
		diagProfiles = nil
	}
	diag := codexInstallResult{
		Tools: view.tools, ToolsPresent: len(view.toolsLines) > 0,
		WantTools: enabledToolsForProfiles(diagProfiles), ArgsReadable: argsReadable,
	}
	marker, markerFound := codexMarkerValue(lines, sp, view)
	class, begin, end := classifyMarkers(lines)
	inOldBlock := class == classReplace && sp.table.found && sp.table.start > begin && sp.table.start < end
	if sp.table.found && !codexOwnership(marker, markerFound, view.command, inOldBlock) {
		// 판정 근거는 "블록 밖에 있음"이 아니라 "표식이 없고 명령도 우리 것이 아님"이다(D80).
		return codexInstallResult{
			Out: existing, State: mcpExistingHeader, TableFound: sp.table.found, InputParses: inputParses,
			Tools: diag.Tools, ToolsPresent: diag.ToolsPresent, WantTools: diag.WantTools, ArgsReadable: diag.ArgsReadable,
		}
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
			// 마커 제외는 없다 — 이 갈래는 관리 테이블이 없어 inOldBlock이 서지 않는다.
			base = spliceCodexLines(lines,
				[]codexSplice{{sp.env.start, sp.env.end, codexEnvBody(lines, sp.env, req.Marker, eol, -1, -1)}}, nil)
		} else {
			body = append(body, codexEnvBody(lines, codexSpan{}, req.Marker, eol, -1, -1)...)
		}
		return gateCodexOutput(existing, codexInstallResult{
			Out: appendBlock(base, body, crlf), State: mcpWritten, Changed: true, Profiles: resultProfiles, ExecExposed: execExposed, TableFound: sp.table.found, InputParses: inputParses,
			Tools: diag.Tools, ToolsPresent: diag.ToolsPresent, WantTools: diag.WantTools, ArgsReadable: diag.ArgsReadable,
		})
	}
	// D90 — 점 표기 env가 있고 표식을 새로 넣거나 갱신해야 하면 쓸 자리가 없다. 헤더를 붙이면
	// 같은 논리 테이블이 두 번 정의되고, 점 표기로 쓰면 이전 릴리스 바이너리가 그 파일을 깬다 —
	// **기입 형태는 개정 뒤에도 점 표기가 아니다**(D90 개정은 판독만 통일한다).
	// 표식이 이미 현재 값이면 이탈하지 않는다 — 고칠 것이 없는 파일에 사유를 내는 오경보다.
	// 유효 입력에서는 marker가 곧 dottedMarker이므로 뒤의 두 항이 같은 것을 묻는다. 그래도 둘을
	// 남긴다 — 두 형태를 함께 담은 **무효** 입력에서 marker는 헤더 구간 값이라 둘이 갈린다.
	if view.dottedEnv && (!view.dottedMarkerFound || view.dottedMarker != req.Marker) && marker != req.Marker {
		return codexInstallResult{
			Out: existing, State: mcpMarkerAnomaly, TableFound: sp.table.found, Anomaly: anomalyDottedEnv, InputParses: inputParses,
			Tools: diag.Tools, ToolsPresent: diag.ToolsPresent, WantTools: diag.WantTools, ArgsReadable: diag.ArgsReadable,
		}
	}
	// 인라인 env의 깊이 1 마디 중 **첫 마디가 표식 키인 점 표기**가 있으면 삽입은 중복 정의를
	// 만들고, 표식으로 읽히지도 않아 갱신 갈래도 설 수 없다. 게이트가 그 산출을 거부하지만
	// 그 문면은 "우리 결함이니 알려 주세요"라 사용자가 할 일이 없다 — 수행 가능한 안내로 바꾼다.
	if view.inlineEnv >= 0 && codexInlineMarkerBlocked(lines, view.inlineEnvEntry) {
		return codexInstallResult{
			Out: existing, State: mcpMarkerAnomaly, TableFound: sp.table.found,
			Anomaly: anomalyDottedEnv, InputParses: inputParses,
			Tools: diag.Tools, ToolsPresent: diag.ToolsPresent,
			WantTools: diag.WantTools, ArgsReadable: diag.ArgsReadable,
		}
	}
	// 무변경 판정(D84): 우리 소유 키 넷의 값이 모두 같고, 새로 만들 테이블도 지울 마커 줄도
	// 없으면 쓰기와 백업을 생략한다. **키 단위 동치는 바이트 동일을 포함**하므로 호스트가 우리
	// 테이블을 다른 형태(키 순서·인용·공백·env 표기)로 되썼을 때에도 무변경 재실행마다 .bak이
	// 생기지 않는다 — 스펙 §1.3-1 ② 게이트의 두 갈래를 한 경로가 함께 만족한다.
	// 점 표기 env가 이미 그 서브테이블을 정의하므로 헤더를 붙이면 중복 정의다(D90) — 인라인
	// 대입과 같은 이유로 같은 자리에서 뺀다.
	envMissing := !sp.env.found && view.inlineEnv < 0 && !view.dottedEnv
	// keepArgs 갈래에서는 args·enabled_tools를 비교하지 않는다 — 보존하는 값을 비교하면 사용자가
	// 넓힌 목록이 매 실행 "다르다"로 읽혀 .bak이 매번 새로 생기고 D84 단일 슬롯 계약이
	// 무의미해진다.
	ownedSame := view.command == hookBinaryName && marker == req.Marker &&
		(keepArgs || (slices.Equal(view.args, mcpArgsForProfiles(profiles)) &&
			slices.Equal(view.tools, enabledToolsForProfiles(profiles))))
	if !envMissing && !inOldBlock && ownedSame {
		return codexInstallResult{
			Out: existing, State: mcpWritten, Profiles: resultProfiles, ArgsKept: argsKept, ExecExposed: execExposed, TableFound: sp.table.found, InputParses: inputParses,
			Tools: diag.Tools, ToolsPresent: diag.ToolsPresent, WantTools: diag.WantTools, ArgsReadable: diag.ArgsReadable,
		}
	}
	// D84 마이그레이션 — 마커 두 줄이 **우리 구간 안**에 들어와 있으면 아래 drop 맵으로는
	// 지워지지 않는다: spliceCodexLines는 편집 구간 안에서 drop을 보지 않고 body를 통째로 얹기
	// 때문이다. 그래서 **두 구간의 조립기가 각자** 뺀다 — 블록이 우리 테이블만 감싼 형태(§3 표4)는
	// 관리 테이블 쪽 keep이, 블록이 두 테이블을 모두 감싸 한쪽 마커가 env 구간에 들어온 형태는
	// codexEnvBody의 dropBegin·dropEnd가 받는다. 한쪽만 두면 짝 없는 마커가 남고, 그러면 다음
	// 실행의 마커 분류가 이상으로 떨어져 그 파일의 마이그레이션 경로가 닫힌다. 구간 밖 마커는
	// 여전히 drop이 지운다 — 셋이 서로를 대신하지 않는다.
	dropBegin, dropEnd := -1, -1
	if inOldBlock {
		dropBegin, dropEnd = begin, end
		view.keep = slices.DeleteFunc(view.keep, func(i int) bool { return i == begin || i == end })
	}
	body := codexTableBody(lines, sp.table, view, profiles, keepArgs, req.Marker, eol)
	if envMissing { // env 서브테이블을 우리 구간 끝에 잇는다(다음 테이블 헤더 앞이라 안전하다)
		body = append(ensureEOL(body, eol), codexEnvBody(lines, codexSpan{}, req.Marker, eol, -1, -1)...)
	}
	edits := []codexSplice{{sp.table.start, sp.table.end, body}}
	if sp.env.found {
		edits = append(edits, codexSplice{sp.env.start, sp.env.end, codexEnvBody(lines, sp.env, req.Marker, eol, dropBegin, dropEnd)})
	}
	drop := map[int]bool{}
	if inOldBlock { // D84 마이그레이션 — 추가로 지우는 것은 **마커 두 줄뿐**이다
		drop[begin], drop[end] = true, true
	}
	// Changed는 **기입 바이트와 기존 바이트의 비교**로 정한다 — D84의 1차 기준이 그것이고,
	// 위 ownedSame(키 단위 동치)은 재직렬화 때문에 바이트 비교가 성립하지 않을 때의 하강
	// 경로다. 여기를 참으로 고정하면 ownedSame이 접지 못하는 상태가 그대로 새어 나간다:
	// 인라인 env 대입이 여는 중괄호 없이 다음 줄로 이어지면 setInlineEnvMarker가 그 줄을
	// 보존하므로 marker가 영영 req.Marker와 같아지지 않고, 그런 파일은 무변경 재실행마다
	// .bak이 다시 생겨 단일 슬롯 계약이 무의미해진다.
	out := spliceCodexLines(lines, edits, drop)
	return gateCodexOutput(existing, codexInstallResult{
		Out: out, State: mcpWritten,
		Changed: !bytes.Equal(out, existing), Profiles: resultProfiles, ArgsKept: argsKept, ExecExposed: execExposed,
		TableFound: sp.table.found, InputParses: inputParses,
		Tools: diag.Tools, ToolsPresent: diag.ToolsPresent, WantTools: diag.WantTools, ArgsReadable: diag.ArgsReadable,
	})
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
// 구간 판정 이상은 **사유째 돌려준다**(D85) — 무변경으로 이탈했다는 것만으로는 호출자가
// "관리 테이블이 남았다"와 그 조치를 사용자에게 알릴 수 없고, 그러면 제거가 성공한 문면만
// 보이는데 Codex는 그 MCP 서버를 계속 띄운다. 판정을 내린 자리에서 실어 보내므로 호출자가
// 같은 판정을 다시 도출하지 않는다.
func uninstallCodexConfigBlock(existing []byte) (out []byte, changed bool, anomaly codexAnomaly) {
	lines := splitLinesKeepEnds(existing)
	sp := codexManagedSpans(lines)
	if sp.anomaly != anomalyNone || !sp.table.found {
		return existing, false, sp.anomaly
	}
	view := codexReadTable(lines, sp.table)
	marker, markerFound := codexMarkerValue(lines, sp, view)
	class, begin, end := classifyMarkers(lines)
	inOldBlock := class == classReplace && sp.table.start > begin && sp.table.start < end
	if !codexOwnership(marker, markerFound, view.command, inOldBlock) {
		// 테이블 부재 갈래(위)와 **같은 반환값이면 호출자가 가르지 못한다** — 캐치올 분기를
		// 두면 미설치 사용자의 흔한 실행에 잔존 문면이 나간다. 사유값으로 가른다.
		return existing, false, anomalyNotOwned
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
	return out, true, anomalyNone
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
	// 헤더를 새로 붙이지 않는다(중복 정의 금지) — 표식은 그 대입 안에서 갈아 끼운다.
	inlineEnv int
	// inlineEnvEntry — 그 대입의 **논리 엔트리 구간**([첫 줄, 마지막 줄]). 없으면 (-1,-1)이다.
	// 여러 줄로 이어진 인라인 테이블은 라인 하나로 열거할 수 없으므로 판독·되쓰기가 이 구간을
	// 받는다. inlineEnv(첫 줄)는 보존 라인 판정에 계속 쓰이므로 남는다 — 둘의 역할이 다르다.
	inlineEnvEntry [2]int
	// dottedEnv — 구간 안에 점 표기 env.* 키가 있는가, 그중 표식 줄의 값은 무엇인가(D90 개정).
	// dottedEnv는 install의 이탈 판정만 쓰지만, dottedMarker·dottedMarkerFound는 **표식 판독기
	// codexMarkerValue가 셋째 형태로 읽는다** — 그래서 소유 판정·라벨·uninstall이 그 형태의
	// 우리 등록물을 우리 것으로 본다. 판독기와 판정이 갈리면 정상 등록물에 잘못된 조치가 나간다.
	dottedEnv         bool
	dottedMarker      string
	dottedMarkerFound bool
	// keep — 우리 소유가 아닌 라인. 원문 그대로 되돌린다.
	// argsLines·toolsLines — 되읽기 실패 시 keep에 합류해 args·enabled_tools도 원문으로 돌아간다.
	keep, argsLines, toolsLines []int
}

// codexReadTable — 관리 테이블 구간을 훑어 소유 키 값과 보존 라인을 가른다.
func codexReadTable(lines [][]byte, sp codexSpan) codexTableView {
	view := codexTableView{inlineEnv: -1, inlineEnvEntry: [2]int{-1, -1}}
	if !sp.found {
		return view
	}
	for _, e := range codexEntries(lines, sp) {
		raw, _ := codexEntryRaw(lines, e)
		joined := codexEntryText(lines, e)
		values := []string(nil)
		if eq := tomlTopLevelEq(raw); eq >= 0 {
			values = tomlStringList(joined[strings.Index(joined, "=")+1:])
		}
		switch codexKeyName(raw) {
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
			view.inlineEnv, view.inlineEnvEntry = e[0], e
		}
		// D90 — 점 표기 `env.<이름>`도 TOML에서는 mcp_servers.ctr.env를 **정의**한다. 위 switch의
		// 어느 case에도 걸리지 않으므로 이 줄은 그대로 아래 keep으로 떨어져야 한다 — 형제 case를
		// 따라 continue로 빼면 사용자의 env 줄이 재기입 때 조용히 사라진다. 술어에 넘기는 것은
		// **엔트리 첫 줄의 원문**이다(점 표기 키는 한 줄이다): joined는 따옴표 안 공백까지 지운
		// 정규화라 그것에 걸면 `"e n v"`가 env로 읽혀 타인 테이블에 우리 표식이 선다.
		if head, rest := tomlDottedEnvKey(trimEOL(lines[e[0]])); head == "env" {
			view.dottedEnv = true
			if rest == codexMarkerKey {
				// 값은 위에서 이미 읽은 values를 그대로 쓴다 — 같은 엔트리를 두 기준으로 읽으면
				// 그 둘이 갈린다. 문자열이 아닌 값은 ""로 남아 "현재 값이 아니다"로 읽히고,
				// 개정 뒤에는 그것이 곧 "소유가 아니다"다(codexMarkerValue가 이 값을 낸다).
				view.dottedMarkerFound = true
				if len(values) > 0 {
					view.dottedMarker = values[0]
				}
			}
		}
		for i := e[0]; i <= e[1]; i++ {
			view.keep = append(view.keep, i)
		}
	}
	return view
}

// codexInlineMarker — 인라인 env의 우리 표식 값(D92 소비자 1). 깊이 1 마디 중 segs==1이고
// 이름이 표식 키인 것만 우리 표식이다 — 중첩 인라인 안쪽 마디는 값으로 통째 잡히므로 여기
// 오르지 않는다. 그것이 관측 ①(사용자 값 조용한 덮어쓰기)이 닫히는 지점이다. 키 표기의
// 따옴표를 벗기는 것은 TOML이 `CTR_MANAGED`와 `"CTR_MANAGED"`·홑따옴표 표기를 **같은 키**로
// 읽기 때문이다 — 벗기지 않으면 따옴표 표기를 부재로 읽고 표식을 한 번 더 넣어 중복 키가 된다.
//
// **값 해석의 폭은 종전 라인 한정 판독기와 같다**: 값 구간이 완결된 큰따옴표 문자열일 때만
// 값을 읽고, 그 밖(정수·불리언·홑따옴표·미종료)은 ("", true)다. strings.Trim(값, 따옴표)로
// 넓히면 종전이 소유로 읽지 않던 값이 소유 표식이 되어 재기준선 밖의 판정이 바뀐다.
func codexInlineMarker(lines [][]byte, e [2]int) (value string, found bool) {
	sc := tomlScanInline(lines, e)
	if !sc.ok {
		return "", false
	}
	joined, at := codexEntryRaw(lines, e)
	for _, en := range sc.entries {
		if en.segs != 1 || strings.Trim(tomlSpanText(joined, at, e, en.key), `"'`) != codexMarkerKey {
			continue
		}
		v := tomlSpanText(joined, at, e, en.value)
		if n := basicStringLen(v); len(v) >= 2 && v[0] == '"' && n == len(v) && v[n-1] == '"' {
			return v[1 : n-1], true
		}
		return "", true // 문자열이 아닌 값 — 키는 있다(종전과 같은 계약)
	}
	return "", false
}

// codexInlineMarkerBlocked — 깊이 1 마디 중 첫 마디가 표식 키인 점 표기가 있는가.
// tomlSplitDotted가 이미 따옴표 밖 '.'에서 가른다 — 새 함수는 없다.
func codexInlineMarkerBlocked(lines [][]byte, e [2]int) bool {
	sc := tomlScanInline(lines, e)
	if !sc.ok {
		return false
	}
	joined, at := codexEntryRaw(lines, e)
	for _, en := range sc.entries {
		if en.segs <= 1 {
			continue
		}
		s := tomlSpanText(joined, at, e, en.key)
		if segs := tomlSplitDotted(s); len(segs) > 0 && strings.Trim(segs[0], `"'`) == codexMarkerKey {
			return true
		}
	}
	return false
}

// codexMarkerValue — 소유 표식 값을 읽는다. env 서브테이블을 정의하는 **세 형태를 모두**
// 인식한다: [mcp_servers.ctr.env] 헤더 구간·관리 테이블 안의 인라인 env 대입(D80)·점 표기
// env.CTR_MANAGED(D90 개정). found는 키가 있는가이고, 소유 판정은 값 기준(isOurMarkerValue)이라
// 키만 있고 값이 비면 소유가 아니다.
//
// 점 표기를 여기서 읽는 것이 D90 개정의 본절이다. 판독기(codexReadTable)만 그 형태를 알고
// 판정이 모르면 **정상 등록물**에 잘못된 조치가 나간다 — 호스트는 단일 키 서브테이블을 그
// 형태로 접으므로 정상 설치 뒤 정상 사용으로 도달하는 상태다. 소유 판정·라벨·uninstall이
// 이 한 함수를 지나므로 여기서 읽으면 셋이 함께 옳아진다(D89의 "판정원 하나"와 같은 주제).
//
// **셋의 순서는 유효 입력에서 관측되지 않는다.** 셋 다 같은 논리 테이블 mcp_servers.ctr.env를
// **정의**하고 TOML은 한 테이블의 중복 정의를 금지하므로, Codex가 읽을 수 있는 파일에는
// 많아야 하나가 있다(codexTOMLParses가 그 사실을 잰다 — 두 형태를 함께 담은 입력은 파스되지
// 않는다). 그러므로 새 경로를 **맨 뒤에** 둔다: 기존 두 경로가 이미 다루던 모든 입력에서
// 바이트 단위로 같은 답을 유지하는 배치이며, 순서로 무언가를 결정하지 않는다.
func codexMarkerValue(lines [][]byte, sp codexSpans, view codexTableView) (string, bool) {
	if sp.env.found {
		for _, e := range codexEntries(lines, sp.env) {
			raw, _ := codexEntryRaw(lines, e)
			if codexKeyName(raw) != codexMarkerKey {
				continue
			}
			joined := codexEntryText(lines, e)
			if v := tomlStringList(joined[strings.Index(joined, "=")+1:]); len(v) > 0 {
				return v[0], true
			}
			return "", true
		}
	}
	if view.inlineEnv >= 0 {
		return codexInlineMarker(lines, view.inlineEnvEntry)
	}
	if view.dottedMarkerFound {
		return view.dottedMarker, true
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

// setInlineEnvMarker — 인라인 env 대입에 소유 표식을 심는다(D92 소비자 2). 입력은 **논리
// 엔트리**다 — 라인 하나로는 여러 줄 인라인 테이블을 열거할 수 없다.
// 표식 마디가 이미 있으면 그 마디의 **값 구간만** 바꾼다. 값이 문자열이 아니어도(정수·불리언·
// 홑따옴표 등) 그 토큰을 표식 문자열로 갈아 끼운다 — 삽입이 아니라 치환이라 중복 키가 생기지
// 않고, 그대로 두면 표식이 영영 현재 값이 되지 못해 doctor가 "경고 없는 표식없음"과 "무변경"
// 두 줄을 함께 내는 상태로 굳는다. **빈 문자열 값도 같다**: 값 구간이 있으므로 제자리에서
// 현재 값으로 갱신한다.
// 값으로 첫 일치를 치환하지 않는 이유: 표식과 **값이 같은** 사용자 키가 앞서 있으면 그 값이
// 바뀌고 CTR_MANAGED는 옛 값으로 남는다 — 사용자 환경변수를 조용히 고치는 경로다.
// 표식이 없으면 여는 중괄호 뒤에 더한다 — 내부가 비면 쉼표를 붙이지 않는다(TOML 1.0.0은
// 인라인 테이블의 후행 쉼표를 금지한다). 삽입은 `open`·`close`가 **함께** 확정될 때만 서고
// 빈 여부는 `len(sc.entries) == 0`이 정한다: 절반만 열거된 결과에 삽입하면 같은 인라인
// 테이블에 키가 두 번 생긴다.
// 구조가 확정되지 않으면 **엔트리 바이트를 그대로 옮긴다** — nil을 돌려주면 호출자의
// append가 그 줄들을 산출에서 없애 사용자 값이 조용히 사라진다.
func setInlineEnvMarker(lines [][]byte, e [2]int, marker string) []byte {
	raw := func() []byte { // 원문 보존 — 실패 갈래는 엔트리 바이트를 그대로 옮긴다
		var b []byte
		for i := e[0]; i <= e[1]; i++ {
			b = append(b, lines[i]...)
		}
		return b
	}
	sc := tomlScanInline(lines, e)
	if !sc.ok || !sc.open.valid() || !sc.close.valid() {
		return raw()
	}
	// 갱신 갈래 — segs==1이고 이름이 표식 키인 마디의 값 구간만 치환한다.
	joined, at := codexEntryRaw(lines, e)
	for _, en := range sc.entries {
		if en.segs != 1 || strings.Trim(tomlSpanText(joined, at, e, en.key), `"'`) != codexMarkerKey {
			continue
		}
		return spliceInlineSpan(lines, e, en.value, `"`+marker+`"`)
	}
	// 삽입 갈래 — 여는 중괄호 **바로 뒤**에 ` KEY = "marker"<sep>`를 끼운다.
	sep := ","
	if len(sc.entries) == 0 {
		sep = ""
	}
	ins := tomlSpan{start: tomlPoint{line: sc.open.line, col: sc.open.col + 1}, end: tomlPoint{line: sc.open.line, col: sc.open.col + 1}}
	return spliceInlineSpan(lines, e, ins, " "+codexMarkerKey+` = "`+marker+`"`+sep)
}

// spliceInlineSpan — 논리 엔트리의 [sp.start, sp.end) 파일 좌표 구간을 repl로 갈아 끼우고
// 엔트리 전체를 라인 종결자까지 원문 그대로 배출한다. 구간이 빈 지점이면 삽입이다.
// **줄 종결자를 새로 만들지 않는다** — 원문 라인을 그대로 옮기므로 CRLF도 마지막 줄 종결자
// 없음도 자동으로 옳다(v0.17 §1.4-라가 종결자 재생성에서 물린 자리다).
//
// **경계 검사는 모든 차원을 한 곳에서 본다** — 줄뿐 아니라 열, 그리고 시작≤끝 순서까지.
// 줄만 보는 부분 검사로 두면 열이 줄 길이를 넘은 좌표가 그대로 슬라이스에 들어가고,
// internal/cli에는 recover가 없으므로 그것은 **사용자 config를 쓰는 도중의 프로세스 종료**다
// (실측: 열 999 → `slice bounds out of range`). 뒤집힌 같은 줄 구간은 패닉 대신 바이트를
// 복제해 조용히 파일을 깨뜨린다. 형제 tomlSpanText가 같은 이유로 같은 검사를 하고
// codexPointAt이 상한을 받는 이유도 같다.
// **생산자 쪽 불변식에 기대지 않는다**: 지금의 두 생산자는 그런 좌표를 내지 않지만 이 원시는
// 소비자가 더 붙는 자리다. 어느 차원으로든 범위 밖이면 갈아 끼우지 않고 엔트리 원문을 옮긴다.
// 조건의 **순서가 계약이다** — '||'의 좌→우 단락 평가 덕에 줄 범위가 선 뒤에야
// lines[...] 첨자가 평가된다. 줄 검사를 뒤로 옮기면 그 첨자가 먼저 터진다.
func spliceInlineSpan(lines [][]byte, e [2]int, sp tomlSpan, repl string) []byte {
	if !sp.start.valid() || !sp.end.valid() ||
		sp.start.line < e[0] || sp.end.line > e[1] || sp.start.line > sp.end.line ||
		sp.start.col > len(lines[sp.start.line]) || sp.end.col > len(lines[sp.end.line]) ||
		(sp.start.line == sp.end.line && sp.start.col > sp.end.col) {
		var b []byte
		for i := e[0]; i <= e[1]; i++ {
			b = append(b, lines[i]...)
		}
		return b
	}
	var b []byte
	for i := e[0]; i <= e[1]; i++ {
		switch {
		case i < sp.start.line || i > sp.end.line:
			b = append(b, lines[i]...)
		case i == sp.start.line:
			b = append(b, lines[i][:sp.start.col]...)
			b = append(b, repl...)
			if i == sp.end.line {
				b = append(b, lines[i][sp.end.col:]...)
			}
		case i == sp.end.line:
			b = append(b, lines[i][sp.end.col:]...)
		}
		// 구간 안쪽 줄(start.line < i < end.line)은 통째로 사라진다 — 그것이 치환의 정의다.
	}
	return b
}

// codexTableBody — 관리 테이블 구간의 새 내용(소유 키 + 보존 라인). 기존 테이블이면 헤더
// 라인을 원문 그대로 옮긴다 — 호스트가 헤더 공백을 바꿔 놓아도 그것만으로 재기입이 나지
// 않게 하는 지점이다. keepArgs면 args·enabled_tools를 보존 라인으로 되돌린다 — 호출자가 그
// 인자에 두 사유를 합류시킨다(D81 되읽기 실패·D86 표식 전용).
//
// D88 — 이 재생성이 관리 키 줄의 후행 주석을 지운다. 보존 대상이 아니라는 것이 계약이며,
// 보존 라인(keep)의 주석은 원문 그대로 옮겨진다.
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
	// 인라인 env는 엔트리 **첫 줄**에서 엔트리 전체를 내보내고 나머지 줄을 건너뛴다 —
	// 건너뛰지 않으면 여러 줄 엔트리의 뒷줄이 두 번 나간다(view.keep은 엔트리의 모든 줄을 담는다).
	skipTo := -1
	for _, i := range keep {
		if i <= skipTo {
			continue
		}
		if i == view.inlineEnv {
			b = append(b, setInlineEnvMarker(lines, view.inlineEnvEntry, marker)...)
			skipTo = view.inlineEnvEntry[1]
			continue
		}
		b = append(b, lines[i]...)
	}
	return b
}

// codexEnvBody — [mcp_servers.ctr.env] 구간의 새 내용. CTR_MANAGED만 우리 것이고 나머지
// 환경변수는 보존한다(D80).
//
// dropBegin·dropEnd — 마이그레이션 갈래에서 **이 구간 안에 들어온 구 마커 두 줄**의 인덱스.
// 구간 편집은 편집 범위 안에서 drop 맵을 보지 않으므로 여기서 빼야 한다 — 관리 테이블 쪽이
// view.keep에서 같은 일을 하는 것과 같은 기제다. 무효값은 (-1, -1)이다: 엔트리 첫 줄은
// sp.start+1 이상이라 마커 분류가 마커 없을 때 돌려주는 (0, 0)도 실제로는 어느 엔트리와도
// 겹치지 않지만, 그 값을 무효로 쓰면 "마커가 없다"와 "0행이 마커다"가 같은 값이 된다 —
// 마이그레이션이 아닌 호출이 유효 인덱스를 넘기지 않는다는 것을 값 자체로 못박는다.
func codexEnvBody(lines [][]byte, sp codexSpan, marker, eol string, dropBegin, dropEnd int) []byte {
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
		if e[0] == dropBegin || e[0] == dropEnd {
			continue
		}
		if r, _ := codexEntryRaw(lines, e); codexKeyName(r) == codexMarkerKey {
			continue
		}
		for i := e[0]; i <= e[1]; i++ {
			b = append(b, lines[i]...)
		}
	}
	return b
}

// tomlPoint — 파일 좌표. line은 lines 슬라이스의 인덱스, col은 그 줄 안의 바이트 오프셋이다.
// **무효값은 (-1, -1)이다** — 0을 무효로 쓰면 "없다"와 "맨 앞이다"가 같은 값이 되고, 그
// 혼동이 v0.17 §1.4-라에서 구간 첫 줄을 지우는 결함을 만들었다.
type tomlPoint struct{ line, col int }

// tomlNoPoint — 무효 지점.
func tomlNoPoint() tomlPoint { return tomlPoint{line: -1, col: -1} }

// valid — 실재하는 지점인가.
func (p tomlPoint) valid() bool { return p.line >= 0 && p.col >= 0 }

// tomlSpan — [start, end) 반오픈 구간. 두 지점 모두 파일 좌표다.
type tomlSpan struct{ start, end tomlPoint }

// tomlInlineEntry — 인라인 테이블의 **깊이 1 마디** 하나.
// key는 점 표기면 **경로 전체**의 구간이다(마디 하나가 아니다).
// segs — 따옴표 밖 '.'로 가른 마디 수. 1이면 단순 키다. **소비한다**(스펙 §0 D92 계약 3).
// escaped — 어느 마디든 역슬래시 이스케이프를 담는가. **표시만 하고 소비하지 않는다** —
// 소비하면 무변경 집합이 넓어지고 그것이 D93이다.
type tomlInlineEntry struct {
	key     tomlSpan
	value   tomlSpan
	segs    int
	escaped bool
}

// tomlInlineScan — 한 논리 엔트리의 인라인 테이블 열거 결과.
// ok=false면 구조가 확정되지 않은 것이고 **entries는 비어 있다**(부분 산출 금지 — 계약 4).
type tomlInlineScan struct {
	open, close tomlPoint
	entries     []tomlInlineEntry
	ok          bool
}

// tomlTripleLen — s의 맨 앞이 삼중 큰따옴표나 삼중 홑따옴표면 그 여러 줄 문자열 토큰
// 전체의 길이, 아니면 0. 닫히지 않으면 len(s)다 — 호출자가 그 자리에서 fail로 빠진다.
// 닫기 탐색의 이스케이프 기준은 tomlLineScanner.advance와 **같다**: strings.Index로 찾으면
// 역슬래시로 이스케이프된 따옴표를 닫기로 오인한다.
//
// **문서 주석에 홑따옴표 두 개를 잇달아 적지 마라** — Go 1.19+ gofmt의 doc 주석 정규화가
// 그것을 오른쪽 겹따옴표로 바꿔 `gofumpt -l .`이 적색이 된다(실측). 저장소가 이미 같은
// 이유로 tomlLineScanner 주석에서 "삼중 홑따옴표"라고 풀어 쓴다(codex_toml.go:168).
func tomlTripleLen(s string) int {
	var q string
	switch {
	case strings.HasPrefix(s, `"""`):
		q = `"""`
	case strings.HasPrefix(s, "'''"):
		q = "'''"
	default:
		return 0
	}
	for i := 3; i < len(s); {
		if q == `"""` && s[i] == '\\' {
			i += 2
			continue
		}
		if strings.HasPrefix(s[i:], q) {
			return i + 3
		}
		i++
	}
	return len(s)
}

// tomlSpanText — 구간의 원문 텍스트. joined·at은 codexEntryRaw가 낸 짝이고 e는 그 논리
// 엔트리다. 지점이 무효이거나 범위 밖이면 ""다 — 같은 인덱스 식을 소비자마다 되풀이하면
// 그중 하나만 경계 검사를 빠뜨려도 슬라이스 패닉이 되고 internal/cli에는 recover가 없다.
func tomlSpanText(joined string, at []int, e [2]int, sp tomlSpan) string {
	off := func(p tomlPoint) int {
		k := p.line - e[0]
		if !p.valid() || k < 0 || k >= len(at) {
			return -1
		}
		return at[k] + p.col
	}
	s, x := off(sp.start), off(sp.end)
	if s < 0 || x < s || x > len(joined) {
		return ""
	}
	return joined[s:x]
}

// tomlScanInline — 논리 엔트리의 인라인 테이블을 **한 번** 훑어 깊이 1의 마디를 낸다(D92).
// 입력은 codexEntryRaw가 낸 원문이고 출력 지점은 전부 파일 좌표다.
//
// **깊이 1만 낸다** — 중첩 인라인 테이블은 값으로 통째 잡히므로 안쪽 키가 바깥 마디로
// 올라오지 않는다. 그것이 "중첩 안쪽 표식을 바깥 것으로 읽어 사용자 값을 덮어쓰는" 결함을
// 구조적으로 막는 지점이다.
// **점 표기 키는 중첩이 아니라 깊이 1 마디다**(TOML 명세의 `{ type.name = "pug" }`) — 그래서
// 깊이 1 한정만으로는 부족하고 segs가 그 몫을 진다.
// **부분 산출을 내지 않는다** — 미종료·짝 어긋남·close 부재 어느 경우든 entries를 비운다.
// 절반만 열거된 결과는 삽입 소비자에게 "비어 있다"로 보이고 그것이 곧 중복 정의다.
func tomlScanInline(lines [][]byte, e [2]int) tomlInlineScan {
	fail := tomlInlineScan{open: tomlNoPoint(), close: tomlNoPoint()}
	joined, at := codexEntryRaw(lines, e)
	pt := func(off int) tomlPoint { return codexPointAt(e, at, len(joined), off) }

	i := tomlTopLevelEq(joined)
	if i < 0 {
		return fail
	}
	i++
	for i < len(joined) && (joined[i] == ' ' || joined[i] == '\t') {
		i++
	}
	if i >= len(joined) || joined[i] != '{' {
		return fail // 우변이 인라인 테이블이 아니다 — Task 9가 이 갈래를 소비한다
	}
	out := tomlInlineScan{open: pt(i), close: tomlNoPoint()}
	stack := []byte{'{'}
	i++

	skipSpace := func() {
		for i < len(joined) && (joined[i] == ' ' || joined[i] == '\t') {
			i++
		}
	}
	for {
		skipSpace()
		if i >= len(joined) {
			return fail
		}
		if joined[i] == '}' { // 빈 테이블 또는 후행 쉼표 뒤 정상 종료
			out.close, out.ok = pt(i), true
			return out
		}
		// 키 자리에 구분자가 오면 구조가 확정되지 않은 것이다 — `{ A = "1",, B = "2" }`가
		// 그 형태이고, 막지 않으면 ", B"가 키 구간이 되어 P0가 뒤늦게 문다.
		if joined[i] == ',' || joined[i] == '=' {
			return fail
		}
		// 키 토큰 — 따옴표 밖 '=' 앞까지가 경로 전체다.
		keyStart := i
		for i < len(joined) && joined[i] != '=' {
			switch joined[i] {
			case '"':
				i += basicStringLen(joined[i:])
			case '\'':
				i += literalStringLen(joined[i:])
			default:
				i++
			}
		}
		if i >= len(joined) {
			return fail
		}
		keyEnd := i
		for keyEnd > keyStart && (joined[keyEnd-1] == ' ' || joined[keyEnd-1] == '\t') {
			keyEnd--
		}
		if keyEnd == keyStart {
			return fail
		}
		keyText := joined[keyStart:keyEnd]
		// escaped — **어느 마디든** 이스케이프를 담는가(계약 3). keyText 통째로 검사하면 점
		// 표기에서 첫 마디 뒤 '.'에 걸려 늘 false다(실측: { "C:\t".sub = "x" } → false).
		// v0.18은 이 값을 소비하지 않지만 D93이 읽으므로 판정원이 지금 옳아야 한다.
		segs := tomlSplitDotted(keyText)
		escaped := false
		for _, seg := range segs {
			if tomlKeyTokenHasEscape(seg + "=") {
				escaped = true
				break
			}
		}
		i++ // '='
		skipSpace()
		// 값 토큰 — 깊이 1로 돌아올 때까지 스택으로 센다.
		valStart := i
		for i < len(joined) {
			c := joined[i]
			if len(stack) == 1 && (c == ',' || c == '}') {
				break
			}
			// 여러 줄 문자열은 닫힘까지 통째로 건너뛴다 — 한 줄 문자열로 읽으면 값 안의
			// '#'·','·'}'가 구조 문자로 잡힌다. 한 줄 갈래보다 **먼저** 본다.
			if n := tomlTripleLen(joined[i:]); n > 0 {
				i += n
				continue
			}
			switch c {
			case '"':
				i += basicStringLen(joined[i:])
			case '\'':
				i += literalStringLen(joined[i:])
			case '{', '[':
				stack = append(stack, c)
				i++
			case '}', ']':
				want := byte('{')
				if c == ']' {
					want = '['
				}
				if len(stack) < 2 || stack[len(stack)-1] != want {
					return fail
				}
				stack = stack[:len(stack)-1]
				i++
			default:
				i++
			}
		}
		if i >= len(joined) {
			return fail
		}
		valEnd := i
		for valEnd > valStart && (joined[valEnd-1] == ' ' || joined[valEnd-1] == '\t') {
			valEnd--
		}
		// 값 구간이 비면 구조가 확정되지 않은 것이다 — `{ A = }`가 그 형태다.
		if valEnd == valStart {
			return fail
		}
		out.entries = append(out.entries, tomlInlineEntry{
			key:     tomlSpan{start: pt(keyStart), end: pt(keyEnd)},
			value:   tomlSpan{start: pt(valStart), end: pt(valEnd)},
			segs:    len(segs),
			escaped: escaped,
		})
		if joined[i] == '}' {
			out.close, out.ok = pt(i), true
			return out
		}
		i++ // ','
	}
}
