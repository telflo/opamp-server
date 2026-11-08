package management

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/telflo/opamp-server/internal/testcert"
)

func envOf(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// testServerURL is the OPAMP_SERVER_URL the tests' settings carry.
const testServerURL = "https://opamp.test.invalid/opamp/api/v1"

func TestPortDefaultsTo51235(t *testing.T) {
	cases := map[string]int{"": 51235, "40000": 40000, "0": 0}
	for raw, want := range cases {
		port, err := Port(envOf(map[string]string{PortVariable: raw}))
		if err != nil || port != want {
			t.Errorf("Port(%q) = %d, %v; want %d", raw, port, err, want)
		}
	}
	for _, raw := range []string{"51235x", "-1", "65536"} {
		if _, err := Port(envOf(map[string]string{PortVariable: raw})); err == nil {
			t.Errorf("Port(%q) accepted it", raw)
		}
	}
}

// Empty entries (a trailing comma, ",,", only spaces) are ignored, not
// refused as short secrets; every other entry is kept as written.
func TestSplitPreviousIgnoresEmptyEntries(t *testing.T) {
	cases := map[string][]string{
		"":          nil,
		",":         nil,
		" , ,":      nil,
		"a,":        {"a"},
		",a,,b, ,":  {"a", "b"},
		"a , b":     {"a ", " b"},
		"only-one":  {"only-one"},
		",,only-x,": {"only-x"},
	}
	for raw, want := range cases {
		if got := splitPrevious(raw); !reflect.DeepEqual(got, want) {
			t.Errorf("splitPrevious(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestHostIsAnIPAddressOrEveryInterface(t *testing.T) {
	for raw, want := range map[string]string{"": "", "127.0.0.1": "127.0.0.1", "0.0.0.0": "0.0.0.0", "::1": "::1", "::": "::", "10.1.2.3": "10.1.2.3"} {
		host, err := Host(envOf(map[string]string{HostVariable: raw}))
		if err != nil || host != want {
			t.Errorf("Host(%q) = %q, %v; want %q", raw, host, err, want)
		}
	}
	// A name could resolve to another machine, which healthcheck must never
	// ask; a port belongs in OPAMP_MANAGEMENT_PORT.
	for _, raw := range []string{"localhost", "opamp.internal", "127.0.0.1:51235", "[::1]", "10.1.2.300", " 127.0.0.1"} {
		_, err := Host(envOf(map[string]string{HostVariable: raw}))
		if err == nil || err.Error() != "OPAMP_MANAGEMENT_HOST must be an IP address, such as 127.0.0.1 or ::1" {
			t.Errorf("Host(%q) error = %v", raw, err)
		}
	}
}

// Both TLS files or neither. One alone is refused, naming both variables.
func TestTLSFilesAreBothOrNeither(t *testing.T) {
	_, _, enabled, err := TLSFiles(envOf(nil))
	if err != nil || enabled {
		t.Fatalf("TLSFiles() with neither = %v, %v; want off", enabled, err)
	}
	certFile, keyFile, enabled, err := TLSFiles(envOf(map[string]string{TLSCertFileVariable: "c.pem", TLSKeyFileVariable: "k.pem"}))
	if err != nil || !enabled || certFile != "c.pem" || keyFile != "k.pem" {
		t.Fatalf("TLSFiles() with both = %q, %q, %v, %v", certFile, keyFile, enabled, err)
	}
	for set, other := range map[string]string{TLSCertFileVariable: TLSKeyFileVariable, TLSKeyFileVariable: TLSCertFileVariable} {
		_, err := LoadTLS(envOf(map[string]string{set: "/some/file-marker-3d1"}))
		want := "OPAMP_MANAGEMENT_TLS_CERT_FILE and OPAMP_MANAGEMENT_TLS_KEY_FILE must be set together, or neither (only " + set + " is set)"
		if err == nil || err.Error() != want {
			t.Errorf("with only %s: error = %v, want %q", set, err, want)
		}
		if err != nil && (!strings.Contains(err.Error(), other) || strings.Contains(err.Error(), "file-marker-3d1")) {
			t.Errorf("with only %s: %q doesn't name %s, or quotes the path", set, err, other)
		}
	}
}

// A TLS file that can't be read, or a pair that isn't a certificate and its
// key, is refused naming the variable, never the file's content.
func TestLoadTLSRefusesAFileItCannotUseWithoutQuotingIt(t *testing.T) {
	good := testcert.New(t)
	other := testcert.New(t)
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage.pem")
	const marker = "content-marker-8b2"
	if err := os.WriteFile(garbage, []byte("-----BEGIN CERTIFICATE-----\n"+marker+"\n-----END CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.pem")
	pairMessage := "OPAMP_MANAGEMENT_TLS_CERT_FILE and OPAMP_MANAGEMENT_TLS_KEY_FILE aren't a PEM certificate and its matching private key"
	cases := []struct {
		name, cert, key, want string
	}{
		{"a missing certificate", missing, good.KeyFile, "OPAMP_MANAGEMENT_TLS_CERT_FILE names a file that doesn't exist"},
		{"a missing key", good.CertFile, missing, "OPAMP_MANAGEMENT_TLS_KEY_FILE names a file that doesn't exist"},
		{"a directory for the certificate", dir, good.KeyFile, "OPAMP_MANAGEMENT_TLS_CERT_FILE names a file that can't be read"},
		{"a certificate that isn't one", garbage, good.KeyFile, pairMessage},
		{"a key that isn't one", good.CertFile, garbage, pairMessage},
		{"the key swapped for the certificate", good.KeyFile, good.CertFile, pairMessage},
		{"another certificate's key", good.CertFile, other.KeyFile, pairMessage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			config, err := LoadTLS(envOf(map[string]string{TLSCertFileVariable: c.cert, TLSKeyFileVariable: c.key}))
			if err == nil || err.Error() != c.want || config != nil {
				t.Fatalf("LoadTLS() = %v, %v; want %q", config, err, c.want)
			}
			if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "PRIVATE KEY") || strings.Contains(err.Error(), dir) {
				t.Fatalf("the refusal quotes a file or its path: %q", err)
			}
		})
	}

	config, err := LoadTLS(envOf(map[string]string{TLSCertFileVariable: good.CertFile, TLSKeyFileVariable: good.KeyFile}))
	if err != nil || config == nil || config.MinVersion != tls.VersionTLS12 || len(config.Certificates) != 1 {
		t.Fatalf("LoadTLS() with a good pair = %+v, %v; want TLS 1.2 or later with the certificate", config, err)
	}
}

func TestLoadSettingsReadsTheHostAndTLS(t *testing.T) {
	files := testcert.New(t)
	settings, err := LoadSettings(envOf(map[string]string{
		SecretVariable:      strings.Repeat("c", MinSecretBytes),
		HostVariable:        "127.0.0.1",
		TLSCertFileVariable: files.CertFile,
		TLSKeyFileVariable:  files.KeyFile,
	}))
	if err != nil || settings.Host != "127.0.0.1" || settings.TLS == nil {
		t.Fatalf("LoadSettings() = %+v, %v", settings, err)
	}
	// Without them: every interface, plain HTTP.
	settings, err = LoadSettings(envOf(map[string]string{
		SecretVariable: strings.Repeat("c", MinSecretBytes),
	}))
	if err != nil || settings.Host != "" || settings.TLS != nil {
		t.Fatalf("LoadSettings() = %+v, %v", settings, err)
	}
}
