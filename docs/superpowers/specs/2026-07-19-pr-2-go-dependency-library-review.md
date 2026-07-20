# PR #2 Go 의존성 라이브러리 조사 및 변경 범위 결정서

> 문서 작성일: 2026-07-19 KST
>
> 조사 기준일: 2026-07-18 KST
>
> 대상: PR #2 (`feat/v0.0.1-global-cli` → `main`)
>
> 상태: Claude Code 리뷰 및 의존성 변경 범위 결정용
>
> 대상 파일: [`go.mod`](../../../go.mod)

## 1. 결론

현재 `go.mod`의 선택은 context-router의 제약에 전반적으로 적합하다.

- 직접 의존성 7개는 모두 프로덕션 코드에서 실제 사용된다.
- 현재 의존성 그래프는 Windows와 Linux에서 `CGO_ENABLED=0`으로 빌드된다.
- `go.mod`의 26개 모듈은 모두 유효한 Go 버전이다.
- 잘못된 major suffix, 검증 불가 버전, 현재 버전에 대한 retract는 발견되지 않았다.
- 가장 근거가 강한 개선 후보는 `codeberg.org/readeck/go-readability/v2`이지만, PR #2에는 넣지 않는 것이 기본 결정이다.
- `golang.org/x/net`과 `golang.org/x/sys`는 라이브러리 자체는 유지하고, 최신 태그 승격은 별도 유지보수 PR에서 검증한다.
- MCP SDK, Starlark, HTML→Markdown, SQLite 드라이버는 현행 유지가 가장 합리적이다.

### PR #2 범위 결정

**기본 결정: 이 문서 외의 라이브러리 변경을 PR #2에 추가하지 않는다.**

근거:

- 현재 로컬 `main...HEAD` diff는 55개 파일, 약 8,570줄이다.
- PR #2에는 현재 `go.mod`와 `go.sum` 변경이 없다.
- 조사에서 즉시 수정해야 하는 잘못된 버전, retract, CGO 침투, 차단급 호환성 문제는 확인되지 않았다.
- `go-readability`의 `/v2` 전환은 major import path와 출력 동작이 바뀌는 독립 변경이다.
- 의존성 업데이트를 현재 PR에 섞으면 기능 회귀의 원인과 리뷰 책임 범위가 불필요하게 넓어진다.

같은 PR에 넣을 수 있는 예외는 리뷰에서 아래 중 하나가 **구체적으로 입증된 경우뿐**이다.

1. 현재 버전 때문에 PR #2의 필수 동작이 실패한다.
2. 현재 버전에 공개된 차단급 보안·데이터 무결성 문제가 있고 우회가 불가능하다.
3. PR #2 코드가 이미 새 API를 전제로 하여 버전 변경 없이는 빌드되지 않는다.

현재 조사 결과는 세 예외에 모두 해당하지 않는다.

## 2. 조사 범위와 판정 기준

다음 자료를 교차 확인했다.

- 로컬 `go.mod`, `go.sum`, 모든 Go 프로덕션 코드와 관련 테스트
- `go mod graph`를 통한 직접·간접 의존 관계
- Go 공식 모듈 문서와 pkg.go.dev
- 각 프로젝트의 공식 저장소, 고정 태그, 릴리스, 정확한 pseudo-version 커밋
- Context7의 `modernc.org/sqlite` 공식 문서 색인
- 독립 범위로 나눈 서브에이전트 7개의 조사 결과

판정 기준은 다음과 같다.

- 실제 사용 여부와 교체 시 보존해야 하는 동작
- 공식성, 유지보수 상태, 공개 채택 신호
- 태그·pseudo-version의 유효성, SemVer 안정성, 최신성, retract 여부
- `CGO_ENABLED=0`, 단일 바이너리, Windows/Linux 호환성
- `database/sql`, WAL, FTS5, MCP, Starlark 격리 계약과의 적합성
- 교체 비용, 회귀 위험, 검증 가능성

pkg.go.dev의 `Imported by` 수는 다운로드 수가 아니라 공개 모듈 그래프에 알려진 importer 수다. 별 수와 importer 수는 조사 시점의 스냅샷이므로 품질을 단독으로 보증하지 않는다.

이번 조사는 의존성 적합성·버전·대체재 조사다. CVE 전수 감사나 `govulncheck` 전체 감사는 포함하지 않았다.

## 3. `go.mod`의 역할

이 프로젝트의 `go.mod`는 다음을 정의한다.

- 모듈 경로: `github.com/wotjr1649/context-router`
- 요구 Go 버전: `go 1.26.5`
- 프로젝트 코드가 직접 import하는 모듈 7개
- 직접 모듈이 끌어오는 선택된 간접 모듈 19개

`go.mod`는 단순한 라이브러리 목록이 아니라 Go의 Minimal Version Selection에 사용되는 재현 가능한 모듈 그래프의 입력이다. `go.sum`은 선택 모듈 콘텐츠의 체크섬 검증 자료다.

## 4. 직접 의존성 7개의 실제 사용

직접 의존성은 모두 실제 프로덕션 경로에 연결된다. 즉시 제거 가능한 항목은 없다.

| 모듈 | 실제 사용 위치와 기능 | 교체 시 보존해야 하는 계약 | 교체 난이도 |
|---|---|---|---|
| `codeberg.org/readeck/go-readability` | [`internal/netfetch/netfetch.go`](../../../internal/netfetch/netfetch.go#L420)의 본문·제목 추출 | 상대 URL, 제목, 빈 본문·500자 미만·텍스트 30% 미만·`pre/code` 50% 미만에서 원문 전체로 fail-open하는 fidelity 계약 | 높음 |
| `github.com/JohannesKaufmann/html-to-markdown/v2` | 같은 파일의 base/commonmark/table 플러그인 기반 HTML→Markdown 변환 | 코드 펜스, GFM 표, 링크·이미지·강조, escape 및 오류 계약 | 중간~높음 |
| `github.com/modelcontextprotocol/go-sdk` | [`internal/mcp/mcp.go`](../../../internal/mcp/mcp.go#L191)의 서버·stdio transport·도구 등록과 schema 생성 | MCP 수명주기, tool annotation, structured content, `IsError`, in-memory transport 테스트 | 높음 |
| `go.starlark.net` | [`internal/transform/transform.go`](../../../internal/transform/transform.go#L73)의 hermetic 변환 실행 | 파일·네트워크·환경·시계·난수 비노출, step 취소, 출력 제한, 결정적 값 변환, 오류 요약 | 매우 높음 |
| `golang.org/x/net` | [`internal/netfetch/netfetch.go`](../../../internal/netfetch/netfetch.go#L273)의 charset 감지·UTF-8 변환과 HTML5 DOM 파싱 | EUC-KR 등 비UTF 입력, meta charset, 원문 보존, DOM fidelity 계산 | 중간~높음 |
| `golang.org/x/sys` | [`internal/store/store_lock_windows.go`](../../../internal/store/store_lock_windows.go#L9)의 `LockFileEx`와 [`internal/transform/worker_windows.go`](../../../internal/transform/worker_windows.go#L10)의 Job Object | Windows 비차단 배타 잠금, 256MB Job 제한, `KILL_ON_JOB_CLOSE`, 핸들 정리 | 중간 |
| `modernc.org/sqlite` | [`internal/store/store.go`](../../../internal/store/store.go#L35)의 `database/sql` driver, WAL·FTS5·DSN·오류코드 처리 | driver명 `sqlite`, `_pragma`, `_txlock=immediate`, FTS5, trigger, `sqlite.Error.Code()`, BUSY/LOCKED 재시도 | 매우 높음 |

HTML 관련 모듈은 기능이 겹쳐 보이지만 중복 대체재가 아니다. 현재 파이프라인은 다음 책임을 분리한다.

`fetch → charset decode → readability 추출 → 원문/추출 DOM fidelity 비교 → Markdown 변환`

## 5. 직접 의존성의 버전·채택도·권고

| 모듈과 현재 버전 | 공식성·채택 신호 | 버전 판정 | CGO 판정 | 권고 |
|---|---|---|---|---|
| [`go-readability`](https://pkg.go.dev/codeberg.org/readeck/go-readability) `v0.0.0-20260615104154-29522a0e224f` | Readability.js 계열의 Readeck 포크. v0 공개 importer 3 | 유효한 pseudo-version이지만 태그·안정판이 아니며 v0 호환 트랙 | 없음 | 같은 프로젝트의 `/v2`를 별도 PR에서 검토 |
| [`html-to-markdown/v2`](https://pkg.go.dev/github.com/JohannesKaufmann/html-to-markdown/v2) `v2.5.2` | 약 3.7k GitHub stars, 공개 importer 약 113 | 최신 안정판. [v2.5.2 릴리스](https://github.com/JohannesKaufmann/html-to-markdown/releases/tag/v2.5.2) | 없음 | 유지 |
| [`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk) `v1.6.1` | 공식 MCP 조직 SDK, Google과 협업 관리, 공개 importer 약 1,443, [Tier 1 SDK](https://modelcontextprotocol.io/docs/sdk) | 최신 안정판. `v1.7.0-pre.3`은 프리릴리스 | 없음. 다만 간접 모듈에 assembly 최적화가 있음 | 강력 유지 |
| [`go.starlark.net`](https://pkg.go.dev/go.starlark.net) `v0.0.0-20260708150628-5395d018f003` | canonical upstream은 `google/starlark-go`, 공개 importer 약 1,209 | upstream에 안정 태그가 없어 pseudo-version 고정이 정상. 조사 시점 최신 HEAD | 없음 | 정확한 커밋 유지 |
| [`golang.org/x/net`](https://pkg.go.dev/golang.org/x/net) `v0.55.0` | Go 프로젝트 공식 서브리포지터리, 매우 널리 사용 | 유효한 공식 v0 태그, 최신 `v0.57.0`보다 두 릴리스 뒤 | 사용하는 `html`, `html/charset`은 CGO 없음 | 라이브러리 유지, 버전은 별도 PR에서 검증 |
| [`golang.org/x/sys`](https://pkg.go.dev/golang.org/x/sys) `v0.46.0` | Go 프로젝트 공식 서브리포지터리, 매우 널리 사용 | 유효한 공식 v0 태그, 최신 `v0.47.0`보다 한 릴리스 뒤 | CGO는 없으나 플랫폼 syscall·assembly 포함 | 라이브러리 유지, 버전은 별도 PR에서 검증 |
| [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) `v1.54.0` | 공개 importer 약 3,518, BSD-3-Clause | 최신 안정판. SQLite 3.53.3 포함. [v1.54.0 릴리스](https://github.com/modernc-org/sqlite/releases/tag/v1.54.0) | 소비자 빌드 CGO 없음. C 코드를 Go로 변환한 생성 구현 | 강력 유지 |

### `go-readability`의 정확한 해석

현재 v0 모듈은 유효하고 최근 갱신된 호환성 트랙이므로 잘못된 선택은 아니다. 다만 공식 문서는 개발이 `/v2`에서 계속되며 최상의 속도와 메모리 효율을 위해 v2를 선택하라고 명시한다.

- 권장 후보: [`codeberg.org/readeck/go-readability/v2 v2.1.2`](https://pkg.go.dev/codeberg.org/readeck/go-readability/v2)
- v2는 태그가 있는 안정 major다.
- 주요 API 차이는 `Article` 공개 필드 일부가 메서드로 바뀌고 HTML/Text 출력을 render 메서드로 얻는 것이다.
- v2는 휴면 상태인 `araddon/dateparse` 대신 유지되는 `itlightning/dateparse`를 사용한다.
- 추출 결과가 달라질 수 있으므로 import path만 바꾸는 기계적 업데이트로 취급하면 안 된다.

### Starlark pseudo-version의 정확한 해석

`go.starlark.net`은 GitHub 저장소 주소와 다른 canonical vanity import path다. `github.com/google/starlark-go`로 import path를 바꾸면 안 된다.

현재 커밋 [`5395d018f003`](https://github.com/google/starlark-go/commit/5395d018f003)은 pseudo-version 시각·SHA와 일치하며, 과도하게 중첩된 괄호 표현식이 parser stack crash를 일으키는 문제를 막는 수정이 포함된 조사 시점 최신 HEAD다. upstream이 아직 Go API의 파괴적 변경 가능성을 유보하므로 정확한 revision 고정과 transform 회귀 테스트가 적절하다.

## 6. 전체 버전 무결성

26개 항목을 모두 확인한 결과는 다음과 같다.

- canonical Go 버전 형식: 26/26
- Go index에서 유효: 26/26
- major suffix 규칙 적합: 26/26
- 현재 버전에 대한 retract: 0
- 검증 불가 또는 upstream보다 앞선 버전: 0
- release tag: 20
- pseudo-version: 6
- 안정 SemVer(`v1+`, non-prerelease): 12
- 조사 시점 최신: 19
- 더 새 태그 또는 권장 major 존재: 7

Go에서 `v0`와 pseudo-version은 가짜 버전이 아니라 canonical 버전이다. 다만 `v0`는 안정성과 하위 호환을 약속하지 않는다. 기준은 [Go Modules Reference](https://go.dev/ref/mod#versions)와 [Go 모듈 버전 규칙](https://go.dev/doc/modules/version-numbers)이다.

## 7. 간접 의존성 19개 전체 판정

간접 모듈은 프로젝트가 직접 API를 선택한 것이 아니라 직접 모듈의 요구사항과 Go MVS에 의해 선택된다. 개별 모듈을 임의로 승격하거나 대체하지 않는다.

| 간접 모듈 | 현재 버전 | 조사 시점 최신·상태 | 구현·위험 메모 | 조치 |
|---|---|---|---|---|
| [`github.com/JohannesKaufmann/dom`](https://pkg.go.dev/github.com/JohannesKaufmann/dom) | `v0.3.1` | 동일, current | html-to-markdown 목적형 pure-Go DOM 유틸 | 상위 모듈과 함께 유지 |
| [`github.com/andybalholm/cascadia`](https://pkg.go.dev/github.com/andybalholm/cascadia) | `v1.3.4` | 동일, current | 널리 쓰이는 pure-Go CSS selector | 유지 |
| [`github.com/araddon/dateparse`](https://pkg.go.dev/github.com/araddon/dateparse) | `v0.0.0-20210429162001-6b43995a97de` | 동일 HEAD, current | 2021년 이후 휴면이지만 공개 importer는 많음 | 직접 교체 금지. readability v2 전환 시 자연 제거 검토 |
| [`github.com/dustin/go-humanize`](https://pkg.go.dev/github.com/dustin/go-humanize) | `v1.0.1` | 동일, current | 성숙한 pure-Go 유틸 | 유지 |
| [`github.com/go-shiori/dom`](https://pkg.go.dev/github.com/go-shiori/dom) | `v0.0.0-20230515143342-73569d674e1c` | 동일 HEAD, current | 작은 휴면 pure-Go DOM 모듈 | 직접 포크·교체 금지 |
| [`github.com/gogs/chardet`](https://pkg.go.dev/github.com/gogs/chardet) | `v0.0.0-20211120154057-b7413eaefb8f` | 동일 HEAD, current | 휴면 pure-Go 문자셋 감지. ICU 유래 고지 보존 필요 | 상위 모듈을 통해 관리 |
| [`github.com/google/jsonschema-go`](https://pkg.go.dev/github.com/google/jsonschema-go) | `v0.4.3` | 동일, current | stdlib-only pure Go | 유지 |
| [`github.com/google/uuid`](https://pkg.go.dev/github.com/google/uuid) | `v1.6.0` | 동일, current | 매우 널리 쓰이는 pure Go | 유지 |
| [`github.com/mattn/go-isatty`](https://pkg.go.dev/github.com/mattn/go-isatty) | `v0.0.22` | `v0.0.23`, behind | CGO 없음, 플랫폼별 Go 코드와 `x/sys` 사용 | 직접 승격하지 말고 상위 모듈 업데이트에 맡김 |
| [`github.com/ncruces/go-strftime`](https://pkg.go.dev/github.com/ncruces/go-strftime) | `v1.0.0` | 동일, current | 소규모 pure-Go 유틸 | 유지 |
| [`github.com/remyoudompheng/bigfft`](https://pkg.go.dev/github.com/remyoudompheng/bigfft) | `v0.0.0-20230129092748-24d4a6f8daec` | 동일 HEAD, current | CGO는 없지만 `unsafe`와 `go:linkname`으로 `math/big` 내부 구현 사용. modernc 계층의 전이 의존성 | 앱에서 직접 대체하지 않음. modernc 업데이트로만 관리 |
| [`github.com/segmentio/asm`](https://pkg.go.dev/github.com/segmentio/asm) | `v1.1.3` | `v1.2.1`, behind | amd64/arm64 assembly 최적화와 pure-Go fallback | MCP SDK/encoding이 선택하게 둠 |
| [`github.com/segmentio/encoding`](https://pkg.go.dev/github.com/segmentio/encoding) | `v0.5.4` | 동일, current | CGO 없는 고성능 인코딩 구현 | 유지 |
| [`github.com/yosida95/uritemplate/v3`](https://pkg.go.dev/github.com/yosida95/uritemplate/v3) | `v3.0.2` | 동일, current | pure Go, `/v3` suffix 정상 | 유지 |
| [`golang.org/x/oauth2`](https://pkg.go.dev/golang.org/x/oauth2) | `v0.35.0` | `v0.36.0`, behind | Go 공식 pure-Go 인증 프로토콜 모듈 | MCP SDK 업데이트와 함께 검증 |
| [`golang.org/x/text`](https://pkg.go.dev/golang.org/x/text) | `v0.37.0` | `v0.40.0`, behind | Go 공식 pure-Go Unicode 데이터·처리 | `x/net` 업데이트가 선택하게 둠 |
| [`modernc.org/libc`](https://pkg.go.dev/modernc.org/libc) | `v1.74.1` | 동일, current | modernc/sqlite와 정확한 호환 버전 일치가 중요 | `sqlite v1.54.0` 요구값 그대로 유지 |
| [`modernc.org/mathutil`](https://pkg.go.dev/modernc.org/mathutil) | `v1.7.1` | 동일, current | 최신 태그, modernc 계층 | 유지 |
| [`modernc.org/memory`](https://pkg.go.dev/modernc.org/memory) | `v1.11.0` | 동일, current | 최신 태그, modernc 계층 | 유지 |

더 새 버전 또는 권장 major가 존재하는 7개는 다음과 같다.

1. `go-readability` v0 → `/v2 v2.1.2`
2. `golang.org/x/net v0.55.0` → `v0.57.0`
3. `golang.org/x/sys v0.46.0` → `v0.47.0`
4. `github.com/mattn/go-isatty v0.0.22` → `v0.0.23`
5. `github.com/segmentio/asm v1.1.3` → `v1.2.1`
6. `golang.org/x/oauth2 v0.35.0` → `v0.36.0`
7. `golang.org/x/text v0.37.0` → `v0.40.0`

이 목록은 보안 긴급도를 의미하지 않는다. 최신 태그가 존재한다는 뜻이며, 조사에서 현재 버전에 대한 차단급 CVE를 판정한 것은 아니다.

## 8. CGO-free와 엄밀한 pure Go의 차이

현재 구성은 외부 C 컴파일러나 시스템 SQLite가 필요 없는 **CGO-free 구성**이다. 다만 모든 소스가 손으로 작성된 `.go` 파일뿐이라는 엄밀한 의미의 pure Go와는 차이가 있다.

- `modernc.org/sqlite`: SQLite C amalgamation을 ccgo로 변환한 생성 Go 코드와 libc 에뮬레이션을 사용한다.
- `golang.org/x/sys`: 플랫폼 syscall 및 일부 assembly를 사용한다.
- `github.com/segmentio/asm`: assembly 최적화와 pure-Go fallback을 제공한다.
- 위 구성 모두 소비자 빌드에 C 컴파일러가 필요하지 않는다.

실제 검증 결과:

- 현재 플랫폼 의존 그래프에서 `CgoFiles`: 0건
- `CGO_ENABLED=0`, `GOOS=windows`, `GOARCH=amd64`, `go build ./...`: 성공
- `CGO_ENABLED=0`, `GOOS=linux`, `GOARCH=amd64`, `go build ./...`: 성공

따라서 CGO 제거를 목적으로 한 라이브러리 교체는 필요하지 않다.

## 9. 대체재 분석

### 9.1 본문 추출

| 후보 | 장점 | 손실·위험 | 판정 |
|---|---|---|---|
| [`go-readability/v2`](https://pkg.go.dev/codeberg.org/readeck/go-readability/v2) | 동일 계열의 공식 권장판, tagged v2, 속도·메모리 개선, 휴면 dateparse 제거 | API 변경, 추출 HTML/Text 변화 가능 | 별도 PR의 최우선 후보 |
| [`go-trafilatura`](https://github.com/markusmobius/go-trafilatura) | 더 풍부한 metadata와 추출 정확도 후보 | 더 느리고 의존성·설정이 크며 자체 benchmark 외 독립 근거 부족 | 실제 corpus에서 현행 fidelity가 실패할 때만 비교 |
| [`go-domdistiller`](https://github.com/markusmobius/go-domdistiller) | Chromium DOM Distiller 계열 | 유지보수 우위가 없고 코드·의존성 규모 증가 | 교체 비추천 |
| `x/net/html` + 사이트별 selector | 추가 추출 의존성 없음, 고정 사이트에서 예측 가능 | 범용 URL의 본문 추출과 신규 사이트 적응 상실 | 입력 사이트가 제한될 때만 가능 |

폐기된 `github.com/go-shiori/go-readability`로 되돌아가면 안 된다. 해당 저장소는 deprecated·archive 상태이며 Readeck 포크를 후속으로 안내한다.

### 9.2 HTML→Markdown

현행 `html-to-markdown/v2`는 GFM 표, 코드 펜스, commonmark 동작을 이미 테스트 계약으로 사용한다. 더 단순하면서 같은 출력 품질과 확장성을 제공하는 성숙한 CGO-free 대체재는 확인하지 못했다.

- `jaytaylor/html2text`: Markdown보다는 Markdown-flavored plain text에 가깝고 복잡한 표·escape·플러그인 계약이 약하다.
- `mattn/godown`: upstream이 WIP로 설명하며 오래된 최소 릴리스다.
- `Article.RenderText`: 의존성은 줄지만 링크·이미지·강조·표·코드 구조가 소실된다.

결론은 현행 유지다.

### 9.3 MCP SDK

[`mark3labs/mcp-go`](https://github.com/mark3labs/mcp-go)는 별 수와 공개 importer가 공식 SDK보다 많을 수 있고 고수준 API가 편리하다. 그러나 비공식 v0 구현이다. context-router는 이미 공식 Tier 1 v1 SDK의 stdio transport, schema 생성, tool annotation, structured content, 테스트 transport를 깊게 사용한다.

비공식 구현으로 교체하면 프로토콜 거버넌스는 약해지고 마이그레이션 범위만 커진다. 현행 공식 SDK를 유지한다.

### 9.4 Starlark

Go 애플리케이션에 바로 대체할 수 있는 다른 canonical Go Starlark 구현은 확인하지 못했다. Python·Rust·Java 구현은 별도 런타임, FFI 또는 프로세스 경계가 필요하며 drop-in 대체재가 아니다.

Goja·Lua 등 다른 스크립트 엔진으로 바꾸면 현재의 hermetic Starlark 언어 계약과 결정성·격리 검증을 다시 설계해야 한다. 현행 유지가 가장 작고 안전하다.

### 9.5 `x/net`과 `x/sys`

- stdlib에는 `x/net/html`과 `html/charset`의 HTML5 parsing·비UTF charset 감지/변환을 함께 대체하는 API가 없다.
- stdlib에는 Windows `LockFileEx`와 Job Object를 동등하게 제공하는 고수준 API가 없다.
- `syscall`은 frozen 상태이므로 새 Windows ABI 코드를 직접 작성하는 것이 개선이 아니다.

라이브러리는 유지하고 버전만 별도 검증한다.

### 9.6 SQLite

| 후보 | 구현·채택 | context-router 적합성 | 판정 |
|---|---|---|---|
| [`modernc.org/sqlite v1.54.0`](https://pkg.go.dev/modernc.org/sqlite) | C→Go 변환, CGO-free, `database/sql`, SQLite 3.53.3, 공개 importer 약 3,518 | 현 DSN, WAL, FTS5, 오류코드, Windows/Linux 단일 바이너리에 정확히 부합 | 유지 |
| [`mattn/go-sqlite3`](https://github.com/mattn/go-sqlite3) | 가장 높은 채택도와 성숙도 | `CGO_ENABLED=1`, GCC 및 cross-C toolchain 필요. FTS5 build tag도 별도 | 현재 요구와 충돌 |
| [`ncruces/go-sqlite3 v0.35.2`](https://pkg.go.dev/github.com/ncruces/go-sqlite3) | WASM→Go, CGO-free, `database/sql`, custom VFS | 연결별 메모리가 더 크고 driver명·DSN·error·FTS5 등록 변경 필요. 현재 [Windows 고동시성 WAL 이슈 #404](https://github.com/ncruces/go-sqlite3/issues/404) 경고 존재 | 지금 교체 금지 |
| [`zombiezen.com/go/sqlite`](https://github.com/zombiezen/go-sqlite) | modernc 엔진 위 저수준 API | `database/sql` 드라이버가 아니고 modernc를 제거하지도 않음 | API 재설계 목적이 아니면 부적합 |
| `glebarez/go-sqlite` | modernc 기반 포크 | upstream modernc보다 유지보수 우위 없음 | 부적합 |

`ncruces`는 #404가 해결된 릴리스 이후 Windows WAL stress와 `PRAGMA integrity_check`, RSS, binary size, build time에서 명확히 이길 때만 다시 검토한다.

## 10. context-router에 맞는 개선 순서

### P1: `go-readability/v2` 별도 PR

가장 근거가 강한 라이브러리 개선이다. PR #2 병합 후 독립 PR로 진행한다.

예상 변경 범위:

- `go.mod`, `go.sum`
- [`internal/netfetch/netfetch.go`](../../../internal/netfetch/netfetch.go)
- 관련 netfetch fixture와 benchmark
- 변경된 전이 의존성에 따른 `THIRD-PARTY-NOTICES` 또는 `NOTICE` 재검증

필수 검증:

- 기존 HTML fixture의 제목·본문·추출 여부 비교
- 빈 본문, 500자 경계, 텍스트 30%, `pre/code` 50% fidelity fail-open
- 상대 링크·이미지 URL
- 코드 펜스와 GFM 표
- EUC-KR/meta charset
- 대표 HTML의 CPU·allocation benchmark
- Windows/Linux `CGO_ENABLED=0` build

성능 우위가 실측되지 않더라도 tagged v2와 유지되는 전이 의존성이라는 유지보수 이점은 있다. 그러나 출력 회귀가 있으면 현행 v0 유지가 우선이다.

### P2: `x/net`·`x/sys` 버전 업데이트 별도 PR

readability 변경과도 분리한다.

- `x/net v0.57.0`: HTML parser·charset fixture 검증
- `x/sys v0.47.0`: Windows LockFileEx·Job Object 테스트 및 Windows cross-build
- MVS로 함께 바뀌는 `x/text` 등 간접 모듈을 검토
- `go mod tidy` 이후 NOTICE와 checksum 변경 확인

현재 버전이 잘못되거나 차단급으로 취약하다는 근거는 없으므로 낮은 우선순위 유지보수 작업이다.

### P3: 새 의존성 없는 DOM 파싱 최적화

새 라이브러리를 추가하기 전에 이미 설치된 API로 중복 파싱을 줄일 수 있는지 측정한다.

후보 흐름:

`html.Parse → readability.FromDocument → converter.ConvertNode`

현재 흐름은 readability, fidelity 검사, Markdown 변환에서 HTML을 최대 2~3회 parse할 가능성이 있다. DOM 재사용은 결과 동등성 fixture와 benchmark가 확인될 때만 반영한다. readability v2 전환과 한 PR에 섞지 않는다.

### 추가하지 않을 항목

- ORM/GORM
- 별도 FTS 검색 엔진
- 브라우저 또는 JavaScript Readability 런타임
- 범용 file-lock 패키지
- 두 번째 MCP 구현
- Starlark를 대체하는 별도 스크립트 런타임

현재 단일 바이너리·최소 의존성·보안 경계보다 이득이 작다.

## 11. PR #2 포함 여부 결정표

| 변경 후보 | PR #2 포함 | 이유 | 별도 작업 조건 |
|---|---|---|---|
| 이 조사 문서 | 예 | 동작·의존성 그래프를 바꾸지 않고 리뷰 판단 근거만 추가 | 없음 |
| `go-readability` → `/v2` | **아니오** | major import/API·출력 변화, fixture/benchmark 필요 | PR #2 병합 후 독립 PR |
| `x/net v0.57.0` | **아니오** | 현재 버전도 유효하며 PR #2 차단 문제가 아님 | HTML·charset 회귀 테스트가 있는 유지보수 PR |
| `x/sys v0.47.0` | **아니오** | Windows 저수준 동작의 원인 분리가 필요 | Windows 테스트·cross-build 전용 유지보수 PR |
| 간접 모듈 4개 개별 승격 | **아니오** | 직접 관리 대상이 아니며 상위 모듈/MVS와 분리하면 호환 위험 | 직접 모듈 업데이트 결과로만 수용 |
| `modernc/sqlite` 교체 | **아니오** | 현행이 최신이며 context-router 계약에 가장 적합 | 실제 RSS·build·WAL 무결성 우위와 ncruces #404 해결 후 재평가 |
| MCP SDK 교체 | **아니오** | 공식 Tier 1 SDK를 비공식 v0 구현으로 낮출 이유 없음 | 공식 SDK로 충족 불가능한 필수 기능이 생길 때만 |
| 새 라이브러리 추가 | **아니오** | D8 최소 의존성과 PR #2 범위를 위반 | 기존 도구·stdlib로 해결 불가한 필수 요구가 입증될 때만 |

### Claude Code가 같은 PR 포함을 제안할 때 요구할 근거

아래 질문에 모두 `예`여야 한다.

- [ ] PR #2의 현재 요구사항을 충족하는 데 반드시 필요한가?
- [ ] 현행 라이브러리/API로 수정할 수 없는가?
- [ ] 변경하지 않으면 재현되는 실패나 보안·무결성 문제가 있는가?
- [ ] 변경 범위가 한 직접 의존성과 필요한 호출부로 제한되는가?
- [ ] 출력·프로토콜·DB 호환성 fixture가 준비됐는가?
- [ ] Windows/Linux `CGO_ENABLED=0` build가 통과하는가?
- [ ] `go mod verify`, `go mod tidy` diff, NOTICE/THIRD-PARTY 고지를 검토했는가?
- [ ] 의존성 변경을 독립 commit으로 분리해 쉽게 revert할 수 있는가?

하나라도 `아니오`이면 별도 PR로 분리한다.

## 12. Claude Code PR #2 리뷰 체크리스트

### 라이브러리 관련

- [ ] PR diff에 의도하지 않은 `go.mod`/`go.sum` 변경이 없는지 확인
- [ ] 직접 의존성 7개 외 신규 direct module이 추가되지 않았는지 확인
- [ ] `modernc.org/libc v1.74.1`이 sqlite v1.54.0의 요구값과 어긋나지 않는지 확인
- [ ] `go.starlark.net`을 GitHub import path로 잘못 바꾸지 않았는지 확인
- [ ] `github.com/go-shiori/go-readability`로 회귀하지 않았는지 확인
- [ ] Windows 코드가 `x/sys/windows` 대신 frozen `syscall` 또는 수제 ABI로 확장되지 않았는지 확인
- [ ] SQLite driver명, DSN, FTS5, BUSY/LOCKED 오류 처리 계약이 유지되는지 확인
- [ ] MCP transport·schema·structured error 계약이 유지되는지 확인

### 같은 PR 포함 판단

- [ ] 리뷰 지적이 라이브러리 변경 없이 코드에서 고칠 수 있는지 먼저 확인
- [ ] 단순 최신화·성능 기대·정리 목적이면 별도 PR로 분리
- [ ] dependency 변경이 필요하면 exact release note와 API diff를 확인
- [ ] 직접 의존성 변경으로 생긴 간접 모듈 변경만 수용
- [ ] NOTICE와 THIRD-PARTY 고지를 실제 선택 버전 기준으로 재검증

## 13. 수행한 검증과 결과

조사 중 실행한 최소 검증은 다음과 같다.

```powershell
go mod verify
# 결과: all modules verified

$env:CGO_ENABLED='0'
go list ./...
# 결과: 프로젝트 9개 패키지 해석 성공

$env:CGO_ENABLED='1'
go list -deps -f '{{if .CgoFiles}}{{.ImportPath}} {{join .CgoFiles ","}}{{end}}' ./...
# 결과: 출력 없음(CgoFiles 0건)

$env:CGO_ENABLED='0'
$env:GOOS='windows'
$env:GOARCH='amd64'
go build ./...
# 결과: 성공

$env:GOOS='linux'
go build ./...
# 결과: 성공
```

추가 확인:

- 직접 의존성 7개 모두 프로덕션 import 확인
- `go.mod` 26개 버전의 canonical 형식·tag/commit·major suffix·retract 확인
- 공식 릴리스·pkg.go.dev·원본 저장소 교차 확인
- 서브에이전트 7개 조사 범위 모두 완료 및 종료
- 조사 당시 파일이나 의존성 변경 없음

이 문서 작성 전 작업 트리는 clean이었으며, 문서 작성으로 추가되는 변경은 이 Markdown 파일 하나다.

## 14. 최종 권고

Claude Code는 PR #2에서 현행 의존성 7개를 전제로 코드·테스트·고지의 정확성을 리뷰한다. 라이브러리 최신화나 대체는 리뷰 지적을 고치는 수단으로 먼저 선택하지 않는다.

PR #2의 의존성 결정은 다음 한 줄로 요약한다.

> **PR #2는 현행 `go.mod`를 유지하고, `go-readability/v2`와 `x/net`·`x/sys` 업데이트는 각각 검증 가능한 별도 PR로 분리한다.**

결정 상태: 차단 근거가 새로 확인되지 않으면 PR #2의 현행 의존성을 유지한다.
