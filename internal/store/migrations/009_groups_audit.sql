-- 009_groups_audit.sql — wave 6a: permission/group management.
--
-- - Timed server-group assignments (145): assigned_at/expires_at on
--   server_group_members (a reaper removes expired rows).
-- - Group cosmetics (177-179 data side): icon/color/hoist on server_groups.
-- - Channel tier permissions: the channel itself can carry permissions
--   (e.g. i_channel_needed_talk_power per channel). The loader reads this
--   as the TierChannel tier (between channel_specific and channel_group in
--   evaluation order).
-- - audit_log (149/197): permission/group/admin action trail.
-- Idempotent: safe to re-run via Store.Migrate().

ALTER TABLE server_group_members
    ADD COLUMN IF NOT EXISTS assigned_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;

ALTER TABLE server_groups
    ADD COLUMN IF NOT EXISTS icon TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS color TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS hoist BOOLEAN NOT NULL DEFAULT FALSE;

-- Heal pre-009 databases: 001 originally declared client_permissions.channel_id
-- NOT NULL with a PK over (user_id, permission_id, channel_id), although NULL
-- means "server-wide". Drop that PK (only when it covers channel_id — fresh
-- databases have a surrogate id PK instead) and make the column nullable. The
-- unique index uq_client_permissions already guarantees uniqueness.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pg_constraint c
        JOIN pg_class t ON t.oid = c.conrelid
        JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
        WHERE t.relname = 'client_permissions'
          AND c.contype = 'p'
          AND a.attname = 'channel_id'
    ) THEN
        ALTER TABLE client_permissions DROP CONSTRAINT client_permissions_pkey;
    END IF;
END $$;
ALTER TABLE client_permissions ALTER COLUMN channel_id DROP NOT NULL;

CREATE TABLE IF NOT EXISTS channel_permissions (
    channel_id    BIGINT NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    permission_id BIGINT NOT NULL REFERENCES permissions (id) ON DELETE CASCADE,
    PRIMARY KEY (channel_id, permission_id)
);

CREATE TABLE IF NOT EXISTS audit_log (
    id              BIGSERIAL PRIMARY KEY,
    actor_unique_id TEXT NOT NULL DEFAULT '',
    action          TEXT NOT NULL,
    target          TEXT NOT NULL DEFAULT '',
    detail          TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_audit_log_created ON audit_log (id DESC);
