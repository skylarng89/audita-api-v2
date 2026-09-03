package notification

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SLAMonitor scans pending-approval CRs whose scheduled_end is approaching
// (warning) or passed (breach), and emits notifications to the requester
// (v1 FR-10: SLA notifications go to the requester only).
type SLAMonitor struct {
	pool       *pgxpool.Pool
	svc        *Service
	warnBefore time.Duration
	interval   time.Duration
	logger     *slog.Logger
}

// NewSLAMonitor builds the scheduler. warnBefore is the org-configurable
// SLA_WARN_HOURS_BEFORE window (v1 FR-10.3).
func NewSLAMonitor(pool *pgxpool.Pool, svc *Service, warnBefore time.Duration, logger *slog.Logger) *SLAMonitor {
	if warnBefore <= 0 {
		warnBefore = 24 * time.Hour
	}
	return &SLAMonitor{
		pool:       pool,
		svc:        svc,
		warnBefore: warnBefore,
		interval:   time.Minute, // blueprint §4.2: 60s scheduler tick
		logger:     logger,
	}
}

// Run blocks until ctx is cancelled, scanning each tick. Intended as a
// BackgroundJob in cmd/api.
func (m *SLAMonitor) Run(ctx context.Context) {
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := m.tick(ctx); err != nil {
				m.logger.Error("sla scan failed", slog.String("error", err.Error()))
			}
		}
	}
}

type slaRow struct {
	crID          uuid.UUID
	createdBy     uuid.UUID
	scheduledEnd  time.Time
	slaBreached   bool
	displayID     int64
	title         string
}

func (m *SLAMonitor) tick(ctx context.Context) error {
	targets, err := m.pendingTargets(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, t := range targets {
		if err := m.processTarget(ctx, t, now); err != nil {
			return err
		}
	}
	return nil
}

// pendingTargets loads pending-approval CRs with a scheduled end.
func (m *SLAMonitor) pendingTargets(ctx context.Context) ([]slaRow, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT id, created_by, scheduled_end, sla_breached, display_id, title
		FROM change_requests
		WHERE status = 'pending_approval' AND scheduled_end IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []slaRow
	for rows.Next() {
		var r slaRow
		if err := rows.Scan(&r.crID, &r.createdBy, &r.scheduledEnd, &r.slaBreached, &r.displayID, &r.title); err != nil {
			return nil, err
		}
		targets = append(targets, r)
	}
	return targets, rows.Err()
}

// processTarget emits a breach or warning notification for one CR.
func (m *SLAMonitor) processTarget(ctx context.Context, t slaRow, now time.Time) error {
	switch {
	case now.After(t.scheduledEnd) && !t.slaBreached:
		return m.flagBreach(ctx, t)
	case !t.slaBreached && now.After(t.scheduledEnd.Add(-m.warnBefore)):
		return m.warnIfFirst(ctx, t)
	}
	return nil
}

// flagBreach marks the CR breached and notifies the requester.
func (m *SLAMonitor) flagBreach(ctx context.Context, t slaRow) error {
	if _, err := m.pool.Exec(ctx,
		`UPDATE change_requests SET sla_breached = TRUE WHERE id = $1`, t.crID); err != nil {
		return err
	}
	if _, err := m.svc.Notify(ctx, t.createdBy, TypeSLABreached,
		"SLA breached",
		"Change request #"+itoa64(t.displayID)+" ("+t.title+") passed its scheduled end.",
		crLink(t.crID)); err != nil {
		m.logger.Warn("sla breach notify failed", slog.String("crId", t.crID.String()), slog.String("error", err.Error()))
	}
	return nil
}

// warnIfFirst sends the warning notification once per CR (dedupe by link).
func (m *SLAMonitor) warnIfFirst(ctx context.Context, t slaRow) error {
	var alreadyWarned bool
	if err := m.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM notifications
			WHERE recipient_id = $1 AND type = $2 AND link = $3
		)`, t.createdBy, TypeSLAWarning, crLink(t.crID)).Scan(&alreadyWarned); err != nil {
		return err
	}
	if alreadyWarned {
		return nil
	}
	if _, err := m.svc.Notify(ctx, t.createdBy, TypeSLAWarning,
		"SLA warning",
		"Change request #"+itoa64(t.displayID)+" ("+t.title+") is approaching its scheduled end.",
		crLink(t.crID)); err != nil {
		m.logger.Warn("sla warn notify failed", slog.String("crId", t.crID.String()), slog.String("error", err.Error()))
	}
	return nil
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := [21]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
