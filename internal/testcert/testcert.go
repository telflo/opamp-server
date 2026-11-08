// Package testcert makes a throwaway TLS certificate for tests: generated
// when a test asks, written to the test's own temporary directory, and never
// committed. Only tests import it.
package testcert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Files is a self-signed certificate for 127.0.0.1, ::1 and localhost, and
// its key, as PEM files, with a pool that trusts the certificate.
type Files struct {
	CertFile string
	KeyFile  string
	Pool     *x509.CertPool
}

// New generates a certificate and key in t.TempDir(), removed when the test
// ends.
func New(t testing.TB) Files {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "opamp-server test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	files := Files{
		CertFile: filepath.Join(dir, "management.crt"),
		KeyFile:  filepath.Join(dir, "management.key"),
		Pool:     x509.NewCertPool(),
	}
	files.Pool.AddCert(certificate)
	write(t, files.CertFile, "CERTIFICATE", der)
	write(t, files.KeyFile, "PRIVATE KEY", keyDER)
	return files
}

func write(t testing.TB, path, blockType string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}
