-- 014_token_meta.sql — privilege-token manager metadata (backlog 174).
--
-- The token manager lists an operator-supplied description and the redeemer
-- next to each key; 001_init.sql stores neither. Both columns are additive
-- with defaults, so existing rows stay valid and the file is idempotent.

ALTER TABLE tokens
    ADD COLUMN IF NOT EXISTS description TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS used_by     TEXT NOT NULL DEFAULT '';
