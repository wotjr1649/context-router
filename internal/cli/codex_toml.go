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
	"strings"
)

const (
	codexBlockBegin = "# BEGIN context-router"
	codexBlockEnd   = "# END context-router"
)

// codexBlockBody — 관리 블록 본문(LF 기준). CRLF 지배 파일엔 개행만 CRLF로 치환해 기입.
const codexBlockBody = codexBlockBegin + "\n" +
	"[mcp_servers.ctr]\n" +
	"command = \"context-router\"\n" +
	"args = []\n" +
	"enabled_tools = [\"ctr_search\", \"ctr_fetch\", \"ctr_transform\", \"ctr_record_event\", \"ctr_session_summary\", \"ctr_export_events\"]\n" +
	"# ingest/net 활성화 시 권장: default_tools_approval_mode = \"prompt\"\n" +
	codexBlockEnd + "\n"

type codexMCPState int

const (
	mcpWritten        codexMCPState = iota // 블록 기입/갱신 성공 — MCP 확정
	mcpExistingHeader                      // 블록 밖 정확 [mcp_servers.ctr] 헤더 실존 — 기입 생략·MCP 확정
	mcpConflict                            // 키-경계 충돌 — 기입 생략·MCP 미확정
	mcpMarkerAnomaly                       // 마커 무결성·소유권 이상 — 무변경·MCP 미확정
)

type markerClass int

const (
	classAppend  markerClass = iota // 마커 0쌍 — append 후보
	classReplace                    // 소유 블록 1쌍 — 교체 후보
	classAnomaly                    // 그 외 마커 배치 — 무변경
)

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
// dup은 같은 이름의 헤더가 둘 이상이라는 뜻이며, 그 자체가 TOML 중복 정의라 무변경으로 뺀다.
type codexSpans struct {
	table codexSpan
	env   codexSpan
	dup   bool
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
				out.dup = true
			}
			out.table, cur = codexSpan{start: i, end: len(lines), found: true}, 0
		case codexManagedEnv:
			if out.env.found {
				out.dup = true
			}
			out.env, cur = codexSpan{start: i, end: len(lines), found: true}, 1
		}
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

// codexKeyName — 정규화 라인에서 대입 키 이름을 뽑는다(따옴표 키도 벗긴다). '='가 없거나
// 앞부분이 비면 ""(키 줄이 아니다). 주석 줄은 '#'로 시작하므로 우리 키 이름과 절대 같아지지
// 않는다 — 주석은 언제나 보존 라인으로 간다.
func codexKeyName(s string) string {
	i := strings.Index(s, "=")
	if i <= 0 {
		return ""
	}
	return strings.Trim(s[:i], `"'`)
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

// codexBlockBytes — 파일 지배 개행으로 블록 라인 생성(CRLF 지배 시 CRLF).
func codexBlockBytes(crlf bool) []byte {
	if crlf {
		return []byte(strings.ReplaceAll(codexBlockBody, "\n", "\r\n"))
	}
	return []byte(codexBlockBody)
}

// classifyMarkers — 마커 정확 라인 매치로 배치 분류(계약 2). 소유 replace는 begin/end 인덱스도 반환.
func classifyMarkers(lines [][]byte) (class markerClass, begin, end int) {
	var begins, ends []int
	for i, ln := range lines {
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

// scanOutside — 블록 밖 검사(계약 3). replace 후보는 소유 블록 라인[begin..end]을 제외.
func scanOutside(lines [][]byte, class markerClass, begin, end int) (hasHeader, conflict bool) {
	var hasMcp, hasSignal, assign bool
	for i, ln := range lines {
		if class == classReplace && i >= begin && i <= end {
			continue
		}
		s := stripLine(ln)
		if s == "[mcp_servers.ctr]" {
			hasHeader = true
		}
		if strings.Contains(s, "mcp_servers") {
			hasMcp = true
		}
		if ctrKeySignal(s) {
			hasSignal = true
		}
		if mcpServersAssign(s) {
			assign = true // 루트 mcp_servers 대입 — 단독 충돌(Codex P1)
		}
	}
	return hasHeader, (hasMcp && hasSignal) || assign
}

// installCodexConfigBlock — 관리 블록 병합(스펙 §0 D48·§3). 순수 변환: 파일 IO 없음.
func installCodexConfigBlock(existing []byte) (out []byte, state codexMCPState) {
	lines := splitLinesKeepEnds(existing)
	class, begin, end := classifyMarkers(lines)
	if class == classAnomaly {
		return existing, mcpMarkerAnomaly
	}
	if hasHeader, conflict := scanOutside(lines, class, begin, end); hasHeader {
		return existing, mcpExistingHeader
	} else if conflict {
		return existing, mcpConflict
	}
	crlf := bytes.Contains(existing, []byte("\r\n"))
	block := codexBlockBytes(crlf)
	if class == classReplace {
		return replaceBlock(lines, begin, end, block), mcpWritten
	}
	return appendBlock(existing, block, crlf), mcpWritten
}

// probeCodexMCPBlock — doctor [16] 존재 판별(D52, 스펙 v0.9 §0). install 상태기계는 "무엇을
// 쓸지"의 분류(classReplace/classAppend→동일 mcpWritten)라 존재/부재 판별에 부적합 — 같은
// 순수 라인 헬퍼를 재사용해 읽기 전용으로 판정한다. present: 블록 밖 canonical 헤더 또는
// (충돌 없는) 소유 블록(classReplace). anomaly: 마커 무결성 이상 또는 키-경계 충돌(→ [16]
// warning ⑤). 우선순위는 installCodexConfigBlock과 1:1 대응(hasHeader > conflict > classReplace)
// — canonical 헤더 라인은 스스로 mcp_servers·ctr] 신호를 겸해 conflict도 켜므로(예:
// "[mcp_servers.ctr]"), install처럼 hasHeader를 conflict보다 먼저 봐야 헤더 실존을 존재로 본다.
// 그다음 conflict를 classReplace보다 먼저 판정해야 한다 — install은 class와 무관하게 conflict면
// mcpConflict로 반환(교체 분기 진입 자체를 막음)하므로, 소유 블록(classReplace)이라도 블록 밖에
// 진짜 충돌(예: 루트 mcp_servers 대입)이 있으면 존재가 아니라 이상으로 본다.
func probeCodexMCPBlock(existing []byte) (present bool, anomaly bool) {
	lines := splitLinesKeepEnds(existing)
	class, begin, end := classifyMarkers(lines)
	if class == classAnomaly {
		return false, true
	}
	hasHeader, conflict := scanOutside(lines, class, begin, end)
	if hasHeader {
		return true, false
	}
	if conflict {
		return false, true
	}
	if class == classReplace {
		return true, false
	}
	return false, false
}

// replaceBlock — 소유 블록 라인[begin..end]을 fresh 블록으로 교체, 앞뒤 라인은 바이트 보존.
func replaceBlock(lines [][]byte, begin, end int, block []byte) []byte {
	var out []byte
	for _, ln := range lines[:begin] {
		out = append(out, ln...)
	}
	out = append(out, block...)
	for _, ln := range lines[end+1:] {
		out = append(out, ln...)
	}
	return out
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

// uninstallCodexConfigBlock — 소유 블록 제거(계약 5). 소유 replace 후보만 변경.
func uninstallCodexConfigBlock(existing []byte) (out []byte, changed bool) {
	lines := splitLinesKeepEnds(existing)
	class, begin, end := classifyMarkers(lines)
	if class != classReplace {
		return existing, false
	}
	from := begin
	if begin > 0 && isBlankLine(lines[begin-1]) {
		from = begin - 1 // 직전 빈 줄 1개만 함께 제거
	}
	for _, ln := range lines[:from] {
		out = append(out, ln...)
	}
	for _, ln := range lines[end+1:] {
		out = append(out, ln...)
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
