// Package ingest — §3.0 파이프라인·secret 필터·청킹·경로 정책. 설계서 §3.0, §4.4, §5.1.
package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/store"
)

var (
	ErrWorkspace   = errors.New("ingest: outside workspace")
	ErrUnsupported = errors.New("ingest: unsupported file")
)

// Request/Report/SkipEntry: Run 입출력 계약 (설계 §3.0, §4.4).
type Request struct {
	Path, Content, Title string
	Include, Exclude     []string
	MaxFileBytes         int64
}

type SkipEntry struct{ Path, Reason string }

type Report struct {
	Indexed     int
	BytesStored int64
	Skipped     []SkipEntry
}

const defaultMaxFileBytes = 5 << 20 // 5MB (§4.4 기본값)

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

// headingRe matches an ATX 헤딩 라인(트레일링 개행 제거 후 매칭): 1~6개 '#' + 공백/탭 + 텍스트.
var headingRe = regexp.MustCompile(`^#{1,6}[ \t]+\S`)

// headingText reports whether line(트레일링 개행 포함 가능)이 Markdown 헤딩이면
// (제목 텍스트, true)를 반환한다.
func headingText(line string) (string, bool) {
	t := strings.TrimRight(line, "\r\n")
	if !headingRe.MatchString(t) {
		return "", false
	}
	i := strings.IndexAny(t, " \t")
	return strings.TrimSpace(t[i+1:]), true
}

// splitLines splits text into lines that each retain their trailing '\n'(마지막
// 줄만 없을 수 있음) — 그러므로 lines를 순서대로 이어붙이면 text가 정확히
// 복원된다. offsets[i]는 lines[i]가 시작하는 바이트 오프셋, offsets[len(lines)]는
// len(text)와 같다.
func splitLines(text string) (lines []string, offsets []int64) {
	offsets = append(offsets, 0)
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			lines = append(lines, text[start:i+1])
			offsets = append(offsets, int64(i+1))
			start = i + 1
		}
	}
	if start < len(text) {
		lines = append(lines, text[start:])
		offsets = append(offsets, int64(len(text)))
	}
	return lines, offsets
}

const chunkTargetBytes = 4096

// ChunkText splits text into ~4KB 라인 블록 청크(브리프/설계 §3.4): 4096B를 넘기기
// 직전 줄에서 절단하고, Markdown(isMarkdown)이면 헤딩 라인을 새 청크 시작 경계로
// 우선하며 각 청크의 Title에 직전 헤딩 텍스트를 채운다. 청크 간 1라인 오버랩(단,
// 청크가 정확히 1줄이면 오버랩 시 무한 루프이므로 생략). 좌표는 text(저장본)
// 기준 — ByteEnd는 반개구간 끝, LineStart/LineEnd는 1-기반 포함 구간.
func ChunkText(text string, isMarkdown bool) []store.Chunk {
	if text == "" {
		return nil
	}
	lines, offsets := splitLines(text)
	var chunks []store.Chunk
	lastTitle := ""
	ordinal := 0
	for i := 0; i < len(lines); {
		start := i
		titleAtStart := lastTitle
		curBytes := 0
		j := i
		headingBreak := false
		for j < len(lines) {
			if isMarkdown {
				if h, ok := headingText(lines[j]); ok {
					if j == start {
						titleAtStart = h
						lastTitle = h
					} else {
						headingBreak = true
						break // 헤딩 경계 — 다음 청크가 여기서 시작
					}
				}
			}
			lineLen := len(lines[j])
			if curBytes > 0 && curBytes+lineLen > chunkTargetBytes {
				break
			}
			curBytes += lineLen
			j++
		}
		if j <= start { // 방어적 안전망 — 위 루프 구조상 도달하지 않음(무한 루프 차단)
			j = start + 1
		}
		end := j
		title := ""
		if isMarkdown {
			title = titleAtStart
		}
		chunks = append(chunks, store.Chunk{
			Ordinal: ordinal, ByteStart: offsets[start], ByteEnd: offsets[end],
			LineStart: start + 1, LineEnd: end, Title: title, Text: text[offsets[start]:offsets[end]],
		})
		ordinal++
		if end >= len(lines) {
			break
		}
		if !headingBreak && end-start > 1 {
			i = end - 1 // 1라인 오버랩 — 헤딩 경계에서는 생략(직전 절 마지막 줄이 다음
			// 청크 머리에서 중복+ByteEnd 미증가하는 퇴화 청크를 막는다)
		} else {
			i = end
		}
	}
	return chunks
}

// isMarkdownExt reports whether name의 확장자가 .md/.markdown인지(대소문자 무시).
func isMarkdownExt(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".md" || ext == ".markdown"
}

// isBinary sniffs the first 8KB of b for a NUL byte (§4.4 스킵 규칙).
func isBinary(b []byte) bool {
	n := len(b)
	if n > 8192 {
		n = 8192
	}
	return bytes.IndexByte(b[:n], 0) != -1
}

// canonicalPath resolves p to its absolute, symlink-free real path(원본 케이스 유지).
func canonicalPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("ingest: canonicalize: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("ingest: canonicalize: %w", err)
	}
	return real, nil
}

// withinRoot reports whether real(canonicalPath 결과)이 foldedRoot(ident.Fold된 허용
// root) 하위인지 — filepath.Rel 결과가 ".." 세그먼트로 시작하면 위반(설계 §4.4:
// 문자열 접두사 매칭 금지 — /proj vs /proj-evil 오탐 차단).
func withinRoot(foldedRoot, real string) bool {
	rel, err := filepath.Rel(foldedRoot, ident.Fold(real))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func withinAny(foldedRoots []string, real string) bool {
	for _, r := range foldedRoots {
		if withinRoot(r, real) {
			return true
		}
	}
	return false
}

// relDisplay: project-relative 표시 경로(SkipEntry.Path 등 — 절대경로 노출 금지).
func relDisplay(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return filepath.Base(p)
	}
	return filepath.ToSlash(rel)
}

func globMatchAny(patterns []string, name string) bool {
	for _, g := range patterns {
		if ok, _ := filepath.Match(g, name); ok {
			return true
		}
	}
	return false
}

// workItem: WalkDir 수집 단계(메타데이터·정책 판정)와 워커 풀(읽기·해시·redact·
// chunk) 사이의 작업 단위. preSkip이 채워지면 워커는 I/O 없이 즉시 그 사유로 뭉갠다.
type workItem struct {
	abs, rel, base string
	size, mtimeNS  int64
	preSkip        string
}

// collect walks root(디렉터리): 파일마다 재canonicalize+경계 재검증(심링크 이탈
// 차단) 후 Include/Exclude(조용히 제외)·DeniedFilename·크기 상한을 판정해 workItem을
// 만든다. WalkDir 자체 오류(루트 접근 불가)만 error로 전파.
func collect(ctx context.Context, root string, foldedRoots []string, projectRoot string, req Request, maxBytes int64) ([]workItem, error) {
	var items []workItem
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			items = append(items, workItem{rel: relDisplay(projectRoot, p), preSkip: "unreadable"})
			return nil
		}
		if d.IsDir() {
			return nil
		}
		base := d.Name()
		if len(req.Include) > 0 && !globMatchAny(req.Include, base) {
			return nil
		}
		if globMatchAny(req.Exclude, base) {
			return nil
		}
		real, cerr := canonicalPath(p)
		if cerr != nil {
			items = append(items, workItem{rel: relDisplay(projectRoot, p), preSkip: "unreadable"})
			return nil
		}
		if !withinAny(foldedRoots, real) {
			items = append(items, workItem{rel: relDisplay(projectRoot, p), preSkip: "outside-workspace"})
			return nil
		}
		info, serr := os.Stat(real)
		if serr != nil {
			items = append(items, workItem{rel: relDisplay(projectRoot, p), preSkip: "unreadable"})
			return nil
		}
		if info.IsDir() { // 심링크→디렉터리: 파일 아님, 조용히 제외
			return nil
		}
		if DeniedFilename(base) {
			items = append(items, workItem{rel: relDisplay(projectRoot, p), preSkip: "secret-denylist"})
			return nil
		}
		if info.Size() > maxBytes {
			items = append(items, workItem{rel: relDisplay(projectRoot, p), preSkip: "too-large"})
			return nil
		}
		items = append(items, workItem{
			abs: real, rel: relDisplay(projectRoot, p), base: base,
			size: info.Size(), mtimeNS: info.ModTime().UnixNano(),
		})
		return nil
	})
	if err != nil {
		return items, fmt.Errorf("ingest: run: %w", err)
	}
	return items, nil
}

// ingestOne runs the §3.0 pipeline for one workItem: 원본읽기→binary sniff→
// src_hash→Redact→ChunkText→store.Register. preSkip이 있으면 I/O 없이 그 사유를
// 반환한다. 개별 파일 실패는 전부 reason 문자열로 흡수(전체 실패시키지 않음).
func ingestOne(ctx context.Context, st *store.Store, w workItem) (skipReason string, storedBytes int64) {
	if w.preSkip != "" {
		return w.preSkip, 0
	}
	raw, err := os.ReadFile(w.abs)
	if err != nil {
		return "unreadable", 0
	}
	if isBinary(raw) {
		return "binary", 0
	}
	sum := sha256.Sum256(raw)
	srcHash := hex.EncodeToString(sum[:])
	stored, spans := Redact(raw)
	redaction := "none"
	if spans > 0 {
		redaction = "spans"
	}
	md := isMarkdownExt(w.base)
	mediaType := "text/plain"
	if md {
		mediaType = "text/markdown"
	}
	_, err = st.Register(ctx, store.Registration{
		StoredBytes: stored,
		MediaType:   mediaType,
		Redaction:   redaction,
		Source: store.SourceMeta{
			URI: ident.Fold(w.abs), Kind: "file",
			Size: w.size, MtimeNS: w.mtimeNS, SrcHash: srcHash,
		},
		Chunks: ChunkText(string(stored), md),
	})
	if err != nil {
		return "register-failed", 0
	}
	return "", int64(len(stored))
}

// runPool: min(GOMAXPROCS,4) 고정 worker pool로 items를 병렬 처리한다(파일별
// goroutine 생성 금지 — 규약 §7). store.Register 직렬화는 store 내부(writer
// SetMaxOpenConns(1))가 보장하므로 워커가 각자 호출해도 안전하다.
func runPool(ctx context.Context, st *store.Store, items []workItem) Report {
	n := runtime.GOMAXPROCS(0)
	if n > 4 {
		n = 4
	}
	if n < 1 {
		n = 1
	}

	ch := make(chan workItem)
	var mu sync.Mutex
	var rep Report
	var wg sync.WaitGroup
	wg.Add(n)
	for k := 0; k < n; k++ {
		go func() {
			defer wg.Done()
			for it := range ch {
				reason, stored := ingestOne(ctx, st, it)
				mu.Lock()
				if reason != "" {
					rep.Skipped = append(rep.Skipped, SkipEntry{Path: it.rel, Reason: reason})
				} else {
					rep.Indexed++
					rep.BytesStored += stored
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for _, it := range items {
		select {
		case <-ctx.Done():
			break feed
		case ch <- it:
		}
	}
	close(ch)
	wg.Wait()
	return rep
}

// runInline ingests req.Content directly (uri=inline:<Title>) — §3.0 순서에서 파일
// 읽기 단계만 생략한다. ponytail: 경로 정책(denylist·바이너리 sniff·크기 상한)은
// 디스크 파일 고유 위험을 다루는 것이라 호출자가 이미 신뢰하고 전달한 인라인
// 텍스트에는 적용하지 않는다 — 필요해지면 여기서 동일 검사를 추가한다.
func runInline(ctx context.Context, st *store.Store, req Request) (Report, error) {
	raw := []byte(req.Content)
	sum := sha256.Sum256(raw)
	srcHash := hex.EncodeToString(sum[:])
	stored, spans := Redact(raw)
	redaction := "none"
	if spans > 0 {
		redaction = "spans"
	}
	md := isMarkdownExt(req.Path) // 인라인 모드에서도 Path를 확장자 힌트로 재사용
	mediaType := "text/plain"
	if md {
		mediaType = "text/markdown"
	}
	_, err := st.Register(ctx, store.Registration{
		StoredBytes: stored,
		MediaType:   mediaType,
		Redaction:   redaction,
		Source: store.SourceMeta{
			URI: "inline:" + req.Title, Kind: "inline",
			Size: int64(len(raw)), SrcHash: srcHash,
		},
		Chunks: ChunkText(string(stored), md),
	})
	if err != nil {
		return Report{}, fmt.Errorf("ingest: run: %w", err)
	}
	return Report{Indexed: 1, BytesStored: int64(len(stored))}, nil
}

// Run ingests req.Path(파일|디렉터리) 또는 req.Content(inline)를 §3.0 파이프라인으로
// 저장한다. 경로 정책(설계 §4.4): 허용 root=projectRoot+allowPaths(이미 canonical),
// 위반은 ErrWorkspace. 개별 파일 실패는 Report.Skipped로, 경계 위반·루트 접근
// 불가만 error 반환.
func Run(ctx context.Context, st *store.Store, projectRoot string, allowPaths []string, req Request) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	if req.Content != "" {
		return runInline(ctx, st, req)
	}

	maxBytes := req.MaxFileBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxFileBytes
	}

	real, err := canonicalPath(req.Path)
	if err != nil {
		return Report{}, err
	}
	foldedRoots := make([]string, 0, 1+len(allowPaths))
	foldedRoots = append(foldedRoots, ident.Fold(projectRoot))
	for _, p := range allowPaths {
		foldedRoots = append(foldedRoots, ident.Fold(p))
	}
	if !withinAny(foldedRoots, real) {
		return Report{}, fmt.Errorf("ingest: run: %w", ErrWorkspace)
	}

	info, err := os.Stat(real)
	if err != nil {
		return Report{}, fmt.Errorf("ingest: run: %w", err)
	}

	var items []workItem
	if info.IsDir() {
		items, err = collect(ctx, real, foldedRoots, projectRoot, req, maxBytes)
		if err != nil {
			return Report{}, err
		}
	} else {
		base := filepath.Base(real)
		rel := relDisplay(projectRoot, real)
		var it workItem
		switch {
		case DeniedFilename(base):
			it = workItem{rel: rel, preSkip: "secret-denylist"}
		case info.Size() > maxBytes:
			it = workItem{rel: rel, preSkip: "too-large"}
		default:
			it = workItem{abs: real, rel: rel, base: base, size: info.Size(), mtimeNS: info.ModTime().UnixNano()}
		}
		items = []workItem{it}
	}

	return runPool(ctx, st, items), nil
}
