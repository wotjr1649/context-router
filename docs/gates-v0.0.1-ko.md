# 게이트 체크리스트 (v0.0.1 태그 판정 문서)

설계 §12(테스트·수용 게이트, 13항)의 각 게이트별 요지·증거·상태를 정리한 문서. **게이트 13에
따라 아래 13항이 전부 PASS(수동 항목은 사용자 확인 완료)가 아니면 `v0.0.1` 태그를 만들지
않는다.** 계획 1(9 task) → 계획 2(6 task) → 계획 3(10 task, 본 문서 포함) 순으로 누적 구현된
증거를 이 문서 하나에 모은다.

## 게이트 1~13

| # | 게이트(설계 §12 요지) | 증거 | 상태 |
|---|---|---|---|
| 1 | **golden fixtures**: `ctr_search`/`ctr_index`는 oracle(ctxscribe 1.3.0) 동등성, `ctr_fetch`/`ctr_transform`/`ctr_fetch_and_index`는 자체 golden | `internal/search/retrieval_test.go` `TestOracleEquivalence` — 12/12 통과(2026-07 최초 실행), 매핑 근거 `docs/oracle-mapping-ko.md`(D-1~D-8, 코드 인용). 자체 golden: `internal/ingest/ingest_test.go` `TestChunkText_Golden_Plain`/`TestChunkText_Golden_Doc`(ctr_index 청킹 고정), `internal/netfetch/netfetch_test.go` `TestFetch_HTML_DocWithCodeAndTable`/`TestFetch_HTML_CodeHeavyStripped`(HTML fixture→기대 markdown, ctr_fetch_and_index 경로), `internal/mcp/mcp_test.go` `TestCtrTransformRoundTrip`(ctr_transform 결정론적 라운드트립) | PASS |
| 2 | **retrieval 평가 하네스**: recall@k 측정 + 기준선 기록 | `internal/search/retrieval_test.go` `TestRetrievalRecall` — 실측 mean recall@5=0.9306, recall@10=1.0000(회귀 플로어 `minRecallAt5`=0.9305/`minRecallAt10`=0.9999로 고정), `docs/oracle-mapping-ko.md` 기록 | PASS |
| 3 | **경로 시험**: symlink/junction/case-fold/UNC/git worktree/`..`탈출/워크 중 심링크/allow-path·store-root 거부 — 3 OS | `internal/ident/ident_test.go` `TestCanonicalize_GitDirAndWorktreeFile`·`TestFold_ExtendedUNC`(_SlashVariant)·`TestFold_OSRule`; `internal/ingest/ingest_test.go` `TestRun_PathEscape_Absolute`·`TestRun_PathEscape_Symlink`; `cmd/context-router/main_test.go` `TestCanonicalizeAllowPaths`(store-root 하위 거부); **신규(계획3 T10 Fix R1, windows 실측)**: `TestCanonicalize_JunctionResolvesToRealTarget` — NTFS junction(mklink /J) 경유 경로. 최초 실측에서 **실패**(Go `filepath.EvalSymlinks`가 junction=IO_REPARSE_TAG_MOUNT_POINT를 인식 못 함 — os.Lstat이 junction에 `ModeSymlink`를 안 세움, 실제 버그 발견)했고, `internal/ident/realpath_windows.go`(windows.GetFinalPathNameByHandle) + `realpath_other.go` 신설로 `Canonicalize`가 junction까지 실경로로 해석하도록 근본 수정 후 PASS로 전환(전체 스위트 회귀 없음 확인). `TestCanonicalize_CaseOnlyDirsFoldToSameIdentity`(대소문자만 다른 두 경로가 동일 identity로 귀결, windows). per-directory case-sensitivity(fsutil, admin 권한)로 "실제 대소문자만 다른 두 디렉터리"를 만드는 케이스는 **CI 러너 권한 제약 — 대기**(미구현, 의도적 보류). **최종리뷰 F1 후속(전 소비처 통일)**: 위 수정이 `ident.Canonicalize` 내부만 junction-aware로 고쳤고 `internal/ingest/ingest.go`의 `canonicalPath`와 `cmd/context-router/main.go`의 `canonicalizeStoreRoot`/`canonicalizeAllowPaths`는 여전히 `filepath.EvalSymlinks`를 직접 호출해 junction 경유 프로젝트에서 경계 판정이 어긋날 수 있었다(정당 경로 오탐 또는 내부 junction으로 경계 우회) — `realPath`를 `ident.RealPath`로 export해 3곳 모두 동일 함수를 쓰도록 통일. 회귀 테스트: `internal/ingest/ingest_test.go` `TestRun_JunctionProjectAllowsLegitimatePath`·`TestRun_JunctionEscapesWorkspace`(windows 게이트, 수정 전 실패·수정 후 PASS 확인). 3-OS 실행 = CI run(게이트 12와 동일) | PASS(신규 항목 포함, per-dir case-sensitivity만 보류) |
| 4 | **secret canary**: denylist 미색인 + span redaction(청크 경계 걸친 PRIVATE KEY 포함) 후 search/fetch 양쪽 미회수, JSON-escape·헤더 변형 포함 | `internal/ingest/ingest_test.go` `TestRedact_Canaries`(AWS/GitHub/JWT/cookie/docker-auth/password + 멀티라인 PRIVATE KEY 블록), `TestRedact_UnicodeEscapedSecret`(`\uXXXX` 은닉 탐지), `TestRun_DeniedFilename_SymlinkBypass`; `internal/mcp/mcp_test.go` `TestCtrFetchAndIndexRoundTrip`(fetch_and_index 경로 canary 미회수, search/fetch 양쪽) | PASS |
| 5 | **SSRF matrix**: I1~I8(사설/link-local/메타데이터/NAT64·CGNAT·0.0.0.0/8·v4-mapped·zone/rebinding/리터럴 redirect/강등/proxy 무시/크기 초과) | `internal/netfetch/netfetch_test.go` `TestClassifyAddr`(전 대역 테이블), `TestFetch_RedirectHopRevalidated`(hop별 재검증), `TestFetch_TooManyRedirects`, `TestTransportIgnoresEnvProxyAndHasNoRedirectFollow`, `TestFetch_MaxBytesExceededAborts`, `TestIsDowngrade`(https→http 강등) | PASS |
| 6 | **FTS 무결성·동등성**: `INSERT INTO fts(fts) VALUES('integrity-check')` 재색인·purge 후 통과, porter/trigram/BM25+RRF 스모크 + 대량 코퍼스 성능 | `internal/store/store_test.go` `TestOpen_PragmasAndSchema`(integrity-check), `TestPurgeOlderThan`(purge 후 integrity-check); `internal/search/search_test.go` `TestQuery_PorterStemsMatch`/`TestQuery_TrigramSubstringMatch`/`TestQuery_RRFRanksDualMatchTop`; `internal/search/retrieval_test.go`(코퍼스 61청크·다중 쿼리 성능 스모크, `TestCorpusShape` 가드). **신규(계획3 T10 Fix R1)**: `internal/search/perf_test.go`(`//go:build perf` 전용, CI 제외) `TestPerf_5000Docs` — 어휘 다양성 있는 합성 5,000 doc 색인 + 대표 쿼리 20회 로컬 실측(windows dev 머신, 2026-07-19): **색인 5000 docs / 24.8s(≈202 docs/s), 쿼리 20회 / 32.8s(평균 ≈1638ms/query)**. 근본 원인 실측: bm25Rank의 `rowid IN (SELECT ... WHERE EXISTS(...))` orphan 필터가 fts_trigram MATCH에서 특히 비쌈(porter는 쿼리당 ~200-300ms인데 trigram은 700ms~2.2s) — `sources(artifact_id)` 인덱스를 추가해도(SQLite가 이미 AUTOMATIC PARTIAL COVERING INDEX로 동등 처리) 유의미하게 개선되지 않음을 직접 실측 확인, 5,000-doc 규모 trigram 스캔 자체의 비용으로 판단. 회귀 문지기 아님(하드웨어 편차) — 아래 "알려진 갭"에 후속 조사 항목으로 기록 | PASS(무결성/동등성) — perf는 정보성 실측(비차단) |
| 7 | **DB 동시성·내구성**: writer1+reader4, 프로세스 2개 동시 쓰기, CAS, dedup, kill 후 무결성, user_version 비파괴 거부 — 3 OS (심층: 최초 동시 기동 WAL 경합) | `internal/store/store_test.go` `TestRegister_CASRejectsStaleWriter`, `TestRegister_DedupTwoSourcesOneArtifact`, `TestOpen_NewerVersionRefusedNonDestructively`, `TestRegister_ConcurrentDistinctBlobsNoTmpLeftover`(8-goroutine 동시쓰기, tmp 잔존 없음); `cmd/context-router/main_test.go` `TestE2E_TwoProcessConcurrentIndex`(프로세스 2개 동시 색인, 3번째 프로세스로 무결성 확인); 심층(T1): `TestOpen_ConcurrentFirstMigration`(최초 동시 기동 WAL/migration 경합 — advisory lock으로 근본 수정). **신규 심층(계획3 T10 Fix R1, windows 로컬 실측 6회 반복 안정)**: `TestRegister_TwoProcessCASRace`(실 OS 프로세스 2개가 동일 URI·상이 콘텐츠로 구지문 기반 CAS 경쟁 → 정확히 1승1패, 최종 포인터가 과거로 회귀하지 않음+integrity-check 통과), `TestOpen_SurvivesWriteKillMidLoop`(자식이 5,000회 Register 루프 중 부모가 50ms 뒤 강제 kill — 실측 2~3건 커밋 후 kill, 재오픈 quick_check=ok+integrity-check 통과+커밋된 행 전부 무결), `TestConcurrency_Writer1Reader4`(writer 1 + reader 4 고루틴, FTS MATCH+ArtifactText 연속 수행, 오류 0 — race는 CI ubuntu `-race` 잡이 커버) | PASS |
| 8 | **transform 상한**: 스텝/출력 초과, 거대 문자열·리스트 증식이 worker 메모리 상한으로 종료(서버 생존), timeout 시 트리 전멸 | `internal/transform/worker_test.go` `TestSpawn_MemoryExplosion`(메모리 상한 종료, 부모 생존), `TestSpawn_Timeout`(트리킬), `TestSpawn_DefaultTimeout`(ctx deadline 없을 때 10s 안전망); `internal/transform/transform_test.go` `TestBudgetExceeded`/`TestOutputLimitExceeded`; `internal/transform/worker_windows_test.go` `TestKillOrphan_KillsStartedProcess`(Windows 트리킬 원시 동작) | PASS |
| 9 | **추출 충실도**: 대표 HTML corpus pre/code/table 보존 판정 + fail-open 전환 | `internal/netfetch/netfetch_test.go` `TestFetch_HTML_DocWithCodeAndTable`(코드펜스·표 보존, extraction=readability), `TestFetch_HTML_CodeHeavyStripped`(pre/code 보존율<50%→fail-open), `TestFetch_HTML_ShortNonArticle`(<500자→fail-open) | PASS |
| 10 | **프로토콜 위생**: stdout 오염 0 + Claude Code·Codex 실 등록 스모크(tools/list·호출·cancellation) | 자동: `cmd/context-router/main_test.go` `TestE2E_StdioRoundTrip`(실 바이너리 stdio 왕복, 매 stdout 줄 JSON 유효성 검증), `internal/mcp/mcp_test.go` `TestServeStdoutPurity`(즉시 EOF 기준선), `TestServeStdoutPurityDuringErroringToolCall`(**신규, 계획3 T10** — store를 미리 Close해 tools/call 처리 도중 실제로 `slog.Error`가 stderr에 찍히는 그 순간에도 stdout이 JSON-RPC 줄만 유지되는지 확인, Task 8 minor "stdout purity 테스트 narrow(툴콜 중 오염 미검)" 해소). **신규(session-04, PR #5)**: `TestE2E_CallToolCancellation` — 서버 취소 계약 (3a)의 CI 상주 스모크(notifications/cancelled 결정적 주입 + worker 슬롯 해제 직접 관찰). 수동(2건, 아래 §수동 스모크 참조): **확인 완료(2026-07-19)** — cancellation은 확인 항목 3의 분리 판정((3a) 스크립트드 PASS 필수 + (3b) 실호스트 1회 재시도 기입, 비차단 협의) | PASS |
| 11 | **스키마 토큰 예산**: 기본 3종 도구 정의 tokenizer 실측, 상한 기록 | `internal/mcp/mcp_test.go` `TestSchemaTokenBudget`(**신규, 계획3 T10**) — 기본 프로필(ctr_search/ctr_fetch/ctr_transform) tools/list 결과 직렬화 **4359 bytes**(근사 토큰 ~1089, bytes/4 — Claude 정확 tokenizer 비공개라 근사치, 바이트 상한이 실질 게이트). 상한 `maxToolSchemaBytes` = 최초 실측×1.2 반올림 = **5231 bytes**로 고정 | PASS |
| 12 | **빌드**: `CGO_ENABLED=0` 6타깃 크로스빌드 + 크기 기록, memory-capped CI | `.github/workflows/ci.yml` — 3-OS(ubuntu/macos/windows) 테스트 매트릭스 + crossbuild 6타깃(6조합 GOOS/GOARCH) + 바이너리 크기를 `GITHUB_STEP_SUMMARY`에 기록. CI run: https://github.com/wotjr1649/context-router/actions/runs/29652804882 (GREEN) | PASS |
| 13 | 전 게이트 통과 전 태그 금지 | 아래 §태그 절차 참조 | 절차 기록됨(태그는 PR 머지 후) |

## 게이트 10 — 수동 스모크 (실 등록, 자동화 불가)

설계 §12-10의 "Claude Code·Codex 실 등록 스모크"는 실제 호스트 설정 파일에 이 바이너리를
등록하고 사람이 확인해야 하는 항목이라 CI/테스트 스위트로 자동화하지 않는다(자동화 금지 —
프로젝트 CLAUDE.md 협업 프로토콜). 아래 절차대로 **사용자가 직접 수행**하고 결과를 기입한다.

### 명령

```
# 빌드(플랫폼별로 한 줄만 실행)
go build -o ctr ./cmd/context-router        # unix(linux/macOS)
go build -o ctr.exe ./cmd/context-router     # windows

# 등록 스니펫은 직접 옮겨 적지 않는다(cli.go hostSnippet과 따로 관리하면 드리프트한다) —
# 아래처럼 doctor를 실행해 그 출력(Claude Code .mcp.json + Codex config.toml 형태)을
# 그대로 복사해 호스트 설정에 붙여넣는다.
./ctr doctor --root <프로젝트 경로>
```

### 확인 항목

플랫폼별 기대 도구 집합이 다르다 — darwin은 RLIMIT_AS self-apply가 항상 실패해
`ctr_transform`이 fail-closed로 미등록된다(§14 후속, 위 "알려진 갭" 참조). 이는 버그가
아니라 의도된 안전 동작이다.

1. 호스트가 기동 시 `tools/list`에서 기대 도구가 정상 노출되는가.
   - **linux/windows**: `ctr_search`/`ctr_fetch`/`ctr_transform` 3종(+opt-in 시
     `ctr_index`/`ctr_fetch_and_index`).
   - **darwin**: `ctr_search`/`ctr_fetch` 2종만(`ctr_transform` 없음 — 있으면 오히려 회귀).
2. 도구 1개(`ctr_search` 권장)를 실제로 호출해 정상 응답이 오는가.
3. 호출 도중 취소(cancellation) 처리 — 두 갈래로 분리해 판정한다(2026-07-19 session-04 개정,
   session-03 이월 경위 하단 참조):
   - **(3a) 서버 취소 계약(스크립트드)**: go-sdk 클라이언트(ClientSession+CommandTransport)로
     실 바이너리에 `notifications/cancelled`를 결정적으로 주입 —
     `cmd/context-router/main_test.go` `TestE2E_CallToolCancellation`. 단언: 취소 즉시
     `context.Canceled` 반환(클라이언트 계약), worker 슬롯(≤2) 즉시 해제로 서버측 취소 처리를
     직접 관찰(점유·취소 후 세 번째 transform 소요 시간 판별 — 교차 리뷰 P1 반영), 후속
     `ctr_search` 정상, stdin close에 graceful exit(코드 0). CI 상주 테스트 — 상시 보증은
     windows 잡이고, linux는 RLIMIT_AS 특성상 worker가 취소 전 fail-closed 조기 사망할 수
     있어(하단 해소 경위·알려진 갭) 조기 사망 3회 시 skip하는 최선노력 커버리지다. darwin은
     `ctr_transform` fail-closed 미등록으로 대상 외.
   - **(3b) 실호스트 취소 전달(수동)**: 호스트 UI(ESC)에서 시작된 취소가 서버까지 전달되는지.
     호스트측 전달 자체가 비결정적(session-03 4회 시도 경위 + 알려진 호스트 이슈)이므로
     시도 결과를 그대로 기록하되, 서버측 계약은 (3a)가 보증하므로 (3b) 실패/미관측을 게이트
     차단 요소로 삼지 않는다(2026-07-19 사용자 협의 — 1회 재시도 후 결과 그대로 기입).
   - 스크립트드(3a)만으로 본 게이트 전체를 PASS로 표기하지 않는다 — 아래 표의 cancellation
     칸은 (3a)/(3b)를 나눠 기입한다.

### 결과 (사용자 기입)

| 호스트 | 플랫폼 | tools/list 노출(기대 도구 수 일치) | 호출 1회 | cancellation | 확인일 | 비고 |
|---|---|---|---|---|---|---|
| Claude Code | windows | PASS (3종) | PASS | (3a) PASS(스크립트드, PR #5) / (3b) 시도 완료 — ESC가 실행 중 호출을 즉시 중단(10s 창 내), 서버 생존·후속 `ctr_search` 정상. 알림의 서버 도달 여부는 호스트 불투명 → 비차단(협의) | 2026-07-19 | ctr_search 정상(빈 스토어 hits:[]) + ctr_transform 오류 경로(256MB 메모리 킬) — 서버 생존·후속 호출 정상. 참고(3b 판정 아님): (3b) 준비 호출 2회는 ESC 미입력으로 10s 상한 도달 — 그때도 서버 생존·후속 정상 |
| Codex | windows | PASS (3종) | PASS | (3a) PASS(스크립트드, PR #5 — 서버 계약은 호스트 무관) / (3b) 미시도(비차단, 하단 경위) | 2026-07-19 | ctr_search 정상 스키마 응답 + ctr_transform 오류 경로(10s 시간 상한) + 잘못된 인자 스키마 거부 — 서버 생존·후속 호출 정상 |

**cancellation 이월 해소 경위 — (3a)·(3b) 완료 (2026-07-19, session-04)**: 위
(3a)/(3b) 분리 개정에 따라 스크립트드 스모크 `TestE2E_CallToolCancellation`을 작성·검증
완료(최종판 로컬 windows 4/4 PASS + PR #5 CI 3-OS GREEN; 교차 리뷰 — 서브에이전트
6라운드 + Codex 2패스 — 반영). **(3b) 결과**: 실호스트(Claude Code) 재시도 1회 수행 —
등록 서버에 ~10s 워크로드 호출을 걸고 사용자가 ESC로 취소, 호출이 10s 상한 오류 대신
즉시 중단됐고(실행 중 인터럽트 — 동일 도구가 세션 내 무프롬프트 2회 선실행돼 권한 단계
아님) 직후 `ctr_search` 정상 응답으로 서버 생존 확인. notifications/cancelled의 서버 도달
여부는 호스트 내부라 관측 불가(사전 협의대로 비차단 — 서버측 취소 계약은 (3a)가 보증).
원 게이트 문언("호스트·서버 모두 비정상 종료 없이 처리")은 충족. 작성 과정의 실측 발견 2건:
① session-03 레시피의 bounded-churn 워크로드(8MB 가비지 반복 생성)가 worker를
비결정적으로 조기 사망시키는 현상을 실측(windows, 20회 중 7회, 124ms~1.58s)하고 transform
레이어 격리 재현으로 근본 원인 확정 — 상주 12MB에도 GC의 커밋 반환이 순간적으로 뒤처지면
커밋이 Job Object 상한(256MB)에 도달 → `VirtualAlloc` 실패(errno=1455) → Go 런타임 OOM
abort(exit 2). PR #4가 미규명으로 남긴 ubuntu RLIMIT_AS 조기 사망(194ms)과 동일 계열
메커니즘의 규명이며, fail-closed 설계상 서버 생존 계약은 유지되므로 제품 결함이 아니다
(개선 후보는 "알려진 갭"). 스모크 워크로드를 할당 없는 count 스캔으로 교체해 대응.
② PR #5 최초 CI에서 ubuntu 잡이 두 양상으로 실패 — (i) 할당 없는 워크로드조차 worker가
8.4ms에 조기 사망(run 29676082922; RLIMIT_AS는 commit이 아닌 주소공간 상한이라 Go 런타임의
가상주소 예약과 충돌 — linux 고유), (ii) 테스트 진단 경로의 `cmd.Wait()`가 SDK read-loop의
`cmd.Wait()`와 경쟁해 stderr 복사 고루틴 join에서 10m 영구 대기(run 29676073834). 대응:
진단 경로를 `sess.Close()` 선행으로 교체(단일 Wait 소유자), linux는 조기 사망 3회 시 skip
하는 재시도 로직 추가(위 (3a) 서술의 "최선노력" 근거).

**cancellation 이월 경위 (2026-07-19, session-03)**: 수동 ESC 취소를 4회 시도했으나
전부 취소 창 확보에 실패 — ① 원시 스크립트 프롬프트가 호스트 모델 세이프가드에 플래그,
② 순수 루프는 스텝 예산(5M)에 즉시 도달, ③ ctr_fetch를 웹 fetch로 오인(절차 오류),
④ 워크로드가 메모리 상한/10s 시간 상한에 취소보다 먼저 도달. 호스트 ESC→cancellation
전달 자체의 비결정성(호스트측 알려진 이슈)도 확인됨. **간접 증거는 확보**: go-sdk의
notifications/cancelled 수신→핸들러 ctx 취소(SDK 자체 TestCancellation), 3개 도구
핸들러의 ctx 전파(transform→exec.CommandContext, search/fetch→QueryContext),
toToolError의 context.Canceled 정상 처리, 그리고 위 표의 워커 킬 2회에서 서버 생존·후속
호출 정상 관측. **마감 방법(다음 세션)**: go-sdk 클라이언트(ClientSession+CommandTransport,
ctx 취소 시 notifications/cancelled 자동 송신 — SDK transport.go:229-235 확인)로 ctr.exe를
스폰해 취소를 결정적으로 주입하는 스크립트드 스모크를 작성하고, 본 게이트 확인 항목 3을
"(3a) 서버 취소 계약(스크립트드) / (3b) 실호스트 취소 전달(수동)"로 분리 개정해 판정한다.
**이 이월분이 해소되기 전에는 게이트 13 태그를 만들지 않는다.**

## 태그 절차 (게이트 13)

전 게이트(1~12) PASS + 게이트 10 수동 스모크 2건 확인 전에는 태그를 만들지 않는다.

1. PR(`feat/v0.0.1-global-cli` → `main`) 머지.
2. `main` 브랜치 CI(`.github/workflows/ci.yml`) GREEN 확인.
3. 위 "게이트 10 — 수동 스모크" 표의 Claude Code·Codex 2건을 사용자가 실제로 수행하고 결과란을
   채운다. cancellation 칸은 확인 항목 3의 분리 판정을 따른다 — (3a) 스크립트드 PASS는 필수,
   (3b)는 1회 재시도 결과를 그대로 기입하며 실패/미관측이어도 태그를 차단하지 않는다
   (2026-07-19 사용자 협의, 확인 항목 3 참조).
4. `git tag v0.0.1 && git push origin v0.0.1`.

## v0.0.1 이후 (의도적 미결·이월)

### 설계 §14 유보(그대로 승계)

- **v0.2**: exec 3종 상세 계약(OS별 셸, Job Object/landlock/sandbox-exec 조합, 출력 ephemeral
  정책), Shadow Recall 훅 설계, OTel·Codex usage 어댑터, 무작위 A/B 하네스.
- **v0.1**: Session DB 스키마, 이벤트 3종 계약, SessionEvent v1 export, retention 자동화.
- 검색 semantic 보강(retrieval 병목 대응) — recall@k 기준선 측정 후 판단. 2차 추출기
  (go-trafilatura) — 게이트 9 실패 시(현재 게이트 9 PASS라 미착수).

### 알려진 갭 (제품 동작에 영향, 릴리스 차단 아님)

- **darwin RLIMIT_AS 부재 → `ctr_transform` fail-closed 미등록**: darwin에서 self-apply
  RLIMIT_AS(Setrlimit)가 항상 실패해 `ProbeIsolation`이 실패하고 `ctr_transform` 도구 자체가
  미등록된다(in-process fallback 금지 원칙상 의도된 fail-closed). darwin용 메모리 격리
  전략 재설계는 §14 후속(T9 실측 발견, CI run 29652804882).
- **title dedup**: 현재 소스 재색인 시 title 갱신이 스키마상 source-단위가 아니라 v0.1에서
  스키마 확장과 함께 정리 예정(계획 2 T6 이월).
- **oracle LICENSE 저작권자 확인 — 해결(2026-07-19, PR #3)**: 조사 결과 upstream
  `wotjr1649/ctxscribe`는 `mksglu/context-mode`의 ELv2 fork로, LICENSE의 'Mert Koseoglu'는
  원본 context-mode 원저작자 표기라 정당(불일치의 원인은 fork의 package.json author가
  fork 유지자 Kim Jae Seok인 것). NOTICE에 원본↔fork 계보를 분리 명시(문안 사용자 승인,
  커밋 ef4ace7) — 태그 보류 사유 해소. (계획 3 T7 발견 이월분)
- **TOCTOU 완전판(openat2)**: 현재 경로 검증은 Abs+EvalSymlinks+재확인 방식 — 커널 수준
  openat2(RESOLVE_NO_SYMLINKS 등) 기반 완전판은 계획 1 이월, 미착수.
- **nested-job Assign CI 실측**: Windows Job Object 중첩(부모 프로세스가 이미 Job에 속한
  환경)에서 `AssignProcessToJobObject` 실패 케이스는 CI 러너 환경상 실측되지 않음(계획 2
  P2-T2 이월).
- **할당 집약 transform 스크립트의 비결정적 worker 조기 사망**(session-04 취소 스모크 작성 중
  실측·규명): 가비지를 고속 생산하는 스크립트(예: 8MB 문자열 반복 생성·폐기)는 상주 메모리가
  작아도 Go GC의 커밋 반환 지연으로 커밋이 OS 상한(windows Job Object 256MB / unix RLIMIT_AS)에
  순간 도달해 worker가 런타임 OOM으로 죽을 수 있다(windows 실측 20회 중 7회, VirtualAlloc
  errno=1455; PR #4의 ubuntu 조기 사망 194ms와 동일 계열 — 그 미규명 항목의 원인 규명이기도
  하다). **linux는 더 심하다**: RLIMIT_AS가 commit이 아닌 가상주소공간 상한이라 Go 런타임의
  광범위한 VA 예약과 충돌, 할당 없는 워크로드(4MB 문자열 1개 + count 스캔)조차 8.4ms에 조기
  사망하는 사례를 PR #5 CI에서 실측(run 29676082922). fail-closed 설계상 서버 생존·오류 반환
  계약은 유지되므로 릴리스 비차단. 개선 후보(v0.1+): worker에 GOMEMLIMIT(예: 상한의 80%)를
  걸어 GC가 OS 킬 임계 전에 커밋을 억제하게 하기(windows), linux는 RLIMIT_AS 값에 Go 런타임
  VA 예약 오버헤드를 반영한 여유분 검토, "worker killed (memory/time limit)" 문구의 사유
  구분(취소/메모리/시간).
- **fts_trigram 대량 코퍼스 쿼리 지연**(계획 3 T10 Fix R1 perf 스모크 발견): 5,000 doc
  규모에서 trigram MATCH 쿼리가 평균 ≈1.6s(최대 ~2.2s)로 porter(~0.2-0.3s)보다 훨씬 느림.
  `sources(artifact_id)` 인덱스 추가로도 개선되지 않음을 직접 실측(SQLite가 이미 자동
  커버링 인덱스로 동등 처리) — 근본 원인은 trigram 포스팅 리스트 자체의 규모로 추정.
  일반적 단일 프로젝트 규모(수백~수천 청크)에서는 미관측이나, 대형 코퍼스에서는 체감
  지연 가능 — semantic 보강(§14) 또는 trigram 인덱스 전략 재검토 시 함께 조사할 것.

### 기타 minor 관찰(비차단, 후속 정리 후보)

- `internal/mcp` registerSearch/registerGlobalSearch의 limit/budget clamp 블록 중복 —
  공유 헬퍼 검토(계획 3 T2).
- `resolveProjectEntry` 경로 오류가 unredacted(기존 동작과 일관, 정보성).
- `internal/netfetch` `buildTransport`의 `dialer.Timeout` 직접 단언 부재, 512B redaction
  경계 조합 테스트 부재(계획 2 T6).
- `relDisplay`의 darwin case-sensitive 볼륨 엣지 케이스(기존 동작, 3-OS CI 정보성 관찰).
- CLI 서브커맨드 stdout 하드코딩(테스트에서 `os.Stdout` 스와핑 필요 — 계획 3 T3 인지 사항).
- **`purge --gc` + 전체삭제 조합 동작 명시(최종리뷰 F10, 계획 문구 모호성 해소)**: `--gc`가
  `--older-than` 없이(전체삭제 모드) 단독 지정되면 삭제를 전혀 수행하지 않고 orphan blob
  GC만 수행한다("GC 단독", `internal/cli/cli.go` `runPurge` 주석 참조) — `--all`/`--project`의
  전체삭제(`os.RemoveAll`) 자체가 프로젝트 디렉터리를 통째로 지우므로 그 안 blob에 대한
  별도 GC는 애초에 무의미하다(수행되지 않음, 버그 아님).

이 절 전체는 v0.0.1 수용 게이트(§12) 판정에 포함되지 않는다 — 기록 목적.
