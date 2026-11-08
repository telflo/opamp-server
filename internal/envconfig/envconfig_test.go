package envconfig

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/telflo/opamp-server/internal/testcert"
)

// envOf is a Getenv over values.
func envOf(values map[string]string) Getenv {
	return func(name string) string { return values[name] }
}

// A port is its fallback when unset, any number from 0 to 65535, and
// refused otherwise, naming the variable only.
func TestPortIsItsFallbackOrFrom0To65535(t *testing.T) {
	if port, err := Port(envOf(nil), "P", 51234); err != nil || port != 51234 {
		t.Fatalf("unset: %d, %v", port, err)
	}
	for _, raw := range []string{"0", "1", "65535"} {
		if _, err := Port(envOf(map[string]string{"P": raw}), "P", 1); err != nil {
			t.Errorf("%q refused: %v", raw, err)
		}
	}
	for _, raw := range []string{"-1", "65536", "x", " 80"} {
		_, err := Port(envOf(map[string]string{"P": raw}), "P", 1)
		if err == nil || err.Error() != "P must be a port number from 0 to 65535" {
			t.Errorf("%q: %v", raw, err)
		}
	}
}

// A host is an IP address in its canonical form, or "" for every
// interface; a name is refused.
func TestHostIsACanonicalIPAddressOrEveryInterface(t *testing.T) {
	if host, err := Host(envOf(nil), "H"); err != nil || host != "" {
		t.Fatalf("unset: %q, %v", host, err)
	}
	if host, err := Host(envOf(map[string]string{"H": "0:0:0:0:0:0:0:1"}), "H"); err != nil || host != "::1" {
		t.Fatalf("::1 long form: %q, %v", host, err)
	}
	if _, err := Host(envOf(map[string]string{"H": "localhost"}), "H"); err == nil ||
		!strings.HasPrefix(err.Error(), "H must be an IP address") {
		t.Fatalf("a name: %v", err)
	}
}

// The TLS files are both set or neither; one alone is refused, naming the
// one set.
func TestTLSFilesAreBothOrNeither(t *testing.T) {
	if _, _, enabled, err := TLSFiles(envOf(nil), "C", "K"); err != nil || enabled {
		t.Fatalf("neither: %v %v", enabled, err)
	}
	if c, k, enabled, err := TLSFiles(envOf(map[string]string{"C": "c", "K": "k"}), "C", "K"); err != nil || !enabled || c != "c" || k != "k" {
		t.Fatalf("both: %q %q %v %v", c, k, enabled, err)
	}
	_, _, _, err := TLSFiles(envOf(map[string]string{"K": "k"}), "C", "K")
	if err == nil || err.Error() != "C and K must be set together, or neither (only K is set)" {
		t.Fatalf("one: %v", err)
	}
}

// LoadTLS serves the pair with TLS 1.2 or later, and refuses a missing
// file or a pair that doesn't match, without quoting either file.
func TestLoadTLSLoadsAPairAndRefusesWhatItCannotUse(t *testing.T) {
	files := testcert.New(t)
	config, err := LoadTLS(envOf(map[string]string{"C": files.CertFile, "K": files.KeyFile}), "C", "K")
	if err != nil || config.MinVersion != tls.VersionTLS12 || len(config.Certificates) != 1 {
		t.Fatalf("a valid pair: %+v, %v", config, err)
	}
	if config, err := LoadTLS(envOf(nil), "C", "K"); err != nil || config != nil {
		t.Fatalf("neither: %v, %v", config, err)
	}

	missing := filepath.Join(t.TempDir(), "missing.pem")
	_, err = LoadTLS(envOf(map[string]string{"C": missing, "K": files.KeyFile}), "C", "K")
	if err == nil || err.Error() != "C names a file that doesn't exist" {
		t.Fatalf("a missing file: %v", err)
	}

	other := testcert.New(t)
	_, err = LoadTLS(envOf(map[string]string{"C": files.CertFile, "K": other.KeyFile}), "C", "K")
	if err == nil || err.Error() != "C and K aren't a PEM certificate and its matching private key" {
		t.Fatalf("a mismatched pair: %v", err)
	}
	key, _ := os.ReadFile(files.KeyFile)
	if strings.Contains(err.Error(), string(key)) {
		t.Fatal("the error quotes the key")
	}
}

// A ranged number is decimal digits only, within its bounds, "0" the one
// number with a leading zero; its fallback when unset.
func TestRangedIntIsDecimalDigitsWithinItsBounds(t *testing.T) {
	if value, err := RangedInt(envOf(nil), "N", 5, 0, 30); err != nil || value != 5 {
		t.Fatalf("unset: %d, %v", value, err)
	}
	for raw, want := range map[string]int{"0": 0, "30": 30, "7": 7} {
		if value, err := RangedInt(envOf(map[string]string{"N": raw}), "N", 5, 0, 30); err != nil || value != want {
			t.Errorf("%q: %d, %v", raw, value, err)
		}
	}
	for _, raw := range []string{"31", "-1", "+3", "07", " 3", "3 ", "1e1", "3.0"} {
		_, err := RangedInt(envOf(map[string]string{"N": raw}), "N", 5, 0, 30)
		if err == nil || err.Error() != "N must be a whole number from 0 to 30" {
			t.Errorf("%q: %v", raw, err)
		}
	}
}

// A file error says why, naming the variable only.
func TestFileErrorSaysWhyNamingTheVariable(t *testing.T) {
	_, err := os.ReadFile(filepath.Join(t.TempDir(), "absent"))
	if got := FileError("F", err).Error(); got != "F names a file that doesn't exist" {
		t.Fatalf("absent: %q", got)
	}
	if got := FileError("F", os.ErrPermission).Error(); got != "F names a file this process may not read" {
		t.Fatalf("permission: %q", got)
	}
	if got := FileError("F", os.ErrClosed).Error(); got != "F names a file that can't be read" {
		t.Fatalf("other: %q", got)
	}
}
