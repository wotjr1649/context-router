package cli

import (
	"github.com/pelletier/go-toml/v2"
)

// codexTOMLParses — 바이트가 TOML로 파스되는가. **이 패키지에서 파서를 부르는 유일한
// 자리다.** 값을 읽지 않고 오류만 본다 — D80의 "파서 비의존"은 판정과 기입을 파서로 옮기는
// 것을 금지하며, 검증 전용 사용은 그 금지 밖이다. 접점을 한 함수로 좁혀 그 경계를 눈으로
// 확인할 수 있게 한다.
//
// **소비처는 하나다: doctor [16]의 다음 걸음 분기**(cli.go). 파스되면 `codex mcp list`·
// `codex mcp remove`가 그 파일에 닿으므로 그 경로를 안내하고, 파스되지 않으면 Codex 자신도
// 그 파일을 못 읽어 두 명령 다 닿지 못하므로 짚은 줄을 직접 열라고 안내한다. 기입 게이트로
// 쓰이던 옛 소비처(installCodexConfigBlock)는 기입 경로와 함께 v0.19에서 사라졌다(D96 계약 1).
//
// 순수하다(파일 IO·시간·난수 없음) — 읽기 전용 doctor가 부르는 경로에 놓이므로 그 성질이
// 유지돼야 한다(D85).
//
// 선두 UTF-8 BOM은 판정 전에 뗀다. go-toml은 그 세 바이트를 키의 첫 글자로 읽어 거부하는데,
// Windows 편집기(PowerShell 5.1의 `Out-File -Encoding utf8`, 구버전 메모장)가 붙이는 바이트라
// 사용자 config.toml에 실재하고 **Codex 자신은 그 파일을 정상으로 읽는다** `[실측]`(BOM 붙인
// config.toml에서 codex mcp list가 등록물을 낸다). 갈리는 것은 우리 판정뿐이다 — 떼지 않으면
// [16]이 멀쩡한 파일에 "TOML로 파스되지 않는다"를 인쇄하고, 호스트 CLI로 정리할 수 있는
// 사용자에게 손으로 고치라고 안내한다.
//
// **다른 인코딩 사고는 범위 밖이다.** 레거시 코드페이지로 저장된 바이트는 TOML 명세가 요구하는
// UTF-8이 아니라 실제로 무효이고 Codex도 그 파일을 거부한다 `[실측]`(invalid utf-8 sequence) —
// 무효 판정이 사실이므로 고칠 것이 없다. 코드페이지를 추정해 옮기면 판정 대상이 사용자 파일의
// 실제 바이트와 달라진다.
func codexTOMLParses(b []byte) bool {
	var v any
	return toml.Unmarshal(trimBOM(b), &v) == nil
}
