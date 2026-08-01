// custom.go implements the store layer for custom per-client key/value
// properties (wave 10a, 222; migration 011).
package store

import (
	"context"
	"fmt"
)

// CustomEntry is one custom property (222).
type CustomEntry struct {
	Key   string
	Value string
}

// CustomSet upserts a custom property for a user (by unique ID).
func (s *Store) CustomSet(ctx context.Context, uniqueID, key, value string) error {
	const q = `INSERT INTO client_custom (unique_id, key, value) VALUES ($1, $2, $3)
	          ON CONFLICT (unique_id, key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()`
	if _, err := s.db.ExecContext(ctx, q, uniqueID, key, value); err != nil {
		return fmt.Errorf("setting custom property: %w", err)
	}
	return nil
}

// CustomDel removes a custom property.
func (s *Store) CustomDel(ctx context.Context, uniqueID, key string) error {
	const q = `DELETE FROM client_custom WHERE unique_id = $1 AND key = $2`
	if _, err := s.db.ExecContext(ctx, q, uniqueID, key); err != nil {
		return fmt.Errorf("deleting custom property: %w", err)
	}
	return nil
}

// CustomInfo returns all custom properties of a user, ordered by key.
func (s *Store) CustomInfo(ctx context.Context, uniqueID string) ([]CustomEntry, error) {
	const q = `SELECT key, value FROM client_custom WHERE unique_id = $1 ORDER BY key`
	rows, err := s.db.QueryContext(ctx, q, uniqueID)
	if err != nil {
		return nil, fmt.Errorf("querying custom properties: %w", err)
	}
	defer rows.Close()
	var out []CustomEntry
	for rows.Next() {
		var e CustomEntry
		if err := rows.Scan(&e.Key, &e.Value); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
