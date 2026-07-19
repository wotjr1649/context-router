// Package transform — starlark 엔진·worker 프로토콜·OS 상한. 설계서 §4.3.
package transform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.starlark.net/resolve"
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

// Result: Eval의 순수 출력. ErrKind: ""|"budget"|"output_limit"|"script"|"no_isolation".
// no_isolation은 worker가 OS 메모리 격리를 자기 자신에게 적용하지 못해(unix Setrlimit 실패)
// Eval을 실행하지 않고 거부했다는 신호다(Spawn이 ErrNoIsolation으로 변환, 리뷰 B2).
type Result struct {
	Output    string
	StepsUsed int64
	Truncated bool
	ErrKind   string
	// ErrSummary: ErrKind!=""일 때 원인 첫 줄 요약(위치/종류만). 스크립트 원문·입력 데이터는
	// 포함하지 않는다(승계 계약 T1→T2 (a)).
	ErrSummary string
}

// ErrBudget/ErrOutputLimit: T3 toToolError 매핑용 sentinel. Eval 자체는 ErrKind로 표현한다.
var (
	ErrBudget      = errors.New("transform: step budget exceeded")
	ErrOutputLimit = errors.New("transform: output limit exceeded")

	// ErrNoIsolation: OS 메모리 상한을 적용할 수 없는 환경 — transform 도구 비활성 신호
	// (in-process fallback 금지, 설계 §4.3/§5.3).
	ErrNoIsolation = errors.New("transform: OS memory isolation unavailable")
)

const (
	defaultMaxSteps       = 5_000_000
	defaultMaxOutputBytes = 32768
	defaultMemLimitBytes  = 256 * 1024 * 1024 // 설계 §4.3 기본 상한 256MB
	defaultWorkerTimeout  = 10 * time.Second  // 리뷰 Important: ctx에 deadline 없을 때 안전망
	gomemlimitRatio       = 0.8               // D19-a: Job/RLIMIT 캡의 80%에서 GC 선제 발동
)

// gomemlimitBytes: 자식 worker의 GOMEMLIMIT 값(바이트) — Job Object/RLIMIT 캡(commit·가상
// 주소공간 기준, defaultMemLimitBytes)의 80%. windows 사망 원인은 GC의 commit 반환 지연 —
// GOMEMLIMIT이 이 소프트 한도에서 GC를 선제 발동시켜 commit이 Job 캡(256MB)에 도달하기 전에
// 회수시킨다. Go 공식 가이드는 5-10% 여유를 권장하나 Job은 RSS가 아닌 commit을 재므로 보수적
// 80%(설계 §1.3 D19-a, 최종 수치는 실측으로 확정). 어디까지나 GC용 soft limit — Job
// Object/RLIMIT 하드 백스톱은 그대로 유지된다(제거하지 않는다).
func gomemlimitBytes() int64 {
	limitBytes := float64(defaultMemLimitBytes) // 변수 경유: 상수식 그대로면 214748364.8이
	return int64(limitBytes * gomemlimitRatio)  // 정확한 int64로 안 떨어져 컴파일타임 변환 불가
}

// Eval: 순수 함수 — 파일·네트워크·env·시계·난수 접근 없음. panic 없이 항상 Result를 반환한다.
func Eval(req Request) (result Result) {
	defer func() {
		if r := recover(); r != nil {
			// 고정 문구만 — panic 값(any)은 예측 불가해 내용을 절대 신뢰하지 않는다(B1).
			result = Result{ErrKind: "script", ErrSummary: "script error (panic)"}
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

	// ExecFile은 deprecated(SA1019, legacy 전역변수 의존) — ExecFile 자체가 내부적으로
	// syntax.LegacyFileOptions()를 넘겨 ExecFileOptions를 호출할 뿐이라(go.starlark.net
	// eval.go), 그걸 그대로 인라인해 동작은 완전히 동일하게 유지하면서 경고만 없앤다.
	_, err := starlark.ExecFileOptions(syntax.LegacyFileOptions(), thread, "script.star", req.Script, predeclared)

	res := Result{
		Output:    out.String(),
		StepsUsed: int64(thread.ExecutionSteps()),
	}
	switch {
	case err == nil:
	case budgetHit:
		res.ErrKind = "budget"
		res.ErrSummary = ErrBudget.Error()
	case outputHit:
		res.ErrKind = "output_limit"
		res.Truncated = true
		res.ErrSummary = ErrOutputLimit.Error()
	default:
		res.ErrKind = "script"
		res.ErrSummary = scriptErrSummary(err)
	}
	return res
}

// scriptErrSummary: ErrKind="script" 오류의 안전 요약 — 고정 카테고리 문구 + 가능하면 스크립트
// 내 줄 번호만. err.Error()/EvalError.Msg는 사용자 스크립트의 fail(...)이 주입한 임의 문자열
// (입력 데이터 포함 가능)을 그대로 담고 있으므로 절대 사용하지 않는다(리뷰 B1 — MCP 오류
// 채널로 출력 상한을 우회한 입력 유출 차단).
func scriptErrSummary(err error) string {
	switch e := err.(type) {
	case *starlark.EvalError:
		if len(e.CallStack) > 0 {
			return fmt.Sprintf("script error (eval) at line %d", e.CallStack.At(0).Pos.Line)
		}
		return "script error (eval)"
	case resolve.ErrorList:
		if len(e) > 0 {
			return fmt.Sprintf("script error (resolve) at line %d", e[0].Pos.Line)
		}
		return "script error (resolve)"
	case syntax.Error:
		return fmt.Sprintf("script error (syntax) at line %d", e.Pos.Line)
	default:
		return "script error"
	}
}

// workerSem: worker 프로세스 동시 실행 ≤2 (설계 §4.3). 패키지 레벨 — 이 프로세스 내 모든
// Spawn 호출이 공유하는 세마포어.
var workerSem = make(chan struct{}, 2)

// RunWorker: worker 프로세스(자기 재실행, "__transform-worker") 측 entrypoint. stdin에서
// Request JSON 1건을 읽어 Eval을 실행하고 Result JSON 1건을 stdout에 쓴다. 호출자(main)는
// 이 함수 전후로 배너·로그를 stdout에 출력해서는 안 된다(stdout은 JSON 1건이어야 한다).
func RunWorker(r io.Reader, w io.Writer) error {
	// unix: CTR_WORKER_MEM 있으면 self Setrlimit. windows: no-op(부모가 Job으로 이미 제한).
	// 실패하면 무제한 상태로 스크립트를 실행할 수 없으므로(in-process fallback 금지, §4.3/§5.3)
	// Eval을 건너뛰고 신호용 Result만 stdout에 쓴다 — exit는 0으로 유지해 Spawn이 stdout JSON을
	// 정상 파싱해서 ErrNoIsolation으로 변환할 수 있게 한다(비정상 exit는 Spawn에서 "worker
	// killed"로 뭉개져 신호가 유실된다).
	if err := selfApplyMemLimit(); err != nil {
		return json.NewEncoder(w).Encode(Result{ErrKind: "no_isolation"})
	}
	var req Request
	if err := json.NewDecoder(r).Decode(&req); err != nil {
		return fmt.Errorf("transform: request 디코딩: %w", err)
	}
	res := Eval(req)
	if err := json.NewEncoder(w).Encode(res); err != nil {
		return fmt.Errorf("transform: result 인코딩: %w", err)
	}
	return nil
}

// Spawn: selfExe(자기 바이너리 경로)를 "__transform-worker" 인자로 재실행해 req를 격리
// 평가한다. 동시 실행 ≤2(workerSem), OS 메모리 상한(applyMemLimit, 부모측 실패 시
// ErrNoIsolation; 자식측 self-apply 실패는 Result.ErrKind="no_isolation"으로 보고되어 아래서
// 동일하게 ErrNoIsolation으로 변환), ctx 취소/timeout 시 트리킬(applyMemLimit이 설정하는
// cmd.Cancel). worker가 상한·timeout으로
// 죽어도(exit code 비정상·부분 출력) 이 함수는 error를 반환하지 않고 합성 Result를 반환한다
// — 부모 프로세스는 절대 죽지 않는다. error 반환은 "실행 자체를 시작 못함" 케이스뿐이다:
// ctx가 세마포어 대기 중 취소, Request 인코딩 실패, ErrNoIsolation.
func Spawn(ctx context.Context, selfExe string, req Request) (Result, error) {
	// 안전망: 호출자 ctx에 deadline이 없으면 CPU-only 무한루프가 메모리 상한보다 먼저 죽도록
	// 10s를 씌운다. deadline이 이미 있으면 그대로 존중(더 길어도 줄이지 않음).
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultWorkerTimeout)
		defer cancel()
	}

	select {
	case workerSem <- struct{}{}:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	defer func() { <-workerSem }()

	payload, err := json.Marshal(req)
	if err != nil {
		return Result{}, fmt.Errorf("transform: request 인코딩: %w", err)
	}

	cmd := exec.CommandContext(ctx, selfExe, "__transform-worker")
	cmd.Stdin = bytes.NewReader(payload)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.WaitDelay = 2 * time.Second

	cleanup, err := applyMemLimit(cmd, defaultMemLimitBytes)
	if err != nil {
		// 원인 보존: sentinel(ErrNoIsolation)로 errors.Is 판정은 유지하면서 applyMemLimit의
		// 실제 실패 사유(OS API 오류 등)를 메시지에 남긴다 — 이전엔 err을 버려 슬로그로도
		// 진단이 안 됐다.
		return Result{}, fmt.Errorf("%w: %v", ErrNoIsolation, err)
	}
	defer cleanup()

	waitErr := cmd.Wait()
	var res Result
	if waitErr != nil {
		// 사유 구분(D19-c): 호출자가 취소한 것인지, ctx deadline을 넘긴 것인지, 그 외
		// 비정상 종료(OS 메모리 상한 kill·Go 런타임 OOM abort 등)인지를 접미사로만 구분한다
		// — "worker killed" 접두사는 main_test.go:1026,1077·mcp_test.go의 부분 문자열
		// 단언 전제이므로 불변.
		reason := "memory or crash"
		switch ctx.Err() {
		case context.Canceled:
			reason = "cancelled"
		case context.DeadlineExceeded:
			reason = "time limit"
		}
		return Result{ErrKind: "script", ErrSummary: "worker killed (" + reason + ")"}, nil
	}
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		// ctx는 정상 종료(취소도 timeout도 아님)인데 stdout이 깨진 경우 — kill 사유가 아니라
		// 출력 파싱 실패이므로 별도 접미사로 구분한다.
		return Result{ErrKind: "script", ErrSummary: "worker killed (bad output)"}, nil
	}
	if res.ErrKind == "no_isolation" {
		// worker가 자기 격리 적용에 실패해 Eval을 거부했다(리뷰 B2) — 격리 실패는 in-process
		// fallback 없이 도구 비활성화로 이어져야 하므로 ErrNoIsolation으로 변환한다.
		return Result{}, ErrNoIsolation
	}
	return res, nil
}

// ProbeIsolation: OS 메모리 격리(applyMemLimit)가 이 환경에서 실제로 동작하는지 최소 비용
// (빈 스크립트 1회 Spawn)으로 확인한다. mcp.NewServer가 ctr_transform 등록 여부를 정하는 데
// 쓴다 — 실패 시 도구 자체를 미등록해야 한다(in-process fallback 금지, 설계 §4.3/§5.3).
// Spawn의 error 반환은 정확히 "실행을 시작조차 못함"(ctx 취소·인코딩 실패·ErrNoIsolation)
// 케이스뿐이므로 그대로 반환해 호출자가 원인을 구분할 수 있게 한다.
func ProbeIsolation(selfExe string) error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultWorkerTimeout)
	defer cancel()
	_, err := Spawn(ctx, selfExe, Request{Script: ""})
	return err
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
