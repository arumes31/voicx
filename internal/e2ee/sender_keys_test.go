package e2ee

import "testing"

func TestSenderKeyBindsMessagesToTheirGroup(t *testing.T) {
	secret := []byte("sender-key group binding test secret")
	sender := NewSenderKey(secret, true)
	receiver := NewSenderKey(secret, false)

	message, err := sender.Encrypt([]byte("for group alpha"), "alpha")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := receiver.Decrypt(message, "beta"); err == nil {
		t.Fatal("Decrypt opened a message under another group")
	}
	plain, err := receiver.Decrypt(message, "alpha")
	if err != nil {
		t.Fatalf("Decrypt under the originating group: %v", err)
	}
	if got, want := string(plain), "for group alpha"; got != want {
		t.Fatalf("plaintext = %q, want %q", got, want)
	}
}

func TestSenderKeyFailedAuthenticationDoesNotConsumeReceiveState(t *testing.T) {
	secret := []byte("sender-key failed authentication test")
	sender := NewSenderKey(secret, true)
	receiver := NewSenderKey(secret, false)

	first, err := sender.Encrypt([]byte("first"), "group")
	if err != nil {
		t.Fatalf("encrypt first: %v", err)
	}
	if _, err := sender.Encrypt([]byte("skipped"), "group"); err != nil {
		t.Fatalf("encrypt skipped: %v", err)
	}
	future, err := sender.Encrypt([]byte("future"), "group")
	if err != nil {
		t.Fatalf("encrypt future: %v", err)
	}
	tampered := future
	tampered.Ciphertext = append([]byte(nil), future.Ciphertext...)
	tampered.Ciphertext[len(tampered.Ciphertext)-1] ^= 1

	if _, err := receiver.Decrypt(tampered, "group"); err == nil {
		t.Fatal("tampered future message decrypted")
	}
	if got := receiver.ratchet.SkippedCount(); got != 0 {
		t.Fatalf("failed authentication consumed %d skipped keys", got)
	}
	plain, err := receiver.Decrypt(first, "group")
	if err != nil {
		t.Fatalf("decrypt after failed authentication: %v", err)
	}
	if got, want := string(plain), "first"; got != want {
		t.Fatalf("plaintext = %q, want %q", got, want)
	}
}

func TestSenderKeyRotationRoundTrip(t *testing.T) {
	secret := []byte("sender-key rotation initial secret")
	rotation := []byte("sender-key rotation material")
	sender := NewSenderKey(secret, true)
	receiver := NewSenderKey(secret, false)

	before, err := sender.Encrypt([]byte("before rotation"), "group")
	if err != nil {
		t.Fatalf("encrypt before rotation: %v", err)
	}
	sender.Rotate(rotation)
	receiver.Rotate(rotation)
	after, err := sender.Encrypt([]byte("after rotation"), "group")
	if err != nil {
		t.Fatalf("encrypt after rotation: %v", err)
	}

	for _, test := range []struct {
		name string
		msg  Message
		want string
	}{
		{name: "current generation", msg: after, want: "after rotation"},
		{name: "retained previous generation", msg: before, want: "before rotation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			plain, err := receiver.Decrypt(test.msg, "group")
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if got := string(plain); got != test.want {
				t.Fatalf("plaintext = %q, want %q", got, test.want)
			}
		})
	}
}
