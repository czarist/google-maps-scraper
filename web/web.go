package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"uuid"
)

//go:embed static
var static embed.FS

const (
	jobsPageLimit           = 20
	methodNotAllowedMessage = "Method not allowed"
	formChecked             = "on"
)

type Server struct {
	tmpl     map[string]*template.Template
	srv      *http.Server
	svc      *Service
	sendMail MailSender
}

func New(svc *Service, addr string) (*Server, error) {
	ans := Server{
		svc:      svc,
		sendMail: SendSMTP,
		tmpl:     make(map[string]*template.Template),
		srv: &http.Server{
			Addr:              addr,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       60 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    1 << 20,
		},
	}

	staticFS, err := fs.Sub(static, "static")
	if err != nil {
		return nil, err
	}

	fileServer := http.FileServer(http.FS(staticFS))
	mux := http.NewServeMux()

	mux.Handle("/static/", http.StripPrefix("/static/", fileServer))
	mux.HandleFunc("/scrape", ans.scrape)
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		r = requestWithID(r)

		ans.download(w, r)
	})
	mux.HandleFunc("/delete", func(w http.ResponseWriter, r *http.Request) {
		r = requestWithID(r)

		ans.delete(w, r)
	})
	mux.HandleFunc("/jobs", ans.getJobs)
	mux.HandleFunc("/jobs/delete", ans.deleteMany)
	mux.HandleFunc("/jobs/retry", func(w http.ResponseWriter, r *http.Request) {
		r = requestWithID(r)

		ans.retryJob(w, r)
	})
	mux.HandleFunc("/jobs/campaign", ans.moveJobs)
	mux.HandleFunc("/campaigns", ans.campaigns)
	mux.HandleFunc("/campaigns/{id}", ans.campaign)
	mux.HandleFunc("/campaigns/services", ans.serviceCreate)
	mux.HandleFunc("/campaigns/services/{name}/update", ans.serviceUpdate)
	mux.HandleFunc("/campaigns/services/{name}/delete", ans.serviceDelete)
	mux.HandleFunc("/campaigns/{id}/rename", ans.campaignRename)
	mux.HandleFunc("/campaigns/{id}/delete", ans.campaignDelete)
	mux.HandleFunc("/campaigns/{id}/jobs", ans.campaignAssign)
	mux.HandleFunc("/campaigns/{id}/jobs/remove", ans.campaignUnassign)
	mux.HandleFunc("/campaigns/{id}/jobs/retry", ans.campaignJobRetry)
	mux.HandleFunc("/campaigns/{id}/mail", ans.campaignMail)
	mux.HandleFunc("/campaigns/{id}/mail/test", ans.campaignMailTest)
	mux.HandleFunc("/campaigns/{id}/mail/check", ans.campaignMailCheck)
	mux.HandleFunc("/campaigns/{id}/mail/preview", ans.campaignMailPreview)
	mux.HandleFunc("/campaigns/{id}/dispatch", ans.campaignDispatch)
	mux.HandleFunc("/campaigns/{id}/dispatch/failed", ans.campaignDispatchFailed)
	mux.HandleFunc("/campaigns/{id}/dispatch/requeue", ans.campaignRequeueFailed)
	mux.HandleFunc("/campaigns/{id}/dispatch/pause", ans.campaignDispatchPause)
	mux.HandleFunc("/campaigns/{id}/dispatch/resume", ans.campaignDispatchResume)
	mux.HandleFunc("/campaigns/{id}/auto", ans.campaignAuto)
	mux.HandleFunc("/campaigns/{id}/leads.csv", ans.campaignLeads)
	mux.HandleFunc("/view", func(w http.ResponseWriter, r *http.Request) {
		r = requestWithID(r)

		ans.viewJob(w, r)
	})
	mux.HandleFunc("/proxyscrape/status", ans.proxyScrapeStatus)
	mux.HandleFunc("/tasks", ans.index)
	mux.HandleFunc("/dashboard/stats", ans.dashboardStats)
	mux.HandleFunc("/backup", ans.backupNow)
	mux.HandleFunc("/", ans.home)
	mux.Handle("/mcp", ans.mcpHandler())

	// api routes
	mux.HandleFunc("/api/docs", ans.redocHandler)
	mux.HandleFunc("/api/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			ans.apiScrape(w, r)
		case http.MethodGet:
			ans.apiGetJobs(w, r)
		default:
			ans := apiError{
				Code:    http.StatusMethodNotAllowed,
				Message: methodNotAllowedMessage,
			}

			renderJSON(w, http.StatusMethodNotAllowed, ans)
		}
	})

	mux.HandleFunc("/api/v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		r = requestWithID(r)

		switch r.Method {
		case http.MethodGet:
			ans.apiGetJob(w, r)
		case http.MethodDelete:
			ans.apiDeleteJob(w, r)
		default:
			ans := apiError{
				Code:    http.StatusMethodNotAllowed,
				Message: methodNotAllowedMessage,
			}

			renderJSON(w, http.StatusMethodNotAllowed, ans)
		}
	})

	mux.HandleFunc("/api/v1/jobs/{id}/download", func(w http.ResponseWriter, r *http.Request) {
		r = requestWithID(r)

		if r.Method != http.MethodGet {
			ans := apiError{
				Code:    http.StatusMethodNotAllowed,
				Message: methodNotAllowedMessage,
			}

			renderJSON(w, http.StatusMethodNotAllowed, ans)

			return
		}

		ans.download(w, r)
	})

	handler := securityHeaders(withLang(pushBoostedURL(mux)))
	ans.srv.Handler = handler

	tmplsKeys := []string{
		"static/templates/index.html",
		"static/templates/job_groups.html",
		"static/templates/job_view.html",
		"static/templates/redoc.html",
		"static/templates/campaigns.html",
		"static/templates/campaign.html",
		"static/templates/dashboard.html",
		"static/templates/dashboard_cards.html",
	}

	for _, key := range tmplsKeys {
		tmp, err := template.New(path.Base(key)).Funcs(templateFuncs(LangEN)).ParseFS(static, key)
		if err != nil {
			return nil, err
		}

		ans.tmpl[key] = tmp
	}

	return &ans, nil
}

func (s *Server) Start(ctx context.Context) error {
	// Automatic sending: campaigns with it on get new addresses sent without a click.
	go s.svc.RunAutoDispatch(ctx, s.sendMail, AutoDispatchInterval)

	// Daily copy of the database (campaigns, SMTP settings, who got which email).
	go s.svc.RunBackups(ctx)

	go func(shutdownSource context.Context) {
		<-shutdownSource.Done()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(shutdownSource), 10*time.Second)
		defer cancel()

		err := s.srv.Shutdown(shutdownCtx)
		if err != nil {
			log.Println(err)

			return
		}

		log.Println("server stopped")
	}(ctx)

	fmt.Fprintf(os.Stderr, "visit http://localhost%s\n", s.srv.Addr)

	err := s.srv.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		return err
	}

	return nil
}

type formData struct {
	Name     string
	MaxTime  string
	Keywords []string
	Language string
	Zoom     int
	FastMode bool
	Radius   int
	Lat      string
	Lon      string
	Depth    int
	Email    bool
	Proxies  []string
	// ProxyScrape is true when PROXYSCRAPE_API_KEY is set.
	ProxyScrape bool
	Campaigns   []CampaignSummary
	// ListCampaign preselects the job table filter, from /?campaign=<id>.
	ListCampaign string
}

type ctxKey string

const idCtxKey ctxKey = "id"

func requestWithID(r *http.Request) *http.Request {
	id := r.PathValue("id")
	if id == "" {
		id = r.URL.Query().Get("id")
	}

	parsed, err := uuid.Parse(id)
	if err == nil {
		r = r.WithContext(context.WithValue(r.Context(), idCtxKey, parsed))
	}

	return r
}

func getIDFromRequest(r *http.Request) (uuid.UUID, bool) {
	id, ok := r.Context().Value(idCtxKey).(uuid.UUID)

	return id, ok
}

//nolint:gocritic // this is used in template
func (f formData) ProxiesString() string {
	return strings.Join(f.Proxies, "\n")
}

// RepeatUFs lists the states for "repeat in every municipality of a state".
func (f formData) RepeatUFs() []UFCount { return MunicipalityUFs() }

// CapitalCount is how many jobs "repeat in every capital" creates.
func (f formData) CapitalCount() int { return CapitalCount() }

// MunicipalityCount is how many jobs "repeat in every municipality" creates.
func (f formData) MunicipalityCount() int { return MunicipalityCount() }

//nolint:gocritic // this is used in template
func (f formData) KeywordsString() string {
	return strings.Join(f.Keywords, "\n")
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	data := formData{
		Name:     "",
		MaxTime:  "10m",
		Keywords: []string{},
		Language: resultsLanguage(langFrom(r.Context())),
		Zoom:     15,
		FastMode: false,
		Radius:   50000,
		Lat:      "0",
		Lon:      "0",
		Depth:    10,
		Email:    true,
	}

	campaigns, err := s.svc.ListCampaigns(r.Context())
	if err != nil {
		log.Printf("list campaigns: %v", err)
	} else {
		data.Campaigns = campaigns
	}

	if saved, err := s.svc.SavedProxies(); err != nil {
		log.Printf("saved proxies: %v", err)
	} else {
		data.Proxies = saved
	}

	data.ProxyScrape = s.svc.proxyScrape != nil
	data.ListCampaign = strings.TrimSpace(r.URL.Query().Get("campaign"))

	if err := s.execTemplate(w, r, "static/templates/index.html", data); err != nil {
		log.Printf("render index: %v", err)
	}
}

func (s *Server) scrape(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	err := r.ParseForm()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	campaignID, err := optionalCampaignID(r.Form.Get("campaign_id"))
	if err != nil {
		http.Error(w, "invalid campaign", http.StatusUnprocessableEntity)

		return
	}

	if campaignID != "" {
		if _, err := s.svc.GetCampaign(r.Context(), campaignID); err != nil {
			http.Error(w, publicErr(err), http.StatusUnprocessableEntity)

			return
		}
	}

	newJob := Job{
		ID:     uuid.NewV4().String(),
		Name:   r.Form.Get("name"),
		Date:   time.Now().UTC(),
		Status: StatusPending,
		Data:   JobData{},
	}

	maxTimeStr := r.Form.Get("maxtime")

	maxTime, err := time.ParseDuration(maxTimeStr)
	if err != nil {
		http.Error(w, "invalid max time", http.StatusUnprocessableEntity)

		return
	}

	if maxTime < time.Minute*3 {
		http.Error(w, "max time must be more than 3m", http.StatusUnprocessableEntity)

		return
	}

	newJob.Data.MaxTime = maxTime

	keywordsStr, ok := r.Form["keywords"]
	if !ok {
		http.Error(w, "missing keywords", http.StatusUnprocessableEntity)

		return
	}

	keywords := strings.Split(keywordsStr[0], "\n")
	for _, k := range keywords {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}

		newJob.Data.Keywords = append(newJob.Data.Keywords, k)
	}

	newJob.Data.Lang = r.Form.Get("lang")

	newJob.Data.Zoom, err = strconv.Atoi(r.Form.Get("zoom"))
	if err != nil {
		http.Error(w, "invalid zoom", http.StatusUnprocessableEntity)

		return
	}

	if r.Form.Get("fastmode") == formChecked {
		newJob.Data.FastMode = true
	}

	newJob.Data.Radius, err = strconv.Atoi(r.Form.Get("radius"))
	if err != nil {
		http.Error(w, "invalid radius", http.StatusUnprocessableEntity)

		return
	}

	newJob.Data.Lat = r.Form.Get("latitude")
	newJob.Data.Lon = r.Form.Get("longitude")

	newJob.Data.Depth, err = strconv.Atoi(r.Form.Get("depth"))
	if err != nil {
		http.Error(w, "invalid depth", http.StatusUnprocessableEntity)

		return
	}

	newJob.Data.Email = r.Form.Get("email") == formChecked

	if s.svc.proxyScrape != nil && r.Form.Get("proxy_manual") != formChecked {
		// Default: proxies from the ProxyScrape account.
		newJob.Data.Proxies, err = s.svc.DefaultProxies(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}
	} else {
		for _, p := range strings.Split(r.Form.Get("proxies"), "\n") {
			if p = strings.TrimSpace(p); p != "" {
				newJob.Data.Proxies = append(newJob.Data.Proxies, p)
			}
		}
	}

	if len(newJob.Data.Proxies) == 0 && s.svc.RequiresProxies() {
		http.Error(w, ErrNoProxies.Error(), http.StatusUnprocessableEntity)

		return
	}

	err = newJob.Validate()
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)

		return
	}

	jobs := []Job{newJob}

	// "Repeat in other cities": one job per city, same settings.
	if mode := r.Form.Get("repeat_cities"); mode != RepeatNone {
		cities, err := RepeatCities(mode, r.Form.Get("repeat_uf"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)

			return
		}

		jobs, err = ExpandJobByCities(&newJob, cities, func() string { return uuid.NewV4().String() })
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)

			return
		}
	}

	for i := range jobs {
		// Distinct timestamps keep the cities in list order in the queue.
		jobs[i].Date = newJob.Date.Add(time.Duration(i) * time.Millisecond)
	}

	// One transaction for all of them: thousands of cities stay instant.
	if err := s.svc.CreateJobs(r.Context(), jobs, campaignID); err != nil {
		http.Error(w, publicErr(err), http.StatusInternalServerError)

		return
	}

	if err := s.renderJobGroups(r, w, jobs[0].ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// renderJobGroups writes the home page job list, grouped by campaign.
// newJobID highlights a job that was just created.
func (s *Server) renderJobGroups(r *http.Request, w io.Writer, newJobID string) error {
	page, _ := strconv.Atoi(r.FormValue("list_page"))

	groups, err := s.svc.ListJobTable(r.Context(), newJobID, strings.TrimSpace(r.FormValue("list_campaign")),
		strings.TrimSpace(r.FormValue("list_status")), strings.TrimSpace(r.FormValue("list_sort")), page)
	if err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}

	var output bytes.Buffer
	if err := s.execTemplate(&output, r, "static/templates/job_groups.html", groups); err != nil {
		return fmt.Errorf("render jobs: %w", err)
	}

	if _, err := output.WriteTo(w); err != nil {
		return fmt.Errorf("write jobs: %w", err)
	}

	return nil
}

func (s *Server) getJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	if err := s.renderJobGroups(r, w, ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	ctx := r.Context()

	id, ok := getIDFromRequest(r)
	if !ok {
		http.Error(w, "Invalid ID", http.StatusUnprocessableEntity)

		return
	}

	filePath, err := s.svc.GetCSV(ctx, id.String())
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	file, err := os.Open(filePath) //nolint:gosec // GetCSV returns a validated path rooted in the configured data directory.
	if err != nil {
		http.Error(w, "Failed to open file", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	fileName := filepath.Base(filePath)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", fileName))
	w.Header().Set("Content-Type", "text/csv")

	_, err = io.Copy(w, file)
	if err != nil {
		http.Error(w, "Failed to send file", http.StatusInternalServerError)
		return
	}
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	deleteID, ok := getIDFromRequest(r)
	if !ok {
		http.Error(w, "Invalid ID", http.StatusUnprocessableEntity)

		return
	}

	err := s.svc.Delete(r.Context(), deleteID.String())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	if err := s.renderJobGroups(r, w, ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// selectedJobIDs reads the jobs a bulk action applies to: ids=<id>,<id>,... or,
// with scope=filter, every job of the table filter list_campaign (all pages).
func (s *Server) selectedJobIDs(r *http.Request) ([]string, error) {
	if r.FormValue("scope") == "filter" {
		status := strings.TrimSpace(r.FormValue("list_status"))
		if !ValidJobStatusFilter(status) {
			return nil, errors.New("invalid status")
		}

		return s.svc.JobIDsInFilter(r.Context(), strings.TrimSpace(r.FormValue("list_campaign")), status)
	}

	var ids []string

	for _, v := range r.Form["ids"] {
		for _, id := range strings.Split(v, ",") {
			if id = strings.TrimSpace(id); id != "" {
				if _, err := uuid.Parse(id); err != nil {
					return nil, errors.New("Invalid ID")
				}

				ids = append(ids, id)
			}
		}
	}

	return ids, nil
}

// deleteMany deletes several jobs at once. POST /jobs/delete with ids=... or scope=filter.
// retryJob queues a failed job again and redraws the job table. POST /jobs/retry?id=
func (s *Server) retryJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	id, ok := getIDFromRequest(r)
	if !ok {
		http.Error(w, "Invalid ID", http.StatusUnprocessableEntity)

		return
	}

	if err := s.svc.RetryJob(r.Context(), id.String()); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, ErrJobNotFailed) {
			code = http.StatusConflict
		}

		http.Error(w, trFrom(r, publicJobErr(err)), code)

		return
	}

	if err := s.renderJobGroups(r, w, ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// publicJobErr is the message shown for a failed job action.
func publicJobErr(err error) string {
	if errors.Is(err, ErrJobNotFailed) {
		return "Only failed jobs can be run again."
	}

	return "The job could not be queued again."
}

func (s *Server) deleteMany(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)

		return
	}

	ids, err := s.selectedJobIDs(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)

		return
	}

	if len(ids) == 0 {
		http.Error(w, "no jobs selected", http.StatusUnprocessableEntity)

		return
	}

	if err := s.svc.DeleteMany(r.Context(), ids); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	if err := s.renderJobGroups(r, w, ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// moveJobs changes the campaign of several jobs at once. POST /jobs/campaign
// with ids=... or scope=filter, and either target=<campaign id>, target=none
// (no campaign), or target=new with new_campaign_name (and optional servico).
// A new campaign is answered with HX-Refresh so every campaign list on the page updates.
func (s *Server) moveJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)

		return
	}

	ids, err := s.selectedJobIDs(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)

		return
	}

	if len(ids) == 0 {
		http.Error(w, "no jobs selected", http.StatusUnprocessableEntity)

		return
	}

	target := strings.TrimSpace(r.FormValue("target"))
	created := false

	switch target {
	case "":
		http.Error(w, "choose a campaign", http.StatusUnprocessableEntity)

		return
	case noCampaignKey:
		target = ""
	case "new":
		target, err = s.svc.CreateCampaignNamed(r.Context(), r.FormValue("new_campaign_name"), r.FormValue("servico"))
		if err != nil {
			http.Error(w, publicErr(err), http.StatusUnprocessableEntity)

			return
		}

		created = true
	default:
		if _, err := uuid.Parse(target); err != nil {
			http.Error(w, "invalid campaign", http.StatusUnprocessableEntity)

			return
		}
	}

	if err := s.svc.MoveJobs(r.Context(), target, ids); err != nil {
		http.Error(w, publicErr(err), http.StatusUnprocessableEntity)

		return
	}

	if created {
		w.Header().Set("HX-Refresh", "true")
	}

	if err := s.renderJobGroups(r, w, ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type apiScrapeRequest struct {
	Name string
	JobData
}

type apiScrapeResponse struct {
	ID string `json:"id"`
}

func (s *Server) redocHandler(w http.ResponseWriter, _ *http.Request) {
	tmpl, ok := s.tmpl["static/templates/redoc.html"]
	if !ok {
		http.Error(w, "missing tpl", http.StatusInternalServerError)

		return
	}

	_ = tmpl.Execute(w, nil)
}

func (s *Server) apiScrape(w http.ResponseWriter, r *http.Request) {
	var req apiScrapeRequest

	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		ans := apiError{
			Code:    http.StatusUnprocessableEntity,
			Message: err.Error(),
		}

		renderJSON(w, http.StatusUnprocessableEntity, ans)

		return
	}

	newJob := Job{
		ID:     uuid.NewV4().String(),
		Name:   req.Name,
		Date:   time.Now().UTC(),
		Status: StatusPending,
		Data:   req.JobData,
	}

	// convert to seconds
	newJob.Data.MaxTime *= time.Second

	err = newJob.Validate()
	if err != nil {
		ans := apiError{
			Code:    http.StatusUnprocessableEntity,
			Message: err.Error(),
		}

		renderJSON(w, http.StatusUnprocessableEntity, ans)

		return
	}

	err = s.svc.Create(r.Context(), &newJob)
	if err != nil {
		ans := apiError{
			Code:    http.StatusInternalServerError,
			Message: err.Error(),
		}

		renderJSON(w, http.StatusInternalServerError, ans)

		return
	}

	ans := apiScrapeResponse{
		ID: newJob.ID,
	}

	renderJSON(w, http.StatusCreated, ans)
}

func (s *Server) apiGetJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.svc.All(r.Context())
	if err != nil {
		apiError := apiError{
			Code:    http.StatusInternalServerError,
			Message: err.Error(),
		}

		renderJSON(w, http.StatusInternalServerError, apiError)

		return
	}

	renderJSON(w, http.StatusOK, jobs)
}

func (s *Server) apiGetJob(w http.ResponseWriter, r *http.Request) {
	id, ok := getIDFromRequest(r)
	if !ok {
		apiError := apiError{
			Code:    http.StatusUnprocessableEntity,
			Message: "Invalid ID",
		}

		renderJSON(w, http.StatusUnprocessableEntity, apiError)

		return
	}

	job, err := s.svc.Get(r.Context(), id.String())
	if err != nil {
		apiError := apiError{
			Code:    http.StatusNotFound,
			Message: http.StatusText(http.StatusNotFound),
		}

		renderJSON(w, http.StatusNotFound, apiError)

		return
	}

	renderJSON(w, http.StatusOK, job)
}

// viewJob renders the map modal fragment for a job, embedding the job's places
// directly so the client needs no separate data request.
func (s *Server) viewJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	id, ok := getIDFromRequest(r)
	if !ok {
		http.Error(w, "Invalid ID", http.StatusUnprocessableEntity)

		return
	}

	places, err := s.svc.GetPlaces(r.Context(), id.String())
	if err != nil {
		if !errors.Is(err, ErrPlacesNotFound) {
			log.Printf("view job %s: %v", id, err) //nolint:gosec // id is a parsed UUID and cannot contain log control characters.
			http.Error(w, "internal server error", http.StatusInternalServerError)

			return
		}

		// No CSV yet: render the modal with an empty state rather than an error.
		places = []Place{}
	}

	var buf bytes.Buffer
	if err := s.execTemplate(&buf, r, "static/templates/job_view.html", places); err != nil {
		log.Printf("view job %s: render: %v", id, err) //nolint:gosec // id is a parsed UUID and cannot contain log control characters.
		http.Error(w, "internal server error", http.StatusInternalServerError)

		return
	}

	_, _ = buf.WriteTo(w)
}

func (s *Server) apiDeleteJob(w http.ResponseWriter, r *http.Request) {
	id, ok := getIDFromRequest(r)
	if !ok {
		apiError := apiError{
			Code:    http.StatusUnprocessableEntity,
			Message: "Invalid ID",
		}

		renderJSON(w, http.StatusUnprocessableEntity, apiError)

		return
	}

	err := s.svc.Delete(r.Context(), id.String())
	if err != nil {
		apiError := apiError{
			Code:    http.StatusInternalServerError,
			Message: err.Error(),
		}

		renderJSON(w, http.StatusInternalServerError, apiError)

		return
	}

	w.WriteHeader(http.StatusOK)
}

// proxyScrapeStatus tests the ProxyScrape API key. GET /proxyscrape/status[?refresh=1]
func (s *Server) proxyScrapeStatus(w http.ResponseWriter, r *http.Request) {
	st := s.svc.ProxyScrapeStatus(r.Context(), r.URL.Query().Get("refresh") == "1")

	code := http.StatusOK
	if !st.OK {
		code = http.StatusBadGateway
		if !st.Configured {
			code = http.StatusServiceUnavailable
		}
	}

	renderJSON(w, code, st)
}

// home is the dashboard: machine, scraper, proxies, email and backups at a glance.
// The numbers load right after the page, so it opens instantly.
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)

		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	if err := s.execTemplate(w, r, "static/templates/dashboard.html", nil); err != nil {
		log.Printf("render dashboard: %v", err)
	}
}

// dashboardStats renders the dashboard cards; the page polls it every few seconds.
func (s *Server) dashboardStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	if err := s.execTemplate(w, r, "static/templates/dashboard_cards.html", s.svc.Dashboard(r.Context())); err != nil {
		log.Printf("render dashboard cards: %v", err)
	}
}

// backupNow makes a database backup right away and redraws the dashboard cards.
func (s *Server) backupNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	if _, err := s.svc.BackupNow(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	get := r.Clone(r.Context())
	get.Method = http.MethodGet

	s.dashboardStats(w, get)
}

func renderJSON(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)

	_ = json.NewEncoder(w).Encode(data)
}

func formatDate(t time.Time) string {
	return t.Format("Jan 02, 2006 15:04:05")
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' cdn.redoc.ly cdnjs.cloudflare.com 'unsafe-inline' 'unsafe-eval'; "+
				"worker-src 'self' blob:; "+
				"style-src 'self' 'unsafe-inline' fonts.googleapis.com cdnjs.cloudflare.com; "+
				"img-src 'self' data: cdn.redoc.ly cdnjs.cloudflare.com *.tile.openstreetmap.org; "+
				// cdnjs serves the Font Awesome icon fonts.
				"font-src 'self' fonts.gstatic.com cdnjs.cloudflare.com; "+
				"connect-src 'self'")

		next.ServeHTTP(w, r)
	})
}

// pushBoostedURL tells htmx which URL a boosted navigation ended on. A boosted
// form that redirects is followed by the browser, and without this header the
// address bar would keep the form's action instead of the page shown.
func pushBoostedURL(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.Header.Get("HX-Boosted") == "true" {
			w.Header().Set("HX-Push-Url", r.URL.RequestURI())
		}

		next.ServeHTTP(w, r)
	})
}

// execTemplate renders the named template in the request language.
func (s *Server) execTemplate(w io.Writer, r *http.Request, name string, data any) error {
	tmpl, ok := s.tmpl[name]
	if !ok {
		return fmt.Errorf("missing template %s", name)
	}

	localizedTmpl, err := localized(tmpl, r)
	if err != nil {
		return fmt.Errorf("localize %s: %w", name, err)
	}

	return localizedTmpl.Execute(w, data)
}

// resultsLanguage is the default Google Maps results language for an interface language.
func resultsLanguage(lang Lang) string {
	if lang == LangPTBR {
		return "pt"
	}

	return "en"
}
