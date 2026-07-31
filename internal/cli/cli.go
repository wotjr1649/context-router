// Package cli — doctor·upgrade·stats·purge 진입점. 설계서 §7.
package cli

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	ctrexec "github.com/wotjr1649/context-router/internal/exec"
	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/session"
	"github.com/wotjr1649/context-router/internal/store"
)

// releaseURL: 컴파일타임 상수 — upgrade가 응답에서 취하는 것은 tag_name 버전 문자열뿐이다.
// 응답이 제공하는 URL·명령·기타 필드는 절대 출력하지 않는다(위생, 설계 §7).
const releaseURL = "https://api.github.com/repos/wotjr1649/context-router/releases/latest"

// defaultStoreWarnBytes — D38 store 용량 경고 임계 기본값(설계 v0.4 §5 "기본 100MiB").
const defaultStoreWarnBytes = 100 << 20 // 100MiB

// storeWarnBytes — CTR_STORE_WARN_BYTES 양수만 채택, 파싱 실패·비양수는 기본값(D38 — 측정
// 실체가 CAS 전체 blob이라 STORE 명명).
func storeWarnBytes(getenv func(string) string) int64 {
	if v, err := strconv.ParseInt(getenv("CTR_STORE_WARN_BYTES"), 10, 64); err == nil && v > 0 {
		return v
	}
	return defaultStoreWarnBytes
}

// defaultContentFileWarnBytes — D46 content.db 파일 축 경고 임계 기본값(설계 v0.6 §4 — 가시화
// 트리거: 조용한 기본값이 아니다).
const defaultContentFileWarnBytes = 100 << 20 // 100MiB

// contentFileWarnBytes — CTR_CONTENT_FILE_WARN_BYTES 양수만 채택(storeWarnBytes와 동형 규율).
// blob 키와 분리한 전용 키(D46) — 두 축은 크기·성장·구제 경로가 달라 한쪽 조정이 다른 축을
// 무력화하면 안 된다.
func contentFileWarnBytes(getenv func(string) string) int64 {
	if v, err := strconv.ParseInt(getenv("CTR_CONTENT_FILE_WARN_BYTES"), 10, 64); err == nil && v > 0 {
		return v
	}
	return defaultContentFileWarnBytes
}

// Run: cli 서브커맨드 단일 진입점. storeRoot·projectRoot는 main이 이미 결정해 넘긴다(cli는
// 재도출하지 않는다 — 설계서 §7 Produces). sub은 main이 9개 이름(doctor·upgrade·stats·purge·
// session·hook·usage·codex-hook·version) 중 하나임을 이미 확인했다. args는 doctor·upgrade에서 미사용, stats가 --provider
// 고유 플래그 파싱에 쓴다(전용 flag.NewFlagSet, 설계 §7 — main의 serverFlags와 별개). stderr는
// session export의 worktree 후보 목록·진단 안내 전용(태스크9, §7 stdout purity 게이트 선례 —
// 그 외 서브커맨드는 여전히 미사용). storeRootExplicit/storeRootRaw는 hook install이 --store-root를
// 명시된 경우에만 훅 명령 args에 주입하기 위한 것이다(prescanRootFlags가 토큰을 소비해 cli는
// 명시/기본을 구분할 수 없으므로 main이 전달, 설계 §7).
func Run(ctx context.Context, sub string, args []string, storeRoot, projectRoot, version string, storeRootExplicit bool, storeRootRaw string, stdout, stderr io.Writer) error {
	switch sub {
	case "version":
		// D56 — ProductVersion 1줄(CI 태그 검증·사용자 확인용 추출 표면). 부가 정보 없음 —
		// 상세 commit/dirty는 doctor build 라인 전담(스펙 §0 분리).
		if len(args) > 0 {
			return fmt.Errorf("cli: version: 예상치 않은 인자 %d개", len(args))
		}
		fmt.Fprintln(stdout, version)
		return nil
	case "doctor":
		// D83 이음새 ① — --fix 하나만 받는 자체 flagset. 그 밖의 인자는 종전대로 거부하고,
		// 오류 문면에는 사용자 입력(원시 args)을 에코하지 않는다(규약 §6) — flag 패키지의
		// 오류는 플래그 이름을 담으므로 %w로 감싸지 않는다.
		fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fix := fs.Bool("fix", false, "MCP 등록물의 버전 표식 드리프트를 현재 버전으로 다시 기입한다(기존 파일만)")
		if err := fs.Parse(args); err != nil {
			return errors.New("cli: doctor: 인식할 수 없는 플래그(사용 가능: --fix)")
		}
		if rest := fs.Args(); len(rest) > 0 {
			return fmt.Errorf("cli: doctor: 예상치 않은 인자 %d개", len(rest))
		}
		return runDoctor(ctx, stdout, storeRoot, projectRoot, version, *fix)
	case "upgrade":
		if len(args) > 0 {
			return fmt.Errorf("cli: upgrade: 예상치 않은 인자 %d개", len(args))
		}
		client := &http.Client{Timeout: 10 * time.Second}
		return runUpgrade(stdout, client, releaseURL, version)
	case "stats":
		return runStats(ctx, stdout, args, storeRoot, projectRoot)
	case "usage":
		// transcript 세션별 토큰 집계 + cc: 스트림 대조(hooks on/off) — 읽기 전용(설계 §6, 태스크9).
		return runUsage(ctx, stdout, args, storeRoot, projectRoot)
	case "purge":
		// TTY 판정은 여기서만 한다(cli.Run 시그니처는 불변 — confirmPurge는 그 결과값만
		// 받는 순수 함수, 설계 §7). os.Stdin.Stat() 실패는 TTY 아님으로 취급(비대화형 파이프
		// 등과 동일하게 --force를 요구).
		isTTY := false
		if fi, statErr := os.Stdin.Stat(); statErr == nil {
			isTTY = fi.Mode()&os.ModeCharDevice != 0
		}
		return runPurge(ctx, os.Stdin, stdout, stderr, storeRoot, args, isTTY)
	case "session":
		return runSession(ctx, stdout, stderr, args, storeRoot)
	case "hook":
		// install/uninstall + 러닝 훅(무인자/--no-shadow) 디스패치는 runHook이 소유한다(설계 §7,
		// hook_install.go). 러닝 훅은 항상 exit 0(fail-open §2.3).
		return runHook(ctx, args, storeRoot, storeRootRaw, storeRootExplicit, projectRoot, version, stdout)
	case "codex-hook":
		// Codex 러닝 훅(설계 v0.4 §2 D35) — 항상 exit 0(fail-open §2.3). 전용 서브커맨드 =
		// 구버전 바이너리 오귀속 차단 게이트(§11.2 F3).
		return runCodexHook(ctx, args, storeRoot, version, stdout)
	default:
		return fmt.Errorf("cli: 미지 서브커맨드: %s", sub)
	}
}

// tagNameRe: tag_name 위생 검증(설계 §7) — 영숫자·점·플러스·하이픈만, 1~64자.
var tagNameRe = regexp.MustCompile(`^v?[0-9A-Za-z.+-]{1,64}$`)

// runUpgrade: releaseURL에 GET → JSON tag_name만 취해 current/latest 두 줄 + 설치 안내
// 1줄을 출력한다. 응답이 제공하는 다른 필드(URL·명령 등)는 절대 읽지도 출력하지도 않는다
// (§7 위생). 네트워크 실패·타임아웃·비200·파싱실패·tag_name 위생검증 실패는 전부 동일하게
// "current만 출력하고 nil 반환"으로 수렴한다 — upgrade는 진단 도구가 아니라 안내 도구이므로
// 이런 실패를 사용자 오류로 다루지 않는다(정상 종료).
func runUpgrade(w io.Writer, client *http.Client, releaseURL, current string) error {
	printCurrent := func() { fmt.Fprintf(w, "current: v%s\n", current) }

	resp, err := client.Get(releaseURL)
	if err != nil {
		printCurrent()
		return nil
	}
	defer func() { _ = resp.Body.Close() }() // 응답 다 읽은 뒤 정리 — 실패해도 이미 취할 조치 없음
	if resp.StatusCode != http.StatusOK {
		printCurrent()
		return nil
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || !tagNameRe.MatchString(body.TagName) {
		printCurrent()
		return nil
	}
	printCurrent()
	fmt.Fprintf(w, "latest: %s\n", body.TagName)
	fmt.Fprintln(w, "install: download from the project releases page and replace the binary")
	return nil
}

// runStats: local(ledger.db 집계)과 --provider(transcript JSONL 실측)를 분기한다(설계 §6).
// 두 경로 모두 토큰·달러 환산과 절약률 주장을 출력하지 않는다(§6 차단 항목 — v0.2 A/B
// 게이트 전까지) — local은 바이트 집계만, provider는 실측 토큰 합계만 보여준다.
func runStats(ctx context.Context, w io.Writer, args []string, storeRoot, projectRoot string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	provider := fs.String("provider", "", "Claude Code transcript JSONL 경로 — 실측 토큰 합계만 출력")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("stats: 플래그 파싱 실패: %w", err)
	}
	if rest := fs.Args(); len(rest) > 0 {
		// 위치 인자를 침묵 수용하지 않는다(리뷰 Fix Round 3, item 4) — 개수만 밝힌다(item 5와
		// 동일 원칙, 사용자 입력 원문 에코 금지).
		return fmt.Errorf("stats: 예상치 않은 인자 %d개", len(rest))
	}
	if *provider != "" {
		return runStatsProvider(ctx, w, *provider)
	}
	return runStatsLocal(w, storeRoot, projectRoot)
}

// runStatsLocal: 현재 프로젝트(<storeRoot>/projects/<ProjectID>)의 ledger.db를
// store.LedgerStats로 집계해 tool/calls/bytes_stored/bytes_returned/span(RFC3339) 표를
// 출력한다. 합계 줄 끝에는 고정 문구 "bytes suppressed (local, 진단용)"를 붙인다 — 토큰·달러
// 환산이나 절약률 주장은 여기 어디에도 없다(설계 §6).
func runStatsLocal(w io.Writer, storeRoot, projectRoot string) error {
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		// 원인(err)을 감싸지 않는다 — Canonicalize의 오류는 filepath.Abs/EvalSymlinks발
		// *fs.PathError라 절대경로를 담고 있다(§12 canary). runDoctor([2] 분기)와 동일하게
		// 반환 오류에는 경로 없는 정적 메시지만 남긴다(리뷰 Fix Round 2, Critical).
		return errors.New("stats: 프로젝트 식별 실패")
	}
	projDir := filepath.Join(storeRoot, "projects", canon.ProjectID)
	stats, err := store.LedgerStats(projDir)
	if err != nil {
		return fmt.Errorf("stats: ledger 집계 실패: %w", err)
	}

	fmt.Fprintln(w, "tool\tcalls\tbytes_stored\tbytes_returned\tspan")
	var totalCalls, totalStored, totalReturned int64
	for _, s := range stats {
		span := time.Unix(s.FirstTS, 0).UTC().Format(time.RFC3339) + "~" + time.Unix(s.LastTS, 0).UTC().Format(time.RFC3339)
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%s\n", s.Tool, s.Calls, s.BytesStored, s.BytesReturned, span)
		totalCalls += s.Calls
		totalStored += s.BytesStored
		totalReturned += s.BytesReturned
	}
	fmt.Fprintf(w, "total\t%d\t%d\t%d\tbytes suppressed (local, 진단용)\n", totalCalls, totalStored, totalReturned)
	return nil
}

// providerUsageLine: Claude Code transcript 한 줄 중 관심 필드만 취한다(그 외 필드는 무시,
// 설계 §6). Usage가 포인터인 이유는 "message.usage 키 자체가 없는 줄"과 "값이 0인 usage"를
// 구분해 전자를 skipped로 세기 위해서다.
type providerUsageLine struct {
	Message struct {
		Usage *struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// maxProviderLine: transcript 한 줄의 상한(설계 §6 — 파일 크기와 무관하게 10MB). 이 상한을
// 넘는 한 줄은 파싱을 포기하고 드레인만 한다(readTranscriptLine) — bufio.Scanner의 고정
// 버퍼 방식(예전 구현)은 이 상한을 넘는 줄 하나가 나오면 Scan() 자체가 실패해 그 뒤 정상
// 줄까지 전부 못 읽고 명령 전체가 중단됐다(리뷰 Fix Round 2, Important-3).
const maxProviderLine = 10 << 20

// readTranscriptLine: br에서 개행(\n) 단위로 한 줄을 읽는다. 누적 길이가 max를 넘으면 그
// 순간부터 line을 비우고(메모리에 쌓지 않음) 다음 개행까지 그냥 흘려보낸다(truncated=true로
// 표시) — 호출자는 그 줄을 skipped로 세고 다음 줄로 진행하면 된다. 마지막 줄(개행 없이
// EOF)도 처리한다(err=io.EOF와 함께 그때까지 읽은 조각을 반환).
func readTranscriptLine(br *bufio.Reader, max int) (line []byte, truncated bool, err error) {
	for {
		frag, ferr := br.ReadSlice('\n')
		if !truncated && len(line)+len(frag) > max {
			truncated = true
			line = nil
		}
		if !truncated {
			line = append(line, frag...)
		}
		switch ferr {
		case nil:
			return line, truncated, nil
		case bufio.ErrBufferFull:
			continue // 델리미터를 아직 못 찾음(버퍼 한 채가 다 참) — 계속 이어붙이거나 드레인
		default:
			return line, truncated, ferr
		}
	}
}

// cancelCheckLines: runStatsProvider가 ctx 취소를 확인하는 주기(줄 수) — 리뷰 Fix Round 3
// item 7. 매 줄마다 ctx.Err()를 호출하지 않는 이유는 순전히 비용 절감이며(취소는 어차피
// 사람 타임스케일 이벤트), 상한(maxProviderLine)과 무관하게 파일이 아무리 커도 이 주기마다
// 반드시 취소를 반영한다.
const cancelCheckLines = 256

// usageSums — transcript 한 파일의 usage 합계(설계 §6). runStatsProvider(단일 파일 실측)와
// runUsage(디렉터리 세션별 집계, 태스크9)가 공유한다 — 합산 로직은 sumTranscript 한 곳에만
// 둔다(재구현 금지, 브리프 파서 재사용 계약).
type usageSums struct {
	input, output, cacheRead, cacheCreate, records, skipped int64
}

// sumTranscript: br(한 transcript 파일)을 개행 단위로 스캔해 message.usage의 4개 토큰 필드를
// 합산하고 usage 보유 레코드 수를 센다(설계 §6). 파싱 불가 줄·message.usage 없는 줄·
// maxProviderLine을 넘는 줄은 로그 없이 skipped 카운트만 올린다(마지막 경우는 스캔을
// 중단시키지 않고 계속 진행). readTranscriptLine·providerUsageLine을 재사용한다. ctx가 취소되면
// (cancelCheckLines줄마다 확인) 그 시점까지의 결과와 함께 ctx.Err()를 반환한다 — 오류 문구
// 조립은 호출자 몫이다(runStatsProvider·sumTranscriptFile이 각자의 접두사로 사상).
func sumTranscript(ctx context.Context, br *bufio.Reader) (usageSums, error) {
	var s usageSums
	for lineNo := 0; ; lineNo++ {
		if lineNo%cancelCheckLines == 0 {
			if cerr := ctx.Err(); cerr != nil {
				return s, cerr
			}
		}
		raw, truncated, ferr := readTranscriptLine(br, maxProviderLine)
		switch {
		case truncated:
			s.skipped++
		case len(raw) > 0:
			var line providerUsageLine
			if jsonErr := json.Unmarshal(bytes.TrimRight(raw, "\r\n"), &line); jsonErr != nil || line.Message.Usage == nil {
				s.skipped++
			} else {
				u := line.Message.Usage
				s.input += u.InputTokens
				s.output += u.OutputTokens
				s.cacheRead += u.CacheReadInputTokens
				s.cacheCreate += u.CacheCreationInputTokens
				s.records++
			}
		}
		if ferr != nil {
			if errors.Is(ferr, io.EOF) {
				return s, nil
			}
			return s, ferr
		}
	}
}

// runStatsProvider: path의 Claude Code transcript JSONL을 sumTranscript로 스캔·합산해 실측
// 합계만 출력한다 — 절약 주장·비교 문구 없음(설계 §6). ctx가 취소되면 오류로 중단한다(아주 큰
// transcript 스캔 중 상위 호출자가 취소할 길). 파일 부재·기타 열기 실패·스캔 실패는 반환 오류에
// 절대경로를 담지 않는다(§12 canary).
func runStatsProvider(ctx context.Context, w io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		// *fs.PathError는 경로를 담는다(§12 canary) — errors.Is로 원인 종류만 남기고
		// 경로는 반환 오류에서 제거한다(리뷰 Fix Round 2, Critical). os.ErrNotExist의
		// Error() 문구 자체는 정적("file does not exist")이라 경로가 섞이지 않는다.
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stats provider: 파일이 없습니다: %w", os.ErrNotExist)
		}
		return errors.New("stats provider: 파일 열기 실패")
	}
	defer func() { _ = f.Close() }() // 읽기 전용 — 닫기 실패해도 데이터 유실 없음

	s, err := sumTranscript(ctx, bufio.NewReaderSize(f, 64*1024))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("stats provider: 취소됨: %w", err)
		}
		return errors.New("stats provider: 스캔 실패")
	}

	fmt.Fprintf(w, "input_tokens: %d\n", s.input)
	fmt.Fprintf(w, "output_tokens: %d\n", s.output)
	fmt.Fprintf(w, "cache_read_input_tokens: %d\n", s.cacheRead)
	fmt.Fprintf(w, "cache_creation_input_tokens: %d\n", s.cacheCreate)
	fmt.Fprintf(w, "usage records: %d\n", s.records)
	fmt.Fprintf(w, "skipped: %d\n", s.skipped)
	return nil
}

// transcriptDirRe: Claude Code가 cwd를 transcript 디렉터리명으로 바꾸는 규칙(태스크9 Step 1
// 실측 확정) — **영숫자가 아닌 모든 문자를 '-'로 치환**한다(예: C:\Users\js\Documents\AI_DEV\
// context-router → C--Users-js-Documents-AI-DEV-context-router). plan의 "경로 구분자·콜론만"
// 규칙과 다르다: 밑줄('_')·점('.')도 '-'가 된다(브리프 Step 1 — 실측이 plan과 어긋나면 실측을
// 따른다).
var transcriptDirRe = regexp.MustCompile(`[^A-Za-z0-9]`)

// transcriptDirFor: projectRoot로부터 ~/.claude/projects/<치환명>을 유도한다(--transcripts
// 미지정 시, 설계 §6). Claude Code의 리터럴 문자열 치환을 그대로 재현하므로 ident.Canonicalize
// (심링크 해석)를 쓰지 않는다 — 디스크의 실제 디렉터리명은 심링크 미해석 cwd로 만들어졌다.
// HomeDir 실패 시 빈 경로를 돌려주면 후속 os.ReadDir가 명확한 오류로 표면화한다.
func transcriptDirFor(projectRoot string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	abs, err := filepath.Abs(projectRoot)
	if err != nil {
		abs = projectRoot // Abs 실패는 사실상 없음 — 그래도 원본으로 치환은 진행
	}
	return filepath.Join(home, ".claude", "projects", transcriptDirRe.ReplaceAllString(abs, "-"))
}

// loadCCSessions: 현재 worktree의 session.db에서 cc: 세션 식별자 집합을 읽는다(설계 §6 —
// transcript 파일명 UUID가 cc:<uuid>로 존재 = 그 세션에 훅 스트림이 있었다는 정확 신호).
// session.db가 없거나(훅 미사용) 열기·조회에 실패하면 빈 집합을 돌려준다 — 훅 스트림 증거가
// 없으면 hooks:off로 수렴하는 것이 정확한 읽기(fail-soft). read-only(session.OpenReadOnly)라
// 대상 DB를 오염시키지 않는다. projectRoot는 session.db 경로(pid/wid) 유도에만 쓰며 여기서는
// ident.Canonicalize(정본 식별)를 쓴다 — transcriptDirFor의 리터럴 치환과 목적이 다르다.
func loadCCSessions(ctx context.Context, storeRoot, projectRoot string) map[string]bool {
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil || canon.ProjectID == "" {
		return nil
	}
	sessDir := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)
	if fi, statErr := os.Stat(filepath.Join(sessDir, "session.db")); statErr != nil || fi.IsDir() {
		return nil // session.db 없음 → 전부 hooks:off
	}
	db, err := session.OpenReadOnly(sessDir)
	if err != nil {
		return nil
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, "SELECT session_id FROM sessions WHERE session_id LIKE 'cc:%'")
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

// sumTranscriptFile: path 파일을 열어 sumTranscript로 합산한다(runUsage 디렉터리 순회 보조 —
// 파일마다 자체 defer Close로 즉시 닫아 다중 파일에서 fd가 함수 종료까지 쌓이지 않게 한다).
// ctx 취소는 그대로 전파, 그 외 열기·스캔 실패는 정적 메시지로 사상한다(§12 canary).
func sumTranscriptFile(ctx context.Context, path string) (usageSums, error) {
	f, err := os.Open(path)
	if err != nil {
		return usageSums{}, errors.New("usage: transcript 파일 열기 실패")
	}
	defer func() { _ = f.Close() }()
	s, err := sumTranscript(ctx, bufio.NewReaderSize(f, 64*1024))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return usageSums{}, fmt.Errorf("usage: 취소됨: %w", err)
		}
		return usageSums{}, errors.New("usage: transcript 스캔 실패")
	}
	return s, nil
}

// runUsage: `context-router usage [--transcripts <dir>]` — 읽기 전용 측정 리포트(설계 §6·D27).
// transcript 디렉터리(미지정 시 cwd에서 transcriptDirFor로 유도)의 각 <uuid>.jsonl(= 호스트
// 세션 1개)을 sumTranscript로 스캔해 usage 토큰을 세션별로 합산하고, 파일명 UUID가 현재
// worktree session.db에 cc:<uuid>로 존재하면 hooks:on, 아니면 hooks:off로 표기해 TSV 표로
// 출력한다. 네트워크 없음. 토큰·달러 환산·절약률 주장 없음(§6 — 실측 합계만). 손상 줄은
// sumTranscript 내부에서 skip되어 명령을 중단시키지 않는다.
func runUsage(ctx context.Context, w io.Writer, args []string, storeRoot, projectRoot string) error {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	transcripts := fs.String("transcripts", "", "Claude Code transcript 디렉터리(생략 시 cwd에서 유도)")
	totals := fs.Bool("totals", false, "본표 뒤 hooks:on/off 그룹 집계 2행 추가(설계 v0.3 §5)")
	compare := fs.Bool("compare", false, "채널(cc/cx)×hooks:on/off 비교 리포트(설계 v0.6 D45 — 본표 대체)")
	minRecords := fs.Int64("min-records", 0, "집계 제외 임계(cc=records, cx=turns; --compare 전용)")
	rollouts := fs.String("rollouts", "", "Codex rollout 루트(--compare 전용, 생략 시 ~/.codex/sessions)")
	adoption := fs.Bool("adoption", false, "MCP 서버별 호출·세션 채택 계측(session.db 집계 — transcript 토큰 집계와 별개, 설계 v0.12 D63)")
	days := fs.Int("days", 0, "최근 N일로 제한(--adoption 전용, 0=전체)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: 플래그 파싱 실패: %w", err)
	}
	if rest := fs.Args(); len(rest) > 0 {
		return fmt.Errorf("usage: 예상치 않은 인자 %d개", len(rest))
	}
	if *compare && *totals {
		return errors.New("usage: --compare와 --totals는 함께 쓸 수 없습니다")
	}
	if !*compare && (*minRecords != 0 || *rollouts != "") {
		return errors.New("usage: --min-records/--rollouts는 --compare 전용입니다")
	}
	if *adoption && (*compare || *totals) {
		return errors.New("usage: --adoption은 --compare/--totals와 함께 쓸 수 없습니다")
	}
	if !*adoption && *days != 0 {
		return errors.New("usage: --days는 --adoption 전용입니다")
	}
	if *adoption {
		return runUsageAdoption(ctx, w, storeRoot, projectRoot, *days, time.Now())
	}
	if *compare {
		dir := *transcripts
		if dir == "" {
			dir = transcriptDirFor(projectRoot)
		}
		rroot := *rollouts
		if rroot == "" {
			rroot = defaultRolloutRoot()
		}
		return runUsageCompare(ctx, w, storeRoot, projectRoot, dir, rroot, *minRecords, time.Now())
	}

	dir := *transcripts
	if dir == "" {
		dir = transcriptDirFor(projectRoot)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// §12 canary: os.ReadDir의 *fs.PathError는 절대경로를 담는다 — 원문을 %w로 감싸지
		// 않고 정적 메시지만(runStatsProvider의 파일 부재 처리와 동일 관례).
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("usage: transcript 디렉터리가 없습니다")
		}
		return errors.New("usage: transcript 디렉터리 열기 실패")
	}

	ccSet := loadCCSessions(ctx, storeRoot, projectRoot)

	fmt.Fprintln(w, "session\tinput\toutput\tcache_read\tcache_creation\trecords\thooks")
	var onSum, offSum usageSums // --totals 그룹 누적(hooks:on/off) — 열 구조 불변, 세션 수는 미표시
	for _, e := range entries { // os.ReadDir는 파일명 오름차순 정렬을 보장(결정론)
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		s, sumErr := sumTranscriptFile(ctx, filepath.Join(dir, e.Name()))
		if sumErr != nil {
			return sumErr // ctx 취소·I/O 오류만 여기 도달(손상 줄은 sumTranscript 내부에서 skip)
		}
		uuid := strings.TrimSuffix(e.Name(), ".jsonl")
		hooks := "hooks:off"
		grp := &offSum
		if ccSet["cc:"+uuid] {
			hooks = "hooks:on"
			grp = &onSum
		}
		grp.input += s.input
		grp.output += s.output
		grp.cacheRead += s.cacheRead
		grp.cacheCreate += s.cacheCreate
		grp.records += s.records
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%d\t%s\n", uuid, s.input, s.output, s.cacheRead, s.cacheCreate, s.records, hooks)
	}
	// --totals: 본표 뒤에 그룹 합계 2행만 덧붙인다(무플래그 출력은 byte-for-byte 불변 — 설계 §8 게이트).
	// session 열=그룹 라벨, hooks 열=그룹 라벨, 나머지 5열=토큰·records 합계(열 구조 불변).
	if *totals {
		fmt.Fprintf(w, "TOTAL:hooks:on\t%d\t%d\t%d\t%d\t%d\thooks:on\n", onSum.input, onSum.output, onSum.cacheRead, onSum.cacheCreate, onSum.records)
		fmt.Fprintf(w, "TOTAL:hooks:off\t%d\t%d\t%d\t%d\t%d\thooks:off\n", offSum.input, offSum.output, offSum.cacheRead, offSum.cacheCreate, offSum.records)
	}
	return nil
}

// openSessionDBReadOnly — 현재 worktree의 session.db를 read-only로 연다(loadCCSessions의 열기
// 관례 재사용: canonicalize → sessDir → session.OpenReadOnly). 대상 DB를 오염시키지 않는다.
func openSessionDBReadOnly(storeRoot, projectRoot string) (*sql.DB, error) {
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil || canon.ProjectID == "" {
		// §12 canary: Canonicalize의 *fs.PathError는 절대경로를 담는다 — 정적 메시지만.
		return nil, errors.New("usage: 프로젝트 식별 실패")
	}
	sessDir := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)
	return session.OpenReadOnly(sessDir)
}

// mcpServerOf: "mcp__<server>__<tool>" 형태의 도구 이름에서 서버 네임스페이스를 뽑는다.
// 서버 세그먼트에는 밑줄이 들어갈 수 있으므로(예: plugin_ctxscribe_mcp) 접두 제거 후
// 첫 "__"에서 끊는다. MCP 도구가 아니면 ok=false.
func mcpServerOf(summary string) (string, bool) {
	const prefix = "mcp__"
	if !strings.HasPrefix(summary, prefix) {
		return "", false
	}
	rest := summary[len(prefix):]
	i := strings.Index(rest, "__")
	if i <= 0 || i+2 >= len(rest) {
		return "", false
	}
	return rest[:i], true
}

// runUsageAdoption: session_events(tool_call)를 MCP 서버 네임스페이스 단위로 집계한다(설계
// v0.12 D63 ①). v0.11의 부분 문자열 버킷팅(ctr_)은 mcp__ctr__*와 mcp__ctr-exec__*를 한
// 카운터에 합쳐 서버 등록 형태의 차이를 지웠고, 세션 분모가 없어 "한 세션에서 몰아 쓴 것"과
// "여러 세션에서 고루 쓴 것"이 구분되지 않았다. 지금은 서버별 절대 호출 수 + 그 서버를 부른
// 고유 세션 수를 tool_call 세션 분모와 함께 낸다. ratio 참고 문면은 폐기했다 — 브리지 종료 후
// 분모가 붕괴하는 지표였고 세션 분모가 그 자리를 대신한다. read-only라 대상 DB를 오염시키지
// 않는다.
func runUsageAdoption(ctx context.Context, w io.Writer, storeRoot, projectRoot string, days int, now time.Time) error {
	if days < 0 {
		return errors.New("usage: --days는 음수일 수 없습니다")
	}
	// 사전 점검: session.db가 없으면 "실패"가 아니라 "아직 데이터 없음"이다(loadCCSessions:372
	// 관례 — os.Stat + IsDir). 경로는 메시지에 담지 않는다(§12 canary).
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil || canon.ProjectID == "" {
		return errors.New("usage: 프로젝트 식별 실패")
	}
	sessDir := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)
	if fi, statErr := os.Stat(filepath.Join(sessDir, "session.db")); statErr != nil || fi.IsDir() {
		fmt.Fprintln(w, "# 세션 이벤트가 아직 없습니다(훅 미설치 또는 첫 세션 전) — hook install 후 다시 실행하세요")
		return nil
	}
	db, err := openSessionDBReadOnly(storeRoot, projectRoot) // loadCCSessions의 열기 관례 재사용
	if err != nil {
		return errors.New("usage: 세션 저장소를 열 수 없습니다")
	}
	defer func() { _ = db.Close() }()
	// days 경계는 세 쿼리에 같은 값으로 적용한다 — 호출 수·분자·분모가 서로 다른 창을 서술하면
	// 서버별 세션 수가 분모와 짝이 맞지 않는다.
	var since string
	var args []any
	if days > 0 {
		since = " AND ts >= ?"
		args = append(args, now.AddDate(0, 0, -days).Unix())
	}
	rows, err := db.QueryContext(ctx, "SELECT summary, COUNT(*) FROM session_events WHERE event_type='tool_call'"+since+" GROUP BY summary", args...)
	if err != nil {
		return errors.New("usage: 집계 조회 실패")
	}
	defer func() { _ = rows.Close() }()
	calls := make(map[string]int64) // 서버 네임스페이스 → 절대 호출 수
	var nonMCP int64                // MCP 도구가 아닌 호출(Read·Bash 등) 합
	for rows.Next() {
		var s string
		var n int64
		if err := rows.Scan(&s, &n); err != nil {
			return errors.New("usage: 행 스캔 실패")
		}
		if server, ok := mcpServerOf(s); ok {
			calls[server] += n
			continue
		}
		nonMCP += n
	}
	if err := rows.Err(); err != nil {
		return errors.New("usage: 집계 조회 중단")
	}
	_ = rows.Close() // 다음 조회 전에 연결을 돌려준다(defer의 중복 Close는 무해)
	// 분모: 도구 호출이 하나라도 있는 세션 수(빈 세션 제외 — sessions 테이블 전체가 아니다).
	const qDenom = `SELECT COUNT(DISTINCT session_id) FROM session_events WHERE event_type='tool_call'`
	var denom int64
	if err := db.QueryRowContext(ctx, qDenom+since, args...).Scan(&denom); err != nil {
		return errors.New("usage: 세션 분모 조회 실패")
	}
	// 분자: 서버별로, 그 서버 도구를 한 번이라도 부른 세션 수.
	const qPerServer = `SELECT session_id, summary FROM session_events WHERE event_type='tool_call'`
	srows, err := db.QueryContext(ctx, qPerServer+since, args...)
	if err != nil {
		return errors.New("usage: 세션 집계 조회 실패")
	}
	defer func() { _ = srows.Close() }()
	sessionsBy := make(map[string]map[string]struct{}) // 서버 → 세션 id 집합(고유 세션 수)
	for srows.Next() {
		var sid, s string
		if err := srows.Scan(&sid, &s); err != nil {
			return errors.New("usage: 행 스캔 실패")
		}
		server, ok := mcpServerOf(s) // 행 수가 커질 수 있으므로 summary는 파싱만 하고 보관하지 않는다
		if !ok {
			continue
		}
		if sessionsBy[server] == nil {
			sessionsBy[server] = make(map[string]struct{})
		}
		sessionsBy[server][sid] = struct{}{}
	}
	if err := srows.Err(); err != nil {
		return errors.New("usage: 세션 집계 조회 중단")
	}
	// 출력 결정성: 호출 수 내림차순, 동수는 서버 이름 오름차순 — map 순회 순서가 출력으로
	// 새어 나오면 안 된다.
	servers := make([]string, 0, len(calls))
	for server := range calls {
		servers = append(servers, server)
	}
	sort.Slice(servers, func(i, j int) bool {
		if calls[servers[i]] != calls[servers[j]] {
			return calls[servers[i]] > calls[servers[j]]
		}
		return servers[i] < servers[j]
	})
	fmt.Fprintln(w, "server\tcalls\tsessions")
	for _, server := range servers {
		fmt.Fprintf(w, "%s\t%d\t%d\n", server, calls[server], len(sessionsBy[server]))
	}
	fmt.Fprintf(w, "# tool_call 세션 분모: %d\n", denom)
	fmt.Fprintf(w, "# 비-MCP 도구 호출: %d\n", nonMCP)
	return nil
}

// confirmPurge: 삭제 확인 규칙(설계 §7) — TTY면 expected 슬러그를 보여주고 사용자가 그
// 슬러그를 그대로 입력해야 nil을 반환한다(정적 "yes" 같은 건 없다 — 그냥 문자열 비교라
// expected와 다르면 무엇을 입력해도 오류). 비TTY면 force가 없으면 즉시 오류(in을 전혀 읽지
// 않는다), force가 있으면 즉시 nil(역시 in을 읽지 않는다) — 자동화 경로에서 stdin을 소비하지
// 않기 위함. 순수 함수(설계 §8 규약) — TTY 판정·os.Stdin 소유는 호출자(Run의 purge 분기) 몫.
func confirmPurge(in io.Reader, out io.Writer, isTTY bool, force bool, scopeNote, expected string) error {
	if !isTTY {
		if !force {
			return errors.New("purge: 비TTY 환경에서는 --force 없이 진행할 수 없습니다")
		}
		return nil
	}
	// scopeNote는 삭제 범위 고지 — 호출자가 제공한다. 전체 purge(B2)는 content.db·artifacts와 함께
	// 세션 이벤트 데이터(session.db 계열)도 지운다고 명시하고, --hook-only는 shadow-owned 한정임을
	// 명시한다(세션 이벤트·explicit 소스 보존). 순수 함수라 범위 판정은 호출자 몫(행동 무변).
	fmt.Fprintf(out, "purge: 삭제 대상을 확인합니다(%s). 계속하려면 다음을 그대로 입력하세요: %s\n> ", scopeNote, expected)
	sc := bufio.NewScanner(in)
	if !sc.Scan() {
		return errors.New("purge: 확인 입력을 읽지 못했습니다 — 삭제하지 않았습니다")
	}
	if strings.TrimSpace(sc.Text()) != expected {
		return errors.New("purge: 확인 슬러그가 일치하지 않습니다 — 삭제하지 않았습니다")
	}
	return nil
}

// purgeProjectID: --project 값(ID 또는 경로)을 ProjectID로 정규화한다. 먼저(리뷰 P2-3,
// Fix Round 1) 경로 구분자가 없고 <storeRoot>/projects/<entry>가 실재하면 그 자체로 이미
// store ID이므로 확정하고 경로 해석을 아예 하지 않는다 — 그러지 않으면 cwd에 우연히 동명
// 디렉터리가 있을 때(예: 현재 작업 디렉터리 하위 우연한 이름 충돌) store ID가 경로로
// 오인되어 엉뚱한 프로젝트가 가려진다. 그 외에는 기존 로직: 경로 구분자를 포함하거나
// 실재하는 디렉터리면 경로로 보고 ident.Canonicalize, 그 외는 ID 문자열 그대로 취급한다
// — main.resolveProjectEntry와 동형(설계 §4.6/§7, D13상 서로 다른 패키지라 자체 인터페이스
// 없이 각자 소유).
func purgeProjectID(storeRoot, entry string) (string, error) {
	hasSep := strings.ContainsAny(entry, `/\`)
	if !hasSep {
		if fi, err := os.Stat(filepath.Join(storeRoot, "projects", entry)); err == nil && fi.IsDir() {
			return entry, nil // 이미 store ID로 확정 — 경로 해석 생략
		}
	}
	looksLikePath := hasSep
	if !looksLikePath {
		if fi, err := os.Stat(entry); err == nil && fi.IsDir() {
			looksLikePath = true
		}
	}
	if !looksLikePath {
		return entry, nil
	}
	canon, err := ident.Canonicalize(entry)
	if err != nil {
		return "", err
	}
	return canon.ProjectID, nil
}

// listProjectDirs: <storeRoot>/projects/ 하위 디렉터리 이름(=ProjectID) 목록(--all 대상,
// 설계 §7). projects/ 자체가 없으면(아무 프로젝트도 색인된 적 없음) 빈 목록+nil.
func listProjectDirs(storeRoot string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(storeRoot, "projects"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, errors.New("purge: 프로젝트 목록 조회 실패")
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

// runPurge: purge 서브커맨드(설계 §7). --project/--all 중 정확히 하나. --older-than 지정 시
// 선택 삭제(store.PurgeOlderThan, chunks/FTS 동기+무결성 확인은 store가 책임), 미지정 시
// 프로젝트 디렉터리 전체 삭제(os.RemoveAll). --gc가 --older-than 없이 단독으로 주어지면
// ("GC 단독", 설계 §7) 삭제를 전혀 하지 않고 orphan blob GC만 수행하며 확인을 생략한다 —
// 고아 blob은 정의상 미참조 데이터라 삭제 확인 규칙(데이터 삭제 대상) 밖이다. 그 외 모든
// 경로는 삭제 전 confirmPurge로 확인해야 하며, --gc가 --older-than과 함께면 확인 후
// 삭제→GC 순서로 수행한다. --sessions(설계 §5)는 session.db 파일 계열(purgeSessionFiles)을
// 대상에 추가한다 — --older-than 없이 단독이면 "GC 단독"과 동형으로 content.db는 건드리지
// 않고 세션 파일만 지우되(데이터 삭제라 확인은 생략하지 않는다), --older-than과 함께면
// 선택 삭제 뒤에 이어서 지운다.
func runPurge(ctx context.Context, in io.Reader, w, stderr io.Writer, storeRoot string, args []string, isTTY bool) error {
	fs := flag.NewFlagSet("purge", flag.ContinueOnError)
	project := fs.String("project", "", "purge 대상 프로젝트(ID 또는 경로)")
	all := fs.Bool("all", false, "storeRoot 하위 전체 프로젝트 대상")
	olderThanFlag := fs.String("older-than", "", "time.ParseDuration 형식 — 지정 시 선택 삭제, 미지정 시 전체 삭제")
	gc := fs.Bool("gc", false, "orphan blob GC 수행")
	force := fs.Bool("force", false, "비TTY 환경에서 확인을 생략(자동화 전용)")
	sessions := fs.Bool("sessions", false, "session.db 파일(계열, -wal/-shm 포함) 삭제 대상 포함 — .bak-*·recover-pending 마커는 제외(설계 §5)")
	hookOnly := fs.Bool("hook-only", false, "shadow 귀속(참조가 전부 hook) 아티팩트만 선택 삭제(--project 전용) — content.db·explicit 소스는 보존(설계 §3, D41)")
	vacuum := fs.Bool("vacuum", false, "삭제 후 VACUUM+wal_checkpoint(TRUNCATE)로 content.db 파일 축 회수(--older-than 결합 전용, D50)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("purge: 플래그 파싱 실패: %w", err)
	}
	if rest := fs.Args(); len(rest) > 0 {
		return fmt.Errorf("purge: 예상치 않은 인자 %d개", len(rest))
	}
	// D50 정적 검증 — 파싱 직후 최우선(기존 --project/--all XOR·--hook-only 조기 분기보다 앞,
	// 겹치는 입력은 vacuum 조합 오류가 이긴다 — 설계 v0.8 §0): --vacuum은 content.db 행을 실제로
	// 삭제하는 유일 모드인 --older-than과 결합해서만 유효하다. --gc 단독은 read-only 연결+행
	// 미삭제(이중 부적합), --hook-only는 이미 무조건 VACUUM을 후행한다(D41).
	if *vacuum {
		if *hookOnly {
			return errors.New("purge: --hook-only는 상시 VACUUM을 수행합니다 — --vacuum 불필요")
		}
		if *olderThanFlag == "" {
			return errors.New("purge: --vacuum은 --older-than과 함께 지정해야 합니다")
		}
	}
	// D41 조기 분기: --hook-only는 전역 confirmPurge(아래)·전체 삭제(os.RemoveAll)에 도달하기
	// 전에 전용 경로로 인터셉트한다 — 그러지 않으면(예: --sessions처럼 전역 confirm 뒤에 두면)
	// 확인 프롬프트가 2회 뜬다(설계 §3). --project 단독과만 조합 가능하다.
	if *hookOnly {
		if *all || *olderThanFlag != "" || *sessions || *gc {
			return errors.New("purge: --hook-only는 --project와만 조합할 수 있습니다")
		}
		if *project == "" {
			return errors.New("purge: --hook-only는 --project가 필요합니다")
		}
		return runPurgeHookOnly(ctx, in, w, stderr, storeRoot, *project, *force, isTTY)
	}
	if (*project != "") == *all {
		return errors.New("purge: --project와 --all 중 정확히 하나를 지정해야 합니다")
	}

	var ids []string
	var expected string
	if *all {
		list, err := listProjectDirs(storeRoot)
		if err != nil {
			return err
		}
		if len(list) == 0 { // 리뷰 P2-2: 빈 --all은 즉시 오류(무엇을 삭제할지 모호한 채로 진행 금지)
			return errors.New("purge: 대상 프로젝트 없음")
		}
		ids = list
		expected = fmt.Sprintf("all-%d-projects", len(list))
	} else {
		id, err := purgeProjectID(storeRoot, *project)
		if err != nil {
			return errors.New("purge: 프로젝트 식별 실패")
		}
		ids = []string{id}
		expected = id
	}

	selective := *olderThanFlag != ""
	// --sessions는 §5상 데이터 삭제이므로 "확인 생략" 전용인 gcOnly 모드에는 포함하지
	// 않는다(고아 blob GC와 달리 session 이벤트는 참조되는 1차 데이터) — sessions가
	// 주어지면 selective 여부와 무관하게 항상 confirmPurge를 거친다.
	gcOnly := *gc && !selective && !*sessions
	var cutoffUnix int64
	if selective {
		// 리뷰 P2-1: 파싱 실패 오류는 사용자 입력(*olderThanFlag)을 담은 err를 %w로 감싸지
		// 않는다(정적 메시지만) — 원문 에코 금지. d<=0(음수·0)도 유효한 기간이 아니므로 거부.
		d, err := time.ParseDuration(*olderThanFlag)
		if err != nil || d <= 0 {
			return errors.New("purge: --older-than 값이 유효한 기간이 아님")
		}
		cutoffUnix = time.Now().Add(-d).Unix()
	}

	if !gcOnly { // GC 단독은 데이터 삭제가 아니므로 확인 생략(설계 §7)
		if err := confirmPurge(in, w, isTTY, *force, "전체 삭제는 세션 이벤트 데이터를 포함합니다", expected); err != nil {
			return err
		}
	}

	var vacuumFailed int
	var vacuumDiskAbort bool
	for _, id := range ids {
		if err := ctx.Err(); err != nil { // --all 다중 순회 취소 전파
			return err
		}
		projDir := filepath.Join(storeRoot, "projects", id)
		// 리뷰 P2-2: writable store.Open 전에 대상 존재를 확인한다 — store.Open(dir,false)는
		// MkdirAll+migrate로 존재하지 않던 프로젝트를 그 자리에서 새로 만들어버리고(phantom
		// project), 전체 삭제 분기의 os.RemoveAll은 대상이 없어도 조용히 nil을 반환해 오타를
		// 성공으로 오인시킨다. content.db 존재로 "실재하는 프로젝트"를 판정한다. 이 존재 검사는
		// --project 단일 대상의 오타 방지용이다 — --all은 이미 listProjectDirs가 실재 목록에서
		// 뽑은 id를 순회하므로 오타일 수 없다.
		if _, err := os.Stat(filepath.Join(projDir, "content.db")); err != nil {
			if !*all {
				return errors.New("purge: 대상 프로젝트 없음")
			}
			// 최종리뷰 F3: lock 타임아웃·migrate 실패·크래시가 artifacts/+lock 파일만 남기고
			// content.db는 없는 부분 생성 디렉터리를 남길 수 있다(설계 §3.1). --all 순회
			// 도중 이런 디렉터리를 만나도 배치 전체를 fail-stuck시키지 않는다: 전체삭제
			// 모드(선택 삭제 아님)면 정리 목적에 부합하게 통째로 지운다. 선택 삭제
			// (--older-than)는 indexed_at 판단 근거(content.db)가 없어 지울 수 없으므로
			// skip하고 계속 진행한다 — 나머지 정상 프로젝트가 이 하나 때문에 막히면 안 된다.
			if selective {
				fmt.Fprintf(w, "purge: skip %s (content.db 없음)\n", id)
				continue
			}
			if err := os.RemoveAll(projDir); err != nil {
				return errors.New("purge: 손상된 프로젝트 디렉터리 정리 실패")
			}
			continue
		}

		if gcOnly {
			if err := runGCOrphan(ctx, projDir); err != nil {
				return err
			}
			continue
		}

		if !selective && *sessions {
			// 세션 단독(older-than 없음, --gc 단독과 동형 선례) — content.db는 건드리지
			// 않는다. §5 명문 계약: .bak-*·recover-pending 마커는 purgeSessionFiles가
			// 애초에 대상으로 삼지 않으므로 잔존한다.
			if err := purgeSessionFiles(projDir, stderr); err != nil {
				return err
			}
			if *gc {
				if err := runGCOrphan(ctx, projDir); err != nil {
					return err
				}
			}
			continue
		}

		if !selective { // 전체 삭제
			if err := os.RemoveAll(projDir); err != nil {
				return errors.New("purge: 프로젝트 삭제 실패")
			}
			continue
		}

		// 선택 삭제 (+ 후속 --sessions, + 후속 --gc, + 후속 --vacuum(D50))
		var beforeB int64
		if *vacuum {
			beforeB = contentFootprint(projDir) // 전 실측 = 명령 착수 전 기준점 — 보고 Δ는 명령 전체(삭제+VACUUM+checkpoint)의 총점유 순감소다
		}
		st, err := store.Open(projDir, false)
		if err != nil {
			return err
		}
		_, _, purgeErr := st.PurgeOlderThan(ctx, cutoffUnix)
		if purgeErr == nil && *sessions {
			purgeErr = purgeSessionFiles(projDir, stderr)
		}
		if purgeErr == nil && *gc {
			_, purgeErr = st.GCOrphanBlobs(ctx)
		}
		if purgeErr == nil && *vacuum && !vacuumDiskAbort {
			// D50 집계 계약: VACUUM/checkpoint 실패는 프로젝트별 보고 후 계속 진행하고 루프
			// 종료 시 비-zero로 집계한다(무성 성공 위장 방지). 디스크 계열(FULL/IOERR)만
			// 잔여 프로젝트 VACUUM 중단(연쇄 악화 방지).
			if verr := vacuumReclaim(ctx, st, projDir, beforeB, w); verr != nil {
				fmt.Fprintf(stderr, "ctr: %s: %v\n", id, verr)
				vacuumFailed++
				if store.IsDiskErr(verr) {
					vacuumDiskAbort = true
				}
			}
		} else if purgeErr == nil && *vacuum && vacuumDiskAbort {
			// 앞선 프로젝트의 디스크 계열 실패로 잔여 VACUUM이 중단된 상태 — 이 프로젝트는
			// 시도조차 하지 않았음을 통지한다(집계 미변경: vacuumFailed 미증가). else 분기라
			// 디스크 실패를 처음 유발한 프로젝트가 "생략"으로 이중 통지되지 않는다.
			fmt.Fprintf(stderr, "ctr: %s: VACUUM 생략(디스크 계열 실패로 잔여 중단)\n", id)
		}
		closeErr := st.Close()
		if purgeErr != nil {
			return purgeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if vacuumFailed > 0 {
		return fmt.Errorf("purge: %d개 프로젝트 VACUUM/checkpoint 실패", vacuumFailed)
	}
	return nil
}

// contentFootprint — content.db+(-wal/-shm) 총합 바이트(부재=0). D50 보고·검증은 main 파일
// 단독이 아니라 총합으로 한다 — WAL 모드에서 VACUUM 직후 main 단독 실측은 회수 0/WAL 팽창을
// 성공으로 오인시킨다(설계 v0.8 §0·§5 실험 근거).
func contentFootprint(projDir string) int64 {
	var total int64
	for _, suf := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(filepath.Join(projDir, "content.db"+suf)); err == nil && !fi.IsDir() {
			total += fi.Size()
		}
	}
	return total
}

// vacuumReclaim — D50: VACUUM → wal_checkpoint(TRUNCATE) busy 검증 → 총합 보고. busy 계열
// (VACUUM 경합·checkpoint 미완료)은 "라이브 프로세스" 안내로 매핑한다. 어느 실패든 이미 커밋된
// 삭제분은 유지된다(호출자가 되돌리지 않음 — 설계 v0.8 §0). VACUUM은 원본 크기급 임시 공간을
// 요구한다(도그푸딩 ~100MiB → 여유 ~2배 권고) — 부족 시 SQLITE_FULL/IOERR 실패(무손상)로
// 표면화되고 --all에서는 잔여 프로젝트 VACUUM 중단 사유다(§0 임시 공간 주석).
func vacuumReclaim(ctx context.Context, st *store.Store, projDir string, beforeB int64, w io.Writer) error {
	if err := st.Vacuum(ctx); err != nil {
		if store.IsBusyErr(err) {
			return errors.New("purge: VACUUM 경합 — 라이브 프로세스 가동 중 추정 — 종료 후 재시도")
		}
		return err
	}
	busy, _, _, err := st.CheckpointTruncate(ctx)
	if err != nil {
		return err
	}
	if busy != 0 {
		return errors.New("purge: checkpoint 미완료 — 라이브 프로세스 가동 중 추정 — 종료 후 재시도")
	}
	after := contentFootprint(projDir)
	fmt.Fprintf(w, "purge: content.db(+wal/shm) %dB → %dB (파일 축 회수 %dB)\n", beforeB, after, beforeB-after)
	return nil
}

// runPurgeHookOnly: purge --hook-only 전용 경로(설계 §3, D41). shadow 귀속(그 hash를 참조하는
// 소스가 전부 hook) 아티팩트만 삭제하고 content.db·explicit 소스는 보존한다. 흐름 순서 고정:
// ⓪ store open 선행(견적) — 이 분기 한정으로 현행 confirm→open을 open→confirm으로 재배치한다
// (견적 문구를 confirm에 담기 위해). 견적은 store.SizeStats(projDir)의 ShadowOwned 물리 바이트
// 합·hash 수다(별도 견적 함수 없음). ① confirmPurge(견적 문구) → ② PurgeHookOnly → ④ 실회수
// 보고 먼저 → ⑤ VACUUM 후행(D55: vacuumReclaim 합류 — 실패는 rc≠0로 전파·이미 커밋된 삭제분은
// 유지, 부분 성공 노출을 VACUUM 뒤로 미루지 않는다). SizeStats는 content.db가 없으면 (nil,nil)이라, phantom 프로젝트(store.Open이
// 없는 대상을 새로 생성)를 막기 위해 열기 전 content.db 실재를 먼저 판정한다(runPurge와 동형).
func runPurgeHookOnly(ctx context.Context, in io.Reader, w, stderr io.Writer, storeRoot, project string, force, isTTY bool) error {
	id, err := purgeProjectID(storeRoot, project)
	if err != nil {
		return errors.New("purge: 프로젝트 식별 실패")
	}
	projDir := filepath.Join(storeRoot, "projects", id)
	if _, err := os.Stat(filepath.Join(projDir, "content.db")); err != nil {
		return errors.New("purge: 대상 프로젝트 없음")
	}
	var estBytes int64
	var estHashes int
	if sz, err := store.SizeStats(projDir); err != nil {
		return err
	} else if sz != nil {
		for _, b := range sz.ShadowOwned {
			estBytes += b
		}
		estHashes = len(sz.ShadowOwned)
	}
	if err := confirmPurge(in, w, isTTY, force, "shadow-owned 아티팩트만 삭제 — 세션 이벤트·explicit 소스는 보존",
		fmt.Sprintf("shadow %dB(%d hashes) 선택 삭제", estBytes, estHashes)); err != nil {
		return err
	}
	st, err := store.Open(projDir, false)
	if err != nil {
		return err
	}
	beforeB := contentFootprint(projDir) // D55: open 후·PurgeHookOnly 전 — 삭제+VACUUM 효과 격리(스펙 §0)
	rep, purgeErr := st.PurgeHookOnly(ctx)
	var vacErr error
	if purgeErr == nil {
		// ④ 실회수 보고 먼저(스펙 §3 순서) — VACUUM 성패와 무관하게 부분 성공을 즉시 노출한다.
		fmt.Fprintf(w, "hook-only purge: 실회수 %dB(%d hashes), 유예 %d건, 실패 %d건\n",
			rep.ReclaimedB, rep.Hashes, rep.DeferredFiles, rep.FailedFiles)
		// ⑤ D55: vacuumReclaim 합류 — checkpoint busy 검증·총합 보고, 실패는 rc≠0(본경로 동일).
		// 이미 커밋된 삭제분은 유지된다(vacuumReclaim 계약 — 호출자 미롤백).
		vacErr = vacuumReclaim(ctx, st, projDir, beforeB, w)
	}
	closeErr := st.Close()
	if purgeErr != nil {
		return purgeErr
	}
	if vacErr != nil {
		return vacErr
	}
	return closeErr
}

// runGCOrphan: read-only store로 orphan blob GC만 수행한다(gcOnly·세션 단독+--gc 두 경로
// 공용 — 둘 다 DB 쓰기가 없는 조회 기반 삭제라 read-only로 충분하다).
func runGCOrphan(ctx context.Context, projDir string) error {
	st, err := store.Open(projDir, true)
	if err != nil {
		return err
	}
	_, gcErr := st.GCOrphanBlobs(ctx)
	closeErr := st.Close()
	if gcErr != nil {
		return gcErr
	}
	return closeErr
}

// sessionDBFiles: session.db 파일 계열(설계 §5 "session.db 파일 삭제 계열(-wal/-shm 포함)").
// session.lock/session.init.lock(활성 프로세스 조정 파일)과 session.db.bak-<ts>·
// session.recover-pending(수동 복구 자산)은 의도적으로 이 목록에 없다 — purgeSessionFiles는
// 정확히 이 3개 이름만 지운다(글롭·접두사 매칭 없음 — 우발적 확장 삭제 방지).
var sessionDBFiles = [3]string{"session.db", "session.db-wal", "session.db-shm"}

// purgeSessionFiles: projDir/worktrees/*/ 하위 각 worktree 디렉터리에서 sessionDBFiles만
// 삭제한다(설계 §5 purge 확장 — 경로 관례는 설계 §2.1 `projects/<pid>/worktrees/<wid>/
// session.db`). worktrees/ 디렉터리가 아직 없으면(session 기능 미사용 프로젝트) 조용히
// 통과한다. `.bak-<ts>` 파일·session.recover-pending 마커는 이름이 sessionDBFiles와 정확히
// 일치하지 않으므로 자연히 보존된다(명문 계약, §5).
func purgeSessionFiles(projDir string, stderr io.Writer) error {
	root := filepath.Join(projDir, "worktrees")
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return errors.New("purge: worktrees 목록 조회 실패")
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		wtDir := filepath.Join(root, e.Name())
		// B1(Codex P1): 삭제 전 그 worktree의 session.lock에 exclusive를 시도한다 — 실패(서버가
		// shared lease 보유 중·복구 CLI가 exclusive 보유 중)면 그 worktree는 스킵하고 stderr로
		// 고지한다. unix의 unlink-while-open은 열린 서버가 계속 append하는 파일을 지워 이벤트를
		// 유실시키고, 복구와도 경합하기 때문. release는 비멱등(store.AcquireLock 계약)이라 삭제
		// 직후 정확히 1회 호출한다.
		release, lockErr := store.AcquireLock(filepath.Join(wtDir, session.LockFileName), false)
		if lockErr != nil {
			fmt.Fprintf(stderr, "purge: skip worktree %s (session.lock 점유 — 활성 서버/복구 중)\n", e.Name())
			continue
		}
		delErr := removeSessionDBFiles(wtDir)
		release()
		if delErr != nil {
			return delErr
		}
	}
	return nil
}

// removeSessionDBFiles: 한 worktree에서 sessionDBFiles(session.db 계열)만 삭제한다 —
// purgeSessionFiles가 exclusive lease를 잡은 상태에서만 호출한다.
func removeSessionDBFiles(wtDir string) error {
	for _, name := range sessionDBFiles {
		if err := os.Remove(filepath.Join(wtDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("purge: session.db 삭제 실패")
		}
	}
	return nil
}

// runSession: "session" 서브커맨드 내부 디스패치(설계 §6.3·§7, 태스크9). args[0]이 하위
// 서브커맨드 이름이다 — 9a는 "export", 9b는 "recover"를 구현한다.
func runSession(ctx context.Context, stdout, stderr io.Writer, args []string, storeRoot string) error {
	if len(args) == 0 {
		return errors.New("cli: session: 서브커맨드 필요 (export|recover)")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "export":
		return runSessionExport(ctx, stdout, stderr, rest, storeRoot)
	case "recover":
		return runSessionRecover(stderr, rest, storeRoot)
	default:
		return fmt.Errorf("cli: session: 미지 서브커맨드: %s", sub)
	}
}

// runSessionRecover: session recover 서브커맨드(설계 §6.3 7단계·§7). --project는 필수,
// --worktree는 worktree가 정확히 1개일 때만 생략 가능(export와 동일한 worktree 특정 계약,
// resolveWorktreeID 재사용). 실제 마커·인양·게시 루프는 session.Recover가 소유한다(cli는
// 플래그 해석·프로젝트/worktree 배선·결과를 stderr 문구로 조립하는 것까지만 — 규약 소유
// 경계, 태스크9b). stdout에는 아무것도 쓰지 않는다(CLI 결과 전용 규약상 recover는 stdout
// 출력이 없는 것이 안전 기본, stderr에만 진행 보고).
func runSessionRecover(stderr io.Writer, args []string, storeRoot string) error {
	fs := flag.NewFlagSet("session recover", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	project := fs.String("project", "", "대상 프로젝트(ID 또는 경로)")
	worktree := fs.String("worktree", "", "대상 worktree(ID 또는 경로) — 생략은 worktree가 1개일 때만 허용")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("session recover: 플래그 파싱 실패: %w", err)
	}
	if rest := fs.Args(); len(rest) > 0 {
		return fmt.Errorf("session recover: 예상치 않은 인자 %d개", len(rest))
	}
	if *project == "" {
		return errors.New("session recover: --project 필수")
	}

	pid, err := purgeProjectID(storeRoot, *project)
	if err != nil {
		return errors.New("session recover: 프로젝트 식별 실패")
	}
	projDir := filepath.Join(storeRoot, "projects", pid)

	wid, err := resolveWorktreeID(stderr, projDir, *worktree)
	if err != nil {
		return err
	}
	dbDir := filepath.Join(projDir, "worktrees", wid)

	// 부채정리 ④: 지정한 worktree 디렉터리가 아예 없으면(잘못된 --worktree id) 아래 session.db·
	// 복구 자산 판정이 opaque하게 실패하므로(HasRecoverArtifacts의 dir 읽기 실패), 먼저 후보
	// worktree id를 안내한다(listWorktreeDirs 재사용, resolveWorktreeID 다중 후보 분기와 동형).
	if fi, statErr := os.Stat(dbDir); statErr != nil || !fi.IsDir() {
		ids, _ := listWorktreeDirs(projDir)
		if len(ids) > 0 {
			return fmt.Errorf("session recover: worktree %q 없음 — 후보: %s", wid, strings.Join(ids, ", "))
		}
		return fmt.Errorf("session recover: worktree %q 없음 — 이 프로젝트에 worktree가 없습니다", wid)
	}

	// 최종리뷰 A1(Critical): session.db가 없어도 복구 자산(마커·인양본·백업 main)이 있으면
	// 게시(⑥) rename 도중 crash로 session.db만 사라진 상태이므로 session.Recover에 위임한다
	// (Recover가 모든 잔여 상태 판정을 소유 — T9b 재개 분기가 CLI로 도달 가능해야 영구 wedge가
	// 재도입되지 않는다). 어느 자산도 없을 때만 "없음"으로 거부한다.
	fi, statErr := os.Stat(filepath.Join(dbDir, "session.db"))
	if statErr != nil || fi.IsDir() {
		hasAssets, artErr := session.HasRecoverArtifacts(dbDir)
		if artErr != nil {
			return errors.New("session recover: 복구 자산 확인 실패")
		}
		if !hasAssets {
			return errors.New("session recover: session.db 없음")
		}
	}

	result, err := session.Recover(dbDir)
	if err != nil {
		// session.Recover의 오류는 이미 "session recover: ..."로 자기서술적이다(recover.go) —
		// 여기서 다시 감싸면 접두사가 중복된다(runSessionExport가 session.OpenReadOnly를 감쌀
		// 때와 달리, 이 경우는 서브커맨드 이름이 완전히 동일해 추가 문맥이 없다).
		return err
	}

	switch {
	case result.NoOp:
		fmt.Fprintln(stderr, "session recover: 손상 아님 — 조치 없음")
	case result.MarkerOnly:
		fmt.Fprintln(stderr, "session recover: 이미 게시 완료 — 마커만 삭제했습니다")
	default:
		fmt.Fprintf(stderr, "session recover: 인양 완료 — events=%d sessions=%d backup=%s\n",
			result.RecoveredEvents, result.RecoveredSessions, result.BackupPrefix)
	}
	return nil
}

// runSessionExport: session export 서브커맨드(설계 §6.3 export 부분·§7). --project는 필수
// (purge와 동일한 자체 flag.NewFlagSet 관례 — main이 prescan한 --root는 session에 쓰지 않는다).
// --worktree는 해당 프로젝트에 worktree가 정확히 1개일 때만 생략할 수 있다(resolveWorktreeID —
// worktree 특정 계약). stdout에는 EventV1 JSONL만 쓴다(UTF-8 no BOM, LF) — 진단·후보 목록은
// stderr(stdout purity 게이트 선례). session.Open이 아니라 session.OpenReadOnly로 열어 export
// 대상 DB에 session_start 이벤트가 섞이지 않게 한다(브리프 명시 지침).
func runSessionExport(ctx context.Context, stdout, stderr io.Writer, args []string, storeRoot string) error {
	fs := flag.NewFlagSet("session export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	project := fs.String("project", "", "대상 프로젝트(ID 또는 경로)")
	worktree := fs.String("worktree", "", "대상 worktree(ID 또는 경로) — 생략은 worktree가 1개일 때만 허용")
	sessionID := fs.String("session", "", "세션 ID 필터, 생략 시 worktree 전체")
	after := fs.Int64("after", 0, "커서(rowid), 생략 시 처음부터")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("session export: 플래그 파싱 실패: %w", err)
	}
	if rest := fs.Args(); len(rest) > 0 {
		return fmt.Errorf("session export: 예상치 않은 인자 %d개", len(rest))
	}
	if *project == "" {
		return errors.New("session export: --project 필수")
	}

	pid, err := purgeProjectID(storeRoot, *project) // 기존 --project 해석 헬퍼 재사용(브리프 지침)
	if err != nil {
		return errors.New("session export: 프로젝트 식별 실패")
	}
	projDir := filepath.Join(storeRoot, "projects", pid)

	wid, err := resolveWorktreeID(stderr, projDir, *worktree)
	if err != nil {
		return err
	}
	dbDir := filepath.Join(projDir, "worktrees", wid)

	if fi, statErr := os.Stat(filepath.Join(dbDir, "session.db")); statErr != nil || fi.IsDir() {
		return errors.New("session export: session.db 없음")
	}

	reader, err := session.OpenReadOnly(dbDir)
	if err != nil {
		return fmt.Errorf("session export: %w", err)
	}
	defer func() { _ = reader.Close() }() // read-only export 전용 — 닫기 실패해도 데이터 유실 없음

	return exportJSONL(ctx, stdout, reader, *sessionID, *after)
}

// exportJSONL: session.Export를 rowid 커서로 반복 호출해(전량 소진까지) EventV1을 행당
// json.Marshal → w에 JSONL로 쓴다(설계 §7 — MCP 도구의 max_return_bytes 예산 절단은 CLI
// export에는 없다, 매핑은 EventV1 1곳, D16).
func exportJSONL(ctx context.Context, w io.Writer, reader *sql.DB, sessionID string, after int64) error {
	const batchLimit = 500 // ponytail: 배치 크기 상수 — 튜닝 필요해지면 그때 플래그화
	cur := after
	for {
		events, next, err := session.Export(ctx, reader, cur, sessionID, batchLimit)
		if err != nil {
			return fmt.Errorf("session export: %w", err)
		}
		for _, ev := range events {
			b, mErr := json.Marshal(ev)
			if mErr != nil {
				return fmt.Errorf("session export: JSON 인코딩 실패: %w", mErr)
			}
			b = append(b, '\n')
			if _, wErr := w.Write(b); wErr != nil {
				return fmt.Errorf("session export: 출력 실패: %w", wErr)
			}
		}
		if len(events) == 0 || next == cur {
			return nil
		}
		cur = next
	}
}

// resolveWorktreeID: --worktree 값(ID 또는 경로)을 worktrees/ 하위 디렉터리 이름으로
// 정규화한다(설계 §7 worktree 특정 계약 — main.resolveProjectEntry·cli.purgeProjectID와 동형인
// "ID 우선, 그다음 경로" 판별, D13상 각자 소유). entry가 비어 있으면 projDir/worktrees/ 하위
// 후보가 정확히 1개일 때만 그 이름을 쓰고, 0개나 다중이면 후보 목록을 stderr에 출력한 뒤
// 오류를 반환한다 — --project만으로는 대상 session.db가 결정되지 않는다.
func resolveWorktreeID(stderr io.Writer, projDir, entry string) (string, error) {
	if entry != "" {
		hasSep := strings.ContainsAny(entry, `/\`)
		if !hasSep {
			if fi, statErr := os.Stat(filepath.Join(projDir, "worktrees", entry)); statErr == nil && fi.IsDir() {
				return entry, nil // 이미 worktree ID로 확정 — 경로 해석 생략
			}
		}
		looksLikePath := hasSep
		if !looksLikePath {
			if fi, statErr := os.Stat(entry); statErr == nil && fi.IsDir() {
				looksLikePath = true
			}
		}
		if !looksLikePath {
			return entry, nil // ID 문자열 그대로(미존재면 후속 session.db 오픈에서 오류로 표면화)
		}
		canon, err := ident.Canonicalize(entry)
		if err != nil {
			// Canonicalize의 원인은 *fs.PathError라 절대경로를 담는다(§12 canary) — 원문을
			// 감싸지 않고 정적 메시지만 남긴다(runStatsLocal·purgeProjectID 호출부와 동일 관례).
			return "", errors.New("session: worktree 식별 실패")
		}
		return canon.WorktreeID, nil
	}

	ids, err := listWorktreeDirs(projDir)
	if err != nil {
		return "", err
	}
	switch len(ids) {
	case 0:
		return "", errors.New("session: 이 프로젝트에 worktree가 없습니다")
	case 1:
		return ids[0], nil
	default:
		fmt.Fprintln(stderr, "session: worktree가 여럿입니다 — --worktree 지정 필요, 후보:")
		for _, id := range ids {
			fmt.Fprintf(stderr, "  %s\n", id)
		}
		return "", errors.New("session: worktree가 여럿입니다 — --worktree 지정 필요")
	}
}

// listWorktreeDirs: projDir/worktrees/ 하위 디렉터리 이름(=worktree ID) 목록. listProjectDirs와
// 동형(대상 하위경로만 다름) — worktrees/ 자체가 없으면(session 기능 미사용 프로젝트) 빈
// 목록+nil.
func listWorktreeDirs(projDir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(projDir, "worktrees"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, errors.New("session: worktrees 목록 조회 실패")
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

// probeWritable: dir에 임시 파일을 만들고 즉시 지워 쓰기 가능 여부만 확인한다 — dir 자체를
// 생성하지 않는다(doctor no-create 원칙).
func probeWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".ctr-doctor-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()       // 프로브 전용 임시파일 — 닫기 실패해도 아래 Remove로 정리 시도는 계속한다
	_ = os.Remove(name) // best-effort 정리 — 실패해도 쓰기 가능 여부 판정(true)에는 영향 없음
	return true
}

// nearestExistingDir: path의 조상 중 실제로 존재하는 가장 가까운 항목을 찾는다. 그 항목이
// 디렉터리면 (경로, true)를 반환해 store.Open의 MkdirAll(path/artifacts, ...)이 거기서부터
// 중간 디렉터리를 만들 수 있는지(설계 §3.1) probeWritable로 확인하게 한다. 그 항목이
// 디렉터리가 아닌 일반 파일이면(즉 os.Stat(path)이 ENOTDIR로 실패할 만큼 중간 조상이
// 비디렉터리인 경우 포함) (경로, false)를 반환한다 — MkdirAll은 그 비디렉터리 조상을 뚫고
// 지나갈 수 없어 절대 성공하지 못하므로, 그 이상 위로 계속 올라가 엉뚱한 "쓰기 가능한 먼
// 조상"을 찾아 성공으로 오판하면 안 된다(최종리뷰 F6 — 예전 구현은 `err == nil &&
// fi.IsDir()`를 종료 조건으로 삼아 비디렉터리 조상을 그냥 지나쳐버렸다). 딱 한 단계 위가
// 아니라 실제로 존재하는 가장 가까운 조상까지 오르는 이유는 여러 단계가 한꺼번에
// 미생성인 신규 배치를 다루기 위해서다(리뷰 Fix Round 3, item 2).
func nearestExistingDir(path string) (dir string, isDir bool) {
	dir = path
	for {
		if fi, err := os.Stat(dir); err == nil {
			return dir, fi.IsDir()
		}
		parent := filepath.Dir(dir)
		if parent == dir { // 루트 도달 — 더 못 올라감(사실상 발생하지 않음, 방어적 종료)
			return dir, false
		}
		dir = parent
	}
}

// probeFTS5: reader(열려있는 content.db 연결)가 있으면 그 reader로, 없으면(content.db
// 미존재) :memory: 연결로 fts5 모듈 등록 여부를 순수 SELECT로 확인한다. CREATE VIRTUAL
// TABLE 방식은 채택하지 않는다 — reader는 store.Open(dir,true)의 mode=ro&
// _pragma=query_only(ON) 연결이라 TEMP 스키마 생성조차 SQLITE_READONLY로 거부되므로,
// 이미 초기화된 프로젝트에서 doctor가 항상 fts5 불가로 오판했다(리뷰 발견 버그). 순수
// SELECT는 read-only 연결에서도 항상 동작한다.
func probeFTS5(ctx context.Context, reader *sql.DB) error {
	db := reader
	if db == nil {
		var err error
		db, err = sql.Open("sqlite", ":memory:")
		if err != nil {
			return fmt.Errorf("fts5 probe: %w", err)
		}
		defer func() { _ = db.Close() }() // :memory: 프로브 전용 — 닫기 실패해도 영향 없음
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_module_list WHERE name='fts5'").Scan(&count); err != nil {
		return fmt.Errorf("fts5 probe: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("fts5 probe: 모듈 미등록")
	}
	return nil
}

// shadowIndexNames — D73 대상 색인 이름. store의 DDL과 같은 집합을 doctor가 센다. 이름을 여기에
// 복사해 두는 것은 store 내부 변수를 진단이 읽지 않기 위한 것이다(패키지 경계 유지).
var shadowIndexNames = []string{
	"idx_sources_artifact_kind",
	"idx_sources_blobhash_kind",
	"idx_sources_artifact_indexed",
}

// countShadowIndexes — read-only 프로브. 조회 실패는 0으로 센다(진단은 fail-soft).
func countShadowIndexes(ctx context.Context, db *sql.DB) int {
	n := 0
	for _, name := range shadowIndexNames {
		var got string
		if err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&got); err == nil {
			n++
		}
	}
	return n
}

// hostSnippet: doctor 마지막에 출력하는 호스트 등록 안내(설계 §9) — Claude Code(.mcp.json +
// ingest/net/global ask 규칙)와 Codex(config.toml 기본 6-도구 프로필). exec는 기본 OFF·프로필
// opt-in(--enable exec, D58)이라 활성 예시와 enabled_tools 확장을 병기하되, 승인 강도는 호스트
// 권한 모드가 정하게 둔다 — exec를 ask에 넣지 않는다(D64).
const hostSnippet = `--- host adapter snippets (설계 §9) ---

## Claude Code (.mcp.json — 단일 서버가 표준이다, D63 ②)
{
  "mcpServers": {
    "ctr-exec": { "command": "context-router", "args": ["--enable", "ingest,net"], "alwaysLoad": true }
  }
}
# 이 블록은 "context-router hook install"이 자동으로 병합한다(기본 프로필 ingest,net · exec는 --enable-exec opt-in).
# alwaysLoad는 Claude Code v2.1.121 이상에서만 동작한다 — 그 이전 호스트는 이 필드를 조용히 무시한다.
# 과거의 "ctr" 등록은 이 항목의 도구 집합에 완전히 포함된다 — 함께 두면 6개 도구가 중복
# 노출되므로 "context-router hook install"이 자동으로 제거한다.
# 모든 프로젝트에서 쓰려면 프로젝트 .mcp.json이 아니라 사용자 스코프로 등록한다 — 사용자 스코프
# 서버는 ~/.claude.json에 저장돼 프로젝트를 가로질러 쓰이고, enabledMcpjsonServers 승인이 필요 없다
# (그 키는 저장소가 제공하는 프로젝트 .mcp.json 서버를 승인하는 장치다):
#   claude mcp add --scope user ctr-exec -- context-router --enable ingest,net
# -- 는 claude 자신의 플래그와 서버 명령을 가른다(없으면 --enable을 claude가 자기 옵션으로 읽는다).
# 이 플래그 형태로 alwaysLoad를 설정하는 수단은 문서에 없다 — 임의 필드가 필요하면 서버 설정 스키마를
# 그대로 받는 claude mcp add-json 형태를 쓴다(--scope user 동일 적용).
# global-search는 별개 프로필이라 필요할 때만 따로 등록한다(설치기가 만들지 않는다):
#   "ctr-global": { "command": "context-router", "args": ["--profile", "global-search", "--projects", "<path-or-id,...>"] }
permissions (.claude/settings.json 예시 — ingest/net/global 도구에 ask를 건다):
{
  "permissions": {
    "ask": ["mcp__ctr-exec__ctr_index", "mcp__ctr-exec__ctr_fetch_and_index", "mcp__ctr-global__*"]
  }
}
# 이 두 도구 규칙은 그 프로필을 켠 등록에서만 대상이 있다 — ctr_index는 --enable ingest,
# ctr_fetch_and_index는 --enable net에서만 등록된다. 위 등록은 설치기 기본 프로필(ingest,net)이라
# 두 도구가 모두 있고 규칙이 그대로 매치한다. exec까지 함께 쓰려면 위 등록의 args를
# ["--enable", "ingest,net,exec"]으로 바꾼다 — 플래그 없는 재설치는 그 args를 보존하고
# "hook install --enable-exec"은 exec만으로 되돌린다. mcp__ctr-global__*도 위의 ctr-global
# 등록을 따로 만든 경우에만 대상이 있다.
# exec 2종(ctr_execute·ctr_execute_file)은 ask에 넣지 않는다 — 승인 강도는 호스트 권한 모드가
# 정한다. default 모드에서는 MCP 기본 프롬프트가 그대로 작동하고, 무프롬프트 모드이거나 그
# 도구를 덮는 allow 규칙(프롬프트의 '다시 묻지 않기'·--allowedTools가 남기는 항목)이 있으면
# 프롬프트 없이 실행된다 — 기존 ask를 지우면 그동안 가려져 있던 allow가 유효해진다(실측: ask
# 2종이 allow 1종을 무력화). ask 규칙을 넣으면 두 경우 모두 프롬프트가 강제된다: 무프롬프트
# 모드에서도 ask는 프롬프트를 띄우고, 평가 순서(deny→ask→allow) 때문에 더 구체적인 allow도
# ask를 이기지 못한다.
# 이중 동의는 유지된다: ① --enable exec 서버 프로필(기동 시) ② 호스트 권한 모델(모드와 규칙에
# 따름 — 무프롬프트 모드이거나 덮는 allow 규칙이 있으면 이 층은 프롬프트를 만들지 않는다).

## Codex (~/.codex/config.toml)
[mcp_servers.ctr]
command = "context-router"
args = ["--enable", "ingest,net"]
enabled_tools = ["ctr_search", "ctr_fetch", "ctr_transform", "ctr_record_event", "ctr_session_summary", "ctr_export_events", "ctr_index", "ctr_fetch_and_index"]

[mcp_servers.ctr.env]
CTR_MANAGED = "context-router"
# 이 두 테이블은 "context-router hook install --codex"가 자동으로 병합한다. 관리 단위는 테이블
# 경계이며 주석 마커가 아니다 — Codex는 다른 서버를 추가하는 것만으로 파일 전체를 재직렬화하며
# 주석을 지우기 때문이다. CTR_MANAGED는 소유 표식일 뿐이고 서버는 이 환경변수를 읽지 않는다.
# 위 값은 무버전이다 — doctor가 "표식 있음·버전 미상"으로 읽고 doctor --fix가 현재 버전으로 채운다.
# 승인 프롬프트가 필요하면 [mcp_servers.ctr]에 default_tools_approval_mode = "prompt"를 직접
# 넣는다. 설치기는 그 키를 쓰지 않고(D64와 같은 사유 — 무프롬프트 모드에서도 프롬프트를 강제해
# 사용자가 끌 수 없게 된다), 넣어 둔 키는 재설치가 원문 그대로 보존한다.
# 그 키는 대화형 세션 전용이다 — 프로그램이 Codex를 비대화형으로 몰면 프롬프트에 답할 수단이
# 없어 그 서버의 도구 호출이 응답 없이 매달린다.
# exec 프로필은 "hook install --codex --enable-exec"으로 켠다 — args가 ["--enable", "exec"]가
# 되고 enabled_tools에 "ctr_execute","ctr_execute_file"이 함께 붙는다(ingest·net까지 함께 쓰려면
# "--enable ingest,net,exec"으로 지정한다 — 두 플래그는 합집합이다). 승인 강도는 Codex 승인 모드가 정한다.
# [mcp_servers.ctr-exec]는 설치기가 읽지도 쓰지도 않는다 — 그 이름의 테이블을 직접 만들었다면 그대로 둔다.

## exec 결과 읽기(호스트 공통)
# shell 러너: exit_code는 스니펫의 종료 상태다(중간 비종결 오류는 반영되지 않는다).
# "마지막 명령의 상태"가 문자 그대로인 것은 sh뿐이다 — PowerShell -File은 exit이나 종결
# 오류가 없으면 0이라, 마지막 줄이 비 0으로 죽은 네이티브 명령이면 exit_code는 0이다. 이
# 경우 stderr에 그 상황을 알리는 안내 줄이 한 줄 남는다(exit_code는 바뀌지 않는다 —
# $ErrorActionPreference = 'Stop'도 이 부류는 멈추지 못한다 — pwsh 7.6.0·powershell 5.1 실측).
# 판정은 exit_code와 stderr를 함께 보고, 엄격 동작은 스니펫에 직접 적는다:
#   PowerShell: 첫 줄 $ErrorActionPreference = 'Stop'(cmdlet 오류) + 네이티브 명령은
#               $LASTEXITCODE 확인, 마지막 줄 exit $LASTEXITCODE로 전파.
#   sh: 첫 줄 set -e — 네이티브 비 0 종료에서도 멈춘다.
`

// runDoctor: 5항목 진단(저장 루트/프로젝트 식별/content.db/FTS5/ledger.db) + 호스트 등록
// 스니펫을 w에 출력한다. store를 생성하지 않는다(store.Open(dir, true)만 사용, 설계 §7).
// 실패 항목이 있으면 error를 반환한다(main이 exit 1) — 반환 오류 메시지에는 절대경로를
// 담지 않는다(§12 canary), 대신 상세는 w의 진단 본문에 있다.
func runDoctor(ctx context.Context, w io.Writer, storeRoot, projectRoot, version string, fix bool) error {
	var failed []string
	fmt.Fprintln(w, "context-router doctor")
	fmt.Fprintln(w)

	// [1] 저장 루트 존재·쓰기 가능. storeRoot 자체는 절대 만들지 않는다(no-create 원칙) —
	// 이미 존재하는 디렉터리면 그 자신을, 존재하지 않으면 실제로 존재하는 가장 가까운 조상을
	// 프로브 대상으로 삼는다(nearestExistingDir, 리뷰 Fix Round 3 item 2). 그 경로에
	// 디렉터리가 아닌 무언가(일반 파일 등)가 이미 있으면 store.Open의 MkdirAll이 절대
	// 성공할 수 없으므로 프로브 없이 명시 거부한다.
	exists := false
	var writable bool
	if fi, err := os.Stat(storeRoot); err == nil {
		exists = true
		if fi.IsDir() {
			writable = probeWritable(storeRoot)
		} // else: 디렉터리가 아닌 기존 경로 — writable은 false로 둔다(명시 거부)
	} else if nearest, isDir := nearestExistingDir(storeRoot); isDir {
		writable = probeWritable(nearest)
	} // else: 비디렉터리 조상이 경로를 막고 있음(F6) — MkdirAll이 절대 성공 못 하므로 writable=false 유지
	fmt.Fprintf(w, "[1] store-root: exists=%v writable=%v\n", exists, writable)
	if !writable {
		failed = append(failed, "store-root")
	}

	// [2] 프로젝트 식별
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		fmt.Fprintf(w, "[2] project: 식별 실패: %v\n", err)
		failed = append(failed, "project")
	} else {
		fmt.Fprintf(w, "[2] project: ProjectID=%s WorktreeRoot=%s\n", canon.ProjectID, canon.WorktreeRoot)
	}

	var reader *sql.DB // [3]에서 열린 read-only reader — 성공하면 [4]에서 재사용
	// [4] fts5는 [3]과 [5] 사이(오름차순, §9 부채 정렬)에 출력한다 — reader는 [3] 성공 시 재사용,
	// 미초기화/식별 실패면 nil(:memory: 프로브). 두 분기 같은 위치에 찍으려고 클로저로 묶는다.
	doFTS5 := func() {
		if err := probeFTS5(ctx, reader); err != nil {
			fmt.Fprintf(w, "[4] fts5: 불가 (%v)\n", err)
			failed = append(failed, "fts5")
		} else {
			fmt.Fprintln(w, "[4] fts5: 가능")
		}
	}
	if canon.ProjectID == "" {
		fmt.Fprintln(w, "[3] content.db: skip (project 식별 실패)")
		doFTS5()
		fmt.Fprintln(w, "[5] ledger.db: skip (project 식별 실패)")
	} else {
		projDir := filepath.Join(storeRoot, "projects", canon.ProjectID)
		dbPath := filepath.Join(projDir, "content.db")
		if fi, err := os.Stat(dbPath); err != nil || fi.IsDir() {
			fmt.Fprintln(w, "[3] content.db: not initialized")
		} else {
			st, err := store.Open(projDir, true) // read-only — 절대 생성하지 않는다
			if err != nil {
				fmt.Fprintf(w, "[3] content.db: open 실패: %v\n", err)
				failed = append(failed, "content.db")
			} else {
				defer func() { _ = st.Close() }() // read-only 진단 프로브 — 닫기 실패해도 영향 없음
				var userVersion int
				var quickCheck string
				uvErr := st.Reader().QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion)
				qcErr := st.Reader().QueryRowContext(ctx, "PRAGMA quick_check").Scan(&quickCheck)
				if uvErr != nil || qcErr != nil || quickCheck != "ok" {
					fmt.Fprintf(w, "[3] content.db: quick_check 실패 (user_version=%d quick_check=%q)\n", userVersion, quickCheck)
					failed = append(failed, "content.db")
				} else {
					// D73: 색인 유무를 버전이 아니라 진단으로 관측한다. 병기는 quick_check **뒤**에
					// 둔다 — 기존 테스트가 "user_version=%d quick_check=ok"를 부분문자열로 단정하므로
					// 뒤에 붙이면 골든을 갱신하지 않고 정보만 더한다. 분자는 대상 색인 3개 중 실재
					// 수다(sources 전체 색인 수를 세면 uri PK의 autoindex가 섞인다).
					fmt.Fprintf(w, "[3] content.db: user_version=%d quick_check=ok indexes=%d/%d\n",
						userVersion, countShadowIndexes(ctx, st.Reader()), len(shadowIndexNames))
					reader = st.Reader()
				}
			}
		}
		doFTS5()

		// [5] ledger.db 존재 여부(정보성 — 실패로 취급하지 않는다, ledger는 best-effort)
		ledgerExists := false
		if fi, err := os.Stat(filepath.Join(projDir, "ledger.db")); err == nil && !fi.IsDir() {
			ledgerExists = true
		}
		fmt.Fprintf(w, "[5] ledger.db: exists=%v\n", ledgerExists)
	}

	// [6]-[8] 세션 진단(설계 §7, 태스크9a): session.db quick_check·lease shared 프로브·
	// session.recover-pending 마커 존재. worktree 디렉터리(projects/<pid>/worktrees/<wid>,
	// canon.WorktreeID — doctor는 cwd/--root 자체가 이미 특정 worktree이므로 export와 달리
	// --worktree 다중 후보 모호성이 없다)가 아직 없으면(session 기능 미사용) 3항목 모두
	// "not initialized"로 정보성 표시하고 실패로 세지 않는다(content.db [3] 분기와 동일
	// 원칙 — no-create: 이 블록도 sessDir을 만들지 않는다).
	sessDirFI, sessDirErr := os.Stat(filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID))
	switch {
	case canon.ProjectID == "":
		fmt.Fprintln(w, "[6] session.db: skip (project 식별 실패)")
		fmt.Fprintln(w, "[7] session.lock: skip (project 식별 실패)")
		fmt.Fprintln(w, "[8] session.recover-pending: skip (project 식별 실패)")
	case sessDirErr != nil || !sessDirFI.IsDir():
		fmt.Fprintln(w, "[6] session.db: not initialized")
		fmt.Fprintln(w, "[7] session.lock: not initialized")
		fmt.Fprintln(w, "[8] session.recover-pending: not initialized")
	default:
		sessDir := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)

		// [6] session.db quick_check — read-only(session.OpenReadOnly는 store.Open(dir,true)의
		// mode=ro&query_only(ON) 선례와 동형 DSN, doctor no-create 원칙상 session.Open은 쓰지
		// 않는다).
		if fi, statErr := os.Stat(filepath.Join(sessDir, "session.db")); statErr != nil || fi.IsDir() {
			fmt.Fprintln(w, "[6] session.db: not initialized")
		} else if reader, openErr := session.OpenReadOnly(sessDir); openErr != nil {
			fmt.Fprintf(w, "[6] session.db: open 실패: %v\n", openErr)
			failed = append(failed, "session.db")
		} else {
			var quickCheck string
			qcErr := reader.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&quickCheck)
			// D42 [6] 병기: 전체 세션 수 + 빈 세션 수(비-session_start 이벤트 0건). empty 술어는
			// GC와 동일하되 나이 게이트는 무관(경과 무관 진단). close 전에 조회한다.
			var sessCount, emptyCount int64
			var countErr error
			if qcErr == nil && quickCheck == "ok" {
				countErr = reader.QueryRowContext(ctx, `SELECT
					(SELECT COUNT(*) FROM sessions),
					(SELECT COUNT(*) FROM sessions s WHERE NOT EXISTS(
						SELECT 1 FROM session_events e
						WHERE e.session_id = s.session_id AND e.event_type != 'session_start'))`).Scan(&sessCount, &emptyCount)
			}
			closeErr := reader.Close()
			switch {
			case qcErr != nil || quickCheck != "ok":
				fmt.Fprintf(w, "[6] session.db: quick_check 실패 (quick_check=%q)\n", quickCheck)
				failed = append(failed, "session.db")
			default:
				// empty 계상 조회 실패는 [6]을 실패로 뒤집지 않는다(정보성 유지 — 설계 §8).
				counts := ""
				if countErr == nil {
					counts = fmt.Sprintf(" sessions=%d (empty=%d)", sessCount, emptyCount)
				}
				if closeErr != nil {
					fmt.Fprintf(w, "[6] session.db: quick_check=ok%s (close 경고: %v)\n", counts, closeErr)
				} else {
					fmt.Fprintf(w, "[6] session.db: quick_check=ok%s\n", counts)
				}
			}
		}

		// [7] lease shared 프로브(설계 §6.2) — 시도-즉시-해제. exclusive 프로브는 하지 않는다
		// (시작 중인 서버의 shared 획득과 경합해 오분기시키는 경로 차단, 설계 §6.2 명문 계약).
		if release, lockErr := store.AcquireLock(filepath.Join(sessDir, session.LockFileName), true); lockErr != nil {
			fmt.Fprintf(w, "[7] session.lock: shared 획득 실패: %v\n", lockErr)
			failed = append(failed, "session.lock")
		} else {
			release()
			fmt.Fprintln(w, "[7] session.lock: shared 획득 가능")
		}

		// [8] 복구 마커 존재(설계 §6.3) — 존재하면 서버가 fail-closed 상태이므로 실패 항목에
		// 센다(session recover 재실행 필요를 doctor가 능동적으로 알림).
		_, markerErr := os.Stat(filepath.Join(sessDir, "session.recover-pending"))
		switch {
		case markerErr == nil:
			fmt.Fprintln(w, "[8] session.recover-pending: 존재 (session recover 필요)")
			failed = append(failed, "session.recover-pending")
		case errors.Is(markerErr, os.ErrNotExist):
			fmt.Fprintln(w, "[8] session.recover-pending: 없음")
		default:
			fmt.Fprintf(w, "[8] session.recover-pending: 확인 실패: %v\n", markerErr)
			failed = append(failed, "session.recover-pending")
		}
	}

	// [9]-[13] 훅/사이드카 진단(설계 §7·§9 확장). 전부 정보성 — 미설치·미해석·drops 존재는
	// 정상 상태라 실패로 세지 않는다(진단 본문에는 절대경로 허용, §12 canary는 반환 오류 전용).
	// [9] 훅 등록 상태 — 프로젝트 + 사용자(~/.claude) 범위 양쪽 검사(F5: `hook install --user`
	// 등록을 프로젝트-only 검사가 놓쳐 "미등록" 오보하던 문제). 두 경로 모두 install/uninstall과
	// 동일한 hookSettingsPath 이음새로 도출한다(사용자 홈 = os.UserHomeDir).
	// 마커 버전(install이 기입한 __ctrManaged 버전)을 바이너리 version과 대조해 불일치 시
	// 재설치를 안내한다(설계 v0.3 §5·D33a — 훅 계약이 바뀐 구버전 마커가 남아 있으면 러닝 훅이
	// 신 계약과 어긋난다).
	hookScope := func(path string, pathErr error) string {
		if pathErr != nil {
			return "확인불가"
		}
		n, marker, err := scanRegisteredHooks(path)
		switch {
		case err != nil:
			return "파싱실패"
		case n == 0:
			return "미등록"
		case marker == "":
			// D82 — 훅 등록물은 무버전 마커를 쓴다. 존재·개수만 본다: 버전 비교는 [20]의 MCP
			// 등록물 검사가 맡는다(여기서 비교하면 상시 불일치 경고가 된다).
			return fmt.Sprintf("등록됨(%d개)", n)
		case marker != version:
			return fmt.Sprintf("등록됨(%d개, marker %s≠%s — hook install 재실행)", n, marker, version)
		default:
			return fmt.Sprintf("등록됨(%d개, marker %s)", n, marker)
		}
	}
	projPath, _ := hookSettingsPath(false, projectRoot) // 프로젝트 경로는 오류를 내지 않는다
	userPath, userPathErr := hookSettingsPath(true, projectRoot)
	fmt.Fprintf(w, "[9] hooks: project=%s user=%s (context-router hook install)\n",
		hookScope(projPath, nil), hookScope(userPath, userPathErr))

	if p, lookErr := exec.LookPath(hookBinaryName); lookErr != nil {
		fmt.Fprintln(w, "[10] context-router: PATH 미해석 (설치 후 훅 실행 가능)")
	} else {
		fmt.Fprintf(w, "[10] context-router: %s\n", p)
	}

	fmt.Fprintf(w, "[11] store-root path: %s\n", storeRoot)

	// [12] drops 건수 — 두 위치 합산(T4 결정): <storeRoot>/드롭(식별 전) + <sessionDir>/드롭(worktree별).
	// 각 위치를 사유별로 롤업해 "N(사유=n,...)"로 렌더한다(설계 v0.3 §5·D33a). total은 줄 수 합계
	// (빈 줄·미파싱 줄 포함 — 기존 줄 수 계약 보존).
	rootTotal, rootReasons, rootLast := dropsByReason(filepath.Join(storeRoot, dropsFileName))
	wtTotal, wtReasons, wtLast := 0, map[string]int(nil), map[string]int64(nil)
	if canon.ProjectID != "" && canon.WorktreeID != "" {
		wtTotal, wtReasons, wtLast = dropsByReason(filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID, dropsFileName))
	}
	fmt.Fprintf(w, "[12] drops: store-root=%s worktree=%s total=%d\n",
		formatDropCount(rootTotal, rootReasons, rootLast), formatDropCount(wtTotal, wtReasons, wtLast), rootTotal+wtTotal)

	// [13] 사이드카(drops.log) 기록 가능 여부 — worktree 세션 dir이 실재하면 그걸, 아니면
	// store-root를 프로브한다(식별 전 drops는 store-root, 이후는 worktree dir, §2.3).
	sidecarDir := storeRoot
	if canon.ProjectID != "" && canon.WorktreeID != "" {
		if wt := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID); dirExistsCLI(wt) {
			sidecarDir = wt
		}
	}
	fmt.Fprintf(w, "[13] sidecar writable: %v\n", dirExistsCLI(sidecarDir) && probeWritable(sidecarDir))

	// [14] content.db 규모 — shadow 성장 관측 채널(설계 v0.3 §2 보존·D33). 정보성 — 실패로
	// 세지 않는다. store.SizeStats가 artifacts/ CAS 물리 blob 바이트를 소유(D13 anti-fragmentation).
	// project 식별 실패·content.db 부재·조회 실패는 전부 "없음"으로 fail-soft(행은 항상 방출).
	// 서버가 동시 실행 중이면 SizeStats의 ro-open이 content.db 점유와 경합해 실패할 수 있고 그때도
	// "없음"으로 나온다 — 손상이 아니라 일시적 경합(서버 정지 후 재실행하면 값이 나옴).
	var sz *store.SizeStat // [15]가 소비 — [14] else에서만 채워지고 그 외엔 nil(귀속 0으로 처리)
	if canon.ProjectID == "" {
		fmt.Fprintln(w, "[14] content.db: 없음")
	} else if s, err := store.SizeStats(filepath.Join(storeRoot, "projects", canon.ProjectID)); err != nil || s == nil {
		fmt.Fprintln(w, "[14] content.db: 없음")
	} else {
		sz = s
		fmt.Fprintf(w, "[14] content.db: sources=%d artifacts=%d blob=%dB file=%dB\n", sz.Sources, sz.Artifacts, sz.BlobBytes, sz.FileBytes)
		// D38 — CAS 전체 blob 총량 경고(shadow 전용 아님 — [14] 측정 실체 그대로). 관측 채널이지
		// 정책 집행이 아니다(D27): 자동 삭제 없음. SizeStats 실패 경로는 이 분기 밖이라 미평가.
		if warn := storeWarnBytes(os.Getenv); sz.BlobBytes > warn {
			fmt.Fprintf(w, "[14] warning: blob %dB > 임계 %dB(CTR_STORE_WARN_BYTES) — 수동 구제는 purge 계열 CLI(purge --project <id> --hook-only로 shadow만 선택 삭제 가능). 자동 삭제 없음\n", sz.BlobBytes, warn)
		}
		// D46 — content.db 파일 축(청크 텍스트+FTS) 자문 경고. D38 기준 축(blob)은 대체하지
		// 않는다 — 파일 축은 purge 후에도 free page로 즉시 안 줄어 별도 안내가 계약(설계 v0.6 §4).
		if warn := contentFileWarnBytes(os.Getenv); sz.FileBytes > warn {
			fmt.Fprintf(w, "[14] warning: file %dB > 임계 %dB(CTR_CONTENT_FILE_WARN_BYTES) — 청크 텍스트+FTS 축(자문). purge 행 삭제 후에도 free page로 즉시 줄지 않음, 회수는 VACUUM(라이브 서버 제약 — 서버 비가동 시), --hook-only는 shadow 귀속 한정(explicit 소스 감축은 전체 purge). 자동 삭제 없음\n", sz.FileBytes, warn)
		}
	}

	// [15] D40 §2 — shadow-owned 접두 분해: projects/<pid>/worktrees/* 하위 각 session.db를
	// read-only로 순회해 artifact_created 이벤트의 session_id 접두(cc:/cx:)로 귀속 hash를
	// 버킷팅한다. 귀속 hash→물리 바이트는 [14]의 sz.ShadowOwned가 소유한다(CAS 경로 재구성
	// 금지 — D13). worktree 단위 격리: 열기·조회·스캔·JSON 파싱 어느 단계든 실패하면 그
	// worktree만 건너뛰고 괄호 끝에 ' incomplete'를 병기한다(전역 failed 목록에 넣지 않는
	// 관측 채널). session.db 부재는 실패가 아니라 세션 미사용이라 [6]과 동일하게 조용히 건너뛴다
	// (usable에도 세지 않는다). 쓸 수 있는 worktree가 하나도 없으면(부재·전부 불용) '세션 분해
	// 없음'으로 렌더한다. hash 추출은 LastIndex("sha256-")(세션ID의 ':' 성분과 무충돌, §3.1).
	owned := map[string]int64(nil)
	var ownedBytes int64
	var ownedHashes int
	if sz != nil {
		owned, ownedBytes, ownedHashes = sz.ShadowOwned, sz.ShadowOwnedBytes, sz.ShadowOwnedHashes
	}
	hashPrefixes := map[string]map[string]bool{} // 귀속 hash → 관측된 호스트 접두 집합({cc:,cx:}만)
	usable, incomplete := 0, false
	if canon.ProjectID != "" {
		wtRoot := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees")
		entries, _ := os.ReadDir(wtRoot) // 부재/오류 → 빈 목록 → '세션 분해 없음'으로 귀결
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			wdir := filepath.Join(wtRoot, e.Name())
			if fi, statErr := os.Stat(filepath.Join(wdir, "session.db")); statErr != nil || fi.IsDir() {
				continue // session.db 부재 — 세션 미사용 worktree, 실패 아님([6] 원칙)
			}
			if err := func() error {
				db, openErr := session.OpenReadOnly(wdir)
				if openErr != nil {
					return openErr
				}
				defer func() { _ = db.Close() }()
				rows, qErr := db.QueryContext(ctx,
					`SELECT session_id, artifact_refs FROM session_events
					 WHERE event_type='artifact_created' AND artifact_refs IS NOT NULL`)
				if qErr != nil {
					return qErr
				}
				defer func() { _ = rows.Close() }()
				for rows.Next() {
					var sid, refsJSON string
					if scanErr := rows.Scan(&sid, &refsJSON); scanErr != nil {
						return scanErr
					}
					var refs []string
					if jErr := json.Unmarshal([]byte(refsJSON), &refs); jErr != nil {
						return jErr
					}
					var prefix string
					switch {
					case strings.HasPrefix(sid, "cc:"):
						prefix = "cc:"
					case strings.HasPrefix(sid, "cx:"):
						prefix = "cx:"
					default:
						continue // 미지 호스트 접두 — 귀속에 기여하지 않음(unattributed로 귀결)
					}
					for _, ref := range refs {
						i := strings.LastIndex(ref, "sha256-")
						if i < 0 {
							continue
						}
						h := ref[i+len("sha256-"):]
						if _, ok := owned[h]; !ok {
							continue // 귀속 hash만 버킷팅 대상
						}
						if hashPrefixes[h] == nil {
							hashPrefixes[h] = map[string]bool{}
						}
						hashPrefixes[h][prefix] = true
					}
				}
				return rows.Err()
			}(); err != nil {
				incomplete = true // 이 worktree만 skip — 전역 failed 금지
				continue
			}
			usable++
		}
	}
	if usable == 0 {
		fmt.Fprintf(w, "[15] shadow-owned: %dB hashes=%d (세션 분해 없음)\n", ownedBytes, ownedHashes)
	} else {
		var ccB, cxB, sharedB, unattrB int64
		for h, b := range owned { // 귀속 hash 전수 — 각 hash는 정확히 한 버킷으로(합=ownedBytes 불변)
			switch set := hashPrefixes[h]; {
			case len(set) >= 2:
				sharedB += b
			case set["cc:"]:
				ccB += b
			case set["cx:"]:
				cxB += b
			default:
				unattrB += b
			}
		}
		suffix := ""
		if incomplete {
			suffix = " incomplete"
		}
		fmt.Fprintf(w, "[15] shadow-owned: %dB hashes=%d (cc:=%dB cx:=%dB shared=%dB unattributed=%dB%s)\n",
			ownedBytes, ownedHashes, ccB, cxB, sharedB, unattrB, suffix)
	}

	// [16] codex 등록 상태(D52, 스펙 v0.9 §0) — 읽기 전용 5분기. 경고는 [14]와 동일하게
	// failed에 미계상(doctor exit 계약 무변경). trust 해시 검증은 비범위(스펙 §1.2).
	codexHooksScope := func(path string, pathErr error) (string, bool) { // (표시, 등록 존재)
		if pathErr != nil {
			return "확인불가", false
		}
		n, marker, scanErr := scanCodexRegisteredHooks(path)
		switch {
		case scanErr != nil:
			return "파싱실패", false
		case n == 0:
			return "미등록", false
		case marker == "":
			return fmt.Sprintf("등록됨(%d개)", n), true
		case marker != version:
			return fmt.Sprintf("등록됨(%d개, marker %s≠%s — hook install --codex 재실행)", n, marker, version), true
		default:
			return fmt.Sprintf("등록됨(%d개, marker %s)", n, marker), true
		}
	}
	projCodexHooks, _ := codexHooksPath(false, projectRoot)
	userCodexHooks, userCodexHooksErr := codexHooksPath(true, projectRoot)
	projScope, projMarker := codexHooksScope(projCodexHooks, nil)
	userScope, userMarker := codexHooksScope(userCodexHooks, userCodexHooksErr)
	markerPresent := projMarker || userMarker
	cfgPath, cfgPathErr := codexConfigPath()
	switch {
	case cfgPathErr != nil:
		fmt.Fprintln(w, "[16] codex: config.toml 경로 확인불가")
	default:
		cfgData, readErr := os.ReadFile(cfgPath)
		switch {
		case os.IsNotExist(readErr): // 분기① — 마커 여부 무관(상태 루트 부재=미사용, 소멸 시그니처 아님)
			fmt.Fprintf(w, "[16] codex: config.toml 없음 — 미사용/미설치 (hooks: project=%s user=%s)\n", projScope, userScope)
		case readErr != nil:
			fmt.Fprintln(w, "[16] codex: config.toml 읽기 실패")
		default:
			present, anomaly := probeCodexMCPBlock(cfgData)
			switch {
			case anomaly != anomalyNone: // 분기⑤
				fmt.Fprintf(w, "[16] codex: [mcp_servers.ctr] 테이블=이상 (hooks: project=%s user=%s)\n", projScope, userScope)
				fmt.Fprintf(w, "[16] warning: %s — 수동 확인 필요(hook install --codex 안내 참조)\n", anomaly.reason())
			case present: // 분기④
				fmt.Fprintf(w, "[16] codex: [mcp_servers.ctr] 테이블=존재 (hooks: project=%s user=%s)\n", projScope, userScope)
			case markerPresent: // 분기② — 소멸 시그니처
				fmt.Fprintf(w, "[16] codex: [mcp_servers.ctr] 테이블=부재 (hooks: project=%s user=%s)\n", projScope, userScope)
				fmt.Fprintln(w, "[16] warning: 훅은 설치됐으나 MCP 테이블 부재 — deny 안내가 가리키는 ctr_search/ctr_fetch를 Codex가 볼 수 없음. hook install --codex 재기입 권장")
			default: // 분기③
				fmt.Fprintf(w, "[16] codex: [mcp_servers.ctr] 테이블=부재·훅 미설치 — hook install --codex (hooks: project=%s user=%s)\n", projScope, userScope)
			}
		}
	}

	bi, _ := debug.ReadBuildInfo() // 실패 시 nil — formatBuildLine이 생략 처리
	fmt.Fprintln(w, formatBuildLine(version, bi))

	// [18] exec 러너 감지(설계 v0.11 D58 — 실행 없음, LookPath/버전 게이트만). 정보성 라인 —
	// exec는 기본 OFF·프로필 opt-in이라 미검출이어도 failed에 세지 않는다.
	var parts []string
	for _, s := range ctrexec.RunnerStatus() {
		if s.OK {
			label := s.Runner
			if s.Version != "" {
				label += "/" + s.Version
			}
			parts = append(parts, s.Lang+"="+label)
		} else {
			parts = append(parts, s.Lang+"=미검출")
		}
	}
	fmt.Fprintf(w, "[18] exec runners: %s\n", strings.Join(parts, " "))

	// [19] 승인 규칙 정합 — ask와 도구가 겹치는 allow는 "설정을 바꿨는데 계속 물어본다"로만
	// 드러나 사용자가 알아채기 어렵다(설계 v0.12 D64). 판정이 도구 집합 교집합이라 일부만 겹치는
	// allow도 보고에 든다 — 그쪽은 겹치는 도구에서만 무력하고 나머지 도구에서는 유효하므로 문면은
	// "덮는다"가 아니라 "겹친다"다(전부 죽었다고 읽히면 사용자가 규칙을 통째로 지운다).
	// 결과는 세 갈래다: 겹침 발견·충돌 없음·판정 불가.
	// 판정이 실패했을 때 "충돌 없음"을 찍으면 확인하지 않은 것을 확인했다고
	// 말하는 셈이라 침묵보다 나쁘다 — 세 번째 라인을 따로 둔다(스코프 경로 해석·읽기·파싱 중
	// 어느 것이 실패해도 이 갈래다). 오류·경로는 문면에 담지 않는다(§12).
	if shadowed, err := askShadowedAllows(projectRoot, os.ReadFile); err != nil {
		fmt.Fprintln(w, "[19] permissions: ask/allow 판정 불가 — 설정 스코프를 확인하지 못했다(경로 해석·읽기·파싱 중 하나가 실패, 충돌 없음이 아니다)")
	} else if len(shadowed) > 0 {
		fmt.Fprintf(w, "[19] permissions: ask와 겹치는 allow 항목 %d건 — %s (평가 순서 deny→ask→allow — 겹치는 도구에서는 allow가 효력이 없고 겹치지 않는 도구에서는 그대로 유효하다. ask를 지우면 호스트 권한 모드가 승인 강도를 정한다)\n",
			len(shadowed), strings.Join(shadowed, ", "))
	} else {
		fmt.Fprintln(w, "[19] permissions: ask/allow 충돌 없음")
	}

	// [20] MCP 등록물의 버전 표식(D83) — .mcp.json의 __ctrManaged와 config.toml의
	// env.CTR_MANAGED. D82가 버전을 이 두 곳으로 옮겼으므로 이것이 --fix의 유일한 감지원이다.
	// [9]는 훅 그룹만 읽고 [16]의 버전 비교는 hooks.json 값이며 config.toml은 존재·부재·이상만
	// 읽는다. 경고는 [16]과 같이 failed에 계상하지 않는다(종료코드 계약 무변경).
	// 항목 번호가 [20]인 이유: 현재 마지막이 [19]이고 기존 번호는 밀지 않는다([16]·[17] 문면에
	// 묶인 테스트가 있다).
	markerState := func(data []byte, readErr error, read func([]byte) (string, string, bool)) (string, bool) {
		switch {
		case errors.Is(readErr, os.ErrNotExist):
			return "없음", false
		case readErr != nil:
			return "읽기실패", false
		}
		marker, command, found := read(data)
		if !ownedRegistration(marker, command, found) {
			return "미등록", false // 우리 소유가 아니면 고칠 대상이 아니다
		}
		if !isOurMarkerValue(marker) {
			// 표식은 없지만 command가 우리 것 — D80의 인수 대상이라 --fix가 표식을 채운다.
			// 이것을 "미등록"으로 접으면 [20]은 고칠 것이 없다고 말하는데 --fix는 파일을 바꾼다.
			return "표식없음", true
		}
		v := markerVersion(marker)
		if !markerDrift(marker, version) {
			return "marker " + v, false
		}
		if v == "" { // 무버전 표식 — "표식 있음·버전 미상"(hostSnippet 붙여넣기 경로)
			return "marker 버전미상≠" + version, true
		}
		return "marker " + v + "≠" + version, true
	}
	mcpData, mcpReadErr := os.ReadFile(mcpConfigPath(projectRoot))
	mcpLabel, mcpDrift := markerState(mcpData, mcpReadErr, mcpManagedMarker)
	codexLabel, codexDrift := "확인불가", false
	codexReason := ""
	if cfgP, cfgErr := codexConfigPath(); cfgErr == nil {
		cfgData, cfgReadErr := os.ReadFile(cfgP)
		codexLabel, codexDrift, codexReason = codexMarkerLabel(cfgData, cfgReadErr, version)
	}
	fmt.Fprintf(w, "[20] mcp markers: .mcp.json=%s codex=%s\n", mcpLabel, codexLabel)
	if codexReason != "" {
		// 이상 라벨은 세 사유를 한 값으로 묶으므로 사유를 함께 낸다 — 사유마다 필요한 조치가
		// 다르고, 그것을 알리지 않으면 사용자는 install이 영구히 무변경인 이유를 알 수 없다.
		fmt.Fprintf(w, "[20] codex: %s\n", codexReason)
	}
	if mcpDrift || codexDrift {
		// 문면을 사유 중립으로 둔다 — 표식은 현재인데 형식·command가 어긋나 --fix가 파일을
		// 바꾸는 상태(구형식 포함)에서 "버전 표식이 다르다"는 사유를 잘못 말한다. 사유는
		// 라벨이 담는다. .mcp.json 갈래는 은퇴 이름 항목도 함께 정리하므로 그것을 예고한다.
		// **command 되쓰기도 예고한다**: 이 경고는 --fix가 파일을 바꾸는 모든 상태에서 나오고
		// 그중에는 command만 사용자가 고쳐 둔 등록물이 있다(D86이 표식과 command를 맞춘다).
		// 보존되는 것만 열거하면 절대경로·래퍼로 고쳐 둔 값이 예고 없이 사라지고, PATH에 우리
		// 이름이 없는 호스트에서는 그 등록이 더 이상 기동하지 않는다.
		fmt.Fprintf(w, "[20] warning: 다시 기입이 필요한 MCP 등록물이 있습니다 — doctor --fix로 고치세요(기존 파일만 고치고 등록을 만들지 않습니다. args·enabled_tools는 보존하지만 command는 %q로 다시 씁니다 — 직접 고쳐 둔 실행 경로가 있으면 그 값이 사라집니다. config.toml은 백업을 남깁니다. .mcp.json은 대체된 옛 이름의 항목을 함께 정리합니다)\n", hookBinaryName)
	}
	if fix {
		doctorFixRegistrations(w, projectRoot, version)
	}

	fmt.Fprintln(w)
	fmt.Fprint(w, hostSnippet)

	if len(failed) > 0 {
		return fmt.Errorf("doctor: 진단 실패 항목 %d개", len(failed))
	}
	return nil
}

// codexMarkerLabel — [20]의 Codex 라벨·권고 여부·사유(D85). **권고는 codexVerdict.shouldFix()가
// 정한다** — 라벨은 진단 정보라 자유도가 있지만 권고는 --fix의 행동과 등가여야 한다.
// 라벨 셋이 추가된다: 구간 밖 충돌은 충돌, 구간 판정 불가는 이상, 소유하지만
// ownedRegistration이 거짓인 인수 대상은 구형식이다. 마지막 술어가 여전히 필요한 이유는 그
// 거짓이 **인수 경로임을 가리키는 유일한 신호**라는 것이다 — 판정 권한을 나눠 갖는 것이
// 아니라 라벨을 구별할 뿐이다.
// 셋째 반환값은 사유이며 이상 갈래에서만 비어 있지 않다 — 그 라벨이 세 사유를 한 값으로
// 묶으므로 호출자가 별도 줄로 낸다.
// .mcp.json 갈래는 이 함수를 쓰지 않고 runDoctor의 markerState 클로저에 남는다(D85).
func codexMarkerLabel(data []byte, readErr error, version string) (label string, fix bool, reason string) {
	switch {
	case errors.Is(readErr, os.ErrNotExist):
		return "없음", false, ""
	case readErr != nil:
		return "읽기실패", false, ""
	}
	v := codexRegistrationVerdict(data, version)
	shouldFix := v.shouldFix()
	switch v.State {
	case mcpMarkerAnomaly:
		return "이상", false, v.Anomaly.reason()
	case mcpConflict:
		return "충돌", false, ""
	case mcpExistingHeader:
		return "미등록", false, ""
	}
	if !v.TableFound {
		// **권고값을 여기서 고정하지 않는다.** shouldFix가 이미 TableFound를 보므로 거짓이
		// 나오고, false를 하드코딩하면 판정원이 둘이 되어 shouldFix의 결함이 이 갈래에서
		// 가려진다 — 그 절을 되돌리는 유도 FAIL이 물지 않게 된다.
		return "미등록", shouldFix, "" // append하면 되지만 --fix는 등록을 만들지 않는다
	}
	if !ownedRegistration(v.Marker, v.Command, true) {
		return "구형식", shouldFix, "" // D84 셋째 절이 소유로 받는 인수 대상
	}
	if !isOurMarkerValue(v.Marker) {
		return "표식없음", shouldFix, ""
	}
	// 버전 불일치 접미는 **버전 비교**로 붙인다. shouldFix로 붙이면 표식이 현재 버전인데 형식
	// 드리프트 때문에 기입이 필요한 파일(구 블록 안의 현재 버전 표식)에서 같은 버전을 좌우에 둔
	// 표기가 나온다. 형식 드리프트는 별도 접미로 구별한다.
	// early return으로 적는다 — if-else 체인은 revive의 indent-error-flow에 걸린다.
	mv := markerVersion(v.Marker)
	if mv == "" {
		return "marker 버전미상≠" + version, shouldFix, ""
	}
	if mv != version {
		return "marker " + mv + "≠" + version, shouldFix, ""
	}
	if shouldFix {
		return "marker " + mv + "(형식)", true, ""
	}
	return "marker " + mv, false, ""
}

// codexVerdict — config.toml 등록물의 판정(D85). 감지(doctor [20])와 고침(doctor --fix)이 이
// 한 값을 공유한다.
type codexVerdict struct {
	State      codexMCPState
	Anomaly    codexAnomaly
	TableFound bool
	Changed    bool
	Marker     string
	Command    string
	// Out — 기입할 산출 바이트. 기입 갈래가 이 값을 쓰므로 **요청 조립 지점이 한 곳으로
	// 묶인다** — 기입 쪽에서 요청을 다시 조립하면 그 두 조립이 갈릴 수 있고, 그것이 이
	// 릴리스가 닫는 어긋남과 같은 형태다. 진단 경로는 이 필드를 쓰지 않고 무시한다.
	Out []byte
}

// codexRegistrationVerdict — 감지와 고침이 공유하는 판정(D85). **요청 조립이 이 함수 안에만
// 있다** — "같은 인자로 부른다"를 호출자의 규율로 두면 다음 변경에서 한쪽이 빠지고, 그러면
// 요청 필드 축으로 감지와 고침이 다시 갈린다(D86의 MarkerOnly가 그 축이다).
// 이 함수는 그 자체로 판정하지 않는다 — **권고에 쓰이는 State·TableFound·Changed·Out은
// installCodexConfigBlock 하나에서 오고**, 라벨 전용 Marker·Command는 같은 바이트를 다시 읽는
// 순수 판독기(codexConfigMarker)에서 온다. 라벨 전용 Anomaly는 결과가 실은 사유를 **우선**하고
// 그것이 없을 때만 probeCodexMCPBlock을 읽는다(D89) — install만 아는 이탈은 probe가 알지 못하고
// 구간 밖 충돌은 반대로 probe에만 사유가 있어, 한쪽만 읽으면 사유 하나가 판정값에서 사라진다.
// 셋 모두 같은
// 바이트에서 codexManagedSpans를 다시 도출하므로 값은 일치한다. installCodexConfigBlock이
// 순수 변환(파일 IO 없음)이라는 것이 읽기 전용 경로에서 부를 수 있는 근거이며, 스펙 §1.3
// 게이트 1이 그것을 확인한다.
func codexRegistrationVerdict(data []byte, version string) codexVerdict {
	res := installCodexConfigBlock(data, codexInstallRequest{
		Marker: hookMarker(version), MarkerOnly: true,
	})
	anomaly := res.Anomaly
	if anomaly == anomalyNone {
		_, anomaly = probeCodexMCPBlock(data)
	}
	marker, command, _ := codexConfigMarker(data)
	return codexVerdict{
		State: res.State, Anomaly: anomaly, TableFound: res.TableFound,
		Changed: res.Changed, Marker: marker, Command: command, Out: res.Out,
	}
}

// shouldFix — doctor --fix가 실제로 기입하는 조건 전체(D85). 셋의 논리곱이며 Changed 하나로
// 줄이면 "파일은 있고 관리 테이블만 없는" 상태에서 install이 append 경로로 Changed=true를
// 내는데 --fix는 등록을 만들지 않으므로 오권고가 된다 — 지금 "미등록·무경고"로 옳게 보고되는
// 가장 흔한 미설치 상태가 그것이다.
func (v codexVerdict) shouldFix() bool {
	return v.State == mcpWritten && v.TableFound && v.Changed
}

// doctorFixRegistrations — doctor --fix(D83). MCP 등록물의 표식을 현재 버전으로 다시 기입하고,
// D80 형식으로의 마이그레이션이 남은 config.toml을 함께 변환한다. **소유가 확인된 등록물만
// 고친다** — 부재하는 파일도, 부재하는 등록물도 만들지 않고 안내만 낸다(doctor no-create
// 원칙의 범위는 파일이 아니라 등록물이다). **이 조건은 .mcp.json 갈래 한정이다** — 그쪽은
// ownedRegistration이 [20]이 "미등록"으로 보고하는 상태와 같은 술어라 감지와 고침의 대상이
// 어긋나지 않는다. Codex 갈래의 조건은 codexVerdict.shouldFix()가 쓰는 State·TableFound·
// Changed 세 필드다(D85). 기입은 새 경로를 만들지 않고 install이 쓰는 경로를 그대로 쓴다:
// Codex는 installCodexConfigBlock, .mcp.json은 mergeMCPServers. D84의 백업과 무변경 판정도
// 그 경로에 있는 것을 그대로 쓴다.
// 자동이 아니라 명시적 행위인 이유는 §0에 있다 — 기동 시 자동 재기입은 제품이 사용자 설정
// 파일을 예고 없이 고치는 동작이 되고, 마커 교체 직후가 그 경로의 검증이 가장 얕은 시점이다.
func doctorFixRegistrations(w io.Writer, projectRoot, version string) {
	missing := 0
	mcpPath := mcpConfigPath(projectRoot)
	switch data, err := os.ReadFile(mcpPath); {
	case errors.Is(err, os.ErrNotExist):
		missing++
	case err != nil:
		fmt.Fprintln(w, "[20] fix: .mcp.json을 읽지 못해 건너뜁니다")
	case !ownedRegistration(mcpManagedMarker(data)):
		fmt.Fprintln(w, "[20] fix: .mcp.json에 우리 소유로 확인된 등록물이 없습니다 — 만들지 않습니다. hook install로 먼저 등록하세요")
	default:
		entry := mcpServerEntry{Command: hookBinaryName, AlwaysLoad: true, Managed: hookMarker(version)}
		// setProfile=false — 표식만 고치고 프로필은 기존 항목의 것을 그대로 둔다.
		if merged, _, mErr := mergeMCPServers(data, ctrMCPServerName, entry, true, false); mErr != nil {
			fmt.Fprintf(w, "[20] fix: %q 이름에 우리가 소유하지 않은 항목이 있어 .mcp.json을 그대로 두었습니다\n", ctrMCPServerName)
		} else if bytes.Equal(data, merged) {
			fmt.Fprintln(w, "[20] fix: .mcp.json은 이미 현재 버전입니다 — 무변경")
		} else if wErr := atomicWriteFile(mcpPath, merged); wErr != nil {
			fmt.Fprintln(w, "[20] fix: .mcp.json 기록 실패")
		} else {
			fmt.Fprintf(w, "[20] fix: .mcp.json 표식을 %s로 다시 기입했습니다(프로필은 보존, 대체된 옛 이름의 항목이 있으면 함께 정리했습니다)\n", version)
		}
	}
	cfgPath, cfgErr := codexConfigPath()
	switch data, err := readIfPathOK(cfgPath, cfgErr); {
	case cfgErr != nil:
		fmt.Fprintln(w, "[20] fix: config.toml 경로 확인불가")
	case errors.Is(err, os.ErrNotExist):
		missing++
	case err != nil:
		fmt.Fprintln(w, "[20] fix: config.toml을 읽지 못해 건너뜁니다")
	default:
		v := codexRegistrationVerdict(data, version)
		switch {
		case v.State != mcpWritten:
			// 구간 판정 불가·구간 밖 충돌·사용자 소유 테이블이 여기로 온다. 사유는 [16]과 [20]이
			// 라벨로 말한다.
			fmt.Fprintln(w, "[20] fix: config.toml이 기입 가능한 상태가 아닙니다 — [16] 안내를 먼저 따르세요")
		case !v.TableFound:
			// 관리 테이블 자체가 없다 — mcpWritten은 "append하면 된다"는 뜻이지만 --fix는
			// 등록을 만들지 않는다. 이 상태를 [20]도 "미등록"으로 보고하며 권고하지 않는다.
			fmt.Fprintln(w, "[20] fix: config.toml에 우리 관리 테이블이 없습니다 — 만들지 않습니다. hook install --codex로 먼저 등록하세요")
		case !v.Changed:
			fmt.Fprintln(w, "[20] fix: config.toml은 이미 현재 형식·버전입니다 — 무변경")
		default:
			if bErr := backupCodexConfig(cfgPath, data); bErr != nil {
				fmt.Fprintln(w, "[20] fix: config.toml 백업 실패 — 기입하지 않았습니다")
			} else if wErr := atomicWriteFile(cfgPath, v.Out); wErr != nil {
				fmt.Fprintln(w, "[20] fix: config.toml 기록 실패")
			} else {
				fmt.Fprintln(w, "[20] fix: config.toml 관리 테이블의 표식과 command를 다시 기입했습니다(args·enabled_tools는 원문 보존, 백업 config.toml.bak)")
			}
		}
	}
	if missing > 0 {
		fmt.Fprintf(w, "[20] fix: 대상 파일이 없어 %d건을 건너뛰었습니다 — 만들지 않습니다. hook install로 먼저 등록하세요\n", missing)
	}
}

// readIfPathOK — 경로 해석이 실패했으면 읽지 않는다(doctorFixRegistrations의 switch 초기화용).
func readIfPathOK(path string, pathErr error) ([]byte, error) {
	if pathErr != nil {
		return nil, pathErr
	}
	return os.ReadFile(path)
}

// formatBuildLine — doctor [17] build 라인 순수 포매터(D56 — 테스트 주입점, 검수 반영). bi가
// nil이면 버전만(ReadBuildInfo 실패 경로 — 요소 생략·정보 라인·경고 아님). commit·dirty는
// marker·stale 비교에 절대 비관여(안정 SemVer 계약 — 스펙 §0).
func formatBuildLine(version string, bi *debug.BuildInfo) string {
	parts := []string{}
	if bi != nil {
		parts = append(parts, "go="+bi.GoVersion)
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				r := s.Value
				if len(r) > 12 {
					r = r[:12]
				}
				parts = append(parts, "commit="+r)
			case "vcs.time":
				parts = append(parts, "time="+s.Value)
			case "vcs.modified":
				if s.Value == "true" {
					parts = append(parts, "dirty")
				}
			}
		}
	}
	return fmt.Sprintf("[17] build: %s (%s)", version, strings.Join(parts, " "))
}

// dirExistsCLI — path가 실재하는 디렉터리인지(doctor 사이드카 프로브 보조 — probeWritable은
// 존재하는 dir을 요구한다).
func dirExistsCLI(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
