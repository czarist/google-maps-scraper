package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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

type campaignView struct {
	Campaign       Campaign
	Services       []MailService
	Mail           MailConfig
	PasswordSet    bool
	Jobs           []Job
	Available      []Job
	Dispatches     []Dispatch
	Preview        DispatchPreview
	Notice         string
	Error          string
	Running        bool
	RunningDetail  string
	PreviewWarning string
	Stats          CampaignStats
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

	dispatches, err := s.svc.ListDispatches(r.Context(), id)
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
		Notice:         queryMessage(r, "notice"),
		Error:          queryMessage(r, "error"),
		Running:        running,
		PreviewWarning: warning,
		Stats:          stats,
	}

	return view, nil
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

		result, err := s.svc.StartDispatch(r.Context(), id, r.Form.Get("resend") == formChecked, s.sendMail)
		if err != nil {
			return err
		}

		if result.Started {
			return errNotice("Dispatch started. This page refreshes while it runs.")
		}

		return errNotice(result.Detail)
	}, "")
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
