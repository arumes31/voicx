// Package updatemanifest authenticates the signed release manifest consumed by
// the desktop updater and produced by the release pipeline.
package updatemanifest

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strings"
)

const (
	// MaxSize bounds in-memory processing of a release manifest.
	MaxSize = 1 << 20

	// VersionPrefix identifies the signed release version in a manifest.
	VersionPrefix = "# voicx-version: "
)

// Verify authenticates a detached, base64-encoded Ed25519 signature with one
// of encodedPublicKeys and binds the manifest to expectedVersion.
func Verify(manifest, signature []byte, encodedPublicKeys, expectedVersion string) error {
	if len(manifest) == 0 || len(manifest) > MaxSize {
		return fmt.Errorf("manifest size must be between 1 and %d bytes", MaxSize)
	}
	if strings.TrimSpace(expectedVersion) == "" {
		return fmt.Errorf("expected release version is required")
	}

	keys, err := ParsePublicKeys(encodedPublicKeys)
	if err != nil {
		return err
	}
	sig, err := DecodeSignature(signature)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if ed25519.Verify(key, manifest, sig) {
			return VerifyVersion(manifest, expectedVersion)
		}
	}
	return fmt.Errorf("update manifest signature is not trusted")
}

// ParsePublicKeys decodes the comma-separated public keys configured for
// update verification. Keys are base64-encoded Ed25519 public keys.
func ParsePublicKeys(encodedPublicKeys string) ([]ed25519.PublicKey, error) {
	if strings.TrimSpace(encodedPublicKeys) == "" {
		return nil, fmt.Errorf("no trusted update signing key is configured")
	}

	encodedKeys := strings.Split(encodedPublicKeys, ",")
	keys := make([]ed25519.PublicKey, 0, len(encodedKeys))
	for _, encoded := range encodedKeys {
		decoded, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(encoded))
		if err != nil {
			return nil, fmt.Errorf("decode trusted update signing key: %w", err)
		}
		if len(decoded) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("invalid trusted update signing key length")
		}
		keys = append(keys, ed25519.PublicKey(decoded))
	}
	return keys, nil
}

// DecodeSignature decodes one base64-encoded Ed25519 signature.
func DecodeSignature(signature []byte) ([]byte, error) {
	sig, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(string(signature)))
	if err != nil {
		return nil, fmt.Errorf("decode update signature: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("invalid update signature length")
	}
	return sig, nil
}

// VerifyVersion binds a manifest to its expected release version. It requires
// exactly one non-empty version header as the first line so producers and
// consumers share the same release-version contract.
func VerifyVersion(manifest []byte, expectedVersion string) error {
	if strings.TrimSpace(expectedVersion) == "" {
		return fmt.Errorf("expected release version is required")
	}

	lines := strings.Split(string(manifest), "\n")
	firstLine := strings.TrimSuffix(lines[0], "\r")
	if !strings.HasPrefix(firstLine, VersionPrefix) {
		for _, line := range lines[1:] {
			if strings.HasPrefix(strings.TrimSuffix(line, "\r"), VersionPrefix) {
				return fmt.Errorf("signed manifest release version must be the first line")
			}
		}
		return fmt.Errorf("signed manifest has no release version")
	}
	got := strings.TrimSpace(strings.TrimPrefix(firstLine, VersionPrefix))
	if got == "" {
		return fmt.Errorf("signed manifest release version is empty")
	}
	if got != expectedVersion {
		return fmt.Errorf("signed manifest version %q does not match release %q", got, expectedVersion)
	}
	for _, line := range lines[1:] {
		if strings.HasPrefix(strings.TrimSuffix(line, "\r"), VersionPrefix) {
			return fmt.Errorf("signed manifest contains multiple release versions")
		}
	}
	return nil
}
