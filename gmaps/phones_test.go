package gmaps

import (
	"reflect"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

func TestCleanEmailStripsGluedPhone(t *testing.T) {
	cases := map[string]string{
		"99545-3421contatohigi@colegiojphigi.com.br": "contatohigi@colegiojphigi.com.br",
		"99885-1359secretaria@colegiojphigi.com.br":  "secretaria@colegiojphigi.com.br",
		"(51)3013-4545escola@example.com.br":         "escola@example.com.br",
		"2020turma@example.com":                      "2020turma@example.com",
		"12345678@example.com":                       "12345678@example.com",
		"contato@example.com":                        "contato@example.com",
	}

	for in, want := range cases {
		got, ok := cleanEmail(in)
		if !ok || got != want {
			t.Errorf("cleanEmail(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

func TestWebsitePhones(t *testing.T) {
	html := `<html><body>
		<a href="tel:+55 51 3013-4545">ligue</a>
		<a href="https://wa.me/5551995453421?text=oi">zap</a>
		<a href="https://api.whatsapp.com/send?phone=5551998851359&text=x">zap2</a>
		<p>Tel: (51) 3062-7993WhatsApp: (51) 99545-3421contatohigi@colegiojphigi.com.br</p>
		<p>+55 (11) 2345 6789 · CEP 90050-170 · CNPJ 12.345.678/0001-90 · 3013-4545 sem DDD</p>
		<p>(51) 1234-5678 inválido · (51) 3013-45456 longo demais</p>
		<script>var x = "(51) 3333-4444";</script>
	</body></html>`

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		t.Fatal(err)
	}

	got := websitePhones(doc)
	want := []string{"(51) 3013-4545", "(51) 99545-3421", "(51) 99885-1359", "(51) 3062-7993", "(11) 2345-6789"}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
