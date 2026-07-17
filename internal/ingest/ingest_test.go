package ingest

import (
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		out, spans := Redact(b)
		if spans < 0 {
			t.Fatalf("negative spans: %d", spans)
		}
		_ = out
	})
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
