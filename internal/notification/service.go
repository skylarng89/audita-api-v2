// Package notification implements the bounded-context module for in-app
// notifications, the WebSocket realtime gateway (ADR-6), and SLA breach
// monitoring (blueprint §3.2, Sprint 3).
package notification

import (
	"context"
	"net/http"
	"time"

	"github.com/audita/audita-api/internal/httpx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Notification is an in-app notification for a user.
type Notification struct {
	ID          uuid.UUID  `json:"id"`
	RecipientID uuid.UUID  `json:"recipientId"`
	Type        string     `json:"type"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	Link        string     `json:"link,omitempty"`
	IsRead      bool       `json:"isRead"`
	ReadAt      *time.Time `json:"readAt"`
	CreatedAt   time.Time  `json:"createdAt"`
}

// Common notification types emitted by event consumers.
const (
	TypeCRSubmitted   = "cr.submitted"
	TypeCRApproved    = "cr.approved"
	TypeCRRejected    = "cr.rejected"
	TypeCRCompleted   = "cr.completed"
	TypeCRCancelled   = "cr.cancelled"
	TypeCommentPosted = "comment.created"
	TypeMentioned     = "mention.detected"
	TypeWatcherAdded  = "cr.watcher_added"
	TypeUserInvited   = "user.invited"
	TypeSLAWarning    = "sla.warning"
	TypeSLABreached   = "sla.breached"
)

// ErrNotFound indicates the notification doesn't exist.
var ErrNotFound = httpx.NewProblem(http.StatusNotFound, httpx.CodeNotFound, "notification not found")

// Service persists and lists notifications; dispatch to connected clients is
// the Gateway's job.
type Service struct {
	tx      TenantTxRunner
	gateway *Gateway
}

// TenantTxRunner runs fn inside a tenant-scoped RLS transaction.
type TenantTxRunner func(ctx context.Context, fn func(tx pgx.Tx) error) error

// NewService builds a notification Service.
func NewService(tx TenantTxRunner, gateway *Gateway) *Service {
	return &Service{tx: tx, gateway: gateway}
}

// Notify persists a notification and pushes it to any connected WebSocket
// client. Recipients are resolved by the caller (consumer logic).
func (s *Service) Notify(ctx context.Context, recipientID uuid.UUID, nType, title, body, link string) (*Notification, error) {
	n := &Notification{
		ID:          uuid.New(),
		RecipientID: recipientID,
		Type:        nType,
		Title:       title,
		Body:        body,
		Link:        link,
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO notifications (id, recipient_id, type, title, body, link)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))
			RETURNING created_at`, n.ID, recipientID, nType, title, body, link)
		return row.Scan(&n.CreatedAt)
	})
	if err != nil {
		return nil, err
	}
	if s.gateway != nil {
		s.gateway.PushToUser(recipientID, n)
	}
	return n, nil
}

// List returns the user's notifications newest-first.
func (s *Service) List(ctx context.Context, recipientID uuid.UUID) ([]*Notification, error) {
	var out []*Notification
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, recipient_id, type, title, body, COALESCE(link,''), is_read, read_at, created_at
			FROM notifications WHERE recipient_id = $1 ORDER BY created_at DESC`, recipientID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n Notification
			if err := rows.Scan(&n.ID, &n.RecipientID, &n.Type, &n.Title, &n.Body, &n.Link, &n.IsRead, &n.ReadAt, &n.CreatedAt); err != nil {
				return err
			}
			out = append(out, &n)
		}
		return rows.Err()
	})
	return out, err
}

// UnreadCount returns the user's unread notification count.
func (s *Service) UnreadCount(ctx context.Context, recipientID uuid.UUID) (int, error) {
	var n int
	err := s.tx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM notifications WHERE recipient_id = $1 AND is_read = FALSE`, recipientID).Scan(&n)
	})
	return n, err
}

// MarkRead marks one notification read (idempotent).
func (s *Service) MarkRead(ctx context.Context, recipientID, id uuid.UUID) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE notifications SET is_read = TRUE, read_at = now()
			WHERE id = $1 AND recipient_id = $2 AND is_read = FALSE`, id, recipientID)
		return err
	})
}

// MarkAllRead marks every notification read for the user.
func (s *Service) MarkAllRead(ctx context.Context, recipientID uuid.UUID) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE notifications SET is_read = TRUE, read_at = now()
			WHERE recipient_id = $1 AND is_read = FALSE`, recipientID)
		return err
	})
}

// recipientCtxKey scopes the RLS transaction to a specific recipient when
// consumers write on behalf of users.
type recipientCtxKey struct{}

// WithRecipient stamps the acting recipient for tx resolution.
func WithRecipient(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, recipientCtxKey{}, id)
}

// userContext extracts the httpx user if present (bridged by composition
// root to avoid direct cross-module imports).
var userLookup = func(ctx context.Context) (uuid.UUID, bool) {
	return uuid.Nil, false
}

// UserIDFromContext is the module-level accessor.
func UserIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	return userLookup(ctx)
}

// SetUserLookup wires the extractor; called once from cmd/api.
func SetUserLookup(fn func(ctx context.Context) (uuid.UUID, bool)) {
	userLookup = fn
}

// requireActor guards handler routes.
func requireActor(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.RespondProblem(w, r, httpx.NewProblem(http.StatusUnauthorized, httpx.CodeUnauthorized, "not authenticated"))
		return uuid.Nil, false
	}
	return id, true
}
