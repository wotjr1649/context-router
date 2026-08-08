# FTS 세그먼트 병합(D102)과 회수 실적 계측(D103) 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 저장 비용 눈금을 고치고(파일의 75.9%가 회수 가능한 FTS 세그먼트다), 회수 실적을 퍼지 사정권 밖인 `ledger.db`에 남겨 72시간 창을 데이터로 판단할 수 있게 만든다.

**Architecture:** 두 단계다. **1단계(v0.19.1)** 는 `Store.MergeFTS`라는 원시 명령 하나를 더하고, 정책은 호출자가 갖는다 — **자동 경로는 서버 생존 ctx에 묶인 자기 주기 고루틴**(기동 직후 한 번, 그 뒤 24시간마다 재확인)이고, 수동 회수 CLI 둘은 VACUUM 직전 무조건이다. **2단계(v0.20)** 는 `ledger`에 열 둘을 붙여 `ctr_fetch`의 해소·미해소를 artifact id와 회수 시점 나이로 남기고, 훅 포착마다 분모 행을 하나 남긴다.

**Tech Stack:** Go 1.26.5 · `modernc.org/sqlite` **v1.54.0**(번들 SQLite **3.53.3**, FTS5 external-content, porter + trigram) · stdlib only.

## Global Constraints

- **게이트 다섯을 모든 태스크 끝에서 돌린다**: `go build ./...` · `go vet ./...` · `go test ./... -count=1` · `gofumpt -l .`(무출력이어야 한다) · `golangci-lint run`. 하나라도 빼지 않는다.
- **자기 파일 전체를 다시 쓰지 않는다** — 기존 함수·주석을 보존하고 필요한 자리만 고친다.
- ★ **FTS를 재는 테스트는 `Chunks`를 명시로 실어 시드한다.** `regSource`(`internal/store/store_test.go:1717`)와 `seedShadowContentDB`(`internal/cli/cli_test.go:466`)는 **`Chunks` 없는 `Registration`** 을 만들고, `Register`는 `reg.Chunks`에서만 chunks를 INSERT한다(`internal/store/store.go:459`) — **그 헬퍼로 시드하면 chunks 행이 0개라 FTS 행도 0개**이고, 인덱스 바이트를 재는 어떤 단정도 병합 유무와 무관하게 통과한다. 시드 헬퍼를 새로 만들되 **기존 공용 헬퍼는 건드리지 않는다**(다른 테스트가 그 행수를 단정한다).
- **긴 테스트 데이터는 `strings.Repeat`로 만든다.** 리터럴로 붙여넣지 않는다.
- **비밀값 리터럴 금지.** 필요하면 런타임 분할(`"xox"+"b-..."`).
- **주석은 한국어, 기존 밀도에 맞춘다.** 왜 그렇게 했는지를 적고, 무엇을 하는지는 코드가 말하게 둔다.
- **설계 근거는 `docs/context-router-design-v0.20-ko.md`** (D102·D103·D104)다. 계약 번호를 주석에 인용한다.
- **`ledger`는 best-effort 보조 DB다** — 쓰기 실패는 삼키는 것이 기존 관례(`_, _ =`)이고 그 관례를 깨지 않는다. **읽기 경로는 다르다**: 이관 전 원장만 빈 값+nil이고 그 외 오류는 반환한다.
- **훅은 항상 exit 0(fail-open)** — 계측 추가가 그 성질을 바꾸면 안 된다.

---

# 1단계 — v0.19.1 (D102)

### Task 1: `Store.MergeFTS` — 병합 원시 명령

**Files:**
- Modify: `internal/store/store.go` (`checkFTSIntegrity`(776-783) 바로 아래)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: 없음
- Produces: `func (s *Store) MergeFTS(ctx context.Context) error`

- [ ] **Step 1: 실패하는 테스트를 쓴다**

`internal/store/store_test.go` 끝에 붙인다. `strconv`·`strings`·`time`·`os`·`filepath`·`errors`는 이미 import되어 있다(3-25행).

```go
// regChunked: regSource와 같되 Chunks를 명시로 실어 **FTS 행을 실제로 만든다**. regSource
// (store_test.go:1717)는 Chunks 없는 Registration을 만들고 Register는 reg.Chunks에서만
// chunks를 INSERT하므로(store.go:459) FTS 행이 0개다 — 인덱스 바이트를 재는 테스트가 그
// 헬퍼로 시드하면 병합 유무와 무관하게 통과한다.
func regChunked(t *testing.T, st *Store, content, uri string) {
	t.Helper()
	if _, err := st.Register(t.Context(), Registration{
		StoredBytes: []byte(content), MediaType: "text/plain",
		Source: SourceMeta{URI: uri, Kind: "hook", SrcHash: "sh-" + uri},
		Chunks: []Chunk{{Ordinal: 0, ByteEnd: int64(len(content)), Text: content}},
	}); err != nil {
		t.Fatal(err)
	}
}

// TestMergeFTSShrinksIndex: 삭제가 남긴 세그먼트 표식을 MergeFTS가 걷어낸다.
// FTS5 외부 콘텐츠 테이블의 삭제는 tombstone을 새 세그먼트에 쌓을 뿐이라, 병합 없이는
// 행을 다 지워도 _data 바이트가 줄지 않는다 — 그것이 D102가 고치는 결함이고, 이 테스트가
// 고정하는 것도 "삭제만으로는 안 준다"와 "병합하면 준다" 두 가지다.
func TestMergeFTSShrinksIndex(t *testing.T) {
	st := openAt(t, t.TempDir())
	body := strings.Repeat("alpha beta gamma delta epsilon ", 4000) // 약 120 KB/건
	for i := range 20 {
		regChunked(t, st, body+strconv.Itoa(i), "shadow:Bash:seg"+strconv.Itoa(i))
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
	regChunked(t, st, strings.Repeat("needle haystack ", 2000), "shadow:Bash:keep")
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

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/store -run 'TestMergeFTS' -count=1 -v`
Expected: 컴파일 실패 — `st.MergeFTS undefined`

- [ ] **Step 3: 최소 구현**

`internal/store/store.go`의 `checkFTSIntegrity`(776행) 바로 아래에 붙인다.

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
// 원시 명령은 'optimize'다. 'merge=N'은 검토 후 기각됐다 — merge=512 한 번이 실측 churn의
// 1/4~1/18에 그쳐 하루 한 번으로는 수렴하지 못한다(D102 계약 3). 비용 논거가 기대는 성질:
// **이미 한 세그먼트로 병합된 인덱스에 건 optimize는 일 없이 반환한다**(번들 SQLite 3.53.3 /
// modernc.org/sqlite v1.54.0) — 그래서 필요 없는 실행이 싸고, 조정 손잡이(환경 변수)를
// 지금 만들지 않는다.
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
git commit -m "feat(store): MergeFTS — FTS5 세그먼트를 병합한다 (D102 계약 1·3)"
```

---

### Task 2: 하루 한 번 게이트 — 스탬프 파일

**Files:**
- Modify: `internal/store/store.go` (Task 1의 `MergeFTS` 아래)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: `(*Store).MergeFTS`
- Produces: `const mergeStampName`, `func (s *Store) MergeFTSIfDue(ctx context.Context, interval time.Duration, now time.Time) (bool, error)`

**조건은 시간 하나다 — 삭제 건수도, "마지막 병합 이후 변경이 있었는가"도 조건에 넣지 않는다.** 세그먼트는 삽입으로도 쌓이므로 건수 문턱은 삽입만 있는 구간에서 병합을 영영 막고(D102 계약 2), 변경 유무 조건은 계약 3의 성질(이미 병합된 인덱스의 `optimize`는 일 없이 반환) 때문에 아낄 비용이 없다 — 조건 하나를 더 두는 값이 관측되지 않았다.

- [ ] **Step 1: 실패하는 테스트를 쓴다**

```go
// TestMergeFTSIfDueStamp: 스탬프가 없으면 돌고, 방금 돌았으면 안 돌고, interval이 지나면
// 다시 돈다. 조건이 **시간 하나**라는 것이 계약이다 — 삭제 건수는 조건에 들어가지 않는다
// (설계 v0.20 D102 계약 2: 세그먼트는 삽입으로도 쌓이므로 건수 문턱은 삽입만 있고 삭제가
// 적은 구간에서 병합을 영영 막는다).
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

// TestMergeFTSIfDueFutureStampIsDue: now보다 **미래**인 스탬프도 "돌 때가 됐다"로 읽는다.
// 시계 되돌림·복원·파일시스템 타임스탬프 이상으로 미래 mtime이 생기면 경과가 음수가 되는데,
// 그것을 "아직 이르다"로 읽으면 그 저장소는 **영구히** 병합하지 않는다(설계 v0.20 D102 계약 2).
func TestMergeFTSIfDueFutureStampIsDue(t *testing.T) {
	dir := t.TempDir()
	st := openAt(t, dir)
	now := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	stamp := filepath.Join(dir, mergeStampName)
	f, err := os.OpenFile(stamp, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("스탬프 생성: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("스탬프 Close: %v", err)
	}
	future := now.Add(72 * time.Hour)
	if err := os.Chtimes(stamp, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	ran, err := st.MergeFTSIfDue(t.Context(), 24*time.Hour, now)
	if err != nil || !ran {
		t.Fatalf("미래 mtime에서 안 돌았다(영구 정지): ran=%v err=%v", ran, err)
	}
	fi, err := os.Stat(stamp)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.ModTime().After(now) {
		t.Fatalf("병합 후에도 스탬프가 미래에 남았다: %v", fi.ModTime())
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
// **"돌 때가 됐다"로 읽는 것이 둘이다**: ① 스탬프를 못 읽는 어떤 사유(부재·권한·손상) —
// 병합은 멱등이고, 못 읽어서 영영 안 도는 쪽이 더 나쁜 실패다. ② mtime이 now보다 **미래**인
// 스탬프 — 시계 되돌림이나 복원 뒤에 음수 경과가 나오는데, 그것을 "아직 이르다"로 읽으면
// 그 저장소는 영구히 병합하지 않는다. 반대로 스탬프 **쓰기** 실패는 무시한다: 다음 기회에
// 한 번 더 도는 것이 전부다.
//
// 프로세스 둘이 동시에 기동하면 둘 다 스탬프를 낡은 것으로 보고 병합할 수 있다. 그래도
// 무해하다 — 쓰기 락이 둘을 직렬화하고, 뒤엣것은 이미 한 세그먼트가 된 인덱스에 optimize를
// 걸어 일 없이 반환한다(번들 SQLite 3.53.3 소스 대조, D102 계약 2·3).
func (s *Store) MergeFTSIfDue(ctx context.Context, interval time.Duration, now time.Time) (bool, error) {
	stamp := filepath.Join(s.dir, mergeStampName)
	if fi, err := os.Stat(stamp); err == nil {
		if elapsed := now.Sub(fi.ModTime()); elapsed >= 0 && elapsed < interval {
			return false, nil
		}
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
Expected: PASS 셋

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

### Task 3: 자동 경로 — 서버 생존 ctx에 묶인 주기 고루틴

**Files:**
- Modify: `cmd/context-router/main.go` (상수는 `startupPurgeMaxHashes`(523행) 근처, 배선은 기동 퍼지 고루틴 **밖**)
- Test: `cmd/context-router/main_test.go`

**Interfaces:**
- Consumes: `(*Store).MergeFTSIfDue`
- Produces: `const defaultFTSMergeInterval`, `const ftsMergeStartDelay`, `func runFTSMergeLoop(ctx context.Context, st *store.Store, delay, interval time.Duration)`

**★ 자동 경로는 기동 퍼지 고루틴에 얹지 않는다 — 자기 고루틴이다**(설계 v0.20 D102 계약 2). 퍼지 고루틴에 얹으면 안 되는 이유가 셋이고 전부 실측이다:

1. **기동 시 1회만 돈다.** 훅은 별개 단명 프로세스(`internal/hook/shadow.go:75`가 자기 스토어를 연다)라 서버가 사는 내내 삽입이 계속되는데, 오래 사는 서버는 시작 때 한 번 병합하고 그 뒤 영영 하지 않는다.
2. **`purgeErr`에 걸린다.** 정상 종료도 `purgeCtx` 취소로 non-nil을 만든다(`main.go:679-685`가 그 분기를 명시한다) — 짧은 세션이 이어지는 사용에서 병합이 한 번도 안 도는 구간이 생긴다. **그래서 병합은 `purgeErr`를 보지 않는다.**
3. **60초 예산을 공유한다.** `optimize`는 단일 암시 트랜잭션이라 중단되면 통째 롤백된다 — `startupPurgeBudget`(`main.go:494`)이 남지 않은 기동에서는 매번 0의 진전을 낸다. **그래서 `purgeCtx`를 쓰지 않는다.**

배선 자리도 계약이다: 퍼지 고루틴 안의 `cutoff <= 0` 조기 반환(`main.go:645-652`) **아래에 두지 않는다** — 보존 기간을 크게 잡은 설정에서 병합까지 함께 꺼진다.

- [ ] **Step 1: 실패하는 테스트를 쓴다**

`cmd/context-router/main_test.go`에 붙인다. **`strconv`를 import에 더한다**(현재 없음).

```go
// TestFTSMergeLoopMergesAndStamps: 병합 루프가 **실제로** 병합하고 스탬프를 남긴다.
// 상수만 재는 테스트는 MergeFTSIfDue 호출을 통째로 빼먹어도 통과한다 — 이 테스트가 그 구멍을
// 막는다. 판정은 둘이다: 스탬프 파일이 생겼는가(호출이 배선됐는가)와 tombstone이 실제로
// 걷혔는가(삭제만으로는 인덱스가 줄지 않는다).
func TestFTSMergeLoopMergesAndStamps(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir, false)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	body := strings.Repeat("alpha beta gamma delta ", 3000) // 약 70 KB/건
	for i := range 12 {
		s := body + strconv.Itoa(i)
		if _, err := st.Register(context.Background(), store.Registration{
			StoredBytes: []byte(s), MediaType: "text/plain",
			Source: store.SourceMeta{
				URI: "shadow:Bash:seg" + strconv.Itoa(i), Kind: "hook", SrcHash: "sh" + strconv.Itoa(i),
			},
			Chunks: []store.Chunk{{Ordinal: 0, ByteEnd: int64(len(s)), Text: s}},
		}); err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
	}
	// 전량 삭제 → FTS에는 tombstone만 쌓인다(병합 전에는 줄지 않는다).
	if _, _, err := st.PurgeOlderThan(context.Background(), time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatalf("PurgeOlderThan: %v", err)
	}
	before := ftsTrigramBytes(t, st)
	if before == 0 {
		t.Fatal("시드가 FTS 인덱스를 만들지 않았다 — 이 테스트가 공허 통과한다")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); runFTSMergeLoop(ctx, st, time.Millisecond, time.Hour) }()

	// store.mergeStampName은 비공개다 — 이름을 여기서 리터럴로 고정한다(바뀌면 이 테스트가 잡는다).
	stamp := filepath.Join(dir, "fts-merge.stamp")
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(stamp); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("루프가 스탬프를 남기지 않았다 — MergeFTSIfDue 호출이 배선되지 않았다")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done // 병합이 끝난 뒤에 읽는다(쓰기 중 측정 금지)

	if after := ftsTrigramBytes(t, st); after >= before {
		t.Fatalf("병합 후에도 인덱스가 줄지 않았다: %d → %d", before, after)
	}
}

// ftsTrigramBytes — fts_trigram_data의 block 바이트 합(세그먼트 실점유).
func ftsTrigramBytes(t *testing.T, st *store.Store) int64 {
	t.Helper()
	var n int64
	if err := st.Reader().QueryRow(
		`SELECT coalesce(sum(length(block)),0) FROM fts_trigram_data`).Scan(&n); err != nil {
		t.Fatalf("fts_trigram_data: %v", err)
	}
	return n
}

// TestFTSMergeIntervalIsDaily: 자동 경로가 쓰는 병합 주기가 하루다. 위 루프 테스트가 동작을
// 재고 이 상수 테스트는 **값**을 잰다 — 이 값이 D67의 락 보유 규율과 맞물려 있어서다:
// 정상상태 병합이 쓰기 락을 약 1.2초 잡는데 훅의 총예산이 2000 ms다(설계 v0.20 D102 계약 9).
// 매 기동으로 낮추는 변경은 이 테스트를 지나야 한다.
func TestFTSMergeIntervalIsDaily(t *testing.T) {
	if defaultFTSMergeInterval != 24*time.Hour {
		t.Fatalf("병합 주기 = %v, 기대 24h — 설계 v0.20 D102 계약 2를 먼저 고쳐라",
			defaultFTSMergeInterval)
	}
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./cmd/context-router -run 'TestFTSMerge' -count=1 -v`
Expected: 컴파일 실패 — `runFTSMergeLoop`·`defaultFTSMergeInterval` undefined

- [ ] **Step 3: 상수와 루프를 넣는다**

`main.go`의 `startupPurgeMaxHashes`(523행) 선언 아래에 둔다.

```go
// defaultFTSMergeInterval — D102 계약 2. 세그먼트 축적이 하루 약 36 MB이고 정상상태 병합이
// 약 1.2초 쓰기 락을 잡는다(설계 v0.20 관측 B). 그 보유가 훅 Read 가드의 2000 ms 총예산과
// 겹치면 그 훅의 포착이 버려지므로(계약 9 — 수용 위험, doctor [12]의 shadow-store drop으로
// 사후 관측된다) 하루 한 번으로 묶는다.
const defaultFTSMergeInterval = 24 * time.Hour

// ftsMergeStartDelay — 기동 첨두를 비켜나는 지연. 기동 직후에는 퍼지 배치가 쓰기 락을 잡고
// (startupPurgeMaxHashes 주석의 실측 보유) 세션 시작 훅이 몰린다 — 그 위에 병합을 얹지 않는다.
// **이 지연보다 짧은 세션은 병합하지 않는다**(의도된 성질): 스탬프는 벽시계라 다음에 이 지연을
// 넘긴 세션 하나가 밀린 몫을 한 번에 걷는다.
const ftsMergeStartDelay = 30 * time.Second

// runFTSMergeLoop — D102 계약 2의 자동 경로. ctx가 죽을 때까지 delay 뒤 한 번, 그 뒤 interval
// 마다 조건을 다시 본다. **기동 퍼지 고루틴에 얹지 않는 이유가 셋이다**(설계 v0.20 D102 계약 2):
// 그 고루틴은 기동당 1회만 돌아 오래 사는 서버에서 병합이 영영 안 오고, purgeErr는 정상 종료
// (purgeCtx 취소)에서도 non-nil이며, 60초 예산을 나눠 쓰면 optimize가 단일 암시 트랜잭션이라
// 중단 시 통째 롤백돼 매번 0의 진전을 낸다. 그래서 여기는 purgeErr를 보지 않고 purgeCtx도
// 쓰지 않는다. 실패는 로그만 남긴다 — 스탬프가 안 찍히므로 다음 주기가 그대로 재시도한다.
func runFTSMergeLoop(ctx context.Context, st *store.Store, delay, interval time.Duration) {
	t := time.NewTimer(delay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if ran, err := st.MergeFTSIfDue(ctx, interval, time.Now()); err != nil {
			slog.Warn("FTS 병합 실패 — 다음 주기에 재시도", "error", err)
		} else if ran {
			slog.Info("FTS 병합 완료")
		}
		t.Reset(interval)
	}
}
```

- [ ] **Step 4: 배선한다**

`run()`의 기동 퍼지 `defer func() { cancelPurge(); <-purgeDone }()`(711행) **다음**, `return mcp.Serve(...)`(712행) **앞**에 붙인다. **퍼지 고루틴 안(`cutoff <= 0` 조기 반환 아래)이 아니다.**

```go
	// D102 계약 2: 병합은 퍼지와 별개 고루틴이고 서버 생존 ctx에 묶인다 — 퍼지의 60초 예산도
	// purgeErr도 cutoff<=0 건너뛰기도 공유하지 않는다(그 셋 중 어느 것에 걸려도 병합이 영영
	// 안 도는 구간이 생긴다). defer 등록이 st.Close()(592행)보다 뒤라 LIFO로 먼저 돌아, 고루틴이
	// 닫힌 DB를 만지거나 프로세스보다 오래 사는 일이 없다.
	mergeCtx, cancelMerge := context.WithCancel(ctx)
	mergeDone := make(chan struct{})
	go func() {
		defer close(mergeDone)
		runFTSMergeLoop(mergeCtx, st, ftsMergeStartDelay, defaultFTSMergeInterval)
	}()
	defer func() { cancelMerge(); <-mergeDone }()
```

- [ ] **Step 5: 통과를 확인한다**

Run: `go test ./cmd/context-router -run 'TestFTSMerge' -count=1 -v`
Expected: PASS 둘

- [ ] **Step 6: 게이트 다섯**

```bash
go build ./... && go vet ./... && go test ./... -count=1 && gofumpt -l . && golangci-lint run
```

- [ ] **Step 7: 커밋**

```bash
git add cmd/context-router/main.go cmd/context-router/main_test.go
git commit -m "feat(server): 서버 생존 동안 도는 FTS 병합 고루틴 (D102 계약 2)"
```

---

### Task 4: 수동 회수 경로 둘이 VACUUM 앞에 병합한다

**Files:**
- Modify: `internal/cli/cli.go:927-937` (`runPurge`의 `--vacuum` 분기), `internal/cli/cli.go:952-955` (루프 뒤 집계 반환)
- Modify: `internal/cli/cli.go:1032-1048` (`runPurgeHookOnly`)
- Test: `internal/cli/cli_test.go`

**Interfaces:**
- Consumes: `(*Store).MergeFTS`
- Produces: 없음 (동작 변경)

**배경**: 지금 두 경로는 VACUUM만 한다. 실측으로 그것은 회수 가능분의 **29.6%만**(159,408,128 / 539,099,136) 되돌린다 — 나머지는 병합해야 free page가 된다. 자동 경로와 달리 여기는 **주기 게이트를 걸지 않는다**(사용자가 회수를 명시로 요청한 자리다). 그리고 **스탬프도 갱신하지 않는다** — 스탬프는 자동 경로 것이고, 수동 회수 뒤 기동이 한 번 더 병합해도 계약 3의 성질 때문에 싸다(설계 v0.20 D102 계약 4).

- [ ] **Step 1: 실패하는 테스트를 쓴다**

`internal/cli/cli_test.go`에 붙인다. **`strconv`를 import에 더한다**(현재 없음).

```go
// seedShadowChunkedProject — hook 귀속 아티팩트 12건을 **Chunks를 실어** 시드해 FTS 인덱스를
// 실제로 채우고, file(비귀속) 소스 하나를 남겨 --hook-only가 선택 삭제임을 유지한다.
// 공용 seedShadowContentDB(cli_test.go:466)를 쓰지 않는 이유: 그 헬퍼는 Chunks 없는
// Registration을 만들고 Register는 reg.Chunks에서만 chunks를 INSERT하므로(store.go:459)
// fts_trigram_data가 병합 전후 모두 0바이트다 — 그 헬퍼로 시드하면 이 테스트는 병합이 있든
// 없든 통과한다. 기존 헬퍼는 다른 테스트가 행수를 단정하므로 건드리지 않는다.
func seedShadowChunkedProject(t *testing.T) (pid, projDir string) {
	t.Helper()
	storeRoot, projectRoot := t.TempDir(), t.TempDir()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	pid = canon.ProjectID
	projDir = filepath.Join(storeRoot, "projects", pid)
	st, err := store.Open(projDir, false)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	body := strings.Repeat("segment merge probe ", 4000) // 약 80 KB/건
	for i := range 12 {
		s := body + strconv.Itoa(i)
		if _, err := st.Register(context.Background(), store.Registration{
			StoredBytes: []byte(s), MediaType: "text/plain",
			Source: store.SourceMeta{
				URI: "shadow:Bash:" + strconv.Itoa(i), Kind: "hook", SrcHash: "sh" + strconv.Itoa(i),
			},
			Chunks: []store.Chunk{{Ordinal: 0, ByteEnd: int64(len(s)), Text: s}},
		}); err != nil {
			t.Fatalf("register hook %d: %v", i, err)
		}
	}
	if _, err := st.Register(context.Background(), store.Registration{ // 비귀속 — 보존 대상
		StoredBytes: []byte("explicit-file-content"), MediaType: "text/plain",
		Source: store.SourceMeta{URI: "/tmp/f.txt", Kind: "file", SrcHash: "sh-file"},
		Chunks: []store.Chunk{{Ordinal: 0, Text: "explicit-file-content"}},
	}); err != nil {
		t.Fatalf("register file: %v", err)
	}
	// **여기서 반드시 닫는다** — runPurge는 writable Open(lockStore)을 하므로 열어 둔 채
	// 부르면 잠금 경합으로 실패한다.
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
	return pid, projDir
}

// ftsTrigramBytesRO — projDir/content.db를 read-only로 열어 fts_trigram_data의 block 바이트
// 합을 읽는다. 파일 크기가 아니라 이 값을 재는 이유: VACUUM은 라이브 프로세스 제약을 받아
// 테스트에서 불안정하지만 병합 여부는 결정적이다.
func ftsTrigramBytesRO(t *testing.T, projDir string) int64 {
	t.Helper()
	st, err := store.Open(projDir, true)
	if err != nil {
		t.Fatalf("store.Open ro: %v", err)
	}
	defer func() { _ = st.Close() }()
	var n int64
	if err := st.Reader().QueryRow(
		`SELECT coalesce(sum(length(block)),0) FROM fts_trigram_data`).Scan(&n); err != nil {
		t.Fatalf("fts_trigram_data: %v", err)
	}
	return n
}

// TestPurgeHookOnlyMergesBeforeVacuum: --hook-only가 VACUUM 전에 FTS를 병합한다.
// 병합 없이 VACUUM만 하면 tombstone은 free page가 아니라 live page라 회수되지 않는다 —
// 실측 기준으로 회수 가능분의 29.6%만 돌아온다(설계 v0.20 D102 계약 4).
// 진입점은 runPurge다: runPurgeHookOnly를 직접 부르는 테스트는 없고 --hook-only는 runPurge의
// 조기 분기(cli.go:793-801)가 인터셉트한다. --force가 없으면 비TTY에서 confirmPurge가 즉시
// 거부한다(cli.go:682-687).
func TestPurgeHookOnlyMergesBeforeVacuum(t *testing.T) {
	pid, projDir := seedShadowChunkedProject(t)
	storeRoot := storeRootOf(projDir)
	before := ftsTrigramBytesRO(t, projDir)
	if before < 200<<10 { // 시드 약 960 KB 텍스트 → trigram 인덱스는 그 몇 배다
		t.Fatalf("시드가 FTS를 충분히 채우지 않았다(%dB) — 이 테스트가 공허 통과한다", before)
	}

	var out bytes.Buffer
	args := []string{"--project", pid, "--hook-only", "--force"}
	if err := runPurge(context.Background(), failReader{}, &out, io.Discard, storeRoot, args, false); err != nil {
		t.Fatalf("runPurge err=%v out=%s", err, out.String())
	}

	after := ftsTrigramBytesRO(t, projDir)
	if after >= before {
		t.Fatalf("병합이 없다 — 삭제만으로는 세그먼트가 줄지 않는다: %d → %d", before, after)
	}
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/cli -run TestPurgeHookOnlyMergesBeforeVacuum -count=1 -v`
Expected: FAIL — 병합이 없어 `fts_trigram_data`가 줄지 않는다

- [ ] **Step 3: 두 자리에 병합을 넣고 종료 상태에 반영한다**

`runPurgeHookOnly`(1032-1048행)를 이렇게 고친다 — `vacErr` 선언 옆에 `mergeErr`를 두고, `vacuumReclaim`(1039행) **앞**에서 병합한다.

```go
	var mergeErr, vacErr error
	if purgeErr == nil {
		// ④ 실회수 보고 먼저(스펙 §3 순서) — VACUUM 성패와 무관하게 부분 성공을 즉시 노출한다.
		fmt.Fprintf(w, "hook-only purge: 실회수 %dB(%d hashes), 유예 %d건, 실패 %d건\n",
			rep.ReclaimedB, rep.Hashes, rep.DeferredFiles, rep.FailedFiles)
		// D102 계약 4: VACUUM은 free page만 되돌린다. 삭제가 남긴 FTS tombstone은 **free page가
		// 아니라 live page**라 병합해야 회수되고, 병합 없이 VACUUM만 하면 회수 가능분의 약 30%만
		// 돌아온다. 사용자가 회수를 명시로 요청한 자리이므로 주기 게이트를 걸지 않고, 스탬프도
		// 갱신하지 않는다(스탬프는 자동 경로 것이다). 실패해도 VACUUM은 진행한다 — 부분 회수가
		// 무회수보다 낫고 이미 커밋된 삭제는 유효하다 — 다만 **종료 상태에는 반영한다**:
		// 스크립트가 부른 실행이 free page 몫만 회수하고 성공으로 보이면 안 된다.
		if mergeErr = st.MergeFTS(ctx); mergeErr != nil {
			fmt.Fprintf(stderr, "ctr: FTS 병합 실패(회수량이 줄어든다): %v\n", mergeErr)
		}
		// ⑤ D55: vacuumReclaim 합류 — checkpoint busy 검증·총합 보고, 실패는 rc≠0(본경로 동일).
		vacErr = vacuumReclaim(ctx, st, projDir, beforeB, w)
	}
	closeErr := st.Close()
	if purgeErr != nil {
		return purgeErr
	}
	if vacErr != nil {
		return vacErr
	}
	if mergeErr != nil {
		// 원인 문면은 위에서 stderr로 이미 냈다 — 반환 오류에는 경로 없는 정적 메시지만 남긴다(§12 canary).
		return errors.New("purge: FTS 병합 실패 — 회수가 부분에 그쳤습니다")
	}
	return closeErr
```

`runPurge`의 `--vacuum` 분기(927-937행)에는 같은 블록을 `vacuumReclaim` 호출 앞에 넣고, `vacuumFailed`(849행) 옆에 `mergeFailed` 카운터를 둔다.

```go
		if purgeErr == nil && *vacuum && !vacuumDiskAbort {
			// D102 계약 4 — runPurgeHookOnly와 같은 이유·같은 순서(VACUUM 앞, 실패해도 진행,
			// 종료 상태에는 반영). 프로젝트별 보고 후 계속하고 루프 끝에서 집계한다(D50 관례).
			if merr := st.MergeFTS(ctx); merr != nil {
				fmt.Fprintf(stderr, "ctr: %s: FTS 병합 실패(회수량이 줄어든다): %v\n", id, merr)
				mergeFailed++
			}
			if verr := vacuumReclaim(ctx, st, projDir, beforeB, w); verr != nil {
```

루프 뒤 집계(952-955행)에 한 줄을 더한다 — VACUUM 실패가 더 큰 실패이므로 먼저 이긴다.

```go
	if vacuumFailed > 0 {
		return fmt.Errorf("purge: %d개 프로젝트 VACUUM/checkpoint 실패", vacuumFailed)
	}
	if mergeFailed > 0 {
		return fmt.Errorf("purge: %d개 프로젝트 FTS 병합 실패 — 회수가 부분에 그쳤습니다", mergeFailed)
	}
	return nil
```

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

### Task 5: `doctor [14]` — 판정 기준을 live 바이트로 옮긴다

**Files:**
- Modify: `internal/store/store.go` (`SizeStat` 952-959, `SizeStats`의 `st := SizeStat{...}`(1001행) 직후)
- Modify: `internal/cli/cli.go:50` (임계 상수), `internal/cli/cli.go:1878`(출력)·`1886-1887`(경고 조건·문면)
- Test: `internal/store/store_test.go`, `internal/cli/cli_test.go`

**Interfaces:**
- Consumes: 없음
- Produces: `SizeStat.FreeBytes int64`

**판정 기준을 옮기는 이유**(설계 v0.20 D102 계약 6): 자동 경로가 VACUUM을 하지 않으므로 **파일 크기는 고수위에 영원히 머문다** — 임계를 256 MiB로 올려도 상시 초과라 D67이 지적한 죽은 신호가 그대로 남는다. live 바이트(`FileBytes - FreeBytes`)는 다르다: 부푼 상태 550,481,920 B → 병합 뒤 170,790,912 B → 병합이 깨지면 다시 자란다. **`free=`는 병기하되 판정에 쓰지 않는다**(계약 7) — 병합되지 않은 세그먼트는 free page가 아니라 live page이고, freelist는 결함이 있을 때 오히려 낮게 읽힌다.

- [ ] **Step 1: 실패하는 테스트를 쓴다**

`internal/store/store_test.go`:

```go
// TestSizeStatsReportsFreeBytes: SizeStats가 회수 가능 바이트(free page)를 낸다.
// 이 값이 없으면 "파일이 크다"와 "파일에 쓰레기가 있다"를 doctor에서 가를 수 없고,
// 그 구분이 없어서 D67의 임계가 한 달 동안 죽은 신호였다(설계 v0.20 관측 B).
func TestSizeStatsReportsFreeBytes(t *testing.T) {
	dir := t.TempDir()
	st := openAt(t, dir)
	body := strings.Repeat("free page seed ", 8000)
	for i := range 10 {
		regChunked(t, st, body+strconv.Itoa(i), "shadow:Bash:free"+strconv.Itoa(i))
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
```

`internal/cli/cli_test.go`:

```go
// TestContentFileWarnDefaultIs256MiB: 임계가 256 MiB다. 정리 후 정상상태가 실측 171 MB이므로
// 옛 100 MiB는 정리 뒤에도 상시 초과라 신호로서 죽는다. 256 MiB는 그 1.5배이고,
// 창을 7일로 늘리는 결정(약 400 MB)이 이 신호를 켜도록 고른 값이다(설계 v0.20 D102 계약 6).
func TestContentFileWarnDefaultIs256MiB(t *testing.T) {
	if defaultContentFileWarnBytes != 256<<20 {
		t.Fatalf("임계 = %d, 기대 %d", defaultContentFileWarnBytes, 256<<20)
	}
}

// TestRunDoctor_ContentLiveWarnUsesLiveBytes: [14] 경고가 파일 크기가 아니라 live 바이트로
// 판정한다. free page가 임계를 넘는 몫을 차지하면 경고가 **꺼져야** 한다 — 그것이 "파일이
// 크다"와 "쓰레기가 쌓였다"를 가르는 지점이고, 파일 크기 판정에서는 구조적으로 불가능하다.
func TestRunDoctor_ContentLiveWarnUsesLiveBytes(t *testing.T) {
	isolateCodexHome(t)
	storeRoot, projectRoot := doctorSizeWarnSetup(t)
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	projDir := filepath.Join(storeRoot, "projects", canon.ProjectID)
	sz, err := store.SizeStats(projDir)
	if err != nil || sz == nil {
		t.Fatalf("SizeStats: sz=%v err=%v", sz, err)
	}
	// live 바로 위에 임계를 둔다 — file 기준이면 초과(발화), live 기준이면 미달(침묵).
	live := sz.FileBytes - sz.FreeBytes
	t.Setenv("CTR_CONTENT_FILE_WARN_BYTES", strconv.FormatInt(live, 10))
	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.0.1-dev"); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	if strings.Contains(buf.String(), "[14] warning: live ") {
		t.Fatalf("live 임계와 같은 값에서 경고가 발화(> 판정 위반):\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), " free=") {
		t.Fatalf("[14] 줄에 free= 병기 없음:\n%s", buf.String())
	}
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/store -run TestSizeStatsReportsFreeBytes -count=1 -v && go test ./internal/cli -run 'TestContentFileWarnDefaultIs256MiB|TestRunDoctor_ContentLiveWarn' -count=1 -v`
Expected: 컴파일 실패(`FreeBytes` 없음) · FAIL(임계 100 MiB, `free=` 없음)

- [ ] **Step 3: store 구현**

`SizeStat`에 필드를 더한다(959행 아래):

```go
	FreeBytes          int64            // free page × page_size — 회수 가능분(D102 계약 7, 표시 전용)
```

`SizeStats`의 `st := SizeStat{...}`(1001행) 직후에 붙인다:

```go
	// D102 계약 7: 회수 가능분을 낸다. free page는 삭제가 만들고 이후 기록이 재사용하므로,
	// 이 값이 크다는 것은 "파일이 크다"가 아니라 "지운 만큼이 아직 파일 안에 있다"는 뜻이다.
	// 판정(계약 6)은 이 값이 아니라 FileBytes-FreeBytes로 한다 — 병합되지 않은 세그먼트는
	// free page가 아니라 live page라 freelist는 결함이 있을 때 오히려 낮게 읽힌다.
	// 두 pragma 모두 best-effort — 실패하면 0으로 두고 진단 전체를 실패시키지 않는다.
	var pageSize, freeCount int64
	if err := db.QueryRow("PRAGMA page_size").Scan(&pageSize); err == nil {
		if err := db.QueryRow("PRAGMA freelist_count").Scan(&freeCount); err == nil {
			st.FreeBytes = pageSize * freeCount
		}
	}
```

- [ ] **Step 4: cli 구현 — 임계·출력·경고**

`internal/cli/cli.go:50`:

```go
const defaultContentFileWarnBytes = 256 << 20 // 256MiB — D102 계약 6(정리 후 정상상태 실측 171 MB의 약 1.5배)
```

`internal/cli/cli.go:1878` 출력 줄에 `free=`를 더한다(기존 테스트는 `blob=`까지를 접두로 단정하므로 끝에 붙이는 것이 안전하다):

```go
		fmt.Fprintf(w, "[14] content.db: sources=%d artifacts=%d blob=%dB file=%dB free=%dB\n",
			sz.Sources, sz.Artifacts, sz.BlobBytes, sz.FileBytes, sz.FreeBytes)
```

`internal/cli/cli.go:1886-1887`의 조건과 문면을 바꾼다:

```go
		// D102 계약 6·8 — content.db 라이브 축(청크 텍스트+FTS) 자문 경고. 판정은 파일 크기가
		// 아니라 live 바이트다: 자동 경로가 VACUUM을 하지 않으므로 파일은 고수위에 머물고,
		// 파일 기준 임계는 정리 뒤에도 상시 초과라 신호로서 죽는다(D67의 관측된 결함).
		// free는 병기만 한다 — 병합 안 된 세그먼트는 live page라 freelist는 결함이 있을 때
		// 오히려 낮게 읽힌다(계약 7). 문면의 "자동 VACUUM 없음"은 옛 "자동 삭제 없음"을 고친
		// 것이다: 바로 위에서 보고하는 artifacts 수는 D67 퍼지 때문에 사용자 조작 없이 줄어든다.
		if warn := contentFileWarnBytes(os.Getenv); sz.FileBytes-sz.FreeBytes > warn {
			fmt.Fprintf(w, "[14] warning: live %dB > 임계 %dB(CTR_CONTENT_FILE_WARN_BYTES) — 청크 텍스트+FTS 축(자문, live=file-free). free %dB는 이미 회수돼 재사용을 기다리는 몫이라 판정에 넣지 않는다. 파일 축소는 VACUUM(라이브 서버 제약 — 서버 비가동 시 purge --older-than --vacuum), --hook-only는 shadow 귀속 한정(explicit 소스 감축은 전체 purge). 훅 아티팩트 보존 창 기본 72시간(CTR_SHADOW_RETENTION). 자동 VACUUM 없음\n",
				sz.FileBytes-sz.FreeBytes, warn, sz.FreeBytes)
		}
```

**보존 창을 값이 아니라 "기본 72시간 + 키 이름"으로 적는 이유**: 실효값을 계산하는 `shadowRetention`은 `cmd/context-router`(main.go:466)에 있고 `internal/cli`는 그 패키지를 import할 수 없다. 규칙을 여기에 복제하면 두 구현이 갈라진다(D13) — 기본값과 키를 함께 적으면 D104의 측정 구간처럼 키가 덮인 상태에서도 문면이 거짓이 되지 않는다.

- [ ] **Step 5: 임계 변경이 깨는 기존 테스트를 함께 고친다**

임계 상수를 바꾸면 `TestContentFileWarnBytes`(`internal/cli/cli_test.go:453`·`459`)의 `100<<20` 리터럴 **둘**이 실패한다 — 같은 커밋에서 `256<<20`으로 고친다. 문면을 바꾸면 `"[14] warning: file "`을 찾는 자리 둘도 함께 고친다:

- `cli_test.go:453`·`459`: `100<<20` → `256<<20`
- `cli_test.go:424` (`TestRunDoctor_ContentFileWarn`, 발화 단정): `"[14] warning: file "` → `"[14] warning: live "`
- `cli_test.go:446` (`TestRunDoctor_ContentFileWarnAxisIndependent`, 침묵 단정): 같은 치환

`TestDoctorWarnMentionsHookOnly`(`cli_test.go:2465`)는 blob 축 경고를 재므로 영향 없다.

- [ ] **Step 6: 통과를 확인한다**

Run: `go test ./internal/store ./internal/cli -count=1`
Expected: PASS

- [ ] **Step 7: 실물 확인**

Run: `go run ./cmd/context-router doctor | grep '^\[14\]'`
Expected: `free=`가 보이고, 경고가 뜨면 `live ...B > 임계 268435456B`와 `자동 VACUUM 없음`·`보존 창 기본 72시간`을 인용한다.

- [ ] **Step 8: 게이트 다섯 + 커밋**

```bash
go build ./... && go vet ./... && go test ./... -count=1 && gofumpt -l . && golangci-lint run
git add internal/store internal/cli
git commit -m "feat(doctor): [14] 판정을 live 바이트로 옮기고 free=를 병기한다 (D102 계약 6·7·8)"
```

---

### Task 6: v0.19.1 배송

**Files:**
- Modify: `internal/buildinfo/buildinfo.go:11` (`var productVersion`)
- Modify: `CHANGELOG.md`

- [ ] **Step 1: 버전을 올린다**

`internal/buildinfo/buildinfo.go:11`의 `productVersion`을 `"0.19.1"`로 올린다(전 소비처의 유일 입구 — 배너·훅 Producer·marker·doctor `[17]`·MCP serverInfo가 여기서 읽는다). 다른 자리에 하드코딩이 남아 있지 않은지 확인한다:

```bash
grep -rn '0\.19\.0' --include=*.go . | grep -v _test
```

- [ ] **Step 2: CHANGELOG `Unreleased` 아래에 `[0.19.1]` 절을 만든다**

```markdown
## [0.19.1] — 2026-08-XX

### Fixed

- **FTS 세그먼트가 병합되지 않아 저장 파일의 76%가 회수되지 않고 쌓였다** (D102). 외부 콘텐츠
  FTS5의 삭제는 tombstone을 새 세그먼트에 쌓을 뿐이고, 이 저장소에는 병합을 부르는 코드가
  없었다 — D67의 72시간 퍼지가 지운 몫이 파일에서 돌아오지 않았다. 실측: 파일
  709,890,048 B 중 539,099,136 B가 회수 가능분이었고, `optimize` 뒤 170,790,912 B가 됐다.
  **서버가 사는 동안 도는 고루틴이 하루 한 번 병합**하고, 수동 회수 경로 둘은 VACUUM 앞에
  병합한다.
- **`purge --vacuum`·`--hook-only`가 회수 가능분의 약 30%만 되돌리던 것**을 고쳤다 — VACUUM은
  free page만 되돌리고 tombstone은 병합해야 free page가 된다. 병합 실패는 이제 **명령의 종료
  상태에 반영된다**(VACUUM은 그대로 진행한다 — 부분 회수가 무회수보다 낫다).

### Changed

- `doctor [14]`의 **판정 기준이 파일 크기에서 live 바이트(`file-free`)로 바뀐다**. 자동 경로가
  VACUUM을 하지 않아 파일은 고수위에 머물므로 파일 기준 임계는 정리 뒤에도 상시 초과였다 —
  한 달 동안 죽은 신호였던 이유다. `free=`를 병기하되 판정에는 쓰지 않는다.
- `CTR_CONTENT_FILE_WARN_BYTES` 기본값을 **100 MiB → 256 MiB**로 올린다. 정리 후 정상상태가
  실측 171 MB이고, 창을 7일로 늘리는 결정(약 400 MB)이 이 신호를 켜도록 고른 값이다.
- `doctor [14]` 경고 문면: *"자동 삭제 없음"* → *"자동 VACUUM 없음"*(바로 위에서 보고하는
  `artifacts=`는 D67 퍼지로 사용자 조작 없이 줄어든다 — 옛 문면은 평문으로 읽으면 어긋났다).
  같은 줄에 **훅 아티팩트 보존 창(기본 72시간, `CTR_SHADOW_RETENTION`)** 을 적는다.
```

- [ ] **Step 3: 게이트 다섯 + 실물 확인**

```bash
go build ./... && go vet ./... && go test ./... -count=1 && gofumpt -l . && golangci-lint run
go run ./cmd/context-router doctor | grep -E '^\[14\]|^\[17\]'
```

- [ ] **Step 4: 배포 뒤 관측 항목을 기록한다(코드 아님)**

설계 §4-1이 라이브 DB에서 재라고 남긴 셋을 배포 후 첫 병합에서 잰다 — **소요 시간 · 쓰기 바이트 · `content.db-wal`의 피크**. 세 번째가 필요한 이유: `doctor [14]`는 `content.db` 본체만 재므로 약 490 MB 인덱스를 한 트랜잭션에서 다시 쓰는 동안의 `-wal` 팽창이 그 눈금에 안 보인다(D102 계약 5). 함께 `doctor [12]`의 `shadow-store` drop 추이를 본다 — 계약 9가 병합↔훅 충돌을 사후 관측하는 자리다. 값은 다음 세션 인수인계 기록에 적는다.

- [ ] **Step 5: 커밋**

```bash
git add internal/buildinfo/buildinfo.go CHANGELOG.md
git commit -m "chore(release): 0.19.1 — FTS 세그먼트 병합 (D102)"
```

---

# 2단계 — v0.20 (D103)

### Task 7: `ledger` 열 둘 + 이관 상태 판정

**Files:**
- Modify: `internal/store/store.go:151-162` (ledger 초기화), `915-948` (`LedgerStats` 아래)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: 없음
- Produces: `type FetchStat`, `func LedgerFetchStats(dir string) (FetchStat, error)`

**행 셋을 값으로 가른다**(설계 v0.20 D103 계약 1). `ALTER`는 기존 행을 NULL로 남기고 이 원장에는 이미 `ctr_fetch` 49행이 있다 — `artifact_id IS NULL`을 그대로 "미해소"로 읽으면 배포 첫날 레거시만으로 D104의 "미해소 5건 이상"이 발화한다.

| 상태 | `artifact_id` | `artifact_age_s` |
|---|---|---|
| 레거시(이관 전 행) | NULL | **NULL** |
| 미해소 | NULL | **−1** |
| 해소 | 값 | 값 |

- [ ] **Step 1: 실패하는 테스트를 쓴다**

```go
// TestLedgerFetchStatsToleratesOldSchema: 새 열이 없는 옛 ledger.db를 만나도 실패하지 않는다.
// ALTER는 writable Open에서만 도는데 LedgerFetchStats는 ledger.db를 따로 read-only로 연다 —
// 새 바이너리의 stats가 아직 이관되지 않은 원장을 먼저 만날 수 있다(설계 v0.20 D103 계약 7).
// 총 호출(Calls)은 옛 원장에서도 읽힌다 — 그 열은 처음부터 있었다.
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
	if _, err := db.Exec(
		`INSERT INTO ledger(ts,tool,bytes_stored,bytes_returned,duration_ms) VALUES(1,'ctr_fetch',0,10,1)`); err != nil {
		t.Fatalf("레거시 행: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fs, err := LedgerFetchStats(dir)
	if err != nil {
		t.Fatalf("옛 스키마에서 오류가 났다: %v", err)
	}
	if fs.Calls != 1 {
		t.Fatalf("총 호출=%d, 기대 1(레거시 행도 센다)", fs.Calls)
	}
	if fs.Resolved != 0 || fs.Missed != 0 {
		t.Fatalf("레거시 행이 해소/미해소로 셌다: %+v", fs)
	}
}

// TestLedgerFetchStatsPartialMigration: 두 ALTER는 독립이라 **하나만 성공한 상태가 도달
// 가능하다**. 그 상태에서 분위수 질의를 돌리면 경성 오류가 나므로, 열 **둘 다** 있을 때만 새
// 질의를 돌리고 아니면 이관 전과 같은 빈 값 경로를 탄다(설계 v0.20 D103 계약 7).
func TestLedgerFetchStatsPartialMigration(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "ledger.db")))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE ledger(
		id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, tool TEXT NOT NULL,
		bytes_stored INTEGER NOT NULL DEFAULT 0, bytes_returned INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0, artifact_id INTEGER)`); err != nil { // age 열만 없다
		t.Fatalf("부분 이관 스키마: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	fs, err := LedgerFetchStats(dir)
	if err != nil {
		t.Fatalf("부분 이관에서 오류가 났다: %v", err)
	}
	if fs.Resolved != 0 || fs.Missed != 0 {
		t.Fatalf("부분 이관에서 0이 아닌 값: %+v", fs)
	}
}

// TestLedgerColumnsAddedOnWritableOpen: writable Open이 두 열을 **실제로** 붙인다.
// 판정을 PRAGMA table_info로 하는 이유: LedgerFetchStats가 열 부재를 관용하므로, 반환값만
// 보면 ALTER를 아예 넣지 않은 구현도 0을 내며 통과한다. 두 번 열어도 "duplicate column name"이
// 밖으로 새지 않는다(ledger 전체가 best-effort라 삼킨다).
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

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "ledger.db"))+"?mode=ro")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	cols, err := ledgerColumns(db)
	if err != nil {
		t.Fatalf("ledgerColumns: %v", err)
	}
	if !cols["artifact_id"] || !cols["artifact_age_s"] {
		t.Fatalf("writable Open이 두 열을 붙이지 않았다: %v", cols)
	}
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/store -run 'TestLedger' -count=1 -v`
Expected: 컴파일 실패 — `LedgerFetchStats`·`ledgerColumns` undefined

- [ ] **Step 3: 구현**

`store.go`의 ledger `CREATE TABLE`(156-159행) 직후:

```go
			// D103 계약 1: artifact 단위 회수 실적 두 열. 옛 ledger.db에도 붙여야 하므로 ALTER를
			// 쓰는데, 이미 있으면 "duplicate column name"으로 실패한다 — ledger 전체가
			// best-effort이므로 그 실패를 그대로 무시한다(위 CREATE와 같은 `_, _ =` 관례).
			// 둘은 독립 문장이라 하나만 성공한 부분 이관이 도달 가능하다 — 읽는 쪽이
			// PRAGMA table_info로 **둘 다** 있는지 보고 아니면 빈 값 경로를 탄다(계약 7).
			_, _ = l.Exec(`ALTER TABLE ledger ADD COLUMN artifact_id INTEGER`)
			_, _ = l.Exec(`ALTER TABLE ledger ADD COLUMN artifact_age_s INTEGER`)
```

`LedgerStats`(948행) 아래:

```go
// FetchStat: D103 회수 실적. Calls는 원장의 ctr_fetch 행 전부(레거시 포함)이고 D104의 채택
// 문턱이 읽는 수다. Resolved는 artifact를 실제로 돌려준 fetch, Missed는 **artifact 부재**로
// 끝난 fetch다(계약 3 — 잘못된 chunk id는 여기 들지 않는다). Age*는 **회수 시점에 박아 둔**
// 나이(초)의 분포 — 아티팩트가 나중에 지워져도 남는다는 것이 이 계측의 요지다(계약 2).
type FetchStat struct {
	Calls                  int64
	Resolved, Missed       int64
	AgeP50, AgeP90, AgeMax int64
}

// ledgerColumns: PRAGMA table_info(ledger)의 열 이름 집합. 테이블이 없으면 빈 집합(오류 아님).
// 드라이버 오류 문자열 대조("no such column")를 쓰지 않는 이유: 그 문면은 우리가 통제하지
// 않는 계약이고 드라이버 판마다 바뀐다(설계 v0.20 D103 계약 7).
func ledgerColumns(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query(`PRAGMA table_info(ledger)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// LedgerFetchStats: dir/ledger.db를 read-only로 열어 회수 실적을 낸다.
// 파일 미존재는 LedgerStats와 동일하게 빈 값+nil. **새 열이 없는(또는 하나만 있는) 원장도
// 해소/미해소 0 + nil이다** — ALTER는 writable Open에서만 돌므로 이 경로가 이관 전 원장을
// 먼저 만날 수 있고, 그것은 손상이 아니다(설계 v0.20 D103 계약 7). 그 외 오류는 삼키지 않는다.
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

	cols, err := ledgerColumns(db)
	if err != nil {
		return FetchStat{}, fmt.Errorf("store LedgerFetchStats: %w", err)
	}
	if len(cols) == 0 { // ledger 테이블 자체가 없다 — 이관 전과 같은 빈 값 경로
		return fs, nil
	}
	if err := db.QueryRow(
		`SELECT count(*) FROM ledger WHERE tool='ctr_fetch'`).Scan(&fs.Calls); err != nil {
		return FetchStat{}, fmt.Errorf("store LedgerFetchStats: %w", err)
	}
	if !cols["artifact_id"] || !cols["artifact_age_s"] {
		return fs, nil // 이관 전·부분 이관 — 총 호출만 유효하다
	}
	// 계약 1의 표: 레거시는 두 열 다 NULL, 미해소는 age=-1, 해소는 둘 다 값.
	if err := db.QueryRow(`SELECT
			coalesce(sum(artifact_id IS NOT NULL),0),
			coalesce(sum(artifact_id IS NULL AND artifact_age_s IS NOT NULL),0)
		FROM ledger WHERE tool='ctr_fetch'`).Scan(&fs.Resolved, &fs.Missed); err != nil {
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

- [ ] **Step 4: 통과를 확인한다**

Run: `go test ./internal/store -run 'TestLedger' -count=1 -v`
Expected: PASS 셋

- [ ] **Step 5: 게이트 다섯 + 커밋**

```bash
go build ./... && go vet ./... && go test ./... -count=1 && gofumpt -l . && golangci-lint run
git add internal/store
git commit -m "feat(store): ledger 회수 실적 두 열 + 이관 상태 판정 (D103 계약 1·7)"
```

---

### Task 8a: store 표면 — 나이의 시계와 fetch 전용 기록

**Files:**
- Modify: `internal/store/store.go` (`LedgerAppend`(893-899) 아래)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: Task 7의 두 열
- Produces:
  - `func (s *Store) LastIndexedAtByHash(ctx context.Context, contentHash string) (int64, error)`
  - `func (s *Store) LedgerAppendFetch(returned, ms, artifactID, ageS int64)`

**★ 나이의 시계와 범위가 이 태스크의 핵심 판단**(설계 v0.20 D103 계약 2). 시계는 `artifacts.created_at`이 아니라 **`max(sources.indexed_at)`** 이고, 범위는 artifact가 아니라 **`content_hash`** 다. 둘 다 퍼지와 같아야 한다:

- 시계 — 내용 주소 저장이라 같은 바이트를 다시 포착하면 `created_at`은 첫 포착에 고정된 채 `indexed_at`만 움직인다. 퍼지는 마지막 포착으로 고른다(`shadowOwnedFilter`의 나이 절, `store.go:1099-1102`).
- 범위 — 퍼지 술어는 **같은 `content_hash`를 가진 모든 artifact의 모든 소스**에 걸린다. 같은 바이트가 media_type마다 별개 artifact 행을 갖기 때문이다(`store.go:1089-1091`이 그 이유를 적는다). **artifact 단위로 재면 형제가 방금 재포착된 아티팩트가 실제보다 늙어 보이고, 그 오차는 창을 늘리는 쪽으로만 작용한다.**

- [ ] **Step 1: 실패하는 테스트를 쓴다**

```go
// TestLastIndexedAtByHashUsesMaxOverSiblingArtifacts: 나이 시계가 마지막 포착이고 범위가
// content_hash다. 두 축을 한 번에 잰다 — 같은 바이트를 media_type 둘로 등록하면 artifact 행이
// 둘 생기는데(store.go:452의 조회 키가 (content_hash, media_type)), 퍼지 술어는 그 둘의 소스를
// 전부 본다(shadowOwnedFilter). **artifact 단위로 재는 구현은 이 테스트에서 떨어진다** —
// 형제 쪽이 최근 값을 쥐고 있기 때문이다.
func TestLastIndexedAtByHashUsesMaxOverSiblingArtifacts(t *testing.T) {
	st := openAt(t, t.TempDir())
	regSource(t, st, "aged", "text/plain", "shadow:Bash:first", "hook")
	regSource(t, st, "aged", "application/json", "shadow:Bash:second", "hook") // 같은 바이트, 다른 media_type

	old, recent := int64(1_000), int64(9_000)
	if _, err := st.writer.Exec(
		`UPDATE sources SET indexed_at=? WHERE uri='shadow:Bash:first'`, old); err != nil {
		t.Fatalf("첫 소스 시각: %v", err)
	}
	if _, err := st.writer.Exec(
		`UPDATE sources SET indexed_at=? WHERE uri='shadow:Bash:second'`, recent); err != nil {
		t.Fatalf("둘째 소스 시각: %v", err)
	}
	// 형제 둘 중 **오래된 쪽**의 artifact를 회수했다고 가정해도 나이는 최근 값이어야 한다.
	got, err := st.LastIndexedAtByHash(t.Context(), hashOf("aged"))
	if err != nil {
		t.Fatalf("LastIndexedAtByHash: %v", err)
	}
	if got != recent {
		t.Fatalf("LastIndexedAtByHash=%d, 기대 %d(형제 포함 최댓값)", got, recent)
	}
}

// TestLastIndexedAtByHashMissingIsZero: 소스가 없으면 (0, nil)이다 — 호출부가 나이를 0으로 두고
// 계속한다(회수 자체는 성공했다).
func TestLastIndexedAtByHashMissingIsZero(t *testing.T) {
	st := openAt(t, t.TempDir())
	got, err := st.LastIndexedAtByHash(t.Context(), hashOf("nothing here"))
	if err != nil || got != 0 {
		t.Fatalf("got=%d err=%v, 기대 0/nil", got, err)
	}
}

// TestLedgerAppendFetchMissMarksMinusOne: 미해소 행은 artifact_id NULL + artifact_age_s **−1**
// 이다. NULL로 적으면 ALTER가 남긴 레거시 행과 구분되지 않아, 배포 첫날 레거시 49건만으로
// D104의 "미해소 5건 이상"이 발화한다(설계 v0.20 D103 계약 1).
func TestLedgerAppendFetchMissMarksMinusOne(t *testing.T) {
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
	if fs.Calls != 2 || fs.Resolved != 1 || fs.Missed != 1 {
		t.Fatalf("calls=%d resolved=%d missed=%d, 기대 2/1/1", fs.Calls, fs.Resolved, fs.Missed)
	}
	if fs.AgeMax != 3600 {
		t.Fatalf("AgeMax=%d, 기대 3600(미해소의 −1이 분포에 섞이면 안 된다)", fs.AgeMax)
	}
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/store -run 'TestLastIndexedAt|TestLedgerAppendFetch' -count=1 -v`
Expected: 컴파일 실패

- [ ] **Step 3: 구현**

```go
// LastIndexedAtByHash — D103 계약 2: 이 콘텐츠의 **마지막 포착** 시각(unix 초). 시계도 범위도
// D67 퍼지와 같은 것을 쓴다 — 퍼지 술어(shadowOwnedFilter의 나이 절, store.go:1099-1102)가
// 같은 content_hash를 가진 모든 artifact의 모든 소스에 대해 indexed_at을 보므로, 나이도 그렇게
// 재야 분포가 보존 창 위에 그대로 겹쳐진다. artifact 단위로 재면 형제가 방금 재포착된
// 아티팩트가 실제보다 늙어 보이고 그 오차는 창을 늘리는 쪽으로만 작용한다. 소스가 없으면 (0, nil).
// 색인: artifacts는 UNIQUE(content_hash, media_type)의 선두 컬럼이, sources는
// idx_sources_artifact_indexed(artifact_id, indexed_at)가 각각 덮는다.
func (s *Store) LastIndexedAtByHash(ctx context.Context, contentHash string) (int64, error) {
	var ts sql.NullInt64
	if err := s.reader.QueryRowContext(ctx, `SELECT max(s.indexed_at)
		FROM sources s JOIN artifacts a ON a.id = s.artifact_id
		WHERE a.content_hash = ?`, contentHash).Scan(&ts); err != nil {
		return 0, fmt.Errorf("store LastIndexedAtByHash: %w", err)
	}
	if !ts.Valid {
		return 0, nil
	}
	return ts.Int64, nil
}

// LedgerAppendFetch — D103: ctr_fetch 전용 원장 기록. artifactID<=0이면 artifact_id를 NULL로,
// artifact_age_s를 **−1**로 남겨 **해소되지 않은 회수**를 기록한다 — 그것이 "창이 짧아서 못
// 찾았다"의 유일한 직접 증거이고, 성공 기록만 남기면 영원히 나오지 않는다(계약 3).
// −1인 이유: ALTER가 남긴 레거시 행은 두 열이 다 NULL이라 그 둘을 값으로 갈라야 한다(계약 1).
// LedgerAppend와 같은 best-effort 계약이다(ledger 없음·오류는 무시). S4: 정수와 도구 이름만
// 담는다 — 선택자도 경로도 내용도 담지 않는다(계약 6).
func (s *Store) LedgerAppendFetch(returned, ms, artifactID, ageS int64) {
	if s.ledger == nil {
		return
	}
	var idCol any            // nil = NULL
	age := int64(-1)         // 미해소 표식
	if artifactID > 0 {
		idCol, age = artifactID, ageS
	}
	_, _ = s.ledger.Exec(
		`INSERT INTO ledger(ts,tool,bytes_stored,bytes_returned,duration_ms,artifact_id,artifact_age_s)
		 VALUES(?,'ctr_fetch',0,?,?,?,?)`,
		time.Now().Unix(), returned, ms, idCol, age)
}
```

- [ ] **Step 4: 통과 확인 + 게이트 다섯 + 커밋**

```bash
go test ./internal/store -run 'TestLastIndexedAt|TestLedgerAppendFetch' -count=1 -v
go build ./... && go vet ./... && go test ./... -count=1 && gofumpt -l . && golangci-lint run
git add internal/store
git commit -m "feat(store): 회수 나이 시계(hash 범위)와 fetch 전용 원장 기록 (D103 계약 1·2)"
```

---

### Task 8b: `ctr_fetch`가 해소와 **artifact 부재**를 남긴다

**Files:**
- Modify: `internal/mcp/mcp.go:538-575` (fetch 핸들러)
- Test: `internal/mcp/mcp_test.go`

**Interfaces:**
- Consumes: Task 8a의 두 메서드
- Produces: 없음 (동작 변경)

**★ `ErrNotFound` 둘 중 하나만 센다**(설계 v0.20 D103 계약 3). `ctr_fetch`의 이른 반환은 다섯이고 다섯이 같은 것을 뜻하지 않는다:

| 자리 | 뜻 | 세나 |
|---|---|---|
| `selectorFromInput` 실패 (`mcp.go:540-543`) | 잘못된 입력 | 아니다 |
| `ReadRange`의 `ErrInvalidSelector` (`store.go:688`) | 선택자 종류 불명 | 아니다 |
| **`ReadRange`의 `ErrNotFound`** (`store.go:695`) | **artifact가 없다** | **★ 이것만** |
| `readChunk`의 `ErrNotFound` (`store.go:580`) | **chunk id가 없다 — 입력 문제다** | 아니다 |
| 그 밖의 `ReadRange` 오류 (`store.go:698`) | DB 오류 | 아니다 |

**`errors.Is(err, store.ErrNotFound)` 하나로는 셋째와 넷째가 안 갈린다 — 그 둘이 같은 센티넬을 쓴다.** artifact 행의 존재를 따로 확인해 가른다: 기존 `ArtifactHashByID`(`store.go:670`)가 정확히 그 조회이고 artifact 부재에만 `ErrNotFound`를 낸다 — 새 표면을 만들지 않는다. **"실패를 센다"를 넓게 읽으면 잘못된 chunk id가 창을 늘리는 근거로 둔갑한다.**

- [ ] **Step 1: 실패하는 테스트를 쓴다**

`internal/mcp/mcp_test.go`에 붙인다. 서버 헬퍼는 **`newRecordEventTestServer`**(`mcp_test.go:1704`)를 쓴다 — 기본 표면에 `ctr_fetch`가 들어 있고 **`storeDir`을 반환하는 유일한 계열**이다. `newTestServer`(`:140`)는 스토어를 반환값 없는 `t.TempDir()`에 열어 `LedgerFetchStats`에 넘길 경로가 없다.

```go
// TestFetchRecordsMissOnlyOnAbsentArtifact: 없는 artifact를 요청하면 미해소 행이 남고,
// 잘못된 chunk id·잘못된 선택자는 남지 않는다. 앞의 둘은 store.ErrNotFound 하나를 공유하므로
// (store.go:695·580) errors.Is로는 안 갈린다 — 이 테스트가 그 구분을 고정한다(D103 계약 3).
func TestFetchRecordsMissOnlyOnAbsentArtifact(t *testing.T) {
	cs, st, _, storeDir := newRecordEventTestServer(t)
	ctx := context.Background()

	body := "haystack needle haystack"
	artID, err := st.Register(ctx, store.Registration{
		StoredBytes: []byte(body), MediaType: "text/plain",
		Source: store.SourceMeta{URI: "shadow:Bash:hit", Kind: "hook", SrcHash: "sh-hit"},
		Chunks: []store.Chunk{{Ordinal: 0, ByteEnd: int64(len(body)), Text: body}},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// ① 없는 artifact → 미해소 1행
	callFetch(t, cs, FetchInput{ArtifactID: artID + 9999, ChunkID: ptrTo(int64(1))})
	if fs := fetchStats(t, storeDir); fs.Missed != 1 || fs.Resolved != 0 {
		t.Fatalf("artifact 부재 뒤 resolved=%d missed=%d, 기대 0/1", fs.Resolved, fs.Missed)
	}
	// ② 있는 artifact + 없는 chunk id → 입력 문제다, 미해소가 늘면 안 된다
	callFetch(t, cs, FetchInput{ArtifactID: artID, ChunkID: ptrTo(int64(999_999))})
	if fs := fetchStats(t, storeDir); fs.Missed != 1 {
		t.Fatalf("잘못된 chunk id가 미해소로 셌다: missed=%d, 기대 1", fs.Missed)
	}
	// ③ 선택자 없음(ErrInvalidSelector) → 역시 늘지 않는다
	callFetch(t, cs, FetchInput{ArtifactID: artID})
	if fs := fetchStats(t, storeDir); fs.Missed != 1 {
		t.Fatalf("잘못된 선택자가 미해소로 셌다: missed=%d, 기대 1", fs.Missed)
	}
}

// TestFetchRecordsAgeOnResolve: 해소되면 나이가 박힌다. 시계는 max(sources.indexed_at)이므로
// 소스 시각을 과거로 옮기면 그만큼 나이가 나온다(D103 계약 2).
func TestFetchRecordsAgeOnResolve(t *testing.T) {
	cs, st, _, storeDir := newRecordEventTestServer(t)
	ctx := context.Background()
	body := "resolved body"
	artID, err := st.Register(ctx, store.Registration{
		StoredBytes: []byte(body), MediaType: "text/plain",
		Source: store.SourceMeta{URI: "shadow:Bash:age", Kind: "hook", SrcHash: "sh-age"},
		Chunks: []store.Chunk{{Ordinal: 0, ByteEnd: int64(len(body)), Text: body}},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := st.Reader().Exec(
		`UPDATE sources SET indexed_at=? WHERE uri='shadow:Bash:age'`,
		time.Now().Add(-2*time.Hour).Unix()); err != nil {
		t.Fatalf("소스 시각: %v", err)
	}

	callFetch(t, cs, FetchInput{ArtifactID: artID, ByteStart: ptrTo(int64(0)), ByteEnd: ptrTo(int64(5))})
	fs := fetchStats(t, storeDir)
	if fs.Resolved != 1 || fs.Missed != 0 {
		t.Fatalf("resolved=%d missed=%d, 기대 1/0", fs.Resolved, fs.Missed)
	}
	if fs.AgeMax < 7000 || fs.AgeMax > 7400 { // 2시간 = 7200초, 실행 시각 오차 허용
		t.Fatalf("AgeMax=%d, 기대 약 7200", fs.AgeMax)
	}
}

// callFetch: ctr_fetch를 한 번 부른다. 오류 응답도 정상 반환이다(IsError로 온다) — 이 테스트가
// 재는 것은 응답이 아니라 원장이다.
func callFetch(t *testing.T, cs *mcp.ClientSession, in FetchInput) {
	t.Helper()
	if _, err := cs.CallTool(context.Background(),
		&mcp.CallToolParams{Name: "ctr_fetch", Arguments: in}); err != nil {
		t.Fatalf("call ctr_fetch: %v", err)
	}
}

// fetchStats: storeDir의 원장에서 회수 실적을 읽는다(라이브 writer와 동시 열기 —
// TestRecordEventLedgerAppend가 LedgerStats로 하는 것과 같은 형태다).
func fetchStats(t *testing.T, storeDir string) store.FetchStat {
	t.Helper()
	fs, err := store.LedgerFetchStats(storeDir)
	if err != nil {
		t.Fatalf("LedgerFetchStats: %v", err)
	}
	return fs
}

func ptrTo[T any](v T) *T { return &v }
```

`ptrTo`와 동형의 헬퍼가 `mcp_test.go`에 이미 있으면 그것을 쓰고 새로 만들지 않는다.

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/mcp -run 'TestFetchRecords' -count=1 -v`
Expected: FAIL — 미해소 행이 0이고 나이가 안 박힌다

- [ ] **Step 3: 핸들러를 고친다**

`ReadRange` 실패 분기(545-547행):

```go
		res, err := st.ReadRange(ctx, in.ArtifactID, sel)
		if err != nil {
			// D103 계약 3: ErrNotFound **둘 중 artifact 부재만** 미해소로 센다. ReadRange의
			// artifact 부재(store.go:695)와 readChunk의 chunk id 부재(store.go:580)가 같은
			// 센티넬을 쓰므로 errors.Is 하나로는 안 갈린다 — artifact 행의 존재를 따로 확인해
			// 가른다(ArtifactHashByID는 artifact 부재에만 ErrNotFound를 낸다, store.go:670).
			// 잘못된 선택자·DB 오류는 창의 길이에 대해 아무 말도 하지 않으며, 넓게 세면 잘못된
			// chunk id가 창을 늘리는 근거로 둔갑한다.
			if errors.Is(err, store.ErrNotFound) {
				if _, hashErr := st.ArtifactHashByID(ctx, in.ArtifactID); errors.Is(hashErr, store.ErrNotFound) {
					st.LedgerAppendFetch(0, time.Since(start).Milliseconds(), 0, 0)
				}
			}
			return nil, FetchOutput{}, toToolError(err)
		}
```

성공 분기(573행)의 `LedgerAppend`를 갈아끼운다:

```go
		// D103 계약 2: 나이는 **회수 시점에 계산해 박는다** — 사후 계산은 아티팩트가 지워지면
		// 불가능하다. 시계와 범위는 D67 퍼지와 같다(hash 단위 max(sources.indexed_at)).
		// 나이 조회 실패는 회수를 실패시키지 않는다 — 0으로 두고 행은 남긴다.
		var ageS int64
		if at, ageErr := st.LastIndexedAtByHash(ctx, res.Artifact.ContentHash); ageErr == nil && at > 0 {
			ageS = time.Now().Unix() - at
		}
		st.LedgerAppendFetch(jsonLen(out), time.Since(start).Milliseconds(), res.Artifact.ID, ageS)
```

`errors`·`store`·`time` import를 확인한다(셋 다 이미 있다).

- [ ] **Step 4: 통과 확인 + 게이트 다섯 + 커밋**

```bash
go test ./internal/mcp -count=1
go build ./... && go vet ./... && go test ./... -count=1 && gofumpt -l . && golangci-lint run
git add internal/mcp
git commit -m "feat(fetch): 해소와 artifact 부재를 원장에 남긴다 (D103 계약 2·3)"
```

---

### Task 9: 훅 포착이 분모를 남긴다 — 훅의 ctx를 타고

**Files:**
- Modify: `internal/store/store.go` (`LedgerAppend`(893-899) 자리)
- Modify: `internal/hook/shadow.go:33-99` (`shadowCapture`)
- Test: `internal/store/store_test.go`, `internal/hook/hook_test.go`

**Interfaces:**
- Consumes: 없음
- Produces: `func (s *Store) LedgerAppendContext(ctx context.Context, tool string, stored, returned, ms int64)`

**분모의 정의**: 원장의 `tool='hook:shadow'` 행 수 = **성공한 포착 건수**다. 내용 주소 저장이라 같은 바이트의 재포착은 아티팩트를 새로 만들지 않으므로 **이 수는 만들어진 고유 아티팩트 수의 상한**이다. 이름이 `ctr_` 로 시작하지 않는 것도 계약이다 — D104의 채택 문턱은 `ctr_*` 행만 세는데 하루 약 295행이면 훅 행이 그 총계를 지배한다(설계 v0.20 D103 계약 4).

**★ 훅의 원장 쓰기는 훅의 ctx를 탄다**(계약 8). 지금 `LedgerAppend`는 `s.ledger.Exec`로 ctx 없이 쓰는데 `ledger.db`의 `busy_timeout`은 5000 ms이고 훅의 총예산은 2000 ms다(`internal/hook/hook.go:60`의 `defaultDeadlineMS`, `:112`에서 ctx에 걸린다) — 훅 프로세스가 여럿 겹치면 그 INSERT가 예산 밖에서 블록될 수 있다.

- [ ] **Step 1: 실패하는 테스트를 쓴다 (store — 경합 측정)**

`internal/store/store_test.go`:

```go
// TestLedgerAppendContextUnderContention: 겹친 쓰기에서 원장 INSERT가 훅 예산 안에 든다.
// **단일 무경합 실행은 이 경로를 구조적으로 못 본다** — busy_timeout(5000ms)은 다른 연결이
// 락을 쥐고 있을 때만 개입하고, 그 값이 훅의 총예산 2000ms보다 크다는 것이 계약 8의 위험이다.
// 스토어를 넷 열어(각자 자기 ledger 연결) 동시에 쓴다: 같은 프로세스지만 연결이 별개라 파일
// 락 계층은 훅 프로세스 여럿과 같다.
func TestLedgerAppendContextUnderContention(t *testing.T) {
	dir := t.TempDir()
	const writers, perWriter = 4, 25
	stores := make([]*Store, writers)
	for i := range stores {
		stores[i] = openAt(t, dir)
	}

	var worst atomic.Int64
	var wg sync.WaitGroup
	for _, st := range stores {
		wg.Add(1)
		go func(st *Store) {
			defer wg.Done()
			for range perWriter {
				ctx, cancel := context.WithTimeout(t.Context(), 2000*time.Millisecond) // 훅 총예산
				begin := time.Now()
				st.LedgerAppendContext(ctx, "hook:shadow", 16384, 0, 1)
				cancel()
				if ms := time.Since(begin).Milliseconds(); ms > worst.Load() {
					worst.Store(ms)
				}
			}
		}(st)
	}
	wg.Wait()
	t.Logf("경합 INSERT 최악 소요 = %dms (훅 총예산 2000ms)", worst.Load())

	rows, err := LedgerStats(dir)
	if err != nil {
		t.Fatalf("LedgerStats: %v", err)
	}
	var got int64
	for _, r := range rows {
		if r.Tool == "hook:shadow" {
			got = r.Calls
		}
	}
	if want := int64(writers * perWriter); got != want {
		t.Fatalf("hook:shadow 행=%d, 기대 %d — 예산 안에서 못 쓴 INSERT가 있다", got, want)
	}
	if worst.Load() >= 2000 {
		t.Fatalf("경합 INSERT가 훅 예산을 넘겼다: %dms — 설계 §4-4의 분모 재배치 판단으로 간다", worst.Load())
	}
}
```

`internal/hook/hook_test.go`:

```go
// TestShadowCaptureRecordsLedgerRow: 성공한 포착마다 원장에 분모 행이 하나 남고, 저장되지
// 않은 호출은 남기지 않는다. 이 분모가 없으면 회수율의 분모를 72시간 스냅샷에서 세게 되고,
// 그것이 세션 54가 상계 11.6%를 잘못 낸 형태다 — 13일치 분자를 사흘치 분모로 나눴다.
// 읽는 자리는 store.LedgerStats(contentDir)다. **임계 미달 케이스는 store를 열기 전에
// 반환하므로 ledger.db 자체가 없고, 그때 LedgerStats는 nil 슬라이스+nil을 낸다** — 그것을
// "0행"으로 받는다(Fatal이 아니다).
func TestShadowCaptureRecordsLedgerRow(t *testing.T) {
	_, _, contentDir, sdir := shadowSetup(t)
	ad, err := session.OpenAppend(context.Background(), sdir, session.AppendOptions{
		ExternalSessionID: "cc:3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		Producer:          "context-router/test",
	})
	if err != nil {
		t.Fatalf("OpenAppend: %v", err)
	}
	defer func() { _ = ad.Close() }()
	getenv := func(string) string { return "" }

	// ① 임계 미달 → 저장도 분모도 없다(store를 열기 전에 반환한다).
	small, err := json.Marshal(bigStdout(100))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	shadowCapture(context.Background(), ad,
		hookInput{HookEventName: "PostToolUse", ToolName: "Bash", ToolResponse: small},
		sdir, contentDir, "cc:3f2504e0-4f89-41d3-9a0c-0305e82c3301", getenv)
	if n := hookLedgerRows(t, contentDir); n != 0 {
		t.Fatalf("임계 미달인데 분모 행=%d", n)
	}

	// ② 임계 초과 → 저장 1건, 분모 1행.
	big, err := json.Marshal(bigStdout(20000))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	shadowCapture(context.Background(), ad,
		hookInput{HookEventName: "PostToolUse", ToolName: "Bash", ToolResponse: big},
		sdir, contentDir, "cc:3f2504e0-4f89-41d3-9a0c-0305e82c3301", getenv)
	if n := contentArtifacts(t, contentDir); n != 1 {
		t.Fatalf("artifacts=%d want 1", n)
	}
	if n := hookLedgerRows(t, contentDir); n != 1 {
		t.Fatalf("성공한 포착 뒤 분모 행=%d want 1", n)
	}
}

// hookLedgerRows — contentDir 원장의 hook:shadow 행 수. ledger.db 미존재는 nil 슬라이스라
// 자연히 0이 된다(store.LedgerStats 계약).
func hookLedgerRows(t *testing.T, contentDir string) int64 {
	t.Helper()
	rows, err := store.LedgerStats(contentDir)
	if err != nil {
		t.Fatalf("LedgerStats: %v", err)
	}
	for _, r := range rows {
		if r.Tool == "hook:shadow" {
			return r.Calls
		}
	}
	return 0
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/store -run TestLedgerAppendContextUnderContention -count=1 -v && go test ./internal/hook -run TestShadowCaptureRecordsLedgerRow -count=1 -v`
Expected: 컴파일 실패(`LedgerAppendContext` undefined) · FAIL(분모 행 0)

- [ ] **Step 3: store 구현 — ctx를 받는 경로 하나**

`LedgerAppend`(893-899행)를 이렇게 고친다. SQL은 한 자리에만 둔다(D13).

```go
// LedgerAppend: best-effort 사용량 기록 — ledger 없음/오류는 무시(§3.5).
// 기존 호출부 열 곳(전부 internal/mcp)의 시그니처를 유지하려고 남긴 얇은 위임이다.
func (s *Store) LedgerAppend(tool string, stored, returned, ms int64) {
	s.LedgerAppendContext(context.Background(), tool, stored, returned, ms)
}

// LedgerAppendContext — D103 계약 8: ctx를 ExecContext로 넘기는 원장 기록. **훅이 부르는
// 경로가 이것이다**: ledger.db의 busy_timeout은 5000 ms인데 훅의 총예산은 2000 ms라
// (internal/hook/hook.go:60·112) ctx 없이 쓰면 훅 프로세스가 겹칠 때 그 INSERT가 예산 밖에서
// 블록된다. 예산 초과는 오류로 돌아오고 best-effort라 삼켜진다 — 훅의 fail-open이 유지된다.
func (s *Store) LedgerAppendContext(ctx context.Context, tool string, stored, returned, ms int64) {
	if s.ledger == nil {
		return
	}
	_, _ = s.ledger.ExecContext(ctx,
		`INSERT INTO ledger(ts,tool,bytes_stored,bytes_returned,duration_ms) VALUES(?,?,?,?,?)`,
		time.Now().Unix(), tool, stored, returned, ms)
}
```

- [ ] **Step 4: 훅 구현**

`shadowCapture`(33행) 선두에 시작 시각을 잡는다:

```go
func shadowCapture(ctx context.Context, ad *session.AppendDB, in hookInput, dir, contentDir, external string, getenv func(string) string) {
	start := time.Now()
```

`ingest.Run` 성공 판정(85-88행) **직후**, `ref` 조립(90행) **앞**에 붙인다:

```go
	// D103 계약 4: 회수율의 **분모**다. 성공한 ingest 뒤에만 쓴다 — 임계 미달·denylist·
	// 바이너리·스토어 열기 실패로 끝난 호출은 저장되지 않았으므로 회수 대상이 아니다.
	// 훅은 writable로 스토어를 열므로(위 OpenContext의 readOnly=false) ledger 연결이 이미
	// 있다 — 지금까지 쓰지 않았을 뿐이다. ctx를 넘기는 이유는 계약 8: ledger.db의
	// busy_timeout(5000ms)이 훅 총예산(2000ms)보다 커서, 겹친 훅에서 ctx 없이 쓰면 예산 밖에서
	// 블록된다. best-effort라 실패해도 포착 자체와 훅의 fail-open 성질은 바뀌지 않는다.
	// 이름이 ctr_로 시작하지 않는 것도 계약이다 — D104의 채택 문턱은 ctr_* 행만 센다.
	st.LedgerAppendContext(ctx, "hook:shadow", int64(size), 0, time.Since(start).Milliseconds())
```

`time`을 `internal/hook/shadow.go`의 import에 더한다(현재 없다).

- [ ] **Step 5: 통과 확인 + 실물 관측**

```bash
go test ./internal/store -run TestLedgerAppendContextUnderContention -count=1 -v
go test ./internal/hook -count=1
go run ./cmd/context-router doctor | grep -E '^\[12\]'   # shadow drop이 늘지 않아야 한다
```

Expected: 경합 최악 소요가 로그로 남고 2000 ms 미만, 기존 shadow 테스트 전부 PASS, drop 무변화.
**경합 INSERT가 훅 예산을 넘기면 이 태스크를 멈추고 보고한다** — 설계 §4-4가 미리 열어 둔 자리이고, 그때의 결론은 "분모를 다른 자리에서 센다"이지 "분모 없이 배송한다"가 아니다.

- [ ] **Step 6: 게이트 다섯 + 커밋**

```bash
go build ./... && go vet ./... && go test ./... -count=1 && gofumpt -l . && golangci-lint run
git add internal/store internal/hook
git commit -m "feat(hook): 포착마다 원장에 분모 행을 남긴다 — 훅 ctx 경유 (D103 계약 4·8)"
```

---

### Task 10: `stats`가 회수 실적을 낸다

**Files:**
- Modify: `internal/cli/cli.go:204-229` (`runStatsLocal`)
- Test: `internal/cli/cli_test.go`

**Interfaces:**
- Consumes: `store.LedgerFetchStats`
- Produces: 없음

- [ ] **Step 1: 실패하는 테스트를 쓴다**

셋업은 `TestRunStats_Local`(`cli_test.go:1581`)의 관례를 그대로 쓴다 — 임시 storeRoot/projectRoot, `store.Open(projDir,false)`로 원장을 시드하고 `Run(ctx,"stats",...)`을 부른다.

```go
// TestStatsPrintsFetchStats: stats가 회수 실적 줄을 낸다. 이 줄이 D104의 착수 조건을 사람이
// 눈으로 확인하는 자리다 — ctr_* 호출 10건·해소 30건 또는 미해소 5건. **총 호출을 병기하는
// 것이 계약**이다: 이 릴리스부터 위 표의 ctr_fetch calls가 뜻을 바꾸고(전에는 성공만, 이제
// 성공 + artifact 부재), 채택 문턱이 읽는 수가 바로 그 총계다(D103 계약 9).
func TestStatsPrintsFetchStats(t *testing.T) {
	storeRoot, projectRoot := t.TempDir(), t.TempDir()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	projDir := filepath.Join(storeRoot, "projects", canon.ProjectID)
	st, err := store.Open(projDir, false)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	st.LedgerAppendFetch(100, 1, 7, 600)  // 해소(나이 600s)
	st.LedgerAppendFetch(200, 1, 9, 1800) // 해소(나이 1800s)
	st.LedgerAppendFetch(0, 1, 0, 0)      // 미해소
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	var out, errOut bytes.Buffer
	if err := Run(context.Background(), "stats", nil, storeRoot, projectRoot, "0.0.1-dev", &out, &errOut); err != nil {
		t.Fatalf("Run stats err=%v out=%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"fetch\t", "calls=3", "resolved=2", "missed=1", "max=1800"} {
		if !strings.Contains(got, want) {
			t.Fatalf("회수 실적 줄에 %q 없음:\n%s", want, got)
		}
	}
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/cli -run TestStatsPrintsFetchStats -count=1 -v`
Expected: FAIL — 그 줄이 없다

- [ ] **Step 3: 구현**

`runStatsLocal`의 합계 줄(227행) 다음, `return nil`(228행) 앞에 붙인다.

```go
	// D103 계약 9: 회수 실적 한 줄. D104의 착수 조건(ctr_* 10건 · 해소 30건 또는 미해소 5건)을
	// 여기서 읽는다. 총 호출을 같은 줄에 병기하는 이유: 이 릴리스부터 위 표의 ctr_fetch calls가
	// 뜻을 바꾸고(전에는 성공만, 이제 성공 + artifact 부재) 채택 문턱이 읽는 수가 그 총계다 —
	// 병기하지 않으면 해소·미해소가 둘 다 0인 것과 원장 쓰기가 깨진 것이 구분되지 않는다.
	// 오류는 위 LedgerStats 호출(213-216)과 같은 방식으로 반환한다 — 이관 전 원장은 이미 오류
	// 없는 0 결과이므로 여기서 삼킬 것이 없고, 삼키면 권한·손상 실패가 "데이터 없음"으로 읽힌다.
	fs, err := store.LedgerFetchStats(projDir)
	if err != nil {
		return fmt.Errorf("stats: 회수 실적 집계 실패: %w", err)
	}
	fmt.Fprintf(w, "fetch\tcalls=%d\tresolved=%d\tmissed=%d\tage_s p50=%d p90=%d max=%d\n",
		fs.Calls, fs.Resolved, fs.Missed, fs.AgeP50, fs.AgeP90, fs.AgeMax)
	return nil
```

- [ ] **Step 4: 통과 확인 + 실물**

```bash
go test ./internal/cli -count=1
go run ./cmd/context-router stats | tail -3
```

기존 `TestRunStats_Local`이 금지 문자열(`token`·`$`)을 전 출력에서 재므로 새 줄에 그 둘이 없는지 함께 확인한다(현재 문면에는 없다). `TestRunStats_Local_NoLedger`(원장 부재)는 전부 0인 줄을 받는다 — 오류가 아니다.

- [ ] **Step 5: 게이트 다섯 + 커밋**

```bash
go build ./... && go vet ./... && go test ./... -count=1 && gofumpt -l . && golangci-lint run
git add internal/cli
git commit -m "feat(stats): 회수 실적을 낸다 — D104 착수 조건을 읽는 자리 (D103 계약 9)"
```

---

### Task 11: v0.20 배송 + 측정 구간 개시

**Files:**
- Modify: `internal/buildinfo/buildinfo.go:11`, `CHANGELOG.md`

- [ ] **Step 1: 버전을 `0.20.0`으로 올린다**

`internal/buildinfo/buildinfo.go:11`. 잔여 하드코딩 확인: `grep -rn '0\.19\.1' --include=*.go . | grep -v _test`

- [ ] **Step 2: CHANGELOG `[0.20.0]` 절**

```markdown
## [0.20.0] — 2026-08-XX

### Added

- **회수 실적 계측** (D103). `ledger.db`에 `artifact_id`·`artifact_age_s` 두 열이 붙고,
  `ctr_fetch`는 **해소했을 때와 artifact가 없어 실패했을 때 모두** 행을 남긴다 — 앞의 것은
  "창이 충분했다", 뒤의 것은 "창이 짧았다"의 증거다. 훅 포착마다 분모 행(`hook:shadow`)이
  하나 남는다. 나이는 회수 시점에 박고 시계·범위는 D67 퍼지와 같은
  `max(sources.indexed_at)` / `content_hash`이므로, 아티팩트가 지워진 뒤에도 분포가 남는다.
- `stats`에 `fetch: calls=… resolved=… missed=… age_s p50/p90/max` 줄.

### Changed

- ★ **`stats` 표의 `ctr_fetch` calls가 뜻을 바꾼다.** 전에는 성공한 회수만 셌고, 이제
  **성공 + artifact 부재**를 센다(잘못된 선택자나 잘못된 chunk id는 여전히 세지 않는다 —
  그것들은 창의 길이에 대해 아무 말도 하지 않는다). 같은 원장의 옛 행은 그대로 남고 새 두 열이
  NULL이라 해소·미해소 어느 쪽에도 들지 않는다.

### Unchanged (의도)

- **72시간 보존 창은 그대로다** (D104). 늘리거나 줄이는 판단은 이 계측이 배포된 뒤 14일,
  그리고 `ctr_*` 호출 10건 이상 + (해소 30건 또는 미해소 5건)이 찼을 때 한다.
  **미달이면 "늘리지 않는다"가 결론이다** — 설계서에 판정 규칙을 미리 박아 뒀다.
```

- [ ] **Step 3: 게이트 다섯 + 커밋**

```bash
go build ./... && go vet ./... && go test ./... -count=1 && gofumpt -l . && golangci-lint run
git add internal/buildinfo/buildinfo.go CHANGELOG.md
git commit -m "chore(release): 0.20.0 — 회수 실적 계측 (D103·D104)"
```

- [ ] **Step 4: 측정 구간을 연다 (운영 조작 — 코드 아님)**

배송 뒤 소유자가 서버 환경에 **`CTR_SHADOW_RETENTION=336h`**(14일)를 세운다. 코드 변경이 아니라 환경 변수 하나이고(D101이 구성값을 환경 변수로 수렴시켜 뒀다), 그 구간의 나이 분포는 14일까지 절단이 없다 — **현행 창에서 우측 절단된 분포로는 "사흘이 짧은가"에 구조적으로 답할 수 없다**는 것이 D104의 처방 근거다.

- 알고 받는 대가: 그 기간 live 바이트가 **800 MB 안팎**으로 올라 `doctor [14]`가 내내 경고한다 — **의도된 상태이고, 경고가 그것을 정확히 보고하는 것이 D102 계약 6이 사는 증거다.**
- **14일이 지나면 변수를 지운다.** 그 다음 기동의 퍼지가 초과분을 걷는다.
- 판정은 `stats`의 회수 실적 줄을 읽어 D104의 다섯 행 표(위에서부터 첫 일치)로 한다 — 이 계획의 범위 밖이고, 다음 판단 세션의 일이다.

---

## 자기검토 결과

**스펙 대조** — v0.20 설계서의 계약(D102 아홉 · D103 아홉)과 태스크 대응:

| 계약 | 태스크 |
|---|---|
| D102-1 병합은 원시 명령 하나, 시점은 호출자 셋 | T1(원시) · T3(자동) · T4(수동 둘) |
| D102-2 자동 경로는 자기 주기 고루틴(스탬프 mtime, 미래 mtime 포함) | T2(게이트) · T3(고루틴·배선 자리) |
| D102-3 원시 명령은 `optimize`, `merge=N` 기각, 손잡이 없음 | T1(주석에 번들 3.53.3 / v1.54.0 근거 인용) |
| D102-4 수동 회수가 병합 선행 — 종료 상태 반영, 스탬프 미갱신 | T4 |
| D102-5 자동 경로는 VACUUM 없음 | T3(병합만 하고 VACUUM을 부르지 않는다) |
| D102-6 판정 기준 live 바이트 + 임계 256 MiB | T5 |
| D102-7 `free=` 병기 — 표시이지 판정이 아니다 | T5 |
| D102-8 문면 "자동 VACUUM 없음" + 보존 창 병기 | T5 |
| D102-9 병합↔훅 충돌은 수용 위험, `doctor [12]`로 관측 | T3(주기 근거) · T6 Step 4(배포 후 drop 추이) |
| D103-1 열 둘 + 레거시/미해소/해소 세 상태 | T7(열·판정) · T8a(−1 기록) |
| D103-2 해소 시 나이 박기 — hash 범위 max(indexed_at) | T8a(store) · T8b(핸들러) |
| D103-3 `ErrNotFound` 다섯 중 artifact 부재만 | T8b |
| D103-4 훅 분모, `ctr_*` 아닌 이름 | T9 |
| D103-5 원장 무기한 | T7·T9 (지우는 코드를 넣지 않는다) |
| D103-6 S4 — 정수와 도구 이름만 | T8a·T8b·T9 (선택자·경로·내용을 담지 않는다) |
| D103-7 read-only 내성 + `PRAGMA table_info` + 부분 이관 | T7 |
| D103-8 훅의 원장 쓰기는 훅의 ctx | T9 |
| D103-9 `stats` 회수 실적 줄(총 호출 병기) | T10 · T11(CHANGELOG의 의미 변경) |
| D104 측정 구간 창 확대(환경 변수) | T11 Step 4 (운영 조작, 코드 없음) |
| D104 착수 조건·판정 5행 | 설계서 (규칙 자체) + T10 (읽는 자리) |

**미확정 대응**: 설계서 §4-1(라이브 `optimize`의 소요·쓰기 바이트·`-wal` 피크) → **T6 Step 4가 배포 후 관측 항목으로 고정**한다. §4-2(`optimize`의 ctx 취소 반응성) → T3의 종료 경로가 그것을 실제로 밟으므로 같은 관측에 싣는다. §4-3(`checkFTSIntegrity` 대칭) → **이 계획은 대칭을 만들지 않는다**: 병합을 store 메서드로 두고 호출자가 정책을 갖는 형태라 두 퍼지 메서드의 후처리 차이를 건드리지 않는다. §4-4(훅 예산) → **T9 Step 1의 경합 테스트가 잰다**(단일 무경합 실행이 아니다). §4-5(다른 프로젝트 스토어) → 계획 밖.

**설계서와 어긋난 것 없음.** 이전 판의 계획이 자동 병합을 기동 퍼지 고루틴 안에 두고 `purgeErr`로 게이트했는데, 그 형태는 D102 계약 2가 명시로 배제한 셋(기동 1회·`purgeErr`·60초 예산 공유)에 전부 걸렸다 — T3이 자기 고루틴으로 옮겨 닫았다. 검토가 낸 나머지 제안 둘은 계약이 이미 답한 것이라 채택하지 않는다: **`merge=N` 전환**은 계약 3이 산술로 기각했고, **"마지막 병합 이후 변경 있었나" 조건 추가**는 계약 3의 성질(이미 병합된 인덱스의 `optimize`는 일 없이 반환) 때문에 아낄 비용이 없다.
