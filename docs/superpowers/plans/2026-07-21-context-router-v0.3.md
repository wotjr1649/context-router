# Context Router v0.3 Implementation Plan (강제 채널 완성 + 신뢰성)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 설계서 `docs/context-router-design-v0.3-ko.md`(D29~D33)의 v0.3 —
shadow URI 콘텐츠 해시 키, 재귀 leaf decode-sniff, Bash 단일파일 덤프 가드,
계측 가시성(doctor/usage), 부채 편승 7건 — 을 구현·검증·태그한다.

**Architecture:** 전부 기존 단일 바이너리 안의 증분 — ingest(runInline URI
분기), hook(shadow sniff 교체·guardBash 신설·dispatch 라우팅), cli(doctor
롤업·usage --totals·hook install matcher). 새 파일 없음, 새 의존성 없음.

**Tech Stack:** Go 1.24+, SQLite(FTS5), 표준 라이브러리만.

## Global Constraints

- 테스트는 반드시 `go test -p 1 ./...`(메모리 캡 규율 — 병렬 금지). 패키지
  단위 실행도 `-p 1` 유지.
- gofumpt 포맷(CI lint gate). 파일은 UTF-8(no BOM)·LF.
- 시크릿 카나리아는 런타임 분할 리터럴(`"xox"+"b-..."`)로만 조립(푸시 보호).
- 테스트 대용량 데이터는 `strings.Repeat`/testdata로 조립(전체 파일 재작성
  금지 — 응답 분할 규율).
- 브랜치: `feat/v0.3-reliability` (Task 1 시작 전 main에서 생성). 리뷰 BASE는
  SDD ledger의 태스크별 기록 기준(`HEAD~1` 금지).
- 소스 주석은 한국어, 기존 밀도·어조 유지. `ponytail:` 주석 관례 유지.
- 설계서 문면과 어긋나는 발견 시: 구현을 설계에 맞추지 말고 **중단 후 보고**
  (T0-style verify-then-halt).

---

### Task 0: [plan 검증] matcher `Read|Bash` 실호스트 프리체크 (설계 §9 ⓪)

코드 변경 없음. 설계 §4 배선의 구조적 전제(호스트 matcher 정규식 alternation)를
실호스트에서 확인한다. 실패 시 이 계획의 Task 3은 무효 — **중단 후 보고**
(폴백: 설계 §9 ⓪ — merge를 (event,matcher) 키로 확장해 그룹 2개 등록).

**Files:** 없음 (스크래치 디렉터리에서만 작업)

- [ ] **Step 1: 스크래치 프로젝트 + 캡처 훅 settings 작성**

스크래치 디렉터리(예: `C:/tmp/ctr-matcher-check`)에 `.claude/settings.json`:

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Read|Bash",
        "hooks": [{"type": "command", "command": "cat >> 'C:/tmp/ctr-matcher-check/cap-pre.log'", "timeout": 10}]
      }
    ]
  }
}
```

(훅 명령은 Windows에서도 POSIX sh 파싱 — T11 실측. 절대경로는 홑따옴표.)

- [ ] **Step 2: headless 세션으로 Read·Bash 각 1회 유발**

스크래치 디렉터리에서 실행 (session-11 §5 하네스 — deny-listed 명령 회피):

```bash
cd /c/tmp/ctr-matcher-check
echo hello > probe.txt
claude -p "Read the file probe.txt, then run this exact bash command: seq 3" \
  --model haiku --output-format json \
  --allowedTools 'Read' 'Bash(seq:*)'
```

- [ ] **Step 3: 캡처 판정**

Run: `grep -o '"tool_name":"[A-Za-z]*"' cap-pre.log | sort -u`
Expected: `"tool_name":"Bash"`와 `"tool_name":"Read"` 두 줄 모두 존재.
둘 다 있으면 전제 성립 → Task 1 진행. 하나라도 없으면 **중단·보고**(폴백 발동
여부는 컨트롤러 결정). 판정 결과를 SDD ledger에 기록. 스크래치 정리는 수동.

---

### Task 1: D30 — shadow URI 콘텐츠 해시 키 (runInline 분기)

**Files:**
- Modify: `internal/ingest/ingest.go` (runInline, 656~696행 부근)
- Test: `internal/ingest/ingest_test.go`, `internal/search/search_test.go`

**Interfaces:**
- Consumes: `store.Registration`/`SourceMeta`(기존), `Redact`, `ChunkText`.
- Produces: URI 규약 — `SourceKind=="hook"`이면
  `shadow:<Title>:<content_hash 64-hex>`, 그 외 인라인은 기존
  `inline:<Title>` 불변. `Report.Hash` = content_hash(기존 의미 불변).

- [ ] **Step 1: 실패하는 테스트 작성** — `internal/ingest/ingest_test.go`에 추가:

```go
// TestRunInline_HookShadowURI: D30 — SourceKind "hook"은 shadow:<Title>:<content_hash>
// 키로 저장되어 상이 콘텐츠가 서로를 덮지 않고(2행), 동일 콘텐츠 재등장은 같은 URI
// 갱신(여전히 2행)이어야 한다. 비-hook 인라인은 기존 inline:<Title> 불변.
func TestRunInline_HookShadowURI(t *testing.T) {
	st := newTestStore(t) // 파일 상단 기존 헬퍼 관례를 따를 것(없으면 store.Open(t.TempDir(), false))
	ctx := t.Context()
	big := strings.Repeat("alpha ", 100)
	if _, err := Run(ctx, st, "", nil, Request{Content: big, Title: "Bash", SourceKind: "hook"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, st, "", nil, Request{Content: big + "beta", Title: "Bash", SourceKind: "hook"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, st, "", nil, Request{Content: big, Title: "Bash", SourceKind: "hook"}); err != nil {
		t.Fatal(err) // 동일 콘텐츠 재등장 — 행 수 불변이어야 함
	}
	rows, err := st.Reader().Query("SELECT uri FROM sources WHERE uri LIKE 'shadow:Bash:%' ORDER BY uri")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var uris []string
	for rows.Next() {
		var u string
		if rows.Scan(&u) == nil {
			uris = append(uris, u)
		}
	}
	if len(uris) != 2 {
		t.Fatalf("want 2 shadow rows(clobber 없음·dedup 유지), got %d: %v", len(uris), uris)
	}
	for _, u := range uris {
		if !regexp.MustCompile(`^shadow:Bash:[0-9a-f]{64}$`).MatchString(u) {
			t.Fatalf("URI 형식 위반: %q", u)
		}
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test -p 1 ./internal/ingest -run TestRunInline_HookShadowURI -v`
Expected: FAIL — `want 2 shadow rows, got 0` (현행은 `inline:Bash` 단일 행 clobber).

- [ ] **Step 3: 최소 구현** — `runInline`의 URI 조립·해시 중복 제거:

```go
	kind := req.SourceKind
	if kind == "" {
		kind = "inline"
	}
	// content_hash = redact 후 저장본 sha256(Register 계산값과 동일 규칙 — 결정적).
	csum := sha256.Sum256(stored)
	contentHash := hex.EncodeToString(csum[:])
	uri := "inline:" + req.Title
	if kind == "hook" {
		// D30: hook 패시브 색인 전용 네임스페이스 — 콘텐츠 주소 키라 상이 출력이
		// 서로를 덮지 않는다(설계 v0.3 §2). MCP inline: 규약은 불변.
		uri = "shadow:" + req.Title + ":" + contentHash
	}
	_, err := st.Register(ctx, store.Registration{
		StoredBytes: stored,
		MediaType:   mediaType,
		Redaction:   redaction,
		Source: store.SourceMeta{
			URI: uri, Kind: kind,
			Size: int64(len(raw)), SrcHash: srcHash,
		},
		Chunks: ChunkText(string(stored), md),
	})
	if err != nil {
		return Report{}, fmt.Errorf("ingest: run: %w", err)
	}
	return Report{Indexed: 1, BytesStored: int64(len(stored)), Hash: contentHash}, nil
```

(함수 말미의 기존 `csum` 재계산 블록은 위로 이동했으므로 삭제 — 이중 해시 제거.)

- [ ] **Step 4: 통과 확인 + 소비처 결정성 테스트 추가**

Run: `go test -p 1 ./internal/ingest -run TestRunInline_HookShadowURI -v` → PASS.

`internal/search/search_test.go`에 α6 확장 케이스 추가(기존
`TestQuery_HitSourceDeterministicMultiSource` 바로 아래, 동일 패턴 재사용):

```go
// TestQuery_HitSourceShadowCoexist: D30 — 같은 artifact에 구 inline: 행과 신규
// shadow: 행이 공존하면 uri ASC 규약대로 inline:이 결정적으로 표시된다(설계 v0.3
// §2 승계 한계 문서화 케이스). RelativizeSource는 shadow: URI를 무변형 통과.
func TestQuery_HitSourceShadowCoexist(t *testing.T) {
	st, err := store.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	body := "coexist needle content"
	reg := func(uri, kind string) {
		t.Helper()
		if _, err := st.Register(t.Context(), store.Registration{
			StoredBytes: []byte(body), MediaType: "text/plain", Redaction: "none",
			Source: store.SourceMeta{URI: uri, Kind: kind, SrcHash: "h"},
			Chunks: []store.Chunk{{Title: "t", Text: body}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	reg("shadow:Bash:"+strings.Repeat("ab", 32), "hook") // 역순 등록 — 삽입순 우연 배제
	reg("inline:Bash", "hook")
	res, err := Query(t.Context(), st, "", []string{"needle"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(res[0].Hits) != 1 || res[0].Hits[0].Source != "inline:Bash" {
		t.Fatalf("want inline:Bash (uri ASC 결정성), got %+v", res[0].Hits)
	}
	// shadow: 단독 artifact — Source가 shadow: URI 그대로(무변형 통과, §8 게이트
	// "RelativizeSource의 shadow: 통과" 직접 커버).
	only := "solo shadow needle2 body"
	reg2URI := "shadow:Grep:" + strings.Repeat("cd", 32)
	if _, err := st.Register(t.Context(), store.Registration{
		StoredBytes: []byte(only), MediaType: "text/plain", Redaction: "none",
		Source: store.SourceMeta{URI: reg2URI, Kind: "hook", SrcHash: "h2"},
		Chunks: []store.Chunk{{Title: "t2", Text: only}},
	}); err != nil {
		t.Fatal(err)
	}
	res2, err := Query(t.Context(), st, "", []string{"needle2"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2[0].Hits) != 1 || res2[0].Hits[0].Source != reg2URI {
		t.Fatalf("want %s verbatim (RelativizeSource 무변형), got %+v", reg2URI, res2[0].Hits)
	}
}
```

(Register/Query 시그니처·Chunk 필드명은 인접 기존 테스트의 실제 사용을 그대로
복사해 맞출 것 — 위 코드는 형태 스케치이며 컴파일 기준은 기존 테스트 관례다.)

Run: `go test -p 1 ./internal/search -run TestQuery_HitSourceShadowCoexist -v` → PASS
(구현 변경 없이 통과해야 정상 — 규약 승계 확인 케이스).

- [ ] **Step 5: T6 잔여 편승** — `internal/hook/shadow` 관련 테스트의 lock 파일명
하드코딩을 store 상수/헬퍼 참조로 교체(anchor: `grep -rn "session.lock\|content.lock" internal/hook/*_test.go`
중 리터럴 문자열 부분), `artifact_created` 이벤트의 ref 부착 단정(shadow 테스트에서
`artifact_created` payload에 `rep.Hash` 기반 ref가 실제 포함되는지 단정 1개 추가 —
anchor: `grep -n "artifact_created" internal/hook`).

- [ ] **Step 6: 패키지 GREEN + 커밋**

Run: `go test -p 1 ./internal/ingest ./internal/search ./internal/hook` → PASS

```bash
git add internal/ingest internal/search internal/hook
git commit -m "feat(v0.3): D30 shadow URI 콘텐츠 해시 키 — hook 전용 shadow:<tool>:<hash64> 네임스페이스, clobber 소멸 + α6 공존 결정성 케이스 + T6 잔여"
```

---

### Task 2: D31 — 재귀 string-leaf decode-sniff

**Files:**
- Modify: `internal/ingest/ingest.go` (`isBinary` → `IsBinary` export, 371~374행)
- Modify: `internal/hook/shadow.go` (47~54행 NUL 게이트 블록 교체)
- Test: `internal/hook/hook_test.go`(또는 shadow 전용 테스트 파일), `internal/ingest/ingest_test.go`

**Interfaces:**
- Consumes: `ingest.IsBinary(b []byte) bool` (이 태스크에서 export).
- Produces: shadow 게이트 신규 판정 — leaf 문자열 디코드 후 `IsBinary` 판정.
  게이트 순서 불변: OFF → MIN → MAX → denylist → **leaf sniff** → 저장.

- [ ] **Step 1: `isBinary` export** — `internal/ingest/ingest.go:374`의
`func isBinary(` → `func IsBinary(`, 패키지 내 호출처(anchor:
`grep -n "isBinary(" internal/ingest`)와 주석·테스트명을 함께 갱신(gofumpt 유지).
동작 변경 없음.

Run: `go test -p 1 ./internal/ingest` → PASS (rename만).

- [ ] **Step 2: 실패하는 테스트 작성** — shadow 게이트 테스트(기존 shadow 테스트
파일의 헬퍼·픽스처 관례를 재사용해 다음 3케이스 추가):

```go
// TestShadow_DecodeSniff: D31 — (a) NUL 이스케이프 시퀀스를 텍스트로 "논하는"
// 정상 콘텐츠(C2 FP 사례)는 이제 저장되고, (b) 디코드된 leaf에 실제 NUL이 있는
// 객체 응답은 미저장이며, (c) {stdout,stderr} 객체 fixture의 정상 대용량 출력은
// 저장된다. NUL leaf 니들은 소스에 리터럴 제어 바이트를 두지 않기 위해 런타임
// 조립한다(string(rune(0)) — Edit 도구 이스케이프 함정 회피).
func TestShadow_DecodeSniff(t *testing.T) {
	// (a) FP 사례: 본문이 백슬래시-u-0-0-0-0 텍스트를 담는 JSON 문자열 — 저장 기대.
	fpBody := `"discussing the ` + `\\` + `u0000 escape in prose ` + strings.Repeat("pad ", 5000) + `"`
	// (b) 실 NUL leaf: 디코드하면 leaf 안에 실제 NUL 바이트 — 미저장 기대.
	nulBody := `{"stdout":"abc` + ` ` + `def` + strings.Repeat("pad ", 5000) + `","stderr":""}`
	// (c) 정상 객체 fixture — 저장 기대.
	okBody := `{"stdout":"` + strings.Repeat("line ", 5000) + `","stderr":""}`
	// 각 body를 기존 shadowCapture 테스트 하네스로 투입하고 저장/미저장을
	// content.db 행 수·drops 부재로 단정한다(기존 케이스의 단정 방식 복사).
	_ = fpBody; _ = nulBody; _ = okBody // 실제 단정은 기존 하네스 관례로 작성
}
```

주의: (b)의 ` `은 **Go 소스의 이스케이프 문자열 리터럴**(백틱 아닌 큰따옴표
문자열 안 ` `)로 두면 컴파일 시 실 NUL이 되어 의도대로 leaf에 들어간다 —
단 파일 편집 도구로 쓸 때는 `"abc" + string(rune(0)) + "def"` 런타임 조립로
우회할 것(위 스케치의 문자열 연결 표기는 그 지시다).

Run: `go test -p 1 ./internal/hook -run TestShadow_DecodeSniff -v`
Expected: FAIL — (a)가 현행 부분문자열 검사에 걸려 미저장(C2 FP), (b)는 통과되어
저장됨(현행은 escape-text만 검사, leaf 디코드 안 함) — 둘 다 역전이어야 함.

- [ ] **Step 3: 구현** — `shadow.go` 47~54행 블록을 교체:

```go
	// D31 decode-sniff: body는 hook.Run 외부 파싱을 통과한 유효 JSON — 문자열 leaf를
	// 재귀 수집해 디코드된 실바이트로 판정한다(C2 부분문자열 검사의 FP 상한 제거).
	// 유효 JSON 전제에서 Unmarshal은 실패하지 않는다(실패는 직조립 입력 방어 — 조용히 스킵).
	var decoded any
	if json.Unmarshal(body, &decoded) != nil {
		return
	}
	for _, leaf := range stringLeaves(decoded, nil) {
		if ingest.IsBinary([]byte(leaf)) {
			return // leaf에 NUL·비텍스트 — 조용히 미저장(현행 관례 승계)
		}
	}
```

파일 하단에 헬퍼 추가:

```go
// stringLeaves — 디코드된 JSON 값에서 문자열 leaf를 전부 모은다(문자열·배열·객체
// 재귀, 그 외 스칼라 무시). D31 판정 전용 — 저장 바이트는 원문 직렬화 그대로다.
func stringLeaves(v any, acc []string) []string {
	switch t := v.(type) {
	case string:
		acc = append(acc, t)
	case []any:
		for _, e := range t {
			acc = stringLeaves(e, acc)
		}
	case map[string]any:
		for _, e := range t {
			acc = stringLeaves(e, acc)
		}
	}
	return acc
}
```

- [ ] **Step 4: 통과 확인 + canary 게이트 승계 확인**

Run: `go test -p 1 ./internal/hook -run TestShadow -v` → 전부 PASS
(기존 비밀 미색인 canary·denylist·oversize 케이스 포함 — denylist가 sniff보다
선행하는 순서가 깨지지 않았는지 기존 케이스로 확인).

- [ ] **Step 5: 커밋**

```bash
git add internal/ingest internal/hook
git commit -m "feat(v0.3): D31 재귀 leaf decode-sniff — C2 FP 상한 제거, ingest.IsBinary export 재사용"
```

---

### Task 3: D32 — Bash 단일파일 덤프 가드 + matcher 배선

**Files:**
- Modify: `internal/hook/hook.go` (PreToolUse dispatch 139~142행, guardRead 아래 guardBash 신설, denyRead → denyTool 일반화 212~231행)
- Modify: `internal/cli/hook_install.go` (hookRegistrations 36행 matcher)
- Test: `internal/hook/hook_test.go`, `internal/cli/hook_install_test.go`

**Interfaces:**
- Consumes: `guardReadMax`(동일 임계 재사용), `ingest.Run` 4조건 판정(guardRead 관례), `denyTool`.
- Produces: `bashDumpPath(command string) string`(판정 성립 시 절대경로, 불성립 `""`),
  `guardBash(ctx, ad, in, dir, contentDir, worktreeRoot, getenv, stdout)`(guardRead 동형 시그니처),
  `denyTool(ctx, ad, in, dir, toolName, filePath, size, stdout)`(구 denyRead 일반화).

- [ ] **Step 1: 실패하는 테스트 작성** — `bashDumpPath` 단위 테스트:

```go
// TestBashDumpPath: D32 정적 판정 — 단일 단순 `cat <절대경로>`만 경로를 반환하고
// 나머지는 전부 ""(allow). 오탐 deny 차단이 목적이므로 거부 케이스가 본론이다.
func TestBashDumpPath(t *testing.T) {
	cases := []struct{ cmd, want string }{
		{"cat /c/big/file.log", "/c/big/file.log"},
		{"cat C:/big/file.log", "C:/big/file.log"},
		{"cat file.log", ""},              // 상대경로 — cwd 불확정, 판정 제외(설계 §4)
		{"cat -n /c/f", ""},               // 옵션 — 제외
		{"cat /c/a /c/b", ""},             // 인자 2개 — 제외
		{"cat /c/f | head", ""},           // 파이프 — 제외
		{"cat /c/f > /c/g", ""},           // 리다이렉트 — 제외
		{"cat /c/f; ls", ""},              // 체이닝 — 제외
		{"cat \"/c/f\"", ""},              // 인용 — 제외(보수)
		{"cat /c/with\\ space", ""},       // 백슬래시 — 제외(bash가 소비, 정적 판정 불가)
		{"type /c/big/file.log", ""},      // bash type=명령 조회, 덤프 아님
		{"tac /c/f", ""},                  // cat 외 명령 — 제외
		{"", ""},
	}
	for _, c := range cases {
		if got := bashDumpPath(c.cmd); got != c.want {
			t.Fatalf("bashDumpPath(%q)=%q want %q", c.cmd, got, c.want)
		}
	}
}
```

Run: `go test -p 1 ./internal/hook -run TestBashDumpPath -v`
Expected: FAIL — `bashDumpPath` 미정의.

- [ ] **Step 2: `bashDumpPath` 구현** — hook.go의 guardRead 근처에 추가:

```go
// bashDumpPath — D32 정적 판정: 명령이 "단일 단순 `cat <절대경로>`"일 때만 그 경로를
// 반환한다(그 외 전부 "" = allow). 셸 메타문자·인용·백슬래시가 하나라도 있으면 정적
// 판정 불가로 본다 — 파서는 확신이 있을 때만 deny하고, 오동작의 최대 피해는 "가드
// 미발화"다(설계 v0.3 §4·§7). 상대경로는 훅 cwd와 도구 cwd 불일치 시 오파일 판정
// 위험이 있어 제외(절대경로 한정). Windows 드라이브 경로는 bash 관례상 슬래시형
// (C:/...)만 온다 — 백슬래시형은 bash가 소비하므로 정적 판정 대상이 아니다.
func bashDumpPath(command string) string {
	if strings.ContainsAny(command, "|&;<>`$(){}*?[]'\"\\\n\r\t~#") {
		return ""
	}
	fields := strings.Fields(command)
	if len(fields) != 2 || fields[0] != "cat" || strings.HasPrefix(fields[1], "-") {
		return ""
	}
	p := fields[1]
	driveAbs := len(p) > 2 && ((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) && p[1] == ':' && p[2] == '/'
	if !strings.HasPrefix(p, "/") && !driveAbs {
		return ""
	}
	return p
}
```

Run: `go test -p 1 ./internal/hook -run TestBashDumpPath -v` → PASS.

- [ ] **Step 3: denyRead → denyTool 일반화 + guardBash + dispatch**

`denyRead`의 시그니처를 `denyTool(ctx, ad, in, dir, toolName, filePath string, size int64, stdout)`로
바꾸고 본문의 `summaryLine("Read", ...)` → `summaryLine(toolName, ...)`,
permissionDecisionReason 문구는 불변. guardRead의 호출부를
`denyTool(ctx, ad, in, dir, "Read", f.FilePath, info.Size(), stdout)`로 갱신.

guardBash — guardRead 동형(4조건 공유):

```go
// guardBash — D32 Bash 단일파일 덤프 가드(guardRead의 형제, 설계 v0.3 §4). 정적
// 판정(bashDumpPath) 성립 시에만 D25 4조건(임계 초과·경계 내·denylist 아님·현장
// 인덱싱 성공)을 guardRead와 동일 경로로 판정하고 deny한다. 그 외 전부 통과.
func guardBash(ctx context.Context, ad *session.AppendDB, in hookInput, dir, contentDir, worktreeRoot string, getenv func(string) string, stdout io.Writer) {
	var f struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(in.ToolInput, &f) != nil {
		return
	}
	path := bashDumpPath(f.Command)
	if path == "" {
		return // 정적 판정 불가·비덤프 — 통과
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
	denyTool(ctx, ad, in, dir, "Bash", path, info.Size(), stdout)
}
```

dispatch(hook.go 139~142행)를 tool_name 라우팅으로:

```go
	if in.HookEventName == "PreToolUse" {
		switch in.ToolName {
		case "Read":
			guardRead(ctx, ad, in, dir, contentDir, worktreeRoot, getenv, stdout)
		case "Bash":
			guardBash(ctx, ad, in, dir, contentDir, worktreeRoot, getenv, stdout)
		}
		return
	}
```

- [ ] **Step 4: guardBash 통합 테스트** — 기존 guardRead 테스트 관례(대형 파일
fixture·deny JSON 단정·warning 이벤트 단정)를 복사해 케이스 추가:
대형 파일 단순 cat=deny(+warning 이벤트) / 파이프 포함=allow / 상대경로=allow /
임계 미만=allow / 경계 밖=allow. drops `guard-store` 경로는 기존 관례 승계.

Run: `go test -p 1 ./internal/hook` → PASS.

- [ ] **Step 5: matcher 배선 + 설치 테스트 갱신**

`hook_install.go:36` `{"PreToolUse", "Read"}` → `{"PreToolUse", "Read|Bash"}`
(+ 28~30행 주석 갱신: "PreToolUse는 Read|Bash 정규식 매칭 — 관리 그룹 1개 유지가
merge의 동일-이벤트 상호 제거 함정을 회피한다, 설계 v0.3 §4").
`hook_install_test.go`의 matcher 단정(anchor: `grep -n '"Read"' internal/cli/hook_install_test.go`)을
`"Read|Bash"`로 교체. 등록 수 `want 4`는 불변(레지스트리 여전히 4항목).

Run: `go test -p 1 ./internal/cli` → PASS.

- [ ] **Step 6: 커밋**

```bash
git add internal/hook internal/cli
git commit -m "feat(v0.3): D32 Bash 단일파일 덤프 가드 — bashDumpPath 정적 판정 + guardBash(4조건 공유) + matcher Read|Bash 단일 그룹"
```

---

### Task 4: D33a — doctor: drops 사유 롤업 + [9] 마커 버전 + [14] content.db 규모

**Files:**
- Modify: `internal/cli/hook_install.go` (countDropsLog 390행 부근 → 사유 맵 버전 추가)
- Modify: `internal/cli/cli.go` (doctor [9] 1229행 부근, [12] 1240~1246행, [13] 뒤 [14] 신설)
- Modify: `internal/store/store.go` (규모 조회 헬퍼 — 기존 Reader() 쿼리로 충분하면 cli 측 쿼리로 대체 가능, 구현자가 기존 관례에 맞춰 선택)
- Test: `internal/cli/hook_install_test.go`

**Interfaces:**
- Produces: `dropsByReason(path string) (total int, reasons map[string]int)`
  (파싱 불가 줄은 `"unparsed"` 키), `formatDropCount(total int, reasons map[string]int) string`
  → `"5(a=2,b=3)"`(사유 알파벳순, reasons 비면 `"0"`), doctor `[14]` 행.

- [ ] **Step 1: 실패하는 테스트 작성** — 기존 doctor 테스트(hook_install_test.go
412~416행·456행 부근)의 정확-문자열 단정을 신형식으로 교체 + [14] 추가:

```go
// (기존 "[12] drops: store-root=2 worktree=3 total=5" 단정을 교체)
if !strings.Contains(out, "[12] drops: store-root=2(x=1,y=1) worktree=3(unknown-session=3) total=5") {
	t.Fatalf("out missing reason-rollup drops line:\n%s", out)
}
// (헤더 리스트 단정에 추가)
"[14] content.db:",
```

(테스트가 심는 drops 사유는 기존 fixture의 실제 사유 문자열로 맞출 것 — 위 x/y는
자리표시가 아니라 "기존 fixture가 쓰는 사유명을 그대로 옮기라"는 지시다.)

Run: `go test -p 1 ./internal/cli -run TestDoctor -v` → FAIL (구형식 출력).

- [ ] **Step 2: 구현**

`hook_install.go` — countDropsLog 옆에:

```go
// dropsByReason — drops.log을 사유별로 집계한다. 줄 형식 "<ts>\t<reason>"(hook
// appendDrop 계약)이 아니면 "unparsed"로 센다(포맷 관용 — 진단은 절대 중단하지
// 않는다, 설계 v0.3 §5). 파일 부재·읽기 실패는 (0, nil) — 기존 countDropsLog와
// 동일한 fail-soft.
func dropsByReason(path string) (int, map[string]int) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil
	}
	defer func() { _ = f.Close() }()
	total, reasons := 0, map[string]int{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		total++
		if _, reason, ok := strings.Cut(line, "\t"); ok && reason != "" {
			reasons[reason]++
		} else {
			reasons["unparsed"]++
		}
	}
	return total, reasons
}

// formatDropCount — "N(사유=n,...)" 렌더(사유 알파벳순, 결정적). N==0이면 "0".
func formatDropCount(total int, reasons map[string]int) string {
	if total == 0 {
		return "0"
	}
	keys := make([]string, 0, len(reasons))
	for k := range reasons {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, reasons[k]))
	}
	return fmt.Sprintf("%d(%s)", total, strings.Join(parts, ","))
}
```

(기존 countDropsLog는 dropsByReason(total만 사용)으로 대체 가능하면 제거 —
남는 호출처가 없을 때만. import에 bufio·sort 추가 필요 여부 확인.)

cli.go [12]:

```go
	rootTotal, rootReasons := dropsByReason(filepath.Join(storeRoot, dropsFileName))
	wtTotal, wtReasons := 0, map[string]int(nil)
	if canon.ProjectID != "" && canon.WorktreeID != "" {
		wtTotal, wtReasons = dropsByReason(filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID, dropsFileName))
	}
	fmt.Fprintf(w, "[12] drops: store-root=%s worktree=%s total=%d\n",
		formatDropCount(rootTotal, rootReasons), formatDropCount(wtTotal, wtReasons), rootTotal+wtTotal)
```

[9] — hookScope 렌더에 마커 버전 부가: countRegisteredHooks 옆에 마커 버전 수집
헬퍼를 추가해(설정 JSON에서 `__ctrManaged` 값의 `hookBinaryName+"/"` 접두 뒤
버전을 수집 — isOurHookGroup 관례 재사용) `등록됨(4개, marker 0.2.0)` 형태로,
바이너리 버전과 다르면 `등록됨(4개, marker 0.2.0≠0.3.0 — hook install 재실행)`.
(doctor에 현재 version 문자열이 전달되는지 확인 — 없으면 Run 경유로 배선,
anchor: `grep -n "version" internal/cli/cli.go | head`.)

[14] — [13] 출력 뒤:

```go
	// [14] content.db 규모 — shadow 성장 관측 채널(설계 v0.3 §2 보존·D33). 정보성.
	// sources·artifacts는 COUNT 쿼리, blob 바이트는 CAS 디렉터리 파일 크기 합산
	// (orphan-GC가 걷는 것과 동일한 blob 경로 헬퍼 재사용 — anchor: store_test.go
	// writeAgedOrphanBlob의 경로 조립).
	fmt.Fprintf(w, "[14] content.db: sources=%d artifacts=%d blob=%dB\n", nSources, nArtifacts, blobBytes)
```

- [ ] **Step 3: 통과 확인 + 커밋**

Run: `go test -p 1 ./internal/cli ./internal/store` → PASS

```bash
git add internal/cli internal/store
git commit -m "feat(v0.3): D33a doctor — drops 사유 롤업·[9] 마커 버전 불일치 감지·[14] content.db 규모 행"
```

---

### Task 5: D33b — usage --totals + T4 drop-reason 테스트

**Files:**
- Modify: `internal/cli/cli.go` (runUsage 379~423행)
- Test: `internal/cli/usage_test.go`, `internal/hook/hook_test.go`

**Interfaces:**
- Produces: `usage --totals` 플래그 — 본표 뒤 `TOTAL:hooks:on`/`TOTAL:hooks:off`
  2행(토큰·records 그룹 합계, hooks 열은 그룹 라벨). 기본 출력 불변.

- [ ] **Step 1: 실패하는 테스트 작성** — usage_test.go 기존 하네스 재사용:

```go
// TestUsage_TotalsFlag: D33 — --totals가 그룹 합계 2행을 덧붙이고, 플래그 없으면
// 출력이 완전히 불변이어야 한다(행=세션 1:1 계약 유지, 이중 집계 방지).
// fixture 조립은 기존 usage_test.go 상단 하네스(transcript 파일 + cc: 세션 시드)를
// 그대로 재사용한다 — 아래 단정 값은 그 fixture의 실측 합계로 맞출 것.
func TestUsage_TotalsFlag(t *testing.T) {
	// (기존 하네스: hooks:on 1세션 + hooks:off 1세션 시드)
	outPlain := runUsageForTest(t /*, ... 하네스 인자*/)               // 플래그 없음
	if strings.Contains(outPlain, "TOTAL:") {
		t.Fatalf("무플래그 출력에 TOTAL 행이 있으면 안 됨:\n%s", outPlain)
	}
	outTotals := runUsageForTest(t /*, ... , "--totals"*/)
	for _, want := range []string{"TOTAL:hooks:on\t", "TOTAL:hooks:off\t"} {
		if !strings.Contains(outTotals, want) {
			t.Fatalf("--totals 출력에 %q 부재:\n%s", want, outTotals)
		}
	}
	// 합산 정확성: fixture의 세션 행 값을 파싱해 그룹별 기대 합계를 계산하고
	// TOTAL 행의 각 열과 일치 단정(열 인덱스 1~5: input/output/cache_read/
	// cache_creation/records).
}
```

(`runUsageForTest`는 기존 테스트가 runUsage를 감싸 쓰는 방식의 대명사 — 실제
이름·인자는 usage_test.go의 기존 호출 형태를 그대로 쓴다.)

Run: `go test -p 1 ./internal/cli -run TestUsage_TotalsFlag -v` → FAIL.

- [ ] **Step 2: 구현** — runUsage에:

```go
	totals := fs.Bool("totals", false, "hooks:on/off 그룹 집계 2행 추가(설계 v0.3 §5)")
	...
	var onSum, offSum usageSums
	var onN, offN int
	// (세션 행 출력 루프 안에서 그룹 누적)
	if hooks == "hooks:on" {
		onSum.add(s); onN++
	} else {
		offSum.add(s); offN++
	}
	// (루프 종료 후)
	if *totals {
		fmt.Fprintf(w, "TOTAL:hooks:on\t%d\t%d\t%d\t%d\t%d\thooks:on\n", onSum.input, onSum.output, onSum.cacheRead, onSum.cacheCreate, onSum.records)
		fmt.Fprintf(w, "TOTAL:hooks:off\t%d\t%d\t%d\t%d\t%d\thooks:off\n", offSum.input, offSum.output, offSum.cacheRead, offSum.cacheCreate, offSum.records)
	}
```

(`usageSums.add`가 없으면 필드별 가산 4~5줄로 추가. onN/offN은 현재 미표시 —
열 구조 불변 원칙. 세션 수가 필요해지면 별도 결정.)

- [ ] **Step 3: T4 drop-reason 테스트** — `internal/hook/hook_test.go`의 기존
`readDrops` 관례로, 아직 단정되지 않은 사유 경로를 커버(anchor:
`grep -n "appendDrop(" internal/hook/*.go`로 전 사유 나열 →
`grep -n "사유문자열" internal/hook/*_test.go`로 미커버 확인):
최소 `bad-input`·`guard-store`·`shadow-store`·`shadow-ingest` 중 미커버 전부.
각각 해당 실패를 유발하는 기존 테스트 패턴(손상 stdin·잠긴 store 등)을 복사해
drops 한 줄 단정.

- [ ] **Step 4: GREEN + 커밋**

Run: `go test -p 1 ./internal/cli ./internal/hook` → PASS

```bash
git add internal/cli internal/hook
git commit -m "feat(v0.3): D33b usage --totals 옵트인 + T4 drop-reason 미커버 사유 테스트"
```

---

### Task 6: 부채 편승 minors 웨이브 (설계 §6 잔여)

**Files:** 각 항목의 anchor로 특정(아래). 항목당 변경은 1~10줄 수준.

- [ ] **Step 1: T1** — `resumePublishFromTmp` stale 주석 정정(anchor:
`grep -rn "resumePublishFromTmp" internal/session` — 주석 문면을 현 동작에 맞게).
- [ ] **Step 2: T3** — mcp cap-test의 refs 계열 119B 가산 미행사 케이스 추가(anchor:
`grep -n "119" internal/mcp/*_test.go` 또는 cap 상수 테스트 — session-10 ⑪ 동결
문면대로 가산 행사 케이스 1개).
- [ ] **Step 3: T5** — `matched_pattern` attr 방출 추가(anchor:
`grep -rn "matched_pattern" internal docs` — 설계상 방출해야 하나 미방출인 지점).
- [ ] **Step 4: T7** — guardRead offset-단독·limit-단독 통과 케이스 2개(anchor:
hook_test.go의 기존 부분 읽기 케이스 옆).
- [ ] **Step 5: T10b** — UPDATE_GOLDEN 가드·fan-out 알파벳 절단(anchor:
`grep -rn "UPDATE_GOLDEN" cmd` — golden 재생성 가드 + 절단 케이스).
- [ ] **Step 6: C4** — SessionStart source 절단을 byte-slice → `truncateUTF8`로
(anchor: `grep -n "64" internal/hook/hook.go`의 source 절단 지점 — session-11 C4 nit).
- [ ] **Step 7: GREEN + 커밋**

Run: `go test -p 1 ./...` → 전체 PASS (여기서만 전체 스위트 1회)

```bash
git add -A
git commit -m "chore(v0.3): 부채 편승 minors — T1·T3·T5·T7·T10b·C4 (T6는 Task 1 편승)"
```

주의: 각 anchor가 실코드와 어긋나면(이미 해소·위치 상이) 해당 항목만 skip하고
사유를 ledger에 기록 — 무리한 적용 금지.

---

### Task 7: 버전 범프 + 실호스트 스모크 + 도그푸딩 재설치 (설계 §9 ⑥)

**Files:**
- Modify: `cmd/context-router/main.go`(version const), `internal/mcp` serverVersion
  (anchor: `grep -rn "0.2.0" cmd internal` — 두 지점)

- [ ] **Step 1: 버전 0.2.0 → 0.3.0** 두 지점 갱신 + 전체 GREEN 확인

Run: `go test -p 1 ./...` → PASS / `gofumpt -l .` → 출력 없음

```bash
git add -A && git commit -m "chore: 버전 0.3.0 범프"
```

- [ ] **Step 2: 재빌드·재설치** — `go install ./cmd/context-router` →
`context-router hook install`(이 저장소, 프로젝트 범위) → settings의
`__ctrManaged` marker `context-router/0.3.0` + PreToolUse matcher `Read|Bash` 확인.

- [ ] **Step 3: 실호스트 스모크** — 새 headless 세션(`claude -p --model haiku`,
Task 0 하네스 재사용)에서: ① 대형 파일 `cat` deny 발화 + warning 이벤트 ②
`{stdout,stderr}` 대용량 출력이 `shadow:Bash:<hash>` URI로 색인 ③ doctor 신행
([12] 롤업·[9] marker 0.3.0·[14] 규모) ④ `usage --totals` 동작. 결과를 ledger와
session-12 기록에 남긴다.

- [ ] **Step 4: 머지 준비** — superpowers:finishing-a-development-branch로 PR
(최종 whole-branch 이중 리뷰는 표준 프로토콜: 서브에이전트 + Codex
`review --base <branch-point>` 병렬 1패스).
