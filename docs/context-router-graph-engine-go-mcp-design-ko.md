# Context Router & Graph Engine
## Go 기반 MCP 전용 이중 저장소 설계 기준서

> 문서 상태: 설계 기준서 초안  
> 문서 버전: 0.1.0  
> 작성 기준일: 2026-07-17  
> 원본 브레인스토밍: `context_mode_graphify_chat_transcript_visible.docx`  
> 대상 저장소: `context-router`, `graph-engine`

---

## 0. 문서 목적

이 문서는 기존 `context-mode`와 `graphify`에 관한 브레인스토밍을 재정리하고, 최종 제품을 다음 두 개의 **독립적인 Go 저장소**로 개발하기 위한 공통 설계 기준을 정의한다.

1. **Context Router**
   - AI 코딩 에이전트의 대형 출력, 세션 이벤트, 작업 결정, 검색 가능한 작업 기록과 컨텍스트 예산을 관리한다.
2. **Graph Engine**
   - 코드·설정·SQL·테스트·문서·변경 이력을 근거 중심의 지식 그래프로 구축하고, 경로·영향도·설명·위키형 지식을 제공한다.

두 프로젝트는 목적, 데이터 수명, 처리 흐름, 보안 위험, 성능 특성이 다르므로 하나의 저장소나 하나의 제품으로 합치지 않는다.

이 문서는 두 저장소를 함께 사용할 수 있도록 상호 운용 계약도 정의하지만, 다음 원칙을 지킨다.

- 내부 코드를 공유하지 않는다.
- 내부 데이터베이스를 공유하지 않는다.
- 한 서버가 다른 서버의 내부 저장소를 직접 읽지 않는다.
- 공개되고 버전이 명시된 MCP 인터페이스와 JSON 계약으로만 연동한다.
- 기본적으로 MCP 클라이언트인 Claude Code, Codex 또는 다른 에이전트가 두 서버의 호출 순서를 조정한다.

---

# 1. 최종 확정 사항

## 1.1 확정된 상위 결정

| 항목 | 결정 |
|---|---|
| 저장소 수 | 두 개의 독립 저장소 |
| 저장소 이름 | `context-router`, `graph-engine` |
| 구현 언어 | 두 프로젝트 모두 Go |
| 외부 기능 인터페이스 | MCP 전용 |
| 기본 배포 형태 | 로컬 우선 단일 실행 파일 |
| 기본 MCP 전송 | STDIO |
| 선택적 전송 | 인증이 적용된 Streamable HTTP를 후속 단계에서 검토 |
| 직접 REST API | 제공하지 않음 |
| 사용자용 CLI 질의 기능 | 제공하지 않음 |
| 프로세스 시작 옵션 | 설정 파일, 전송 방식, 로그 수준 등 운영용 플래그만 허용 |
| 공통 데이터베이스 | 사용하지 않음 |
| 직접 서버 간 내부 호출 | 기본적으로 사용하지 않음 |
| 연동 방식 | MCP 클라이언트 조정 + 버전형 JSON 계약 |
| 초기 운영 대상 | 개인 로컬 개발 환경 |
| 초기 지원 코드 생태계 | C#/.NET 및 SQL 우선 |
| LLM 추론 | 선택 기능이며 기본적으로 비활성화 또는 격리 |
| 근거 없는 지식 저장 | 금지 |

## 1.2 기존 브레인스토밍에서 폐기된 결정

다음 제안은 새로운 사용자 결정으로 대체한다.

- Graph Engine의 Rust 코어 제안은 폐기한다.
- Context Router의 TypeScript 유지 제안은 폐기한다.
- Rust 코어 + TypeScript 어댑터의 다중 언어 구조는 사용하지 않는다.
- Python 기반 Graphify 호환 코어를 장기간 유지하는 구조는 사용하지 않는다.
- 두 프로젝트를 하나의 GraphMesh 플랫폼으로 합치는 구조는 사용하지 않는다.
- 별도의 세 번째 통합 저장소는 초기 범위에서 만들지 않는다.

## 1.3 아직 최종 선택이 필요한 항목

| 항목 | 기본 권장안 | 최종 결정 필요 시점 |
|---|---|---|
| 실제 fork/port 또는 독립 재구현 | 독립 Go 재구현 우선 | 코드 작성 전 |
| CGO 허용 여부 | 파서 정확도를 위해 제한적 허용 검토 | 파서 PoC 전 |
| SQLite 구현체 | 저장소 인터페이스 뒤에 격리 | 저장소 PoC 전 |
| FTS5 사용 여부 | 가능하면 사용, 대체 검색 백엔드 준비 | 검색 PoC 전 |
| Graph Engine 내부 LLM 호출 | 기본 비활성화 | Wiki 단계 전 |
| HTTP MCP 제공 | STDIO 안정화 이후 | 팀 공유 단계 전 |
| 팀 공유 서버 | MVP 제외 | 로컬 제품 검증 이후 |
| 벡터 검색 | MVP 제외 | 그래프·텍스트 검색 품질 평가 이후 |

---

# 2. 핵심 개념과 역할 분리

## 2.1 한 문장 정의

### Context Router

> AI 코딩 에이전트가 큰 파일, 빌드 로그, 테스트 출력, 명령 결과와 긴 세션 기록을 모델 컨텍스트에 그대로 넣지 않고, 외부 저장·검색·부분 조회·세션 복구를 통해 필요한 정보만 전달하도록 하는 MCP 컨텍스트 관리 서버다.

### Graph Engine

> 코드베이스의 파일, 심볼, 호출, 상속, SQL, 설정, 테스트, 문서, 변경 이력과 근거를 연결한 살아 있는 지식 그래프를 구축하고, MCP를 통해 구조 탐색·경로·영향도·설명·위키형 ContextPack을 제공하는 서버다.

## 2.2 역할 비교

| 구분 | Context Router | Graph Engine |
|---|---|---|
| 핵심 목적 | 현재 작업 컨텍스트 절약과 세션 연속성 | 장기적인 코드베이스 구조·근거 지식 관리 |
| 주 데이터 | 명령 출력, 파일 조각, 세션 이벤트, 결정, 오류, 테스트 결과 | 파일, 심볼, 관계, 증거, claim, commit, wiki page |
| 데이터 수명 | 세션 중심, 보존 정책에 따라 정리 | 저장소와 commit 중심, 장기 보존 |
| 대표 질의 | “이전 테스트 실패 출력에서 오류 부분만 찾아줘” | “이 메서드를 바꾸면 어떤 API와 테이블이 영향받나?” |
| 핵심 검색 | 전문 검색, artifact 조각 조회 | 그래프 탐색 + 전문 검색 + 근거 랭킹 |
| 코드 파싱 | 하지 않음 | 핵심 기능 |
| 명령 실행 | 안전한 실행 도구를 통해 지원 | 원칙적으로 하지 않음 |
| 세션 이벤트 | 원본 생성·보관 | 명시적 import 후 지식 근거로 사용 |
| Wiki 기능 | 하지 않음 | 후속 핵심 기능 |
| 변경 영향도 | 하지 않음 | 핵심 기능 |
| 컨텍스트 예산 | 핵심 기능 | ContextPack 생성 시 적용 |

## 2.3 둘을 합치지 않는 이유

1. **책임이 다르다.**
   - Context Router는 에이전트 실행 중 발생하는 단기·중기 작업 기록을 다룬다.
   - Graph Engine은 저장소 구조와 장기 지식을 다룬다.
2. **보안 모델이 다르다.**
   - Context Router는 명령 실행과 원시 출력 보관 때문에 높은 실행 권한 위험이 있다.
   - Graph Engine은 신뢰할 수 없는 저장소 내용, 경로, 파서, 그래프 오염 위험이 중심이다.
3. **갱신 방식이 다르다.**
   - Context Router는 append 중심 이벤트 저장이다.
   - Graph Engine은 파일·commit 변경에 따른 증분 재구축이 중심이다.
4. **성능 병목이 다르다.**
   - Context Router는 대형 텍스트 저장·검색·부분 반환이 중요하다.
   - Graph Engine은 파싱·관계 해석·그래프 순회가 중요하다.
5. **장애 격리가 필요하다.**
   - Context Router의 실행기 오류가 그래프 색인을 중단시키지 않아야 한다.
   - Graph Engine의 대형 색인이 세션 도구 응답을 지연시키지 않아야 한다.

---

# 3. 전체 시스템 구조

## 3.1 권장 구조

```text
Claude Code / Codex / 기타 MCP Client
                  │
          ┌───────┴────────┐
          │                │
          ▼                ▼
  Context Router       Graph Engine
  Go MCP Server        Go MCP Server
          │                │
          │                ├─ Source Graph
          │                ├─ Evidence Graph
          │                ├─ Concept Graph
          │                ├─ Wiki Graph
          │                └─ Temporal Graph
          │
          ├─ Artifact Store
          ├─ Session Event Store
          ├─ Text Search Index
          └─ Context Budget Manager
```

## 3.2 상호 운용 흐름

```text
1. 에이전트가 context_execute를 호출한다.
2. Context Router가 전체 출력을 저장하고 짧은 결과만 반환한다.
3. 에이전트가 필요한 경우 context_search/context_fetch로 세부 조각을 조회한다.
4. Context Router가 세션 이벤트를 SessionEvent v1으로 export한다.
5. 에이전트가 graph_import_events를 호출해 선택된 이벤트를 Graph Engine에 전달한다.
6. Graph Engine이 저장소 그래프와 세션 근거를 함께 탐색한다.
7. Graph Engine이 ContextPack을 반환한다.
8. 에이전트는 ContextPack을 직접 사용하거나 Context Router에 artifact로 저장한다.
```

## 3.3 서버 간 호출 원칙

기본 설계에서는 Context Router와 Graph Engine이 서로 MCP 클라이언트가 되지 않는다.

```text
허용:
MCP Client → Context Router
MCP Client → Graph Engine
MCP Client가 한 서버의 결과를 다른 서버에 전달

금지:
Context Router → Graph Engine → Context Router 재귀 호출
Graph Engine이 Context Router DB 직접 조회
Context Router가 Graph Engine DB 직접 조회
```

이 원칙은 순환 호출, 장애 전파, 권한 상승, 버전 결합을 예방한다.

---

# 4. MCP 전용 제품 원칙

## 4.1 MCP 전용의 의미

두 프로젝트에서 “MCP 전용”은 다음을 의미한다.

- 모든 사용자 기능은 MCP tool, resource, prompt 중 하나로 제공한다.
- 별도의 REST 비즈니스 API를 제공하지 않는다.
- 별도의 사용자용 CLI 명령 체계를 제공하지 않는다.
- 웹 UI를 MVP에 포함하지 않는다.
- 실행 파일은 MCP 서버 프로세스를 시작하기 위한 운영 플래그만 가진다.

허용 가능한 실행 예시는 다음과 같다.

```bash
context-router --config ./context-router.yaml --transport stdio

graph-engine --config ./graph-engine.yaml --transport stdio
```

다음과 같은 사용자 질의 CLI는 만들지 않는다.

```bash
# 만들지 않음
graph-engine query "authentication flow"
context-router search "test failure"
```

해당 기능은 각각 `graph_query`, `context_search` MCP tool로만 제공한다.

## 4.2 MCP 전용으로 인한 중요한 제한

MCP 서버만으로는 Claude Code나 Codex가 제공하는 네이티브 `Read`, `Bash`, `WebFetch` 결과를 투명하게 가로채거나 자동 재작성할 수 없다.

따라서 Context Router는 다음 방식으로 사용한다.

1. 에이전트가 큰 명령을 네이티브 셸 도구 대신 `context_execute`로 실행한다.
2. 큰 파일이나 산출물은 `context_index`로 색인한다.
3. 필요한 조각만 `context_search`와 `context_fetch`로 읽는다.
4. `CLAUDE.md`, `AGENTS.md` 또는 MCP 서버 instructions에 우선 사용 규칙을 명시한다.

권장 에이전트 규칙 예시는 다음과 같다.

```md
- 큰 명령 출력은 네이티브 Bash 대신 Context Router의 context_execute를 우선 사용한다.
- 전체 파일을 반복해서 읽지 말고 context_index 이후 context_search/context_fetch를 사용한다.
- 아키텍처, 교차 파일 관계, 영향도 분석은 Graph Engine을 먼저 사용한다.
- Graph Engine의 추론 관계는 소스와 테스트 근거를 확인한 뒤 수정 근거로 사용한다.
```

자동 훅과 플랫폼 전용 플러그인이 필요해지는 경우에도 핵심 Go 저장소와 분리된 얇은 배포 설정으로 다루며, MVP 핵심 범위에는 포함하지 않는다.

---

# Part A. Context Router

# 5. Context Router 개요

## 5.1 목표

Context Router의 목표는 모델의 컨텍스트 창을 대형 원시 데이터로 채우지 않으면서도, 필요할 때 정확한 원문 조각을 다시 찾을 수 있게 하는 것이다.

핵심 가치:

- 원시 출력을 모델 컨텍스트 밖에 저장한다.
- 에이전트에는 짧고 구조화된 결과만 반환한다.
- 전문 검색으로 필요한 조각만 다시 조회한다.
- 사용자 결정, 제약, 파일 변경, 테스트와 오류를 세션 이벤트로 보존한다.
- compaction 이후에도 현재 작업 상태를 복원할 수 있게 한다.
- 실제 절약량과 검색 사용량을 측정한다.

## 5.2 책임 범위

Context Router가 담당한다.

- 안전한 명령 실행과 전체 stdout/stderr 저장
- 대형 파일·텍스트·도구 출력의 artifact 저장
- content-addressed artifact 식별
- 텍스트 청크 분할과 검색 색인
- 부분 조회와 범위 조회
- 세션 생성·종료·복구
- 세션 이벤트 append
- 사용자 요구사항과 제약 기록
- 결정과 거절된 접근 기록
- 파일 변경 및 Git diff 요약 기록
- 빌드·테스트 실행 기록
- 오류와 실패 원인 조각 저장
- compaction용 세션 요약
- 컨텍스트 예산 계산
- 민감 정보 탐지·마스킹
- SessionEvent 계약 export
- 통계·진단·보존 정책

## 5.3 비책임 범위

Context Router는 다음을 하지 않는다.

- 전체 저장소 AST 파싱
- call graph 생성
- 상속·구현 관계 해석
- SQL 테이블 의존성 그래프 구축
- 저장소 Wiki 생성
- commit별 그래프 diff
- 코드 변경 영향도 계산
- Graph Engine 내부 데이터베이스 조회
- 임의의 Graph Engine tool 자동 호출
- 에이전트 네이티브 도구 결과의 투명한 자동 가로채기

---

# 6. Context Router 핵심 데이터 모델

## 6.1 Session

```text
Session
- session_id
- agent_kind
- workspace_root
- started_at
- ended_at
- status
- current_goal
- active_constraints
- last_compaction_at
- retention_policy
```

## 6.2 SessionEvent

초기 이벤트 종류:

```text
user_instruction
constraint
decision
rejected_approach
tool_call
tool_result_summary
artifact_created
file_edit
git_diff
build_run
test_run
error
warning
compaction_summary
session_checkpoint
```

이벤트는 immutable append를 기본으로 하고, 잘못된 이벤트는 삭제보다 `supersedes` 또는 `invalidates` 관계로 교정한다.

## 6.3 Artifact

```text
Artifact
- artifact_id
- content_hash
- session_id
- media_type
- byte_length
- storage_path
- compression
- created_at
- source
- privacy_label
- redaction_status
- retention_until
```

## 6.4 TextChunk

```text
TextChunk
- chunk_id
- artifact_id
- ordinal
- byte_start
- byte_end
- line_start
- line_end
- text
- token_estimate
- search_metadata
```

## 6.5 ContextBudget

```text
ContextBudget
- max_tokens
- reserved_answer_tokens
- summary_tokens
- snippet_tokens
- metadata_tokens
- over_budget_policy
```

---

# 7. Context Router 처리 흐름

## 7.1 대형 명령 실행

```text
context_execute 요청
→ 입력 검증
→ workspace/command 정책 확인
→ 프로세스 실행
→ stdout/stderr 스트리밍 캡처
→ 크기 제한 및 시간 제한 적용
→ 원문 artifact 저장
→ 청크 분할 및 검색 색인
→ 오류·경고·head/tail 중심의 결정론적 요약
→ artifact URI와 요약만 반환
```

기본 반환에는 전체 출력이 포함되지 않는다.

반환 예시:

```json
{
  "status": "failed",
  "exitCode": 1,
  "summary": "dotnet test가 3개 테스트 실패로 종료되었습니다.",
  "highlights": [
    "Lib.Db.Tests.PostgreSqlParameterTests failed",
    "NpgsqlException: connection timeout"
  ],
  "artifactUri": "artifact://session-123/sha256-abcd",
  "storedBytes": 824513,
  "returnedBytes": 1932,
  "truncated": true,
  "nextActions": [
    "context_search로 테스트 이름 또는 예외 문자열을 검색"
  ]
}
```

## 7.2 검색과 부분 조회

```text
context_search
→ 세션·artifact·기간·종류 필터
→ 전문 검색
→ score 기반 snippet 선택
→ 중복 조각 제거
→ 토큰 예산 내 결과 반환
```

`context_fetch`는 정확한 artifact 범위, line 범위 또는 chunk ID를 조회한다.

## 7.3 세션 복구

```text
최근 checkpoint
+ 미결정 제약
+ 최근 결정
+ 수정된 파일
+ 실패한 빌드/테스트
+ 열린 오류
+ 다음 작업
→ context_session_summary 반환
```

요약은 원본 이벤트를 대체하지 않는다. 각 요약 항목은 이벤트 URI를 근거로 가진다.

## 7.4 Graph Engine 연동 이벤트 export

Context Router는 선택된 이벤트를 `session-event.v1` 묶음으로 반환한다.

- 모든 도구 호출을 무조건 Graph Engine에 넣지 않는다.
- 의미 있는 결정, 제약, 테스트 결과, 오류, 파일 변경만 선택할 수 있어야 한다.
- raw stdout 전체를 Graph Engine에 복사하지 않고 artifact URI와 핵심 evidence만 전달한다.

---

# 8. Context Router MCP 인터페이스

## 8.1 MVP tools

| Tool | 목적 |
|---|---|
| `context_execute` | 승인된 명령을 실행하고 전체 출력을 외부 저장 |
| `context_index` | 파일·텍스트·기존 산출물을 artifact로 등록하고 색인 |
| `context_search` | artifact와 세션 이벤트 검색 |
| `context_fetch` | 특정 artifact의 정확한 범위 조회 |
| `context_record_event` | 결정·제약·오류 등 세션 이벤트 기록 |
| `context_session_summary` | 현재 세션의 복구용 요약 생성 |
| `context_export_events` | Graph Engine에서 소비할 이벤트 묶음 생성 |
| `context_stats` | 저장량·반환량·절약량·검색 통계 |
| `context_health` | 저장소, 검색, 실행기 상태 진단 |

## 8.2 후속 tools

| Tool | 목적 |
|---|---|
| `context_batch_execute` | 독립적인 명령을 제한된 동시성으로 실행 |
| `context_compare_artifacts` | 두 출력이나 파일의 차이 요약 |
| `context_checkpoint` | 명시적 세션 체크포인트 생성 |
| `context_retention_apply` | 보존 정책에 따라 정리 |
| `context_redact` | 기존 artifact의 민감 정보 마스킹 |

## 8.3 resources

```text
session://<session-id>
session://<session-id>/events
artifact://<session-id>/<content-hash>
artifact://<session-id>/<content-hash>#L<start>-L<end>
context://stats
context://health
```

## 8.4 prompts

MVP 이후 다음 MCP prompt를 제공할 수 있다.

```text
recover_current_session
inspect_test_failure
summarize_large_artifact
prepare_graph_event_export
```

---

# 9. Context Router 저장소 설계

## 9.1 권장 저장 구조

```text
.context-router/
├─ metadata.db
├─ artifacts/
│  └─ <content-hash>
├─ indexes/
├─ sessions/
└─ locks/
```

## 9.2 데이터 저장 원칙

- SQLite 계열 저장소를 기본 메타데이터 저장소로 검토한다.
- 검색 백엔드는 인터페이스로 분리한다.
- FTS5를 선택할 수 있지만 구현체와 배포 제약을 코어 로직에 누출하지 않는다.
- 원문 artifact는 데이터베이스 BLOB보다 content-addressed 파일로 저장한다.
- 압축 여부와 검색용 평문 청크는 분리할 수 있다.
- 동일한 content hash는 중복 저장하지 않는다.
- artifact 삭제 시 참조 카운트와 세션 보존 정책을 확인한다.

## 9.3 검색 인터페이스

```go
type SearchIndex interface {
    Index(ctx context.Context, chunks []TextChunk) error
    Search(ctx context.Context, query SearchQuery) ([]SearchHit, error)
    DeleteArtifact(ctx context.Context, artifactID string) error
    Health(ctx context.Context) error
}
```

## 9.4 저장소 인터페이스

```go
type SessionStore interface {
    CreateSession(ctx context.Context, session Session) error
    AppendEvent(ctx context.Context, event SessionEvent) error
    ListEvents(ctx context.Context, filter EventFilter) ([]SessionEvent, error)
    SaveCheckpoint(ctx context.Context, checkpoint Checkpoint) error
}

type ArtifactStore interface {
    Put(ctx context.Context, meta Artifact, r io.Reader) (ArtifactRef, error)
    Open(ctx context.Context, ref ArtifactRef) (io.ReadCloser, error)
    ReadRange(ctx context.Context, ref ArtifactRef, start, length int64) ([]byte, error)
    Delete(ctx context.Context, ref ArtifactRef) error
}
```

---

# 10. Context Router Go 패키지 구조

폴더를 처음부터 과도하게 세분화하지 않는다. 다음은 MVP 기준 최소 구조다.

```text
context-router/
├─ cmd/
│  └─ context-router/
│     └─ main.go
├─ internal/
│  ├─ app/          # 조립, lifecycle, config
│  ├─ mcp/          # tools/resources/prompts, MCP transport
│  ├─ context/      # session, event, budget, summary
│  ├─ artifact/     # 저장, chunk, fetch, retention
│  ├─ execute/      # 안전한 프로세스 실행
│  ├─ search/       # 전문 검색 추상화
│  ├─ store/        # metadata persistence
│  └─ security/     # path, command, secret, redaction policy
├─ schemas/
│  └─ session-event.v1.schema.json
├─ testdata/
├─ docs/
├─ go.mod
└─ README.md
```

패키지가 비대해질 때만 추가 분리한다. 초기부터 `domain`, `application`, `infrastructure`를 여러 계층으로 중복 생성하지 않는다.

---

# 11. Context Router 보안 요구사항

## 11.1 명령 실행

- 셸 문자열보다 `executable + argv` 구조를 기본으로 한다.
- `cmd.exe /c`, `powershell -Command`, `sh -c`는 기본 차단하거나 별도 위험 권한이 필요하다.
- 허용 workspace root 밖의 cwd를 금지한다.
- symlink와 junction을 해석한 최종 경로를 검증한다.
- 실행 시간, 출력 크기, 동시 프로세스 수를 제한한다.
- 환경 변수 전달은 allowlist 또는 denylist 정책을 적용한다.
- 입력에 포함된 비밀 값이 로그에 다시 기록되지 않도록 한다.
- 네트워크 접근과 파일 쓰기 권한을 도구 요청에서 명시적으로 표시한다.

## 11.2 Artifact와 검색

- 토큰, 비밀번호, 연결 문자열, 개인 정보 후보를 탐지한다.
- raw artifact와 redacted search text를 분리할 수 있어야 한다.
- 검색 결과에 `privacyLabel`을 포함한다.
- 보존 기한과 삭제 정책을 설정할 수 있어야 한다.
- artifact URI만으로 workspace 밖의 파일을 읽을 수 없어야 한다.

## 11.3 Prompt injection 방어

도구 출력과 파일 내용은 신뢰할 수 없는 데이터로 취급한다.

- 파일 안의 “이 지시를 따르라” 같은 문자열을 시스템 명령으로 해석하지 않는다.
- 반환 결과에 `untrustedContent: true`를 표시할 수 있다.
- 산출물에서 발견한 명령을 자동 실행하지 않는다.

---

# 12. Context Router MVP 완료 기준

다음 조건을 충족하면 1차 MVP로 본다.

1. STDIO MCP 서버로 정상 실행된다.
2. `context_execute`가 대형 출력을 원문 그대로 반환하지 않고 artifact로 저장한다.
3. 100MB 이상의 테스트 출력에서도 메모리 사용이 제한된다.
4. `context_search`가 오류 문자열과 테스트 이름을 검색한다.
5. `context_fetch`가 정확한 line 범위를 반환한다.
6. 세션 이벤트가 append되고 재시작 후 복구된다.
7. 결정, 제약, 테스트 실패를 포함한 세션 요약을 생성한다.
8. `session-event.v1`을 export한다.
9. workspace 탈출, symlink 탈출, 위험 셸 호출 테스트가 통과한다.
10. 저장한 bytes와 모델에 반환한 bytes를 측정한다.

---

# Part B. Graph Engine

# 13. Graph Engine 개요

## 13.1 목표

Graph Engine은 저장소를 단순 문자열 검색 대상이 아니라, 코드·데이터·테스트·설계 근거가 연결된 지식망으로 변환한다.

핵심 가치:

- 소스에서 결정론적으로 추출한 구조를 우선한다.
- 모든 중요한 관계와 claim에 근거를 연결한다.
- 현재 commit과 branch를 인식한다.
- 변경된 부분만 증분 갱신한다.
- 질문과 토큰 예산에 맞는 ContextPack을 반환한다.
- 추론 관계와 실제 추출 관계를 구분한다.
- 코드 변경 시 stale wiki claim을 탐지한다.

## 13.2 책임 범위

Graph Engine이 담당한다.

- 저장소 스캔과 ignore 규칙
- 파일 fingerprint 및 commit 인식
- C#/.NET 구조 파싱
- SQL 및 다중 RDBMS 아티팩트 파싱
- 설정·프로젝트 파일 파싱
- 파일·심볼·API·테이블·테스트 노드 생성
- import/call/inheritance/implementation 관계 생성
- SQL read/write/execute 관계 생성
- 근거 URI와 source span 생성
- 그래프 경로 탐색
- 노드 설명
- 변경 영향도 분석
- commit/branch 그래프 diff
- evidence-backed claim 관리
- 위키 페이지와 stale 상태 관리
- 세션 이벤트 import
- ContextPack 생성
- MCP tool/resource 제공

## 13.3 비책임 범위

Graph Engine은 다음을 하지 않는다.

- 임의 명령 실행
- 대형 stdout/stderr 원문 보관
- 범용 세션 저장소 역할
- Context Router 데이터베이스 직접 조회
- Claude Code/Codex 네이티브 도구 가로채기
- 모든 언어를 MVP에서 지원
- 모든 관계를 LLM으로 생성
- evidence가 없는 추론을 확정 사실로 저장
- SaaS 또는 팀 공유 서버를 MVP에서 제공

---

# 14. Graph Engine 지식 모델

물리적으로는 하나의 통합 node/edge/evidence 모델을 사용하고, 논리적으로 다섯 레이어를 구분한다.

## 14.1 Source Graph

실제 저장소 구조와 코드 관계다.

대표 node:

```text
Repository
Commit
Branch
Directory
File
Solution
Project
Package
Namespace
Class
Struct
Interface
Enum
Method
Function
Property
Field
Endpoint
Table
Column
View
StoredProcedure
DatabaseFunction
Trigger
Index
ConfigKey
Dependency
Test
```

대표 edge:

```text
contains
defines
imports
references
calls
inherits
implements
reads_from
writes_to
executes_sql
queries_table
handles_route
uses_config
depends_on
tested_by
```

이 레이어는 가능한 한 LLM 없이 생성한다.

## 14.2 Concept Graph

사람이 이해하는 업무·아키텍처 개념을 표현한다.

대표 node:

```text
Authentication
Authorization
Shipment Label Issuance
Sorter Routing
Retry Policy
Provider Abstraction
Transaction Boundary
Data Retention
```

대표 edge:

```text
implemented_by
depends_on
conflicts_with
documented_in
related_to
owned_by
```

Concept Graph는 다음 출처로 생성할 수 있다.

1. 명시적인 사람이 작성한 mapping
2. 문서와 이름 규칙을 이용한 정적 추출
3. MCP 클라이언트 LLM이 제안한 candidate
4. 선택적 내부 LLM enrichment

3번과 4번은 반드시 `INFERRED_LLM` 상태로 저장하며 자동 확정하지 않는다.

## 14.3 Evidence Graph

모든 설명과 claim의 근거를 표현한다.

대표 node:

```text
SourceSpan
CodeSnippet
DocChunk
ConfigEntry
SqlStatement
TestResult
BuildResult
LogExcerpt
Decision
UserInstruction
AgentFinding
```

대표 edge:

```text
evidence_for
derived_from
contradicts
supersedes
validated_by
invalidated_by
```

## 14.4 Wiki Graph

사람이 읽는 지식 페이지를 표현한다.

대표 node:

```text
WikiPage
Section
Claim
FAQ
HowTo
ArchitectureNote
Runbook
GlossaryEntry
```

대표 edge:

```text
contains_claim
links_to
backlinks_to
summarizes
has_evidence
stale_due_to
```

Wiki는 Markdown 파일 하나를 진실의 원천으로 삼지 않는다. 내부 진실의 원천은 `Page → Section → Claim → EvidenceRef` 구조다. Markdown은 렌더링 결과다.

## 14.5 Temporal Graph

시간과 버전 변화를 표현한다.

대표 node:

```text
GraphSnapshot
CommitDelta
PageRevision
ClaimRevision
DecisionRevision
ExtractorVersion
```

대표 edge:

```text
created_in
changed_in
deleted_in
supersedes
regressed_by
fixed_by
stale_due_to
```

## 14.6 Flow와 hyperedge-like entity

실제 시스템 흐름은 단순한 `A → B` edge 하나로 표현하기 어렵다. 여러 참여자와 순서가 함께 필요한 관계는 독립적인 flow node로 모델링한다.

대표 node:

```text
Flow
Transaction
RequestPath
DataPipeline
Workflow
Decision
```

예시:

```text
Flow: Shipment Label Issuance
├─ actor → Operator
├─ starts_at → IssueLabelCommand
├─ calls → CarrierApiClient
├─ writes_to → ShipmentTable
├─ emits → LabelIssuedEvent
└─ verified_by → LabelIssueIntegrationTest
```

flow node에는 단계 순서, 참여 node, 성공·실패 분기, transaction boundary와 evidence를 연결한다. 내부 저장소가 hyperedge를 직접 지원하지 않더라도 flow 자체를 node로 승격해 일반 edge로 표현한다.

---

# 15. 신뢰도와 provenance 모델

## 15.1 신뢰도 분류

```text
HUMAN_VERIFIED
EXTRACTED_AST
EXTRACTED_PROJECT
EXTRACTED_CONFIG
EXTRACTED_SCHEMA
EXTRACTED_DOC
OBSERVED_TEST
OBSERVED_BUILD
OBSERVED_RUNTIME
INFERRED_STATIC
INFERRED_LLM
HUMAN_REJECTED
CONFLICTED
STALE
```

## 15.2 우선순위

기본 우선순위는 다음과 같다.

```text
HUMAN_VERIFIED
> EXTRACTED_AST / EXTRACTED_PROJECT / EXTRACTED_SCHEMA
> OBSERVED_TEST / OBSERVED_BUILD / OBSERVED_RUNTIME
> EXTRACTED_DOC
> INFERRED_STATIC
> INFERRED_LLM
```

단, 테스트나 문서도 오래되거나 잘못될 수 있으므로 confidence만으로 확정하지 않는다. freshness와 conflict 상태를 함께 평가한다.

## 15.3 모든 관계와 claim에 필요한 메타데이터

```text
extractor_name
extractor_version
source_uri
source_span
repository_id
commit_hash
branch
created_at
updated_at
confidence
freshness_status
review_status
model_id            # LLM 사용 시에만
prompt_hash         # LLM 사용 시에만
content_digest
privacy_label
```

---

# 16. Graph Engine 색인 파이프라인

## 16.1 최초 색인

```text
Repository 등록
→ workspace root 검증
→ Git metadata 확인
→ ignore 규칙 적용
→ 파일 분류
→ 파일 fingerprint 계산
→ 언어별 parser 선택
→ AST/구조 추출
→ symbol table 생성
→ cross-file 관계 해석
→ SQL/설정/테스트 관계 해석
→ EvidenceRef 생성
→ node/edge 저장
→ 검색 색인 구축
→ adjacency index 생성
→ snapshot 기록
```

## 16.2 증분 갱신

```text
현재 snapshot과 Git/file fingerprint 비교
→ 추가/변경/삭제 파일 계산
→ 영향받은 parser 결과 제거
→ 변경 파일 재파싱
→ 관련 cross-file edge 재해석
→ 영향받은 claim과 wiki page stale 처리
→ 새 snapshot 기록
```

전체 재색인은 명시적으로 요청하거나 schema/extractor 버전이 호환되지 않을 때만 수행한다.

## 16.3 파일 분류

초기 분류 예시:

```text
source
project
config
sql
test
document
generated
vendor
binary
unknown
```

`generated`, `vendor`, 대형 binary는 기본 제외한다.

---

# 17. 초기 언어 및 생태계 우선순위

사용자의 실제 개발 환경을 기준으로 다음 순서를 사용한다.

## 17.1 1순위: C#/.NET

지원 대상:

```text
.cs
.sln
.csproj
Directory.Build.props
Directory.Build.targets
packages.lock.json
appsettings*.json
launchSettings.json
```

추출 대상:

```text
namespace
class/struct/interface/enum
method/property/field
generic type
attribute
using/import
inheritance
interface implementation
method call
constructor dependency
ASP.NET Core controller/endpoint
DI registration
WPF View/ViewModel 관계
WinForms form/control event
async call chain
test fixture/test case
NuGet dependency
project reference
```

Go 단일 구현 원칙상 초기에는 Roslyn 기반 별도 서비스에 의존하지 않는다. Tree-sitter 계열 파서 또는 Go에서 사용할 수 있는 문법 파서를 추상화 뒤에 둔다. 정확도가 부족한 semantic 영역은 confidence를 낮추고 evidence와 함께 반환한다.

## 17.2 2순위: SQL 및 다중 RDBMS

대상:

```text
SQL Server
MySQL/MariaDB
PostgreSQL
Oracle
```

추출 대상:

```text
table
column
view
stored procedure
function
trigger
index
foreign key
DDL object reference
SELECT read dependency
INSERT/UPDATE/DELETE write dependency
procedure call
raw SQL string
parameter binding
provider-specific branch
migration
```

목표 관계 예시:

```text
C# Method
  └─ executes_sql
      └─ SQL Statement
          ├─ reads_from → Table
          └─ writes_to → Table
```

## 17.3 3순위: 설정과 배포

```text
JSON
XML
YAML
Dockerfile
Docker Compose
GitHub Actions
.env template
```

## 17.4 후속 언어

```text
TypeScript/JavaScript
Python
Go 자체 분석
기타 실제 저장소 수요가 확인된 언어
```

모든 언어를 동시에 지원하지 않는다.

---

# 18. Graph query 모델

## 18.1 Query modes

| Mode | 목적 |
|---|---|
| `local` | 특정 파일·심볼·오류와 가까운 지식 |
| `path` | 두 엔터티 사이의 연결 경로 |
| `impact` | 파일·심볼·테이블 변경 영향 |
| `global` | 저장소 전체 구조와 핵심 허브 |
| `wiki` | 사람이 읽기 쉬운 페이지와 claim 중심 |
| `debug` | 테스트·오류·세션 이벤트를 포함한 디버깅 |
| `mix` | 그래프, wiki, source evidence를 함께 사용 |

## 18.2 자연어 질의 처리

Graph Engine은 내부 LLM 없이도 다음의 결정론적 단계를 지원해야 한다.

```text
질문 토큰화
→ 파일/심볼/개념 검색
→ 후보 entity ranking
→ query mode 선택 또는 요청값 사용
→ neighborhood/path/impact 탐색
→ evidence expansion
→ freshness/conflict 검사
→ token budget packing
→ ContextPack 반환
```

자연어 의미 해석이 부족한 경우 MCP 클라이언트가 명시적인 entity 후보를 함께 전달할 수 있다.

```json
{
  "question": "Oracle provider 변경 영향 범위를 설명해줘",
  "mode": "impact",
  "entityHints": [
    "symbol://csharp/Lib.Db.OracleProvider"
  ],
  "maxTokens": 2200
}
```

## 18.3 경로 탐색

- 기본 BFS는 unweighted relation에 사용한다.
- 관계 신뢰도, freshness, 타입별 비용을 반영한 weighted shortest path를 지원한다.
- 너무 일반적인 허브 node는 penalty를 적용한다.
- 같은 의미의 반복 경로는 deduplicate한다.
- 반환 경로마다 각 edge의 evidence를 포함한다.

## 18.4 영향도 분석

영향도는 확정적 예측이 아니라 근거 기반 후보 집합이다.

분류 예시:

```text
direct
transitive
test
schema
configuration
runtime-hypothesis
```

각 결과에 다음을 포함한다.

```text
impact_reason
path
evidence
confidence
freshness
verification_suggestion
```

---

# 19. Wiki형 지식망

## 19.1 목표

Graph Engine을 단순 코드 그래프 도구가 아니라 “Wiki LLM식 지식 그물망”에 가깝게 만드는 핵심은 페이지 자체가 아니라 **검증 가능한 claim과 evidence의 연결**이다.

## 19.2 자동 생성 대상

```text
Architecture Overview
Project Structure
Data Model
API Surface
Dependency Injection
Database Access
Stored Procedures
Background Jobs
Error Handling
Testing Strategy
Deployment
Glossary
Risky Couplings
Recently Changed Concepts
```

## 19.3 페이지 내부 구조

```text
WikiPage
└─ Section
   └─ Claim
      ├─ EvidenceRef[]
      ├─ Confidence
      ├─ Freshness
      ├─ LastVerifiedCommit
      ├─ ReviewStatus
      └─ Contradictions[]
```

## 19.4 stale detector

```text
Source file 변경
→ 관련 Symbol 변경
→ Symbol을 근거로 하는 Claim 조회
→ Claim을 STALE로 표시
→ 관련 WikiPage를 PARTIALLY_STALE로 표시
→ 다음 질의에 경고 포함
```

## 19.5 Human override

사람이 검증한 관계와 claim은 추론보다 우선한다.

```text
INFERRED_LLM claim은 삭제하지 않고 HUMAN_REJECTED로 표시 가능
HUMAN_VERIFIED claim이 동일 범위의 추론보다 우선
서로 다른 verified claim이 충돌하면 CONFLICTED로 표시
```

## 19.6 내부 LLM 정책

MVP에서는 내부 LLM provider를 넣지 않는다.

권장 단계:

1. 결정론적 Source/Evidence Graph 완성
2. 템플릿 기반 Wiki 생성
3. MCP 클라이언트가 생성한 candidate claim을 별도 tool로 제출
4. 품질 평가 후 선택적 내부 enrichment 검토

LLM이 제안한 claim은 evidence가 없으면 저장하지 않거나 `hypothesis` 격리 영역에 둔다.

---

# 20. Graph Engine MCP 인터페이스

## 20.1 MVP tools

| Tool | 목적 |
|---|---|
| `graph_index` | 저장소 최초 색인 |
| `graph_update` | 변경 파일만 증분 갱신 |
| `graph_query` | local/global/wiki/debug/mix 질의 |
| `graph_path` | 두 entity 사이 경로 탐색 |
| `graph_explain` | node·symbol·concept 설명 |
| `graph_impact` | 파일·심볼·DB 객체 변경 영향도 |
| `graph_context_pack` | 토큰 예산에 맞는 ContextPack 생성 |
| `graph_status` | repository, snapshot, parser, stale 상태 진단 |

## 20.2 후속 tools

| Tool | 목적 |
|---|---|
| `graph_diff` | commit/branch 간 그래프 차이 |
| `graph_page` | Wiki page 생성·조회·갱신 |
| `graph_stale` | stale claim/page 조회 |
| `graph_import_events` | Context Router SessionEvent import |
| `graph_decisions` | 과거 결정·제약·거절된 접근 검색 |
| `graph_review_claim` | claim 승인·거절·수정 |
| `graph_get_node` | node 상세 조회 |
| `graph_neighbors` | 제한된 neighborhood 조회 |

## 20.3 resources

```text
repo://<repository-id>@<commit>/<path>
repo://<repository-id>@<commit>/<path>#L<start>-L<end>
symbol://<language>/<qualified-name>
graph://node/<node-id>
graph://edge/<edge-id>
wiki://<page-id>
wiki://<page-id>/<section-id>
test://<run-id>/<test-id>
decision://<session-id>/<decision-id>
```

## 20.4 prompts

후속 단계에서 다음 MCP prompt를 제공할 수 있다.

```text
explain_architecture
trace_request_flow
analyze_change_impact
review_stale_wiki
prepare_refactoring_context
```

---

# 21. Graph Engine 저장소 설계

## 21.1 권장 저장 구조

```text
.graph-engine/
├─ metadata.db
├─ repositories/
│  └─ <repository-id>/
│     ├─ snapshots/
│     ├─ artifacts/
│     └─ locks/
├─ indexes/
└─ cache/
```

## 21.2 기본 저장 전략

```text
Metadata store:
- repository
- snapshot
- node
- edge
- evidence
- claim
- wiki page
- revision
- imported session event

Search index:
- symbol names
- file paths
- docs
- claims
- wiki sections
- evidence snippets

In-memory adjacency:
- node → outgoing edge IDs
- node → incoming edge IDs
- type-specific indexes
- path query cache
```

외부 그래프 DB는 MVP에서 사용하지 않는다.

## 21.3 핵심 인터페이스

```go
type Parser interface {
    ID() string
    Version() string
    Supports(path string, contentPrefix []byte) bool
    Parse(ctx context.Context, req ParseRequest) (ParseResult, error)
}

type GraphStore interface {
    ApplyDelta(ctx context.Context, delta GraphDelta) error
    GetNode(ctx context.Context, id NodeID) (Node, error)
    GetEdges(ctx context.Context, filter EdgeFilter) ([]Edge, error)
    Snapshot(ctx context.Context, repoID, commit string) (Snapshot, error)
}

type QueryEngine interface {
    Query(ctx context.Context, req QueryRequest) (QueryResult, error)
    Path(ctx context.Context, req PathRequest) (PathResult, error)
    Impact(ctx context.Context, req ImpactRequest) (ImpactResult, error)
    Pack(ctx context.Context, req ContextPackRequest) (ContextPack, error)
}
```

---

# 22. Graph Engine Go 패키지 구조

MVP 기준 최소 구조:

```text
graph-engine/
├─ cmd/
│  └─ graph-engine/
│     └─ main.go
├─ internal/
│  ├─ app/          # 조립, lifecycle, config
│  ├─ mcp/          # MCP tools/resources/prompts
│  ├─ repo/         # repository 등록, git, ignore, fingerprint
│  ├─ parser/       # 언어 parser interface와 구현
│  ├─ graph/        # node, edge, delta, traversal
│  ├─ query/        # entity resolution, path, impact, packing
│  ├─ knowledge/    # evidence, claim, wiki, temporal
│  ├─ search/       # 전문 검색 추상화
│  ├─ store/        # persistence
│  └─ security/     # 경로, 파일, secret, untrusted corpus 정책
├─ schemas/
│  ├─ evidence-ref.v1.schema.json
│  ├─ context-pack.v1.schema.json
│  └─ graph-resource.v1.schema.json
├─ testdata/
│  ├─ csharp-console/
│  ├─ aspnet-api/
│  ├─ wpf-mvvm/
│  ├─ sqlserver-procedures/
│  └─ multi-provider-db/
├─ docs/
├─ go.mod
└─ README.md
```

C#, SQL, config parser가 커지면 `internal/parser/csharp`, `internal/parser/sql`처럼 분리한다.

---

# 23. Graph Engine 보안 요구사항

## 23.1 신뢰할 수 없는 저장소

- 저장소의 Markdown, 코드 주석, 파일명은 모두 신뢰할 수 없는 입력이다.
- 파일 내용에 포함된 지시는 MCP 서버 명령으로 실행하지 않는다.
- path traversal, symlink escape, junction escape를 방지한다.
- 저장소 root 밖의 파일을 색인하지 않는다.
- 매우 큰 파일, binary, 압축 폭탄, 순환 symlink를 제한한다.
- vendor/generated/worktree 중복을 기본 제외한다.

## 23.2 Parser 격리

- parser panic은 서버 전체를 종료시키지 않아야 한다.
- 파일별 timeout과 크기 제한을 적용한다.
- native parser binding을 사용할 경우 안전한 wrapper와 fuzz test가 필요하다.
- extractor version을 결과에 저장한다.

## 23.3 정보 노출

- source URI에 사용자 홈 절대 경로를 포함하지 않는다.
- repository-relative URI를 사용한다.
- secret 후보가 포함된 source span은 정책에 따라 마스킹한다.
- HTTP MCP를 활성화할 경우 인증·TLS·repository ACL이 필수다.

## 23.4 렌더링

웹 UI가 없더라도 Markdown 또는 향후 HTML export에 저장소 문자열을 넣을 때 escape한다.

- 파일명과 symbol label을 원시 HTML로 신뢰하지 않는다.
- Markdown 링크의 URI scheme을 제한한다.
- script, event handler, data URI를 허용하지 않는다.

---

# 24. Graph Engine MVP 완료 기준

1. STDIO MCP 서버로 실행된다.
2. C# solution/project/file/symbol을 색인한다.
3. class/interface/method와 기본 call/inheritance/implementation edge를 생성한다.
4. SQL 파일에서 table/procedure와 read/write 관계를 생성한다.
5. 모든 추출 edge가 source span과 extractor 정보를 가진다.
6. `graph_query`, `graph_path`, `graph_explain`이 동작한다.
7. 파일 변경 후 `graph_update`가 전체 재색인 없이 반영한다.
8. 삭제된 파일의 node/edge가 정확히 정리된다.
9. `graph_impact`가 direct/transitive/test 후보를 구분한다.
10. `graph_context_pack`이 지정 토큰 예산 안에서 결과를 반환한다.
11. stale claim의 기반 구조가 준비된다.
12. malicious path, oversized file, malformed source에 대한 테스트가 통과한다.

---

# Part C. 공통 상호 운용 계약

# 25. 공통 URI 체계

## 25.1 Repository source

```text
repo://<repository-id>@<commit>/<normalized-path>#L<start>-L<end>
```

예시:

```text
repo://lib-db@abc123/src/Providers/OracleProvider.cs#L40-L72
```

규칙:

- 경로 구분자는 `/`로 정규화한다.
- Windows drive letter와 사용자 홈 디렉터리를 노출하지 않는다.
- commit을 포함해 재현 가능하게 만든다.
- line range가 없는 파일 URI도 허용한다.
- rename 추적은 별도 node identity로 관리한다.

## 25.2 Symbol

```text
symbol://<language>/<qualified-name>
```

예시:

```text
symbol://csharp/Lib.Db.Providers.OracleProvider.ExecuteAsync
```

## 25.3 Session과 artifact

```text
session://<session-id>
artifact://<session-id>/<content-hash>
decision://<session-id>/<decision-id>
test://<run-id>/<test-id>
```

## 25.4 Graph와 wiki

```text
graph://node/<node-id>
graph://edge/<edge-id>
wiki://<page-id>/<section-id>
```

---

# 26. SessionEvent v1

소유 저장소: **Context Router**  
소비 저장소: **Graph Engine**

```json
{
  "schemaVersion": "1.0",
  "eventId": "evt-001",
  "sessionId": "session-123",
  "eventType": "test_run",
  "timestamp": "2026-07-17T09:10:00Z",
  "summary": "PostgreSQL integration tests failed",
  "repository": {
    "repositoryId": "lib-db",
    "commit": "abc123",
    "branch": "feature/provider"
  },
  "artifactRefs": [
    "artifact://session-123/sha256-output"
  ],
  "relatedResources": [
    "symbol://csharp/Lib.Db.PostgreSqlProvider"
  ],
  "attributes": {
    "exitCode": 1,
    "failedTests": 3
  },
  "privacyLabel": "internal",
  "producer": {
    "name": "context-router",
    "version": "0.1.0"
  }
}
```

요구사항:

- 하위 호환 필드는 optional로 추가한다.
- 의미가 바뀌는 변경은 major schema version을 올린다.
- 알 수 없는 eventType은 보존할 수 있어야 한다.
- Graph Engine은 필수 필드만 검증하고 확장 필드는 손실 없이 저장할 수 있다.

---

# 27. EvidenceRef v1

소유 저장소: **Graph Engine**  
Context Router도 SessionEvent의 근거를 표현할 때 사용할 수 있다.

```json
{
  "schemaVersion": "1.0",
  "uri": "repo://lib-db@abc123/src/Connection.cs#L40-L72",
  "sourceType": "source_span",
  "extractor": {
    "name": "csharp-parser",
    "version": "0.1.0"
  },
  "confidence": "EXTRACTED_AST",
  "freshness": {
    "commit": "abc123",
    "status": "CURRENT"
  },
  "digest": "sha256-...",
  "privacyLabel": "internal"
}
```

---

# 28. ContextPack v1

소유 저장소: **Graph Engine**  
소비자: MCP 클라이언트 및 선택적으로 Context Router

```json
{
  "schemaVersion": "1.0",
  "question": "Oracle provider 변경 영향 범위를 설명해줘",
  "intent": "impact",
  "summary": "Oracle provider 변경은 connection factory, parameter binding, integration tests에 직접 영향을 줄 가능성이 높습니다.",
  "budget": {
    "requestedTokens": 2200,
    "estimatedTokens": 1830
  },
  "nodes": [],
  "edges": [],
  "paths": [],
  "claims": [],
  "evidenceRefs": [],
  "warnings": [
    {
      "code": "STALE_CLAIM",
      "message": "Oracle parameter binding 설명 일부가 현재 commit 이전에 검증되었습니다."
    }
  ],
  "omitted": {
    "nodeCount": 37,
    "edgeCount": 81,
    "reason": "TOKEN_BUDGET"
  }
}
```

ContextPack은 raw graph dump가 아니다. 에이전트 질문에 필요한 최소 정보만 포함해야 한다.

---

# 29. Claim 모델

```json
{
  "claimId": "claim-auth-001",
  "text": "모든 API 요청은 controller 실행 전에 인증 middleware를 통과한다.",
  "status": "CURRENT",
  "confidence": "EXTRACTED_AST",
  "evidenceRefs": [
    "repo://sample@abc123/src/AuthMiddleware.cs#L12-L64"
  ],
  "lastVerifiedCommit": "abc123",
  "review": {
    "status": "UNREVIEWED"
  }
}
```

규칙:

- evidence가 없는 claim은 `HYPOTHESIS`로 제한한다.
- `INFERRED_LLM` claim은 자동으로 `HUMAN_VERIFIED`가 될 수 없다.
- source digest가 바뀌면 freshness를 다시 평가한다.

---

# 30. 스키마 소유권과 배포

세 번째 저장소를 만들지 않기 위해 스키마 소유권을 나눈다.

| Schema | 소유 저장소 | 배포 방식 |
|---|---|---|
| `session-event.v1` | Context Router | tagged release artifact |
| `artifact-ref.v1` | Context Router | tagged release artifact |
| `evidence-ref.v1` | Graph Engine | tagged release artifact |
| `context-pack.v1` | Graph Engine | tagged release artifact |
| `graph-resource.v1` | Graph Engine | tagged release artifact |

각 저장소는 상대 저장소의 schema 파일을 `schemas/vendor/`에 버전 고정하여 포함할 수 있다. 복사본에는 원본 schema version과 source release를 기록한다.

내부 Go struct를 공유 모듈로 강제하지 않는다. JSON schema와 contract test가 상호 운용의 기준이다.

---

# Part D. Go 공통 기술 설계

# 31. Go 구현 원칙

## 31.1 단일 언어 원칙

- 핵심 서버, 저장소, 파서 조정, MCP transport를 Go로 구현한다.
- Python 또는 TypeScript 런타임을 필수 의존성으로 두지 않는다.
- 외부 프로세스가 필요한 경우 명시적 optional adapter로만 허용한다.
- MVP는 외부 LLM worker 없이 실행 가능해야 한다.

## 31.2 Context 사용

모든 장시간 작업은 `context.Context`를 받는다.

```text
MCP request cancellation
→ parser/index cancellation
→ process execution cancellation
→ DB query cancellation
→ streaming response stop
```

## 31.3 동시성

- 무제한 goroutine을 만들지 않는다.
- parser, file scan, command execution에 별도 semaphore를 둔다.
- repository별 write lock을 둔다.
- 읽기 query와 snapshot 갱신의 일관성을 정의한다.
- panic은 request boundary에서 recover하고 구조화된 오류로 변환한다.

## 31.4 메모리

- 대형 파일과 stdout을 `io.Reader` 스트리밍으로 처리한다.
- 전체 artifact를 메모리에 올리지 않는다.
- 그래프 adjacency는 메모리 상한과 lazy loading을 고려한다.
- ContextPack 결과는 budget을 초과하기 전에 중단한다.

## 31.5 오류 모델

공통 오류 코드 예시:

```text
INVALID_ARGUMENT
NOT_FOUND
PERMISSION_DENIED
WORKSPACE_VIOLATION
UNSUPPORTED_FILE
PARSER_FAILED
INDEX_OUTDATED
BUDGET_EXCEEDED
EXECUTION_TIMEOUT
OUTPUT_LIMIT_EXCEEDED
STORAGE_UNAVAILABLE
CONFLICT
INTERNAL
```

MCP error 메시지에는 내부 절대 경로, 환경 변수, 비밀 정보를 노출하지 않는다.

---

# 32. MCP 공통 응답 envelope

도구별 데이터는 달라도 공통 메타데이터 형식을 맞춘다.

```json
{
  "ok": true,
  "data": {},
  "meta": {
    "requestId": "req-123",
    "server": "graph-engine",
    "serverVersion": "0.1.0",
    "durationMs": 48,
    "truncated": false,
    "warnings": []
  }
}
```

오류 예시:

```json
{
  "ok": false,
  "error": {
    "code": "WORKSPACE_VIOLATION",
    "message": "요청 경로가 허용된 저장소 범위를 벗어났습니다.",
    "retryable": false
  },
  "meta": {
    "requestId": "req-124",
    "server": "context-router"
  }
}
```

---

# 33. 설정 원칙

## 33.1 Context Router 예시

```yaml
transport:
  type: stdio

workspace:
  allowedRoots:
    - C:/Users/js/Documents

execution:
  allowedCommands:
    - git
    - dotnet
    - go
  allowShell: false
  timeoutSeconds: 300
  maxOutputBytes: 104857600
  maxConcurrent: 2

storage:
  root: .context-router
  retentionDays: 30

privacy:
  redactSecrets: true
  rawArtifactPolicy: local-only
```

## 33.2 Graph Engine 예시

```yaml
transport:
  type: stdio

repository:
  allowedRoots:
    - C:/Users/js/Documents
  followSymlinks: false
  maxFileBytes: 5242880

index:
  workers: 4
  includeTests: true
  includeDocs: true
  incremental: true

parsers:
  csharp: true
  sql: true
  config: true

query:
  defaultMaxTokens: 2000
  maxPathDepth: 8

storage:
  root: .graph-engine
```

환경 변수는 비밀 값과 머신별 override에만 사용하고, 주요 정책은 명시적인 설정 파일로 관리한다.

---

# 34. 관측 가능성

두 서버는 stderr 또는 설정된 로그 sink로 구조화 로그를 기록한다. STDIO transport의 stdout에는 MCP protocol 외 데이터를 쓰지 않는다.

공통 로그 필드:

```text
timestamp
level
request_id
session_id
repository_id
tool_name
duration_ms
result_status
bytes_read
bytes_written
token_estimate
error_code
```

민감 데이터와 원문 source/output은 기본 로그에 포함하지 않는다.

---

# 35. 성능 목표

정확한 수치는 PoC 후 조정하되 다음 방향을 기준으로 한다.

## 35.1 Context Router

- 대형 stdout을 bounded memory로 저장한다.
- 검색은 세션 규모가 커져도 전체 파일 순차 읽기를 피한다.
- 기본 도구 결과는 원문 대비 매우 작은 비율만 반환한다.
- 명령 실행 시간 외의 추가 지연을 최소화한다.

## 35.2 Graph Engine

- 변경 없는 파일은 다시 파싱하지 않는다.
- query 요청마다 전체 그래프를 재로딩하지 않는다.
- path와 impact 결과는 limit, depth, budget을 가진다.
- 대형 저장소에서도 초기 색인과 증분 갱신을 별도 측정한다.

---

# Part E. 테스트·평가·보안 검증

# 36. 공통 테스트 전략

## 36.1 Unit test

- schema validation
- URI parsing/normalization
- path policy
- token estimation
- chunking
- graph traversal
- confidence/freshness calculation
- retention policy

## 36.2 Golden test

고정 fixture에서 예상 node, edge, path, search hit, ContextPack을 snapshot으로 검증한다.

## 36.3 Integration test

- 실제 STDIO MCP 요청/응답
- 서버 재시작 후 상태 복구
- SQLite transaction failure
- cancellation
- concurrent requests
- malformed requests

## 36.4 Fuzz test

Go fuzzing을 다음에 적용한다.

```text
URI parser
path normalizer
JSON contract decoder
source chunker
C#/SQL parser wrapper
graph delta application
MCP argument validation
```

## 36.5 Security test

```text
path traversal
symlink/junction escape
malicious file name
HTML/Markdown injection
prompt injection text
secret leakage
oversized files
binary files
zip bomb-like inputs
nested worktree
malformed UTF-8
command injection
shell metacharacters
execution timeout
output flooding
```

---

# 37. Graph Engine fixture 우선순위

```text
testdata/
├─ csharp-console/
├─ csharp-multi-project/
├─ aspnet-api/
├─ wpf-mvvm/
├─ winforms-events/
├─ sqlserver-procedures/
├─ mysql-queries/
├─ postgresql-queries/
├─ oracle-procedures/
├─ multi-provider-db/
├─ malformed-source/
└─ hostile-repository/
```

특히 `multi-provider-db`는 다음을 검증한다.

- SQL Server, MySQL/MariaDB, PostgreSQL, Oracle provider 분기
- raw SQL
- stored procedure
- parameter binding
- connection factory
- provider-specific test

---

# 38. 품질 평가 지표

## 38.1 Context Router

```text
stored_bytes
returned_bytes
estimated_tokens_avoided
search_precision
search_recall
fetch_after_search_rate
session_recovery_success
missing_event_rate
duplicate_event_rate
execution_failure_rate
security_policy_denial_rate
```

## 38.2 Graph Engine

```text
symbol_extraction_precision
edge_precision
edge_recall
source_evidence_coverage
path_usefulness
impact_false_positive_rate
impact_false_negative_rate
incremental_update_accuracy
stale_claim_detection_rate
index_time
update_time
query_latency
context_pack_token_accuracy
```

## 38.3 통합 평가

동일 질문을 다음 방식으로 비교한다.

```text
Baseline:
네이티브 grep/read/bash 반복

Proposed:
Graph Engine 탐색 + Context Router 대형 출력 관리
```

비교 항목:

```text
정답 정확도
근거 정확도
tool call 수
모델 입력 토큰 수
읽은 파일 수
잘못 탐색한 파일 수
문제 해결 완료율
compaction 이후 복구율
```

---

# Part F. 개발 로드맵

# 39. Phase 0 — 프로젝트 원칙과 라이선스 분류

두 저장소에 다음을 먼저 만든다.

```text
PROJECT_CHARTER.md
SCOPE.md
NON_GOALS.md
SECURITY.md
LICENSE_STRATEGY.md
INTEROP.md
```

완료 조건:

- 두 저장소의 책임 범위가 중복되지 않는다.
- Go/MCP-only 원칙이 명시된다.
- fork/port와 독립 재구현 중 실제 방식을 결정한다.
- 원본 LICENSE와 NOTICE 적용 범위를 확정한다.
- 공통 URI와 schema ownership을 확정한다.

# 40. Phase 1 — 공통 MCP 기반과 contract

각 저장소에서 독립적으로 구현한다.

```text
STDIO MCP lifecycle
structured logging
config loading
request ID
cancellation
common error model
JSON schema validation
contract tests
```

완료 조건:

- 두 서버가 Codex와 Claude Code에서 각각 연결된다.
- stdout protocol 오염이 없다.
- sample tool 호출과 cancellation이 동작한다.

# 41. Phase 2A — Context Router MVP

순서:

1. artifact store
2. streaming command capture
3. chunking
4. search index
5. fetch
6. session event store
7. session summary
8. event export
9. stats/health
10. security hardening

# 42. Phase 2B — Graph Engine MVP

순서:

1. repository registration
2. Git/fingerprint snapshot
3. C# parser
4. project/config parser
5. graph store
6. query/path/explain
7. SQL parser
8. impact
9. incremental update
10. ContextPack

두 Phase 2 트랙은 저장소별로 병렬 진행할 수 있다.

# 43. Phase 3 — 상호 운용

```text
Context Router SessionEvent export
→ MCP client 전달
→ Graph Engine event import
→ code/test/decision 연결
→ Graph ContextPack 생성
→ 필요 시 Context Router에 artifact 저장
```

완료 조건:

- 서버가 서로 직접 호출하지 않는다.
- schema mismatch를 명확한 오류로 반환한다.
- 같은 이벤트를 재import해도 중복되지 않는다.
- ContextPack의 evidence URI가 실제 resource로 해석된다.

# 44. Phase 4 — Evidence, Wiki, Temporal

```text
claim model
wiki page model
human review
stale detector
commit diff
decision history
rejected approach history
```

# 45. Phase 5 — 성능과 팀 운영 검토

MVP 품질을 확인한 뒤에만 검토한다.

```text
Streamable HTTP MCP
인증과 ACL
다중 repository
공유 서버
벡터 검색
웹 viewer
런타임 trace
PR impact
```

---

# Part G. 저장소별 초기 README 핵심 문구

# 46. Context Router 설명 초안

```md
# Context Router

Context Router is a Go-based MCP server that keeps large tool outputs,
searchable artifacts, and session events outside the model context window.
It returns only the relevant summaries and source ranges required by an AI coding agent.

The project is intentionally focused on context routing, artifact retrieval,
session continuity, and context-budget management. It does not build a source-code graph.
```

# 47. Graph Engine 설명 초안

```md
# Graph Engine

Graph Engine is a Go-based MCP server that builds an evidence-backed,
commit-aware knowledge graph from source code, project files, SQL, tests, and documentation.
It provides graph query, path, explanation, impact analysis, wiki claims,
and token-budgeted ContextPack results for AI coding agents.

The project does not manage large command outputs or act as a general session store.
```

---

# 48. 원본 프로젝트와 라이선스 전략

## 48.1 반드시 구분할 용어

### 실제 fork

원본 저장소를 분기하고 원본 코드를 수정한다.

### port

원본 구현을 다른 언어로 옮기며 구조, 알고리즘, 테스트 또는 코드를 실질적으로 참조한다.

### 독립 재구현

공개된 요구사항과 동작을 기준으로 새로운 구조와 테스트로 구현하며 원본 코드를 복사·번역하지 않는다.

## 48.2 기본 권장 전략

두 저장소 모두 Go로 새로 만드는 만큼 다음을 기본으로 권장한다.

```text
독립 Go 재구현
+ 필요한 경우 공개 데이터 포맷 importer/exporter
+ 원본 코드 직접 복사 금지
+ 독자적인 schema와 golden test 작성
```

그러나 실제로 원본 코드, 문서, 테스트, schema, 명령 구조를 실질적으로 옮기면 독립 재구현으로 표현하면 안 된다.

## 48.3 원본 고지

이전 조사 당시 다음으로 파악되었다.

- Graphify: MIT 계열 라이선스
- context-mode: Elastic License 2.0 계열 라이선스

구현을 시작하기 전에 각 원본 저장소의 최신 `LICENSE`, `NOTICE`, 릴리스 상태를 다시 확인해야 한다.

실제 fork/port라면:

- 원저작권 고지를 유지한다.
- 원 라이선스 조건을 따른다.
- 수정된 프로젝트임을 명시한다.
- 원본과 공식 제휴 관계가 없음을 명시한다.

독립 재구현이라면 제품명에 원본 이름을 사용하지 않고, 필요할 경우 다음 정도의 관계만 밝힌다.

```md
This project is an independent Go implementation inspired by graph-based
codebase exploration and context-routing tools. It is not affiliated with
the original projects.
```

## 48.4 호환성 경계

독립 재구현을 선택해도 기존 사용자의 마이그레이션을 위해 제한적인 호환 기능은 제공할 수 있다.

권장 후속 기능:

```text
Graph Engine:
- 기존 graph JSON 산출물의 명시적 importer
- importer 결과를 내부 node/edge/evidence schema로 변환
- 원본 내부 schema를 런타임 진실의 원천으로 사용하지 않음

Context Router:
- 기존 세션 기록이 공개 export 형식을 가질 경우 명시적 importer
- 기존 SQLite 테이블을 직접 공유하거나 계속 조회하지 않음
```

호환 도구는 migration boundary이며 코어 모델이 아니다. 원본과 같은 tool 이름, README 문구, 내부 schema를 그대로 복제하는 것을 호환성으로 오해하지 않는다.

이 절은 법률 자문이 아니며, 배포 전에 라이선스 검토가 필요하다.

---

# 49. 하지 말아야 할 것

## 49.1 공통

- 두 저장소를 하나로 합치지 않는다.
- 공통 내부 Go package를 만들기 위해 세 번째 저장소를 즉시 만들지 않는다.
- 내부 DB schema를 상호 의존시키지 않는다.
- 서버 간 재귀 MCP 호출을 만들지 않는다.
- MCP 결과에 무제한 원문을 포함하지 않는다.
- 절대 경로와 비밀 값을 로그에 남기지 않는다.
- 모든 기능을 첫 버전에 넣지 않는다.

## 49.2 Context Router

- 네이티브 도구를 자동으로 가로챌 수 있다고 가정하지 않는다.
- 임의 셸 실행을 기본 허용하지 않는다.
- artifact 원문을 매 응답마다 모델에 반환하지 않는다.
- 단순 요약만 저장하고 원문을 버리지 않는다.
- 코드 그래프 기능을 추가하지 않는다.

## 49.3 Graph Engine

- raw `graph.json` 전체를 모델에 전달하지 않는다.
- LLM 추론 edge를 소스 추출 edge와 동일하게 취급하지 않는다.
- evidence 없는 claim을 확정 지식으로 저장하지 않는다.
- 모든 언어 parser를 동시에 만들지 않는다.
- Neo4j, vector DB, 웹 UI부터 시작하지 않는다.
- 코드 변경 없이 오래된 wiki를 계속 CURRENT로 두지 않는다.

---

# 50. 최종 제품 사용 예시

## 50.1 대형 테스트 실패 분석

```text
Agent
→ context_execute(dotnet test ...)
→ 요약 + artifact URI 수신
→ context_search("ORA-01017")
→ context_fetch(정확한 오류 범위)
→ graph_query(mode=debug, entityHints=[OracleProvider])
→ 관련 코드/SQL/테스트 경로 수신
→ graph_context_pack(maxTokens=2200)
→ 근거 기반 수정 수행
```

## 50.2 Provider 변경 영향도 분석

```text
Agent
→ graph_impact(symbol://csharp/Lib.Db.Providers.OracleProvider)
→ ConnectionFactory, parameter binding, integration tests 후보 수신
→ source evidence 확인
→ 필요한 테스트를 context_execute로 실행
→ 결과를 context_record_event(test_run)으로 기록
→ 선택된 이벤트를 graph_import_events로 반영
```

## 50.3 세션 compaction 이후 복구

```text
Agent
→ context_session_summary
→ 현재 목표, 결정, 제약, 수정 파일, 실패 테스트 복원
→ graph_context_pack(intent=debug)
→ 코드 구조와 이전 세션 근거를 함께 확보
```

---

# 51. 최종 프로젝트 원칙

```text
1. Context Router와 Graph Engine은 서로 다른 제품이며 별도 저장소다.
2. 두 제품은 모두 Go로 구현한다.
3. 사용자 기능은 MCP로만 노출한다.
4. STDIO MCP를 로컬 기본 전송으로 사용한다.
5. Context Router는 대형 출력, artifact, 세션, 컨텍스트 예산을 담당한다.
6. Graph Engine은 코드 구조, 관계, 근거, 영향도, wiki, 시간 모델을 담당한다.
7. Context Router는 코드 그래프를 만들지 않는다.
8. Graph Engine은 범용 명령 실행기나 세션 저장소가 되지 않는다.
9. 두 서버는 내부 코드와 데이터베이스를 공유하지 않는다.
10. MCP 클라이언트가 두 서버의 호출을 조정한다.
11. SessionEvent, EvidenceRef, ContextPack 같은 버전형 계약으로 연동한다.
12. Source Graph는 최대한 결정론적으로 생성한다.
13. LLM 추론은 별도 confidence와 provenance를 가진다.
14. evidence 없는 claim은 확정 지식으로 저장하지 않는다.
15. commit-aware 증분 갱신과 stale detection을 핵심으로 둔다.
16. C#/.NET과 SQL을 초기 지원 우선순위로 둔다.
17. 로컬 우선, 최소 의존성, 단일 바이너리를 목표로 한다.
18. Graph DB, vector DB, 웹 UI, SaaS는 MVP 범위에서 제외한다.
19. 실제 port/fork 여부와 원 라이선스 의무를 코드 작성 전에 확정한다.
20. 제품 성공 기준은 기능 수가 아니라 정확도, 근거성, 컨텍스트 절약, 복구 가능성이다.
```

---

# 52. 최종 요약

**Context Router**는 현재 작업의 대형 출력과 세션 기억을 효율적으로 관리하는 Go MCP 서버다.  
**Graph Engine**은 저장소의 코드·SQL·테스트·문서·변경 이력을 근거 중심 지식 그래프로 유지하는 Go MCP 서버다.

둘은 합쳐지지 않으며, MCP 클라이언트가 각각을 독립적으로 호출한다. Context Router가 `SessionEvent`를 내보내고 Graph Engine이 이를 명시적으로 가져오며, Graph Engine은 질문별 `ContextPack`을 반환한다. 이 구조를 통해 에이전트는 모든 파일과 로그를 반복해서 읽는 대신, 필요한 구조와 근거만 제한된 컨텍스트 안에서 사용할 수 있다.
