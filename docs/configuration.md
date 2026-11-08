# Configuring and running serve

`serve` runs the server's two components from one binary, on one
database:

| Command | Runs |
|---|---|
| `opamp-server serve agent` | the agent endpoint, which collectors poll |
| `opamp-server serve management` | the management API |
| `opamp-server serve` | both, in one process |

Run separately, each scales and restarts on its own, and the agent endpoint
never holds the management secret. Both must run the same release.

```sh
OPAMP_DATABASE_URL=postgresql://telflo_opamp:<password>@db.internal:5432/telflo_opamp?pool_max_conns=10 \
OPAMP_MANAGEMENT_SECRET=<at least 32 bytes> \
OPAMP_ENCRYPTION_KEY_FILE=/run/secrets/opamp-encryption-key \
OPAMP_SERVER_URL=https://opamp.example.com/opamp/api/v1 \
  opamp-server serve
```

## The required settings

Every setting is an environment variable; the Helm chart sets them from
its Secrets and values. Generate the secret and the key with
`openssl rand -base64 48`, and keep every value in a secret store.

| Variable | Read by |
|---|---|
| [`OPAMP_DATABASE_URL`](#opamp_database_url) | `serve`, `serve agent`, `serve management` |
| [`OPAMP_MIGRATION_DATABASE_URL`](#opamp_migration_database_url) | `migrate` |
| [`OPAMP_MANAGEMENT_SECRET`](#opamp_management_secret) | `serve`, `serve management` |
| [`OPAMP_ENCRYPTION_KEY_FILE` or `OPAMP_ENCRYPTION_KEY`](#opamp_encryption_key_file-or-opamp_encryption_key) | `serve`, `serve agent`, `serve management` |
| [`OPAMP_SERVER_URL`](#opamp_server_url) | `serve`, `serve agent` |

### `OPAMP_DATABASE_URL`

The database, as the server's own role, `telflo_opamp`, which reads and
writes rows but can't change tables. `serve` refuses to start as the
owner or a superuser.

```sh
OPAMP_DATABASE_URL='postgresql://telflo_opamp:<password>@db.internal:5432/telflo_opamp?pool_max_conns=10'
```

- Set `pool_max_conns`; see [The connection pool](#the-connection-pool).
- Percent-encode special characters in the password, such as `@` as `%40`.
- Across a network, add `&sslmode=verify-full`. For a database whose
  certificate comes from a private CA, also add `&sslrootcert=<file>`.

### `OPAMP_MIGRATION_DATABASE_URL`

The same database, as its owner, `telflo_opamp_owner`. Only `migrate up`
uses it, and it refuses any other role.

```sh
OPAMP_MIGRATION_DATABASE_URL='postgresql://telflo_opamp_owner:<password>@db.internal:5432/telflo_opamp'
```

### `OPAMP_MANAGEMENT_SECRET`

The shared secret for the management API, sent as
`Authorization: Bearer <secret>`, at least 32 bytes. It opens everything,
so give day-to-day callers an API key instead
([security.md](security.md)). To change it without an outage, put the old
one in `OPAMP_MANAGEMENT_SECRETS_PREVIOUS` while the new one rolls out.

```sh
OPAMP_MANAGEMENT_SECRET='<output of openssl rand -base64 48>'
```

### `OPAMP_ENCRYPTION_KEY_FILE` or `OPAMP_ENCRYPTION_KEY`

The key that encrypts enrollment tokens' bearers. Set one of the two;
prefer the file, since a value can be read from the process's
environment. At least 32 bytes, with no whitespace at either end. Keep it
the same on every replica and across restarts, and back it up: bearers
made with it can't be read without it. [security.md](security.md) says
how to rotate it.

```sh
OPAMP_ENCRYPTION_KEY_FILE=/run/secrets/opamp-encryption-key
```

### `OPAMP_SERVER_URL`

The agent endpoint's URL as collectors reach it, such as your load
balancer's. Every collector is handed it after its first poll and polls
it from then on, so a URL collectors can't reach strands them.

```sh
OPAMP_SERVER_URL=https://opamp.example.com/opamp/api/v1
```

## Every setting

| Variable | Default | Meaning |
|---|---|---|
| `OPAMP_DATABASE_URL` | required | The server's role. See above. |
| `OPAMP_MIGRATION_DATABASE_URL` | required for `migrate` | The owner. See above. |
| `OPAMP_MANAGEMENT_SECRET` | required | See above. |
| `OPAMP_MANAGEMENT_SECRETS_PREVIOUS` | none | Comma-separated secrets still accepted during a rotation. |
| `OPAMP_ENCRYPTION_KEY`, `OPAMP_ENCRYPTION_KEY_FILE` | required, one of the two | See above. |
| `OPAMP_ENCRYPTION_KEYS_PREVIOUS`, `OPAMP_ENCRYPTION_KEYS_PREVIOUS_FILE` | none | Comma-separated keys still accepted for reading bearers during a rotation, at most 8. |
| `OPAMP_SERVER_URL` | required | See above. |
| `OPAMP_MANAGEMENT_PORT` | `51235` | The management API's port. |
| `OPAMP_MANAGEMENT_HOST` | every interface | The IP address it listens on, such as `127.0.0.1`. |
| `OPAMP_MANAGEMENT_TLS_CERT_FILE`, `OPAMP_MANAGEMENT_TLS_KEY_FILE` | none | Both or neither: PEM files that make the management API speak HTTPS. |
| `OPAMP_AGENT_PORT` | `51234` | The agent endpoint's port. |
| `OPAMP_AGENT_HOST` | every interface | The IP address it listens on. |
| `OPAMP_AGENT_TLS_CERT_FILE`, `OPAMP_AGENT_TLS_KEY_FILE` | none | Both or neither: HTTPS on the agent endpoint. Without them, it expects a load balancer to terminate TLS. |
| `OPAMP_AGENT_MAX_CONNECTIONS` | `10000` | Connections the agent endpoint holds open at once, idle ones included, 1 to 1,000,000. Past it, a new connection waits until one closes. A collector polling directly keeps one open. |
| `OPAMP_HEARTBEAT_INTERVAL_SECONDS` | `30` | How often collectors poll, 10 to 300. |
| `OPAMP_DEFAULT_REQUESTS_PER_MINUTE` | `12000` | An organization's agent requests a minute **to each pod**, unless its own limit is set (`PUT …/limits`). 12,000 is about 6,000 collectors. |
| `OPAMP_COLLECTOR_REQUESTS_PER_MINUTE` | `60` | One collector's requests a minute to each pod. |
| `OPAMP_MAX_CONCURRENT_REQUESTS` | `100` | Agent requests one pod handles at once; past it, `429`. |
| `OPAMP_MAX_CONCURRENT_REQUESTS_PER_ORGANIZATION` | `25` | The same, for one organization. |
| `OPAMP_AGENT_DIAGNOSTICS_PORT`, `OPAMP_AGENT_DIAGNOSTICS_HOST` | none, `127.0.0.1` | Go's profiles for `serve agent`, on a loopback address only. Reach it with `kubectl port-forward`. |
| `OPAMP_SHUTDOWN_DRAIN_SECONDS` | `5` | How long the listeners stay open after a stop signal, 0 to 30. See [Shutdown](#shutdown). |
| `OPAMP_ALLOW_DEVELOPMENT_SECRETS` | `false` | `true` lets `serve` start with `compose.yaml`'s committed management secret and encryption key. Only `compose.yaml` sets it; never set it in a deployment. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | none | OTLP over gRPC for traces and metrics. Telemetry is off when unset. |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | `telflo-opamp-server` | The telemetry's service name and attributes. |

Any other variable is ignored, apart from the OpenTelemetry SDK's own
`OTEL_*` options. [`.env.example`](../.env.example) has an example of
each.

## The connection pool

Each process holds at most `pool_max_conns` connections, plus one for
`/ready`. Unset, `pool_max_conns` depends on the machine's CPU count, so
set it. Each agent request holds a connection while its database work
runs, at most 5 seconds; a pod runs up to `OPAMP_MAX_CONCURRENT_REQUESTS`
at once, and those past the pool wait for a connection.

PostgreSQL's `max_connections` must hold, beside its other clients:

    replicas × (pool_max_conns + 1)

counting every process, agent and management, at its autoscaler's
maximum. For example, 4 replicas at `pool_max_conns=20` take up to 84.

## Startup checks

Before it listens, `serve` checks its settings, its database role and the
schema. If anything is wrong, it logs one line naming the problem (never
a value) and exits 1. It refuses, among others:

- a missing or malformed setting;
- `compose.yaml`'s development management secret or encryption key, which
  everyone holds, unless `OPAMP_ALLOW_DEVELOPMENT_SECRETS` is `true`;
- a database role that could do more than its grants allow, such as a
  superuser or the owner, or one without `telflo_opamp_runtime`'s rights
  ([database.md](database.md) says how to set the role up);
- a schema behind this build ("run `opamp-server migrate up`") or ahead of
  it ("run the opamp-server release that migrated it").

A running server keeps working through a migration, but one of the
previous release refuses the new schema when it restarts, so roll every
server to the new release right after `migrate up`.

## Shutdown

On `SIGTERM` or `SIGINT`, `/ready` answers `503` at once, and the
listeners keep serving for `OPAMP_SHUTDOWN_DRAIN_SECONDS`, so a load
balancer stops sending new polls before the port closes. Requests in
flight then get 10 seconds to finish. Give the container a stop grace
period of at least 30 seconds (`terminationGracePeriodSeconds` in
Kubernetes, `stop_grace_period` in Compose): Docker's default of 10 cuts
the shutdown short. With a `preStop` sleep in Kubernetes, set the drain
to 0.

## Logs and telemetry

`serve` writes JSON lines to stdout: one per listener at startup, one per
management API request (method, route, status, duration), and one per
refused or failed agent poll. Successful polls aren't logged, and repeated
refusals are summarized once a minute with a count.

No bearer, key, secret, configuration text or request body ever reaches a
log line, an error or a span. With `OTEL_EXPORTER_OTLP_ENDPOINT` set, the
server exports traces (a span per request and per database call) and Go
runtime metrics. If telemetry can't start, `serve` runs without it.

## Health checks

Each component answers `GET /health` with `200 ok` while the process is
up, without touching the database, and `GET /ready` when it can also
reach the database ([agent-endpoint.md](agent-endpoint.md)). The image has
no curl, so use `opamp-server healthcheck agent` or
`opamp-server healthcheck management` as the container's health check. It
asks that component's `/health` on its own address and exits 0 on a
`200`.
