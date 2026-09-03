package changerequest

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/audita/audita-api/internal/platform/eventbus"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EventPublisher implements Publisher by appending events to the
// transactional outbox; the relay then publishes them to NATS. This is the
// durable path required for audit-critical lifecycle events (§4.3).
type EventPublisher struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// NewEventPublisher builds an outbox-backed publisher.
func NewEventPublisher(pool *pgxpool.Pool, log *slog.Logger) *EventPublisher {
	return &EventPublisher{pool: pool, log: log}
}

// PublishCR stages a CR domain event in the outbox table (same-DB insert,
// relayed to NATS by eventbus.OutboxRelay).
func (p *EventPublisher) PublishCR(ctx context.Context, event string, actorID, crID uuid.UUID, payload any) error {
	if payload == nil {
		payload = map[string]any{"changeRequestId": crID.String()}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ev, err := eventbus.NewEvent(crID.String(), event, actorID.String(), raw)
	if err != nil {
		return err
	}
	// Fix the tenant: crID is a CR id, but the envelope's TenantID should be
	// the tenant UUID for subject namespacing. The tenant is resolved by the
	// relay's RLS-scoped query, so embed tenant from context instead.
	tenantID, _ := TenantFromContext(ctx)
	if tenantID != uuid.Nil {
		ev.TenantID = tenantID.String()
	}
	out, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx,
		`INSERT INTO outbox_events (event) VALUES ($1)`, out)
	if err != nil {
		p.log.Error("cr publisher: outbox insert failed", slog.String("error", err.Error()))
	}
	return err
}
