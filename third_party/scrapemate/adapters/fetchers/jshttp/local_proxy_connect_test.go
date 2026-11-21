package jshttp

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func resetUpstreamRateLimit() {
	rateLimitMu.Lock()
	rateLimitUntil = time.Time{}
	singleLaneUntil = time.Time{}
	upstreamRecoveries = 0
	bandwidthLimited = false
	rateLimitMu.Unlock()

	rateLimitLogMu.Lock()
	rateLimitLogAt = time.Time{}
	rateLimitLogMu.Unlock()

	connectMu.Lock()
	connectLastCall = time.Time{}
	connectMu.Unlock()
}

// serveConnectUpstream speaks just enough HTTP to answer a CONNECT.
// respond owns the connection and closes it.
func serveConnectUpstream(t *testing.T, respond func(net.Conn, *http.Request)) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			go func(c net.Conn) {
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))

				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil {
					c.Close()

					return
				}

				respond(c, req)
			}(c)
		}
	}()

	return ln.Addr().String()
}

func testPinnedPool(t *testing.T, rawURLs ...string) *AuthProxy {
	t.Helper()

	proxies := make([]*AuthProxy, 0, len(rawURLs))

	for _, raw := range rawURLs {
		p, err := StartAuthProxy(raw, "user", "secret")
		if err != nil {
			t.Fatalf("start auth proxy: %v", err)
		}

		t.Cleanup(func() { _ = p.Close() })

		proxies = append(proxies, p)
	}

	pool := &ProxyPool{mu: &sync.Mutex{}, proxies: proxies}
	for _, p := range proxies {
		p.pool = pool
	}

	return proxies[0]
}

func connectThrough(t *testing.T, proxyAddr string) *http.Response {
	t.Helper()

	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial local proxy: %v", err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	if _, err = fmt.Fprintf(conn, "CONNECT 1.2.3.4:443 HTTP/1.1\r\nHost: 1.2.3.4:443\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read local proxy response: %v", err)
	}

	t.Cleanup(func() { _ = resp.Body.Close() })

	return resp
}

func TestConnectRateLimitDoesNotFailover(t *testing.T) {
	resetUpstreamRateLimit()
	t.Cleanup(resetUpstreamRateLimit)

	var (
		hitsA   atomic.Int32
		hitsB   atomic.Int32
		uaMu    sync.Mutex
		gotUA   string
		gotAuth string
	)

	body := "account concurrency exceeded"

	addrA := serveConnectUpstream(t, func(c net.Conn, r *http.Request) {
		hitsA.Add(1)

		uaMu.Lock()
		gotUA = r.Header.Get("User-Agent")
		gotAuth = r.Header.Get("Proxy-Authorization")
		uaMu.Unlock()

		fmt.Fprintf(c, "HTTP/1.1 429 Too Many Requests\r\nRetry-After: 1\r\nServer: proxyscrape-test\r\nX-RateLimit-Limit: 10\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
		c.Close()
	})

	addrB := serveConnectUpstream(t, func(c net.Conn, _ *http.Request) {
		hitsB.Add(1)
		c.Close()
	})

	pinned := testPinnedPool(t, "http://"+addrA, "http://"+addrB)

	var logs bytes.Buffer
	pinned.SetLogger(log.New(&logs, "", 0))

	start := time.Now()
	resp := connectThrough(t, pinned.Address())
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("local proxy status = %d, want 502", resp.StatusCode)
	}

	if hitsA.Load() != 1 {
		t.Fatalf("rate-limited exit was dialed %d times, want 1 (the browser must not be held for Retry-After)", hitsA.Load())
	}

	if hitsB.Load() != 0 {
		t.Fatalf("failover exit was dialed %d times; a 429 must not walk the pool", hitsB.Load())
	}

	if elapsed >= 800*time.Millisecond {
		t.Fatalf("CONNECT returned in %s; a 429 must fail the socket instead of waiting out Retry-After", elapsed)
	}

	if wait := UpstreamRateLimitWait(); wait < 200*time.Millisecond || wait > 2*time.Second {
		t.Fatalf("pause = %s, want about 1s from Retry-After", wait)
	}

	if UpstreamLaneCap(4) != 1 {
		t.Fatalf("lane cap = %d, want 1 right after a 429", UpstreamLaneCap(4))
	}

	uaMu.Lock()
	defer uaMu.Unlock()

	if gotUA != connectUserAgent {
		t.Fatalf("CONNECT User-Agent = %q, want %q", gotUA, connectUserAgent)
	}

	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:secret"))
	if gotAuth != wantAuth {
		t.Fatalf("Proxy-Authorization = %q, want %q", gotAuth, wantAuth)
	}

	logged := logs.String()
	for _, part := range []string{body, "429", "retry-after=\"1\"", "proxyscrape-test", "X-Ratelimit-Limit=10"} {
		if !bytes.Contains(logs.Bytes(), []byte(part)) {
			t.Fatalf("rate-limit log %q missing %q", logged, part)
		}
	}
}

func TestRetryAfter120IsNotCutShort(t *testing.T) {
	resetUpstreamRateLimit()
	t.Cleanup(resetUpstreamRateLimit)

	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("Retry-After", "120")

	d := retryAfterDuration(resp)
	if d != 120*time.Second {
		t.Fatalf("backoff = %s, want 120s", d)
	}

	noteUpstreamRateLimit(d)

	wait := UpstreamRateLimitWait()
	if wait < 110*time.Second || wait > 120*time.Second {
		t.Fatalf("pause = %s, want about 120s", wait)
	}
}

func TestConnectFailoverOnNonRateLimit(t *testing.T) {
	resetUpstreamRateLimit()
	t.Cleanup(resetUpstreamRateLimit)

	var hitsB atomic.Int32

	addrA := serveConnectUpstream(t, func(c net.Conn, _ *http.Request) {
		fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		c.Close()
	})

	addrB := serveConnectUpstream(t, func(c net.Conn, _ *http.Request) {
		hitsB.Add(1)

		_ = c.SetDeadline(time.Time{})
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")

		buf := make([]byte, 4)
		_, _ = io.ReadFull(c, buf)
		_, _ = c.Write(buf)
		c.Close()
	})

	pinned := testPinnedPool(t, "http://"+addrA, "http://"+addrB)

	conn, err := net.Dial("tcp", pinned.Address())
	if err != nil {
		t.Fatalf("dial local proxy: %v", err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	if _, err = fmt.Fprintf(conn, "CONNECT 1.2.3.4:443 HTTP/1.1\r\nHost: 1.2.3.4:443\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	br := bufio.NewReader(conn)

	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read local proxy response: %v", err)
	}

	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("local proxy status = %d, want 200 from the second exit", resp.StatusCode)
	}

	if _, err = conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write tunnel byte: %v", err)
	}

	buf := make([]byte, 4)
	if _, err = io.ReadFull(br, buf); err != nil {
		t.Fatalf("read tunnel byte: %v", err)
	}

	if string(buf) != "ping" {
		t.Fatalf("tunnel payload = %q, want ping", buf)
	}

	if hitsB.Load() != 1 {
		t.Fatalf("second exit dialed %d times, want 1", hitsB.Load())
	}
}

func TestConnectFailoverUsesDialOnlyAlternate(t *testing.T) {
	resetUpstreamRateLimit()
	t.Cleanup(resetUpstreamRateLimit)

	var hitsB atomic.Int32

	addrA := serveConnectUpstream(t, func(c net.Conn, _ *http.Request) {
		fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		c.Close()
	})

	addrB := serveConnectUpstream(t, func(c net.Conn, _ *http.Request) {
		hitsB.Add(1)
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
		c.Close()
	})

	pinned, err := StartAuthProxy("http://"+addrA, "user", "secret")
	if err != nil {
		t.Fatalf("start pinned proxy: %v", err)
	}

	t.Cleanup(func() { _ = pinned.Close() })

	alt, err := prepareAuthProxy("http://"+addrB, "user", "secret")
	if err != nil {
		t.Fatalf("prepare alternate: %v", err)
	}

	if alt.Port() != 0 {
		t.Fatalf("dial-only alternate is listening on %d", alt.Port())
	}

	pool := &ProxyPool{mu: &sync.Mutex{}, proxies: []*AuthProxy{pinned, alt}}
	pinned.pool = pool
	alt.pool = pool

	resp := connectThrough(t, pinned.Address())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("local proxy status = %d, want 200 from the dial-only exit", resp.StatusCode)
	}

	if hitsB.Load() != 1 {
		t.Fatalf("dial-only exit dialed %d times, want 1", hitsB.Load())
	}
}

func TestConnectWhilePausedDoesNotDial(t *testing.T) {
	resetUpstreamRateLimit()
	t.Cleanup(resetUpstreamRateLimit)

	var hits atomic.Int32

	addr := serveConnectUpstream(t, func(c net.Conn, _ *http.Request) {
		hits.Add(1)
		c.Close()
	})

	noteUpstreamRateLimit(2 * time.Second)

	pinned := testPinnedPool(t, "http://"+addr)

	start := time.Now()
	resp := connectThrough(t, pinned.Address())
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("local proxy status = %d, want 502", resp.StatusCode)
	}

	if hits.Load() != 0 {
		t.Fatalf("upstream was dialed %d times during the pause", hits.Load())
	}

	if elapsed >= 500*time.Millisecond {
		t.Fatalf("paused CONNECT took %s; it must fail locally", elapsed)
	}
}

func TestBurst429DoesNotStackThePause(t *testing.T) {
	resetUpstreamRateLimit()
	t.Cleanup(resetUpstreamRateLimit)

	noteUpstreamRateLimit(120 * time.Second)
	first := UpstreamRateLimitWait()

	noteUpstreamRateLimit(120 * time.Second)
	second := UpstreamRateLimitWait()

	if second > first+time.Second {
		t.Fatalf("same burst stretched the pause from %s to %s", first, second)
	}

	if second < 100*time.Second {
		t.Fatalf("pause = %s, want about 120s", second)
	}
}

func TestSecond429WaitsAnother120s(t *testing.T) {
	resetUpstreamRateLimit()
	t.Cleanup(resetUpstreamRateLimit)

	noteUpstreamRateLimit(120 * time.Second)

	rateLimitMu.Lock()
	rateLimitUntil = time.Now().Add(-time.Second)
	rateLimitMu.Unlock()

	noteUpstreamRateLimit(120 * time.Second)

	got := UpstreamRateLimitWait()
	if got < 110*time.Second || got > 120*time.Second {
		t.Fatalf("pause = %s, want another 120s", got)
	}

	if UpstreamLaneCap(4) != 1 {
		t.Fatal("a repeated 429 must keep a single job")
	}
}

func TestLaneStaysNarrowAfterBandwidth429(t *testing.T) {
	resetUpstreamRateLimit()
	t.Cleanup(resetUpstreamRateLimit)

	if got := UpstreamLaneCap(4); got != 4 {
		t.Fatalf("before a 429, cap = %d, want 4", got)
	}

	noteUpstreamRateLimit(time.Second)

	if got := UpstreamLaneCap(4); got != 1 {
		t.Fatalf("after a 429, cap = %d, want 1", got)
	}

	NoteUpstreamRecovered()

	if got := UpstreamLaneCap(4); got != 1 {
		t.Fatalf("after one success, cap = %d, want 1", got)
	}

	NoteUpstreamRecovered()

	if got := UpstreamLaneCap(4); got != 2 {
		t.Fatalf("after two successes, cap = %d, want 2", got)
	}
}

func TestLaneOpensFullyWhenBandwidthWasFine(t *testing.T) {
	resetUpstreamRateLimit()
	t.Cleanup(resetUpstreamRateLimit)

	if got := UpstreamLaneCap(4); got != 4 {
		t.Fatalf("startup cap = %d, want 4", got)
	}

	NoteUpstreamRecovered()

	if got := UpstreamLaneCap(4); got != 4 {
		t.Fatalf("a success with no 429 changed the cap to %d", got)
	}
}
