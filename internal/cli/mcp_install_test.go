package cli

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// scopeKeyForTest — 주입된 readFile이 받은 경로를 스코프 라벨로 바꾼다. 판별 순서가
// 중요하다: local도 projectRoot 하위이므로 파일명 검사가 먼저다.
func scopeKeyForTest(projectRoot, p string) string {
	sp, root := filepath.ToSlash(p), filepath.ToSlash(projectRoot)
	switch {
	case strings.HasSuffix(sp, "/settings.local.json"):
		return "LOCAL"
	case strings.HasPrefix(sp, root+"/"):
		return "PROJECT"
	case strings.HasSuffix(sp, "/.claude/settings.json"):
		return "USER"
	}
	return ""
}

// TestScopeKeyForTestSeparatesUserAndProject: 스텁 라벨러 자체의 회귀 방지. Windows에서
// t.TempDir()이 %USERPROFILE%\AppData\Local\Temp 하위라, 홈 접두로 가르면 project 경로까지
// USER로 찍혀 T5의 ask/allow 테스트가 빈 목록을 읽는다.
func TestScopeKeyForTestSeparatesUserAndProject(t *testing.T) {
	proj := t.TempDir()
	userPath, err := hookSettingsPath(true, proj)
	if err != nil {
		t.Fatalf("hookSettingsPath(user): %v", err)
	}
	projectPath, err := hookSettingsPath(false, proj)
	if err != nil {
		t.Fatalf("hookSettingsPath(project): %v", err)
	}
	localPath := filepath.Join(proj, ".claude", "settings.local.json")
	for _, c := range []struct{ path, want string }{
		{userPath, "USER"}, {projectPath, "PROJECT"}, {localPath, "LOCAL"},
	} {
		if got := scopeKeyForTest(proj, c.path); got != c.want {
			t.Errorf("scopeKeyForTest(%q)=%q want %q", c.path, got, c.want)
		}
	}
}

// TestAskShadowedAllows: ask와 allow가 같은 도구를 가리키면 그 도구를 보고한다.
// 평가 순서가 deny→ask→allow라 이 조합에서 allow는 효력이 없다.
func TestAskShadowedAllows(t *testing.T) {
	proj := t.TempDir()
	files := map[string]string{
		"PROJECT": `{"permissions":{"ask":["mcp__ctr-exec__ctr_execute","mcp__ctr-exec__ctr_execute_file"]}}`,
		"LOCAL":   `{"permissions":{"allow":["mcp__ctr-exec__ctr_execute"]}}`,
		"USER":    `{}`,
	}
	read := func(p string) ([]byte, error) {
		if s, ok := files[scopeKeyForTest(proj, p)]; ok {
			return []byte(s), nil
		}
		return nil, os.ErrNotExist
	}
	got, err := askShadowedAllows(proj, read)
	if err != nil {
		t.Fatalf("askShadowedAllows: %v", err)
	}
	if len(got) != 1 || got[0] != "mcp__ctr-exec__ctr_execute" {
		t.Errorf("got=%v, [mcp__ctr-exec__ctr_execute] 여야 한다", got)
	}
}

// TestAskShadowedAllowsGlob: ask의 도구 위치 glob이 allow의 리터럴을 덮는 경우도 잡는다.
func TestAskShadowedAllowsGlob(t *testing.T) {
	proj := t.TempDir()
	files := map[string]string{
		"PROJECT": `{"permissions":{"ask":["mcp__ctr-exec__ctr_*"]}}`,
		"LOCAL":   `{"permissions":{"allow":["mcp__ctr-exec__ctr_execute"]}}`,
	}
	read := func(p string) ([]byte, error) {
		if s, ok := files[scopeKeyForTest(proj, p)]; ok {
			return []byte(s), nil
		}
		return nil, os.ErrNotExist
	}
	got, err := askShadowedAllows(proj, read)
	if err != nil {
		t.Fatalf("askShadowedAllows: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got=%v, glob이 리터럴을 덮는 조합을 잡아야 한다", got)
	}
}

// TestAskShadowedAllowsServerWideAsk: 서버 단위 ask 규칙("mcp__ctr-exec" — 그 서버의 전 도구를 덮는
// 문서화된 형태)이 그 서버 도구의 allow를 가리는 조합도 잡는다. 이 형태를 놓치면 doctor [19]가
// "충돌 없음"이라는 거짓 clean을 낸다(최종 리뷰 F5). 이름이 접두로 겹치는 **다른** 서버(ctr-exec2)와
// 무관한 서버(ctr-global)의 allow는 보고 대상이 아니다 — 구분자 "__" 없이 접두만 보면 전자를
// 덮는다고 오판하므로 그 케이스를 함께 넣는다. 그래서 이 테스트는 "1건"으로만 통과한다.
func TestAskShadowedAllowsServerWideAsk(t *testing.T) {
	proj := t.TempDir()
	files := map[string]string{
		"PROJECT": `{"permissions":{"ask":["mcp__ctr-exec"]}}`,
		"LOCAL": `{"permissions":{"allow":["mcp__ctr-exec__ctr_execute",` +
			`"mcp__ctr-exec2__ctr_execute","mcp__ctr-global__ctr_search"]}}`,
	}
	read := func(p string) ([]byte, error) {
		if s, ok := files[scopeKeyForTest(proj, p)]; ok {
			return []byte(s), nil
		}
		return nil, os.ErrNotExist
	}
	got, err := askShadowedAllows(proj, read)
	if err != nil {
		t.Fatalf("askShadowedAllows: %v", err)
	}
	if len(got) != 1 || got[0] != "mcp__ctr-exec__ctr_execute" {
		t.Errorf("got=%v, 서버 단위 ask가 덮는 그 서버 도구 1건만이어야 한다", got)
	}
}

// TestAskShadowedAllowsWidenedAllowForms: 형태 확장(서버 단위·접미 glob)은 두 인자에 대칭으로
// 적용된다 — 판정은 "두 규칙의 도구 집합이 겹치는가"이므로 allow가 넓은 형태여도 그 안의 도구를
// 가리키는 ask는 여전히 프롬프트를 강제한다. 한쪽에만 확장을 적용하면 서버 단위 allow와 와일드카드
// allow가 진단에서 거짓 clean으로 나온다(G5). 겹치지 않는 형태를 같은 픽스처에 넣어 접두 비교가
// 리터럴끼리를 잡아먹지 않게(ctr_index vs ctr_indexer) 함께 고정한다 — 그래서 "2건"으로만 통과한다.
func TestAskShadowedAllowsWidenedAllowForms(t *testing.T) {
	proj := t.TempDir()
	files := map[string]string{
		"PROJECT": `{"permissions":{"ask":["mcp__ctr-exec__ctr_index"]}}`,
		"LOCAL": `{"permissions":{"allow":["mcp__ctr-exec","mcp__ctr-exec__*",` +
			`"mcp__ctr-exec2","mcp__ctr-exec__ctr_indexer","mcp__ctr-global__*"]}}`,
	}
	read := func(p string) ([]byte, error) {
		if s, ok := files[scopeKeyForTest(proj, p)]; ok {
			return []byte(s), nil
		}
		return nil, os.ErrNotExist
	}
	got, err := askShadowedAllows(proj, read)
	if err != nil {
		t.Fatalf("askShadowedAllows: %v", err)
	}
	if !slices.Equal(got, []string{"mcp__ctr-exec", "mcp__ctr-exec__*"}) {
		t.Errorf("got=%v, 서버 단위·접미 glob allow 2건만이어야 한다(다른 서버·다른 도구는 제외)", got)
	}
}

// TestAskShadowedAllowsClean: 겹치지 않으면 빈 목록이다.
func TestAskShadowedAllowsClean(t *testing.T) {
	proj := t.TempDir()
	files := map[string]string{
		"LOCAL": `{"permissions":{"allow":["mcp__ctr-exec__ctr_execute"]}}`,
	}
	read := func(p string) ([]byte, error) {
		if s, ok := files[scopeKeyForTest(proj, p)]; ok {
			return []byte(s), nil
		}
		return nil, os.ErrNotExist
	}
	got, err := askShadowedAllows(proj, read)
	if err != nil {
		t.Fatalf("askShadowedAllows: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got=%v, 비어야 한다", got)
	}
}

// TestAskShadowedAllowsReportsUnreadableScope: 확인하지 못한 스코프가 있으면 오류를 반환한다 —
// 조용히 건너뛰면 doctor가 "충돌 없음"이라는 거짓 clean을 찍는다(리뷰 F1). 미존재(os.ErrNotExist)만
// "그 스코프에 규칙 없음"으로 확인된 상태다. 오류 문면에는 경로가 들어가지 않는다(§12).
func TestAskShadowedAllowsReportsUnreadableScope(t *testing.T) {
	proj := t.TempDir()
	for _, c := range []struct {
		name string
		read func(string) ([]byte, error)
	}{
		{"읽기 실패", func(p string) ([]byte, error) {
			if scopeKeyForTest(proj, p) == "LOCAL" {
				return nil, errors.New("읽기 거부") // 미존재가 아닌 오류
			}
			return nil, os.ErrNotExist
		}},
		{"파싱 실패", func(p string) ([]byte, error) {
			if scopeKeyForTest(proj, p) == "LOCAL" {
				return []byte(`{"permissions":`), nil
			}
			return nil, os.ErrNotExist
		}},
	} {
		got, err := askShadowedAllows(proj, c.read)
		if err == nil {
			t.Errorf("%s: 오류가 없다(got=%v) — 확인 못 한 스코프를 충돌 없음으로 세면 안 된다", c.name, got)
			continue
		}
		if strings.Contains(err.Error(), proj) {
			t.Errorf("%s: 오류 문면에 경로가 새어나온다", c.name)
		}
	}
}

// TestAskShadowedAllowsIgnoresNonMCPRules: 비-MCP 규칙(Read/Edit 형태)은 인자에 절대경로를 담을
// 수 있어 비교·출력 범위 밖이다(리뷰 F5 — 진단 라인은 도구 이름만 낸다). 같은 픽스처의 mcp__
// 규칙은 그대로 잡아야 하므로 이 테스트는 "빈 목록"으로 통과할 수 없다.
func TestAskShadowedAllowsIgnoresNonMCPRules(t *testing.T) {
	proj := t.TempDir()
	files := map[string]string{
		"PROJECT": `{"permissions":{"ask":["Read(/abs/path/x.txt)","mcp__ctr-exec__ctr_execute"]}}`,
		"LOCAL":   `{"permissions":{"allow":["Read(/abs/path/x.txt)","mcp__ctr-exec__ctr_execute"]}}`,
	}
	read := func(p string) ([]byte, error) {
		if s, ok := files[scopeKeyForTest(proj, p)]; ok {
			return []byte(s), nil
		}
		return nil, os.ErrNotExist
	}
	got, err := askShadowedAllows(proj, read)
	if err != nil {
		t.Fatalf("askShadowedAllows: %v", err)
	}
	if len(got) != 1 || got[0] != "mcp__ctr-exec__ctr_execute" {
		t.Errorf("got=%v, mcp__ 규칙 1건만이어야 한다(비-MCP 규칙은 경로를 담을 수 있어 출력 금지)", got)
	}
}

// TestHostSnippetSingleServerRegistration — D96·D97·D98 재기준선(Task 4). 손편집 .mcp.json
// 예시는 더는 싣지 않는다(등록은 이제 플러그인 설치 절차이고, 우리는 어떤 호스트 설정
// 파일에도 쓰지 않는다 — D96 계약 1). 이 테스트가 지키던 "이중 등록 방지"라는 목적은 살아
// 있다 — 옛 JSON 예시가 그 자리였고, 지금은 설치 절차의 0번 걸음(옛 등록물 제거)이 같은
// 목적을 진다(A⑧ — 호스트가 command·args 일치 서버를 경고 없이 버린다).
func TestHostSnippetSingleServerRegistration(t *testing.T) {
	for _, retired := range []string{`"ctr": {`, `"` + ctrMCPServerName + `": {`} {
		if strings.Contains(hostSnippet, retired) {
			t.Errorf("손편집 .mcp.json 등록 예시가 남아 있다(D96 계약 1 — 그 경로를 더는 안내하지 않는다): %q\n%s", retired, hostSnippet)
		}
	}
	if !strings.Contains(hostSnippet, "옛 등록물을 먼저 지운다") {
		t.Errorf("이중 등록을 막는 0번 걸음 안내가 없다:\n%s", hostSnippet)
	}
}

// TestHostSnippetUsesCurrentServerPrefix — D98 재기준선(Task 4). permission 예시가 은퇴한
// 도구 접두를 가리키지 않는다. 도구 접두가 mcp__<서버>__에서 mcp__plugin_<플러그인>_<서버>__로
// 옮겨갔으므로(D96 "사용자에게 보이는 변화"), 옛 mcp__ctr-exec__ 규칙을 그대로 복사한 사용자는
// 아무것도 매칭하지 않는 ask 규칙을 갖게 되고 ingest/net이 무보호로 남는다. ctr-global은
// 손편집 프로필 예시 자체가 D96 아래 안내 대상에서 빠졌으므로 그 접두도 함께 지운다.
func TestHostSnippetUsesCurrentServerPrefix(t *testing.T) {
	for _, retired := range []string{"mcp__ctr__", "mcp__ctr-exec__", "mcp__ctr-global__"} {
		if strings.Contains(hostSnippet, retired) {
			t.Errorf("은퇴한 서버 접두가 남아 있다: %q\n%s", retired, hostSnippet)
		}
	}
	for _, want := range []string{
		"mcp__plugin_context-router_ctr__ctr_index",
		"mcp__plugin_context-router_ctr__ctr_fetch_and_index",
	} {
		if !strings.Contains(hostSnippet, want) {
			t.Errorf("%q 규칙이 안내에서 사라졌다", want)
		}
	}
}
