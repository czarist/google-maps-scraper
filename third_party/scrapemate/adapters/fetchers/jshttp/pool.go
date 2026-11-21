package jshttp

import (
	"fmt"
	"net/url"
	"sync"
)

type ProxyPool struct {
	current int
	mu      *sync.Mutex
	proxies []*AuthProxy
}

func NewProxyPool(proxies []string) (*ProxyPool, error) {
	if len(proxies) == 0 {
		return nil, fmt.Errorf("no proxies provided")
	}

	pool := &ProxyPool{
		mu:      &sync.Mutex{},
		proxies: make([]*AuthProxy, 0, len(proxies)),
	}

	for _, p := range proxies {
		cfg, err := parseProxy(p)
		if err != nil {
			return nil, fmt.Errorf("failed to parse proxy %s: %w", p, err)
		}

		authProxy, err := StartAuthProxy(cfg.u, cfg.username, cfg.password)
		if err != nil {
			return nil, fmt.Errorf("failed to start auth proxy for %s: %w", p, err)
		}

		authProxy.pool = pool

		pool.proxies = append(pool.proxies, authProxy)
	}

	return pool, nil
}

func (pp *ProxyPool) Next() *AuthProxy {
	pp.mu.Lock()
	defer pp.mu.Unlock()

	p := pp.proxies[pp.current%len(pp.proxies)]
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
