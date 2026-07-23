# Context Router v0.11 설계서 (D58–D62 — exec 2종·OS 격리·러너 6언어·출력 ephemeral·채택 개선)

- 전제: v0.10.0 태그(main 7cbcaa5, PR #22, CI 3-OS GREEN, tag-version
  잡 첫 실관측), 도그푸딩 marker 0.10.0(cc 6이벤트·codex
  project+user), cx 재신뢰·§9-2 블록 생존. 관찰(2026-07-24,
  session-28, §3): doctor [16] codex user 스코프 스테일 해소(양
  스코프 0.10.0), D54 Grep 가드 **matcher·deny 양 경로 라이브 파이어
  실증**(warning 이벤트 `Grep: head_limit=0 content` 실기록), D53
  생애주기 누적 1쌍(추가 spawn 없음 — 파이프라인 검증 상태 유지),
  empty 91건(07-20~23 생성, GC 첫 적격 07-27·피크 07-28), D46
  139.2MB(+22.5MB/일 — 회수는 사용자 수동 창 결정: 전 세션 종료 후
  `purge --hook-only` 직접 실행), error 이벤트 캡처 실동작.
- 스코프 확정 근거: session-28 브레인스토밍 — 사용자 결정 8건: ①
  축=**exec 주력 + 회수 경로 채택 개선 병행**(D57 문면 그대로) ②
  exec 표면=**2종**(execute·execute_file — batch 미도입, 실측 근거
  §3) ③ OS 격리=3 OS 전부 구현 + per-OS 명시 + CI 매트릭스
  실검증(가정 승인) ④ 언어=**6종**(shell/js/ts/python/go/**csharp**
  — C#은 사용자 자문 요청 후 3조건부 확정, SDK 10.0.301 실확인) ⑤
  채택 수단=도구 설명 개정 + 계측 상시화(스니펫 확장·스킬 표면
  제외) ⑥ 접근=**A안 단일 릴리스**(격리 코어→러너→노출·계측) ⑦
  timeout 상한 600→**1800초** 개정 ⑧ TS 감지=bun 전용안 기각 →
  **bun→node 폴백**(버전 게이트 부가).
- 착수 조건 처리: exec의 노출 선행 조건(D3: 실질 격리 + §8.3 실행
  계약 + 호스트 승인)은 본 릴리스가 그 이행 자체다 — 별도 선행
  게이트 없음. `ctr_fetch`/내장 WebFetch 중복 우려는 검증 완료(§3.5
  비혼동 계약 기존재 — 설명 문구 + 가드 테스트
  `TestFetchDescriptionMentionsByteExactNotWebFetch`, 웹 fetch인
  `ctr_fetch_and_index`는 net 프로필 옵트인으로 base 미노출).

## 0. 결정 이력 (v0.11 신규 — D57까지는 v0.10 설계서·이전 델타 체인)

- **D58** exec 표면 2종 + 노출 계약:
  - **`ctr_execute(language, code, timeout_ms?)`** — `language` enum
    6종(§D60), `code` 필수, `timeout_ms` 기본 120,000ms·상한
    **1,800,000ms**(컴파일형 콜드 스타트·장시간 분석 감안, 사용자
    개정). 실행 단위: 호출마다 OS temp 하위 고유 스크래치 디렉터리
    생성 → 스니펫 파일 기록 → 샌드박스 실행 → 즉시 삭제(§D61).
    실행 cwd = **워크트리 루트 고정**(분석 편의), 쓰기 허용 경로 =
    스크래치(§D59 — 경로는 `CTR_SCRATCH` env로 스니펫에 노출),
    stdin 닫음.
  - **`ctr_execute_file(language, path, code)`** — execute의 파일
    입력 변형. `path` 검증(절대 경로·존재·파일) 후 스니펫에
    `CTR_FILE` env로 전달. 나머지 계약 동일(구현 공유).
  - **`ctr_batch_execute` 미도입** — 도구 감사 문서 §2.2 재평가
    소진: 실측(§3) exec 계열 580회 중 30회(5.2%), execute 반복
    호출로 커버. 재도입은 실수요 실증 시(§4).
  - **노출**: base 프로필 기본 노출 + 호스트 ask 게이트(D3 문면
    이행 — ingest/net 선례와 동일하게 서버측 게이트 신설 없음).
    doctor 호스트 어댑터 스니펫 개정: cc `permissions.ask`에 exec
    2종 추가, Codex `enabled_tools`에 2종 추가(+
    `default_tools_approval_mode = "prompt"` 권장 문면 유지).
    **doctor [18] 신설**: 러너 감지 상태 라인(언어별 감지
    결과·버전 게이트 통과 여부 — 예:
    `exec runners: shell=pwsh js=bun ts=bun py=python3 go=ok cs=10.0.301`).
  - 장시간 실행은 호스트 MCP 클라이언트 타임아웃 설정에 따라
    클라이언트측 중단 가능 — 문서화만(서버측 대응 없음). 동시성은
    호스트 직렬 호출 전제(세션별 stdio 서버 프로세스).
- **D59** 격리 계약 — per-OS 명시(vision §7 "하나의 보안 계약으로
  포장 금지" 준수):
  - **공통(전 OS)**: 프로세스 **트리 제어**(타임아웃·취소 시 고아
    0 보장), `WaitDelay` 필수(강제 킬 유예), env **allowlist**(OS
    기반 세트: PATH·HOME/USERPROFILE·TEMP/TMP·SystemRoot 등 + 러너
    테이블의 언어별 필수 항목 + C# in-job 3종 주입 — 그 외 차단,
    비밀 운반 차단·D24 allowlist 철학 정합), 출력 캡(§D61).
  - **Windows**: **Job Object** — CREATE_SUSPENDED → assign →
    resume, kill-on-job-close, 메모리·프로세스 수 캡. **FS 제한
    없음 명시**(AppContainer는 후속 §4). 캡 기본값(메모리·프로세스
    수·WaitDelay 유예)은 구현 계획에서 확정.
  - **Linux**: setpgid(그룹 킬) + **landlock BestEffort**(go-landlock
    v0.9.0) — **쓰기 = 스크래치 한정, 읽기 전역 허용**(think-in-code
    유용성 유지). 미지원 커널은 BestEffort 통과(명시).
  - **macOS**: setpgid + **sandbox-exec 프로필** best-effort —
    Linux와 동일 정책(쓰기 스크래치 한정·읽기 허용).
  - **네트워크는 격리 경계가 아니다**(1문장 계약) — `go run` 모듈
    다운로드·NuGet restore와 동일 정책, SSRF 경계는 ctr_fetch_and_index
    몫. 구현은 OS별 파일 분리(build tag) — 자기정의 인터페이스 0
    계약 유지.
- **D60** 러너 테이블 6언어 — 전부 외부 toolchain 감지, 내장
  인터프리터 금지(vision §5 확정 계약 승계):

  | 언어 | 감지(우선순위) | 실행 | 게이트 |
  |---|---|---|---|
  | shell | Win: `pwsh`→`powershell` 폴백 / Unix: `/bin/sh` | 스크립트 파일 | — |
  | javascript | `bun`→`node` | 스니펫 파일 | — |
  | typescript | `bun`→`node` | bun 직접 / node `--experimental-transform-types` 조립 | **node ≥22.7**(버전 게이트) |
  | python | `python3`→`python` | 스니펫 파일 | — |
  | go | `go` | `go run` | — |
  | csharp | `dotnet` | `dotnet run file.cs`(file-based apps) | **SDK ≥10**(버전 게이트) |

  - **TS 폴백 근거**(bun 전용안 기각, 사용자 결정): bun 미설치
    유저 커버. node의 TS 실행은 버전 조건부(22.6+ 플래그·22.18/
    23.6+ 무플래그)이고 기본 type stripping은 erasable 문법만
    지원(enum·런타임 namespace·parameter properties 불가) —
    의미론 드리프트 최소화를 위해 **항상
    `--experimental-transform-types`를 명시 조립**(strip-types 자동
    포함, 22.7+에서 항상 유효 — 분기 1개). experimental 경고는
    stderr 병기라 무해. 게이트 미달 시 명확 오류.
  - **C# 3조건**(2026-07-24 사용자 자문 후 확정): ① **in-job env
    3종 강제** — `DOTNET_CLI_DO_NOT_USE_MSBUILD_SERVER=1`(CLI 지속
    MSBuild 서버 비활성)·`MSBUILDDISABLENODEREUSE=1`(워커 노드 상주
    금지)·`UseSharedCompilation=false`(Roslyn 공유 서버 VBCSCompiler
    금지, env가 MSBuild 전역 프로퍼티로 주입됨). 근거: 상주 서버는
    (a) 호스트의 기존 공유 서버에 붙어 실컴파일이 잡 밖에서 실행
    (격리 우회) (b) 잡 안에서 뜬 서버의 살해·데몬화로 호스트 부수
    피해·고아 0 위반 (c) 스크래치 핸들 점유로 삭제 실패 — 3방향
    충돌. env 방식(argv 아님)인 이유: 스니펫 내부의 재귀 dotnet
    호출에도 상속. 부작용 = 매 실행 콜드 컴파일(MS 문서도 일회성
    환경에서 서버 무익 명시). ② **SDK ≥10 버전 게이트**(file-based
    apps 전용 — 감지 테이블 버전 조건, 미달 시 명확 오류) ③
    네트워크 비경계 명시(#:package restore — D59와 동일 문장).
  - 감지 캐시: `LookPath`는 호출마다(저비용), 프로세스 실행이
    필요한 버전 확인(`dotnet --list-sdks`·`node --version`)만 서버
    수명 캐시.
- **D61** 출력 ephemeral 정책(v0.0.1 §14 미결 소진):
  - **저장 표면 0** — 서버는 실행 결과를 저장하지 않는다. 스크래치
    즉시 삭제(best-effort — 실패는 warning 로그) + 서버 기동 시
    24h+ 스테일 스크래치 스윕. §5.3 "출력 자동 영구 FTS 색인
    금지" 정합. **회수는 기존 파이프**: exec 호출 자체가 cc 훅
    tool_call로 기록되고 대형 결과는 shadow recall(D26, 하드
    캡·denylist)이 담당 — 신규 저장 장치 없음.
  - **캡**: stdout 32KiB·stderr 8KiB. 초과 시 `truncated=true` +
    안내 "출력 캡 초과 — 코드 안에서 더 집계/필터하여 파생 답만
    print"(think-in-code 유도).
  - **반환 구조**: 프로세스 성립 시 비 0 exit도 정상
    반환(`stdout`·`stderr`·`exit_code`·`duration_ms`·`truncated`),
    타임아웃은 `timed_out=true` + 부분 출력 반환. 도구 오류는 성립
    불가 사유만(미지원 언어·toolchain 미설치·버전 게이트 미달·path
    검증 실패).
- **D62** 회수 경로 채택 개선(D57 병행 축 — 실측 8 vs 422 대응):
  - **도구 설명 개정**: `ctr_search`(재읽기 전에 검색 — 트리거
    시나리오)·`ctr_fetch`(byte-exact 회수 — **§3.5 비혼동 계약
    보존**: "웹 fetch가 아니다" 문구·가드 테스트 유지)·신규 exec
    2종(think-in-code 가이드 + truncated 시 ctr_search 연계).
    서버 내 description 상수 수정만.
  - **채택 계측 상시화**: `ctr usage --adoption [--days N]` —
    session.db tool_call 이벤트 집계로 ctr_* vs ctxscribe(ctx_*)
    호출 수·비율 출력. **지표 정의**: adoption ratio = ctr MCP
    호출 / (ctr + ctxscribe MCP 호출). 베이스라인(§3) ~1.5%.
    v0.11 계약 = 베이스라인 기록 + 추세 관찰 — **목표치 미설정**.

## 1. v0.11 제품 계약

### 1.1 범위

- `internal/sandbox` 신설 — OS별 build tag 3파일(Job Object /
  setpgid+landlock / setpgid+sandbox-exec) + 공통 트리 킬·WaitDelay·
  env allowlist·스크래치 수명. 자기정의 인터페이스 0(동일 함수
  시그니처, 파일별 구현).
- `internal/exec` 신설 — 러너 테이블 6언어(감지·버전 게이트·명령
  조립·C# env 주입), 출력 캡·반환 구조 조립.
- MCP 표면 — `ctr_execute`·`ctr_execute_file` 등록(base 프로필),
  도구 설명 개정 4건(D62), tools/list 직렬화 상한 테스트 상향(2도구
  추가·설명 확장 반영 — 의도된 상향임을 커밋에 명시).
- CLI — `ctr usage --adoption [--days N]`, doctor [18] 러너 감지
  라인, doctor 호스트 어댑터 스니펫 개정(cc ask 2종·codex
  enabled_tools 2종).
- 문서 — 코드 아키텍처 문서 갱신(신규 패키지 2개 — 의존 그래프·
  D13 반파편화 정합), 본 설계서.

### 1.2 명시적 비범위 (v0.11)

- `ctr_batch_execute`(D58 — 재도입은 실수요 실증 시).
- AppContainer(Win)·seccomp(Linux) 등 권한 축소 강화 — 후속(§4).
- 네트워크 격리(D59 — 경계 아님 명문화가 본 릴리스 계약).
- bun 경로 CI 검증(setup-bun) — 도그푸딩 실관측으로 커버, 추가는
  플랜 재량·릴리스 게이트 아님.
- 스킬/플러그인 표면·doctor 스니펫의 CLAUDE.md 권장 문구(채택 수단
  후보 중 사용자 제외분).
- adoption 목표치 설정·exec 출력의 서버측 인덱싱(D61 저장 표면 0).
- ctxscribe 브리지 종료 절차(D57 대체 판정은 adoption 추세 확보
  후 — §4).

### 1.3 선행 게이트

- 없음 — D3의 노출 3조건(실질 격리·실행 계약·호스트 승인)은 본
  릴리스가 이행 자체(전제 블록). 관찰 이월 항목(D46 회수 창·empty
  GC 07-27+)은 본 릴리스와 직교.

## 2. 검증·테스트 계약

- **golden fixtures**(언어별): 결정적 성공 출력 / 비 0 exit /
  타임아웃(`timed_out`) / 출력 캡(`truncated`) / `CTR_FILE`
  (execute_file) / 감지 부재 `t.Skip`(러너별 toolchain 유무 강건).
  버전 게이트 2행은 게이트 미달 오류 fixture 추가(TS·C#).
- **격리 실증**(계약별 1개 이상): 트리 킬 — 자식 spawn 후 타임아웃
  → 잔존 프로세스 0(전 OS); Windows 메모리 캡 초과 → Job Object
  종료; Linux/macOS 스크래치 밖 쓰기 실패(BestEffort 미지원 환경
  통과 명시); C# env 3종 주입 검증(스니펫이 env 읽어 확인).
- **CI**: 기존 3-OS 매트릭스 — 러너 이미지 제공
  toolchain(node·python·go·dotnet)은 전 OS 실검증. .NET 10 이미지
  부재 시 setup-dotnet 핀. bun 경로만 CI 밖(§1.2).
- **메모리 캡 규칙**: `go test -p 1` 준수 — exec 테스트는
  실프로세스를 띄우므로 기존 직렬 규칙이 방어선. 대형 테스트 데이터
  는 `strings.Repeat`/testdata(응답 분할 규율).
- **릴리스 게이트(v0.11.0)**: ① CI 3-OS GREEN(격리 실증 fixture
  포함) ② 도그푸딩 — 설치 후 execute·execute_file 각 1회 이상
  실사용 + adoption 베이스라인 기록 ③ doctor [18] 실관측 ④ 관찰
  이월 목록 갱신.

## 3. 관측 실측 기록 (2026-07-24, session-28 — 컨트롤러 수행)

- **doctor**: [16] codex 양 스코프 marker 0.10.0(user 스테일 해소
  — session-27 `--user` 별도 설치 실효), [17] build 0.10.0
  (7cbcaa5, dirty=untracked 코스메틱 지속), [14] file
  145,993,728B(139.2MB — D46 116.7MB에서 +22.5MB/일), [15]
  shadow-owned 34,653,787B(cc 32.6MB·cx 2.0MB), [6] 131 세션
  (empty 91), [12] drops 총 400(worktree unknown-session 399 —
  불변).
- **D54 라이브 파이어**(최초 실증): Grep 실사용 → 0.10.0 marker
  아래 tool_call 기록 확인(matcher 경로); `head_limit=0`+content
  의도 시도 → deny reason "무제한 content 검색 — head_limit을
  지정하거나 ctr_search로 검색" + **warning 이벤트
  `Grep: head_limit=0 content` 실기록**(deny 경로). 기존 warning
  2건(07-21)은 구버전 Bash/PowerShell 대용량 덤프 경고로 D54와
  무관 확인.
- **D53**: subagent_start/stop 누적 1쌍(session-27 Explore,
  agent_id a062965346e7e8796) — 이후 spawn 없음. 세션 실시간 기록
  건강(session-28 자체 19이벤트 — AskUserQuestion·error 2건 포함).
- **empty GC**: 91건 전부 07-20(11)·07-21(50)·07-22(13)·07-23(17)
  생성 — 첫 적격 07-27, 피크 07-28(예정대로 미도래).
- **D46 회수 결정**(사용자): 수동 창 — 전 CC/Codex 세션 종료 후
  `context-router purge --hook-only --project context-router-4d307dacd75b`
  직접 실행(유예 = age-gate 1h뿐이라 shadow 34.7MB 대부분 회수
  기대, 139.2 → ≈104MB). busy 실패 시 열린 세션 확인 후 재실행.
- **exec 계열 실측**(session.db 전 기간, D58·D60 근거):
  ctx_execute 378 · ctx_execute_file 172 · ctx_batch_execute
  30(5.2%) · ctx_search 33 · ctx_fetch_and_index 5. **adoption
  베이스라인**: ctr MCP 9회(search 4·session_summary 3·
  export_events 2) vs ctxscribe 618회 ≈ **1.5%**.
- **환경 실확인**: dotnet SDK 10.0.301(이 머신), CI 3-OS
  매트릭스(ubuntu·windows·macos-latest), 이월 스크래치
  C:\tmp\ctr-g4 삭제 확인(해소).

## 4. 의도적 미결 (v0.12+ 후보 — v0.10 §4에서 이월·개정)

- **ctxscribe 브리지 종료 절차** — D57 대체 판정: exec 도그푸딩
  정착 + adoption 추세(`usage --adoption`) 확보 후 별도 세션에서
  결정(포크 유지보수 종료 수순 포함).
- 격리 강화 — AppContainer(Win)·seccomp(Linux) 권한 축소(D59
  후속), 네트워크 격리 재평가.
- `ctr_batch_execute` 재도입 — exec 도입 후 병렬 실수요 실증 시.
- bun 경로 CI(setup-bun) — 도그푸딩 실관측 부족 판명 시.
- 무작위 A/B 하네스·OTel(D27) — cx arm n 정체 지속으로 cc 축 우선
  검토(이월).
- 서브에이전트 캡처 확장 — last_assistant_message 수용(응답 본문
  취급 계약 재설계 전제)·agent_type matcher 선별·cx 표면 등장 시
  cx 축(이월).
- synthetic 표식 영속화 — retention 창 초과 생존 세션 오분류 실증
  시(이월).
