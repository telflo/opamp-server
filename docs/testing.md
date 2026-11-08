# Running the tests

`make check` runs every gate this repository has: the Go module's (tidy,
verify, gofmt, vet, the licence check and its self-test, the bootstrap
check, `go test -race` with no skipped test) and those of the chart and the
scripts. It needs a **superuser** Postgres it never starts itself; for
example, a throwaway one:

```sh
docker run -d --name opamp-test-postgres -e POSTGRES_PASSWORD=opamp-test-local \
  -p 127.0.0.1:55433:5432 postgres:18
export OPAMP_TEST_ADMIN_DATABASE_URL=postgresql://postgres:opamp-test-local@127.0.0.1:55433/postgres
export PSQL="docker exec -i -e OPAMP_OWNER_PASSWORD -e OPAMP_RUNTIME_PASSWORD opamp-test-postgres psql -U postgres"
make check
```

[`CONTRIBUTING.md`](../CONTRIBUTING.md) says what each gate checks, and how to
run them offline.

`OPAMP_TEST_ADMIN_DATABASE_URL` is a superuser URL to a Postgres server.
From it the tests make, for each package, a database named
`telflo_opamp_<package>_test`, dropped and created again for every test
that needs one, so packages running in parallel never share one. Each is a
copy of `telflo_opamp_<package>_template_test`, migrated once per run of the
package's tests, so a test doesn't wait for the migrations. Only a
database whose name ends in `_test` is ever created or dropped. The three
roles are created if missing, with the test harness's **development
passwords** (`telflo-opamp-owner-local`, `telflo-opamp-local`): roles belong
to the whole server. So never point the tests at a server whose roles have
real passwords. The refusal tests make throwaway roles with unique names and
drop them.

Without the variable, the tests that need Postgres skip, and `make check`
fails on any skipped test. `scripts/check-bootstrap.sh` runs the bootstrap
twice and fails if the second run changed anything.

`handlers`' `BenchmarkTenThousandCollectors` (a measured check of 10,000
collectors, [`docs/STATE_MODEL.md`](STATE_MODEL.md)) never runs in
`go test ./...`: it needs `OPAMP_SCALE_CHECK_DIR`, a folder for its results
and EXPLAIN plans, and `OPAMP_TEST_ADMIN_DATABASE_URL`, and runs, without
`-race`, as

```sh
OPAMP_SCALE_CHECK_DIR=<folder> OPAMP_TEST_ADMIN_DATABASE_URL=<superuser URL> \
GOTOOLCHAIN=local go test -run '^$' -bench TenThousand -benchtime 1x ./handlers/
```
