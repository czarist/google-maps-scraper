package jshttp

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/mxschmitt/playwright-go"

	"github.com/gosom/scrapemate"
	playwrightadapter "github.com/gosom/scrapemate/adapters/browsers/playwright"
)

var _ scrapemate.HTTPFetcher = (*jsFetch)(nil)

// closeTimeout is the maximum time allowed for a Playwright page.Close() before
// the caller abandons it. A wedged Playwright driver (e.g. EPIPE after a browser
// crash) can make Close block forever; abandoning it frees the worker goroutine.
// The abandoned page lives until its browser is recycled or the fetcher is
// closed, and jsFetch.Close kills the driver and its browsers if they hang, so
// nothing outlives the fetcher.
const closeTimeout = 5 * time.Second

const (
	// browsersCloseTimeout bounds closing all browsers when the fetcher closes.
	browsersCloseTimeout = 15 * time.Second
	// driverStopTimeout bounds stopping the Playwright driver process.
	driverStopTimeout = 5 * time.Second
)

// errFetcherClosed is returned to fetches made after the fetcher was closed.
var errFetcherClosed = errors.New("jshttp: fetcher closed")

// closeWithTimeout runs closer in a background goroutine and returns when it
// completes or when d elapses, whichever comes first. It reports whether closer
// finished in time. On timeout the goroutine is abandoned (it finishes once the
// driver recovers or is killed). Without this bound, a hung Close propagates
// through the worker WaitGroup and prevents the caller from being unblocked
// even by context cancellation.
func closeWithTimeout(closer func() error, d time.Duration) bool {
	done := make(chan struct{})

	go func() {
		_ = closer()

		close(done)
	}()

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

type JSFetcherOptions struct {
	Headless           bool
	DisableImages      bool
	Rotator            scrapemate.ProxyRotator
	PoolSize           int
	MaxPagesPerBrowser int
	PageReuseLimit     int
	BrowserReuseLimit  int
	UserAgent          string
}

func New(params JSFetcherOptions) (scrapemate.HTTPFetcher, error) {
	opts := []*playwright.RunOptions{
		{
			Browsers: []string{"chromium"},
			Verbose:  true,
		},
	}

	if err := playwright.Install(opts...); err != nil {
		return nil, err
	}

	pw, err := playwright.Run()
	if err != nil {
		return nil, err
	}

	var pool *ProxyPool

	if params.Rotator != nil {
		proxies := params.Rotator.Proxies()

		if len(proxies) > 0 {
			pool, err = NewProxyPool(proxies)
			if err != nil {
				stopDriver(pw)

				return nil, err
			}
		}
	}

	maxPagesPerBrowser := params.MaxPagesPerBrowser
	if maxPagesPerBrowser < 1 {
		maxPagesPerBrowser = 1
	}

	ans := jsFetch{
		pw:                 pw,
		headless:           params.Headless,
		disableImages:      params.DisableImages,
		pageReuseLimit:     params.PageReuseLimit,
		browserReuseLimit:  params.BrowserReuseLimit,
		ua:                 params.UserAgent,
		proxyPool:          pool,
		rotator:            params.Rotator,
		maxPagesPerBrowser: maxPagesPerBrowser,
		closed:             make(chan struct{}),
	}

	if maxPagesPerBrowser > 1 {
		ans.pageSlots, err = newPageSlotPool(pageSlotPoolConfig{
			poolSize:           params.PoolSize,
			maxPagesPerBrowser: maxPagesPerBrowser,
			factory: playwrightSlotFactory{
				pw:            pw,
				headless:      params.Headless,
				disableImages: params.DisableImages,
				proxyPool:     pool,
				ua:            params.UserAgent,
			},
		})
		if err != nil {
			_ = ans.Close()

			return nil, err
		}

		return &ans, nil
	}

	ans.slots = make(chan *sessionSlot, params.PoolSize)

	sessionFactory := &playwrightRuntimeFactory{
		pw:            pw,
		headless:      params.Headless,
		disableImages: params.DisableImages,
		proxyPool:     pool,
		ua:            params.UserAgent,
	}
	ans.factory = sessionFactory

	for range params.PoolSize {
		ans.slots <- newSessionSlot(sessionFactory)
	}

	return &ans, nil
}

type jsFetch struct {
	pw                *playwright.Playwright
	headless          bool
	disableImages     bool
	pageReuseLimit    int
	browserReuseLimit int
	ua                string
	proxyPool         *ProxyPool
	factory           runtimeFactory
	slots             chan *sessionSlot
	pageSlots         *pageSlotPool

	rotator            scrapemate.ProxyRotator
	maxPagesPerBrowser int

	directMu sync.Mutex
	direct   *browser

	// closed is closed when Close starts; fetches still running then give
	// their slots back to nobody instead of the (drained) pool.
	closed    chan struct{}
	closeOnce sync.Once
}

func (o *jsFetch) getSlot(ctx context.Context) (*sessionSlot, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-o.closed:
		return nil, errFetcherClosed
	case slot := <-o.slots:
		return slot, nil
	}
}

func (o *jsFetch) putSlot(ctx context.Context, slot *sessionSlot) {
	// Checked first so a closed fetcher never gets a slot back, even when the
	// pool has room. The slots channel is never closed, so sending is safe.
	select {
	case <-o.closed:
		_ = slot.close()

		return
	default:
	}

	select {
	case <-ctx.Done():
		_ = slot.close()
	case <-o.closed:
		_ = slot.close()
	case o.slots <- slot:
	}
}

// Close closes every browser and stops the Playwright driver. It never blocks
// for more than browsersCloseTimeout+driverStopTimeout: if the browsers or the
// driver hang, the driver process and all browsers it launched are killed, so
// no process outlives the fetcher. It is safe to call more than once and
// concurrently with running fetches.
func (o *jsFetch) Close() error {
	o.closeOnce.Do(o.shutdown)

	return nil
}

func (o *jsFetch) shutdown() {
	if o.closed != nil {
		close(o.closed)
	}

	closeWithTimeout(func() error {
		o.closeBrowsers()

		return nil
	}, browsersCloseTimeout)

	if o.pw != nil {
		stopDriver(o.pw)
	}
}

// closeBrowsers closes the browsers of every pool. Slots held by running
// fetches are closed by putSlot when those fetches finish.
func (o *jsFetch) closeBrowsers() {
	if o.pageSlots != nil {
		o.pageSlots.close()
	}

	for o.slots != nil {
		select {
		case slot := <-o.slots:
			_ = slot.close()

			continue
		default:
		}

		break
	}

	o.directMu.Lock()

	if o.direct != nil {
		o.direct.Close()
		o.direct = nil
	}

	o.directMu.Unlock()
}

// stopDriver stops the Playwright driver and makes sure neither it nor any
// browser it launched keeps running. Without the kill, a driver whose Stop hangs
// stays alive for as long as the process runs, together with its browsers.
func stopDriver(pw *playwright.Playwright) {
	// Taken before stopping: once the driver exits, its browsers are re-parented
	// and can no longer be found from its PID.
	procs := processTree(pw.Pid())

	if closeWithTimeout(pw.Stop, driverStopTimeout) {
		// The driver exited and was reaped; only browsers it left behind remain.
		if len(procs) > 1 {
			procs = procs[1:]
		} else {
			procs = nil
		}
	}

	if n := killProcesses(procs); n > 0 {
		log.Printf("jshttp: killed %d leftover playwright process(es) of driver pid %d", n, pw.Pid())
	}
}

// Fetch fetches the url specicied by the job and returns the response
func (o *jsFetch) Fetch(ctx context.Context, job scrapemate.IJob) scrapemate.Response {
	if scrapemate.SkipsProxy(job) {
		return o.fetchWithoutProxy(ctx, job)
	}

	if o.maxPagesPerBrowser > 1 {
		return o.fetchWithPageSlot(ctx, job)
	}

	slot, err := o.getSlot(ctx)
	if err != nil {
		return scrapemate.Response{
			Error: err,
		}
	}

	defer o.putSlot(ctx, slot)

	p, err := slot.acquirePage(ctx)
	if err != nil {
		return scrapemate.Response{
			Error: err,
		}
	}

	pp, ok := p.(*playwrightPage)
	if !ok {
		return scrapemate.Response{
			Error: errors.New("unexpected page type"),
		}
	}

	if job.GetTimeout() > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, job.GetTimeout())

		defer cancel()

		pp.playwrightPage().SetDefaultTimeout(float64(job.GetTimeout().Milliseconds()))
	}

	resp := runBrowserActions(ctx, job, playwrightadapter.NewPage(pp.playwrightPage()))

	if cleanErr := slot.release(ctx); cleanErr != nil && resp.Error == nil {
		resp.Error = cleanErr
	}

	return resp
}

// fetchWithoutProxy loads a job in a browser context that has no proxy.
// The proxied pool stays reserved for Google search.
func (o *jsFetch) fetchWithoutProxy(ctx context.Context, job scrapemate.IJob) scrapemate.Response {
	if job.GetTimeout() > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, job.GetTimeout())

		defer cancel()
	}

	b, err := o.directBrowser()
	if err != nil {
		return scrapemate.Response{Error: err}
	}

	page, err := b.ctx.NewPage()
	if err != nil {
		return scrapemate.Response{Error: err}
	}

	defer closeWithTimeout(func() error { return page.Close() }, closeTimeout)

	if job.GetTimeout() > 0 {
		page.SetDefaultTimeout(float64(job.GetTimeout().Milliseconds()))
	}

	return runBrowserActions(ctx, job, playwrightadapter.NewPage(page))
}

func (o *jsFetch) directBrowser() (*browser, error) {
	o.directMu.Lock()
	defer o.directMu.Unlock()

	select {
	case <-o.closed:
		return nil, errFetcherClosed
	default:
	}

	if o.direct != nil {
		return o.direct, nil
	}

	b, err := newBrowser(o.pw, o.headless, o.disableImages, nil, o.ua)
	if err != nil {
		return nil, err
	}

	o.direct = b

	return b, nil
}

func (o *jsFetch) fetchWithPageSlot(ctx context.Context, job scrapemate.IJob) scrapemate.Response {
	if job.GetTimeout() > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, job.GetTimeout())

		defer cancel()
	}

	lease, err := o.pageSlots.acquire(ctx)
	if err != nil {
		return scrapemate.Response{Error: err}
	}

	defer lease.release(ctx)

	page, err := lease.slot.ctx.NewPage()
	if err != nil {
		return scrapemate.Response{Error: err}
	}

	defer closeWithTimeout(func() error { return page.Close() }, closeTimeout)

	if job.GetTimeout() > 0 {
		page.SetDefaultTimeout(float64(job.GetTimeout().Milliseconds()))
	}

	lease.slot.mu.Lock()
	lease.slot.browserUsage++
	lease.slot.mu.Unlock()

	return runBrowserActions(ctx, job, playwrightadapter.NewPage(page))
}

func runBrowserActions(ctx context.Context, job scrapemate.IJob, page scrapemate.BrowserPage) scrapemate.Response {
	if hooks, ok := page.(interface{ ClearNetworkHooks() }); ok {
		defer hooks.ClearNetworkHooks()
	}

	return job.BrowserActions(ctx, page)
}

type browser struct {
	browser      playwright.Browser
	ctx          playwright.BrowserContext
	page0Usage   int
	browserUsage int
}

func (o *browser) Close() {
	_ = o.ctx.Close()
	_ = o.browser.Close()
}

func newBrowser(pw *playwright.Playwright, headless, disableImages bool, proxyPool *ProxyPool, ua string) (*browser, error) {
	opts := playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(headless),
		Args: []string{
			`--start-maximized`,
			`--no-default-browser-check`,
			`--disable-dev-shm-usage`,
			`--no-sandbox`,
			`--disable-setuid-sandbox`,
			`--no-zygote`,
			`--disable-gpu`,
			`--mute-audio`,
			`--disable-extensions`,
			`--single-process`,
			`--disable-breakpad`,
			`--disable-features=TranslateUI,BlinkGenPropertyTrees`,
			`--disable-ipc-flooding-protection`,
			`--enable-features=NetworkService,NetworkServiceInProcess`,
			"--enable-features=NetworkService",
			`--disable-default-apps`,
			`--disable-notifications`,
			`--disable-webgl`,
			`--disable-blink-features=AutomationControlled`,
			"--ignore-certificate-errors",
			"--ignore-certificate-errors-spki-list",
			"--disable-web-security",
		},
	}
	if disableImages {
		opts.Args = append(opts.Args, `--blink-settings=imagesEnabled=false`)
	}

	br, err := pw.Chromium.Launch(opts)
	if err != nil {
		return nil, err
	}

	const defaultWidth, defaultHeight = 1920, 1080

	bctx, err := br.NewContext(playwright.BrowserNewContextOptions{
		UserAgent: func() *string {
			if ua == "" {
				defaultUA := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36"

				return &defaultUA
			}

			return &ua
		}(),
		Viewport: &playwright.Size{
			Width:  defaultWidth,
			Height: defaultHeight,
		},
		Proxy: func() *playwright.Proxy {
			if proxyPool != nil {
				authProxy := proxyPool.Next()

				addr := authProxy.Address()

				return &playwright.Proxy{
					Server: addr,
				}
			}

			return nil
		}(),
	})
	if err != nil {
		return nil, err
	}

	ans := browser{
		browser: br,
		ctx:     bctx,
	}

	return &ans, nil
}
