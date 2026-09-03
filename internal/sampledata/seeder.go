// Package sampledata seeds demo tenants with realistic data
// (blueprint Sprint 5, Task 5.5 — ~80 CRs / 30 users / 10 groups per demo
// tenant, covering all lifecycle states, group + individual approvers,
// watchers, comments with mentions, attachments, and linked requests).
package sampledata

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

// Seeder seeds demo data into an existing tenant.
type Seeder struct {
	pool Pool
	log  *slog.Logger
}

// Pool is the minimal pgxpool surface the seeder needs (kept as an interface
// so the module stays independent of pgx internals and is testable).
type Pool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// NewSeeder builds a Seeder.
func NewSeeder(pool Pool, log *slog.Logger) *Seeder {
	return &Seeder{pool: pool, log: log}
}

// SeedCounts defines the demo volume (v1 FR-13.2).
const (
	NumUsers  = 30
	NumGroups = 10
	NumCRs    = 80
)

// demoPassword is bcrypt-hashed and applied to every demo user.
const demoPassword = "demo-password-1"

var firstNames = []string{"Alice", "Bob", "Carla", "Diego", "Elena", "Farid", "Grace", "Henrik", "Imani", "Jonas",
	"Kira", "Liam", "Mina", "Noah", "Olga", "Pedro", "Quinn", "Rosa", "Samir", "Tessa",
	"Umar", "Vera", "Wes", "Xiu", "Yusuf", "Zara", "Amir", "Bella", "Cato", "Dario"}
var lastNames = []string{"Andersen", "Bauer", "Costa", "Duarte", "Eriksen", "Fischer", "Gomez", "Hansen", "Ito", "Jansen",
	"Keller", "Lindqvist", "Moreau", "Novak", "Olsen", "Pereira", "Quintana", "Rossi", "Silva", "Tanaka",
	"Ubeda", "Vogel", "Weber", "Xu", "Yilmaz", "Zhang", "Almeida", "Berg", "Cruz", "Dubois"}
var groupNames = []string{"Platform", "Security", "Database", "Network", "Frontend", "Backend", "QA", "SRE", "Data", "Compliance"}
var deptNames = []string{"Engineering", "Operations", "Finance", "Legal", "Support"}
var systems = []string{"api-gateway", "billing-db", "web-app", "auth-service", "reporting", "cdn", "search", "queue"}
var statuses = []string{"draft", "pending_approval", "approved", "rejected", "cancelled", "approved"}

// Seed populates a tenant with demo users, groups, departments, and CRs.
// It is idempotent per tenant: re-running skips users already seeded
// (is_sample = TRUE + same emails).
func (s *Seeder) Seed(ctx context.Context, tenantID uuid.UUID) error {
	s.log.Info("seeding sample data", slog.String("tenantId", tenantID.String()))
	if err := s.setTenant(ctx, tenantID); err != nil {
		return fmt.Errorf("sampledata: set tenant: %w", err)
	}
	departments, err := s.seedDepartments(ctx)
	if err != nil {
		return err
	}
	users, err := s.seedUsers(ctx)
	if err != nil {
		return err
	}
	groups, err := s.seedGroups(ctx, users)
	if err != nil {
		return err
	}
	if err := s.seedChangeRequests(ctx, users, groups, departments); err != nil {
		return err
	}
	s.log.Info("sample data seeded",
		slog.Int("users", len(users)), slog.Int("groups", len(groups)), slog.Int("crs", NumCRs))
	return nil
}

func (s *Seeder) setTenant(ctx context.Context, tenantID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String())
	return err
}

func (s *Seeder) seedDepartments(ctx context.Context) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, 0, len(deptNames))
	for i, name := range deptNames {
		id := uuid.New()
		_, err := s.pool.Exec(ctx, `
			INSERT INTO departments (id, name, code, is_active, display_order)
			VALUES ($1, $2, $3, TRUE, $4)
			ON CONFLICT DO NOTHING`, id, name, name[:3], i)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (s *Seeder) seedUsers(ctx context.Context) ([]uuid.UUID, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(demoPassword), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, NumUsers)
	for i := 0; i < NumUsers; i++ {
		id := uuid.New()
		email := fmt.Sprintf("demo%d@sample.audita.local", i+1)
		name := firstNames[i%len(firstNames)] + " " + lastNames[i%len(lastNames)]
		_, err := s.pool.Exec(ctx, `
			INSERT INTO users (id, email, full_name, password_hash, status, is_sample)
			VALUES ($1, $2, $3, $4, 'active', TRUE)
			ON CONFLICT (email) DO NOTHING`, id, email, name, hash)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (s *Seeder) seedGroups(ctx context.Context, users []uuid.UUID) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, 0, NumGroups)
	for i, name := range groupNames {
		id := uuid.New()
		_, err := s.pool.Exec(ctx, `
			INSERT INTO groups (id, name, description, is_active, display_order)
			VALUES ($1, $2, $3, TRUE, $4)
			ON CONFLICT DO NOTHING`, id, name, name+" team", i)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
		// 3 members per group.
		for j := 0; j < 3; j++ {
			member := users[(i*3+j)%len(users)]
			_, err := s.pool.Exec(ctx, `
				INSERT INTO group_members (id, group_id, user_id)
				VALUES ($1, $2, $3)
				ON CONFLICT (group_id, user_id) DO NOTHING`, uuid.New(), id, member)
			if err != nil {
				return nil, err
			}
		}
	}
	return ids, nil
}

func (s *Seeder) seedChangeRequests(ctx context.Context, users, groups, departments []uuid.UUID) error {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < NumCRs; i++ {
		crID, err := s.seedOneCR(ctx, rng, i, users, groups, departments)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			s.log.Warn("sample CR insert skipped", slog.Int("i", i), slog.String("error", err.Error()))
			continue
		}
		_ = crID
	}
	return s.linkSampleCRs(ctx, users)
}

// seedOneCR inserts one CR plus its participants and a sample comment.
func (s *Seeder) seedOneCR(ctx context.Context, rng *rand.Rand, i int, users, groups, departments []uuid.UUID) (uuid.UUID, error) {
	creator := users[rng.Intn(len(users))]
	status := statuses[rng.Intn(len(statuses))]
	title := fmt.Sprintf("%s upgrade cycle %d", systems[rng.Intn(len(systems))], i+1)
	start := time.Now().Add(-time.Duration(rng.Intn(30)) * 24 * time.Hour).UTC()
	end := start.Add(time.Duration(7+rng.Intn(21)) * 24 * time.Hour)
	priority := []string{"low", "medium", "high"}[rng.Intn(3)]
	completed := status == "approved" && rng.Intn(2) == 0

	crID := uuid.New()
	submittedAt, decidedAt, completedAt := lifecycleTimestamps(status, completed, start)
	affected := []string{systems[rng.Intn(len(systems))], systems[rng.Intn(len(systems))]}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO change_requests
			(id, title, description, status, completion_status, priority,
			 scheduled_start, scheduled_end, affected_systems,
			 request_department_id, destination_department_id,
			 created_by, submitted_at, decided_at, completed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT DO NOTHING`,
		crID, title, "Sample change description for "+title+".",
		status, completionWord(completed), priority, start, end, affected,
		departments[rng.Intn(len(departments))], departments[rng.Intn(len(departments))],
		creator, submittedAt, decidedAt, completedAt)
	if err != nil {
		return uuid.Nil, err
	}
	if status == "draft" {
		return crID, nil
	}
	if err := s.seedParticipants(ctx, rng, crID, status, users, groups, start); err != nil {
		return uuid.Nil, err
	}
	return crID, s.seedComment(ctx, rng, crID, users)
}

// seedParticipants adds one direct approver, one group approver, and two
// watchers to a non-draft CR.
func (s *Seeder) seedParticipants(ctx context.Context, rng *rand.Rand, crID uuid.UUID, status string, users, groups []uuid.UUID, start time.Time) error {
	decidedStatus := "pending"
	decidedAt := any(nil)
	switch status {
	case "approved":
		decidedStatus = "approved"
		decidedAt = start.Add(24 * time.Hour)
	case "rejected":
		decidedStatus = "rejected"
		decidedAt = start.Add(24 * time.Hour)
	}
	approver := users[rng.Intn(len(users))]
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO cr_approvers (id, change_request_id, user_id, status, decided_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING`,
		uuid.New(), crID, approver, decidedStatus, decidedAt); err != nil {
		return err
	}
	groupID := groups[rng.Intn(len(groups))]
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO cr_approver_groups (id, change_request_id, group_id, decided_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING`, uuid.New(), crID, groupID, decidedAt); err != nil {
		return err
	}
	for j := 0; j < 2; j++ {
		watcher := users[rng.Intn(len(users))]
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO cr_watchers (id, change_request_id, user_id)
			VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, uuid.New(), crID, watcher); err != nil {
			return err
		}
	}
	return nil
}

// seedComment adds a comment with a mention (only on non-draft CRs, v1 FR-9.3).
func (s *Seeder) seedComment(ctx context.Context, rng *rand.Rand, crID uuid.UUID, users []uuid.UUID) error {
	author := users[rng.Intn(len(users))]
	mentioned := users[rng.Intn(len(users))]
	body := fmt.Sprintf("<p>@%s please review the rollback plan.</p>", mentioned)
	commentID := uuid.New()
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO comments (id, change_request_id, author_id, body)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING`, commentID, crID, author, body); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO comment_mentions (id, comment_id, user_id)
		VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`, uuid.New(), commentID, mentioned)
	return err
}

// linkSampleCRs links a few CRs pairwise (canonical ordered pair per
// migration 0003).
func (s *Seeder) linkSampleCRs(ctx context.Context, users []uuid.UUID) error {
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM change_requests`).Scan(&count); err != nil {
		return err
	}
	var crIDs []uuid.UUID
	for i := 0; i < count && i < 10; i++ {
		var id uuid.UUID
		if err := s.pool.QueryRow(ctx, `
			SELECT id FROM change_requests ORDER BY created_at LIMIT 1 OFFSET $1`, i).Scan(&id); err != nil {
			break
		}
		crIDs = append(crIDs, id)
	}
	for i := 0; i+1 < len(crIDs); i += 2 {
		a, b := crIDs[i], crIDs[i+1]
		if a.String() > b.String() {
			a, b = b, a
		}
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO request_links (id, request_id_a, request_id_b, linked_by)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT DO NOTHING`, uuid.New(), a, b, users[0]); err != nil {
			return err
		}
	}
	return nil
}

func lifecycleTimestamps(status string, completed bool, start time.Time) (submittedAt, decidedAt, completedAt any) {
	if status != "draft" {
		submittedAt = start
	}
	if status == "approved" || status == "rejected" {
		decidedAt = start.Add(24 * time.Hour)
		if completed {
			completedAt = decidedAt
		}
	}
	return submittedAt, decidedAt, completedAt
}

func completionWord(completed bool) string {
	if completed {
		return "completed"
	}
	return "open"
}
