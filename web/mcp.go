package web

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
	"uuid"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	mcpServerName      = "google-maps-scraper"
	mcpServerVersion   = "1.1.0"
	mcpDefaultLimit    = 20
	mcpMaxLimit        = 500
	mcpDefaultDepth    = 10
	mcpDefaultZoom     = 15
	mcpDefaultMinutes  = 10
	mcpMinMinutes      = 3
	mcpMaxMinutes      = 120
	mcpDefaultRadiusKM = 50
	metersPerKM        = 1000
	// mcpMaxWait stays under the server's 60s write timeout.
	mcpMaxWait     = 45 * time.Second
	mcpWaitPoll    = 3 * time.Second
	mcpSampleSize  = 5
	mcpListPreview = 10
)

const mcpInstructions = `Google Maps business scraper with email campaigns.

Typical workflow:
1. create_job with a name and one or more searches like "escolas porto alegre". Optionally pass campaign (a name; it is created if missing).
2. wait_for_job with the job name or id. Repeat while it says the job is still running.
3. get_job_results to read the places found (only_with_email=true for places with email).
4. To email the businesses: configure_campaign_email, then preview_campaign_email and send_test_email.
5. start_campaign_dispatch sends real email. Call it once without confirm to see how many would be sent, ask the user, then call again with confirm=true.

Jobs and campaigns can be referred to by name or by id. Every answer has a next_step hint.`

// defaultResultColumns are the CSV columns returned unless all columns are asked for.
var defaultResultColumns = []string{
	"title", "category", "address", "phone", "website", "emails", "website_phones",
	"review_rating", "review_count", "latitude", "longitude", "link",
}

// mcpHandler serves the MCP endpoint. Every session shares one server.
func (s *Server) mcpHandler() http.Handler {
	server := s.newMCPServer()

	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
}

func ptr[T any](v T) *T { return &v }

func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: ptr(false)}
}

func writes(title string, destructive bool) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: ptr(destructive), OpenWorldHint: ptr(false)}
}

func external(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: ptr(false), OpenWorldHint: ptr(true)}
}

func tool(name, title, description string, ann *mcp.ToolAnnotations) *mcp.Tool {
	ann.Title = title

	return &mcp.Tool{Name: name, Description: description, Annotations: ann}
}

//nolint:funlen // one registration per tool keeps the tool list readable.
func (s *Server) newMCPServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: mcpServerName, Version: mcpServerVersion},
		&mcp.ServerOptions{Instructions: mcpInstructions})

	// Jobs
	mcp.AddTool(server, tool("create_job", "Create job",
		`Start a Google Maps search job. It runs in the background; next call wait_for_job.
Example: {"name": "escolas poa", "searches": ["escolas porto alegre", "escolas canoas"], "campaign": "Escolas RS"}`,
		writes("", false)), s.mcpCreateJob)
	mcp.AddTool(server, tool("wait_for_job", "Wait for job",
		"Wait up to 45 seconds for a job to finish, then report its status, places found and a sample. Call again if still running.",
		readOnly("")), s.mcpWaitJob)
	mcp.AddTool(server, tool("get_job", "Get job", "Show a job's settings, status and progress.", readOnly("")), s.mcpGetJob)
	mcp.AddTool(server, tool("list_jobs", "List jobs", "List jobs, newest first.", readOnly("")), s.mcpListJobs)
	mcp.AddTool(server, tool("get_job_results", "Get job results",
		"Read the places a job found. Use only_with_email=true to get only places that have an email. Use offset to page.",
		readOnly("")), s.mcpJobResults)
	mcp.AddTool(server, tool("delete_job", "Delete job", "Delete a job and its results. Cannot be undone.", writes("", true)), s.mcpDeleteJob)

	// Proxies
	mcp.AddTool(server, tool("list_proxies", "List proxies", "Show the saved proxies. New jobs use them automatically.", readOnly("")), s.mcpListProxies)
	mcp.AddTool(server, tool("add_proxies", "Add proxies",
		`Save proxies for new jobs. Format: http://user:pass@host:port, https://host:port or socks5://host:port.`,
		writes("", false)), s.mcpAddProxies)
	mcp.AddTool(server, tool("remove_proxies", "Remove proxies", "Remove saved proxies. Pass all=true to remove every proxy.", writes("", true)), s.mcpRemoveProxies)
	mcp.AddTool(server, tool("test_proxies", "Test proxies",
		"Check that each saved proxy can reach Google Maps. remove_failed=true deletes the ones that fail.",
		external("")), s.mcpTestProxies)
	mcp.AddTool(server, tool("proxyscrape_status", "ProxyScrape status",
		"Test the ProxyScrape API key (PROXYSCRAPE_API_KEY in .env): plans, how many proxies it gives, wallet balance. When the key is set, new jobs use these proxies by default.",
		external("")), s.mcpProxyScrapeStatus)
	mcp.AddTool(server, tool("proxyscrape_order", "Order ProxyScrape proxies",
		`Buy or renew a ProxyScrape plan, paid from the wallet. Without confirm it only quotes the price (dry run); show it to the user, then call again with confirm=true.
Needs API ordering enabled by ProxyScrape support. Fields follow https://docs.proxyscrape.com/api/account/api-ordering
Example: {"order": {"type": "new_datacenter_shared", "country": "Brazil", "proxies": 10, "days": 30, "is_subscription": false}}`,
		writes("", false)), s.mcpProxyScrapeOrder)
	mcp.AddTool(server, tool("proxyscrape_order_status", "ProxyScrape order status",
		`Check a ProxyScrape order. When status is "processed", the new proxies are used by new jobs.`,
		external("")), s.mcpProxyScrapeOrderStatus)

	// Campaigns
	mcp.AddTool(server, tool("list_campaigns", "List campaigns", "List campaigns with their job count and email status.", readOnly("")), s.mcpListCampaigns)
	mcp.AddTool(server, tool("get_campaign", "Get campaign",
		"Show a campaign: its jobs, email settings, how many addresses are ready to receive email, and send history.",
		readOnly("")), s.mcpGetCampaign)
	mcp.AddTool(server, tool("create_campaign", "Create campaign", "Create a campaign. A campaign groups jobs and has its own email settings.", writes("", false)), s.mcpCreateCampaign)
	mcp.AddTool(server, tool("rename_campaign", "Rename campaign", "Rename a campaign.", writes("", false)), s.mcpRenameCampaign)
	mcp.AddTool(server, tool("delete_campaign", "Delete campaign", "Delete a campaign with its email settings and history. Jobs are kept.", writes("", true)), s.mcpDeleteCampaign)
	mcp.AddTool(server, tool("add_job_to_campaign", "Add job to campaign", "Put a job in a campaign. The campaign is created if it does not exist.", writes("", false)), s.mcpAssignJob)
	mcp.AddTool(server, tool("remove_job_from_campaign", "Remove job from campaign", "Take a job out of its campaign. The job is kept.", writes("", false)), s.mcpUnassignJob)

	// Campaign email
	mcp.AddTool(server, tool("configure_campaign_email", "Configure campaign email",
		`Save the campaign's SMTP server and message. Only the fields you pass change.
In subject and body you can use {{.Title}} {{.Email}} {{.Phone}} {{.Website}} {{.Address}} {{.Category}}.
Example: {"campaign": "Escolas RS", "smtp_host": "smtp.gmail.com", "smtp_port": 587, "encryption": "starttls", "smtp_username": "me@gmail.com", "smtp_password": "app-password", "from_email": "me@gmail.com", "subject": "Olá {{.Title}}", "body": "..."}`,
		writes("", false)), s.mcpConfigureMail)
	mcp.AddTool(server, tool("test_campaign_smtp", "Test SMTP", "Check that the campaign's SMTP server accepts the login. Sends nothing.", external("")), s.mcpCheckMail)
	mcp.AddTool(server, tool("preview_campaign_email", "Preview email", "Show the campaign's email filled with sample business data. Sends nothing.", readOnly("")), s.mcpPreviewMail)
	mcp.AddTool(server, tool("send_test_email", "Send test email", "Send one test email to the address in to.", external("")), s.mcpSendTestMail)
	mcp.AddTool(server, tool("start_campaign_dispatch", "Send campaign emails",
		"Email every business the campaign's jobs found. Without confirm=true it only reports how many would be sent. "+
			"Always ask the user before calling with confirm=true. Addresses that already got this campaign are skipped.",
		&mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)}), s.mcpStartDispatch)

	return server
}

// ---------- lookups by name or id

func (s *Server) findJob(ctx context.Context, ref string) (Job, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Job{}, errors.New("job is required: pass the job name or id (see list_jobs)")
	}

	jobs, err := s.svc.All(ctx)
	if err != nil {
		return Job{}, err
	}

	var byName []Job

	for i := range jobs {
		if jobs[i].ID == ref {
			return jobs[i], nil
		}

		if strings.EqualFold(jobs[i].Name, ref) {
			byName = append(byName, jobs[i])
		}
	}

	switch len(byName) {
	case 1:
		return byName[0], nil
	case 0:
		names := make([]string, 0, mcpListPreview)
		for i := 0; i < len(jobs) && i < mcpListPreview; i++ {
			names = append(names, fmt.Sprintf("%q", jobs[i].Name))
		}

		return Job{}, fmt.Errorf("no job named %q. Recent jobs: %s. Call list_jobs to see all", ref, strings.Join(names, ", "))
	default:
		ids := make([]string, 0, len(byName))
		for i := range byName {
			ids = append(ids, byName[i].ID)
		}

		return Job{}, fmt.Errorf("%d jobs are named %q; pass one of these ids instead: %s", len(byName), ref, strings.Join(ids, ", "))
	}
}

func (s *Server) findCampaign(ctx context.Context, ref string) (CampaignSummary, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return CampaignSummary{}, errors.New("campaign is required: pass the campaign name or id (see list_campaigns)")
	}

	items, err := s.svc.ListCampaigns(ctx)
	if err != nil {
		return CampaignSummary{}, errors.New(publicErr(err))
	}

	for _, c := range items {
		if c.ID == ref || strings.EqualFold(c.Name, ref) {
			return c, nil
		}
	}

	names := make([]string, 0, len(items))
	for _, c := range items {
		names = append(names, fmt.Sprintf("%q", c.Name))
	}

	if len(names) == 0 {
		return CampaignSummary{}, fmt.Errorf("no campaign named %q and there are no campaigns yet. Call create_campaign first", ref)
	}

	return CampaignSummary{}, fmt.Errorf("no campaign named %q. Existing campaigns: %s", ref, strings.Join(names, ", "))
}

// findOrCreateCampaign returns the campaign, creating it when no campaign has that name.
func (s *Server) findOrCreateCampaign(ctx context.Context, ref string) (Campaign, bool, error) {
	if c, err := s.findCampaign(ctx, ref); err == nil {
		return c.Campaign, false, nil
	}

	if _, err := uuid.Parse(strings.TrimSpace(ref)); err == nil {
		return Campaign{}, false, fmt.Errorf("no campaign has id %s. Call list_campaigns", ref)
	}

	c := Campaign{ID: uuid.NewV4().String(), Name: strings.TrimSpace(ref)}
	if err := s.svc.CreateCampaign(ctx, &c); err != nil {
		return Campaign{}, false, errors.New(publicErr(err))
	}

	return c, true, nil
}

// ---------- jobs

type jobRef struct {
	Job string `json:"job" jsonschema:"job name or id"`
}

type jobView struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Status       string   `json:"status" jsonschema:"pending (queued), working (running), ok (finished) or failed"`
	Places       int      `json:"places_found"`
	Searches     []string `json:"searches,omitempty"`
	Campaign     string   `json:"campaign,omitempty"`
	CreatedAt    string   `json:"created_at"`
	Elapsed      string   `json:"elapsed,omitempty"`
	MaxMinutes   int      `json:"max_minutes,omitempty"`
	RadiusKM     float64  `json:"radius_km,omitempty"`
	Lang         string   `json:"lang,omitempty"`
	CollectEmail bool     `json:"collect_emails"`
	Proxies      int      `json:"proxies_used"`
	Error        string   `json:"error,omitempty"`
}

func (s *Server) view(j *Job, full bool) jobView {
	places := j.Data.Run.Places
	if live, ok := s.svc.Progress(j.ID); ok {
		places = live
	}

	v := jobView{
		ID: j.ID, Name: j.Name, Status: j.Status, Places: places, Campaign: j.CampaignName,
		CreatedAt: j.Date.UTC().Format(time.RFC3339), CollectEmail: j.Data.Email, Proxies: len(j.Data.Proxies),
		Error: j.Data.Run.Error,
	}

	if j.Data.Run.Started() {
		v.Elapsed = j.ElapsedLabel()
	}

	if full {
		v.Searches = j.Data.Keywords
		v.MaxMinutes = int(j.Data.MaxTime.Minutes())
		v.RadiusKM = float64(j.Data.Radius) / metersPerKM
		v.Lang = j.Data.Lang
	}

	return v
}

func jobNextStep(j *jobView) string {
	switch j.Status {
	case StatusOK:
		return fmt.Sprintf("Job finished. Call get_job_results with job=%q to read the places.", j.Name)
	case StatusFailed:
		if j.Error != "" {
			return "Job failed: " + j.Error + ". Tell the user; do not retry more than once."
		}

		return "Job failed. Tell the user; do not retry more than once."
	default:
		return fmt.Sprintf("Job is %s. Call wait_for_job with job=%q.", j.Status, j.Name)
	}
}

type jobOutput struct {
	Job      jobView `json:"job"`
	NextStep string  `json:"next_step"`
}

func (s *Server) mcpGetJob(ctx context.Context, _ *mcp.CallToolRequest, in jobRef) (*mcp.CallToolResult, jobOutput, error) {
	j, err := s.findJob(ctx, in.Job)
	if err != nil {
		return nil, jobOutput{}, err
	}

	v := s.view(&j, true)

	return nil, jobOutput{Job: v, NextStep: jobNextStep(&v)}, nil
}

type listJobsInput struct {
	Page int `json:"page,omitempty" jsonschema:"page number, starting at 1"`
}

type listJobsOutput struct {
	Jobs       []jobView `json:"jobs"`
	Page       int       `json:"page"`
	TotalPages int       `json:"total_pages"`
	Running    int       `json:"running"`
	Queued     int       `json:"queued"`
}

func (s *Server) mcpListJobs(ctx context.Context, _ *mcp.CallToolRequest, in listJobsInput) (*mcp.CallToolResult, listJobsOutput, error) {
	jp, err := s.svc.ListJobs(ctx, max(in.Page, 1), jobsPageLimit)
	if err != nil {
		return nil, listJobsOutput{}, err
	}

	out := listJobsOutput{Jobs: make([]jobView, 0, len(jp.Jobs)), Page: jp.CurrentPage, TotalPages: jp.TotalPages, Running: jp.Running, Queued: jp.Queued}
	for i := range jp.Jobs {
		out.Jobs = append(out.Jobs, s.view(&jp.Jobs[i], false))
	}

	return nil, out, nil
}

type createJobInput struct {
	Name          string   `json:"name" jsonschema:"a short name for the job"`
	Searches      []string `json:"searches" jsonschema:"Google Maps searches, each with what and where, e.g. [\"escolas porto alegre\"]"`
	Campaign      string   `json:"campaign,omitempty" jsonschema:"campaign name or id to put the job in; created if it does not exist"`
	MaxMinutes    int      `json:"max_minutes,omitempty" jsonschema:"time limit in minutes, 3 to 120, default 10"`
	RadiusKM      float64  `json:"radius_km,omitempty" jsonschema:"drop places farther than this many km from the searched area, default 50"`
	CollectEmails *bool    `json:"collect_emails,omitempty" jsonschema:"visit each business website to find emails, default true"`
	Lang          string   `json:"lang,omitempty" jsonschema:"two-letter results language, default pt"`
	Depth         int      `json:"depth,omitempty" jsonschema:"how far to scroll the results list; more finds more places, default 10"`
	Lat           *float64 `json:"lat,omitempty" jsonschema:"optional search center latitude"`
	Lon           *float64 `json:"lon,omitempty" jsonschema:"optional search center longitude"`
	UseProxies    *bool    `json:"use_proxies,omitempty" jsonschema:"use the saved proxies, default true"`
}

type createJobOutput struct {
	Job             jobView `json:"job"`
	CampaignCreated bool    `json:"campaign_created,omitempty"`
	NextStep        string  `json:"next_step"`
}

func (s *Server) mcpCreateJob(ctx context.Context, _ *mcp.CallToolRequest, in createJobInput) (*mcp.CallToolResult, createJobOutput, error) {
	job, err := s.buildJob(ctx, &in)
	if err != nil {
		return nil, createJobOutput{}, err
	}

	var (
		campaign Campaign
		created  bool
	)

	if strings.TrimSpace(in.Campaign) != "" {
		campaign, created, err = s.findOrCreateCampaign(ctx, in.Campaign)
		if err != nil {
			return nil, createJobOutput{}, err
		}
	}

	if err := s.svc.Create(ctx, &job); err != nil {
		return nil, createJobOutput{}, err
	}

	if campaign.ID != "" {
		if err := s.svc.AssignJob(ctx, campaign.ID, job.ID); err != nil {
			return nil, createJobOutput{}, errors.New(publicErr(err))
		}

		job.CampaignID, job.CampaignName = campaign.ID, campaign.Name
	}

	v := s.view(&job, true)

	return nil, createJobOutput{
		Job: v, CampaignCreated: created,
		NextStep: fmt.Sprintf("Job queued. Call wait_for_job with job=%q until it finishes (about %d minutes at most).", job.Name, v.MaxMinutes),
	}, nil
}

func (s *Server) buildJob(ctx context.Context, in *createJobInput) (Job, error) {
	job := Job{
		ID:     uuid.NewV4().String(),
		Name:   strings.TrimSpace(in.Name),
		Date:   time.Now().UTC(),
		Status: StatusPending,
	}

	if job.Name == "" {
		return Job{}, errors.New(`name is required, e.g. "escolas poa"`)
	}

	for _, q := range in.Searches {
		// Accept several searches in one string, one per line.
		for _, line := range strings.Split(q, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				job.Data.Keywords = append(job.Data.Keywords, line)
			}
		}
	}

	if len(job.Data.Keywords) == 0 {
		return Job{}, errors.New(`searches is required, e.g. ["escolas porto alegre"]`)
	}

	minutes := orDefault(in.MaxMinutes, mcpDefaultMinutes)
	if minutes < mcpMinMinutes || minutes > mcpMaxMinutes {
		return Job{}, fmt.Errorf("max_minutes must be between %d and %d", mcpMinMinutes, mcpMaxMinutes)
	}

	job.Data.MaxTime = time.Duration(minutes) * time.Minute
	job.Data.Lang = strings.ToLower(orDefault(strings.TrimSpace(in.Lang), "pt"))
	job.Data.Depth = orDefault(in.Depth, mcpDefaultDepth)
	job.Data.Zoom = mcpDefaultZoom
	job.Data.Radius = int(orDefault(in.RadiusKM, mcpDefaultRadiusKM) * metersPerKM)
	job.Data.Email = in.CollectEmails == nil || *in.CollectEmails

	if (in.Lat == nil) != (in.Lon == nil) {
		return Job{}, errors.New("give both lat and lon, or neither")
	}

	if in.Lat != nil {
		job.Data.Lat = fmt.Sprint(*in.Lat)
		job.Data.Lon = fmt.Sprint(*in.Lon)
	}

	if in.UseProxies == nil || *in.UseProxies {
		saved, err := s.svc.DefaultProxies(ctx)
		if err != nil {
			return Job{}, err
		}

		job.Data.Proxies = saved
	}

	if len(job.Data.Proxies) == 0 && s.svc.RequiresProxies() {
		return Job{}, ErrNoProxies
	}

	return job, job.Validate()
}

func orDefault[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}

	return v
}

type waitOutput struct {
	Job        jobView             `json:"job"`
	Finished   bool                `json:"finished"`
	WithEmail  int                 `json:"places_with_email,omitempty"`
	SamplePlcs []map[string]string `json:"sample_places,omitempty"`
	NextStep   string              `json:"next_step"`
}

func (s *Server) mcpWaitJob(ctx context.Context, _ *mcp.CallToolRequest, in jobRef) (*mcp.CallToolResult, waitOutput, error) {
	j, err := s.findJob(ctx, in.Job)
	if err != nil {
		return nil, waitOutput{}, err
	}

	deadline := time.Now().Add(mcpMaxWait)

	for (j.Status == StatusPending || j.Status == StatusWorking) && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, waitOutput{}, ctx.Err()
		case <-time.After(mcpWaitPoll):
		}

		if j, err = s.svc.Get(ctx, j.ID); err != nil {
			return nil, waitOutput{}, err
		}
	}

	_ = s.svc.attachCampaigns(ctx, []Job{j})

	v := s.view(&j, false)
	out := waitOutput{Job: v, Finished: j.Status == StatusOK || j.Status == StatusFailed}

	if j.Status != StatusOK {
		out.NextStep = jobNextStep(&v)
		if !out.Finished {
			out.NextStep = fmt.Sprintf("Still %s with %d places so far. Call wait_for_job again with job=%q.", j.Status, v.Places, j.Name)
		}

		return nil, out, nil
	}

	if res, err := s.readResults(j.ID); err == nil {
		out.Job.Places = res.Total
		out.WithEmail = res.WithEmail

		for i := 0; i < len(res.rows) && i < mcpSampleSize; i++ {
			out.SamplePlcs = append(out.SamplePlcs, pick(res.header, res.rows[i], []string{"title", "address", "phone", "emails"}))
		}
	}

	out.NextStep = fmt.Sprintf("Finished: %d places, %d with email. Call get_job_results with job=%q for the full list.", out.Job.Places, out.WithEmail, j.Name)

	return nil, out, nil
}

func (s *Server) mcpDeleteJob(ctx context.Context, _ *mcp.CallToolRequest, in jobRef) (*mcp.CallToolResult, message, error) {
	j, err := s.findJob(ctx, in.Job)
	if err != nil {
		return nil, message{}, err
	}

	if err := s.svc.Delete(ctx, j.ID); err != nil {
		return nil, message{}, err
	}

	return nil, message{Message: fmt.Sprintf("Job %q and its results were deleted.", j.Name)}, nil
}

// ---------- results

type csvResults struct {
	header    []string
	rows      [][]string
	Total     int
	WithEmail int
	Path      string
}

func (s *Server) readResults(id string) (csvResults, error) {
	path, err := s.svc.csvPath(id)
	if err != nil {
		return csvResults{}, err
	}

	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return csvResults{}, errors.New("this job has no results yet. Call wait_for_job first")
	}

	if err != nil {
		return csvResults{}, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1

	rows, err := r.ReadAll()
	if err != nil {
		return csvResults{}, fmt.Errorf("read results: %w", err)
	}

	res := csvResults{Path: path}
	if len(rows) == 0 {
		return res, nil
	}

	res.header, res.rows = rows[0], rows[1:]
	res.Total = len(res.rows)

	emailCol := index(res.header, "emails")
	for _, row := range res.rows {
		if emailCol >= 0 && emailCol < len(row) && strings.TrimSpace(row[emailCol]) != "" {
			res.WithEmail++
		}
	}

	return res, nil
}

func index(header []string, name string) int {
	for i, h := range header {
		if h == name {
			return i
		}
	}

	return -1
}

func pick(header, row, cols []string) map[string]string {
	out := make(map[string]string, len(cols))

	for _, c := range cols {
		if i := index(header, c); i >= 0 && i < len(row) && row[i] != "" {
			out[c] = row[i]
		}
	}

	return out
}

type resultsInput struct {
	Job           string `json:"job" jsonschema:"job name or id"`
	OnlyWithEmail bool   `json:"only_with_email,omitempty" jsonschema:"return only places that have an email"`
	Offset        int    `json:"offset,omitempty" jsonschema:"places to skip, for paging"`
	Limit         int    `json:"limit,omitempty" jsonschema:"places to return, default 20, max 500"`
	AllColumns    bool   `json:"all_columns,omitempty" jsonschema:"return every column (reviews, hours, etc.) instead of the main ones"`
}

type resultsOutput struct {
	Total     int                 `json:"total_places"`
	WithEmail int                 `json:"places_with_email"`
	Matching  int                 `json:"matching"`
	Offset    int                 `json:"offset"`
	Places    []map[string]string `json:"places"`
	CSVPath   string              `json:"csv_path"`
	NextStep  string              `json:"next_step"`
}

func (s *Server) mcpJobResults(ctx context.Context, _ *mcp.CallToolRequest, in resultsInput) (*mcp.CallToolResult, resultsOutput, error) {
	j, err := s.findJob(ctx, in.Job)
	if err != nil {
		return nil, resultsOutput{}, err
	}

	res, err := s.readResults(j.ID)
	if err != nil {
		return nil, resultsOutput{}, err
	}

	cols := defaultResultColumns
	if in.AllColumns {
		cols = res.header
	}

	emailCol := index(res.header, "emails")
	matching := make([][]string, 0, len(res.rows))

	for _, row := range res.rows {
		if in.OnlyWithEmail && (emailCol < 0 || emailCol >= len(row) || strings.TrimSpace(row[emailCol]) == "") {
			continue
		}

		matching = append(matching, row)
	}

	out := resultsOutput{
		Total: res.Total, WithEmail: res.WithEmail, Matching: len(matching),
		Offset: max(in.Offset, 0), Places: []map[string]string{}, CSVPath: res.Path,
	}

	limit := min(orDefault(in.Limit, mcpDefaultLimit), mcpMaxLimit)
	for i := out.Offset; i < len(matching) && len(out.Places) < limit; i++ {
		out.Places = append(out.Places, pick(res.header, matching[i], cols))
	}

	if end := out.Offset + len(out.Places); end < len(matching) {
		out.NextStep = fmt.Sprintf("Showing %d-%d of %d. Call again with offset=%d for more.", out.Offset+1, end, len(matching), end)
	} else {
		out.NextStep = "That is all of them. To email these businesses, put the job in a campaign and use configure_campaign_email."
	}

	return nil, out, nil
}

// ---------- proxies

type message struct {
	Message string `json:"message"`
}

type proxiesInput struct {
	Proxies []string `json:"proxies" jsonschema:"proxy URLs like http://user:pass@host:port or socks5://host:port"`
}

type removeProxiesInput struct {
	Proxies []string `json:"proxies,omitempty" jsonschema:"proxies to remove"`
	All     bool     `json:"all,omitempty" jsonschema:"remove every saved proxy"`
}

type proxiesOutput struct {
	Proxies []string `json:"proxies"`
	Count   int      `json:"count"`
}

func pool(p []string, err error) (*mcp.CallToolResult, proxiesOutput, error) {
	if err != nil {
		return nil, proxiesOutput{}, err
	}

	return nil, proxiesOutput{Proxies: p, Count: len(p)}, nil
}

func (s *Server) mcpListProxies(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, proxiesOutput, error) {
	return pool(s.svc.SavedProxies())
}

func (s *Server) mcpAddProxies(_ context.Context, _ *mcp.CallToolRequest, in proxiesInput) (*mcp.CallToolResult, proxiesOutput, error) {
	return pool(s.svc.AddSavedProxies(in.Proxies))
}

func (s *Server) mcpRemoveProxies(_ context.Context, _ *mcp.CallToolRequest, in removeProxiesInput) (*mcp.CallToolResult, proxiesOutput, error) {
	if in.All {
		return pool(s.svc.SetSavedProxies(nil))
	}

	if len(in.Proxies) == 0 {
		return nil, proxiesOutput{}, errors.New("pass proxies to remove, or all=true to remove every proxy")
	}

	return pool(s.svc.RemoveSavedProxies(in.Proxies))
}

type testProxiesInput struct {
	Proxies      []string `json:"proxies,omitempty" jsonschema:"proxies to test; leave empty to test the saved ones"`
	RemoveFailed bool     `json:"remove_failed,omitempty" jsonschema:"delete saved proxies that fail"`
}

type testProxiesOutput struct {
	Results []ProxyCheck `json:"results"`
	Working int          `json:"working"`
	Failing int          `json:"failing"`
	Removed []string     `json:"removed,omitempty"`
}

func (s *Server) mcpTestProxies(ctx context.Context, _ *mcp.CallToolRequest, in testProxiesInput) (*mcp.CallToolResult, testProxiesOutput, error) {
	proxies := in.Proxies
	if len(proxies) == 0 {
		saved, err := s.svc.SavedProxies()
		if err != nil {
			return nil, testProxiesOutput{}, err
		}

		proxies = saved
	}

	if len(proxies) == 0 {
		return nil, testProxiesOutput{}, errors.New("there are no saved proxies to test. Add some with add_proxies")
	}

	out := testProxiesOutput{Results: TestProxies(ctx, proxies)}

	var failed []string

	for _, r := range out.Results {
		if r.OK {
			out.Working++
		} else {
			out.Failing++
			failed = append(failed, r.Proxy)
		}
	}

	if in.RemoveFailed && len(failed) > 0 {
		if _, err := s.svc.RemoveSavedProxies(failed); err != nil {
			return nil, out, err
		}

		out.Removed = failed
	}

	return nil, out, nil
}

// ---------- ProxyScrape

func (s *Server) mcpProxyScrapeStatus(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ProxyScrapeStatus, error) {
	return nil, s.svc.ProxyScrapeStatus(ctx, true), nil
}

type proxyScrapeOrderInput struct {
	Order   map[string]any `json:"order" jsonschema:"order fields from the ProxyScrape docs; must include type, e.g. new_datacenter_shared"`
	Confirm bool           `json:"confirm,omitempty" jsonschema:"true places the real order and charges the wallet; otherwise only a price quote"`
}

type proxyScrapeOrderOutput struct {
	DryRun   bool            `json:"dry_run"`
	Result   json.RawMessage `json:"result"`
	NextStep string          `json:"next_step"`
}

func (s *Server) mcpProxyScrapeOrder(ctx context.Context, _ *mcp.CallToolRequest, in proxyScrapeOrderInput) (*mcp.CallToolResult, proxyScrapeOrderOutput, error) {
	ps, err := s.svc.ProxyScrapeClient()
	if err != nil {
		return nil, proxyScrapeOrderOutput{}, err
	}

	res, err := ps.CreateOrder(ctx, in.Order, !in.Confirm)
	if err != nil {
		return nil, proxyScrapeOrderOutput{}, err
	}

	out := proxyScrapeOrderOutput{DryRun: !in.Confirm, Result: res}
	if in.Confirm {
		out.NextStep = "Order placed. Call proxyscrape_order_status with the order id until status is processed."
	} else {
		out.NextStep = "This is only a quote. Ask the user, then call again with confirm=true to buy."
	}

	return nil, out, nil
}

type proxyScrapeOrderStatusInput struct {
	OrderID string `json:"order_id" jsonschema:"order id returned by proxyscrape_order"`
}

func (s *Server) mcpProxyScrapeOrderStatus(ctx context.Context, _ *mcp.CallToolRequest, in proxyScrapeOrderStatusInput) (*mcp.CallToolResult, ProxyScrapeOrder, error) {
	ps, err := s.svc.ProxyScrapeClient()
	if err != nil {
		return nil, ProxyScrapeOrder{}, err
	}

	order, err := ps.OrderStatus(ctx, strings.TrimSpace(in.OrderID))
	if err != nil {
		return nil, ProxyScrapeOrder{}, err
	}

	if order.Status == "processed" {
		// Pick up the new plan's proxies right away.
		if _, err := ps.CachedProxies(ctx, true); err != nil {
			log.Printf("proxyscrape refresh: %v", err)
		}
	}

	return nil, *order, nil
}

// ---------- campaigns

type campaignRef struct {
	Campaign string `json:"campaign" jsonschema:"campaign name or id"`
}

type campaignSummary struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Jobs       int    `json:"jobs"`
	EmailReady bool   `json:"email_configured"`
	LastSend   string `json:"last_send,omitempty"`
}

type listCampaignsOutput struct {
	Campaigns []campaignSummary `json:"campaigns"`
}

func (s *Server) mcpListCampaigns(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listCampaignsOutput, error) {
	items, err := s.svc.ListCampaigns(ctx)
	if err != nil {
		return nil, listCampaignsOutput{}, errors.New(publicErr(err))
	}

	out := listCampaignsOutput{Campaigns: make([]campaignSummary, 0, len(items))}
	for _, c := range items {
		out.Campaigns = append(out.Campaigns, campaignSummary{ID: c.ID, Name: c.Name, Jobs: c.JobCount, EmailReady: c.MailReady, LastSend: c.LastStatus})
	}

	return nil, out, nil
}

type mailView struct {
	Host        string `json:"smtp_host"`
	Port        int    `json:"smtp_port"`
	Username    string `json:"smtp_username,omitempty"`
	PasswordSet bool   `json:"password_saved"`
	Encryption  string `json:"encryption"`
	FromEmail   string `json:"from_email"`
	FromName    string `json:"from_name,omitempty"`
	Subject     string `json:"subject"`
	Body        string `json:"body"`
	Format      string `json:"format"`
}

type dispatchView struct {
	Status    string `json:"status"`
	StartedAt string `json:"started_at"`
	Sent      int    `json:"sent"`
	Failed    int    `json:"failed"`
	Skipped   int    `json:"skipped"`
	Detail    string `json:"detail,omitempty"`
}

type failedEmail struct {
	Email string `json:"email"`
	Error string `json:"error"`
}

type campaignDetail struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	Jobs            []jobView      `json:"jobs"`
	Email           mailView       `json:"email"`
	Places          int            `json:"places_found"`
	PlacesWithEmail int            `json:"places_with_email"`
	AddressesFound  int            `json:"email_addresses_found"`
	EmailsAccepted  int            `json:"emails_accepted_by_smtp"`
	EmailsFailed    int            `json:"emails_failed"`
	FailedEmails    []failedEmail  `json:"failed_emails,omitempty"`
	ReadyToSend     int            `json:"addresses_ready_to_send"`
	AlreadySent     int            `json:"addresses_already_sent"`
	JobsWithoutFile int            `json:"jobs_without_results"`
	Sending         bool           `json:"sending_now"`
	History         []dispatchView `json:"send_history"`
	Warning         string         `json:"warning,omitempty"`
	NextStep        string         `json:"next_step"`
}

func (s *Server) campaignDetail(ctx context.Context, id string) (campaignDetail, error) {
	c, err := s.svc.GetCampaign(ctx, id)
	if err != nil {
		return campaignDetail{}, errors.New(publicErr(err))
	}

	mail, passwordSet, err := s.svc.CampaignMailForForm(ctx, id)
	if err != nil {
		return campaignDetail{}, err
	}

	jobs, err := s.svc.CampaignJobList(ctx, id)
	if err != nil {
		return campaignDetail{}, err
	}

	dispatches, err := s.svc.ListDispatches(ctx, id)
	if err != nil {
		return campaignDetail{}, err
	}

	running, err := s.svc.DispatchRunning(ctx, id)
	if err != nil {
		return campaignDetail{}, err
	}

	d := campaignDetail{
		ID: c.ID, Name: c.Name, Sending: running,
		Jobs:    make([]jobView, 0, len(jobs)),
		History: make([]dispatchView, 0, len(dispatches)),
		Email: mailView{
			Host: mail.Host, Port: mail.Port, Username: mail.Username, PasswordSet: passwordSet,
			Encryption: string(mail.Encryption), FromEmail: mail.FromEmail, FromName: mail.FromName,
			Subject: mail.Subject, Body: mail.Body, Format: string(mail.Format),
		},
	}

	for i := range jobs {
		jobs[i].CampaignName = c.Name
		d.Jobs = append(d.Jobs, s.view(&jobs[i], false))
	}

	for i := range dispatches {
		d.History = append(d.History, dispatchView{
			Status: dispatches[i].Status, StartedAt: dispatches[i].StartedAt.UTC().Format(time.RFC3339),
			Sent: dispatches[i].Sent, Failed: dispatches[i].Failed, Skipped: dispatches[i].Skipped, Detail: dispatches[i].Detail,
		})
	}

	if st, err := s.svc.CampaignStats(ctx, id); err == nil {
		d.Places, d.PlacesWithEmail, d.AddressesFound = st.Places, st.WithEmail, st.Addresses
		d.EmailsAccepted, d.EmailsFailed = st.Sent, st.SendFailed

		for _, e := range st.FailedMails {
			d.FailedEmails = append(d.FailedEmails, failedEmail{Email: e.Email, Error: e.Error})
		}
	}

	if preview, err := s.svc.DispatchPreview(ctx, id); err != nil {
		d.Warning = publicErr(err)
	} else {
		d.ReadyToSend, d.AlreadySent, d.JobsWithoutFile = preview.Ready, preview.AlreadySent, preview.MissingResults
	}

	switch {
	case len(d.Jobs) == 0:
		d.NextStep = fmt.Sprintf("The campaign has no jobs. Call create_job with campaign=%q, or add_job_to_campaign.", c.Name)
	case running:
		d.NextStep = "Emails are being sent now. Call get_campaign again in a minute to follow it."
	case mail.Host == "" || mail.Subject == "" || mail.Body == "" || !passwordSet:
		d.NextStep = fmt.Sprintf("Email is not set up. Call configure_campaign_email with campaign=%q.", c.Name)
	case d.ReadyToSend > 0:
		d.NextStep = fmt.Sprintf("%d addresses are ready. Check with preview_campaign_email, then ask the user before start_campaign_dispatch.", d.ReadyToSend)
	default:
		d.NextStep = "No addresses are waiting to be sent."
	}

	return d, nil
}

func (s *Server) mcpGetCampaign(ctx context.Context, _ *mcp.CallToolRequest, in campaignRef) (*mcp.CallToolResult, campaignDetail, error) {
	c, err := s.findCampaign(ctx, in.Campaign)
	if err != nil {
		return nil, campaignDetail{}, err
	}

	d, err := s.campaignDetail(ctx, c.ID)

	return nil, d, err
}

type nameInput struct {
	Name string `json:"name" jsonschema:"campaign name"`
}

func (s *Server) mcpCreateCampaign(ctx context.Context, _ *mcp.CallToolRequest, in nameInput) (*mcp.CallToolResult, campaignDetail, error) {
	if existing, err := s.findCampaign(ctx, in.Name); err == nil {
		d, err := s.campaignDetail(ctx, existing.ID)
		d.Warning = "A campaign with this name already exists; returning it instead of creating another."

		return nil, d, err
	}

	c := Campaign{ID: uuid.NewV4().String(), Name: strings.TrimSpace(in.Name)}
	if err := s.svc.CreateCampaign(ctx, &c); err != nil {
		return nil, campaignDetail{}, errors.New(publicErr(err))
	}

	d, err := s.campaignDetail(ctx, c.ID)

	return nil, d, err
}

type renameInput struct {
	Campaign string `json:"campaign" jsonschema:"current campaign name or id"`
	NewName  string `json:"new_name" jsonschema:"the new name"`
}

func (s *Server) mcpRenameCampaign(ctx context.Context, _ *mcp.CallToolRequest, in renameInput) (*mcp.CallToolResult, message, error) {
	c, err := s.findCampaign(ctx, in.Campaign)
	if err != nil {
		return nil, message{}, err
	}

	if err := s.svc.RenameCampaign(ctx, c.ID, in.NewName); err != nil {
		return nil, message{}, errors.New(publicErr(err))
	}

	return nil, message{Message: fmt.Sprintf("Campaign renamed to %q.", strings.TrimSpace(in.NewName))}, nil
}

func (s *Server) mcpDeleteCampaign(ctx context.Context, _ *mcp.CallToolRequest, in campaignRef) (*mcp.CallToolResult, message, error) {
	c, err := s.findCampaign(ctx, in.Campaign)
	if err != nil {
		return nil, message{}, err
	}

	if err := s.svc.DeleteCampaign(ctx, c.ID); err != nil {
		return nil, message{}, errors.New(publicErr(err))
	}

	return nil, message{Message: fmt.Sprintf("Campaign %q deleted. Its jobs were kept.", c.Name)}, nil
}

type assignInput struct {
	Campaign string `json:"campaign" jsonschema:"campaign name or id"`
	Job      string `json:"job" jsonschema:"job name or id"`
}

func (s *Server) mcpAssignJob(ctx context.Context, _ *mcp.CallToolRequest, in assignInput) (*mcp.CallToolResult, message, error) {
	j, err := s.findJob(ctx, in.Job)
	if err != nil {
		return nil, message{}, err
	}

	c, created, err := s.findOrCreateCampaign(ctx, in.Campaign)
	if err != nil {
		return nil, message{}, err
	}

	if err := s.svc.AssignJob(ctx, c.ID, j.ID); err != nil {
		return nil, message{}, errors.New(publicErr(err))
	}

	msg := fmt.Sprintf("Job %q is now in campaign %q.", j.Name, c.Name)
	if created {
		msg += " The campaign was created."
	}

	return nil, message{Message: msg}, nil
}

func (s *Server) mcpUnassignJob(ctx context.Context, _ *mcp.CallToolRequest, in assignInput) (*mcp.CallToolResult, message, error) {
	j, err := s.findJob(ctx, in.Job)
	if err != nil {
		return nil, message{}, err
	}

	c, err := s.findCampaign(ctx, in.Campaign)
	if err != nil {
		return nil, message{}, err
	}

	if err := s.svc.UnassignJob(ctx, c.ID, j.ID); err != nil {
		return nil, message{}, errors.New(publicErr(err))
	}

	return nil, message{Message: fmt.Sprintf("Job %q removed from campaign %q.", j.Name, c.Name)}, nil
}

// ---------- campaign email

type mailInput struct {
	Campaign   string  `json:"campaign" jsonschema:"campaign name or id"`
	Host       *string `json:"smtp_host,omitempty" jsonschema:"SMTP server, e.g. smtp.gmail.com"`
	Port       *int    `json:"smtp_port,omitempty" jsonschema:"SMTP port: 587 (starttls), 465 (ssl) or 25"`
	Username   *string `json:"smtp_username,omitempty" jsonschema:"SMTP login"`
	Password   *string `json:"smtp_password,omitempty" jsonschema:"SMTP password; leave out to keep the saved one"`
	Encryption *string `json:"encryption,omitempty" jsonschema:"starttls, ssl or none"`
	FromEmail  *string `json:"from_email,omitempty" jsonschema:"sender email address"`
	FromName   *string `json:"from_name,omitempty" jsonschema:"sender name"`
	Subject    *string `json:"subject,omitempty" jsonschema:"subject, may use {{.Title}}"`
	Body       *string `json:"body,omitempty" jsonschema:"message text, may use {{.Title}} {{.Address}} {{.Phone}} etc."`
	Format     *string `json:"format,omitempty" jsonschema:"plain or html, default plain"`
}

type mailOutput struct {
	Email    mailView `json:"email"`
	NextStep string   `json:"next_step"`
}

func set[T any](dst *T, v *T) {
	if v != nil {
		*dst = *v
	}
}

func (s *Server) mcpConfigureMail(ctx context.Context, _ *mcp.CallToolRequest, in mailInput) (*mcp.CallToolResult, mailOutput, error) {
	c, err := s.findCampaign(ctx, in.Campaign)
	if err != nil {
		return nil, mailOutput{}, err
	}

	cfg, _, err := s.svc.CampaignMailForForm(ctx, c.ID)
	if err != nil {
		return nil, mailOutput{}, err
	}

	set(&cfg.Host, in.Host)
	set(&cfg.Port, in.Port)
	set(&cfg.Username, in.Username)
	set(&cfg.Password, in.Password)
	set(&cfg.FromEmail, in.FromEmail)
	set(&cfg.FromName, in.FromName)
	set(&cfg.Subject, in.Subject)
	set(&cfg.Body, in.Body)

	if in.Encryption != nil {
		cfg.Encryption = MailEncryption(strings.ToLower(strings.TrimSpace(*in.Encryption)))
	}

	if in.Format != nil {
		cfg.Format = MailFormat(strings.ToLower(strings.TrimSpace(*in.Format)))
	}

	if err := s.svc.SaveCampaignMail(ctx, c.ID, &cfg); err != nil {
		return nil, mailOutput{}, errors.New(publicErr(err))
	}

	d, err := s.campaignDetail(ctx, c.ID)
	if err != nil {
		return nil, mailOutput{}, err
	}

	next := fmt.Sprintf("Saved. Call test_campaign_smtp and preview_campaign_email with campaign=%q.", c.Name)
	if missing := missingMailFields(&d.Email); missing != "" {
		next = "Saved, but still missing: " + missing + ". Call configure_campaign_email again with those fields."
	}

	return nil, mailOutput{Email: d.Email, NextStep: next}, nil
}

func missingMailFields(m *mailView) string {
	var missing []string

	for _, f := range []struct {
		name string
		ok   bool
	}{
		{"smtp_host", m.Host != ""}, {"smtp_password", m.PasswordSet}, {"from_email", m.FromEmail != ""},
		{"subject", m.Subject != ""}, {"body", m.Body != ""},
	} {
		if !f.ok {
			missing = append(missing, f.name)
		}
	}

	return strings.Join(missing, ", ")
}

// savedMail returns the campaign and its stored settings. The service fills in the saved password.
func (s *Server) savedMail(ctx context.Context, ref string) (CampaignSummary, MailConfig, error) {
	c, err := s.findCampaign(ctx, ref)
	if err != nil {
		return CampaignSummary{}, MailConfig{}, err
	}

	cfg, _, err := s.svc.CampaignMailForForm(ctx, c.ID)

	return c, cfg, err
}

func (s *Server) mcpCheckMail(ctx context.Context, _ *mcp.CallToolRequest, in campaignRef) (*mcp.CallToolResult, message, error) {
	c, cfg, err := s.savedMail(ctx, in.Campaign)
	if err != nil {
		return nil, message{}, err
	}

	if err := s.svc.CheckCampaignMail(ctx, c.ID, &cfg); err != nil {
		return nil, message{}, fmt.Errorf("SMTP check failed: %s. Fix it with configure_campaign_email", publicErr(err))
	}

	return nil, message{Message: "SMTP connection and login work."}, nil
}

type previewOutput struct {
	Subject  string `json:"subject"`
	Body     string `json:"body"`
	HTML     bool   `json:"html"`
	NextStep string `json:"next_step"`
}

func (s *Server) mcpPreviewMail(ctx context.Context, _ *mcp.CallToolRequest, in campaignRef) (*mcp.CallToolResult, previewOutput, error) {
	c, cfg, err := s.savedMail(ctx, in.Campaign)
	if err != nil {
		return nil, previewOutput{}, err
	}

	subject, body, err := s.svc.PreviewCampaignMail(ctx, c.ID, &cfg)
	if err != nil {
		return nil, previewOutput{}, fmt.Errorf("%s. Fix it with configure_campaign_email", publicErr(err))
	}

	return nil, previewOutput{
		Subject: subject, Body: body, HTML: cfg.Format == MailFormatHTML,
		NextStep: "Show this preview to the user. To check delivery, call send_test_email.",
	}, nil
}

type testMailInput struct {
	Campaign string `json:"campaign" jsonschema:"campaign name or id"`
	To       string `json:"to" jsonschema:"email address that receives the test"`
}

func (s *Server) mcpSendTestMail(ctx context.Context, _ *mcp.CallToolRequest, in testMailInput) (*mcp.CallToolResult, message, error) {
	c, cfg, err := s.savedMail(ctx, in.Campaign)
	if err != nil {
		return nil, message{}, err
	}

	if err := s.svc.SendTestMail(ctx, c.ID, &cfg, in.To, s.sendMail); err != nil {
		return nil, message{}, fmt.Errorf("test email not sent: %s", publicErr(err))
	}

	return nil, message{Message: "Test email sent to " + in.To + "."}, nil
}

type dispatchInput struct {
	Campaign string `json:"campaign" jsonschema:"campaign name or id"`
	Confirm  bool   `json:"confirm,omitempty" jsonschema:"true to really send; only after the user agreed"`
	Resend   bool   `json:"resend,omitempty" jsonschema:"also send to addresses that already got this campaign"`
}

type dispatchOutput struct {
	Sent        bool   `json:"started"`
	ReadyToSend int    `json:"addresses_ready_to_send"`
	Detail      string `json:"detail"`
	NextStep    string `json:"next_step"`
}

func (s *Server) mcpStartDispatch(ctx context.Context, _ *mcp.CallToolRequest, in dispatchInput) (*mcp.CallToolResult, dispatchOutput, error) {
	c, err := s.findCampaign(ctx, in.Campaign)
	if err != nil {
		return nil, dispatchOutput{}, err
	}

	d, err := s.campaignDetail(ctx, c.ID)
	if err != nil {
		return nil, dispatchOutput{}, err
	}

	if missing := missingMailFields(&d.Email); missing != "" {
		return nil, dispatchOutput{}, fmt.Errorf("email is not set up, missing: %s. Call configure_campaign_email first", missing)
	}

	ready := d.ReadyToSend
	if in.Resend {
		ready += d.AlreadySent
	}

	if !in.Confirm {
		return nil, dispatchOutput{
			ReadyToSend: ready,
			Detail:      fmt.Sprintf("Nothing was sent. %d addresses would get the email %q from %s.", ready, d.Email.Subject, d.Email.FromEmail),
			NextStep:    "Ask the user to confirm. Only if they agree, call start_campaign_dispatch again with confirm=true.",
		}, nil
	}

	res, err := s.svc.StartDispatch(ctx, c.ID, in.Resend, s.sendMail)
	if err != nil {
		return nil, dispatchOutput{}, errors.New(publicErr(err))
	}

	out := dispatchOutput{Sent: res.Started, ReadyToSend: ready, Detail: res.Detail, NextStep: "Nothing to send."}
	if res.Started {
		out.NextStep = fmt.Sprintf("Sending in the background, in batches with pauses. Call get_campaign with campaign=%q to follow it.", c.Name)
	}

	return nil, out, nil
}
