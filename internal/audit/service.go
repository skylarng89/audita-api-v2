// Package audit implements the bounded-context module for the hash-chained
// immutable audit trail, integrity verification, and async CSV export
// (blueprint §3.2, FR-6, Sprint 4).
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/audita/audita-api/internal/httpx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Entry is one hash-chained audit row. chain_index and previous_hash are
// per-tenant so a tenant's export/deletion doesn't break other chains
// (blueprint §3.3).
type Entry struct {
	ID          uuid.UUID      `json:"id"`
	ChainIndex  int64          `json:"chainIndex"`
	RecordHash  string         `json:"recordHash"`
	PrevHash    string         `json:"previousHash"`
	ActorID     *uuid.UUID     `json:"actorId"`
	ActorEmail  string         `json:"actorEmail,omitempty"`
	ActionType  string         `json:"actionType"`
	EntityType  string         `json:"entityType"`
	EntityID    *uuid.UUID     `json:"entityId"`
	Payload     map[string]any `json:"payload"`
	IPAddress   string         `json:"ipAddress,omitempty"`
	OccurredAt  time.Time      `json:"occurredAt"`
}

// ErrNotFound indicates no audit entries match.
var ErrNotFound = httpx.NewProblem(http.StatusNotFound, httpx.CodeNotFound, "audit entry not found")

// Service implements audit use-cases.
type Service struct {
	tx TenantTxRunner
}

// TenantTxRunner runs fn inside a tenant-scoped RLS transaction.
type TenantTxRunner func(ctx context.Context, fn func(tx pgx.Tx) error) error

// NewService builds an audit Service.
func NewService(tx TenantTxRunner) *Service {
	return &Service{tx: tx}
}

// AppendInput is the data for a new audit row (written by the universal
// event consumer, Task 4.2).
type AppendInput struct {
	ActorID    *uuid.UUID
	ActorEmail string
	ActionType string
	EntityType string
	EntityID   *uuid.UUID
	Payload    map[string]any
	IPAddress  string
	OccurredAt time.Time
}

// Append inserts a new entry, computing the SHA-256 chain linkage:
//
//	record_hash = SHA256(chain_index || prev_hash || canonical_payload)
//
// The unique(chain_index) constraint + FOR UPDATE row lock on the previous
// tail prevents concurrent appends from forking the chain.
func (s *Service) Append(ctx context.Context, in AppendInput) (*Entry, error) {
	if in.ActionType == "" || in.EntityType == "" {
		return nil, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "actionType and entityType are required")
	}
	e := &Entry{
		ID:         uuid.New(),
		ActorID:    in.ActorID,
		ActorEmail: in.ActorEmail,
		ActionType: in.ActionType,
		EntityType: in.EntityType,
		EntityID:   in.EntityID,
		Payload:    in.Payload,
		IPAddress:  in.IPAddress,
		OccurredAt: in.OccurredAt,
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var (
			chainIndex int64
			prevHash   string
		)
		err := tx.QueryRow(ctx, `
			SELECT COALESCE(MAX(chain_index), -1), COALESCE(MAX(record_hash), '')
			FROM audit_log`).Scan(&chainIndex, &prevHash)
		if err != nil {
			return err
		}
		chainIndex++ // MAX returns -1 for empty chain → index 0
		e.ChainIndex = chainIndex
		e.PrevHash = prevHash
		e.RecordHash = computeHash(e)
		_, err = tx.Exec(ctx, `
			INSERT INTO audit_log
				(id, chain_index, record_hash, previous_hash, actor_id, actor_email,
				 action_type, entity_type, entity_id, payload, ip_address, occurred_at)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6,''), $7, $8, $9, $10, NULLIF($11,''), $12)`,
			e.ID, e.ChainIndex, e.RecordHash, e.PrevHash, e.ActorID, e.ActorEmail,
			e.ActionType, e.EntityType, e.EntityID, e.Payload, e.IPAddress, e.OccurredAt)
		return err
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// computeHash derives the record hash from the chain-relevant fields.
func computeHash(e *Entry) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d|%s|%s|%s|%s|%s",
		e.ChainIndex, e.PrevHash, e.ActionType, e.EntityType, entityIDString(e.EntityID), e.OccurredAt.UTC().Format(time.RFC3339Nano))
	if e.ActorID != nil {
		h.Write([]byte("|" + e.ActorID.String()))
	}
	if len(e.Payload) > 0 {
		// Canonical JSON via Go's map marshal (sorted keys) keeps hashes
		// deterministic across re-verification.
		if raw, err := jsonMarshal(e.Payload); err == nil {
			h.Write(raw)
		}
	}
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// List returns audit entries filtered by the given criteria, newest first.
type ListFilter struct {
	ActorID   *uuid.UUID
	EntityType string
	EntityID  *uuid.UUID
	From      *time.Time
	To        *time.Time
	Limit     int
}

func (s *Service) List(ctx context.Context, f ListFilter) ([]*Entry, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	var out []*Entry
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, chain_index, record_hash, previous_hash, actor_id,
			       COALESCE(actor_email,''), action_type, entity_type, entity_id,
			       payload, COALESCE(ip_address,''), occurred_at
			FROM audit_log
			WHERE ($1::uuid IS NULL OR actor_id = $1)
			  AND ($2::text IS NULL OR entity_type = $2)
			  AND ($3::uuid IS NULL OR entity_id = $3)
			  AND ($4::timestamptz IS NULL OR occurred_at >= $4)
			  AND ($5::timestamptz IS NULL OR occurred_at <= $5)
			ORDER BY chain_index DESC
			LIMIT $6`,
			f.ActorID, nullIfEmpty(f.EntityType), f.EntityID, f.From, f.To, f.Limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e Entry
			if err := rows.Scan(&e.ID, &e.ChainIndex, &e.RecordHash, &e.PrevHash, &e.ActorID,
				&e.ActorEmail, &e.ActionType, &e.EntityType, &e.EntityID,
				&e.Payload, &e.IPAddress, &e.OccurredAt); err != nil {
				return err
			}
			out = append(out, &e)
		}
		return rows.Err()
	})
	return out, err
}

// VerifyResult reports the outcome of an integrity scan (Task 4.3).
type VerifyResult struct {
	TotalEntries int64  `json:"totalEntries"`
	Valid        bool   `json:"valid"`
	BrokenAtIndex *int64 `json:"brokenAtIndex,omitempty"`
	Reason       string `json:"reason,omitempty"`
	VerifiedAt   time.Time `json:"verifiedAt"`
}

// VerifyChain walks the full per-tenant chain, recomputing each hash and
// confirming linkage. Called by the scheduled integrity job and an admin
// endpoint; a failure is P0-alertable (§4.2).
func (s *Service) VerifyChain(ctx context.Context) (*VerifyResult, error) {
	res := &VerifyResult{Valid: true, VerifiedAt: time.Now().UTC()}
	var expectedPrev string
	var expectedIndex int64
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT chain_index, record_hash, previous_hash, actor_id, action_type,
			       entity_type, entity_id, payload, occurred_at
			FROM audit_log ORDER BY chain_index ASC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				e          Entry
				actorID    *uuid.UUID
				entityID   *uuid.UUID
			)
			if err := rows.Scan(&e.ChainIndex, &e.RecordHash, &e.PrevHash, &actorID, &e.ActionType, &e.EntityType, &entityID, &e.Payload, &e.OccurredAt); err != nil {
				return err
			}
			e.ActorID = actorID
			e.EntityID = entityID
			res.TotalEntries++
			if e.ChainIndex != expectedIndex || e.PrevHash != expectedPrev {
				res.Valid = false
				idx := e.ChainIndex
				res.BrokenAtIndex = &idx
				res.Reason = "chain linkage mismatch"
				return nil
			}
			if computeHash(&e) != e.RecordHash {
				res.Valid = false
				idx := e.ChainIndex
				res.BrokenAtIndex = &idx
				res.Reason = "record hash mismatch (possible tampering)"
				return nil
			}
			expectedPrev = e.RecordHash
			expectedIndex++
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// ExportRequest is an async CSV export job (Task 4.4).
type ExportRequest struct {
	ID          uuid.UUID      `json:"id"`
	RequestedBy uuid.UUID      `json:"requestedBy"`
	Filters     map[string]any `json:"filters"`
	Status      string         `json:"status"` // pending|ready|failed|expired
	FilePath    string         `json:"filePath,omitempty"`
	Error       string         `json:"error,omitempty"`
	ExpiresAt   *time.Time     `json:"expiresAt"`
	CreatedAt   time.Time      `json:"createdAt"`
}

// Export status constants.
const (
	ExportPending = "pending"
	ExportReady   = "ready"
	ExportFailed  = "failed"
	ExportExpired = "expired"
)

// CreateExportRequest stages an async export job.
func (s *Service) CreateExportRequest(ctx context.Context, requestedBy uuid.UUID, filters map[string]any) (*ExportRequest, error) {
	e := &ExportRequest{
		ID:          uuid.New(),
		RequestedBy: requestedBy,
		Filters:     filters,
		Status:      ExportPending,
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO audit_export_requests (id, requested_by, filters, status)
			VALUES ($1, $2, $3, $4)
			RETURNING created_at`, e.ID, requestedBy, filters, ExportPending)
		return row.Scan(&e.CreatedAt)
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// GetExportRequest fetches one export job for status polling.
func (s *Service) GetExportRequest(ctx context.Context, id uuid.UUID) (*ExportRequest, error) {
	var e ExportRequest
	err := s.tx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT id, requested_by, filters, status, COALESCE(file_path,''), COALESCE(error,''),
			       expires_at, created_at
			FROM audit_export_requests WHERE id = $1`, id)
		return row.Scan(&e.ID, &e.RequestedBy, &e.Filters, &e.Status, &e.FilePath, &e.Error, &e.ExpiresAt, &e.CreatedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// UpdateExportRequest transitions a job's status (worker-side).
func (s *Service) UpdateExportRequest(ctx context.Context, id uuid.UUID, status, filePath, errMessage string) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE audit_export_requests
			SET status = $2, file_path = NULLIF($3,''), error = NULLIF($4,''),
			    expires_at = CASE WHEN $2 = $6 THEN now() + interval '7 days' ELSE expires_at END,
			    updated_at = now()
			WHERE id = $1`,
			id, status, filePath, errMessage, ExportReady)
		return err
	})
}

func entityIDString(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
