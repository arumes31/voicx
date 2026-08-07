-- voicx:no-transaction
-- 016_chat_consistency.sql — idempotent client-message identifiers.
-- Marked files contain exactly one concurrent index statement so the runner
-- can verify its full catalog definition before recording the ledger row.

CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_messages_client_msg_id
    ON public.chat_messages (channel_id, from_unique_id, client_msg_id)
    WHERE client_msg_id IS NOT NULL AND client_msg_id <> '';
