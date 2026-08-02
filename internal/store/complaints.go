// complaints.go implements the complaints queries against the complaints
// table (migration 004_complaints.sql).
package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrComplaintLimit is returned when a reporter already has the maximum
// number of open complaints.
var ErrComplaintLimit = errors.New("complaint limit reached")

// MaxOpenComplaints is the maximum number of open complaints per reporter.
const MaxOpenComplaints = 5

// Complaint describes one complaint row.
type Complaint struct {
	ID        int64
	Reporter  string
	Target    string
	Reason    string
	CreatedAt time.Time
}

// AddComplaint records a complaint. A reporter may have at most
// MaxOpenComplaints open complaints; beyond that ErrComplaintLimit is
// returned.
func (s *Store) AddComplaint(ctx context.Context, reporter, target, reason string) error {
	var open int
	const cq = `SELECT COUNT(*) FROM complaints WHERE reporter = $1`
	if err := s.db.QueryRowContext(ctx, cq, reporter).Scan(&open); err != nil {
		return fmt.Errorf("counting open complaints: %w", err)
	}
	if open >= MaxOpenComplaints {
		return ErrComplaintLimit
	}

	const q = `INSERT INTO complaints (reporter, target, reason) VALUES ($1, $2, $3)`
	if _, err := s.db.ExecContext(ctx, q, reporter, target, reason); err != nil {
		return fmt.Errorf("inserting complaint: %w", err)
	}
	return nil
}

// ListComplaints returns all complaints, oldest first.
func (s *Store) ListComplaints(ctx context.Context) ([]Complaint, error) {
	const q = `SELECT id, reporter, target, reason, created_at FROM complaints ORDER BY id`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("querying complaints: %w", err)
	}
	defer rows.Close()

	var out []Complaint
	for rows.Next() {
		var c Complaint
		if err := rows.Scan(&c.ID, &c.Reporter, &c.Target, &c.Reason, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning complaint: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteComplaint removes one complaint by id.
func (s *Store) DeleteComplaint(ctx context.Context, id int64) error {
	const q = `DELETE FROM complaints WHERE id = $1`
	if _, err := s.db.ExecContext(ctx, q, id); err != nil {
		return fmt.Errorf("deleting complaint: %w", err)
	}
	return nil
}

// DeleteComplaintsAgainst removes the complaints filed against target. An
// empty reporter clears every complaint against that target; otherwise only
// the ones that reporter filed. It returns the number of rows removed so the
// caller can distinguish "cleared" from "nothing matched" (173).
func (s *Store) DeleteComplaintsAgainst(ctx context.Context, target, reporter string) (int64, error) {
	q := `DELETE FROM complaints WHERE target = $1`
	args := []any{target}
	if reporter != "" {
		q += ` AND reporter = $2`
		args = append(args, reporter)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("deleting complaints: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeleteAllComplaints removes all complaints.
func (s *Store) DeleteAllComplaints(ctx context.Context) error {
	const q = `DELETE FROM complaints`
	if _, err := s.db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("deleting all complaints: %w", err)
	}
	return nil
}
