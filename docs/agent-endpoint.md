# The agent endpoint

Collectors poll `POST /opamp/api/v1` on the agent port (`OPAMP_AGENT_PORT`,
51234): OpAMP over HTTP with protobuf bodies, each request carrying
`Authorization: Bearer <an enrollment token's bearer>`. Unlike the
management port, it faces the collectors' networks, the internet included,
so give it **its own port, behind its own load balancer, never a web
gateway** ([`docs/decisions/0002-the-agent-tier.md`](decisions/0002-the-agent-tier.md)).

It serves supervisors: `server` fleets (on a host) and
`kubernetes-supervisor` fleets (in a cluster). The OpenTelemetry Operator's
bridge isn't served.

## The URL collectors are handed

A collector started with its fleet's default token is handed the fleet's
rotate token and `OPAMP_SERVER_URL`, and polls that URL from then on. A URL
it can't reach strands it silently until it restarts. So set it to the
address your collectors reach, such as your load balancer's
`https://…/opamp/api/v1`. `compose.yaml`'s default,
`http://agent:51234/opamp/api/v1`, reaches only collectors on the compose
network.

## TLS

Set `OPAMP_AGENT_TLS_CERT_FILE` and `OPAMP_AGENT_TLS_KEY_FILE` and the agent
port speaks HTTPS (TLS 1.2 or later), loaded as the management port's
certificate is ([security.md](security.md), "TLS"). Without them it speaks
plain HTTP and expects a load balancer in front to terminate TLS. Bearers
cross any hop without TLS in the clear, so keep the hop from the load
balancer to the pods on a private network. A supervisor speaking plain HTTP
needs `tls.insecure: true` in its `supervisor.yaml`.

## Running it separately

`opamp-server serve agent` runs the agent endpoint without the management
API or the management secret. It still holds the encryption key and the
database role ([security.md](security.md#what-a-compromised-agent-pod-still-has-the-database)
says what a compromised pod can do with the role). The Helm chart and the quick start run it this way.

## Readiness

The agent port's `GET /ready` answers `200 ready` once its process's
listeners are open and the database answers a ping within 2 seconds. It
answers `503 not ready` while `serve` starts, once it is told to stop, and
while the database doesn't answer.

- Use `/ready` as the readiness probe, with `timeoutSeconds` of at least 3,
  and `/health` as the liveness probe, so a pod that is stopping or has
  lost its database gets no new polls.
- Keep `/ready` off the public load balancer where you can: route only
  `POST /opamp/api/v1`, and `/health` if the load balancer's own health
  check needs a path. The kubelet probes the pod directly.
- Give the pod a termination grace period of at least 30 seconds
  ([configuration.md](configuration.md#shutdown)).

## Several replicas

Run as many `serve agent` pods as you need on the same database: any pod
may answer any poll. A revoked token, or a change to an organization's
`requestsPerMinute`, reaches every pod within 10 seconds. Count the
database connections as
[configuration.md](configuration.md#the-connection-pool) says.

## The limits are per pod

Each pod counts its own, so across the cluster each limit is multiplied by
the pods (for one collector, by the pods its polls reach). A fleet-wide
limit is the load balancer's.

| Limit | Default | Refused with |
|---|---|---|
| An organization's requests a minute (`requestsPerMinute` from `PUT …/limits`, else `OPAMP_DEFAULT_REQUESTS_PER_MINUTE`) | 12,000 | `429`, `Retry-After` 30–60 s |
| One collector's requests a minute (`OPAMP_COLLECTOR_REQUESTS_PER_MINUTE`) | 60, a burst of 20 | `429`, `Retry-After` 60 s |
| Requests in flight (`OPAMP_MAX_CONCURRENT_REQUESTS`) | 100 | `429`, `Retry-After` 3–8 s |
| One organization's requests in flight (`OPAMP_MAX_CONCURRENT_REQUESTS_PER_ORGANIZATION`) | 25 | `429`, `Retry-After` 3–8 s |

Each pod also tracks at most 20,000 of one organization's collectors, and
refuses its new collectors `429` past that. So spread an organization with
more than 20,000 collectors over more pods than its count ÷ 20,000.

A request's headers may be at most 16 KiB (else `431`). Its body may be at
most 4 MiB, before and after gzip (else `413`), and must arrive within 5
seconds (else `400`). Its database work has 5 seconds, else `503` with a
`Retry-After` of 10–30 s.

Each pod holds at most `OPAMP_AGENT_MAX_CONNECTIONS` connections open at
once (10,000), idle kept-alive ones included; a new one waits until one
closes. A collector polling directly keeps one open, so raise it for more
direct collectors per pod. Behind a load balancer, far fewer connections
carry them all. Limit connections per client address at the load
balancer: the server sees only its own.

## Enrollment

A collector new to the organization is enrolled only by a message that
carries its agent description, which a supervisor sends only in its full state.
A poll without one is answered `200`, asking for the full state. **The
description must name its group** (`telflo.collector_group_id`): one naming
none is refused `403` (outcome `no_group`), never enrolled into the Default
group. An organization's collector cap (`maxCollectors`) refuses a new
collector `403` "collector limit reached".

A deleted collector that keeps polling enrolls again, with nothing pinned.
A group's delete deletes its collectors; one that keeps polling, naming the
deleted group, is refused `403` (`foreign_group`) and keeps the
configuration it has.

A collector with no pin and no published version on its group is sent no
configuration and shows sync `unassigned`. It keeps running what it has.
Assign its group a published version, or pin one on it, and it is sent that
on its next poll.

## The answers

| Answer | When |
|---|---|
| `401` | a missing, malformed, unknown, revoked or expired bearer, or one minted for another fleet than its token's |
| `403` | an archived fleet, a collector of another fleet, a group that isn't its fleet's, no group, or the collector cap |
| `400` | a request it can't read |
| `413` | a body over 4 MiB, or one holding over 100,000 fields (a real supervisor's holds a few thousand) |
| `429` | a limit above |
| `503`, with `Retry-After` | a database failure (never `400`) |

Refusals name no id, count or reason; the log line has the reason.

## Revoking and rotating tokens

**Think before you revoke a fleet's default token**
(`POST …/tokens/{t}/revoke`). The supervisor doesn't keep a pushed rotate
token across a restart, so each of the fleet's collectors that restarts
presents the default token again and, refused, stays offline. Once the
fleet's default tokens are all revoked, `POST …/tokens/default` makes a new
one; re-run a stranded collector's install command with it.

**Rotate the rotate token before it expires, and think before you revoke
it.** A rotation hands the new token to collectors polling with the old one
during its grace, so rotate while the current one is still valid. A revoked
or expired rotate token is refused `401`, and the supervisor keeps polling
with it: **its collectors are stranded until their supervisors restart**
and present the default token again. A fleet with no valid rotate token is
"suspended": a collector on its default token is served nothing until a
rotation makes one.

## Diagnostics

Go's profiles (`/api/v1/debug/pprof/`) and the admission view
(`/api/v1/debug/admission`) are never on the agent port. With `serve`, they
are on the management port, behind the management secret. With
`serve agent`, set `OPAMP_AGENT_DIAGNOSTICS_PORT` to serve them on a
loopback address only, and reach them with
`kubectl port-forward pod/<an agent pod> 6060:<the port>`. A CPU profile or
trace takes at most 25 seconds.
