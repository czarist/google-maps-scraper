package gmaps

import (
	"regexp"
	"strconv"
	"strings"
)

// searchCenterMetaKey holds the map center Google settled on for a search.
const searchCenterMetaKey = "search_center"

// searchURLMetaKey holds the page URL after the results loaded.
const searchURLMetaKey = "search_url"

var (
	// Place links carry their coordinates as !3d<lat>!4d<lon>.
	placeCoordsRe = regexp.MustCompile(`!3d(-?\d+(?:\.\d+)?)!4d(-?\d+(?:\.\d+)?)`)
	// Map URLs carry the viewport center as /@<lat>,<lon>,<zoom>z.
	viewportRe = regexp.MustCompile(`/@(-?\d+(?:\.\d+)?),(-?\d+(?:\.\d+)?),`)
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
