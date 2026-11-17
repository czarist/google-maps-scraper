//nolint:testpackage // drives the MCP endpoint against the in-memory store
package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// jobMemStore is memStore with jobs that are actually stored.
type jobMemStore struct {
	*memStore
}

func (m jobMemStore) Create(_ context.Context, j *Job) error {
	m.jobs = append(m.jobs, *j)

	return nil
}

func (m jobMemStore) Get(_ context.Context, id string) (Job, error) {
	for _, j := range m.jobs {
		if j.ID == id {
			return j, nil
		}
	}

	return Job{}, ErrJobNotFound
}

func (m jobMemStore) Delete(_ context.Context, id string) error {
	m.jobs = slices.DeleteFunc(m.jobs, func(j Job) bool { return j.ID == id })

	return nil
}

func mcpSession(t *testing.T) *mcp.ClientSession {
	t.Helper()

	srv := newTestServerWithRepo(t, jobMemStore{newMemStore()})
	ts := httptest.NewServer(srv.srv.Handler)
	t.Cleanup(ts.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)

	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	t.Cleanup(func() { _ = session.Close() })

	return session
}

func call[T any](t *testing.T, s *mcp.ClientSession, tool string, args map[string]any) T {
	t.Helper()

	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}

	if res.IsError {
		t.Fatalf("%s returned a tool error: %v", tool, res.Content[0].(*mcp.TextContent).Text)
	}

	var out T

	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: decode %s: %v", tool, raw, err)
	}

	return out
}

func toolError(t *testing.T, s *mcp.ClientSession, tool string, args map[string]any) string {
	t.Helper()

	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}

	if !res.IsError {
		t.Fatalf("%s: expected a tool error", tool)
	}

	return res.Content[0].(*mcp.TextContent).Text
}

func TestMCPListsEveryTool(t *testing.T) {
	s := mcpSession(t)

	tools, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}
	for _, tool := range tools.Tools {
		got[tool.Name] = true
	}

	for _, want := range []string{
		"create_job", "wait_for_job", "get_job", "list_jobs", "get_job_results", "delete_job",
		"list_proxies", "add_proxies", "remove_proxies", "test_proxies",
		"list_campaigns", "get_campaign", "create_campaign", "rename_campaign", "delete_campaign",
		"add_job_to_campaign", "remove_job_from_campaign",
		"configure_campaign_email", "test_campaign_smtp", "preview_campaign_email", "send_test_email", "start_campaign_dispatch",
	} {
		if !got[want] {
			t.Errorf("missing tool %s", want)
		}
	}
}

func TestMCPProxyPoolFeedsNewJobs(t *testing.T) {
	s := mcpSession(t)

	pool := call[proxiesOutput](t, s, "add_proxies", map[string]any{"proxies": []string{"http://u:p@1.2.3.4:8080", "socks5://5.6.7.8:1080", "http://u:p@1.2.3.4:8080"}})
	if pool.Count != 2 {
		t.Fatalf("pool = %+v, want 2 unique proxies", pool)
	}

	if msg := toolError(t, s, "add_proxies", map[string]any{"proxies": []string{"ftp://nope"}}); !strings.Contains(msg, "invalid proxy") {
		t.Fatalf("error = %q", msg)
	}

	// Several searches in one string are split by line.
	job := call[createJobOutput](t, s, "create_job", map[string]any{"name": "escolas", "searches": []string{"escolas porto alegre\nescolas canoas"}})
	if job.Job.Proxies != 2 || job.Job.Lang != "pt" || job.Job.RadiusKM != 50 || !job.Job.CollectEmail ||
		job.Job.MaxMinutes != 10 || len(job.Job.Searches) != 2 || !strings.Contains(job.NextStep, "wait_for_job") {
		t.Fatalf("job defaults wrong: %+v", job)
	}

	direct := call[createJobOutput](t, s, "create_job", map[string]any{"name": "direto", "searches": []string{"x"}, "use_proxies": false})
	if direct.Job.Proxies != 0 {
		t.Fatalf("use_proxies=false still used %d proxies", direct.Job.Proxies)
	}

	pool = call[proxiesOutput](t, s, "remove_proxies", map[string]any{"all": true})
	if pool.Count != 0 {
		t.Fatalf("after remove all: %+v", pool)
	}

	if msg := toolError(t, s, "create_job", map[string]any{"name": "x", "searches": []string{"x"}, "max_minutes": 1}); !strings.Contains(msg, "max_minutes") {
		t.Fatalf("error = %q", msg)
	}
}

func TestMCPFindsByNameWithHelpfulErrors(t *testing.T) {
	s := mcpSession(t)

	call[createJobOutput](t, s, "create_job", map[string]any{"name": "Escolas POA", "searches": []string{"escolas porto alegre"}})

	got := call[jobOutput](t, s, "get_job", map[string]any{"job": "escolas poa"})
	if got.Job.Name != "Escolas POA" || !strings.Contains(got.NextStep, "wait_for_job") {
		t.Fatalf("lookup by name: %+v", got)
	}

	if msg := toolError(t, s, "get_job", map[string]any{"job": "padarias"}); !strings.Contains(msg, `"Escolas POA"`) {
		t.Fatalf("error should list existing jobs: %q", msg)
	}

	if msg := toolError(t, s, "get_campaign", map[string]any{"campaign": "nada"}); !strings.Contains(msg, "create_campaign") {
		t.Fatalf("error should point to create_campaign: %q", msg)
	}
}

func TestMCPCampaignFlow(t *testing.T) {
	s := mcpSession(t)

	job := call[createJobOutput](t, s, "create_job", map[string]any{"name": "escolas", "searches": []string{"escolas porto alegre"}, "campaign": "Escolas RS"})
	if !job.CampaignCreated || job.Job.Campaign != "Escolas RS" {
		t.Fatalf("campaign not created with the job: %+v", job)
	}

	// Creating a campaign that exists returns it instead of a duplicate.
	again := call[campaignDetail](t, s, "create_campaign", map[string]any{"name": "escolas rs"})
	if again.Warning == "" || len(again.Jobs) != 1 {
		t.Fatalf("duplicate campaign: %+v", again)
	}

	detail := call[campaignDetail](t, s, "get_campaign", map[string]any{"campaign": "Escolas RS"})
	if !strings.Contains(detail.NextStep, "configure_campaign_email") {
		t.Fatalf("next step = %q", detail.NextStep)
	}

	mail := call[mailOutput](t, s, "configure_campaign_email", map[string]any{"campaign": "Escolas RS", "smtp_host": "mailpit", "smtp_port": 1025, "encryption": "NONE"})
	if !strings.Contains(mail.NextStep, "smtp_password") || !strings.Contains(mail.NextStep, "subject") {
		t.Fatalf("missing fields not reported: %q", mail.NextStep)
	}

	mail = call[mailOutput](t, s, "configure_campaign_email", map[string]any{
		"campaign": "Escolas RS", "smtp_password": "x", "from_email": "eu@example.com", "subject": "Olá {{.Title}}", "body": "Oi {{.Title}}",
	})
	if mail.Email.Host != "mailpit" || !mail.Email.PasswordSet || strings.Contains(mail.NextStep, "missing") {
		t.Fatalf("partial update lost fields: %+v", mail)
	}

	preview := call[previewOutput](t, s, "preview_campaign_email", map[string]any{"campaign": "Escolas RS"})
	if preview.Subject == "" || strings.Contains(preview.Subject, "{{") {
		t.Fatalf("preview not rendered: %+v", preview)
	}

	// Without confirm nothing is sent.
	dry := call[dispatchOutput](t, s, "start_campaign_dispatch", map[string]any{"campaign": "Escolas RS"})
	if dry.Sent || !strings.Contains(dry.NextStep, "confirm=true") {
		t.Fatalf("dispatch without confirm: %+v", dry)
	}

	call[message](t, s, "remove_job_from_campaign", map[string]any{"campaign": "Escolas RS", "job": "escolas"})
	call[message](t, s, "delete_campaign", map[string]any{"campaign": "Escolas RS"})
	call[message](t, s, "delete_job", map[string]any{"job": "escolas"})
}
