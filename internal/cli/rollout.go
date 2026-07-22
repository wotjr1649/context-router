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
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/session"
)

type cxUsage struct{ input, cachedInput, output, total int64 }

// cxRollout — rollout 파일 1개의 파싱 결과(세션 그룹은 scanRollouts가 session_id로 병합 — "파일=세션" 폐기, §2).
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
	// 루트 경계 정규화(양쪽 trailing '/' 제거) — foldedRoot가 드라이브/유닉스 루트("c:/"·"/")면
	// root+"/"가 "c://"·"//"가 되어 전 하위 cwd가 배제된다(Codex P2).
	root := strings.TrimSuffix(foldedRoot, "/")
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
				if m.Cwd == "" {
					// cwd 부재/null = 형식 변경 스킵 강등(§2, 필드 부재 규율) — 발산으로 오판해
					// 프로젝트 rollout을 skip=0인 채 조용히 배제하지 않는다(Codex P2).
					r.skipped++
					break
				}
				fc := strings.TrimSuffix(ident.Fold(m.Cwd), "/")
				if fc == root || strings.HasPrefix(fc, root+"/") {
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
					r.use = cxUsage{
						input: *tc.Info.Total.Input, cachedInput: *tc.Info.Total.CachedInput,
						output: *tc.Info.Total.Output, total: *tc.Info.Total.Total,
					}
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

// cxScan — 프로젝트 귀속 rollout의 arm 분류·커버리지 집계(D45 §3 소비).
type cxScan struct {
	on, off, unknown               []cxRollout
	diverged, skipFiles, skipLines int64
	incomplete                     bool
	registered, matched            int
}

// mustAbs — projectRoot의 절대화(실패는 사실상 없음 — 원본 반환, transcriptDirFor 관례).
func mustAbs(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// cxOffWindow — 관측 창(§2): retention 스윕=이벤트만 삭제 + 빈 세션 GC=started_at<now-7d 필수
// (retention_sec 무관)라는 실코드 불변식상, 시작 후 7일 이내 등록 행은 자동 삭제 경로가 없다 —
// 7일 이내 부재만 off 확정 가능(그 밖·DB 불용은 unknown).
const cxOffWindow = 7 * 24 * time.Hour

// loadCXSessions — 프로젝트 worktrees/* 전체 session.db를 read-only 순회 병합해 cx: 세션 집합을
// 만든다(D40 ③ 정합 — [15]와 같은 순회 패턴, loadCCSessions의 단일 worktree 조회 재사용 금지 §2).
// worktree 단위 실패는 그 worktree만 skip + incomplete=true([15] 폴백 정합 — off 확정 금지 신호).
func loadCXSessions(ctx context.Context, storeRoot, projectRoot string) (map[string]bool, bool) {
	// complete 판정은 "진짜 부재"일 때만 — 식별 실패·ReadDir/Stat의 부재 외 오류는 관측 불능이라
	// incomplete=true(off 확정 금지, §2 — 계획 검수 교정: 관측 실패를 complete로 취급하면
	// 최근 미등록 rollout이 거짓 off가 된다).
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil || canon.ProjectID == "" {
		return nil, true
	}
	wtRoot := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees")
	entries, rdErr := os.ReadDir(wtRoot)
	if rdErr != nil && !os.IsNotExist(rdErr) {
		return nil, true
	}
	set := map[string]bool{}
	incomplete := false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		wdir := filepath.Join(wtRoot, e.Name())
		fi, statErr := os.Stat(filepath.Join(wdir, "session.db"))
		if statErr != nil {
			if !os.IsNotExist(statErr) {
				incomplete = true // 권한 등 — 부재만 "세션 미사용"([6]·[15] 원칙)
			}
			continue
		}
		if fi.IsDir() {
			incomplete = true
			continue
		}
		if err := func() error {
			db, openErr := session.OpenReadOnly(wdir)
			if openErr != nil {
				return openErr
			}
			defer func() { _ = db.Close() }()
			rows, qErr := db.QueryContext(ctx, "SELECT session_id FROM sessions WHERE session_id LIKE 'cx:%'")
			if qErr != nil {
				return qErr
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					set[id] = true
				}
			}
			return rows.Err()
		}(); err != nil {
			incomplete = true
		}
	}
	return set, incomplete
}

// scanRollouts — rolloutRoot 재귀 순회(§2): rollout-*.jsonl만 파싱, 모든 meta cwd가 루트 하위인
// 파일만 채택(발산=제외+집계, 타 프로젝트=자연 배제). 채택 파일은 meta session_id로 병합(D44
// c65da3d — 재개·포크가 같은 session_id로 여러 rollout 파일을 만들고 파일 간 누적은 독립 재시작이라
// 파일 단위 dedup은 후속 파일을 버려 과소계상): turns 합·use는 파일별 max-wins 벡터의 필드별 합·
// start는 그룹 최초 파일 ts. arm 분류는 그룹 완성 후 세션 단위 1회(session_id 사전순 결정론 §3):
// cx: 등록=on; 미등록은 !incomplete이고 시작이 7일 창 이내면 off, 그 밖은 unknown. 반환 오류는
// 루트 자체를 못 읽는 경우뿐(호출자가 "rollout 루트 없음" 렌더).
func scanRollouts(ctx context.Context, rolloutRoot, projectRoot string, cxSet map[string]bool, incomplete bool, now time.Time) (cxScan, error) {
	sc := cxScan{incomplete: incomplete, registered: len(cxSet)}
	foldedRoot := ident.Fold(mustAbs(projectRoot))
	if _, err := os.Stat(rolloutRoot); err != nil {
		return sc, errors.New("usage: rollout 루트 없음")
	}
	groups := map[string]*cxRollout{} // meta session_id → 병합 누적(파일=세션 폐기, D44 c65da3d)
	walkErr := filepath.WalkDir(rolloutRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			sc.skipFiles++ // 읽을 수 없는 하위 트리 — 디렉터리 단위 1회 미귀속 계상(침묵 탈락 금지, Codex P2)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		start, _, ok := rolloutStartFromName(d.Name())
		if !ok {
			return nil
		}
		r, perr := parseRolloutFile(ctx, path, foldedRoot)
		if perr != nil {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			sc.skipFiles++ // 파일 단위 실패 — 프로젝트 판별 불가라 "미귀속"으로 렌더(§3, 침묵 탈락 금지)
			return nil
		}
		if !r.cwdAny {
			if !r.cwdOut {
				// 유효 cwd meta 전무(전부 부재/무효) — 프로젝트 판별 불가 = meta 전무와 동일 미귀속(Codex P2).
				sc.skipFiles++
			}
			return nil // cwdOut만(타 프로젝트) → 자연 배제(라인 스킵도 미집계 — 경고 오염 방지)
		}
		sc.skipLines += r.skipped // 프로젝트 귀속(채택·발산) 파일의 라인 스킵만 집계(파일 단위)
		if r.cwdOut {
			sc.diverged++ // cwd 발산 — 제외+집계(§2, 파일 단위)
			return nil
		}
		// session_id 그룹 병합: turns 합·use 필드별 합(파일별 max-wins 벡터의 합)·start 최초 ts.
		g := groups[r.id]
		if g == nil {
			g = &cxRollout{id: r.id, start: start}
			groups[r.id] = g
		}
		g.turns += r.turns
		g.use.input += r.use.input
		g.use.cachedInput += r.use.cachedInput
		g.use.output += r.use.output
		g.use.total += r.use.total
		if start.Before(g.start) {
			g.start = start // 그룹 최초 파일 ts
		}
		return nil
	})
	if walkErr != nil {
		return sc, walkErr // ctx 취소만 도달
	}
	// 분류는 그룹 완성 후 세션 단위 1회 — session_id 사전순 순회(결정론 §3), matched 세션당 1회.
	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	cutoff := now.Add(-cxOffWindow)
	for _, id := range ids {
		g := *groups[id]
		switch {
		case cxSet["cx:"+id]:
			sc.matched++
			sc.on = append(sc.on, g)
		case !incomplete && !g.start.Before(cutoff):
			// inclusive(정확히 7일 전 포함) — GC 술어가 started_at < now-7d 엄격 미만이라
			// 경계 세션의 행 존재도 보장된다(계획 검수 교정).
			sc.off = append(sc.off, g)
		default:
			sc.unknown = append(sc.unknown, g)
		}
	}
	return sc, nil
}
