package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/mcp"
	"github.com/wotjr1649/context-router/internal/session"
	"github.com/wotjr1649/context-router/internal/store"
)

func TestParseFlags(t *testing.T) {
	// CTR_ENABLE(D101)이 이 함수를 환경에 민감하게 만들었다 — 이 테스트가 단정하는 Enable
	// 필드가 우연한 ambient CTR_ENABLE에 흔들리지 않게 빈 값으로 고정한다.
	t.Setenv("CTR_ENABLE", "")
	tests := []struct {
		name    string
		args    []string
		want    serverFlags
		wantErr bool
	}{
		{"defaults", nil, serverFlags{Profile: []string{"search", "fetch", "transform"}, Enable: []string{"ingest", "net"}, LogLevel: "info"}, false},
		{"enable", []string{"--enable", "ingest,net"}, serverFlags{Profile: []string{"search", "fetch", "transform"}, Enable: []string{"ingest", "net"}, LogLevel: "info"}, false},
		{"unknown", []string{"--bogus"}, serverFlags{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseFlags(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			if err == nil && strings.Join(got.Profile, ",") != strings.Join(tt.want.Profile, ",") {
				t.Fatalf("profile=%v want %v", got.Profile, tt.want.Profile)
			}
			if err == nil && strings.Join(got.Enable, ",") != strings.Join(tt.want.Enable, ",") {
				t.Fatalf("enable=%v want %v", got.Enable, tt.want.Enable)
			}
		})
	}
}

// TestParseFlags_RejectsPositionalArgs: 최종리뷰 F4 — 서브커맨드가 플래그 뒤에 오타로
// 붙으면(예: "--store-root X doctor") fs.Parse는 이를 소비하지 않고 위치 인자로 남긴다.
// dispatchCLI는 args[1]("--store-root")이 "-"로 시작하므로 손대지 않고 run()으로
// 넘기는데, 예전 parseFlags는 이 잔여 위치 인자를 조용히 버려 MCP 서버가 기동해버렸다
// ("미지 서브커맨드 거부" 원칙과 반대). parseFlags가 명시적으로 거부해야 한다.
func TestParseFlags_RejectsPositionalArgs(t *testing.T) {
	if _, err := parseFlags([]string{"--store-root", "X", "doctor"}); err == nil {
		t.Fatal("want error for trailing positional arg, got nil")
	}
}

// TestParseFlags_CTR_ENABLE — D101: --enable이 빈 문자열일 때만(부재·"--enable=" 둘 다 —
// flag 패키지로는 구분 불가) CTR_ENABLE을 대신 읽고, 값이 있으면 플래그가 이긴다. 문법은
// --enable과 같고(쉼표 구분·항목별 트림·빈 항목 무시) 모르는 이름은 두 입구 모두 오류다
// (§6 — 오류 문면에 입력 원문 미포함). storeRootFor·CTR_STORE_ROOT의 t.Setenv 관례와
// 동형(TestStoreRootFor). 둘 다 없으면 D101 계약 2의 기본 프로필(ingest,net)로 돈다(v0.19
// 리뷰 C1) — 세 갈래(플래그 > 환경 변수 > 기본값) 우선순위 전체는 별도 표
// TestParseFlags_EnablePrecedence가 한 곳에서 잰다.
func TestParseFlags_CTR_ENABLE(t *testing.T) {
	t.Run("env_fills_when_flag_absent", func(t *testing.T) {
		t.Setenv("CTR_ENABLE", "ingest,net")
		got, err := parseFlags(nil)
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		want := []string{"ingest", "net"}
		if strings.Join(got.Enable, ",") != strings.Join(want, ",") {
			t.Fatalf("Enable=%v want %v", got.Enable, want)
		}
	})

	t.Run("flag_wins_over_env", func(t *testing.T) {
		t.Setenv("CTR_ENABLE", "ingest")
		got, err := parseFlags([]string{"--enable", "exec"})
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		want := []string{"exec"}
		if strings.Join(got.Enable, ",") != strings.Join(want, ",") {
			t.Fatalf("Enable=%v want %v (flag must win over CTR_ENABLE)", got.Enable, want)
		}
	})

	// v0.19 리뷰 C1(소유자 결정)이 이름과 기대값을 뒤집었다: 둘 다 없으면 이제 빈 값이 아니라
	// D101 계약 2의 기본 프로필(ingest,net)로 돈다 — plugin/mcp.json이 더는 args를 고정하지
	// 않으므로 서버 자신이 그 기본값을 갖지 않으면 CTR_ENABLE이 모든 플러그인 설치에서 죽은
	// 경로가 된다(그 근거가 C1 자체다).
	t.Run("empty_env_falls_back_to_default_profile", func(t *testing.T) {
		t.Setenv("CTR_ENABLE", "")
		got, err := parseFlags(nil)
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		want := []string{"ingest", "net"}
		if strings.Join(got.Enable, ",") != strings.Join(want, ",") {
			t.Fatalf("Enable=%v want %v (no --enable, CTR_ENABLE=\"\" → D101 계약 2 기본값)", got.Enable, want)
		}
	})

	t.Run("whitespace_normalized", func(t *testing.T) {
		t.Setenv("CTR_ENABLE", " ingest , net ")
		got, err := parseFlags(nil)
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		want := []string{"ingest", "net"}
		if strings.Join(got.Enable, ",") != strings.Join(want, ",") {
			t.Fatalf("Enable=%v want %v", got.Enable, want)
		}
	})

	t.Run("unknown_name_in_env_rejected_without_echo", func(t *testing.T) {
		t.Setenv("CTR_ENABLE", "bogus")
		_, err := parseFlags(nil)
		if err == nil {
			t.Fatal("want error for unknown CTR_ENABLE profile name, got nil")
		}
		if strings.Contains(err.Error(), "bogus") {
			t.Fatalf("error echoes user input(규약 §6 위반): %v", err)
		}
	})

	t.Run("unknown_name_in_flag_rejected_without_echo", func(t *testing.T) {
		t.Setenv("CTR_ENABLE", "")
		_, err := parseFlags([]string{"--enable", "bogus"})
		if err == nil {
			t.Fatal("want error for unknown --enable profile name, got nil")
		}
		if strings.Contains(err.Error(), "bogus") {
			t.Fatalf("error echoes user input(규약 §6 위반): %v", err)
		}
	})
}

// TestParseFlags_EnablePrecedence — v0.19 리뷰 C1·재검토 리뷰 2. 세 값의 우선순위(--enable >
// CTR_ENABLE > defaultEnableProfile)를 한 표에서 잰다. C1 이전에는 셋째 갈래(둘 다 없음)가 빈
// 값으로 떨어져 plugin/mcp.json의 고정 args가 CTR_ENABLE을 항상 이겼다 — 그 표를 D101 계약
// 2가 요구하는 기본값으로 뒤집는 것이 이 테스트의 대상이다. 마지막 두 행은 재검토 리뷰 2 —
// 공백뿐이거나 쉼표뿐인 값은 원본 문자열이 비어 있지 않아도 파싱하면 이름이 하나도 안 남는다
// (parseEnableList가 그런 값을 오류 없이 빈 슬라이스로 돌려준다) — 그 상태가 "그 단계는
// 쓸모없다"로 다음 단계에 넘어가는지를 각각 env·flag 자리에서 확인한다.
func TestParseFlags_EnablePrecedence(t *testing.T) {
	cases := []struct {
		name    string
		flag    string
		env     string
		wantOut []string
	}{
		{"둘 다 없음 → 기본값", "", "", []string{"ingest", "net"}},
		{"env만 있음 → env", "", "exec", []string{"exec"}},
		{"flag만 있음 → flag", "ingest", "", []string{"ingest"}},
		{"둘 다 있음 → flag가 이긴다", "exec", "ingest,net", []string{"exec"}},
		{"env가 공백/쉼표뿐 → 기본값", "", "  , , ", []string{"ingest", "net"}},
		{"flag가 쉼표뿐 → env로 폴백", ",", "exec", []string{"exec"}},
		// 릴리스 리뷰 F2: none은 값을 준 단계다 — 다음 단계로 넘어가지 않는다. 마지막 두 행이
		// 우선순위 표의 그 갈래다(flag none이 값 있는 env를 이기고, env none이 기본값을 이긴다).
		{"flag가 none → 프로필 0개", "none", "ingest,net", nil},
		{"env가 none → 프로필 0개", "", "none", nil},
		{"none 좌우 공백도 같다", " none ", "ingest", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CTR_ENABLE", c.env)
			var args []string
			if c.flag != "" {
				args = []string{"--enable", c.flag}
			}
			got, err := parseFlags(args)
			if err != nil {
				t.Fatalf("err=%v", err)
			}
			if strings.Join(got.Enable, ",") != strings.Join(c.wantOut, ",") {
				t.Fatalf("Enable=%v want %v", got.Enable, c.wantOut)
			}
		})
	}
}

// TestParseFlags_EnableNone — 릴리스 리뷰 F2. `--enable=`·`CTR_ENABLE=""`는 "이 단계는 값을
// 주지 않았다"라서 기본값 ingest,net으로 떨어지고, 그래서 v0.19 이전 자세(색인 쓰기도
// 아웃바운드 HTTP도 없음)를 요청할 자리가 없었다 — 이름 none이 그 자리다. 다른 이름과 섞인
// 값은 오류이고(두 해석이 다 성립한다), 문면은 허용 값만 나열한다(규약 §6 입력 원문 에코
// 금지). 우선순위 자체는 TestParseFlags_EnablePrecedence의 표가 잰다.
func TestParseFlags_EnableNone(t *testing.T) {
	t.Run("배너가 전부 off로 읽힌다", func(t *testing.T) {
		t.Setenv("CTR_ENABLE", "ingest,net")
		got, err := parseFlags([]string{"--enable", "none"})
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if len(got.Enable) != 0 {
			t.Fatalf("Enable=%v want 빈 목록", got.Enable)
		}
		line := banner(got, "/p")
		if !strings.Contains(line, "ingest=off net=off") {
			t.Fatalf("banner=%q want ingest=off net=off", line)
		}
	})

	for _, c := range []struct{ name, flag, env string }{
		{"flag에서 none과 다른 이름 혼합", "none,ingest", ""},
		{"flag에서 순서를 바꿔도 같다", "ingest,none", ""},
		{"env에서 none과 다른 이름 혼합", "", "net,none"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CTR_ENABLE", c.env)
			var args []string
			if c.flag != "" {
				args = []string{"--enable", c.flag}
			}
			_, err := parseFlags(args)
			if err == nil {
				t.Fatal("want error for none mixed with another profile name, got nil")
			}
			raw := c.flag + c.env
			if strings.Contains(err.Error(), raw) {
				t.Fatalf("error echoes user input(규약 §6 위반): %v", err)
			}
			for _, name := range append(slices.Clone(enableProfileNames), enableProfileNone) {
				if !strings.Contains(err.Error(), name) {
					t.Fatalf("error=%q missing allowed value %q", err, name)
				}
			}
		})
	}
}

func TestParseFlagsNet(t *testing.T) {
	got, err := parseFlags([]string{"--net-allow-local", "--net-ports", "8080,9090"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !got.NetAllowLocal {
		t.Fatalf("NetAllowLocal=false want true")
	}
	if len(got.NetPorts) != 2 || got.NetPorts[0] != 8080 || got.NetPorts[1] != 9090 {
		t.Fatalf("NetPorts=%v want [8080 9090]", got.NetPorts)
	}
}

// TestParseFlags_Projects: --projects는 콤마 구분·공백 트림으로 분해되고, 미지정 시
// 빈 값이어야 한다(설계 §8).
func TestParseFlags_Projects(t *testing.T) {
	got, err := parseFlags([]string{"--projects", "proj-a, proj-b ,proj-c"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	want := []string{"proj-a", "proj-b", "proj-c"}
	if strings.Join(got.Projects, ",") != strings.Join(want, ",") {
		t.Fatalf("Projects=%v want %v", got.Projects, want)
	}

	def, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(def.Projects) != 0 {
		t.Fatalf("default Projects=%v want empty", def.Projects)
	}
}

// TestParseRetentionEventsFlag — 브리프 Step1 ⑥: time.ParseDuration 표준 동작 그대로("720h"
// OK, "30d"는 커스텀 단위라 오류) + 기본 off(빈 문자열=0)·음수 거부(설계 §5).
func TestParseRetentionEventsFlag(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"default_empty_is_off", "", 0, false},
		{"720h_ok", "720h", 720 * time.Hour, false},
		{"30d_rejected_no_custom_units", "30d", 0, true},
		{"negative_rejected", "-1h", 0, true},
		{"garbage_rejected", "abc", 0, true},
		{"sub_second_positive_rejected", "500ms", 0, true}, // D4: 양수 sub-초는 0 절삭(무기한) 대신 거부
		{"exactly_one_second_ok", "1s", time.Second, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRetentionEventsFlag(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("got=%v want=%v", got, tt.want)
			}
		})
	}
}

// TestRetentionSecFromDuration: --retention-events → session.Options.RetentionSec(초) 변환.
func TestRetentionSecFromDuration(t *testing.T) {
	if got := retentionSecFromDuration(720 * time.Hour); got != 720*3600 {
		t.Fatalf("got=%d want %d", got, 720*3600)
	}
	if got := retentionSecFromDuration(0); got != 0 {
		t.Fatalf("got=%d want 0", got)
	}
}

// TestParseFlags_RetentionEvents: --retention-events가 parseFlags를 거쳐 serverFlags에
// 실리고, 파싱 실패("30d")는 parseFlags 자체를 기동 거부시킨다(설계 §5).
func TestParseFlags_RetentionEvents(t *testing.T) {
	got, err := parseFlags([]string{"--retention-events", "720h"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got.RetentionEvents != 720*time.Hour {
		t.Fatalf("RetentionEvents=%v want 720h", got.RetentionEvents)
	}

	if _, err := parseFlags([]string{"--retention-events", "30d"}); err == nil {
		t.Fatal("want error for \"30d\" (time.ParseDuration 표준 동작 — 커스텀 단위 미지원)")
	}
}

// TestSweepSessionRetentionAtStart_LogsCountOnSuccess: 세션 DB가 열려 있을 때 시작 시 1회
// 스윕 헬퍼가 삭제 건수를 stderr 1줄로 고지한다(설계 §5 "조용한 삭제 금지").
func TestSweepSessionRetentionAtStart_LogsCountOnSuccess(t *testing.T) {
	dir := t.TempDir()
	d, err := session.Open(dir, session.Options{Producer: "test", RetentionSec: 1})
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	defer d.Close()

	var stderr bytes.Buffer
	sweepSessionRetentionAtStart(context.Background(), d, time.Now().Add(time.Hour), &stderr)
	if !strings.Contains(stderr.String(), "session retention sweep") {
		t.Fatalf("stderr=%q want mention of sweep result", stderr.String())
	}
}

// TestSweepSessionRetentionAtStart_LogAndContinueOnFailure: Sweep이 실패해도(취소된 ctx로
// 강제) 헬퍼는 오류를 반환하지 않고(반환값 없음 시그니처) stderr에만 실패를 남긴다
// (log-and-continue, 설계 §5 "시작을 막지 않는다").
func TestSweepSessionRetentionAtStart_LogAndContinueOnFailure(t *testing.T) {
	dir := t.TempDir()
	d, err := session.Open(dir, session.Options{Producer: "test", RetentionSec: 1})
	if err != nil {
		t.Fatalf("session.Open: %v", err)
	}
	defer d.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 즉시 취소 — Sweep이 오류를 반환하도록 강제

	var stderr bytes.Buffer
	sweepSessionRetentionAtStart(ctx, d, time.Now(), &stderr)
	if !strings.Contains(stderr.String(), "실패") {
		t.Fatalf("stderr=%q want 실패 문구(log-and-continue)", stderr.String())
	}
}

// TestRun_GlobalProfile_RequiresProjects: --profile global-search인데 --projects
// 미지정이면 시작을 거부해야 한다(설계 §4.6 "기본값 없음").
func TestRun_GlobalProfile_RequiresProjects(t *testing.T) {
	var stderr bytes.Buffer
	err := run(context.Background(), []string{"--profile", "global-search", "--store-root", t.TempDir()}, &stderr)
	if err == nil {
		t.Fatal("want error for global-search profile without --projects, got nil")
	}
}

// TestRun_DefaultProfile_RejectsProjects: 기본 프로필에서 --projects 지정은 모호성
// 차단을 위해 오류로 거부해야 한다(설계 §4.6/§8).
func TestRun_DefaultProfile_RejectsProjects(t *testing.T) {
	var stderr bytes.Buffer
	err := run(context.Background(), []string{"--root", t.TempDir(), "--store-root", t.TempDir(), "--projects", "some-id"}, &stderr)
	if err == nil {
		t.Fatal("want error for default profile with --projects, got nil")
	}
}

// TestRun_ArbitraryProfileSubset_Rejected: mcp.NewServer는 Profile로 도구를 게이팅하지
// 않으므로(v0.0.1 예약), 기본 3종·global-search 단독 외의 임의 부분집합을 조용히 받으면
// 사용자가 "일부만 켰다"고 오인한다 — 시작 시점에 명시 오류로 거부해야 한다(Codex 교차
// 리뷰 P1-2, 설계 §2.1 "등록됨 = 보안 경계").
func TestRun_ArbitraryProfileSubset_Rejected(t *testing.T) {
	var stderr bytes.Buffer
	err := run(context.Background(), []string{"--profile", "search", "--root", t.TempDir(), "--store-root", t.TempDir()}, &stderr)
	if err == nil {
		t.Fatal("want error for --profile search (arbitrary subset), got nil")
	}
}

// TestRun_GlobalProfile_OpenFailureRejectsStart: --projects 엔트리 중 store가 아직 없는
// (디렉터리/DB 없음) 것이 하나라도 있으면 시작 전체를 거부해야 한다(fail-closed, 설계 §4.6).
func TestRun_GlobalProfile_OpenFailureRejectsStart(t *testing.T) {
	var stderr bytes.Buffer
	err := run(context.Background(), []string{
		"--profile", "global-search", "--store-root", t.TempDir(), "--projects", "nonexistent-project-id",
	}, &stderr)
	if err == nil {
		t.Fatal("want error for missing project store, got nil")
	}
}

// TestResolveProjectEntry_StoreIDNotShadowedByCwdDir: 최종리뷰 F5 — cli.purgeProjectID의
// 동일 회귀 케이스와 대응: cwd에 store ProjectID와 동명의 디렉터리가 우연히 있어도
// --projects 엔트리는 store 쪽 프로젝트 ID로 확정돼야 한다(예전엔 "구분자 없고 cwd에
// 동명 디렉터리 존재"를 경로로 오인해 ident.Canonicalize(그 cwd 디렉터리)로 완전히 다른
// ID를 계산해버렸다).
func TestResolveProjectEntry_StoreIDNotShadowedByCwdDir(t *testing.T) {
	storeRoot := t.TempDir()
	registeredRoot := t.TempDir()
	canon, err := ident.Canonicalize(registeredRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	id := canon.ProjectID
	if err := os.MkdirAll(filepath.Join(storeRoot, "projects", id), 0o755); err != nil {
		t.Fatal(err)
	}

	cwdBase := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwdBase, id), 0o755); err != nil {
		t.Fatal(err)
	}
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwdBase); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(origWD) })

	gotID, gotRoot, err := resolveProjectEntry(storeRoot, id)
	if err != nil {
		t.Fatalf("resolveProjectEntry: %v", err)
	}
	if gotID != id {
		t.Fatalf("gotID=%q want %q (store ID가 cwd 동명 디렉터리에 가려짐)", gotID, id)
	}
	if gotRoot != "" {
		t.Fatalf("gotRoot=%q want empty(ID 엔트리는 root 상대화 없음)", gotRoot)
	}
}

// TestBuildGlobalProjects_DedupesRepeatedEntries: 최종리뷰 F5 — 같은 프로젝트를 경로 형태와
// ProjectID 형태로 두 번 --projects에 주면 store는 한 번만 열리고 결과에 1개만 남아야
// 한다(중복 hit 방지).
func TestBuildGlobalProjects_DedupesRepeatedEntries(t *testing.T) {
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()
	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	projDir := filepath.Join(storeRoot, "projects", canon.ProjectID)
	st, err := store.Open(projDir, false)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	projects, err := buildGlobalProjects(context.Background(), storeRoot, []string{projectRoot, canon.ProjectID})
	if err != nil {
		t.Fatalf("buildGlobalProjects: %v", err)
	}
	defer func() {
		for _, p := range projects {
			p.Store.Close()
		}
	}()
	if len(projects) != 1 {
		t.Fatalf("len(projects)=%d want 1 (중복 --projects가 dedupe되지 않음): %+v", len(projects), projects)
	}
}

func TestBanner(t *testing.T) {
	f := serverFlags{Profile: []string{"search", "fetch", "transform"}, LogLevel: "info"}
	got := banner(f, "C:/proj")
	want := "[ctr] v" + version + " profile=search,fetch,transform ingest=off net=off root=C:/proj"
	if got != want {
		t.Fatalf("banner=%q want %q", got, want)
	}
	f2 := serverFlags{Profile: []string{"search"}, Enable: []string{"ingest"}, LogLevel: "info"}
	got2 := banner(f2, "/p")
	want2 := "[ctr] v" + version + " profile=search ingest=on net=off root=/p"
	if got2 != want2 {
		t.Fatalf("banner on-branch=%q want %q", got2, want2)
	}
}

func TestCanonicalizeAllowPaths(t *testing.T) {
	storeRoot := t.TempDir()
	inside := filepath.Join(storeRoot, "projects", "p1")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	outside := t.TempDir()

	if _, err := canonicalizeAllowPaths([]string{inside}, storeRoot); !errors.Is(err, errAllowPathViolation) {
		t.Fatalf("inside store-root: err=%v want errAllowPathViolation", err)
	}
	got, err := canonicalizeAllowPaths([]string{outside}, storeRoot)
	if err != nil {
		t.Fatalf("outside store-root: unexpected err=%v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got=%v want 1 entry", got)
	}
}

func TestCanonicalizeAllowPaths_NonexistentPathErrors(t *testing.T) {
	storeRoot := t.TempDir()
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := canonicalizeAllowPaths([]string{missing}, storeRoot); err == nil {
		t.Fatal("want error for nonexistent allow-path, got nil")
	}
}

func TestCanonicalizeStoreRoot_RelativeBecomesAbsolute(t *testing.T) {
	got, err := canonicalizeStoreRoot("relative-store-dir-does-not-exist")
	if err != nil {
		t.Fatalf("unexpected err=%v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("got=%q want absolute path", got)
	}
}

func TestStoreRootFor(t *testing.T) {
	t.Run("flag_wins", func(t *testing.T) {
		got, err := storeRootFor(serverFlags{StoreRoot: "C:/custom"})
		if err != nil || got != "C:/custom" {
			t.Fatalf("got=%q err=%v", got, err)
		}
	})
	t.Run("env_wins_over_default", func(t *testing.T) {
		t.Setenv("CTR_STORE_ROOT", "C:/from-env")
		got, err := storeRootFor(serverFlags{})
		if err != nil || got != "C:/from-env" {
			t.Fatalf("got=%q err=%v", got, err)
		}
	})
}

// TestMainDispatch_CLI: doctor 서브커맨드가 dispatchCLI를 거쳐 storeRoot(미생성)·프로젝트
// 디렉터리에서 정상 종료하는지 확인한다(Task3, 설계 §7). run()이 아닌 dispatchCLI를 직접
// 호출해 os.Exit 경로 없이 검증한다.
func TestMainDispatch_CLI(t *testing.T) {
	proj := t.TempDir()
	storeRoot := filepath.Join(t.TempDir(), "storeroot") // 의도적 미생성
	args := []string{"context-router", "doctor", "--root", proj, "--store-root", storeRoot}
	handled, err := dispatchCLI(context.Background(), args)
	if !handled {
		t.Fatal("want handled=true for doctor subcommand")
	}
	if err != nil {
		t.Fatalf("doctor dispatch err=%v", err)
	}
}

// TestMainDispatch_HookPreprocFailOpen(최종 리뷰 C3): 실행 훅의 root 플래그 파싱 실패(값 없는
// --store-root)는 fail-open으로 흡수돼 err=nil(exit 0)이어야 한다(D23 — settings에 잔존한
// 잘못된 훅 명령이 호스트를 막으면 안 됨). install은 같은 실패를 그대로 전파한다(사용자 대면).
func TestMainDispatch_HookPreprocFailOpen(t *testing.T) {
	handled, err := dispatchCLI(context.Background(), []string{"context-router", "hook", "--store-root"})
	if !handled || err != nil {
		t.Fatalf("running hook: handled=%v err=%v want true/nil(fail-open)", handled, err)
	}
	handled, err = dispatchCLI(context.Background(), []string{"context-router", "hook", "install", "--store-root"})
	if !handled || err == nil {
		t.Fatalf("install: handled=%v err=%v want true/non-nil(전파)", handled, err)
	}
}

// TestMainDispatch_Session: "session" 서브커맨드가 cliSubcommands를 통과해 cli.Run까지
// 위임되는지 확인한다(태스크9a, 설계 §7 — main.go: sub "session" 허용). 이 프로젝트에는
// worktree가 없어 export 자체는 실패하지만(handled=true·err!=nil), 그 오류가 "미지
// 서브커맨드"가 아니어야 한다 — dispatchCLI가 session을 정상적으로 cli.Run에 위임했다는
// 증거(recover 등 하위 서브커맨드 자리는 cli.Run 내부 소관, 여기서는 최상위 라우팅만 검증).
func TestMainDispatch_Session(t *testing.T) {
	proj := t.TempDir()
	storeRoot := filepath.Join(t.TempDir(), "storeroot")
	args := []string{"context-router", "session", "export", "--project", proj, "--root", proj, "--store-root", storeRoot}
	handled, err := dispatchCLI(context.Background(), args)
	if !handled {
		t.Fatal("want handled=true for session subcommand")
	}
	if err == nil {
		t.Fatal("want error (no worktree exists yet), got nil")
	}
	if strings.Contains(err.Error(), "미지 서브커맨드") {
		t.Fatalf("session must not be rejected as unknown subcommand: %v", err)
	}
}

// TestMainDispatch_Usage: "usage" 서브커맨드가 cliSubcommands를 통과해 cli.Run까지 위임되는지
// 확인한다(태스크9, 설계 §6·§7). transcript 디렉터리가 없어 usage 자체는 실패하지만
// (handled=true·err!=nil), 그 오류가 "미지 서브커맨드"가 아니어야 한다 — dispatchCLI가 usage를
// 정상적으로 cli.Run에 위임했다는 증거(TestMainDispatch_Session과 동형).
func TestMainDispatch_Usage(t *testing.T) {
	proj := t.TempDir()
	storeRoot := filepath.Join(t.TempDir(), "storeroot")
	missing := filepath.Join(t.TempDir(), "no-transcripts")
	args := []string{"context-router", "usage", "--transcripts", missing, "--root", proj, "--store-root", storeRoot}
	handled, err := dispatchCLI(context.Background(), args)
	if !handled {
		t.Fatal("want handled=true for usage subcommand")
	}
	if err == nil {
		t.Fatal("want error (missing transcripts dir), got nil")
	}
	if strings.Contains(err.Error(), "미지 서브커맨드") {
		t.Fatalf("usage must not be rejected as unknown subcommand: %v", err)
	}
}

// TestMainDispatch_NotHandled: 서브커맨드가 아닌(MCP 서버용) 인자는 dispatchCLI가 손대지
// 않아야 한다 — 미지 단어가 cli로 잘못 흡수되지 않는지의 반대쪽 보증(설계 §7).
func TestMainDispatch_NotHandled(t *testing.T) {
	handled, err := dispatchCLI(context.Background(), []string{"context-router", "--profile", "search"})
	if handled {
		t.Fatalf("want handled=false, err=%v", err)
	}
}

// TestMainDispatch_UnknownSubcommandRejected: "-"로 시작하지 않으면서 cliSubcommands 중
// 어느 것도 아닌 첫 인자(예: "stats"의 오타 "stat")는 조용히 MCP 서버 경로로 흘러가면 안 된다 —
// handled=true와 명시 오류를 반환해야 한다(리뷰 Fix Round 3, item 1). 진짜 서버 플래그
// (--profile 등, "-" 시작)는 여전히 handled=false로 통과한다(TestMainDispatch_NotHandled).
func TestMainDispatch_UnknownSubcommandRejected(t *testing.T) {
	handled, err := dispatchCLI(context.Background(), []string{"context-router", "stat"})
	if !handled {
		t.Fatal("want handled=true for unknown non-flag first arg (typo'd subcommand)")
	}
	if err == nil {
		t.Fatal("want error for unknown subcommand-like arg, got nil")
	}
}

// TestPrescanRootFlags: dispatchCLI가 서버 전체 flagset(parseFlags) 재사용을 그만두고 쓰는
// 경량 프리스캔 — "--f v"/"--f=v"/"-f v" 세 형태 모두에서 --root/--store-root만 뽑고
// 나머지(서브커맨드 전용 플래그, 예: --provider)는 손대지 않아야 한다(Task4 Fix Round 1).
// missing_value_looks_like_flag/missing_value_at_end: "--f v" 형태에서 다음 토큰이 없거나
// 다른 플래그처럼 보이면(- 접두사) 값으로 삼키지 않고 오류를 반환해야 한다 — 그렇지 않으면
// `stats --root --provider p` 오타가 --provider를 --root의 값으로 조용히 삼킨다(리뷰 Fix
// Round 2, Important-1).
func TestPrescanRootFlags(t *testing.T) {
	tests := []struct {
		name                    string
		args                    []string
		wantRoot, wantStoreRoot string
		wantRest                []string
		wantErr                 bool
	}{
		{"space_form", []string{"--root", "R", "--store-root", "S"}, "R", "S", []string{}, false},
		{"eq_form", []string{"--root=R", "--store-root=S", "--provider", "p"}, "R", "S", []string{"--provider", "p"}, false},
		{"single_dash", []string{"-root", "R"}, "R", "", []string{}, false},
		{"no_root_flags", []string{"--provider", "p"}, "", "", []string{"--provider", "p"}, false},
		{"root_flags_interleaved", []string{"--provider", "p", "--root", "R"}, "R", "", []string{"--provider", "p"}, false},
		{"missing_value_looks_like_flag", []string{"--root", "--provider", "p"}, "", "", nil, true},
		{"missing_value_at_end", []string{"--root"}, "", "", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, storeRoot, rest, err := prescanRootFlags(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (root=%q storeRoot=%q rest=%v)", root, storeRoot, rest)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err=%v", err)
			}
			if root != tt.wantRoot || storeRoot != tt.wantStoreRoot {
				t.Fatalf("root=%q storeRoot=%q want %q/%q", root, storeRoot, tt.wantRoot, tt.wantStoreRoot)
			}
			if strings.Join(rest, ",") != strings.Join(tt.wantRest, ",") {
				t.Fatalf("rest=%v want %v", rest, tt.wantRest)
			}
		})
	}
}

// captureStdout: fn 실행 동안 프로세스 전역 os.Stdout을 파이프로 바꿔 출력을 문자열로
// 받는다. dispatchCLI가 os.Stdout을 하드코딩해 cli.Run에 넘기므로(Task3 이관 인지 사항)
// dispatchCLI 레벨에서 실제 출력 내용을 확인하려면 이 방법뿐이다 — 병렬 테스트(t.Parallel)와
// 섞이지 않는 한 안전하다(이 파일은 어떤 테스트도 병렬 실행하지 않는다).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = orig
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(out)
}

// TestMainDispatch_Hook: "hook" 서브커맨드가 cliSubcommands를 통과해 cli.Run까지 위임되는지
// 확인한다(v0.2 설계 §2 — main.go: sub "hook" 등재). internal/hook만 테스트하면 맵 등재
// 누락에도 GREEN이 되는 사각을 막는다(브리프 ⑨). hook.Run은 stdin을 읽으므로(fail-open) os.Stdin을
// 즉시 EOF인 파이프로 잠시 대체해 ReadAll이 블록하지 않게 한다 — 빈 stdin은 bad-input drop 후
// exit 0(→ cli.Run nil)이라 dispatchCLI는 handled=true·err=nil을 반환해야 한다.
func TestMainDispatch_Hook(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "storeroot")

	origStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	_ = w.Close() // 즉시 EOF
	os.Stdin = r
	defer func() { os.Stdin = origStdin; _ = r.Close() }()

	handled, err := dispatchCLI(context.Background(), []string{"context-router", "hook", "--store-root", storeRoot})
	if !handled {
		t.Fatal("want handled=true for hook subcommand")
	}
	if err != nil {
		t.Fatalf("hook dispatch err=%v (must not be rejected as unknown subcommand)", err)
	}
}

// TestMainDispatch_Hook_AbsorbsPreprocError: F2 — 실행 훅(install/uninstall 아님)의 전처리
// 실패는 exit 1이 아니라 흡수돼야 한다(설계 §2 always-exit-0). store-root 기본값 도출을 OS별
// env를 비워 실패시키는 seam으로 주입하고, dispatchCLI가 handled=true·err=nil을 반환하는지 본다.
// install은 종전대로 오류를 전파해야 한다(흡수 대상은 실행 훅 한정).
func TestMainDispatch_Hook_AbsorbsPreprocError(t *testing.T) {
	// storeRootFor → defaultStoreRoot 실패 강제(3-OS): windows LOCALAPPDATA, linux XDG/HOME,
	// darwin HOME 모두 비운다(CTR_STORE_ROOT도 비워 env 우선순위 우회 차단).
	t.Setenv("CTR_STORE_ROOT", "")
	t.Setenv("LOCALAPPDATA", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "")

	t.Run("running_hook_absorbs", func(t *testing.T) {
		origStdin := os.Stdin
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("pipe: %v", err)
		}
		_ = w.Close() // 즉시 EOF — 흡수 경로가 stdin을 drain
		os.Stdin = r
		defer func() { os.Stdin = origStdin; _ = r.Close() }()

		handled, err := dispatchCLI(context.Background(), []string{"context-router", "hook"})
		if !handled {
			t.Fatal("want handled=true for hook subcommand")
		}
		if err != nil {
			t.Fatalf("running hook preproc error must be absorbed (exit 0), got err=%v", err)
		}
	})

	t.Run("install_still_errors", func(t *testing.T) {
		handled, err := dispatchCLI(context.Background(), []string{"context-router", "hook", "install", "--root", t.TempDir()})
		if !handled {
			t.Fatal("want handled=true for hook install")
		}
		if err == nil {
			t.Fatal("install preproc error must NOT be absorbed, got nil")
		}
	})
}

// TestMainDispatch_HookInstall_GuidanceOnly: D96·D97 — 실경로(dispatchCLI)에서 `hook install`이
// 안내를 내고 비-0으로 끝나며 **프로젝트에 아무 파일도 만들지 않는다**. 옛 플래그(--store-root
// 등)가 붙어도 같다 — 그 값들을 나르던 배선이 지워졌으므로 여기서 갈릴 자리가 없다.
func TestMainDispatch_HookInstall_GuidanceOnly(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--store-root", filepath.Join(t.TempDir(), "storeroot")},
		{"--codex", "--user"},
	} {
		proj := t.TempDir()
		argv := append([]string{"context-router", "hook", "install", "--root", proj}, args...)
		var handled bool
		var derr error
		out := captureStdout(t, func() {
			handled, derr = dispatchCLI(context.Background(), argv)
		})
		if !handled {
			t.Fatalf("args=%v want handled=true", args)
		}
		if derr == nil {
			t.Fatalf("args=%v: hook install이 비-0으로 끝나지 않았다", args)
		}
		if !strings.Contains(out, "옛 등록물을 먼저 지운다") {
			t.Errorf("args=%v: 0번 걸음 안내가 없다:\n%s", args, out)
		}
		entries, err := os.ReadDir(proj)
		if err != nil {
			t.Fatalf("readdir: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("args=%v: hook install이 프로젝트에 파일을 만들었다: %v", args, entries)
		}
	}
}

// TestMainDispatch_Stats_Provider: 실제 dispatchCLI 경로로 `stats --provider <jsonl>`이
// (--root/--store-root 없이) 끝까지 동작해 실측 토큰 합계를 출력하는지 확인한다(설계 §7 —
// Task4 Fix Round 1: 이전에는 main의 서버 flagset이 --provider를 몰라 여기서 항상
// "flag provided but not defined"로 실패했었다).
func TestMainDispatch_Stats_Provider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	line := `{"message":{"usage":{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	var handled bool
	var dispatchErr error
	out := captureStdout(t, func() {
		handled, dispatchErr = dispatchCLI(context.Background(), []string{"context-router", "stats", "--provider", path})
	})
	if !handled {
		t.Fatal("want handled=true for stats subcommand")
	}
	if dispatchErr != nil {
		t.Fatalf("stats --provider dispatch err=%v out=%s", dispatchErr, out)
	}
	for _, want := range []string{"input_tokens: 7", "output_tokens: 3", "usage records: 1", "skipped: 0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("out missing %q: %s", want, out)
		}
	}
}

// TestMainDispatch_Stats_WithStoreRoot: `stats --root <proj> --store-root <dir>`(플래그 조합)이
// 실제 dispatchCLI를 거쳐 로컬 ledger 표를 출력하는지 확인한다 — prescanRootFlags가 값을
// 뽑아 storeRootFor+canonicalizeStoreRoot에 넘기는 경로의 회귀 테스트.
func TestMainDispatch_Stats_WithStoreRoot(t *testing.T) {
	proj := t.TempDir()
	storeRoot := filepath.Join(t.TempDir(), "storeroot")

	var handled bool
	var dispatchErr error
	out := captureStdout(t, func() {
		handled, dispatchErr = dispatchCLI(context.Background(), []string{
			"context-router", "stats", "--root", proj, "--store-root", storeRoot,
		})
	})
	if !handled {
		t.Fatal("want handled=true for stats subcommand")
	}
	if dispatchErr != nil {
		t.Fatalf("stats dispatch err=%v out=%s", dispatchErr, out)
	}
	if !strings.Contains(out, "bytes suppressed (local, 진단용)") {
		t.Fatalf("out missing fixed suppression phrase: %s", out)
	}
}

// TestMainDispatch_CLI_Upgrade: doctor에 이어 upgrade도 새 dispatchCLI(프리스캔) 경로로
// 여전히 정상 라우팅되는지 확인한다(회귀) — 네트워크는 절대 건드리지 않는다(리뷰 Fix Round 3,
// item 6: 예전엔 실제 releaseURL(GitHub API)까지 client.Get으로 도달해 오프라인 환경에서
// DNS/연결 타임아웃에 의존했다). "upgrade" 뒤에 미지 인자를 하나 붙여 cli.Run의
// unexpected-args 검사(네트워크 호출보다 먼저 실행됨)에서 오류로 반환되는 경로만으로
// dispatchCLI가 "upgrade"를 cli.Run에 제대로 넘기는지 검증한다 — runUpgrade 자체의
// 네트워크 정책(현재/최신 버전, 실패 시 폴백 등)은 internal/cli의 TestRunUpgrade_Table이
// httptest 서버를 주입해 이미 결정적으로 커버한다.
func TestMainDispatch_CLI_Upgrade(t *testing.T) {
	handled, err := dispatchCLI(context.Background(), []string{"context-router", "upgrade", "bogus-arg"})
	if !handled {
		t.Fatal("want handled=true for upgrade subcommand")
	}
	if err == nil {
		t.Fatal("want error for upgrade with unexpected arg (routing check — network never reached)")
	}
}

// TestMainDispatch_Version_NoStoreRootResolution: F1(최종 리뷰) — version은 CI/패키징
// 메타데이터 명령이라 cwd/store-root/env 해석 이전에 조기 디스패치돼야 한다. store-root
// 도출을 깨는 env(3-OS: LOCALAPPDATA/XDG/HOME + CTR_STORE_ROOT 비움, 최소 환경 모사)를
// 주입해도 version은 store-root 실패로 죽지 않고 정확히 버전 1줄만 출력해야 한다 — 예전엔
// storeRootFor→defaultStoreRoot 실패가 그대로 전파돼 err!=nil이었다(TestMainDispatch_Hook_
// AbsorbsPreprocError와 동일 env-강제 관례).
func TestMainDispatch_Version_NoStoreRootResolution(t *testing.T) {
	t.Setenv("CTR_STORE_ROOT", "")
	t.Setenv("LOCALAPPDATA", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "")

	var handled bool
	var derr error
	out := captureStdout(t, func() {
		handled, derr = dispatchCLI(context.Background(), []string{"context-router", "version"})
	})
	if !handled {
		t.Fatal("want handled=true for version subcommand")
	}
	if derr != nil {
		t.Fatalf("version must not fail on store-root resolution, got err=%v", derr)
	}
	if out != version+"\n" {
		t.Fatalf("stdout=%q want %q", out, version+"\n")
	}
}

// --- E2E stdio 스모크 (Task 9, 설계 §12-7·10 기초) ---
//
// 손수 프레이밍한 JSON-RPC로 실바이너리와 stdin/stdout 파이프를 주고받는다(SDK
// 클라이언트 미사용 — 프로토콜 오염을 외부 관찰자 시점에서 직접 검증하기 위함).
// go-sdk StdioTransport 실동작(internal/jsonrpc2 wire.go) 확인 결과 Content-Length
// 프레이밍 없이 개행 구분 JSON 한 줄당 메시지 하나.

type wireMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *wireError      `json:"error,omitempty"`
}

type wireError struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

// stdioClient: 최소 JSON-RPC 클라이언트. 고루틴에서도 안전하게 쓰도록 *testing.T를
// 붙잡지 않고 전부 error를 반환한다(Fatal은 테스트 고루틴에서만 호출 가능하므로).
type stdioClient struct {
	stdin  io.WriteCloser
	scan   *bufio.Scanner
	nextID int
}

func newStdioClient(stdin io.WriteCloser, stdout io.Reader) *stdioClient {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	return &stdioClient{stdin: stdin, scan: sc}
}

func (c *stdioClient) writeLine(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = c.stdin.Write(b)
	return err
}

// readLine reads one stdout line and requires it to be valid JSON — the direct
// check for zero protocol pollution on stdout (설계 §5.5).
func (c *stdioClient) readLine() (wireMsg, error) {
	if !c.scan.Scan() {
		if err := c.scan.Err(); err != nil {
			return wireMsg{}, err
		}
		return wireMsg{}, io.ErrUnexpectedEOF
	}
	line := c.scan.Bytes()
	var m wireMsg
	if err := json.Unmarshal(line, &m); err != nil {
		return wireMsg{}, fmt.Errorf("stdout line is not valid JSON (protocol pollution): %q: %w", line, err)
	}
	return m, nil
}

func (c *stdioClient) notify(method string, params any) error {
	return c.writeLine(wireMsg{JSONRPC: "2.0", Method: method, Params: params})
}

// call sends a request and returns the id-matched response, skipping any
// unexpected notifications read in between.
func (c *stdioClient) call(method string, params any) (wireMsg, error) {
	c.nextID++
	id := c.nextID
	if err := c.writeLine(wireMsg{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		return wireMsg{}, err
	}
	for {
		resp, err := c.readLine()
		if err != nil {
			return wireMsg{}, err
		}
		if resp.ID == id {
			if resp.Error != nil {
				return resp, fmt.Errorf("%s: rpc error [%d] %s", method, resp.Error.Code, resp.Error.Message)
			}
			return resp, nil
		}
	}
}

type toolCallResult struct {
	IsError           bool            `json:"isError"`
	StructuredContent json.RawMessage `json:"structuredContent"`
}

// callTool invokes name via tools/call and decodes structuredContent into out
// (out may be nil to skip decoding).
func callTool(c *stdioClient, name string, args, out any) error {
	resp, err := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return err
	}
	var tr toolCallResult
	if err := json.Unmarshal(resp.Result, &tr); err != nil {
		return fmt.Errorf("%s: decode result: %w", name, err)
	}
	if tr.IsError {
		return fmt.Errorf("%s: tool isError=true: %s", name, resp.Result)
	}
	if out != nil {
		if err := json.Unmarshal(tr.StructuredContent, out); err != nil {
			return fmt.Errorf("%s: decode structuredContent: %w", name, err)
		}
	}
	return nil
}

// handshake performs initialize + notifications/initialized.
func handshake(c *stdioClient, clientName string) error {
	if _, err := c.call("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": clientName, "version": "0.0.1"},
	}); err != nil {
		return err
	}
	return c.notify("notifications/initialized", map[string]any{})
}

// buildCtrBinary go build's the real binary into a temp dir (Go build cache
// makes repeat calls across tests cheap).
func buildCtrBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ctr.exe")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// spawnCtr starts bin with args, wiring stdin/stdout into a stdioClient and
// stderr into a buffer. Read the returned buffer only after the process has
// exited (exec.Cmd synchronizes non-pipe Stderr writers with Wait).
// Registers t.Cleanup to Kill and Wait on the process if it's still running
// (no-op after closeAndWait, safe on graceful exit).
func spawnCtr(t *testing.T, bin string, args ...string) (*exec.Cmd, *stdioClient, *bytes.Buffer, error) {
	cmd := exec.Command(bin, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	return cmd, newStdioClient(stdin, stdout), &stderrBuf, nil
}

// closeAndWait closes stdin (client-initiated stdio shutdown per MCP spec) and
// waits up to 5s for the process to exit.
func closeAndWait(cmd *exec.Cmd, c *stdioClient) error {
	if err := c.stdin.Close(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		return errors.New("process did not exit within 5s of stdin close")
	}
}

func TestE2E_StdioRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("느린 E2E 스모크 — short 모드 skip")
	}
	bin := buildCtrBinary(t)

	proj := t.TempDir()
	if err := os.WriteFile(filepath.Join(proj, "hello.txt"), []byte("alpha bravo charlie\n"), 0o644); err != nil {
		t.Fatalf("write hello.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(proj, "note.md"), []byte("# Note\nbravo appears here too.\n"), 0o644); err != nil {
		t.Fatalf("write note.md: %v", err)
	}
	storeRoot := t.TempDir()

	cmd, c, stderrBuf, err := spawnCtr(t, bin, "--root", proj, "--store-root", storeRoot, "--enable", "ingest")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if err := handshake(c, "ctr-e2e-test"); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	listResp, err := c.call("tools/list", nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var lt struct {
		Tools []struct {
			Name        string `json:"name"`
			Annotations struct {
				ReadOnlyHint bool `json:"readOnlyHint"`
			} `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(listResp.Result, &lt); err != nil {
		t.Fatalf("tools/list decode: %v", err)
	}
	gotNames, roHints := map[string]bool{}, map[string]bool{}
	for _, tl := range lt.Tools {
		gotNames[tl.Name] = true
		roHints[tl.Name] = tl.Annotations.ReadOnlyHint
	}
	for _, want := range []string{"ctr_index", "ctr_search", "ctr_fetch"} {
		if !gotNames[want] {
			t.Fatalf("tools/list missing %q: %+v", want, lt.Tools)
		}
	}
	if !roHints["ctr_search"] || !roHints["ctr_fetch"] {
		t.Fatalf("readOnlyHint missing on search/fetch: %+v", lt.Tools)
	}

	var idxOut mcp.IndexOutput
	if err := callTool(c, "ctr_index", mcp.IndexInput{Path: proj}, &idxOut); err != nil {
		t.Fatalf("ctr_index: %v", err)
	}
	if idxOut.Indexed != 2 {
		t.Fatalf("indexed=%d want 2 (skipped=%+v)", idxOut.Indexed, idxOut.Skipped)
	}

	var searchOut mcp.SearchOutput
	if err := callTool(c, "ctr_search", mcp.SearchInput{Queries: []string{"bravo"}}, &searchOut); err != nil {
		t.Fatalf("ctr_search: %v", err)
	}
	if !searchOut.Untrusted || len(searchOut.Results) != 1 || len(searchOut.Results[0].Hits) == 0 {
		t.Fatalf("bad search output: %+v", searchOut)
	}
	hit := searchOut.Results[0].Hits[0]

	var fetchOut mcp.FetchOutput
	fin := mcp.FetchInput{ArtifactID: hit.ArtifactID, LineStart: &hit.LineStart, LineEnd: &hit.LineEnd}
	if err := callTool(c, "ctr_fetch", fin, &fetchOut); err != nil {
		t.Fatalf("ctr_fetch: %v", err)
	}
	if fetchOut.ExactScope != "artifact" || fetchOut.Content == "" {
		t.Fatalf("bad fetch output: %+v", fetchOut)
	}

	if err := closeAndWait(cmd, c); err != nil {
		t.Fatalf("process exit: %v (stderr=%s)", err, stderrBuf.String())
	}
	if !strings.Contains(stderrBuf.String(), "[ctr] v") {
		t.Fatalf("banner missing in stderr: %q", stderrBuf.String())
	}
}

// indexOneFile spawns one process, indexes a single file, and shuts it down.
// Runs inside a goroutine in TestE2E_TwoProcessConcurrentIndex — must never
// call t.Fatal*, only return error (testing.T.FailNow requires the test
// goroutine).
func indexOneFile(t *testing.T, bin, proj, storeRoot, name string) error {
	cmd, c, stderrBuf, err := spawnCtr(t, bin, "--root", proj, "--store-root", storeRoot, "--enable", "ingest")
	if err != nil {
		return fmt.Errorf("%s: spawn: %w", name, err)
	}
	// fail: 실패 시 프로세스를 정리(Wait까지)하고 나서 stderrBuf를 읽는다 — Wait 전
	// 읽기는 exec의 내부 stderr 복사 고루틴과 경합한다.
	fail := func(stage string, err error) error {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("%s: %s: %w (stderr=%s)", name, stage, err, stderrBuf.String())
	}
	if err := handshake(c, "ctr-e2e-mp-"+name); err != nil {
		return fail("handshake", err)
	}
	var idxOut mcp.IndexOutput
	if err := callTool(c, "ctr_index", mcp.IndexInput{Path: filepath.Join(proj, name)}, &idxOut); err != nil {
		return fail("ctr_index", err)
	}
	if idxOut.Indexed != 1 {
		return fail("index-count", fmt.Errorf("indexed=%d want 1 (skipped=%+v)", idxOut.Indexed, idxOut.Skipped))
	}
	if err := closeAndWait(cmd, c); err != nil {
		return fmt.Errorf("%s: process exit: %w (stderr=%s)", name, err, stderrBuf.String())
	}
	return nil
}

func TestE2E_TwoProcessConcurrentIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("느린 다중 프로세스 스모크 — short 모드 skip")
	}
	bin := buildCtrBinary(t)

	proj := t.TempDir()
	files := map[string]string{
		"fileA.txt": "alpha content for process A\n",
		"fileB.txt": "zulu content for process B\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(proj, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	storeRoot := t.TempDir()

	// 워밍업 1회: 두 프로세스가 같은 store를 "최초로" 동시 생성하면 WAL 전환이
	// busy_timeout 적용 이전에 일어나 SQLITE_BUSY 경합이 실측됨 (Task 9 발견).
	// 근본 수정(DSN pragma 순서 또는 최초 migrate 파일락)은 계획 3 게이트 7 심층 범위 —
	// 컨트롤러 파견문이 이 테스트에서는 기초 검증(동시 쓰기)만 요구하고
	// integrity_check는 "세 번째 프로세스의 정상 동작으로 갈음 가능"으로 명시 허용.
	// 추적: .superpowers/sdd/progress.md "Task 9 발견" 항목.
	warmCmd, warmC, warmStderr, err := spawnCtr(t, bin, "--root", proj, "--store-root", storeRoot, "--enable", "ingest")
	if err != nil {
		t.Fatalf("warmup spawn: %v", err)
	}
	if err := handshake(warmC, "ctr-e2e-warmup"); err != nil {
		t.Fatalf("warmup handshake: %v", err)
	}
	if err := closeAndWait(warmCmd, warmC); err != nil {
		t.Fatalf("warmup exit: %v (stderr=%s)", err, warmStderr.String())
	}

	errs := make(chan error, len(files))
	var wg sync.WaitGroup
	for name := range files {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			errs <- indexOneFile(t, bin, proj, storeRoot, name)
		}(name)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	// 세 번째(신규) 프로세스로 두 파일 내용이 모두 검색되는지 확인 — sources=2·DB
	// 무결성은 이 정상 동작으로 갈음한다(설계 §12-10 기초, 심층 검증은 계획 3).
	cmd3, c3, stderrBuf3, err := spawnCtr(t, bin, "--root", proj, "--store-root", storeRoot, "--enable", "ingest")
	if err != nil {
		t.Fatalf("spawn#3: %v", err)
	}
	if err := handshake(c3, "ctr-e2e-verify"); err != nil {
		t.Fatalf("handshake#3: %v", err)
	}
	var searchOut mcp.SearchOutput
	if err := callTool(c3, "ctr_search", mcp.SearchInput{Queries: []string{"alpha", "zulu"}}, &searchOut); err != nil {
		t.Fatalf("ctr_search#3: %v", err)
	}
	if len(searchOut.Results) != 2 {
		t.Fatalf("results=%d want 2: %+v", len(searchOut.Results), searchOut.Results)
	}
	for _, qr := range searchOut.Results {
		if len(qr.Hits) == 0 {
			t.Fatalf("query %q: no hits: %+v", qr.Query, searchOut.Results)
		}
	}
	if err := closeAndWait(cmd3, c3); err != nil {
		t.Fatalf("process#3 exit: %v (stderr=%s)", err, stderrBuf3.String())
	}
}

// TestE2E_FetchAndIndex: 실바이너리를 --enable net --net-allow-local --net-ports <포트>로
// 띄우고 httptest URL을 ctr_fetch_and_index로 색인한 뒤 ctr_search로 본문을 찾는다(T6).
func TestE2E_FetchAndIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("느린 E2E 스모크 — short 모드 skip")
	}
	bin := buildCtrBinary(t)

	const page = `<html><body><h1>Doc</h1><p>zulunet unique e2e marker text.</p></body></html>`
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(page))
	}))
	defer httpSrv.Close()
	u, err := url.Parse(httpSrv.URL)
	if err != nil {
		t.Fatalf("parse httptest url: %v", err)
	}

	proj := t.TempDir()
	storeRoot := t.TempDir()
	cmd, c, stderrBuf, err := spawnCtr(t, bin, "--root", proj, "--store-root", storeRoot,
		"--enable", "net", "--net-allow-local", "--net-ports", u.Port())
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if err := handshake(c, "ctr-e2e-net"); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	var fiOut mcp.FetchAndIndexOutput
	if err := callTool(c, "ctr_fetch_and_index", mcp.FetchAndIndexInput{URL: httpSrv.URL}, &fiOut); err != nil {
		t.Fatalf("ctr_fetch_and_index: %v", err)
	}
	if fiOut.ArtifactID == 0 || fiOut.IndexedChunks == 0 {
		t.Fatalf("bad fetch_and_index output: %+v", fiOut)
	}

	var searchOut mcp.SearchOutput
	if err := callTool(c, "ctr_search", mcp.SearchInput{Queries: []string{"zulunet"}}, &searchOut); err != nil {
		t.Fatalf("ctr_search: %v", err)
	}
	if len(searchOut.Results) != 1 || len(searchOut.Results[0].Hits) == 0 {
		t.Fatalf("no hits: %+v", searchOut.Results)
	}

	if err := closeAndWait(cmd, c); err != nil {
		t.Fatalf("process exit: %v (stderr=%s)", err, stderrBuf.String())
	}
}

// sessionDBPathFor: --root proj/--store-root storeRoot 조합이 실제로 여는 session.db 절대
// 경로를 재계산한다(설계 §2.1 "projects/<pid>/worktrees/<wid>/session.db" 예약 — main.go의
// run()이 쓰는 canon.ProjectID/WorktreeID 해석을 테스트에서 재사용).
func sessionDBPathFor(t *testing.T, storeRoot, proj string) string {
	t.Helper()
	canon, err := ident.Canonicalize(proj)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	return filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID, "session.db")
}

// sessionToolNames: tools/list 응답에서 이름만 뽑아 집합으로 반환(세션 E2E 4종 공용).
func sessionToolNames(t *testing.T, c *stdioClient) map[string]bool {
	t.Helper()
	listResp, err := c.call("tools/list", nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var lt struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(listResp.Result, &lt); err != nil {
		t.Fatalf("tools/list decode: %v", err)
	}
	got := map[string]bool{}
	for _, tl := range lt.Tools {
		got[tl.Name] = true
	}
	return got
}

// TestE2E_SessionRoundTrip — T10 브리프 Step1 ①: 실바이너리로 record → summary → export →
// search(scope=events) round-trip. 세션 3종은 기본 등록(Enable 불요, 설계 §1.1)이므로 별도
// 플래그 없이도 tools/list에 나타나야 한다.
func TestE2E_SessionRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("느린 E2E 스모크 — short 모드 skip")
	}
	bin := buildCtrBinary(t)

	proj := t.TempDir()
	storeRoot := t.TempDir()

	cmd, c, stderrBuf, err := spawnCtr(t, bin, "--root", proj, "--store-root", storeRoot)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if err := handshake(c, "ctr-e2e-session-roundtrip"); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	gotNames := sessionToolNames(t, c)
	for _, want := range []string{"ctr_record_event", "ctr_session_summary", "ctr_export_events"} {
		if !gotNames[want] {
			t.Fatalf("tools/list missing %q: %+v", want, gotNames)
		}
	}

	const needle = "e2eroundtripzzyzxqp"
	var recOut mcp.RecordEventOutput
	recIn := mcp.RecordEventInput{EventType: "decision", Summary: "roundtrip marker " + needle}
	if err := callTool(c, "ctr_record_event", recIn, &recOut); err != nil {
		t.Fatalf("ctr_record_event: %v", err)
	}
	if recOut.EventID == "" || recOut.SessionID == "" || recOut.Ts == 0 {
		t.Fatalf("bad ctr_record_event output: %+v", recOut)
	}

	var sumOut mcp.SessionSummaryOutput
	if err := callTool(c, "ctr_session_summary", mcp.SessionSummaryInput{}, &sumOut); err != nil {
		t.Fatalf("ctr_session_summary: %v", err)
	}
	sawSummary := false
	for _, g := range sumOut.Groups {
		if g.EventType != "decision" {
			continue
		}
		for _, ev := range g.Events {
			if ev.EventID == recOut.EventID {
				sawSummary = true
			}
		}
	}
	if !sawSummary {
		t.Fatalf("ctr_session_summary missing recorded event: %+v", sumOut)
	}

	var expOut mcp.ExportEventsOutput
	if err := callTool(c, "ctr_export_events", mcp.ExportEventsInput{}, &expOut); err != nil {
		t.Fatalf("ctr_export_events: %v", err)
	}
	sawExport := false
	for _, ev := range expOut.Events {
		if ev.EventID != recOut.EventID {
			continue
		}
		sawExport = true
		if ev.SchemaVersion != "1.0" {
			t.Fatalf("export schemaVersion=%q want 1.0: %+v", ev.SchemaVersion, ev)
		}
		if ev.Producer.Name != "context-router" {
			t.Fatalf("export producer=%+v want name=context-router", ev.Producer)
		}
	}
	if !sawExport {
		t.Fatalf("ctr_export_events missing recorded event: %+v", expOut)
	}

	var searchOut mcp.SearchOutput
	searchIn := mcp.SearchInput{Queries: []string{needle}, Scope: "events"}
	if err := callTool(c, "ctr_search", searchIn, &searchOut); err != nil {
		t.Fatalf("ctr_search(events): %v", err)
	}
	if len(searchOut.Results) != 1 {
		t.Fatalf("search results=%+v want 1", searchOut.Results)
	}
	sawSearch := false
	for _, e := range searchOut.Results[0].Events {
		if e.EventID == recOut.EventID {
			sawSearch = true
		}
	}
	if !sawSearch {
		t.Fatalf("ctr_search(scope=events) missing recorded event: %+v", searchOut.Results[0])
	}

	if err := closeAndWait(cmd, c); err != nil {
		t.Fatalf("process exit: %v (stderr=%s)", err, stderrBuf.String())
	}
}

// TestE2E_SessionDBCorruptFailsClosed — T10 브리프 Step1 ②: session.db 헤더를 실바이너리
// 종료(lease 해제) 후 훼손하고 재스폰하면 세션 3종 도구가 tools/list에서 사라지고
// (fail-closed), 그와 무관하게 ctr_search(기본 scope=content)는 정상 응답해야 한다(설계
// §6.2 "content 도구는 정상 서빙 계속").
func TestE2E_SessionDBCorruptFailsClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("느린 E2E 스모크 — short 모드 skip")
	}
	bin := buildCtrBinary(t)

	proj := t.TempDir()
	storeRoot := t.TempDir()

	// 1회 스폰해 session.db를 정상 생성시키고 lease를 해제한다(writer가 파일을 계속
	// 참조 중인 상태로 훼손하면 결과가 비결정적이다).
	cmd0, c0, stderrBuf0, err := spawnCtr(t, bin, "--root", proj, "--store-root", storeRoot)
	if err != nil {
		t.Fatalf("warmup spawn: %v", err)
	}
	if err := handshake(c0, "ctr-e2e-corrupt-warmup"); err != nil {
		t.Fatalf("warmup handshake: %v", err)
	}
	if err := closeAndWait(cmd0, c0); err != nil {
		t.Fatalf("warmup exit: %v (stderr=%s)", err, stderrBuf0.String())
	}

	dbPath := sessionDBPathFor(t, storeRoot, proj)
	f, err := os.OpenFile(dbPath, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open session.db for corruption: %v", err)
	}
	if _, err := f.WriteAt([]byte("NOT-A-VALID-SQLITE-HEADER-BYTES!"), 0); err != nil {
		t.Fatalf("corrupt session.db header: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close corrupted session.db: %v", err)
	}

	cmd, c, stderrBuf, err := spawnCtr(t, bin, "--root", proj, "--store-root", storeRoot)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if err := handshake(c, "ctr-e2e-corrupt"); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	gotNames := sessionToolNames(t, c)
	for _, absent := range []string{"ctr_record_event", "ctr_session_summary", "ctr_export_events"} {
		if gotNames[absent] {
			t.Fatalf("session tool %q registered despite corrupt session.db: %+v", absent, gotNames)
		}
	}
	if !gotNames["ctr_search"] || !gotNames["ctr_fetch"] {
		t.Fatalf("content tools missing: %+v", gotNames)
	}

	var searchOut mcp.SearchOutput
	if err := callTool(c, "ctr_search", mcp.SearchInput{Queries: []string{"anything"}}, &searchOut); err != nil {
		t.Fatalf("ctr_search(content) should still work despite corrupt session.db: %v", err)
	}

	if err := closeAndWait(cmd, c); err != nil {
		t.Fatalf("process exit: %v (stderr=%s)", err, stderrBuf.String())
	}
	if !strings.Contains(stderrBuf.String(), "session.db") {
		t.Fatalf("stderr missing fail-closed warning: %q", stderrBuf.String())
	}
}

// recordSessionEvents spawns one process and records n ctr_record_event calls before
// shutting down cleanly — runs inside a goroutine in TestE2E_TwoProcessSessionLease, so it
// must never call t.Fatal*, only return error (mirrors indexOneFile).
func recordSessionEvents(t *testing.T, bin, proj, storeRoot, clientName string, n int) error {
	cmd, c, stderrBuf, err := spawnCtr(t, bin, "--root", proj, "--store-root", storeRoot)
	if err != nil {
		return fmt.Errorf("%s: spawn: %w", clientName, err)
	}
	fail := func(stage string, err error) error {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("%s: %s: %w (stderr=%s)", clientName, stage, err, stderrBuf.String())
	}
	if err := handshake(c, "ctr-e2e-lease-"+clientName); err != nil {
		return fail("handshake", err)
	}
	for i := 0; i < n; i++ {
		var out mcp.RecordEventOutput
		in := mcp.RecordEventInput{EventType: "note", Summary: fmt.Sprintf("%s event %d", clientName, i)}
		if err := callTool(c, "ctr_record_event", in, &out); err != nil {
			return fail(fmt.Sprintf("record#%d", i), err)
		}
		if out.EventID == "" {
			return fail(fmt.Sprintf("record#%d", i), errors.New("empty event_id"))
		}
	}
	if err := closeAndWait(cmd, c); err != nil {
		return fmt.Errorf("%s: process exit: %w (stderr=%s)", clientName, err, stderrBuf.String())
	}
	return nil
}

// TestE2E_TwoProcessSessionLease — T10 브리프 Step1 ③(G2·G8 실프로세스): 실바이너리 2개가
// 같은 worktree의 session.db를 동시에 열어(shared lease 공존, 설계 §6.2 ①) 양쪽 모두
// ctr_record_event에 성공해야 하고, 총 이벤트 수는 두 프로세스가 기록한 건수의 합과 정확히
// 같아야 한다(무손실). 검증은 export 도구로 한다(브리프 지침 — "이벤트 총수 검증은 export
// 도구 또는 CLI export로").
func TestE2E_TwoProcessSessionLease(t *testing.T) {
	if testing.Short() {
		t.Skip("느린 다중 프로세스 스모크 — short 모드 skip")
	}
	bin := buildCtrBinary(t)

	proj := t.TempDir()
	storeRoot := t.TempDir()

	// 워밍업 1회 — 두 프로세스가 store/session 디렉터리를 동시에 "최초로" 생성하는 경합을
	// 피한다(TestE2E_TwoProcessConcurrentIndex와 동일한 근거, Task 9 발견).
	warmCmd, warmC, warmStderr, err := spawnCtr(t, bin, "--root", proj, "--store-root", storeRoot)
	if err != nil {
		t.Fatalf("warmup spawn: %v", err)
	}
	if err := handshake(warmC, "ctr-e2e-lease-warmup"); err != nil {
		t.Fatalf("warmup handshake: %v", err)
	}
	if err := closeAndWait(warmCmd, warmC); err != nil {
		t.Fatalf("warmup exit: %v (stderr=%s)", err, warmStderr.String())
	}

	const perProc = 20
	names := []string{"procA", "procB"}
	errs := make(chan error, len(names))
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			errs <- recordSessionEvents(t, bin, proj, storeRoot, name, perProc)
		}(name)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	// 세 번째(신규) 프로세스로 export해 "note" 타입 이벤트 총수가 무손실인지 확인한다.
	cmdV, cV, stderrV, err := spawnCtr(t, bin, "--root", proj, "--store-root", storeRoot)
	if err != nil {
		t.Fatalf("verify spawn: %v", err)
	}
	if err := handshake(cV, "ctr-e2e-lease-verify"); err != nil {
		t.Fatalf("verify handshake: %v", err)
	}
	total := 0
	after := int64(0)
	for {
		var out mcp.ExportEventsOutput
		in := mcp.ExportEventsInput{After: after, Limit: 200}
		if err := callTool(cV, "ctr_export_events", in, &out); err != nil {
			t.Fatalf("ctr_export_events: %v", err)
		}
		for _, ev := range out.Events {
			if ev.EventType == "note" {
				total++
			}
		}
		if len(out.Events) == 0 || out.NextAfter == after {
			break
		}
		after = out.NextAfter
	}
	if total != 2*perProc {
		t.Fatalf("total note events=%d want %d (lease coexistence dropped events)", total, 2*perProc)
	}
	if err := closeAndWait(cmdV, cV); err != nil {
		t.Fatalf("verify exit: %v (stderr=%s)", err, stderrV.String())
	}
}

// TestE2E_SessionRecoverMarkerBlocks — T10 브리프 Step1 ④: session.recover-pending 마커가
// 존재하면 quick_check 결과와 무관하게 fail-closed해야 한다(설계 §6.3 "서버 open 계약 추가"
// — 빈 DB 신규 생성 금지). 세션 도구 부재 + stderr에 복구 CLI 안내가 나와야 한다.
func TestE2E_SessionRecoverMarkerBlocks(t *testing.T) {
	if testing.Short() {
		t.Skip("느린 E2E 스모크 — short 모드 skip")
	}
	bin := buildCtrBinary(t)

	proj := t.TempDir()
	storeRoot := t.TempDir()

	cmd0, c0, stderrBuf0, err := spawnCtr(t, bin, "--root", proj, "--store-root", storeRoot)
	if err != nil {
		t.Fatalf("warmup spawn: %v", err)
	}
	if err := handshake(c0, "ctr-e2e-marker-warmup"); err != nil {
		t.Fatalf("warmup handshake: %v", err)
	}
	if err := closeAndWait(cmd0, c0); err != nil {
		t.Fatalf("warmup exit: %v (stderr=%s)", err, stderrBuf0.String())
	}

	sessDir := filepath.Dir(sessionDBPathFor(t, storeRoot, proj))
	if err := os.WriteFile(filepath.Join(sessDir, "session.recover-pending"), nil, 0o600); err != nil {
		t.Fatalf("write recover marker: %v", err)
	}

	cmd, c, stderrBuf, err := spawnCtr(t, bin, "--root", proj, "--store-root", storeRoot)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if err := handshake(c, "ctr-e2e-marker"); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	gotNames := sessionToolNames(t, c)
	for _, absent := range []string{"ctr_record_event", "ctr_session_summary", "ctr_export_events"} {
		if gotNames[absent] {
			t.Fatalf("session tool %q registered despite recover marker: %+v", absent, gotNames)
		}
	}

	if err := closeAndWait(cmd, c); err != nil {
		t.Fatalf("process exit: %v (stderr=%s)", err, stderrBuf.String())
	}
	if !strings.Contains(stderrBuf.String(), "session recover") {
		t.Fatalf("stderr missing recover CLI guidance: %q", stderrBuf.String())
	}
}

// TestE2E_CallToolCancellation: 게이트 10 확인 항목 (3a) — 서버 취소 계약의 스크립트드
// 스모크(session-03 이월, gates-v0.0.1-ko.md §게이트 10 "이월 경위" 참조). go-sdk
// 클라이언트는 CallTool ctx가 취소되면 notifications/cancelled를 자동 송신하므로(SDK
// transport.go call()의 ctx.Err() 분기), 사람 개입 없이 실 바이너리에 취소를 결정적으로
// 주입할 수 있다. 손수 만든 stdioClient가 아니라 SDK ClientSession을 쓰는 이유: call()이
// 동기라 호출 도중 취소를 보낼 수 없다. 단언 3종:
//
//	(a) 취소 시점에 즉시 context.Canceled 반환 — 클라이언트측 계약이다(jsonrpc2 Await가
//	    로컬 select라 서버 처리와 무관하게 성립). 서버가 취소를 처리했다는 증거는 아니고,
//	    notifications/cancelled 송신까지만 보장한다(교차 리뷰 지적).
//	(b) 서버가 취소를 실제로 처리했는가의 직접 관찰 — worker 슬롯(transform.go workerSem
//	    ≤2)을 장기 호출 2건으로 점유하고 취소한 직후, 짧은 transform 2건을 동시에 실행해
//	    둘 다 수 초 내 완료돼야 한다. 서버가 취소 알림을 무시하면 슬롯이 각 핸들러의 10s
//	    timeout까지 잠겨 ≥9s가 걸리고(교차 리뷰 P1), 검증을 1건만 하면 "취소 1건만 처리된
//	    회귀"가 빠져나간다(교차 리뷰 P2) — 그래서 2건 동시·양쪽 상한 단언.
//	(c) 후속 ctr_search 정상 응답 + stdin close에 graceful exit(코드 0, 서버 생존).
//
// darwin은 ctr_transform이 fail-closed 미등록이라 대상 외(게이트 10 확인 항목 1 참조).
// linux는 RLIMIT_AS(주소공간 상한)와 Go 런타임의 광범위한 가상주소 예약이 충돌해 worker가
// 취소 전에 fail-closed로 조기 사망할 수 있다(PR #4 실측 194ms, 본 PR CI 실측 8.4ms) —
// 조기 사망으로 취소 창이 안 열리면 최대 3회 재시도 후 skip한다(알려진 갭, 게이트 문서 참조).
func TestE2E_CallToolCancellation(t *testing.T) {
	if testing.Short() {
		t.Skip("느린 E2E 스모크 — short 모드 skip")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("ctr_transform 미등록 플랫폼 — 취소 창을 만들 장시간 도구가 없음")
	}
	bin := buildCtrBinary(t)

	cmd := exec.Command(bin, "--root", t.TempDir(), "--store-root", t.TempDir())
	var stderrBuf bytes.Buffer // Wait 이후에만 읽는다(spawnCtr 주석과 동일한 계약)
	cmd.Stderr = &stderrBuf
	// SDK가 Close에서 Kill/Wait까지 책임지지만, 그 전에 t.Fatal로 이탈하면 자식이
	// 잔존한다 — spawnCtr과 동일한 안전망(정상 종료 후에는 no-op).
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})

	client := sdk.NewClient(&sdk.Implementation{Name: "ctr-cancel-smoke", Version: "0.0.1"}, nil)
	// Connect ctx는 핸드셰이크만 바운드한다(세션 수명 비종속) — 무기한이면 initialize가
	// 멈추는 회귀에서 패키지 전체 timeout까지 매달린다(교차 리뷰 P2).
	connectCtx, cancelConnect := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelConnect()
	sess, err := client.Connect(connectCtx, &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	// bounded churn: 워커 상한(5M steps·256MB·10s)에 스스로 도달하기 전에 취소가
	// 끼어들 수 초짜리 창을 만든다. 반드시 할당 없는 CPU 워크로드여야 한다 — 8MB
	// 가비지를 반복 생성하는 초안(base+base 버리기)은 GC의 커밋 반환이 순간적으로
	// 뒤처지면 상주 12MB로도 커밋이 Job Object 상한(256MB)에 닿아 worker가
	// 비결정적으로 조기 사망했다(windows 실측 20회 중 7회, VirtualAlloc errno=1455 →
	// Go runtime OOM abort; PR #4가 미규명으로 남긴 ubuntu RLIMIT_AS 조기 사망과 동일
	// 계열). count 스캔은 스텝당 수~수십 ms짜리 순수 CPU 작업이라 스텝 예산(≈32K ≪ 5M)과
	// 메모리 상한(상주 ≈4MB 고정, 가비지 0) 어느 쪽에도 닿지 않는다.
	//
	// 반복 수 8000의 근거(재리뷰 Important — 자연 완주가 10s 미만이면 아래 (b)의 슬롯
	// 판별이 "취소 무시 + 자연 완주" 회귀를 그린으로 통과시킬 수 있다): 이 워크로드의
	// 1500회 버전이 실호스트 스모크 2회에서 모두 핸들러 10s 상한에 도달했다(자연 완주
	// >10s 실측 하한). 8000회는 그 ≈5.3배라 ~5배 빠른 머신에서도 자연 완주가 10s를
	// 확실히 넘는다 — 어떤 실패 모드든 슬롯 해제는 "취소 처리" 아니면 "10s timeout"뿐.
	script := "base = \"x\" * 4000000\n" +
		"def churn():\n" +
		"    n = 0\n" +
		"    for _ in range(8000):\n" +
		"        n += base.count(\"xx\")\n" +
		"    return str(n)\n" +
		"emit(churn())\n"

	type callOutcome struct {
		res     *sdk.CallToolResult
		err     error
		elapsed time.Duration
	}
	// failDiag: 실패 진단은 반드시 sess.Close()로 SDK(cmd.Wait의 단일 소유자)가 프로세스
	// reap과 stderr 복사 고루틴 join을 끝낸 뒤 stderr를 읽는다. 테스트가 cmd.Wait()를 직접
	// 부르면, 서버 사망을 본 SDK read-loop의 conn 종료 경로가 부르는 cmd.Wait와 경쟁해
	// 둘 중 하나가 stderr 복사 고루틴 join(chan receive)에서 영구 대기한다 — CI 실측:
	// run 29676073834에서 이 테스트가 정확히 그 지점에서 10m 타임아웃.
	failDiag := func(format string, args ...any) {
		t.Helper()
		_ = sess.Close()
		t.Fatalf("%s\nstderr=%s", fmt.Sprintf(format, args...), stderrBuf.String())
	}

	// worker 슬롯 2개를 모두 점유하는 장기 호출 2건을 동시에 걸고 1s 뒤 함께 취소한다.
	churnPair := func() [2]callOutcome {
		churnCtx, cancelChurn := context.WithCancel(context.Background())
		defer cancelChurn()
		timer := time.AfterFunc(1*time.Second, cancelChurn)
		defer timer.Stop()
		ch := make(chan callOutcome, 2)
		for range 2 {
			go func() {
				start := time.Now()
				r, err := sess.CallTool(churnCtx, &sdk.CallToolParams{
					Name: "ctr_transform", Arguments: mcp.TransformInput{Script: script},
				})
				ch <- callOutcome{res: r, err: err, elapsed: time.Since(start)}
			}()
		}
		return [2]callOutcome{<-ch, <-ch}
	}

	// linux fail-closed 조기 사망 대응: 취소 창이 열린 시도(두 호출 모두 context.Canceled)가
	// 나올 때까지 최대 3회. 조기 사망은 서버 생존 계약을 깨지 않는 정상 fail-closed 경로지만
	// 취소를 exercise하지 못하므로 그 시도는 무효다. 한쪽만 조기 사망해도 슬롯 판별(b)의
	// 전제(두 슬롯 모두 취소로 해제)가 무너지므로 재시도한다.
	const maxAttempts = 3
	cancelledOK := false
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		earlyDeath := false
		for _, oc := range churnPair() {
			if errors.Is(oc.err, context.Canceled) {
				// 취소(1s) 직후 반환해야 한다. 상한만 단언하고 하한은 두지 않는다(PR #4
				// 교훈 — 하한은 구현 우연에 대한 단언이 된다).
				if oc.elapsed >= 4*time.Second {
					failDiag("(a) 취소 반환이 늦음: %v", oc.elapsed)
				}
				continue
			}
			b, _ := json.Marshal(oc.res)
			if oc.err == nil && oc.res != nil && oc.res.IsError && strings.Contains(string(b), "worker killed") {
				earlyDeath = true
				continue
			}
			failDiag("(a) want context.Canceled, got err=%v res=%s (elapsed=%v)", oc.err, string(b), oc.elapsed)
		}
		if !earlyDeath {
			cancelledOK = true
			break
		}
		t.Logf("attempt %d/%d: worker 조기 사망(fail-closed) — 취소 창 미확보, 재시도", attempt, maxAttempts)
		time.Sleep(1 * time.Second) // 취소된 상대 핸들러의 슬롯 반납 여유
	}
	if !cancelledOK {
		if runtime.GOOS == "windows" {
			// windows Job Object는 commit 상한이라 이 할당 없는 워크로드로는 조기 사망이
			// 없어야 한다(로컬 17회 연속 생존 실측) — 여기 도달하면 회귀 신호다.
			failDiag("windows에서 %d회 연속 worker 조기 사망 — 회귀 신호", maxAttempts)
		}
		t.Skipf("linux: worker fail-closed 조기 사망 %d회로 취소 창 미확보(알려진 갭 — 게이트 문서 §게이트 10). (3a) 상시 증거는 windows 잡", maxAttempts)
	}

	// (b) 서버측 취소 처리의 직접 관찰: 취소가 핸들러 ctx→worker까지 전파됐다면 슬롯 2개가
	// 곧 풀려 아래 2건은 각각 ~2s 내(스폰 포함)에 끝난다. 무시됐다면 churn 핸들러가 각자의
	// 10s timeout까지 슬롯을 쥐고 있어 ≥9s — 7s 상한이 두 경우를 가른다. 반드시 2건을
	// 동시에 검사한다: 1건만 보면 취소 하나만 처리된 회귀에서도 통과한다(교차 리뷰 P2).
	probes := make(chan callOutcome, 2)
	for range 2 {
		go func() {
			pctx, pcancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer pcancel()
			start := time.Now()
			r, err := sess.CallTool(pctx, &sdk.CallToolParams{
				Name: "ctr_transform", Arguments: mcp.TransformInput{Script: `emit("ok")`},
			})
			probes <- callOutcome{res: r, err: err, elapsed: time.Since(start)}
		}()
	}
	for range 2 {
		oc := <-probes
		// 판별 신호는 성공 여부가 아니라 시간이다 — 조기 사망한 probe도 슬롯 획득은
		// 이미 끝난 뒤이므로(Spawn은 sem 획득 후 스폰) elapsed가 상한 안이면 슬롯
		// 해제는 입증된다. 그래서 시간 단언을 먼저 한다.
		if oc.elapsed >= 7*time.Second {
			failDiag("(b) worker 슬롯이 제때 풀리지 않음(서버 취소 미처리 의심): %v", oc.elapsed)
		}
		if oc.err != nil {
			failDiag("(b) 취소 직후 transform: %v (elapsed=%v)", oc.err, oc.elapsed)
		}
		if oc.res.IsError {
			b, _ := json.Marshal(oc.res)
			if runtime.GOOS != "windows" && strings.Contains(string(b), "worker killed") {
				// linux fail-closed 조기 사망은 probe 워커에도 스크립트와 무관하게
				// 발생할 수 있다(워커 시작 단계의 VA 예약 충돌) — 재리뷰 (iv).
				t.Logf("(b) probe worker 조기 사망(fail-closed) — elapsed=%v로 슬롯 해제는 확인됨", oc.elapsed)
				continue
			}
			failDiag("(b) 취소 직후 transform 도구 오류: %s", string(b))
		}
	}

	// (c) 동일 세션 후속 호출 — 빈 스토어라 히트 내용은 보지 않고 정상 응답만 확인.
	folCtx, cancelFol := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelFol()
	res, err := sess.CallTool(folCtx, &sdk.CallToolParams{
		Name: "ctr_search", Arguments: mcp.SearchInput{Queries: []string{"cancel-smoke"}},
	})
	if err != nil {
		t.Fatalf("(c) 후속 ctr_search: %v", err)
	}
	if res.IsError {
		t.Fatalf("(c) 후속 ctr_search가 도구 오류를 반환: %+v", res)
	}

	// (c-2) stdin close 후 TerminateDuration(기본 5s) 안에 graceful exit해야 한다 —
	// 서버가 취소로 죽었거나 핸들러가 매달려 있으면 kill 경로로 빠져 오류가 난다.
	if err := sess.Close(); err != nil {
		t.Fatalf("(c) graceful shutdown: %v (stderr=%s)", err, stderrBuf.String())
	}
	if st := cmd.ProcessState; st == nil || !st.Success() {
		t.Fatalf("(c) exit state=%v want 코드 0 (stderr=%s)", st, stderrBuf.String())
	}
}

// ─── v0.2 forced-channel 통합 게이트 (설계 §10, T10 Step1-2) ───────────────────
//
// 실바이너리를 one-shot `context-router hook`으로 실행해(별도 프로세스·stdin 주입) cc: 세션
// 스트림을 E2E로 검증한다. internal/hook 단위 테스트가 이미 개별 동작을 커버하므로 대부분
// born-green이며, 여기서의 가치는 실 프로세스 경계·실 잠금 경합·MCP 서버 동시 가동을 pin하는 것.

// hookDeadlineEnv — 훅 deadline을 300ms로 낮춰 잠금 경합 경로가 예산 안에 종료하는지 측정한다.
var hookDeadlineEnv = map[string]string{"CTR_HOOK_DEADLINE_MS": "300"}

// hookFixture — internal/hook/testdata 골든 픽스처(T0)를 읽어 overrides로 필드를 덮어쓴 stdin
// JSON을 만든다(internal/hook의 fixtureWith를 package main에서 미러 — internals import 금지).
// 픽스처의 하드코딩 cwd는 호스트 의존적이라 각 테스트가 실재 t.TempDir()로 대체한다.
func hookFixture(t *testing.T, name string, overrides map[string]any) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "hook", "testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal fixture %s: %v", name, err)
	}
	for k, v := range overrides {
		m[k] = v
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal fixture %s: %v", name, err)
	}
	return out
}

// bigToolResponse — >CTR_SHADOW_MIN(16KiB)인 tool_response 오브젝트(strings.Repeat 생성 —
// 대용량 리터럴 금지 규율). Shadow Recall을 발화시킨다(hook_test.bigStdout와 동형).
func bigToolResponse() map[string]any {
	return map[string]any{"stdout": strings.Repeat("a", 20000), "stderr": ""}
}

// runHookOneShot — 실바이너리를 `hook --store-root <storeRoot>`로 1회 실행하고(신규 exec 헬퍼:
// stdin 주입·env 병합·exit code 캡처, spawnCtr는 장기 MCP stdio 전용이라 재사용 불가) 종료 코드와
// 경과 시간을 돌려준다. 훅은 fail-open이라 항상 exit 0이어야 한다. spawn/wait 실패만 t.Fatal한다.
func runHookOneShot(t *testing.T, bin, storeRoot string, stdin []byte, env map[string]string) (int, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "hook", "--store-root", storeRoot)
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)
	if err != nil {
		// ExitError는 종료 코드로 흡수(훅은 0이어야 하지만 판정은 호출자) — 그 외(spawn/timeout)는 Fatal.
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("hook one-shot 실행 실패: %v (stderr=%s)", err, stderrBuf.String())
		}
	}
	return cmd.ProcessState.ExitCode(), elapsed
}

// hookSessionDir — --root proj/--store-root storeRoot 조합의 worktree 세션 디렉터리(session.db·
// drops.log 위치). sessionDBPathFor의 부모.
func hookSessionDir(t *testing.T, storeRoot, proj string) string {
	t.Helper()
	return filepath.Dir(sessionDBPathFor(t, storeRoot, proj))
}

// hookContentDir — 프로젝트 레벨 content.db 디렉터리(<storeRoot>/projects/<pid>, main·§5 join과 동일).
func hookContentDir(t *testing.T, storeRoot, proj string) string {
	t.Helper()
	canon, err := ident.Canonicalize(proj)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	return filepath.Join(storeRoot, "projects", canon.ProjectID)
}

// readHookDrops — dir/session.drops.log 내용(fail-open 사이드카 검증용).
func readHookDrops(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "session.drops.log"))
	if err != nil {
		t.Fatalf("read drops in %s: %v", dir, err)
	}
	return string(b)
}

// assertOneDrop — drops.log이 정확히 1줄이고 그 줄이 want 토큰을 포함하는지 검증(deadline 게이트:
// 사이드카는 드롭당 1줄 append이므로 스퓨리어스 추가 드롭이 있으면 줄 수로 잡힌다).
func assertOneDrop(t *testing.T, dir, want string) {
	t.Helper()
	got := readHookDrops(t, dir)
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("drops=%d줄 want 1줄 (%q)", len(lines), got)
	}
	if !strings.Contains(lines[0], want) {
		t.Fatalf("drops=%q want %q 포함", got, want)
	}
}

// contentArtifactCount — contentDir/content.db(read-only)의 artifacts 행 수. 미존재(Shadow 미저장)면 -1.
func contentArtifactCount(t *testing.T, contentDir string) int {
	t.Helper()
	if _, err := os.Stat(filepath.Join(contentDir, "content.db")); os.IsNotExist(err) {
		return -1
	}
	st, err := store.Open(contentDir, true)
	if err != nil {
		t.Fatalf("open content.db ro: %v", err)
	}
	defer func() { _ = st.Close() }()
	var n int
	if err := st.Reader().QueryRow("SELECT count(*) FROM artifacts").Scan(&n); err != nil {
		t.Fatalf("count artifacts: %v", err)
	}
	return n
}

// countEventsByType — session.db(read-only)에서 event_type별 행 수를 맵으로 반환한다(worktree 전체).
func countEventsByType(t *testing.T, dbDir string) map[string]int {
	t.Helper()
	reader, err := session.OpenReadOnly(dbDir)
	if err != nil {
		t.Fatalf("open session.db: %v", err)
	}
	defer func() { _ = reader.Close() }()
	rows, err := reader.Query("SELECT event_type, count(*) FROM session_events GROUP BY event_type")
	if err != nil {
		t.Fatalf("group events: %v", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var et string
		var n int
		if err := rows.Scan(&et, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		counts[et] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return counts
}

// TestE2E_HookForcedChannel — 게이트족①(E2E) + 세션 규칙 재확인. 실바이너리 one-shot 훅으로
// SessionStart(×2, 멱등)·PostToolUse 3건(bash 소·write·bash 대=shadow)·미지 세션 1건을 실행하고
// session.db·content.db·drops.log를 직접 읽어 검증한다(서버 미개입 — 훅 기록 결정의 직접 관찰).
func TestE2E_HookForcedChannel(t *testing.T) {
	if testing.Short() {
		t.Skip("느린 E2E 스모크 — short 모드 skip")
	}
	bin := buildCtrBinary(t)
	proj := t.TempDir()
	storeRoot := t.TempDir()

	// SessionStart ×2 — EnsureSession 멱등: session_start 이벤트는 1건만 남아야 한다(설계 §2.2).
	for i := 0; i < 2; i++ {
		if rc, _ := runHookOneShot(t, bin, storeRoot, hookFixture(t, "sessionstart.json", map[string]any{"cwd": proj}), nil); rc != 0 {
			t.Fatalf("SessionStart #%d rc=%d want 0", i, rc)
		}
	}
	// PostToolUse 3건.
	posts := [][]byte{
		hookFixture(t, "posttooluse-bash.json", map[string]any{"cwd": proj}),                                     // test_run(소)
		hookFixture(t, "posttooluse-write.json", map[string]any{"cwd": proj}),                                    // file_edit
		hookFixture(t, "posttooluse-bash.json", map[string]any{"cwd": proj, "tool_response": bigToolResponse()}), // test_run + shadow
	}
	for i, p := range posts {
		if rc, _ := runHookOneShot(t, bin, storeRoot, p, nil); rc != 0 {
			t.Fatalf("PostToolUse #%d rc=%d want 0", i, rc)
		}
	}
	// D51 — 미지 세션(SessionStart 없이) PostToolUse → drop이 아니라 합성 등록(source="first-event")
	// 후 트리거 이벤트까지 기록된다(같은 worktree 세션 dir). posttooluse-bash.json은 test_run이라
	// 새 cc: 세션에 session_start 1건 + test_run 1건이 추가된다.
	unknownSID := "11111111-2222-4333-8444-555555555555"
	if rc, _ := runHookOneShot(t, bin, storeRoot, hookFixture(t, "posttooluse-bash.json", map[string]any{"cwd": proj, "session_id": unknownSID}), nil); rc != 0 {
		t.Fatalf("first-event rc=%d want 0", rc)
	}

	dbDir := hookSessionDir(t, storeRoot, proj)
	counts := countEventsByType(t, dbDir)
	// session_start 2건(기존 멱등 1 + D51 합성 등록 1), test_run 3건(bash 소·대 + 합성 세션 트리거),
	// file_edit 1건, shadow 이벤트 2건(artifact_created·tool_result_summary).
	want := map[string]int{"session_start": 2, "test_run": 3, "file_edit": 1, "artifact_created": 1, "tool_result_summary": 1}
	for et, n := range want {
		if counts[et] != n {
			t.Fatalf("event_type %q count=%d want %d (all=%+v)", et, counts[et], n, counts)
		}
	}
	// 스퓨리어스/예기치 못한 event_type 검출: 기대 카운트 합 == session_events 총 행 수.
	wantTotal, gotTotal := 0, 0
	for _, n := range want {
		wantTotal += n
	}
	for _, n := range counts {
		gotTotal += n
	}
	if gotTotal != wantTotal {
		t.Fatalf("session_events 총 %d행 want %d (예기치 못한 event_type: all=%+v)", gotTotal, wantTotal, counts)
	}
	// content.db에 shadow 아티팩트 1건.
	if n := contentArtifactCount(t, hookContentDir(t, storeRoot, proj)); n != 1 {
		t.Fatalf("content artifacts=%d want 1 (shadow 미저장)", n)
	}
	// D51 — 기존 세션(cc:3f25, source=startup)과 합성 등록 세션(cc:1111)이 둘 다 cc: 네임스페이스로
	// 존재하고, 합성 세션의 session_start payload가 source=first-event 마커를 담는다(E2E 층 D51 증명).
	reader, err := session.OpenReadOnly(dbDir)
	if err != nil {
		t.Fatalf("open session.db: %v", err)
	}
	defer func() { _ = reader.Close() }()
	var ccSessions int
	if err := reader.QueryRow("SELECT count(*) FROM sessions WHERE session_id LIKE 'cc:%'").Scan(&ccSessions); err != nil {
		t.Fatalf("cc sessions count: %v", err)
	}
	if ccSessions != 2 {
		t.Fatalf("cc sessions=%d want 2 (기존 + D51 합성 등록)", ccSessions)
	}
	var synPayload string
	if err := reader.QueryRow("SELECT payload FROM session_events WHERE session_id=? AND event_type='session_start'", "cc:"+unknownSID).Scan(&synPayload); err != nil {
		t.Fatalf("합성 세션 session_start payload: %v", err)
	}
	if !strings.Contains(synPayload, `"source":"first-event"`) {
		t.Fatalf("합성 세션 payload=%q want source=first-event (D51 마커)", synPayload)
	}
}

// TestE2E_HookDeadlineDeterminism — 게이트족②(deadline 결정론). 세 잠금 경합 경로 각각에 대해
// 실바이너리 훅을 CTR_HOOK_DEADLINE_MS=300으로 실행 → 예산 안에 종료(경과 측정)·exit 0·drops 1줄.
// (계획 리뷰 교체: exclusive lease 선점은 논블로킹 AcquireLock이 즉시 ErrLeaseHeld로 떨어져
// deadline 경로에 진입조차 못하므로 세 기법 ①동일 session.db BEGIN IMMEDIATE ②신규 DB init-lock
// ③content store open-lock으로 대체.) 5초 하드 대기 회귀를 3초 상한으로 잡는다(spawn 오버헤드 여유).
func TestE2E_HookDeadlineDeterminism(t *testing.T) {
	if testing.Short() {
		t.Skip("느린 E2E 스모크 — short 모드 skip")
	}
	bin := buildCtrBinary(t)

	// ① 동일 session.db에 BEGIN IMMEDIATE write txn 점유 → 후속 훅의 Append가 SQLite BUSY로
	//    예산 안에 포기(append-failed). 부모 프로세스가 OS 파일락으로 write 락을 잡는다(WAL 단일 writer).
	t.Run("append_begin_immediate", func(t *testing.T) {
		proj := t.TempDir()
		storeRoot := t.TempDir()
		if rc, _ := runHookOneShot(t, bin, storeRoot, hookFixture(t, "sessionstart.json", map[string]any{"cwd": proj}), nil); rc != 0 {
			t.Fatalf("SessionStart rc=%d want 0", rc)
		}
		dbPath := sessionDBPathFor(t, storeRoot, proj)
		// store.go와 동일 DSN(WAL·busy_timeout·_txlock=immediate) — modernc "sqlite" 드라이버는
		// session/store 패키지 import로 이미 등록돼 있다(별도 blank import 불필요).
		dsn := "file:" + filepath.ToSlash(dbPath) + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate"
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatalf("open session.db: %v", err)
		}
		db.SetMaxOpenConns(1)
		defer func() { _ = db.Close() }()
		tx, err := db.BeginTx(context.Background(), nil) // BEGIN IMMEDIATE — RESERVED write 락 취득
		if err != nil {
			t.Fatalf("begin immediate: %v", err)
		}
		// 임시 테이블 생성으로 write 락을 확실히 잡는다(rollback 시 원복). _txlock=immediate만으로도
		// 성립하지만 벨트-앤-서스펜더.
		if _, err := tx.ExecContext(context.Background(), "CREATE TABLE IF NOT EXISTS _e2e_writelock(x)"); err != nil {
			_ = tx.Rollback()
			t.Fatalf("write-lock probe: %v", err)
		}
		defer func() { _ = tx.Rollback() }()

		rc, elapsed := runHookOneShot(t, bin, storeRoot, hookFixture(t, "posttooluse-bash.json", map[string]any{"cwd": proj}), hookDeadlineEnv)
		t.Logf("① BEGIN IMMEDIATE: elapsed=%v", elapsed)
		if rc != 0 {
			t.Fatalf("rc=%d want 0(fail-open)", rc)
		}
		// 단일 SQLite busy 대기는 ctx-blind라 busy_timeout(500ms)이 300ms 예산을 넘긴다(spawn 포함
		// 계측 ≈1s). 상한은 2s — 옛 5초 하드 대기 회귀는 잡되 busy-wait 지배 경로에 여유를 둔다.
		if elapsed > 2*time.Second {
			t.Fatalf("deadline 미관측 의심 — %v 소요(5초 하드 대기 추정)", elapsed)
		}
		assertOneDrop(t, hookSessionDir(t, storeRoot, proj), "append-failed")
	})

	// ② 신규 DB의 session.init.lock을 exclusive 점유 → SessionStart 훅의 최초 WAL 전환 직렬화가
	//    예산 안에 포기(lease-held). session.db는 만들지 않는다(존재하면 init-lock 경로가 스킵된다).
	t.Run("init_lock_new_db", func(t *testing.T) {
		proj := t.TempDir()
		storeRoot := t.TempDir()
		dbDir := hookSessionDir(t, storeRoot, proj)
		if err := os.MkdirAll(dbDir, 0o700); err != nil {
			t.Fatalf("mkdir sessDir: %v", err)
		}
		release, err := store.AcquireLock(filepath.Join(dbDir, "session.init.lock"), false) // exclusive 선점
		if err != nil {
			t.Fatalf("acquire init-lock: %v", err)
		}
		defer release()

		rc, elapsed := runHookOneShot(t, bin, storeRoot, hookFixture(t, "sessionstart.json", map[string]any{"cwd": proj}), hookDeadlineEnv)
		t.Logf("② init-lock: elapsed=%v", elapsed)
		if rc != 0 {
			t.Fatalf("rc=%d want 0(fail-open)", rc)
		}
		// ctx-aware 대기 경로 — 예산 초과 즉시 포기(계측 ≈325ms). 상한 1s.
		if elapsed > time.Second {
			t.Fatalf("deadline 미관측 의심 — %v 소요", elapsed)
		}
		assertOneDrop(t, dbDir, "lease-held")
	})

	// ③ content store open-lock(content.db.rebuild.lock)을 exclusive 점유 → Shadow Recall의
	//    store.OpenContext가 예산 안에 포기(shadow-store). 대용량 응답으로 shadow 게이트에 진입시킨다.
	t.Run("content_store_lock", func(t *testing.T) {
		proj := t.TempDir()
		storeRoot := t.TempDir()
		if rc, _ := runHookOneShot(t, bin, storeRoot, hookFixture(t, "sessionstart.json", map[string]any{"cwd": proj}), nil); rc != 0 {
			t.Fatalf("SessionStart rc=%d want 0", rc)
		}
		contentDir := hookContentDir(t, storeRoot, proj)
		if err := os.MkdirAll(contentDir, 0o700); err != nil {
			t.Fatalf("mkdir contentDir: %v", err)
		}
		release, err := store.AcquireLock(filepath.Join(contentDir, "content.db.rebuild.lock"), false) // exclusive 선점
		if err != nil {
			t.Fatalf("acquire content lock: %v", err)
		}
		defer release()

		in := hookFixture(t, "posttooluse-bash.json", map[string]any{"cwd": proj, "tool_response": bigToolResponse()})
		rc, elapsed := runHookOneShot(t, bin, storeRoot, in, hookDeadlineEnv)
		t.Logf("③ content-lock: elapsed=%v", elapsed)
		if rc != 0 {
			t.Fatalf("rc=%d want 0(fail-open)", rc)
		}
		// ctx-aware 대기 경로 — 예산 초과 즉시 포기(계측 ≈323ms). 상한 1s.
		if elapsed > time.Second {
			t.Fatalf("deadline 미관측 의심 — %v 소요", elapsed)
		}
		assertOneDrop(t, hookSessionDir(t, storeRoot, proj), "shadow-store")
	})
}

// TestE2E_HookConcurrentSummaryExport — 게이트족①(2-프로세스 동시성) + 게이트족③(summary/export
// 왕복). SessionStart로 cc: 세션을 만든 뒤 MCP 서버(spawnCtr)를 장기 가동한 상태에서 one-shot 훅을
// 실행해(shared lease 공존·content.db/session.db 동시 쓰기) 무손실을 확인하고, 서버의
// ctr_session_summary/ctr_export_events가 훅 이벤트를 producer 정확·untrusted 표기로 반환하는지 검증한다.
func TestE2E_HookConcurrentSummaryExport(t *testing.T) {
	if testing.Short() {
		t.Skip("느린 E2E 스모크 — short 모드 skip")
	}
	bin := buildCtrBinary(t)
	proj := t.TempDir()
	storeRoot := t.TempDir()

	// cc: 세션 선재 — 미지 세션의 후속 이벤트는 drop되므로.
	if rc, _ := runHookOneShot(t, bin, storeRoot, hookFixture(t, "sessionstart.json", map[string]any{"cwd": proj}), nil); rc != 0 {
		t.Fatalf("SessionStart rc=%d want 0", rc)
	}

	// MCP 서버 장기 가동(shared lease + content store 열기). 이 동안 훅이 append한다.
	cmd, c, stderrBuf, err := spawnCtr(t, bin, "--root", proj, "--store-root", storeRoot)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if err := handshake(c, "ctr-e2e-hook-concurrent"); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	// 서버 가동 중 PostToolUse 4건(bash 소 3 + bash 대 1=shadow → content.db 동시 쓰기).
	const smallPosts = 3
	for i := 0; i < smallPosts; i++ {
		if rc, _ := runHookOneShot(t, bin, storeRoot, hookFixture(t, "posttooluse-bash.json", map[string]any{"cwd": proj}), nil); rc != 0 {
			t.Fatalf("PostToolUse 소 #%d rc=%d want 0", i, rc)
		}
	}
	if rc, _ := runHookOneShot(t, bin, storeRoot, hookFixture(t, "posttooluse-bash.json", map[string]any{"cwd": proj, "tool_response": bigToolResponse()}), nil); rc != 0 {
		t.Fatalf("PostToolUse 대 rc=%d want 0", rc)
	}
	wantTestRun := smallPosts + 1 // bash 4건 모두 test_run

	// 서버의 ctr_export_events로 훅 이벤트 무손실 + producer/untrusted 검증(worktree 전체 범위).
	testRun, artifactCreated := 0, 0
	var sawProducer bool
	after := int64(0)
	for {
		var out mcp.ExportEventsOutput
		if err := callTool(c, "ctr_export_events", mcp.ExportEventsInput{After: after, Limit: 200}, &out); err != nil {
			t.Fatalf("ctr_export_events: %v", err)
		}
		if !out.Untrusted {
			t.Fatal("export untrusted=false want true")
		}
		for _, ev := range out.Events {
			switch ev.EventType {
			case "test_run":
				testRun++
				if ev.SchemaVersion != "1.0" {
					t.Fatalf("hook 이벤트 schemaVersion=%q want 1.0", ev.SchemaVersion)
				}
				if ev.Producer.Name != "context-router" || ev.Producer.Version != version {
					t.Fatalf("hook 이벤트 producer=%+v want name=context-router version=%s", ev.Producer, version)
				}
				if ev.PrivacyLabel != "internal" {
					t.Fatalf("hook 이벤트 privacyLabel=%q want internal", ev.PrivacyLabel)
				}
				sawProducer = true
			case "artifact_created":
				artifactCreated++
			}
		}
		if len(out.Events) == 0 || out.NextAfter == after {
			break
		}
		after = out.NextAfter
	}
	if testRun != wantTestRun {
		t.Fatalf("test_run 이벤트=%d want %d (서버 동시 가동 중 훅 append 손실)", testRun, wantTestRun)
	}
	if artifactCreated != 1 {
		t.Fatalf("artifact_created=%d want 1 (content.db 동시 쓰기 손실)", artifactCreated)
	}
	if !sawProducer {
		t.Fatal("훅 이벤트의 producer/untrusted 표기를 확인하지 못함")
	}

	// ctr_session_summary도 훅 이벤트를 untrusted로 반환한다.
	var sumOut mcp.SessionSummaryOutput
	if err := callTool(c, "ctr_session_summary", mcp.SessionSummaryInput{}, &sumOut); err != nil {
		t.Fatalf("ctr_session_summary: %v", err)
	}
	if !sumOut.Untrusted {
		t.Fatal("summary untrusted=false want true")
	}
	sawSummaryTestRun := false
	for _, g := range sumOut.Groups {
		if g.EventType == "test_run" && len(g.Events) > 0 {
			sawSummaryTestRun = true
		}
	}
	if !sawSummaryTestRun {
		t.Fatalf("ctr_session_summary에 훅 test_run 그룹 부재: %+v", sumOut.Groups)
	}

	if err := closeAndWait(cmd, c); err != nil {
		t.Fatalf("process exit: %v (stderr=%s)", err, stderrBuf.String())
	}
}

// TestFTSMergeLoopMergesAndStamps: 병합 루프가 **실제로** 병합하고 스탬프를 남긴다.
// 상수만 재는 테스트는 MergeFTSIfDue 호출을 통째로 빼먹어도 통과한다 — 이 테스트가 그 구멍을
// 막는다. 판정은 둘이다: 스탬프 파일이 생겼는가(호출이 배선됐는가)와 tombstone이 실제로
// 걷혔는가(삭제만으로는 인덱스가 줄지 않는다).
func TestFTSMergeLoopMergesAndStamps(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir, false)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	body := strings.Repeat("alpha beta gamma delta ", 3000) // 약 70 KB/건
	for i := range 12 {
		s := body + strconv.Itoa(i)
		if _, err := st.Register(context.Background(), store.Registration{
			StoredBytes: []byte(s), MediaType: "text/plain",
			Source: store.SourceMeta{
				URI: "shadow:Bash:seg" + strconv.Itoa(i), Kind: "hook", SrcHash: "sh" + strconv.Itoa(i),
			},
			Chunks: []store.Chunk{{Ordinal: 0, ByteEnd: int64(len(s)), Text: s}},
		}); err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
	}
	// 전량 삭제 → FTS에는 tombstone만 쌓인다(병합 전에는 줄지 않는다).
	if _, _, err := st.PurgeOlderThan(context.Background(), time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatalf("PurgeOlderThan: %v", err)
	}
	before := ftsTrigramBytes(t, st)
	if before == 0 {
		t.Fatal("시드가 FTS 인덱스를 만들지 않았다 — 이 테스트가 공허 통과한다")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); runFTSMergeLoop(ctx, st, time.Millisecond, time.Hour) }()

	// store.mergeStampName은 비공개다 — 이름을 여기서 리터럴로 고정한다(바뀌면 이 테스트가 잡는다).
	stamp := filepath.Join(dir, "fts-merge.stamp")
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(stamp); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("루프가 스탬프를 남기지 않았다 — MergeFTSIfDue 호출이 배선되지 않았다")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done // 병합이 끝난 뒤에 읽는다(쓰기 중 측정 금지)

	if after := ftsTrigramBytes(t, st); after >= before {
		t.Fatalf("병합 후에도 인덱스가 줄지 않았다: %d → %d", before, after)
	}
}

// ftsTrigramBytes — fts_trigram_data의 block 바이트 합(세그먼트 실점유).
func ftsTrigramBytes(t *testing.T, st *store.Store) int64 {
	t.Helper()
	var n int64
	if err := st.Reader().QueryRow(
		`SELECT coalesce(sum(length(block)),0) FROM fts_trigram_data`,
	).Scan(&n); err != nil {
		t.Fatalf("fts_trigram_data: %v", err)
	}
	return n
}

// TestFTSMergeIntervalIsDaily: 자동 경로가 쓰는 병합 주기가 하루다. 위 루프 테스트가 동작을
// 재고 이 상수 테스트는 **값**을 잰다 — 이 값이 D67의 락 보유 규율과 맞물려 있어서다:
// 정상상태 병합이 쓰기 락을 약 1.2초 잡는데 훅의 총예산이 2000 ms다(설계 v0.20 D102 계약 9).
// 매 기동으로 낮추는 변경은 이 테스트를 지나야 한다.
func TestFTSMergeIntervalIsDaily(t *testing.T) {
	if defaultFTSMergeInterval != 24*time.Hour {
		t.Fatalf("병합 주기 = %v, 기대 24h — 설계 v0.20 D102 계약 2를 먼저 고쳐라",
			defaultFTSMergeInterval)
	}
}
