-- 004_complaints.sql — user complaints for voicx.
--
-- One row per complaint. "Open" complaints are all rows; deletion (via
-- ServerQuery) is the resolution path. Idempotent: safe to re-run.

CREATE TABLE IF NOT EXISTS complaints (
    id         BIGSERIAL PRIMARY KEY,
    reporter   TEXT NOT NULL, -- unique_id of the reporting user
    target     TEXT NOT NULL, -- unique_id of the user being reported
    reason     TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_complaints_reporter ON complaints (reporter);
CREATE INDEX IF NOT EXISTS idx_complaints_target ON complaints (target);
