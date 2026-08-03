-- 012_chat_encryption.sql — ciphertext at rest for channel/global chat
-- (backlog 91-135, "all msg must be encrypted").
--
-- Bodies move from chat_messages.body (plaintext TEXT, 007_chat.sql) to
-- body_enc = base64(nonce[24] || secretbox(plain, scope_key[key_id])).
-- The legacy column survives one release, pinned empty by a CHECK, so the
-- Go backfill can read it and so operators get one release of audit room.
--
-- Applied EXACTLY ONCE via the schema_migrations ledger, so the destructive
-- statements below are safe. Do NOT add destructive DML to migrations 001-011:
-- those predate the ledger and are still replayed once on the upgrade boot.

-- 1. Persistent scope key generations ---------------------------------------
-- Without these, every stored ciphertext becomes unreadable at the next
-- restart or rotation. wrapped_key is nonce[24] || secretbox(key32, KEK);
-- the KEK is NEVER in this database (chat_master_key_file / VOICX_CHAT_MASTER_KEY).
CREATE TABLE IF NOT EXISTS chat_scope_keys (
    scope_id    BIGINT      NOT NULL,               -- 0 = global, else channels.id
    key_id      BIGINT      NOT NULL,               -- generation; never reused
    wrapped_key BYTEA       NOT NULL,
    kek_id      SMALLINT    NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    retired_at  TIMESTAMPTZ,                        -- NULL = current generation
    PRIMARY KEY (scope_id, key_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_chat_scope_keys_current
    ON chat_scope_keys (scope_id) WHERE retired_at IS NULL;

-- Generation ids are allocated here, never as MAX(key_id)+1 and never from
-- an in-RAM counter, so a generation id can never be re-minted with
-- different key material.
CREATE TABLE IF NOT EXISTS chat_scope_seq (
    scope_id    BIGINT PRIMARY KEY,
    next_key_id BIGINT NOT NULL DEFAULT 1
);

-- 2. chat_messages: ciphertext columns --------------------------------------
ALTER TABLE chat_messages
    ADD COLUMN IF NOT EXISTS body_enc TEXT   NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS key_id   BIGINT NOT NULL DEFAULT 0;

-- 3. server_settings: motd/announcement carry a generation ------------------
-- Values are NOT blanked here. The Go backfill seals the existing plaintext
-- in place, exactly as it does for chat bodies — blanking would silently
-- destroy operator copy on upgrade for no security gain.
ALTER TABLE server_settings
    ADD COLUMN IF NOT EXISTS key_id BIGINT NOT NULL DEFAULT 0;

-- 4. files: mark client-encrypted chat attachments --------------------------
ALTER TABLE files
    ADD COLUMN IF NOT EXISTS encrypted BOOLEAN NOT NULL DEFAULT FALSE;

-- 5. Pre-4b plaintext spool rows --------------------------------------------
-- deliverSpooled treats from_unique_id='' rows as plaintext and replays them
-- as chat. They predate E2EE and cannot be re-sealed (no recipient key at
-- rest). Undelivered ones are dropped. Already-delivered ones are unreachable
-- (PendingMessages filters on delivered_at IS NULL) but still hold a plaintext
-- body at rest, so their text is blanked: offline_messages_sealed below is
-- VALIDATED, and one such row would otherwise abort the whole migration.
-- Safe because the ledger runs this file exactly once.
DELETE FROM offline_messages WHERE from_unique_id = '' AND delivered_at IS NULL;
UPDATE offline_messages SET message = '' WHERE from_unique_id = '' AND message <> '';

-- 6. Structural bans on plaintext -------------------------------------------
-- NOT VALID: existing rows still hold plaintext until the Go backfill runs.
-- New/updated rows are checked immediately, which is the point: from this
-- moment no code path can write a plaintext body, present or future.
-- The backfill VALIDATEs both constraints when it finishes.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chat_messages_no_plaintext') THEN
        ALTER TABLE chat_messages
            ADD CONSTRAINT chat_messages_no_plaintext CHECK (body = '') NOT VALID;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chat_messages_sealed') THEN
        ALTER TABLE chat_messages
            ADD CONSTRAINT chat_messages_sealed
            CHECK (deleted_at IS NOT NULL OR (body_enc <> '' AND key_id > 0)) NOT VALID;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'offline_messages_sealed') THEN
        -- Validated on add: step 5 above has just proven the table clean, and
        -- the point of this one is the audit, not only the future write ban.
        ALTER TABLE offline_messages
            ADD CONSTRAINT offline_messages_sealed
            CHECK (from_unique_id <> '' OR message = '');
    END IF;
END $$;
