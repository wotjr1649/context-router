// Package ingest — §3.0 파이프라인·secret 필터·청킹·경로 정책. 설계서 §3.0, §4.4, §5.1.
package ingest

import (
	"bytes"
	"errors"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

var (
	ErrWorkspace   = errors.New("ingest: outside workspace")
	ErrUnsupported = errors.New("ingest: unsupported file")
)

// denyGlobs: base name 매칭 secret 파일명 denylist (설계 §5.1). *.log는 의도적으로 미포함.
var denyGlobs = []string{
	".env*", "*.pem", "*.key", "id_rsa*", "id_ed25519*", "*.pfx", "*.p12", "*.kdbx",
	"credentials*", "*.tfstate", ".netrc", ".npmrc", "*.har", "*.jks", "*.p8", "kubeconfig*",
}

// DeniedFilename reports whether path must be skipped entirely (secret 파일명 denylist, §5.1).
func DeniedFilename(path string) bool {
	base := filepath.Base(path)
	for _, g := range denyGlobs {
		if ok, _ := filepath.Match(g, base); ok {
			return true
		}
	}
	slash := filepath.ToSlash(path)
	return slash == ".docker/config.json" || strings.HasSuffix(slash, "/.docker/config.json")
}

// secretPattern: 컴파일된 정규식 1개 + 치환 대상(전체 매치 또는 값 서브그룹).
// valueGroup==0이면 매치 전체를 치환, >0이면 해당 서브그룹(1-based)만 치환해
// key/prefix(예: "password=", "Cookie:")는 남기고 값만 가린다.
type secretPattern struct {
	typ        string
	re         *regexp.Regexp
	valueGroup int
	token      []byte
}

func mkPattern(typ, pattern string, group int) secretPattern {
	return secretPattern{typ: typ, re: regexp.MustCompile(pattern), valueGroup: group, token: []byte("«REDACTED:" + typ + "»")}
}

// patterns: 설계 §5.1 span redaction 패턴 전수(RE2 컴파일 — ReDoS 없음). 패키지 불변.
var patterns = []secretPattern{
	mkPattern("aws", `AKIA[0-9A-Z]{16}`, 0),
	mkPattern("github", `gh[pousr]_[A-Za-z0-9]{20,}`, 0),
	mkPattern("slack", `xox[baprs]-[A-Za-z0-9-]{10,}`, 0),
	mkPattern("privkey", `(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`, 0),
	// JDBC/ADO 자격 부분(pwd=)도 이 패턴에 통합(설계 §5.1 "위와 통합 가능").
	mkPattern("password", `(?i)(?:password|pwd|passwd)\s*[=:]\s*([^\s;&"']+)`, 1),
	mkPattern("jwt", `eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`, 0),
	mkPattern("authheader", `(?im)^(?:authorization|proxy-authorization):\s*(?:basic|bearer)\s+(\S+)`, 1),
	mkPattern("cookie", `(?im)^(?:cookie|set-cookie):\s*(\S.*)$`, 1),
	mkPattern("dockerauth", `"auth"\s*:\s*"([A-Za-z0-9+/=]{12,})"`, 1),
}

// applyPattern replaces every non-overlapping match of p in b (whole match, or
// just its value subgroup when p.valueGroup>0) with p.token. Returns a new
// slice (or b unchanged if no match) and the number of replacements made.
func applyPattern(b []byte, p secretPattern) ([]byte, int) {
	locs := p.re.FindAllSubmatchIndex(b, -1)
	if len(locs) == 0 {
		return b, 0
	}
	out := make([]byte, 0, len(b))
	last, n := 0, 0
	for _, loc := range locs {
		start, end := loc[0], loc[1]
		if p.valueGroup > 0 {
			gi := p.valueGroup * 2
			if gi+1 >= len(loc) || loc[gi] < 0 {
				continue // 서브그룹 미참여(방어적 — 패턴상 발생 안 함)
			}
			start, end = loc[gi], loc[gi+1]
		}
		out = append(out, b[last:start]...)
		out = append(out, p.token...)
		last = end
		n++
	}
	out = append(out, b[last:]...)
	return out, n
}

// redactPatterns runs every pattern in table order over b, chaining outputs.
func redactPatterns(b []byte) ([]byte, int) {
	total := 0
	for _, p := range patterns {
		nb, n := applyPattern(b, p)
		b = nb
		total += n
	}
	return b, total
}

// jsonLiteralRe matches a single JSON string literal (quotes 포함, 이스케이프 인식).
var jsonLiteralRe = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// unescapeJSONBytes decodes JSON backslash escapes (\", \\, \/, \b\f\n\r\t, \uXXXX
// — 대리쌍도 조합) in inner (quotes 제외). Best-effort: malformed \u는 리터럴 보존.
func unescapeJSONBytes(inner []byte) []byte {
	out := make([]byte, 0, len(inner))
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if c != '\\' || i+1 >= len(inner) {
			out = append(out, c)
			continue
		}
		i++
		switch inner[i] {
		case '"', '\\', '/':
			out = append(out, inner[i])
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			if i+4 >= len(inner) {
				out = append(out, '\\', 'u')
				continue
			}
			v, err := strconv.ParseUint(string(inner[i+1:i+5]), 16, 32)
			if err != nil {
				out = append(out, '\\', 'u')
				continue
			}
			r := rune(v)
			i += 4
			if r >= 0xD800 && r <= 0xDBFF && i+6 < len(inner) && inner[i+1] == '\\' && inner[i+2] == 'u' {
				if v2, err2 := strconv.ParseUint(string(inner[i+3:i+7]), 16, 32); err2 == nil {
					if r2 := rune(v2); r2 >= 0xDC00 && r2 <= 0xDFFF {
						r = ((r - 0xD800) << 10) | (r2 - 0xDC00) + 0x10000
						i += 6
					}
				}
			}
			var buf [4]byte
			out = append(out, buf[:utf8.EncodeRune(buf[:], r)]...)
		default:
			out = append(out, inner[i])
		}
	}
	return out
}

// matchAnyPattern returns the token of the first pattern matching b, or nil.
func matchAnyPattern(b []byte) []byte {
	for _, p := range patterns {
		if p.re.Match(b) {
			return p.token
		}
	}
	return nil
}

// redactJSONEscaped: 2번째 뷰 — JSON 문자열 리터럴 중 백슬래시 이스케이프를 포함한
// 것만 디코드해 비밀 패턴 검사, 매치 시 raw의 해당 리터럴 값 전체를 치환(보수적
// 접근 — 부분 오프셋 역산 대신 리터럴 전체를 가림). 이스케이프 없는 리터럴은
// redactPatterns(raw 뷰)가 이미 처리했으므로 스킵.
func redactJSONEscaped(b []byte) ([]byte, int) {
	locs := jsonLiteralRe.FindAllIndex(b, -1)
	if len(locs) == 0 {
		return b, 0
	}
	out := make([]byte, 0, len(b))
	last, n := 0, 0
	for _, loc := range locs {
		start, end := loc[0], loc[1]
		inner := b[start+1 : end-1]
		if bytes.IndexByte(inner, '\\') == -1 {
			continue
		}
		tok := matchAnyPattern(unescapeJSONBytes(inner))
		if tok == nil {
			continue
		}
		out = append(out, b[last:start+1]...) // 여는 따옴표까지 포함
		out = append(out, tok...)
		out = append(out, '"')
		last = end
		n++
	}
	out = append(out, b[last:]...)
	return out, n
}

// Redact scans b for secret spans and replaces them with «REDACTED:<type>» markers (§5.1).
// 2뷰 스캔: raw 바이트 뷰(redactPatterns) 후 JSON-unescape 뷰(redactJSONEscaped) —
// \uXXXX 등으로 은닉된 비밀도 잡는다.
func Redact(b []byte) (out []byte, spans int) {
	out = append([]byte(nil), b...) // 항상 새 슬라이스 반환(계약) — 입력 별칭 금지
	var n1, n2 int
	out, n1 = redactPatterns(out)
	out, n2 = redactJSONEscaped(out)
	spans = n1 + n2
	return out, spans
}
