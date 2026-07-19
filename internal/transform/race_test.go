//go:build race

package transform

// raceEnabled: -race 빌드 여부(빌드 태그 관용구). TestSpawn_DefaultTimeout이 -race 스텝에서
// skip 판정하는 데 쓴다 — 근거·실측은 해당 테스트의 skip 주석 참조(worker_test.go).
const raceEnabled = true
