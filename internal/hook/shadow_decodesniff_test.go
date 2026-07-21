package hook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wotjr1649/context-router/internal/session"
)

// TestShadow_DecodeSniff — D31: tool_response의 문자열 leaf를 재귀 디코드해 실 NUL·비텍스트만
// 거부한다. (a) NUL 이스케이프 시퀀스를 텍스트로 "논하는" 정상 콘텐츠(C2 FP)는 이제 저장된다
// (유일한 동작 변화 — 현행 부분문자열 게이트는 이를 오거부). (b) stdout leaf가 디코드 후 실 NUL이
// 되는 응답·(d) 배열 leaf 속 NUL은 전후 모두 미저장(컨트롤 — 회귀 방지). (c) 정상 객체는 저장.
// (e) float64 초과 수(1e1000)를 담은 유효 JSON은 UseNumber 디코드로 저장된다 — 수 leaf는
// json.Number(string 계열 별개 타입)라 stringLeaves가 무시(Unmarshal은 float64 오버플로로 오거부).
// 니들의 이스케이프 시퀀스는 소스에 그대로 두지 않고 반드시 런타임 조립한다 — 편집 도구의 유니코드
// 디코드 함정으로 실 제어 바이트가 박히는 사고 방지(단일 백슬래시=디코드 시 실 NUL, 이중=리터럴 텍스트).
func TestShadow_DecodeSniff(t *testing.T) {
	esc := "\\" + "u0000"               // 단일 백슬래시 — JSON 디코드 시 leaf에 실 NUL 생성
	escLit := "\\" + esc                // 이중 백슬래시 — 디코드 후 리터럴 텍스트(실 NUL 아님)
	pad := strings.Repeat("pad ", 5000) // 20000B — Shadow MIN(16KiB) 초과 보장
	cases := []struct {
		name  string
		body  string
		store bool
	}{
		{"fp-prose", `"discussing the ` + escLit + ` escape in prose ` + pad + `"`, true},
		{"stdout-nul", `{"stdout":"abc` + esc + `def` + pad + `","stderr":""}`, false},
		{"ok-object", `{"stdout":"` + pad + `","stderr":""}`, true},
		{"array-nul", `["ok` + pad + `","x` + esc + `y"]`, false},
		{"bignum", `{"stdout":"` + pad + `","n":1e1000}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, contentDir, sdir := shadowSetup(t)
			ad, err := session.OpenAppend(context.Background(), sdir, session.AppendOptions{
				ExternalSessionID: "cc:3f2504e0-4f89-41d3-9a0c-0305e82c3301",
				Producer:          "context-router/test",
			})
			if err != nil {
				t.Fatalf("OpenAppend: %v", err)
			}
			defer func() { _ = ad.Close() }()
			in := hookInput{HookEventName: "PostToolUse", ToolName: "Bash", ToolResponse: json.RawMessage(tc.body)}
			shadowCapture(context.Background(), ad, in, sdir, contentDir, "cc:3f2504e0-4f89-41d3-9a0c-0305e82c3301", func(string) string { return "" })
			n := contentArtifacts(t, contentDir)
			switch {
			case tc.store && n != 1:
				t.Fatalf("artifacts=%d want 1(저장 기대)", n)
			case !tc.store && n != -1:
				t.Fatalf("artifacts=%d want -1(미저장 기대)", n)
			}
		})
	}
}
