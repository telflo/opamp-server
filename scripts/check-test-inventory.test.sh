#!/usr/bin/env bash
# Verifies scripts/check-test-inventory.sh passes a module whose tests match
# its inventory, and refuses (exit 1, naming the test) one that lost a test
# or gained one, and that --update makes it pass again. The module is a
# scratch one with no dependencies, so this needs no network.
set -uo pipefail
cd "$(dirname "$0")/.."
FAILURES=0

scratch=$(mktemp -d "${TMPDIR:-/tmp}/check-test-inventory-test.XXXXXX")
trap 'rm -rf "$scratch"' EXIT
module="$scratch/module"
inventory="$scratch/inventory.txt"
mkdir -p "$module/a" "$module/b"
printf 'module example.com/inventory\n\ngo 1.21\n' >"$module/go.mod"
printf 'package a\n\nimport "testing"\n\nfunc TestOne(t *testing.T) {}\n\nfunc TestTwo(t *testing.T) {}\n' >"$module/a/a_test.go"
printf 'package b\n\nimport "testing"\n\nfunc BenchmarkThree(b *testing.B) {}\n' >"$module/b/b_test.go"

# expect <status> <description> <text the output must hold> [--update]
expect() {
  local want="$1" description="$2" holds="$3" output status
  shift 3
  output=$(GOFLAGS= GOTOOLCHAIN=local ./scripts/check-test-inventory.sh "$@" "$module" "$inventory" 2>&1)
  status=$?
  if [ "$status" -ne "$want" ] || ! grep -qF -- "$holds" <<<"$output"; then
    echo "FAIL: $description: want exit $want and \"$holds\", got exit $status:"
    echo "$output" | sed 's/^/    /'
    FAILURES=$((FAILURES + 1))
  fi
}

expect 2 "no inventory yet" "is missing"
expect 0 "--update writes the inventory" "wrote 3 tests" --update
expect 0 "the tests match" "3 tests match"
rm "$module/b/b_test.go"
expect 1 "a removed test is refused" "- example.com/inventory/b BenchmarkThree"
printf 'package b\n\nimport "testing"\n\nfunc BenchmarkThree(b *testing.B) {}\n\nfunc TestFour(t *testing.T) {}\n' >"$module/b/b_test.go"
expect 1 "an added test is refused" "+ example.com/inventory/b TestFour"
expect 0 "--update accepts it" "wrote 4 tests" --update
expect 0 "and the tests match again" "4 tests match"

if [ "$FAILURES" -ne 0 ]; then
  echo "check-test-inventory self-test: $FAILURES failure(s)"
  exit 1
fi
echo "check-test-inventory self-test: PASS"
