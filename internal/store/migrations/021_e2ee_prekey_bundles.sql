-- 021_e2ee_prekey_bundles.sql — persistent X3DH device identity and signed
-- prekey. One-time prekeys remain in e2ee_prekeys and are consumed atomically.

CREATE TABLE IF NOT EXISTS e2ee_prekey_bundles (
    user_id          BIGINT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    identity_dh      BYTEA NOT NULL,
    signing_public   BYTEA NOT NULL,
    signed_prekey_id BIGINT NOT NULL,
    signed_prekey    BYTEA NOT NULL,
    signature        BYTEA NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
