package store

import "testing"

func TestPIICipherBindsCiphertextToFieldAndUser(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	cipher, err := NewPIICipher(key)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := cipher.seal("person@example.test", piiAAD(7, "email"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := cipher.open(blob, piiAAD(7, "email"))
	if err != nil || got != "person@example.test" {
		t.Fatalf("open = %q, %v", got, err)
	}
	if _, err := cipher.open(blob, piiAAD(8, "email")); err == nil {
		t.Fatal("ciphertext opened for a different user")
	}
	if _, err := cipher.open(blob, piiAAD(7, "last_ip")); err == nil {
		t.Fatal("ciphertext opened in a different column")
	}
}
