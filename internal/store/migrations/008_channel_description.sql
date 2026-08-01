-- 008_channel_description.sql — per-channel description text (wave 5b,
-- backlog 112/113). Rendered client-side with markdown; edited via
-- ChannelEdit/ServerQuery channeledit. Idempotent via Store.Migrate().

ALTER TABLE channels
    ADD COLUMN IF NOT EXISTS description TEXT NOT NULL DEFAULT '';
