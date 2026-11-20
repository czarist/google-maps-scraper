package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

func TestDispatchSkipsAddressesReachedBySameServiceCampaign(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	dir := t.TempDir()
	svc := web.NewService(repo, dir)
	svc.SetOptOut(allowAll{}, "https://example.test/descadastro")

	for _, id := range []string{"first", "camp"} {
		if err := repo.CreateCampaign(ctx, &web.Campaign{ID: id, Name: id, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}

	createTestJob(t, repo, "job-1", time.Unix(10, 0))

	if err := repo.AssignJob(ctx, "camp", "job-1"); err != nil {
		t.Fatal(err)
	}

	csv := "title,emails\nCafe,\"a@cafe.test, b@cafe.test\"\n"
	if err := os.WriteFile(filepath.Join(dir, "job-1.csv"), []byte(csv), 0o600); err != nil {
		t.Fatal(err)
	}

	// The other campaign of the same service already reached a@.
	if err := repo.RecordMail(ctx, "first", "a@cafe.test", web.MailStatusSent, ""); err != nil {
		t.Fatal(err)
	}

	cfg := readyMail()
	if err := svc.SaveCampaignMail(ctx, "camp", &cfg); err != nil {
		t.Fatal(err)
	}

	var sent []string

	sender := func(_ context.Context, _ *web.MailConfig, msg *web.OutgoingMail) error {
		sent = append(sent, msg.To)

		return nil
	}

	if _, err := svc.StartDispatch(ctx, "camp", false, web.DefaultResendAfter, sender); err != nil {
		t.Fatal(err)
	}

	waitDispatch(t, repo, "camp")

	if len(sent) != 1 || sent[0] != "b@cafe.test" {
		t.Fatalf("sent %v, want only b@cafe.test", sent)
	}
}

func TestCampaignLeadsMergesThePlaceFoundByTwoJobs(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	dir := t.TempDir()
	svc := web.NewService(repo, dir)

	if err := repo.CreateCampaign(ctx, &web.Campaign{ID: "camp", Name: "Camp", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	head := "title,phone,emails,website_phones,whatsapp,place_id\n"
	files := map[string]string{
		"job-1": head + "Padaria A,(51) 3013-4545,a@x.com,,,P1\nPadaria B,(51) 99999-0000,,,,P2\n",
		"job-2": head + "Padaria A,,a2@x.com,(51) 98888-1111,,P1\n",
	}

	for id, body := range files {
		createTestJob(t, repo, id, time.Unix(10, 0))

		if err := repo.AssignJob(ctx, "camp", id); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(dir, id+".csv"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	leads, err := svc.CampaignLeads(ctx, "camp")
	if err != nil {
		t.Fatal(err)
	}

	if len(leads) != 2 {
		t.Fatalf("got %d leads, want 2 (Padaria A merged)", len(leads))
	}

	a, b := leads[0], leads[1]
	if a.Name != "Padaria A" || len(a.Emails) != 2 || a.Phone != "(51) 3013-4545" || len(a.ProbableWhatsApp) != 1 {
		t.Fatalf("merged lead = %+v", a)
	}

	if len(b.ProbableWhatsApp) != 1 || b.ProbableWhatsApp[0] != "(51) 99999-0000" {
		t.Fatalf("mobile not flagged: %+v", b)
	}
}
