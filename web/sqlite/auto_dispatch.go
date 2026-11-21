package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gosom/google-maps-scraper/web"
)

// defaultResendAfterDays matches the column default.
const defaultResendAfterDays = 4

// GetAutoDispatch returns a campaign's automatic sending setting. None means disabled.
func (repo *repo) GetAutoDispatch(ctx context.Context, campaignID string) (web.AutoDispatch, error) {
	a := web.AutoDispatch{ResendAfterDays: defaultResendAfterDays}

	err := repo.db.QueryRowContext(ctx, `
		SELECT enabled, resend, resend_after_days, parallel FROM campaign_auto WHERE campaign_id = ?
	`, campaignID).Scan(&a.Enabled, &a.Resend, &a.ResendAfterDays, &a.AtOnce)
	if errors.Is(err, sql.ErrNoRows) {
		return a, nil
	}

	return a, err
}

// SaveAutoDispatch stores a campaign's automatic sending setting.
func (repo *repo) SaveAutoDispatch(ctx context.Context, campaignID string, a web.AutoDispatch) error {
	_, err := repo.db.ExecContext(ctx, `
		INSERT INTO campaign_auto (campaign_id, enabled, resend, resend_after_days, parallel, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(campaign_id) DO UPDATE SET
			enabled = excluded.enabled,
			resend = excluded.resend,
			resend_after_days = excluded.resend_after_days,
			parallel = excluded.parallel,
			updated_at = excluded.updated_at
	`, campaignID, a.Enabled, a.Resend, a.ResendAfterDays, a.AtOnce, time.Now().UTC().Unix())

	return err
}

// EnabledAutoDispatch returns the settings of campaigns with automatic sending on.
func (repo *repo) EnabledAutoDispatch(ctx context.Context) (map[string]web.AutoDispatch, error) {
	rows, err := repo.db.QueryContext(ctx, `
		SELECT campaign_id, resend, resend_after_days, parallel FROM campaign_auto WHERE enabled = 1
	`)
	if err != nil {
		return nil, err
	}

	defer func() {
		_ = rows.Close()
	}()

	out := make(map[string]web.AutoDispatch)

	for rows.Next() {
		var (
			id string
			a  = web.AutoDispatch{Enabled: true}
		)

		if err := rows.Scan(&id, &a.Resend, &a.ResendAfterDays, &a.AtOnce); err != nil {
			return nil, err
		}

		out[id] = a
	}

	return out, rows.Err()
}
