package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wotjr1649/context-router/internal/exec"
)

// listToolNames: cs의 tools/list 이름 집합(exec 게이팅 검증 공용).
func listToolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	lt, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := make([]string, len(lt.Tools))
	for i, tl := range lt.Tools {
		names[i] = tl.Name
	}
	return names
}

// TestValidateExecPath: 경로 검증 분기 — 비절대·미존재·디렉터리는 ErrInvalidPath, 정규 파일만
// 입력 절대경로를 그대로 반환한다. 디렉터리 케이스가 IsRegular 배제를 실증한다(Windows 로컬은
// FIFO·소켓 등 비정규 파일을 이식성 있게 만들 수 없어 디렉터리로 충분).
func TestValidateExecPath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "snippet.go")
	if err := os.WriteFile(file, []byte("package x\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// 비절대: 존재하는 상대 경로여도 거부.
	if _, err := validateExecPath("rel/path.go"); !errors.Is(err, exec.ErrInvalidPath) {
		t.Errorf("비절대: err=%v want ErrInvalidPath", err)
	}
	// 미존재: 절대이지만 없는 경로.
	if _, err := validateExecPath(filepath.Join(dir, "missing.go")); !errors.Is(err, exec.ErrInvalidPath) {
		t.Errorf("미존재: err=%v want ErrInvalidPath", err)
	}
	// 디렉터리: 절대·존재하지만 정규 파일 아님(IsRegular 배제).
	if _, err := validateExecPath(dir); !errors.Is(err, exec.ErrInvalidPath) {
		t.Errorf("디렉터리: err=%v want ErrInvalidPath", err)
	}
	// 정규 파일: 통과, 입력 절대경로 그대로 반환.
	got, err := validateExecPath(file)
	if err != nil {
		t.Fatalf("정규 파일: unexpected err=%v", err)
	}
	if got != file {
		t.Errorf("정규 파일: got=%q want %q", got, file)
	}
}

// TestExecuteNotRegisteredWithoutProfile: exec 프로필 미활성 시 두 도구 모두 미등록(삼중
// 게이트 중 서버측 프로필 게이트 — Enable에 "exec"가 없다).
func TestExecuteNotRegisteredWithoutProfile(t *testing.T) {
	cs, _ := newTestServer(t, nil)
	for _, n := range listToolNames(t, cs) {
		if n == "ctr_execute" || n == "ctr_execute_file" {
			t.Fatalf("exec 프로필 없이 %s 등록됨", n)
		}
	}
}

// TestExecuteRegisteredAndRuns: exec 프로필+프로브 게이트 통과 시 ctr_execute가 등록되고
// shell 러너로 왕복 실행돼 stdout을 반환한다. 프로브가 실패하는 플랫폼(도구 미등록)이나
// shell toolchain 부재 환경에서는 검증 대상이 없어 skip한다(브리프).
func TestExecuteRegisteredAndRuns(t *testing.T) {
	cs, _ := newTestServer(t, []string{"exec"})
	if !slices.Contains(listToolNames(t, cs), "ctr_execute") {
		t.Skip("exec 격리 프로브 실패로 ctr_execute 미등록 — 이 플랫폼에서 검증 대상 없음")
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "ctr_execute", Arguments: ExecuteInput{Language: "shell", Code: "echo ok"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		if text := res.Content[0].(*mcp.TextContent).Text; strings.HasPrefix(text, "["+codeUnsupportedFile+"]") {
			t.Skip("shell 러너 미설치 — 검증 대상 없음: " + text)
		}
		t.Fatalf("ctr_execute error: %+v", res.Content)
	}
	var out ExecuteOutput
	remarshal(t, res.StructuredContent, &out)
	if !strings.Contains(out.Stdout, "ok") {
		t.Fatalf("stdout=%q want to contain \"ok\"", out.Stdout)
	}
}
