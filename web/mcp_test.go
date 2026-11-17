//nolint:testpackage // drives the MCP endpoint against the in-memory store
package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
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
		"list_jobs", "get_job", "create_job", "delete_job", "get_job_results",
		"list_proxies", "add_proxies", "remove_proxies", "set_proxies", "test_proxies",
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

	if msg := toolError(t, s, "add_proxies", map[string]any{"proxies": []string{"ftp://nope"}}); msg == "" {
		t.Fatal("expected invalid proxy error")
	}

	job := call[jobDetail](t, s, "create_job", map[string]any{"name": "escolas", "queries": []string{"escolas porto alegre"}})
	if job.ProxyCount != 2 || job.Lang != "pt" || job.RadiusKM != 50 || !job.Emails || job.Status != StatusPending {
		t.Fatalf("job defaults wrong: %+v", job)
	}

	noProxy := call[jobDetail](t, s, "create_job", map[string]any{"name": "direto", "queries": []string{"x"}, "use_saved_proxies": false})
	if noProxy.ProxyCount != 0 {
		t.Fatalf("use_saved_proxies=false still used %d proxies", noProxy.ProxyCount)
	}

	pool = call[proxiesOutput](t, s, "remove_proxies", map[string]any{"proxies": []string{"socks5://5.6.7.8:1080"}})
	if pool.Count != 1 {
		t.Fatalf("after remove: %+v", pool)
	}

	if msg := toolError(t, s, "create_job", map[string]any{"name": "x", "queries": []string{"x"}, "max_time": "1m"}); msg == "" {
		t.Fatal("expected max_time error")
	}
}

func TestMCPCampaignFlow(t *testing.T) {
	s := mcpSession(t)

	c := call[campaignDetail](t, s, "create_campaign", map[string]any{"name": "Escolas POA"})
	job := call[jobDetail](t, s, "create_job", map[string]any{"name": "escolas", "queries": []string{"escolas porto alegre"}, "campaign_id": c.ID})

	if job.CampaignID != c.ID {
		t.Fatalf("job not in campaign: %+v", job)
	}

	mail := call[mailView](t, s, "configure_campaign_email", map[string]any{
		"id": c.ID, "smtp_host": "mailpit", "smtp_port": 1025, "encryption": "none", "smtp_password": "x",
		"from_email": "eu@example.com", "subject": "Olá {{.Title}}", "body": "Oi {{.Title}}",
	})
	if mail.Host != "mailpit" || !mail.PasswordSet || mail.Subject != "Olá {{.Title}}" {
		t.Fatalf("mail = %+v", mail)
	}

	// A partial update keeps the other fields and the password.
	mail = call[mailView](t, s, "configure_campaign_email", map[string]any{"id": c.ID, "from_name": "Equipe"})
	if mail.Host != "mailpit" || !mail.PasswordSet || mail.FromName != "Equipe" {
		t.Fatalf("partial update lost fields: %+v", mail)
	}

	preview := call[previewOutput](t, s, "preview_campaign_email", map[string]any{"id": c.ID})
	if preview.Subject == "" || preview.Subject == "Olá {{.Title}}" {
		t.Fatalf("preview not rendered: %+v", preview)
	}

	detail := call[campaignDetail](t, s, "get_campaign", map[string]any{"id": c.ID})
	if len(detail.Jobs) != 1 || detail.Email.Host != "mailpit" {
		t.Fatalf("detail = %+v", detail)
	}

	list := call[listCampaignsOutput](t, s, "list_campaigns", nil)
	if len(list.Campaigns) != 1 || list.Campaigns[0].Jobs != 1 {
		t.Fatalf("list = %+v", list)
	}

	call[message](t, s, "remove_job_from_campaign", map[string]any{"campaign_id": c.ID, "job_id": job.ID})
	call[message](t, s, "delete_campaign", map[string]any{"id": c.ID})
	call[message](t, s, "delete_job", map[string]any{"id": job.ID})
}
