# 퍼지 감사 로그 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 퍼지가 **언제·무엇을·어떤 정책으로** 지웠는지를 프로젝트별 사이드카 로그에 남겨, 삭제가 어디에도 기록되지 않던 구멍을 닫는다.

**Architecture:** `internal/store` 가 형식과 쓰기를 소유한다 — 삭제를 실제로 하는 패키지이자 `projects/<pid>` 경로를 아는 자리다. `internal/cli` 가 파서와 진단 문면을 소유한다(`dropsByReason` 이 이미 세운 쓰기/읽기 분리 선례). **호출은 삭제 경로 넷의 호출자가 한다** — `status` 분류가 ctx 종료 사유에 걸려 있고 그 정보는 store 안에 없다. 원장 DB 를 쓰지 않는 이유가 설계의 요지다: 퍼지는 `lockStoreCtx` 를 쥔 60초 예산 구간에서 돌고 그 자리의 ledger INSERT 는 best-effort 라 실패하면 조용히 사라진다 — **그것이 애초의 문제였다.**

**Tech Stack:** Go 1.26 표준 라이브러리만. **새 의존성 0.** 새 스키마 0(사이드카 텍스트 파일).

**Spec:** `docs/superpowers/specs/2026-08-14-purge-audit-log-design.md` — 이 계획은 그 스펙에서 논증한다. 실행자는 **둘 다** 읽는다.

## Global Constraints

스펙의 프로젝트 전역 요구사항이다. **모든 태스크의 요구사항에 암묵적으로 포함된다.**

- **게이트 다섯을 매 태스크 끝에 직접 돌린다**: `go build ./...` · `go vet ./...` · `go test ./... -count=1 -p 1` · `gofumpt -l .`(무출력) · `golangci-lint run`(0 issues). **`&&` 로 잇지 마라** — 하나가 빨개도 나머지를 봐야 한다.
- **`internal/exec` 의 넷은 이 머신의 환경 조건이다**(`TestNpmrcScratchConfigWins` · `TestRunShellPS51ProviderWriteReturns` · `TestD78SignalOnExit` · `TestD78NoObservableSideEffects`). 이 계획은 그 패키지를 한 줄도 안 건드리므로 **회귀가 아니다.** 다른 패키지가 빨가면 그때가 회귀다.
- **`go test` 에 `-p 1` 필수.**
- **행 번호를 주석·문서에 적지 마라** — 심볼 이름을 적는다.
- **긴 테스트 데이터는 `strings.Repeat` 또는 testdata** — 리터럴로 늘어놓지 않는다.
- **★ 테스트 헬퍼 이름을 지어내지 마라.** 이 계획의 `newTestServerRoot` · `runServerOnce` · `newPurgeFixture` 는 **"그 역할을 하는 기존 헬퍼로 바꿔 쓰라"는 지시**이지 존재하는 이름이 아니다. 각 태스크의 첫 걸음은 해당 테스트 파일에서 가장 가까운 기존 테스트를 찾아 그 형태를 읽는 것이다. 없으면 그 테스트를 복제해 만든다.
- **best-effort 계약**: 로그 쓰기 실패는 `slog.Warn` 한 줄이고 **퍼지 결과·종료 코드가 불변**이다(`appendDrop` 과 동형).
- **★ 설치하지 마라.** 판정일(2026-08-26) 전에 설치하면 진행 중인 14일 계측이 오염된다. 빌드·테스트까지만 하고 `go install` 은 하지 않는다.
- **`ctr_*` MCP 도구를 이 저장소에서 호출하지 마라** — 회수 줄이 판정 지표다.

## File Structure

| 파일 | 책임 | 규모 |
|---|---|---|
| `internal/store/purgelog.go` **(신규)** | 형식·쓰기·위생·정책 문자열 조립 | ~110줄 |
| `internal/store/purgelog_test.go` **(신규)** | 위 넷의 단위 테스트 | ~180줄 |
| `internal/store/store.go` **(수정)** | `ProjectDir()` 접근자 한 줄 — **기록이 프로젝트 디렉터리를 알아야 한다.** `[실측]` 그런 접근자가 지금 없고 `s.dir` 이 그 디렉터리가 맞다(`store.Open` 이 모든 자리에서 `filepath.Join(storeRoot,"projects",id)` 로 불린다) | +4줄 |
| `internal/cli/purgelog.go` **(신규)** | 파서 (파일명은 `store.PurgeLogName` 을 직접 쓴다 — 미러 없음) | ~90줄 |
| `internal/cli/purgelog_test.go` **(신규)** | 파서 단위 테스트 | ~150줄 |
| `cmd/context-router/main.go` **(수정)** | 기동 퍼지 switch — **0건 분기 신설** + 모든 분기에서 기록 | +40줄 |
| `internal/cli/cli.go` **(수정)** | CLI 세 경로 배선 + `doctor [21]` | +60줄 |

**새 파일을 만드는 이유**: `internal/store/store.go` 는 이미 1,956줄이고 `internal/cli/cli.go` 는 2,451줄이다. `internal/cli` 는 이미 책임별로 파일이 갈려 있다(`codex_scan.go` · `mcp_install.go` · `rollout.go` · `compare.go`) — 그 관행을 따른다. `internal/store` 는 파일이 적지만, 이 기능은 SQL 을 한 줄도 쓰지 않아 `store.go` 의 어느 절에도 속하지 않는다.

---

### Task 1: `internal/store` — 형식·쓰기·정책 문자열

**Files:**
- Create: `internal/store/purgelog.go`
- Test: `internal/store/purgelog_test.go`

**Interfaces:**
- Consumes: 없음(이 태스크가 첫 번째다).
- Produces: 후속 태스크 전부가 이것들을 쓴다 —
  - `store.PurgeRecord` 구조체(필드: `Path, Policy, Status string` · `Cutoff, Count int64` · `Bytes *int64` · `Deferred, Failed *int`)
  - `store.AppendPurgeLog(projectDir string, rec PurgeRecord)` — 반환값 없음
  - `store.PurgePolicy(d time.Duration, source string) string` — `"336h0m0s/pwsh-profile"` 형태
  - `store.PurgeLogName` 상수 = `"purge.log"` — **`internal/cli` 가 이것을 직접 쓴다. 미러 상수를 만들지 마라**(검토 소견 F8)
  - `func (s *Store) ProjectDir() string { return s.dir }` — `store.go` 에 넣는다. `[실측]` 그런 접근자가 지금 없고 `s.dir` 이 프로젝트 디렉터리가 맞다
  - `store.PurgeStatus(purgeErr error, hashes int, cancelled, budgetSpent, capped bool) string` — 결과 분류. **`internal/store` 에 두는 이유**: 기동 경로(`cmd/context-router`)와 CLI 경로(`internal/cli`)가 **같은 분류를 써야 한다**(검토 소견 F4). `cmd` 에 두면 CLI 가 손을 못 대 두 벌이 생긴다. 우선순위 표 테스트도 이 태스크 소관이다

- [ ] **Step 1: 실패하는 테스트를 쓴다**

`internal/store/purgelog_test.go`:

```go
package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAppendPurgeLogFields — 9필드 고정과 미측정(`-`) 대 실측 0의 구분을 잠근다.
// 뒤 세 칸이 포인터인 이유가 그것이다(스펙 §3): 0은 "쟀더니 0"이라는 주장이고,
// 함수가 애초에 그 값을 내지 않는 경로에서 0을 쓰면 거짓 측정이 된다.
func TestAppendPurgeLogFields(t *testing.T) {
	dir := t.TempDir()
	zero := int64(0)
	zeroI := 0
	AppendPurgeLog(dir, PurgeRecord{
		Path: "startup-shadow", Policy: "336h0m0s/pwsh-profile", Status: "ok",
		Cutoff: 1753971299, Count: 0, Bytes: &zero, Deferred: &zeroI, Failed: &zeroI,
	})
	data, err := os.ReadFile(filepath.Join(dir, PurgeLogName))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	line := strings.TrimRight(string(data), "\n")
	f := strings.Split(line, "\t")
	if len(f) != 9 {
		t.Fatalf("필드 %d개 want 9: %q", len(f), line)
	}
	// ts는 실행 시각이라 값을 고정하지 않는다 — 숫자인 것만 본다.
	if f[0] == "" || strings.ContainsAny(f[0], "abcdef-") {
		t.Fatalf("ts가 unix 초가 아니다: %q", f[0])
	}
	want := []string{"startup-shadow", "336h0m0s/pwsh-profile", "1753971299", "ok", "0", "0", "0", "0"}
	if got := f[1:]; !equalSlice(got, want) {
		t.Fatalf("필드 = %q\nwant %q", got, want)
	}
}

// TestAppendPurgeLogUnmeasuredIsDash — 못 잰 것은 `-`, 재서 0인 것은 `0`.
// cli-older-than 경로가 바이트·유예·실패를 내지 않으므로(스펙 §2.0) 이 구분이 없으면
// 읽는 쪽이 "회수 바이트가 0이었다"는 하지 않은 측정을 읽는다.
func TestAppendPurgeLogUnmeasuredIsDash(t *testing.T) {
	dir := t.TempDir()
	AppendPurgeLog(dir, PurgeRecord{
		Path: "cli-older-than", Policy: "720h0m0s/-", Status: "ok", Cutoff: 0, Count: 12,
	})
	data, _ := os.ReadFile(filepath.Join(dir, PurgeLogName))
	f := strings.Split(strings.TrimRight(string(data), "\n"), "\t")
	if len(f) != 9 {
		t.Fatalf("필드 %d개 want 9", len(f))
	}
	if f[3] != "-" {
		t.Errorf("cutoff=%q want %q — 0은 1970년이 아니라 '경계 개념 없음'이다", f[3], "-")
	}
	if f[5] != "12" {
		t.Errorf("count=%q want 12", f[5])
	}
	for i, name := range map[int]string{6: "bytes", 7: "deferred", 8: "failed"} {
		if f[i] != "-" {
			t.Errorf("%s=%q want %q — nil은 미측정이다", name, f[i], "-")
		}
	}
}

// TestAppendPurgeLogSanitizes — 탭·개행·CR은 공백으로, 64자 상한, 빈 값은 `-`.
// CTR_RETENTION_SOURCE는 사람이 임의 문자열을 넣는 자리다 — 위생이 없으면 그 한 값이
// 파서를 오염시키고, 그것이 이 로그가 막으려던 바로 그 종류의 침묵을 만든다(스펙 §2.4).
func TestAppendPurgeLogSanitizes(t *testing.T) {
	dir := t.TempDir()
	AppendPurgeLog(dir, PurgeRecord{
		Path: "cli-gc", Policy: "a\tb\nc\rd", Status: strings.Repeat("x", 100), Count: 3,
	})
	data, _ := os.ReadFile(filepath.Join(dir, PurgeLogName))
	line := strings.TrimRight(string(data), "\n")
	if strings.Count(line, "\t") != 8 {
		t.Fatalf("탭이 8개가 아니다 — 위생이 필드를 쪼갰다: %q", line)
	}
	f := strings.Split(line, "\t")
	if f[2] != "a b c d" {
		t.Errorf("policy=%q want %q", f[2], "a b c d")
	}
	if len(f[4]) != 64 {
		t.Errorf("status 길이=%d want 64(상한)", len(f[4]))
	}
}

// TestAppendPurgeLogAppends — append-only. 두 번 부르면 두 줄이다.
func TestAppendPurgeLogAppends(t *testing.T) {
	dir := t.TempDir()
	AppendPurgeLog(dir, PurgeRecord{Path: "cli-gc", Status: "ok"})
	AppendPurgeLog(dir, PurgeRecord{Path: "cli-gc", Status: "ok"})
	data, _ := os.ReadFile(filepath.Join(dir, PurgeLogName))
	if n := strings.Count(string(data), "\n"); n != 2 {
		t.Fatalf("줄 %d개 want 2 — append-only가 아니다", n)
	}
}

// TestAppendPurgeLogBestEffort — 쓸 수 없는 자리에서도 패닉하지 않고 조용히 넘어간다.
// 퍼지 결과에 영향이 없다는 계약(스펙 §5)의 하한이다.
func TestAppendPurgeLogBestEffort(t *testing.T) {
	f := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	AppendPurgeLog(f, PurgeRecord{Path: "cli-gc", Status: "ok"}) // 디렉터리가 아니다 — 패닉하지 않아야 한다
}

// TestPurgePolicy — 값과 출처를 한 칸에 합치고, 빈 출처는 `-`.
// 빈 출처 자체가 신호다: 프로필 처방이 닿지 않은 계통이라는 뜻이다(스펙 §2.3).
func TestPurgePolicy(t *testing.T) {
	if got := PurgePolicy(336*time.Hour, "pwsh-profile"); got != "336h0m0s/pwsh-profile" {
		t.Errorf("PurgePolicy = %q want %q", got, "336h0m0s/pwsh-profile")
	}
	if got := PurgePolicy(72*time.Hour, ""); got != "72h0m0s/-" {
		t.Errorf("빈 출처 = %q want %q", got, "72h0m0s/-")
	}
}

// TestPurgeStatusPriority — 스펙 §2.2의 우선순위가 계약이다(§6 항목 6). **partial이 budget·
// cancelled를 이긴다**: 행이 이미 삭제돼 다음 기동 술어에 다시 안 잡히므로, 중단 사유가
// 무엇이든 남은 파일의 유일한 회수 경로가 purge --gc다. 순서가 뒤집히면 "다음 기동이 다시
// 집는다"는 **거짓 안내**가 나간다. 예산 소진·종료 취소를 e2e로 유도하는 것은 비싸고
// 불안정하므로 분류를 순수 함수로 두고 표로 잰다 — 이 표가 없으면 순서 교체가 조용히 통과한다.
func TestPurgeStatusPriority(t *testing.T) {
	errX := errors.New("x")
	for _, tc := range []struct {
		name                      string
		err                       error
		hashes                    int
		cancelled, budget, capped bool
		want                      string
	}{
		{"정상 0건", nil, 0, false, false, false, "ok"},
		{"정상 N건", nil, 5, false, false, false, "ok"},
		{"상한", nil, 100, false, false, true, "capped"},
		{"행 삭제 실패", errX, 0, false, false, false, "failed"},
		{"예산 소진", errX, 0, false, true, false, "budget"},
		{"종료 취소", errX, 0, true, false, false, "cancelled"},
		{"커밋 뒤 취소", errX, 5, true, false, false, "partial"},
		{"커밋 뒤 예산", errX, 5, false, true, false, "partial"},
		{"커밋 뒤 둘 다", errX, 5, true, true, false, "partial"},
	} {
		if got := PurgeStatus(tc.err, tc.hashes, tc.cancelled, tc.budget, tc.capped); got != tc.want {
			t.Errorf("%s: %q want %q", tc.name, got, tc.want)
		}
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/store/... -count=1 -p 1 -run 'PurgeLog|PurgePolicy'`
Expected: 컴파일 실패 — `undefined: AppendPurgeLog`, `undefined: PurgeRecord`, `undefined: PurgeLogName`, `undefined: PurgePolicy`

- [ ] **Step 3: 최소 구현**

`internal/store/purgelog.go`:

```go
// 퍼지 감사 로그 — 삭제가 어디에도 기록되지 않던 구멍을 닫는다.
//
// **왜 원장 DB가 아닌가**: 퍼지는 lockStoreCtx를 쥔 60초 예산 구간에서 돌고, 그 자리의
// ledger INSERT는 best-effort라 실패하면 조용히 사라진다 — 그것이 애초의 문제였다. 판정
// 표면(stats의 tool별 표)도 건드리지 않는다: 그 표에 새 tool 행이 끼면 "계상 방식이
// 바뀌었다" 계열의 혼란이 하나 더 생긴다.
//
// 프로젝트 레벨인 이유는 삭제의 단위가 content.db이고 그것이 이 디렉터리에 있기 때문이다.
// worktree 레벨인 session.drops.log(hook 소유)와 층이 다르다.
package store

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// PurgeLogName — 사이드카 파일명. internal/cli가 미러 상수로 든다(dropsFileName 관례).
const PurgeLogName = "purge.log"

// purgeFieldMax — 필드당 상한(바이트). hook.appendDrop의 san과 같은 값이다.
const purgeFieldMax = 64

// PurgeRecord — 한 삭제 배치의 기록. 호출자가 채우고 AppendPurgeLog가 조립한다.
//
// **뒤 세 칸이 포인터인 이유**: 0은 "쟀더니 0이었다"는 주장이고, 함수가 애초에 그 값을
// 내지 않는 경로에서 그것을 쓰면 **거짓 측정**이 된다. 이 저장소는 같은 함정을 이미 한 번
// 지났다 — recordFetchResolve의 ageS *int64가 "0으로 두면 같은 초에 회수한 행과 구분되지
// 않아 분포가 내려간다"는 이유로 nil을 쓴다(D103 소견 F9). 같은 규칙: **못 잰 것은 `-`,
// 재서 0인 것은 `0`.** 경로별로 어느 칸이 채워지는지는 스펙 §2.0의 의미표가 든다.
type PurgeRecord struct {
	Path   string // 삭제 경로 라벨(닫힌 집합) — startup-shadow · cli-hook-only · cli-older-than · cli-gc
	Policy string // 실효 정책 + 출처. PurgePolicy가 조립한다. cli-gc는 "-"
	Status string // 결과 분류(닫힌 집합) — failed · partial · budget · cancelled · capped · ok
	Cutoff int64  // 경계 unix 초. 0이면 "-"(cli-gc는 경계 개념이 없다)
	Count  int64  // 삭제 단위 수 — **의미가 경로마다 다르다**(스펙 §2.0)
	Bytes  *int64 // 실제 unlink된 바이트. nil이면 "-"
	Deferred, Failed *int // age-gate 유예 / unlink 실패. nil이면 "-"
}

// PurgePolicy — 실효 보존값과 그 출처를 한 칸에 합친다.
//
// **한 칸인 이유**: 두 값이 항상 함께 읽히고, 합쳐 두면 성공 기준이 `grep '72h0m0s/'` 한
// 줄이 된다 — 200개가 사라진 그 기동의 행에 `72h0m0s/-`가 적혀 있었다면 그 규명은 한 줄
// 조회였다. 쪼개면 그 한 줄을 잃는다.
//
// **조립을 여기 한 곳에 두는 이유**(D13): 호출 지점이 셋이고, 각자 fmt.Sprintf로 손수
// 합치면 다음 수정이 한 곳에만 닿는다.
//
// **파싱은 첫 `/`로만 쪼갠다** — 앞칸인 Duration.String()에는 `/`가 없어 안전하고, 뒤칸인
// 출처는 사람이 임의 문자열을 넣는 자리라 `/`가 들어올 수 있다.
func PurgePolicy(d time.Duration, source string) string {
	if source == "" {
		source = "-"
	}
	return d.String() + "/" + source
}

// sanPurgeField — hook.appendDrop의 san과 **같은 규칙**이다: 탭·개행·CR을 공백으로, 64자
// 상한, 빈 값은 "-".
//
// **왜 부르지 않고 다시 쓰는가**: 그쪽은 클로저이고, 방향도 막혀 있다 — internal/hook이
// internal/store를 import하므로 store가 hook을 부를 수 없다.
//
// **★ 그리고 두 벌이 갈리는 것을 막는 장치는 없다**(검토 소견 F9). TestAppendPurgeLogSanitizes는
// 이쪽 사본만 잰다 — hook 쪽 san의 상한이나 치환 집합이 바뀌어도 아무것도 빨개지지 않는다.
// 진짜 해소는 이 규칙을 store에서 export하고 hook.appendDrop이 그것을 부르는 것이고(import
// 방향이 이미 그것을 허용한다), 그것은 internal/hook을 건드리므로 **별건으로 이월한다.**
// 여기 적어 두는 이유는 "테스트가 잠근다"는 거짓 주장을 남기지 않기 위해서다.
func sanPurgeField(s string) string {
	if s == "" {
		return "-"
	}
	s = strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(s)
	if len(s) > purgeFieldMax {
		s = s[:purgeFieldMax]
	}
	return s
}

// optInt64 / optInt — nil을 "-"로, 값이 있으면 10진수로.
func optInt64(p *int64) string {
	if p == nil {
		return "-"
	}
	return strconv.FormatInt(*p, 10)
}

func optInt(p *int) string {
	if p == nil {
		return "-"
	}
	return strconv.Itoa(*p)
}

// AppendPurgeLog — 기록 1행을 append한다. **best-effort**: 실패는 slog.Warn 한 줄이고
// 퍼지 결과·종료 코드에 영향이 없다(appendDrop과 동형). 반환값이 없는 이유는 호출자가
// 판정할 것이 없기 때문이다.
func AppendPurgeLog(projectDir string, rec PurgeRecord) {
	cutoff := "-"
	if rec.Cutoff != 0 {
		cutoff = strconv.FormatInt(rec.Cutoff, 10)
	}
	line := strings.Join([]string{
		strconv.FormatInt(time.Now().Unix(), 10),
		sanPurgeField(rec.Path),
		sanPurgeField(rec.Policy),
		cutoff,
		sanPurgeField(rec.Status),
		strconv.FormatInt(rec.Count, 10),
		optInt64(rec.Bytes),
		optInt(rec.Deferred),
		optInt(rec.Failed),
	}, "\t") + "\n"

	f, err := os.OpenFile(filepath.Join(projectDir, PurgeLogName),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		slog.Warn("퍼지 기록 실패", "stage", "open", "path", rec.Path)
		return
	}
	// **닫기 실패도 보고한다**(검토 소견 F13). Windows에서는 지연된 쓰기 실패가 Close에서
	// 드러나므로, 그것을 버리면 **파일에 닿지 못한 append가 경고 없이 사라진다** — best-effort
	// 계약은 "퍼지 결과에 영향이 없다"이지 "실패를 숨긴다"가 아니다. 쓰기가 이미 실패했으면
	// 경고를 두 번 내지 않는다(호출당 한 줄).
	wrote := true
	if _, err := fmt.Fprint(f, line); err != nil {
		slog.Warn("퍼지 기록 실패", "stage", "write", "path", rec.Path)
		wrote = false
	}
	if cerr := f.Close(); cerr != nil && wrote {
		slog.Warn("퍼지 기록 실패", "stage", "close", "path", rec.Path)
	}
}
```

- [ ] **Step 4: 통과를 확인한다**

Run: `go test ./internal/store/... -count=1 -p 1 -run 'PurgeLog|PurgePolicy' -v`
Expected: 여섯 테스트 전부 PASS

- [ ] **Step 5: 게이트 다섯을 돌린다**

```
go build ./...
go vet ./...
go test ./... -count=1 -p 1
gofumpt -l .
golangci-lint run
```
Expected: build/vet 0 · gofumpt 무출력 · lint 0 issues · 테스트 12/13 패키지(`internal/exec` 넷은 환경 조건)

- [ ] **Step 6: 커밋**

```bash
git add internal/store/purgelog.go internal/store/purgelog_test.go
git commit -m "feat(store): 퍼지 감사 로그 — 9필드 사이드카 형식과 쓰기"
```

---

### Task 2: `internal/cli` — 파서

**Files:**
- Create: `internal/cli/purgelog.go`
- Test: `internal/cli/purgelog_test.go`

**Interfaces:**
- Consumes: `store.PurgeLogName`(Task 1) — 미러 상수로 값을 맞춘다.
- Produces: Task 5(`doctor [21]`)가 쓴다 —
  - `purgeEntry` 구조체(필드: `TS, Cutoff, Count int64` · `Path, Policy, Status string` · `Bytes, Deferred, Failed string` — 뒤 셋은 `-`를 그대로 나르는 표시용 문자열)
  - `purgeLogTail(path string, n int) (entries []purgeEntry, total int, unparsed int)`

- [ ] **Step 1: 실패하는 테스트를 쓴다**

`internal/cli/purgelog_test.go`:

```go
package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePurgeLog(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), store.PurgeLogName)
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestPurgeLogTailParses — 정상 9필드 줄을 읽고 꼬리 n건을 최신 순으로 낸다.
func TestPurgeLogTailParses(t *testing.T) {
	p := writePurgeLog(t,
		"1755180000\tstartup-shadow\t336h0m0s/pwsh-profile\t1753971299\tok\t0\t0\t0\t0",
		"1755180899\tcli-older-than\t720h0m0s/-\t-\tok\t12\t-\t-\t-",
	)
	entries, total, unparsed := purgeLogTail(p, 5)
	if total != 2 || unparsed != 0 || len(entries) != 2 {
		t.Fatalf("total=%d unparsed=%d entries=%d want 2/0/2", total, unparsed, len(entries))
	}
	// 최신이 먼저다 — doctor가 "최근 N건"으로 읽는다.
	if entries[0].Path != "cli-older-than" || entries[0].Count != 12 {
		t.Errorf("entries[0]=%+v want cli-older-than/12", entries[0])
	}
	if entries[0].Bytes != "-" || entries[0].Deferred != "-" || entries[0].Failed != "-" {
		t.Errorf("미측정 칸이 %q/%q/%q — `-`를 그대로 날라야 한다(0으로 바꾸면 하지 않은 측정이 된다)",
			entries[0].Bytes, entries[0].Deferred, entries[0].Failed)
	}
	if entries[1].Cutoff != 1753971299 {
		t.Errorf("cutoff=%d want 1753971299", entries[1].Cutoff)
	}
}

// TestPurgeLogTailAcceptsWiderRows — **9필드 이상은 수용하고 10번째부터 무시한다**.
// 스펙 §2.0이 "필요해지면 그때 열을 늘린다"를 열어 뒀고 로그는 append-only라, 정확
// 매칭으로 잠그면 확장 순간 옛 파서가 새 줄을 전부 버린다. 이 단정이 없으면 다음 사람이
// "정확히 9필드"로 되조인다.
func TestPurgeLogTailAcceptsWiderRows(t *testing.T) {
	p := writePurgeLog(t,
		"1755180000\tcli-older-than\t720h0m0s/-\t-\tok\t12\t-\t-\t-\t999",
	)
	entries, _, unparsed := purgeLogTail(p, 5)
	if unparsed != 0 || len(entries) != 1 {
		t.Fatalf("10필드 줄이 unparsed=%d entries=%d — 수용해야 한다", unparsed, len(entries))
	}
	if entries[0].Count != 12 {
		t.Errorf("count=%d want 12 — 앞 9칸의 의미가 유지돼야 한다", entries[0].Count)
	}
}

// TestPurgeLogTailUnparsed — 9필드보다 짧은 줄·비숫자 ts는 unparsed로 세고 **진단을
// 중단하지 않는다**(dropsByReason 관례). total은 빈 줄 포함 줄 수 계약이다.
func TestPurgeLogTailUnparsed(t *testing.T) {
	p := writePurgeLog(t,
		"1755180000\tstartup-shadow\t336h0m0s/-\t-\tok\t0\t0\t0\t0",
		"짧다\t줄",
		"",
		"notanumber\tcli-gc\t-\t-\tok\t1\t-\t-\t-",
	)
	entries, total, unparsed := purgeLogTail(p, 5)
	if total != 4 {
		t.Errorf("total=%d want 4 — 빈 줄도 센다(줄 수 계약)", total)
	}
	if unparsed != 3 {
		t.Errorf("unparsed=%d want 3", unparsed)
	}
	if len(entries) != 1 {
		t.Errorf("entries=%d want 1 — 정상 줄만 나온다", len(entries))
	}
}

// TestPurgeLogTailAbsent — 파일 부재는 (nil, 0, 0)이고 오류가 아니다.
// **"삭제가 없었다"와 구분하는 것은 doctor의 문면 몫이다**(Task 5).
func TestPurgeLogTailAbsent(t *testing.T) {
	entries, total, unparsed := purgeLogTail(filepath.Join(t.TempDir(), "nope.log"), 5)
	if entries != nil || total != 0 || unparsed != 0 {
		t.Fatalf("부재 = %v/%d/%d want nil/0/0", entries, total, unparsed)
	}
}

// TestPurgeLogTailLimit — n건만 낸다. 꼬리에서 자른다.
func TestPurgeLogTailLimit(t *testing.T) {
	p := writePurgeLog(t,
		"1755180001\tcli-gc\t-\t-\tok\t1\t-\t-\t-",
		"1755180002\tcli-gc\t-\t-\tok\t2\t-\t-\t-",
		"1755180003\tcli-gc\t-\t-\tok\t3\t-\t-\t-",
	)
	entries, total, _ := purgeLogTail(p, 2)
	if total != 3 || len(entries) != 2 {
		t.Fatalf("total=%d entries=%d want 3/2", total, len(entries))
	}
	if entries[0].Count != 3 || entries[1].Count != 2 {
		t.Errorf("꼬리 2건이 %d,%d want 3,2", entries[0].Count, entries[1].Count)
	}
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/cli/... -count=1 -p 1 -run 'PurgeLogTail'`
Expected: 컴파일 실패 — `undefined: purgeLogTail`, `undefined: store.PurgeLogName`

- [ ] **Step 3: 최소 구현**

`internal/cli/purgelog.go`:

```go
// 퍼지 감사 로그의 읽는 쪽. 쓰는 쪽은 internal/store가 소유한다 — dropsByReason이
// internal/hook의 appendDrop을 읽는 그 분리를 그대로 잇는다.
package cli

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// purgeLogFields — 형식이 고정한 필드 수. 파서는 이 수 **이상**을 수용한다(아래).
// 파일명은 store.PurgeLogName을 그대로 쓴다 — 미러 상수를 두지 않는다(검토 소견 F8).
const purgeLogFields = 9

// purgePathSet / purgeStatusSet — 스펙 §2.1·§2.2의 닫힌 집합. **형식 검증의 주력이다**
// (검토 소견 F3): 필드 수만 보면 합성 행이 통과한다.
var (
	purgePathSet = map[string]bool{
		"startup-shadow": true, "cli-hook-only": true, "cli-older-than": true, "cli-gc": true,
	}
	purgeStatusSet = map[string]bool{
		"failed": true, "partial": true, "budget": true,
		"cancelled": true, "capped": true, "ok": true,
	}
)

// isDashOrInt — `-`(미측정)이거나 10진 정수인가.
func isDashOrInt(s string) bool {
	if s == "-" {
		return true
	}
	_, err := strconv.ParseInt(s, 10, 64)
	return err == nil
}

// purgeEntry — 로그 한 줄. 뒤 세 칸이 문자열인 이유는 `-`(미측정)를 그대로 날라야 하기
// 때문이다 — int로 받으면 "못 잰 것"과 "재서 0"이 같은 값이 된다.
type purgeEntry struct {
	TS                       int64
	Path, Policy, Status     string
	Cutoff                   int64
	Count                    int64
	Bytes, Deferred, Failed  string
}

// purgeLogTail — 꼬리 n건을 **최신 순**으로 낸다. total은 빈 줄 포함 줄 수 계약이고
// unparsed는 형식에 안 맞는 줄 수다(dropsByReason 관례 — 진단은 절대 중단하지 않는다).
//
// **9필드 이상을 수용하고 10번째부터 무시한다.** 스펙 §2.0이 열어 둔 확장이 append-only
// 파일에서 무해하려면 옛 파서가 새 줄을 버리지 않아야 한다. dropsByReason이 `== 2 || == 5`
// 인 것과 어긋나지 않는다 — 그쪽이 막는 것은 구형/신형 혼재 사이의 **중간 필드 수**이고,
// 여기서 잘린 append는 9필드보다 **짧게** 나오므로 이 조건이 이미 거른다.
//
// 파일 부재·읽기 실패는 (nil, 0, 0) — dropsByReason과 동일 fail-soft다.
func purgeLogTail(path string, n int) (entries []purgeEntry, total int, unparsed int) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20) // 긴 줄에도 스캔 중단 방지(dropsByReason 관례)
	var all []purgeEntry
	for sc.Scan() {
		total++
		fields := strings.Split(sc.Text(), "\t")
		if len(fields) < purgeLogFields {
			unparsed++
			continue
		}
		ts, tsErr := strconv.ParseInt(fields[0], 10, 64)
		count, cErr := strconv.ParseInt(fields[5], 10, 64)
		// **필드 수만 보지 않는다**(검토 소견 F3). append-only 파일에서 찢어진 조각 뒤에는 다음
		// writer의 레코드가 이어붙고, 절단점이 필드 경계에 떨어지면 합쳐진 줄이 **정확히 9필드**가
		// 된다 — ts·path·count만 보면 그 합성 행이 통과해 doctor가 **일어나지 않은 삭제**를
		// 보고한다. 닫힌 집합 둘과 나머지 칸의 형식을 함께 요구해 그 창을 좁힌다.
		if tsErr != nil || cErr != nil ||
			!purgePathSet[fields[1]] || !purgeStatusSet[fields[4]] ||
			!isDashOrInt(fields[3]) || !isDashOrInt(fields[6]) ||
			!isDashOrInt(fields[7]) || !isDashOrInt(fields[8]) {
			unparsed++
			continue
		}
		cutoff, _ := strconv.ParseInt(fields[3], 10, 64) // "-"는 0 — 경계 개념 없음과 같은 표시다
		all = append(all, purgeEntry{
			TS: ts, Path: fields[1], Policy: fields[2], Cutoff: cutoff,
			Status: fields[4], Count: count,
			Bytes: fields[6], Deferred: fields[7], Failed: fields[8],
		})
	}
	// **스캔 중단을 침묵으로 넘기지 않는다**(검토 소견 F11). 1 MiB를 넘는 줄이 있으면 루프가
	// 조용히 멈춰 total이 과소 계상되고 "최근 N건"이 잘린 앞부분을 최신으로 보고한다 — 이 절이
	// 닫으려는 "침묵을 초록으로 읽는" 부류 그대로다.
	if sc.Err() != nil {
		unparsed++
	}
	for i := len(all) - 1; i >= 0 && len(entries) < n; i-- {
		entries = append(entries, all[i])
	}
	return entries, total, unparsed
}
```

- [ ] **Step 4: 합성 행 거부 테스트를 더한다 — 검토 소견 F3 의 잠금**

`internal/cli/purgelog_test.go` 끝에:

```go
// TestPurgeLogTailRejectsSplicedLine — **이 테스트가 형식 검증의 존재 이유다**(검토 소견 F3).
// append-only 파일에서 찢어진 조각 뒤에 다음 레코드가 이어붙으면 합쳐진 줄이 정확히 9필드가
// 될 수 있고, 필드 수와 ts·path·count만 보는 파서는 그것을 수용한다 — doctor가 **일어나지
// 않은 삭제 사건**을 보고하게 되고, 그것이 이 로그가 막으려던 바로 그것이다.
//
// 아래 픽스처는 앞 레코드가 policy 중간에서 잘리고 뒤 레코드의 앞부분이 붙은 모양이다:
// 필드 수는 9이고 ts도 숫자이며 path도 닫힌 집합에 있지만, status 칸에 타임스탬프 조각이 온다.
func TestPurgeLogTailRejectsSplicedLine(t *testing.T) {
	p := writePurgeLog(t,
		"1755180000\tstartup-shadow\t336h0m0\t-\t1755180899\tcli-gc\t-\t-\tok",
	)
	entries, total, unparsed := purgeLogTail(p, 5)
	if total != 1 || unparsed != 1 || len(entries) != 0 {
		t.Fatalf("합성 행이 수용됐다 — total=%d unparsed=%d entries=%d want 1/1/0",
			total, unparsed, len(entries))
	}
}

// TestPurgeLogTailRejectsBadFieldShapes — 닫힌 집합 밖 값과 정수도 `-`도 아닌 칸을 거른다.
// 각 줄이 한 축만 어긋난다 — 어느 검사가 잡았는지 실패 메시지로 갈린다.
func TestPurgeLogTailRejectsBadFieldShapes(t *testing.T) {
	for name, line := range map[string]string{
		"path 밖":   "1755180000\tunknown-path\t-\t-\tok\t1\t-\t-\t-",
		"status 밖": "1755180000\tcli-gc\t-\t-\tdone\t1\t-\t-\t-",
		"bytes 비정수": "1755180000\tcli-gc\t-\t-\tok\t1\tmany\t-\t-",
		"cutoff 비정수": "1755180000\tcli-gc\t-\tsoon\tok\t1\t-\t-\t-",
	} {
		t.Run(name, func(t *testing.T) {
			entries, _, unparsed := purgeLogTail(writePurgeLog(t, line), 5)
			if unparsed != 1 || len(entries) != 0 {
				t.Fatalf("unparsed=%d entries=%d want 1/0", unparsed, len(entries))
			}
		})
	}
}
```

`writePurgeLog` 헬퍼는 파일명에 **`store.PurgeLogName` 을 쓴다** — 미러 상수는 만들지 않는다.
테스트 파일의 import 에 `"github.com/wotjr1649/context-router/internal/store"` 를 더한다.

- [ ] **Step 5: 통과를 확인한다**

Run: `go test ./internal/cli/... -count=1 -p 1 -run 'PurgeLog' -v`
Expected: 여섯 테스트 전부 PASS

- [ ] **Step 6: 게이트 다섯 + 커밋**

```bash
git add internal/cli/purgelog.go internal/cli/purgelog_test.go
git commit -m "feat(cli): 퍼지 감사 로그 파서 — 9필드 이상 수용, 미측정은 그대로 나른다"
```

---

### Task 3: 기동 퍼지 배선 — **0건 기동도 한 행을 남긴다**

**Files:**
- Modify: `cmd/context-router/main.go` — 기동 퍼지 고루틴의 결과 분류 switch
- Test: `cmd/context-router/main_test.go`

**Interfaces:**
- Consumes: `store.AppendPurgeLog`, `store.PurgeRecord`, `store.PurgePolicy`(Task 1)
- Produces: 없음(배선이다).

**요점**: 지금 그 switch 는 **다섯 분기**이고 `rep.Hashes == 0` 이면서 오류도 없는 정상 기동은 **어느 분기에도 안 걸린다** — 아무것도 안 한다. 소유자 결정 2가 *"0건 기동도 1행"* 인 이유가 그것이고, **그 분기를 위한 자리를 만드는 것이 이 태스크의 요점이다.** *"안 돌았다"* 와 *"돌았는데 0건"* 이 갈리는 지점이 여기다.

- [ ] **Step 0: 기존 e2e 헬퍼를 찾는다 — 그리고 종료 방식을 확인한다**

이 계획은 헬퍼 이름을 **지어내지 않는다.** `cmd/context-router/main_test.go` 에서 서버를 띄웠다 내리는 기존 테스트를 하나 찾아 그 헬퍼 이름과 시그니처를 그대로 쓴다. 아래 테스트 코드의 `newTestServerRoot` / `runServerOnce` 는 **자리표시가 아니라 "그 역할을 하는 기존 헬퍼로 바꿔 쓰라"는 지시**다. 없으면 그 파일의 가장 가까운 기존 테스트를 복제해 만든다.

**★ 그 헬퍼가 서버를 *어떻게* 내리는지 반드시 확인한다**(검토 소견 F16). 기동 행은 `run()` 의 defer 가 기다리는 **퍼지 고루틴**이 쓰고, 이 패키지의 e2e 는 빌드된 바이너리를 **하위 프로세스로** 돌린다. 헬퍼가 stdin 을 닫는 **정상 종료** 대신 프로세스를 kill 하면 그 defer 가 안 돌아 **두 새 테스트가 배선과 무관한 이유로 실패한다.** kill 하는 헬퍼밖에 없으면 정상 종료 경로를 쓰는 변형을 만든다.

- [ ] **Step 1: 분류 표 테스트와 e2e 테스트를 쓴다**

**분류 우선순위 표 테스트(스펙 §6 항목 6)는 Task 1 이 든다** — 그 함수가 `internal/store` 에 살기 때문이다. 여기서는 e2e 만 쓴다. **Step 0 에서 찾은 헬퍼 이름으로 바꿔 쓴다.**

```go
// TestStartupPurgeLogsZeroCount — 삭제 0인 기동이 **행 하나를 남기는가**.
// 지금 코드에서 그 분기는 아무것도 하지 않는다 — 이 테스트가 그 침묵을 잠근다.
// "퍼지가 안 돌았다"와 "돌았는데 0건"이 갈리지 않으면, 200개가 사라진 그 기동을
// 사후에 재구성할 수단이 다시 없어진다.
func TestStartupPurgeLogsZeroCount(t *testing.T) {
	// (기존 e2e 헬퍼로 storeRoot·프로젝트를 만들고 서버를 한 번 띄웠다 내린다.
	//  CTR_SHADOW_RETENTION을 336h로 두어 지울 대상이 없게 한다.)
	storeRoot, projDir := newTestServerRoot(t) // 헬퍼 이름은 기존 파일의 것을 쓴다
	t.Setenv("CTR_SHADOW_RETENTION", "336h")
	t.Setenv("CTR_RETENTION_SOURCE", "test")
	runServerOnce(t, storeRoot) // 기동 → 즉시 종료

	data, err := os.ReadFile(filepath.Join(projDir, store.PurgeLogName))
	if err != nil {
		t.Fatalf("0건 기동이 행을 안 남겼다: %v", err)
	}
	f := strings.Split(strings.TrimRight(string(data), "\n"), "\t")
	if len(f) < 9 {
		t.Fatalf("필드 %d개 want >=9: %q", len(f), data)
	}
	if f[1] != "startup-shadow" {
		t.Errorf("path=%q want startup-shadow", f[1])
	}
	if f[4] != "ok" {
		t.Errorf("status=%q want ok", f[4])
	}
	if f[5] != "0" {
		t.Errorf("count=%q want 0", f[5])
	}
	// **회귀 잠금의 핵심**: 실효 정책값이 행에 박혀 있는가.
	if !strings.HasPrefix(f[2], "336h0m0s/") {
		t.Errorf("policy=%q want 336h0m0s/ 접두 — 이 칸이 이 설계의 존재 이유다", f[2])
	}
}

// TestStartupPurgeLogsSilentDowngrade — **이 테스트가 이 설계의 존재 이유다**(스펙 §6 항목 4).
// `14d`는 ParseDuration에 `d`가 없어 조용히 기본 72h로 떨어진다 — 세션 58~62가 세 세션 걸려
// 찾은 함정이다. 그 상태로 돈 기동의 행에 `72h0m0s`가 적혀 있어야 하고, **환경변수 원문이
// 아니라 store.ShadowRetention의 실효 반환값**이어야 한다. 200개가 사라진 그 기동의 행에
// `72h0m0s/-`가 있었다면 그 규명은 세 세션이 아니라 한 줄 조회였다.
func TestStartupPurgeLogsSilentDowngrade(t *testing.T) {
	storeRoot, projDir := newTestServerRoot(t) // Step 0에서 찾은 헬퍼로 바꿔 쓴다
	t.Setenv("CTR_SHADOW_RETENTION", "14d")    // ParseDuration이 못 읽는다 → 실효 72h
	t.Setenv("CTR_RETENTION_SOURCE", "")       // 출처 없음 → `-`
	runServerOnce(t, storeRoot)

	data, err := os.ReadFile(filepath.Join(projDir, store.PurgeLogName))
	if err != nil {
		t.Fatalf("기동이 행을 안 남겼다: %v", err)
	}
	f := strings.Split(strings.TrimRight(string(data), "\n"), "\t")
	if f[2] != "72h0m0s/-" {
		t.Fatalf("policy=%q want %q — 환경변수 원문(`14d`)이 아니라 **실효 반환값**을 적어야 한다. "+
			"이 칸이 아니면 조용한 72h 강등을 사후에 알 방법이 없다", f[2], "72h0m0s/-")
	}
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./cmd/context-router/... -count=1 -p 1 -run 'StartupPurgeLogsZeroCount'`
Expected: FAIL — `0건 기동이 행을 안 남겼다: ... no such file`

- [ ] **Step 3: switch 에 0건 분기를 더하고 모든 분기에서 기록한다**

`cmd/context-router/main.go` 의 기동 퍼지 고루틴. **기존 다섯 분기의 slog 호출과 주석은 한 줄도 지우지 않는다** — 분류 근거가 거기 있고, 로그와 이 기록이 갈리면 안 된다.

각 분기 끝에 `status` 를 정하고, switch **뒤에** 한 번 기록한다:

```go
		// **분류는 startupPurgeStatus 하나가 정한다** — slog 분기와 기록이 각자 판정하면 둘이
		// 갈린다. 아래 switch는 이제 **문면만** 고른다. 기존 case 순서·주석·slog 인자는 한 줄도
		// 바꾸지 않는다(분류 근거가 거기 있다).
		capped := rep.Hashes == startupPurgeMaxHashes
		cancelled := errors.Is(purgeCtxErr, context.Canceled)
		purgeStatus := store.PurgeStatus(purgeErr, rep.Hashes, cancelled, budgetSpent, capped)
		switch {
		case purgeErr != nil && rep.Hashes > 0:
			// (기존 주석 전부 그대로)
			slog.Warn("기동 shadow 회수 중단 — 행 삭제 완료, 파일 회수 잔여분은 purge --gc",
				"hashes", rep.Hashes, "bytes", rep.ReclaimedB, "deferred", rep.DeferredFiles,
				"capped", capped, "budget_spent", budgetSpent)
		case purgeErr != nil && cancelled:
			slog.Debug("기동 shadow 회수 중단 — 서버 종료", "error", purgeErr)
		case purgeErr != nil && budgetSpent:
			slog.Warn("기동 shadow 회수 예산 소진 — 행 삭제 미완료, 다음 기동에서 재시도")
		case purgeErr != nil:
			slog.Warn("기동 shadow 회수 실패 — 다음 기동에서 재시도", "error", purgeErr)
		case rep.Hashes > 0:
			slog.Info("기동 shadow 회수", "hashes", rep.Hashes, "bytes", rep.ReclaimedB,
				"deferred", rep.DeferredFiles, "capped", capped, "budget_spent", budgetSpent)
			// **0건 기동은 slog를 내지 않는다** — 정상 기동마다 INFO를 내면 소음이다. 그런데
			// 지금까지는 **아무 자국도** 안 남았고, 그래서 "퍼지가 안 돌았다"와 "돌았는데 지울
			// 것이 없었다"가 구별되지 않았다. 아래 기록이 그 구멍을 닫는다.
		}
		reclaimed, deferred, failed := rep.ReclaimedB, rep.DeferredFiles, rep.FailedFiles
		store.AppendPurgeLog(st.ProjectDir(), store.PurgeRecord{
			Path:   "startup-shadow",
			Policy: store.PurgePolicy(retention, os.Getenv("CTR_RETENTION_SOURCE")),
			Status: purgeStatus,
			Cutoff: cutoff,
			Count:  int64(rep.Hashes), // HookPurgeReport.Hashes는 int, PurgeRecord.Count는 int64 (검토 소견 F6)
			Bytes:  &reclaimed, Deferred: &deferred, Failed: &failed,
		})
```

분류 함수는 **Task 1 이 `internal/store` 에 만든 `store.PurgeStatus` 다**(검토 소견 F4 — CLI 도 같은 것을 써야 해서 `cmd/context-router` 에 두면 손이 닿지 않는다). 여기서는 그것을 부르기만 한다. 함수 본문은 참고로 싣는다:

```go
// PurgeStatus — 퍼지 결과 분류(스펙 §2.2의 닫힌 집합). **우선순위가 계약이다**:
// partial이 budget·cancelled를 이긴다 — rep.Hashes>0이면 행 삭제 tx가 이미 커밋됐고
// (store.purgeHookRows는 실패 시 항상 빈 리포트를 반환하므로 그 값 자체가 커밋의 증거다)
// 행이 없어진 배치는 다음 기동 술어에 다시 안 잡히므로, 중단 사유가 무엇이든 남은 파일의
// 유일한 회수 경로가 purge --gc다. 순서가 뒤집히면 "다음 기동이 다시 집는다"는 **거짓
// 안내**가 나간다 — 그 판단은 위 switch가 이미 내렸고, 이 함수는 그것을 옮겨 담을 뿐이다.
//
// **마지막 default가 0건 기동이다.** 그 자리가 이 설계의 요점이다.
func PurgeStatus(purgeErr error, hashes int, cancelled, budgetSpent, capped bool) string {
	switch {
	case purgeErr != nil && hashes > 0:
		return "partial"
	case purgeErr != nil && cancelled:
		return "cancelled"
	case purgeErr != nil && budgetSpent:
		return "budget"
	case purgeErr != nil:
		return "failed"
	case capped:
		return "capped"
	default:
		return "ok"
	}
}
```

`hashes` 의 타입은 `HookPurgeReport.Hashes` 에 맞춘다 — 그 필드가 `int` 가 아니면 시그니처를 그 타입으로 바꾼다.

- [ ] **Step 3b: `cutoff <= 0` 조기 반환 경로에도 행을 쓴다**

같은 고루틴 앞부분에 `if cutoff <= 0 { … return }` 가드가 있다 — 보존값이 과도하게 크면 회수 자체를 건너뛴다. **거기서 그냥 return 하면 행이 하나도 안 남고**, 그것은 이 설계가 닫으려는 *"아무 자국도 없음"* 을 **같은 오설정 부류**가 다시 만드는 것이다(검토 소견 F5 — `14d` 가 조용히 72h 가 되는 함정의 거울상). 기존 주석과 `return` 은 그대로 두고 기록만 앞에 넣는다:

```go
		if cutoff <= 0 {
			// (기존 주석 전부 그대로)
			store.AppendPurgeLog(st.ProjectDir(), store.PurgeRecord{
				Path:   "startup-shadow",
				Policy: store.PurgePolicy(retention, os.Getenv("CTR_RETENTION_SOURCE")),
				Status: "ok",
				Cutoff: 0, // 경계를 세우지 않았다 → "-"
				Count:  0,
				// 뒤 세 칸은 nil — 회수를 **아예 안 돌렸다**. 0으로 쓰면 "쟀더니 0"이라는
				// 거짓 측정이 된다(스펙 §3의 포인터 규칙).
			})
			return
		}
```

**주의 셋:**
- `retention` 은 이 고루틴이 `cutoff` 를 구할 때 이미 쓴 `store.ShadowRetention(os.Getenv)` 의 반환값이다. **환경변수 원문이 아니라 그 실효 반환값을 넘긴다** — `14d` 처럼 `ParseDuration` 에 없는 접미로 조용히 72h 가 된 경우가 **첫 줄에서 드러나는** 지점이 여기다. 그 값이 지역 변수로 안 남아 있으면 한 번 더 부르지 말고 **`cutoff` 계산부에서 변수로 받아 내려보낸다.**
- `st.ProjectDir()` 는 **Task 1 이 이미 만들었다**(그 태스크의 Produces). `[실측]` 그 접근자가 원래 없었고 `s.dir` 이 프로젝트 디렉터리가 맞다 — `store.Open` 이 모든 자리에서 `filepath.Join(storeRoot, "projects", id)` 로 불린다.
- `rep` 의 세 필드를 지역 변수로 받아 주소를 넘긴다. 구조체 필드 주소를 직접 넘기면 `rep` 의 수명이 기록에 묶인다.

- [ ] **Step 4: 통과를 확인한다**

Run: `go test ./cmd/context-router/... -count=1 -p 1 -run 'StartupPurge' -v`
Expected: PASS

- [ ] **Step 5: 기존 기동 테스트가 안 깨졌는지 본다**

Run: `go test ./cmd/context-router/... -count=1 -p 1`
Expected: 전부 PASS — 이 태스크는 slog 문면을 한 줄도 안 바꿨다

- [ ] **Step 6: 게이트 다섯 + 커밋**

```bash
git add cmd/context-router/main.go cmd/context-router/main_test.go
git commit -m "feat(startup): 기동 퍼지가 0건에도 감사 행을 남긴다"
```

---

### Task 4: CLI 세 경로 배선

**Files:**
- Modify: `internal/cli/cli.go` — `runPurgeHookOnly` · `runPurge` 의 `PurgeOlderThan` 직후 · 같은 함수의 `GCOrphanBlobs` 직후
- Test: `internal/cli/cli_test.go`

**Interfaces:**
- Consumes: `store.AppendPurgeLog`, `store.PurgeRecord`, `store.PurgePolicy`(Task 1) · `purgeLogTail`(Task 2, 테스트에서 검증용)
- Produces: 없음.

**요점 둘:**
- `runPurge` 는 지금 **반환값을 버린다** — `_, _, purgeErr := st.PurgeOlderThan(...)` 과 `_, purgeErr = st.GCOrphanBlobs(...)`. 그 값이 `count` 다. 받아야 한다.
- **★ `runPurgeHookOnly` 는 `st.PurgeHookOnly(ctx)` 를 부른다** `[실측]` — 기동 경로의 `PurgeHookOnlyOlderThan` 이 **아니고 경계 인자를 받지 않는다.** 사용자가 *"hook 귀속을 전부 지운다"* 를 명시로 요청한 자리라 **보존 창도 경계도 개입하지 않는다.** 그 두 칸에 `336h0m0s/…` 를 적으면 **일어나지 않은 정책 판정을 기록하는 것**이고, 이 로그가 막으려던 바로 그 종류의 거짓 기록이다. (스펙 초안이 둘을 한 함수로 적었고 이 계획을 쓰며 정정했다 — 스펙 §2.0 의 ★ 항목.)

**경로별 채우는 칸**(스펙 §2.0 의미표 그대로. **이 표를 어기면 읽는 쪽이 빈칸을 "0바이트 회수"로 읽는다**):

| 필드 | `cli-hook-only` | `cli-older-than` | `cli-gc` |
|---|---|---|---|
| `Policy` | **`"-"`** — 보존 창을 안 본다 | `PurgePolicy(사용자가 준 기간, "")` → `720h0m0s/-` | `"-"` |
| `Cutoff` | **`0`** → `-` — 경계 인자가 없다 | 사용자가 지정한 경계 | `0` → `-` |
| `Count` | `rep.Hashes` | `artifacts`(둘째 반환값) | `removed` |
| `Bytes` | `&rep.ReclaimedB` | `nil` | `nil` |
| `Deferred`/`Failed` | `&rep.DeferredFiles` / `&rep.FailedFiles` | `nil` | `nil` |

**★ 회귀 잠금(조용한 72h 강등)은 이 태스크가 아니라 Task 3 이 든다** — `policy` 가 실질을 갖는 경로는 `startup-shadow` 하나뿐이고, 그것이 바로 200개가 사라진 경로다.

- [ ] **Step 1: 실패하는 테스트를 쓴다**

`internal/cli/cli_test.go` 에 더한다. **기존 purge e2e 헬퍼의 형태를 따른다.**

```go
// TestRunPurgeLogsOlderThan — --older-than 경로가 행을 남기고, **미측정 칸이 0이 아니라
// `-`로 나오는가**. PurgeOlderThan은 바이트도 유예도 실패도 내지 않는다(스펙 §2.0) —
// 0으로 새면 읽는 쪽이 "회수 바이트가 0이었다"는 하지 않은 측정을 읽는다.
func TestRunPurgeLogsOlderThan(t *testing.T) {
	// (기존 헬퍼로 storeRoot·프로젝트·content.db를 만들고 --older-than --force로 돌린다)
	storeRoot, projDir, projectID := newPurgeFixture(t) // 헬퍼 이름은 기존 파일의 것을 쓴다
	var out, errBuf bytes.Buffer
	if err := runPurge(context.Background(), strings.NewReader(""), &out, &errBuf,
		storeRoot, []string{"--project", projectID, "--older-than", "720h", "--force"}, false); err != nil {
		t.Fatalf("runPurge: %v", err)
	}
	entries, total, unparsed := purgeLogTail(filepath.Join(projDir, store.PurgeLogName), 5)
	if total != 1 || unparsed != 0 || len(entries) != 1 {
		t.Fatalf("total=%d unparsed=%d entries=%d want 1/0/1", total, unparsed, len(entries))
	}
	e := entries[0]
	if e.Path != "cli-older-than" {
		t.Errorf("path=%q want cli-older-than", e.Path)
	}
	if e.Bytes != "-" || e.Deferred != "-" || e.Failed != "-" {
		t.Errorf("미측정 칸이 %q/%q/%q want -/-/- — 이 경로는 그 셋을 재지 않는다",
			e.Bytes, e.Deferred, e.Failed)
	}
	// **전체 값을 단정한다**(검토 소견 F7). 접미만 보면 원시 플래그 문자열("720h")을 그대로
	// 실어도 통과해 §2.3의 정규화 형식(Duration.String())이 깨진 것을 못 잡는다.
	if e.Policy != "720h0m0s/-" {
		t.Errorf("policy=%q want %q — 원시 플래그가 아니라 파싱된 기간의 String()이다",
			e.Policy, "720h0m0s/-")
	}
}

// TestRunPurgeLogsGC — --gc 경로. cutoff 개념이 없고 count는 제거된 고아 blob 파일 수다.
// **이 경로가 세 축 정합(blob − 고아 = hashes)이 깨지는 유일한 자리**라, 그 등식이 깨진 채
// 발견되면 이 행이 원인을 즉시 답한다(스펙 §2.0).
func TestRunPurgeLogsGC(t *testing.T) {
	storeRoot, projDir, projectID := newPurgeFixture(t)
	var out, errBuf bytes.Buffer
	if err := runPurge(context.Background(), strings.NewReader(""), &out, &errBuf,
		storeRoot, []string{"--project", projectID, "--gc", "--force"}, false); err != nil {
		t.Fatalf("runPurge --gc: %v", err)
	}
	entries, _, _ := purgeLogTail(filepath.Join(projDir, store.PurgeLogName), 5)
	if len(entries) != 1 || entries[0].Path != "cli-gc" {
		t.Fatalf("entries=%+v want 1건 cli-gc", entries)
	}
	if entries[0].Policy != "-" {
		t.Errorf("policy=%q want %q — GC에는 보존 정책 개념이 없다", entries[0].Policy, "-")
	}
}

// TestRunPurgeHookOnlyLogsNoPolicy — 이 경로는 **보존 창을 보지 않는다**(PurgeHookOnly는
// 경계 인자를 받지 않는 전량 삭제다). 그래서 policy·cutoff가 `-`여야 한다 — 값을 적으면
// **일어나지 않은 정책 판정**을 기록하는 것이고, 이 로그가 막으려던 바로 그 종류의 거짓
// 기록이다. 뒤 세 칸은 HookPurgeReport를 그대로 받으므로 채워진다.
func TestRunPurgeHookOnlyLogsNoPolicy(t *testing.T) {
	storeRoot, projDir, projectID := newPurgeFixture(t)
	t.Setenv("CTR_SHADOW_RETENTION", "336h") // 설정돼 있어도 이 경로는 안 본다
	t.Setenv("CTR_RETENTION_SOURCE", "pwsh-profile")
	var out, errBuf bytes.Buffer
	if err := runPurgeHookOnly(context.Background(), strings.NewReader(""), &out, &errBuf,
		storeRoot, projectID, true, false); err != nil {
		t.Fatalf("runPurgeHookOnly: %v", err)
	}
	entries, _, _ := purgeLogTail(filepath.Join(projDir, store.PurgeLogName), 5)
	if len(entries) != 1 {
		t.Fatalf("entries=%d want 1", len(entries))
	}
	e := entries[0]
	if e.Path != "cli-hook-only" {
		t.Errorf("path=%q want cli-hook-only", e.Path)
	}
	if e.Policy != "-" {
		t.Errorf("policy=%q want %q — 이 경로는 보존 창을 보지 않는다. 환경변수가 설정돼 있다는 "+
			"이유로 값을 적으면 일어나지 않은 판정을 기록하는 것이다", e.Policy, "-")
	}
	if e.Cutoff != 0 {
		t.Errorf("cutoff=%d want 0(`-`) — PurgeHookOnly는 경계 인자를 받지 않는다", e.Cutoff)
	}
	if e.Bytes == "-" || e.Deferred == "-" || e.Failed == "-" {
		t.Errorf("뒤 세 칸이 %q/%q/%q — 이 경로는 HookPurgeReport를 받으므로 실측값이 있다",
			e.Bytes, e.Deferred, e.Failed)
	}
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/cli/... -count=1 -p 1 -run 'RunPurgeLogs|RunPurgeHookOnlyLogsPolicy'`
Expected: FAIL — 로그 파일이 없어 `total=0`

- [ ] **Step 3: 네 호출 지점을 배선한다 — `cli-gc` 는 자리가 둘이다**

**★ 먼저 `--gc` 의 실제 경로를 확인한다**(검토 소견 F1). `--gc` 를 단독으로 주면 `gcOnly` 가 서서 **`runGCOrphan(ctx, projDir)`** 으로 가고, `--older-than` 과 함께면 선택 삭제 뒤에 이어 돈다. **한 자리만 배선하면 `purge --gc` 와 `--sessions --gc` 가 행을 하나도 안 남기고, 이 태스크의 `TestRunPurgeLogsGC`(`--project X --gc --force`)가 실패한다.** 가장 싼 해소는 **`runGCOrphan` 안에 한 번** 넣고 선택 분기가 그것을 거쳐 가게 하는 것이다 — 조립이 한 곳이면 갈리지 않는다(D13). 통합이 어려우면 두 자리에 넣되 같은 `PurgeRecord` 를 만드는 헬퍼를 공유한다.

**분류는 `store.PurgeStatus` 하나를 쓴다**(검토 소견 F4). *"CLI 는 두 갈래뿐"* 이라는 초안의 가정은 틀렸다:

- `runPurgeHookOnly` 의 `PurgeHookOnly` 는 **행 삭제 tx 가 커밋된 뒤** 파일 회수에서 실패할 수 있고(`rep.Hashes > 0` + `err != nil`), 그것이 **`partial`** 이다.
- `dispatchCLI` 의 ctx 는 `signal.NotifyContext(os.Interrupt)` 라 **Ctrl-C 가 `cancelled`** 를 낸다.

둘을 `failed` 로 뭉개면 §2.2 의 안내(*"다음 기동이 같은 배치를 다시 집는다"*)가 **거짓**이 된다 — 행이 이미 없으면 다음 기동은 그것을 안 집는다.

```go
cancelled := errors.Is(ctx.Err(), context.Canceled)
```

**`PurgeOlderThan` 직후** — 버려지던 둘째 반환값을 받는다. **`olderThan` 이라는 변수는 없다**(검토 소견 F7): 파싱된 기간은 `d` 이고 `if selective` 블록 안에 선언돼 있으므로 **밖으로 끌어올린다.**

```go
		_, artifacts, purgeErr := st.PurgeOlderThan(ctx, cutoffUnix)
		store.AppendPurgeLog(projDir, store.PurgeRecord{
			Path:   "cli-older-than",
			Policy: store.PurgePolicy(d, ""), // 사용자가 준 기간에는 출처가 없다
			Status: store.PurgeStatus(purgeErr, 0, cancelled, false, false),
			Cutoff: cutoffUnix,
			Count:  artifacts,
			// Bytes·Deferred·Failed는 nil — 이 함수가 그 셋을 내지 않는다(스펙 §2.0)
		})
```

**`GCOrphanBlobs` 직후 — 위에서 확인한 두 자리 모두:**

```go
			var removed int64
			removed, purgeErr = st.GCOrphanBlobs(ctx)
			store.AppendPurgeLog(projDir, store.PurgeRecord{
				Path:   "cli-gc",
				Policy: "-",
				Status: store.PurgeStatus(purgeErr, 0, cancelled, false, false),
				Count:  removed,
			})
```

`runPurgeHookOnly` — `st.PurgeHookOnly(ctx)` 직후. **앞 두 칸이 기동 경로와 다르다**:

```go
		rep, purgeErr := st.PurgeHookOnly(ctx)
		reclaimed, deferred, failed := rep.ReclaimedB, rep.DeferredFiles, rep.FailedFiles
		store.AppendPurgeLog(projDir, store.PurgeRecord{
			Path: "cli-hook-only",
			// 이 경로는 **보존 창을 보지 않는다** — PurgeHookOnly는 경계 인자를 받지 않는
			// 전량 삭제이고, 사용자가 그것을 명시로 요청한 자리다. 환경변수가 설정돼 있다는
			// 이유로 336h0m0s를 적으면 **일어나지 않은 정책 판정**을 기록하는 것이다.
			Policy: "-",
			Cutoff: 0, // 경계 개념 없음 → "-"
			// rep.Hashes를 넘겨 partial을 살린다 — 커밋 뒤 회수 실패가 failed로 뭉개지면
			// §2.2의 "다음 기동이 다시 집는다"가 거짓 안내가 된다(검토 소견 F4).
			Status: store.PurgeStatus(purgeErr, rep.Hashes, cancelled, false, false),
			Count:  int64(rep.Hashes), // HookPurgeReport.Hashes는 int, PurgeRecord.Count는 int64 (검토 소견 F6)
			Bytes:  &reclaimed, Deferred: &deferred, Failed: &failed,
		})
```

**CLI 전용 상태 헬퍼를 만들지 마라.** 초안은 `internal/cli` 에 `purgeStatusOf(err)` 를 두려 했는데, 그것이 바로 검토 소견 F4 가 잡은 두 벌이다 — `store.PurgeStatus` 하나를 네 자리가 함께 쓴다.

- [ ] **Step 4: 통과를 확인한다**

Run: `go test ./internal/cli/... -count=1 -p 1 -run 'RunPurge' -v`
Expected: 세 테스트 PASS + 기존 purge 테스트 전부 PASS

- [ ] **Step 5: 게이트 다섯 + 커밋**

```bash
git add internal/cli/cli.go internal/cli/purgelog.go internal/cli/cli_test.go
git commit -m "feat(cli): purge 세 경로가 감사 행을 남긴다 — 미측정과 0을 가른다"
```

---

### Task 5: `doctor [21]` — 그리고 부재를 "삭제 없음"으로 읽히지 않게

**Files:**
- Modify: `internal/cli/cli.go` — `runDoctor` 의 **끝**
- Test: `internal/cli/cli_test.go`

**Interfaces:**
- Consumes: `purgeLogTail`, `store.PurgeLogName`(Task 2)
- Produces: 없음(마지막 태스크다).

**요점 둘:**
- **절 번호는 끝에 `[21]` 로 붙인다.** 중간 삽입은 `[1]`~`[20]` 을 전부 밀어 `assertDoctorAscending` · 문서 · **append-only 핸드오프 레코드**의 번호 참조를 한꺼번에 깬다. 옛 레코드는 고칠 수 없으니 그 어긋남이 영구다.
- **파일 부재 문면이 "삭제가 없었다"로 읽히면 안 된다.** 이 절이 닫으려는 결함이 정확히 그 부류(침묵을 초록으로 읽는 것)다.

- [ ] **Step 0: `doctorOut` 을 storeRoot 지정 가능하게 넓힌다**

`doctorOut(t, projectRoot)` 는 storeRoot 를 **매번 새 `t.TempDir()`** 로 만들어 넘긴다 `[실측]`. 그래서 테스트가 그 경로를 알 수 없고, **`purge.log` 를 놓을 자리를 만들 수 없다.** 기존 호출부를 건드리지 않고 한 겹만 벗긴다:

```go
// doctorOutIn — doctorOut의 storeRoot 지정 판. purge.log처럼 storeRoot 아래에 픽스처를
// 놓아야 하는 테스트가 그 경로를 알아야 해서 갈랐다. doctorOut은 이 함수의 얇은 래퍼로
// 남는다 — 기존 호출부는 한 줄도 안 바뀐다.
func doctorOutIn(t *testing.T, storeRoot, projectRoot string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := runDoctor(context.Background(), &buf, storeRoot, projectRoot, "0.17.0")
	return buf.String(), err
}

func doctorOut(t *testing.T, projectRoot string) (string, error) {
	t.Helper()
	return doctorOutIn(t, t.TempDir(), projectRoot)
}
```

`purge.log` 를 놓을 정확한 경로는 `runDoctor` 이 쓰는 것과 같다: `filepath.Join(storeRoot, "projects", <projectID>)`. **프로젝트 ID 는 지어내지 말고** `runDoctor` 이 `canon.ProjectID` 를 얻는 그 경로를 테스트에서 같은 방식으로 구한다(`[2] project:` 줄이 그 값을 출력하므로, 먼저 `doctorOutIn` 을 한 번 돌려 그 줄에서 읽어도 된다).

- [ ] **Step 1: 실패하는 테스트를 쓴다**

```go
// TestDoctorPurgeSectionAbsent — 파일이 없을 때의 문면이 **"삭제 없음"으로 읽히지 않는가**.
// 이 절이 닫으려는 결함이 바로 그 부류다 — 침묵을 초록으로 읽는 것. 문면 그대로 단정한다.
func TestDoctorPurgeSectionAbsent(t *testing.T) {
	projectRoot := t.TempDir()
	out, _ := doctorOut(t, projectRoot)
	const want = "[21] purges: 기록 없음 (이 릴리스 이전 기동이거나 삭제 경로가 아직 안 돌았다)"
	if !strings.Contains(out, want) {
		t.Fatalf("[21] 부재 문면이 다르다 — want %q:\n%s", want, out)
	}
	// 부작용 축: 그 줄 **안에** "삭제 없음"으로 읽히는 어휘가 없어야 한다.
	// **접두를 붙여 Contains 하면 아무것도 못 잡는다**(검토 소견 F10) — 위 정확 문면 단정이
	// 성립하는 순간 "[21] purges: 삭제 없음"은 절대 매치하지 않으므로, 그 루프는 자기가
	// 주장하는 성질과 무관한 이유로 통과한다. 줄을 뽑아 그 안을 본다.
	var line string
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "[21] purges:") {
			line = ln
			break
		}
	}
	if line == "" {
		t.Fatalf("[21] 줄이 없다:\n%s", out)
	}
	for _, banned := range []string{"삭제 없음", "0건", "없습니다"} {
		if strings.Contains(line, banned) {
			t.Errorf("[21] 줄에 %q 가 있다 — 부재와 0건은 다른 사실이다: %s", banned, line)
		}
	}
}

// TestDoctorPurgeSectionLists — 기록이 있으면 총계와 최근 건을 짚는다.
func TestDoctorPurgeSectionLists(t *testing.T) {
	storeRoot, projectRoot := t.TempDir(), t.TempDir()
	// **프로젝트 ID를 doctor 자신에게서 얻는다** — 지어내면 doctor가 다른 디렉터리를 본다.
	// [2] 줄이 `ProjectID=<값>`을 내므로 한 번 돌려 그 값을 읽는다.
	first, _ := doctorOutIn(t, storeRoot, projectRoot)
	pid := ""
	for _, ln := range strings.Split(first, "\n") {
		if _, rest, ok := strings.Cut(ln, "ProjectID="); ok {
			pid, _, _ = strings.Cut(rest, " ")
			break
		}
	}
	if pid == "" {
		t.Fatalf("[2]에서 ProjectID를 못 읽었다:\n%s", first)
	}
	projDir := filepath.Join(storeRoot, "projects", pid)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := strings.Join([]string{
		"1755180000\tstartup-shadow\t336h0m0s/pwsh-profile\t1753971299\tok\t0\t0\t0\t0",
		"1755180899\tstartup-shadow\t72h0m0s/-\t1753971299\tok\t200\t1024\t0\t0",
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(projDir, store.PurgeLogName), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _ := doctorOutIn(t, storeRoot, projectRoot)
	if !strings.Contains(out, "[21] purges: 2행") {
		t.Errorf("[21]이 총계를 안 낸다:\n%s", out)
	}
	// **회귀 잠금**: 조용한 72h 강등이 이 줄에서 눈에 보여야 한다.
	if !strings.Contains(out, "72h0m0s/-") {
		t.Errorf("[21]이 policy를 안 짚는다 — 그 칸이 이 설계의 존재 이유다:\n%s", out)
	}
}

// TestDoctorSectionOrderStillAscending — [21]이 끝에 붙어 오름차순이 유지되는가.
func TestDoctorSectionOrderStillAscending(t *testing.T) {
	out, _ := doctorOut(t, t.TempDir())
	assertDoctorAscending(t, out)
	if !strings.Contains(out, "[21] purges:") {
		t.Fatalf("[21]이 없다:\n%s", out)
	}
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/cli/... -count=1 -p 1 -run 'DoctorPurgeSection|DoctorSectionOrder'`
Expected: FAIL — `[21]` 이 출력에 없다

- [ ] **Step 3: `runDoctor` 끝에 절을 더한다**

```go
	// [21] 퍼지 감사 로그 — 삭제가 어디에도 안 남던 구멍을 닫은 사이드카(스펙 §4).
	// **끝에 붙이는 이유**: 중간 삽입은 [1]~[20]을 전부 밀어 assertDoctorAscending·문서·
	// append-only 핸드오프 레코드의 번호 참조를 한꺼번에 깬다.
	{
		entries, total, unparsed := purgeLogTail(filepath.Join(projDir, store.PurgeLogName), 3)
		switch {
		case total == 0:
			// **"삭제 없음"으로 읽히면 안 된다** — 이 절이 닫으려는 결함이 그 부류다.
			fmt.Fprintln(w, "[21] purges: 기록 없음 (이 릴리스 이전 기동이거나 삭제 경로가 아직 안 돌았다)")
		default:
			var parts []string
			for _, e := range entries {
				parts = append(parts, fmt.Sprintf("%s %s %s %s %d개 %sB",
					time.Unix(e.TS, 0).UTC().Format("01-02T15:04:05Z"),
					e.Path, e.Policy, e.Status, e.Count, e.Bytes))
			}
			line := fmt.Sprintf("[21] purges: %d행 (최근 %d건) — %s", total, len(entries), strings.Join(parts, " · "))
			if unparsed > 0 {
				line += fmt.Sprintf(" · 파싱 실패 %d줄", unparsed)
			}
			fmt.Fprintln(w, line)
		}
	}
```

`projDir` 이 그 자리에 없으면 **새로 계산하지 말고** `runDoctor` 가 이미 아는 프로젝트 디렉터리 변수를 쓴다 — `[14]`·`[15]` 가 같은 디렉터리의 `content.db` 를 읽는다.

**★ `canon.ProjectID` 가 빈 문자열인 분기를 따로 낸다**(검토 소견 F14). `[2] project: 식별 실패` 경로에서는 그 `projDir` 이 `<storeRoot>/projects` 가 되어 `[21]` 이 **엉뚱한 자리를 읽고 진짜로 빈 로그와 똑같은 `기록 없음` 문면**을 낸다 — 이 절의 문면이 막으려던 바로 그 혼동이다. `[14]` 가 이미 같은 상황에 다른 문면을 내므로 그 선례를 따르고, **`total == 0` 보다 먼저** 둔다:

```go
		case canon.ProjectID == "":
			fmt.Fprintln(w, "[21] purges: 판정 불가 (프로젝트 식별 실패 — [2] 참조)")
```

- [ ] **Step 4: 통과를 확인한다**

Run: `go test ./internal/cli/... -count=1 -p 1 -run 'Doctor' -v`
Expected: 새 셋 + 기존 doctor 테스트 전부 PASS

- [ ] **Step 5: 게이트 다섯**

```
go build ./...
go vet ./...
go test ./... -count=1 -p 1
gofumpt -l .
golangci-lint run
```
Expected: build/vet 0 · gofumpt 무출력 · lint 0 issues · 12/13 패키지

- [ ] **Step 6: CHANGELOG 에 적는다**

`[Unreleased]` 의 `### Added` 절(없으면 만든다)에:

```markdown
- **퍼지 감사 로그** (`projects/<id>/purge.log`). 삭제가 지금까지 어디에도 기록되지 않았다 —
  퍼지 결과는 `slog`로만 나가고 MCP 서버의 stderr는 호스트가 버리며, 파일시스템 자국은 생성과
  삭제를 구분하지 못하고, 재고 세 축은 순감만 보여준다. 그래서 아티팩트 약 200개가 사라진
  사건의 원인 규명에 세 세션이 들었다가 "더 팔 관측이 없다"로 닫혔다.

  이제 삭제 경로 넷(`startup-shadow` · `cli-hook-only` · `cli-older-than` · `cli-gc`)이 9필드
  탭 구분 행을 남긴다. **0건 기동도 한 행을 남긴다** — *"퍼지가 안 돌았다"* 와 *"돌았는데 지울
  것이 없었다"* 는 다른 사실이고, 그 구분이 없던 것이 위 규명을 막은 것 중 하나다.

  **핵심 칸은 `policy`** 다: `store.ShadowRetention`의 **실효 반환값**과 `CTR_RETENTION_SOURCE`를
  `값/출처`로 함께 박는다. `CTR_SHADOW_RETENTION=14d`처럼 `ParseDuration`에 없는 접미로 조용히
  72h가 된 경우가 **첫 줄에서 드러난다**. `doctor [21]`이 총계와 최근 3건을 짚고, 파일이 없을
  때의 문면은 *"삭제 없음"* 이 아니라 *"기록 없음"* 이다 — 그 둘은 다른 사실이다.

  기록은 **best-effort**다: 실패는 경고 한 줄이고 퍼지 결과·종료 코드는 바뀌지 않는다.
  `stats`의 판정 표면과 원장 DB는 건드리지 않는다.
```

- [ ] **Step 7: 커밋**

```bash
git add internal/cli/cli.go internal/cli/cli_test.go CHANGELOG.md
git commit -m "feat(doctor): [21] purges — 부재와 0건을 가르는 문면"
```

---

## 완료 후

- **리뷰 래더**: 태스크 체크포인트마다 서브에이전트 리뷰. 마지막에 whole-branch 리뷰 → PR. 크로스모델 패스는 **현재 요청이 있을 때만** 돈다(외부 전송이다).
- **★ 설치하지 않는다.** 판정일(2026-08-26) 뒤에 설치한다 — 그전에 설치하면 진행 중인 14일 계측이 오염된다. 머지는 무관하다.
- **판정 뒤**: 이 스펙을 다음 설계 버전에 D-number 로 흡수하고, 이 파일은 그 결정의 유래 기록으로 남는다.
