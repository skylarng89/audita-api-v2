-- 0007_tenant_scoping.down.sql
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
        EXECUTE format('DROP INDEX IF EXISTS idx_%s_tenant', t);
        EXECUTE format('ALTER TABLE %I DROP COLUMN IF EXISTS tenant_id', t);
    END LOOP;
END $$;
