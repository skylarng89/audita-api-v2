-- 0003_changerequest.up.sql
-- Change-request domain tables (blueprint §3.2 `changerequest` module).
-- All tenant-scoped (tenant_id + RLS applied in 0006).

CREATE TABLE departments (
    id            UUID PRIMARY KEY,
    name          TEXT NOT NULL,
    code          TEXT,
    is_active     BOOLEAN NOT NULL DEFAULT TRUE,
    display_order INT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE groups (
    id            UUID PRIMARY KEY,
    name          TEXT NOT NULL,
    description   TEXT,
    is_active     BOOLEAN NOT NULL DEFAULT TRUE,
    display_order INT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE group_members (
    id         UUID PRIMARY KEY,
    group_id   UUID NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id    UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (group_id, user_id)
);

CREATE INDEX idx_group_members_user ON group_members (user_id);
CREATE INDEX idx_group_members_group ON group_members (group_id);

CREATE TABLE change_requests (
    id                  UUID PRIMARY KEY,
    display_id          BIGSERIAL,               -- tenant-unique human-readable number
    title               TEXT NOT NULL,
    description         TEXT NOT NULL DEFAULT '',
    status              TEXT NOT NULL DEFAULT 'draft'
                        CHECK (status IN ('draft','pending_approval','approved','rejected','cancelled')),
    completion_status   TEXT NOT NULL DEFAULT 'open'
                        CHECK (completion_status IN ('open','completed')),
    priority            TEXT NOT NULL DEFAULT 'medium' CHECK (priority IN ('low','medium','high')),
    scheduled_start     TIMESTAMPTZ,
    scheduled_end       TIMESTAMPTZ,
    affected_systems    TEXT[] NOT NULL DEFAULT '{}',
    sla_breached        BOOLEAN NOT NULL DEFAULT FALSE,
    request_department_id  UUID REFERENCES departments(id),
    destination_department_id UUID REFERENCES departments(id),
    request_group_id    UUID REFERENCES groups(id),
    created_by          UUID NOT NULL,           -- users.id (no FK: cross-module isolation)
    submitted_at        TIMESTAMPTZ,
    decided_at          TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ,
    cancelled_at        TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_change_requests_display_id ON change_requests (display_id);
CREATE INDEX idx_change_requests_status ON change_requests (status);
CREATE INDEX idx_change_requests_created_by ON change_requests (created_by);
CREATE INDEX idx_change_requests_scheduled_end ON change_requests (scheduled_end) WHERE status = 'pending_approval';

CREATE TABLE cr_approvers (
    id                 UUID PRIMARY KEY,
    change_request_id  UUID NOT NULL REFERENCES change_requests(id) ON DELETE CASCADE,
    user_id            UUID NOT NULL,
    status             TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','rejected')),
    rejection_reason   TEXT,
    decision_comment   TEXT,
    decided_at         TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (change_request_id, user_id)
);

CREATE INDEX idx_cr_approvers_user ON cr_approvers (user_id);
CREATE INDEX idx_cr_approvers_cr ON cr_approvers (change_request_id);

CREATE TABLE cr_approver_groups (
    id                 UUID PRIMARY KEY,
    change_request_id  UUID NOT NULL REFERENCES change_requests(id) ON DELETE CASCADE,
    group_id           UUID NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    approved_by        UUID,                    -- first group member who approved
    decided_at         TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (change_request_id, group_id)
);

CREATE TABLE cr_watchers (
    id                 UUID PRIMARY KEY,
    change_request_id  UUID NOT NULL REFERENCES change_requests(id) ON DELETE CASCADE,
    user_id            UUID NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (change_request_id, user_id)
);

CREATE INDEX idx_cr_watchers_user ON cr_watchers (user_id);
CREATE INDEX idx_cr_watchers_cr ON cr_watchers (change_request_id);

CREATE TABLE cr_watcher_groups (
    id                 UUID PRIMARY KEY,
    change_request_id  UUID NOT NULL REFERENCES change_requests(id) ON DELETE CASCADE,
    group_id           UUID NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (change_request_id, group_id)
);

CREATE TABLE custom_field_definitions (
    id            UUID PRIMARY KEY,
    label         TEXT NOT NULL,
    field_type    TEXT NOT NULL CHECK (field_type IN ('text','number','select','multiselect','date','boolean')),
    options       TEXT[] NOT NULL DEFAULT '{}',
    min_value     NUMERIC,
    max_value     NUMERIC,
    required      BOOLEAN NOT NULL DEFAULT FALSE,
    display_order INT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE change_request_custom_fields (
    id                    UUID PRIMARY KEY,
    change_request_id     UUID NOT NULL REFERENCES change_requests(id) ON DELETE CASCADE,
    custom_field_def_id   UUID NOT NULL REFERENCES custom_field_definitions(id) ON DELETE CASCADE,
    value                 JSONB NOT NULL DEFAULT 'null',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (change_request_id, custom_field_def_id)
);

CREATE TABLE request_links (
    id             UUID PRIMARY KEY,
    request_id_a   UUID NOT NULL REFERENCES change_requests(id) ON DELETE CASCADE,
    request_id_b   UUID NOT NULL REFERENCES change_requests(id) ON DELETE CASCADE,
    linked_by      UUID NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- canonical ordered pair to prevent A<->B duplicates
    CONSTRAINT request_links_ordered CHECK (request_id_a < request_id_b),
    UNIQUE (request_id_a, request_id_b)
);
