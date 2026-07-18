# oracle 매핑 — ctr ↔ ctxscribe(context-mode) 필드/의미 대응 (게이트 1)

설계 §12 게이트 1의 근거 문서. **oracle** = ctxscribe 1.3.0의 로컬 참조 구현
`context-mode`(TypeScript/Node, `build/store.js`로 컴파일됨). 게이트 1의 "동등성"은
**byte 동일성이 아니라, 이 문서에 명문화된 이름·필드 매핑 기준 하의 의미 동등성**이다.

README.md의 도구 이름 매핑표(`ctx_* → ctr_*`)를 전제로, 여기서는 `ctr_search`↔`ctx_search`,
`ctr_index`↔`ctx_index`의 요청/응답 필드 대응과 **의도적 차이**를 실제 구현 인용으로
확정한다(추정 금지 — 코드 근거만). 인용 표기: `ctr` = 이 저장소,
`oracle` = `C:\Users\js\Documents\ClaudeCode\context-mode`.

## 1. ctr_search ↔ ctx_search

### 입력 필드

| ctx_search (oracle) | ctr_search | 대응/차이 근거 |
|---|---|---|
| `queries: string[]` | `queries: string[]` | 동등. ctr는 1~8개 강제(`ctr internal/mcp/mcp.go:197`), oracle는 배열 강제 후 개수 제한 없음(`oracle src/search/ctx-search-schema.ts:78`). |
| `limit` (기본 3) | `limit` (기본 3, 최대 10) | 기본값 동일. ctr는 상한 10 클램프(`ctr mcp.go:200-205`), oracle는 상한 없음(`oracle ctx-search-schema.ts:90-94`). |
| `source` (부분 일치 필터) | — | ctr v0.0.1 미구현(도구 스키마에 없음, `ctr mcp.go:151-155`). oracle는 label LIKE 필터(`oracle src/store.ts:1116-1129`). **의도적 차이 D-1**. |
| `contentType: code\|prose` | — | ctr 미구현. oracle는 content_type 컬럼 필터(`oracle store.ts:1144-1160`). **의도적 차이 D-1**. |
| `sort: relevance\|timeline` | — (relevance만) | ctr는 relevance 전용(세션/타임라인 소스 없음). oracle의 timeline은 SessionDB+auto-memory 병합(`oracle src/search/unified.ts:70-176`). **의도적 차이 D-2**. |
| `project` (shared 모드) | (별도 도구) | ctr는 교차 프로젝트를 `ctr_global_search`로 분리(README). oracle는 shared 모드에서 `project` 파라미터(`oracle ctx-search-schema.ts:62-113`). |
| — | `max_return_bytes` (기본 8192) | ctr 고유: 스니펫 바이트 예산(`ctr mcp.go:154,206-209`). oracle는 store 계층에 예산 없음 — 응답 포매팅 계층에서 절단. **의도적 차이 D-5**. |

### 출력 필드 (히트 단위)

| ctx_search (oracle `SearchResult`/`UnifiedSearchResult`) | ctr_search (`searchHit`) | 대응/차이 근거 |
|---|---|---|
| `source` (= `sources.label`, `oracle store.ts:1107`) | `source` (project-relative 경로, `ctr search.go:194,209`) | **의미 동등**하되 표현 차이: oracle=색인 시 넘긴 label(파일명/문자열), ctr=projectRoot 상대경로. 동등성 비교는 **basename으로 정규화**(§4). |
| `title` (청크 제목: 헤딩/첫 줄, `oracle store.ts:1682,1928`) | — | ctr는 청크 title을 히트에 노출하지 않음(`ctr search.go:17-24`). 청크 title은 색인/FTS 내부에만 존재. |
| `content` (청크 전문, `oracle store.ts:1106`) | `snippet` (매치 중심 ±250B 창, `ctr search.go:234-342`) | **의도적 차이 D-5**: oracle은 청크 전문 반환, ctr은 매치 앵커 중심 스니펫 창. |
| `highlighted` (`highlight()` 마커, `oracle store.ts:602`) | — | ctr 미노출. |
| `rank` (= `-score`, `oracle store.ts:1292`) | `score` (RRF 합산 점수, `ctr search.go:137-145`) | 부호/의미 반대: oracle rank는 작을수록 상위(음수 점수), ctr score는 클수록 상위. |
| `contentType`, `origin`, `timestamp`, `matchLayer` | — | ctr 미노출(단일 소스·relevance 전용). |
| — | `artifact_id`, `chunk_id`, `line_start`, `line_end` | ctr 고유: 좌표·fetch 연동(`ctr mcp.go:158-162`). |
| — | `stale`, `redacted`, `source_coords_exact` | ctr 고유: 파일 변경 감지·redaction·좌표 정확도 플래그(`ctr mcp.go:165-167`). |

## 2. ctr_index ↔ ctx_index

### 입력 필드

| ctx_index (oracle) | ctr_index | 대응/차이 근거 |
|---|---|---|
| `content` | `content` (+ `title` 필수) | 대응. ctr은 inline 시 title 필수(`ctr mcp.go:405-413`), oracle은 `source`가 label 역할(`oracle server.ts:2188`). |
| `path` (파일 또는 디렉터리) | `path` | 대응. ctr은 워크스페이스 경계 강제(`ctr internal/ingest/ingest.go:679-703`), oracle은 보안 게이트 별도(`oracle server.ts:2097-2147`). |
| `source` (label) | `title` (inline) / 경로 기반(파일) | oracle의 `source`는 명시 label, ctr은 파일 URI(=`ident.Fold(abs)`)/`inline:<title>`로 파생. |
| `include`/`exclude` 확장자·글롭 | `include`/`exclude` 글롭 | 대응(base name 글롭, `ctr ingest.go:464-469`). |
| `maxChunkBytes` 등 | `max_file_bytes` (기본 5MB) | ctr은 파일 크기 상한(`ctr ingest.go:46`), oracle은 청크 바이트 상한이 상수(`oracle store.ts:153`). |

### 출력 필드

| ctx_index (oracle `IndexResult`, `oracle store.ts:1093-1098`) | ctr_index (`IndexOutput`, `ctr mcp.go:397-401`) | 대응 |
|---|---|---|
| `totalChunks`, `codeChunks`, `label` (텍스트 응답으로 요약, `oracle server.ts:2194`) | `indexed`(파일 수), `bytes_stored`, `skipped[]{path,reason}` | 요약 지표. ctr은 파일 단위 색인 수·저장 바이트·스킵 사유를, oracle은 청크 수·label을 보고. 의미상 "무엇이 색인되었나"로 동등. |

## 3. 의도적 차이 목록 (검색 알고리즘 — 코드 인용)

동등성 비교의 전제. 아래는 **소스 파일 집합** 수준 동등성에는 영향이 작지만 **개별 청크
랭킹**에는 영향을 주는, 확인된 구현 차이다.

- **D-1 질의 결합(AND vs OR)** — ctr은 토큰을 `" AND "`로 결합(`ctr search.go:40-51`,
  `normalizeQuery`). oracle은 porter/trigram 모두 **OR** 모드로 질의(`oracle store.ts:1263-1264`,
  `#rrfSearch`). ⇒ oracle이 더 넓게(어느 토큰이든) 후보를 잡고, ctr은 더 좁게(모든 토큰)
  잡는다. 소스 집합 동등성을 위해 게이트 쿼리는 **정답이 ≥2 문서에 함께 등장하는 2~3 토큰
  구**로 설계했다(§4, testdata/retrieval/labels.json).
- **D-2 소스 범위** — ctr relevance는 ContentStore(프로젝트 색인) 전용. oracle의 timeline은
  SessionDB·auto-memory까지 병합(`oracle unified.ts:132-159`). ctr에는 세션 메모리 개념이
  없어(설계 유보) relevance만 대응한다.
- **D-3 RRF 병합 키·상수** — RRF 상수는 **양쪽 K=60 동일**(`ctr search.go:32`,
  `oracle store.ts:1260`). 병합 키가 다름: ctr은 **chunk id**로 병합(`ctr search.go:137-145`),
  oracle은 **`source::title`**로 병합(`oracle store.ts:1267`). 후보 폭도 다름: ctr은 표당
  `limit×4`(`ctr search.go:389,393`), oracle은 `max(limit×2,10)`(`oracle store.ts:1261`).
- **D-4 재랭킹·퍼지 폴백** — ctr은 RRF 점수 내림차순(동점 시 id 오름차순)만(`ctr search.go:154-169`),
  추가 재랭킹 없음. oracle은 근접성·title·구절 부스트 재랭킹(`oracle store.ts:1297-1345`)과
  결과 0건 시 편집거리 퍼지 교정 재질의(`oracle store.ts:1376-1395`)를 더한다.
- **D-5 스니펫·예산·출력 형태** — ctr은 매치 중심 ±250B 스니펫 창(`ctr search.go:234-342`)과
  쿼리 간 바이트 예산 배분(`ctr search.go:377-437`)을 store 계층에서 수행. oracle store는
  청크 전문 + `highlight()` 마커를 반환하고 예산/절단은 서버 응답 포매팅 계층 몫이다.
- **D-6 BM25 가중치** — ctr은 기본 가중(`bm25(table)`, 무가중, `ctr search.go:118`).
  oracle은 title 5.0/content 1.0 가중(`bm25(chunks, 5.0, 1.0)`, `oracle store.ts:601`). ⇒ oracle은
  제목(헤딩/함수명) 매치를 더 우대.
- **D-7 청킹** — 청크 크기 상한은 **양쪽 ~4096B**(`ctr ingest.go:277` `chunkTargetBytes=4096`,
  `oracle store.ts:153` `MAX_CHUNK_BYTES=4096`). 경계 규칙이 다름: ctr은 **확장자 인식** —
  `.md/.markdown`은 헤딩 경계 청커, 그 외(코드·로그·json)는 4KB 라인 블록 청커(`ctr ingest.go:290-362`).
  oracle은 `ctx_index`의 content/단일 path를 **항상 markdown 청커**로 처리하고(`oracle server.ts:2188`
  → `oracle store.ts:895`), 초과 시 문단(빈 줄) 경계로 하위 분할(`oracle store.ts:1692-1720`).
- **D-8 fetch 계약 재정의** — README 표대로 `ctx_fetch`(웹 fetch)와 `ctr_fetch`(저장 아티팩트
  byte-exact 읽기)는 계약이 다르다. 게이트 1 범위 밖(검색/색인만 대상).

## 4. 동등성 판정 기준 (명문화 — 설계 §12 게이트 1)

- **비교 단위**: 동일 corpus(`testdata/oracle/corpus/`, 14개 문서: md 7 / 코드 4 / 로그 2 / json 1)와
  동일 쿼리(`testdata/oracle/queries.json`, 12개)를 oracle과 ctr에 각각 색인·검색한다.
- **정규화**: 히트의 소스를 **basename으로 정규화**한다(oracle=label, ctr=project-relative —
  경로 규약 차이 흡수, §1). top-k에서 **distinct 소스 파일 집합**을 등장 순서로 뽑는다(k=3).
- **판정식**: 각 쿼리에서 `|oracle top-3 소스 집합 ∩ ctr top-3 소스 집합| ≥ 2`를 만족하면
  통과. **전체 쿼리의 80%(브리프 기준 "10개 중 8개") 이상**이 통과해야 게이트 1 합격.
- **미달 시**: 원인 분석 후 본 문서 "의도적 차이"에 기록하고 **기준 임의 완화 금지 —
  사용자 확인(BLOCKED)**. 임의 골든 조작 금지.
- **실측(2026-07 최초 실행, `TestOracleEquivalence`)**: **12/12 통과**(교집합 전부 ≥2, 8개는 3).
  D-1~D-8 차이에도 소스 파일 집합 수준에서는 강하게 일치. 상세 로그는
  `go test -v -run TestOracleEquivalence ./internal/search/`.

## 5. golden 생성 (요약)

oracle 실행이 필요한 부분은 **1회성 수동 생성**이다. 산출물 `testdata/oracle/golden.json`은
커밋되며, 동등성 테스트는 이 골든을 읽어 ctr 실검색과 대조한다(테스트가 oracle을 재실행하지
않음). 정확한 생성 절차·명령은 **`testdata/oracle/README.md`**, 생성 스크립트는
`testdata/oracle/gen-golden.mjs`(참조 구현 `build/store.js`를 `ctx_index`/`ctx_search`와 동일 경로로
구동)를 참조.
