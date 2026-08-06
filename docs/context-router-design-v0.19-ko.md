# Context Router v0.19 설계서 (D96–D100 — 남의 파일에 쓰지 않는다 · 채택은 설득으로 얻지 않는다)

델타 체인: `context-router-design-v0.0.1-ko.md` → `-v0.1-ko.md`(D14–D20) →
… → `-v0.17-ko.md`(D89–D91) → `-v0.18-ko.md`(D92) → **이 문서(D96–D100)**.

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
플러그인 경로로 옮기는 것이 사용자의 기존 설정에 **부수 피해를 주지 않는다**는 뜻이고,
이것이 D96을 지지하는 마지막 조각이다.

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
  .claude-plugin/plugin.json      name·version·skills  (mcpServers는 루트 .mcp.json 관례)
  .claude-plugin/marketplace.json 자기 자신이 마켓플레이스 (source "./") — 양쪽 공용
  .codex-plugin/plugin.json       mcpServers→"./.mcp.json" · hooks→"./hooks/codex.json"
                                  · skills→"./skills/"
  .mcp.json                       ★ 공유
  hooks/claude.json               Claude 매처
  hooks/codex.json                Codex 매처(`is_exact_matcher`라 `mcp__` 대신 `mcp__.*`)
  skills/<이름>/SKILL.md          ★ 공유
```

훅 파일을 하나로 합칠 수 있는지는 §5의 미확정 항목이다 — 정규식 매처를 양쪽 안전하게 쓰면
가능해 보이나 재지 않았다.

**이름**: 플러그인 이름과 서버 이름이 곱해져 도구 접두가 된다
(`mcp__plugin_<플러그인>_<서버>__`). **이름을 정하기 전에는 문서에 접두를 박지 마라.**

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

1. **진입 도구는 최소로.** 후보는 `ctr_search`(저장된 것으로 들어가는 유일한 문)와
   `ctr_index`(넣는 문). 확정은 §5.
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
3. **강제는 좁게, fail-open으로.** 겨눌 후보는 "이미 저장소에 색인된 바이트를 다시 읽는 것"과
   "임계값을 넘는 전체 읽기" 둘이다. 참조 구현의 적대적 리뷰가 100 KB 범위를 기각하고 1 MiB로
   좁힌 이력을 그대로 받는다 — 오탐이 편집 워크플로를 깨기 때문이다. 모든 갈래는 실패 시 통과.
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

---

## 1. 층별 대조

| 층 | 지금 | 이 설계 뒤 |
|---|---|---|
| 도구 표면 | MCP `ctr_*`, 서버 전체 상시 로드 | MCP 유지. **진입 도구만 상시**, 나머지 지연 + 설명 색인 |
| 등록 | 우리가 `.mcp.json`·`config.toml`을 손편집 | **호스트가 쓴다.** 우리 자국은 각 호스트 설정의 한 줄 |
| 가드 | 우리가 `settings.json`·`hooks.json` 그룹을 병합 | 매니페스트가 선언, 호스트가 읽는다 |
| 지침·발견 | **없다** | 짧은 스킬 한 벌, 양쪽 공통 |
| 채택 | **기제 없음 — 모델의 선택에 전적으로 의존** | 가시성 + 좁은 강제 + (후보) 수동 포착 |

## 2. 설치 절차

1. 바이너리 확보 — 릴리스 산출물 또는 `go install`. **지금과 같다.**
2. `claude plugin marketplace add <owner>/<repo>` → `claude plugin install <name> --scope <user|project|local>`
3. `codex plugin marketplace add <owner>/<repo>` → `codex plugin add <name>`

**우리도 사용자도 설정 파일을 편집하는 걸음이 없다** — §5-9가 Codex의 `hooks` 기능이 기본으로
켜져 있음을 실측했고, A⑦이 설치가 기존 설정을 훼손하지 않음을 실측했다.

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

1. **훅이 실제로 발화하는가** — **양쪽 다 닫혔다** `[실측]`.
   - **Claude**: 플러그인이 선언한 `PreToolUse` 훅이 이 Windows에서 발화하고(마커 파일이
     기록됨) exit 2가 거부로 존중된다(읽기가 실제로 차단됨).
   - **Codex**: 격리 홈에서는 인증이 없어 401이 세션을 끊는 바람에 직접 재지 못했다. 대신
     **이 머신의 실물이 증명한다**: 설치된 한 플러그인의 훅 스크립트가 `.ponytail-active`를
     ``process.env.PLUGIN_DATA``에 쓰고 그 코드가 ``const isCodex = … Boolean(process.env.
     PLUGIN_DATA); if (isCodex) stateDir = process.env.PLUGIN_DATA;``다. 그 파일이
     `~/.codex/plugins/data/<plugin>-<marketplace>/`에 실재하므로 **`PLUGIN_DATA`가 설정된
     프로세스 — 즉 Codex가 띄운 플러그인 훅 — 만이 그것을 쓸 수 있었다.** 사용자 설정을 한
     바이트도 건드리지 않고 얻은 증명이다. 보강 증거로 `config.toml`에 플러그인 범위 훅 신뢰
     항목 여덟이 있다.
   - **부수 확정 — Codex의 플러그인 환경 변수 둘**: `PLUGIN_ROOT`(경로 치환)와
     **`PLUGIN_DATA`(플러그인 전용 쓰기 가능 디렉터리)**. 우리 훅이 상태를 둘 자리가 여기다.
   - *미해결 관측 하나*: `cmd /c exit 2`만 둔 훅은 차단하지 못했고, 같은 exit 코드에 앞선 명령을
     더한 형태는 차단했다. 원인 불명 — 거부를 설계에 쓰기 전에 확인하라.
2. **훅 파일 하나로 양쪽을 섬길 수 있는가** — **닫혔다. 된다** `[실측]`. 설치된 한 플러그인의
   `.claude-plugin/plugin.json`과 `.codex-plugin/plugin.json`이 **같은 파일**
   (`./hooks/claude-codex-hooks.json`)을 가리키고, 진짜 `config.toml`에 그 파일의 항목별 신뢰
   해시가 있으며(Codex가 소비했다는 뜻), 같은 플러그인이 Claude 세션에서도 돈다.
   **조건 둘**: `SessionStart`에도 `matcher`를 달 것, Windows용 **`commandWindows`**를 함께 담을 것.
3. **`outputSchema`가 모델 컨텍스트에 실리는가** — **닫혔다. 실리지 않는다**(B①). D99 계약 4가
   그 결론이다.
4. **진입 도구 집합** — **표현 수단과 숫자는 확정, 집합 자체만 소유자 판정으로 남는다.**
   표현 수단 `[실측: go doc]`: 설치된 SDK의 `mcp.Tool`이 ``Meta `json:"_meta,omitempty"` ``를
   담는다. 참조 구현이 쓰는 키는 `"anthropic/alwaysLoad"`다.
   숫자 `[계산: D99 측정표의 도구별 내역 × B①의 밀도]` — **프롬프트에 실리는 것만** 센다:

   | 상시 집합 | `name`+`description`+`inputSchema` | 추정 토큰 |
   |---|---|---|
   | 후보 둘 (`ctr_search` + `ctr_index`) | 1,159자 | 약 390 |
   | 현행 전체 8도구 (`ingest,net`) | 4,571자 | 약 1,530 |

   **차이는 세션당 약 1,140토큰이고, 그와 별개로 지연 로드 기능이 사용자에게 돌아온다**
   (D99 근거 ②). 집합은 계약 3의 강등 규칙과 한 쌍으로 정한다.
5. **`userConfig` 스키마의 실제 형태** `[미실측]`.
6. **마켓플레이스 배포** — **닫혔다** `[실측]`. 양쪽이 `owner/repo` 원격 git을 1급 소스로 받고
   (`codex plugin marketplace add`의 도움말: *"a local path, owner/repo[@ref], HTTPS Git URL,
   or SSH Git URL"*), 양쪽이 `.claude-plugin/marketplace.json`을 읽으며(Codex가 설치된 플러그인
   하나에 대해 그 경로를 그대로 인쇄한다), `"source": "./"` 자기 호스팅 형태가 양쪽에서
   수용된다(내 프로브가 Codex에서, 설치된 플러그인이 Claude에서). `claude plugin validate`도 통과.
   **남은 것은 설계 위험이 아니라 게시 시점 연기 시험 하나** — 저장소를 실제로 게시해 양쪽에서
   한 번 설치해 볼 것. 부수: Codex의 `--sparse`가 git 마켓플레이스의 부분 체크아웃을 지원한다 —
   저장소가 커질 때의 수단이다.
7. **버전 번호 판정.** 도구 이름공간 이동은 사용자의 권한 규칙을 깬다. `0.19`인지 `1.0`인지는
   소유자 판정이며 이 문서는 파일명만 델타 체인에 맞췄다.
8. **수동 포착** — **닫혔다: 도입 여부의 문제가 아니었다.** 이미 배송된 동작이고 저장 전에
   리댁션을 지난다(D100 계약 4가 필터 사슬 전문을 적는다). 남는 것은 "패턴 리댁션이 Bash
   출력에 충분한가"이며, 그것은 이 설계의 범위 밖이고 설계 §5.1의 알려진 한계로 이미 추적된다.
   **이 릴리스는 그 동작을 바꾸지 않는다.**
9. **`[features] hooks`** — **닫혔다** `[실측]`. `config.toml`이 **아예 없는** 새 `CODEX_HOME`에서
   `codex features list`가 `hooks` · `plugins` · `plugin_sharing` · `skill_search`를 전부
   `stable / true`로 낸다. `plugin_hooks`는 `removed / false`다 — 참조 구현 README가 요구하는
   그 키는 낡았다. **사용자가 설정 파일에 한 줄도 칠 필요가 없다.**

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
