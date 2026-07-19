// Package netfetch — SSRF-safe fetch. 설계서 §5.2, §4.5.
package netfetch

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// Config: Fetch 정책. 값 0/nil = 기본값(Timeout=30s, MaxBytes<=0 → 기본 10MB 상한).
type Config struct {
	AllowLocal bool
	ExtraPorts []int
	MaxBytes   int64
	Timeout    time.Duration
}

// Result: text/html일 때만 D12 파이프라인 적용(설계 §4.5, T5) — RawHTML=원문 보존,
// Body=markdown(추출 성공 시 readability 결과, 실패/저충실도 시 원문 전체를 변환),
// Extraction="readability"|"full", MediaType="text/markdown"으로 갱신. 그 외 미디어는
// T4 그대로 Body=원문·Extraction=""·MediaType=원본.
type Result struct {
	RawHTML    []byte
	Body       []byte
	MediaType  string
	Extraction string
	FinalURL   string
	// Title: readability Article.Title — text/html 경로에서만 채워진다(그 외 미디어는 "").
	Title string
}

// ErrDenied: SSRF 정책(주소/스킴/포트/강등/redirect 목적지) 위반 — 목적지 거부.
var ErrDenied = errors.New("netfetch: destination denied")

const (
	defaultMaxBytes  = 10 << 20 // 10MB — 설계 §4.5 fetch_and_index 기본과 정합.
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
	// v4CompatPrefix: RFC4291 폐지된 v4-compatible IPv6(예: ::127.0.0.1, ::10.0.0.1) — 임베디드
	// IPv4 대역. v4-mapped(::ffff:0:0/96, Unmap()이 걷어내는 대역)와는 다른 별개 대역이라
	// 명시적으로 차단해야 한다.
	v4CompatPrefix = netip.MustParsePrefix("::/96")
	// blockedPrefixes: IANA special-use 등록 대역(RFC 5737/2544/1112 등) — 공인 라우팅
	// 목적이 아니므로 목적지로 허용하지 않는다. loopback/private/link-local 등은 위 개별
	// 필드로 이미 처리되어 여기 포함하지 않는다.
	blockedPrefixes = []netip.Prefix{
		netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
		netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
		netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
		netip.MustParsePrefix("198.18.0.0/15"),   // benchmark
		netip.MustParsePrefix("240.0.0.0/4"),     // reserved
		netip.MustParsePrefix("2001:db8::/32"),   // documentation
		netip.MustParsePrefix("2001::/23"),       // IETF protocol assignments
		netip.MustParsePrefix("2002::/16"),       // 6to4
		netip.MustParsePrefix("2001::/32"),       // Teredo
	}
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
		zeroNet.Contains(a) ||
		v4CompatPrefix.Contains(a) {
		return "block"
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return "block"
		}
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
// block이면 전체 거부(의미 불변). fallback resolver 없음(I6) — net.DefaultResolver 1회만.
// 반환은 검증을 통과한 전체 주소 목록(각 Unmap() 적용) — literal IP는 단일 원소.
func resolveAndValidate(ctx context.Context, host string, cfg Config) ([]netip.Addr, error) {
	if lit, err := netip.ParseAddr(host); err == nil {
		if !allowedAddr(lit, cfg) {
			return nil, fmt.Errorf("netfetch: address %s denied: %w", host, ErrDenied)
		}
		return []netip.Addr{lit.Unmap()}, nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("netfetch: resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("netfetch: %s: no addresses: %w", host, ErrDenied)
	}
	out := make([]netip.Addr, len(addrs))
	for i, a := range addrs {
		if !allowedAddr(a, cfg) {
			return nil, fmt.Errorf("netfetch: %s resolves to denied address %s: %w", host, a, ErrDenied)
		}
		out[i] = a.Unmap()
	}
	return out, nil
}

// errAddrTimeout: 주소 1건의 dial+TLS handshake 공유 예산(perAddrConnTimeout) 초과 —
// classifyAddrCtxErr가 부모 ctx는 아직 살아있다고 판단했을 때만 이 sentinel로 감싼다
// (최종리뷰 F9). retryableDialErr가 이를 최우선으로 재시도 대상 분류해 다음 주소로
// 넘어간다 — 부모 ctx(fetch 전체) 자체의 취소·만료는 이 sentinel로 감싸지 않으므로 기존
// 그대로 non-retryable로 남는다.
var errAddrTimeout = errors.New("netfetch: 주소별 dial/TLS 예산 초과")

// retryableDialErr: hop 루프가 다음 주소로 재시도할지 판단 — 연결 계층 오류(dial·TLS
// handshake)만 참. client.Do는 HTTP 응답을 받으면 항상 err=nil이므로, 응답 수신 후 오류
// (상태코드·본문 등)는 애초에 이 함수의 판정 대상이 되지 않는다. 판정 순서(Fix Round 1
// 리뷰 P2-2, 최종리뷰 F9로 0번 추가):
//  0. errAddrTimeout(주소별 예산 초과, 부모 ctx는 생존) → true. 반드시 1번보다 먼저 검사한다
//     — 내부 dial/handshake 오류 체인이 우연히 context.DeadlineExceeded를 함께 감싸고
//     있어도 이 sentinel이 우선한다.
//  1. ctx 취소/데드라인(*net.OpError가 원인으로 감싸고 있어도) → false. 목적지 문제가
//     아니라 호출자 쪽 사정이라 다음 주소로 넘어가도 의미가 없다.
//  2. *net.OpError(dial 실패: connection refused 등) → true.
//  3. TLS handshake 실패 부류(레코드 헤더 오류·TLS alert·인증서 검증 오류) → true — 이
//     주소가 애초에 유효한 TLS 종단이 아니라는 신호라 다음 주소를 시도할 가치가 있다.
func retryableDialErr(err error) bool {
	if errors.Is(err, errAddrTimeout) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var recordHeaderErr tls.RecordHeaderError
	if errors.As(err, &recordHeaderErr) {
		return true
	}
	var alertErr tls.AlertError
	if errors.As(err, &alertErr) {
		return true
	}
	var hostnameErr x509.HostnameError
	if errors.As(err, &hostnameErr) {
		return true
	}
	var authorityErr x509.UnknownAuthorityError
	if errors.As(err, &authorityErr) {
		return true
	}
	var certErr x509.CertificateInvalidError
	return errors.As(err, &certErr)
}

// perAddrConnTimeout: 주소 시도 1건당 dial+TLS handshake 공유 예산(Fix Round 1 리뷰 P2-3,
// 최종리뷰 F9로 dial과 handshake가 하나의 예산을 공유하도록 재수정) — 없으면 첫 주소가
// SYN이나 handshake를 조용히 드롭할 때 fetch 전체 ctx(기본 30s)를 그 시도 혼자 소비해버려,
// 남은 주소들이 이미 만료된 ctx로 즉시 실패하고 재시도 기회를 못 받는다.
const perAddrConnTimeout = 10 * time.Second

// classifyAddrCtxErr: addrCtx(parent에서 파생된 주소별 자식 context)가 만료돼 dial/handshake가
// 실패했을 때, parent(fetch 전체 ctx)는 아직 살아있는지로 재시도 여부를 가른다(최종리뷰 F9).
// parent가 살아있고 addrCtx만 만료됐다면 이 주소 하나의 예산 초과일 뿐이므로 errAddrTimeout으로
// 감싸 retryableDialErr가 다음 주소로 넘어가게 한다. parent 자체가 만료·취소됐다면(fetch 전체
// 종료) 원본 오류를 그대로 반환해 기존 non-retryable 판정을 그대로 유지한다.
func classifyAddrCtxErr(parent, addrCtx context.Context, err error) error {
	if parent.Err() == nil && errors.Is(addrCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", errAddrTimeout, err)
	}
	return err
}

// dialAddr: 주소별 자식 context(perAddrConnTimeout)로 pinnedIP:port에 접속한다. dial과(https
// 경로에서는) TLS handshake가 이 자식 context 하나를 공유해야 "한 주소 시도"의 예산이 dial과
// handshake로 이중 소비되지 않는다(최종리뷰 F9(a)).
func dialAddr(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// buildTransport: dial(+https는 handshake까지)은 검증된 pinnedIP:port로만(I4) — Transport가
// 넘기는 addr은 무시하고 closure의 pinnedIP:port를 그대로 재사용(hop의 주소 시도마다
// Transport를 새로 만들어 넘기므로 항상 addr과 동일값), hostname 재조회 경로 자체가 없다.
// https는 DialTLSContext로 dial+handshake를 직접 수행해 perAddrConnTimeout 자식 context
// 하나를 dial과 handshake 전체에 공유한다(최종리뷰 F9) — 이전엔 net.Dialer.Timeout(dial)과
// Transport.TLSHandshakeTimeout(handshake)이 각자 별도 10s를 새로 소비해 한 주소 시도가
// 최악 20s를 먹었고, handshake 타임아웃은 net/http 비공개 타입(tlsHandshakeTimeoutError)이라
// retryableDialErr가 재시도 대상으로 분류하지도 못했다(다음 IP로 못 넘어감). serverName은
// 호출자가 원 요청 hostname(u.Hostname())을 넘겨 SNI/인증서 검증을 유지한다(I5,
// DialTLSContext 경로에서는 Transport가 자동 설정하지 않는다). Proxy: nil로 환경 프록시
// 무시(I6).
func buildTransport(pinnedIP netip.Addr, port int, serverName string) *http.Transport {
	addr := net.JoinHostPort(pinnedIP.String(), strconv.Itoa(port))
	return &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true, // 주소 시도마다 1회용 Transport — 재사용 없음, 유휴 소켓 잔존 방지.
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			addrCtx, cancel := context.WithTimeout(ctx, perAddrConnTimeout)
			defer cancel()
			conn, err := dialAddr(addrCtx, network, addr)
			if err != nil {
				return nil, classifyAddrCtxErr(ctx, addrCtx, err)
			}
			return conn, nil
		},
		DialTLSContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			addrCtx, cancel := context.WithTimeout(ctx, perAddrConnTimeout)
			defer cancel()
			conn, err := dialAddr(addrCtx, network, addr)
			if err != nil {
				return nil, classifyAddrCtxErr(ctx, addrCtx, err)
			}
			tlsConn := tls.Client(conn, &tls.Config{ServerName: serverName})
			if err := tlsConn.HandshakeContext(addrCtx); err != nil {
				conn.Close()
				return nil, classifyAddrCtxErr(ctx, addrCtx, err)
			}
			return tlsConn, nil
		},
	}
}

// ErrTooManyRedirects: redirect 체인이 maxRedirects를 초과 (자원/루프 한도 — 목적지 정책과 별개).
var ErrTooManyRedirects = errors.New("netfetch: too many redirects")

// ErrBodyTooLarge: 응답 본문이 Config.MaxBytes 초과 — 스트리밍 중단(I8, 계약: 절단 아닌 오류).
var ErrBodyTooLarge = errors.New("netfetch: response body exceeds MaxBytes")

// ErrUnsupportedMedia: 색인 파이프라인이 다루지 않는 미디어 타입(바이너리 등) — 처리 전 거부.
var ErrUnsupportedMedia = errors.New("netfetch: unsupported media type")

// mediaTypeAllowed: text/*·application/json·application/xml·application/xhtml+xml만 통과.
// Content-Type 없음/파싱 실패(mt=="")는 계약상 보수적으로 거부.
func mediaTypeAllowed(mt string) bool {
	if mt == "" {
		return false
	}
	if strings.HasPrefix(mt, "text/") {
		return true
	}
	switch mt {
	case "application/json", "application/xml", "application/xhtml+xml":
		return true
	}
	return false
}

// decodeToUTF8: Content-Type의 charset 파라미터(및 HTML이면 meta 태그) 기준으로 body를 UTF-8로
// 변환. charset이 없거나 이미 utf-8이면 원문 그대로(A5). RawHTML은 이 결과가 아닌 디코딩 전
// 원본 바이트를 보존해야 한다 — 호출부에서 반드시 body(원본)를 따로 유지할 것.
func decodeToUTF8(body []byte, contentTypeHeader string) ([]byte, error) {
	r, err := charset.NewReader(bytes.NewReader(body), contentTypeHeader)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// Fetch: I1~I8 전체 적용. scheme http/https만, redirect 매 hop 재검증(최대 5회),
// https→http 강등 hop 거부, 쿠키/자격 헤더 미전송(Jar 미설정), UA 고정.
func Fetch(ctx context.Context, cfg Config, rawURL string) (Result, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultMaxBytes
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
		if u.User != nil {
			return Result{}, fmt.Errorf("netfetch: userinfo not allowed: %w", ErrDenied)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return Result{}, fmt.Errorf("netfetch: scheme %q not allowed: %w", u.Scheme, ErrDenied)
		}
		port, err := resolvePort(u, cfg)
		if err != nil {
			return Result{}, err
		}
		addrs, err := resolveAndValidate(ctx, u.Hostname(), cfg)
		if err != nil {
			return Result{}, err
		}

		// 주소별 시도 — 연결 계층 오류(retryableDialErr)일 때만 다음 주소로 넘어간다.
		// HTTP 응답을 받은 뒤의 오류는 이 루프에 나타나지 않는다(client.Do가 이미 nil을
		// 반환했을 것이므로) — 재시도 금지 요구사항은 그 사실만으로 자동 충족된다.
		var resp *http.Response
		for i, addr := range addrs {
			transport := buildTransport(addr, port, u.Hostname())
			defer transport.CloseIdleConnections() // 주소 시도별 1회용 Transport 정리 — 반환/오류 경로 모두.
			client := &http.Client{
				Transport: transport,
				CheckRedirect: func(req *http.Request, via []*http.Request) error {
					return http.ErrUseLastResponse // 자동 redirect 비활성 — 아래 수동 루프가 처리(I6).
				},
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
			if err != nil {
				return Result{}, fmt.Errorf("netfetch: build request: %w", err)
			}
			req.Header.Set("User-Agent", defaultUserAgent)

			resp, err = client.Do(req)
			if err == nil {
				break
			}
			if i < len(addrs)-1 && retryableDialErr(err) {
				continue
			}
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
		if !mediaTypeAllowed(mediaType) {
			return Result{}, fmt.Errorf("netfetch: media type %q: %w", mediaType, ErrUnsupportedMedia)
		}
		decoded, err := decodeToUTF8(body, resp.Header.Get("Content-Type"))
		if err != nil {
			return Result{}, fmt.Errorf("netfetch: charset decode: %w", err)
		}
		result := Result{Body: decoded, MediaType: mediaType, FinalURL: current}
		if mediaType == "text/html" {
			result.RawHTML = body // 원문 바이트(디코딩 전) 보존 — 재처리/감사용.
			md, extraction, title, err := convertToMarkdown(decoded, u)
			if err != nil {
				return Result{}, err
			}
			result.Body = md
			result.Extraction = extraction
			result.MediaType = "text/markdown"
			result.Title = title
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

// fidelityMinChars/fidelityMinTextRatio/fidelityMinPreCodeRatio: 설계 §4.5 D12 충실도
// 판정 임계값 — 넷 중 하나라도 위반하면 readability 추출을 버리고 원문 전체를 쓴다(fail-open).
const (
	fidelityMinChars        = 500
	fidelityMinTextRatio    = 0.30
	fidelityMinPreCodeRatio = 0.50
)

// convertToMarkdown: D12 파이프라인 — readability 추출 → 충실도 판정 → html-to-markdown 변환.
// pageURL은 readability가 상대 링크를 절대화하는 데 사용. title은 readability가 추출에
// 성공하면(fidelityOK 결과와 무관하게) article.Title(), 실패하면 "".
func convertToMarkdown(rawHTML []byte, pageURL *url.URL) ([]byte, string, string, error) {
	contentHTML := string(rawHTML)
	extraction := "full"
	title := ""
	if article, err := readability.FromReader(bytes.NewReader(rawHTML), pageURL); err == nil {
		title = article.Title()
		if fidelityOK(rawHTML, article) {
			// /v2는 Content 문자열 대신 Node 렌더링 — 실패(Node nil)면 fail-open(full) 유지.
			// fidelityOK가 참이면 RenderText가 이미 성공했으므로 사실상 도달하지 않는 가드다.
			var buf bytes.Buffer
			if article.RenderHTML(&buf) == nil {
				contentHTML = buf.String()
				extraction = "readability"
			}
		}
	}
	md, err := htmlToMarkdown(contentHTML)
	if err != nil {
		return nil, "", "", fmt.Errorf("netfetch: html to markdown: %w", err)
	}
	return md, extraction, title, nil
}

// fidelityOK: 설계 §4.5 D12 — 아래 중 하나라도 참이면 false(=full 전환):
// 빈 추출·<500자·가시 텍스트 비율<30%·pre+code 보존율<50%.
func fidelityOK(rawHTML []byte, article readability.Article) bool {
	// /v2는 TextContent 필드 대신 RenderText — Node가 없으면 오류 = 빈 추출과 동일 취급.
	var textBuf bytes.Buffer
	if article.RenderText(&textBuf) != nil {
		return false
	}
	text := strings.TrimSpace(textBuf.String())
	if text == "" || len([]rune(text)) < fidelityMinChars {
		return false
	}
	origDoc, err := html.Parse(bytes.NewReader(rawHTML))
	if err != nil {
		return true // 원문 재파싱 실패 — 이미 Fetch가 받은 바이트이므로 사실상 발생하지 않음.
	}
	if origVisible := visibleTextLen(origDoc); origVisible > 0 {
		if float64(len([]rune(text)))/float64(origVisible) < fidelityMinTextRatio {
			return false
		}
	}
	if origPreCode := countPreCode(origDoc); origPreCode > 0 {
		if float64(countPreCode(article.Node))/float64(origPreCode) < fidelityMinPreCodeRatio {
			return false
		}
	}
	return true
}

// visibleTextLen: script/style 제외 텍스트 노드 rune 길이 합(node별 trim, 공백전용 노드는 0).
func visibleTextLen(n *html.Node) int {
	total := 0
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style") {
			return
		}
		if n.Type == html.TextNode {
			total += len([]rune(strings.TrimSpace(n.Data)))
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return total
}

// countPreCode: <pre>+<code> 엘리먼트 노드 수(중첩된 <pre><code>는 2개로 카운트 — 원문/추출
// 양쪽에 동일 규칙 적용이므로 보존율 비교엔 무관).
func countPreCode(n *html.Node) int {
	if n == nil {
		return 0
	}
	count := 0
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && (n.Data == "pre" || n.Data == "code") {
			count++
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return count
}

// htmlToMarkdown: base+commonmark+table 플러그인 — GFM 표(파이프)·코드펜스 보존.
func htmlToMarkdown(htmlContent string) ([]byte, error) {
	conv := converter.NewConverter(converter.WithPlugins(
		base.NewBasePlugin(),
		commonmark.NewCommonmarkPlugin(),
		table.NewTablePlugin(),
	))
	md, err := conv.ConvertString(htmlContent)
	if err != nil {
		return nil, err
	}
	return []byte(md), nil
}
