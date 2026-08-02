-- 021_e2ee_prekey_bundles.sql — persistent X3DH device identity and signed
-- prekey. One-time prekeys remain in e2ee_prekeys and are consumed atomically.
-- voicx currently supports one cryptographic device identity per account, so
-- user_id is intentionally the primary/conflict key. Multi-device support must
-- introduce device_id in a new migration and update the wire protocol first.

CREATE TABLE IF NOT EXISTS e2ee_prekey_bundles (
    user_id          BIGINT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    identity_dh      BYTEA NOT NULL CONSTRAINT e2ee_prekey_bundle_identity_size CHECK (octet_length(identity_dh) = 32),
    signing_public   BYTEA NOT NULL,
    signed_prekey_id BIGINT NOT NULL,
    signed_prekey    BYTEA NOT NULL CONSTRAINT e2ee_prekey_bundle_signed_size CHECK (octet_length(signed_prekey) = 32),
    signature        BYTEA NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
