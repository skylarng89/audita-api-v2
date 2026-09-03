package changerequest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/audita/audita-api/internal/httpx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Status and completion values (two-track model carried from v1).
const (
	StatusDraft           = "draft"
	StatusPendingApproval = "pending_approval"
	StatusApproved        = "approved"
	StatusRejected        = "rejected"
	StatusCancelled       = "cancelled"

	CompletionOpen      = "open"
	CompletionCompleted = "completed"

	PriorityLow    = "low"
	PriorityMedium = "medium"
	PriorityHigh   = "high"

	ApproverPending = "pending"
	ApproverApproved = "approved"
	ApproverRejected = "rejected"
)

// ChangeRequest is the core aggregate.
type ChangeRequest struct {
	ID                      uuid.UUID  `json:"id"`
	DisplayID               int64      `json:"displayId"`
	Title                   string     `json:"title"`
	Description             string     `json:"description"`
	Status                  string     `json:"status"`
	CompletionStatus        string     `json:"completionStatus"`
	Priority                string     `json:"priority"`
	ScheduledStart          *time.Time `json:"scheduledStart"`
	ScheduledEnd            *time.Time `json:"scheduledEnd"`
	AffectedSystems         []string   `json:"affectedSystems"`
	SLABreached             bool       `json:"slaBreached"`
	RequestDepartmentID     *uuid.UUID `json:"requestDepartmentId"`
	DestinationDepartmentID *uuid.UUID `json:"destinationDepartmentId"`
	RequestGroupID          *uuid.UUID `json:"requestGroupId"`
	CreatedBy               uuid.UUID  `json:"createdBy"`
	SubmittedAt             *time.Time `json:"submittedAt"`
	DecidedAt               *time.Time `json:"decidedAt"`
	CompletedAt             *time.Time `json:"completedAt"`
	CreatedAt               time.Time  `json:"createdAt"`
	UpdatedAt               time.Time  `json:"updatedAt"`
}

// Approver is a direct user approval assignment.
type Approver struct {
	ID              uuid.UUID  `json:"id"`
	ChangeRequestID uuid.UUID  `json:"changeRequestId"`
	UserID          uuid.UUID  `json:"userId"`
	Status          string     `json:"status"`
	RejectionReason string      `json:"rejectionReason,omitempty"`
	DecisionComment string      `json:"decisionComment,omitempty"`
	DecidedAt       *time.Time `json:"decidedAt"`
}

// ApproverGroup is a group-approval requirement (any-member quorum).
type ApproverGroup struct {
	ID              uuid.UUID  `json:"id"`
	ChangeRequestID uuid.UUID  `json:"changeRequestId"`
	GroupID         uuid.UUID  `json:"groupId"`
	ApprovedBy      *uuid.UUID `json:"approvedBy"`
	DecidedAt       *time.Time `json:"decidedAt"`
}

// Watcher is a user following a CR.
type Watcher struct {
	ID              uuid.UUID `json:"id"`
	ChangeRequestID uuid.UUID `json:"changeRequestId"`
	UserID          uuid.UUID `json:"userId"`
}

// WatcherGroup is a group following a CR.
type WatcherGroup struct {
	ID              uuid.UUID `json:"id"`
	ChangeRequestID uuid.UUID `json:"changeRequestId"`
	GroupID         uuid.UUID `json:"groupId"`
}

// CreateRequest is the input for creating a new draft.
type CreateRequest struct {
	Title                   string     `json:"title"`
	Description             string     `json:"description"`
	Priority                string     `json:"priority"`
	ScheduledStart          *time.Time `json:"scheduledStart"`
	ScheduledEnd            *time.Time `json:"scheduledEnd"`
	AffectedSystems         []string   `json:"affectedSystems"`
	RequestDepartmentID     *uuid.UUID `json:"requestDepartmentId"`
	DestinationDepartmentID *uuid.UUID `json:"destinationDepartmentId"`
	RequestGroupID          *uuid.UUID `json:"requestGroupId"`
}

// SubmitRequest validates a draft before transitioning to pending_approval.
type SubmitRequest struct {
	ScheduledStart *time.Time `json:"scheduledStart"`
	ScheduledEnd   *time.Time `json:"scheduledEnd"`
}

// DuplicateParticipantError mirrors v1's structured DUPLICATE_PARTICIPANT
// contract (blueprint FR-11 in v1's SRS): identifies the user, their
// existing role on the CR, and the group context when applicable.
type DuplicateParticipantError struct {
	UserID       uuid.UUID  `json:"userId"`
	ExistingRole string     `json:"existingRole"` // "approver" | "watcher"
	GroupID      *uuid.UUID `json:"groupId,omitempty"`
}

// Error implements error.
func (e *DuplicateParticipantError) Error() string {
	role := e.ExistingRole
	if e.GroupID != nil {
		return fmt.Sprintf("user %s is already a %s (via group %s)", e.UserID, role, e.GroupID)
	}
	return fmt.Sprintf("user %s is already a %s", e.UserID, role)
}

// Problem converts the domain error into the API Problem shape so handlers
// return HTTP 409 with structured details the frontend renders verbatim.
func (e *DuplicateParticipantError) Problem() *httpx.Problem {
	p := httpx.NewProblem(http.StatusConflict, httpx.CodeDuplicateParticipant, e.Error())
	details, _ := json.Marshal(e)
	var m map[string]any
	_ = json.Unmarshal(details, &m)
	p.Details = m
	return p
}

// ErrNotPermitted is the domain guard for unauthorized lifecycle actions.
var ErrNotPermitted = httpx.NewProblem(http.StatusForbidden, httpx.CodeNotPermitted, "not permitted on this change request")

// ErrInvalidTransition guards illegal state transitions.
var ErrInvalidTransition = httpx.NewProblem(http.StatusConflict, httpx.CodeInvalidTransition, "invalid status transition")

// ErrNotFound is returned when a CR doesn't exist in the tenant.
var ErrNotFound = httpx.NewProblem(http.StatusNotFound, httpx.CodeNotFound, "change request not found")

// ErrNoApprovers blocks submit when no approvers are assigned (v1 FR-3.1).
var ErrNoApprovers = httpx.NewProblem(http.StatusBadRequest, "NO_APPROVERS", "at least one approver is required before submit")

// Service implements the CR lifecycle use-cases (blueprint Sprint 2,
// tasks 2.2/2.3). All DB access flows through TenantTxRunner for RLS
// enforcement (ADR-4); side effects are emitted via eventbus (FR-4).
type Service struct {
	tx      TenantTxRunner
	publish Publisher
}

// TenantTxRunner runs fn inside a tenant-scoped RLS transaction (provided by
// the composition root).
type TenantTxRunner func(ctx context.Context, fn func(tx pgx.Tx) error) error

// Publisher emits domain events (outbox-backed in production).
type Publisher interface {
	PublishCR(ctx context.Context, event string, actorID uuid.UUID, crID uuid.UUID, payload any) error
}

// NewService builds a changerequest Service.
func NewService(tx TenantTxRunner, publish Publisher) *Service {
	return &Service{tx: tx, publish: publish}
}

// Create inserts a new draft owned by actorID.
func (s *Service) Create(ctx context.Context, actorID uuid.UUID, req CreateRequest) (*ChangeRequest, error) {
	if req.Title == "" {
		return nil, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "title is required")
	}
	if req.Priority == "" {
		req.Priority = PriorityMedium
	}
	if req.Priority != PriorityLow && req.Priority != PriorityMedium && req.Priority != PriorityHigh {
		return nil, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "invalid priority")
	}
	var cr ChangeRequest
	err := s.tx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO change_requests
				(title, description, status, completion_status, priority,
				 scheduled_start, scheduled_end, affected_systems,
				 request_department_id, destination_department_id, request_group_id, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			RETURNING id, display_id, title, description, status, completion_status, priority,
			          scheduled_start, scheduled_end, affected_systems, sla_breached,
			          request_department_id, destination_department_id, request_group_id,
			          created_by, submitted_at, decided_at, completed_at, created_at, updated_at`,
			req.Title, req.Description, StatusDraft, CompletionOpen, req.Priority,
			req.ScheduledStart, req.ScheduledEnd, req.AffectedSystems,
			req.RequestDepartmentID, req.DestinationDepartmentID, req.RequestGroupID, actorID)
		return scanCR(row, &cr)
	})
	if err != nil {
		return nil, err
	}
	return &cr, nil
}

// Get fetches a CR by ID.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (*ChangeRequest, error) {
	var cr ChangeRequest
	err := s.tx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT id, display_id, title, description, status, completion_status, priority,
			       scheduled_start, scheduled_end, affected_systems, sla_breached,
			       request_department_id, destination_department_id, request_group_id,
			       created_by, submitted_at, decided_at, completed_at, created_at, updated_at
			FROM change_requests WHERE id = $1`, id)
		return scanCR(row, &cr)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &cr, nil
}

// List returns CRs ordered newest-first, optionally filtered by status.
func (s *Service) List(ctx context.Context, status string) ([]*ChangeRequest, error) {
	var out []*ChangeRequest
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var (
			rows pgx.Rows
			err  error
		)
		if status != "" {
			rows, err = tx.Query(ctx, `
				SELECT id, display_id, title, description, status, completion_status, priority,
				       scheduled_start, scheduled_end, affected_systems, sla_breached,
				       request_department_id, destination_department_id, request_group_id,
				       created_by, submitted_at, decided_at, completed_at, created_at, updated_at
				FROM change_requests WHERE status = $1 ORDER BY created_at DESC`, status)
		} else {
			rows, err = tx.Query(ctx, `
				SELECT id, display_id, title, description, status, completion_status, priority,
				       scheduled_start, scheduled_end, affected_systems, sla_breached,
				       request_department_id, destination_department_id, request_group_id,
				       created_by, submitted_at, decided_at, completed_at, created_at, updated_at
				FROM change_requests ORDER BY created_at DESC`)
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var cr ChangeRequest
			if err := scanCR(rows, &cr); err != nil {
				return err
			}
			out = append(out, &cr)
		}
		return rows.Err()
	})
	return out, err
}

// EditWhitelist is the field set the requester may modify post-submission
// (v1 FR-4.1/4.5: title and scope locked; enforced server-side here).
func isEditablePostSubmit(req *CreateRequest) bool {
	return req.Description != "" || req.ScheduledStart != nil || req.ScheduledEnd != nil ||
		len(req.AffectedSystems) > 0 || req.RequestDepartmentID != nil ||
		req.DestinationDepartmentID != nil
}

// Update mutates a CR. Drafts accept full edits; submitted CRs only accept
// whitelisted fields and preserve existing approvals (v1 FR-4.3).
func (s *Service) Update(ctx context.Context, actorID uuid.UUID, id uuid.UUID, req CreateRequest) (*ChangeRequest, error) {
	var cr ChangeRequest
	err := s.tx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT id, display_id, title, description, status, completion_status, priority,
			       scheduled_start, scheduled_end, affected_systems, sla_breached,
			       request_department_id, destination_department_id, request_group_id,
			       created_by, submitted_at, decided_at, completed_at, created_at, updated_at
			FROM change_requests WHERE id = $1 FOR UPDATE`, id)
		if err := scanCR(row, &cr); err != nil {
			return err
		}
		if cr.CreatedBy != actorID {
			return ErrNotPermitted
		}
		if cr.Status == StatusDraft {
			_, err := tx.Exec(ctx, `
				UPDATE change_requests SET
					title = $2, description = $3, priority = $4,
					scheduled_start = $5, scheduled_end = $6, affected_systems = $7,
					request_department_id = $8, destination_department_id = $9,
					request_group_id = $10, updated_at = now()
				WHERE id = $1`,
				id, req.Title, req.Description, req.Priority,
				req.ScheduledStart, req.ScheduledEnd, req.AffectedSystems,
				req.RequestDepartmentID, req.DestinationDepartmentID, req.RequestGroupID)
			return err
		}
		if cr.Status != StatusPendingApproval {
			return ErrInvalidTransition
		}
		// Whitelist enforcement: reject attempts to change locked fields.
		if req.Title != "" && req.Title != cr.Title {
			return httpx.NewProblem(http.StatusBadRequest, "TITLE_LOCKED", "title cannot be changed after submission")
		}
		if req.Priority != "" && req.Priority != cr.Priority {
			return httpx.NewProblem(http.StatusBadRequest, "PRIORITY_LOCKED", "priority cannot be changed after submission")
		}
		if !isEditablePostSubmit(&req) {
			return httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "no editable fields supplied")
		}
		_, err := tx.Exec(ctx, `
			UPDATE change_requests SET
				description = COALESCE(NULLIF($2, ''), description),
				scheduled_start = COALESCE($3, scheduled_start),
				scheduled_end = COALESCE($4, scheduled_end),
				affected_systems = CASE WHEN cardinality($5::text[]) > 0 THEN $5 ELSE affected_systems END,
				request_department_id = COALESCE($6, request_department_id),
				destination_department_id = COALESCE($7, destination_department_id),
				updated_at = now()
			WHERE id = $1`,
			id, req.Description, req.ScheduledStart, req.ScheduledEnd, req.AffectedSystems,
			req.RequestDepartmentID, req.DestinationDepartmentID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// Submit transitions draft → pending_approval, applying v1's default date
// rules (FR-1.3/1.4: blank start = now; blank end = start + 14 days) and
// requiring at least one approver (FR-3.1).
func (s *Service) Submit(ctx context.Context, actorID uuid.UUID, id uuid.UUID) (*ChangeRequest, error) {
	var cr ChangeRequest
	err := s.tx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT id, display_id, title, description, status, completion_status, priority,
			       scheduled_start, scheduled_end, affected_systems, sla_breached,
			       request_department_id, destination_department_id, request_group_id,
			       created_by, submitted_at, decided_at, completed_at, created_at, updated_at
			FROM change_requests WHERE id = $1 FOR UPDATE`, id)
		if err := scanCR(row, &cr); err != nil {
			return err
		}
		if err := assertSubmittable(&cr, actorID); err != nil {
			return err
		}
		if err := assertHasApprovers(ctx, tx, id); err != nil {
			return err
		}
		start, end := defaultScheduledWindow(cr.ScheduledStart, cr.ScheduledEnd)
		_, err := tx.Exec(ctx, `
			UPDATE change_requests SET
				status = $2, scheduled_start = $3, scheduled_end = $4, submitted_at = now(), updated_at = now()
			WHERE id = $1`, id, StatusPendingApproval, start, end)
		if err != nil {
			return err
		}
		cr.Status = StatusPendingApproval
		cr.ScheduledStart = start
		cr.ScheduledEnd = end
		return nil
	})
	if err != nil {
		return nil, err
	}
	_ = s.publish.PublishCR(ctx, "cr.submitted", actorID, id, map[string]any{"displayId": cr.DisplayID})
	return &cr, nil
}

// Decide applies an approver's decision and evaluates quorum: the CR is
// approved when every direct approver has approved AND every approver-group
// has at least one member approval (v1 FR-5.4 any-member semantics).
func (s *Service) Decide(ctx context.Context, actorID, crID, approverID uuid.UUID, approve bool, reason, comment string) (*ChangeRequest, error) {
	if !approve && reason == "" {
		return nil, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "rejectionReason is required when rejecting")
	}
	newStatus := ApproverRejected
	if approve {
		newStatus = ApproverApproved
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE cr_approvers SET status = $3, rejection_reason = $4, decision_comment = $5,
			       decided_at = now(), updated_at = now()
			WHERE id = $1 AND change_request_id = $2 AND user_id = $6 AND status = 'pending'`,
			approverID, crID, newStatus, nullIfEmpty(reason), nullIfEmpty(comment), actorID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotPermitted
		}
		return evaluateApprovalClosure(ctx, tx, crID)
	})
	if err != nil {
		return nil, err
	}
	event := "cr.rejected"
	if approve {
		event = "cr.approved"
	}
	_ = s.publish.PublishCR(ctx, event, actorID, crID, nil)
	return s.Get(ctx, crID)
}

// GroupApprove records a group member's approval; first member wins the
// approved_by slot (v1 FR-5.4).
func (s *Service) GroupApprove(ctx context.Context, actorID, crID, groupID uuid.UUID, approve bool, reason string) (*ChangeRequest, error) {
	if !approve && reason == "" {
		return nil, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "rejectionReason is required when rejecting")
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := assertGroupMember(ctx, tx, crID, groupID, actorID); err != nil {
			return err
		}
		if err := applyGroupDecision(ctx, tx, crID, groupID, actorID, approve, reason); err != nil {
			return err
		}
		return evaluateApprovalClosure(ctx, tx, crID)
	})
	if err != nil {
		return nil, err
	}
	event := "cr.rejected"
	if approve {
		event = "cr.approved"
	}
	_ = s.publish.PublishCR(ctx, event, actorID, crID, nil)
	return s.Get(ctx, crID)
}

// assertGroupMember verifies the actor belongs to the approver group on this CR.
func assertGroupMember(ctx context.Context, tx pgx.Tx, crID, groupID, actorID uuid.UUID) error {
	var isMember bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM group_members gm
			JOIN cr_approver_groups cag ON cag.group_id = gm.group_id
			WHERE cag.change_request_id = $1 AND cag.group_id = $2 AND gm.user_id = $3
		)`, crID, groupID, actorID).Scan(&isMember); err != nil {
		return err
	}
	if !isMember {
		return ErrNotPermitted
	}
	return nil
}

// applyGroupDecision records the group outcome: approval claims the
// approved_by slot; rejection marks the group decided and adds a synthetic
// rejected approver row so quorum logic sees a decided-negative group.
func applyGroupDecision(ctx context.Context, tx pgx.Tx, crID, groupID, actorID uuid.UUID, approve bool, reason string) error {
	if approve {
		_, err := tx.Exec(ctx, `
			UPDATE cr_approver_groups SET approved_by = $3, decided_at = now()
			WHERE id = $1 AND approved_by IS NULL AND decided_at IS NULL`, groupID, crID, actorID)
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE cr_approver_groups SET decided_at = now()
		WHERE change_request_id = $1 AND id = $2 AND decided_at IS NULL`, crID, groupID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO cr_approvers (id, change_request_id, user_id, status, rejection_reason, decided_at)
		VALUES ($2, $1, $3, 'rejected', $4, now())
		ON CONFLICT (change_request_id, user_id) DO NOTHING`, crID, uuid.New(), actorID, reason)
	return err
}

// evaluateApprovalClosure flips the CR to approved/rejected once quorum is
// met. Called inside the tx so decision + status are atomic.
func evaluateApprovalClosure(ctx context.Context, tx pgx.Tx, crID uuid.UUID) error {
	var (
		pendingDirect int
		rejectedAny   int
		pendingGroups int
	)
	if err := tx.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM cr_approvers WHERE change_request_id = $1 AND status = 'pending'),
			(SELECT count(*) FROM cr_approvers WHERE change_request_id = $1 AND status = 'rejected'),
			(SELECT count(*) FROM cr_approver_groups WHERE change_request_id = $1 AND decided_at IS NULL)
		`, crID).Scan(&pendingDirect, &rejectedAny, &pendingGroups); err != nil {
		return err
	}
	switch {
	case rejectedAny > 0:
		_, err := tx.Exec(ctx, `
			UPDATE change_requests SET status = $2, decided_at = now(), updated_at = now()
			WHERE id = $1 AND status = 'pending_approval'`, crID, StatusRejected)
		return err
	case pendingDirect == 0 && pendingGroups == 0:
		_, err := tx.Exec(ctx, `
			UPDATE change_requests SET status = $2, decided_at = now(), updated_at = now()
			WHERE id = $1 AND status = 'pending_approval'`, crID, StatusApproved)
		return err
	}
	return nil
}

// Complete transitions approved → completed (requester only, v1 FR-8).
func (s *Service) Complete(ctx context.Context, actorID, id uuid.UUID) (*ChangeRequest, error) {
	err := s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE change_requests SET
				completion_status = $2, completed_at = now(), updated_at = now()
			WHERE id = $1 AND status = $3 AND created_by = $4 AND completion_status = 'open'`,
			id, CompletionCompleted, StatusApproved, actorID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrInvalidTransition
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	_ = s.publish.PublishCR(ctx, "cr.completed", actorID, id, nil)
	return s.Get(ctx, id)
}

// Cancel transitions draft/pending_approval → cancelled (requester only).
func (s *Service) Cancel(ctx context.Context, actorID, id uuid.UUID) (*ChangeRequest, error) {
	err := s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE change_requests SET
				status = $2, cancelled_at = now(), updated_at = now()
			WHERE id = $1 AND status IN ($3, $4) AND created_by = $5`,
			id, StatusCancelled, StatusDraft, StatusPendingApproval, actorID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrInvalidTransition
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	_ = s.publish.PublishCR(ctx, "cr.cancelled", actorID, id, nil)
	return s.Get(ctx, id)
}

// AddApprover assigns a direct approver; duplicate-participant detection
// returns the structured error (v1 FR-11).
func (s *Service) AddApprover(ctx context.Context, actorID, crID, userID uuid.UUID) (*Approver, error) {
	if err := s.ensureNotDuplicate(ctx, crID, userID); err != nil {
		return nil, err
	}
	if err := s.assertParticipantsEditable(ctx, crID); err != nil {
		return nil, err
	}
	var ap Approver
	err := s.tx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO cr_approvers (id, change_request_id, user_id, status)
			VALUES ($1, $2, $3, 'pending')
			RETURNING id, change_request_id, user_id, status, rejection_reason, decision_comment, decided_at`,
			uuid.New(), crID, userID)
		return scanApprover(row, &ap)
	})
	if err != nil {
		return nil, err
	}
	_ = s.publish.PublishCR(ctx, "cr.approver_added", actorID, crID, map[string]any{"userId": userID})
	return &ap, nil
}

// AddApproverGroup assigns a group approver.
func (s *Service) AddApproverGroup(ctx context.Context, actorID, crID, groupID uuid.UUID) (*ApproverGroup, error) {
	if err := s.assertParticipantsEditable(ctx, crID); err != nil {
		return nil, err
	}
	var ag ApproverGroup
	err := s.tx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO cr_approver_groups (id, change_request_id, group_id)
			VALUES ($1, $2, $3)
			RETURNING id, change_request_id, group_id, approved_by, decided_at`,
			uuid.New(), crID, groupID)
		return scanApproverGroup(row, &ag)
	})
	if err != nil {
		return nil, err
	}
	_ = s.publish.PublishCR(ctx, "cr.approver_added", actorID, crID, map[string]any{"groupId": groupID})
	return &ag, nil
}

// AddWatcher assigns a user watcher.
func (s *Service) AddWatcher(ctx context.Context, actorID, crID, userID uuid.UUID) (*Watcher, error) {
	if err := s.ensureNotDuplicate(ctx, crID, userID); err != nil {
		return nil, err
	}
	var w Watcher
	err := s.tx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO cr_watchers (id, change_request_id, user_id)
			VALUES ($1, $2, $3)
			RETURNING id, change_request_id, user_id`,
			uuid.New(), crID, userID)
		return scanWatcher(row, &w)
	})
	if err != nil {
		return nil, err
	}
	_ = s.publish.PublishCR(ctx, "cr.watcher_added", actorID, crID, map[string]any{"userId": userID})
	return &w, nil
}

// AddWatcherGroup assigns a group watcher.
func (s *Service) AddWatcherGroup(ctx context.Context, actorID, crID, groupID uuid.UUID) (*WatcherGroup, error) {
	var wg WatcherGroup
	err := s.tx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO cr_watcher_groups (id, change_request_id, group_id)
			VALUES ($1, $2, $3)
			RETURNING id, change_request_id, group_id`,
			uuid.New(), crID, groupID)
		return scanWatcherGroup(row, &wg)
	})
	if err != nil {
		return nil, err
	}
	_ = s.publish.PublishCR(ctx, "cr.watcher_added", actorID, crID, map[string]any{"groupId": groupID})
	return &wg, nil
}

// RemoveApprover / RemoveWatcher delete direct assignments.
func (s *Service) RemoveApprover(ctx context.Context, crID, approverID uuid.UUID) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM cr_approvers WHERE id = $1 AND change_request_id = $2`, approverID, crID)
		return err
	})
}

func (s *Service) RemoveWatcher(ctx context.Context, crID, watcherID uuid.UUID) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM cr_watchers WHERE id = $1 AND change_request_id = $2`, watcherID, crID)
		return err
	})
}

// PromoteWatcherToApprover switches a watcher into an approver (v1 FR-7.3).
func (s *Service) PromoteWatcherToApprover(ctx context.Context, crID, watcherID uuid.UUID) (*Approver, error) {
	var ap Approver
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var userID uuid.UUID
		if err := tx.QueryRow(ctx, `DELETE FROM cr_watchers WHERE id = $1 AND change_request_id = $2 RETURNING user_id`, watcherID, crID).Scan(&userID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.NewProblem(http.StatusNotFound, httpx.CodeNotFound, "watcher not found")
			}
			return err
		}
		row := tx.QueryRow(ctx, `
			INSERT INTO cr_approvers (id, change_request_id, user_id, status)
			VALUES ($1, $2, $3, 'pending')
			RETURNING id, change_request_id, user_id, status, rejection_reason, decision_comment, decided_at`,
			uuid.New(), crID, userID)
		return scanApprover(row, &ap)
	})
	return &ap, err
}

// DemoteApproverToWatcher switches an approver into a watcher (v1 FR-7.3).
func (s *Service) DemoteApproverToWatcher(ctx context.Context, crID, approverID uuid.UUID) (*Watcher, error) {
	var w Watcher
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var userID uuid.UUID
		if err := tx.QueryRow(ctx, `DELETE FROM cr_approvers WHERE id = $1 AND change_request_id = $2 RETURNING user_id`, approverID, crID).Scan(&userID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.NewProblem(http.StatusNotFound, httpx.CodeNotFound, "approver not found")
			}
			return err
		}
		row := tx.QueryRow(ctx, `
			INSERT INTO cr_watchers (id, change_request_id, user_id)
			VALUES ($1, $2, $3)
			RETURNING id, change_request_id, user_id`,
			uuid.New(), crID, userID)
		return scanWatcher(row, &w)
	})
	return &w, err
}

// ApproverCandidates lists tenant users eligible as approvers, excluding
// those already on the CR (v1 approver-candidates lookup).
func (s *Service) ApproverCandidates(ctx context.Context, crID uuid.UUID) ([]map[string]any, error) {
	var out []map[string]any
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT u.id, u.full_name, u.email
			FROM users u
			WHERE u.status = 'active'
			  AND u.id NOT IN (SELECT user_id FROM cr_approvers WHERE change_request_id = $1)
			  AND u.id NOT IN (SELECT user_id FROM cr_watchers  WHERE change_request_id = $1)
			ORDER BY u.full_name`, crID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			var name, email string
			if err := rows.Scan(&id, &name, &email); err != nil {
				return err
			}
			out = append(out, map[string]any{"id": id, "fullName": name, "email": email})
		}
		return rows.Err()
	})
	return out, err
}

// ListApprovers / ListApprovers / ListWatcherGroups expose participants.
func (s *Service) ListApprovers(ctx context.Context, crID uuid.UUID) ([]*Approver, error) {
	var out []*Approver
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, change_request_id, user_id, status, COALESCE(rejection_reason,''),
			       COALESCE(decision_comment,''), decided_at
			FROM cr_approvers WHERE change_request_id = $1 ORDER BY created_at`, crID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a Approver
			if err := rows.Scan(&a.ID, &a.ChangeRequestID, &a.UserID, &a.Status, &a.RejectionReason, &a.DecisionComment, &a.DecidedAt); err != nil {
				return err
			}
			out = append(out, &a)
		}
		return rows.Err()
	})
	return out, err
}

func (s *Service) ListApproversGroups(ctx context.Context, crID uuid.UUID) ([]*ApproverGroup, error) {
	var out []*ApproverGroup
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, change_request_id, group_id, approved_by, decided_at
			FROM cr_approver_groups WHERE change_request_id = $1`, crID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g ApproverGroup
			if err := rows.Scan(&g.ID, &g.ChangeRequestID, &g.GroupID, &g.ApprovedBy, &g.DecidedAt); err != nil {
				return err
			}
			out = append(out, &g)
		}
		return rows.Err()
	})
	return out, err
}

func (s *Service) ListWatchers(ctx context.Context, crID uuid.UUID) ([]*Watcher, error) {
	var out []*Watcher
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, change_request_id, user_id FROM cr_watchers
			WHERE change_request_id = $1 ORDER BY created_at`, crID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var w Watcher
			if err := rows.Scan(&w.ID, &w.ChangeRequestID, &w.UserID); err != nil {
				return err
			}
			out = append(out, &w)
		}
		return rows.Err()
	})
	return out, err
}

func (s *Service) ListWatcherGroups(ctx context.Context, crID uuid.UUID) ([]*WatcherGroup, error) {
	var out []*WatcherGroup
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, change_request_id, group_id FROM cr_watcher_groups
			WHERE change_request_id = $1`, crID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g WatcherGroup
			if err := rows.Scan(&g.ID, &g.ChangeRequestID, &g.GroupID); err != nil {
				return err
			}
			out = append(out, &g)
		}
		return rows.Err()
	})
	return out, err
}

// ensureNotDuplicate returns the structured DuplicateParticipantError when
// the user already participates in any role on the CR (v1 FR-11).
func (s *Service) ensureNotDuplicate(ctx context.Context, crID, userID uuid.UUID) error {
	var p participantState
	err := s.tx(ctx, func(tx pgx.Tx) error {
		return p.load(ctx, tx, crID, userID)
	})
	if err != nil {
		return err
	}
	return p.duplicateError(userID)
}

// participantState captures every way a user may already participate in a CR.
type participantState struct {
	isApprover  bool
	isWatcher   bool
	approverGrp *uuid.UUID
	watcherGrp  *uuid.UUID
}

// load probes direct and group-level participation inside one tx.
func (p *participantState) load(ctx context.Context, tx pgx.Tx, crID, userID uuid.UUID) error {
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM cr_approvers WHERE change_request_id = $1 AND user_id = $2)`, crID, userID).Scan(&p.isApprover); err != nil {
		return err
	}
	if !p.isApprover {
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM cr_watchers WHERE change_request_id = $1 AND user_id = $2)`, crID, userID).Scan(&p.isWatcher); err != nil {
			return err
		}
	}
	if p.isApprover || p.isWatcher {
		return nil
	}
	if err := scanOptionalGroup(ctx, tx, `
		SELECT cag.group_id FROM cr_approver_groups cag
		JOIN group_members gm ON gm.group_id = cag.group_id
		WHERE cag.change_request_id = $1 AND gm.user_id = $2 LIMIT 1`,
		crID, userID, &p.approverGrp); err != nil {
		return err
	}
	if p.approverGrp == nil {
		return scanOptionalGroup(ctx, tx, `
			SELECT cwg.group_id FROM cr_watcher_groups cwg
			JOIN group_members gm ON gm.group_id = cwg.group_id
			WHERE cwg.change_request_id = $1 AND gm.user_id = $2 LIMIT 1`,
			crID, userID, &p.watcherGrp)
	}
	return nil
}

// duplicateError converts the participation state into the structured error
// (v1 FR-11) or nil when the user is new to the CR.
func (p *participantState) duplicateError(userID uuid.UUID) error {
	switch {
	case p.isApprover:
		return &DuplicateParticipantError{UserID: userID, ExistingRole: "approver"}
	case p.isWatcher:
		return &DuplicateParticipantError{UserID: userID, ExistingRole: "watcher"}
	case p.approverGrp != nil:
		return &DuplicateParticipantError{UserID: userID, ExistingRole: "approver", GroupID: p.approverGrp}
	case p.watcherGrp != nil:
		return &DuplicateParticipantError{UserID: userID, ExistingRole: "watcher", GroupID: p.watcherGrp}
	}
	return nil
}

// scanOptionalGroup scans a nullable group_id, tolerating ErrNoRows.
func scanOptionalGroup(ctx context.Context, tx pgx.Tx, query string, crID, userID uuid.UUID, dest **uuid.UUID) error {
	err := tx.QueryRow(ctx, query, crID, userID).Scan(dest)
	if err != nil && errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

// assertParticipantsEditable blocks approver additions outside
// draft/pending_approval (v1 FR-5.2).
func (s *Service) assertParticipantsEditable(ctx context.Context, crID uuid.UUID) error {
	var status string
	err := s.tx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM change_requests WHERE id = $1`, crID).Scan(&status)
	})
	if err != nil {
		return err
	}
	if status != StatusDraft && status != StatusPendingApproval {
		return httpx.NewProblem(http.StatusConflict, httpx.CodeInvalidTransition,
			"approvers can only be added in draft or pending_approval")
	}
	return nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// assertSubmittable guards the draft→pending_approval transition (owner-only,
// draft-only) per v1 FR-2/FR-3.
func assertSubmittable(cr *ChangeRequest, actorID uuid.UUID) error {
	if cr.Status != StatusDraft {
		return ErrInvalidTransition
	}
	if cr.CreatedBy != actorID {
		return ErrNotPermitted
	}
	return nil
}

// assertHasApprovers requires at least one direct or group approver (v1 FR-3.1).
func assertHasApprovers(ctx context.Context, tx pgx.Tx, crID uuid.UUID) error {
	var approverCount, groupCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM cr_approvers WHERE change_request_id = $1`, crID).Scan(&approverCount); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM cr_approver_groups WHERE change_request_id = $1`, crID).Scan(&groupCount); err != nil {
		return err
	}
	if approverCount == 0 && groupCount == 0 {
		return ErrNoApprovers
	}
	return nil
}

// defaultScheduledWindow applies v1 FR-1.3/1.4: blank start = now, blank
// end = start + 14 days.
func defaultScheduledWindow(start, end *time.Time) (*time.Time, *time.Time) {
	if start == nil {
		now := time.Now().UTC()
		start = &now
	}
	if end == nil {
		plus14 := start.Add(14 * 24 * time.Hour)
		end = &plus14
	}
	return start, end
}

// scanCR maps a CR row from either pgx.Row or pgx.Rows.
func scanCR(row interface{ Scan(dest ...any) error }, cr *ChangeRequest) error {
	return row.Scan(&cr.ID, &cr.DisplayID, &cr.Title, &cr.Description, &cr.Status, &cr.CompletionStatus,
		&cr.Priority, &cr.ScheduledStart, &cr.ScheduledEnd, &cr.AffectedSystems, &cr.SLABreached,
		&cr.RequestDepartmentID, &cr.DestinationDepartmentID, &cr.RequestGroupID,
		&cr.CreatedBy, &cr.SubmittedAt, &cr.DecidedAt, &cr.CompletedAt, &cr.CreatedAt, &cr.UpdatedAt)
}

func scanApprover(row interface{ Scan(dest ...any) error }, a *Approver) error {
	return row.Scan(&a.ID, &a.ChangeRequestID, &a.UserID, &a.Status, &a.RejectionReason, &a.DecisionComment, &a.DecidedAt)
}

func scanApproverGroup(row interface{ Scan(dest ...any) error }, g *ApproverGroup) error {
	return row.Scan(&g.ID, &g.ChangeRequestID, &g.GroupID, &g.ApprovedBy, &g.DecidedAt)
}

func scanWatcher(row interface{ Scan(dest ...any) error }, w *Watcher) error {
	return row.Scan(&w.ID, &w.ChangeRequestID, &w.UserID)
}

func scanWatcherGroup(row interface{ Scan(dest ...any) error }, g *WatcherGroup) error {
	return row.Scan(&g.ID, &g.ChangeRequestID, &g.GroupID)
}
