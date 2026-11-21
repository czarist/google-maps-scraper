package webrunner

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gosom/google-maps-scraper/deduper"
	"github.com/gosom/google-maps-scraper/exiter"
	"github.com/gosom/google-maps-scraper/mysqlstore"
	"github.com/gosom/google-maps-scraper/optout"
	"github.com/gosom/google-maps-scraper/runner"
	"github.com/gosom/google-maps-scraper/tlmt"
	"github.com/gosom/google-maps-scraper/web"
	"github.com/gosom/google-maps-scraper/web/sqlite"
	"github.com/gosom/scrapemate"
	"github.com/gosom/scrapemate/adapters/fetchers/jshttp"
	"github.com/gosom/scrapemate/adapters/writers/csvwriter"
	"github.com/gosom/scrapemate/scrapemateapp"
	"golang.org/x/sync/errgroup"
)

type webrunner struct {
	srv *web.Server
	svc *web.Service
	cfg *runner.Config
	// jobCfg is cfg with the per-job browser concurrency for parallel jobs.
	jobCfg    *runner.Config
	mysqlDB   *sql.DB
	optOut    *optout.Checker
	setupMate func(context.Context, io.Writer, *web.Job) (mateRunner, error)
}

type mateRunner interface {
	Start(context.Context, ...scrapemate.IJob) error
	Close() error
}

func New(cfg *runner.Config) (runner.Runner, error) {
	if cfg.DataFolder == "" {
		return nil, fmt.Errorf("data folder is required")
	}

	if err := os.MkdirAll(cfg.DataFolder, os.ModePerm); err != nil {
		return nil, err
	}

	const dbfname = "jobs.db"

	dbpath := filepath.Join(cfg.DataFolder, dbfname)

	repo, err := sqlite.New(dbpath)
	if err != nil {
		return nil, err
	}

	svc := web.NewService(repo, cfg.DataFolder)

	optOut, err := openOptOut(svc)
	if err != nil {
		return nil, err
	}

	srv, err := web.New(svc, cfg.Addr)
	if err != nil {
		return nil, err
	}

	var mysqlDB *sql.DB

	if cfg.MySQLDSN != "" {
		mysqlDB, err = mysqlstore.Open(context.Background(), cfg.MySQLDSN)
		if err != nil {
			return nil, err
		}

		log.Printf("saving web job results to MySQL as well as CSV")
	}

	jobCfg := *cfg
	jobCfg.Concurrency = perJobConcurrency(cfg, jobWorkers(cfg))

	ans := webrunner{
		srv:       srv,
		svc:       svc,
		cfg:       cfg,
		jobCfg:    &jobCfg,
		mysqlDB:   mysqlDB,
		optOut:    optOut,
		setupMate: defaultSetupMate(&jobCfg, mysqlDB, svc),
	}

	return &ans, nil
}

func (w *webrunner) Run(ctx context.Context) error {
	egroup, ctx := errgroup.WithContext(ctx)

	egroup.Go(func() error {
		return w.work(ctx)
	})

	egroup.Go(func() error {
		return w.srv.Start(ctx)
	})

	return egroup.Wait()
}

func (w *webrunner) Close(context.Context) error {
	if w.optOut != nil {
		_ = w.optOut.Close()
	}

	if w.mysqlDB != nil {
		return w.mysqlDB.Close()
	}

	return nil
}

// openOptOut connects the unsubscribe database. Without it every email send is
// refused, so a missing configuration is logged but does not stop the web UI.
func openOptOut(svc *web.Service) (*optout.Checker, error) {
	base := optout.UnsubscribeBaseFromEnv()
	if _, err := web.UnsubscribeURL(base, web.ServiceTrentin); err != nil {
		return nil, fmt.Errorf("%s: %w", optout.EnvUnsubscribeBase, err)
	}

	cfg, ok := optout.ConfigFromEnv()
	if !ok {
		log.Printf("unsubscribe database is not configured (%s, %s, %s): email sending is disabled",
			optout.EnvHost, optout.EnvUser, optout.EnvDatabase)
		svc.SetOptOut(nil, base)

		return nil, nil //nolint:nilnil // no unsubscribe database is a valid setup: sends are refused
	}

	checker, err := optout.Open(&cfg)
	if err != nil {
		return nil, err
	}

	if err := checker.Ping(context.Background()); err != nil {
		log.Printf("unsubscribe database is not reachable yet, sends will wait for it: %v", err)
	}

	svc.SetOptOut(checker, base)

	return checker, nil
}

// jobWorkers is how many jobs run at the same time. Each job drives its own
// headless browser, so the default is half of the CPU threads (4 on an i7 with
// 8 threads). WEB_JOB_WORKERS or -web-workers overrides it.
func jobWorkers(cfg *runner.Config) int {
	if cfg.WebJobWorkers > 0 {
		return cfg.WebJobWorkers
	}

	return max(runtime.NumCPU()/2, 1)
}

// perJobConcurrency splits the CPU between the parallel jobs, so N jobs together
// use about as many browser pages as there are CPU threads. An explicit -c wins.
func perJobConcurrency(cfg *runner.Config, workers int) int {
	if cfg.ConcurrencySet {
		return cfg.Concurrency
	}

	return max(runtime.NumCPU()/max(workers, 1), 1)
}

// work runs queued jobs in parallel, up to jobWorkers at a time, oldest first.
// A job that finishes frees its slot for the next one right away.
func (w *webrunner) work(ctx context.Context) error {
	workers := jobWorkers(w.cfg)
	w.svc.SetWorkerSlots(workers)

	perJob := w.cfg.Concurrency
	if w.jobCfg != nil {
		perJob = w.jobCfg.Concurrency
	}

	log.Printf("web runner: up to %d jobs in parallel, %d browser pages each", workers, perJob)

	// Nothing can be running yet: jobs marked running were cut by a restart.
	if n, err := w.svc.RequeueInterrupted(ctx); err != nil {
		log.Printf("requeue interrupted jobs: %v", err)
	} else if n > 0 {
		log.Printf("requeued %d jobs interrupted by a restart", n)
	}

	if n, err := w.svc.RequeueFailedRuns(ctx); err != nil {
		log.Printf("requeue failed jobs: %v", err)
	} else if n > 0 {
		log.Printf("requeued %d failed jobs", n)
	}

	var (
		mu       sync.Mutex
		inflight = make(map[string]struct{}, workers)
		wg       sync.WaitGroup
		done     = make(chan struct{}, workers)
	)

	defer wg.Wait()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	lane := workers

	for {
		// A 429 is account-wide. Starting another browser now would dial
		// straight back into the same window.
		if wait := jshttp.UpstreamRateLimitWait(); wait > 0 {
			if wait >= time.Second {
				log.Printf("bandwidth limit: waiting %s, then continuing", wait.Round(time.Second))
			}

			timer := time.NewTimer(wait)

			select {
			case <-ctx.Done():
				timer.Stop()

				return nil
			case <-timer.C:
			case <-done:
				timer.Stop()
			}

			continue
		}

		slots := jshttp.UpstreamLaneCap(workers)
		if slots != lane {
			log.Printf("proxy bandwidth limit: running %d at a time", slots)
			lane = slots
		}

		mu.Lock()
		free := slots - len(inflight)
		busy := len(inflight)
		mu.Unlock()

		if free > 0 {
			// Ask for extra rows: jobs just started may still read as pending,
			// and a job waiting out RetryAfter must not hide a ready one.
			jobs, err := w.svc.SelectPendingN(ctx, max(free+busy+pendingLookahead, pendingLookahead))
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}

				return err
			}

			for i := range jobs {
				if free == 0 {
					break
				}

				job := jobs[i]

				// A failed job waits a little before its next try.
				if job.Data.Run.RetryAfter.After(time.Now()) {
					continue
				}

				mu.Lock()
				if _, running := inflight[job.ID]; running {
					mu.Unlock()

					continue
				}

				inflight[job.ID] = struct{}{}
				mu.Unlock()

				free--

				wg.Add(1)

				go func() {
					defer func() {
						mu.Lock()
						delete(inflight, job.ID)
						mu.Unlock()

						wg.Done()

						select {
						case done <- struct{}{}:
						default:
						}
					}()

					w.runOne(ctx, &job)
				}()
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-done:
		}
	}
}

// runOne runs a job and reports how it went.
func (w *webrunner) runOne(ctx context.Context, job *web.Job) {
	t0 := time.Now().UTC()

	err := w.scrapeJob(ctx, job)

	params := map[string]any{
		"job_count": len(job.Data.Keywords),
		"duration":  time.Now().UTC().Sub(t0).String(),
	}

	if err != nil {
		params["error"] = err.Error()

		log.Printf("error scraping job %s: %v", job.ID, err)
	} else if job.Status == web.StatusOK {
		log.Printf("job %s scraped successfully", job.ID)
	}

	_ = runner.Telemetry().Send(ctx, tlmt.NewEvent("web_runner", params))
}

func (w *webrunner) scrapeJob(ctx context.Context, job *web.Job) error {
	job.Status = web.StatusWorking
	job.Data.Run = web.JobRun{StartedAt: time.Now().UTC(), Attempts: job.Data.Run.Attempts + 1}

	w.svc.StartProgress(job.ID)

	err := w.svc.Update(ctx, job)
	if err != nil {
		w.svc.StopProgress(job.ID)

		return err
	}

	if len(job.Data.Keywords) == 0 {
		return w.finishJob(ctx, job, web.StatusFailed, nil)
	}

	// Never reach Google without a working proxy when proxies are required,
	// and never with proxies that are dead (expired plan).
	proxies := job.Data.Proxies
	if len(w.cfg.Proxies) > 0 {
		proxies = w.cfg.Proxies
	} else if w.svc.RequiresProxies() {
		// The list saved on the job is from the day it was created. ProxyScrape
		// rotates the plan username; the old one still answers 429 while the
		// current plan has bandwidth. Refresh talks to the account API only.
		fresh, refreshErr := w.svc.AccountProxies(ctx)
		if refreshErr != nil {
			log.Printf("job %s proxy refresh: %v", job.ID, refreshErr)
		} else if len(fresh) > 0 {
			proxies = fresh
			job.Data.Proxies = fresh

			if uerr := w.svc.Update(ctx, job); uerr != nil {
				log.Printf("job %s proxy refresh save: %v", job.ID, uerr)
			}
		}
	}

	outpath := filepath.Join(w.cfg.DataFolder, job.ID+".csv")

	outfile, err := os.Create(outpath)
	if err != nil {
		return w.finishJob(ctx, job, web.StatusFailed, err)
	}

	defer func() {
		_ = outfile.Close()
	}()

	// A bandwidth 429 pauses this same run for the gateway's 120s and then
	// continues. The job is not failed and does not go to the back of the queue.
	for {
		healthy, err := w.svc.JobProxies(ctx, proxies)
		if err != nil && !errors.Is(err, web.ErrProxyCooling) && len(w.cfg.Proxies) == 0 {
			// This job's own proxy list (frozen since it was created, possibly
			// several retries ago) is exhausted. Pull a fresh one from ProxyScrape
			// before giving up - the account's pool may have changed since, and
			// without this a job would just keep replaying the same dead list.
			if fresh, refreshErr := w.svc.RefreshProxies(ctx); refreshErr == nil {
				if healthy, err = w.svc.JobProxies(ctx, fresh); err == nil {
					proxies = fresh
					job.Data.Proxies = fresh
				}
			}
		}

		if errors.Is(err, web.ErrProxyCooling) {
			if werr := pauseForBandwidth(ctx, job.ID); werr != nil {
				return w.finishJob(ctx, job, web.StatusFailed, werr)
			}

			continue
		}

		if err != nil {
			log.Printf("job %s not started: %v", job.ID, err)

			return w.finishJob(ctx, job, web.StatusFailed, err)
		}

		if _, seekErr := outfile.Seek(0, io.SeekStart); seekErr == nil {
			_ = outfile.Truncate(0)
		}

		mate, runErr := w.runJobBrowser(ctx, job, outfile, healthy)
		if errors.Is(runErr, errBandwidthPause) {
			closeMate(mate)

			if werr := pauseForBandwidth(ctx, job.ID); werr != nil {
				return w.finishJob(ctx, job, web.StatusFailed, werr)
			}

			continue
		}

		var finishErr error
		if runErr != nil {
			finishErr = w.finishJob(ctx, job, web.StatusFailed, runErr)
		} else {
			finishErr = w.finishJob(ctx, job, web.StatusOK, nil)
		}

		closeMate(mate)

		return finishErr
	}
}

// errBandwidthPause means the run stopped because the proxy account is in
// its 120s bandwidth window. The caller waits and continues the same job.
var errBandwidthPause = errors.New("bandwidth limit")

// pauseForBandwidth blocks until the gateway's Retry-After has elapsed.
func pauseForBandwidth(ctx context.Context, jobID string) error {
	wait := jshttp.UpstreamRateLimitWait()
	if wait < time.Second {
		wait = jshttp.AccountBandwidthPause
	}

	log.Printf("job %s bandwidth limit: waiting %s, then continuing", jobID, wait.Round(time.Second))

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// runJobBrowser runs one pass of the job. It returns errBandwidthPause when
// the account 429'd before any place was found, so the caller can wait and
// try this same job again. The caller closes mate after the job status is saved.
func (w *webrunner) runJobBrowser(ctx context.Context, job *web.Job, outfile io.Writer, healthy []string) (mateRunner, error) {
	mateJob := *job
	if len(w.cfg.Proxies) == 0 && len(healthy) > 0 {
		mateJob.Data.Proxies = healthy
	}

	setupMate := w.setupMate
	if setupMate == nil {
		setupMate = defaultSetupMate(w.cfg, w.mysqlDB, w.svc)
	}

	mate, err := setupMate(ctx, outfile, &mateJob)
	if err != nil {
		return nil, err
	}

	var coords string
	if hasCoordinates(job.Data.Lat, job.Data.Lon) {
		coords = job.Data.Lat + "," + job.Data.Lon
	}

	radius := float64(job.Data.Radius)
	if radius <= 0 {
		radius = runner.DefaultRadiusMeters
	}

	dedup := deduper.New()
	exitMonitor := exiter.New()

	seedJobs, err := runner.CreateSeedJobs(
		job.Data.FastMode,
		job.Data.Lang,
		strings.NewReader(strings.Join(job.Data.Keywords, "\n")),
		job.Data.Depth,
		job.Data.Email,
		coords,
		job.Data.Zoom,
		radius,
		dedup,
		exitMonitor,
		w.cfg.ExtraReviews || job.Data.ExtraReviews,
		runner.WithMaxDistance(radius),
	)
	if err != nil {
		return mate, err
	}

	if len(seedJobs) == 0 {
		return mate, nil
	}

	exitMonitor.SetSeedCount(len(seedJobs))

	allowedSeconds := max(60, len(seedJobs)*10*job.Data.Depth/50+120)

	if job.Data.MaxTime > 0 {
		if job.Data.MaxTime.Seconds() < 180 {
			allowedSeconds = 180
		} else {
			allowedSeconds = int(job.Data.MaxTime.Seconds())
		}
	}

	log.Printf("running job %s with %d seed jobs and %d allowed seconds", job.ID, len(seedJobs), allowedSeconds)

	mateCtx, cancel := context.WithTimeout(ctx, time.Duration(allowedSeconds)*time.Second)
	defer cancel()

	exitMonitor.SetCancelFunc(cancel)

	go exitMonitor.Run(mateCtx)

	// The browser normally stops at the time limit. If it hangs past it, the
	// job is ended anyway so its slot goes to the next one.
	started := make(chan error, 1)

	go func() { started <- mate.Start(mateCtx, seedJobs...) }()

	hardLimit := time.NewTimer(time.Duration(allowedSeconds)*time.Second + stuckGrace)
	defer hardLimit.Stop()

	select {
	case err = <-started:
	case <-hardLimit.C:
		cancel()

		log.Printf("job %s went past its time limit; stopping it", job.ID)

		return mate, errors.New(web.ErrMsgStuck)
	case <-ctx.Done():
		cancel()

		return mate, ctx.Err()
	}

	cancel()

	if places, _ := w.svc.Progress(job.ID); places == 0 && jshttp.UpstreamRateLimitWait() > 0 {
		return mate, errBandwidthPause
	}

	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return mate, err
	}

	return mate, nil
}

// pendingLookahead is how many queued jobs are inspected per tick. The oldest
// row can be sitting on RetryAfter while a later one is ready.
const pendingLookahead = 50

// stuckGrace is how long past its time limit a run may take to stop by itself.
const stuckGrace = 2 * time.Minute

// mateCloseTimeout bounds how long closing a job's browser may take. The JS
// fetcher bounds its own close (about 20s) and kills the Playwright driver and
// its browsers when they hang, so this is only a last resort for the worker.
const mateCloseTimeout = 30 * time.Second

// closeMate closes the job's app, fetcher and browsers, also when the run is
// still stuck in Start, without letting a hung close block the worker.
func closeMate(m mateRunner) {
	if m == nil {
		return
	}

	done := make(chan struct{})

	go func() {
		_ = m.Close()

		close(done)
	}()

	select {
	case <-done:
	case <-time.After(mateCloseTimeout):
		log.Printf("closing a job browser took longer than %s; moving on", mateCloseTimeout)
	}
}

// finishJob records how the run ended (status, end time and places found)
// and returns runErr, or the update error when there was no run error.
func (w *webrunner) finishJob(ctx context.Context, job *web.Job, status string, runErr error) error {
	job.Status = status
	job.Data.Run.FinishedAt = time.Now().UTC()
	job.Data.Run.Places = w.svc.StopProgress(job.ID)

	if job.Data.Run.Places > 0 {
		jshttp.NoteUpstreamRecovered()
	}

	switch {
	case runErr != nil:
		job.Data.Run.Error = runErr.Error()
	case status == web.StatusOK && job.Data.Run.Places == 0:
		// Seed failures are logged by scrapemate but do not fail the run. A Google
		// Maps search with no places at all almost always means the search failed.
		job.Status = web.StatusFailed
		job.Data.Run.Error = noPlacesError
	}

	// A failed run goes to the end of the queue for another try, unless the
	// server is shutting down (then the restart requeues it).
	if job.Status == web.StatusFailed && ctx.Err() == nil && web.QueueRetry(job, time.Now().UTC()) {
		log.Printf("job %s failed (attempt %d), back of the queue: %s",
			job.ID, job.Data.Run.Attempts, job.Data.Run.Error)
	}

	// The job context may already be canceled here; the result still needs saving.
	err := w.svc.Update(context.WithoutCancel(ctx), job)
	if err != nil {
		log.Printf("failed to update job status: %v", err)
	}

	if runErr != nil {
		return runErr
	}

	return err
}

// progressWriter counts places as they are found, for the live job list.
type progressWriter struct {
	add func(int)
}

func (p *progressWriter) Run(_ context.Context, in <-chan scrapemate.Result) error {
	for result := range in {
		n := 1

		if v := reflect.ValueOf(result.Data); v.Kind() == reflect.Slice {
			n = v.Len()
		}

		p.add(n)
	}

	return nil
}

func defaultSetupMate(cfg *runner.Config, mysqlDB *sql.DB, svc *web.Service) func(context.Context, io.Writer, *web.Job) (mateRunner, error) {
	return func(_ context.Context, writer io.Writer, job *web.Job) (mateRunner, error) {
		conc := cfg.Concurrency
		if conc < 1 {
			conc = 1
		}

		// One page while the account is on a bandwidth limit. A second page
		// is another full Maps load on the same bucket.
		if jshttp.UpstreamBandwidthLimited() {
			conc = 1
		}

		opts := []func(*scrapemateapp.Config) error{
			scrapemateapp.WithConcurrency(conc),
			scrapemateapp.WithExitOnInactivity(time.Minute * 3),
		}

		if !job.Data.FastMode {
			opts = append(opts,
				scrapemateapp.WithJS(scrapemateapp.DisableImages()),
			)
		} else {
			opts = append(opts,
				scrapemateapp.WithStealth("firefox"),
			)
		}

		opts = runner.AppendBrowserCapacityOptions(opts, cfg)

		hasProxy := false

		if len(cfg.Proxies) > 0 {
			opts = append(opts, scrapemateapp.WithProxies(cfg.Proxies))
			hasProxy = true
		} else if len(job.Data.Proxies) > 0 {
			opts = append(opts,
				scrapemateapp.WithProxies(job.Data.Proxies),
			)
			hasProxy = true
		}

		if !cfg.DisablePageReuse {
			opts = append(opts,
				scrapemateapp.WithPageReuseLimit(2),
				scrapemateapp.WithBrowserReuseLimit(200),
			)
		}

		log.Printf("job %s has proxy: %v", job.ID, hasProxy)

		resultWriters := []scrapemate.ResultWriter{
			csvwriter.NewCsvWriter(csv.NewWriter(writer)),
		}

		if mysqlDB != nil {
			resultWriters = append(resultWriters, mysqlstore.NewResultWriter(mysqlDB, job.ID, job.Name))
		}

		if svc != nil {
			jobID := job.ID
			resultWriters = append(resultWriters, &progressWriter{add: func(n int) { svc.AddProgress(jobID, n) }})
		}

		writers := []scrapemate.ResultWriter{newTeeWriter(resultWriters...)}

		matecfg, err := scrapemateapp.NewConfig(
			writers,
			opts...,
		)
		if err != nil {
			return nil, err
		}

		return scrapemateapp.NewScrapeMateApp(matecfg)
	}
}
