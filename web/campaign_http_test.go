//nolint:testpackage // tests unexported campaign handlers directly
package web

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIndexLinksCampaigns(t *testing.T) {
	srv := newTestServer(t, t.TempDir())
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	rec := httptest.NewRecorder()
	srv.index(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `href="/campaigns"`) || !strings.Contains(body, "No campaign") {
		t.Fatalf("index missing campaign controls: %s", body)
	}

	if !strings.Contains(body, `id="email" name="email" checked`) {
		t.Fatalf("email extraction is not on by default: %s", body)
	}

	if !strings.Contains(body, `id="theme-toggle"`) || !strings.Contains(body, `/static/js/theme.js`) {
		t.Fatalf("index missing theme control: %s", body)
	}
}

func TestCampaignPagesHidePassword(t *testing.T) {
	store := newMemStore()
	store.campaigns["11111111-1111-1111-1111-111111111111"] = Campaign{
		ID:        "11111111-1111-1111-1111-111111111111",
		Name:      "Spring",
		CreatedAt: time.Unix(10, 0).UTC(),
	}
	store.mail[store.campaigns["11111111-1111-1111-1111-111111111111"].ID] = MailConfig{
		Host:       "smtp.secret.test",
		Port:       587,
		Password:   "super-secret-value",
		FromEmail:  "from@cafe.test",
		Encryption: MailStartTLS,
	}

	srv := newTestServerWithRepo(t, store)
	req := httptest.NewRequest(http.MethodGet, "/campaigns/11111111-1111-1111-1111-111111111111", http.NoBody)
	req.SetPathValue("id", "11111111-1111-1111-1111-111111111111")

	rec := httptest.NewRecorder()
	srv.campaign(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if strings.Contains(body, "super-secret-value") {
		t.Fatal("page contains the SMTP password")
	}

	if !strings.Contains(body, "smtp.secret.test") || !strings.Contains(body, "A password is saved") {
		t.Fatalf("page missing mail form: %s", body)
	}

	if strings.Contains(body, `name="from_email"`) || strings.Contains(body, `name="from_name"`) || strings.Contains(body, "Empty keeps the name") {
		t.Fatal("message form still has a shared sender")
	}

	if !strings.Contains(body, `name="sender_from_email"`) || !strings.Contains(body, `data-sender-check="primary"`) || !strings.Contains(body, `name="test_sender"`) {
		t.Fatal("page missing per-sender edit and connection test")
	}

	for _, svc := range DefaultServices {
		if !strings.Contains(body, `<option value="`+svc.Name+`"`) {
			t.Fatalf("service select has no %s option", svc.Name)
		}
	}
}

func TestSaveCampaignMessageKeepsEachSender(t *testing.T) {
	store := newMemStore()
	id := "11111111-1111-1111-1111-111111111111"
	store.campaigns[id] = Campaign{ID: id, Name: "Spring", CreatedAt: time.Unix(10, 0).UTC()}
	store.mail[id] = MailConfig{
		Host: "smtp.zoho.com", Port: 465, Username: "contact@example.com",
		Password: "keep-me", Encryption: MailSSL,
		FromEmail: "contact@example.com", FromName: "SplitQuota",
		Subject: "Old", Body: "Old body", Format: MailFormatPlain,
	}
	svc := &Service{repo: store}
	ctx := context.Background()

	if err := svc.SaveCampaignMessage(ctx, id, "New subject", "New body", MailFormatHTML); err != nil {
		t.Fatal(err)
	}

	got, ok, err := store.GetMail(ctx, id)
	if err != nil || !ok || got.FromEmail != "contact@example.com" || got.FromName != "SplitQuota" || got.Password != "keep-me" || got.Host != "smtp.zoho.com" || got.Subject != "New subject" || got.Body != "New body" || got.Format != MailFormatHTML {
		t.Fatalf("message save changed the sender: %+v %v", got, err)
	}

	extra := &SMTPAccount{
		ID: "extra-1", Host: "smtp.other.test", Port: 465, Username: "two@example.com",
		Password: "other-secret", Encryption: MailSSL, FromEmail: "two@example.com",
	}
	if err := store.InsertSMTPAccount(ctx, id, extra); err != nil {
		t.Fatal(err)
	}

	updated := SMTPAccount{
		ID: "extra-1", Host: "smtp.other.test", Port: 465, Username: "two@example.com",
		Encryption: MailSSL, FromEmail: "two@example.com", FromName: "Only Two",
	}
	if err := svc.UpdateSMTPAccount(ctx, id, &updated); err != nil {
		t.Fatal(err)
	}

	list, err := store.ListSMTPAccounts(ctx, id)
	if err != nil || len(list) != 1 || list[0].Password != "other-secret" || list[0].FromName != "Only Two" {
		t.Fatalf("extra %+v %v", list, err)
	}

	primary := SMTPAccount{
		Host: "smtp.zoho.com", Port: 465, Username: "contact@example.com",
		Encryption: MailSSL, FromEmail: "contact@example.com", FromName: "New Name",
	}
	if err := svc.SavePrimarySender(ctx, id, &primary); err != nil {
		t.Fatal(err)
	}

	got, _, err = store.GetMail(ctx, id)
	if err != nil || got.Password != "keep-me" || got.FromName != "New Name" || got.Subject != "New subject" {
		t.Fatalf("primary save %+v %v", got, err)
	}
}

func TestPreviewMailDoesNotEchoPassword(t *testing.T) {
	store := newMemStore()
	id := "11111111-1111-1111-1111-111111111111"
	store.campaigns[id] = Campaign{ID: id, Name: "Spring", CreatedAt: time.Unix(10, 0).UTC()}
	srv := newTestServerWithRepo(t, store)

	form := url.Values{
		"smtp_password": {"super-secret-value"},
		"subject":       {"Hello {{.Title}}"},
		"body":          {"<p>{{.Title}}</p>"},
		"format":        {"html"},
	}
	req := httptest.NewRequest(http.MethodPost, "/campaigns/"+id+"/mail/preview", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", id)

	rec := httptest.NewRecorder()
	srv.campaignMailPreview(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if strings.Contains(body, "super-secret-value") {
		t.Fatal("preview contains the SMTP password")
	}

	if !strings.Contains(body, "Test business") || !strings.Contains(body, `"html":true`) {
		t.Fatalf("preview: %s", body)
	}
}

func TestPreviewMailReadsMultipartForm(t *testing.T) {
	store := newMemStore()
	id := "11111111-1111-1111-1111-111111111111"
	store.campaigns[id] = Campaign{ID: id, Name: "Spring", CreatedAt: time.Unix(10, 0).UTC()}
	srv := newTestServerWithRepo(t, store)

	var buf bytes.Buffer

	writer := multipart.NewWriter(&buf)
	_ = writer.WriteField("subject", "Hello {{.Title}}")
	_ = writer.WriteField("body", "Hi {{.Email}}")

	_ = writer.WriteField("format", "plain")
	if err := writer.Close(); err != nil {
		t.Fatalf("multipart: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/campaigns/"+id+"/mail/preview", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.SetPathValue("id", id)

	rec := httptest.NewRecorder()
	srv.campaignMailPreview(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if strings.Contains(body, "subject and body are required") || !strings.Contains(body, "Hello Test business") || !strings.Contains(body, "Hi preview@example.com") {
		t.Fatalf("preview: %s", body)
	}
}

func TestCreateCampaignRedirects(t *testing.T) {
	store := newMemStore()
	srv := newTestServerWithRepo(t, store)
	form := url.Values{"name": {"North"}}
	req := httptest.NewRequest(http.MethodPost, "/campaigns", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	srv.campaigns(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d", rec.Code)
	}

	if !strings.Contains(rec.Header().Get("Location"), "/campaigns/") {
		t.Fatalf("location %s", rec.Header().Get("Location"))
	}

	if len(store.campaigns) != 1 {
		t.Fatalf("campaigns: %#v", store.campaigns)
	}
}

type memStore struct {
	mockJobRepo
	mu        sync.Mutex
	campaigns map[string]Campaign
	// order keeps creation order, so lists are stable like the SQLite store.
	order      []string
	members    map[string][]string
	jobCamp    map[string]string
	mail       map[string]MailConfig
	sent       map[string]map[string]string
	reasons    map[string]map[string]string
	services   map[string]MailService
	dispatches []Dispatch
	smtp       map[string][]SMTPAccount
	froms      map[string]map[string]string
	events     map[string][]MailLogEntry
}

func newMemStore() *memStore {
	return &memStore{
		campaigns: map[string]Campaign{},
		members:   map[string][]string{},
		jobCamp:   map[string]string{},
		mail:      map[string]MailConfig{},
		sent:      map[string]map[string]string{},
		reasons:   map[string]map[string]string{},
		services:  defaultServiceMap(),
	}
}

func defaultServiceMap() map[string]MailService {
	out := map[string]MailService{}
	for _, svc := range DefaultServices {
		out[svc.Name] = svc
	}

	return out
}

func (m *memStore) SetCampaignService(_ context.Context, id, service string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.campaigns[id]
	if !ok {
		return ErrNotFound
	}

	c.ServiceName = service
	m.campaigns[id] = c

	return nil
}

func (m *memStore) ListServices(context.Context) ([]MailService, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]MailService, 0, len(m.services))
	for _, svc := range m.services {
		out = append(out, svc)
	}

	return out, nil
}

func (m *memStore) GetService(_ context.Context, name string) (MailService, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	svc, ok := m.services[name]
	if !ok {
		return MailService{}, ErrNotFound
	}

	return svc, nil
}

func (m *memStore) CreateService(_ context.Context, svc *MailService) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.services[svc.Name]; ok {
		return ErrServiceExists
	}

	m.services[svc.Name] = *svc

	return nil
}

func (m *memStore) UpdateService(_ context.Context, svc *MailService) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.services[svc.Name]; !ok {
		return ErrNotFound
	}

	m.services[svc.Name] = *svc

	return nil
}

func (m *memStore) DeleteService(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, c := range m.campaigns {
		if c.ServiceName == name {
			return ErrServiceInUse
		}
	}

	delete(m.services, name)

	return nil
}

func (m *memStore) CreateCampaign(_ context.Context, c *Campaign) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	copied := *c
	if _, ok := m.campaigns[c.ID]; !ok {
		m.order = append(m.order, c.ID)
	}

	m.campaigns[c.ID] = copied

	return nil
}

func (m *memStore) ListCampaigns(context.Context) ([]CampaignSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]CampaignSummary, 0, len(m.campaigns))

	for _, id := range m.order {
		c, ok := m.campaigns[id]
		if !ok {
			continue
		}

		_, ready := m.mail[c.ID]
		out = append(out, CampaignSummary{Campaign: c, JobCount: len(m.members[c.ID]), MailReady: ready})
	}

	return out, nil
}

func (m *memStore) GetCampaign(_ context.Context, id string) (Campaign, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.campaigns[id]
	if !ok {
		return Campaign{}, ErrNotFound
	}

	return c, nil
}

func (m *memStore) RenameCampaign(_ context.Context, id, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.campaigns[id]
	if !ok {
		return ErrNotFound
	}

	c.Name = name
	m.campaigns[id] = c

	return nil
}

func (m *memStore) DeleteCampaign(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.campaigns[id]; !ok {
		return ErrNotFound
	}

	delete(m.campaigns, id)
	delete(m.members, id)
	delete(m.mail, id)
	delete(m.sent, id)

	return nil
}

func (m *memStore) AssignJob(_ context.Context, campaignID, jobID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.campaigns[campaignID]; !ok {
		return ErrNotFound
	}

	if old, ok := m.jobCamp[jobID]; ok {
		m.members[old] = removeID(m.members[old], jobID)
	}

	m.jobCamp[jobID] = campaignID
	m.members[campaignID] = append(m.members[campaignID], jobID)

	return nil
}

func (m *memStore) UnassignJob(_ context.Context, campaignID, jobID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.members[campaignID] = removeID(m.members[campaignID], jobID)
	delete(m.jobCamp, jobID)

	return nil
}

func (m *memStore) CampaignJobs(_ context.Context, campaignID string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]string(nil), m.members[campaignID]...), nil
}

func (m *memStore) CampaignsForJobs(_ context.Context, jobIDs []string) (map[string]CampaignRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := map[string]CampaignRef{}

	for _, id := range jobIDs {
		campID, ok := m.jobCamp[id]
		if !ok {
			continue
		}

		out[id] = CampaignRef{ID: campID, Name: m.campaigns[campID].Name}
	}

	return out, nil
}

func (m *memStore) ListSMTPAccounts(_ context.Context, campaignID string) ([]SMTPAccount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]SMTPAccount(nil), m.smtp[campaignID]...), nil
}

func (m *memStore) InsertSMTPAccount(_ context.Context, campaignID string, a *SMTPAccount) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.smtp == nil {
		m.smtp = map[string][]SMTPAccount{}
	}

	copied := *a
	m.smtp[campaignID] = append(m.smtp[campaignID], copied)

	return nil
}

func (m *memStore) UpdateSMTPAccount(_ context.Context, campaignID string, a *SMTPAccount) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	list := m.smtp[campaignID]
	for i := range list {
		if list[i].ID == a.ID {
			m.smtp[campaignID][i] = *a

			return nil
		}
	}

	return errors.New("SMTP server not found")
}

func (m *memStore) DeleteSMTPAccount(_ context.Context, campaignID, accountID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	list := m.smtp[campaignID]
	for i := range list {
		if list[i].ID == accountID {
			m.smtp[campaignID] = append(list[:i], list[i+1:]...)

			return nil
		}
	}

	return errors.New("SMTP server not found")
}

func (m *memStore) GetMail(_ context.Context, campaignID string) (MailConfig, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cfg, ok := m.mail[campaignID]

	return cfg, ok, nil
}

func (m *memStore) SaveMail(_ context.Context, campaignID string, cfg *MailConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.mail[campaignID] = *cfg

	return nil
}

// SentEmailsSince ignores since: the in-memory store keeps no send times.
func (m *memStore) SentEmailsSince(ctx context.Context, campaignID string, _ time.Time) (map[string]struct{}, error) {
	return m.SentEmails(ctx, campaignID)
}

func (m *memStore) SentEmails(_ context.Context, campaignID string) (map[string]struct{}, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := map[string]struct{}{}

	for email, status := range m.sent[campaignID] {
		if status == MailStatusSent || status == MailStatusUnsubscribed {
			out[email] = struct{}{}
		}
	}

	return out, nil
}

func (m *memStore) RecordMail(ctx context.Context, campaignID, email, status, reason string) error {
	return m.RecordMailFrom(ctx, campaignID, email, status, reason, "")
}

func (m *memStore) RecordMailFrom(_ context.Context, campaignID, email, status, reason, fromEmail string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.sent[campaignID] == nil {
		m.sent[campaignID] = map[string]string{}
		m.reasons[campaignID] = map[string]string{}
	}

	if m.froms == nil {
		m.froms = map[string]map[string]string{}
	}

	if m.froms[campaignID] == nil {
		m.froms[campaignID] = map[string]string{}
	}

	m.sent[campaignID][email] = status
	m.reasons[campaignID][email] = reason
	m.froms[campaignID][email] = fromEmail
	m.appendSenderEvent(campaignID, email, status, reason, fromEmail)

	return nil
}

func (m *memStore) RecordSenderResult(_ context.Context, campaignID, email, status, reason, fromEmail string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.appendSenderEvent(campaignID, email, status, reason, fromEmail)

	return nil
}

func (m *memStore) appendSenderEvent(campaignID, email, status, reason, fromEmail string) {
	fromEmail = strings.ToLower(strings.TrimSpace(fromEmail))
	if fromEmail == "" {
		return
	}

	if m.events == nil {
		m.events = map[string][]MailLogEntry{}
	}

	m.events[campaignID] = append(m.events[campaignID], MailLogEntry{
		Email:     strings.ToLower(email),
		Status:    status,
		Error:     reason,
		FromEmail: fromEmail,
		SentAt:    time.Now().UTC(),
	})
}

func (m *memStore) SenderLog(_ context.Context, campaignID string) ([]MailLogEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	src := m.events[campaignID]
	out := make([]MailLogEntry, len(src))

	for i := range src {
		out[len(src)-1-i] = src[i]
	}

	return out, nil
}

func (m *memStore) RequeueFailed(_ context.Context, campaignID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	n := 0

	for email, status := range m.sent[campaignID] {
		if status == MailStatusFailed {
			delete(m.sent[campaignID], email)
			delete(m.reasons[campaignID], email)
			n++
		}
	}

	return n, nil
}

func (m *memStore) MailLog(_ context.Context, campaignID string) ([]MailLogEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]MailLogEntry, 0, len(m.sent[campaignID]))
	for email, status := range m.sent[campaignID] {
		out = append(out, MailLogEntry{Email: email, Status: status, FromEmail: m.froms[campaignID][email]})
	}

	return out, nil
}

func (m *memStore) InsertDispatch(_ context.Context, d *Dispatch) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.dispatches = append(m.dispatches, *d)

	return nil
}

func (m *memStore) UpdateDispatch(_ context.Context, d *Dispatch) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.dispatches {
		if m.dispatches[i].ID != d.ID {
			continue
		}

		m.dispatches[i].Status = d.Status
		m.dispatches[i].FinishedAt = d.FinishedAt
		m.dispatches[i].Sent = d.Sent
		m.dispatches[i].Failed = d.Failed
		m.dispatches[i].Skipped = d.Skipped
		m.dispatches[i].Detail = d.Detail
	}

	return nil
}

func (m *memStore) ListDispatches(_ context.Context, campaignID string, limit int) ([]Dispatch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []Dispatch

	for i := len(m.dispatches) - 1; i >= 0 && len(out) < limit; i-- {
		if m.dispatches[i].CampaignID == campaignID {
			out = append(out, m.dispatches[i])
		}
	}

	return out, nil
}

func (m *memStore) LatestRunningDispatch(_ context.Context, campaignID string) (Dispatch, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := len(m.dispatches) - 1; i >= 0; i-- {
		if m.dispatches[i].CampaignID == campaignID && m.dispatches[i].Status == DispatchRunning {
			return m.dispatches[i], true, nil
		}
	}

	return Dispatch{}, false, nil
}

func (m *memStore) ExpireRunningDispatches(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	n := 0

	for i := range m.dispatches {
		if m.dispatches[i].Status == DispatchRunning {
			m.dispatches[i].Status = DispatchFailed
			m.dispatches[i].Detail = "interrupted"
			n++
		}
	}

	return n, nil
}

func removeID(ids []string, id string) []string {
	out := ids[:0]

	for _, item := range ids {
		if item != id {
			out = append(out, item)
		}
	}

	return out
}

func TestParseEmailCellStripsGluedPhone(t *testing.T) {
	got := parseEmailCell("99885-1359secretaria@colegiojphigi.com.br, 99545-3421contatohigi@colegiojphigi.com.br, secretaria@colegiojphigi.com.br")
	want := []string{"secretaria@colegiojphigi.com.br", "contatohigi@colegiojphigi.com.br"}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}
