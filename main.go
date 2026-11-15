// Commands
//
//	opamp-server serve                   the management API and, on its own
//	                                     port, the agent endpoint that
//	                                     collectors poll, in one process
//	opamp-server serve agent             the agent endpoint alone
//	opamp-server serve management        the management API alone
//	opamp-server migrate up|status       the embedded migrations, as the owner
//	opamp-server migrate down --yes-drop-data
//	                                     rolls the newest one back, dropping
//	                                     its data: for development only
//	opamp-server healthcheck [agent|management]
//	                                     asks a running server's /health on
//	                                     that component's port (the
//	                                     management API's by default), for
//	                                     the distroless image's health check

// Settings come from the environment in three groups. The Helm chart sets
// each one from its Secret, a TLS Secret or its values
// (charts/opamp-server/README.md).
//
// Credentials, to be kept in a secret store and never in a plain config file:
//
//	OPAMP_DATABASE_URL                 serve: the server's own login role
//	                                   (telflo_opamp_server), a member of
//	                                   telflo_opamp_server_runtime; holds its
//	                                   password
//	OPAMP_MIGRATION_DATABASE_URL       migrate: telflo_opamp_server_owner; holds its
//	                                   password
//	OPAMP_MANAGEMENT_SECRET            serve: the management API's shared
//	                                   secret, at least 32 bytes
//	OPAMP_MANAGEMENT_SECRETS_PREVIOUS  serve: comma-separated secrets still
//	                                   accepted during a rotation, each at
//	                                   least 32 bytes (empty entries ignored)
//	OPAMP_ENCRYPTION_KEY               serve: the enrollment tokens' key, at
//	OPAMP_ENCRYPTION_KEY_FILE          least 32 bytes, as a value or a file
//	                                   (exactly one); the value is unset from
//	                                   the environment once read
//	OPAMP_ENCRYPTION_KEYS_PREVIOUS     serve: comma-separated keys still
//	OPAMP_ENCRYPTION_KEYS_PREVIOUS_FILE accepted during a key rotation, each
//	                                   at least 32 bytes, at most 8, as a
//	                                   value or a file (at most one)
//
// TLS, paths to a certificate and its key:
//
//	OPAMP_MANAGEMENT_TLS_CERT_FILE     serve, healthcheck: both or neither;
//	OPAMP_MANAGEMENT_TLS_KEY_FILE      with both, the management API speaks
//	                                   TLS 1.2 or later
//	OPAMP_AGENT_TLS_CERT_FILE          serve: both or neither; with both, the
//	OPAMP_AGENT_TLS_KEY_FILE           agent endpoint speaks TLS 1.2 or
//	                                   later; without, a TLS-terminating
//	                                   load balancer is expected in front
//
// Plain settings:
//
//	OPAMP_AGENT_DIAGNOSTICS_PORT       serve agent: the diagnostics' port on a
//	OPAMP_AGENT_DIAGNOSTICS_HOST       loopback address (127.0.0.1 by
//	                                   default; any other refused); unset,
//	                                   no diagnostics listener
//	OPAMP_SERVER_URL                   serve, required: the agent endpoint's
//	                                   public http(s) URL, handed to
//	                                   collectors (no user name, password,
//	                                   query or fragment; a port from 1 to
//	                                   65535 or none)
//	OPAMP_MANAGEMENT_PORT              serve, healthcheck: the management port
//	                                   (default 51235)
//	OPAMP_MANAGEMENT_HOST              serve, healthcheck: the IP address to
//	                                   listen on (default: every interface)
//	OPAMP_AGENT_PORT                   serve: the agent endpoint's port
//	                                   (default 51234)
//	OPAMP_AGENT_HOST                   serve: the IP address it listens on
//	                                   (default: every interface)
//	OPAMP_HEARTBEAT_INTERVAL_SECONDS   serve: the heartbeat handed to
//	                                   collectors (default 30, 10 to 300)
//	OPAMP_DEFAULT_REQUESTS_PER_MINUTE  serve: an organization's agent requests
//	                                   a minute to each pod when it has none
//	                                   set (default 12000)
//	OPAMP_COLLECTOR_REQUESTS_PER_MINUTE serve: one collector's requests a
//	                                   minute, with a burst of 20 (default 60)
//	OPAMP_MAX_CONCURRENT_REQUESTS      serve: the agent requests one pod
//	                                   handles at once (default 100)
//	OPAMP_MAX_CONCURRENT_REQUESTS_PER_ORGANIZATION
//	                                   serve: one organization's agent
//	                                   requests one pod handles at once
//	                                   (default 25, at most the above)
//	OPAMP_SHUTDOWN_DRAIN_SECONDS       serve: how long both ports keep
//	                                   serving once /ready answers 503 on
//	                                   SIGTERM or SIGINT (default 5, 0 to 30)
//	OTEL_EXPORTER_OTLP_ENDPOINT        serve: OTLP export, off when unset
//	OTEL_SERVICE_NAME                  serve: the telemetry's service.name
//	                                   (default telflo-opamp-server)
//	OTEL_RESOURCE_ATTRIBUTES           serve: more resource attributes,
//	                                   k1=v1,k2=v2
//
// serve's agent port answers POST /opamp/api/v1 (the poll), GET /health
// (liveness) and GET /ready (the database answers, on a connection of its
// own, and serve isn't stopping); the management port answers /health,
// /ready and the API behind the secret, the diagnostics
// (/api/v1/debug/pprof/, /api/v1/debug/admission) included. `serve agent`
// serves the diagnostics on OPAMP_AGENT_DIAGNOSTICS_PORT instead, on a
// loopback address, when it is set. On SIGTERM or SIGINT, /ready answers
// 503 at once and every port finishes its requests within 10 seconds.
//
// `serve agent` reads only the process's settings and the agent
// endpoint's (never the management secret); `serve management` only the
// process's and the management API's (never OPAMP_SERVER_URL).
//
// deploy/bootstrap.sql creates the database and the roles, run once by a
// superuser before the first `migrate up`.
//
// main only dispatches: each subcommand takes its context, the environment
// and its log's writer, so tests run it in-process.
package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/telflo/opamp-server/serve"
)

const usage = "usage: opamp-server serve [agent|management] | migrate up|status | migrate down " + yesDropData +
	" | healthcheck [agent|management]"

// main stops serve gracefully on SIGTERM (compose and Kubernetes send it)
// or SIGINT.
func main() {
	ctx, stop := signalContext()
	code := run(ctx, os.Args[1:], os.Getenv, os.Unsetenv, os.Stdout)
	stop()
	os.Exit(code)
}

// signalContext ends on the first SIGINT or SIGTERM, and then stops
// catching them: a second one gets the default action and ends the
// process, so a Ctrl-C still works on a server stuck shutting down.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	context.AfterFunc(ctx, stop)
	return ctx, stop
}

// runs the subcommand args names and returns the process's exit code:
// 0 on success, 1 when the subcommand fails, 2 for a command line it doesn't
// understand. Every log line goes to stdout as JSON. serve unsets the
// encryption keys' variables with unsetenv once it has read them.
func run(ctx context.Context, args []string, getenv func(string) string, unsetenv func(string) error, stdout io.Writer) int {
	logger := slog.New(slog.NewJSONHandler(stdout, nil))
	if len(args) == 0 {
		logger.Error(usage)
		return 2
	}
	switch args[0] {
	case "serve":
		role, ok := serve.ParseRole(args[1:])
		if !ok {
			logger.Error(usage)
			return 2
		}
		return serve.Serve(ctx, role, getenv, unsetenv, logger)
	case "migrate":
		return migrate(ctx, args[1:], getenv, logger)
	case "healthcheck":
		target, ok := parseHealthTarget(args[1:])
		if !ok {
			logger.Error(usage)
			return 2
		}
		return healthcheck(ctx, target, getenv, logger)
	default:
		logger.Error(usage)
		return 2
	}
}
