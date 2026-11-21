//nolint:testpackage // tests the unexported delivery gate and dispatch loop
package web

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

const testUnsubscribeBase = "https://trentin.software/descadastro"

// fakeOptOut is an unsubscribe list in memory. Keys are "email|service".
type fakeOptOut struct {
	mu      sync.Mutex
	out     map[string]bool
	err     error
	queries []string
}

func (f *fakeOptOut) OptedOut(_ context.Context, email, service string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.queries = append(f.queries, email+"|"+service)
	if f.err != nil {
		return false, f.err
	}

	return f.out[email+"|"+service], nil
}

func (f *fakeOptOut) ServiceExists(context.Context, string) (bool, error) {
	return f.err == nil, f.err
}

func (f *fakeOptOut) optOut(email, service string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.out == nil {
		f.out = map[string]bool{}
	}

	f.out[email+"|"+service] = true
}

func (f *fakeOptOut) queryLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.queries...)
}

// recordingSender is a mail provider that keeps what it was asked to send.
type recordingSender struct {
	mu   sync.Mutex
	msgs []OutgoingMail
}

func (r *recordingSender) send(_ context.Context, _ *MailConfig, msg *OutgoingMail) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.msgs = append(r.msgs, *msg)

	return nil
}

func (r *recordingSender) to() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, len(r.msgs))
	for i := range r.msgs {
		out[i] = r.msgs[i].To
	}

	return out
}

func newGateService(store *memStore, list *fakeOptOut) *Service {
	svc := &Service{repo: store, optOut: list, unsubscribeBase: testUnsubscribeBase}
	svc.dispatchWait = func(lo, _ time.Duration) time.Duration { return lo }
	svc.dispatchSleep = func(context.Context, time.Duration) {}

	return svc
}

func newRunningDispatch(store *memStore) {
	store.dispatches = []Dispatch{{ID: "disp", CampaignID: "camp", Status: DispatchRunning, StartedAt: time.Now().UTC()}}
}

func queue(addrs ...string) []renderedMail {
	out := make([]renderedMail, len(addrs))
	for i, a := range addrs {
		out[i] = renderedMail{To: a, Subject: "Hi", Body: "Body"}
	}

	return out
}

func TestDispatchSkipsAddressUnsubscribedFromCampaignService(t *testing.T) {
	store := newMemStore()
	newRunningDispatch(store)

	list := &fakeOptOut{}
	list.optOut("fulano@empresa.com", ServiceTrentin)

	sender := &recordingSender{}
	svc := newGateService(store, list)

	svc.runDispatch("camp", "disp", ServiceTrentin, &MailConfig{}, queue("fulano@empresa.com", "outro@empresa.com"), 0, 0, 0, sender.send, 1)

	if got := sender.to(); len(got) != 1 || got[0] != "outro@empresa.com" {
		t.Fatalf("sent to %v, want only outro@empresa.com", got)
	}

	if status := store.sent["camp"]["fulano@empresa.com"]; status != MailStatusUnsubscribed {
		t.Fatalf("status %q, want %q (never %q)", status, MailStatusUnsubscribed, MailStatusSent)
	}

	if reason := store.reasons["camp"]["fulano@empresa.com"]; reason != "descadastro:trentin" {
		t.Fatalf("reason %q", reason)
	}

	final := store.dispatches[0]
	if final.Sent != 1 || final.Skipped != 1 || final.Status != DispatchOK {
		t.Fatalf("dispatch %+v", final)
	}

	// Taken out of the queue: the next run does not pick it up again.
	sent, _ := store.SentEmails(context.Background(), "camp")
	if _, ok := sent["fulano@empresa.com"]; !ok {
		t.Fatal("unsubscribed address would be queued again")
	}
}

func TestUnsubscribeAppliesToOneServiceOnly(t *testing.T) {
	for _, service := range []string{"splitquota", "caderna"} {
		t.Run(service, func(t *testing.T) {
			store := newMemStore()
			newRunningDispatch(store)

			list := &fakeOptOut{}
			list.optOut("fulano@empresa.com", ServiceTrentin)

			sender := &recordingSender{}
			svc := newGateService(store, list)

			svc.runDispatch("camp", "disp", service, &MailConfig{}, queue("fulano@empresa.com"), 0, 0, 0, sender.send, 1)

			if got := sender.to(); len(got) != 1 {
				t.Fatalf("sent %v, want the address to receive %s mail", got, service)
			}

			if store.sent["camp"]["fulano@empresa.com"] != MailStatusSent {
				t.Fatalf("status %q", store.sent["camp"]["fulano@empresa.com"])
			}

			if q := list.queryLog(); len(q) != 1 || q[0] != "fulano@empresa.com|"+service {
				t.Fatalf("queries %v", q)
			}
		})
	}
}

func TestQueuedMessageIsBlockedWhenUnsubscribeArrivesLater(t *testing.T) {
	store := newMemStore()
	newRunningDispatch(store)

	list := &fakeOptOut{}
	sender := &recordingSender{}
	svc := newGateService(store, list)

	// The queue is built before the opt-out. The opt-out lands while the first message goes out.
	send := func(ctx context.Context, cfg *MailConfig, msg *OutgoingMail) error {
		list.optOut("segundo@empresa.com", ServiceTrentin)

		return sender.send(ctx, cfg, msg)
	}

	svc.runDispatch("camp", "disp", ServiceTrentin, &MailConfig{}, queue("primeiro@empresa.com", "segundo@empresa.com"), 0, 0, 0, send, 1)

	if got := sender.to(); len(got) != 1 || got[0] != "primeiro@empresa.com" {
		t.Fatalf("sent %v", got)
	}

	if store.sent["camp"]["segundo@empresa.com"] != MailStatusUnsubscribed {
		t.Fatalf("queued message status %q", store.sent["camp"]["segundo@empresa.com"])
	}
}

func TestLookupFailureSuspendsAndKeepsQueue(t *testing.T) {
	store := newMemStore()
	newRunningDispatch(store)

	list := &fakeOptOut{err: errors.New("dial tcp: i/o timeout")}
	sender := &recordingSender{}
	svc := newGateService(store, list)

	svc.runDispatch("camp", "disp", ServiceTrentin, &MailConfig{}, queue("a@empresa.com", "b@empresa.com", "c@empresa.com"), 0, 0, 0, sender.send, 1)

	if got := sender.to(); len(got) != 0 {
		t.Fatalf("provider called for %v", got)
	}

	if len(store.sent["camp"]) != 0 {
		t.Fatalf("recorded %v: nothing may be marked, so every address stays queued", store.sent["camp"])
	}

	// The run stops at the first failure instead of hammering the database.
	if q := list.queryLog(); len(q) != 1 {
		t.Fatalf("queries %v, want the run to stop after one failure", q)
	}

	final := store.dispatches[0]
	if final.Status != DispatchFailed || final.Sent != 0 || !strings.Contains(final.Detail, "unsubscribe check failed") {
		t.Fatalf("dispatch %+v", final)
	}

	sent, _ := store.SentEmails(context.Background(), "camp")
	if len(sent) != 0 {
		t.Fatalf("addresses left the queue: %v", sent)
	}
}

func TestMissingUnsubscribeDatabaseRefusesToSend(t *testing.T) {
	svc := &Service{unsubscribeBase: testUnsubscribeBase}
	sender := &recordingSender{}

	err := svc.deliver(context.Background(), &MailConfig{}, ServiceTrentin, &OutgoingMail{To: "a@empresa.com"}, sender.send)
	if !errors.Is(err, ErrOptOutUnavailable) {
		t.Fatalf("err %v", err)
	}

	if len(sender.to()) != 0 {
		t.Fatal("sent without an unsubscribe database")
	}
}

func TestInvalidServiceSuspendsWithoutLookupOrProvider(t *testing.T) {
	for _, service := range []string{"", "Trentin", "trentin software", "x' OR 1=1"} {
		list := &fakeOptOut{}
		sender := &recordingSender{}
		svc := &Service{optOut: list, unsubscribeBase: testUnsubscribeBase}

		err := svc.deliver(context.Background(), &MailConfig{}, service, &OutgoingMail{To: "a@empresa.com"}, sender.send)
		if !errors.Is(err, ErrInvalidService) {
			t.Fatalf("service %q: err %v", service, err)
		}

		if len(list.queryLog()) != 0 || len(sender.to()) != 0 {
			t.Fatalf("service %q reached the database or the provider", service)
		}
	}
}

func TestUnregisteredCampaignServiceBlocksTestMail(t *testing.T) {
	store := newMemStore()
	store.campaigns["camp"] = Campaign{ID: "camp", Name: "Camp", ServiceName: "removido"}

	list := &fakeOptOut{}
	sender := &recordingSender{}
	svc := newGateService(store, list)

	cfg := readyTestMail()

	err := svc.SendTestMail(context.Background(), "camp", &cfg, "eu@empresa.com", sender.send)
	if !errors.Is(err, ErrInvalidService) {
		t.Fatalf("err %v", err)
	}

	if len(list.queryLog()) != 0 || len(sender.to()) != 0 {
		t.Fatal("unregistered service reached the database or the provider")
	}
}

func TestManualTestMailIsChecked(t *testing.T) {
	store := newMemStore()
	store.campaigns["camp"] = Campaign{ID: "camp", Name: "Camp", ServiceName: "caderna"}

	list := &fakeOptOut{}
	list.optOut("eu@empresa.com", "caderna")

	sender := &recordingSender{}
	svc := newGateService(store, list)
	cfg := readyTestMail()

	err := svc.SendTestMail(context.Background(), "camp", &cfg, "Eu@Empresa.com", sender.send)

	var optedOut ErrUnsubscribed
	if !errors.As(err, &optedOut) || optedOut.Error() != "descadastro:caderna" {
		t.Fatalf("err %v", err)
	}

	if len(sender.to()) != 0 {
		t.Fatal("test mail reached the provider")
	}
}

func TestLookupUsesNormalizedEmail(t *testing.T) {
	list := &fakeOptOut{}
	sender := &recordingSender{}
	svc := &Service{optOut: list, unsubscribeBase: testUnsubscribeBase}

	err := svc.deliver(context.Background(), &MailConfig{}, ServiceTrentin, &OutgoingMail{To: "  Fulano@Empresa.COM "}, sender.send)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if q := list.queryLog(); len(q) != 1 || q[0] != "fulano@empresa.com|trentin" {
		t.Fatalf("queries %v", q)
	}
}

func TestUnsubscribeLinkInBodyAndHeader(t *testing.T) {
	cases := []struct {
		name    string
		format  MailFormat
		service string
		body    string
		want    string
	}{
		{"plain footer", MailFormatPlain, "splitquota", "Olá", "Descadastre-se: https://trentin.software/descadastro?servico=splitquota"},
		{"html footer", MailFormatHTML, ServiceTrentin, "<html><body><p>Olá</p></body></html>", `<a href="https://trentin.software/descadastro?servico=trentin">Descadastre-se</a>.</p>` + "\n</body>"},
		{"html marker", MailFormatHTML, "caderna", `<a href="{{UNSUBSCRIBE_URL}}">sair</a>`, `<a href="https://trentin.software/descadastro?servico=caderna">sair</a>`},
		{"plain marker", MailFormatPlain, ServiceTrentin, "Sair: {{UNSUBSCRIBE_URL}}", "Sair: https://trentin.software/descadastro?servico=trentin"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := &recordingSender{}
			svc := &Service{optOut: &fakeOptOut{}, unsubscribeBase: testUnsubscribeBase}

			err := svc.deliver(context.Background(), &MailConfig{Format: tc.format}, tc.service, &OutgoingMail{To: "a@empresa.com", Subject: "Oi", Body: tc.body}, sender.send)
			if err != nil {
				t.Fatalf("deliver: %v", err)
			}

			msg := sender.msgs[0]
			if !strings.Contains(msg.Body, tc.want) {
				t.Fatalf("body %q does not contain %q", msg.Body, tc.want)
			}

			if strings.Contains(msg.Body, unsubscribeMarker) {
				t.Fatalf("marker left in body: %q", msg.Body)
			}

			if strings.Contains(tc.body, unsubscribeMarker) && strings.Contains(msg.Body, "Descadastre-se") {
				t.Fatalf("footer added although the template has the marker: %q", msg.Body)
			}

			if msg.ListUnsubscribe != "https://trentin.software/descadastro?servico="+tc.service {
				t.Fatalf("header link %q", msg.ListUnsubscribe)
			}

			raw := buildMessage("Eu", "eu@empresa.com", msg.To, &msg, tc.format)
			if !strings.Contains(raw, "\r\nList-Unsubscribe: <https://trentin.software/descadastro?servico="+tc.service+">\r\n") {
				t.Fatalf("raw message has no List-Unsubscribe header:\n%s", raw)
			}

			if strings.Contains(raw, "List-Unsubscribe-Post") {
				t.Fatal("one-click unsubscribe header must not be sent")
			}
		})
	}
}

func TestUnsubscribeLinkIsEscapedInHTML(t *testing.T) {
	body := withUnsubscribeLink("<body></body>", "https://x.test/d?a=1&servico=trentin", MailFormatHTML)
	if !strings.Contains(body, `href="https://x.test/d?a=1&amp;servico=trentin"`) {
		t.Fatalf("link not escaped: %s", body)
	}
}

func TestUnsubscribeURLEncodesService(t *testing.T) {
	got, err := UnsubscribeURL("https://trentin.software/en/unsubscribe", "a b&c")
	if err != nil {
		t.Fatalf("url: %v", err)
	}

	if got != "https://trentin.software/en/unsubscribe?servico=a+b%26c" {
		t.Fatalf("url %s", got)
	}

	if _, err := UnsubscribeURL("trentin.software/descadastro", ServiceTrentin); err == nil {
		t.Fatal("relative base accepted")
	}
}

func TestMarkerSurvivesTemplateRendering(t *testing.T) {
	subjectT, bodyT, err := parseMailTemplates("Oi {{.Title}}", "Olá {{.Title}}. Sair: {{UNSUBSCRIBE_URL}}")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	_, body, err := renderMail(subjectT, bodyT, &MailVars{Title: "Cafe"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if body != "Olá Cafe. Sair: {{UNSUBSCRIBE_URL}}" {
		t.Fatalf("body %q", body)
	}
}

func TestCreateServiceRequiresWebsiteService(t *testing.T) {
	store := newMemStore()

	svc := newGateService(store, &fakeOptOut{err: errors.New("down")})
	if err := svc.CreateService(context.Background(), &MailService{Name: "novo"}); !errors.Is(err, ErrOptOutUnavailable) {
		t.Fatalf("err %v", err)
	}

	if _, ok := store.services["novo"]; ok {
		t.Fatal("service created without checking the website")
	}

	svc = newGateService(store, &fakeOptOut{})
	if err := svc.CreateService(context.Background(), &MailService{Name: "Novo Serviço"}); err == nil {
		t.Fatal("invalid name accepted")
	}

	if err := svc.CreateService(context.Background(), &MailService{Name: "novo", Label: "Novo"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := svc.CreateService(context.Background(), &MailService{Name: "novo"}); !errors.Is(err, ErrServiceExists) {
		t.Fatalf("duplicate err %v", err)
	}
}

func TestCreateCampaignDefaultsToTrentinAndRejectsUnknownService(t *testing.T) {
	store := newMemStore()
	svc := newGateService(store, &fakeOptOut{})

	c := Campaign{ID: "c1", Name: "One"}
	if err := svc.CreateCampaign(context.Background(), &c); err != nil {
		t.Fatalf("create: %v", err)
	}

	if store.campaigns["c1"].ServiceName != ServiceTrentin {
		t.Fatalf("service %q", store.campaigns["c1"].ServiceName)
	}

	bad := Campaign{ID: "c2", Name: "Two", ServiceName: "desconhecido"}
	if err := svc.CreateCampaign(context.Background(), &bad); !errors.Is(err, ErrInvalidService) {
		t.Fatalf("err %v", err)
	}
}

func readyTestMail() MailConfig {
	return MailConfig{
		Host:      "smtp.empresa.test",
		Port:      587,
		Password:  "secret",
		FromEmail: "eu@empresa.test",
		Subject:   "Oi",
		Body:      "Olá {{.Title}}",
	}
}
