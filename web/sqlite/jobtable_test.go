package sqlite

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gosom/google-maps-scraper/web"
)

func TestJobTableIsPagedInSQL(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	svc := web.NewService(repo, t.TempDir())

	if err := repo.CreateCampaign(ctx, &web.Campaign{ID: "camp", Name: "Camp", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	const n = 5000

	jobs := make([]web.Job, n)
	base := time.Now().Add(-time.Hour)

	for i := range jobs {
		jobs[i] = web.Job{ID: fmt.Sprintf("job-%05d", i), Name: "j", Status: web.StatusPending, Date: base.Add(time.Duration(i) * time.Second)}
	}

	jobs[0].Status = web.StatusWorking // the oldest is running

	if err := repo.CreateJobs(ctx, jobs[:4000], ""); err != nil {
		t.Fatal(err)
	}

	if err := repo.CreateJobs(ctx, jobs[4000:], "camp"); err != nil {
		t.Fatal(err)
	}

	start := time.Now()

	page, err := svc.ListJobTable(ctx, "", "", "", "", 1)
	if err != nil {
		t.Fatal(err)
	}

	if took := time.Since(start); took > time.Second {
		t.Fatalf("first page of %d jobs took %s", n, took)
	}

	if page.Total != n || len(page.Jobs) != web.JobTablePageSize || page.Jobs[0].ID != "job-00000" || page.Running != 1 || page.Queued != n-1 {
		t.Fatalf("page: total %d, %d rows, first %s, running %d, queued %d", page.Total, len(page.Jobs), page.Jobs[0].ID, page.Running, page.Queued)
	}

	if page.Jobs[1].ID != "job-04999" {
		t.Fatalf("after the running job comes the newest, got %s", page.Jobs[1].ID)
	}

	// A page past the end shows the last one.
	last, err := svc.ListJobTable(ctx, "", "camp", "", "", 999)
	if err != nil {
		t.Fatal(err)
	}

	if last.Total != 1000 || last.CurrentPage != 100 || last.Selected == nil || last.Selected.JobCount != 1000 || last.Jobs[0].CampaignName != "Camp" {
		t.Fatalf("campaign filter: total %d page %d selected %+v", last.Total, last.CurrentPage, last.Selected)
	}

	none, err := svc.ListJobTable(ctx, "", "none", "", "", 1)
	if err != nil || none.Total != 4000 {
		t.Fatalf("no-campaign filter: %d, %v", none.Total, err)
	}

	ids, err := svc.JobIDsInFilter(ctx, "camp", "")
	if err != nil || len(ids) != 1000 {
		t.Fatalf("ids in filter: %d, %v", len(ids), err)
	}
}

func TestJobTableOrderStatusFilterAndRetry(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	svc := web.NewService(repo, t.TempDir())

	base := time.Now().Add(-time.Hour)
	statuses := []string{web.StatusOK, web.StatusFailed, web.StatusOK, web.StatusPending, web.StatusFailed, web.StatusOK}
	jobs := make([]web.Job, len(statuses))

	for i, st := range statuses {
		jobs[i] = web.Job{ID: fmt.Sprintf("job-%d", i), Name: "j", Status: st, Date: base.Add(time.Duration(i) * time.Minute)}
		jobs[i].Data.Run.Attempts = web.MaxJobAttempts
		jobs[i].Data.Run.Error = "boom"
	}

	if err := repo.CreateJobs(ctx, jobs, ""); err != nil {
		t.Fatal(err)
	}

	// Failed first: the two failed jobs (newest first), then the rest.
	page, err := svc.ListJobTable(ctx, "", "", "", web.JobOrderFailed, 1)
	if err != nil {
		t.Fatal(err)
	}

	if page.Jobs[0].ID != "job-4" || page.Jobs[1].ID != "job-1" || page.Jobs[2].Status == web.StatusFailed {
		t.Fatalf("failed first: %s, %s, %s", page.Jobs[0].ID, page.Jobs[1].ID, page.Jobs[2].Status)
	}

	// Only failed: nothing else, and the bulk "all of this view" ids agree.
	only, err := svc.ListJobTable(ctx, "", "", web.StatusFailed, "", 1)
	if err != nil || only.Total != 2 || len(only.Jobs) != 2 || only.Status != web.StatusFailed {
		t.Fatalf("only failed: total %d rows %d status %q err %v", only.Total, len(only.Jobs), only.Status, err)
	}

	ids, err := svc.JobIDsInFilter(ctx, "", web.StatusFailed)
	if err != nil || len(ids) != 2 {
		t.Fatalf("failed ids: %v, %v", ids, err)
	}

	// Run again: back in the queue as a fresh run.
	if err := svc.RetryJob(ctx, "job-1"); err != nil {
		t.Fatal(err)
	}

	got, err := svc.Get(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}

	if got.Status != web.StatusPending || got.Data.Run.Attempts != 0 || got.Data.Run.Error != "" {
		t.Fatalf("retried job: status %s attempts %d error %q", got.Status, got.Data.Run.Attempts, got.Data.Run.Error)
	}

	if err := svc.RetryJob(ctx, "job-0"); !errors.Is(err, web.ErrJobNotFailed) {
		t.Fatalf("retry of a completed job: %v", err)
	}
}

func TestBackupKeepsACopyOfTheDatabase(t *testing.T) {
	src := newTestRepo(t)
	ctx := context.Background()
	svc := web.NewService(src, t.TempDir())

	if err := src.CreateCampaign(ctx, &web.Campaign{ID: "camp", Name: "Camp", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	path, err := svc.BackupNow(ctx)
	if err != nil {
		t.Fatal(err)
	}

	db, err := initDatabase(path)
	if err != nil {
		t.Fatal(err)
	}

	defer db.Close()

	if _, err := (&repo{db: db}).GetCampaign(ctx, "camp"); err != nil {
		t.Fatalf("backup misses the campaign: %v", err)
	}

	if st := svc.BackupStatus(); !st.Enabled || st.Count != 1 || st.Last.IsZero() {
		t.Fatalf("status = %+v", st)
	}
}
