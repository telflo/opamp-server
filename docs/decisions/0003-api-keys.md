# 0003. API keys beside the shared secret, limited by organization

**Status:** Accepted
**Date:** 2026-10-28

## Context

`0001-reached-only-through-its-api.md` puts the management API behind one
shared secret, and records its cost: the secret opens every organization
until it is rotated. That suits a platform that holds the secret and calls
the API for its users. It doesn't suit an enterprise that self-hosts the
server and wants its own people and tools to call the API directly:

- a person or a pipeline that needs one organization gets all of them;
- every holder has the same credential, so taking one holder's access away
  means rotating the secret for everyone;
- nothing records which holder made a call.

The platform keeps calling with the secret, so whatever is added must sit
beside it, not replace it.

## Decision

### Keys the secret's holder mints

- **The secret mints, renames, lists and revokes keys**
  (`/api/v1/api-keys`). A key can't manage keys, so a key can never make
  a stronger one.
- **Each key names its organizations, or opens every one.** A key for
  every organization is for tooling that registers organizations; only
  it may call `POST /api/v1/organizations`.
- **Every key expires**, at most 2 years after it is minted, and at most
  100 may be valid at once. The 100 are counted under an advisory lock, so
  two mints can't both pass the count.
- **A key's organizations and expiry never change.** To widen a key,
  mint another and revoke the first. Only its name and description change.

### What a key opens

- **The routes under `/api/v1/organizations/{organizationId}`, for its
  organizations.** Under any other organization a key gets the same 404
  as an organization that isn't registered, so it can't learn which
  organizations exist. Answering that 404 also keeps the contract test's
  documented answers as they are: no route gains a 403.
- **But not setting an organization's limits.** `PUT …/limits` sets the
  guards (`maxCollectors`, `requestsPerMinute`) that keep one organization
  from taking the agent pods and database its neighbours share, so it is
  the operator's: the secret, or a key for every organization. A key for
  some organizations, this one included, gets `403` `forbidden`; any key
  for the organization can still read them. The 403 tells the key nothing
  it didn't know: it holds the organization.
- **Nothing else.** The key routes, the diagnostics, and any route added
  without a rule of its own answer a key `403` `forbidden`. A new route is
  closed to keys until someone decides otherwise.
- **Every check is by the route's pattern, in one wrapper** around the
  API's router (`authorize`), not in each handler, so a handler can't
  forget it.

### The key itself

- **`opk_`, the key's id in hex, `_`, and 32 random bytes in base64url.**
  The prefix lets a secret scanner find a leaked key. The id lets the
  server look the key up, and lets a log line name it without its secret.
- **Only the SHA-256 of the secret part is stored.** It is 32 random
  bytes, so a plain hash is enough: there is nothing to guess.
  The hashes are compared in constant time.
- **A key is checked against the database on every request**, with no
  cache, and its `last_used_at` is written each time. So a revoke holds
  from the next request on every server, where an enrollment token's takes
  up to 10 s (`0002-the-agent-tier.md`). The cost is a read and a write per
  request made with a key; the management API's traffic is small.
- **The secret is checked first**, without the database, so the platform's
  calls are unchanged and keep working while the database is down.

### The server's own table

- **`api_keys` belongs to no organization.** It is the first such table:
  every other table holds an organization's rows, with `organization_id`
  and a cascade from its organization (`0001`'s rule). A key's
  organizations are rows of `api_key_organizations`, which carry
  `organization_id` and its cascade, so an organization's delete takes it
  from every key. Registering the same id again doesn't give a key access
  back. The schema tests name `api_keys` as the one exception
  (`database.ServerTable`).
- **Keys are never deleted.** A revoke sets `revoked_at`, and the row
  stays as the record of who held what. The runtime role can't delete a
  key, or change its secret, organizations or expiry.

## Alternatives rejected

- **Signed tokens (JWTs) a caller verifies itself.** Revoking one before
  it expires needs a deny list, which is the database lookup this avoids
  only on paper. A stored key is revoked by a write.
- **Keys scoped by route or by read and write.** Organization is the
  boundary everything else in the server keeps. Finer scopes can be added
  later as columns: a key minted now would get the full scope.
- **`403` for another organization's routes.** It would tell a key which
  organizations exist, and every organization route would document one
  more answer.
- **A key's id in the audit columns** (`created_by`, `changed_by`, an
  event's actor). Those columns hold the platform's person, as an id and an
  email. Adding a key's id to each is a change to seven tables and to
  every reply that shows an actor. The request's log line names the key
  instead.
- **Caching keys for 10 s, as tokens are.** That would save a read per
  request on an API that is rarely busy, at the price of a revoke that
  doesn't hold at once.

## Consequences

- An enterprise can give each person and tool its own key, for the
  organizations it needs, and revoke one holder's access without touching
  anyone else's.
- The audit rows still name whoever the request's `actor` says, as with
  the secret. Which key made a change is only in the request's log line
  (`apiKeyId`) and in the key's `lastUsedAt`.
- A key for every organization is nearly as strong as the secret; the
  docs say to give it only to tooling that registers organizations.
- Revoked and expired keys stay listed; nothing prunes them.
- A server of an earlier release refuses every key, so keys are minted
  only once every server runs this one (`docs/upgrading.md`).
