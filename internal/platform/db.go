// Package runtime provides shared cross-cutting infrastructure: the
// tenant-scoped DB transaction template, middleware chain, metrics, and
// logging used by every module.
package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/audita/audita-api/internal/httpx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TenantDB wraps a pgxpool and provides a transaction template that sets the
// Postgres session variables (`app.tenant_id`, `app.user_id`, `app.role`) used
// by RLS policies (blueprint ADR-4 / FR-3). Every tenant-scoped query MUST go
// through InTenantTx (or TenantQuery) so isolation cannot be bypassed by
// accidentally using the raw pool.
type TenantDB struct {
	pool            *pgxpool.Pool
	statementTimeout time.Duration
}

// NewTenantDB builds a TenantDB around a pre-configured pgxpool.
func NewTenantDB(pool *pgxpool.Pool, statementTimeout time.Duration) *TenantDB {
	if statementTimeout <= 0 {
		statementTimeout = 10 * time.Second
	}
	return &TenantDB{pool: pool, statementTimeout: statementTimeout}
}

// Pool exposes the underlying pool for non-tenant-scoped (platform-level)
// operations only, e.g. migrations or the super-admin tenant CRUD path.
// It MUST NOT be used for tenant-scoped business queries.
func (t *TenantDB) Pool() *pgxpool.Pool { return t.pool }

// Close drains the pool.
func (t *TenantDB) Close() { t.pool.Close() }

// Ping checks database liveness.
func (t *TenantDB) Ping(ctx context.Context) error { return t.pool.Ping(ctx) }

// TenantContext carries the resolved tenant/user/role from the request into
// the DB session so RLS can scope every query.
type TenantContext struct {
	TenantID uuid.UUID
	UserID   uuid.UUID
	Role     string
}

// InTenantTx runs fn inside a single transaction with RLS session variables
// set via `SET LOCAL`, guaranteeing they reset at commit/rollback even on
// panic. This is the ONLY sanctioned way to run tenant-scoped business logic.
func (t *TenantDB) InTenantTx(ctx context.Context, tc TenantContext, fn func(pgx.Tx) error) error {
	if tc.TenantID == uuid.Nil {
		return httpx.NewProblem(http.StatusUnauthorized, httpx.CodeUnauthorized, "tenant context missing")
	}
	txOpts := pgx.TxOptions{}
	tx, err := t.pool.BeginTx(ctx, txOpts)
	if err != nil {
		return fmt.Errorf("platform: begin tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tc.TenantID.String()); err != nil {
		return fmt.Errorf("platform: set app.tenant_id: %w", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.user_id', $1, true)", tc.UserID.String()); err != nil {
		return fmt.Errorf("platform: set app.user_id: %w", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.role', $1, true)", tc.Role); err != nil {
		return fmt.Errorf("platform: set app.role: %w", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", t.statementTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("platform: set statement_timeout: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// TenantQuery runs a read-only query with RLS session variables set on a
// dedicated connection, for single-statement reads that don't need a full tx.
func (t *TenantDB) TenantQuery(ctx context.Context, tc TenantContext, fn func(pgx.Tx) error) error {
	return t.InTenantTx(ctx, tc, fn)
}

// NewRequestID generates a random 16-byte hex request correlation ID.
func NewRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
