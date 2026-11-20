package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// ErrNoDispatch is returned when pausing or resuming a campaign that is not sending.
var ErrNoDispatch = errors.New("no dispatch is running for this campaign")

// dispatchControl lets the page pause a running dispatch and hand it new SMTP
// settings. The run checks it before every message.
type dispatchControl struct {
	mu     sync.Mutex
	paused bool
	// resume is closed when a paused run may continue.
	resume chan struct{}
	// reload asks the run to read the saved SMTP settings again.
	reload bool
	// nextSend is when the run sends its next message; zero while sending.
	nextSend time.Time
}

func (c *dispatchControl) setNextSend(t time.Time) {
	if c == nil {
		return
	}

	c.mu.Lock()
	c.nextSend = t
	c.mu.Unlock()
}

// NextDispatchSend is when the campaign's running dispatch sends its next
// email, for the countdown on the page. It is zero when nothing is waiting.
func (s *Service) NextDispatchSend(campaignID string) time.Time {
	ctl := s.dispatchControlFor(campaignID)
	if ctl == nil {
		return time.Time{}
	}

	ctl.mu.Lock()
	defer ctl.mu.Unlock()

	if ctl.paused {
		return time.Time{}
	}

	return ctl.nextSend
}

// openDispatchControl registers the control of a run that is starting.
func (s *Service) openDispatchControl(campaignID string) *dispatchControl {
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()

	if s.dispatchCtl == nil {
		s.dispatchCtl = map[string]*dispatchControl{}
	}

	ctl := &dispatchControl{}
	s.dispatchCtl[campaignID] = ctl

	return ctl
}

func (s *Service) closeDispatchControl(campaignID string, ctl *dispatchControl) {
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()

	if s.dispatchCtl[campaignID] == ctl {
		delete(s.dispatchCtl, campaignID)
	}
}

func (s *Service) dispatchControlFor(campaignID string) *dispatchControl {
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()

	return s.dispatchCtl[campaignID]
}

// PauseDispatch stops a running dispatch before its next message. Nothing is
// lost: the remaining addresses wait until ResumeDispatch.
func (s *Service) PauseDispatch(campaignID string) error {
	ctl := s.dispatchControlFor(campaignID)
	if ctl == nil {
		return ErrNoDispatch
	}

	ctl.mu.Lock()
	defer ctl.mu.Unlock()

	if !ctl.paused {
		ctl.paused = true
		ctl.resume = make(chan struct{})
	}

	return nil
}

// ResumeDispatch continues a paused dispatch with the saved SMTP settings.
func (s *Service) ResumeDispatch(campaignID string) error {
	ctl := s.dispatchControlFor(campaignID)
	if ctl == nil {
		return ErrNoDispatch
	}

	ctl.mu.Lock()
	defer ctl.mu.Unlock()

	if ctl.paused {
		ctl.paused = false
		ctl.reload = true
		close(ctl.resume)
	}

	return nil
}

// DispatchPaused reports whether the campaign's running dispatch is paused.
func (s *Service) DispatchPaused(campaignID string) bool {
	ctl := s.dispatchControlFor(campaignID)
	if ctl == nil {
		return false
	}

	ctl.mu.Lock()
	defer ctl.mu.Unlock()

	return ctl.paused
}

// mailSettingsChanged tells a running dispatch to use the newly saved settings
// from its next message on.
func (s *Service) mailSettingsChanged(campaignID string) {
	ctl := s.dispatchControlFor(campaignID)
	if ctl == nil {
		return
	}

	ctl.mu.Lock()
	ctl.reload = true
	ctl.mu.Unlock()
}

// dispatchCheckpoint runs before each message: it waits while the run is
// paused and applies SMTP settings saved since the last message.
func (s *Service) dispatchCheckpoint(ctx context.Context, store CampaignStore, ctl *dispatchControl, campaignID, dispatchID string, cfg *MailConfig, sent, failed, skipped int) {
	if ctl == nil {
		return
	}

	ctl.mu.Lock()
	paused, resume := ctl.paused, ctl.resume
	ctl.mu.Unlock()

	if paused {
		detail := fmt.Sprintf("Paused. Sent %d, failed %d, skipped %d.", sent, failed, skipped)
		if err := s.writeDispatchProgress(ctx, store, dispatchID, sent, failed, skipped, detail); err != nil {
			log.Printf("update dispatch %s: %v", dispatchID, err)
		}

		<-resume
	}

	ctl.mu.Lock()
	reload := ctl.reload
	ctl.reload = false
	ctl.mu.Unlock()

	if reload {
		s.reloadTransport(ctx, store, campaignID, cfg)
	}
}

// reloadTransport copies the saved server, login and sender into cfg. Subject
// and body are not changed: the messages of this run are already rendered.
// Settings that are not complete are ignored and the run keeps the old ones.
func (s *Service) reloadTransport(ctx context.Context, store CampaignStore, campaignID string, cfg *MailConfig) {
	fresh, ok, err := store.GetMail(ctx, campaignID)
	if err != nil || !ok {
		log.Printf("dispatch %s: reload email settings: %v", campaignID, err) //nolint:gosec // G706: campaignID is a parsed UUID.

		return
	}

	fresh = fresh.Normalize()
	if err := fresh.ReadyToSend(); err != nil {
		log.Printf("dispatch %s: new email settings not used: %v", campaignID, err) //nolint:gosec // G706: campaignID is a parsed UUID.

		return
	}

	cfg.Host = fresh.Host
	cfg.Port = fresh.Port
	cfg.Username = fresh.Username
	cfg.Password = fresh.Password
	cfg.Encryption = fresh.Encryption
	cfg.FromEmail = fresh.FromEmail
	cfg.FromName = fresh.FromName
}
