#!/usr/bin/env bash
# The cloud-neutral check's self-test: the check passes on the chart as it
# is, and fails on a copy of it broken each way it must catch, from the
# fixtures in scripts/testdata/cloud-neutral/:
#   - a cloud marker in a template, in values.yaml, in values.schema.json, in
#     the README, in NOTES.txt (the packaged files);
#   - a cloud marker only in the rendered output (an annotation passed through
#     a values map; a storage class);
#   - an annotation key written by a template: literally, built at render
#     time, in a branch no profile renders, and as an inline map.
#
# Run: scripts/check-chart-cloud-neutral.test.sh
# Offline; prints PASS/FAIL lines only; exit 1 when any case failed. Every
# copy lives in a temporary directory removed on exit. bash 3.2 compatible.
set -u

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
CHECK="$REPO_DIR/scripts/check-chart-cloud-neutral.sh"
CHART_DIR="$REPO_DIR/charts/opamp-server"
FIXTURES="$REPO_DIR/scripts/testdata/cloud-neutral"

tmp="$(mktemp -d "${TMPDIR:-/tmp}/cloud-neutral-test.XXXXXX")" || exit 2
if [ -z "$tmp" ] || [ ! -d "$tmp" ]; then echo "self-test: no temporary directory" >&2; exit 2; fi
trap 'rm -rf "$tmp"' EXIT

passed=0
failed=0
n=0
run_case() { # <name> <command...>
  local name="$1"; shift
  if "$@"; then
    echo "PASS - $name"; passed=$((passed + 1))
  else
    echo "FAIL - $name"; failed=$((failed + 1))
  fi
}

# fresh: a new copy of the chart (its profiles included) in $c; check: run the
# check on it, its output in $c.out and its exit status in $c.rc.
fresh() { n=$((n + 1)); c="$tmp/case$n"; mkdir -p "$c" && cp -R "$CHART_DIR" "$c/chart"; }
check() { TMPDIR="$tmp" "$CHECK" "$c/chart" "$c/chart/tests/profiles" >"$c.out" 2>"$c.err"; echo $? >"$c.rc"; }
has() { grep -qxF -- "$1" "$c.out"; }
fails_with() { # <expected FAIL lines...>: exit 1, and each line among its failures
  local line
  [ "$(cat "$c.rc")" = 1 ] || return 1
  for line in "$@"; do has "FAIL - $line" || return 1; done
}
no_fail_matching() { ! grep -q -- "^FAIL - $1" "$c.out"; }
line_of() { grep -n -- "$2" "$1" | head -1 | cut -d: -f1; }

as_it_is() {
  fresh && check || return 1
  [ "$(cat "$c.rc")" = 0 ] && has "cloud-neutral check: passed" && ! grep -q '^FAIL' "$c.out"
}
run_case "the chart as it is passes" as_it_is

in_template() {
  fresh && cp "$FIXTURES/marker-template.yaml" "$c/chart/templates/" && check
  fails_with "packaged file templates/marker-template.yaml holds the cloud marker arn:aws" \
    "rendered with bundled.yaml: holds the cloud marker arn:aws"
}
run_case "a marker in a template fails (the packaged file and the output)" in_template

in_values() {
  fresh && cat "$FIXTURES/values-marker.txt" >>"$c/chart/values.yaml" && check
  fails_with "packaged file values.yaml holds the cloud marker gp3" && no_fail_matching "rendered with"
}
run_case "a marker only in values.yaml (a comment, never rendered) fails" in_values

in_schema() {
  fresh || return 1
  sed "s|\"title\": \"opamp-server chart values\"|\"title\": \"opamp-server chart values $(cat "$FIXTURES/schema-marker.txt")\"|" \
    "$CHART_DIR/values.schema.json" >"$c/chart/values.schema.json" && check
  fails_with "packaged file values.schema.json holds the cloud marker azure.workload.identity" && no_fail_matching "rendered with"
}
run_case "a marker in values.schema.json fails" in_schema

in_readme() {
  fresh && cat "$FIXTURES/readme-marker.txt" >>"$c/chart/README.md" && check
  fails_with "packaged file README.md holds the cloud marker service.beta.kubernetes.io/aws-" && no_fail_matching "rendered with"
}
run_case "a marker in the README fails" in_readme

in_notes() {
  fresh && cat "$FIXTURES/notes-marker.txt" >>"$c/chart/templates/NOTES.txt" && check
  fails_with "packaged file templates/NOTES.txt holds the cloud marker iam.gke.io"
}
run_case "a marker in NOTES.txt fails" in_notes

only_output() {
  fresh && cp "$FIXTURES/output-only-profile.yaml" "$c/chart/tests/profiles/output-only.yaml" && check
  fails_with "rendered with output-only.yaml: holds the cloud marker service.beta.kubernetes.io/azure-" &&
    no_fail_matching "packaged file" && no_fail_matching "rendered with bundled.yaml"
}
run_case "a marker only in the output (a cloud annotation through a values map) fails" only_output

storage_class() {
  fresh && cp "$FIXTURES/storage-class-profile.yaml" "$c/chart/tests/profiles/storage-class.yaml" && check
  fails_with "rendered with storage-class.yaml: holds the cloud marker managed-csi" && no_fail_matching "packaged file"
}
run_case "a cloud's storage class in the output fails" storage_class

hard_coded() {
  fresh && cp "$FIXTURES/hard-coded-key.yaml" "$c/chart/templates/" && check
  fails_with "template templates/hard-coded-key.yaml line $(line_of "$FIXTURES/hard-coded-key.yaml" 'example.invalid/hard-coded') writes an annotation key itself" \
    "rendered with bundled.yaml, every annotation map emptied: an annotation is left at ConfigMap/check-fixture.metadata.annotations" &&
    no_fail_matching "packaged file" && no_fail_matching "rendered with bundled.yaml:"
}
run_case "a hard-coded annotation key fails (the templates' text and the output)" hard_coded

built_key() {
  fresh && cp "$FIXTURES/hard-coded-key-built.yaml" "$c/chart/templates/" && check
  fails_with "rendered with full.yaml, every annotation map emptied: an annotation is left at ConfigMap/check-fixture.metadata.annotations" &&
    no_fail_matching "template "
}
run_case "an annotation key built at render time fails (the output)" built_key

unrendered_key() {
  fresh && cp "$FIXTURES/hard-coded-key-unrendered.yaml" "$c/chart/templates/" && check
  fails_with "template templates/hard-coded-key-unrendered.yaml line $(line_of "$FIXTURES/hard-coded-key-unrendered.yaml" 'example.invalid/unrendered') writes an annotation key itself" &&
    no_fail_matching "rendered with"
}
run_case "a hard-coded key in a branch no profile renders fails (the templates' text)" unrendered_key

inline_key() {
  fresh && cp "$FIXTURES/hard-coded-key-inline.yaml" "$c/chart/templates/" && check
  fails_with "template templates/hard-coded-key-inline.yaml line $(line_of "$FIXTURES/hard-coded-key-inline.yaml" 'annotations: {') writes an annotation key itself" \
    "rendered with external.yaml, every annotation map emptied: an annotation is left at ConfigMap/check-fixture.metadata.annotations"
}
run_case "an inline annotation map fails" inline_key

echo "# $passed passed, $failed failed"
[ "$failed" -eq 0 ]
