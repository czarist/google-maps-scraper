package web

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	_ "time/tzdata" // "today" on the dashboard uses the Brazilian time zone even in a slim container.

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"
	"github.com/shirou/gopsutil/v4/sensors"
)

// dashboardTZ is the zone that decides when "today" starts. DASHBOARD_TZ overrides it.
func dashboardTZ() *time.Location {
	name := strings.TrimSpace(os.Getenv("DASHBOARD_TZ"))
	if name == "" {
		name = "America/Sao_Paulo"
	}

	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.Local
	}

	return loc
}

// ActivityStats is today's job and email activity, counted in the database.
type ActivityStats struct {
	JobsDone       int
	JobsFailed     int
	PlacesFound    int
	AvgJobDuration time.Duration
	EmailsSent     int
	EmailsFailed   int
	AutoCampaigns  int
	Dispatching    int
}

// activityStore counts today's activity.
type activityStore interface {
	DashboardStats(ctx context.Context, since time.Time) (ActivityStats, error)
}

// SystemInfo describes the machine the scraper runs on.
type SystemInfo struct {
	Hostname    string
	OS          string
	Kernel      string
	InContainer bool
	HostUptime  time.Duration
	AppUptime   time.Duration
	CPUModel    string
	CPUCores    int
	CPUThreads  int
	CPUPercent  float64
	Load1       float64
	Load5       float64
	Load15      float64
	MemTotal    uint64
	MemUsed     uint64
	MemPercent  float64
	SwapTotal   uint64
	SwapUsed    uint64
	DiskPath    string
	DiskTotal   uint64
	DiskUsed    uint64
	DiskPercent float64
	// Temps lists the temperature sensors; empty when the system does not expose them.
	Temps []Temperature
	// Browsers counts the headless Chromium processes and the memory they use.
	Browsers    int
	BrowsersMem uint64
	AppMem      uint64
	Goroutines  int
	GoVersion   string
}

// Temperature is one sensor reading in °C.
type Temperature struct {
	Name    string
	Celsius float64
}

// Dashboard is everything the home panel shows.
type Dashboard struct {
	System   SystemInfo
	Activity ActivityStats
	Workers  int
	Running  int
	Queued   int
	// QueueETA estimates when the queue empties, from the recent run times.
	QueueETA  time.Duration
	Proxies   ProxySummary
	Backup    BackupInfo
	Generated time.Time
}

// ProxySummary is the proxy part of the dashboard.
type ProxySummary struct {
	ProxyScrape bool
	Plans       []string
	Available   int
	Error       string
	Healthy     int
	Bad         int
	Checked     time.Time
}

var appStarted = time.Now()

// cpuSampler keeps the latest CPU usage, measured in the background, so a
// dashboard refresh never waits for a measurement.
var (
	cpuSamplerOnce sync.Once
	cpuPercentBits atomic.Uint64
)

func startCPUSampler() {
	cpuSamplerOnce.Do(func() {
		go func() {
			for {
				if p, err := cpu.Percent(2*time.Second, false); err == nil && len(p) > 0 {
					cpuPercentBits.Store(uint64(p[0] * 100))
				}
			}
		}()
	})
}

func cpuPercent() float64 {
	return float64(cpuPercentBits.Load()) / 100
}

// SetWorkerSlots records how many jobs the runner runs at once, for the dashboard.
func (s *Service) SetWorkerSlots(n int) {
	s.workerSlots.Store(int64(n))
}

// Dashboard gathers the home panel. Every part is best effort: what the system
// does not expose (temperature in a container) is left empty.
func (s *Service) Dashboard(ctx context.Context) Dashboard {
	startCPUSampler()

	d := Dashboard{
		System:    s.systemInfo(ctx),
		Workers:   int(s.workerSlots.Load()),
		Backup:    s.BackupStatus(),
		Proxies:   s.proxySummary(ctx),
		Generated: time.Now(),
	}

	if store, ok := s.repo.(jobTableStore); ok {
		if counts, err := store.StatusCounts(ctx); err == nil {
			d.Running, d.Queued = counts[StatusWorking], counts[StatusPending]
		}
	}

	if store, ok := s.repo.(activityStore); ok {
		y, m, day := time.Now().In(dashboardTZ()).Date()
		midnight := time.Date(y, m, day, 0, 0, 0, 0, dashboardTZ())

		if act, err := store.DashboardStats(ctx, midnight); err == nil {
			d.Activity = act
		}
	}

	if d.Workers > 0 && d.Activity.AvgJobDuration > 0 && d.Queued > 0 {
		rounds := (d.Queued + d.Workers - 1) / d.Workers
		d.QueueETA = time.Duration(rounds) * d.Activity.AvgJobDuration
	}

	return d
}

func (s *Service) systemInfo(ctx context.Context) SystemInfo {
	info := SystemInfo{
		CPUPercent: cpuPercent(),
		AppUptime:  time.Since(appStarted),
		Goroutines: runtime.NumGoroutine(),
		GoVersion:  runtime.Version(),
		DiskPath:   s.dataFolder,
	}

	if _, err := os.Stat("/.dockerenv"); err == nil {
		info.InContainer = true
	}

	if h, err := host.InfoWithContext(ctx); err == nil {
		info.Hostname = h.Hostname
		info.OS = strings.TrimSpace(h.Platform + " " + h.PlatformVersion)
		info.Kernel = h.KernelVersion
		info.HostUptime = time.Duration(h.Uptime) * time.Second
	}

	if cpus, err := cpu.InfoWithContext(ctx); err == nil && len(cpus) > 0 {
		info.CPUModel = strings.TrimSpace(cpus[0].ModelName)
	}

	if n, err := cpu.CountsWithContext(ctx, false); err == nil {
		info.CPUCores = n
	}

	info.CPUThreads = runtime.NumCPU()

	if l, err := load.AvgWithContext(ctx); err == nil {
		info.Load1, info.Load5, info.Load15 = l.Load1, l.Load5, l.Load15
	}

	if v, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		info.MemTotal, info.MemUsed, info.MemPercent = v.Total, v.Used, v.UsedPercent
	}

	if sw, err := mem.SwapMemoryWithContext(ctx); err == nil {
		info.SwapTotal, info.SwapUsed = sw.Total, sw.Used
	}

	if du, err := disk.UsageWithContext(ctx, s.dataFolder); err == nil {
		info.DiskTotal, info.DiskUsed, info.DiskPercent = du.Total, du.Used, du.UsedPercent
	}

	if temps, err := sensors.TemperaturesWithContext(ctx); err == nil || len(temps) > 0 {
		for _, t := range temps {
			if t.Temperature > 0 && t.Temperature < 150 { //nolint:mnd // drop sensors that report nonsense
				info.Temps = append(info.Temps, Temperature{Name: t.SensorKey, Celsius: t.Temperature})
			}
		}
	}

	var ms runtime.MemStats

	runtime.ReadMemStats(&ms)
	info.AppMem = ms.Sys

	if procs, err := process.ProcessesWithContext(ctx); err == nil {
		for _, p := range procs {
			name, err := p.NameWithContext(ctx)
			if err != nil {
				continue
			}

			lower := strings.ToLower(name)
			if !strings.Contains(lower, "chrom") && !strings.Contains(lower, "headless_shell") {
				continue
			}

			info.Browsers++

			if m, err := p.MemoryInfoWithContext(ctx); err == nil {
				info.BrowsersMem += m.RSS
			}
		}
	}

	return info
}

// proxySummary caches the ProxyScrape part for a few minutes: the dashboard
// refreshes every few seconds and the account API should not be hit that often.
func (s *Service) proxySummary(ctx context.Context) ProxySummary {
	s.proxySummaryMu.Lock()
	defer s.proxySummaryMu.Unlock()

	good, bad := ProxyHealthCounts()

	if time.Since(s.proxySummaryAt) < proxySummaryTTL {
		cached := s.proxySummaryVal
		cached.Healthy, cached.Bad = good, bad

		return cached
	}

	ps := ProxySummary{ProxyScrape: s.proxyScrape != nil, Healthy: good, Bad: bad, Checked: time.Now()}

	if s.proxyScrape != nil {
		subs, err := s.proxyScrape.Subaccounts(ctx)
		if err != nil {
			ps.Error = err.Error()
		}

		for _, sub := range subs {
			label := sub.Label
			if label == "" {
				label = sub.AccountType
			}

			ps.Plans = append(ps.Plans, label)
		}

		if proxies, err := s.proxyScrape.CachedProxies(ctx, false); err == nil {
			ps.Available = len(proxies)
		} else if ps.Error == "" {
			ps.Error = err.Error()
		}
	} else if saved, err := s.SavedProxies(); err == nil {
		ps.Available = len(saved)
	}

	s.proxySummaryVal, s.proxySummaryAt = ps, time.Now()

	return ps
}

// proxySummaryTTL is how long the dashboard reuses the ProxyScrape numbers.
const proxySummaryTTL = 5 * time.Minute

// Formatting for the dashboard template.

func bytesLabel(b uint64) string {
	const gb = 1 << 30

	const mb = 1 << 20

	if b >= gb {
		return strings.Replace(strconvFloat(float64(b)/gb, 1), ".", ",", 1) + " GB"
	}

	return strconvFloat(float64(b)/mb, 0) + " MB"
}

func strconvFloat(v float64, prec int) string {
	return strconv.FormatFloat(v, 'f', prec, 64)
}

// level classifies a usage percentage for the bar color.
func level(p float64) string {
	switch {
	case p >= 90: //nolint:mnd // red above 90%
		return "high"
	case p >= 70: //nolint:mnd // amber above 70%
		return "warn"
	default:
		return "ok"
	}
}

//nolint:gocritic // value receivers keep the methods callable from templates.
func (i SystemInfo) MemUsedLabel() string { return bytesLabel(i.MemUsed) }

//nolint:gocritic // template method
func (i SystemInfo) MemTotalLabel() string { return bytesLabel(i.MemTotal) }

//nolint:gocritic // template method
func (i SystemInfo) SwapUsedLabel() string { return bytesLabel(i.SwapUsed) }

//nolint:gocritic // template method
func (i SystemInfo) SwapTotalLabel() string { return bytesLabel(i.SwapTotal) }

//nolint:gocritic // template method
func (i SystemInfo) DiskUsedLabel() string { return bytesLabel(i.DiskUsed) }

//nolint:gocritic // template method
func (i SystemInfo) DiskTotalLabel() string { return bytesLabel(i.DiskTotal) }

//nolint:gocritic // template method
func (i SystemInfo) BrowsersMemLabel() string { return bytesLabel(i.BrowsersMem) }

//nolint:gocritic // template method
func (i SystemInfo) AppMemLabel() string { return bytesLabel(i.AppMem) }

//nolint:gocritic // template method
func (i SystemInfo) CPUPct() int { return int(i.CPUPercent + 0.5) }

//nolint:gocritic // template method
func (i SystemInfo) MemPct() int { return int(i.MemPercent + 0.5) }

//nolint:gocritic // template method
func (i SystemInfo) DiskPct() int { return int(i.DiskPercent + 0.5) }

//nolint:gocritic // template method
func (i SystemInfo) CPULevel() string { return level(i.CPUPercent) }

//nolint:gocritic // template method
func (i SystemInfo) MemLevel() string { return level(i.MemPercent) }

//nolint:gocritic // template method
func (i SystemInfo) DiskLevel() string { return level(i.DiskPercent) }

//nolint:gocritic // template method
func (i SystemInfo) HostUptimeLabel() string { return FormatDuration(i.HostUptime) }

//nolint:gocritic // template method
func (i SystemInfo) AppUptimeLabel() string { return FormatDuration(i.AppUptime) }

//nolint:gocritic // template method
func (i SystemInfo) LoadLabel() string {
	return strconvFloat(i.Load1, 2) + " · " + strconvFloat(i.Load5, 2) + " · " + strconvFloat(i.Load15, 2)
}

// MaxTemp is the hottest sensor, or 0 when there is none.
//
//nolint:gocritic // template method
func (i SystemInfo) MaxTemp() int {
	hottest := 0.0
	for _, t := range i.Temps {
		hottest = max(hottest, t.Celsius)
	}

	return int(hottest + 0.5)
}

//nolint:gocritic // template method
func (d Dashboard) QueueETALabel() string { return FormatDuration(d.QueueETA) }

//nolint:gocritic // template method
func (d Dashboard) AvgJobLabel() string { return FormatDuration(d.Activity.AvgJobDuration) }

//nolint:gocritic // template method
func (d Dashboard) WorkerPct() int {
	if d.Workers == 0 {
		return 0
	}

	return min(d.Running*100/d.Workers, 100) //nolint:mnd // percent
}

//nolint:gocritic // template method
func (d Dashboard) BackupAgeLabel() string {
	if d.Backup.Last.IsZero() {
		return ""
	}

	return FormatDuration(time.Since(d.Backup.Last))
}

//nolint:gocritic // template method
func (d Dashboard) BackupSizeLabel() string { return bytesLabel(uint64(max(d.Backup.Size, 0))) }

//nolint:gocritic // template method
func (p ProxySummary) PlansLabel() string { return strings.Join(p.Plans, ", ") }
