package web

import (
	"context"
	"errors"
)

// ErrJobNotFailed is returned when retrying a job that did not fail.
var ErrJobNotFailed = errors.New("only failed jobs can be run again")

// RetryJob puts a failed job back in the queue as a fresh run: its attempts
// start over and it can run as soon as a worker is free.
func (s *Service) RetryJob(ctx context.Context, id string) error {
	job, err := s.Get(ctx, id)
	if err != nil {
		return err
	}

	if job.Status != StatusFailed {
		return ErrJobNotFailed
	}

	job.Status = StatusPending
	job.Data.Run = JobRun{}

	return s.Update(ctx, &job)
}

// ValidJobStatusFilter reports whether status is a job list status filter:
// "" for every job, or one job status.
func ValidJobStatusFilter(status string) bool {
	switch status {
	case "", StatusOK, StatusFailed, StatusWorking, StatusPending:
		return true
	default:
		return false
	}
}

// matchesStatus reports whether a job with status passes the status filter.
func matchesStatus(filter, status string) bool {
	return filter == "" || filter == status
}
