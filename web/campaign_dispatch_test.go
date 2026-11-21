//nolint:testpackage // tests the unexported paced dispatch loop
package web

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRunDispatchPacesEveryAddress(t *testing.T) {
	const n = 25

	store := newMemStore()
	store.dispatches = []Dispatch{{
		ID:         "disp",
		CampaignID: "camp",
		Status:     DispatchRunning,
		StartedAt:  time.Now().UTC(),
	}}

	svc := &Service{repo: store, optOut: &fakeOptOut{}, unsubscribeBase: testUnsubscribeBase}

	var details []string

	svc.dispatchWait = func(lo, _ time.Duration) time.Duration {
		return lo
	}
	svc.dispatchSleep = func(context.Context, time.Duration) {
		store.mu.Lock()
		details = append(details, store.dispatches[0].Detail)
		store.mu.Unlock()
	}

	msgs := make([]renderedMail, n)
	for i := range msgs {
		msgs[i] = renderedMail{To: fmt.Sprintf("user%02d@example.com", i), Subject: "Hi", Body: "Body"}
	}

	var got []string

	send := func(_ context.Context, _ *MailConfig, msg *OutgoingMail) error {
		to := msg.To

		got = append(got, to)
		if to == "user02@example.com" {
			return errors.New("smtp down")
		}

		return nil
	}

	svc.runDispatch("camp", "disp", ServiceTrentin, &MailConfig{}, msgs, 3, 1, 0, send)

	if len(got) != n {
		t.Fatalf("sent calls %d, want %d", len(got), n)
	}

	for i := range msgs {
		if got[i] != msgs[i].To {
			t.Fatalf("order[%d] = %s, want %s", i, got[i], msgs[i].To)
		}
	}

	// A random pause between messages and the batch gap after each full batch.
	// Nothing waits after the last message.
	if len(details) != n-1 {
		t.Fatalf("pauses %d, want %d", len(details), n-1)
	}

	for i, detail := range details {
		want := "Waiting " + mailMinWait.Round(time.Second).String() + " before the next message"
		if (i+1)%mailBatchSize == 0 {
			want = "Waiting " + mailBatchMinGap.Round(time.Second).String() + " before the next batch"
		}

		if !strings.Contains(detail, want) {
			t.Fatalf("pause %d detail %q, want %q", i, detail, want)
		}
	}

	store.mu.Lock()
	final := store.dispatches[0]
	store.mu.Unlock()

	if final.Status != DispatchOK {
		t.Fatalf("status %s", final.Status)
	}

	if final.Sent != n-1 || final.Failed != 1 || final.Skipped != 3 {
		t.Fatalf("counts sent=%d failed=%d skipped=%d", final.Sent, final.Failed, final.Skipped)
	}

	if strings.Contains(final.Detail, "Waiting") {
		t.Fatalf("final detail still waiting: %s", final.Detail)
	}

	if store.sent["camp"]["user02@example.com"] != MailStatusFailed {
		t.Fatalf("failed address status %q", store.sent["camp"]["user02@example.com"])
	}

	if store.sent["camp"]["user00@example.com"] != MailStatusSent {
		t.Fatalf("sent address status %q", store.sent["camp"]["user00@example.com"])
	}
}

func TestRunDispatchStopsOnProviderBlock(t *testing.T) {
	store := newMemStore()
	store.dispatches = []Dispatch{{
		ID:         "disp",
		CampaignID: "camp",
		Status:     DispatchRunning,
		StartedAt:  time.Now().UTC(),
	}}

	svc := &Service{repo: store, optOut: &fakeOptOut{}, unsubscribeBase: testUnsubscribeBase}
	svc.dispatchSleep = func(context.Context, time.Duration) {}

	msgs := []renderedMail{
		{To: "a@example.com", Subject: "Hi", Body: "Body"},
		{To: "b@example.com", Subject: "Hi", Body: "Body"},
		{To: "c@example.com", Subject: "Hi", Body: "Body"},
	}

	attempts := 0

	send := func(_ context.Context, _ *MailConfig, msg *OutgoingMail) error {
		attempts++

		if msg.To == "b@example.com" {
			return errors.New(`550 5.4.6 Unusual sending activity detected. UnblockMe`)
		}

		return nil
	}

	svc.runDispatch("camp", "disp", ServiceTrentin, &MailConfig{}, msgs, 0, 0, 0, send)

	if attempts != 2 {
		t.Fatalf("attempts %d, want 2", attempts)
	}

	if store.sent["camp"]["a@example.com"] != MailStatusSent {
		t.Fatalf("first status %q", store.sent["camp"]["a@example.com"])
	}

	if _, ok := store.sent["camp"]["b@example.com"]; ok {
		t.Fatal("blocked address was recorded")
	}

	if _, ok := store.sent["camp"]["c@example.com"]; ok {
		t.Fatal("address after the block was recorded")
	}

	store.mu.Lock()
	final := store.dispatches[0]
	store.mu.Unlock()

	if final.Status != DispatchFailed {
		t.Fatalf("status %s", final.Status)
	}

	if !strings.Contains(final.Detail, "stay queued") || !providerBlockText(final.Detail) {
		t.Fatalf("detail %s", final.Detail)
	}
}

func TestLiveDispatchIsNotMarkedInterrupted(t *testing.T) {
	store := newMemStore()
	id := "11111111-1111-1111-1111-111111111111"
	store.dispatches = []Dispatch{{
		ID:         "d1",
		CampaignID: id,
		Status:     DispatchRunning,
		StartedAt:  time.Now().Add(-3 * time.Hour).UTC(),
	}}

	svc := &Service{
		repo:        store,
		dispatching: map[string]struct{}{id: {}},
	}

	if err := svc.expireStaleDispatch(context.Background(), store, id); err != nil {
		t.Fatalf("expire: %v", err)
	}

	if store.dispatches[0].Status != DispatchRunning {
		t.Fatalf("live run status %s", store.dispatches[0].Status)
	}

	running, err := svc.DispatchRunning(context.Background(), id)
	if err != nil || !running {
		t.Fatalf("running %v err %v", running, err)
	}

	dead := &Service{repo: store}
	if err := dead.expireStaleDispatch(context.Background(), store, id); err != nil {
		t.Fatalf("expire dead: %v", err)
	}

	if store.dispatches[0].Status != DispatchFailed || store.dispatches[0].Detail != "interrupted" {
		t.Fatalf("dead run %+v", store.dispatches[0])
	}
}
