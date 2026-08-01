// Package buildinfo — 제품 버전 단일 소스(스펙 v0.10 D56). 무의존 leaf — 아키텍처 문서 의존
// 그래프의 D13 예외(파편화가 아닌 단일 상수 leaf, store.OpenContext 선례와 동형 — Task 6에서
// 문서 갱신). commit/dirty/pseudo-version은 여기 절대 불포함(marker 등호 비교 보호 — D56).
package buildinfo

// productVersion — 릴리스 수동 지점 유일 1곳. **릴리스 커밋에서 이 값을 그 릴리스 버전으로
// 올린다** — dev 사이클에 "-dev" 접미를 두지 않는다(v0.10~v0.12의 실무가 그러했고, 지키지 않는
// 절차를 처방하는 주석이 부정확한 쪽이다: 설계 v0.13 D72). marker 등호 비교 보호와 태그 일치
// 검증은 스펙 v0.10 D56·§1.3. var(const 아님) — 향후 -ldflags -X 주입 전환 여지
// (unexported + accessor = 외부 변경 차단).
var productVersion = "0.17.2"

// ProductVersion — 전 소비처(CLI 배너·hook Producer·marker·doctor·MCP serverInfo·version
// 서브커맨드)의 유일 입구.
func ProductVersion() string { return productVersion }
