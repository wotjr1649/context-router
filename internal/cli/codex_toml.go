package cli

// D48 — config.toml 관리 블록 병합(스펙 v0.7 §0 D48·§3). TOML 파서 비의존(신규 의존 금지):
// 마커 정확 라인 매치 + 본문 검증(소유) + 키-경계 보수 스캔(충돌)이 파스 에러 방향
// (중복 정의로 사용자 Codex 전체 파손)을 구조 봉쇄한다. 순수 바이트 변환 — IO는 호출자.

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
