// Package turn mints time-limited TURN credentials using the coturn REST API
// algorithm (draft-uberti-behave-turn-rest-00): the username is
// "<expiry unix>:<label>" and the credential is
// base64(HMAC-SHA1(static-auth-secret, username)). coturn verifies the
// credential against its --static-auth-secret, so voicx never needs to store
// per-user TURN passwords.
package turn

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"time"
)

// Credentials returns the username and credential a client should present to
// a TURN server that shares the given static auth secret. The credential is
// valid until now+ttl. label identifies the user (voicx uses the client's
// unique ID); it is embedded in the username for auditability on the TURN
// server side.
func Credentials(secret, label string, ttl time.Duration, now time.Time) (username, credential string) {
	username = fmt.Sprintf("%d:%s", now.Add(ttl).Unix(), label)
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))
	credential = base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return username, credential
}
