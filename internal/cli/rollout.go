// rollout.go — D44 Codex usage 어댑터(설계 v0.6 §2): ~/.codex/sessions rollout JSONL 읽기 전용
// 파싱. 소비 필드는 2종뿐(session_meta.payload{session_id,cwd} + event_msg/token_count.info.
// total_token_usage) — 비공표 내부 형식 의존은 experimental 등급으로 한정한다(§5 한계).
// 소비자는 D45 --compare뿐 — 무플래그 usage 본표는 이 파일을 전혀 타지 않는다(byte-for-byte 게이트).
package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/wotjr1649/context-router/internal/ident"
)

type cxUsage struct{ input, cachedInput, output, total int64 }

// cxRollout — rollout 파일 1개의 파싱 결과(파일=세션 — UUID 중복 0 실측 §7).
type cxRollout struct {
	id      string    // session_meta.payload.session_id 권위(§2 — 파일명 UUID는 발견 수단)
	start   time.Time // 파일명 rollout-<ts> 로컬 시간(§2 — meta에 시각 필드 없음 §7)
	turns   int64
	use     cxUsage
	cwdAny  bool
	cwdOut  bool
	skipped int64
}

// rolloutNameRe — rollout-<ts>-<uuid>.jsonl. ts는 로컬 시간(§7 실측 — KST 오프셋 대조).
var rolloutNameRe = regexp.MustCompile(`^rollout-(\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2})-([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\.jsonl$`)

// rolloutStartFromName — 파일명에서 세션 시작 시각(관측 창 게이트 §2의 권위 — "발견 수단" 원칙의
// 명시 예외)과 UUID를 파싱한다. 불일치 파일명은 ok=false(스캔 층이 미채택).
func rolloutStartFromName(name string) (time.Time, string, bool) {
	m := rolloutNameRe.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, "", false
	}
	start, err := time.ParseInLocation("2006-01-02T15-04-05", m[1], time.Local)
	if err != nil {
		return time.Time{}, "", false
	}
	return start, m[2], true
}

// parseRolloutFile — rollout 1파일 파싱(§2): 첫 meta의 session_id 권위, 토큰 총계는 max-wins
// (total 최대 스냅샷의 벡터 전체 — 재개 98건 전수 단조 실측 §7, 감소 스냅샷=재전송 간주 무시,
// 리셋 판정·구간 분리 불필요). cwd는 모든 meta를 미해석 Fold 비교(RealPath 비적용 — cc:
// transcriptDirFor의 Canonicalize 회피 선례와 동형)로 루트 하위 여부를 집계한다. 손상·초과 줄은
// skip 집계(cc: sumTranscript 규율). 반환 오류에 절대경로 금지(§12 canary).
func parseRolloutFile(ctx context.Context, path, foldedRoot string) (cxRollout, error) {
	f, err := os.Open(path)
	if err != nil {
		return cxRollout{}, errors.New("usage: rollout 파일 열기 실패")
	}
	defer func() { _ = f.Close() }()
	br := bufio.NewReaderSize(f, 64*1024)
	var r cxRollout
	metaSeen := false
	for lineNo := 0; ; lineNo++ {
		if lineNo%cancelCheckLines == 0 {
			if cerr := ctx.Err(); cerr != nil {
				return r, cerr
			}
		}
		raw, truncated, ferr := readTranscriptLine(br, maxProviderLine)
		switch {
		case truncated:
			r.skipped++
		case len(raw) > 0:
			var head struct {
				Type    string          `json:"type"`
				Payload json.RawMessage `json:"payload"`
			}
			if json.Unmarshal(bytes.TrimRight(raw, "\r\n"), &head) != nil {
				r.skipped++
				break
			}
			switch head.Type {
			case "session_meta":
				var m struct {
					SessionID string `json:"session_id"`
					ID        string `json:"id"`
					Cwd       string `json:"cwd"`
				}
				if json.Unmarshal(head.Payload, &m) != nil {
					r.skipped++
					break
				}
				if !metaSeen {
					metaSeen = true
					r.id = m.SessionID
					if r.id == "" {
						r.id = m.ID
					}
				}
				folded := ident.Fold(m.Cwd)
				if folded == foldedRoot || strings.HasPrefix(folded, foldedRoot+"/") {
					r.cwdAny = true
				} else {
					r.cwdOut = true
				}
			case "event_msg":
				// 숫자 필드는 전부 포인터 — JSON 부재가 0으로 흡수되면 skip=0인 채 평균을
				// 희석한다(형식 변경은 스킵 강등이 계약, §2 — 계획 검수 교정).
				var tc struct {
					Type string `json:"type"`
					Info *struct {
						Total *struct {
							Input       *int64 `json:"input_tokens"`
							CachedInput *int64 `json:"cached_input_tokens"`
							Output      *int64 `json:"output_tokens"`
							Total       *int64 `json:"total_tokens"`
						} `json:"total_token_usage"`
					} `json:"info"`
				}
				if json.Unmarshal(head.Payload, &tc) != nil {
					r.skipped++
					break
				}
				if tc.Type != "token_count" {
					break
				}
				if tc.Info == nil || tc.Info.Total == nil || tc.Info.Total.Input == nil ||
					tc.Info.Total.CachedInput == nil || tc.Info.Total.Output == nil || tc.Info.Total.Total == nil {
					r.skipped++ // 필수 필드 부재 = 형식 변경 — 스킵 강등(§2), turn 미계상
					break
				}
				r.turns++
				if *tc.Info.Total.Total > r.use.total { // max-wins(§2) — 동률은 첫 스냅샷 유지(결정론)
					r.use = cxUsage{input: *tc.Info.Total.Input, cachedInput: *tc.Info.Total.CachedInput,
						output: *tc.Info.Total.Output, total: *tc.Info.Total.Total}
				}
			}
		}
		if ferr != nil {
			if errors.Is(ferr, io.EOF) {
				if !metaSeen || r.id == "" {
					return r, errors.New("usage: rollout meta 없음")
				}
				return r, nil
			}
			return r, errors.New("usage: rollout 스캔 실패")
		}
	}
}
