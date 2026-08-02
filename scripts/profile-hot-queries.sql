\set ON_ERROR_STOP on
\if :{?channel_id}
\else
\set channel_id 1
\endif
\if :{?user_id}
\else
\set user_id 1
\endif

\echo 'chat history: newest page by channel'
EXPLAIN (ANALYZE, BUFFERS, WAL, SETTINGS)
SELECT id, from_unique_id, from_nickname, reply_to_id, version,
       COALESCE(client_msg_id, ''), body_enc, key_id, sent_at, edited_at, deleted_at
FROM chat_messages
WHERE channel_id = :channel_id
ORDER BY id DESC
LIMIT 50;

\echo 'chat history: cursor page by channel'
EXPLAIN (ANALYZE, BUFFERS, WAL, SETTINGS)
SELECT id, body_enc, key_id
FROM chat_messages
WHERE channel_id = :channel_id
  AND id < COALESCE((SELECT MAX(id) FROM chat_messages), 9223372036854775807)
ORDER BY id DESC
LIMIT 50;

\echo 'permission memberships for a user/channel'
EXPLAIN (ANALYZE, BUFFERS, WAL, SETTINGS)
SELECT channel_group_id
FROM channel_group_members
WHERE user_id = :user_id AND channel_id = :channel_id
ORDER BY channel_group_id;

\echo 'channel-client permission tier'
EXPLAIN (ANALYZE, BUFFERS, WAL, SETTINGS)
SELECT p.permission_key, p.value, p.grant_value, p.skip_flag, p.negate_flag
FROM channel_client_permissions ccp
JOIN permissions p ON p.id = ccp.permission_id
WHERE ccp.user_id = :user_id AND ccp.channel_id = :channel_id;

\echo 'inherited channel permission walk'
EXPLAIN (ANALYZE, BUFFERS, WAL, SETTINGS)
WITH RECURSIVE chain(id, parent_id, inherit, depth) AS (
  SELECT id, parent_id, inherit_permissions, 0 FROM channels WHERE id = :channel_id
  UNION ALL
  SELECT c.id, c.parent_id, c.inherit_permissions, chain.depth + 1
  FROM channels c JOIN chain ON c.id = chain.parent_id
  WHERE chain.inherit AND chain.depth < 64
)
SELECT id FROM chain ORDER BY depth;
