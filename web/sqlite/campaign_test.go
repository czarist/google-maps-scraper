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

	sender := func(_ context.Context, _ *web.MailConfig, to, subject, body string) error {
		sent = append(sent, to+"|"+subject+"|"+body)

		return nil
	}

	result, err := svc.StartDispatch(ctx, "camp", false, sender)
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
