package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gosom/scrapemate"

	"github.com/gosom/google-maps-scraper/gmaps"
)

const (
	maxBatchSize = 50
	// placeKeyMaxLen matches the place_key column size.
	placeKeyMaxLen = 255
	saveTimeout    = 30 * time.Second
)

var errInvalidData = errors.New("mysqlstore: result is not a *gmaps.Entry")

// placeRow is one row of the places table.
type placeRow struct {
	PlaceKey     string
	Title        string
	Category     string
	Phone        string
	Website      string
	Emails       sql.NullString
	Address      string
	City         string
	State        string
	Country      string
	PostalCode   string
	Latitude     sql.NullFloat64
	Longitude    sql.NullFloat64
	ReviewCount  int
	ReviewRating float64
	Link         string
	Data         []byte
}

// NewResultWriter returns a writer that saves every place of a job to MySQL.
// Rows are unique per job and place, so re-saving a place updates it.
func NewResultWriter(db *sql.DB, jobID, jobName string) scrapemate.ResultWriter {
	return &resultWriter{db: db, jobID: jobID, jobName: truncate(jobName, 255)}
}

type resultWriter struct {
	db      *sql.DB
	jobID   string
	jobName string
}

// Run saves results in batches. A database error is logged and the writer
// keeps draining the channel, so the scrape (and its CSV) still finishes.
func (w *resultWriter) Run(ctx context.Context, in <-chan scrapemate.Result) error {
	buff := make([]placeRow, 0, maxBatchSize)
	failed := 0

	flush := func() {
		if len(buff) == 0 {
			return
		}

		if err := w.save(ctx, buff); err != nil {
			failed += len(buff)
			log.Printf("mysql: could not save %d places for job %s: %v", len(buff), w.jobID, err)
		}

		buff = buff[:0]
	}

	for result := range in {
		entry, ok := result.Data.(*gmaps.Entry)
		if !ok {
			log.Printf("mysql: %v", errInvalidData)

			continue
		}

		row, err := toRow(entry)
		if err != nil {
			log.Printf("mysql: skipping place %q: %v", entry.Title, err)

			continue
		}

		buff = append(buff, row)

		if len(buff) >= maxBatchSize {
			flush()
		}
	}

	flush()

	if failed > 0 {
		log.Printf("mysql: job %s finished with %d places not saved", w.jobID, failed)
	}

	return nil
}

func (w *resultWriter) save(ctx context.Context, rows []placeRow) error {
	const cols = 19

	var q strings.Builder

	q.WriteString(`INSERT INTO places (job_id, job_name, place_key, title, category, phone, website,
		emails, address, city, state, country, postal_code, latitude, longitude,
		review_count, review_rating, link, data) VALUES `)

	args := make([]any, 0, len(rows)*cols)

	for i := range rows {
		if i > 0 {
			q.WriteString(", ")
		}

		q.WriteString("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")

		r := &rows[i]
		args = append(args,
			w.jobID, w.jobName, r.PlaceKey, r.Title, r.Category, r.Phone, r.Website,
			r.Emails, r.Address, r.City, r.State, r.Country, r.PostalCode, r.Latitude, r.Longitude,
			r.ReviewCount, r.ReviewRating, r.Link, r.Data,
		)
	}

	q.WriteString(` ON DUPLICATE KEY UPDATE
		job_name = VALUES(job_name), title = VALUES(title), category = VALUES(category),
		phone = VALUES(phone), website = VALUES(website), emails = VALUES(emails),
		address = VALUES(address), city = VALUES(city), state = VALUES(state),
		country = VALUES(country), postal_code = VALUES(postal_code),
		latitude = VALUES(latitude), longitude = VALUES(longitude),
		review_count = VALUES(review_count), review_rating = VALUES(review_rating),
		link = VALUES(link), data = VALUES(data)`)

	// The scrape context is canceled as soon as the job ends, which is exactly
	// when the last batch is flushed, so saves use their own timeout instead.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), saveTimeout)
	defer cancel()

	if _, err := w.db.ExecContext(saveCtx, q.String(), args...); err != nil {
		return fmt.Errorf("insert places: %w", err)
	}

	return nil
}

// toRow maps a scraped place to a table row. The full place goes into data.
func toRow(e *gmaps.Entry) (placeRow, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return placeRow{}, fmt.Errorf("marshal place: %w", err)
	}

	// Lengths match the column sizes in createPlacesTable; the full values
	// are always kept in data.
	row := placeRow{
		PlaceKey:     placeKey(e),
		Title:        truncate(e.Title, 512),
		Category:     truncate(e.Category, 255),
		Phone:        truncate(e.Phone, 64),
		Website:      truncate(e.WebSite, 1024),
		Address:      truncate(e.Address, 1024),
		City:         truncate(e.CompleteAddress.City, 255),
		State:        truncate(e.CompleteAddress.State, 255),
		Country:      truncate(e.CompleteAddress.Country, 64),
		PostalCode:   truncate(e.CompleteAddress.PostalCode, 32),
		ReviewCount:  e.ReviewCount,
		ReviewRating: e.ReviewRating,
		Link:         e.Link,
		Data:         data,
	}

	if len(e.Emails) > 0 {
		row.Emails = sql.NullString{String: strings.Join(e.Emails, ","), Valid: true}
	}

	// 0,0 means the coordinates were not found.
	if e.Latitude != 0 || e.Longtitude != 0 {
		row.Latitude = sql.NullFloat64{Float64: e.Latitude, Valid: true}
		row.Longitude = sql.NullFloat64{Float64: e.Longtitude, Valid: true}
	}

	return row, nil
}

// placeKey picks the most stable identifier Google Maps gives for a place.
func placeKey(e *gmaps.Entry) string {
	for _, k := range []string{e.PlaceID, e.DataID, e.Cid, e.Link, e.Title + "|" + e.Address} {
		if strings.TrimSpace(k) != "" && k != "|" {
			return truncate(k, placeKeyMaxLen)
		}
	}

	return ""
}

func truncate(s string, maxLen int) string {
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}

	return string([]rune(s)[:maxLen])
}
