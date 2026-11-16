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

	svc := &Service{repo: store}

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

	send := func(_ context.Context, _ *MailConfig, to, _, _ string) error {
		got = append(got, to)
		if to == "user02@example.com" {
			return errors.New("smtp down")
		}

		return nil
	}

	svc.runDispatch("camp", "disp", &MailConfig{}, msgs, 3, 1, 0, send)

	if len(got) != n {
		t.Fatalf("sent calls %d, want %d", len(got), n)
	}

	for i := range msgs {
		if got[i] != msgs[i].To {
			t.Fatalf("order[%d] = %s, want %s", i, got[i], msgs[i].To)
		}
	}

	// 24 pauses: a short wait between messages, and the longer wait after each full 10.
	// Nothing waits after the last message.
	wantWaits := make([]time.Duration, 0, n-1)
	for i := 0; i < n-1; i++ {
		if (i+1)%mailBatchSize == 0 {
			wantWaits = append(wantWaits, batchGapMinWait)

			continue
		}

		wantWaits = append(wantWaits, intraBatchMinWait)
	}

	if len(details) != len(wantWaits) {
		t.Fatalf("pauses %d, want %d", len(details), len(wantWaits))
	}

	for i, detail := range details {
		want := "Waiting " + wantWaits[i].String()
		if !strings.Contains(detail, want) {
			t.Fatalf("pause %d detail %q, want %q", i, detail, want)
		}

		if (i+1)%mailBatchSize == 0 {
			if !strings.Contains(detail, "before the next batch") {
				t.Fatalf("pause %d should be a batch gap: %s", i, detail)
			}

			continue
		}

		if !strings.Contains(detail, "before the next message") {
			t.Fatalf("pause %d should be inside the batch: %s", i, detail)
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
