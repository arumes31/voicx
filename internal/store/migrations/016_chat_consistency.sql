-- 016_chat_consistency.sql — replies, idempotent sends, and optimistic edits.

ALTER TABLE chat_messages
    ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE chat_messages
    ADD COLUMN IF NOT EXISTS client_msg_id TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_chat_messages_client_msg_id
    ON chat_messages (channel_id, from_unique_id, client_msg_id)
    WHERE client_msg_id IS NOT NULL AND client_msg_id <> '';

CREATE INDEX IF NOT EXISTS idx_chat_messages_reply_to
    ON chat_messages (reply_to_id)
    WHERE reply_to_id IS NOT NULL;
