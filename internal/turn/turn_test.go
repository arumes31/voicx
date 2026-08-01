package turn

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"testing"
	"time"
)

// TestCredentialsGolden verifies the exact wire format against an
// independently computed reference value (openssl dgst -sha1 -hmac).
func TestCredentialsGolden(t *testing.T) {
	now := time.Unix(1704067200, 0) // 2024-01-01T00:00:00Z
	username, credential := Credentials("s3cr3t", "user1", time.Hour, now)

	if want := "1704070800:user1"; username != want {
		t.Fatalf("username = %q, want %q", username, want)
	}
	// Reference: printf '1704070800:user1' | openssl dgst -sha1 -hmac 's3cr3t' -binary | base64
	if want := "a8d4ALUK1tq6YtNZYtXEnBaqvkg="; credential != want {
		t.Fatalf("credential = %q, want %q", credential, want)
	}
}

// TestCredentialsAlgorithm recomputes the credential with the coturn REST API
// algorithm straight from the standard library and compares.
func TestCredentialsAlgorithm(t *testing.T) {
	now := time.Unix(1700000000, 0)
	username, credential := Credentials("another-secret", "abc=", 2*time.Hour, now)

	if want := "1700007200:abc="; username != want {
		t.Fatalf("username = %q, want %q", username, want)
	}
	mac := hmac.New(sha1.New, []byte("another-secret"))
	mac.Write([]byte(username))
	if want := base64.StdEncoding.EncodeToString(mac.Sum(nil)); credential != want {
		t.Fatalf("credential = %q, want %q", credential, want)
	}
}

// TestCredentialsExpiryInUsername verifies the TTL lands in the username's
// expiry prefix (coturn rejects expired usernames).
func TestCredentialsExpiryInUsername(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	username, _ := Credentials("s", "u", 24*time.Hour, now)
	want := time.Now().Add(24 * time.Hour).Unix()
	var expiry int64
	if _, err := fmt.Sscanf(username, "%d:", &expiry); err != nil {
		t.Fatalf("username %q has no expiry prefix: %v", username, err)
	}
	if expiry < want-2 || expiry > want+2 {
		t.Fatalf("expiry = %d, want ~%d", expiry, want)
	}
}
