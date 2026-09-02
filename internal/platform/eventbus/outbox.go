package eventbus

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// OutboxRelay implements the transactional-outbox pattern (blueprint §4.3):
// domain mutations insert an `outbox_events` row in the SAME Postgres
// transaction as the domain change; this relay then publishes those rows to
// NATS and marks them delivered — eliminating dual-write inconsistency
// between Postgres and NATS for audit-critical events.
type OutboxRelay struct {
	pool   *pgxpool.Pool
	bus    *Bus
	logger *slog.Logger
	batch  int
}

// NewOutboxRelay builds a relay polling the outbox table.
func NewOutboxRelay(pool *pgxpool.Pool, bus *Bus, logger *slog.Logger) *OutboxRelay {
	return &OutboxRelay{pool: pool, bus: bus, logger: logger, batch: 100}
}

// Run blocks until ctx is cancelled, polling the outbox and publishing.
// Intended to run in a goroutine supervised by cmd/api.
func (r *OutboxRelay) Run(ctx context.Context, pollInterval time.Duration) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.tick(ctx); err != nil {
				r.logger.Error("outbox relay tick failed", slog.String("error", err.Error()))
			}
		}
	}
}

type outboxRow struct {
	id    int64
	event Event
}

func (r *OutboxRelay) tick(ctx context.Context) error {
	rows := make([]outboxRow, 0, r.batch)
	err := r.pool.Acquire(ctx, func(conn *pgxpool.Conn) error {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		// Claim a batch with FOR UPDATE SKIP LOCKED so multiple relay
		// instances (if scaled) never publish the same row twice.
		dbRows, err := tx.Query(ctx, `
			SELECT id, event FROM outbox_events
			WHERE delivered_at IS NULL AND attempts < 10
			ORDER BY id
			LIMIT $1
			FOR UPDATE SKIP LOCKED`, r.batch)
		if err != nil {
			return err
		}
		for dbRows.Next() {
			var row outboxRow
			var raw []byte
			if err := dbRows.Scan(&row.id, &raw); err != nil {
				dbRows.Close()
				return err
			}
			if err := json.Unmarshal(raw, &row.event); err != nil {
				dbRows.Close()
				return err
			}
			rows = append(rows, row)
		}
		dbRows.Close()
		if err := dbRows.Err(); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := r.bus.Publish(ctx, &row.event); err != nil {
			r.logger.Warn("outbox publish failed, will retry", slog.Int64("outbox_id", row.id), slog.String("error", err.Error()))
			_, _ = r.pool.Exec(ctx,
				`UPDATE outbox_events SET attempts = attempts + 1, last_error = $2 WHERE id = $1`,
				row.id, err.Error())
			continue
		}
		_, _ = r.pool.Exec(ctx,
			`UPDATE outbox_events SET delivered_at = now() WHERE id = $1`, row.id)
	}
	return nil
}
