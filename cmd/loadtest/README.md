# OpAMP load-test client

`loadtest` impersonates many OpAMP **supervisor** clients at once so you can
measure how much polling load the current OpAMP server configuration absorbs
(CPU, memory, database connections, rate-limit headroom).

Each *virtual supervisor* is a goroutine with its own instance UID, generated
name (`col-0001`, `col-0002`, …), per-instance `host.ip`, and sequence counter.
It POSTs an `AgentToServer` protobuf to `/opamp/api/v1` on its own jittered
cadence and records the outcome. It manages no collector and runs no real OTel
OpAMP client — it just replays the protobuf a real supervisor sends, and holds
in memory the same state a real supervisor persists between polls.

From the server's (and its collector list's) point of view a virtual supervisor is
**indistinguishable from a real one**: it reports a distinct `host.ip` /
`host.name` / `service.version`, completes the token-rotation handoff, and
acknowledges delivered configs by hash. See [Lifecycle](#lifecycle).

It is a **pure client**: a fleet's default token already encodes
`{token_id, deployment_id}` (the fleet's id, under the key `deployment_id`),
so the tool needs no encryption key and no database access. You paste in
tokens; it replays them.

> **Heads up:** because these clients look real, each one persists a real
> instance row (and a rotated token) that shows up in the fleet's collector
> list (`GET …/fleets/{f}/collectors`) and does not self-clean. Point the
> tool at a throwaway/test fleet and clean the rows up yourself afterward.

## Build

```bash
cd opamp-server
go build -o bin/loadtest ./cmd/loadtest
```

## Run

```bash
# 5 clients against a local dev server (TLS disabled), fast poll to smoke-test
go run ./cmd/loadtest \
  --endpoint http://localhost:51234/opamp/api/v1 \
  --tokens "Bearer <ciphertext>" \
  --collector_group_id 8f3c… \
  --count 5 --poll_interval 5s --insecure

# 500 clients bound to a group, realistic cadence, 5-minute run — exercises the
# full rotation + config-delivery path (the group must have a published config)
go run ./cmd/loadtest \
  --endpoint https://opamp.example.com/opamp/api/v1 \
  --tokens-file ./tokens.txt \
  --collector_group_id 8f3c… \
  --count 500 --poll_interval 30s --deploy_interval 2m --duration 5m
```

By default the tool is **quiet except for errors** — transport failures and
non-2xx responses always print, nothing else does. Pass `--verbose` to turn on
the startup banner, the metrics summaries (every `--metrics-interval`, default
10s, plus a final one at exit), and the per-poll lifecycle logs:

```
active=500 total=8123 rps=16.4 | 2xx=7890 401=0 429=233(rl=233 shed=0) other=0 err=8(ref=6 rst=2 to=0 oth=0) rot=500 cfg=500 | p50=12ms p95=41ms p99=88ms max=210ms (n=164)
```

- **429** — split by the `Retry-After` the server sent, because the two 429s
  mean opposite things: **rl** is a rate limit (the organization over its
  requests a minute, or one collector over its own; `Retry-After` of 30 s or
  more — see below), **shed** is the server's admission gate at its
  in-flight cap, the pod's or the organization's share of it
  (`Retry-After: 3–8`, jittered — the pod is busy, not over a rate). A 429 with neither
  (older server, header stripped) counts in the total only.
- **401** — a token is invalid, revoked, or expired.
- **err** — requests that never got an HTTP response, split by cause:
  - **ref** (connection refused) and **rst** (reset / EOF / broken pipe) are
    almost always **generator/OS-side** limits — accept-backlog overflow,
    ephemeral-port pressure, keep-alive races — not the server topping out.
    Cut them with `--ramp`, `--shared-transport`, higher `ulimit -n`/`somaxconn`,
    or by running the generator on a **separate box** from the server.
  - **to** (timeout) is the interesting one: it means the server accepted the
    connection but was too slow to respond within the client's 30s deadline —
    a genuine server-side ceiling worth digging into (DB pool, CPU).
  - **oth** — anything else (including request-build failures).
- **rot** — rotated tokens **adopted** (the default→rotate handoff completed).
  Expect this to converge on `--count` shortly after startup.
- **cfg** — remote configs the server delivered and clients stored. Stays `0`
  unless the instances resolve a config (their group has one published) — see
  `--collector_group_id` below.

## Finding the server's real ceiling

The tool emulates N clients from **one** box, so at high `--count` the first
limits you hit are the generator's, not the server's (see **ref**/**rst** above).
To measure the *server*:

- **`--max-inflight N`** caps how many polls are in flight at once across all
  clients — a client-side throttle on offered concurrency. Sweep it (50 → 100 →
  250 → …) and watch where errors and latency appear: if `err` only climbs once
  offered concurrency is high, that's a real server ceiling; if it's high even at
  `--max-inflight 50`, something's broken low. It does **not** touch the server —
  it only shapes what this tool offers.
- Reduce generator noise with `--ramp`, `--shared-transport`, and OS limits so
  the errors you *do* see are the server's, not the laptop's.
- Remember steady-state load = `count × (60 / poll_interval_seconds)` requests
  a minute, and the rate limits are the first server wall (see below).

## Lifecycle

Each client walks the same handshake a real server-supervisor does:

1. **Poll 1 (default token).** The server replies with a bare ack carrying the
   `ReportFullState` flag **and** a rotation offer — a new per-instance token in
   `ConnectionSettings.Opamp`. The client adopts the token (keeping the same
   `--endpoint`; it does **not** follow the offer's `DestinationEndpoint`) and
   holds a noop config in memory.
2. **Poll 2 (rotated token, full state).** First poll on the new token reports
   the full `AgentDescription` + effective config. This is the server's first
   config-resolution pass; if the instance resolves a config, the response
   carries `RemoteConfig{body, ConfigHash}`, which the client stores in memory.
3. **Poll 3+ (rotated token).** Every poll now reports
   `RemoteConfigStatus{APPLIED, hash}`, flipping the instance to **in sync** in
   the UI, and re-reports the stored config as its effective state whenever the
   server asks for full state again.

The "apply" is memory-only — no collector restart, no config squashing — but the
token switch and hash acknowledgement the server sees are identical to a real
supervisor's.

**`--collector_group_id` is required**: the server refuses (`403`) a collector
whose description names no group. It also drives steps 2–3: config
resolution is two-tier (`instance pin ?? collector_group`) and a fake instance
has no pin, so a group without a published config resolves nothing and the
server just loops on `ReportFullState` acks (`cfg=0`). Point it at a group that
has a published config to drive the delivery path.

## Flags

| Flag | Default | Purpose |
|------|---------|---------|
| `--endpoint` | (required) | OpAMP server URL, e.g. `http://localhost:51234/opamp/api/v1` |
| `--tokens` | | comma-separated fleet default token(s) |
| `--tokens-file` | | file of tokens, one per line (`#` comments allowed) |
| `--count` | `1` | number of virtual supervisors |
| `--poll_interval` | `30s` | steady-state cadence: base delay between a client's polls after it has deployed |
| `--deploy_interval` | `0` (= `--poll_interval`) | window the first poll of all `--count` clients is spread across, i.e. how long the fleet takes to fully deploy |
| `--jitter` | `0.1` | ±fraction applied to each poll interval |
| `--name-prefix` | `col-` | prefix for generated `ServerName` values |
| `--collector_group_id` | (required) | `telflo.collector_group_id` each client reports, a group of the tokens' fleet; the server resolves its published config to deliver (see [Lifecycle](#lifecycle)) |
| `--ramp` | `0` | spread client startup over this window |
| `--duration` | `0` | total run time (`0` = until SIGINT) |
| `--metrics-interval` | `10s` | summary print cadence |
| `--max-inflight` | `0` | cap on polls in flight at once across all clients (`0` = unlimited); a client-side throttle on offered concurrency for finding the server's ceiling |
| `--insecure` | `false` | skip TLS verification (dev servers with `OPAMP_TLS_DISABLED=true`) |
| `--shared-transport` | `false` | share one HTTP transport; default gives each client its own connection (closer to N real supervisors) |
| `--verbose` | `false` | print all non-error output (startup banner, metrics summaries, per-poll lifecycle); errors and non-2xx responses always print. Pair with `--count 1` to trace a single client's handshake |
| `--poll-timing` | `false` | print a timing line for **every** poll: when it **started** (elapsed since launch) and how long it **took** (server round-trip). Separates client-side wait from per-poll server latency |
| `--otlp-endpoint` | `$OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP gRPC endpoint to export the tool's own metrics + phase markers to (see [Telemetry](#telemetry-and-phases)); empty = no export |
| `--run-id` | `opamp-<count>-<UTC ts>` | label stamped on every exported metric/log as `loadtest.run_id` |

Provide exactly one of `--tokens` or `--tokens-file`.

## Telemetry and phases

With `--otlp-endpoint` set, the tool exports its own view of the run over OTLP,
so it lands next to the server's own telemetry in whatever backend your
collector exports to. Nothing about what the server emits changes.

Metrics (resource: `service.name=telflo-loadtest`, `loadtest.run_id`,
`loadtest.count`, `loadtest.poll_interval`, `loadtest.target`):

| Metric | Type | Attributes |
|---|---|---|
| `loadtest.polls` | counter | `outcome` = 2xx / 401 / 429/ratelimit / 429/shed / 429 / other / error (+ `error.kind`), `phase` |
| `loadtest.poll.duration` (ms) | histogram | `outcome`, `phase` |
| `loadtest.rotations`, `loadtest.configs.delivered` | counter | |
| `loadtest.clients.active` | gauge | |
| `loadtest.phase` (1 = current) / `loadtest.phase.index` (0/1) | gauge | `phase` |

Log events (`LogAttributes['loadtest.event']`): `run.start`, `phase.start`
(with `reason`, `rotations`, `configs`), `run.end` (final counters).

A run has two phases. **registration** runs from launch until every client
has adopted a rotated token and stored a delivered config (a group without a
published config leaves the deadline to end it); a deadline of `deploy_interval + 2 × jittered poll_interval
+ 5s` ends it regardless (the `reason` records any shortfall, e.g. clients
shed with 429). Everything after is **steady**. The transition is always
printed to the console, even without `--verbose`.

The standard `OTEL_EXPORTER_OTLP_HEADERS=authorization=<key>` is honored, so
the tool can also export straight into a ClickStack ingest endpoint without a
collector in between.

## The rate limits are the first wall

Each pod counts, on its own (docs/agent-endpoint.md, "The limits are per pod"):

- an organization's agent requests a minute: its own `requestsPerMinute`
  (`PUT …/limits` on the management API), else the server's
  `OPAMP_DEFAULT_REQUESTS_PER_MINUTE` (12,000 by default);
- each collector's requests a minute: `OPAMP_COLLECTOR_REQUESTS_PER_MINUTE`
  (60 by default, with a burst of 20).

Past either, requests get **429** with a `Retry-After` of 30–60 s (the
organization's rate) or 60 s (a collector's), the `rl` count. Every client
of a token polls under that token's organization, and the tool does not
honor `Retry-After`, so a run past its organization's rate on a pod is
refused for as long as it keeps up the pace. To push higher:

- raise the organization's `requestsPerMinute` (`PUT …/limits`), or the
  server's `OPAMP_DEFAULT_REQUESTS_PER_MINUTE`, or
- lengthen `--poll_interval`, or
- spread across **tokens of fleets in different organizations** via
  `--tokens` / `--tokens-file` (clients are assigned round-robin across the
  list), or
- run more pods behind the load balancer (each counts its own).

## Getting tokens

The tool needs a fleet's **default token's bearer**. Only the server mints
bearers (it encrypts `{token_id, deployment_id}` with its
`OPAMP_ENCRYPTION_KEY`), through its management API: a fleet's create
answers both its tokens' bearers, and `POST …/tokens/{t}/reveal` answers a
fresh bearer for a token (docs/management-api.md). Locally,
`compose.yaml`'s server works the same way. A bearer is a credential: keep
the tokens file out of your shell history and the repository.

## Scope

Server fleets only (`/opamp/api/v1`). The OpenTelemetry Operator bridge's
endpoint (`/opamp/api/v1/kubernetes`), which used a different message
shape, is not served by this server, so the tool has no mode for it.

## Test

```bash
GOTOOLCHAIN=local go test ./cmd/loadtest/...
```
