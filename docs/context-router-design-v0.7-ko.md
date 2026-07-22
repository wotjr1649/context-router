# Context Router v0.7 설계서 (Codex 가드 동등성 — D35 후속)

- 전제: v0.6.1 태그(main 28a5958, PR #18, CI 3-OS GREEN), 도그푸딩 marker
  0.6.1. Codex는 훅 캐프처(SessionStart+PostToolUse)만 등록, MCP 등록은
  session-20에서 제거된 상태(스테일 0.1.0 차단 — bare 등록은 가능 상태).
  실측(2026-07-23, §7): cx: tool_call 148건 분해 — Bash(exec) 48건 중
  Get-Content 22건(46%, raw PowerShell 구문·pwsh 래핑 없음), MCP 도구
  95건, update_plan 5건. cc: 관찰 A/B 신호 2점째 안정(cache_read/rec
  0.60→0.643). D46 파일 축 92.5→94.3MB(임계 89.9% — 미발화).
- 스코프 확정 근거: session-22 브레인스토밍 → 사용자 결정 4건(축=D35
  후속 Codex 가드 동등성 / deny 대체 경로=ctr MCP 재등록 포함(완전
  동등성) / 설치=`hook install --codex`가 MCP까지 자동 병합(TOML 관리
  블록) / 편승=D46 회수 자동화 **설계만 선행**) + 접근 A(게이트
  이식+관리 블록) 승인 + 설계 4섹션(§A~§D) 승인. 초안의 "게이트
  직렬"·"헤더 라인 스캔"은 이중 적대 검수 1패스가 반례로 폐기 —
  호스트×GOOS 단독 게이트·보수 광역 스캔으로 교정(§10).

## 0. 결정 이력 (v0.7 신규 — D46까지는 v0.6 설계서·이전 델타 체인)

- **D47** Codex 가드 동등성: cx: PreToolUse에 D25·D32·D36·D39의 덤프
  가드를 확장한다 — **가드 판정 로직(조건·순서)은 무수정**이며, deny
  **출력 직렬화**만 G4 판정에 따라 호스트 매핑될 수 있다(§8 행 2).
  구현 갭은 3구성(§2) — ① hooks.json PreToolUse 등록(설치기), ②
  **호스트×GOOS 단독 게이트 선택**: cx:+Windows는 **PS 게이트
  (psDumpArg+psAbsPath) 단독**, cx:+비Windows는 **bash 게이트
  (bashDumpArg+dumpAbsPath) 단독**, cc:는 현행 유지. 게이트는 어휘
  술어+절대경로 해석이 묶인 **완결 정책 단위**라 술어만의 직렬·혼합은
  불건전하다 — 초안의 "ps→bash 직렬"은 적대 검수 1패스가 폐기(§10):
  cx:Windows `cat /c/big.log`에서 bash 레그의 MSYS 변환이 psAbsPath가
  의도적으로 배제한 오파일 deny를 재도입(v0.4 §11.1 파생②,
  hook_test.go의 1450 vs 1506 계약이 분기를 명문 고정)하고, 경로
  해석을 역방향 통일하면 cc:의 `cat /c/…` MSYS deny가 회귀한다. ③
  Codex PreToolUse deny 응답 계약 정합(G4 게이트). 라우팅 자체는
  신설이 아니다 — `dispatch`는 호스트 무관 공유 경로라 PreToolUse
  분기가 이미 존재하나(hook.go:153, §2 실코드 확인) **host가 현재
  세션 접두 문자열로만 진입하므로 Run→dispatch→가드 명시 전달을
  신설**한다(현행 가드는 host 무분기 — 사실상 cc: 고정 동작을 명시
  host 스레딩으로 대체, 재검수 용어 정밀화 §10). 게이트
  선택의 실측 근거는 §7(Windows Codex exec = raw PS 구문 지배).
  deny 사유의 ctr_search/ctr_fetch 안내는 D48(MCP 재등록)로
  실효화된다 — 안내된 도구가 없는 "차단+복구 불능" 조합 금지(D32
  원칙)가 범위 결합의 근거. deny 시 warning 이벤트는 cc: 동형으로
  cx: session.db에 기록(실발화 관측 표면). matcher는 `Bash`
  단독(Codex 표면에 Read·PowerShell tool_name 부재 — §7 실측 148건
  정합). **설치 결합**(2패스 교정 §10): PreToolUse 그룹 등록은 D48의
  MCP 대체 경로 확정 — 관리 블록 기입/갱신 성공 **또는** 블록 밖 정확
  `[mcp_servers.ctr]` 헤더 실존(사용자 소유 등록=대체 경로 실존) —
  시에만 수행한다. 미확정(충돌 스킵·마커 이상)이면 가드 등록을
  보류하고 캐프처만 유지+안내 — "deny + 안내 도구 부재"(D32 위반)
  조합을 설치 시점에 구조 봉쇄. **본 축 전체가 G4에 게이트된다** —
  행 3 판정 시 가드 축은 중단·보고하고 D48은 독립 진행한다(§8, 적대
  검수 Important-3 교정: 확정 어조 금지).
- **D48** Codex ctr MCP 자동 등록 + PreToolUse 훅 등록: `hook install
  --codex`가 (a) hooks.json에 PreToolUse 그룹 추가(기존 JSON 병합기 —
  F4 전건 소유 판정·정의 변경 재신뢰 안내 승계), (b)
  `~/.codex/config.toml`에 **관리 블록**(`# BEGIN context-router` …
  `# END context-router`)으로 `[mcp_servers.ctr]`(doctor 스니펫 동일 —
  bare command + enabled_tools)을 병합한다. TOML 병합은 **블록 원자
  교체**(임시 파일+rename) — 블록 밖 바이트 무변경이라 D28 원칙(멱등·
  원자 쓰기·타 항목/주석/미지 키 보존·제거 대칭·실패 시 원본 무변경)
  중 보존이 **구조적으로** 충족된다. TOML AST round-trip은 기각 —
  go-toml v2는 주석을 보존하지 않아(D28 위반) v0.4 G3가 병합기 신규
  구현을 피했던 사유가 그대로 유효하며, 관리 블록은 파서 없이 원칙을
  만족하는 최소 구현이다. 충돌 규칙(1·2패스 교정 §10 — 초안의 헤더
  라인 스캔은 quoted key·인라인 테이블·부모 테이블+점표기 합법
  변형을 놓쳐 중복 정의 파스 에러를 허용, 1패스의 무경계 동시-등장
  규칙은 `electron`·`spectra`가 `ctr`를 부분열로 포함해 오탐 영구
  생략 — 양쪽 폐기): **키-경계 보수 스캔** — 블록 밖 내용에
  `mcp_servers` 존재 **AND** ctr **키-경계 신호**(공백 제거 라인
  정규화 기준 `ctr]`·`ctr"`·`ctr'`·`ctr=`·행두 `ctr.`·`.ctr.` —
  마지막은 루트 완전-점표기 `mcp_servers.ctr.command=…` 포착, 재검수
  Minor 교정 §10.2) 존재 시 표기 형태 불문 기입 생략+안내 1줄. 표기
  관례 변형(헤더·quoted key·인라인·부모 테이블+점표기·루트
  완전-점표기)이 경계 신호를 동반해 그물에 걸리고(2패스·재검수 실행
  확인) 단순 부분열은 경계 신호 부재로 오탐하지 않는다. 잔여 오탐(값 문자열 `"ctr"` 등)의 실패
  방향은 설치 거부+수동 안내라 안전하며(설치 거부≠파손 — F4 불가침
  우선) 이때 D47 가드 등록도 함께 보류된다(설치 결합 — §0 D47).
  TOML 파서 의존은 계속 비도입 — go.mod에 TOML 의존 0(실측), 검증
  전용 파싱은 D28 반론(round-trip 보존)이 적용되지 않는 별개
  선택지이나(§10) 경계 스캔이 의존 0으로 같은 봉쇄를 달성한다.
  **마커 소유권+무결성**(2패스 Critical 교정 — 접두 자유 텍스트
  매치는 사용자 주석 블록 `# BEGIN context-router migration` 등을
  오소유해 교체·삭제함): 마커는 **고정 문자열 정확 라인 매치**
  (`# BEGIN context-router` / `# END context-router` — 자유 접미
  폐기, 버전 문자열 없음)이고, BEGIN/END 정확 1쌍·정순 **AND 블록
  본문에 `[mcp_servers.ctr]` 헤더 라인 실존(본문 검증)**이 전부
  성립할 때만 소유 블록으로 교체·삭제한다. 하나라도 불성립(단독·
  중복·역순·본문 불일치)이면 **무변경+안내**, 양쪽 마커 부재는 정상
  append 경로(§3 — both-absent는 이상이 아니다). multiline string 내
  정확 마커+본문 오인은 이론상 잔존(희귀 한계 명문, §3). **개행
  계약**: 병합은 바이트 스팬 연산으로 원본 개행
  형식(CRLF 포함)을 보존하고, 원본이 개행 없이 끝나면 append 경로가
  EOF 개행 1개를 추가한다(유일하게 허용되는 블록 밖 바이트 변경 —
  명문 한계). 왕복 `f_uninstall(f_install(x))`은 그 EOF 개행을
  제외하고 바이트 동일(제거=블록+설치가 추가한 선행 구분 빈 줄 1개
  삭제), 동일 버전 `f(f(x))==f(x)` 바이트 멱등, 블록 부재 시
  uninstall 무변경. `[hooks.state]`(Codex 소유 trust-hash) 불가침
  유지.
- **D49** content.db 파일 축 회수 — **설계 선행·구현 이월**: 경로
  후보로 `purge` 계열 `--vacuum` 플래그(행 삭제 후 VACUUM 실행)를
  채택하되 구현 착수 조건은 **D46 경고 실발화**(현재 임계 100MiB의
  89.9%). 제약
  명문화: VACUUM은 라이브 MCP 서버 가동 중 불가(파일 잠금 — 세션 락
  표면으로 사전 감지 가능), 자동 실행 없음(수동 트리거 일관 원칙),
  purge 행 삭제 후에도 free page라 VACUUM 전까지 파일 크기 불변(D46
  경고 문구가 이미 안내). 상세 계약(옵션 이름·잠금 검사·진행 보고)은
  발화 후 버전에서 확정한다.

## 1. v0.7 제품 계약

### 1.1 범위

- cx: PreToolUse 덤프 가드 — 호스트×GOOS 단독 게이트(§2). **G4 게이트
  조건부**(2패스 Minor-8 정합): 행 3 판정 시 본 항목은 제외되고 아래
  D48 항목만 진행.
- `hook install --codex` 확장 — config.toml 관리 블록 MCP 병합 +
  (MCP 확정 시) PreToolUse 훅 등록(§3 설치 결합 순서).
- D49 설계 §(§4 — 구현 없음), A/B 해석 주석(§5), §7 실측 교정 추기.
- 버전 0.7.0 범프(2지점), 도그푸딩 갱신.

### 1.2 명시적 비범위 (v0.7)

- exec 3종(D21 트랙), 무작위 A/B 하네스·OTel(D27), 서브에이전트 캡처,
  register-on-first-event(D43), Grep 가드 — 전부 이월 유지(v0.6 §8).
- apply_patch·`mcp__*`·update_plan tool_name 가드 — 덤프 표면이 아님
  (§7 실측: 덤프 성향은 Bash(exec)에 집중).
- PS 명령의 이벤트 2차 분류(v0.4 §1.2 유지).
- D49 구현(착수 조건 미충족 — 경고 실발화 대기).

## 2. 가드 이식 상세 (D47)

- **실코드 기점**: `hook.Run(host)` → `dispatch`는 이미 호스트 무관
  공유 경로다 — PreToolUse 분기(hook.go:153)와 guardBash·guardRead·
  guardPowerShell 라우팅이 cx: 진입에도 살아 있고, 현재 가드가 도달
  불가한 유일한 이유는 **hooks.json에 PreToolUse가 미등록**이기
  때문이다(v0.4 G3 "캐프처 전용" 결정). 따라서 이식의 최소 갭:
  1. 설치기가 PreToolUse 그룹을 등록(§3).
  2. **호스트×GOOS 단독 게이트 선택**: host를 Run→dispatch→guardBash
     명시 파라미터로 전달(현재는 세션 접두 문자열에만 반영 — 가드는
     host 무분기로 사실상 cc: 고정 동작)하고, `cx:`+Windows이면
     tool_input.command에 **PS 게이트(psDumpArg+psAbsPath) 단독**,
     `cx:`+비Windows이면 **bash 게이트(bashDumpArg+dumpAbsPath)
     단독**, `cc:`이면 현행 유지. 게이트는 술어+경로 해석의 완결
     정책이라 직렬·혼합 금지(§0 D47 — cx:Windows `cat /c/…` 반례:
     PS 시맨틱에서 `/c/x`는 현재 드라이브 상대이므로 bash 레그의
     MSYS 변환은 오파일 deny, v0.4 §11.1 파생② 위반). 근거: Codex
     Windows exec는 raw PS 구문(§7 — Get-Content 22건, pwsh 래핑
     없음: 래핑은 실행 계층이고 tool_input.command는 원문), Unix
     Codex는 sh 구문. cc: Bash에 PS 게이트를 합류시키지 않는 이유:
     Claude의 Bash는 Git Bash(POSIX)라 PS 구문이 실행 불가 명령이고,
     게이트 합류는 시맨틱 오염만 남긴다(접근 B의 프로파일 테이블은
     호스트 2개에 과잉 — 접근 A 채택). **셸 방언 한계(명문, 2패스
     §10)**: hook 입력에는 실행 셸을 식별할 신호가 없다(G1 실측 —
     command 원문뿐). host×GOOS 선택은 §7 실측(Windows Codex=raw PS
     구문)에 근거한 추정이며, 반대 방언 덤프 — Windows-cx의 POSIX
     구문(`cat /c/…`), 비Windows-cx의 pwsh 구문(Get-Content) — 는
     **miss=allow**로 통과한다. 어휘 게이트의 allow-편향
     by-design(v0.4 §3 비ASCII·8.3 경로 선례 동급)이고, 셸 신호 기반
     게이트 선택 권고는 신호 부재로 구현 불가라 기각. cx: 실측에서
     반대 방언 덤프가 관측되면 재상정.
  3. **deny 응답 계약 정합(G4)**: 현행 deny는 Claude PreToolUse
     hookSpecificOutput(permissionDecision=deny + reason) JSON을
     출력한다. Codex가 동형 JSON을 수용하는지(G1은 훅 시스템 동형까지
     확인, 차단 필드는 미관측)를 G4가 판정하고 결정표(§8)에 따른다.
- **deny 조건·순서 무수정 승계(D25·D32·D39)**: ① 어휘 게이트의 정적
  단일파일 덤프 증명 → ② 절대경로 존재·정규 파일·경계 내·임계
  초과(기본 256KiB, `CTR_GUARD_READ_MAX`) → ③ denylist 정적
  대조(걸리면 deny 아닌 allow+미색인) → ④ 현장 색인 `Indexed==1` →
  전건 성립 시에만 deny + 대체 안내(상대경로·절대경로 비포함 양식).
  하나라도 실패하면 allow — 복구 불능 상태 금지.
- **이벤트·계측**: deny 시 warning 이벤트 + 현장 색인 artifact(cc:
  동형)를 cx: 세션에 기록. PostToolUse shadow 경로는 현행 그대로(이미
  cx: 합류 완료 — v0.4 행 1). tool_call 중복 계상 없음(PreToolUse는
  가드 몫 — hook.go:150 주석 계약 유지).
- **구버전 호환 창**: "신 hooks.json + 구 바이너리(≤0.6.x)" 창에서 구
  codex-hook도 PreToolUse를 인식해 guardBash(bashDumpArg 단독)를
  실행하나, raw PS 구문(Get-Content 등)은 bashDumpArg에 절대 매치하지
  않으므로 이 창의 deny 표면은 사실상 0(§7 실측 — cat류 sh 구문만
  이론상 대상)이고 fail-open 계약이 유지된다. 업그레이드 순서(바이너리
  먼저, v0.4 §2)는 그대로 권고.
- **격리**: 동일 UUID cc/cx 오귀속 금지 테스트에 PreToolUse 가드
  경로를 추가(cx: deny가 cc: 세션에 이벤트를 남기지 않는 방향 포함).

## 3. 설치 상세 (D48)

- **hooks.json**: PreToolUse 그룹(matcher `Bash`, command
  `context-router codex-hook`, statusMessage 마커 탑재) 추가. 기존
  JSON 병합기 재사용 — F4 전건 소유 판정(모든 항목이 command 토큰 정확
  일치 AND 마커 접두일 때만 자기 그룹, 혼합 그룹 불가침), 동일 버전
  바이트 멱등. **정의 변경이므로 사용자가 Codex `/hooks`에서 재신뢰해야
  실행된다** — 설치기 안내 1줄(G3 승계).
- **config.toml 관리 블록**: 형식 —

  ```toml
  # BEGIN context-router
  [mcp_servers.ctr]
  command = "context-router"
  args = []
  enabled_tools = ["ctr_search", "ctr_fetch", "ctr_transform", "ctr_record_event", "ctr_session_summary", "ctr_export_events"]
  # ingest/net 활성화 시 권장: default_tools_approval_mode = "prompt"
  # END context-router
  ```

  (블록 내용 = doctor 스니펫과 동일 — 승인 모드 권장 주석 포함, 적대
  검수 Minor-5 교정.)

  - 병합 알고리즘: 파일을 바이트 스팬으로 읽고(개행 분해 시 CRLF
    보존) → BEGIN/END 마커 쌍을 찾아 블록 교체(부재 시 말미 추가 —
    앞에 구분 빈 줄 1개, 원본이 개행 없이 끝나면 EOF 개행 1개 선행
    추가[유일 허용 블록 밖 변경, D48]) → 임시 파일 기록 후
    rename(원자). 마커 판정은 **고정 문자열 정확 라인 매치**(`# BEGIN
    context-router` / `# END context-router` — 자유 접미 없음, 버전
    문자열 없음: 버전 간 마커 호환. 2패스 Critical 교정 §10) + 소유
    인정에는 **본문 검증**(블록 내부 `[mcp_servers.ctr]` 헤더 라인
    실존) 추가. **양쪽 마커 부재=정상 append**, 그 외 무결성·소유권
    이상(END 단독·중복 쌍·역순·END 부재·본문 불일치)은 전부
    **무변경+안내**(파손 확대 금지, 실패 시 원본 무변경 원칙). 한계
    명문: multiline string 안에 정확 마커 1쌍+관리 본문이 공존하는
    이론상 케이스는 오인 가능(희귀 — 소유권 검증이 그 외 오인을
    걸러냄).
  - 충돌 검사(D48 키-경계 보수 스캔): 블록 밖 내용에 `mcp_servers`
    존재 AND ctr 키-경계 신호(`ctr]`·`ctr"`·`ctr'`·`ctr=`·행두
    `ctr.`·`.ctr.` — 공백 제거 라인 정규화) 존재 시 기입 생략+안내.
    오탐의
    실패 방향은 설치 거부+수동 안내(doctor 스니펫 + 기존 등록의
    command 경로를 doctor [10]과 대조하라는 스테일 감사 권고 1줄)로
    안전 — 미탐지로 인한 중복 정의 파스 에러(사용자 Codex 전체 파손)
    방향을 구조적으로 봉쇄.
  - **설치 결합 순서**(§0 D47): config.toml 병합을 먼저 수행하고, MCP
    대체 경로 확정(기입/갱신 성공 또는 블록 밖 정확
    `[mcp_servers.ctr]` 헤더 실존) 시에만 hooks.json에 PreToolUse
    그룹을 포함해 병합한다. 미확정이면 캐프처 그룹(SessionStart+
    PostToolUse)만 갱신 + 보류 안내 — 수동 등록 후 재실행이 복구
    경로.
  - 제거 대칭: uninstall 시 블록(마커 라인 포함)을 삭제하고, 선행
    구분 빈 줄은 **직전 라인이 빈 줄일 때만 그 1줄만** 함께
    삭제(2패스 Minor — 블록이 파일 중간으로 이동된 경우 사용자
    내용 오삭제 방지). 블록 부재 시 무변경. 왕복
    `f_uninstall(f_install(x))`은 EOF 개행 정규화를 제외하고 바이트
    동일, 동일 버전 `f(f(x))==f(x)` 바이트 멱등 테스트(D48).
  - `[hooks.state]` 등 블록 밖 내용은 바이트 그대로 보존(구조적 보증).
  - MCP 등록은 훅 신뢰와 무관하게 Codex 재시작 시 반영 — 안내 1줄.
- **doctor**: 신규 검사 항목은 두지 않는다 — 기존 host adapter 스니펫
  출력이 수동 대조 표면(YAGNI, 필요 시 후속).

## 4. D49 설계 (구현 이월 — 착수 조건: D46 경고 실발화)

§0 D49 참조. 본 버전에서는 설계 계약만 확정하고 코드 변경 없음.

## 5. A/B 측정 해석 주석 (D45 연계)

- PreToolUse 가드 등록은 cx: hooks:on arm의 **treatment 정의를
  변경**한다(캐프처 전용 → 캐프처+가드+MCP 재등록). `usage --compare`
  cx: 블록 해석에는 이 경계를 병기한다 — 기존 코호트 라벨(대화형
  (경량) 한정) 유지, 경계 전후 혼합 비교 금지 주석. 경계의 실체는
  "v0.7 배포 시점" 전역선이 아니라 **세션 단위 설치 시점**이며, 모든
  세션 이벤트에 이미 각인된 `Producer="context-router/<version>"`이
  기계적 경계 근거를 제공한다(2패스 Minor-9 — v0.7 범위에서는 수동
  주석으로 족하고, Producer 기반 자동 경계 표기는 후속 측정 도구
  후보로 §9에 기재). cc: 축은 영향 없음.
- MCP 재등록으로 Codex 세션이 ctr_search/ctr_fetch를 사용하게 되면
  cx: 이벤트 구성(tool_call 분포)도 변한다 — §7 분해가 재등록 전
  기준선이 된다.

## 6. 검증·테스트 계약

- 게이트 선택(호스트×GOOS 블랙박스 4계약 + 회귀 핀 — **픽스처
  tool_name은 전부 `"Bash"` 고정**: Codex exec 표면이며, PowerShell로
  두면 기존 guardPowerShell 경로로 자명 통과해 신설 라우팅이 미검증
  — 2패스 Minor-7): ① Windows-cx `Get-Content C:\…\big.txt`(임계
  초과) → deny, ② Windows-cx `cat /c/big.log` → **allow**(psAbsPath
  드라이브 상대 — MSYS 오파일 deny 재도입 금지 핀, §0 D47), ③
  비Windows-cx `cat /abs/big` → deny, ④ 양쪽 미매치 → allow. cc:
  현행 무변경 회귀 핀(`cat /c/big.log` MSYS deny 유지 포함). 부분
  읽기 플래그·파이프·상대경로 allow 케이스는 기존 표(bashDumpArg
  16행·psDumpArg 표) 재사용.
- deny 경로: cx: deny JSON 출력 + warning 이벤트 + 현장 색인 artifact
  + denylist 걸림 시 allow+미색인(D39). G4 판정 결과에 따른 출력 계약
  픽스처.
- 격리: 동일 UUID cc/cx — PreToolUse 가드 경로 확장(§2).
- TOML 병합 계약: 멱등(바이트)·블록 밖 보존(주석·미지 키·
  [hooks.state] 포함 픽스처)·**충돌 생략 4형**(quoted key·인라인
  테이블·부모 테이블+점표기·루트 완전-점표기)·**부분열 오탐
  회피**(`electron` 언급
  픽스처 → 정상 설치)·**마커 소유권**(사용자 유사 마커 블록·본문
  불일치 → 무변경)·제거 대칭+왕복(EOF 개행 제외 바이트 동일, 중간
  블록의 선행 비-빈줄 보존)·원자성(실패 시 원본 무변경)·**마커
  무결성 4형**(END 단독·중복 쌍·역순·END 부재 무변경+안내)·CRLF
  보존·무개행 EOF 파일 append·**설치 결합**(MCP 미확정 시 PreToolUse
  그룹 미등록+캐프처 유지).
- hooks.json: PreToolUse 추가 멱등·F4 전건 판정 회귀.
- CI 3-OS, `go test -p 1`, §12 canary(비밀 리터럴 분해) 유지.

## 7. 관측 실측 기록 (2026-07-23, 브레인스토밍 세션 — 컨트롤러 수행)

- **v0.6 §7 교정(추기)**: "cx: tool_call 148건 전부 'Bash: …' 형태"는
  부정확 — session.db 실측 분해: **Bash(exec) 48건**(Get-Content 22·
  rg 14·`<arg>` 8·echo 3·Test-Path 1) + **MCP 도구 95건**
  (mcp__mcp__ctx_execute_file 63·ctx_execute 23·ctx_batch_execute 5·
  ctx_search 4 — Codex 세션에 ctxscribe 부착 상태) + **update_plan
  5건** = 148. summary는 최대 27바이트 절단이라 첫 토큰 수준 분해만
  가능(전체 명령 미저장 — payload None).
- 가드 실효성 실증: 셸 명령의 46%(22/48)가 Get-Content — Claude 채널과
  동일한 덤프 성향. 단 관찰된 사용은 부분 읽기(-TotalCount·파이프)
  다수로 추정되어 실제 deny 발화율은 낮을 수 있음(정직한 기대치 —
  가드는 전량 덤프만 잡는다).
- 명령 구문: pwsh 래핑 토큰 0건 — Codex companion 로그의
  `pwsh.exe -Command` 표시는 실행 계층이고, tool_input.command는 raw
  구문(Windows=PS 구문·Get-Content/Test-Path 실재)이 hook에 도달한다.
- 도그푸딩 스냅샷(v0.6.1 직후): [6] sessions=99(empty=72 — 67→69→72
  증가, 7일 GC 회수 창 미도래), [12] drops=325 불변, [14] blob=25.6MB·
  file=94.3MB(임계 100MiB의 89.9% — D46 미발화), [15] cc:=18.6MB·
  cx:=768KB.
- cc: 관찰 A/B 2점째: output/rec 0.866·cache_read/rec 0.643 —
  session-21 첫 신호(0.60·0.63)와 일관.

## 8. 관측 프리체크 게이트 (계획 Task 0 — 컨트롤러 수행·판정, §11 관례)

- **G4 — Codex PreToolUse 표면 판정(순서 고정 절차 — 2패스 교정:
  독립 관측의 비배타 행 분류·"수용≠강제" 혼동·payload 미관측을
  해소)**: 공식 훅 문서(developers.openai.com/codex/hooks) +
  실호스트 스크래치 관측. **관측 항목 4**: (a) PreToolUse 등록
  수용(hooks.json 이벤트 키 인식·발화), (b) **payload 운반** —
  PreToolUse stdin에 `tool_name`+`tool_input.command`가 실리는지(§7
  148건은 전부 PostToolUse 유래라 PreToolUse payload는 데이터 0 —
  미운반이면 게이트 입력이 공문자열이라 가드가 조용히 무력), (c)
  **차단 강제** — deny 출력 시 도구 실행이 실제로 차단되는지(수용≠
  강제: 자문-deny[경고 후 그대로 실행]는 불합격 — 덤프가 컨텍스트에
  진입해 가드 목적 자체 실패), (d) matcher `Bash` 필터 존중(독립
  관측 — 미존중=전 도구 발화는 무해[dispatch가 Read/Bash/PowerShell
  외 no-op], 기록만·행 판정 비관여). **판정 절차(순서 고정 — 유일
  결정)**: ① (a) 불수용 → 행 3. ② (b) 미운반 → 행 3(가드 무력·
  보고). ③ (c) 강제 실증 실패(자문-deny 포함) → 행 3. ④ 강제 실증
  성공 — Claude 동형 JSON이면 행 1, 비동형 차단 표면(필드명 상이·
  exit code 기반 등)이면 행 2. 문서화 부재는 그 자체로 중단 사유가
  아니다 — 실증 우선 + experimental 기록(D44 선례), 단 실증 불가·
  버전 간 불안정 관측이면 행 3. 결정표:

  | 행 | 판정(위 절차) | 구현 |
  |----|----|----|
  | 1 | 강제 실증 + Claude 동형 JSON | 현행 deny 출력 무수정 — 전체 이식 |
  | 2 | 강제 실증 + 비동형 차단 표면 | codex-hook에서 host별 deny **출력 직렬화만** 매핑(공유 가드 판정 로직 무변경) |
  | 3 | 등록 미수용·payload 미운반·강제 실증 실패·표면 불안정 | 가드 축 중단·보고(설계 개정 없이 재상정 — v0.4 행 5 관례). D48 MCP 등록은 독립 진행 |

- **G5 — 종결(설계 시점 실코드 확인)**: dispatch는 호스트 무관
  PreToolUse 분기를 이미 가지며(hook.go:153), 구 바이너리 창의 deny
  표면은 bashDumpArg 단독이라 raw PS 구문에 매치 불가 — 위험 낮음(§2).
- **G6 — 사용자 config.toml 실물 관측**: 기존 `[mcp_servers]` 항목·
  `[hooks.state]` 배치·주석 실태를 관리 블록 픽스처에 반영(Task 0).

## 9. 의도적 미결 (v0.8+ 후보)

D49 구현(D46 실발화 후), 무작위 A/B 하네스·OTel(D27), exec 3종(D21
트랙), 서브에이전트 캡처(호스트 표면 필요), register-on-first-event
(D43), Grep 도구 가드, plugin manifest, semantic 보강, spill journal,
`repository{}` 기입, `invalidates`, doctor의 Codex MCP 등록 상태 검사
(D48 사후 필요 실증 시), Producer 버전 기반 A/B treatment 자동 경계
표기(§5 — 세션 단위 기계 경계, 수동 주석 대체 후보).

**v0.7 최종 이중 리뷰 파생 잔존 리스크 3건** (2026-07-23, §10.4 —
전부 수동 개입·부분 손상을 전제하는 협소 경로, 복구 실존, 코드
무변경 결정. v0.7.x에서 사용자 판단으로 재상정 가능):

1. **다운그레이드-재설치 스테일 가드**(서브 I + Codex P1 수렴):
   설치 1이 MCP 확정·가드 등록 → 사용자가 config.toml을 수동
   훼손(conflict/marker 이상) → 설치 2가 `withGuard=false`로
   PreToolUse를 순회하지 않아 기존 가드 잔존. 스펙 Produces 공식
   (`withGuard || !install`)의 귀결. **해법 양론**: 서브=현상 유지
   +문서화(conflict 대부분이 의미상 유효한 수동 MCP 등록이라 가드
   잔존이 정상 — 자가치유는 정당한 가드를 박탈하는 회귀) /
   Codex=자가치유(미확정 설치 시 가드 제거 — 블록 삭제 후 재설치
   시나리오는 MCP 실부재). 문서화 채택, 자가치유는 사용자 결정
   대기. 복구: `hook uninstall --codex`(가드 무조건 소거) 또는
   config 정상화 후 재설치.
2. **전역 블록 vs 프로젝트 훅 수명 불일치**(Codex P1 부분 채택):
   MCP 블록은 `$CODEX_HOME/config.toml` 전역 단일, 훅은 프로젝트
   스코프 가능 — 프로젝트 A/B 설치 후 한쪽 uninstall이 공유 블록을
   제거하면 타 프로젝트 가드만 잔존. 크로스 프로젝트 탐지는 구조상
   불가(타 프로젝트 `.codex/` 열거 불능)라 uninstall 안내 메시지에
   재설치 힌트 보강으로 완화(재설치가 블록을 멱등 재기입). 
3. **라인 기반 스캔의 문자열 컨텍스트 무추적**(Codex P2 문서화):
   멀티라인 문자열 안에 정확한 `[mcp_servers.ctr]` 라인이 들어
   있으면 mcpExistingHeader 오확정(가드 등록·MCP 실부재) 이론
   가능. TOML 파서 비도입(D48) 설계의 명문 한계 — 발생 개연성
   극저(설정 파일 내 문서용 멀티라인 문자열 필요), 관측 시 재상정.

## 10. 적대 검수 처리 기록 (2026-07-23, 설계 체크포인트)

### §10.1 1패스 처리 기록 (초안 2b9c05c 대상)

- 이중 적대 검수(초안 2b9c05c 대상): 서브에이전트(opus)
  FIX-REQUIRED(Important 3·Minor 5) + Codex adversarial-review
  needs-attention(high 2). **수렴 2건**이 구조 교정을 강제:
  - **게이트 직렬 폐기 → 호스트×GOOS 단독 게이트**(D47): 게이트=술어+
    경로 해석의 완결 정책. 반례 쌍 — cx:Windows `cat /c/big.log`에서
    bash 레그 MSYS 변환의 오파일 deny(v0.4 §11.1 파생② 위반,
    hook_test.go 1450/1506 계약 명문) vs 경로 통일 시 cc: MSYS deny
    회귀. 초안 §2("채택")·§6("3분기")의 직렬 의미 모순도 함께 해소.
    host는 Run→dispatch→가드 명시 전달(현재 세션 접두로만 진입,
    헬퍼 HostClaude 하드코딩 실측 — Codex 검수).
  - **충돌 스캔 보수 광역화**(D48): 헤더 라인 스캔은 quoted key·
    인라인 테이블·부모 테이블+점표기(합법 TOML)를 놓쳐 중복 정의
    파스 에러(사용자 Codex 전체 파손 — 설계가 봉쇄를 표방한 바로 그
    방향)를 허용. `mcp_servers`+`ctr` 동시 등장 시 형태 불문
    생략+안내로 교체(오탐=안전 방향). 검증 전용 TOML 파싱은
    D28(round-trip 보존) 반론이 적용되지 않는 별개 선택지임을
    명시하되(서브에이전트 — 초안이 "파서 사용"과 "파서 round-trip"을
    뭉뚱그림) 보수 스캔이 의존 0으로 동일 봉쇄를 달성해 비채택.
- **서브에이전트 고유 반영**: Important-3(D47 확정 어조 → G4 게이트
  조건화 명문), Minor-4(G4 관측 항목에 PreToolUse 등록 수용·matcher
  필터 존중 추가, 행 2 라벨 확대), Minor-5(블록에 승인 모드 권장
  주석 추가 — doctor 스니펫 동일성 복원), Minor-6/7(EOF 개행·선행
  빈 줄 왕복 계약 명문, 마커 무결성 4형 무변경+안내), Minor-8(D47
  "무수정"을 판정 로직 한정으로 정밀화 — 출력 직렬화는 G4 행 2 매핑
  허용).
- **Codex 고유 반영**: CRLF·EOF 개행 바이트 보존 명문(멱등 주장
  전제), multiline string 내 마커 오인 한계 명문(마커 무결성 규칙이
  그 외를 걸러냄), 교차 호스트·경로 블랙박스 테스트 4계약(§6).
- 실코드 대조 결과(서브에이전트): dispatch 공유 경로·codex-hook→
  HostCodex 전달·deny JSON 형태(hookSpecificOutput/permissionDecision)
  주장 전부 부합, go.mod TOML 의존 0 확인.

### §10.2 2패스 처리 기록 (2026-07-23, 사용자 지시 — 585dcaa·0772e2a 대상)

- 이중 적대 검수 2패스(신선 컨텍스트 서브에이전트 + Codex): 서브
  FIX-REQUIRED(Important 3·Minor 8) + Codex needs-attention(critical
  1·high 3). **1패스 교정 자체의 결함 3건이 적발되어 재교정**:
  - **마커 소유권**(Codex Critical): 접두 자유 텍스트+개수 검사는
    소유권을 증명하지 못해 사용자 주석 블록(`# BEGIN context-router
    migration` 등)을 오소유·교체·삭제 — 고정 문자열 정확 라인 매치 +
    본문 검증(`[mcp_servers.ctr]` 실존)으로 교체, 불일치=무변경+안내.
  - **부분열 오탐**(서브 Imp-1, 실행 검증): 1패스의 무경계 동시-등장
    스캔은 공백 제거로 토큰 경계가 소멸해 `electron`·`spectra`의
    `ctr` 부분열에 오탐(영구 생략) — 키-경계 신호 스캔(`ctr]`·
    `ctr"`·`ctr'`·`ctr=`·행두 `ctr.`)으로 교체. 합법 변형 전부 경계
    신호 동반 확인(실행 검증). 도그푸딩 config.toml 실물은 `ctr`
    부분열 0회로 무발동 확인(무력화 가설 반증).
  - **설치 결합 공백**(수렴 — 서브 Imp-2·Codex high): TOML 병합
    생략(오탐 포함)과 hooks.json PreToolUse 등록이 독립 수행되면
    "deny + 안내 도구 부재"(D32 위반 — 설계가 봉쇄를 표방한 조합)
    발생 — MCP 확정 시에만 PreToolUse 등록하는 결합 순서 신설.
- **G4 재작성**(수렴 — 서브 Imp-3/Min-4·Codex high): 독립 관측의
  비배타 행 분류(행 1·3 동시 성립 조합 존재) + "deny 수용≠강제"
  혼동(자문-deny=거짓 행 1) + PreToolUse payload 운반(tool_input.
  command) 완전 미관측(§7 148건은 전부 PostToolUse 유래)을 해소 —
  관측 4항목·순서 고정 판정 절차로 교체.
- **한계 명문·기각**: 셸 방언 신호 기반 게이트 선택(Codex 권고)은
  hook 입력에 신호 부재로 구현 불가 기각 — 반대 방언 덤프
  miss=allow를 by-design 한계로 §2 명문(v0.4 비ASCII 선례 동급),
  관측 시 재상정.
- **Minor 반영**: §1.1 잔존 "직렬" 어구·G4 조건부 병기, §6 픽스처
  tool_name="Bash" 고정, both-absent=append 명문, uninstall 선행
  빈 줄 가드(직전 라인이 빈 줄일 때만), §5 Producer 세션 단위 기계
  경계 명시(+§9 자동 표기 후보), "임계 100MiB의 89.9%" 표기 정밀화.
- 독립 재검증(서브): 게이트 4계약 실코드 성립(`go test
  ./internal/hook/... -p 1` GREEN), 합법 변형 스캔 포착 실행
  확인, doctor 스니펫=블록 내용 일치, hook.go:153·GOOS 무분기·접두
  전달 주장 정확.
- **재검수(8526d49 대상, 서브 단독): SHIP** — Imp 3+Codex crit/high
  전부 폐쇄, G4 전순서 유일 결정 확인, 신규 모순 없음. 잔존 Minor
  1건 즉시 반영: 루트 완전-점표기(`mcp_servers.ctr.command=…`)가
  신호 5종을 회피해 중복 정의 파스 에러 방향 miss — `.ctr.` 신호
  추가(실행 검증: 표기 관례 변형 포착 유지·부분열 오탐 없음),
  "합법 변형 전부" 과장을 "표기 관례 변형"으로 완화.

### §10.3 관측 프리체크 결과·판정 (Task 0)

2026-07-23, 컨트롤러 수행. codex-cli **0.144.6**, 스크래치
`CODEX_HOME=C:\tmp\ctr-g4\codex-home`(auth만 복사 — 사용자 실환경
무접촉), 워크스페이스 `C:\tmp\ctr-g4\ws`(git init·trusted 기입).
실증 아티팩트: `C:\tmp\ctr-g4\{payload,deny-payload}.jsonl`.

**G4 관측** (§8 관측 a~d):

- **(a) 공식 문서**(developers.openai.com/codex/hooks): 등록은
  `hooks.json` 또는 config.toml 인라인 `[hooks]` — 이벤트→matcher
  그룹→command 핸들러의 Claude 동형 3계층, 셸 명령은 matcher
  `Bash`로 매치(unified exec 포함). PreToolUse 입력에
  `tool_name`·`tool_input`(Bash는 `tool_input.command`) 명문. 차단
  응답은 **Claude 동형**(`hookSpecificOutput.permissionDecision:
  "deny"`+`permissionDecisionReason`)을 공식 수용, 구형
  `{"decision":"block"}`·exit 2+stderr도 병행 수용. 비관리 훅은
  `/hooks` 리뷰·신뢰(정의 해시 기록, 변경 시 재신뢰) 필수 —
  `--dangerously-bypass-hook-trust`로 1회성 우회 가능(스크래치
  실증의 거짓 행 3 방지에 사용).
- **(b) payload 운반 ✓**: user-layer hooks.json PreToolUse(matcher
  `Bash`) 캡처 훅으로 codex exec 1회 — stdin payload에
  `tool_name:"Bash"`, `tool_input.command:"Set-Content -Path
  ran1.txt -Value executed"`(**raw PS 구문 그대로** — §7 실측 재확증)
  운반. Claude 동형 필드(session_id·cwd·hook_event_name·
  tool_use_id) + Codex 확장(turn_id·transcript_path·model·
  permission_mode).
- **(c) 차단 강제 ✓**: baseline(sandbox 해제) 동일 명령 실행
  성공(`ran1.txt` 생성) ↔ 훅을 Claude 동형 deny JSON 출력으로 교체
  후 동일 조건에서 `hook: PreToolUse Blocked` +
  `Command blocked by PreToolUse hook: test`(reason 모델 노출) +
  부작용 파일(`proof.txt`) **미생성** — 자문-deny 아닌 실행 차단.
  부수 관측: read-only sandbox에서는 router 정책이 훅 판정과
  별개로 쓰기 명령을 자체 거부(declined) — 훅 발화 자체는 sandbox
  거부와 무관하게 유지.
- **(d) matcher 존중 ✓**(기록만): 비셸 런(텍스트 응답만)에서 훅
  미발화, 캡처된 전 payload가 `tool_name:"Bash"`.

**G4 판정 — 행 1** (§8 순서 고정: ① 등록 수용 ✓ → ② 운반 ✓ → ③
강제 ✓ → ④ Claude 동형 JSON 수용): 가드 이식은 denyTool 출력
무변경으로 진행. **Task 1 Step 8(행 2 분기) 비활성**, T1~T4 계획
원안 그대로.

**G6 관측** (사용자 실물 `C:\Users\js\.codex\config.toml` —
`CODEX_HOME` env 미설정 확인): 17,060B·508라인·**LF 지배(CRLF
0)**·EOF 개행 유·BOM 무·주석 라인 0. `[mcp_servers.*]` 6종
(L319–362: openaiDeveloperDocs·node_repl(+env)·context7·github·
chrome-devtools(+env)), `[hooks.state]` L33~(트러스트 해시 — 본
저장소 `.codex/hooks.json`의 post_tool_use·session_start 포함).
관리 블록 마커 0개, **ctr 키-경계 신호 0**(2패스 §10.2 관측
재확인). 플러그인 스코프
`plugins."ctxscribe@wotjr1649".mcp_servers.mcp`가 `mcp_servers`
부분열을 보유하나 키-경계 신호 비해당 — §3 (b) AND 조건 미충족으로
무발동(오탐 회피 설계의 실물 확증). 예상 설치 경로: append →
`mcpWritten`, LF 블록 기입.

### §10.4 최종 이중 리뷰 처리 기록 (2026-07-23, 구현 체크포인트 —
브랜치 0632630..4a002bc 대상)

- 서브(opus whole-branch): **With fixes** — C0/I1/M3. 크로스파일
  정합 완전 확인(관리 블록 `enabled_tools` = doctor 스니펫 = 무조건
  등록 MCP 6도구 일치, matcher `Bash` ↔ dispatch ↔ deny JSON 이음새
  일관, cc: 하위 호환 유지).
- Codex(review --base): P1×3·P2×2.
- **병합 판정**:
  - **Codex P1-1 채택(코드)**: 루트 인라인 `mcp_servers = {…}`(ctr
    신호 무)가 §3 (b) AND 조건을 회피해 append → 인라인 테이블
    자기완결 규칙으로 `[mcp_servers.ctr]` 확장이 **파스 에러**(tomllib
    실검증) — D48이 봉쇄를 표방한 방향의 계약 자체 갭. 루트 할당
    정규화 프리픽스(`mcp_servers=`·quoted 변형) 단독 충돌 신호로
    §3 (b) 개정. 헤더 정의(`[mcp_servers]`) + 타 서버는 확장 합법
    이므로 비충돌 유지(회귀 핀 추가).
  - **Codex P1-2 = 서브 I 수렴**(교차 확증): 스테일 가드 — 해법
    상충(문서화 vs 자가치유)으로 §9-1 양론 기록, 코드 무변경,
    사용자 결정 이월.
  - **Codex P1-3 부분 채택**: 전역 블록 수명 — uninstall 안내 보강
    +§9-2 문서화(구조 수정은 크로스 프로젝트 탐지 불능으로 기각).
  - **Codex P2-1 채택(코드)**: uninstall이 hooks.json 부재 시 config
    블록 정리 스킵(부분 설치 잔재 영구화) — 부재=빈 집합 취급, 정리
    지속.
  - **Codex P2-2 문서화**: 문자열 내 헤더 오확정 — §9-3 명문 한계.
  - **서브 M 채택**: doctor 스니펫 주석 "3-도구" 스테일 정정(6도구).
    서브 M 잔여 2건(cx+win 토큰 재파싱 — psDumpArg 계약 연동 주의
    주석 수준, mergeCodexHooks 72줄 — 형제 관례 수용)은 무조치 기록.
- fix 웨이브 1회(코드 4건) 후 재리뷰는 서브 단독(Codex 체크포인트
  1패스 소진 규약).
