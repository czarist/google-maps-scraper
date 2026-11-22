package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// ProxyScrape account API. Docs: https://docs.proxyscrape.com/api/account
const (
	proxyScrapeBaseURL     = "https://api.proxyscrape.com/v4/account"
	proxyScrapeTimeout     = 30 * time.Second
	proxyScrapeCacheTTL    = 10 * time.Minute
	proxyScrapeMaxBodySize = 4 << 20

	// Environment variables read by the ProxyScrape integration.
	envProxyScrapeKey         = "PROXYSCRAPE_API_KEY"
	envProxyScrapeProtocol    = "PROXYSCRAPE_PROTOCOL"
	envProxyScrapeCountry     = "PROXYSCRAPE_COUNTRY"
	envProxyScrapeResidential = "PROXYSCRAPE_RESIDENTIAL_GATEWAY"
	envProxyScrapeSubaccounts = "PROXYSCRAPE_SUBACCOUNTS"
)

// ErrProxyScrapeDisabled means PROXYSCRAPE_API_KEY is not set.
var ErrProxyScrapeDisabled = errors.New("ProxyScrape is not configured: set " + envProxyScrapeKey + " in .env")

// ProxyScrape talks to the ProxyScrape account API.
type ProxyScrape struct {
	apiKey             string
	protocol           string // http or socks
	country            string // ISO alpha-2, empty for all
	residentialGateway string // host:port, empty to skip residential plans
	// onlySubaccounts, when not empty, is the set of plan IDs to use. Others are ignored.
	onlySubaccounts map[string]struct{}
	baseURL            string
	client             *http.Client

	mu        sync.Mutex
	cached    []string
	cachedAt  time.Time
	cacheErr  error
	cacheTime func() time.Time
}

// NewProxyScrapeFromEnv builds a client from environment variables.
// It returns nil when PROXYSCRAPE_API_KEY is empty.
func NewProxyScrapeFromEnv() *ProxyScrape {
	key := strings.TrimSpace(os.Getenv(envProxyScrapeKey))
	if key == "" {
		return nil
	}

	protocol := strings.ToLower(strings.TrimSpace(os.Getenv(envProxyScrapeProtocol)))
	if protocol != "socks" {
		protocol = "http"
	}

	return &ProxyScrape{
		apiKey:             key,
		protocol:           protocol,
		country:            strings.ToLower(strings.TrimSpace(os.Getenv(envProxyScrapeCountry))),
		residentialGateway: strings.TrimSpace(os.Getenv(envProxyScrapeResidential)),
		onlySubaccounts:    parseSubaccountIDs(os.Getenv(envProxyScrapeSubaccounts)),
		baseURL:            proxyScrapeBaseURL,
		client:             &http.Client{Timeout: proxyScrapeTimeout},
		cacheTime:          time.Now,
	}
}

// parseSubaccountIDs reads a comma or space separated list of plan IDs.
// It returns nil for an empty list, which means every plan.
func parseSubaccountIDs(raw string) map[string]struct{} {
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '	' })
	if len(fields) == 0 {
		return nil
	}

	out := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		out[strings.ToLower(f)] = struct{}{}
	}

	return out
}

// ProxyScrapeSubaccount is one plan (subaccount) of the account.
type ProxyScrapeSubaccount struct {
	AccountID   string `json:"AccountID"`
	AccountType string `json:"AccountType"`
	Label       string `json:"label"`
	IsHidden    bool   `json:"is_hidden"`
}

// ProxyScrapeBalance is the wallet balance used by API ordering.
type ProxyScrapeBalance struct {
	Balance  float64 `json:"balance"`
	Currency string  `json:"currency"`
}

// ProxyScrapeStatus summarizes whether the API key works and what it gives access to.
type ProxyScrapeStatus struct {
	Configured   bool                    `json:"configured"`
	OK           bool                    `json:"ok"`
	Error        string                  `json:"error,omitempty"`
	Subaccounts  []ProxyScrapeSubaccount `json:"subaccounts,omitempty"`
	ProxyCount   int                     `json:"proxy_count"`
	Sample       []string                `json:"sample,omitempty"`
	ProxyError   string                  `json:"proxy_error,omitempty"`
	Balance      *ProxyScrapeBalance     `json:"balance,omitempty"`
	BalanceError string                  `json:"balance_error,omitempty"`
}

// ProxyScrapeOrder is an order's state as returned by the orders API.
type ProxyScrapeOrder struct {
	OrderID        string                 `json:"order_id"`
	Status         string                 `json:"status"`
	Type           string                 `json:"type"`
	Price          float64                `json:"price"`
	Currency       string                 `json:"currency"`
	PaidWithCredit bool                   `json:"paid_with_credit"`
	BlockReason    string                 `json:"block_reason,omitempty"`
	Subaccount     *ProxyScrapeSubaccount `json:"subaccount,omitempty"`
}

// proxyScrapeAPIError carries the HTTP status and the API's message.
type proxyScrapeAPIError struct {
	Status  int
	Message string
}

func (e *proxyScrapeAPIError) Error() string {
	return fmt.Sprintf("ProxyScrape API %d: %s", e.Status, e.Message)
}

func (p *ProxyScrape) do(ctx context.Context, method, path string, query url.Values, body any) ([]byte, error) {
	u := p.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var reader io.Reader = http.NoBody

	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}

		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return nil, err
	}

	req.Header.Set("api-token", p.apiKey)
	req.Header.Set("Accept", "application/json")

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ProxyScrape request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, proxyScrapeMaxBodySize))
	if err != nil {
		return nil, fmt.Errorf("ProxyScrape read: %w", err)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, &proxyScrapeAPIError{Status: resp.StatusCode, Message: apiErrorMessage(raw)}
	}

	return raw, nil
}

// apiErrorMessage pulls a readable message out of the API's varied error bodies.
func apiErrorMessage(raw []byte) string {
	var body struct {
		Error   string `json:"error"`
		Info    string `json:"info"`
		Message string `json:"message"`
	}

	if json.Unmarshal(raw, &body) == nil {
		for _, m := range []string{body.Error, body.Info, body.Message} {
			if m != "" {
				return m
			}
		}
	}

	msg := strings.TrimSpace(string(raw))
	if len(msg) > 200 { //nolint:mnd // keep error messages short
		msg = msg[:200]
	}

	if msg == "" {
		msg = "empty response"
	}

	return msg
}

// Subaccounts lists the account's plans. When PROXYSCRAPE_SUBACCOUNTS is set,
// only those plans are listed.
func (p *ProxyScrape) Subaccounts(ctx context.Context) ([]ProxyScrapeSubaccount, error) {
	raw, err := p.do(ctx, http.MethodGet, "/subaccounts", nil, nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Data struct {
			Subaccounts []ProxyScrapeSubaccount `json:"subaccounts"`
		} `json:"data"`
	}

	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parse subaccounts: %w", err)
	}

	return p.allowedSubaccounts(resp.Data.Subaccounts), nil
}

func (p *ProxyScrape) allowedSubaccounts(subs []ProxyScrapeSubaccount) []ProxyScrapeSubaccount {
	if len(p.onlySubaccounts) == 0 {
		return subs
	}

	out := make([]ProxyScrapeSubaccount, 0, len(subs))
	for _, sub := range subs {
		if _, ok := p.onlySubaccounts[strings.ToLower(strings.TrimSpace(sub.AccountID))]; ok {
			out = append(out, sub)
		}
	}

	return out
}

// Balance returns the wallet balance. Needs API ordering enabled by ProxyScrape support.
func (p *ProxyScrape) Balance(ctx context.Context) (*ProxyScrapeBalance, error) {
	raw, err := p.do(ctx, http.MethodGet, "/orders/balance", nil, nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Data ProxyScrapeBalance `json:"data"`
	}

	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parse balance: %w", err)
	}

	return &resp.Data, nil
}

// datacenterProxies downloads the online proxies of one datacenter plan as proxy URLs.
// It asks for user:pass credentials first and falls back to IP-whitelisted host:port.
func (p *ProxyScrape) datacenterProxies(ctx context.Context, id string) ([]string, error) {
	path := "/" + url.PathEscape(id) + "/datacenter_shared/proxy-list"

	q := url.Values{}
	q.Set("protocol", p.protocol)
	q.Set("type", "displayproxies")
	q.Set("status", "online")
	q.Set("format", "credentials")
	q.Set("credential_format", "3") // scheme://user:pass@host:port

	if p.country != "" {
		q.Set("country[]", p.country)
	}

	raw, err := p.do(ctx, http.MethodGet, path, q, nil)

	var apiErr *proxyScrapeAPIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden {
		// Credential auth is off for this account: use IP authentication.
		q.Del("format")
		q.Del("credential_format")
		q.Set("proxy_format", "protocolipport")

		raw, err = p.do(ctx, http.MethodGet, path, q, nil)
	}

	if err != nil {
		return nil, err
	}

	return parseProxyLines(raw, p.scheme()), nil
}

func (p *ProxyScrape) scheme() string {
	if p.protocol == "socks" {
		return "socks5"
	}

	return "http"
}

// parseProxyLines reads one proxy per line and adds the scheme where missing.
func parseProxyLines(raw []byte, scheme string) []string {
	var out []string

	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}

		if !strings.Contains(line, "://") {
			line = scheme + "://" + line
		}

		out = append(out, line)
	}

	return out
}

// residentialProxy returns the gateway URL for a residential plan.
func (p *ProxyScrape) residentialProxy(ctx context.Context, id string) (string, error) {
	raw, err := p.do(ctx, http.MethodGet, "/"+url.PathEscape(id)+"/residential/login", nil, nil)
	if err != nil {
		return "", err
	}

	var login struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}

	if err := json.Unmarshal(raw, &login); err != nil {
		return "", fmt.Errorf("parse residential login: %w", err)
	}

	user := login.Username
	if p.country != "" {
		user += "-country-" + p.country
	}

	u := url.URL{Scheme: p.scheme(), User: url.UserPassword(user, login.Password), Host: p.residentialGateway}

	return u.String(), nil
}

// FetchProxies downloads the proxies of every plan on the account.
func (p *ProxyScrape) FetchProxies(ctx context.Context) ([]string, error) {
	subs, err := p.Subaccounts(ctx)
	if err != nil {
		return nil, err
	}

	var (
		out  []string
		errs []error
	)

	for _, sub := range subs {
		kind := strings.ToLower(sub.AccountType)

		switch {
		case strings.HasPrefix(kind, "datacenter"):
			list, err := p.datacenterProxies(ctx, sub.AccountID)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s %s: %w", sub.AccountType, sub.AccountID, err))

				continue
			}

			out = append(out, list...)
		case strings.HasPrefix(kind, "residential") && p.residentialGateway != "":
			proxy, err := p.residentialProxy(ctx, sub.AccountID)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s %s: %w", sub.AccountType, sub.AccountID, err))

				continue
			}

			out = append(out, proxy)
		}
	}

	if len(out) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	for _, e := range errs {
		log.Printf("proxyscrape: %v", e)
	}

	if len(out) > maxSavedProxies {
		out = out[:maxSavedProxies]
	}

	return out, nil
}

// CachedProxies returns the account proxies, refreshing at most every proxyScrapeCacheTTL.
func (p *ProxyScrape) CachedProxies(ctx context.Context, refresh bool) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !refresh && !p.cachedAt.IsZero() && p.cacheTime().Sub(p.cachedAt) < proxyScrapeCacheTTL {
		return p.cached, p.cacheErr
	}

	proxies, err := p.FetchProxies(ctx)
	p.cached, p.cacheErr, p.cachedAt = proxies, err, p.cacheTime()

	return proxies, err
}

// Status checks the key, lists plans, counts the online proxies and reads the balance.
// refresh downloads the proxy list again instead of using the cache.
func (p *ProxyScrape) Status(ctx context.Context, refresh bool) ProxyScrapeStatus {
	st := ProxyScrapeStatus{Configured: true}

	subs, err := p.Subaccounts(ctx)
	if err != nil {
		st.Error = err.Error()

		return st
	}

	st.OK = true
	st.Subaccounts = subs

	proxies, err := p.CachedProxies(ctx, refresh)
	if err != nil {
		st.ProxyError = err.Error()
	}

	st.ProxyCount = len(proxies)
	st.Sample = redactProxies(proxies[:min(len(proxies), 3)]) //nolint:mnd // a short sample is enough

	if bal, err := p.Balance(ctx); err != nil {
		st.BalanceError = err.Error()
	} else {
		st.Balance = bal
	}

	return st
}

// redactProxies hides passwords so the sample can be shown safely.
func redactProxies(proxies []string) []string {
	out := make([]string, len(proxies))

	for i, raw := range proxies {
		u, err := url.Parse(raw)
		if err != nil || u.User == nil {
			out[i] = raw

			continue
		}

		if _, has := u.User.Password(); has {
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
		}

		out[i] = u.String()
	}

	return out
}

// CreateOrder places (or, with dryRun, only prices) an order paid from the wallet.
// order holds the product fields from the docs, e.g. {"type": "new_datacenter_shared", "country": "Brazil", ...}.
// It returns the order id, or the raw quote when dryRun is set.
func (p *ProxyScrape) CreateOrder(ctx context.Context, order map[string]any, dryRun bool) (json.RawMessage, error) {
	if _, ok := order["type"].(string); !ok {
		return nil, errors.New(`order needs a "type", e.g. "new_datacenter_shared"`)
	}

	body := make(map[string]any, len(order)+1)
	for k, v := range order {
		body[k] = v
	}

	body["dry_run"] = dryRun

	raw, err := p.do(ctx, http.MethodPost, "/orders/create", nil, body)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Data json.RawMessage `json:"data"`
	}

	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parse order: %w", err)
	}

	return resp.Data, nil
}

// OrderStatus reads an order's state. When it is "processed", the new plan's proxies are available.
func (p *ProxyScrape) OrderStatus(ctx context.Context, orderID string) (*ProxyScrapeOrder, error) {
	raw, err := p.do(ctx, http.MethodGet, "/orders/"+url.PathEscape(orderID)+"/status", nil, nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Data ProxyScrapeOrder `json:"data"`
	}

	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parse order status: %w", err)
	}

	return &resp.Data, nil
}
