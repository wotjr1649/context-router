# Changelog

이 파일은 **v0.12.0부터** 기록한다. 그 이전 릴리스는 git 태그와 설계 문서
(`docs/context-router-design-v0.X-ko.md` — D 번호 결정 이력)를 참조한다. 소급
작성은 하지 않았다.

형식은 [Keep a Changelog](https://keepachangelog.com/ko/1.1.0/), 버전은
[Semantic Versioning](https://semver.org/lang/ko/)을 따른다. 각 항목의 D 번호는
그 결정의 근거가 있는 설계 문서 절을 가리킨다.

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

[0.12.1]: https://github.com/wotjr1649/context-router/compare/v0.12.0...v0.12.1
[0.12.0]: https://github.com/wotjr1649/context-router/compare/v0.11.1...v0.12.0
