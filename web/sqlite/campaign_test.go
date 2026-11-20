//nolint:testpackage // tests the private SQLite repository and schema directly
package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gosom/google-maps-scraper/web"
)

func TestCampaignAssignMovesJobAndDeleteKeepsIt(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	createTestJob(t, repo, "job-1", time.Unix(10, 0))
	first := &web.Campaign{ID: "camp-a", Name: "A", CreatedAt: time.Unix(20, 0)}
	second := &web.Campaign{ID: "camp-b", Name: "B", CreatedAt: time.Unix(30, 0)}

	if err := repo.CreateCampaign(ctx, first); err != nil {
		t.Fatalf("create a: %v", err)
	}

	if err := repo.CreateCampaign(ctx, second); err != nil {
		t.Fatalf("create b: %v", err)
	}

	if err := repo.AssignJob(ctx, first.ID, "job-1"); err != nil {
		t.Fatalf("assign a: %v", err)
	}

	if err := repo.AssignJob(ctx, second.ID, "job-1"); err != nil {
		t.Fatalf("assign b: %v", err)
	}

	left, err := repo.CampaignJobs(ctx, first.ID)
	if err != nil {
		t.Fatalf("jobs a: %v", err)
	}

	if len(left) != 0 {
		t.Fatalf("job still in a: %#v", left)
	}

	got, err := repo.CampaignJobs(ctx, second.ID)
	if err != nil || len(got) != 1 || got[0] != "job-1" {
		t.Fatalf("jobs b: %#v %v", got, err)
	}

	if err := repo.DeleteCampaign(ctx, second.ID); err != nil {
		t.Fatalf("delete campaign: %v", err)
	}

	job, err := repo.Get(ctx, "job-1")
	if err != nil || job.ID != "job-1" {
		t.Fatalf("job after delete: %#v %v", job, err)
	}
}

func TestSaveMailKeepsPasswordWhenBlank(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	svc := web.NewService(repo, t.TempDir())

	if err := repo.CreateCampaign(ctx, &web.Campaign{ID: "camp", Name: "Camp", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("create: %v", err)
	}

	cfg := readyMail()
	if err := svc.SaveCampaignMail(ctx, "camp", &cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	cfg.Password = ""
	cfg.Format = web.MailFormatHTML

	cfg.Host = "smtp.other.test"
	if err := svc.SaveCampaignMail(ctx, "camp", &cfg); err != nil {
		t.Fatalf("save blank password: %v", err)
	}

	stored, ok, err := repo.GetMail(ctx, "camp")
	if err != nil || !ok {
		t.Fatalf("get mail: ok=%v %v", ok, err)
	}

	if stored.Password != "stored-secret" || stored.Host != "smtp.other.test" || stored.Format != web.MailFormatHTML {
		t.Fatalf("stored: host %q password %q format %q", stored.Host, stored.Password, stored.Format)
	}

	form, set, err := svc.CampaignMailForForm(ctx, "camp")
	if err != nil || !set || form.Password != "" {
		t.Fatalf("form leaked password: set=%v password=%q err=%v", set, form.Password, err)
	}
}

func TestDispatchSkipsSentAddresses(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	dir := t.TempDir()
	svc := web.NewService(repo, dir)
	svc.SetOptOut(allowAll{}, "https://example.test/descadastro")

	if err := repo.CreateCampaign(ctx, &web.Campaign{ID: "camp", Name: "Camp", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("create: %v", err)
	}

	createTestJob(t, repo, "job-1", time.Unix(10, 0))
	createTestJob(t, repo, "job-2", time.Unix(11, 0))

	if err := repo.AssignJob(ctx, "camp", "job-1"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	if err := repo.AssignJob(ctx, "camp", "job-2"); err != nil {
		t.Fatalf("assign missing file: %v", err)
	}

	csv := "title,emails,phone,website,address,category\nCafe,\"a@cafe.test, b@cafe.test\",555,https://cafe.test,1 Main,Cafe\n"
	if err := os.WriteFile(filepath.Join(dir, "job-1.csv"), []byte(csv), 0o600); err != nil {
		t.Fatalf("csv: %v", err)
	}

	if err := repo.RecordMail(ctx, "camp", "a@cafe.test", web.MailStatusSent, ""); err != nil {
		t.Fatalf("record: %v", err)
	}

	cfg := readyMail()
	if err := svc.SaveCampaignMail(ctx, "camp", &cfg); err != nil {
		t.Fatalf("mail: %v", err)
	}

	var sent []string

	sender := func(_ context.Context, _ *web.MailConfig, msg *web.OutgoingMail) error {
		sent = append(sent, msg.To+"|"+msg.Subject+"|"+msg.Body)

		return nil
	}

	result, err := svc.StartDispatch(ctx, "camp", false, web.DefaultResendAfter, sender)
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	if !result.Started {
		t.Fatalf("result: %#v", result)
	}

	waitDispatch(t, repo, "camp")

	if len(sent) != 1 || !strings.Contains(sent[0], "b@cafe.test") || strings.Contains(sent[0], "a@cafe.test") {
		t.Fatalf("sent: %#v", sent)
	}

	if !strings.Contains(sent[0], "Hello Cafe") || !strings.Contains(sent[0], "Hi b@cafe.test") {
		t.Fatalf("template: %#v", sent)
	}
}

func TestAddMailFormatColumnIsIdempotent(t *testing.T) {
	repo := newTestRepo(t)

	if _, err := repo.db.Exec(`ALTER TABLE campaign_mail DROP COLUMN format`); err != nil {
		t.Fatalf("drop format: %v", err)
	}

	if err := addMailFormatColumn(repo.db); err != nil {
		t.Fatalf("add format: %v", err)
	}

	if err := addMailFormatColumn(repo.db); err != nil {
		t.Fatalf("add format again: %v", err)
	}
}

func readyMail() web.MailConfig {
	return web.MailConfig{
		Host:       "smtp.test",
		Port:       587,
		Username:   "user",
		Password:   "stored-secret",
		FromEmail:  "from@cafe.test",
		FromName:   "Cafe",
		Subject:    "Hello {{.Title}}",
		Body:       "Hi {{.Email}}",
		Encryption: web.MailStartTLS,
	}
}

func waitDispatch(t *testing.T, repo *repo, campaignID string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		rows, err := repo.ListDispatches(context.Background(), campaignID, 5)
		if err != nil {
			t.Fatalf("list: %v", err)
		}

		if len(rows) > 0 && rows[0].Status != web.DispatchRunning {
			if rows[0].Sent != 1 || rows[0].Skipped != 1 {
				t.Fatalf("dispatch: %#v", rows[0])
			}

			return
		}

		time.Sleep(15 * time.Millisecond)
	}

	t.Fatal("dispatch did not finish")
}

// allowAll is an unsubscribe list where nobody opted out.
type allowAll struct{}

func (allowAll) OptedOut(context.Context, string, string) (bool, error) { return false, nil }

func (allowAll) ServiceExists(context.Context, string) (bool, error) { return true, nil }

func TestRequeueFailedForgetsOnlyFailedSends(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	if err := repo.CreateCampaign(ctx, &web.Campaign{ID: "camp", Name: "Camp", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("create: %v", err)
	}

	for email, status := range map[string]string{
		"bad1@x.test": web.MailStatusFailed,
		"bad2@x.test": web.MailStatusFailed,
		"ok@x.test":   web.MailStatusSent,
		"out@x.test":  web.MailStatusUnsubscribed,
	} {
		if err := repo.RecordMail(ctx, "camp", email, status, ""); err != nil {
			t.Fatal(err)
		}
	}

	n, err := repo.RequeueFailed(ctx, "camp")
	if err != nil || n != 2 {
		t.Fatalf("RequeueFailed = %d, %v; want 2, nil", n, err)
	}

	log, err := repo.MailLog(ctx, "camp")
	if err != nil {
		t.Fatal(err)
	}

	if len(log) != 2 {
		t.Fatalf("mail log after requeue = %v, want the sent and unsubscribed rows", log)
	}

	for _, e := range log {
		if e.Status == web.MailStatusFailed {
			t.Fatalf("failed row %s still in the log", e.Email)
		}
	}
}

func TestSentEmailsSinceUsesTheResendWindow(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	if err := repo.CreateCampaign(ctx, &web.Campaign{ID: "camp", Name: "Camp", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("create: %v", err)
	}

	for _, e := range []string{"old@x.test", "new@x.test"} {
		if err := repo.RecordMail(ctx, "camp", e, web.MailStatusSent, ""); err != nil {
			t.Fatal(err)
		}
	}

	if err := repo.RecordMail(ctx, "camp", "out@x.test", web.MailStatusUnsubscribed, ""); err != nil {
		t.Fatal(err)
	}

	fiveDaysAgo := time.Now().Add(-5 * 24 * time.Hour).Unix()
	if _, err := repo.db.Exec(`UPDATE campaign_mail_log SET sent_at = ? WHERE email IN ('old@x.test', 'out@x.test')`, fiveDaysAgo); err != nil {
		t.Fatal(err)
	}

	// Window of 4 days: "old" got it 5 days ago, so it may be sent again.
	got, err := repo.SentEmailsSince(ctx, "camp", time.Now().Add(-web.DefaultResendAfter))
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := got["old@x.test"]; ok {
		t.Fatalf("old address must be resendable: %v", got)
	}

	for _, e := range []string{"new@x.test", "out@x.test"} {
		if _, ok := got[e]; !ok {
			t.Fatalf("%s must be skipped: %v", e, got)
		}
	}
}
