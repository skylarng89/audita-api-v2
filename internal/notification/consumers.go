package notification

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/audita/audita-api/internal/platform/eventbus"
	"github.com/google/uuid"
)

// Consumers wires the notification module's NATS subscriptions (Sprint 3,
// Tasks 3.2/3.5). Each consumer is idempotent — JetStream redelivers on
// failure, so handlers must tolerate duplicates.
type Consumers struct {
	bus    *eventbus.Bus
	svc    *Service
	pool   interface {
		Query(ctx context.Context, sql string, args ...any) (Rows, error)
	}
	logger *slog.Logger
}

// Rows abstracts pgx.Rows for participant lookups.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

// NewConsumers builds the consumer set.
func NewConsumers(bus *eventbus.Bus, svc *Service, logger *slog.Logger) *Consumers {
	return &Consumers{bus: bus, svc: svc, logger: logger}
}

// Subscribe registers all notification consumers on the event bus.
func (c *Consumers) Subscribe(ctx context.Context) error {
	handlers := map[string]eventHandler{
		"cr.submitted":    c.onCRSubmitted,
		"cr.approved":     c.onCRDecided,
		"cr.rejected":     c.onCRDecided,
		"cr.completed":    c.onCRCompleted,
		"cr.cancelled":    c.onCRCancelled,
		"cr.watcher_added": c.onWatcherAdded,
		"comment.created": c.onCommentCreated,
		"mention.detected": c.onMentionDetected,
		"user.invited":    c.onUserInvited,
	}
	for eventType, handler := range handlers {
		filter := eventbus.SubjectPrefix + ".*.*." + eventTypeSubject(eventType)
		if err := c.bus.Subscribe(ctx, "notification-"+eventType, filter, handler); err != nil {
			return err
		}
	}
	// Mention watcher auto-enrollment (Task 3.5) lives in the changerequest
	// module's consumer set, not here — this handler only notifies.
	return nil
}

type eventHandler = func(ctx context.Context, ev *eventbus.Event) error

// crLinkPrefix is the frontend route prefix for CR deep-links.
const crLinkPrefix = "/change-requests/"

// crLink builds the frontend deep-link for a CR.
func crLink(crID uuid.UUID) string {
	return crLinkPrefix + crID.String()
}

// crRef is the human-readable "#N (Title)" fragment used in bodies.
func crRef(displayID int64, title string) string {
	return "Change request #" + itoa64(displayID) + " (" + title + ")"
}

// notifyUser wraps svc.Notify with consistent failure logging.
func (c *Consumers) notifyOne(ctx context.Context, recipient uuid.UUID, nType, title, body, link string) {
	if _, err := c.svc.Notify(ctx, recipient, nType, title, body, link); err != nil {
		c.logger.Warn("notify failed", slog.String("userId", recipient.String()), slog.String("error", err.Error()))
	}
}

// onCRSubmitted notifies all approvers (incl. group members) and watchers
// (v1 FR-3.3).
func (c *Consumers) onCRSubmitted(ctx context.Context, ev *eventbus.Event) error {
	payload, crID, err := decodeCRPayload(ev)
	if err != nil || crID == uuid.Nil {
		return err
	}
	recipients, err := c.participants(ctx, ev.TenantID, crID)
	if err != nil {
		return err
	}
	for _, r := range recipients {
		if r == parseOrZero(ev.ActorID) {
			continue // never notify the actor about their own action
		}
		c.notifyOne(ctx, r, TypeCRSubmitted,
			"Approval requested",
			crRef(payload.DisplayID, payload.Title)+" needs your approval.",
			crLink(crID))
	}
	return nil
}

// onCRDecided notifies watchers + requester of approve/reject outcomes.
func (c *Consumers) onCRDecided(ctx context.Context, ev *eventbus.Event) error {
	payload, crID, err := decodeCRPayload(ev)
	if err != nil || crID == uuid.Nil {
		return err
	}
	recipients, err := c.participants(ctx, ev.TenantID, crID)
	if err != nil {
		return err
	}
	title := "Change request approved"
	if ev.Type == "cr.rejected" {
		title = "Change request rejected"
	}
	for _, r := range recipients {
		if r == parseOrZero(ev.ActorID) {
			continue
		}
		c.notifyOne(ctx, r, ev.Type, title,
			crRef(payload.DisplayID, payload.Title)+" was "+statusWord(ev.Type)+".",
			crLink(crID))
	}
	return nil
}

func (c *Consumers) onCRCompleted(ctx context.Context, ev *eventbus.Event) error {
	return c.notifyParticipants(ctx, ev, "Change request completed", "was marked complete")
}

func (c *Consumers) onCRCancelled(ctx context.Context, ev *eventbus.Event) error {
	return c.notifyParticipants(ctx, ev, "Change request cancelled", "was cancelled")
}

// onWatcherAdded notifies the newly-added watcher (v1 FR-6.3 — draft adds
// are silent; the frontend filters by CR status if needed).
func (c *Consumers) onWatcherAdded(ctx context.Context, ev *eventbus.Event) error {
	var payload struct {
		ChangeRequestID string `json:"changeRequestId"`
		UserID          string `json:"userId"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return nil
	}
	uid := parseOrZero(payload.UserID)
	if uid == uuid.Nil || uid == parseOrZero(ev.ActorID) {
		return nil
	}
	_, err := c.svc.Notify(ctx, uid, TypeWatcherAdded,
		"You are now watching a change request",
		"You were added as a watcher.",
		crLinkPrefix+payload.ChangeRequestID)
	return err
}
// onCommentCreated notifies participants about new comments (v1 FR-9.3:
// only outside draft; the event is only emitted post-submit).
func (c *Consumers) onCommentCreated(ctx context.Context, ev *eventbus.Event) error {
	var payload struct {
		ChangeRequestID string   `json:"changeRequestId"`
		CommentID       string   `json:"commentId"`
		Mentions        []string `json:"mentions"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return nil
	}
	crID := parseOrZero(payload.ChangeRequestID)
	if crID == uuid.Nil {
		return nil
	}
	mentioned := map[string]struct{}{}
	for _, m := range payload.Mentions {
		mentioned[m] = struct{}{}
	}
	recipients, err := c.participants(ctx, ev.TenantID, crID)
	if err != nil {
		return err
	}
	for _, r := range recipients {
		if r == parseOrZero(ev.ActorID) {
			continue
		}
		if _, isMentioned := mentioned[r.String()]; isMentioned {
			continue // mention.detected handles those
		}
		c.notifyOne(ctx, r, TypeCommentPosted,
			"New comment",
			"A participant commented on a change request you follow.",
			crLink(crID))
	}
	return nil
}

// onMentionDetected notifies mentioned users (v1 FR-9.2).
func (c *Consumers) onMentionDetected(ctx context.Context, ev *eventbus.Event) error {
	var payload struct {
		ChangeRequestID  string   `json:"changeRequestId"`
		MentionedUserIDs []string `json:"mentionedUserIds"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return nil
	}
	for _, raw := range payload.MentionedUserIDs {
		uid := parseOrZero(raw)
		if uid == uuid.Nil || uid == parseOrZero(ev.ActorID) {
			continue
		}
		if _, err := c.svc.Notify(ctx, uid, TypeMentioned,
			"You were mentioned",
			"You were mentioned in a comment.",
			crLinkPrefix+payload.ChangeRequestID); err != nil {
			c.logger.Warn("mention notify failed", slog.String("userId", uid.String()), slog.String("error", err.Error()))
		}
	}
	return nil
}

// onUserInvited notifies via the in-app channel (email is sent separately by
// the identity module's dispatch).
func (c *Consumers) onUserInvited(ctx context.Context, ev *eventbus.Event) error {
	var payload struct {
		UserID string `json:"userId"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return nil
	}
	uid := parseOrZero(payload.UserID)
	if uid == uuid.Nil {
		return nil
	}
	_, err := c.svc.Notify(ctx, uid, TypeUserInvited,
		"Welcome to Audita",
		"Your account was created. Check your email for the invitation link.",
		"/auth/accept-invite")
	return err
}

func (c *Consumers) notifyParticipants(ctx context.Context, ev *eventbus.Event, title, verb string) error {
	payload, crID, err := decodeCRPayload(ev)
	if err != nil || crID == uuid.Nil {
		return err
	}
	recipients, err := c.participants(ctx, ev.TenantID, crID)
	if err != nil {
		return err
	}
	for _, r := range recipients {
		if r == parseOrZero(ev.ActorID) {
			continue
		}
		c.notifyOne(ctx, r, ev.Type, title,
			crRef(payload.DisplayID, payload.Title)+" "+verb+".",
			crLink(crID))
	}
	return nil
}

// crPayload is the common CR event payload shape.
type crPayload struct {
	ChangeRequestID string `json:"changeRequestId"`
	DisplayID       int64  `json:"displayId"`
	Title           string `json:"title"`
}

// decodeCRPayload unmarshals a CR event payload and parses its CR id.
func decodeCRPayload(ev *eventbus.Event) (crPayload, uuid.UUID, error) {
	var p crPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return p, uuid.Nil, nil // malformed payloads are terminated upstream
	}
	return p, parseOrZero(p.ChangeRequestID), nil
}

// participants resolves direct approvers + watchers + group members for a
// CR. The RLS session var is set per query so the pool connection is scoped.
func (c *Consumers) participants(ctx context.Context, tenantID string, crID uuid.UUID) ([]uuid.UUID, error) {
	if c.pool == nil {
		return nil, nil
	}
	if _, err := c.pool.Query(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		return nil, err
	}
	rows, err := c.pool.Query(ctx, `
		SELECT DISTINCT user_id FROM (
			SELECT user_id FROM cr_approvers  WHERE change_request_id = $1
			UNION
			SELECT user_id FROM cr_watchers   WHERE change_request_id = $1
			UNION
			SELECT gm.user_id FROM cr_approver_groups cag
			JOIN group_members gm ON gm.group_id = cag.group_id
			WHERE cag.change_request_id = $1
			UNION
			SELECT gm.user_id FROM cr_watcher_groups cwg
			JOIN group_members gm ON gm.group_id = cwg.group_id
			WHERE cwg.change_request_id = $1
		) t`, crID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func parseOrZero(s string) uuid.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil
	}
	return id
}

func statusWord(eventType string) string {
	if eventType == "cr.approved" {
		return "approved"
	}
	return "rejected"
}

// eventTypeSubject converts "cr.submitted" → "submitted" for the subject filter.
func eventTypeSubject(eventType string) string {
	for i := 0; i < len(eventType); i++ {
		if eventType[i] == '.' {
			return eventType[i+1:]
		}
	}
	return eventType
}
