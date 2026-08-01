-- 006_e2e_keys.sql — end-to-end encryption support (wave 4b, backlog 365).
--
-- users.e2e_public_key: X25519 public key (base64) for E2EE direct messages
-- and sealed channel-key distribution. Nullable: guests keep keys in memory
-- only; registered users persist on first publish.
-- offline_messages.from_unique_id: the sender's unique ID, needed so the
-- recipient can fetch the sender's public key to open spooled E2EE DMs
-- (spooled message bodies are ciphertext the server cannot read).
-- Idempotent: safe to re-run via Store.Migrate().

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS e2e_public_key TEXT;

ALTER TABLE offline_messages
    ADD COLUMN IF NOT EXISTS from_unique_id TEXT NOT NULL DEFAULT '';
