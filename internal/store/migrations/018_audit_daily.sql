-- 018_audit_daily.sql — compact old permission audit events into daily deltas.

CREATE TABLE IF NOT EXISTS audit_log_daily (
    day         DATE NOT NULL,
    action      TEXT NOT NULL,
    target      TEXT NOT NULL DEFAULT '',
    event_count BIGINT NOT NULL,
    PRIMARY KEY (day, action, target)
);
