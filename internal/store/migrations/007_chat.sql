-- 007_chat.sql — server-side chat infrastructure (wave 5a, backlog 96-133).
--
-- chat_messages stores channel/global chat history. The body column below is
-- superseded by body_enc in 012_chat_encryption.sql (91-135): bodies are
-- CIPHERTEXT at rest and body is pinned empty by a CHECK, so nothing may be
-- read from it. Direct messages are NOT stored here: they are true E2EE
-- and their history is client-local by design.
-- Idempotent: safe to re-run via Store.Migrate().

CREATE TABLE IF NOT EXISTS chat_messages (
    id            BIGSERIAL PRIMARY KEY,
    scope         SMALLINT NOT NULL,           -- 0=global, 1=channel
    channel_id    BIGINT NOT NULL DEFAULT 0,   -- 0 = global scope
    from_unique_id TEXT NOT NULL,
    from_nickname TEXT NOT NULL DEFAULT '',
    body          TEXT NOT NULL DEFAULT '',
    sent_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    edited_at     TIMESTAMPTZ,
    deleted_at    TIMESTAMPTZ,
    reply_to_id   BIGINT
);
CREATE INDEX IF NOT EXISTS idx_chat_messages_channel ON chat_messages (channel_id, id);

CREATE TABLE IF NOT EXISTS pinned_messages (
    channel_id BIGINT NOT NULL,
    message_id BIGINT NOT NULL REFERENCES chat_messages (id) ON DELETE CASCADE,
    pinned_by  TEXT NOT NULL,
    pinned_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (channel_id, message_id)
);

CREATE TABLE IF NOT EXISTS chat_reactions (
    message_id BIGINT NOT NULL REFERENCES chat_messages (id) ON DELETE CASCADE,
    unique_id  TEXT NOT NULL,
    emoji      TEXT NOT NULL,
    PRIMARY KEY (message_id, unique_id, emoji)
);

CREATE TABLE IF NOT EXISTS server_settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);

-- (114) Slow mode: minimum seconds between a non-privileged user's messages
-- in this channel. 0 = off.
ALTER TABLE channels
    ADD COLUMN IF NOT EXISTS slow_mode_seconds INTEGER NOT NULL DEFAULT 0;
