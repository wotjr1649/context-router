package transform

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestBuiltins(t *testing.T) {
	cases := []struct {
		name   string
		script string
		inputs []Input
		want   string
	}{
		{
			name:   "regex_extract",
			script: `emit(",".join(regex_extract("[0-9]+", inputs[0].text())))`,
			inputs: []Input{{ID: 1, Text: "a1 b22 c333"}},
			want:   "1,22,333",
		},
		{
			name:   "json_project",
			script: `emit(json_project(inputs[0].json(), "a.b.1"))`,
			inputs: []Input{{ID: 1, Text: `{"a":{"b":[10,20,30]}}`}},
			want:   "20",
		},
		{
			name:   "line_window",
			script: `emit(",".join(line_window(inputs[0].lines(), 2, 4)))`,
			inputs: []Input{{ID: 1, Text: "l1\nl2\nl3\nl4\nl5"}},
			want:   "l2,l3,l4",
		},
		{
			name:   "head",
			script: `emit(",".join(head(inputs[0].lines(), 2)))`,
			inputs: []Input{{ID: 1, Text: "l1\nl2\nl3\nl4\nl5"}},
			want:   "l1,l2",
		},
		{
			name:   "tail",
			script: `emit(",".join(tail(inputs[0].lines(), 2)))`,
			inputs: []Input{{ID: 1, Text: "l1\nl2\nl3\nl4\nl5"}},
			want:   "l4,l5",
		},
		{
			name:   "count",
			script: `emit(count(inputs[0].lines()))`,
			inputs: []Input{{ID: 1, Text: "l1\nl2\nl3\nl4\nl5"}},
			want:   "5",
		},
		{
			name:   "sort",
			script: `emit(",".join(sort(["banana", "apple", "cherry"])))`,
			want:   "apple,banana,cherry",
		},
		{
			name:   "dedupe",
			script: `emit(",".join(dedupe(["a", "b", "a", "c", "b"])))`,
			want:   "a,b,c",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Eval(Request{Script: c.script, Inputs: c.inputs})
			if r.ErrKind != "" {
				t.Fatalf("ErrKind=%q want \"\" (Output=%q)", r.ErrKind, r.Output)
			}
			if r.Output != c.want {
				t.Fatalf("Output=%q want %q", r.Output, c.want)
			}
		})
	}
}

func TestDeterminism(t *testing.T) {
	req := Request{
		Script: `emit(",".join(args.keys())); emit("|"); emit(",".join(sort(["b", "a", "c"])))`,
		Args:   map[string]string{"z": "1", "a": "2", "m": "3"},
	}
	r1 := Eval(req)
	r2 := Eval(req)
	if r1 != r2 {
		t.Fatalf("nondeterministic: %+v != %+v", r1, r2)
	}
	want := "a,m,z|a,b,c"
	if r1.Output != want {
		t.Fatalf("Output=%q want %q", r1.Output, want)
	}
}

func TestBudgetExceeded(t *testing.T) {
	req := Request{
		Script: "def f():\n\tfor i in range(100000000):\n\t\tpass\n\nf()\n",
		Caps:   Caps{MaxSteps: 1000},
	}
	r := Eval(req)
	if r.ErrKind != "budget" {
		t.Fatalf("ErrKind=%q want budget", r.ErrKind)
	}
	if r.StepsUsed <= 0 {
		t.Fatalf("StepsUsed=%d want >0", r.StepsUsed)
	}
}

func TestOutputLimitExceeded(t *testing.T) {
	req := Request{
		Script: "def f():\n\tfor i in range(1000):\n\t\temit(\"x\")\n\nf()\n",
		Caps:   Caps{MaxOutputBytes: 5},
	}
	r := Eval(req)
	if r.ErrKind != "output_limit" {
		t.Fatalf("ErrKind=%q want output_limit", r.ErrKind)
	}
	if !r.Truncated {
		t.Fatalf("Truncated=false want true")
	}
	if r.Output != "xxxxx" {
		t.Fatalf("Output=%q want %q", r.Output, "xxxxx")
	}
}

func TestForbiddenAccess(t *testing.T) {
	scripts := []string{
		`x = open("f.txt")`,
		`load("mod.star", "x")`,
	}
	for _, s := range scripts {
		r := Eval(Request{Script: s})
		if r.ErrKind != "script" {
			t.Fatalf("script=%q ErrKind=%q want script", s, r.ErrKind)
		}
	}
}

// TestErrSummary_Script: 승계 계약 T1→T2 (a) — script 오류는 ErrSummary가 비어있지 않고,
// 스크립트 원문(고유 리터럴 "MARKER_LITERAL_TOKEN")과 입력 데이터를 포함하지 않아야 한다.
func TestErrSummary_Script(t *testing.T) {
	const marker = "MARKER_LITERAL_TOKEN"
	req := Request{
		// marker는 참조되지 않는 문자열 리터럴로 심는다 — "undefined: <식별자명>"처럼 오류에
		// 자연히 등장하는 식별자명(과제 예시 "script error: undefined: foo")과 달리, 이
		// 리터럴은 오류 메시지 어디에도 나타나서는 안 된다(스크립트 원문 비유출 검증).
		Script: `x = "` + marker + `" + undefined_name_zzz`,
		Inputs: []Input{{ID: 1, Text: "secret-input-data"}},
	}
	r := Eval(req)
	if r.ErrKind != "script" {
		t.Fatalf("ErrKind=%q want script", r.ErrKind)
	}
	if r.ErrSummary == "" {
		t.Fatal("ErrSummary empty, want non-empty")
	}
	if strings.Contains(r.ErrSummary, marker) {
		t.Fatalf("ErrSummary=%q leaks script source marker", r.ErrSummary)
	}
	if strings.Contains(r.ErrSummary, "secret-input-data") {
		t.Fatalf("ErrSummary=%q leaks input data", r.ErrSummary)
	}
	if strings.Contains(r.ErrSummary, "\n") {
		t.Fatalf("ErrSummary=%q must be a single line", r.ErrSummary)
	}
}

// TestErrSummary_NoInputLeak_Fail: 리뷰 B1(수렴 Critical) — fail(...)로 입력 데이터를 오류
// 메시지에 주입해도 ErrSummary/Output에 새어 나가지 않아야 한다(MCP 오류 채널이 출력 상한
// 32768B를 우회해 최대 8MB 입력을 노출하는 경로 차단).
func TestErrSummary_NoInputLeak_Fail(t *testing.T) {
	const canary = "CANARYBODY123"
	req := Request{
		Script: `fail("SECRET-INPUT-" + inputs[0].text())`,
		Inputs: []Input{{ID: 1, Text: canary}},
	}
	r := Eval(req)
	if r.ErrKind != "script" {
		t.Fatalf("ErrKind=%q want script", r.ErrKind)
	}
	if strings.Contains(r.ErrSummary, canary) {
		t.Fatalf("ErrSummary=%q leaks input canary", r.ErrSummary)
	}
	if strings.Contains(r.ErrSummary, "SECRET-INPUT") {
		t.Fatalf("ErrSummary=%q leaks script literal", r.ErrSummary)
	}
	if strings.Contains(r.Output, canary) {
		t.Fatalf("Output=%q leaks input canary", r.Output)
	}

	// 8MB 입력 — 상한 우회 없이 ErrSummary가 여전히 짧고 내용을 담지 않아야 한다.
	big := strings.Repeat("A", 8*1024*1024)
	br := Eval(Request{
		Script: `fail("SECRET-INPUT-" + inputs[0].text())`,
		Inputs: []Input{{ID: 1, Text: big}},
	})
	if len(br.ErrSummary) > 200 {
		t.Fatalf("ErrSummary len=%d want <=200 (8MB 입력이 그대로 새면 상한 우회)", len(br.ErrSummary))
	}
	if strings.Contains(br.ErrSummary, "AAAA") {
		t.Fatalf("ErrSummary leaks 8MB input content")
	}
}

// TestErrSummary_BudgetAndOutputLimit: budget/output_limit도 ErrSummary가 채워진다.
func TestErrSummary_BudgetAndOutputLimit(t *testing.T) {
	budget := Eval(Request{
		Script: "def f():\n\tfor i in range(100000000):\n\t\tpass\n\nf()\n",
		Caps:   Caps{MaxSteps: 1000},
	})
	if budget.ErrSummary == "" {
		t.Fatal("budget ErrSummary empty")
	}

	limit := Eval(Request{
		Script: "def f():\n\tfor i in range(1000):\n\t\temit(\"x\")\n\nf()\n",
		Caps:   Caps{MaxOutputBytes: 5},
	})
	if limit.ErrSummary == "" {
		t.Fatal("output_limit ErrSummary empty")
	}
}

// TestRunWorker_RoundTrip: 프로토콜 round-trip — 프로세스 스폰 없이 파이프(bytes.Buffer)로
// RunWorker를 직접 호출해 Request JSON→Result JSON 왕복이 Eval과 일치하는지 검증한다.
func TestRunWorker_RoundTrip(t *testing.T) {
	req := Request{
		Script: `emit(",".join(sort(["b", "a", "c"])))`,
	}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	if err := RunWorker(bytes.NewReader(reqBytes), &stdout); err != nil {
		t.Fatalf("RunWorker error: %v", err)
	}

	var got Result
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("Result JSON 파싱 실패: %v (raw=%q)", err, stdout.String())
	}

	want := Eval(req)
	if got != want {
		t.Fatalf("RunWorker Result=%+v want %+v", got, want)
	}
}

// TestRlimitASBytes: linux self-apply(selfApplyMemLimit)가 RLIMIT_AS에 실제로 거는 값 —
// CTR_WORKER_MEM(순수 캡)에 vaHeadroomBytes를 더한 값이어야 한다(D19-b). 상한 절삭(기존
// hard limit이 더 낮으면 그 값 유지)은 selfApplyMemLimit이 이 함수의 반환값에 대해 그대로
// 재사용하므로 여기서는 가산 자체만 검증한다.
func TestRlimitASBytes(t *testing.T) {
	cases := []struct {
		name string
		cap  int64
		want int64
	}{
		{name: "256MB cap", cap: 256 << 20, want: 1024 << 20}, // 256MB + 768MB = 1GB
		{name: "zero cap", cap: 0, want: vaHeadroomBytes},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rlimitASBytes(c.cap); got != c.want {
				t.Fatalf("rlimitASBytes(%d) = %d, want %d", c.cap, got, c.want)
			}
		})
	}
}

func FuzzEval(f *testing.F) {
	f.Add("emit(1)", "hello")
	f.Add("def f():\n\tfor i in range(10):\n\t\temit(i)\n\nf()\n", "")
	f.Add(`open("x")`, "text")
	f.Add(`emit(",".join(sort(inputs[0].lines())))`, "b\na")
	f.Fuzz(func(t *testing.T, script, text string) {
		req := Request{
			Script: script,
			Inputs: []Input{{ID: 1, Text: text}},
			Caps:   Caps{MaxSteps: 20000, MaxOutputBytes: 4096},
		}
		_ = Eval(req)
	})
}
