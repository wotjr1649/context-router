// 퍼지 감사 로그의 읽는 쪽. 쓰는 쪽은 internal/store가 소유한다 — dropsByReason이
// internal/hook의 appendDrop을 읽는 그 분리를 그대로 잇는다.
package cli

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// purgeLogFields — 형식이 고정한 필드 수. 파서는 이 수 **이상**을 수용한다(아래).
// 파일명은 store.PurgeLogName을 그대로 쓴다 — 미러 상수를 두지 않는다(검토 소견 F8).
const purgeLogFields = 9

// purgePathSet / purgeStatusSet — 스펙 §2.1·§2.2의 닫힌 집합. **형식 검증의 주력이다**
// (검토 소견 F3): 필드 수만 보면 합성 행이 통과한다.
var (
	purgePathSet = map[string]bool{
		"startup-shadow": true, "cli-hook-only": true, "cli-older-than": true, "cli-gc": true,
	}
	purgeStatusSet = map[string]bool{
		"failed": true, "partial": true, "budget": true,
		"cancelled": true, "capped": true, "ok": true,
	}
)

// isDashOrInt — `-`(미측정)이거나 10진 정수인가.
func isDashOrInt(s string) bool {
	if s == "-" {
		return true
	}
	_, err := strconv.ParseInt(s, 10, 64)
	return err == nil
}

// purgeEntry — 로그 한 줄. 뒤 세 칸이 문자열인 이유는 `-`(미측정)를 그대로 날라야 하기
// 때문이다 — int로 받으면 "못 잰 것"과 "재서 0"이 같은 값이 된다.
type purgeEntry struct {
	TS                      int64
	Path, Policy, Status    string
	Cutoff                  int64
	Count                   int64
	Bytes, Deferred, Failed string
}

// stripCtrl — 렌더 직전에 제어 문자를 뺀다. **파스 단계가 아니라 여기다**: 이 문자를 이유로
// 줄을 거부하면 실재한 삭제 기록 하나가 사라지고, 쓰는 쪽 sanPurgeField를 고치면 그쪽 주석이
// 경고한 hook.appendDrop 사본과의 갈림이 더 벌어진다. 값이 정상이면 무동작이다.
//
// **막는 것**: purge.log에 append할 수 있는 주체는 policy 칸에 유효한 형식과 임의 제어
// 바이트를 함께 실을 수 있고, doctor가 그것을 %s로 터미널에 그대로 낸다 — ANSI 이스케이프가
// 주변 진단 줄을 덮거나 지운다. 하필 그 표면이 **증거로 신뢰받는 것이 존재 이유**인 자리다.
func stripCtrl(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// purgeLogTail — 꼬리 n건을 **최신 순**으로 낸다. total은 빈 줄 포함 줄 수 계약이고
// unparsed는 형식에 안 맞는 줄 수다(dropsByReason 관례 — 진단은 절대 중단하지 않는다).
// aborted는 스캔이 끝까지 못 갔다는 별개 신호다 — 아래 sc.Err() 절이 그 이유를 든다.
//
// **9필드 이상을 수용하고 10번째부터 무시한다.** 스펙 §2.0이 열어 둔 확장이 append-only
// 파일에서 무해하려면 옛 파서가 새 줄을 버리지 않아야 한다. dropsByReason이 `== 2 || == 5`
// 인 것과 어긋나지 않는다 — 그쪽의 정확 매칭은 구형 2필드·신형 5필드(D43)가 **한 파일에
// 실제로 혼재하는 이력**을 가르기 위한 것이고, 퍼지 로그에는 그런 구형이 없다: 선례를 어긴
// 것이 아니라 같은 규칙이 다른 형상에서 다른 답을 낸 것이다.
//
// **이 길이 조건은 찢어져 이어붙은 줄을 걸러내지 않는다**(스펙 §4 — 초안의 "잘린 append는
// 9필드보다 짧게 나온다"는 주장은 거짓으로 판정됐다). append-only 파일에서 찢어진 조각 뒤에는
// 다음 writer의 레코드가 이어붙고, 절단점이 필드 경계에 떨어지면 합쳐진 줄도 9필드 이상으로
// 나올 수 있다 — 그 방어는 아래 루프의 닫힌 집합·형식 검사가 맡는다(검토 소견 F3).
//
// 파일 부재·열기 실패는 (nil, 0, 0, false) — dropsByReason과 동일 fail-soft다.
func purgeLogTail(path string, n int) (entries []purgeEntry, total int, unparsed int, aborted bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20) // 긴 줄에도 스캔 중단 방지(dropsByReason 관례)
	var all []purgeEntry
	for sc.Scan() {
		total++
		fields := strings.Split(sc.Text(), "\t")
		if len(fields) < purgeLogFields {
			unparsed++
			continue
		}
		ts, tsErr := strconv.ParseInt(fields[0], 10, 64)
		count, cErr := strconv.ParseInt(fields[5], 10, 64)
		// **필드 수만 보지 않는다**(검토 소견 F3). append-only 파일에서 찢어진 조각 뒤에는 다음
		// writer의 레코드가 이어붙고, 절단점이 필드 경계에 떨어지면 합쳐진 줄이 **정확히 9필드**가
		// 된다 — ts·path·count만 보면 그 합성 행이 통과해 doctor가 **일어나지 않은 삭제**를
		// 보고한다. 닫힌 집합 둘과 나머지 칸의 형식을 함께 요구해 그 창을 좁힌다.
		if tsErr != nil || cErr != nil ||
			!purgePathSet[fields[1]] || !purgeStatusSet[fields[4]] ||
			!isDashOrInt(fields[3]) || !isDashOrInt(fields[6]) ||
			!isDashOrInt(fields[7]) || !isDashOrInt(fields[8]) {
			unparsed++
			continue
		}
		cutoff, _ := strconv.ParseInt(fields[3], 10, 64) // "-"는 0 — 경계 개념 없음과 같은 표시다
		all = append(all, purgeEntry{
			TS: ts, Path: fields[1], Policy: fields[2], Cutoff: cutoff,
			Status: fields[4], Count: count,
			Bytes: fields[6], Deferred: fields[7], Failed: fields[8],
		})
	}
	// **스캔 중단을 침묵으로 넘기지 않는다**(검토 소견 F11). 1 MiB를 넘는 줄이 있으면 루프가
	// 조용히 멈춰 total이 과소 계상되고 "최근 N건"이 잘린 앞부분을 최신으로 보고한다 — 이 절이
	// 닫으려는 "침묵을 초록으로 읽는" 부류 그대로다.
	//
	// **unparsed로 세지 않는다**(전체 검토): 그러면 수천 줄을 못 읽고 멈춘 파일이 깨진 줄 하나와
	// 같은 문면(`파싱 실패 1줄`)으로 나와, 완화가 스스로 침묵을 다시 만든다. 별개 신호로 내고
	// doctor가 별개 절로 찍는다.
	aborted = sc.Err() != nil
	for i := len(all) - 1; i >= 0 && len(entries) < n; i-- {
		entries = append(entries, all[i])
	}
	return entries, total, unparsed, aborted
}
