package web

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ListServices returns the registered sending services.
func (s *Service) ListServices(ctx context.Context) ([]MailService, error) {
	store, ok := s.repo.(CampaignStore)
	if !ok || store == nil {
		return nil, nil
	}

	return store.ListServices(ctx)
}

// CreateService registers a sending service. The name must already exist in the
// website's servicos table, otherwise no unsubscribe would ever match it.
func (s *Service) CreateService(ctx context.Context, svc *MailService) error {
	store, err := s.campaignStore()
	if err != nil {
		return err
	}

	clean, err := cleanService(svc)
	if err != nil {
		return err
	}

	if _, err := store.GetService(ctx, clean.Name); err == nil {
		return ErrServiceExists
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}

	if s.optOut == nil {
		return fmt.Errorf("%w: unsubscribe database is not configured, so the service name cannot be checked", ErrOptOutUnavailable)
	}

	known, err := s.optOut.ServiceExists(ctx, clean.Name)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrOptOutUnavailable, err)
	}

	if !known {
		return fmt.Errorf("service %q is not registered on the unsubscribe site", clean.Name)
	}

	return store.CreateService(ctx, &clean)
}

// UpdateService changes the display label. The name is the key shared with the
// website and cannot change.
func (s *Service) UpdateService(ctx context.Context, svc *MailService) error {
	store, err := s.campaignStore()
	if err != nil {
		return err
	}

	clean, err := cleanService(svc)
	if err != nil {
		return err
	}

	return store.UpdateService(ctx, &clean)
}

// DeleteService removes a service no campaign uses.
func (s *Service) DeleteService(ctx context.Context, name string) error {
	store, err := s.campaignStore()
	if err != nil {
		return err
	}

	return store.DeleteService(ctx, strings.TrimSpace(name))
}

func cleanService(svc *MailService) (MailService, error) {
	out := MailService{
		Name:  strings.TrimSpace(svc.Name),
		Label: strings.TrimSpace(svc.Label),
	}

	if !ValidServiceName(out.Name) {
		return MailService{}, errors.New("service name must be 1 to 64 lowercase letters, digits, dash or underscore")
	}

	if out.Label == "" {
		out.Label = out.Name
	}

	if len(out.Label) > maxServiceLabel {
		return MailService{}, errors.New("service label is too long")
	}

	return out, nil
}
