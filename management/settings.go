package management

import (
	"crypto/tls"
	"strings"

	"github.com/telflo/telflo-opamp-server/internal/envconfig"
)

// The management API's settings. The process's own (the database URL, the
// encryption keys, the shutdown drain) are package serve's; the agent
// endpoint's are package agent's.
const (
	PortVariable            = "OPAMP_MANAGEMENT_PORT"
	SecretVariable          = "OPAMP_MANAGEMENT_SECRET"
	PreviousSecretsVariable = "OPAMP_MANAGEMENT_SECRETS_PREVIOUS"
	HostVariable            = "OPAMP_MANAGEMENT_HOST"
	TLSCertFileVariable     = "OPAMP_MANAGEMENT_TLS_CERT_FILE"
	TLSKeyFileVariable      = "OPAMP_MANAGEMENT_TLS_KEY_FILE"

	// DefaultPort is the management API's port. The agent endpoint's,
	// 51234, is agent.DefaultPort.
	DefaultPort = 51235

	// MinSecretBytes is the shortest secret `serve` accepts.
	MinSecretBytes = 32
)

// Settings are the management API's settings as `serve` reads them. The
// secrets are held only as Secrets, which keeps their digests, never the
// text.
type Settings struct {
	// Host is the address the management listener binds, "" for every
	// interface.
	Host string
	Port int
	// TLS is the management listener's TLS configuration, or nil for plain
	// HTTP.
	TLS     *tls.Config
	Secrets *Secrets
}

// LoadSettings reads the management API's settings, failing on the first
// one that is missing or wrong: OPAMP_MANAGEMENT_PORT, OPAMP_MANAGEMENT_HOST,
// the two TLS files, OPAMP_MANAGEMENT_SECRET (required) and
// OPAMP_MANAGEMENT_SECRETS_PREVIOUS. Its errors name the variable, never its
// value.
func LoadSettings(getenv func(string) string) (Settings, error) {
	port, err := Port(getenv)
	if err != nil {
		return Settings{}, err
	}
	host, err := Host(getenv)
	if err != nil {
		return Settings{}, err
	}
	tlsConfig, err := LoadTLS(getenv)
	if err != nil {
		return Settings{}, err
	}
	secrets, err := NewSecrets(getenv(SecretVariable), splitPrevious(getenv(PreviousSecretsVariable)))
	if err != nil {
		return Settings{}, err
	}
	return Settings{Host: host, Port: port, TLS: tlsConfig, Secrets: secrets}, nil
}

// Port reads OPAMP_MANAGEMENT_PORT, or its default. 0 asks the system for a
// free port (tests use it; `serve` logs the port it got). `healthcheck` reads
// it too.
func Port(getenv func(string) string) (int, error) {
	return envconfig.Port(getenv, PortVariable, DefaultPort)
}

// Host reads OPAMP_MANAGEMENT_HOST: the IP address the management listener
// binds, such as 127.0.0.1 to keep the port off the network, or "" (unset)
// for every interface, which a container needs. Only an IP address: a name
// could resolve to another machine, and `healthcheck`, which asks this
// address, must only ever reach its own host. `healthcheck` reads it too.
func Host(getenv func(string) string) (string, error) {
	return envconfig.Host(getenv, HostVariable)
}

// TLSFiles reads OPAMP_MANAGEMENT_TLS_CERT_FILE and
// OPAMP_MANAGEMENT_TLS_KEY_FILE: both set (TLS on) or neither (plain HTTP).
// One alone is refused, naming both. `healthcheck` reads them to know which
// scheme to ask with; `serve` loads them (LoadTLS).
func TLSFiles(getenv func(string) string) (certFile, keyFile string, enabled bool, err error) {
	return envconfig.TLSFiles(getenv, TLSCertFileVariable, TLSKeyFileVariable)
}

// MinTLSVersion is the oldest TLS either listener speaks.
const MinTLSVersion = envconfig.MinTLSVersion

// LoadTLS is the management listener's TLS configuration: nil when neither
// TLS variable is set, else the certificate and key the two files hold
// (envconfig.LoadTLS).
func LoadTLS(getenv func(string) string) (*tls.Config, error) {
	return envconfig.LoadTLS(getenv, TLSCertFileVariable, TLSKeyFileVariable)
}

// splitPrevious splits OPAMP_MANAGEMENT_SECRETS_PREVIOUS on commas. Empty
// entries (an unset variable, a trailing comma, ",,") are dropped here, so
// they are ignored rather than refused as short secrets: a rotation that
// leaves "old," behind must not stop the server. Every other entry, however
// short, goes on to NewSecrets, which refuses it. A secret can't contain a
// comma once it is moved to this list.
func splitPrevious(raw string) []string {
	var entries []string
	for _, entry := range strings.Split(raw, ",") {
		if strings.TrimSpace(entry) != "" {
			entries = append(entries, entry)
		}
	}
	return entries
}
