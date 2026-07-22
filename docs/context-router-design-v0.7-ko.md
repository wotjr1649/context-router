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
  신설**한다(가드 헬퍼의 HostClaude 하드코딩 제거 포함). 게이트
  선택의 실측 근거는 §7(Windows Codex exec = raw PS 구문 지배).
  deny 사유의 ctr_search/ctr_fetch 안내는 D48(MCP 재등록)로
  실효화된다 — 안내된 도구가 없는 "차단+복구 불능" 조합 금지(D32
  원칙)가 범위 결합의 근거. deny 시 warning 이벤트는 cc: 동형으로
  cx: session.db에 기록(실발화 관측 표면). matcher는 `Bash`
  단독(Codex 표면에 Read·PowerShell tool_name 부재 — §7 실측 148건
  정합). **본 축 전체가 G4에 게이트된다** — 행 3(차단 표면 부재) 판정
  시 가드 축은 중단·보고하고 D48은 독립 진행한다(§8, 적대 검수
  Important-3 교정: 확정 어조 금지).
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
  만족하는 최소 구현이다. 충돌 규칙(적대 검수 1패스 교정 §10 —
  초안의 헤더 라인 스캔은 quoted key `[mcp_servers."ctr"]`·인라인
  테이블 `mcp_servers.ctr = {…}`·부모 테이블+점표기 등 합법 변형을
  놓쳐 중복 정의 파스 에러로 사용자 Codex 전체를 깨뜨릴 수 있어
  폐기): **보수 광역 스캔** — 블록 밖 내용의 공백 제거 정규화
  문자열에 `mcp_servers`와 `ctr` 토큰이 **동시 등장하면 표기 형태
  불문 기입 생략+안내 1줄**. 합법 변형 전부가 그물에 걸리고, 오탐
  (주석 언급 등)의 실패 방향은 "설치 거부+수동 안내"라 안전하다(설치
  거부≠파손 — F4 불가침 우선). TOML 파서 의존은 계속 비도입 —
  go.mod에 TOML 의존 0(실측), 검증 전용 파싱은 D28 반론(round-trip
  보존)이 적용되지 않는 별개 선택지이나(§10) 보수 스캔이 의존 0으로
  같은 봉쇄를 달성한다. **마커 무결성**: BEGIN/END가 정확히 1쌍·
  정순이 아니면(END 단독·중복 쌍·역순·END 부재) 전부 **무변경+안내**
  (multiline string 내 정확 1쌍 마커 오인은 이론상 잔존 — 희귀 한계
  명문, §3). **개행 계약**: 병합은 바이트 스팬 연산으로 원본 개행
  형식(CRLF 포함)을 보존하고, 원본이 개행 없이 끝나면 append 경로가
  EOF 개행 1개를 추가한다(유일하게 허용되는 블록 밖 바이트 변경 —
  명문 한계). 왕복 `f_uninstall(f_install(x))`은 그 EOF 개행을
  제외하고 바이트 동일(제거=블록+설치가 추가한 선행 구분 빈 줄 1개
  삭제), 동일 버전 `f(f(x))==f(x)` 바이트 멱등, 블록 부재 시
  uninstall 무변경. `[hooks.state]`(Codex 소유 trust-hash) 불가침
  유지.
- **D49** content.db 파일 축 회수 — **설계 선행·구현 이월**: 경로는
  `purge` 계열 `--vacuum` 플래그(행 삭제 후 VACUUM 실행) 후보로
  확정하되 구현 착수 조건은 **D46 경고 실발화**(현재 89.9%). 제약
  명문화: VACUUM은 라이브 MCP 서버 가동 중 불가(파일 잠금 — 세션 락
  표면으로 사전 감지 가능), 자동 실행 없음(수동 트리거 일관 원칙),
  purge 행 삭제 후에도 free page라 VACUUM 전까지 파일 크기 불변(D46
  경고 문구가 이미 안내). 상세 계약(옵션 이름·잠금 검사·진행 보고)은
  발화 후 버전에서 확정한다.

## 1. v0.7 제품 계약

### 1.1 범위

- cx: PreToolUse 덤프 가드 — 호스트별 게이트 선택 + 직렬 적용(§2).
- `hook install --codex` 확장 — PreToolUse 훅 등록 + config.toml 관리
  블록 MCP 병합(§3).
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
     명시 파라미터로 전달(현재는 세션 접두 문자열에만 반영 — 가드
     헬퍼의 HostClaude 하드코딩 제거 포함)하고, `cx:`+Windows이면
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
     호스트 2개에 과잉 — 접근 A 채택).
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
  # BEGIN context-router (managed — 이 블록은 hook install --codex가 소유)
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
    rename(원자). 마커 판정은 **라인 접두 정확 매치**(`# BEGIN
    context-router` / `# END context-router` — 접두 이후는 설명 자유
    텍스트, 버전 문자열 없음: 버전 간 마커 호환). **마커 무결성
    이상은 전부 무변경+안내**(D48 — END 단독·중복 쌍·역순·END 부재.
    파손 확대 금지, 실패 시 원본 무변경 원칙). 한계 명문: multiline
    string 안에 정확 1쌍 마커가 존재하는 이론상 케이스는 오인
    가능(희귀 — 마커 무결성 규칙이 그 외 오인을 걸러냄).
  - 충돌 검사(D48 보수 광역 스캔): 블록 밖 내용을 공백 제거 정규화한
    문자열에 `mcp_servers`와 `ctr`가 동시 등장하면 표기 형태 불문
    기입 생략+안내. 오탐의 실패 방향은 설치 거부+수동 안내(doctor
    스니펫)로 안전 — 미탐지로 인한 중복 정의 파스 에러(사용자 Codex
    전체 파손) 방향을 구조적으로 봉쇄.
  - 제거 대칭: uninstall 시 블록(마커 라인 포함)+설치가 추가한 선행
    구분 빈 줄 1개를 삭제, 블록 부재 시 무변경. 왕복
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
  변경**한다(캐프처 전용 → 캐프처+가드+MCP 재등록). v0.7 배포 시점
  이후의 `usage --compare` cx: 블록 해석에는 이 경계를 병기한다 —
  기존 코호트 라벨(대화형(경량) 한정) 유지, 경계 전후 혼합 비교 금지
  주석. cc: 축은 영향 없음.
- MCP 재등록으로 Codex 세션이 ctr_search/ctr_fetch를 사용하게 되면
  cx: 이벤트 구성(tool_call 분포)도 변한다 — §7 분해가 재등록 전
  기준선이 된다.

## 6. 검증·테스트 계약

- 게이트 선택(호스트×GOOS 블랙박스 4계약 + 회귀 핀): ① Windows-cx
  `Get-Content C:\…\big.txt`(임계 초과) → deny, ② Windows-cx
  `cat /c/big.log` → **allow**(psAbsPath 드라이브 상대 — MSYS 오파일
  deny 재도입 금지 핀, §0 D47), ③ 비Windows-cx `cat /abs/big` →
  deny, ④ 양쪽 미매치 → allow. cc: 현행 무변경 회귀 핀(`cat
  /c/big.log` MSYS deny 유지 포함). 부분 읽기 플래그·파이프·상대경로
  allow 케이스는 기존 표(bashDumpArg 16행·psDumpArg 표) 재사용.
- deny 경로: cx: deny JSON 출력 + warning 이벤트 + 현장 색인 artifact
  + denylist 걸림 시 allow+미색인(D39). G4 판정 결과에 따른 출력 계약
  픽스처.
- 격리: 동일 UUID cc/cx — PreToolUse 가드 경로 확장(§2).
- TOML 병합 계약: 멱등(바이트)·블록 밖 보존(주석·미지 키·
  [hooks.state] 포함 픽스처)·**충돌 생략 3형**(quoted key·인라인
  테이블·부모 테이블+점표기 — 광역 스캔 픽스처)·제거 대칭+왕복
  (EOF 개행 제외 바이트 동일)·원자성(실패 시 원본 무변경)·**마커
  무결성 4형**(END 단독·중복 쌍·역순·END 부재 무변경+안내)·CRLF
  보존·무개행 EOF 파일 append.
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
  file=94.3MB(임계 89.9% — D46 미발화), [15] cc:=18.6MB·cx:=768KB.
- cc: 관찰 A/B 2점째: output/rec 0.866·cache_read/rec 0.643 —
  session-21 첫 신호(0.60·0.63)와 일관.

## 8. 관측 프리체크 게이트 (계획 Task 0 — 컨트롤러 수행·판정, §11 관례)

- **G4 — Codex PreToolUse 차단 응답 계약 + 등록·matcher 표면**: 공식
  훅 문서(developers.openai.com/codex/hooks) + 실호스트 스크래치
  관측으로 판정한다. 관측 항목은 3가지(적대 검수 Minor-4 확장): (a)
  deny 응답 계약, (b) **PreToolUse 등록 자체의 수용 여부**(hooks.json
  이벤트 키 인식 — 미수용이면 가드가 조용히 죽는다: 행 3 처리), (c)
  **matcher `Bash` 필터 존중 여부**(미존중=전 도구 발화는 무해 —
  dispatch가 Read/Bash/PowerShell 외 no-op — 관측 기록만). 결정표:

  | 행 | 관측 | 구현 |
  |----|----|----|
  | 1 | Claude 동형 JSON(permissionDecision) 수용 | 현행 deny 출력 무수정 — 전체 이식 |
  | 2 | 비동형 차단 표면(JSON 필드명 상이·exit code 기반 등) | codex-hook에서 host별 deny **출력 직렬화만** 매핑(공유 가드 판정 로직 무변경) |
  | 3 | 차단 표면 부재·문서화 안 됨·PreToolUse 등록 미수용 | 가드 축 중단·보고(설계 개정 없이 재상정 — v0.4 행 5 관례). D48 MCP 등록은 독립 진행 |

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
(D48 사후 필요 실증 시).

## 10. 적대 검수 처리 기록 (2026-07-23, 설계 체크포인트 — 1패스)

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
