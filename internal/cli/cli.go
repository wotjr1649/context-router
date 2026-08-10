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
	"slices"
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

// warnBytesEnv — 임계 키 셋의 공통 파싱: 양수만 채택하고 파싱 실패·비양수·미설정은 기본값
// (D38이 세운 규율을 세 키가 그대로 쓴다 — 잘못 쓴 값이 경고를 조용히 끄면 안 된다).
func warnBytesEnv(getenv func(string) string, key string, def int64) int64 {
	if v, err := strconv.ParseInt(getenv(key), 10, 64); err == nil && v > 0 {
		return v
	}
	return def
}

// storeWarnBytes — CTR_STORE_WARN_BYTES(D38 — 측정 실체가 CAS 전체 blob이라 STORE 명명).
func storeWarnBytes(getenv func(string) string) int64 {
	return warnBytesEnv(getenv, "CTR_STORE_WARN_BYTES", defaultStoreWarnBytes)
}

// defaultContentFileWarnBytes — content.db **file 축**(본체+`-wal`+`-shm` 총점유) 경고 임계
// 기본값. D102 계약 6이 판정을 live로 옮기면서 이 축의 경고가 사라졌던 것을 되살리며 값을
// 다시 골랐다(릴리스 패스 B2): 정리 후 정상상태는 본체 170,790,912 B에 그날의 병합이 다시 쓴
// `-wal`(정리 후 인덱스 116 MB급)을 더해도 300 MB대라 조용하고, 실측된 부푼 상태는 본체만
// 709,890,048 B라 발화한다. 이 축이 묻는 것은 "디스크를 얼마나 먹나"이고 답은 VACUUM이다.
const defaultContentFileWarnBytes = 512 << 20 // 512MiB=536,870,912B — 정상상태 300 MB대 ↔ 부푼 실측 709,890,048 B 사이

// defaultContentLiveWarnBytes — content.db **live 축**((page_count-freelist)×page_size) 경고
// 임계 기본값. D102 계약 6이 고른 값 그대로다 — 정리 후 정상상태 실측 170,790,912 B의 약
// 1.5배이고, 창을 7일로 늘리는 결정(약 400 MB)이 이 신호를 켠다. 이 축이 묻는 것은
// "쓰레기가 쌓였나"이고 답은 퍼지+병합이다.
const defaultContentLiveWarnBytes = 256 << 20 // 256MiB=268,435,456B — D102 계약 6

// contentFileWarnBytes — CTR_CONTENT_FILE_WARN_BYTES. blob 키와 분리한 전용 키(D46) — 축마다
// 크기·성장·구제 경로가 달라 한쪽 조정이 다른 축을 무력화하면 안 된다. 이름이 "file"이고
// 이제 실제로 file 축(총점유)을 잰다 — 계약 6이 잠시 live에 붙였던 이름/의미 불일치도 닫혔다.
func contentFileWarnBytes(getenv func(string) string) int64 {
	return warnBytesEnv(getenv, "CTR_CONTENT_FILE_WARN_BYTES", defaultContentFileWarnBytes)
}

// contentLiveWarnBytes — CTR_CONTENT_LIVE_WARN_BYTES(신규 키, 릴리스 패스 B1). file 키와
// 나눈 이유는 두 축이 서로 다른 질문에 답하기 때문이다: 둘 다 뜰 수 있고 그것이 의도다.
func contentLiveWarnBytes(getenv func(string) string) int64 {
	return warnBytesEnv(getenv, "CTR_CONTENT_LIVE_WARN_BYTES", defaultContentLiveWarnBytes)
}

// Run: cli 서브커맨드 단일 진입점. storeRoot·projectRoot는 main이 이미 결정해 넘긴다(cli는
// 재도출하지 않는다 — 설계서 §7 Produces). sub은 main이 9개 이름(doctor·upgrade·stats·purge·
// session·hook·usage·codex-hook·version) 중 하나임을 이미 확인했다. args는 doctor·upgrade에서 미사용, stats가 --provider
// 고유 플래그 파싱에 쓴다(전용 flag.NewFlagSet, 설계 §7 — main의 serverFlags와 별개). stderr는
// session export의 worktree 후보 목록·진단 안내 전용(태스크9, §7 stdout purity 게이트 선례 —
// 그 외 서브커맨드는 여전히 미사용).
//
// storeRootExplicit/storeRootRaw 두 매개변수는 v0.19에서 사라졌다 — 그 값을 읽던 유일한
// 소비처가 `hook install`이 훅 명령에 --store-root를 주입하는 자리였고, 등록이 플러그인
// 매니페스트로 옮겨가면서 함께 사라졌다(D96).
func Run(ctx context.Context, sub string, args []string, storeRoot, projectRoot, version string, stdout, stderr io.Writer) error {
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
		// D96·D97 — doctor는 읽기 전용이다. --fix(D83 이음새 ①)는 기입 경로였으므로 플래그째
		// 지웠다(무동작으로 남기지 않는다 — 무동작 플래그는 거짓말이다). 남은 플래그가 없으므로
		// version 서브커맨드와 같은 형태로 인자만 거부한다. --fix만 자기 사유를 낸다(리뷰 M1) —
		// 그 자리를 대신하는 것이 무엇인지 없으면 마이그레이션하는 사용자가 아무것도 배우지 못한다.
		if len(args) > 0 {
			if isRetiredFixFlag(args[0]) {
				return errors.New("cli: doctor: --fix는 더는 없습니다 — 등록물 재기입은 각 호스트 CLI(codex mcp remove·claude mcp remove 등)의 몫입니다. doctor는 옛 등록물 잔존을 읽기 전용으로 알립니다")
			}
			return fmt.Errorf("cli: doctor: 예상치 않은 인자 %d개", len(args))
		}
		return runDoctor(ctx, stdout, storeRoot, projectRoot, version)
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
		// install 안내 + uninstall + 러닝 훅(무인자/--no-shadow) 디스패치는 runHook이
		// 소유한다(설계 §7, hook_install.go). 러닝 훅은 항상 exit 0(fail-open §2.3).
		return runHook(ctx, args, storeRoot, projectRoot, version, stdout)
	case "codex-hook":
		// Codex 러닝 훅(설계 v0.4 §2 D35) — 항상 exit 0(fail-open §2.3). 전용 서브커맨드 =
		// 구버전 바이너리 오귀속 차단 게이트(§11.2 F3).
		return runCodexHook(ctx, args, storeRoot, version, stdout)
	default:
		return fmt.Errorf("cli: 미지 서브커맨드: %s", sub)
	}
}

// isRetiredFixFlag — 옛 doctor의 flag.NewFlagSet이 --fix로 받아들이던 철자인가(릴리스 리뷰 F6).
// 그 flagset은 대시 한 개와 두 개를 같게 다루고 불 플래그에 `=값`을 허용했으므로 `--fix`·`-fix`·
// `--fix=true`·`-fix=false`가 전부 그 자리를 쳤다. 그중 하나를 친 사용자에게 일반 "예상치 않은
// 인자" 오류를 내면 그 능력이 어디로 갔는지 배우지 못하고, 그것이 이 특수 분기의 존재 이유다.
// `=false`까지 포함하는 이유는 문면이 안내이지 값 해석이 아니기 때문이다 — 플래그 자체가 없다.
// 판정은 이름 정확 일치다(접두 매치 금지 — `--fixture`는 `--fix`가 아니다).
func isRetiredFixFlag(arg string) bool {
	s, ok := strings.CutPrefix(arg, "--")
	if !ok {
		if s, ok = strings.CutPrefix(arg, "-"); !ok {
			return false
		}
	}
	name, _, _ := strings.Cut(s, "=")
	return name == "fix"
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
//
// **집계가 실패해도 찍을 수 있는 것은 다 찍는다**(릴리스 패스 소견 F4) — 근거는 아래
// LedgerStats 호출 자리의 주석에 있다. 실패는 그대로 오류로 반환하므로 종료 코드는 여전히
// 0이 아니다.
func runStatsLocal(w io.Writer, storeRoot, projectRoot string) error {
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		// 원인(err)을 감싸지 않는다 — Canonicalize의 오류는 filepath.Abs/EvalSymlinks발
		// *fs.PathError라 절대경로를 담고 있다(§12 canary). runDoctor([2] 분기)와 동일하게
		// 반환 오류에는 경로 없는 정적 메시지만 남긴다(리뷰 Fix Round 2, Critical).
		return errors.New("stats: 프로젝트 식별 실패")
	}
	projDir := filepath.Join(storeRoot, "projects", canon.ProjectID)
	// ★ 릴리스 패스 소견 F4: **집계 실패로 명령 전체를 중단하지 않는다.** 종전에는 여기서 바로
	// 반환해 헤더도 total도 회수 줄도 **한 줄도** 안 찍혔다. 도달 경로가 드물지 않다: `ledger.db`는
	// 있는데 `ledger` 테이블이 없는 상태 — SQLite는 연결을 여는 순간 파일을 만들고 CREATE는 그
	// 뒤에 도므로, 디스크가 차거나 busy_timeout을 넘긴 잠금이 그 CREATE만 떨어뜨리면 도달한다 —
	// 에서 LedgerStats는 `no such table: ledger`로 죽지만 **LedgerFetchStats는 같은 상태를 오류
	// 없이 LedgerOK=false로 낸다**(계약 7의 관용이 그 계단까지 덮는다). 즉 회수 줄은 낼 수 있는데
	// 그 앞에서 죽고 있었고, 14일 측정 구간의 운영자는 D104 행 0(판정 불가)으로 가는 표식 대신
	// 깨진 명령만 봤다.
	//
	// **저장소 쪽을 관용적으로 만드는 안은 기각됐다**(소유자 판정): 진짜 이상을 조용한 빈 결과로
	// 바꾸는 것은 이 릴리스가 세운 원칙의 정확한 역이다. 그래서 고치는 자리가 호출부이고, 형태는
	// "찍을 수 있는 것은 다 찍고 실패는 그대로 반환한다"다 — 침묵도 거짓 0도 만들지 않는다.
	//
	// **아래 LedgerFetchStats는 반대로 조기 반환을 유지한다.** F4가 지목한 해악은 "명령이 아무
	// 것도 안 찍는다"이고 그것은 **먼저 도는** 읽기가 죽을 때만 생긴다 — 뒤의 읽기가 죽으면 표는
	// 이미 찍혀 있고, 남는 것은 마지막 한 줄뿐이다. 두 자리의 처리가 다른 것은 실수가 아니라 이
	// 비대칭 때문이다.
	stats, statsErr := store.LedgerStats(projDir)
	if statsErr != nil {
		statsErr = fmt.Errorf("stats: ledger 집계 실패: %w", statsErr)
	}

	fmt.Fprintln(w, "tool\tcalls\tbytes_stored\tbytes_returned\tspan")
	var totalCalls, totalStored, totalReturned int64
	for _, s := range stats {
		span := time.Unix(s.FirstTS, 0).UTC().Format(time.RFC3339) + "~" + time.Unix(s.LastTS, 0).UTC().Format(time.RFC3339)
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%s\n", s.Tool, s.Calls, s.BytesStored, s.BytesReturned, span)
		// D103 계약 4 후반부(릴리스 리뷰 W1): 훅 포착 활동량 행(`hook:shadow`)은 이름이 `ctr_`로
		// 시작하지 않고, **총계에서 빠지는 것까지가 그 계약이다** — 하루 약 295행이면 그 총계를
		// 훅이 지배한다. 이 릴리스 전 원장의 도구는 전부 `ctr_*`였으므로 이 제외는 총계의 뜻을
		// 바꾸는 게 아니라 유지한다. **총계를 읽는 것은 이제 M6뿐이다**: D104의 채택 문턱은
		// 총계가 아니라 아래 회수 줄의 `resolved_artifacts + missed`를 읽는다(W2 소유자 판정 +
		// 소견 F7이 그 앞 항을 행 수에서 아티팩트 수로 옮겼다).
		// 행 자체는 위에서 이미 찍었다 — 관측 채널은 잃지 않는다.
		if !strings.HasPrefix(s.Tool, "ctr_") {
			continue
		}
		totalCalls += s.Calls
		totalStored += s.BytesStored
		totalReturned += s.BytesReturned
	}
	// 총계의 세 칸도 표식을 탄다(소견 F4). 집계가 실패하면 위 루프가 **0바퀴** 돈 것이라 여기서
	// 0을 찍는 순간 아무것도 못 읽은 원장이 "호출 0회"로 읽힌다 — 이 릴리스 패스의 지배적 결함
	// 부류(못 잰 수를 0으로 찍는다) 그 자체이고, 회수 줄에 표식을 세운 것과 같은 이유다.
	// 표식이 선 total 줄은 진단이기도 하다: **원장 파일이 아예 없는 정상 상태는 여기가 숫자 0**
	// 이다(LedgerStats가 미존재를 오류 없는 빈 결과로 낸다). 두 상태는 회수 줄에서 똑같이
	// `없음`이라, stdout만 보고 "아직 아무것도 안 썼다"와 "읽지 못했다"를 가르는 것이 이 줄이다.
	total := func(v int64) string {
		if statsErr != nil {
			return "없음"
		}
		return strconv.FormatInt(v, 10)
	}
	fmt.Fprintf(w, "total\t%s\t%s\t%s\tbytes suppressed (local, 진단용)\n",
		total(totalCalls), total(totalStored), total(totalReturned))

	// D103 계약 9: 회수 실적 한 줄. D104의 착수 조건을 여기서 읽는다 —
	// **`resolved_artifacts + missed` 10건**(채택 문턱, 소견 F7이 앞 항을 행 수에서 아티팩트
	// 수로 옮겼다) · **`shadow_artifacts` 30건 또는 `missed` 5건**.
	// 조건이 필드 이름 그대로인 것이 계약이다: 문턱이 읽는 수와 사람이 보는 수가 갈리지 않는다.
	// 총 호출을 같은 줄에 병기하는 이유: 이 릴리스부터 위 표의 ctr_fetch calls가 뜻을 바꾸고
	// (전에는 성공만, 이제 성공 + artifact 부재) 그 수가 레거시까지 품기 때문이다 —
	// 병기하지 않으면 해소·미해소가 둘 다 0인 것과 원장 쓰기가 깨진 것이 구분되지 않는다.
	// 오류는 위 LedgerStats 호출과 같은 방식으로 반환한다 — 이관 전 원장은 이미 오류 없는 0
	// 결과이므로 여기서 삼킬 것이 없고, 삼키면 권한·손상 실패가 "데이터 없음"으로 읽힌다.
	fs, err := store.LedgerFetchStats(projDir)
	if err != nil {
		// 조기 반환을 유지하는 근거는 위 LedgerStats 자리의 주석 마지막 문단이다. statsErr를
		// 함께 나르는 이유: 둘 다 실패했는데 하나만 보고하면 남은 하나가 조용히 사라진다.
		return errors.Join(statsErr, fmt.Errorf("stats: 회수 실적 집계 실패: %w", err))
	}
	// ★ **못 잰 수는 0으로 찍지 않는다**(릴리스 패스 M3이 doctor [14] free/live에서 세운 관례).
	// ledger는 스키마 버전을 두지 않는 best-effort 보조 DB라 세 ALTER가 아직 안 돈 원장을 stats가
	// 먼저 만나는 상태가 정상적으로 도달 가능하고(D103 계약 7), 그 계단에서 0은 측정값이
	// 아니다. 여기서는 doctor보다 무겁다 — **D104의 착수 조건이 shadow_artifacts를 읽는다.**
	// 표식 없이 그 0이 숫자로 찍히면 **결정표가 아무것도 재지 않은 원장을 관측으로 받는다**:
	// 칸이 전부 숫자라 행 0이 발화하지 못하고 `resolved_artifacts + missed = 0`이라 행 2로 떨어진다.
	// 창 판단이 닫히지는 않지만 **처방이 틀린다** — 행 2는 "채택의 문제다"라고 말하는데 할 일은
	// 채택을 늘리는 것이 아니라 **바이너리를 가는 것**이고, 그것을 모르면 다음 14일도 아무것도
	// 재지 않는다. 표식이 그 오독을 행 0(판정 불가)에서 끊는다.
	// 스키마 계단이 셋이라 표식의 **자리**가 곧 계단이다(①은 calls부터, ②는 legacy부터, ③은
	// shadow_rows부터 미이관). **예외가 하나 있고 그것이 의도다**: legacy_after_migrate는
	// MigrateMarkOK를 타는데 그 축은 계단과 독립이라, 왼쪽에서 오른쪽으로 이어지는 접두 모양이
	// 그 칸 하나에서만 끊길 수 있다(열은 다 있는데 워터마크가 없는 원장). 아래 그 칸의 설명 참조.
	// 말이 둘인 이유: 왜 못 쟀는지가 다르고, 읽는 사람이 할 일도 다르다. `미이관`은 "새
	// 바이너리로 스토어를 한 번 열면 채워진다", `없음`은 "잴 것이 아직 없다 — 원장 자체가 없거나
	// 읽지 못했다"다. 뒤의 둘(파일 부재 ↔ 소견 F4의 테이블 부재)은 위 total 줄이 갈라 준다.
	// **셋째 말을 만들지 않은 것이 판단이다**: D104는 숫자가 아닌 칸이 하나라도 있으면 행 0
	// (판정 불가)으로 가고 그 처방은 어느 쪽이든 같다 — "이 원장으로는 판정하지 않는다".
	// 말을 늘리면 결정표가 읽지 않는 구분이 줄에만 늘어난다.
	mark := "미이관"
	if !fs.LedgerOK {
		mark = "없음"
	}
	num := func(v int64, measured bool) string {
		if !measured {
			return mark
		}
		return strconv.FormatInt(v, 10)
	}
	// 분위수는 한 겹 더 갈린다: 열이 없으면 위와 같고, 열은 있는데 귀속 해소가 0건이면 잴
	// 표본 자체가 "없음"이다 — 셋 다 "0초에 회수했다"와는 다른 말이다.
	age := func(v int64) string {
		switch {
		case !fs.ShadowOK:
			return mark
		case fs.ShadowResolved == 0:
			return "없음"
		}
		return strconv.FormatInt(v, 10)
	}
	// 병기하는 수마다 이유가 다르다. **행을 세는 칸 바로 옆에 그 부분집합을 놓는 것**이 이 줄의
	// 배치 규칙이다 — 포함 관계가 눈에 보여야 두 수를 뒤바꿔 읽지 않는다.
	//  - legacy: 이관 전 행은 해소에도 미해소에도 안 드는데 calls에는 든다 — 병기하지 않으면
	//    calls가 결과의 분모로 읽힌다(소견 F9). 라이브 원장은 `calls=49` `[실측]`이고, 이관되면
	//    그 49가 전부 legacy로 간다 `[추정]` — 세 상태 질의는 아직 한 번도 돌지 않았다(설치본이
	//    ALTER보다 앞서고 이 경로는 원장을 read-only로만 연다). 실측된 줄은 `legacy=미이관`이었다.
	//  - legacy_after_migrate: 그 legacy 중 **이관 워터마크 뒤에 쓰인** 행(소견 F2). **이 줄에서
	//    가장 값비싼 칸이다** — 원장은 이관됐는데 `ctr_fetch`를 쓰는 것은 여전히 옛 서버인 상태를
	//    이관 전 역사(정상)와 가른다. 그 상태에서는 나머지 칸이 다 숫자라 결정표가 행 2("채택의
	//    문제")로 떨어지는데, 할 일은 채택을 늘리는 것이 아니라 **서버를 다시 띄우는 것**이다.
	//    **이 수는 누적이고 지워지지 않는다** — 서버를 다시 띄워도 더 늘지 않을 뿐 0으로 돌아
	//    가지 않는다. 그것이 옳다: 그 행들이 쌓인 구간의 회수는 실제로 안 재졌고, D104의 14일은
	//    그만큼 오염됐다. 불리언이 아니라 **수**인 이유도 그것이다(크기를 옆의 수들과 대 볼 수
	//    있다). 그래서 이 칸이 0이 아닌 것을 보고 서버를 재시작한 사람은, 내일 같은 수를 다시
	//    보더라도 그것을 "고쳐지지 않았다"로 읽으면 안 된다 — 늘어나는지를 봐야 한다.
	//    표식 축이 다른 것도 그래서다: MigrateMarkOK는 스키마 계단(LedgerOK ⊇ OutcomeOK ⊇
	//    ShadowOK)과 **독립**이라 열이 다 있어도 false일 수 있다. 그 경우의 `미이관`에는 두 번째
	//    원인이 있다 — 열은 붙었는데 워터마크가 없는 원장(이 브랜치의 앞선 빌드)은 바이너리를
	//    갈아도 표식이 채워지지 않고 그 축에서 영구히 판정 불가다(markLedgerMigrated 참조).
	//  - resolved_artifacts: 그 해소의 distinct artifact 수이고 **D104 채택 문턱이 읽는 수**다
	//    (소견 F7). ctr_fetch는 기본 16 KiB까지만 돌려주므로 아티팩트 하나를 끝까지 읽는 것이
	//    여러 호출이다 — 행으로 세면 아티팩트 하나를 한 번 읽은 14일이 문턱 10을 통과한다.
	//    `resolved`와 나란히 놓아야 그 페이징 배수가 보인다. 표식은 **OutcomeOK를 그대로 탄다**:
	//    같은 SELECT의 한 칸이라 새 축이 아니다.
	//  - shadow_rows: 나이 분위수가 실제로 선 모집단(shadow 귀속 해소 행)이다. explicit
	//    아티팩트는 창이 손대지 않으므로 그 나이는 창의 길이에 답하지 않는다(소견 F4).
	//    **어떤 판정도 이 수를 읽지 않는다**(분위수의 N도 착수 조건도 shadow_artifacts다) —
	//    그래도 남기는 이유는 이것 하나다: 옆의 아티팩트 수와 나란히 놓여야 사람이 페이징 집중을
	//    본다. 판정이 안 읽는다고 "정리"하면 그 관측 채널이 사라진다.
	//  - shadow_artifacts: 그 모집단의 distinct artifact 수이고 **D104 착수 조건이 읽는 수**다
	//    (소견 F5).
	fmt.Fprintf(w, "fetch\tcalls=%s\tlegacy=%s\tlegacy_after_migrate=%s\tresolved=%s\tresolved_artifacts=%s\tmissed=%s\tshadow_rows=%s\tshadow_artifacts=%s\tage_s p50=%s p90=%s max=%s\n",
		num(fs.Calls, fs.LedgerOK),
		num(fs.Legacy, fs.OutcomeOK), num(fs.LegacyAfterMigrate, fs.MigrateMarkOK),
		num(fs.Resolved, fs.OutcomeOK), num(fs.ResolvedArtifacts, fs.OutcomeOK),
		num(fs.Missed, fs.OutcomeOK),
		num(fs.ShadowResolved, fs.ShadowOK), num(fs.ShadowArtifacts, fs.ShadowOK),
		age(fs.AgeP50), age(fs.AgeP90), age(fs.AgeMax))
	return statsErr
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
		fmt.Fprintln(w, "# 세션 이벤트가 아직 없습니다(훅 미등록 또는 첫 세션 전) — 플러그인 설치 뒤 세션을 한 번 돌리고 다시 실행하세요")
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
	var mergeFailed int
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
			beforeB = contentFootprint(projDir) // 전 실측 = 명령 착수 전 기준점 — 보고 Δ는 명령 전체(삭제+병합+VACUUM+checkpoint)의 총점유 순감소다
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
			// D102 계약 4 — runPurgeHookOnly와 같은 이유·같은 순서(VACUUM 앞, 실패해도 진행,
			// 종료 상태에는 반영). 프로젝트별 보고 후 계속하고 루프 끝에서 집계한다(D50 관례).
			if merr := st.MergeFTS(ctx); merr != nil {
				fprintMergeErr(stderr, fmt.Sprintf("ctr: %s: %s", id, mergeFailMsg(merr)), merr)
				mergeFailed++
				// M5(릴리스 패스): 디스크 계열 병합 실패도 VACUUM 실패와 **같은 가드**를 탄다 —
				// 꽉 찬 디스크에서 --all 스윕이 잔여 프로젝트로 계속 밀어붙이는 것을 막는다.
				// 이 프로젝트의 VACUUM은 계약 4대로 그대로 진행한다(부분 회수 > 무회수);
				// 가드가 걸리는 것은 잔여 프로젝트다.
				if store.IsDiskErr(merr) {
					vacuumDiskAbort = true
				}
			}
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
	if mergeFailed > 0 {
		return fmt.Errorf("purge: %d개 프로젝트 FTS 병합 실패 — 회수가 부분에 그쳤습니다", mergeFailed)
	}
	return nil
}

// mergeFailMsg — 병합 실패 안내 문면. BUSY/LOCKED가 원인에 **섞여 있으면** 경합 안내를 덧붙인다
// (릴리스 패스 M4) — vacuumReclaim이 VACUUM 경합에 쓰는 것과 같은 규율이다. errors.Join 위의
// IsBusyErr는 OR라 "전부 경합"을 뜻하지 않으므로 문면도 그 이상을 주장하지 않는다: 축별 원인은
// 바로 아래 fprintMergeErr가 줄마다 그대로 낸다.
func mergeFailMsg(err error) string {
	if store.IsBusyErr(err) {
		return "FTS 병합 실패(회수량이 줄어든다) — 경합 포함(라이브 프로세스 가동 중 추정 — 종료 후 재시도)"
	}
	return "FTS 병합 실패(회수량이 줄어든다)"
}

// fprintMergeErr — 원인을 **줄마다 접두를 붙여** 낸다(릴리스 패스 B5). MergeFTS는 두 축
// (porter·trigram)의 실패를 errors.Join으로 합치고 그 Error()는 원소 사이에 개행을 넣으므로,
// %v로 한 번에 찍으면 둘째 줄부터 접두 없는 맨 줄이 되어 "한 줄 = 한 진단"인 stderr 문면이
// 깨진다(줄 단위로 읽는 스크립트가 그 줄의 출처를 잃는다).
func fprintMergeErr(w io.Writer, prefix string, err error) {
	for _, line := range strings.Split(err.Error(), "\n") {
		fmt.Fprintf(w, "%s: %s\n", prefix, line)
	}
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
	beforeB := contentFootprint(projDir) // D55: open 후·PurgeHookOnly 전 — 삭제+병합+VACUUM 효과 격리(스펙 §0)
	rep, purgeErr := st.PurgeHookOnly(ctx)
	var mergeErr, vacErr error
	if purgeErr == nil {
		// ④ 실회수 보고 먼저(스펙 §3 순서) — VACUUM 성패와 무관하게 부분 성공을 즉시 노출한다.
		fmt.Fprintf(w, "hook-only purge: 실회수 %dB(%d hashes), 유예 %d건, 실패 %d건\n",
			rep.ReclaimedB, rep.Hashes, rep.DeferredFiles, rep.FailedFiles)
		// D102 계약 4: VACUUM은 free page만 되돌린다. 삭제가 남긴 FTS tombstone은 **free page가
		// 아니라 live page**라 병합해야 회수되고, 병합 없이 VACUUM만 하면 회수 가능분의 약 30%만
		// 돌아온다. 사용자가 회수를 명시로 요청한 자리이므로 주기 게이트를 걸지 않고, 스탬프도
		// 갱신하지 않는다(스탬프는 자동 경로 것이다). 실패해도 VACUUM은 진행한다 — 부분 회수가
		// 무회수보다 낫고 이미 커밋된 삭제는 유효하다 — 다만 **종료 상태에는 반영한다**:
		// 스크립트가 부른 실행이 free page 몫만 회수하고 성공으로 보이면 안 된다.
		if mergeErr = st.MergeFTS(ctx); mergeErr != nil {
			fprintMergeErr(stderr, "ctr: "+mergeFailMsg(mergeErr), mergeErr)
		}
		// ⑤ D55: vacuumReclaim 합류 — checkpoint busy 검증·총합 보고, 실패는 rc≠0(본경로 동일).
		vacErr = vacuumReclaim(ctx, st, projDir, beforeB, w)
	}
	closeErr := st.Close()
	if purgeErr != nil {
		return purgeErr
	}
	if vacErr != nil {
		return vacErr
	}
	if mergeErr != nil {
		// 원인 문면은 위에서 stderr로 이미 냈다 — 반환 오류에는 경로 없는 정적 메시지만 남긴다(§12 canary).
		// closeErr를 **버리지 않는다**(릴리스 패스 B4): 옛 코드는 mergeErr가 있으면 닫기 실패를
		// 반환에도 stderr에도 내지 않아, 라이터를 못 닫은(=체크포인트 실패 포함) 실행이 병합 실패
		// 하나로만 보였다. 반환값에 합치지 않는 이유는 Close 자신이 errors.Join이라 그 문면에
		// 개행이 들어가기 때문이다(B5와 같은 결함) — 원인은 stderr에 줄마다 접두를 붙여 낸다.
		if closeErr != nil {
			fprintMergeErr(stderr, "ctr: store 닫기 실패", closeErr)
		}
		return errors.New("purge: FTS 병합 실패 — 회수가 부분에 그쳤습니다")
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

// hostInstallSteps — 설치 절차 본문. **`hook install` 안내와 hostSnippet이 공유하는 단일
// 원본이다**(Task 5) — 같은 절차를 두 자리에 따로 적으면 한쪽만 고쳐지고, 그 어긋남이 이
// 릴리스가 닫는 형태다. 0번 걸음이 옛 등록물 제거인 것은 A⑧ 실측이 근거다.
const hostInstallSteps = `## 설치 절차 (D96·D97 — 등록은 호스트가 한다)

0. 옛 등록물을 먼저 지운다: claude mcp remove <이름> · codex mcp remove <이름>. 옛 등록물은
   플러그인과 같은 바이너리를 가리키는 중복이라 어느 호스트든 먼저 지운다.
   Claude Code: [실측] command·args가 이미 등록된 서버와 같은 플러그인 서버를 경고 없이
   버린다 — 이 걸음을 건너뛴 사용자는 "설치했는데 아무 일도 안 일어난다"를 겪는다.
   Codex: [미실측] 같은 메커니즘인지는 재지 않았다 — 지우는 이유는 위와 같다.
   [실측] codex mcp remove는 없는 이름에도 exit 0을 반환한다 — 부재 확인은 mcp list로 한다.
1. Claude Code:
   claude plugin marketplace add wotjr1649/context-router
   claude plugin install context-router --scope <user|project|local>
2. Codex:
   codex plugin marketplace add wotjr1649/context-router
   codex plugin add context-router
3. 확인 — 호스트마다 mcp list의 표시 형태가 다르다 [실측]:
   Claude Code: plugin:context-router:ctr … Connected 항목이 나열되는가.
   Codex: ctr … enabled 항목이 나열되는가.
   [실측] claude plugin details는 이 확인에 쓸 수 없다 — 경로 문자열로 선언한 서버를
   해석하지 않아 런타임이 붙어 있는데도 MCP servers (0)을 낸다.
4. Codex에서 훅 신뢰를 준다 — Codex의 훅은 지속되는 신뢰를 요구한다
   [문서: codex --help의 --dangerously-bypass-hook-trust]. 신뢰를 주지 않은 홈에서 돌린
   프로브는 SessionStart를 포함해 훅이 하나도 발화하지 않았다 [실측]. 이 걸음을 건너뛰면
   수동 포착(대형 도구 출력을 컨텍스트 밖으로 옮기는 그 동작)이 조용히 빠진다.
   Claude Code는 플러그인 설치만으로 훅 여섯이 실린다 [실측].
`

// hookMigrationHint — [9]·[16]이 옛 훅 그룹을 발견했을 때 내는 다음 걸음. 마커 형태와 무관하게
// 한 문면이다(최종 리뷰): 무버전(D82 이후 v0.15+)이든 버전 있는(그 이전) 등록물이든 사용자가
// 처한 상황은 같다 — 옛 그룹은 지워질 때까지 발화하고, 플러그인 매니페스트가 같은 이벤트에 훅을
// 실으므로(D96) 두 벌이 함께 있으면 같은 포착이 두 번 일어난다. 목적지(플러그인 설치)를 함께
// 적는 이유는 제거만 안내하면 훅을 통째로 잃는 걸음으로 읽히기 때문이다. uninstallCmd로 호스트를
// 가른다(Claude `hook uninstall` · Codex `hook uninstall --codex`).
func hookMigrationHint(uninstallCmd string) string {
	return uninstallCmd + "로 옛 그룹을 지우고 플러그인 설치로 옮기세요 — 두 벌이 함께 있으면 같은 포착이 두 번 일어납니다"
}

// heldGroupHint — [9]·[16]이 **동거 그룹**(우리 항목과 사용자 항목이 한 그룹에 함께 있는 형태)을
// 발견했을 때 내는 다음 걸음. 소유 판정이 전건이라 `hook uninstall`은 이 그룹을 보존하고
// `[문서]`(설계 v0.19 §6 — 그 판정을 되돌리면 사용자 훅 항목이 파괴된다), 그래서 목적지가
// hookMigrationHint와 다르다: 정리는 호스트의 `/hooks`에서 사용자가 우리 항목만 지우는 것이다.
// 잔존 사실을 함께 적는 이유는 이 그룹도 계속 발화해 플러그인 훅과 같은 포착을 두 번 만들기
// 때문이다 — 그 결과가 없으면 이 줄은 지워도 그만인 정보로 읽힌다.
func heldGroupHint(n int) string {
	return fmt.Sprintf("사용자 항목과 함께 있는 우리 항목 %d그룹 — 호스트의 /hooks에서 그 항목을 지우세요(플러그인 훅과 겹쳐 같은 포착이 두 번 일어납니다)", n)
}

// ctrToolPrefix — 플러그인이 등록한 서버의 도구 이름 접두(D98). 조각 둘이 매니페스트에서 온다:
// 플러그인 이름(`.claude-plugin/plugin.json`의 `name`)과 MCP 서버 키(`plugin/mcp.json`의
// `mcpServers` 유일 키). 호스트가 조합하는 형태는 `mcp__plugin_<플러그인>_<서버>__`다. 어느 쪽
// 이름을 바꿔도 이 값이 함께 움직여야 doctor가 아무것도 매치하지 않는 접두를 안내하지 않는다 —
// TestToolPrefixMatchesPluginManifests가 그 두 파일을 읽어 이 값을 유도한다(최종 리뷰 S6:
// 이전 단정은 이 리터럴을 자기 자신으로 재고 있었다).
const ctrToolPrefix = "mcp__plugin_context-router_ctr__"

// hostSnippet: doctor 마지막에 출력하는 등록 안내(설계 §9, D96·D97·D98·D99·D101) — 등록은
// 플러그인 설치 절차이고(hostInstallSteps), 프로필을 아무것도 지정하지 않았을 때의 기본값은
// 서버 자신이 갖는다(D101 계약 2, v0.19 리뷰 C1 — plugin/mcp.json이 프로필을 고정하면
// CTR_ENABLE이 모든 플러그인 설치에서 죽은 경로가 된다). 도구 접두는 ctrToolPrefix가 든다 —
// alwaysLoad는 서버 단위 플래그가 폐기돼(D99) 안내하지 않는다.
const hostSnippet = `--- host adapter snippets (설계 §9) ---

` + hostInstallSteps + `
## 프로필

기본값은 서버가 갖는다 — --enable도 CTR_ENABLE도 지정하지 않으면 ingest,net으로 켜진다
(플러그인은 프로필을 고정하지 않는다). 바꾸려면 CTR_ENABLE 환경 변수를 쓴다 — 값은 --enable과
같은 쉼표 목록이다(예: CTR_ENABLE=ingest,net,exec). opt-in을 전부 끄려면 CTR_ENABLE=none을
쓴다(none은 값 전체일 때만 유효하고 다른 이름과 섞으면 오류다). --enable을 직접 넘기면 그
값이 항상 이긴다.

## 도구 접두

새 접두는 ` + ctrToolPrefix + `다. 손으로 넣어 둔 권한 규칙이 있으면 이 접두로
맞춰야 다시 매치한다.

permissions (.claude/settings.json 예시 — ingest/net 도구에 ask를 건다):
{
  "permissions": {
    "ask": ["` + ctrToolPrefix + `ctr_index", "` + ctrToolPrefix + `ctr_fetch_and_index"]
  }
}
# 이 두 도구 규칙은 그 프로필이 켜진 등록에서만 대상이 있다 — ctr_index는 ingest, ctr_fetch_and_index는
# net에서만 등록된다. 기본 프로필(ingest,net)은 둘 다 켜므로 위 규칙이 그대로 매치한다.
# exec까지 함께 쓰려면 CTR_ENABLE=ingest,net,exec로 프로필을 넓힌다.
# exec 2종(ctr_execute·ctr_execute_file)의 승인 강도는 호스트 권한 모드가 정한다. default
# 모드에서는 MCP 기본 프롬프트가 그대로 작동하고, 무프롬프트 모드이거나 그 도구를 덮는 allow
# 규칙(프롬프트의 '다시 묻지 않기'·--allowedTools가 남기는 항목)이 있으면 프롬프트 없이
# 실행된다 — 기존 ask를 지우면 그동안 가려져 있던 allow가 유효해진다(실측: ask 2종이 allow
# 1종을 무력화). ask 규칙을 넣으면 두 경우 모두 프롬프트가 강제된다: 무프롬프트 모드에서도
# ask는 프롬프트를 띄우고, 평가 순서(deny→ask→allow)가 ask를 allow보다 먼저 적용한다.
# 이중 동의는 유지된다: ① CTR_ENABLE에 exec를 포함한 프로필(기동 시) ② 호스트 권한 모델(모드와
# 규칙에 따름 — 무프롬프트 모드이거나 덮는 allow 규칙이 있으면 이 층은 프롬프트를 만들지 않는다).

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

// doctorCodexConfigPath — Codex config.toml 경로(CODEX_HOME 우선, 없으면 ~/.codex/config.toml).
// 기입 경로가 같은 규칙의 codexConfigPath를 codex_toml.go에 들고 있었고, doctor가 그 파일을
// 부르지 않도록 Task 4가 여기 8줄을 복제했다(D97 계약 2). 그 판단이 값을 했다: Task 5가
// codex_toml.go를 통째로 지웠고 이 함수는 손대지 않았다. 이제 유일한 자리다.
func doctorCodexConfigPath() (string, error) {
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		return filepath.Join(codexHome, "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("codex: 홈 디렉터리 해석 실패")
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

// codexOwnedServer — config.toml에서 우리 이름으로 확인된 서버 하나와 그 헤더가 나타난 줄들.
// Lines는 이미 렌더된 "4" 또는 "1,4"다 — 한 서버는 한 번 보고하고 한 번 지운다(제거는 서버
// 단위이지 헤더 줄 단위가 아니다).
type codexOwnedServer struct {
	Name  string
	Lines string
}

// ownedCodexHits — codexServerHeaders가 낸 헤더 줄에서 **우리가 등록한 적 있는 이름**의 것만
// 남긴다(릴리스 리뷰 F1). 판정은 hit.Name의 첫 점 앞 세그먼트가 owned에 있는가다 — [20]의
// ownedRegistration 관문과 같은 이름 집합을 쓰고, 파서를 부르지 않아 무효 TOML에서도 그대로
// 돈다. 첫 세그먼트로 자르는 것이 [mcp_servers.ctr.env] 서브테이블 헤더를 잡는 유일한 길이다
// (그 헤더만 홀로 남아도 TOML 점 표기 규칙상 부모 mcp_servers.ctr를 되살린다). 같은 서버의
// 헤더가 여럿이면 줄만 이어 붙여 한 항목으로 접는다. 출력 순서는 첫 등장 순(파일 순) —
// map 순회 순서가 doctor 출력으로 새어 나가지 않는다.
func ownedCodexHits(hits []codexHeaderHit, owned []string) []codexOwnedServer {
	var out []codexOwnedServer
	at := map[string]int{}
	for _, hit := range hits {
		// 자른 세그먼트를 다듬고 나서 비교한다 `[실측]`(재검토 리뷰 — 이 머신에서 헤더 철자
		// 열다섯을 재서 나온 거짓 음성 둘). hit.Name은 codexHeaderName이 첫 점 기준으로 나누지
		// 않은 원문이라 세그먼트 안에 TOML이 허용하는 점 주위 공백([mcp_servers.ctr . env] →
		// "ctr ")과 세그먼트 단위 인용([mcp_servers."ctr".env] → `"ctr"`)이 그대로 남는다. 둘 다
		// 유효 TOML이라 파스 실패 줄도 뜨지 않아, 다듬지 않고 비교하던 동안 그 사용자는 doctor
		// 한 번에서 아무 신호도 받지 못했다 — 손으로 고친 사용자가 정확히 이 스캐너의 대상이다.
		name, _, _ := strings.Cut(hit.Name, ".")
		name = unquoteHeaderName(strings.TrimSpace(name))
		if !slices.Contains(owned, name) {
			continue
		}
		if i, seen := at[name]; seen {
			out[i].Lines += "," + strconv.Itoa(hit.Line)
			continue
		}
		at[name] = len(out)
		out = append(out, codexOwnedServer{Name: name, Lines: strconv.Itoa(hit.Line)})
	}
	return out
}

// runDoctor: 5항목 진단(저장 루트/프로젝트 식별/content.db/FTS5/ledger.db) + 호스트 등록
// 스니펫을 w에 출력한다. store를 생성하지 않는다(store.Open(dir, true)만 사용, 설계 §7).
// 실패 항목이 있으면 error를 반환한다(main이 exit 1) — 반환 오류 메시지에는 절대경로를
// 담지 않는다(§12 canary), 대신 상세는 w의 진단 본문에 있다.
func runDoctor(ctx context.Context, w io.Writer, storeRoot, projectRoot, version string) error {
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
	// [9] 훅 등록 상태 — 프로젝트 + 사용자(~/.claude) 범위 양쪽 검사(F5: 사용자 스코프 등록을
	// 프로젝트-only 검사가 놓쳐 "미등록" 오보하던 문제). 두 경로 모두 uninstall과 동일한
	// hookSettingsPath 이음새로 도출한다(사용자 홈 = os.UserHomeDir).
	//
	// 이 줄이 세는 것은 **`.claude/settings.json`에 있는 우리 훅 그룹**이다. v0.19부터 훅 정의는
	// 플러그인 매니페스트가 나르므로(D96) 여기 잡히는 그룹은 전부 옛 설치기가 남긴 것이고, 그
	// 그룹은 지워질 때까지 계속 발화한다 — 플러그인 훅과 겹쳐 같은 포착이 두 번 일어나므로 마커
	// 형태와 무관하게 제거 걸음을 낸다(무버전 = D82 이후 v0.15+ 설치본이 다수 코호트다).
	//
	// uninstallCmd를 스코프마다 받는 이유(릴리스 리뷰 F3): `hook uninstall`은 프로젝트 파일이
	// 기본이라 사용자 스코프의 잔존물에 닿지 못한다. 사용자 스코프에 그 명령을 안내하면 사용자는
	// 다른 파일에서 "제거할 항목 없음"을 읽고 exit 0으로 끝나며, 잔존 그룹은 그대로 남아 doctor가
	// 같은 안내를 되풀이한다. 스코프마다 자기 자리에 닿는 철자를 낸다.
	hookScope := func(path string, pathErr error, uninstallCmd string) string {
		if pathErr != nil {
			return "확인불가"
		}
		n, held, marker, err := scanRegisteredHooks(path)
		var base string
		switch {
		case err != nil:
			return "파싱실패"
		case n == 0:
			// "미등록"이 아니라 "옛 그룹 없음"이다 — 플러그인 매니페스트로만 설치한 사용자는 이
			// 파일에 우리 그룹이 하나도 없는 것이 정상이고, 그때 "미등록"은 "훅이 꺼져 있다"로
			// 읽힌다. 이 줄이 세는 대상은 옛 설치기가 남긴 그룹뿐이다.
			base = "옛 그룹 없음"
		case marker == "":
			// D82 — v0.15 이후 등록물은 무버전 마커를 쓴다. 버전 비교는 여기서 하지 않지만
			// (상시 불일치 경고가 된다) 마이그레이션 걸음은 버전 있는 갈래와 **같은 문면으로**
			// 낸다 — 두 코호트가 처한 상황이 같기 때문이다.
			base = fmt.Sprintf("등록됨(%d개 — %s)", n, hookMigrationHint(uninstallCmd))
		case marker != version:
			base = fmt.Sprintf("등록됨(%d개, marker %s≠%s — %s)", n, marker, version, hookMigrationHint(uninstallCmd))
		default:
			base = fmt.Sprintf("등록됨(%d개, marker %s)", n, marker)
		}
		if held > 0 {
			// 동거 그룹은 "지울 수 있는 옛 그룹"과 부류가 다르므로 그 수에 섞지 않고 병기한다
			// (릴리스 리뷰 F4) — 섞으면 uninstall로 사라지지 않을 것을 uninstall 대상이라 말한다.
			base += ", " + heldGroupHint(held)
		}
		return base
	}
	projPath, _ := hookSettingsPath(false, projectRoot) // 프로젝트 경로는 오류를 내지 않는다
	userPath, userPathErr := hookSettingsPath(true, projectRoot)
	fmt.Fprintf(w, "[9] hooks: project=%s user=%s (.claude/settings.json의 우리 훅 그룹)\n",
		hookScope(projPath, nil, "hook uninstall"), hookScope(userPath, userPathErr, "hook uninstall --user"))

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
	projDir := filepath.Join(storeRoot, "projects", canon.ProjectID)
	if canon.ProjectID == "" {
		fmt.Fprintln(w, "[14] content.db: 없음")
	} else if s, err := store.SizeStats(projDir); err != nil || s == nil {
		fmt.Fprintln(w, "[14] content.db: 없음")
	} else {
		sz = s
		// 두 축의 **판정값**을 정보 줄에 병기한다(최종리뷰 F8, 릴리스 패스 B1·B2) — 경고가 없는
		// 상태에서 사용자가 판정을 손으로 재현해야 하는 것을 없앤다. file은 contentFootprint
		// (본체+`-wal`+`-shm`)이고 live는 store가 한 스냅샷 안에서 낸 값이다: 옛 file-free는 서로
		// 다른 두 스냅샷의 뺄셈이라 WAL이 클 때 0으로 읽혔다(정의는 store.SizeStats 주석).
		fileB := contentFootprint(projDir)
		// 측정 실패는 0과 갈라 적는다(릴리스 패스 M3) — free=0B가 "free page 없음"과 "pragma
		// 실패"를 같은 문면으로 내면 판정을 재현할 수 없다.
		freeS, liveS := "측정실패", "측정실패"
		if sz.PageStatsOK {
			freeS = strconv.FormatInt(sz.FreeBytes, 10) + "B"
			liveS = strconv.FormatInt(sz.LiveBytes, 10) + "B"
		}
		fmt.Fprintf(w, "[14] content.db: sources=%d artifacts=%d blob=%dB file=%dB(db+wal+shm) free=%s live=%s\n",
			sz.Sources, sz.Artifacts, sz.BlobBytes, fileB, freeS, liveS)
		// D38 — CAS 전체 blob 총량 경고(shadow 전용 아님 — [14] 측정 실체 그대로). 관측 채널이지
		// 정책 집행이 아니다(D27): [14] 자신은 아무것도 지우지 않는다. SizeStats 실패 경로는 이
		// 분기 밖이라 미평가.
		if warn := storeWarnBytes(os.Getenv); sz.BlobBytes > warn {
			fmt.Fprintf(w, "[14] warning: blob %dB > 임계 %dB(CTR_STORE_WARN_BYTES) — 수동 구제는 purge 계열 CLI(purge --project <id> --hook-only로 shadow만 선택 삭제 가능). shadow 귀속분은 기동 시 D67 퍼지가 보존 창 밖의 것을 자동 회수한다(explicit 소스는 자동 삭제 없음)\n", sz.BlobBytes, warn)
		}
		// D102 계약 6·8 — 축이 둘이다(소유자 판정, 릴리스 패스 B1·B2). **둘 다 뜰 수 있고 그것이
		// 의도다**: 서로 다른 질문에 답하기 때문이다.
		//
		// ① live 축 — "쓰레기가 쌓였나". 파일 크기로는 이 신호가 안 산다(자동 경로가 VACUUM을
		// 하지 않아 파일은 고수위에 머물고, 파일 기준 임계는 정리 뒤에도 상시 초과라 죽는다 —
		// D67의 관측된 결함). free는 **live 산출에서 차감할 뿐 독립된 경고 신호가 아니다**
		// (계약 7, 최종리뷰 F5) — 병합 안 된 세그먼트는 free page가 아니라 live page라 freelist는
		// 결함이 있을 때 오히려 낮게 읽힌다. 측정 실패(PageStatsOK=false)에서는 아예 판정하지
		// 않는다 — 0을 "깨끗하다"로 읽으면 안 된다(릴리스 패스 M3).
		if warn := contentLiveWarnBytes(os.Getenv); sz.PageStatsOK && sz.LiveBytes > warn {
			fmt.Fprintf(w, "[14] warning: live %dB > 임계 %dB(CTR_CONTENT_LIVE_WARN_BYTES) — 살아 있는 페이지(청크 텍스트+FTS) 축(자문, live=(page_count-freelist_count)×page_size 한 스냅샷). 콘텐츠가 늘었거나 병합이 밀렸다는 뜻이고 VACUUM으로는 안 줄어든다 — 줄이는 것은 삭제+병합이다: 훅 아티팩트 보존 창 %s(CTR_SHADOW_RETENTION) 밖은 기동 시 D67 퍼지가 자동 회수하고, 즉시 줄이려면 purge --project <id> --hook-only(shadow 귀속 한정, explicit 소스 감축은 전체 purge)\n",
				sz.LiveBytes, warn, store.ShadowRetention(os.Getenv))
		}
		// ② file 축 — "디스크를 얼마나 먹나". 0.19.0이 경고하던 자리를 되살린 것이다(릴리스 패스
		// B2): 계약 6이 판정을 live로 옮기면서 **회수 가능한 페이지로 부푼 파일**(정확히 VACUUM이
		// 필요한 상태)에 대해 아무 진단도 남지 않았다. 재는 값은 contentFootprint(본체+`-wal`+
		// `-shm`)라 계약 5가 적은 `-wal` 팽창도 이 축이 덮는다. 문면의 "자동 VACUUM 없음"은 옛
		// "자동 삭제 없음"을 고친 것이다 — 위에서 보고하는 artifacts 수는 D67 퍼지 때문에 사용자
		// 조작 없이 줄어든다.
		if warn := contentFileWarnBytes(os.Getenv); fileB > warn {
			fmt.Fprintf(w, "[14] warning: file %dB > 임계 %dB(CTR_CONTENT_FILE_WARN_BYTES) — content.db 총점유(본체+-wal+-shm) 축(자문). 회수 가능분 free=%s는 VACUUM이 되돌린다(라이브 서버 제약 — 서버 비가동 시 purge --project <id> --older-than <기간> --vacuum). 자동 VACUUM 없음 — 자동 경로는 free page 재사용에 기대므로 파일은 고수위에 머문다(D102 계약 5)\n",
				fileB, warn, freeS)
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

	// [16] codex 등록 상태 — 훅 스코프(D35)는 그대로 보고한다(Codex 등록물 진단과는 무관한
	// 별개 기제). 등록물 잔존은 순수 스캐너(codexServerHeaders, codex_scan.go)가 찾는다(D97
	// 계약 2). 옛 판정·마커·드리프트·verdict 경로(codex_toml.go)는 기입 경로와 함께 지워졌다.
	// uninstallCmd 스코프 분리·동거 부류 병기는 [9]와 같은 근거다(릴리스 리뷰 F3·F4) — 두 줄이
	// 같은 형태를 지지 않으면 한 호스트의 사용자만 닿지 않는 명령을 읽는다.
	codexHooksScope := func(path string, pathErr error, uninstallCmd string) (string, bool) { // (표시, 등록 존재)
		if pathErr != nil {
			return "확인불가", false
		}
		n, held, marker, scanErr := scanCodexRegisteredHooks(path)
		var base string
		switch {
		case scanErr != nil:
			return "파싱실패", false
		case n == 0:
			base = "옛 그룹 없음" // [9]와 같은 이유 — 플러그인만 쓰는 설치에서 정상 상태다
		case marker == "":
			// [9]의 무버전 갈래와 같은 근거(D82 + D96 매니페스트 훅).
			base = fmt.Sprintf("등록됨(%d개 — %s)", n, hookMigrationHint(uninstallCmd))
		case marker != version:
			base = fmt.Sprintf("등록됨(%d개, marker %s≠%s — %s)", n, marker, version, hookMigrationHint(uninstallCmd))
		default:
			base = fmt.Sprintf("등록됨(%d개, marker %s)", n, marker)
		}
		if held > 0 {
			base += ", " + heldGroupHint(held)
		}
		return base, n > 0
	}
	projCodexHooks, _ := codexHooksPath(false, projectRoot)
	userCodexHooks, userCodexHooksErr := codexHooksPath(true, projectRoot)
	projScope, _ := codexHooksScope(projCodexHooks, nil, "hook uninstall --codex")
	userScope, _ := codexHooksScope(userCodexHooks, userCodexHooksErr, "hook uninstall --codex --user")
	fmt.Fprintf(w, "[16] codex hooks: project=%s user=%s\n", projScope, userScope)

	// retiredServerNames — 옛 설치기가 등록에 쓴 적 있는 이름 전부: 현재 이름(ctrMCPServerName)과
	// D63 ②가 대체한 더 옛 이름(supersededMCPServerNames). [16]의 config.toml 절과 [20]의 두 절이
	// 같은 목록을 공유한다 — 절마다 다른 이름 집합을 보면 같은 상태에 절마다 다른 답이 나온다.
	retiredServerNames := append([]string{ctrMCPServerName}, supersededMCPServerNames...)

	// 플러그인 이전 방식 등록물 감지 — 무효 TOML에서도 동작하는 것이 codexServerHeaders의
	// 존재 이유다(D97 "알고 받는 대가") — 줄 단위 스캔이라 Codex 자신도 못 읽는 파일에서도 어느
	// 줄인지 짚을 수 있다. 파일 부재는 조용히 건너뛴다(등록물이 없으면 보고도 없다) — 그러나
	// 존재하는데 못 읽는 경우(권한 등)는 [19]와 같은 원칙으로 "읽기 실패"를 보고한다(리뷰
	// I3) — 못 연 파일을 조용히 "깨끗함"으로 읽으면 안 된다.
	//
	// "손편집"이라 부르지 않는다(재검토 리뷰 7) — 이 줄이 가리키는 등록물은 사용자가 직접 쓴
	// 것일 수도, hook install(우리 옛 설치기)이 쓴 것일 수도 있다. 둘 다에 대해 참인 사실은
	// "플러그인 경로 이전부터 있었다"는 것뿐이다.
	//
	// **보고는 우리 이름으로 거른다**(릴리스 리뷰 F1 — ownedCodexHits). codexServerHeaders는
	// 이름을 가리지 않는 스캐너라 [mcp_servers.github]처럼 남이 등록한 서버의 헤더도 그대로
	// 낸다. 거르지 않으면 우리 등록물을 가진 적 없는 사용자가 자기 서버 하나당 한 줄씩 "우리
	// 잔존물이 남아 있다"를 읽고 codex mcp remove로 남의 서버를 지운다. 관문은 [20]의 소유
	// 판정과 같은 자리다 — 거름은 보고 지점이 지고 스캐너는 순수한 채로 둔다(무효 TOML에서
	// 도는 성질이 스캐너의 존재 이유다).
	//
	// 그 거름이 생기면서 이름도 함께 인쇄한다(리뷰 I2의 반대 판정을 대체한다) — 보고되는 이름은
	// hit.Name의 추정값이 아니라 retiredServerNames의 리터럴이고, 점도 인용부호도 없는 알려진
	// 값 둘 중 하나라 codex mcp remove에 그대로 넘길 수 있다.
	//
	// **받아들이는 대가는 양방향으로 하나씩이고 둘 다 쟀다** `[실측]`(재검토 리뷰가 헤더 철자
	// 열다섯을 두 바이너리에서 재고, TestDoctorCodexOwnedServerFilter가 그 둘을 사례로 든다):
	//   ① 과검출 — 이름 자체에 점이 있는 인용 헤더([mcp_servers."ctr.env"])는 첫 세그먼트가
	//      ctr이라 우리 이름으로 잡히고 보고 이름도 ctr이 된다. 그 형태를 놓치지 않는 쪽을 고른
	//      것이다 — [mcp_servers.ctr.env] 서브테이블 헤더만 홀로 남아도 TOML 점 표기 규칙상 부모
	//      mcp_servers.ctr를 암묵적으로 되살리므로 첫 세그먼트 절단이 그 줄을 잡는 유일한 길이다.
	//   ② 미검출 — 닫는 따옴표를 빠뜨린 오타([mcp_servers."ctr])는 이름이 `"ctr`로 남는다.
	//      unquoteHeaderName은 양끝 짝이 맞을 때만 벗기므로 이 이름은 우리 이름과 맞지 않아
	//      등록물 보고에서 빠진다. 한 겹 더 벗기는 쪽은 두지 않는다 — 그 파일은 TOML로 파스되지
	//      않아 위 파스 실패 줄이 무조건 뜨고, 그 줄이 "파일을 열어 고치라"로 그 사용자를 같은
	//      자리에 데려다 놓는다. 조용히 넘어가는 코호트가 아니다.
	//
	// 파스 판정은 등록물 발견 여부와 무관하게 낸다(릴리스 리뷰 F2). Codex는 문법 오류 하나로
	// 그 파일 **전체**를 무시하므로 모델 설정도 [hooks] 신뢰 항목도 프로필도 함께 죽는다 —
	// mcp_servers 테이블이 없는 무효 파일에서 침묵하면 그 사용자는 아무 신호도 받지 못한다.
	// codexTOMLParses는 codex_toml_valid.go 소유다(D97 계약 2가 금하는 판정·마커·드리프트 경로
	// 밖) — 이 패키지에서 실제 TOML 파서를 부르는 유일한 자리이고 값을 읽지 않고 파스 성패만
	// 본다(D80 "파서 비의존"은 판정·기입 이관을 금하지 검증 전용 사용까지 막지 않는다).
	//
	// 다음 걸음은 그 파스 결과에 따라 갈린다(재검토 리뷰 6). 파스되면 codex mcp list·codex mcp
	// remove가 그 파일에 닿으므로 그 경로를 안내한다. 안 되면 Codex 자신도 그 파일을 못 읽어 그
	// 두 명령 다 닿지 못한다 — 이 갈래가 codexServerHeaders가 존재하는 이유다(D97, 설계문서 §6
	// "그 사용자는 손으로 지워야 하고 doctor가 어느 줄인지 짚는다"). 그래서 이 갈래만 손편집을
	// 직접 안내한다.
	codexCfgPath, codexCfgPathErr := doctorCodexConfigPath()
	switch {
	case codexCfgPathErr != nil:
		fmt.Fprintln(w, "[16] codex: config.toml 경로 확인불가")
	default:
		data, readErr := os.ReadFile(codexCfgPath)
		switch {
		case errors.Is(readErr, os.ErrNotExist):
			// 파일이 없다 — 보고 없음.
		case readErr != nil:
			fmt.Fprintln(w, "[16] codex: config.toml 읽기 실패")
		default:
			parses := codexTOMLParses(data)
			if !parses {
				fmt.Fprintf(w, "[16] codex: config.toml이 TOML로 파스되지 않습니다 — %s (Codex는 이 파일 전체를 무시합니다 — 모델 설정·[hooks] 신뢰 항목·프로필이 함께 죽습니다. 파일을 열어 문법을 고치세요)\n", codexCfgPath)
			}
			owned := ownedCodexHits(codexServerHeaders(data), retiredServerNames)
			for _, srv := range owned {
				fmt.Fprintf(w, "[16] codex: 플러그인 이전 방식의 등록물이 남아 있다 — %s:%s (%s)\n", codexCfgPath, srv.Lines, srv.Name)
			}
			switch {
			case len(owned) == 0:
				// 다음 걸음 없음.
			case parses:
				for _, srv := range owned {
					fmt.Fprintf(w, "[16] codex: 다음 걸음 — codex mcp remove %s 뒤 codex mcp list로 부재를 확인하세요(그 명령은 없는 이름에도 exit 0을 반환합니다)\n", srv.Name)
				}
			default:
				fmt.Fprintln(w, "[16] codex: 다음 걸음 — 이 파일은 TOML로 파스되지 않아 codex mcp list·codex mcp remove가 닿지 못합니다. 위에서 짚은 줄을 직접 열어 정리하세요")
			}
		}
		// 옛 설치기의 단일 슬롯 백업(D84). 그 슬롯을 만들던 경로도 그것을 지우던 경로도 없어서
		// 사용자 디스크에 영구 잔존한다 — doctor가 다른 잔존 부류를 전부 짚는데 이것만 빠지면
		// 마이그레이션을 마친 사용자에게 아무도 언급하지 않는 파일 하나가 남는다. 등록물 유무와
		// 무관하게 본다: 관리 블록을 이미 손으로 지운 사용자에게도 .bak은 남아 있다.
		if _, bakErr := os.Stat(codexCfgPath + ".bak"); bakErr == nil {
			fmt.Fprintf(w, "[16] codex: 옛 설치기가 남긴 config.toml 백업이 있다 — %s.bak (되돌릴 내용이 필요 없으면 지우세요)\n", codexCfgPath)
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

	// [20] .mcp.json 잔존 등록물 — [16]과 같은 모양(파일 + 다음 걸음)으로 보고한다(리뷰 I4b).
	// 옛 [20]은 라벨(존재·미등록·표식없음·버전)만 보여줬는데, 그 라벨은 마이그레이션에 쓸모가
	// 없다 — A⑧의 위험(호스트가 command·args 일치 서버를 경고 없이 버린다)이 .mcp.json 쪽에도
	// 그대로 있다. "손편집"이라 부르지 않는다(재검토 리뷰 7) — [16]과 같은 이유다.
	// 이름은 현재 이름(ctrMCPServerName)과 그보다 더 옛 이름(supersededMCPServerNames, D63
	// ②가 대체한 "ctr") 둘 다 확인한다(재검토 리뷰 4) — 아래 enabledMcpjsonServers 절이 이미
	// 두 이름을 보는데 이 절만 하나만 보면 같은 파일에서 절마다 다른 답을 낸다. 그 목록
	// (retiredServerNames)은 [16]이 이미 조립해 뒀다 — 이제 세 절이 한 목록을 공유한다.
	// 파일당 언마샬은 **한 번**이다(릴리스 리뷰 F7 — mcpServerEntries): 예전에는 파싱 판정 1회 +
	// 이름당 1회로 파일 하나를 세 번 훑었고, 그 파일 하나가 프로젝트별 이력을 통째로 든
	// `~/.claude.json`이라 무거운 사용자에게서 수십 MB를 반복해 파싱했다.
	// ownedRegistration은 이 절에서 계속 쓰인다(소유 판정).
	// markerDriftLabel은 doctor 어디서도 더는 부르지 않고 그 유일한 호출자였던
	// TestMarkerDriftLabel과 함께 지웠다(재검토 리뷰 3 — 프로덕션 호출자를 잃고 자기 테스트
	// 하나로만 살아있던 함수는, 그 테스트가 이 태스크 자신의 Files 안에 있는 한 orphan이다).
	// 자리는 둘이다(최종 리뷰 S11): 프로젝트 스코프 `.mcp.json`과 사용자 스코프 `~/.claude.json`
	// 최상위. 후자가 `claude mcp add --scope user`가 쓰는 자리이고 v0.18의 hostSnippet이 그
	// 스코프를 권했으므로, 프로젝트 파일만 보면 그 코호트에게 doctor가 "정리할 것 없음"을
	// 보고한다. 두 자리 모두 최상위 `mcpServers` 객체라 같은 판독기가 그대로 닿는다(설계 v0.12의
	// 스코프 표). 같은 파일의 `projects.<경로>.mcpServers`(local 스코프)는 보지 않는다 — 호스트가
	// 정규화해 둔 경로 키와 우리 projectRoot를 맞추는 일이 이 한 줄 진단보다 크다.
	mcpScanPaths := []string{mcpConfigPath(projectRoot)}
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		mcpScanPaths = append(mcpScanPaths, filepath.Join(home, ".claude.json"))
	}
	for _, mcpPath := range mcpScanPaths {
		mcpData, mcpReadErr := os.ReadFile(mcpPath)
		var entries map[string]mcpServerEntry
		parsed := false
		if mcpReadErr == nil {
			entries, parsed = mcpServerEntries(mcpData) // 파일당 언마샬 1회 — 아래 두 판정이 이 결과를 공유한다
		}
		switch {
		case errors.Is(mcpReadErr, os.ErrNotExist):
			// 없음 — 보고 없음.
		case mcpReadErr != nil:
			fmt.Fprintf(w, "[20] claude: %s 읽기 실패\n", mcpPath)
		case !parsed:
			// [16]과 같은 원칙(cli.go의 codexServerHeaders 절) — 못 연 파일을 조용히 "깨끗함"으로
			// 읽으면 안 된다. 쉼표 하나가 남은 파일은 파싱만 실패하고 등록물은 그대로 살아 있다.
			fmt.Fprintf(w, "[20] claude: %s 파싱 실패 — 잔존 등록물 여부를 판정하지 못했다(파일을 직접 열어 확인하세요)\n", mcpPath)
		default:
			for _, name := range retiredServerNames {
				e, found := entries[name]
				if !ownedRegistration(e.Managed, e.Command, found) {
					continue
				}
				fmt.Fprintf(w, "[20] claude: 플러그인 이전 방식의 등록물이 남아 있다 — %s (%s)\n", mcpPath, name)
				fmt.Fprintf(w, "[20] claude: 다음 걸음 — claude mcp remove %s 뒤 claude mcp list로 부재를 확인하세요\n", name)
			}
		}
	}

	// [20]의 두 번째 절 — enabledMcpjsonServers 잔존(소유자 판정 추가 항목, D97 인접). 다음
	// 이 키를 정리하던 우리 쓰기 코드는 v0.19에서 지워졌다(D96 계약 1 — 쓰지 않는다). 호스트
	// CLI(claude mcp remove)는 이 키를 건드리지 않으므로, 옛 등록물만 지운 사용자는 더는
	// 존재하지 않는 이름을 승인 목록에 남긴 채로 남는다. 세 스코프(local·project·user) 모두
	// 확인한다(리뷰 I5) — 이 키는 스코프 간 병합되지 않고 각 스코프의 최상위 정의가 그 스코프
	// 안에서 유효하므로, 한 스코프만 보면 다른
	// 스코프의 잔존을 놓친다. 이름은 위에서 이미 구한 retiredServerNames를 그대로 쓴다.
	localSettingsPath := filepath.Join(projectRoot, ".claude", "settings.local.json")
	userSettingsPath, userSettingsErr := hookSettingsPath(true, projectRoot)
	enabledScopePaths := []string{localSettingsPath, projPath} // projPath: [9]가 이미 구한 프로젝트 스코프
	if userSettingsErr == nil && userSettingsPath != projPath {
		// 재검토 리뷰 5: projectRoot==HOME이면 user와 project 스코프가 같은 파일이라 중복
		// 추가하면 같은 잔존을 두 번 읽고 두 번 찍는다. local은 파일명이 달라(settings.local.json)
		// 이 충돌이 구조적으로 없다.
		enabledScopePaths = append(enabledScopePaths, userSettingsPath)
	}
	for _, p := range enabledScopePaths {
		// 읽기·파싱 실패를 조용히 건너뛰지 않는다(릴리스 리뷰 F5) — 마흔 줄 위의 [20] 앞 절과
		// [19]·[16]이 이미 세운 원칙이고, 여기서 놓치는 잔존은 **어느 호스트 CLI도 지우지 않는**
		// 키다(claude mcp remove는 등록물만 건드린다). 확인하지 못한 스코프를 깨끗함으로 읽으면
		// 그 사용자에게는 아무도 이 키를 언급하지 않는다. 부재만 조용하다 — 그 스코프에 파일이
		// 없다는 것은 판정된 사실이다.
		data, err := os.ReadFile(p)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(w, "[20] claude: %s 읽기 실패 — enabledMcpjsonServers 잔존 여부를 판정하지 못했다(파일을 직접 열어 확인하세요)\n", p)
			}
			continue
		}
		// 빈 파일·공백뿐인 파일은 판정된 사실이다 — 그 안에 잔존 이름은 없다. 형제
		// scanRegisteredHooks가 같은 가드로 같은 바이트를 `옛 그룹 없음`으로 읽으므로(hook_install.go)
		// 이 가드가 없으면 doctor 한 번의 출력에서 [9]와 [20]이 같은 파일을 다르게 판정하고,
		// 아무것도 없는 파일을 열어 보라는 안내가 남는다 `[실측]`(재검토 리뷰 — 0바이트와 공백뿐인
		// .claude/settings.json에서 두 줄을 함께 재서 확인).
		if len(bytes.TrimSpace(data)) == 0 {
			continue
		}
		var enabledDoc struct {
			Enabled []string `json:"enabledMcpjsonServers"`
		}
		if json.Unmarshal(data, &enabledDoc) != nil {
			fmt.Fprintf(w, "[20] claude: %s 파싱 실패 — enabledMcpjsonServers 잔존 여부를 판정하지 못했다(파일을 직접 열어 확인하세요)\n", p)
			continue
		}
		for _, name := range retiredServerNames {
			if slices.Contains(enabledDoc.Enabled, name) {
				fmt.Fprintf(w, "[20] claude: %s의 enabledMcpjsonServers에 옛 서버 이름 %q가 남아 있다 — 배열에서 지우세요\n", p, name)
			}
		}
	}

	fmt.Fprintln(w)
	fmt.Fprint(w, hostSnippet)

	if len(failed) > 0 {
		return fmt.Errorf("doctor: 진단 실패 항목 %d개", len(failed))
	}
	return nil
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
