# v0.5 Store 수명주기 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** 설계 정본 `docs/context-router-design-v0.5-ko.md`(커밋 22eb061,
D40~D43 + 편승)를 구현한다 — shadow 귀속 집계(doctor [14]/[15]), hook 전용
선택 purge(CAS 실회수), 빈 세션 GC, drops 진단 필드, breadcrumb 제거,
v0.5.0.

**Architecture:** 기존 단일 바이너리 구조 유지. store 계층(SizeStat 확장·
선택 purge 작업)과 session 계층(빈 세션 GC·append 게이트)에 각 1개 신규
작업을 추가하고 CLI(doctor·purge)가 배선한다. 신규 인터페이스 정의 없음
(D13 반파편화 준수).

**Tech Stack:** Go(모듈 `github.com/wotjr1649/context-router`), SQLite
(mattn 계열 기존 드라이버), 표준 라이브러리만.

## Global Constraints

- **테스트 실행은 항상 `go test -p 1 ./...`** (메모리 캡 — 이 머신은
  페이지파일 의도적 비활성, 무거운 프로세스 동시 실행 금지).
- deny 단정 + 현장 색인 테스트는 `CTR_HOOK_DEADLINE_MS=60000` 주입
  (세션-17 F2 처방). 신규 훅 테스트도 동일.
- `git add -A` 금지 — 변경 파일만 명시 add (untracked `.claude/`·`.codex/`
  보호).
- 커밋 트레일러: `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
- 파일 출력 UTF-8(no BOM), LF.
- 응답 분할 규율: whole-file rewrite 금지, 긴 테스트 데이터는
  `strings.Repeat`/testdata.
- 신규 self-defined interface 금지(D13). 오류는 기존 단일점 매핑 경로 재사용.
- 각 태스크 시작 전 **Files의 "선행 읽기" 파일을 먼저 Read** — 이 계획의
  코드 블록은 설계 계약 기준이며, 기존 코드의 실제 시그니처·헬퍼에 맞춰
  조정한다(계약 자체는 불변).
- 브랜치: `feat/v0.5-store-lifecycle` (BASE = 계획 커밋 직후 main HEAD,
  레저에 기록 — `HEAD~1` 금지).

---

### Task 1: D43 — drops 진단 필드 + [12] 파서 완화

**Files:**
- Modify: `internal/hook/hook.go` (`appendDrop` 시그니처·전 호출점)
- Modify: `internal/hook/shadow.go` (shadow-oversize·shadow-denylist 호출점)
- Modify: `internal/cli/hook_install.go` (`dropsByReason`)
- Test: `internal/hook/hook_test.go`, `internal/cli/hook_install_test.go`
- 선행 읽기: `internal/hook/hook.go`(appendDrop·dispatch),
  `internal/cli/hook_install.go:630-660`(dropsByReason·isUnixTS),
  `internal/cli/hook_install_test.go:520-550`(기존 unparsed 단정)

**Interfaces:**
- Produces: drop 라인 신형식 `ts \t reason \t sid8 \t hook_event \t tool`
  (미상 필드 `-`, sanitize: 탭·개행 제거 + 각 필드 64자 상한).
  `dropsByReason`은 정확 2 또는 정확 5 탭필드만 수용(필드[1]=reason).
- Consumes: 없음 (독립 태스크).

- [ ] **Step 1: 실패 테스트 작성 — 신형식 기록**

`internal/hook/hook_test.go`에 추가(기존 readDrops 헬퍼 재사용):

```go
// TestDropLineDiagnosticFields — D43: unknown-session drop 라인이 5필드
// (ts, reason, sid8, hook_event, tool)를 기록한다. 미상 필드는 "-".
func TestDropLineDiagnosticFields(t *testing.T) {
	dir := t.TempDir()
	// 미지 세션의 PostToolUse를 발화시켜 unknown-session drop 생성
	// (기존 TestUnknownSessionDrop 계열의 입력 구성 재사용 — hook_test.go:279 참조)
	in := postToolUseInput(t, "cc:99999999-0000-7000-8000-000000000000", "Read")
	runDispatchForDrop(t, dir, in) // 기존 테스트 헬퍼 경로 재사용
	got := readDrops(t, dir)
	line := strings.TrimSpace(got)
	fields := strings.Split(line, "\t")
	if len(fields) != 5 {
		t.Fatalf("fields=%d want 5 (line=%q)", len(fields), line)
	}
	if fields[1] != "unknown-session" {
		t.Fatalf("reason=%q want unknown-session", fields[1])
	}
	if fields[2] != "cc:99999" { // 세션ID 앞 8자
		t.Fatalf("sid8=%q want cc:99999", fields[2])
	}
	if fields[3] != "PostToolUse" || fields[4] != "Read" {
		t.Fatalf("event/tool=%q/%q want PostToolUse/Read", fields[3], fields[4])
	}
}
```

주의(검수 정정 2건): ① `postToolUseInput`/`runDispatchForDrop`은 부재 —
기존 unknown-session 테스트(hook_test.go:279 부근)의 구성 코드를 따라
작성(새 파일 금지). ② **session_id 함정**: 입력 JSON의 `session_id`는
canonical UUID여야 하며(비정형은 dispatch 이전 `bad-session-id` drop —
hook.go:96), 호스트 접두(`cc:`)는 dispatch가 `string(host)+":"+SessionID`
로 부여한다(hook.go:111). 따라서 appendDrop에는 **접두된 external**을
전달해야 sid8이 `cc:99999`가 된다 — `in.SessionID`(맨 UUID)를 넘기면
`99999999`가 되어 단정 실패.

- [ ] **Step 2: 실행 — RED 확인**

Run: `go test -p 1 ./internal/hook -run TestDropLineDiagnosticFields -v`
Expected: FAIL (현행 2필드 라인 — fields=2)

- [ ] **Step 3: appendDrop 확장 구현**

`internal/hook/hook.go` — 시그니처 확장 + sanitize:

```go
// appendDrop — D43: 진단 5필드. sid8은 세션ID 앞 8자, 미상은 "-".
// sanitize: 탭·개행 제거 + 64자 상한(파서 오염 방지, 설계 §5).
func appendDrop(dir, reason, sessionID, hookEvent, tool string) {
	san := func(s string) string {
		if s == "" {
			return "-"
		}
		s = strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(s)
		if len(s) > 64 {
			s = s[:64]
		}
		return s
	}
	sid8 := "-"
	if sessionID != "" {
		sid8 = sessionID
		if len(sid8) > 8 {
			sid8 = sid8[:8]
		}
	}
	line := fmt.Sprintf("%d\t%s\t%s\t%s\t%s\n",
		time.Now().Unix(), san(reason), san(sid8), san(hookEvent), san(tool))
	// 이하 기존 append 쓰기 경로 유지
	...
}
```

전 호출점(unknown-session·bad-input·shadow-oversize·shadow-denylist 등)에
그 지점에서 가용한 sessionID·hookEvent·tool을 전달(없으면 "").

- [ ] **Step 4: GREEN 확인 + 기존 훅 테스트 회귀**

Run: `go test -p 1 ./internal/hook -v`
Expected: 전부 PASS (readDrops substring 단정은 신형식에서도 통과 — 설계 §5)

- [ ] **Step 5: 실패 테스트 — dropsByReason {2,5} 수용**

`internal/cli/hook_install_test.go`에 추가. **주의(검수 정정)**:
`dropsByReason`의 실제 입력은 문자열이 아니라 **파일 경로**다 — 기존
`TestDropsByReason_StrictParsing`(hook_install_test.go:528 부근)처럼 임시
파일에 기록 후 경로로 호출한다. 기존 3필드 unparsed 단정 테스트는
**무변경 유지**(설계 §5).

```go
// TestDropsByReasonFiveFields — D43: 정확 5필드 신형식 라인의 reason을
// 집계한다. 3·4필드는 여전히 unparsed(기존 단정 불변).
func TestDropsByReasonFiveFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "session.drops.log")
	data := "1700000000\tunknown-session\tcc:99999\tPostToolUse\tRead\n" +
		"1700000001\tshadow-oversize\t-\t-\t-\n" +
		"1700000002\tbroken\textra\n" // 3필드 → unparsed
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil { t.Fatal(err) }
	got := dropsByReason(p) // 기존 시그니처(경로 입력) 그대로
	if got["unknown-session"] != 1 || got["shadow-oversize"] != 1 {
		t.Fatalf("5필드 reason 집계 실패: %v", got)
	}
	if got["unparsed"] != 1 {
		t.Fatalf("3필드는 unparsed 유지: %v", got)
	}
}
```

- [ ] **Step 6: RED → dropsByReason 완화 구현 → GREEN**

Run: `go test -p 1 ./internal/cli -run TestDropsByReason -v` → FAIL 확인 후:

```go
// dropsByReason — D43: 정확 2필드(구) 또는 정확 5필드(신)만 수용.
// 그 외 필드 수는 unparsed(느슨 수용 금지 — 설계 §5).
fields := strings.Split(text, "\t")
ok := (len(fields) == 2 || len(fields) == 5) &&
	fields[1] != "" && isUnixTS(fields[0])
if !ok {
	reasons["unparsed"]++
	continue
}
reasons[fields[1]]++
```

Run: `go test -p 1 ./internal/cli -v` → 전부 PASS(기존 unparsed 단정 포함).

- [ ] **Step 7: 커밋**

```bash
git add internal/hook/hook.go internal/hook/shadow.go internal/hook/hook_test.go internal/cli/hook_install.go internal/cli/hook_install_test.go
git commit -m "feat(v0.5): D43 drops 진단 5필드 + [12] 파서 {2,5}필드 완화"
```

---

### Task 2: D40 — SizeStat 확장 (content_hash 귀속·물리 파일 합산)

**Files:**
- Modify: `internal/store/store.go` (SizeStat 계열 확장)
- Test: `internal/store/store_test.go`
- 선행 읽기: `internal/store/store.go:180-200`(스키마)·`store.go:900-985`
  (SizeStat·blob walk)·`store.go:400-460`(Register·raw_blob_hash 기입)

**Interfaces:**
- Produces(검수 정정 — 실코드 API에 고정): 실제 표면은 **구조체
  `type SizeStat`(단수) + 패키지 함수 `func SizeStats(dir string)
  (*SizeStat, error)`**(store.go:914·928 — content.db를 dir 기준 `mode=ro`
  로 새로 연다, live 핸들 아님). 여기에 필드 추가 —
  `FileBytes int64`(content.db os.Stat 크기),
  `ShadowOwnedBytes int64`(귀속 물리 바이트 합),
  `ShadowOwnedHashes int`(귀속 hash 수),
  `ShadowOwned map[string]int64`(**귀속 hash → 물리 CAS 파일 바이트** —
  Task 3b 접두 분해·Task 5a/5b 견적이 공용 소비하는 원천. 스칼라 2필드는
  이 맵의 합산·len과 항상 일치).
- 귀속 술어(설계 §2, 불변): hash h가 shadow 귀속 ⟺ h를 참조하는 모든
  artifact의 모든 source가 kind='hook' AND h를 raw_blob_hash로 참조하는
  비-hook source 없음 AND hook source ≥ 1. 바이트는 CAS 물리 파일 크기.

- [ ] **Step 1: 실패 테스트 작성 — 귀속 술어 6케이스**

`internal/store/store_test.go`에 추가(기존 Register 헬퍼 재사용, 케이스별
독립 TempDir):

```go
// TestShadowOwnedAttribution — D40 §2: content_hash 단위 귀속 술어.
func TestShadowOwnedAttribution(t *testing.T) {
	cases := []struct {
		name string
		seed func(t *testing.T, st *Store) // Register 조합
		wantHashes int
	}{
		{"hook만 참조 → 귀속", seedHookOnly, 1},
		{"hook+explicit 공유 → 비귀속", seedHookPlusFile, 0},
		{"cross-media 공유(동일 hash·상이 media_type) → 비귀속", seedCrossMedia, 0},
		{"raw_blob_hash 비-hook 참조 → 비귀속", seedRawRefByFile, 0},
		{"source 0개 → 비귀속", seedNoSource, 0},
		{"hook 2개(동일 hash) → 귀속 1(hash 단위 dedup)", seedTwoHooks, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			st := openT(t, dir) // 기존 store 테스트 열기 관례(선행 읽기로 확정)
			c.seed(t, st)
			st.Close() // SizeStats는 dir 기준 별도 ro open — 시드 후 닫고 조회
			sz, err := SizeStats(dir)
			if err != nil { t.Fatal(err) }
			if sz.ShadowOwnedHashes != c.wantHashes {
				t.Fatalf("ShadowOwnedHashes=%d want %d", sz.ShadowOwnedHashes, c.wantHashes)
			}
			if len(sz.ShadowOwned) != c.wantHashes {
				t.Fatalf("ShadowOwned len=%d want %d", len(sz.ShadowOwned), c.wantHashes)
			}
		})
	}
}

// TestShadowOwnedBytesPhysical — D40 §2: 물리 CAS 파일 기저 정확 단정 —
// 동일 hash의 hook-only cross-media artifact 2행이어도 ShadowOwnedBytes는
// 물리 파일 1개의 os.Stat 크기와 정확히 같다(논리 byte_length 2배 합산이면
// 오구현 — 검수 요구 정밀 단정).
func TestShadowOwnedBytesPhysical(t *testing.T) {
	dir := t.TempDir()
	st := openT(t, dir)
	h := seedTwoHookArtifactsSameHashDiffMedia(t, st) // 동일 콘텐츠·상이 media_type
	st.Close()
	want := statBlobFile(t, dir, h).Size() // artifacts/<h[:2]>/<h> os.Stat
	sz, err := SizeStats(dir)
	if err != nil { t.Fatal(err) }
	if sz.ShadowOwnedBytes != want {
		t.Fatalf("ShadowOwnedBytes=%d want %d(물리 파일 크기 — 논리 합산 금지)", sz.ShadowOwnedBytes, want)
	}
}

// TestSizeStatFileBytes — D40 §2: FileBytes = content.db 파일 실크기.
func TestSizeStatFileBytes(t *testing.T) {
	dir := t.TempDir()
	st := openT(t, dir)
	seedHookOnly(t, st)
	st.Close()
	sz, err := SizeStats(dir)
	if err != nil { t.Fatal(err) }
	if sz.FileBytes <= 0 {
		t.Fatalf("FileBytes=%d want >0", sz.FileBytes)
	}
}
```

seed 헬퍼는 기존 Register 호출로 구성: hook은 `Kind: "hook"` +
`shadow:<Tool>:<hash>` URI, explicit은 `Kind: "file"` 등. cross-media는
동일 콘텐츠·상이 media_type 2회 Register. raw_blob_hash 케이스는 Register
경로가 raw_blob_hash를 기입하는 입력(선행 읽기에서 확인)으로 구성.

- [ ] **Step 2: RED 확인**

Run: `go test -p 1 ./internal/store -run 'TestShadowOwned|TestSizeStatFile' -v`
Expected: FAIL (필드 부재 — 컴파일 에러도 RED로 간주)

- [ ] **Step 3: SizeStat 확장 구현**

`internal/store/store.go` — 귀속 hash 집합 산출(1쿼리) + 물리 파일 합산:

```go
// D40 §2: shadow 귀속 hash — 해당 hash를 참조하는 전 소스가 hook이고
// (직접 참조·raw_blob_hash 참조 모두), hook source가 1개 이상인 hash.
const shadowOwnedHashQuery = `
SELECT a.content_hash
FROM artifacts a
GROUP BY a.content_hash
HAVING
  SUM(CASE WHEN EXISTS(
    SELECT 1 FROM sources s WHERE s.artifact_id=a.id AND s.source_kind='hook'
  ) THEN 1 ELSE 0 END) >= 1
  AND NOT EXISTS(
    SELECT 1 FROM sources s2
    JOIN artifacts a2 ON a2.id = s2.artifact_id
    WHERE a2.content_hash = a.content_hash AND s2.source_kind != 'hook'
  )
  AND NOT EXISTS(
    SELECT 1 FROM sources s3
    WHERE s3.raw_blob_hash = a.content_hash AND s3.source_kind != 'hook'
  )
  AND EXISTS(SELECT 1 FROM sources s4
    JOIN artifacts a4 ON a4.id = s4.artifact_id
    WHERE a4.content_hash = a.content_hash)`
```

(SQL은 계약 표현 — 구현 시 스키마 실측에 맞춰 단순화 가능. "source 0개
hash 비귀속"은 마지막 EXISTS가 보장.) 물리 바이트는 기존 blob walk
(`artifacts/<2hex>/<64hex>`)에서 귀속 집합 멤버십 파일만 합산. FileBytes는
content.db 경로 `os.Stat`. 실패는 기존 SizeStat 오류 경로와 동일 처리.

- [ ] **Step 4: GREEN + store 전체 회귀**

Run: `go test -p 1 ./internal/store -v`
Expected: 전부 PASS

- [ ] **Step 5: 커밋**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -m "feat(v0.5): D40 SizeStat 확장 — content_hash 귀속 술어·물리 파일 합산·FileBytes"
```

---

### Task 3a: D40 — doctor [14] `file=` 병기

**Files:**
- Modify: `internal/cli/cli.go` (doctor [14] 라인)
- Test: `internal/cli/cli_test.go`
- 선행 읽기: `internal/cli/cli.go:1300-1315`([14]·경고)

**Interfaces:**
- Consumes: Task 2 `SizeStat.FileBytes`.
- Produces: `[14] content.db: sources=%d artifacts=%d blob=%dB file=%dB`
  (순서 고정 — 기존 `blob=45B` Contains 단정은 접미 확장에 안전).
  D38 경고 평가 기준은 [14] blob 그대로(문구 갱신은 Task 5b).

- [ ] **Step 1: 실패 테스트** — 기존 doctor 테스트(cli_test.go:230 부근)
  옆에 `strings.Contains(out, " file=")` 단정 1개 추가.
- [ ] **Step 2: RED 확인** — `go test -p 1 ./internal/cli -run TestDoctor -v` → FAIL.
- [ ] **Step 3: 구현** — [14] Fprintf에 ` file=%dB`(FileBytes) 추가.
- [ ] **Step 4: GREEN** — `go test -p 1 ./internal/cli -v` 전부 PASS.
- [ ] **Step 5: 커밋**

```bash
git add internal/cli/cli.go internal/cli/cli_test.go
git commit -m "feat(v0.5): D40 doctor [14] file= 병기"
```

---

### Task 3b: D40 — doctor [15] shadow-owned 접두 분해 (worktree 순회)

**Files:**
- Modify: `internal/cli/cli.go` ([15] 신설·worktrees/* 순회)
- Test: `internal/cli/cli_test.go`
- 선행 읽기: `internal/cli/cli.go:1180-1330`(doctor [6]~[14]),
  `cli.go:700-715`(purgeSessionFiles의 worktrees/* 순회 관례 —
  `os.ReadDir(projDir/worktrees)` 인라인, 전용 헬퍼 없음),
  `internal/session/session.go:300-320`(OpenReadOnly — **raw `*sql.DB`
  반환**, SQL 직접 조회), `internal/hook/hook_test.go:981-1011`
  (`artifact://` ref에서 `LastIndex("sha256-")`로 hash 추출하는 선례 —
  세션ID의 `:` 성분과 무충돌)

**Interfaces:**
- Consumes: Task 2 `SizeStat.ShadowOwned map[string]int64`(귀속 hash→물리
  바이트 — 접두별 %dB는 이 맵으로 합산. **CAS 경로를 cli에서 재구성하지
  말 것** — D13 소유 계약 위반).
- Produces: doctor 출력 계약(설계 §2, 문면 고정):
  - `[15] shadow-owned: %dB hashes=%d (cc:=%dB cx:=%dB shared=%dB unattributed=%dB)`
  - 일부 session.db 불용(열기·조회·스캔·JSON 파싱 어느 단계든) 시 괄호
    끝 ` incomplete`, 전부 불용 시
    `[15] shadow-owned: %dB hashes=%d (세션 분해 없음)`.
  - 실패는 worktree 단위 격리 — doctor 전역 failed 목록에 넣지 않는다.

- [ ] **Step 1: 실패 테스트 작성**

`internal/cli/cli_test.go`에 추가(기존 doctor 테스트 구성 재사용 —
cli_test.go:230 부근의 시드 방식):

```go
// TestDoctorShadowOwnedLine — D40 §2: [15] 접두 분해.
func TestDoctorShadowOwnedLine(t *testing.T) {
	// 시드: hook-만 hash 1개(cc: 세션 artifact_created ref 부여),
	// explicit hash 1개. worktree session.db에 artifact_created 이벤트 기입
	// (sessions 직접 INSERT 선례: session export_test.go:390 방식).
	...(기존 시드 헬퍼 조합 — doctor 실행 헬퍼는 기존 doctor 테스트 관례)...
	out := runDoctor(t, storeRoot, projRoot)
	for _, want := range []string{"[15] shadow-owned: ", "cc:="} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor 출력에 %q 없음:\n%s", want, out)
		}
	}
}

// TestDoctorShadowOwnedIncomplete — 손상 session.db 1개 혼재 시 그
// worktree만 건너뛰고 incomplete 병기, doctor는 성공 종료.
func TestDoctorShadowOwnedIncomplete(t *testing.T) {
	...(worktrees/ 아래 정상 DB 1 + 0xEE 4096B 손상 DB 1 구성 —
	   mcp_test.go:1937의 손상 시드 방식 재사용)...
	out := runDoctor(t, storeRoot, projRoot)
	if !strings.Contains(out, "incomplete") {
		t.Fatalf("incomplete 병기 없음:\n%s", out)
	}
	// doctor 종료 코드/failed 목록에 [15] 실패가 없어야 함 — 기존 성공 단정 재사용
}
```

- [ ] **Step 2: RED 확인**

Run: `go test -p 1 ./internal/cli -run TestDoctorShadowOwned -v`
Expected: FAIL

- [ ] **Step 3: 구현**

doctor [14] 라인에 ` file=%dB` 추가(Task 2 FileBytes). [15] 신설:

```go
// [15] D40 §2 — 접두 분해: worktrees/* 전체 순회, 실패는 worktree 단위
// 격리(열기·조회·스캔·JSON 파싱 어느 단계든 skip+incomplete, 전역 실패
// 전파 금지).
owned := sz.ShadowOwned // Task 2 산출: 귀속 hash → 물리 바이트
hashPrefix := map[string]map[string]bool{} // hash → {"cc:","cx:"} 관측 집합
incomplete := false
entries, _ := os.ReadDir(filepath.Join(projDir, "worktrees")) // 순회 관례 인라인(purgeSessionFiles 동형)
for _, e := range entries {
	wdir := filepath.Join(projDir, "worktrees", e.Name())
	if err := func() error {
		db, err := session.OpenReadOnly(wdir) // raw *sql.DB — SQL 직접 조회
		if err != nil { return err }
		defer db.Close()
		rows, err := db.QueryContext(ctx,
			`SELECT session_id, artifact_refs FROM session_events
			 WHERE event_type='artifact_created' AND artifact_refs IS NOT NULL`)
		if err != nil { return err }
		defer rows.Close()
		for rows.Next() {
			// artifact_refs = JSON 배열, 각 ref = "artifact://<sid>/sha256-<hash>"
			// hash 추출은 LastIndex("sha256-") 선례(hook_test.go:995) —
			// 세션ID의 ':' 성분과 무충돌.
			...JSON 디코드 + hash 추출 + 접두(cc:/cx:/기타) 기록...
		}
		return rows.Err()
	}(); err != nil {
		incomplete = true // 해당 worktree만 skip — 전역 failed 금지
	}
}
// hash별(owned의 키만 대상): 접두 1종 → 해당 버킷에 owned[h] 합산,
// 2종 이상 → shared, 기록 없음/기타 접두 → unattributed.
// 출력 문면은 Interfaces 계약 그대로.
```

- [ ] **Step 4: GREEN + cli 전체 회귀**

Run: `go test -p 1 ./internal/cli -v`
Expected: 전부 PASS ([14] 기존 Contains 단정은 접미 확장에 안전 — 설계 §11)

- [ ] **Step 5: 커밋**

```bash
git add internal/cli/cli.go internal/cli/cli_test.go
git commit -m "feat(v0.5): D40 doctor [15] shadow-owned 접두 분해(worktree 순회·incomplete 격리)"
```

---

### Task 4a: D42 — 빈 세션 GC (배치 분할) + [6] 확장

**Files:**
- Modify: `internal/session/retention.go` (Sweep 확장)
- Modify: `cmd/context-router/main.go` (Sweep 호출자 —
  sweepSessionRetentionAtStart의 반환 소비·고지 문구, 검수 정정: 기존
  `(int64, error)` 반환을 "삭제된 이벤트 수"로 출력하므로 시그니처 변경이
  여기 파급)
- Modify: `internal/cli/cli.go` ([6] sessions/empty 병기)
- Test: `internal/session/retention_test.go`(없으면 기존 세션 테스트 파일),
  `cmd/context-router/main_test.go`(고지 문구 소비처가 있으면),
  `internal/cli/cli_test.go`
- 선행 읽기: `internal/session/retention.go` 전체,
  `internal/session/session.go:60-100`(스키마·FTS 트리거),
  `cmd/context-router/main.go:125-140`(sweepSessionRetentionAtStart),
  `internal/cli/cli.go:1198-1215`([6])

**Interfaces:**
- Produces(검수 정정 — 두 종류 건수는 기존 `(int64, error)`로 보고 불가):
  ```go
  type SweepReport struct {
  	EventsDeleted        int64 // 기존 retention 삭제 이벤트 수
  	EmptySessionsDeleted int64 // 빈 세션 GC 세션 수
  }
  func Sweep(ctx context.Context, d *DB, now time.Time) (SweepReport, error)
  ```
  호출자(main.go) 고지: 기존 stderr 1줄에 `empty-session GC n건` 병합.
  GC 술어(설계 §4, 불변): 비-session_start 이벤트 0건 AND
  `started_at < now-7d`(상수 `emptySessionMaxAgeSec = 7*24*3600`),
  retention_sec 무관. **배치 상수 `sweepBatchSessions = 64`** — 배치당
  단일 트랜잭션, 배치 간 잠금 양보(설계 §4 잠금 예산). **후보 선정·빈
  술어 재검증·DELETE 2종은 반드시 같은 `BEGIN IMMEDIATE` 트랜잭션
  안에서**(검수 정정 — 후보를 tx 밖에서 선정하면 선정↔삭제 사이에
  커밋된 실이벤트까지 삭제하는 TOCTOU).
- Consumes: 없음.

- [ ] **Step 1: 실패 테스트 작성**

```go
// TestSweepEmptySessionGC — D42 §4: 빈 세션 GC 경계.
func TestSweepEmptySessionGC(t *testing.T) {
	d := openT(t, t.TempDir(), Options{Producer: "test"})
	now := time.Unix(1800000000, 0)
	old := now.Add(-8 * 24 * time.Hour).Unix()   // 7일 경과 → GC
	fresh := now.Add(-6 * 24 * time.Hour).Unix() // 미경과 → 보존
	seedSession(t, d, "019aaa...", old, 0)        // retention 0 무관 GC
	seedSession(t, d, "cc:bbb...", old, 2592000)  // retention 30d 무관 GC
	seedSessionStart(t, d, "019aaa...", old)      // session_start만
	seedSessionStart(t, d, "cc:bbb...", old)
	seedSessionStart(t, d, "cc:bbb...", old+1)    // session_start 복수도 빈 세션
	seedSession(t, d, "cc:ccc...", fresh, 0)      // 미경과 보존
	seedSessionStart(t, d, "cc:ccc...", fresh)
	seedSession(t, d, "cc:ddd...", old, 0)        // 실이벤트 보유 → 보존
	seedSessionStart(t, d, "cc:ddd...", old)
	insertRawEvent(t, d, "e1", "cc:ddd...", "tool_call", old+1, "x", ...)

	if _, err := Sweep(context.Background(), d, now); err != nil { t.Fatal(err) }
	remain := querySessionIDs(t, d)
	want := []string{"cc:ccc...", "cc:ddd..."}
	if !slices.Equal(remain, want) {
		t.Fatalf("잔존 세션=%v want %v", remain, want)
	}
	// GC된 세션의 이벤트도 소멸(FTS는 AFTER DELETE 트리거 동기 — 설계 §4)
	if n := countEventsFor(t, d, "019aaa..."); n != 0 {
		t.Fatalf("GC 세션 이벤트 잔존 %d", n)
	}
}

// TestSweepEmptySessionGCBatch — 배치 상수 초과(65개) 시 복수 트랜잭션
// 커밋(전량 GC됨을 단정 — 분할이 결과를 바꾸지 않음).

// TestSweepEmptySessionGCPreservesLateEvent — barrier(검수 요구): 후보로
// 보일 세션에 Sweep 직전 실이벤트를 커밋 → 그 세션·이벤트 보존 단정
// (빈-술어가 DELETE와 같은 tx 안에서 재평가됨을 고정).
```

seed 헬퍼는 `export_test.go:390` 방식(직접 INSERT) 재사용.

- [ ] **Step 2: RED 확인**

Run: `go test -p 1 ./internal/session -run TestSweepEmptySession -v
Expected: FAIL

- [ ] **Step 3: Sweep 확장 구현**

```go
// D42 §4: 빈 세션 GC — 배치 분할(배치당 단일 BEGIN IMMEDIATE tx).
// 후보 SELECT·술어 재검증·DELETE 2종이 전부 같은 tx 안(TOCTOU 봉쇄 —
// tx 밖 후보 선정은 선정↔삭제 사이 커밋된 실이벤트를 오삭제).
const (
	emptySessionMaxAgeSec = 7 * 24 * 3600
	sweepBatchSessions    = 64
)
for {
	var n int64
	err := d.runTx(ctx, func(tx *sql.Tx) error { // BEGIN IMMEDIATE — 기존 tx 관례
		// 같은 tx 안에서 선정+검증: DELETE의 WHERE에 빈-술어를 직접 내장
		res, err := tx.Exec(`
			DELETE FROM sessions WHERE session_id IN (
				SELECT s.session_id FROM sessions s
				WHERE s.started_at < ?
				  AND NOT EXISTS(SELECT 1 FROM session_events e
				        WHERE e.session_id = s.session_id
				          AND e.event_type != 'session_start')
				LIMIT ?)`, nowUnix-emptySessionMaxAgeSec, sweepBatchSessions)
		// 삭제된 세션의 session_start 이벤트도 같은 tx에서 삭제
		// (sessions 삭제 대상 목록을 RETURNING 또는 선행 SELECT로 같은 tx 내 확보)
		...
		n, _ = res.RowsAffected()
		return err
	})
	if err != nil || n == 0 { break }
	rep.EmptySessionsDeleted += n // 실삭제 행만 집계
	// 배치 간 잠금 양보(tx 종료로 자동)
}
```

기존 retention 삭제도 동일 배치 상수로 분할(설계 §4 — 단일 대형 tx 금지).
main.go 호출자 갱신: SweepReport 소비 + 고지 `… empty-session GC n건` 병합.

- [ ] **Step 4: GREEN + session 회귀**

Run: `go test -p 1 ./internal/session -v` → 전부 PASS

- [ ] **Step 5: [6] 확장 — 실패 테스트 → 구현 → GREEN**

cli_test.go: doctor 출력에 `sessions=` / `(empty=` Contains 단정 추가 후
RED 확인. 구현: `[6] session.db: quick_check=ok sessions=%d (empty=%d)` —
empty 술어는 GC와 동일(경과 무관). **empty 계상 조회 실패 시 [6]을
실패로 뒤집지 않음**(정보성 유지 — quick_check=ok 렌더 유지, 설계 §8).
Run: `go test -p 1 ./internal/cli -v` → PASS.

- [ ] **Step 6: 커밋**

```bash
git add internal/session/retention.go internal/session/*_test.go cmd/context-router/main.go cmd/context-router/main_test.go internal/cli/cli.go internal/cli/cli_test.go
git commit -m "feat(v0.5): D42 빈 세션 GC(SweepReport·tx 내 재검증·배치 분할) + doctor [6] 세션 집계"
```

---

### Task 4b: D42 — append 존재 게이트 (전체 append·WHERE EXISTS)

**Files:**
- Modify: `internal/session/session.go` (Append 경로)
- Test: `internal/session/session_test.go`(동시성 회귀),
  `internal/hook/hook_test.go`(shadow 경로 게이트)
- 선행 읽기: `internal/session/session.go`의 Append/AppendDB 트랜잭션
  구조, `internal/hook/hook.go:141-180`(dispatch→Append→shadowCapture),
  `internal/hook/shadow.go:33-100`

**Interfaces:**
- Produces: Append의 이벤트 INSERT가 `INSERT … SELECT … WHERE EXISTS
  (SELECT 1 FROM sessions WHERE session_id=?)`로 게이트 — 세션 부재 시
  0행 삽입 + 명시 오류(기존 오류 경로 재사용, 호출자는 drop 처리).
  훅 경로의 **모든** append(기본 + shadowCapture의 artifact_created·
  tool_result_summary)가 이 게이트를 통과(별도 우회 INSERT 금지).
- Consumes: Task 4a의 GC(경합 상대).

- [ ] **Step 1: 실패 테스트 작성 — 삭제 후 삽입 거부**

```go
// TestAppendRejectsDeletedSession — D42 §4: 세션 행 삭제 후 Append는
// 이벤트를 커밋하지 않는다(orphan 금지).
func TestAppendRejectsDeletedSession(t *testing.T) {
	ad := openAppendT(t, dir, AppendOptions{ExternalSessionID: "cc:...", ...})
	// 세션 행을 직접 DELETE(GC 시뮬레이션)
	execT(t, ad, `DELETE FROM sessions WHERE session_id='cc:...'`)
	err := ad.Append(ctx, Event{Type: "tool_call", ...})
	if err == nil {
		t.Fatal("삭제된 세션 Append가 성공 — 게이트 부재")
	}
	if n := countEventsFor(t, ad, "cc:..."); n != 0 {
		t.Fatalf("orphan 이벤트 %d건", n)
	}
}
```

- [ ] **Step 2: RED 확인**

Run: `go test -p 1 ./internal/session -run TestAppendRejectsDeleted -v
Expected: FAIL (현행은 FK 부재로 성공 커밋)

- [ ] **Step 3: 구현 — INSERT…WHERE EXISTS 게이트**

Append의 이벤트 INSERT를 게이트 형태로 교체(같은 tx 내 모든 파생 행 동일
원칙). `RowsAffected()==0`이면 세션 부재 오류 반환(기존 오류 매핑 경로).
미지 세션 이벤트는 write lock을 잡지 않는다(WHERE EXISTS 0행 — 설계 §4).

- [ ] **Step 4: GREEN + shadow 이벤트 타입 게이트 검증(세션 계층)**

Run: `go test -p 1 ./internal/session ./internal/hook -v`
Expected: 전부 PASS. **검수 정정 — 훅 블랙박스로 shadow 게이트를 검증하지
말 것**: dispatch는 SessionExists=false면 unknown-session drop 후 조기
반환이라 shadowCapture에 도달하지 않아, 게이트 없이도 0건이 참이 되는
공허 테스트가 된다. 대신 **세션 계층에서 직접** 검증한다 — 삭제된 세션에
`ev.Type="artifact_created"`·`"tool_result_summary"`를 `ad.Append`로 넣어
거부(0행) 단정. 모든 append가 appendEvent(session.go:652) 단일 경로를
지나므로 이 단정이 곧 shadowCapture 경로 커버다(설계 §8 계약 충족).

- [ ] **Step 5: resume 자가 회복 회귀**

```go
// TestSessionStartAfterGCRecreates — D42 §4 resume: GC로 세션 행이
// 사라진 뒤 같은 session_id의 session_start가 오면 멱등 재생성된다.
```
기존 EnsureSession 경로가 이미 멱등이면 이 테스트는 즉시 GREEN — 그래도
회귀 고정 가치로 추가(설계 §8 요구).

- [ ] **Step 6: 커밋**

```bash
git add internal/session/session.go internal/session/*_test.go internal/hook/hook_test.go
git commit -m "feat(v0.5): D42 append 존재 게이트(WHERE EXISTS·전체 append) + resume 재생성 회귀"
```

---

### Task 5a: D41 — store 선택 purge 작업 (행 삭제 + CAS age-gate 회수)

**Files:**
- Modify: `internal/store/store.go` (신규 작업 `PurgeHookOnly`)
- Test: `internal/store/store_test.go`
- 선행 읽기: `internal/store/store.go:700-850`(PurgeOlderThan·
  GCOrphanBlobs·gcOrphanMinAge·lockStore), Task 2 결과물

**Interfaces:**
- Produces:
  ```go
  type HookPurgeReport struct {
  	Hashes        int   // 행 삭제된 귀속 hash 수
  	ReclaimedB    int64 // 실제 unlink된 물리 바이트 합
  	DeferredFiles int   // age-gate/교체 감지 유예 건수
  	FailedFiles   int   // unlink 실패(orphan 잔존) 건수
  }
  func (s *Store) PurgeHookOnly(ctx context.Context) (HookPurgeReport, error)
  func (s *Store) Vacuum(ctx context.Context) error // s.writer.ExecContext("VACUUM") — Task 5b 소비(검수 정정: Writer() 접근자 부재)
  ```
- 실행 계약(설계 §3, 불변): 단일 tx에서 술어 재검증(Task 2 술어 재실행)
  → sources·chunks(FTS 동기)·artifacts 행 삭제 → 커밋 → `lockStore`
  (패키지 함수 — 검수 정정: 메서드 아님) 획득 하에 hash 명시 집합을
  **rename 격리 프로토콜**로 회수(아래 — 검수 발견: Stat↔unlink 사이에
  Register의 writeBlob 교체가 끼면 fresh 파일을 오삭제, 이미 읽은
  ModTime으로는 age gate가 못 막음). VACUUM 실행은 CLI(Task 5b)가
  `Vacuum(ctx)` 호출로 후행.
- rename 격리 프로토콜(파일별): ① `os.Rename(p, p+".purging")` — 원자,
  실패(부재)는 skip ② 격리본 re-Stat: mtime이 gcOrphanMinAge(1h) 이내
  **또는** DB 재확인에서 참조 존재 → `os.Rename` 롤백 + Deferred++
  ③ 아니면 `os.Remove(격리본)` + ReclaimedB += size. rename 이후 도착한
  Register.writeBlob은 원 경로에 새 파일을 만들 뿐이라 무충돌, rename
  이전 교체분은 격리본의 fresh mtime이 ②에서 걸려 롤백 — 창 폐쇄.

- [ ] **Step 1: 실패 테스트 작성**

```go
// TestPurgeHookOnly — D41 §3: explicit 공유 보존 + hook-만 행·파일 삭제.
func TestPurgeHookOnly(t *testing.T) {
	st := openTestStore(t)
	hookHash := seedHookOnly(t, st)      // hook-만
	sharedHash := seedHookPlusFile(t, st) // explicit 공유
	ageBlobFile(t, st, hookHash, -2*time.Hour) // mtime 2h 전 — age gate 통과
	rep, err := st.PurgeHookOnly(context.Background())
	if err != nil { t.Fatal(err) }
	if rep.Hashes != 1 || rep.ReclaimedB <= 0 || rep.DeferredFiles != 0 {
		t.Fatalf("report=%+v", rep)
	}
	assertHashGone(t, st, hookHash)     // 행·파일·FTS 히트 소멸
	assertHashIntact(t, st, sharedHash) // 공유 hash 완전 보존
}

// TestPurgeHookOnlyAgeGateDefers — mtime 30분 전(1h 이내) 파일은 행만
// 삭제되고 unlink 유예(DeferredFiles=1, 파일 잔존 → --gc 후속 회수 경로).
func TestPurgeHookOnlyAgeGateDefers(t *testing.T) {
	st := openTestStore(t)
	h := seedHookOnly(t, st)
	ageBlobFile(t, st, h, -30*time.Minute)
	rep, _ := st.PurgeHookOnly(context.Background())
	if rep.DeferredFiles != 1 || rep.ReclaimedB != 0 {
		t.Fatalf("report=%+v want 유예 1·회수 0", rep)
	}
	assertBlobFileExists(t, st, h) // 파일 보존(재등록 경합 안전)
}

// TestPurgeHookOnlyRevalidates — 견적 후 비-hook source가 생긴 hash는
// tx 내 재검증으로 대상 제외(행·파일 모두 보존).

// TestPurgeHookOnlyReplacedFileRollsBack — 경합 시뮬(검수 요구): 행 삭제
// 커밋 후·파일 회수 전 시점에 동일 경로를 fresh mtime 파일로 교체
// (Register.writeBlob 재등록 시뮬) → rename 격리 ②의 fresh-mtime 검출로
// 롤백(원 경로 복원·Deferred=1·ReclaimedB=0) 단정.
```

`ageBlobFile`은 `os.Chtimes`로 mtime 조작(결정론 — 설계 §8). 교체 시뮬은
회수 단계를 함수로 분리해 테스트가 행 삭제와 회수 사이에 개입할 수 있게
구성한다(내부 함수 분리는 가 — 신규 공개 표면 아님).

- [ ] **Step 2: RED 확인**

Run: `go test -p 1 ./internal/store -run TestPurgeHookOnly -v` → FAIL

- [ ] **Step 3: 구현**

```go
func (s *Store) PurgeHookOnly(ctx context.Context) (HookPurgeReport, error) {
	var rep HookPurgeReport
	var hashes []string
	// ① tx: 술어 재검증(shadowOwnedHashQuery 재실행) + 행 삭제.
	// tx 래퍼는 기존 관례(runTx/txRetry — store.go:360/378, 검수 정정:
	// withTx 아님)를 재사용, 삭제 순서는 PurgeOlderThan(chunks 우선) 동형.
	err := s.runTx(ctx, func(tx *sql.Tx) error {
		hashes = queryShadowOwnedHashes(tx) // Task 2 술어 재사용
		...chunks(FTS 동기)·sources·artifacts 행 삭제...
		rep.Hashes = len(hashes)
		return nil
	})
	if err != nil { return rep, err }
	// ② lockStore(s.dir) — 패키지 함수(store.go:56, 검수 정정: 메서드
	// 아님) 획득 하에 rename 격리 회수. 실패해도 행 삭제는 유효(orphan은
	// --gc 후속).
	unlock, err := lockStore(s.dir)
	if err != nil { return rep, err }
	defer unlock()
	for _, h := range hashes {
		p := filepath.Join(s.dir, "artifacts", h[:2], h) // blobPath 헬퍼 부재 — 인라인 관례
		q := p + ".purging"
		if err := os.Rename(p, q); err != nil { continue } // 부재 등 skip
		fi, err := os.Stat(q)
		rollback := err != nil ||
			time.Since(fi.ModTime()) < gcOrphanMinAge || // 교체 감지 겸 age gate
			stillReferenced(ctx, s, h)                   // DB 재확인
		if rollback {
			_ = os.Rename(q, p)
			rep.DeferredFiles++
			continue
		}
		if err := os.Remove(q); err != nil {
			_ = os.Rename(q, p)
			rep.FailedFiles++
			continue
		}
		rep.ReclaimedB += fi.Size()
	}
	return rep, nil
}

func (s *Store) Vacuum(ctx context.Context) error {
	_, err := s.writer.ExecContext(ctx, "VACUUM")
	return err
}
```

- [ ] **Step 4: GREEN + store 회귀**

Run: `go test -p 1 ./internal/store -v` → 전부 PASS

- [ ] **Step 5: 커밋**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -m "feat(v0.5): D41 PurgeHookOnly — 술어 재검증·행 삭제·rename 격리 CAS 회수 + Vacuum API"
```

---

### Task 5b: D41 — CLI 배선 (`purge --hook-only`) + D38 경고 문구 승격

**Files:**
- Modify: `internal/cli/cli.go` (runPurge 조기 분기·open→confirm 재배치·
  VACUUM·보고, [14] 경고 문구)
- Test: `internal/cli/cli_test.go`
- 선행 읽기: `internal/cli/cli.go:532-740`(runPurge 전체 흐름 —
  selective:581, gcOnly:585/634, sessions:641, RemoveAll:656-657,
  confirm:597, store open:664), `cli.go:1310-1315`(경고),
  `cli_test.go:295`(무구분 단정)

**Interfaces:**
- Consumes: Task 5a `PurgeHookOnly`·`HookPurgeReport`·`Vacuum(ctx)`,
  Task 2 `SizeStats(dir)`의 `ShadowOwned` 맵(견적 = 합산·len — 별도
  estimateShadowOwned 함수 부재, 검수 정정).
- Produces: CLI 계약(설계 §3) —
  - `--hook-only`는 **전역 confirmPurge(cli.go:597)보다 앞**에 조기 전용
    분기로 인터셉트(검수 정정 — sessions:641은 전역 confirm 뒤라 그
    위치면 확인이 2회 뜬다). 확인은 자체 confirmPurge 1회만, 전체 삭제
    `os.RemoveAll` 기본 분기 비도달.
  - `--all`·`--older-than`·`--sessions`·`--gc`와 조합 시 사용 오류
    (오류 반환은 기존 runPurge의 사용 오류 관례 재사용 — usageError
    헬퍼 부재, 검수 정정).
  - 흐름(스펙 §3 순서 고정): store open → 견적(ShadowOwned 합산) →
    confirmPurge(견적 문구) → PurgeHookOnly → **④ 실회수 보고 먼저** →
    ⑤ `st.Vacuum(ctx)`(실패는 stderr log-and-continue — 보고를 VACUUM
    뒤로 미루면 부분 성공 노출이 늦어진다, 검수 정정).
  - [14] 경고 문구: "현행 purge는 source_kind 무구분 삭제 …" →
    `"purge --project <id> --hook-only로 shadow만 선택 삭제 가능"`.

- [ ] **Step 1: 실패 테스트 작성**

```go
// TestPurgeHookOnlyCLI — e2e: 조기 분기·보존·보고 문면.
func TestPurgeHookOnlyCLI(t *testing.T) {
	...(시드: hook-만 + explicit 공유, mtime 2h 전)...
	out := runPurgeCLI(t, storeRoot, "--project", pid, "--hook-only", "--force")
	if !strings.Contains(out, "실회수") { t.Fatalf("보고 문면 없음:\n%s", out) }
	// 전체 삭제 비도달: content.db·explicit 행 잔존
	assertProjectDirExists(t, storeRoot, pid)
	assertHashIntact(t, ..., sharedHash)
}

// TestPurgeHookOnlyComboRejected — --all/--older-than/--sessions/--gc 조합
// 각각 사용 오류 반환(rc != 0, 삭제 없음).

// TestDoctorWarnMentionsHookOnly — [14] 경고 문구가 --hook-only를 안내
// (기존 cli_test.go:295의 "무구분" 단정을 신문구로 갱신 — 설계 §8).

// TestPurgeHookOnlySinglePrompt — TTY 경로에서 확인 프롬프트가 정확히
// 1회만 출력(--force 없이 — 전역 confirm과의 중복 방지 회귀, 검수 요구).

// TestPurgeHookOnlyVacuumFailureContinues — Vacuum 실패 시에도 실회수
// 보고는 이미 출력됐고 rc=0(log-and-continue) 단정.
```

- [ ] **Step 2: RED 확인**

Run: `go test -p 1 ./internal/cli -run 'TestPurgeHookOnly|TestDoctorWarn' -v
Expected: FAIL

- [ ] **Step 3: 구현**

runPurge에 플래그 추가 + 조합 검증 + 조기 분기(**전역 confirm(597)보다
앞** — 확인 중복 방지):

```go
if *hookOnlyFlag {
	if *allFlag || *olderThanFlag != "" || *sessionsFlag || *gcFlag {
		return fmt.Errorf("--hook-only는 --project와만 조합") // 기존 사용 오류 관례로 조정
	}
	// ⓪ open 선행(견적) — 검수 정정: 현행 confirm→open 순서를 이 분기에
	// 한해 open→confirm으로. 견적은 SizeStats(projDir)의 ShadowOwned 합산.
	sz, err := store.SizeStats(projDir)
	if err != nil { return err }
	var estB int64
	for _, b := range sz.ShadowOwned { estB += b }
	if err := confirmPurge(in, w, isTTY, *force,
		fmt.Sprintf("shadow %dB(%d hashes) 선택 삭제", estB, len(sz.ShadowOwned))); err != nil {
		return err
	}
	st, err := store.Open(projDir, false)
	if err != nil { return err }
	defer st.Close()
	rep, err := st.PurgeHookOnly(ctx)
	if err != nil { return err }
	// ④ 실회수 보고 먼저(스펙 §3 순서) → ⑤ VACUUM 후행
	fmt.Fprintf(w, "hook-only purge: 실회수 %dB(%d hashes), 유예 %d건, 실패 %d건\n",
		rep.ReclaimedB, rep.Hashes, rep.DeferredFiles, rep.FailedFiles)
	if verr := st.Vacuum(ctx); verr != nil {
		fmt.Fprintf(stderr, "ctr: VACUUM 실패(계속 진행 — 서버 정지 후 재실행 시 회수): %v\n", verr)
	}
	return nil
}
```

경고 문구 갱신(cli.go:1313 부근) + `cli_test.go:295` 단정 갱신.

- [ ] **Step 4: GREEN + cli 전체 회귀**

Run: `go test -p 1 ./internal/cli -v` → 전부 PASS

- [ ] **Step 5: 커밋**

```bash
git add internal/cli/cli.go internal/cli/cli_test.go
git commit -m "feat(v0.5): D41 purge --hook-only CLI 배선 — 조기 분기·견적 확인·VACUUM·실회수 보고 + D38 경고 문구 승격"
```

---

### Task 6: 편승 — breadcrumb 제거 + 버전 0.5.0

**Files:**
- Modify: `internal/cli/cli.go:36` 부근(구명 `CTR_SHADOW_WARN_BYTES` 주석 제거)
- Modify: `docs/context-router-design-v0.4-ko.md` D38 항목(구명 병기 구절 제거)
- Modify: `cmd/context-router/main.go:29` (`const version = "0.5.0"`)
- Modify: `internal/mcp/mcp.go:30` (`const ServerVersion = "0.5.0"` —
  **검수 발견 C1**: `cmd/context-router/main_test.go:33`의
  `TestVersionPinnedToServerVersion`이 두 상수 동일을 강제한다. main.go만
  바꾸면 전체 스위트 RED — 반드시 동반 범프)
- Test: 기존 스위트 회귀만(신규 테스트 없음 — 주석·문서·상수)

**Interfaces:** 없음.

- [ ] **Step 1: cli.go:36 구명 주석 제거** — `CTR_SHADOW_WARN_BYTES` 문자열이
  코드베이스 `.go`에서 0회가 되는지 Grep으로 확인.
- [ ] **Step 2: v0.4 설계서 D38 문면의 "구 `CTR_SHADOW_WARN_BYTES`" 병기
  구절만 제거**(D38 항목 나머지 불변 — append-only는 docs/prompts 규약,
  설계 정본은 amend-in-place).
- [ ] **Step 3: 버전 상수 2개 동반 범프** — main.go:29 `version`과
  mcp.go:30 `ServerVersion`을 함께 `"0.5.0"`으로(§1.1의 "버전 상수"는
  실제 2개다). 그 외 잔여 확인: Grep `0\.4\.0` in `*.go`.
- [ ] **Step 4: 전체 회귀**

Run: `go test -p 1 ./...`
Expected: 전부 PASS (`TestVersionPinnedToServerVersion` 포함)

- [ ] **Step 5: 커밋**

```bash
git add internal/cli/cli.go docs/context-router-design-v0.4-ko.md cmd/context-router/main.go internal/mcp/mcp.go
git commit -m "chore(v0.5): 구명 breadcrumb 2건 제거 + version·ServerVersion 0.5.0"
```

---

### Task 7: 통합 — 전체 회귀·도그푸딩 스모크·PR

- [ ] **Step 1: 전체 스위트** — `go test -p 1 ./...` 전부 PASS.
- [ ] **Step 2: 도그푸딩 스모크(컨트롤러 수행)** — **검수 정정: `go
  install` 금지**(전역 GOBIN을 덮어 리뷰 전 코드가 활성 훅 바이너리로
  즉시 투입되거나, PATH 불일치 시 구버전을 검사하는 거짓 양성). 대신:

  ```
  go build -o "$env:TEMP\ctr-smoke\context-router.exe" ./cmd/context-router
  & "$env:TEMP\ctr-smoke\context-router.exe" doctor   # 절대경로 직접 실행
  ```

  [14] `file=` 병기·[15] 라인·[6] `sessions=… (empty=…)` 문면 존재만
  단정(실측치는 시점 의존). 파괴적 명령(purge)은 실store에 실행하지
  않는다. **활성 훅 바이너리 교체(`go install`)는 머지 후 별도
  단계**(아래 Step 4 이후)로 분리.
- [ ] **Step 3: PR 생성** — base main, 제목 `feat: v0.5 store 수명주기
  (D40~D43)`, 본문에 설계 정본·§11 검수 요약 링크. 3-OS CI GREEN 확인.
- [ ] **Step 4: 머지·태그는 최종 리뷰(이중: 서브에이전트+Codex `review
  --base`) 통과 후** — 표준 프로토콜. 태그 `v0.5.0`. 머지 후에만
  `go install ./cmd/context-router`로 도그푸딩 바이너리 갱신.

---

## Self-Review 기록

- 스펙 커버리지: §1.1 6항목 ↔ Task 1(D43)·2/3a/3b(D40)·4a/4b(D42)·
  5a/5b(D41)·6(편승·버전)·7(릴리스) — 갭 없음. §3 실행 계약 ⓪~⑤ ↔
  Task 5a(②③④의 store 작업+Vacuum API)·5b(⓪①·④보고·⑤호출). §4 3계약 ↔
  4a(GC·배치·tx 내 재검증)·4b(게이트·resume). §8 검증 계약의 각 항목이
  태스크 내 테스트로 배치됨(cross-media·raw_blob_hash·URI 추출·incomplete·
  rename 격리 유예·경합 롤백·조합 거부·기본 분기 비도달·배치 분할·barrier·
  [6] 정보성·{2,5}필드·5필드 수용·경고 문구 갱신·TTY 단일 프롬프트·
  VACUUM 실패 continue).
- 타입 일관성: `HookPurgeReport`·`PurgeHookOnly`·`Vacuum`·`SizeStat`(단수
  구조체)/`SizeStats(dir)`(패키지 함수)·`ShadowOwned`(맵)·
  `ShadowOwnedBytes`·`ShadowOwnedHashes`·`SweepReport`·
  `sweepBatchSessions`·`emptySessionMaxAgeSec` 명칭을 태스크 간 교차
  참조로 고정.
- 계획 코드 블록은 설계 계약 기준 — 구현자는 선행 읽기 후 기존 시그니처에
  맞춰 조정하되 Interfaces 블록의 이름·계약은 불변.

## 계획 체크포인트 적대 검수 반영 (2026-07-22, 초판 8a53164 대상)

- 이중 검수: 서브에이전트(opus) C1·I3·M5·분할 권고 + Codex(high 4·
  medium 2, NO-SHIP) — 전 건 반영.
- 수렴: Vacuum 접근자 부재·Task 5b 스코프 공백(→ Task 5a에 `Vacuum(ctx)`
  신설), SizeStat 심볼 반전(→ 실코드 API 고정 + 물리 기저 정확 단정).
- Codex 고유: CAS 회수 Stat↔unlink 창(→ rename 격리 프로토콜), 빈 세션
  GC 후보 tx 밖 선정(→ tx 내 재검증 + barrier 테스트), `go install`
  스모크의 활성 훅 오염(→ `go build -o` 임시경로), Sweep `(int64,error)`
  로 2종 건수 보고 불가(→ `SweepReport` + main.go 스코프 편입), confirm
  중복·보고/VACUUM 순서(→ 전역 confirm 앞 분기·보고 선행).
- 서브에이전트 고유: `mcp.ServerVersion` 동반 범프 누락(C — Task 6),
  hash→크기 맵 인터페이스 공백(→ `ShadowOwned` 맵), shadow 게이트 훅
  블랙박스 테스트 공허(→ 세션 계층 직접 Append 검증), dropsByReason
  경로 입력·sid8 접두 함정 문면 정정, Task 3 분할(3a/3b).
