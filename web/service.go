package web

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Service struct {
	repo       JobRepository
	dataFolder string

	// progress holds a live place counter (*atomic.Int64) per running job.
	progress sync.Map
}

// StartProgress starts a live place counter for a job.
func (s *Service) StartProgress(id string) {
	s.progress.Store(id, new(atomic.Int64))
}

// AddProgress adds found places to a running job's counter.
func (s *Service) AddProgress(id string, n int) {
	if v, ok := s.progress.Load(id); ok {
		v.(*atomic.Int64).Add(int64(n)) //nolint:errcheck // only *atomic.Int64 is stored
	}
}

// Progress returns how many places a running job has found so far.
func (s *Service) Progress(id string) (int, bool) {
	v, ok := s.progress.Load(id)
	if !ok {
		return 0, false
	}

	return int(v.(*atomic.Int64).Load()), true //nolint:errcheck // only *atomic.Int64 is stored
}

// StopProgress removes a job's counter and returns its final value.
func (s *Service) StopProgress(id string) int {
	v, ok := s.progress.LoadAndDelete(id)
	if !ok {
		return 0
	}

	return int(v.(*atomic.Int64).Load()) //nolint:errcheck // only *atomic.Int64 is stored
}

type jobCounter interface {
	Count(context.Context, SelectParams) (int, error)
}

func NewService(repo JobRepository, dataFolder string) *Service {
	return &Service{
		repo:       repo,
		dataFolder: dataFolder,
	}
}

func (s *Service) Create(ctx context.Context, job *Job) error {
	return s.repo.Create(ctx, job)
}

func (s *Service) All(ctx context.Context) ([]Job, error) {
	return s.repo.Select(ctx, SelectParams{})
}

// ListJobs returns a page of jobs, clamping invalid page and limit values.
func (s *Service) ListJobs(ctx context.Context, page, limit int) (JobPage, error) {
	var ans JobPage

	if page < 1 {
		page = 1
	}

	if limit < 1 {
		limit = 20
	}

	total, err := s.countJobs(ctx, SelectParams{})
	if err != nil {
		return ans, err
	}

	totalPages := (total + limit - 1) / limit
	if totalPages == 0 {
		totalPages = 1
	}

	if page > totalPages {
		page = totalPages
	}

	offset := (page - 1) * limit
	params := SelectParams{
		Limit:  limit,
		Offset: offset,
	}

	jobs, err := s.repo.Select(ctx, params)
	if err != nil {
		return ans, err
	}

	for i := range jobs {
		if jobs[i].Status != StatusWorking {
			continue
		}

		if n, ok := s.Progress(jobs[i].ID); ok {
			jobs[i].Data.Run.Places = n
		}
	}

	running, err := s.countJobs(ctx, SelectParams{Status: StatusWorking})
	if err != nil {
		return ans, err
	}

	queued, err := s.countJobs(ctx, SelectParams{Status: StatusPending})
	if err != nil {
		return ans, err
	}

	ans = JobPage{
		Running:     running,
		Queued:      queued,
		RenderedAt:  time.Now().UTC(),
		Jobs:        jobs,
		CurrentPage: page,
		TotalPages:  totalPages,
		Total:       total,
		HasPrev:     page > 1,
		HasNext:     page < totalPages,
		PrevPage:    page - 1,
		NextPage:    page + 1,
		HasPages:    totalPages > 1,
	}

	return ans, nil
}

func (s *Service) countJobs(ctx context.Context, params SelectParams) (int, error) {
	if counter, ok := s.repo.(jobCounter); ok {
		return counter.Count(ctx, params)
	}

	jobs, err := s.repo.Select(ctx, params)
	if err != nil {
		return 0, fmt.Errorf("select jobs for count: %w", err)
	}

	return len(jobs), nil
}

func (s *Service) Get(ctx context.Context, id string) (Job, error) {
	return s.repo.Get(ctx, id)
}

func (s *Service) Delete(ctx context.Context, id string) error {
	datapath, err := s.csvPath(id)
	if err != nil {
		return err
	}

	if _, err := os.Stat(datapath); err == nil {
		if err := os.Remove(datapath); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	return s.repo.Delete(ctx, id)
}

func (s *Service) Update(ctx context.Context, job *Job) error {
	return s.repo.Update(ctx, job)
}

func (s *Service) SelectPending(ctx context.Context) ([]Job, error) {
	return s.repo.Select(ctx, SelectParams{Status: StatusPending, Limit: 1})
}

// csvPath returns the on-disk path of a job's CSV output, rejecting ids that
// could escape the data folder.
func (s *Service) csvPath(id string) (string, error) {
	if strings.Contains(id, "/") || strings.Contains(id, "\\") || strings.Contains(id, "..") {
		return "", fmt.Errorf("invalid file name")
	}

	return filepath.Join(s.dataFolder, id+".csv"), nil
}

func (s *Service) GetCSV(_ context.Context, id string) (string, error) {
	datapath, err := s.csvPath(id)
	if err != nil {
		return "", err
	}

	if _, err := os.Stat(datapath); os.IsNotExist(err) {
		return "", fmt.Errorf("csv file not found for job %s", id)
	}

	return datapath, nil
}
