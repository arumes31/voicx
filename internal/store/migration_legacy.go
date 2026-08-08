package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
)

const (
	legacyChatConsistencyFilename = "016_chat_consistency.sql"
	legacyChatConsistencyChecksum = "52cc829a0ed3713a2d1faac18111a242374b6ef065f9a2dc98f6f8cfc272bf75"
	chatReplyIndexFilename        = "016z_chat_reply_index.sql"
)

// legacyChatConsistencySQL is the exact canonical 016 content shipped before
// marked migrations were restricted to one concurrent index. Filename-only
// ledgers cannot prove these bytes ran, so the upgrade path verifies and
// repairs their catalog effects before recording the current split baseline.
const legacyChatConsistencySQL = `-- voicx:no-transaction
-- 016_chat_consistency.sql — replies, idempotent sends, and optimistic edits.
-- The concurrent indexes are applied as separate autocommit statements by the
-- migration runner so production writes are not blocked while they build.

ALTER TABLE chat_messages
    ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE chat_messages
    ADD COLUMN IF NOT EXISTS client_msg_id TEXT;

CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_messages_client_msg_id
    ON chat_messages (channel_id, from_unique_id, client_msg_id)
    WHERE client_msg_id IS NOT NULL AND client_msg_id <> '';

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_messages_reply_to
    ON chat_messages (reply_to_id)
    WHERE reply_to_id IS NOT NULL;
`

func verifyLegacyChatConsistencyDescriptor() error {
	checksum := fmt.Sprintf("%x", sha256.Sum256(canonicalMigrationSQL(
		[]byte(legacyChatConsistencySQL),
	)))
	if checksum != legacyChatConsistencyChecksum {
		return fmt.Errorf(
			"legacy migration %s descriptor checksum = %s, want pinned %s",
			legacyChatConsistencyFilename, checksum, legacyChatConsistencyChecksum,
		)
	}
	return nil
}

func repairLegacyChatConsistency(
	ctx context.Context,
	conn *sql.Conn,
	embeddedByName map[string]embeddedMigration,
) error {
	if err := verifyLegacyChatConsistencyDescriptor(); err != nil {
		return err
	}
	// Repair the two column effects of the prior multi-statement migration in
	// the canonical public schema before constructing either expected index.
	if _, err := conn.ExecContext(ctx, `ALTER TABLE public.chat_messages
		ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 1`); err != nil {
		return fmt.Errorf("repairing legacy chat version column: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `ALTER TABLE public.chat_messages
		ADD COLUMN IF NOT EXISTS client_msg_id TEXT`); err != nil {
		return fmt.Errorf("repairing legacy client-message column: %w", err)
	}

	for _, filename := range []string{legacyChatConsistencyFilename, chatReplyIndexFilename} {
		migration, ok := embeddedByName[filename]
		if !ok {
			return fmt.Errorf("legacy compatibility requires embedded migration %s", filename)
		}
		if err := applyNonTransactionalMigration(ctx, conn, string(migration.content)); err != nil {
			return fmt.Errorf("verifying legacy index via %s: %w", filename, err)
		}
	}
	return nil
}
