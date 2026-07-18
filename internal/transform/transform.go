// Package transform — starlark 엔진·worker 프로토콜·OS 상한. 설계서 §4.3.
package transform

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// Caps: 실행 상한. 0이면 기본값(defaultMaxSteps/defaultMaxOutputBytes) 사용.
type Caps struct {
	MaxSteps       int64
	MaxOutputBytes int
}

// Input: 스크립트에 전달되는 원본 텍스트 1건.
type Input struct {
	ID   int64
	Text string
}

// Request: Eval 1회 호출의 전체 입력.
type Request struct {
	Script string
	Inputs []Input
	Args   map[string]string
	Caps   Caps
}

// Result: Eval의 순수 출력. ErrKind: ""|"budget"|"output_limit"|"script".
type Result struct {
	Output    string
	StepsUsed int64
	Truncated bool
	ErrKind   string
}

// ErrBudget/ErrOutputLimit: T3 toToolError 매핑용 sentinel. Eval 자체는 ErrKind로 표현한다.
var (
	ErrBudget      = errors.New("transform: step budget exceeded")
	ErrOutputLimit = errors.New("transform: output limit exceeded")
)

const (
	defaultMaxSteps       = 5_000_000
	defaultMaxOutputBytes = 32768
)

// Eval: 순수 함수 — 파일·네트워크·env·시계·난수 접근 없음. panic 없이 항상 Result를 반환한다.
func Eval(req Request) (result Result) {
	defer func() {
		if r := recover(); r != nil {
			result = Result{ErrKind: "script"}
		}
	}()

	maxSteps := req.Caps.MaxSteps
	if maxSteps <= 0 {
		maxSteps = defaultMaxSteps
	}
	maxOutputBytes := req.Caps.MaxOutputBytes
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultMaxOutputBytes
	}

	var out strings.Builder
	var budgetHit, outputHit bool

	thread := &starlark.Thread{}
	thread.OnMaxSteps = func(th *starlark.Thread) {
		budgetHit = true
		th.Cancel("transform: step budget exceeded")
	}
	thread.SetMaxExecutionSteps(uint64(maxSteps))

	emit := starlark.NewBuiltin("emit", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var v starlark.Value
		if err := starlark.UnpackPositionalArgs("emit", args, kwargs, 1, &v); err != nil {
			return nil, err
		}
		s := displayString(v)
		if out.Len()+len(s) > maxOutputBytes {
			outputHit = true
			return nil, ErrOutputLimit
		}
		out.WriteString(s)
		return starlark.None, nil
	})

	predeclared := starlark.StringDict{
		"inputs":        buildInputs(req.Inputs),
		"args":          buildArgs(req.Args),
		"emit":          emit,
		"regex_extract": starlark.NewBuiltin("regex_extract", biRegexExtract),
		"json_project":  starlark.NewBuiltin("json_project", biJSONProject),
		"line_window":   starlark.NewBuiltin("line_window", biLineWindow),
		"head":          starlark.NewBuiltin("head", biHead),
		"tail":          starlark.NewBuiltin("tail", biTail),
		"count":         starlark.NewBuiltin("count", biCount),
		"sort":          starlark.NewBuiltin("sort", biSort),
		"dedupe":        starlark.NewBuiltin("dedupe", biDedupe),
	}

	_, err := starlark.ExecFile(thread, "script.star", req.Script, predeclared)

	res := Result{
		Output:    out.String(),
		StepsUsed: int64(thread.ExecutionSteps()),
	}
	switch {
	case err == nil:
	case budgetHit:
		res.ErrKind = "budget"
	case outputHit:
		res.ErrKind = "output_limit"
		res.Truncated = true
	default:
		res.ErrKind = "script"
	}
	return res
}

// displayString: emit()의 str() 대응 — 문자열은 따옴표 없이, 그 외는 starlark 기본 표현.
func displayString(v starlark.Value) string {
	if s, ok := v.(starlark.String); ok {
		return string(s)
	}
	return v.String()
}

func buildInputs(inputs []Input) *starlark.List {
	elems := make([]starlark.Value, len(inputs))
	for i, in := range inputs {
		elems[i] = &inputValue{id: in.ID, text: in.Text}
	}
	return starlark.NewList(elems)
}

// buildArgs: map 순회 비의존 — 키 정렬 후 삽입(결정론).
func buildArgs(m map[string]string) *starlark.Dict {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	d := starlark.NewDict(len(keys))
	for _, k := range keys {
		_ = d.SetKey(starlark.String(k), starlark.String(m[k]))
	}
	return d
}

// inputValue: inputs[i] 원소 — .text()/.lines()/.json() 메서드 보유.
type inputValue struct {
	id   int64
	text string
}

var _ starlark.HasAttrs = (*inputValue)(nil)

func (v *inputValue) String() string       { return fmt.Sprintf("<input %d>", v.id) }
func (v *inputValue) Type() string         { return "input" }
func (v *inputValue) Freeze()              {}
func (v *inputValue) Truth() starlark.Bool { return starlark.Bool(v.text != "") }
func (v *inputValue) Hash() (uint32, error) {
	return 0, fmt.Errorf("unhashable type: input")
}

func (v *inputValue) Attr(name string) (starlark.Value, error) {
	switch name {
	case "text":
		return starlark.NewBuiltin("text", func(_ *starlark.Thread, _ *starlark.Builtin, _ starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
			return starlark.String(v.text), nil
		}), nil
	case "lines":
		return starlark.NewBuiltin("lines", func(_ *starlark.Thread, _ *starlark.Builtin, _ starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
			return linesList(v.text), nil
		}), nil
	case "json":
		return starlark.NewBuiltin("json", func(_ *starlark.Thread, _ *starlark.Builtin, _ starlark.Tuple, _ []starlark.Tuple) (starlark.Value, error) {
			return jsonToStarlark(v.text)
		}), nil
	}
	return nil, nil
}

func (v *inputValue) AttrNames() []string { return []string{"text", "lines", "json"} }

func splitLines(text string) []string {
	if text == "" {
		return []string{}
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

func linesList(text string) *starlark.List {
	parts := splitLines(text)
	elems := make([]starlark.Value, len(parts))
	for i, p := range parts {
		elems[i] = starlark.String(p)
	}
	return starlark.NewList(elems)
}

func jsonToStarlark(text string) (starlark.Value, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}
	return goToStarlark(v), nil
}

// goToStarlark: encoding/json 값 → starlark 값. 객체 키는 정렬 삽입(결정론).
func goToStarlark(v any) starlark.Value {
	switch x := v.(type) {
	case nil:
		return starlark.None
	case bool:
		return starlark.Bool(x)
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return starlark.MakeInt64(i)
		}
		f, _ := x.Float64()
		return starlark.Float(f)
	case string:
		return starlark.String(x)
	case []any:
		elems := make([]starlark.Value, len(x))
		for i, e := range x {
			elems[i] = goToStarlark(e)
		}
		return starlark.NewList(elems)
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		d := starlark.NewDict(len(keys))
		for _, k := range keys {
			_ = d.SetKey(starlark.String(k), goToStarlark(x[k]))
		}
		return d
	default:
		return starlark.None
	}
}

// jsonProject: 점 표기 경로("a.b.0.c") 탐색. 경로 실패 시 None(스크립트 오류로 만들지 않음).
func jsonProject(v starlark.Value, path string) starlark.Value {
	cur := v
	if path == "" {
		return cur
	}
	for _, seg := range strings.Split(path, ".") {
		switch c := cur.(type) {
		case *starlark.Dict:
			val, found, err := c.Get(starlark.String(seg))
			if err != nil || !found {
				return starlark.None
			}
			cur = val
		case *starlark.List:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= c.Len() {
				return starlark.None
			}
			cur = c.Index(idx)
		default:
			return starlark.None
		}
	}
	return cur
}

func toSlice(v starlark.Value) ([]starlark.Value, error) {
	iterable, ok := v.(starlark.Iterable)
	if !ok {
		return nil, fmt.Errorf("expected iterable, got %s", v.Type())
	}
	var out []starlark.Value
	for e := range starlark.Elements(iterable) {
		out = append(out, e)
	}
	return out, nil
}

// sortSeq: 안정 정렬(starlark.Compare 실패 시 String() 폴백 — 결코 panic하지 않음).
func sortSeq(elems []starlark.Value) *starlark.List {
	out := append([]starlark.Value(nil), elems...)
	sort.SliceStable(out, func(i, j int) bool {
		lt, err := starlark.Compare(syntax.LT, out[i], out[j])
		if err != nil {
			return out[i].String() < out[j].String()
		}
		return lt
	})
	return starlark.NewList(out)
}

func dedupeSeq(elems []starlark.Value) *starlark.List {
	seen := make(map[string]bool, len(elems))
	out := make([]starlark.Value, 0, len(elems))
	for _, v := range elems {
		key := v.Type() + ":" + v.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, v)
	}
	return starlark.NewList(out)
}

func biRegexExtract(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var pattern, text string
	if err := starlark.UnpackPositionalArgs("regex_extract", args, kwargs, 2, &pattern, &text); err != nil {
		return nil, err
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("regex_extract: %w", err)
	}
	matches := re.FindAllString(text, -1)
	elems := make([]starlark.Value, len(matches))
	for i, m := range matches {
		elems[i] = starlark.String(m)
	}
	return starlark.NewList(elems), nil
}

func biJSONProject(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var value starlark.Value
	var path string
	if err := starlark.UnpackPositionalArgs("json_project", args, kwargs, 2, &value, &path); err != nil {
		return nil, err
	}
	return jsonProject(value, path), nil
}

func biLineWindow(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var lines starlark.Value
	var start, end int
	if err := starlark.UnpackPositionalArgs("line_window", args, kwargs, 3, &lines, &start, &end); err != nil {
		return nil, err
	}
	elems, err := toSlice(lines)
	if err != nil {
		return nil, err
	}
	n := len(elems)
	if start < 1 {
		start = 1
	}
	if end > n {
		end = n
	}
	if start > end {
		return starlark.NewList(nil), nil
	}
	return starlark.NewList(append([]starlark.Value(nil), elems[start-1:end]...)), nil
}

func biHead(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var seq starlark.Value
	var n int
	if err := starlark.UnpackPositionalArgs("head", args, kwargs, 2, &seq, &n); err != nil {
		return nil, err
	}
	elems, err := toSlice(seq)
	if err != nil {
		return nil, err
	}
	if n < 0 {
		n = 0
	}
	if n > len(elems) {
		n = len(elems)
	}
	return starlark.NewList(append([]starlark.Value(nil), elems[:n]...)), nil
}

func biTail(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var seq starlark.Value
	var n int
	if err := starlark.UnpackPositionalArgs("tail", args, kwargs, 2, &seq, &n); err != nil {
		return nil, err
	}
	elems, err := toSlice(seq)
	if err != nil {
		return nil, err
	}
	if n < 0 {
		n = 0
	}
	if n > len(elems) {
		n = len(elems)
	}
	return starlark.NewList(append([]starlark.Value(nil), elems[len(elems)-n:]...)), nil
}

func biCount(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var seq starlark.Value
	if err := starlark.UnpackPositionalArgs("count", args, kwargs, 1, &seq); err != nil {
		return nil, err
	}
	n := starlark.Len(seq)
	if n < 0 {
		return nil, fmt.Errorf("count: unsupported type %s", seq.Type())
	}
	return starlark.MakeInt(n), nil
}

func biSort(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var seq starlark.Value
	if err := starlark.UnpackPositionalArgs("sort", args, kwargs, 1, &seq); err != nil {
		return nil, err
	}
	elems, err := toSlice(seq)
	if err != nil {
		return nil, err
	}
	return sortSeq(elems), nil
}

func biDedupe(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var seq starlark.Value
	if err := starlark.UnpackPositionalArgs("dedupe", args, kwargs, 1, &seq); err != nil {
		return nil, err
	}
	elems, err := toSlice(seq)
	if err != nil {
		return nil, err
	}
	return dedupeSeq(elems), nil
}
