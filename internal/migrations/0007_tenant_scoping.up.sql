-- 0007_tenant_scoping.up.sql
-- Adds `tenant_id` to every tenant-scoped table and indexes it (blueprint ADR-4).
-- This migration is separated from the table-creation migrations so the
-- platform-core tables (which have no tenant_id) are unambiguous.

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
        EXECUTE format('ALTER TABLE %I ADD COLUMN tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE', t);
        EXECUTE format('CREATE INDEX idx_%s_tenant ON %I (tenant_id)', t, t);
    END LOOP;
END $$;
