package jshttp

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
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
// one page (every subresource: scripts, images, XHRs, ...), and since one
// browser is pinned to one exit proxy for its whole session (ProxyPool.Next
// is called once per browser, not per request), a single real Maps page load
// alone easily exceeds a shared datacenter proxy's per-IP connection limit -
// this is the 429 seen even with only one job running at a time. Queuing the
// extra connections here instead of firing them all at once makes the page
// load a bit slower but keeps it under the exit IP's own limit.
//
// This has to be keyed by the upstream address and shared process-wide, not
// kept on the AuthProxy struct: every job builds its own ProxyPool with its
// own fresh AuthProxy instances, so two different jobs that happen to pick
// the same exit IP would otherwise each get their own independent budget for
// the same real upstream connection limit.
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
// tried, through this same local listener, when the one this AuthProxy is
// pinned to keeps rejecting CONNECT. The browser never notices: it only ever
// talks to this one local address, whichever real exit IP ends up serving it.
// Set fairly high because a shared datacenter plan's 429 is unpredictable per
// IP - keep trying different exit IPs until one works. Bounded well below
// the full pool size so one subresource request can't, in the worst case
// (every candidate rejected), eat the whole job's time budget at
// connectMinInterval pacing (25 * connectMinInterval here).
const maxFailoverCandidates = 25

// connectCandidate dials one candidate upstream and attempts the CONNECT.
// With a shared datacenter plan, a 429 on one exit IP says little about the
// next one, so this does not retry the same IP - handleHTTPConnect's caller
// moves on to the next candidate instead, which cycles through many more
// distinct exit IPs in the same amount of time than retrying one repeatedly
// would. On success the returned connection's concurrency slot is already
// held for cand.upstream.Host; the caller must release it once the tunnel is
// done with releaseConnectSlotFor.
func (p *AuthProxy) connectCandidate(r *http.Request, cand *AuthProxy) (net.Conn, error) {
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

	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", r.Host, r.Host)
	if cand.auth != "" {
		connectReq += "Proxy-Authorization: Basic " + cand.auth + "\r\n"
	}

	connectReq += "\r\n"
	if _, err = conn.Write([]byte(connectReq)); err != nil {
		releaseConnectSlotFor(cand.upstream.Host)
		conn.Close()

		return nil, fmt.Errorf("write CONNECT to %s: %w", cand.upstream.Host, err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), r)
	if err != nil {
		releaseConnectSlotFor(cand.upstream.Host)
		conn.Close()

		return nil, fmt.Errorf("read CONNECT response from %s: %w", cand.upstream.Host, err)
	}

	resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return conn, nil
	}

	releaseConnectSlotFor(cand.upstream.Host)
	conn.Close()

	return nil, fmt.Errorf("upstream %s: %s", cand.upstream.Host, resp.Status)
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

	var (
		upstreamConn  net.Conn
		connectedHost string
		lastErr       error
	)

	for _, cand := range candidates {
		conn, connErr := p.connectCandidate(r, cand)
		if connErr == nil {
			upstreamConn = conn
			connectedHost = cand.upstream.Host

			break
		}

		lastErr = connErr
	}

	if upstreamConn == nil {
		p.logger.Printf("All %d proxy candidate(s) rejected CONNECT for %s: %v", len(candidates), r.Host, lastErr)
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
