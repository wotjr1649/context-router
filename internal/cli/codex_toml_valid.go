package cli

import "github.com/pelletier/go-toml/v2"

// codexTOMLParses — 바이트가 TOML로 파스되는가(D89). **이 패키지에서 파서를 부르는 유일한
// 자리다.** 값을 읽지 않고 오류만 본다 — D80의 "파서 비의존"은 판정과 기입을 파서로 옮기는
// 것을 금지하며, 검증 전용 사용은 그 금지 밖이다. 접점을 한 함수로 좁혀 그 경계를 눈으로
// 확인할 수 있게 한다.
//
// 순수하다(파일 IO·시간·난수 없음) — 읽기 전용 doctor가 부르는 경로에 놓이므로 그 성질이
// 유지돼야 한다(D85).
func codexTOMLParses(b []byte) bool {
	var v any
	return toml.Unmarshal(b, &v) == nil
}
