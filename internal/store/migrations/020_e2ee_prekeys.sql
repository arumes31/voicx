-- 020_e2ee_prekeys.sql — public X3DH signed and one-time prekeys.

CREATE TABLE IF NOT EXISTS e2ee_prekeys (
    user_id      BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    key_id       BIGINT NOT NULL,
    public_key   BYTEA NOT NULL,
    signature    BYTEA,
    one_time     BOOLEAN NOT NULL DEFAULT FALSE,
    consumed_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, key_id)
);

CREATE INDEX IF NOT EXISTS idx_e2ee_prekeys_available
    ON e2ee_prekeys (user_id, key_id)
    WHERE one_time AND consumed_at IS NULL;
