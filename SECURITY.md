# Security policy

## Supported versions

Before 1.0, only the latest release gets security fixes. A fix ships as a
new release; upgrade to it ([docs/upgrading.md](docs/upgrading.md)).

## Reporting a vulnerability

**Don't open a public issue.** Report it privately through GitHub: this
repository's **Security** tab → **Report a vulnerability**. Only the
maintainers see the report.

Include what you can of:

- the release or commit, and how you run it (Docker, binary, Helm chart);
- which part is affected: the agent endpoint, the management API, the
  chart, the image or the release artefacts;
- the steps to reproduce it, and what an attacker gains.

You'll get an acknowledgement, then updates until it's fixed or declined.
Once a fix is released, it's published as a GitHub security advisory,
crediting you unless you'd rather not be named.

## Hardening

[docs/security.md](docs/security.md) covers running opamp-server safely:
who holds the management secret, API keys, the encryption key, rotating
each, and TLS.
