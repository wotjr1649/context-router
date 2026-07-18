package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/store"
)

func TestRedact_Canaries(t *testing.T) {
	tests := []struct{ name, in, mustGone string }{
		{"aws", "key=AKIAIOSFODNN7EXAMPLE ok", "AKIAIOSFODNN7EXAMPLE"},
		{"github", "token ghp_abcdefghijklmnopqrstuvwxyz012345 x", "ghp_abcdefghijklmnopqrstuvwxyz012345"},
		{"privkey-multiline", "a\n-----BEGIN RSA PRIVATE KEY-----\nMIIE\nxyz\n-----END RSA PRIVATE KEY-----\nb", "MIIE"},
		{"authorization", "Authorization: Bearer eyJhbGciOi.something.sig", "eyJhbGciOi"},
		{"cookie", "Set-Cookie: session=SECRETVAL; Path=/", "SECRETVAL"},
		{"docker-auth", `{"auths":{"r.io":{"auth":"dXNlcjpwYXNzd29yZDEyMw=="}}}`, "dXNlcjpwYXNzd29yZDEyMw"},
		{"json-escaped", `{"t":"ghp_abcdefghijklmnopqrstuvwxyz012345"}`, "abcdefghijklmnopqrstuvwxyz012345"},
		{"password-kv", "password=hunter2xx;db=x", "hunter2xx"},
		// ghp_ — raw 바이트에 "ghp_"가 없음(아래 가드로 실증). unescape 뷰만 잡을 수 있음.
		{"json-escaped-real", `{"k":"gh\p_abcdefghijklmnopqrstuvwxyz012345"}`, "abcdefghijklmnopqrstuvwxyz012345"},
		{"jwt-bare", `{"token":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV"}`, "SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV"},
		// ponytail: 런타임 분할 리터럴 — 소스에 연속 xoxb- 토큰이 없어 secret-scanner 오탐 방지 (규약 §8).
		// 값도 실제 Slack 토큰의 -<숫자>-<영숫자> 다중 세그먼트 구조와 다른 명백한 테스트 문자열.
		{"slack", "hook " + "xox" + "b-CANARYtestnotarealtoken" + " end", "xox" + "b-CANARYtestnotarealtoken"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "json-escaped-real" && strings.Contains(tt.in, "ghp_") {
				t.Fatal("입력에 평문 ghp_ 존재 — 2뷰(unescape) 경로 미검증")
			}
			out, spans := Redact([]byte(tt.in))
			if strings.Contains(string(out), tt.mustGone) {
				t.Fatalf("누출: %q 가 남음\n%s", tt.mustGone, out)
			}
			if spans == 0 {
				t.Fatal("spans=0")
			}
			if !strings.Contains(string(out), "«REDACTED:") {
				t.Fatalf("마커 없음: %s", out)
			}
		})
	}
}

// FuzzRedact: 불변식은 panic 없음(RE2 매치·슬라이스 조립 어디도 임의 입력에 패닉해선
// 안 됨). 시드는 canary 입력들.
func FuzzRedact(f *testing.F) {
	for _, s := range []string{
		"key=AKIAIOSFODNN7EXAMPLE ok",
		"token ghp_abcdefghijklmnopqrstuvwxyz012345 x",
		"a\n-----BEGIN RSA PRIVATE KEY-----\nMIIE\nxyz\n-----END RSA PRIVATE KEY-----\nb",
		"Authorization: Bearer eyJhbGciOi.something.sig",
		"Set-Cookie: session=SECRETVAL; Path=/",
		`{"auths":{"r.io":{"auth":"dXNlcjpwYXNzd29yZDEyMw=="}}}`,
		`{"t":"ghp_abcdefghijklmnopqrstuvwxyz012345"}`,
		"password=hunter2xx;db=x",
		`{"k":"gh\p_abcdefghijklmnopqrstuvwxyz012345"}`,
		`{"token":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV"}`,
	} {
		f.Add([]byte(s))
	}
	f.Add([]byte(`{"k":"gh` + string(rune(92)) + `u0070_abcdefghijklmnopqrstuvwxyz012345"}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		out, spans := Redact(b)
		if spans < 0 {
			t.Fatalf("negative spans: %d", spans)
		}
		_ = out
	})
}

func TestRedact_DoesNotMutateInput(t *testing.T) {
	in := []byte("key=AKIAIOSFODNN7EXAMPLE ok")
	orig := append([]byte(nil), in...)
	out, _ := Redact(in)
	if !bytes.Equal(in, orig) {
		t.Fatal("입력이 변조됨")
	}
	if bytes.Contains(out, []byte("AKIAIOSFODNN7EXAMPLE")) {
		t.Fatal("비밀 잔존")
	}
}

// TestRedact_UnicodeEscapedSecret: unescapeJSONBytes의 \uXXXX 디코드 분기(실전 은닉의
// 주 벡터) 실증 — json-escaped-real(Fix Round 1)은 default 분기(\p→p)만 거쳤다.
func TestRedact_UnicodeEscapedSecret(t *testing.T) {
	esc := string(rune(92)) + "u0070" // 백슬래시(92)+"u0070" == p('p')
	in := []byte(`{"k":"gh` + esc + `_abcdefghijklmnopqrstuvwxyz012345"}`)
	if bytes.Contains(in, []byte("ghp_")) {
		t.Fatal("입력에 평문 ghp_ 존재 — \\u 디코드 경로 미검증")
	}
	out, spans := Redact(in)
	if bytes.Contains(out, []byte("abcdefghijklmnopqrstuvwxyz012345")) {
		t.Fatalf("누출: %s", out)
	}
	if spans < 1 {
		t.Fatal("spans<1")
	}
}

var updateGolden = flag.Bool("update", false, "update golden files")

func goldenPath(name string) string { return filepath.Join("testdata", "golden", name) }

// assertChunkInvariants: 좌표 유효성·단조 증가·Text==text[ByteStart:ByteEnd]·오버랩
// 제외 재결합 시 원문 복원을 검증한다(ChunkText 골든·synthetic·fuzz 테스트 공용).
func assertChunkInvariants(t *testing.T, text string, chunks []store.Chunk) {
	t.Helper()
	var buf strings.Builder
	pos := int64(0)
	for i, c := range chunks {
		if c.Ordinal != i {
			t.Fatalf("ordinal[%d]: got=%d", i, c.Ordinal)
		}
		if c.ByteStart < 0 || c.ByteEnd > int64(len(text)) || c.ByteStart >= c.ByteEnd {
			t.Fatalf("chunk[%d] byte 범위 이상: [%d,%d) len=%d", i, c.ByteStart, c.ByteEnd, len(text))
		}
		if c.LineStart < 1 || c.LineEnd < c.LineStart {
			t.Fatalf("chunk[%d] line 범위 이상: [%d,%d]", i, c.LineStart, c.LineEnd)
		}
		if c.Text != text[c.ByteStart:c.ByteEnd] {
			t.Fatalf("chunk[%d].Text != text[ByteStart:ByteEnd]", i)
		}
		if i > 0 {
			p := chunks[i-1]
			if c.ByteStart <= p.ByteStart || c.ByteEnd <= p.ByteEnd || c.LineStart <= p.LineStart {
				t.Fatalf("chunk[%d] 좌표 비단조 (prev=%+v cur=%+v)", i, p, c)
			}
		}
		start := c.ByteStart
		if start < pos {
			start = pos
		}
		if start <= c.ByteEnd {
			buf.WriteString(text[start:c.ByteEnd])
		}
		pos = c.ByteEnd
	}
	if buf.String() != text {
		t.Fatalf("재결합 불일치: got %d bytes want %d bytes", buf.Len(), len(text))
	}
}

func chunkTextGolden(t *testing.T, srcName, jsonName string, isMarkdown bool) {
	t.Helper()
	src, err := os.ReadFile(goldenPath(srcName))
	if err != nil {
		t.Fatal(err)
	}
	got := ChunkText(string(src), isMarkdown)
	assertChunkInvariants(t, string(src), got)
	if *updateGolden {
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath(jsonName), b, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Skip("golden 갱신됨 — -update 없이 재실행해 확인")
	}
	wantB, err := os.ReadFile(goldenPath(jsonName))
	if err != nil {
		t.Fatalf("golden 없음(-update로 먼저 생성): %v", err)
	}
	var want []store.Chunk
	if err := json.Unmarshal(wantB, &want); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("청크 수: got=%d want=%d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("chunk[%d]:\ngot =%+v\nwant=%+v", i, got[i], want[i])
		}
	}
}

func TestChunkText_Golden_Plain(t *testing.T) {
	chunkTextGolden(t, "plain.txt", "plain.chunks.json", false)
}

func TestChunkText_Golden_Doc(t *testing.T) {
	chunkTextGolden(t, "doc.md", "doc.chunks.json", true)
}

func TestChunkText_Invariants_Synthetic(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		isMarkdown bool
	}{
		{"empty", "", false},
		{"single-short-line", "hello\n", false},
		{"single-huge-line", strings.Repeat("x", 5000) + "\n", false},
		{"no-trailing-newline", "abc\ndef", false},
		{"headings-back-to-back", "# A\n## B\n### C\nbody\n", true},
		{"markdown-off-ignores-headings", "# A\n## B\nbody\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks := ChunkText(tc.text, tc.isMarkdown)
			assertChunkInvariants(t, tc.text, chunks)
			if tc.name == "empty" && chunks != nil {
				t.Fatal("빈 입력은 nil 청크 기대")
			}
			if tc.name == "markdown-off-ignores-headings" {
				for _, c := range chunks {
					if c.Title != "" {
						t.Fatalf("isMarkdown=false인데 Title 채워짐: %q", c.Title)
					}
				}
			}
		})
	}
}

// TestChunkText_OverlapOnlyChunk_Regression: 리뷰어 반례(500B/500B/4090B/10B) —
// 오버랩 생략이 headingBreak에만 조건화되면 예산 절단 시점에 현재 청크가
// 오버랩 라인만 담은 채 재절단되어 ByteEnd 비단조(퇴화 청크)가 발생했다.
// assertChunkInvariants(이미 ByteEnd 단조 검사 포함)에 더해 인접 쌍 ByteEnd
// 엄격 증가를 명시적으로도 재확인한다.
func TestChunkText_OverlapOnlyChunk_Regression(t *testing.T) {
	text := strings.Repeat("a", 499) + "\n" +
		strings.Repeat("b", 499) + "\n" +
		strings.Repeat("c", 4089) + "\n" +
		strings.Repeat("d", 9) + "\n"
	cases := []struct {
		name       string
		isMarkdown bool
	}{
		{"plain", false},
		{"markdown-no-heading", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks := ChunkText(text, tc.isMarkdown)
			assertChunkInvariants(t, text, chunks)
			for i := 1; i < len(chunks); i++ {
				if chunks[i].ByteEnd <= chunks[i-1].ByteEnd {
					t.Fatalf("chunk[%d].ByteEnd=%d 가 chunk[%d].ByteEnd=%d 이하 — 오버랩 전용 퇴화 청크(진전 불변식 위반)",
						i, chunks[i].ByteEnd, i-1, chunks[i-1].ByteEnd)
				}
			}
		})
	}
}

func FuzzChunkText(f *testing.F) {
	f.Add("hello\nworld\n", true)
	f.Add("# H\nbody\n## H2\nmore body here\n", true)
	f.Add(strings.Repeat("line filler text\n", 400), false)
	f.Add("", false)
	f.Add("no newline at all", false)
	// 리뷰어 반례(500B/500B/4090B/10B) — 오버랩 전용 퇴화 청크 회귀 시드.
	f.Add(strings.Repeat("a", 499)+"\n"+strings.Repeat("b", 499)+"\n"+strings.Repeat("c", 4089)+"\n"+strings.Repeat("d", 9)+"\n", false)
	f.Fuzz(func(t *testing.T, text string, isMarkdown bool) {
		chunks := ChunkText(text, isMarkdown)
		assertChunkInvariants(t, text, chunks)
	})
}

func TestDeniedFilename(t *testing.T) {
	for _, p := range []string{".env", ".env.local", "id_rsa", "cert.pem", "x.har", "kubeconfig", "a/.docker/config.json", "k.jks", "s.p8", "cred.tfstate"} {
		if !DeniedFilename(p) {
			t.Fatalf("허용됨: %s", p)
		}
	}
	for _, p := range []string{"build.log", "main.go", "config.json"} { // 일반 config.json은 허용
		if DeniedFilename(p) {
			t.Fatalf("차단됨: %s", p)
		}
	}
}

// TestRun_DeniedFilename_SubdirPathSuffix: DeniedFilename의 ".docker/config.json"
// 접미 규칙은 base name만 받으면 절대 매치되지 않는다(β1-1 사문화) — collect가
// base 대신 전체 상대경로도 넘기는지 실증.
func TestRun_DeniedFilename_SubdirPathSuffix(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "x", ".docker", "config.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"auths":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	st, _ := openStoreT(t)
	rep, err := Run(context.Background(), st, root, nil, Request{Path: root})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Indexed != 0 || len(rep.Skipped) != 1 || rep.Skipped[0].Reason != "secret-denylist" {
		t.Fatalf("rep=%+v want 0 indexed / 1 skip secret-denylist", rep)
	}
}

// TestRun_DeniedFilename_SymlinkBypass: 심링크(safe.txt -> .env)로 denylist를
// 우회할 수 있었다(β1-1) — collect가 canonicalize된 real 경로도 검사하는지 실증.
// unix 한정(windows는 심링크 생성 권한 부족 시 skip).
func TestRun_DeniedFilename_SymlinkBypass(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "safe.txt")
	if err := os.Symlink(filepath.Join(root, ".env"), link); err != nil {
		t.Skipf("심링크 생성 실패(권한 부족으로 추정) — skip: %v", err)
	}
	st, _ := openStoreT(t)
	rep, err := Run(context.Background(), st, root, nil, Request{Path: root})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Indexed != 0 || len(rep.Skipped) != 2 {
		t.Fatalf("rep=%+v want 0 indexed / 2 skip (both secret-denylist)", rep)
	}
	for _, s := range rep.Skipped {
		if s.Reason != "secret-denylist" {
			t.Fatalf("skip=%+v want secret-denylist", s)
		}
	}
}

// TestCanonicalUnchanged: β1-2 TOCTOU 재검증 로직 단위 테스트(함수 분리로 검증 —
// 실제 레이스 재현 불요). canonical한 경로는 true, 그 자리가 다른 곳으로
// 재링크되면 false. unix 한정(windows는 심링크 생성 권한 부족 시 skip).
func TestCanonicalUnchanged(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	real, err := canonicalPath(f)
	if err != nil {
		t.Fatal(err)
	}
	if !canonicalUnchanged(real) {
		t.Fatal("변화 없는데 false")
	}

	elsewhere := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(elsewhere, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, real); err != nil {
		t.Skipf("심링크 생성 실패(권한 부족으로 추정) — skip: %v", err)
	}
	if canonicalUnchanged(real) {
		t.Fatal("교체됐는데 true")
	}
}

// TestIsBinary_NonUTF8: NUL 없는 무효 UTF-8(CP949 등)도 binary 게이트로 skip돼야
// 한다(β1-3 — byte-exact 계약 보호).
func TestIsBinary_NonUTF8(t *testing.T) {
	b := bytes.Repeat([]byte{0xB0, 0xA1, 0xFF, 0xFE}, 50)
	if !isBinary(b) {
		t.Fatal("무효 UTF-8인데 isBinary=false")
	}
}

// TestFatalRegisterErr: β1-4 분기 단위 테스트 — ctx 취소·store.ErrUnavailable은
// fatal(전파), 그 외 개별 오류는 skip 흡수 유지.
func TestFatalRegisterErr(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{context.Canceled, true},
		{context.DeadlineExceeded, true},
		{store.ErrUnavailable, true},
		{errors.New("disk full"), false},
	}
	for _, tc := range cases {
		if got := fatalRegisterErr(tc.err); got != tc.want {
			t.Fatalf("fatalRegisterErr(%v)=%v want %v", tc.err, got, tc.want)
		}
	}
}

// TestRunPool_CancelledCtxPropagatesError: 취소된 ctx로 인한 store.Register 실패가
// "register-failed" skip으로 흡수되지 않고 error로 전파돼야 한다(β1-4).
func TestRunPool_CancelledCtxPropagatesError(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "a.txt")
	if err := os.WriteFile(f, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	real, err := canonicalPath(f)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(real)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := openStoreT(t)
	items := []workItem{{
		abs: real, rel: "a.txt", base: "a.txt",
		size: info.Size(), mtimeNS: info.ModTime().UnixNano(), maxBytes: defaultMaxFileBytes,
	}}

	cctx, cancel := context.WithCancel(context.Background())
	cancel()

	rep, err := runPool(cctx, st, items)
	if err == nil {
		t.Fatalf("취소된 ctx인데 error==nil (rep=%+v)", rep)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v want context.Canceled 포함", err)
	}
	for _, s := range rep.Skipped {
		if s.Reason == "register-failed" {
			t.Fatalf("취소가 register-failed skip으로 흡수됨: %+v", rep)
		}
	}
}

func sha256hex(t *testing.T, b []byte) string {
	t.Helper()
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func openStoreT(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, dir
}

// TestRun_DirectoryPipeline: 임시 프로젝트(일반/비밀포함/denylist/6MB초과/서브디렉터리)를
// 워크해 Report{Indexed,Skipped}와 store 반영(redaction·src_hash≠content_hash)을 검증.
func TestRun_DirectoryPipeline(t *testing.T) {
	root := t.TempDir()
	st, storeDir := openStoreT(t)

	secretContent := "key=AKIAIOSFODNN7EXAMPLE ok\n"
	writeFile := func(rel string, data []byte) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("notes.txt", []byte("hello world\nsecond line\n"))
	writeFile("withsecret.txt", []byte(secretContent))
	writeFile(".env", []byte("SECRET=1\n"))
	writeFile("big.bin", bytes.Repeat([]byte("a"), 6<<20)) // 기본 5MB 초과
	writeFile("sub/nested.txt", []byte("nested content\n"))

	rep, err := Run(context.Background(), st, root, nil, Request{Path: root})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Indexed != 3 {
		t.Fatalf("Indexed=%d want 3 (skipped=%+v)", rep.Indexed, rep.Skipped)
	}
	if len(rep.Skipped) != 2 {
		t.Fatalf("Skipped=%d want 2: %+v", len(rep.Skipped), rep.Skipped)
	}
	reasons := map[string]int{}
	for _, s := range rep.Skipped {
		reasons[s.Reason]++
		if filepath.IsAbs(s.Path) {
			t.Fatalf("SkipEntry.Path가 절대경로: %s", s.Path)
		}
		if strings.Contains(s.Path, root) || strings.Contains(s.Path, ident.Fold(root)) {
			t.Fatalf("SkipEntry.Path에 프로젝트 루트 노출: %s (root=%s)", s.Path, root)
		}
	}
	if reasons["secret-denylist"] != 1 || reasons["too-large"] != 1 {
		t.Fatalf("reasons=%+v (skipped=%+v)", reasons, rep.Skipped)
	}
	if rep.BytesStored <= 0 {
		t.Fatalf("BytesStored=%d", rep.BytesStored)
	}

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(storeDir, "content.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var srcHash, redaction, contentHash string
	err = db.QueryRow(`SELECT s.src_hash, a.redaction, a.content_hash FROM sources s
		JOIN artifacts a ON a.id = s.artifact_id WHERE s.uri LIKE ?`, "%withsecret.txt").
		Scan(&srcHash, &redaction, &contentHash)
	if err != nil {
		t.Fatal(err)
	}
	if want := sha256hex(t, []byte(secretContent)); srcHash != want {
		t.Fatalf("src_hash=%s want=%s", srcHash, want)
	}
	if redaction != "spans" {
		t.Fatalf("redaction=%s want spans", redaction)
	}
	if srcHash == contentHash {
		t.Fatal("src_hash == content_hash — redaction이 저장본에 반영되지 않음")
	}
}

func TestRun_PathEscape_Absolute(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, _ := openStoreT(t)

	_, err := Run(context.Background(), st, root, nil, Request{Path: outsideFile})
	if !errors.Is(err, ErrWorkspace) {
		t.Fatalf("err=%v want ErrWorkspace", err)
	}
}

func TestRun_PathEscape_Symlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "real.txt")
	if err := os.WriteFile(outsideFile, []byte("nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape.txt")
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Skipf("심링크 생성 실패(권한 부족으로 추정) — skip: %v", err)
	}
	st, _ := openStoreT(t)

	_, err := Run(context.Background(), st, root, nil, Request{Path: link})
	if !errors.Is(err, ErrWorkspace) {
		t.Fatalf("err=%v want ErrWorkspace", err)
	}
}

func TestRun_SingleFile(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "one.md")
	if err := os.WriteFile(f, []byte("# Solo\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, _ := openStoreT(t)
	rep, err := Run(context.Background(), st, root, nil, Request{Path: f})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Indexed != 1 || len(rep.Skipped) != 0 {
		t.Fatalf("rep=%+v", rep)
	}
}

func TestRun_InlineContent(t *testing.T) {
	st, _ := openStoreT(t)
	rep, err := Run(context.Background(), st, t.TempDir(), nil, Request{
		Content: "# Title\nbody text\n", Title: "note1", Path: "note1.md",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Indexed != 1 || rep.BytesStored == 0 {
		t.Fatalf("rep=%+v", rep)
	}
}

// TestRunWeb_SnippetRedacted: 최종리뷰 C1(수렴 Critical) — WebReport.Snippet은 저장본
// (redacted) 기준이어야 한다. mcp 핸들러가 netfetch 원문이 아닌 이 값만 노출하므로, 여기서
// 새지 않아야 호출부의 redaction 우회가 근본적으로 막힌다.
func TestRunWeb_SnippetRedacted(t *testing.T) {
	st, _ := openStoreT(t)
	// 런타임 분할 리터럴 — 소스에 연속 secret 토큰 금지(규약 §8).
	canary := "AKIA" + "NOTAREALKEY01234"
	body := []byte("hello world token=" + canary + " end of body\n")

	rep, err := RunWeb(context.Background(), st, "http://example.invalid/", nil, body, "text/plain", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rep.Snippet, canary) {
		t.Fatalf("Snippet=%q leaks secret canary", rep.Snippet)
	}
	if len(rep.Snippet) == 0 || len(rep.Snippet) > 1024 {
		t.Fatalf("bad snippet length=%d", len(rep.Snippet))
	}
}

// TestRunWeb_TitleFillsEmptyChunkTitle: title(netfetch.Result.Title 상당)이 전달되면 헤딩을
// 못 찾아 Title이 빈 청크의 기본값으로 반영돼야 한다(계획2 §4 이월 (2)).
func TestRunWeb_TitleFillsEmptyChunkTitle(t *testing.T) {
	st, _ := openStoreT(t)
	body := []byte("plain body text with no markdown heading\n")

	rep, err := RunWeb(context.Background(), st, "http://example.invalid/", nil, body, "text/plain", "", "Example Page Title")
	if err != nil {
		t.Fatal(err)
	}

	var title sql.NullString
	if err := st.Reader().QueryRow(`SELECT title FROM chunks WHERE artifact_id=? ORDER BY ordinal LIMIT 1`, rep.ArtifactID).Scan(&title); err != nil {
		t.Fatalf("query chunk title: %v", err)
	}
	if !title.Valid || title.String != "Example Page Title" {
		t.Fatalf("chunk title = %+v, want %q", title, "Example Page Title")
	}
}
