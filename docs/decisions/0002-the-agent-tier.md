# 0002. The agent tier: its own port, stateless replicas, two token kinds, isolation by composite keys

**Status:** Accepted
**Date:** 2026-10-12 (written for this repository on 2026-10-25, with its
later changes folded in: importing removed on 2026-10-14, a new default
token after a revoke on 2026-10-16, and its own process on 2026-11-01)

## Context

`0001-reached-only-through-its-api.md` gives the server its own database
and a management API on a private port. Collectors reach the server
another way: through the OpenTelemetry supervisor, they poll for their
configuration with an enrollment token, from wherever they run, the
internet included.

The codebase this server came from ran its agent side as one process, on
one port, with every concern inside it:

- **One listener**, TLS from two files or refused unless turned off, with
  pprof and an admission view on the same port behind a token of their own,
  CORS answering `*`, no graceful shutdown, and logs to a file.
- **Process-local state as truth**: a token row cached 30 s (a revocation
  lagged that long per process), a configuration cache keyed by its
  SHA-256 alone and answered before the organization's check (one
  organization could be served another's bytes), a miss cached as an empty
  file and pushed, unbounded maps (the rate limiter's, a group-binding
  log's), bodies read whole before any check.
- **Two token kinds.** A fleet's default token (it never expires) enrolls
  a collector, which is handed the fleet's rotate token in its connection
  settings; a rotation gives the old rotate token a grace end and pushes
  the new one. The bearer is `base64(nonce ‖ AES-256-GCM(SHA-256(key),
  {"token_id","deployment_id"}) ‖ tag)`. Authentication put the row's
  organization in the context but **the bearer's fleet**, never comparing
  the two.
- **Imports into another product's tables.** A collector with nothing
  assigned had its own configuration copied out and assigned to its fleet's
  Default group: the first collector's configuration was pushed onto every
  other.
- **Isolation by trigger and row-level security.** A collector's fleet's
  organization was checked by a trigger; an assigned version's wasn't, so a
  group could be assigned another organization's.

The scale to plan for: fleets of tens of thousands of collectors, about
670 polls a second at 20,000 collectors polling every 30 s, deployed on
Kubernetes and rolled without an outage.

## Decision

**The agent tier is its own port behind its own load balancer, served by
stateless replicas whose only truth is the database. It authenticates two
token kinds with a key only the opamp-server holds, stores none of a
collector's own configuration, and isolates organizations by composite
keys that carry the organization through every link, without row-level
security.**

### Its own port and its own load balancer

- **Its own process, when deployed apart.** `serve agent` runs the agent
  endpoint alone and `serve management` the management API alone, on the
  same database; `serve` runs both in one process. Apart, each is sized,
  scaled, rolled and stopped on its own, and the agent tier, which faces
  the collectors' networks, never holds the management secret (it still
  holds the encryption key, to read bearers). Readiness is the process's,
  answered on each component's port; the error history's cleanup runs with
  the agent endpoint; the diagnostics, which `serve agent` has no secret to
  put behind, are on a loopback-only listener of their own there. The two
  run the same release: each refuses a schema newer than its build. The
  Helm chart runs them as two Deployments.
- `serve` opens **two listeners**: the management API on
  `OPAMP_MANAGEMENT_PORT` (51235, private, behind the secret) and the agent
  endpoint on `OPAMP_AGENT_PORT` (**51234**), each with its own optional
  TLS (`OPAMP_AGENT_TLS_CERT_FILE` and `_KEY_FILE`, both or neither, TLS 1.2
  or later). Without TLS the agent listener logs once that a
  TLS-terminating load balancer is expected in front.
- **Never a web gateway.** A gateway shared with a web application couples
  deploys, failures and abuse: a web deploy would touch every collector,
  and the agent path, which anyone can reach, would share workers with
  people. Production puts the agent port behind its own load balancer or
  ingress; an on-premises operator may still front both with one proxy.
- **`OPAMP_SERVER_URL` is required**: the agent endpoint's public URL,
  pushed to every collector in its connection settings, where it polls
  from then on. An `http(s)` URL with a host, a port from 1 to 65535 if
  any, and no user name, password, query or fragment; `serve` warns once
  when its host is a loopback address or its path isn't `/opamp/api/v1`.
- The agent port answers **`POST /opamp/api/v1`, `GET /health`** (liveness,
  no database) **and `GET /ready`** (the database answers a ping within
  2 s, on a connection of its own, and the server isn't stopping; one ping
  in flight at a time, shared by its callers, its answer kept a second, so
  a flood on the public port can't queue the kubelet's probe out), and
  `404` to anything else, logged nowhere. pprof and the admission view are
  on the management listener under `/api/v1/debug/`, behind the secret;
  there is no CORS.
- **Graceful stop.** Told to stop, a pod answers `/ready` `503` at once and
  keeps serving for `OPAMP_SHUTDOWN_DRAIN_SECONDS` (5 s) before its
  listeners close, so the load balancer's readiness checks see it leave (a
  `preStop` sleep may do the waiting instead, the drain then 0); then both
  listeners finish their requests within 10 s, the cleanup job stops, the
  pools close, and the keys are destroyed and memguard purges its buffers.
  The keys are destroyed on every way out.

### Stateless replicas, caches with stated staleness

- **Several replicas from the start.** A Kubernetes Deployment's pods are
  replicas: any pod may get any poll, and a rolling update replaces them
  one at a time. A replica keeps nothing the database doesn't hold but its
  caches and its limiters' counts; a test runs two replicas on one
  database (enroll through one, poll through the other, a revocation
  refused by both).
- **Caches only for speed, each with a staleness bound**, least recently
  used, bounded, on the injected clock: a token row by its id, joined with
  its fleet's type and status, **≤ 10 s** from the time its read began
  (found rows only; a slower, older read never overwrites a newer one;
  20,000 rows), so a revocation is refused by every replica within 10 s;
  an organization's `requestsPerMinute` ≤ 10 s (10,000 entries); a
  configuration by **organization and SHA-256** (64 MiB in all, none over
  4 MiB, a miss never cached, an empty configuration never pushed); an
  organization's variables by the organization, its generation, its
  variables counter and the collector's two variable groups, exact because
  every write that can change a filled-in text moves the counter
  (`docs/STATE_MODEL.md`).
- **Limits counted per pod**, documented as "the limit × the number of
  pods": an organization's requests a minute (its own `requestsPerMinute`,
  else `OPAMP_DEFAULT_REQUESTS_PER_MINUTE`, 12,000); a per-collector guard
  (`OPAMP_COLLECTOR_REQUESTS_PER_MINUTE`, 60, a burst of 20; at most 20,000
  tracked collectors per organization per pod, a ceiling); the pod's
  concurrency cap (`OPAMP_MAX_CONCURRENT_REQUESTS`, 100) and an
  organization's share of it
  (`OPAMP_MAX_CONCURRENT_REQUESTS_PER_ORGANIZATION`, 25). A fleet-wide or
  cross-pod limit is the load balancer's. Every map is bounded and swept.
- **A request's cost is bounded before it is trusted.** The stack checks,
  in order: a declared length over 4 MiB (`413`, nothing read), the pod's
  slots, the credential (the `Authorization` header only), the
  organization's share of the slots, the collector's own guard by the
  `OpAMP-Instance-Uid` header the supervisor sends (so a collector polling
  in a loop is refused before it spends its organization's rate; the
  body's instance uid must then be the header's; without the header the
  handler checks the guard once the body is read), the organization's
  rate, and only then reads the body, capped at 4 MiB (after decompression
  too) within 5 s, and decodes it only if it holds at most 100,000 fields
  nested at most 64 deep (decoding multiplies structure: 4 MiB of empty
  attributes decoded to 160 MB); the database work has its own 5 s. An unauthenticated
  caller never has its body read, and one organization's credential can
  hold at most its share of a pod's slots, each for at most 5 s reading a
  body and at most 30 s (the listener's write timeout) for a client that
  doesn't read its answer.

### Both token kinds, and the key only the opamp-server holds

- **Default and rotate tokens.** A fleet is created, in one transaction,
  with its Default group, a default token (it never expires) and a rotate
  token, and both bearers are in the reply. A default token's poll enrolls
  the collector and pushes the fleet's current rotate token in
  `connection_settings.opamp`. A rotation gives the current rotate token
  `expires_at = least(its own, the grace end)` and `do_refresh`, makes a
  new one, and pushes it to every collector that still authenticates with
  the old (a poll on a token with `do_refresh` set, in its grace, is pushed
  the fleet's newest rotate token); a
  revocation sets `revoked_at`. A rotation reaches only collectors whose
  token still authenticates: a revoked or expired rotate token strands the
  collectors holding it (refused `401`, they keep the refused bearer) until
  their supervisors restart and come back through the default token. So a
  rotate token is rotated before it expires, and revoked only knowing
  that.
- **Why two kinds.** The rotate token changes and expires without touching
  a host. The supervisor doesn't keep a pushed token across a restart, so
  the never-expiring default token in every install command is the durable
  secret, and revoking it is what locks out a leaked install command.
- **A new default token after a revoke.** Revoking a fleet's default token
  is allowed, and its cost is written down: a collector that restarts
  presents the default token again and, refused, stays offline.
  `POST …/fleets/{f}/tokens/default` issues a new default token, with its
  `issued` event, only while the fleet has no valid one (`409
  enrollment_token_default_exists` otherwise, also for the loser of two
  racing issues; `409 fleet_archived`). The revoked token stays revoked: a
  collector stranded by it comes back when its install command is run
  again with the new token.
- **The bearer's format is the earlier codebase's exactly** (its JSON keeps
  `deployment_id`), so a bearer it made decrypts with the same key. The
  **management API mints every bearer**; the opamp-server alone holds
  `OPAMP_ENCRYPTION_KEY` (and up to 8 previous keys for a rotation of the
  key), as a value or, preferably, a file. Each key is used exactly as
  written (no whitespace at either end), its SHA-256 the AES key, held in
  memguard's locked memory; bearers are never stored, and a reveal
  encrypts afresh and is audited.
- **A token authenticates** when its row exists, **the bearer's fleet
  equals the row's**, it isn't revoked or past its expiry, and its fleet is
  active and server-class (`server` or `kubernetes-supervisor`; the
  OpenTelemetry Operator's bridge is not served). The principal's
  organization, fleet, kind and `do_refresh` all come from the row.
- **Audit rows** (`issued`, `viewed`, `rotated`, `revoked`) carry ids and
  the actor's id and email, never a bearer, a label or configuration text.
  Every reply carrying a bearer is `Cache-Control: no-store`.

### Nothing of a collector's own configuration is stored

- A collector that nothing resolves for (no pin, no published version on
  its group) is **sent no configuration**: an acknowledgement asking for
  its full state at most once per 3 minutes (and, in its token's grace, the
  fleet's rotate token). It keeps running what it has (its own file, or the
  last published version it was sent), and its sync is `unassigned`.
  Nothing reads its reported effective configuration.
- **A pin's shape leaves room**: `{kind: "publication", id}` or `null`, so
  a later kind can be added without changing what callers send today; any
  other kind is refused as an invalid request.

### Isolation without row-level security

- **One opamp-server serves every organization**, so isolation between
  them is a real boundary. **Every link between rows carries
  `organization_id` in a composite foreign key** (a group to its fleet and
  its published version, a collector to its fleet, its group and its pins,
  a token and its events to the fleet), so a link across organizations, or
  a collector's group of another fleet, can't be stored. **Every table also
  has a direct `(organization_id) → organizations(id) on delete cascade`**,
  so deleting an organization removes everything in one statement in any
  trigger order, while deleting a published version a group or a collector
  uses is refused (`409 publication_in_use`). An organization's or a
  fleet's delete first locks its collectors (in id order), so it waits for
  a poll that changes a collector's token rather than deadlocking with it,
  and runs again on a `40P01`, at most three times.
- **Every query filters on the organization first**, with the two
  exceptions `CONTRIBUTING.md` names, each by its nature: the
  authentication's `Lookup` reads a token by its id alone (the bearer names
  it; the row then gives the organization), and the error history's
  `Cleanup` deletes across organizations. Every index on an organization's
  rows leads with it; the primary keys are on `(id)`. A test calls every
  store method and every management route with another organization's ids
  (a plain `404`, nothing changed).
- **No row-level security**, so no per-transaction setting on the hot
  path. Policies can be added later without changing the keys.

## Alternatives rejected

- **The agent endpoint behind a web gateway.** One origin and one
  certificate are a web application's concerns; for collectors a shared
  gateway would couple a web deploy to every collector and share its
  workers with an unauthenticated path.
- **One process, as the earlier codebase ran it.** A single replica can't
  be rolled without an outage for every collector and can't scale out; its
  caches were truth.
- **One minimal token per fleet.** Simpler, but the default-to-rotate
  hand-off is what lets an install command carry a credential that never
  grants configuration, and a rotation needs two.
- **Importing a collector's own configuration** when nothing is assigned
  to it. Kept anywhere, an import is a second source of configuration
  beside the published versions, and the first collector's configuration
  pushed onto every other is what the earlier codebase did. Importing
  again, with rules of its own, is a decision of its own.
- **Row-level security in `opamp_server`.** The policy itself is cheap,
  but every path (jobs, tests, pooled connections) would set the
  organization first on every transaction. Composite keys make a
  cross-organization link unstorable, which a policy alone wouldn't;
  performance came first.
- **A counter for the collector cap.** A counter drifts; the cap is
  counted under the organization's lock at each enrollment.

## Consequences

- An operator runs and scales the agent tier apart from anything else: its
  own load balancer, its own TLS or a TLS-terminating one, readiness on
  the agent port's `/ready`, and a pool per replica of `pool_max_conns` + 1
  (the readiness ping's), so Postgres's `max_connections` must hold
  replicas × (`pool_max_conns` + 1). `docs/configuration.md` gives the settings
  and the arithmetic.
- Every bound is per pod: an organization's rate across a cluster is its
  limit × the pods, and so is its share of concurrent requests. A
  fleet-wide count would be a decision of its own.
- The `OPAMP_SERVER_URL` handed to collectors must be reachable from where
  they run: a collector pushed one it can't reach goes silent until it
  restarts. Each install sets it.
- A revocation takes effect within 10 s on every replica, not at once; a
  new organization rate within 10 s; a new collector cap at the next
  enrollment.
- The fleet model's writes come only through the management API (`0001`).
- Row-level security, a cross-pod rate limit and importing remain open;
  each is a decision of its own.
