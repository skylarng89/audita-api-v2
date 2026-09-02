-- 0005_notification_audit.up.sql
-- Notification and audit tables (blueprint §3.2). All tenant-scoped (RLS in 0006).

CREATE TABLE notifications (
    id          UUID PRIMARY KEY,
    recipient_id UUID NOT NULL,
    type        TEXT NOT NULL,
    title       TEXT NOT NULL,
    body        TEXT NOT NULL DEFAULT '',
    link        TEXT,
    is_read     BOOLEAN NOT NULL DEFAULT FALSE,
    read_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_notifications_recipient ON notifications (recipient_id, created_at DESC);
CREATE INDEX idx_notifications_unread ON notifications (recipient_id) WHERE is_read = FALSE;

CREATE TABLE audit_log (
    id            UUID PRIMARY KEY,
    chain_index   BIGINT NOT NULL,
    record_hash   TEXT NOT NULL,
    previous_hash TEXT NOT NULL,
    actor_id      UUID,
    actor_email   TEXT,
    action_type   TEXT NOT NULL,
    entity_type   TEXT NOT NULL,
    entity_id     UUID,
    payload       JSONB NOT NULL DEFAULT '{}',
    ip_address    TEXT,
    occurred_at   TIMESTAMPTZ NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (chain_index)
);

CREATE INDEX idx_audit_log_occurred ON audit_log (occurred_at DESC);
CREATE INDEX idx_audit_log_actor ON audit_log (actor_id);
CREATE INDEX idx_audit_log_entity ON audit_log (entity_type, entity_id);

CREATE TABLE audit_export_requests (
    id           UUID PRIMARY KEY,
    requested_by UUID NOT NULL,
    filters      JSONB NOT NULL DEFAULT '{}',
    status       TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','ready','failed','expired')),
    file_path    TEXT,
    error        TEXT,
    expires_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_audit_export_requests_status ON audit_export_requests (status);
