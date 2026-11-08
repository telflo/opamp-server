#!/usr/bin/env bash
# No test disappears unnoticed: the module's tests, benchmarks, examples and
# fuzz targets, listed per package, must match scripts/test-inventory.txt.
# A change that adds, renames, moves or removes one updates that file in the
# same commit (--update), so every change to the set of tests shows in
# review.
#
# Usage: scripts/check-test-inventory.sh [--update] [MODULE_DIR [INVENTORY]]
#   --update    rewrite INVENTORY from the module instead of checking it
#   MODULE_DIR  default: the repository's root
#   INVENTORY   default: MODULE_DIR/scripts/test-inventory.txt
#
# It lists with `go test -list`, which compiles each package's tests and runs
# none: no database, no network once the modules are downloaded. Exit 0 when
# the lists match (or after --update), 1 when they differ, printing each
# test added (+) or missing (-), 2 when it can't run. bash 3.2 compatible.
set -uo pipefail

update=false
if [ "${1:-}" = "--update" ]; then
  update=true
  shift
fi
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
MODULE_DIR="$(cd "${1:-$REPO_DIR}" && pwd)" || exit 2
INVENTORY="${2:-$MODULE_DIR/scripts/test-inventory.txt}"

scratch=$(mktemp -d "${TMPDIR:-/tmp}/check-test-inventory.XXXXXX") || exit 2
trap 'rm -rf "$scratch"' EXIT

# go test prints each package's names, then that package's "ok" line, one
# package at a time; each name is written out with the package the "ok"
# line names, as "<package> <name>".
if ! (cd "$MODULE_DIR" && go test -list '.*' ./...) >"$scratch/listing" 2>"$scratch/errors"; then
  echo "FAIL: go test -list couldn't list the tests:"
  sed 's/^/    /' "$scratch/errors" "$scratch/listing"
  exit 2
fi
awk '
  /^(Test|Benchmark|Example|Fuzz)/ { names[++n] = $1; next }
  /^ok[ \t]/ { for (i = 1; i <= n; i++) print $2, names[i]; n = 0 }
' "$scratch/listing" | LC_ALL=C sort >"$scratch/current"
if [ ! -s "$scratch/current" ]; then
  echo "FAIL: go test -list found no tests in $MODULE_DIR"
  exit 2
fi

if $update; then
  cp "$scratch/current" "$INVENTORY" || exit 2
  echo "PASS: wrote $(wc -l <"$INVENTORY" | tr -d ' ') tests to $INVENTORY"
  exit 0
fi
if [ ! -f "$INVENTORY" ]; then
  echo "FAIL: $INVENTORY is missing; create it with $0 --update"
  exit 2
fi

LC_ALL=C sort "$INVENTORY" >"$scratch/recorded"
LC_ALL=C comm -13 "$scratch/recorded" "$scratch/current" | sed 's/^/+ /' >"$scratch/added"
LC_ALL=C comm -23 "$scratch/recorded" "$scratch/current" | sed 's/^/- /' >"$scratch/missing"
if [ -s "$scratch/added" ] || [ -s "$scratch/missing" ]; then
  echo "FAIL: the tests differ from $INVENTORY (+ new, - missing):"
  cat "$scratch/added" "$scratch/missing" | sed 's/^/    /'
  echo "If the change is intended, run $0 --update and commit the file with it."
  exit 1
fi
echo "PASS: $(wc -l <"$scratch/current" | tr -d ' ') tests match $INVENTORY"
