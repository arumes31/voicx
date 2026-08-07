-- voicx:no-transaction
-- 016z_chat_reply_index.sql — concurrent reply lookup without blocking writes.

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_messages_reply_to
    ON public.chat_messages (reply_to_id)
    WHERE reply_to_id IS NOT NULL;
