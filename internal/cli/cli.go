// Package cli — doctor·upgrade·stats(purge는 Task5까지 임시 placeholder) 진입점. 설계서 §7.
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
	"path/filepath"
	"regexp"
	"time"

	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/store"
)

// releaseURL: 컴파일타임 상수 — upgrade가 응답에서 취하는 것은 tag_name 버전 문자열뿐이다.
// 응답이 제공하는 URL·명령·기타 필드는 절대 출력하지 않는다(위생, 설계 §7).
const releaseURL = "https://api.github.com/repos/wotjr1649/context-router/releases/latest"

// Run: cli 서브커맨드 단일 진입점. storeRoot·projectRoot는 main이 이미 결정해 넘긴다(cli는
// 재도출하지 않는다 — 설계서 §7 Produces). sub은 main이 4개 이름 중 하나임을 이미 확인했다.
// args는 doctor·upgrade에서 미사용, stats가 --provider 고유 플래그 파싱에 쓴다(전용
// flag.NewFlagSet, 설계 §7 — main의 serverFlags와 별개). stderr는 아직 어떤 서브커맨드도
// 쓰지 않는다(의도적 미사용, 시그니처는 4개 서브커맨드 공통).
func Run(ctx context.Context, sub string, args []string, storeRoot, projectRoot, version string, stdout, stderr io.Writer) error {
	switch sub {
	case "doctor":
		if len(args) > 0 {
			// 사용자 입력(원시 args)을 오류 문구에 에코하지 않는다 — 개수만(규약 §6, 리뷰
			// Fix Round 3, item 5).
			return fmt.Errorf("cli: doctor: 예상치 않은 인자 %d개", len(args))
		}
		return runDoctor(ctx, stdout, storeRoot, projectRoot)
	case "upgrade":
		if len(args) > 0 {
			return fmt.Errorf("cli: upgrade: 예상치 않은 인자 %d개", len(args))
		}
		client := &http.Client{Timeout: 10 * time.Second}
		return runUpgrade(stdout, client, releaseURL, version)
	case "stats":
		return runStats(ctx, stdout, args, storeRoot, projectRoot)
	case "purge":
		return fmt.Errorf("cli: 미구현 서브커맨드: %s", sub)
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
	defer resp.Body.Close()
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

// runStatsProvider: path의 Claude Code transcript JSONL을 한 줄씩 스캔해 message.usage의 4개
// 토큰 필드를 합산하고 usage 보유 레코드 수를 센다(설계 §6). 파싱 불가 줄·message.usage 없는
// 줄·maxProviderLine을 넘는 줄은 로그 없이 skipped 카운트만 올린다(마지막 경우는 명령을
// 중단시키지 않고 계속 진행). 실측 합계만 출력한다 — 절약 주장·비교 문구 없음. ctx가 취소되면
// (cancelCheckLines줄마다 확인) 그 시점까지 읽은 결과를 버리고 오류로 중단한다 — 아주 큰
// transcript를 스캔하는 동안 상위 호출자가 취소할 길을 남겨둔다.
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
	defer f.Close()

	var input, output, cacheRead, cacheCreate, records, skipped int64
	br := bufio.NewReaderSize(f, 64*1024)
	for lineNo := 0; ; lineNo++ {
		if lineNo%cancelCheckLines == 0 {
			if cerr := ctx.Err(); cerr != nil {
				return fmt.Errorf("stats provider: 취소됨: %w", cerr)
			}
		}
		raw, truncated, ferr := readTranscriptLine(br, maxProviderLine)
		switch {
		case truncated:
			skipped++
		case len(raw) > 0:
			var line providerUsageLine
			if jsonErr := json.Unmarshal(bytes.TrimRight(raw, "\r\n"), &line); jsonErr != nil || line.Message.Usage == nil {
				skipped++
			} else {
				u := line.Message.Usage
				input += u.InputTokens
				output += u.OutputTokens
				cacheRead += u.CacheReadInputTokens
				cacheCreate += u.CacheCreationInputTokens
				records++
			}
		}
		if ferr != nil {
			if errors.Is(ferr, io.EOF) {
				break
			}
			return errors.New("stats provider: 스캔 실패")
		}
	}

	fmt.Fprintf(w, "input_tokens: %d\n", input)
	fmt.Fprintf(w, "output_tokens: %d\n", output)
	fmt.Fprintf(w, "cache_read_input_tokens: %d\n", cacheRead)
	fmt.Fprintf(w, "cache_creation_input_tokens: %d\n", cacheCreate)
	fmt.Fprintf(w, "usage records: %d\n", records)
	fmt.Fprintf(w, "skipped: %d\n", skipped)
	return nil
}

// probeWritable: dir에 임시 파일을 만들고 즉시 지워 쓰기 가능 여부만 확인한다 — dir 자체를
// 생성하지 않는다(doctor no-create 원칙).
func probeWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".ctr-doctor-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

// nearestExistingDir: path의 조상 디렉터리 중 실제로 존재하는 가장 가까운 것을 찾는다.
// store.Open은 MkdirAll(path/artifacts, ...)로 중간 디렉터리를 몇 단계든 한 번에 만들 수
// 있으므로(설계 §3.1), 미생성 storeRoot의 쓰기 가능 여부는 딱 한 단계 위(filepath.Dir)가
// 아니라 실제로 존재하는 조상에서 판정해야 한다 — 예전 구현은 한 단계 위까지 없는 신규
// 배치(예: storeRoot의 부모·조부모가 전부 미생성)에서 그 부모조차 못 만들고 늘 "쓰기
// 불가"로 오판했다(리뷰 Fix Round 3, item 2).
func nearestExistingDir(path string) string {
	dir := path
	for {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir { // 루트 도달 — 더 못 올라감
			return dir
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
		defer db.Close()
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

// hostSnippet: doctor 마지막에 출력하는 호스트 등록 안내(설계 §9) — Claude Code(.mcp.json +
// permissions ask 규칙)와 Codex(config.toml 기본 3-도구 프로필 + approval prompt 권장).
const hostSnippet = `--- host adapter snippets (설계 §9) ---

## Claude Code (.mcp.json)
{
  "mcpServers": {
    "ctr": { "command": "context-router", "args": [] },
    "ctr-global": { "command": "context-router", "args": ["--profile", "global-search", "--projects", "<path-or-id,...>"] }
  }
}
permissions (.claude/settings.json 예시 — ingest/net/global은 기본 ask):
{
  "permissions": {
    "ask": ["mcp__ctr__ctr_index", "mcp__ctr__ctr_fetch_and_index", "mcp__ctr-global__*"]
  }
}

## Codex (~/.codex/config.toml)
[mcp_servers.ctr]
command = "context-router"
args = []
enabled_tools = ["ctr_search", "ctr_fetch", "ctr_transform"]
# ingest/net 활성화 시 권장: default_tools_approval_mode = "prompt"
`

// runDoctor: 5항목 진단(저장 루트/프로젝트 식별/content.db/FTS5/ledger.db) + 호스트 등록
// 스니펫을 w에 출력한다. store를 생성하지 않는다(store.Open(dir, true)만 사용, 설계 §7).
// 실패 항목이 있으면 error를 반환한다(main이 exit 1) — 반환 오류 메시지에는 절대경로를
// 담지 않는다(§12 canary), 대신 상세는 w의 진단 본문에 있다.
func runDoctor(ctx context.Context, w io.Writer, storeRoot, projectRoot string) error {
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
	} else {
		writable = probeWritable(nearestExistingDir(storeRoot))
	}
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
	if canon.ProjectID == "" {
		fmt.Fprintln(w, "[3] content.db: skip (project 식별 실패)")
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
				defer st.Close()
				var userVersion int
				var quickCheck string
				uvErr := st.Reader().QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion)
				qcErr := st.Reader().QueryRowContext(ctx, "PRAGMA quick_check").Scan(&quickCheck)
				if uvErr != nil || qcErr != nil || quickCheck != "ok" {
					fmt.Fprintf(w, "[3] content.db: quick_check 실패 (user_version=%d quick_check=%q)\n", userVersion, quickCheck)
					failed = append(failed, "content.db")
				} else {
					fmt.Fprintf(w, "[3] content.db: user_version=%d quick_check=ok\n", userVersion)
					reader = st.Reader()
				}
			}
		}

		// [5] ledger.db 존재 여부(정보성 — 실패로 취급하지 않는다, ledger는 best-effort)
		ledgerExists := false
		if fi, err := os.Stat(filepath.Join(projDir, "ledger.db")); err == nil && !fi.IsDir() {
			ledgerExists = true
		}
		fmt.Fprintf(w, "[5] ledger.db: exists=%v\n", ledgerExists)
	}

	// [4] FTS5 가용성
	if err := probeFTS5(ctx, reader); err != nil {
		fmt.Fprintf(w, "[4] fts5: 불가 (%v)\n", err)
		failed = append(failed, "fts5")
	} else {
		fmt.Fprintln(w, "[4] fts5: 가능")
	}

	fmt.Fprintln(w)
	fmt.Fprint(w, hostSnippet)

	if len(failed) > 0 {
		return fmt.Errorf("doctor: 진단 실패 항목 %d개", len(failed))
	}
	return nil
}
