// Package mysqlstore saves scraped places to a MySQL database.
package mysqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	// Registers the "mysql" driver for database/sql.
	_ "github.com/go-sql-driver/mysql"
)

const (
	// connectTimeout is how long Open keeps retrying while MySQL starts up.
	connectTimeout = 90 * time.Second
	retryDelay     = 2 * time.Second
)

const createPlacesTable = `
CREATE TABLE IF NOT EXISTS places (
	id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
	job_id         VARCHAR(36)  NOT NULL,
	job_name       VARCHAR(255) NOT NULL DEFAULT '',
	place_key      VARCHAR(255) NOT NULL,
	title          VARCHAR(512) NOT NULL DEFAULT '',
	category       VARCHAR(255) NOT NULL DEFAULT '',
	phone          VARCHAR(64)  NOT NULL DEFAULT '',
	website        VARCHAR(1024) NOT NULL DEFAULT '',
	emails         TEXT         NULL,
	address        VARCHAR(1024) NOT NULL DEFAULT '',
	city           VARCHAR(255) NOT NULL DEFAULT '',
	state          VARCHAR(255) NOT NULL DEFAULT '',
	country        VARCHAR(64)  NOT NULL DEFAULT '',
	postal_code    VARCHAR(32)  NOT NULL DEFAULT '',
	latitude       DOUBLE       NULL,
	longitude      DOUBLE       NULL,
	review_count   INT          NOT NULL DEFAULT 0,
	review_rating  DECIMAL(3,2) NOT NULL DEFAULT 0,
	link           TEXT         NULL,
	data           JSON         NOT NULL,
	created_at     TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at     TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
	PRIMARY KEY (id),
	UNIQUE KEY uq_places_job_place (job_id, place_key),
	KEY idx_places_city (city),
	KEY idx_places_category (category)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`

// Open connects to MySQL, waiting for the server to accept connections,
// and makes sure the places table exists.
func Open(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}

	deadline := time.Now().Add(connectTimeout)

	for {
		err = db.PingContext(ctx)
		if err == nil {
			break
		}

		if time.Now().After(deadline) {
			_ = db.Close()

			return nil, fmt.Errorf("connect to mysql: %w", err)
		}

		select {
		case <-ctx.Done():
			_ = db.Close()

			return nil, ctx.Err()
		case <-time.After(retryDelay):
		}
	}

	if err := EnsureSchema(ctx, db); err != nil {
		_ = db.Close()

		return nil, err
	}

	return db, nil
}

// EnsureSchema creates the places table when it does not exist yet.
func EnsureSchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, createPlacesTable); err != nil {
		return fmt.Errorf("create places table: %w", err)
	}

	return nil
}
