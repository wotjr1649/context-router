# Session 29 — v0.11 계획·스펙 SDD 전 검토(Fable, 실코드 전수 대조) (2026-07-24)

1. **Session/span/model**: session-29, 2026-07-24, claude-fable-5(단독 —
   서브에이전트·Codex 미사용: 검토 자체가 이 세션의 목적이고 Codex는
   session-28에서 1패스 소진).

2. **Starting prompt (verbatim)**:
   > docs/prompts 최신 기록(session-28)을 읽고 재개. v0.11은 스펙·계획까지 커밋 완료·코드 미착수 상태(main 2104e10). 이번 세션 목표 = **SDD 실행 전 계획·스펙 검토(Fable)**. 대상: 정본 스펙 `docs/context-router-design-v0.11-ko.md`(D58~D62)와 계획 `docs/superpowers/plans/2026-07-24-v0.11-d58-d62.md`(11태스크). 검토 관점: ① 11태스크의 Go 타입·시그니처 정합을 실코드(internal/mcp·transform·cli·cmd)와 전수 대조 ② 러너 캐시 스크래치 재지정 완전성(go GOCACHE/GOMODCACHE·dotnet NUGET_PACKAGES/DOTNET_CLI_HOME·python PYTHONPYCACHEPREFIX 누락 여부) ③ env 닫힌 표가 6러너 실행성과 양립하나 ④ landlock 자기재실행(__exec-launcher) 통합테스트가 무한재귀 없이 실증 가능한가 ⑤ doctor [18]·usage --adoption의 헬퍼 재사용 지점 실존 확인 ⑥ D58~D62·§5 검수 수용 10건이 태스크에 빠짐없이 반영됐나. 발견 시 스펙/계획 fix 또는 사용자에게 질문, 문제 없으면 검토 완료 보고 후 종료(SDD 실행은 그다음 세션). 메모리 캡 테스트 규칙(-p 1) 준수. git add -A 금지. 페이지파일 재활성화됨. **Fable 유지 + 보안 서술 최소화 엄수(검수 벡터 재나열이 세이프가드 재발 원인 — 개념 수준 추상화)**. ultrathink

3. **What was done** (관점 ①~⑥ 전수 수행, 발견 → 문서 fix 반영):
   - **P1 2건**: (④) T3/T4 테스트의 selfExe=`os.Executable()`이 Linux
     wrapArgv 자기재실행에서 테스트 스위트 재귀(무한재귀) — worker_test.go:26
     `testSelfExe` 선례(실 ctr 바이너리 sync.Once 1회 빌드)로 교체, main.go
     `__exec-launcher` 분기 T3로 이동, `SelfExe==""` 무격리 우회 제거
     (fail-closed), landlock 실증 테스트 T3 편입(T6 "축소" 탈출구 삭제 —
     스펙 §2 게이트 충족, 커널 미지원은 `/sys/kernel/security/lsm`로 Skip).
     (②③) dotnet file-based 앱 산출물이 OS temp에 기록 → Unix 쓰기 계약과
     충돌(csharp 실행 불능)·Windows 산출물 잔존(D61 위반) — **공통
     TMPDIR/TEMP·TMP 스크래치 재지정** 신설(스펙 D60 표 + 계획 env 조립).
   - **P2 5건**: D59 WaitDelay 미구현(킬 후 무기한 파이프 대기 행 위험 —
     CommandContext+Cancel+WaitDelay 5s 확정) / Windows Job 캡 미확정
     (4GiB·64 확정 + Spec 주입 필드 + 캡 실증 테스트) / D60 버전 게이트
     서버 수명 캐시 미구현(매 호출 `--list-sdks` 실행이던 것) / 검수 ⑩
     절반 누락(`CTR_WORKTREE` env 미배선 → Request.WorktreeRoot +
     cfg.Canon.WorktreeRoot) / ScratchRoot 스펙-계획 충돌(store-root vs
     D58 OS temp — 스펙 승, `os.TempDir()/ctr-exec-<projectID>`).
   - **P3 다수**: `windows.SysProcAttr`→`syscall.SysProcAttr` / 실존 안
     하는 `TestProfileToolExposure`→`TestNewServerProfileGating`(:183) /
     go-landlock "(기존 의존)" 오기→신규 승인 의존 / `runUsageAdoption`
     `ctx` 재선언 / cli.go `internal/exec` 별칭 import(기존 os/exec 충돌) /
     landlock·sandbox-exec `/dev/null` 쓰기 허용 / 스펙 §2 fixture 전수
     명시(CTR_FILE·C# env 주입·게이트 미달·cold-cache·닫힌 표 스니펫
     레벨) / 스크래치 삭제 실패 warning / usage 테스트 위치·seedCCSession
     실명. 전체 목록 = 계획 말미 "검토 반영 이력" 섹션.
   - **정합 확인(무수정)**: runUsage/runDoctor/loadCCSessions/hostSnippet/
     doctor [17]→[18]/session_events(event_type·ts unix초·summary)/
     toToolError·코드 상수/LedgerAppend·jsonLen/Config{Store,SelfExe,
     Enable,Canon}/maxToolSchemaBytes=12029/AddTool 핸들러 형태/x/sys
     v0.47.0 Job·Toolhelp 심볼 실존/transformWorkerArg main.go:586.
   - 사용자 질문 필요 항목 없음 — 전부 "스펙 승" 규칙 또는 스펙이 계획에
     위임한 결정(캡 기본값·env 최종 표)의 이행이라 문서 fix로 처리.

4. **Repo state**: main = session-28의 2104e10 + 이 세션 검토 커밋(스펙
   D60 표 갱신·계획 검토 반영·본 기록). tag v0.10.0 유지(기릴리스). 오픈
   PR·피처 브랜치 없음. **코드 여전히 미착수** — v0.11은 문서만.

5. **Carryovers**:
   - **다음 세션 = v0.11 SDD 실행**(superpowers:subagent-driven-development,
     11태스크, general-purpose는 `model: "opus"` 명시). 계획의 "검토 반영"
     표기 지점들이 초안 스니펫을 정정하므로 **implementer에게 노트까지
     반드시 전달**(스니펫만 복사하면 정정 누락).
   - 관찰 이월(session-28 그대로): ① D46 139.2MB 회수 — 사용자 수동 창
     (`purge --hook-only --project context-router-4d307dacd75b`) ② empty
     GC 첫 회수 07-27·피크 07-28 ③ D51 합성 경로·cx arm n 정체 관찰 ④
     doctor [17] dirty=untracked 코스메틱.
   - v0.12+ 후보(스펙 §4): ctxscribe 브리지 종료 절차·격리 강화·batch
     재도입·웜 캐시·bun CI.

6. **Standing protocols**: session-28 §6 그대로(Fable 세이프가드 — 검수
   산출물 반영 시 개념 수준 추상화 / 페이지파일 재활성화 / gofumpt 로컬 /
   gh pr checks 전체 출력 / Codex 1패스 가드 / `git add -A` 금지 / go
   test `-p 1`). 추가: **계획 실행 중 스펙-계획 충돌 발견 시 스펙 승**(계획
   자체 규칙 — 이번 검토에서 ScratchRoot로 실증됨).

7. **Next-session starting prompt**:
   > docs/prompts 최신 기록(session-29)을 읽고 재개. v0.11 스펙·계획은 SDD 전 Fable 검토까지 완료(계획 말미 "검토 반영 이력" 참조 — 초안 스니펫을 정정하는 노트가 각 태스크에 박혀 있으니 implementer 프롬프트에 노트 포함 필수). 이번 세션 = **v0.11 SDD 실행**: superpowers:subagent-driven-development로 `docs/superpowers/plans/2026-07-24-v0.11-d58-d62.md` 11태스크 실행(T1부터, 태스크별 fresh 서브에이전트 — general-purpose는 model: "opus" 명시, BASE는 원장 기록). 태스크/통합/최종 리뷰 체크포인트는 서브 리뷰어 + Codex `review --base <BASE>` 병렬 1패스(재리뷰는 서브 단독). 신규 의존은 go-landlock v0.9.0 1건만. 메모리 캡 테스트 규칙(go test -p 1) 준수, gofumpt 로컬 확인, git add -A 금지. Windows 로컬에서 unix/linux 파일은 컴파일 대상이 아니므로 CI 3-OS로 실검증(T11 PR 게이트). **Fable 유지 + 보안 서술 최소화 엄수(exec/sandbox 구현 커밋 메시지·리뷰 문면도 개념 수준 — 벡터 열거 금지)**. ultrathink
