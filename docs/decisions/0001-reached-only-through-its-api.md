# 0001. The server is reached only through its API

**Status:** Accepted
**Date:** 2026-10-03 (written for this repository on 2026-10-25)

## Context

The opamp-server stores collector configurations and the fleets that run
them for many organizations at once. Something has to put the
configurations there and manage the fleets: a platform built on it, an
operator's own tooling, or a person with `curl`.

The codebase this server came from shows where shared database objects
lead. Its server and its web frontend met in one database through views,
stored procedures and foreign keys, and the frontend wrote the server's
tables directly. One of its functions trusted an organization's own row
naming the version to serve: since a foreign key ignores row-level
security, an organization could point at another organization's version and
have its collectors served that configuration.

A server that anyone can run needs one interface that every caller uses.
A caller that reaches the server some other way keeps a second interface
that the server's own tests can't see.

## Decision

**Nothing shares a database object with the opamp-server. Every caller
reaches it only through its management API**, and every caller uses the
same API.

- **Its own database, schema and roles.** The database is `telflo_opamp`
  (any name may be passed to the bootstrap), on whatever Postgres server
  the operator chooses, beside other databases or alone. The schema is
  `telflo_opamp`, named in every statement. `telflo_opamp_owner` owns the
  database and everything in it and runs the migrations;
  `telflo_opamp_runtime` (`NOLOGIN`) holds exactly the privileges the
  server needs; the server logs in as `telflo_opamp`, a member of it with
  no DDL rights. An idempotent `deploy/bootstrap.sql`, run by a superuser,
  creates the database and the roles.
- **`CONNECT` is revoked from `PUBLIC`** on the database, and granted back
  only to its two login roles, so no other role on the same Postgres
  server can connect to it. A foreign key, join or query from another
  database is impossible, not just tested for. `TEMPORARY` is revoked from
  `PUBLIC` too.
- **Migrations are a separate step.** `opamp-server migrate up|down|status`
  runs goose with SQL embedded in the binary, as the owner, before the
  server starts. `serve` refuses to start when the schema is behind or
  ahead of the binary, or when its role is a superuser, has `BYPASSRLS`,
  owns an object in the schema or is a member of `telflo_opamp_owner`.
  Every migration keeps working with the previous release (expand, then
  contract).
- **The organization is an opaque tenant key**: `organization_id uuid not
  null` on every row, with no foreign key out of the schema. A caller
  registers each organization through the API under its own id, with a
  display name, and the server never interprets either: it has no members,
  people or accounts. Every management route sits under
  `/api/v1/organizations/{organizationId}/…`, and every query filters on
  it.
- **The management API has its own port and a shared secret.**
  `OPAMP_MANAGEMENT_PORT` (default 51235) is never exposed to the
  internet. Callers send `Authorization: Bearer <secret>`; the server
  compares the SHA-256 of what was sent, in constant time, with that of
  `OPAMP_MANAGEMENT_SECRET` (at least 32 bytes) and of each of
  `OPAMP_MANAGEMENT_SECRETS_PREVIOUS`, which makes a rotation a rolling
  restart, not an outage. Neither the header nor the secret is ever
  logged. *Amended by `0003-api-keys.md`: API keys, each limited to its
  organizations, authenticate beside the secret.*

## Alternatives rejected

- **Shared tables and designated views** for a caller to read. The views
  would be a second interface, used by one caller only, beside the API
  everyone else gets. Coupling through views, stored procedures and
  foreign keys is what let the earlier codebase's cross-tenant read
  happen.
- **A schema of its own in a caller's database.** A separate schema still
  lets a grant, a join or a foreign key cross the boundary, and only a
  test would notice. A separate database with `CONNECT` revoked makes the
  crossing impossible.
- **Signed short-lived tokens** (an Ed25519-signed JWT per call). The
  shared secret is simpler to run, and its cost is recorded here: it is
  one string that opens every organization until it is rotated. Keeping it
  inside the callers' own networks, never exposing the port, and never
  giving it to the callers' own users is what bounds that.
- **Migrating at startup.** The running server would then need the
  owner's rights, and a bad migration would crash-loop the process every
  collector polls. A separate step, plus `serve`'s startup version check,
  catches a mismatch without that risk. The cost is two commands instead
  of one.
- **Plain `telflo` as the schema's name.** `telflo.publications` reads as
  some other product's data, and could collide with a caller that keeps
  its own tables in a schema of that name in a shared database.

## Consequences

- Every write from outside is an API call: organizations, published
  versions, fleets, collector groups and their assignments, collectors,
  enrollment tokens, limits and organization variables
  (`api/openapi.yaml`).
- No transaction spans the server and a caller. A caller that keeps its
  own records orders its writes so that a failure leaves at most a
  harmless orphan (a published version nothing names, say), never a record
  pointing at nothing.
- The API is the only path, so `api/openapi.yaml` is the contract: a Go
  test sends every documented request through the real handler and
  validates each request and reply against it, and callers can test their
  fakes against the same file.
- The server is relocatable: another server is a URL and a secret.
- The management listener speaks plain HTTP unless it is given a
  certificate and its key (`OPAMP_MANAGEMENT_TLS_CERT_FILE` and
  `OPAMP_MANAGEMENT_TLS_KEY_FILE`, TLS 1.2 or later), so without them the
  port must stay on a private network.
- `serve` refuses a schema newer than its build, so the upgrade procedure
  is a backup before every `migrate up`, and rolling a release back means
  restoring it.
- A change to the API that the previous release's callers can't send, or
  that the previous release can't answer, needs a new API version once a
  caller other than the server's own maintainers depends on it.
- Collectors don't use this API: they poll the agent endpoint, its own
  port behind its own load balancer
  (`0002-the-agent-tier.md`).
