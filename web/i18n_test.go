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

func TestPTBRTranslatesDispatchProgress(t *testing.T) {
	cases := map[string]string{
		"Sent 20, failed 0, skipped 0. Waiting 4m56s before the next batch.": "Enviados 20, com erro 0, ignorados 0. Aguardando 4m56s antes do próximo lote.",
		"Sent 3, failed 1, skipped 2. Waiting 48s before the next message.":  "Enviados 3, com erro 1, ignorados 2. Aguardando 48s antes da próxima mensagem.",
		"No new email addresses. Skipped 7 already sent.":                    "Nenhum endereço novo. Ignorados 7 já enviados.",
		"something else": "something else",
	}

	for in, want := range cases {
		if got := translate(LangPTBR, in); got != want {
			t.Errorf("translate(%q) = %q, want %q", in, got, want)
		}

		if got := translate(LangEN, in); got != in {
			t.Errorf("English changed %q to %q", in, got)
		}
	}
}
