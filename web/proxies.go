package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gosom/scrapemate"
	"golang.org/x/time/rate"
)

const (
	proxiesFile      = "proxies.json"
	proxyTestURL     = "https://www.google.com/maps?hl=en"
	proxyTestTimeout = 15 * time.Second
	maxSavedProxies  = 1000
)

var proxiesMu sync.Mutex

// ProxyCheck is the result of testing one proxy.
type ProxyCheck struct {
	Proxy     string `json:"proxy"`
	OK        bool   `json:"ok"`
	Status    int    `json:"status,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"`
}

// normalizeProxies trims, drops blanks and duplicates, and validates each proxy.
func normalizeProxies(raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))

	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p == "" || slices.Contains(out, p) {
			continue
		}

		if _, err := scrapemate.NewProxy(p); err != nil {
			return nil, fmt.Errorf("invalid proxy %q: %w (supported: http, https, socks5)", p, err)
		}

		out = append(out, p)
	}

	if len(out) > maxSavedProxies {
		return nil, fmt.Errorf("too many proxies: %d (max %d)", len(out), maxSavedProxies)
	}

	return out, nil
}

func (s *Service) proxiesPath() string {
	return filepath.Join(s.dataFolder, proxiesFile)
}

// SavedProxies returns the saved proxy pool. New jobs can use it by default.
func (s *Service) SavedProxies() ([]string, error) {
	proxiesMu.Lock()
	defer proxiesMu.Unlock()

	return s.readProxies()
}

// ErrNoProxies stops a job that would otherwise reach Google with the server's own IP.
var ErrNoProxies = errors.New("no working proxies: the job was not started so your IP is not exposed to Google. Check the ProxyScrape plan")

// DefaultProxies returns the proxies new jobs use: the ProxyScrape account's
// proxies when PROXYSCRAPE_API_KEY is set, otherwise the saved pool.
// With ProxyScrape there is no fallback: if the API fails or has no online
// proxies, it returns ErrNoProxies instead of letting the job connect directly.
func (s *Service) DefaultProxies(ctx context.Context) ([]string, error) {
	return s.accountProxies(ctx, false)
}

// RefreshProxies re-fetches the ProxyScrape account's proxies instead of
// using the (up to proxyScrapeCacheTTL old) cache. A job keeps its proxy list
// from when it was created, so a job that ran through every proxy it was
// given calls this before giving up, in case the account's pool has changed
// since - rather than being stuck replaying the same list every retry.
func (s *Service) RefreshProxies(ctx context.Context) ([]string, error) {
	return s.accountProxies(ctx, true)
}

func (s *Service) accountProxies(ctx context.Context, refresh bool) ([]string, error) {
	if s.proxyScrape == nil {
		return s.SavedProxies()
	}

	proxies, err := s.proxyScrape.CachedProxies(ctx, refresh)
	if err != nil {
		return nil, fmt.Errorf("%w (ProxyScrape: %s)", ErrNoProxies, err.Error())
	}

	if len(proxies) == 0 {
		return nil, ErrNoProxies
	}

	return proxies, nil
}

// RequiresProxies reports whether jobs must never run without a proxy.
// It is true when ProxyScrape is configured.
func (s *Service) RequiresProxies() bool {
	return s.proxyScrape != nil
}

// proxyPreflightSample is how many of a job's proxies are tested before it starts.
const proxyPreflightSample = 5

// CheckJobProxies runs right before a job starts. When proxies are required it
// refuses a job without proxies, and it refuses any job whose proxies cannot
// reach Google Maps (for example, the plan expired while the job was queued).
func (s *Service) CheckJobProxies(ctx context.Context, proxies []string) error {
	_, err := s.JobProxies(ctx, proxies)

	return err
}

// JobProxies returns the proxies a job should run with: the given ones minus
// those that failed a recent check. It tests a sample of the proxies whose health
// is unknown or old, in parallel, and remembers the results, so a dead proxy
// leaves the rotation for a while instead of failing request after request.
// It returns ErrNoProxies when none works (or none is given and they are required).
func (s *Service) JobProxies(ctx context.Context, proxies []string) ([]string, error) {
	if len(proxies) == 0 {
		if s.RequiresProxies() {
			return nil, ErrNoProxies
		}

		return nil, nil
	}

	now := time.Now()

	var (
		good, unknown []string
		lastErr       string
	)

	for _, p := range proxies {
		switch h, ok := proxyHealthOf(p, now); {
		case !ok:
			unknown = append(unknown, p)
		case h.ok:
			good = append(good, p)
		default:
			lastErr = h.err
		}
	}

	// Test the unknown ones in a random order, a sample at a time.
	rand.Shuffle(len(unknown), func(a, b int) { unknown[a], unknown[b] = unknown[b], unknown[a] })

	sample := unknown[:min(len(unknown), proxyPreflightSample)]
	untested := unknown[len(sample):]

	testedOK := 0

	for _, r := range TestProxies(ctx, sample) {
		proxyHealth.Store(r.Proxy, proxyHealthEntry{ok: r.OK, err: r.Error, at: now})

		if r.OK {
			good = append(good, r.Proxy)
			testedOK++
		} else {
			lastErr = r.Error
		}
	}

	// good keeps the same relative order across calls (the health cache always
	// remembers the same proxies as good, in the order the input list gives
	// them), so every job's own fresh browser.ProxyPool - which always starts
	// picking from index 0 - would otherwise land on the exact same proxy as
	// every other concurrently running job. Shuffle it so concurrent jobs
	// spread out across the pool instead of piling connections onto one IP.
	rand.Shuffle(len(good), func(a, b int) { good[a], good[b] = good[b], good[a] })

	if len(good) == 0 {
		// The sampled proxies all failed the preflight, but there may be hundreds
		// more in the pool that were never even tried. Give those a chance instead
		// of failing the whole job over one unlucky (or temporarily rate-limited)
		// sample of proxyPreflightSample proxies.
		if len(untested) > 0 {
			return untested, nil
		}

		return nil, fmt.Errorf("%w (tested %d proxies, last error: %s)", ErrNoProxies, len(sample), lastErr)
	}

	// Proxies not tested yet stay in; the job's own requests will tell.
	return append(good, untested...), nil
}

// proxyHealthEntry is the last check of one proxy.
type proxyHealthEntry struct {
	ok  bool
	err string
	at  time.Time
}

// proxyHealth remembers proxy checks: proxy URL -> proxyHealthEntry.
var proxyHealth sync.Map

const (
	// proxyGoodFor is how long a working proxy is trusted without a new check.
	proxyGoodFor = 10 * time.Minute
	// proxyBadFor is how long a failing proxy stays out of the rotation.
	proxyBadFor = 15 * time.Minute
)

// proxyHealthOf returns the remembered health of a proxy, if still fresh.
func proxyHealthOf(p string, now time.Time) (proxyHealthEntry, bool) {
	v, ok := proxyHealth.Load(p)
	if !ok {
		return proxyHealthEntry{}, false
	}

	h, _ := v.(proxyHealthEntry)

	ttl := proxyGoodFor
	if !h.ok {
		ttl = proxyBadFor
	}

	if now.Sub(h.at) > ttl {
		return proxyHealthEntry{}, false
	}

	return h, true
}

// ProxyHealthCounts reports how many proxies were seen working and failing in
// their latest fresh check, for the dashboard.
func ProxyHealthCounts() (good, bad int) {
	now := time.Now()

	proxyHealth.Range(func(k, _ any) bool {
		if h, ok := proxyHealthOf(k.(string), now); ok {
			if h.ok {
				good++
			} else {
				bad++
			}
		}

		return true
	})

	return good, bad
}

// ProxyScrapeStatus tests the ProxyScrape API key and reports what it gives access to.
// refresh downloads the proxy list again instead of using the cache.
func (s *Service) ProxyScrapeStatus(ctx context.Context, refresh bool) ProxyScrapeStatus {
	if s.proxyScrape == nil {
		return ProxyScrapeStatus{Error: ErrProxyScrapeDisabled.Error()}
	}

	return s.proxyScrape.Status(ctx, refresh)
}

// ProxyScrapeClient returns the ProxyScrape client, or ErrProxyScrapeDisabled.
func (s *Service) ProxyScrapeClient() (*ProxyScrape, error) {
	if s.proxyScrape == nil {
		return nil, ErrProxyScrapeDisabled
	}

	return s.proxyScrape, nil
}

func (s *Service) readProxies() ([]string, error) {
	raw, err := os.ReadFile(s.proxiesPath())
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read proxies: %w", err)
	}

	var proxies []string
	if err := json.Unmarshal(raw, &proxies); err != nil {
		return nil, fmt.Errorf("parse proxies: %w", err)
	}

	return proxies, nil
}

func (s *Service) writeProxies(proxies []string) error {
	raw, err := json.MarshalIndent(proxies, "", "  ")
	if err != nil {
		return err
	}

	tmp := s.proxiesPath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write proxies: %w", err)
	}

	return os.Rename(tmp, s.proxiesPath())
}

// SetSavedProxies replaces the saved proxy pool.
func (s *Service) SetSavedProxies(proxies []string) ([]string, error) {
	clean, err := normalizeProxies(proxies)
	if err != nil {
		return nil, err
	}

	proxiesMu.Lock()
	defer proxiesMu.Unlock()

	return clean, s.writeProxies(clean)
}

// AddSavedProxies appends proxies to the pool, skipping ones already saved.
func (s *Service) AddSavedProxies(proxies []string) ([]string, error) {
	proxiesMu.Lock()
	defer proxiesMu.Unlock()

	current, err := s.readProxies()
	if err != nil {
		return nil, err
	}

	clean, err := normalizeProxies(append(current, proxies...))
	if err != nil {
		return nil, err
	}

	return clean, s.writeProxies(clean)
}

// RemoveSavedProxies drops the given proxies from the pool.
func (s *Service) RemoveSavedProxies(proxies []string) ([]string, error) {
	proxiesMu.Lock()
	defer proxiesMu.Unlock()

	current, err := s.readProxies()
	if err != nil {
		return nil, err
	}

	drop := make(map[string]struct{}, len(proxies))
	for _, p := range proxies {
		drop[strings.TrimSpace(p)] = struct{}{}
	}

	kept := make([]string, 0, len(current))

	for _, p := range current {
		if _, ok := drop[p]; !ok {
			kept = append(kept, p)
		}
	}

	return kept, s.writeProxies(kept)
}

// proxyTestLimiter caps how fast testProxy hits Google across every job in the
// queue combined. Without this, a large backlog keeps freeing worker slots
// (a failed preflight is near-instant) and firing off new tests fast enough
// to get the whole shared proxy pool rate-limited by Google at once.
var proxyTestLimiter = rate.NewLimiter(rate.Limit(3), 5)

// TestProxies requests Google Maps through each proxy, in parallel.
func TestProxies(ctx context.Context, proxies []string) []ProxyCheck {
	results := make([]ProxyCheck, len(proxies))

	var wg sync.WaitGroup

	for i, p := range proxies {
		wg.Add(1)

		go func() {
			defer wg.Done()

			results[i] = testProxy(ctx, p)
		}()
	}

	wg.Wait()

	return results
}

func testProxy(ctx context.Context, raw string) ProxyCheck {
	check := ProxyCheck{Proxy: raw}

	if _, err := scrapemate.NewProxy(raw); err != nil {
		check.Error = err.Error()

		return check
	}

	proxyURL, err := url.Parse(raw)
	if err != nil {
		check.Error = err.Error()

		return check
	}

	if err := proxyTestLimiter.Wait(ctx); err != nil {
		check.Error = err.Error()

		return check
	}

	client := &http.Client{
		Timeout:   proxyTestTimeout,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxyTestURL, http.NoBody)
	if err != nil {
		check.Error = err.Error()

		return check
	}

	start := time.Now()

	resp, err := client.Do(req)
	if err != nil {
		check.Error = err.Error()

		return check
	}

	_ = resp.Body.Close()

	check.Status = resp.StatusCode
	check.LatencyMS = time.Since(start).Milliseconds()
	check.OK = resp.StatusCode < http.StatusBadRequest

	if !check.OK {
		check.Error = resp.Status
	}

	return check
}
