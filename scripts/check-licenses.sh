#!/usr/bin/env bash
# Fails if any Go module this module builds on carries a licence that does
# not allow redistribution, or if the module itself is not reported as
# Apache-2.0. The rules are CONTRIBUTING.md's licence section.
#
# Usage: scripts/check-licenses.sh [license-policy.json] [module-dir]
#
#   license-policy.json  the allowed list; defaults to scripts/license-policy.json
#                        in this repository.
#   module-dir           the Go module to check; defaults to this repository.
#                        The self-test points it at its fixtures.
#
# Runs go-licenses (pinned, via go run) over every package in the module
# (./...), the module's own packages included. go-licenses follows the
# packages' non-test imports only, so it checks what the binaries ship: a
# module that only tests import (kin-openapi, for the OpenAPI conformance
# test) is never shipped, and isn't checked here. Every module it reports
# must carry a licence on the policy's "allowed" list; one that go-licenses
# can't classify ("Unknown") fails too, since an unknown licence can't be
# shown to be redistributable. Only the policy's "allowed" list is read.
#
# The module itself: when the checked module is github.com/telflo/telflo-opamp-server
# (`go list -m`), every line the report holds for it must say Apache-2.0, and
# there must be at least one. Without this, a root LICENSE swapped for another
# allowed licence (MIT, say) would pass. Checking this repository (no
# module-dir argument), the module must be github.com/telflo/telflo-opamp-server.
#
# Exits 0 when every licence is allowed and the module is Apache-2.0, 1 when
# either isn't, 2 when the check itself can't run.
set -euo pipefail

GO_LICENSES="github.com/google/go-licenses/v2@v2.0.1"
OWN_MODULE="github.com/telflo/telflo-opamp-server"
OWN_LICENCE="Apache-2.0"
# go.mod's toolchain line is the one every developer uses; never let go run
# download a newer one.
export GOTOOLCHAIN=local

service_dir=$(cd "$(dirname "$0")/.." && pwd)
policy=${1:-$service_dir/scripts/license-policy.json}
module_dir=${2:-$service_dir}

if [ ! -f "$policy" ]; then
  echo "ERROR: no licence policy at $policy" >&2
  exit 2
fi
if [ ! -f "$module_dir/go.mod" ]; then
  echo "ERROR: no go.mod in $module_dir" >&2
  exit 2
fi

if ! allowed=$(jq -er '.allowed[]' "$policy"); then
  echo "ERROR: $policy has no \"allowed\" list" >&2
  exit 2
fi

if ! module=$(cd "$module_dir" && go list -m); then
  echo "ERROR: go list -m failed in $module_dir" >&2
  exit 2
fi

# go-licenses logs warnings and download progress on stderr; keep them out
# of the way unless the tool itself fails.
stderr_log=$(mktemp "${TMPDIR:-/tmp}/check-licenses.XXXXXX")
trap 'rm -f "$stderr_log"' EXIT
if ! report=$(cd "$module_dir" && go run "$GO_LICENSES" report ./... 2>"$stderr_log"); then
  cat "$stderr_log" >&2
  echo "ERROR: go-licenses failed in $module_dir" >&2
  exit 2
fi
if [ -z "$report" ]; then
  # Nothing reported means nothing was checked: never a pass.
  cat "$stderr_log" >&2
  echo "ERROR: go-licenses reported no modules in $module_dir" >&2
  exit 2
fi

# Each report line is "module,licence URL,licence". A module with several
# licence files gets a line for each, and every one must be allowed. Without
# a root LICENSE, go-licenses reports each of the module's packages on its
# own line (as "Unknown"), so the module's lines are its path and the paths
# under it.
disallowed=0
own_lines=0
own_wrong=0
echo "Go module licences (${module_dir}):"
while IFS=, read -r name _url licence; do
  mark=""
  if ! grep -qxF -- "$licence" <<<"$allowed"; then
    mark="  <- NOT ALLOWED"
    disallowed=$((disallowed + 1))
  fi
  if [ "$module" = "$OWN_MODULE" ]; then
    case "$name" in
      "$OWN_MODULE" | "$OWN_MODULE"/*)
        own_lines=$((own_lines + 1))
        if [ "$licence" != "$OWN_LICENCE" ]; then
          mark="$mark  <- NOT $OWN_LICENCE"
          own_wrong=$((own_wrong + 1))
        fi
        ;;
    esac
  fi
  printf '  %-72s %s%s\n' "$name" "$licence" "$mark"
done < <(sort -u <<<"$report")

own_failed=0
if [ "$#" -lt 2 ] && [ "$module" != "$OWN_MODULE" ]; then
  echo ""
  echo "ERROR: this repository's module is $module, not $OWN_MODULE."
  own_failed=1
elif [ "$module" = "$OWN_MODULE" ]; then
  if [ "$own_lines" -eq 0 ]; then
    echo ""
    echo "ERROR: the report holds no line for $OWN_MODULE itself; it must be $OWN_LICENCE."
    own_failed=1
  elif [ "$own_wrong" -gt 0 ]; then
    echo ""
    echo "ERROR: $own_wrong line(s) above for $OWN_MODULE itself are not $OWN_LICENCE (its root LICENSE must be the Apache License 2.0)."
    own_failed=1
  fi
fi

if [ "$disallowed" -gt 0 ]; then
  echo ""
  echo "ERROR: $disallowed module licence(s) above are not on the allowed list in $policy."
fi
if [ "$disallowed" -gt 0 ] || [ "$own_failed" -ne 0 ]; then
  echo "See CONTRIBUTING.md's licence rules."
  exit 1
fi
echo "All Go module licences are allowed."
if [ "$module" = "$OWN_MODULE" ]; then
  echo "$OWN_MODULE itself is $OWN_LICENCE."
fi
