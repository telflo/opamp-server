# Collector state model

This document explains the columns of `opamp_server.collector_instances`
that say what a collector should run, what it was served and what it
reports, the OpAMP messages they come from, and how the server computes a
collector's sync and liveness from them, organization variables
included.

If you are debugging "why does this collector read pending when I expect
in sync", start at [Sync](#sync), then walk back through
[Who writes what](#who-writes-what).


## The three concerns

| Concern | Owner of truth | Columns |
|---|---|---|
| **Intent**: what the collector should run | Operators, through the management API | `pinned_publication_id` (a pin), else its group's `collector_groups.assigned_publication_id`; filled in with its variables: `pinned_variable_group_id`'s overrides, then its group's `collector_groups.variable_group_id`'s, then the organization's values ([Organization variables](#organization-variables)) |
| **Delivery**: what the server last served | The poll | `served_publication_id`, `served_sha256` (the filled-in bytes'), `served_at`, `served_variables_stamp` (the marker) |
| **Reality**: what the collector reports | The collector's `RemoteConfigStatus` | `effective_sha256`, `effective_status`, `effective_error`, `effective_at` |

```
resolved = pin ?? group's assignment ?? nothing   ← INTENT
served                                            ← DELIVERY
effective                                         ← REALITY
```

The resolution is computed by `store/collectors`' poll statement (one
statement) by its SHA-256 only, never its YAML; there is no fleet default.
A pin wins over the group's assignment; clearing it lets the group's
apply.


## OpAMP messages the poll reads

### `AgentToServer.remote_config_status`

```protobuf
message RemoteConfigStatus {
  bytes  last_remote_config_hash = 1;  // the ConfigHash it was served
  RemoteConfigStatuses status   = 2;   // UNSET | APPLIED | APPLYING | FAILED
  string error_message          = 3;   // with FAILED
}
```

The reality signal. `effective_sha256` is the hash it reports, in lowercase
hex; a hash that isn't 32 bytes, or `UNSET`, records nothing. The
collector's *effective configuration* bytes are never compared with what
was served: the supervisor's effective configuration
adds its own parts, spells out defaults and drops comments, so it never
equals what was served.

### `AgentToServer.effective_config`

Read by nothing. The server stores none of a collector's configuration,
and never compares it to work out a collector's sync.

### `AgentToServer.agent_description`

Sent on connecting and on a change. The poll reads `service.version`
(identifying) into `reported_collector_version`, `host.ip`
(non-identifying, a string or an array's first string) into
`reported_host_ip`, `telflo.collector_group_id` to choose a new
collector's group (required: a description naming none is refused
`403`), and all its string-like attributes into
`reported_attributes` (bounded by the store's 16 KiB). The request's
client address goes to `reported_ip`, for display only: the leftmost
address of its `X-Forwarded-For` header when it has one, else its
`X-Real-IP`, else the connection's peer address. The collector, or
anything between it and the load balancer, can set either header, so it is
never trusted for anything; only an IP address is stored, never other text
(`middleware/client_ip.go`).

A collector the organization doesn't hold yet is enrolled only by a
message carrying its description: the description names the group it
asks for (`telflo.collector_group_id`), and a foreign group is refused
`403`. One that polls without it (a supervisor's later messages leave the
description out while it is unchanged) is answered `200` asking for its
full state (`ReportFullState`, the outcome
`full_state_before_enrollment`), nothing recorded, and enrolled by the
poll that answers with its description, so it is never enrolled into a
group it didn't name. A description naming no group is refused `403`
(outcome `no_group`) too, never enrolled into the Default group.


## Who writes what

Every agent-side write is the poll's (`handlers/`), through
`store/collectors`' `Report`: one update per poll, each part guarded by
`is distinct from`, so an unchanged poll writes nothing. `last_seen_at` is in no index, so a heartbeat is a HOT update.

| Column | Written | Notes |
|---|---|---|
| `last_seen_at`, `enrollment_token_id` | by the poll statement itself | At most once per 15 s unless the token changed; `greatest` of the stored and the poll's time |
| `served_*`, `served_at` | `Report.Served`: a push | The push's published version, the SHA-256 of the rendered bytes, and the poll's time. A re-delivery of a stuck failed collector is a push too, recorded at its own time |
| `served_*` | `Report.Reconciled`: no push | The collector already reports the rendered hash (a row re-enrolled, a retarget to identical bytes): the pointer and hash move; `served_at` is set only when it was null, to the earlier of now and its report's time |
| `served_variables_stamp` | `Report.Served` and `Report.Reconciled`, or `Report.VariablesStamp` alone | The marker: the wanted stamp the poll filled the text in at, with a push or a reconcile; alone, without a push, when the collector was already served the resolved version's filled-in bytes but its marker differs (a change that left the bytes as they were). It is in no index, so its write stays HOT |
| `effective_*` | `Report.Effective` | Every poll carrying a `RemoteConfigStatus` (APPLIED, APPLYING, FAILED); `effective_at` is the poll's time, so a re-report refreshes it. `effective_error` only with `failed`, bounded to 8 KiB |
| `collector_errors` (a row) | `RecordError` | Each FAILED (or health-gated) report: its message, or "The collector reported a failure without a message.", and the hash. The newest 100 per collector are kept, 30 days at most. A filled-in text over 1 MiB adds one row of its own ([The push](#the-push)) |
| `full_state_requested_at` | `Report.FullStateRequestedAt` | When the poll asked the collector for its full state; see below |
| `reported_*` | `Report.Reported` | Each only when it differs from the stored value (attributes whenever a description comes) |

### The health gate on APPLIED

The supervisor answers a re-delivered configuration whose merged content is
unchanged with APPLIED at once, without re-applying it or looking at the
collector, and after a crash during apply reports FAILED once and then
restarts the collector every 5 s in silence. The same message carries
`Health` (the full-state ask makes it), which the supervisor sets unhealthy
with the exit reason and a log tail as `last_error` on every crash. So an
APPLIED with an unhealthy `Health` carrying a `last_error` is recorded
`failed`, with that error (and an error row). An unhealthy `Health` with an
empty `last_error` is the supervisor's report before it starts the
collector, not a crash: it doesn't demote. A healthy APPLIED, or one
without `Health`, is applied.

### Asking for the full state (`ReportFullState`)

OpAMP compresses messages: a collector re-sends its `RemoteConfigStatus`
and description only on a change. The poll sets `ReportFullState` on its
answer:

- to a default token's poll that is pushed the rotate token (the
  hand-off); one that isn't (a suspended fleet, or an agent that takes no
  connection settings) is asked as an unassigned collector is;
- for a collector something resolves for, whose sequence number is past 1
  (it polled before), when it has no `effective_sha256` (its row deleted
  and re-enrolled), or has reported `applying` or `failed` for more than
  3 minutes (`StuckThreshold`); a stuck `failed` collector is re-delivered
  the same bytes as well (a push, recorded served now), since a bare ask
  only makes it repeat its verdict;
- for a collector nothing resolves for, on a fleet of either type (see
  [Nothing resolved](#nothing-resolved)).

Each ask but the hand-off's records `full_state_requested_at`, and is made
only when that is null or 3 minutes old (`FullStateInterval`), whichever
replica asks, so no collector is asked, or re-delivered to, on every poll.


## The push

```
values   = the collector's organization variables (below), from the variables cache
rendered = fill in (YAML of what resolved, values)
push when served_sha256 != sha256(rendered), or a stuck failed re-delivery,
     unless the collector already reports sha256(rendered) (then Reconciled)
```

- The YAML comes from the YAML cache, keyed by the organization **and** the
  SHA-256 (so one organization's entry never answers another's), else from
  the store, its SHA-256 checked against
  the one resolved. What resolved, deleted before its YAML is read, is a
  `503` retry.
- **The fill-in** (`store/orgvariables`' `render.go`): each live `${org:NAME}` whose NAME the collector has a value
  for is replaced by that value, **every `$` of the value written `$$`**, so
  the collector's own confmap pass yields the value exactly (a value
  `${env:X}` reaches the collector as that text, never as a reference). A
  reference after an odd run of `$` (`$${org:X}`) is an escape and stays as
  written; so does a name no variable has; `$orgvar:NAME` is plain text.
  There is no YAML round trip: a text with no live reference is served
  exactly as published. Values are single-line (no line break or control
  character), but the fill-in doesn't read the YAML around a
  reference: a value holding ` #` or `: ` in an unquoted scalar changes how
  YAML reads it, so quote the reference in the configuration.
- **The 1 MiB cap**: the filled-in
  length is counted first (`RenderedLength`), so nothing is built before
  its size is known. A text over `orgvariables.MaxRenderedBytes` (1 MiB of
  UTF-8; exactly 1 MiB is pushed) is never built: its SHA-256 is streamed
  through `RenderTo`, nothing is pushed or recorded served, the collector
  keeps what it runs and reads pending, and the poll writes one error row
  of that hash, "The configuration with its organization variables filled
  in is over 1 MiB." (`ErrorReport.OncePerHash`: no second row while the
  collector's history holds one of that hash and message), with the outcome
  `rendered_too_large`. Its reports are written as on any poll.
- An empty rendered YAML is never pushed, nor recorded served.
- The push is one file, `otelcol.yml`, `text/yaml`, `ConfigHash` the
  SHA-256 of its bytes: the filled-in bytes, so a value's change is a new
  hash and a push, with no republish.
- **The marker** goes with the push or the reconcile: `served_variables_stamp`
  is the poll's wanted stamp ([Organization variables](#organization-variables)).
  A collector already served the resolved version's filled-in bytes whose
  marker differs (an override equal to the value it hides, a binding to a
  variable group that overrides nothing its text uses) has its marker moved
  alone, without a push.

The push is keyed on delivery against intent, not reality: a
collector that reports FAILED on what it was served isn't pushed it again
every poll; the operator fixes the configuration and publishes, which
changes the hash. Only the stuck re-delivery, at most every 3 minutes,
repeats a push.


## Organization variables

An organization's variables, its variable groups and their overrides live
in `opamp_server` (`variables`, `variable_groups`,
`variable_group_overrides`, migration 00012) behind the management API, and
the poll fills a published version's `${org:NAME}` references in with
them. There are no secret variables:
a value is configuration text, never logged, and secrets stay `${env:NAME}`.

### Where a value comes from

Per variable, from the top down (`orgvariables.Choose`):

```
the pin's variable group's override       collector_instances.pinned_variable_group_id
  else the group's variable group's        collector_groups.variable_group_id
  else the organization's value            variables.value
```

The collector group's variable group applies under a pin too; a pinned
variable group is independent of a pinned published version (either, both
or neither). An override's Reset keeps its row with no value (a **reset
row**), which is passed over for the value as a missing row is.

### Stamps, the counter and the marker

Sync needs "was it served the values it should run?" without a query per
collector and without trusting replicas' clocks. So:

- **The counter**, `organizations.variables_revision`, moves on by one in
  the transaction of every write that can change any collector's filled-in
  text, under the organization's row `for no key update` (the
  lock order's first step, `CONTRIBUTING.md`). Its values are the
  stamps, so they are commit-ordered.
- **Each change stamps the row that changed**, never the variable
  group: a variable's create, value change or rename stamps the variable's
  row; an override's set, change or Reset stamps the override's row (a
  Reset keeps it as a reset row with its stamp; setting it again reuses
  it); a collector group's variable-group setting set, changed or cleared
  (or a group created with one) stamps `collector_groups.variable_group_stamp`;
  a pin's variable group set, changed or cleared, and a move between groups
  whose variable groups differ, stamp `collector_instances.pinned_variable_group_stamp`.
  A variable's delete moves the counter but stamps nothing (its row is
  gone; the delete is refused while a used version references it). A
  description, a variable group's create, name, description or delete, and
  a write that changes nothing (the same value again, a Reset of nothing)
  move nothing. A binding stamp is kept when the binding is cleared.
- **A move between different groups whose variable groups are the same,
  or both none**, moves no counter: it carries the old group's setting's
  stamp onto the collector (`pinned_variable_group_stamp` = the greater of
  its own and the old group's). Without the carry, a change of
  the old group's variable group the collector hadn't been served yet would
  read In sync in its new group, whose own setting's stamp may be older.
- **The wanted stamp** of a collector is 0 when its text references no
  name; else the greatest of each name's stamp (the greatest stamp among the
  rows read from the top down to the value's source, the source included:
  a reset row above the source counts, so its Reset is seen; a row below it
  doesn't, so a change it hides isn't) and the two binding stamps
  (`orgvariables.WantedStamp`). The poll computes it in Go from the cached
  variables; List, Get and the fleet summaries compute it in SQL with one
  shared fragment (`store/collectors`' `wantedStamp`: the distinct
  combinations of resolved version, pin's and group's variable groups
  materialized once, set-based, never a query per collector), held equal by
  parity tests.
- **The marker** (`served_variables_stamp`) is the wanted stamp the poll
  filled the collector's text in at, **capped at the counter the poll
  statement read**: on a cache miss the values are read at a
  later revision than the statement read the collector's bindings and
  stamps at, and a rebinding and an override landing in between would
  otherwise mark it served a change whose values it never got. Capped, it
  reads pending until its next poll.

### The names a published version references

`publication_variable_names` holds each published version's referenced
names (`orgvariables.References`: the same scanner as the fill-in), written
**at publish only**, in the publication's own insert statement. A text
with a live reference to a name over 128 characters, which no variable can
have, is refused at publish (`400 invalid_request` naming `yaml`). The names serve three readers: the wanted stamp
above, the in-use guards (a variable's rename or delete, `409
variable_in_use`) and the undefined check on an assignment or a pin (`422
variables_undefined`).

**A version published before migration 00012 has no names until it is
republished** (development databases only). It is filled in at poll as usual, since the poll takes the names from
the fill-in, never from this table; but the guards don't count it and the
targeted Status ignores it: its wanted stamp is 0, so after a value's or a
binding's change its collectors read in sync until their next poll pushes
the new bytes (then pending until they report, as after any push).
Republish it to bring it under the guards and the Status.

### The variables cache

`handlers/variables_cache.go`, per pod:

- keyed by (organization, its **generation**, its **counter**, the
  collector group's variable group, the pin's variable group), `uuid.Nil`
  for none; the poll statement answers the counter and the generation, so
  a steady poll reads no variables;
- least recently used, at most 64 MiB in all and 16 MiB per entry, with no
  time to live; an organization only ever reads its own entries;
- a miss reads the variables in one statement and keeps them under the
  revision that read answered, and only when that read answered the poll
  statement's generation too (`orgvariables.Resolved` carries both its
  `Revision` and its `Generation`; `Put` keeps nothing read at another
  generation), so the values under one key never change; an
  organization whose counter is 0 (it never had a variable) reads nothing
  and keeps nothing; a read that fails is the poll's `503`, kept nowhere;
- **exact**: every write that can change filled-in text moves the counter
  (a variable's delete included), and the next poll looks up the
  new revision and misses, so no entry is ever served after a change. The
  generation (`organizations.generation`, a random uuid the store writes
  when the organization is registered) keeps an organization
  deleted and registered again under the same id, whose counter starts at
  0 again, from reading its previous incarnation's entries.

Each replica has its own cache, so a change costs one miss per combination
per replica. Concurrent misses of one key each read (no single-flight
yet).

**What an entry costs** (`variablesSize`): Σ(len(name) + len(value) + 64)
+ 256 bytes, in UTF-8. An entry is every variable of the organization,
each resolved for one combination of the two variable groups, so at the
bounds (at most 150 variables, a name of at most 128 characters, a value
of at most 1,024) the worst entry is about 0.2 MB, about 0.65 MB with
4-byte characters, and 64 MiB holds about 100 to 360 combinations per pod
before the least recently used goes. A two-level cache, each
organization's values kept once per revision and each combination only
its overrides, is later work.


## Nothing resolved

A collector nothing resolves for (no pin, and no published version on its
group) is sent no configuration: its poll is acknowledged,
asking for its full state at most once per `FullStateInterval` (3 minutes),
on either fleet type, and its reports are recorded as on any poll. It keeps
running what it has, its own file or the last published version it was sent;
its `served_*` columns are left as they were, and it reads `unassigned`
(below). Once its group is assigned a published version, or one is pinned on
it, its next poll is a push (unless it already runs those bytes, [The
push](#the-push)).


## Sync

A collector's sync says whether it runs what resolves for it: the
publication it's assigned or pinned to, with its variables filled in. A
changed variable makes it `pending` as surely as a new publication does.
The server computes it from the injected clock, ids, stamps and hashes
only (`collectors.SyncOf`; the fleet summaries count by the same steps, in
the same order):

```
if nothing resolves:                                   unassigned
if never served, or served another published version than the resolved one,
   or served_variables_stamp (0 when null) < the wanted stamp,
   or no report since it was served (effective_at < served_at):   pending
if effective_status == failed:                         failed
if effective_status == applied:
    effective_sha256 == served_sha256 ?                in_sync : drifted
otherwise (applying):                                  pending
```

`effective_at >= served_at` answers "is its last report about what it was
last served?": a report from before a
new delivery doesn't count, so a collector pushed a new version reads
pending until it reports again.

The served hash is the filled-in bytes', so it is never compared with the
published version's own hash, which would read every collector using a
variable pending forever. "Served what it should run" is **the served
published version is the resolved one, and the marker is
at least the wanted stamp** ([Stamps, the counter and the
marker](#stamps-the-counter-and-the-marker)). So a change reads pending only
for the collectors whose text uses the changed value and whose value comes
from the changed row, until each is served it: a value no collector's text
uses, or one a higher level overrides, leaves them in sync. A retarget to
another published version with identical bytes reads pending until the
next poll, which reconciles it without a push.

**What may read pending without cause, never in sync without cause**, but
for a version published before migration 00012 (development databases
only, below):

- a stamped change that leaves the filled-in bytes as they were (an
  override equal to the value it hides, a binding to a variable group that
  overrides nothing the text uses) reads pending until the next poll, which
  moves the marker without a push (by design);
- a move into a group of the same variable group whose setting was stamped
  after the collector's last poll reads pending until that next poll, which
  moves the marker without a push;
- a marker capped at the poll statement's counter (above) reads pending
  until the next poll.

The one exception: a version published before migration 00012 has a wanted stamp of 0
([The names a published version references](#the-names-a-published-version-references)),
so after a value's or a binding's change its collectors read in sync until
their next poll pushes the new bytes.

### Liveness

From `last_seen_at`, by the injected clock: `online` under 60 s, `warning`
to 120 s, `unknown` to 300 s, `offline` from then on.
A collector's heartbeat lands at most every 15 s, well inside the first
step.

### The supervisor's statuses

```
no report yet ── RemoteConfig received ──▶ APPLYING ──▶ APPLIED  (in_sync)
                                                    └─▶ FAILED   (failed)
```

APPLYING: it wrote the new configuration and is restarting the collector.
APPLIED: the collector runs the configuration of `last_remote_config_hash`
(subject to the health gate). FAILED: the collector exited on it; the
supervisor usually keeps the previous one running, and
`RemoteConfigStatus.error_message` (or the health's `last_error`) says why.


## The error history

`opamp_server.collector_errors`: one row per FAILED report (and per
health-gated APPLIED), with its message and the hash it failed to apply,
so a transient failure that since recovered stays visible after
`effective_error` clears. The newest 100 per collector, and 30 days at most
(a cleanup under an advisory lock, one replica at a time). Read through the
management API's `GET …/collectors/{c}/errors`. A message is collector
error text: it never reaches a log line or a span.


## The poll's data access

A steady poll is one statement (`store/collectors`' `Poll`): the collector
before its heartbeat, the coalesced heartbeat, what resolves, by its
SHA-256, the collector's and its group's variable groups with their stamps,
and the organization's counter and generation. The YAML and the variables
come from their caches; nothing else is read or written unless something
changed. Enrollment (once per collector), a cache miss (the YAML's or the
variables') and a report each add their own round trip.

### At scale (a measured check)

`BenchmarkTenThousandCollectors` (`handlers/variables_scale_bench_test.go`;
opt-in, with no time threshold) ran three times at 150 variables (the
bound), on a laptop (10 CPUs,
Postgres 18 in Docker) on battery with a load average of 5 to 14 from
other processes, so read these as orders of magnitude, not bounds. **All
three runs were not saturated** (every poll started within `scaleLagLimit`,
100 ms, of its due time; the most late 26, 10 and 21 ms), so every row is
the median of the three runs' p50s (or of the figure named), with their
range in brackets. The fixture: 1 organization and 1 fleet, 20 groups (10
bound to 5 variable groups), 10,000 collectors (100 pinned to a 6th), 150
variables, one published version of 3,528 bytes referencing 10 names
assigned to every group, every collector served and in sync. The poll ran
on **two replicas, each with its own variables cache**, 16 workers at
about 330 polls a second for 60 seconds, and changed `V010`, a value
nothing overrides, at 20 seconds: so the change reached **all 10,000**
collectors, the worst case's push herd.

| What | Measured |
|---|---|
| A used value's save (`UpdateVariable`, 50 times) | p50 5.23 ms (5.06–5.33 ms); p99 9.12–9.72 ms |
| The fleet counts (`SummariesForFleets`, 50 times) | p50 14.37 ms before the change (14.29–15.16 ms), 14.31 ms after (14.19–14.55 ms); its `EXPLAIN ANALYZE` 18.4–22.2 ms |
| A page of 50 (`List`); one collector (`Get`) | `List` p50 3.23 ms before the change (3.20–3.31 ms), 3.23 ms after (3.16–3.30 ms); `Get` p50 1.17 ms before (1.11–1.30 ms), 1.19 ms after (1.17–1.23 ms) |
| After `V001`'s change (it reaches the collectors that don't override it) | 5,050 in sync, 4,950 pending (each run) |
| The poll (29,801 polls a run, all `200`) | steady p50 1.32 ms (1.31–1.37 ms), p99 3.34 ms (2.98–4.58 ms); a cache miss p50 6.15 ms (4.51–7.41 ms; 24 to 25 misses a run, 12 or 13 per replica); a push p50 3.43 ms (3.33–3.53 ms), p99 10.53 ms (9.21–10.90 ms); an `APPLIED` report p50 2.96 ms (2.91–3.06 ms); the herd's window (20–25 s) p99 10.63 ms (9.00–11.58 ms), max 24.42 ms (16.70–29.52 ms); each run 10,000 pushes, 20,000 report writes, 0 full-state asks, 0 error rows, 0 YAML reads, and every collector in sync after |
| An unused value's change | 10,000 polls, 0 pushes, 0 report writes, every collector in sync (each run) |
| The fill-in's CPU per steady poll | 10.5 µs for the 3,528-byte text with 10 references (10.4–11.3 µs; 6.2–6.6 µs of wall time); 436.8 µs for 256 KiB (429.9–461.5 µs); 3.8 µs with no live reference (3.8–4.2 µs) |
| The process's CPU per steady poll, the load generator's included | 311–326 µs |

An earlier set of three runs, at 500 variables and on a more
loaded machine (a load average of 9 to 63), saturated the load generator
in two of them; the check has flagged such a run itself since
(`poll_saturated`, when a poll starts more than `scaleLagLimit` after it
was due: the verdict rests on that lag alone, and a run within it still
says that the machine's load shows in its latencies, with the load average
beside them). Under an unpaced burst the two replicas made 33 to 35
variable reads instead of 24 or 25, since concurrent misses of one key
each read (bounded by the workers). The herd is the cost that dominates:
every collector a change reaches restarts within a heartbeat (30 s by
default); staggered rollouts are later work.
