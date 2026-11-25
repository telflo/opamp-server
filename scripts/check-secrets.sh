#!/usr/bin/env bash
# The secret scan: gitleaks (pinned, built with go install) over every file
# git would commit, offline. `make secrets` runs it with its self-test, and
# CI runs it as its own job, so a leak fails within a minute.
#
# Usage: scripts/check-secrets.sh [--root DIR]
#   --root  the git checkout to scan (default: this repository); the
#           self-test's fixtures pass their own.
#
# It scans every file git would commit (tracked, and untracked but not
# ignored: never a local .env or .quickstart/), copied into a temporary
# folder, with the checkout's own .gitleaks.toml and nothing else
# (GITLEAKS_CONFIG and GITLEAKS_CONFIG_TOML are never read), or gitleaks'
# default rules when it has none. .gitleaks.toml keeps every default rule
# and allows only reviewed example and test values, each by its value.
#
# --redact: a value gitleaks finds is never printed, only its fingerprint,
# <path>:<rule>:<line>.
#
# Exit 0 when gitleaks finds nothing; 1 on a finding or when gitleaks
# fails; 2 on bad usage. bash 3.2 compatible.
set -uo pipefail
export LC_ALL=C

GITLEAKS="github.com/zricethezav/gitleaks/v8@v8.30.1"

# go.mod's toolchain line is the one every developer uses; never let a go
# command download a newer one.
export GOTOOLCHAIN=local

usage() {
  echo "usage: scripts/check-secrets.sh [--root DIR]" >&2
  exit 2
}

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
root=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --root) [ "$#" -ge 2 ] || usage; root=$2; shift 2 ;;
    *) echo "check-secrets: unknown argument: $1" >&2; usage ;;
  esac
done
[ -n "$root" ] || root="$REPO_DIR"
[ -d "$root" ] && root="$(cd "$root" && pwd -P)" || { echo "check-secrets: --root $root isn't a folder" >&2; exit 2; }
[ "$(git -C "$root" rev-parse --show-toplevel 2>/dev/null)" = "$root" ] ||
  { echo "check-secrets: $root isn't a git checkout's root" >&2; exit 2; }

work=$(mktemp -d "${TMPDIR:-/tmp}/check-secrets.XXXXXX") || { echo "check-secrets: FAIL: no temporary folder" >&2; exit 1; }
trap 'rm -rf "$work"' EXIT
mkdir "$work/tree" "$work/bin"

# The pinned gitleaks, built into the temporary folder from the module
# cache (checked against the Go checksum database), outside any module.
if ! (cd "$work" && GOBIN="$work/bin" GOFLAGS= go install "$GITLEAKS") >"$work/log" 2>&1; then
  echo "check-secrets: FAIL: cannot build $GITLEAKS:" >&2
  sed 's/^/  /' "$work/log" >&2
  exit 1
fi

# The files git would commit that are there (a deleted tracked file isn't), plain files only.
(
  cd "$root" || exit 1
  git ls-files -z -co --exclude-standard |
    while IFS= read -r -d '' f; do
      [ -f "$f" ] && [ ! -L "$f" ] && printf '%s\0' "$f"
    done
) >"$work/files" || { echo "check-secrets: FAIL: cannot list the files" >&2; exit 1; }
files=$(tr -cd '\0' <"$work/files" | wc -c | tr -d ' ')
if [ "$files" -gt 0 ]; then
  (cd "$root" && tar -cf - --null -T "$work/files") | tar -xf - -C "$work/tree" ||
    { echo "check-secrets: FAIL: cannot copy the files" >&2; exit 1; }
fi

# The checkout's own .gitleaks.toml, named explicitly (a copy outside the
# scanned tree), or gitleaks' default rules.
config=()
if [ -f "$work/tree/.gitleaks.toml" ]; then
  mv "$work/tree/.gitleaks.toml" "$work/gitleaks.toml"
  config=(--config "$work/gitleaks.toml")
fi

printf '{{range .}}{{.Fingerprint}}\n{{end}}' >"$work/fingerprint.tmpl"
if ! (cd "$work/tree" && env -u GITLEAKS_CONFIG -u GITLEAKS_CONFIG_TOML "$work/bin/gitleaks" dir . \
  ${config[@]+"${config[@]}"} \
  --redact --no-banner --no-color --log-level error --exit-code 0 \
  --report-format template --report-template "$work/fingerprint.tmpl" --report-path "$work/found") >"$work/log" 2>&1; then
  echo "check-secrets: FAIL: gitleaks failed:" >&2
  sed 's/^/  /' "$work/log" >&2
  exit 1
fi

sort -u "$work/found" | sed '/^$/d' >"$work/found.sorted"
status=0
while IFS= read -r fp; do
  echo "check-secrets: FAIL: gitleaks finds a secret: $fp (<path>:<rule>:<line>). Remove it, and rotate it if it was real; if it is an example or a test value, allow it by its value in .gitleaks.toml after review."
  status=1
done <"$work/found.sorted"
if [ "$status" = 0 ]; then
  echo "check-secrets: $files files, no finding"
fi
exit "$status"
