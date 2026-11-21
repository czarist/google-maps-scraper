package web

import (
	"context"
	"strings"
	"time"
)

// Errors shown on jobs that are queued again.
const (
	// ErrMsgInterrupted marks a job whose run was cut by a server restart.
	ErrMsgInterrupted = "interrupted: the server restarted during the run"
	// ErrMsgStuck marks a run that went far past its time limit and was stopped.
	ErrMsgStuck = "stopped: the run went past its time limit and did not finish"
)

// proxyRetryDelay is the minimum wait before retrying a job that failed only
// because no proxy could be confirmed working. It gives the proxy health
// cache (proxyGoodFor/proxyBadFor in proxies.go) time to recover instead of
// retrying straight back into a pool that was just marked bad account-wide.
const proxyRetryDelay = 2 * time.Minute

// retryDelay is how long a failed job waits before it can run again: 1 minute
// after the first failure, 2 after the second. Other queued jobs go first.
// A job that failed on ErrNoProxies waits at least proxyRetryDelay.
func retryDelay(attempts int, lastErr string) time.Duration {
	d := time.Duration(max(attempts, 1)) * time.Minute

	if strings.Contains(lastErr, ErrNoProxies.Error()) {
		d = max(d, proxyRetryDelay)
	}

	return d
}

// QueueRetry turns a failed run into another try at the end of the queue, unless
// the job already used MaxJobAttempts. It reports whether the job was queued again.
// The caller saves the job.
func QueueRetry(job *Job, now time.Time) bool {
	if job.Data.Run.Attempts >= MaxJobAttempts {
		return false
	}

	job.Status = StatusPending
	job.Data.Run.RetryAfter = now.Add(retryDelay(job.Data.Run.Attempts, job.Data.Run.Error))

	return true
}

// RequeueInterrupted puts jobs left "running" by a stopped server back in the queue.
// It runs when the server starts, when no job can really be running. A restart is
// not the job's fault, so it does not count as an attempt.
func (s *Service) RequeueInterrupted(ctx context.Context) (int, error) {
	jobs, err := s.repo.Select(ctx, SelectParams{Status: StatusWorking})
	if err != nil {
		return 0, err
	}

	now := time.Now().UTC()

	for i := range jobs {
		j := &jobs[i]
		j.Status = StatusPending
		j.Data.Run.Attempts = max(j.Data.Run.Attempts-1, 0)
		j.Data.Run.FinishedAt = now
		j.Data.Run.Error = ErrMsgInterrupted
		j.Data.Run.RetryAfter = time.Time{}

		if err := s.repo.Update(ctx, j); err != nil {
			return i, err
		}
	}

	return len(jobs), nil
}
