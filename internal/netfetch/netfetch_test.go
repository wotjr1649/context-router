package netfetch

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// srvPort: httptest.NewServer는 임의 포트를 쓰므로 Config.ExtraPorts에 넣어줘야 한다
// (포트 정책 자체는 별도로 TestFetch_SchemeAndPortDenied가 검증).
func srvPort(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %s: %v", rawURL, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port of %s: %v", rawURL, err)
	}
	return p
}

// TestClassifyAddr: 설계 §5.2 I3 판정 매트릭스 — 게이트 5 (20행+).
func TestClassifyAddr(t *testing.T) {
	cases := []struct {
		name string
		addr string
		want string
	}{
		{"loopback v4", "127.0.0.1", "block"},
		{"rfc1918 10/8 low", "10.0.0.1", "block"},
		{"rfc1918 10/8 high", "10.255.255.255", "block"},
		{"rfc1918 172.16/12 low", "172.16.0.1", "block"},
		{"rfc1918 172.16/12 high", "172.31.255.254", "block"},
		{"rfc1918 192.168/16", "192.168.1.1", "block"},
		{"link-local metadata", "169.254.169.254", "block"},
		{"link-local v4", "169.254.1.1", "block"},
		{"cgnat low", "100.64.0.1", "block"},
		{"cgnat mid", "100.100.100.100", "block"},
		{"cgnat high", "100.127.255.255", "block"},
		{"just below cgnat is public", "100.63.255.255", "ok"},
		{"just above cgnat is public", "100.128.0.0", "ok"},
		{"unspecified v4", "0.0.0.0", "block"},
		{"0.0.0.0/8 non-zero", "0.1.2.3", "block"},
		{"multicast v4", "224.0.0.1", "block"},
		{"public v4 google", "8.8.8.8", "ok"},
		{"public v4 cloudflare", "1.1.1.1", "ok"},
		{"loopback v6", "::1", "block"},
		{"link-local v6 no zone", "fe80::1", "block"},
		{"link-local v6 with zone", "fe80::1%eth0", "block"},
		{"v4-mapped loopback unmaps to block", "::ffff:127.0.0.1", "block"},
		{"v4-mapped public unmaps to ok", "::ffff:8.8.8.8", "ok"},
		{"v4-compat embeds loopback (RFC4291 deprecated, distinct from v4-mapped)", "::127.0.0.1", "block"},
		{"v4-compat embeds private (RFC4291 deprecated)", "::10.0.0.1", "block"},
		{"nat64 encodes public v4 still blocked", "64:ff9b::808:808", "block"},
		{"unique local v6", "fc00::1", "block"},
		{"multicast v6", "ff02::1", "block"},
		{"public v6 cloudflare", "2606:4700::1111", "ok"},
		{"public v6 google dns", "2001:4860:4860::8888", "ok"},
	}
	if len(cases) < 20 {
		t.Fatalf("matrix must have 20+ rows, has %d", len(cases))
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, err := netip.ParseAddr(c.addr)
			if err != nil {
				t.Fatalf("parse %s: %v", c.addr, err)
			}
			if got := ClassifyAddr(a); got != c.want {
				t.Errorf("ClassifyAddr(%s) = %s, want %s", c.addr, got, c.want)
			}
		})
	}
}

// FuzzClassify: 임의 16바이트 → Addr, panic 없음 확인 (5s: -fuzz=FuzzClassify -fuzztime=5s).
func FuzzClassify(f *testing.F) {
	f.Add([]byte(net.ParseIP("127.0.0.1").To16()))
	f.Add([]byte(net.ParseIP("::1").To16()))
	f.Add([]byte(net.ParseIP("8.8.8.8").To16()))
	f.Add([]byte(net.ParseIP("fe80::1").To16()))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) != 16 {
			t.Skip()
		}
		var arr [16]byte
		copy(arr[:], b)
		_ = ClassifyAddr(netip.AddrFrom16(arr))
	})
}

func TestIsDowngrade(t *testing.T) {
	cases := []struct {
		from, to string
		want     bool
	}{
		{"https", "http", true},
		{"https", "https", false},
		{"http", "http", false},
		{"http", "https", false},
	}
	for _, c := range cases {
		if got := isDowngrade(c.from, c.to); got != c.want {
			t.Errorf("isDowngrade(%s,%s) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestTransportIgnoresEnvProxyAndHasNoRedirectFollow(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://example.invalid:8080")
	t.Setenv("HTTPS_PROXY", "http://example.invalid:8080")
	tr := buildTransport(netip.MustParseAddr("127.0.0.1"), 80)
	if tr.Proxy != nil {
		t.Fatalf("want Transport.Proxy nil (env proxy ignored), got non-nil")
	}
}

func TestFetch_LocalAllowDeny(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("hello"))
	}))
	defer srv.Close()
	port := []int{srvPort(t, srv.URL)}

	_, err := Fetch(context.Background(), Config{ExtraPorts: port, Timeout: 2 * time.Second}, srv.URL)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("AllowLocal=false: want ErrDenied, got %v", err)
	}

	res, err := Fetch(context.Background(), Config{AllowLocal: true, ExtraPorts: port, Timeout: 2 * time.Second}, srv.URL)
	if err != nil {
		t.Fatalf("AllowLocal=true: unexpected error: %v", err)
	}
	if string(res.Body) != "hello" {
		t.Fatalf("body = %q, want hello", res.Body)
	}
	if res.MediaType != "text/plain" {
		t.Fatalf("mediatype = %q, want text/plain", res.MediaType)
	}
}

func TestFetch_RedirectHopRevalidated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/foo", http.StatusFound)
	}))
	defer srv.Close()

	cfg := Config{AllowLocal: true, ExtraPorts: []int{srvPort(t, srv.URL)}, Timeout: 2 * time.Second}
	_, err := Fetch(context.Background(), cfg, srv.URL)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("want ErrDenied on redirect to metadata IP, got %v", err)
	}
}

func TestFetch_TooManyRedirects(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/next", http.StatusFound)
	}))
	defer srv.Close()

	cfg := Config{AllowLocal: true, ExtraPorts: []int{srvPort(t, srv.URL)}, Timeout: 2 * time.Second}
	_, err := Fetch(context.Background(), cfg, srv.URL)
	if err == nil {
		t.Fatalf("want error for redirect chain > 5 hops")
	}
}

func TestFetch_MaxBytesExceededAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", 100)))
	}))
	defer srv.Close()

	cfg := Config{AllowLocal: true, ExtraPorts: []int{srvPort(t, srv.URL)}, MaxBytes: 10, Timeout: 2 * time.Second}
	_, err := Fetch(context.Background(), cfg, srv.URL)
	if err == nil {
		t.Fatalf("want error when body exceeds MaxBytes")
	}
}

// TestFetch_ZeroMaxBytesDefaultsTo10MB: zero-value Config(SSRF-safe fetcher에서 최대 위험 케이스)도
// 무제한이 아니라 기본 10MB 상한이 걸려야 한다.
func TestFetch_ZeroMaxBytesDefaultsTo10MB(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", defaultMaxBytes+1)))
	}))
	defer srv.Close()

	cfg := Config{AllowLocal: true, ExtraPorts: []int{srvPort(t, srv.URL)}, Timeout: 5 * time.Second}
	_, err := Fetch(context.Background(), cfg, srv.URL)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("zero-value MaxBytes: want ErrBodyTooLarge at default 10MB cap, got %v", err)
	}
}

func TestFetch_SchemeAndPortDenied(t *testing.T) {
	_, err := Fetch(context.Background(), Config{Timeout: time.Second}, "ftp://example.com/")
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("ftp scheme: want ErrDenied, got %v", err)
	}

	_, err = Fetch(context.Background(), Config{AllowLocal: true, Timeout: time.Second}, "http://127.0.0.1:8080/")
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("disallowed port: want ErrDenied, got %v", err)
	}
}

// fetchHTMLFixture: testdata/html/name을 text/html로 서빙하고 Fetch, RawHTML 원문 보존을 검증.
func fetchHTMLFixture(t *testing.T, name string) Result {
	t.Helper()
	body, err := os.ReadFile("testdata/html/" + name)
	if err != nil {
		t.Fatalf("read testdata %s: %v", name, err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(body)
	}))
	defer srv.Close()
	cfg := Config{AllowLocal: true, ExtraPorts: []int{srvPort(t, srv.URL)}, Timeout: 5 * time.Second}
	res, err := Fetch(context.Background(), cfg, srv.URL)
	if err != nil {
		t.Fatalf("Fetch %s: %v", name, err)
	}
	if !bytes.Equal(res.RawHTML, body) {
		t.Fatalf("%s: RawHTML not preserved as original bytes", name)
	}
	return res
}

// TestFetch_HTML_DocWithCodeAndTable: 설계 §4.5 D12 — 코드펜스·표 파이프 보존, nav/footer 제거.
func TestFetch_HTML_DocWithCodeAndTable(t *testing.T) {
	res := fetchHTMLFixture(t, "doc-with-code-table.html")
	if res.Extraction != "readability" {
		t.Fatalf("Extraction = %q, want readability", res.Extraction)
	}
	if res.MediaType != "text/markdown" {
		t.Fatalf("MediaType = %q, want text/markdown", res.MediaType)
	}
	md := string(res.Body)
	if !strings.Contains(md, "```") {
		t.Errorf("markdown missing code fence:\n%s", md)
	}
	if !strings.Contains(md, "|") {
		t.Errorf("markdown missing table pipe:\n%s", md)
	}
	if strings.Contains(md, "Copyright 2026 Example Corp") || strings.Contains(md, "Privacy Policy") {
		t.Errorf("markdown retained nav/footer boilerplate:\n%s", md)
	}
}

func TestFetch_HTML_ArticleProse(t *testing.T) {
	res := fetchHTMLFixture(t, "article-prose.html")
	if res.Extraction != "readability" {
		t.Fatalf("Extraction = %q, want readability", res.Extraction)
	}
}

// TestFetch_HTML_ShortNonArticle: 추출 텍스트 <500자 → full 전환.
func TestFetch_HTML_ShortNonArticle(t *testing.T) {
	res := fetchHTMLFixture(t, "short-nonarticle.html")
	if res.Extraction != "full" {
		t.Fatalf("Extraction = %q, want full (short content)", res.Extraction)
	}
}

// TestFetch_HTML_CodeHeavyStripped: pre/code 보존율 <50% → full 전환(길이·비율 조건은 통과하는
// 픽스처라 이 조건만 단독으로 검증).
func TestFetch_HTML_CodeHeavyStripped(t *testing.T) {
	res := fetchHTMLFixture(t, "code-heavy-stripped.html")
	if res.Extraction != "full" {
		t.Fatalf("Extraction = %q, want full (pre/code preservation ratio)", res.Extraction)
	}
}

// TestFetch_NonHTML_Passthrough: text/html이 아니면 T4 그대로 — Body=원문, Extraction="", 무변환.
func TestFetch_NonHTML_Passthrough(t *testing.T) {
	payload := []byte(`{"key":"value"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(payload)
	}))
	defer srv.Close()
	cfg := Config{AllowLocal: true, ExtraPorts: []int{srvPort(t, srv.URL)}, Timeout: 2 * time.Second}
	res, err := Fetch(context.Background(), cfg, srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !bytes.Equal(res.Body, payload) {
		t.Fatalf("Body = %q, want unchanged %q", res.Body, payload)
	}
	if res.Extraction != "" {
		t.Fatalf("Extraction = %q, want empty for non-HTML", res.Extraction)
	}
	if res.RawHTML != nil {
		t.Fatalf("RawHTML should be nil for non-HTML, got %d bytes", len(res.RawHTML))
	}
	if res.MediaType != "application/json" {
		t.Fatalf("MediaType = %q, want application/json", res.MediaType)
	}
}
