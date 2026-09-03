package collaboration

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/audita/audita-api/internal/platform/eventbus"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EventPublisher stages collaboration events in the transactional outbox
// (durable path, same as changerequest).
type EventPublisher struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// NewEventPublisher builds an outbox-backed publisher.
func NewEventPublisher(pool *pgxpool.Pool, log *slog.Logger) *EventPublisher {
	return &EventPublisher{pool: pool, log: log}
}

// PublishCollab stages an event for the relay.
func (p *EventPublisher) PublishCollab(ctx context.Context, event string, actorID, crID uuid.UUID, payload any) error {
	if payload == nil {
		payload = map[string]any{"changeRequestId": crID.String()}
	}
	ev, err := eventbus.NewEvent(crID.String(), event, actorID.String(), payload)
	if err != nil {
		return err
	}
	if tenantID, ok := TenantFromContext(ctx); ok && tenantID != uuid.Nil {
		ev.TenantID = tenantID.String()
	}
	out, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `INSERT INTO outbox_events (event) VALUES ($1)`, out)
	if err != nil {
		p.log.Error("collab publisher: outbox insert failed", slog.String("error", err.Error()))
	}
	return err
}
