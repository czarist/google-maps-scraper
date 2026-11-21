package jshttp

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

var (
	connectMu       sync.Mutex
	connectLastCall time.Time
)

// connectMinInterval spaces out upstream CONNECT attempts across every proxy
// in the pool (a shared/rate-limited plan enforces its 429 account-wide, not
// per exit IP), so a big batch of concurrently running jobs can't all dial
// the upstream at once and trip its rate limit.
const connectMinInterval = 250 * time.Millisecond

// throttleConnect blocks until it is this goroutine's turn to dial the
// upstream proxy, enforcing connectMinInterval globally for the process.
func throttleConnect(ctx context.Context) error {
	for {
		connectMu.Lock()

		wait := connectMinInterval - time.Since(connectLastCall)
		if wait <= 0 {
			connectLastCall = time.Now()

			connectMu.Unlock()

			return nil
		}

		connectMu.Unlock()

		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// maxConcurrentPerProxy caps how many upstream tunnels can be open at once
// through a single exit IP. Chromium opens many simultaneous connections for
// one page (every subresource: scripts, images, XHRs, ...), and one browser
// stays pinned to one exit proxy for its whole session (ProxyPool.Next is
// called once per browser, not per request). Queuing the extras keeps a
// single exit from taking the whole account budget.
//
// A 429 from this gateway is not a signal about that exit IP. It is applied
// to the account (or the source IP) and is handled by noteUpstreamRateLimit,
// which stops the candidate walk instead of opening more connections.
//
// The cap is keyed by the upstream address and shared process-wide, not kept
// on the AuthProxy struct: every job builds its own ProxyPool with its own
// fresh AuthProxy instances, so two jobs that pick the same exit IP would
// otherwise each get their own budget for the same upstream.
const maxConcurrentPerProxy = 6

var (
	connectSemsMu sync.Mutex
	connectSems   = map[string]chan struct{}{}
)

func connectSemFor(upstream string) chan struct{} {
	connectSemsMu.Lock()
	defer connectSemsMu.Unlock()

	sem, ok := connectSems[upstream]
	if !ok {
		sem = make(chan struct{}, maxConcurrentPerProxy)
		connectSems[upstream] = sem
	}

	return sem
}

func acquireConnectSlotFor(ctx context.Context, upstreamHost string) error {
	sem := connectSemFor(upstreamHost)

	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func releaseConnectSlotFor(upstreamHost string) {
	sem := connectSemFor(upstreamHost)

	select {
	case <-sem:
	default:
	}
}

type ProxyType int

const (
	ProxyTypeHTTP ProxyType = iota
	ProxyTypeSOCKS5
)

type AuthProxy struct {
	server     *http.Server
	port       int
	upstream   *url.URL
	auth       string
	proxyType  ProxyType
	client     *http.Client
	socks5Auth *proxy.Auth
	bufferPool sync.Pool
	logger     *log.Logger
	shutdownCh chan struct{}
	once       sync.Once
	pool       *ProxyPool
}

// HTTPClient returns a client configured to use this proxy
func (p *AuthProxy) HTTPClient() *http.Client {
	proxyURL, _ := url.Parse(fmt.Sprintf("http://localhost:%d", p.port))

	return &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyURL(proxyURL),
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		Timeout: 120 * time.Second,
	}
}

// Port returns the port number the proxy is listening on
func (p *AuthProxy) Port() int {
	return p.port
}

func (p *AuthProxy) Address() string {
	return fmt.Sprintf("localhost:%d", p.port)
}

func createHTTPProxyClient(upstream *url.URL) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyURL(upstream),
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			DisableKeepAlives:     false,
		},
		Timeout: 120 * time.Second,
	}
}

func createSOCKS5ProxyClient(upstream *url.URL, auth *proxy.Auth) *http.Client {
	dialer, err := proxy.SOCKS5("tcp", upstream.Host, auth, proxy.Direct)
	if err != nil {
		return &http.Client{Timeout: 120 * time.Second}
	}

	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		DisableKeepAlives:   false,
	}

	if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
		transport.DialContext = contextDialer.DialContext
	} else {
		transport.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		}
	}

	return &http.Client{
		Transport: transport,
		Timeout:   120 * time.Second,
	}
}

// StartAuthProxy starts an HTTP proxy that authenticates against an upstream proxy (HTTP or SOCKS5)
func StartAuthProxy(proxyURL, username, password string) (*AuthProxy, error) {
	if proxyURL == "" {
		return nil, fmt.Errorf("proxy URL cannot be empty")
	}

	// Patched: credentials are optional so unauthenticated proxies also work.
	hasAuth := username != "" || password != ""

	upstreamURL, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL: %v", err)
	}

	var proxyType ProxyType

	switch upstreamURL.Scheme {
	case "http", "https":
		proxyType = ProxyTypeHTTP
	case "socks5", "socks5h":
		proxyType = ProxyTypeSOCKS5
	default:
		return nil, fmt.Errorf("unsupported proxy scheme: %s (supported: http, https, socks5, socks5h)", upstreamURL.Scheme)
	}

	upstream := *upstreamURL
	if proxyType == ProxyTypeHTTP && hasAuth {
		upstream.User = url.UserPassword(username, password)
	}

	var auth string
	if hasAuth {
		auth = base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	}

	authProxy := &AuthProxy{
		upstream:   &upstream,
		auth:       auth,
		proxyType:  proxyType,
		shutdownCh: make(chan struct{}),
		logger:     log.New(os.Stderr, "[AuthProxy] ", log.LstdFlags),
	}

	if proxyType == ProxyTypeSOCKS5 && hasAuth {
		authProxy.socks5Auth = &proxy.Auth{
			User:     username,
			Password: password,
		}
	}

	if proxyType == ProxyTypeSOCKS5 {
		authProxy.client = createSOCKS5ProxyClient(&upstream, authProxy.socks5Auth)
	} else {
		authProxy.client = createHTTPProxyClient(&upstream)
	}

	authProxy.bufferPool = sync.Pool{
		New: func() any {
			return make([]byte, 32*1024)
		},
	}

	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return nil, fmt.Errorf("failed to find free port: %v", err)
	}

	authProxy.port = listener.Addr().(*net.TCPAddr).Port //nolint:errcheck // always ok

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 10<<20) // 10MB max

		if r.Method == http.MethodConnect {
			authProxy.handleConnect(w, r)
		} else {
			authProxy.handleHTTP(w, r)
		}
	})

	authProxy.server = &http.Server{
		Handler:        handler,
		ReadTimeout:    60 * time.Second,
		WriteTimeout:   60 * time.Second,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	go func() {
		if err := authProxy.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			authProxy.logger.Printf("Server error: %v", err)
		}
	}()

	return authProxy, nil
}

func (p *AuthProxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Host == "" && r.URL.Scheme == "" {
		r.URL.Scheme = "http"

		if r.TLS != nil {
			r.URL.Scheme = "https"
		}

		r.URL.Host = r.Host
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, r.Method, r.URL.String(), r.Body) //nolint:gosec // G704: this proxy intentionally forwards the caller's URL.
	if err != nil {
		http.Error(w, "Failed to create request", http.StatusBadRequest)

		return
	}

	req.Header = r.Header.Clone()

	for _, h := range []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailers", "Transfer-Encoding", "Upgrade"} {
		req.Header.Del(h)
	}

	if p.proxyType == ProxyTypeHTTP && p.auth != "" {
		req.Header.Set("Proxy-Authorization", "Basic "+p.auth)
	}

	resp, err := p.client.Do(req) //nolint:gosec // G704: the caller's URL is the intended proxy destination.
	if err != nil {
		p.logger.Printf("HTTP request failed: %v", err)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)

		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	w.WriteHeader(resp.StatusCode)

	buf := p.bufferPool.Get().([]byte) //nolint:errcheck // false positive
	defer p.bufferPool.Put(buf)        //nolint:staticcheck // false positive

	if _, err := io.CopyBuffer(w, resp.Body, buf); err != nil {
		p.logger.Printf("Failed to copy response body: %v", err)
	}
}

func (p *AuthProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	switch p.proxyType {
	case ProxyTypeHTTP:
		p.handleHTTPConnect(w, r)
	case ProxyTypeSOCKS5:
		p.handleSOCKS5Connect(w, r)
	}
}

// maxFailoverCandidates is how many other proxies already in the pool get
// tried, through this same local listener, when the pinned upstream rejects
// CONNECT for a reason specific to that exit (refused, timed out, 5xx, auth).
// The browser never notices: it only ever talks to this one local address.
//
// A 429 is not one of those reasons. On a shared datacenter plan the gateway
// applies it to the account, so the next exit IP answers 429 as well, and
// walking the list multiplies the requests that tripped the limit. The
// candidate loop stops on 429, the process waits out Retry-After, and one
// exit is tried again.
const maxFailoverCandidates = 25

// connectUserAgent is sent on the upstream CONNECT. Go's net/http Transport
// (the preflight in web/proxies.go) and curl both send a User-Agent; a bare
// CONNECT with only Host and Proxy-Authorization is the one byte-level
// difference those clients do not share.
const connectUserAgent = "Go-http-client/1.1"

// A 429 is account-wide, so every in-flight CONNECT in the process has to
// pause together. Otherwise each browser socket would keep dialing through
// the cooldown and the window would never recover.
const (
	defaultRateLimitBackoff = 15 * time.Second
	maxRateLimitBackoff     = 20 * time.Second
	rateLimitLogInterval    = time.Minute
	upstreamReadTimeout     = 15 * time.Second
)

// errUpstreamRateLimited is wrapped around the upstream status when CONNECT
// is answered with 429. Callers stop trying other exits and wait.
var errUpstreamRateLimited = errors.New("upstream proxy rate limited")

var (
	rateLimitMu    sync.Mutex
	rateLimitUntil time.Time

	rateLimitLogMu sync.Mutex
	rateLimitLogAt time.Time
)

// noteUpstreamRateLimit pushes the process-wide pause out to now+d. A later
// 429 can only extend the pause, never shorten one already in effect.
func noteUpstreamRateLimit(d time.Duration) {
	if d <= 0 {
		d = defaultRateLimitBackoff
	}

	d = capRateLimitBackoff(d)
	until := time.Now().Add(d)

	rateLimitMu.Lock()
	defer rateLimitMu.Unlock()

	if until.After(rateLimitUntil) {
		rateLimitUntil = until
	}
}

// waitUpstreamRateLimit blocks until the process-wide 429 pause has elapsed.
// The deadline is re-read after every sleep because another CONNECT may have
// extended it while this one was waiting.
func waitUpstreamRateLimit(ctx context.Context) error {
	for {
		rateLimitMu.Lock()
		wait := time.Until(rateLimitUntil)
		rateLimitMu.Unlock()

		if wait <= 0 {
			return nil
		}

		timer := time.NewTimer(wait)

		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()

			return ctx.Err()
		}
	}
}

func capRateLimitBackoff(d time.Duration) time.Duration {
	if d > maxRateLimitBackoff {
		return maxRateLimitBackoff
	}

	return d
}

// retryAfterDuration reads Retry-After (delta-seconds or HTTP-date). A
// missing or unusable value falls back to defaultRateLimitBackoff. The result
// is capped so one header cannot hold dials past the local server's write
// timeout, which is also about when the browser gives up on the socket.
func retryAfterDuration(resp *http.Response) time.Duration {
	if resp == nil {
		return defaultRateLimitBackoff
	}

	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return defaultRateLimitBackoff
	}

	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return capRateLimitBackoff(time.Duration(secs) * time.Second)
	}

	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return capRateLimitBackoff(d)
		}
	}

	return defaultRateLimitBackoff
}

func upstreamBodySnippet(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}

	defer resp.Body.Close()

	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	s := strings.Join(strings.Fields(string(b)), " ")

	if len(s) > 200 {
		s = s[:200]
	}

	return s
}

func rateLimitHeaderSummary(h http.Header) string {
	if len(h) == 0 {
		return ""
	}

	parts := make([]string, 0, 2)

	for k, vs := range h {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "ratelimit") || strings.Contains(lk, "rate-limit") {
			parts = append(parts, k+"="+strings.Join(vs, ","))
		}
	}

	sort.Strings(parts)

	return strings.Join(parts, "; ")
}

// logUpstreamRateLimit records the gateway's 429 once a minute. Every browser
// socket hits the same response; the body and Retry-After only need to be
// visible the first time the window trips.
func logUpstreamRateLimit(logger *log.Logger, upstream string, resp *http.Response, body string) {
	if logger == nil || resp == nil {
		return
	}

	rateLimitLogMu.Lock()
	defer rateLimitLogMu.Unlock()

	if !rateLimitLogAt.IsZero() && time.Since(rateLimitLogAt) < rateLimitLogInterval {
		return
	}

	rateLimitLogAt = time.Now()

	logger.Printf("upstream %s CONNECT rate limit: %s retry-after=%q server=%q rate-limit=%q body=%q",
		upstream, resp.Status, resp.Header.Get("Retry-After"), resp.Header.Get("Server"), rateLimitHeaderSummary(resp.Header), body)
}

// prefixConn replays bytes http.ReadResponse pulled into its buffer ahead
// of the tunnel. After that buffer is empty, reads go straight to the conn.
type prefixConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if c.r != nil && c.r.Buffered() > 0 {
		return c.r.Read(p)
	}

	return c.Conn.Read(p)
}

func writeConnectRequest(conn net.Conn, host, auth string) error {
	var b strings.Builder

	fmt.Fprintf(&b, "CONNECT %s HTTP/1.1\r\n", host)
	fmt.Fprintf(&b, "Host: %s\r\n", host)
	fmt.Fprintf(&b, "User-Agent: %s\r\n", connectUserAgent)
	b.WriteString("Proxy-Connection: Keep-Alive\r\n")

	if auth != "" {
		fmt.Fprintf(&b, "Proxy-Authorization: Basic %s\r\n", auth)
	}

	b.WriteString("\r\n")

	_, err := io.WriteString(conn, b.String())

	return err
}

// connectCandidate dials one candidate upstream and attempts the CONNECT.
// On success the returned connection's concurrency slot is already held for
// cand.upstream.Host; the caller must release it once the tunnel is done
// with releaseConnectSlotFor. A 429 does not try another exit: the error
// wraps errUpstreamRateLimited and the process pause is already armed.
func (p *AuthProxy) connectCandidate(r *http.Request, cand *AuthProxy) (net.Conn, error) {
	// The pause uses the request context, not the dial timeout below. A 15s
	// cooldown must not expire the 30s dial budget before the TCP handshake.
	if err := waitUpstreamRateLimit(r.Context()); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := acquireConnectSlotFor(ctx, cand.upstream.Host); err != nil {
		return nil, err
	}

	if err := throttleConnect(ctx); err != nil {
		releaseConnectSlotFor(cand.upstream.Host)

		return nil, err
	}

	var d net.Dialer

	conn, err := d.DialContext(ctx, "tcp", cand.upstream.Host)
	if err != nil {
		releaseConnectSlotFor(cand.upstream.Host)

		return nil, fmt.Errorf("dial %s: %w", cand.upstream.Host, err)
	}

	if err = writeConnectRequest(conn, r.Host, cand.auth); err != nil {
		releaseConnectSlotFor(cand.upstream.Host)
		conn.Close()

		return nil, fmt.Errorf("write CONNECT to %s: %w", cand.upstream.Host, err)
	}

	// Bound the header read. The dial context covers the handshake only, and
	// a 429 body with no length would otherwise sit in Body.Close forever.
	_ = conn.SetReadDeadline(time.Now().Add(upstreamReadTimeout))

	br := bufio.NewReader(conn)

	resp, err := http.ReadResponse(br, r)
	if err != nil {
		releaseConnectSlotFor(cand.upstream.Host)
		conn.Close()

		return nil, fmt.Errorf("read CONNECT response from %s: %w", cand.upstream.Host, err)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		body := upstreamBodySnippet(resp)

		noteUpstreamRateLimit(retryAfterDuration(resp))
		logUpstreamRateLimit(p.logger, cand.upstream.Host, resp, body)

		releaseConnectSlotFor(cand.upstream.Host)
		conn.Close()

		return nil, fmt.Errorf("%w: upstream %s: %s", errUpstreamRateLimited, cand.upstream.Host, resp.Status)
	}

	if resp.StatusCode == http.StatusOK {
		// RFC 9110: a 2xx CONNECT response has no body. ReadResponse still
		// treats a length-less 200 as "read until the peer closes", and
		// Body.Close would discard the tunnel. Keep the buffered reader so
		// bytes already pulled off the wire are delivered to the browser.
		_ = conn.SetDeadline(time.Time{})

		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetNoDelay(true)
			_ = tcp.SetKeepAlive(true)
			_ = tcp.SetKeepAlivePeriod(30 * time.Second)
		}

		return &prefixConn{Conn: conn, r: br}, nil
	}

	resp.Body.Close()

	releaseConnectSlotFor(cand.upstream.Host)
	conn.Close()

	return nil, fmt.Errorf("upstream %s: %s", cand.upstream.Host, resp.Status)
}

// dialCandidates tries exits until one accepts the CONNECT. It returns the
// candidate that answered 429 so the caller can retry that one exit after
// the pause, without walking the rest of the list.
func (p *AuthProxy) dialCandidates(r *http.Request, candidates []*AuthProxy) (net.Conn, string, *AuthProxy, error) {
	var lastErr error

	for _, cand := range candidates {
		conn, err := p.connectCandidate(r, cand)
		if err == nil {
			return conn, cand.upstream.Host, nil, nil
		}

		lastErr = err

		if errors.Is(err, errUpstreamRateLimited) {
			return nil, "", cand, err
		}
	}

	return nil, "", nil, lastErr
}

func (p *AuthProxy) handleHTTPConnect(w http.ResponseWriter, r *http.Request) {
	if r.Host == "" {
		http.Error(w, "Missing host in CONNECT request", http.StatusBadRequest)
		return
	}

	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}

	if err = p.checkPrivateNetwork(host); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)

		return
	}

	candidates := []*AuthProxy{p}
	if p.pool != nil {
		candidates = append(candidates, p.pool.Alternates(p, maxFailoverCandidates)...)
	}

	upstreamConn, connectedHost, limited, lastErr := p.dialCandidates(r, candidates)
	if upstreamConn == nil && limited != nil {
		// One retry of the exit that answered, after the account pause.
		// connectCandidate waits again, so a 429 that landed while we were
		// already asleep cannot be dialed early.
		if err = waitUpstreamRateLimit(r.Context()); err != nil {
			lastErr = err
		} else if conn, connErr := p.connectCandidate(r, limited); connErr != nil {
			lastErr = connErr
		} else {
			upstreamConn = conn
			connectedHost = limited.upstream.Host
		}
	}

	if upstreamConn == nil {
		if !errors.Is(lastErr, errUpstreamRateLimited) {
			p.logger.Printf("All %d proxy candidate(s) rejected CONNECT for %s: %v", len(candidates), r.Host, lastErr)
		}

		http.Error(w, "Bad Gateway", http.StatusBadGateway)

		return
	}

	defer releaseConnectSlotFor(connectedHost)

	p.tunnelConnection(w, r, upstreamConn)
}

func (p *AuthProxy) handleSOCKS5Connect(w http.ResponseWriter, r *http.Request) {
	if r.Host == "" {
		http.Error(w, "Missing host in CONNECT request", http.StatusBadRequest)
		return
	}

	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}

	if p.upstream.Scheme != "socks5h" {
		if err = p.checkPrivateNetwork(host); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)

			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	dialer, err := proxy.SOCKS5("tcp", p.upstream.Host, p.socks5Auth, proxy.Direct)
	if err != nil {
		p.logger.Printf("Failed to create SOCKS5 dialer: %v", err)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)

		return
	}

	var upstreamConn net.Conn
	if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
		upstreamConn, err = contextDialer.DialContext(ctx, "tcp", r.Host)
	} else {
		upstreamConn, err = dialer.Dial("tcp", r.Host)
	}

	if err != nil {
		p.logger.Printf("Failed to connect through SOCKS5 proxy: %v", err)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)

		return
	}

	p.tunnelConnection(w, r, upstreamConn)
}

func (p *AuthProxy) checkPrivateNetwork(host string) error {
	ips, err := net.LookupIP(host)
	if err == nil {
		for _, ip := range ips {
			if ip.IsLoopback() || ip.IsPrivate() {
				return fmt.Errorf("connection to private networks not allowed")
			}
		}
	}

	return nil
}

func (p *AuthProxy) tunnelConnection(w http.ResponseWriter, _ *http.Request, upstreamConn net.Conn) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Connection hijacking not supported", http.StatusInternalServerError)
		upstreamConn.Close()

		return
	}

	clientConn, _, err := hj.Hijack()
	if err != nil {
		p.logger.Printf("Failed to hijack client connection: %v", err)
		upstreamConn.Close()

		return
	}

	_, _ = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	if tcpConn, ok := clientConn.(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}

	if tcpConn, ok := upstreamConn.(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}

	var wg sync.WaitGroup

	wg.Add(2)

	// Client -> Upstream
	go func() {
		defer wg.Done()

		buf := p.bufferPool.Get().([]byte) //nolint:errcheck // false positive
		defer p.bufferPool.Put(buf)        //nolint:staticcheck // false positive

		_, err := io.CopyBuffer(upstreamConn, clientConn, buf)
		if err != nil && !isExpectedNetworkError(err) {
			p.logger.Printf("client->upstream error: %v", err)
		}

		upstreamConn.Close()
	}()

	// Upstream -> Client
	go func() {
		defer wg.Done()

		buf := p.bufferPool.Get().([]byte) //nolint:errcheck // false positive
		defer p.bufferPool.Put(buf)        //nolint:staticcheck // false positive

		_, err := io.CopyBuffer(clientConn, upstreamConn, buf)
		if err != nil && !isExpectedNetworkError(err) {
			p.logger.Printf("client->upstream error: %v", err)
		}

		clientConn.Close()
	}()

	wg.Wait()
}

func (p *AuthProxy) SetLogger(logger *log.Logger) {
	if logger != nil {
		p.logger = logger
	} else {
		p.logger = log.New(io.Discard, "[AuthProxy] ", log.LstdFlags)
	}
}

func (p *AuthProxy) Close() error {
	var err error

	p.once.Do(func() {
		close(p.shutdownCh)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		err = p.server.Shutdown(ctx)
		if err == context.DeadlineExceeded {
			err = p.server.Close()
		}
	})

	return err
}

// ProxyType returns the type of upstream proxy being used
func (p *AuthProxy) ProxyType() ProxyType {
	return p.proxyType
}

// ProxyTypeString returns a string representation of the proxy type
func (p *AuthProxy) ProxyTypeString() string {
	switch p.proxyType {
	case ProxyTypeHTTP:
		return "HTTP"
	case ProxyTypeSOCKS5:
		return "SOCKS5"
	default:
		return "Unknown"
	}
}

func isExpectedNetworkError(err error) bool {
	if err == nil || err == io.EOF {
		return true
	}

	errStr := err.Error()

	return strings.Contains(errStr, "use of closed network connection") ||
		strings.Contains(errStr, "connection reset by peer") ||
		strings.Contains(errStr, "broken pipe")
}
