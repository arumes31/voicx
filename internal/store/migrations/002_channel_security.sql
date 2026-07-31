-- 002_channel_security.sql — channel security columns for voicx.
--
-- Adds the per-channel needed join power used by the join-permission
-- enforcement (the client's i_channel_join_power must meet or exceed it).
-- The channels table already carries password_hash from 001_init.sql.
-- Idempotent: safe to re-run via Store.Migrate().

ALTER TABLE channels
    ADD COLUMN IF NOT EXISTS needed_join_power INTEGER NOT NULL DEFAULT 0;
