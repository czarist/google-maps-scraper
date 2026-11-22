package jshttp

import (
	"strings"
	"testing"
)

func TestChromeUserAgent(t *testing.T) {
	cases := map[string]string{
		"145.0.7632.6": "Chrome/145.0.0.0 ",
		" 139.0.1 ":    "Chrome/139.0.0.0 ",
		"":             "Chrome/" + fallbackChromeMajor + ".0.0.0 ",
		"Headless/1.2": "Chrome/" + fallbackChromeMajor + ".0.0.0 ",
	}

	for in, want := range cases {
		got := chromeUserAgent(in)
		if !strings.Contains(got, want) || strings.Contains(got, "Headless") {
			t.Fatalf("chromeUserAgent(%q) = %q, want %q inside", in, got, want)
		}
	}
}
