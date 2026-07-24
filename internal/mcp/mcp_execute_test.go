package mcp

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
