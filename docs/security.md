# Secrets, keys and TLS

## The management secret

The management secret is an administrator credential for every
organization on this server: with it, a caller can reveal or mint a bearer
for any fleet's collectors, and so be served any configuration. Treat it as
you would the database's superuser password:

- Give it only to whatever calls the management API on everyone's behalf:
  a platform built on opamp-server, or your own tooling. Give a person or
  tool that needs only some organizations an [API key](#api-keys) instead.
- Generate it with `openssl rand -base64 48`, and keep it in your
  orchestrator's secret store.
- Run the agent endpoint separately, as `serve agent` (the Helm chart and
  the quick start do), so the processes that face the collectors' networks
  never hold the secret.
- To serve tenants who don't trust each other, run one instance per
  tenant.

### What a compromised agent pod still has: the database

The agent endpoint and the management API log in as the same role,
`telflo_opamp`, and that role holds every right the management API needs.
So code running in an agent pod could change the database directly,
without the API: add an API key for every organization, publish a
configuration, or assign one to any group. The pod also holds the
[encryption key](#the-encryption-key), so it can mint a bearer for any
token whose ids it reads there. Until the agent endpoint has a
role of its own, limited to what a poll needs, treat an agent pod as able
to change every organization's data:

- Keep the agent endpoint's image current, and run only the image.
- Turn on the chart's NetworkPolicies (`networkPolicy.enabled`), so only
  the peers you list reach the management API, and only the server's pods
  reach the bundled PostgreSQL. They limit what reaches each pod, not what
  an agent pod reaches, and it reaches the database by design.
- Watch the API key list (`GET /api/v1/api-keys`) for keys you didn't
  mint.

### Rotating it

`serve` reads its secrets only at startup. To rotate the secret without an
outage:

1. Generate the new secret.
2. On every opamp-server that runs the management API (`serve`,
   `serve management`): set `OPAMP_MANAGEMENT_SECRET` to the new secret and
   add the old one to `OPAMP_MANAGEMENT_SECRETS_PREVIOUS`. Restart them one
   at a time. Each startup line now says `"acceptedSecrets":2`.
3. Give every caller the new secret and restart them.
4. When no caller uses the old one, remove it from
   `OPAMP_MANAGEMENT_SECRETS_PREVIOUS` and restart the opamp-servers again.

A secret in the previous list can't contain a comma. **If the secret
leaked**, don't put it in the previous list: set the new secret on both
sides at once, and accept that calls fail with `401` until both have
restarted.

### Never expose the management port to the internet

Without TLS, the management API speaks plain HTTP, on every interface
unless `OPAMP_MANAGEMENT_HOST` names one, so the secret and API keys cross
the network in the clear. Keep the port on a private network that only its
callers reach, and turn on [TLS](#tls) when a caller sits across a network
you don't trust. `compose.yaml` publishes the port on the host's
`127.0.0.1` only, and is for development only: its secret and key are
committed to the repository, and `serve` refuses them unless
`OPAMP_ALLOW_DEVELOPMENT_SECRETS` is `true`, as only `compose.yaml` sets.

## API keys

An API key lets a person or a tool call the management API without the
secret ([management-api.md](management-api.md) has the routes). The
secret's holder mints each key with `POST /api/v1/api-keys` and chooses:

- **Which organizations it opens.** A key limited to some organizations
  can't see or change any other, can't register one, and can't change its
  own organizations' limits. A key for every organization
  (`organizationIds` null) is nearly as strong as the secret: only the API
  key routes and the diagnostics stay closed to it.
- **When it expires**, at most 2 years ahead. Mint a new one before then.

With a key, a caller can reveal or mint its organizations' collector
bearers, and so be served their configurations. So:

- Keep it in a secret store, as you would the secret.
- Give each person or tool its own key, named for its holder, so a revoke
  takes away exactly one holder's access.
- Revoke a key that leaked, or whose holder left
  (`POST /api/v1/api-keys/{apiKeyId}/revoke`). The next request with it is
  refused, on every server.
- Revoke keys nobody uses: `GET /api/v1/api-keys` shows each one's
  `lastUsedAt`.

The server stores only a hash of each key's secret part, so a lost key
can't be shown again: revoke it and mint another. Keys start with `opk_`,
so a secret scanner can find one committed by mistake.

### Publishing runs on the collectors' hosts

A key, like the secret, can publish a configuration and assign it to its
organizations' groups, and the server sends it to their collectors as
written. A collector's configuration can read the host's files and
environment (`${file:…}`, `${env:…}`), send what it reads to any endpoint,
and write files. So whoever holds a key can do, on every collector host of
its organizations, what the collector's user can. Run collectors as an
unprivileged user, never root, with only the files and environment they
need, and give keys only to those you'd trust with that.

## Fleets: collectors in one trust each other

A fleet's collectors share its bearers: the default token's, from their
install, and the rotate token's, pushed to them. The server tells them
apart by the instance UID each sends, which isn't secret: the supervisor
sends it on every request, and telemetry usually carries it. So a host
holding a fleet's bearer can:

- act as any other collector of the fleet: take its configuration, report
  its status, and keep it on its current configuration after a publish;
- enroll into any group of the fleet, and be served that group's
  configuration.

Deleting such a collector doesn't keep it out: it enrolls again. Revoke the
fleet's default token, make a new one, rotate the rotate token, and
redeploy the fleet's collectors with the new default bearer
([agent-endpoint.md](agent-endpoint.md#revoking-and-rotating-tokens)).

So give hosts that don't trust each other, or whose configurations must
stay apart, fleets of their own.

## The encryption key

The management API mints each enrollment token's bearer under
`OPAMP_ENCRYPTION_KEY`, and the agent endpoint reads bearers with it and
with the previous keys, and mints the rotate token's bearer it hands each
collector. Bearers are never stored: a reveal encrypts the token afresh.
Whoever holds a key can read every bearer minted under it, and mint one
for any token whose ids they know.

- Generate it with `openssl rand -base64 48`.
- Prefer `OPAMP_ENCRYPTION_KEY_FILE` (a compose or Kubernetes secret): a
  key given as a value stays readable in `/proc/<pid>/environ`.
- A key with whitespace at either end, other than a file's final line
  ending, is refused at startup. **Replace it, don't trim it**: a trimmed
  key opens none of its bearers. Then mint new bearers for its collectors
  (each fleet's reveal or rotation).

`serve` keeps the keys in locked memory (memguard), but each encryption or
decryption briefly expands the key on Go's heap. This narrows where a key
lives; it doesn't make a memory dump of the process safe.

### Rotating it

To rotate the key without refusing any collector, every running
opamp-server must always hold every key another one mints with, since old
and new servers answer side by side while they restart one at a time.

1. Generate the new key.
2. On every opamp-server: add the new key, exactly as it is, to
   `OPAMP_ENCRYPTION_KEYS_PREVIOUS`, leaving `OPAMP_ENCRYPTION_KEY` as it
   is. Restart them one at a time. Each startup line now says
   `"encryptionKeys":2`.
3. On every opamp-server, in one change: set `OPAMP_ENCRYPTION_KEY` to the
   new key and `OPAMP_ENCRYPTION_KEYS_PREVIOUS` to the old one, exactly as
   it was. Restart them one at a time.
4. Give collectors bearers under the new key: rotate each fleet's rotate
   token (`POST …/tokens/rotate`), which pushes every collector still
   polling a new bearer, and redeploy collectors with a default token's
   bearer under the new key (a reveal shows one), since a collector that
   restarts presents its default token again.
5. When no collector presents an old bearer, remove the old key from
   `OPAMP_ENCRYPTION_KEYS_PREVIOUS` and restart the opamp-servers again.

**If a key leaked**, don't put it in the previous list, where it would
still open the bearers minted with it. Set a new key as
`OPAMP_ENCRYPTION_KEY` on every opamp-server at once, with the leaked key
in neither setting, and accept that collectors are refused `401` until
they have new bearers. Then reveal each fleet's default bearer under the
new key, and redeploy its collectors with it.

**If a bearer leaked**, the key is safe: revoke that bearer's token. For a
default token's bearer, make a new default token too, and rotate the
fleet's rotate token: a default bearer is handed the current rotate
bearer on request, so its holder may hold that one already.

## TLS

Set **both** `OPAMP_MANAGEMENT_TLS_CERT_FILE` and
`OPAMP_MANAGEMENT_TLS_KEY_FILE` to PEM files, a certificate and its private
key, and the management API speaks HTTPS with TLS 1.2 or later; plain HTTP
on that port gets a `400`. Set neither and it speaks plain HTTP. One without
the other is refused at startup.

- The certificate must name the address callers use (the host in the URL
  they call).
- The files are read once, at startup: restart `serve` after renewing them.
- The key file must be readable by the image's user (uid 65532).
- Callers then use the `https://` URL. Each caller must trust a certificate
  from a private CA (a Node.js caller, say, through `NODE_EXTRA_CA_CERTS`
  pointing at the CA's PEM file).
