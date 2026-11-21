package jshttp

import (
	"fmt"
	"math/rand/v2"
	"net/url"
	"sync"
)

// maxListeningProxies is how many local ports one job may bind.
//
// A browser is pinned to one local listener. When that exit rejects a
// CONNECT, the same listener dials other upstreams directly, so the rest of
// the list does not need a socket. Binding every ProxyScrape exit (up to
// 1000) per job fills the ephemeral range, about 28k ports. After that,
// listen on 127.0.0.1:0 fails with "address already in use" and every
// outbound dial, including SMTP, fails with "cannot assign requested address".
// A second ceiling, maxProcessListeners, stops a leaked pool from growing
// without bound across jobs.
const maxListeningProxies = 32

type ProxyPool struct {
	current   int
	mu        *sync.Mutex
	proxies   []*AuthProxy // every upstream, including dial-only failover targets
	listening []*AuthProxy // the ones with a local port, handed to browsers
}

func NewProxyPool(proxies []string) (*ProxyPool, error) {
	if len(proxies) == 0 {
		return nil, fmt.Errorf("no proxies provided")
	}

	pool := &ProxyPool{
		mu:      &sync.Mutex{},
		proxies: make([]*AuthProxy, 0, len(proxies)),
	}

	// Any failure, including a panic while binding, has to drop the ports
	// already taken. Otherwise they stay in LISTEN for the life of the process.
	started := false
	defer func() {
		if !started {
			_ = pool.Close()
		}
	}()

	for _, p := range proxies {
		cfg, err := parseProxy(p)
		if err != nil {
			return nil, fmt.Errorf("failed to parse proxy %s: %w", redactProxyURL(p), err)
		}

		authProxy, err := prepareAuthProxy(cfg.u, cfg.username, cfg.password)
		if err != nil {
			return nil, fmt.Errorf("failed to prepare auth proxy for %s: %w", redactProxyURL(p), err)
		}

		authProxy.pool = pool

		pool.proxies = append(pool.proxies, authProxy)
	}

	// Spread the listeners through the list so parallel jobs are not all
	// pinned to the same first exits.
	start := 0
	if len(pool.proxies) > 1 {
		start = rand.IntN(len(pool.proxies))
	}

	n := min(len(pool.proxies), maxListeningProxies)
	for i := range n {
		authProxy := pool.proxies[(start+i)%len(pool.proxies)]
		if err := authProxy.listen(); err != nil {
			return nil, fmt.Errorf("failed to start auth proxy for %s: %w", redactProxyURL(proxies[(start+i)%len(proxies)]), err)
		}

		pool.listening = append(pool.listening, authProxy)
	}

	started = true

	return pool, nil
}

// redactProxyURL drops userinfo so a bind failure does not land credentials in the job error.
func redactProxyURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "invalid-proxy-url"
	}

	u.User = nil

	return u.String()
}

func (pp *ProxyPool) Next() *AuthProxy {
	pp.mu.Lock()
	defer pp.mu.Unlock()

	list := pp.listening
	if len(list) == 0 {
		// Pools built in tests pin a listener on every entry and leave
		// listening empty. A browser must never receive a dial-only proxy.
		list = pp.proxies
	}

	p := list[pp.current%len(list)]
	pp.current++

	return p
}

// Alternates returns up to n other proxies already in the pool (skipping
// exclude), for a single CONNECT to fail over to when exclude's own upstream
// is persistently rejecting it (e.g. a sustained 429). This does not affect
// Next()'s own rotation, which still decides which proxy each new browser
// gets assigned to for its whole session.
func (pp *ProxyPool) Alternates(exclude *AuthProxy, n int) []*AuthProxy {
	pp.mu.Lock()
	defer pp.mu.Unlock()

	if len(pp.proxies) <= 1 || n <= 0 {
		return nil
	}

	out := make([]*AuthProxy, 0, n)

	for i := 0; i < len(pp.proxies) && len(out) < n; i++ {
		p := pp.proxies[(pp.current+i)%len(pp.proxies)]
		if p == exclude {
			continue
		}

		out = append(out, p)
	}

	return out
}

// Close shuts down every local listener in the pool. Dial-only entries have
// nothing bound. Without this, each job leaks its listeners for the life of
// the process and outbound dials eventually fail with
// "cannot assign requested address".
func (pp *ProxyPool) Close() error {
	pp.mu.Lock()
	proxies := pp.proxies
	pp.mu.Unlock()

	var firstErr error

	for _, p := range proxies {
		if err := p.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

type proxyConfig struct {
	u        string
	username string
	password string
}

func parseProxy(proxy string) (*proxyConfig, error) {
	if proxy == "" {
		return nil, fmt.Errorf("proxy URL cannot be empty")
	}

	u, err := url.Parse(proxy)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL: %w", err)
	}

	var username, password string
	if u.User != nil {
		username = u.User.Username()
		password, _ = u.User.Password()
	}

	u.User = nil // Clear user info from URL

	return &proxyConfig{
		u:        u.String(),
		username: username,
		password: password,
	}, nil
}
