package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestProxyScrape(t *testing.T, credentials bool) *ProxyScrape {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/subaccounts", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("api-token") != "key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"status":"unauthorized","info":"bad key"}`))

			return
		}

		_, _ = w.Write([]byte(`{"success":true,"data":{"subaccounts":[
			{"AccountID":"dc1","AccountType":"datacenter_shared"},
			{"AccountID":"res1","AccountType":"residential"}],"total":2}}`))
	})
	mux.HandleFunc("/dc1/datacenter_shared/proxy-list", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") == "credentials" {
			if !credentials {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"Credential-based authentication is not enabled for this account."}`))

				return
			}

			_, _ = w.Write([]byte("http://u:p@1.2.3.4:3128\r\nhttp://u:p@5.6.7.8:3128\n"))

			return
		}

		_, _ = w.Write([]byte("http://1.2.3.4:3128\n"))
	})
	mux.HandleFunc("/res1/residential/login", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"username":"ruser","password":"rpass"}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &ProxyScrape{
		apiKey:             "key",
		protocol:           "http",
		country:            "br",
		residentialGateway: "rp.scrapegw.com:6060",
		baseURL:            srv.URL,
		client:             srv.Client(),
		cacheTime:          time.Now,
	}
}

func TestProxyScrapeFetchProxies(t *testing.T) {
	got, err := newTestProxyScrape(t, true).FetchProxies(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"http://u:p@1.2.3.4:3128", "http://u:p@5.6.7.8:3128", "http://ruser-country-br:rpass@rp.scrapegw.com:6060"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("proxy %d: got %q, want %q", i, got[i], want[i])
		}
	}

	if _, err := normalizeProxies(got); err != nil {
		t.Fatalf("proxies are not valid for jobs: %v", err)
	}
}

func TestProxyScrapeFallsBackToIPAuth(t *testing.T) {
	got, err := newTestProxyScrape(t, false).FetchProxies(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(got) == 0 || got[0] != "http://1.2.3.4:3128" {
		t.Fatalf("got %v", got)
	}
}

func TestProxyScrapeOnlyListedSubaccounts(t *testing.T) {
	ps := newTestProxyScrape(t, true)
	ps.onlySubaccounts = parseSubaccountIDs(" RES1 ")

	got, err := ps.FetchProxies(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0] != "http://ruser-country-br:rpass@rp.scrapegw.com:6060" {
		t.Fatalf("got %v, want only the res1 proxy", got)
	}

	if parseSubaccountIDs(" , ") != nil {
		t.Fatal("blank list should mean every plan")
	}
}

func TestProxyScrapeBadKey(t *testing.T) {
	ps := newTestProxyScrape(t, true)
	ps.apiKey = "wrong"

	st := ps.Status(context.Background(), true)
	if st.OK || st.Error == "" {
		t.Fatalf("expected error status, got %+v", st)
	}
}

func TestRedactProxies(t *testing.T) {
	got := redactProxies([]string{"http://u:secret@h:1", "http://h:2"})
	if got[0] != "http://u:xxxxx@h:1" || got[1] != "http://h:2" {
		t.Fatalf("got %v", got)
	}
}

func TestCheckJobProxiesRefusesDirectWhenRequired(t *testing.T) {
	svc := &Service{proxyScrape: &ProxyScrape{}}

	if err := svc.CheckJobProxies(context.Background(), nil); !errors.Is(err, ErrNoProxies) {
		t.Fatalf("got %v, want ErrNoProxies", err)
	}

	if err := (&Service{}).CheckJobProxies(context.Background(), nil); err != nil {
		t.Fatalf("without ProxyScrape a job may run direct, got %v", err)
	}
}

func TestCheckJobProxiesRefusesDeadProxies(t *testing.T) {
	// Nothing listens on port 1, so the proxy is dead.
	err := (&Service{}).CheckJobProxies(context.Background(), []string{"http://127.0.0.1:1"})
	if !errors.Is(err, ErrNoProxies) {
		t.Fatalf("got %v, want ErrNoProxies", err)
	}
}

func TestDefaultProxiesNoFallbackWhenAPIFails(t *testing.T) {
	ps := newTestProxyScrape(t, true)
	ps.apiKey = "wrong"

	svc := &Service{proxyScrape: ps, dataFolder: t.TempDir()}
	if _, err := svc.AddSavedProxies([]string{"http://5.5.5.5:80"}); err != nil {
		t.Fatal(err)
	}

	if got, err := svc.DefaultProxies(context.Background()); !errors.Is(err, ErrNoProxies) {
		t.Fatalf("got %v, %v; want ErrNoProxies", got, err)
	}
}

func TestDeadProxyLeavesTheRotation(t *testing.T) {
	dead := "http://127.0.0.1:1"

	if _, err := (&Service{}).JobProxies(context.Background(), []string{dead}); !errors.Is(err, ErrNoProxies) {
		t.Fatalf("got %v, want ErrNoProxies", err)
	}

	h, ok := proxyHealthOf(dead, time.Now())
	if !ok || h.ok {
		t.Fatalf("dead proxy not remembered as bad: %+v %v", h, ok)
	}

	// Still out of the rotation without a new test, and not handed to the job.
	start := time.Now()
	if _, err := (&Service{}).JobProxies(context.Background(), []string{dead}); !errors.Is(err, ErrNoProxies) {
		t.Fatalf("got %v, want ErrNoProxies", err)
	}

	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("a known-bad proxy was tested again (%s)", took)
	}

	if _, bad := ProxyHealthCounts(); bad < 1 {
		t.Fatal("dashboard count misses the bad proxy")
	}
}

func TestRateLimitDoesNotSidelineThePool(t *testing.T) {
	proxies := []string{"http://10.8.8.1:8080", "http://10.8.8.2:8080", "http://10.8.8.3:8080"}
	resetProxyCooling()

	t.Cleanup(func() {
		for _, p := range proxies {
			proxyHealth.Delete(p)
		}

		resetProxyCooling()

		runProxyTests = TestProxies
	})

	// What the queue looked like after the outage: every exit cached as a 429,
	// so the next job tested nothing and failed.
	for _, p := range proxies {
		proxyHealth.Store(p, proxyHealthEntry{
			ok:  false,
			err: `Get "https://www.google.com/maps?hl=en": Too Many Requests`,
			at:  time.Now(),
		})
	}

	calls := 0
	runProxyTests = func(_ context.Context, got []string) []ProxyCheck {
		calls++

		return []ProxyCheck{{
			Proxy: got[0],
			Error: `Get "https://www.google.com/maps?hl=en": Too Many Requests`,
		}}
	}

	_, err := (&Service{}).JobProxies(context.Background(), proxies)
	if !errors.Is(err, ErrProxyCooling) {
		t.Fatalf("rate limit error = %v, want the job to wait", err)
	}

	if calls != 1 {
		t.Fatalf("tested %d proxies, want 1 (a 429 stops the sample)", calls)
	}

	for _, p := range proxies {
		if h, ok := proxyHealthOf(p, time.Now()); ok && !h.ok {
			t.Fatalf("proxy %s still cached as dead: %s", p, h.err)
		}
	}

	// While the account is cooling, the next job waits and does not open another test.
	calls = 0
	if _, err := (&Service{}).JobProxies(context.Background(), proxies); !errors.Is(err, ErrProxyCooling) {
		t.Fatalf("while cooling, error = %v, want the job to wait", err)
	}

	if calls != 0 {
		t.Fatalf("cooling job tested %d proxies, want 0", calls)
	}
}
