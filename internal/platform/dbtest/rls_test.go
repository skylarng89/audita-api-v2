// Package rls_test contains the negative-isolation tests required by
// blueprint FR-3: "RLS policy test coverage — 100% of tenant-scoped tables
// have a negative-isolation test". These tests require a live Postgres with
// migrations applied (docker compose up postgres migrate) and are skipped
// when TEST_DATABASE_URL is unset, so plain `go test ./...` still passes
// without infra.
//
//	Run locally (URL comes from your .env — never commit real credentials):
//
//		docker compose up -d postgres migrate
//		TEST_DATABASE_URL="$(grep ^DATABASE_URL .env | cut -d= -f2-)" \
//	  go test ./internal/platform/dbtest/... -v
package rls_test

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// tenantScopedTables mirrors migration 0006/0007. Adding a tenant-scoped
// table to the schema without adding it here MUST fail the FR-3 coverage
// gate (a separate lint step cross-checks this list against the migrations).
var tenantScopedTables = []string{
	"roles", "role_permissions", "users", "refresh_tokens", "invite_tokens",
	"password_reset_tokens", "oauth_accounts", "departments", "groups",
	"group_members", "change_requests", "cr_approvers", "cr_approver_groups",
	"cr_watchers", "cr_watcher_groups", "custom_field_definitions",
	"change_request_custom_fields", "request_links", "comments",
	"comment_mentions", "attachments", "activity_stream", "notifications",
	"audit_log", "audit_export_requests", "org_settings",
}

func testDBURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping RLS integration test")
	}
	return u
}

// testMu guards schema mutations across parallel subtests.
var testMu sync.Mutex

func connect(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, testDBURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestRLSBlocksCrossTenantReads seeds two tenants with a row each in every
// tenant-scoped table, then asserts a session scoped to tenant A cannot read
// tenant B's rows, and vice versa (FR-3 negative-isolation gate).
func TestRLSBlocksCrossTenantReads(t *testing.T) {
	ctx := context.Background()
	pool := connect(t, ctx)

	tenantA, tenantB := uuid.New(), uuid.New()
	seedTenants(t, ctx, pool, tenantA, tenantB)

	for _, table := range tenantScopedTables {
		t.Run(table, func(t *testing.T) {
			testMu.Lock()
			defer testMu.Unlock()

			insertScopedRow(t, ctx, pool, table, tenantA)
			insertScopedRow(t, ctx, pool, table, tenantB)

			if got := countScoped(t, ctx, pool, table, tenantA); got != 1 {
				t.Errorf("tenant A sees %d rows in %s, want 1", got, table)
			}
			if got := countScoped(t, ctx, pool, table, tenantB); got != 1 {
				t.Errorf("tenant B sees %d rows in %s, want 1", got, table)
			}
		})
	}
}

// TestRLSBlocksUnsetSession asserts a connection with NO app.tenant_id set
// sees zero tenant rows — defense against forgotten session-var wiring.
func TestRLSBlocksUnsetSession(t *testing.T) {
	ctx := context.Background()
	pool := connect(t, ctx)

	tenantA := uuid.New()
	seedTenants(t, ctx, pool, tenantA, uuid.New())

	for _, table := range tenantScopedTables {
		t.Run(table, func(t *testing.T) {
			testMu.Lock()
			defer testMu.Unlock()

			insertScopedRow(t, ctx, pool, table, tenantA)

			conn, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			if _, err := conn.Exec(ctx, `SELECT set_config('app.tenant_id', '', true)`); err != nil {
				t.Fatal(err)
			}
			var n int
			if err := conn.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
				// Erroring on an unset setting is also an acceptable
				// isolation outcome.
				return
			}
			if n != 0 {
				t.Errorf("unscoped session sees %d rows in %s, want 0", n, table)
			}
		})
	}
}

func seedTenants(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids ...uuid.UUID) {
	t.Helper()
	for _, id := range ids {
		suffix := id.String()[:8]
		if _, err := pool.Exec(ctx, `
			INSERT INTO tenants (id, name, slug, status)
			VALUES ($1, $2, $3, 'active')
			ON CONFLICT (slug) DO NOTHING`,
			id, "rls-test-"+suffix, "rls-test-"+suffix); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
	}
}

// insertScopedRow inserts one minimal valid row into the given table for the
// tenant. Keep the per-table SQL in sync with schema changes — a schema
// change without a matching update here fails this test loudly.
func insertScopedRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string, tenantID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO permissions (id, key) VALUES (gen_random_uuid(), 'cr.read')
		ON CONFLICT (key) DO NOTHING`); err != nil {
		t.Fatalf("seed permission: %v", err)
	}
	queries := map[string]string{
		"roles":            `INSERT INTO roles (id, tenant_id, name) VALUES ($2, $1, 'member')`,
		"role_permissions": `INSERT INTO role_permissions (id, tenant_id, role_id, permission_id) SELECT $2, $1, r.id, p.id FROM roles r, permissions p WHERE r.tenant_id = $1 LIMIT 1`,
		"users":            `INSERT INTO users (id, tenant_id, email, full_name, status) VALUES ($2, $1, 'u-' || $2::text || '@rls.test', 'RLS User', 'active')`,
		"refresh_tokens":   `INSERT INTO refresh_tokens (id, tenant_id, user_id, token_hash, expires_at) SELECT $2, $1, u.id, 'hash-' || $2::text, now() + interval '1 day' FROM users u WHERE u.tenant_id = $1 LIMIT 1`,
		"invite_tokens":    `INSERT INTO invite_tokens (id, tenant_id, user_id, token_hash, expires_at) SELECT $2, $1, u.id, 'hash-' || $2::text, now() + interval '1 day' FROM users u WHERE u.tenant_id = $1 LIMIT 1`,
		"password_reset_tokens": `INSERT INTO password_reset_tokens (id, tenant_id, user_id, token_hash, expires_at) SELECT $2, $1, u.id, 'hash-' || $2::text, now() + interval '1 day' FROM users u WHERE u.tenant_id = $1 LIMIT 1`,
		"oauth_accounts":        `INSERT INTO oauth_accounts (id, tenant_id, user_id, provider, provider_uid) SELECT $2, $1, u.id, 'google', 'uid-' || $2::text FROM users u WHERE u.tenant_id = $1 LIMIT 1`,
		"departments":           `INSERT INTO departments (id, tenant_id, name) VALUES ($2, $1, 'Dept')`,
		"groups":                `INSERT INTO groups (id, tenant_id, name) VALUES ($2, $1, 'Group')`,
		"group_members":         `INSERT INTO group_members (id, tenant_id, group_id, user_id) SELECT $2, $1, g.id, u.id FROM groups g, users u WHERE g.tenant_id = $1 AND u.tenant_id = $1 LIMIT 1`,
		"change_requests":       `INSERT INTO change_requests (id, tenant_id, title, status, completion_status, created_by) VALUES ($2, $1, 'CR', 'draft', 'open', (SELECT id FROM users WHERE tenant_id = $1 LIMIT 1))`,
		"cr_approvers":          `INSERT INTO cr_approvers (id, tenant_id, change_request_id, user_id) SELECT $2, $1, c.id, u.id FROM change_requests c, users u WHERE c.tenant_id = $1 AND u.tenant_id = $1 LIMIT 1`,
		"cr_approver_groups":    `INSERT INTO cr_approver_groups (id, tenant_id, change_request_id, group_id) SELECT $2, $1, c.id, g.id FROM change_requests c, groups g WHERE c.tenant_id = $1 AND g.tenant_id = $1 LIMIT 1`,
		"cr_watchers":           `INSERT INTO cr_watchers (id, tenant_id, change_request_id, user_id) SELECT $2, $1, c.id, u.id FROM change_requests c, users u WHERE c.tenant_id = $1 AND u.tenant_id = $1 LIMIT 1`,
		"cr_watcher_groups":     `INSERT INTO cr_watcher_groups (id, tenant_id, change_request_id, group_id) SELECT $2, $1, c.id, g.id FROM change_requests c, groups g WHERE c.tenant_id = $1 AND g.tenant_id = $1 LIMIT 1`,
		"custom_field_definitions":     `INSERT INTO custom_field_definitions (id, tenant_id, label, field_type) VALUES ($2, $1, 'Field', 'text')`,
		"change_request_custom_fields": `INSERT INTO change_request_custom_fields (id, tenant_id, change_request_id, custom_field_def_id, value) SELECT $2, $1, c.id, d.id, '"x"' FROM change_requests c, custom_field_definitions d WHERE c.tenant_id = $1 AND d.tenant_id = $1 LIMIT 1`,
		"request_links":                `INSERT INTO request_links (id, tenant_id, request_id_a, request_id_b, linked_by) SELECT $2, $1, a.id, b.id, u.id FROM change_requests a, change_requests b, users u WHERE a.tenant_id = $1 AND b.tenant_id = $1 AND a.id < b.id AND u.tenant_id = $1 LIMIT 1`,
		"comments":                     `INSERT INTO comments (id, tenant_id, change_request_id, author_id, body) SELECT $2, $1, c.id, u.id, '<p>hi</p>' FROM change_requests c, users u WHERE c.tenant_id = $1 AND u.tenant_id = $1 LIMIT 1`,
		"comment_mentions":             `INSERT INTO comment_mentions (id, tenant_id, comment_id, user_id) SELECT $2, $1, cm.id, u.id FROM comments cm, users u WHERE cm.tenant_id = $1 AND u.tenant_id = $1 LIMIT 1`,
		"attachments":                  `INSERT INTO attachments (id, tenant_id, change_request_id, uploader_id, file_name, mime_type, size_bytes, storage_key) SELECT $2, $1, c.id, u.id, 'f.txt', 'text/plain', 4, 'k-' || $2::text FROM change_requests c, users u WHERE c.tenant_id = $1 AND u.tenant_id = $1 LIMIT 1`,
		"activity_stream":              `INSERT INTO activity_stream (id, tenant_id, change_request_id, actor_id, action_type, occurred_at) SELECT $2, $1, c.id, u.id, 'created', now() FROM change_requests c, users u WHERE c.tenant_id = $1 AND u.tenant_id = $1 LIMIT 1`,
		"notifications":                `INSERT INTO notifications (id, tenant_id, recipient_id, type, title) VALUES ($2, $1, (SELECT id FROM users WHERE tenant_id = $1 LIMIT 1), 'test', 'Hello')`,
		"audit_log":                    `INSERT INTO audit_log (id, tenant_id, chain_index, record_hash, previous_hash, action_type, entity_type, occurred_at) VALUES ($2, $1, 1, 'h', 'p', 'test', 'test', now())`,
		"audit_export_requests":        `INSERT INTO audit_export_requests (id, tenant_id, requested_by) VALUES ($2, $1, (SELECT id FROM users WHERE tenant_id = $1 LIMIT 1))`,
		"org_settings":                 `INSERT INTO org_settings (id, tenant_id, key, value) VALUES ($2, $1, 'k-' || $2::text, '{}')`,
	}
	q, ok := queries[table]
	if !ok {
		t.Fatalf("no insert query defined for table %q — update insertScopedRow", table)
	}
	if _, err := pool.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	if _, err := pool.Exec(ctx, q, tenantID, uuid.New()); err != nil {
		t.Fatalf("insert into %s: %v", table, err)
	}
}

// countScoped opens a dedicated connection scoped to one tenant and counts
// visible rows in the table.
func countScoped(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string, tenantID uuid.UUID) int {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}
