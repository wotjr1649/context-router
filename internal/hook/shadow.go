package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"

	"github.com/wotjr1649/context-router/internal/ingest"
	"github.com/wotjr1649/context-router/internal/session"
	"github.com/wotjr1649/context-router/internal/store"
)

// Shadow Recall 자체 방어 상한(설계 §5). 인라인 ingest 경로(runInline)는 무검사이므로 훅이 캡·
// denylist·바이너리 판정을 명시 구현한다 — 인라인 경로에 기대지 않는다.
const (
	defaultShadowMinBytes = 16384   // 16KiB — 이하는 재소환 가치가 낮아 미저장(CTR_SHADOW_MIN)
	defaultShadowMaxBytes = 1048576 // 1MiB — 초과는 직렬화 전 하드 컷(CTR_SHADOW_MAX)
)

// fileOriginTools: tool_response가 파일 내용에서 유래하는 도구 — tool_input 경로를 secret
// 파일명 denylist에 대조한다(비밀 파일 출력의 파일명 우회 색인 차단, 설계 §5).
var fileOriginTools = map[string]bool{"Read": true, "NotebookRead": true}

// shadowCapture — PostToolUse(성공) 이벤트의 tool_response를 §5 자체 방어(OFF→MIN→MAX→denylist
// →바이너리)로 판정하고, 통과 시 content.db 아티팩트로 저장한 뒤 artifact_created·
// tool_result_summary 이벤트(artifact ref 포함)를 append한다. 모든 스킵은 조용히, 방어적 미저장
// (oversize·denylist·store 불가)만 drops 1줄을 남긴다(fail-open §2.3 연장 — 어떤 실패도 훅
// exit code를 바꾸지 않는다). drops는 세션 dir(dir)에, 아티팩트는 프로젝트 dir(contentDir)에.
func shadowCapture(ctx context.Context, ad *session.AppendDB, in hookInput, dir, contentDir, external string, getenv func(string) string) {
	if getenv("CTR_SHADOW_OFF") == "1" {
		return
	}
	body := in.ToolResponse // tool_response 직렬화 바이트 = 저장·판정 대상(전문)
	size := len(body)
	switch {
	case size <= shadowLimit(getenv, "CTR_SHADOW_MIN", defaultShadowMinBytes):
		return // 임계 이하 — 재소환 가치 낮음, 조용히 스킵
	case size > shadowLimit(getenv, "CTR_SHADOW_MAX", defaultShadowMaxBytes):
		appendDrop(dir, "shadow-oversize") // 하드 캡 초과 — 미저장
		return
	}
	if fileOriginTools[in.ToolName] && shadowInputDenied(in.ToolInput) {
		appendDrop(dir, "shadow-denylist") // 비밀 파일 유래 응답 — 미저장
		return
	}
	// D31 decode-sniff: body는 hook.Run 외부 파싱을 통과한 유효 JSON — 문자열 leaf를
	// 재귀 수집해 디코드된 실바이트로 판정한다(C2 부분문자열 검사의 FP 상한 제거).
	// UseNumber로 수를 json.Number(string 계열)로 받아 float64 범위 밖 수(1e1000 등)의
	// UnmarshalTypeError 오거부를 없앤다 — 수 leaf는 NUL을 담을 수 없어 stringLeaves가 무시.
	// 진짜 디코드 불가(직조립 입력)만 조용히 스킵한다.
	var decoded any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if dec.Decode(&decoded) != nil {
		return
	}
	for _, leaf := range stringLeaves(decoded, nil) {
		b := []byte(leaf)
		// 전장 IndexByte가 필수다: NUL은 유효 UTF-8 코드포인트라 IsBinary의 utf8.Valid를
		// 통과하고, IsBinary의 NUL 탐색은 첫 8KiB뿐 — late-NUL(기존 회귀 테스트
		// TestShadowEscapedNULSkips의 20KB 뒤 니들)은 여기서만 잡힌다.
		if bytes.IndexByte(b, 0) != -1 || ingest.IsBinary(b) {
			return // leaf에 NUL·비텍스트 — 조용히 미저장(현행 관례 승계)
		}
	}

	st, err := store.OpenContext(ctx, contentDir, false)
	if err != nil {
		appendDrop(dir, "shadow-store") // 잠금 경합·손상 등 — deadline 예산 안에서 포기
		return
	}
	defer func() { _ = st.Close() }()

	rep, err := ingest.Run(ctx, st, "", nil, ingest.Request{
		Content: string(body), Title: in.ToolName, SourceKind: "hook",
	})
	if err != nil || rep.Indexed == 0 || rep.Hash == "" {
		appendDrop(dir, "shadow-ingest")
		return
	}

	ref := "artifact://" + external + "/sha256-" + rep.Hash // 문자열 조립만(url.Parse 금지, §5)
	shadowAppend(ctx, ad, dir, session.Event{
		Type: "artifact_created", Summary: summaryLine(in.ToolName, "shadow artifact"),
		ArtifactRefs: []string{ref},
	})
	shadowAppend(ctx, ad, dir, session.Event{
		Type: "tool_result_summary", Summary: summaryLine(in.ToolName, strconv.Itoa(size)+"B"),
		ArtifactRefs: []string{ref},
	})
}

// shadowInputDenied: tool_input의 파일 경로(file_path 우선, 없으면 notebook_path)가 secret 파일명
// denylist에 걸리는지. 경로가 비면 false(대조 대상 없음).
func shadowInputDenied(toolInput json.RawMessage) bool {
	var f toolInputFields
	_ = json.Unmarshal(toolInput, &f)
	path := f.FilePath
	if path == "" {
		path = f.NotebookPath
	}
	return path != "" && ingest.DeniedFilename(path)
}

// shadowAppend: Shadow 이벤트 1건 append — 실패는 drops 1줄만 남기고 계속한다(부분 성공 허용,
// fail-open). 요약은 도구명+허용 요소만이라 응답 원문·비밀을 운반하지 않는다(§3 allowlist).
func shadowAppend(ctx context.Context, ad *session.AppendDB, dir string, ev session.Event) {
	if _, _, _, err := ad.Append(ctx, ev); err != nil {
		appendDrop(dir, "shadow-append")
	}
}

// shadowLimit: env 정수 상한(양수만 채택, 파싱 불가·비양수는 기본값 — deadline/retention과 동형).
func shadowLimit(getenv func(string) string, key string, def int) int {
	if v, err := strconv.Atoi(getenv(key)); err == nil && v > 0 {
		return v
	}
	return def
}

// stringLeaves — 디코드된 JSON 값에서 문자열 leaf를 전부 모은다(문자열·배열·객체 재귀,
// 그 외 스칼라 무시). D31 판정 전용 — 저장 바이트는 원문 직렬화 그대로다.
func stringLeaves(v any, acc []string) []string {
	switch t := v.(type) {
	case string:
		acc = append(acc, t)
	case []any:
		for _, e := range t {
			acc = stringLeaves(e, acc)
		}
	case map[string]any:
		for _, e := range t {
			acc = stringLeaves(e, acc)
		}
	}
	return acc
}
