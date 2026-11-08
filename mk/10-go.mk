# The Go module's gates, in the order `make check` runs them.

# go.mod's `go` line is the toolchain: never let a go command download a
# newer one.
export GOTOOLCHAIN := local

# go test's -json report, read by go-test's failing-output and no-skip steps.
GO_TEST_REPORT := $(CHECK_DIR)/go-test-report.json

# The output of each failing test and package, from the report.
GO_TEST_FAILED_OUTPUT := (map(select(.Action == "fail") | [.Package, (.Test // "")])) as $$failed | .[] | select((.Action == "output" and ([.Package, (.Test // "")] | IN($$failed[]))) or .Action == "build-output") | .Output
# Tests that skipped themselves. Only test-level skips count: go test also
# reports a "skip" for every package that has no test files.
GO_TEST_SKIPPED := select(.Action == "skip" and .Test != null) | "\(.Package) \(.Test)"
GO_TEST_PASSED := select(.Action == "pass" and .Test != null) | .Test

GO_CHECK_TARGETS := go-tidy go-verify go-fmt go-vet go-staticcheck go-test-inventory go-test-inventory-test go-licenses go-licenses-test go-bootstrap go-test
CHECK_TARGETS += $(GO_CHECK_TARGETS)

.PHONY: $(GO_CHECK_TARGETS)

# Each tool is pinned and run with `go run`, as go-licenses is.
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.2
GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.9.0

# govulncheck is not a gate (not in CHECK_TARGETS), and CI runs it beside
# `make check`: its answer changes as advisories are published, so a commit
# that passed it can fail it later. It fails on a vulnerability the code
# reaches; one only in a module it requires is listed and passes.
.PHONY: go-vulncheck
go-vulncheck:
	go run $(GOVULNCHECK) ./...

# go.mod and go.sum say exactly what the code imports: fails, printing the
# diff, when `go mod tidy` would change them.
go-tidy:
	go mod tidy -diff

# Every downloaded module matches go.sum's hashes.
go-verify:
	go mod verify

go-fmt:
	@set -euo pipefail; \
	unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
	  echo "gofmt would change these files:"; \
	  echo "$$unformatted"; \
	  exit 1; \
	fi

go-vet:
	go vet ./...

# staticcheck's default checks. A file that breaks one on purpose says why
# in a //lint:file-ignore directive.
go-staticcheck:
	go run $(STATICCHECK) ./...

# The module's tests, listed per package, match scripts/test-inventory.txt: a
# test that disappears, or appears, without the file changing with it fails.
go-test-inventory:
	./scripts/check-test-inventory.sh

# The self-test builds a scratch module with no go.sum: without GOFLAGS, as
# go-licenses-test.
go-test-inventory-test:
	env -u GOFLAGS ./scripts/check-test-inventory.test.sh

# Every Go module this one builds on allows redistribution, and the module
# itself is Apache-2.0 (CONTRIBUTING.md's licence rules).
go-licenses:
	./scripts/check-licenses.sh

# The self-test runs without GOFLAGS: it builds scratch modules with no
# go.sum, and a -mod flag set in the environment (say -mod=readonly) can make
# go refuse them for a reason that has nothing to do with licences.
go-licenses-test:
	env -u GOFLAGS ./scripts/check-licenses.test.sh

# deploy/bootstrap.sql, run twice by the superuser: the second run must change
# nothing. Its database is a throwaway (`_test`), dropped when the script
# exits; the roles stay for the Go tests.
go-bootstrap: require-postgres
	./scripts/check-bootstrap.sh telflo_opamp_bootstrap_test

# The tests, with -json so skips can be seen; a failing test's output is
# printed from the report. A test that skips itself (say, one that finds no
# database) would pass silently, so any skipped test fails the gate.
go-test: require-postgres
	@set -euo pipefail; \
	mkdir -p $(CHECK_DIR); \
	status=0; \
	go test -race -count=1 -json ./... > $(GO_TEST_REPORT) || status=$$?; \
	if [ "$$status" -ne 0 ]; then \
	  echo "go test failed; the output of each failing test and package:"; \
	  jq -rs '$(GO_TEST_FAILED_OUTPUT)' $(GO_TEST_REPORT); \
	  exit "$$status"; \
	fi; \
	skipped=$$(jq -r '$(GO_TEST_SKIPPED)' $(GO_TEST_REPORT)); \
	if [ -n "$$skipped" ]; then \
	  echo "These tests were skipped; make check runs every test:"; \
	  echo "$$skipped"; \
	  exit 1; \
	fi; \
	passed=$$(jq -r '$(GO_TEST_PASSED)' $(GO_TEST_REPORT) | wc -l | tr -d ' '); \
	echo "go test: $$passed tests and subtests passed, none skipped (report: $(GO_TEST_REPORT))."
