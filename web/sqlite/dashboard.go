package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/gosom/google-maps-scraper/web"
)

// DashboardStats counts today's job and email activity straight from SQL.
func (repo *repo) DashboardStats(ctx context.Context, since time.Time) (web.ActivityStats, error) {
	var st web.ActivityStats

	from := since.UTC().Format(time.RFC3339)

	var done, failed, places sql.NullInt64

	err := repo.db.QueryRowContext(ctx, `
		SELECT
			SUM(CASE WHEN status = 'ok' AND julianday(json_extract(data, '$.run.finished_at')) >= julianday(?) THEN 1 ELSE 0 END),
			SUM(CASE WHEN status = 'failed' AND julianday(json_extract(data, '$.run.finished_at')) >= julianday(?) THEN 1 ELSE 0 END),
			SUM(CASE WHEN julianday(json_extract(data, '$.run.finished_at')) >= julianday(?) THEN COALESCE(json_extract(data, '$.run.places'), 0) ELSE 0 END)
		FROM jobs
	`, from, from, from).Scan(&done, &failed, &places)
	if err != nil {
		return st, err
	}

	st.JobsDone, st.JobsFailed, st.PlacesFound = int(done.Int64), int(failed.Int64), int(places.Int64)

	// Average run time of the last finished jobs, for the queue estimate.
	var avg sql.NullFloat64

	err = repo.db.QueryRowContext(ctx, `
		SELECT AVG((julianday(json_extract(data, '$.run.finished_at')) - julianday(json_extract(data, '$.run.started_at'))) * 86400)
		FROM (SELECT data FROM jobs
			WHERE status IN ('ok', 'failed') AND json_extract(data, '$.run.finished_at') IS NOT NULL
				AND json_extract(data, '$.run.started_at') IS NOT NULL
			ORDER BY updated_at DESC LIMIT 20)
	`).Scan(&avg)
	if err != nil {
		return st, err
	}

	if avg.Valid && avg.Float64 > 0 {
		st.AvgJobDuration = time.Duration(avg.Float64 * float64(time.Second))
	}

	var sent, mailFailed sql.NullInt64

	err = repo.db.QueryRowContext(ctx, `
		SELECT SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), SUM(CASE WHEN status = ? THEN 1 ELSE 0 END)
		FROM campaign_mail_log WHERE sent_at >= ?
	`, web.MailStatusSent, web.MailStatusFailed, since.UTC().Unix()).Scan(&sent, &mailFailed)
	if err != nil {
		return st, err
	}

	st.EmailsSent, st.EmailsFailed = int(sent.Int64), int(mailFailed.Int64)

	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM campaign_auto WHERE enabled = 1`).Scan(&st.AutoCampaigns); err != nil {
		return st, err
	}

	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM campaign_dispatches WHERE status = ?`, web.DispatchRunning).Scan(&st.Dispatching); err != nil {
		return st, err
	}

	return st, nil
}
