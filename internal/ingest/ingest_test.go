package ingest

import (
	"bytes"
	"strings"
	"testing"
)

func TestRedact_Canaries(t *testing.T) {
	tests := []struct{ name, in, mustGone string }{
		{"aws", "key=AKIAIOSFODNN7EXAMPLE ok", "AKIAIOSFODNN7EXAMPLE"},
		{"github", "token ghp_abcdefghijklmnopqrstuvwxyz012345 x", "ghp_abcdefghijklmnopqrstuvwxyz012345"},
		{"privkey-multiline", "a\n-----BEGIN RSA PRIVATE KEY-----\nMIIE\nxyz\n-----END RSA PRIVATE KEY-----\nb", "MIIE"},
		{"authorization", "Authorization: Bearer eyJhbGciOi.something.sig", "eyJhbGciOi"},
		{"cookie", "Set-Cookie: session=SECRETVAL; Path=/", "SECRETVAL"},
		{"docker-auth", `{"auths":{"r.io":{"auth":"dXNlcjpwYXNzd29yZDEyMw=="}}}`, "dXNlcjpwYXNzd29yZDEyMw"},
		{"json-escaped", `{"t":"ghp_abcdefghijklmnopqrstuvwxyz012345"}`, "abcdefghijklmnopqrstuvwxyz012345"},
		{"password-kv", "password=hunter2xx;db=x", "hunter2xx"},
		// ghp_ — raw 바이트에 "ghp_"가 없음(아래 가드로 실증). unescape 뷰만 잡을 수 있음.
		{"json-escaped-real", `{"k":"gh\p_abcdefghijklmnopqrstuvwxyz012345"}`, "abcdefghijklmnopqrstuvwxyz012345"},
		{"jwt-bare", `{"token":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV"}`, "SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV"},
		{"slack", "hook xoxb-123456789012-abcdefghijklmnop end", "xoxb-123456789012-abcdefghijklmnop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "json-escaped-real" && strings.Contains(tt.in, "ghp_") {
				t.Fatal("입력에 평문 ghp_ 존재 — 2뷰(unescape) 경로 미검증")
			}
			out, spans := Redact([]byte(tt.in))
			if strings.Contains(string(out), tt.mustGone) {
				t.Fatalf("누출: %q 가 남음\n%s", tt.mustGone, out)
			}
			if spans == 0 {
				t.Fatal("spans=0")
			}
			if !strings.Contains(string(out), "«REDACTED:") {
				t.Fatalf("마커 없음: %s", out)
			}
		})
	}
}

// FuzzRedact: 불변식은 panic 없음(RE2 매치·슬라이스 조립 어디도 임의 입력에 패닉해선
// 안 됨). 시드는 canary 입력들.
func FuzzRedact(f *testing.F) {
	for _, s := range []string{
		"key=AKIAIOSFODNN7EXAMPLE ok",
		"token ghp_abcdefghijklmnopqrstuvwxyz012345 x",
		"a\n-----BEGIN RSA PRIVATE KEY-----\nMIIE\nxyz\n-----END RSA PRIVATE KEY-----\nb",
		"Authorization: Bearer eyJhbGciOi.something.sig",
		"Set-Cookie: session=SECRETVAL; Path=/",
		`{"auths":{"r.io":{"auth":"dXNlcjpwYXNzd29yZDEyMw=="}}}`,
		`{"t":"ghp_abcdefghijklmnopqrstuvwxyz012345"}`,
		"password=hunter2xx;db=x",
		`{"k":"gh\p_abcdefghijklmnopqrstuvwxyz012345"}`,
		`{"token":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV"}`,
	} {
		f.Add([]byte(s))
	}
	f.Add([]byte(`{"k":"gh` + string(rune(92)) + `u0070_abcdefghijklmnopqrstuvwxyz012345"}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		out, spans := Redact(b)
		if spans < 0 {
			t.Fatalf("negative spans: %d", spans)
		}
		_ = out
	})
}

func TestRedact_DoesNotMutateInput(t *testing.T) {
	in := []byte("key=AKIAIOSFODNN7EXAMPLE ok")
	orig := append([]byte(nil), in...)
	out, _ := Redact(in)
	if !bytes.Equal(in, orig) {
		t.Fatal("입력이 변조됨")
	}
	if bytes.Contains(out, []byte("AKIAIOSFODNN7EXAMPLE")) {
		t.Fatal("비밀 잔존")
	}
}

// TestRedact_UnicodeEscapedSecret: unescapeJSONBytes의 \uXXXX 디코드 분기(실전 은닉의
// 주 벡터) 실증 — json-escaped-real(Fix Round 1)은 default 분기(\p→p)만 거쳤다.
func TestRedact_UnicodeEscapedSecret(t *testing.T) {
	esc := string(rune(92)) + "u0070" // 백슬래시(92)+"u0070" == p('p')
	in := []byte(`{"k":"gh` + esc + `_abcdefghijklmnopqrstuvwxyz012345"}`)
	if bytes.Contains(in, []byte("ghp_")) {
		t.Fatal("입력에 평문 ghp_ 존재 — \\u 디코드 경로 미검증")
	}
	out, spans := Redact(in)
	if bytes.Contains(out, []byte("abcdefghijklmnopqrstuvwxyz012345")) {
		t.Fatalf("누출: %s", out)
	}
	if spans < 1 {
		t.Fatal("spans<1")
	}
}

func TestDeniedFilename(t *testing.T) {
	for _, p := range []string{".env", ".env.local", "id_rsa", "cert.pem", "x.har", "kubeconfig", "a/.docker/config.json", "k.jks", "s.p8", "cred.tfstate"} {
		if !DeniedFilename(p) {
			t.Fatalf("허용됨: %s", p)
		}
	}
	for _, p := range []string{"build.log", "main.go", "config.json"} { // 일반 config.json은 허용
		if DeniedFilename(p) {
			t.Fatalf("차단됨: %s", p)
		}
	}
}
