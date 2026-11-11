# Changelog

Every release, newest first. **Breaking** marks a change that needs you to
act when you upgrade; [docs/upgrading.md](docs/upgrading.md) says what to
do. Each release's binaries, image and chart are on its
[GitHub release](https://github.com/telflo/telflo-opamp-server/releases).

## Unreleased

- **Breaking.** The repository is now `telflo/telflo-opamp-server`, and
  what is published from it follows the new name: the image is
  `ghcr.io/telflo/telflo-opamp-server`, the chart
  `oci://ghcr.io/telflo/charts/telflo-opamp-server`, and the Go module
  `github.com/telflo/telflo-opamp-server`. The chart's default resource
  names are now `<release>-telflo-opamp-server-…`, and a parent chart sets
  its values under `telflo-opamp-server:`. The binary, its settings and the
  database keep their names. 0.2.4 stays published under the old names.

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
