package auth

import (
	"errors"
	"fmt"
	"testing"

	"github.com/lib/pq"
)

func TestGenerateChallengeHasProtocolLength(t *testing.T) {
	t.Parallel()

	challenge, err := GenerateChallenge()
	if err != nil {
		t.Fatalf("GenerateChallenge: %v", err)
	}
	if len(challenge) != 32 {
		t.Fatalf("challenge length = %d, want 32", len(challenge))
	}
}

func TestIsUniqueViolation(t *testing.T) {
	t.Parallel()

	unique := &pq.Error{Code: "23505"}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", want: false},
		{name: "unrelated", err: errors.New("database unavailable"), want: false},
		{name: "different PostgreSQL code", err: &pq.Error{Code: "23503"}, want: false},
		{name: "unique violation", err: unique, want: true},
		{name: "wrapped unique violation", err: fmt.Errorf("insert failed: %w", unique), want: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := isUniqueViolation(test.err); got != test.want {
				t.Errorf("isUniqueViolation(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}
