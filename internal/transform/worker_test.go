package transform

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// testSelfExe: 실 ctr 바이너리를 1회만 빌드해(sync.Once) 재사용한다 — Spawn은 main.go의
// "__transform-worker" 분기를 실제로 재실행하므로, transform 패키지가 leaf를 유지한 채
// (cmd를 import하지 않고) 프로덕션과 동일한 프로세스 경계로 테스트한다.
var (
	testExeOnce sync.Once
	testExePath string
	testExeErr  error
)

func testSelfExe(t *testing.T) string {
	t.Helper()
	testExeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ctr-worker-test-*")
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
			testExeErr = &buildErr{err: err, out: string(out)}
			return
		}
		testExePath = bin
	})
	if testExeErr != nil {
		t.Fatalf("selfExe 빌드 실패: %v", testExeErr)
	}
	return testExePath
}

type buildErr struct {
	err error
	out string
}

func (e *buildErr) Error() string { return e.err.Error() + ": " + e.out }

func TestMain(m *testing.M) {
	code := m.Run()
	if testExePath != "" {
		os.RemoveAll(filepath.Dir(testExePath))
	}
	os.Exit(code)
}

// TestSpawn_Normal: 정상 스크립트 → Spawn이 올바른 Output을 반환한다(실 프로세스 경계).
func TestSpawn_Normal(t *testing.T) {
	exe := testSelfExe(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req := Request{Script: `emit(",".join(sort(["b", "a", "c"])))`}
	res, err := Spawn(ctx, exe, req)
	if err != nil {
		t.Fatalf("Spawn error: %v", err)
	}
	if res.ErrKind != "" {
		t.Fatalf("ErrKind=%q want \"\" (res=%+v)", res.ErrKind, res)
	}
	if res.Output != "a,b,c" {
		t.Fatalf("Output=%q want %q", res.Output, "a,b,c")
	}
}

// TestSpawn_MemoryExplosion: 게이트 8 — 문자열 배증 스크립트가 OS 메모리 상한(256MB)을
// 넘으면 worker가 죽고, Spawn은 오류 Result를 반환하며 부모(테스트 프로세스)는 생존해야
// 한다. Windows 실측 필수(Job Object).
func TestSpawn_MemoryExplosion(t *testing.T) {
	exe := testSelfExe(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req := Request{
		Script: "def f():\n\ts = \"A\" * 1024\n\tfor i in range(30):\n\t\ts = s + s\n\treturn len(s)\n\nf()\n",
	}
	res, err := Spawn(ctx, exe, req)
	if err != nil {
		t.Fatalf("Spawn returned Go error (parent must survive OS-killed worker): %v", err)
	}
	if res.ErrKind == "" {
		t.Fatalf("want non-empty ErrKind (worker should have been memory-limit killed), got %+v", res)
	}
	t.Logf("gate8 result: %+v", res)
}

// TestSpawn_Timeout: 무거운 스텝 스크립트 + 짧은 ctx → 트리킬 후 오류 Result, 프로세스는
// 살아남고 Spawn은 ctx 데드라인 부근에서 반환해야 한다(행 금지).
func TestSpawn_Timeout(t *testing.T) {
	exe := testSelfExe(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req := Request{
		Script: "def f():\n\tfor i in range(2000000000):\n\t\tpass\n\nf()\n",
		Caps:   Caps{MaxSteps: 2_000_000_000_000}, // budget보다 ctx timeout이 먼저 발동하도록
	}

	start := time.Now()
	res, err := Spawn(ctx, exe, req)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Spawn returned Go error (parent must survive): %v", err)
	}
	if res.ErrKind == "" {
		t.Fatalf("want non-empty ErrKind (worker should have been timeout-killed), got %+v", res)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Spawn took %v, want bounded near ctx deadline (트리킬 실패 의심)", elapsed)
	}
	t.Logf("timeout result: %+v elapsed=%v", res, elapsed)
}

// TestSpawn_DefaultTimeout: ctx에 deadline이 없으면 Spawn이 내부 안전망(defaultWorkerTimeout
// =10s)을 씌운다 — CPU-only 무한루프(스텝 budget은 충분히 크게 줘 budget보다 timeout이 먼저
// 발동하도록)가 10s 근방(±2s)에서 오류 Result로 죽는지 검증한다. 기존 TestSpawn_Timeout(명시
// deadline 500ms, elapsed<10s 단언)이 "deadline 있으면 그대로 존중"의 회귀 가드를 겸한다.
func TestSpawn_DefaultTimeout(t *testing.T) {
	exe := testSelfExe(t)
	req := Request{
		Script: "def f():\n\tfor i in range(1000000000000):\n\t\tpass\n\nf()\n",
		Caps:   Caps{MaxSteps: 2_000_000_000_000},
	}

	start := time.Now()
	res, err := Spawn(context.Background(), exe, req) // deadline 없음
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Spawn returned Go error (parent must survive): %v", err)
	}
	if res.ErrKind == "" {
		t.Fatalf("want non-empty ErrKind (worker should have been default-timeout-killed), got %+v", res)
	}
	if elapsed < 8*time.Second || elapsed > 12*time.Second {
		t.Fatalf("elapsed=%v want ~10s (±2s) for defaultWorkerTimeout safety net", elapsed)
	}
	t.Logf("default timeout result: %+v elapsed=%v", res, elapsed)
}

// TestSpawn_ConcurrencyLimit: 3개 동시 Spawn → workerSem 관측상 동시 실행이 2를 넘지 않는다.
func TestSpawn_ConcurrencyLimit(t *testing.T) {
	exe := testSelfExe(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req := Request{
		Script: "def f():\n\tfor i in range(30000000):\n\t\tpass\n\nf()\n",
	}

	var maxObserved int
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				n := len(workerSem)
				mu.Lock()
				if n > maxObserved {
					maxObserved = n
				}
				mu.Unlock()
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Spawn(ctx, exe, req); err != nil {
				t.Errorf("Spawn error: %v", err)
			}
		}()
	}
	wg.Wait()
	close(done)

	mu.Lock()
	defer mu.Unlock()
	if maxObserved > 2 {
		t.Fatalf("maxObserved concurrent workers=%d want <=2", maxObserved)
	}
	if maxObserved < 2 {
		t.Logf("경고: maxObserved=%d — 워크로드가 폴링보다 빨라 동시성 2를 관측 못했을 수 있음(cap 위반은 아님)", maxObserved)
	}
	t.Logf("maxObserved=%d", maxObserved)
}
