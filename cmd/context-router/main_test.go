package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wotjr1649/context-router/internal/mcp"
)

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    serverFlags
		wantErr bool
	}{
		{"defaults", nil, serverFlags{Profile: []string{"search", "fetch", "transform"}, LogLevel: "info"}, false},
		{"enable", []string{"--enable", "ingest,net"}, serverFlags{Profile: []string{"search", "fetch", "transform"}, Enable: []string{"ingest", "net"}, LogLevel: "info"}, false},
		{"unknown", []string{"--bogus"}, serverFlags{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseFlags(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			if err == nil && strings.Join(got.Profile, ",") != strings.Join(tt.want.Profile, ",") {
				t.Fatalf("profile=%v want %v", got.Profile, tt.want.Profile)
			}
			if err == nil && strings.Join(got.Enable, ",") != strings.Join(tt.want.Enable, ",") {
				t.Fatalf("enable=%v want %v", got.Enable, tt.want.Enable)
			}
		})
	}
}

func TestBanner(t *testing.T) {
	f := serverFlags{Profile: []string{"search", "fetch", "transform"}, LogLevel: "info"}
	got := banner(f, "C:/proj")
	want := "[ctr] v" + version + " profile=search,fetch,transform ingest=off net=off root=C:/proj"
	if got != want {
		t.Fatalf("banner=%q want %q", got, want)
	}
	f2 := serverFlags{Profile: []string{"search"}, Enable: []string{"ingest"}, LogLevel: "info"}
	got2 := banner(f2, "/p")
	want2 := "[ctr] v" + version + " profile=search ingest=on net=off root=/p"
	if got2 != want2 {
		t.Fatalf("banner on-branch=%q want %q", got2, want2)
	}
}

func TestCanonicalizeAllowPaths(t *testing.T) {
	storeRoot := t.TempDir()
	inside := filepath.Join(storeRoot, "projects", "p1")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	outside := t.TempDir()

	if _, err := canonicalizeAllowPaths([]string{inside}, storeRoot); !errors.Is(err, errAllowPathViolation) {
		t.Fatalf("inside store-root: err=%v want errAllowPathViolation", err)
	}
	got, err := canonicalizeAllowPaths([]string{outside}, storeRoot)
	if err != nil {
		t.Fatalf("outside store-root: unexpected err=%v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got=%v want 1 entry", got)
	}
}

func TestCanonicalizeAllowPaths_NonexistentPathErrors(t *testing.T) {
	storeRoot := t.TempDir()
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := canonicalizeAllowPaths([]string{missing}, storeRoot); err == nil {
		t.Fatal("want error for nonexistent allow-path, got nil")
	}
}

func TestStoreRootFor(t *testing.T) {
	t.Run("flag_wins", func(t *testing.T) {
		got, err := storeRootFor(serverFlags{StoreRoot: "C:/custom"})
		if err != nil || got != "C:/custom" {
			t.Fatalf("got=%q err=%v", got, err)
		}
	})
	t.Run("env_wins_over_default", func(t *testing.T) {
		t.Setenv("CTR_STORE_ROOT", "C:/from-env")
		got, err := storeRootFor(serverFlags{})
		if err != nil || got != "C:/from-env" {
			t.Fatalf("got=%q err=%v", got, err)
		}
	})
}

// --- E2E stdio 스모크 (Task 9, 설계 §12-7·10 기초) ---
//
// 손수 프레이밍한 JSON-RPC로 실바이너리와 stdin/stdout 파이프를 주고받는다(SDK
// 클라이언트 미사용 — 프로토콜 오염을 외부 관찰자 시점에서 직접 검증하기 위함).
// go-sdk StdioTransport 실동작(internal/jsonrpc2 wire.go) 확인 결과 Content-Length
// 프레이밍 없이 개행 구분 JSON 한 줄당 메시지 하나.

type wireMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *wireError      `json:"error,omitempty"`
}

type wireError struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

// stdioClient: 최소 JSON-RPC 클라이언트. 고루틴에서도 안전하게 쓰도록 *testing.T를
// 붙잡지 않고 전부 error를 반환한다(Fatal은 테스트 고루틴에서만 호출 가능하므로).
type stdioClient struct {
	stdin  io.WriteCloser
	scan   *bufio.Scanner
	nextID int
}

func newStdioClient(stdin io.WriteCloser, stdout io.Reader) *stdioClient {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	return &stdioClient{stdin: stdin, scan: sc}
}

func (c *stdioClient) writeLine(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = c.stdin.Write(b)
	return err
}

// readLine reads one stdout line and requires it to be valid JSON — the direct
// check for zero protocol pollution on stdout (설계 §5.5).
func (c *stdioClient) readLine() (wireMsg, error) {
	if !c.scan.Scan() {
		if err := c.scan.Err(); err != nil {
			return wireMsg{}, err
		}
		return wireMsg{}, io.ErrUnexpectedEOF
	}
	line := c.scan.Bytes()
	var m wireMsg
	if err := json.Unmarshal(line, &m); err != nil {
		return wireMsg{}, fmt.Errorf("stdout line is not valid JSON (protocol pollution): %q: %w", line, err)
	}
	return m, nil
}

func (c *stdioClient) notify(method string, params any) error {
	return c.writeLine(wireMsg{JSONRPC: "2.0", Method: method, Params: params})
}

// call sends a request and returns the id-matched response, skipping any
// unexpected notifications read in between.
func (c *stdioClient) call(method string, params any) (wireMsg, error) {
	c.nextID++
	id := c.nextID
	if err := c.writeLine(wireMsg{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		return wireMsg{}, err
	}
	for {
		resp, err := c.readLine()
		if err != nil {
			return wireMsg{}, err
		}
		if resp.ID == id {
			if resp.Error != nil {
				return resp, fmt.Errorf("%s: rpc error [%d] %s", method, resp.Error.Code, resp.Error.Message)
			}
			return resp, nil
		}
	}
}

type toolCallResult struct {
	IsError           bool            `json:"isError"`
	StructuredContent json.RawMessage `json:"structuredContent"`
}

// callTool invokes name via tools/call and decodes structuredContent into out
// (out may be nil to skip decoding).
func callTool(c *stdioClient, name string, args, out any) error {
	resp, err := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return err
	}
	var tr toolCallResult
	if err := json.Unmarshal(resp.Result, &tr); err != nil {
		return fmt.Errorf("%s: decode result: %w", name, err)
	}
	if tr.IsError {
		return fmt.Errorf("%s: tool isError=true: %s", name, resp.Result)
	}
	if out != nil {
		if err := json.Unmarshal(tr.StructuredContent, out); err != nil {
			return fmt.Errorf("%s: decode structuredContent: %w", name, err)
		}
	}
	return nil
}

// handshake performs initialize + notifications/initialized.
func handshake(c *stdioClient, clientName string) error {
	if _, err := c.call("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": clientName, "version": "0.0.1"},
	}); err != nil {
		return err
	}
	return c.notify("notifications/initialized", map[string]any{})
}

// buildCtrBinary go build's the real binary into a temp dir (Go build cache
// makes repeat calls across tests cheap).
func buildCtrBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ctr.exe")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// spawnCtr starts bin with args, wiring stdin/stdout into a stdioClient and
// stderr into a buffer. Read the returned buffer only after the process has
// exited (exec.Cmd synchronizes non-pipe Stderr writers with Wait).
func spawnCtr(bin string, args ...string) (*exec.Cmd, *stdioClient, *bytes.Buffer, error) {
	cmd := exec.Command(bin, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	return cmd, newStdioClient(stdin, stdout), &stderrBuf, nil
}

// closeAndWait closes stdin (client-initiated stdio shutdown per MCP spec) and
// waits up to 5s for the process to exit.
func closeAndWait(cmd *exec.Cmd, c *stdioClient) error {
	if err := c.stdin.Close(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		return errors.New("process did not exit within 5s of stdin close")
	}
}

func TestE2E_StdioRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("느린 E2E 스모크 — short 모드 skip")
	}
	bin := buildCtrBinary(t)

	proj := t.TempDir()
	if err := os.WriteFile(filepath.Join(proj, "hello.txt"), []byte("alpha bravo charlie\n"), 0o644); err != nil {
		t.Fatalf("write hello.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(proj, "note.md"), []byte("# Note\nbravo appears here too.\n"), 0o644); err != nil {
		t.Fatalf("write note.md: %v", err)
	}
	storeRoot := t.TempDir()

	cmd, c, stderrBuf, err := spawnCtr(bin, "--root", proj, "--store-root", storeRoot, "--enable", "ingest")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if err := handshake(c, "ctr-e2e-test"); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	listResp, err := c.call("tools/list", nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var lt struct {
		Tools []struct {
			Name        string `json:"name"`
			Annotations struct {
				ReadOnlyHint bool `json:"readOnlyHint"`
			} `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(listResp.Result, &lt); err != nil {
		t.Fatalf("tools/list decode: %v", err)
	}
	gotNames, roHints := map[string]bool{}, map[string]bool{}
	for _, tl := range lt.Tools {
		gotNames[tl.Name] = true
		roHints[tl.Name] = tl.Annotations.ReadOnlyHint
	}
	for _, want := range []string{"ctr_index", "ctr_search", "ctr_fetch"} {
		if !gotNames[want] {
			t.Fatalf("tools/list missing %q: %+v", want, lt.Tools)
		}
	}
	if !roHints["ctr_search"] || !roHints["ctr_fetch"] {
		t.Fatalf("readOnlyHint missing on search/fetch: %+v", lt.Tools)
	}

	var idxOut mcp.IndexOutput
	if err := callTool(c, "ctr_index", mcp.IndexInput{Path: proj}, &idxOut); err != nil {
		t.Fatalf("ctr_index: %v", err)
	}
	if idxOut.Indexed != 2 {
		t.Fatalf("indexed=%d want 2 (skipped=%+v)", idxOut.Indexed, idxOut.Skipped)
	}

	var searchOut mcp.SearchOutput
	if err := callTool(c, "ctr_search", mcp.SearchInput{Queries: []string{"bravo"}}, &searchOut); err != nil {
		t.Fatalf("ctr_search: %v", err)
	}
	if !searchOut.Untrusted || len(searchOut.Results) != 1 || len(searchOut.Results[0].Hits) == 0 {
		t.Fatalf("bad search output: %+v", searchOut)
	}
	hit := searchOut.Results[0].Hits[0]

	var fetchOut mcp.FetchOutput
	fin := mcp.FetchInput{ArtifactID: hit.ArtifactID, LineStart: &hit.LineStart, LineEnd: &hit.LineEnd}
	if err := callTool(c, "ctr_fetch", fin, &fetchOut); err != nil {
		t.Fatalf("ctr_fetch: %v", err)
	}
	if fetchOut.ExactScope != "artifact" || fetchOut.Content == "" {
		t.Fatalf("bad fetch output: %+v", fetchOut)
	}

	if err := closeAndWait(cmd, c); err != nil {
		t.Fatalf("process exit: %v (stderr=%s)", err, stderrBuf.String())
	}
	if !strings.Contains(stderrBuf.String(), "[ctr] v") {
		t.Fatalf("banner missing in stderr: %q", stderrBuf.String())
	}
}

// indexOneFile spawns one process, indexes a single file, and shuts it down.
// Runs inside a goroutine in TestE2E_TwoProcessConcurrentIndex — must never
// call t.Fatal*, only return error (testing.T.FailNow requires the test
// goroutine).
func indexOneFile(bin, proj, storeRoot, name string) error {
	cmd, c, stderrBuf, err := spawnCtr(bin, "--root", proj, "--store-root", storeRoot, "--enable", "ingest")
	if err != nil {
		return fmt.Errorf("%s: spawn: %w", name, err)
	}
	// fail: 실패 시 프로세스를 정리(Wait까지)하고 나서 stderrBuf를 읽는다 — Wait 전
	// 읽기는 exec의 내부 stderr 복사 고루틴과 경합한다.
	fail := func(stage string, err error) error {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("%s: %s: %w (stderr=%s)", name, stage, err, stderrBuf.String())
	}
	if err := handshake(c, "ctr-e2e-mp-"+name); err != nil {
		return fail("handshake", err)
	}
	var idxOut mcp.IndexOutput
	if err := callTool(c, "ctr_index", mcp.IndexInput{Path: filepath.Join(proj, name)}, &idxOut); err != nil {
		return fail("ctr_index", err)
	}
	if idxOut.Indexed != 1 {
		return fail("index-count", fmt.Errorf("indexed=%d want 1 (skipped=%+v)", idxOut.Indexed, idxOut.Skipped))
	}
	if err := closeAndWait(cmd, c); err != nil {
		return fmt.Errorf("%s: process exit: %w (stderr=%s)", name, err, stderrBuf.String())
	}
	return nil
}

func TestE2E_TwoProcessConcurrentIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("느린 다중 프로세스 스모크 — short 모드 skip")
	}
	bin := buildCtrBinary(t)

	proj := t.TempDir()
	files := map[string]string{
		"fileA.txt": "alpha content for process A\n",
		"fileB.txt": "zulu content for process B\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(proj, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	storeRoot := t.TempDir()

	// 워밍업: 스키마 최초 생성(WAL 파일 포함)을 먼저 끝내 둔다 — 이후 두 프로세스는
	// 브리프가 검증 대상으로 명시한 "동시 쓰기"를 테스트한다. 동시 최초 마이그레이션
	// 경합(WAL 파일 최초 생성)은 브리프 self-review상 계획 3 게이트 하네스 범위.
	warmCmd, warmC, warmStderr, err := spawnCtr(bin, "--root", proj, "--store-root", storeRoot, "--enable", "ingest")
	if err != nil {
		t.Fatalf("warmup spawn: %v", err)
	}
	if err := handshake(warmC, "ctr-e2e-warmup"); err != nil {
		t.Fatalf("warmup handshake: %v", err)
	}
	if err := closeAndWait(warmCmd, warmC); err != nil {
		t.Fatalf("warmup exit: %v (stderr=%s)", err, warmStderr.String())
	}

	errs := make(chan error, len(files))
	var wg sync.WaitGroup
	for name := range files {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			errs <- indexOneFile(bin, proj, storeRoot, name)
		}(name)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	// 세 번째(신규) 프로세스로 두 파일 내용이 모두 검색되는지 확인 — sources=2·DB
	// 무결성은 이 정상 동작으로 갈음한다(설계 §12-10 기초, 심층 검증은 계획 3).
	cmd3, c3, stderrBuf3, err := spawnCtr(bin, "--root", proj, "--store-root", storeRoot, "--enable", "ingest")
	if err != nil {
		t.Fatalf("spawn#3: %v", err)
	}
	if err := handshake(c3, "ctr-e2e-verify"); err != nil {
		t.Fatalf("handshake#3: %v", err)
	}
	var searchOut mcp.SearchOutput
	if err := callTool(c3, "ctr_search", mcp.SearchInput{Queries: []string{"alpha", "zulu"}}, &searchOut); err != nil {
		t.Fatalf("ctr_search#3: %v", err)
	}
	if len(searchOut.Results) != 2 {
		t.Fatalf("results=%d want 2: %+v", len(searchOut.Results), searchOut.Results)
	}
	for _, qr := range searchOut.Results {
		if len(qr.Hits) == 0 {
			t.Fatalf("query %q: no hits: %+v", qr.Query, searchOut.Results)
		}
	}
	if err := closeAndWait(cmd3, c3); err != nil {
		t.Fatalf("process#3 exit: %v (stderr=%s)", err, stderrBuf3.String())
	}
}
