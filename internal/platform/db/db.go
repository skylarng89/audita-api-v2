package db

// Package db provides database connection bootstrap (pgxpool) and
// golang-migrate integration (blueprint ADR-3 / FR-11).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ConnectPool builds a pgxpool with sensible production defaults: bounded
// lifetime, health checks, and a statement-level timeout to keep slow
// background queries from starving the request pool (blueprint §4.3).
func ConnectPool(ctx context.Context, databaseURL string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("db: parse DATABASE_URL: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// MigrateUp applies all pending migrations from the embedded/local
// migrations directory. It is safe to run on every deploy (idempotent).
// migrationsPath points at the filesystem folder containing
// `NNNN_name.up.sql` / `NNNN_name.down.sql` files.
func MigrateUp(databaseURL, migrationsPath string) error {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return fmt.Errorf("db: parse url for migrate: %w", err)
	}
	// golang-migrate's pgx driver expects scheme `pgx5://`.
	if u.Scheme == "postgres" || u.Scheme == "postgresql" {
		u.Scheme = "pgx5"
	}
	m, err := migrate.New("file://"+migrationsPath, u.String())
	if err != nil {
		return fmt.Errorf("db: init migrate: %w", err)
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("db: migrate up: %w", err)
	}
	return nil
}

// WaitForDB blocks until the database accepts connections or ctx is done,
// with a bounded retry loop so `docker-compose up` ordering doesn't crash
// the API before Postgres is ready.
func WaitForDB(ctx context.Context, databaseURL string, timeout time.Duration, logger *slog.Logger) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		pool, err := ConnectPool(ctx, databaseURL, 1)
		if err == nil {
			pool.Close()
			return nil
		}
		lastErr = err
		logger.Info("waiting for database", slog.String("error", err.Error()))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("db: not ready after %s: %w", timeout, lastErr)
}
