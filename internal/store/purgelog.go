// 퍼지 감사 로그 — 삭제가 어디에도 기록되지 않던 구멍을 닫는다.
//
// **왜 원장 DB가 아닌가**: 퍼지는 lockStoreCtx를 쥔 60초 예산 구간에서 돌고, 그 자리의
// ledger INSERT는 best-effort라 실패하면 조용히 사라진다 — 그것이 애초의 문제였다. 판정
// 표면(stats의 tool별 표)도 건드리지 않는다: 그 표에 새 tool 행이 끼면 "계상 방식이
// 바뀌었다" 계열의 혼란이 하나 더 생긴다.
//
// 프로젝트 레벨인 이유는 삭제의 단위가 content.db이고 그것이 이 디렉터리에 있기 때문이다.
// worktree 레벨인 session.drops.log(hook 소유)와 층이 다르다.
package store

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// PurgeLogName — 사이드카 파일명. internal/cli가 이것을 직접 쓴다 — 미러 상수를 두지 않는다
// (F8): dropsFileName이 미러인 이유는 internal/hook의 dropsLogName이 unexported라서이고,
// 여기는 export하므로 그 명분이 없다(D13). 미러를 두면 그것을 지키는 가드 테스트가 또
// 필요해지는데, 그 테스트가 지키는 문제는 export가 이미 없앤다.
const PurgeLogName = "purge.log"

// purgeFieldMax — 필드당 상한(바이트). hook.appendDrop의 san과 같은 값이다.
const purgeFieldMax = 64

// PurgeRecord — 한 삭제 배치의 기록. 호출자가 채우고 AppendPurgeLog가 조립한다.
//
// **뒤 세 칸이 포인터인 이유**: 0은 "쟀더니 0이었다"는 주장이고, 함수가 애초에 그 값을
// 내지 않는 경로에서 그것을 쓰면 **거짓 측정**이 된다. 이 저장소는 같은 함정을 이미 한 번
// 지났다 — recordFetchResolve의 ageS *int64가 "0으로 두면 같은 초에 회수한 행과 구분되지
// 않아 분포가 내려간다"는 이유로 nil을 쓴다(D103 소견 F9). 같은 규칙: **못 잰 것은 `-`,
// 재서 0인 것은 `0`.** 경로별로 어느 칸이 채워지는지는 스펙 §2.0의 의미표가 든다.
type PurgeRecord struct {
	Path             string // 삭제 경로 라벨(닫힌 집합) — startup-shadow · cli-hook-only · cli-older-than · cli-gc
	Policy           string // 실효 정책 + 출처. PurgePolicy가 조립한다. cli-gc는 "-"
	Status           string // 결과 분류(닫힌 집합) — failed · partial · budget · cancelled · capped · ok
	Cutoff           int64  // 경계 unix 초. 0이면 "-"(cli-gc는 경계 개념이 없다)
	Count            int64  // 삭제 단위 수 — **의미가 경로마다 다르다**(스펙 §2.0)
	Bytes            *int64 // 실제 unlink된 바이트. nil이면 "-"
	Deferred, Failed *int   // age-gate 유예 / unlink 실패. nil이면 "-"
}

// PurgePolicy — 실효 보존값과 그 출처를 한 칸에 합친다.
//
// **한 칸인 이유**: 두 값이 항상 함께 읽히고, 합쳐 두면 성공 기준이 `grep '72h0m0s/'` 한
// 줄이 된다 — 200개가 사라진 그 기동의 행에 `72h0m0s/-`가 적혀 있었다면 그 규명은 한 줄
// 조회였다. 쪼개면 그 한 줄을 잃는다.
//
// **조립을 여기 한 곳에 두는 이유**(D13): 호출 지점이 셋이고, 각자 fmt.Sprintf로 손수
// 합치면 다음 수정이 한 곳에만 닿는다.
//
// **파싱은 첫 `/`로만 쪼갠다** — 앞칸인 Duration.String()에는 `/`가 없어 안전하고, 뒤칸인
// 출처는 사람이 임의 문자열을 넣는 자리라 `/`가 들어올 수 있다.
func PurgePolicy(d time.Duration, source string) string {
	if source == "" {
		source = "-"
	}
	return d.String() + "/" + source
}

// PurgeStatus — 퍼지 결과 분류(스펙 §2.2의 닫힌 집합). **우선순위가 계약이다**:
// partial이 budget·cancelled를 이긴다 — rep.Hashes>0이면 행 삭제 tx가 이미 커밋됐고
// (store.purgeHookRows는 실패 시 항상 빈 리포트를 반환하므로 그 값 자체가 커밋의 증거다)
// 행이 없어진 배치는 다음 기동 술어에 다시 안 잡히므로, 중단 사유가 무엇이든 남은 파일의
// 유일한 회수 경로가 purge --gc다. 순서가 뒤집히면 "다음 기동이 다시 집는다"는 **거짓
// 안내**가 나간다.
//
// **마지막 default가 0건 기동이다.** 그 자리가 이 설계의 요점이다.
func PurgeStatus(purgeErr error, hashes int, cancelled, budgetSpent, capped bool) string {
	switch {
	case purgeErr != nil && hashes > 0:
		return "partial"
	case purgeErr != nil && cancelled:
		return "cancelled"
	case purgeErr != nil && budgetSpent:
		return "budget"
	case purgeErr != nil:
		return "failed"
	case capped:
		return "capped"
	default:
		return "ok"
	}
}

// sanPurgeField — hook.appendDrop의 san과 **같은 규칙**이다: 탭·개행·CR을 공백으로, 64자
// 상한, 빈 값은 "-".
//
// **왜 부르지 않고 다시 쓰는가**: 그쪽은 클로저이고, 방향도 막혀 있다 — internal/hook이
// internal/store를 import하므로 store가 hook을 부를 수 없다.
//
// **★ 그리고 두 벌이 갈리는 것을 막는 장치는 없다**(검토 소견 F9). TestAppendPurgeLogSanitizes는
// 이쪽 사본만 잰다 — hook 쪽 san의 상한이나 치환 집합이 바뀌어도 아무것도 빨개지지 않는다.
// 진짜 해소는 이 규칙을 store에서 export하고 hook.appendDrop이 그것을 부르는 것이고(import
// 방향이 이미 그것을 허용한다), 그것은 internal/hook을 건드리므로 **별건으로 이월한다.**
// 여기 적어 두는 이유는 "테스트가 잠근다"는 거짓 주장을 남기지 않기 위해서다.
func sanPurgeField(s string) string {
	if s == "" {
		return "-"
	}
	s = strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(s)
	if len(s) > purgeFieldMax {
		s = s[:purgeFieldMax]
	}
	return s
}

// optInt64 / optInt — nil을 "-"로, 값이 있으면 10진수로.
func optInt64(p *int64) string {
	if p == nil {
		return "-"
	}
	return strconv.FormatInt(*p, 10)
}

func optInt(p *int) string {
	if p == nil {
		return "-"
	}
	return strconv.Itoa(*p)
}

// AppendPurgeLog — 기록 1행을 append한다. **best-effort**: 실패는 slog.Warn 한 줄이고
// 퍼지 결과·종료 코드에 영향이 없다(appendDrop과 동형). 반환값이 없는 이유는 호출자가
// 판정할 것이 없기 때문이다.
//
// **경고에 원인을 싣되 경로는 떼고 낸다**: 감사 기록이 실패한 이유를 설명하는 유일한 줄에서
// 그 이유를 빼면 stage 하나만 남는다. 그런데 이 셋의 오류는 전부 *os.PathError라 스토어
// 절대경로(프로젝트 id 포함)를 물고 오고, **설계 v0.0.1 §5.5는 그 금지를 slog(stderr)에
// 건다** — *"원문·쿼리 본문·절대경로·env 미기록"*. 그래서 sanitizeIOErr로 감싼다: syscall
// 원인은 남고 경로만 빠진다. 같은 패키지의 MergeFTSIfDue가 같은 모양(best-effort 파일 조작의
// 실패 경고)에서 이미 그렇게 한다.
//
// **키가 "path"가 아니라 "purge_path"인 이유**: rec.Path는 파일 경로가 아니라 삭제 경로
// 라벨(startup-shadow 따위)이다.
func AppendPurgeLog(projectDir string, rec PurgeRecord) {
	cutoff := "-"
	if rec.Cutoff != 0 {
		cutoff = strconv.FormatInt(rec.Cutoff, 10)
	}
	line := strings.Join([]string{
		strconv.FormatInt(time.Now().Unix(), 10),
		sanPurgeField(rec.Path),
		sanPurgeField(rec.Policy),
		cutoff,
		sanPurgeField(rec.Status),
		strconv.FormatInt(rec.Count, 10),
		optInt64(rec.Bytes),
		optInt(rec.Deferred),
		optInt(rec.Failed),
	}, "\t") + "\n"

	f, err := os.OpenFile(filepath.Join(projectDir, PurgeLogName),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		slog.Warn("퍼지 기록 실패", "stage", "open", "purge_path", rec.Path,
			"error", sanitizeIOErr("purge log open", err))
		return
	}
	// **닫기 실패도 보고한다**(검토 소견 F13). Windows에서는 지연된 쓰기 실패가 Close에서
	// 드러나므로, 그것을 버리면 **파일에 닿지 못한 append가 경고 없이 사라진다** — best-effort
	// 계약은 "퍼지 결과에 영향이 없다"이지 "실패를 숨긴다"가 아니다. 쓰기가 이미 실패했으면
	// 경고를 두 번 내지 않는다(호출당 한 줄).
	wrote := true
	if _, err := fmt.Fprint(f, line); err != nil {
		slog.Warn("퍼지 기록 실패", "stage", "write", "purge_path", rec.Path,
			"error", sanitizeIOErr("purge log write", err))
		wrote = false
	}
	if cerr := f.Close(); cerr != nil && wrote {
		slog.Warn("퍼지 기록 실패", "stage", "close", "purge_path", rec.Path,
			"error", sanitizeIOErr("purge log close", cerr))
	}
}
