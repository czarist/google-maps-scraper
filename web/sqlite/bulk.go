package sqlite

import (
	"context"
	"time"

	"github.com/gosom/google-maps-scraper/web"
)

// CreateJobs inserts many jobs in one transaction and, when campaignID is set,
// puts them in that campaign. Thousands of jobs take a fraction of a second.
func (repo *repo) CreateJobs(ctx context.Context, jobs []web.Job, campaignID string) error {
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

	if campaignID != "" {
		if err := requireRow(ctx, tx, `SELECT 1 FROM campaigns WHERE id = ?`, campaignID, web.ErrNotFound); err != nil {
			return err
		}
	}

	insJob, err := tx.PrepareContext(ctx, `INSERT INTO jobs (id, name, status, data, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insJob.Close()

	insMember, err := tx.PrepareContext(ctx, `INSERT INTO campaign_jobs (campaign_id, job_id, created_at) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insMember.Close()

	now := time.Now().UTC().Unix()

	for i := range jobs {
		item, err := jobToRow(&jobs[i])
		if err != nil {
			return err
		}

		if _, err := insJob.ExecContext(ctx, item.ID, item.Name, item.Status, item.Data, item.CreatedAt, item.UpdatedAt); err != nil {
			return err
		}

		if campaignID != "" {
			if _, err := insMember.ExecContext(ctx, campaignID, item.ID, now); err != nil {
				return err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	committed = true

	return nil
}

// AssignJobs moves many jobs into a campaign in one transaction. An empty
// campaignID takes the jobs out of any campaign. Unknown job ids are skipped.
func (repo *repo) AssignJobs(ctx context.Context, campaignID string, jobIDs []string) error {
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

	if campaignID != "" {
		if err := requireRow(ctx, tx, `SELECT 1 FROM campaigns WHERE id = ?`, campaignID, web.ErrNotFound); err != nil {
			return err
		}
	}

	del, err := tx.PrepareContext(ctx, `DELETE FROM campaign_jobs WHERE job_id = ?`)
	if err != nil {
		return err
	}
	defer del.Close()

	ins, err := tx.PrepareContext(ctx, `
		INSERT INTO campaign_jobs (campaign_id, job_id, created_at)
		SELECT ?, id, ? FROM jobs WHERE id = ?
	`)
	if err != nil {
		return err
	}
	defer ins.Close()

	now := time.Now().UTC().Unix()

	for _, id := range jobIDs {
		if _, err := del.ExecContext(ctx, id); err != nil {
			return err
		}

		if campaignID != "" {
			if _, err := ins.ExecContext(ctx, campaignID, now, id); err != nil {
				return err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	committed = true

	return nil
}

// DeleteJobs deletes many jobs and their campaign links in one transaction.
func (repo *repo) DeleteJobs(ctx context.Context, ids []string) error {
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

	for _, q := range []string{`DELETE FROM campaign_jobs WHERE job_id = ?`, `DELETE FROM jobs WHERE id = ?`} {
		stmt, err := tx.PrepareContext(ctx, q)
		if err != nil {
			return err
		}

		for _, id := range ids {
			if _, err := stmt.ExecContext(ctx, id); err != nil {
				_ = stmt.Close()

				return err
			}
		}

		_ = stmt.Close()
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	committed = true

	return nil
}
