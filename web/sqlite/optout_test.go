//nolint:testpackage // uses the private SQLite repository and schema directly
package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gosom/google-maps-scraper/web"
)

// optOutList is an unsubscribe list in memory. Keys are "email|service".
type optOutList struct {
	mu  sync.Mutex
	out map[string]bool
}

func (l *optOutList) OptedOut(_ context.Context, email, service string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.out[email+"|"+service], nil
}

func (l *optOutList) ServiceExists(context.Context, string) (bool, error) { return true, nil }

func TestRetryAndResendPassTheUnsubscribeCheck(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	dir := t.TempDir()
	svc := web.NewService(repo, dir)

	list := &optOutList{out: map[string]bool{
		"a@cafe.test|trentin": true,
		"b@cafe.test|trentin": true,
	}}
	svc.SetOptOut(list, "https://example.test/descadastro")

	if err := svc.CreateCampaign(ctx, &web.Campaign{ID: "camp", Name: "Camp"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	createTestJob(t, repo, "job-1", time.Unix(10, 0))

	if err := repo.AssignJob(ctx, "camp", "job-1"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	csv := "title,emails\nCafe,\"a@cafe.test, b@cafe.test\"\n"
	if err := os.WriteFile(filepath.Join(dir, "job-1.csv"), []byte(csv), 0o600); err != nil {
		t.Fatalf("csv: %v", err)
	}

	// a failed before and is retried. b was sent before the opt-out.
	if err := repo.RecordMail(ctx, "camp", "a@cafe.test", web.MailStatusFailed, "smtp down"); err != nil {
		t.Fatalf("record: %v", err)
	}

	if err := repo.RecordMail(ctx, "camp", "b@cafe.test", web.MailStatusSent, ""); err != nil {
		t.Fatalf("record: %v", err)
	}

	cfg := readyMail()
	if err := svc.SaveCampaignMail(ctx, "camp", &cfg); err != nil {
		t.Fatalf("mail: %v", err)
	}

	var sent []string

	sender := func(_ context.Context, _ *web.MailConfig, msg *web.OutgoingMail) error {
		sent = append(sent, msg.To)

		return nil
	}

	for _, resend := range []bool{false, true} {
		if _, err := svc.StartDispatch(ctx, "camp", resend, 0, sender); err != nil {
			t.Fatalf("start resend=%v: %v", resend, err)
		}

		waitIdle(t, svc, "camp")
	}

	if len(sent) != 0 {
		t.Fatalf("retry or resend reached the provider: %v", sent)
	}

	log, err := repo.MailLog(ctx, "camp")
	if err != nil {
		t.Fatalf("log: %v", err)
	}

	for _, e := range log {
		if e.Status != web.MailStatusUnsubscribed || e.Error != "descadastro:trentin" {
			t.Fatalf("log entry %+v", e)
		}
	}
}

func TestCampaignServiceColumnDefaultsToTrentin(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	if _, err := repo.db.Exec(`INSERT INTO campaigns (id, name, created_at, updated_at) VALUES ('old', 'Old', 1, 1)`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	c, err := repo.GetCampaign(ctx, "old")
	if err != nil || c.ServiceName != web.ServiceTrentin {
		t.Fatalf("campaign %+v err %v", c, err)
	}

	if err := ensureServiceSchema(repo.db); err != nil {
		t.Fatalf("schema again: %v", err)
	}
}

func TestServiceCRUD(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()
	svc := web.NewService(repo, t.TempDir())
	svc.SetOptOut(&optOutList{}, "https://example.test/descadastro")

	services, err := svc.ListServices(ctx)
	if err != nil || len(services) != len(web.DefaultServices) {
		t.Fatalf("defaults %v err %v", services, err)
	}

	if err := svc.CreateService(ctx, &web.MailService{Name: "novo", Label: "Novo"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := svc.UpdateService(ctx, &web.MailService{Name: "novo", Label: "Novo serviço"}); err != nil {
		t.Fatalf("update: %v", err)
	}

	if err := svc.CreateCampaign(ctx, &web.Campaign{ID: "camp", Name: "Camp", ServiceName: "novo"}); err != nil {
		t.Fatalf("campaign: %v", err)
	}

	if err := svc.DeleteService(ctx, "novo"); !errors.Is(err, web.ErrServiceInUse) {
		t.Fatalf("delete in use: %v", err)
	}

	// Skips recorded under the old service are cleared when the service changes.
	if err := repo.RecordMail(ctx, "camp", "x@cafe.test", web.MailStatusUnsubscribed, "descadastro:novo"); err != nil {
		t.Fatalf("record: %v", err)
	}

	if err := svc.SetCampaignService(ctx, "camp", "caderna"); err != nil {
		t.Fatalf("set service: %v", err)
	}

	if done, _ := repo.SentEmails(ctx, "camp"); len(done) != 0 {
		t.Fatalf("old unsubscribe skips kept: %v", done)
	}

	if err := svc.SetCampaignService(ctx, "camp", "inexistente"); !errors.Is(err, web.ErrInvalidService) {
		t.Fatalf("unknown service: %v", err)
	}

	if err := svc.DeleteService(ctx, "novo"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := repo.GetService(ctx, "novo"); !errors.Is(err, web.ErrNotFound) {
		t.Fatalf("still there: %v", err)
	}
}

func waitIdle(t *testing.T, svc *web.Service, campaignID string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		running, err := svc.DispatchRunning(context.Background(), campaignID)
		if err != nil {
			t.Fatalf("running: %v", err)
		}

		if !running {
			return
		}

		time.Sleep(15 * time.Millisecond)
	}

	t.Fatal("dispatch did not finish")
}
