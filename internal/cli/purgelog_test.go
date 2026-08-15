package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wotjr1649/context-router/internal/store"
)

func writePurgeLog(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), store.PurgeLogName)
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestPurgeLogTailParses — 정상 9필드 줄을 읽고 꼬리 n건을 최신 순으로 낸다.
func TestPurgeLogTailParses(t *testing.T) {
	p := writePurgeLog(
		t,
		"1755180000\tstartup-shadow\t336h0m0s/pwsh-profile\t1753971299\tok\t0\t0\t0\t0",
		"1755180899\tcli-older-than\t720h0m0s/-\t-\tok\t12\t-\t-\t-",
	)
	entries, total, unparsed := purgeLogTail(p, 5)
	if total != 2 || unparsed != 0 || len(entries) != 2 {
		t.Fatalf("total=%d unparsed=%d entries=%d want 2/0/2", total, unparsed, len(entries))
	}
	// 최신이 먼저다 — doctor가 "최근 N건"으로 읽는다.
	if entries[0].Path != "cli-older-than" || entries[0].Count != 12 {
		t.Errorf("entries[0]=%+v want cli-older-than/12", entries[0])
	}
	if entries[0].Bytes != "-" || entries[0].Deferred != "-" || entries[0].Failed != "-" {
		t.Errorf("미측정 칸이 %q/%q/%q — `-`를 그대로 날라야 한다(0으로 바꾸면 하지 않은 측정이 된다)",
			entries[0].Bytes, entries[0].Deferred, entries[0].Failed)
	}
	if entries[1].Cutoff != 1753971299 {
		t.Errorf("cutoff=%d want 1753971299", entries[1].Cutoff)
	}
}

// TestPurgeLogTailAcceptsWiderRows — **9필드 이상은 수용하고 10번째부터 무시한다**.
// 스펙 §2.0이 "필요해지면 그때 열을 늘린다"를 열어 뒀고 로그는 append-only라, 정확
// 매칭으로 잠그면 확장 순간 옛 파서가 새 줄을 전부 버린다. 이 단정이 없으면 다음 사람이
// "정확히 9필드"로 되조인다.
func TestPurgeLogTailAcceptsWiderRows(t *testing.T) {
	p := writePurgeLog(
		t,
		"1755180000\tcli-older-than\t720h0m0s/-\t-\tok\t12\t-\t-\t-\t999",
	)
	entries, _, unparsed := purgeLogTail(p, 5)
	if unparsed != 0 || len(entries) != 1 {
		t.Fatalf("10필드 줄이 unparsed=%d entries=%d — 수용해야 한다", unparsed, len(entries))
	}
	if entries[0].Count != 12 {
		t.Errorf("count=%d want 12 — 앞 9칸의 의미가 유지돼야 한다", entries[0].Count)
	}
}

// TestPurgeLogTailUnparsed — 9필드보다 짧은 줄·비숫자 ts는 unparsed로 세고 **진단을
// 중단하지 않는다**(dropsByReason 관례). total은 빈 줄 포함 줄 수 계약이다.
func TestPurgeLogTailUnparsed(t *testing.T) {
	p := writePurgeLog(
		t,
		"1755180000\tstartup-shadow\t336h0m0s/-\t-\tok\t0\t0\t0\t0",
		"짧다\t줄",
		"",
		"notanumber\tcli-gc\t-\t-\tok\t1\t-\t-\t-",
	)
	entries, total, unparsed := purgeLogTail(p, 5)
	if total != 4 {
		t.Errorf("total=%d want 4 — 빈 줄도 센다(줄 수 계약)", total)
	}
	if unparsed != 3 {
		t.Errorf("unparsed=%d want 3", unparsed)
	}
	if len(entries) != 1 {
		t.Errorf("entries=%d want 1 — 정상 줄만 나온다", len(entries))
	}
}

// TestPurgeLogTailAbsent — 파일 부재는 (nil, 0, 0)이고 오류가 아니다.
// **"삭제가 없었다"와 구분하는 것은 doctor의 문면 몫이다**(Task 5).
func TestPurgeLogTailAbsent(t *testing.T) {
	entries, total, unparsed := purgeLogTail(filepath.Join(t.TempDir(), "nope.log"), 5)
	if entries != nil || total != 0 || unparsed != 0 {
		t.Fatalf("부재 = %v/%d/%d want nil/0/0", entries, total, unparsed)
	}
}

// TestPurgeLogTailLimit — n건만 낸다. 꼬리에서 자른다.
func TestPurgeLogTailLimit(t *testing.T) {
	p := writePurgeLog(
		t,
		"1755180001\tcli-gc\t-\t-\tok\t1\t-\t-\t-",
		"1755180002\tcli-gc\t-\t-\tok\t2\t-\t-\t-",
		"1755180003\tcli-gc\t-\t-\tok\t3\t-\t-\t-",
	)
	entries, total, _ := purgeLogTail(p, 2)
	if total != 3 || len(entries) != 2 {
		t.Fatalf("total=%d entries=%d want 3/2", total, len(entries))
	}
	if entries[0].Count != 3 || entries[1].Count != 2 {
		t.Errorf("꼬리 2건이 %d,%d want 3,2", entries[0].Count, entries[1].Count)
	}
}

// TestPurgeLogTailRejectsSplicedLine — **이 테스트가 형식 검증의 존재 이유다**(검토 소견 F3).
// append-only 파일에서 찢어진 조각 뒤에 다음 레코드가 이어붙으면 합쳐진 줄이 정확히 9필드가
// 될 수 있고, ts·path·count만 보는 파서는 그것을 수용한다 — doctor가 **일어나지 않은 삭제
// 사건**을 보고하게 되고, 그것이 이 로그가 막으려던 바로 그것이다.
//
// 아래 픽스처는 ts·path·count와 cutoff·bytes·deferred·failed(전부 `-`)를 유효하게 두고
// status 칸 하나만 다음 레코드의 ts 조각(`1755180899`)으로 어긋나게 만든다 — 절단이 status
// 필드 위치에서 일어난 모양이다. 그래서 이 줄을 거르는 것은 **`purgeStatusSet[fields[4]]`
// 검사 단독**이다: 나머지 여덟 칸은 전부 유효한데 status만 닫힌 집합 밖이라 거부된다.
func TestPurgeLogTailRejectsSplicedLine(t *testing.T) {
	p := writePurgeLog(
		t,
		"1755180000\tstartup-shadow\t336h0m0s/pwsh\t-\t1755180899\t1\t-\t-\t-",
	)
	entries, total, unparsed := purgeLogTail(p, 5)
	if total != 1 || unparsed != 1 || len(entries) != 0 {
		t.Fatalf("합성 행이 수용됐다 — total=%d unparsed=%d entries=%d want 1/1/0",
			total, unparsed, len(entries))
	}
}

// TestPurgeLogTailRejectsBadFieldShapes — 닫힌 집합 밖 값과 정수도 `-`도 아닌 칸을 거른다.
// 각 줄이 한 축만 어긋난다 — 어느 검사가 잡았는지 실패 메시지로 갈린다.
func TestPurgeLogTailRejectsBadFieldShapes(t *testing.T) {
	for name, line := range map[string]string{
		"path 밖":     "1755180000\tunknown-path\t-\t-\tok\t1\t-\t-\t-",
		"status 밖":   "1755180000\tcli-gc\t-\t-\tdone\t1\t-\t-\t-",
		"bytes 비정수":  "1755180000\tcli-gc\t-\t-\tok\t1\tmany\t-\t-",
		"cutoff 비정수": "1755180000\tcli-gc\t-\tsoon\tok\t1\t-\t-\t-",
	} {
		t.Run(name, func(t *testing.T) {
			entries, _, unparsed := purgeLogTail(writePurgeLog(t, line), 5)
			if unparsed != 1 || len(entries) != 0 {
				t.Fatalf("unparsed=%d entries=%d want 1/0", unparsed, len(entries))
			}
		})
	}
}
