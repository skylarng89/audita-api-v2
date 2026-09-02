-- 0006_rls_policies.up.sql
-- Row-Level Security policies for every tenant-scoped table (blueprint ADR-4 / FR-3).
-- Session variables set per-transaction via platform/db.go TenantDB.InTenantTx:
--   app.tenant_id : UUID of the current tenant (required for RLS predicate)
--   app.user_id   : UUID of the authenticated user (application-level scoping)
--   app.role      : primary role name (application-level authorization)
-- The `current_setting(..., true)` 2-arg form returns NULL (not an error) when
-- unset; policies then evaluate to false, blocking accidental full-table reads.

DO $$
DECLARE
    t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'roles',
        'role_permissions',
        'users',
        'refresh_tokens',
        'invite_tokens',
        'password_reset_tokens',
        'oauth_accounts',
        'departments',
        'groups',
        'group_members',
        'change_requests',
        'cr_approvers',
        'cr_approver_groups',
        'cr_watchers',
        'cr_watcher_groups',
        'custom_field_definitions',
        'change_request_custom_fields',
        'request_links',
        'comments',
        'comment_mentions',
        'attachments',
        'activity_stream',
        'notifications',
        'audit_log',
        'audit_export_requests',
        'org_settings'
    ]
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$
            CREATE POLICY tenant_isolation ON %I
            USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
        $p$, t);
    END LOOP;
END $$;

-- Note on `roles`/`permissions` (shared system data): these tables are seeded
-- once per tenant from app-level migrations/system bootstrap with that
-- tenant's id stamped in, so RLS keeps tenants from reading each other's
-- role definitions while allowing each tenant its own role customisation.

-- Platform tables (tenants, super_admins, tenant_allowed_domains,
-- tenant_sso_configs, outbox_events, idempotency_keys) intentionally have NO
-- tenant_id and NO RLS; access is restricted by the DB role
-- (audita_platform has BYPASSRLS, audita_app does not) enforced at the
-- connection level via DATABASE_URL credentials (blueprint §3.3).
