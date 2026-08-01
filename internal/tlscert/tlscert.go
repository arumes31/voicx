// Package tlscert manages the self-signed TLS certificate of the voicx
// control channel. On first start a certificate is generated (ECDSA P-256,
// valid 10 years, SANs for localhost and the server name) and persisted; on
// later starts it is loaded back, so the SHA-256 fingerprint clients pin via
// TOFU stays stable across restarts.
package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// certLifetime is how long a generated self-signed certificate is valid.
const certLifetime = 10 * 365 * 24 * time.Hour // ~10 years

// Ensure loads the certificate from certFile/keyFile, or generates and
// persists a new self-signed one when the files do not exist. Empty
// certFile/keyFile default to <dir>/cert.pem and <dir>/key.pem. hosts are
// added as DNS SANs (localhost and 127.0.0.1/::1 are always included). It
// returns the certificate and its SHA-256 fingerprint (colon-separated hex).
func Ensure(dir, certFile, keyFile string, hosts []string) (tls.Certificate, string, error) {
	if certFile == "" {
		certFile = filepath.Join(dir, "cert.pem")
	}
	if keyFile == "" {
		keyFile = filepath.Join(dir, "key.pem")
	}

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err == nil {
		fp, err := Fingerprint(&cert)
		return cert, fp, err
	}
	if !errors.Is(err, os.ErrNotExist) {
		return tls.Certificate{}, "", fmt.Errorf("loading TLS certificate: %w", err)
	}

	cert, err = generate(hosts)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("generating TLS certificate: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(certFile), 0o750); err != nil {
		return tls.Certificate{}, "", fmt.Errorf("creating TLS dir: %w", err)
	}
	if err := writePEM(cert, certFile, keyFile); err != nil {
		return tls.Certificate{}, "", err
	}
	fp, err := Fingerprint(&cert)
	return cert, fp, err
}

// Fingerprint returns the SHA-256 fingerprint of the certificate's leaf as
// colon-separated lowercase hex (the value clients pin via TOFU).
func Fingerprint(cert *tls.Certificate) (string, error) {
	if len(cert.Certificate) == 0 {
		return "", errors.New("tlscert: certificate has no chain")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return "", fmt.Errorf("parsing leaf certificate: %w", err)
	}
	return FingerprintDER(leaf.Raw), nil
}

// FingerprintDER returns the SHA-256 fingerprint of a DER-encoded
// certificate as colon-separated lowercase hex.
func FingerprintDER(der []byte) string {
	sum := sha256.Sum256(der)
	out := make([]string, len(sum))
	for i, b := range sum {
		out[i] = hex.EncodeToString([]byte{b})
	}
	return joinColon(out)
}

func joinColon(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	n := len(parts)*3 - 1
	buf := make([]byte, 0, n)
	for i, p := range parts {
		if i > 0 {
			buf = append(buf, ':')
		}
		buf = append(buf, p...)
	}
	return string(buf)
}

// generate creates a self-signed ECDSA P-256 certificate valid for
// certLifetime with SANs for localhost (+ the given hosts) and the loopback
// addresses.
func generate(hosts []string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}

	dnsNames := []string{"localhost"}
	seen := map[string]bool{"localhost": true}
	for _, h := range hosts {
		if h != "" && !seen[h] {
			seen[h] = true
			dnsNames = append(dnsNames, h)
		}
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "voicx", Organization: []string{"voicx (self-signed)"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(certLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}, nil
}

// writePEM persists the certificate chain and private key in PEM form.
func writePEM(cert tls.Certificate, certFile, keyFile string) error {
	var certPEM []byte
	for _, der := range cert.Certificate {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	if err := os.WriteFile(certFile, certPEM, 0o644); err != nil {
		return fmt.Errorf("writing certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(cert.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		return fmt.Errorf("marshaling private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		return fmt.Errorf("writing private key: %w", err)
	}
	return nil
}
