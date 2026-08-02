-- 015_server_rules.sql — per-user acceptance of the server rules (215).
--
-- The rules TEXT itself lives in server_settings under "server_rules" (it is
-- operator content, not message content, so it is stored in the clear like
-- server_name). This table only records who accepted which wording: the
-- acceptance is keyed by the sha256 of the rules text, so editing the rules
-- re-asks everyone instead of silently counting the old consent.

CREATE TABLE IF NOT EXISTS server_rules_acceptance (
    user_id     BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    rules_hash  TEXT        NOT NULL,
    accepted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id)
);

CREATE INDEX IF NOT EXISTS idx_server_rules_acceptance_hash
    ON server_rules_acceptance (rules_hash);
