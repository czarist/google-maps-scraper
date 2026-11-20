package web

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// noCampaignKey identifies the group of jobs that belong to no campaign.
const noCampaignKey = "none"

// JobGroup is one accordion on the home page: a campaign and its jobs.
type JobGroup struct {
	// JobCount is how many jobs the group has (Jobs may hold only some of them).
	JobCount     int
	WithPhone    int
	WithWhatsApp int
	Key          string
	CampaignID   string
	Name         string
	MailReady    bool
	LastStatus   string
	Jobs         []Job
	Running      int
	Queued       int
	Done         int
	Places       int
	WithEmail    int
	Sent         int
	Failed       int
	Open         bool
}

// CampaignStats collects every number known about a campaign.
type CampaignStats struct {
	WithPhone    int
	WithWhatsApp int
	Jobs         int
	Running      int
	Queued       int
	Done         int
	FailedJobs   int
	Places       int
	WithEmail    int
	Addresses    int
	Sent         int
	SendFailed   int
	LastSentAt   time.Time
	Dispatches   int
	LastStatus   string
	MailLog      []MailLogEntry
	FailedMails  []MailLogEntry
}

// LastSentLabel is the UTC time of the last send attempt.
func (c CampaignStats) LastSentLabel() string {
	if c.LastSentAt.IsZero() {
		return ""
	}

	return c.LastSentAt.UTC().Format("2006-01-02 15:04")
}

// csvStats is what a job's CSV says, cached by file size and time.
type csvStats struct {
	size      int64
	modTime   time.Time
	places    int
	withEmail int
	emails    int
	// withPhone counts places with a phone, from Google Maps or the website.
	withPhone int
	// withWhatsApp counts places whose website links to a WhatsApp number.
	withWhatsApp int
}

var csvStatsCache sync.Map // path -> csvStats

// jobCSVStats counts places, places with an email, and email addresses in a job's CSV.
func (s *Service) jobCSVStats(id string) (csvStats, bool) {
	return s.jobCSVStatsCached(id, false)
}

// jobCSVStatsCached is jobCSVStats; with final set, a cached result is trusted
// without checking the file, because a finished job's CSV does not change.
func (s *Service) jobCSVStatsCached(id string, final bool) (csvStats, bool) {
	path, err := s.csvPath(id)
	if err != nil {
		return csvStats{}, false
	}

	if final {
		if cached, ok := csvStatsCache.Load(path); ok {
			return cached.(csvStats), true
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		return csvStats{}, false
	}

	if cached, ok := csvStatsCache.Load(path); ok {
		c := cached.(csvStats)
		if c.size == info.Size() && c.modTime.Equal(info.ModTime()) {
			return c, true
		}
	}

	stats, err := countCSV(path)
	if err != nil {
		return csvStats{}, false
	}

	stats.size, stats.modTime = info.Size(), info.ModTime()
	csvStatsCache.Store(path, stats)

	return stats, true
}

func countCSV(path string) (csvStats, error) {
	f, err := os.Open(path)
	if err != nil {
		return csvStats{}, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1

	header, err := r.Read()
	if errors.Is(err, io.EOF) {
		return csvStats{}, nil
	}

	if err != nil {
		return csvStats{}, err
	}

	emailCol, phoneCol, sitePhonesCol, whatsappCol := -1, -1, -1, -1

	for i, h := range header {
		switch h {
		case "emails":
			emailCol = i
		case "phone":
			phoneCol = i
		case "website_phones":
			sitePhonesCol = i
		case "whatsapp":
			whatsappCol = i
		}
	}

	has := func(row []string, col int) bool {
		return col >= 0 && col < len(row) && strings.TrimSpace(row[col]) != ""
	}

	var stats csvStats

	for {
		row, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return csvStats{}, err
		}

		stats.places++

		if has(row, phoneCol) || has(row, sitePhonesCol) {
			stats.withPhone++
		}

		if has(row, whatsappCol) {
			stats.withWhatsApp++
		}

		if emailCol < 0 || emailCol >= len(row) {
			continue
		}

		n := 0

		for _, e := range strings.Split(row[emailCol], ",") {
			if strings.TrimSpace(e) != "" {
				n++
			}
		}

		if n > 0 {
			stats.withEmail++
			stats.emails += n
		}
	}

	return stats, nil
}

// ListJobGroups returns every job grouped by campaign, campaigns without jobs
// included, then the jobs with no campaign. newJobID marks a job just created.
func (s *Service) ListJobGroups(ctx context.Context, newJobID string) (JobPage, error) {
	jobs, err := s.All(ctx)
	if err != nil {
		return JobPage{}, err
	}

	campaigns, err := s.ListCampaigns(ctx)
	if errors.Is(err, errCampaignsUnavailable) {
		campaigns = nil
	} else if err != nil {
		return JobPage{}, err
	}

	groups := make([]JobGroup, 0, len(campaigns)+1)
	index := make(map[string]int, len(campaigns)+1)

	for _, c := range campaigns {
		index[c.ID] = len(groups)
		groups = append(groups, JobGroup{Key: c.ID, CampaignID: c.ID, Name: c.Name, MailReady: c.MailReady, LastStatus: c.LastStatus})
	}

	page := JobPage{RenderedAt: time.Now().UTC(), Jobs: jobs, Total: len(jobs), NewJobID: newJobID}

	for i := range jobs {
		j := &jobs[i]

		if j.Status == StatusWorking {
			if n, ok := s.Progress(j.ID); ok {
				j.Data.Run.Places = n
			}
		}

		// Queued jobs have no CSV yet; finished ones never change, so their
		// cached numbers are reused without touching the disk.
		if j.Status != StatusPending {
			final := j.Status == StatusOK || j.Status == StatusFailed
			if stats, ok := s.jobCSVStatsCached(j.ID, final); ok {
				j.WithEmail = stats.withEmail
				j.WithPhone = stats.withPhone
				j.WithWhatsApp = stats.withWhatsApp
			}
		}

		key := j.CampaignID
		if key == "" {
			key = noCampaignKey
		}

		gi, ok := index[key]
		if !ok {
			index[key] = len(groups)
			gi = len(groups)
			groups = append(groups, JobGroup{Key: noCampaignKey})
		}

		g := &groups[gi]
		g.Jobs = append(g.Jobs, *j)
		g.Places += j.Data.Run.Places
		g.WithEmail += j.WithEmail
		g.WithPhone += j.WithPhone
		g.WithWhatsApp += j.WithWhatsApp

		switch j.Status {
		case StatusWorking:
			g.Running++
			page.Running++
		case StatusPending:
			g.Queued++
			page.Queued++
		case StatusOK:
			g.Done++
		}

		if j.ID == newJobID {
			g.Open = true
		}
	}

	for i := range groups {
		g := &groups[i]
		if g.Running > 0 || g.Queued > 0 {
			g.Open = true
		}

		if g.CampaignID == "" {
			continue
		}

		if log, err := s.MailLog(ctx, g.CampaignID); err == nil {
			for _, e := range log {
				if e.Status == MailStatusSent {
					g.Sent++
				} else {
					g.Failed++
				}
			}
		}
	}

	// The no-campaign group always comes last.
	if gi, ok := index[noCampaignKey]; ok && gi != len(groups)-1 {
		none := groups[gi]
		groups = append(append(groups[:gi:gi], groups[gi+1:]...), none)
	}

	page.Groups = groups

	return page, nil
}

// MailLog returns the latest send result per address of a campaign.
func (s *Service) MailLog(ctx context.Context, campaignID string) ([]MailLogEntry, error) {
	store, err := s.campaignStore()
	if err != nil {
		return nil, err
	}

	return store.MailLog(ctx, campaignID)
}

// CampaignStats gathers job, place and email numbers for a campaign.
func (s *Service) CampaignStats(ctx context.Context, campaignID string) (CampaignStats, error) {
	var st CampaignStats

	jobs, err := s.CampaignJobList(ctx, campaignID)
	if err != nil {
		return st, err
	}

	st.Jobs = len(jobs)

	for i := range jobs {
		switch jobs[i].Status {
		case StatusWorking:
			st.Running++
		case StatusPending:
			st.Queued++
		case StatusOK:
			st.Done++
		case StatusFailed:
			st.FailedJobs++
		}

		places := jobs[i].Data.Run.Places
		if n, ok := s.Progress(jobs[i].ID); ok {
			places = n
		}

		st.Places += places

		if stats, ok := s.jobCSVStats(jobs[i].ID); ok {
			st.WithEmail += stats.withEmail
			st.WithPhone += stats.withPhone
			st.WithWhatsApp += stats.withWhatsApp
			st.Addresses += stats.emails
		}
	}

	log, err := s.MailLog(ctx, campaignID)
	if err != nil {
		return st, err
	}

	st.MailLog = log

	for _, e := range log {
		if e.Status == MailStatusSent {
			st.Sent++
		} else {
			st.SendFailed++
			st.FailedMails = append(st.FailedMails, e)
		}

		if e.SentAt.After(st.LastSentAt) {
			st.LastSentAt = e.SentAt
		}
	}

	dispatches, err := s.ListDispatches(ctx, campaignID)
	if err != nil {
		return st, err
	}

	st.Dispatches = len(dispatches)
	if len(dispatches) > 0 {
		st.LastStatus = dispatches[0].Status
	}

	return st, nil
}

// JobTablePageSize is how many jobs one page of the home table shows.
const JobTablePageSize = 10

// ListJobTable returns one page of the home job table.
// filter is "" for every job, noCampaignKey for jobs without a campaign, or a campaign id.
// page starts at 1; a page past the end shows the last page.
// When newJobID is set, the page that holds it is shown.
func (s *Service) ListJobTable(ctx context.Context, newJobID, filter, status, order string, page int) (JobPage, error) {
	if !ValidJobOrder(order) {
		order = JobOrderNewest
	}

	if !ValidJobStatusFilter(status) {
		status = ""
	}

	if store, ok := s.repo.(jobTableStore); ok {
		return s.listJobTableSQL(ctx, store, newJobID, filter, status, order, page)
	}

	return s.listJobTableInMemory(ctx, newJobID, filter, status, order, page)
}

// listJobTableInMemory builds the table from every job; used by repositories
// without SQL paging (tests).
func (s *Service) listJobTableInMemory(ctx context.Context, newJobID, filter, status, order string, page int) (JobPage, error) {
	all, err := s.ListJobGroups(ctx, newJobID)
	if err != nil {
		return JobPage{}, err
	}

	names := make(map[string]string, len(all.Groups))
	for _, g := range all.Groups {
		if g.CampaignID != "" {
			names[g.CampaignID] = g.Name
		}
	}

	jobs := make([]Job, 0, len(all.Jobs))

	for _, j := range all.Jobs {
		switch {
		case filter == "":
		case filter == noCampaignKey && j.CampaignID == "":
		case filter == j.CampaignID:
		default:
			continue
		}

		if j.Status == StatusFailed {
			all.FailedCount++
		}

		if !matchesStatus(status, j.Status) {
			continue
		}

		if j.CampaignName == "" {
			j.CampaignName = names[j.CampaignID]
		}

		jobs = append(jobs, j)
	}

	for i := range all.Groups {
		all.Groups[i].JobCount = len(all.Groups[i].Jobs)
	}

	// The chosen status comes first (running by default); the rest keep their
	// order (newest first).
	sortJobsBy(jobs, order)

	for i := range all.Groups {
		g := &all.Groups[i]
		if (filter == noCampaignKey && g.CampaignID == "") || (filter != "" && g.CampaignID == filter) {
			all.Selected = g
		}
	}

	if newJobID != "" {
		for i := range jobs {
			if jobs[i].ID == newJobID {
				page = i/JobTablePageSize + 1
			}
		}
	}

	total := len(jobs)
	pages := max((total+JobTablePageSize-1)/JobTablePageSize, 1)
	page = min(max(page, 1), pages)

	start := (page - 1) * JobTablePageSize
	end := min(start+JobTablePageSize, total)

	all.Jobs = jobs[start:end]
	all.Total = total
	all.Filter = filter
	all.Order = order
	all.Status = status
	all.CurrentPage = page
	all.TotalPages = pages
	all.HasPages = pages > 1
	all.HasPrev = page > 1
	all.HasNext = page < pages
	all.PrevPage = page - 1
	all.NextPage = page + 1
	all.From = start + 1
	all.To = end

	return all, nil
}
