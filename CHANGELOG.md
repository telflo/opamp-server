# Changelog

Every release, newest first. **Breaking** marks a change that needs you to
act when you upgrade; [docs/upgrading.md](docs/upgrading.md) says what to
do. Each release's binaries, image and chart are on its
[GitHub release](https://github.com/telflo/opamp-server/releases).

## 0.3.2 (2026-11-27)

- `migrate` no longer hangs on a database that takes its connection but
  never answers. Its connection gives up after 10 seconds (the URL's
  `connect_timeout` when it sets one), and each attempt, the checks after
  the connection included, after 30. While the database can't be reached
  or isn't ready, it tries again, waiting from 1 second up to 10 between
  attempts, for up to 2 minutes, logging each attempt, then exits 1. A
  failed login or a refusal still ends it at once.
- The chart's `wait-for-postgres` init container runs with an external
  Postgres too, against `externalDatabase.host` and `port`, before
  `migrate`, in both Deployments. With `externalDatabase.urlKeys` the chart
  doesn't know the host and there's none: `migrate`'s own retries are what
  waits. It runs `postgres.image`, so a chart that mirrors images needs that
  image with an external Postgres too. Each try's line is now in its log.

## 0.3.1 (2026-11-25)

- golang.org/x/net is 0.63.0, with golang.org/x/crypto 0.59.0,
  golang.org/x/sync 0.25.0, golang.org/x/sys 0.50.0 and golang.org/x/text
  0.44.0. No schema change.

## 0.3.0 (2026-11-15)

- **Breaking.** The database, its schema and its roles are renamed, so
  they name this server and leave room for other Telflo databases beside
  it: the database is `telflo_opamp_server`, the schema `opamp_server`,
  and the roles `telflo_opamp_server_owner`, `telflo_opamp_server_runtime`
  and `telflo_opamp_server`. The chart's `postgres.database` and
  `externalDatabase.database` default to the new name. A 0.2.4 database
  isn't renamed in place: [docs/upgrading.md](docs/upgrading.md).
- Built with Go 1.27.4, which fixes a `net/http` vulnerability in Go
  1.27.3 (GO-2026-6859).
- pgx, the PostgreSQL driver, is 5.13.0.

## 0.2.4 (2026-11-07)

The first public release of opamp-server, with its binaries, image and
Helm chart.

- `serve` refuses `compose.yaml`'s committed management secret and
  encryption key unless `OPAMP_ALLOW_DEVELOPMENT_SECRETS` is `true`, which
  only `compose.yaml` sets.
- The agent endpoint answers headers over 16 KiB `431`, and holds at most
  `OPAMP_AGENT_MAX_CONNECTIONS` connections open (10,000 by default).
- [docs/security.md](docs/security.md) says what a fleet's collectors and
  an API key's holder can do, and how to answer a leaked encryption key or
  bearer.
