package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"time"
	"uuid"
)

// errCampaignsUnavailable means the job repository cannot store campaigns.
var errCampaignsUnavailable = errors.New("campaigns are not available")

func (s *Service) campaignStore() (CampaignStore, error) {
	store, ok := s.repo.(CampaignStore)
	if !ok || store == nil {
		return nil, errCampaignsUnavailable
	}

	return store, nil
}

// ListCampaigns returns campaigns for the nav and the job form.
// A repository without campaign storage returns an empty list.
func (s *Service) ListCampaigns(ctx context.Context) ([]CampaignSummary, error) {
	store, ok := s.repo.(CampaignStore)
	if !ok || store == nil {
		return nil, nil
	}

	return store.ListCampaigns(ctx)
}

// CreateCampaign stores a new campaign.
func (s *Service) CreateCampaign(ctx context.Context, c *Campaign) error {
	store, err := s.campaignStore()
	if err != nil {
		return err
	}

	c.Name = strings.TrimSpace(c.Name)

	c.ServiceName = strings.TrimSpace(c.ServiceName)
	if c.ServiceName == "" {
		c.ServiceName = ServiceTrentin
	}

	if err := requireService(ctx, store, c.ServiceName); err != nil {
		return err
	}

	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}

	if err := c.Validate(); err != nil {
		return err
	}

	return store.CreateCampaign(ctx, c)
}

// GetCampaign loads one campaign.
func (s *Service) GetCampaign(ctx context.Context, id string) (Campaign, error) {
	store, err := s.campaignStore()
	if err != nil {
		return Campaign{}, err
	}

	return store.GetCampaign(ctx, id)
}

// RenameCampaign changes a campaign name.
func (s *Service) RenameCampaign(ctx context.Context, id, name string) error {
	store, err := s.campaignStore()
	if err != nil {
		return err
	}

	c := Campaign{ID: id, Name: name}
	if err := c.Validate(); err != nil {
		return err
	}

	return store.RenameCampaign(ctx, id, c.Name)
}

// SetCampaignService changes the service a campaign sends as.
func (s *Service) SetCampaignService(ctx context.Context, id, service string) error {
	store, err := s.campaignStore()
	if err != nil {
		return err
	}

	service = strings.TrimSpace(service)
	if err := requireService(ctx, store, service); err != nil {
		return err
	}

	return store.SetCampaignService(ctx, id, service)
}

// requireService returns ErrInvalidService unless name is a registered service.
func requireService(ctx context.Context, store CampaignStore, name string) error {
	if !ValidServiceName(name) {
		return ErrInvalidService
	}

	if _, err := store.GetService(ctx, name); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrInvalidService
		}

		return err
	}

	return nil
}

// campaignService returns the validated service of a stored campaign.
func campaignService(ctx context.Context, store CampaignStore, id string) (string, error) {
	c, err := store.GetCampaign(ctx, id)
	if err != nil {
		return "", err
	}

	if err := requireService(ctx, store, c.ServiceName); err != nil {
		return "", err
	}

	return c.ServiceName, nil
}

// DeleteCampaign removes the campaign, its email settings, and its send history.
// Jobs and their CSV files stay.
func (s *Service) DeleteCampaign(ctx context.Context, id string) error {
	store, err := s.campaignStore()
	if err != nil {
		return err
	}

	return store.DeleteCampaign(ctx, id)
}

// DeleteCampaignWithJobs deletes a campaign and every job in it, with their CSV files.
func (s *Service) DeleteCampaignWithJobs(ctx context.Context, id string) (int, error) {
	jobs, err := s.CampaignJobList(ctx, id)
	if err != nil {
		return 0, err
	}

	ids := make([]string, len(jobs))
	for i := range jobs {
		ids[i] = jobs[i].ID
	}

	if err := s.DeleteMany(ctx, ids); err != nil {
		return 0, err
	}

	return len(ids), s.DeleteCampaign(ctx, id)
}

// AssignJob moves a job into a campaign.
func (s *Service) AssignJob(ctx context.Context, campaignID, jobID string) error {
	store, err := s.campaignStore()
	if err != nil {
		return err
	}

	if _, err := s.repo.Get(ctx, jobID); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) {
			return ErrJobNotFound
		}

		return err
	}

	return store.AssignJob(ctx, campaignID, jobID)
}

// UnassignJob removes a job from a campaign without deleting the job.
func (s *Service) UnassignJob(ctx context.Context, campaignID, jobID string) error {
	store, err := s.campaignStore()
	if err != nil {
		return err
	}

	return store.UnassignJob(ctx, campaignID, jobID)
}

// CampaignJobList returns the jobs in a campaign, newest assignment first.
func (s *Service) CampaignJobList(ctx context.Context, campaignID string) ([]Job, error) {
	store, err := s.campaignStore()
	if err != nil {
		return nil, err
	}

	ids, err := store.CampaignJobs(ctx, campaignID)
	if err != nil {
		return nil, err
	}

	jobs := make([]Job, 0, len(ids))

	for _, id := range ids {
		job, err := s.repo.Get(ctx, id)
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) {
			continue
		}

		if err != nil {
			return nil, err
		}

		jobs = append(jobs, job)
	}

	return jobs, nil
}

// SaveCampaignMail stores email settings. An empty password keeps the saved one.
func (s *Service) SaveCampaignMail(ctx context.Context, id string, cfg *MailConfig) error {
	store, err := s.campaignStore()
	if err != nil {
		return err
	}

	if _, err := store.GetCampaign(ctx, id); err != nil {
		return err
	}

	normalized := cfg.Normalize()

	if err := normalized.ValidateShape(); err != nil {
		return err
	}

	if normalized.Password == "" {
		existing, ok, err := store.GetMail(ctx, id)
		if err != nil {
			return err
		}

		if ok {
			normalized.Password = existing.Password
		}
	}

	if err := store.SaveMail(ctx, id, &normalized); err != nil {
		return err
	}

	// A send in progress (or paused) uses the new server, login and sender
	// from its next message on.
	s.mailSettingsChanged(id)

	return nil
}

// SaveCampaignMessage stores the subject and body. Each sender keeps its own
// address, name, server and password.
func (s *Service) SaveCampaignMessage(ctx context.Context, id, subject, body string, format MailFormat) error {
	store, err := s.campaignStore()
	if err != nil {
		return err
	}

	existing, ok, err := store.GetMail(ctx, id)
	if err != nil {
		return err
	}

	if !ok {
		existing = MailConfig{Port: defaultSMTPPort, Encryption: MailStartTLS}
	}

	existing.Subject = subject
	existing.Body = body
	existing.Format = format

	return s.SaveCampaignMail(ctx, id, &existing)
}

// CampaignMailForForm returns settings safe to render. The password is cleared.
func (s *Service) CampaignMailForForm(ctx context.Context, id string) (MailConfig, bool, error) {
	store, err := s.campaignStore()
	if err != nil {
		return MailConfig{}, false, err
	}

	cfg, ok, err := store.GetMail(ctx, id)
	if err != nil {
		return MailConfig{}, false, err
	}

	if !ok {
		return MailConfig{Port: defaultSMTPPort, Encryption: MailStartTLS}, false, nil
	}

	set := cfg.Password != ""
	cfg.Password = ""

	return cfg.Normalize(), set, nil
}

// SendTestMail sends one message with the posted settings and does not save them.
func (s *Service) SendTestMail(ctx context.Context, id string, cfg *MailConfig, to string, send MailSender) error {
	store, err := s.campaignStore()
	if err != nil {
		return err
	}

	service, err := campaignService(ctx, store, id)
	if err != nil {
		return err
	}

	merged, err := s.mergePassword(ctx, store, id, cfg)
	if err != nil {
		return err
	}

	if err := merged.ReadyToSend(); err != nil {
		return err
	}

	toAddr, err := addressOnly(to)
	if err != nil {
		return errors.New("invalid test address")
	}

	subject, body, err := renderTest(&merged, toAddr)
	if err != nil {
		return err
	}

	if send == nil {
		send = SendSMTP
	}

	return s.deliver(ctx, &merged, service, &OutgoingMail{To: toAddr, Subject: subject, Body: body}, send)
}

// CheckCampaignMail dials the posted SMTP server and does not save or send.
func (s *Service) CheckCampaignMail(ctx context.Context, id string, cfg *MailConfig) error {
	store, err := s.campaignStore()
	if err != nil {
		return err
	}

	if _, err := store.GetCampaign(ctx, id); err != nil {
		return err
	}

	merged, err := s.mergePassword(ctx, store, id, cfg)
	if err != nil {
		return err
	}

	if err := merged.ValidateShape(); err != nil {
		return err
	}

	if merged.Host == "" {
		return errors.New("SMTP host is required")
	}

	if merged.Username != "" && merged.Password == "" {
		return errors.New("password is required to test the login")
	}

	return CheckSMTP(ctx, &merged)
}

// PreviewCampaignMail renders the subject and body with sample business data.
func (s *Service) PreviewCampaignMail(ctx context.Context, id string, cfg *MailConfig) (subject, body string, err error) {
	store, err := s.campaignStore()
	if err != nil {
		return "", "", err
	}

	if _, err := store.GetCampaign(ctx, id); err != nil {
		return "", "", err
	}

	normalized := cfg.Normalize()
	if err := normalized.ValidateShape(); err != nil {
		return "", "", err
	}

	if normalized.Subject == "" || normalized.Body == "" {
		return "", "", errors.New("subject and body are required to preview")
	}

	return renderTest(&normalized, "preview@example.com")
}

func renderTest(cfg *MailConfig, to string) (subject, body string, err error) {
	vars := MailVars{
		Title:    "Test business",
		Email:    to,
		Phone:    "+1 555 0100",
		Website:  "https://example.com",
		Address:  "1 Test Street",
		Category: "Example",
	}

	subjectT, bodyT, err := parseMailTemplates(cfg.Subject, cfg.Body)
	if err != nil {
		return "", "", errors.New("email template is invalid")
	}

	return renderMail(subjectT, bodyT, &vars)
}

// DispatchPreview counts addresses before a send.
func (s *Service) DispatchPreview(ctx context.Context, campaignID string) (DispatchPreview, error) {
	store, err := s.campaignStore()
	if err != nil {
		return DispatchPreview{}, err
	}

	recs, missing, err := s.collectRecipients(ctx, store, campaignID)
	if err != nil {
		return DispatchPreview{}, err
	}

	sent, err := store.SentEmails(ctx, campaignID)
	if err != nil {
		return DispatchPreview{}, err
	}

	already := 0

	for i := range recs {
		if _, ok := sent[strings.ToLower(recs[i].Email)]; ok {
			already++
		}
	}

	return DispatchPreview{
		Ready:          len(recs) - already,
		AlreadySent:    already,
		MissingResults: missing,
	}, nil
}

// ListDispatches returns the newest dispatch rows.
func (s *Service) ListDispatches(ctx context.Context, campaignID string) ([]Dispatch, error) {
	store, err := s.campaignStore()
	if err != nil {
		return nil, err
	}

	return store.ListDispatches(ctx, campaignID, 10)
}

// maxHistoryDispatches caps how many past runs the paged history can show.
const maxHistoryDispatches = 1000

// ListDispatchHistory returns the newest dispatch rows for the paged history.
func (s *Service) ListDispatchHistory(ctx context.Context, campaignID string) ([]Dispatch, error) {
	store, err := s.campaignStore()
	if err != nil {
		return nil, err
	}

	return store.ListDispatches(ctx, campaignID, maxHistoryDispatches)
}

// DispatchRunning reports whether a send is still in progress.
func (s *Service) DispatchRunning(ctx context.Context, campaignID string) (bool, error) {
	store, err := s.campaignStore()
	if err != nil {
		return false, err
	}

	running, ok, err := store.LatestRunningDispatch(ctx, campaignID)
	if err != nil || !ok {
		return false, err
	}

	// The in-process lock keeps a long paced run visible after the two-hour mark.
	// A row with no live goroutine still expires on that mark.
	if s.dispatchInProcess(campaignID) {
		return true, nil
	}

	return time.Since(running.StartedAt) < dispatchStaleAfter, nil
}

// StartDispatch sends the campaign mail in the background.
// DispatchOptions controls who a send goes to.
type DispatchOptions struct {
	// Resend also sends to addresses that got the campaign at least ResendAfter ago.
	Resend      bool
	ResendAfter time.Duration
	// SkipFailed leaves out addresses whose last send failed.
	SkipFailed bool
	// OnlyFailed sends only to addresses whose last send failed.
	OnlyFailed bool
	// Quiet records nothing in the history when there is nothing to send.
	Quiet bool
	// AtOnce is how many emails leave together. Zero uses the saved number, or one.
	AtOnce int
}

// StartDispatch sends the campaign to every address not sent yet. With resend,
// addresses that got it at least resendAfter ago are sent again too.
func (s *Service) StartDispatch(ctx context.Context, campaignID string, resend bool, resendAfter time.Duration, send MailSender) (DispatchResult, error) {
	return s.startDispatch(ctx, campaignID, DispatchOptions{Resend: resend, ResendAfter: resendAfter}, send)
}

// RequeueFailed puts the addresses whose last send failed back among the ones
// waiting to be sent, without sending anything now. The next dispatch, manual
// or automatic, sends them with the rest.
func (s *Service) RequeueFailed(ctx context.Context, campaignID string) (int, error) {
	store, err := s.campaignStore()
	if err != nil {
		return 0, err
	}

	if _, err := store.GetCampaign(ctx, campaignID); err != nil {
		return 0, err
	}

	return store.RequeueFailed(ctx, campaignID)
}

// RetryFailedDispatch queues again only the addresses whose last send failed
// and sends them now, without reaching addresses not sent yet.
func (s *Service) RetryFailedDispatch(ctx context.Context, campaignID string, send MailSender) (DispatchResult, error) {
	return s.startDispatch(ctx, campaignID, DispatchOptions{OnlyFailed: true}, send)
}

func (s *Service) startDispatch(ctx context.Context, campaignID string, opts DispatchOptions, send MailSender) (DispatchResult, error) {
	store, err := s.campaignStore()
	if err != nil {
		return DispatchResult{}, err
	}

	if err := s.expireStaleDispatch(ctx, store, campaignID); err != nil {
		return DispatchResult{}, err
	}

	if err := s.lockDispatch(campaignID); err != nil {
		return DispatchResult{}, err
	}

	started := false

	defer func() {
		if !started {
			s.unlockDispatch(campaignID)
		}
	}()

	if running, err := s.DispatchRunning(ctx, campaignID); err != nil || running {
		if err != nil {
			return DispatchResult{}, err
		}

		return DispatchResult{}, ErrDispatchRunning
	}

	// An invalid service stops the run before anything is queued.
	service, err := campaignService(ctx, store, campaignID)
	if err != nil {
		return DispatchResult{}, err
	}

	cfg, err := s.mailReady(ctx, store, campaignID)
	if err != nil {
		return DispatchResult{}, err
	}

	recs, missing, err := s.collectRecipients(ctx, store, campaignID)
	if err != nil {
		return DispatchResult{}, err
	}

	var since *time.Time

	if opts.Resend {
		t := time.Now().UTC().Add(-max(opts.ResendAfter, 0))
		since = &t
	}

	recs, skipped, err := dropSent(ctx, store, campaignID, recs, since)
	if err != nil {
		return DispatchResult{}, err
	}

	// An address that another campaign of the same service already reached is
	// skipped too, so one business does not get the same pitch twice.
	recs, sameService, err := dropSentByService(ctx, s.repo, service, campaignID, recs, since)
	if err != nil {
		return DispatchResult{}, err
	}

	skipped += sameService

	if opts.SkipFailed {
		if recs, err = dropFailed(ctx, store, campaignID, recs); err != nil {
			return DispatchResult{}, err
		}
	}

	if opts.OnlyFailed {
		if recs, err = keepFailed(ctx, store, campaignID, recs); err != nil {
			return DispatchResult{}, err
		}

		// Skips counted above are the addresses that never failed; they are not news here.
		skipped = 0
	}

	msgs, renderFailed, err := prepareMessages(&cfg, recs)
	if err != nil {
		return DispatchResult{}, err
	}

	if len(msgs) == 0 && opts.OnlyFailed {
		return DispatchResult{Detail: "No failed addresses to send again."}, nil
	}

	if len(msgs) == 0 && opts.Quiet {
		return DispatchResult{Detail: "Nothing new to send."}, nil
	}

	if len(msgs) == 0 {
		detail := emptyDispatchDetail(skipped, missing, renderFailed)
		if err := s.finishNow(ctx, store, campaignID, skipped, renderFailed, detail); err != nil {
			return DispatchResult{}, err
		}

		return DispatchResult{Detail: detail}, nil
	}

	dispatchID := uuid.NewV4().String()

	err = store.InsertDispatch(ctx, &Dispatch{
		ID:         dispatchID,
		CampaignID: campaignID,
		Status:     DispatchRunning,
		StartedAt:  time.Now().UTC(),
		Skipped:    skipped,
	})
	if err != nil {
		return DispatchResult{}, err
	}

	if send == nil {
		send = SendSMTP
	}

	atOnce := opts.AtOnce
	if atOnce < 1 {
		atOnce = 1

		if saved, savedErr := s.GetAutoDispatch(ctx, campaignID); savedErr == nil && saved.AtOnce > 0 {
			atOnce = saved.AtOnce
		}
	}

	started = true

	go s.runDispatch(campaignID, dispatchID, service, &cfg, msgs, skipped, missing, renderFailed, send, atOnce) //nolint:gosec // G118: dispatch continues after the HTTP handler returns.

	return DispatchResult{Started: true, Detail: "Dispatch started."}, nil
}

func (s *Service) mailReady(ctx context.Context, store CampaignStore, campaignID string) (MailConfig, error) {
	cfg, ok, err := store.GetMail(ctx, campaignID)
	if err != nil {
		return MailConfig{}, err
	}

	if !ok {
		return MailConfig{}, ErrMailNotReady
	}

	cfg = cfg.Normalize()
	if err := cfg.ReadyToSend(); err != nil {
		return MailConfig{}, err
	}

	return cfg, nil
}

func (s *Service) mergePassword(ctx context.Context, store CampaignStore, id string, cfg *MailConfig) (MailConfig, error) {
	out := cfg.Normalize()
	if out.Password != "" {
		return out, nil
	}

	existing, ok, err := store.GetMail(ctx, id)
	if err != nil {
		return MailConfig{}, err
	}

	if ok {
		out.Password = existing.Password
	}

	return out, nil
}

func (s *Service) finishNow(ctx context.Context, store CampaignStore, campaignID string, skipped, failed int, detail string) error {
	status := DispatchOK
	if failed > 0 && skipped == 0 {
		status = DispatchFailed
	}

	return store.InsertDispatch(ctx, &Dispatch{
		ID:         uuid.NewV4().String(),
		CampaignID: campaignID,
		Status:     status,
		StartedAt:  time.Now().UTC(),
		FinishedAt: time.Now().UTC(),
		Failed:     failed,
		Skipped:    skipped,
		Detail:     detail,
	})
}

func emptyDispatchDetail(skipped, missing, failed int) string {
	switch {
	case skipped > 0:
		return fmt.Sprintf("No new email addresses. Skipped %d already sent or unsubscribed.", skipped)
	case missing > 0:
		return fmt.Sprintf("No email addresses to send. %d jobs had no results file.", missing)
	case failed > 0:
		return "No email addresses to send. The template failed for every row."
	default:
		return "No email addresses to send."
	}
}

// dropSent removes addresses that already got the campaign. With since set,
// only sends at or after since count, so older recipients get it again.
func dropSent(ctx context.Context, store CampaignStore, campaignID string, recs []Recipient, since *time.Time) ([]Recipient, int, error) {
	var (
		sent map[string]struct{}
		err  error
	)

	if since == nil {
		sent, err = store.SentEmails(ctx, campaignID)
	} else {
		sent, err = store.SentEmailsSince(ctx, campaignID, *since)
	}

	if err != nil {
		return nil, 0, err
	}

	kept := make([]Recipient, 0, len(recs))
	skipped := 0

	for i := range recs {
		if _, ok := sent[strings.ToLower(recs[i].Email)]; ok {
			skipped++

			continue
		}

		kept = append(kept, recs[i])
	}

	return kept, skipped, nil
}

// RecoverInterruptedDispatches marks every dispatch still "running" as
// interrupted. Call this once when the server starts: nothing can really be
// sending yet, but a dispatch row left "running" by the previous process
// otherwise still passes DispatchRunning's check for up to dispatchStaleAfter
// (2h), silently blocking every auto or manual send for the campaign until
// then.
func (s *Service) RecoverInterruptedDispatches(ctx context.Context) (int, error) {
	store, err := s.campaignStore()
	if err != nil {
		if errors.Is(err, errCampaignsUnavailable) {
			return 0, nil
		}

		return 0, err
	}

	return store.ExpireRunningDispatches(ctx)
}

func (s *Service) expireStaleDispatch(ctx context.Context, store CampaignStore, campaignID string) error {
	// A paced run can outlive the two-hour mark. Only a dead process is interrupted.
	if s.dispatchInProcess(campaignID) {
		return nil
	}

	running, ok, err := store.LatestRunningDispatch(ctx, campaignID)
	if err != nil || !ok {
		return err
	}

	if time.Since(running.StartedAt) < dispatchStaleAfter {
		return nil
	}

	running.Status = DispatchFailed
	running.FinishedAt = time.Now().UTC()
	running.Detail = "interrupted"

	return store.UpdateDispatch(ctx, &running)
}

func (s *Service) dispatchInProcess(id string) bool {
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()

	if s.dispatching == nil {
		return false
	}

	_, ok := s.dispatching[id]

	return ok
}

func (s *Service) lockDispatch(id string) error {
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()

	if s.dispatching == nil {
		s.dispatching = map[string]struct{}{}
	}

	if _, ok := s.dispatching[id]; ok {
		return ErrDispatchRunning
	}

	s.dispatching[id] = struct{}{}

	return nil
}

func (s *Service) unlockDispatch(id string) {
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()

	delete(s.dispatching, id)
}

// dispatchProgress is the shared tally of one run. The queue cursor advances
// under mu before anyone sends, so two emails never take the same address.
type dispatchProgress struct {
	mu                         sync.Mutex
	next                       int
	sent, failed, unsubscribed int
	firstErr                   string
	halt                       error
	// deadSenders holds, per sender, when it may be tried again after a login
	// refusal. A refusal is often a provider's temporary hold, so the sender
	// comes back on its own instead of sitting out a run that can last days.
	deadSenders map[string]time.Time
	now         func() time.Time
}

// senderRetryAfter is how long a sender whose login was refused stays out of
// the rotation before the run tries it again.
const senderRetryAfter = 30 * time.Minute

func (p *dispatchProgress) clock() time.Time {
	if p.now != nil {
		return p.now()
	}

	return time.Now()
}

func (p *dispatchProgress) snapshot() (sent, failed, unsubscribed int, firstErr string, halt error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.sent, p.failed, p.unsubscribed, p.firstErr, p.halt
}

// claim reserves the next n messages. n is already capped at the messages left.
func (p *dispatchProgress) claim(n, total int) (start, end int, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.halt != nil || p.next >= total || n < 1 {
		return 0, 0, false
	}

	start = p.next
	end = start + n

	if end > total {
		end = total
	}

	p.next = end

	return start, end, true
}

func (p *dispatchProgress) senderDead(cfg MailConfig) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := senderKey(cfg)

	until, ok := p.deadSenders[key]
	if !ok {
		return false
	}

	if p.clock().Before(until) {
		return true
	}

	delete(p.deadSenders, key)
	log.Printf("sender %s is back in the rotation", cfg.FromEmail)

	return false
}

func (p *dispatchProgress) killSender(cfg MailConfig) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.deadSenders == nil {
		p.deadSenders = map[string]time.Time{}
	}

	until := p.clock().Add(senderRetryAfter)
	p.deadSenders[senderKey(cfg)] = until
	log.Printf("sender %s refused the login; trying it again at %s", cfg.FromEmail, until.Format(time.TimeOnly))
}

// reviveSenders puts every sender back in the rotation. Saved settings may
// hold a fixed password, so they get a fresh try right away.
func (p *dispatchProgress) reviveSenders() {
	p.mu.Lock()
	defer p.mu.Unlock()

	clear(p.deadSenders)
}

func (p *dispatchProgress) setHalt(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.halt == nil {
		p.halt = err
	}
}

func (s *Service) runDispatch(campaignID, dispatchID, service string, cfg *MailConfig, msgs []renderedMail, skipped, missing, renderFailed int, send MailSender, atOnce int) {
	defer s.unlockDispatch(campaignID)

	ctl := s.openDispatchControl(campaignID)
	defer s.closeDispatchControl(campaignID, ctl)

	ctx := context.Background()

	store, err := s.campaignStore()
	if err != nil {
		return
	}

	// One email per live sender. The saved "at once" number is not used:
	// three senders leave three emails together, each to a different address.
	_ = atOnce
	msgs = uniqueRecipients(msgs)

	prog := &dispatchProgress{failed: renderFailed, now: s.dispatchNow}

	for {
		sent, failed, unsubscribed, _, halt := prog.snapshot()
		if halt != nil || prog.next >= len(msgs) {
			break
		}

		// Pause and SMTP reload happen before the wave, so a wave shares one cfg.
		if s.dispatchCheckpoint(ctx, store, ctl, campaignID, dispatchID, cfg, sent, failed, skipped+unsubscribed) {
			prog.reviveSenders()
		}

		extras := s.smtpAccounts(ctx, campaignID)
		accounts := liveAccounts(*cfg, extras, prog.senderDead)
		if len(accounts) == 0 {
			prog.setHalt(errSenderLogin)

			break
		}

		start, end, ok := prog.claim(len(accounts), len(msgs))
		if !ok {
			break
		}

		if end-start < len(accounts) {
			accounts = accounts[:end-start]
		}

		sentBefore, failedBefore, _, _, _ := prog.snapshot()

		wave := msgs[start:end]

		s.sendWave(ctx, store, prog, service, campaignID, dispatchID, wave, *cfg, extras, accounts, send)

		sent, failed, unsubscribed, _, halt = prog.snapshot()
		if halt != nil || end >= len(msgs) {
			break
		}

		// An address that only opted out is not a send, so the run does not wait.
		if sent == sentBefore && failed == failedBefore {
			continue
		}

		wait, detail := s.nextPause(sent, failed, skipped+unsubscribed, end-start, end/mailBatchSize > start/mailBatchSize)
		detail += " " + timeLeftLabel(wait, end, len(msgs), end-start)

		if err := s.writeDispatchProgress(ctx, store, dispatchID, sent, failed, skipped+unsubscribed, detail); err != nil {
			log.Printf("update dispatch %s: %v", dispatchID, err)
		}

		ctl.setNextSend(time.Now().Add(wait))
		s.paceSleep(ctx, wait)
		ctl.setNextSend(time.Time{})
	}

	sent, failed, unsubscribed, firstErr, halt := prog.snapshot()

	status := DispatchOK
	if halt != nil || (sent == 0 && failed > 0) {
		status = DispatchFailed
	}

	detail := fmt.Sprintf("Sent %d, failed %d, skipped %d.", sent, failed, skipped+unsubscribed)
	if unsubscribed > 0 {
		detail += fmt.Sprintf(" %d unsubscribed from %s.", unsubscribed, service)
	}

	if missing > 0 {
		detail += fmt.Sprintf(" %d jobs had no results file.", missing)
	}

	if halt != nil {
		detail = "Stopped: " + halt.Error() + ". The remaining addresses stay queued. " + detail
	}

	if firstErr != "" {
		detail += " First failure: " + trimDetail(firstErr)
	}

	err = store.UpdateDispatch(ctx, &Dispatch{
		ID:         dispatchID,
		Status:     status,
		FinishedAt: time.Now().UTC(),
		Sent:       sent,
		Failed:     failed,
		Skipped:    skipped + unsubscribed,
		Detail:     trimDetail(detail),
	})
	if err != nil {
		log.Printf("update dispatch %s: %v", dispatchID, err)
	}
}

// sendWave delivers wave. Each slot is one live sender and one address.
// A single message stays on this goroutine. A larger wave leaves together.
// The messages were already claimed, so two sends cannot take the same address.
func (s *Service) sendWave(ctx context.Context, store CampaignStore, prog *dispatchProgress, service, campaignID, dispatchID string, wave []renderedMail, primary MailConfig, extras []SMTPAccount, accounts []MailConfig, send MailSender) {
	// A login rejection is the sender's fault. Try the next live sender for
	// that same address, and leave the rejected sender out for the rest of the run.
	deliver := func(offset int) {
		if offset >= len(accounts) || offset >= len(wave) {
			return
		}

		slot := senderSlot(primary, extras, accounts[offset])
		account := accounts[offset]
		tries := 1 + len(extras)

		for try := 0; try < tries; try++ {
			if prog.senderDead(account) {
				next, ok := nextLiveAccount(primary, extras, slot+1, prog.senderDead)
				if !ok {
					prog.setHalt(errSenderLogin)

					return
				}

				slot++
				account = next

				continue
			}

			sendErr := s.deliver(ctx, &account, service, &OutgoingMail{To: wave[offset].To, Subject: wave[offset].Subject, Body: wave[offset].Body}, send)
			if isSenderLoginFailure(sendErr) {
				if permanentSenderLoginFailure(sendErr) {
					prog.killSender(account)
				}

				s.recordSenderResult(ctx, store, campaignID, wave[offset].To, MailStatusFailed, sendErr.Error(), account.FromEmail)

				next, ok := nextLiveAccount(primary, extras, slot+1, prog.senderDead)
				if !ok {
					prog.setHalt(fmt.Errorf("%w: %s", errSenderLogin, sendErr.Error()))

					return
				}

				slot++
				account = next

				continue
			}

			s.noteDelivery(ctx, store, prog, campaignID, dispatchID, &wave[offset], account.FromEmail, sendErr)

			return
		}

		prog.setHalt(errSenderLogin)
	}

	if len(wave) < 2 {
		if len(wave) == 1 {
			deliver(0)
		}

		return
	}

	var wg sync.WaitGroup

	wg.Add(len(wave))

	for i := range wave {
		go func(offset int) {
			defer wg.Done()

			deliver(offset)
		}(i)
	}

	wg.Wait()
}

// noteDelivery records one result. A provider block or a lookup failure stops
// the run without recording that address. recordMail stays outside the lock.
func (s *Service) noteDelivery(ctx context.Context, store CampaignStore, prog *dispatchProgress, campaignID, dispatchID string, msg *renderedMail, fromEmail string, sendErr error) {
	var optedOut ErrUnsubscribed

	if errors.As(sendErr, &optedOut) {
		prog.mu.Lock()
		prog.unsubscribed++
		prog.mu.Unlock()

		s.recordMail(ctx, store, campaignID, msg.To, MailStatusUnsubscribed, optedOut.Error(), fromEmail)

		return
	}

	if suspendsSend(sendErr) || isProviderBlock(sendErr) {
		halt := sendErr
		if isProviderBlock(sendErr) {
			halt = fmt.Errorf("%w: %s", errProviderBlock, sendErr.Error())
		}

		prog.mu.Lock()
		first := prog.halt == nil

		if first {
			prog.halt = halt
		}

		prog.mu.Unlock()

		if first {
			log.Printf("dispatch %s stopped: %v", dispatchID, halt)
		}

		return
	}

	status := MailStatusSent
	errMsg := ""

	prog.mu.Lock()

	if sendErr != nil {
		prog.failed++
		status = MailStatusFailed
		errMsg = sendErr.Error()

		if prog.firstErr == "" {
			prog.firstErr = msg.To + ": " + errMsg
		}
	} else {
		prog.sent++
	}

	prog.mu.Unlock()

	s.recordMail(ctx, store, campaignID, msg.To, status, errMsg, fromEmail)
}

// errProviderBlock stops a run when the SMTP account itself is refused.
var errProviderBlock = errors.New("email provider blocked this account for unusual sending activity")

// errSenderLogin stops a run only when every registered sender rejects the login.
var errSenderLogin = errors.New("the SMTP server rejected the login")

// suspendsSend reports errors that stop a send without trying the provider.
func suspendsSend(err error) bool {
	return errors.Is(err, ErrOptOutUnavailable) || errors.Is(err, ErrInvalidService) || errors.Is(err, errUnsubscribeLink)
}

// isProviderBlock reports an SMTP refusal of the sending account, such as Zoho 5.4.6.
// isSenderLoginFailure reports a refusal of the mailbox password. The address
// itself was not rejected, so another sender can take it.
func isSenderLoginFailure(err error) bool {
	if err == nil {
		return false
	}

	text := strings.ToLower(err.Error())

	return strings.Contains(text, "authentication failed") ||
		strings.Contains(text, "authentication failure") ||
		strings.Contains(text, "5.7.8") ||
		strings.Contains(text, "535 ") ||
		strings.Contains(text, "4.7.0")
}

// permanentSenderLoginFailure is a password refusal. A temporary authentication
// failure keeps the sender in the rotation for a later message.
func permanentSenderLoginFailure(err error) bool {
	if err == nil {
		return false
	}

	text := strings.ToLower(err.Error())

	return strings.Contains(text, "authentication failed") ||
		strings.Contains(text, "5.7.8") ||
		strings.Contains(text, "535 ")
}

func isProviderBlock(err error) bool {
	if err == nil {
		return false
	}

	return providerBlockText(err.Error())
}

func providerBlockText(s string) bool {
	text := strings.ToLower(s)

	return strings.Contains(text, "unusual sending activity") ||
		strings.Contains(text, "5.4.6") ||
		strings.Contains(text, "unblockme")
}

// providerBlockCooling reports whether automatic sending should wait. A manual
// send does not call this.
func (s *Service) providerBlockCooling(ctx context.Context, campaignID string) bool {
	store, err := s.campaignStore()
	if err != nil {
		return false
	}

	list, err := store.ListDispatches(ctx, campaignID, 1)
	if err != nil || len(list) == 0 {
		return false
	}

	last := list[0]
	if last.Status == DispatchRunning || last.FinishedAt.IsZero() {
		return false
	}

	if !providerBlockText(last.Detail) {
		return false
	}

	return time.Since(last.FinishedAt) < providerBlockCooldown
}

func (s *Service) recordMail(ctx context.Context, store CampaignStore, campaignID, email, status, errMsg, fromEmail string) {
	err := store.RecordMailFrom(ctx, campaignID, strings.ToLower(email), status, errMsg, fromEmail)
	if err != nil {
		log.Printf("record mail result for campaign %s: %v", campaignID, err) //nolint:gosec // G706: campaignID is a parsed UUID. The error is the local write failure.
	}
}

func (s *Service) recordSenderResult(ctx context.Context, store CampaignStore, campaignID, email, status, errMsg, fromEmail string) {
	err := store.RecordSenderResult(ctx, campaignID, strings.ToLower(email), status, errMsg, fromEmail)
	if err != nil {
		log.Printf("record sender result for campaign %s: %v", campaignID, err) //nolint:gosec // G706: campaignID is a parsed UUID. The error is the local write failure.
	}
}

// SenderLog is every recorded attempt for the campaign, newest first.
func (s *Service) SenderLog(ctx context.Context, campaignID string) ([]MailLogEntry, error) {
	store, err := s.campaignStore()
	if err != nil {
		return nil, err
	}

	return store.SenderLog(ctx, campaignID)
}

// nextPause is the shared wait after a wave. together is how many left at once.
// batchGap is set when the wave crossed a multiple of mailBatchSize.
func (s *Service) nextPause(sent, failed, skipped, together int, batchGap bool) (wait time.Duration, detail string) {
	why := "before the next message"
	wait = s.paceWait(mailMinWait, mailMaxWait)

	if together > 1 {
		why = "before the next emails"
	}

	if batchGap {
		why = "before the next batch"
		wait = s.paceWait(mailBatchMinGap, mailBatchMaxGap)
	}

	detail = fmt.Sprintf(
		"Sent %d, failed %d, skipped %d. Waiting %s %s.",
		sent, failed, skipped, wait.Round(time.Second), why,
	)

	return wait, detail
}

// estimateTimeLeft guesses how long the rest of a dispatch takes. wait is the
// pause about to start, done how many messages were claimed so far, total the
// queue size and senders how many emails go out per wave. Pauses count at the
// middle of their random range; the send itself is not counted.
func estimateTimeLeft(wait time.Duration, done, total, senders int) time.Duration {
	remaining := total - done
	if remaining <= 0 {
		return 0
	}

	senders = max(senders, 1)
	waves := (remaining + senders - 1) / senders
	pauses := waves - 1 // nothing waits after the last wave

	// A wave that crosses a multiple of mailBatchSize waits the batch gap.
	gaps := min(max((total-1)/mailBatchSize-done/mailBatchSize, 0), pauses)

	avgWait := (mailMinWait + mailMaxWait) / 2
	avgGap := (mailBatchMinGap + mailBatchMaxGap) / 2

	return wait + time.Duration(pauses-gaps)*avgWait + time.Duration(gaps)*avgGap
}

// timeLeftLabel is the progress text for estimateTimeLeft, e.g.
// "About 1h 20m left for 95 emails."
func timeLeftLabel(wait time.Duration, done, total, senders int) string {
	left := estimateTimeLeft(wait, done, total, senders)
	remaining := max(total-done, 0)

	return fmt.Sprintf("About %s left for %d emails.", roughDuration(left), remaining)
}

// roughDuration formats d to the minute: "under a minute", "45m", "3h 05m".
func roughDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	if d < time.Minute {
		return "under a minute"
	}

	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)

	if h == 0 {
		return fmt.Sprintf("%dm", m)
	}

	return fmt.Sprintf("%dh %02dm", h, m)
}

func (s *Service) writeDispatchProgress(ctx context.Context, store CampaignStore, dispatchID string, sent, failed, skipped int, detail string) error {
	return store.UpdateDispatch(ctx, &Dispatch{
		ID:      dispatchID,
		Status:  DispatchRunning,
		Sent:    sent,
		Failed:  failed,
		Skipped: skipped,
		Detail:  trimDetail(detail),
	})
}

func (s *Service) paceWait(lo, hi time.Duration) time.Duration {
	if s.dispatchWait != nil {
		return s.dispatchWait(lo, hi)
	}

	return randomBetween(lo, hi)
}

func (s *Service) paceSleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}

	if s.dispatchSleep != nil {
		s.dispatchSleep(ctx, d)

		return
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func randomBetween(lo, hi time.Duration) time.Duration {
	if hi < lo {
		lo, hi = hi, lo
	}

	if lo == hi {
		return lo
	}

	span := int64(hi - lo)

	return lo + time.Duration(rand.Int64N(span+1)) //nolint:gosec // G404: the pause jitter is not a secret.
}

// uniqueRecipients keeps the first copy of each address. Later copies are not sent.
func uniqueRecipients(msgs []renderedMail) []renderedMail {
	seen := make(map[string]struct{}, len(msgs))
	out := make([]renderedMail, 0, len(msgs))

	for i := range msgs {
		key := strings.ToLower(strings.TrimSpace(msgs[i].To))
		if key == "" {
			continue
		}

		if _, ok := seen[key]; ok {
			continue
		}

		seen[key] = struct{}{}
		out = append(out, msgs[i])
	}

	return out
}

func trimDetail(s string) string {
	const maxLen = 500
	if len(s) <= maxLen {
		return s
	}

	return s[:maxLen]
}

func (s *Service) collectRecipients(ctx context.Context, store CampaignStore, campaignID string) ([]Recipient, int, error) {
	ids, err := store.CampaignJobs(ctx, campaignID)
	if err != nil {
		return nil, 0, err
	}

	var recs []Recipient

	missing := 0
	seen := map[string]struct{}{}

	for _, id := range ids {
		job, err := s.repo.Get(ctx, id)
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) {
			continue
		}

		if err != nil {
			return nil, 0, err
		}

		parsed, absent, err := s.recipientsForJob(job.ID, job.Name)
		if err != nil {
			return nil, 0, err
		}

		if absent {
			missing++

			continue
		}

		for i := range parsed {
			key := strings.ToLower(parsed[i].Email)
			if _, ok := seen[key]; ok {
				continue
			}

			seen[key] = struct{}{}

			recs = append(recs, parsed[i])
		}
	}

	return recs, missing, nil
}

func (s *Service) recipientsForJob(id, name string) ([]Recipient, bool, error) {
	path, err := s.csvPath(id)
	if err != nil {
		return nil, false, err
	}

	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true, nil
	}

	if err != nil {
		return nil, false, err
	}

	defer func() {
		_ = f.Close()
	}()

	recs, err := recipientsFromCSV(id, name, f)
	if err != nil {
		return nil, false, fmt.Errorf("read results for %s: %w", name, err)
	}

	return recs, false, nil
}

func (s *Service) attachCampaigns(ctx context.Context, jobs []Job) error {
	store, ok := s.repo.(CampaignStore)
	if !ok || store == nil || len(jobs) == 0 {
		return nil
	}

	ids := make([]string, len(jobs))
	for i := range jobs {
		ids[i] = jobs[i].ID
	}

	refs, err := store.CampaignsForJobs(ctx, ids)
	if err != nil {
		return err
	}

	for i := range jobs {
		ref, found := refs[jobs[i].ID]
		if !found {
			continue
		}

		jobs[i].CampaignID = ref.ID
		jobs[i].CampaignName = ref.Name
	}

	return nil
}
