# v0.1 (session events) 계획 킥오프 — DRAFT

> **상태: 완료(2026-07-19, session-05에서 승격).** §4 열린 결정 6개는 모두 합의되어
> `context-router-design-v0.1-ko.md`(D14–D20)로 승격되었다. 이 문서는 이력 참조용으로만
> 유지한다. 원래 목적: 설계·게이트 문서에 흩어진 v0.1 유보 항목과 열린 결정의 집결점.

## 1. 목표(피치)

세션 이벤트 기록·복구 — **"무손실 복원"** (vision-proposal 로드맵 v0.1 행).
호스트 세션이 끊기거나 압축돼도 작업 맥락(결정·오류·산출물 포인터)을 도구
호출만으로 재구성할 수 있게 한다.

## 2. 범위 후보 (설계·게이트 문서 승계분)

| 항목 | 근거 | 비고 |
|---|---|---|
| Session DB 스키마 | 설계 §3(D10) — `projects/<pid>/worktrees/<wid>/session.db` 경로 예약됨, worktreeRoot는 v0.0.1이 이미 기록 | Content DB와 분리(D10) |
| 이벤트 3종 도구 계약 | vision-proposal 로드맵: `ctr_record_event` · `ctr_session_summary` · `ctr_export_events` | 계약 상세는 열린 결정 |
| SessionEvent v1 export | 설계 §14 — 설계 기준서 §26 승계 | 포맷(JSONL?) 열린 결정 |
| retention 자동화 스윕 | 설계 §14 — v0.0.1은 `purge --older-than` 수동 | 기본 정책 열린 결정 |
| Session DB 손상 대응 | 설계 §복구 — `.bak` 보존 후 재생성 | Content DB 절차와 대칭 |
| title dedup | 게이트 문서 알려진 갭 — source-단위 title 갱신, 스키마 확장과 함께 | 계획 2 T6 이월 |

## 3. session-04에서 규명된 개선 후보 (v0.1 포함 여부 논의 대상)

| 후보 | 근거(실측) | 예상 규모 |
|---|---|---|
| worker `GOMEMLIMIT`(상한의 ~80%) | windows: GC 커밋 반환 지연 → Job 256MB 도달 → VirtualAlloc errno=1455 OOM(20회 중 7회) | 소 — worker 진입부 1곳 |
| linux RLIMIT_AS에 Go 런타임 VA 예약 여유분 | ubuntu: 할당 없는 워크로드도 8.4ms 조기 사망(주소공간 상한 ↔ VA 예약 충돌) | 소 — 상한 산정식 1곳 + 실측 |
| `worker killed` 사유 구분(취소/메모리/시간) | 게이트 문서 minor + 이번 flake 조사에서 진단 비용 실증 | 소 — ErrSummary 위생 계약 유지 필요 |
| `ctr_fetch` 설명 강화(웹 fetch 오인 방지) | session-03: Fable·Codex 모두 오인 | 소 — 설명 문구(개명은 파괴적이라 보류 권고) |

## 4. 열린 결정 (session-05 브레인스토밍 안건)

1. **이벤트 3종 계약의 상세**: 무엇을 기록하는가(결정/오류/포인터/자유 텍스트?),
   이벤트 스키마(type·payload·ts·세션 식별), 크기 상한, secret redaction 재사용
   (ingest의 denylist/span redaction을 이벤트 payload에도 적용?).
2. **Session DB ↔ Content DB 관계**: 이벤트 payload가 artifact를 참조하는 방식
   (artifact_id 외래 참조? 별도 복사?), 이벤트도 FTS 대상인가(ctr_search 확장 vs
   session_summary 전용).
3. **export 포맷**: SessionEvent v1(JSONL?) — 설계 기준서 §26 원문 재검토 필요.
4. **retention 기본 정책**: 스윕 주기·보존 기간·session vs content 차등.
5. **§3 개선 후보들의 포함 여부**: 특히 GOMEMLIMIT은 transform 신뢰성에 직결 —
   v0.1에 실을지, 별도 패치로 먼저 낼지.
6. **게이트**: v0.1 수용 게이트를 v0.0.1 방식(게이트 문서 + 교차 리뷰)으로 승계할지.

## 5. 진행 방식 제안

session-05: superpowers:brainstorming(위 §4 안건) → 설계 문서 v0.1 절 보강 →
superpowers:writing-plans(태스크 분해) → SDD 실행(교차 모델 리뷰 프로토콜 유지).
