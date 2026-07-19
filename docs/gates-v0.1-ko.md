# 게이트 체크리스트 (v0.1 session events)

설계 `docs/context-router-design-v0.1-ko.md` §9(테스트·수용 게이트, D20)의 G1~G9를
정리한 문서. v0.0.1의 13항 게이트(`docs/gates-v0.0.1-ko.md`)는 "main CI GREEN = 회귀
없음" 1줄로 승계하고 재판정하지 않는다(§9 명문) — 본 문서는 **v0.1이 새로 여는 표면만**
다룬다.

## 사전 규칙 (§9 D20)

> 호스트 의존·비결정 검증은 처음부터 비차단(정보성 + 1회 재시도 기입). 차단 게이트는
> 스크립트드-결정론 가능한 것만.

설계 §9의 G1~G9 표는 검증 방식 열이 **전부 "결정론"**으로만 채워져 있다 — v0.0.1의
게이트 10(Claude Code·Codex 실 등록 수동 스모크)에 해당하는 호스트-의존 항목이 v0.1
신규 표면에는 없다. 따라서 본 문서에는 v0.0.1과 달리 "수동 스모크" 절이 없다: G1~G9
전부가 `go test`로 재현 가능한 차단 게이트다.

## G1~G9

| # | 게이트(설계 §9 요지) | 검증 방식 | 증거(테스트) | 상태 |
|---|---|---|---|---|
| G1 | Session DB 스키마·PRAGMA·user_version·멱등 DDL | 단위 + 재오픈 | `internal/session/session_test.go` `TestOpen_ReopenIdempotent`(Open→재Open이 user_version=1 유지, 각 Open이 자기 session_id로 session_start 누적), `TestOpen_SessionStartAndSessionsRow`(sessions 행 + session_start 이벤트 자동 기록), `TestOpen_ColdStartInitLockContention`(최초 동시 생성 시 init.lock 직렬화) | PASS |
| G2 | 다중 프로세스 동시 append 무손실 | 2-프로세스 스크립트드(순수 INSERT 경합) | `internal/session/session_test.go` `TestAppend_ConcurrentAcrossConnections`(같은 dir을 여는 별도 연결 2개, 각 100건 → 총 200건·event_id 전유일 — 프로세스 경계 근사); **T10 실프로세스**: `cmd/context-router/main_test.go` `TestE2E_TwoProcessSessionLease`(실 OS 바이너리 2개가 동시에 session.db를 열고 각 20건씩 `ctr_record_event` — 양쪽 모두 성공, 3번째 프로세스의 `ctr_export_events`로 note 타입 이벤트 총 40건 무손실 확인) | PASS |
| G3 | 이벤트 3종 계약(golden + 상한 + 오류 매핑) | golden 파일(비결정 필드 event_id·ts 마스킹/고정 주입) | `internal/mcp/mcp_test.go` `TestRecordEventRoundTrip`/`TestRecordEventCapViolations`(type≤64B·summary≤2048B·attributes≤4096B·refs≤16개·총합≤8KB)/`TestRecordEventArtifactRefs`/`TestRecordEventSupersedesMissingIsNotFound`/`TestRecordEventSchemaGating`/`TestRecordEventLedgerAppend`; `TestSummary_RoundTrip`/`TestSummary_SessionIDFilter`/`TestSummary_CheckpointIncludedAndDedupedFromGroups`/`TestSummary_CheckpointBudgetOmitted`/`TestSummary_GroupTruncatedUnderBudget`/`TestSummary_MissingArtifactRef`/`TestSummary_LimitClamp`/`TestSummary_SchemaGating`/`TestSummary_LedgerAppend`; `internal/session/session_test.go` `TestSummarize_GroupsByTypeTimeDescendingAcrossSessions`/`TestSummarize_SessionIDFilter`/`TestSummarize_SupersededExcluded`/`TestSummarize_CheckpointSelection`/`TestSummarize_LimitPerTypeClamps` | PASS |
| G4 | redaction 재사용(이벤트 경로 canary — 소스는 분할 리터럴) | search/summary/export 3경로 미회수 | `internal/mcp/mcp_test.go` `TestRecordEventSecretCanaryRedacted`(summary·attributes·related_resources에 분할 리터럴 canary → 저장 행에 원문 부재, `redaction='spans'`). redaction은 **기록 경로 1회**만 적용되고(설계 §3.1 "기록 전 1회가 immutable append 아래 유일하게 건전한 시점") `ctr_search(scope=events)`·`ctr_session_summary`·`ctr_export_events` 셋 다 이 저장 행을 그대로 읽으므로, 저장 행에 원문이 없다는 이 1건이 3경로 공용으로 미회수를 보증한다(별도 read-path별 canary 중복 불요) | PASS |
| G5 | 이벤트 FTS + scope(superseded 플래그, fail-closed 시 명시 오류) | 단위 + 통합 | `internal/session/session_test.go` `TestFTSEvents_SyncOnAppend`/`TestFTSEvents_DeleteRemovesFromIndex`/`TestFTSEvents_PayloadNotIndexed`/`TestFTSEvents_IntegrityCheckPasses`; `internal/search/events_test.go` `TestQueryEvents_PorterDominant`/`TestQueryEvents_TrigramDominant`/`TestQueryEvents_SupersededFlag`; `internal/mcp/mcp_test.go` `TestSearchScopeDefaultContent`/`TestSearchScopeEvents`/`TestSearchScopeAll`/`TestSearchScopeRequiresSessionForEventsAndAll`(session.db 불용 시 STORAGE_UNAVAILABLE, 조용한 빈 결과 아님)/`TestSearchScopeInvalidValue` | PASS |
| G6 | export 준수(§26 스키마·JSONL·커서 페이지네이션) | golden + round-trip(JSONL export → 파싱 → DB 내용 대조) | `internal/session/export_test.go` `TestExport_GoldenAllFields`/`TestExport_ProducerDerivedFromSessionsRow`/`TestExport_CursorPagination`/`TestExport_IncludesSupersededEvents`/`TestExport_WireJSONShape`/`TestExport_SessionIDFilter`; `internal/mcp/mcp_test.go` `TestExportEvents_RoundTrip`/`TestExportEvents_DefaultLimitClamp`/`TestClampExportLimit`/`TestExportEvents_CursorPagination`/`TestExportEvents_MaxReturnBytesTruncatesWithoutLoss`/`TestExportEvents_IncludesSuperseded`/`TestExportEvents_SchemaGating`/`TestExportEvents_LedgerAppend`; `internal/cli/cli_test.go` `TestRunSessionExport_JSONLRoundTrip`/`TestRunSessionExport_WorktreeContract`(CLI JSONL); **T10**: `cmd/context-router/main_test.go` `TestE2E_SessionRoundTrip`(실바이너리 MCP round-trip: record→summary→export→search(scope=events), export 결과의 schemaVersion="1.0"·producer.name="context-router" 확인) | PASS |
| G7 | retention 스윕 결정론 | 시계 주입 | `internal/session/session_test.go` `TestSweep_PerSessionClockInjected`(세션 A(retention 1h)·B(미표명) 시드, now+2h → A만 삭제·B 불가침)/`TestSweep_AllUndeclaredReturnsZero`/`TestSweep_DanglingSupersedesAllowed`; `cmd/context-router/main_test.go` `TestSweepSessionRetentionAtStart_LogsCountOnSuccess`/`TestSweepSessionRetentionAtStart_LogAndContinueOnFailure`/`TestParseRetentionEventsFlag`(720h OK·30d 거부·음수 거부) | PASS |
| G8 | fail-closed + 수동 복구(lease 거부·복구 마커 중단 후 재개 포함) | 파일 바이트 훼손 + 2-프로세스 lease + 마커 잔존 시나리오(건강 DB + 마커 케이스 포함) | `internal/session/session_test.go` `TestOpen_CorruptHeaderFailsClosed`(헤더 훼손 → ErrCorrupt, family 바이트 불변)/`TestOpen_RecoverMarkerBlocks`(마커 존재 → ErrRecoverPending, lease 정상 해제); `internal/session/recover_test.go` `TestRecover_CorruptDB_RescuesAndPublishes`/`TestRecover_ResumesAfterCrashBeforePublish`/`TestRecover_ResumesAfterPublishInterruptedMidRename`/`TestRecover_RestoresFromBackupWhenTmpMissingAfterPublishCrash`/`TestRecover_HealthyDBWithLeftoverMarker_DeletesMarkerOnly`(건강 DB+마커 잔존 → 마커만 삭제)/`TestRecover_ServerRunning_RejectsImmediately`; `internal/cli/cli_test.go` `TestRunSessionRecover_HappyPath`/`TestRunSessionRecover_ServerRunning_RejectsImmediately`/`TestRunDoctor_SessionItems`; **T10 실프로세스**: `cmd/context-router/main_test.go` `TestE2E_SessionDBCorruptFailsClosed`(훼손 후 실바이너리 스폰 → 세션 3종 도구 부재·`ctr_search`(content) 정상·stderr 경고), `TestE2E_SessionRecoverMarkerBlocks`(마커 존재 스폰 → 세션 도구 부재·stderr에 `context-router session recover` 안내), `TestE2E_TwoProcessSessionLease`(shared lease 2-프로세스 공존 실증, G2와 공유 증거) | PASS |
| G9 | 스키마 토큰 예산 재기준화(게이트 11 승계) | `TestSchemaTokenBudget` 신규 기준 | `internal/mcp/mcp_test.go` `TestSchemaTokenBudget` — v0.1 기본 표면(session.db 정상 open 시 6-도구: ctr_search/ctr_fetch/ctr_transform+ctr_record_event/ctr_session_summary/ctr_export_events) 실측 **10024 bytes**, 상한 `maxToolSchemaBytes` = 10024×1.2=12028.8 → 올림 **12029 bytes**로 재기준화(커밋 13677fa, 이전 3-도구 기준 4359B/5231B·5139B/6167B는 폐기) | PASS |

## 회귀 확인

- `go test -p 1 ./...` 전체 GREEN(로컬 windows, 이 세션 — `internal/session`·`internal/mcp`·
  `internal/search`·`internal/cli`·`internal/store`·`internal/ident`·`internal/ingest`·
  `internal/netfetch`·`internal/transform`·`cmd/context-router` 전 패키지).
- `go build ./...` + `GOOS=linux go build ./...` + `GOOS=darwin go build ./...` 크로스
  컴파일 클린.
- `gofmt -l .`(testdata 고정 fixture 제외) 클린.

## v0.0.1 게이트 승계

기존 v0.0.1 13항 게이트는 재판정하지 않는다(설계 §9 명문) — "main CI GREEN = 회귀
없음" 1줄로 승계. 상세는 `docs/gates-v0.0.1-ko.md` 참조.

## 알려진 갭 (제품 동작에 영향, 릴리스 차단 아님)

- G2/G8의 2-프로세스 실증(`TestE2E_TwoProcessSessionLease`)은 각 프로세스당 20건(총
  40건) 규모로 검증했다 — T2의 `TestAppend_ConcurrentAcrossConnections`(각 100건, 고루틴
  근사)보다 이벤트 수가 적다. 실프로세스 E2E는 스폰·JSON-RPC 왕복 비용이 커 규모를
  키우면 테스트 스위트 전체 소요가 늘어나므로, "실 OS 프로세스 경계에서 lease가 실제로
  공존하는가"라는 G2·G8의 핵심 질문에 필요한 최소 규모로 잡았다(무손실 총계는 정확히
  검증). 대량 처리량은 T2의 고루틴 근사 테스트가 이미 다룬다.
- 세션 3종 도구는 registration 시점(서버 시작 1회)에만 fail-closed를 판정한다(설계
  §3.4 "fail-closed의 도구 미등록 판정은 시작 시 1회 — 가동 중 발생한 손상은 등록된
  도구의 해당 호출 실패로 표면화된다"). 가동 중 손상 시나리오는 이번 게이트 범위 밖(T3·
  T7이 이미 STORAGE_UNAVAILABLE 매핑을 커버).
- `openSessionDB`는 sentinel 3종(ErrCorrupt·ErrRecoverPending·ErrLeaseHeld)뿐 아니라 **비-
  sentinel 예기치 못한 오류도 nil+계속으로 흡수**한다(원오류는 stderr 1줄 기록) — 세션 표면만
  미등록되고 content 도구는 정상 서빙되는 가용성 트레이드오프(설계 §6.2). 최종리뷰 편승 명시.
