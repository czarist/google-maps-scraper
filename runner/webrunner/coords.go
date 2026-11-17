package webrunner

import "strconv"

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
