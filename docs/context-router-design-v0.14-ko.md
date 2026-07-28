# Context Router v0.14 설계서 (D77–D79 — 잠금 보유 재측정·.ps1 종료 신호 완결·lint 미검출 집합)

델타 체인: `-v0.0.1-ko.md` → `-v0.1-ko.md`(D14–D20) → `-v0.2-ko.md`(D21–D28) →
… → `-v0.12-ko.md`(D63–D67) → `-v0.13-ko.md`(D68–D76) → **이 문서(D77–D79)**.
D76까지의 결정은 이전 문서들이 소유하며 여기서 되풀이하지 않는다.

## 근거가 된 관측 (session-40, 2026-07-28)

이 릴리스의 항목은 전부 **v0.13 설계 §4가 남긴 것**이다. 새로 발굴한 것은 없다.

- **기동 purge의 잠금 보유 창이 훅의 색인 갱신을 막는다.** `PurgeHookOnlyOlderThan`의
  두 번째 단계 `reclaimHookBlobs`는 `internal/store/store.go:1222`에서
  `lockStoreCtx(ctx, s.dir)`로 advisory lock을 잡고 파일 회수 구간 내내 보유한다
  (`cmd/context-router/main.go:402`가 "파일 회수로 lockStore를 잡는데"로 적고
  있다). 훅의 Read 가드가 총예산 2000ms(`internal/hook/hook.go:60`의
  `defaultDeadlineMS`) 안에서 writable `OpenContext`를 할 때 기다리는 것이 바로
  그 락이다(`store.go:119`). 대기 자체는 `lockStoreCtx`가 ctx를 감시하므로 예산
  안에 포기할 수 있다 — **문제는 그 창 동안 훅이 색인을 포기하고 가드를 조용히
  통과시킨다는 것**이고(`main.go:403-404`), 창의 크기를 묶는 것이
  `startupPurgeMaxHashes`다(`main.go:401-410`). 그 값을 정당화하는 실측
  (632·633ms)은 D73 색인 도입 **이전** 값이다(`main.go:418-420`).
- **회수 경로는 나이 게이트 뒤에 있다.** `reclaimHookBlobs`의 판정은
  `store.go:1247`의 `statErr != nil || time.Since(fi.ModTime()) < gcOrphanMinAge ||
  s.stillReferenced(ctx, h)`이고 `gcOrphanMinAge`는 1시간이다(`store.go:809`).
  갓 만든 픽스처의 CAS 파일은 mtime이 신선해 두 번째 논리합에서 단락되므로
  **`stillReferenced`(해시당 reader 쿼리)도 `os.Remove`도 실행되지 않는다** —
  632·633ms를 만든 해시당 비용이 정확히 그 둘이다. D77의 픽스처는 이 게이트를
  넘겨야 의미가 있다(§2.1).
- **D76 보강은 stderr가 비어 있을 때만 나간다.** `internal/exec/exec.go:187`의
  조건은 `.ps1` 러너 · `exit_code == 0` · **`resp.Stderr == ""`** 셋이다. 원래
  비어 있던 자리에만 넣어 사용자 출력과 섞이지 않게 한 D76의 의도적 설계다.
  D78은 이 조건을 바꾸지 않으므로, **오류를 낸 스니펫에서는 사이드파일이 정확해도
  보강 줄이 나가지 않는다** — 그래서 D78의 감시선은 보강 줄이 아니라 **사이드파일
  값**을 기준으로 세운다(§2).
- **D79의 잔여 미검출 집합은 정확히 세 파일이다.** `//go:build` 선언 16개를 전수
  조사한 결과 어느 lint 축에도 안 잡히는 것은 `internal/sandbox/launcher_darwin.go`
  (darwin) · `internal/sandbox/launcher_other.go`(`!windows && !linux && !darwin`) ·
  `internal/search/perf_test.go`(perf 태그) 셋이다. `!windows` 선언 다섯 파일
  (`realpath_other.go`·`run_unix.go`·`run_unix_test.go`·`store_lock_unix.go`·
  `worker_unix.go`)은 ubuntu 런에 **이미 포함된다**.
- **`!windows && !linux && !darwin` 갈래의 대표는 아무 GOOS나 되지 않는다.**
  lint 축을 더하려면 그 GOOS에서 **전 패키지가 빌드**돼야 한다 — 그렇지 않으면
  golangci-lint가 `typechecking error`와 함께 `0 issues.`를 내며 종료 코드만
  실패한다. `freebsd`는 불가하고(`worker_unix.go`가 딸려 들어오는데 그 GOOS의
  `syscall.Rlimit`는 `Cur`/`Max`가 int64다), `openbsd`도 불가하며
  (`syscall.RLIMIT_AS` 미정의), **`netbsd`가 소스 변경 0으로 성립한다**(§3 표7).

### 설계 체크포인트 교차 검수가 바꾼 것 (2026-07-28, 5라운드)

이 문서의 초안은 **다섯** 결정이었고 tail 승격 수단은 `try/finally`였다. 다섯
라운드의 검수(서브에이전트 반증 시도 + Codex 2패스)가 **결정 둘을 내리고, 남은
하나의 메커니즘을 통째로 교체한 뒤 그 교체본을 세 번 고쳤고, 마지막에 다른 두
결정에서 구현 차단 문제를 찾았다**. critical은 5 → 2 → 1 → 1 → 1로 줄었다.
초안의 결론을 지우지 않고 여기에 남긴다 — 왜 지금 형태인지가 이 기록에 있다.
반증 실측은 §3 표3~표7에 있다.

**1라운드 — 초안 결정 둘의 전제가 무너졌다**

1. **초안 D77(`OpenContext` 취소 전파)의 근거 둘이 틀렸다.** ① 다른 커넥션이
   쓰기 락을 쥔 상태에서도 기존 색인의 `CREATE INDEX IF NOT EXISTS`는 **14ms에
   nil로 반환**한다. ② 설령 발생해도 ctx로 끊을 수 없다 — 200ms 데드라인으로
   불러도 반환까지 5.03s가 걸리고 **오류 분류만** 바뀐다. → **범위에서 내린다**(§4).
2. **초안 D79(windows dotnet 억제)의 배너가 재현되지 않았다.** 배너는
   `DOTNET_CLI_HOME`을 냉각했을 때만 났다. v0.13 §4의 서술은 **코드 이력**에
   근거했지 동작 실측이 아니었다. → **범위에서 내린다**(§4).
3. 폴백을 `param(` 정규식 하나로 판정한 것에 미탐이 실재한다 → 미니 파서로 넓혔다.
4. "tail이 항상 위조값을 덮는다"는 주장을 **철회**했다.

**2라운드 — 승격 수단 교체**

5. **"기동 purge는 advisory lock을 쥐지 않는다"는 서술이 틀렸다** —
   `reclaimHookBlobs`가 회수 구간에서 같은 락을 다시 잡는다. → 서사를 "예산 초과
   위험"에서 "훅이 색인을 포기하는 창의 크기"로 정정했다.
6. **`try/finally`가 문 종료 오류의 제어 흐름을 바꾼다** — `41` + 명령 미발견 +
   `69`는 감싸지 않으면 사이드파일 69인데 래핑하면 41이고, **종료 코드가 양쪽 다
   0이라 종료 코드 단정으로는 검출되지 않는다**. → `Register-EngineEvent`로 교체.
7. 명명 블록(`begin`/`process`/`end`)이 폴백 토큰 목록에 없었다 → 넣었다.

**3라운드 — 교체본의 부작용**

8. **`Register-EngineEvent -Action`이 세션 작업표에 `PSEventJob`을 남긴다.**
   `Get-Job | Wait-Job`이 **끝나지 않고** `Get-Job | Remove-Job -Force`가
   **사이드파일을 지운다**. → **`-SupportEvent`**를 더했다.
9. **등록 문을 앞 줄에 두면 모든 오류의 줄 번호가 +1 밀린다.** → 스니펫 첫 줄에
   **인라인으로 잇는다**. 검수는 이 회피를 `#requires` 무력화 우려로 기각했으나
   실측이 그 근거를 반증했다(4형태 전부 동일).
10. 명명 블록 `dynamicparam`이 빠져 있었다 → 넣었다.
11. "`param`이 정상 바인딩된다"는 서술이 틀렸다 → 기전을 정정했다.

**4라운드 — 인라인의 경계**

12. **인라인이 줄 번호를 보존하는 것은 오류가 2행 이상일 때다.** 1행 오류에서는
    열 번호가 밀리고 등록 문 꼬리가 소스 줄 에코에 실린다. 한 줄 스니펫은 전부
    여기 걸린다. → 인라인은 **유지**(앞 줄은 더 넓은 회귀)하고 한계를 §1.2에 적고
    §2.6 단정을 두 형태로 나눴다.
13. **`-SupportEvent`가 값 조작 경로를 좁혔다** — 숨김 구독이라 `-Force` 없는
    `Unregister-Event`는 실패한다. → 열거를 `-Force` 형태로 좁혔다.
14. 폴백 경로의 미보강 원인이 `exit`뿐이 아니다(`throw`도) → 넓혔다.

**5라운드 — 지금까지 검수가 D78에 쏠린 사이 다른 둘에 남아 있던 것**

15. **`GOOS=freebsd` 축은 CI에 넣는 즉시 영구 적색이다.** 깨지는 파일은 D79가
    겨냥한 셋이 아니라 `!windows`라 딸려 들어오는 `internal/transform/worker_unix.go`
    이고, **§1.3의 스코프 확인(1단계)은 통과한다** — 1단계가 초록인데 2단계가
    다른 패키지에서 죽는 형태다. → 대표를 **`netbsd`**로 바꿨다(소스 변경 0,
    §3 표7). 판정 1단계에 **전 패키지 빌드 확인**을 선행 조건으로 넣었다.
16. **D77의 픽스처가 종이 게이트가 될 뻔했다.** 규모 하한만 지정하면 갓 만든
    픽스처가 `gcOrphanMinAge` 단락으로 유예 경로만 재고, 원 실측을 만든 해시당
    비용(reader 쿼리 + unlink)이 측정에서 빠진다. → §2.1에 **경로 조건**을 더했다
    (mtime 소급 + `ReclaimedB > 0` · `DeferredFiles == 0` 단정).
17. **임계 1000ms의 여유가 1.58배뿐이다.** 형태를 따르라고 지정한 D73 §2⑦은
    여유가 195~583배라 CI에서 견딘다(오늘 재실측 8.59ms/25.65ms). D77은 3-OS
    `./...`와 `-race`로 **4회** 도는데 §1.3이 탈출구를 다 막아 러너 편차 한 번이
    BLOCKED가 된다. → 임계는 유지하고 **픽스처 규모를 개발기 실측이 임계의 1/5
    이하가 되도록** 잡기로 했다(§2.1). 잡는 것은 해시당 비용의 회귀이며, 100해시
    절대 상한은 합성 픽스처로 애초에 검증할 수 없다(§0 D77이 이미 적은 바).
18. **미니 파서가 선두 줄 연속 문자(백틱)를 넘지 못한다** — 백틱 + 개행 뒤의
    `using namespace`가 승격되어 exit 0 → 1, 본문·사이드파일이 사라진다.
    → 3단계 안전 규칙에 백틱을 더했다.
19. **등록 문의 `sidePath` 작은따옴표 이중화가 문서에 없었다.** 현재 코드는
    `exec.go:288`에서 반드시 이중화한다. 문면대로 다시 쓰면 그 처리가 사라져
    사용자명에 `'`가 있는 호스트에서 **모든 shell 스니펫이 ParserError**가 된다 —
    D76보다 넓은 회귀다. → §0 형태와 §2 단정에 넣었다.

검수가 **확인해 준 것**: D77의 수치 인용은 전부 실제와 일치했고, `file:line`
전수 대조가 다섯 라운드 내내 일치했으며, 미니 파서 절차는 적대 입력을 포함한
22개 입력에서 미탐 0건이었고(백틱은 그 집합 밖이었다), D77의 측정 지점은 실재한다
(store 테스트가 in-package이고 두 잠금 백엔드가 OFD/핸들 단위라 같은 프로세스
관측이 실제로 경합한다), darwin·perf 축은 오늘 그대로 초록이다(§3 표7).

## 0. 결정 이력 (v0.14 신규 — D76까지는 v0.13 설계서·이전 델타 체인)

- **D77** `PurgeHookOnlyOlderThan` 실 잠금 보유 시간 재측정 + 자동 게이트:
  행 삭제와 파일 회수를 포함한 실 보유 시간을 D73 색인 도입 **이후** 값으로 다시
  재고, 회귀를 잡는 게이트를 남긴다. 재는 구간에는 `reclaimHookBlobs`가 advisory
  lock을 보유하는 회수 구간이 **포함된다**.
  - **이 재측정의 무게는 검수 뒤 올라갔다.** 초안 D77(직접 차단)이 성립하지
    않음이 확인됐으므로, `startupPurgeMaxHashes`로 창을 묶는 것이 **훅의 색인
    갱신 기회를 지키는 유일한 장치임이 확정됐다**.
  - **게이트 형태는 D73 §2 ⑦을 따른다** — `elapsed >= budget`이면 `Fatalf`로
    실패하는 단정이며 수동 판독이 아니다. **임계값은 절대 1000ms**다(훅 예산
    2000ms의 50%, `main.go:406`의 관측 상한 표현 "1/3~2/5"(≤800ms) 위 25% 여유).
  - **픽스처는 두 조건을 함께 만족해야 한다.**
    · **경로 조건** — 블롭 mtime을 `gcOrphanMinAge` 이전으로 소급시켜 실제 회수
    경로를 타게 한다. 그러지 않으면 유예 경로만 재는 종이 게이트가 된다(§관측).
    · **규모 조건** — 개발기 실측이 **임계의 1/5 이하**가 되도록 해시 수를 잡는다.
    여유 1.58배는 3-OS·`-race`·공유 러너에서 성립하지 않는다(§관측 17).
  - **그래서 이 게이트가 잡는 것은 "해시당 비용의 회귀"다.** `startupPurgeMaxHashes`
    (100해시) 배치의 절대 보유 시간은 합성 픽스처로 검증할 수 없다 — 원 실측
    632·633ms는 실 도그푸딩 저장소(아티팩트 1254개·210MiB)의 값이고 분포가 다르다.
    비교가 아니라 **상한 감시**가 목적이며, 그 한정을 여기 적어 둔다.
  - v0.13 §3 미측정 (c)의 한계를 그대로 물려받는다.
- **D78** `.ps1` 종료 신호를 `Register-EngineEvent`로 승격 + 최상단 전용 구문
  폴백: `snippetContent`(`internal/exec/exec.go:278`)가 만드는 스크립트를
  "스니펫 + tail"에서 "**등록 문 + `; ` + 스니펫**"으로 바꾼다. 형태는 이렇다
  (BOM 뒤, 한 줄로 이어서):

  ```
  Register-EngineEvent -SourceIdentifier PowerShell.Exiting -SupportEvent -Action { try { [System.IO.File]::WriteAllText('<sidePath>', 'ctr-exit-code:' + [string]$LASTEXITCODE) } catch {} }; <스니펫>
  ```

  `<sidePath>`는 **작은따옴표를 이중화한 값**이다(`strings.ReplaceAll(sidePath,
  "'", "''")` — 현재 `exec.go:288`이 tail에 하는 처리와 같다). 이 처리가 빠지면
  사용자명에 `'`가 있는 호스트에서 스크립트 자체가 파싱되지 않아 **모든 shell
  스니펫이 실패한다**. 스크래치 경로는 `os.TempDir()` 하위이므로 windows에서
  사용자 프로필 이름이 그대로 들어온다(`main.go:514`).
  - **세 가지가 각각 하나의 실측된 문제를 막는다.**
    · **감싸지 않는 것** — `try/finally`는 문 종료 오류의 제어 흐름을 바꾼다
    (§관측 6).
    · **`-SupportEvent`** — 없으면 세션 작업표에 `PSEventJob`이 남아
    `Get-Job | Wait-Job`이 끝나지 않고, 작업 표가 **stdout에도 찍힌다**(§관측 8).
    부수 효과로 값 조작 난이도도 올라간다(§관측 13).
    · **`; `로 첫 줄에 잇는 것** — 앞 줄에 두면 **모든** 오류의 줄 번호가 +1
    밀린다(§관측 9). 인라인은 **2행 이상 오류에서 stderr를 바이트 단위로
    보존**하고, 1행 오류에서는 줄 번호만 정확하다(§관측 12 · §1.2 한계).
  - **닫히는 공백 둘**(v0.13 §1.2에서 제거한다):
    ① "스니펫이 `exit`으로 끝나면 tail은 실행되지 않는다"
    ② "인수 없는 `exit`은 0을 반환해 네이티브 실패가 가려진다".
  - **보강 조건은 D76 그대로다.** `exec.go:187`이 `.ps1` · `exit_code == 0` ·
    `stderr == ""`일 때만 보강 줄을 낸다 — 그래서 §2의 감시선은 보강 줄이 아니라
    **사이드파일 값**을 본다.
  - **승격 대상 스니펫에서 종료 코드는 바뀌지 않는다**(§3 표4). 표4의 명명 블록·
    `using` 행은 종료 코드가 바뀌지만 둘 다 폴백 대상이라 승격되지 않는다.
    이벤트 핸들러는 `$LASTEXITCODE`를 직접 읽으므로 D76 tail이 쓰던 `Get-Variable`
    경유가 사라진다(`global:` 한정자는 필요 없다 — §3 표5).
  - **폴백 판정은 미니 파서로 한다.** 절차는 이렇다 — 적용 대상은 **BOM을 붙이기
    전의 원본 코드**다:
    1. 선두 U+FEFF가 있으면 제거한다(그러지 않으면 공백 판정이 BOM에서 멈춰
       모든 감지가 실패한다).
    2. 앞에서부터 공백과 주석을 소비한다. 판정은 **현재 위치의 첫 문자로만** 한다
       — `<`이고 다음이 `#`이면 블록 주석(`#>`까지 소비), `#`이면 라인 주석(줄
       끝까지 소비). 이 좌→우 우선순위가 없으면 `# ... <#` 형태에서 구현이
       갈린다. 닫히지 않은 `<#`는 **EOF까지 소비**한다(그 입력은 어느 형태로도
       ParserError이므로 결과가 같다). PowerShell 블록 주석은 중첩되지 않으므로
       중첩 처리를 하지 않는다. `#requires`는 라인 주석 형태라 여기서 함께
       소비되며, 승격되어도 지시자가 그대로 작동한다(§3 표6).
    3. 남은 첫 실질 토큰을 `[A-Za-z_][A-Za-z0-9_]*`로 끊는다. 그 **전체**가
       `param` · `using` · `dynamicparam` · `begin` · `process` · `end` · `clean`
       중 하나(대소문자 무시)이면 **승격하지 않는다**. 첫 문자가 `[`(속성 선언
       시작)이거나 **백틱 U+0060**(줄 연속)이어도 승격하지 않는다 — 백틱은 2단계가
       소비하지 않으므로 그 뒤의 최상단 전용 구문이 그대로 승격되는 미탐 경로였다
       (§관측 18).
    4. 그 외에는 승격한다.
  - **과잉 폴백은 허용한다.** `[int]$x = 5`처럼 속성이 아닌 `[`로 시작하는 문장,
    `end`를 명령명으로 쓰는 문장, 백틱으로 시작하는 임의 문장도 폴백되지만 그것은
    기존 동작 유지다. **미탐만이 회귀**이며, 이 비대칭이 판정을 넓게 잡는 이유다.
  - **위조 무력화를 주장하지 않는다.** 이벤트 방식은 D76 tail이 뚫렸던 경로 하나
    (`Get-Variable` 재정의)를 막고 `-SupportEvent`가 또 하나를 좁혔지만, 스니펫이
    값을 정하는 경로가 셋 남는다(§3 표5) — **`Unregister-Event -Force`** ·
    `$LASTEXITCODE` 대입(최상단 대입만으로 충분) · **스니펫 자신의
    `PowerShell.Exiting` 핸들러 등록**(같은 소스 식별자여도 충돌 없이 공존하고
    스니펫 쪽이 이긴다). 새 권한은 아니다 — 스니펫은 자기 `stderr`로 같은 줄을
    직접 낼 수 있고 `exit_code`도 불변이다. 정보 정확성 문제다.
  - 마커 판별(`exec.go:258`)과 읽기 상한(`:206`)은 불변이다. nonce를 도입하지 않는다.
  - **기각한 대안 둘**(§3 표2·표4): `try/finally`(오류 흐름 변경) · dot-source
    래퍼(`exit` 종료 코드와 `$LASTEXITCODE` 오염).
  - **남는 한계**(§1.2에 유지):
    · `[Environment]::Exit(N)`은 이벤트를 발화시키지 않는다.
    · 폴백 경로는 등록 문이 없으므로 기존 공백을 그대로 갖는다.
    · 위 세 조작 경로.
    · **스니펫 첫 줄에서 난 오류는 열 번호가 등록 문 길이만큼 밀리고 등록 문
    꼬리가 오류의 소스 줄 에코에 실린다**(줄 번호는 정확하다). 한 줄짜리 스니펫은
    모든 오류가 여기 걸린다. 에코 길이가 콘솔 폭 의존이라 차이가 환경 의존적이다.
    · `using namespace`로 시작하는 스니펫은 폴백되지 않으면 ParserError로 본문도
    사이드파일도 사라진다(그래서 폴백 대상이다).
- **D79** lint 미검출 집합 비우기: lint 잡에 `GOOS=darwin` · **`GOOS=netbsd`** ·
  `--build-tags perf` 세 축을 더한다.
  - D69가 `GOOS=windows` 축을 세우고 잔여를 §4로 이월했다. 그 잔여를 전수
    조사해 **세 파일로 확정**했다(위 관측).
  - **대표 GOOS의 선행 조건은 "그 GOOS에서 전 패키지가 빌드된다"이다.**
    `freebsd`는 이 조건을 만족하지 않는다 — `!windows`라 딸려 들어오는
    `internal/transform/worker_unix.go`가 그 GOOS의 `syscall.Rlimit`(Cur/Max가
    int64) 때문에 컴파일되지 않고, golangci-lint는 `typechecking error`와 함께
    `0 issues.`를 내며 종료 코드만 실패한다. `openbsd`도 불가하다
    (`syscall.RLIMIT_AS` 미정의). **`netbsd`는 소스 변경 0으로 성립하고 같은
    `!windows && !linux && !darwin` 갈래이므로 미검출 집합을 동일하게 닫는다**
    (§3 표7).
  - **D69의 판정 2단계를 새 축에도 적용하되, 1단계에 빌드 확인을 선행시킨다** —
    ① 그 GOOS/태그에서 **전 패키지가 빌드되는지** 확인하고, ② 대상 파일이 린터의
    보고 범위에 있는지 확인하고, ③ 그 뒤의 통과/실패를 결과로 받는다. ①이 없으면
    스코프 확인은 초록인데 lint가 **다른 패키지**에서 죽는다(§관측 15).
  - **perf 축의 스코프 확인은 형태가 다르다.** D69가 쓰는 관용구는
    `.github/workflows/ci.yml:128`의 `go list -f '{{join .GoFiles "\n"}}'`인데
    `.GoFiles`는 `_test.go`를 포함하지 않아 `internal/search/perf_test.go`를 볼
    수 없다. perf 축은 `-tags perf`와 `.TestGoFiles`/`.XTestGoFiles`를 쓴다.
  - `.golangci.yml`이 남긴 결합 경고는 새 축에도 그대로 적용된다 — 주석을 새
    축까지 포함하도록 갱신한다.

## 1. v0.14 제품 계약

### 1.1 범위와 배송

세 결정을 세 계층으로 배송한다. **계층 간 의존이 없다** — 서로 다른 파일을
건드리므로 순서는 자유롭고, 아래는 위험이 큰 순이다.

| 순서 | 계층 | 결정 | 주 파일 |
| --- | --- | --- | --- |
| 1 | exec | D78 | `internal/exec/exec.go` |
| 2 | store | D77 | `internal/store/` 테스트 · `cmd/context-router/main.go`(주석) |
| 3 | CI | D79 | `.github/workflows/ci.yml` · `.golangci.yml` |

**세 결정 모두 Go 제품 소스의 동작을 바꾸지 않는다** — D78만 `exec.go`의 스크립트
조립을 바꾸고, D77은 테스트와 주석, D79는 CI 구성이다. `internal/transform`은
어느 결정의 대상도 아니다(대표 GOOS를 `netbsd`로 고른 이유가 그것이다).

릴리스 마감은 `CHANGELOG.md`의 `[0.14.0]` 절과 `productVersion` 0.14.0이다.

### 1.2 명시적 비범위 (v0.14)

- **`OpenContext` 경로의 취소 전파** — 전제 둘이 반증됐다(§4).
- **windows shell 갈래의 dotnet first-run 억제** — 배너가 재현되지 않았다(§4).
- **`worker_unix.go`의 GOOS 이식성** — `freebsd`/`openbsd`에서 컴파일되지 않지만
  둘 다 지원 대상이 아니고, D79는 대표를 `netbsd`로 골라 이 변경을 피한다.
  RLIMIT_AS 자기적용은 ci.yml의 ubuntu/macos 런이 실증하는 격리 시행점이라
  건드리려면 그 실증을 함께 재검토해야 한다.
- **`[Environment]::Exit(N)` 경로의 신호** — 스크립트 수준 수단이 없다.
- **폴백 경로 스니펫의 신호 승격** — AST 파싱이 선행된다.
- **첫 줄 오류의 열 번호·에코 오염** — 인라인 배치의 대가다. 등록 문을 앞 줄로
  옮기면 모든 오류의 줄 번호가 밀리므로 더 넓은 회귀가 된다.
- **사이드파일 값 조작 경로의 방어** — 격리 계층이 필요하다.
- **보강 조건 완화** — `stderr == ""` 요건은 D76 결정이며 D78이 바꾸지 않는다.
- **`startupPurgeMaxHashes` 값 변경** — D77은 재측정과 게이트까지다.
- **100해시 배치의 절대 보유 시간 검증** — 합성 픽스처로는 성립하지 않는다(§0 D77).
- **v0.13 §4에서 이월 유지**: FTS 인덱스 구성 재검토 · `indexed_at` 단독 색인과
  `PurgeOlderThan` 경로 측정 · doctor `[12]` 로그 로테이션 ·
  `ctr_batch_execute` 재도입(D58) · Windows AppContainer FS 제한(D59) · 타인
  배포 채널·MCP 설치 표준.

### 1.3 선행 게이트

각 결정의 **첫 스텝**에서 확인한다. "유리한 결과가 나올 때까지 픽스처를 고치지
않는다"는 규율의 적용점이며, 이 규율은 이번 검수에서 실제로 두 결정을 내리고 한
메커니즘을 교체한 뒤 그 교체본을 세 번 고쳤다 — 게이트는 장식이 아니다.

1. **D77 픽스처** — 경로 조건(회수 경로를 실제로 탄다)과 규모 조건(개발기 실측이
   임계의 1/5 이하)을 **둘 다** 만족해야 착수한다. 규모를 맞추려고 경로 조건을
   버리면 종이 게이트가 되고, 경로를 살리려고 규모를 키우면 CI에서 flaky가 된다.
   개발기 실측이 임계를 넘기면 BLOCKED다.
2. **D78 폴백** — 폴백되어야 하는 입력 집합(§2.5)이 전부 폴백 경로로 가고
   baseline과 동일하게 동작해야 한다. 그중 미탐이 실증된 것은 넷
   (`<# doc #> param($x)` · `[CmdletBinding()] param($x)` ·
   `using namespace System.Text` · 백틱 줄 연속 + `using`)이고 나머지는 회귀
   방지용이다.
3. **D79 축** — 세 축 각각에서 ① 전 패키지가 빌드되고 ② 대상 파일이 린터의 보고
   범위에 들어가는 것을 순서대로 확인한 뒤에 lint 결과를 받는다. ①이 실패하면
   그 축은 성립하지 않는다.

## 2. 검증·테스트 계약

각 항목은 **"검사를 추가했다"가 아니라 "그 검사가 물린다"** 를 증거로 요구한다.
감시선을 만드는 항목은 회귀를 유도해 FAIL을 캡처하고 원복해 PASS를 캡처한다.
D78의 단정은 보강 줄이 아니라 **사이드파일 값**을 본다 — 보강은 `stderr == ""`
일 때만 나가므로(§관측) 오류가 있는 케이스에서는 보강 줄이 없다.

1. **D77 게이트** — `elapsed >= 1000ms`면 `Fatalf`. 다음을 함께 요구한다.
   · **경로 단정**: 블롭 mtime을 `gcOrphanMinAge` 이전으로 소급시키고(기존 헬퍼
   `ageBlobFile`, `store_test.go:1854`가 그 관용구다), 측정 대상이 실제 회수
   경로였음을 같은 테스트가 단정한다 — `rep.ReclaimedB > 0` **그리고**
   `rep.DeferredFiles == 0` **그리고 `rep.FailedFiles == 0`**. 이 줄들이 없으면
   어떤 경로를 쟀는지 사후에 판별할 수 없다. `FailedFiles`는 구현 단계 검수가
   더한 항이다(§3) — 빼면 rename/unlink 실패로 일부 해시만 회수돼도 앞의 두
   조건이 성립해 **줄어든 측정량**을 감춘 채 넉넉한 여유를 보고한다.
   `rep.Hashes`까지 넷이 맞아야 전 해시가 회수 경로를 밟았음이 함의된다.
   · **규모**: 개발기 실측이 임계의 1/5 이하가 되는 해시 수를 고르고 그 수를
   테스트에 고정한다. 픽스처 규모(행 수·artifact 수·해시 수)를 로그에 남긴다.
   · **실행 위치**: 이 게이트는 `go test ./...`에 포함되므로 CI에서 3-OS와
   `-race`로 **네 번** 돈다. 그 전제로 규모를 잡는다(`-race`에서 skip하지 않는다).
   · **진단**: `Fatalf` 문면에 `rep.Hashes`·`rep.ReclaimedB`·`rep.DeferredFiles`·
   `rep.FailedFiles`와 개발기 기준값을 함께 찍어 "회귀인가 러너 편차인가"를 가를
   수 있게 한다.
   · **유도 FAIL**: 임계를 일시적으로 낮춰 게이트가 실제로 실패하는 것을 한 번
   캡처하고 원복한다.
2. **D78 종료 코드 동일성** — 승격 대상 네 케이스(`exit N`·정상·`throw`·인수 없는
   `exit`)에서 승격 전후 종료 코드가 같다.
3. **D78 본문 완주와 값 동일성** — 문 종료 오류가 있는 스니펫
   (`cmd /c exit 41` + 명령 미발견 + `cmd /c exit 69`)에서 **마지막 문장이
   실행되고 사이드파일이 69**여야 한다. 종료 코드는 양쪽 0이라 2번 단정으로는
   검출되지 않는다 — 이 단정이 `try/finally` 회귀를 막는 감시선이다.
4. **D78 관측 가능한 부작용 한정** — 세 가지를 단정한다.
   · 스니펫 시점에 `Get-Job` = 0이고 `Get-EventSubscriber` = 0이다(**`-Force`
   없는 형태**. `-SupportEvent`는 숨김이지 부재가 아니라 `Get-EventSubscriber
   -Force`로는 1이 보인다). 이것이 `-SupportEvent` 회귀 감시선이며, 빠지면
   `-Force` 없이도 1이 되고 작업 표가 stdout에 찍힌다.
   · `Get-Job | Wait-Job`으로 끝나는 스니펫이 정상 종료하고 사이드파일이 남는다.
   · `Get-Job | Remove-Job -Force` 뒤에도 사이드파일이 남는다.
5. **D78 폴백 판정** — 폴백되어야 하는 입력(`param($x)` · `<# doc #> param($x)` ·
   `[CmdletBinding()] param($x)` · `using namespace System.Text` · 여러 줄 블록
   주석 뒤의 param · **선두 U+FEFF + param** · **백틱 줄 연속 + `using`** ·
   `dynamicparam` · `begin` · `process` · `end` · **`clean`**)과 승격되어야 하는
   입력(일반 스니펫 · 라인 주석으로 시작 · `#requires`로 시작 · `Set-StrictMode`로
   시작)을 각각 단정한다. 폴백 쪽은 baseline과 종료 코드·본문 실행·표준 출력·
   사이드파일이 모두 같아야 하며, 폴백이 만드는 스크립트는 현재 D76 형태와
   **바이트 동일**하므로 이 대조는 구성상 성립한다. 명명 블록은 **토큰마다 따로**
   단정한다 — `dynamicparam`이 3라운드에서, `clean`이 5라운드에서 빠졌던
   자리다(`clean`은 7.x에서만 명명 블록이라 5.1에서는 과잉 폴백으로 무해하다).
6. **D78 stderr 무회귀** — 두 형태로 나눠 단정한다. 여기서 baseline은 §2.5와 달리
   **자동으로 성립하지 않는다** — 승격 대상은 트리 안에 등록 문 없는 경로가
   없으므로, 대조군은 테스트가 `BOM + 스니펫 + D76 tail`을 직접 조립해 같은 러너
   argv(`-NoProfile -NonInteractive -File`)로 돌린 실행이다.
   · **오류가 2행 이상인 스니펫**: 승격 경로의 stderr가 대조군과 **바이트 동일**.
   등록 문을 앞 줄에 두면 여기서 +1 드리프트가 잡힌다. 사례로 `param`을 고르지
   않는다 — 승격해도 stderr가 0B라 비교할 오류가 없다(§3 표4).
   · **1행 오류 스니펫**: 보고 줄 번호가 1인 것만 단정하고 **열 번호와 소스 줄
   에코의 차이는 명시적으로 허용**한다(§1.2 한계).
   · `#requires`가 붙은 스니펫이 승격돼도 지시자가 그대로 작동하는 것을 함께
   단정한다.
7. **D78 경로 이스케이프** — `'`를 포함한 `sidePath`로 `snippetContent`를 부르면
   생성된 등록 문에 `''`가 들어가고, 그 스크립트가 정상 파싱되어 사이드파일이
   기록된다. 이 단정이 없으면 이중화가 사라진 것을 아무도 잡지 못하고, 그런
   호스트에서는 모든 shell 스니펫이 ParserError가 된다(§관측 19).
8. **D78 신호 생성** — `exit N`으로 끝나는 스니펫에서 사이드파일이 생기고 마지막
   외부 명령의 코드가 담긴다. **현재 코드에서 이 테스트는 FAIL해야 한다**.
9. **D78 한계 고정** — 경로마다 결과가 다르므로 따로 단정한다.
   · `[Environment]::Exit(N)` → 사이드파일 없음.
   · 폴백 경로 → 스니펫이 `exit`으로 끝나거나 **처리되지 않은 종료 오류**(`throw`
   등)로 끝날 때 보강하지 않는다. 그 외에는 기존 D76 동작대로 정상 기록된다.
   · `Unregister-Event -Force` · `$LASTEXITCODE` 대입 · 스니펫 자신의 핸들러 등록
   → **스니펫이 정한 값이 사이드파일에 남는다**(`exec.go:226-234`).
   · **`-Force` 없는 `Unregister-Event`는 실패해 진짜 값이 남는다** — 이 대비가
   깨지면 `-SupportEvent`가 사라진 것이다.
10. **D79 축 판정** — 축마다 ① 전 패키지 빌드 ② 스코프 포함 ③ lint 결과 순으로
    확인한다. ①은 `GOOS=<축> go build ./...`(perf 축은 `-tags perf`)이고, 실패는
    "그 축이 성립하지 않음"으로 읽는다 — lint 실패와 구분해 보고한다. ②의 perf
    축은 `.TestGoFiles`/`.XTestGoFiles`를 본다.

**무회귀 대상**: D73 색인 계약(`SchemaVersion` 1 유지 · 색인은 버전 스위치 밖) ·
D76의 읽기 상한·마커 판별·`stderr == ""` 보강 조건·`sidePath` 이스케이프 ·
D75의 unix shell 공유 지점 · D69의 `GOOS=windows` 축 · `exit_code`는 스니펫의
종료 상태라는 계약.

## 3. 관측 실측 기록

측정 조건: Windows 11 Home. PowerShell 케이스는 `pwsh 7`과 `powershell 5.1`
양쪽에서 `-NoProfile -NonInteractive -File`로 실행했다(러너 argv와 같은 형태).
명시가 없으면 **각 칸은 단일 실행**이고, **모든 표에서 두 셸의 결과가 동일**했다.

**표1 — `try/finally` 승격 전후(초안 검증, 기각된 대안)**

| 스니펫 | 감싸지 않음 exit | 래핑 exit | 판정 | 래핑 시 사이드파일 |
| --- | --- | --- | --- | --- |
| `cmd /c exit 71` + `exit 3` | 3 | 3 | 동일 | `ctr-exit-code:71` |
| `cmd /c exit 72` | 0 | 0 | 동일 | `ctr-exit-code:72` |
| `cmd /c exit 73` + `throw` | 1 | 1 | 동일 | `ctr-exit-code:73` |
| `cmd /c exit 76` + 인수 없는 `exit` | 0 | 0 | 동일 | `ctr-exit-code:76` |
| 현재 D76 방식 + `exit 3` | 3 | — | — | **없음**(공백 재현) |
| `[Environment]::Exit(5)` | — | 5 | — | 없음(한계) |
| `#requires -Version 5` | — | 0 | — | `ctr-exit-code:81` |
| 주석으로 시작 | — | 0 | — | `ctr-exit-code:82` |
| `Set-StrictMode` + 외부 명령 없음 | — | 0 | — | 빈 값(기존 계약) |
| 외부 명령 둘(11 → 22) | — | 0 | — | `ctr-exit-code:22`(마지막) |

**표2 — dot-source 대안(기각 근거)**

| 스니펫 | 단독 exit | dot-source 래핑 exit | 사이드파일 |
| --- | --- | --- | --- |
| `cmd /c exit 71` + `exit 3` | 3 | **0** | `ctr-exit-code:3`(오염 — 71이어야) |
| `cmd /c exit 76` + 인수 없는 `exit` | 0 | 0 | `ctr-exit-code:0`(오염 — 76이어야) |
| `param($x)` + `cmd /c exit 74` | 0 | 0 | `ctr-exit-code:74`(정상) |
| `cmd /c exit 73` + `throw` | 1 | 1 | `ctr-exit-code:73`(정상) |
| 변수 대입 + `cmd /c exit 77` | 0 | 0 | `ctr-exit-code:77`(정상) |

**표3 — 1라운드 검수 실측(초안 결정 둘의 반증 근거)**

| 대상 | 조건 | 관측 |
| --- | --- | --- |
| 기존 색인 `CREATE INDEX IF NOT EXISTS` | 다른 커넥션이 `BEGIN IMMEDIATE` 보유 | **14ms, nil** |
| 기존 테이블 `CREATE TABLE IF NOT EXISTS` | 동일 | 2ms |
| `PRAGMA user_version` 읽기 | 동일 | 0s |
| 같은 커넥션의 실제 쓰기 | 동일 | 5.03s(락 보유 확인) |
| `ExecContext`(200ms 데드라인) | 쓰기 락 경합 | 반환 **5.027s / 5.034s**, 오류 `DeadlineExceeded` |
| `ExecContext`(150ms 데드라인) | 경합 없는 장시간 문장 | 150ms에 중단 |
| `dotnet new list` · `dotnet run` | `USERPROFILE`만 빈 스크래치 | 배너 없음, `.dotnet` 미생성 |
| 동일 | `DOTNET_CLI_HOME`을 빈 디렉터리로 | 배너 있음, `.dotnet` 생성 |

**표4 — 승격 수단 세 형태 비교(D78 채택 근거)**

| 케이스 | 현재(tail만) | `try/finally` | **`Register-EngineEvent`** |
| --- | --- | --- | --- |
| `cmd /c exit 71` + `exit 3` | 없음 / exit 3 | `:71` / exit 3 | `:71` / exit 3 |
| `cmd /c exit 76` + 인수 없는 `exit` | 없음 | `:76` | `:76` |
| `cmd /c exit 73` + `throw` | **없음** / exit 1 | `:73` / exit 1 | `:73` / exit 1 |
| `cmd /c exit 72` | `:72` | `:72` | `:72` |
| **41 + 명령 미발견 + 69** | **`:69`** | **`:41`**(조기 중단) | **`:69`**(본문 완주) |
| `param($x="dflt")` + `$x` 참조 + 80 | `:80`, 출력 `x=dflt`, stderr 0B | 본문 미실행 | `:80`, 출력 `x=dflt`, **stderr 0B**(`$Error.Count` 0→1) |
| `using namespace System.Text` + 78 | `:78` / exit 0 | 깨짐 | **없음** / exit 1(ParserError) |
| 백틱 줄 연속 + `using` + 78 | `:78` / exit 0 / stdout `ok` | — | **없음** / exit 1 / stdout 빈 값 |
| 명명 블록(`begin`/`process`/`end`) + 79 | 없음 / exit 1 | — | 빈 값 / exit 0 |
| `clean` + `end` + 79 (pwsh 7.x) | 없음 / exit 1 | — | 빈 값 / exit 0 |
| `[Environment]::Exit(5)` | — | 없음 | 없음 |
| `Set-StrictMode` + 외부 명령 없음 | — | 빈 값 | 빈 값 |
| stdout 오염 | — | 없음 | 없음(**`-SupportEvent` 없으면 작업 표가 찍힌다**) |
| 안정성(각 셸 10회) | — | — | 누락 0 · 값오류 0 · 종료코드오류 0 |

`param` 행에서 승격 형태의 관측 차이는 **없다**(stderr 0B). `param`이 명령으로
해석되는 것은 사실이나(`$Error.Count`가 0→1) 그 오류는 stderr로 나가지 않고,
기본값 없는 `param($x)`도 양쪽 다 `x=[]`다. 그래서 `param`의 폴백은 **과잉
폴백**이며 §0이 허용한 방향이다 — 다만 §2.6의 stderr 대조 사례로는 쓸 수 없다.
`throw` 행의 현재 형태가 사이드파일 없음인 것은 tail이 처리되지 않은 종료 오류에서
실행되지 않기 때문이며, 폴백 경로가 그 성질을 그대로 물려받는다(§2.9).

**표5 — 사이드파일 값 조작 경로(D78 한계 근거, 채택 형태 기준)**

| 시나리오 | `try/finally` | **인라인 + `-SupportEvent`** |
| --- | --- | --- |
| 선기록 + `Get-Variable`을 throw 함수로 재정의 | 위조값 잔존 | **`:74`**(진짜 값 — 막힘) |
| 선기록 + `Unregister-Event`(**`-Force` 없음**) | — | **`:45`**(진짜 값 — 숨김 구독을 못 찾아 실패) |
| 선기록 + `Unregister-Event -Force` | — | `:8888`(스니펫이 정한 값) |
| `Get-EventSubscriber \| Unregister-Event`(`-Force` 없음) | — | `:47`(신호 유지) |
| `Get-EventSubscriber -Force \| Unregister-Event -Force` | — | 사이드파일 없음(신호 소멸) |
| `$LASTEXITCODE = 7777`(최상단 대입, `global:` 불필요) | — | `:7777` |
| 스니펫이 자기 `PowerShell.Exiting` 핸들러를 등록 | — | `:9999`(6회 반복 전부 스니펫 승) |

같은 소스 식별자로 두 번 등록해도 충돌 오류가 나지 않고 두 핸들러가 공존한다.
`-SupportEvent`가 `-Force` 없는 정리 관용구를 무력화해 **조작 난이도를 올린
것**은 이 표가 기록하는 긍정 결과다.

**표6 — `-SupportEvent`와 인라인 배치(3·4라운드 대응 근거)**

| 항목 | 앞 줄, `-SupportEvent` 없음 | 앞 줄, `-SupportEvent` | **인라인 + `-SupportEvent`** |
| --- | --- | --- | --- |
| `Get-Job` / `Get-EventSubscriber`(`-Force` 없이) | **1 / 1** | 0 / 0 | 0 / 0 (`-Force`로는 1) |
| `Get-Job \| Wait-Job` | **미종료**(15s 강제 종료) | 정상 종료 | 정상 종료 |
| `Get-Job \| Remove-Job -Force` 후 사이드파일 | **없음** | 있음 | 있음 |
| **2행 오류** 줄 번호(baseline `.ps1:2`) | `.ps1:3` | `.ps1:3` | **`.ps1:2`**, stderr 바이트 동일 |
| **1행 오류**(baseline `.ps1:1` `문자:1`) | — | — | `.ps1:1` `문자:324`, 에코에 등록 문 꼬리(pwsh 438→557B · 5.1 322→446B) |
| `41 + 명령 미발견 + 69` 사이드파일 | `:69` | `:69` | `:69` |
| 첫 줄이 라인 주석 / 블록 주석 | — | — | `:55` / `:56`(신호 유지) |
| `#requires -Version 99`(baseline exit 1) | exit 1 | exit 1 | **exit 1**(4형태 확인) |
| here-string으로 시작 | — | — | `:62`(정상) |
| 29입력 × 3형태 × 2셸 | — | — | 종료 코드·사이드파일·stdout 차이 **0건** |

**표7 — D79 축 실측(5라운드, 대표 GOOS 결정 근거)**

| 축 | `go build ./...` | golangci-lint v2.12.2 | 스코프 확인 |
| --- | --- | --- | --- |
| `GOOS=freebsd` | **exit 1** — `worker_unix.go:98:29` (`syscall.Rlimit.Cur`가 int64) | **exit 7**, `typechecking error` + `0 issues.` | `launcher_other.go` 포함(1단계는 통과) |
| `GOOS=openbsd` | **불가** — `syscall.RLIMIT_AS` 미정의(`worker_unix.go:75`·`:102`) | — | — |
| **`GOOS=netbsd`** | **exit 0** | **exit 0 / 0 issues / 10s** | `launcher_other.go run_unix.go sandbox.go` |
| `GOOS=darwin` | exit 0 | exit 0 / 0 issues / 13s | `launcher_darwin.go` 포함 |
| `--build-tags perf` | exit 0 | exit 0 / 0 issues / 12s | `.TestGoFiles`로 `perf_test.go` 포함 |

D73 §2⑦ 예산 게이트의 오늘 재실측은 rows=2000 **8.5882ms** · rows=20000
**25.6539ms**로, 예산 5s 대비 195배·583배 여유다 — D77 게이트의 여유(임계
1000ms 대비 기준값 632·633ms = 1.58배)와 대비해 §2.1의 규모 조건을 정한 근거다.

**D77 재측정(구현 단계)** — `go test -p 1 ./internal/store/ -run
TestPurgeHookOnlyLockHoldBudget -v`. 픽스처는 hook-only 소스 **20개**이며 블롭
mtime을 `gcOrphanMinAge` 이전으로 소급시켜 회수 경로를 타게 했다(`ReclaimedB=370B`,
`DeferredFiles=0`, `FailedFiles=0`). 실 보유 시간 **19.8ms**(해시당 약 1.0ms)로
임계 1000ms 대비 여유 **50.5배**다. `-count 5` 재실행 대역 13.0~23.7ms(여유
42.1~76.8배)는 **이 호스트의 값**이고, 같은 픽스처를 계획 검수가 다른 개발기에서
돌린 **44.1ms**(여유 22.7배)가 알려진 상단 관측이다. 그 상단도 §2.1 규모 조건의
상한(임계의 1/5 = 200ms)의 4분의 1 아래다. 원 실측 632·633ms는 **D73 색인 도입
이전**의 실 도그푸딩 저장소(색인 없음, 아티팩트 1254개·210MiB) 값이라 절대값 비교
대상이 아니다 — 이 게이트가 잡는 것은 해시당 비용의 회귀다.

**D77 유도 FAIL 3건**(감시선이 물린다는 증거 — 단정 항마다 하나씩). ①
`ageBlobFile` 호출을 주석 처리하면 `회수 경로를 타지 않았다: ReclaimedB=0
DeferredFiles=20 FailedFiles=0`. 나이 게이트가 단락되면 `stillReferenced`도
`os.Remove`도 실행되지 않아 유예 경로만 재는 종이 게이트가 된다는 §0 D77의 근거가
실측으로 확인된다(그 상태의 소요 0.04s는 PASS 경로 0.05~0.06s보다 **짧다** — 경로
단정이 없으면 "더 빨라진" 종이 게이트가 조용히 초록이 된다). ② `budget`을 1ms로
낮추면 `잠금 보유가 예산을 넘었다: 16.3535ms >= 1ms (hashes=20 reclaimed=370B
deferred=0 failed=0)`. `Fatalf` 문면이 보고서 값 넷을 함께 실어 "회귀인가 러너
편차인가"를 가를 수 있다는 것을 같은 출력이 함께 보인다. ③ **`FailedFiles` 항의 유도** — 한
해시의 rename 목적지(`<hash>.purging`)를 디렉터리로 선점해 `os.Rename`을 실패시키면
`회수 경로를 타지 않았다: ReclaimedB=352 DeferredFiles=0 FailedFiles=1`이다.
**`ReclaimedB>0`·`DeferredFiles==0`이 그대로 성립**하므로 `FailedFiles` 항이 없던
형태는 이것을 통과시킨다 — 20해시 중 19해시만 회수한 **줄어든 배치**를 재고도
넉넉한 여유를 보고했을 것이다(370−352 = 18B = 빠진 해시 하나의 정확한 크기).
셋 다 확인 후 원복해 PASS를 재확인했다.

**ConstrainedLanguage 모드(D78, 구현 단계 실측 — 미측정 목록에서 내린다)** —
등록 문이 스니펫보다 먼저 실행되므로 핸들러 스크립트블록이 **FullLanguage에서
생성**되고, 스니펫이 자신을 `ConstrainedLanguage`로 낮춘 뒤 열리는 종료 시점에도
기록에 성공한다(사이드파일 `ctr-exit-code:5` · exit 0 · stderr는 보강 줄 하나).
D76 tail은 강등 **뒤에** 실행돼 `WriteAllText`가 막혔고 `try/catch`로 삼키는 것이
최선이었다(신호 소실) — 승격이 그 공백을 닫는다. 호스트는 pwsh 7 갈래이며, 5.1은
아래 환경 항목의 이유로 이 호스트에서 검증할 수 없다.

같은 단계에서 둘을 더 쟀다. · **strict mode와 핸들러 스코프**: `Set-StrictMode
-Version Latest`로 시작하는 스니펫에서도 핸들러가 실행돼 기록에 성공했고 값은 빈
문자열이었다 — 핸들러가 스니펫의 strict mode 스코프 **밖**에서 돈다는 뜻이고,
승격 경로가 `Get-Variable` 없이 `$LASTEXITCODE`를 직접 읽어도 되는 근거다(빈 값은
`readNativeExitCode`가 "보강하지 않음"으로 거른다). · **`-SupportEvent`의 stdout
청결성**: 생성 형태 그대로를 직접 실행하면 stdout이 마커 한 줄뿐이고 stderr 0B ·
exit 0 · 사이드파일 정상이다 — 표4의 "stdout 오염 없음" 행을 생성물 기준으로
재확인한 것이다.

**선두 `[`의 과잉 폴백이 한계 픽스처를 무효화한다(구현 단계 발견)** — §0 D78이
허용한 과잉 폴백(`[int]$x = 5`)의 실제 귀결로 **`[System.IO.File]::…`로 시작하는
스니펫도 폴백된다**. 계획이 §2.9 한계용으로 잡은 픽스처 둘이 그 형태였고, 등록
문이 아예 붙지 않아 승격 경로를 밟지 못했다. 실측 4형태 비교(폴백형은 `-Force`
유무와 무관하게 둘 다 `:8888`, 승격형은 `-Force`면 `:8888`·`-Force` 없으면
`:45`)에서 `-Force` 없는 쪽은 **통과 불가능**, `-Force` 쪽은 무엇을 지우든
통과하는 **거짓 초록**임이 드러났다. 구현이 첫 줄을 `$ctrProbe = Join-Path …`로 바꾸고 두
케이스 모두 승격 판정을 먼저 단정해 닫았다(`internal/exec/exec_test.go:2120`).
남길 교훈: **승격 경로를 검증하려는 픽스처는 선두 문자가 폴백 규칙에 걸리지 않는지
먼저 확인해야 한다.**

**테스트가 제품 환경을 재현하지 않으면 결과가 세션 의존이 된다(구현 단계 발견)** —
실행 헬퍼가 `cmd.Env`를 nil로 두면 자식이 호스트 환경 **전체**를 상속해
(`internal/sandbox/sandbox.go:134-136`이 경고하는 그 경로) 제품 러너의 닫힌
allowlist(`sandbox.BaseEnv()`)와 다른 환경에서 돈다. 그 탓에 `powershell 5.1`
레그가 **테스트를 띄운 세션이 상속한 `PSExecutionPolicyPreference`에 따라
초록/빨강이 갈렸고**, D65가 일부러 떼어낸 호스트 `PSModulePath`도 스니펫에 닿았다.
헬퍼를 `sandbox.BaseEnv()` + `tmpEnv` + 러너의 `extra`로 닫은 뒤 이 호스트의 5.1
레그는 red이며, **`ci.yml`의 windows 잡이 5.1 갈래의 유일한 권위**다
(`internal/exec/exec_test.go:1842`). 위 표들의 5.1 칸은 손으로 조립한 프로브의
값이라 이 한정과 별개다.

**미측정**: 원격 세션, 등록 문이 실패하는 환경, 스니펫이 대용량 stdout을 내는
중의 핸들러 기록 순서, `-race` 아래의 D77 게이트 실측(로컬은 cgo 부재로 불가).

**D79 빌드 태그 전수 조사** — `//go:build` 선언 16개 중 어느 lint 축에도 잡히지
않는 것은 세 파일이다. 이 수는 조사 시점(2026-07-28)의 값이며, 파일이 추가되면
다시 세어야 한다.

## 4. 의도적 미결 (v0.15+ 후보)

**이번 검수가 반증해 내린 둘 — 근거 없이 다시 올리지 말 것**

- **`OpenContext` 경로의 취소 전파** — 두 전제가 §3 표3으로 반증됐다. 다시
  올리려면 **먼저 그 두 실측을 뒤집어야** 한다. 대기를 실제로 줄이려면 ctx가
  아니라 해당 경로 전용의 낮은 `busy_timeout` 또는 별도 감시 수단이 필요하며,
  그것은 정상 경합에서 `Open`을 실패시키는 새 위험을 만든다.
- **windows shell 갈래의 dotnet first-run 억제** — 배너가 재현되지 않았다(§3
  표3). 다시 올리려면 **배너를 실제로 내는 조건**을 먼저 제시해야 한다.

**기각한 형태 — 되돌리지 말 것**

- **`try { <스니펫> } finally { <tail> }`** — 문 종료 오류의 제어 흐름을 바꾸고
  종료 코드가 같아 검출되지 않는다(§3 표4). §2.3이 감시선이다.
- **dot-source 래퍼** — `exit`의 종료 코드와 `$LASTEXITCODE`를 깨뜨린다(§3 표2).
- **등록 문을 앞 줄에 두기** — 모든 오류의 줄 번호가 +1 밀린다(§3 표6). 인라인은
  2행 이상 오류에서 이를 없애고 1행 오류에서는 줄 번호만 보존한다 — 더 좁은
  회귀를 택한 것이다. §2.6이 감시선이다.
- **`-SupportEvent` 없이 등록하기** — 작업표가 남아 `Get-Job | Wait-Job`이 끝나지
  않고 작업 표가 stdout에 찍히며, 값 조작 난이도도 낮아진다(§3 표5·표6).
  §2.4와 §2.9가 감시선이다.
- **`GOOS=freebsd`를 lint 축 대표로 쓰기** — 전 패키지가 빌드되지 않아 lint가
  `typechecking error`로 죽는다(§3 표7). 스코프 확인만으로는 이 실패가 걸러지지
  않으므로 §1.3-3의 ①이 감시선이다.

**그 밖의 미결**

- **`[Environment]::Exit(N)` 경로**(§1.2) — 스크립트 수준 수단이 없다.
- **폴백 경로 스니펫의 신호 승격**(§1.2) — PowerShell AST 파싱이 선행된다.
- **첫 줄 오류의 열 번호·에코 오염**(§1.2) — 등록 문을 짧게 하려면 기록 동작을
  외부화해야 한다(예: 러너가 미리 정의한 함수를 부르는 형태). 그 자체가 새
  주입면이므로 별도 결정이다.
- **사이드파일 값 조작 경로**(§1.2) — 격리 계층이 선행된다.
- **보강의 `stderr == ""` 요건**(§1.2) — D76의 "빈 자리에만 넣는다" 결정을 다시
  열어야 한다.
- **`worker_unix.go`의 GOOS 이식성**(§1.2) — `freebsd`·`openbsd`에서 컴파일되지
  않는다. 지원 대상이 되면 그때 RLIMIT_AS 자기적용 실증과 함께 재검토한다.
- **100해시 배치의 절대 보유 시간 검증** — 실 저장소 분포를 재현하는 픽스처나
  도그푸딩 저장소 기반 측정이 선행된다.
- **`startupPurgeMaxHashes` 값 재조정** — D77 측정 결과가 입력이다.
- **로컬 설치 산출물의 버전 마커 드리프트** — `.mcp.json`과
  `.claude/settings.json`의 `__ctrManaged`가 `context-router/0.12.0`인데
  `internal/buildinfo/buildinfo.go:11`의 `productVersion`은 0.13.0이다. 둘 다
  git 추적 대상이 아니므로 이 설계의 범위 밖이지만, D64가 self-heal 대상으로
  지목한 상태가 실제로 발생해 있다는 관측이다 — 릴리스 후 `hook install`
  재실행이 절차에 없다. 두 디렉터리가 **미추적이지 무시(ignore)는 아니라는**
  점도 함께 남긴다.
- **v0.13 §4 이월분**: FTS 인덱스 구성 재검토 · `indexed_at` 단독 색인과
  `PurgeOlderThan` 경로 측정 · doctor `[12]` 로그 로테이션 ·
  `ctr_batch_execute` 재도입(D58) · Windows AppContainer FS 제한(D59) · 타인
  배포 채널·MCP 설치 표준.
- **이월 Minor** — 로컬 원장 `progress.md`의 잔여분.
