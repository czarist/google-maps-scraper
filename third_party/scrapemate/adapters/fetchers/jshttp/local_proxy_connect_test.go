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

	if hitsA.Load() != 2 {
		t.Fatalf("rate-limited exit was dialed %d times, want 2 (one retry after Retry-After)", hitsA.Load())
	}

	if hitsB.Load() != 0 {
		t.Fatalf("failover exit was dialed %d times; a 429 must not walk the pool", hitsB.Load())
	}

	if elapsed < 900*time.Millisecond {
		t.Fatalf("retry returned in %s, want at least the Retry-After pause", elapsed)
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
