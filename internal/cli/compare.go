// compare.go — D45 usage --compare(설계 v0.6 §3): 채널(cc/cx)×arm(hooks:on/off) 집계·비율·캐비앗
// 리포트 전용 출력(세션별 본표 미출력 — 대체). 무플래그·--totals 경로는 여기를 전혀 타지 않는다
// (byte-for-byte 게이트). 채널 간 교차 비교는 계약 밖 — 단위(record vs turn)가 다르다.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/session"
)

// ccCompareArm — cc: 한 arm의 세션·합계 누적(--min-records 적용 후).
type ccCompareArm struct {
	sessions int64
	sums     usageSums
}

func avg(sum, n int64) int64 {
	if n == 0 {
		return 0
	}
	return sum / n
}

// ratioStr — on/off 단위당 평균비 %.3f(분모 0·off 평균 0은 n/a — §3).
func ratioStr(onSum, onN, offSum, offN int64) string {
	if onN == 0 || offN == 0 {
		return "n/a"
	}
	offAvg := float64(offSum) / float64(offN)
	if offAvg == 0 {
		return "n/a"
	}
	return strconv.FormatFloat(float64(onSum)/float64(onN)/offAvg, 'f', 3, 64)
}

// defaultRolloutRoot — Codex rollout 소스 루트(§2). HomeDir 실패 시 빈 경로 → "루트 없음" 렌더.
func defaultRolloutRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "sessions")
}

// loadCCSynthetic — D51(v0.9): source="first-event" 합성 등록 cc: 세션 집합(--compare 전용
// 기본 제외 — 구성상 불완전 표본). loadCCSessions 본문을 복제하되 SQL만 교체한다(재사용 금지 —
// loadCCSessions는 무플래그 본표(cli.go:367)와 공유하는 byte-for-byte 게이트). 실패는 빈 집합
// (fail-soft, loadCCSessions와 동형). read-only라 대상 DB를 오염시키지 않는다.
func loadCCSynthetic(ctx context.Context, storeRoot, projectRoot string) map[string]bool {
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil || canon.ProjectID == "" {
		return nil
	}
	sessDir := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)
	if fi, statErr := os.Stat(filepath.Join(sessDir, "session.db")); statErr != nil || fi.IsDir() {
		return nil
	}
	db, err := session.OpenReadOnly(sessDir)
	if err != nil {
		return nil
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, `SELECT s.session_id FROM sessions s JOIN session_events e
		ON e.session_id = s.session_id WHERE s.session_id LIKE 'cc:%'
		AND e.event_type='session_start' AND json_extract(e.payload,'$.source')='first-event'`)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	set := make(map[string]bool)
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			set[id] = true
		}
	}
	return set
}

// runUsageCompare — D45 리포트(§3 출력 계약 — byte 단위 고정). now는 관측 창 게이트의 기준
// 시각(테스트 주입 — 결정론 §6). 반환 오류에 절대경로 금지(§12 canary).
func runUsageCompare(ctx context.Context, w io.Writer, storeRoot, projectRoot, tdir, rolloutRoot string, minRecords int64, now time.Time) error {
	// ── cc 블록: transcript 순회(무플래그 경로와 동일 헬퍼 재사용 — 합산 로직 단일점 D13).
	entries, err := os.ReadDir(tdir)
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New("usage: transcript 디렉터리가 없습니다")
		}
		return errors.New("usage: transcript 디렉터리 열기 실패")
	}
	ccSet := loadCCSessions(ctx, storeRoot, projectRoot)
	ccSyn := loadCCSynthetic(ctx, storeRoot, projectRoot) // D51 — 합성 등록 세션(source=first-event)
	var on, off ccCompareArm
	var ccExcluded, ccSynthetic int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		s, sumErr := sumTranscriptFile(ctx, filepath.Join(tdir, e.Name()))
		if sumErr != nil {
			return sumErr
		}
		if s.records < minRecords {
			ccExcluded++
			continue
		}
		id := "cc:" + strings.TrimSuffix(e.Name(), ".jsonl")
		if ccSyn[id] {
			ccSynthetic++ // D51 — 합성 등록 세션은 양 arm 모두에서 제외(불완전 표본)
			continue
		}
		grp := &off
		if ccSet[id] {
			grp = &on
		}
		grp.sessions++
		grp.sums.input += s.input
		grp.sums.output += s.output
		grp.sums.cacheRead += s.cacheRead
		grp.sums.records += s.records
	}
	fmt.Fprintln(w, "== cc (단위=record) ==")
	fmt.Fprintln(w, "arm\tsessions\trecords\toutput/rec\tcache_read/rec")
	fmt.Fprintf(w, "hooks:on\t%d\t%d\t%d\t%d\n", on.sessions, on.sums.records, avg(on.sums.output, on.sums.records), avg(on.sums.cacheRead, on.sums.records))
	fmt.Fprintf(w, "hooks:off\t%d\t%d\t%d\t%d\n", off.sessions, off.sums.records, avg(off.sums.output, off.sums.records), avg(off.sums.cacheRead, off.sums.records))
	fmt.Fprintf(w, "ratio(on/off)\t-\t-\t%s\t%s\n",
		ratioStr(on.sums.output, on.sums.records, off.sums.output, off.sums.records),
		ratioStr(on.sums.cacheRead, on.sums.records, off.sums.cacheRead, off.sums.records))
	fmt.Fprintf(w, "excluded(--min-records): %d | synthetic=%d\n", ccExcluded, ccSynthetic)

	// ── cx 블록(experimental — 비공표 내부 형식 의존 등급 표명 §5).
	fmt.Fprintln(w, "== cx (단위=turn, experimental) ==")
	cxSet, cxSyn, incomplete := loadCXSessions(ctx, storeRoot, projectRoot)
	sc, scanErr := scanRollouts(ctx, rolloutRoot, projectRoot, cxSet, cxSyn, incomplete, now)
	if scanErr != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		fmt.Fprintln(w, "rollout 루트 없음") // usage 자체는 성공(§2)
	} else {
		type cxArm struct {
			sessions, turns int64
			sums            cxUsage
		}
		var cxExcluded int64
		fold := func(list []cxRollout) cxArm {
			var a cxArm
			for _, r := range list {
				if r.turns < minRecords { // cx는 turns 기준(§3)
					cxExcluded++
					continue
				}
				a.sessions++
				a.turns += r.turns
				a.sums.input += r.use.input
				a.sums.cachedInput += r.use.cachedInput
				a.sums.output += r.use.output
			}
			return a
		}
		aOn, aOff := fold(sc.on), fold(sc.off)
		unknownN := int64(len(sc.unknown))
		fmt.Fprintln(w, "arm\tsessions\tturns\tinput/turn\tcached_input/turn\toutput/turn")
		fmt.Fprintf(w, "hooks:on\t%d\t%d\t%d\t%d\t%d\n", aOn.sessions, aOn.turns, avg(aOn.sums.input, aOn.turns), avg(aOn.sums.cachedInput, aOn.turns), avg(aOn.sums.output, aOn.turns))
		fmt.Fprintf(w, "hooks:off\t%d\t%d\t%d\t%d\t%d\n", aOff.sessions, aOff.turns, avg(aOff.sums.input, aOff.turns), avg(aOff.sums.cachedInput, aOff.turns), avg(aOff.sums.output, aOff.turns))
		warn := ""
		if sc.skipFiles+sc.skipLines > 0 {
			warn = " [경고: 파싱 스킵>0]" // 조용한 cohort 왜곡 차단(§2)
		}
		fmt.Fprintf(w, "ratio(on/off, 대화형(경량) 코호트 한정)%s\t-\t-\t%s\t%s\t%s\n", warn,
			ratioStr(aOn.sums.input, aOn.turns, aOff.sums.input, aOff.turns),
			ratioStr(aOn.sums.cachedInput, aOn.turns, aOff.sums.cachedInput, aOff.turns),
			ratioStr(aOn.sums.output, aOn.turns, aOff.sums.output, aOff.turns))
		inc := ""
		if sc.incomplete {
			inc = " (incomplete)"
		}
		fmt.Fprintf(w, "coverage: on 등록=%d 매칭=%d n/a=%d | off 창내미등록=%d | unknown=%d%s | excluded(--min-records)=%d | synthetic=%d\n",
			sc.registered, sc.matched, sc.registered-sc.matched, len(sc.off), unknownN, inc, cxExcluded, sc.syntheticN)
		fmt.Fprintf(w, "skipped: files(미귀속)=%d lines=%d cwd_diverged=%d\n", sc.skipFiles, sc.skipLines, sc.diverged)
	}
	fmt.Fprintln(w, "주의: 관찰 데이터(무작위화 아님) — 워크로드 교란 존재")
	return nil
}
