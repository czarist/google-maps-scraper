package web

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
	"uuid"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	mcpServerName      = "google-maps-scraper"
	mcpServerVersion   = "1.0.0"
	mcpDefaultLimit    = 50
	mcpMaxLimit        = 1000
	mcpDefaultDepth    = 10
	mcpDefaultZoom     = 15
	mcpDefaultMaxTime  = 10 * time.Minute
	mcpMinMaxTime      = 3 * time.Minute
	mcpDefaultRadiusKM = 50
	metersPerKM        = 1000
)

// defaultResultColumns are the CSV columns returned unless all columns are asked for.
var defaultResultColumns = []string{
	"title", "category", "address", "phone", "website", "emails",
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

//nolint:funlen // one registration per tool keeps the tool list readable.
func (s *Server) newMCPServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: mcpServerName, Version: mcpServerVersion}, &mcp.ServerOptions{
		Instructions: "Scrape businesses from Google Maps, manage the proxy pool, group jobs into campaigns " +
			"and email the businesses found. Jobs run in the background: create_job returns at once, " +
			"then poll get_job until status is ok or failed, and read places with get_job_results.",
	})

	// Jobs
	mcp.AddTool(server, &mcp.Tool{Name: "list_jobs", Description: "List scrape jobs, newest first, with status and places found.",
		Annotations: readOnly("List jobs")}, s.mcpListJobs)
	mcp.AddTool(server, &mcp.Tool{Name: "get_job", Description: "Get one job: settings, status, live progress and campaign.",
		Annotations: readOnly("Get job")}, s.mcpGetJob)
	mcp.AddTool(server, &mcp.Tool{Name: "create_job", Description: "Create a Google Maps scrape job. It is queued and runs in the background. " +
		"Each query is one Google Maps search, such as 'escolas porto alegre'. Places farther than radius_km from the searched area are dropped. " +
		"With no proxies given, the saved proxy pool is used unless use_saved_proxies is false.",
		Annotations: writes("Create job", false)}, s.mcpCreateJob)
	mcp.AddTool(server, &mcp.Tool{Name: "delete_job", Description: "Delete a job and its CSV results. This cannot be undone.",
		Annotations: writes("Delete job", true)}, s.mcpDeleteJob)
	mcp.AddTool(server, &mcp.Tool{Name: "get_job_results", Description: "Read the places a job found, from its CSV. Paginate with offset and limit.",
		Annotations: readOnly("Get job results")}, s.mcpJobResults)

	// Proxies
	mcp.AddTool(server, &mcp.Tool{Name: "list_proxies", Description: "List the saved proxy pool used by new jobs.",
		Annotations: readOnly("List proxies")}, s.mcpListProxies)
	mcp.AddTool(server, &mcp.Tool{Name: "add_proxies", Description: "Add proxies to the saved pool. Supports http, https and socks5, e.g. http://user:pass@host:port.",
		Annotations: writes("Add proxies", false)}, s.mcpAddProxies)
	mcp.AddTool(server, &mcp.Tool{Name: "remove_proxies", Description: "Remove proxies from the saved pool.",
		Annotations: writes("Remove proxies", true)}, s.mcpRemoveProxies)
	mcp.AddTool(server, &mcp.Tool{Name: "set_proxies", Description: "Replace the whole saved proxy pool. An empty list clears it.",
		Annotations: writes("Replace proxies", true)}, s.mcpSetProxies)
	mcp.AddTool(server, &mcp.Tool{Name: "test_proxies", Description: "Request Google Maps through each proxy and report status and latency. " +
		"Tests the saved pool when no proxies are given. remove_failed drops failing ones from the pool.",
		Annotations: external("Test proxies")}, s.mcpTestProxies)

	// Campaigns
	mcp.AddTool(server, &mcp.Tool{Name: "list_campaigns", Description: "List campaigns with job count, email readiness and last dispatch status.",
		Annotations: readOnly("List campaigns")}, s.mcpListCampaigns)
	mcp.AddTool(server, &mcp.Tool{Name: "get_campaign", Description: "Get a campaign: jobs, email settings (password hidden), recipients ready and dispatch history.",
		Annotations: readOnly("Get campaign")}, s.mcpGetCampaign)
	mcp.AddTool(server, &mcp.Tool{Name: "create_campaign", Description: "Create a campaign. Campaigns group jobs and hold their own email settings.",
		Annotations: writes("Create campaign", false)}, s.mcpCreateCampaign)
	mcp.AddTool(server, &mcp.Tool{Name: "rename_campaign", Description: "Rename a campaign.",
		Annotations: writes("Rename campaign", false)}, s.mcpRenameCampaign)
	mcp.AddTool(server, &mcp.Tool{Name: "delete_campaign", Description: "Delete a campaign, its email settings and send history. Jobs and CSVs are kept.",
		Annotations: writes("Delete campaign", true)}, s.mcpDeleteCampaign)
	mcp.AddTool(server, &mcp.Tool{Name: "add_job_to_campaign", Description: "Put a job in a campaign. A job belongs to one campaign, so it leaves its previous one.",
		Annotations: writes("Add job to campaign", false)}, s.mcpAssignJob)
	mcp.AddTool(server, &mcp.Tool{Name: "remove_job_from_campaign", Description: "Take a job out of a campaign. The job is kept.",
		Annotations: writes("Remove job from campaign", false)}, s.mcpUnassignJob)

	// Campaign email
	mcp.AddTool(server, &mcp.Tool{Name: "configure_campaign_email", Description: "Save SMTP and message settings for a campaign. " +
		"Only the fields given change. Subject and body are Go templates with {{.Title}} {{.Email}} {{.Phone}} {{.Website}} {{.Address}} {{.Category}}.",
		Annotations: writes("Configure campaign email", false)}, s.mcpConfigureMail)
	mcp.AddTool(server, &mcp.Tool{Name: "test_campaign_smtp", Description: "Connect and log in to the campaign's SMTP server without sending anything.",
		Annotations: external("Test SMTP connection")}, s.mcpCheckMail)
	mcp.AddTool(server, &mcp.Tool{Name: "preview_campaign_email", Description: "Render the campaign's subject and body with sample business data.",
		Annotations: readOnly("Preview campaign email")}, s.mcpPreviewMail)
	mcp.AddTool(server, &mcp.Tool{Name: "send_test_email", Description: "Send one test message with the campaign's settings to the given address.",
		Annotations: external("Send test email")}, s.mcpSendTestMail)
	mcp.AddTool(server, &mcp.Tool{Name: "start_campaign_dispatch", Description: "Email every business address collected by the campaign's jobs. " +
		"This sends real email to real businesses unless the SMTP server is a test inbox. Confirm with the user first. " +
		"Addresses already sent are skipped unless resend is true. Runs in the background; follow it with get_campaign.",
		Annotations: &mcp.ToolAnnotations{Title: "Start campaign dispatch", DestructiveHint: ptr(true), OpenWorldHint: ptr(true)}}, s.mcpStartDispatch)

	return server
}

// ---------- shared types

type idInput struct {
	ID string `json:"id" jsonschema:"the job or campaign ID"`
}

type message struct {
	Message string `json:"message"`
}

type jobSummary struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	CreatedAt    string `json:"created_at"`
	Places       int    `json:"places"`
	Queries      int    `json:"queries"`
	CampaignID   string `json:"campaign_id,omitempty"`
	CampaignName string `json:"campaign_name,omitempty"`
}

func (s *Server) summarize(j *Job) jobSummary {
	places := j.Data.Run.Places
	if live, ok := s.svc.Progress(j.ID); ok {
		places = live
	}

	return jobSummary{
		ID:           j.ID,
		Name:         j.Name,
		Status:       j.Status,
		CreatedAt:    j.Date.UTC().Format(time.RFC3339),
		Places:       places,
		Queries:      len(j.Data.Keywords),
		CampaignID:   j.CampaignID,
		CampaignName: j.CampaignName,
	}
}

// ---------- jobs

type listJobsInput struct {
	Page int `json:"page,omitempty" jsonschema:"page number, starting at 1"`
}

type listJobsOutput struct {
	Jobs       []jobSummary `json:"jobs"`
	Page       int          `json:"page"`
	TotalPages int          `json:"total_pages"`
	Running    int          `json:"running"`
	Queued     int          `json:"queued"`
}

func (s *Server) mcpListJobs(ctx context.Context, _ *mcp.CallToolRequest, in listJobsInput) (*mcp.CallToolResult, listJobsOutput, error) {
	page := max(in.Page, 1)

	jp, err := s.svc.ListJobs(ctx, page, jobsPageLimit)
	if err != nil {
		return nil, listJobsOutput{}, err
	}

	out := listJobsOutput{Jobs: make([]jobSummary, 0, len(jp.Jobs)), Page: jp.CurrentPage, TotalPages: jp.TotalPages, Running: jp.Running, Queued: jp.Queued}
	for i := range jp.Jobs {
		out.Jobs = append(out.Jobs, s.summarize(&jp.Jobs[i]))
	}

	return nil, out, nil
}

type jobDetail struct {
	jobSummary
	Queries      []string `json:"queries"`
	Lang         string   `json:"lang"`
	Depth        int      `json:"depth"`
	MaxTime      string   `json:"max_time"`
	RadiusKM     float64  `json:"radius_km"`
	Lat          string   `json:"lat,omitempty"`
	Lon          string   `json:"lon,omitempty"`
	Zoom         int      `json:"zoom"`
	FastMode     bool     `json:"fast_mode"`
	Emails       bool     `json:"collect_emails"`
	ProxyCount   int      `json:"proxy_count"`
	StartedAt    string   `json:"started_at,omitempty"`
	FinishedAt   string   `json:"finished_at,omitempty"`
	ElapsedLabel string   `json:"elapsed,omitempty"`
}

func (s *Server) jobDetail(ctx context.Context, id string) (jobDetail, error) {
	job, err := s.svc.Get(ctx, id)
	if err != nil {
		return jobDetail{}, err
	}

	withCampaign := []Job{job}
	if err := s.svc.attachCampaigns(ctx, withCampaign); err == nil {
		job = withCampaign[0]
	}

	d := jobDetail{
		jobSummary: s.summarize(&job),
		Queries:    job.Data.Keywords,
		Lang:       job.Data.Lang,
		Depth:      job.Data.Depth,
		MaxTime:    job.Data.MaxTime.String(),
		RadiusKM:   float64(job.Data.Radius) / metersPerKM,
		Lat:        job.Data.Lat,
		Lon:        job.Data.Lon,
		Zoom:       job.Data.Zoom,
		FastMode:   job.Data.FastMode,
		Emails:     job.Data.Email,
		ProxyCount: len(job.Data.Proxies),
	}

	if job.Data.Run.Started() {
		d.StartedAt = job.Data.Run.StartedAt.UTC().Format(time.RFC3339)
		d.ElapsedLabel = job.ElapsedLabel()
	}

	if !job.Data.Run.FinishedAt.IsZero() {
		d.FinishedAt = job.Data.Run.FinishedAt.UTC().Format(time.RFC3339)
	}

	return d, nil
}

func (s *Server) mcpGetJob(ctx context.Context, _ *mcp.CallToolRequest, in idInput) (*mcp.CallToolResult, jobDetail, error) {
	d, err := s.jobDetail(ctx, in.ID)

	return nil, d, err
}

type createJobInput struct {
	Name            string   `json:"name" jsonschema:"job name"`
	Queries         []string `json:"queries" jsonschema:"Google Maps searches, one per item, e.g. 'escolas porto alegre'"`
	Lang            string   `json:"lang,omitempty" jsonschema:"two-letter results language, default pt"`
	Depth           int      `json:"depth,omitempty" jsonschema:"how many times the results list is scrolled, default 10"`
	MaxTime         string   `json:"max_time,omitempty" jsonschema:"job time limit like 10m or 1h, minimum 3m, default 10m"`
	RadiusKM        float64  `json:"radius_km,omitempty" jsonschema:"drop places farther than this from the searched area, default 50"`
	Lat             *float64 `json:"lat,omitempty" jsonschema:"search center latitude, optional (required in fast mode)"`
	Lon             *float64 `json:"lon,omitempty" jsonschema:"search center longitude, optional (required in fast mode)"`
	Zoom            int      `json:"zoom,omitempty" jsonschema:"map zoom 1-21, default 15"`
	FastMode        bool     `json:"fast_mode,omitempty" jsonschema:"beta: search only around lat/lon with fewer details"`
	CollectEmails   *bool    `json:"collect_emails,omitempty" jsonschema:"visit each business website to find emails, default true"`
	Proxies         []string `json:"proxies,omitempty" jsonschema:"proxies for this job only"`
	UseSavedProxies *bool    `json:"use_saved_proxies,omitempty" jsonschema:"use the saved pool when proxies is empty, default true"`
	CampaignID      string   `json:"campaign_id,omitempty" jsonschema:"put the job in this campaign"`
}

func (s *Server) mcpCreateJob(ctx context.Context, _ *mcp.CallToolRequest, in createJobInput) (*mcp.CallToolResult, jobDetail, error) {
	job, err := s.buildJob(&in)
	if err != nil {
		return nil, jobDetail{}, err
	}

	campaignID, err := optionalCampaignID(in.CampaignID)
	if err != nil {
		return nil, jobDetail{}, err
	}

	if campaignID != "" {
		if _, err := s.svc.GetCampaign(ctx, campaignID); err != nil {
			return nil, jobDetail{}, errors.New(publicErr(err))
		}
	}

	if err := s.svc.Create(ctx, &job); err != nil {
		return nil, jobDetail{}, err
	}

	if campaignID != "" {
		if err := s.svc.AssignJob(ctx, campaignID, job.ID); err != nil {
			return nil, jobDetail{}, errors.New(publicErr(err))
		}
	}

	d, err := s.jobDetail(ctx, job.ID)

	return nil, d, err
}

func (s *Server) buildJob(in *createJobInput) (Job, error) {
	job := Job{
		ID:     uuid.NewV4().String(),
		Name:   strings.TrimSpace(in.Name),
		Date:   time.Now().UTC(),
		Status: StatusPending,
	}

	for _, q := range in.Queries {
		if q = strings.TrimSpace(q); q != "" {
			job.Data.Keywords = append(job.Data.Keywords, q)
		}
	}

	job.Data.Lang = orDefault(strings.TrimSpace(in.Lang), "pt")
	job.Data.Depth = orDefault(in.Depth, mcpDefaultDepth)
	job.Data.Zoom = orDefault(in.Zoom, mcpDefaultZoom)
	job.Data.Radius = int(orDefault(in.RadiusKM, mcpDefaultRadiusKM) * metersPerKM)
	job.Data.FastMode = in.FastMode
	job.Data.Email = in.CollectEmails == nil || *in.CollectEmails

	job.Data.MaxTime = mcpDefaultMaxTime

	if in.MaxTime != "" {
		d, err := time.ParseDuration(in.MaxTime)
		if err != nil {
			return Job{}, fmt.Errorf("invalid max_time %q: use values like 10m or 1h", in.MaxTime)
		}

		if d < mcpMinMaxTime {
			return Job{}, errors.New("max_time must be at least 3m")
		}

		job.Data.MaxTime = d
	}

	if (in.Lat == nil) != (in.Lon == nil) {
		return Job{}, errors.New("give both lat and lon, or neither")
	}

	if in.Lat != nil {
		job.Data.Lat = fmt.Sprint(*in.Lat)
		job.Data.Lon = fmt.Sprint(*in.Lon)
	}

	proxies := in.Proxies
	if len(proxies) == 0 && (in.UseSavedProxies == nil || *in.UseSavedProxies) {
		saved, err := s.svc.SavedProxies()
		if err != nil {
			return Job{}, err
		}

		proxies = saved
	}

	clean, err := normalizeProxies(proxies)
	if err != nil {
		return Job{}, err
	}

	job.Data.Proxies = clean

	return job, job.Validate()
}

func orDefault[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}

	return v
}

func (s *Server) mcpDeleteJob(ctx context.Context, _ *mcp.CallToolRequest, in idInput) (*mcp.CallToolResult, message, error) {
	job, err := s.svc.Get(ctx, in.ID)
	if err != nil {
		return nil, message{}, err
	}

	if err := s.svc.Delete(ctx, job.ID); err != nil {
		return nil, message{}, err
	}

	return nil, message{Message: fmt.Sprintf("job %q deleted", job.Name)}, nil
}

type resultsInput struct {
	ID         string `json:"id" jsonschema:"the job ID"`
	Offset     int    `json:"offset,omitempty" jsonschema:"rows to skip"`
	Limit      int    `json:"limit,omitempty" jsonschema:"rows to return, default 50, max 1000"`
	AllColumns bool   `json:"all_columns,omitempty" jsonschema:"return every CSV column instead of the main ones"`
}

type resultsOutput struct {
	Total   int                 `json:"total"`
	Offset  int                 `json:"offset"`
	Places  []map[string]string `json:"places"`
	CSVPath string              `json:"csv_path"`
}

func (s *Server) mcpJobResults(_ context.Context, _ *mcp.CallToolRequest, in resultsInput) (*mcp.CallToolResult, resultsOutput, error) {
	path, err := s.svc.csvPath(in.ID)
	if err != nil {
		return nil, resultsOutput{}, err
	}

	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, resultsOutput{}, errors.New("no results yet: the job has not written its CSV")
	}

	if err != nil {
		return nil, resultsOutput{}, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1

	rows, err := r.ReadAll()
	if err != nil {
		return nil, resultsOutput{}, fmt.Errorf("read results: %w", err)
	}

	out := resultsOutput{Places: []map[string]string{}, CSVPath: path, Offset: max(in.Offset, 0)}
	if len(rows) == 0 {
		return nil, out, nil
	}

	header, data := rows[0], rows[1:]
	out.Total = len(data)

	want := map[string]bool{}
	for _, c := range defaultResultColumns {
		want[c] = true
	}

	limit := min(orDefault(in.Limit, mcpDefaultLimit), mcpMaxLimit)

	for i := out.Offset; i < len(data) && len(out.Places) < limit; i++ {
		place := make(map[string]string)

		for c, name := range header {
			if c < len(data[i]) && (in.AllColumns || want[name]) && data[i][c] != "" {
				place[name] = data[i][c]
			}
		}

		out.Places = append(out.Places, place)
	}

	return nil, out, nil
}

// ---------- proxies

type proxiesInput struct {
	Proxies []string `json:"proxies" jsonschema:"proxy URLs like http://user:pass@host:port or socks5://host:port"`
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

func (s *Server) mcpRemoveProxies(_ context.Context, _ *mcp.CallToolRequest, in proxiesInput) (*mcp.CallToolResult, proxiesOutput, error) {
	return pool(s.svc.RemoveSavedProxies(in.Proxies))
}

func (s *Server) mcpSetProxies(_ context.Context, _ *mcp.CallToolRequest, in proxiesInput) (*mcp.CallToolResult, proxiesOutput, error) {
	return pool(s.svc.SetSavedProxies(in.Proxies))
}

type testProxiesInput struct {
	Proxies      []string `json:"proxies,omitempty" jsonschema:"proxies to test; the saved pool when empty"`
	RemoveFailed bool     `json:"remove_failed,omitempty" jsonschema:"remove failing proxies from the saved pool"`
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
		return nil, testProxiesOutput{}, errors.New("no proxies to test: the saved pool is empty")
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

// ---------- campaigns

type campaignSummary struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	CreatedAt  string `json:"created_at"`
	Jobs       int    `json:"jobs"`
	EmailReady bool   `json:"email_ready"`
	LastStatus string `json:"last_dispatch_status,omitempty"`
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
		out.Campaigns = append(out.Campaigns, campaignSummary{
			ID: c.ID, Name: c.Name, CreatedAt: c.CreatedAt.UTC().Format(time.RFC3339),
			Jobs: c.JobCount, EmailReady: c.MailReady, LastStatus: c.LastStatus,
		})
	}

	return nil, out, nil
}

type mailView struct {
	Host        string `json:"smtp_host"`
	Port        int    `json:"smtp_port"`
	Username    string `json:"smtp_username,omitempty"`
	PasswordSet bool   `json:"password_set"`
	Encryption  string `json:"encryption"`
	FromEmail   string `json:"from_email"`
	FromName    string `json:"from_name,omitempty"`
	Subject     string `json:"subject"`
	Body        string `json:"body"`
	Format      string `json:"format"`
}

type dispatchView struct {
	Status     string `json:"status"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	Sent       int    `json:"sent"`
	Failed     int    `json:"failed"`
	Skipped    int    `json:"skipped"`
	Detail     string `json:"detail,omitempty"`
}

type campaignDetail struct {
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	Jobs             []jobSummary   `json:"jobs"`
	Email            mailView       `json:"email"`
	RecipientsReady  int            `json:"recipients_ready"`
	AlreadySent      int            `json:"already_sent"`
	JobsWithoutFile  int            `json:"jobs_without_results"`
	DispatchRunning  bool           `json:"dispatch_running"`
	Dispatches       []dispatchView `json:"dispatches"`
	RecipientWarning string         `json:"recipient_warning,omitempty"`
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
		ID: c.ID, Name: c.Name, DispatchRunning: running,
		Jobs:       make([]jobSummary, 0, len(jobs)),
		Dispatches: make([]dispatchView, 0, len(dispatches)),
		Email: mailView{
			Host: mail.Host, Port: mail.Port, Username: mail.Username, PasswordSet: passwordSet,
			Encryption: string(mail.Encryption), FromEmail: mail.FromEmail, FromName: mail.FromName,
			Subject: mail.Subject, Body: mail.Body, Format: string(mail.Format),
		},
	}

	for i := range jobs {
		d.Jobs = append(d.Jobs, s.summarize(&jobs[i]))
	}

	for i := range dispatches {
		v := dispatchView{
			Status: dispatches[i].Status, StartedAt: dispatches[i].StartedAt.UTC().Format(time.RFC3339),
			Sent: dispatches[i].Sent, Failed: dispatches[i].Failed, Skipped: dispatches[i].Skipped, Detail: dispatches[i].Detail,
		}
		if !dispatches[i].FinishedAt.IsZero() {
			v.FinishedAt = dispatches[i].FinishedAt.UTC().Format(time.RFC3339)
		}

		d.Dispatches = append(d.Dispatches, v)
	}

	if preview, err := s.svc.DispatchPreview(ctx, id); err != nil {
		d.RecipientWarning = publicErr(err)
	} else {
		d.RecipientsReady, d.AlreadySent, d.JobsWithoutFile = preview.Ready, preview.AlreadySent, preview.MissingResults
	}

	return d, nil
}

func (s *Server) mcpGetCampaign(ctx context.Context, _ *mcp.CallToolRequest, in idInput) (*mcp.CallToolResult, campaignDetail, error) {
	d, err := s.campaignDetail(ctx, in.ID)

	return nil, d, err
}

type nameInput struct {
	Name string `json:"name" jsonschema:"campaign name"`
}

func (s *Server) mcpCreateCampaign(ctx context.Context, _ *mcp.CallToolRequest, in nameInput) (*mcp.CallToolResult, campaignDetail, error) {
	c := Campaign{ID: uuid.NewV4().String(), Name: in.Name}
	if err := s.svc.CreateCampaign(ctx, &c); err != nil {
		return nil, campaignDetail{}, errors.New(publicErr(err))
	}

	d, err := s.campaignDetail(ctx, c.ID)

	return nil, d, err
}

type renameInput struct {
	ID   string `json:"id" jsonschema:"the campaign ID"`
	Name string `json:"name" jsonschema:"new campaign name"`
}

func (s *Server) mcpRenameCampaign(ctx context.Context, _ *mcp.CallToolRequest, in renameInput) (*mcp.CallToolResult, message, error) {
	if err := s.svc.RenameCampaign(ctx, in.ID, in.Name); err != nil {
		return nil, message{}, errors.New(publicErr(err))
	}

	return nil, message{Message: "campaign renamed"}, nil
}

func (s *Server) mcpDeleteCampaign(ctx context.Context, _ *mcp.CallToolRequest, in idInput) (*mcp.CallToolResult, message, error) {
	if err := s.svc.DeleteCampaign(ctx, in.ID); err != nil {
		return nil, message{}, errors.New(publicErr(err))
	}

	return nil, message{Message: "campaign deleted; its jobs were kept"}, nil
}

type assignInput struct {
	CampaignID string `json:"campaign_id" jsonschema:"the campaign ID"`
	JobID      string `json:"job_id" jsonschema:"the job ID"`
}

func (s *Server) mcpAssignJob(ctx context.Context, _ *mcp.CallToolRequest, in assignInput) (*mcp.CallToolResult, message, error) {
	if err := s.svc.AssignJob(ctx, in.CampaignID, in.JobID); err != nil {
		return nil, message{}, errors.New(publicErr(err))
	}

	return nil, message{Message: "job added to the campaign"}, nil
}

func (s *Server) mcpUnassignJob(ctx context.Context, _ *mcp.CallToolRequest, in assignInput) (*mcp.CallToolResult, message, error) {
	if err := s.svc.UnassignJob(ctx, in.CampaignID, in.JobID); err != nil {
		return nil, message{}, errors.New(publicErr(err))
	}

	return nil, message{Message: "job removed from the campaign"}, nil
}

// ---------- campaign email

type mailInput struct {
	ID         string  `json:"id" jsonschema:"the campaign ID"`
	Host       *string `json:"smtp_host,omitempty" jsonschema:"SMTP host, e.g. smtp.gmail.com or mailpit"`
	Port       *int    `json:"smtp_port,omitempty" jsonschema:"SMTP port, e.g. 587, 465 or 1025"`
	Username   *string `json:"smtp_username,omitempty" jsonschema:"SMTP login; empty for no login"`
	Password   *string `json:"smtp_password,omitempty" jsonschema:"SMTP password; omit to keep the saved one"`
	Encryption *string `json:"encryption,omitempty" jsonschema:"starttls, ssl or none"`
	FromEmail  *string `json:"from_email,omitempty" jsonschema:"sender address"`
	FromName   *string `json:"from_name,omitempty" jsonschema:"sender name"`
	Subject    *string `json:"subject,omitempty" jsonschema:"subject template"`
	Body       *string `json:"body,omitempty" jsonschema:"body template"`
	Format     *string `json:"format,omitempty" jsonschema:"plain or html"`
}

func set[T any](dst *T, v *T) {
	if v != nil {
		*dst = *v
	}
}

func (s *Server) mcpConfigureMail(ctx context.Context, _ *mcp.CallToolRequest, in mailInput) (*mcp.CallToolResult, mailView, error) {
	cfg, _, err := s.svc.CampaignMailForForm(ctx, in.ID)
	if err != nil {
		return nil, mailView{}, err
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
		cfg.Encryption = MailEncryption(*in.Encryption)
	}

	if in.Format != nil {
		cfg.Format = MailFormat(*in.Format)
	}

	if err := s.svc.SaveCampaignMail(ctx, in.ID, &cfg); err != nil {
		return nil, mailView{}, errors.New(publicErr(err))
	}

	d, err := s.campaignDetail(ctx, in.ID)

	return nil, d.Email, err
}

// savedMail returns the stored settings. The service fills in the saved password.
func (s *Server) savedMail(ctx context.Context, id string) (MailConfig, error) {
	if _, err := s.svc.GetCampaign(ctx, id); err != nil {
		return MailConfig{}, errors.New(publicErr(err))
	}

	cfg, _, err := s.svc.CampaignMailForForm(ctx, id)

	return cfg, err
}

func (s *Server) mcpCheckMail(ctx context.Context, _ *mcp.CallToolRequest, in idInput) (*mcp.CallToolResult, message, error) {
	cfg, err := s.savedMail(ctx, in.ID)
	if err != nil {
		return nil, message{}, err
	}

	if err := s.svc.CheckCampaignMail(ctx, in.ID, &cfg); err != nil {
		return nil, message{}, errors.New(publicErr(err))
	}

	return nil, message{Message: "SMTP connection succeeded"}, nil
}

type previewOutput struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
	HTML    bool   `json:"html"`
}

func (s *Server) mcpPreviewMail(ctx context.Context, _ *mcp.CallToolRequest, in idInput) (*mcp.CallToolResult, previewOutput, error) {
	cfg, err := s.savedMail(ctx, in.ID)
	if err != nil {
		return nil, previewOutput{}, err
	}

	subject, body, err := s.svc.PreviewCampaignMail(ctx, in.ID, &cfg)
	if err != nil {
		return nil, previewOutput{}, errors.New(publicErr(err))
	}

	return nil, previewOutput{Subject: subject, Body: body, HTML: cfg.Format == MailFormatHTML}, nil
}

type testMailInput struct {
	ID string `json:"id" jsonschema:"the campaign ID"`
	To string `json:"to" jsonschema:"address that receives the test"`
}

func (s *Server) mcpSendTestMail(ctx context.Context, _ *mcp.CallToolRequest, in testMailInput) (*mcp.CallToolResult, message, error) {
	cfg, err := s.savedMail(ctx, in.ID)
	if err != nil {
		return nil, message{}, err
	}

	if err := s.svc.SendTestMail(ctx, in.ID, &cfg, in.To, s.sendMail); err != nil {
		return nil, message{}, errors.New(publicErr(err))
	}

	return nil, message{Message: "test email sent to " + in.To}, nil
}

type dispatchInput struct {
	ID     string `json:"id" jsonschema:"the campaign ID"`
	Resend bool   `json:"resend,omitempty" jsonschema:"also send to addresses that already received this campaign"`
}

type dispatchOutput struct {
	Started bool   `json:"started"`
	Detail  string `json:"detail"`
}

func (s *Server) mcpStartDispatch(ctx context.Context, _ *mcp.CallToolRequest, in dispatchInput) (*mcp.CallToolResult, dispatchOutput, error) {
	res, err := s.svc.StartDispatch(ctx, in.ID, in.Resend, s.sendMail)
	if err != nil {
		return nil, dispatchOutput{}, errors.New(publicErr(err))
	}

	return nil, dispatchOutput(res), nil
}
