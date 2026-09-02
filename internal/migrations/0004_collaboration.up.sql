-- 0004_collaboration.up.sql
-- Collaboration module tables (blueprint §3.2): comments, comment_mentions,
-- attachments, activity_stream. All tenant-scoped (RLS in 0006).

CREATE TABLE comments (
    id                 UUID PRIMARY KEY,
    change_request_id  UUID NOT NULL,
    author_id          UUID NOT NULL,
    body               TEXT NOT NULL,          -- sanitized HTML (bluemonday)
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_comments_cr ON comments (change_request_id, created_at);

CREATE TABLE comment_mentions (
    id          UUID PRIMARY KEY,
    comment_id  UUID NOT NULL REFERENCES comments(id) ON DELETE CASCADE,
    user_id     UUID NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (comment_id, user_id)
);

CREATE INDEX idx_comment_mentions_user ON comment_mentions (user_id);

CREATE TABLE attachments (
    id                 UUID PRIMARY KEY,
    change_request_id  UUID NOT NULL,
    uploader_id        UUID NOT NULL,
    file_name          TEXT NOT NULL,
    mime_type          TEXT NOT NULL,
    size_bytes         BIGINT NOT NULL,
    storage_key        TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_attachments_cr ON attachments (change_request_id);

CREATE TABLE activity_stream (
    id                 UUID PRIMARY KEY,
    change_request_id  UUID NOT NULL,
    actor_id           UUID NOT NULL,
    action_type        TEXT NOT NULL,
    payload            JSONB NOT NULL DEFAULT '{}',
    occurred_at        TIMESTAMPTZ NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_activity_stream_cr ON activity_stream (change_request_id, occurred_at DESC);
