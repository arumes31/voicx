// Command signrelease creates the detached Ed25519 signature consumed by the
// VoicX client updater. The private key is read only from the environment so it
// does not appear in process arguments or repository files.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"
)

const (
	signingKeyEnv  = "VOICX_UPDATE_SIGNING_KEY"
	maxManifestLen = 1 << 20
)

func main() {
	input := flag.String("in", "checksums.txt", "signed manifest path")
	output := flag.String("out", "checksums.txt.sig", "detached signature path")
	generateKey := flag.String("generate-key", "", "write a new private signing key to this exclusive file")
	flag.Parse()

	if *generateKey != "" {
		publicKey, err := generateKeyFile(*generateKey)
		if err == nil {
			_, err = fmt.Fprintf(os.Stdout, "VOICX_UPDATE_PUBLIC_KEYS=%s\n", publicKey)
		}
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "generate update key: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := signFile(*input, *output, os.Getenv(signingKeyEnv)); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "sign release: %v\n", err)
		os.Exit(1)
	}
}

func generateKeyFile(outputPath string) (string, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		return "", fmt.Errorf("generate Ed25519 key: %w", err)
	}
	// #nosec G304 -- this local operator CLI intentionally accepts an explicit key destination.
	file, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create private key file: %w", err)
	}
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(outputPath)
		}
	}()
	encodedPrivateKey := base64.StdEncoding.EncodeToString(privateKey.Seed()) + "\n"
	if _, err := file.WriteString(encodedPrivateKey); err != nil {
		return "", fmt.Errorf("write private key file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("sync private key file: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close private key file: %w", err)
	}
	keep = true
	return base64.StdEncoding.EncodeToString(publicKey), nil
}

func signFile(inputPath, outputPath, encodedKey string) error {
	privateKey, err := decodePrivateKey(encodedKey)
	if err != nil {
		return err
	}
	// #nosec G304 -- this local operator/CI CLI intentionally accepts an explicit manifest path.
	manifest, err := os.ReadFile(inputPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	if len(manifest) == 0 || len(manifest) > maxManifestLen {
		return fmt.Errorf("manifest size must be between 1 and %d bytes", maxManifestLen)
	}
	signature := ed25519.Sign(privateKey, manifest)
	encodedSignature := base64.StdEncoding.EncodeToString(signature) + "\n"
	// #nosec G703 -- this local operator/CI CLI intentionally accepts an explicit signature path.
	if err := os.WriteFile(outputPath, []byte(encodedSignature), 0o600); err != nil {
		return fmt.Errorf("write signature: %w", err)
	}
	return nil
}

func decodePrivateKey(encoded string) (ed25519.PrivateKey, error) {
	if strings.TrimSpace(encoded) == "" {
		return nil, fmt.Errorf("%s is not configured", signingKeyEnv)
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", signingKeyEnv, err)
	}
	switch len(decoded) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(decoded), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(decoded), nil
	default:
		return nil, fmt.Errorf("%s must decode to a %d-byte seed or %d-byte private key",
			signingKeyEnv, ed25519.SeedSize, ed25519.PrivateKeySize)
	}
}
