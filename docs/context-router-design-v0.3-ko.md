# Context Router v0.3 설계서 (강제 채널 완성 + 신뢰성)

- 전제: v0.2.0 태그(9b9437f, main CI GREEN), 이 레포에 훅 도그푸딩 설치(프로젝트
  범위, marker 0.2.0). 실측: drops 217건 전부 설계상 정상(설치 시점 실행 중이던
  세션의 unknown-session 버스트 216 + bad-input 1, §1.3), 실손실 0.
- 스코프 확정 근거: session-12 브레인스토밍 → 사용자 3답
  (강제 채널 완성+신뢰성 / Bash 단일파일 덤프 가드만, Grep 비범위 /
  shadow 키 = 콘텐츠 해시).

## 0. 결정 이력 (v0.3 신규 — D28까지는 v0.2 설계서·v0.1·vision-proposal)

- **D29** v0.3 범위 = "강제 채널 완성 + 신뢰성": shadow URI 콘텐츠 해시 키(D30) +
  decode-sniff 승격(D31) + Bash 단일파일 덤프 가드(D32) + 계측 가시성(D33) +
  부채 편승 7건(§6). spill journal은 **판정 종료·재이월** — D23의 기준("drop
  데이터가 필요성을 입증할 때")에 대해 실측이 반증(§1.3). exec 3종은 v0.4 별도
  트랙(D21 유지).
- **D30** shadow 저장 URI = `shadow:<도구명>:<content_hash 64-hex 전체>` — hook
  패시브 색인 전용 네임스페이스 신설. 키 해시는 저장본 content_hash 전체(접두
  절단 없음 — 충돌 시나리오 자체를 제거). 다른 콘텐츠 = 다른 행(clobber 소멸),
  동일 콘텐츠 재등장 = 같은 URI 갱신(자연 dedup). `inline:` 네임스페이스는
  MCP ctr_index 표면 계약 그대로(불변). T6 한계(clobber 유발 dangling의 나이
  무관 orphan-GC)는 원인 소멸로 자동 해소 — GC 로직 무변경.
- **D31** shadow 게이트 decode-sniff 승격: C2의 JSON-escape-text 부분문자열
  검사(FP 상한 문서화)를 **재귀 string-leaf 디코드 후 실바이트 sniff**(NUL·
  바이너리 판정, 기존 판정기 재사용)로 교체 — tool_response는 문자열·객체·배열
  모두 가능(실측 fixture는 `{stdout,stderr}` 객체)하므로 leaf 문자열을 수집해
  각각 판정한다. FP 상한 제거. `CTR_SHADOW_MAX` 캡은 디코드 전 직렬화 크기에
  선적용 유지(디코드 비용 상한). 저장 바이트는 원문 직렬화 그대로(해시·D30 키
  안정성).
- **D32** Bash 단일파일 덤프 가드 = D25의 형제: PreToolUse(Bash)에서 명령이
  **정적으로 단일 파일 덤프로 판정 가능한 경우만**(명령어 `cat` 한정) D25 4조건
  공유 판정 후 deny + 대체 경로 안내. 판정 불가·복합 명령은 전부 통과(fail-open).
  v0.2 §4가 기각한 "명령 휴리스틱 오탐"은 보수적 판정 범위로 원천 차단. 설치
  배선: PreToolUse matcher `"Read"` → `"Read|Bash"`(호스트 정규식 매처, 관리
  그룹 1개 유지 — merge의 동일-이벤트 상호 제거 함정 회피), 디스패치가
  tool_name으로 guardRead/guardBash 라우팅. Grep 도구는 호스트 기본 캡이 이미
  억제 → 비범위 유지.
- **D33** 계측 가시성: doctor `[12]` drops를 위치×사유 롤업으로, doctor `[9]`에
  설치 마커 버전 vs 바이너리 버전 불일치 표시(업그레이드 감지), doctor에
  content.db 규모 행(sources·artifacts·blob 바이트 — shadow 성장 관측 채널),
  `usage --totals` 옵트인 플래그로 그룹 집계 2행(기본 출력 불변 — 이중 집계
  방지). D27 원칙(실측 합계만, 비율·달러 환산 주장 없음) 유지. T4(drop-reason
  테스트)를 여기 편입.

## 1. v0.3 제품 계약

### 1.1 범위

- shadow URI 콘텐츠 해시 키 재설계(§2) + decode-sniff 승격(§3).
- Bash 단일파일 덤프 가드(§4).
- doctor drops 사유 롤업 + usage 집계행(§5).
- 부채 편승 7건(§6).

### 1.2 명시적 비범위 (v0.3)

- spill journal — 판정 종료(§1.3 실측 반증). drop 실측이 뒤집히면 재상정.
- exec 3종 — v0.4 별도 트랙(D21). Codex 훅 `cx:`(D28), plugin manifest,
  무작위 A/B 하네스·OTel(D27), semantic 보강 — 이월 유지.
- Grep 도구 출력 가드 — 호스트 기본 캡(250줄)이 억제, 실측 필요성 미입증.
- PowerShell 도구 가드 — 훅 matcher·tool_input 계약 관측 부족, 관측 후 상정.

### 1.3 spill journal 판정 데이터 (D23 후속 확정)

도그푸딩 실측(2026-07-21): drops 총 217건 = worktree 216건 전부
`unknown-session`(설치 시점에 이미 실행 중이던 세션의 87분 단일 버스트 —
§2.2 설계 그대로, SessionStart 미발화 세션의 후속 이벤트 drop) + store-root
`bad-input` 1건(T11 임시 capture 훅 실험 창). 정상 운영 중 기록 실패로 인한
drop은 0건. spill journal이 구제할 대상이 관측되지 않았으므로 도입하지 않는다.

## 2. Shadow URI 재설계 (D30)

- 생산: shadow 경로가 저장본 content_hash(sha256 64-hex 전체)를 키로 URI
  `shadow:<도구명>:<64-hex>`를 조립한다. 전체 해시 채택 근거: 접두 절단은 충돌
  시 ON CONFLICT(uri)가 last-writer-wins로 기존 행을 덮어 recall 손실을 만들 수
  있다(CAS 아님) — 전체 해시는 그 시나리오 자체를 제거하고, URI 길이 비용은
  provenance 표시에 국한된다. 구현은 runInline의 URI 조립에 SourceKind=="hook"
  분기(또는 동등한 공유 함수 분기)가 필요 — `inline:<Title>` 규약은 MCP 표면
  그대로(불변), 상세 배선은 구현 계획에서.
- 불변식: **다른 콘텐츠는 다른 sources 행** — 같은 도구의 연속 대용량 출력이
  서로를 덮지 않는다(v0.2 §5 T6 한계 해소). 동일 콘텐츠 재등장은 같은 URI로
  ON CONFLICT 갱신(source_kind·indexed_at 갱신 — C6 정합 유지).
- provenance 단일 표시 규약 승계: 다중 source가 같은 artifact를 가리키면
  검색·fetch는 uri ASC 첫 행을 결정적으로 표시한다(v0.0.1 α6 계약). 따라서 구
  `inline:<도구>` 행이 잔존하는 동안 동일 콘텐츠의 표시 라벨이 구 행일 수 있다
  — recall·byte-exact 회수에는 무영향, 수동 purge로 소멸. 표시 우선순위 재설계는
  §10 이월.
- 소비: `RelativizeSource` 등 URI 소비처가 `shadow:` 접두를 인라인 계열로
  인지(경로 상대화 없이 통과). 검색·fetch·provenance 표면은 URI 문자열 외 불변.
- 보존: 마이그레이션 없음 — 단, content.db에 자동 retention은 없다. 구
  `inline:<도구명>` 행과 누적 shadow 행의 회수 경로는 **수동
  `purge --older-than`뿐**(자동 회수 없음 — 운영 문서에 명기). 저장 성장의
  "완만" 여부는 주장하지 않고 관측으로 판정한다 — 관측 채널은 doctor의
  content.db 규모 행(D33). shadow 전용 자동 캡/sweep은 실측이 필요성을 입증하면
  상정(§10 이월).

## 3. decode-sniff 승격 (D31)

- 순서(현행 v0.2 §5 게이트 순서 승계, sniff 단계만 교체): OFF → MIN → MAX
  (직렬화 크기 캡) → 파일 유래 denylist 대조(디코드 전 단락 — drop 사유 집계
  순서 불변) → **재귀 string-leaf 디코드·sniff** → `Redact()` → 저장.
- 재귀 leaf 계약: `tool_response`는 hook.Run의 외부 파싱을 통과한 유효 JSON이다
  (문자열·객체·배열 모두 가능 — 실측 fixture는 `{stdout,stderr}` 객체). leaf
  문자열 값을 재귀 수집해 디코드된 실바이트 각각에 NUL·바이너리 sniff(기존
  판정기)를 적용하고, 하나라도 비텍스트면 조용히 미저장(현행 관례). 유효 JSON
  전제에서 디코드는 실패하지 않으므로 별도 drop 사유는 두지 않는다(도달 불가
  사유 금지).
- 저장 바이트: 직렬화 원문 기준(현행 경로 그대로 — `Redact()` 통과본이 저장됨,
  v0.2 §5 불변) — 저장 계약·해시(D30 키) 안정성 유지, 디코드본은 판정에만 쓴다. C2가 문서화한 부분문자열 FP 사례(NUL 이스케이프
  시퀀스를 텍스트로 논하는 콘텐츠)는 이제 저장되어야 한다(게이트 §8).

## 4. Bash 단일파일 덤프 가드 (D32)

- 판정(전부 정적): 단일 단순 명령(파이프·리다이렉트·체이닝·서브셸·변수 확장
  없음) ∧ 명령어 = `cat` ∧ 인자 = 옵션 없는 **절대경로** 1개. 하나라도 불성립 →
  **allow 통과**. 상대경로는 판정 제외 — 훅 프로세스 cwd와 도구 실행 cwd가
  어긋나면 다른 파일을 판정할 수 있어(오탐 deny), cwd 추론 대신 절대경로
  한정으로 §7의 "최대 피해 = 가드 미발화" 단언을 유지한다. `type`은 bash에서
  명령 조회라 덤프가 아니고(오탐 시나리오 — 큰 워크스페이스 경로 인자여도
  deny하면 안 됨), `Get-Content`는 PowerShell 도구 트랙(§1.2 비범위)이라
  집합에서 제외.
- 판정 성립 시 D25 4조건 공유: 워크스페이스 내 ∧ denylist 아님 ∧ 임계 초과
  (D25와 동일 임계 재사용) ∧ **현장 인덱싱 성공 확인** — 전부 참일 때만
  deny + 대체 경로 안내(ctr_search/ctr_fetch). D25와 동일하게 deny는 모델
  피드백형 소프트 강제, 복귀 경로도 동일(CTR_HOOKS_OFF·임계 상향·uninstall).
- 발화 시 `warning` 이벤트(명령·파일·크기·안내 요지) — 가드 활동이 세션 DB에서
  측정 가능(D25 문면 승계). 기록 실패는 판정에 영향 없음.
- 설치 배선: hookRegistrations의 PreToolUse matcher `"Read"` → `"Read|Bash"`
  (호스트 정규식 매처) — 관리 그룹 1개를 유지해 merge의 동일-이벤트 상호 제거
  함정(isOurHookGroup이 matcher 비교 없이 마커+명령 토큰으로 소유 판정)을
  회피한다. 디스패치는 tool_name 기준 guardRead/guardBash 라우팅.
- 업그레이드 계약: 바이너리 교체만으로 settings는 갱신되지 않는다 —
  `hook install` 재실행이 공식 업그레이드 경로(멱등 병합이 그룹 교체 + 마커
  갱신)이며, doctor가 마커 버전 불일치를 표시한다(D33).

## 5. 계측 가시성 (D33)

- doctor `[12]`: drops.log 각 줄(`<ts>\t<reason>`)을 위치별로 사유 롤업 —
  `store-root=1(bad-input=1) worktree=216(unknown-session=216)` 형태. 파싱 불가
  줄은 `unparsed`로 집계(포맷 관용, 명령 중단 없음). 기존 정확-문자열 단정
  테스트(hook_install_test.go의 `[12]` 행)는 신형식으로 교체.
- doctor 추가: ① `[9]` hooks 행에 설치 마커 버전 표기 + 바이너리 버전 불일치
  시 재설치 안내(업그레이드 감지) ② content.db 규모 행(sources·artifacts·blob
  바이트) — shadow 성장 관측 채널(§2 보존).
- `usage --totals`(옵트인 플래그): 본표 뒤 집계 2행 — session 열
  `TOTAL:hooks:on`/`TOTAL:hooks:off`, 토큰·records 열은 그룹 합계, hooks 열은
  그룹 라벨 반복. **기본 출력은 불변**(행 = transcript 세션 1:1 계약 유지 —
  합성 행의 이중 집계·행 문법 파괴를 원천 차단). 수동 A/B 프로토콜 문서가
  `--totals` 사용을 안내.
- T4 편입: drop 사유별 기록 경로 테스트(unknown-session·bad-input 등)를 이
  변경의 게이트로 흡수.

## 6. 부채 편승 배치 (7건 — session-10 §4.3 / session-11 §4.1 승계)

T1 stale comment, T3 cap-test 119B, T5 matched_pattern attr, T6 shadow 테스트
하드코딩(D30로 일부 자연 해소 — 잔여만), T7 offset/limit-alone 케이스,
T10b fan-out 알파벳 절단, C4 rune-safe truncation. 상세 문면은 해당 세션
기록·리뷰 원장이 정본 — 구현 계획에서 태스크로 전개.

## 7. 보안 계약 (승계 중심)

- 신규 네트워크 표면 없음. shadow 방어 체계(캡·denylist 대조·바이너리
  판정·Redact·canary 게이트)는 v0.2 §5 그대로, D31은 그중 판정 정확도만 올린다.
- D32는 Bash 명령 문자열을 판정 입력으로 새로 파싱한다(신규 신뢰 경계 1건) —
  파서는 정적 확신이 있을 때만 deny하고 그 외 전부 allow이므로, 오동작의 최대
  피해는 "가드 미발화"다. 판정·기록 실패가 호스트를 막지 않는다(D23 승계).

## 8. 테스트·수용 게이트

- D30: 동일 도구 상이 출력 2건 → sources 2행 / 동일 출력 재등장 → 1행 갱신 /
  구 `inline:` 행과 공존 + 단일 표시 결정성(uri ASC — α6 확장 케이스) /
  `RelativizeSource`의 `shadow:` 통과.
- D31: 객체 fixture(`{stdout,stderr}`)·배열·문자열 각각의 leaf sniff / C2 문서화
  FP 사례 저장 통과 / leaf 내 NUL·바이너리 미저장 / denylist 단락이 sniff보다
  선행(drop 사유 집계 순서) / canary(비밀 미색인) 게이트 승계.
- D32: 대형 파일 단순 cat(인덱싱 성공)=deny / 파이프·체이닝·인용·공백 경로
  변형=allow / bash `type` 명령=allow(비덤프) / 경계 밖=allow / 임계 미만=allow /
  인덱싱 실패=allow / warning 이벤트 기록.
- D32 설치: v0.2 settings 위 재설치 → PreToolUse 관리 그룹 1개·matcher
  `Read|Bash`·Read 가드 잔존(기존 `want 4`·`matcher=="Read"` 단정 교체) /
  실호스트 스모크에서 matcher 정규식 동작 확인(§9 ⑥).
- D33: 두 위치·복수 사유 롤업 정확성 / usage 기본 출력 불변 + `--totals` 합산 /
  doctor 마커 불일치 표시·content.db 규모 행 / T4 drop-reason 케이스.
- 전체 `go test -p 1` GREEN(메모리 캡 규율), gofumpt, CI 3 OS.

## 9. 마일스톤 스케치 (상세는 writing-plans)

⓪ matcher `Read|Bash` 정규식 실호스트 프리체크(§4 배선의 구조적 전제 —
실패 시 폴백: merge를 (event,matcher) 키로 확장해 그룹 2개 등록) →
① D30 shadow 키 + T6 잔여 → ② D31 decode-sniff → ③ D32 덤프 가드 →
④ D33 doctor/usage + T4 → ⑤ minors 웨이브(§6 잔여) → ⑥ 실호스트 스모크 +
도그푸딩 재설치(marker 0.3.0) + 수동 A/B 재실측.

## 10. 의도적 미결 (v0.4+ 후보)

exec 3종(D21 트랙), Codex 훅 `cx:`(D28), plugin manifest, 무작위 A/B·OTel(D27),
semantic 보강(recall@k 기준선 후), PowerShell 도구 가드(관측 후), Grep 도구
가드(실측 후), spill journal(재상정 조건 §1.3), shadow 전용 자동 캡/sweep
(doctor 규모 실측 후 — §2 보존), 다중 source provenance 표시 우선순위 재설계
(§2 승계 한계), `repository{}` 기입, `invalidates`, payload 필드 조회(virtual
generated column), title dedup, CAS 갱신 시 구버전 blob의 즉시 orphan-GC(선존
v0.0.1 동작 — 실해 미관측).

## 11. 적대 검증 처리 기록 (2026-07-21, 설계 체크포인트)

서브에이전트(opus) P1×2·P2×2·Minor×4 + Codex(교차 모델) P1×4·P2×3 병렬 1패스 —
중복 제거·실코드 검증 후 병합 반영:

- 채택(설계 수정): 설치 배선 누락 + merge 동일-이벤트 상호 제거 함정(→ matcher
  `Read|Bash` 단일 그룹, §4) / D31 객체 입력 미정의·순진 구현 시 Bash 출력 전부
  drop·`shadow-decode` 도달 불가(→ 재귀 leaf 계약·사유 철회, §3) / "자연
  정리"·"완만 성장"·"usage 관찰" 과장(→ 수동 purge 명시·doctor 규모 행, §2·§5) /
  12-hex 충돌은 CAS 아닌 last-writer-wins(→ 전체 64-hex, §2) / bash `type` 오탐
  시나리오·`Get-Content` 스코프 모순(→ `cat` 한정, §4) / usage TOTAL 이중
  집계(→ `--totals` 옵트인, §5) / 명령 파싱 신규 신뢰 경계(§7) / doctor
  정확-문자열 단정 교체·업그레이드 감지(§5·§8).
- 하향 판정 1건: "다중 source provenance 오귀속"(Codex P1) — 실코드 확인 결과
  uri ASC 단일 표시는 v0.0.1 α6의 의도된 결정성 계약이고 recall·byte-exact
  회수는 무손실. 표시 라벨 한계로 재분류해 §2에 문서화, 우선순위 재설계는 §10
  이월.
- 두 리뷰어는 이번에도 상보적(설치 함정은 양쪽 발견, provenance·업그레이드·이중
  집계는 Codex 단독, 명령 집합 셸 모순·doctor 단정은 서브에이전트가 먼저) —
  이중 최종 리뷰 프로토콜 유지 근거 추가.
- 재검토(서브에이전트 단독, fix-fidelity): **Ready** — 채택 (a)~(g) 전부 반영
  확인 + 실코드 스팟체크 3건 성립. 보강 3건 반영: D32 절대경로 한정(상대경로
  cwd 불일치의 오파일 판정 차단, §4), matcher 정규식 프리체크를 ⓪로 전진 +
  폴백 명기(§9), §3 저장 바이트 문구를 Redact 통과본 기준으로 명확화.
