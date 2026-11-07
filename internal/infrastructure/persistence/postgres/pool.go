// Package postgres implements the PostgreSQL 16+ persistence layer compatible with
// PgBouncer transaction pooling via Simple Protocol Mode (ADR-003) and transactional
// internal audit ledger writes (ADR-004).
package postgres

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rag-prompt-registry-service/internal/infrastructure/persistence/migrations"
)

// PoolOptions holds configuration parameters for the PostgreSQL connection pool.
type PoolOptions struct {
	DSN               string
	MaxConns          int32
	MinConns          int32
	MaxConnLifetime   time.Duration
	MaxConnIdleTime   time.Duration
	HealthCheckPeriod time.Duration
}

// BuildPoolConfig parses the PostgreSQL connection string and explicitly configures
// ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol (ADR-003)
// to prevent prepared statement collisions across PgBouncer transaction pooling.
func BuildPoolConfig(opts PoolOptions) (*pgxpool.Config, error) {
	if strings.TrimSpace(opts.DSN) == "" {
		return nil, fmt.Errorf("postgres DSN is required")
	}

	cfg, err := pgxpool.ParseConfig(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("failed to parse postgres configuration: %s", SanitizeDSNError(err, opts.DSN))
	}

	// ADR-003 Invariant 6: Force Simple Protocol Mode for PgBouncer transaction pooling compatibility.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	} else {
		cfg.MaxConns = 25
	}
	if opts.MinConns >= 0 {
		cfg.MinConns = opts.MinConns
	} else {
		cfg.MinConns = 2
	}
	if opts.MaxConnLifetime > 0 {
		cfg.MaxConnLifetime = opts.MaxConnLifetime
	} else {
		cfg.MaxConnLifetime = 30 * time.Minute
	}
	if opts.MaxConnIdleTime > 0 {
		cfg.MaxConnIdleTime = opts.MaxConnIdleTime
	} else {
		cfg.MaxConnIdleTime = 5 * time.Minute
	}
	if opts.HealthCheckPeriod > 0 {
		cfg.HealthCheckPeriod = opts.HealthCheckPeriod
	} else {
		cfg.HealthCheckPeriod = 30 * time.Second
	}

	return cfg, nil
}

// NewPool initializes a pgxpool.Pool configured with QueryExecModeSimpleProtocol.
func NewPool(ctx context.Context, opts PoolOptions) (*pgxpool.Pool, error) {
	cfg, err := BuildPoolConfig(opts)
	if err != nil {
		return nil, err
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create postgres pool: %s", SanitizeDSNError(err, opts.DSN))
	}
	return pool, nil
}

// ApplyMigrations executes the embedded 000001_init_schema.up.sql migration script.
func ApplyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	ddl, err := migrations.InitSchemaUp()
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("failed to apply schema migration: %w", err)
	}
	return nil
}

// SanitizeDSNError masks any password or raw DSN credentials from error messages (Invariant 2).
func SanitizeDSNError(err error, dsn string) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if dsn != "" {
		msg = strings.ReplaceAll(msg, dsn, "[REDACTED_DSN]")
		if u, parseErr := url.Parse(dsn); parseErr == nil && u.User != nil {
			if pass, hasPass := u.User.Password(); hasPass && pass != "" {
				msg = strings.ReplaceAll(msg, pass, "[REDACTED]")
			}
		}
	}
	return msg
}
