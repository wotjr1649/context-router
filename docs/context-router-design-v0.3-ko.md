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
- **D30** shadow 저장 URI = `shadow:<도구명>:<sha256 hex 12자 접두>` — hook 패시브
  색인 전용 네임스페이스 신설. 다른 콘텐츠 = 다른 행(clobber 소멸), 동일 콘텐츠
  재등장 = 같은 URI 갱신(자연 dedup). `inline:` 네임스페이스는 MCP ctr_index
  표면 계약 그대로(불변). T6 한계(clobber 유발 dangling의 나이 무관 orphan-GC)는
  원인 소멸로 자동 해소 — GC 로직 무변경.
- **D31** shadow 게이트 decode-sniff 승격: C2의 JSON-escape-text 부분문자열
  검사(FP 상한 문서화)를 실제 JSON 디코드 후 바이트 sniff(NUL·바이너리 판정,
  기존 판정기 재사용)로 교체. FP 상한 제거. `CTR_SHADOW_MAX` 캡은 디코드 전
  직렬화 크기에 선적용 유지(디코드 비용 상한).
- **D32** Bash 단일파일 덤프 가드 = D25의 형제: PreToolUse(Bash)에서 명령이
  **정적으로 단일 파일 덤프로 판정 가능한 경우만** D25 4조건 공유 판정 후
  deny + 대체 경로 안내. 판정 불가·복합 명령은 전부 통과(fail-open). v0.2 §4가
  기각한 "명령 휴리스틱 오탐"은 보수적 판정 범위로 원천 차단. Grep 도구는
  호스트 기본 캡이 이미 억제 → 비범위 유지.
- **D33** 계측 가시성: doctor `[12]` drops를 위치×사유 롤업으로, `usage`에
  그룹 집계 2행(TOTAL hooks:on/off) 추가. D27 원칙(실측 합계만, 비율·달러
  환산 주장 없음) 유지. T4(drop-reason 테스트)를 여기 편입.

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

- 생산: shadow 경로가 콘텐츠 sha256을 선계산해 URI `shadow:<도구명>:<hex 12자>`로
  저장한다(기존 CAS 해시 인프라 재사용 — 이중 해시 계산 없이 전달 경로 정리는
  구현 계획에서). 12자 접두(48bit)는 도구별 네임스페이스·저볼륨(임계 초과 출력만)
  전제에서 충분하고, 만에 하나의 충돌도 CAS 갱신으로 수렴한다(오염 아님).
  `runInline`의 `inline:<Title>` 규약은 MCP 표면 그대로.
- 불변식: **다른 콘텐츠는 다른 sources 행** — 같은 도구의 연속 대용량 출력이
  서로를 덮지 않는다(v0.2 §5 T6 한계 해소). 동일 콘텐츠 재등장은 같은 URI로
  ON CONFLICT 갱신(source_kind·indexed_at 갱신 — C6 정합 유지).
- 소비: `RelativizeSource` 등 URI 소비처가 `shadow:` 접두를 인라인 계열로
  인지(경로 상대화 없이 통과). 검색·fetch·provenance 표면은 URI 문자열 외 불변.
- 보존: 마이그레이션 없음 — 구 `inline:<도구명>` 행은 retention/purge가 자연
  정리. 저장 성장은 임계(기본 16KiB) 초과 출력 + 해시 dedup 조건이라 완만 —
  도그푸딩 실측으로 관찰(§5 usage).

## 3. decode-sniff 승격 (D31)

- 순서: 직렬화 크기 캡(`CTR_SHADOW_MAX`) → tool_response 문자열 추출 →
  **JSON 디코드** → 실바이트에 NUL·바이너리 sniff(기존 판정기) → denylist
  대조·`Redact()` → 저장. C2가 문서화한 부분문자열 FP 사례는 이제 통과해야
  한다(게이트 §8).
- 실패 처리: 디코드 불가 응답은 미저장(보수) + drops 사유 `shadow-decode`
  기록(§5 `shadow-oversize` 관례 승계).

## 4. Bash 단일파일 덤프 가드 (D32)

- 판정(전부 정적): 단일 단순 명령(파이프·리다이렉트·체이닝·서브셸·변수 확장
  없음) ∧ 명령어 ∈ {cat, type, Get-Content}(도구 셸에서 자연 발생하는 것만
  실효) ∧ 인자 = 옵션 없는 경로 1개. 하나라도 불성립 → **allow 통과**.
- 판정 성립 시 D25 4조건 공유: 워크스페이스 내 ∧ denylist 아님 ∧ 임계 초과
  (D25와 동일 임계 재사용) ∧ **현장 인덱싱 성공 확인** — 전부 참일 때만
  deny + 대체 경로 안내(ctr_search/ctr_fetch). D25와 동일하게 deny는 모델
  피드백형 소프트 강제, 복귀 경로도 동일(CTR_HOOKS_OFF·임계 상향·uninstall).
- 발화 시 `warning` 이벤트(명령·파일·크기·안내 요지) — 가드 활동이 세션 DB에서
  측정 가능(D25 문면 승계). 기록 실패는 판정에 영향 없음.

## 5. 계측 가시성 (D33)

- doctor `[12]`: drops.log 각 줄(`<ts>\t<reason>`)을 위치별로 사유 롤업 —
  `store-root=1(bad-input=1) worktree=216(unknown-session=216)` 형태. 파싱 불가
  줄은 `unparsed`로 집계(포맷 관용, 명령 중단 없음).
- `usage`: 본표 뒤 집계 2행 — session 열 `TOTAL:hooks:on`/`TOTAL:hooks:off`,
  토큰·records 열은 그룹 합계, hooks 열은 그룹 라벨 반복(열 구조·TSV 불변).
  소비자는 현재 자사 문서(수동 A/B 프로토콜)뿐이라 계약 개정 부담 없음 —
  문서에 행 의미 명기.
- T4 편입: drop 사유별 기록 경로 테스트(unknown-session·bad-input 등)를 이
  변경의 게이트로 흡수.

## 6. 부채 편승 배치 (7건 — session-10 §4.3 / session-11 §4.1 승계)

T1 stale comment, T3 cap-test 119B, T5 matched_pattern attr, T6 shadow 테스트
하드코딩(D30로 일부 자연 해소 — 잔여만), T7 offset/limit-alone 케이스,
T10b fan-out 알파벳 절단, C4 rune-safe truncation. 상세 문면은 해당 세션
기록·리뷰 원장이 정본 — 구현 계획에서 태스크로 전개.

## 7. 보안 계약 (승계 중심)

- 신규 네트워크·신규 입력 표면 없음. shadow 방어 체계(캡·denylist 대조·바이너리
  판정·Redact·canary 게이트)는 v0.2 §5 그대로, D31은 그중 판정 정확도만 올린다.
- D32 가드는 fail-open — 판정·기록 실패가 호스트를 막지 않는다(D23 승계).

## 8. 테스트·수용 게이트

- D30: 동일 도구 상이 출력 2건 → sources 2행 / 동일 출력 재등장 → 1행 갱신 /
  구 `inline:` 행과 공존 / `RelativizeSource`의 `shadow:` 통과.
- D31: C2 문서화 FP 사례 통과 + 실 NUL·바이너리 검출 유지 + canary(비밀 미색인)
  게이트 승계.
- D32: 대형 파일 단순 cat(인덱싱 성공)=deny / 파이프·체이닝 포함=allow /
  경계 밖=allow / 임계 미만=allow / 인덱싱 실패=allow / warning 이벤트 기록.
- D33: 두 위치·복수 사유 롤업 정확성 / usage TOTAL 행 합산 / T4 drop-reason
  케이스.
- 전체 `go test -p 1` GREEN(메모리 캡 규율), gofumpt, CI 3 OS.

## 9. 마일스톤 스케치 (상세는 writing-plans)

① D30 shadow 키 + T6 잔여 → ② D31 decode-sniff → ③ D32 덤프 가드 →
④ D33 doctor/usage + T4 → ⑤ minors 웨이브(§6 잔여) → ⑥ 실호스트 스모크 +
도그푸딩 재설치(marker 0.3.0) + 수동 A/B 재실측.

## 10. 의도적 미결 (v0.4+ 후보)

exec 3종(D21 트랙), Codex 훅 `cx:`(D28), plugin manifest, 무작위 A/B·OTel(D27),
semantic 보강(recall@k 기준선 후), PowerShell 도구 가드(관측 후), Grep 도구
가드(실측 후), spill journal(재상정 조건 §1.3), `repository{}` 기입,
`invalidates`, payload 필드 조회(virtual generated column), title dedup,
CAS 갱신 시 구버전 blob의 즉시 orphan-GC(선존 v0.0.1 동작 — 실해 미관측).
