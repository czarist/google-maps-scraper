package sqlite

import (
	"context"

	"github.com/gosom/google-maps-scraper/web"
)

// jobFilterWhere is the WHERE part for a home table filter: "" all jobs,
// "none" jobs without a campaign, otherwise one campaign id.
func jobFilterWhere(filter string) (string, []any) {
	switch filter {
	case "":
		return "", nil
	case "none":
		return ` WHERE NOT EXISTS (SELECT 1 FROM campaign_jobs cj WHERE cj.job_id = j.id)`, nil
	default:
		return ` WHERE EXISTS (SELECT 1 FROM campaign_jobs cj WHERE cj.job_id = j.id AND cj.campaign_id = ?)`, []any{filter}
	}
}

// JobTablePage returns one page of the home table and how many jobs match the
// filter. Running jobs come first, then the newest. Only that page is decoded.
func (repo *repo) JobTablePage(ctx context.Context, filter string, offset, limit int) ([]web.Job, int, error) {
	where, args := jobFilterWhere(filter)

	var total int
	if err := repo.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs j`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := `SELECT j.id, j.name, j.status, j.data, j.created_at, j.updated_at FROM jobs j` + where +
		` ORDER BY (j.status = 'working') DESC, j.created_at DESC, j.id DESC LIMIT ? OFFSET ?`

	rows, err := repo.db.QueryContext(ctx, q, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var jobs []web.Job

	for rows.Next() {
		j, err := rowToJob(rows)
		if err != nil {
			return nil, 0, err
		}

		jobs = append(jobs, j)
	}

	return jobs, total, rows.Err()
}

// JobSummaries returns id, status and places found of every job of a filter,
// newest first, read straight from SQL without decoding the job data.
func (repo *repo) JobSummaries(ctx context.Context, filter string) ([]web.JobSummary, error) {
	where, args := jobFilterWhere(filter)

	rows, err := repo.db.QueryContext(ctx, `SELECT j.id, j.status, COALESCE(json_extract(j.data, '$.run.places'), 0)
		FROM jobs j`+where+` ORDER BY j.created_at DESC, j.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []web.JobSummary

	for rows.Next() {
		var js web.JobSummary
		if err := rows.Scan(&js.ID, &js.Status, &js.Places); err != nil {
			return nil, err
		}

		out = append(out, js)
	}

	return out, rows.Err()
}

// StatusCounts counts jobs per status.
func (repo *repo) StatusCounts(ctx context.Context) (map[string]int, error) {
	rows, err := repo.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM jobs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}

	for rows.Next() {
		var (
			status string
			n      int
		)

		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}

		out[status] = n
	}

	return out, rows.Err()
}

// Backup writes a consistent copy of the whole database to path, while it is in use.
func (repo *repo) Backup(ctx context.Context, path string) error {
	_, err := repo.db.ExecContext(ctx, `VACUUM INTO ?`, path)

	return err
}
