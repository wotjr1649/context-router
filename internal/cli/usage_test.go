package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wotjr1649/context-router/internal/ident"
	"github.com/wotjr1649/context-router/internal/session"
)

// writeUsageTranscript — testdata용 축약 합성 transcript(민감값 없음, 실파일 복사 금지 —
// 브리프 Step 1). 파일명은 <session-uuid>.jsonl(호스트 세션 = 파일 = UUID, Step 1 실측 확인).
func writeUsageTranscript(t *testing.T, dir, uuid string, lines []string) {
	t.Helper()
	p := filepath.Join(dir, uuid+".jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript %s: %v", uuid, err)
	}
}

// TestRunUsage_PerSessionSums(시나리오 ①) — transcript 2세션 → 파일별(세션별) usage 합산이
// 정확해야 한다(설계 §6). usage 없는 줄(user)은 합산 대상 밖.
func TestRunUsage_PerSessionSums(t *testing.T) {
	tdir := t.TempDir()
	writeUsageTranscript(t, tdir, "aaaaaaaa-0000-0000-0000-000000000001", []string{
		`{"type":"user","message":{"role":"user"}}`,
		`{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":5,"cache_creation_input_tokens":1}}}`,
		`{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":50,"output_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
	})
	writeUsageTranscript(t, tdir, "bbbbbbbb-0000-0000-0000-000000000002", []string{
		`{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":2,"cache_creation_input_tokens":9}}}`,
	})

	var out, errOut bytes.Buffer
	if err := Run(context.Background(), "usage", []string{"--transcripts", tdir}, t.TempDir(), t.TempDir(), "0.0.1-dev", false, "", &out, &errOut); err != nil {
		t.Fatalf("Run usage err=%v out=%s", err, out.String())
	}
	got := out.String()
	// 컬럼: session\tinput\toutput\tcache_read\tcache_creation\trecords\thooks
	for _, want := range []string{
		"aaaaaaaa-0000-0000-0000-000000000001\t150\t30\t5\t1\t2\t",
		"bbbbbbbb-0000-0000-0000-000000000002\t7\t3\t2\t9\t1\t",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("out missing row %q:\n%s", want, got)
		}
	}
}

// TestRunUsage_HooksOnWhenCCSeeded(시나리오 ②) — session.db에 cc:<uuid>가 존재하면(훅 스트림
// 존재 신호) 그 세션 행은 hooks:on으로 표기해야 한다(설계 §6). cc: 세션은 OpenAppend+
// EnsureSession으로 시드한다(hook 런타임과 동일 경로).
func TestRunUsage_HooksOnWhenCCSeeded(t *testing.T) {
	storeRoot := t.TempDir()
	projectRoot := t.TempDir()
	uuid := "cccccccc-0000-0000-0000-000000000003"

	canon, err := ident.Canonicalize(projectRoot)
	if err != nil {
		t.Fatalf("canon: %v", err)
	}
	sessDir := filepath.Join(storeRoot, "projects", canon.ProjectID, "worktrees", canon.WorktreeID)
	ad, err := session.OpenAppend(context.Background(), sessDir, session.AppendOptions{ExternalSessionID: "cc:" + uuid, Producer: "test", RetentionSec: 0})
	if err != nil {
		t.Fatalf("OpenAppend: %v", err)
	}
	if _, err := ad.EnsureSession(context.Background(), "startup", ""); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	if err := ad.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	tdir := t.TempDir()
	writeUsageTranscript(t, tdir, uuid, []string{
		`{"type":"assistant","message":{"usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
	})

	var out, errOut bytes.Buffer
	if err := Run(context.Background(), "usage", []string{"--transcripts", tdir}, storeRoot, projectRoot, "0.0.1-dev", false, "", &out, &errOut); err != nil {
		t.Fatalf("Run usage err=%v out=%s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, uuid) {
		t.Fatalf("row for %s missing:\n%s", uuid, got)
	}
	// uuid 줄이 hooks:on이어야 한다 — 그 줄만 뽑아 확인(다른 줄 오염 방지).
	line := usageRowFor(got, uuid)
	if !strings.Contains(line, "hooks:on") {
		t.Fatalf("want %s marked hooks:on, got line %q", uuid, line)
	}
}

// TestRunUsage_HooksOffWhenNoSession(시나리오 ③) — 그 UUID의 cc: 세션이 session.db에 없으면
// hooks:off여야 한다(설계 §6). session.db 자체가 없는 경우(훅 미사용 프로젝트)도 동일.
func TestRunUsage_HooksOffWhenNoSession(t *testing.T) {
	tdir := t.TempDir()
	uuid := "dddddddd-0000-0000-0000-000000000004"
	writeUsageTranscript(t, tdir, uuid, []string{
		`{"type":"assistant","message":{"usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
	})

	var out, errOut bytes.Buffer
	if err := Run(context.Background(), "usage", []string{"--transcripts", tdir}, t.TempDir(), t.TempDir(), "0.0.1-dev", false, "", &out, &errOut); err != nil {
		t.Fatalf("Run usage err=%v out=%s", err, out.String())
	}
	got := out.String()
	line := usageRowFor(got, uuid)
	if line == "" {
		t.Fatalf("row for %s missing:\n%s", uuid, got)
	}
	if !strings.Contains(line, "hooks:off") {
		t.Fatalf("want %s hooks:off (no session seeded), got line %q", uuid, line)
	}
}

// TestRunUsage_CorruptLineSkipped(시나리오 ④) — 비JSON 손상 줄은 건너뛰고 명령 전체는
// 중단되지 않아야 한다(설계 §6, readTranscriptLine/providerUsageLine 재사용 계약). 앞뒤 정상
// 줄은 계속 합산된다.
func TestRunUsage_CorruptLineSkipped(t *testing.T) {
	tdir := t.TempDir()
	uuid := "eeeeeeee-0000-0000-0000-000000000005"
	writeUsageTranscript(t, tdir, uuid, []string{
		`{"type":"assistant","message":{"usage":{"input_tokens":9,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
		`not valid json`,
		`{"type":"assistant","message":{"usage":{"input_tokens":1,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
	})

	var out, errOut bytes.Buffer
	if err := Run(context.Background(), "usage", []string{"--transcripts", tdir}, t.TempDir(), t.TempDir(), "0.0.1-dev", false, "", &out, &errOut); err != nil {
		t.Fatalf("corrupt line must not abort: err=%v out=%s", err, out.String())
	}
	got := out.String()
	// 정상 2줄만: input 10, output 1, cache 0/0, records 2 (손상 줄은 제외).
	if want := uuid + "\t10\t1\t0\t0\t2\t"; !strings.Contains(got, want) {
		t.Fatalf("want corrupt-skipped sums %q, got:\n%s", want, got)
	}
}

// TestRunUsage_MissingDirError(시나리오 ⑤) — transcript 디렉터리 부재는 명확한 오류여야 하고,
// 반환 오류 문구에 절대경로가 섞이면 안 된다(§12 canary — os.ReadDir의 *fs.PathError는 경로를
// 담는다).
func TestRunUsage_MissingDirError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-transcripts-dir")
	var out, errOut bytes.Buffer
	err := Run(context.Background(), "usage", []string{"--transcripts", missing}, t.TempDir(), t.TempDir(), "0.0.1-dev", false, "", &out, &errOut)
	if err == nil {
		t.Fatal("want error for missing transcripts dir, got nil")
	}
	if !strings.Contains(err.Error(), "디렉터리") {
		t.Fatalf("want a clear directory error, got: %v", err)
	}
	if strings.Contains(err.Error(), missing) {
		t.Fatalf("error must not leak the path: %v", err)
	}
}

// TestTranscriptDirFor_NonAlnumToHyphen — Step 1 실측 계약 고정: cwd → transcript 디렉터리명은
// 영숫자가 아닌 모든 문자를 '-'로 치환한다(밑줄·점 포함). plan의 "경로 구분자·콜론만" 규칙이었다면
// AI_DEV의 밑줄이 남아 실제 디렉터리(AI-DEV)와 어긋난다 — 이 테스트가 회귀를 막는다(브리프 Step 1).
func TestTranscriptDirFor_NonAlnumToHyphen(t *testing.T) {
	// 실측 예: C:\Users\js\Documents\AI_DEV\context-router
	//        → C--Users-js-Documents-AI-DEV-context-router
	got := transcriptDirRe.ReplaceAllString(`C:\Users\js\Documents\AI_DEV\context-router`, "-")
	if want := "C--Users-js-Documents-AI-DEV-context-router"; got != want {
		t.Fatalf("transform mismatch: got %q want %q", got, want)
	}
	if strings.Contains(got, "_") { // 밑줄이 '-'로 바뀌었는지(plan 규칙과의 차이) 명시 확인
		t.Fatalf("underscore must be replaced with '-': %q", got)
	}
}

// usageRowFor — out에서 uuid로 시작하는 표 행 하나를 돌려준다(없으면 "").
func usageRowFor(out, uuid string) string {
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, uuid) {
			return ln
		}
	}
	return ""
}
