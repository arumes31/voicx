-- 019_user_private_data.sql — AES-256-GCM protected user PII.

CREATE TABLE IF NOT EXISTS user_private_data (
    user_id     BIGINT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    email_enc   BYTEA,
    last_ip_enc BYTEA,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
