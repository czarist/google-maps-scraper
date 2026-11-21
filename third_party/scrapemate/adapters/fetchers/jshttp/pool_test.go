package jshttp

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestNewProxyPoolBindsOnlyAFewListeners(t *testing.T) {
	const extra = 10

	raw := make([]string, maxListeningProxies+extra)
	for i := range raw {
		raw[i] = fmt.Sprintf("http://user:secret@192.0.2.1:%d", 10000+i)
	}

	pool, err := NewProxyPool(raw)
	if err != nil {
		t.Fatalf("NewProxyPool: %v", err)
	}

	t.Cleanup(func() { _ = pool.Close() })

	if got, want := len(pool.proxies), len(raw); got != want {
		t.Fatalf("proxies = %d, want %d", got, want)
	}

	if got, want := len(pool.listening), maxListeningProxies; got != want {
		t.Fatalf("listening = %d, want %d", got, want)
	}

	bound := 0

	for _, p := range pool.proxies {
		if p.Port() != 0 {
			bound++
		}
	}

	if bound != maxListeningProxies {
		t.Fatalf("bound ports = %d, want %d", bound, maxListeningProxies)
	}

	seen := map[int]struct{}{}

	for range maxListeningProxies * 2 {
		p := pool.Next()
		if p.Port() == 0 {
			t.Fatal("Next returned a dial-only proxy")
		}

		seen[p.Port()] = struct{}{}
	}

	if len(seen) != maxListeningProxies {
		t.Fatalf("Next rotated through %d ports, want %d", len(seen), maxListeningProxies)
	}
}

func TestProxyPoolCloseReleasesPorts(t *testing.T) {
	pool, err := NewProxyPool([]string{"http://user:secret@192.0.2.1:3129"})
	if err != nil {
		t.Fatalf("NewProxyPool: %v", err)
	}

	port := pool.Next().Port()
	if port == 0 {
		t.Fatal("listener has no port")
	}

	if err := pool.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := waitPortFree(port); err != nil {
		t.Fatal(err)
	}
}

func TestAuthProxyCloseBeforeServeReleasesPort(t *testing.T) {
	p, err := StartAuthProxy("http://user:secret@192.0.2.1:3129", "user", "secret")
	if err != nil {
		t.Fatalf("StartAuthProxy: %v", err)
	}

	port := p.Port()

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := waitPortFree(port); err != nil {
		t.Fatal(err)
	}
}

func TestProcessListenerCapStaysFarBelowTheEphemeralRange(t *testing.T) {
	got := maxProcessListeners.Load()
	if got < 128 || got > 2048 {
		t.Fatalf("process listener cap = %d, want 128..2048 so outbound dials keep a port", got)
	}
}

func TestListenerCapReleasesTheSlotOnClose(t *testing.T) {
	before := liveListeners.Load()
	prev := maxProcessListeners.Load()

	maxProcessListeners.Store(before + 1)
	t.Cleanup(func() { maxProcessListeners.Store(prev) })

	first, err := StartAuthProxy("http://user:secret@192.0.2.1:3129", "user", "secret")
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}

	second, err := StartAuthProxy("http://user:secret@192.0.2.2:3129", "user", "secret")
	if err == nil {
		_ = second.Close()
		_ = first.Close()

		t.Fatal("listen succeeded after the process cap was reached")
	}

	if !strings.Contains(err.Error(), "cap") {
		t.Fatalf("cap error = %v", err)
	}

	if got := liveListeners.Load(); got != before+1 {
		t.Fatalf("live listeners = %d, want %d", got, before+1)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if got := liveListeners.Load(); got != before {
		t.Fatalf("after close live listeners = %d, want %d", got, before)
	}

	third, err := StartAuthProxy("http://user:secret@192.0.2.3:3129", "user", "secret")
	if err != nil {
		t.Fatalf("listen after a slot was released: %v", err)
	}

	if err := third.Close(); err != nil {
		t.Fatalf("close third: %v", err)
	}

	if got := liveListeners.Load(); got != before {
		t.Fatalf("final live listeners = %d, want %d", got, before)
	}
}

func TestNewProxyPoolReleasesSlotsWhenTheCapStopsIt(t *testing.T) {
	before := liveListeners.Load()
	prev := maxProcessListeners.Load()

	maxProcessListeners.Store(before + 2)
	t.Cleanup(func() { maxProcessListeners.Store(prev) })

	_, err := NewProxyPool([]string{
		"http://user:secret@192.0.2.1:3129",
		"http://user:secret@192.0.2.2:3129",
		"http://user:secret@192.0.2.3:3129",
	})
	if err == nil {
		t.Fatal("expected the process cap to reject the pool")
	}

	if strings.Contains(err.Error(), "user") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("proxy credentials in error: %v", err)
	}

	if got := liveListeners.Load(); got != before {
		t.Fatalf("failed pool leaked listeners: live=%d before=%d", got, before)
	}
}

func TestRedactProxyURLDropsUserinfo(t *testing.T) {
	got := redactProxyURL("http://user:secret@192.0.2.1:3129")
	if strings.Contains(got, "user") || strings.Contains(got, "secret") {
		t.Fatalf("redacted URL = %q", got)
	}

	if got != "http://192.0.2.1:3129" {
		t.Fatalf("redacted URL = %q", got)
	}

	if redactProxyURL("not a url") != "invalid-proxy-url" {
		t.Fatalf("invalid URL was not redacted: %q", redactProxyURL("not a url"))
	}
}

func waitPortFree(port int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(2 * time.Second)

	var last error

	for time.Now().Before(deadline) {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			_ = ln.Close()

			return nil
		}

		last = err

		time.Sleep(10 * time.Millisecond)
	}

	return fmt.Errorf("port %d still in use: %w", port, last)
}
