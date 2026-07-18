# testdata/oracle — 게이트 1 oracle 동등성 골든

설계 §12 게이트 1의 증거 자료. **oracle** = ctxscribe 1.3.0 로컬 참조 구현
(`context-mode`). 여기 담긴 `golden.json`은 oracle을 **1회성 수동 실행**해 얻은
정적 산출물이며 커밋된다. Go 동등성 테스트(`internal/search/retrieval_test.go`의
`TestOracleEquivalence`)는 이 골든을 **읽어** ctr 실검색과 대조할 뿐, oracle을
재실행하지 않는다.

필드/의미 매핑과 동등성 판정 기준은 `docs/oracle-mapping-ko.md` 참조.

## 파일 구성

- `corpus/` — 게이트 1·2 공유 코퍼스(14개: markdown 7 / 코드 4 / 로그 2 / json 1,
  실제적 내용, 팽창 없음). 게이트 2 recall 하네스도 동일 코퍼스를 쓴다.
  - **의도적 편차(팽창 금지 우선)**: 파일당 청크 수는 균일하지 않다 — markdown 7개는 각
    ≥3청크(헤딩 경계), 코드 4개는 ~2청크, 소형 distractor(access.log·config.json)는 실제
    크기라 1청크다. `strings.Repeat`/생성 스크립트로 억지 증량하지 않는다(브리프 계약).
    aggregate 하한은 `TestCorpusShape`가 고정(총 청크 ≥50, 다중청크≥3 파일 ≥7).
- `queries.json` — 공유 쿼리 12개 + oracle top-k(`oracle_k=3`).
- `golden.json` — oracle 실측: 쿼리 → oracle top-3 소스 파일(basename) 목록.
- `gen-golden.mjs` — 골든 생성 스크립트(재현용, 커밋됨).
- (게이트 2 정답 라벨은 `testdata/retrieval/labels.json`.)

## golden 생성 절차 (1회성 수동)

`gen-golden.mjs`는 참조 구현이 `ctx_index`/`ctx_search`를 처리하는 것과 **동일 경로**로
oracle을 구동한다:

- 색인: 코퍼스 각 파일을 `ContentStore.index({ content, source: <basename> })`로 등록
  — 이는 oracle `ctx_index`(content/단일 path)가 호출하는 바로 그 경로다
  (`context-mode/src/server.ts:2188` → `src/store.ts:895`, 모든 내용을 markdown 청커로 처리).
- 검색: 각 쿼리를 `store.searchWithFallback(q, 3, undefined, undefined, "like")`로 질의
  — 이는 oracle `ctx_search`의 기본(`sort:"relevance"`) 경로다
  (`src/server.ts:2483` → `src/search/unified.ts:107` → `src/store.ts:1349`).
- 각 쿼리 top-3 결과에서 distinct 소스(basename)를 등장 순서로 뽑아 `golden.json`에 기록.

결정성: 파일을 정렬 순서로 색인하고 참조 검색은 corpus·query의 순수 함수라, 재실행하면
`golden.json`이 동일하게 재생성된다.

### 실행 명령 (Windows / PowerShell 또는 Bash)

참조 저장소 디렉터리에서 실행해야 `better-sqlite3`(native)와 `build/store.js`가 해석된다:

```sh
# 1) 참조 구현이 빌드돼 있어야 한다(build/store.js 존재). 없으면:
cd C:/Users/js/Documents/ClaudeCode/context-mode
npm run build            # tsc → build/*.js (better-sqlite3는 node_modules에 이미 설치됨)

# 2) 골든 생성 (참조 저장소 디렉터리에서 ctr 스크립트를 실행)
cd C:/Users/js/Documents/ClaudeCode/context-mode
node C:/Users/js/Documents/AI_DEV/context-router/testdata/oracle/gen-golden.mjs
```

환경 변수(선택):

- `CTX_ORACLE_STORE` — 참조 `build/store.js` 절대 경로(기본:
  `C:/Users/js/Documents/ClaudeCode/context-mode/build/store.js`).
- `CTX_ORACLE_VERSION` — 골든에 기록할 oracle 버전 문자열(기본 `1.3.0`).

성공 시 `wrote .../golden.json`과 쿼리별 top-3 소스가 출력된다.

## 재현 불가 시 (BLOCKED 규칙)

참조 저장소가 없거나 `better-sqlite3`/빌드가 로컬에서 동작하지 않아 oracle 실행이
불가능하면 — 맹목 재시도·임의 골든 조작 금지. 무엇을 시도했고 무엇이 막혔는지 구체적으로
적어 **BLOCKED로 보고**하고, 수동 골든 생성을 사용자에게 요청한다(설계 §12, 브리프 계약).

## 최초 실측 (2026-07)

- oracle 실행 환경: `context-mode` 1.3.0, `build/store.js`(컴파일본),
  `better-sqlite3`(abi 네이티브), Node의 내장/네이티브 SQLite + FTS5 정상.
- 동등성 결과: `TestOracleEquivalence` **12/12 통과**(교집합 ≥2, 기준 80%).
- recall 기준선: `TestRetrievalRecall` mean recall@5 ≈ **0.9306**, recall@10 = **1.0000**
  (플로어 `minRecallAt5=0.9305`로 고정).
