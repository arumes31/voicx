-- Registered nicknames are login identifiers and must be unambiguous.
-- Keep the oldest account's nickname when repairing databases created before
-- this constraint; renamed duplicates remain unique and continue to be
-- addressable by their stable unique_id.
DO $$
DECLARE
    duplicate RECORD;
    candidate TEXT;
    attempt BIGINT;
BEGIN
    FOR duplicate IN
        SELECT id, nickname
          FROM (
              SELECT id, nickname,
                     ROW_NUMBER() OVER (PARTITION BY nickname ORDER BY id) AS duplicate_number
                FROM users
               WHERE nickname IS NOT NULL
          ) AS ranked
         WHERE duplicate_number > 1
         ORDER BY id
    LOOP
        attempt := 0;
        LOOP
            candidate := duplicate.nickname || '#duplicate-' || duplicate.id;
            IF attempt > 0 THEN
                candidate := candidate || '-' || attempt;
            END IF;
            EXIT WHEN NOT EXISTS (
                SELECT 1 FROM users
                 WHERE nickname = candidate
                   AND id <> duplicate.id
            );
            attempt := attempt + 1;
        END LOOP;

        UPDATE users
           SET nickname = candidate
         WHERE id = duplicate.id;
    END LOOP;
END
$$;

CREATE UNIQUE INDEX IF NOT EXISTS uq_users_nickname ON users (nickname)
    WHERE nickname IS NOT NULL;
