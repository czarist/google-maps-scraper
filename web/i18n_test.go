package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var tCallRe = regexp.MustCompile("\\{\\{t (\"(?:[^\"\\\\]|\\\\.)*\"|`[^`]*`)")

func TestPTBRCoversTemplateStrings(t *testing.T) {
	matches, err := fs.Glob(static, "static/templates/*.html")
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range matches {
		raw, err := fs.ReadFile(static, name)
		if err != nil {
			t.Fatal(err)
		}

		for _, m := range tCallRe.FindAllStringSubmatch(string(raw), -1) {
			key := m[1]
			if strings.HasPrefix(key, "`") {
				key = strings.Trim(key, "`")
			} else if key, err = strconv.Unquote(key); err != nil {
				t.Fatalf("%s: %v", name, err)
			}

			if _, ok := ptBR[key]; !ok {
				t.Errorf("%s: missing pt-BR translation for %q", name, key)
			}
		}
	}
}

func TestNegotiateLang(t *testing.T) {
	cases := []struct {
		query, cookie, accept string
		want                  Lang
	}{
		{"", "", "", LangEN},
		{"", "", "pt-BR,pt;q=0.9,en;q=0.8", LangPTBR},
		{"", "en", "pt-BR", LangEN},
		{"pt-BR", "en", "", LangPTBR},
		{"xx", "", "de,en;q=0.5", LangEN},
	}

	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/?hl="+tc.query, http.NoBody)
		if tc.cookie != "" {
			r.AddCookie(&http.Cookie{Name: langCookie, Value: tc.cookie})
		}

		r.Header.Set("Accept-Language", tc.accept)

		if got, _ := negotiateLang(r); got != tc.want {
			t.Errorf("%+v: got %s, want %s", tc, got, tc.want)
		}
	}
}

func TestIndexRendersInPortuguese(t *testing.T) {
	srv, err := New(NewService(nil, t.TempDir()), ":0")
	if err != nil {
		t.Fatal(err)
	}

	for lang, want := range map[string][]string{
		"pt-BR": {`lang="pt-BR"`, "Nova tarefa", "Criar tarefa", `"Dê um nome à tarefa."`, `value="pt"`},
		"en":    {`lang="en"`, "New job", "Create job", `"Give the job a name."`},
	} {
		rec := httptest.NewRecorder()
		srv.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?hl="+lang, http.NoBody))

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", lang, rec.Code)
		}

		body := rec.Body.String()
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Errorf("%s: body missing %q", lang, w)
			}
		}
	}
}
