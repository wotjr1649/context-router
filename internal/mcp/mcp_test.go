package mcp

import (
	"bufio"
	"bytes"
	"context"
	"database/sql" // 원장 열을 직접 읽는 테스트용 — mcp **본체**는 아키텍처 "부패 방지 계약"이 이 import를 금지한다
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/ingest"
	"github.com/wotjr1649/context-router/internal/netfetch"
	"github.com/wotjr1649/context-router/internal/sandbox"
	"github.com/wotjr1649/context-router/internal/session"
	"github.com/wotjr1649/context-router/internal/store"
	"github.com/wotjr1649/context-router/internal/transform"
)

// testSelfExe: 실 ctr 바이너리를 1회만 빌드해(sync.Once) 재사용한다 — ctr_transform은
// 실제 "__transform-worker" 프로세스 경계를 타므로(internal/transform/worker_test.go와
// 동형 패턴), NewServer의 ProbeIsolation·Spawn이 프로덕션과 동일 경로로 검증된다.
var (
	testExeOnce sync.Once
	testExePath string
	testExeErr  error
)

func testSelfExe(t *testing.T) string {
	t.Helper()
	testExeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ctr-mcp-test-*")
		if err != nil {
			testExeErr = err
			return
		}
		bin := filepath.Join(dir, "ctr-test")
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", bin, "github.com/wotjr1649/context-router/cmd/context-router")
		if out, err := cmd.CombinedOutput(); err != nil {
			testExeErr = fmt.Errorf("selfExe 빌드 실패: %w: %s", err, out)
			return
		}
		testExePath = bin
	})
	if testExeErr != nil {
		t.Fatalf("selfExe 빌드 실패: %v", testExeErr)
	}
	return testExePath
}

func TestMain(m *testing.M) {
	code := m.Run()
	if testExePath != "" {
		os.RemoveAll(filepath.Dir(testExePath))
	}
	os.Exit(code)
}

func TestToToolError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{"not_found", store.ErrNotFound, codeNotFound},
		{"not_found_wrapped", fmt.Errorf("op: %w", store.ErrNotFound), codeNotFound},
		{"invalid_selector", store.ErrInvalidSelector, codeInvalidArgument},
		{"unavailable", store.ErrUnavailable, codeStorageUnavailable},
		{"no_isolation", transform.ErrNoIsolation, codeStorageUnavailable},
		{"budget", transform.ErrBudget, codeBudgetExceeded},
		{"output_limit", transform.ErrOutputLimit, codeOutputLimitExceeded},
		{"workspace", ingest.ErrWorkspace, codeWorkspaceViolation},
		{"unsupported", ingest.ErrUnsupported, codeUnsupportedFile},
		{"network_denied", netfetch.ErrDenied, codeNetworkDenied},
		{"body_too_large", netfetch.ErrBodyTooLarge, codeOutputLimitExceeded},
		{"too_many_redirects", netfetch.ErrTooManyRedirects, codeNetworkDenied},
		{"unsupported_media", netfetch.ErrUnsupportedMedia, codeUnsupportedFile},
		{"not_exist", fs.ErrNotExist, codeNotFound},
		{"not_exist_wrapped", fmt.Errorf("ingest: canonicalize: %w", fs.ErrNotExist), codeNotFound},
		{"session_lease_held", session.ErrLeaseHeld, codeStorageUnavailable},
		{"session_recover_pending", session.ErrRecoverPending, codeStorageUnavailable},
		{"session_corrupt", session.ErrCorrupt, codeStorageUnavailable},
		{"unknown", errors.New("boom"), codeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toToolError(tt.err).Error()
			want := "[" + tt.code + "]"
			if !strings.HasPrefix(got, want) {
				t.Fatalf("toToolError(%v) = %q, want prefix %q", tt.err, got, want)
			}
		})
	}
}

// TestToToolErrorCancellation: 최종리뷰 C2(fable Imp3) — 취소/데드라인은 sentinel 매핑을
// 타지 않고 SDK가 처리하도록 원본 ctx 오류를 그대로 반환한다(INTERNAL로 뭉개거나 slog
// 소음을 내지 않음). toToolError가 모든 핸들러의 단일 오류 변환 지점이므로(§6) 여기서
// 검증하면 handler 호출 경로 전체를 대표한다.
func TestToToolErrorCancellation(t *testing.T) {
	tests := []error{
		context.Canceled,
		context.DeadlineExceeded,
		fmt.Errorf("ingest: run web: %w", context.Canceled),
	}
	for _, err := range tests {
		got := toToolError(err)
		if got != err {
			t.Fatalf("toToolError(%v) = %v, want original error unchanged", err, got)
		}
		if strings.Contains(got.Error(), codeInternal) {
			t.Fatalf("toToolError(%v) = %q, want no INTERNAL code", err, got)
		}
	}
}

// newTestServer: canon+실물 store로 서버를 만들고 in-memory transport로 클라이언트까지 연결한다.
func newTestServer(t *testing.T, enable []string) (*mcp.ClientSession, ident.Canon) {
	t.Helper()
	dir := t.TempDir()
	canon, err := ident.Canonicalize(dir)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	st, err := store.Open(t.TempDir(), false)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// ScratchRoot: exec 프로필(Enable "exec")이 sandbox.NewScratch 부모로 쓴다 — 빈 값이면
	// exec.Run이 ErrSetup. t.TempDir()는 OS temp 하위(D58)이고 테스트 종료 시 자동 정리된다.
	srv, err := NewServer(Config{Canon: canon, Store: st, SelfExe: testSelfExe(t), ScratchRoot: t.TempDir(), Enable: enable})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	srvT, cliT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, srvT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, cliT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, canon
}

// skipDarwinNoIsolation: transform 패키지와 동일 원인(3-OS CI 최초 실행 — darwin에서
// RLIMIT_AS self-apply가 항상 실패, internal/transform/worker_test.go의 동명 헬퍼 참조)이
// 여기서는 ctr_transform 도구 자체가 미등록되는 형태로 나타난다(NewServer가
// transform.ProbeIsolation 실패 시 등록을 건너뛴다 — in-process fallback 금지, 설계
// §4.3/§5.3). 이 도구 전용 테스트는 도구가 없으면 검증할 대상이 없다.
func skipDarwinNoIsolation(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("darwin: ctr_transform이 RLIMIT_AS self-apply 실패로 미등록 — 백로그: darwin 메모리 격리 전략 재설계")
	}
}

func TestNewServerProfileGating(t *testing.T) {
	tests := []struct {
		name   string
		enable []string
		want   []string
	}{
		{"base", nil, []string{"ctr_fetch", "ctr_search", "ctr_transform"}},
		{"ingest", []string{"ingest"}, []string{"ctr_fetch", "ctr_index", "ctr_search", "ctr_transform"}},
		{"net", []string{"net"}, []string{"ctr_fetch", "ctr_fetch_and_index", "ctr_search", "ctr_transform"}},
		// exec 프로필: sandbox.Probe 통과 시 ctr_execute·ctr_execute_file 2종을 추가 등록한다
		// (unix Probe는 no-op nil, windows는 Job Object 실게이트지만 테스트 환경에서 통과 —
		// T5 TestExecuteRegisteredAndRuns가 실왕복 실증). darwin은 아래 DeleteFunc가 ctr_transform만
		// 제거하고 exec 2종은 그대로 남긴다(exec는 격리 프로브가 unix에서 무조건 성공).
		{"exec", []string{"exec"}, []string{"ctr_execute", "ctr_execute_file", "ctr_fetch", "ctr_search", "ctr_transform"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.want
			if runtime.GOOS == "darwin" {
				// darwin: RLIMIT_AS self-apply가 항상 실패해 ctr_transform이 미등록된다
				// (skipDarwinNoIsolation 주석 참조) — 이 서브테스트가 검증하는 프로필별
				// ingest/net 게이팅 자체는 darwin에서도 유효하니 그 부분은 계속 확인한다.
				want = slices.DeleteFunc(slices.Clone(tt.want), func(s string) bool { return s == "ctr_transform" })
			}
			if sandbox.Probe(context.Background()) != nil {
				// exec: sandbox.Probe 실패 환경(예: Job Object 미가용 windows)에서 NewServer는
				// fail-closed로 ctr_execute·ctr_execute_file를 미등록한다(mcp.go:93-98). 프로필
				// 게이팅 자체 검증은 유지한 채 두 도구만 기대에서 제외한다(위 darwin ctr_transform
				// 선례와 동형 — 형제 TestExecuteRegisteredAndRuns의 skip과 동일 프로브 인지).
				want = slices.DeleteFunc(slices.Clone(want), func(s string) bool {
					return s == "ctr_execute" || s == "ctr_execute_file"
				})
			}
			cs, _ := newTestServer(t, tt.enable)
			lt, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatalf("list tools: %v", err)
			}
			got := make([]string, len(lt.Tools))
			for i, tl := range lt.Tools {
				got[i] = tl.Name
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("tools=%v want %v", got, want)
			}
		})
	}
}

// maxToolSchemaBytes: 게이트 11(스키마 토큰 예산, 설계 §2.3·§3.5) — v0.15 재기준화로 측정
// 표면이 **설치기가 만들 수 있는 최대 프로필**이 됐다: 세션 정상 open의 6-도구
// (ctr_search/ctr_fetch/ctr_transform + ctr_record_event/ctr_session_summary/ctr_export_events)에
// 설치기 기본 프로필 ingest,net의 2종(ctr_index·ctr_fetch_and_index)과 --enable-exec 형태의
// 2종(ctr_execute·ctr_execute_file)을 더한 10-도구다(D81). 이전 형태는 exec 프로필만 켠
// 8-도구(12914B)를 재서, D81이 여는 10-도구 표면이 옛 상한 13000을 1853B 넘겨도 통과했다 —
// 물리지 않는 검사였다. tools/list 결과 JSON 직렬화 바이트 상한이며 설명 문구 비대화 회귀의
// 조기 감지용이다. D62부터의 규칙 그대로 **실측 + 최소 여유**로 잡는다(과대 상향 금지 —
// 여유가 100B 이상이면 "설명에 100바이트를 덧붙이면 넘긴다"는 감시선이 죽는다).
// [실측] D99(진입 도구 ctr_search·ctr_index 상시 로드 + Description 꼬리에 지연 여섯을 색인,
// 이 파일의 deferredToolIndex)가 표면을 14853B→15795B로 키웠다(+942B: deferredToolIndex
// 432B가 두 도구 Description에 각각 붙어 864B, `"_meta":{"anthropic/alwaysLoad":true}`가
// 그 두 도구에만 38B씩 76B — 합 940B, 나머지 2B는 JSON 콤마 재배치). 100의 배수로 올려 15800
// (여유 5B — 위 규칙대로 의도적으로 빡빡하게 유지, 폐기된 이전 값 14900). 이 값은 **단일 서버**의
// tools/list 표면이다 — 클라이언트가 ctr·ctr-exec를 함께 등록하면 상주 총량은 두 서버 합집합이라
// 다르다(더 오래된 폐기 값: exec 8-도구 12914→13000, 6-도구 10024×1.2=12029).
// [실측] 릴리스 리뷰 F1(2026-08-08, 이 머신)이 exec 둘을 색인에 넣어 15795B→16067B가 됐다
// (+272B: idxExecute 65B + idxExecuteFile 67B + 구분자 ", " 둘 4B = 136B가 진입 도구 둘의
// Description에 각각 붙는다). 위 규칙대로 100의 배수로 올려 16100(여유 33B, 폐기된 이전 값
// 15800). 이 증가분은 D99 설계가 값을 치르기로 한 자리다 — 색인에 없는 지연 도구는 모델이
// 호출하지 않는다는 것이 이 색인의 근거이고, F1은 그 상태를 exec 프로필에서 실측한 것이다.
// 실측값·근거는 docs/gates-v0.0.1-ko.md 게이트 11 항목 참조(정식 갱신은 게이트 문서 마일스톤에서).
const maxToolSchemaBytes = 16100

// TestSchemaTokenBudget: tools/list 결과(ListToolsResult 전체 — 실제 클라이언트가 받는
// JSON 그대로) 직렬화 바이트가 maxToolSchemaBytes를 넘지 않는지 확인한다(게이트 11). 근사
// 토큰 수 = bytes/4는 로그로만 남긴다 — Claude 정확 tokenizer는 비공개라 근사치일 뿐이고,
// 실질 게이트는 바이트 상한 쪽이다.
func TestSchemaTokenBudget(t *testing.T) {
	// newRecordEventTestServer(t, "ingest", "net", "exec"): 세션 6-도구 + ingest 1 + net 1 +
	// exec 2 = **설치기가 만들 수 있는 최대 프로필**의 10-도구 표면이다(D81 — 기본 ingest,net에
	// --enable-exec을 더한 형태). 프로브가 실패하는 환경이면 도구가 줄지만 상한은 상계이므로
	// 여전히 통과한다.
	cs, _, _, _ := newRecordEventTestServer(t, "ingest", "net", "exec")
	lt, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	b, err := json.Marshal(lt)
	if err != nil {
		t.Fatalf("marshal tools/list result: %v", err)
	}
	approxTokens := len(b) / 4 // 근사치(bytes/4) — 정확 tokenizer 비공개, 주석 참조.
	t.Logf("tools/list schema: %d bytes (~%d tokens approx, %d tools)", len(b), approxTokens, len(lt.Tools))
	if len(b) > maxToolSchemaBytes {
		t.Fatalf("tools/list schema=%d bytes exceeds budget %d bytes (게이트 11, 설계 §2.3)", len(b), maxToolSchemaBytes)
	}
}

// assertDeferredIndexMatchesRegistration — 진입 도구의 Description 꼬리 색인이 그 서버의
// tools/list와 정확히 맞물리는지 확인한다: 등록된 지연 도구는 전부 색인에 이름이 있고, 색인이
// 든 이름은 전부 등록돼 있다(양방향).
//
// **후보 집합도 서버에서 얻는다** `[실측]`(재검토 리뷰, 이 머신). 옛 형태는 진리를
// tools/list에서 얻으면서 후보는 손으로 유지하는 이름 목록에서 훑었고, 그래서 mcp.go의
// deferred에 든 도구가 그 목록에서 빠지면 기대값이 검사 대상과 같은 누락에서 파생돼 결함이
// 초록으로 통과했다 — 릴리스 리뷰 F1의 exec 둘이 정확히 그 형태였다(`CTR_ENABLE=…,exec`로
// 켠 서버가 ctr_execute를 등록하고도 색인 문장에서 이름을 뺐다). 이제 후보는 tools/list에서
// `anthropic/alwaysLoad`가 달린 진입 도구를 뺀 나머지라, 지연 도구가 아홉째로 늘어도 이
// 단정이 손질 없이 함께 는다. 진입 도구가 정확히 어느 둘인지는
// TestAlwaysLoadMetaExactlyEntryTools가 따로 잰다.
//
// 색인 문장의 머리말과 항목 구분자는 deferredToolIndex 자신에서 유도한다 — 문면이 바뀌면 이
// 단정이 따라간다. 항목에서 이름을 `(` 앞까지로 끊는 이유는 항목이 `이름(한 줄 용도)` 형태라
// `ctr_fetch`가 `ctr_fetch_and_index`의 부분 문자열로 잡히지 않게 하기 위해서다.
func assertDeferredIndexMatchesRegistration(t *testing.T, cs *mcp.ClientSession) {
	t.Helper()
	lt, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var entryTools []string
	deferredNames, descByName := map[string]bool{}, map[string]string{}
	for _, tl := range lt.Tools {
		if v, _ := tl.Meta["anthropic/alwaysLoad"].(bool); v {
			entryTools = append(entryTools, tl.Name)
			descByName[tl.Name] = tl.Description
			continue
		}
		deferredNames[tl.Name] = true
	}
	if len(entryTools) == 0 {
		// 진입 도구가 하나도 없으면 아래 루프가 통째로 돌지 않아 이 단정이 공회전한다.
		t.Fatalf("alwaysLoad 도구가 tools/list에 없다 — 색인을 실을 자리가 없다: %d개 도구", len(lt.Tools))
	}
	indexLead, _, _ := strings.Cut(deferredToolIndex([]string{"\x00"}), "\x00")
	for _, entry := range entryTools {
		_, tail, found := strings.Cut(descByName[entry], indexLead)
		if !found {
			t.Errorf("%s 설명에 지연 도구 색인 문장이 없다:\n%s", entry, descByName[entry])
			continue
		}
		var order []string
		indexed := map[string]bool{}
		for _, item := range strings.Split(strings.TrimSuffix(tail, "."), ", ") {
			name, _, _ := strings.Cut(item, "(")
			order = append(order, name)
			indexed[name] = true
		}
		for _, tl := range lt.Tools { // tools/list 순서로 — 보고가 결정적이다
			if deferredNames[tl.Name] && !indexed[tl.Name] {
				t.Errorf("%s 설명의 색인이 등록된 지연 도구 %q를 빠뜨렸다:\n%s", entry, tl.Name, descByName[entry])
			}
		}
		for _, name := range order {
			if !deferredNames[name] {
				t.Errorf("%s 설명의 색인이 등록되지 않은 %q를 광고한다:\n%s", entry, name, descByName[entry])
			}
		}
	}
}

// TestAlwaysLoadMetaExactlyEntryTools — D99 요구 1: Enable{"ingest","net"}+Session 있음(설치기가
// 만들 수 있는 프로필)에서 tools/list의 _meta.anthropic/alwaysLoad=true인 도구 이름 집합이
// 정확히 {ctr_search, ctr_index}여야 한다 — 다른 어떤 도구에도 새지 않는지까지 함께 확인한다.
func TestAlwaysLoadMetaExactlyEntryTools(t *testing.T) {
	cs, _, _, _ := newRecordEventTestServer(t, "ingest", "net")
	lt, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	got := map[string]bool{}
	for _, tl := range lt.Tools {
		if v, _ := tl.Meta["anthropic/alwaysLoad"].(bool); v {
			got[tl.Name] = true
		}
	}
	want := map[string]bool{"ctr_search": true, "ctr_index": true}
	if len(got) != len(want) {
		t.Fatalf("alwaysLoad tools=%v want exactly %v", got, want)
	}
	for name := range want {
		if !got[name] {
			t.Fatalf("alwaysLoad tools=%v missing %q", got, name)
		}
	}
}

// TestEntryToolDescriptionsIndexDeferredTools — D99 요구 2 + 최종 리뷰 S4 재기준선. 옛 형태는
// 고정 문면(패키지 상수)이 지연 여섯의 이름을 전부 담는지만 봤고, 그래서 프로필을 좁힌 서버가
// 등록하지도 않은 도구를 이름으로 광고해도 초록이었다 — `CTR_ENABLE=ingest`로 켠 서버가
// `ctr_fetch_and_index`를 광고하는 것을 stdio로 실측한 것이 그 결함이다. 여덟 중 일곱이 조건부
// 등록이므로 최대 프로필과 좁힌 프로필 양쪽에서 색인과 등록의 일치를 잰다. 반대 방향(등록했는데
// 색인에 없음)도 같은 단정이 잡는다 — 릴리스 리뷰 F1의 exec 둘이 그 방향이었다.
func TestEntryToolDescriptionsIndexDeferredTools(t *testing.T) {
	t.Run("최대 프로필 — 여덟 전부", func(t *testing.T) {
		cs, _, _, _ := newRecordEventTestServer(t, "ingest", "net", "exec")
		assertDeferredIndexMatchesRegistration(t, cs)
	})
	t.Run("좁힌 프로필 — net·세션 없음", func(t *testing.T) {
		// ingest만: ctr_fetch_and_index(net)도 세션 3종도 등록되지 않는다.
		cs, _ := newTestServer(t, []string{"ingest"})
		assertDeferredIndexMatchesRegistration(t, cs)
	})
}

// TestToolDescriptionsAvoidHortatoryVocabulary — Global Constraints 어휘 규칙: 어느 도구
// 설명에도 금지 어휘가 없어야 한다. 최대 프로필(10-도구)로 D99 신규 문면(deferredToolIndex)까지
// 전수 검사한다.
func TestToolDescriptionsAvoidHortatoryVocabulary(t *testing.T) {
	cs, _, _, _ := newRecordEventTestServer(t, "ingest", "net", "exec")
	lt, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	banned := []string{"MANDATORY", "BLOCKED", "Do NOT", "Never", "PREFER"}
	for _, tl := range lt.Tools {
		for _, word := range banned {
			if strings.Contains(tl.Description, word) {
				t.Fatalf("%s description contains banned vocabulary %q: %q", tl.Name, word, tl.Description)
			}
		}
	}
}

func remarshal(t *testing.T, v, out any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("remarshal unmarshal: %v", err)
	}
}

func TestRoundTrip(t *testing.T) {
	cs, canon := newTestServer(t, []string{"ingest"})
	ctx := context.Background()

	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tl := range lt.Tools {
		byName[tl.Name] = tl
	}
	if tl := byName["ctr_search"]; tl == nil || tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
		t.Fatalf("ctr_search readOnlyHint 누락: %+v", tl)
	}
	if tl := byName["ctr_fetch"]; tl == nil || tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
		t.Fatalf("ctr_fetch readOnlyHint 누락: %+v", tl)
	}

	tmpFile := filepath.Join(canon.ProjectRoot, "note.txt")
	if err := os.WriteFile(tmpFile, []byte("needle content for round trip\n"), 0o644); err != nil {
		t.Fatalf("write tmp file: %v", err)
	}
	idxRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_index", Arguments: IndexInput{Path: tmpFile}})
	if err != nil {
		t.Fatalf("ctr_index call: %v", err)
	}
	if idxRes.IsError {
		t.Fatalf("ctr_index error: %+v", idxRes.Content)
	}
	var idxOut IndexOutput
	remarshal(t, idxRes.StructuredContent, &idxOut)
	if idxOut.Indexed != 1 {
		t.Fatalf("indexed=%d want 1 (skipped=%+v)", idxOut.Indexed, idxOut.Skipped)
	}

	searchRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_search", Arguments: SearchInput{Queries: []string{"needle"}}})
	if err != nil {
		t.Fatalf("ctr_search call: %v", err)
	}
	if searchRes.IsError {
		t.Fatalf("ctr_search error: %+v", searchRes.Content)
	}
	var searchOut SearchOutput
	remarshal(t, searchRes.StructuredContent, &searchOut)
	if !searchOut.Untrusted {
		t.Fatalf("untrusted flag missing: %+v", searchOut)
	}
	if len(searchOut.Results) != 1 || len(searchOut.Results[0].Hits) == 0 {
		t.Fatalf("no hits: %+v", searchOut.Results)
	}
	hit := searchOut.Results[0].Hits[0]

	chunkID := hit.ChunkID
	fetchRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_fetch", Arguments: FetchInput{ArtifactID: hit.ArtifactID, ChunkID: &chunkID}})
	if err != nil {
		t.Fatalf("ctr_fetch call: %v", err)
	}
	if fetchRes.IsError {
		t.Fatalf("ctr_fetch error: %+v", fetchRes.Content)
	}
	var fetchOut FetchOutput
	remarshal(t, fetchRes.StructuredContent, &fetchOut)
	if fetchOut.ExactScope != "artifact" {
		t.Fatalf("exact_scope=%q want artifact", fetchOut.ExactScope)
	}
	if fetchOut.Content == "" {
		t.Fatalf("fetch content empty")
	}
	if fetchOut.Provenance.SrcHash == "" {
		t.Fatalf("provenance.src_hash empty: %+v", fetchOut.Provenance)
	}
	if fetchOut.Provenance.Source == "" || filepath.IsAbs(fetchOut.Provenance.Source) {
		t.Fatalf("provenance.source want project-relative, got %q", fetchOut.Provenance.Source)
	}
	if fetchOut.Provenance.Stale {
		t.Fatalf("provenance.stale=true, want false before modification")
	}

	// 파일 수정 후 재-fetch: 같은 chunk 선택자라도 provenance.stale이 true로 바뀌어야 한다.
	future := time.Now().Add(time.Hour)
	if err := os.WriteFile(tmpFile, []byte("needle content MODIFIED for round trip\n"), 0o644); err != nil {
		t.Fatalf("modify tmp file: %v", err)
	}
	if err := os.Chtimes(tmpFile, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	fetchRes2, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_fetch", Arguments: FetchInput{ArtifactID: hit.ArtifactID, ChunkID: &chunkID}})
	if err != nil {
		t.Fatalf("ctr_fetch call 2: %v", err)
	}
	if fetchRes2.IsError {
		t.Fatalf("ctr_fetch error 2: %+v", fetchRes2.Content)
	}
	var fetchOut2 FetchOutput
	remarshal(t, fetchRes2.StructuredContent, &fetchOut2)
	if !fetchOut2.Provenance.Stale {
		t.Fatalf("provenance.stale=false after modification, want true: %+v", fetchOut2.Provenance)
	}
}

// TestServeStdoutPurity: 실제 Serve()가 stdio에 물릴 os.Stdin/os.Stdout을 파이프로 교체해
// JSON-RPC 프로토콜 외 바이트가 stdout에 전혀 쓰이지 않음을 확인한다(배너는 stderr, §5.5).
func TestServeStdoutPurity(t *testing.T) {
	dir := t.TempDir()
	canon, err := ident.Canonicalize(dir)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	st, err := store.Open(t.TempDir(), false)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	defer st.Close()

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	inW.Close() // 즉시 EOF → Run()이 곧바로 반환
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	serveErr := Serve(context.Background(), Config{Canon: canon, Store: st})
	os.Stdin, os.Stdout = oldIn, oldOut
	outW.Close()

	data, err := io.ReadAll(outR)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("stdout polluted: %q (serve err=%v)", data, serveErr)
	}
}

// TestServeStdoutPurityDuringErroringToolCall: Task 8 최종리뷰 minor "stdout purity 테스트
// narrow(툴콜 중 오염 미검)" 해소(계획 3 게이트 10) — 위 TestServeStdoutPurity는 stdin을 즉시
// 닫아 실제 툴콜이 한 번도 실행되지 않는다. 여기서는 실제 initialize→tools/call 왕복 도중
// 핸들러가 진짜 오류를 내며(store를 미리 Close — mock이 아니라 실 리소스의 실 종료 상태)
// db.QueryContext가 반환하는 오류가 toToolError의 어떤 sentinel에도 매칭되지 않아 default
// 분기(codeInternal)로 떨어져 slog.Error가 stderr에 기록되는 바로 그 순간에도 stdout에는
// 개행 구분 JSON-RPC 응답만 나오는지 확인한다(§5.5).
func TestServeStdoutPurityDuringErroringToolCall(t *testing.T) {
	dir := t.TempDir()
	canon, err := ident.Canonicalize(dir)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	st, err := store.Open(t.TempDir(), false)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	st.Close() // 의도적: 이후 모든 쿼리가 실 오류("database is closed" 부류)로 실패한다.

	// slog 기본 핸들러는 os.Stderr *값을 생성 시점에 캡처*하므로(main.go의 run()이 동일
	// 이유로 slog.SetDefault(stderr)를 명시 호출한다) 아래 os.Stdout 스와핑과 달리 전역
	// os.Stderr 재대입만으로는 캡처되지 않는다 — 핸들러를 직접 버퍼로 교체해야 한다.
	prevLogger := slog.Default()
	var stderrBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&stderrBuf, nil)))
	defer slog.SetDefault(prevLogger)

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer inR.Close()
	defer inW.Close()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer outR.Close()
	defer outW.Close()

	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()

	serveDone := make(chan error, 1)
	go func() { serveDone <- Serve(context.Background(), Config{Canon: canon, Store: st}) }()

	writeLine := func(v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := inW.Write(append(b, '\n')); err != nil {
			t.Fatalf("write stdin: %v", err)
		}
	}
	sc := bufio.NewScanner(outR)
	readLine := func() json.RawMessage {
		t.Helper()
		if !sc.Scan() {
			t.Fatalf("scan stdout: %v", sc.Err())
		}
		line := sc.Bytes()
		if !json.Valid(line) {
			t.Fatalf("stdout line is not valid JSON (protocol pollution): %q", line)
		}
		return json.RawMessage(append([]byte(nil), line...))
	}

	writeLine(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "purity-test", "version": "0.0.1"},
		},
	})
	readLine() // initialize 응답 — JSON 유효성만 확인.
	writeLine(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized", "params": map[string]any{}})

	writeLine(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "ctr_search", "arguments": SearchInput{Queries: []string{"needle"}}},
	})
	var resp struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(readLine(), &resp); err != nil {
		t.Fatalf("decode tools/call response: %v", err)
	}
	var tr struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(resp.Result, &tr); err != nil {
		t.Fatalf("decode tools/call result: %v", err)
	}
	if !tr.IsError || len(tr.Content) == 0 || !strings.HasPrefix(tr.Content[0].Text, "["+codeInternal+"]") {
		t.Fatalf("want %s error for closed-store search, got %+v", codeInternal, tr)
	}

	inW.Close() // client-initiated shutdown → Run()이 stdin EOF로 반환
	if err := <-serveDone; err != nil {
		t.Fatalf("serve: %v", err)
	}
	os.Stdin, os.Stdout = oldIn, oldOut
	outW.Close()

	for sc.Scan() { // 종료 전후 잔여 바이트까지 전부 JSON 한 줄이어야 한다.
		if !json.Valid(sc.Bytes()) {
			t.Fatalf("trailing stdout polluted: %q", sc.Bytes())
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan trailing stdout: %v", err)
	}

	if !strings.Contains(stderrBuf.String(), "mcp: internal tool error") {
		t.Fatalf("want internal tool error logged to stderr during the call, got %q", stderrBuf.String())
	}
}

// TestWorktreeRootIsPathBasis: linked git worktree를 Canon{ProjectRoot:A, WorktreeRoot:B}로
// 흉내낸다(실제 git worktree 픽스처 불요) — B 하위 파일이 ctr_index에 성공하고(A 기준이면
// WORKSPACE_VIOLATION) source가 B-relative여야 한다(β2-1).
func TestWorktreeRootIsPathBasis(t *testing.T) {
	projCanon, err := ident.Canonicalize(t.TempDir())
	if err != nil {
		t.Fatalf("canonicalize project: %v", err)
	}
	wtDir := t.TempDir()
	wtCanon, err := ident.Canonicalize(wtDir)
	if err != nil {
		t.Fatalf("canonicalize worktree: %v", err)
	}
	canon := ident.Canon{ProjectRoot: projCanon.ProjectRoot, WorktreeRoot: wtCanon.WorktreeRoot}

	st, err := store.Open(t.TempDir(), false)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	defer st.Close()
	srv, err := NewServer(Config{Canon: canon, Store: st, Enable: []string{"ingest"}})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	srvT, cliT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, srvT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, cliT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	tmpFile := filepath.Join(wtDir, "note.txt")
	if err := os.WriteFile(tmpFile, []byte("needle content in worktree\n"), 0o644); err != nil {
		t.Fatalf("write tmp file: %v", err)
	}
	idxRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_index", Arguments: IndexInput{Path: tmpFile}})
	if err != nil {
		t.Fatalf("ctr_index call: %v", err)
	}
	if idxRes.IsError {
		t.Fatalf("ctr_index error (want success under WorktreeRoot): %+v", idxRes.Content)
	}

	searchRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_search", Arguments: SearchInput{Queries: []string{"needle"}}})
	if err != nil {
		t.Fatalf("ctr_search call: %v", err)
	}
	var searchOut SearchOutput
	remarshal(t, searchRes.StructuredContent, &searchOut)
	if len(searchOut.Results) != 1 || len(searchOut.Results[0].Hits) == 0 {
		t.Fatalf("no hits: %+v", searchOut.Results)
	}
	if source := searchOut.Results[0].Hits[0].Source; source != "note.txt" {
		t.Fatalf("source=%q want worktree-relative %q", source, "note.txt")
	}
}

// TestCtrIndexMissingPathIsNotFound: 존재하지 않는 path는 INTERNAL이 아니라 NOT_FOUND여야
// 한다(β2-3, toToolError의 fs.ErrNotExist 분기).
func TestCtrIndexMissingPathIsNotFound(t *testing.T) {
	cs, _ := newTestServer(t, []string{"ingest"})
	ctx := context.Background()
	missing := filepath.Join(t.TempDir(), "does-not-exist.txt")
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_index", Arguments: IndexInput{Path: missing}})
	if err != nil {
		t.Fatalf("ctr_index call: %v", err)
	}
	if !res.IsError {
		t.Fatalf("want IsError=true for missing path, got %+v", res)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.HasPrefix(text, "["+codeNotFound+"]") {
		t.Fatalf("want %s prefix, got %q", codeNotFound, text)
	}
}

// TestValidateIndexInput: path/content는 XOR이고 content 사용 시 title이 필수다(β2-4).
func TestValidateIndexInput(t *testing.T) {
	tests := []struct {
		name    string
		in      IndexInput
		wantErr bool
	}{
		{"neither", IndexInput{}, true},
		{"both_path_and_content", IndexInput{Path: "p", Content: "c", Title: "t"}, true},
		{"content_without_title", IndexInput{Content: "c"}, true},
		{"path_only", IndexInput{Path: "p"}, false},
		{"content_with_title", IndexInput{Content: "c", Title: "t"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateIndexInput(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateIndexInput(%+v) err=%v wantErr=%v", tt.in, err, tt.wantErr)
			}
			if err != nil && !strings.HasPrefix(err.Error(), "["+codeInvalidArgument+"]") {
				t.Fatalf("err=%v want %s prefix", err, codeInvalidArgument)
			}
		})
	}
}

// TestSourceCoordsExact: fetch의 source_coords_exact는 search와 같은 의미론이어야 한다(β2-2).
func TestSourceCoordsExact(t *testing.T) {
	base := store.RangeResult{Artifact: store.ArtifactMeta{Redaction: "none"}}

	inline := base
	inline.HasSource = true
	inline.Source = store.SourceInfo{Kind: "inline"}
	if !sourceCoordsExact(inline) {
		t.Fatalf("want true for inline no-redaction, got false: %+v", inline)
	}

	noSource := base
	noSource.HasSource = false
	if sourceCoordsExact(noSource) {
		t.Fatalf("want false when HasSource=false, got true: %+v", noSource)
	}
}

// TestRepresentationOf: source_kind==inline이면 media_type과 무관하게 "inline"이어야 한다(β2-5).
func TestRepresentationOf(t *testing.T) {
	if got := representationOf("text/plain", "inline"); got != "inline" {
		t.Fatalf("got=%q want inline", got)
	}
	if got := representationOf("text/markdown", "file"); got != "markdown" {
		t.Fatalf("got=%q want markdown", got)
	}
	if got := representationOf("text/plain", "file"); got != "file" {
		t.Fatalf("got=%q want file", got)
	}
}

// TestApplyFetchBudgetNewlineBoundary: 절단이 개행으로 정확히 끝나면 line_end가 다음 줄로
// 과계산되지 않아야 한다(β2-6).
func TestApplyFetchBudgetNewlineBoundary(t *testing.T) {
	res := store.RangeResult{
		Text: []byte("a\nb\nc\n"), ByteStart: 0, ByteEnd: 6,
		LineStart: 1, LineEnd: 3,
	}
	text, byteEnd, lineEnd, truncated := applyFetchBudget(res, 4) // cut="a\nb\n"
	if !truncated || string(text) != "a\nb\n" || byteEnd != 4 {
		t.Fatalf("text=%q byteEnd=%d truncated=%v", text, byteEnd, truncated)
	}
	if lineEnd != 2 {
		t.Fatalf("lineEnd=%d want 2 (개행 경계 과계산 회귀)", lineEnd)
	}
}

// TestCtrTransformRoundTrip: 색인(ingest) → artifact_id → ctr_transform이 저장된 텍스트
// 길이를 정확히 반환해야 한다(T3 TDD 항목 2). def 래핑(top-level for/재귀 비활성) 준수 스크립트.
func TestCtrTransformRoundTrip(t *testing.T) {
	skipDarwinNoIsolation(t)
	cs, canon := newTestServer(t, []string{"ingest"})
	ctx := context.Background()

	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tl := range lt.Tools {
		byName[tl.Name] = tl
	}
	if tl := byName["ctr_transform"]; tl == nil || tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
		t.Fatalf("ctr_transform readOnlyHint 누락: %+v", tl)
	}

	body := "needle content for transform round trip\n"
	tmpFile := filepath.Join(canon.ProjectRoot, "xform.txt")
	if err := os.WriteFile(tmpFile, []byte(body), 0o644); err != nil {
		t.Fatalf("write tmp file: %v", err)
	}
	idxRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_index", Arguments: IndexInput{Path: tmpFile}})
	if err != nil {
		t.Fatalf("ctr_index call: %v", err)
	}
	if idxRes.IsError {
		t.Fatalf("ctr_index error: %+v", idxRes.Content)
	}

	searchRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_search", Arguments: SearchInput{Queries: []string{"needle"}}})
	if err != nil {
		t.Fatalf("ctr_search call: %v", err)
	}
	var searchOut SearchOutput
	remarshal(t, searchRes.StructuredContent, &searchOut)
	if len(searchOut.Results) != 1 || len(searchOut.Results[0].Hits) == 0 {
		t.Fatalf("no hits: %+v", searchOut.Results)
	}
	artifactID := searchOut.Results[0].Hits[0].ArtifactID

	script := "def f():\n  emit(str(len(inputs[0].text())))\nf()\n"
	xRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_transform", Arguments: TransformInput{Script: script, Inputs: []int64{artifactID}}})
	if err != nil {
		t.Fatalf("ctr_transform call: %v", err)
	}
	if xRes.IsError {
		t.Fatalf("ctr_transform error: %+v", xRes.Content)
	}
	var xOut TransformOutput
	remarshal(t, xRes.StructuredContent, &xOut)
	want := fmt.Sprintf("%d", len(body))
	if xOut.Result != want {
		t.Fatalf("result=%q want %q (stored text length)", xOut.Result, want)
	}
}

// TestCtrTransformConfigTimeout: Config.TransformTimeout이 registerTransform 핸들러에
// context.WithTimeout으로 실제 적용되는지 확인한다(계획2 §4 이월 (1)).
// 결정성(Fix Round 2 — 재리뷰 잔존 1건): 이전 버전(50ms + "timeout 또는 budget 중 하나면
// 통과")은 registerTransform의 WithTimeout 배선이 통째로 제거돼도 잡지 못했다 — ctx가
// 무제한이면 Spawn 내부 안전망(defaultWorkerTimeout=10s)만 걸리는데, 이 스크립트(기본
// 5,000,000 step budget, 100M회 루프)는 budget이 수백 ms 내 소진되므로(worker_test.go의
// TestSpawn_Timeout이 budget보다 ctx timeout을 먼저 발동시키려 MaxSteps를 2조로 올려야
// 했던 것이 증거) 배선이 없어도 "budget 소진"으로 통과해버렸다.
// 그래서 TransformTimeout=1ns로 낮춘다 — WithTimeout 적용 직후 ctx는 사실상 이미
// 데드라인을 넘긴 상태이므로, Spawn이 스텝을 하나도 실행하기 전에 ctx-deadline 경로로
// 실패해야 한다(하드웨어 무관 결정론). 이 경로는 플랫폼/타이밍에 따라 코드 문자열이 셋 중
// 하나로 갈릴 수 있다(worker killed=INVALID_ARGUMENT / raw ctx.Err() / applyMemLimit이 그
// ctx.Err()를 감싸는 STORAGE_UNAVAILABLE) — 셋 다 codeBudgetExceeded는 아니므로 특정 코드
// 문자열을 고정하지 않고 "budget이 아님"만으로 배선 제거 회귀를 판별한다(Fix Round 3).
func TestCtrTransformConfigTimeout(t *testing.T) {
	skipDarwinNoIsolation(t)
	dir := t.TempDir()
	canon, err := ident.Canonicalize(dir)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	st, err := store.Open(t.TempDir(), false)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	srv, err := NewServer(Config{Canon: canon, Store: st, SelfExe: testSelfExe(t), TransformTimeout: time.Nanosecond})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	srvT, cliT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, srvT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, cliT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	start := time.Now()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_transform", Arguments: TransformInput{
		Script: "def f():\n\tfor i in range(100000000):\n\t\tpass\n\nf()\n",
	}})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatalf("want IsError=true for a ctx-already-expired transform call, got %+v", res)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	// 명시 배제: budget 소진이면 WithTimeout(1ns) 배선이 제거된 회귀다 — 위 주석의 세 합법
	// 경로(worker killed/raw ctx.Err()/STORAGE_UNAVAILABLE) 중 어느 것도 budget이 아니다.
	if strings.HasPrefix(text, "["+codeBudgetExceeded+"]") {
		t.Fatalf("got budget-exceeded — TransformTimeout(1ns) wiring이 제거된 것으로 의심됨: %q", text)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("elapsed=%v — want well under the 10s default", elapsed)
	}
}

// TestCtrTransformCapsMapping: budget/output_limit 초과 스크립트가 각각 BUDGET_EXCEEDED/
// OUTPUT_LIMIT_EXCEEDED로 매핑돼야 한다(T3 TDD 항목 3).
func TestCtrTransformCapsMapping(t *testing.T) {
	skipDarwinNoIsolation(t)
	cs, _ := newTestServer(t, nil)
	ctx := context.Background()

	budgetRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_transform", Arguments: TransformInput{
		Script: "def f():\n\tfor i in range(100000000):\n\t\tpass\n\nf()\n",
	}})
	if err != nil {
		t.Fatalf("budget call: %v", err)
	}
	if !budgetRes.IsError {
		t.Fatalf("want IsError=true for budget script, got %+v", budgetRes)
	}
	if text := budgetRes.Content[0].(*mcp.TextContent).Text; !strings.HasPrefix(text, "["+codeBudgetExceeded+"]") {
		t.Fatalf("want %s prefix, got %q", codeBudgetExceeded, text)
	}

	outRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_transform", Arguments: TransformInput{
		Script:         "def f():\n\tfor i in range(1000):\n\t\temit(\"x\")\n\nf()\n",
		MaxOutputBytes: 4,
	}})
	if err != nil {
		t.Fatalf("output_limit call: %v", err)
	}
	if !outRes.IsError {
		t.Fatalf("want IsError=true for output_limit script, got %+v", outRes)
	}
	if text := outRes.Content[0].(*mcp.TextContent).Text; !strings.HasPrefix(text, "["+codeOutputLimitExceeded+"]") {
		t.Fatalf("want %s prefix, got %q", codeOutputLimitExceeded, text)
	}
}

// TestCtrTransformInputValidation: inputs 9개(최대 8 초과)·script 64KB 초과는 각각
// INVALID_ARGUMENT여야 한다(T3 TDD 항목 4, 승계 (c)).
func TestCtrTransformInputValidation(t *testing.T) {
	skipDarwinNoIsolation(t)
	cs, _ := newTestServer(t, nil)
	ctx := context.Background()

	tooManyInputs := make([]int64, 9)
	for i := range tooManyInputs {
		tooManyInputs[i] = int64(i + 1)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_transform", Arguments: TransformInput{
		Script: "def f():\n  emit('x')\nf()\n", Inputs: tooManyInputs,
	}})
	if err != nil {
		t.Fatalf("9-inputs call: %v", err)
	}
	if !res.IsError {
		t.Fatalf("want IsError=true for 9 inputs, got %+v", res)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.HasPrefix(text, "["+codeInvalidArgument+"]") {
		t.Fatalf("want %s prefix, got %q", codeInvalidArgument, text)
	}

	res2, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_transform", Arguments: TransformInput{Script: strings.Repeat("a", 70000)}})
	if err != nil {
		t.Fatalf("big-script call: %v", err)
	}
	if !res2.IsError {
		t.Fatalf("want IsError=true for 64KB+ script, got %+v", res2)
	}
	if text := res2.Content[0].(*mcp.TextContent).Text; !strings.HasPrefix(text, "["+codeInvalidArgument+"]") {
		t.Fatalf("want %s prefix, got %q", codeInvalidArgument, text)
	}
}

// TestCtrTransformDescriptionMentionsDefWrapping: 도구 description에 def 래핑 제약이
// 명시돼야 한다(T1/T2 승계 (b) — 모르면 자연스러운 top-level for/while 스크립트가 실패한다).
func TestCtrTransformDescriptionMentionsDefWrapping(t *testing.T) {
	skipDarwinNoIsolation(t)
	cs, _ := newTestServer(t, nil)
	lt, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	for _, tl := range lt.Tools {
		if tl.Name != "ctr_transform" {
			continue
		}
		if !strings.Contains(tl.Description, "def f()") {
			t.Fatalf("description에 def 래핑 제약 누락: %q", tl.Description)
		}
		// 최종리뷰 C5(fable triage b): while/재귀는 def 안에서도 지원 안 됨을 정확히 명시.
		const wantPhrase = "최상위 for는 def 함수 안에서 사용. while·재귀는 starlark 기본 설정상 지원 안 됨(def 안에서도)."
		if !strings.Contains(tl.Description, wantPhrase) {
			t.Fatalf("description에 while/재귀 정정 문구 누락: %q", tl.Description)
		}
		return
	}
	t.Fatal("ctr_transform 도구 없음")
}

// --- ctr_fetch_and_index (설계 §4.5, T6) ---

// srvPort extracts srv.URL's port as int (httptest 서버는 임의 포트를 쓰므로 ExtraPorts에
// 필요 — internal/netfetch/netfetch_test.go의 동형 헬퍼를 패키지 경계상 재사용 불가해 복제).
func srvPort(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %s: %v", rawURL, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port of %s: %v", rawURL, err)
	}
	return p
}

// newNetTestServer: newTestServer과 동형이나 "net" 프로필+NetAllowLocal/NetPorts까지
// 구성하고, store와 저장 디렉터리(raw blob 파일 확인용)도 반환한다(T6 전용).
func newNetTestServer(t *testing.T, allowLocal bool, ports []int) (*mcp.ClientSession, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	canon, err := ident.Canonicalize(dir)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	storeDir := t.TempDir()
	st, err := store.Open(storeDir, false)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	srv, err := NewServer(Config{
		Canon: canon, Store: st, SelfExe: testSelfExe(t), Enable: []string{"net"},
		NetAllowLocal: allowLocal, NetPorts: ports,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	srvT, cliT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, srvT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, cliT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, st, storeDir
}

// secretCanary: AWS 키 형태 캐너리. 소스에 연속 토큰을 두지 않는다(규약 §8, push
// protection 재발 방지 — .superpowers/sdd/progress.md 2026-07-18 CANARY FIX) — 런타임
// 분할 리터럴 + 비실토큰 값으로 구성.
const secretCanary = "AKIA" + "NOTAREALKEY01234"

const fetchAndIndexTestPage = `<html><body><h1>Sample Doc</h1><p>needle unique marker text for search verification. token=` + secretCanary + `</p><script>var rawOnlyMarkerXYZ789 = 1;</script></body></html>`

// TestCtrFetchAndIndexRoundTrip: fetch_and_index→search 본문 hit(+SourceCoordsExact=false
// [이월 검증])·raw blob 파일 보존·FTS 미등록(스크립트 전용 마커는 검색으로 안 잡힘)을 확인한다.
func TestCtrFetchAndIndexRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(fetchAndIndexTestPage))
	}))
	defer srv.Close()

	cs, st, storeDir := newNetTestServer(t, true, []int{srvPort(t, srv.URL)})
	ctx := context.Background()

	fiRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_fetch_and_index", Arguments: FetchAndIndexInput{URL: srv.URL}})
	if err != nil {
		t.Fatalf("ctr_fetch_and_index call: %v", err)
	}
	if fiRes.IsError {
		t.Fatalf("ctr_fetch_and_index error: %+v", fiRes.Content)
	}
	var fiOut FetchAndIndexOutput
	remarshal(t, fiRes.StructuredContent, &fiOut)
	if fiOut.ArtifactID == 0 || fiOut.IndexedChunks == 0 || fiOut.ByteLength == 0 {
		t.Fatalf("bad fetch_and_index output: %+v", fiOut)
	}
	if fiOut.Extraction == "" {
		t.Fatalf("extraction empty: %+v", fiOut)
	}
	if len(fiOut.Snippet) == 0 || len(fiOut.Snippet) > 1024 {
		t.Fatalf("bad snippet length=%d", len(fiOut.Snippet))
	}
	// 최종리뷰 C1(수렴 Critical): snippet은 저장본(redacted) 기준이어야 한다 — netfetch 원문
	// 기준이면 redaction 이전 secret이 응답 1KB로 그대로 유출된다.
	if strings.Contains(fiOut.Snippet, secretCanary) {
		t.Fatalf("snippet leaks secret canary (redaction bypass): %q", fiOut.Snippet)
	}
	// 최종리뷰 C4(fable Imp5): 원격 웹 콘텐츠 반환 도구는 SearchOutput/FetchOutput과 일관되게
	// untrusted 마커를 달아야 한다(프롬프트 주입 표면 표시).
	if !fiOut.Untrusted {
		t.Fatalf("untrusted flag missing: %+v", fiOut)
	}

	searchRes, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_search", Arguments: SearchInput{Queries: []string{"needle"}}})
	if err != nil {
		t.Fatalf("ctr_search call: %v", err)
	}
	var searchOut SearchOutput
	remarshal(t, searchRes.StructuredContent, &searchOut)
	if len(searchOut.Results) != 1 || len(searchOut.Results[0].Hits) == 0 {
		t.Fatalf("no hits for body text: %+v", searchOut.Results)
	}
	if searchOut.Results[0].Hits[0].SourceCoordsExact {
		t.Fatalf("want SourceCoordsExact=false for web source: %+v", searchOut.Results[0].Hits[0])
	}

	var rawBlobHash string
	if err := st.Reader().QueryRow(`SELECT raw_blob_hash FROM sources WHERE artifact_id=?`, fiOut.ArtifactID).Scan(&rawBlobHash); err != nil {
		t.Fatalf("query raw_blob_hash: %v", err)
	}
	if rawBlobHash == "" {
		t.Fatalf("raw_blob_hash empty")
	}
	blobPath := filepath.Join(storeDir, "artifacts", rawBlobHash[:2], rawBlobHash)
	raw, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("read raw blob file: %v", err)
	}
	if string(raw) != fetchAndIndexTestPage {
		t.Fatalf("raw blob content mismatch: %q", raw)
	}

	searchRes2, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_search", Arguments: SearchInput{Queries: []string{"rawOnlyMarkerXYZ789"}}})
	if err != nil {
		t.Fatalf("ctr_search(script marker) call: %v", err)
	}
	var searchOut2 SearchOutput
	remarshal(t, searchRes2.StructuredContent, &searchOut2)
	if len(searchOut2.Results) != 1 || len(searchOut2.Results[0].Hits) != 0 {
		t.Fatalf("script-only marker unexpectedly indexed: %+v", searchOut2.Results)
	}

	// secret 캐너리는 redaction으로 저장 단계에서 사라지므로 search/fetch 어디서도 안 나와야
	// 한다(기존 보장 유지 확인 — snippet 쪽 유출만 이번 회귀의 신규 지점이었다).
	searchRes3, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_search", Arguments: SearchInput{Queries: []string{secretCanary}}})
	if err != nil {
		t.Fatalf("ctr_search(canary) call: %v", err)
	}
	var searchOut3 SearchOutput
	remarshal(t, searchRes3.StructuredContent, &searchOut3)
	if len(searchOut3.Results) != 1 || len(searchOut3.Results[0].Hits) != 0 {
		t.Fatalf("secret canary unexpectedly searchable: %+v", searchOut3.Results)
	}

	var bs, be int64 = 0, fiOut.ByteLength
	fetchRes3, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_fetch", Arguments: FetchInput{ArtifactID: fiOut.ArtifactID, ByteStart: &bs, ByteEnd: &be}})
	if err != nil {
		t.Fatalf("ctr_fetch(full) call: %v", err)
	}
	if fetchRes3.IsError {
		t.Fatalf("ctr_fetch(full) error: %+v", fetchRes3.Content)
	}
	var fetchOut3 FetchOutput
	remarshal(t, fetchRes3.StructuredContent, &fetchOut3)
	if strings.Contains(fetchOut3.Content, secretCanary) {
		t.Fatalf("fetch leaks secret canary: %q", fetchOut3.Content)
	}
	// 이월 검증(계획2 §4 (7)): 웹 경로(extraction!="")로 색인한 artifact는 ctr_fetch에서도
	// source_coords_exact=false여야 한다(mcp.go sourceCoordsExact 분기 직접 실증).
	if fetchOut3.SourceCoordsExact {
		t.Fatalf("want SourceCoordsExact=false for web source (extraction=%q): %+v", fiOut.Extraction, fetchOut3)
	}
}

// --- ctr_global_search (설계 §4.6/§5.4, Task 2) ---

// newGlobalTestProject: srcDir에 content를 담은 파일 1개를 만들어 실제 store에 ingest.Run으로
// 색인한 뒤 store를 닫고 read-only로 재오픈해 GlobalProject를 만든다 — global-search는
// 항상 read-only 연결만 쓰므로(설계 §5.4 query_only=ON) 색인은 별도 writable 오픈으로 선행한다.
func newGlobalTestProject(t *testing.T, id, content string) GlobalProject {
	t.Helper()
	dir := t.TempDir()
	writeSt, err := store.Open(dir, false)
	if err != nil {
		t.Fatalf("store open (write): %v", err)
	}
	srcDir := t.TempDir()
	file := filepath.Join(srcDir, "note.txt")
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	// ingest.Run은 projectRoot가 이미 canonical(Abs+EvalSymlinks)함을 계약으로 삼는다
	// (§2.1 인가 루트 고정, Codex 교차리뷰 P1-2 — newTestServer는 ident.Canonicalize를
	// 거쳐 이미 이 계약을 지키지만, 여기는 raw t.TempDir()를 직접 넘기므로 별도 해석 필요).
	canonSrcDir, err := filepath.EvalSymlinks(srcDir)
	if err != nil {
		t.Fatalf("realpath srcDir: %v", err)
	}
	if _, err := ingest.Run(context.Background(), writeSt, canonSrcDir, nil, ingest.Request{Path: file}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	writeSt.Close()

	roSt, err := store.Open(dir, true)
	if err != nil {
		t.Fatalf("store open (ro): %v", err)
	}
	t.Cleanup(func() { roSt.Close() })
	return GlobalProject{ID: id, Root: srcDir, Store: roSt}
}

// TestGlobalSearch_MergesAcrossProjects: 서로 다른 두 프로젝트 store의 hit이 project 라벨과
// 함께 score 내림차순으로 병합되고, tools/list에 ctr_global_search 하나만 노출되는지(설계
// §4.6 금지 조항) 검증한다.
func TestGlobalSearch_MergesAcrossProjects(t *testing.T) {
	ctx := context.Background()
	p1 := newGlobalTestProject(t, "proj-one", "needle content in project one\n")
	p2 := newGlobalTestProject(t, "proj-two", "needle content in project two\n")

	srv, err := NewGlobalServer(GlobalConfig{Projects: []GlobalProject{p1, p2}})
	if err != nil {
		t.Fatalf("new global server: %v", err)
	}
	srvT, cliT := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, srvT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, cliT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(lt.Tools) != 1 || lt.Tools[0].Name != "ctr_global_search" {
		t.Fatalf("tools/list=%v want exactly [ctr_global_search] (설계 §4.6 금지 조항)", lt.Tools)
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_global_search", Arguments: SearchInput{Queries: []string{"needle"}}})
	if err != nil {
		t.Fatalf("ctr_global_search call: %v", err)
	}
	if res.IsError {
		t.Fatalf("ctr_global_search error: %+v", res.Content)
	}
	var out GlobalSearchOutput
	remarshal(t, res.StructuredContent, &out)
	if !out.Untrusted {
		t.Fatalf("untrusted flag missing: %+v", out)
	}
	if len(out.Results) != 1 {
		t.Fatalf("results=%d want 1", len(out.Results))
	}
	hits := out.Results[0].Hits
	if len(hits) != 2 {
		t.Fatalf("hits=%d want 2 (one per project): %+v", len(hits), hits)
	}
	seen := map[string]bool{}
	for i, h := range hits {
		seen[h.Project] = true
		if i > 0 && hits[i-1].Score < h.Score {
			t.Fatalf("hits not score-descending: %+v", hits)
		}
	}
	if !seen["proj-one"] || !seen["proj-two"] {
		t.Fatalf("missing project labels: %+v", hits)
	}
	if out.Results[0].Truncated {
		t.Fatalf("want Truncated=false with default limit, got true: %+v", out.Results[0])
	}

	// limit=1: 두 프로젝트가 각각 1 hit씩 내도 병합 후 1개로 절단되고 truncated=true여야 한다.
	res2, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_global_search", Arguments: SearchInput{Queries: []string{"needle"}, Limit: 1}})
	if err != nil {
		t.Fatalf("ctr_global_search(limit=1) call: %v", err)
	}
	var out2 GlobalSearchOutput
	remarshal(t, res2.StructuredContent, &out2)
	if len(out2.Results) != 1 || len(out2.Results[0].Hits) != 1 {
		t.Fatalf("limit=1 hits=%+v want exactly 1", out2.Results)
	}
	if !out2.Results[0].Truncated {
		t.Fatalf("want Truncated=true after merge cut to limit=1: %+v", out2.Results[0])
	}
}

// TestNewGlobalServerEmptyProjectsErrors: Projects가 비면 시작 자체를 거부해야 한다(설계
// §4.6 계약 — allowlist 미지정 시 시작 거부와 동일 취지의 방어선).
func TestNewGlobalServerEmptyProjectsErrors(t *testing.T) {
	if _, err := NewGlobalServer(GlobalConfig{}); err == nil {
		t.Fatal("want error for empty Projects, got nil")
	}
}

// TestGlobalSearchRejectsScope — 최종리뷰 E1(fable): 공유 SearchInput의 scope는 global-search가
// 지원하지 않으므로 조용히 무시하지 않고 INVALID_ARGUMENT로 거부한다.
func TestGlobalSearchRejectsScope(t *testing.T) {
	ctx := context.Background()
	p := newGlobalTestProject(t, "proj-scope", "needle content\n")
	srv, err := NewGlobalServer(GlobalConfig{Projects: []GlobalProject{p}})
	if err != nil {
		t.Fatalf("new global server: %v", err)
	}
	srvT, cliT := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, srvT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, cliT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ctr_global_search",
		Arguments: SearchInput{Queries: []string{"needle"}, Scope: "events"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatalf("want IsError=true for scope on global-search, got %+v", res.StructuredContent)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.HasPrefix(text, "["+codeInvalidArgument+"]") {
		t.Fatalf("want %s prefix, got %q", codeInvalidArgument, text)
	}
}

// TestCtrFetchAndIndexDenied: AllowLocal=false에서 사설/루프백 목적지는 NETWORK_DENIED.
func TestCtrFetchAndIndexDenied(t *testing.T) {
	cs, _, _ := newNetTestServer(t, false, nil)
	ctx := context.Background()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_fetch_and_index", Arguments: FetchAndIndexInput{URL: "http://127.0.0.1:1/"}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatalf("want IsError=true for denied local address, got %+v", res)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.HasPrefix(text, "["+codeNetworkDenied+"]") {
		t.Fatalf("want %s prefix, got %q", codeNetworkDenied, text)
	}
}

// --- ctr_search scope 확장 + ctr_fetch 문구 (태스크 7, 설계 §3.4·§3.5) ---

// newSearchScopeTestServer: newTestServer(ingest)와 newRecordEventTestServer를 합친 형태 —
// content 색인(ctr_index)과 세션 이벤트(sess.Append)를 같은 서버에서 함께 검증해야 하는
// scope=all 테스트를 위해 별도로 둔다(기존 헬퍼 시그니처 변경은 다른 태스크의 호출부를 건드림).
func newSearchScopeTestServer(t *testing.T) (cs *mcp.ClientSession, canon ident.Canon, sess *session.DB) {
	t.Helper()
	var err error
	canon, err = ident.Canonicalize(t.TempDir())
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	st, err := store.Open(t.TempDir(), false)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	sess, err = session.Open(t.TempDir(), session.Options{Producer: "test/search-scope"})
	if err != nil {
		t.Fatalf("session open: %v", err)
	}
	t.Cleanup(func() { sess.Close() })

	srv, err := NewServer(Config{Canon: canon, Store: st, SelfExe: testSelfExe(t), Enable: []string{"ingest"}, Session: sess})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	srvT, cliT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, srvT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err = client.Connect(ctx, cliT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, canon, sess
}

// indexNeedle: canon.ProjectRoot 아래 파일 1개를 needle 텍스트로 써서 ctr_index로 색인한다
// (scope 테스트들의 공용 content 시드 헬퍼).
func indexNeedle(t *testing.T, cs *mcp.ClientSession, canon ident.Canon, needle string) {
	t.Helper()
	tmpFile := filepath.Join(canon.ProjectRoot, "note.txt")
	if err := os.WriteFile(tmpFile, []byte(needle+" content in file\n"), 0o644); err != nil {
		t.Fatalf("write tmp file: %v", err)
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "ctr_index", Arguments: IndexInput{Path: tmpFile}})
	if err != nil {
		t.Fatalf("ctr_index call: %v", err)
	}
	if res.IsError {
		t.Fatalf("ctr_index error: %+v", res.Content)
	}
}

// TestSearchScopeDefaultContent — 브리프 Step1 ①: scope 생략 시 기존 content-only 동작과
// 동일(이벤트 섹션 비어 있음) — 기존 호출 무변 계약의 명시적 회귀 케이스.
func TestSearchScopeDefaultContent(t *testing.T) {
	cs, canon, _ := newSearchScopeTestServer(t)
	indexNeedle(t, cs, canon, "needle")

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "ctr_search", Arguments: SearchInput{Queries: []string{"needle"}}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("search error: %+v", res.Content)
	}
	var out SearchOutput
	remarshal(t, res.StructuredContent, &out)
	if len(out.Results) != 1 || len(out.Results[0].Hits) == 0 {
		t.Fatalf("no content hits: %+v", out.Results)
	}
	if len(out.Results[0].Events) != 0 {
		t.Fatalf("events want empty for default scope, got %+v", out.Results[0].Events)
	}
}

// TestSearchScopeEvents — 브리프 Step1 ②: scope=events는 content hits 없이 EventHit만
// 반환하고, 교정된(superseded) 이벤트도 포함하되 플래그로 구분한다(§2.3 색인 미제거 대칭).
func TestSearchScopeEvents(t *testing.T) {
	cs, _, sess := newSearchScopeTestServer(t)
	origID := mustAppend(t, sess, session.Event{Type: "decision", Summary: "adopt widgetfoo approach"})
	newID := mustAppend(t, sess, session.Event{Type: "decision", Summary: "revise widgetfoo approach", Supersedes: origID})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ctr_search",
		Arguments: SearchInput{Queries: []string{"widgetfoo"}, Scope: "events"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("search error: %+v", res.Content)
	}
	var out SearchOutput
	remarshal(t, res.StructuredContent, &out)
	if len(out.Results) != 1 || len(out.Results[0].Hits) != 0 {
		t.Fatalf("results=%+v want 1 result with empty content hits", out.Results)
	}
	events := out.Results[0].Events
	if len(events) != 2 {
		t.Fatalf("events=%+v want 2", events)
	}
	var sawOrig, sawNew bool
	for _, e := range events {
		if e.SessionID != sess.SessionID() || e.EventType != "decision" {
			t.Fatalf("event fields=%+v", e)
		}
		switch e.EventID {
		case origID:
			sawOrig = true
			if !e.Superseded {
				t.Fatalf("orig event want superseded=true: %+v", e)
			}
		case newID:
			sawNew = true
			if e.Superseded {
				t.Fatalf("new event want superseded=false: %+v", e)
			}
		}
	}
	if !sawOrig || !sawNew {
		t.Fatalf("want both orig+new event, got %+v", events)
	}
}

// TestSearchScopeEventsBudget — 최종리뷰 C3(Codex P2): scope=events에서 max_return_bytes가
// 이벤트 섹션에도 적용된다 — 예산을 작게 잡으면 일부 이벤트만 실리고 truncated=true로 신호한다
// (이전엔 이벤트 섹션이 예산 무시로 무제한이었다).
func TestSearchScopeEventsBudget(t *testing.T) {
	cs, _, sess := newSearchScopeTestServer(t)
	const n = 5
	summary := "budgettoken " + strings.Repeat("x", 200) // 212B
	for i := 0; i < n; i++ {
		mustAppend(t, sess, session.Event{Type: "note", Summary: summary})
	}

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ctr_search",
		Arguments: SearchInput{Queries: []string{"budgettoken"}, Scope: "events", Limit: n, MaxReturnBytes: 300},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("search error: %+v", res.Content)
	}
	var out SearchOutput
	remarshal(t, res.StructuredContent, &out)
	events := out.Results[0].Events
	if len(events) == 0 || len(events) >= n {
		t.Fatalf("events len=%d want 0<n<%d (예산 절단)", len(events), n)
	}
	if !out.Results[0].Truncated {
		t.Fatalf("Truncated=false want true(이벤트 예산 초과 절단): %+v", out.Results[0])
	}
}

// TestSearchScopeAll — 브리프 Step1 ③: scope=all은 같은 질의 결과에 content hits와 이벤트
// 섹션이 동시에 실린다.
func TestSearchScopeAll(t *testing.T) {
	cs, canon, sess := newSearchScopeTestServer(t)
	indexNeedle(t, cs, canon, "gizmoqux")
	mustAppend(t, sess, session.Event{Type: "note", Summary: "gizmoqux discussion recap"})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ctr_search",
		Arguments: SearchInput{Queries: []string{"gizmoqux"}, Scope: "all"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("search error: %+v", res.Content)
	}
	var out SearchOutput
	remarshal(t, res.StructuredContent, &out)
	if len(out.Results) != 1 {
		t.Fatalf("results=%+v want 1", out.Results)
	}
	if len(out.Results[0].Hits) == 0 {
		t.Fatalf("want content hits in scope=all, got none: %+v", out.Results[0])
	}
	if len(out.Results[0].Events) == 0 {
		t.Fatalf("want events in scope=all, got none: %+v", out.Results[0])
	}
}

// TestSearchScopeRequiresSessionForEventsAndAll — 브리프 Step1 ④: session.db 불용(T10 배선
// 전이므로 Session=nil 주입)에서 events/all은 조용한 빈 결과가 아니라 STORAGE_UNAVAILABLE로
// 실패해야 한다(설계 §3.4). content scope는 세션과 무관하게 정상이어야 하므로 함께 확인한다.
func TestSearchScopeRequiresSessionForEventsAndAll(t *testing.T) {
	cs, _ := newTestServer(t, nil) // Session=nil(base profile)
	ctx := context.Background()

	for _, scope := range []string{"events", "all"} {
		t.Run(scope, func(t *testing.T) {
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{
				Name:      "ctr_search",
				Arguments: SearchInput{Queries: []string{"anything"}, Scope: scope},
			})
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if !res.IsError {
				t.Fatalf("want IsError=true for scope=%s without session, got %+v", scope, res)
			}
			text := res.Content[0].(*mcp.TextContent).Text
			if !strings.HasPrefix(text, "["+codeStorageUnavailable+"]") {
				t.Fatalf("want %s prefix, got %q", codeStorageUnavailable, text)
			}
		})
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_search", Arguments: SearchInput{Queries: []string{"anything"}}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("content scope without session should still succeed, got error: %+v", res.Content)
	}
}

// TestSearchScopeInvalidValue: 미지의 scope 값은 신규 코드 없이 기존 INVALID_ARGUMENT로 거부.
func TestSearchScopeInvalidValue(t *testing.T) {
	cs, _ := newTestServer(t, nil)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "ctr_search", Arguments: SearchInput{Queries: []string{"x"}, Scope: "bogus"}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatalf("want IsError=true for invalid scope")
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.HasPrefix(text, "["+codeInvalidArgument+"]") {
		t.Fatalf("want %s prefix, got %q", codeInvalidArgument, text)
	}
}

// TestFetchDescriptionMentionsByteExactNotWebFetch — 브리프 Step1 ⑤: ctr_fetch 설명에
// "byte-exact"·"웹 fetch"·"ctr_fetch_and_index" 문구가 있는지 확인한다(설계 §3.5).
func TestFetchDescriptionMentionsByteExactNotWebFetch(t *testing.T) {
	cs, _ := newTestServer(t, nil)
	lt, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var desc string
	for _, tl := range lt.Tools {
		if tl.Name == "ctr_fetch" {
			desc = tl.Description
		}
	}
	if desc == "" {
		t.Fatalf("ctr_fetch not found in tools/list")
	}
	for _, want := range []string{"byte-exact", "웹 fetch", "ctr_fetch_and_index"} {
		if !strings.Contains(desc, want) {
			t.Fatalf("ctr_fetch description=%q want to contain %q", desc, want)
		}
	}
}

// TestRecordEventSchemaAttributesFloat64Caveat — 부채정리 ③: ctr_record_event 설명에 JSON
// 숫자가 float64로 디코딩돼 큰 정수는 정밀도를 잃는다는 attributes 캐비앗이 있어야 한다.
func TestRecordEventSchemaAttributesFloat64Caveat(t *testing.T) {
	cs, _, _, _ := newRecordEventTestServer(t)
	lt, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var desc string
	for _, tl := range lt.Tools {
		if tl.Name == "ctr_record_event" {
			desc = tl.Description
		}
	}
	if desc == "" {
		t.Fatal("ctr_record_event not found in tools/list")
	}
	if !strings.Contains(desc, "float64") {
		t.Fatalf("ctr_record_event description missing attributes float64 caveat: %q", desc)
	}
}

// --- ctr_record_event (태스크 3, 설계 §3.1) ---

// newRecordEventTestServer: newTestServer와 동형이지만 Session DB까지 배선해 ctr_record_event를
// 기본 표면에 등록한다. storeDir을 함께 반환한다(LedgerStats(dir) 재조회용, ⑥).
func newRecordEventTestServer(t *testing.T, enable ...string) (cs *mcp.ClientSession, st *store.Store, sess *session.DB, storeDir string) {
	t.Helper()
	canon, err := ident.Canonicalize(t.TempDir())
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	storeDir = t.TempDir()
	st, err = store.Open(storeDir, false)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	sess, err = session.Open(t.TempDir(), session.Options{Producer: "test/record-event"})
	if err != nil {
		t.Fatalf("session open: %v", err)
	}
	t.Cleanup(func() { sess.Close() })

	// ScratchRoot: enable에 "exec"가 실리면 registerExecute가 등록되므로 sandbox 부모가 필요하다
	// (TestSchemaTokenBudget 10-도구 재기준화 경로). 다른 호출자는 enable 비어 6-도구 그대로.
	srv, err := NewServer(Config{Canon: canon, Store: st, SelfExe: testSelfExe(t), ScratchRoot: t.TempDir(), Session: sess, Enable: enable})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	srvT, cliT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, srvT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err = client.Connect(ctx, cliT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, st, sess, storeDir
}

// recordEventErrPrefix: ctr_record_event 호출이 IsError=true이고 원하는 코드 prefix를 갖는지
// 확인한다(반복되는 오류 케이스 어서션 공용 헬퍼).
func recordEventErrPrefix(t *testing.T, cs *mcp.ClientSession, in RecordEventInput, wantCode string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "ctr_record_event", Arguments: in})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatalf("want IsError=true, got %+v", res)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.HasPrefix(text, "["+wantCode+"]") {
		t.Fatalf("want %s prefix, got %q", wantCode, text)
	}
}

// mapAttrsOfSize: {"k":"aaa..."} 형태로 마샬링 시 정확히 n바이트가 되는 attributes map을
// 만든다(n>=8, 단일 키 "k" — json.Marshal(map[string]any)의 결정적 출력에 의존).
func mapAttrsOfSize(n int) map[string]any {
	const overhead = len(`{"k":""}`)
	return map[string]any{"k": strings.Repeat("a", n-overhead)}
}

// TestRecordEventRoundTrip: 브리프 Step1 ① — record → {event_id, session_id, ts} 반환 → Reader
// 직조회로 행 검증.
func TestRecordEventRoundTrip(t *testing.T) {
	cs, _, sess, _ := newRecordEventTestServer(t)
	ctx := context.Background()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_record_event", Arguments: RecordEventInput{
		EventType: "decision", Summary: "chose approach A",
	}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("record_event error: %+v", res.Content)
	}
	var out RecordEventOutput
	remarshal(t, res.StructuredContent, &out)
	if out.EventID == "" || out.SessionID != sess.SessionID() || out.Ts <= 0 {
		t.Fatalf("out=%+v want non-empty event_id, session_id=%q, ts>0", out, sess.SessionID())
	}

	var gotSummary, gotType, gotRedaction string
	if err := sess.Reader().QueryRow("SELECT summary, event_type, redaction FROM session_events WHERE event_id=?", out.EventID).
		Scan(&gotSummary, &gotType, &gotRedaction); err != nil {
		t.Fatalf("row query: %v", err)
	}
	if gotSummary != "chose approach A" || gotType != "decision" || gotRedaction != "none" {
		t.Fatalf("row=(%q,%q,%q) want (chose approach A, decision, none)", gotSummary, gotType, gotRedaction)
	}
}

// TestRecordEventCapViolations: 브리프 Step1 ② — 상한 위반 각 1건(type 65B·summary 2049B·
// attributes 4097B·refs 17개) → INVALID_ARGUMENT. 개별 필드는 상한 이내인데 총합만 8KB를
// 넘는 경우도 별도로 검증한다.
func TestRecordEventCapViolations(t *testing.T) {
	cs, st, _, _ := newRecordEventTestServer(t)

	tests := []struct {
		name string
		in   RecordEventInput
	}{
		{"event_type_65B", RecordEventInput{EventType: strings.Repeat("a", 65), Summary: "s"}},
		{"summary_2049B", RecordEventInput{EventType: "note", Summary: strings.Repeat("s", 2049)}},
		{"attributes_4097B", RecordEventInput{EventType: "note", Summary: "s", Attributes: mapAttrsOfSize(4097)}},
		{"refs_17", RecordEventInput{EventType: "note", Summary: "s", ArtifactRefs: make([]int64, 17)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recordEventErrPrefix(t, cs, tt.in, codeInvalidArgument)
		})
	}

	t.Run("total_8KB", func(t *testing.T) {
		related := make([]string, 6)
		for i := range related {
			related[i] = "https://example.com/" + strings.Repeat("a", 512-len("https://example.com/"))
		}
		in := RecordEventInput{
			EventType:        "note",
			Summary:          strings.Repeat("s", maxSummaryBytes),
			Attributes:       mapAttrsOfSize(4000),
			RelatedResources: related,
		}
		recordEventErrPrefix(t, cs, in, codeInvalidArgument)
	})

	// C5(T3): refs 바이트 가산(session.ValidateEvent)이 실제로 계상되는지 게이트한다. 소계 7000B는
	// refs 없이 통과(≤8192)하지만, 실재 artifact로 해석된 URI 16개(각 ≥83B)를 더하면 8192B를 넘어
	// 거부돼야 한다 — refs 가산만이 판정을 뒤집는다. (refs는 바이트 합산 전에 resolve되므로 미등록
	// store의 id 0 refs로는 합산 전 거부돼 가산을 행사하지 못한다 → 반드시 실재 id를 등록해 쓴다.)
	t.Run("total_8KB_with_artifact_uris", func(t *testing.T) {
		id, err := st.Register(context.Background(), store.Registration{
			StoredBytes: []byte("ref body"), MediaType: "text/plain",
			Source: store.SourceMeta{URI: "/ref.txt", Kind: "file", SrcHash: "refh"},
		})
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		refs := make([]int64, maxRefsOrRelated) // 동일 실재 id 16개 — URI 길이 동일, 16×≥83B 가산
		for i := range refs {
			refs[i] = id
		}
		item := "https://example.com/" + strings.Repeat("a", 474-len("https://example.com/")) // 474B×2 = 948
		base := RecordEventInput{
			EventType:        "note",                    // 4
			Summary:          strings.Repeat("s", 2048), // 2048
			Attributes:       mapAttrsOfSize(4000),      // 4000
			RelatedResources: []string{item, item},      // 948 → 소계 7000(≤8192)
		}
		// refs 없이는 통과 — 소계가 상한 이내임을 고정(without-refs sibling).
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "ctr_record_event", Arguments: base})
		if err != nil {
			t.Fatalf("call(no refs): %v", err)
		}
		if res.IsError {
			t.Fatalf("refs 없이 거부됨(소계 7000B는 통과해야 함): %+v", res.Content)
		}
		// refs 16개를 더하면 초과 거부 — 가산 회귀(session.go refs 합산 루프 삭제)를 정확히 잡는다.
		withRefs := base
		withRefs.ArtifactRefs = refs
		recordEventErrPrefix(t, cs, withRefs, codeInvalidArgument)
	})
}

// TestRecordEventSecretCanaryRedacted: 브리프 Step1 ③(G4 기록 경로) — summary·attributes·
// related_resources에 분할 리터럴 canary → 저장 행에 원문 부재 + redaction='spans'.
func TestRecordEventSecretCanaryRedacted(t *testing.T) {
	cs, _, sess, _ := newRecordEventTestServer(t)
	ctx := context.Background()

	// 런타임 분할 리터럴 — 소스에 연속 secret 토큰 금지(규약 §8).
	canary := "xox" + "b-1234567890ABCDEF"

	in := RecordEventInput{
		EventType:        "note",
		Summary:          "leaked: " + canary,
		Attributes:       map[string]any{"token": canary},
		RelatedResources: []string{"https://example.com/?token=" + canary},
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_record_event", Arguments: in})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("record_event error: %+v", res.Content)
	}
	var out RecordEventOutput
	remarshal(t, res.StructuredContent, &out)

	var summary, payload, related, redaction string
	if err := sess.Reader().QueryRow("SELECT summary, payload, related, redaction FROM session_events WHERE event_id=?", out.EventID).
		Scan(&summary, &payload, &related, &redaction); err != nil {
		t.Fatalf("row query: %v", err)
	}
	if strings.Contains(summary, canary) || strings.Contains(payload, canary) || strings.Contains(related, canary) {
		t.Fatalf("canary leaked: summary=%q payload=%q related=%q", summary, payload, related)
	}
	if redaction != "spans" {
		t.Fatalf("redaction=%q want spans", redaction)
	}
	if !strings.Contains(summary, "REDACTED") || !strings.Contains(payload, "REDACTED") || !strings.Contains(related, "REDACTED") {
		t.Fatalf("redaction marker missing: summary=%q payload=%q related=%q", summary, payload, related)
	}
}

// TestRecordEventArtifactRefs: 브리프 Step1 ④ — 유효 id는 정본 URI(artifact://<session_id>/
// sha256-<hash>)로 저장되고, 미존재 id는 INVALID_ARGUMENT.
func TestRecordEventArtifactRefs(t *testing.T) {
	cs, st, sess, _ := newRecordEventTestServer(t)
	ctx := context.Background()

	id, err := st.Register(ctx, store.Registration{
		StoredBytes: []byte("artifact body"), MediaType: "text/plain",
		Source: store.SourceMeta{URI: "/a.txt", Kind: "file", SrcHash: "h1"},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	hash, err := st.ArtifactHashByID(ctx, id)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	wantURI := "artifact://" + sess.SessionID() + "/sha256-" + hash

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_record_event", Arguments: RecordEventInput{
		EventType: "note", Summary: "s", ArtifactRefs: []int64{id},
	}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("record_event error: %+v", res.Content)
	}
	var out RecordEventOutput
	remarshal(t, res.StructuredContent, &out)

	var refsJSON string
	if err := sess.Reader().QueryRow("SELECT artifact_refs FROM session_events WHERE event_id=?", out.EventID).Scan(&refsJSON); err != nil {
		t.Fatalf("row query: %v", err)
	}
	var refs []string
	if err := json.Unmarshal([]byte(refsJSON), &refs); err != nil {
		t.Fatalf("unmarshal artifact_refs: %v", err)
	}
	if len(refs) != 1 || refs[0] != wantURI {
		t.Fatalf("artifact_refs=%v want [%q]", refs, wantURI)
	}

	recordEventErrPrefix(t, cs, RecordEventInput{EventType: "note", Summary: "s", ArtifactRefs: []int64{999999}}, codeInvalidArgument)
}

// TestRecordEventSupersedesMissingIsInvalidArgument: 최종리뷰 C1(설계 §3.1 명문) — supersedes
// 미존재는 INVALID_ARGUMENT(artifact_refs 미존재와 대칭). 이전 구현은 NOT_FOUND였으나 설계서
// 우선 규칙으로 교정(플랜 T3 문면 교정 필요).
func TestRecordEventSupersedesMissingIsInvalidArgument(t *testing.T) {
	cs, _, _, _ := newRecordEventTestServer(t)
	recordEventErrPrefix(t, cs, RecordEventInput{
		EventType: "decision", Summary: "corrected", Supersedes: "00000000-0000-7000-8000-000000000000",
	}, codeInvalidArgument)
}

// TestRecordEventSessionAbsentIsNotFound: 최종리뷰 I1 — Task 4b의 append 존재 게이트로 세션
// 부재 Append도 store.ErrNotFound를 낸다. supersedes를 지정하지 않은 호출은 supersedes 매핑
// (INVALID_ARGUMENT)이 아니라 NOT_FOUND로 흘러야 한다 — 세션 부재를 supersedes 오류로
// 오표기하지 않는다.
func TestRecordEventSessionAbsentIsNotFound(t *testing.T) {
	cs, _, sess, _ := newRecordEventTestServer(t)
	// 현재(빈) 세션 행을 공개 API로 제거 — Sweep의 빈-세션 GC(started_at<now-7d)를 미래 now로
	// 트리거한다(raw SQL 없이 세션 부재를 재현). session_start만 있는 세션은 빈 세션으로 수거된다.
	rep, err := session.Sweep(context.Background(), sess, time.Now().Add(100*24*time.Hour))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if rep.EmptySessionsDeleted != 1 {
		t.Fatalf("EmptySessionsDeleted=%d want 1(세션 부재 재현 실패)", rep.EmptySessionsDeleted)
	}
	// supersedes 미지정 + 세션 부재 → NOT_FOUND(과거엔 supersedes INVALID_ARGUMENT로 오표기).
	recordEventErrPrefix(t, cs, RecordEventInput{EventType: "note", Summary: "s"}, codeNotFound)
}

// TestRecordEventLedgerAppend: 브리프 Step1 ⑥ — 호출마다 LedgerAppend(ctr_fetch/ctr_search
// 패턴 승계) → ledger.db에 1행.
func TestRecordEventLedgerAppend(t *testing.T) {
	cs, _, _, storeDir := newRecordEventTestServer(t)
	ctx := context.Background()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_record_event", Arguments: RecordEventInput{
		EventType: "note", Summary: "s",
	}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("record_event error: %+v", res.Content)
	}

	stats, err := store.LedgerStats(storeDir)
	if err != nil {
		t.Fatalf("LedgerStats: %v", err)
	}
	found := false
	for _, s := range stats {
		if s.Tool == "ctr_record_event" {
			found = true
			if s.Calls != 1 {
				t.Fatalf("ctr_record_event calls=%d want 1", s.Calls)
			}
		}
	}
	if !found {
		t.Fatalf("ctr_record_event ledger row missing: %+v", stats)
	}
}

// TestRecordEventSchemaGating: 브리프 Step1 ⑦(이 태스크 시점 — ctr_record_event 1종). Session이
// 있으면 기본 표면에 등장(DestructiveHint=false)하고, Session이 nil이면 등장하지 않는다.
func TestRecordEventSchemaGating(t *testing.T) {
	cs, _, _, _ := newRecordEventTestServer(t)
	lt, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var tool *mcp.Tool
	for _, tl := range lt.Tools {
		if tl.Name == "ctr_record_event" {
			tool = tl
		}
	}
	if tool == nil {
		t.Fatalf("ctr_record_event not in tools/list: %+v", lt.Tools)
	}
	if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
		t.Fatalf("ctr_record_event DestructiveHint want &false, got %+v", tool.Annotations)
	}

	csNoSession, _ := newTestServer(t, nil)
	lt2, err := csNoSession.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools (no session): %v", err)
	}
	for _, tl := range lt2.Tools {
		if tl.Name == "ctr_record_event" {
			t.Fatalf("ctr_record_event should not be registered when Session is nil")
		}
	}
}

// --- ctr_session_summary (태스크 4, 설계 §3.2) ---

// newSummaryTestServer: newRecordEventTestServer와 동형이지만 sessionDir·storeDir을 모두
// 반환한다(session_id 필터 테스트가 같은 session.db를 가리키는 2번째 session.Open을 필요로
// 하고, ledger 테스트가 storeDir을 필요로 함 — 기존 헬퍼 시그니처는 7개 호출부를 건드리게 돼
// 별도로 둔다).
func newSummaryTestServer(t *testing.T) (cs *mcp.ClientSession, st *store.Store, sess *session.DB, sessionDir, storeDir string) {
	t.Helper()
	canon, err := ident.Canonicalize(t.TempDir())
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	storeDir = t.TempDir()
	st, err = store.Open(storeDir, false)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	sessionDir = t.TempDir()
	sess, err = session.Open(sessionDir, session.Options{Producer: "test/session-summary"})
	if err != nil {
		t.Fatalf("session open: %v", err)
	}
	t.Cleanup(func() { sess.Close() })

	srv, err := NewServer(Config{Canon: canon, Store: st, SelfExe: testSelfExe(t), Session: sess})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	srvT, cliT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, srvT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err = client.Connect(ctx, cliT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, st, sess, sessionDir, storeDir
}

// findSummaryGroup: 응답 groups에서 event_type이 일치하는 그룹을 찾는다(테스트 공용 헬퍼).
func findSummaryGroup(out SessionSummaryOutput, eventType string) *summaryGroup {
	for i := range out.Groups {
		if out.Groups[i].EventType == eventType {
			return &out.Groups[i]
		}
	}
	return nil
}

// TestSessionRuntimeStorageErrorMapsToStorageUnavailable — 최종리뷰 C2(Codex P2): startup 이후
// session.db가 malformed/불용이 됐을 때 세션 질의가 던지는 raw SQLite 오류가 INTERNAL로
// 떨어지지 않고 STORAGE_UNAVAILABLE로 매핑돼야 한다. 세 핸들러(summary/export/search-events)가
// 공통으로 쓰는 매핑 조합 `toToolError(session.ClassifyStorageErr(err))`를, 실제 훼손 파일에
// 대한 실 SQLite 오류로 검증한다 — 열린 연결을 in-place로 훼손하는 방식은 WAL 그림자와 Windows
// 의 -shm 메모리 매핑 때문에 이식성이 없어(실측), 프레시 훼손 파일로 동일 오류 경로를 재현한다.
func TestSessionRuntimeStorageErrorMapsToStorageUnavailable(t *testing.T) {
	dir := t.TempDir()
	// 헤더 없는 쓰레기 바이트 = SQLITE_NOTADB(=26). session.OpenReadOnly는 지연 연결이라
	// 첫 쿼리에서 오류가 표면화된다.
	if err := os.WriteFile(filepath.Join(dir, "session.db"), bytes.Repeat([]byte{0xEE}, 4096), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	reader, err := session.OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer func() { _ = reader.Close() }()
	var s string
	qErr := reader.QueryRow("PRAGMA quick_check").Scan(&s)
	if qErr == nil {
		t.Fatalf("expected a SQLite storage error from garbage db, got quick_check=%q", s)
	}

	mapped := toToolError(session.ClassifyStorageErr(qErr))
	if !strings.HasPrefix(mapped.Error(), "["+codeStorageUnavailable+"]") {
		t.Fatalf("mapped=%q want %s prefix", mapped.Error(), codeStorageUnavailable)
	}
}

// TestSummary_RoundTrip: 브리프 Step1 ①⑦ — 타입 그룹·시간 역순·artifact_refs 왕복·untrusted:true.
func TestSummary_RoundTrip(t *testing.T) {
	cs, _, sess, _, _ := newSummaryTestServer(t)
	ctx := context.Background()

	mustAppend(t, sess, session.Event{Type: "decision", Summary: "chose A"})
	mustAppend(t, sess, session.Event{Type: "decision", Summary: "chose B"})
	mustAppend(t, sess, session.Event{Type: "note", Summary: "fyi"})

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_session_summary", Arguments: SessionSummaryInput{}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("summary error: %+v", res.Content)
	}
	var out SessionSummaryOutput
	remarshal(t, res.StructuredContent, &out)

	if !out.Untrusted {
		t.Fatalf("Untrusted=false want true")
	}
	decisions := findSummaryGroup(out, "decision")
	if decisions == nil || len(decisions.Events) != 2 {
		t.Fatalf("decision group=%+v want 2 events", decisions)
	}
	if decisions.Events[0].Summary != "chose B" || decisions.Events[1].Summary != "chose A" {
		t.Fatalf("decision order=%+v want [chose B, chose A] (time desc)", decisions.Events)
	}
	notes := findSummaryGroup(out, "note")
	if notes == nil || len(notes.Events) != 1 || notes.Events[0].Summary != "fyi" {
		t.Fatalf("note group=%+v want exactly [fyi]", notes)
	}
}

// TestSummary_SessionIDFilter: 브리프 Step1 ② — session_id 지정 시 다른 세션 이벤트가 섞이지
// 않는다(같은 worktree session.db에 2번째 session.Open으로 별도 세션을 만든다).
func TestSummary_SessionIDFilter(t *testing.T) {
	cs, _, sess1, sessionDir, _ := newSummaryTestServer(t)
	ctx := context.Background()
	mustAppend(t, sess1, session.Event{Type: "note", Summary: "from-sess1"})

	sess2, err := session.Open(sessionDir, session.Options{Producer: "test/session-summary-2"})
	if err != nil {
		t.Fatalf("session2 open: %v", err)
	}
	t.Cleanup(func() { sess2.Close() })
	mustAppend(t, sess2, session.Event{Type: "note", Summary: "from-sess2"})

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_session_summary", Arguments: SessionSummaryInput{SessionID: sess1.SessionID()}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("summary error: %+v", res.Content)
	}
	var out SessionSummaryOutput
	remarshal(t, res.StructuredContent, &out)

	notes := findSummaryGroup(out, "note")
	if notes == nil || len(notes.Events) != 1 || notes.Events[0].Summary != "from-sess1" {
		t.Fatalf("note group=%+v want exactly [from-sess1]", notes)
	}
}

// TestSummary_CheckpointIncludedAndDedupedFromGroups: 브리프 Step1 ④(1/2) — 최신 checkpoint가
// checkpoint 필드에 실리고 groups에는 중복되지 않는다.
func TestSummary_CheckpointIncludedAndDedupedFromGroups(t *testing.T) {
	cs, _, sess, _, _ := newSummaryTestServer(t)
	ctx := context.Background()
	mustAppend(t, sess, session.Event{Type: "session_checkpoint", Summary: "cp-old"})
	cpID := mustAppend(t, sess, session.Event{Type: "session_checkpoint", Summary: "cp-latest"})

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_session_summary", Arguments: SessionSummaryInput{}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("summary error: %+v", res.Content)
	}
	var out SessionSummaryOutput
	remarshal(t, res.StructuredContent, &out)

	if out.Checkpoint == nil || out.Checkpoint.EventID != cpID || out.Checkpoint.Summary != "cp-latest" {
		t.Fatalf("checkpoint=%+v want event_id=%s(cp-latest)", out.Checkpoint, cpID)
	}
	grp := findSummaryGroup(out, "session_checkpoint")
	for _, e := range grp.Events {
		if e.EventID == cpID {
			t.Fatalf("checkpoint event duplicated in groups: %+v", grp)
		}
	}
}

// TestSummary_CheckpointBudgetOmitted: 브리프 Step1 ④(2/2) — checkpoint 단독으로
// max_return_bytes를 초과하면 생략되고(checkpoint 필드 없음) checkpoint_truncated로 표시한다
// (hard cap 유지 — 예산을 넘겨 싣지 않음, 설계 §3.2).
func TestSummary_CheckpointBudgetOmitted(t *testing.T) {
	cs, _, sess, _, _ := newSummaryTestServer(t)
	ctx := context.Background()
	mustAppend(t, sess, session.Event{Type: "session_checkpoint", Summary: strings.Repeat("c", 100)})

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_session_summary", Arguments: SessionSummaryInput{MaxReturnBytes: 10}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("summary error: %+v", res.Content)
	}
	var out SessionSummaryOutput
	remarshal(t, res.StructuredContent, &out)

	if out.Checkpoint != nil {
		t.Fatalf("checkpoint=%+v want nil(omitted, 100B > 10B budget)", out.Checkpoint)
	}
	if !out.CheckpointTruncated {
		t.Fatalf("CheckpointTruncated=false want true")
	}
}

// TestSummary_GroupTruncatedUnderBudget: 브리프 Step1 ⑥ — 그룹별 truncated 개별 표기(예산
// 소진 지점의 그룹만 truncated:true, 하드캡 유지로 예산 초과 없이 절단).
func TestSummary_GroupTruncatedUnderBudget(t *testing.T) {
	cs, _, sess, _, _ := newSummaryTestServer(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		mustAppend(t, sess, session.Event{Type: "note", Summary: strings.Repeat("n", 50)})
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_session_summary", Arguments: SessionSummaryInput{MaxReturnBytes: 120}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("summary error: %+v", res.Content)
	}
	var out SessionSummaryOutput
	remarshal(t, res.StructuredContent, &out)

	notes := findSummaryGroup(out, "note")
	if notes == nil {
		t.Fatalf("note group missing: %+v", out)
	}
	if !notes.Truncated {
		t.Fatalf("note.Truncated=false want true(5x50B > 120B budget)")
	}
	if len(notes.Events) == 0 || len(notes.Events) >= 5 {
		t.Fatalf("note.Events len=%d want 0<n<5(budget-limited)", len(notes.Events))
	}
}

// TestSummary_BudgetMeasuresSummaryLenOnly: 부채 ②(설계 §9). summary 예산은 직렬화 전체가
// 아니라 이벤트 summary 텍스트 길이(len(Summary))만 잰다는 관례를 계약으로 고정한다. UUIDv7
// event_id·session_id만으로도 이벤트 직렬화는 80바이트를 넘지만, 3바이트 요약은 5바이트 예산에
// 들어간다 — 직렬화 전체를 쟀다면 첫 이벤트조차 실리지 못해 0건이어야 한다. v0.1 최종리뷰가
// 명시한 "len(Summary)-only(full-payload라 최대 ~4배 초과 가능)"가 export(직렬화 전체 계상)와
// 달리 summary에서는 의도된 설계임을 못박는다. 기존 동작 고정 — born-green.
func TestSummary_BudgetMeasuresSummaryLenOnly(t *testing.T) {
	cs, _, sess, _, _ := newSummaryTestServer(t)
	ctx := context.Background()
	mustAppend(t, sess, session.Event{Type: "note", Summary: "bbbbbb"}) // 6B, 더 오래됨
	mustAppend(t, sess, session.Event{Type: "note", Summary: "aaa"})    // 3B, 가장 최근

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_session_summary", Arguments: SessionSummaryInput{MaxReturnBytes: 5}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("summary error: %+v", res.Content)
	}
	var out SessionSummaryOutput
	remarshal(t, res.StructuredContent, &out)

	// len(Summary)-only: 최신 "aaa"(3B) ≤ 5 → 1건 유지, 남은 예산 2B < "bbbbbb"(6B) → 절단.
	notes := findSummaryGroup(out, "note")
	if notes == nil || len(notes.Events) != 1 || notes.Events[0].Summary != "aaa" {
		t.Fatalf("note group=%+v want exactly [aaa](len(Summary)-only 관례 — 직렬화 전체였다면 0건)", notes)
	}
	if !notes.Truncated {
		t.Fatalf("note.Truncated=false want true(6B 이벤트가 남은 예산 초과)")
	}
}

// TestSummary_GroupsTruncatedFanOutCap: 부채 ①(설계 §9) wire 배선 — session.Summarize의
// fan-out 캡 신호가 ctr_session_summary 출력의 groups_truncated로 그대로 노출된다(소비자 대면
// 계약). session.maxSummaryGroups(=32)를 넘는 구별 event_type을 시드해 신호를 강제한다.
func TestSummary_GroupsTruncatedFanOutCap(t *testing.T) {
	cs, _, sess, _, _ := newSummaryTestServer(t)
	ctx := context.Background()
	for i := 0; i < 40; i++ { // > 32(session.maxSummaryGroups) 확실히 초과
		mustAppend(t, sess, session.Event{Type: fmt.Sprintf("type_%d", i), Summary: "s"})
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_session_summary", Arguments: SessionSummaryInput{MaxReturnBytes: 1 << 20}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("summary error: %+v", res.Content)
	}
	var out SessionSummaryOutput
	remarshal(t, res.StructuredContent, &out)

	if !out.GroupsTruncated {
		t.Fatalf("GroupsTruncated=false want true(40 타입 > 32 상한)")
	}
	if len(out.Groups) > 32 {
		t.Fatalf("len(Groups)=%d want <=32(fan-out 상한)", len(out.Groups))
	}
}

// TestSummary_MissingArtifactRef: 브리프 Step1 ⑤ — content.db에 없는 hash를 가리키는
// artifact_refs는 missing:true(D15 hint, 오류 아님 — 호출 자체는 성공한다).
func TestSummary_MissingArtifactRef(t *testing.T) {
	cs, st, sess, _, _ := newSummaryTestServer(t)
	ctx := context.Background()

	id, err := st.Register(ctx, store.Registration{
		StoredBytes: []byte("present"), MediaType: "text/plain",
		Source: store.SourceMeta{URI: "/p.txt", Kind: "file", SrcHash: "h1"},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	presentHash, err := st.ArtifactHashByID(ctx, id)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	presentURI := "artifact://" + sess.SessionID() + "/sha256-" + presentHash
	missingURI := "artifact://" + sess.SessionID() + "/sha256-" + strings.Repeat("0", 64)

	mustAppend(t, sess, session.Event{Type: "note", Summary: "refs", ArtifactRefs: []string{presentURI, missingURI}})

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_session_summary", Arguments: SessionSummaryInput{}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("summary error(missing는 hint여야지 오류가 아님): %+v", res.Content)
	}
	var out SessionSummaryOutput
	remarshal(t, res.StructuredContent, &out)

	notes := findSummaryGroup(out, "note")
	if notes == nil || len(notes.Events) != 1 || len(notes.Events[0].ArtifactRefs) != 2 {
		t.Fatalf("note group=%+v want 1 event with 2 refs", notes)
	}
	refs := notes.Events[0].ArtifactRefs
	if refs[0].URI != presentURI || refs[0].Missing {
		t.Fatalf("present ref=%+v want missing=false", refs[0])
	}
	if refs[1].URI != missingURI || !refs[1].Missing {
		t.Fatalf("missing ref=%+v want missing=true", refs[1])
	}
}

// TestSummary_LimitClamp: limit 기본 5·최대 20(초과 클램프).
func TestSummary_LimitClamp(t *testing.T) {
	cs, _, sess, _, _ := newSummaryTestServer(t)
	ctx := context.Background()
	for i := 0; i < 25; i++ {
		mustAppend(t, sess, session.Event{Type: "note", Summary: fmt.Sprintf("n%02d", i)})
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_session_summary", Arguments: SessionSummaryInput{}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var out SessionSummaryOutput
	remarshal(t, res.StructuredContent, &out)
	if notes := findSummaryGroup(out, "note"); notes == nil || len(notes.Events) != 5 {
		t.Fatalf("default limit note group len=%v want 5", notes)
	}

	res2, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_session_summary", Arguments: SessionSummaryInput{Limit: 999}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var out2 SessionSummaryOutput
	remarshal(t, res2.StructuredContent, &out2)
	if notes := findSummaryGroup(out2, "note"); notes == nil || len(notes.Events) != 20 {
		t.Fatalf("limit=999 note group len=%v want 20(clamped)", notes)
	}
}

// TestSummary_SchemaGating: Session이 있으면 기본 표면에 등장(ReadOnlyHint=true)하고, 없으면
// 등장하지 않는다(registerRecordEvent와 동일 게이트).
func TestSummary_SchemaGating(t *testing.T) {
	cs, _, _, _, _ := newSummaryTestServer(t)
	lt, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var tool *mcp.Tool
	for _, tl := range lt.Tools {
		if tl.Name == "ctr_session_summary" {
			tool = tl
		}
	}
	if tool == nil {
		t.Fatalf("ctr_session_summary not in tools/list: %+v", lt.Tools)
	}
	if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
		t.Fatalf("ctr_session_summary ReadOnlyHint want true, got %+v", tool.Annotations)
	}

	csNoSession, _ := newTestServer(t, nil)
	lt2, err := csNoSession.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools (no session): %v", err)
	}
	for _, tl := range lt2.Tools {
		if tl.Name == "ctr_session_summary" {
			t.Fatalf("ctr_session_summary should not be registered when Session is nil")
		}
	}
}

// TestSummary_LedgerAppend: 호출마다 LedgerAppend(ctr_record_event 패턴 승계).
func TestSummary_LedgerAppend(t *testing.T) {
	cs, _, sess, _, storeDir := newSummaryTestServer(t)
	ctx := context.Background()
	mustAppend(t, sess, session.Event{Type: "note", Summary: "s"})

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_session_summary", Arguments: SessionSummaryInput{}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("summary error: %+v", res.Content)
	}

	stats, err := store.LedgerStats(storeDir)
	if err != nil {
		t.Fatalf("LedgerStats: %v", err)
	}
	found := false
	for _, s := range stats {
		if s.Tool == "ctr_session_summary" {
			found = true
			if s.Calls != 1 {
				t.Fatalf("ctr_session_summary calls=%d want 1", s.Calls)
			}
		}
	}
	if !found {
		t.Fatalf("ctr_session_summary ledger row missing: %+v", stats)
	}
}

// mustAppend: session.DB.Append의 테스트 공용 래퍼(event_id만 필요한 호출부용).
func mustAppend(t *testing.T, sess *session.DB, ev session.Event) string {
	t.Helper()
	_, eventID, _, err := sess.Append(ev)
	if err != nil {
		t.Fatalf("append(%+v): %v", ev, err)
	}
	return eventID
}

// --- ctr_export_events (태스크 5, 설계 §3.3, D16) ---

// exportBaseline: session_start 자동 이벤트(Open 시점 기록)를 건너뛰기 위해, 테스트가 이벤트를
// 추가로 append하기 전 현재 세션의 next_after를 조회해 커서 시작점으로 쓴다.
func exportBaseline(t *testing.T, cs *mcp.ClientSession, sessionID string) int64 {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ctr_export_events",
		Arguments: ExportEventsInput{SessionID: sessionID, Limit: maxExportLimit},
	})
	if err != nil {
		t.Fatalf("exportBaseline call: %v", err)
	}
	var out ExportEventsOutput
	remarshal(t, res.StructuredContent, &out)
	return out.NextAfter
}

// findExportEvent: 응답 events에서 event_id로 찾는 테스트 공용 헬퍼.
func findExportEvent(out ExportEventsOutput, eventID string) *session.EventV1 {
	for i := range out.Events {
		if out.Events[i].EventID == eventID {
			return &out.Events[i]
		}
	}
	return nil
}

// TestExportEvents_RoundTrip: record_event로 기록한 이벤트가 export에서 §26 전 필드로
// 왕복한다(schemaVersion·privacyLabel 상수, producer 유도, artifact_refs/related/attributes
// 보존, untrusted:true).
func TestExportEvents_RoundTrip(t *testing.T) {
	cs, _, sess, _, _ := newSummaryTestServer(t)
	ctx := context.Background()
	eid := mustAppend(t, sess, session.Event{
		Type: "decision", Summary: "chose approach A",
		Attributes:   json.RawMessage(`{"k":"v"}`),
		ArtifactRefs: []string{"artifact://" + sess.SessionID() + "/sha256-abc"},
		Related:      []string{"symbol://x"},
	})

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_export_events", Arguments: ExportEventsInput{}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("export error: %+v", res.Content)
	}
	var out ExportEventsOutput
	remarshal(t, res.StructuredContent, &out)

	if !out.Untrusted {
		t.Fatalf("Untrusted=false want true")
	}
	ev := findExportEvent(out, eid)
	if ev == nil {
		t.Fatalf("event %s not found in export: %+v", eid, out.Events)
	}
	if ev.SchemaVersion != "1.0" || ev.PrivacyLabel != "internal" {
		t.Fatalf("ev=%+v want schemaVersion=1.0 privacyLabel=internal", ev)
	}
	if ev.SessionID != sess.SessionID() || ev.EventType != "decision" || ev.Summary != "chose approach A" {
		t.Fatalf("ev identity=%+v", ev)
	}
	if ev.Producer.Name != "context-router" {
		t.Fatalf("ev.Producer=%+v want name=context-router", ev.Producer)
	}
	if len(ev.ArtifactRefs) != 1 || ev.ArtifactRefs[0] != "artifact://"+sess.SessionID()+"/sha256-abc" {
		t.Fatalf("ev.ArtifactRefs=%v", ev.ArtifactRefs)
	}
	if len(ev.RelatedResources) != 1 || ev.RelatedResources[0] != "symbol://x" {
		t.Fatalf("ev.RelatedResources=%v", ev.RelatedResources)
	}
	attrs, ok := ev.Attributes.(map[string]any)
	if !ok || attrs["k"] != "v" {
		t.Fatalf("ev.Attributes=%v want {k:v}", ev.Attributes)
	}
}

// TestExportEvents_DefaultLimitClamp: limit 미지정 시 기본 50건까지만 반환(브리프 설계 §3.3).
func TestExportEvents_DefaultLimitClamp(t *testing.T) {
	cs, _, sess, _, _ := newSummaryTestServer(t)
	ctx := context.Background()
	for i := 0; i < 60; i++ {
		mustAppend(t, sess, session.Event{Type: "note", Summary: fmt.Sprintf("n%02d", i)})
	}

	// C4: 예산 계상이 직렬화 전체 바이트가 된 뒤로 기본 예산(8192)이 50건 전에 절단하므로,
	// limit 클램프(50)만 격리 검증하도록 예산을 크게 준다.
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_export_events", Arguments: ExportEventsInput{MaxReturnBytes: 1 << 20}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("export error: %+v", res.Content)
	}
	var out ExportEventsOutput
	remarshal(t, res.StructuredContent, &out)
	if len(out.Events) != 50 {
		t.Fatalf("default limit len=%d want 50", len(out.Events))
	}
}

// TestClampExportLimit: 기본 50·최대 200(초과 클램프), 0 이하는 기본값(순수 함수 단위 테스트).
func TestClampExportLimit(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, defaultExportLimit},
		{-5, defaultExportLimit},
		{10, 10},
		{200, 200},
		{201, maxExportLimit},
		{9999, maxExportLimit},
	}
	for _, c := range cases {
		if got := clampExportLimit(c.in); got != c.want {
			t.Fatalf("clampExportLimit(%d)=%d want %d", c.in, got, c.want)
		}
	}
}

// TestExportEvents_CursorPagination: limit=2로 도구를 반복 호출하면 시드된 이벤트 전부를
// 정확히 한 번씩, 삽입 순서대로 방문하고 next_after가 매 호출 단조 증가한다(session_id로
// 필터해 session_start 자동 이벤트와 섞이지 않게 한다).
func TestExportEvents_CursorPagination(t *testing.T) {
	cs, _, sess, _, _ := newSummaryTestServer(t)
	ctx := context.Background()
	after := exportBaseline(t, cs, sess.SessionID()) // session_start 자동 이벤트를 건너뛴다.

	const n = 5
	want := make([]string, n)
	for i := 0; i < n; i++ {
		want[i] = mustAppend(t, sess, session.Event{Type: "note", Summary: fmt.Sprintf("n%d", i)})
	}

	var got []string
	lastAfter := after
	for i := 0; i < 10; i++ { // 안전 상한(무한루프 방지)
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{
			Name:      "ctr_export_events",
			Arguments: ExportEventsInput{After: after, SessionID: sess.SessionID(), Limit: 2},
		})
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if res.IsError {
			t.Fatalf("export error: %+v", res.Content)
		}
		var out ExportEventsOutput
		remarshal(t, res.StructuredContent, &out)
		if len(out.Events) == 0 {
			break
		}
		if out.NextAfter <= lastAfter {
			t.Fatalf("iter %d: next_after=%d want > %d(단조 증가)", i, out.NextAfter, lastAfter)
		}
		for _, ev := range out.Events {
			got = append(got, ev.EventID)
		}
		after = out.NextAfter
		lastAfter = out.NextAfter
	}

	if len(got) != n {
		t.Fatalf("got=%v(len=%d) want %d events", got, len(got), n)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d]=%s want %s(순서/중복 위반)", i, got[i], want[i])
		}
	}
}

// TestExportEvents_MaxReturnBytesTruncatesWithoutLoss: max_return_bytes를 작게 잡아 배치를
// 강제로 절단시켜도, next_after를 따라 반복 호출하면 이벤트가 하나도 유실되지 않는다(mcp
// 계층 applyExportBudget의 존재 이유 — RowID 기반 next_after 정확성 회귀 검증).
func TestExportEvents_MaxReturnBytesTruncatesWithoutLoss(t *testing.T) {
	cs, _, sess, _, _ := newSummaryTestServer(t)
	ctx := context.Background()
	after := exportBaseline(t, cs, sess.SessionID()) // session_start 자동 이벤트를 건너뛴다.

	const n = 5
	want := make([]string, n)
	for i := 0; i < n; i++ {
		want[i] = mustAppend(t, sess, session.Event{Type: "note", Summary: strings.Repeat("n", 50)})
	}

	// C4: 예산 계상이 summary가 아니라 직렬화 전체이므로, 이벤트 1건 직렬화 크기(L)를 실측해
	// 2건치 예산(2L)을 잡는다 — 1건은 항상 진행(무손실), 5건은 여러 배치로 절단된다.
	big, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ctr_export_events",
		Arguments: ExportEventsInput{After: after, SessionID: sess.SessionID(), MaxReturnBytes: 1 << 20},
	})
	if err != nil {
		t.Fatalf("probe call: %v", err)
	}
	var bigOut ExportEventsOutput
	remarshal(t, big.StructuredContent, &bigOut)
	if len(bigOut.Events) != n {
		t.Fatalf("probe: got %d events want %d", len(bigOut.Events), n)
	}
	evBytes, err := json.Marshal(bigOut.Events[0])
	if err != nil {
		t.Fatalf("marshal probe event: %v", err)
	}
	budget := 2 * len(evBytes)

	var got []string
	sawTruncated := false
	for i := 0; i < 20; i++ { // 안전 상한(무한루프 방지)
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{
			Name:      "ctr_export_events",
			Arguments: ExportEventsInput{After: after, SessionID: sess.SessionID(), MaxReturnBytes: budget},
		})
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if res.IsError {
			t.Fatalf("export error: %+v", res.Content)
		}
		var out ExportEventsOutput
		remarshal(t, res.StructuredContent, &out)
		if len(out.Events) == 0 {
			break
		}
		if out.Truncated {
			sawTruncated = true
		}
		if out.NextAfter <= after {
			t.Fatalf("next_after=%d want > %d(진행 없음 — 무손실 재구성 위반)", out.NextAfter, after)
		}
		for _, ev := range out.Events {
			got = append(got, ev.EventID)
		}
		after = out.NextAfter
	}

	if !sawTruncated {
		t.Fatalf("2건치 예산(%dB)으로 5건을 실었는데 truncated=true가 한 번도 없었다 — 테스트 전제 오류", budget)
	}
	if len(got) != n {
		t.Fatalf("got=%v(len=%d) want %d events(no loss under byte budget)", got, len(got), n)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d]=%s want %s(순서/중복/유실 위반)", i, got[i], want[i])
		}
	}
}

// TestExportEvents_IncludesSuperseded: export는 무필터라 superseded 이벤트도 포함한다
// (ctr_session_summary와의 핵심 차이).
func TestExportEvents_IncludesSuperseded(t *testing.T) {
	cs, _, sess, _, _ := newSummaryTestServer(t)
	ctx := context.Background()
	oldID := mustAppend(t, sess, session.Event{Type: "decision", Summary: "first take"})
	newID := mustAppend(t, sess, session.Event{Type: "decision", Summary: "corrected", Supersedes: oldID})

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ctr_export_events",
		Arguments: ExportEventsInput{SessionID: sess.SessionID()},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var out ExportEventsOutput
	remarshal(t, res.StructuredContent, &out)

	if findExportEvent(out, oldID) == nil {
		t.Fatalf("superseded event %s missing from export(무필터 위반): %+v", oldID, out.Events)
	}
	newEv := findExportEvent(out, newID)
	if newEv == nil || newEv.Supersedes != oldID {
		t.Fatalf("newEv=%+v want Supersedes=%s", newEv, oldID)
	}
}

// TestExportEvents_SchemaGating: Session이 있으면 기본 표면에 등장(ReadOnlyHint=true)하고,
// 없으면 등장하지 않는다(registerRecordEvent/registerSessionSummary와 동일 게이트).
func TestExportEvents_SchemaGating(t *testing.T) {
	cs, _, _, _, _ := newSummaryTestServer(t)
	lt, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var tool *mcp.Tool
	for _, tl := range lt.Tools {
		if tl.Name == "ctr_export_events" {
			tool = tl
		}
	}
	if tool == nil {
		t.Fatalf("ctr_export_events not in tools/list: %+v", lt.Tools)
	}
	if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
		t.Fatalf("ctr_export_events ReadOnlyHint want true, got %+v", tool.Annotations)
	}

	csNoSession, _ := newTestServer(t, nil)
	lt2, err := csNoSession.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools (no session): %v", err)
	}
	for _, tl := range lt2.Tools {
		if tl.Name == "ctr_export_events" {
			t.Fatalf("ctr_export_events should not be registered when Session is nil")
		}
	}
}

// TestExportEvents_LedgerAppend: 호출마다 LedgerAppend(ctr_record_event/ctr_session_summary
// 패턴 승계).
func TestExportEvents_LedgerAppend(t *testing.T) {
	cs, _, sess, _, storeDir := newSummaryTestServer(t)
	ctx := context.Background()
	mustAppend(t, sess, session.Event{Type: "note", Summary: "s"})

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ctr_export_events", Arguments: ExportEventsInput{}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("export error: %+v", res.Content)
	}

	stats, err := store.LedgerStats(storeDir)
	if err != nil {
		t.Fatalf("LedgerStats: %v", err)
	}
	found := false
	for _, s := range stats {
		if s.Tool == "ctr_export_events" {
			found = true
			if s.Calls != 1 {
				t.Fatalf("ctr_export_events calls=%d want 1", s.Calls)
			}
		}
	}
	if !found {
		t.Fatalf("ctr_export_events ledger row missing: %+v", stats)
	}
}

// --- ctr_fetch 원장 계측 (태스크 8b, 설계 v0.20 D103 계약 2·3) ---

// TestFetchRecordsMissOnlyOnAbsentArtifact: 없는 artifact를 요청하면 미해소 행이 남고,
// 잘못된 chunk id·잘못된 선택자는 남지 않는다. 앞의 둘은 store.ErrNotFound 하나를 공유하므로
// (store.go:722·607) errors.Is로는 안 갈린다 — 이 테스트가 그 구분을 고정한다(D103 계약 3).
func TestFetchRecordsMissOnlyOnAbsentArtifact(t *testing.T) {
	cs, st, _, storeDir := newRecordEventTestServer(t)
	ctx := context.Background()

	body := "haystack needle haystack"
	artID, err := st.Register(ctx, store.Registration{
		StoredBytes: []byte(body), MediaType: "text/plain",
		Source: store.SourceMeta{URI: "shadow:Bash:hit", Kind: "hook", SrcHash: "sh-hit"},
		Chunks: []store.Chunk{{Ordinal: 0, ByteEnd: int64(len(body)), Text: body}},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// ① 없는 artifact → 미해소 1행
	callFetch(t, cs, FetchInput{ArtifactID: artID + 9999, ChunkID: ptrTo(int64(1))})
	if fs := fetchStats(t, storeDir); fs.Missed != 1 || fs.Resolved != 0 {
		t.Fatalf("artifact 부재 뒤 resolved=%d missed=%d, 기대 0/1", fs.Resolved, fs.Missed)
	}
	// ② 있는 artifact + 없는 chunk id → 입력 문제다, 미해소가 늘면 안 된다
	callFetch(t, cs, FetchInput{ArtifactID: artID, ChunkID: ptrTo(int64(999_999))})
	if fs := fetchStats(t, storeDir); fs.Missed != 1 {
		t.Fatalf("잘못된 chunk id가 미해소로 셌다: missed=%d, 기대 1", fs.Missed)
	}
	// ③ 선택자 없음 → ReadRange까지 가지 않는다. selectorFromInput의 "정확히 1개" 게이트
	// (mcp.go:484-486)가 먼저 거른다. ReadRange 자신의 ErrInvalidSelector(store.go:712-716,
	// 미인식 sel.Kind)는 selectorFromInput이 "chunk"/"line"/"byte" 외의 Kind를 절대 만들지
	// 않으므로 ctr_fetch 경로로는 구조적으로 도달 불가능하다 — 그래도 미해소가 늘면 안 되는
	// 것은 다른 잘못된 입력과 같다.
	callFetch(t, cs, FetchInput{ArtifactID: artID})
	if fs := fetchStats(t, storeDir); fs.Missed != 1 {
		t.Fatalf("잘못된 선택자가 미해소로 셌다: missed=%d, 기대 1", fs.Missed)
	}
}

// TestFetchRecordsAgeOnResolve: 해소되면 나이가 박힌다. 시계는 max(sources.indexed_at)이므로
// 소스 시각을 과거로 옮기면 그만큼 나이가 나온다(D103 계약 2).
func TestFetchRecordsAgeOnResolve(t *testing.T) {
	cs, st, _, storeDir := newRecordEventTestServer(t)
	ctx := context.Background()
	body := "resolved body"
	artID, err := st.Register(ctx, store.Registration{
		StoredBytes: []byte(body), MediaType: "text/plain",
		Source: store.SourceMeta{URI: "shadow:Bash:age", Kind: "hook", SrcHash: "sh-age"},
		Chunks: []store.Chunk{{Ordinal: 0, ByteEnd: int64(len(body)), Text: body}},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := st.Reader().Exec(
		`UPDATE sources SET indexed_at=? WHERE uri='shadow:Bash:age'`,
		time.Now().Add(-2*time.Hour).Unix(),
	); err != nil {
		t.Fatalf("소스 시각: %v", err)
	}

	callFetch(t, cs, FetchInput{ArtifactID: artID, ByteStart: ptrTo(int64(0)), ByteEnd: ptrTo(int64(5))})
	fs := fetchStats(t, storeDir)
	if fs.Resolved != 1 || fs.Missed != 0 {
		t.Fatalf("resolved=%d missed=%d, 기대 1/0", fs.Resolved, fs.Missed)
	}
	if fs.AgeMax < 7000 || fs.AgeMax > 7400 { // 2시간 = 7200초, 실행 시각 오차 허용
		t.Fatalf("AgeMax=%d, 기대 약 7200", fs.AgeMax)
	}
}

// TestFetchRecordsShadowOwnershipOnResolve: 해소 행에 **퍼지 대상 여부**가 함께 박힌다
// (릴리스 리뷰 소견 F4). explicit 아티팩트는 보존 창이 손대지 않으므로 그 회수 나이는 창의
// 길이에 대해 아무 말도 하지 않는데, 표식이 없으면 그 회수가 D104의 "해소 30건"을 채우고
// 분위수까지 지배한다. **표식이 없거나 항상 참이면 이 테스트가 두 군데서 떨어진다** —
// explicit 쪽 나이를 hook 쪽보다 두 자릿수 크게 심어 두었기 때문이다.
func TestFetchRecordsShadowOwnershipOnResolve(t *testing.T) {
	cs, st, _, storeDir := newRecordEventTestServer(t)
	ctx := context.Background()
	reg := func(body, uri, kind string) int64 {
		t.Helper()
		id, err := st.Register(ctx, store.Registration{
			StoredBytes: []byte(body), MediaType: "text/plain",
			Source: store.SourceMeta{URI: uri, Kind: kind, SrcHash: "sh-" + uri},
			Chunks: []store.Chunk{{Ordinal: 0, ByteEnd: int64(len(body)), Text: body}},
		})
		if err != nil {
			t.Fatalf("register %s: %v", uri, err)
		}
		return id
	}
	hookID := reg("hook captured body", "shadow:Bash:owned", "hook")
	fileID := reg("explicitly indexed body", "/tmp/explicit.txt", "file")
	age := func(uri string, ago time.Duration) {
		t.Helper()
		if _, err := st.Reader().Exec(
			`UPDATE sources SET indexed_at=? WHERE uri=?`, time.Now().Add(-ago).Unix(), uri,
		); err != nil {
			t.Fatalf("소스 시각 %s: %v", uri, err)
		}
	}
	age("shadow:Bash:owned", time.Hour)           // 3600초
	age("/tmp/explicit.txt", 500_000*time.Second) // 두 자릿수 더 큰 나이
	// 사전 가드: 두 아티팩트가 정말 별개 hash여야 한다 — 같으면 귀속 판정이 한 집합에 섞인다.
	var hashes int
	if err := st.Reader().QueryRow(
		`SELECT count(DISTINCT content_hash) FROM artifacts WHERE id IN (?,?)`, hookID, fileID,
	).Scan(&hashes); err != nil {
		t.Fatalf("hash 확인: %v", err)
	}
	if hashes != 2 {
		t.Fatalf("픽스처가 의도한 상태가 아니다: 별개 hash %d개(기대 2)", hashes)
	}

	callFetch(t, cs, FetchInput{ArtifactID: hookID, ByteStart: ptrTo(int64(0)), ByteEnd: ptrTo(int64(5))})
	callFetch(t, cs, FetchInput{ArtifactID: fileID, ByteStart: ptrTo(int64(0)), ByteEnd: ptrTo(int64(5))})

	fs := fetchStats(t, storeDir)
	if fs.Resolved != 2 {
		t.Fatalf("Resolved=%d want 2 — 채택 게이트는 두 회수를 다 센다", fs.Resolved)
	}
	if fs.ShadowResolved != 1 {
		t.Fatalf("ShadowResolved=%d want 1 — explicit 회수가 퍼지 대상 모집단에 섞였다", fs.ShadowResolved)
	}
	if fs.AgeMax < 3500 || fs.AgeMax > 3700 {
		t.Fatalf("AgeMax=%d, 기대 약 3600 — explicit 쪽 500000초가 분포를 지배했다", fs.AgeMax)
	}
}

// TestFetchRecordsZeroAgeWhenSourceAbsent: LastIndexedAtByHash가 (0, nil)을 내는 경로 —
// source 행이 없으면(artifact·chunks는 남아 ReadRange는 여전히 해소된다) at>0 조건이
// 거짓이라 ageS는 초기값 0에 머문다. 나이 조회 실패·부재가 회수 자체를 실패시키지 않는다는
// 계약(mcp.go의 fetch 핸들러 주석)의 유일한 실행 증거 — 해소·미해소 두 테스트 모두 이 분기를
// 지나지 않는다. AgeMax는 원장 전체의 max이므로 다른 나이 값과 섞이면 못 잰다 — 전용 서버.
func TestFetchRecordsZeroAgeWhenSourceAbsent(t *testing.T) {
	cs, st, _, storeDir := newRecordEventTestServer(t)
	ctx := context.Background()
	body := "no source body"
	artID, err := st.Register(ctx, store.Registration{
		StoredBytes: []byte(body), MediaType: "text/plain",
		Source: store.SourceMeta{URI: "shadow:Bash:noage", Kind: "hook", SrcHash: "sh-noage"},
		Chunks: []store.Chunk{{Ordinal: 0, ByteEnd: int64(len(body)), Text: body}},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := st.Reader().Exec(`DELETE FROM sources WHERE uri='shadow:Bash:noage'`); err != nil {
		t.Fatalf("소스 삭제: %v", err)
	}
	// 사전 가드: DELETE가 실패해 조용히 age-양성 테스트의 중복이 되는 것을 막는다.
	var remaining int
	if err := st.Reader().QueryRow(`SELECT count(*) FROM sources WHERE artifact_id=?`, artID).Scan(&remaining); err != nil {
		t.Fatalf("소스 잔존 확인: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("소스 삭제 실패: 잔존 %d행", remaining)
	}

	callFetch(t, cs, FetchInput{ArtifactID: artID, ByteStart: ptrTo(int64(0)), ByteEnd: ptrTo(int64(5))})
	fs := fetchStats(t, storeDir)
	if fs.Resolved != 1 || fs.Missed != 0 {
		t.Fatalf("resolved=%d missed=%d, 기대 1/0", fs.Resolved, fs.Missed)
	}
	// 소스가 없으면 hook 소스도 없다 = 퍼지 술어가 고르지 않는다 → 귀속 모집단 밖이다(소견 F4).
	if fs.ShadowResolved != 0 || fs.AgeMax != 0 {
		t.Fatalf("ShadowResolved=%d AgeMax=%d, 기대 0/0 (source 부재 폴백)", fs.ShadowResolved, fs.AgeMax)
	}
	// 소견 F6: 그 행의 나이는 **NULL**이어야 한다. 0으로 적히면 "방금 포착한 것을 같은 초에
	// 회수했다"와 같은 값이 되고, 집계만 보는 위 두 단언은 그 차이를 못 본다 — 열을 직접 읽는다.
	if got := ledgerAgeCell(t, storeDir); got != "NULL" {
		t.Fatalf("artifact_age_s=%s, 기대 NULL (나이 미상)", got)
	}
}

// --- 요청 ctx 취소와 시계 역행 (릴리스 패스 소견 F3·F9) ---

// registerAgedHookArtifact — hook 소스 하나짜리 아티팩트를 등록하고 그 소스의 indexed_at을
// now+offset으로 옮긴다(offset이 음수면 그만큼 늙은 것, 양수면 **시계가 뒤로 간 뒤에 본 미래
// 타임스탬프**다). 반환은 (artifact id, content_hash).
// 사전 가드: 나이 시계와 귀속 표식이 정말 심은 대로인지 LastIndexedAtByHash로 확인한다 —
// 소스가 안 옮겨졌거나 hook 귀속이 아니면 아래 단정들이 다른 이유로 통과·실패한다.
func registerAgedHookArtifact(t *testing.T, st *store.Store, body, uri string, offset time.Duration) (int64, string) {
	t.Helper()
	ctx := context.Background()
	artID, err := st.Register(ctx, store.Registration{
		StoredBytes: []byte(body), MediaType: "text/plain",
		Source: store.SourceMeta{URI: uri, Kind: "hook", SrcHash: "sh-" + uri},
		Chunks: []store.Chunk{{Ordinal: 0, ByteEnd: int64(len(body)), Text: body}},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	want := time.Now().Add(offset).Unix()
	if _, err := st.Reader().Exec(`UPDATE sources SET indexed_at=? WHERE uri=?`, want, uri); err != nil {
		t.Fatalf("소스 시각: %v", err)
	}
	var hash string
	if err := st.Reader().QueryRow(`SELECT content_hash FROM artifacts WHERE id=?`, artID).Scan(&hash); err != nil {
		t.Fatalf("content_hash: %v", err)
	}
	at, owned, err := st.LastIndexedAtByHash(ctx, hash)
	if err != nil {
		t.Fatalf("사전 가드 LastIndexedAtByHash: %v", err)
	}
	if at != want || !owned {
		t.Fatalf("픽스처가 의도한 상태가 아니다: indexed_at=%d(기대 %d) shadow 귀속=%v(기대 true)", at, want, owned)
	}
	return artID, hash
}

// cancelledCtx — 이미 취소된 요청 ctx. 사용자가 Esc를 누르거나 클라이언트가 시간 초과해
// 핸들러의 ctx가 죽은 상태를 결정론적으로 만든다.
func cancelledCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ctx.Err() == nil {
		t.Fatal("사전 가드: ctx가 취소되지 않았다 — 아래 단정이 취소 없는 정상 경로를 재게 된다")
	}
	return ctx
}

// TestFetchResolveRecordsUnderCancelledCtx — 릴리스 패스 소견 F3. 응답을 **다 만든 뒤** 요청
// ctx가 취소되면(Esc·클라이언트 시간 초과) database/sql이 그 ctx의 ExecContext를 거절하고
// best-effort `_, _ =`가 그 오류를 삼켜, **실제로 성공한 회수가 원장에 흔적을 안 남긴다**.
// 바로 위의 나이 조회도 같은 ctx에서 죽으므로 살아남은 행조차 나이도 귀속 표식도 없다.
// 취소는 부하와 상관이 있어 14일 동안 resolved·resolved_artifacts·shadow_artifacts가 함께
// 낮게 나오고, 정말 쓰인 창이 채택 문턱 미달로 읽혀 D104가 창의 판정 대신 "채택의 문제"로
// 떨어진다 — 이 계측이 사고로 도달하지 않으려는 바로 그 결론이다.
// 세 단정이 각각 원장 기록·나이 조회·귀속 조회의 생존을 잡는다.
func TestFetchResolveRecordsUnderCancelledCtx(t *testing.T) {
	_, st, _, storeDir := newRecordEventTestServer(t)
	artID, hash := registerAgedHookArtifact(t, st, "cancelled but resolved", "shadow:Bash:cancel", -2*time.Hour)

	recordFetchResolve(cancelledCtx(t), st, artID, hash, 123, 4)

	fs := fetchStats(t, storeDir)
	if fs.Resolved != 1 {
		t.Fatalf("Resolved=%d want 1 — 성공한 회수의 행이 ctx 취소로 사라졌다", fs.Resolved)
	}
	if fs.ShadowResolved != 1 {
		t.Fatalf("ShadowResolved=%d want 1 — 행은 남았는데 귀속 조회가 취소된 ctx에서 죽었다", fs.ShadowResolved)
	}
	if fs.AgeMax < 7000 || fs.AgeMax > 7400 { // 2시간 = 7200초, 실행 시각 오차 허용
		t.Fatalf("AgeMax=%d, 기대 약 7200 — 나이 조회가 취소된 ctx에서 죽었다", fs.AgeMax)
	}
}

// TestFetchMissRecordsUnderCancelledCtx — 같은 소견 F3의 미해소 쪽. 여기서는 원장 쓰기 앞의
// **판별 질의**(ArtifactHashByID)가 먼저 죽는다: 취소된 ctx에서 그것은 ErrNotFound가 아니라
// 드라이버 오류를 내므로 미해소 행은 삼켜지는 게 아니라 아예 시도되지 않는다. 미해소가 적게
// 세이면 D104의 "미해소 5건" 발화가 미달로 떨어져 편향 방향은 해소 쪽과 같다 — 창이 넉넉해
// 보인다.
func TestFetchMissRecordsUnderCancelledCtx(t *testing.T) {
	_, st, _, storeDir := newRecordEventTestServer(t)
	const absentID = 424242
	// 사전 가드: 그 id가 정말 없어야 미해소다. 있으면 이 테스트는 아무것도 안 재게 된다.
	if _, err := st.ArtifactHashByID(context.Background(), absentID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("사전 가드: artifact %d가 부재가 아니다(err=%v)", absentID, err)
	}

	recordFetchMiss(cancelledCtx(t), st, absentID, 3)

	fs := fetchStats(t, storeDir)
	if fs.Missed != 1 || fs.Resolved != 0 {
		t.Fatalf("resolved=%d missed=%d, 기대 0/1 — 미해소 행이 ctx 취소로 사라졌다", fs.Resolved, fs.Missed)
	}
}

// TestFetchLedgerCtxIsDetachedButBounded — 소견 F3의 고침이 **두 조건을 동시에** 만족하는지
// 잰다. 위 두 테스트는 앞 절반(취소가 안 옮는다)만 잡는데, 뒤 절반 없이 앞 절반만 있으면
// 죽은 요청 위에서 무한히 기다리는 쓰기가 되어 그 자체로 결함이다. 상한이 DSN pragma와 풀
// 크기에서 추론되는 값이 아니라 코드에 적힌 리터럴이라는 것이 요지다(fetchLedgerBudget).
func TestFetchLedgerCtxIsDetachedButBounded(t *testing.T) {
	lctx, cancel := fetchLedgerCtx(cancelledCtx(t))
	defer cancel()
	if err := lctx.Err(); err != nil {
		t.Fatalf("떼어 낸 ctx가 이미 죽었다: %v — 요청 취소가 그대로 옮았다", err)
	}
	dl, ok := lctx.Deadline()
	if !ok {
		t.Fatal("기한이 없다 — 죽은 요청 위의 무한 쓰기다")
	}
	if left := time.Until(dl); left <= 0 || left > fetchLedgerBudget {
		t.Fatalf("남은 기한 %v, 기대 (0, %v] — 상한이 의도한 값이 아니다", left, fetchLedgerBudget)
	}
}

// TestFetchNegativeAgeIsUnknownNotZero — 릴리스 패스 소견 F9. NTP 스텝·VM 재개·듀얼부팅이
// 벽시계를 뒤로 돌리면, 스텝 **이전에** 포착된 아티팩트의 indexed_at은 새 시계 기준으로 미래다.
// 그때 `time.Now().Unix() - at`은 음수이고, 하한이 없으면 그 값이 그대로 원장에 적힌다.
// 음수는 `ORDER BY artifact_age_s`에서 선두로 정렬되므로 p50·p90을 아래로 끌고, D104 행 5는
// 그 p90을 보존 창의 처방값으로 바꾼다 — 계측이 참값보다 **짧은** 창을 처방하게 된다.
//
// **픽스처가 세 후보를 갈라 놓는다**(indexed_at = now+30분):
//   - 하한 없음(naive): 셀 "-1800", ShadowResolved=1, AgeMax=-1800 — 분포에 들어간다
//   - 0으로 클램프:      셀 "0",     ShadowResolved=1, AgeMax=0    — "방금 포착"이라는 측정을 주장한다
//   - 미상(nil, 채택):   셀 "NULL",  ShadowResolved=0, AgeMax=0    — 측정이 없다고 인정한다
//
// 셋 다 Resolved=1이다 — 시계가 어긋났어도 바이트는 실제로 돌려줬고, 그 사실까지 버리면
// 채택 게이트가 F3과 같은 방향으로 또 낮아진다.
func TestFetchNegativeAgeIsUnknownNotZero(t *testing.T) {
	cs, st, _, storeDir := newRecordEventTestServer(t)
	artID, _ := registerAgedHookArtifact(t, st, "clock stepped back", "shadow:Bash:skew", 30*time.Minute)

	callFetch(t, cs, FetchInput{ArtifactID: artID, ByteStart: ptrTo(int64(0)), ByteEnd: ptrTo(int64(5))})

	fs := fetchStats(t, storeDir)
	if fs.Resolved != 1 || fs.Missed != 0 {
		t.Fatalf("resolved=%d missed=%d, 기대 1/0 — 시계 역행이 회수 자체를 버렸다", fs.Resolved, fs.Missed)
	}
	if got := ledgerAgeCell(t, storeDir); got != "NULL" {
		t.Fatalf("artifact_age_s=%s, 기대 NULL — 음수 나이는 아티팩트가 젊다는 증거가 아니라 시계가 움직였다는 증거다", got)
	}
	if fs.ShadowResolved != 0 || fs.AgeMax != 0 {
		t.Fatalf("ShadowResolved=%d AgeMax=%d, 기대 0/0 — 시계가 어긋난 표본이 분위수 모집단에 들어갔다", fs.ShadowResolved, fs.AgeMax)
	}
}

// ledgerAgeCell — storeDir 원장의 유일한 ctr_fetch 행에서 artifact_age_s를 문자열로 읽는다
// ("NULL" 또는 십진수). 행이 하나가 아니면 실패한다 — 이 헬퍼를 쓰는 테스트는 전용 서버에서
// 단일 회수만 한다는 전제 위에 서 있고, 그 전제가 깨지면 어느 행을 본 것인지 알 수 없다.
func ledgerAgeCell(t *testing.T, storeDir string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(storeDir, "ledger.db"))+"?mode=ro")
	if err != nil {
		t.Fatalf("ledger open: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT artifact_age_s FROM ledger WHERE tool='ctr_fetch'`)
	if err != nil {
		t.Fatalf("ledger query: %v", err)
	}
	defer rows.Close()
	var cells []string
	for rows.Next() {
		var age sql.NullInt64
		if err := rows.Scan(&age); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !age.Valid {
			cells = append(cells, "NULL")
			continue
		}
		cells = append(cells, strconv.FormatInt(age.Int64, 10))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(cells) != 1 {
		t.Fatalf("ctr_fetch 행 %d개(기대 1): %v", len(cells), cells)
	}
	return cells[0]
}

// callFetch: ctr_fetch를 한 번 부른다. 오류 응답도 정상 반환이다(IsError로 온다) — 이 테스트가
// 재는 것은 응답이 아니라 원장이다.
func callFetch(t *testing.T, cs *mcp.ClientSession, in FetchInput) {
	t.Helper()
	if _, err := cs.CallTool(context.Background(),
		&mcp.CallToolParams{Name: "ctr_fetch", Arguments: in}); err != nil {
		t.Fatalf("call ctr_fetch: %v", err)
	}
}

// fetchStats: storeDir의 원장에서 회수 실적을 읽는다(라이브 writer와 동시 열기 —
// TestRecordEventLedgerAppend가 LedgerStats로 하는 것과 같은 형태다).
func fetchStats(t *testing.T, storeDir string) store.FetchStat {
	t.Helper()
	fs, err := store.LedgerFetchStats(storeDir)
	if err != nil {
		t.Fatalf("LedgerFetchStats: %v", err)
	}
	return fs
}

func ptrTo[T any](v T) *T { return &v }
