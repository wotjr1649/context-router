package store

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestAppendPurgeLogFields — 9필드 고정과 미측정(`-`) 대 실측 0의 구분을 잠근다.
// 뒤 세 칸이 포인터인 이유가 그것이다(스펙 §3): 0은 "쟀더니 0"이라는 주장이고,
// 함수가 애초에 그 값을 내지 않는 경로에서 0을 쓰면 거짓 측정이 된다.
func TestAppendPurgeLogFields(t *testing.T) {
	dir := t.TempDir()
	zero := int64(0)
	zeroI := 0
	AppendPurgeLog(dir, PurgeRecord{
		Path: "startup-shadow", Policy: "336h0m0s/pwsh-profile", Status: "ok",
		Cutoff: 1753971299, Count: 0, Bytes: &zero, Deferred: &zeroI, Failed: &zeroI,
	})
	data, err := os.ReadFile(filepath.Join(dir, PurgeLogName))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	line := strings.TrimRight(string(data), "\n")
	f := strings.Split(line, "\t")
	if len(f) != 9 {
		t.Fatalf("필드 %d개 want 9: %q", len(f), line)
	}
	// ts는 실행 시각이라 값을 고정하지 않는다 — 숫자인 것만 본다.
	if f[0] == "" || strings.ContainsAny(f[0], "abcdef-") {
		t.Fatalf("ts가 unix 초가 아니다: %q", f[0])
	}
	want := []string{"startup-shadow", "336h0m0s/pwsh-profile", "1753971299", "ok", "0", "0", "0", "0"}
	if got := f[1:]; !slices.Equal(got, want) {
		t.Fatalf("필드 = %q\nwant %q", got, want)
	}
}

// TestAppendPurgeLogUnmeasuredIsDash — 못 잰 것은 `-`, 재서 0인 것은 `0`.
// cli-older-than 경로가 바이트·유예·실패를 내지 않으므로(스펙 §2.0) 이 구분이 없으면
// 읽는 쪽이 "회수 바이트가 0이었다"는 하지 않은 측정을 읽는다.
func TestAppendPurgeLogUnmeasuredIsDash(t *testing.T) {
	dir := t.TempDir()
	AppendPurgeLog(dir, PurgeRecord{
		Path: "cli-older-than", Policy: "720h0m0s/-", Status: "ok", Cutoff: 0, Count: 12,
	})
	data, _ := os.ReadFile(filepath.Join(dir, PurgeLogName))
	f := strings.Split(strings.TrimRight(string(data), "\n"), "\t")
	if len(f) != 9 {
		t.Fatalf("필드 %d개 want 9", len(f))
	}
	if f[3] != "-" {
		t.Errorf("cutoff=%q want %q — 0은 1970년이 아니라 '경계 개념 없음'이다", f[3], "-")
	}
	if f[5] != "12" {
		t.Errorf("count=%q want 12", f[5])
	}
	for i, name := range map[int]string{6: "bytes", 7: "deferred", 8: "failed"} {
		if f[i] != "-" {
			t.Errorf("%s=%q want %q — nil은 미측정이다", name, f[i], "-")
		}
	}
}

// TestAppendPurgeLogSanitizes — 탭·개행·CR은 공백으로, 64자 상한, 빈 값은 `-`.
// CTR_RETENTION_SOURCE는 사람이 임의 문자열을 넣는 자리다 — 위생이 없으면 그 한 값이
// 파서를 오염시키고, 그것이 이 로그가 막으려던 바로 그 종류의 침묵을 만든다(스펙 §2.4).
func TestAppendPurgeLogSanitizes(t *testing.T) {
	dir := t.TempDir()
	AppendPurgeLog(dir, PurgeRecord{
		Path: "cli-gc", Policy: "a\tb\nc\rd", Status: strings.Repeat("x", 100), Count: 3,
	})
	data, _ := os.ReadFile(filepath.Join(dir, PurgeLogName))
	line := strings.TrimRight(string(data), "\n")
	if strings.Count(line, "\t") != 8 {
		t.Fatalf("탭이 8개가 아니다 — 위생이 필드를 쪼갰다: %q", line)
	}
	f := strings.Split(line, "\t")
	if f[2] != "a b c d" {
		t.Errorf("policy=%q want %q", f[2], "a b c d")
	}
	if len(f[4]) != 64 {
		t.Errorf("status 길이=%d want 64(상한)", len(f[4]))
	}
}

// TestAppendPurgeLogAppends — append-only. 두 번 부르면 두 줄이다.
func TestAppendPurgeLogAppends(t *testing.T) {
	dir := t.TempDir()
	AppendPurgeLog(dir, PurgeRecord{Path: "cli-gc", Status: "ok"})
	AppendPurgeLog(dir, PurgeRecord{Path: "cli-gc", Status: "ok"})
	data, _ := os.ReadFile(filepath.Join(dir, PurgeLogName))
	if n := strings.Count(string(data), "\n"); n != 2 {
		t.Fatalf("줄 %d개 want 2 — append-only가 아니다", n)
	}
}

// TestAppendPurgeLogBestEffort — 쓸 수 없는 자리에서도 패닉하지 않고 조용히 넘어간다.
// 퍼지 결과에 영향이 없다는 계약(스펙 §5)의 하한이다.
func TestAppendPurgeLogBestEffort(t *testing.T) {
	f := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	AppendPurgeLog(f, PurgeRecord{Path: "cli-gc", Status: "ok"}) // 디렉터리가 아니다 — 패닉하지 않아야 한다
}

// TestPurgePolicy — 값과 출처를 한 칸에 합치고, 빈 출처는 `-`.
// 빈 출처 자체가 신호다: 프로필 처방이 닿지 않은 계통이라는 뜻이다(스펙 §2.3).
func TestPurgePolicy(t *testing.T) {
	if got := PurgePolicy(336*time.Hour, "pwsh-profile"); got != "336h0m0s/pwsh-profile" {
		t.Errorf("PurgePolicy = %q want %q", got, "336h0m0s/pwsh-profile")
	}
	if got := PurgePolicy(72*time.Hour, ""); got != "72h0m0s/-" {
		t.Errorf("빈 출처 = %q want %q", got, "72h0m0s/-")
	}
}

// TestPurgeStatusPriority — 스펙 §2.2의 우선순위가 계약이다(§6 항목 6). **partial이 budget·
// cancelled를 이긴다**: 행이 이미 삭제돼 다음 기동 술어에 다시 안 잡히므로, 중단 사유가
// 무엇이든 남은 파일의 유일한 회수 경로가 purge --gc다. 순서가 뒤집히면 "다음 기동이 다시
// 집는다"는 **거짓 안내**가 나간다. 예산 소진·종료 취소를 e2e로 유도하는 것은 비싸고
// 불안정하므로 분류를 순수 함수로 두고 표로 잰다 — 이 표가 없으면 순서 교체가 조용히 통과한다.
func TestPurgeStatusPriority(t *testing.T) {
	errX := errors.New("x")
	for _, tc := range []struct {
		name                      string
		err                       error
		hashes                    int
		cancelled, budget, capped bool
		want                      string
	}{
		{"정상 0건", nil, 0, false, false, false, "ok"},
		{"정상 N건", nil, 5, false, false, false, "ok"},
		{"상한", nil, 100, false, false, true, "capped"},
		{"행 삭제 실패", errX, 0, false, false, false, "failed"},
		{"예산 소진", errX, 0, false, true, false, "budget"},
		{"종료 취소", errX, 0, true, false, false, "cancelled"},
		{"커밋 뒤 취소", errX, 5, true, false, false, "partial"},
		{"커밋 뒤 예산", errX, 5, false, true, false, "partial"},
		{"커밋 뒤 둘 다", errX, 5, true, true, false, "partial"},
	} {
		if got := PurgeStatus(tc.err, tc.hashes, tc.cancelled, tc.budget, tc.capped); got != tc.want {
			t.Errorf("%s: %q want %q", tc.name, got, tc.want)
		}
	}
}
