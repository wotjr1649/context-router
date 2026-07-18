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
| 3 | **경로 시험**: symlink/junction/case-fold/UNC/git worktree/`..`탈출/워크 중 심링크/allow-path·store-root 거부 — 3 OS | `internal/ident/ident_test.go` `TestCanonicalize_GitDirAndWorktreeFile`·`TestFold_ExtendedUNC`(_SlashVariant)·`TestFold_OSRule`; `internal/ingest/ingest_test.go` `TestRun_PathEscape_Absolute`·`TestRun_PathEscape_Symlink`; `cmd/context-router/main_test.go` `TestCanonicalizeAllowPaths`(store-root 하위 거부); **신규(계획3 T10 Fix R1, windows 실측)**: `TestCanonicalize_JunctionResolvesToRealTarget` — NTFS junction(mklink /J) 경유 경로. 최초 실측에서 **실패**(Go `filepath.EvalSymlinks`가 junction=IO_REPARSE_TAG_MOUNT_POINT를 인식 못 함 — os.Lstat이 junction에 `ModeSymlink`를 안 세움, 실제 버그 발견)했고, `internal/ident/realpath_windows.go`(windows.GetFinalPathNameByHandle) + `realpath_other.go` 신설로 `Canonicalize`가 junction까지 실경로로 해석하도록 근본 수정 후 PASS로 전환(전체 스위트 회귀 없음 확인). `TestCanonicalize_CaseOnlyDirsFoldToSameIdentity`(대소문자만 다른 두 경로가 동일 identity로 귀결, windows). per-directory case-sensitivity(fsutil, admin 권한)로 "실제 대소문자만 다른 두 디렉터리"를 만드는 케이스는 **CI 러너 권한 제약 — 대기**(미구현, 의도적 보류). 3-OS 실행 = CI run(게이트 12와 동일) | PASS(신규 항목 포함, per-dir case-sensitivity만 보류) |
| 4 | **secret canary**: denylist 미색인 + span redaction(청크 경계 걸친 PRIVATE KEY 포함) 후 search/fetch 양쪽 미회수, JSON-escape·헤더 변형 포함 | `internal/ingest/ingest_test.go` `TestRedact_Canaries`(AWS/GitHub/JWT/cookie/docker-auth/password + 멀티라인 PRIVATE KEY 블록), `TestRedact_UnicodeEscapedSecret`(`\uXXXX` 은닉 탐지), `TestRun_DeniedFilename_SymlinkBypass`; `internal/mcp/mcp_test.go` `TestCtrFetchAndIndexRoundTrip`(fetch_and_index 경로 canary 미회수, search/fetch 양쪽) | PASS |
| 5 | **SSRF matrix**: I1~I8(사설/link-local/메타데이터/NAT64·CGNAT·0.0.0.0/8·v4-mapped·zone/rebinding/리터럴 redirect/강등/proxy 무시/크기 초과) | `internal/netfetch/netfetch_test.go` `TestClassifyAddr`(전 대역 테이블), `TestFetch_RedirectHopRevalidated`(hop별 재검증), `TestFetch_TooManyRedirects`, `TestTransportIgnoresEnvProxyAndHasNoRedirectFollow`, `TestFetch_MaxBytesExceededAborts`, `TestIsDowngrade`(https→http 강등) | PASS |
| 6 | **FTS 무결성·동등성**: `INSERT INTO fts(fts) VALUES('integrity-check')` 재색인·purge 후 통과, porter/trigram/BM25+RRF 스모크 + 대량 코퍼스 성능 | `internal/store/store_test.go` `TestOpen_PragmasAndSchema`(integrity-check), `TestPurgeOlderThan`(purge 후 integrity-check); `internal/search/search_test.go` `TestQuery_PorterStemsMatch`/`TestQuery_TrigramSubstringMatch`/`TestQuery_RRFRanksDualMatchTop`; `internal/search/retrieval_test.go`(코퍼스 61청크·다중 쿼리 성능 스모크, `TestCorpusShape` 가드). **신규(계획3 T10 Fix R1)**: `internal/search/perf_test.go`(`//go:build perf` 전용, CI 제외) `TestPerf_5000Docs` — 어휘 다양성 있는 합성 5,000 doc 색인 + 대표 쿼리 20회 로컬 실측(windows dev 머신, 2026-07-19): **색인 5000 docs / 24.8s(≈202 docs/s), 쿼리 20회 / 32.8s(평균 ≈1638ms/query)**. 근본 원인 실측: bm25Rank의 `rowid IN (SELECT ... WHERE EXISTS(...))` orphan 필터가 fts_trigram MATCH에서 특히 비쌈(porter는 쿼리당 ~200-300ms인데 trigram은 700ms~2.2s) — `sources(artifact_id)` 인덱스를 추가해도(SQLite가 이미 AUTOMATIC PARTIAL COVERING INDEX로 동등 처리) 유의미하게 개선되지 않음을 직접 실측 확인, 5,000-doc 규모 trigram 스캔 자체의 비용으로 판단. 회귀 문지기 아님(하드웨어 편차) — 아래 "알려진 갭"에 후속 조사 항목으로 기록 | PASS(무결성/동등성) — perf는 정보성 실측(비차단) |
| 7 | **DB 동시성·내구성**: writer1+reader4, 프로세스 2개 동시 쓰기, CAS, dedup, kill 후 무결성, user_version 비파괴 거부 — 3 OS (심층: 최초 동시 기동 WAL 경합) | `internal/store/store_test.go` `TestRegister_CASRejectsStaleWriter`, `TestRegister_DedupTwoSourcesOneArtifact`, `TestOpen_NewerVersionRefusedNonDestructively`, `TestRegister_ConcurrentDistinctBlobsNoTmpLeftover`(8-goroutine 동시쓰기, tmp 잔존 없음); `cmd/context-router/main_test.go` `TestE2E_TwoProcessConcurrentIndex`(프로세스 2개 동시 색인, 3번째 프로세스로 무결성 확인); 심층(T1): `TestOpen_ConcurrentFirstMigration`(최초 동시 기동 WAL/migration 경합 — advisory lock으로 근본 수정). **신규 심층(계획3 T10 Fix R1, windows 로컬 실측 6회 반복 안정)**: `TestRegister_TwoProcessCASRace`(실 OS 프로세스 2개가 동일 URI·상이 콘텐츠로 구지문 기반 CAS 경쟁 → 정확히 1승1패, 최종 포인터가 과거로 회귀하지 않음+integrity-check 통과), `TestOpen_SurvivesWriteKillMidLoop`(자식이 5,000회 Register 루프 중 부모가 50ms 뒤 강제 kill — 실측 2~3건 커밋 후 kill, 재오픈 quick_check=ok+integrity-check 통과+커밋된 행 전부 무결), `TestConcurrency_Writer1Reader4`(writer 1 + reader 4 고루틴, FTS MATCH+ArtifactText 연속 수행, 오류 0 — race는 CI ubuntu `-race` 잡이 커버) | PASS |
| 8 | **transform 상한**: 스텝/출력 초과, 거대 문자열·리스트 증식이 worker 메모리 상한으로 종료(서버 생존), timeout 시 트리 전멸 | `internal/transform/worker_test.go` `TestSpawn_MemoryExplosion`(메모리 상한 종료, 부모 생존), `TestSpawn_Timeout`(트리킬), `TestSpawn_DefaultTimeout`(ctx deadline 없을 때 10s 안전망); `internal/transform/transform_test.go` `TestBudgetExceeded`/`TestOutputLimitExceeded`; `internal/transform/worker_windows_test.go` `TestKillOrphan_KillsStartedProcess`(Windows 트리킬 원시 동작) | PASS |
| 9 | **추출 충실도**: 대표 HTML corpus pre/code/table 보존 판정 + fail-open 전환 | `internal/netfetch/netfetch_test.go` `TestFetch_HTML_DocWithCodeAndTable`(코드펜스·표 보존, extraction=readability), `TestFetch_HTML_CodeHeavyStripped`(pre/code 보존율<50%→fail-open), `TestFetch_HTML_ShortNonArticle`(<500자→fail-open) | PASS |
| 10 | **프로토콜 위생**: stdout 오염 0 + Claude Code·Codex 실 등록 스모크(tools/list·호출·cancellation) | 자동: `cmd/context-router/main_test.go` `TestE2E_StdioRoundTrip`(실 바이너리 stdio 왕복, 매 stdout 줄 JSON 유효성 검증), `internal/mcp/mcp_test.go` `TestServeStdoutPurity`(즉시 EOF 기준선), `TestServeStdoutPurityDuringErroringToolCall`(**신규, 계획3 T10** — store를 미리 Close해 tools/call 처리 도중 실제로 `slog.Error`가 stderr에 찍히는 그 순간에도 stdout이 JSON-RPC 줄만 유지되는지 확인, Task 8 minor "stdout purity 테스트 narrow(툴콜 중 오염 미검)" 해소). 수동(2건, 아래 §수동 스모크 참조): **사용자 확인 대기** | 자동 PASS / 수동 대기 |
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
3. 호출 도중 취소(cancellation)를 보내도 호스트·서버 모두 비정상 종료 없이 처리되는가.

### 결과 (사용자 기입)

| 호스트 | 플랫폼 | tools/list 노출(기대 도구 수 일치) | 호출 1회 | cancellation | 확인일 | 비고 |
|---|---|---|---|---|---|---|
| Claude Code | | 대기 | 대기 | 대기 | | |
| Codex | | 대기 | 대기 | 대기 | | |

## 태그 절차 (게이트 13)

전 게이트(1~12) PASS + 게이트 10 수동 스모크 2건 확인 전에는 태그를 만들지 않는다.

1. PR(`feat/v0.0.1-global-cli` → `main`) 머지.
2. `main` 브랜치 CI(`.github/workflows/ci.yml`) GREEN 확인.
3. 위 "게이트 10 — 수동 스모크" 표의 Claude Code·Codex 2건을 사용자가 실제로 수행하고 결과란을
   채운다.
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
- **oracle LICENSE 저작권자 확인 필요**: upstream `oracle`(ctxscribe/context-mode) LICENSE
  저작권자(Mert Koseoglu)와 `package.json` author 불일치 — 릴리스 전 사용자 확인 필요
  (계획 3 T7 발견, 미해결 시 태그 보류 사유가 될 수 있음 — §태그 절차 전 재확인 권장).
- **TOCTOU 완전판(openat2)**: 현재 경로 검증은 Abs+EvalSymlinks+재확인 방식 — 커널 수준
  openat2(RESOLVE_NO_SYMLINKS 등) 기반 완전판은 계획 1 이월, 미착수.
- **nested-job Assign CI 실측**: Windows Job Object 중첩(부모 프로세스가 이미 Job에 속한
  환경)에서 `AssignProcessToJobObject` 실패 케이스는 CI 러너 환경상 실측되지 않음(계획 2
  P2-T2 이월).
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

이 절 전체는 v0.0.1 수용 게이트(§12) 판정에 포함되지 않는다 — 기록 목적.
