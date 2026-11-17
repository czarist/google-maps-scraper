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
	Key        string
	CampaignID string
	Name       string
	MailReady  bool
	LastStatus string
	Jobs       []Job
	Running    int
	Queued     int
	Done       int
	Places     int
	WithEmail  int
	Sent       int
	Failed     int
	Open       bool
}

// CampaignStats collects every number known about a campaign.
type CampaignStats struct {
	Jobs        int
	Running     int
	Queued      int
	Done        int
	FailedJobs  int
	Places      int
	WithEmail   int
	Addresses   int
	Sent        int
	SendFailed  int
	LastSentAt  time.Time
	Dispatches  int
	LastStatus  string
	MailLog     []MailLogEntry
	FailedMails []MailLogEntry
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
}

var csvStatsCache sync.Map // path -> csvStats

// jobCSVStats counts places, places with an email, and email addresses in a job's CSV.
func (s *Service) jobCSVStats(id string) (csvStats, bool) {
	path, err := s.csvPath(id)
	if err != nil {
		return csvStats{}, false
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

	emailCol := -1

	for i, h := range header {
		if h == "emails" {
			emailCol = i
		}
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

		if stats, ok := s.jobCSVStats(j.ID); ok {
			j.WithEmail = stats.withEmail
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
