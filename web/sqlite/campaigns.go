package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gosom/google-maps-scraper/web"
)

func ensureCampaignSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS campaigns (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			created_at INT NOT NULL,
			updated_at INT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS campaign_jobs (
			campaign_id TEXT NOT NULL,
			job_id TEXT NOT NULL,
			created_at INT NOT NULL,
			PRIMARY KEY (campaign_id, job_id),
			FOREIGN KEY (campaign_id) REFERENCES campaigns(id) ON DELETE CASCADE,
			FOREIGN KEY (job_id) REFERENCES jobs(id) ON DELETE CASCADE
		);

		CREATE UNIQUE INDEX IF NOT EXISTS idx_campaign_jobs_job
			ON campaign_jobs (job_id);

		CREATE TABLE IF NOT EXISTS campaign_mail (
			campaign_id TEXT PRIMARY KEY,
			smtp_host TEXT NOT NULL DEFAULT '',
			smtp_port INT NOT NULL DEFAULT 587,
			smtp_username TEXT NOT NULL DEFAULT '',
			smtp_password TEXT NOT NULL DEFAULT '',
			from_email TEXT NOT NULL DEFAULT '',
			from_name TEXT NOT NULL DEFAULT '',
			subject TEXT NOT NULL DEFAULT '',
			body TEXT NOT NULL DEFAULT '',
			encryption TEXT NOT NULL DEFAULT 'starttls',
			format TEXT NOT NULL DEFAULT 'plain',
			updated_at INT NOT NULL,
			FOREIGN KEY (campaign_id) REFERENCES campaigns(id) ON DELETE CASCADE
		);

		CREATE TABLE IF NOT EXISTS campaign_mail_log (
			campaign_id TEXT NOT NULL,
			email TEXT NOT NULL,
			status TEXT NOT NULL,
			error TEXT NOT NULL DEFAULT '',
			sent_at INT NOT NULL,
			PRIMARY KEY (campaign_id, email),
			FOREIGN KEY (campaign_id) REFERENCES campaigns(id) ON DELETE CASCADE
		);

		CREATE TABLE IF NOT EXISTS campaign_dispatches (
			id TEXT PRIMARY KEY,
			campaign_id TEXT NOT NULL,
			status TEXT NOT NULL,
			started_at INT NOT NULL,
			finished_at INT NOT NULL DEFAULT 0,
			sent INT NOT NULL DEFAULT 0,
			failed INT NOT NULL DEFAULT 0,
			skipped INT NOT NULL DEFAULT 0,
			detail TEXT NOT NULL DEFAULT '',
			FOREIGN KEY (campaign_id) REFERENCES campaigns(id) ON DELETE CASCADE
		);

		CREATE INDEX IF NOT EXISTS idx_campaign_dispatches_campaign
			ON campaign_dispatches (campaign_id, started_at);

		CREATE TABLE IF NOT EXISTS campaign_auto (
			campaign_id TEXT PRIMARY KEY,
			enabled INT NOT NULL DEFAULT 0,
			resend INT NOT NULL DEFAULT 0,
			resend_after_days INT NOT NULL DEFAULT 4,
			updated_at INT NOT NULL,
			FOREIGN KEY (campaign_id) REFERENCES campaigns(id) ON DELETE CASCADE
		);
	`)
	if err != nil {
		return err
	}

	if err := addMailFormatColumn(db); err != nil {
		return err
	}

	return ensureServiceSchema(db)
}

// ensureServiceSchema adds the sending services and the campaign service column.
// Campaigns created before services existed become trentin.
func ensureServiceSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS mail_services (
			name TEXT PRIMARY KEY,
			label TEXT NOT NULL,
			created_at INT NOT NULL
		)
	`)
	if err != nil {
		return err
	}

	now := time.Now().UTC().Unix()

	for _, svc := range web.DefaultServices {
		_, err := db.Exec(`INSERT OR IGNORE INTO mail_services (name, label, created_at) VALUES (?, ?, ?)`,
			svc.Name, svc.Label, now)
		if err != nil {
			return err
		}
	}

	_, err = db.Exec(`ALTER TABLE campaigns ADD COLUMN servico TEXT NOT NULL DEFAULT '` + web.ServiceTrentin + `'`)
	if err == nil || strings.Contains(err.Error(), "duplicate column") {
		return nil
	}

	return err
}

// addMailFormatColumn upgrades databases created before HTML bodies existed.
func addMailFormatColumn(db *sql.DB) error {
	_, err := db.Exec(`ALTER TABLE campaign_mail ADD COLUMN format TEXT NOT NULL DEFAULT 'plain'`)
	if err == nil || strings.Contains(err.Error(), "duplicate column") {
		return nil
	}

	return err
}

func (repo *repo) CreateCampaign(ctx context.Context, c *web.Campaign) error {
	created := c.CreatedAt.UTC().Unix()
	_, err := repo.db.ExecContext(ctx, `
		INSERT INTO campaigns (id, name, servico, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
	`, c.ID, strings.TrimSpace(c.Name), serviceOrDefault(c.ServiceName), created, created)

	return err
}

func (repo *repo) ListCampaigns(ctx context.Context) ([]web.CampaignSummary, error) {
	rows, err := repo.db.QueryContext(ctx, `
		SELECT
			c.id,
			c.name,
			c.servico,
			c.created_at,
			(SELECT COUNT(*) FROM campaign_jobs j WHERE j.campaign_id = c.id),
			CASE
				WHEN m.smtp_host != '' AND m.from_email != '' AND m.subject != ''
					AND m.body != '' AND m.smtp_password != '' THEN 1
				ELSE 0
			END,
			COALESCE((
				SELECT d.status FROM campaign_dispatches d
				WHERE d.campaign_id = c.id
				ORDER BY d.started_at DESC
				LIMIT 1
			), '')
		FROM campaigns c
		LEFT JOIN campaign_mail m ON m.campaign_id = c.id
		ORDER BY c.created_at DESC, c.id DESC
	`)
	if err != nil {
		return nil, err
	}

	defer func() {
		_ = rows.Close()
	}()

	var out []web.CampaignSummary

	for rows.Next() {
		var (
			item    web.CampaignSummary
			created int64
			ready   int
		)

		if err := rows.Scan(&item.ID, &item.Name, &item.ServiceName, &created, &item.JobCount, &ready, &item.LastStatus); err != nil {
			return nil, err
		}

		item.CreatedAt = time.Unix(created, 0).UTC()
		item.MailReady = ready == 1
		out = append(out, item)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return out, nil
}

func (repo *repo) GetCampaign(ctx context.Context, id string) (web.Campaign, error) {
	var (
		c       web.Campaign
		created int64
	)

	err := repo.db.QueryRowContext(ctx, `
		SELECT id, name, servico, created_at FROM campaigns WHERE id = ?
	`, id).Scan(&c.ID, &c.Name, &c.ServiceName, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return web.Campaign{}, web.ErrNotFound
	}

	if err != nil {
		return web.Campaign{}, err
	}

	c.CreatedAt = time.Unix(created, 0).UTC()

	return c, nil
}

func (repo *repo) RenameCampaign(ctx context.Context, id, name string) error {
	res, err := repo.db.ExecContext(ctx, `
		UPDATE campaigns SET name = ?, updated_at = ? WHERE id = ?
	`, strings.TrimSpace(name), time.Now().UTC().Unix(), id)
	if err != nil {
		return err
	}

	return rowsAffected(res, web.ErrNotFound)
}

func (repo *repo) DeleteCampaign(ctx context.Context, id string) error {
	tx, err := repo.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	committed := false

	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	for _, q := range []string{
		`DELETE FROM campaign_mail_log WHERE campaign_id = ?`,
		`DELETE FROM campaign_dispatches WHERE campaign_id = ?`,
		`DELETE FROM campaign_mail WHERE campaign_id = ?`,
		`DELETE FROM campaign_jobs WHERE campaign_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM campaigns WHERE id = ?`, id)
	if err != nil {
		return err
	}

	if err := rowsAffected(res, web.ErrNotFound); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	committed = true

	return nil
}

func (repo *repo) AssignJob(ctx context.Context, campaignID, jobID string) error {
	tx, err := repo.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	committed := false

	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := requireRow(ctx, tx, `SELECT 1 FROM campaigns WHERE id = ?`, campaignID, web.ErrNotFound); err != nil {
		return err
	}

	if err := requireRow(ctx, tx, `SELECT 1 FROM jobs WHERE id = ?`, jobID, web.ErrJobNotFound); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM campaign_jobs WHERE job_id = ?`, jobID); err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO campaign_jobs (campaign_id, job_id, created_at) VALUES (?, ?, ?)
	`, campaignID, jobID, time.Now().UTC().Unix())
	if err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	committed = true

	return nil
}

func (repo *repo) UnassignJob(ctx context.Context, campaignID, jobID string) error {
	_, err := repo.db.ExecContext(ctx, `
		DELETE FROM campaign_jobs WHERE campaign_id = ? AND job_id = ?
	`, campaignID, jobID)

	return err
}

func (repo *repo) CampaignJobs(ctx context.Context, campaignID string) ([]string, error) {
	rows, err := repo.db.QueryContext(ctx, `
		SELECT job_id FROM campaign_jobs
		WHERE campaign_id = ?
		ORDER BY created_at DESC, job_id DESC
	`, campaignID)
	if err != nil {
		return nil, err
	}

	defer func() {
		_ = rows.Close()
	}()

	var ids []string

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}

		ids = append(ids, id)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return ids, nil
}

func (repo *repo) CampaignsForJobs(ctx context.Context, jobIDs []string) (map[string]web.CampaignRef, error) {
	out := make(map[string]web.CampaignRef)
	if len(jobIDs) == 0 {
		return out, nil
	}

	const chunk = 500

	for start := 0; start < len(jobIDs); start += chunk {
		end := start + chunk
		if end > len(jobIDs) {
			end = len(jobIDs)
		}

		if err := repo.campaignsForJobChunk(ctx, jobIDs[start:end], out); err != nil {
			return nil, err
		}
	}

	return out, nil
}

func (repo *repo) campaignsForJobChunk(ctx context.Context, jobIDs []string, out map[string]web.CampaignRef) error {
	placeholders := make([]string, len(jobIDs))
	args := make([]any, len(jobIDs))

	for i, id := range jobIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	//nolint:gosec // G201: the format string only inserts "?" placeholders. Values stay in args.
	q := fmt.Sprintf(`
		SELECT cj.job_id, c.id, c.name
		FROM campaign_jobs cj
		JOIN campaigns c ON c.id = cj.campaign_id
		WHERE cj.job_id IN (%s)
	`, strings.Join(placeholders, ","))

	rows, err := repo.db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}

	defer func() {
		_ = rows.Close()
	}()

	for rows.Next() {
		var jobID string

		var ref web.CampaignRef

		if err := rows.Scan(&jobID, &ref.ID, &ref.Name); err != nil {
			return err
		}

		out[jobID] = ref
	}

	return rows.Err()
}

func (repo *repo) GetMail(ctx context.Context, campaignID string) (web.MailConfig, bool, error) {
	var cfg web.MailConfig

	err := repo.db.QueryRowContext(ctx, `
		SELECT smtp_host, smtp_port, smtp_username, smtp_password,
			from_email, from_name, subject, body, encryption, format
		FROM campaign_mail WHERE campaign_id = ?
	`, campaignID).Scan(
		&cfg.Host, &cfg.Port, &cfg.Username, &cfg.Password,
		&cfg.FromEmail, &cfg.FromName, &cfg.Subject, &cfg.Body, &cfg.Encryption, &cfg.Format,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return web.MailConfig{}, false, nil
	}

	if err != nil {
		return web.MailConfig{}, false, err
	}

	return cfg, true, nil
}

func (repo *repo) SaveMail(ctx context.Context, campaignID string, cfg *web.MailConfig) error {
	_, err := repo.db.ExecContext(ctx, `
		INSERT INTO campaign_mail (
			campaign_id, smtp_host, smtp_port, smtp_username, smtp_password,
			from_email, from_name, subject, body, encryption, format, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(campaign_id) DO UPDATE SET
			smtp_host = excluded.smtp_host,
			smtp_port = excluded.smtp_port,
			smtp_username = excluded.smtp_username,
			smtp_password = excluded.smtp_password,
			from_email = excluded.from_email,
			from_name = excluded.from_name,
			subject = excluded.subject,
			body = excluded.body,
			encryption = excluded.encryption,
			format = excluded.format,
			updated_at = excluded.updated_at
	`, campaignID, cfg.Host, cfg.Port, cfg.Username, cfg.Password,
		cfg.FromEmail, cfg.FromName, cfg.Subject, cfg.Body, string(cfg.Encryption), string(cfg.Format), time.Now().UTC().Unix())

	return err
}

func (repo *repo) SentEmails(ctx context.Context, campaignID string) (map[string]struct{}, error) {
	rows, err := repo.db.QueryContext(ctx, `
		SELECT email FROM campaign_mail_log
		WHERE campaign_id = ? AND status IN (?, ?)
	`, campaignID, web.MailStatusSent, web.MailStatusUnsubscribed)
	if err != nil {
		return nil, err
	}

	return scanEmails(rows)
}

// SentEmailsSince returns addresses sent after since, plus every unsubscribed address.
func (repo *repo) SentEmailsSince(ctx context.Context, campaignID string, since time.Time) (map[string]struct{}, error) {
	rows, err := repo.db.QueryContext(ctx, `
		SELECT email FROM campaign_mail_log
		WHERE campaign_id = ? AND (status = ? OR (status = ? AND sent_at > ?))
	`, campaignID, web.MailStatusUnsubscribed, web.MailStatusSent, since.UTC().Unix())
	if err != nil {
		return nil, err
	}

	return scanEmails(rows)
}

func scanEmails(rows *sql.Rows) (map[string]struct{}, error) {
	defer func() {
		_ = rows.Close()
	}()

	out := make(map[string]struct{})

	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, err
		}

		out[strings.ToLower(email)] = struct{}{}
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return out, nil
}

// MailLog returns the latest send result per address, newest first.
func (repo *repo) MailLog(ctx context.Context, campaignID string) ([]web.MailLogEntry, error) {
	rows, err := repo.db.QueryContext(ctx, `
		SELECT email, status, error, sent_at FROM campaign_mail_log
		WHERE campaign_id = ?
		ORDER BY sent_at DESC, email
	`, campaignID)
	if err != nil {
		return nil, err
	}

	defer func() {
		_ = rows.Close()
	}()

	var out []web.MailLogEntry

	for rows.Next() {
		var (
			e      web.MailLogEntry
			sentAt int64
		)

		if err := rows.Scan(&e.Email, &e.Status, &e.Error, &sentAt); err != nil {
			return nil, err
		}

		e.SentAt = time.Unix(sentAt, 0).UTC()
		out = append(out, e)
	}

	return out, rows.Err()
}

func (repo *repo) RecordMail(ctx context.Context, campaignID, email, status, errMsg string) error {
	_, err := repo.db.ExecContext(ctx, `
		INSERT INTO campaign_mail_log (campaign_id, email, status, error, sent_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(campaign_id, email) DO UPDATE SET
			status = excluded.status,
			error = excluded.error,
			sent_at = excluded.sent_at
	`, campaignID, strings.ToLower(email), status, errMsg, time.Now().UTC().Unix())

	return err
}

// RequeueFailed deletes the failed send rows of a campaign. With no row left,
// those addresses count as not sent and wait for the next dispatch.
func (repo *repo) RequeueFailed(ctx context.Context, campaignID string) (int, error) {
	res, err := repo.db.ExecContext(ctx, `
		DELETE FROM campaign_mail_log WHERE campaign_id = ? AND status = ?
	`, campaignID, web.MailStatusFailed)
	if err != nil {
		return 0, err
	}

	n, err := res.RowsAffected()

	return int(n), err
}

func (repo *repo) InsertDispatch(ctx context.Context, d *web.Dispatch) error {
	_, err := repo.db.ExecContext(ctx, `
		INSERT INTO campaign_dispatches (
			id, campaign_id, status, started_at, finished_at, sent, failed, skipped, detail
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, d.ID, d.CampaignID, d.Status, unixOrZero(d.StartedAt), unixOrZero(d.FinishedAt),
		d.Sent, d.Failed, d.Skipped, d.Detail)

	return err
}

func (repo *repo) UpdateDispatch(ctx context.Context, d *web.Dispatch) error {
	res, err := repo.db.ExecContext(ctx, `
		UPDATE campaign_dispatches
		SET status = ?, finished_at = ?, sent = ?, failed = ?, skipped = ?, detail = ?
		WHERE id = ?
	`, d.Status, unixOrZero(d.FinishedAt), d.Sent, d.Failed, d.Skipped, d.Detail, d.ID)
	if err != nil {
		return err
	}

	return rowsAffected(res, web.ErrNotFound)
}

func (repo *repo) ListDispatches(ctx context.Context, campaignID string, limit int) ([]web.Dispatch, error) {
	if limit < 1 {
		limit = 10
	}

	rows, err := repo.db.QueryContext(ctx, `
		SELECT id, campaign_id, status, started_at, finished_at, sent, failed, skipped, detail
		FROM campaign_dispatches
		WHERE campaign_id = ?
		ORDER BY started_at DESC
		LIMIT ?
	`, campaignID, limit)
	if err != nil {
		return nil, err
	}

	defer func() {
		_ = rows.Close()
	}()

	return scanDispatches(rows)
}

func (repo *repo) LatestRunningDispatch(ctx context.Context, campaignID string) (web.Dispatch, bool, error) {
	rows, err := repo.db.QueryContext(ctx, `
		SELECT id, campaign_id, status, started_at, finished_at, sent, failed, skipped, detail
		FROM campaign_dispatches
		WHERE campaign_id = ? AND status = ?
		ORDER BY started_at DESC
		LIMIT 1
	`, campaignID, web.DispatchRunning)
	if err != nil {
		return web.Dispatch{}, false, err
	}

	defer func() {
		_ = rows.Close()
	}()

	list, err := scanDispatches(rows)
	if err != nil {
		return web.Dispatch{}, false, err
	}

	if len(list) == 0 {
		return web.Dispatch{}, false, nil
	}

	return list[0], true, nil
}

func scanDispatches(rows *sql.Rows) ([]web.Dispatch, error) {
	var out []web.Dispatch

	for rows.Next() {
		var d web.Dispatch

		var started, finished int64

		if err := rows.Scan(
			&d.ID, &d.CampaignID, &d.Status, &started, &finished,
			&d.Sent, &d.Failed, &d.Skipped, &d.Detail,
		); err != nil {
			return nil, err
		}

		d.StartedAt = unixTime(started)
		d.FinishedAt = unixTime(finished)
		out = append(out, d)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return out, nil
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func requireRow(ctx context.Context, q queryRower, query, id string, missing error) error {
	var one int

	err := q.QueryRowContext(ctx, query, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return missing
	}

	return err
}

func rowsAffected(res sql.Result, missing error) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if n == 0 {
		return missing
	}

	return nil
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}

	return t.UTC().Unix()
}

func unixTime(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}

	return time.Unix(sec, 0).UTC()
}

func serviceOrDefault(name string) string {
	if strings.TrimSpace(name) == "" {
		return web.ServiceTrentin
	}

	return name
}

// SetCampaignService changes the campaign service. Addresses skipped as
// unsubscribed under the old service become eligible again, because the
// opt-out applies to one service only.
func (repo *repo) SetCampaignService(ctx context.Context, id, service string) error {
	tx, err := repo.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	defer func() {
		_ = tx.Rollback()
	}()

	var current string

	err = tx.QueryRowContext(ctx, `SELECT servico FROM campaigns WHERE id = ?`, id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return web.ErrNotFound
	}

	if err != nil {
		return err
	}

	if current == service {
		return tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE campaigns SET servico = ?, updated_at = ? WHERE id = ?
	`, service, time.Now().UTC().Unix(), id); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM campaign_mail_log WHERE campaign_id = ? AND status = ?
	`, id, web.MailStatusUnsubscribed); err != nil {
		return err
	}

	return tx.Commit()
}

// ListServices returns services with how many campaigns use each.
func (repo *repo) ListServices(ctx context.Context) ([]web.MailService, error) {
	rows, err := repo.db.QueryContext(ctx, `
		SELECT s.name, s.label,
			(SELECT COUNT(*) FROM campaigns c WHERE c.servico = s.name)
		FROM mail_services s
		ORDER BY s.created_at, s.name
	`)
	if err != nil {
		return nil, err
	}

	defer func() {
		_ = rows.Close()
	}()

	var out []web.MailService

	for rows.Next() {
		var svc web.MailService
		if err := rows.Scan(&svc.Name, &svc.Label, &svc.Campaigns); err != nil {
			return nil, err
		}

		out = append(out, svc)
	}

	return out, rows.Err()
}

// GetService loads one service by name.
func (repo *repo) GetService(ctx context.Context, name string) (web.MailService, error) {
	var svc web.MailService

	err := repo.db.QueryRowContext(ctx, `
		SELECT s.name, s.label,
			(SELECT COUNT(*) FROM campaigns c WHERE c.servico = s.name)
		FROM mail_services s WHERE s.name = ?
	`, name).Scan(&svc.Name, &svc.Label, &svc.Campaigns)
	if errors.Is(err, sql.ErrNoRows) {
		return web.MailService{}, web.ErrNotFound
	}

	return svc, err
}

// CreateService stores a new service.
func (repo *repo) CreateService(ctx context.Context, svc *web.MailService) error {
	_, err := repo.db.ExecContext(ctx, `
		INSERT INTO mail_services (name, label, created_at) VALUES (?, ?, ?)
	`, svc.Name, svc.Label, time.Now().UTC().Unix())
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return web.ErrServiceExists
	}

	return err
}

// UpdateService changes the label of a service.
func (repo *repo) UpdateService(ctx context.Context, svc *web.MailService) error {
	res, err := repo.db.ExecContext(ctx, `
		UPDATE mail_services SET label = ? WHERE name = ?
	`, svc.Label, svc.Name)
	if err != nil {
		return err
	}

	return rowsAffected(res, web.ErrNotFound)
}

// DeleteService removes a service no campaign uses.
func (repo *repo) DeleteService(ctx context.Context, name string) error {
	tx, err := repo.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	defer func() {
		_ = tx.Rollback()
	}()

	var used int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM campaigns WHERE servico = ?`, name).Scan(&used); err != nil {
		return err
	}

	if used > 0 {
		return web.ErrServiceInUse
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM mail_services WHERE name = ?`, name)
	if err != nil {
		return err
	}

	if err := rowsAffected(res, web.ErrNotFound); err != nil {
		return err
	}

	return tx.Commit()
}
