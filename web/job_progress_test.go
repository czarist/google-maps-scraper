package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                              "0s",
		45 * time.Second:               "45s",
		3 * time.Minute:                "3m",
		3*time.Minute + 12*time.Second: "3m 12s",
		time.Hour:                      "1h",
		time.Hour + 5*time.Minute + 9*time.Second: "1h 5m",
	}

	for in, want := range cases {
		if got := FormatDuration(in); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestListJobsAddsLiveProgressAndCounts(t *testing.T) {
	repo := &mockJobRepo{jobs: []Job{
		{ID: "a", Name: "running", Status: StatusWorking},
		{ID: "b", Name: "queued-1", Status: StatusPending},
		{ID: "c", Name: "queued-2", Status: StatusPending},
		{ID: "d", Name: "done", Status: StatusOK, Data: JobData{Run: JobRun{Places: 7}}},
	}}
	svc := NewService(repo, t.TempDir())

	svc.StartProgress("a")
	svc.AddProgress("a", 3)
	svc.AddProgress("a", 2)

	page, err := svc.ListJobs(context.Background(), 1, 20)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}

	if page.Running != 1 || page.Queued != 2 || !page.Active() {
		t.Fatalf("unexpected counts: running=%d queued=%d", page.Running, page.Queued)
	}

	if page.Jobs[0].Data.Run.Places != 5 {
		t.Fatalf("running job should show live count 5, got %d", page.Jobs[0].Data.Run.Places)
	}

	if page.Jobs[3].Data.Run.Places != 7 {
		t.Fatalf("finished job should keep its saved count, got %d", page.Jobs[3].Data.Run.Places)
	}

	if final := svc.StopProgress("a"); final != 5 {
		t.Fatalf("StopProgress = %d, want 5", final)
	}

	if _, ok := svc.Progress("a"); ok {
		t.Fatal("counter should be removed after StopProgress")
	}
}

func TestJobRowsShowRunActivity(t *testing.T) {
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	repo := &mockJobRepo{jobs: []Job{
		{ID: "11111111-1111-1111-1111-111111111111", Name: "live", Status: StatusWorking,
			Data: JobData{MaxTime: 10 * time.Minute, Run: JobRun{StartedAt: started}}},
		{ID: "22222222-2222-2222-2222-222222222222", Name: "done", Status: StatusOK,
			Data: JobData{Run: JobRun{StartedAt: started, FinishedAt: started.Add(192 * time.Second), Places: 16}}},
	}}
	srv := newTestServerWithRepo(t, repo)
	srv.svc.StartProgress("11111111-1111-1111-1111-111111111111")
	srv.svc.AddProgress("11111111-1111-1111-1111-111111111111", 4)

	rec := httptest.NewRecorder()
	srv.getJobs(rec, httptest.NewRequest(http.MethodGet, "/jobs", http.NoBody))

	body := rec.Body.String()
	for _, want := range []string{
		`hx-trigger="every 3s"`,
		`data-running="1"`,
		`<strong class="run-count">4</strong> places found`,
		`data-started="2026-01-02T03:04:05Z"`,
		`aria-valuemax="600"`,
		`of 10m limit`,
		`16 places in 3m 12s`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}
