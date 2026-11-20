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
