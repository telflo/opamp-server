package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/telflo/telflo-opamp-server/agent"
	"github.com/telflo/telflo-opamp-server/internal/envconfig"
	"github.com/telflo/telflo-opamp-server/management"
)

// healthcheckTimeout bounds the request, under any sensible health check
// interval.
const healthcheckTimeout = 5 * time.Second

// healthTarget is the listener a health check asks: a component's, read
// from that component's own settings.
type healthTarget struct {
	name string
	port func(getenv func(string) string) (int, error)
	host func(getenv func(string) string) (string, error)
	tls  func(getenv func(string) string) (string, string, bool, error)
}

// The two components' health targets.
var (
	managementHealth = healthTarget{name: "management", port: management.Port, host: management.Host, tls: management.TLSFiles}
	agentHealth      = healthTarget{name: "agent", port: agent.Port, host: agent.Host, tls: agent.TLSFiles}
)

// parseHealthTarget is the target `healthcheck`'s argument names: none or
// "management" for the management API's port, "agent" for the agent
// endpoint's.
func parseHealthTarget(args []string) (healthTarget, bool) {
	switch {
	case len(args) == 0:
		return managementHealth, true
	case len(args) > 1:
		return healthTarget{}, false
	case args[0] == "management":
		return managementHealth, true
	case args[0] == "agent":
		return agentHealth, true
	}
	return healthTarget{}, false
}

// healthcheck asks the server on this host for target's /health and exits
// 0 on a 200, 1 otherwise: the distroless image has no curl or wget, so its
// health check runs the binary itself. It reads the component's own
// settings: the port, the host serve binds (a specific IP address is asked
// directly; every interface, the default, is asked on 127.0.0.1), and the
// TLS files, whose presence means https.
func healthcheck(ctx context.Context, target healthTarget, getenv func(string) string, logger *slog.Logger) int {
	port, err := target.port(getenv)
	if err != nil {
		logger.Error("healthcheck: " + err.Error())
		return 1
	}
	host, err := target.host(getenv)
	if err != nil {
		logger.Error("healthcheck: " + err.Error())
		return 1
	}
	_, _, useTLS, err := target.tls(getenv)
	if err != nil {
		logger.Error("healthcheck: " + err.Error())
		return 1
	}
	if host == "" || net.ParseIP(host).IsUnspecified() {
		host = "127.0.0.1"
	}
	scheme := "http"
	transport := &http.Transport{
		// Never through a proxy from the environment: this asks its own host.
		Proxy:             nil,
		DisableKeepAlives: true,
	}
	if useTLS {
		scheme = "https"
		transport.TLSClientConfig = &tls.Config{
			MinVersion: envconfig.MinTLSVersion,
			// Not verified, and only because of where it dials: this host's
			// own listener, on loopback or on the IP address serve binds here
			// (the host settings take only an IP address, never a name that
			// could resolve to another machine). The connection never
			// crosses a network, and no secret is sent: /health needs none.
			// The certificate names the address callers use, which needn't
			// be this one.
			InsecureSkipVerify: true,
		}
	}
	client := &http.Client{Transport: transport}

	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()

	url := scheme + "://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/health"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		logger.Error("healthcheck failed", "component", target.name, "error", err.Error())
		return 1
	}
	response, err := client.Do(request)
	if err != nil {
		logger.Error("healthcheck failed", "component", target.name, "error", err.Error())
		return 1
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		logger.Error("healthcheck failed", "component", target.name, "status", response.StatusCode)
		return 1
	}
	return 0
}
