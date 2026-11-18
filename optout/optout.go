// Package optout reads the unsubscribe list kept by the Trentin website.
// The tables belong to the website: this package only reads them.
package optout

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

const (
	// EnvHost and the other Env names configure the unsubscribe database.
	EnvHost     = "MYSQL_BANCO_NAO_PERTURBE_HOST"
	EnvPort     = "MYSQL_BANCO_NAO_PERTURBE_PORT"
	EnvUser     = "MYSQL_BANCO_NAO_PERTURBE_USER"
	EnvPassword = "MYSQL_BANCO_NAO_PERTURBE_PASS" //nolint:gosec // G101: this is the variable name, not a secret.
	EnvDatabase = "MYSQL_BANCO_NAO_PERTURBE_DB"
	// EnvUnsubscribeBase is the page linked from every message.
	EnvUnsubscribeBase = "UNSUBSCRIBE_BASE_URL"

	// DefaultUnsubscribeBase is used when EnvUnsubscribeBase is empty.
	DefaultUnsubscribeBase = "https://trentin.software/descadastro"

	defaultPort = "3306"
	// queryTimeout bounds connecting and each lookup, so a slow database stops a send quickly.
	queryTimeout = 5 * time.Second
	maxOpenConns = 2
	connLifetime = 5 * time.Minute
)

const optedOutQuery = `
SELECT 1
  FROM emails_nao_perturbe e
  JOIN servicos s ON s.id = e.servico_id
 WHERE e.email = ? AND s.servico_nome = ?
 LIMIT 1`

const serviceExistsQuery = `SELECT 1 FROM servicos WHERE servico_nome = ? LIMIT 1`

// Config is the connection to the unsubscribe database.
type Config struct {
	Host     string
	Port     string
	User     string
	Password string
	Database string
}

// ConfigFromEnv reads the connection from the environment.
// ok is false when host, user, or database is missing.
func ConfigFromEnv() (cfg Config, ok bool) {
	cfg = Config{
		Host:     strings.TrimSpace(os.Getenv(EnvHost)),
		Port:     strings.TrimSpace(os.Getenv(EnvPort)),
		User:     strings.TrimSpace(os.Getenv(EnvUser)),
		Password: os.Getenv(EnvPassword),
		Database: strings.TrimSpace(os.Getenv(EnvDatabase)),
	}

	if cfg.Port == "" {
		cfg.Port = defaultPort
	}

	return cfg, cfg.Host != "" && cfg.User != "" && cfg.Database != ""
}

// UnsubscribeBaseFromEnv returns the unsubscribe page, or the default page.
func UnsubscribeBaseFromEnv() string {
	if v := strings.TrimSpace(os.Getenv(EnvUnsubscribeBase)); v != "" {
		return v
	}

	return DefaultUnsubscribeBase
}

// DSN returns the driver connection string with short timeouts and utf8mb4.
func (c *Config) DSN() string {
	mc := mysql.NewConfig()
	mc.User = c.User
	mc.Passwd = c.Password
	mc.Net = "tcp"
	mc.Addr = net.JoinHostPort(c.Host, c.Port)
	mc.DBName = c.Database
	mc.Timeout = queryTimeout
	mc.ReadTimeout = queryTimeout
	mc.WriteTimeout = queryTimeout
	mc.Params = map[string]string{"charset": "utf8mb4"}

	return mc.FormatDSN()
}

// Checker looks up opted-out addresses. It never writes.
type Checker struct {
	db *sql.DB
}

// Open creates a small pool. It does not connect until the first lookup.
func Open(cfg *Config) (*Checker, error) {
	db, err := sql.Open("mysql", cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("open unsubscribe database: %w", err)
	}

	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxOpenConns)
	db.SetConnMaxLifetime(connLifetime)

	return New(db), nil
}

// New wraps an open database.
func New(db *sql.DB) *Checker {
	return &Checker{db: db}
}

// OptedOut reports whether email has a confirmed opt-out for service.
// email must already be trimmed and lowercased.
func (c *Checker) OptedOut(ctx context.Context, email, service string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var one int

	err := c.db.QueryRowContext(ctx, optedOutQuery, email, service).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	return true, nil
}

// ServiceExists reports whether the website has a service with this name.
func (c *Checker) ServiceExists(ctx context.Context, service string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var one int

	err := c.db.QueryRowContext(ctx, serviceExistsQuery, service).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	return true, nil
}

// Ping checks the connection within the lookup timeout.
func (c *Checker) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	return c.db.PingContext(ctx)
}

// Close releases the pool.
func (c *Checker) Close() error {
	return c.db.Close()
}
