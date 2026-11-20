package web

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"uuid"
)

// bulkJobStore is implemented by repositories that can change many jobs in one
// transaction. Without it, the service falls back to one write per job.
type bulkJobStore interface {
	CreateJobs(ctx context.Context, jobs []Job, campaignID string) error
	AssignJobs(ctx context.Context, campaignID string, jobIDs []string) error
	DeleteJobs(ctx context.Context, ids []string) error
}

// CreateJobs stores many jobs at once and, when campaignID is set, puts them in that campaign.
func (s *Service) CreateJobs(ctx context.Context, jobs []Job, campaignID string) error {
	if bulk, ok := s.repo.(bulkJobStore); ok {
		return bulk.CreateJobs(ctx, jobs, campaignID)
	}

	for i := range jobs {
		if err := s.repo.Create(ctx, &jobs[i]); err != nil {
			return err
		}

		if campaignID != "" {
			if err := s.AssignJob(ctx, campaignID, jobs[i].ID); err != nil {
				return err
			}
		}
	}

	return nil
}

// MoveJobs puts jobs in a campaign. An empty campaignID takes them out of any campaign.
func (s *Service) MoveJobs(ctx context.Context, campaignID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}

	if bulk, ok := s.repo.(bulkJobStore); ok {
		return bulk.AssignJobs(ctx, campaignID, ids)
	}

	store, err := s.campaignStore()
	if err != nil {
		return err
	}

	if campaignID != "" {
		for _, id := range ids {
			if err := store.AssignJob(ctx, campaignID, id); err != nil {
				return err
			}
		}

		return nil
	}

	refs, err := store.CampaignsForJobs(ctx, ids)
	if err != nil {
		return err
	}

	for id, ref := range refs {
		if err := store.UnassignJob(ctx, ref.ID, id); err != nil {
			return err
		}
	}

	return nil
}

// DeleteMany deletes several jobs and their CSV files.
func (s *Service) DeleteMany(ctx context.Context, ids []string) error {
	bulk, ok := s.repo.(bulkJobStore)
	if !ok {
		for _, id := range ids {
			if err := s.Delete(ctx, id); err != nil {
				return fmt.Errorf("delete job %s: %w", id, err)
			}
		}

		return nil
	}

	for _, id := range ids {
		path, err := s.csvPath(id)
		if err != nil {
			return err
		}

		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	return bulk.DeleteJobs(ctx, ids)
}

// JobIDsInFilter returns the ids of every job the home table shows for a filter:
// "" for all, noCampaignKey for jobs without a campaign, or a campaign id.
func (s *Service) JobIDsInFilter(ctx context.Context, filter string) ([]string, error) {
	jobs, err := s.All(ctx)
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(jobs))

	for i := range jobs {
		switch {
		case filter == "":
		case filter == noCampaignKey && jobs[i].CampaignID == "":
		case filter == jobs[i].CampaignID:
		default:
			continue
		}

		ids = append(ids, jobs[i].ID)
	}

	return ids, nil
}

// CreateCampaignNamed creates a campaign with a name and service and returns its id.
func (s *Service) CreateCampaignNamed(ctx context.Context, name, service string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("missing campaign name")
	}

	c := Campaign{ID: uuid.NewV4().String(), Name: name, ServiceName: service, CreatedAt: time.Now().UTC()}
	if err := s.CreateCampaign(ctx, &c); err != nil {
		return "", err
	}

	return c.ID, nil
}

// MergeCampaignJobs moves every job of the source campaigns into target and
// returns how many moved. The source campaigns stay, with their email settings
// and history, and can be deleted afterwards.
func (s *Service) MergeCampaignJobs(ctx context.Context, target string, sources []string) (int, error) {
	store, err := s.campaignStore()
	if err != nil {
		return 0, err
	}

	var ids []string

	for _, src := range sources {
		src = strings.TrimSpace(src)
		if src == "" || src == target {
			continue
		}

		jobIDs, err := store.CampaignJobs(ctx, src)
		if err != nil {
			return 0, err
		}

		ids = append(ids, jobIDs...)
	}

	if err := s.MoveJobs(ctx, target, ids); err != nil {
		return 0, err
	}

	return len(ids), nil
}
