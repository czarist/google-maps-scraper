package gmaps

import (
	"context"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/gosom/scrapemate"
)

const (
	poaPlace = "https://www.google.com/maps/place/Escola+A/data=!4m7!3m6!1s0x0:0x1!8m2!3d-30.0346!4d-51.2177!16s"
	canoas   = "https://www.google.com/maps/place/Escola+B/data=!4m7!3m6!1s0x0:0x2!8m2!3d-29.9178!4d-51.1839!16s"
	salvador = "https://www.google.com/maps/place/Escola+C/data=!4m7!3m6!1s0x0:0x3!8m2!3d-12.9714!4d-38.5014!16s"
	noCoords = "https://www.google.com/maps/place/Escola+D/"
)

func TestPlaceCoordsAndViewport(t *testing.T) {
	p, ok := placeCoords(salvador)
	if !ok || p.Lat != -12.9714 || p.Lon != -38.5014 {
		t.Fatalf("placeCoords = %+v, %v", p, ok)
	}

	if _, ok := placeCoords(noCoords); ok {
		t.Fatal("expected no coordinates")
	}

	c, ok := viewportCenter("https://www.google.com/maps/search/escolas+porto+alegre/@-30.0277,-51.2287,12z?hl=pt")
	if !ok || c.Lat != -30.0277 || c.Lon != -51.2287 {
		t.Fatalf("viewportCenter = %+v, %v", c, ok)
	}

	if _, ok := parseGeoCoordinates("0,0"); ok {
		t.Fatal("0,0 must count as unset")
	}
}

func TestProcessDropsFarPlaces(t *testing.T) {
	html := `<div role="feed">` +
		`<div jsaction="x"><a href="` + poaPlace + `"></a></div>` +
		`<div jsaction="x"><a href="` + canoas + `"></a></div>` +
		`<div jsaction="x"><a href="` + salvador + `"></a></div>` +
		`<div jsaction="x"><a href="` + noCoords + `"></a></div>` +
		`</div>`

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		t.Fatal(err)
	}

	job := NewGmapJob("", "pt", "escolas porto alegre", 1, false, "", 0, WithMaxDistance(50000))
	resp := &scrapemate.Response{
		URL:      job.URL,
		Document: doc,
		Meta:     map[string]any{searchCenterMetaKey: latLon{Lat: -30.0277, Lon: -51.2287}},
	}

	_, next, err := job.Process(context.Background(), resp)
	if err != nil {
		t.Fatal(err)
	}

	got := make([]string, 0, len(next))
	for _, n := range next {
		got = append(got, n.(*PlaceJob).URL)
	}

	if len(got) != 3 || strings.Contains(strings.Join(got, " "), "Escola+C") {
		t.Fatalf("kept %v, want Porto Alegre, Canoas and the place without coordinates", got)
	}
}
