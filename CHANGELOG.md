# Changelog

Every release, newest first. **Breaking** marks a change that needs you to
act when you upgrade; [docs/upgrading.md](docs/upgrading.md) says what to
do. Each release's binaries, image and chart are on its
[GitHub release](https://github.com/telflo/opamp-server/releases).

## Unreleased

- **Breaking.** The database, its schema and its roles are renamed, so
  they name this server and leave room for other Telflo databases beside
  it: the database is `telflo_opamp_server`, the schema `opamp_server`,
  and the roles `telflo_opamp_server_owner`, `telflo_opamp_server_runtime`
  and `telflo_opamp_server`. The chart's `postgres.database` and
  `externalDatabase.database` default to the new name. A 0.2.4 database
  isn't renamed in place: [docs/upgrading.md](docs/upgrading.md).

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
