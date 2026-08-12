package store

import (
	"errors"
	"testing"
	"time"
)

// TestArtifactIDNotReusedAfterPurge — D104 F10. artifacts.id는 rowid 별칭이고 스키마에
// AUTOINCREMENT가 없어, 퍼지가 최고 rowid를 지우면 SQLite가 그 id를 다음 INSERT에 재발급한다.
// 그러면 옛 artifact_id 참조가 **오류 없이 무관한 내용을 돌려준다** — ReadRange의 첫 조회가
// `WHERE id=?` 하나이기 때문이고, chunk 선택자도 chunks.id가 함께 재발급되어 막지 못한다
// `[실측 — 2026-08-12 재현: line·byte·chunk 셋 다 새 아티팩트의 내용을 반환했다]`.
//
// 이 테스트가 고정하는 것은 **id를 재발급하지 않는다**는 것 하나다. 그것이 서면 옛 참조는
// artifacts 조회에서 NOT_FOUND가 되므로 chunks.id 재사용은 도달 불가가 된다 — 그래서 셋을
// 다 단언하면서도 고치는 자리는 발급 한 곳이다.
func TestArtifactIDNotReusedAfterPurge(t *testing.T) {
	s := openT(t)
	ctx := t.Context()

	regOne := func(uri, body string) int64 {
		t.Helper()
		id, err := s.Register(ctx, Registration{
			StoredBytes: []byte(body), MediaType: "text/plain",
			Source: SourceMeta{URI: uri, Kind: "file", SrcHash: uri},
			Chunks: []Chunk{{Ordinal: 0, ByteStart: 0, ByteEnd: int64(len(body)), LineStart: 1, LineEnd: 1, Text: body}},
		})
		if err != nil {
			t.Fatalf("Register %s: %v", uri, err)
		}
		return id
	}

	idA := regOne("/a.txt", "FIRST-artifact-body\n")
	var chunkA int64
	if err := s.reader.QueryRowContext(ctx, `SELECT id FROM chunks WHERE artifact_id=?`, idA).Scan(&chunkA); err != nil {
		t.Fatalf("chunk id 조회: %v", err)
	}

	if _, _, err := s.PurgeOlderThan(ctx, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatalf("PurgeOlderThan: %v", err)
	}

	idB := regOne("/b.txt", "SECOND-artifact-body-completely-unrelated\n")
	if idB == idA {
		t.Fatalf("id 재사용: A와 B가 둘 다 %d — 퍼지 뒤 발급이 워터마크를 넘지 않았다", idA)
	}

	// 옛 참조는 세 선택자 모두 NOT_FOUND여야 한다 — artifacts 조회에서 먼저 막힌다.
	for _, tc := range []struct {
		name string
		sel  Selector
	}{
		{"line", Selector{Kind: "line", LineStart: 1, LineEnd: 1}},
		{"byte", Selector{Kind: "byte", ByteStart: 0, ByteEnd: 5}},
		{"chunk", Selector{Kind: "chunk", ChunkID: chunkA}},
	} {
		res, err := s.ReadRange(ctx, idA, tc.sel)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s 선택자: want ErrNotFound, got err=%v text=%q", tc.name, err, res.Text)
		}
	}
}

// TestRegisterSurvivesMissingIDWatermark — 워터마크 테이블이 없어도 등록은 계속돼야 한다.
// 그 테이블 생성은 ensureIndexes와 같은 fail-open 경로(경고만)이므로 없는 상태가 도달
// 가능하고, 없으면 발급이 현행 동작(max(id)+1)으로 되돌아갈 뿐 **등록이 깨지면 안 된다** —
// 이 결함을 고치다가 저장 자체를 못 하게 만드는 것이 가장 나쁜 결과다.
func TestRegisterSurvivesMissingIDWatermark(t *testing.T) {
	s := openT(t)
	ctx := t.Context()
	if _, err := s.writer.Exec(`DROP TABLE IF EXISTS id_watermark`); err != nil {
		t.Fatalf("DROP id_watermark: %v", err)
	}
	body := "body-without-watermark\n"
	if _, err := s.Register(ctx, Registration{
		StoredBytes: []byte(body), MediaType: "text/plain",
		Source: SourceMeta{URI: "/w.txt", Kind: "file", SrcHash: "hw"},
		Chunks: []Chunk{{Ordinal: 0, ByteStart: 0, ByteEnd: int64(len(body)), LineStart: 1, LineEnd: 1, Text: body}},
	}); err != nil {
		t.Fatalf("워터마크 테이블 없이 Register 실패(fail-open 위반): %v", err)
	}
}
