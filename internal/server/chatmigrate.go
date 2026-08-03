// chatmigrate.go holds the one-time conversion of pre-012 plaintext chat
// history to ciphertext (91). The server is, by the documented trust model,
// a holder of the channel/global scope keys, and those rows are readable by
// it right now — so it can seal them. The alternative, destroying every
// message ever sent, is an enormous and avoidable user cost, and remains
// available as chat_legacy_history=purge.
package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// backfillMarker records that the legacy conversion has run. Its presence
// plus CountPlaintextBodies() == 0 is the whole idempotence check.
const backfillMarker = "chat_ciphertext_backfill_at"

// backfillPageSize is the resume granularity: a crash re-reads at most this
// many rows, because SetChatCiphertext blanks body as it goes.
const backfillPageSize = 500

// deadTupleWarning is logged verbatim after a successful encrypt pass.
// Blanking a column does not erase the old tuple, and pretending otherwise
// would be the dishonest half of this feature.
const deadTupleWarning = "legacy chat history encrypted in place. THE OLD PLAINTEXT IS NOT GONE: " +
	"run VACUUM (FULL, ANALYZE) chat_messages, then rotate or destroy every base " +
	"backup and WAL archive taken before this upgrade."

// EncryptLegacyChatHistory converts pre-012 plaintext bodies to ciphertext in
// place. It runs once, before the listener binds, and is fatal on error: a
// partially encrypted table behind a NOT VALID constraint is exactly the
// silent-plaintext state the requirement forbids. Idempotent and resumable
// by id.
//
// mode is chat_legacy_history: "encrypt" (default) seals every row under a
// freshly minted generation of its own scope; "purge" tombstones them all.
func (s *TCPServer) EncryptLegacyChatHistory(ctx context.Context, mode string) error {
	if s.deps == nil || s.deps.Chat == nil {
		return errors.New("chat store unavailable")
	}
	if s.chatKeys == nil || !s.chatKeys.configured() {
		return errChatKeysUnconfigured
	}
	if mode == "" {
		mode = "encrypt"
	}
	if mode != "encrypt" && mode != "purge" {
		return fmt.Errorf("invalid chat_legacy_history %q: must be \"encrypt\" or \"purge\"", mode)
	}

	if done, err := s.backfillAlreadyDone(ctx); err != nil {
		return err
	} else if done {
		return nil
	}

	var converted int64
	var err error
	if mode == "purge" {
		converted, err = s.deps.Chat.PurgeLegacyPlaintext(ctx)
	} else {
		converted, err = s.sealLegacyRows(ctx)
	}
	if err != nil {
		return err
	}

	// The MOTD and the announcement are operator-authored broadcast text in
	// the same dump; 012 deliberately does not blank them, so they are sealed
	// here in place rather than destroyed. This runs under "purge" too: the
	// marker below stops the backfill ever running again, so skipping it would
	// leave them plaintext at rest until someone happened to read them.
	if err := s.sealLegacyServerSettings(ctx); err != nil {
		return err
	}

	if n, err := s.deps.Chat.CountPlaintextBodies(ctx); err != nil {
		return fmt.Errorf("auditing plaintext chat bodies: %w", err)
	} else if n != 0 {
		return fmt.Errorf("legacy chat backfill left %d plaintext bodies behind", n)
	}
	// Both constraints only become enforceable once the table is clean; each
	// VALIDATE is a full scan, so it runs exactly here and never again.
	if err := s.deps.Chat.ValidateChatNoPlaintext(ctx); err != nil {
		return fmt.Errorf("validating chat plaintext constraints: %w", err)
	}
	if err := s.deps.Chat.SetServerSetting(ctx, backfillMarker, time.Now().UTC().Format(time.RFC3339), 0); err != nil {
		return fmt.Errorf("stamping %s: %w", backfillMarker, err)
	}

	if mode == "purge" {
		s.logger.Warn("legacy chat history purged (chat_legacy_history=purge)", zap.Int64("rows", converted))
		return nil
	}
	s.logger.Warn(deadTupleWarning, zap.Int64("rows", converted))
	return nil
}

// backfillAlreadyDone reports whether the marker is set, asserting that the
// table really is clean rather than trusting the stamp alone.
func (s *TCPServer) backfillAlreadyDone(ctx context.Context) (bool, error) {
	stamp, _, err := s.deps.Chat.GetServerSetting(ctx, backfillMarker)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", backfillMarker, err)
	}
	if stamp == "" {
		return false, nil
	}
	n, err := s.deps.Chat.CountPlaintextBodies(ctx)
	if err != nil {
		return false, fmt.Errorf("auditing plaintext chat bodies: %w", err)
	}
	if n != 0 {
		return false, fmt.Errorf("%s is set but %d chat bodies are still plaintext", backfillMarker, n)
	}
	return true, nil
}

// sealLegacyRows pages the unsealed rows and seals each under a generation of
// its own scope. The store selects on an empty body_enc rather than a
// non-empty body: an encrypted message whose plaintext was the empty string
// stores exactly such a row, and sealing the empty string yields a valid
// non-empty ciphertext that satisfies chat_messages_sealed.
func (s *TCPServer) sealLegacyRows(ctx context.Context) (int64, error) {
	var (
		afterID int64
		total   int64
	)
	for {
		rows, err := s.deps.Chat.LegacyPlaintextPage(ctx, afterID, backfillPageSize)
		if err != nil {
			return total, fmt.Errorf("reading legacy chat page: %w", err)
		}
		if len(rows) == 0 {
			return total, nil
		}
		for _, row := range rows {
			// EnsureScope, not current: a scope whose only trace is legacy
			// history has no generation yet, and this is a boot-time path
			// with no attacker-supplied scope id.
			if _, _, err := s.chatKeys.EnsureScope(ctx, row.ChannelID); err != nil {
				return total, fmt.Errorf("minting scope %d for legacy chat: %w", row.ChannelID, err)
			}
			keyID, ct, err := s.chatKeys.seal(ctx, row.ChannelID, row.Body)
			if err != nil {
				return total, fmt.Errorf("sealing legacy chat row %d: %w", row.ID, err)
			}
			if err := s.deps.Chat.SetChatCiphertext(ctx, row.ID, ct, keyID); err != nil {
				return total, err
			}
			total++
			afterID = row.ID
		}
	}
}

// sealLegacyServerSettings seals the operator-authored broadcast texts under
// the global generation, leaving the values intact.
func (s *TCPServer) sealLegacyServerSettings(ctx context.Context) error {
	if _, _, err := s.chatKeys.EnsureScope(ctx, globalChatScope); err != nil {
		return fmt.Errorf("minting the global scope for server settings: %w", err)
	}
	for _, key := range []string{"motd", "announcement"} {
		value, gen, err := s.deps.Chat.GetServerSetting(ctx, key)
		if err != nil {
			return fmt.Errorf("reading server setting %s: %w", key, err)
		}
		if value == "" || gen != 0 {
			continue // unset, or already sealed by an earlier partial run
		}
		id, ct, err := s.chatKeys.seal(ctx, globalChatScope, value)
		if err != nil {
			return fmt.Errorf("sealing server setting %s: %w", key, err)
		}
		if err := s.deps.Chat.SetServerSetting(ctx, key, ct, id); err != nil {
			return fmt.Errorf("storing sealed server setting %s: %w", key, err)
		}
	}
	return nil
}
