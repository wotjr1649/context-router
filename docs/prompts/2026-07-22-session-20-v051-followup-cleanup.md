# Session 20 — 수동 정리(스테일 0.1.0 등록 차단) + v0.5.x follow-up 웨이브 → v0.5.1 머지·태그·도그푸딩 갱신

- **Span:** 2026-07-22
- **Model:** Fable 5 (컨트롤러). Subagents (opus, explicit `model:`): 구현자
  ×1(번들 F1), 태스크 리뷰어 ×1. Codex: `review --base 5adc181` 1패스(병렬).
- **One line:** 스테일 ctr 0.1.0 등록 2곳(Codex config + Claude `.mcp.json` —
  후자는 기록 밖 신규 발견) 차단 → §4.1 백로그 8건+0.5.1 범프를 1구현자
  번들로 실행 → 이중 리뷰(서브에이전트 Approved 0C/0I/3M ACCEPT, Codex 확정
  버그 0) → CI 3-OS GREEN → PR #16 머지 8cdec1e, tag v0.5.1, marker 0.5.1.

## 1. Starting prompt (verbatim — original Korean, do not translate)

```
context-router 다음 작업을 진행한다. 먼저 docs/prompts/2026-07-22-session-19-v05-implementation.md(최신 기록)를 정독한다. 상태: main 90ce825(+기록 커밋), tag v0.5.0 릴리스 완료, 도그푸딩 Claude marker 0.5.0 + Codex A/B 가동, 빈 세션 GC·[15] 접두 분해·purge --hook-only 실배포.
후보(우선순위는 사용자와 확인): ① v0.5.x follow-up 웨이브(기록 §4.1 백로그 — 전부 비차단 minors, 1구현자 번들 선례) ② v0.6 신규 범위 브레인스토밍(cx: A/B 축적 데이터 활용처, §10 잔여) ③ 수동 정리 안내(스테일 mcp_servers.ctr 차단 등).
메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지. 페이지파일 의도적 비활성(사용자 확정 — 재권고 금지): 무거운 프로세스 상시 직렬화. Fable 유지, 보안 서술 최소화 준수. ultrathink
```

사용자 선택: ③ 정리 안내 → ① follow-up 순차.

## 2. What was done

**1) 수동 정리 (후보 ③):** 조사에서 기록 밖 사실 발견 — repo-root
`ctr.exe`(0.1.0, 07-20 빌드, hook-only 등 v0.4+ 기능 전무)를 Codex
`[mcp_servers.ctr]`("v0.0.1 스모크용 임시 등록" 주석 잔존)와 **Claude
`.mcp.json`도** 절대경로로 참조 — 즉 지금까지 세션들의 ctr MCP 서버가
0.1.0이었고 이것이 빈 세션 재생산 근원. 진짜 설치본은
`go\bin\context-router.exe`(훅 양쪽은 bare 호출로 이미 정상, cc:/cx: 축적이
증거). 조치: Codex 섹션 제거 + 스테일 "ctr quote probe" projects trust 제거,
`.mcp.json`을 doctor 스니펫 권장대로 bare `"context-router"`로 교체,
`hook install --codex` 재실행(statusMessage 0.4.0→0.5.0). scratchpad 프로브
2곳은 사용자 삭제 완료, C:\tmp는 이미 정리 상태였음. repo-root ctr.exe는
세션 중 프로세스 잠금 때문에 **세션 종료 후 사용자 직접 삭제 예정**.

**2) follow-up 웨이브 (후보 ①):** 브랜치 feat/v0.5.x-followup(BASE 5adc181,
레저 기록), 브리프 `.superpowers/sdd/task-followup-brief.md`(9항목 = §4.1
8건 + 0.5.1 범프), opus 구현자 1기 번들. 커밋 체인: 8446a79(items 2·3:
FailedFiles 관측+FTS pre-state 핀) → 32a7fdb(4·7: sanitize 테스트+
shadowAppend event/tool) → dd12afe(1·6: [15] 분해 4케이스+confirm hook-only
문구) → 76ac83d(5·8: 주석 교정+D41 각주) → 19ae817(0.5.1 범프). 전체 회귀
`-p 1` 11패키지 ok, TDD 증거(2·6·7), 아이템 1 RED 발견 없음(구현 무변경
확인).

**3) 체크포인트 이중 리뷰(병렬, 단일 태스크라 태스크=최종 병합):**
서브에이전트(opus) Approved — 0 Critical/0 Important/3 Minor 전부
ACCEPT(field[2] 명시 단언 폴리시, store.go:1128 긴 주석, dd12afe 묶음 투명성
노트). pre-state 핀 비공허성·trigram N/A 판정·주석-소스 대조를 독립 검증.
Codex: 확정 버그 0, `git diff --check` 통과(테스트 실행은 승인 정책 차단 —
구현자 실행분이 증거). fix 웨이브 불요.

**4) 릴리스:** 컨트롤러 `gofumpt -l .` 사전 확인 GREEN(세션-19 처방) →
PR #16 → CI 양 런 3-OS test+crossbuild+lint 전부 pass → 머지
**8cdec1e**(10파일 +228/−18) → tag **v0.5.1** → `go install` +
`hook install`(Claude·Codex) → doctor [9] marker 0.5.1, [10]
go\bin\context-router.exe, [15] cc:=15.4MB/cx:=768KB/shared=0B/
unattributed=0B, [6] sessions=92 (empty=67), [12] drops total=325
(unknown-session=324 — 관측 소재).

## 3. Current repo state

- `main` @ 8cdec1e(+이 기록 커밋), tag **v0.5.1**. 오픈 PR·피처 브랜치 없음.
- 도그푸딩: marker 0.5.1. `.mcp.json` bare 등록 — **다음 Claude 세션부터
  0.5.1 서버**(이번 세션의 mcp__ctr__*는 스테일 0.1.0 프로세스라 미사용).
  Codex는 MCP 등록 제거(훅 A/B만 유지).

## 4. Carryovers / next work

1. **v0.6 신규 범위 브레인스토밍** — 유일한 대형 후보. cx: A/B 축적 데이터
   활용처, §10 잔여. [12] drops unknown-session=324도 관측 소재.
2. repo-root `ctr.exe` 삭제 — 사용자, 세션 종료 후(프로세스 잠금).
3. purge --hook-only 실사용 여부 사용자 결정(이월; [15] 16.1MB, [14] blob
   22.4MB — 100MiB 경고 한참 아래).
4. empty=67 관측 — 재생산 근원 차단 완료, 7일 GC 회수 확인 포인트.
5. F1 리뷰 Minor 3건은 ACCEPT 종결(재작업 불요).

## 5. Standing protocols (delta only — rest as session-19 §5)

- **스테일 호스트 등록 감사**: doctor [10]의 실행 바이너리 경로와 호스트
  등록(.mcp.json·config.toml)의 command 경로가 다르면 스테일 —
  bare `context-router`(doctor 스니펫 그대로)로 통일이 정답. 절대경로 등록은
  재설치마다 썩는다.
- **단일 태스크 웨이브의 체크포인트 병합**: 태스크와 최종이 같은 diff면
  태스크 리뷰(서브에이전트)+Codex 1패스를 한 체크포인트로 병렬 실행.
- 페이지파일 비활성·무거운 프로세스 직렬화·`-p 1` 그대로 유지.

## 6. Next-session starting prompt (paste-ready)

```
context-router 다음 작업을 진행한다. 먼저 docs/prompts/2026-07-22-session-20-v051-followup-cleanup.md(최신 기록)를 정독한다. 상태: main 8cdec1e(+기록 커밋), tag v0.5.1, 도그푸딩 marker 0.5.1(.mcp.json bare 등록 — 이번 세션부터 0.5.1 서버 접속), Codex MCP 등록 제거(훅 A/B만 유지), 빈 세션 재생산 근원 차단(empty=67 → 7일 GC 회수 관측).
주 후보: v0.6 신규 범위 브레인스토밍(superpowers:brainstorming) — 소재: cx: A/B 축적 데이터 활용처, §10 잔여, doctor [12] drops unknown-session=324 관측. v0.5.x 백로그는 소진됨.
메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지. 페이지파일 의도적 비활성(사용자 확정 — 재권고 금지): 무거운 프로세스 상시 직렬화. Fable 유지, 보안 서술 최소화 준수.
```
