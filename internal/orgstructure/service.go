// Package orgstructure implements groups, departments, and custom-field
// admin CRUD (blueprint Sprint 5, Tasks 5.2/5.3 — Groups/departments admin,
// tenant settings, custom fields are Sprint 2 Task 2.4 + Sprint 5 5.2/5.3).
package orgstructure

import (
	"context"
	"errors"
	"net/http"

	"github.com/audita/audita-api/internal/httpx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Group is a team used for approver/watcher participation.
type Group struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	IsActive    bool      `json:"isActive"`
	DisplayOrder int      `json:"displayOrder"`
	MemberCount int       `json:"memberCount"`
}

// GroupMember links a user into a group.
type GroupMember struct {
	ID       uuid.UUID `json:"id"`
	GroupID  uuid.UUID `json:"groupId"`
	UserID   uuid.UUID `json:"userId"`
	FullName string    `json:"fullName"`
	Email    string    `json:"email"`
}

// Department is an org unit referenced on CRs.
type Department struct {
	ID           uuid.UUID `json:"id"`
	Name         string    `json:"name"`
	Code         string    `json:"code,omitempty"`
	IsActive     bool      `json:"isActive"`
	DisplayOrder int       `json:"displayOrder"`
}

// CustomFieldDefinition is an admin-defined CR field.
type CustomFieldDefinition struct {
	ID           uuid.UUID `json:"id"`
	Label        string    `json:"label"`
	FieldType    string    `json:"fieldType"` // text|number|select|multiselect|date|boolean
	Options      []string  `json:"options"`
	Required     bool      `json:"required"`
	DisplayOrder int       `json:"displayOrder"`
}

// Valid field types.
var validFieldTypes = map[string]struct{}{
	"text": {}, "number": {}, "select": {}, "multiselect": {}, "date": {}, "boolean": {},
}

// ErrNotFound indicates the entity doesn't exist in the tenant.
var ErrNotFound = httpx.NewProblem(http.StatusNotFound, httpx.CodeNotFound, "not found")

// ErrNameTaken indicates a duplicate group/department name.
var ErrNameTaken = httpx.NewProblem(http.StatusConflict, httpx.CodeConflict, "name already in use")

// Service implements org-structure use-cases.
type Service struct {
	tx TenantTxRunner
}

// TenantTxRunner runs fn inside a tenant-scoped RLS transaction.
type TenantTxRunner func(ctx context.Context, fn func(tx pgx.Tx) error) error

// NewService builds an orgstructure Service.
func NewService(tx TenantTxRunner) *Service { return &Service{tx: tx} }

// ── Groups ────────────────────────────────────────────────────────────────

// ListGroups returns active groups with member counts.
func (s *Service) ListGroups(ctx context.Context) ([]*Group, error) {
	var out []*Group
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT g.id, g.name, COALESCE(g.description,''), g.is_active, g.display_order,
			       (SELECT count(*) FROM group_members gm WHERE gm.group_id = g.id) AS member_count
			FROM groups g ORDER BY g.display_order, g.name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g Group
			if err := rows.Scan(&g.ID, &g.Name, &g.Description, &g.IsActive, &g.DisplayOrder, &g.MemberCount); err != nil {
				return err
			}
			out = append(out, &g)
		}
		return rows.Err()
	})
	return out, err
}

// CreateGroup inserts a group.
func (s *Service) CreateGroup(ctx context.Context, name, description string, displayOrder int) (*Group, error) {
	if name == "" {
		return nil, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "name is required")
	}
	g := &Group{ID: uuid.New(), Name: name, Description: description, IsActive: true, DisplayOrder: displayOrder}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO groups (id, name, description, display_order)
			VALUES ($1, $2, $3, $4)
			RETURNING is_active`, g.ID, name, description, displayOrder)
		return row.Scan(&g.IsActive)
	})
	if isUniqueViolation(err) != nil {
		return nil, ErrNameTaken
	}
	if err != nil {
		return nil, err
	}
	return g, nil
}

// UpdateGroup mutates name/description/active/order.
func (s *Service) UpdateGroup(ctx context.Context, id uuid.UUID, name, description string, isActive bool, displayOrder int) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE groups SET name = $2, description = $3, is_active = $4, display_order = $5, updated_at = now()
			WHERE id = $1`, id, name, description, isActive, displayOrder)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// DeleteGroup removes a group (cascade removes memberships).
func (s *Service) DeleteGroup(ctx context.Context, id uuid.UUID) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM groups WHERE id = $1`, id)
		return err
	})
}

// AddGroupMember adds a user to a group.
func (s *Service) AddGroupMember(ctx context.Context, groupID, userID uuid.UUID) (*GroupMember, error) {
	m := &GroupMember{ID: uuid.New(), GroupID: groupID, UserID: userID}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO group_members (id, group_id, user_id)
			VALUES ($1, $2, $3)
			RETURNING (SELECT full_name FROM users WHERE id = $3),
			          (SELECT email FROM users WHERE id = $3)`,
			m.ID, groupID, userID)
		return row.Scan(&m.FullName, &m.Email)
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

// RemoveGroupMember removes a user from a group.
func (s *Service) RemoveGroupMember(ctx context.Context, groupID, userID uuid.UUID) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM group_members WHERE group_id = $1 AND user_id = $2`, groupID, userID)
		return err
	})
}

// ListGroupMembers returns a group's members.
func (s *Service) ListGroupMembers(ctx context.Context, groupID uuid.UUID) ([]*GroupMember, error) {
	var out []*GroupMember
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT gm.id, gm.group_id, gm.user_id, u.full_name, u.email
			FROM group_members gm JOIN users u ON u.id = gm.user_id
			WHERE gm.group_id = $1 ORDER BY u.full_name`, groupID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m GroupMember
			if err := rows.Scan(&m.ID, &m.GroupID, &m.UserID, &m.FullName, &m.Email); err != nil {
				return err
			}
			out = append(out, &m)
		}
		return rows.Err()
	})
	return out, err
}

// ── Departments ───────────────────────────────────────────────────────────

// ListDepartments returns departments ordered by display_order.
func (s *Service) ListDepartments(ctx context.Context) ([]*Department, error) {
	var out []*Department
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, name, COALESCE(code,''), is_active, display_order
			FROM departments ORDER BY display_order, name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d Department
			if err := rows.Scan(&d.ID, &d.Name, &d.Code, &d.IsActive, &d.DisplayOrder); err != nil {
				return err
			}
			out = append(out, &d)
		}
		return rows.Err()
	})
	return out, err
}

// CreateDepartment inserts a department.
func (s *Service) CreateDepartment(ctx context.Context, name, code string, displayOrder int) (*Department, error) {
	if name == "" {
		return nil, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "name is required")
	}
	d := &Department{ID: uuid.New(), Name: name, Code: code, IsActive: true, DisplayOrder: displayOrder}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO departments (id, name, code, display_order)
			VALUES ($1, $2, NULLIF($3,''), $4)
			RETURNING is_active`, d.ID, name, code, displayOrder)
		return row.Scan(&d.IsActive)
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

// DeleteDepartment removes a department.
func (s *Service) DeleteDepartment(ctx context.Context, id uuid.UUID) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM departments WHERE id = $1`, id)
		return err
	})
}

// ── Custom fields ─────────────────────────────────────────────────────────

// ListCustomFields returns admin-defined CR field definitions.
func (s *Service) ListCustomFields(ctx context.Context) ([]*CustomFieldDefinition, error) {
	var out []*CustomFieldDefinition
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, label, field_type, options, required, display_order
			FROM custom_field_definitions ORDER BY display_order, label`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c CustomFieldDefinition
			if err := rows.Scan(&c.ID, &c.Label, &c.FieldType, &c.Options, &c.Required, &c.DisplayOrder); err != nil {
				return err
			}
			out = append(out, &c)
		}
		return rows.Err()
	})
	return out, err
}

// CreateCustomField inserts a field definition.
func (s *Service) CreateCustomField(ctx context.Context, def *CustomFieldDefinition) (*CustomFieldDefinition, error) {
	if def.Label == "" {
		return nil, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "label is required")
	}
	if _, ok := validFieldTypes[def.FieldType]; !ok {
		return nil, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "invalid fieldType")
	}
	def.ID = uuid.New()
	err := s.tx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO custom_field_definitions (id, label, field_type, options, required, display_order)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING id`, def.ID, def.Label, def.FieldType, def.Options, def.Required, def.DisplayOrder)
		return row.Scan(&def.ID)
	})
	if err != nil {
		return nil, err
	}
	return def, nil
}

// DeleteCustomField removes a field definition (cascades CR values).
func (s *Service) DeleteCustomField(ctx context.Context, id uuid.UUID) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM custom_field_definitions WHERE id = $1`, id)
		return err
	})
}

// isUniqueViolation maps pg's 23505 to ErrNameTaken.
func isUniqueViolation(err error) error {
	if err == nil {
		return nil
	}
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" {
		return ErrNameTaken
	}
	return err
}
