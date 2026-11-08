# The management API

[`api/openapi.yaml`](../api/openapi.yaml) (OpenAPI 3.1) is the complete
contract: every request, reply and error code. A test checks the server
against it, and your client's tests can check against it too. This page is
a summary.

## Authentication

Every route under `/api/` needs `Authorization: Bearer <credential>`: the
management secret, or an API key. A missing, malformed or wrong header, a
revoked or expired key, or two `Authorization` headers answer `401`.

The secret opens everything. An API key opens the organizations it names,
or every organization:

- Under its organizations, a key's routes answer as they do with the
  secret. Under any other, they answer the same `404` as an organization
  that isn't registered, so a key learns nothing about organizations it
  doesn't hold.
- Registering an organization (`POST /api/v1/organizations`) needs a key
  for every organization; another key gets `403` `forbidden`.
- So does setting an organization's limits (`PUT …/limits`): they keep one
  organization from taking the agent pods its neighbours share, so a key
  for that organization alone gets `403` `forbidden`. Any key for the
  organization can read them.
- The API key routes, and the diagnostics, take the secret only: a key
  gets `403` `forbidden`.

## Organizations and publications

Register an organization before you store anything under it. Every route
under an organization that isn't registered answers `404`
`organization_not_found`, so a typo can't create an orphan tenant. Pass
your own id (a platform passes its tenant's) and a name of 1 to 400
characters; without an `id`, the server makes one. Every other route sits
under the organization's path, written `…` below:
`/api/v1/organizations/{organizationId}`. Another organization's data
answers a plain `404`, as an unknown id does.

| Method and path | Answers |
|---|---|
| `POST /api/v1/organizations` with `{"id"?: UUID, "name": "…"}` | `201` `{"organizationId"}`; `200` when the id is already registered with exactly this name (a safe retry); `409` `organization_conflict` when it's registered with another name |
| `GET …` | `200` `{"organizationId","name","createdAt","updatedAt"}`; `404` |
| `PATCH …` with `{"name": "…"}` | `200` the organization, renamed; `404` |
| `DELETE …` | `204`, with everything it owns: its fleets, collectors, tokens, publications and variables; `404`; `503` `delete_in_progress` ([Long deletes](#long-deletes)) |
| `POST …/publications` with an optional `id` | `201` stored, its `id` in the reply; `200` an identical one was already stored under the `id`; `409` `publication_conflict`; `422` `sha256_mismatch` |
| `GET …/publications/{p}` | `200` with its `yaml`; `404` |
| `DELETE …/publications/{p}` | `204`; `404`; `409` `publication_in_use` while a collector group is assigned it or a collector is pinned to it |
| `GET …/publications?configurationId=…&limit=…&cursor=…` | `200` one page, newest first, without `yaml`; `nextCursor` for the next page, or null on the last. `configurationId` is required; `limit` is 1 to 100, default 50 |
| `DELETE …/configurations/{c}/publications` | `204`, whether or not any existed; `409` `publication_in_use`, deleting none, while a group or a collector uses any of them |
| `GET …/configurations/{c}/usage` | `200` the configuration's published versions, newest first, each with the groups assigned it and how many collectors are pinned to it, and `inUse` |
| `GET /health` | `200 ok`; no secret, no database |
| `GET /ready` | `200 ready` once the process serves and reaches its database; `503 not ready` otherwise; no secret ([agent-endpoint.md](agent-endpoint.md)) |

## Long deletes

Deleting an organization, a fleet or a group deletes everything under it,
and on a big fleet that takes seconds: an organization of 40,000
collectors took about 8. Give these three deletes a longer timeout than
other calls, 25 seconds, say. Once started, a delete runs to its end even
if you hang up, within 5 minutes.

A retry of an organization's or a fleet's delete can answer `503`
`delete_in_progress`, with `Retry-After`, while the first is still
running: retry later. A retry of a group's delete waits behind the first.
Once the delete is done, a retry answers `404`: count that as deleted.

## API keys

The secret's holder mints keys for people and tools that shouldn't hold
the secret. Each key expires within 2 years, and at most 100 may be valid
at once.

- The server keeps only a digest of the key. The key itself is answered
  once, by its create, with `Cache-Control: no-store`.
- A revoke holds from the next request on, on every server.
- Deleting an organization removes it from every key. Registering its id
  again doesn't give a key access back.

| Method and path | Answers |
|---|---|
| `POST /api/v1/api-keys` with `{"name", "description", "organizationIds", "expiresAt"}` | `201` the key, with `key` answered this once; `404` `organization_not_found`; `409` `api_key_limit_reached`. `organizationIds` null opens every organization |
| `GET /api/v1/api-keys` | `200` `{"apiKeys"}`, newest first, each with its `status` (`active`, `expired` or `revoked`) and `lastUsedAt` |
| `GET /api/v1/api-keys/{apiKeyId}` | `200` the key; `404` `api_key_not_found` |
| `PATCH /api/v1/api-keys/{apiKeyId}` with `{"name"?, "description"?}` | `200` the key; `404`. A key's organizations and expiry never change: mint another |
| `POST /api/v1/api-keys/{apiKeyId}/revoke` | `200` the key, revoked; `404` |

## Fleets, groups, collectors and tokens

| Method and path | Does |
|---|---|
| `POST …/fleets` with `{"id"?, "label", "type", "rotateExpiresAt", "actor"}` | `201` the fleet, its "Default" group, and both tokens' **bearers**; `200` the fleet without bearers when the id is already this fleet (a safe retry); `409` `fleet_conflict` or `fleet_label_taken`. `type` is `server` or `kubernetes-supervisor` |
| `GET …/fleets?status=&limit=&cursor=`, `GET …/fleets/{f}` | the fleets, newest first, or one, each with its collectors counted by liveness and by sync, when one was last seen, and its rotate token's state |
| `PATCH …/fleets/{f}` | `label`, `status` (`archived` or `active`); `409` `fleet_label_taken` |
| `DELETE …/fleets/{f}` | `204`, with its groups, collectors and tokens; `503` `delete_in_progress` ([Long deletes](#long-deletes)) |
| `GET`, `POST …/fleets/{f}/groups`; `GET`, `PATCH`, `DELETE …/groups/{g}` | the groups; create or change one: its name, its published version, its variable group, and `annotations`, your own JSON data, which the server stores and never reads; `409` `collector_group_name_taken`; `422` `publication_not_found`, `variable_group_not_found` or `variables_undefined`. The Default group can't be deleted (`409` `collector_group_is_default`); deleting another deletes its collectors |
| `GET …/fleets/{f}/collectors?q=&groupId=&sort=&order=&limit=&cursor=` | the collectors, searched by name and addresses (`q`), sorted by `created`, `lastSeen` or `name` |
| `GET`, `PATCH`, `DELETE …/collectors/{c}` | one, with what it's served and what it reports; rename it, move it to another group, pin a published version (`{"kind": "publication", "id"}`) or a variable group (`pinnedVariableGroupId`), or `null` to unpin; delete it (it enrolls again on its next poll, see [agent-endpoint.md](agent-endpoint.md)) |
| `POST …/fleets/{f}/collectors/pin` | pin or unpin up to 500 collectors at once |
| `GET …/collectors/{c}/errors` | its error history, newest first (the newest 100, 30 days at most) |
| `GET …/fleets/{f}/tokens` | the fleet's tokens, each with the collectors seen on it in the last 5 minutes; never a bearer |
| `GET …/fleets/{f}/tokens/events?limit=&cursor=` | the token events (`issued`, `viewed`, `rotated`, `revoked`), newest first, each with its `actor` |
| `POST …/fleets/{f}/tokens/rotate` with `{"expiresAt", "graceEndsAt", "actor"}` | `201` a new rotate token and its **bearer**; the current one expires at the grace end, and its collectors are pushed the new one; `409` `fleet_archived` |
| `POST …/fleets/{f}/tokens/default` with `{"actor"}` | `201` a new default token and its **bearer**, once every default token the fleet had is revoked; `409` `fleet_archived` or `enrollment_token_default_exists` |
| `POST …/tokens/{t}/reveal` with `{"actor"}` | a fresh **bearer** for the token; `409` `enrollment_token_revoked` or `enrollment_token_expired` |
| `POST …/tokens/{t}/revoke` with `{"actor"}` | the token, revoked; refused by every pod within 10 s |
| `GET`, `PUT …/limits` | the organization's `maxCollectors` (null: no cap) and `requestsPerMinute` (null: the server's default), both sent on a `PUT`, which takes the secret or a key for every organization |

Every reply carrying a bearer is sent `Cache-Control: no-store`. `q` puts a
collector's name or address in the URL: the server never logs a path or a
query, and you shouldn't log the URLs you call either.

## Organization variables

A collector's configuration can reference an organization's variable as
`${org:NAME}`; each poll fills it in for that collector. A name is
`[A-Za-z_][A-Za-z0-9_]*`, at most 128 characters, and a value is up to
1,024 characters on one line. A variable group holds overrides of the
values, used by the groups and collectors that choose it. At most 150
variables and 100 variable groups per organization.

**There are no secret variables**: every value is answered in the lists.
Keep secrets as `${env:NAME}` on the collector.

Every create, change, override and reset takes an `actor` (`{id, email}`
or `null`); the last write wins.

| Method and path | Does |
|---|---|
| `GET`, `POST …/variables` | every variable, values included; create one, under your own `id` to make a retry safe (`201`; `200` the same again; `409` `variable_conflict`, `variable_name_taken` or `variable_limit_reached`) |
| `PATCH`, `DELETE …/variables/{v}` | rename it, change its value or description; delete it with its overrides. A rename or a delete is `409` `variable_in_use` while a published version in use references it |
| `GET`, `POST …/variable-groups`; `GET`, `PATCH`, `DELETE …/variable-groups/{g}` | the variable groups; one with its overrides; create (`409` `variable_group_conflict`, `variable_group_name_taken`, `variable_group_limit_reached`), rename, delete it with its overrides (`409` `variable_group_in_use` while a group or a collector uses it) |
| `PUT`, `DELETE …/variable-groups/{g}/overrides/{v}` | set the group's override of a variable (`201` new, `200` replaced); reset it (`204`; the body is `{"actor"}`) |

## Errors

Errors are `{"error":{"code":"…","message":"…"}}`. A message names the
field at fault, never its value. `variable_in_use`,
`variable_group_in_use` and `variables_undefined` add `details`: what is in
the way, never a value.

Every route under `/api/` can also answer `401`, `400` for a path id that
isn't a UUID, and `500` `internal_error`. A route that takes a body answers
`400` when a field breaks its rules and `413` when the body is over 2 MiB.
A path no route matches answers `404` `not_found`.

## A publication

A publication is the `POST`'s body, sent as `application/json`. Every
field but `id` is required (`null` where allowed), and an unknown field is
refused:

| Field | Rule |
|---|---|
| `id` | optional; a UUID. Left out, the server makes one |
| `configuration.id`, `version.id` | UUIDs: your own ids |
| `configuration.name` | 1 to 200 characters |
| `version.number` | an integer from 1 to 2,147,483,647 |
| `version.label` | 1 to 80 characters, or `null` |
| `yaml` | 1 byte to 1 MiB of UTF-8; stored and returned exactly |
| `sha256` | 64 lowercase hex characters: the SHA-256 of `yaml`, else `422` |
| `publishedBy` | `{ "id": UUID, "email": 1 to 320 characters }`, or `null` when no person published |
| `publishedAt` | an RFC 3339 date-time, kept to the millisecond |
| `collector` | `{ "distribution": …, "version": … }`, such as `otelcol-contrib` and `0.162.0`: the collector it was published for. Never `null` |

**Send an `id` to make a retry safe.** The same id again with an identical
body changes nothing and answers `200`. The same id with anything
different, or an id another organization holds, answers `409`. Without an
`id`, each `POST` stores a new publication, so a retry after a lost reply
stores a second.

Nothing is ever updated: a new version is a new publication. A delete is a
hard delete.

## An example

With `jq` building the body (`--rawfile` keeps the file's exact bytes, so
the digest matches; on macOS use `shasum -a 256` for `sha256sum`). The
header goes in a file, so the secret stays off curl's command line:

```sh
(umask 077; printf 'Authorization: Bearer %s\n' "$OPAMP_MANAGEMENT_SECRET" > auth-header)
organization=$(uuidgen)
# The publication's id, sent in the body so a retry is safe. Leave it out
# of the body and the server makes one, answered in the reply's "id".
publication=$(uuidgen)

# Register the organization first: 201, and 200 if you run it again.
curl -sS -X POST -H @auth-header -H 'Content-Type: application/json' \
  --data "{\"id\": \"$organization\", \"name\": \"Payments team\"}" \
  http://127.0.0.1:51235/api/v1/organizations

jq -n --rawfile yaml collector.yaml --arg id "$publication" \
  --arg sha256 "$(sha256sum collector.yaml | cut -d' ' -f1)" \
  --arg configuration "$(uuidgen)" --arg version "$(uuidgen)" \
  '{id: $id, configuration: {id: $configuration, name: "payments-gateway"},
    version: {id: $version, number: 1, label: null},
    yaml: $yaml, sha256: $sha256, publishedBy: null,
    publishedAt: (now | todate),
    collector: {distribution: "otelcol-contrib", version: "0.162.0"}}' > publication.json

curl -sS -X POST -H @auth-header -H 'Content-Type: application/json' \
  --data-binary @publication.json \
  "http://127.0.0.1:51235/api/v1/organizations/$organization/publications"
curl -sS -H @auth-header \
  "http://127.0.0.1:51235/api/v1/organizations/$organization/publications/$publication"

# Rename it; read it; delete it, and its publications with it.
curl -sS -X PATCH -H @auth-header -H 'Content-Type: application/json' \
  --data '{"name": "Payments platform team"}' \
  "http://127.0.0.1:51235/api/v1/organizations/$organization"
curl -sS -H @auth-header "http://127.0.0.1:51235/api/v1/organizations/$organization"
curl -sS -X DELETE -H @auth-header "http://127.0.0.1:51235/api/v1/organizations/$organization"
```

A publication under an organization that isn't registered (another
`uuidgen`, say) answers:

```json
{"error":{"code":"organization_not_found","message":"No organization with this id is registered: register it with POST /api/v1/organizations first."}}
```
