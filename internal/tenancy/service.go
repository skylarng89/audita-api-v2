// Package tenancy implements the bounded-context module for platform
// tenants: bootstrap, CRUD, slug→tenant resolution, and SSO config
// (blueprint §3.2).
package tenancy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/audita/audita-api/internal/httpx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Status values for a tenant.
const (
	StatusActive    = "active"
	StatusSuspended = "suspended"
	StatusArchived  = "archived"
)

// Tenant is the platform-level tenant record (public schema, no RLS).
type Tenant struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	Subdomain string    `json:"subdomain,omitempty"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Service provides tenant management operations.
type Service struct {
	// db is a raw pgxpool — platform tables bypass RLS by design
	// (audita_app role still enforces schema-level privileges).
	db TenantDBPool
}

// slugPattern enforces safe slugs (SQL-injection defense at the app layer,
// complementing DB-level constraints).
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$`)

// ErrNotFound indicates no tenant matched.
var ErrNotFound = httpx.NewProblem(http.StatusNotFound, httpx.CodeNotFound, "tenant not found")

// ErrSlugTaken indicates slug collision on create/rename.
var ErrSlugTaken = httpx.NewProblem(http.StatusConflict, httpx.CodeConflict, "tenant slug already in use")

// NewService builds a Service over a pgx pool.
func NewService(db TenantDBPool) *Service { return &Service{db: db} }

// TenantDBPool is the minimal pool surface Service needs, kept as an
// interface for testability and future extraction (ADR-1).
type TenantDBPool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Create provisions a new tenant. Called only from the platform bootstrap or
// super-admin console (Sprint 5).
func (s *Service) Create(ctx context.Context, name, slug, subdomain string) (*Tenant, error) {
	if !slugPattern.MatchString(slug) {
		return nil, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation,
			"slug must be 3-64 chars of lowercase letters, digits, and hyphens")
	}
	if name == "" {
		return nil, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "name is required")
	}
	id := uuid.New()
	var created Tenant
	err := s.db.QueryRow(ctx, `
		INSERT INTO tenants (id, name, slug, subdomain, status)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5)
		ON CONFLICT (slug) DO NOTHING
		RETURNING id, name, slug, COALESCE(subdomain, ''), status, created_at, updated_at`,
		id, name, slug, subdomain, StatusActive,
	).Scan(&created.ID, &created.Name, &created.Slug, &created.Subdomain, &created.Status, &created.CreatedAt, &created.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSlugTaken
	}
	if err != nil {
		return nil, fmt.Errorf("tenancy: create tenant: %w", err)
	}
	return &created, nil
}

// GetByID fetches a tenant by UUID.
func (s *Service) GetByID(ctx context.Context, id uuid.UUID) (*Tenant, error) {
	t, err := s.scanOne(s.db.QueryRow(ctx, `
		SELECT id, name, slug, COALESCE(subdomain, ''), status, created_at, updated_at
		FROM tenants WHERE id = $1`, id))
	if err != nil {
		return nil, err
	}
	return t, nil
}

// GetBySlug fetches a tenant by slug (used by the tenant-resolution middleware).
func (s *Service) GetBySlug(ctx context.Context, slug string) (*Tenant, error) {
	t, err := s.scanOne(s.db.QueryRow(ctx, `
		SELECT id, name, slug, COALESCE(subdomain, ''), status, created_at, updated_at
		FROM tenants WHERE slug = $1`, slug))
	if err != nil {
		return nil, err
	}
	return t, nil
}

// List returns all tenants (platform admin only).
func (s *Service) List(ctx context.Context) ([]*Tenant, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, name, slug, COALESCE(subdomain, ''), status, created_at, updated_at
		FROM tenants ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("tenancy: list: %w", err)
	}
	defer rows.Close()
	out := []*Tenant{}
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetStatus suspends or reactivates a tenant.
func (s *Service) SetStatus(ctx context.Context, id uuid.UUID, status string) error {
	if status != StatusActive && status != StatusSuspended && status != StatusArchived {
		return httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "invalid status")
	}
	tag, err := s.db.Exec(ctx, `UPDATE tenants SET status = $2, updated_at = now() WHERE id = $1`, id, status)
	if err != nil {
		return fmt.Errorf("tenancy: set status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) scanOne(row pgx.Row) (*Tenant, error) {
	t, err := scanTenant(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTenant(r rowScanner) (*Tenant, error) {
	var t Tenant
	if err := r.Scan(&t.ID, &t.Name, &t.Slug, &t.Subdomain, &t.Status, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	return &t, nil
}
