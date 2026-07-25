// Package buildinfo — 제품 버전 단일 소스(스펙 v0.10 D56). 무의존 leaf — 아키텍처 문서 의존
// 그래프의 D13 예외(파편화가 아닌 단일 상수 leaf, store.OpenContext 선례와 동형 — Task 6에서
// 문서 갱신). commit/dirty/pseudo-version은 여기 절대 불포함(marker 등호 비교 보호 — D56).
package buildinfo

// productVersion — 릴리스 수동 지점 유일 1곳. dev 사이클 중 "-dev" 접미, 정식 릴리스 커밋에서
// 제거(마커 경계·도그푸딩 절차는 스펙 v0.10 D56·§1.3). var(const 아님) — 향후 -ldflags -X
// 주입 전환 여지(unexported + accessor = 외부 변경 차단).
var productVersion = "0.11.1"

// ProductVersion — 전 소비처(CLI 배너·hook Producer·marker·doctor·MCP serverInfo·version
// 서브커맨드)의 유일 입구.
func ProductVersion() string { return productVersion }
