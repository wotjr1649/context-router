# Session 50 — session-49 §7 감사 · 호스트 설정 (v0.18 구현 직전)

## 1. 세션 정보

- **번호**: 50 (앞: session-49 = v0.17.1·v0.17.2 릴리스 + v0.18 설계·계획)
- **기간**: 2026-08-05
- **모델/노력**: Opus 5 (1M context), ultrathink
- **결과**: **코드 무변경.** session-49 §7의 다음 세션 프롬프트를 현재 저장소·`CLAUDE.md`와
  대조해 **넷을 정정**하고, 그 수정본을 이 레코드 §7에 싣는다. 부수로 호스트 설정 둘.

## 2. 시작 프롬프트 (원문)

> 로컬 셋팅 파일을 만들고 lsp 2개를 활성화 시키는건 어떤지 확인 요청

중간 지시 넷:

> gopls-lsp만 local 스코프로 켠다.

> 추가한다. (`.gitignore`에 `.claude/`)

> .codex/도 추가한다.

> 다음 세션에서 @docs/prompts/2026-08-01-session-49-v0.17.1-v0.17.2-v0.18-design.md 를
> 그대로 진행해도 되는가?

> $7 프롬프트에 위 넷을 반영한 수정본을 만든다. ultrathink

## 3. 한 일

### 3.1 호스트 설정 둘 (저장소 코드 무관)

- **`gopls-lsp`를 local 스코프로 활성화** — `.claude/settings.local.json`에
  `enabledPlugins`가 붙었다. `typescript-lsp`는 **켜지 않았다**: 이 저장소의 `.ts`는
  `testdata/oracle/corpus/db_pool.ts` 하나뿐인 픽스처다(`.py`도 같은 자리 하나).
  실측: `~/.claude/settings.json`은 md5 불변 — 전역은 건드리지 않았다.
- **`.gitignore`에 `.claude/`·`.codex/`** — 둘 다 untracked이면서 ignore가 아니었으므로
  `git add .` 한 번이면 `settings.local.json`과 `review_*.diff` 셋이 커밋에 들어갔다.
  `.superpowers/`·`.mcp.json`과 같은 범주(호스트가 재작성하는 로컬 설정)다.

### 3.2 ★★ session-49 §7 감사 — 넷을 정정한다

**그대로 붙여넣으면 안 되는 상태였다.** 넷 중 셋은 문면이 사실과 어긋났고, 넷째는 빠져
있어서 검수 한 갈래가 통째로 안 돌 참이었다.

| # | session-49 §7의 문면 | 실측 | 정정 |
|---|---|---|---|
| ① | `main=185a901` | 실제 `96c2f6e`. 그 위 docs 커밋 **다섯** | **SHA를 박지 않는다** |
| ② | "게이트 **넷**" | `CLAUDE.md`는 **다섯**을 한 묶음으로 요구 | 다섯으로 통일 |
| ③ | "태스크마다 전체 스위트" | 계획서가 도는 것은 `./internal/cli` **한 패키지** | 만나는 자리를 옮김 |
| ④ | cross-model 언급 없음 | 요청이 없으면 **한 번도 안 돈다** | 승인 범위를 명시 |

**① SHA는 코드 무변경으로도 낡는다.** `185a901` 위의 다섯(`9fd4c7f` 핸드오프 · `c189124`
§5.3 정정 · `f62b7d2`·`9ab9f86`·`96c2f6e` CLAUDE.md 정비)은 전부 docs다. v0.18 구현에
실질 영향은 없으나 프롬프트가 거짓 상태를 진술한다. §7 수정본은 SHA 대신 **성질**을
적는다("코드는 v0.17.2 그대로, 그 위는 전부 docs").

**② 실질 누락은 아니었다 — 명명이 어긋났다.** 계획서 Global Constraints는 테스트를
"CI 게이트 넷"과 **별도 항목**으로 세면서(27·29행) 둘 다 태스크 끝에 요구하므로, 각
태스크 verification에는 다섯이 다 있다. 그러나 §7만 읽는 서브에이전트는 넷만 돌린다.
`CLAUDE.md`는 "다섯을 계획서 자체의 검증 절차에 써 넣으라"고 한다.

**③ 경고의 전제가 계획서와 어긋났다.** 플레이크 다섯은 `cmd/context-router`·
`internal/exec`·`internal/hook`에 있는데 계획서는 그 셋을 **돌리지 않는다.** 태스크
중에는 만나지 않고 **릴리스 전 전체 스위트에서** 만난다. 판정 규칙 자체는 그대로 옳다.

**④ cross-model은 현재 요청이 요구할 때만 도는 export다.** §7이 요구하지 않으므로 그대로
쓰면 한 패스도 안 돈다. 릴리스 전 `/code-review`도 사용자가 직접 치는 슬래시 명령이다.
수정본은 **플랜 종료 한 패스만 승인**하고 태스크마다의 패스는 요구하지 않는다 —
`CLAUDE.md` 표가 태스크 체크포인트에도 적지만, 12태스크 × export는 이 작업의 위험에
비해 과하고, 그 표가 근거로 드는 사고(태스크 리뷰 11건이 전부 clean이었는데 shipping
blocker 다섯)는 **플랜 종료와 릴리스 패스가 잡는 자리**다.

## 4. 저장소 현재 상태

- **`main` HEAD**: `96c2f6e` — **이 레코드 커밋 직전 값이다.** 시작할 때 직접 읽어라.
- **`productVersion`**: `0.17.2` (`internal/buildinfo/buildinfo.go:11`) — session-49에서 무변경
- **태그**: `v0.17.0` · `v0.17.1` · `v0.17.2` 모두 존재·푸시됨
- **열린 PR**: 없음 (session-49 이후 코드 커밋 0건)
- **미푸시**: `f62b7d2` · `9ab9f86` · `96c2f6e` **3커밋** + 이 레코드 + `.gitignore` 변경
- **원격 브랜치**: `fix/v0.17.1-install-signal` · `fix/v0.17.2-bom-header` 남아 있음
- **리허설 워크트리**: `.claude/worktrees/agent-a00ec0cffe1f28287` (`afa47df`) 그대로 존재
- **다음 작업의 base**: `main` 최신

## 5. 이월 사항

### 5.1 ★ 최우선 — v0.18 구현

session-49 §5.1 그대로다. 계획서 12태스크를 Subagent-Driven으로 실행한다. 태스크 순서·
재기준선 다섯·"스펙이 이긴다"는 session-49와 계획서에서 읽는다. **§3.2의 넷만 이 레코드가
덮어쓴다.**

### 5.2 v0.19 대상

session-49 §5.2 전부 유효 — D93 · 설치 결합의 술어 · 라벨 어휘 · 세대 표식 · 철회된 D94의
재개 조건. 이번 세션이 더하거나 뺀 것은 없다.

### 5.3 Windows 플레이크

session-49 §5.3의 표(관측 이름 다섯)와 판정 규칙은 유효하다. **도달 시점만 §3.2 ③으로
정정된다** — 태스크 중이 아니라 릴리스 전 전체 스위트다.

### 5.4 푸시

미푸시 3커밋 + 이 레코드 + `.gitignore`를 다음 세션 시작 전에 올린다.

## 6. 상시 프로토콜 (이번 세션에서 측정된 것)

- **핸드오프 프롬프트에 `main` SHA를 박지 마라.** 코드가 한 바이트도 안 바뀌어도 docs
  커밋만으로 낡는다. 이번에 정확히 그 일이 났다(다섯 커밋). base는 성질로 적는다.
- **`-p 1`은 살아 있는 기계적 가드다.** 전역 훅 `test-go-parallel`이 `-p` 없는 `go test`를
  차단한다(`~/.agents/hooks/agents/policy.json`). `CLAUDE.md`에서 문장이 빠진 것은 훅이
  이미 강제하기 때문이지 규칙이 죽어서가 아니다 — 계획서의 요구는 옳다.
- **Claude Code의 LSP는 플러그인 컴포넌트다**(`lspServers`, `agents`·`themes`와 같은 층).
  공식 마켓플레이스에 언어별 12종이 있고 `gopls-lsp`가 그중 하나다. `claude plugin enable
  <p> --scope local`이 `.claude/settings.local.json`에 쓴다(실측). 컨텍스트 비용은 0 —
  out-of-process라 세션 토큰을 더하지 않는다.
- 리뷰 프로토콜·SDD 실행·서브 위생은 `CLAUDE.md`와 `docs/codex-secure-review.md` 참조.
  session-49 §6(워크플로 resume 동일 세션 한정 · Codex `review --base` 블록 · 검수 갈래가
  서로를 대신하지 않음 · 인용 없는 주장은 신호)은 그대로 유효하다.

## 7. 다음 세션 시작 프롬프트

```
docs/prompts/2026-08-05-session-50-handoff-audit.md 와
docs/prompts/2026-08-01-session-49-v0.17.1-v0.17.2-v0.18-design.md 를 읽고 이어서 작업한다.
v0.18의 실질 내용은 session-49에 있고 session-50 §3.2가 그중 넷을 정정한다 — 충돌하면
session-50이 이긴다.

이번 세션은 **v0.18 구현**이다. docs/superpowers/plans/2026-08-01-v0.18-d92-d95.md의
12태스크를 Subagent-Driven(태스크별 fresh 서브 + 태스크 리뷰 + fix 재리뷰)으로 실행한다.
계획을 축자로 따르되 계약과 어긋나면 스펙(docs/context-router-design-v0.18-ko.md)이 이긴다.

**base는 `main` 최신이다.** v0.17.2 이후 코드는 한 바이트도 바뀌지 않았고 그 위는 전부
docs 커밋이다 — SHA는 시작할 때 `git log -1`로 직접 읽어라. 레코드에 적힌 값은 그 레코드를
커밋한 시점의 것이다.

**매 태스크 끝의 게이트는 다섯이다. 하나도 빼지 마라.**

    go build ./...
    go vet ./internal/cli
    go test ./internal/cli -p 1 -count=1                 # 전체 스위트, 약 49초
    gofumpt -l .                                          # 무출력이어야 한다
    golangci-lint run --max-same-issues=0 --max-issues-per-linter=0 ./...   # 0 issues

계획서는 테스트를 "CI 게이트 넷"과 별도 항목으로 세지만(Global Constraints 27·29행) 둘 다
태스크 끝에 요구한다 — 합쳐서 다섯이다. session-48이 둘을 빠뜨려 릴리스 차단 둘을 맞았다.
`-p 1`은 선택이 아니다: 전역 훅 test-go-parallel이 `-p` 없는 `go test`를 기계적으로 막는다.

재기준선은 **다섯**이다(계획서 T2·T6·T8·T9에 분산). 그 밖의 판정이 하나라도 바뀌면 범위를
넘은 것이니 멈추고 보고하라.

**리뷰.** 태스크마다 서브에이전트 리뷰 + fix 재리뷰. 플랜 종료(브랜치의 마지막 리뷰, PR
전)에서 전브랜치 max-effort 서브 + **cross-model 한 패스를 돌린다 — 그 한 번의 브랜치 diff
export를 이 프롬프트가 승인한다.** 태스크마다의 cross-model은 요구하지 않는다; 필요해지면
그때 요청하겠다. 릴리스 태그 전 `/code-review`는 내가 직접 친다 — **태그 직전에 멈추고
알려라.**

**Windows 플레이크.** session-49 §5.3의 다섯은 회귀가 아니다(문서만 바꾼 커밋의 windows
런이 실패한 것이 증거). 다만 §5.3이 말한 "태스크마다 전체 스위트를 돌리므로 밟을 것이 거의
확실"은 틀렸다 — 계획서가 도는 것은 ./internal/cli 한 패키지이고 플레이크 다섯은
cmd/context-router · internal/exec · internal/hook에 있다. 태스크 중에는 만나지 않고
**릴리스 전 전체 스위트에서 만난다.** 판정 규칙은 그대로다 — 그 실패가 이번 태스크가 만진
패키지의 것인가. v0.18은 그 셋을 손대지 않는다.

리허설 워크트리(.claude/worktrees/agent-a00ec0cffe1f28287, afa47df)에 T1~T5 참고 구현이
있으나 검수가 잡은 결함들(오라클 no-op · trimBOM 되사상 · 호출부 미전환)을 그대로 담고
있다. 참고만 하고 개정된 계획으로 새로 구현한다. 지워도 무방하다.

시작 전에 미푸시 커밋(session-50 §5.4)을 올린다.

문제 발견되면 fix 혹은 브레인스토밍하여 사용자에게 질문하고, 문제 없으면 릴리스 직전까지
진행한다. ultrathink
```
