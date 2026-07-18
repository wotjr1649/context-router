// Package netfetch — SSRF-safe fetch. 설계서 §5.2, §4.5.
package netfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"time"
)

// Config: Fetch 정책. 값 0/nil = 기본값(Timeout=30s, MaxBytes=무제한).
type Config struct {
	AllowLocal bool
	ExtraPorts []int
	MaxBytes   int64
	Timeout    time.Duration
}

// Result: T4에서는 Extraction=""·Body=원문 그대로(변환은 T5). RawHTML은 text/html일 때만
// Body와 동일 바이트로 채운다(그 외 미설정) — T5가 readability/html→md로 확장.
type Result struct {
	RawHTML    []byte
	Body       []byte
	MediaType  string
	Extraction string
	FinalURL   string
}

// ErrDenied: SSRF 정책(주소/스킴/포트/강등/redirect 목적지) 위반 — 목적지 거부.
var ErrDenied = errors.New("netfetch: destination denied")

const (
	defaultTimeout   = 30 * time.Second
	defaultUserAgent = "context-router/0.0.1"
	maxRedirects     = 5
)

var (
	loopback4 = netip.MustParseAddr("127.0.0.1")
	loopback6 = netip.MustParseAddr("::1")
	cgnat     = netip.MustParsePrefix("100.64.0.0/10")
	nat64     = netip.MustParsePrefix("64:ff9b::/96")
	zeroNet   = netip.MustParsePrefix("0.0.0.0/8")
)

// ClassifyAddr: 순수 함수 — I3 판정. "ok" | "block".
func ClassifyAddr(a netip.Addr) string {
	a = a.Unmap()
	if !a.IsValid() || a.Zone() != "" {
		return "block"
	}
	if !a.IsGlobalUnicast() ||
		a.IsLoopback() ||
		a.IsPrivate() ||
		a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() ||
		a.IsMulticast() ||
		a.IsUnspecified() ||
		cgnat.Contains(a) ||
		nat64.Contains(a) ||
		zeroNet.Contains(a) {
		return "block"
	}
	return "ok"
}

// allowedAddr: ClassifyAddr + --net-allow-local 예외(127.0.0.1/::1만).
func allowedAddr(a netip.Addr, cfg Config) bool {
	a = a.Unmap()
	if cfg.AllowLocal && (a == loopback4 || a == loopback6) {
		return true
	}
	return ClassifyAddr(a) == "ok"
}

// isDowngrade: https→http 강등 hop 거부 판정 (I7, 로직 단위 테스트 대상).
func isDowngrade(fromScheme, toScheme string) bool {
	return fromScheme == "https" && toScheme == "http"
}

// resolvePort: scheme 기본 포트(80/443) + ExtraPorts만 허용 (I7).
func resolvePort(u *url.URL, cfg Config) (int, error) {
	portStr := u.Port()
	port := 0
	if portStr == "" {
		if u.Scheme == "https" {
			port = 443
		} else {
			port = 80
		}
	} else {
		p, err := strconv.Atoi(portStr)
		if err != nil {
			return 0, fmt.Errorf("netfetch: bad port %q: %w", portStr, ErrDenied)
		}
		port = p
	}
	if port == 80 || port == 443 {
		return port, nil
	}
	for _, p := range cfg.ExtraPorts {
		if p == port {
			return port, nil
		}
	}
	return 0, fmt.Errorf("netfetch: port %d not allowed: %w", port, ErrDenied)
}

// resolveAndValidate: literal IP 우선(I1), 아니면 전 레코드 조회 후 전부 검증(I2) — 하나라도
// block이면 거부. fallback resolver 없음(I6) — net.DefaultResolver 1회만.
func resolveAndValidate(ctx context.Context, host string, cfg Config) (netip.Addr, error) {
	if lit, err := netip.ParseAddr(host); err == nil {
		if !allowedAddr(lit, cfg) {
			return netip.Addr{}, fmt.Errorf("netfetch: address %s denied: %w", host, ErrDenied)
		}
		return lit.Unmap(), nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("netfetch: resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return netip.Addr{}, fmt.Errorf("netfetch: %s: no addresses: %w", host, ErrDenied)
	}
	for _, a := range addrs {
		if !allowedAddr(a, cfg) {
			return netip.Addr{}, fmt.Errorf("netfetch: %s resolves to denied address %s: %w", host, a, ErrDenied)
		}
	}
	return addrs[0].Unmap(), nil
}

// buildTransport: dial은 검증된 pinnedIP:port로만(I4) — Transport가 넘기는 addr의 host부는
// 무시하고 port만 취해 재사용, hostname 재조회 경로 자체가 없다. TLSClientConfig는 미설정 —
// net/http가 원 요청 URL의 hostname으로 ServerName을 자동 설정하므로 SNI/인증서 검증은
// 정규화 hostname 기준으로 유지된다(I5). Proxy: nil로 환경 프록시 무시(I6).
func buildTransport(pinnedIP netip.Addr, port int) *http.Transport {
	dialer := &net.Dialer{}
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			p := strconv.Itoa(port)
			if _, gotPort, err := net.SplitHostPort(addr); err == nil {
				p = gotPort
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(pinnedIP.String(), p))
		},
	}
}

// ErrTooManyRedirects: redirect 체인이 maxRedirects를 초과 (자원/루프 한도 — 목적지 정책과 별개).
var ErrTooManyRedirects = errors.New("netfetch: too many redirects")

// ErrBodyTooLarge: 응답 본문이 Config.MaxBytes 초과 — 스트리밍 중단(I8, 계약: 절단 아닌 오류).
var ErrBodyTooLarge = errors.New("netfetch: response body exceeds MaxBytes")

// Fetch: I1~I8 전체 적용. scheme http/https만, redirect 매 hop 재검증(최대 5회),
// https→http 강등 hop 거부, 쿠키/자격 헤더 미전송(Jar 미설정), UA 고정.
func Fetch(ctx context.Context, cfg Config, rawURL string) (Result, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	current := rawURL
	redirects := 0
	for {
		u, err := url.Parse(current)
		if err != nil {
			return Result{}, fmt.Errorf("netfetch: parse url: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return Result{}, fmt.Errorf("netfetch: scheme %q not allowed: %w", u.Scheme, ErrDenied)
		}
		port, err := resolvePort(u, cfg)
		if err != nil {
			return Result{}, err
		}
		pinnedIP, err := resolveAndValidate(ctx, u.Hostname(), cfg)
		if err != nil {
			return Result{}, err
		}

		client := &http.Client{
			Transport: buildTransport(pinnedIP, port),
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse // 자동 redirect 비활성 — 아래 수동 루프가 처리(I6).
			},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return Result{}, fmt.Errorf("netfetch: build request: %w", err)
		}
		req.Header.Set("User-Agent", defaultUserAgent)

		resp, err := client.Do(req)
		if err != nil {
			return Result{}, fmt.Errorf("netfetch: request %s: %w", u.Redacted(), err)
		}

		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			resp.Body.Close()
			if loc == "" {
				return Result{}, fmt.Errorf("netfetch: redirect without Location")
			}
			redirects++
			if redirects > maxRedirects {
				return Result{}, fmt.Errorf("netfetch: %s: %w", u.Redacted(), ErrTooManyRedirects)
			}
			next, err := u.Parse(loc)
			if err != nil {
				return Result{}, fmt.Errorf("netfetch: bad redirect location: %w", err)
			}
			if isDowngrade(u.Scheme, next.Scheme) {
				return Result{}, fmt.Errorf("netfetch: https->http downgrade redirect: %w", ErrDenied)
			}
			current = next.String()
			continue
		}

		body, err := readBody(resp, cfg.MaxBytes)
		resp.Body.Close()
		if err != nil {
			return Result{}, err
		}
		mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if err != nil {
			mediaType = ""
		}
		result := Result{Body: body, MediaType: mediaType, FinalURL: current}
		if mediaType == "text/html" {
			result.RawHTML = body
		}
		return result, nil
	}
}

// readBody: MaxBytes<=0이면 무제한. 초과 시 절단이 아닌 오류로 중단(I8).
func readBody(resp *http.Response, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return io.ReadAll(resp.Body)
	}
	limited := io.LimitReader(resp.Body, maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("netfetch: read body: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrBodyTooLarge
	}
	return data, nil
}
