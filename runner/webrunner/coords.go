package webrunner

import "strconv"

// noPlacesError is recorded when a run ends without finding any place.
const noPlacesError = "no places found: the search failed or was blocked (see the server log); try again, use proxies, or check the search text"

// hasCoordinates reports whether lat and lon form a real point.
// The form sends 0,0 by default, which means "not set".
func hasCoordinates(lat, lon string) bool {
	la, err := strconv.ParseFloat(lat, 64)
	if err != nil {
		return false
	}

	lo, err := strconv.ParseFloat(lon, 64)
	if err != nil {
		return false
	}

	return la != 0 || lo != 0
}
