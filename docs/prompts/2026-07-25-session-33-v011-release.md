# Session 33 — v0.11 SDD T5~T11 완료 + 릴리스 (2026-07-24~25)

1. **Session/span/model**: session-33, 2026-07-24 밤~07-25 새벽. Fable 5(xhigh)로 T5~T10
   실행·리뷰·CI 초기 라운드 진행, 세션 중반 사용자 `/model`로 Opus 4.8(1M, max)로 전환해
   macOS CI 격리 디버깅·릴리스 마무리. 구현/리뷰/리서치/rescue 서브에이전트 전부 model:"opus".

2. **Starting prompt (verbatim)** — session-32 next-session 프롬프트로 부팅:
   > docs/prompts 최신 기록(session-32)을 읽고 재개. v0.11 SDD 실행 중 — 브랜치 `feat/v0.11-d58-d62`, **T1·T2·T3·T4 구현 완료, 다음 = T5부터, BASE=7c94b61**. superpowers:subagent-driven-development 계속(계획 `docs/superpowers/plans/2026-07-24-v0.11-d58-d62.md`, 태스크별 fresh 서브에이전트 general-purpose는 model:"opus" 명시, 계획 각 태스크의 "검토 반영" 노트를 implementer에 필수 전달, BASE는 원장 기록). T5(mcp 등록 — exec 프로필 게이트+프로브+도구 2종+sentinel 배선, 계획 §Task 5). 다운스트림 계약: post-Start 취소=(Result,nil) vs pre-Start=ctx 오류 → T5는 ctx.Err() 참조. **크로스모델 리뷰는 근본 재설계됨(session-32, hook 폐기)**: `docs/codex-secure-review.md`대로 — Codex `review --background`로 시작(컨트롤러 result 미호출)·단일 리뷰 서브(opus)가 Codex 원문 자기 컨텍스트 소비+실 diff 대조 후 **고정 스키마 JSON findings**로 통합·컨트롤러는 저밀도 JSON만 소비·fix 서브를 JSON 파일에 조준. 중립 프레이밍(정확성·계약·리소스 안전), 위협 서술 금지. 재리뷰 서브 단독(Codex 1패스/체크포인트). 태스크/통합/최종 리뷰 동일. go test -p 1·gofumpt 로컬·git add -A 금지. Windows 로컬 미컴파일 unix는 CI 3-OS 실검증(T11). **Fable 유지 + 보안 서술 최소화(벡터 열거 금지)**. ultrathink

3. **What was done — v0.11 D58~D62 전 태스크 완료 + 릴리스**:
   - **T5** (base 9116da2, commits `148f00f`+`c9caa3c`, 1 fix): mcp `ctr_execute`/`ctr_execute_file`
     등록(exec 프로필 + sandbox.Probe 이중 게이트), sentinel 4종+ErrSetup 매핑, `Config.ScratchRoot`,
     ctx.Err() 다운스트림 계약. fix=validateExecPath 정규파일 검증(IsRegular)+테스트.
     **크로스모델 새 프로토콜 무사고 첫 적용**(Codex --background + 단일 리뷰 서브 JSON findings).
   - **T6** (`c5855ad`, 0 fix): main `--enable exec`·ScratchRoot(OS temp)·기동 SweepStale.
   - **T7** (`065f670`, 0 fix): `usage --adoption [--days N]` 채택 계측(절대 호출 수 1차, D62).
   - **T8** (`e3e04f8`+`fd5aaaa`, 1 fix): doctor `[18]` 러너 감지 + hostSnippet exec 안내.
     fix=ask 접두 `mcp__ctr-exec__*` 정합(스펙 §5 이중 동의).
   - **T9** (`14eb4a3`+`44b115e`, 1 fix): D62 ctr_search·ctr_fetch 설명 개정(§3.5 비혼동 보존) +
     프로필 테이블 exec 행 + `maxToolSchemaBytes` 12029→13000(실측 12864). fix=exec 행 프로브 인지.
   - **T10** (`331eae7`, 0 fix): 아키텍처 문서 §2 의존그래프(sandbox·exec)·§6 sentinel(exec 4+sandbox 1) +
     CI setup-dotnet(3-OS 무조건).
   - **최종 whole-branch 이중 리뷰**(서브 opus + Codex --base 1223504): ready_to_merge, 스펙
     커버리지 D58~D62 전부 ✅. must_fix 2 fix웨이브: `b2d93c0`(NOTICE/THIRD-PARTY-NOTICES —
     go-landlock·libcap/psx 추기, GOOS 합집합 28모듈 전수) + `bc15bad`(productVersion 0.11.0, D56).
   - **PR #23 → CI 격리결함 4라운드 근본해소**(Windows 로컬 미컴파일 unix를 CI가 실검증 — 예견된 경로):
     `474c637`(lint SA4032 도달불능 GOOS 분기), `3c6d04a`(RC-A probe `cmd.WaitDelay`=1s / RC-B
     darwin SBPL `filepath.EvalSymlinks` — /var→/private/var 심링크 / RC-C dotnet named mutex
     회피 — XDG_DATA_HOME+NuGet Migrations/1 sentinel), `43e7d03`(macOS Application Support —
     HOME+CFFIXED_USER_HOME=scratch/home), `59496dc`(macOS LocalApplicationData — scratch/home/
     Library/Application Support MkdirAll + DOTNET_GENERATE_ASPNET_CERTIFICATE=false). **전부 격리
     무손상**(모든 쓰기 scratch subpath 내, SBPL/landlock 규칙 무변경). 3회 whack-a-mole 후
     협업(웹 리서치 + Codex rescue) 독립 진단 수렴으로 근본 확정. 서브 재리뷰 approved(격리 보존).
   - **릴리스**: PR #23 머지=merge commit `9f58277`, tag `v0.11.0`=`67b65b0`(annotated), 태그 CI
     ALL GREEN(**tag-version success = D56 실태그 검증**). 도그푸딩: `version`=0.11.0, doctor
     `[17]` build 0.11.0, `[18]` exec runners **6언어 전부 감지**(shell=pwsh/7 js=bun ts=bun
     py=python3 go=go cs=dotnet/10+). exec 러너 실행은 3-OS CI(TestRunGo/CS/TS/Py 등)가 실증.

4. **Repo state**: main HEAD=`9f58277`(merge #23), tag `v0.11.0`=`67b65b0`, 브랜치
   `feat/v0.11-d58-d62`(머지됨, 미삭제). **오픈 PR 없음**. tag CI 전체 GREEN. 이전 tag v0.10.0 유지.

5. **Carryovers**:
   - **도그푸딩 잔여(사용자 액션)**: ① `context-router hook install`(cc) + `hook install --codex --user`(cx)로
     marker 0.11.0 갱신 — doctor `[16]`이 현재 marker 0.10.0≠0.11.0 표시 ② MCP `--enable exec`
     재기동으로 클라이언트 경유 `ctr_execute`/`ctr_execute_file` 실사용 ③ cx `/hooks` 재신뢰(0.11.0).
   - **Minor triage 이월**: `.superpowers/sdd/final-review-findings.json` defer 9건(T1 심링크 가드
     테스트·T3 ABI 하강루프 V3 dead·refer rename CI 커버·freebsd 크로스빌드 transform/worker_unix.go
     RLIMIT_AS·psx v1.2.77 인간확인·T4 M1~M10·T7 rows.Err/음수 days·T10 sandbox.go stale 주석 등),
     closed 6(T6 F1/F2·T7 F1 plan_conflict 존중, T3 deadline TimedOut 코드 처리 확인). v0.12+ 후보.
   - **관찰 이월(session-28~)**: D46 139MB 회수(사용자 창), empty GC 회수, doctor dirty=untracked
     .claude/.codex 코스메틱.
   - 다음 = v0.12 스코프 브레인스토밍 or 도그푸딩 마무리 관측(사용자 지시 대기).

6. **Standing protocols** (유지):
   - **크로스모델 리뷰 = 중립 프레이밍 + 구조화 JSON findings**(hook 폐기, session-32 근본재설계).
     이번 세션 11 태스크 리뷰 + 최종 리뷰 전부 무사고 적용 — 프로토콜 실증됨. Codex rescue 원문도
     중립 엔지니어링 진단이라 컨트롤러 직접 읽어도 무트립(review 원문과 구분). 상세 `docs/codex-secure-review.md`.
   - **막히면 협업(3회 실패 STOP)**: 이번 macOS CI가 정확한 사례 — whack-a-mole 3회 후 웹 리서치(라이브러리
     사실)+Codex rescue(다른 관점) 독립 진단 수렴으로 근본 확정, blind 4번째 회피. [[stuck-protocol-codex-web]].
   - **CI 3-OS = unix 실검증 권위**: Windows 로컬 미컴파일 격리 코드는 CI가 유일 검증. `gh pr checks` **전체**
     확인(tail 잘림 금지 — v0.10 교훈), 태그 후 tag-version 잡까지 확인.
   - 유지: general-purpose 서브 model:"opus" 명시 / go test -p 1 / gofumpt 로컬 / `git add -A` 금지 /
     Codex 1패스/체크포인트(재리뷰 서브 단독) / 스펙 승 / "검토 반영" 노트 implementer 필수 / BASE 원장 /
     Fable + 보안 서술 최소화(벡터 열거 금지).

7. **Next-session starting prompt**:
   > docs/prompts 최신 기록(session-33)을 읽고 재개. **v0.11 릴리스 완료** — main=`9f58277`(merge #23),
   > tag `v0.11.0`(태그 CI ALL GREEN, tag-version success=D56 검증). D58~D62 전부 구현·이중리뷰·3-OS CI
   > 실검증 완료(격리 결함 4라운드 근본해소, 전부 격리 무손상). **다음 후보**: ① 도그푸딩 마무리(사용자 액션
   > — `hook install` cc/cx marker 0.11.0 갱신·MCP `--enable exec` 재기동 실사용·cx `/hooks` 재신뢰,
   > doctor `[16]` 0.10.0≠0.11.0 지적) ② v0.11 실관측(exec 실사용·adoption 베이스라인) ③ v0.12 스코프
   > 브레인스토밍. 이월 Minor(`.superpowers/sdd/final-review-findings.json` defer 9)는 v0.12+ triage.
   > 크로스모델 리뷰=중립 프레이밍+JSON findings(`docs/codex-secure-review.md`), 막히면 웹+Codex 협업,
   > CI 3-OS가 unix 실검증 권위(`gh pr checks` 전체 확인). Fable 유지+보안 서술 최소화. 사용자 지시 대기.
