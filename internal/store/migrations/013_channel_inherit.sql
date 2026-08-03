-- 013_channel_inherit.sql — sub-channel permission inheritance (157) and the
-- index backing the total channel order (163).
--
-- inherit_permissions defaults FALSE so existing channels keep resolving only
-- their own channel permissions; the toggle is set through ChannelEdit.
-- Idempotent DDL: safe even though the schema_migrations ledger runs this once.

ALTER TABLE channels
    ADD COLUMN IF NOT EXISTS inherit_permissions BOOLEAN NOT NULL DEFAULT FALSE;

CREATE INDEX IF NOT EXISTS idx_channels_order
    ON channels (parent_id, order_index, id);
