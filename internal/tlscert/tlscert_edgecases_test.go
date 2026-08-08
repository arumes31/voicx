package tlscert

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureGeneratesServerOnlyCertificateWithExpectedSANs(t *testing.T) {
	t.Parallel()

	cert, _, err := Ensure(t.TempDir(), "", "", []string{"voice.example.test", "voice.example.test", ""})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}

	if _, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok {
		t.Fatalf("public key type = %T, want ECDSA", leaf.PublicKey)
	}
	if leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Fatalf("key usage = %v, want DigitalSignature", leaf.KeyUsage)
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Fatalf("extended key usage = %v, want server auth only", leaf.ExtKeyUsage)
	}
	if leaf.NotAfter.Sub(leaf.NotBefore) < certLifetime {
		t.Fatalf("certificate lifetime = %v, want at least %v", leaf.NotAfter.Sub(leaf.NotBefore), certLifetime)
	}

	dnsCounts := make(map[string]int, len(leaf.DNSNames))
	for _, name := range leaf.DNSNames {
		dnsCounts[name]++
	}
	if dnsCounts["localhost"] != 1 || dnsCounts["voice.example.test"] != 1 || len(dnsCounts) != 2 {
		t.Fatalf("DNS SANs = %v, want unique localhost and voice.example.test", leaf.DNSNames)
	}
	for _, want := range []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")} {
		found := false
		for _, got := range leaf.IPAddresses {
			if got.Equal(want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("IP SANs = %v, missing %s", leaf.IPAddresses, want)
		}
	}
}

func TestEnsureRejectsCorruptExistingCertificate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	if _, _, err := Ensure(dir, certFile, keyFile, nil); err != nil {
		t.Fatalf("initial Ensure: %v", err)
	}
	const corrupt = "not a certificate"
	if err := os.WriteFile(certFile, []byte(corrupt), 0o600); err != nil {
		t.Fatalf("corrupting certificate: %v", err)
	}

	if _, _, err := Ensure(dir, certFile, keyFile, nil); err == nil || !strings.Contains(err.Error(), "loading TLS certificate") {
		t.Fatalf("Ensure(corrupt certificate) error = %v, want load error", err)
	}
	contents, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(contents) != corrupt {
		t.Fatal("Ensure replaced a corrupt existing certificate instead of failing closed")
	}
}

func TestFingerprintRejectsMissingOrMalformedLeaf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cert tls.Certificate
	}{
		{name: "missing chain"},
		{name: "malformed DER", cert: tls.Certificate{Certificate: [][]byte{[]byte("not DER")}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Fingerprint(&test.cert); err == nil {
				t.Fatal("Fingerprint unexpectedly accepted invalid certificate")
			}
		})
	}
}
