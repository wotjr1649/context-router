package transform

import "testing"

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
