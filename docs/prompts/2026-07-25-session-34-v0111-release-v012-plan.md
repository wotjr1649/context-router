# Session 34 — v0.11.1 릴리스 + v0.12 설계·계획 확정 (2026-07-25)

1. **Session/span/model**: session-34, 2026-07-25. Opus 5(1M, max effort) 단일 모델.
   구현/리뷰/fix 서브에이전트 전부 `model:"opus"` 명시.

2. **Starting prompt (verbatim)** — session-33 next-session 프롬프트로 부팅:
   > docs/prompts 최신 기록(session-33)을 읽고 재개. **v0.11 릴리스 완료** — main=`9f58277`(merge #23),
   > tag `v0.11.0`(태그 CI ALL GREEN, tag-version success=D56 검증). D58~D62 전부 구현·이중리뷰·3-OS CI
   > 실검증 완료(격리 결함 4라운드 근본해소, 전부 격리 무손상). **다음 후보**: ① 도그푸딩 마무리(사용자 액션
   > — `hook install` cc/cx marker 0.11.0 갱신·MCP `--enable exec` 재기동 실사용·cx `/hooks` 재신뢰,
   > doctor `[16]` 0.10.0≠0.11.0 지적) ② v0.11 실관측(exec 실사용·adoption 베이스라인) ③ v0.12 스코프
   > 브레인스토밍(**사용자 요청 신규 항목: 설치/배포 자동화** — MCP 등록·exec 활성화 수동 3파일 편집을
   > 원커맨드 자동 설치/업데이트로 개선; hook install 병합 인프라 확장·self-heal·exec 명시 opt-in;
   > 상세 핸드오프 5번 Carryovers + 메모리 v012-install-deploy-automation). 이월 Minor
   > (`.superpowers/sdd/final-review-findings.json` defer 9)는 v0.12+ triage.
   > 크로스모델 리뷰=중립 프레이밍+JSON findings(`docs/codex-secure-review.md`), 막히면 웹+Codex 협업,
   > CI 3-OS가 unix 실검증 권위(`gh pr checks` 전체 확인). Fable 유지+보안 서술 최소화. 사용자 지시 대기.

   이후 사용자가 순차 지시: 세 후보 전부 진행 → 계층/골격 적대 검수 → bypass에서 ask 없이 즉시 실행
   요구 추가 → writing-plans → 계획도 적대 검수 후 다음 세션에서 subagent-driven 실행.

3. **What was done**

   **도그푸딩이 결함을 잡았다(CI가 구조적으로 못 보는 경로)**. 클라이언트 경유로 exec 러너를 전수
   실사용: shell(pwsh 7)·go 1.26.5·javascript(bun 1.3.14)·typescript·python 3.14.2·`ctr_execute_file`
   전부 통과, **csharp만 `NU1301` 실패**. 원인은 호스트 `%APPDATA%\NuGet\NuGet.Config`에 등록된
   부재 로컬 소스였다. `NUGET_*` env는 캐시·패키지 *경로*만 재지정하고 *구성 파일 검색 경로*는
   격리하지 않는다. unix는 `csEnv`의 `HOME` 재지정이 사용자 구성을 우연히 가려 3-OS CI가 이 경로를
   볼 수 없었고, `TestRunCS`는 그 실패를 "환경 비가용" Skip으로 흡수해 결함이 테스트에 드러나지
   않았다.

   **v0.11.1 릴리스**:
   - `0cbbc1c` — `csEnv`가 스크래치에 `nuget.config`를 배치(섹션별 `<clear />` 5종: config·
     packageSources·disabledPackageSources·packageSourceMapping·fallbackPackageFolders).
     `config`의 `globalPackagesFolder`가 `NUGET_PACKAGES` env보다 우선하므로 격리 계약 보호에도 필요.
     `ctr_execute`/`ctr_execute_file` 스키마에 shell 방언 표기.
   - `207bf40` — productVersion 0.11.1 (D56).
   - `819040d` — 교차 리뷰 반영 3건: 방언 폴백 정정(pwsh 부재 시 powershell 5.1), 게이트 11
     기준값 12,864B→12,914B, `nuget.config` 기록 실패 경고.
   - PR #24 머지 `f27a36a`, tag `v0.11.1`, **태그 CI success**(tag-version = D56 실태그 검증).
   - **A/B 대조로 검증**: 수정 전 `TestRunCS` SKIP(NU1301) → 수정 후 PASS(14.0s).

   **Codex P1을 기각했다**. "소문자 `nuget.config`가 대소문자 구분 FS에서 미발견"이라는 지적은
   NuGet.Client `Settings.cs`로 반증했다 — `OrderedSettingsFileNames`가 대소문자 구분 환경에서
   소문자를 **첫 번째(preferred)** 로 두고, FS 판정은 OS 분기가 아니라 실제 프로브다. 서브에이전트도
   독립 검증 후 동의. 다만 그 지적이 드러낸 "구성 *적용*을 검증하지 않는 테스트 갭"은 D65에 반영했다.

   **v0.12 설계** (`a4dfa24`, `6aac698`, F10 개정 포함 `07a8958`): `docs/context-router-design-v0.12-ko.md`
   D63~D67. 브레인스토밍 중 설계 골격 2건이 검수에서 무효화돼 설계 단계에서 고쳐졌다(상시 로드 대상
   오지정, 게이트 수치의 상주 비용 오용).

   **v0.12 계획** (`ec62a72` → 검수 반영 `07a8958`): `docs/superpowers/plans/2026-07-25-v0.12-d63-d67.md`
   T1~T12, 2,205줄. 교차 검수 15건(critical 3·important 9·minor 3) 중 14건 반영, F10은 사용자 결정으로
   설계서를 개정.

   **로컬 설정 변경(gitignored·untracked — 커밋 대상 아님)**:
   - `.mcp.json`을 `ctr-exec` 단일 서버로 통일(`ctr` 제거) — 6도구 중복 해소.
   - `.claude/settings.json`에서 exec 2종 `permissions.ask` 제거 — `bypassPermissions`에서도 ask가
     프롬프트를 강제하고 평가 순서(deny→ask→allow)상 `settings.local.json`의 `allow`가 무력화돼
     있었다.

4. **Repo state**: main HEAD=`07a8958`, tag `v0.11.1`(태그 CI success), 이전 tag `v0.11.0` 유지.
   **오픈 PR 없음.** 브랜치 `fix/v0.11.1-exec-runner-hardening`(머지됨, 미삭제).
   working tree clean(untracked `.claude/`·`.codex/`·`.mcp.json`은 로컬 설정).

5. **Carryovers**

   - **v0.12 실행 = 다음 세션의 본 작업**. 계획 `docs/superpowers/plans/2026-07-25-v0.12-d63-d67.md`를
     superpowers:subagent-driven-development로 **T1부터** 실행한다. BASE는 원장에 기록(`HEAD~1` 금지).
     **T1이 선행 게이트**다 — 설계 §1.3이 계측 교정을 상시 로드보다 먼저로 못박았다. 순서가 뒤집히면
     전후 비교의 근거가 사라진다.
   - **사용자 액션(미완)**: ① MCP 재기동 — `.mcp.json` 단일화와 settings ask 제거는 세션 로드 시점
     기준이라 재기동해야 반영된다. ② `hook install`로 marker 0.11.0→0.11.1 갱신(doctor `[9]`·`[16]`).
     ③ doctor `[9] user=미등록`을 등록할지 판단(project 등록만으로 동작 중).
   - **관측 이월**: D63 ④(SessionStart 안내 도입 여부)와 D67 임계값 재설정은 **T6·T12 적용 후
     정상상태 관측**이 선행되어야 정할 수 있다. 설계서가 둘 다 조건부/관측 후로 규정했다.
   - **이월 Minor**: `.superpowers/sdd/final-review-findings.json` defer 9건 중 2건(`rows.Err()` 누락,
     음수 `days`)은 T1이 처리한다. 나머지 7건은 이 계획과 접점이 없어 별도 triage.
   - **v0.13+ 후보**: FTS 인덱스 구성 재검토(porter·trigram 이중 인덱스가 chunk 텍스트 대비 약 3.6배
     승수 — 보존 정책으로는 승수를 줄일 수 없다), `ctr_batch_execute` 재도입, Windows AppContainer,
     타인 배포 채널.

   **관측 실측(session-34, D67·D63 근거)**:

   | 항목 | 값 |
   | --- | --- |
   | adoption 호출 수 | ctr 9→16(exec 7회), ctxscribe 1,189 |
   | **adoption 세션 분모** | tool_call 세션 59 중 `mcp__ctr` 5(8.5%) / `ctx_` 42(71%) |
   | content.db | page 43,052×4,096B=168.2MiB, **freelist 284(0.7%)** |
   | 나이 분포 | ≤1d 155건 / ≤2d 291 / ≤3d 271 / ≤7d 277 / **7d 초과 0건** |
   | chunk 텍스트 vs 파일 | 36.0MiB vs 168.2MiB(**FTS 약 3.6배**) |
   | 성장률 | 하루 약 33–37MB(세션 중 172.4→176.3MB) |
   | shadow | 42,950,019B / 992 해시(cc 39.5MB · cx 3.4MB) |
   | 스키마 표면 | `ctr` 6도구 10,466B / `ctr-exec` 8도구 12,914B(게이트 11 상한 13,000B) |

6. **Standing protocols** (유지 + 이번 세션 실증)

   - **교차 검수는 실행 전에 한다 — 값이 증명됐다.** 계획 검수 critical 3건이 전부 "실행하면 바로
     막히거나 깨지는" 것이었다. 특히 F2는 그대로 구현했다면 새 shadow의 chunks·sources만 지워지고
     artifacts 행이 남아 **이후 어떤 purge도 닿지 못하는 고아**를 만들었을 것이다.
   - **리뷰어 출력은 검증 대상이다.** Codex P1(케이싱)은 상류 소스로 기각했다. 반대로 fix
     서브에이전트는 실측으로 계획의 전제를 뒤집었다 — pwsh는 `PSModulePath`를 설정해도 호스트 사용자
     모듈 경로를 prepend하므로 T9의 유일한 레버는 shell 러너 스코프 `USERPROFILE` 재지정이었다.
   - **크로스모델 리뷰 = 중립 프레이밍 + JSON findings**(`docs/codex-secure-review.md`). 이번 세션
     3회 적용(코드 diff·설계 골격·계획) 전부 무사고. `review`는 focus를 받지 않으므로 초점 검토는
     `adversarial-review`에 **중립 엔지니어링 용어 + in-diff** 조건으로 건다.
   - **막히면 협업**: 3회 실패 STOP, 웹 리서치(라이브러리 사실)·Codex·서브에이전트 독립 진단 수렴.
   - **CI 3-OS = unix 실검증 권위**, `gh pr checks` **전체** 확인(tail 잘림 금지), 태그 후 tag-version 확인.
   - 유지: general-purpose 서브 `model:"opus"` 명시 / `go test -p 1` / gofumpt 로컬 / **`git add -A` 금지**
     (로컬 설정이 untracked) / Codex 1패스/체크포인트 / 스펙 승 / BASE 원장 / 보안 서술 최소화.

7. **Next-session starting prompt**:
   > docs/prompts 최신 기록(session-34)을 읽고 재개. **v0.11.1 릴리스 완료**(main=`07a8958`,
   > tag `v0.11.1`, 태그 CI success — 도그푸딩이 잡은 csharp 호스트 NuGet 구성 상속 결함을 차단).
   > **v0.12 설계·계획 확정 완료**: 설계 `docs/context-router-design-v0.12-ko.md`(D63~D67, 열린 항목
   > 없음), 계획 `docs/superpowers/plans/2026-07-25-v0.12-d63-d67.md`(T1~T12, 2,205줄, 교차 검수
   > 15건 반영 완료). **다음 = 계획 실행**: superpowers:subagent-driven-development로 **T1부터**
   > 시작한다(태스크별 fresh 서브에이전트 general-purpose는 `model:"opus"` 명시, 태스크 리뷰=스펙
   > 준수+품질, fix→재리뷰, BASE는 원장 기록·`HEAD~1` 금지). **T1이 선행 게이트** — 설계 §1.3이
   > 계측 교정을 상시 로드(T6)보다 먼저로 못박았고, 순서가 뒤집히면 채택 전후 비교의 근거가
   > 사라진다. 실행 순서는 계획 말미 "실행 순서와 의존" 참조(T2→T3→T6, T4·T5·T7·T8·T9·T10 독립,
   > T11→T12). **사용자 액션 대기**: MCP 재기동(`.mcp.json` 단일화·settings ask 제거 반영),
   > `hook install`로 marker 0.11.1 갱신. **관측 후 결정 2건**: D63 ④(SessionStart 안내)와 D67
   > 임계값은 T6·T12 적용 후 정상상태 관측이 선행. 크로스모델 리뷰=중립 프레이밍+JSON findings
   > (`docs/codex-secure-review.md`, `review`는 focus 미지원 → 초점 검토는 `adversarial-review`를
   > in-diff로), 막히면 웹+Codex+서브 협업, CI 3-OS가 unix 실검증 권위(`gh pr checks` 전체 확인),
   > `git add -A` 금지(로컬 설정 untracked). ultrathink
