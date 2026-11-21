package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/gosom/google-maps-scraper/web"
)

func TestSMTPAccountRoundTrip(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	if err := repo.CreateCampaign(ctx, &web.Campaign{ID: "camp", Name: "Camp", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	first := &web.SMTPAccount{
		ID: "a", Host: "smtp.one.test", Port: 465, Username: "one", Password: "secret-a",
		Encryption: web.MailSSL, FromEmail: "one@cafe.test", FromName: "One",
	}
	second := &web.SMTPAccount{
		ID: "b", Host: "smtp.two.test", Port: 587, Username: "two", Password: "secret-b",
		Encryption: web.MailStartTLS, FromEmail: "two@cafe.test",
	}

	if err := repo.InsertSMTPAccount(ctx, "camp", first); err != nil {
		t.Fatal(err)
	}

	if err := repo.InsertSMTPAccount(ctx, "camp", second); err != nil {
		t.Fatal(err)
	}

	got, err := repo.ListSMTPAccounts(ctx, "camp")
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 || got[0].ID != "a" || got[0].Password != "secret-a" || got[1].ID != "b" || got[1].Host != "smtp.two.test" {
		t.Fatalf("list %+v", got)
	}

	first.Host = "smtp.one-b.test"
	first.FromName = "One B"
	first.Password = "secret-a2"
	if err := repo.UpdateSMTPAccount(ctx, "camp", first); err != nil {
		t.Fatal(err)
	}

	got, err = repo.ListSMTPAccounts(ctx, "camp")
	if err != nil || len(got) != 2 || got[0].Host != "smtp.one-b.test" || got[0].Password != "secret-a2" || got[0].FromName != "One B" || got[1].ID != "b" {
		t.Fatalf("after update %+v %v", got, err)
	}

	if err := repo.UpdateSMTPAccount(ctx, "camp", &web.SMTPAccount{ID: "missing"}); err == nil {
		t.Fatal("missing server was updated")
	}

	if err := repo.DeleteSMTPAccount(ctx, "camp", "a"); err != nil {
		t.Fatal(err)
	}

	got, err = repo.ListSMTPAccounts(ctx, "camp")
	if err != nil || len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("after delete %+v, %v", got, err)
	}

	if err := repo.DeleteSMTPAccount(ctx, "camp", "missing"); err == nil {
		t.Fatal("missing server was removed")
	}

	if err := repo.DeleteCampaign(ctx, "camp"); err != nil {
		t.Fatal(err)
	}

	got, err = repo.ListSMTPAccounts(ctx, "camp")
	if err != nil || len(got) != 0 {
		t.Fatalf("after campaign delete %+v, %v", got, err)
	}
}
