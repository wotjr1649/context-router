// Package-level: `.mcp.json`과 `.claude/settings.json`의 **읽기 전용 판독기**. 소유 표식 판정,
// 등록물 감지(doctor [20]), 승인 규칙 정합 검사(doctor [19])가 여기 산다.
//
// 병합·기입 쪽(mergeMCPServers·mergeEnabledServers와 그 부속 — 프로필 해석·항목 키 왕복
// 보존·스코프 승자 판정)은 v0.19에서 전부 지웠다(D96 계약 1): 우리는 `.mcp.json`에도
// 승인 키에도 쓰지 않고, 등록·제거는 호스트 CLI(`claude mcp remove`)가 맡는다.
package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ctrMCPServerName — **옛 설치기가 쓰던 서버 이름**. v0.19에는 이 이름으로 등록하는 코드가
// 없다(D96 계약 1) — 남은 용도는 doctor [20]의 읽기 전용 감지기가 잔존물에서 찾을 이름
// 하나다. 현재 살아 있는 서버 이름은 플러그인 매니페스트(`plugin/mcp.json`)의 `ctr`이고,
// 도구 접두는 그 매니페스트에서 파생된 ctrToolPrefix가 든다(D98).
const ctrMCPServerName = "ctr-exec"

// supersededMCPServerNames — doctor [20]이 함께 찾는 **더 옛 이름**. D63 ②의 단일 서버 등록이
// 이 이름을 대체한 뒤로 옛 설치기는 이 이름을 만들지 않았다. 이름 `ctr` 자체는 플러그인
// 매니페스트에서 다시 살아 있지만, 여기서 찾는 대상은 그 플러그인 서버가 아니라 옛 기입
// 경로가 `.mcp.json`·`enabledMcpjsonServers`에 남긴 항목이다 — 플러그인 서버는 호스트가
// 자기 상태에 들고 있어 이 파일들에 나타나지 않는다. ctr-global은 다른 프로필
// (global-search)이라 대체 대상이 아니었다 — 옛 설치기가 만들지도, 지우지도 않았다.
var supersededMCPServerNames = []string{"ctr"}

// isOurMarkerValue — 소유 표식 값의 기준(D82·D84). **정확 일치 `context-router`**(무버전 —
// D82 이후 훅 등록물과 hostSnippet이 쓰는 값)이거나 **`context-router/` 접두**(버전 있는 값)일
// 때만 소유다. 키의 존재만으로는 소유가 아니다. 정확 일치 절은 과도기 장치가 아니라 영구
// 본절이며 D84의 v1.0 제거 대상에서 제외한다 — 지우면 v0.15 이후 설치본이 소유 판정에서
// 탈락해 대칭 제거가 깨진다.
func isOurMarkerValue(v string) bool {
	return v == hookBinaryName || strings.HasPrefix(v, hookMarkerPrefix())
}

// mcpConfigPath — 프로젝트 루트의 .mcp.json 경로.
func mcpConfigPath(projectRoot string) string {
	return filepath.Join(projectRoot, ".mcp.json")
}

// mcpServerEntry — 등록 파일의 서버 항목 하나에서 우리가 읽는 두 필드. command까지 읽는 이유:
// 표식이 없어도 command가 우리 것이면 우리가 남긴 등록물이므로 잔존 보고 대상이다
// (ownedRegistration이 그 논리합을 든다).
type mcpServerEntry struct {
	Managed string `json:"__ctrManaged"`
	Command string `json:"command"`
}

// mcpServersDoc — MCP 등록 파일에서 우리가 읽는 형태. 최상위 `mcpServers` 객체 하나이고
// 프로젝트 `.mcp.json`과 사용자 스코프 `~/.claude.json` 최상위가 같은 모양이다(설계 v0.12의
// 스코프 표).
type mcpServersDoc struct {
	Servers map[string]mcpServerEntry `json:"mcpServers"`
}

// mcpServerEntries — 등록 파일을 **파일당 한 번** 언마샬해 서버 항목 맵과 파싱 성패를 함께
// 돌려준다. 파싱 실패는 ok=false다 — doctor [20]이 그것을 조용히 "잔존물 없음"으로 읽지 않게
// 하는 판정원이고([16]의 codexTOMLParses와 같은 자리, 최종 리뷰 S5), 쉼표 하나가 남은 파일은
// 파싱만 실패하고 등록물은 그대로 살아 있다.
//
// 판정과 조회가 **한 언마샬을 공유하는** 것이 이 함수의 형태다(릴리스 리뷰 F7). 앞선 구현은
// 파싱 판정 1회 + 확인할 이름마다 1회로 같은 바이트를 파일당 세 번 훑었다 — 그 파일 하나가
// 프로젝트별 이력을 통째로 든 `~/.claude.json`이라 무거운 사용자에게서 수십 MB를 반복해
// 파싱했다. 한 자리로 합치면 두 판정이 같은 형태를 본다는 성질도 구조적으로 보장된다(형태가
// 두 벌로 갈라지면 한쪽만 받아들이는 파일에서 두 판정이 서로 모순된다).
func mcpServerEntries(data []byte) (map[string]mcpServerEntry, bool) {
	var doc mcpServersDoc
	if json.Unmarshal(data, &doc) != nil {
		return nil, false
	}
	return doc.Servers, true // nil 맵 조회는 (제로값, false)라 "mcpServers 키 없음"이 자연히 부재로 귀결된다
}

// permissionRules — settings 파일에서 읽는 규칙 배열.
type permissionRules struct {
	Permissions struct {
		Ask   []string `json:"ask"`
		Allow []string `json:"allow"`
	} `json:"permissions"`
}

// ruleMatches — ask 규칙 r과 allow 규칙 a가 가리키는 도구 집합이 겹치는가. 겹치면 그 교집합의
// 도구에서 allow는 효력이 없다. 세 형태를 다룬다: 리터럴 완전 일치, **서버 단위 규칙("mcp__server" —
// 그 서버의 전 도구를 덮는 문서화된 형태)**, 도구 위치 접미 glob("mcp__server__prefix_*"). 서버
// 세그먼트에는 glob이 오지 않는다.
// 형태 확장은 두 인자에 대칭으로 적용한다 — 한쪽(ask)에만 넓히면 서버 단위·와일드카드 **allow**가
// 진단에서 거짓 clean으로 나온다. ask가 그 집합 안의 도구를 가리키면 프롬프트는 그대로 강제된다
// (최종 리뷰 F5의 근거를 allow 쪽까지 적용한 것이다).
// 판정은 mcp__ 접두 규칙에 한정한다: 매칭 규칙이 그 형태에만 정의돼 있고, 비-MCP 규칙(Read/Edit
// 형태)은 인자에 절대경로를 담을 수 있어 진단 라인에 그대로 실리면 안 된다(리뷰 F5, §12).
// 두 인자 모두를 걸러 여기 한 곳에서 비교 범위와 출력 범위가 함께 좁혀진다 — 아래 형태 확장은
// 모두 이 관문 뒤에 있다.
func ruleMatches(r, a string) bool {
	if !strings.HasPrefix(r, "mcp__") || !strings.HasPrefix(a, "mcp__") {
		return false
	}
	rp, rLiteral := ruleToolSet(r)
	ap, aLiteral := ruleToolSet(a)
	switch {
	case rLiteral && aLiteral:
		// 리터럴끼리는 완전 일치만이다 — 접두로 비교하면 ctr_index가 ctr_indexer를 덮는다고 오판한다.
		return rp == ap
	case rLiteral:
		return strings.HasPrefix(rp, ap)
	case aLiteral:
		return strings.HasPrefix(ap, rp)
	}
	// 둘 다 집합이면 한쪽 접두가 다른 쪽 접두를 포함할 때만 겹친다(좁은 쪽이 곧 교집합).
	return strings.HasPrefix(rp, ap) || strings.HasPrefix(ap, rp)
}

// ruleToolSet — 규칙 하나가 가리키는 도구 집합을 "접두 + 리터럴 여부"로 환원한다. 서버 단위 규칙은
// 구분자까지 붙여 접두로 만든다 — 구분자 없이 이름 접두만 보면 이름이 그 규칙으로 시작하는 **다른**
// 서버(mcp__ctr-exec2__…)의 도구까지 집합에 든다고 오판한다. 서버 단위 판정은 "mcp__" 뒤에 구분자가
// 더 없는가로 한다(도구 자리가 비어 있다는 뜻).
func ruleToolSet(rule string) (prefix string, literal bool) {
	if strings.HasSuffix(rule, "*") {
		return strings.TrimSuffix(rule, "*"), false
	}
	if !strings.Contains(strings.TrimPrefix(rule, "mcp__"), "__") {
		return rule + "__", false
	}
	return rule, true
}

// askShadowedAllows — 모든 스코프의 permissions를 모아, ask와 도구가 겹치는 allow 항목을 보고한다.
// 규칙 평가가 deny→ask→allow 순이라 그 교집합의 도구에서 allow는 효력이 없다(설계 v0.12 D64).
// 교집합이 allow의 전부가 아닐 수 있으므로(부분 겹침) 보고 문면은 "덮는다"가 아니라 "겹친다"다.
// permission 규칙은 스코프 간 concat+dedup으로 병합되므로 전 스코프를 합쳐 판정한다 —
// 덮어쓰기가 아니라 합집합이라 순회 순서가 결과를 바꾸지 않는다(enabledMcpjsonServers 키와 다른 점이다).
func askShadowedAllows(projectRoot string, readFile func(string) ([]byte, error)) ([]string, error) {
	userPath, err := hookSettingsPath(true, projectRoot)
	if err != nil {
		return nil, err
	}
	projectPath, err := hookSettingsPath(false, projectRoot)
	if err != nil {
		return nil, err
	}
	localPath := filepath.Join(projectRoot, ".claude", "settings.local.json")

	// 확인하지 못한 스코프는 "규칙 없음"이 아니다 — 조용히 건너뛰면 doctor가 거짓 clean을
	// 찍는다(리뷰 F1). 미존재만 확인된 상태("그 스코프에 규칙 없음")로 보고, 그 밖의 읽기·파싱
	// 실패는 판정 불가로 올린다. 오류 문면에는 경로·원문을 담지 않는다(§12).
	var asks, allows []string
	for _, p := range []string{userPath, projectPath, localPath} {
		b, err := readFile(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, errors.New("permissions: 설정 파일 읽기 실패")
		}
		var doc permissionRules
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, errors.New("permissions: 설정 파싱 실패")
		}
		asks = append(asks, doc.Permissions.Ask...)
		allows = append(allows, doc.Permissions.Allow...)
	}
	var shadowed []string
	seen := map[string]bool{}
	for _, a := range allows {
		for _, r := range asks {
			if ruleMatches(r, a) && !seen[a] {
				seen[a] = true
				shadowed = append(shadowed, a)
				break
			}
		}
	}
	return shadowed, nil
}
