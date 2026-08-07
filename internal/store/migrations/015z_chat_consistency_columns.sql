-- 015z_chat_consistency_columns.sql — columns required by consistency indexes.
-- Kept transactional; only the potentially long index builds need autocommit.

ALTER TABLE public.chat_messages
    ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE public.chat_messages
    ADD COLUMN IF NOT EXISTS client_msg_id TEXT;
