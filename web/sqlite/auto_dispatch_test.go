package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gosom/google-maps-scraper/web"
)

func TestAutoDispatchSettingRoundTrip(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	if err := repo.CreateCampaign(ctx, &web.Campaign{ID: "camp", Name: "Camp", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	a, err := repo.GetAutoDispatch(ctx, "camp")
	if err != nil || a.Enabled || a.ResendAfterDays != 4 {
		t.Fatalf("default: %+v, %v", a, err)
	}

	want := web.AutoDispatch{Enabled: true, Resend: true, ResendAfterDays: 7}
	if err := repo.SaveAutoDispatch(ctx, "camp", want); err != nil {
		t.Fatal(err)
	}

	if got, _ := repo.GetAutoDispatch(ctx, "camp"); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	on, err := repo.EnabledAutoDispatch(ctx)
	if err != nil || len(on) != 1 || on["camp"] != want {
		t.Fatalf("enabled: %+v, %v", on, err)
	}
}

// One round sends only the new address: the sent one and the failed one are left out,
// and a round with nothing new records nothing in the history.
func TestAutoDispatchSendsOnlyNewAddresses(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	dir := t.TempDir()
	svc := web.NewService(repo, dir)
	svc.SetOptOut(allowAll{}, "https://example.test/descadastro")

	if err := repo.CreateCampaign(ctx, &web.Campaign{ID: "camp", Name: "Camp", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	createTestJob(t, repo, "job-1", time.Unix(10, 0))

	if err := repo.AssignJob(ctx, "camp", "job-1"); err != nil {
		t.Fatal(err)
	}

	csv := "title,emails\nCafe,\"sent@cafe.test, bad@cafe.test, new@cafe.test\"\n"
	if err := os.WriteFile(filepath.Join(dir, "job-1.csv"), []byte(csv), 0o600); err != nil {
		t.Fatal(err)
	}

	_ = repo.RecordMail(ctx, "camp", "sent@cafe.test", web.MailStatusSent, "")
	_ = repo.RecordMail(ctx, "camp", "bad@cafe.test", web.MailStatusFailed, "smtp down")

	cfg := readyMail()
	if err := svc.SaveCampaignMail(ctx, "camp", &cfg); err != nil {
		t.Fatal(err)
	}

	if err := svc.SaveAutoDispatch(ctx, "camp", web.AutoDispatch{Enabled: true, ResendAfterDays: 4}); err != nil {
		t.Fatal(err)
	}

	var sent []string

	sender := func(_ context.Context, _ *web.MailConfig, msg *web.OutgoingMail) error {
		sent = append(sent, msg.To)

		return nil
	}

	runRound := func() {
		svc.AutoDispatchOnce(ctx, sender)
		waitIdle(t, svc, "camp")
	}

	runRound()

	if len(sent) != 1 || sent[0] != "new@cafe.test" {
		t.Fatalf("sent: %v", sent)
	}

	runRound()

	rows, err := repo.ListDispatches(ctx, "camp", 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 1 || len(sent) != 1 {
		t.Fatalf("second round must do nothing: %d dispatches, sent %v", len(rows), sent)
	}
}
