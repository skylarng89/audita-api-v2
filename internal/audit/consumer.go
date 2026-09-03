package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/audita/audita-api/internal/platform/eventbus"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// jsonMarshal is a seam for deterministic payload serialization.
var jsonMarshal = json.Marshal

// EventConsumer subscribes to every domain event and appends an audit entry
// per event (Task 4.2). Durable + at-least-once: the hash chain tolerates
// duplicates being filtered by the caller's idempotency (outbox event ID).
type EventConsumer struct {
	bus    *eventbus.Bus
	svc    *Service
	logger *slog.Logger
}

// NewEventConsumer builds the universal audit consumer.
func NewEventConsumer(bus *eventbus.Bus, svc *Service, logger *slog.Logger) *EventConsumer {
	return &EventConsumer{bus: bus, svc: svc, logger: logger}
}

// Subscribe attaches to `audita.>` (all domain events).
func (c *EventConsumer) Subscribe(ctx context.Context) error {
	return c.bus.Subscribe(ctx, "audit-all", eventbus.SubjectPrefix+".>", c.handle)
}

func (c *EventConsumer) handle(ctx context.Context, ev *eventbus.Event) error {
	actorID := parseID(ev.ActorID)
	var entityID *uuid.UUID
	// Best-effort entity extraction from the payload.
	var probe struct {
		ChangeRequestID string `json:"changeRequestId"`
		CommentID       string `json:"commentId"`
		UserID          string `json:"userId"`
	}
	_ = json.Unmarshal(ev.Payload, &probe)
	for _, candidate := range []string{probe.ChangeRequestID, probe.CommentID, probe.UserID} {
		if id := parseID(candidate); id != nil {
			entityID = id
			break
		}
	}
	var payload map[string]any
	if len(ev.Payload) > 0 {
		_ = json.Unmarshal(ev.Payload, &payload)
	}
	_, err := c.svc.Append(ctx, AppendInput{
		ActorID:    actorID,
		ActionType: ev.Type,
		EntityType: eventSubjectContext(ev.Type),
		EntityID:   entityID,
		Payload:    payload,
		OccurredAt: ev.Occurred,
	})
	if err != nil {
		c.logger.Error("audit append failed (will redeliver)",
			slog.String("event_id", ev.ID), slog.String("error", err.Error()))
		return err
	}
	return nil
}

// IntegrityVerifier runs the scheduled chain-verification job (Task 4.3)
// and records the metric.
type IntegrityVerifier struct {
	svc        *Service
	verifyDur  interface{ Observe(float64) }
	logger     *slog.Logger
	interval   time.Duration
}

// NewIntegrityVerifier builds the scheduled verifier.
func NewIntegrityVerifier(svc *Service, verifyDur interface{ Observe(float64) }, logger *slog.Logger) *IntegrityVerifier {
	return &IntegrityVerifier{svc: svc, verifyDur: verifyDur, logger: logger, interval: 6 * time.Hour}
}

// Run blocks until ctx is cancelled, verifying each tick. A tamper finding
// is logged at ERROR (P0 alert target per §4.2).
func (v *IntegrityVerifier) Run(ctx context.Context) {
	t := time.NewTicker(v.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			start := time.Now()
			res, err := v.svc.VerifyChain(ctx)
			if v.verifyDur != nil {
				v.verifyDur.Observe(time.Since(start).Seconds())
			}
			if err != nil {
				v.logger.Error("audit verify failed", slog.String("error", err.Error()))
				continue
			}
			if !res.Valid {
				v.logger.Error("AUDIT CHAIN TAMPERING DETECTED",
					slog.Int64("brokenAtIndex", derefInt64(res.BrokenAtIndex)),
					slog.String("reason", res.Reason))
			} else {
				v.logger.Info("audit chain verified",
					slog.Int64("entries", res.TotalEntries))
			}
		}
	}
}

// ExportWorker processes pending audit_export_requests rows into CSV files
// (Task 4.4). Runs as a background job in cmd/api.
type ExportWorker struct {
	svc       *Service
	tx        TenantTxRunner
	outDir    string
	logger    *slog.Logger
	interval  time.Duration
}

// NewExportWorker builds the CSV export worker.
func NewExportWorker(svc *Service, tx TenantTxRunner, outDir string, logger *slog.Logger) *ExportWorker {
	return &ExportWorker{svc: svc, tx: tx, outDir: outDir, logger: logger, interval: 30 * time.Second}
}

// Run blocks until ctx is cancelled, processing pending exports.
func (w *ExportWorker) Run(ctx context.Context) {
	if err := os.MkdirAll(w.outDir, 0o750); err != nil {
		w.logger.Error("export worker: mkdir failed", slog.String("error", err.Error()))
		return
	}
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.tick(ctx); err != nil {
				w.logger.Error("export worker tick failed", slog.String("error", err.Error()))
			}
		}
	}
}

func (w *ExportWorker) tick(ctx context.Context) error {
	var pendingID *uuid.UUID
	err := w.tx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT id FROM audit_export_requests
			WHERE status = $1 ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED`, ExportPending).Scan(&pendingID)
	})
	if err != nil {
		return nil // none pending or transient error; retry next tick
	}
	req, err := w.svc.GetExportRequest(ctx, *pendingID)
	if err != nil {
		return err
	}
	entries, err := w.svc.List(ctx, filtersFromMap(req.Filters))
	if err != nil {
		_ = w.svc.UpdateExportRequest(ctx, req.ID, ExportFailed, "", err.Error())
		return err
	}
	filePath := filepath.Join(w.outDir, fmt.Sprintf("audit-export-%s.csv", req.ID))
	if err := writeCSV(filePath, entries); err != nil {
		_ = w.svc.UpdateExportRequest(ctx, req.ID, ExportFailed, "", err.Error())
		return err
	}
	return w.svc.UpdateExportRequest(ctx, req.ID, ExportReady, filePath, "")
}

func filtersFromMap(m map[string]any) ListFilter {
	f := ListFilter{}
	if m == nil {
		return f
	}
	if v, ok := m["entityType"].(string); ok {
		f.EntityType = v
	}
	if v, ok := m["limit"].(float64); ok {
		f.Limit = int(v)
	}
	if v, ok := m["actorId"].(string); ok {
		if id, err := uuid.Parse(v); err == nil {
			f.ActorID = &id
		}
	}
	return f
}

func writeCSV(path string, entries []*Entry) error {
	var b strings.Builder
	b.WriteString("chain_index,occurred_at,action_type,entity_type,actor_email,entity_id\n")
	for _, e := range entries {
		b.WriteString(fmt.Sprintf("%d,%s,%s,%s,%s,%s\n",
			e.ChainIndex,
			e.OccurredAt.Format(time.RFC3339),
			csvEscape(e.ActionType),
			csvEscape(e.EntityType),
			csvEscape(e.ActorEmail),
			entityIDString(e.EntityID),
		))
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

func csvEscape(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

func parseID(s string) *uuid.UUID {
	if s == "" {
		return nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return nil
	}
	return &id
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

// eventSubjectContext returns "cr" from "cr.submitted".
func eventSubjectContext(eventType string) string {
	for i := 0; i < len(eventType); i++ {
		if eventType[i] == '.' {
			return eventType[:i]
		}
	}
	return eventType
}
