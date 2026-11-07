package jshttp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStartAuthProxyWithoutCredentials(t *testing.T) {
	var gotAuth string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Proxy-Authorization")
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	p, err := StartAuthProxy(upstream.URL, "", "")
	if err != nil {
		t.Fatalf("expected proxy without credentials to start, got %v", err)
	}
	defer p.Close()

	resp, err := p.HTTPClient().Get("http://example.com/")
	if err != nil {
		t.Fatalf("request through proxy failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("unexpected body %q", body)
	}

	if gotAuth != "" {
		t.Fatalf("expected no Proxy-Authorization header, got %q", gotAuth)
	}
}
