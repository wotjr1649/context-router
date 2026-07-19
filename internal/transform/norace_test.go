//go:build !race

package transform

// raceEnabled: race_test.go의 !race 짝 — 빌드 태그 관용구라 파일 분리가 불가피하다.
const raceEnabled = false
