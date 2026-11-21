//nolint:testpackage // This test needs unexported hooks to avoid running a browser.
package webrunner

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/gosom/google-maps-scraper/runner"
	"github.com/gosom/google-maps-scraper/web"
	"github.com/gosom/scrapemate"
)

func TestScrapeJobMarksOKBeforeClosingMate(t *testing.T) {
	t.Parallel()

	repo := &memoryJobRepo{}
	svc := web.NewService(repo, t.TempDir())
	job := web.Job{
		ID:     "job-1",
		Name:   "coffee",
		Date:   time.Now().UTC(),
		Status: web.StatusPending,
		Data: web.JobData{
			Keywords: []string{"coffee"},
			Lang:     "en",
			Zoom:     15,
			Lat:      "37.7749",
			Lon:      "-122.4194",
			FastMode: true,
			Radius:   1000,
			Depth:    10,
			MaxTime:  time.Minute,
		},
	}

	if err := svc.Create(context.Background(), &job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	w := &webrunner{
		svc: svc,
		cfg: &runner.Config{DataFolder: t.TempDir(), Concurrency: 1},
		setupMate: func(_ context.Context, _ io.Writer, _ *web.Job) (mateRunner, error) {
			return fakeMate{
				onStart: func() { svc.AddProgress(job.ID, 3) },
				onClose: func() {
					got, err := svc.Get(context.Background(), job.ID)
					if err != nil {
						t.Fatalf("get job during close: %v", err)
					}

					if got.Status != web.StatusOK {
						t.Fatalf("status during close = %q, want %q", got.Status, web.StatusOK)
					}
				},
			}, nil
		},
	}

	if err := w.scrapeJob(context.Background(), &job); err != nil {
		t.Fatalf("scrape job: %v", err)
	}
}

type fakeMate struct {
	onStart func()
	onClose func()
}

func (m fakeMate) Start(context.Context, ...scrapemate.IJob) error {
	if m.onStart != nil {
		m.onStart()
	}

	return nil
}

func (m fakeMate) Close() error {
	if m.onClose != nil {
		m.onClose()
	}

	return nil
}

func TestScrapeJobWithNoPlacesFails(t *testing.T) {
	t.Parallel()

	repo := &memoryJobRepo{}
	svc := web.NewService(repo, t.TempDir())
	job := web.Job{
		ID:     "job-2",
		Name:   "nothing",
		Date:   time.Now().UTC(),
		Status: web.StatusPending,
		Data:   web.JobData{Keywords: []string{"x"}, Lang: "en", Zoom: 15, Depth: 1, MaxTime: time.Minute},
	}
	// Past the old attempt cap: a miss still goes back to the queue.
	job.Data.Run.Attempts = web.MaxJobAttempts - 1

	if err := svc.Create(context.Background(), &job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	w := &webrunner{
		svc: svc,
		cfg: &runner.Config{DataFolder: t.TempDir(), Concurrency: 1},
		setupMate: func(context.Context, io.Writer, *web.Job) (mateRunner, error) {
			return fakeMate{}, nil
		},
	}

	if err := w.scrapeJob(context.Background(), &job); err != nil {
		t.Fatalf("scrape job: %v", err)
	}

	got, err := svc.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}

	if got.Status != web.StatusPending || got.Data.Run.Attempts != web.MaxJobAttempts || got.Data.Run.Error == "" {
		t.Fatalf("status = %q, attempts = %d, error = %q; want queued again with a reason",
			got.Status, got.Data.Run.Attempts, got.Data.Run.Error)
	}

	if !got.Data.Run.RetryAfter.After(time.Now()) {
		t.Fatal("expected the job to wait its turn at the back of the queue")
	}
}

type memoryJobRepo struct {
	jobs map[string]web.Job
}

func (r *memoryJobRepo) Get(_ context.Context, id string) (web.Job, error) {
	return r.jobs[id], nil
}

func (r *memoryJobRepo) Create(_ context.Context, job *web.Job) error {
	if r.jobs == nil {
		r.jobs = make(map[string]web.Job)
	}

	r.jobs[job.ID] = *job

	return nil
}

func (r *memoryJobRepo) Delete(_ context.Context, id string) error {
	delete(r.jobs, id)
	return nil
}

func (r *memoryJobRepo) Select(_ context.Context, params web.SelectParams) ([]web.Job, error) {
	var jobs []web.Job

	for id := range r.jobs {
		job := r.jobs[id]

		if params.Status == "" || job.Status == params.Status {
			jobs = append(jobs, job)
		}
	}

	// Sort by created_at DESC (Date), or like the run queue with OldestFirst:
	// fewer attempts first, then oldest.
	before := func(a, b web.Job) bool {
		if params.OldestFirst {
			if a.Data.Run.Attempts != b.Data.Run.Attempts {
				return a.Data.Run.Attempts < b.Data.Run.Attempts
			}

			return a.Date.Before(b.Date)
		}

		return a.Date.After(b.Date) || (a.Date.Equal(b.Date) && a.ID > b.ID)
	}

	for i := 0; i < len(jobs); i++ {
		for j := i + 1; j < len(jobs); j++ {
			if before(jobs[j], jobs[i]) {
				jobs[i], jobs[j] = jobs[j], jobs[i]
			}
		}
	}

	if params.Offset > 0 {
		if params.Offset > len(jobs) {
			jobs = nil
		} else {
			jobs = jobs[params.Offset:]
		}
	}

	if params.Limit > 0 && len(jobs) > params.Limit {
		jobs = jobs[:params.Limit]
	}

	return jobs, nil
}

func (r *memoryJobRepo) Count(_ context.Context, params web.SelectParams) (int, error) {
	count := 0

	for id := range r.jobs {
		job := r.jobs[id]
		if params.Status == "" || job.Status == params.Status {
			count++
		}
	}

	return count, nil
}

func (r *memoryJobRepo) Update(_ context.Context, job *web.Job) error {
	r.jobs[job.ID] = *job
	return nil
}
