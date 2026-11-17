package gmaps

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// searchCenterMetaKey holds the map center Google settled on for a search.
const searchCenterMetaKey = "search_center"

// feedLinkSelector matches the place links in the search results list.
const feedLinkSelector = `div[role=feed] div[jsaction]>a`

// minMedianPoints is the fewest results that give a trustworthy median center.
const minMedianPoints = 3

// searchURLMetaKey holds the page URL after the results loaded.
const searchURLMetaKey = "search_url"

var (
	// Place links carry their coordinates as !3d<lat>!4d<lon>.
	placeCoordsRe = regexp.MustCompile(`!3d(-?\d+(?:\.\d+)?)!4d(-?\d+(?:\.\d+)?)`)
	// Map URLs carry the viewport center as /@<lat>,<lon>,<zoom>z.
	viewportRe = regexp.MustCompile(`/@(-?\d+(?:\.\d+)?),(-?\d+(?:\.\d+)?),`)
	// The page boots with the viewport as APP_INITIALIZATION_STATE=[[[<span>,<lon>,<lat>].
	appStateRe = regexp.MustCompile(`APP_INITIALIZATION_STATE=\[\[\[(-?[\d.eE+-]+),(-?\d+(?:\.\d+)?),(-?\d+(?:\.\d+)?)\]`)
)

type latLon struct {
	Lat, Lon float64
}

func parseLatLon(rawLat, rawLon string) (latLon, bool) {
	lat, err := strconv.ParseFloat(rawLat, 64)
	if err != nil || lat < -90 || lat > 90 {
		return latLon{}, false
	}

	lon, err := strconv.ParseFloat(rawLon, 64)
	if err != nil || lon < -180 || lon > 180 {
		return latLon{}, false
	}

	return latLon{Lat: lat, Lon: lon}, true
}

// placeCoords extracts the coordinates embedded in a Google Maps place link.
func placeCoords(href string) (latLon, bool) {
	m := placeCoordsRe.FindStringSubmatch(href)
	if m == nil {
		return latLon{}, false
	}

	return parseLatLon(m[1], m[2])
}

// viewportCenter extracts the map center from a Google Maps URL.
func viewportCenter(u string) (latLon, bool) {
	m := viewportRe.FindStringSubmatch(u)
	if m == nil {
		return latLon{}, false
	}

	return parseLatLon(m[1], m[2])
}

// parseGeoCoordinates parses "lat,lon". A 0,0 point counts as unset.
func parseGeoCoordinates(s string) (latLon, bool) {
	rawLat, rawLon, ok := strings.Cut(strings.ReplaceAll(s, " ", ""), ",")
	if !ok {
		return latLon{}, false
	}

	p, ok := parseLatLon(rawLat, rawLon)
	if !ok || (p.Lat == 0 && p.Lon == 0) {
		return latLon{}, false
	}

	return p, true
}

// distanceMeters returns the great-circle distance between a and b.
func distanceMeters(a, b latLon) float64 {
	e := Entry{Latitude: b.Lat, Longtitude: b.Lon}

	return e.haversineDistance(a.Lat, a.Lon)
}

// appStateCenter extracts the initial map center from a Google Maps page.
func appStateCenter(body string) (latLon, bool) {
	m := appStateRe.FindStringSubmatch(body)
	if m == nil {
		return latLon{}, false
	}

	p, ok := parseLatLon(m[3], m[2])
	if !ok || (p.Lat == 0 && p.Lon == 0) {
		return latLon{}, false
	}

	return p, true
}

// feedPlaceCoords returns the coordinates of every result link that has them.
func feedPlaceCoords(doc *goquery.Document) []latLon {
	var points []latLon

	doc.Find(feedLinkSelector).Each(func(_ int, s *goquery.Selection) {
		if p, ok := placeCoords(s.AttrOr("href", "")); ok {
			points = append(points, p)
		}
	})

	return points
}

// medianCenter returns the per-axis median of points.
func medianCenter(points []latLon) (latLon, bool) {
	if len(points) < minMedianPoints {
		return latLon{}, false
	}

	lats := make([]float64, len(points))
	lons := make([]float64, len(points))

	for i, p := range points {
		lats[i], lons[i] = p.Lat, p.Lon
	}

	return latLon{Lat: median(lats), Lon: median(lons)}, true
}

func median(xs []float64) float64 {
	sort.Float64s(xs)

	mid := len(xs) / 2
	if len(xs)%2 == 0 {
		return (xs[mid-1] + xs[mid]) / 2
	}

	return xs[mid]
}
