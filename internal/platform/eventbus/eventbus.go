package eventbus

// Package eventbus wraps NATS JetStream for publishing and consuming domain
// events with trace-context propagation and an outbox relay (ADR-5, FR-4,
// blueprint §4.3).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Subject namespace (ADR-5): audita.<tenant_id>.<context>.<event>
const SubjectPrefix = "audita"

// Subject builds a namespaced event subject from its parts.
func Subject(tenantID, contextName, event string) string {
	return fmt.Sprintf("%s.%s.%s.%s", SubjectPrefix, tenantID, contextName, event)
}

// Event is the envelope published on every domain event. Consumers rely on
// the ID for idempotent deduplication (at-least-once delivery, ADR-5).
type Event struct {
	ID        string          `json:"id"`
	TenantID  string          `json:"tenantId"`
	Type      string          `json:"type"`
	ActorID   string          `json:"actorId,omitempty"`
	Occurred  time.Time       `json:"occurredAt"`
	Payload   json.RawMessage `json:"payload"`
	TraceCtx  map[string]string `json:"-"`
}

// NewEvent creates an Event envelope with a fresh UUID and UTC timestamp.
func NewEvent(tenantID, eventType, actorID string, payload any) (*Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("eventbus: marshal payload: %w", err)
	}
	return &Event{
		ID:       uuid.NewString(),
		TenantID: tenantID,
		Type:     eventType,
		ActorID:  actorID,
		Occurred: time.Now().UTC(),
		Payload:  raw,
	}, nil
}

// Bus publishes and subscribes to domain events over NATS JetStream.
type Bus struct {
	conn   *nats.Conn
	js     jetstream.JetStream
	tracer trace.Tracer
	logger *slog.Logger
}

// Connect establishes a NATS connection with JetStream enabled.
func Connect(ctx context.Context, url string, logger *slog.Logger) (*Bus, error) {
	nc, err := nats.Connect(url,
		nats.Name("audita-api"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
		nats.Timeout(10*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("eventbus: connect: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("eventbus: jetstream init: %w", err)
	}
	return &Bus{conn: nc, js: js, tracer: otel.GetTracerProvider().Tracer("audita/eventbus"), logger: logger}, nil
}

// Close drains the connection.
func (b *Bus) Close() error {
	if b == nil || b.conn == nil {
		return nil
	}
	return b.conn.Drain()
}

// EnsureStream creates (or updates) the domain-events stream. Called once at
// startup; subjects cover every module's namespace.
func (b *Bus) EnsureStream(ctx context.Context) error {
	_, err := b.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      "AUDITA_EVENTS",
		Subjects:  []string{SubjectPrefix + ".>"},
		Retention: jetstream.LimitsPolicy,
		MaxAge:    7 * 24 * time.Hour,
		Storage:   jetstream.FileStorage,
	})
	if err != nil {
		return fmt.Errorf("eventbus: ensure stream: %w", err)
	}
	return nil
}

// Publish injects trace context into message headers and publishes the event.
// Callers that need durability guarantees (audit-critical events) should use
// the outbox relay instead of calling Publish directly.
func (b *Bus) Publish(ctx context.Context, ev *Event) error {
	sub := Subject(ev.TenantID, subjectContext(ev.Type), subjectEvent(ev.Type))
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, &carrier)
	hdrs := nats.Header{}
	for k, v := range carrier {
		hdrs.Set(k, v)
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("eventbus: marshal event: %w", err)
	}
	// PublishMsgAsync(m, opts...) — no ctx (async publish returns
	// immediately); dedupe via Nats-Msg-Id.
	_, err = b.js.PublishMsgAsync(&nats.Msg{
		Subject: sub,
		Header:  hdrs,
		Data:    raw,
	}, jetstream.WithMsgID(ev.ID))
	if err != nil {
		return fmt.Errorf("eventbus: publish %s: %w", sub, err)
	}
	return nil
}

// Subscribe creates a durable pull consumer on the given subject filter and
// invokes handler for each message via Consume (the current jetstream API —
// js.Subscribe was removed in favour of consumer handles). JetStream
// redelivers un-acked messages; handler must be idempotent.
func (b *Bus) Subscribe(ctx context.Context, name, subjectFilter string, handler func(ctx context.Context, ev *Event) error) error {
	cons, err := b.js.CreateOrUpdateConsumer(ctx, "AUDITA_EVENTS", jetstream.ConsumerConfig{
		Durable:       name,
		FilterSubject: subjectFilter,
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckWait:       30 * time.Second,
		MaxDeliver:    5,
		MaxAckPending: 256,
	})
	if err != nil {
		return fmt.Errorf("eventbus: create consumer %s: %w", name, err)
	}
	cc, err := cons.Consume(func(msg jetstream.Msg) {
		var ev Event
		if err := json.Unmarshal(msg.Data(), &ev); err != nil {
			b.logger.Error("eventbus: unmarshal event failed, terminating message", slog.String("consumer", name), slog.String("error", err.Error()))
			_ = msg.Term()
			return
		}
		// Extract trace context for consumer-side spans.
		carrier := propagation.MapCarrier{}
		for k, vals := range msg.Headers() {
			if len(vals) > 0 {
				carrier.Set(k, vals[0])
			}
		}
		msgCtx := otel.GetTextMapPropagator().Extract(context.Background(), &carrier)
		msgCtx, span := b.tracer.Start(msgCtx, fmt.Sprintf("consumer.%s", name))
		defer span.End()
		if err := handler(msgCtx, &ev); err != nil {
			b.logger.Error("eventbus: handler failed, will redeliver",
				slog.String("consumer", name),
				slog.String("event_id", ev.ID),
				slog.String("error", err.Error()))
			_ = msg.NakWithDelay(5 * time.Second)
			return
		}
		_ = msg.Ack()
	})
	if err != nil {
		return fmt.Errorf("eventbus: subscribe %s: %w", name, err)
	}
	// Stop the consumer when ctx is cancelled.
	go func() {
		<-ctx.Done()
		cc.Stop()
	}()
	return nil
}

// IsConnected reports current connection health for /healthz.
func (b *Bus) IsConnected() bool { return b != nil && b.conn != nil && b.conn.IsConnected() }

func subjectContext(eventType string) string {
	// "cr.submitted" → "cr"; "user.invited" → "user"
	for i := 0; i < len(eventType); i++ {
		if eventType[i] == '.' {
			return eventType[:i]
		}
	}
	return eventType
}

func subjectEvent(eventType string) string {
	for i := 0; i < len(eventType); i++ {
		if eventType[i] == '.' {
			return eventType[i+1:]
		}
	}
	return eventType
}

// ErrNotConnected is returned by the outbox relay when NATS is unreachable.
var ErrNotConnected = errors.New("eventbus: not connected")
