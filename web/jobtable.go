package web

import (
	"context"
	"errors"
	"time"
)

// JobSummary is the little the home table needs about a job it does not show.
type JobSummary struct {
	ID     string
	Status string
	Places int
}

// jobTableStore pages and filters jobs in the database, so the home table and
// its 3-second refresh stay fast with thousands of jobs.
type jobTableStore interface {
	JobTablePage(ctx context.Context, filter string, offset, limit int) ([]Job, int, error)
	JobSummaries(ctx context.Context, filter string) ([]JobSummary, error)
	StatusCounts(ctx context.Context) (map[string]int, error)
}

func (s *Service) listJobTableSQL(ctx context.Context, store jobTableStore, newJobID, filter string, page int) (JobPage, error) {
	if newJobID != "" {
		page = 1 // a new job is the newest one
	}

	page = max(page, 1)

	jobs, total, err := store.JobTablePage(ctx, filter, (page-1)*JobTablePageSize, JobTablePageSize)
	if err != nil {
		return JobPage{}, err
	}

	pages := max((total+JobTablePageSize-1)/JobTablePageSize, 1)
	if page > pages {
		page = pages

		if jobs, total, err = store.JobTablePage(ctx, filter, (page-1)*JobTablePageSize, JobTablePageSize); err != nil {
			return JobPage{}, err
		}
	}

	if err := s.attachCampaigns(ctx, jobs); err != nil {
		return JobPage{}, err
	}

	for i := range jobs {
		s.decorateJob(&jobs[i])
	}

	counts, err := store.StatusCounts(ctx)
	if err != nil {
		return JobPage{}, err
	}

	start := (page - 1) * JobTablePageSize

	out := JobPage{
		Jobs:        jobs,
		Total:       total,
		Filter:      filter,
		NewJobID:    newJobID,
		RenderedAt:  time.Now().UTC(),
		Running:     counts[StatusWorking],
		Queued:      counts[StatusPending],
		CurrentPage: page,
		TotalPages:  pages,
		HasPages:    pages > 1,
		HasPrev:     page > 1,
		HasNext:     page < pages,
		PrevPage:    page - 1,
		NextPage:    page + 1,
		From:        start + 1,
		To:          start + len(jobs),
	}

	if filter != "" {
		g, err := s.filterSummary(ctx, store, filter)
		if err != nil {
			return JobPage{}, err
		}

		out.Selected = &g
	}

	return out, nil
}

// decorateJob fills the live place count and the CSV numbers of a job.
func (s *Service) decorateJob(j *Job) {
	if j.Status == StatusWorking {
		if n, ok := s.Progress(j.ID); ok {
			j.Data.Run.Places = n
		}
	}

	if j.Status == StatusPending {
		return
	}

	final := j.Status == StatusOK || j.Status == StatusFailed
	if st, ok := s.jobCSVStatsCached(j.ID, final); ok {
		j.WithEmail = st.withEmail
		j.WithPhone = st.withPhone
		j.WithWhatsApp = st.withWhatsApp
	}
}

// filterSummary adds up the numbers of every job of a filter for the strip
// above the table. Queued jobs have no CSV and are not read.
func (s *Service) filterSummary(ctx context.Context, store jobTableStore, filter string) (JobGroup, error) {
	g := JobGroup{Key: filter}

	if filter != noCampaignKey {
		g.CampaignID = filter

		campaigns, err := s.ListCampaigns(ctx)
		if err != nil && !errors.Is(err, errCampaignsUnavailable) {
			return g, err
		}

		for _, c := range campaigns {
			if c.ID == filter {
				g.Name, g.MailReady, g.LastStatus = c.Name, c.MailReady, c.LastStatus
			}
		}
	}

	all, err := store.JobSummaries(ctx, filter)
	if err != nil {
		return g, err
	}

	g.JobCount = len(all)

	for _, js := range all {
		places := js.Places

		switch js.Status {
		case StatusWorking:
			g.Running++

			if n, ok := s.Progress(js.ID); ok {
				places = n
			}
		case StatusPending:
			g.Queued++
		case StatusOK:
			g.Done++
		}

		g.Places += places

		if js.Status == StatusPending {
			continue
		}

		final := js.Status == StatusOK || js.Status == StatusFailed
		if st, ok := s.jobCSVStatsCached(js.ID, final); ok {
			g.WithEmail += st.withEmail
			g.WithPhone += st.withPhone
			g.WithWhatsApp += st.withWhatsApp
		}
	}

	if g.CampaignID != "" {
		if log, err := s.MailLog(ctx, g.CampaignID); err == nil {
			for _, e := range log {
				switch e.Status {
				case MailStatusSent:
					g.Sent++
				case MailStatusFailed:
					g.Failed++
				}
			}
		}
	}

	return g, nil
}
