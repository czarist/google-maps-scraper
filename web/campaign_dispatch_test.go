//nolint:testpackage // tests the unexported paced dispatch loop
package web

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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

	svc.runDispatch("camp", "disp", ServiceTrentin, &MailConfig{}, msgs, 3, 1, 0, send, 1)

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

	svc.runDispatch("camp", "disp", ServiceTrentin, &MailConfig{}, msgs, 0, 0, 0, send, 1)

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

func TestRunDispatchRotatesSMTPServers(t *testing.T) {
	store := newMemStore()
	store.dispatches = []Dispatch{{
		ID: "disp", CampaignID: "camp", Status: DispatchRunning, StartedAt: time.Now().UTC(),
	}}
	store.smtp = map[string][]SMTPAccount{
		"camp": {{
			ID: "extra", Host: "two.example", Port: 465, Username: "two", Password: "secret-2",
			Encryption: MailSSL, FromEmail: "two@example.com", FromName: "Two",
		}},
	}

	svc := &Service{repo: store, optOut: &fakeOptOut{}, unsubscribeBase: testUnsubscribeBase}
	svc.dispatchSleep = func(context.Context, time.Duration) {}

	msgs := make([]renderedMail, 4)
	for i := range msgs {
		msgs[i] = renderedMail{To: fmt.Sprintf("user%02d@example.com", i), Subject: "Same", Body: "Same body"}
	}

	var mu sync.Mutex

	got := map[string]string{}
	changed := ""

	send := func(_ context.Context, cfg *MailConfig, msg *OutgoingMail) error {
		mu.Lock()
		defer mu.Unlock()

		if msg.Subject != "Same" || !strings.HasPrefix(msg.Body, "Same body") {
			changed = msg.To
		}

		got[msg.To] = cfg.Host + " " + cfg.FromEmail

		return nil
	}

	primary := MailConfig{Host: "one.example", FromEmail: "one@example.com", FromName: "One", Password: "secret-1"}
	svc.runDispatch("camp", "disp", ServiceTrentin, &primary, msgs, 0, 0, 0, send, 2)

	if changed != "" {
		t.Fatalf("message changed for %s", changed)
	}

	want := map[string]string{
		"user00@example.com": "one.example one@example.com",
		"user01@example.com": "two.example two@example.com",
		"user02@example.com": "one.example one@example.com",
		"user03@example.com": "two.example two@example.com",
	}

	for to, host := range want {
		if got[to] != host {
			t.Fatalf("%s via %q, want %q", to, got[to], host)
		}
	}
}

func TestRunDispatchSkipsSenderThatRejectsLogin(t *testing.T) {
	store := newMemStore()
	store.dispatches = []Dispatch{{
		ID: "disp", CampaignID: "camp", Status: DispatchRunning, StartedAt: time.Now().UTC(),
	}}
	store.smtp = map[string][]SMTPAccount{
		"camp": {{
			ID: "extra", Host: "uh.example", Port: 465, Username: "uh", Password: "secret-2",
			Encryption: MailSSL, FromEmail: "two@example.com",
		}},
	}

	svc := &Service{repo: store, optOut: &fakeOptOut{}, unsubscribeBase: testUnsubscribeBase}
	svc.dispatchSleep = func(context.Context, time.Duration) {}

	msgs := make([]renderedMail, 4)
	for i := range msgs {
		msgs[i] = renderedMail{To: fmt.Sprintf("user%02d@example.com", i), Subject: "Same", Body: "Same body"}
	}

	var mu sync.Mutex

	calls := map[string]int{}

	send := func(_ context.Context, cfg *MailConfig, msg *OutgoingMail) error {
		mu.Lock()
		defer mu.Unlock()

		calls[cfg.Host]++

		if cfg.Host == "uh.example" {
			return errors.New(`535 5.7.8 Error: authentication failed`)
		}

		if msg.To == "" || cfg.FromEmail != "one@example.com" {
			t.Errorf("sent %s via %s", msg.To, cfg.FromEmail)
		}

		return nil
	}

	primary := MailConfig{Host: "one.example", Username: "one", FromEmail: "one@example.com", Password: "secret-1"}
	svc.runDispatch("camp", "disp", ServiceTrentin, &primary, msgs, 0, 0, 0, send, 1)

	if calls["uh.example"] != 1 {
		t.Fatalf("rejected sender was tried %d times", calls["uh.example"])
	}

	if calls["one.example"] != 4 {
		t.Fatalf("working sender calls %d", calls["one.example"])
	}

	for i := range msgs {
		if store.sent["camp"][msgs[i].To] != MailStatusSent {
			t.Fatalf("%s status %q", msgs[i].To, store.sent["camp"][msgs[i].To])
		}
	}

	log, err := store.SenderLog(context.Background(), "camp")
	if err != nil {
		t.Fatal(err)
	}

	failed := 0
	sent := 0

	for _, entry := range log {
		switch {
		case entry.FromEmail == "two@example.com" && entry.Status == MailStatusFailed:
			failed++
		case entry.FromEmail == "one@example.com" && entry.Status == MailStatusSent:
			sent++
		default:
			t.Fatalf("unexpected attempt %+v", entry)
		}
	}

	if failed != 1 || sent != 4 {
		t.Fatalf("sender log failed %d sent %d", failed, sent)
	}
}

func TestRunDispatchKeepsSenderAfterTemporaryAuthFailure(t *testing.T) {
	store := newMemStore()
	store.dispatches = []Dispatch{{
		ID: "disp", CampaignID: "camp", Status: DispatchRunning, StartedAt: time.Now().UTC(),
	}}
	store.smtp = map[string][]SMTPAccount{
		"camp": {{
			ID: "extra", Host: "uh.example", Port: 465, Username: "uh", Password: "secret-2",
			Encryption: MailSSL, FromEmail: "two@example.com",
		}},
	}

	svc := &Service{repo: store, optOut: &fakeOptOut{}, unsubscribeBase: testUnsubscribeBase}
	svc.dispatchSleep = func(context.Context, time.Duration) {}

	msgs := make([]renderedMail, 4)
	for i := range msgs {
		msgs[i] = renderedMail{To: fmt.Sprintf("user%02d@example.com", i), Subject: "Same", Body: "Same body"}
	}

	var mu sync.Mutex

	uhCalls := 0

	send := func(_ context.Context, cfg *MailConfig, _ *OutgoingMail) error {
		mu.Lock()
		defer mu.Unlock()

		if cfg.Host != "uh.example" {
			return nil
		}

		uhCalls++
		if uhCalls == 1 {
			return errors.New("454 4.7.0 Temporary authentication failure: Connection lost to authentication server")
		}

		return nil
	}

	primary := MailConfig{Host: "one.example", Username: "one", FromEmail: "one@example.com", Password: "secret-1"}
	svc.runDispatch("camp", "disp", ServiceTrentin, &primary, msgs, 0, 0, 0, send, 1)

	if uhCalls != 2 {
		t.Fatalf("temporary auth failure removed the sender after %d tries", uhCalls)
	}

	for i := range msgs {
		if store.sent["camp"][msgs[i].To] != MailStatusSent {
			t.Fatalf("%s status %q", msgs[i].To, store.sent["camp"][msgs[i].To])
		}
	}
}

func TestParseAtOnce(t *testing.T) {
	ok := []struct {
		raw  string
		want int
	}{
		{raw: "", want: 1},
		{raw: "  ", want: 1},
		{raw: "1", want: 1},
		{raw: "42", want: 42},
		{raw: "100000", want: 100000},
	}

	for _, tc := range ok {
		got, err := parseAtOnce(tc.raw)
		if err != nil || got != tc.want {
			t.Fatalf("parseAtOnce(%q) = %d, %v", tc.raw, got, err)
		}
	}

	for _, raw := range []string{"0", "-3", "1.5", "nope"} {
		if _, err := parseAtOnce(raw); err == nil {
			t.Fatalf("parseAtOnce(%q) accepted", raw)
		}
	}
}

func TestRunDispatchSendsTogetherWithoutRepeats(t *testing.T) {
	const (
		n      = 9
		atOnce = 4
	)

	store := newMemStore()
	store.dispatches = []Dispatch{{
		ID:         "disp",
		CampaignID: "camp",
		Status:     DispatchRunning,
		StartedAt:  time.Now().UTC(),
	}}

	store.smtp = map[string][]SMTPAccount{
		"camp": {
			{ID: "e1", Host: "two.example", Username: "two", Password: "p", FromEmail: "two@example.com", Encryption: MailSSL, Port: 465},
			{ID: "e2", Host: "three.example", Username: "three", Password: "p", FromEmail: "three@example.com", Encryption: MailSSL, Port: 465},
			{ID: "e3", Host: "four.example", Username: "four", Password: "p", FromEmail: "four@example.com", Encryption: MailSSL, Port: 465},
		},
	}

	svc := &Service{repo: store, optOut: &fakeOptOut{}, unsubscribeBase: testUnsubscribeBase}

	var details []string

	svc.dispatchSleep = func(context.Context, time.Duration) {
		store.mu.Lock()
		details = append(details, store.dispatches[0].Detail)
		store.mu.Unlock()
	}

	msgs := make([]renderedMail, n)
	for i := range msgs {
		msgs[i] = renderedMail{To: fmt.Sprintf("user%02d@example.com", i), Subject: "Hi", Body: "Body"}
	}

	// The same address again must not leave a second time.
	msgs = append(msgs, renderedMail{To: "user00@example.com", Subject: "Hi", Body: "Body"})

	var (
		mu          sync.Mutex
		cond        = sync.NewCond(&mu)
		inflight    int
		maxInflight int
		arrived     int
		generation  int
		seen        = map[string]int{}
	)

	full := (n / atOnce) * atOnce

	send := func(_ context.Context, _ *MailConfig, msg *OutgoingMail) error {
		var idx int

		_, _ = fmt.Sscanf(msg.To, "user%d@", &idx)

		mu.Lock()
		seen[msg.To]++
		inflight++

		if inflight > maxInflight {
			maxInflight = inflight
		}

		if idx < full {
			arrived++

			if arrived == atOnce {
				arrived = 0
				generation++
				cond.Broadcast()
			} else {
				gen := generation

				for gen == generation {
					cond.Wait()
				}
			}
		}

		inflight--
		mu.Unlock()

		return nil
	}

	svc.runDispatch("camp", "disp", ServiceTrentin, &MailConfig{}, msgs, 0, 0, 0, send, atOnce)

	if maxInflight != atOnce {
		t.Fatalf("max inflight %d, want %d", maxInflight, atOnce)
	}

	if len(seen) != n {
		t.Fatalf("addresses %d, want %d", len(seen), n)
	}

	for i := range msgs {
		if seen[msgs[i].To] != 1 {
			t.Fatalf("%s sent %d times", msgs[i].To, seen[msgs[i].To])
		}
	}

	store.mu.Lock()
	final := store.dispatches[0]
	store.mu.Unlock()

	if final.Status != DispatchOK || final.Sent != n {
		t.Fatalf("dispatch %+v", final)
	}

	// Two full waves of one email per sender, then the last address. Nine sends stay inside the first batch of 12.
	if len(details) != 2 {
		t.Fatalf("pauses %d, want 2: %v", len(details), details)
	}

	for _, detail := range details {
		if !strings.Contains(detail, "before the next emails") || strings.Contains(detail, "before the next batch") {
			t.Fatalf("detail %q", detail)
		}
	}
}

func TestRunDispatchCapsTheWaveAtTheQueue(t *testing.T) {
	const n = 5

	store := newMemStore()
	store.dispatches = []Dispatch{{
		ID:         "disp",
		CampaignID: "camp",
		Status:     DispatchRunning,
		StartedAt:  time.Now().UTC(),
	}}

	store.smtp = map[string][]SMTPAccount{
		"camp": {
			{ID: "e1", Host: "two.example", Username: "two", Password: "p", FromEmail: "two@example.com", Encryption: MailSSL, Port: 465},
			{ID: "e2", Host: "three.example", Username: "three", Password: "p", FromEmail: "three@example.com", Encryption: MailSSL, Port: 465},
			{ID: "e3", Host: "four.example", Username: "four", Password: "p", FromEmail: "four@example.com", Encryption: MailSSL, Port: 465},
			{ID: "e4", Host: "five.example", Username: "five", Password: "p", FromEmail: "five@example.com", Encryption: MailSSL, Port: 465},
		},
	}

	svc := &Service{repo: store, optOut: &fakeOptOut{}, unsubscribeBase: testUnsubscribeBase}

	var paused bool

	svc.dispatchSleep = func(context.Context, time.Duration) {
		paused = true
	}

	msgs := make([]renderedMail, n)
	for i := range msgs {
		msgs[i] = renderedMail{To: fmt.Sprintf("user%02d@example.com", i), Subject: "Hi", Body: "Body"}
	}

	var (
		mu          sync.Mutex
		cond        = sync.NewCond(&mu)
		inflight    int
		maxInflight int
		arrived     int
		generation  int
		seen        = map[string]int{}
	)

	send := func(_ context.Context, _ *MailConfig, msg *OutgoingMail) error {
		mu.Lock()
		seen[msg.To]++
		inflight++

		if inflight > maxInflight {
			maxInflight = inflight
		}

		arrived++

		if arrived == n {
			generation++
			cond.Broadcast()
		} else {
			gen := generation

			for gen == generation {
				cond.Wait()
			}
		}

		inflight--
		mu.Unlock()

		return nil
	}

	svc.runDispatch("camp", "disp", ServiceTrentin, &MailConfig{}, msgs, 0, 0, 0, send, 1000)

	if maxInflight != n {
		t.Fatalf("max inflight %d, want %d", maxInflight, n)
	}

	if len(seen) != n {
		t.Fatalf("addresses %d", len(seen))
	}

	for _, count := range seen {
		if count != 1 {
			t.Fatalf("a address left %d times", count)
		}
	}

	if paused {
		t.Fatal("paused after the only wave")
	}
}

func TestRunDispatchParallelStopsAfterTheBlockedWave(t *testing.T) {
	store := newMemStore()
	store.dispatches = []Dispatch{{
		ID:         "disp",
		CampaignID: "camp",
		Status:     DispatchRunning,
		StartedAt:  time.Now().UTC(),
	}}

	store.smtp = map[string][]SMTPAccount{
		"camp": {
			{ID: "e1", Host: "two.example", Username: "two", Password: "p", FromEmail: "two@example.com", Encryption: MailSSL, Port: 465},
			{ID: "e2", Host: "three.example", Username: "three", Password: "p", FromEmail: "three@example.com", Encryption: MailSSL, Port: 465},
		},
	}

	svc := &Service{repo: store, optOut: &fakeOptOut{}, unsubscribeBase: testUnsubscribeBase}
	svc.dispatchSleep = func(context.Context, time.Duration) {}

	msgs := make([]renderedMail, 6)
	for i := range msgs {
		msgs[i] = renderedMail{To: fmt.Sprintf("user%02d@example.com", i), Subject: "Hi", Body: "Body"}
	}

	var mu sync.Mutex

	attempts := map[string]int{}

	send := func(_ context.Context, _ *MailConfig, msg *OutgoingMail) error {
		mu.Lock()
		attempts[msg.To]++
		mu.Unlock()

		if msg.To == "user01@example.com" {
			return errors.New(`550 5.4.6 Unusual sending activity detected. UnblockMe`)
		}

		return nil
	}

	svc.runDispatch("camp", "disp", ServiceTrentin, &MailConfig{}, msgs, 0, 0, 0, send, 3)

	if len(attempts) != 3 {
		t.Fatalf("attempts %v, want the first wave only", attempts)
	}

	for _, to := range []string{"user00@example.com", "user01@example.com", "user02@example.com"} {
		if attempts[to] != 1 {
			t.Fatalf("attempts %v", attempts)
		}
	}

	if store.sent["camp"]["user00@example.com"] != MailStatusSent || store.sent["camp"]["user02@example.com"] != MailStatusSent {
		t.Fatalf("wave statuses %v", store.sent["camp"])
	}

	if _, ok := store.sent["camp"]["user01@example.com"]; ok {
		t.Fatal("blocked address was recorded")
	}

	for _, to := range []string{"user03@example.com", "user04@example.com", "user05@example.com"} {
		if _, ok := attempts[to]; ok {
			t.Fatalf("address after the block was attempted: %s", to)
		}
	}

	store.mu.Lock()
	final := store.dispatches[0]
	store.mu.Unlock()

	if final.Status != DispatchFailed || !strings.Contains(final.Detail, "stay queued") || !providerBlockText(final.Detail) {
		t.Fatalf("detail %s status %s", final.Detail, final.Status)
	}
}
