// Package e2ee contains the cryptographic state machines used by private chat.
package e2ee

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

const MaxSkippedKeys = 2000

var (
	ErrInvalidSignature = errors.New("invalid signed prekey signature")
	ErrTooManySkipped   = errors.New("message gap exceeds skipped-key limit")
	ErrSkippedKeyGone   = errors.New("skipped message key is unavailable")
)

type KeyPair struct {
	Private []byte
	Public  []byte
}

func GenerateX25519() (KeyPair, error) {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return KeyPair{}, err
	}
	return KeyPair{Private: key.Bytes(), Public: key.PublicKey().Bytes()}, nil
}

func privateKey(raw []byte) (*ecdh.PrivateKey, error) { return ecdh.X25519().NewPrivateKey(raw) }
func publicKey(raw []byte) (*ecdh.PublicKey, error)   { return ecdh.X25519().NewPublicKey(raw) }

func exchange(private, public []byte) ([]byte, error) {
	priv, err := privateKey(private)
	if err != nil {
		return nil, err
	}
	pub, err := publicKey(public)
	if err != nil {
		return nil, err
	}
	return priv.ECDH(pub)
}

// PreKeyBundle is the public X3DH material published for one device.
type PreKeyBundle struct {
	IdentityDH      []byte
	SigningPublic   []byte
	SignedPreKeyID  uint32
	SignedPreKey    []byte
	Signature       []byte
	OneTimePreKeyID uint32
	OneTimePreKey   []byte
}

func preKeySignedBytes(id uint32, public []byte) []byte {
	prefix := []byte("voicx:signed-prekey:")
	out := make([]byte, len(prefix)+4+len(public))
	copy(out, prefix)
	binary.BigEndian.PutUint32(out[len(prefix):], id)
	copy(out[len(prefix)+4:], public)
	return out
}

func NewPreKeyBundle(identityDH KeyPair, signingPrivate ed25519.PrivateKey, signedID uint32, signedPreKey KeyPair, oneTimeID uint32, oneTimePreKey KeyPair) PreKeyBundle {
	signingPublic := signingPrivate.Public().(ed25519.PublicKey)
	return PreKeyBundle{
		IdentityDH: identityDH.Public, SigningPublic: append([]byte(nil), signingPublic...),
		SignedPreKeyID: signedID, SignedPreKey: signedPreKey.Public,
		Signature:       ed25519.Sign(signingPrivate, preKeySignedBytes(signedID, signedPreKey.Public)),
		OneTimePreKeyID: oneTimeID, OneTimePreKey: oneTimePreKey.Public,
	}
}

// VerifyPreKeyBundle validates the binding between a device signing identity
// and its medium-lived X25519 signed prekey without initiating a session.
func VerifyPreKeyBundle(bundle PreKeyBundle) bool {
	return len(bundle.IdentityDH) == 32 && len(bundle.SignedPreKey) == 32 &&
		len(bundle.SigningPublic) == ed25519.PublicKeySize && len(bundle.Signature) == ed25519.SignatureSize &&
		ed25519.Verify(bundle.SigningPublic, preKeySignedBytes(bundle.SignedPreKeyID, bundle.SignedPreKey), bundle.Signature)
}

type X3DHInitial struct {
	EphemeralPublic []byte
	SignedPreKeyID  uint32
	OneTimePreKeyID uint32
}

// InitiateX3DH authenticates the responder's signed prekey and derives the
// same 32-byte session secret RespondX3DH derives from the private prekeys.
func InitiateX3DH(identityPrivate []byte, bundle PreKeyBundle) ([]byte, X3DHInitial, error) {
	if !VerifyPreKeyBundle(bundle) {
		return nil, X3DHInitial{}, ErrInvalidSignature
	}
	ephemeral, err := GenerateX25519()
	if err != nil {
		return nil, X3DHInitial{}, err
	}
	dh1, err := exchange(identityPrivate, bundle.SignedPreKey)
	if err != nil {
		return nil, X3DHInitial{}, err
	}
	dh2, err := exchange(ephemeral.Private, bundle.IdentityDH)
	if err != nil {
		return nil, X3DHInitial{}, err
	}
	dh3, err := exchange(ephemeral.Private, bundle.SignedPreKey)
	if err != nil {
		return nil, X3DHInitial{}, err
	}
	material := append(append(dh1, dh2...), dh3...)
	if len(bundle.OneTimePreKey) > 0 {
		dh4, err := exchange(ephemeral.Private, bundle.OneTimePreKey)
		if err != nil {
			return nil, X3DHInitial{}, err
		}
		material = append(material, dh4...)
	}
	return hkdf(material, nil, []byte("voicx:x3dh"), 32), X3DHInitial{
		EphemeralPublic: ephemeral.Public, SignedPreKeyID: bundle.SignedPreKeyID, OneTimePreKeyID: bundle.OneTimePreKeyID,
	}, nil
}

func RespondX3DH(identityPrivate, signedPreKeyPrivate, oneTimePreKeyPrivate, initiatorIdentityPublic []byte, initial X3DHInitial) ([]byte, error) {
	dh1, err := exchange(signedPreKeyPrivate, initiatorIdentityPublic)
	if err != nil {
		return nil, err
	}
	dh2, err := exchange(identityPrivate, initial.EphemeralPublic)
	if err != nil {
		return nil, err
	}
	dh3, err := exchange(signedPreKeyPrivate, initial.EphemeralPublic)
	if err != nil {
		return nil, err
	}
	material := append(append(dh1, dh2...), dh3...)
	if len(oneTimePreKeyPrivate) > 0 {
		dh4, err := exchange(oneTimePreKeyPrivate, initial.EphemeralPublic)
		if err != nil {
			return nil, err
		}
		material = append(material, dh4...)
	}
	return hkdf(material, nil, []byte("voicx:x3dh"), 32), nil
}

func hkdf(secret, salt, info []byte, size int) []byte {
	if salt == nil {
		salt = make([]byte, sha256.Size)
	}
	extract := hmac.New(sha256.New, salt)
	_, _ = extract.Write(secret)
	prk := extract.Sum(nil)
	out := make([]byte, 0, size)
	var previous []byte
	for counter := byte(1); len(out) < size; counter++ {
		expand := hmac.New(sha256.New, prk)
		_, _ = expand.Write(previous)
		_, _ = expand.Write(info)
		_, _ = expand.Write([]byte{counter})
		previous = expand.Sum(nil)
		out = append(out, previous...)
	}
	return out[:size]
}

func chainStep(chain []byte) (next, message []byte) {
	next = hkdf(chain, nil, []byte("voicx:chain:next"), 32)
	message = hkdf(chain, nil, []byte("voicx:chain:message"), 32)
	return next, message
}

type Message struct {
	Number     uint32
	Ciphertext []byte // nonce || AES-GCM ciphertext
}

// Ratchet provides per-message forward-secret chain keys and a bounded
// out-of-order skipped-key cache. RatchetStep mixes a new DH result into the
// root when either side rotates its ratchet key.
type Ratchet struct {
	mu        sync.Mutex
	root      []byte
	sendChain []byte
	recvChain []byte
	sendN     uint32
	recvN     uint32
	skipped   map[uint32][]byte
	initiator bool
}

func NewRatchet(sharedSecret []byte, initiator bool) *Ratchet {
	root := hkdf(sharedSecret, nil, []byte("voicx:double-ratchet:root"), 32)
	a := hkdf(root, nil, []byte("voicx:double-ratchet:a"), 32)
	b := hkdf(root, nil, []byte("voicx:double-ratchet:b"), 32)
	if initiator {
		return &Ratchet{root: root, sendChain: a, recvChain: b, skipped: map[uint32][]byte{}, initiator: true}
	}
	return &Ratchet{root: root, sendChain: b, recvChain: a, skipped: map[uint32][]byte{}}
}

func (r *Ratchet) RatchetStep(dhSecret []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.root = hkdf(dhSecret, r.root, []byte("voicx:double-ratchet:step"), 32)
	a := hkdf(r.root, nil, []byte("voicx:double-ratchet:a"), 32)
	b := hkdf(r.root, nil, []byte("voicx:double-ratchet:b"), 32)
	if r.initiator {
		r.sendChain, r.recvChain = a, b
	} else {
		r.sendChain, r.recvChain = b, a
	}
	r.sendN, r.recvN = 0, 0
	r.skipped = map[uint32][]byte{}
}

func messageAAD(number uint32, aad []byte) []byte {
	out := make([]byte, 4+len(aad))
	binary.BigEndian.PutUint32(out, number)
	copy(out[4:], aad)
	return out
}

func sealAES(key, plain, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return append(nonce, aead.Seal(nil, nonce, plain, aad)...), nil
}

func openAES(key, blob, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(blob) < aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("invalid ciphertext")
	}
	return aead.Open(nil, blob[:aead.NonceSize()], blob[aead.NonceSize():], aad)
}

func (r *Ratchet) Encrypt(plain, aad []byte) (Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	next, key := chainStep(r.sendChain)
	number := r.sendN
	blob, err := sealAES(key, plain, messageAAD(number, aad))
	if err != nil {
		return Message{}, err
	}
	r.sendChain = next
	r.sendN++
	return Message{Number: number, Ciphertext: blob}, nil
}

func (r *Ratchet) Decrypt(message Message, aad []byte) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if message.Number < r.recvN {
		key, ok := r.skipped[message.Number]
		if !ok {
			return nil, ErrSkippedKeyGone
		}
		delete(r.skipped, message.Number)
		return openAES(key, message.Ciphertext, messageAAD(message.Number, aad))
	}
	if uint64(message.Number-r.recvN) > MaxSkippedKeys || len(r.skipped)+int(message.Number-r.recvN) > MaxSkippedKeys {
		return nil, ErrTooManySkipped
	}
	for r.recvN < message.Number {
		next, key := chainStep(r.recvChain)
		r.recvChain = next
		r.skipped[r.recvN] = key
		r.recvN++
	}
	next, key := chainStep(r.recvChain)
	plain, err := openAES(key, message.Ciphertext, messageAAD(message.Number, aad))
	if err != nil {
		return nil, err
	}
	r.recvChain = next
	r.recvN++
	return plain, nil
}

func (r *Ratchet) SkippedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.skipped)
}

// DeriveMessageKey exposes the common HKDF construction for protocol layers
// that need a separately labelled key without reusing a chain key directly.
func DeriveMessageKey(root []byte, conversationID string, number uint64) []byte {
	info := make([]byte, len(conversationID)+8)
	copy(info, conversationID)
	binary.BigEndian.PutUint64(info[len(conversationID):], number)
	return hkdf(root, nil, append([]byte("voicx:message:"), info...), 32)
}

func EncryptFileChunk(master []byte, fileID string, index uint64, plain []byte) ([]byte, error) {
	key := DeriveMessageKey(master, "file:"+fileID, index)
	var aad [8]byte
	binary.BigEndian.PutUint64(aad[:], index)
	return sealAES(key, plain, append([]byte(fileID), aad[:]...))
}

func DecryptFileChunk(master []byte, fileID string, index uint64, blob []byte) ([]byte, error) {
	key := DeriveMessageKey(master, "file:"+fileID, index)
	var aad [8]byte
	binary.BigEndian.PutUint64(aad[:], index)
	return openAES(key, blob, append([]byte(fileID), aad[:]...))
}

// EqualFingerprint avoids data-dependent early exits in identity comparisons.
func EqualFingerprint(a, b []byte) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare(a, b) == 1
}

func ValidateKeySize(key []byte) error {
	if len(key) != 32 {
		return fmt.Errorf("key length %d, want 32", len(key))
	}
	return nil
}
