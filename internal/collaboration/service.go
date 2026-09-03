// Package collaboration implements the bounded-context module for comments,
// @mentions, attachments, and the per-CR activity stream (blueprint §3.2).
package collaboration

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/audita/audita-api/internal/httpx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Comment is a sanitized-HTML comment on a CR (v1 FR-9).
type Comment struct {
	ID              uuid.UUID `json:"id"`
	ChangeRequestID uuid.UUID `json:"changeRequestId"`
	AuthorID        uuid.UUID `json:"authorId"`
	Body            string    `json:"body"`
	Mentions        []uuid.UUID `json:"mentions,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
}

// Attachment is an uploaded file on a CR.
type Attachment struct {
	ID              uuid.UUID `json:"id"`
	ChangeRequestID uuid.UUID `json:"changeRequestId"`
	UploaderID      uuid.UUID `json:"uploaderId"`
	FileName        string    `json:"fileName"`
	MimeType        string    `json:"mimeType"`
	SizeBytes       int64     `json:"sizeBytes"`
	StorageKey      string    `json:"storageKey"`
	CreatedAt       time.Time `json:"createdAt"`
}

// ActivityEntry is one row in a CR's timeline.
type ActivityEntry struct {
	ID              uuid.UUID `json:"id"`
	ChangeRequestID uuid.UUID `json:"changeRequestId"`
	ActorID         uuid.UUID `json:"actorId"`
	ActionType      string    `json:"actionType"`
	Payload         map[string]any `json:"payload"`
	OccurredAt      time.Time `json:"occurredAt"`
}

// ErrNotFound indicates the CR or comment doesn't exist in the tenant.
var ErrNotFound = httpx.NewProblem(http.StatusNotFound, httpx.CodeNotFound, "not found")

// Sanitizer abstracts HTML sanitization (bluemonday in production, no-op in
// tests) so the module has no direct security dependency.
type Sanitizer func(html string) string

// mentionPattern matches @<uuid> tokens produced by the Tiptap mention
// extension on the frontend.
var mentionPattern = regexp.MustCompile(`@([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

// ExtractMentions returns unique user IDs mentioned in a comment body.
func ExtractMentions(body string) []uuid.UUID {
	seen := map[uuid.UUID]struct{}{}
	var out []uuid.UUID
	for _, m := range mentionPattern.FindAllStringSubmatch(body, -1) {
		if id, err := uuid.Parse(m[1]); err == nil {
			if _, dup := seen[id]; !dup {
				seen[id] = struct{}{}
				out = append(out, id)
			}
		}
	}
	return out
}

// Service implements collaboration use-cases (Sprint 2, Task 2.7).
type Service struct {
	tx        TenantTxRunner
	sanitize  Sanitizer
	publish   Publisher
}

// TenantTxRunner runs fn inside a tenant-scoped RLS transaction.
type TenantTxRunner func(ctx context.Context, fn func(tx pgx.Tx) error) error

// Publisher emits collaboration events (comment.created, mention.detected).
type Publisher interface {
	PublishCollab(ctx context.Context, event string, actorID, crID uuid.UUID, payload any) error
}

// NewService builds a collaboration Service.
func NewService(tx TenantTxRunner, sanitize Sanitizer, publish Publisher) *Service {
	return &Service{tx: tx, sanitize: sanitize, publish: publish}
}

// AddComment sanitizes the body, persists the comment, records mentions, and
// emits comment.created + mention.detected events (mention watcher
// enrollment is a notification-module consumer, Sprint 3 Task 3.5).
func (s *Service) AddComment(ctx context.Context, actorID, crID uuid.UUID, rawBody string) (*Comment, error) {
	if rawBody == "" {
		return nil, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "comment body is required")
	}
	body := s.sanitize(rawBody)
	if body == "" {
		return nil, httpx.NewProblem(http.StatusBadRequest, "COMMENT_EMPTY_AFTER_SANITIZE", "comment contains no allowed content")
	}
	mentions := ExtractMentions(body)
	c := &Comment{
		ID:              uuid.New(),
		ChangeRequestID: crID,
		AuthorID:        actorID,
		Body:            body,
		Mentions:        mentions,
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM change_requests WHERE id = $1)`, crID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		row := tx.QueryRow(ctx, `
			INSERT INTO comments (id, change_request_id, author_id, body)
			VALUES ($1, $2, $3, $4)
			RETURNING created_at`, c.ID, crID, actorID, body)
		if err := row.Scan(&c.CreatedAt); err != nil {
			return err
		}
		for _, m := range mentions {
			if _, err := tx.Exec(ctx, `
				INSERT INTO comment_mentions (id, comment_id, user_id)
				VALUES ($1, $2, $3)
				ON CONFLICT (comment_id, user_id) DO NOTHING`, uuid.New(), c.ID, m); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	_ = s.publish.PublishCollab(ctx, "comment.created", actorID, crID, map[string]any{
		"commentId": c.ID, "mentions": mentions,
	})
	if len(mentions) > 0 {
		_ = s.publish.PublishCollab(ctx, "mention.detected", actorID, crID, map[string]any{
			"commentId": c.ID, "mentionedUserIds": mentions,
		})
	}
	return c, nil
}

// ListComments returns the CR comment thread oldest-first.
func (s *Service) ListComments(ctx context.Context, crID uuid.UUID) ([]*Comment, error) {
	var out []*Comment
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, change_request_id, author_id, body, created_at
			FROM comments WHERE change_request_id = $1 ORDER BY created_at`, crID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c Comment
			if err := rows.Scan(&c.ID, &c.ChangeRequestID, &c.AuthorID, &c.Body, &c.CreatedAt); err != nil {
				return err
			}
			out = append(out, &c)
		}
		return rows.Err()
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return out, err
}

// AddAttachment records upload metadata; bytes are stored via the injected
// storage Store before this call (composition root handles streaming).
func (s *Service) AddAttachment(ctx context.Context, actorID, crID uuid.UUID, fileName, mimeType string, sizeBytes int64, storageKey string) (*Attachment, error) {
	if fileName == "" || storageKey == "" {
		return nil, httpx.NewProblem(http.StatusBadRequest, httpx.CodeValidation, "fileName and storageKey are required")
	}
	a := &Attachment{
		ID:              uuid.New(),
		ChangeRequestID: crID,
		UploaderID:      actorID,
		FileName:        fileName,
		MimeType:        mimeType,
		SizeBytes:       sizeBytes,
		StorageKey:      storageKey,
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO attachments
				(id, change_request_id, uploader_id, file_name, mime_type, size_bytes, storage_key)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING created_at`,
			a.ID, crID, actorID, fileName, mimeType, sizeBytes, storageKey)
		return row.Scan(&a.CreatedAt)
	})
	if err != nil {
		return nil, err
	}
	_ = s.publish.PublishCollab(ctx, "attachment.added", actorID, crID, map[string]any{
		"attachmentId": a.ID, "fileName": fileName,
	})
	return a, nil
}

// ListAttachments returns CR attachments newest-first.
func (s *Service) ListAttachments(ctx context.Context, crID uuid.UUID) ([]*Attachment, error) {
	var out []*Attachment
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, change_request_id, uploader_id, file_name, mime_type, size_bytes, storage_key, created_at
			FROM attachments WHERE change_request_id = $1 ORDER BY created_at DESC`, crID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a Attachment
			if err := rows.Scan(&a.ID, &a.ChangeRequestID, &a.UploaderID, &a.FileName, &a.MimeType, &a.SizeBytes, &a.StorageKey, &a.CreatedAt); err != nil {
				return err
			}
			out = append(out, &a)
		}
		return rows.Err()
	})
	return out, err
}

// RecordActivity appends a timeline row (called by the activity projection
// consumer when CR/comment events arrive).
func (s *Service) RecordActivity(ctx context.Context, actorID, crID uuid.UUID, actionType string, payload map[string]any, occurredAt time.Time) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO activity_stream (id, change_request_id, actor_id, action_type, payload, occurred_at)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			uuid.New(), crID, actorID, actionType, payload, occurredAt)
		return err
	})
}

// ListActivity returns the CR timeline newest-first.
func (s *Service) ListActivity(ctx context.Context, crID uuid.UUID) ([]*ActivityEntry, error) {
	var out []*ActivityEntry
	err := s.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, change_request_id, actor_id, action_type, payload, occurred_at
			FROM activity_stream WHERE change_request_id = $1
			ORDER BY occurred_at DESC`, crID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e ActivityEntry
			if err := rows.Scan(&e.ID, &e.ChangeRequestID, &e.ActorID, &e.ActionType, &e.Payload, &e.OccurredAt); err != nil {
				return err
			}
			out = append(out, &e)
		}
		return rows.Err()
	})
	return out, err
}
