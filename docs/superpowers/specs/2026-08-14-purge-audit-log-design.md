# 퍼지 감사 로그 설계 — 삭제가 어디에도 안 남는 문제를 닫는다

- **작성** 2026-08-14 (세션 64, 축 C)
- **상태** 설계 확정 · **구현은 판정일(2026-08-26) 이후** (§7)
- **배경 레코드** `docs/prompts/2026-08-14-session-64-*.md` §3.8·§3.9 · 세션 61 §3.3·§8.1 ·
  세션 62 §9 · 세션 63 §3.4

## 0. 왜 지금 쓰는가

이 저장소는 2026-08-08~08-12 사이에 shadow 아티팩트 약 200개를 잃었고, **원인 규명에 세 세션이
들었다가 결국 "더 팔 관측이 없다"로 닫혔다.** 그 셋이 못 푼 질문은 하나였다:

> **어느 계통이 336h 를 못 받았나.**

답이 안 나온 이유는 삭제가 **어디에도 기록되지 않기 때문**이다:

- 퍼지 결과는 `slog` 로만 나가고 **MCP 서버의 stderr 는 호스트가 버린다**(세션 61 §3.3).
- 파일시스템 자국은 **생성과 삭제를 구분하지 못한다**(세션 61 §8.1).
- 재고 세 축(blob · hashes · sources)은 **순감만** 보여주고 삭제량은 하한이다(세션 61 §9.2).

이 설계는 그 구멍 하나를 닫는다.

## 1. 성공 기준

기록은 세 질문에 답한다.

1. **삭제가 있었나 · 얼마나** — 언제, 몇 hash, 몇 바이트.
2. **어떤 정책으로 돌았나** — 실효 보존값과 **그 값의 출처**.
3. **퍼지가 돌기는 했나** — *"안 돌았다"* 와 *"돌았는데 0건"* 이 갈린다.

**판정 기준**: 200개가 사라진 그 기동의 행에 `72h0m0s/-` 가 적혀 있다. 세 세션이 아니라 한 줄
조회로 끝난다.

## 2. 저장 위치와 형식

```
<store-root>/projects/<project-id>/purge.log
```

프로젝트 레벨이다 — 삭제의 단위가 `content.db` 이고 그것이 이 디렉터리에 있다. worktree 레벨인
`session.drops.log`(훅 소유)와 층이 다르다.

append-only · 탭 구분 · UTF-8 · LF. **9필드 고정:**

```
<ts>\t<path>\t<policy>\t<cutoff>\t<status>\t<hashes>\t<bytes>\t<deferred>\t<failed>
```

| # | 필드 | 의미 | 예 |
|---|---|---|---|
| 1 | `ts` | unix 초 | `1755180899` |
| 2 | `path` | 삭제 경로 라벨(닫힌 집합) | `startup-shadow` |
| 3 | `policy` | 실효 정책 + 출처 | `336h0m0s/pwsh-profile` |
| 4 | `cutoff` | 경계 unix 초, 없으면 `-` | `1753971299` |
| 5 | `status` | 결과 분류(닫힌 집합) | `ok` |
| 6 | `count` | 삭제 단위 수 — **의미가 경로마다 다르다(§2.0)** | `0` |
| 7 | `bytes` | 실제 unlink 된 바이트, 미측정이면 `-` | `0` |
| 8 | `deferred` | age-gate·교체 감지 유예 건수, 없으면 `-` | `0` |
| 9 | `failed` | unlink 실패(고아 잔존) 건수, 없으면 `-` | `0` |

### 2.0 경로마다 반환 모양이 다르다 — 뒤 네 칸의 의미표

이 로그가 덮는 함수 셋은 서로 다른 것을 돌려준다. **균일한 스키마에 다른 것을 담으면서 그 사실을
적지 않으면, 읽는 쪽이 `cli-older-than` 의 `-` 를 "0바이트 회수"로 읽는다.** 그래서 계약을 표로
못 박는다.

| 필드 | `startup-shadow` · `cli-hook-only` | `cli-older-than` | `cli-gc` |
|---|---|---|---|
| `cutoff` | `ShadowCutoff` 결과 | 사용자가 지정한 경계 | `-` (개념 없음) |
| `count` | 행 삭제된 **hash 수** (`Hashes`) | 삭제된 **artifact 수** | 제거된 **고아 blob 파일 수** |
| `bytes` | `ReclaimedB` | `-` (함수가 안 낸다) | `-` (함수가 안 낸다) |
| `deferred` | `DeferredFiles` | `-` | `-` |
| `failed` | `FailedFiles` | `-` | `-` |

근거: `PurgeHookOnlyOlderThan` 만 `HookPurgeReport` 네 값을 낸다. `PurgeOlderThan` 은
`(sources, artifacts int64)` 로 **바이트도 유예/실패도 내지 않고**, `GCOrphanBlobs` 는
`removed int64` 하나뿐이다.

`PurgeOlderThan` 의 `sources` 는 버린다 — §1 의 세 질문에 필요하지 않고, 이 저장소의 관측상
`sources` 는 `artifacts` 와 고정 차로 함께 움직인다(`sources = hashes + 2`). 필요해지면 그때
열을 늘린다.

**★ `cli-gc` 는 세 축 정합이 깨지는 유일한 경로다** — blob 파일만 지우고 DB 행을 안 건드리므로
`blob − 고아 = hashes` 등식의 좌변만 줄어든다. 그 등식이 깨진 채 발견되면 이 로그의 `cli-gc`
행이 그 원인을 즉시 답한다.

### 2.1 `path` — 삭제 경로 넷

| 라벨 | 트리거 | 호출 함수 |
|---|---|---|
| `startup-shadow` | 서버 기동 시 1회 | `PurgeHookOnlyOlderThan` |
| `cli-hook-only` | `purge --hook-only` | 〃 |
| `cli-older-than` | `purge --older-than` | `PurgeOlderThan` |
| `cli-gc` | `purge --gc` | `GCOrphanBlobs` |

**`reclaimHookBlobs`(파일 회수 단계)는 별도 행이 아니다.** 그 결과가 `HookPurgeReport` 의
`ReclaimedB`·`DeferredFiles`·`FailedFiles` 에 이미 담겨 앞 두 경로의 행에 들어간다. 별도 행을
내면 같은 배치가 두 줄이 되어 합산이 어긋난다.

**`--vacuum` 은 기록하지 않는다** — 삭제가 아니라 이미 지워진 것의 공간 회수다. 재고 세 축을
움직이지 않는다.

### 2.2 `status` — 결과 분류

우선순위 순으로 **정확히 하나**를 고른다.

| 값 | 뜻 | 다음 걸음 |
|---|---|---|
| `failed` | 행 삭제 자체가 실패(커밋 전 롤백) | 다음 기동이 같은 배치를 다시 집는다 |
| `partial` | 행 삭제는 커밋됐고 **파일 회수가 중단**됨 | 남은 파일은 `purge --gc` 몫 |
| `budget` | 예산 소진으로 중단(행 삭제 미완) | 다음 기동에서 재시도 |
| `cancelled` | 종료 신호로 중단(커밋 전) | 다음 기동이 그대로 다시 집는다 |
| `capped` | 건수 상한에 걸림 — 성공이되 배치가 남음 | 다음 기동이 이어서 집는다 |
| `ok` | 정상 종료 | — |

**이 분류는 새로 만드는 것이 아니다.** `cmd/context-router/main.go` 의 기동 퍼지 고루틴이 이미
`purgeErr`·`rep.Hashes`·`purgeCtx.Err()` 세 값으로 같은 갈래를 낸다. 그 switch 를 그대로 쓴다 —
분류를 다시 쓰면 로그와 slog 가 갈린다.

`partial` 이 `budget`·`cancelled` 보다 위인 이유도 그 switch 의 근거 그대로다: **행이 이미
삭제돼 다음 기동 술어에 다시 안 잡히므로**, 중단 사유가 무엇이든 남은 파일의 유일한 회수 경로가
`purge --gc` 다. 순서가 뒤집히면 *"다음 기동이 다시 집는다"* 는 거짓 안내가 나간다.

### 2.3 `policy` — 이 설계의 핵심 필드

경로마다 의미가 다르다.

| 경로 | 형식 | 예 |
|---|---|---|
| `startup-shadow` · `cli-hook-only` | `<실효 보존값>/<출처>` | `336h0m0s/pwsh-profile` |
| `cli-older-than` | `<사용자가 준 기간>/-` | `720h0m0s/-` |
| `cli-gc` | `-` | `-` |

**실효 보존값은 `store.ShadowRetention(os.Getenv)` 의 *반환값*이지 환경변수 원문이 아니다.**
그 함수는 `time.ParseDuration` 실패·비양수를 기본값 72h 로 흡수한다 — 즉 `CTR_SHADOW_RETENTION=14d`
처럼 **`d` 접미가 없어 조용히 72h 로 떨어진 경우에도 로그에는 `72h0m0s` 가 찍힌다.** 세션 61 이
세 세션 걸려 찾은 그 함정이 첫 줄에서 드러난다.

**출처는 `CTR_RETENTION_SOURCE` 값을 그대로 적는다.** 코드는 이 값을 해석하지 않는다 — 사람이
붙인 진단 라벨이고(세션 61 이 도입, 게이트 확인 2 의 `SRC`), 기록만 한다. 비어 있으면 `-` 다.
**빈 출처 자체가 신호다**: 프로필 처방이 닿지 않은 계통이라는 뜻이다.

### 2.4 위생

모든 필드에 `internal/hook` 의 `appendDrop` 이 쓰는 `san()` 과 **같은 처리**를 건다:

- 탭 · 개행 · CR → 공백
- 필드당 **64자 상한**
- 빈 값 → `-`

`CTR_RETENTION_SOURCE` 는 사람이 임의 문자열을 넣을 수 있는 자리다. 위생이 없으면 그 한 값이
파서를 오염시키고, 그것은 이 로그가 막으려던 바로 그 종류의 침묵을 만든다.

## 3. 쓰는 쪽

형식과 쓰기 함수는 `internal/store` 가 소유한다 — 삭제를 실제로 하는 패키지이고
`projects/<pid>` 경로를 아는 자리다.

```go
type PurgeRecord struct {
	Path, Policy, Status string
	Cutoff               int64  // 0이면 "-"
	Count                int64  // 의미는 경로마다 다르다 — §2.0
	Bytes                *int64 // nil이면 "-" (함수가 바이트를 안 내는 경로)
	Deferred, Failed     *int   // 〃
}

func AppendPurgeLog(projectDir string, rec PurgeRecord)
```

- **best-effort.** 실패는 `slog.Warn` 한 줄이고 퍼지 결과에 영향이 없다(`appendDrop` 과 동형).
- 반환값 없음 — 호출자가 판정할 것이 없다.

**★ 뒤 세 칸이 포인터인 이유**: `0` 은 *"쟀더니 0이었다"* 는 주장이고, 함수가 애초에 그 값을
내지 않는 경로(§2.0)에서 그것을 쓰면 **거짓 측정**이 된다. 이 저장소는 같은 함정을 이미 한 번
지났다 — `recordFetchResolve` 의 `ageS *int64` 가 *"0으로 두면 같은 초에 회수한 행과 구분되지
않아 분포가 내려간다"* 는 이유로 nil 을 쓴다(D103 소견 F9). 같은 규칙을 여기에도 건다: **못 잰
것은 `-`, 재서 0인 것은 `0`.**

**부르는 것은 호출자다.** `status` 분류가 ctx 종료 사유에 걸려 있고 그 정보는 store 안에 없다.
호출 지점 넷:

1. `cmd/context-router/main.go` — 기동 퍼지 고루틴의 switch. **모든 분기에서 한 번씩 부른다.**
   지금 `case rep.Hashes > 0:` 만 로그를 내는 그 switch에 **0건 분기를 위한 자리를 만드는 것이
   이 항목의 요점이다** — 그것이 "기동마다 1행"이 성립하는 지점이다.
2. `internal/cli` `runPurgeHookOnly`
3. `internal/cli` `runPurge` — `PurgeOlderThan` 직후
4. 같은 함수 — `GCOrphanBlobs` 직후

세 곳이 같은 형식을 손으로 조립하면 갈린다(D13). 그래서 조립은 `AppendPurgeLog` 한 곳이고
호출자는 `PurgeRecord` 만 채운다.

## 4. 읽는 쪽

파서와 진단 문면은 `internal/cli` 가 소유한다 — `dropsByReason` 이 이미 같은 자리에 있는
선례이고, 쓰기(`internal/hook`)와 읽기(`internal/cli`)가 갈라진 그 패턴을 그대로 잇는다.
파일명 상수는 `dropsFileName` 이 그러듯 미러 상수로 둔다.

```go
func purgeLogTail(path string, n int) (entries []purgeEntry, total int, unparsed int)
```

**`doctor [21]`** — 새 절은 **끝에 붙인다.** 중간 삽입은 `[1]`~`[20]` 을 전부 밀어 테스트
(`assertDoctorAscending`)·문서·핸드오프 레코드의 번호 참조를 한꺼번에 깨뜨린다.

```
[21] purges: 47행 (최근 3건) — 08-14T10:54:59Z startup-shadow 336h0m0s/pwsh-profile ok 0개 0B ·
     08-14T08:59:42Z startup-shadow 336h0m0s/pwsh-profile ok 0개 0B · …
```

- 파일 없음 → `기록 없음 (이 릴리스 이전 기동이거나 삭제 경로가 아직 안 돌았다)`
  — **"삭제가 없었다"로 읽히면 안 된다.** 문면이 그 둘을 가른다.
- 9필드가 아닌 줄은 `unparsed` 로 세고 진단을 **중단하지 않는다**(`dropsByReason` 관례).
- `total` 은 줄 수 계약이다(빈 줄 포함) — 같은 관례.

## 5. 오류 처리

| 상황 | 동작 |
|---|---|
| 로그 쓰기 실패 | `slog.Warn` 한 줄. 퍼지 결과·종료 코드 불변 |
| 프로젝트 디렉터리 부재 | 발생하지 않는다(퍼지가 도는 시점엔 이미 있다). 그래도 실패는 위와 동일 |
| 로그 읽기 실패 | doctor 절이 `읽기 실패` 를 내고 나머지 진단은 계속 |
| 깨진 줄 | `unparsed` 카운트, 나머지 줄은 정상 처리 |

## 6. 테스트

1. **`AppendPurgeLog` 필드 조립** — 9필드 · 빈 값의 `-` · 탭/개행/64자 초과 위생.
2. **파서** — 정확 9필드만 수용, 그 외 `unparsed`. 빈 파일 · 부재 · 깨진 줄 혼재.
3. **doctor `[21]`** — 부재 문면이 *"삭제 없음"* 으로 읽히지 않는지를 **문면 그대로** 단정한다.
4. **★ 회귀 잠금** — 세션 58~62 시나리오: 보존값이 72h 로 떨어진 상태의 퍼지 행이 `policy` 에
   `72h0m0s` 를 담는가. **이 테스트가 이 설계의 존재 이유다.**
5. **0건 기동** — `cmd/context-router` e2e 에서 삭제 0인 기동이 **행 하나를 남기는가**.
   (지금 코드에서 그 분기는 아무것도 안 한다 — 이 테스트가 그 침묵을 잠근다.)
6. `status` 여섯 갈래의 우선순위 표 테스트 — 특히 `partial` 이 `budget`·`cancelled` 를 이기는가.
7. **미측정과 0의 구분** — `cli-older-than` 행의 `bytes`·`deferred`·`failed` 가 `0` 이 아니라
   `-` 로 나오는가. 세 칸이 포인터인 이유가 이것이고(§3), `0` 으로 새면 읽는 쪽이 *"회수 바이트가
   0이었다"* 는 하지 않은 측정을 읽는다.

**게이트 다섯**(`go build` · `go vet` · 전체 테스트 `-count=1 -p 1` · `gofumpt -l .` ·
`golangci-lint run`)을 구현 플랜의 검증 단계에 그대로 쓴다.

## 7. 착수 시점과 문서 승격

**구현은 판정일(2026-08-26) 이후다.** 설치를 하지 않으면 이 코드는 돌지 않고, 판정 전 설치는
채택 레버를 살려 판정 지표를 오염시킨다.

**이 스펙이 `docs/superpowers/specs/` 에 있는 이유**는 제품 계약이 아니어서가 아니다 — 계약이
맞다. 지금 `docs/context-router-design-v*.md` 를 열면 그 문면이 `ctr_fetch` 를 권해 진행 중인
14일 계측을 오염시키기 때문이다(세션 60 §5.3). **판정 뒤 이 스펙을 다음 설계 버전에 D-number 로
흡수하고, 그때 이 파일은 그 결정의 유래 기록으로 남는다.**

## 8. 명시적 비목표 (YAGNI)

| 뺀 것 | 이유 |
|---|---|
| 지워진 hash 목록 | 되살릴 수 없고 200개 삭제가 200줄이 된다. 재고 세 축이 "무엇이 남았나"를 이미 답한다 |
| 프로세스·PID·계통 | `policy` 가 *"누가 잘못된 값을 받았나"* 의 시간대를 이미 좁힌다. PID·실행 경로는 egress 표면을 넓히고(S5) 원장은 프로젝트 레벨이라 worktree 귀속도 따로 정해야 한다 |
| 로그 로테이션 | 기동당 약 100 B. 하루 20 기동이면 1년에 1 MB 미만이다 |
| 질의 가능한 테이블 | §1 의 세 질문에 질의가 필요 없다. 꼬리 N행과 총계로 끝난다 |
| `stats` 표 노출 | 판정 지표를 읽는 그 표에 새 `tool` 행이 끼면 세션 63 이 겪은 *"계상 방식이 바뀌었다"* 계열의 혼란이 하나 더 생긴다 |
| 원장 DB 기록 | 퍼지는 `lockStoreCtx` 를 쥐고 60초 예산 안에서 돈다. 그 구간의 ledger INSERT 는 실패 확률이 오르고 best-effort 라 **조용히 사라진다 — 그것이 애초의 문제였다** |

## 9. 이 설계가 닫지 *않는* 것

- **삭제 시각의 역산.** 이 릴리스 **이전**에 일어난 삭제는 여전히 관측 불가다. 로그는 앞으로만
  쌓인다.
- **누가 지웠나.** 계통 정보를 뺐으므로(§8) `policy` 가 가리키는 시간대까지가 한계다.
- **재고 감시의 대체.** 세 축 정합(`blob − 고아 = hashes = 일별 칸 합`)은 그대로 주축이다. 이
  로그는 **원인 축**을 더할 뿐 순감 관측을 대신하지 않는다.
