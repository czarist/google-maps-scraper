package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gosom/scrapemate"
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
	if s.proxyScrape == nil {
		return s.SavedProxies()
	}

	proxies, err := s.proxyScrape.CachedProxies(ctx, false)
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
	if len(proxies) == 0 {
		if s.RequiresProxies() {
			return ErrNoProxies
		}

		return nil
	}

	sample := proxies[:min(len(proxies), proxyPreflightSample)]

	var lastErr string

	for _, r := range TestProxies(ctx, sample) {
		if r.OK {
			return nil
		}

		lastErr = r.Error
	}

	return fmt.Errorf("%w (tested %d proxies, last error: %s)", ErrNoProxies, len(sample), lastErr)
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
