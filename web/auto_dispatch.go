package web

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"
)

// AutoDispatchInterval is how often campaigns with automatic sending are checked.
const AutoDispatchInterval = time.Minute

// defaultAutoResendDays is the default wait before an automatic resend.
const defaultAutoResendDays = 4

// AutoDispatch is a campaign's automatic sending setting. When enabled, new
// addresses found by the campaign's jobs are queued and sent without a click,
// with the same pacing, unsubscribe check and duplicate protection as a manual send.
type AutoDispatch struct {
	Enabled bool
	// Resend also sends again to addresses whose last send is older than ResendAfterDays.
	Resend          bool
	ResendAfterDays int
}

// ResendAfter is ResendAfterDays as a duration.
func (a AutoDispatch) ResendAfter() time.Duration {
	return time.Duration(max(a.ResendAfterDays, 0)) * 24 * time.Hour
}

// AutoDispatchStore keeps the automatic sending setting per campaign.
type AutoDispatchStore interface {
	// GetAutoDispatch returns the setting; a campaign without one is disabled.
	GetAutoDispatch(ctx context.Context, campaignID string) (AutoDispatch, error)
	SaveAutoDispatch(ctx context.Context, campaignID string, a AutoDispatch) error
	// EnabledAutoDispatch returns the settings of campaigns with automatic sending on.
	EnabledAutoDispatch(ctx context.Context) (map[string]AutoDispatch, error)
}

func (s *Service) autoStore() (AutoDispatchStore, error) {
	store, ok := s.repo.(AutoDispatchStore)
	if !ok || store == nil {
		return nil, errCampaignsUnavailable
	}

	return store, nil
}

// GetAutoDispatch returns a campaign's automatic sending setting.
func (s *Service) GetAutoDispatch(ctx context.Context, campaignID string) (AutoDispatch, error) {
	store, err := s.autoStore()
	if err != nil {
		return AutoDispatch{ResendAfterDays: defaultAutoResendDays}, err
	}

	a, err := store.GetAutoDispatch(ctx, campaignID)
	if err != nil {
		return AutoDispatch{}, err
	}

	return a, nil
}

// SaveAutoDispatch stores a campaign's automatic sending setting.
// Turning it on needs a campaign whose email is ready to send.
func (s *Service) SaveAutoDispatch(ctx context.Context, campaignID string, a AutoDispatch) error {
	if a.ResendAfterDays < 0 {
		return errors.New("resend interval must be 0 days or more")
	}

	store, err := s.autoStore()
	if err != nil {
		return err
	}

	if a.Enabled {
		cs, err := s.campaignStore()
		if err != nil {
			return err
		}

		if _, err := s.mailReady(ctx, cs, campaignID); err != nil {
			return err
		}
	}

	return store.SaveAutoDispatch(ctx, campaignID, a)
}

// RunAutoDispatch checks every interval for campaigns with automatic sending on
// and starts a send when they have addresses waiting. It stops when ctx ends.
func (s *Service) RunAutoDispatch(ctx context.Context, send MailSender, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		s.AutoDispatchOnce(ctx, send)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// AutoDispatchOnce runs one round: every campaign with automatic sending on and
// addresses waiting gets a send started. Campaigns already sending are skipped.
func (s *Service) AutoDispatchOnce(ctx context.Context, send MailSender) {
	store, err := s.autoStore()
	if err != nil {
		return
	}

	enabled, err := store.EnabledAutoDispatch(ctx)
	if err != nil {
		log.Printf("auto dispatch: %v", err)

		return
	}

	for id, a := range enabled {
		if ctx.Err() != nil {
			return
		}

		if s.dispatchInProcess(id) {
			continue
		}

		if s.providerBlockCooling(ctx, id) {
			log.Printf("auto dispatch %s: waiting after a provider block", id)

			continue
		}

		res, err := s.startDispatch(ctx, id, DispatchOptions{
			Resend:      a.Resend,
			ResendAfter: a.ResendAfter(),
			SkipFailed:  true,
			Quiet:       true,
		}, send)

		switch {
		case errors.Is(err, ErrDispatchRunning):
		case err != nil:
			log.Printf("auto dispatch %s: %v", id, err)
		case res.Started:
			log.Printf("auto dispatch %s: started", id)
		}
	}
}

// dropFailed removes addresses whose last send failed, so an automatic send
// does not retry a bad address every round. A manual send still retries them.
func dropFailed(ctx context.Context, store CampaignStore, campaignID string, recs []Recipient) ([]Recipient, error) {
	return filterFailed(ctx, store, campaignID, recs, false)
}

// keepFailed keeps only the addresses whose last send failed, once each.
func keepFailed(ctx context.Context, store CampaignStore, campaignID string, recs []Recipient) ([]Recipient, error) {
	return filterFailed(ctx, store, campaignID, recs, true)
}

func filterFailed(ctx context.Context, store CampaignStore, campaignID string, recs []Recipient, wantFailed bool) ([]Recipient, error) {
	entries, err := store.MailLog(ctx, campaignID)
	if err != nil {
		return nil, err
	}

	failed := make(map[string]struct{})

	for _, e := range entries {
		if e.Status == MailStatusFailed {
			failed[strings.ToLower(e.Email)] = struct{}{}
		}
	}

	if len(failed) == 0 && !wantFailed {
		return recs, nil
	}

	kept := recs[:0:0]
	seen := make(map[string]struct{})

	for i := range recs {
		email := strings.ToLower(recs[i].Email)

		if _, ok := failed[email]; ok != wantFailed {
			continue
		}

		if wantFailed {
			if _, dup := seen[email]; dup {
				continue
			}

			seen[email] = struct{}{}
		}

		kept = append(kept, recs[i])
	}

	return kept, nil
}
