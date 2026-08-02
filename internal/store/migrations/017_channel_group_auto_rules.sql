-- 017_channel_group_auto_rules.sql — channel-group assignment on subscribe.

CREATE TABLE IF NOT EXISTS channel_group_auto_rules (
    id               BIGSERIAL PRIMARY KEY,
    channel_id       BIGINT NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    channel_group_id BIGINT NOT NULL REFERENCES channel_groups (id) ON DELETE CASCADE,
    priority         INTEGER NOT NULL DEFAULT 0,
    enabled          BOOLEAN NOT NULL DEFAULT TRUE,
    UNIQUE (channel_id, channel_group_id)
);

CREATE INDEX IF NOT EXISTS idx_channel_group_auto_rules_match
    ON channel_group_auto_rules (channel_id, priority DESC, id)
    WHERE enabled;
