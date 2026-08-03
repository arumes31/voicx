package e2ee

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
)

func TestX3DHAndOutOfOrderRatchet(t *testing.T) {
	aliceIdentity, _ := GenerateX25519()
	bobIdentity, _ := GenerateX25519()
	bobSigned, _ := GenerateX25519()
	bobOneTime, _ := GenerateX25519()
	_, signingPrivate, _ := ed25519.GenerateKey(rand.Reader)
	bundle := NewPreKeyBundle(bobIdentity, signingPrivate, 4, bobSigned, 9, bobOneTime)
	sharedA, initial, err := InitiateX3DH(aliceIdentity.Private, bundle)
	if err != nil {
		t.Fatal(err)
	}
	sharedB, err := RespondX3DH(bobIdentity.Private, bobSigned.Private, bobOneTime.Private, aliceIdentity.Public, initial)
	if err != nil || !EqualFingerprint(sharedA, sharedB) {
		t.Fatalf("X3DH mismatch: %v", err)
	}
	alice := NewRatchet(sharedA, true)
	bob := NewRatchet(sharedB, false)
	m0, _ := alice.Encrypt([]byte("zero"), nil)
	m1, _ := alice.Encrypt([]byte("one"), nil)
	m2, _ := alice.Encrypt([]byte("two"), nil)
	if got, err := bob.Decrypt(m2, nil); err != nil || string(got) != "two" {
		t.Fatalf("message 2: %q %v", got, err)
	}
	if bob.SkippedCount() != 2 {
		t.Fatalf("skipped = %d", bob.SkippedCount())
	}
	for _, tc := range []struct {
		msg  Message
		want string
	}{{m0, "zero"}, {m1, "one"}} {
		if got, err := bob.Decrypt(tc.msg, nil); err != nil || string(got) != tc.want {
			t.Fatalf("delayed message: %q %v", got, err)
		}
	}
}

func TestSkippedKeyLimitAndChunkBinding(t *testing.T) {
	secret := make([]byte, 32)
	r := NewRatchet(secret, false)
	if _, err := r.Decrypt(Message{Number: MaxSkippedKeys + 1}, nil); !errors.Is(err, ErrTooManySkipped) {
		t.Fatalf("gap error = %v", err)
	}
	blob, err := EncryptFileChunk(secret, "file-a", 3, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := DecryptFileChunk(secret, "file-a", 3, blob); err != nil || string(plain) != "payload" {
		t.Fatalf("chunk = %q, %v", plain, err)
	}
	if _, err := DecryptFileChunk(secret, "file-a", 4, blob); err == nil {
		t.Fatal("chunk opened under a different index")
	}
}

func TestSignedPreKeyTamperRejected(t *testing.T) {
	identity, _ := GenerateX25519()
	signed, _ := GenerateX25519()
	oneTime, _ := GenerateX25519()
	_, signingPrivate, _ := ed25519.GenerateKey(rand.Reader)
	bundle := NewPreKeyBundle(identity, signingPrivate, 1, signed, 2, oneTime)
	bundle.SignedPreKey[0] ^= 1
	if _, _, err := InitiateX3DH(identity.Private, bundle); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("error = %v", err)
	}
}

func TestInitiateX3DHOmitOneTimeKeyID(t *testing.T) {
	alice, _ := GenerateX25519()
	bob, _ := GenerateX25519()
	signed, _ := GenerateX25519()
	_, signingPrivate, _ := ed25519.GenerateKey(rand.Reader)
	bundle := NewPreKeyBundle(bob, signingPrivate, 1, signed, 99, KeyPair{})
	_, initial, err := InitiateX3DH(alice.Private, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if initial.OneTimePreKeyID != 0 {
		t.Fatalf("one-time key id = %d, want 0", initial.OneTimePreKeyID)
	}
}

func TestRatchetRejectsTamperWithoutAdvancing(t *testing.T) {
	secret := make([]byte, 32)
	sender := NewRatchet(secret, true)
	receiver := NewRatchet(secret, false)
	m0, _ := sender.Encrypt([]byte("zero"), nil)
	_, _ = sender.Encrypt([]byte("one"), nil)
	m2, _ := sender.Encrypt([]byte("two"), nil)
	tampered := m2
	tampered.Ciphertext = append([]byte(nil), m2.Ciphertext...)
	tampered.Ciphertext[len(tampered.Ciphertext)-1] ^= 1
	if _, err := receiver.Decrypt(tampered, nil); err == nil {
		t.Fatal("tampered future message decrypted")
	}
	if receiver.SkippedCount() != 0 {
		t.Fatalf("failed authentication committed %d skipped keys", receiver.SkippedCount())
	}
	if got, err := receiver.Decrypt(m0, nil); err != nil || string(got) != "zero" {
		t.Fatalf("message after failed authentication = %q, %v", got, err)
	}
}

func TestRatchetKeepsSkippedKeyAfterTamper(t *testing.T) {
	secret := make([]byte, 32)
	sender := NewRatchet(secret, true)
	receiver := NewRatchet(secret, false)
	m0, _ := sender.Encrypt([]byte("zero"), nil)
	_, _ = sender.Encrypt([]byte("one"), nil)
	m2, _ := sender.Encrypt([]byte("two"), nil)
	if _, err := receiver.Decrypt(m2, nil); err != nil {
		t.Fatal(err)
	}
	tampered := m0
	tampered.Ciphertext = append([]byte(nil), m0.Ciphertext...)
	tampered.Ciphertext[len(tampered.Ciphertext)-1] ^= 1
	if _, err := receiver.Decrypt(tampered, nil); err == nil {
		t.Fatal("tampered skipped message decrypted")
	}
	if receiver.SkippedCount() != 2 {
		t.Fatalf("skipped count after tamper = %d, want 2", receiver.SkippedCount())
	}
	if got, err := receiver.Decrypt(m0, nil); err != nil || string(got) != "zero" {
		t.Fatalf("valid skipped message = %q, %v", got, err)
	}
}

func TestRatchetAcceptsDelayedPreviousStep(t *testing.T) {
	secret := make([]byte, 32)
	sender := NewRatchet(secret, true)
	receiver := NewRatchet(secret, false)
	delayed, _ := sender.Encrypt([]byte("old"), nil)
	dh := []byte("new ratchet material")
	sender.RatchetStep(dh)
	receiver.RatchetStep(dh)
	current, _ := sender.Encrypt([]byte("new"), nil)
	if got, err := receiver.Decrypt(current, nil); err != nil || string(got) != "new" {
		t.Fatalf("current step = %q, %v", got, err)
	}
	if got, err := receiver.Decrypt(delayed, nil); err != nil || string(got) != "old" {
		t.Fatalf("delayed previous step = %q, %v", got, err)
	}
}
