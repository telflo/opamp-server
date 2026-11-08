// Package envconfig reads settings from the environment: a listener's
// port, host and TLS files, and bounded whole numbers. It knows no
// setting's meaning: each caller names its own variables and defaults.
//
// Every error names the variable and never its value, which could be a
// secret, and never a file's content.
package envconfig

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"strconv"
)

// Getenv reads one environment variable, "" when unset: os.Getenv, or a
// test's map.
type Getenv func(string) string

// MinTLSVersion is the oldest TLS a listener speaks.
const MinTLSVersion = tls.VersionTLS12

// Port reads variable's port number from 0 to 65535, or fallback when it is
// unset. 0 asks the system for a free port (tests use it).
func Port(getenv Getenv, variable string, fallback int) (int, error) {
	raw := getenv(variable)
	if raw == "" {
		return fallback, nil
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < 0 || port > 65535 {
		return 0, fmt.Errorf("%s must be a port number from 0 to 65535", variable)
	}
	return port, nil
}

// Host reads variable's IP address, in its canonical form, or "" (unset)
// for every interface. Only an IP address: a name could resolve to another
// machine.
func Host(getenv Getenv, variable string) (string, error) {
	raw := getenv(variable)
	if raw == "" {
		return "", nil
	}
	ip := net.ParseIP(raw)
	if ip == nil {
		return "", fmt.Errorf("%s must be an IP address, such as 127.0.0.1 or ::1", variable)
	}
	return ip.String(), nil
}

// TLSFiles reads a listener's two TLS file variables: both set (TLS on) or
// neither (plain HTTP). One alone is refused, naming both.
func TLSFiles(getenv Getenv, certVariable, keyVariable string) (certFile, keyFile string, enabled bool, err error) {
	certFile, keyFile = getenv(certVariable), getenv(keyVariable)
	switch {
	case certFile == "" && keyFile == "":
		return "", "", false, nil
	case certFile == "" || keyFile == "":
		set := certVariable
		if certFile == "" {
			set = keyVariable
		}
		return "", "", false, fmt.Errorf("%s and %s must be set together, or neither (only %s is set)",
			certVariable, keyVariable, set)
	}
	return certFile, keyFile, true, nil
}

// LoadTLS is a listener's TLS configuration: nil when neither TLS variable
// is set, else the certificate and key the two files hold, served with
// MinTLSVersion or later. A file that can't be read, or a pair that isn't a
// valid certificate and matching key, is refused. The files are read once:
// a renewed certificate takes a restart.
func LoadTLS(getenv Getenv, certVariable, keyVariable string) (*tls.Config, error) {
	certFile, keyFile, enabled, err := TLSFiles(getenv, certVariable, keyVariable)
	if err != nil || !enabled {
		return nil, err
	}
	certPEM, err := readFile(certVariable, certFile)
	if err != nil {
		return nil, err
	}
	keyPEM, err := readFile(keyVariable, keyFile)
	if err != nil {
		return nil, err
	}
	// X509KeyPair's errors describe the PEM's shape, never its bytes; they
	// aren't passed on all the same.
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("%s and %s aren't a PEM certificate and its matching private key",
			certVariable, keyVariable)
	}
	return &tls.Config{
		MinVersion:   MinTLSVersion,
		Certificates: []tls.Certificate{certificate},
	}, nil
}

// RangedInt is variable's whole number from low to high (decimal digits
// only: no sign, space or leading zero, "0" itself the one number starting
// with one), or fallback when it is unset.
func RangedInt(getenv Getenv, variable string, fallback, low, high int) (int, error) {
	raw := getenv(variable)
	if raw == "" {
		return fallback, nil
	}
	problem := fmt.Errorf("%s must be a whole number from %d to %d", variable, low, high)
	if raw[0] == '0' && raw != "0" {
		return 0, problem
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, problem
		}
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < low || value > high {
		return 0, problem
	}
	return value, nil
}

// readFile reads the file a variable names.
func readFile(variable, path string) ([]byte, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, FileError(variable, err)
	}
	return content, nil
}

// FileError is why the file a variable names can't be read (it doesn't
// exist, this process may not read it, or another failure), naming the
// variable and never the path's content.
func FileError(variable string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%s names a file that doesn't exist", variable)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("%s names a file this process may not read", variable)
	}
	return fmt.Errorf("%s names a file that can't be read", variable)
}
