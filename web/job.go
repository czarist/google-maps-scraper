package web

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gosom/scrapemate"
)

var jobs []Job

const (
	StatusPending = "pending"
	StatusWorking = "working"
	StatusOK      = "ok"
	StatusFailed  = "failed"
)

type SelectParams struct {
	Status string
	Limit  int
	Offset int
}

// JobPage contains one page of jobs and the metadata needed to navigate it.
type JobPage struct {
	Jobs        []Job
	CurrentPage int
	TotalPages  int
	Total       int
	HasPrev     bool
	HasNext     bool
	PrevPage    int
	NextPage    int
	HasPages    bool
	// Running and Queued count jobs across all pages.
	Running int
	Queued  int
	// RenderedAt is the server time, so the page timer can correct for a
	// browser clock that is off.
	RenderedAt time.Time
}

// RenderedAtISO is RenderedAt in RFC 3339.
//
//nolint:gocritic // value receiver keeps the method callable from templates.
func (p JobPage) RenderedAtISO() string {
	return p.RenderedAt.UTC().Format(time.RFC3339)
}

// Active reports whether any job is running or waiting, so the list can
// refresh more often while there is something to watch.
//
//nolint:gocritic // value receiver keeps the method callable from templates.
func (p JobPage) Active() bool {
	return p.Running > 0 || p.Queued > 0
}

type JobRepository interface {
	Get(context.Context, string) (Job, error)
	Create(context.Context, *Job) error
	Delete(context.Context, string) error
	Select(context.Context, SelectParams) ([]Job, error)
	Update(context.Context, *Job) error
}

type Job struct {
	ID     string
	Name   string
	Date   time.Time
	Status string
	Data   JobData
}

func (j *Job) Validate() error {
	if j.ID == "" {
		return errors.New("missing id")
	}

	if j.Name == "" {
		return errors.New("missing name")
	}

	if j.Status == "" {
		return errors.New("missing status")
	}

	if j.Date.IsZero() {
		return errors.New("missing date")
	}

	if err := j.Data.Validate(); err != nil {
		return err
	}

	return nil
}

type JobData struct {
	Keywords     []string      `json:"keywords"`
	Lang         string        `json:"lang"`
	Zoom         int           `json:"zoom"`
	Lat          string        `json:"lat"`
	Lon          string        `json:"lon"`
	FastMode     bool          `json:"fast_mode"`
	Radius       int           `json:"radius"`
	Depth        int           `json:"depth"`
	Email        bool          `json:"email"`
	ExtraReviews bool          `json:"extra_reviews"`
	MaxTime      time.Duration `json:"max_time"`
	Proxies      []string      `json:"proxies"`
	Run          JobRun        `json:"run"`
}

// JobRun records what happened when a job ran. While a job is running,
// Places is filled from the live counter (see Service.StartProgress).
type JobRun struct {
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	Places     int       `json:"places"`
}

// Started reports whether the job has begun running.
//
//nolint:gocritic // value receiver keeps the method callable from templates.
func (r JobRun) Started() bool {
	return !r.StartedAt.IsZero()
}

// StartedAtISO is the start time in RFC 3339, for the live timer in the page.
//
//nolint:gocritic // value receiver keeps the method callable from templates.
func (r JobRun) StartedAtISO() string {
	return r.StartedAt.UTC().Format(time.RFC3339)
}

// Took is how long a finished run lasted, such as "3m 12s".
//
//nolint:gocritic // value receiver keeps the method callable from templates.
func (r JobRun) Took() string {
	if r.StartedAt.IsZero() || r.FinishedAt.IsZero() {
		return ""
	}

	return FormatDuration(r.FinishedAt.Sub(r.StartedAt))
}

// ElapsedSeconds is how long a running job has been running.
//
//nolint:gocritic // value receiver keeps the method callable from templates.
func (j Job) ElapsedSeconds() int {
	if !j.Data.Run.Started() {
		return 0
	}

	return max(0, int(time.Since(j.Data.Run.StartedAt)/time.Second))
}

// ElapsedLabel is ElapsedSeconds in a short readable form.
//
//nolint:gocritic // value receiver keeps the method callable from templates.
func (j Job) ElapsedLabel() string {
	return FormatDuration(time.Duration(j.ElapsedSeconds()) * time.Second)
}

// TimeUsedPercent is the share of the time limit already used, from 0 to 100.
// The server renders it so the bar does not jump back to zero on refresh.
//
//nolint:gocritic // value receiver keeps the method callable from templates.
func (j Job) TimeUsedPercent() string {
	limit := j.Data.MaxSeconds()
	if limit <= 0 {
		return "0"
	}

	return fmt.Sprintf("%.1f", min(100, float64(j.ElapsedSeconds())*100/float64(limit)))
}

// MaxSeconds is the time limit in whole seconds.
//
//nolint:gocritic // value receiver keeps the method callable from templates.
func (d JobData) MaxSeconds() int {
	return int(d.MaxTime / time.Second)
}

// MaxTimeLabel is the time limit in a short readable form, such as "10m".
//
//nolint:gocritic // value receiver keeps the method callable from templates.
func (d JobData) MaxTimeLabel() string {
	return FormatDuration(d.MaxTime)
}

// FormatDuration renders a duration as "45s", "3m 12s" or "1h 5m".
func FormatDuration(d time.Duration) string {
	d = d.Round(time.Second)

	h := int(d / time.Hour)
	m := int(d % time.Hour / time.Minute)
	s := int(d % time.Minute / time.Second)

	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	case m > 0 && s > 0:
		return fmt.Sprintf("%dm %ds", m, s)
	case m > 0:
		return fmt.Sprintf("%dm", m)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

func (d *JobData) Validate() error {
	if len(d.Keywords) == 0 {
		return errors.New("missing keywords")
	}

	if d.Lang == "" {
		return errors.New("missing lang")
	}

	if len(d.Lang) != 2 {
		return errors.New("invalid lang")
	}

	if d.Depth == 0 {
		return errors.New("missing depth")
	}

	if d.MaxTime == 0 {
		return errors.New("missing max time")
	}

	if d.FastMode && (d.Lat == "" || d.Lon == "") {
		return errors.New("missing geo coordinates")
	}

	// Reject unsupported proxies here: the scraper panics on them at runtime.
	for _, p := range d.Proxies {
		if _, err := scrapemate.NewProxy(p); err != nil {
			return fmt.Errorf("invalid proxy %q: %w (supported: http, https, socks5)", p, err)
		}
	}

	return nil
}
