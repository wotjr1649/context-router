// Package cli — doctor·upgrade·stats(purge는 Task5까지 임시 placeholder) 진입점. 설계서 §7.
package cli

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
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
		return runDoctor(ctx, stdout, storeRoot, projectRoot)
	case "upgrade":
		client := &http.Client{Timeout: 10 * time.Second}
		return runUpgrade(stdout, client, releaseURL, version)
	case "stats":
		return runStats(stdout, args, storeRoot, projectRoot)
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
func runStats(w io.Writer, args []string, storeRoot, projectRoot string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	provider := fs.String("provider", "", "Claude Code transcript JSONL 경로 — 실측 토큰 합계만 출력")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("stats: 플래그 파싱 실패: %w", err)
	}
	if *provider != "" {
		return runStatsProvider(w, *provider)
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
		return fmt.Errorf("stats: 프로젝트 식별 실패: %w", err)
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

// runStatsProvider: path의 Claude Code transcript JSONL을 한 줄씩 스캔해 message.usage의 4개
// 토큰 필드를 합산하고 usage 보유 레코드 수를 센다(설계 §6). 파싱 불가 줄·message.usage 없는
// 줄은 로그 없이 skipped 카운트만 올린다. 실측 합계만 출력한다 — 절약 주장·비교 문구 없음.
// 파일 크기와 무관하게 큰 버퍼(10MB)의 bufio.Scanner를 쓴다(transcript 한 줄이 클 수 있음).
func runStatsProvider(w io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("stats provider: 파일 열기 실패: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10<<20)

	var input, output, cacheRead, cacheCreate, records, skipped int64
	for scanner.Scan() {
		var line providerUsageLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil || line.Message.Usage == nil {
			skipped++
			continue
		}
		u := line.Message.Usage
		input += u.InputTokens
		output += u.OutputTokens
		cacheRead += u.CacheReadInputTokens
		cacheCreate += u.CacheCreationInputTokens
		records++
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("stats provider: 스캔 실패: %w", err)
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

	// [1] 저장 루트 존재·쓰기 가능
	checkDir := storeRoot
	exists := false
	if fi, err := os.Stat(storeRoot); err == nil && fi.IsDir() {
		exists = true
	} else {
		checkDir = filepath.Dir(storeRoot) // 미생성이면 상위만 확인 — storeRoot 자체는 만들지 않는다
	}
	writable := probeWritable(checkDir)
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
