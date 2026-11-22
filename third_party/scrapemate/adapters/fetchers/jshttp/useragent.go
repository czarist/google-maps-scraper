package jshttp

import "strings"

// fallbackChromeMajor is used when the browser does not report a version.
const fallbackChromeMajor = "140"

// chromeUserAgent is the user agent of a desktop Chrome with the same major
// version as the running Chromium. A fixed old version next to a much newer
// engine is an easy bot signal. The platform matches the Linux container, and
// minor parts are zero, as Chrome itself reports them.
func chromeUserAgent(version string) string {
	major, _, _ := strings.Cut(strings.TrimSpace(version), ".")
	if major == "" || strings.Trim(major, "0123456789") != "" {
		major = fallbackChromeMajor
	}

	return "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" +
		major + ".0.0.0 Safari/537.36"
}
