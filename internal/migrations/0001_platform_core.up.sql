-- 0001_platform_core.up.sql
-- Platform-level tables (blueprint §3.3): tenants, super_admins,
-- tenant_allowed_domains, tenant_sso_configs, org_settings (org_settings is
-- tenant-scoped and RLS-protected; the rest are cross-tenant platform tables).

CREATE TABLE tenants (
    id          UUID PRIMARY KEY,
    name        TEXT NOT NULL,
    slug        TEXT NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$'),
    subdomain   TEXT UNIQUE,
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','suspended','archived')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE super_admins (
    id            UUID PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE tenant_allowed_domains (
    id          UUID PRIMARY KEY,
    domain      TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (domain)
);

CREATE TABLE tenant_sso_configs (
    id               UUID PRIMARY KEY,
    provider         TEXT NOT NULL CHECK (provider IN ('google','microsoft')),
    client_id        TEXT NOT NULL,
    client_secret_encrypted TEXT NOT NULL, -- AES-GCM (platform/crypto)
    ms_tenant_id     TEXT,
    scopes           TEXT[] NOT NULL DEFAULT '{}',
    enabled          BOOLEAN NOT NULL DEFAULT TRUE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider)
);

CREATE TABLE org_settings (
    id          UUID PRIMARY KEY,
    key         TEXT NOT NULL,
    value       JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (key)
);

CREATE INDEX idx_org_settings_key ON org_settings (key);

-- Shared outbox for transactional event publication (blueprint §4.3).
CREATE TABLE outbox_events (
    id           BIGSERIAL PRIMARY KEY,
    event        JSONB NOT NULL,          -- eventbus.Event envelope
    delivered_at TIMESTAMPTZ,
    attempts     INT NOT NULL DEFAULT 0,
    last_error   TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_outbox_undelivered ON outbox_events (id) WHERE delivered_at IS NULL;

-- Idempotency keys (blueprint §4.3), platform-level (cross-module).
CREATE TABLE idempotency_keys (
    id               UUID PRIMARY KEY,
    user_id          UUID NOT NULL,
    operation        TEXT NOT NULL,
    idempotency_key  TEXT NOT NULL,
    resource_id      TEXT,
    response_body    BYTEA,
    response_status  INT,
    expires_at       TIMESTAMPTZ NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, operation, idempotency_key)
);

CREATE INDEX idx_idempotency_expiry ON idempotency_keys (expires_at);
