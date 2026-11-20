package web

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"uuid"
)

const (
	tmplCampaigns = "static/templates/campaigns.html"
	tmplCampaign  = "static/templates/campaign.html"
)

type campaignsView struct {
	Campaigns []CampaignSummary
	Services  []MailService
	Notice    string
	Error     string
}

// waitingSentence is the "Waiting 8s before the next message." part of a
// dispatch detail, replaced on the page by a live countdown.
var waitingSentence = regexp.MustCompile(`\s*Waiting \S+ before the next (message|batch)\.`)

type campaignView struct {
	Campaign      Campaign
	Services      []MailService
	Mail          MailConfig
	PasswordSet   bool
	Jobs          []Job
	Available     []Job
	Dispatches    []Dispatch
	Preview       DispatchPreview
	Notice        string
	Error         string
	Running       bool
	Paused        bool
	RunningDetail string
	// NextSendMS is how long until the next email, for the live countdown.
	NextSendMS     int64
	NextIsBatch    bool
	PreviewWarning string
	Auto           AutoDispatch
	Stats          CampaignStats

	// One page of each long list, 10 rows each, and their pagers.
	PagedJobs       []Job
	JobsPager       Pager
	JobsOrder       string
	JobsStatus      string
	FailedJobs      int
	PagedDispatches []Dispatch
	HistoryPager    Pager
	PagedMailLog    []MailLogEntry
	MailPager       Pager
	// PageQuery keeps the pages and order above in the live refresh.
	PageQuery string
}

// Query parameters of the paged lists on the campaign page.
const (
	jobsPageParam   = "jobs_page"
	jobsOrderParam  = "jobs_sort"
	jobsStatusParam = "jobs_status"
	histPageParam   = "hist_page"
	mailPageParam   = "mail_page"
)

// pageCampaignView fills the paged lists of the campaign page from the request.
func pageCampaignView(view *campaignView, r *http.Request) {
	query := url.Values{}

	for _, k := range []string{jobsPageParam, jobsOrderParam, jobsStatusParam, histPageParam, mailPageParam} {
		if v := strings.TrimSpace(r.URL.Query().Get(k)); v != "" {
			query.Set(k, v)
		}
	}

	if !ValidJobOrder(query.Get(jobsOrderParam)) {
		query.Del(jobsOrderParam)
	}

	if !ValidJobStatusFilter(query.Get(jobsStatusParam)) {
		query.Del(jobsStatusParam)
	}

	view.JobsOrder = query.Get(jobsOrderParam)
	view.JobsStatus = query.Get(jobsStatusParam)

	jobs := make([]Job, 0, len(view.Jobs))

	for i := range view.Jobs {
		if view.Jobs[i].Status == StatusFailed {
			view.FailedJobs++
		}

		if matchesStatus(view.JobsStatus, view.Jobs[i].Status) {
			jobs = append(jobs, view.Jobs[i])
		}
	}

	sortJobsBy(jobs, view.JobsOrder)

	var start, end int

	view.JobsPager, start, end = newPager(query, jobsPageParam, len(jobs), ListPageSize, "jobs")
	view.PagedJobs = jobs[start:end]

	view.HistoryPager, start, end = newPager(query, histPageParam, len(view.Dispatches), ListPageSize, "history")
	view.PagedDispatches = view.Dispatches[start:end]

	view.MailPager, start, end = newPager(query, mailPageParam, len(view.Stats.MailLog), ListPageSize, "history")
	view.PagedMailLog = view.Stats.MailLog[start:end]

	view.PageQuery = pageQuery(query, jobsPageParam, jobsOrderParam, jobsStatusParam, histPageParam, mailPageParam)
}

func (s *Server) campaigns(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.renderCampaigns(w, r)
	case http.MethodPost:
		s.createCampaign(w, r)
	default:
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)
	}
}

func (s *Server) renderCampaigns(w http.ResponseWriter, r *http.Request) {
	items, err := s.svc.ListCampaigns(r.Context())
	if err != nil {
		http.Error(w, publicErr(err), http.StatusInternalServerError)

		return
	}

	services, err := s.svc.ListServices(r.Context())
	if err != nil {
		http.Error(w, publicErr(err), http.StatusInternalServerError)

		return
	}

	s.renderTemplate(w, r, tmplCampaigns, campaignsView{
		Campaigns: items,
		Services:  services,
		Notice:    queryMessage(r, "notice"),
		Error:     queryMessage(r, "error"),
	})
}

func (s *Server) createCampaign(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	campaign := Campaign{
		ID:          uuid.NewV4().String(),
		Name:        r.Form.Get("name"),
		ServiceName: r.Form.Get("servico"),
	}
	if err := s.svc.CreateCampaign(r.Context(), &campaign); err != nil {
		redirectQuery(w, r, "/campaigns", "error", publicErr(err))

		return
	}

	// "Take the jobs of these campaigns": merge them into the new one.
	moved, err := s.svc.MergeCampaignJobs(r.Context(), campaign.ID, r.Form["merge_from"])
	if err != nil {
		redirectQuery(w, r, "/campaigns/"+campaign.ID, "error", publicErr(err))

		return
	}

	if moved > 0 {
		redirectQuery(w, r, "/campaigns/"+campaign.ID, "notice", "Campaign created with the jobs of the chosen campaigns.")

		return
	}

	http.Redirect(w, r, "/campaigns/"+campaign.ID, http.StatusSeeOther)
}

func (s *Server) campaign(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	id, err := campaignIDFromRequest(r)
	if err != nil {
		http.Error(w, "invalid campaign", http.StatusUnprocessableEntity)

		return
	}

	view, err := s.loadCampaignView(r, id)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)

		return
	}

	if err != nil {
		http.Error(w, publicErr(err), http.StatusInternalServerError)

		return
	}

	s.renderTemplate(w, r, tmplCampaign, &view)
}

func (s *Server) loadCampaignView(r *http.Request, id string) (campaignView, error) {
	var view campaignView

	campaign, err := s.svc.GetCampaign(r.Context(), id)
	if err != nil {
		return view, err
	}

	services, err := s.svc.ListServices(r.Context())
	if err != nil {
		return view, err
	}

	mail, passwordSet, err := s.svc.CampaignMailForForm(r.Context(), id)
	if err != nil {
		return view, err
	}

	jobs, err := s.svc.CampaignJobList(r.Context(), id)
	if err != nil {
		return view, err
	}

	all, err := s.svc.All(r.Context())
	if err != nil {
		return view, err
	}

	dispatches, err := s.svc.ListDispatchHistory(r.Context(), id)
	if err != nil {
		return view, err
	}

	running, err := s.svc.DispatchRunning(r.Context(), id)
	if err != nil {
		return view, err
	}

	preview, previewErr := s.svc.DispatchPreview(r.Context(), id)
	warning := ""

	if previewErr != nil {
		warning = publicErr(previewErr)
	}

	member := make(map[string]struct{}, len(jobs))
	for i := range jobs {
		member[jobs[i].ID] = struct{}{}
	}

	available := make([]Job, 0, len(all))

	for i := range all {
		if _, ok := member[all[i].ID]; ok {
			continue
		}

		available = append(available, all[i])
	}

	stats, err := s.svc.CampaignStats(r.Context(), id)
	if err != nil {
		return view, err
	}

	runningDetail := ""
	if running && len(dispatches) > 0 {
		runningDetail = dispatches[0].Detail
	}

	// While the run waits, the page counts down to the next email itself, so
	// the fixed "Waiting 8s" sentence is dropped from the detail.
	var (
		nextSendMS  int64
		nextIsBatch bool
	)

	if next := s.svc.NextDispatchSend(id); running && !next.IsZero() {
		nextSendMS = max(time.Until(next).Milliseconds(), 0)
		nextIsBatch = strings.Contains(runningDetail, "before the next batch")
		runningDetail = waitingSentence.ReplaceAllString(runningDetail, "")
	}

	view = campaignView{
		Campaign:       campaign,
		Services:       services,
		Mail:           mail,
		PasswordSet:    passwordSet,
		Jobs:           jobs,
		Available:      available,
		Dispatches:     dispatches,
		Preview:        preview,
		RunningDetail:  runningDetail,
		NextSendMS:     nextSendMS,
		NextIsBatch:    nextIsBatch,
		Notice:         queryMessage(r, "notice"),
		Error:          queryMessage(r, "error"),
		Running:        running,
		Paused:         running && s.svc.DispatchPaused(id),
		PreviewWarning: warning,
		Stats:          stats,
	}

	if view.Auto, err = s.svc.GetAutoDispatch(r.Context(), id); err != nil {
		log.Printf("auto dispatch setting %s: %v", id, err)
	}

	pageCampaignView(&view, r)

	return view, nil
}

// campaignAuto saves the automatic sending setting. POST /campaigns/{id}/auto
func (s *Server) campaignAuto(w http.ResponseWriter, r *http.Request) {
	s.postCampaign(w, r, func(id string) error {
		if err := r.ParseForm(); err != nil {
			return err
		}

		days := DefaultResendAfter
		a := AutoDispatch{
			Enabled:         r.Form.Get("auto_enabled") == formChecked,
			Resend:          r.Form.Get("auto_resend") == formChecked,
			ResendAfterDays: int(days / (24 * time.Hour)),
		}

		if raw := strings.TrimSpace(r.Form.Get("auto_resend_after_days")); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				return errors.New("resend interval must be a whole number of days, 0 or more")
			}

			a.ResendAfterDays = n
		}

		if err := s.svc.SaveAutoDispatch(r.Context(), id, a); err != nil {
			return err
		}

		if a.Enabled {
			return errNotice("Automatic sending is on. New addresses are sent within a minute.")
		}

		return errNotice("Automatic sending is off.")
	}, "")
}

func (s *Server) campaignRename(w http.ResponseWriter, r *http.Request) {
	s.postCampaign(w, r, func(id string) error {
		if err := r.ParseForm(); err != nil {
			return err
		}

		if err := s.svc.RenameCampaign(r.Context(), id, r.Form.Get("name")); err != nil {
			return err
		}

		if service := r.Form.Get("servico"); service != "" {
			return s.svc.SetCampaignService(r.Context(), id, service)
		}

		return nil
	}, "Campaign saved")
}

func (s *Server) campaignDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	id, err := campaignIDFromRequest(r)
	if err != nil {
		http.Error(w, "invalid campaign", http.StatusUnprocessableEntity)

		return
	}

	if r.FormValue("delete_jobs") == "1" {
		if _, err := s.svc.DeleteCampaignWithJobs(r.Context(), id); err != nil {
			redirectQuery(w, r, "/campaigns", "error", publicErr(err))

			return
		}

		redirectQuery(w, r, "/campaigns", "notice", "Campaign and its jobs deleted.")

		return
	}

	if err := s.svc.DeleteCampaign(r.Context(), id); err != nil {
		redirectQuery(w, r, "/campaigns", "error", publicErr(err))

		return
	}

	redirectQuery(w, r, "/campaigns", "notice", "Campaign deleted. Jobs were kept.")
}

func (s *Server) campaignAssign(w http.ResponseWriter, r *http.Request) {
	s.postCampaign(w, r, func(id string) error {
		if err := r.ParseForm(); err != nil {
			return err
		}

		jobID, err := optionalCampaignID(r.Form.Get("job_id"))
		if err != nil || jobID == "" {
			return errors.New("invalid job")
		}

		return s.svc.AssignJob(r.Context(), id, jobID)
	}, "Job added to the campaign")
}

func (s *Server) campaignUnassign(w http.ResponseWriter, r *http.Request) {
	s.postCampaign(w, r, func(id string) error {
		if err := r.ParseForm(); err != nil {
			return err
		}

		jobID, err := optionalCampaignID(r.Form.Get("job_id"))
		if err != nil || jobID == "" {
			return errors.New("invalid job")
		}

		return s.svc.UnassignJob(r.Context(), id, jobID)
	}, "Job removed from the campaign")
}

// campaignJobRetry queues a failed job of the campaign again. POST /campaigns/{id}/jobs/retry
func (s *Server) campaignJobRetry(w http.ResponseWriter, r *http.Request) {
	s.postCampaign(w, r, func(_ string) error {
		if err := r.ParseForm(); err != nil {
			return err
		}

		jobID, err := optionalCampaignID(r.Form.Get("job_id"))
		if err != nil || jobID == "" {
			return errors.New("invalid job")
		}

		return s.svc.RetryJob(r.Context(), jobID)
	}, "Job queued again")
}

func (s *Server) campaignMail(w http.ResponseWriter, r *http.Request) {
	s.postCampaign(w, r, func(id string) error {
		cfg, err := mailConfigFromForm(r)
		if err != nil {
			return err
		}

		return s.svc.SaveCampaignMail(r.Context(), id, &cfg)
	}, "Email settings saved")
}

func (s *Server) campaignMailTest(w http.ResponseWriter, r *http.Request) {
	if wantsMailJSON(r) {
		s.campaignMailJSON(w, r, func(id string, cfg MailConfig) mailJSON {
			if err := s.svc.SendTestMail(r.Context(), id, &cfg, r.Form.Get("test_to"), s.sendMail); err != nil {
				return mailJSON{OK: false, Error: trFrom(r, publicErr(err))}
			}

			return mailJSON{OK: true, Notice: trFrom(r, "Test email sent. Settings were not saved.")}
		})

		return
	}

	s.postCampaign(w, r, func(id string) error {
		cfg, err := mailConfigFromForm(r)
		if err != nil {
			return err
		}

		return s.svc.SendTestMail(r.Context(), id, &cfg, r.Form.Get("test_to"), s.sendMail)
	}, "Test email sent. Settings were not saved.")
}

func (s *Server) campaignMailCheck(w http.ResponseWriter, r *http.Request) {
	s.campaignMailJSON(w, r, func(id string, cfg MailConfig) mailJSON {
		if err := s.svc.CheckCampaignMail(r.Context(), id, &cfg); err != nil {
			return mailJSON{OK: false, Error: trFrom(r, publicErr(err))}
		}

		return mailJSON{OK: true, Notice: trFrom(r, "SMTP connection succeeded.")}
	})
}

func (s *Server) campaignMailPreview(w http.ResponseWriter, r *http.Request) {
	s.campaignMailJSON(w, r, func(id string, cfg MailConfig) mailJSON {
		subject, body, err := s.svc.PreviewCampaignMail(r.Context(), id, &cfg)
		if err != nil {
			return mailJSON{OK: false, Error: trFrom(r, publicErr(err))}
		}

		return mailJSON{OK: true, Subject: subject, Body: body, HTML: cfg.Format == MailFormatHTML}
	})
}

// mailJSON is the response for preview, connection test, and JSON send-test.
type mailJSON struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Notice  string `json:"notice,omitempty"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body,omitempty"`
	HTML    bool   `json:"html,omitempty"`
}

func (s *Server) campaignMailJSON(w http.ResponseWriter, r *http.Request, fn func(id string, cfg MailConfig) mailJSON) {
	if r.Method != http.MethodPost {
		writeMailJSON(w, http.StatusMethodNotAllowed, &mailJSON{Error: methodNotAllowedMessage})

		return
	}

	id, err := campaignIDFromRequest(r)
	if err != nil {
		writeMailJSON(w, http.StatusUnprocessableEntity, &mailJSON{Error: "invalid campaign"})

		return
	}

	cfg, err := mailConfigFromForm(r)
	if err != nil {
		writeMailJSON(w, http.StatusUnprocessableEntity, &mailJSON{Error: trFrom(r, publicErr(err))})

		return
	}

	payload := fn(id, cfg)
	writeMailJSON(w, http.StatusOK, &payload)
}

func writeMailJSON(w http.ResponseWriter, status int, payload *mailJSON) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(payload)
}

func wantsMailJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func (s *Server) campaignDispatch(w http.ResponseWriter, r *http.Request) {
	s.postCampaign(w, r, func(id string) error {
		if err := r.ParseForm(); err != nil {
			return err
		}

		resendAfter := DefaultResendAfter
		if raw := strings.TrimSpace(r.Form.Get("resend_after_days")); raw != "" {
			days, err := strconv.Atoi(raw)
			if err != nil || days < 0 {
				return errors.New("resend interval must be a whole number of days, 0 or more")
			}

			resendAfter = time.Duration(days) * 24 * time.Hour
		}

		result, err := s.svc.StartDispatch(r.Context(), id, r.Form.Get("resend") == formChecked, resendAfter, s.sendMail)
		if err != nil {
			return err
		}

		if result.Started {
			return errNotice("Dispatch started. This page refreshes while it runs.")
		}

		return errNotice(result.Detail)
	}, "")
}

// campaignDispatchFailed sends again only to the addresses whose last send failed.
func (s *Server) campaignDispatchFailed(w http.ResponseWriter, r *http.Request) {
	s.postCampaign(w, r, func(id string) error {
		result, err := s.svc.RetryFailedDispatch(r.Context(), id, s.sendMail)
		if err != nil {
			return err
		}

		if result.Started {
			return errNotice("Failed addresses queued again. This page refreshes while they are sent.")
		}

		return errNotice(result.Detail)
	}, "")
}

// campaignRequeueFailed puts the failed addresses back among those waiting to be sent.
func (s *Server) campaignRequeueFailed(w http.ResponseWriter, r *http.Request) {
	s.postCampaign(w, r, func(id string) error {
		n, err := s.svc.RequeueFailed(r.Context(), id)
		if err != nil {
			return err
		}

		if n == 0 {
			return errNotice("No failed addresses to send again.")
		}

		return errNotice(trFrom(r, "%d failed addresses are waiting to be sent again.", n))
	}, "")
}

// campaignDispatchPause pauses the running dispatch before its next email.
func (s *Server) campaignDispatchPause(w http.ResponseWriter, r *http.Request) {
	s.postCampaign(w, r, s.svc.PauseDispatch, "Sending paused. You can change the email settings and resume.")
}

// campaignDispatchResume continues a paused dispatch with the saved settings.
func (s *Server) campaignDispatchResume(w http.ResponseWriter, r *http.Request) {
	s.postCampaign(w, r, s.svc.ResumeDispatch, "Sending resumed.")
}

// errNotice carries a success message out of postCampaign.
type errNotice string

func (e errNotice) Error() string {
	return string(e)
}

func (s *Server) postCampaign(w http.ResponseWriter, r *http.Request, fn func(id string) error, notice string) {
	if r.Method != http.MethodPost {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	id, err := campaignIDFromRequest(r)
	if err != nil {
		http.Error(w, "invalid campaign", http.StatusUnprocessableEntity)

		return
	}

	if err := fn(id); err != nil {
		var note errNotice
		if errors.As(err, &note) {
			redirectQuery(w, r, "/campaigns/"+id, "notice", note.Error())

			return
		}

		redirectQuery(w, r, "/campaigns/"+id, "error", publicErr(err))

		return
	}

	if notice == "" {
		http.Redirect(w, r, "/campaigns/"+id, http.StatusSeeOther) //nolint:gosec // G710: id is a parsed UUID. The path is only /campaigns/{id}.

		return
	}

	redirectQuery(w, r, "/campaigns/"+id, "notice", notice)
}

func mailConfigFromForm(r *http.Request) (MailConfig, error) {
	// The preview and test buttons send FormData, which is multipart.
	// ParseForm only reads urlencoded bodies, so those fields would arrive empty.
	const maxForm = maxMailText + 64<<10

	r.Body = http.MaxBytesReader(nil, r.Body, maxForm)

	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(maxForm); err != nil { //nolint:gosec // G120: MaxBytesReader above caps the body, and maxForm caps the parsed form.
			return MailConfig{}, err
		}
	} else if err := r.ParseForm(); err != nil {
		return MailConfig{}, err
	}

	port := defaultSMTPPort

	if raw := strings.TrimSpace(r.Form.Get("smtp_port")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return MailConfig{}, errors.New("invalid SMTP port")
		}

		port = n
	}

	cfg := MailConfig{
		Host:       r.Form.Get("smtp_host"),
		Port:       port,
		Username:   r.Form.Get("smtp_username"),
		Password:   r.Form.Get("smtp_password"),
		FromEmail:  r.Form.Get("from_email"),
		FromName:   r.Form.Get("from_name"),
		Subject:    r.Form.Get("subject"),
		Body:       r.Form.Get("body"),
		Encryption: MailEncryption(r.Form.Get("encryption")),
		Format:     MailFormat(r.Form.Get("format")),
	}
	cfg = cfg.Normalize()

	if err := cfg.ValidateShape(); err != nil {
		return MailConfig{}, err
	}

	return cfg, nil
}

func optionalCampaignID(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}

	if _, err := uuid.Parse(raw); err != nil {
		return "", errors.New("invalid campaign")
	}

	return raw, nil
}

func campaignIDFromRequest(r *http.Request) (string, error) {
	id, err := optionalCampaignID(r.PathValue("id"))
	if err != nil || id == "" {
		return "", errors.New("invalid campaign")
	}

	return id, nil
}

func publicErr(err error) string {
	switch {
	case errors.Is(err, ErrNotFound):
		return "Campaign not found"
	case errors.Is(err, ErrJobNotFound):
		return "Job not found"
	case errors.Is(err, ErrDispatchRunning):
		return "A dispatch is already running for this campaign"
	case errors.Is(err, ErrNoDispatch):
		return "No dispatch is running for this campaign"
	case errors.Is(err, ErrInvalidService):
		return "The campaign service is missing or not registered"
	case errors.Is(err, ErrServiceInUse):
		return "This service is used by a campaign. Move those campaigns to another service first"
	case errors.Is(err, ErrServiceExists):
		return "This service already exists"
	case errors.Is(err, ErrOptOutUnavailable):
		return "Could not check the unsubscribe list. Nothing was sent. Try again later"
	case errors.As(err, new(ErrUnsubscribed)):
		return "This address unsubscribed from the campaign service. Nothing was sent"
	case errors.Is(err, ErrMailNotReady):
		return "Fill in the SMTP host, from address, password, subject, and body before sending"
	default:
		return err.Error()
	}
}

func redirectQuery(w http.ResponseWriter, r *http.Request, path, key, msg string) {
	if len(msg) > 300 {
		msg = msg[:300]
	}

	target := path
	if msg != "" {
		target += "?" + key + "=" + url.QueryEscape(msg)
	}

	http.Redirect(w, r, target, http.StatusSeeOther) //nolint:gosec // G710: path is /campaigns/{uuid}. The query value is escaped.
}

func queryMessage(r *http.Request, key string) string {
	msg := r.URL.Query().Get(key)
	if len(msg) > 300 {
		return ""
	}

	return msg
}

func (s *Server) renderTemplate(w http.ResponseWriter, r *http.Request, name string, data any) {
	var buf bytes.Buffer
	if err := s.execTemplate(&buf, r, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "could not render the page", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if _, err := buf.WriteTo(w); err != nil {
		log.Printf("write %s: %v", name, err)
	}
}

// campaignLeads downloads the campaign's businesses as one CSV, merged across
// its jobs. GET /campaigns/{id}/leads.csv
func (s *Server) campaignLeads(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	id, err := campaignIDFromRequest(r)
	if err != nil {
		http.Error(w, "invalid campaign", http.StatusUnprocessableEntity)

		return
	}

	campaign, err := s.svc.GetCampaign(r.Context(), id)
	if err != nil {
		http.Error(w, publicErr(err), http.StatusNotFound)

		return
	}

	leads, err := s.svc.CampaignLeads(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+leadsFileName(campaign.Name)+`"`)

	// A byte order mark makes Excel read the accents as UTF-8.
	_, _ = w.Write([]byte("\xEF\xBB\xBF"))

	out := csv.NewWriter(w)
	_ = out.Write(LeadCSVHeader)

	for i := range leads {
		_ = out.Write(leads[i].Row())
	}

	out.Flush()
}

// leadsFileName is a safe download name such as "leads-escolas-rs.csv".
func leadsFileName(name string) string {
	var b strings.Builder

	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteRune('-')
		}
	}

	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = "campanha"
	}

	return "leads-" + slug + ".csv"
}
