package e2ee

// SenderKey is the group-chat sender-key construction: each sender owns one
// outbound chain and recipients hold the corresponding inbound chain. Group
// membership changes rotate the root through Rotate.
type SenderKey struct{ ratchet *Ratchet }

func NewSenderKey(secret []byte, sender bool) *SenderKey {
	return &SenderKey{ratchet: NewRatchet(secret, sender)}
}

func (s *SenderKey) Encrypt(plain []byte, groupID string) (Message, error) {
	return s.ratchet.Encrypt(plain, []byte("group:"+groupID))
}

func (s *SenderKey) Decrypt(message Message, groupID string) ([]byte, error) {
	return s.ratchet.Decrypt(message, []byte("group:"+groupID))
}

func (s *SenderKey) Rotate(sharedSecret []byte) { s.ratchet.RatchetStep(sharedSecret) }
