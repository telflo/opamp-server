package agent

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/telflo/telflo-opamp-server/internal/envconfig"
	"github.com/telflo/telflo-opamp-server/internal/testcert"
	"github.com/telflo/telflo-opamp-server/middleware"
)

// The agent listener's settings, as LoadSettings reads them with only
// OPAMP_SERVER_URL set, and with every one set.
func TestLoadAgentSettingsReadsTheDefaultsAndEverySetting(t *testing.T) {
	settings, err := LoadSettings(envOf(map[string]string{ServerURLVariable: testServerURL}))
	if err != nil {
		t.Fatal(err)
	}
	if DefaultPort != 51234 || settings.Port != DefaultPort || settings.Host != "" || settings.TLS != nil ||
		settings.ServerURL != testServerURL || settings.ServerURLLoopback || settings.HeartbeatInterval != 30*time.Second ||
		settings.Limits != (Limits{DefaultRequestsPerMinute: middleware.DefaultRequestsPerMinute,
			CollectorRequestsPerMinute:           middleware.DefaultCollectorRequestsPerMinute,
			MaxConcurrentRequests:                middleware.DefaultMaxConcurrentRequests,
			MaxConcurrentRequestsPerOrganization: middleware.DefaultMaxConcurrentRequestsPerOrganization}) ||
		middleware.DefaultMaxConcurrentRequestsPerOrganization != 25 || settings.ServerURLUnexpectedPath ||
		DefaultMaxConnections != 10_000 || settings.MaxConnections != DefaultMaxConnections {
		t.Fatalf("the defaults: %+v", settings)
	}

	files := testcert.New(t)
	settings, err = LoadSettings(envOf(map[string]string{
		PortVariable:                                 "40002",
		HostVariable:                                 "::1",
		TLSCertFileVariable:                          files.CertFile,
		TLSKeyFileVariable:                           files.KeyFile,
		ServerURLVariable:                            "http://host.docker.internal:51234/opamp/api/v1",
		HeartbeatIntervalVariable:                    "300",
		MaxConcurrentRequestsVariable:                "7",
		CollectorRequestsPerMinuteVariable:           "12",
		DefaultRequestsPerMinuteVariable:             "600",
		MaxConcurrentRequestsPerOrganizationVariable: "3",
		MaxConnectionsVariable:                       "250",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if settings.Port != 40002 || settings.Host != "::1" || settings.TLS == nil || settings.TLS.MinVersion != envconfig.MinTLSVersion ||
		settings.ServerURL != "http://host.docker.internal:51234/opamp/api/v1" || settings.ServerURLLoopback ||
		settings.HeartbeatInterval != 300*time.Second || settings.MaxConnections != 250 ||
		settings.Limits != (Limits{DefaultRequestsPerMinute: 600, CollectorRequestsPerMinute: 12, MaxConcurrentRequests: 7,
			MaxConcurrentRequestsPerOrganization: 3}) {
		t.Fatalf("every setting: %+v", settings)
	}

	// An organization's share defaults to 25, or to the pod's cap when that
	// is lower, and may equal the pod's cap.
	for _, c := range []struct {
		pod, perOrganization string
		want                 int
	}{{"7", "", 7}, {"40", "", 25}, {"40", "40", 40}, {"40", "1", 1}} {
		settings, err := LoadSettings(envOf(map[string]string{ServerURLVariable: testServerURL,
			MaxConcurrentRequestsVariable: c.pod, MaxConcurrentRequestsPerOrganizationVariable: c.perOrganization}))
		if err != nil || settings.Limits.MaxConcurrentRequestsPerOrganization != c.want {
			t.Errorf("pod %s, organization %q: %d, %v; want %d", c.pod, c.perOrganization,
				settings.Limits.MaxConcurrentRequestsPerOrganization, err, c.want)
		}
	}
}

// Each agent setting's refusal is one error naming the variable and never
// its value (a marker in each value is searched for).
func TestLoadAgentSettingsRefusesEachSettingNamingItsVariableOnly(t *testing.T) {
	files := testcert.New(t)
	garbage := t.TempDir() + "/garbage.pem"
	if err := os.WriteFile(garbage, []byte("not-a-certificate-marker-5e0"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		values map[string]string
		want   string
	}{
		{"a port that isn't a number", map[string]string{PortVariable: "51234-marker"},
			"OPAMP_AGENT_PORT must be a port number from 0 to 65535"},
		{"a port over 65535", map[string]string{PortVariable: "65536"},
			"OPAMP_AGENT_PORT must be a port number from 0 to 65535"},
		{"a host that is a name", map[string]string{HostVariable: "opamp-marker.internal"},
			"OPAMP_AGENT_HOST must be an IP address, such as 127.0.0.1 or ::1"},
		{"only the certificate", map[string]string{TLSCertFileVariable: files.CertFile},
			"OPAMP_AGENT_TLS_CERT_FILE and OPAMP_AGENT_TLS_KEY_FILE must be set together, or neither (only OPAMP_AGENT_TLS_CERT_FILE is set)"},
		{"only the key", map[string]string{TLSKeyFileVariable: files.KeyFile},
			"OPAMP_AGENT_TLS_CERT_FILE and OPAMP_AGENT_TLS_KEY_FILE must be set together, or neither (only OPAMP_AGENT_TLS_KEY_FILE is set)"},
		{"a certificate file that doesn't exist", map[string]string{TLSCertFileVariable: files.CertFile + ".missing-marker",
			TLSKeyFileVariable: files.KeyFile}, "OPAMP_AGENT_TLS_CERT_FILE names a file that doesn't exist"},
		{"a certificate that isn't one", map[string]string{TLSCertFileVariable: garbage, TLSKeyFileVariable: files.KeyFile},
			"OPAMP_AGENT_TLS_CERT_FILE and OPAMP_AGENT_TLS_KEY_FILE aren't a PEM certificate and its matching private key"},
		{"no server URL", map[string]string{ServerURLVariable: ""},
			"OPAMP_SERVER_URL is not set: the agent endpoint's public URL, which collectors are handed to poll"},
		{"a server URL of another scheme", map[string]string{ServerURLVariable: "ftp://opamp-marker.example/opamp/api/v1"},
			"OPAMP_SERVER_URL must be an http or https URL"},
		{"a server URL without a scheme", map[string]string{ServerURLVariable: "opamp-marker.example/opamp/api/v1"},
			"OPAMP_SERVER_URL must be an http or https URL"},
		{"an opaque server URL", map[string]string{ServerURLVariable: "http:opamp-marker.example"},
			"OPAMP_SERVER_URL must be an http or https URL"},
		{"a server URL with spaces at an end", map[string]string{ServerURLVariable: " https://opamp-marker.example/opamp/api/v1"},
			"OPAMP_SERVER_URL must be an http or https URL"},
		{"a server URL that doesn't parse", map[string]string{ServerURLVariable: "https://opamp-marker.example:port/"},
			"OPAMP_SERVER_URL must be an http or https URL"},
		{"a server URL with a password", map[string]string{ServerURLVariable: "https://someone:a-password-marker@opamp.example/opamp/api/v1"},
			"OPAMP_SERVER_URL must not carry a user name or password"},
		{"a server URL with a user name", map[string]string{ServerURLVariable: "https://user-marker@opamp.example/opamp/api/v1"},
			"OPAMP_SERVER_URL must not carry a user name or password"},
		{"a server URL without a host", map[string]string{ServerURLVariable: "https:///opamp-marker/api/v1"},
			"OPAMP_SERVER_URL must name a host"},
		{"a server URL with a query", map[string]string{ServerURLVariable: "https://opamp.example/opamp/api/v1?token=query-marker"},
			"OPAMP_SERVER_URL must not carry a query"},
		{"a server URL with an empty query", map[string]string{ServerURLVariable: "https://opamp-marker.example/opamp/api/v1?"},
			"OPAMP_SERVER_URL must not carry a query"},
		{"a server URL with a fragment", map[string]string{ServerURLVariable: "https://opamp.example/opamp/api/v1#fragment-marker"},
			"OPAMP_SERVER_URL must not carry a fragment"},
		{"a server URL with an empty fragment", map[string]string{ServerURLVariable: "https://opamp-marker.example/opamp/api/v1#"},
			"OPAMP_SERVER_URL must not carry a fragment"},
		{"a heartbeat under 10 s", map[string]string{HeartbeatIntervalVariable: "9"},
			"OPAMP_HEARTBEAT_INTERVAL_SECONDS must be a whole number from 10 to 300"},
		{"a heartbeat over 300 s", map[string]string{HeartbeatIntervalVariable: "301"},
			"OPAMP_HEARTBEAT_INTERVAL_SECONDS must be a whole number from 10 to 300"},
		{"a heartbeat with a leading zero", map[string]string{HeartbeatIntervalVariable: "030"},
			"OPAMP_HEARTBEAT_INTERVAL_SECONDS must be a whole number from 10 to 300"},
		{"a heartbeat that isn't a number", map[string]string{HeartbeatIntervalVariable: "30s-marker"},
			"OPAMP_HEARTBEAT_INTERVAL_SECONDS must be a whole number from 10 to 300"},
		{"a connection cap of 0", map[string]string{MaxConnectionsVariable: "0"},
			"OPAMP_AGENT_MAX_CONNECTIONS must be a whole number from 1 to 1000000"},
		{"a connection cap over its bound", map[string]string{MaxConnectionsVariable: "1000001"},
			"OPAMP_AGENT_MAX_CONNECTIONS must be a whole number from 1 to 1000000"},
		{"a connection cap that isn't a number", map[string]string{MaxConnectionsVariable: "lots-marker"},
			"OPAMP_AGENT_MAX_CONNECTIONS must be a whole number from 1 to 1000000"},
		{"a concurrency cap of 0", map[string]string{MaxConcurrentRequestsVariable: "0"},
			"OPAMP_MAX_CONCURRENT_REQUESTS must be a whole number from 1 to 10000"},
		{"a collector rate that isn't a number", map[string]string{CollectorRequestsPerMinuteVariable: "sixty-marker"},
			"OPAMP_COLLECTOR_REQUESTS_PER_MINUTE must be a whole number from 1 to 600"},
		{"a default rate over its bound", map[string]string{DefaultRequestsPerMinuteVariable: "12000001"},
			"OPAMP_DEFAULT_REQUESTS_PER_MINUTE must be a whole number from 1 to 12000000"},
		{"an organization's share of 0", map[string]string{MaxConcurrentRequestsPerOrganizationVariable: "0"},
			"OPAMP_MAX_CONCURRENT_REQUESTS_PER_ORGANIZATION must be a whole number from 1 to OPAMP_MAX_CONCURRENT_REQUESTS's value, 100"},
		{"an organization's share over the pod's cap", map[string]string{MaxConcurrentRequestsVariable: "10",
			MaxConcurrentRequestsPerOrganizationVariable: "11"},
			"OPAMP_MAX_CONCURRENT_REQUESTS_PER_ORGANIZATION must be a whole number from 1 to OPAMP_MAX_CONCURRENT_REQUESTS's value, 10"},
		{"an organization's share that isn't a number", map[string]string{MaxConcurrentRequestsPerOrganizationVariable: "25-marker"},
			"OPAMP_MAX_CONCURRENT_REQUESTS_PER_ORGANIZATION must be a whole number from 1 to OPAMP_MAX_CONCURRENT_REQUESTS's value, 100"},
		{"a server URL with port 0", map[string]string{ServerURLVariable: "http://opamp-marker.example:0/opamp/api/v1"},
			"OPAMP_SERVER_URL must name a port from 1 to 65535, or none"},
		{"a server URL with a port over 65535", map[string]string{ServerURLVariable: "http://opamp-marker.example:65536/opamp/api/v1"},
			"OPAMP_SERVER_URL must name a port from 1 to 65535, or none"},
		{"a server URL with a port with a leading zero", map[string]string{ServerURLVariable: "http://opamp-marker.example:051234/opamp/api/v1"},
			"OPAMP_SERVER_URL must name a port from 1 to 65535, or none"},
		{"a server URL with an empty port", map[string]string{ServerURLVariable: "http://opamp-marker.example:/opamp/api/v1"},
			"OPAMP_SERVER_URL must name a port from 1 to 65535, or none"},
	} {
		t.Run(c.name, func(t *testing.T) {
			values := map[string]string{ServerURLVariable: testServerURL}
			for name, value := range c.values {
				values[name] = value
			}
			_, err := LoadSettings(envOf(values))
			if err == nil || err.Error() != c.want {
				t.Fatalf("error = %v, want %q", err, c.want)
			}
			// Every value but a bare number (which a bound may spell) is
			// searched for whole, and every marker.
			for _, value := range c.values {
				if len(value) > 5 && strings.Contains(err.Error(), value) {
					t.Fatalf("the refusal quotes the value: %q", err)
				}
			}
			if strings.Contains(err.Error(), "marker") {
				t.Fatalf("the refusal quotes a value: %q", err)
			}
		})
	}
}

// The heartbeat's bounds are its own: 10 and 300 are accepted.
func TestTheHeartbeatAcceptsItsBounds(t *testing.T) {
	for raw, want := range map[string]time.Duration{"10": 10 * time.Second, "300": 300 * time.Second, "": 30 * time.Second} {
		settings, err := LoadSettings(envOf(map[string]string{ServerURLVariable: testServerURL, HeartbeatIntervalVariable: raw}))
		if err != nil || settings.HeartbeatInterval != want {
			t.Errorf("OPAMP_HEARTBEAT_INTERVAL_SECONDS=%q: %v, %v", raw, settings.HeartbeatInterval, err)
		}
	}
}

// A server URL whose host is a loopback address is accepted, and flagged
// so serve warns once; host.docker.internal (compose's default) and a
// public name are not loopback.
func TestTheServerURLsLoopbackHostIsFlagged(t *testing.T) {
	for raw, loopback := range map[string]bool{
		"http://localhost:51234/opamp/api/v1":            true,
		"http://LOCALHOST.:51234/opamp/api/v1":           true,
		"http://collector.localhost/opamp/api/v1":        true,
		"http://127.0.0.1:51234/opamp/api/v1":            true,
		"http://127.9.8.7/opamp/api/v1":                  true,
		"http://[::1]:51234/opamp/api/v1":                true,
		"http://[::ffff:127.0.0.1]:51234/opamp/api/v1":   true,
		"http://host.docker.internal:51234/opamp/api/v1": false,
		"https://opamp.example.com/opamp/api/v1":         false,
		"http://10.0.0.7:51234/opamp/api/v1":             false,
		"http://telflo-opamp-server:51234/opamp/api/v1":  false,
	} {
		url, got, err := ServerURL(envOf(map[string]string{ServerURLVariable: raw}))
		if err != nil || url != raw || got != loopback {
			t.Errorf("ServerURL(%q) = %q, %v, %v; want loopback %v", raw, url, got, err, loopback)
		}
	}
}

// A server URL whose path isn't the agent route is accepted (a proxy in
// front may rewrite it) and flagged, so serve warns once; the agent route
// itself, with any port from 1 to 65535 or none, isn't flagged.
func TestTheServerURLsPathIsFlaggedUnlessItIsTheAgentRoute(t *testing.T) {
	for raw, unexpected := range map[string]bool{
		"http://host.docker.internal:51234/opamp/api/v1": false,
		"https://opamp.example.com/opamp/api/v1":         false,
		"https://opamp.example.com:1/opamp/api/v1":       false,
		"https://opamp.example.com:65535/opamp/api/v1":   false,
		"https://opamp.example.com/opamp/api/v1/":        true,
		"https://opamp.example.com/":                     true,
		"https://opamp.example.com":                      true,
		"https://opamp.example.com/agents/opamp/api/v1":  true,
		"https://opamp.example.com/OPAMP/api/v1":         true,
	} {
		settings, err := LoadSettings(envOf(map[string]string{ServerURLVariable: raw}))
		if err != nil || settings.ServerURLUnexpectedPath != unexpected {
			t.Errorf("%q: flagged %v, %v; want %v", raw, settings.ServerURLUnexpectedPath, err, unexpected)
		}
	}
}

// The diagnostics listener is off while its port is unset; set, it binds
// 127.0.0.1 by default or the loopback address given, and refuses any
// other address and a host without a port, naming the variable only.
func TestTheDiagnosticsListenerIsOffOrOnALoopbackAddress(t *testing.T) {
	for _, c := range []struct {
		name   string
		values map[string]string
		want   *Diagnostics
	}{
		{"unset", nil, nil},
		{"a port", map[string]string{DiagnosticsPortVariable: "6060"}, &Diagnostics{Host: "127.0.0.1", Port: 6060}},
		{"a free port", map[string]string{DiagnosticsPortVariable: "0"}, &Diagnostics{Host: "127.0.0.1", Port: 0}},
		{"::1", map[string]string{DiagnosticsPortVariable: "6060", DiagnosticsHostVariable: "::1"}, &Diagnostics{Host: "::1", Port: 6060}},
		{"127.0.0.2", map[string]string{DiagnosticsPortVariable: "6060", DiagnosticsHostVariable: "127.0.0.2"}, &Diagnostics{Host: "127.0.0.2", Port: 6060}},
	} {
		got, err := LoadDiagnostics(envOf(c.values))
		if err != nil || (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s: %+v, %v; want %+v", c.name, got, err, c.want)
		}
	}
	for _, c := range []struct {
		name   string
		values map[string]string
		want   string
	}{
		{"every interface", map[string]string{DiagnosticsPortVariable: "6060", DiagnosticsHostVariable: "0.0.0.0"},
			"OPAMP_AGENT_DIAGNOSTICS_HOST must be a loopback address, such as 127.0.0.1 or ::1: the diagnostics have no authentication"},
		{"a network address", map[string]string{DiagnosticsPortVariable: "6060", DiagnosticsHostVariable: "10.0.0.7"},
			"OPAMP_AGENT_DIAGNOSTICS_HOST must be a loopback address, such as 127.0.0.1 or ::1: the diagnostics have no authentication"},
		{"a name", map[string]string{DiagnosticsPortVariable: "6060", DiagnosticsHostVariable: "localhost"},
			"OPAMP_AGENT_DIAGNOSTICS_HOST must be an IP address, such as 127.0.0.1 or ::1"},
		{"a bad port", map[string]string{DiagnosticsPortVariable: "60x"},
			"OPAMP_AGENT_DIAGNOSTICS_PORT must be a port number from 0 to 65535"},
		{"a host alone", map[string]string{DiagnosticsHostVariable: "127.0.0.1"},
			"OPAMP_AGENT_DIAGNOSTICS_HOST is set without OPAMP_AGENT_DIAGNOSTICS_PORT"},
	} {
		if _, err := LoadDiagnostics(envOf(c.values)); err == nil || err.Error() != c.want {
			t.Errorf("%s: %v; want %q", c.name, err, c.want)
		}
	}
	settings, err := LoadSettings(envOf(map[string]string{ServerURLVariable: testServerURL, DiagnosticsPortVariable: "6060"}))
	if err != nil || settings.Diagnostics == nil || settings.Diagnostics.Port != 6060 {
		t.Fatalf("LoadSettings doesn't carry the diagnostics: %+v, %v", settings.Diagnostics, err)
	}
}
