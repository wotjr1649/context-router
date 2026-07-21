// recover_test.go — 태스크9b Step1 ③④⑤⑥(설계 §6.3 수동 복구 CLI 7단계, G8). export·doctor·
// worktree 계약(①②⑦)은 9a가 이미 커버했다(cli_test.go).
package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wotjr1649/context-router/internal/store"
)

// maxImmediateRejectDuration — 서버 실행 중 recover 즉시 거부(⑥) 판정 상한. 논블로킹
// AcquireLock은 수 ms 내 실패해야 하므로 여유 있게 잡아도 "대기·재시도 없음"을 충분히 검증한다.
const maxImmediateRejectDuration = 2 * time.Second

// seedAndCorruptEvents — n개 이벤트(session_start 자동 1건 + n개 note)를 시드한 뒤 Close하고,
// session_events 루트 페이지의 셀 포인터 배열 영역을 훼손한다. rootpage·page_size는
// PRAGMA/sqlite_master로 실측해 하드코딩하지 않는다(스키마 변경에 취약해지지 않도록). 이
// 훼손 패턴은 실측 확인됨: PRAGMA quick_check가 malformed를 보고하면서도 SELECT는 앞부분
// 다수 행을 정상 반환하다가 SQLITE_CORRUPT로 중단된다(정확히 어느 id부터 영구 유실되는지는
// 이벤트 개수·페이지 배치에 따라 달라진다 — 실측상 이 상수들로는 항상 "앞부분 연속 구간은
// 살아있고 그 뒤 일부가 영구 유실"되는 형태가 재현된다). 반환값은 손상 전 (id → event_id)
// 매핑 — 인양 후 rowid 보존을 "재넘버링되지 않았는가"로 정확히 검증하기 위함(손실 패턴 자체는
// 구현 세부라 가정하지 않는다).
func seedAndCorruptEvents(t *testing.T, dir string, n int) map[int64]string {
	t.Helper()
	d, err := Open(dir, Options{Producer: "test/corrupt"})
	if err != nil {
		t.Fatal(err)
	}
	original := make(map[int64]string, n+1)
	for i := 0; i < n; i++ {
		id, eventID, _, err := d.Append(Event{Type: "note", Summary: fmt.Sprintf("evt-%d-%s", i, strings.Repeat("pad", 30))})
		if err != nil {
			t.Fatal(err)
		}
		original[id] = eventID
	}
	rows, err := d.Reader().Query("SELECT id, event_id FROM session_events")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		var eventID string
		if err := rows.Scan(&id, &eventID); err != nil {
			t.Fatal(err)
		}
		original[id] = eventID
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()

	var pageSize, rootPage int
	if err := d.Reader().QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if err := d.Reader().QueryRow("SELECT rootpage FROM sqlite_master WHERE name='session_events'").Scan(&rootPage); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(dir, dbFileName)
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	off := (rootPage-1)*pageSize + 50
	if off+40 > len(raw) {
		t.Fatalf("corrupt helper: offset out of range (size=%d off=%d)", len(raw), off)
	}
	cp := append([]byte(nil), raw...)
	for i := 0; i < 40; i++ {
		cp[off+i] = 0xEE
	}
	if err := os.WriteFile(dbPath, cp, 0o600); err != nil {
		t.Fatal(err)
	}
	return original
}

func bakFamilyFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	prefix := dbFileName + bakInfix
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			names = append(names, e.Name())
		}
	}
	return names
}

// TestRecover_CorruptDB_RescuesAndPublishes — 브리프 9b Step1 ③: 훼손 DB → 인양·게시·
// 마커 소멸·`.bak-<ts>` family 존재(원본 훼손 바이트 그대로 보존)·이벤트 rowid 보존(재넘버링
// 아님 — 유실 패턴(어느 id부터 잘리는지)은 구현 세부라 가정하지 않고, 손상 전 id→event_id
// 매핑과 정확히 일치하는지로 검증한다).
func TestRecover_CorruptDB_RescuesAndPublishes(t *testing.T) {
	dir := t.TempDir()
	const n = 400
	original := seedAndCorruptEvents(t, dir, n)

	corruptBytes, err := os.ReadFile(filepath.Join(dir, dbFileName))
	if err != nil {
		t.Fatal(err)
	}

	result, err := Recover(dir)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if result.NoOp || result.MarkerOnly {
		t.Fatalf("want real recovery, got %+v", result)
	}
	if result.RecoveredEvents <= 0 || result.RecoveredEvents >= int64(n+1) {
		t.Fatalf("RecoveredEvents=%d want in (0, %d) — 부분 손상 시나리오여야 함(전량 복구는 손상 미발동을 뜻함)", result.RecoveredEvents, n+1)
	}

	// 마커 소멸
	if _, statErr := os.Stat(filepath.Join(dir, recoverMarkerName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("marker should be removed after recovery, stat err=%v", statErr)
	}

	// .bak-<ts> family 존재 + 원본(훼손) 바이트 그대로 보존
	names := bakFamilyFiles(t, dir)
	if len(names) == 0 {
		t.Fatal("no .bak-<ts> family found")
	}
	var bakMain string
	for _, n := range names {
		if !strings.HasSuffix(n, "-wal") && !strings.HasSuffix(n, "-shm") {
			bakMain = n
		}
	}
	if bakMain == "" {
		t.Fatalf(".bak family has no main db member: %v", names)
	}
	bakBytes, err := os.ReadFile(filepath.Join(dir, bakMain))
	if err != nil {
		t.Fatal(err)
	}
	if string(bakBytes) != string(corruptBytes) {
		t.Fatal(".bak family bytes differ from pre-recovery corrupted original — 원본 불변 위반")
	}

	// 이벤트 rowid 보존: 게시된 session.db에서 읽은 (id, event_id) 각 쌍이 손상 전 매핑과
	// 정확히 일치해야 한다(재넘버링 금지 — 어느 id까지/부터 유실됐는지는 손상 세부라 가정하지
	// 않는다).
	reader, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	rows, err := reader.Query("SELECT id, event_id FROM session_events ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	var gotIDs []int64
	seen := int64(0)
	for rows.Next() {
		var id int64
		var eventID string
		if err := rows.Scan(&id, &eventID); err != nil {
			t.Fatal(err)
		}
		wantEventID, ok := original[id]
		if !ok {
			t.Fatalf("recovered id=%d not present in pre-corruption mapping — id 위조(재넘버링) 의심", id)
		}
		if eventID != wantEventID {
			t.Fatalf("id=%d event_id=%q want %q — rowid 보존 위반", id, eventID, wantEventID)
		}
		gotIDs = append(gotIDs, id)
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("published db read failed: %v", err)
	}
	_ = rows.Close()

	if seen != result.RecoveredEvents {
		t.Fatalf("published row count=%d want RecoveredEvents=%d", seen, result.RecoveredEvents)
	}
	if len(gotIDs) == 0 {
		t.Fatal("no recovered rows")
	}
	if gotIDs[0] != 1 {
		t.Fatalf("first recovered id=%d want 1 (session_start)", gotIDs[0])
	}
	for i := 1; i < len(gotIDs); i++ {
		if gotIDs[i] <= gotIDs[i-1] {
			t.Fatalf("ids not strictly increasing at %d: %d <= %d", i, gotIDs[i], gotIDs[i-1])
		}
	}
}

// TestRecover_ResumesAfterCrashBeforePublish — 브리프 9b Step1 ④(G8): 게시(⑥) 직전 단계에서
// 인위 중단(마커·인양·검증까지만 직접 호출, publishRescued는 호출하지 않음 — 프로세스 크래시
// 시뮬레이션) → Recover(dir) 재호출이 이어서 완료해야 한다.
func TestRecover_ResumesAfterCrashBeforePublish(t *testing.T) {
	dir := t.TempDir()
	_ = seedAndCorruptEvents(t, dir, 400)

	markerPath := filepath.Join(dir, recoverMarkerName)
	if err := createMarker(markerPath); err != nil {
		t.Fatalf("createMarker: %v", err)
	}
	if _, _, err := rescueAll(dir); err != nil {
		t.Fatalf("rescueAll: %v", err)
	}
	if err := verifyRescued(dir); err != nil {
		t.Fatalf("verifyRescued: %v", err)
	}
	// publishRescued를 의도적으로 호출하지 않는다 — "게시 직전 crash" 상태.

	if _, statErr := os.Stat(markerPath); statErr != nil {
		t.Fatalf("precondition: marker should still exist, stat err=%v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, dbFileName)); statErr != nil {
		t.Fatalf("precondition: original session.db should still exist, stat err=%v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, recoverTmpName)); statErr != nil {
		t.Fatalf("precondition: rescued tmp file should still exist, stat err=%v", statErr)
	}

	result, err := Recover(dir)
	if err != nil {
		t.Fatalf("resume Recover: %v", err)
	}
	if result.NoOp || result.MarkerOnly {
		t.Fatalf("want real (resumed) recovery, got %+v", result)
	}
	if result.RecoveredEvents <= 0 {
		t.Fatalf("RecoveredEvents=%d want >0 on resume", result.RecoveredEvents)
	}

	if _, statErr := os.Stat(markerPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("marker should be gone after resume, stat err=%v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, recoverTmpName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("tmp rescue file should be consumed(renamed) after resume, stat err=%v", statErr)
	}
	if len(bakFamilyFiles(t, dir)) == 0 {
		t.Fatal("resume should still produce a .bak-<ts> family")
	}

	reader, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	if err := quickCheck(reader); err != nil {
		t.Fatalf("published db not healthy after resume: %v", err)
	}
}

// TestRecover_ResumesAfterPublishInterruptedMidRename — Fix Round1 Critical(재리뷰 발견):
// publishRescued의 backupOriginal까지만 실행하고(원본 → .bak 완료) finishPublish(tmp→
// session.db)는 호출하지 않는 상태("게시 rename 도중 crash" — session.db 부재 + 검증 완료된
// 건강한 tmp + .bak family 존재)에서 Recover(dir) 재호출이 **재인양 없이** 게시만 마저
// 끝내야 한다. 수정 전 코드는 이 상태에서 rescueAll의 첫 줄(removeDBFamily)이 검증 완료된
// tmp를 삭제하고, 이어서 부재한 session.db를 열려다 영구 wedge됐다.
func TestRecover_ResumesAfterPublishInterruptedMidRename(t *testing.T) {
	dir := t.TempDir()
	seedAndCorruptEvents(t, dir, 400)

	markerPath := filepath.Join(dir, recoverMarkerName)
	if err := createMarker(markerPath); err != nil {
		t.Fatalf("createMarker: %v", err)
	}
	wantEvents, wantSessions, err := rescueAll(dir)
	if err != nil {
		t.Fatalf("rescueAll: %v", err)
	}
	if err := verifyRescued(dir); err != nil {
		t.Fatalf("verifyRescued: %v", err)
	}
	if _, err := backupOriginal(dir); err != nil {
		t.Fatalf("backupOriginal: %v", err)
	}
	// finishPublish를 의도적으로 호출하지 않는다 — "session.db 부재 + tmp = 검증된 건강
	// 인양본" crash 상태.

	if _, statErr := os.Stat(filepath.Join(dir, dbFileName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("precondition: session.db should be gone(renamed to bak), stat err=%v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, recoverTmpName)); statErr != nil {
		t.Fatalf("precondition: rescued tmp should still exist, stat err=%v", statErr)
	}
	if len(bakFamilyFiles(t, dir)) == 0 {
		t.Fatal("precondition: backup family should already exist")
	}

	result, err := Recover(dir)
	if err != nil {
		t.Fatalf("resume Recover: %v", err)
	}
	if result.NoOp || result.MarkerOnly {
		t.Fatalf("want real resumed-publish recovery, got %+v", result)
	}
	if result.RecoveredEvents != wantEvents || result.RecoveredSessions != wantSessions {
		t.Fatalf("resume RecoveredEvents/Sessions=%d/%d want %d/%d(재인양 없이 이전 인양 건수 그대로 보고)",
			result.RecoveredEvents, result.RecoveredSessions, wantEvents, wantSessions)
	}

	if _, statErr := os.Stat(markerPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("marker should be gone, stat err=%v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, recoverTmpName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("tmp should be consumed(renamed), stat err=%v", statErr)
	}

	reader, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	if err := quickCheck(reader); err != nil {
		t.Fatalf("published db not healthy after resume: %v", err)
	}
	var count int64
	if err := reader.QueryRow("SELECT COUNT(*) FROM session_events").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != wantEvents {
		t.Fatalf("published session_events count=%d want %d", count, wantEvents)
	}
}

// TestRecover_RestoresFromBackupWhenTmpMissingAfterPublishCrash — Fix Round1 Critical 방어
// 분기(리뷰 지시 3): session.db 부재 + 인양본(tmp)도 부재 + `.bak-<ts>` family 존재인 극단
// 상태(게시 rename 도중 crash 이후 tmp까지 사라진 경우)에서 Recover가 가장 최근 백업을
// session.db 자리로 복원한 뒤 처음부터 다시 인양·게시를 완료해야 한다.
func TestRecover_RestoresFromBackupWhenTmpMissingAfterPublishCrash(t *testing.T) {
	dir := t.TempDir()
	seedAndCorruptEvents(t, dir, 400)

	markerPath := filepath.Join(dir, recoverMarkerName)
	if err := createMarker(markerPath); err != nil {
		t.Fatalf("createMarker: %v", err)
	}
	if _, _, err := rescueAll(dir); err != nil {
		t.Fatalf("rescueAll: %v", err)
	}
	if err := verifyRescued(dir); err != nil {
		t.Fatalf("verifyRescued: %v", err)
	}
	backupName, err := backupOriginal(dir)
	if err != nil {
		t.Fatalf("backupOriginal: %v", err)
	}
	// tmp까지 사라진 극단 상태를 흉내(예: 디스크 이슈로 게시 대상 인양본이 추가로 유실) —
	// 직접 지운다.
	if err := removeDBFamily(filepath.Join(dir, recoverTmpName)); err != nil {
		t.Fatalf("removeDBFamily(tmp): %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(dir, dbFileName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("precondition: session.db should be gone, stat err=%v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, recoverTmpName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("precondition: tmp should be gone, stat err=%v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, backupName)); statErr != nil {
		t.Fatalf("precondition: backup should exist, stat err=%v", statErr)
	}

	result, err := Recover(dir)
	if err != nil {
		t.Fatalf("resume Recover: %v", err)
	}
	if result.NoOp || result.MarkerOnly {
		t.Fatalf("want real re-recovery via backup restore, got %+v", result)
	}
	if result.RecoveredEvents <= 0 {
		t.Fatalf("RecoveredEvents=%d want >0", result.RecoveredEvents)
	}

	if _, statErr := os.Stat(markerPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("marker should be gone, stat err=%v", statErr)
	}
	reader, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	if err := quickCheck(reader); err != nil {
		t.Fatalf("published db not healthy: %v", err)
	}
	// 복원에 쓰인 원래 백업이 정상 파이프라인에 의해 다시 인양·백업됐으므로(새 타임스탬프),
	// 최소 1개의 .bak-<ts> family가 남아있어야 한다.
	if len(bakFamilyFiles(t, dir)) == 0 {
		t.Fatal("expected at least one .bak-<ts> family to remain after re-pipeline")
	}
}

// TestRecover_HealthyDBWithLeftoverMarker_DeletesMarkerOnly — 브리프 9b Step1 ⑤(N-2 회귀):
// 게시가 이미 완료된 상태(건강한 session.db + .bak family 존재)에서 마커만 잔존 → 재실행이
// 마커만 삭제하고 session.db는 절대 건드리지 않는다("무작업 종료 금지"이되 재인양도 금지).
func TestRecover_HealthyDBWithLeftoverMarker_DeletesMarkerOnly(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, Options{Producer: "test/healthy"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := d.Append(Event{Type: "note", Summary: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	// 게시 완료 잔존 상태 조작: bak family(임의 내용) + 마커.
	bakPath := filepath.Join(dir, dbFileName+bakInfix+"20260101T000000.000000000Z")
	if err := os.WriteFile(bakPath, []byte("fake-backup-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(dir, recoverMarkerName)
	if err := os.WriteFile(markerPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	beforeDB, err := os.ReadFile(filepath.Join(dir, dbFileName))
	if err != nil {
		t.Fatal(err)
	}

	result, err := Recover(dir)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !result.MarkerOnly {
		t.Fatalf("want MarkerOnly=true, got %+v", result)
	}
	if result.RecoveredEvents != 0 || result.RecoveredSessions != 0 {
		t.Fatalf("MarkerOnly path should not report recovered rows, got %+v", result)
	}

	if _, statErr := os.Stat(markerPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("marker should be removed, stat err=%v", statErr)
	}
	afterDB, err := os.ReadFile(filepath.Join(dir, dbFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeDB) != string(afterDB) {
		t.Fatal("session.db bytes changed — N-2 경로는 건강한 DB를 건드리면 안 됨")
	}
	if _, statErr := os.Stat(bakPath); statErr != nil {
		t.Fatalf("pre-existing .bak file should remain untouched, stat err=%v", statErr)
	}
}

// TestRecover_ServerRunning_RejectsImmediately — 브리프 9b Step1 ⑥: 다른 핸들이 session.lock을
// shared로 보유 중(=서버 실행 중)이면 recover의 exclusive 취득이 즉시 실패해야 한다(대기 없음).
func TestRecover_ServerRunning_RejectsImmediately(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir, Options{Producer: "test/running"}) // shared lease 보유
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	start := time.Now()
	_, err = Recover(dir)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("err=%v want ErrLeaseHeld", err)
	}
	if elapsed > maxImmediateRejectDuration {
		t.Fatalf("Recover took %v — want immediate rejection(no blocking/retry), threshold=%v", elapsed, maxImmediateRejectDuration)
	}

	// lease가 여전히 정상 보유 중이어야(recover가 실패 경로에서 뭔가를 잘못 해제/훼손하지 않음).
	if _, lockErr := store.AcquireLock(filepath.Join(dir, lockFileName), false); lockErr == nil {
		t.Fatal("session.lock should still be exclusively unavailable — server(d)가 shared를 보유 중이어야 함")
	}
}

// TestRecover_SweepsIncompleteBakOrphans — 부채정리 ②: backupOriginal은 -shm→-wal→main
// 순으로 옮기므로 main rename 전 crash하면 main 멤버가 없는 .bak-<ts> sidecar 고아가 남는다
// (resumePublishFromTmp 주석의 "부분 이동 family는 포렌식 잔재로 남는다"). recover 완료 후
// 그런 불완전 ts family는 스윕돼 사라져야 하고, main을 포함한 완전한 백업은 보존돼야 한다.
func TestRecover_SweepsIncompleteBakOrphans(t *testing.T) {
	dir := t.TempDir()
	seedAndCorruptEvents(t, dir, 400)

	// main 없는 .bak-<oldts> sidecar 고아 주입(부분 이동 잔재). ts는 backupOriginal 신규 ts보다
	// 앞선 과거값(사전순 정렬).
	orphanTS := "20200101T000000.000000000Z"
	orphans := []string{
		dbFileName + bakInfix + orphanTS + "-wal",
		dbFileName + bakInfix + orphanTS + "-shm",
	}
	for _, name := range orphans {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := Recover(dir); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	for _, name := range orphans {
		if _, statErr := os.Stat(filepath.Join(dir, name)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("incomplete ts-orphan %s should be swept after recover, stat err=%v", name, statErr)
		}
	}

	// 완전한 백업(main 포함)은 스윕이 지우면 안 된다.
	if _, found, err := latestBackupMain(dir); err != nil {
		t.Fatal(err)
	} else if !found {
		t.Fatal("legitimate .bak-<ts> main should survive the orphan sweep")
	}
}
