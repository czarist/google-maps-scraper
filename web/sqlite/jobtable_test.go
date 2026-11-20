package sqlite

import (
	"context"
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

	page, err := svc.ListJobTable(ctx, "", "", "", 1)
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
	last, err := svc.ListJobTable(ctx, "", "camp", "", 999)
	if err != nil {
		t.Fatal(err)
	}

	if last.Total != 1000 || last.CurrentPage != 100 || last.Selected == nil || last.Selected.JobCount != 1000 || last.Jobs[0].CampaignName != "Camp" {
		t.Fatalf("campaign filter: total %d page %d selected %+v", last.Total, last.CurrentPage, last.Selected)
	}

	none, err := svc.ListJobTable(ctx, "", "none", "", 1)
	if err != nil || none.Total != 4000 {
		t.Fatalf("no-campaign filter: %d, %v", none.Total, err)
	}

	ids, err := svc.JobIDsInFilter(ctx, "camp")
	if err != nil || len(ids) != 1000 {
		t.Fatalf("ids in filter: %d, %v", len(ids), err)
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
