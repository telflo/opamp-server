# Contributing

The rules for changing the opamp-server. `README.md` says what the server
does and how to run it; the decision records in `docs/decisions/` say why
it is built the way it is.

## Before you push: `make check`

`make check` runs every gate, in order, and stops at the first failure.
Every gate can also run alone (`make go-vet`; `make help` lists them). The
Go module's gates are:

1. `go mod tidy -diff` and `go mod verify`: `go.mod` and `go.sum` say
   exactly what the code imports, and every downloaded module matches its
   hash;
2. `gofmt -l .` (it must list nothing) and `go vet ./...`;
3. `scripts/check-test-inventory.sh` and its self-test: every test,
   benchmark, example and fuzz target, listed per package, matches
   `scripts/test-inventory.txt`. A commit that adds, renames, moves or
   removes one runs `scripts/check-test-inventory.sh --update` and commits
   the file with it, so a lost test shows in review;
4. `scripts/check-licenses.sh` and its self-test (below);
5. `scripts/check-bootstrap.sh telflo_opamp_bootstrap_test`: the bootstrap,
   run twice, changes nothing the second time;
6. `go test -race -count=1 -json ./...`, and no test may skip itself.

The chart and the scripts add their own gates to the same list.
`make smoke`, not a gate, installs the chart on a throwaway minikube profile.

**It needs a superuser Postgres**, which it never starts itself:
`OPAMP_TEST_ADMIN_DATABASE_URL`, a superuser URL to the server's `postgres`
database, and `PSQL`, the psql command to run as that superuser. Without
either, `make check` refuses to run (exit 2). `docs/testing.md`
gives a throwaway Postgres in one `docker run`.

**Offline.** `make check` leaves `GOPROXY` to you. `go run` of the pinned
go-licenses asks the module proxy whether the tool is deprecated even when
it is already in the module cache (so `GOPROXY=off` fails), so to run the
gates without the network, point the proxy at the cache itself:

```sh
GOPROXY=file://$(go env GOMODCACHE)/cache/download make check
```

That works once every module the gates need has been downloaded once.

## The toolchain

- **Go 1.27.4**, as `go.mod`'s `go` line says, and **`GOTOOLCHAIN=local`
  for every Go command**: the Makefile, the scripts and the image set it,
  and so should you. Without it, a dependency that asks for a newer Go
  makes the `go` command download one, and the build silently stops being
  the one the gates checked. The `go` line changes only by a decision of
  its own.
- **goose stays pinned at v3.29.0.** It moves only by a decision of its
  own, with its migrations' behaviour checked again; Dependabot doesn't
  propose it.
- The image builds on `golang:1.27-alpine` and runs on
  `gcr.io/distroless/static-debian12:nonroot`, each pinned by digest (the
  Dockerfile says how to move them).
- **`gofmt -l .` prints nothing and `go vet ./...` is clean.**
- **`go test -race`**, always.

## Dependencies and licences

- **No dependency without a need for it**, and every one passes the licence
  check. `scripts/check-licenses.sh` runs go-licenses v2.0.1 (through
  `go run`) over every package of the module (`./...`), and checks every
  module it reports against `scripts/license-policy.json`'s `allowed` list:
  licences that allow redistribution. A licence go-licenses can't classify
  fails too, since an unknown licence can't be shown to be redistributable.
  It follows the packages' non-test imports, so it checks what the binaries
  ship: a module only tests import (kin-openapi, for the conformance test)
  isn't checked.
- **The module itself must be Apache-2.0.** The check fails unless the
  report lists `github.com/telflo/opamp-server` under the Apache License
  2.0, which `LICENSE` is, so a root `LICENSE` swapped for another allowed
  licence still fails. `NOTICE` goes with it.
- `scripts/check-licenses.test.sh` shows the check refusing a GPL-3.0
  fixture module, a module with no licence and this module under an MIT
  licence, and passing a module whose path only starts with this one's. It
  runs without `GOFLAGS` (`make go-licenses-test` unsets it): it builds
  scratch modules with no `go.sum`, which a `-mod` flag can make `go`
  refuse for a reason that has nothing to do with licences.
- **Changing the policy** is a decision of its own: add a licence to
  `allowed` only when it allows redistribution of the binary and the image
  without conditions the project can't meet.

## SQL

- **Every statement names the schema**: `telflo_opamp.publications`, never
  `publications`. The runtime role's `search_path` is pinned to
  `telflo_opamp`, but no code relies on it: a statement that works only
  because of a `search_path` breaks for an operator whose server logs in
  under another role, and a stray unqualified name could resolve into
  `public`.
- **Every query takes `organization_id` as its first filter**, and every
  index on an organization's rows leads with it. The organization is the
  only isolation there is: one role serves every organization, with no
  row-level security (`docs/decisions/0002-the-agent-tier.md`), and the Go
  tests call every store method and every management route with a second
  organization's ids. **Two exceptions, each by its nature**:
  `store/enrollmenttokens`' `Lookup`, the agent's authentication, reads a
  token by its id alone (a bearer names its token and fleet, and the row
  then gives the organization; the id is the primary key), and
  `store/collectors`' `Cleanup`, the error history's retention job, deletes
  across organizations (by Postgres 18's skip scan on `(organization_id,
  reported_at)`). A new exception is a decision of its own, named in its
  store's package comment.
- **No foreign key leaves `telflo_opamp`**
  (`docs/decisions/0001-reached-only-through-its-api.md`); a test checks
  the catalog. A caller's other ids (its configurations, versions, people)
  are plain values. **Inside it, every link between an organization's rows
  is a composite key whose first column on both sides is
  `organization_id`**: a group to its fleet, `(organization_id, fleet_id)`;
  a collector to its group, `(organization_id, fleet_id,
  collector_group_id)`, so its group is always its fleet's. Each parent has
  `unique (organization_id, id)` (and `(organization_id, fleet_id, id)`
  where a child names the fleet) as the target. A link across
  organizations, or into another fleet, can't be stored. **Every table also
  has a direct `(organization_id) → organizations(id) on delete cascade`**:
  with it all the cascades queue before any `NO ACTION` check, so deleting
  an organization works in one statement in any trigger order, while
  deleting a published version a group or a collector uses is refused.
  Only that key may lead with the organization alone;
  `TestNoForeignKeyLeavesTheSchemaAndEveryKeyCarriesTheOrganization` pins
  every key by name and definition.
- **Names.** Constraints and indexes are `uq_<table>_<columns>`,
  `fk_<table>_<what it references>`, `ck_<table>_<what it checks>` and
  `idx_<table>_<columns>`. Every index on an organization's rows leads
  with `organization_id`, so an index's name leaves that column out:
  `idx_<table>_<the columns after organization_id>`
  (`idx_collector_instances_fleet_id_name`), and so does a partial unique
  index's (`uq_collector_groups_fleet_id_where_is_default`). A unique
  constraint's name lists its columns whole, the organization included, as
  the composite keys' targets must (`uq_fleets_organization_id_id`). Every
  name is at most 63 bytes: Postgres silently cuts a longer one, and its
  `comment on` then goes to the cut name.
  `idx_publications_organization_id_configuration_id`, from the first
  migration, predates the rule and keeps its name.
- **An idempotent insert keyed by the caller's id is a targetless `on
  conflict do nothing`** on a table with a composite unique beside its
  primary key: `on conflict (id)` names one arbiter, so a race on the other
  unique raises `23505` (a `500`) instead of doing nothing. Do nothing on
  any conflict, then read the row and classify it (the same row, another,
  another organization's), with a race test.
- **The lock order is organization → fleet → group → collector or token.**
  A transaction that inserts into or locks a fleet-owned row first takes
  the organization's row `for key share` (an enrollment takes it `for no
  key update`, to count the cap), then the fleet's `for key share`, then
  the group, then the collectors (`order by id`) or the token. **The two
  exceptions to the fleet's `for key share`**: `store/enrollmenttokens`'
  `Rotate` and `IssueDefault` take the fleet's row `for no key update`, so
  two rotations of one fleet serialize (the second reads the first's new
  token as current), and so do two issues of a new default token and an
  issue and an archive; a collector's or a token's `for key share` on the
  fleet isn't blocked by it. A write that skips a step deadlocks (`40P01`)
  against a fleet's or an organization's delete; each pair has a race
  test, in `store/collectors/lock_order_test.go` and `races_test.go`,
  `store/collectorgroups/collectorgroups_test.go` and
  `store/enrollmenttokens/enrollmenttokens_test.go` (the two rotations'
  race too, and an issue racing its fleet's or its organization's delete
  in both orders).
- **Organization variables take the organization's row `for no key
  update` first**: every write that moves the organization's variables
  counter or reads what the guards read (the undefined check, the in-use
  guards), so the counter's values are ordered and a guard's answer stands
  until its transaction ends (`store/variableuse`'s `LockOrganization`,
  then `Bump`): a variable's, a variable group's or an override's write,
  and the fleet stores' assignments of a published version, pins, moves
  and variable-group bindings. **A write chooses its lock mode from the
  changes it was asked for, before its first lock, and never upgrades a
  `for key share` to `for no key update` in mid-transaction**, even when
  the rows it then reads show nothing would change: that upgrade deadlocks
  three ways, with an enrollment's `for no key update` and a variable
  write. collectorgroups' Create takes `for no key update` when it names an
  assigned published version or a variable group, and its Update when it
  sets an assigned version or sets or clears the variable group (a rename,
  annotations or an assignment cleared keep `for key share`); collectors'
  Update takes it for a pin, a pinned variable group or a move (a rename
  keeps `for key share`), and Pin, Move and the bulk pin always. Then the
  fleet's `for key share`, the group, the collector. An enrollment and a
  variable write serialize on the organization's row: that is contention,
  not a cycle. `store/collectors/lock_order_test.go`'s
  `TestNoWriteUpgradesItsOrganizationLock` holds it: each write's first
  lock is the mode above, and three sessions (a group's Update naming a
  variable group, an enrollment, a variable's value change) held at each
  lock in turn all finish, none with a `40P01` (no write on those paths
  retries one, so a deadlock fails the test).
- **The deletes lock what a single statement locks first.** A poll or a
  report is one statement that locks a collector's row and then key-shares
  the token or published version its changed key names, and a cascade
  reaching the collector after it deleted those would deadlock with it. So
  an organization's delete takes the organization `for update`, then its
  collectors `for update` `order by id`, then deletes; a fleet's, after the
  organization's `for key share`, takes the fleet `for update` and its
  collectors likewise. Each runs again on a `40P01`, at most three times,
  as a backstop (`WithDeadlockRetries(0)` in the race tests,
  `store/collectors/delete_races_test.go`, so a cycle fails them rather
  than being retried away).
- **A unique name is looked for, never raised**: a name or a label taken
  is found with `on conflict do nothing` or `where not exists` under the
  locks the write already holds, since a unique key's violation makes
  Postgres log the value in its `DETAIL`; only a true race reaches the key,
  still answered as taken (`store/collectors/taken_names_test.go` records
  every refused statement). The agent side answers a deadlock `503` with
  `Retry-After`.

### Migrations

goose SQL files in `migrations/`, named `NNNNN_<what>.sql` and embedded in
the binary, each with `-- +goose Up` and a working `-- +goose Down`, each
run in a transaction:

- **every migration keeps working with the previous release (expand, then
  contract)**, because its servers keep serving while it goes in: add a
  column as nullable or with a value the old code doesn't need, and drop
  what the old code reads only in a later release;
- **an applied migration is never edited.** Two edits in place were made
  before any production install; there will be no more;
- Down revokes every grant its Up made;
- grants to `telflo_opamp_runtime` are explicit and minimal (no `update`
  on an immutable table), and
  `TestRuntimeRolePrivilegesAreExactlyWhatTheServerNeeds` holds them, so a
  new grant changes that test in the same commit;
- no column defaults to `now()` (see "Time");
- **goose splits a file into statements at every line that ends in `;`,
  even inside a string literal or a `comment on` text**: a comment whose
  line ends with a semicolon becomes two broken statements. End such a
  line otherwise;
- names follow "Names" above, and, as `00001` does, a `comment on` each
  table, column and index says why it is there.

**`migrate` runs only as the database's owner, `serve` never as it.**
Whoever runs a migration owns what it creates, so it must always be the
same role; `serve` refuses any role that could act above its grants
(superuser, `BYPASSRLS`, the owner or a member of it, an owner of anything
in the schema).

**`deploy/bootstrap.sql`** is idempotent: a second run changes nothing but
the two passwords. `scripts/check-bootstrap.sh` holds it to that.

## Time

**Time comes from an injected clock**, `func() time.Time`: never
`time.Now()` inside a handler or a store, and never SQL `now()`. Only
`serve` passes `time.Now`; tests pass their own. Stored times are kept to
the millisecond (`publications.Precision`), the precision replies carry,
so a client that sends back a time it was given gets the same instant.

**Two waits use the wall clock, by their nature**: each bounds how long
something waits, and neither is a time recorded or compared with one.

- **The body's read deadline** (`middleware/body_limit.go`,
  `BodyReadTimeout`, 5 s): a socket's read deadline is the kernel's, set
  with `http.ResponseController.SetReadDeadline(time.Now().Add(…))`; an
  injected clock can't move it.
- **A delete's own bound** (`management/deletes.go`, `DeleteTimeout`, 5
  minutes): an organization's or a fleet's delete runs on
  `context.WithoutCancel` of its request, so a caller that hangs up doesn't
  roll it back, under `context.WithTimeout`, a timer's; serve's forced stop
  still cancels it. The tests set the bound short and hold a lock, so only
  the bound can end the delete.

Anything else that waits takes its timer from the caller: `serve`'s
shutdown drain (`OPAMP_SHUTDOWN_DRAIN_SECONDS`) waits on a timer the
replica holds (`time.After` in `serve`, a test's channel in a test).

## Secrets and what is never written down

**No secret, `Authorization` header, YAML, request or response body, or URL
that can carry a password** goes into a log line, an error message, a span
attribute, a span status or a span event. The secrets are the management
secret and its previous secrets, `OPAMP_ENCRYPTION_KEY` and its previous
keys, enrollment tokens' bearers and the database passwords. In practice:

- an error names the variable or the field, never its value; a connection
  URL that doesn't parse is reported without it (pgx's own error quotes
  it);
- a store wraps its errors with the operation's name and adds no argument,
  and nothing passes a failed call's error text on: a failed request logs
  the route and the SQLSTATE, and a database span's status is the
  operation and the SQLSTATE, with no recorded exception, since a driver's
  error text can quote a SQL argument;
- a request's log line and span name the route's pattern, never the path
  (which holds ids) or the query; a method that isn't a standard one is
  `_OTHER`;
- the fleet model's text is configuration text too: a collector's reported
  attributes' values and error messages, its effective configuration, a
  fleet's label, a group's or a collector's name, an organization
  variable's value. None reaches a log line, an error message, a span or
  an audit row; the organization's, fleet's, token's and collector's ids
  may appear in log fields;
- a panic is logged by its value's type, never the value;
- an error that could quote a telemetry endpoint isn't logged;
- a password never goes on a command line, where `ps` shows it: the
  bootstrap reads its passwords from the environment, and `docker exec -e
  NAME` (bare) passes them.

Secrets are compared as SHA-256 digests with `subtle.ConstantTimeCompare`,
against every accepted secret with no early exit
(`management/secrets.go`). A test reads the package's source to hold that
no other comparison touches a secret (`constant_time_test.go`). An API
key's secret is compared the same way, as its SHA-256 against the stored
one (`store/apikeys/format.go`, `Matches`).

**Tests hold these rules** by capturing what the server logs and records,
planting a canary in each kind of secret and configuration text, and
asserting by boolean that it is absent (`strings.Contains` and the like):
never by comparing, diffing or printing a value that could hold a secret,
so a failing test can't print one either. A secret a test needs is made at
run time, never committed (the fixtures under `tokens/testdata` are test
keys only).

## Code shape

- **Components and the process.** `agent` (the agent endpoint: its
  settings, its middleware stack around `handlers`' poll handler, its
  routes) and `management` (the management API) are components: each is
  built from its settings and stores into an `http.Handler`, and holds no
  process code. `serve` is the process: the shared settings (the database
  URL, the encryption keys, the drain), the pools, telemetry, readiness,
  the cleanup job, the listeners' lifecycle, and which components a role
  runs (`serve`, `serve agent`, `serve management`). Neither component
  imports the other or `serve`. What two of them share lives under
  `internal/`: `envconfig` (reading settings), `shutdown` (the forced stop a
  request's context carries), `diagnostics` (the profiles and the admission
  view) and `apierror` (the error envelope). Tests that act as a supervisor
  or set data up through the API from another package use `agent/agenttest`
  and `management/managementtest`, which speak only HTTP and protobuf.
- **Stores**: a package under `store/` for each (`organizations`,
  `publications`, `fleets`, `collectorgroups`, `enrollmenttokens`,
  `collectors`, and `variables`, an organization's variables, variable
  groups and overrides), each an interface and its Postgres implementation
  (pgx). `store/storetest` runs the contract tests against them, and handler
  tests run on them too, each test on a fresh copy of its package's
  template database (`databasetest`). A handler test that needs a store to
  fail or record its calls wraps the Postgres one, or stands in a small fake
  of the interface; there is no in-memory implementation to keep in step.
  `store/orgvariables` holds no rows: it is the fill-in (the
  one scanner, held to `handlers/testdata/org-token-corpus.json`, which
  other implementations of the fill-in can test against too) and the
  stamps' rule; `store/variableuse` is the helpers the variables store and
  the fleet stores share in the caller's transaction (the organization's
  lock, the counter's bump, the guards' reads), so the fleet stores never
  import `store/variables`.
- **Caches only for speed, each with a stated staleness bound**
  (`docs/decisions/0002-the-agent-tier.md`): replicas share nothing but the
  database, so a cache is never the truth. Each is `internal/lru`, bounded
  in entries (and bytes where its values vary), on the injected clock, and
  its bound is written beside it: a token row ≤ 10 s from the time its read
  began (`PutAt`), an organization's limits ≤ 10 s; the YAML by
  organization and SHA-256, a miss never cached; an organization's
  variables by organization, its generation, its counter and the
  collector's two variable groups, exact because every write that can
  change filled-in text moves the counter, which the poll reads in its own
  statement (`docs/STATE_MODEL.md`). A per-process map that grows with
  callers (a limiter's keys) is bounded and swept.
- **`main` only dispatches** subcommands. Each takes its context, the
  environment as `func(string) string` and its log's writer, so tests run
  it in-process. `serve` is wired by hand, with no DI container.
- **Logs are `log/slog` JSON on stdout**, never a file: the distroless
  image runs as a non-root user that can't write beside the binary.
- **The API's contract is `api/openapi.yaml`.** The conformance test
  (kin-openapi, in the handler's package) sends every documented request
  through the real handler, validates request and reply against the file,
  and fails on a registered route the file doesn't document. A new route
  is documented in the same commit. kin-openapi is a test-only dependency.
- **Write an invisible character in `api/openapi.yaml` as a `\uXXXX`
  escape**, as its patterns do: editors offer to strip some of them (VS
  Code's "unusual line terminators", U+2028 and U+2029), which would
  change what a pattern accepts.

## Tests

- **Tests that need Postgres use `store/database/databasetest`**:
  - one setting, `OPAMP_TEST_ADMIN_DATABASE_URL`, a superuser URL to the
    server; the harness derives the owner's and the runtime role's URLs;
  - one database per package, `telflo_opamp_<package>_test`, since `go
    test ./...` runs packages in parallel; `databasetest.New` drops and
    recreates it for each test that asks, as a copy of the package's
    template (`telflo_opamp_<package>_template_test`, migrated once per
    test process, then copied with `create database … template`), and
    refuses any name not ending in `_test`. A copy doesn't carry the
    database's own privileges, so the harness sets them on every copy, as
    `deploy/bootstrap.sql` does;
  - the three roles are created in Go when missing, with the harness's
    development passwords: roles belong to the whole server, so never
    point the tests at a server whose roles have real passwords;
  - a test that needs a role of its own (a superuser, a `BYPASSRLS` role)
    makes it with `ThrowawayRole`: uniquely named, dropped at the end.
- **Skipped only when `OPAMP_TEST_ADMIN_DATABASE_URL` is unset, and `make
  check` fails on any test-level skip**: it runs `go test -json` and fails
  on a `skip` that names a test. A package with no test files also reports
  a skip; that one doesn't count.
- **The Go suite never runs at the same time as another database-backed
  suite** on the same Postgres server: they share its roles.
- **A security-bearing test shows that it fails when its behaviour
  breaks.** Usually that is a deliberate break, made and undone in a
  scratch copy, never in your checkout. Where the harness recreates the
  database for every test, a break made by hand doesn't survive to the
  next run, so **the break becomes a permanent detector test**: a test
  that applies the break to its own database and asserts the real check,
  through the same helper, sees it. For example,
  `TestPrivilegeCheckSeesAGrantedUpdate` grants `update` and
  `TestForeignKeyCheckSeesAForeignKeyIntoPublic` adds a foreign key into
  `public`; `TestTheConstantTimeScanSeesAnEarlyReturnAndAPlainComparison`
  does the same for the source scan.
- **Timing tests bound operation counts**, never milliseconds.
- **A test that names a migration derives its number from
  `LatestMigration()`** (±1), never a literal: a test that wrote "the
  schema ahead is version 4" hung once a real `00004` existed (`serve`
  started, and nothing stopped it). A test that waits for a refusal has a
  hang guard.
- **The bootstrap is checked by a script, not a Go test**:
  `scripts/check-bootstrap.sh` runs `deploy/bootstrap.sql` twice and diffs
  a catalog dump of the roles, their memberships, the database's owner and
  ACL and the role settings. It drops its database only when the name ends
  in `_test`.

## Comments

A comment says what the code guarantees and why, in plain sentences:
every invariant, bound and ordering rule, and the reason for it, but not a
restatement of what the next lines plainly do. It cites nothing a reader
of this repository can't open, as a commit message doesn't: no decision
number, ruling, ticket or task of another project, and no history of where
the code came from (that's git's). A rule too long for a comment goes in
`docs/decisions/` as a short ADR, and the comment cites that file by its
path.

## Commits

- **Plain messages**: a short subject saying what the change does, then,
  where it helps, what changed and why, in plain words. A message cites
  nothing a reader of this repository can't open: no private tracker,
  document, discussion or path outside the repository.
- One change per commit where you can, with its tests and docs in the same
  commit (a new route and its `openapi.yaml` entry; a new grant and the
  privileges test).
- `make check` passes on every commit you push.
- Never commit a secret: development values live only in `compose.yaml`,
  which is for development and public.

## Releasing

A release is one commit and one version: the image
`ghcr.io/telflo/opamp-server:<version>`, the chart
`oci://ghcr.io/telflo/charts/opamp-server` at `<version>`, and the
binaries on the GitHub Release `v<version>`, all built from that commit.
The image's tag, the chart's `version` and its `appVersion` are the same
number, and a published version is never pushed again: a fix is a new
version. Below, `0.2.0` stands for the new version.

**1. The version, through a pull request.** On a branch, set `version`
and `appVersion` in `charts/opamp-server/Chart.yaml` to the new version,
the two equal, and the `helm install … --version` in `docs/kubernetes.md` and
`charts/opamp-server/README.md` (the chart's render tests read the version
from Chart.yaml). If the release adds a migration, `docs/upgrading.md`
names the schema it brings a database to. In `CHANGELOG.md`, add the
version's entry, with its date. Run `make
check` and `make smoke` (the chart and the image from your tree, on a
throwaway minikube profile), then commit:

```sh
git commit -m "Release 0.2.0" -- charts/opamp-server/Chart.yaml docs/kubernetes.md charts/opamp-server/README.md CHANGELOG.md
```

The release commit lands on `main` through a pull request, reviewed like
any other change: never a direct push to `main`.

**2. The tag**, after the merge: annotated, on the commit of `main` that
holds the new version, with the message `opamp-server <version>`. Only the
tag is pushed:

```sh
git switch main
```

```sh
git pull --ff-only origin main
```

```sh
git tag -a v0.2.0 -m "opamp-server 0.2.0"
```

```sh
git push origin v0.2.0
```

**3. The release workflow** (`.github/workflows/release.yml`) starts on
the tag. It refuses a tag that isn't annotated or doesn't match the chart's
version, and runs every gate on the tag's commit. Once they pass, it
publishes, with no further step: pushing the tag is the decision to
release. It publishes the image for linux/amd64 and linux/arm64, then the chart, then the binaries
(`.goreleaser.yaml`) and the GitHub Release that holds them. The run's
summary names the image index's digest, the one a deployment pins.

If it stops part way, fix the cause on `main` and run it again from the
Actions tab ("Run workflow", naming the tag): what is published already is
skipped, never pushed again.

**4. The check of what was published.** It pulls the image and the chart,
loads the image into a throwaway minikube profile and installs the pulled
chart. Both packages are public, so the Docker and Helm configurations it
reads through hold no login, only a placeholder (so no credential helper,
such as the macOS keychain's, is asked):

```sh
export RELEASE_CHECK="$(mktemp -d)" && mkdir "$RELEASE_CHECK/docker"
```

```sh
printf '{"auths":{"placeholder.invalid":{}},"cliPluginsExtraDirs":["%s/.docker/cli-plugins"]}\n' "$HOME" >"$RELEASE_CHECK/docker/config.json"
```

```sh
printf '{"auths":{"placeholder.invalid":{}}}\n' >"$RELEASE_CHECK/registry.json"
```

```sh
DOCKER_CONFIG="$RELEASE_CHECK/docker" HELM_REGISTRY_CONFIG="$RELEASE_CHECK/registry.json" scripts/smoke-minikube.sh --published 0.2.0
```

```sh
rm -rf "$RELEASE_CHECK" && unset RELEASE_CHECK
```

The Docker configuration names `~/.docker/cli-plugins` as a plugins folder
(`cliPluginsExtraDirs`): a per-user Docker Desktop install keeps `docker
buildx` there, and Docker stops looking in it once `DOCKER_CONFIG` points
elsewhere.
