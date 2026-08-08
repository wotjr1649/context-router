# Context Router v0.19 설계서 (D96–D101 — 남의 파일에 쓰지 않는다 · 채택은 설득으로 얻지 않는다)

델타 체인: `context-router-design-v0.0.1-ko.md` → `-v0.1-ko.md`(D14–D20) →
… → `-v0.17-ko.md`(D89–D91) → `-v0.18-ko.md`(D92) → **이 문서(D96–D101)**.

**번호는 D96에서 시작한다.** D93(이스케이프를 담은 키·헤더 이름)은 v0.17 §4가 예약했으나
**이 설계가 그 대상을 없앤다**(§4). D94는 v0.18에서 철회, D95는 v0.18에서 보류. 재사용하지 않는다.

이 릴리스는 기능을 더하지 않는다. **비용의 뿌리를 옮기고, 도구가 실제로 쓰이게 한다.**

증거 등급: `[실측]` 이 머신에서 명령을 돌리거나 파일을 열어 확인 · `[문서]` 공식 문서 ·
`[참조실측]` 참조 구현이 자기 코퍼스로 측정해 기록한 값 · `[추정]` 계산으로 유도 ·
`[미실측]` 확정 못 함.

---

## 근거가 된 관측 (session-52, 2026-08-07)

두 갈래를 각각 물었다. **등록층** — 우리가 남의 설정 파일에 쓰는 것을 그만둘 수 있는가.
**가시성·채택층** — 그만두고 나면 도구가 실제로 쓰이는가.

### A. 등록층

#### A① 플러그인이 선언한 MCP 서버는 사용자 설정 파일에 기입되지 않는다 `[실측]`

`~/.claude.json`의 `mcpServers`가 비어 있는데 플러그인 제공 MCP 도구가 세션에 살아 있다.
Codex 쪽 `~/.codex/config.toml`의 `[mcp_servers.*]`에는 **우리가 손으로 넣은 `ctr`·`ctr-exec`만**
있는데, `codex mcp list`는 **claude-mem이 플러그인 매니페스트로 선언한 `mcp-search`까지 셋을 낸다.**
양쪽 다 매니페스트를 런타임에 읽는다. 호스트가 자기 설정에 남기는 것은 포인터와 상태뿐이다:

```
Claude : ~/.claude/settings.json  enabledPlugins["name@marketplace"] = true
Codex  : ~/.codex/config.toml     [plugins."name@marketplace"] enabled = true
```

#### A② 우리 산출물로 재도 같다 `[실측]`

스크래치패드에 프로브 플러그인을 짓고 양쪽에 올렸다.

- Claude: `claude plugin validate` exit 0 · `claude --plugin-dir`가
  `Skills 1 · Hooks 1(harness-only) · MCP 1 · 상시 ~54 tok`을 내고 도구가 실제로 뜬다.
- Codex: `codex plugin marketplace add <로컬 경로>` → `codex plugin add` → `codex mcp list`가
  공유 `.mcp.json`의 서버를 전부 `enabled`로 낸다.

**사용자 설정 파일에 한 바이트도 쓰이지 않았다.**

#### A③ 한 벌의 파일이 양쪽을 섬긴다 `[실측]`

| 파일 | 근거 |
|---|---|
| `.claude-plugin/marketplace.json` | `codex plugin list`가 **Codex가 읽은 경로로 이 파일을 인쇄**한다(설치된 다른 플러그인 하나가 그 형태다) |
| `.mcp.json` | Claude는 루트 관례로, Codex는 `.codex-plugin/plugin.json`의 `"mcpServers": "./.mcp.json"` 경로 문자열로 **같은 파일**을 읽는다 |
| `hooks/*.json` | 한 파일을 양쪽이 참조하는 실물이 설치돼 있다 |
| `skills/` | 양쪽이 같은 디렉터리를 읽는다 |

중복이 필요한 것은 **`plugin.json` 둘뿐**이고 그것도 거의 같은 파일이다.

#### A④ `command` 형태 중 **bare PATH 이름만** 양쪽에서 동일하게 산다 `[실측]`

서버 셋을 나란히 둔 차등 실험:

| `command` | Claude | Codex |
|---|---|---|
| `context-router` (PATH) | **마운트됨** | 등록·enabled |
| `${CLAUDE_PLUGIN_ROOT}/bin/…exe` | **마운트됨** | 등록되나 목록에 **리터럴 미치환** |
| `${PLUGIN_ROOT}/bin/…exe` | **마운트 실패 — 목록에 없음** | 등록되나 **리터럴 미치환** |

우리 현행 `.mcp.json`도 `hook_install.go`의 `hookBinaryName`도 **이미 bare 이름**이다.
그러므로 등록물의 *내용*은 바뀌지 않는다 — 바뀌는 것은 누가 어디에 쓰는가뿐이다.

#### A⑤ 참조 구현이 문서화한 Windows 파손은 구조적으로 우리 것이 아니다 `[실측]`

두 참조 구현 모두 `${CLAUDE_PLUGIN_ROOT}` 치환이 Windows에서 깨진다고 소스 주석에 적고
(`#369` bare `node`가 Git Bash PATH에서 안 잡힘 · `#372` MSYS 경로 훼손 · 백슬래시 따옴표 손상),
MCP 부팅마다 자기 매니페스트를 절대경로로 다시 쓰는 코드를 유지한다. 셋 다 **`node` +
스크립트 경로 인수**의 문제다. 우리는 자립 바이너리 하나이고 A④가 그것을 실증했다.

#### A⑥ 지금 우리 설치는 이미 양쪽에서 다르다 `[실측]`

Codex의 `ctr-exec`는 `--enable exec`, Claude의 `ctr-exec`는 `--enable ingest,net`이다.
**같은 이름이 두 호스트에서 다른 것을 뜻한다.** 한 `.mcp.json`을 공유하면 구성상 사라진다.

#### A⑦ 플러그인 설치는 사용자 `config.toml`을 훼손하지 않는다 — 순수 추가다 `[실측]`

임시 홈에 458바이트 씨앗 파일을 심고(머리 주석 · 넓은 공백과 작은따옴표와 꼬리 주석이 붙은
`model` 줄 · 미지 키 · 미지 테이블 · 헤더 **바로 위** 주석 · `args = []` · 인라인
`env = { CTR_MANAGED = '…' }` · EOF 주석) `codex plugin marketplace add` + `codex plugin add`를
돌린 전후 비교:

- 추가된 줄만 있고 **바뀌거나 사라진 줄이 하나도 없다**(`[marketplaces.ctrprobe]`와
  `[plugins."context-router@ctrprobe"] enabled = true` 두 블록이 EOF 주석 **앞에** 삽입된다).
- 인라인 `env`가 하위 테이블로 전개되지 않고, 작은따옴표도 `args = []`도 그대로다.

**즉 `codex plugin add`는 `codex mcp add`가 쓰는 직렬화기를 타지 않는다**(§6의 대조 참조).
플러그인 경로로 옮기는 것이 사용자의 기존 설정에 **부수 피해를 주지 않는다**는 뜻이다.
**단 이것은 `add` 경로 한정 측정이다** — 제거·업그레이드·재설치의 바이트 영향은 §5-10.

#### A⑧ ★ 같은 명령줄의 서버는 **조용히 버려진다** `[실측]`

한 변수만 바꾼 차등으로 잡았다. 플러그인이 선언한 서버를 `--enable ingest` → `--enable
ingest,net`으로 바꾸자 마운트가 사라졌고, `mcp list`에서 그 서버가 **목록에 아예 없었다**
(연결 실패가 아니라 미등록). 같은 목록에 `ctr-exec: context-router --enable ingest,net -
✔ Connected`가 있었다 — 프로젝트 `.mcp.json`의 옛 등록물이고 **명령줄이 완전히 같다.**
저장소 밖 작업 디렉터리에서 같은 플러그인을 올리자 정상 연결됐다.

> **Claude Code**에서 `command`+`args`가 이미 등록된 서버와 같은 플러그인 서버를
> **경고 없이 버린다** `[실측]` — Codex에서 같은 메커니즘인지는 재지 않았다 `[미실측]`.
> 옛 등록물은 어느 호스트든 같은 바이너리를 가리키는 중복이라 이 위험은 측정 여부와
> 무관하게 선다: **옛 등록물과 새 플러그인이 동시에 살아 있는 모든 마이그레이션 사용자가
> 여기 걸린다.** 증상은 "플러그인을 깔았는데 아무 일도 안 일어난다"이고 어디에도 사유가
> 안 나온다. §2가 그래서 제거를 0번 걸음으로 둔다.

#### A⑨ 게시 종단 검증 — 공개 원격에서 양쪽 설치 성공 `[실측]`

산출물을 공개 저장소에 게시하고 **원격에서** 설치했다.

| 호스트 | 결과 |
|---|---|
| Claude | `plugin marketplace add <owner>/<repo>` → `plugin install` → `plugin:context-router:ctr … ✔ Connected` |
| Codex | `plugin marketplace add <owner>/<repo>` → `plugin add` → `ctr … enabled`, 버전 `0.19.0` |

같은 저장소·같은 공유 서버 선언 파일·같은 스킬. 부수로 둘: **Codex는 두 마켓플레이스 파일이
함께 있으면 `.agents/plugins/marketplace.json`을 읽는다**(그래서 둘 다 싣는다) ·
**설치는 저장소를 통째로 클론한다**(§5-11).

### B. 가시성·채택층

#### B① MCP 도구 스키마는 프롬프트에 들어가지 않는다 — 지연된다 `[실측]`

같은 프롬프트를 세 조건으로 돌려 `cache_creation + cache_read` 합계를 비교했다.

| 조건 | 합계 | 대비 |
|---|---|---|
| A 플러그인 없음 | 45,273 | — |
| B 서버 셋(스키마 합계 ~31,000자) + 스킬 + 훅 | 45,672 | **+399** |
| C 같은 셋에 `alwaysLoad: true` | 48,907 | **+3,235**, 프롬프트 캐시 파괴(`cache_read` 25,929→0) |

`[문서]` Tool Search가 Claude Code v2.1+에서 기본값이고, MCP 도구 정의는 이름·서버 명령만
올라가고 스키마는 지연된다. B가 그것을 실증한다.

**그리고 상시 로드될 때조차 `outputSchema`는 프롬프트에 실리지 않는다** `[실측]`. 통제 실험:
도구 6개를 내는 최소 stdio 서버를 두 벌 짜서 **`outputSchema` 키 유무만** 다르게 했다
(차이 32,172자). `alwaysLoad: true`로 스키마를 강제 로드한 상태에서 총 프롬프트는
**38,273 대 38,273 — 완전히 같다.** 한 번도 쓰인 적 없는 정체성으로 콜드 스타트한 통제군에서도
차이는 **+12토큰**뿐이었고 그것은 도구 이름 접두가 길어진 몫이다.

> **프롬프트에 들어가는 것은 `name` + `description` + `inputSchema`뿐이다.**
> 도구 6개의 그 세 가지가 **+1,221토큰**을 만들었다(측정 밀도 2.52자/토큰).

#### B② 그런데 지연된 도구는 **쓰이지 않는다** `[참조실측]`

이것이 이 세션에서 가장 값비싼 사실이다. 참조 구현의 ADR-0006이 62일 코퍼스로 기록한다:

> `ctx_index` **had 0 calls in 62 days** — across *both* plugin generations …
> despite being irreplaceable … **It was never unwanted, only unmentioned.**

그리고 그 원인을 이렇게 적는다:

> Claude Code defers MCP tools **unconditionally** … **A tool whose entire purpose is to be
> reached for instead of `Read` cannot be reached for while it is invisible.**

**B①과 B②는 같은 사실의 양면이다.** 지연은 토큰을 아끼고 도구를 잃는다.

#### B③ 설득 채널은 측정으로 실패했다 `[참조실측]`

ADR-0008의 레버 순위표 6행:

> **Persuasion** — ERA-2 organic adoption was **3.82%** *with maximal prompting*
> (CLAUDE.md walls, per-call hook tips, skill docs). **Nudges do not move the needle;
> defaults and enforcement do.** 80% of `ctx_execute` volume arrived via the auto-injected
> subagent routing block — an *enforced* channel

ADR-0007이 같은 코퍼스를 더 잘게 쪼갠다: `intent` 파라미터는 3,041호출 중 **130회(4.3%)**,
**1,136회(37%)**가 `language:"shell"` — 즉 Bash를 감싼 더 나쁜 봉투로 쓰였다.

#### B④ 훈계 어투는 **역효과가 측정됐다** `[참조실측]`

ADR-0002가 38시행 A/B로 금지 토큰표를 세웠다 — `MANDATORY:`, `BLOCKED`,
`PREFER X OVER Y`, `Do NOT use/read/pull`, `Never use`, `SESSION STATE`, `✅/❌`.

> the single hortatory word `"blocked"` in a routing deny reason was misread by Opus 4.6 as a
> safety/network restriction, causing the agent to **capitulate to training data** instead of
> using the redirected tool.

`"blocked"` → `"redirected"` 교정이 그 모델에서 **6/6 → 0/6**으로 굴복률을 없앴다. 부정 해명
("NOT a network restriction")은 부정하려던 프레임을 오히려 점화해 **두 번에 걸쳐 삭제**됐다.
무차별 Bash 팁은 *"a recurring ~85 tokens that trains the agent to ignore the warning"*으로
판정돼 allowlist로 잘렸다.

#### B⑤ 무엇이 실제로 작동했나 `[참조실측]`

| 채널 | 성격 | 측정된 효과 |
|---|---|---|
| 서브에이전트 프롬프트에 라우팅 블록 append | **강제**(`modify`) | `ctx_execute` 사용량의 **80%** |
| PostToolUse 수동 색인 | **채택 불요** | 100 KB 재읽기 거부 시 **98.8% 회수**, 결정 68.6 ms |
| 진입 도구 2개만 `alwaysLoad` + 나머지를 그 설명 꼬리에 색인 | 가시성 | 보이지 않는 도구 문제의 유일한 서버측 해법 |
| 좁게 겨눈 hard deny (1 MiB 초과 전체 읽기, 동일 바이트 재읽기) | **강제**(`deny`) | 적대적 리뷰가 100 KB 범위를 기각시켜 1 MiB로 좁혔다 |
| SessionStart 라우팅 블록 주입 | 유도(상시) | 4,425자/세션 |
| 스킬·`CLAUDE.md`·도구 설명 | 유도만 | 3.82% |

`[문서]` 그리고 우리 쪽 제약도 같은 방향이다 — 스킬은 `allowed-tools`로 **사전 승인만** 하고
도구 선택을 강제하지 못한다. `PreToolUse` 훅은 exit 2 또는 `permissionDecision:"deny"`로
**차단은 가능하나 다른 도구로 치환은 못 한다**. `SessionStart`의 `additionalContext`는 주입
가능하되 **매 턴 토큰을 문다**(최대 10,000자).

---

## 0. 결정 이력 (v0.19 신규)

### D96 — 등록·가드·지침을 플러그인 매니페스트로 옮긴다

**결정**: 호스트 설정 파일에 등록물을 기입하는 일을 그만둔다. MCP 서버·훅·스킬을 저장소가
담은 매니페스트로 선언하고, 등록은 각 호스트의 자기 기제(`claude plugin` / `codex plugin`)가
한다. 호스트별 어댑터를 *실행하지* 않는다 — 정적 JSON 두 개다.

**근거**: A①~A⑤.

**계약 넷**:

1. **우리는 어떤 호스트 설정 파일에도 쓰지 않는다.**
2. **등록물의 실행 형태는 바뀌지 않는다** — `command`는 PATH의 `context-router`(A④가 유일한
   양쪽 공통 형태로 확정). 바이너리는 매니페스트에 담지 않는다(`[실측]` 22.6 MB × 플랫폼).
3. **파일은 최대한 공유한다** — A③의 넷을 한 벌로 둔다.
4. **`plugin.json` 둘만 중복한다.**

**사용자에게 보이는 변화**: 도구 이름공간이 `mcp__<서버>__ctr_*`에서
`mcp__plugin_<플러그인>_<서버>__ctr_*`로 이동한다. 권한 규칙과 `hostSnippet` 문면이 함께
움직인다. **깨는 변경이다** — §5가 버전 번호 판정을 남겨 둔다.

### D97 — 기입 경로를 은퇴시키고, 제거를 호스트 CLI에 위임한다

**결정**: 설정 파일을 **쓰는** 코드를 전부 삭제한다. 기존 손편집 등록물의 제거는 호스트
자신의 CLI(`codex mcp remove` / `claude mcp remove`)가 하고, 우리는 그것이 안 덮는 자리만 맡는다.

**근거**: 파일 파괴 결함 다섯이 전부 기입 경로에 있었다(v0.18 §3.5·§3.7·§3.8). 그리고
`codex mcp remove`는 **깨끗하다** `[실측]` — 블록과 딸린 빈 줄만 정확히 제거하고 주석·미지
키·미지 테이블·다른 서버 항목은 전부 그대로다.

**계약 셋**:

1. **쓰기 없음.** `installCodexConfigBlock`·`mergeMCPServers`·훅 그룹 병합과 그 아래 스캐너·
   좌표계·구간 수술을 삭제한다.
2. **읽기는 남는다.** `doctor`는 기존 손편집 등록물을 **감지해서 알린다**. 판정은 읽기이고
   조치는 사용자·호스트의 것이다. 문면은 D100의 어휘 규칙을 따른다.
3. **CLI가 안 덮는 자리만 우리가 맡는다** — `.claude/settings.json`과 Codex `hooks.json`의
   훅 그룹. 둘 다 JSON이고 D80의 파서 비의존 계약이 필요 없다.

**★ 배송 조건 셋** (소유자 판정, 2026-08-07 — 적대적 검토 둘을 받은 뒤). **전환은 그대로 가되
삭제의 순서를 강제한다.** 아래 셋이 닫히기 전에는 기입 경로를 지우지 않는다.

1. **훅 파일이 매니페스트에 실린다**(§5-8) — 안 실으면 지금 도는 수동 포착이 조용히 죽는다.
   **닫혔다** `[실측, 2026-08-08]`. 추적 파일만 담은 깨끗한 클론 — 원격에서 설치하는 사용자가
   받는 것과 같은 내용 — 에 `claude plugin install`을 걸면 `plugin details`가
   `Hooks (6) SessionStart, PreToolUse, PostToolUse, PostToolUseFailure, SubagentStart,
   SubagentStop (harness-only — no model context cost)`과 상시 비용 `~54 tok`을 보고하고
   `Duplicate hooks file detected`는 나오지 않는다 — D98의 규칙(Claude는 관례 경로
   `hooks/hooks.json`을 스스로 로드하므로 매니페스트가 그 경로를 또 선언하지 않는다)이 그대로
   확인된다. 같은 설치에서 런타임 `claude mcp list`가
   `plugin:context-router:ctr: context-router - ✔ Connected`를 낸다.
2. **install → uninstall → reinstall 왕복의 바이트 비교**(§5-10) — A⑦은 `add` 한 방향만 쟀다.
3. **`doctor`의 감지원이 정해진다**(§5-13) — `codex mcp get`은 무효 TOML에서 실패하고
   `--json`은 `env`를 평문으로 낸다.

**검증 걸음 하나가 거짓 음성을 낸다** `[실측, 2026-08-08]`. 같은 깨끗한 설치에서
`plugin details`는 `MCP servers (0)`을 보고하는데 런타임은 그 서버에 붙는다. 정적 인벤토리
명령이 **경로 문자열로 선언한 `mcpServers`를 해석하지 않고**(D98이 쓰는 형태다) 런타임만
해석한다. `plugin details`를 배송 검증 걸음으로 쓰면 이 자리에서 없는 결함을 본다 — 확인은
런타임 `mcp list`로 한다.

셋이 닫히면 `codex_toml.go`를 통째로 지운다. §8이 든 시간적 위험(표면이 움직인다)은 **폴백을
영구화하는 대신 순서로 흡수한다** — 결함 표면을 영구히 남기는 값이 그 위험보다 크다는 판정이다.

**알고 받는 대가**: `config.toml`이 **무효 TOML**이면 Codex 자신도 못 읽어
`codex mcp remove`가 실패한다. 그 사용자는 손으로 지워야 하고 `doctor`가 어느 줄인지 짚는다.
**의도된 교환이다** — 그 능력을 유지하는 값이 스캐너 전체이고, 그 스캐너가 유효 입력에서
사용자 파일을 다섯 번 파괴했다.

### D98 — 매니페스트의 자리와 이름

**결정**: 공유 가능한 것은 전부 한 벌로 두고, 루트 `.mcp.json`을 **쓴다**.

**근거 개정**: 초안은 이 저장소가 `.mcp.json`(`.gitignore:6`)과 `.codex/`(`:10`)를 제외하고
로컬 설치본이 그 자리를 점유한다는 이유로 루트 `.mcp.json`을 피했다. **D96·D97 아래에서는
그 로컬 설치본이 더는 생기지 않으므로 충돌이 소멸한다.** 그 `.gitignore` 항목은 함께 은퇴한다.

**배치**:

```
context-router/
  .claude-plugin/plugin.json       mcpServers→"./plugin/mcp.json" · skills→"./skills/"
  .claude-plugin/marketplace.json  Claude 정본 마켓플레이스 (source "./")
  .agents/plugins/marketplace.json Codex 정본 마켓플레이스 — 스키마가 다르다
                                   (source 객체 · policy · interface, owner 없음)
  .codex-plugin/plugin.json        mcpServers→"./plugin/mcp.json" · skills→"./skills/"
                                   · interface
  plugin/mcp.json                  ★ 공유 — 두 매니페스트가 경로 문자열로 가리킨다
  skills/<이름>/SKILL.md           ★ 공유
  hooks/hooks.json                 Claude — 관례 경로, 선언하지 않는다. 배송됨(ee03cd3)
  hooks/codex-hooks.json           Codex — .codex-plugin/plugin.json이 선언한다. 배송됨
```

**훅 파일은 한 벌이 아니라 두 벌이다** — 이 배치표의 초안이 적은 "★ 공유 한 벌"을 철회한다.
갈라지는 이유 둘: 러닝 서브커맨드가 호스트마다 다르고(`hook` 대 `codex-hook`), 이벤트 집합도
다르다(Codex 바이너리에 `PostToolUseFailure`가 없다 `[실측: 바이너리 문자열]`). 한 벌로 두면
한 호스트가 다른 호스트의 서브커맨드를 실행한다. §5-2가 이 정정을 자세히 적는다.

**루트 `.mcp.json`을 쓰지 않는다** `[실측]`. Claude의 `plugin.json`이 `mcpServers`를 **경로
문자열로도 받는다**는 것을 쟀다 — `./plugin/mcp.json`에서 서버가 실제로 떴다.
**`.gitignore`는 그대로 둔다** — 초안의 "그 항목을 은퇴시킨다"를 철회한다.

**★ 정정 `[실측, 2026-08-08]` — 루트 자리를 비워 둔다고 로컬 개발 설치본과의 충돌이 사라지지는
않는다.** 위 문단의 그 근거만 틀렸고 결론(경로 문자열을 쓴다)은 그대로다. 로컬 디렉터리 설치는
호스트마다 다르게 재료를 모은다: **Codex의 설치 캐시는 온전한 git 클론이라**(`.git`이 들어 있다)
커밋된 내용만 실린다. **Claude는 작업 트리를 그대로 복사하고 거기에는 gitignore된 파일도
포함된다** — 이 저장소의 gitignore된 루트 `.mcp.json`이 캐시에 실렸고, Claude가 선언된
`./plugin/mcp.json` 대신 **그것을** 읽어 로컬 개발 서버 이름을 보고했다. **루트 관례가 선언된
경로를 이긴다.** 원격에서 설치할 때는 그 파일이 gitignore돼 애초에 없으므로 이 경로는 원격
설치에 영향을 주지 않는다 — 영향받는 것은 로컬 디렉터리로 설치해 배송을 검증하려는 우리 쪽이다.

**마켓플레이스 매니페스트는 두 벌이다** `[실측 + 문서]`. 경로도 스키마도 다르고, Codex는 둘이
함께 있으면 `.agents/` 쪽을 읽는다. Codex가 Claude 형식도 받아들이기는 하지만 정본은 `.agents/`다.

**`hooks` 키는 호스트마다 다르게 다룬다** `[실측]`. Claude는 `hooks/hooks.json`을 관례로 **이미**
로드하므로 그 **관례 경로를** 매니페스트에 또 적으면 `Duplicate hooks file detected`가 나고
플러그인이 `hook-load-failed`로 표시된다. 관례 밖 경로를 선언하는 것은 문제없다. Codex에는
관례가 없어 `.codex-plugin/plugin.json`은 **반드시** 선언해야 한다.

**이름**: 플러그인 `context-router`, MCP 서버 `ctr`. 도구 접두는
**`mcp__plugin_context-router_ctr__`**가 되고 도구 이름은 `ctr_search`처럼 그대로다.

근거: 접두 길이는 비용이 거의 없다 — `[실측]` 도구 **이름**은 지연 로드 상태에서도 프롬프트에
들어가지만(B①의 +399), 콜드 통제군에서 접두를 6자 늘렸을 때 차이는 **+12토큰**이었다. 플러그인
이름을 `ctr`로 줄이면 8도구 기준 약 25~30토큰을 아끼는데, 그 대가로 마켓플레이스 목록에서
제품이 무엇인지 읽히지 않는다. **발견 가능성이 그 값보다 크다.** 저장소·바이너리·플러그인
이름을 하나로 맞추는 이득도 있다.

**깨지는 것**: 권한 규칙과 `hostSnippet` 문면이 옛 접두(`mcp__ctr-exec__`)를 참조한다 —
D96의 "사용자에게 보이는 변화"와 한 쌍으로 갱신한다.

### D99 — 가시성은 서버 단위가 아니라 도구 단위로 산다

**결정**: `.mcp.json`의 **서버 전체 `alwaysLoad`를 버리고**, 진입 도구 소수에만 도구 단위
상시 로드를 걸고, 나머지는 **그 진입 도구의 설명 꼬리에 이름과 한 줄 용도로 색인**한다.

**근거 둘.**

① **측정** — B①이 지연의 이득을(+399 대 +3,235), B②가 지연의 대가를(62일 0회) 잰다. 둘 다
참이므로 전부-상시도 전부-지연도 틀렸다. 참조 구현이 같은 자리에서 11개 중 2개만 상시로 두고
나머지 아홉을 설명 꼬리에 색인하는 형태로 수렴했고, 그 근거를 이렇게 적는다 — *"their
descriptions cost more context every session than the deferral costs in missed use."*

② **기능**(소유자 판정, 2026-08-07) — 서버 단위 `alwaysLoad`는 토큰만 무는 것이 아니라
**호스트의 지연 로드 기능 자체를 사용자에게서 빼앗는다.** 우리 서버를 붙였다는 이유만으로 Tool
Search가 우리 도구 전체에 대해 무력화된다. 이것은 비용이 아니라 **기능 회귀**이고, 토큰 수치와
무관하게 그것만으로 서버 단위 플래그를 버릴 이유가 된다.

**계약 넷**:

1. **진입 도구는 `ctr_search` + `ctr_index` 둘이다** (소유자 판정, 2026-08-07). 나오는 문과
   들어가는 문. 프롬프트에 실리는 몫이 1,159자 ≈ 390토큰이고, 현행 8도구 4,571자 ≈ 1,530토큰
   대비 **세션당 약 1,140토큰**을 던다. 지연되는 여섯: `ctr_fetch` · `ctr_transform` ·
   `ctr_record_event` · `ctr_session_summary` · `ctr_export_events` · `ctr_fetch_and_index`.
2. **나머지는 진입 도구 설명의 꼬리에 색인한다** — 이름 + 한 줄. 호출 방법은 적지 않는다.
3. **강등 규칙을 함께 정한다.** 참조 구현이 `post-install execCallBasis < 15% → demote to
   deferred`라는 수치 기준을 걸었다. 우리도 기준 없이 상시를 유지하지 않는다.
4. **`outputSchema`는 손대지 않는다 — 공짜다.** B①의 통제 실험이 확정했다. 도구 배열의 절반이
   거기 있지만 프롬프트 비용은 **0**이다. 줄일 대상은 `description`과 `inputSchema`뿐이다.
   구현 세션이 `outputSchema` 제거에 시간을 쓰지 않도록 명시적으로 적는다.

**측정 근거** `[실측]`:

| `--enable` | 도구 | 스키마 문자 수 | 그중 `outputSchema` |
|---|---|---|---|
| `ingest` | 7 | 10,175 | 5,214 (51.2%) |
| `net` | 7 | 9,926 | 5,235 (52.7%) |
| `ingest,net` (현행 기본) | 8 | 10,914 | 5,594 (51.3%) |
| `ingest,net,exec` | 10 | 13,002 | 6,470 (49.8%) |

**그중 프롬프트에 실리는 것은 `outputSchema`를 뺀 나머지다** — `ingest,net`에서 10,914자 중
약 5,140자(`name`+`description`+`inputSchema`). 상시 로드된 두 서버(비-`outputSchema` 9,652자)가
+3,235토큰이었으므로 밀도는 약 **2.98자/토큰**이고, 우리 한 서버 8도구의 상시 비용은
**대략 1,700토큰**이다 `[추정]`.

**프로필 선택은 약한 레버다** — 핵심 6도구 9,187자(최대의 70.7%)는 어느 `--enable`로도 안
빠진다. 기본값 대비 절감은 −9%뿐이다. **강한 레버는 진입 도구 축소(계약 1)와
`description`·`inputSchema`의 길이뿐이다.**

### D100 — 채택은 설득이 아니라 기제로 얻는다

**결정**: 스킬과 문면은 **발견 보조**로만 두고, 실제 채택은 (a) 가시성(D99) (b) 훅의 좁은
강제 (c) 모델 채택을 요구하지 않는 수동 포착으로 얻는다. 그리고 **우리가 쓰는 모든 사용자
대면 문면에 훈계 어휘 금지 규칙을 건다.**

**근거**: B③·B④·B⑤.

**계약 다섯**:

1. **스킬은 하나, 짧게.** 프론트매터 `description`만 상시 비용이다 `[실측: 우리 프로브 159자]`.
   본문에 규칙 벽을 쌓지 않는다 — 그 형태가 3.82%를 낸 형태다.
2. **훈계 어휘 금지.** `MANDATORY` · `BLOCKED` · `Do NOT` · `Never` · `PREFER X OVER Y` ·
   ✅/❌ 불릿을 도구 설명·훅 사유·`doctor` 문면에 쓰지 않는다. **부정으로 해명하지 않는다**
   (부정이 그 프레임을 점화한다 — B④). 리다이렉트는 HTTP 301처럼 **대체 목적지만** 말한다.
3. **강제는 좁게, 그리고 fail-open은 선택이 아니라 사실이다.** 겨눌 후보는 "이미 저장소에
   색인된 바이트를 다시 읽는 것"과 "임계값을 넘는 전체 읽기" 둘이다. 참조 구현의 적대적 리뷰가
   100 KB 범위를 기각하고 1 MiB로 좁힌 이력을 그대로 받는다 — 오탐이 편집 워크플로를 깨기
   때문이다.

   **거부의 기제는 실측으로 확정됐다** `[실측]`:
   - 거부가 성립하는 것은 **훅이 종료 코드 2로 끝날 때**(스트림 내용 무관), 또는 **종료 코드 0 +
     stdout의 `permissionDecision:"deny"` JSON**일 때뿐이다.
   - **종료 코드 1·3은 거부가 아니다.** 훅이 오류로 죽으면 도구 호출이 그대로 통과한다 —
     **`PreToolUse` 훅은 보안 게이트가 될 수 없다.** 우리 선택이 아니라 호스트의 성질이다.
   - **`command` 문자열 대신 `command` + `args` 배열 형태를 쓴다.** Windows에서 문자열 형태는
     **Git Bash가 실행하고**(측정: `$0 = /usr/bin/bash`, `$BASH_VERSION = 5.3.15`) MSYS 인자
     변환이 Windows 셸 문법을 **조용히** 망가뜨린다. 배열 형태는 셸을 거치지 않아 종료 코드가
     그대로 온다. 자립 바이너리인 우리에게 자연스러운 형태다.
   - **두 경로는 배타적이다** `[문서]`. `exit 0`이어야 stdout의 JSON이 파싱되고, `exit 2`는
     **stdout을 통째로 무시하고** stderr를 사유로 쓴다. 사유 문자열을 실으려면 **`exit 0` +
     JSON**을 쓴다 — "exit 2 + JSON"은 JSON이 버려진다.
   - `permissionDecision`의 값은 셋이 아니라 **넷**이다 — `allow` · `deny` · `ask` · `defer`
     `[문서]`.

   ★ **현행 코드가 문자열 형태다** `[실측: `buildHookCommand`가 문자열을 조립하고 `hookCmd`에
   `args` 필드가 없다]`. 이관하면서 배열 형태로 바꾼다.
4. **수동 포착은 후보가 아니라 이미 배송돼 있다 — 그리고 우리 방어가 참조 구현보다 두껍다.**
   `[실측: internal/hook/shadow.go · internal/ingest/ingest.go]` PostToolUse가 **전 도구**에 대해
   (등록 매처가 빈 문자열) 성공 응답을 포착하고, 필터 사슬은 이렇다:
   `CTR_SHADOW_OFF` → **16 KiB 하한** → **1 MiB 상한** → 파일 유래 denylist(`Read`·`NotebookRead`
   **한정**) → 정적 덤프 경로 denylist(`Bash`·`PowerShell`의 `cat`류 인수) → NUL·바이너리 leaf →
   **`ingest.Redact`(2뷰 스팬 리댁션, RE2)**.

   참조 구현은 *"큰 Bash 출력은 자격증명을 흔히 담는다"*는 이유로 Bash 포착을 **거부**했다.
   우리는 포착하되 **리댁션 단계를 통과시킨다** — 그들에게 없는 방어다. **그러므로 그들의 규칙을
   그대로 수입하지 않는다.**

   **남는 결정은 하나다**: 패턴 기반 리댁션이 Bash 출력에 충분한가, 아니면 도구 단위 배제를
   더할 것인가. 잔여 표면은 패턴 표가 놓치는 것이고, 그것은 설계 §5.1의 알려진 한계로 이미
   secret canary로 시험된다. **이 릴리스는 이 동작을 바꾸지 않는다.**
5. **상시 주입은 비용이 큰 채널이다.** 참조 구현의 SessionStart 주입은 4,425자/세션,
   서브에이전트 append는 4,379자/호출이다 `[참조실측]`. 채택의 80%가 뒤쪽에서 나왔다는 것이
   그 값을 정당화하지만, 우리 도구 수와 목적이 다르므로 **우리 숫자로 재기 전에는 도입하지
   않는다.**

### D101 — 구성값은 환경 변수로 수렴시킨다

**결정**: 프로필 목록과 저장소 루트를 **환경 변수에서 읽는다.** 매니페스트의 호스트별 구성
기제는 그 환경 변수를 채우는 수단일 뿐이고, 서버는 어느 호스트에서 왔는지 알 필요가 없다.

**근거 — 구성 계층은 양쪽이 갈라진다** `[실측]`:

| 축 | Claude Code | Codex 0.146.0 |
|---|---|---|
| 선언 | `.claude-plugin/plugin.json`의 최상위 `userConfig` | **대응 키 없음** — 바이너리 전체에서 `userConfig` 0건 |
| 설치 시 값 수집 | 대화형 다이얼로그 + `install --config k=v` | 없음 |
| 타입·범위 검증 | `type`·`required`·`min`·`max` (열거형·정규식은 **없다**) | 없음 |
| 비밀값 | `sensitive: true` → 키체인 | 없음 |
| 매니페스트 내 참조 | `${user_config.<KEY>}` (MCP `command`/`args`/`env`, exec형 훅, 스킬 본문) | 없음 |
| 저장 위치 | `~/.claude/settings.json`의 `pluginConfigs[id].options` | — |

**그리고 Codex는 미지 매니페스트 필드를 조용히 버린다** (`ignoring unknown Agent Plugins
manifest field`) — 에러가 아니라 침묵이라 더 나쁘다. `userConfig`를 `.codex-plugin/plugin.json`에
적어도 아무 일도 일어나지 않는다.

**계약 넷**:

1. **서버는 환경 변수만 읽는다.** 프로필 목록은 **쉼표 구분 문자열 하나**로 받는다.
   `[실측]` Claude가 `multiple: true` 배열을 `String(o)`로 문자열화해 쉼표 결합하므로, 이 표현이
   **양쪽에서 동일하게 도착하는 유일한 형태**다. 지금의 `--enable ingest,net`과 같은 모양이다.
2. **기본값은 서버가 갖는다.** 환경 변수가 없으면 현행 기본 프로필로 돈다 — Codex 사용자가
   아무것도 안 해도 동작해야 한다.
3. **Claude 쪽 `userConfig`는 선택적 상위 계층이다.** 다이얼로그·검증·키체인은 Claude에서만
   얻는 부가 혜택으로 두고, **그것에 의존하는 동작을 만들지 않는다.**
4. **검증은 서버가 한다.** 호스트 검증에 기대지 않는다 — 한쪽에 없기 때문이다.
   `[실측]` Claude의 `userConfig`에는 열거형이 없어 프로필 이름을 스키마로 강제할 수도 없다.

**릴리스 리뷰가 더한 셋째 갈래** `[실측]`: 계약 1·2는 "플래그/환경 변수로 이름을 받는다"와
"없으면 기본값"만 말해, 프로필 **0개**를 명시로 요청할 자리가 없었다 — 빈 문자열도 공백뿐인
값도 "안 줬다"로 읽혀 늘 기본값 `ingest,net`으로 떨어졌다. 이름 `none`(값 전체일 때만
유효)이 그 자리다: `CTR_ENABLE=none`은 값을 준 것이므로 계약 2의 폴백을 타지 않고 빈
프로필로 확정된다(v0.19 이전 자세 — search/fetch/transform만, 색인 쓰기도 아웃바운드 HTTP도
없음). 다른 이름과 섞으면(예: `none,ingest`) 오류다 — "0개"와 "그 이름만"이 둘 다 성립하는
입력을 조용히 어느 한쪽으로 해석하지 않는다. 우선순위는 무변경이다: `--enable none`이 값
있는 `CTR_ENABLE`을 이기고, `CTR_ENABLE=none`이 기본값을 이긴다.
`TestParseFlags_EnablePrecedence`·`TestParseFlags_EnableNone`이 잰다.

**알고 받는 대가**: Codex 사용자가 프로필을 바꾸려면 자기 환경에서 그 변수를 설정해야 한다.
그것은 설정 파일 편집이 아니고, 우리가 남의 파일에 쓰는 것도 아니다.

---

## 1. 층별 대조

| 층 | 지금 | 이 설계 뒤 |
|---|---|---|
| 도구 표면 | MCP `ctr_*`, 서버 전체 상시 로드 | MCP 유지. **진입 도구만 상시**, 나머지 지연 + 설명 색인 |
| 등록 | 우리가 `.mcp.json`·`config.toml`을 손편집 | **호스트가 쓴다.** 우리 자국은 각 호스트 설정의 한 줄 |
| 가드 | 우리가 `settings.json`·`hooks.json` 그룹을 병합 | 매니페스트가 선언, 호스트가 읽는다 |
| 지침·발견 | **없다** | 짧은 스킬 한 벌, 양쪽 공통 |
| 채택 | **기제 없음 — 모델의 선택에 전적으로 의존** | 가시성 + 좁은 강제 + (후보) 수동 포착 |

## 2. 설치 절차 — **옛 등록물 제거가 0번 걸음이다**

0. **옛 등록물을 먼저 지운다.** A⑧ 때문이다 — 안 지우면 플러그인 서버가 조용히 버려진다.
   `claude mcp remove <이름>` · `codex mcp remove <이름>`.
   **검증을 종료 코드로 하지 마라** — `codex mcp remove`는 없는 이름에도 exit 0을 낸다(§6).
   `mcp list`로 **부재를 눈으로 확인**한다.
1. 바이너리 확보 — 릴리스 산출물 또는 `go install`. **지금과 같다.**
2. `claude plugin marketplace add <owner>/<repo>` → `claude plugin install <name> --scope <user|project|local>`
3. `codex plugin marketplace add <owner>/<repo>` → `codex plugin add <name>`
4. **확인**: `mcp list`에 `plugin:<플러그인>:<서버>` 형태로 뜨는가. **`plugin details`로 하지
   마라** — 경로 문자열로 선언한 `mcpServers`를 그 명령이 해석하지 않아 `MCP servers (0)`을
   낸다(§0 `[실측]`).
5. **Codex에서 훅 신뢰를 준다.** 신뢰를 주지 않은 홈에서는 `SessionStart`를 포함해 훅이 하나도
   발화하지 않았다 `[실측]`. 이 걸음을 건너뛴 사용자는 수동 포착을 조용히 잃는다 — 근거와
   등급은 §5-8에 있다.

**우리도 사용자도 설정 파일을 편집하는 걸음이 없다** — §5-9가 Codex의 `hooks` 기능이 기본으로
켜져 있음을 실측했고, A⑦이 설치가 기존 설정을 훼손하지 않음을 실측했다. 훅 신뢰는 설정 편집이
아니라 Codex 자신이 묻는 승인이다.

프로필과 `--store-root`를 무엇으로 받을지는 §5-5에 남아 있다.

## 3. 코드 규모

아래는 **파일 전체의 줄 수이지 삭제량이 아니다** `[실측]`. 삭제량의 **상한**으로 읽어라.
각 파일 안의 읽기 경로는 D97 계약 2로 살아남고, 정확한 경계는 구현 세션이 호출부를 세어 정한다.

| 파일 | 생산 | 테스트 | 처분 |
|---|---|---|---|
| `codex_toml.go` | 1,989 | 2,929 + 833(오라클) + 137 | **전부** — 감지도 `codex mcp get`으로 갈음 가능한지 먼저 잰다 |
| `hook_install.go` | 1,111 | 1,625 | 기입·병합부. 훅 그룹 **제거** 한 방향은 남는다 |
| `mcp_install.go` | 569 | 1,567 | 기입·병합부. `mcpManagedMarker`류 감지원은 남는다 |

## 4. 이 설계가 없애는 v0.19 대상

v0.18 핸드오프 §5.2의 목록은 **거의 전부 기입 경로의 부산물**이다.

| 항목 | 왜 사라지나 |
|---|---|
| 배열이 헤더를 삼켜 uninstall이 `config.toml`을 비우는 형태 | 우리가 그 파일을 안 쓴다 |
| 격자 5120이 이 릴리스의 결함을 구조적으로 못 보는 문제 | 격자가 기입 경로의 오라클이다 |
| D95 `.mcp.json` 단일 슬롯 백업의 자리 | 우리가 그 파일을 쓰지 않는다 |
| `anomalyDottedEnv` 형태 열거 불완전 · `codexMarkerUnknown` 사유 | 판독기가 사라진다 |
| F7 괄호 깊이 · F8 후행 쉼표 · F13 이중 훑기 · F14 좌표계 둘 · F15 · `oneLineTok` | 전부 `codex_toml.go` 안 |
| 감시선 갭 여섯 | 감시선이 기입 경로의 오라클이다 |
| **D93** 이스케이프를 담은 키·헤더 이름 | 그 키를 읽을 이유가 사라진다 |

**살아남는 것**: `doctor`의 진단(읽기 전용으로 축소), 훅 그룹 제거 한 방향, Windows 플레이크
추적(무관).

## 5. 미확정 — 구현 세션이 **먼저 재야 할 것**

이 저장소는 측정 없는 주장을 신뢰 불가 신호로 다룬다. 아래를 재기 전에 그 부분의 구현을
시작하지 마라.

1. **훅이 실제로 발화하는가** — **발화는 양쪽 다 닫혔다. 거부 의미는 Claude만 닫혔다.**
   - **Claude**: 플러그인이 선언한 `PreToolUse` 훅이 이 Windows에서 발화하고(마커 파일이
     기록됨) exit 2가 거부로 존중된다(읽기가 실제로 차단됨).
   - **Codex**: 격리 홈에서는 인증이 없어 401이 세션을 끊는 바람에 직접 재지 못했다. 대신
     **이 머신의 실물이 증명한다**: 설치된 한 플러그인의 훅 스크립트가 `.ponytail-active`를
     ``process.env.PLUGIN_DATA``에 쓰고 그 코드가 ``const isCodex = … Boolean(process.env.
     PLUGIN_DATA); if (isCodex) stateDir = process.env.PLUGIN_DATA;``다. 그 파일이
     `~/.codex/plugins/data/<plugin>-<marketplace>/`에 실재하므로 **`PLUGIN_DATA`가 설정된
     프로세스 — 즉 Codex가 띄운 플러그인 훅 — 만이 그것을 쓸 수 있었다.** 사용자 설정을 한
     바이트도 건드리지 않고 얻은 증명이다. 보강 증거로 `config.toml`에 플러그인 범위 훅 신뢰
     항목 여덟이 있다. **등급은 인과 논증이지 직접 측정이 아니다** — 그렇게 읽어라.
   - **★ 남은 것 — Codex의 거부 의미는 미확정.** 위 증명이 말하는 것은 훅이 **발화한다**는
     것뿐이고, D100 계약 3이 의존하는 **거부 규칙**(exit 2 / `permissionDecision` JSON)은
     **Claude에서만** 쟀다. Codex가 같은 규약인지는 재지 않았다.
   - **배열 형태는 Claude에서만 쟀다.** `command` + `args` 배열로 exit 2·JSON 두 경로 모두
     거부가 성립했다. **`commandWindows`와의 공존**과 **Codex에서의 배열 형태 발화**는 미확정.
   - **★ 배송한 파일로 다시 쟀다 — 결론은 호스트별로 갈린다**(2026-08-08, ee03cd3 이후).
     - **Claude, 배열 형태** `[실측]`: `hooks/hooks.json`이 배송하는 `command` + `args` 형태로
       `SessionStart`·`PreToolUse`·`PostToolUse`가 이 Windows에서 발화한다. 이 저장소를
       `--plugin-dir`로 올린 세션이 `cc:` 접두 세션과 `Glob`·`Read`의 `tool_call` 이벤트
       8건을 남겼고 drops는 0이었다. `commandWindows` 없이 성립한다.
     - **★ Codex, 우리 훅 파일의 발화 — 닫혔다** `[실측, 2026-08-08, 태그 v0.19.0 이후]`.
       배송한 **문자열 `command`** 형태가 Codex에서 실제로 돈다. 이 등급을 올린 것은 다음
       측정이다: `codex plugin marketplace add wotjr1649/context-router@v0.19.0` +
       `codex plugin add context-router@context-router`로 설치한 뒤, 저장소를 CWD로 연 새
       Codex 세션에서 **17,000B**를 stdout으로 내는 읽기 전용 명령 하나를 돌렸다. `doctor [15]`의
       Codex 귀속 바이트가 **254,148B → 271,150B(+17,002B)**, 해시가 **921 → 922(+1)**로 움직였다.
       17,000B 출력에 17,002B 귀속 — 이 대응이 이 측정을 강하게 만든다. 2바이트 차이가
       `PostToolUse`의 JSON 봉투라는 것은 `[추정]`이다(봉투 바이트를 따로 세지 않았다).
       **유발 하나가 전부는 아니다** — 설치 전에 잰 235,218B에서 유발 직전 254,148B까지
       이미 +18,930B·해시 +1이 쌓여 있었다. 다만 그 구간에는 Codex 세션이 **둘**(설치를
       돌린 세션과 측정 세션) 들어 있어, 그 증가가 어느 쪽 것인지는 이 측정으로 갈리지
       않는다 `[미실측]`. 유발 명령 하나에 귀속되는 것은 +17,002B다.
       **`cx:`가 이 측정의 축인 이유** — 그 값은 Codex에 귀속된 바이트라 Claude 쪽 활동이
       움직이지 못한다. 같은 구간에서 `cc:`는 59,056,544B로 고정이었다. 임시 store로 환경
       변수를 밀어 넣는 방식은 그 변수가 훅 자식 프로세스까지 전파되는지가 미실측이라
       거짓 음성을 만들 수 있어 쓰지 않았다.
       **`[15]`는 worktree 범위다** `[실측]` — 저장소 밖 디렉터리에서 `doctor`를 돌리면
       `0B hashes=0`이 나오고, 그 값은 "훅이 발화하지 않았다"와 구분되지 않는다. 이 측정을
       재현하려면 CWD가 저장소여야 한다(`doctor [2]`의 `ProjectID`로 확인한다).
       D100 계약 3이 배열을 명하는 근거인 MSYS 인자 변환 함정은 슬래시·백슬래시가 붙은
       인자에서 생기는데 이 명령은 맨 토큰 둘이라 변환될 것이 없다 — 그 추론이 측정과 같은
       방향이었음이 이제 확인됐다.
   - **부수 확정 — Codex의 플러그인 환경 변수 둘**: `PLUGIN_ROOT`(경로 치환)와
     **`PLUGIN_DATA`(플러그인 전용 쓰기 가능 디렉터리)**. 우리 훅이 상태를 둘 자리가 여기다.
   - *그 미해결 관측도 닫혔다* `[실측]`: Windows에서 훅 `command` **문자열**은 Git Bash가
     실행하고, MSYS 인자 변환이 `cmd /c`의 `/c`를 경로로 바꿔 버려 `exit 2`가 **아예 실행되지
     않는다**(cmd가 대화형으로 뜬다). `… & exit 2` 형태에서는 `exit 2`가 **bash 자신의 내장**으로
     실행돼 훅이 2로 끝난다. 차이를 만든 것은 마커도 부수 효과도 아니고 그것뿐이었다.
     기제는 D100 계약 3에 있다. (내 원래 측정에서 마커가 의도한 경로에 남은 것은 리다이렉트
     대상에 **슬래시 경로**를 써서 bash의 백슬래시 이스케이프를 피했기 때문이다.)
2. **훅 파일 하나로 양쪽을 섬길 수 있는가** — **닫혔다. 구조적으로는 되지만 우리는 두 벌로
   배송한다.** 원래 관측 `[실측]`은 그대로다: 설치된 한 플러그인의 `.claude-plugin/plugin.json`과
   `.codex-plugin/plugin.json`이 **같은 파일**(`./hooks/claude-codex-hooks.json`)을 가리키고,
   진짜 `config.toml`에 그 파일의 항목별 신뢰 해시가 있으며(Codex가 소비했다는 뜻), 같은
   플러그인이 Claude 세션에서도 돈다. **조건 둘**: `SessionStart`에도 `matcher`를 달 것,
   Windows용 **`commandWindows`**를 함께 담을 것.
   **★ 우리 내용은 공유될 수 없다**(2026-08-08, ee03cd3의 판정). 한 벌이 성립하려면 두 호스트가
   같은 명령과 같은 이벤트 집합을 받아야 하는데 우리는 둘 다 다르다 — 러닝 서브커맨드가
   `hook` 대 `codex-hook`이고, Codex 바이너리에는 `PostToolUseFailure`가 아예 없다
   `[실측: 바이너리 문자열]`. 그래서 `hooks/hooks.json`(Claude, 6이벤트, 배열 형태)과
   `hooks/codex-hooks.json`(Codex, 3이벤트, 문자열 형태 + `commandWindows`) 두 벌이다. 이
   머신에 설치된 Codex 플러그인 둘이 정확히 같은 두 파일 구조다 `[실측]`. D98의 배치표에서
   `hooks/`의 ★ 공유 표시를 철회한 것이 이 항목이다.
3. **`outputSchema`가 모델 컨텍스트에 실리는가** — **닫혔다. 실리지 않는다**(B①). D99 계약 4가
   그 결론이다.
4. **진입 도구 집합** — **닫혔다.** 소유자 판정으로 `ctr_search` + `ctr_index`(D99 계약 1).
   표현 수단 `[실측: go doc]`: 설치된 SDK의 `mcp.Tool`이 ``Meta `json:"_meta,omitempty"` ``를
   담는다. 참조 구현이 쓰는 키는 `"anthropic/alwaysLoad"`다.
   숫자 `[계산: D99 측정표의 도구별 내역 × B①의 밀도]` — **프롬프트에 실리는 것만** 센다:

   | 상시 집합 | `name`+`description`+`inputSchema` | 추정 토큰 |
   |---|---|---|
   | 후보 둘 (`ctr_search` + `ctr_index`) | 1,159자 | 약 390 |
   | 현행 전체 8도구 (`ingest,net`) | 4,571자 | 약 1,530 |

   **차이는 세션당 약 1,140토큰이고, 그와 별개로 지연 로드 기능이 사용자에게 돌아온다**
   (D99 근거 ②). 강등 규칙(계약 3)의 임계는 배송 후 실제 사용률로 정한다.
   **★ 단 이 표현 수단은 Claude 한정이다.** `_meta`의 `"anthropic/alwaysLoad"`는 Anthropic
   벤더 확장이고 `[문서]`, Codex가 그것을 어떻게 다루는지는 **재지 않았다** — D101이 실측한
   대로 Codex는 미지 필드를 조용히 버린다. **D99는 Claude 한정 최적화로 읽어라.** Codex 쪽
   등가 기제가 있는지는 별도로 재야 한다.
   **★ 마지막 미결이 닫혔다**(2026-08-08). 옛 문면은 *"게시된 `plugin/mcp.json`에는 아직 진입
   도구 표시가 없다"*였다 — 그 파일에는 표시가 들어갈 자리가 없는 것이 D99의 결론이다. 표시는
   서버가 `tools/list`에서 도구별 `_meta`로 내고(`TestAlwaysLoadMetaExactlyEntryTools`가 정확히
   `{ctr_search, ctr_index}`임을 잰다), 매니페스트는 그 서버를 띄우기만 한다. 깨끗한 클론
   설치에서 그 서버가 실제로 붙는 것도 §0에서 실측했다.
   **꼬리 색인은 등록 집합을 따라 움직인다**(최종 리뷰 S4, 2026-08-08 `[실측: stdio]`). 색인이
   부르는 여섯 중 다섯이 조건부 등록이라 고정 문면은 프로필을 좁힌 서버에서 없는 도구를
   광고했다 — `CTR_ENABLE=ingest`가 `ctr_fetch_and_index`를 이름으로 남기면서 등록하지 않았다.
   `NewServer`가 등록한 것만 모아 색인을 만들고, 진입 도구 둘의 등록을 그 뒤로 미룬다 —
   fail-closed 관문(`transform.ProbeIsolation`·`sandbox.Probe`)은 자리를 지킨다. 최대 프로필의
   `tools/list` 바이트는 15,795로 그대로다(게이트 11 무변).
5. **`userConfig` 스키마** — **닫혔다** `[실측: 설치된 바이너리의 zod 정의 + `plugin validate
   --strict` 탐침]`. 스키마 전문과 양쪽 대조는 **D101**에 있다. 요점 셋: Claude에만 있고 ·
   열거형이 없어 프로필 이름을 스키마로 강제할 수 없으며 · Codex는 미지 필드를 조용히 버린다.
   부수 정정: 이 머신의 공식 `plugin-dev` 스킬 문서는 `userConfig`를 **전혀 언급하지 않는다** —
   그 문서가 바이너리보다 낡았다.
6. **마켓플레이스 배포** — **닫혔다** `[실측]`. 양쪽이 `owner/repo` 원격 git을 1급 소스로 받고
   (`codex plugin marketplace add`의 도움말: *"a local path, owner/repo[@ref], HTTPS Git URL,
   or SSH Git URL"*), 양쪽이 `.claude-plugin/marketplace.json`을 읽으며(Codex가 설치된 플러그인
   하나에 대해 그 경로를 그대로 인쇄한다), `"source": "./"` 자기 호스팅 형태가 양쪽에서
   수용된다(내 프로브가 Codex에서, 설치된 플러그인이 Claude에서). `claude plugin validate`도 통과.
   **그리고 게시 연기 시험도 닫혔다 — A⑨.** 공개 원격에서 양쪽 다 설치·연결까지 성공했다.
   정정 하나: 마켓플레이스 매니페스트는 **한 벌이 아니라 두 벌**이다(D98). Codex는 둘이 함께
   있으면 `.agents/` 쪽을 읽는다. 부수: `--sparse`가 양쪽에 있고 §5-11의 완화 후보다.
7. **버전 번호** — **판정됐다**(소유자, 2026-08-07): **이 작업은 `0.19.0`으로 배송한다.**
   `1.0.0`은 (a) 저장소를 실제로 게시해 양쪽에서 설치되는 것을 확인하고, (b) 그 뒤 내부 도구
   개선·수정을 한 차례 거친 다음에 판정한다 — **정식 릴리스는 대기다.** 델타 체인 파일명과
   문서 규약은 그대로 이어진다.
8. **수동 포착** — **닫혔다. 훅이 실린다**(2026-08-08, ee03cd3). 원래 관측은 이랬다:
   *"가만두면 사라진다"* — 포착을 실어 나르는 것이 설치기가 등록하는 `PostToolUse` 훅인데
   D97이 설치기를 지우고 게시된 패키지에는 `hooks/`가 없었다. 그래서 훅 파일을 매니페스트에
   싣는 것이 D97의 릴리스 조건이 됐고(§0 조건 1), 그 조건이 §0에 적은 실측으로 닫혔다 —
   깨끗한 클론 설치의 `plugin details`가 여섯 이벤트를 `Hooks (6)`으로 보고한다.
   *이 항목은 교차 모델 검토의 검증 서브가 잡았다 — 앞선 판정 "이 릴리스는 그 동작을 바꾸지
   않는다"는 틀렸다.* 남는 물음("패턴 리댁션이 Bash 출력에 충분한가")은 설계 §5.1의 알려진
   한계다.
   **★ 같은 실패 형태가 Codex 쪽에 다른 문으로 남아 있다 — 릴리스 관련 위험이다.**
   Codex의 훅은 **지속되는 신뢰(persisted hook trust)** 를 요구한다: `codex --help`가
   `--dangerously-bypass-hook-trust`를 *"Run enabled hooks without requiring persisted hook
   trust for this invocation"* 으로 적는다 `[문서]`. 진짜 Codex 홈에서 돌린 프로브는 빈
   store로 끝났다 — **훅이 하나도 발화하지 않았고 `SessionStart`조차 돌지 않았다** `[실측]`.
   그 프로브는 `config.toml`에 아무 흔적도 남기지 않았다(마켓플레이스·플러그인·`hooks.state`
   항목 전부 없음, 기존 신뢰 항목 열다섯은 그대로) `[실측]`.
   **★ 이 위험은 닫혔다 — 신뢰를 주는 것은 설치다** `[실측, 2026-08-08, 태그 v0.19.0 이후]`.
   `codex plugin add context-router@context-router`가 `config.toml`에 우리 훅 파일의 항목별
   신뢰를 **함께 기록한다**: 설치 직전 `[hooks.state.*]` 열다섯(우리 것 0), 설치 직후 열여덟이고
   그 셋이 `context-router@context-router:hooks/codex-hooks.json:{session_start,pre_tool_use,
   post_tool_use}:0:0`이며 각각 `trusted_hash`가 채워져 있다(값 길이가 이미 돌고 있는 다른
   플러그인의 같은 항목과 같다). 그 뒤 새 세션에서 훅이 발화했다(§5-1의 측정).
   **앞의 부정 결과와 어긋나지 않는다** — 그 프로브는 마켓플레이스·플러그인 등록 없이 훅 파일만
   놓았고, `hooks.state` 항목이 아예 생기지 않아 신뢰가 없었다. 신뢰는 요구되고, 그것을 주는
   경로가 `plugin add`다. 두 측정이 함께 말하는 것은 **설치 경로를 지나면 사용자가 따로 칠 것이
   없다**는 것이다.
   **정확히 무엇을 쟀는지** — `config.toml`을 설치 전후로 읽어 항목이 생긴 것과 그 뒤 훅이
   발화한 것을 쟀다. Codex가 TUI 세션에서 사람에게 승인 창을 띄우는지는 이 측정의 대상이
   아니었다(설치를 돌린 세션도 측정 세션도 승인 창을 보고하지 않았다).
   **CHANGELOG 0.19.0의 "Codex에서는 훅 신뢰를 준 뒤부터다"는 이 측정보다 좁게 읽힌다** —
   플러그인 경로 사용자에게는 따로 할 일이 없다. 그 절은 태그된 릴리스 노트라 여기서
   고치지 않는다. 다음 릴리스의 항목이 이 문장을 정정할 자리다.
   **부수 실측 — 커밋되지 않은 파일은 설치본에 실리지 않는다** `[실측, 2026-08-08]`. 설치는
   커밋된 내용에서 재료를 모으므로(Codex는 git 클론, 원격 설치는 원격의 커밋), 매니페스트
   파일이 커밋돼 있지 않으면 설치된 플러그인에 그 파일이 없다. ee03cd3 이전에 Codex 훅 프로브를
   돌렸다면 그 이유만으로 거짓 음성이 나왔을 것이다 — 소유자가 프로브 전에 이것을 짚었다.
9. **`[features] hooks`** — **닫혔다** `[실측]`. `config.toml`이 **아예 없는** 새 `CODEX_HOME`에서
   `codex features list`가 `hooks` · `plugins` · `plugin_sharing` · `skill_search`를 전부
   `stable / true`로 낸다. `plugin_hooks`는 `removed / false`다 — 참조 구현 README가 요구하는
   그 키는 낡았다. **사용자가 설정 파일에 한 줄도 칠 필요가 없다.**
10. **설치의 왕복 바이트 영향** — **닫혔다.** `[실측]` 호스트별로 결과가 갈린다.
    - **Codex**: 왕복 전체(`plugin marketplace add` → `plugin add` → `plugin remove` →
      `plugin add` → `plugin remove` → `plugin marketplace remove`)가 `config.toml`을
      시작 상태와 **바이트 그대로** 되돌린다(SHA-256 앞자리 `C02D634F`, 277바이트, 무변경).
      단계마다 순수 추가이거나 순수 제거다 — `marketplace add`는 4줄 `[marketplaces.<이름>]`
      블록을, `plugin add`는 3줄 `[plugins."<이름>@<마켓플레이스>"] enabled = true` 블록을
      넣고, 각 제거는 자기가 넣은 것만 그대로 되가져간다. 머리 주석·넓은 공백과 작은따옴표와
      꼬리 주석이 붙은 `model` 줄·미지 키·미지 테이블·헤더 바로 위 주석·`args = []`·인라인
      `env = { … }`·EOF 주석을 담은 씨앗 파일이 전 과정을 무변경으로 통과했다.
    - **Claude**: 사용자 데이터는 살아남지만 `settings.json`이 **통째로 재직렬화**된다 —
      4칸 들여쓰기가 2칸이 되고, 인라인 객체·배열이 여러 줄로 펼쳐지며, 키 순서가 바뀐다.
      `plugin uninstall`은 빈 `"enabledPlugins": {}`를 남겨 왕복이 시작 바이트로 돌아오지
      않는다. 사용자 키·값은 전부 보존된다.
    - **Claude, `settings.json`에 주석이 있으면**: `plugin marketplace add`가
      `Invalid JSON syntax in settings file`로 exit 1이 되고 **파일은 손대지 않는다.**
      Claude의 설정 파일은 JSONC가 아니라 엄격 JSON이다. Fail-closed.

    A⑦이 잰 것은 `add` 한 방향뿐이었다 — 이제 전 왕복이 닫혀 D97의 배송 조건(§0) 중 하나가
    채워졌다.
11. **긴 경로 클론 실패** `[실측]`. 플러그인 설치는 **저장소를 통째로 클론한다.** 설정 홈 경로가
    길면 Windows 파일명 길이 한계로 `fatal: failed to unlink … Filename too long`이 나고 설치가
    죽는다(짧은 경로에서는 성공). **이 코호트는 무효 TOML 코호트보다 크다.** 완화 후보는
    `--sparse`(양쪽 CLI에 있다)이고, 재지 않았다.
12. **옛 접두 권한 규칙** — **`hostSnippet`은 닫혔다. 권한 규칙 문자열 잔존 보고는 만들지
    않는다(범위 판단).**
    - **`hostSnippet`** `[실측]`: 새 접두는 `mcp__plugin_context-router_ctr__`다(D98).
      **이 등급의 근거는 호스트 관측이다**(2026-08-08 정정) — 플러그인이 제공한 MCP 서버가
      실제 세션의 도구 이름 공간에서 `mcp__plugin_<플러그인>_<서버>__` 형태로 잡히는 것을
      최종 검토가 자기 세션에서 확인했다. 옛 문면은 `TestHostSnippetUsesCurrentServerPrefix`를
      근거로 들었는데 그것은 **우리 상수를 우리 상수로 재는 단위 테스트**라 호스트 동작의
      증거가 아니다. 그 단정의 자리는 따로 있다: `TestToolPrefixMatchesPluginManifests`가
      `.claude-plugin/plugin.json`의 `name`과 `plugin/mcp.json`의 서버 키에서 접두를 유도해
      `ctrToolPrefix`와 대조하므로, 위 형태를 전제로 두 이름 중 하나가 바뀌면 테스트가 깨진다
      (최종 리뷰 S6). 사용자가 자기 `settings.json`에 붙여넣은 `mcp__ctr-exec__*` 권한 규칙은
      더 이상 매치하지 않는데, D96 계약 1이 우리가 그 파일을 고치는 것을 금하므로 그 규칙은
      사용자가 직접 옮겨야 한다.
    - **읽기 전용으로 실재하는 것은 등록물 잔존 보고([20])다** `[실측]` — 옛 서버 이름
      (`ctr-exec`·`ctr`)의 `.mcp.json`·`enabledMcpjsonServers` 잔존을 파일과 다음 걸음으로
      알린다. **범위 판단 하나가 여기 붙는다**(최종 리뷰 S11, 2026-08-08): `[20]`은 프로젝트
      `.mcp.json`과 **사용자 스코프 `~/.claude.json` 최상위**를 보고, 같은 파일의 **local 스코프
      (`projects.<경로>.mcpServers`)는 보지 않는다.** local이 `claude mcp add`의 기본 스코프라
      코호트는 더 큰데, 호스트가 정규화해 둔 경로 키를 우리 `projectRoot`와 맞추는 일
      (구분자·대소문자·Windows 심볼릭 링크)이 이 한 줄 진단보다 크다는 판단이다. 스코프 표는
      설계 v0.12에 있다 — 그 표를 다시 유도하는 세션이 이 제외를 모르고 결함으로 읽지 않도록
      여기 적는다.
    - **★ 남은 것 — 사용자의 `permissions.ask`/`allow` 배열에 남은 옛 접두 규칙 문자열 자체는
      doctor가 짚지 않는다.** `[실측: internal/cli 전체를 뒤져도 그런 스캐너가 없다]`. 이
      범위는 넓히지 않는다 — D96 계약 1이 그 파일을 고치는 것을 금해 스캐너를 만들어도 할 수
      있는 일은 등록물 잔존 보고와 같은 읽기 전용 한 줄뿐이고, 같은 사용자는 이미 그 보고로
      제거·재설치 경로에 서며 그 경로의 `hostSnippet`이 올바른 접두를 보여 준다. 문자열
      스캐너가 여전히 값이 있다고 보면 그 판단은 별도 세션의 몫이다.
13. **`doctor`의 감지원** — **닫혔다.** `[실측]` 결정: `codex mcp get`도 어떤 `--json` 형태도
    쓰지 않는다. 이유 둘 — `codex mcp get`은 무효 `config.toml`에서 실패하는데 그것이 정확히
    진단이 필요한 코호트이고, `--json`은 `env` 값을 평문으로 내 우리 출력에 자격증명을 싣게
    된다. 대신 순수 줄 스캐너(`internal/cli/codex_scan.go`)가 TOML 파서 없이
    `[mcp_servers.<이름>]` 헤더와 1-기반 줄 번호를 찾는다 — 파서라면 거부할 파일에서도
    동작하는 것이 존재 이유다. 문서화된 한계 셋: 여러 줄 문자열 안에서 헤더 모양인 줄은
    오탐한다 · 안 닫힌 따옴표는 최선-노력으로 지저분한 이름을 낸다 · 닫는 대괄호 뒤의
    비-주석 내용은 매치하지 않는다. 대가는 의도된 선택이다 — 오탐의 비용은 쓸모없는 줄
    번호 하나이고, 반대로 그냥 넘기면 사용자가 자기 파일이 깨끗하다고 잘못 믿는다.

    **릴리스 리뷰가 보고 지점을 좁혔다** `[실측]`. 스캐너 자신(`codexServerHeaders`)은
    여전히 이름을 가리지 않지만 — 그것이 파서 없이 동작하는 존재 이유다 — 보고 지점
    (`ownedCodexHits`)이 첫 점 앞 세그먼트가 **우리가 등록에 쓴 적 있는 이름**
    (`retiredServerNames` = 현재 이름 `ctr-exec` + D63 ②가 대체한 옛 이름 `ctr`)인 헤더만
    남긴다. 걸러지지 않은 이전 버전은 `[mcp_servers.github]`처럼 남의 서버도 "우리 잔존물"로
    보고했다. 이제 그 이름도 함께 인쇄한다 — `codex mcp remove <이름>`에 그대로 넘길 수 있는
    값이고 추정이 아니라 `retiredServerNames`의 리터럴이다. 같은 서버의 헤더가 여럿(`[mcp_servers.ctr]` + `[mcp_servers.ctr.env]`)이면
    줄만 이어 붙여 한 서버 한 줄로 접는다.

    **재검토가 그 거름의 거짓 음성 셋을 쟀고 둘이 닫혔다** `[실측]`(2026-08-08, 이 머신 —
    헤더 철자 열다섯을 옛·새 바이너리에서 대조). 첫 점 앞 세그먼트를 **다듬지 않고** 비교하던
    동안 `[mcp_servers.ctr . env]`(점 주위 공백 → 세그먼트 `"ctr "`)와
    `[mcp_servers."ctr".env]`(세그먼트 단위 인용 → `"ctr"`)가 보고에서 빠졌다. 둘 다 유효
    TOML이라 파스 실패 줄조차 뜨지 않아 그 사용자는 doctor 한 번에서 아무 신호도 받지
    못했다 — 손으로 고친 코호트가 정확히 이 스캐너의 대상이므로 마이그레이션 진단의 거짓
    음성은 그것이 대체한 거짓 양성보다 나쁘다. 이제 자른 세그먼트를 `strings.TrimSpace` +
    `unquoteHeaderName`으로 다듬고 비교한다.

    **받아들이는 대가는 양방향 하나씩이고 둘 다 쟀다** `[실측]`
    (`TestDoctorCodexOwnedServerFilter`가 사례로 든다). ① 과검출: 이름 자체에 점이
    있는 인용 헤더(`[mcp_servers."ctr.env"]`)는 첫 세그먼트가 `ctr`이라 우리 이름으로 잡힌다
    — `[mcp_servers.ctr.env]` 서브테이블 헤더만 홀로 남아도 TOML 점 표기가 부모를 암묵적으로
    되살리므로, 첫 세그먼트 절단이 그 형태를 놓치지 않는 유일한 길이다. ② 미검출: 닫는
    따옴표를 빠뜨린 오타(`[mcp_servers."ctr]`)는 이름이 `"ctr`로 남아 — `unquoteHeaderName`은
    양끝 짝이 맞을 때만 벗긴다 — 등록물 보고에서 빠진다. 그 파일은 TOML로 파스되지 않으므로
    아래 파스 실패 줄이 무조건 떠서 그 사용자를 같은 자리(파일을 열어 고치는 걸음)에
    데려다 놓는다. **파스 판정도
    등록물 발견 여부에서 풀렸다** — `codexTOMLParses`는 이제 히트 유무와 무관하게 먼저 불려,
    우리 등록물이 하나도 없는 무효 `config.toml`에서도(모델 설정·훅 신뢰 항목·프로필이 함께
    죽는 파일에서 유일한 신호일 수 있으므로) 파스 실패 줄을 낸다. 다음 걸음은 그 결과로
    갈린다 — 파스되면 `codex mcp remove`를, 안 되면 그 명령도 닿지 못하므로 짚은 줄을 직접
    정리하라는 안내로 간다. `TestDoctorCodexOwnedServerFilter`·
    `TestDoctorCodexInvalidTOMLUnconditional`이 잰다.
14. **훅 등록물의 릴리스 간 바이트 동일성 — 속성은 옮겨갔고 새 집은 아직 무주장이다.**
    D82는 서로 다른 두 버전이 훅 등록물을 바이트 그대로 같게 조립한다고 결정했다. 그 핵심
    단정(`TestHookRegistrationBytesVersionIndependent`, 4단정)은 조립기(**쓰기 경로**)가
    있어야 성립했고, 넷 중 셋이 그 조립기를 요구했다 — 이번 릴리스가 조립기를 지웠다(D97
    계약 1). 살아남은 것은 **표식 인식**뿐이다: `scanRegisteredHooks`가 무버전 마커
    (`context-router`)와 구버전 마커(`context-router/0.14.0`)를 똑같이 우리 것으로 읽고
    대칭 제거한다는 단정은 여전히 돈다(`TestLegacyVersionedMarkerStillOwned`). **바이트
    동일성 자체의 새 자리는 정적 매니페스트(`hooks/hooks.json`)다** — 그 파일이 릴리스마다
    같은 바이트여야 그 속성을 물려받는데, 지금은 아무것도 그것을 확인하지 않는다. →
    매니페스트가 쓸 훅 표식이 `isOurHookGroup`/`scanRegisteredHooks`가 받아들이는
    **무버전** 형태(`context-router`)인지 확인하는 단정이 필요하다. 버전 붙은 표식을
    쓰면 `hookScope`의 `marker != version` 분기(`cli.go` [9]·[16])가 매 릴리스 마이그레이션
    경고를 낸다 — D82가 막으려던 바로 그 증상이다. §5-8과 같은 매니페스트를 겨눈 항목이다.

## 6. 기록 정정

- **`codex mcp add`는 기록만큼 파괴적이지 않다.** v0.18 핸드오프가 *"주석 전부·미지 키·
  `args = []`를 지운다"*고 적었으나, 514바이트 시드 파일의 전후 바이트 비교 `[실측]`: 파일
  머리·끝 주석, 미지 키, 미지 테이블, 손대지 않은 최상위 키가 **따옴표 종류·공백 폭·꼬리
  주석까지 바이트 그대로** 살아남는다. 재작성은 **`[mcp_servers.*]` 서브트리 한정**이고 거기서
  테이블 헤더 **바로 위** 주석·`args = []`·인라인 `env = {}`(하위 테이블 전개)·따옴표 정규화가
  일어난다. 두 번 돌리면 SHA-256까지 동일(멱등). 같은 이름에 다른 정의를 주면 **병합이 아니라
  통째 교체**이고 이전 `env`가 조용히 사라진다.
- **그리고 `codex plugin add`는 그 직렬화기를 타지 않는다** `[실측]` — A⑦. 같은 씨앗 파일에
  대해 `mcp add`는 `mcp_servers` 서브트리를 재작성하고 `plugin add`는 **순수 추가**다. 두 명령이
  같은 파일을 다르게 다룬다는 것이 이 설계의 실무적 근거다: 사용자가 플러그인으로 옮겨도
  기존 설정이 상하지 않는다.
- **`codex mcp remove`는 없는 이름에도 exit 0을 낸다** `[실측]`. 종료 코드로 성공을 못 가른다.
- **ctxscribe README의 "an installed ctxscribe plugin" 문장은 저장소에 없다** `[실측]`.
  v0.18 핸드오프 §7이 인용했으나 실물은 매니페스트 쌍이고 결론은 바뀌지 않는다.
- **자격증명 경로 하나** `[실측]`: `codex mcp list`(표)는 `env` 값을 `*****`로 가리는데
  **`--json`은 평문으로 낸다.** `mcp get --json`도 같다. 문서와 `doctor` 안내가 그 명령을
  권할 때 걸린다. D95가 `.mcp.json` 백업에서 본 것과 같은 부류다.
- **`hookRegistrations`·`codexRegistrations`(`internal/cli/hook_install.go`)는 냉동된
  역사적 상수다.** 옛 설치기가 남긴 이벤트 이름 목록이고, 지금은 `hook uninstall`의 제거
  쪽만 읽는다(D96 계약 1의 유일한 예외). 플러그인 매니페스트(`hooks/hooks.json`)와는
  **아무것도 묶여 있지 않고, 묶여서도 안 된다** — 존재 이유는 제거기가 옛 설치기의
  산출물을 알아보게 하는 것이지, 매니페스트가 선언할 이벤트 집합을 정하는 것이 아니다.
- **훅 그룹 소유 판정은 전건이다 — 혼합 그룹은 불가침이고, 그 대가를 우리가 진다.**
  `isOurHookGroup`(Claude)·`isOurCodexGroup`(Codex)은 마커가 소유 값이고 그룹의 **모든** 훅
  항목이 우리 명령일 때만 자기 그룹으로 판정한다. Codex 쪽은 도입 커밋부터 이 규칙이었고
  `[실측]`(`60a36ee`, 2026-07-21), Claude 쪽은 any-판정이라 사용자가 항목을 더한 그룹을
  통째로 지웠다 — 구현 라운드 적대 검토(2026-08-08) F2가 그것을 잡았고 이제 두 호스트가
  같다. **판정의 근거**: 남의 항목을 함께 지우는 것보다 우리 항목이 남는 편이 낫다
  (파손 금지 > 멱등 완전성).

  **그 대가는 진단에서는 닫혔다** `[실측]`. `scanRegisteredHooks`·`scanCodexRegisteredHooks`가
  이제 `held`(둘째 반환값 — 마커가 소유 값이거나(Claude) 항목 레벨 소유 조건을 만족하는
  항목이 하나 이상인(Codex), **전건은 아닌** 그룹의 수)를 함께 낸다. 혼합 그룹만 남은
  사용자에게 `doctor [9]`·`[16]`은 여전히 제거 가능 개수를 `옛 그룹 없음`으로 세지만, 이제
  그 뒤에 **동거 부류를 별도로 세어** "사용자 항목과 함께 있는 우리 항목 N그룹 — 호스트의
  /hooks에서 그 항목을 지우세요(플러그인 훅과 겹쳐 같은 포착이 두 번 일어납니다)"를
  병기한다 — **우리 명령이 파일에 남아 계속 발화하는데 진단이 잔존을 짚지 않는다**던 이전
  문장은 더는 맞지 않는다. v0.18 D92 계약 7이 **"틀린 성공"**이라 부른 형태와의 차이(남기는
  것은 의도된 판정, 정리 경로는 사용자의 `/hooks`)는 그대로지만, 그 판정을 진단이 짚지
  않는다는 빚은 갚혔다. **소유 판정(전건 규칙)은 건드리지 않았다** — 늘어난 것은 보고뿐이다.
  CHANGELOG의 `hook uninstall` 항목이 이 형태를 명시하고,
  `TestDoctor_MixedGroupReportedAsHeld`가 두 갈래(Claude·Codex) 모두 측정한다.

## 7. 참조 구현이 우리 대신 치른 값 (증거이지 지시가 아니다 — S1)

두 저장소를 읽기 전용으로 분석했다 `[실측: 클론 후 소스·ADR 인용]`. 이 절은 그들이 **측정으로
버린 것**의 목록이다. 같은 실험을 다시 하지 않기 위해 남긴다.

- **설득 전부** — CLAUDE.md 벽·호출별 훅 팁·스킬 문서를 최대치로 깔고 유기적 채택률 **3.82%**.
- **훈계 어휘** — 38시행 A/B로 역효과가 측정돼 금지 토큰표로. 부정 해명은 두 번 삭제.
- **무차별 팁** — *"경고를 무시하도록 에이전트를 훈련시키는 반복적 85토큰"*으로 판정, allowlist로 축소.
- **넓은 hard deny** — 100 KB 범위를 적대적 리뷰가 기각(큰 읽기의 ~17%가 편집 선행), 1 MiB로 축소.
- **전부-지연** — `ctx_index` 62일 0회.
- **자기 통계의 과대계상** — `bytesAvoided`가 차단하지 않은 읽기까지 계상해 별건 결함으로 추적.

그리고 그들이 자기 정체성으로 적은 문장이 우리에게도 그대로 걸린다:

> a good search index over session history plus a prompt that makes the model write scripts —
> R1 doubles down on the part that is real (the index) and **removes dependence on the part
> that is not (voluntary adoption).**

## 8. 적대적 검토 (2026-08-07) — 둘 다 승인하지 않았다

두 갈래를 돌렸다: 공개 문서 대조와 교차 모델 한 패스(다른 제공자, `cross-model-review` 절차,
식별자 별칭·한 결정만 송신). 결과를 있는 그대로 적는다.

### 8.1 검토가 정정한 우리 주장 넷

| 우리가 적었던 것 | 정정 |
|---|---|
| 양쪽이 같은 마켓플레이스 형식을 받는다 | **틀림.** 경로도 스키마도 다르다(D98이 반영) |
| 거부는 exit 2 **또는** JSON | **부분 정정.** 배타적이다 — `exit 2`는 stdout을 무시한다. `permissionDecision`은 넷이다 |
| `${user_config.*}`가 훅 명령에 치환된다 | **부분 정정.** 셸 형식에서는 **v2.1.207에서 거부되도록 파괴적으로 바뀌었다.** monitor는 환경 변수조차 못 받는다 |
| `outputSchema`는 프롬프트에 안 실린다 | **문서 근거 없음.** 우리 측정은 유효하나 **문서화된 계약이 아니라 구현 특성**이다 — MCP 명세는 오히려 LLM 전달을 이점으로 서술한다 |

### 8.2 우리가 몰랐던 비용·제약 `[문서]`

- **Tool Search는 베타 API 헤더 위에 선다.** 해제 조건이 여섯이고(환경 변수 둘 · 비-1st-party
  엔드포인트 · 특정 클라우드 배포 · 모델 세대), `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS`는
  **재정의 불가**로 끈다. D99의 전제가 이 헤더 위에 있다.
- **`alwaysLoad: true`는 세션 시작을 최대 5초 붙잡는다** — 서버가 뜰 때까지 기다린다.
  토큰 비용만 있는 게 아니었다.
- **이름은 사실상 불변이다** — 마켓플레이스 항목 `name` 변경에는 별도 `renames` 맵이 필요하다.
  **지금 잘못 고르면 되돌리는 값이 영구적이다**(D98의 이름 판정이 그만큼 무겁다).
- **`version`이 없으면 커밋 SHA가 버전이 된다** — 매 커밋이 새 버전으로 읽힌다.
- **관리자가 배포 경로를 통째로 막을 수 있다** — `strictKnownMarketplaces`·`blockedMarketplaces`.
- **민감 `userConfig`는 OAuth 토큰과 키체인 2KB를 공유한다.**
- **워크스페이스 신뢰 장벽**: 클론한 저장소는 자기 `.mcp.json` 서버를 스스로 승인하지 못한다.
  (플러그인 선언 서버는 이 관문을 지나지 않았다 — 실측)

### 8.3 판정과 대조

**웹 대조: "절반만 지금 하라."** MCP 계층은 문서로 뒷받침되나 **플러그인 배포 계층에 설치 경로
전체를 거는 것은 시기상조**이며 플러그인을 보조 경로로 두라는 것. 근거는 최근 릴리스 열 중
아홉이 플러그인 기제를 건드렸고 `${user_config.*}` 규칙이 이미 한 번 파괴적으로 바뀌었다는 것.

**교차 모델: `needs_changes`.** 검증 서브가 그 주장을 accept 2 · downgrade 3 · reject 1로
가른 뒤 **자체로 여덟을 더 찾았고**, 그중 §5-8(수동 포착 소멸)과 §2(제거가 0번 걸음)가 이
문서를 실제로 바꿨다. 기각 하나는 "설치 전 preflight"였다 — D96 아래 실행 지점이 없다
(설치를 호스트 CLI가 하고 우리 바이너리는 그 경로에 없다).

**우리 실측이 이미 닫은 것**: 게시 종단 검증(A⑨) · 설치의 무해성(A⑦) · 기능 플래그 기본값
(§5-9) · 훅 발화(§5-1) · 중복 제거 함정(A⑧, 검토가 물은 마이그레이션 질문의 답).

**닫지 못한 것은 표면의 시간적 안정성**이고 그것은 측정으로 닫히지 않는다 — 관측 기간이 필요하다.
전면 전환이냐 헤지냐는 그러므로 **측정이 아니라 소유자 판정**이다.
