-- 024_chat_kek_id_range.sql — store the full unsigned 16-bit KEK id range.
--
-- Earlier binaries accepted ids through 65535 but persisted them through a
-- signed SMALLINT conversion. Values 32768..65535 therefore landed as
-- -32768..-1. Recover their unsigned representation while widening the column.

ALTER TABLE chat_scope_keys
    ALTER COLUMN kek_id TYPE INTEGER
    USING (CASE WHEN kek_id < 0 THEN kek_id::INTEGER + 65536 ELSE kek_id::INTEGER END);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'chat_scope_keys_kek_id_range'
          AND conrelid = 'chat_scope_keys'::regclass
    ) THEN
        ALTER TABLE chat_scope_keys
            ADD CONSTRAINT chat_scope_keys_kek_id_range
            CHECK (kek_id BETWEEN 1 AND 65535) NOT VALID;
    END IF;
END $$;

ALTER TABLE chat_scope_keys
    VALIDATE CONSTRAINT chat_scope_keys_kek_id_range;
