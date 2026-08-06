package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"voicx/internal/e2ee"
	"voicx/internal/safecast"
)

func TestPublishPreKeyBundleValidatesBeforeDatabaseAccess(t *testing.T) {
	s := &Store{}
	err := s.PublishPreKeyBundle(context.Background(), 1, PreKeyBundle{}, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid signed prekey bundle") {
		t.Fatalf("error = %v", err)
	}
}

func TestPublishPreKeysCapsBatchBeforeDatabaseAccess(t *testing.T) {
	s := &Store{}
	err := s.PublishPreKeys(context.Background(), 1, make([]PreKey, maxPreKeysPerPublish+1))
	if err == nil || !strings.Contains(err.Error(), "too many prekeys") {
		t.Fatalf("error = %v", err)
	}
}

func TestPublishAndConsumePreKeyBundle(t *testing.T) {
	s := testDBStore(t)
	ctx := context.Background()
	userID := seedTestUser(t, s, fmt.Sprintf("prekey-%d", time.Now().UnixNano()))
	identity, _ := e2ee.GenerateX25519()
	signed, _ := e2ee.GenerateX25519()
	oneTime, _ := e2ee.GenerateX25519()
	_, signingPrivate, _ := ed25519.GenerateKey(rand.Reader)
	bundle := e2ee.NewPreKeyBundle(identity, signingPrivate, 7, signed, 9, oneTime)
	stored := PreKeyBundle{
		IdentityDH: bundle.IdentityDH, SigningPublic: bundle.SigningPublic,
		SignedPreKeyID: bundle.SignedPreKeyID, SignedPreKey: bundle.SignedPreKey, Signature: bundle.Signature,
	}
	if err := s.PublishPreKeyBundle(ctx, userID, stored, []PreKey{{KeyID: 9, PublicKey: oneTime.Public, OneTime: true}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ConsumePreKeyBundle(ctx, userID)
	if err != nil || got.OneTimePreKey == nil || got.OneTimePreKey.KeyID != 9 {
		t.Fatalf("first consume = %+v, %v", got, err)
	}
	got, err = s.ConsumePreKeyBundle(ctx, userID)
	if err != nil || got.OneTimePreKey != nil {
		t.Fatalf("second consume = %+v, %v", got, err)
	}

	if _, err := s.DB().ExecContext(ctx, `UPDATE e2ee_prekey_bundles
		SET signed_prekey_id = $1 WHERE user_id = $2`, int64(math.MaxUint32)+1, userID); err != nil {
		t.Fatalf("seeding out-of-range signed prekey id: %v", err)
	}
	if _, err := s.ConsumePreKeyBundle(ctx, userID); !errors.Is(err, safecast.ErrOutOfRange) {
		t.Fatalf("ConsumePreKeyBundle() error = %v, want ErrOutOfRange", err)
	}
}
