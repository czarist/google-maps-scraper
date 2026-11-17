//nolint:testpackage // tests unexported handlers (viewJob, requestWithID, securityHeaders) directly
package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type mutableJobRepo struct {
	mockJobRepo
}

func (r *mutableJobRepo) Create(_ context.Context, job *Job) error {
	r.jobs = append([]Job{*job}, r.jobs...)

	return nil
}

func (r *mutableJobRepo) Delete(_ context.Context, id string) error {
	for i := range r.jobs {
		if r.jobs[i].ID == id {
			r.jobs = append(r.jobs[:i], r.jobs[i+1:]...)

			break
		}
	}

	return nil
}

func paginatedJobs(count int) []Job {
	jobs := make([]Job, count)

	for i := range jobs {
		jobs[i] = Job{
			ID:   fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1),
			Name: fmt.Sprintf("job-%d", i+1),
		}
	}

	return jobs
}

func newTestServer(t *testing.T, dir string) *Server {
	t.Helper()

	srv, err := New(NewService(nil, dir), ":0")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return srv
}

func TestViewJobRendersPlaces(t *testing.T) {
	dir := t.TempDir()
	id := "11111111-1111-1111-1111-111111111111"

	csv := "title,latitude,longitude\nPlace,1.5,2.5\n"
	if err := os.WriteFile(filepath.Join(dir, id+".csv"), []byte(csv), 0o600); err != nil {
		t.Fatalf("write csv: %v", err)
	}

	srv := newTestServer(t, dir)

	req := requestWithID(httptest.NewRequest(http.MethodGet, "/view?id="+id, http.NoBody))
	rec := httptest.NewRecorder()
	srv.viewJob(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	body := rec.Body.String()
	for _, want := range []string{`id="map-modal"`, `initJobMap()`, `"title":"Place"`, `"latitude":1.5`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}

func TestViewJobEmptyState(t *testing.T) {
	srv := newTestServer(t, t.TempDir())

	id := "22222222-2222-2222-2222-222222222222"
	req := requestWithID(httptest.NewRequest(http.MethodGet, "/view?id="+id, http.NoBody))
	rec := httptest.NewRecorder()
	srv.viewJob(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "var places = [];") {
		t.Fatalf("expected empty places array, got:\n%s", body)
	}
}

func TestViewJobInvalidID(t *testing.T) {
	srv := newTestServer(t, t.TempDir())

	req := requestWithID(httptest.NewRequest(http.MethodGet, "/view?id=not-a-uuid", http.NoBody))
	rec := httptest.NewRecorder()
	srv.viewJob(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", rec.Code)
	}
}

func TestSecurityHeadersAllowMapResources(t *testing.T) {
	handler := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"tile.openstreetmap.org", "cdnjs.cloudflare.com"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP missing %q: %s", want, csp)
		}
	}
}

func newTestServerWithRepo(t *testing.T, repo JobRepository) *Server {
	t.Helper()

	srv, err := New(NewService(repo, t.TempDir()), ":0")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return srv
}

func TestGetJobsPollsAndGroupsJobs(t *testing.T) {
	srv := newTestServerWithRepo(t, &mockJobRepo{jobs: paginatedJobs(1)})

	req := httptest.NewRequest(http.MethodGet, "/jobs", http.NoBody)
	rec := httptest.NewRecorder()
	srv.getJobs(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	body := rec.Body.String()

	// The returned list must only poll: "load" on an outerHTML-swapped element reloads in a loop.
	if strings.Contains(body, `hx-trigger="load`) {
		t.Fatal("job_groups.html must not use hx-trigger=\"load\"")
	}

	for _, want := range []string{
		`hx-trigger="every 10s"`,
		`hx-delete="/delete?id=00000000-0000-0000-0000-000000000001"`,
		`hx-target="#job-groups"`,
		`data-group="none"`,
		"No campaign",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}

func TestIndexScrapeFormReplacesJobGroups(t *testing.T) {
	srv := newTestServer(t, t.TempDir())

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	rec := httptest.NewRecorder()
	srv.index(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `hx-target="#job-groups"`) || !strings.Contains(body, `hx-swap="outerHTML"`) {
		t.Fatal("scrape form must replace the job groups")
	}
}

func TestScrapeHighlightsTheNewJob(t *testing.T) {
	repo := &mutableJobRepo{mockJobRepo: mockJobRepo{jobs: paginatedJobs(20)}}
	srv := newTestServerWithRepo(t, repo)

	form := url.Values{
		"name":      {"new-job"},
		"maxtime":   {"4m"},
		"keywords":  {"coffee"},
		"lang":      {"en"},
		"zoom":      {"15"},
		"radius":    {"10000"},
		"latitude":  {"0"},
		"longitude": {"0"},
		"depth":     {"10"},
	}
	req := httptest.NewRequest(http.MethodPost, "/scrape", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	srv.scrape(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, `<div id="job-groups"`) || !strings.Contains(body, `<tr class="is-new">`) {
		t.Fatalf("scrape response must hold the job groups with the new job marked: %s", body)
	}

	// Every job is listed; the grouped list has no pages.
	if rows := strings.Count(body, "<tr") - strings.Count(body, "<thead>"); rows != 21 {
		t.Fatalf("got %d job rows, want 21", rows)
	}
}

func TestDeleteRerendersJobGroups(t *testing.T) {
	repo := &mutableJobRepo{mockJobRepo: mockJobRepo{jobs: paginatedJobs(3)}}
	srv := newTestServerWithRepo(t, repo)
	deletedJob := repo.jobs[2]

	req := httptest.NewRequest(http.MethodDelete, "/delete?id="+deletedJob.ID, http.NoBody)
	req = requestWithID(req)
	rec := httptest.NewRecorder()
	srv.delete(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, `<div id="job-groups"`) || strings.Contains(body, deletedJob.Name) {
		t.Fatalf("delete response must re-render the groups without %q: %s", deletedJob.Name, body)
	}
}

func TestJobGroupsByCampaignWithEmailNumbers(t *testing.T) {
	store := jobMemStore{newMemStore()}
	srv := newTestServerWithRepo(t, store)
	ctx := context.Background()

	full := Campaign{ID: "11111111-1111-1111-1111-111111111111", Name: "Escolas POA"}
	empty := Campaign{ID: "22222222-2222-2222-2222-222222222222", Name: "Vazia"}

	for _, c := range []*Campaign{&full, &empty} {
		if err := srv.svc.CreateCampaign(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	inCampaign := Job{ID: "33333333-3333-3333-3333-333333333333", Name: "escolas", Status: StatusOK, Date: time.Now()}
	loose := Job{ID: "44444444-4444-4444-4444-444444444444", Name: "avulsa", Status: StatusOK, Date: time.Now()}

	for _, j := range []*Job{&inCampaign, &loose} {
		if err := store.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}

	if err := srv.svc.AssignJob(ctx, full.ID, inCampaign.ID); err != nil {
		t.Fatal(err)
	}

	csv := "title,emails\nA,a@x.com\nB,\nC,\"c@x.com, c2@x.com\"\n"
	if err := os.WriteFile(filepath.Join(srv.svc.dataFolder, inCampaign.ID+".csv"), []byte(csv), 0o600); err != nil {
		t.Fatal(err)
	}

	_ = store.RecordMail(ctx, full.ID, "a@x.com", MailStatusSent, "")
	_ = store.RecordMail(ctx, full.ID, "c@x.com", MailStatusFailed, "550 no such user")

	page, err := srv.svc.ListJobGroups(ctx, "")
	if err != nil {
		t.Fatal(err)
	}

	if len(page.Groups) != 3 || page.Groups[2].Key != noCampaignKey {
		t.Fatalf("want 2 campaigns then the no-campaign group, got %+v", page.Groups)
	}

	g := page.Groups[0]
	if g.Name != "Escolas POA" || len(g.Jobs) != 1 || g.WithEmail != 2 || g.Sent != 1 || g.Failed != 1 {
		t.Fatalf("campaign group = %+v", g)
	}

	if len(page.Groups[1].Jobs) != 0 || page.Groups[2].Jobs[0].Name != "avulsa" {
		t.Fatalf("groups = %+v", page.Groups)
	}

	stats, err := srv.svc.CampaignStats(ctx, full.ID)
	if err != nil {
		t.Fatal(err)
	}

	if stats.Places != 0 || stats.WithEmail != 2 || stats.Addresses != 3 || stats.Sent != 1 || stats.SendFailed != 1 {
		t.Fatalf("stats = %+v", stats)
	}

	rec := httptest.NewRecorder()
	srv.getJobs(rec, httptest.NewRequest(http.MethodGet, "/jobs", http.NoBody))

	for _, want := range []string{`href="/campaigns/` + full.ID + `">Configure</a>`, "1 emails sent", "1 failed", "2 with email"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("home list missing %q", want)
		}
	}
}

func TestIndexEnablesHTMXTemplateFragments(t *testing.T) {
	srv := newTestServer(t, t.TempDir())

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	rec := httptest.NewRecorder()
	srv.index(rec, req)

	body := rec.Body.String()

	// htmx 1.x needs template fragments to swap table markup inside the job groups reliably.
	if !strings.Contains(body, `<meta name="htmx-config" content='{"useTemplateFragments":true}'>`) {
		t.Fatal("index.html must enable htmx template fragments, or the job list swap drops #job-tbody")
	}
}
