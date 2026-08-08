# FTS 세그먼트 병합(D102)과 회수 실적 계측(D103) 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 저장 비용 눈금을 고치고(파일의 75.9%가 회수 가능한 FTS 세그먼트다), 회수 실적을 퍼지 사정권 밖인 `ledger.db`에 남겨 72시간 창을 데이터로 판단할 수 있게 만든다.

**Architecture:** 두 단계다. **1단계(v0.19.1)** 는 `Store.MergeFTS`라는 원시 명령 하나를 더하고, 정책은 호출자가 갖는다 — 기동 퍼지 고루틴은 하루 한 번(스탬프 파일 mtime), 수동 회수 CLI 둘은 VACUUM 직전 무조건. **2단계(v0.20)** 는 `ledger`에 열 둘을 붙여 `ctr_fetch`의 해소·미해소를 artifact id와 회수 시점 나이로 남기고, 훅 포착마다 분모 행을 하나 남긴다.

**Tech Stack:** Go 1.26.5 · `modernc.org/sqlite` (FTS5 external-content, porter + trigram) · stdlib only.

## Global Constraints

- **게이트 다섯을 모든 태스크 끝에서 돌린다**: `go build ./...` · `go vet ./...` · `go test ./... -count=1` · `gofumpt -l .`(무출력이어야 한다) · `golangci-lint run`. 하나라도 빼지 않는다.
- **자기 파일 전체를 다시 쓰지 않는다** — 기존 함수·주석을 보존하고 필요한 자리만 고친다.
- **긴 테스트 데이터는 `strings.Repeat`로 만든다.** 리터럴로 붙여넣지 않는다.
- **비밀값 리터럴 금지.** 필요하면 런타임 분할(`"xox"+"b-..."`).
- **주석은 한국어, 기존 밀도에 맞춘다.** 왜 그렇게 했는지를 적고, 무엇을 하는지는 코드가 말하게 둔다.
- **설계 근거는 `docs/context-router-design-v0.20-ko.md`** (D102·D103·D104)다. 계약 번호를 주석에 인용한다.
- **`ledger`는 best-effort 보조 DB다** — 실패는 삼키는 것이 기존 관례(`_, _ =`)이고 그 관례를 깨지 않는다.
- **훅은 항상 exit 0(fail-open)** — 계측 추가가 그 성질을 바꾸면 안 된다.

---

# 1단계 — v0.19.1 (D102)

### Task 1: `Store.MergeFTS` — 병합 원시 명령

**Files:**
- Modify: `internal/store/store.go` (`checkFTSIntegrity` 바로 아래, 783행 근처)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: 없음
- Produces: `func (s *Store) MergeFTS(ctx context.Context) error`

- [ ] **Step 1: 실패하는 테스트를 쓴다**

`internal/store/store_test.go` 끝에 붙인다.

```go
// TestMergeFTSShrinksIndex: 삭제가 남긴 세그먼트 표식을 MergeFTS가 걷어낸다.
// FTS5 외부 콘텐츠 테이블의 삭제는 tombstone을 새 세그먼트에 쌓을 뿐이라, 병합 없이는
// 행을 다 지워도 _data 바이트가 줄지 않는다 — 그것이 D102가 고치는 결함이고, 이 테스트가
// 고정하는 것도 "삭제만으로는 안 준다"와 "병합하면 준다" 두 가지다.
func TestMergeFTSShrinksIndex(t *testing.T) {
	st := openAt(t, t.TempDir())
	body := strings.Repeat("alpha beta gamma delta epsilon ", 4000) // 약 120 KB/건
	for i := range 20 {
		regSource(t, st, body+strconv.Itoa(i), "text/plain",
			"shadow:Bash:seg"+strconv.Itoa(i), "hook")
	}
	seeded := ftsDataBytes(t, st, "fts_trigram_data")
	if seeded == 0 {
		t.Fatal("시드가 FTS 인덱스를 만들지 않았다 — 이 테스트가 공허 통과한다")
	}

	if _, err := st.writer.Exec(`DELETE FROM chunks`); err != nil {
		t.Fatalf("DELETE FROM chunks: %v", err)
	}
	afterDelete := ftsDataBytes(t, st, "fts_trigram_data")
	if afterDelete < seeded {
		t.Fatalf("삭제만으로 인덱스가 줄었다(%d → %d) — D102의 전제가 이 환경에서 성립하지 않는다",
			seeded, afterDelete)
	}

	if err := st.MergeFTS(t.Context()); err != nil {
		t.Fatalf("MergeFTS: %v", err)
	}
	merged := ftsDataBytes(t, st, "fts_trigram_data")
	if merged >= afterDelete {
		t.Fatalf("MergeFTS 후에도 인덱스가 줄지 않았다: %d → %d", afterDelete, merged)
	}
}

// TestMergeFTSKeepsSearchable: 병합이 검색 결과를 바꾸지 않는다 — optimize는 세그먼트를
// 합칠 뿐 색인 내용을 바꾸지 않아야 한다. 이 확인이 없으면 "줄었다"만 보고 색인을 망가뜨린
// 변경을 통과시킬 수 있다.
func TestMergeFTSKeepsSearchable(t *testing.T) {
	st := openAt(t, t.TempDir())
	regSource(t, st, strings.Repeat("needle haystack ", 2000), "text/plain",
		"shadow:Bash:keep", "hook")
	before := ftsMatchCount(t, st, "needle")
	if before == 0 {
		t.Fatal("시드가 검색되지 않는다 — 이 테스트가 공허 통과한다")
	}
	if err := st.MergeFTS(t.Context()); err != nil {
		t.Fatalf("MergeFTS: %v", err)
	}
	if after := ftsMatchCount(t, st, "needle"); after != before {
		t.Fatalf("병합이 검색 결과를 바꿨다: %d → %d", before, after)
	}
}

// ftsDataBytes: FTS5 그림자 테이블의 block 바이트 합 — 세그먼트 실점유의 직접 측정이다.
// 행 수가 아니라 바이트를 재는 이유는, 병합이 줄이는 것이 세그먼트 수가 아니라 중복
// 저장된 포스팅 바이트이기 때문이다.
func ftsDataBytes(t *testing.T, st *Store, table string) int64 {
	t.Helper()
	var n int64
	if err := st.reader.QueryRow(
		`SELECT coalesce(sum(length(block)),0) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("%s 조회: %v", table, err)
	}
	return n
}

// ftsMatchCount: porter 축의 히트 수.
func ftsMatchCount(t *testing.T, st *Store, term string) int64 {
	t.Helper()
	var n int64
	if err := st.reader.QueryRow(
		`SELECT count(*) FROM fts_porter WHERE fts_porter MATCH ?`, term).Scan(&n); err != nil {
		t.Fatalf("fts_porter MATCH: %v", err)
	}
	return n
}
```

`strconv`·`strings`가 이미 import되어 있는지 확인하고 없으면 더한다.

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/store -run 'TestMergeFTS' -count=1 -v`
Expected: 컴파일 실패 — `st.MergeFTS undefined`

- [ ] **Step 3: 최소 구현**

`internal/store/store.go`의 `checkFTSIntegrity` 바로 아래에 붙인다.

```go
// MergeFTS — D102: fts_porter·fts_trigram의 세그먼트를 하나로 병합한다. 외부 콘텐츠 FTS5의
// 삭제는 tombstone을 새 세그먼트에 쌓기만 하고 automerge(기본 4)가 그것을 따라잡지 못해,
// 병합 없이는 퍼지가 지운 몫이 파일에서 회수되지 않는다 — 실측으로 이 저장소 파일의 75.9%가
// 그렇게 쌓인 것이었다(설계 v0.20 관측 B).
//
// checkFTSIntegrity와 같은 **커밋 후 writer 경로**다. tx 안에서 부르지 않는다 — optimize는
// 자체 트랜잭션을 잡고, 삭제 tx와 한 덩어리로 묶으면 그 tx의 락 보유 시간이 병합 시간만큼
// 늘어난다(D67이 묶어 둔 예산 규율을 깬다).
//
// 어느 한쪽 실패든 그대로 반환한다. 호출자는 이 실패로 기동이나 회수를 막지 않는다 —
// 병합은 멱등이라 다음 기회에 다시 돌면 된다.
func (s *Store) MergeFTS(ctx context.Context) error {
	for _, fts := range [2]string{"fts_porter", "fts_trigram"} {
		if _, err := s.writer.ExecContext(ctx,
			"INSERT INTO "+fts+"("+fts+") VALUES('optimize')"); err != nil {
			return fmt.Errorf("store: %s optimize 실패: %w", fts, err)
		}
	}
	return nil
}
```

- [ ] **Step 4: 통과를 확인한다**

Run: `go test ./internal/store -run 'TestMergeFTS' -count=1 -v`
Expected: PASS 둘

- [ ] **Step 5: 게이트 다섯**

```bash
go build ./... && go vet ./... && go test ./... -count=1 && gofumpt -l . && golangci-lint run
```

- [ ] **Step 6: 커밋**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -m "feat(store): MergeFTS — FTS5 세그먼트를 병합한다 (D102)"
```

---

### Task 2: 하루 한 번 게이트 — 스탬프 파일

**Files:**
- Modify: `internal/store/store.go` (Task 1의 `MergeFTS` 아래)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: `(*Store).MergeFTS`
- Produces: `func (s *Store) MergeFTSIfDue(ctx context.Context, interval time.Duration, now time.Time) (bool, error)`

- [ ] **Step 1: 실패하는 테스트를 쓴다**

```go
// TestMergeFTSIfDueStamp: 스탬프가 없으면 돌고, 방금 돌았으면 안 돌고, interval이 지나면
// 다시 돈다. 조건이 **시간 하나**라는 것이 계약이다 — 삭제 건수는 조건에 들어가지 않는다
// (설계 v0.20 D102 계약 2: 세그먼트는 삽입으로도 쌓이므로 건수 문턱은 삽입만 있는 구간에서
// 병합을 영영 막는다).
func TestMergeFTSIfDueStamp(t *testing.T) {
	dir := t.TempDir()
	st := openAt(t, dir)
	base := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)

	ran, err := st.MergeFTSIfDue(t.Context(), 24*time.Hour, base)
	if err != nil || !ran {
		t.Fatalf("스탬프 없음에서 안 돌았다: ran=%v err=%v", ran, err)
	}
	ran, err = st.MergeFTSIfDue(t.Context(), 24*time.Hour, base.Add(23*time.Hour))
	if err != nil || ran {
		t.Fatalf("interval 이내에 또 돌았다: ran=%v err=%v", ran, err)
	}
	ran, err = st.MergeFTSIfDue(t.Context(), 24*time.Hour, base.Add(25*time.Hour))
	if err != nil || !ran {
		t.Fatalf("interval 경과 후 안 돌았다: ran=%v err=%v", ran, err)
	}
}

// TestMergeFTSIfDueFailureDoesNotStamp: 병합이 실패하면 스탬프를 찍지 않는다 — 찍으면 그
// 프로젝트는 하루 동안 재시도조차 하지 않는다. writer를 닫아 실패를 만든다.
func TestMergeFTSIfDueFailureDoesNotStamp(t *testing.T) {
	dir := t.TempDir()
	st := openAt(t, dir)
	if err := st.writer.Close(); err != nil {
		t.Fatalf("writer Close: %v", err)
	}
	now := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	if _, err := st.MergeFTSIfDue(t.Context(), 24*time.Hour, now); err == nil {
		t.Fatal("닫힌 writer에서 오류가 나지 않았다")
	}
	if _, err := os.Stat(filepath.Join(dir, mergeStampName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("실패했는데 스탬프가 찍혔다: %v", err)
	}
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/store -run 'TestMergeFTSIfDue' -count=1 -v`
Expected: 컴파일 실패 — `MergeFTSIfDue`·`mergeStampName` undefined

- [ ] **Step 3: 최소 구현**

```go
// mergeStampName — D102 계약 2의 "하루 한 번"을 재는 자리. content.db 스키마를 건드리지
// 않으려고 파일 mtime을 쓴다 — 스키마에 넣으면 user_version 축과 구 바이너리 호환을 함께
// 건드려야 하고, 그 값은 이 한 타임스탬프에 비례하지 않는다. 같은 디렉터리에
// content.db.rebuild.lock이 이미 같은 부류로 있다.
const mergeStampName = "fts-merge.stamp"

// MergeFTSIfDue — D102 계약 2: 마지막 병합에서 interval이 지났을 때만 MergeFTS를 돌리고,
// **성공했을 때만** 스탬프를 갱신한다. 돌았으면 true.
//
// 조건은 시간 하나다. 퍼지가 몇 건을 지웠는지는 보지 않는다 — 세그먼트는 삽입으로도 쌓이므로
// 건수 문턱은 삽입만 있고 삭제가 적은 구간에서 병합을 영영 막는다(설계 v0.20 D102 계약 2).
//
// 스탬프를 못 읽는 어떤 사유(부재·권한·손상)도 "돌 때가 됐다"로 읽는다 — 병합은 멱등이고,
// 못 읽어서 영영 안 도는 쪽이 더 나쁜 실패다. 반대로 스탬프 **쓰기** 실패는 무시한다:
// 다음 기회에 한 번 더 도는 것이 전부다.
//
// 프로세스 둘이 동시에 기동하면 둘 다 스탬프를 낡은 것으로 보고 병합할 수 있다. 그래도
// 무해하다 — 쓰기 락이 둘을 직렬화하고, 뒤엣것은 이미 병합된 인덱스를 다시 병합할 뿐이다.
func (s *Store) MergeFTSIfDue(ctx context.Context, interval time.Duration, now time.Time) (bool, error) {
	stamp := filepath.Join(s.dir, mergeStampName)
	if fi, err := os.Stat(stamp); err == nil && now.Sub(fi.ModTime()) < interval {
		return false, nil
	}
	if err := s.MergeFTS(ctx); err != nil {
		return false, err
	}
	if f, err := os.OpenFile(stamp, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		_ = f.Close()
	}
	_ = os.Chtimes(stamp, now, now)
	return true, nil
}
```

- [ ] **Step 4: 통과를 확인한다**

Run: `go test ./internal/store -run 'TestMergeFTSIfDue' -count=1 -v`
Expected: PASS 둘

- [ ] **Step 5: 게이트 다섯**

```bash
go build ./... && go vet ./... && go test ./... -count=1 && gofumpt -l . && golangci-lint run
```

- [ ] **Step 6: 커밋**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -m "feat(store): 병합을 하루 한 번으로 묶는 스탬프 (D102 계약 2)"
```

---

### Task 3: 기동 퍼지 고루틴에 병합을 붙인다

**Files:**
- Modify: `cmd/context-router/main.go:640-704` (D67 기동 퍼지 고루틴)
- Test: `cmd/context-router/main_test.go`

**Interfaces:**
- Consumes: `(*Store).MergeFTSIfDue`
- Produces: `const defaultFTSMergeInterval = 24 * time.Hour`

- [ ] **Step 1: 실패하는 테스트를 쓴다**

```go
// TestFTSMergeIntervalIsDaily: 기동 경로가 쓰는 병합 주기가 하루다. 상수 하나를 고정하는
// 테스트인 이유는 이 값이 D67의 락 보유 규율과 맞물려 있어서다 — 정상상태 병합이 쓰기 락을
// 약 1.2초 잡는데 훅 Read 가드의 총예산이 2000 ms다(설계 v0.20 D102 계약 2). 매 기동으로
// 낮추는 변경은 이 테스트를 지나야 한다.
func TestFTSMergeIntervalIsDaily(t *testing.T) {
	if defaultFTSMergeInterval != 24*time.Hour {
		t.Fatalf("병합 주기 = %v, 기대 24h — 설계 v0.20 D102 계약 2를 먼저 고쳐라",
			defaultFTSMergeInterval)
	}
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./cmd/context-router -run TestFTSMergeIntervalIsDaily -count=1 -v`
Expected: 컴파일 실패 — `defaultFTSMergeInterval undefined`

- [ ] **Step 3: 상수와 호출을 넣는다**

`main.go`의 `startupPurgeMaxHashes` 선언 근처에 상수를 둔다.

```go
// defaultFTSMergeInterval — D102 계약 2. 세그먼트 축적이 하루 약 36 MB이고 정상상태 병합이
// 약 1.2초 쓰기 락을 잡는다(설계 v0.20 관측 B). 매 기동마다 돌리면 그 락 보유가 훅 Read
// 가드의 2000 ms 총예산과 겹치므로 하루 한 번으로 묶는다.
const defaultFTSMergeInterval = 24 * time.Hour
```

고루틴 안, 703행의 `switch` 닫는 괄호 **다음**·`}()` **앞**에 붙인다.

```go
		// D102: 퍼지가 지운 몫은 FTS 세그먼트에 tombstone으로 남아 병합 전까지 파일에서
		// 회수되지 않는다. 퍼지가 정상 종료했을 때만 돈다 — purgeErr가 있으면 예산이 이미
		// 소진됐거나 서버가 종료 중이라, 그 위에 병합을 얹는 것은 같은 예산을 두 번 쓰는
		// 것이다. 실패는 로그만 남기고 기동을 막지 않는다(D67과 같은 계약).
		if purgeErr == nil {
			if ran, mergeErr := st.MergeFTSIfDue(purgeCtx, defaultFTSMergeInterval, time.Now()); mergeErr != nil {
				slog.Warn("기동 FTS 병합 실패 — 다음 기동에서 재시도", "error", mergeErr)
			} else if ran {
				slog.Info("기동 FTS 병합 완료")
			}
		}
```

- [ ] **Step 4: 통과를 확인한다**

Run: `go test ./cmd/context-router -run TestFTSMergeIntervalIsDaily -count=1 -v`
Expected: PASS

- [ ] **Step 5: 게이트 다섯**

```bash
go build ./... && go vet ./... && go test ./... -count=1 && gofumpt -l . && golangci-lint run
```

- [ ] **Step 6: 커밋**

```bash
git add cmd/context-router/main.go cmd/context-router/main_test.go
git commit -m "feat(server): 기동 퍼지 뒤 하루 한 번 FTS 병합 (D102)"
```

---

### Task 4: 수동 회수 경로 둘이 VACUUM 앞에 병합한다

**Files:**
- Modify: `internal/cli/cli.go:927-940` (`runPurge`의 `--vacuum` 분기)
- Modify: `internal/cli/cli.go:1033-1040` (`runPurgeHookOnly`)
- Test: `internal/cli/cli_test.go`

**Interfaces:**
- Consumes: `(*Store).MergeFTS`
- Produces: 없음 (동작 변경)

**배경**: 지금 두 경로는 VACUUM만 한다. 실측으로 그것은 회수 가능분의 **29.6%만**(159,408,128 / 539,099,136) 되돌린다 — 나머지는 병합해야 free page가 된다. 자동 경로와 달리 여기는 **주기 게이트를 걸지 않는다**: 사용자가 회수를 명시로 요청한 자리다.

- [ ] **Step 1: 실패하는 테스트를 쓴다**

```go
// TestPurgeHookOnlyMergesBeforeVacuum: --hook-only가 VACUUM 전에 FTS를 병합한다.
// 병합 없이 VACUUM만 하면 tombstone이 free page가 되지 않아 파일이 거의 그대로다 —
// 실측 기준으로 회수 가능분의 29.6%만 돌아온다(설계 v0.20 D102 계약 4).
// 판정은 파일 크기가 아니라 **삭제 후 남은 FTS _data 바이트**로 한다: VACUUM은 라이브
// 프로세스 제약을 받아 테스트에서 불안정하지만, 병합 여부는 결정적이다.
func TestPurgeHookOnlyMergesBeforeVacuum(t *testing.T) {
	// (기존 purge CLI 테스트의 프로젝트 셋업 헬퍼를 그대로 쓴다 — cli_test.go에서 이미
	//  runPurgeHookOnly를 부르는 테스트가 있으면 그 셋업을 재사용한다.)
	// 1) shadow 소스로 20건을 시드해 FTS를 채운다
	// 2) runPurgeHookOnly 실행
	// 3) content.db를 read-only로 열어 fts_trigram_data 바이트가 0에 가까운지 본다
	//    (행이 다 지워졌고 병합까지 됐으면 남는 것은 빈 구조 몇 바이트뿐이다)
}
```

**주의**: `internal/cli`의 기존 purge 테스트 셋업(프로젝트 디렉터리 생성 + shadow 소스 시드)을 먼저 읽고 그 헬퍼를 재사용한다. 없으면 `internal/store`의 `openAt`/`regSource`와 같은 형태로 이 파일 안에 최소 헬퍼를 만든다 — **다른 패키지의 테스트 헬퍼를 export하지 않는다.**

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/cli -run TestPurgeHookOnlyMergesBeforeVacuum -count=1 -v`
Expected: FAIL — 병합이 없어 `fts_trigram_data`가 여전히 크다

- [ ] **Step 3: 두 자리에 병합을 넣는다**

`runPurgeHookOnly`(1039행 `vacErr = vacuumReclaim(...)` **앞**):

```go
		// D102 계약 4: VACUUM은 free page만 되돌린다. 삭제가 남긴 FTS tombstone은 병합해야
		// free page가 되므로, 병합 없이 VACUUM만 하면 회수 가능분의 약 30%만 돌아온다.
		// 사용자가 회수를 명시로 요청한 자리이므로 주기 게이트를 걸지 않는다. 실패해도
		// VACUUM은 진행한다 — 부분 회수가 무회수보다 낫고, 이미 커밋된 삭제는 유효하다.
		if mergeErr := st.MergeFTS(ctx); mergeErr != nil {
			fmt.Fprintf(stderr, "ctr: FTS 병합 실패(회수량이 줄어든다): %v\n", mergeErr)
		}
```

`runPurge`의 `--vacuum` 분기(931행 `if verr := vacuumReclaim(...)` **앞**)에 같은 블록을 넣는다. 그 자리의 `stderr` 변수명이 다르면 맞춘다.

- [ ] **Step 4: 통과를 확인한다**

Run: `go test ./internal/cli -run 'TestPurge' -count=1 -v`
Expected: 새 테스트 PASS, 기존 purge 테스트 전부 PASS

- [ ] **Step 5: 게이트 다섯**

```bash
go build ./... && go vet ./... && go test ./... -count=1 && gofumpt -l . && golangci-lint run
```

- [ ] **Step 6: 커밋**

```bash
git add internal/cli/cli.go internal/cli/cli_test.go
git commit -m "feat(cli): 수동 회수가 VACUUM 앞에 FTS를 병합한다 (D102 계약 4)"
```

---

### Task 5: `doctor [14]` — 임계 재설정과 회수 가능 바이트

**Files:**
- Modify: `internal/store/store.go` (`SizeStat` 구조체 950-959, `SizeStats` 본문 1001행 근처)
- Modify: `internal/cli/cli.go:50` (임계 상수), `internal/cli/cli.go:1878`·`1887` (출력·경고 문면)
- Test: `internal/store/store_test.go`, `internal/cli/cli_test.go`

**Interfaces:**
- Consumes: 없음
- Produces: `SizeStat.FreeBytes int64`

- [ ] **Step 1: 실패하는 테스트를 쓴다**

```go
// TestSizeStatsReportsFreeBytes: SizeStats가 회수 가능 바이트(free page)를 낸다.
// 이 값이 없으면 "파일이 크다"와 "파일에 쓰레기가 있다"를 doctor에서 가를 수 없고,
// 그 구분이 없어서 D67의 임계가 한 달 동안 죽은 신호였다(설계 v0.20 관측 B).
func TestSizeStatsReportsFreeBytes(t *testing.T) {
	dir := t.TempDir()
	st := openAt(t, dir)
	body := strings.Repeat("free page seed ", 8000)
	for i := range 10 {
		regSource(t, st, body+strconv.Itoa(i), "text/plain",
			"shadow:Bash:free"+strconv.Itoa(i), "hook")
	}
	if _, err := st.writer.Exec(`DELETE FROM chunks`); err != nil {
		t.Fatalf("DELETE FROM chunks: %v", err)
	}
	if err := st.MergeFTS(t.Context()); err != nil {
		t.Fatalf("MergeFTS: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sz, err := SizeStats(dir)
	if err != nil || sz == nil {
		t.Fatalf("SizeStats: sz=%v err=%v", sz, err)
	}
	if sz.FreeBytes <= 0 {
		t.Fatalf("삭제+병합 뒤인데 FreeBytes=%d", sz.FreeBytes)
	}
	if sz.FreeBytes > sz.FileBytes {
		t.Fatalf("FreeBytes(%d)가 FileBytes(%d)보다 크다", sz.FreeBytes, sz.FileBytes)
	}
}

// TestContentFileWarnDefaultIs256MiB: 임계가 256 MiB다. 정리 후 정상상태가 실측 171 MB이므로
// 옛 100 MiB는 정리 뒤에도 상시 초과라 신호로서 죽는다. 256 MiB는 그 1.5배이고,
// 창을 7일로 늘리는 결정(약 400 MB)이 이 신호를 켜도록 고른 값이다(설계 v0.20 D102 계약 6).
func TestContentFileWarnDefaultIs256MiB(t *testing.T) {
	if defaultContentFileWarnBytes != 256<<20 {
		t.Fatalf("임계 = %d, 기대 %d", defaultContentFileWarnBytes, 256<<20)
	}
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/store -run TestSizeStatsReportsFreeBytes -count=1 -v && go test ./internal/cli -run TestContentFileWarnDefaultIs256MiB -count=1 -v`
Expected: 컴파일 실패(`FreeBytes` 없음) · FAIL(임계 100 MiB)

- [ ] **Step 3: 구현**

`SizeStat`에 필드를 더한다:

```go
	FreeBytes          int64            // free page × page_size — 회수 가능분(D102 계약 7)
```

`SizeStats`의 `st := SizeStat{...}` 직후에 붙인다:

```go
	// D102 계약 7: 회수 가능분을 낸다. free page는 삭제가 만들고 이후 기록이 재사용하므로,
	// 이 값이 크다는 것은 "파일이 크다"가 아니라 "지운 만큼이 아직 파일 안에 있다"는 뜻이다.
	// 두 pragma 모두 best-effort — 실패하면 0으로 두고 진단 전체를 실패시키지 않는다.
	var pageSize, freeCount int64
	if err := db.QueryRow("PRAGMA page_size").Scan(&pageSize); err == nil {
		if err := db.QueryRow("PRAGMA freelist_count").Scan(&freeCount); err == nil {
			st.FreeBytes = pageSize * freeCount
		}
	}
```

`internal/cli/cli.go:50`:

```go
const defaultContentFileWarnBytes = 256 << 20 // 256MiB — D102 계약 6(정리 후 정상상태 실측 171 MB의 약 1.5배)
```

`internal/cli/cli.go:1878` 출력 줄에 `free=`를 더한다:

```go
		fmt.Fprintf(w, "[14] content.db: sources=%d artifacts=%d blob=%dB file=%dB free=%dB\n",
			sz.Sources, sz.Artifacts, sz.BlobBytes, sz.FileBytes, sz.FreeBytes)
```

`internal/cli/cli.go:1887` 경고 문면을 바꾼다 — 옛 문면은 회수 수단으로 VACUUM만 적는데, D102 뒤로는 기동 병합이 성장을 실제로 억제하고 VACUUM은 파일 축소 전용이다:

```go
			fmt.Fprintf(w, "[14] warning: file %dB > 임계 %dB(CTR_CONTENT_FILE_WARN_BYTES) — 청크 텍스트+FTS 축(자문). free=%dB는 이미 회수돼 재사용을 기다리는 몫이다. 파일 축소는 VACUUM(라이브 서버 제약 — 서버 비가동 시 purge --older-than --vacuum), --hook-only는 shadow 귀속 한정(explicit 소스 감축은 전체 purge). 자동 삭제 없음\n",
				sz.FileBytes, warn, sz.FreeBytes)
```

- [ ] **Step 4: 통과를 확인한다**

Run: `go test ./internal/store ./internal/cli -count=1`
Expected: PASS. `doctor` 출력 문면을 고정한 기존 테스트가 있으면 함께 고친다.

- [ ] **Step 5: 실물 확인**

Run: `go run ./cmd/context-router doctor | grep '^\[14\]'`
Expected: `free=`가 보이고, 경고가 뜬다면 새 임계(268435456)를 인용한다.

- [ ] **Step 6: 게이트 다섯 + 커밋**

```bash
go build ./... && go vet ./... && go test ./... -count=1 && gofumpt -l . && golangci-lint run
git add internal/store internal/cli
git commit -m "feat(doctor): 회수 가능 바이트를 낸다 + 임계 256MiB (D102 계약 6·7)"
```

---

### Task 6: v0.19.1 배송

**Files:**
- Modify: `CHANGELOG.md`
- Modify: 버전 문자열이 사는 자리 (`go run ./cmd/context-router doctor`의 `[17] build`가 읽는 곳)

- [ ] **Step 1: 버전을 올린다**

`grep -rn "0\.19\.0" --include=*.go . | grep -v _test` 로 자리를 찾아 `0.19.1`로 올린다.

- [ ] **Step 2: CHANGELOG `Unreleased` 아래에 `[0.19.1]` 절을 만든다**

```markdown
## [0.19.1] — 2026-08-XX

### Fixed

- **FTS 세그먼트가 병합되지 않아 저장 파일의 76%가 회수되지 않고 쌓였다** (D102). 외부 콘텐츠
  FTS5의 삭제는 tombstone을 새 세그먼트에 쌓을 뿐이고, 이 저장소에는 병합을 부르는 코드가
  없었다 — D67의 72시간 퍼지가 지운 몫이 파일에서 돌아오지 않았다. 실측: 파일
  709,890,048 B 중 539,099,136 B가 회수 가능분이었고, `optimize` 뒤 170,790,912 B가 됐다.
  기동 퍼지가 하루 한 번 병합하고, 수동 회수 경로 둘은 VACUUM 앞에 병합한다.
- **`purge --vacuum`·`--hook-only`가 회수 가능분의 약 30%만 되돌리던 것**을 고쳤다 — VACUUM은
  free page만 되돌리고 tombstone은 병합해야 free page가 된다.

### Changed

- `doctor [14]`가 **회수 가능 바이트(`free=`)** 를 병기한다. "파일이 크다"와 "지운 만큼이 아직
  파일 안에 있다"를 가르는 값이고, 그 구분이 없어서 임계가 한 달 동안 죽은 신호였다.
- `CTR_CONTENT_FILE_WARN_BYTES` 기본값을 **100 MiB → 256 MiB**로 올린다. 정리 후 정상상태가
  실측 171 MB이므로 옛 값은 정리 뒤에도 상시 초과다.
```

- [ ] **Step 3: 게이트 다섯 + 실물 확인**

```bash
go build ./... && go vet ./... && go test ./... -count=1 && gofumpt -l . && golangci-lint run
go run ./cmd/context-router doctor | grep -E '^\[14\]|^\[17\]'
```

- [ ] **Step 4: 커밋**

```bash
git add CHANGELOG.md
git commit -m "chore(release): 0.19.1 — FTS 세그먼트 병합 (D102)"
```

---

# 2단계 — v0.20 (D103)

### Task 7: `ledger` 열 둘 + read-only 내성

**Files:**
- Modify: `internal/store/store.go:151-161` (ledger 초기화), `915-948` (`LedgerStats` 아래)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: 없음
- Produces: `type FetchStat struct { Resolved, Missed int64; AgeP50, AgeP90, AgeMax int64 }`, `func LedgerFetchStats(dir string) (FetchStat, error)`

- [ ] **Step 1: 실패하는 테스트를 쓴다**

```go
// TestLedgerFetchStatsToleratesOldSchema: 새 열이 없는 옛 ledger.db를 만나도 실패하지 않는다.
// ALTER는 writable Open에서만 도는데 LedgerFetchStats는 ledger.db를 따로 read-only로 연다 —
// 새 바이너리의 stats가 아직 이관되지 않은 원장을 먼저 만날 수 있다(설계 v0.20 D103 계약 7).
func TestLedgerFetchStatsToleratesOldSchema(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "ledger.db")))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE ledger(
		id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, tool TEXT NOT NULL,
		bytes_stored INTEGER NOT NULL DEFAULT 0, bytes_returned INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatalf("옛 스키마 생성: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fs, err := LedgerFetchStats(dir)
	if err != nil {
		t.Fatalf("옛 스키마에서 오류가 났다: %v", err)
	}
	if fs.Resolved != 0 || fs.Missed != 0 {
		t.Fatalf("옛 스키마에서 0이 아닌 값이 나왔다: %+v", fs)
	}
}

// TestLedgerColumnsAddedOnWritableOpen: writable Open이 두 열을 붙인다. 두 번 열어도
// "duplicate column name"이 밖으로 새지 않는다(ledger 전체가 best-effort라 삼킨다).
func TestLedgerColumnsAddedOnWritableOpen(t *testing.T) {
	dir := t.TempDir()
	st := openAt(t, dir)
	if err := st.Close(); err != nil {
		t.Fatalf("첫 Close: %v", err)
	}
	st2 := openAt(t, dir) // 두 번째 Open에서 ALTER가 중복 실패한다 — 그래도 열려야 한다
	if err := st2.Close(); err != nil {
		t.Fatalf("둘째 Close: %v", err)
	}
	if _, err := LedgerFetchStats(dir); err != nil {
		t.Fatalf("LedgerFetchStats: %v", err)
	}
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/store -run 'TestLedger' -count=1 -v`
Expected: 컴파일 실패 — `LedgerFetchStats` undefined

- [ ] **Step 3: 구현**

`store.go`의 ledger `CREATE TABLE` 직후:

```go
			// D103: artifact 단위 회수 실적 두 열. 옛 ledger.db에도 붙여야 하므로 ALTER를
			// 쓰는데, 이미 있으면 "duplicate column name"으로 실패한다 — ledger 전체가
			// best-effort이므로 그 실패를 그대로 무시한다(위 CREATE와 같은 `_, _ =` 관례).
			// artifact_id가 NULL인 ctr_fetch 행은 "해소되지 않았다"를 뜻한다.
			_, _ = l.Exec(`ALTER TABLE ledger ADD COLUMN artifact_id INTEGER`)
			_, _ = l.Exec(`ALTER TABLE ledger ADD COLUMN artifact_age_s INTEGER`)
```

`LedgerStats` 아래:

```go
// FetchStat: D103 회수 실적. Resolved는 artifact를 실제로 돌려준 ctr_fetch, Missed는
// ErrNotFound로 끝난 ctr_fetch다. Age*는 **회수 시점에 박아 둔** 나이(초)의 분포 —
// 아티팩트가 나중에 지워져도 남는다는 것이 이 계측의 요지다(설계 v0.20 D103 계약 2).
type FetchStat struct {
	Resolved, Missed       int64
	AgeP50, AgeP90, AgeMax int64
}

// LedgerFetchStats: dir/ledger.db를 read-only로 열어 회수 실적을 낸다.
// 파일 미존재는 LedgerStats와 동일하게 빈 값+nil. **새 열이 없는 옛 원장도 빈 값+nil이다** —
// ALTER는 writable Open에서만 돌므로 이 경로가 이관 전 원장을 먼저 만날 수 있고, 그것은
// 손상이 아니다(설계 v0.20 D103 계약 7). 그 외 오류는 삼키지 않는다.
func LedgerFetchStats(dir string) (FetchStat, error) {
	var fs FetchStat
	path := filepath.Join(dir, "ledger.db")
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fs, nil
		}
		return fs, sanitizeIOErr("ledger stat", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return fs, fmt.Errorf("store LedgerFetchStats: %w", err)
	}
	defer db.Close()

	if err := db.QueryRow(`SELECT
			coalesce(sum(artifact_id IS NOT NULL),0),
			coalesce(sum(artifact_id IS NULL),0)
		FROM ledger WHERE tool='ctr_fetch'`).Scan(&fs.Resolved, &fs.Missed); err != nil {
		if strings.Contains(err.Error(), "no such column") {
			return FetchStat{}, nil // 이관 전 원장 — 손상이 아니다
		}
		return FetchStat{}, fmt.Errorf("store LedgerFetchStats: %w", err)
	}
	if fs.Resolved == 0 {
		return fs, nil
	}
	// 분위수는 SQLite에 내장이 없으므로 정렬 + OFFSET으로 낸다. 행 수가 수만 단위라
	// 이 방식으로 충분하다(하루 약 300행 × 무기한 보존 = 1년 11만 행).
	for _, q := range []struct {
		dst    *int64
		offset int64
	}{
		{&fs.AgeP50, (fs.Resolved - 1) * 50 / 100},
		{&fs.AgeP90, (fs.Resolved - 1) * 90 / 100},
		{&fs.AgeMax, fs.Resolved - 1},
	} {
		if err := db.QueryRow(`SELECT artifact_age_s FROM ledger
			WHERE tool='ctr_fetch' AND artifact_id IS NOT NULL
			ORDER BY artifact_age_s LIMIT 1 OFFSET ?`, q.offset).Scan(q.dst); err != nil {
			return FetchStat{}, fmt.Errorf("store LedgerFetchStats: %w", err)
		}
	}
	return fs, nil
}
```

`strings`가 import되어 있는지 확인한다.

- [ ] **Step 4: 통과를 확인한다**

Run: `go test ./internal/store -run 'TestLedger' -count=1 -v`
Expected: PASS 둘

- [ ] **Step 5: 게이트 다섯 + 커밋**

```bash
go build ./... && go vet ./... && go test ./... -count=1 && gofumpt -l . && golangci-lint run
git add internal/store
git commit -m "feat(store): ledger에 회수 실적 두 열 + 이관 전 원장 내성 (D103 계약 1·7)"
```

---

### Task 8: `ctr_fetch`가 해소와 미해소를 모두 남긴다

**Files:**
- Modify: `internal/store/store.go` (`LedgerAppend` 아래)
- Modify: `internal/mcp/mcp.go:538-575` (fetch 핸들러)
- Test: `internal/store/store_test.go`, `internal/mcp/mcp_test.go`

**Interfaces:**
- Consumes: Task 7의 두 열
- Produces:
  - `func (s *Store) LastIndexedAt(ctx context.Context, artifactID int64) (int64, error)`
  - `func (s *Store) LedgerAppendFetch(returned, ms, artifactID, ageS int64)`

**나이의 시계가 무엇인가 — 이 태스크의 핵심 판단.** 나이는 `artifacts.created_at`이 아니라 **`max(sources.indexed_at)`** 로 잰다. D67의 퍼지가 쓰는 시계가 그것이고(마지막 포착), 내용 주소 저장이라 같은 바이트가 다시 포착되면 `created_at`은 첫 포착에 고정된 채 `indexed_at`만 움직인다. `created_at`으로 재면 재포착된 아티팩트가 실제보다 늙어 보이고, **그 오차가 창을 늘리는 쪽으로만 작용한다.**

- [ ] **Step 1: 실패하는 테스트를 쓴다 (store)**

```go
// TestLastIndexedAtUsesMaxOverSources: 나이 시계가 마지막 포착이다. D67 퍼지가 "그 hash를
// 참조하는 소스 중 하나라도 경계 이후에 색인됐으면 대상에서 뺀다"로 판정하므로, 회수 나이도
// 같은 시계여야 분포가 창 위에 겹쳐진다. created_at으로 재면 재포착된 아티팩트가 실제보다
// 늙어 보이고 그 오차는 창을 늘리는 쪽으로만 작용한다.
func TestLastIndexedAtUsesMaxOverSources(t *testing.T) {
	st := openAt(t, t.TempDir())
	regSource(t, st, "aged", "text/plain", "shadow:Bash:first", "hook")
	regSource(t, st, "aged", "text/plain", "shadow:Bash:second", "hook") // 같은 바이트 재포착

	var id int64
	if err := st.reader.QueryRow(
		`SELECT id FROM artifacts WHERE content_hash=?`, hashOf("aged")).Scan(&id); err != nil {
		t.Fatalf("artifact id: %v", err)
	}
	old, recent := int64(1_000), int64(9_000)
	if _, err := st.writer.Exec(
		`UPDATE sources SET indexed_at=? WHERE uri='shadow:Bash:first'`, old); err != nil {
		t.Fatalf("첫 소스 시각: %v", err)
	}
	if _, err := st.writer.Exec(
		`UPDATE sources SET indexed_at=? WHERE uri='shadow:Bash:second'`, recent); err != nil {
		t.Fatalf("둘째 소스 시각: %v", err)
	}

	got, err := st.LastIndexedAt(t.Context(), id)
	if err != nil {
		t.Fatalf("LastIndexedAt: %v", err)
	}
	if got != recent {
		t.Fatalf("LastIndexedAt=%d, 기대 %d(최댓값)", got, recent)
	}
}

// TestLedgerAppendFetchNullsOnMiss: artifactID<=0이면 두 열이 NULL로 남는다 — 그것이
// "해소되지 않았다"의 기록이다(설계 v0.20 D103 계약 3).
func TestLedgerAppendFetchNullsOnMiss(t *testing.T) {
	dir := t.TempDir()
	st := openAt(t, dir)
	st.LedgerAppendFetch(0, 1, 42, 3600) // 해소
	st.LedgerAppendFetch(0, 1, 0, 0)     // 미해소
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fs, err := LedgerFetchStats(dir)
	if err != nil {
		t.Fatalf("LedgerFetchStats: %v", err)
	}
	if fs.Resolved != 1 || fs.Missed != 1 {
		t.Fatalf("resolved=%d missed=%d, 기대 1/1", fs.Resolved, fs.Missed)
	}
	if fs.AgeMax != 3600 {
		t.Fatalf("AgeMax=%d, 기대 3600", fs.AgeMax)
	}
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/store -run 'TestLastIndexedAt|TestLedgerAppendFetch' -count=1 -v`
Expected: 컴파일 실패

- [ ] **Step 3: store 구현**

```go
// LastIndexedAt — D103 계약 2: 이 아티팩트의 **마지막 포착** 시각(unix 초). D67 퍼지가 쓰는
// 시계와 같은 것을 쓴다 — 퍼지 술어가 hash 단위 max(indexed_at)이므로 나이도 그렇게 재야
// 분포가 보존 창 위에 그대로 겹쳐진다. 소스가 없으면 (0, nil).
// 인덱스 idx_sources_artifact_indexed가 이 질의를 덮는다.
func (s *Store) LastIndexedAt(ctx context.Context, artifactID int64) (int64, error) {
	var ts sql.NullInt64
	if err := s.reader.QueryRowContext(ctx,
		`SELECT max(indexed_at) FROM sources WHERE artifact_id = ?`, artifactID).Scan(&ts); err != nil {
		return 0, fmt.Errorf("store LastIndexedAt: %w", err)
	}
	if !ts.Valid {
		return 0, nil
	}
	return ts.Int64, nil
}

// LedgerAppendFetch — D103: ctr_fetch 전용 원장 기록. artifactID<=0이면 두 열을 NULL로 남겨
// **해소되지 않은 회수**를 기록한다 — 그것이 "창이 짧아서 못 찾았다"의 유일한 직접 증거이고,
// 성공 기록만 남기면 영원히 나오지 않는다(설계 v0.20 D103 계약 3).
// LedgerAppend와 같은 best-effort 계약이다(ledger 없음·오류는 무시).
func (s *Store) LedgerAppendFetch(returned, ms, artifactID, ageS int64) {
	if s.ledger == nil {
		return
	}
	var idCol, ageCol any
	if artifactID > 0 {
		idCol, ageCol = artifactID, ageS
	}
	_, _ = s.ledger.Exec(
		`INSERT INTO ledger(ts,tool,bytes_stored,bytes_returned,duration_ms,artifact_id,artifact_age_s)
		 VALUES(?,'ctr_fetch',0,?,?,?,?)`,
		time.Now().Unix(), returned, ms, idCol, ageCol)
}
```

- [ ] **Step 4: mcp 핸들러를 고친다**

`internal/mcp/mcp.go`의 fetch 핸들러에서 두 자리를 고친다.

`ReadRange` 실패 분기(546행):

```go
		res, err := st.ReadRange(ctx, in.ArtifactID, sel)
		if err != nil {
			// D103 계약 3: **ErrNotFound만** 센다. 잘못된 선택자(ErrInvalidSelector)나 DB
			// 오류는 창의 길이에 대해 아무 말도 하지 않으며, 넓게 세면 오탐이 창을 늘리는
			// 근거로 둔갑한다.
			if errors.Is(err, store.ErrNotFound) {
				st.LedgerAppendFetch(0, time.Since(start).Milliseconds(), 0, 0)
			}
			return nil, FetchOutput{}, toToolError(err)
		}
```

성공 분기(573행)의 `LedgerAppend`를 갈아끼운다:

```go
		// D103 계약 2: 나이는 **회수 시점에 계산해 박는다** — 사후 계산은 아티팩트가
		// 지워지면 불가능하다. 시계는 D67 퍼지와 같은 max(sources.indexed_at)이다.
		var ageS int64
		if at, ageErr := st.LastIndexedAt(ctx, res.Artifact.ID); ageErr == nil && at > 0 {
			ageS = time.Now().Unix() - at
		}
		st.LedgerAppendFetch(jsonLen(out), time.Since(start).Milliseconds(), res.Artifact.ID, ageS)
```

`errors`·`store` import를 확인한다.

- [ ] **Step 5: mcp 테스트를 더한다**

```go
// TestFetchRecordsMissOnNotFound: 없는 artifact를 요청하면 미해소 행이 남는다.
// 잘못된 선택자는 남지 않는다 — 그 둘이 같은 뜻이 아니기 때문이다(D103 계약 3).
func TestFetchRecordsMissOnNotFound(t *testing.T) {
	// 기존 mcp 테스트의 서버 셋업 헬퍼를 재사용해 ctr_fetch를 두 번 부른다:
	//   ① 존재하지 않는 artifact_id → LedgerFetchStats().Missed == 1
	//   ② 잘못된 selector kind    → Missed 가 늘지 않는다
}
```

`internal/mcp/mcp_test.go`의 기존 서버 셋업 방식을 먼저 읽고 그대로 쓴다.

- [ ] **Step 6: 통과 확인 + 게이트 다섯 + 커밋**

```bash
go test ./internal/store ./internal/mcp -count=1
go build ./... && go vet ./... && go test ./... -count=1 && gofumpt -l . && golangci-lint run
git add internal/store internal/mcp
git commit -m "feat(fetch): 회수 성공과 ErrNotFound를 원장에 남긴다 (D103 계약 2·3)"
```

---

### Task 9: 훅 포착이 분모를 남긴다

**Files:**
- Modify: `internal/hook/shadow.go:33-99` (`shadowCapture`)
- Test: `internal/hook/hook_test.go`

**Interfaces:**
- Consumes: `(*Store).LedgerAppend` (기존 시그니처 그대로)
- Produces: 없음

**분모의 정의**: 원장의 `tool='hook:shadow'` 행 수 = **성공한 포착 건수**다. 내용 주소 저장이라 같은 바이트의 재포착은 아티팩트를 새로 만들지 않으므로, **이 수는 만들어진 고유 아티팩트 수의 상한**이다. 비율을 읽을 때 그렇게 읽는다.

- [ ] **Step 1: 실패하는 테스트를 쓴다**

```go
// TestShadowCaptureRecordsLedgerRow: 성공한 포착마다 원장에 분모 행이 하나 남는다.
// 이 분모가 없으면 회수율의 분모를 72시간 스냅샷에서 세게 되고, 그것이 세션 54가
// 상계 11.6%를 잘못 낸 형태다 — 13일치 분자를 사흘치 분모로 나눴다.
// 임계 미달·denylist 등으로 저장되지 않은 호출은 행을 남기지 않는다.
func TestShadowCaptureRecordsLedgerRow(t *testing.T) {
	// 기존 shadow 테스트의 hookInput 조립 방식을 그대로 쓴다.
	//   ① 16 KiB 초과 tool_response 로 shadowCapture 실행 → ledger 에 hook:shadow 1행
	//   ② 임계 미달 입력으로 실행 → 행이 늘지 않는다
}
```

`internal/hook/hook_test.go`의 기존 `shadowCapture` 호출 방식(임시 디렉터리·`hookInput` 조립)을 먼저 읽고 그대로 쓴다.

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/hook -run TestShadowCaptureRecordsLedgerRow -count=1 -v`
Expected: FAIL — 행이 0이다

- [ ] **Step 3: 구현**

`shadowCapture` 선두에 시작 시각을 잡는다:

```go
func shadowCapture(ctx context.Context, ad *session.AppendDB, in hookInput, dir, contentDir, external string, getenv func(string) string) {
	start := time.Now()
```

`ingest.Run` 성공 판정(88행) **직후**, `ref` 조립 앞에 붙인다:

```go
	// D103 계약 4: 회수율의 **분모**다. 훅은 writable로 스토어를 열므로(위 OpenContext의
	// readOnly=false) ledger 연결이 이미 있다 — 지금까지 쓰지 않았을 뿐이다.
	// LedgerAppend는 best-effort라 실패해도 포착 자체에 영향이 없고, 훅의 fail-open
	// 성질도 바뀌지 않는다.
	st.LedgerAppend("hook:shadow", int64(size), 0, time.Since(start).Milliseconds())
```

`time` import를 확인한다.

- [ ] **Step 4: 예산을 잰다**

훅의 총예산이 2000 ms이므로 INSERT 하나가 그 안에서 무시할 만한지 확인한다.

Run: `go test ./internal/hook -run TestShadowCapture -count=1 -v -benchtime=1x`
그리고 실물로:
```bash
go run ./cmd/context-router doctor | grep '^\[12\]'   # drops 가 늘지 않아야 한다
```
Expected: 기존 shadow 테스트 전부 PASS, drops 무변화. **INSERT 하나가 100 ms를 넘으면 이 태스크를 멈추고 보고한다** — 설계 §4-4가 미리 열어 둔 자리다.

- [ ] **Step 5: 게이트 다섯 + 커밋**

```bash
go build ./... && go vet ./... && go test ./... -count=1 && gofumpt -l . && golangci-lint run
git add internal/hook
git commit -m "feat(hook): 포착마다 원장에 분모 행을 남긴다 (D103 계약 4)"
```

---

### Task 10: `stats`가 회수 실적을 낸다

**Files:**
- Modify: `internal/cli/cli.go:200-260` (`runStatsLocal`)
- Test: `internal/cli/cli_test.go`

**Interfaces:**
- Consumes: `store.LedgerFetchStats`
- Produces: 없음

- [ ] **Step 1: 실패하는 테스트를 쓴다**

```go
// TestStatsPrintsFetchStats: stats 가 회수 실적 줄을 낸다. 이 줄이 D104의 착수 조건을
// 사람이 눈으로 확인하는 자리다 — 해소 30건 또는 미해소 5건.
func TestStatsPrintsFetchStats(t *testing.T) {
	// 임시 프로젝트에 ledger 행을 시드하고 runStatsLocal 출력을 잡아
	// "fetch:" 로 시작하는 줄에 resolved/missed/age p50·p90·max 가 있는지 본다.
}
```

`internal/cli/cli_test.go`의 기존 `runStatsLocal` 테스트 셋업을 재사용한다.

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/cli -run TestStatsPrintsFetchStats -count=1 -v`
Expected: FAIL — 그 줄이 없다

- [ ] **Step 3: 구현**

`runStatsLocal`의 도구별 표 출력이 끝난 뒤에 붙인다.

```go
	// D103: 회수 실적. D104의 착수 조건(해소 30건 또는 미해소 5건)을 여기서 읽는다.
	// 이관 전 원장에서는 전부 0이 나오고 그것은 오류가 아니다(D103 계약 7).
	if fs, err := store.LedgerFetchStats(projDir); err == nil {
		fmt.Fprintf(w, "fetch:\tresolved=%d\tmissed=%d\tage_s p50=%d p90=%d max=%d\n",
			fs.Resolved, fs.Missed, fs.AgeP50, fs.AgeP90, fs.AgeMax)
	}
```

- [ ] **Step 4: 통과 확인 + 실물**

```bash
go test ./internal/cli -count=1
go run ./cmd/context-router stats | tail -3
```

- [ ] **Step 5: 게이트 다섯 + 커밋**

```bash
go build ./... && go vet ./... && go test ./... -count=1 && gofumpt -l . && golangci-lint run
git add internal/cli
git commit -m "feat(stats): 회수 실적을 낸다 — D104 착수 조건을 읽는 자리 (D103)"
```

---

### Task 11: v0.20 배송

**Files:**
- Modify: `CHANGELOG.md`, 버전 문자열

- [ ] **Step 1: 버전을 `0.20.0`으로 올린다**

- [ ] **Step 2: CHANGELOG `[0.20.0]` 절**

```markdown
## [0.20.0] — 2026-08-XX

### Added

- **회수 실적 계측** (D103). `ledger.db`에 `artifact_id`·`artifact_age_s` 두 열이 붙고,
  `ctr_fetch`는 **해소했을 때와 `ErrNotFound`로 끝났을 때 모두** 행을 남긴다 — 앞의 것은
  "창이 충분했다", 뒤의 것은 "창이 짧았다"의 증거다. 훅 포착마다 분모 행이 하나 남는다.
  나이는 회수 시점에 박고 시계는 D67 퍼지와 같은 `max(sources.indexed_at)`이므로,
  아티팩트가 지워진 뒤에도 분포가 남는다.
- `stats`에 `fetch: resolved=… missed=… age_s p50/p90/max` 줄.

### Unchanged (의도)

- **72시간 보존 창은 그대로다** (D104). 늘리거나 줄이는 판단은 이 계측이 배포된 뒤 14일,
  그리고 해소 30건 또는 미해소 5건이 찼을 때 한다. **미달이면 "늘리지 않는다"가 결론이다** —
  설계서에 판정 규칙을 미리 박아 뒀다.
```

- [ ] **Step 3: 게이트 다섯 + 커밋**

```bash
go build ./... && go vet ./... && go test ./... -count=1 && gofumpt -l . && golangci-lint run
git add CHANGELOG.md
git commit -m "chore(release): 0.20.0 — 회수 실적 계측 (D103·D104)"
```

---

## 자기검토 결과

**스펙 대조** — v0.20 설계서의 계약과 태스크 대응:

| 계약 | 태스크 |
|---|---|
| D102-1 병합 원시 명령 | T1 |
| D102-2 하루 한 번 | T2·T3 |
| D102-3 `optimize`, 손잡이 없음 | T1 (환경 변수를 만들지 않는다) |
| D102-4 수동 회수가 병합 선행 | T4 |
| D102-5 자동 경로 VACUUM 없음 | T3 (병합만 하고 VACUUM을 부르지 않는다) |
| D102-6 임계 256 MiB | T5 |
| D102-7 회수 가능 바이트 | T5 |
| D103-1 열 둘 | T7 |
| D103-2 해소 시 나이 박기 | T8 |
| D103-3 `ErrNotFound`만 센다 | T8 |
| D103-4 훅 분모 | T9 |
| D103-5 원장 무기한 | T7·T9 (지우는 코드를 넣지 않는다) |
| D103-6 S4 정수와 이름만 | T8·T9 (선택자·경로를 담지 않는다) |
| D103-7 read-only 내성 | T7 |
| D104 착수 조건 | T10 (읽는 자리) + 설계서 (규칙 자체) |

**미확정 대응**: 설계서 §4-1(스탬프 자리) → T2에서 `fts-merge.stamp`로 확정. §4-2(라이브 optimize 소요) → T6 배포 후 관측, 계획 밖. §4-3(`checkFTSIntegrity` 대칭) → **이 계획은 대칭을 만들지 않는다**. 병합을 store 메서드로 두고 호출자가 정책을 갖는 형태라, 두 퍼지 메서드의 후처리 차이를 건드리지 않는다. §4-4(훅 예산) → T9 Step 4가 잰다. §4-5(다른 프로젝트) → 계획 밖.

**설계서와 어긋난 것 하나 — 개정으로 닫았다**: 설계서 D102 계약 1이 병합 자리를 처음에 *"`PurgeOlderThan`과 `purgeHookRows`의 커밋 직후"* 로 적었는데, 자동 경로는 하루 한 번이고 수동 경로는 무조건이라 **정책이 둘이다** — store 메서드 안에 밀어 넣으면 게이트를 인자로 넘겨야 하고 그 인자가 곧 정책이다. 계약 1을 **"원시 명령은 store, 정책은 호출자 셋"** 으로 개정했고(2026-08-09), 이 계획이 그 형태다.
