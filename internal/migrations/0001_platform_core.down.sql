-- 0001_platform_core.down.sql
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS org_settings;
DROP TABLE IF EXISTS tenant_sso_configs;
DROP TABLE IF EXISTS tenant_allowed_domains;
DROP TABLE IF EXISTS super_admins;
DROP TABLE IF EXISTS tenants;
