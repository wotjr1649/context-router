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
- **`git add -A` 금지** — 이 저장소 작업 트리에는 untracked 로컬 도그푸딩 설정
  (`.claude/settings.json`)이 있다. 커밋은 명시 경로 stage만, 커밋 전
  `git status --short`로 staged 목록 확인.
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

추가 단정 2건을 같은 테스트(또는 인접 신규 테스트)에 넣는다:
① `rep, _ := Run(...)`의 반환으로 `strings.HasSuffix(uri, rep.Hash)` — URI suffix가
**저장본** content_hash와 일치(raw srcHash를 잘못 쓰면 여기서 잡힌다). ② 기존
redact 테스트 니들(anchor: `grep -n "Redact" internal/ingest/ingest_test.go` —
분할 리터럴 관례 준수)을 hook kind로 투입해 raw≠stored인 입력에서도 suffix ==
`rep.Hash`.

- [ ] **Step 2: 실패 확인 + 기존 테스트 갱신 목록 확정**

Run: `go test -p 1 ./internal/ingest -run TestRunInline_HookShadowURI -v`
Expected: FAIL — `want 2 shadow rows, got 0` (현행은 `inline:Bash` 단일 행 clobber).

**기존 테스트 갱신(필수)**: `TestSourceKindHookFlows`(ingest_test.go:777 부근)는
hook 결과를 `uri="inline:Read"`로 조회한다 — D30 적용 즉시 깨진다. `Run` 반환
`rep.Hash`를 받아 `"shadow:Read:"+rep.Hash`로 조회하도록 갱신하고, 같은 테스트의
비-hook `inline:Other` 단정(787행 부근)은 **그대로 유지**한다.

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
	res, err := Query(t.Context(), st, "", []string{"needle"}, 3, 0) // 실 시그니처 6인자(limit, budgetBytes=0 무제한)
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
	res2, err := Query(t.Context(), st, "", []string{"needle2"}, 3, 0)
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
파일의 헬퍼·픽스처 관례를 재사용해 4케이스 추가). **유일한 RED 반전은 (a)다** —
현행 게이트는 리터럴 NUL과 escape-text를 **둘 다** 거부하므로(shadow.go:52),
"현행 저장·신코드 거부"인 입력은 존재하지 않는다. (b)(d)는 both-reject 컨트롤.

```go
// TestShadow_DecodeSniff: D31 — (a) NUL 이스케이프 시퀀스를 텍스트로 "논하는"
// 정상 콘텐츠(C2 FP 사례)는 이제 저장된다(유일한 동작 변화). (b) escape가 디코드
// 되어 leaf에 실 NUL이 생기는 객체 응답과 (d) 배열 leaf 속 NUL은 전후 모두 미저장
// (컨트롤 — 회귀 방지). (c) {stdout,stderr} 정상 객체는 저장. 니들의 escape
// 시퀀스는 소스에 그대로 두지 말고 반드시 런타임 조립(`+"\\"+"u0000"+`)한다 —
// 파일 편집 도구의 \uXXXX 디코드 함정으로 실 제어 바이트가 박히는 사고 방지.
func TestShadow_DecodeSniff(t *testing.T) {
	esc := "\\" + "u0000" // 런타임 조립 — 소스 바이트에는 escape 텍스트만 존재
	// (a) FP 사례: escape를 "논하는" 산문 — 저장 기대(RED: 현행은 부분문자열 검사로 거부).
	fpBody := `"discussing the ` + esc + ` escape in prose ` + strings.Repeat("pad ", 5000) + `"`
	// (b) stdout leaf가 escape를 담아 디코드 후 실 NUL — 전후 모두 미저장(컨트롤).
	nulBody := `{"stdout":"abc` + esc + `def` + strings.Repeat("pad ", 5000) + `","stderr":""}`
	// (c) 정상 객체 fixture — 저장 기대(전후 동일).
	okBody := `{"stdout":"` + strings.Repeat("line ", 5000) + `","stderr":""}`
	// (d) 배열 leaf 속 NUL — []any 재귀 누락을 잡는 게이트(설계 §8 D31).
	arrBody := `["ok` + strings.Repeat("pad ", 5000) + `","x` + esc + `y"]`
	// 각 body를 기존 shadowCapture 테스트 하네스로 투입하고 저장/미저장을
	// content.db 행 수·drops 부재로 단정한다(기존 케이스의 단정 방식 복사).
	_ = fpBody; _ = nulBody; _ = okBody; _ = arrBody // 단정은 기존 하네스 관례로
}
```

주의(함정 재발 방지): 위 `esc` 런타임 조립을 풀어 쓰지 말 것. 계획·소스 어디에도
실 제어 바이트가 들어가면 안 된다(파일이 binary 판정되고 Go 컴파일이 깨진다).
(b)의 raw JSON은 **출력 가능한 escape 텍스트**를 담고, Unmarshal이 그것을 leaf 내
실 NUL로 바꾼다 — 그 leaf를 전장 검사로 거부하는 것이 D31 경로다.

Run: `go test -p 1 ./internal/hook -run TestShadow_DecodeSniff -v`
Expected: FAIL — (a)만 역전(현행 escape-text 부분문자열 검사가 거부 → 신코드는
저장). (b)(c)(d)는 전후 동일 거동이어야 하며 RED 단계에서도 통과가 정상.

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
		b := []byte(leaf)
		// 전장 IndexByte가 필수다: NUL은 유효 UTF-8 코드포인트라 IsBinary의 utf8.Valid를
		// 통과하고, IsBinary의 NUL 탐색은 첫 8KiB뿐 — late-NUL(기존 회귀 테스트
		// TestShadowEscapedNULSkips의 20KB 뒤 니들)은 여기서만 잡힌다.
		if bytes.IndexByte(b, 0) != -1 || ingest.IsBinary(b) {
			return // leaf에 NUL·비텍스트 — 조용히 미저장(현행 관례 승계)
		}
	}
```

(`bytes` import는 그대로 유지된다 — 전장 NUL 검사가 계속 사용. 기존
`TestShadowEscapedNULSkips`는 수정 없이 GREEN 유지가 게이트다: escape 텍스트가
디코드되어 leaf 20KB 뒤 실 NUL이 되고, 전장 검사가 거부한다.)

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
- Produces: `bashDumpArg(command string) string`(어휘 판정 — 후보 경로 인자 또는 `""`),
  `dumpAbsPath(goos, arg string) string`(OS 절대경로 정규화 — goos 테스트 주입점),
  `guardBash(ctx, ad, in, dir, contentDir, worktreeRoot, getenv, stdout)`(guardRead 동형 시그니처),
  `denyTool(ctx, ad, in, dir, toolName, detail string, stdout)`(구 denyRead 일반화).

- [ ] **Step 1: 실패하는 테스트 작성** — `bashDumpArg`/`dumpAbsPath` 단위 테스트:

```go
// TestBashDumpArg: D32 어휘 판정 — 단일 단순 `cat <경로>`만 경로 인자를 반환하고
// 나머지는 전부 ""(allow). 오탐 deny 차단이 목적이므로 거부 케이스가 본론이다.
func TestBashDumpArg(t *testing.T) {
	cases := []struct{ cmd, want string }{
		{"cat /c/big/file.log", "/c/big/file.log"},
		{"cat C:/big/file.log", "C:/big/file.log"}, // 절대 여부는 dumpAbsPath 몫
		{"cat file.log", "file.log"},               // 어휘상 후보 — 절대 판정에서 탈락
		{"cat -n /c/f", ""},                        // 옵션 — 제외
		{"cat /c/a /c/b", ""},                      // 인자 2개 — 제외
		{"cat /c/f | head", ""},                    // 파이프 — 제외
		{"cat /c/f > /c/g", ""},                    // 리다이렉트 — 제외
		{"cat /c/f; ls", ""},                       // 체이닝 — 제외
		{"cat \"/c/f\"", ""},                       // 인용 — 제외(보수)
		{"cat /c/with\\ space", ""},                // 백슬래시 — 제외(bash가 소비)
		{"cat /c/f", ""},                      // NBSP — bash IFS와 달리 Fields가 쪼갬 → 비ASCII 전면 거부
		{"cat /c/a!b", "/c/a!b"},                   // ! — 비대화형 bash에서 리터럴, 허용
		{"cat /c/~backup", ""},                     // ~ 전면 배제 — 의도적 미탐(allow 편향)
		{"type /c/big/file.log", ""},               // bash type=명령 조회, 덤프 아님
		{"tac /c/f", ""},                           // cat 외 명령 — 제외
		{"", ""},
	}
	for _, c := range cases {
		if got := bashDumpArg(c.cmd); got != c.want {
			t.Fatalf("bashDumpArg(%q)=%q want %q", c.cmd, got, c.want)
		}
	}
}

// TestDumpAbsPath: OS별 절대경로 정규화 — goos 주입으로 양쪽 분기를 한 OS에서 검증.
func TestDumpAbsPath(t *testing.T) {
	cases := []struct{ goos, arg, want string }{
		{"windows", "/c/big/f.log", "c:/big/f.log"}, // MSYS → 드라이브형 변환
		{"windows", "C:/big/f.log", "C:/big/f.log"},
		{"windows", "file.log", ""},                 // 상대 — 제외
		{"windows", "/tmp/f", ""},                   // 드라이브 불명 — 제외(보수)
		{"linux", "/tmp/f", "/tmp/f"},
		{"linux", "C:/big/f.log", ""},               // Unix에선 상대경로 — 제외
		{"linux", "file.log", ""},
	}
	for _, c := range cases {
		if got := dumpAbsPath(c.goos, c.arg); got != c.want {
			t.Fatalf("dumpAbsPath(%q,%q)=%q want %q", c.goos, c.arg, got, c.want)
		}
	}
}
```

Run: `go test -p 1 ./internal/hook -run 'TestBashDumpArg|TestDumpAbsPath' -v`
Expected: FAIL — 두 함수 미정의.

- [ ] **Step 2: `bashDumpArg`/`dumpAbsPath` 구현** — hook.go의 guardRead 근처에 추가:

```go
// bashDumpArg — D32 어휘 판정: 명령이 "단일 단순 `cat <경로>`"일 때만 경로 인자를
// 반환한다(그 외 전부 "" = allow). 비ASCII·제어문자는 bash IFS와 strings.Fields의
// 분할 규칙이 달라(NBSP 등) 오판 여지가 있으므로 전면 판정 포기. 파서는 확신이
// 있을 때만 deny하고, 오동작의 최대 피해는 "가드 미발화"다(설계 v0.3 §4·§7).
// ponytail: ~·# 전면 배제는 경로 내 정당한 문자까지 놓치는 의도적 미탐(allow 편향)
// — 실측에서 미탐이 문제되면 위치 인지 파서로 승급.
func bashDumpArg(command string) string {
	for i := 0; i < len(command); i++ {
		if command[i] < 0x20 || command[i] > 0x7e {
			return ""
		}
	}
	if strings.ContainsAny(command, "|&;<>`$(){}*?[]'\"\\~#") {
		return ""
	}
	fields := strings.Fields(command)
	if len(fields) != 2 || fields[0] != "cat" || strings.HasPrefix(fields[1], "-") {
		return ""
	}
	return fields[1]
}

// dumpAbsPath — OS 절대경로 정규화(goos는 테스트 주입점, 실호출은 runtime.GOOS).
// Windows: MSYS 형태 /x/...를 x:/... 드라이브형으로 변환 후 드라이브형만 절대로
// 인정(Go의 경로 의미론에서 /c/x는 현재 드라이브 상대 — 잘못 stat하면 오파일
// 판정이라 제외). Unix: /-접두만 절대. 상대·불명은 전부 ""(allow).
func dumpAbsPath(goos, arg string) string {
	if goos == "windows" {
		if len(arg) >= 3 && arg[0] == '/' && arg[2] == '/' &&
			((arg[1] >= 'a' && arg[1] <= 'z') || (arg[1] >= 'A' && arg[1] <= 'Z')) {
			arg = string(arg[1]) + ":" + arg[2:]
		}
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

guardBash에서의 사용: `path := dumpAbsPath(runtime.GOOS, bashDumpArg(f.Command))`
(`runtime` import 추가 — Step 3 스케치에 이미 반영됨).

Run: `go test -p 1 ./internal/hook -run 'TestBashDumpArg|TestDumpAbsPath' -v` → PASS.

- [ ] **Step 3: denyRead → denyTool 일반화 + guardBash + dispatch**

`denyRead`의 시그니처를 `denyTool(ctx, ad, in, dir, toolName, detail string, stdout)`로
바꾸고 본문의 summary 조립을 `summaryLine(toolName, detail)`로, permissionDecisionReason
문구는 불변. 호출부: guardRead는 기존 문면 그대로
`denyTool(..., "Read", workspaceRel(in.CWD, f.FilePath)+" "+strconv.FormatInt(info.Size(), 10)+"B", stdout)`,
guardBash는 설계 §4 warning 계약(명령·파일·크기·안내 요지)대로
`denyTool(..., "Bash", "cat "+workspaceRel(in.CWD, path)+" "+strconv.FormatInt(info.Size(), 10)+"B — ctr_search/ctr_fetch", stdout)`.
(workspaceRel이 이미 상대화하므로 절대경로는 이벤트에 실리지 않는다 — 테스트로
Bash 라벨·명령어·안내 문구 포함과 절대경로 비포함을 단정.)

guardBash — guardRead 동형(4조건 공유):

```go
// guardBash — D32 Bash 단일파일 덤프 가드(guardRead의 형제, 설계 v0.3 §4). 정적
// 판정(bashDumpArg 어휘 + dumpAbsPath 절대경로) 성립 시에만 D25 4조건(임계 초과·
// 경계 내·denylist 아님·현장 인덱싱 성공)을 guardRead와 동일 경로로 판정하고
// deny한다. 그 외 전부 통과.
func guardBash(ctx context.Context, ad *session.AppendDB, in hookInput, dir, contentDir, worktreeRoot string, getenv func(string) string, stdout io.Writer) {
	var f struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(in.ToolInput, &f) != nil {
		return
	}
	path := dumpAbsPath(runtime.GOOS, bashDumpArg(f.Command))
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
대형 파일 단순 cat=deny(+warning 이벤트: `Bash`·`cat`·`ctr_search` 문구 포함,
절대경로 비포함) / 파이프 포함=allow / 상대경로=allow / 임계 미만=allow /
경계 밖=allow / **denylist 파일 cat=allow(artifact 0건)** / **content store lock
경합=allow + drops `guard-store`**(기존 Read 전용 lock 테스트 패턴 복제 —
Bash 분기의 fail-open 직접 커버). 테스트의 명령 조립은
`"cat " + filepath.ToSlash(bigFile)` — Windows `t.TempDir()`의 백슬래시가
어휘 판정에서 거부되지 않도록 슬래시형으로.

Run: `go test -p 1 ./internal/hook` → PASS.

- [ ] **Step 5: matcher 배선 + 설치 테스트 갱신**

`hook_install.go:36` `{"PreToolUse", "Read"}` → `{"PreToolUse", "Read|Bash"}`
(+ 28~30행 주석 갱신: "PreToolUse는 Read|Bash 정규식 매칭 — 관리 그룹 1개 유지가
merge의 동일-이벤트 상호 제거 함정을 회피한다, 설계 v0.3 §4").
matcher-값 단정은 `hook_install_test.go:122~123` **한 곳뿐** — 그 줄만
`"Read|Bash"`로 교체하고, **광범위 grep 일괄 치환 금지**(legacy uninstall
fixture의 `"Read"` 문자열은 구버전 설정 재현이므로 유지). 등록 수
`want 4`(99·142·212행)는 불변(레지스트리 여전히 4항목).

**업그레이드 재설치 테스트(신규, 설계 §8 D32 설치 게이트)**: v0.2 형태 settings
(marker `context-router/0.2.0` + PreToolUse matcher `"Read"` 그룹)를 seed →
`hook install` 재실행 → 단정: PreToolUse 관리 그룹 **1개**, matcher
`"Read|Bash"`, 총 4그룹, marker가 현재 버전으로 갱신.

Run: `go test -p 1 ./internal/cli` → PASS.

- [ ] **Step 6: 커밋**

```bash
git add internal/hook internal/cli
git commit -m "feat(v0.3): D32 Bash 단일파일 덤프 가드 — bashDumpArg/dumpAbsPath 정적 판정 + guardBash(4조건 공유) + matcher Read|Bash 단일 그룹"
```

---

### Task 4: D33a — doctor: drops 사유 롤업 + [9] 마커 버전 + [14] content.db 규모

**Files:**
- Modify: `internal/cli/hook_install.go` (countDropsLog 390행 부근 → 사유 맵 버전 추가)
- Modify: `internal/cli/cli.go` (doctor [9] 1229행 부근, [12] 1240~1246행, [13] 뒤 [14] 신설, runDoctor 시그니처)
- Modify: `internal/store/store.go` (규모 조회 헬퍼 — 기존 Reader() 쿼리로 충분하면 cli 측 쿼리로 대체 가능, 구현자가 기존 관례에 맞춰 선택)
- Test: `internal/cli/hook_install_test.go`, **`internal/cli/cli_test.go`**
  (runDoctor 직접 호출 11곳 — cli_test.go:133·181·1044·1072·1093·1229·1261·1296 +
  hook_install_test.go:407·449·482 — 시그니처 변경 시 전부 갱신)

**Interfaces:**
- Produces: `dropsByReason(path string) (total int, reasons map[string]int)`
  (파싱 불가 줄은 `"unparsed"` 키), `formatDropCount(total int, reasons map[string]int) string`
  → `"5(a=2,b=3)"`(사유 알파벳순, reasons 비면 `"0"`), doctor `[14]` 행.

- [ ] **Step 1: 실패하는 테스트 작성** — 기존 doctor 테스트(hook_install_test.go
412~416행·456행 부근)의 정확-문자열 단정을 신형식으로 교체 + [14] 추가:

```go
// (기존 "[12] drops: store-root=2 worktree=3 total=5" 단정을 교체 — fixture가 심는
// 실제 사유는 hook_install_test.go:433·444의 root a,b / worktree x,y,z)
if !strings.Contains(out, "[12] drops: store-root=2(a=1,b=1) worktree=3(x=1,y=1,z=1) total=5") {
	t.Fatalf("out missing reason-rollup drops line:\n%s", out)
}
// (412~416행 헤더 리스트 단정에 추가 — 이 서브테스트는 store 없는 unregistered
// 경로이므로 [14]는 fail-soft "없음" 라인으로 방출되어야 한다)
"[14] content.db:",
```

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
		total++ // 빈 줄도 센다 — 기존 countDropsLog의 total 의미 보존(줄 수 계약)
		if _, reason, ok := strings.Cut(sc.Text(), "\t"); ok && reason != "" {
			reasons[reason]++
		} else {
			reasons["unparsed"]++ // 빈 줄·탭 없음·사유 없음 전부 unparsed
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
**배선(필수)**: `cli.Run`(cli.go:39)은 version을 받지만 runDoctor(cli.go:1058)에
전달하지 않는다 — `runDoctor(..., version string)`로 확장하고 Run의 doctor 분기
(cli.go:47 부근)와 **직접 호출 11곳**(Files 참조)을 전부 갱신한다. 테스트:
marker 일치(현재 버전으로 install한 settings)와 불일치(구버전 마커를 seed)
각 1케이스.

[14] — [13] 출력 뒤:

```go
	// [14] content.db 규모 — shadow 성장 관측 채널(설계 v0.3 §2 보존·D33). 정보성 —
	// store 부재·열기 실패는 "없음"으로 fail-soft(unregistered 경로에서도 행은 방출).
	// content.db 경로는 [3] 검사와 동일 이음새로 도출한다(runDoctor 상단의 기존
	// content.db 경로 조립 재사용). blob 바이트는 물리 파일 합산 — 프로덕션 경로
	// 헬퍼는 없으므로 artifacts CAS 디렉터리(artifacts/<hex 2자 prefix>/<64-hex>)를
	// filepath.WalkDir로 걷고 임시 파일(tmp 접두/접미)은 제외한다. store_test.go
	// writeAgedOrphanBlob(:1349)의 경로 조립은 레이아웃 참고용(테스트 헬퍼 — 재사용
	// 불가).
	fmt.Fprintf(w, "[14] content.db: sources=%d artifacts=%d blob=%dB\n", nSources, nArtifacts, blobBytes)
	// (부재 시) fmt.Fprintln(w, "[14] content.db: 없음")
```

테스트: raw blob 2개(그중 1쌍은 동일 콘텐츠 dedup) fixture로 sources·artifacts·
blob 바이트 **정확값** 단정 — 전부 0이거나 DB 크기만 합산하는 오구현을 걸러낸다.

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
	// 기본 출력 불변은 byte-for-byte로 고정한다 — "TOTAL: 부재"만 검사하면 헤더·열
	// 순서·행 형식 회귀를 놓친다(설계 §8 게이트). wantPlain은 fixture에서 계산한
	// 기대 전문(기존 usage 테스트의 기대 문자열 조립 관례).
	if outPlain != wantPlain {
		t.Fatalf("무플래그 출력 회귀:\n got %q\nwant %q", outPlain, wantPlain)
	}
	outTotals := runUsageForTest(t /*, ... , "--totals"*/)
	if !strings.HasPrefix(outTotals, wantPlain) {
		t.Fatalf("--totals 출력의 본표 prefix가 기본 출력과 다름:\n%s", outTotals)
	}
	for _, want := range []string{"TOTAL:hooks:on\t", "TOTAL:hooks:off\t"} {
		if !strings.Contains(outTotals[len(wantPlain):], want) {
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
`readDrops` 관례로, 아직 단정되지 않은 사유 경로를 커버. **grep이 정본이다**
(anchor: `grep -n "appendDrop(" internal/hook/*.go`로 전 사유 나열 →
각 사유 문자열을 `internal/hook/*_test.go`에서 검색해 미커버 확정):
사전 조사 기준 `bad-input`·`guard-store`·`shadow-store`는 이미 커버됨
(hook_test.go:215·1012·819) — 실미커버는 `shadow-ingest`(+grep이 더 찾으면 그것도).
해당 실패를 유발하는 기존 테스트 패턴을 복사해 drops 한 줄 단정.

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
- [ ] **Step 3: T5** — `matched_pattern` attr 방출 추가. **anchor: `internal/hook/hook.go`의
`bashClassifiers`/`classify` 반환부**(internal 코드에는 `matched_pattern` 문자열이
아직 없다 — 설계 문서에만 있음). classifier가 매칭한 패턴을 안정 enum 문자열로
이벤트 attr에 방출 + 단정 1개.
- [ ] **Step 4: T7** — guardRead offset-단독·limit-단독 통과 케이스 2개(anchor:
hook_test.go의 기존 부분 읽기 케이스 옆).
- [ ] **Step 5: T10b** — UPDATE_GOLDEN 가드·fan-out 알파벳 절단. **anchor:
`internal/session/export_test.go`(UPDATE_GOLDEN — `cmd/`가 아니다) +
`internal/session/summary.go`(fan-out 절단 지점)**. golden 재생성 가드 + 절단
케이스 각 1개.
- [ ] **Step 6: C4** — SessionStart source 절단을 byte-slice → `truncateUTF8`로.
**anchor: `internal/hook/hook.go:120`의 `src[:maxSourceBytes]`**(상수는
hook.go:52 `maxSourceBytes=64`; `truncateUTF8`은 227행 기존 함수 재사용) +
멀티바이트 경계 니들 테스트 1개.
- [ ] **Step 7: GREEN + 커밋**

Run: `go test -p 1 ./...` → 전체 PASS (여기서만 전체 스위트 1회)

```bash
git status --short   # internal/ 밖 의도치 않은 변경·untracked(.claude/ 등) 확인
git add internal
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
git status --short   # .claude/ 등 로컬 도그푸딩 설정이 staged되지 않게 확인
git add cmd internal
git commit -m "chore: 버전 0.3.0 범프"
```

- [ ] **Step 2: 재빌드·재설치** — `go install ./cmd/context-router` →
`context-router hook install`(이 저장소, 프로젝트 범위) → settings의
`__ctrManaged` marker `context-router/0.3.0` + PreToolUse matcher `Read|Bash` 확인.

- [ ] **Step 3: 실호스트 스모크** — 새 headless 세션(`claude -p --model haiku`,
Task 0 하네스 재사용)에서: ① 대형 파일 `cat` deny 발화 + warning 이벤트 ②
`{stdout,stderr}` 대용량 출력이 `shadow:Bash:<hash>` URI로 색인 ③ doctor 신행
([12] 롤업·[9] marker 0.3.0·[14] 규모) ④ `usage --totals` 동작. 결과를 ledger와
session-12 기록에 남긴다.

- [ ] **Step 4: CI 3-OS 게이트 + 머지 준비** — PR 생성 후 **Ubuntu·Windows·macOS
matrix가 전부 GREEN일 때만** 머지 준비 완료로 판정한다(설계 §8 — 특히 D32는
OS별 경로 의미론이 달라 로컬 GREEN만으로 회귀를 배제할 수 없다). 이후
superpowers:finishing-a-development-branch로 머지(최종 whole-branch 이중 리뷰는
표준 프로토콜: 서브에이전트 + Codex `review --base <branch-point>` 병렬 1패스).
