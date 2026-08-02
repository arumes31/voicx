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
