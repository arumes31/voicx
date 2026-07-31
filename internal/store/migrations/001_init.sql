-- 001_init.sql — initial PostgreSQL schema for voicx.
--
-- NOTE: This migration uses `CREATE TABLE IF NOT EXISTS` for Phase 2 simplicity
-- so that re-running Migrate() is idempotent. A proper migration tool
-- (e.g. golang-migrate, goose, or a versioned migrations table) should be
-- introduced in a later phase to track applied migrations and support
-- up/down rollbacks. For now, the Store.Migrate() method simply executes
-- every embedded .sql file in lexical order.

-- Extensions -----------------------------------------------------------------
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- users ----------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS users (
    id            BIGSERIAL PRIMARY KEY,
    unique_id     TEXT UNIQUE NOT NULL,
    nickname      TEXT,
    password_hash TEXT,
    public_key    TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at  TIMESTAMPTZ,
    is_admin      BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE INDEX IF NOT EXISTS idx_users_nickname ON users (nickname);

-- channels -------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS channels (
    id            BIGSERIAL PRIMARY KEY,
    parent_id     BIGINT REFERENCES channels (id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    topic         TEXT,
    order_index   INTEGER NOT NULL DEFAULT 0,
    channel_type  SMALLINT NOT NULL, -- 0=temporary, 1=semi-permanent, 2=permanent
    codec         TEXT NOT NULL DEFAULT 'opus',
    max_clients   INTEGER,
    password_hash TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by    BIGINT REFERENCES users (id),
    is_default    BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE INDEX IF NOT EXISTS idx_channels_parent_id ON channels (parent_id);
CREATE INDEX IF NOT EXISTS idx_channels_created_by ON channels (created_by);

-- server_groups --------------------------------------------------------------
CREATE TABLE IF NOT EXISTS server_groups (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    sort_id     INTEGER NOT NULL DEFAULT 0,
    is_template BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- channel_groups -------------------------------------------------------------
CREATE TABLE IF NOT EXISTS channel_groups (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    sort_id    INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- permissions ----------------------------------------------------------------
CREATE TABLE IF NOT EXISTS permissions (
    id             BIGSERIAL PRIMARY KEY,
    permission_key TEXT NOT NULL,
    value          INTEGER NOT NULL DEFAULT 0,
    grant_value    INTEGER NOT NULL DEFAULT 0,
    skip_flag      BOOLEAN NOT NULL DEFAULT FALSE,
    negate_flag    BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE INDEX IF NOT EXISTS idx_permissions_key ON permissions (permission_key);

-- server_group_permissions (junction) ---------------------------------------
CREATE TABLE IF NOT EXISTS server_group_permissions (
    server_group_id BIGINT NOT NULL REFERENCES server_groups (id) ON DELETE CASCADE,
    permission_id   BIGINT NOT NULL REFERENCES permissions (id) ON DELETE CASCADE,
    PRIMARY KEY (server_group_id, permission_id)
);
CREATE INDEX IF NOT EXISTS idx_sgp_server_group_id ON server_group_permissions (server_group_id);
CREATE INDEX IF NOT EXISTS idx_sgp_permission_id   ON server_group_permissions (permission_id);

-- channel_group_permissions (junction) --------------------------------------
CREATE TABLE IF NOT EXISTS channel_group_permissions (
    channel_group_id BIGINT NOT NULL REFERENCES channel_groups (id) ON DELETE CASCADE,
    permission_id     BIGINT NOT NULL REFERENCES permissions (id) ON DELETE CASCADE,
    PRIMARY KEY (channel_group_id, permission_id)
);
CREATE INDEX IF NOT EXISTS idx_cgp_channel_group_id ON channel_group_permissions (channel_group_id);
CREATE INDEX IF NOT EXISTS idx_cgp_permission_id     ON channel_group_permissions (permission_id);

-- client_permissions (user-level server-wide or channel-scoped) --------------
-- channel_id NULL means server-wide. The uniqueness guarantee cannot be a
-- plain PRIMARY KEY over (user_id, permission_id, channel_id) because
-- PostgreSQL forces PK columns NOT NULL (which made the old "nullable"
-- comment a lie); instead use a surrogate PK plus a unique index over
-- COALESCE(channel_id, 0).
CREATE TABLE IF NOT EXISTS client_permissions (
    id            BIGSERIAL PRIMARY KEY,
    user_id       BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    permission_id BIGINT NOT NULL REFERENCES permissions (id) ON DELETE CASCADE,
    channel_id    BIGINT REFERENCES channels (id) ON DELETE CASCADE -- NULL = server-wide
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_client_permissions
    ON client_permissions (user_id, permission_id, (COALESCE(channel_id, 0)));
CREATE INDEX IF NOT EXISTS idx_cp_user_id       ON client_permissions (user_id);
CREATE INDEX IF NOT EXISTS idx_cp_permission_id  ON client_permissions (permission_id);
CREATE INDEX IF NOT EXISTS idx_cp_channel_id    ON client_permissions (channel_id);

-- channel_client_permissions (per-channel client perms) ---------------------
CREATE TABLE IF NOT EXISTS channel_client_permissions (
    user_id       BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    channel_id    BIGINT NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    permission_id BIGINT NOT NULL REFERENCES permissions (id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, channel_id, permission_id)
);
CREATE INDEX IF NOT EXISTS idx_ccp_user_id       ON channel_client_permissions (user_id);
CREATE INDEX IF NOT EXISTS idx_ccp_channel_id    ON channel_client_permissions (channel_id);
CREATE INDEX IF NOT EXISTS idx_ccp_permission_id ON channel_client_permissions (permission_id);

-- server_group_members (junction) -------------------------------------------
CREATE TABLE IF NOT EXISTS server_group_members (
    user_id         BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    server_group_id BIGINT NOT NULL REFERENCES server_groups (id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, server_group_id)
);
CREATE INDEX IF NOT EXISTS idx_sgm_user_id         ON server_group_members (user_id);
CREATE INDEX IF NOT EXISTS idx_sgm_server_group_id ON server_group_members (server_group_id);

-- channel_group_members (junction, per-channel) -----------------------------
CREATE TABLE IF NOT EXISTS channel_group_members (
    user_id          BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    channel_id       BIGINT NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    channel_group_id BIGINT NOT NULL REFERENCES channel_groups (id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, channel_id, channel_group_id)
);
CREATE INDEX IF NOT EXISTS idx_cgm_user_id          ON channel_group_members (user_id);
CREATE INDEX IF NOT EXISTS idx_cgm_channel_id       ON channel_group_members (channel_id);
CREATE INDEX IF NOT EXISTS idx_cgm_channel_group_id ON channel_group_members (channel_group_id);

-- bans -----------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS bans (
    id         BIGSERIAL PRIMARY KEY,
    ban_type   SMALLINT NOT NULL, -- 0=IP, 1=unique_id, 2=nickname
    value      TEXT NOT NULL,
    reason     TEXT,
    banned_by  BIGINT REFERENCES users (id),
    banned_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ, -- NULL = permanent
    channel_id BIGINT REFERENCES channels (id) ON DELETE CASCADE -- channel-specific ban
);
CREATE INDEX IF NOT EXISTS idx_bans_ban_type_value ON bans (ban_type, value);
CREATE INDEX IF NOT EXISTS idx_bans_channel_id      ON bans (channel_id);
CREATE INDEX IF NOT EXISTS idx_bans_expires_at      ON bans (expires_at);

-- tokens (privilege keys) ----------------------------------------------------
CREATE TABLE IF NOT EXISTS tokens (
    id         BIGSERIAL PRIMARY KEY,
    token_key  TEXT UNIQUE NOT NULL,
    token_type SMALLINT NOT NULL, -- 0=server group, 1=channel group
    group_id   BIGINT,
    channel_id BIGINT REFERENCES channels (id) ON DELETE CASCADE,
    created_by BIGINT REFERENCES users (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    uses       INTEGER NOT NULL DEFAULT 0,
    max_uses   INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_tokens_group_id   ON tokens (group_id);
CREATE INDEX IF NOT EXISTS idx_tokens_channel_id ON tokens (channel_id);

-- offline_messages -----------------------------------------------------------
CREATE TABLE IF NOT EXISTS offline_messages (
    id           BIGSERIAL PRIMARY KEY,
    from_user_id BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    to_user_id   BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    message      TEXT NOT NULL,
    sent_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivered_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_offline_messages_to_user_id ON offline_messages (to_user_id);
CREATE INDEX IF NOT EXISTS idx_offline_messages_delivered_at ON offline_messages (delivered_at);
