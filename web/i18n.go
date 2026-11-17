package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"strings"
)

// Lang identifies a supported interface language.
type Lang string

const (
	// LangEN is English, the source language of every message.
	LangEN Lang = "en"
	// LangPTBR is Brazilian Portuguese.
	LangPTBR Lang = "pt-BR"

	langCookie     = "gms-lang"
	langQueryParam = "hl"
	langCookieAge  = 365 * 24 * 60 * 60 // one year, in seconds
)

type langCtxKey struct{}

// catalogs maps a language to its translations. English messages are the keys,
// so a missing entry falls back to the English text.
var catalogs = map[Lang]map[string]string{
	LangPTBR: ptBR,
}

// parseLang returns the supported language matching raw, if any.
func parseLang(raw string) (Lang, bool) {
	raw = strings.ToLower(strings.TrimSpace(raw))

	switch {
	case raw == "":
		return "", false
	case strings.HasPrefix(raw, "pt"):
		return LangPTBR, true
	case strings.HasPrefix(raw, "en"):
		return LangEN, true
	default:
		return "", false
	}
}

// negotiateLang picks the language from ?hl=, then the cookie, then Accept-Language.
func negotiateLang(r *http.Request) (lang Lang, fromQuery bool) {
	if l, ok := parseLang(r.URL.Query().Get(langQueryParam)); ok {
		return l, true
	}

	if c, err := r.Cookie(langCookie); err == nil {
		if l, ok := parseLang(c.Value); ok {
			return l, false
		}
	}

	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag, _, _ := strings.Cut(part, ";")
		if l, ok := parseLang(tag); ok {
			return l, false
		}
	}

	return LangEN, false
}

// withLang stores the negotiated language in the request context and remembers
// an explicit ?hl= choice in a cookie.
func withLang(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lang, fromQuery := negotiateLang(r)
		if fromQuery {
			http.SetCookie(w, &http.Cookie{
				Name:     langCookie,
				Value:    string(lang),
				Path:     "/",
				MaxAge:   langCookieAge,
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			})
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), langCtxKey{}, lang)))
	})
}

// langFrom returns the interface language of the request.
func langFrom(ctx context.Context) Lang {
	if l, ok := ctx.Value(langCtxKey{}).(Lang); ok {
		return l
	}

	return LangEN
}

// translate returns msg in lang, formatted with args when given.
func translate(lang Lang, msg string, args ...any) string {
	if tr, ok := catalogs[lang][msg]; ok {
		msg = tr
	}

	if len(args) > 0 {
		return fmt.Sprintf(msg, args...)
	}

	return msg
}

// trFrom translates msg into the language of the request.
func trFrom(r *http.Request, msg string, args ...any) string {
	return translate(langFrom(r.Context()), msg, args...)
}

// templateFuncs returns the i18n template functions bound to lang.
func templateFuncs(lang Lang) template.FuncMap {
	other, otherName := LangPTBR, "Português"
	if lang == LangPTBR {
		other, otherName = LangEN, "English"
	}

	return template.FuncMap{
		"t": func(msg string, args ...any) string {
			return translate(lang, msg, args...)
		},
		"lang": func() string {
			return string(lang)
		},
		"langSwitch": func() template.HTML {
			return template.HTML(fmt.Sprintf( //nolint:gosec // only constant values are interpolated.
				`<a href="?%s=%s" hreflang="%s" lang="%s">%s</a>`,
				langQueryParam, other, other, other, otherName,
			))
		},
	}
}

// localized returns a copy of tmpl whose i18n functions use the request language.
func localized(tmpl *template.Template, r *http.Request) (*template.Template, error) {
	clone, err := tmpl.Clone()
	if err != nil {
		return nil, err
	}

	return clone.Funcs(templateFuncs(langFrom(r.Context()))), nil
}
