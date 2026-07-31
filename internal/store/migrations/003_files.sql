-- 003_files.sql — file-transfer metadata for voicx.
--
-- One row per uploaded file. (channel_id, name) is unique: uploading a file
-- with the same name in the same channel REPLACES the previous one.
-- Idempotent: safe to re-run via Store.Migrate().

CREATE TABLE IF NOT EXISTS files (
    id          BIGSERIAL PRIMARY KEY,
    channel_id  BIGINT NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    size        BIGINT NOT NULL,
    sha256      TEXT NOT NULL,
    uploader    TEXT, -- unique_id of the uploading user
    uploaded_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (channel_id, name)
);
CREATE INDEX IF NOT EXISTS idx_files_channel_id ON files (channel_id);
