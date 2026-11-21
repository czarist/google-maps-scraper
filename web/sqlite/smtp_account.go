package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gosom/google-maps-scraper/web"
)

func ensureSMTPAccountSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS campaign_smtp (
			id TEXT PRIMARY KEY,
			campaign_id TEXT NOT NULL,
			position INT NOT NULL,
			smtp_host TEXT NOT NULL,
			smtp_port INT NOT NULL,
			smtp_username TEXT NOT NULL DEFAULT '',
			smtp_password TEXT NOT NULL DEFAULT '',
			encryption TEXT NOT NULL DEFAULT 'starttls',
			from_email TEXT NOT NULL,
			from_name TEXT NOT NULL DEFAULT '',
			created_at INT NOT NULL,
			FOREIGN KEY (campaign_id) REFERENCES campaigns(id) ON DELETE CASCADE
		)
	`)
	if err != nil {
		return err
	}

	_, err = db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_campaign_smtp_campaign
			ON campaign_smtp (campaign_id, position)
	`)

	return err
}

// ListSMTPAccounts returns a campaign's extra SMTP servers in send order.
func (repo *repo) ListSMTPAccounts(ctx context.Context, campaignID string) ([]web.SMTPAccount, error) {
	rows, err := repo.db.QueryContext(ctx, `
		SELECT id, smtp_host, smtp_port, smtp_username, smtp_password,
			encryption, from_email, from_name
		FROM campaign_smtp
		WHERE campaign_id = ?
		ORDER BY position, created_at
	`, campaignID)
	if err != nil {
		return nil, err
	}

	defer func() {
		_ = rows.Close()
	}()

	var out []web.SMTPAccount

	for rows.Next() {
		var a web.SMTPAccount

		if err := rows.Scan(&a.ID, &a.Host, &a.Port, &a.Username, &a.Password, &a.Encryption, &a.FromEmail, &a.FromName); err != nil {
			return nil, err
		}

		out = append(out, a)
	}

	return out, rows.Err()
}

// InsertSMTPAccount appends an extra SMTP server at the end of the send order.
func (repo *repo) InsertSMTPAccount(ctx context.Context, campaignID string, a *web.SMTPAccount) error {
	tx, err := repo.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	defer func() {
		_ = tx.Rollback()
	}()

	var position int

	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(position), 0) + 1 FROM campaign_smtp WHERE campaign_id = ?
	`, campaignID).Scan(&position)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO campaign_smtp (
			id, campaign_id, position, smtp_host, smtp_port, smtp_username, smtp_password,
			encryption, from_email, from_name, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, a.ID, campaignID, position, a.Host, a.Port, a.Username, a.Password,
		string(a.Encryption), a.FromEmail, a.FromName, time.Now().UTC().Unix())
	if err != nil {
		return err
	}

	return tx.Commit()
}

// UpdateSMTPAccount replaces one extra server. The position stays.
func (repo *repo) UpdateSMTPAccount(ctx context.Context, campaignID string, a *web.SMTPAccount) error {
	res, err := repo.db.ExecContext(ctx, `
		UPDATE campaign_smtp SET
			smtp_host = ?, smtp_port = ?, smtp_username = ?, smtp_password = ?,
			encryption = ?, from_email = ?, from_name = ?
		WHERE campaign_id = ? AND id = ?
	`, a.Host, a.Port, a.Username, a.Password, string(a.Encryption), a.FromEmail, a.FromName, campaignID, a.ID)
	if err != nil {
		return err
	}

	n, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if n == 0 {
		return errors.New("SMTP server not found")
	}

	return nil
}

// DeleteSMTPAccount removes one extra server of a campaign.
func (repo *repo) DeleteSMTPAccount(ctx context.Context, campaignID, accountID string) error {
	res, err := repo.db.ExecContext(ctx, `
		DELETE FROM campaign_smtp WHERE campaign_id = ? AND id = ?
	`, campaignID, accountID)
	if err != nil {
		return err
	}

	n, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if n == 0 {
		return errors.New("SMTP server not found")
	}

	return nil
}
