package web

import (
	"testing"
	"time"
)

func TestJobDataValidateProxies(t *testing.T) {
	base := JobData{
		Keywords: []string{"cafe"},
		Lang:     "en",
		Depth:    1,
		MaxTime:  10 * time.Minute,
	}

	ok := base
	ok.Proxies = []string{"http://u:p@1.2.3.4:8080", "socks5://u:p@1.2.3.4:1080"}

	if err := ok.Validate(); err != nil {
		t.Fatalf("expected supported proxies to pass, got %v", err)
	}

	bad := base
	bad.Proxies = []string{"socks4://1.2.3.4:4145"}

	if err := bad.Validate(); err == nil {
		t.Fatal("expected socks4 proxy to be rejected")
	}

	noAuth := base
	noAuth.Proxies = []string{"socks5://1.2.3.4:1080", "http://1.2.3.4:8080"}

	if err := noAuth.Validate(); err != nil {
		t.Fatalf("expected proxies without credentials to pass, got %v", err)
	}
}
