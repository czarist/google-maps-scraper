//nolint:testpackage // drives the unexported job loop with a fake browser.
package webrunner

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosom/google-maps-scraper/runner"
	"github.com/gosom/google-maps-scraper/web"
)

// lockedRepo makes memoryJobRepo safe for the parallel workers.
type lockedRepo struct {
	mu sync.Mutex
	memoryJobRepo
}

func (r *lockedRepo) Get(ctx context.Context, id string) (web.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.memoryJobRepo.Get(ctx, id)
}

func (r *lockedRepo) Create(ctx context.Context, job *web.Job) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.memoryJobRepo.Create(ctx, job)
}

func (r *lockedRepo) Delete(ctx context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.memoryJobRepo.Delete(ctx, id)
}

func (r *lockedRepo) Select(ctx context.Context, p web.SelectParams) ([]web.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.memoryJobRepo.Select(ctx, p)
}

func (r *lockedRepo) Update(ctx context.Context, job *web.Job) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.memoryJobRepo.Update(ctx, job)
}

func TestWorkRunsJobsInParallelUpToTheLimit(t *testing.T) {
	repo := &lockedRepo{}
	svc := web.NewService(repo, t.TempDir())

	for i := range 3 {
		job := web.Job{
			ID:     fmt.Sprintf("job-%d", i),
			Name:   "coffee",
			Date:   time.Now().UTC().Add(time.Duration(i) * time.Second),
			Status: web.StatusPending,
			Data:   web.JobData{Keywords: []string{"coffee"}, Lang: "en", Depth: 1, MaxTime: time.Minute},
		}

		if err := svc.Create(context.Background(), &job); err != nil {
			t.Fatal(err)
		}
	}

	var (
		running, peak, started atomic.Int32
		release                = make(chan struct{})
	)

	cfg := &runner.Config{DataFolder: t.TempDir(), Concurrency: 1, WebJobWorkers: 2}
	w := &webrunner{
		svc:    svc,
		cfg:    cfg,
		jobCfg: cfg,
		setupMate: func(context.Context, io.Writer, *web.Job) (mateRunner, error) {
			return fakeMate{onStart: func() {
				started.Add(1)

				n := running.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}

				<-release
				running.Add(-1)
			}}, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = w.work(ctx) }()

	waitFor(t, func() bool { return started.Load() == 2 })

	// The limit is 2: the third job waits while two run.
	time.Sleep(1500 * time.Millisecond)

	if got := started.Load(); got != 2 {
		t.Fatalf("started %d jobs with 2 workers busy, want 2", got)
	}

	close(release)

	waitFor(t, func() bool { return started.Load() == 3 })

	if p := peak.Load(); p != 2 {
		t.Fatalf("peak parallel jobs = %d, want 2", p)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("condition not met in time")
}

func TestFailedJobIsQueuedAgainAfterTheOthers(t *testing.T) {
	repo := &memoryJobRepo{}
	svc := web.NewService(repo, t.TempDir())
	job := web.Job{
		ID: "job-x", Name: "nothing", Date: time.Now().UTC(), Status: web.StatusPending,
		Data: web.JobData{Keywords: []string{"x"}, Lang: "en", Depth: 1, MaxTime: time.Minute},
	}

	if err := svc.Create(context.Background(), &job); err != nil {
		t.Fatal(err)
	}

	cfg := &runner.Config{DataFolder: t.TempDir(), Concurrency: 1}
	w := &webrunner{svc: svc, cfg: cfg, jobCfg: cfg, setupMate: func(context.Context, io.Writer, *web.Job) (mateRunner, error) {
		return fakeMate{}, nil
	}}

	// No places: the run fails, and the job goes back to the queue for a later try.
	_ = w.scrapeJob(context.Background(), &job)

	got, _ := svc.Get(context.Background(), job.ID)
	if got.Status != web.StatusPending || got.Data.Run.Attempts != 1 || !got.Data.Run.RetryAfter.After(time.Now()) || !got.Data.Run.Retrying() {
		t.Fatalf("after 1 failure: status %q, run %+v; want pending for a later retry", got.Status, got.Data.Run)
	}
}

func TestRequeueFailedPutsJobsBack(t *testing.T) {
	repo := &memoryJobRepo{}
	svc := web.NewService(repo, t.TempDir())
	ctx := context.Background()

	failed := web.Job{
		ID: "job-f", Name: "f", Date: time.Now().UTC(), Status: web.StatusFailed,
		Data: web.JobData{
			Keywords: []string{"x"},
			Run:      web.JobRun{Attempts: web.MaxJobAttempts, Error: "no working proxies"},
		},
	}
	empty := web.Job{
		ID: "job-e", Name: "e", Date: time.Now().UTC(), Status: web.StatusFailed,
		Data: web.JobData{Run: web.JobRun{Attempts: 1, Error: "no keywords"}},
	}

	for _, job := range []web.Job{failed, empty} {
		if err := svc.Create(ctx, &job); err != nil {
			t.Fatal(err)
		}
	}

	n, err := svc.RequeueFailedRuns(ctx)
	if err != nil || n != 1 {
		t.Fatalf("requeued %d, %v; want 1", n, err)
	}

	got, err := svc.Get(ctx, failed.ID)
	if err != nil {
		t.Fatal(err)
	}

	if got.Status != web.StatusPending || got.Data.Run.Attempts != web.MaxJobAttempts {
		t.Fatalf("failed job after requeue: status %q attempts %d", got.Status, got.Data.Run.Attempts)
	}

	kept, err := svc.Get(ctx, empty.ID)
	if err != nil {
		t.Fatal(err)
	}

	if kept.Status != web.StatusFailed {
		t.Fatalf("job with no search = %q, want failed", kept.Status)
	}
}

func TestRestartRequeuesJobsLeftRunning(t *testing.T) {
	repo := &memoryJobRepo{}
	svc := web.NewService(repo, t.TempDir())
	job := web.Job{ID: "job-r", Name: "cut", Date: time.Now().UTC(), Status: web.StatusWorking}
	job.Data.Run.Attempts = 1

	if err := svc.Create(context.Background(), &job); err != nil {
		t.Fatal(err)
	}

	n, err := svc.RequeueInterrupted(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("requeued %d, %v", n, err)
	}

	got, _ := svc.Get(context.Background(), job.ID)
	if got.Status != web.StatusPending || got.Data.Run.Attempts != 0 || got.Data.Run.Error != web.ErrMsgInterrupted {
		t.Fatalf("got status %q run %+v", got.Status, got.Data.Run)
	}
}
