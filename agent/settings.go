package agent

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/telflo/telflo-opamp-server/internal/envconfig"
	"github.com/telflo/telflo-opamp-server/middleware"
)

// The agent listener's settings: the port collectors poll, apart from the
// management API's, with its own optional TLS.
const (
	PortVariable        = "OPAMP_AGENT_PORT"
	HostVariable        = "OPAMP_AGENT_HOST"
	TLSCertFileVariable = "OPAMP_AGENT_TLS_CERT_FILE"
	TLSKeyFileVariable  = "OPAMP_AGENT_TLS_KEY_FILE"
	// ServerURLVariable is the agent endpoint's public URL, pushed to every
	// collector in its connection settings (where it polls once handed a
	// token). Required.
	ServerURLVariable = "OPAMP_SERVER_URL"
	// HeartbeatIntervalVariable is the heartbeat pushed with the
	// connection settings, in seconds.
	HeartbeatIntervalVariable = "OPAMP_HEARTBEAT_INTERVAL_SECONDS"
	// DiagnosticsPortVariable turns on the diagnostics listener (package
	// diagnostics: the profiles and the admission view) on this port; unset,
	// there is none. It has no authentication, so it binds a loopback
	// address only (DiagnosticsHostVariable), reached with `kubectl
	// port-forward`: for `serve agent`, which holds no management secret to
	// put them behind.
	DiagnosticsPortVariable = "OPAMP_AGENT_DIAGNOSTICS_PORT"
	// DiagnosticsHostVariable is the loopback address the diagnostics
	// listener binds: 127.0.0.1 by default, or ::1.
	DiagnosticsHostVariable = "OPAMP_AGENT_DIAGNOSTICS_HOST"
	// MaxConnectionsVariable is the most connections the agent listener
	// holds open at once, idle kept-alive ones included; past it, a new
	// connection waits in the system's backlog until one closes. It bounds
	// the memory connections hold before any request is authenticated.
	MaxConnectionsVariable = "OPAMP_AGENT_MAX_CONNECTIONS"

	// DefaultPort is the agent endpoint's port.
	DefaultPort = 51234
	// The connection cap's default and bound. A collector polling directly
	// keeps one connection open, so the default holds 10,000 of them; behind
	// a load balancer, far fewer connections carry them all.
	DefaultMaxConnections = 10_000
	MaxMaxConnections     = 1_000_000
	// MaxHeaderBytes bounds an agent request's headers, before its bearer is
	// read: a supervisor's are under 2 KiB, a bearer at most 1 KiB. Larger
	// headers are answered 431. net/http's own default is 1 MiB.
	MaxHeaderBytes = 16 << 10
	// DefaultDiagnosticsHost is the diagnostics listener's address.
	DefaultDiagnosticsHost = "127.0.0.1"
	// The heartbeat's default and bounds, in seconds.
	DefaultHeartbeatSeconds = 30
	MinHeartbeatSeconds     = 10
	MaxHeartbeatSeconds     = 300
)

// Settings are the agent listener's settings as serve reads them.
type Settings struct {
	// Host is the address the agent listener binds, "" for every interface.
	Host string
	Port int
	// TLS is the agent listener's TLS configuration, or nil for plain HTTP
	// (a TLS-terminating load balancer in front, serve logs).
	TLS *tls.Config
	// ServerURL is OPAMP_SERVER_URL, as written.
	ServerURL string
	// ServerURLLoopback is whether ServerURL's host is a loopback address,
	// which a collector in a container or on another machine can't reach:
	// serve warns once.
	ServerURLLoopback bool
	// ServerURLUnexpectedPath is whether ServerURL's path isn't the agent
	// route, /opamp/api/v1: a collector handed it polls a path this server
	// answers 404, unless a proxy in front rewrites it, so serve warns once
	// rather than refusing.
	ServerURLUnexpectedPath bool
	// MaxConnections is OPAMP_AGENT_MAX_CONNECTIONS.
	MaxConnections int
	// HeartbeatInterval is OPAMP_HEARTBEAT_INTERVAL_SECONDS.
	HeartbeatInterval time.Duration
	// Limits are the agent side's limits (LoadLimits).
	Limits Limits
	// Diagnostics is the diagnostics listener, or nil for none.
	Diagnostics *Diagnostics
}

// Diagnostics is the diagnostics listener's address: a loopback one.
type Diagnostics struct {
	Host string
	Port int
}

// LoadSettings reads the agent listener's settings, failing on the
// first that is wrong: OPAMP_AGENT_PORT (default 51234; 0 asks the system
// for a free port), OPAMP_AGENT_HOST (an IP address; default every
// interface), OPAMP_AGENT_TLS_CERT_FILE and OPAMP_AGENT_TLS_KEY_FILE (both
// or neither, loaded as the management listener's, TLS 1.2 or later),
// OPAMP_AGENT_MAX_CONNECTIONS (10,000, from 1 to 1,000,000),
// OPAMP_SERVER_URL (required), OPAMP_HEARTBEAT_INTERVAL_SECONDS (30, from
// 10 to 300), the limits (LoadLimits), and the diagnostics listener
// (LoadDiagnostics). Its errors name the variable, never its value.
func LoadSettings(getenv func(string) string) (Settings, error) {
	port, err := Port(getenv)
	if err != nil {
		return Settings{}, err
	}
	host, err := Host(getenv)
	if err != nil {
		return Settings{}, err
	}
	tlsConfig, err := envconfig.LoadTLS(getenv, TLSCertFileVariable, TLSKeyFileVariable)
	if err != nil {
		return Settings{}, err
	}
	maxConnections, err := envconfig.RangedInt(getenv, MaxConnectionsVariable, DefaultMaxConnections, 1, MaxMaxConnections)
	if err != nil {
		return Settings{}, err
	}
	serverURL, loopback, err := ServerURL(getenv)
	if err != nil {
		return Settings{}, err
	}
	heartbeat, err := envconfig.RangedInt(getenv, HeartbeatIntervalVariable, DefaultHeartbeatSeconds, MinHeartbeatSeconds, MaxHeartbeatSeconds)
	if err != nil {
		return Settings{}, err
	}
	limits, err := LoadLimits(getenv)
	if err != nil {
		return Settings{}, err
	}
	diagnostics, err := LoadDiagnostics(getenv)
	if err != nil {
		return Settings{}, err
	}
	return Settings{
		Host: host, Port: port, TLS: tlsConfig, MaxConnections: maxConnections,
		ServerURL: serverURL, ServerURLLoopback: loopback, ServerURLUnexpectedPath: unexpectedAgentPath(serverURL),
		HeartbeatInterval: time.Duration(heartbeat) * time.Second,
		Limits:            limits,
		Diagnostics:       diagnostics,
	}, nil
}

// Port reads OPAMP_AGENT_PORT, or its default. `healthcheck agent` reads
// it too.
func Port(getenv func(string) string) (int, error) {
	return envconfig.Port(getenv, PortVariable, DefaultPort)
}

// Host reads OPAMP_AGENT_HOST: an IP address, or "" for every interface.
// `healthcheck agent` reads it too.
func Host(getenv func(string) string) (string, error) {
	return envconfig.Host(getenv, HostVariable)
}

// TLSFiles reads the agent listener's two TLS files: both or neither.
// `healthcheck agent` reads them to know which scheme to ask with.
func TLSFiles(getenv func(string) string) (certFile, keyFile string, enabled bool, err error) {
	return envconfig.TLSFiles(getenv, TLSCertFileVariable, TLSKeyFileVariable)
}

// LoadDiagnostics reads the diagnostics listener: nil while
// OPAMP_AGENT_DIAGNOSTICS_PORT is unset (OPAMP_AGENT_DIAGNOSTICS_HOST alone
// is refused); otherwise its port (0 asks the system for a free one) and a
// loopback address, 127.0.0.1 by default. Any other address is refused:
// the diagnostics have no authentication.
func LoadDiagnostics(getenv func(string) string) (*Diagnostics, error) {
	if getenv(DiagnosticsPortVariable) == "" {
		if getenv(DiagnosticsHostVariable) != "" {
			return nil, fmt.Errorf("%s is set without %s", DiagnosticsHostVariable, DiagnosticsPortVariable)
		}
		return nil, nil
	}
	port, err := envconfig.Port(getenv, DiagnosticsPortVariable, 0)
	if err != nil {
		return nil, err
	}
	host, err := envconfig.Host(getenv, DiagnosticsHostVariable)
	if err != nil {
		return nil, err
	}
	if host == "" {
		host = DefaultDiagnosticsHost
	}
	if !net.ParseIP(host).IsLoopback() {
		return nil, fmt.Errorf("%s must be a loopback address, such as 127.0.0.1 or ::1: the diagnostics have no authentication",
			DiagnosticsHostVariable)
	}
	return &Diagnostics{Host: host, Port: port}, nil
}

// ServerURL reads OPAMP_SERVER_URL: an http or https URL naming a host,
// with a port from 1 to 65535 if it names one, without a user name or
// password, a query or a fragment, and without spaces at its ends. It
// answers the URL as written, and whether its host is a loopback address
// (localhost, a name under .localhost, 127.0.0.0/8 or ::1). Its errors name
// the variable, never the value: a URL could carry a password.
func ServerURL(getenv func(string) string) (string, bool, error) {
	raw := getenv(ServerURLVariable)
	if raw == "" {
		return "", false, fmt.Errorf("%s is not set: the agent endpoint's public URL, which collectors are handed to poll", ServerURLVariable)
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil || strings.TrimSpace(raw) != raw || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "":
		return "", false, fmt.Errorf("%s must be an http or https URL", ServerURLVariable)
	case u.User != nil:
		return "", false, fmt.Errorf("%s must not carry a user name or password", ServerURLVariable)
	case u.Hostname() == "":
		return "", false, fmt.Errorf("%s must name a host", ServerURLVariable)
	case !validURLPort(u):
		return "", false, fmt.Errorf("%s must name a port from 1 to 65535, or none", ServerURLVariable)
	case u.RawQuery != "" || u.ForceQuery:
		return "", false, fmt.Errorf("%s must not carry a query", ServerURLVariable)
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return "", false, fmt.Errorf("%s must not carry a fragment", ServerURLVariable)
	}
	return raw, loopbackHost(u.Hostname()), nil
}

// validURLPort reports whether u names no port, or a port from 1 to 65535
// written without a leading zero (url.Parse takes any digits, an empty port
// after the colon included).
func validURLPort(u *url.URL) bool {
	port := u.Port()
	if port == "" {
		return !strings.HasSuffix(u.Host, ":")
	}
	if port[0] == '0' {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

// unexpectedAgentPath reports whether the server URL's path isn't the agent
// route (a URL ServerURL accepted).
func unexpectedAgentPath(raw string) bool {
	u, err := url.Parse(raw)
	return err != nil || u.EscapedPath() != middleware.AgentRoute
}

// loopbackHost reports whether host is a loopback name or address.
func loopbackHost(host string) bool {
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Unmap().IsLoopback()
}
