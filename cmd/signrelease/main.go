// Command signrelease creates the detached Ed25519 signature consumed by the
// VoicX client updater. The private key is read only from the environment so it
// does not appear in process arguments or repository files.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"voicx/internal/updatemanifest"
)

const (
	signingKeyEnv = "VOICX_UPDATE_SIGNING_KEY"
	publicKeysEnv = "VOICX_UPDATE_PUBLIC_KEYS"
)

type signatureFile interface {
	io.Writer
	Sync() error
	Close() error
}

type signatureFileOpener func(string) (signatureFile, error)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "sign release: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("signrelease", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	input := flags.String("in", "checksums.txt", "signed manifest path")
	output := flags.String("out", "checksums.txt.sig", "detached signature path")
	generateKey := flags.String("generate-key", "", "write a new private signing key to this exclusive file")
	verify := flags.Bool("verify", false, "verify an existing detached signature")
	publicKeys := flags.String("public-keys", os.Getenv(publicKeysEnv), "comma-separated trusted update public keys")
	expectedVersion := flags.String("version", "", "expected signed manifest release version")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}

	if *generateKey != "" {
		publicKey, err := generateKeyFile(*generateKey)
		if err != nil {
			return fmt.Errorf("generate update key: %w", err)
		}
		_, err = fmt.Fprintf(stdout, "VOICX_UPDATE_PUBLIC_KEYS=%s\n", publicKey)
		return err
	}
	if *verify {
		return verifyFile(*input, *output, *publicKeys, *expectedVersion)
	}
	return signFile(*input, *output, os.Getenv(signingKeyEnv), *expectedVersion)
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

func signFile(inputPath, outputPath, encodedKey, expectedVersion string) error {
	privateKey, err := decodePrivateKey(encodedKey)
	if err != nil {
		return err
	}
	// #nosec G304 -- this local operator/CI CLI intentionally accepts an explicit manifest path.
	manifest, err := os.ReadFile(inputPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	if len(manifest) == 0 || len(manifest) > updatemanifest.MaxSize {
		return fmt.Errorf("manifest size must be between 1 and %d bytes", updatemanifest.MaxSize)
	}
	if err := updatemanifest.VerifyVersion(manifest, expectedVersion); err != nil {
		return err
	}
	signature := ed25519.Sign(privateKey, manifest)
	encodedSignature := base64.StdEncoding.EncodeToString(signature) + "\n"
	return writeSignatureFile(outputPath, []byte(encodedSignature))
}

func writeSignatureFile(outputPath string, signature []byte) (err error) {
	return writeSignatureFileWithOpener(outputPath, signature, openSignatureFile)
}

func openSignatureFile(outputPath string) (signatureFile, error) {
	// #nosec G304 -- this local operator/CI CLI intentionally accepts an explicit signature destination.
	return os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

func writeSignatureFileWithOpener(outputPath string, signature []byte, open signatureFileOpener) (err error) {
	file, err := open(outputPath)
	if err != nil {
		return fmt.Errorf("create signature file: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			closeErr = fmt.Errorf("close signature file: %w", closeErr)
			if err == nil {
				err = closeErr
			} else {
				err = errors.Join(err, closeErr)
			}
		}
		if err == nil {
			return
		}
		if removeErr := os.Remove(outputPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove partial signature file: %w", removeErr))
		}
	}()
	if _, err = file.Write(signature); err != nil {
		return fmt.Errorf("write signature file: %w", err)
	}
	if err = file.Sync(); err != nil {
		return fmt.Errorf("sync signature file: %w", err)
	}
	return nil
}

func verifyFile(inputPath, signaturePath, encodedPublicKeys, expectedVersion string) error {
	// #nosec G304 -- this local operator/CI CLI intentionally accepts explicit release artifact paths.
	manifest, err := os.ReadFile(inputPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	// #nosec G304 -- this local operator/CI CLI intentionally accepts explicit release artifact paths.
	signature, err := os.ReadFile(signaturePath)
	if err != nil {
		return fmt.Errorf("read signature: %w", err)
	}
	if err := updatemanifest.Verify(manifest, signature, encodedPublicKeys, expectedVersion); err != nil {
		return err
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
