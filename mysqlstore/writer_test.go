package mysqlstore

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gosom/google-maps-scraper/gmaps"
)

func TestToRowMapsMainColumns(t *testing.T) {
	e := &gmaps.Entry{
		PlaceID:      "ChIJabc",
		Title:        "Dental Clinic",
		Category:     "Dentist",
		Phone:        "(11) 1234-5678",
		WebSite:      "https://example.com",
		Address:      "Main St, 1",
		ReviewCount:  42,
		ReviewRating: 4.7,
		Latitude:     -30.03,
		Longtitude:   -51.23,
		Emails:       []string{"a@example.com", "b@example.com"},
		CompleteAddress: gmaps.Address{
			City: "Chicago", State: "IL", Country: "US", PostalCode: "60601",
		},
	}

	row, err := toRow(e)
	if err != nil {
		t.Fatalf("toRow: %v", err)
	}

	if row.PlaceKey != "ChIJabc" || row.City != "Chicago" || row.Phone != e.Phone {
		t.Fatalf("unexpected row: %+v", row)
	}

	if !row.Emails.Valid || row.Emails.String != "a@example.com,b@example.com" {
		t.Fatalf("unexpected emails: %+v", row.Emails)
	}

	if !row.Latitude.Valid || row.Longitude.Float64 != -51.23 {
		t.Fatalf("unexpected coordinates: %+v %+v", row.Latitude, row.Longitude)
	}

	var data map[string]any
	if err := json.Unmarshal(row.Data, &data); err != nil || data["title"] != "Dental Clinic" {
		t.Fatalf("data should hold the full place JSON: %v %s", err, row.Data)
	}
}

func TestToRowHandlesMissingValues(t *testing.T) {
	row, err := toRow(&gmaps.Entry{Title: "No ids", Address: "Somewhere"})
	if err != nil {
		t.Fatalf("toRow: %v", err)
	}

	if row.PlaceKey != "No ids|Somewhere" {
		t.Fatalf("expected title|address fallback key, got %q", row.PlaceKey)
	}

	if row.Emails.Valid || row.Latitude.Valid {
		t.Fatalf("empty emails and 0,0 coordinates should be NULL: %+v", row)
	}
}

func TestToRowTruncatesLongValues(t *testing.T) {
	long := strings.Repeat("é", 2000)

	row, err := toRow(&gmaps.Entry{PlaceID: long, Title: long, Phone: long})
	if err != nil {
		t.Fatalf("toRow: %v", err)
	}

	if n := len([]rune(row.PlaceKey)); n != placeKeyMaxLen {
		t.Fatalf("place key has %d chars, want %d", n, placeKeyMaxLen)
	}

	if n := len([]rune(row.Title)); n != 512 {
		t.Fatalf("title has %d chars, want 512", n)
	}

	if n := len([]rune(row.Phone)); n != 64 {
		t.Fatalf("phone has %d chars, want 64", n)
	}
}
