//nolint:testpackage // tests the unexported dispatch loop controls
package web

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestPauseResumeDispatchUsesNewSMTPSettings(t *testing.T) {
	store := newMemStore()
	store.dispatches = []Dispatch{{ID: "disp", CampaignID: "camp", Status: DispatchRunning, StartedAt: time.Now().UTC()}}

	svc := &Service{repo: store, optOut: &fakeOptOut{}, unsubscribeBase: testUnsubscribeBase}
	svc.dispatchWait = func(lo, _ time.Duration) time.Duration { return lo }
	svc.dispatchSleep = func(context.Context, time.Duration) {}

	var (
		mu    sync.Mutex
		hosts []string
	)

	pausedAfterFirst := make(chan struct{})

	send := func(_ context.Context, cfg *MailConfig, _ *OutgoingMail) error {
		mu.Lock()
		hosts = append(hosts, cfg.Host)
		first := len(hosts) == 1
		mu.Unlock()

		if first {
			if err := svc.PauseDispatch("camp"); err != nil {
				t.Errorf("PauseDispatch: %v", err)
			}

			close(pausedAfterFirst)
		}

		return nil
	}

	msgs := []renderedMail{
		{To: "a@example.com", Subject: "Hi", Body: "Body"},
		{To: "b@example.com", Subject: "Hi", Body: "Body"},
	}

	done := make(chan struct{})

	go func() {
		svc.runDispatch("camp", "disp", ServiceTrentin, &MailConfig{Host: "old.example.com"}, msgs, 0, 0, 0, send, 1)
		close(done)
	}()

	<-pausedAfterFirst

	// The run must stay paused: the second message is not sent.
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	sentWhilePaused := len(hosts)
	mu.Unlock()

	if sentWhilePaused != 1 {
		t.Fatalf("sent %d messages while paused, want 1", sentWhilePaused)
	}

	if !svc.DispatchPaused("camp") {
		t.Fatal("DispatchPaused = false while paused")
	}

	newCfg := MailConfig{
		Host: "new.example.com", Port: 587, Password: "secret",
		FromEmail: "me@example.com", Subject: "Hi", Body: "Body",
	}
	if err := store.SaveMail(context.Background(), "camp", &newCfg); err != nil {
		t.Fatal(err)
	}

	if err := svc.ResumeDispatch("camp"); err != nil {
		t.Fatalf("ResumeDispatch: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not finish after resume")
	}

	if len(hosts) != 2 || hosts[0] != "old.example.com" || hosts[1] != "new.example.com" {
		t.Fatalf("hosts = %v, want [old.example.com new.example.com]", hosts)
	}

	if err := svc.PauseDispatch("camp"); !errors.Is(err, ErrNoDispatch) {
		t.Fatalf("PauseDispatch after finish = %v, want ErrNoDispatch", err)
	}
}

func TestKeepFailedKeepsOnlyLastFailedAddresses(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()

	for email, status := range map[string]string{
		"failed@example.com": MailStatusFailed,
		"sent@example.com":   MailStatusSent,
	} {
		if err := store.RecordMail(ctx, "camp", email, status, ""); err != nil {
			t.Fatal(err)
		}
	}

	recs := []Recipient{
		{Email: "Failed@example.com"},
		{Email: "failed@example.com"},
		{Email: "sent@example.com"},
		{Email: "new@example.com"},
	}

	got, err := keepFailed(ctx, store, "camp", recs)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0].Email != "Failed@example.com" {
		t.Fatalf("keepFailed = %v, want only the failed address once", got)
	}
}
