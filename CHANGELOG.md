# Changelog

이 파일은 **v0.12.0부터** 기록한다. 그 이전 릴리스는 git 태그와 설계 문서
(`docs/context-router-design-v0.X-ko.md` — D 번호 결정 이력)를 참조한다. 소급
작성은 하지 않았다.

형식은 [Keep a Changelog](https://keepachangelog.com/ko/1.1.0/), 버전은
[Semantic Versioning](https://semver.org/lang/ko/)을 따른다. 각 항목의 D 번호는
그 결정의 근거가 있는 설계 문서 절을 가리킨다.

## [0.14.0] — 2026-07-29

### Changed — 사용자 관측 가능한 동작 변경

- **shell 러너(windows)의 `.ps1` 종료 신호를 `Register-EngineEvent PowerShell.Exiting`으로
  승격했다** (D78). `exit`·인수 없는 `exit`·`throw`로 끝나는 스니펫에서도 마지막 외부
  명령의 종료 코드가 기록되므로, v0.13이 적은 "스니펫이 `exit`으로 끝나면 이 보강은
  동작하지 않는다"는 더 이상 맞지 않는다. 보강 줄의 조건은 D76 그대로다 — `exit_code`가
  0이고 `stderr`가 빈 경우에만 들어가고, `exit_code`의 의미도 바뀌지 않는다. 그래서 줄이
  **새로 붙는 것은 종료 코드 0으로 끝나는 `exit`**(인수 없는 `exit`과 `exit 0`)이고,
  `exit <비 0>`과 `throw`는 종료 코드가 기록돼도 그 조건에서 걸러진다.
  - 등록 문은 스니펫을 **감싸지 않고 첫 줄에 이어 붙는다** — 문 종료 오류가 나도 그 뒤의
    나머지 스니펫이 그대로 실행된다. 대신 **오류가 1행뿐인 스니펫에서는 stderr의 열
    번호가 밀리고 소스 줄 에코에 등록 문 꼬리가 실린다**(줄 번호는 정확하다). 2행 이상
    오류의 stderr는 바이트 그대로다.
  - 최상단 전용 구문(`param`·`using`·`dynamicparam`·명명 블록·속성 선언·줄 연속)으로
    시작하는 스니펫은 승격하지 않고 v0.13 형태로 폴백한다 — 그 경로의 알려진 공백도
    함께 유지된다.

### Internal

- 기동 purge의 실 잠금 보유 시간에 자동 게이트를 세웠다 (D77). 임계는 훅 예산 2000ms의
  50%다. 픽스처는 나이 게이트를 넘겨 **실제 회수 경로를** 재며, 그 사실을 테스트가
  단정한다(`ReclaimedB`·`DeferredFiles`·`FailedFiles`). 잡는 것은 해시당 비용의
  회귀이며, 100해시 배치의 절대 보유 시간은 합성 픽스처로 검증하지 않는다.
- lint에 `GOOS=darwin`·`GOOS=netbsd`·`--build-tags perf` 세 축을 더해 D69가 남긴 미검출
  파일 셋(`launcher_darwin.go`·`launcher_other.go`·`perf_test.go`)을 비웠다 (D79). 축마다
  컴파일을 먼저 확인한다 — 빌드가 깨진 축에서 lint는 `0 issues.`를 내며 종료 코드만
  실패해 스코프 확인이 거짓 초록이 되기 때문이다. `perf`는 태그가 걸린 파일이 테스트
  파일뿐이라 `go build`가 아니라 **테스트 컴파일**로 판정한다. `freebsd`는 그 빌드 조건을
  만족하지 않아 대표에서 제외했다.

## [0.13.0] — 2026-07-27

### Changed — 사용자 관측 가능한 동작 변경

- **shell 러너(unix)가 새로 홈 유도 격리를 적용받는다** (D75). `~`와 `$HOME`이 매 실행
  새 스크래치를 가리키므로 그 안에서 부르는 툴체인이 호스트 사용자 구성을 읽지 않는다 —
  javascript·typescript 러너와 같은 공유 헬퍼다(그 둘은 v0.12/D65부터 이미 스크래치
  홈을 썼고, 이번에 `CFFIXED_USER_HOME`·`XDG_DATA_HOME` 앵커를 추가로 얻는다). **홈
  유도 툴체인 루트와 캐시도 함께 옮겨가 스니펫이 의존물을 다시 받는다** — 실행 시간이
  늘고 네트워크 접근이 발생할 수 있다(dotnet 첫 실행 워크로드 무결성 검사망은 이 배송이
  억제하지만, `npm install`·`dotnet restore` 같은 일반 의존성 복원까지 막지는 않는다).
- **javascript·typescript 러너에서 홈 준비 실패가 실행 거부로 바뀌었다** (D75).
  이전에는 경고만 남기고 실행했다. 홈이 없으면 재지정이 성립하지 않고 그 상태로 실행하면
  호스트 홈으로 되돌아갈 수 있다.
- **shell 러너(windows)가 마지막 외부 명령의 종료 상황을 `stderr`에 한 줄로 남긴다** (D76).
  `exit_code`가 0이고 `stderr`가 빈 경우에만 들어가며 `exit_code`의 의미는 바뀌지 않는다.
  이 줄은 "스니펫이 실패했다"가 아니라 "마지막 외부 명령(네이티브 프로그램 또는 호출된 스크립트)이 비 0으로 끝났다"를
  뜻한다. 스니펫이 `exit`으로 끝나면 이 보강은 동작하지 않는다 — 종료 코드를 명시하는 것이
  그 경우의 대응이다.
- **doctor `[3]`에 대상 색인의 실재 수가 병기된다** (D73): `indexes=<실재>/3`.

### Internal

- shadow 보존 술어에 복합 색인 3개를 더했다 (D73). **스키마 버전은 그대로 1이다** — 색인은
  가산적 변경이라 이 릴리스의 스토어를 이전 버전 바이너리도 계속 열 수 있다. 색인이 있으면
  shadow 아티팩트가 늘어도(2천→2만 행) 술어 소요 증가가 완만하다 — 없으면 거의 제곱으로
  자란다(수치는 설계 §3 실측 참조).
- 기동 purge의 파일 회수 루프가 종료 신호를 관측한다 (D74).

## [0.12.1] — 2026-07-27

### Changed

- **doctor `[12]` drops 진단에 사유별 마지막 발생 시각이 병기된다** (D71).
  형태는 `<사유>=<건수>@<YYYY-MM-DD>`(UTC)다. 누적 집계만 보이던 이전 출력은 이미 해결된
  문제를 현재 발생 중인 것처럼 읽히게 했다 — 시각이 있으면 그 구분이 한눈에 보인다.
  정수 변환에 실패한 **줄만** 병기에서 빠지고, 같은 사유의 다른 줄이 만든 병기는
  남는다(집계는 유지된다). `unparsed`는 사유 단위 버킷이라 애초에 병기 대상이
  아니다.

### Internal

- CI 액션 참조를 커밋 SHA로 고정하고 워크플로 토큰 권한을 `contents: read`로 좁혔다 (D68).
- `GOOS=windows` lint 스텝을 추가해 windows 전용 파일의 미검출 정적 오류를 닫았다 (D69).
- bun을 설치하지 않는 잡을 추가해 node 인터프리터 레그를 CI에서 실행한다 (D70).
- `buildinfo` 주석을 실제 릴리스 절차에 맞췄다 (D72).

## [0.12.0] — 2026-07-26

### Changed — 사용자 관측 가능한 동작 변경

- **shadow 데이터가 3일 후 자동 삭제된다** (D67). 서버 기동 시 1회 실행되며,
  훅이 가로챈 출력(shadow 귀속)에만 적용된다. 나이 기준은 **마지막 포착
  시각**이므로 같은 내용이 다시 포착되면 보존 기간이 갱신된다. `ctr_index` ·
  `ctr_fetch_and_index`로 직접 넣은 explicit 소스는 **무기한 보존**한다.
  보존 기간은 `CTR_SHADOW_RETENTION`(`time.ParseDuration` 형식, 양수만 채택)으로
  조정할 수 있다.
  - **파일 크기는 즉시 줄지 않는다.** 삭제로 생긴 여유 공간은 이후 기록이
    재사용하고, 자동 경로에서 VACUUM은 하지 않는다 — 목표는 파일 축소가 아니라
    성장 억제다. 디스크를 되돌려야 하면 서버 비가동 상태에서
    `purge --project <id> --older-than <dur> --vacuum`.
  - doctor `[14]`의 "자동 삭제 없음"은 **임계 초과 경고**에 붙은 문면이다 —
    임계를 넘겨도 그 경고 자체가 삭제를 유발하지 않는다는 뜻이며, 위의 3일 보존과
    별개 경로다.
- **python 러너에서 호스트 `pip install --user` 패키지가 보이지 않는다** (D65).
  `PYTHONNOUSERSITE=1`로 실행하고 `PIP_CONFIG_FILE`을 스크래치로 돌린다.
- **javascript·typescript 러너가 스크래치 홈을 쓴다** (D65). `HOME`과
  `XDG_CONFIG_HOME`을 스크래치로 돌리므로 호스트 홈의 도구 구성이 스니펫에
  반영되지 않는다.
- **go 러너가 `GOENV`를 스크래치로 돌린다** (D65). 호스트 `go env -w` 설정이
  스니펫 실행에 상속되지 않는다.
- **shell 러너(windows)가 모듈 경로와 사용자 프로필을 고정한다** (D65).
  `PSModulePath`를 스크래치·인터프리터 설치본·머신 전역 세 항목으로 명시하고
  `USERPROFILE`을 스크래치로 돌리므로, 호스트에 설치된 사용자 모듈이 스니펫에서
  자동 로드되지 않는다.
- **격리 구성 쓰기가 실패하면 실행을 거부한다**(fail-closed). 이전에는 경고만
  남기고 그대로 실행했다. 판정 기준은 "그 파일이 없을 때 호스트와 무관한 기본으로
  안전하게 퇴화하는가"이며, 퇴화하지 않는 지점(pip.conf · nuget.config ·
  psmodules · GOTMPDIR · macOS Application Support · NuGet sentinel)이 시행
  대상이다.
- **MCP 서버 등록이 단일화되고 상시 로드가 붙는다** (D63). 설치기가
  `alwaysLoad: true`를 쓴 단일 `ctr-exec` 항목을 만들고, 같은 도구를 중복 노출하던
  옛 `ctr` 등록은 제거한다. `alwaysLoad`는 **Claude Code v2.1.121 이상**에서만
  동작하며 그 이하 호스트는 이 키를 조용히 무시한다.
- **`usage --adoption` 집계 단위가 MCP 서버 네임스페이스로 바뀌었다** (D63).
  이전의 `ctr_` 부분 문자열 버킷팅은 서로 다른 서버 등록을 한 카운터에 합쳤다.
  이제 서버별 호출 수와 그 서버를 부른 고유 세션 수를 tool_call 세션 분모와 함께
  낸다. 옛 2행 출력과 ratio 문면은 없어졌다.

### Added

- **`hook install`이 `.mcp.json` 등록과 호스트 승인 키까지 관리한다** (D64).
  이전에는 훅만 설치했고 MCP 등록은 수동 편집이었다. 플래그 없는 재실행은 기존
  `args`를 보존하고(활성 프로필을 끄지 않는다), 재실행 결과는 바이트 단위로
  멱등이다. Codex 훅은 별도 갈래이므로 전체 설치는 **세 번**이다 —
  `hook install` · `hook install --codex` · `hook install --codex --user`.
- **`hook uninstall`이 설치기가 넣은 것을 되돌린다** (D64). `.mcp.json`의 관리
  항목과 승인 키에서 그 이름을 함께 제거한다.
- **doctor `[19]` 승인 규칙 정합 진단** (D64). `ask`와 `allow`가 같은 도구를
  덮어 한쪽이 무력화되는 상태를 보고한다.
- **doctor 호스트 스니펫이 권한 모드를 기준으로 안내한다** (D64). 승인 강도가
  호스트 권한 모드와 규칙 평가 순서에 따라 달라지는 점을 문면에 반영했다.

[0.14.0]: https://github.com/wotjr1649/context-router/compare/v0.13.0...v0.14.0
[0.13.0]: https://github.com/wotjr1649/context-router/compare/v0.12.1...v0.13.0
[0.12.1]: https://github.com/wotjr1649/context-router/compare/v0.12.0...v0.12.1
[0.12.0]: https://github.com/wotjr1649/context-router/compare/v0.11.1...v0.12.0
