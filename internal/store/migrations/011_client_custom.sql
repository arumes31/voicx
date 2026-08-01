-- 011_client_custom.sql — wave 10a (222): custom per-client key/value
-- properties (TS3 customset). Idempotent: safe to re-run via Store.Migrate().

CREATE TABLE IF NOT EXISTS client_custom (
    unique_id TEXT NOT NULL,
    key       TEXT NOT NULL,
    value     TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (unique_id, key)
);
