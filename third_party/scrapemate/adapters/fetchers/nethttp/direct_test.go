//nolint:testpackage // the direct-fetch branch is on the unexported fetcher
package nethttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gosom/scrapemate"
)

type directJob struct {
	scrapemate.Job
}

func (directJob) DirectFetch() bool { return true }

type denyClient struct {
	called bool
}

func (d *denyClient) Do(*http.Request) (*http.Response, error) {
	d.called = true

	return nil, errors.New("proxied client used")
}

func TestDirectFetchDoesNotUseProxiedClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))
	t.Cleanup(srv.Close)

	proxied := &denyClient{}
	fetcher := New(proxied)
	job := &directJob{Job: scrapemate.Job{Method: http.MethodGet, URL: srv.URL}}

	resp := fetcher.Fetch(context.Background(), job)
	if resp.Error != nil {
		t.Fatalf("fetch: %v", resp.Error)
	}

	if proxied.called {
		t.Fatal("email fetch used the proxied client")
	}

	if string(resp.Body) != "hello" {
		t.Fatalf("body %q", resp.Body)
	}
}
