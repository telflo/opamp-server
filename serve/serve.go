// Package serve is `opamp-server serve`: the process that runs the
// components. It reads the process's settings (the database URL, the
// encryption keys, the shutdown drain) and each component's, opens the
// database pools, starts telemetry, builds the components (package agent's
// endpoint, package management's API), opens a listener for each, answers
// /ready, runs the error history's cleanup, and stops gracefully: the
// drain, the listeners' shutdown, then the pools, the keys and telemetry,
// in that order.
//
// The components hold no process code, and the process holds no request
// handling: a component is a handler built from its settings and stores.
package serve

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/awnumar/memguard"

	"github.com/telflo/opamp-server/agent"
	"github.com/telflo/opamp-server/management"
	"github.com/telflo/opamp-server/middleware"
	"github.com/telflo/opamp-server/store/database"
	"github.com/telflo/opamp-server/telemetry"
	"github.com/telflo/opamp-server/tokens"
)

// Serve is `opamp-server serve [agent|management]`: it reads its settings
// for role, opens the database and checks it, starts telemetry, and serves
// the components role runs (the management API, the agent endpoint, or
// both), each on its own listener, until ctx ends; then it stops
// gracefully. The enrollment tokens' encryption keys are read first, and
// their value variables unset with unsetenv (main passes os.Unsetenv).
//
// Nothing is listened on until every check passes. Every refusal is one log
// line naming the problem, never a URL, a secret or a key, and exit code 1.
// Once every listener is open, /ready answers (on each component's port)
// and, with the agent endpoint, the error history's cleanup starts (once,
// then hourly).
//
// When ctx ends (the first SIGTERM or SIGINT), /ready answers 503 at once;
// the listeners keep serving through the shutdown drain
// (OPAMP_SHUTDOWN_DRAIN_SECONDS, 5 by default), then stop together, their
// in-flight requests given ShutdownTimeout and any still running cut off, a
// delete's included (serveListeners). Then the cleanup stops, both pools
// close, and the keys are destroyed and memguard purges every locked
// buffer, only after every listener's Shutdown returned, so no request
// mints or reads a bearer after; telemetry flushes last. The keys are
// destroyed on every other way out too.
//
// It returns the process's exit code: 0 after a graceful stop; 1 for a
// refusal, a listener that can't open or stops on its own, or requests cut
// off at the shutdown timeout.
func Serve(ctx context.Context, role Role, getenv func(string) string, unsetenv func(string) error, logger *slog.Logger) int {
	return serveWith(ctx, role, getenv, unsetenv, logger, destroyAndPurge, nil)
}

// destroyAndPurge is how serve destroys the keys: the keyring's buffers,
// then every locked buffer the process holds (memguard.Purge).
func destroyAndPurge(keys *tokens.Keyring) {
	destroyKeyring(keys)
	memguard.Purge()
}

// serveWith does Serve's work, with two parameters a test can replace (the last two) :
//
//   - destroy wipes the encryption keys. serveWith calls it exactly once,
//     however it returns: a clean stop, a refusal or a failure. Serve
//     passes destroyAndPurge; a test passes a recorder to check the call.
//   - adjust, when not nil, is called with the replica after it is built
//     and before any port opens. Serve passes nil; a test uses it to
//     swap in its own parts, such as a drain timer it controls or a
//     handler that holds a request in flight.
func serveWith(ctx context.Context, role Role, getenv func(string) string, unsetenv func(string) error, logger *slog.Logger,
	destroy func(*tokens.Keyring), adjust func(*replica)) int {
	settings, err := loadSettings(role, getenv, unsetenv, destroy)
	if err != nil {
		logger.Error("Refusing to start: " + err.Error())
		return 1
	}
	var destroyed sync.Once
	destroyKeys := func() { destroyed.Do(func() { destroy(settings.Keys) }) }
	defer destroyKeys()

	pool, err := database.OpenPool(ctx, settings.DatabaseURL)
	if err != nil {
		logger.Error("Refusing to start: " + DatabaseURLVariable + " " + err.Error())
		return 1
	}
	var closed sync.Once
	closePool := func() { closed.Do(pool.Close) }
	defer closePool()
	if err := database.CheckStartup(ctx, pool); err != nil {
		// Its errors quote no driver text already; described again, so a
		// wrapper that ever let one through still logs none.
		logger.Error(database.DescribeError(err))
		return 1
	}
	// ready pings on a pool of its own, one connection, never the request
	// pool's: busy requests must not make the pod unready.
	readyPool, err := database.OpenReadinessPool(ctx, settings.DatabaseURL)
	if err != nil {
		logger.Error("Refusing to start: " + DatabaseURLVariable + " " + err.Error())
		return 1
	}
	var readyClosed sync.Once
	closeReadyPool := func() { readyClosed.Do(readyPool.Close) }
	defer closeReadyPool()

	// A no-op without OTEL_EXPORTER_OTLP_ENDPOINT. Telemetry that can't
	// start never stops the server. The error isn't logged: it could quote
	// the endpoint.
	tel, err := telemetry.Setup(ctx, getenv, logger)
	if err != nil {
		logger.Warn("telemetry couldn't start; serving without it")
		tel = telemetry.Noop()
	}
	// The end of the stop, in its order: the pools, the keys, telemetry.
	defer func() {
		closeReadyPool()
		closePool()
		destroyKeys()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
		defer cancel()
		if err := tel.Shutdown(shutdownCtx); err != nil {
			logger.Warn("telemetry didn't flush")
		}
	}()

	r, err := newReplica(settings, postgresStores(pool), readyPool.Ping, time.Now, logger)
	if err != nil {
		logger.Error("Refusing to start: " + err.Error())
		return 1
	}
	if adjust != nil {
		adjust(r)
	}
	ticker := time.NewTicker(CleanupInterval)
	defer ticker.Stop()
	return r.serve(ctx, settings, logger, ShutdownTimeout, ticker.C)
}

// serve runs the replica's listeners until ctx ends (serveListeners),
// marking /ready serving once every one is open and starting the cleanup
// job then, on cleanupTicks, when the replica has one; and on the way out
// marks /ready draining before the listeners stop, and stops the cleanup
// job after. It returns serve's exit code.
func (r *replica) serve(ctx context.Context, settings Settings, logger *slog.Logger, shutdownTimeout time.Duration,
	cleanupTicks <-chan time.Time) int {
	cleanupCtx, stopCleanup := context.WithCancel(context.Background())
	defer stopCleanup()
	cleanupDone := make(chan struct{})
	started := false
	code := serveListeners(ctx, r.listeners(settings), lifecycle{
		drain: settings.ShutdownDrain,
		after: r.drainTimer,
		serving: func() {
			r.warn(settings, logger)
			r.readiness.serving()
			if r.cleanup == nil {
				return
			}
			started = true
			go func() {
				defer close(cleanupDone)
				r.cleanup.run(cleanupCtx, cleanupTicks)
			}()
		},
		draining: r.readiness.drain,
	}, logger, shutdownTimeout)
	stopCleanup()
	if started {
		<-cleanupDone
	}
	return code
}

// listeners are the replica's listeners: the management API's, the agent
// endpoint's and its diagnostics', each when the replica has it.
func (r *replica) listeners(settings Settings) []listenerSpec {
	var specs []listenerSpec
	if r.management != nil {
		specs = append(specs, managementSpec(settings, r.management.Handler()))
	}
	if r.agent != nil {
		specs = append(specs, agentSpec(settings, r.agent))
	}
	if r.diagnostics != nil {
		specs = append(specs, diagnosticsSpec(settings, r.diagnostics))
	}
	return specs
}

// warn writes, once every listener is open, what the settings leave
// questionable: the agent endpoint's plain HTTP and its server URL, and a
// management secret given to a process that runs no management API.
func (r *replica) warn(settings Settings, logger *slog.Logger) {
	if settings.UnusedManagementSecret {
		logger.Warn(management.SecretVariable + " is set, but `serve " + settings.Role.String() +
			"` runs no management API: this process needn't hold the secret")
	}
	if r.agent == nil {
		return
	}
	if settings.Agent.TLS == nil {
		logger.Info("the agent endpoint speaks plain HTTP: a TLS-terminating load balancer is expected in front of it")
	}
	if settings.Agent.ServerURLLoopback {
		logger.Warn(agent.ServerURLVariable + " names a loopback address: a collector in a container or on another machine can't reach it")
	}
	if settings.Agent.ServerURLUnexpectedPath {
		logger.Warn(agent.ServerURLVariable + "'s path isn't " + middleware.AgentRoute +
			": collectors handed it poll a path this server doesn't answer, unless a proxy in front rewrites it")
	}
}

// managementSpec is the management listener (OPAMP_MANAGEMENT_HOST,
// OPAMP_MANAGEMENT_PORT, and TLS when its two files are set).
func managementSpec(settings Settings, handler http.Handler) listenerSpec {
	encryptionKeys := 0
	if settings.Keys != nil {
		encryptionKeys = settings.Keys.Count()
	}
	acceptedSecrets := 0
	if settings.Management.Secrets != nil {
		acceptedSecrets = settings.Management.Secrets.Count()
	}
	return listenerSpec{
		what: "the management API", listener: "the management listener", port: "the management port",
		address: net.JoinHostPort(settings.Management.Host, strconv.Itoa(settings.Management.Port)),
		tls:     settings.Management.TLS, handler: handler,
		attrs: []any{"acceptedSecrets", acceptedSecrets, "encryptionKeys", encryptionKeys},
	}
}

// agentSpec is the agent listener (OPAMP_AGENT_HOST, OPAMP_AGENT_PORT, and
// TLS when its two files are set). It faces the collectors' networks, so
// its headers are capped at agent.MaxHeaderBytes and its connections at
// OPAMP_AGENT_MAX_CONNECTIONS.
func agentSpec(settings Settings, handler http.Handler) listenerSpec {
	return listenerSpec{
		what: "the agent endpoint", listener: "the agent listener", port: "the agent port",
		address: net.JoinHostPort(settings.Agent.Host, strconv.Itoa(settings.Agent.Port)),
		tls:     settings.Agent.TLS, handler: handler,
		maxHeaderBytes: agent.MaxHeaderBytes, maxConnections: settings.Agent.MaxConnections,
		attrs: []any{"heartbeatSeconds", int(settings.Agent.HeartbeatInterval.Seconds()),
			"maxConcurrentRequests", settings.Agent.Limits.MaxConcurrentRequests,
			"maxConnections", settings.Agent.MaxConnections},
	}
}

// diagnosticsSpec is the agent endpoint's diagnostics listener
// (OPAMP_AGENT_DIAGNOSTICS_HOST, a loopback address, and
// OPAMP_AGENT_DIAGNOSTICS_PORT): plain HTTP, since it never leaves the host.
func diagnosticsSpec(settings Settings, handler http.Handler) listenerSpec {
	return listenerSpec{
		what: "the agent's diagnostics", listener: "the diagnostics listener", port: "the diagnostics port",
		address: net.JoinHostPort(settings.Agent.Diagnostics.Host, strconv.Itoa(settings.Agent.Diagnostics.Port)),
		handler: handler,
	}
}
