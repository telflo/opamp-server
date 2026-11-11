<p align="center">
  <a href="https://telflo.com">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="docs/assets/telflo-logo-dark.png">
      <img alt="Telflo" src="docs/assets/telflo-logo-light.png" height="80">
    </picture>
  </a>
</p>

<h1 align="center">opamp-server</h1>

<p align="center">
  <strong>Fleet management for OpenTelemetry Collectors, over OpAMP, on your own infrastructure.</strong>
</p>

<p align="center">
  <a href="https://github.com/telflo/telflo-opamp-server/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/telflo/telflo-opamp-server/actions/workflows/ci.yml/badge.svg?branch=main"></a>
  <a href="https://github.com/telflo/telflo-opamp-server/releases/latest"><img alt="Release" src="https://img.shields.io/github/v/release/telflo/telflo-opamp-server"></a>
  <a href="go.mod"><img alt="Go" src="https://img.shields.io/github/go-mod/go-version/telflo/telflo-opamp-server"></a>
  <a href="LICENSE"><img alt="Licence" src="https://img.shields.io/badge/licence-Apache%202.0-blue"></a>
</p>

<p align="center">
  <a href="#quick-start-docker"><strong>Quick start</strong></a> ·
  <a href="#documentation"><strong>Docs</strong></a> ·
  <a href="api/openapi.yaml"><strong>API reference</strong></a> ·
  <a href="charts/telflo-opamp-server/README.md"><strong>Helm chart</strong></a> ·
  <a href="CHANGELOG.md"><strong>Changelog</strong></a>
</p>

<p align="center">
  <sub>An open-source project by <a href="https://telflo.com">Telflo</a>.</sub>
</p>

opamp-server is a self-hosted [OpAMP](https://opentelemetry.io/docs/specs/opamp/)
server. You publish OpenTelemetry Collector configurations to it, decide
which collectors run which, and it delivers them and tells you whether
each collector applied what it was sent. It has two components on one
PostgreSQL database, each deployed, scaled and stopped on its own:

- **The agent endpoint**, which collectors poll for their configuration.
- **The management API**, where you publish configurations and organize
  collectors into fleets and groups.

Each collector runs under its own OpenTelemetry Supervisor, the upstream
program that starts the collector and speaks OpAMP for it. At scale, you
install the Supervisor alongside each collector the way you install the
collector: as a service on each host, or as the entrypoint of a
collector's pod.

> **Status: before 1.0.** A minor release may still change the management
> API or the Helm chart in ways that need you to act.
> [docs/upgrading.md](docs/upgrading.md) lists every such change and what
> to do, and [CHANGELOG.md](CHANGELOG.md) lists every release.

**On this page:** [Pick your path](#pick-your-path) ·
[Core capabilities](#core-capabilities) ·
[Why opamp-server](#why-opamp-server) · [How it works](#how-it-works) ·
[Concepts](#concepts) · [Quick start](#quick-start-docker) ·
[What it doesn't do](#what-it-doesnt-do) ·
[Compatibility](#compatibility) · [Settings](#settings) ·
[Troubleshooting](#troubleshooting) · [Documentation](#documentation) ·
[Community and help](#community-and-help) · [About Telflo](#about-telflo)

## Pick your path

### Try it in ten minutes

Both components, PostgreSQL and a collector, in Docker Compose on your
own machine, with development passwords. You publish a configuration and
watch the collector apply it. You need Git and Docker.

[Start the Quick start ↓](#quick-start-docker)

### Run it on one machine

Each component by hand, from the release binary, so you see what each
part needs: PostgreSQL (in Docker, or yours), two secrets, `migrate`, and
two processes. You need PostgreSQL and `openssl`.

[Walk through it →](docs/standalone.md)

### Run it in production

The Helm chart runs each component as its own Deployment, with the
chart's PostgreSQL or yours, and TLS at your load balancer or in the
pods. You need a cluster, `kubectl` and Helm.

[Install the chart →](docs/kubernetes.md)

## Core capabilities

### Configuration rollout

Publish once, then roll out on purpose.

- **Publications never change.** Each is a snapshot of a configuration's
  YAML, for a collector distribution and version. Publishing again makes a
  new one, and one still in use can't be deleted.
- **Assign by group, pin by collector.** A group's collectors all run the
  publication it's assigned; one collector can be pinned to another.
- **One configuration, many environments.** `${org:NAME}` references are
  filled in per collector, from variables and the variable groups that
  override them.
- **A documented contract.** [`api/openapi.yaml`](api/openapi.yaml)
  (OpenAPI 3.1) is the management API, and the tests hold the server to it.

[Learn more about the management API →](docs/management-api.md)

### Fleet visibility

Know what every collector is running, and whether it's there.

- **Liveness:** `online`, `warning`, `unknown`, `offline`, from each
  collector's polls.
- **Sync:** `inSync`, `pending`, `failed` or `drifted`, comparing what
  the collector reports running with what it was sent.
- **Errors:** each collector's recent errors, newest first, and fleet
  summaries that count collectors by each state.

[Learn more about the state model →](docs/STATE_MODEL.md)

### Security

Credentials sized to who holds them.

- **Two ways in to the management API:** the management secret, for
  whatever runs the server, and API keys restricted to the organizations
  they were created for.
- **Enrollment tokens per fleet:** a default token to join with and a
  rotate token to poll with, rotated with a grace period, revealed again or
  revoked, and every action audited.
- **Bearers encrypted, never stored.** The agent endpoint, which faces the
  collectors' networks, never holds the management secret.

[Learn more about secrets, keys and TLS →](docs/security.md)

### Multi-tenancy

Many teams on one server, kept apart.

- **Everything belongs to an organization**, and every query is scoped to
  one.
- **Limits per organization** on collectors and request rates.
- **API keys per organization**, so a team's tooling never holds the
  secret that opens everyone's.

[Learn more about API keys →](docs/decisions/0003-api-keys.md)

### Built to operate

Small to run, and quiet when it's healthy.

- **Scale each side on its own.** Run any number of agent endpoint and
  management API replicas against one database; each pod rate-limits and
  sheds load on its own.
- **Observable.** JSON logs that never hold secrets or configuration
  text, OTLP traces and metrics, and health and readiness probes.
- **Safe to restart.** It checks its settings, role and schema before it
  listens, and drains gracefully on shutdown.

[Learn more about running it →](docs/configuration.md)

## Why opamp-server

OpAMP is a protocol, not a product. The OpenTelemetry Supervisor runs any
collector under OpAMP, but something has to hold your configurations,
decide which collector gets which, and tell you whether each one applied
it. opamp-server is that piece, and it needs nothing but PostgreSQL.

- **It runs where you do.** It's one binary in your infrastructure, with
  no hosted service behind it. Collectors run under the upstream
  Supervisor, with nothing of opamp-server's installed beside them.
- **Changes roll out on purpose.** Nothing reaches a collector until you
  assign it, and each collector reports whether it's running what it was
  sent.
- **Many teams share one server**, without sharing a credential.
- **It's small to operate.** Any number of replicas share one database
  and nothing else, and it installs with Docker, a binary or a Helm chart.

## How it works

One binary and one image hold both components. The command picks what a
process runs:

| Command | Runs | Used by |
|---|---|---|
| `opamp-server serve agent` | the agent endpoint alone | the Helm chart, the quick start |
| `opamp-server serve management` | the management API alone | the Helm chart, the quick start |
| `opamp-server serve` | both, in one process | one service or container that runs both, such as on a single VM |

```text
 your platform or tooling ──▶ management API ───────────────┐
   management secret or API key   serve management, :51235  │
                                                            ├──▶ PostgreSQL
 collectors (OpAMP supervisor) ──▶ agent endpoint ──────────┘
   enrollment token's bearer      serve agent, :51234
```

- **The management API** (port 51235) is private. Every request carries
  a credential in its `Authorization: Bearer <credential>` header: either
  the management secret, which opens every organization, or an API key,
  which is restricted to the organizations it was created for.
  It is where organizations, published versions, fleets, groups,
  collectors, tokens, limits and variables are managed.
- **The agent endpoint** (port 51234, `POST /opamp/api/v1`) is where
  collectors poll. It faces the collectors' networks, usually behind a load
  balancer, and every request carries an enrollment token's bearer. Run
  alone, it never holds the management secret.

The two components never call each other: they meet only in the database.
So running them separately gives each its own:

- **Scale.** Run as many agent endpoint replicas as your collectors need,
  and as few management API replicas as your administrators need: 10,000
  collectors and 5 administrators is many agent pods and one or two
  management pods.
- **Size and exposure.** Each gets its own CPU and memory, its own network
  (the agent endpoint on the collectors' networks, the management API on
  yours only), and its own TLS.
- **Restarts.** Changing one component's settings or resources restarts
  only that component's pods; the other keeps serving.
- **Outages.** With the management API stopped, collectors keep polling and
  keep their configuration; nothing can be changed until it is back. With
  the agent endpoint stopped, the management API keeps working; collectors
  keep running their last configuration, show as `offline` after five
  minutes, and catch up on their next poll once it is back.

Both components run the same release. A new release upgrades both: each
refuses a database schema newer than its own build.

A collector starts with its fleet's **default token** and names its
collector group in its agent description. On its first poll it is handed
the fleet's **rotate token** and the server's public URL
(`OPAMP_SERVER_URL`), and it polls with those from then on. Until its group
is assigned a version, it is sent nothing and keeps running its own
configuration.

## Concepts

- **Organization** A named organization that isolates all opamp-server entities
  from one another. For example, an Organization named "telflo-dev" would have
  separate Publications, Fleets, Groups, Variables, etc. from one named
  "telflo-prod".
- **Publication** A published OpenTelemetry Collector configuration: its YAML,
  the collector distribution and version it's written for, and who published
  it and when. A publication never changes; publishing a new version of a
  configuration creates a new publication. Each publication names the
  configuration and version it came from (for example "us-prod-east", version
  3), but those are labels supplied by the caller. This server doesn't manage
  configurations or their drafts: whatever calls the API does, such as a
  platform, a CI pipeline or a person with `curl`.
- **Fleet** A logical collection of collectors, such as all US production
  collectors. A collector joins a fleet the first time it connects, using the
  fleet's enrollment token.
- **Group** A fleet has one or more groups. For example, a fleet named
  "US-Production" might have a group named "FrontEnd" and a second group
  named "Middleware". A group is assigned the Publication its collectors run,
  and optionally a Variable Group. Every fleet starts with a "Default" group.
- **Collector** An OpenTelemetry Collector run by an OpAMP supervisor, on a
  server or in a Kubernetes cluster. It names the group it belongs to when it
  first connects, and it's sent its group's Publication. A single collector
  can be pinned to a Publication or a Variable Group of its own, which then
  takes the place of its group's.
- **Enrollment Tokens** Each fleet has two. A new collector joins with the
  fleet's default token. The server then gives each collector the fleet's
  rotate token, which it uses from then on.
- **Variables** Variables let a publication's YAML refer to a value by name,
  as `${org:NAME}`. When the YAML is sent to a collector, each reference is
  replaced with the variable's value, or with a Variable Group's override.
  For example, a variable `OBSERVABILITY_ENDPOINT` with the value
  `http://dev.otel-collector.observability.svc:4318` turns
  `endpoint: ${org:OBSERVABILITY_ENDPOINT}` into
  `endpoint: http://dev.otel-collector.observability.svc:4318`.
- **Variable Group** A set of overrides of variables' values. For example, a
  "Production" variable group can override `OBSERVABILITY_ENDPOINT` with
  `http://prod.otel-collector.observability.svc:4318`. Assigning different
  variable groups to different groups sends their collectors different YAML
  from the same publication. A collector pinned to a variable group uses its
  overrides first, then its group's variable group's, then the variables'
  own values.
- **API Key** A key for the management API that works for one or more
  organizations, used in place of the management secret.

How the entities relate. The tree's branches are ownership: deleting an
organization deletes everything under it. The arrows are assignments.

```text
Organization
├── Publications
├── Variables
├── Variable Groups ─────── override ──────▶ Variables
└── Fleets
    ├── Enrollment Tokens
    └── Groups ──────────┬── is assigned ──▶ a Publication
        │                └── is assigned ──▶ a Variable Group
        └── Collectors ──┬── may be pinned to ──▶ a Publication
                         └── may be pinned to ──▶ a Variable Group
```

API keys aren't in the tree because a key can work for several
organizations. Deleting an organization removes it from its keys.

## Quick start: Docker

Run the opamp-server on your own machine, connect an OpenTelemetry
Collector to it, and send that collector a configuration. It takes about
ten minutes.

This is for learning only, never a deployment: its passwords and keys
are development values published in this repository. To run it for real,
see [Run it standalone](docs/standalone.md) or
[Run it on Kubernetes](docs/kubernetes.md).

### Prerequisites

The steps run on macOS or Linux, in bash or zsh. They use these tools:

| Tool | Used for | Where to get it |
|---|---|---|
| Git | getting the code (step 1) | [git-scm.com](https://git-scm.com/downloads) |
| Docker, with Compose v2 | running the server and the collector | [Docker Desktop](https://docs.docker.com/get-docker/), or Docker Engine and the `docker-compose-plugin` package on Linux |
| `curl` | calling the server | included with macOS and most Linux distributions |
| `jq` 1.6 or later | reading the server's replies and writing the configuration (steps 7 to 12) | `brew install jq`, or `apt install jq` |
| `uuidgen` | making IDs (steps 5 and 10) | included with macOS; `apt install uuid-runtime` on Debian and Ubuntu |
| `sha256sum` or `shasum` | hashing the configuration (step 10) | included with Linux and macOS |

Check that they're all installed:

```sh
git --version && docker compose version && curl --version && jq --version && uuidgen
```

Each command prints a version, and `uuidgen` prints an ID. If one
answers `command not found`, install it before you start.

### Steps

Run every command below in one terminal, in order. The `#` lines are
comments that say what the next command does. In zsh (macOS's default
shell), run this first so the comments paste cleanly; bash needs nothing:

```sh
setopt interactivecomments
```

**1. Get the code.**

```sh
# Download the repository and go into it.
git clone https://github.com/telflo/telflo-opamp-server.git
cd opamp-server
```

**2. Start the server.** Docker builds the server's image from the code
(a few minutes the first time), then starts PostgreSQL and the server's
two components, each in its own container: `agent`, which collectors
connect to, and `management`, the API you use to manage them.

```sh
# Build the server's image and start every container in the background.
docker compose up -d --build
```

Two more containers, `bootstrap` and `migrate`, set up the database and
then stop: it's normal for them to show as exited.

If port 51234 or 51235 is already in use on your machine, run
`docker compose down`, start again with
`OPAMP_AGENT_HOST_PORT=<free port> OPAMP_MANAGEMENT_HOST_PORT=<free port> docker compose up -d --build`,
and use those ports in the steps below.

**3. Check that it's running.**

```sh
# List the containers and their health.
docker compose ps
```

Wait until `agent` and `management` both show `healthy`, running the
command again if needed. Then ask each one:

```sh
# Ask the management API whether it's healthy: it answers ok.
curl http://127.0.0.1:51235/health

# Ask the agent endpoint whether it's ready: it answers ready.
curl http://127.0.0.1:51234/ready
```

**4. Prepare your terminal.** The next steps save credentials in files,
so work in a folder that Git ignores and only you can open.

```sh
# Make the folder (Git ignores .quickstart), open to you alone, and go into it.
mkdir -p .quickstart && chmod 700 .quickstart && cd .quickstart

# Save the development management secret from compose.yaml as a request header.
printf 'Authorization: Bearer %s\n' opamp-compose-development-management-secret-never-for-production > auth-header

# Save the management API's address, which every later call uses.
api=http://127.0.0.1:51235/api/v1
```

**5. Create an organization.** Everything you manage belongs to an
organization.

```sh
# Make a new ID for the organization.
org=$(uuidgen)

# Create the organization, authorized by the management secret.
curl -sS -X POST -H @auth-header -H 'Content-Type: application/json' \
  --data "{\"id\": \"$org\", \"name\": \"My team\"}" "$api/organizations"
```

**6. Create an API key.** The key works only for this organization, and
the remaining steps use it in place of the management secret. It expires
in 30 days. The reply is the only time the key is shown, so it goes
straight into a file.

```sh
# Work out the date 30 days from now (the first form is macOS's, the second Linux's).
expires=$(date -u -v+30d +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '+30 days' +%Y-%m-%dT%H:%M:%SZ)

# Create a key for the organization, expiring at 'expires', and save it into 'key-header'
curl -sS -X POST -H @auth-header -H 'Content-Type: application/json' \
  --data "{\"name\": \"quick start\", \"description\": null, \"organizationIds\": [\"$org\"], \"expiresAt\": \"$expires\"}" \
  "$api/api-keys" | grep -o 'opk_[A-Za-z0-9_-]*' | sed 's/^/Authorization: Bearer /' > key-header
```

[docs/security.md](docs/security.md) explains how to hand out, revoke and
look after keys.

**7. Create a fleet.** The reply includes the fleet's Default group ID and
the enrollment token. These two items are what a collector joins with. It's saved in a file.

```sh
# Create a fleet named 'my-fleet' using the newly minted API key to authorize, and save the reply in the fleet.json file.
curl -sS -X POST -H @key-header -H 'Content-Type: application/json' \
  --data '{"label": "my-fleet", "type": "server", "rotateExpiresAt": null, "actor": null}' \
  "$api/organizations/$org/fleets" > fleet.json

# Read the fleet's ID and its Default group's ID from the reply.
fleet=$(jq -r .fleet.id fleet.json)
group=$(jq -r .defaultGroup.id fleet.json)
```

**8. Start a collector.** This builds and starts OpenTelemetry Collector
Contrib, run by the OpAMP supervisor
([deploy/quickstart-collector](deploy/quickstart-collector)). It joins
the fleet with the token, in the Default group.

```sh
# Export the fleet's enrollment token from the reply in step 7. Used in the docker image.
export OPAMP_TOKEN=$(jq -r .tokens.default.bearer fleet.json)

# Export the Default group's ID, so it joins that group. Used in the docker image
export COLLECTOR_GROUP_ID=$group

# Build and start the collector. --no-deps leaves the running server alone.
docker compose --profile collector up -d --build --no-deps collector
```

**9. Check that the collector joined.** After a few seconds:

```sh
# List the fleet's collectors, showing whether each is connected and configured.
curl -sS -H @key-header "$api/organizations/$org/fleets/$fleet/collectors" \
  | jq '.collectors[] | {name, liveness, sync}'
```

You should see `"liveness": "online"` and `"sync": "unassigned"`: the
collector is connected, but its group has no configuration yet. `sync`
says whether a collector runs what it's assigned; step 12 shows it
become `inSync`.

**10. Publish a configuration.** Write a collector configuration to a
file, then publish it as version 1. A published version can't be changed.

```sh
# Write a collector configuration: receive traces over OTLP and log them.
cat > collector.yaml <<'EOF'
receivers:
  otlp:
    protocols:
      http:
        endpoint: 0.0.0.0:4318
exporters:
  debug: {}
service:
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [debug]
EOF

# Build the publication's request body: the YAML, its SHA-256 hash, new IDs
# for the configuration and its version, and the collector it's written for.
jq -n --rawfile yaml collector.yaml \
  --arg sha256 "$( (sha256sum 2>/dev/null || shasum -a 256) < collector.yaml | cut -d' ' -f1)" \
  --arg configuration "$(uuidgen)" --arg version "$(uuidgen)" \
  '{configuration: {id: $configuration, name: "quick-start"},
    version: {id: $version, number: 1, label: null},
    yaml: $yaml, sha256: $sha256, publishedBy: null, publishedAt: (now | todate),
    collector: {distribution: "otelcol-contrib", version: "0.162.0"}}' > publication.json

# Publish it as version 1. The server gives the publication an ID; save the reply.
curl -sS -X POST -H @key-header -H 'Content-Type: application/json' \
  --data-binary @publication.json "$api/organizations/$org/publications" > published.json

# Read the publication's ID from the reply.
publication=$(jq -r .id published.json)
```

**11. Assign it to the group.** Every collector in the group is sent the
configuration on its next poll.

```sh
# Assign the published version to the Default group.
curl -sS -X PATCH -H @key-header -H 'Content-Type: application/json' \
  --data "{\"assignedPublicationId\": \"$publication\"}" \
  "$api/organizations/$org/fleets/$fleet/groups/$group"
```

**12. Check that the collector applied it.** Within a minute:

```sh
# List the collectors again, now also showing whether each applied the configuration.
curl -sS -H @key-header "$api/organizations/$org/fleets/$fleet/collectors" \
  | jq '.collectors[] | {name, liveness, sync, effective: .effective.status}'
```

You should see `"sync": "inSync"` and `"effective": "applied"`.
`docker compose logs collector` shows the collector restarting with the
new configuration.

**13. Stop one component (optional).** The two components run
independently. Stop the management API, and the collector keeps
polling the agent endpoint:

```sh
# Stop the management API's container.
docker compose stop management

# Ask the agent endpoint whether it's ready: it still answers ready.
curl http://127.0.0.1:51234/ready

# Start the management API again.
docker compose start management
```

Once `docker compose ps` shows `management` as `healthy` again, step 12's
command shows the collector still `online`: it never stopped polling.

**14. Clean up.** This stops every container, deletes the data and the
images, and deletes the repository you cloned in step 1, with the
credentials in `.quickstart`. You can run it after any step, then start
again from step 1. If the repository is a checkout you work in, run
`rm -rf .quickstart` in place of the last command.

```sh
# Go back to the repository's folder (skip this before step 4, when you're already there).
cd ..

# Stop and remove every container, including the collector, delete their data,
# and remove their images (opamp-server:dev, opamp-quickstart-collector:dev, postgres:18).
docker compose --profile collector down -v --rmi all

# Leave the repository and delete it.
cd .. && rm -rf opamp-server
```

**Connecting a collector of your own.** Any OpenTelemetry supervisor can
join the same way:
[deploy/quickstart-collector/supervisor.yaml](deploy/quickstart-collector/supervisor.yaml)
is a complete supervisor configuration. The server tells each collector
which address to poll (`OPAMP_SERVER_URL`). In the quick start, that's an
address only containers can reach, so for a collector running directly on
your machine, start the server in step 2 with
`OPAMP_SERVER_URL=http://localhost:51234/opamp/api/v1 docker compose up -d --build`.

## What it doesn't do

- **No web interface, not yet: coming soon.** Until then,
  everything goes through the management API, and a platform or your own
  tooling builds the screens.
- **No user accounts.** Callers hold the management secret or an API key
  for some organizations. There's no sign-in, single sign-on or roles.
- **No collector upgrades, not yet.** OpAMP can upgrade a collector by
  offering it a package, and the upstream Supervisor has begun to support
  that, but opamp-server doesn't offer packages yet. Upgrade collectors
  the way you installed them: with your package manager, or a new image
  tag.
- **Configurations and connection settings only.** opamp-server sends
  collectors their configuration and the settings they connect with. It
  doesn't yet send the restart command, or tell collectors where to send
  their own telemetry.
- **HTTP polling only.** Collectors poll over plain HTTP or HTTPS. OpAMP's
  WebSocket transport isn't served.
- **No OpenTelemetry Operator bridge.** It serves a supervisor on a host or
  in a cluster, not the Operator's bridge.
- **It doesn't install collectors.** You run the Supervisor and the
  collector your own way; the [Quick start](#quick-start-docker) shows one.
- **The Helm chart renders no Ingress, autoscaler or Secret.** You bring
  those, so they fit your cluster
  ([charts/telflo-opamp-server/README.md](charts/telflo-opamp-server/README.md)).

## Compatibility

| | Works with | Tested with |
|---|---|---|
| PostgreSQL | 16 or later | 18 |
| OpenTelemetry Collector | a distribution with the `opamp` extension, run by the upstream Supervisor | 0.162.0, `otelcol-contrib` |
| Kubernetes, for the Helm chart | 1.25 or later | minikube |
| Helm | 3 or later | |
| Release binaries | Linux and macOS, amd64 and arm64 | |
| Image | `linux/amd64` and `linux/arm64`, distroless, non-root | |
| Building from source | Go 1.27.3 (`go.mod`) | |

## Settings

Every setting is an environment variable. With Helm, the chart sets them
from its Secrets and values. These are the ones you must set, and the
commands that read them:

| Variable | Read by |
|---|---|
| [`OPAMP_DATABASE_URL`](docs/configuration.md#opamp_database_url) | `serve`, `serve agent`, `serve management` |
| [`OPAMP_MIGRATION_DATABASE_URL`](docs/configuration.md#opamp_migration_database_url) | `migrate` |
| [`OPAMP_MANAGEMENT_SECRET`](docs/configuration.md#opamp_management_secret) | `serve`, `serve management` |
| [`OPAMP_ENCRYPTION_KEY_FILE` or `OPAMP_ENCRYPTION_KEY`](docs/configuration.md#opamp_encryption_key_file-or-opamp_encryption_key) | `serve`, `serve agent`, `serve management` |
| [`OPAMP_SERVER_URL`](docs/configuration.md#opamp_server_url) | `serve`, `serve agent` |

[docs/configuration.md](docs/configuration.md) explains each, and lists
every optional setting: TLS, ports, rate limits, shutdown timing and
telemetry.

## Troubleshooting

**`serve` exits at once with `Refusing to start: …`.** It checks its
settings, its database role and the schema before it opens a port, and
the line says what's wrong and what to set. The common ones:

- `the schema is at 12, this build needs 13`: run `opamp-server migrate up`.
- `the schema is at 14, which is newer than this build`: run the release
  that migrated it, or a later one.
- `the database role "telflo_opamp_owner" is …`: `OPAMP_DATABASE_URL`
  names the owner. `serve` connects as `telflo_opamp`, and only `migrate`
  connects as the owner ([docs/database.md](docs/database.md)).
- `can't listen on the management port … address already in use`: another
  `serve`, or a `kubectl port-forward`, already has the port.

**`curl: (7) Failed to connect to 127.0.0.1 port 51235`.** Nothing is
listening there. Each `serve` runs in its own terminal until Ctrl-C:
check that terminal for a `Refusing to start` line. On Kubernetes,
`kubectl port-forward` also runs until Ctrl-C, so run `curl` in a second
terminal.

**A collector shows `online` once, then drops to `unknown`.** After its
first poll, the server hands it `OPAMP_SERVER_URL` (`server.publicUrl` in
the chart), and the collector polls that address from then on. Set it to
an address your collectors reach, then restart them
([docs/agent-endpoint.md](docs/agent-endpoint.md)).

**A collector never appears.** Its Supervisor's log says why:

- `401`: the token is wrong, revoked or expired.
- `403`: it names no group (the `telflo.collector_group_id` attribute), or
  its organization is at its collector limit.
- `429`: it's over its organization's or its own request rate, and it
  retries after the `Retry-After` the server sent.

**The Supervisor logs `"level":"error"` lines as it starts.** At startup,
the Supervisor runs the collector once, briefly, to learn what it is, and
relays that run's output at `error` level. Each message inside is the
collector's own `info` line. One `Post … context canceled` from the same
restart is expected too.

**A collector's `sync` stays `pending`, or is `failed`.** `pending` means
it hasn't been sent its configuration yet, or hasn't reported back since:
give it a poll or two. `failed` means the collector couldn't apply it,
usually an invalid configuration. `GET …/collectors/{collectorId}/errors`
lists what it reported ([docs/management-api.md](docs/management-api.md),
[docs/STATE_MODEL.md](docs/STATE_MODEL.md)).

**A database holding the old imports table** (`telflo_opamp.imports`)
can't be upgraded. `migrate up` and `serve` refuse it and point here.
It's a development database from before the first release. Drop the
`telflo_opamp` database, then run the bootstrap and `migrate up` again
([docs/database.md](docs/database.md)); everything in it is lost.

## Documentation

**Run it**

- [Quick start](#quick-start-docker): Docker Compose, in about ten minutes
- [docs/standalone.md](docs/standalone.md): on one machine, from the
  release binary, one part at a time
- [docs/postgres-in-docker.md](docs/postgres-in-docker.md): PostgreSQL in a
  container, set up for opamp-server, with backups and restores
- [docs/kubernetes.md](docs/kubernetes.md) and the
  [chart's README](charts/telflo-opamp-server/README.md): the Helm chart, every
  value, and the Quick start on minikube
- [docs/upgrading.md](docs/upgrading.md) and [CHANGELOG.md](CHANGELOG.md):
  what each release changed, and what an upgrade needs from you

**Use it**

- [docs/management-api.md](docs/management-api.md): the routes and their
  answers, with a worked example
- [api/openapi.yaml](api/openapi.yaml): the API's contract, OpenAPI 3.1
- [docs/agent-endpoint.md](docs/agent-endpoint.md): what collectors see:
  readiness, replicas, per-pod limits, tokens and refusals

**Operate it**

- [docs/configuration.md](docs/configuration.md): every setting,
  connection pool sizing, startup checks, shutdown, logs and health checks
- [docs/database.md](docs/database.md): the roles and database the
  bootstrap creates, managed PostgreSQL, and `migrate`
- [docs/security.md](docs/security.md): the management secret, API keys,
  the encryption key, rotating each, and TLS

**Understand it**

- [docs/STATE_MODEL.md](docs/STATE_MODEL.md): what the server records
  about each collector, and how liveness and sync are worked out
- [docs/decisions/](docs/decisions/): why it's built the way it is

## Community and help

- **A question, or something not working:** check
  [Troubleshooting](#troubleshooting) first, then
  [open an issue →](https://github.com/telflo/telflo-opamp-server/issues/new/choose).
  The bug report form asks for what helps most: the version, how you run
  it, and the logs.
- **A feature you need:** open an issue with the feature request form.
- **A security problem:** never in an issue. [SECURITY.md](SECURITY.md)
  says how to report it privately.
- **Want to help build it?** Read [Contributing](#contributing), below.

## Contributing

[`CONTRIBUTING.md`](CONTRIBUTING.md) has the rules for changing the code,
and [docs/testing.md](docs/testing.md) explains how to run the tests
(`make check`). Everyone taking part follows the
[code of conduct](CODE_OF_CONDUCT.md).

## Acknowledgements

opamp-server is built on the work of the
[OpenTelemetry](https://opentelemetry.io/) project: the OpAMP
specification, [opamp-go](https://github.com/open-telemetry/opamp-go), and
the Collector and Supervisor it manages. It stores everything with
[pgx](https://github.com/jackc/pgx) and migrates with
[goose](https://github.com/pressly/goose).

## About Telflo

opamp-server is an open-source project of [Telflo, Inc.](https://telflo.com)
Telflo also offers a hosted service and an air-gapped edition. Those are
separate products: opamp-server is complete on its own, and needs neither.

## Licence

The Apache License 2.0: [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE).
opamp-server is provided "as is", without warranty of any kind; sections 7
and 8 of the licence say so in full. The Telflo name and logo are
trademarks of Telflo, Inc., and the licence grants no rights to them.

---

<p align="center">
  <a href="#quick-start-docker">Quick start</a> ·
  <a href="docs/standalone.md">Standalone</a> ·
  <a href="docs/kubernetes.md">Kubernetes</a> ·
  <a href="docs/management-api.md">Management API</a> ·
  <a href="CHANGELOG.md">Changelog</a> ·
  <a href="SECURITY.md">Security</a>
</p>
<p align="center">
  <sub>An open-source project by <a href="https://telflo.com">Telflo, Inc.</a> Built on <a href="https://opentelemetry.io/">OpenTelemetry</a>'s OpAMP. Apache License 2.0.</sub>
</p>
