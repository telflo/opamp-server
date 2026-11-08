#!/usr/bin/env bash
# The chart is cloud-neutral: nothing in it, or rendered from it, names a
# cloud provider's annotations, identity bindings, API groups, hostnames or
# storage classes. Cloud details belong in an operator's own values files,
# passed through the chart's annotation maps, never in the chart.
#
# Usage: scripts/check-chart-cloud-neutral.sh [CHART_DIR [PROFILES_DIR]]
#   CHART_DIR     default: charts/opamp-server
#   PROFILES_DIR  default: CHART_DIR/tests/profiles (complete values files)
#
# It fails (exit 1) when:
#   - a file the chart packages (every file `helm package` puts in the .tgz:
#     templates, values.yaml, values.schema.json, README.md, NOTES.txt, ...)
#     holds a cloud marker;
#   - the chart rendered with any profile holds one (the chart's defaults
#     can't render alone: a profile is the defaults plus the names an install
#     must give);
#   - a template writes an annotation key itself: annotations come only from
#     the values maps (ANNOTATION_VALUES below). Checked twice: in the
#     templates' text, and by rendering every profile with those maps emptied,
#     when no annotation may be left.
#
# Offline: only `helm package` and `helm template`, with every HELM_* variable
# unset and Helm's homes in a temporary directory. Prints PASS/FAIL lines
# naming a file, a profile and a marker, never a value. Exit 2 when it can't
# run. bash 3.2 compatible.
set -u

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
CHART_DIR="${1:-$REPO_DIR/charts/opamp-server}"
PROFILES_DIR="${2:-$CHART_DIR/tests/profiles}"

# The markers (the design's list): matched anywhere, ignoring case.
MARKERS="amazonaws.com
arn:aws
service.beta.kubernetes.io/aws-
eks.amazonaws.com
elbv2.k8s.aws
iam.gke.io
cloud.google.com
azure.workload.identity
service.beta.kubernetes.io/azure-"
# The clouds' storage classes: matched as whole words, ignoring case.
STORAGE_CLASSES="gp2
gp3
standard-rwo
managed-csi"
# The values maps a rendered annotation may come from.
ANNOTATION_VALUES="podAnnotations
service.agent.annotations
service.management.annotations
serviceAccount.annotations
postgres.podAnnotations"

if ! command -v helm >/dev/null 2>&1; then echo "cloud-neutral check: helm is not on PATH" >&2; exit 2; fi
if [ ! -f "$CHART_DIR/Chart.yaml" ]; then echo "cloud-neutral check: no chart at the path given" >&2; exit 2; fi
if [ ! -d "$PROFILES_DIR" ]; then echo "cloud-neutral check: no profiles directory at the path given" >&2; exit 2; fi
CHART_DIR="$(cd "$CHART_DIR" && pwd)"
PROFILES_DIR="$(cd "$PROFILES_DIR" && pwd)"

for v in $(env | sed -n 's/^\(HELM_[A-Za-z0-9_]*\)=.*/\1/p'); do unset "$v"; done
tmp="$(mktemp -d "${TMPDIR:-/tmp}/cloud-neutral.XXXXXX")" || exit 2
if [ -z "$tmp" ] || [ ! -d "$tmp" ]; then echo "cloud-neutral check: no temporary directory" >&2; exit 2; fi
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/home" "$tmp/docker" "$tmp/helm/cache" "$tmp/helm/config" "$tmp/helm/data" "$tmp/kube" "$tmp/pkg" "$tmp/x" || exit 2
export HOME="$tmp/home" DOCKER_CONFIG="$tmp/docker" KUBECONFIG="$tmp/kube/config"
export HELM_CACHE_HOME="$tmp/helm/cache" HELM_CONFIG_HOME="$tmp/helm/config" HELM_DATA_HOME="$tmp/helm/data"

failures=0
fail() { echo "FAIL - $1"; failures=$((failures + 1)); }

# markers_in <file>: each marker the file holds, one a line
markers_in() {
  local m
  while IFS= read -r m; do
    [ -n "$m" ] && grep -qiF -- "$m" "$1" && echo "$m"
  done <<EOF
$MARKERS
EOF
  while IFS= read -r m; do
    [ -n "$m" ] && grep -qiwF -- "$m" "$1" && echo "$m"
  done <<EOF
$STORAGE_CLASSES
EOF
  return 0
}

# --- 1. every file the chart packages -----------------------------------------
if ! helm package "$CHART_DIR" -d "$tmp/pkg" >"$tmp/package.out" 2>&1; then
  echo "cloud-neutral check: helm package failed" >&2
  exit 2
fi
tgz="$(ls "$tmp/pkg"/*.tgz 2>/dev/null | head -1)"
[ -n "$tgz" ] && tar -xzf "$tgz" -C "$tmp/x" || { echo "cloud-neutral check: the package can't be read" >&2; exit 2; }
pkg_root="$(ls -d "$tmp/x"/*/ | head -1)"
pkg_root="${pkg_root%/}"
found_files=0
for want in Chart.yaml values.yaml values.schema.json README.md templates/NOTES.txt; do
  [ -f "$pkg_root/$want" ] || fail "the package holds no $want to check"
done
before=$failures
while IFS= read -r file; do
  found_files=$((found_files + 1))
  rel="${file#"$pkg_root"/}"
  for m in $(markers_in "$file"); do fail "packaged file $rel holds the cloud marker $m"; done
done <<EOF
$(find "$pkg_root" -type f | sort)
EOF
[ "$failures" = "$before" ] && echo "PASS - no cloud marker in the $found_files files the chart packages"

# --- 2. the chart rendered with each profile ----------------------------------
profiles="$(find "$PROFILES_DIR" -maxdepth 1 -type f -name '*.yaml' | sort)"
[ -n "$profiles" ] || { echo "cloud-neutral check: no profile to render" >&2; exit 2; }
while IFS= read -r profile; do
  name="$(basename "$profile")"
  before=$failures
  if ! helm template check "$pkg_root" -f "$profile" >"$tmp/render.yaml" 2>"$tmp/render.err"; then
    fail "the chart doesn't render with the profile $name"
    continue
  fi
  for m in $(markers_in "$tmp/render.yaml"); do fail "rendered with $name: holds the cloud marker $m"; done
  [ "$failures" = "$before" ] && echo "PASS - no cloud marker rendered with $name"
done <<EOF
$profiles
EOF

# --- 3. no annotation key written by a template --------------------------------
# a) The templates' text: under an `annotations:` key, every line is a
#    template action ({{ … }}) or a comment, never a literal key; and no
#    inline `annotations: {…}` map.
while IFS= read -r file; do
  rel="${file#"$pkg_root"/}"
  awk -v rel="$rel" '
    function indent(s) { match(s, /^ */); return RLENGTH }
    {
      line = $0
      if (open) {
        trimmed = line; sub(/^[ \t]+/, "", trimmed)
        if (trimmed == "" || trimmed ~ /^\{\{/ || trimmed ~ /^#/) next
        if (indent(line) > depth) { print rel ":" NR; open = 0; next }
        open = 0
      }
      if (line ~ /^[ \t-]*annotations:[ \t]*$/) { open = 1; depth = indent(line); next }
      # an inline map with a key, or any other value but a template action or {}
      if (line ~ /^[ \t-]*annotations:[ \t]*[^ \t{#]/ || line ~ /^[ \t-]*annotations:[ \t]*\{[ \t]*[^{} \t]/) print rel ":" NR
    }' "$file"
done <<EOF >"$tmp/static.txt"
$(find "$pkg_root/templates" -type f \( -name '*.yaml' -o -name '*.yml' -o -name '*.tpl' \) | sort)
EOF
if [ -s "$tmp/static.txt" ]; then
  while IFS= read -r where; do fail "template ${where%:*} line ${where##*:} writes an annotation key itself"; done <"$tmp/static.txt"
else
  echo "PASS - no template writes an annotation key itself"
fi

# b) Rendered with every annotation map emptied, nothing has an annotation.
mkdir -p "$tmp/annotations/templates" || exit 2
printf 'apiVersion: v2\nname: annotations\nversion: 0.0.0\n' >"$tmp/annotations/Chart.yaml"
cat >"$tmp/annotations/templates/list.yaml" <<'TEMPLATE'
{{- define "walk" }}
{{- $p := index . 0 }}
{{- $v := index . 1 }}
{{- if kindIs "map" $v }}
{{- range $k, $x := $v }}
{{- if and (eq $k "annotations") $x }}
# A {{ $p }}.annotations
{{- end }}
{{- include "walk" (list (printf "%s.%s" $p $k) $x) }}
{{- end }}
{{- else if kindIs "slice" $v }}
{{- range $i, $x := $v }}{{ include "walk" (list (printf "%s[%d]" $p $i) $x) }}{{ end }}
{{- end }}
{{- end }}
{{- range $raw := regexSplit "(?m)^---$" .Values.doc -1 }}
{{- $d := fromYaml $raw }}
{{- if hasKey $d "Error" }}
# A ERROR
{{- else if hasKey $d "kind" }}
{{- include "walk" (list (printf "%s/%s" $d.kind (dig "metadata" "name" "" $d)) $d) }}
{{- end }}
{{- end }}
TEMPLATE
empty_args=""
for key in $ANNOTATION_VALUES; do empty_args="$empty_args --set $key=null"; done
while IFS= read -r profile; do
  name="$(basename "$profile")"
  # shellcheck disable=SC2086 # empty_args is a list of words
  if ! helm template check "$pkg_root" -f "$profile" $empty_args >"$tmp/bare.yaml" 2>"$tmp/bare.err" ||
    ! helm template a "$tmp/annotations" --set-file "doc=$tmp/bare.yaml" >"$tmp/bare.list" 2>"$tmp/bare.list.err"; then
    fail "the chart doesn't render with the profile $name and its annotation maps emptied"
    continue
  fi
  left="$(sed -n 's/^# A //p' "$tmp/bare.list")"
  if [ -n "$left" ]; then
    while IFS= read -r where; do fail "rendered with $name, every annotation map emptied: an annotation is left at $where"; done <<EOF
$left
EOF
  else
    echo "PASS - no annotation rendered with $name once its annotation maps are emptied"
  fi
done <<EOF
$profiles
EOF

if [ "$failures" -gt 0 ]; then
  echo "cloud-neutral check: $failures failure(s)"
  exit 1
fi
echo "cloud-neutral check: passed"
