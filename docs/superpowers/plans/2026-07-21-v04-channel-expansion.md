# context-router v0.4 (채널 확장) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Codex 훅 `cx:` 캐프처(D35) + PowerShell 도구 가드(D36) + command shadow denylist 보강(D39) + provenance 티어(D37) + store 용량 경고(D38)를 구현한다 — `docs/context-router-design-v0.4-ko.md`(D34~D39, §11.1 관측 프리체크 채택 포함)가 스펙이다.

**Architecture:** 기존 훅 파이프라인(hook.Run → dispatch → guard/classify/shadow)에 (a) 호스트 경계 인자(cc/cx)를 신설하고, (b) PreToolUse 가드에 PowerShell 형제(guardPowerShell·psDumpArg·psAbsPath)를 합류시키고, (c) shadowCapture에 command 정적 증명 denylist 대조를 추가한다. 설치는 Claude settings.json 병합기와 형제인 Codex hooks.json JSON 병합기(D28 원칙 승계 — §11.1 G3에 따라 TOML 아님). 검색·조회 표시는 hitQuery/sourceOf 두 SQL의 ORDER BY를 kind-티어 우선식으로 동시 교체, doctor는 [14] 행 뒤 조건부 경고 1줄.

**Tech Stack:** Go 1.26.5, modernc.org/sqlite(기존), stdlib만 — **신규 의존성 금지**(go.mod 불변).

## Global Constraints

- 전체 테스트는 항상 `go test -p 1 ./...` — **-p 1 필수**(메모리 캡 테스트 규칙). 단일 패키지 실행도 `-p 1` 유지.
- gofumpt clean: `gofumpt -l .` 출력 없음이 게이트.
- 파일 출력 UTF-8(no BOM), LF.
- **`git add -A` 금지** — 파일 명시 스테이징(untracked `.claude/`, `ctr.exe`, `ctr-new.exe` 보호).
- 커밋 트레일러: `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
- 훅 fail-open 계약 유지: hook.Run은 **항상 exit 0**, 실패는 drops 1줄.
- 가드는 allow 편향: 판정 불가·복합·상대경로(deny 게이트 기준)는 전부 통과. deny는 §3 순서 ①~④ 전건 성립 시에만.
- 테스트 픽스처 경로는 `filepath.EvalSymlinks`(기존 헬퍼 `evalLong`) 정규화 — Windows 8.3 함정.
- 비밀 canary는 런타임 분할 리터럴(`"xox"+"b-..."`) — 소스에 연속 토큰 금지.
- 안내 이벤트·오류 문자열에 절대경로·원문 미포함(§5.5 승계).
- 긴 테스트 데이터는 `strings.Repeat`/testdata로 — 서브에이전트 응답 분할 규율.
- 브랜치: `feat/v0.4-channel-expansion` (main의 계획 커밋에서 분기). 리뷰 BASE는 SDD 원장에 기록된 분기점 커밋 — `HEAD~1` 금지.
- **부채 편승(설계 §6) = 없음 확정**: session-14 태스크 리뷰 잔여는 M2 advisory 2건뿐이고 "none actionable" 판정(세션 기록 §4-3) — v0.4 편승 코드 작업 없음.

---

### Task 0: G1·G2·G3 관측 프리체크 — **완료됨(검증만 수행)**

2026-07-21 계획 세션(컨트롤러)이 수행·판정 완료. 결과·채택 분기는 설계서
`docs/context-router-design-v0.4-ko.md` **§11.1**에 추기됨(정본), 세션 기록에도 반영.
요지: G1 → §2 결정표 **행 1**(전체: 등록+도구 단위 계측+shadow) / G2 → matcher
`Read|Bash|PowerShell`, tool_input `{command, description}`, tool_response
`{stdout, stderr, interrupted, isImage}` / G3 → TOML 병합기 비발동, `hooks.json`
JSON 병합(마커는 statusMessage), Codex 등록 이벤트 = SessionStart + PostToolUse.

- [ ] **Step 1: 검증** — `docs/context-router-design-v0.4-ko.md`의 `### §11.1` 절이 존재하고 위 3개 채택(행 1 / matcher 문자열 / hooks.json)이 기재돼 있는지 읽어 확인. 불일치 시 중단·보고(컨트롤러 몫).

---

### Task 1: psDumpArg·psAbsPath 어휘 게이트 (D36 전반부)

**Files:**
- Modify: `internal/hook/hook.go` (bashDumpArg·dumpAbsPath 바로 아래에 형제 함수 추가)
- Test: `internal/hook/hook_test.go` (TestDumpAbsPath 아래)

**Interfaces:**
- Consumes: 없음(순수 함수 신설).
- Produces: `psDumpArg(command string) string` — "단일 단순 PS 덤프"면 경로 인자, 아니면 `""`. `psAbsPath(goos, arg string) string` — 절대경로면 슬래시 정규화 경로, 아니면 `""`. (Task 2 guardPowerShell·Task 3 commandDumpPath가 소비.)

- [ ] **Step 1: 실패 테스트 작성** — `internal/hook/hook_test.go`의 `TestDumpAbsPath` 뒤에 추가:

```go
// TestPsDumpArg: D36 어휘 판정(bashDumpArg 자매) — "대소문자 무시 덤프 토큰(Get-Content·gc·
// cat·type) + 위치 인자 정확히 1개"만 경로를 반환, 나머지는 전부 ""(allow). 오탐 deny 차단이
// 목적이라 거부 케이스가 본론이다. G2 실측(설계 §11.1): 입력은 tool_input.command.
func TestPsDumpArg(t *testing.T) {
	cases := []struct{ cmd, want string }{
		{`Get-Content C:\big\file.log`, `C:\big\file.log`}, // 백슬래시는 PS 경로 구분자 — 허용(bash와 차이)
		{"get-content C:/big/file.log", "C:/big/file.log"}, // 대소문자 무시
		{"gc C:/big/file.log", "C:/big/file.log"},          // alias
		{"cat C:/big/file.log", "C:/big/file.log"},         // pwsh alias
		{"type C:/big/file.log", "C:/big/file.log"},        // cmd 유래 alias(PS에선 Get-Content)
		{"Get-Content file.log", "file.log"},               // 어휘상 후보 — 절대 판정은 psAbsPath 몫
		{"Get-Content -TotalCount 5 C:/f", ""},             // 부분 읽기 — 인자 3개
		{"gc C:/f -Tail 10", ""},                           // 부분 읽기(후위)
		{"gc -Raw C:/f", ""},                               // 대시 토큰
		{"gc -Path C:/f", ""},                              // 명명 파라미터
		{"gc C:/a,C:/b", ""},                               // 콤마 배열 — 제외
		{"gc C:/*.log", ""},                                // 와일드카드 — 제외
		{"gc $env:TEMP/f", ""},                             // 변수 전개 — 제외
		{"gc C:/f | Select-Object -First 5", ""},           // 파이프 — 제외
		{"gc C:/f; ls", ""},                                // 복합식 — 제외
		{"Get-Content 'C:/f'", ""},                         // 인용 — 제외(보수)
		{"gc `C:/f`", ""},                                  // 백틱(PS 이스케이프) — 제외
		{"gc C:/한글.log", ""},                              // 비ASCII — 전면 판정 포기
		{"gc ~/f", ""},                                     // ~ 홈 확장 — 제외
		{"gc @(C:/f)", ""},                                 // @ 서브식/배열 — 제외
		{"Set-Content C:/f", ""},                           // 덤프 아닌 명령
		{"", ""},
	}
	for _, c := range cases {
		if got := psDumpArg(c.cmd); got != c.want {
			t.Fatalf("psDumpArg(%q)=%q want %q", c.cmd, got, c.want)
		}
	}
}

// TestPsAbsPath: psDumpArg 인자의 절대 판정 — bash용 MSYS /x/ 변환을 승계하지 않는다
// (설계 §11.1 파생 ②: PS에서 /c/x는 현재 드라이브 루트 상대라 변환 시 오파일 판정 위험).
func TestPsAbsPath(t *testing.T) {
	cases := []struct{ goos, arg, want string }{
		{"windows", `C:\big\f.log`, "C:/big/f.log"}, // ToSlash 정규화
		{"windows", "C:/big/f.log", "C:/big/f.log"},
		{"windows", "/c/big/f.log", ""},   // MSYS형 — PS에선 드라이브 상대, 비절대(allow)
		{"windows", `\\srv\share\f`, ""},  // UNC — 드라이브형 아님, 보수 allow
		{"windows", "f.log", ""},          // 상대
		{"windows", "C:big.log", ""},      // 드라이브 상대(C: 뒤 구분자 없음)
		{"linux", "/var/log/big.log", "/var/log/big.log"},
		{"linux", "f.log", ""},
	}
	for _, c := range cases {
		if got := psAbsPath(c.goos, c.arg); got != c.want {
			t.Fatalf("psAbsPath(%q,%q)=%q want %q", c.goos, c.arg, got, c.want)
		}
	}
}
```

- [ ] **Step 2: 실패 확인** — Run: `go test -p 1 ./internal/hook/ -run 'TestPsDumpArg|TestPsAbsPath'` → Expected: FAIL `undefined: psDumpArg`.

- [ ] **Step 3: 최소 구현** — `internal/hook/hook.go`의 `dumpAbsPath` 함수 아래에 추가:

```go
// psDumpArg — D36 어휘 판정(bashDumpArg 자매): 명령이 "단일 단순 <덤프 토큰> <경로>"일 때만
// 경로 인자를 반환한다(그 외 전부 "" = allow). 덤프 토큰은 대소문자 무시 Get-Content·gc·cat·
// type. PS 메타문자(파이프·리다이렉트·서브식 $()·변수 $·배열 콤마·스플래팅 @·백틱 이스케이프·
// 주석 #·인용·와일드카드·괄호·세미콜론·~)와 비ASCII는 전면 판정 포기 — 파서는 확신이 있을
// 때만 deny하고 오동작의 최대 피해는 "가드 미발화"다(설계 v0.4 §3, bashDumpArg 원칙 승계).
// bash와 달리 백슬래시는 Windows 경로 구분자라 허용한다. 부분 읽기 플래그(-TotalCount·-Head·
// -Tail)와 명명 파라미터(-Raw·-Path 등)는 "인자 정확히 1개 + 대시 토큰 배제"에 이미 걸러진다.
// 덤프 토큰의 별칭 재정의·프로필 함수 shadow로 인한 오탐 deny는 D32 bash `cat` 셰도잉과 동일
// 클래스로 수용(§11.2 F1) — deny 시에도 대상 파일이 현장 색인돼 ctr_search/ctr_fetch로 복구
// 가능하다(비가역 아님).
func psDumpArg(command string) string {
	for i := 0; i < len(command); i++ {
		if command[i] < 0x20 || command[i] > 0x7e {
			return ""
		}
	}
	if strings.ContainsAny(command, "|&;<>`$(){}*?[]'\"~#@,") {
		return ""
	}
	fields := strings.Fields(command)
	if len(fields) != 2 || strings.HasPrefix(fields[1], "-") {
		return ""
	}
	switch strings.ToLower(fields[0]) {
	case "get-content", "gc", "cat", "type":
		return fields[1]
	}
	return ""
}

// psAbsPath — psDumpArg 인자의 절대경로 판정(dumpAbsPath 자매). PS에서 `/c/x`는 MSYS가 아니라
// "현재 드라이브 루트 상대"라 bash용 MSYS 변환을 승계하면 오파일 stat 위험(설계 §11.1 파생 ②)
// — Windows는 드라이브형(`X:\`·`X:/`)만 절대로 인정하고 ToSlash로 정규화한다. Unix(pwsh)는
// `/`-접두만 절대. 그 외 전부 ""(allow).
func psAbsPath(goos, arg string) string {
	if goos == "windows" {
		arg = filepath.ToSlash(arg)
		if len(arg) >= 3 && arg[1] == ':' && arg[2] == '/' &&
			((arg[0] >= 'a' && arg[0] <= 'z') || (arg[0] >= 'A' && arg[0] <= 'Z')) {
			return arg
		}
		return ""
	}
	if strings.HasPrefix(arg, "/") {
		return arg
	}
	return ""
}
```

- [ ] **Step 4: 통과 확인** — Run: `go test -p 1 ./internal/hook/ -run 'TestPsDumpArg|TestPsAbsPath'` → Expected: PASS. `gofumpt -l internal/hook/` → 출력 없음.

- [ ] **Step 5: 커밋**

```bash
git add internal/hook/hook.go internal/hook/hook_test.go
git commit -m "feat(v0.4): psDumpArg·psAbsPath 어휘 게이트 — D36 전반부, MSYS 비승계(§11.1)"
```

---

### Task 2: guardPowerShell 배선 + matcher 확장 (D36 후반부)

**Files:**
- Modify: `internal/hook/hook.go` (dispatch switch ~L138-145, guardBash 아래에 guardPowerShell)
- Modify: `internal/cli/hook_install.go` (hookRegistrations ~L33-41 matcher)
- Test: `internal/hook/hook_test.go`, `internal/cli/hook_install_test.go`

**Interfaces:**
- Consumes: Task 1의 `psDumpArg`·`psAbsPath`, 기존 `guardReadMax`·`denyTool`·`workspaceRel`·`appendDrop`, `ingest.Run`, `store.OpenContext`.
- Produces: `guardPowerShell(ctx context.Context, ad *session.AppendDB, in hookInput, dir, contentDir, worktreeRoot string, getenv func(string) string, stdout io.Writer)` — guardBash와 동일 시그니처. 설치 matcher 문자열 `"Read|Bash|PowerShell"`.

- [ ] **Step 1: 실패 테스트 작성** — `internal/hook/hook_test.go`의 TestGuardBash* 뒤에, 기존 `runGuardBash` 헬퍼를 미러한 헬퍼 + 5개 테스트 추가(셋업·단정은 TestGuardBash* 관례 그대로 — `guardSetup`·`writeSized`·`contentArtifacts`·`evalLong` 재사용):

```go
// runGuardPowerShell — runGuardBash의 PowerShell 형제(tool_name·tool_input만 다름).
func runGuardPowerShell(t *testing.T, storeRoot, cwd, command string, env map[string]string) string {
	t.Helper()
	in := fixtureWith(t, "pretooluse-read.json", map[string]any{
		"cwd":        cwd,
		"tool_name":  "PowerShell",
		"tool_input": map[string]any{"command": command, "description": "test"},
	})
	var out bytes.Buffer
	rc := Run(context.Background(), bytes.NewReader(in), &out, storeRoot, "test", func(k string) string { return env[k] })
	if rc != 0 {
		t.Fatalf("guardPowerShell rc=%d want 0", rc)
	}
	return out.String()
}

// D36-① 대형 파일 단순 Get-Content → deny JSON + warning 이벤트(PowerShell·명령 토큰·상대경로·
// 크기·ctr_search 포함, 절대경로 비포함) + 현장 인덱싱 아티팩트 1건.
func TestGuardPowerShellLargeFileDenies(t *testing.T) {
	storeRoot, cwd, contentDir, sdir := guardSetup(t)
	big := filepath.Join(cwd, "big.txt")
	writeSized(t, big, 300*1024)
	out := runGuardPowerShell(t, storeRoot, cwd, "Get-Content "+filepath.ToSlash(big), nil)

	var got map[string]map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("deny stdout이 유효 JSON 아님: %v (out=%q)", err, out)
	}
	hso := got["hookSpecificOutput"]
	if hso["hookEventName"] != "PreToolUse" || hso["permissionDecision"] != "deny" {
		t.Fatalf("deny 스키마 불일치: %+v", hso)
	}
	if n := contentArtifacts(t, contentDir); n != 1 {
		t.Fatalf("artifacts=%d want 1(현장 인덱싱 성공)", n)
	}
	reader, err := session.OpenReadOnly(sdir)
	if err != nil {
		t.Fatalf("open session.db: %v", err)
	}
	defer func() { _ = reader.Close() }()
	var n int
	var summary string
	if err := reader.QueryRow("SELECT count(*), coalesce(max(summary),'') FROM session_events WHERE event_type='warning'").Scan(&n, &summary); err != nil {
		t.Fatalf("count warning: %v", err)
	}
	if n != 1 {
		t.Fatalf("warning events=%d want 1", n)
	}
	for _, want := range []string{"PowerShell", "Get-Content", "big.txt", "307200", "ctr_search"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("warning summary=%q want 포함 %q", summary, want)
		}
	}
	if strings.Contains(summary, cwd) || strings.Contains(summary, filepath.ToSlash(cwd)) {
		t.Fatalf("warning summary가 절대경로 누출: %q", summary)
	}
}

// D36-② 파이프 포함 → allow. ③ 부분 읽기 플래그 → allow. ④ 상대경로 → allow.
// ⑤ MSYS형 /c/... → allow(psAbsPath 비승계 — bash 가드와 다른 지점의 회귀 방지).
func TestGuardPowerShellPipeAllows(t *testing.T) {
	storeRoot, cwd, _, _ := guardSetup(t)
	big := filepath.Join(cwd, "big.txt")
	writeSized(t, big, 300*1024)
	if out := runGuardPowerShell(t, storeRoot, cwd, "Get-Content "+filepath.ToSlash(big)+" | Select-Object -First 5", nil); out != "" {
		t.Fatalf("stdout=%q want empty (파이프 = allow)", out)
	}
}

func TestGuardPowerShellPartialReadAllows(t *testing.T) {
	storeRoot, cwd, _, _ := guardSetup(t)
	big := filepath.Join(cwd, "big.txt")
	writeSized(t, big, 300*1024)
	if out := runGuardPowerShell(t, storeRoot, cwd, "Get-Content -TotalCount 5 "+filepath.ToSlash(big), nil); out != "" {
		t.Fatalf("stdout=%q want empty (부분 읽기 = allow)", out)
	}
}

func TestGuardPowerShellRelativePathAllows(t *testing.T) {
	storeRoot, cwd, _, _ := guardSetup(t)
	writeSized(t, filepath.Join(cwd, "big.txt"), 300*1024)
	if out := runGuardPowerShell(t, storeRoot, cwd, "Get-Content big.txt", nil); out != "" {
		t.Fatalf("stdout=%q want empty (상대경로 = allow)", out)
	}
}

func TestGuardPowerShellMsysFormAllows(t *testing.T) {
	storeRoot, cwd, _, _ := guardSetup(t)
	big := filepath.Join(cwd, "big.txt")
	writeSized(t, big, 300*1024)
	msys := "/" + strings.ToLower(string(big[0])) + filepath.ToSlash(big[2:]) // C:\x → /c/x
	if out := runGuardPowerShell(t, storeRoot, cwd, "Get-Content "+msys, nil); out != "" {
		t.Fatalf("stdout=%q want empty (MSYS형 = PS 드라이브 상대 = allow)", out)
	}
}
```

`TestGuardPowerShellMsysFormAllows`는 Windows 전용 형태이므로 함수 첫 줄에
`if runtime.GOOS != "windows" { t.Skip("드라이브형 경로 전제") }` 가드(기존 OS 분기 관례).
이 테스트가 `runtime.GOOS`를 쓰므로 **hook_test.go 임포트 블록에 `"runtime"` 추가 필수**
(현재 미임포트 — 계획 리뷰 F6).

- [ ] **Step 2: 실패 확인** — Run: `go test -p 1 ./internal/hook/ -run 'TestGuardPowerShell'` → Expected: FAIL(deny 미발화 — dispatch에 PowerShell 케이스 없음 → LargeFileDenies가 빈 stdout으로 실패).

- [ ] **Step 3: 구현** — `internal/hook/hook.go`:

(a) dispatch의 PreToolUse switch(~L139)에 케이스 추가 + 주석 갱신:

```go
	// PreToolUse는 T7 large-read/dump guard 몫 — tool_call로 중복 계상하지 않고 tool_name으로
	// 분기한다(설계 §4 D25·D32·v0.4 D36). matcher가 Read|Bash|PowerShell라 여기 오는 건 사실상
	// 이 셋뿐이고, 각 가드가 자체 정적 판정으로 그 외를 통과시킨다.
	if in.HookEventName == "PreToolUse" {
		switch in.ToolName {
		case "Read":
			guardRead(ctx, ad, in, dir, contentDir, worktreeRoot, getenv, stdout)
		case "Bash":
			guardBash(ctx, ad, in, dir, contentDir, worktreeRoot, getenv, stdout)
		case "PowerShell":
			guardPowerShell(ctx, ad, in, dir, contentDir, worktreeRoot, getenv, stdout)
		}
		return
	}
```

(b) guardBash 아래에 형제 함수(기존 스타일 그대로 — 공유 헬퍼 추출 안 함, guardRead/guardBash도 중복 구조가 관례):

```go
// guardPowerShell — D36 PowerShell 단일파일 덤프 가드(guardBash의 형제, 설계 v0.4 §3). 정적
// 판정(psDumpArg 어휘 + psAbsPath 절대경로) 성립 시에만 D25 4조건(임계 초과·경계 내·denylist
// 아님·현장 인덱싱 성공)을 guardBash와 동일 경로로 판정하고 deny한다. 그 외 전부 통과.
func guardPowerShell(ctx context.Context, ad *session.AppendDB, in hookInput, dir, contentDir, worktreeRoot string, getenv func(string) string, stdout io.Writer) {
	var f struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(in.ToolInput, &f) != nil {
		return
	}
	path := psAbsPath(runtime.GOOS, psDumpArg(f.Command))
	if path == "" {
		return // 정적 판정 불가·비덤프·비절대 — 통과
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() <= guardReadMax(getenv) {
		return // 임계 이하·stat 불가·디렉터리 — 통과
	}
	st, err := store.OpenContext(ctx, contentDir, false)
	if err != nil {
		appendDrop(dir, "guard-store")
		return
	}
	defer func() { _ = st.Close() }()
	rep, err := ingest.Run(ctx, st, worktreeRoot, nil, ingest.Request{Path: path})
	if err != nil || rep.Indexed != 1 {
		return // 경계 밖·denylist·oversize·색인 실패 — 통과
	}
	// detail의 명령 토큰은 psDumpArg 성립 시 화이트리스트 4토큰 중 하나라 원문 운반이 안전하다.
	cmdToken := strings.Fields(f.Command)[0]
	denyTool(ctx, ad, in, dir, "PowerShell", cmdToken+" "+workspaceRel(in.CWD, path)+" "+strconv.FormatInt(info.Size(), 10)+"B — ctr_search/ctr_fetch", stdout)
}
```

(c) `internal/cli/hook_install.go` hookRegistrations(~L38)의 PreToolUse matcher를 교체하고 주석 갱신:

```go
	{"PreToolUse", "Read|Bash|PowerShell"},
```

- [ ] **Step 4: 기존 matcher 단정 갱신** — `internal/cli/hook_install_test.go`에서 `Read|Bash` 문자열 단정을 전부 `Read|Bash|PowerShell`로 갱신(Grep으로 위치 확인: `grep -n 'Read|Bash' internal/cli/`). 설치 골든/멱등 테스트가 새 matcher로 GREEN인지 확인.

- [ ] **Step 5: 통과 확인** — Run: `go test -p 1 ./internal/hook/ ./internal/cli/` → Expected: PASS. `gofumpt -l internal/` → 출력 없음.

- [ ] **Step 6: 커밋**

```bash
git add internal/hook/hook.go internal/hook/hook_test.go internal/cli/hook_install.go internal/cli/hook_install_test.go
git commit -m "feat(v0.4): guardPowerShell + matcher Read|Bash|PowerShell — D36 배선"
```

---

### Task 3: command shadow denylist 정적 대조 (D39)

**Files:**
- Modify: `internal/hook/shadow.go` (fileOriginTools 게이트 직후)
- Test: `internal/hook/hook_test.go` (TestShadowDenylistSkips 뒤)

**Interfaces:**
- Consumes: Task 1 `psDumpArg`, 기존 `bashDumpArg`, `ingest.DeniedFilename(path string) bool`.
- Produces: `commandDumpPath(in hookInput) string` — command 계열(Bash·PowerShell) tool_input이 정적 단일 파일 덤프로 증명되면 그 경로(절대화 없음), 아니면 `""`.

- [ ] **Step 1: 실패 테스트 작성** — `internal/hook/hook_test.go`에 추가(shadowSetup·bigStdout·contentArtifacts·readDrops 재사용). Bash 케이스는 canary를 겸한다(§8 — 정적 증명 케이스의 비밀 미색인):

```go
// D39-① Bash `cat .env`(정적 증명 덤프, denylist 파일) → 미저장 + drops shadow-denylist.
// 응답 본문에 런타임 분할 canary를 실어 "비밀이 store에 없다"까지 겸증한다(§8).
func TestShadowCommandDenylistSkipsBash(t *testing.T) {
	storeRoot, cwd, contentDir, sdir := shadowSetup(t)
	canary := "xox" + "b-1234567890-ABCDEFGHIJKLMNOP" // 런타임 조립 — 소스에 연속 토큰 금지
	in := fixtureWith(t, "posttooluse-bash.json", map[string]any{
		"cwd":           cwd,
		"tool_input":    map[string]any{"command": "cat .env"},
		"tool_response": map[string]any{"stdout": canary + strings.Repeat("x", 20000), "stderr": ""},
	})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	if n := contentArtifacts(t, contentDir); n != -1 {
		t.Fatalf("artifacts=%d want -1(denylist 미저장 — content.db 미생성)", n)
	}
	if got := readDrops(t, sdir); !strings.Contains(got, "shadow-denylist") {
		t.Fatalf("drops=%q want shadow-denylist", got)
	}
}

// D39-② PowerShell `Get-Content .env` — Bash와 대칭.
func TestShadowCommandDenylistSkipsPowerShell(t *testing.T) {
	storeRoot, cwd, contentDir, sdir := shadowSetup(t)
	in := fixtureWith(t, "posttooluse-bash.json", map[string]any{
		"cwd":           cwd,
		"tool_name":     "PowerShell",
		"tool_input":    map[string]any{"command": "Get-Content .env"},
		"tool_response": bigStdout(20000),
	})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	if n := contentArtifacts(t, contentDir); n != -1 {
		t.Fatalf("artifacts=%d want -1(denylist 미저장)", n)
	}
	if got := readDrops(t, sdir); !strings.Contains(got, "shadow-denylist") {
		t.Fatalf("drops=%q want shadow-denylist", got)
	}
}

// D39-④ 점 세그먼트 변형도 정규화 후 대조된다 — `.docker/config.json` 접미 규칙 우회 봉쇄
// (계획 리뷰 F2).
func TestShadowCommandDenylistNormalizes(t *testing.T) {
	storeRoot, cwd, contentDir, sdir := shadowSetup(t)
	in := fixtureWith(t, "posttooluse-bash.json", map[string]any{
		"cwd":           cwd,
		"tool_input":    map[string]any{"command": "cat ./.docker/./config.json"},
		"tool_response": bigStdout(20000),
	})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	if n := contentArtifacts(t, contentDir); n != -1 {
		t.Fatalf("artifacts=%d want -1(정규화 후 denylist 대조)", n)
	}
	if got := readDrops(t, sdir); !strings.Contains(got, "shadow-denylist") {
		t.Fatalf("drops=%q want shadow-denylist", got)
	}
}

// D39-③ 증명 불가 출력(파이프)은 현행대로 색인 — 커버리지 급감 방지(설계 §7 잔여 한계).
func TestShadowCommandUnprovenStillIndexes(t *testing.T) {
	storeRoot, cwd, contentDir, _ := shadowSetup(t)
	in := fixtureWith(t, "posttooluse-bash.json", map[string]any{
		"cwd":           cwd,
		"tool_input":    map[string]any{"command": "cat .env | head"},
		"tool_response": bigStdout(20000),
	})
	if rc := runHook(t, storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d want 0", rc)
	}
	if n := contentArtifacts(t, contentDir); n != 1 {
		t.Fatalf("artifacts=%d want 1(증명 불가 = 현행 색인 유지)", n)
	}
}
```

- [ ] **Step 2: 실패 확인** — Run: `go test -p 1 ./internal/hook/ -run 'TestShadowCommand'` → Expected: FAIL(①② artifacts가 1 — 대조 없이 색인됨).

- [ ] **Step 3: 구현** — `internal/hook/shadow.go`:

(a) `shadowInputDenied` 아래에 추가:

**`internal/hook/shadow.go` 임포트 블록에 `"path"`·`"path/filepath"` 추가 필수**(현재 둘 다
미임포트 — 계획 리뷰 F6).

```go
// commandDumpPath — command 계열 도구(Bash·PowerShell)의 tool_input이 정적으로 "단일 파일
// 덤프"로 증명되면 그 경로를 반환한다(D39). 증명 불가(파이프·복합식·다중 파일 등)는 "" —
// 현행대로 색인한다(잔여 표면은 설계 v0.4 §7 한계 명문화, Redact·sniff 의존). 절대화는 하지
// 않는다 — 대조는 이름 기반(ingest.DeniedFilename)이라 상대경로 덤프도 커버한다(§11.1 파생 ①).
func commandDumpPath(in hookInput) string {
	var f struct {
		Command string `json:"command"`
	}
	var p string
	switch in.ToolName {
	case "Bash":
		_ = json.Unmarshal(in.ToolInput, &f)
		p = bashDumpArg(f.Command)
	case "PowerShell":
		_ = json.Unmarshal(in.ToolInput, &f)
		p = psDumpArg(f.Command)
	}
	if p == "" {
		return ""
	}
	// 대조 전 정규화(ToSlash+Clean) — 점 세그먼트·중복 구분자로 `.docker/config.json` 접미
	// 규칙을 우회하는 변형을 봉쇄하고, PS 백슬래시 경로의 basename 판정을 OS 무관하게 만든다
	// (계획 리뷰 F2). 대소문자·symlink 변형은 Read 경로 denylist와 동일한 잔여 표면(§7).
	return path.Clean(filepath.ToSlash(p))
}
```

(b) shadowCapture의 fileOriginTools 게이트(~L43-46) 직후에 삽입:

```go
	if p := commandDumpPath(in); p != "" && ingest.DeniedFilename(p) {
		appendDrop(dir, "shadow-denylist") // D39 — 정적 증명 덤프 경로가 denylist 파일, 미저장
		return
	}
```

(c) fileOriginTools 주석에 1줄 보강: `// command 출력(Bash·PowerShell)은 commandDumpPath의 정적 증명 시에만 경로 대조한다(D39).`

- [ ] **Step 4: 통과 확인** — Run: `go test -p 1 ./internal/hook/` → Expected: PASS(기존 shadow 테스트 무회귀 포함). `gofumpt -l internal/hook/` → 출력 없음.

- [ ] **Step 5: 커밋**

```bash
git add internal/hook/shadow.go internal/hook/hook_test.go
git commit -m "feat(v0.4): command shadow denylist 정적 대조 — D39, Bash·PowerShell 대칭"
```

---

### Task 4: 호스트 경계 신설 — hook.Run Host 인자 + cc/cx 격리 (D35 런타임)

**Files:**
- Modify: `internal/hook/hook.go` (Host 타입, Run 시그니처·검증·external 조립)
- Modify: `internal/cli/hook_install.go` (runHook의 hook.Run 호출 + runCodexHook 신설)
- Modify: `internal/cli/cli.go` (Run switch에 `codex-hook` 케이스)
- Modify: `cmd/context-router/main.go` (cliSubcommands + absorbHookPreprocErr)
- Create: `internal/hook/testdata/posttooluse-codex-bash.json`
- Test: `internal/hook/hook_test.go` (헬퍼 갱신 + 격리·bad-host 테스트)

**Interfaces:**
- Consumes: 기존 dispatch(external 인자 이미 존재 — 무변경).
- Produces: `type Host string`, `HostClaude Host = "cc"`, `HostCodex Host = "cx"`, `Run(ctx context.Context, stdin io.Reader, stdout io.Writer, storeRoot, version string, host Host, getenv func(string) string) int`. **CLI 러닝 서브커맨드 `codex-hook`**(Codex 전용 — `--host` 플래그 방식은 기각: v0.3 러닝 훅이 미지 인자를 fail-open으로 무시하므로 "구버전 바이너리 + 신버전 hooks.json" 조합에서 Codex 이벤트가 조용히 cc:로 오귀속된다. 미지 서브커맨드는 v0.3 dispatchCLI가 exit 1로 거부하므로 서브커맨드가 구조적 버전 게이트다 — 계획 리뷰 F3·§11.2). Task 5 설치 명령이 `context-router codex-hook`을 기입한다.

- [ ] **Step 1: 픽스처 생성** — `internal/hook/testdata/posttooluse-codex-bash.json` (G1 실측·문서 기반 Codex 형상 — 추가 필드는 hookInput이 무시함을 겸증). **session_id는 형제 픽스처(sessionstart.json·posttooluse-bash.json)와 동일한 UUID를 쓴다** — 격리 테스트의 목적이 "동일 UUID의 cc/cx 오귀속 금지"라 UUID가 다르면 단순 불일치 drop만 검증하게 된다(계획 리뷰 C1):

```json
{
  "session_id": "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
  "transcript_path": null,
  "cwd": "C:/replaced-by-test",
  "turn_id": "turn-1",
  "tool_use_id": "call-1",
  "permission_mode": "default",
  "model": "gpt-5.6-sol",
  "hook_event_name": "PostToolUse",
  "tool_name": "Bash",
  "tool_input": { "command": "echo hello" },
  "tool_response": { "stdout": "hello", "stderr": "" },
  "duration_ms": 12
}
```

`internal/hook/testdata/README.md`에 1줄 추가: `posttooluse-codex-bash.json — Codex CLI 훅 형상(설계 v0.4 §11.1 G1, codex-cli 0.144.6 문서 기준; 추가 필드는 무시됨).`

- [ ] **Step 2: 실패 테스트 작성** — `internal/hook/hook_test.go`:

(a) 헬퍼 `runHook`(L46-49)에 host 상수 삽입 + 형제 헬퍼 추가:

```go
// runHook — Run을 결정론 입력(env 맵·io.Discard stdout)으로 호출한다(cc 기본).
func runHook(t *testing.T, storeRoot string, in []byte, env map[string]string) int {
	t.Helper()
	return runHookHost(t, HostClaude, storeRoot, in, env)
}

// runHookHost — 호스트 경계 주입 변형(D35).
func runHookHost(t *testing.T, host Host, storeRoot string, in []byte, env map[string]string) int {
	t.Helper()
	return Run(context.Background(), bytes.NewReader(in), io.Discard, storeRoot, "test", host, func(k string) string { return env[k] })
}
```

(b) 신규 테스트:

```go
// D35 — 동일 UUID의 cc/cx 오귀속 금지: cc SessionStart 후 같은 UUID의 cx 이벤트는
// unknown-session drop, cx SessionStart를 거쳐야 cx 이벤트가 기록된다(네임스페이스 격리).
func TestHookHostIsolation(t *testing.T) {
	storeRoot := t.TempDir()
	cwd := evalLong(t, t.TempDir())
	start := fixtureWith(t, "sessionstart.json", map[string]any{"cwd": cwd})
	post := fixtureWith(t, "posttooluse-codex-bash.json", map[string]any{"cwd": cwd})

	if rc := runHook(t, storeRoot, start, nil); rc != 0 { // cc 세션 등록
		t.Fatalf("cc SessionStart rc=%d", rc)
	}
	if rc := runHookHost(t, HostCodex, storeRoot, post, nil); rc != 0 { // 같은 UUID의 cx 이벤트
		t.Fatalf("cx PostToolUse rc=%d", rc)
	}
	sdir := sessDir(t, storeRoot, cwd)
	if got := readDrops(t, sdir); !strings.Contains(got, "unknown-session") {
		t.Fatalf("drops=%q want unknown-session (cx 미등록 — cc로 오귀속 금지)", got)
	}
	if rc := runHookHost(t, HostCodex, storeRoot, start, nil); rc != 0 { // cx 세션 등록
		t.Fatalf("cx SessionStart rc=%d", rc)
	}
	if rc := runHookHost(t, HostCodex, storeRoot, post, nil); rc != 0 {
		t.Fatalf("cx PostToolUse(등록 후) rc=%d", rc)
	}
	reader, err := session.OpenReadOnly(sdir)
	if err != nil {
		t.Fatalf("open session.db: %v", err)
	}
	defer func() { _ = reader.Close() }()
	var cc, cx, cxEv int
	if err := reader.QueryRow(`SELECT
		(SELECT count(*) FROM sessions WHERE session_id LIKE 'cc:%'),
		(SELECT count(*) FROM sessions WHERE session_id LIKE 'cx:%'),
		(SELECT count(*) FROM session_events WHERE session_id LIKE 'cx:%')`).Scan(&cc, &cx, &cxEv); err != nil {
		t.Fatalf("count: %v", err)
	}
	if cc != 1 || cx != 1 || cxEv == 0 {
		t.Fatalf("cc=%d cx=%d cxEv=%d want 1,1,>0 (네임스페이스 격리)", cc, cx, cxEv)
	}

	// 결정표 행 1 수용(§11.1 G1) — cx 이벤트도 shadow 캡처되고 artifact ref가 cx: 네임스페이스를
	// 운반한다(D35+D30, 기존 dispatch·shadowCapture 재사용 경로의 계약 고정).
	bigPost := fixtureWith(t, "posttooluse-codex-bash.json", map[string]any{
		"cwd": cwd, "tool_response": bigStdout(20000),
	})
	if rc := runHookHost(t, HostCodex, storeRoot, bigPost, nil); rc != 0 {
		t.Fatalf("cx big PostToolUse rc=%d", rc)
	}
	var refs string
	if err := reader.QueryRow(`SELECT coalesce(max(artifact_refs),'') FROM session_events
		WHERE session_id LIKE 'cx:%' AND event_type='artifact_created'`).Scan(&refs); err != nil {
		t.Fatalf("refs: %v", err)
	}
	if !strings.Contains(refs, "artifact://cx:") {
		t.Fatalf("refs=%q want artifact://cx: 접두(D35 네임스페이스 운반)", refs)
	}
}

// D35 — 미지 host는 오귀속 대신 drop(bad-host, storeRoot 사이드카) + exit 0.
func TestHookBadHostDrops(t *testing.T) {
	storeRoot := t.TempDir()
	cwd := evalLong(t, t.TempDir())
	in := fixtureWith(t, "sessionstart.json", map[string]any{"cwd": cwd})
	if rc := runHookHost(t, Host("zz"), storeRoot, in, nil); rc != 0 {
		t.Fatalf("rc=%d want 0(fail-open)", rc)
	}
	if got := readDrops(t, storeRoot); !strings.Contains(got, "bad-host") {
		t.Fatalf("drops=%q want bad-host", got)
	}
}
```

- [ ] **Step 3: 실패 확인** — Run: `go test -p 1 ./internal/hook/` → Expected: 컴파일 FAIL(`undefined: HostClaude`, Run 인자 수 불일치).

- [ ] **Step 4: 구현** — `internal/hook/hook.go`:

(a) canonicalUUIDRe 아래에:

```go
// Host — 훅 이벤트의 발신 호스트(설계 v0.4 §2 D35). 값이 곧 세션 네임스페이스 접두다.
// 진입점은 명시적 호스트 인자로 분기한다 — 암묵 재사용으로 인한 cc/cx 오귀속 금지.
type Host string

const (
	HostClaude Host = "cc" // Claude Code
	HostCodex  Host = "cx" // Codex CLI (§11.1 G1 — Claude 동형 훅 페이로드)
)
```

(b) Run 시그니처에 `host Host` 추가(version 뒤) + CTR_HOOKS_OFF 분기 직후 검증 삽입 + external 조립 교체:

```go
func Run(ctx context.Context, stdin io.Reader, stdout io.Writer, storeRoot, version string, host Host, getenv func(string) string) int {
	if getenv("CTR_HOOKS_OFF") == "1" {
		_, _ = io.Copy(io.Discard, stdin)
		return 0
	}
	if host != HostClaude && host != HostCodex {
		_, _ = io.Copy(io.Discard, stdin) // drain — broken pipe 방지
		appendDrop(storeRoot, "bad-host") // 오귀속 대신 drop(D35 격리)
		return 0
	}
	...기존 본문...
	dispatch(ctx, in, dir, contentDir, canon.WorktreeRoot, string(host)+":"+in.SessionID, version, getenv, stdout)
	return 0
}
```

Run·패키지 주석의 "cc: 세션" 문구를 "호스트 접두(cc:/cx:) 세션"으로 갱신.

(c) CLI 배선 — 전용 러닝 서브커맨드 `codex-hook`(계획 리뷰 F3):

- `internal/cli/hook_install.go`: 기존 runHook의 `hook.Run(...)` 호출에 `hook.HostClaude` 인자 삽입(그 외 무변경 — install/uninstall 분기는 host 무관). 그 아래에 형제 함수 신설:

```go
// runCodexHook — Codex 러닝 훅 전용 서브커맨드(설계 v0.4 §2 D35, §11.2 F3). install/uninstall
// 하위 없음 — 모든 인자는 러닝 훅 인자(--no-shadow만 인식, 그 외 fail-open 무시 D23). 전용
// 서브커맨드인 이유: v0.3 러닝 훅은 미지 인자를 무시하므로 플래그로 호스트를 구분하면 구버전
// 바이너리가 Codex 이벤트를 cc:로 오귀속시킨다 — 미지 서브커맨드는 v0.3이 exit 1로 거부해
// 오귀속이 구조적으로 불가능하다(버전 게이트).
func runCodexHook(ctx context.Context, args []string, storeRoot, version string, stdout io.Writer) error {
	getenv := os.Getenv
	for _, a := range args {
		if a == "--no-shadow" {
			getenv = shadowOffGetenv
			break
		}
	}
	hook.Run(ctx, os.Stdin, stdout, storeRoot, version, hook.HostCodex, getenv)
	return nil
}
```

- `internal/cli/cli.go` Run의 switch에 케이스 추가(`case "hook":` 아래):

```go
	case "codex-hook":
		// Codex 러닝 훅(설계 v0.4 §2 D35) — 항상 exit 0(fail-open §2.3). 전용 서브커맨드 =
		// 구버전 바이너리 오귀속 차단 게이트(§11.2 F3).
		return runCodexHook(ctx, args, storeRoot, version, stdout)
```

- `cmd/context-router/main.go` cliSubcommands(L468)에 `"codex-hook": true` 추가 + 주석 1줄(v0.4 — Codex 러닝 훅 전용, 구버전 오귀속 차단 게이트).

- `cmd/context-router/main.go` absorbHookPreprocErr(L522-530): 러닝 훅 판정에 codex-hook 포함(전처리 오류도 fail-open exit 0):

```go
	isRunningHook := sub == "codex-hook" ||
		(sub == "hook" && (len(rest) == 0 || (rest[0] != "install" && rest[0] != "uninstall")))
```

(d) `internal/hook/hook_test.go`의 나머지 `Run(` 호출 전부에 `HostClaude` 인자 삽입 — 직접 호출 2곳(L322·L343) + Read 가드 헬퍼 `runGuard`(L989) + `runGuardBash`(L1241) + Task 2에서 추가된 `runGuardPowerShell`. 명시 열거에 기대지 말고 grep `Run(context.Background`로 **전수 확인**(계획 리뷰 M2 — 열거 누락 방지).

- [ ] **Step 5: 통과 확인** — Run: `go test -p 1 ./internal/hook/ ./internal/cli/` → Expected: PASS. `gofumpt -l internal/` → 출력 없음.

- [ ] **Step 6: 커밋**

```bash
git add internal/hook/hook.go internal/hook/hook_test.go internal/hook/testdata/posttooluse-codex-bash.json internal/hook/testdata/README.md internal/cli/hook_install.go internal/cli/cli.go cmd/context-router/main.go
git commit -m "feat(v0.4): hook.Run 호스트 경계(cc/cx) + codex-hook 서브커맨드 + 동일 UUID 격리 — D35 런타임"
```

---

### Task 5: Codex hooks.json 설치 병합기 (D35 설치 — §11.1 G3 계약)

**Files:**
- Modify: `internal/cli/hook_install.go` (codex 등록·병합·경로·install/uninstall 분기)
- Test: `internal/cli/hook_install_test.go`

**Interfaces:**
- Consumes: 기존 `atomicWriteFile`, `hookMarker`/`hookMarkerPrefix`, `hookTimeoutSec`, Task 4의 러닝 서브커맨드 `codex-hook`.
- Produces: `codexHooksPath(user bool, projectRoot string) (string, error)`, `buildCodexHookCommand(storeRootExplicit bool, storeRootRaw string, noShadow bool) string`(첫 토큰들 `context-router codex-hook`), `isCodexHookCommandToken(cmd string) bool`, `mergeCodexHooks(existing []byte, command, marker string, install bool) ([]byte, error)`, `isOurCodexGroup(raw json.RawMessage) bool`(**전건 판정** — §11.2 F4). CLI: `hook install --codex [--user] [--no-shadow]`, `hook uninstall --codex [--user]`.

설치 계약(§11.1 G3 + §11.2 F4 — 전부 이 태스크의 테스트 대상): 대상 파일은 project
`<root>/.codex/hooks.json` 기본, `--user`는 `~/.codex/hooks.json`. 등록 이벤트는
SessionStart(matcher "")·PostToolUse(matcher "") 2건. 마커는 `statusMessage:
"context-router/<version>"`(미지 필드 금지 — 스키마 관용성 미보증), 소유 판정은
**그룹의 모든 훅 항목**이 command 토큰 정확 일치(`context-router codex-hook`) AND
statusMessage 접두를 만족할 때만(전건 판정 — 혼합 그룹은 불가침: 파손 금지가 멱등
완전성에 우선하며, 사용자가 우리 그룹에 항목을 추가하면 소유권 이전으로 간주하고
그 그룹의 잔존 정리는 사용자 `/hooks` 몫). `config.toml`·`[hooks.state]`는 절대
건드리지 않는다(신뢰 승인 우회 금지). 설치 후 stdout에 Codex `/hooks` 신뢰 승인 안내
1줄. 인용 규칙은 Claude T11 관례(`'...'`)를 승계한다(가정 — 도그푸딩·스모크는
--store-root 미명시 경로만 사용).

- [ ] **Step 1: 실패 테스트 작성** — `internal/cli/hook_install_test.go`(기존 TestMergeHookSettings* 관례 미러):

```go
// D35 설치 — 병합이 타 그룹·미지 최상위 키를 보존하고 자기 2이벤트만 소유한다.
func TestMergeCodexHooksInstallPreservesForeign(t *testing.T) {
	existing := []byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"pwsh -File policy.ps1","timeout":10,"statusMessage":"policy"}]}]},"otherTop":1}`)
	out, err := mergeCodexHooks(existing, "context-router codex-hook", "context-router/0.4.0", true)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("out 파싱: %v", err)
	}
	if _, ok := m["otherTop"]; !ok {
		t.Fatalf("미지 최상위 키 소실: %s", out)
	}
	var hooks map[string][]json.RawMessage
	if err := json.Unmarshal(m["hooks"], &hooks); err != nil {
		t.Fatalf("hooks 파싱: %v", err)
	}
	if len(hooks["PreToolUse"]) != 1 {
		t.Fatalf("타 그룹 보존 실패: %v", hooks["PreToolUse"])
	}
	for _, ev := range []string{"SessionStart", "PostToolUse"} {
		if len(hooks[ev]) != 1 || !isOurCodexGroup(hooks[ev][0]) {
			t.Fatalf("%s 자기 그룹 미등록: %v", ev, hooks[ev])
		}
	}
	if strings.Contains(string(out), "__ctrManaged") {
		t.Fatalf("Codex hooks.json에 미지 필드 금지(§11.1 G3): %s", out)
	}
}

// 멱등: install 2회 = 1회와 동일 바이트. 제거 대칭: install→uninstall이 원본 구조를 복원.
func TestMergeCodexHooksIdempotentAndSymmetric(t *testing.T) {
	existing := []byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"pwsh -File policy.ps1","timeout":10}]}]}}`)
	once, err := mergeCodexHooks(existing, "context-router codex-hook", "context-router/0.4.0", true)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	twice, err := mergeCodexHooks(once, "context-router codex-hook", "context-router/0.4.1", true)
	if err != nil {
		t.Fatalf("재install: %v", err)
	}
	if strings.Contains(string(twice), "0.4.0") {
		t.Fatalf("구버전 마커 잔존(교체 실패): %s", twice)
	}
	removed, err := mergeCodexHooks(twice, "", "", false)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if strings.Contains(string(removed), "context-router") {
		t.Fatalf("자기 항목 잔존: %s", removed)
	}
	if !strings.Contains(string(removed), "policy.ps1") {
		t.Fatalf("타 그룹 소실: %s", removed)
	}
}

// F4 — 혼합 그룹(자기 항목 + 사용자 항목 동거)은 불가침: install이 그 그룹을 건드리지 않고
// 순수 자기 그룹을 별도로 추가한다(파손 금지 > 멱등 완전성 — 혼합 그룹의 자기 잔존 항목
// 정리는 사용자 /hooks 몫).
func TestMergeCodexHooksMixedGroupUntouched(t *testing.T) {
	mixed := []byte(`{"hooks":{"PostToolUse":[{"matcher":"","hooks":[` +
		`{"type":"command","command":"context-router codex-hook","timeout":10,"statusMessage":"context-router/0.3.9"},` +
		`{"type":"command","command":"pwsh -File user.ps1","timeout":10,"statusMessage":"user"}]}]}}`)
	out, err := mergeCodexHooks(mixed, "context-router codex-hook", "context-router/0.4.0", true)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !strings.Contains(string(out), "user.ps1") || !strings.Contains(string(out), "context-router/0.3.9") {
		t.Fatalf("혼합 그룹이 변형·삭제됨(불가침 위반): %s", out)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("out 파싱: %v", err)
	}
	var hooks map[string][]json.RawMessage
	if err := json.Unmarshal(m["hooks"], &hooks); err != nil {
		t.Fatalf("hooks 파싱: %v", err)
	}
	if len(hooks["PostToolUse"]) != 2 {
		t.Fatalf("PostToolUse 그룹 수=%d want 2(혼합 보존 + 순수 신규)", len(hooks["PostToolUse"]))
	}
}

// F4 — 동일 버전 재적용의 진짜 멱등: f(f(x)) == f(x) 바이트 동일(중복·순서 drift 검출).
func TestMergeCodexHooksIdempotentBytes(t *testing.T) {
	existing := []byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"pwsh -File policy.ps1","timeout":10}]}]}}`)
	once, err := mergeCodexHooks(existing, "context-router codex-hook", "context-router/0.4.0", true)
	if err != nil {
		t.Fatalf("1차: %v", err)
	}
	twice, err := mergeCodexHooks(once, "context-router codex-hook", "context-router/0.4.0", true)
	if err != nil {
		t.Fatalf("2차: %v", err)
	}
	if !bytes.Equal(once, twice) {
		t.Fatalf("멱등 위반:\n1차=%s\n2차=%s", once, twice)
	}
}

// 경로: 기본 project <root>/.codex/hooks.json, --user는 ~/.codex/hooks.json.
func TestCodexHooksPath(t *testing.T) {
	p, err := codexHooksPath(false, `C:\proj`)
	if err != nil || p != filepath.Join(`C:\proj`, ".codex", "hooks.json") {
		t.Fatalf("project 경로=%q err=%v", p, err)
	}
	u, err := codexHooksPath(true, `C:\proj`)
	if err != nil || !strings.HasSuffix(u, filepath.Join(".codex", "hooks.json")) {
		t.Fatalf("user 경로=%q err=%v", u, err)
	}
}

// e2e: hook install --codex가 파일 생성 + 신뢰 승인 안내를 출력한다.
func TestRunHookInstallCodex(t *testing.T) {
	root := t.TempDir()
	var out bytes.Buffer
	if err := runHookInstall([]string{"--codex"}, "", "", false, root, "0.4.0", &out); err != nil {
		t.Fatalf("install --codex: %v", err)
	}
	written, err := os.ReadFile(filepath.Join(root, ".codex", "hooks.json"))
	if err != nil {
		t.Fatalf("hooks.json 미생성: %v", err)
	}
	if !strings.Contains(string(written), "context-router codex-hook") {
		t.Fatalf("러닝 명령이 codex-hook 서브커맨드가 아님(§11.2 F3): %s", written)
	}
	if !strings.Contains(out.String(), "/hooks") {
		t.Fatalf("신뢰 승인 안내 누락: %q", out.String())
	}
}
```

- [ ] **Step 2: 실패 확인** — Run: `go test -p 1 ./internal/cli/ -run 'TestMergeCodexHooks|TestCodexHooksPath|TestRunHookInstallCodex'` → Expected: 컴파일 FAIL(`undefined: mergeCodexHooks`).

- [ ] **Step 3: 구현** — `internal/cli/hook_install.go`에 추가(기존 함수들 아래, 형제 배치):

```go
// codexRegistrations — Codex 설치 대상 2항목(D35 캐프처 전용, §11.1 G1 행 1 채택 — PreToolUse
// 미등록·거부 표면 없음 §7). matcher 빈 문자열 = 전체 매칭.
var codexRegistrations = []struct {
	event   string
	matcher string
}{
	{"SessionStart", ""},
	{"PostToolUse", ""},
}

// codexHookCmd — Codex hooks.json 훅 항목. statusMessage가 소유/버전 마커를 겸한다(§11.1 G3
// — 미지 필드의 스키마 관용성이 미보증이라 공식 필드에 탑재; Codex UI에 노출되는 것은 의도).
type codexHookCmd struct {
	Type          string `json:"type"`
	Command       string `json:"command"`
	Timeout       int    `json:"timeout"`
	StatusMessage string `json:"statusMessage"`
}

type codexOwnedGroup struct {
	Matcher string         `json:"matcher"`
	Hooks   []codexHookCmd `json:"hooks"`
}

// codexGroupProbe — 소유 판정에 필요한 필드만(나머지는 raw 왕복 보존).
type codexGroupProbe struct {
	Hooks []struct {
		Command       string `json:"command"`
		StatusMessage string `json:"statusMessage"`
	} `json:"hooks"`
}

// isCodexHookCommandToken — `context-router codex-hook` 정확 일치(접두사 매칭 금지 —
// isHookCommandToken의 형제, 러닝 서브커맨드가 달라 별도 함수).
func isCodexHookCommandToken(cmd string) bool {
	f := strings.Fields(cmd)
	return len(f) >= 2 && f[0] == hookBinaryName && f[1] == "codex-hook"
}

// isOurCodexGroup — 그룹의 **모든** 훅 항목이 자기 것(command 토큰 정확 일치 AND statusMessage
// 마커 접두)일 때만 자기 그룹으로 판정한다(전건 판정 — §11.2 F4). Claude 쪽 isOurHookGroup은
// 그룹 레벨 __ctrManaged 마커가 소유를 표시하지만 Codex hooks.json은 미지 필드 금지라 항목
// 레벨 추론뿐이므로, any-판정이면 사용자가 항목을 추가한 혼합 그룹까지 통째로 지워진다 —
// 혼합 그룹은 불가침(파손 금지 > 멱등 완전성, 잔존 정리는 사용자 /hooks 몫).
func isOurCodexGroup(raw json.RawMessage) bool {
	var p codexGroupProbe
	if json.Unmarshal(raw, &p) != nil {
		return false
	}
	if len(p.Hooks) == 0 {
		return false
	}
	for _, h := range p.Hooks {
		if !isCodexHookCommandToken(h.Command) || !strings.HasPrefix(h.StatusMessage, hookMarkerPrefix()) {
			return false
		}
	}
	return true
}

// buildCodexHookCommand — buildHookCommand의 Codex 형제: 러닝 서브커맨드가 `codex-hook`이다
// (D35 호스트 경계 + §11.2 F3 버전 게이트). 인용 규칙은 T11 관례 승계(가정 — Codex 훅 명령
// 파싱 규칙은 실측 전, 도그푸딩은 --store-root 미명시 경로만 사용).
func buildCodexHookCommand(storeRootExplicit bool, storeRootRaw string, noShadow bool) string {
	cmd := hookBinaryName + " codex-hook"
	if storeRootExplicit && storeRootRaw != "" {
		cmd += " --store-root '" + strings.ReplaceAll(storeRootRaw, "'", `'\''`) + "'"
	}
	if noShadow {
		cmd += " --no-shadow"
	}
	return cmd
}

// codexHooksPath — 설치 대상 hooks.json 경로(§11.1 G3). 기본 프로젝트 `<root>/.codex/hooks.json`,
// --user는 `~/.codex/hooks.json`. config.toml·[hooks.state]는 절대 건드리지 않는다(신뢰 승인
// 우회 금지).
func codexHooksPath(user bool, projectRoot string) (string, error) {
	if user {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("hook: 홈 디렉터리 해석 실패")
		}
		return filepath.Join(home, ".codex", "hooks.json"), nil
	}
	return filepath.Join(projectRoot, ".codex", "hooks.json"), nil
}

// mergeCodexHooks — mergeHookSettings의 Codex 형제(D28 원칙 승계: 멱등·타 항목/미지 키 raw
// 보존·제거 대칭·빈 컨테이너 정리). 등록 집합·그룹 타입·소유 판정만 다르다.
func mergeCodexHooks(existing []byte, command, marker string, install bool) ([]byte, error) {
	settings := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := json.Unmarshal(existing, &settings); err != nil {
			return nil, err
		}
		if settings == nil {
			settings = map[string]json.RawMessage{}
		}
	}
	hooks := map[string]json.RawMessage{}
	if raw, ok := settings["hooks"]; ok {
		if err := json.Unmarshal(raw, &hooks); err != nil {
			return nil, err
		}
		if hooks == nil {
			hooks = map[string]json.RawMessage{}
		}
	}
	for _, reg := range codexRegistrations {
		var arr []json.RawMessage
		if raw, ok := hooks[reg.event]; ok {
			if err := json.Unmarshal(raw, &arr); err != nil {
				return nil, err
			}
		}
		kept := make([]json.RawMessage, 0, len(arr)+1)
		for _, el := range arr {
			if isOurCodexGroup(el) {
				continue
			}
			kept = append(kept, el)
		}
		if install {
			b, err := json.Marshal(codexOwnedGroup{
				Matcher: reg.matcher,
				Hooks:   []codexHookCmd{{Type: "command", Command: command, Timeout: hookTimeoutSec, StatusMessage: marker}},
			})
			if err != nil {
				return nil, err
			}
			kept = append(kept, b)
		}
		if len(kept) == 0 {
			delete(hooks, reg.event)
			continue
		}
		b, err := json.Marshal(kept)
		if err != nil {
			return nil, err
		}
		hooks[reg.event] = b
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		b, err := json.Marshal(hooks)
		if err != nil {
			return nil, err
		}
		settings["hooks"] = b
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}
```

runHookInstall·runHookUninstall에 `--codex` Bool 플래그 추가, 분기:

```go
	codex := fs.Bool("codex", false, "Codex CLI hooks.json에 등록(기본: Claude settings.json)")
	...
	if *codex {
		path, err := codexHooksPath(*user, projectRoot)
		if err != nil {
			return err
		}
		if storeRootExplicit && storeRootRaw != "" {
			if abs, absErr := filepath.Abs(storeRootRaw); absErr == nil {
				storeRootRaw = abs
			}
		}
		command := buildCodexHookCommand(storeRootExplicit, storeRootRaw, *noShadow)
		existing, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("hook: 설정 파일 읽기 실패")
		}
		merged, err := mergeCodexHooks(existing, command, hookMarker(version), true)
		if err != nil {
			return fmt.Errorf("hook: 설정 병합 실패: %w", err)
		}
		if err := atomicWriteFile(path, merged); err != nil {
			return errors.New("hook install: 설정 쓰기 실패")
		}
		fmt.Fprintf(stdout, "hook install (codex): %d개 이벤트 등록 완료 — Codex에서 /hooks로 훅을 리뷰·신뢰해야 실행됩니다\n", len(codexRegistrations))
		return nil
	}
```

uninstall 분기는 대칭(`mergeCodexHooks(existing, "", "", false)` + "제거 완료" 출력; 파일 미존재는 no-op).

- [ ] **Step 4: 통과 확인** — Run: `go test -p 1 ./internal/cli/` → Expected: PASS(기존 Claude 설치 테스트 무회귀 포함). `gofumpt -l internal/cli/` → 출력 없음.

- [ ] **Step 5: 커밋**

```bash
git add internal/cli/hook_install.go internal/cli/hook_install_test.go
git commit -m "feat(v0.4): Codex hooks.json 설치 병합기 — D35 설치, statusMessage 마커(§11.1 G3)"
```

---

### Task 6: provenance source_kind 티어 (D37)

**Files:**
- Modify: `internal/search/search.go` (hitQuery ~L173-179)
- Modify: `internal/store/store.go` (sourceOf ~L571-586)
- Test: `internal/search/search_test.go`, `internal/store/store_test.go`

**Interfaces:**
- Consumes: 기존 스키마(sources.source_kind) — 저장 스키마 불변.
- Produces: 대표 source 선택 규약 — `ORDER BY (source_kind='hook') ASC, uri ASC`(두 질의 동시 — α6 search/fetch 표시 일치 유지).

- [ ] **Step 1: 실패 테스트 작성**:

(a) `internal/search/search_test.go` — `TestQuery_HitSourceShadowCoexist`(L503~) 셋업 관례를 미러해 그 뒤에 추가:

```go
// D37 — 사전순으로 명시 티어보다 앞서는 hook URI가 대표가 되지 않는다(kind-티어 우선).
// 기존 coexist 테스트(구 inline·신 shadow 둘 다 kind=hook 동티어 — inline 우선)는 무수정
// 유지가 계약이다. 셋업은 그 테스트의 Register 관례 그대로.
func TestQuery_HitSourceKindTier(t *testing.T) {
	st, err := store.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	reg := func(uri, kind, body string) {
		t.Helper()
		if _, err := st.Register(t.Context(), store.Registration{
			StoredBytes: []byte(body), MediaType: "text/plain", Redaction: "none",
			Source: store.SourceMeta{URI: uri, Kind: kind, SrcHash: "h"},
			Chunks: []store.Chunk{{
				Ordinal: 0, ByteStart: 0, ByteEnd: int64(len(body)),
				LineStart: 1, LineEnd: 1, Text: body,
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	body := "tierneedle content"
	reg("inline:AAA", "hook", body)   // hook 티어 — uri 사전순 선행(사전식 우연 재현)
	reg("inline:ZZZ", "inline", body) // 명시 티어 — 같은 본문 → 같은 artifact, 소스 2행
	res, err := Query(t.Context(), st, "", []string{"tierneedle"}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res[0].Hits) != 1 || res[0].Hits[0].Source != "inline:ZZZ" {
		t.Fatalf("want inline:ZZZ (kind-티어 우선 — hook 최하위), got %+v", res[0].Hits)
	}
}
```

(b) `internal/store/store_test.go` — sourceOf 경로(fetch 표시)도 동일 규약 단정:

```go
// D37 — sourceOf(fetch 표시 경로)도 kind-티어 우선(α6: search/fetch 대표 일치).
func TestReadRangeSourceKindTier(t *testing.T) {
	s, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	body := "tier body\n"
	reg := func(uri, kind string) int64 {
		t.Helper()
		id, err := s.Register(t.Context(), Registration{
			StoredBytes: []byte(body), MediaType: "text/plain", Redaction: "none",
			Source: SourceMeta{URI: uri, Kind: kind, SrcHash: "h"},
			Chunks: []Chunk{{
				Ordinal: 0, ByteStart: 0, ByteEnd: int64(len(body)),
				LineStart: 1, LineEnd: 1, Text: body,
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	id := reg("inline:AAA", "hook") // uri 사전순 선행 hook 행
	if id2 := reg("inline:ZZZ", "inline"); id2 != id {
		t.Fatalf("동일 content인데 artifact 분리: %d != %d", id, id2)
	}
	r, err := s.ReadRange(t.Context(), id, Selector{Kind: "line", LineStart: 1, LineEnd: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !r.HasSource || r.Source.URI != "inline:ZZZ" || r.Source.Kind == "hook" {
		t.Fatalf("want inline:ZZZ(비-hook 티어), got %+v", r.Source)
	}
}
```

- [ ] **Step 2: 실패 확인** — Run: `go test -p 1 ./internal/search/ ./internal/store/ -run 'KindTier'` → Expected: FAIL(대표가 uri 사전순 hook 행).

- [ ] **Step 3: 구현**:

(a) `internal/search/search.go` hitQuery 교체(주석 포함):

```go
// hitQuery: chunks→artifacts→sources를 artifact_id로 조인한다. 한 artifact에 소스가
// 여러 개면 D37 kind-티어 우선(명시 ingest > hook 패시브), 티어 내 uri 오름차순 첫 행을
// 결정적으로 고른다 — store.sourceOf와 동일 순서(α6, 다중 소스 artifact에서 search/fetch
// 표시 일치). SQLite에서 (expr) ASC는 false(0) 먼저 — 비-hook 티어가 선행한다.
const hitQuery = `SELECT c.artifact_id, c.line_start, c.line_end, c.text, a.redaction, s.uri, s.source_kind,
	s.src_size, s.src_mtime_ns, s.src_hash, s.extraction
	FROM chunks c JOIN artifacts a ON a.id = c.artifact_id JOIN sources s ON s.artifact_id = a.id
	WHERE c.id = ? ORDER BY (s.source_kind = 'hook') ASC, s.uri ASC LIMIT 1`
```

(b) `internal/store/store.go` sourceOf 교체(주석 포함):

```go
// sourceOf: artifactID의 sources 중 대표 1행 — D37 kind-티어 우선(명시 ingest > hook 패시브),
// 티어 내 uri ASC(search.hitQuery와 동일 순서 — α6). 없으면 ok=false.
func (s *Store) sourceOf(artifactID int64) (SourceInfo, bool) {
	...
	err := s.reader.QueryRow(`SELECT uri,source_kind,src_size,src_mtime_ns,src_hash,extraction
		FROM sources WHERE artifact_id=? ORDER BY (source_kind = 'hook') ASC, uri ASC LIMIT 1`, artifactID).
	...
}
```

- [ ] **Step 4: 통과 확인** — Run: `go test -p 1 ./internal/search/ ./internal/store/` → Expected: PASS — **기존 `TestQuery_HitSourceShadowCoexist` 무수정 GREEN이 계약**(구 inline·신 shadow 둘 다 kind=hook 동티어 → 표시 불변). `gofumpt -l internal/` → 출력 없음.

- [ ] **Step 5: 커밋**

```bash
git add internal/search/search.go internal/search/search_test.go internal/store/store.go internal/store/store_test.go
git commit -m "feat(v0.4): provenance source_kind 티어 — D37, hitQuery·sourceOf 동시 교체(α6)"
```

---

### Task 7: store 용량 임계 경고 (D38)

**Files:**
- Modify: `internal/cli/cli.go` (doctor [14] 렌더 분기 ~L1287-1293 + 헬퍼)
- Test: `internal/cli/cli_test.go` (TestRunDoctor_ContentDBSize ~L193 인접)

**Interfaces:**
- Consumes: 기존 `store.SizeStats`(sz.BlobBytes).
- Produces: doctor 조건부 경고 1줄 + `storeWarnBytes(getenv func(string) string) int64`(기본 100MiB, `CTR_SHADOW_WARN_BYTES` 양수만 채택).

- [ ] **Step 1: 실패 테스트 작성** — `internal/cli/cli_test.go`, TestRunDoctor_ContentDBSize 뒤:

```go
// D38 — blob 총량 > 임계면 [14] 뒤 경고 1줄(purge 비선택 성격 병기), 임계 미만이면 무출력.
// SizeStats 실패("없음") 경로는 else 분기 밖이라 경고 미평가가 구조로 보장된다(기존 테스트 커버).
// 셋업·호출은 TestRunDoctor_ContentDBSize 관례.
func doctorSizeWarnSetup(t *testing.T) (storeRoot, projectRoot string) {
	t.Helper()
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

func TestRunDoctor_StoreSizeWarn(t *testing.T) {
	storeRoot, projectRoot := doctorSizeWarnSetup(t)
	t.Setenv("CTR_SHADOW_WARN_BYTES", "5") // blob 10B > 5B → 발화
	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev"); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	for _, want := range []string{"[14] warning:", "purge", "무구분"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("out missing %q:\n%s", want, buf.String())
		}
	}
}

func TestRunDoctor_StoreSizeWarnSilentUnderThreshold(t *testing.T) {
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
```

- [ ] **Step 2: 실패 확인** — Run: `go test -p 1 ./internal/cli/ -run 'StoreSizeWarn'` → Expected: FAIL(경고 라인 미출력).

- [ ] **Step 3: 구현** — `internal/cli/cli.go`:

(a) 파일 상단 상수·헬퍼(가드 임계 관례와 동형). **`internal/cli/cli.go` 임포트 블록에 `"strconv"` 추가 필수** — 현재 cli.go에 strconv 사용처가 없어 미임포트 상태다(계획 리뷰 M1; gofumpt는 임포트를 자동 추가하지 않는다):

```go
// defaultStoreWarnBytes — D38 store 용량 경고 임계 기본값(설계 v0.4 §5 "기본 100MB").
const defaultStoreWarnBytes = 100 << 20 // 100MiB

// storeWarnBytes — CTR_SHADOW_WARN_BYTES 양수만 채택, 파싱 실패·비양수는 기본값(D38).
func storeWarnBytes(getenv func(string) string) int64 {
	if v, err := strconv.ParseInt(getenv("CTR_SHADOW_WARN_BYTES"), 10, 64); err == nil && v > 0 {
		return v
	}
	return defaultStoreWarnBytes
}
```

(b) [14] 정상 렌더 분기(else, ~L1291-1293)에 경고 추가:

```go
	} else {
		fmt.Fprintf(w, "[14] content.db: sources=%d artifacts=%d blob=%dB\n", sz.Sources, sz.Artifacts, sz.BlobBytes)
		// D38 — CAS 전체 blob 총량 경고(shadow 전용 아님 — [14] 측정 실체 그대로). 관측 채널이지
		// 정책 집행이 아니다(D27): 자동 삭제 없음. SizeStats 실패 경로는 이 분기 밖이라 미평가.
		if warn := storeWarnBytes(os.Getenv); sz.BlobBytes > warn {
			fmt.Fprintf(w, "[14] warning: blob %dB > 임계 %dB(CTR_SHADOW_WARN_BYTES) — 수동 구제는 purge 계열 CLI(현행 purge는 source_kind 무구분 삭제 — shadow만 선택 삭제 불가). 자동 삭제 없음\n", sz.BlobBytes, warn)
		}
	}
```

- [ ] **Step 4: 통과 확인** — Run: `go test -p 1 ./internal/cli/` → Expected: PASS(기존 [14] 정확-문자열 무회귀 포함). `gofumpt -l internal/cli/` → 출력 없음.

- [ ] **Step 5: 커밋**

```bash
git add internal/cli/cli.go internal/cli/cli_test.go
git commit -m "feat(v0.4): doctor store 용량 임계 경고 — D38, purge 비선택 성격 병기"
```

---

### Task 8: 버전 범프 + 재설치 + 실호스트 스모크 + 최종 이중 리뷰·CI·머지

**Files:**
- Modify: `cmd/context-router/main.go:29` (`version = "0.4.0"`), `internal/mcp/mcp.go:30` (`ServerVersion = "0.4.0"`)

- [ ] **Step 1: 범프** — 두 상수를 `"0.4.0"`으로 동시 교체(핀 테스트 `TestVersionPinnedToServerVersion`이 불일치를 잡는다).

- [ ] **Step 2: 전체 게이트** — Run: `go test -p 1 ./...` → 전부 PASS. `gofumpt -l .` → 출력 없음. `go vet ./...` → 무경고.

- [ ] **Step 3: 커밋**

```bash
git add cmd/context-router/main.go internal/mcp/mcp.go
git commit -m "chore(v0.4): 버전 범프 0.4.0"
```

- [ ] **Step 4: 로컬 빌드·재설치(컨트롤러 수행)** — `go build -o ctr-new.exe ./cmd/context-router` 후 기존 배포 위치의 실행 파일 교체(사용 중 잠금 시 서버/세션 종료 후). `context-router hook install`(Claude, marker 0.4.0 갱신 — doctor [9] 불일치 해소) + `context-router hook install --codex`(이 레포 `.codex/hooks.json` 생성).

- [ ] **Step 5: 실호스트 스모크(설계 §8 — 컨트롤러 수행·기록)**
  1. **PS deny**: 워크스페이스 내 300KB+ 파일을 PowerShell 도구로 `Get-Content <절대경로>` → deny + 안내, `ctr_search`로 현장 색인 확인.
  2. **PS 통과**: `Get-Content <같은 파일> -TotalCount 5`와 파이프 명령이 통과.
  3. **Codex cx: 등록**: Codex CLI를 이 레포에서 1턴 실행(대화형이면 `/hooks`에서 context-router 훅 신뢰 승인 후; 자동화 경로는 `--dangerously-bypass-hook-trust` — 자기 레포 자기 훅이라 취지 안전) → `context-router session export`(플래그는 `--help` 참조) 출력에 `cx:` 세션·이벤트 레코드 확인(**session.db 표면 기준** — usage 비대상 §1.2).
  4. **[14] 경고 무발화**: `context-router doctor` → `[14] content.db:` 행 존재 + `[14] warning` 부재(현 7.6MB < 100MiB).

- [ ] **Step 6: 최종 이중 리뷰** — 프로토콜(프로젝트 CLAUDE.md): 서브에이전트 리뷰어 + Codex `review --base <원장 BASE>` 병렬 → 발견 병합·수정(재리뷰는 서브에이전트만).

- [ ] **Step 7: PR·CI·머지** — `git push -u origin feat/v0.4-channel-expansion` → PR 생성(본문에 D34~D39 요약 + 스모크 결과) → CI 3-OS GREEN 확인 → 머지 → `git tag v0.4.0` → 세션 기록(docs/prompts) 작성·커밋.

---

## Self-Review 결과 (계획 작성 세션)

- 스펙 커버리지: D35(Task 0·4·5 + 스모크 ③) / D36(Task 1·2) / D37(Task 6) / D38(Task 7) / D39(Task 3) / §6 부채 편승(없음 확정 — Global) / §8 게이트(각 태스크 테스트 + Task 8 스모크·CI).
- 타입 일치: psDumpArg·psAbsPath(Task 1 정의 ← Task 2·3 소비), Host·Run 시그니처 + `codex-hook` 서브커맨드(Task 4 정의 ← Task 5 명령 문자열 `context-router codex-hook`), mergeCodexHooks 시그니처(Task 5 내 일관).
- 순서 의존: Task 2·3 → Task 1 필요. Task 4가 Run 시그니처를 바꾸므로 Task 2·3의 테스트 헬퍼는 Task 4 Step 4(d)에서 일괄 갱신 — Task 4를 Task 2·3보다 먼저 실행하지 말 것.

## 계획 체크포인트 이중 적대 검수 반영 (2026-07-21)

서브에이전트(opus) C1·M1~M4 + Codex adversarial-review 1패스(high 4·medium 2) 병렬 수행,
발견 병합·판정 후 본 계획에 반영 완료 — 처리 기록의 정본은 설계서 §11.2. 요지: 수렴 2건
(픽스처 UUID 통일 C1/F5, Run 호출부·임포트 전수 M1·M2/F6) 반영; Codex 고유 채택 3건
(F3 → `codex-hook` 전용 서브커맨드 = 구버전 오귀속 차단 버전 게이트, F4 → 전건 소유
판정 + 혼합 그룹 불가침 + 바이트 멱등 테스트, F2 부분 → D39 ToSlash+Clean 정규화);
기각 F1(별칭 재정의 — D32 동일 클래스 기수용)·F2 잔여(대소문자·symlink·provider —
기존 표면과 동일 + 안전 방향).
