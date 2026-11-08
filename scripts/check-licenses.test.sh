#!/usr/bin/env bash
# Verifies scripts/check-licenses.sh rejects a disallowed or unknown licence
# and passes an allowed one, and that it holds this module itself to
# Apache-2.0. The fixtures in scripts/testdata are tiny modules with no
# dependencies, each with its own LICENSE, so this needs no network once
# go-licenses is built. (Go leaves testdata out of ./..., so they are never
# part of this module's build.)
set -uo pipefail
cd "$(dirname "$0")/.."
FAILURES=0

OWN_MODULE="github.com/telflo/opamp-server"

scratch=$(mktemp -d "${TMPDIR:-/tmp}/check-licenses-test.XXXXXX")
trap 'rm -rf "$scratch"' EXIT

# expect <pass|fail|fail-own> <description> <module-dir> [policy]
# A failing case must exit 1 (a licence was refused), not 2 (the check
# couldn't run), and must name the reason, so a broken tool can't pass for a
# refusal: "fail" a licence not on the allowed list, "fail-own" the module
# itself not reported as Apache-2.0 (and every licence allowed, so the
# own-module check alone refused it).
expect() {
  local outcome="$1" description="$2" module_dir="$3" policy="${4:-}" output status
  output=$(./scripts/check-licenses.sh "$policy" "$module_dir" 2>&1)
  status=$?
  case "$outcome" in
    pass)
      if [ "$status" -ne 0 ]; then
        echo "FAIL: $description should pass (exit $status):"
        echo "$output" | sed 's/^/    /'
        FAILURES=$((FAILURES + 1))
      fi
      ;;
    fail)
      if [ "$status" -ne 1 ] || ! grep -q "NOT ALLOWED" <<<"$output"; then
        echo "FAIL: $description should be refused (exit $status):"
        echo "$output" | sed 's/^/    /'
        FAILURES=$((FAILURES + 1))
      fi
      ;;
    fail-own)
      if [ "$status" -ne 1 ] || grep -q "NOT ALLOWED" <<<"$output" \
        || ! grep -q "itself are not Apache-2.0" <<<"$output"; then
        echo "FAIL: $description should be refused by the own-module check alone (exit $status):"
        echo "$output" | sed 's/^/    /'
        FAILURES=$((FAILURES + 1))
      fi
      ;;
  esac
}

expect fail "a GPL-3.0 module" scripts/testdata/gpl-module
expect pass "an MIT module" scripts/testdata/mit-module

# No LICENSE at all: go-licenses reports "Unknown", which must be refused.
# Built here rather than committed, so no LICENSE in a parent directory can
# be found for it.
mkdir "$scratch/no-licence"
cp scripts/testdata/mit-module/go.mod scripts/testdata/mit-module/fixture.go "$scratch/no-licence/"
expect fail "a module with no licence" "$scratch/no-licence"

# The policy path is an argument: a policy that allows GPL-3.0 lets the GPL
# fixture through, and one that allows only Apache-2.0 refuses the MIT one.
echo '{ "allowed": ["GPL-3.0"] }' > "$scratch/allow-gpl.json"
expect pass "a GPL-3.0 module under a policy that allows it" scripts/testdata/gpl-module "$scratch/allow-gpl.json"
echo '{ "allowed": ["Apache-2.0"] }' > "$scratch/apache-only.json"
expect fail "an MIT module under a policy without MIT" scripts/testdata/mit-module "$scratch/apache-only.json"

# This module itself must be Apache-2.0: a module named like it with an MIT
# LICENSE passes the allowed list (MIT is allowed) and must still be refused;
# with this repository's LICENSE it passes. Each is the MIT fixture's source
# under the module's name, built here.
own_module() {
  local dir="$1" licence="$2"
  mkdir "$dir"
  sed "s|^module .*|module $OWN_MODULE|" scripts/testdata/mit-module/go.mod > "$dir/go.mod"
  cp scripts/testdata/mit-module/fixture.go "$dir/"
  cp "$licence" "$dir/LICENSE"
}
own_module "$scratch/own-mit" scripts/testdata/mit-module/LICENSE
expect fail-own "$OWN_MODULE with an MIT LICENSE" "$scratch/own-mit"
own_module "$scratch/own-apache" LICENSE
expect pass "$OWN_MODULE with the Apache License 2.0" "$scratch/own-apache"

# A module whose path merely starts with this module's is not this module:
# ${OWN_MODULE}2 with an MIT LICENSE passes on its own, and as a dependency of
# this module (Apache-2.0) its MIT line isn't counted as this module's.
mkdir "$scratch/lookalike"
sed "s|^module .*|module ${OWN_MODULE}2|" scripts/testdata/mit-module/go.mod > "$scratch/lookalike/go.mod"
cp scripts/testdata/mit-module/fixture.go scripts/testdata/mit-module/LICENSE "$scratch/lookalike/"
expect pass "${OWN_MODULE}2 with an MIT LICENSE" "$scratch/lookalike"
mkdir "$scratch/own-with-lookalike"
cat > "$scratch/own-with-lookalike/go.mod" <<EOF
module $OWN_MODULE

go 1.25.5

require ${OWN_MODULE}2 v0.0.0

replace ${OWN_MODULE}2 => ../lookalike
EOF
printf 'package fixture\n\nimport _ "%s"\n' "${OWN_MODULE}2" > "$scratch/own-with-lookalike/fixture.go"
cp LICENSE "$scratch/own-with-lookalike/LICENSE"
expect pass "$OWN_MODULE (Apache-2.0) depending on ${OWN_MODULE}2 (MIT)" "$scratch/own-with-lookalike"

# A missing policy is an error, never a pass.
./scripts/check-licenses.sh "$scratch/no-such-policy.json" scripts/testdata/mit-module >/dev/null 2>&1
status=$?
if [ "$status" -ne 2 ]; then
  echo "FAIL: a missing policy file should exit 2, got $status"; FAILURES=$((FAILURES + 1))
fi

if [ "$FAILURES" -gt 0 ]; then
  echo "$FAILURES licence-check test(s) failed"
  exit 1
fi
echo "All licence-check tests passed"
