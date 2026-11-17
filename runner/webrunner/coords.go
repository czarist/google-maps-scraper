package webrunner

import "strconv"

// defaultRadiusMeters is used when a job has no radius. It covers a city and
// its neighbors while dropping results from other regions.
const defaultRadiusMeters = 50000

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
