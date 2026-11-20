package sqlite

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gosom/google-maps-scraper/web"
)

func TestBulkCreateMoveAndDelete(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	for _, id := range []string{"a", "b"} {
		if err := repo.CreateCampaign(ctx, &web.Campaign{ID: id, Name: id, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}

	jobs := make([]web.Job, 500)
	ids := make([]string, len(jobs))

	for i := range jobs {
		ids[i] = fmt.Sprintf("job-%03d", i)
		jobs[i] = web.Job{ID: ids[i], Name: ids[i], Status: web.StatusPending, Date: time.Now().Add(time.Duration(i) * time.Millisecond)}
	}

	start := time.Now()
	if err := repo.CreateJobs(ctx, jobs, "a"); err != nil {
		t.Fatal(err)
	}

	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("500 jobs took %s", took)
	}

	inA, _ := repo.CampaignJobs(ctx, "a")
	if len(inA) != 500 {
		t.Fatalf("campaign a has %d jobs", len(inA))
	}

	// Move half to b, take 10 out of any campaign.
	if err := repo.AssignJobs(ctx, "b", ids[:250]); err != nil {
		t.Fatal(err)
	}

	if err := repo.AssignJobs(ctx, "", ids[250:260]); err != nil {
		t.Fatal(err)
	}

	inA, _ = repo.CampaignJobs(ctx, "a")
	inB, _ := repo.CampaignJobs(ctx, "b")

	if len(inA) != 240 || len(inB) != 250 {
		t.Fatalf("a=%d b=%d, want 240 and 250", len(inA), len(inB))
	}

	if err := repo.AssignJobs(ctx, "missing", ids[:1]); err == nil {
		t.Fatal("moving to an unknown campaign must fail")
	}

	if err := repo.DeleteJobs(ctx, ids[:300]); err != nil {
		t.Fatal(err)
	}

	left, err := repo.Select(ctx, web.SelectParams{})
	if err != nil || len(left) != 200 {
		t.Fatalf("left %d jobs, %v", len(left), err)
	}

	inB, _ = repo.CampaignJobs(ctx, "b")
	if len(inB) != 0 {
		t.Fatalf("deleted jobs still in campaign b: %d", len(inB))
	}
}

func TestRunQueuePutsRetriesAfterNewJobs(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	retry := web.Job{ID: "old-retry", Name: "r", Status: web.StatusPending, Date: time.Now().Add(-time.Hour)}
	retry.Data.Run.Attempts = 1
	fresh := web.Job{ID: "new", Name: "n", Status: web.StatusPending, Date: time.Now()}

	for _, j := range []*web.Job{&retry, &fresh} {
		if err := repo.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}

	got, err := repo.Select(ctx, web.SelectParams{Status: web.StatusPending, Limit: 10, OldestFirst: true})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 || got[0].ID != "new" || got[1].ID != "old-retry" {
		t.Fatalf("queue order: %v, %v", got, err)
	}
}
