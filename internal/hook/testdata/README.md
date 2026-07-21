# hook stdin 골든 픽스처 + Claude Code 훅 API 검증 기록

**작성**: Task 0 (v0.2 forced-channel 계획, 설계 §1.3) · 2026-07-21
**검증 대상 문서**: <https://code.claude.com/docs/en/hooks> (Hooks reference), <https://code.claude.com/docs/en/hooks-guide>
**성격**: 이 디렉터리는 v0.2 `context-router hook` 서브프로세스가 소비하는 Claude Code 훅 stdin JSON의 **실형태 골든 픽스처**이며, 본 README는 이후 전 태스크(T4·T5·T7·T9 등)가 소비하는 **계약**이다. 픽스처는 위 공식 문서에 실린 필드명·중첩 형태를 그대로 따른다(더미 값만 대입).

> **⚠️ 계획 가정 ① 불일치 (MATERIAL MISMATCH) — 아래 §1 참조.**
> 계획은 "PostToolUse가 도구 실패 시에도 발화하고 `tool_response`가 오류 신호를 담는다"고 가정했으나,
> 현행 공식 문서는 실패를 **별도 이벤트 `PostToolUseFailure`** 로 분리하고 그 페이로드는
> `tool_response`가 아니라 `error`(문자열)를 담는다. 컨트롤러 결정 필요.

---

## 검증 결과 5건

### ① PostToolUse 실패 발화 + `tool_response` 오류 신호 — **불일치**

- **계획 가정**: PostToolUse는 도구 호출이 실패해도 발화하고, `tool_response`가 오류 신호를 담는다.
- **문서 실제**:
  - `PostToolUse`는 **성공한 도구 실행 후** 발화하며 `tool_response`는 도구의 구조화된 Output 객체다.
    Write 예: `"tool_response": { "filePath": "...", "success": true }`.
  - 도구 **실패**는 별도 이벤트 **`PostToolUseFailure`** 로 발화한다. 이 페이로드에는 **`tool_response`가 없고**,
    대신 다음을 받는다:
    - `error` — *"String describing what went wrong"* (예: `"Command exited with non-zero status code 1"`)
    - `is_interrupt` — *"Optional boolean indicating whether the failure was caused by user interruption"*
    - `duration_ms` — optional
  - matcher 표에도 `PreToolUse`, `PostToolUse`, **`PostToolUseFailure`**, `PermissionRequest`, `PermissionDenied`가 별개 이벤트로 tool name 매칭 대상으로 명시됨.
- **인용**(hooks reference, PostToolUseFailure 섹션):
  > `error` | String describing what went wrong
  > `is_interrupt` | Optional boolean indicating whether the failure was caused by user interruption
- **영향**: T5(계측 매핑) 및 설계 §1.3의 "PostToolUse 단일 이벤트로 성공/실패 모두 처리, tool_response 오류 신호 파싱" 전제가 무효.
  실패 계측은 **`PostToolUseFailure` 이벤트 + `error` 문자열** 경로로 재설계해야 함. → **컨트롤러 판단 필요(계획 중단 사유)**.
- **픽스처 처리**: `posttooluse-error.json`은 문서에 충실하게 `hook_event_name: "PostToolUseFailure"` + `error` 필드로 작성함
  (파일명은 브리프 지정을 유지). 계획의 잘못된 가정 형태(`PostToolUse` + `tool_response` 오류)로는 작성하지 않음 — 잘못된 계약을 이후 태스크에 전파하지 않기 위함.

### ② PreToolUse deny 출력 스키마 — **확인**

- **계획 가정**: `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"..."}}` 를 stdout에 기록.
- **문서 실제**: 정확히 일치. `permissionDecision`은 **`allow` / `deny` / `ask` / `defer`** 4값을 받는다(계획은 `deny`만 언급 — 확인·확장).
  다중 PreToolUse 훅 우선순위는 `deny` > `defer` > `ask` > `allow`.
- **stdout 경로 확인**: *"Exit 0 means success. Claude Code parses stdout for JSON output fields. JSON output is only processed on exit 0."* 즉 deny JSON은 **exit 0 + stdout** 로 전달된다.
  (대안: exit 2 → stderr가 Claude에 오류로 피드백되며 PreToolUse의 경우 도구 호출을 차단.)
- **인용**:
  > PreToolUse | `hookSpecificOutput` | `permissionDecision` (allow/deny/ask/defer), `permissionDecisionReason`
- **픽스처**: 이 항목은 **훅 출력(stdout)** 스키마이지 stdin이 아니므로 stdin 픽스처는 없음(계약으로만 기록). guard(T7)가 이 형태로 stdout을 써야 함.

### ③ SessionStart `source` 값 집합 — **확인**

- **계획 가정**: `source` ∈ {startup, resume, clear, compact}.
- **문서 실제**: 정확히 일치.
  > `source` | How the session started: `"startup"` for new sessions, `"resume"` for resumed sessions, `"clear"` after `/clear`, or `"compact"` after compaction
- SessionStart 입력은 공통 필드에 더해 `source`(필수) + 선택적 `model`, `agent_type`, `session_title`을 받는다.
- **픽스처**: `sessionstart.json` — `source: "startup"`.

### ④ settings.json `hooks` 스키마 + matcher 의미 + timeout 단위 — **확인**

- **계획 가정**: `{"hooks":{"<EventName>":[{"matcher":"...","hooks":[{"type":"command","command":"...","timeout":N}]}]}}`.
- **문서 실제**: 스키마 일치. 예:
  ```json
  {
    "hooks": {
      "PostToolUse": [
        {
          "matcher": "Write|Edit",
          "hooks": [
            { "type": "command", "command": "${CLAUDE_PROJECT_DIR}/.claude/hooks/check-style.sh", "args": [] }
          ]
        }
      ]
    }
  }
  ```
- **matcher 의미**:
  - matcher는 **선택적**. `"*"`, `""`, 또는 생략 → 모두 매칭.
  - tool name으로 필터하는 이벤트: **`PreToolUse`, `PostToolUse`, `PostToolUseFailure`, `PermissionRequest`, `PermissionDenied`**.
  - `SessionStart`는 matcher가 **세션 시작 방식**(`startup`/`resume`/`clear`/`compact`)을 필터.
  - matcher 값 평가: 영숫자·`_`·`-`·공백·`,`·`|`만 → 정확 문자열(또는 `|`/`,` 구분 목록); 그 외 문자 포함 → **JS 정규식(unanchored)**. 예: `Edit|Write`는 두 도구 정확 매칭.
- **timeout 단위 — SECONDS (초)**. 밀리초 아님. (계획의 미해결 질문 해소.)
  > `timeout` | no | Seconds before canceling. Defaults: 600 for `command`, `http`, and `mcp_tool`; 30 for `prompt`; 60 for `agent`. `UserPromptSubmit` lowers the `command`, `http`, and `mcp_tool` default to 30, and `MessageDisplay` lowers it to 10
  - 즉 command 훅 기본 timeout = **600초**. install(T8) 시 timeout 값은 **초 단위**로 기입해야 함.
- **픽스처**: settings.json 스키마는 stdin이 아니므로 stdin 픽스처 없음(계약으로만 기록; install/uninstall T8이 소비).

### ⑤ `tool_response` 최대 크기 / 절단 정책 — **문서 침묵 → T11 실측 가정**

- **문서 실제**: PostToolUse `tool_response`에 대한 **최대 크기·절단 정책 명시 없음**.
  가장 근접한 서술은 `PostToolBatch` 섹션:
  > `tool_response` contains the same content the model receives in the corresponding `tool_result` block. The value is a serialized string or content-block array, exactly as the tool emitted it. ... Responses can be large, so parse only the fields you need.
  → "도구가 방출한 그대로(exactly as the tool emitted it)", "커질 수 있으니 필요한 필드만 파싱하라"고만 안내. **절단(truncation)이 존재한다는 서술은 없음.**
- **판단**: 문서가 절단을 명시하지 **않으므로 설계 §5의 "전문 확보" 전제와 모순되지 않음.** (절단이 문서에 있었다면 §5 수정·중단 사유였으나 해당 없음.)
- **가정**: 전문(full content)이 전달된다고 가정. **실제 최대 크기·(만약의) 절단 여부는 T11 실호스트 스모크에서 실측 확인**한다.
  (사용자 실설정 파일을 격리 없이 편집하는 실측 스텝은 재현성·복구 안전성 문제로 이 태스크에서 배제 — 리뷰 반영.)
- **T11 실측 결과 (2026-07-21, Windows 실호스트 + stdin 캡처 훅)**: 문서의 "the same content the
  model receives" 서술 그대로 — Bash 도구의 stdout은 하네스가 **모델에게 보여주는 30,000자 절단본**이
  훅에도 전달된다(원시 3.4MB `seq` 출력 → 훅 stdin 총 37,063B, `tool_response` JSON 36,506B).
  즉 훅 stdin은 호스트 선절단으로 상한이 낮고, F1의 8MiB stdin 캡은 실측 대비 충분한 봉인.
  Shadow Recall "전문 확보"의 실질 상한 = 모델 가시 분량(도구별 하네스 절단 정책)과 동일.
  부수 실측: 권한 deny로 차단된 도구 호출은 PostToolUse/PostToolUseFailure **어느 쪽도 발화하지 않는다**.

---

## 공통 stdin 필드 키명 검증 (이후 코드 의존)

문서 예시로 확인한 정확한 키명 — 이후 파서(T4)가 이 이름들에 의존:

| 키 | 위치 | 확인 |
| --- | --- | --- |
| `session_id` | 공통 | ✓ (canonical UUID 문자열) |
| `transcript_path` | 공통 | ✓ |
| `cwd` | 공통 | ✓ |
| `hook_event_name` | 공통 | ✓ |
| `permission_mode` | tool 이벤트 공통 | ✓ (예: `"default"`) |
| `tool_name` | PreToolUse / PostToolUse / PostToolUseFailure | ✓ |
| `tool_input` | 위 동일 | ✓ (도구별 하위 필드) |
| `tool_response` | **PostToolUse (성공)** | ✓ (도구별 구조화 Output; 실패에는 없음) |
| `tool_use_id` | PostToolUse / PostToolUseFailure | ✓ |
| `error` | **PostToolUseFailure** | ✓ (실패 신호; 문자열) |
| `is_interrupt` | PostToolUseFailure | ✓ (optional bool) |
| `source` | SessionStart | ✓ |

---

## 픽스처 목록

| 파일 | 이벤트(`hook_event_name`) | 시나리오 |
| --- | --- | --- |
| `pretooluse-read.json` | `PreToolUse` | Read 도구, 큰 파일 경로(guard T7 대상 형태) |
| `posttooluse-bash.json` | `PostToolUse` | Bash `command`+구조화 출력(`stdout`/`stderr`/`interrupted`) |
| `posttooluse-write.json` | `PostToolUse` | Write, `tool_response = {filePath, success}` (문서 예시와 동형) |
| `posttooluse-error.json` | **`PostToolUseFailure`** | 실패 도구 호출의 **문서상 오류 신호**(`error` 문자열). 파일명은 브리프 유지, 내용은 문서 충실. |
| `sessionstart.json` | `SessionStart` | `source: "startup"` |
| `posttooluse-codex-bash.json` | `PostToolUse` | Codex CLI 훅 형상(설계 v0.4 §11.1 G1, codex-cli 0.144.6 문서 기준; 추가 필드는 무시됨). |

- 파일 인코딩: UTF-8 (BOM 없음), LF 개행.
- 더미 값: `session_id`는 canonical UUID(`3f2504e0-4f89-41d3-9a0c-0305e82c3301`), 경로는 Windows 스타일.
