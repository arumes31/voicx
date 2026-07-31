// Package auth implements voicx authentication primitives: password hashing
// with Argon2id and identity generation using Ed25519 keys similar to
// TeamSpeak's Unique IDs.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. These are package-level so tests can override them with
// cheaper values (e.g. lower memory) to keep the test suite fast. The defaults
// follow the Argon2id RFC draft recommendations for interactive use.
var (
	// argonTime is the number of iterations (passes) over memory.
	argonTime = uint32(3)
	// argonMemory is the memory in KiB (64 MiB).
	argonMemory = uint32(64 * 1024)
	// argonThreads is the degree of parallelism.
	argonThreads = uint8(4)
	// argonKeyLen is the length of the derived key in bytes.
	argonKeyLen = uint32(32)
	// argonSaltLen is the length of the random salt in bytes.
	argonSaltLen = 16
)

// argon2idVersion is the Argon2 version reported in the encoded hash. Argon2id
// is parameterized by version 0x13 (19) in the reference implementation.
const argon2idVersion = 19

// ErrMalformedHash is returned when an encoded password hash cannot be parsed.
var ErrMalformedHash = errors.New("malformed argon2id hash")

// GenerateSalt returns n cryptographically random bytes from crypto/rand.
func GenerateSalt(n int) ([]byte, error) {
	if n <= 0 {
		return nil, errors.New("salt length must be positive")
	}
	salt := make([]byte, n)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generating salt: %w", err)
	}
	return salt, nil
}

// HashPassword derives an Argon2id hash of the password using the package-level
// parameters and a fresh random salt. The returned string is a self-describing
// encoded format:
//
//	argon2id$v=19$m=65536,t=3,p=4$<base64 salt>$<base64 hash>
//
// so it can be verified later without storing the parameters separately.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("password must not be empty")
	}
	salt, err := GenerateSalt(argonSaltLen)
	if err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return encodeHash(salt, hash), nil
}

// encodeHash assembles the self-describing encoded hash string.
func encodeHash(salt, hash []byte) string {
	return fmt.Sprintf("argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2idVersion,
		argonMemory,
		argonTime,
		argonThreads,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(hash),
	)
}

// VerifyPassword parses the encoded Argon2id hash, re-derives the key with the
// same parameters and salt, and compares the result in constant time. It
// returns nil on a match and an error on mismatch or a malformed hash.
func VerifyPassword(password, encodedHash string) error {
	salt, hash, memory, time, threads, err := parseEncodedHash(encodedHash)
	if err != nil {
		return err
	}

	other := argon2.IDKey([]byte(password), salt, time, memory, threads, uint32(len(hash)))
	if subtle.ConstantTimeCompare(hash, other) != 1 {
		return errors.New("password does not match hash")
	}
	return nil
}

// parseEncodedHash parses an "argon2id$v=19$m=...,t=...,p=...$<salt>$<hash>"
// string and returns the decoded salt, hash, and parameters.
func parseEncodedHash(encoded string) (salt, hash []byte, memory, time uint32, threads uint8, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" {
		return nil, nil, 0, 0, 0, fmt.Errorf("%w: expected 5 fields separated by '$', got %d", ErrMalformedHash, len(parts))
	}

	// parts[1] = "v=19"
	vField := strings.TrimPrefix(parts[1], "v=")
	if vField == parts[1] {
		return nil, nil, 0, 0, 0, fmt.Errorf("%w: missing version field", ErrMalformedHash)
	}
	version, err := strconv.Atoi(vField)
	if err != nil {
		return nil, nil, 0, 0, 0, fmt.Errorf("%w: invalid version: %v", ErrMalformedHash, err)
	}
	if version != argon2idVersion {
		return nil, nil, 0, 0, 0, fmt.Errorf("%w: unsupported argon2 version %d", ErrMalformedHash, version)
	}

	// parts[2] = "m=65536,t=3,p=4"
	memory, time, threads, err = parseParams(parts[2])
	if err != nil {
		return nil, nil, 0, 0, 0, err
	}

	salt, err = base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		return nil, nil, 0, 0, 0, fmt.Errorf("%w: invalid base64 salt: %v", ErrMalformedHash, err)
	}
	hash, err = base64.StdEncoding.DecodeString(parts[4])
	if err != nil {
		return nil, nil, 0, 0, 0, fmt.Errorf("%w: invalid base64 hash: %v", ErrMalformedHash, err)
	}
	if len(hash) == 0 {
		return nil, nil, 0, 0, 0, fmt.Errorf("%w: empty hash", ErrMalformedHash)
	}
	return salt, hash, memory, time, threads, nil
}

// parseParams parses the "m=...,t=...,p=..." parameter segment.
func parseParams(s string) (memory, time uint32, threads uint8, err error) {
	for _, field := range strings.Split(s, ",") {
		kv := strings.SplitN(field, "=", 2)
		if len(kv) != 2 {
			return 0, 0, 0, fmt.Errorf("%w: invalid parameter %q", ErrMalformedHash, field)
		}
		val, err := strconv.ParseUint(kv[1], 10, 32)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("%w: invalid parameter value %q: %v", ErrMalformedHash, kv[1], err)
		}
		switch kv[0] {
		case "m":
			memory = uint32(val)
		case "t":
			time = uint32(val)
		case "p":
			threads = uint8(val)
		default:
			return 0, 0, 0, fmt.Errorf("%w: unknown parameter %q", ErrMalformedHash, kv[0])
		}
	}
	if memory == 0 || time == 0 || threads == 0 {
		return 0, 0, 0, fmt.Errorf("%w: missing parameters", ErrMalformedHash)
	}
	return memory, time, threads, nil
}
