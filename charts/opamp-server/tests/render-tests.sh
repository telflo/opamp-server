#!/usr/bin/env bash
# Render tests for charts/opamp-server.
#
# `helm lint --strict` can't catch most of these: it lints with the values it
# is given, and only warns on a failing `required`. So every guard, refusal,
# name, credential reference and security setting is pinned here by a real
# `helm template`.
#
# Run:  bash charts/opamp-server/tests/render-tests.sh
#
# Offline and credential-free: only `helm template` and `helm lint` (no
# registry, no cluster, no --debug). Every HELM_* variable is unset; Helm's
# homes, HOME, DOCKER_CONFIG and KUBECONFIG are empty directories in a
# temporary directory removed on exit. Each case prints PASS or FAIL and its
# name, never a value or Helm's output (an error's text can quote a value); a
# failing render is that case's FAIL and the script goes on. Exit 1 when any
# case failed. bash 3.2 compatible (macOS's /bin/bash).
set -u

CHART_DIR="$(cd "$(dirname "$0")/.." && pwd)"
REPO_DIR="$(cd "$CHART_DIR/../.." && pwd)"
PROFILES="$CHART_DIR/tests/profiles"
# The chart's version, as Chart.yaml says: a release changes that file and
# the READMEs only (CONTRIBUTING.md, "Releasing"), and the cases follow it.
VERSION=$(sed -n 's/^version: //p' "$CHART_DIR/Chart.yaml")

if ! command -v helm >/dev/null 2>&1; then
  echo "render tests: helm is not on PATH" >&2
  exit 2
fi

for v in $(env | sed -n 's/^\(HELM_[A-Za-z0-9_]*\)=.*/\1/p'); do unset "$v"; done
tmp="$(mktemp -d "${TMPDIR:-/tmp}/opamp-server-render.XXXXXX")" || exit 2
if [ -z "$tmp" ] || [ ! -d "$tmp" ]; then echo "render tests: no temporary directory" >&2; exit 2; fi
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/home" "$tmp/docker" "$tmp/helm/cache" "$tmp/helm/config" "$tmp/helm/data" "$tmp/kube" || exit 2
export HOME="$tmp/home" DOCKER_CONFIG="$tmp/docker" KUBECONFIG="$tmp/kube/config"
export HELM_CACHE_HOME="$tmp/helm/cache" HELM_CONFIG_HOME="$tmp/helm/config" HELM_DATA_HOME="$tmp/helm/data"

REL=rel
NS=opamp
FULL=rel-opamp-server
# The agent endpoint's Deployment, and the management API's (…M).
D="Deployment/$FULL-agent"
POD="$D.spec.template.spec"
MIG="$POD.initContainers[migrate]"
WAIT="$POD.initContainers[wait-for-postgres]"
SRV="$POD.containers[serve]"
DM="Deployment/$FULL-management"
PODM="$DM.spec.template.spec"
MIGM="$PODM.initContainers[migrate]"
WAITM="$PODM.initContainers[wait-for-postgres]"
SRVM="$PODM.containers[serve]"
SS="StatefulSet/$FULL-postgres"
PGPOD="$SS.spec.template.spec"
PG="$PGPOD.containers[postgres]"
CM="ConfigMap/$FULL-bootstrap"
AGENT_SVC="Service/$FULL-agent"
MGMT_SVC="Service/$FULL-management"
PG_DIGEST=sha256:edb0c08dc33befd0262ab3b5cf1c4f3f8cfd8b09e88c05b45adee29d6761ddb6

# A throwaway chart that flattens a rendered manifest stream, parsed with
# Helm's own fromYaml (what Helm reads a release's manifests with), into one
# line per leaf: "<Kind>/<name><path> <JSON value>". A list element that is a
# map with a "name" is addressed by it ("env[OPAMP_DATABASE_URL]") and also
# gets a line "<path>[<name>]#index <i>", for the order; any other element by
# its index. An empty map or list is "{}" or "[]"; a document that fails to
# parse is "ERROR" (never its error's text). The listing goes to a file,
# never to the output.
mkdir -p "$tmp/flat/templates" "$tmp/json/templates" || exit 2
printf 'apiVersion: v2\nname: flat\nversion: 0.0.0\n' >"$tmp/flat/Chart.yaml"
cat >"$tmp/flat/templates/list.yaml" <<'TEMPLATE'
{{- define "walk" }}
{{- $p := index . 0 }}
{{- $v := index . 1 }}
{{- if kindIs "map" $v }}
{{- if not $v }}
# L {{ $p }} {}
{{- end }}
{{- range $k, $x := $v }}{{ include "walk" (list (printf "%s.%s" $p $k) $x) }}{{ end }}
{{- else if kindIs "slice" $v }}
{{- if not $v }}
# L {{ $p }} []
{{- end }}
{{- range $i, $x := $v }}
{{- $seg := toString $i }}
{{- if and (kindIs "map" $x) (hasKey $x "name") }}
{{- $seg = toString $x.name }}
# L {{ $p }}[{{ $seg }}]#index {{ $i }}
{{- end }}
{{- include "walk" (list (printf "%s[%s]" $p $seg) $x) }}
{{- end }}
{{- else }}
# L {{ $p }} {{ toJson $v }}
{{- end }}
{{- end }}
{{- range $raw := regexSplit "(?m)^---$" .Values.doc -1 }}
{{- $d := fromYaml $raw }}
{{- if hasKey $d "Error" }}
# L ERROR
{{- else if hasKey $d "kind" }}
{{- include "walk" (list (printf "%s/%s" $d.kind (dig "metadata" "name" "" $d)) $d) }}
{{- end }}
{{- end }}
TEMPLATE
# Another prints a file's content as the same JSON encoder does, to compare
# a ConfigMap's data with the file it came from.
printf 'apiVersion: v2\nname: json\nversion: 0.0.0\n' >"$tmp/json/Chart.yaml"
printf '# J {{ toJson .Values.f }}\n' >"$tmp/json/templates/j.yaml"

passed=0
failed=0
n=0
next_file() { n=$((n + 1)); f="$tmp/case$n"; }

# render <out> <helm template args...>: stdout to <out>, stderr to <out>.err.
render() { local out="$1"; shift; helm template "$REL" "$CHART_DIR" -n "$NS" "$@" >"$out" 2>"$out.err"; }
# flat <rendered> <listing>
flat() {
  helm template k "$tmp/flat" --set-file "doc=$1" >"$2.raw" 2>"$2.err" || return 1
  sed -n 's/^# L //p' "$2.raw" >"$2"
  [ -s "$2" ] && ! grep -qx 'ERROR' "$2"
}
# render_flat <listing> <helm template args...>: render to <listing>.yaml, then flatten.
render_flat() { local out="$1"; shift; render "$out.yaml" "$@" && flat "$out.yaml" "$out"; }
has_line() { grep -qxF -- "$2" "$1"; }
# value <listing> <path>: the JSON value at exactly <path> (nothing when absent)
value() { awk -v p="$2" 'index($0, p " ") == 1 && length($0) > length(p) + 1 { print substr($0, length(p) + 2) }' "$1"; }
# no_path <listing> <path>: nothing at <path> or under it
no_path() { ! awk -v p="$2" 'index($0, p) == 1 && index(" .[#", substr($0, length(p) + 1, 1)) { found = 1 } END { exit !found }' "$1"; }
# env_names <listing> <container path>: its variables' names, in order, one a line
env_names() {
  awk -v p="$2.env[" 'index($0, p) == 1 && $0 ~ /\]#index [0-9]+$/ {
      rest = substr($0, length(p) + 1); name = rest; sub(/\]#index [0-9]+$/, "", name)
      i = rest; sub(/^.*\]#index /, "", i); print i " " name }' "$1" | sort -n | cut -d' ' -f2
}
# kinds <listing>: "<Kind>/<name>" of every document, sorted
kinds() { sed -n 's/^\([A-Za-z]*\/[^ .]*\)\.kind .*/\1/p' "$1" | sort; }
# secret_keys <listing> <container path>: the keys its secretKeyRefs read, sorted
secret_keys() { awk -v p="$2.env[" 'index($0, p) == 1 && $1 ~ /\.valueFrom\.secretKeyRef\.key$/ { print $2 }' "$1" | sort; }

run_case() { # <name> <command...>
  local name="$1"; shift
  if "$@"; then
    echo "PASS - $name"; passed=$((passed + 1))
  else
    echo "FAIL - $name"; failed=$((failed + 1))
  fi
}

# refused <message> <a value that must not appear, or ""> <helm args...>: the
# render fails with exactly one error, the template guard's <message>.
refused() {
  local msg="$1" canary="$2"; shift 2
  next_file
  if render "$f" "$@"; then return 1; fi
  [ "$(grep -c '^Error:' "$f.err")" = 1 ] || return 1
  sed -n 's/^Error: execution error at (opamp-server\/templates\/[a-z-]*\.yaml:[0-9]*:[0-9]*): //p' "$f.err" >"$f.msg"
  grep -qxF -- "$msg" "$f.msg" || return 1
  [ -z "$canary" ] || ! grep -qF -- "$canary" "$f" "$f.err"
}
# schema_refused <JSON pointer> <reason fragment> <helm args...>: refused by
# values.schema.json at <pointer>. Its error may quote the value, so callers
# pass canary values only, and nothing of it is printed.
schema_refused() {
  local pointer="$1" reason="$2"; shift 2
  next_file
  if render "$f" "$@"; then return 1; fi
  grep -qF "values don't meet the specifications of the schema" "$f.err" || return 1
  grep -F -- "- at '$pointer': " "$f.err" | grep -qF -- "$reason"
}
renders() { next_file; render "$f" "$@"; }

B=(-f "$PROFILES/bundled.yaml")
EXT=(-f "$PROFILES/external.yaml")
URLS=(-f "$PROFILES/external-urls.yaml")
FULLP=(-f "$PROFILES/full.yaml")

# The profiles' listings, rendered once.
render_flat "$tmp/bundled" "${B[@]}"; bundled_ok=$?
render_flat "$tmp/external" "${EXT[@]}"; external_ok=$?
render_flat "$tmp/urls" "${URLS[@]}"; urls_ok=$?
render_flat "$tmp/full" "${FULLP[@]}"; full_ok=$?
L="$tmp/bundled"

# ---------------------------------------------------------------------------
echo "# the files"

bootstrap_copy() { cmp -s "$CHART_DIR/files/bootstrap.sql" "$REPO_DIR/deploy/bootstrap.sql" && [ ! -L "$CHART_DIR/files/bootstrap.sql" ]; }
run_case "files/bootstrap.sql is a plain copy of deploy/bootstrap.sql (cmp)" bootstrap_copy

configmap_holds() { # <data key> <file>
  next_file
  helm template j "$tmp/json" --set-file "f=$2" >"$f" 2>"$f.err" || return 1
  [ "$bundled_ok" = 0 ] && [ -n "$(value "$L" "$CM.data.$1")" ] && [ "$(value "$L" "$CM.data.$1")" = "$(sed -n 's/^# J //p' "$f")" ]
}
run_case "the ConfigMap's bootstrap.sql is the file, byte for byte" configmap_holds bootstrap.sql "$CHART_DIR/files/bootstrap.sql"
run_case "the ConfigMap's initdb.sh is the file, byte for byte" configmap_holds initdb.sh "$CHART_DIR/files/initdb.sh"

initdb_lines() {
  grep -qxF "PGOPTIONS='-c log_min_error_statement=panic'" "$CHART_DIR/files/initdb.sh" &&
    grep -qxF "export PGOPTIONS" "$CHART_DIR/files/initdb.sh" &&
    grep -qxF 'exec psql -X -v ON_ERROR_STOP=1 --no-password --username "$POSTGRES_USER" --dbname postgres \' "$CHART_DIR/files/initdb.sh" &&
    grep -qxF '  -v database="${OPAMP_DATABASE_NAME:-telflo_opamp}" -f /opamp-bootstrap/bootstrap.sql' "$CHART_DIR/files/initdb.sh" &&
    [ "$(grep -n 'PGOPTIONS=' "$CHART_DIR/files/initdb.sh" | cut -d: -f1)" -lt "$(grep -n '^exec psql' "$CHART_DIR/files/initdb.sh" | cut -d: -f1)" ]
}
run_case "initdb.sh sets PGOPTIONS log_min_error_statement=panic, then execs psql on /opamp-bootstrap/bootstrap.sql" initdb_lines

no_lookup() { ! grep -rqw lookup "$CHART_DIR/templates"; }
run_case "no template calls lookup" no_lookup
helmignore_tests() { grep -qx 'tests/' "$CHART_DIR/.helmignore"; }
run_case ".helmignore leaves tests/ out of the package" helmignore_tests
chart_yaml() {
  local c="$CHART_DIR/Chart.yaml"
  [[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] &&
    grep -qx 'name: opamp-server' "$c" && grep -qx "version: $VERSION" "$c" && grep -qx "appVersion: \"$VERSION\"" "$c" &&
    grep -qx 'home: https://github.com/telflo/opamp-server' "$c" && grep -qx '  - https://github.com/telflo/opamp-server' "$c" &&
    grep -q '^kubeVersion: ' "$c"
}
run_case "Chart.yaml: name, version X.Y.Z and appVersion the same, home, sources, kubeVersion" chart_yaml

# ---------------------------------------------------------------------------
echo "# nothing that could carry a credential"

ALL_LISTINGS="$tmp/bundled $tmp/external $tmp/urls $tmp/full"
profiles_render() { [ "$bundled_ok" = 0 ] && [ "$external_ok" = 0 ] && [ "$urls_ok" = 0 ] && [ "$full_ok" = 0 ]; }
run_case "every profile renders" profiles_render
no_kind() { # <kind>
  local l
  for l in $ALL_LISTINGS; do kinds "$l" | grep -q "^$1/" && return 1; done
  return 0
}
run_case "no Secret rendered by any profile" no_kind Secret
run_case "no Job rendered" no_kind Job
run_case "no CronJob rendered" no_kind CronJob
run_case "no Ingress rendered" no_kind Ingress
run_case "no HorizontalPodAutoscaler rendered" no_kind HorizontalPodAutoscaler
no_hook() { ! grep -q 'helm\.sh/hook' "$tmp/bundled.yaml" "$tmp/external.yaml" "$tmp/urls.yaml" "$tmp/full.yaml"; }
run_case "no Helm hook rendered" no_hook
no_kind_secret_text() { ! grep -rqE '^kind: *Secret' "$CHART_DIR/templates"; }
run_case "no template holds kind: Secret" no_kind_secret_text

# every value the chart takes as a name set to a canary: each canary appears
# only where that name belongs (a Secret's name in secretKeyRef.name or a
# volume's secretName, a key in secretKeyRef.key), never in a variable's
# value or a URL.
C_AUTH=cnry-auth-secret
C_PG=cnry-pg-secret
C_AGENT_TLS=cnry-agent-tls
C_MGMT_TLS=cnry-mgmt-tls
C_RUNTIME_KEY=cnry-runtime-url-key
C_MIGRATION_KEY=cnry-migration-url-key
only_at() { # <listing> <canary> <allowed path regex>: present, and only at such paths
  awk -v c="$2" 'index($0, c) { print }' "$1" >"$1.$2" || return 1
  [ -s "$1.$2" ] || return 1
  RE="$3" awk '{ split($0, a, " "); if (a[1] !~ ENVIRON["RE"]) bad = 1 } END { exit bad }' "$1.$2"
}
canaries_bundled() {
  next_file
  render_flat "$f" "${B[@]}" --set-string "auth.existingSecret=$C_AUTH,postgres.auth.existingSecret=$C_PG" \
    --set-string "server.agent.tls.secretName=$C_AGENT_TLS,server.management.tls.secretName=$C_MGMT_TLS" || return 1
  only_at "$f" "$C_AUTH" '(\.valueFrom\.secretKeyRef\.name|\.volumes\[encryption-key\]\.secret\.secretName)$' &&
    only_at "$f" "$C_PG" '^StatefulSet/[^ ]*\.containers\[postgres\]\.env\[POSTGRES_PASSWORD\]\.valueFrom\.secretKeyRef\.name$' &&
    only_at "$f" "$C_AGENT_TLS" '\.volumes\[agent-tls\]\.secret\.secretName$' &&
    only_at "$f" "$C_MGMT_TLS" '\.volumes\[management-tls\]\.secret\.secretName$'
}
run_case "Secret names (canaries) appear only as secretKeyRef names and volume secretNames" canaries_bundled
canaries_urls() {
  next_file
  render_flat "$f" "${URLS[@]}" --set-string "auth.existingSecret=$C_AUTH" \
    --set-string "externalDatabase.urlKeys.runtime=$C_RUNTIME_KEY,externalDatabase.urlKeys.migration=$C_MIGRATION_KEY" || return 1
  only_at "$f" "$C_RUNTIME_KEY" '\.containers\[serve\]\.env\[OPAMP_DATABASE_URL\]\.valueFrom\.secretKeyRef\.key$' &&
    only_at "$f" "$C_MIGRATION_KEY" '\.initContainers\[migrate\]\.env\[OPAMP_MIGRATION_DATABASE_URL\]\.valueFrom\.secretKeyRef\.key$' &&
    only_at "$f" "$C_AUTH" '\.valueFrom\.secretKeyRef\.name$|\.volumes\[encryption-key\]\.secret\.secretName$'
}
run_case "whole-URL keys (canaries) appear only as secretKeyRef keys" canaries_urls
no_value_holds_secret_names() { # no env value names a Secret, in any profile
  local l
  for l in $ALL_LISTINGS; do
    awk '$1 ~ /\.env\[[^]]*\]\.value$/' "$l" | grep -qE 'opamp-server-credentials|opamp-postgres-credentials|registry-pull|tls-certificate|example-extra|cnry-' && return 1
  done
  return 0
}
run_case "no variable's value holds a Secret's name" no_value_holds_secret_names
urls_compose_only() { # every URL value takes its password from a $(VAR), never a literal
  local l
  for l in $ALL_LISTINGS; do
    awk '$1 ~ /DATABASE_URL\]\.value$/ { print $2 }' "$l" | grep -vqE '^"postgresql://telflo_opamp(_owner)?:\$\(OPAMP_(OWNER|RUNTIME)_PASSWORD\)@' && return 1
  done
  return 0
}
run_case "every composed URL holds \$(…) for its password, never a value" urls_compose_only

# ---------------------------------------------------------------------------
echo "# the defaults (the bundled Postgres)"

defaults_kinds() {
  [ "$(kinds "$L" | tr '\n' ' ')" = "ConfigMap/$FULL-bootstrap $D $DM Service/$FULL-agent Service/$FULL-management Service/$FULL-postgres ServiceAccount/$FULL StatefulSet/$FULL-postgres " ]
}
run_case "the defaults render the ConfigMap, two Deployments (agent, management), three Services, ServiceAccount and StatefulSet, and nothing else" defaults_kinds
eq() { [ "$(value "$1" "$2")" = "$3" ]; }
both_eq() { eq "$L" "$D$1" "$2" && eq "$L" "$DM$1" "$2"; }
run_case "one replica of each by default" both_eq .spec.replicas 1
rolling() { both_eq .spec.strategy.type '"RollingUpdate"' && both_eq .spec.strategy.rollingUpdate.maxUnavailable 0; }
run_case "RollingUpdate, never a pod short (both)" rolling
run_case "the servers' image is ghcr.io/telflo/opamp-server:<appVersion> (both)" both_eq .spec.template.spec.containers[serve].image "\"ghcr.io/telflo/opamp-server:$VERSION\""
run_case "migrate runs the same image (both)" both_eq .spec.template.spec.initContainers[migrate].image "\"ghcr.io/telflo/opamp-server:$VERSION\""
mig_args() {
  local d
  for d in "$POD" "$PODM"; do
    eq "$L" "$d.initContainers[migrate].args[0]" '"migrate"' && eq "$L" "$d.initContainers[migrate].args[1]" '"up"' || return 1
  done
  eq "$L" "$SRV.args[0]" '"serve"' && eq "$L" "$SRV.args[1]" '"agent"' && no_path "$L" "$SRV.args[2]" &&
    eq "$L" "$SRVM.args[0]" '"serve"' && eq "$L" "$SRVM.args[1]" '"management"' && no_path "$L" "$SRVM.args[2]"
}
run_case "migrate's args are migrate up; serve's are serve agent and serve management" mig_args
init_order() {
  has_line "$L" "$POD.initContainers[wait-for-postgres]#index 0" && has_line "$L" "$POD.initContainers[migrate]#index 1" &&
    has_line "$L" "$PODM.initContainers[wait-for-postgres]#index 0" && has_line "$L" "$PODM.initContainers[migrate]#index 1"
}
run_case "the init containers: wait-for-postgres, then migrate (both)" init_order
ports() {
  eq "$L" "$SRV.ports[agent].containerPort" 51234 && no_path "$L" "$SRV.ports[management]" &&
    eq "$L" "$SRVM.ports[management].containerPort" 51235 && no_path "$L" "$SRVM.ports[agent]"
}
run_case "each serve its own port: agent 51234, management 51235" ports
probes() { # <container path> <port name>
  eq "$L" "$1.readinessProbe.httpGet.path" '"/ready"' && eq "$L" "$1.readinessProbe.httpGet.port" "\"$2\"" &&
    [ "$(value "$L" "$1.readinessProbe.timeoutSeconds")" -ge 3 ] && eq "$L" "$1.readinessProbe.httpGet.scheme" '"HTTP"' &&
    eq "$L" "$1.livenessProbe.httpGet.path" '"/health"' && eq "$L" "$1.livenessProbe.httpGet.port" "\"$2\"" &&
    eq "$L" "$1.livenessProbe.httpGet.scheme" '"HTTP"'
}
run_case "the agent's probes: /ready and /health on the agent port, readiness timeout at least 3 s" probes "$SRV" agent
run_case "the management API's probes: /ready and /health on the management port" probes "$SRVM" management
grace() { [ "$(value "$L" "$POD.terminationGracePeriodSeconds")" -ge 30 ] && [ "$(value "$L" "$PODM.terminationGracePeriodSeconds")" -ge 30 ]; }
run_case "terminationGracePeriodSeconds is 30 or more (both)" grace
no_token() { both_eq .spec.template.spec.automountServiceAccountToken false && eq "$L" "ServiceAccount/$FULL.automountServiceAccountToken" false && eq "$L" "$PGPOD.automountServiceAccountToken" false; }
run_case "no service account token mounted (both servers' pods, their account, Postgres)" no_token
run_case "the servers' pods run as its ServiceAccount" both_eq .spec.template.spec.serviceAccountName "\"$FULL\""
no_pull_secrets() { no_path "$L" "$POD.imagePullSecrets" && no_path "$L" "$PODM.imagePullSecrets" && no_path "$L" "$PGPOD.imagePullSecrets"; }
run_case "imagePullSecrets is empty by default" no_pull_secrets
no_np_pdb() { ! kinds "$L" | grep -qE '^(NetworkPolicy|PodDisruptionBudget)/'; }
run_case "no NetworkPolicy or PodDisruptionBudget by default" no_np_pdb
no_tunables() {
  local v
  for v in OPAMP_HEARTBEAT_INTERVAL_SECONDS OPAMP_DEFAULT_REQUESTS_PER_MINUTE OPAMP_COLLECTOR_REQUESTS_PER_MINUTE \
    OPAMP_MAX_CONCURRENT_REQUESTS OPAMP_MAX_CONCURRENT_REQUESTS_PER_ORGANIZATION OPAMP_SHUTDOWN_DRAIN_SECONDS \
    OTEL_EXPORTER_OTLP_ENDPOINT OTEL_SERVICE_NAME OTEL_RESOURCE_ATTRIBUTES; do
    no_path "$L" "$SRV.env[$v]" && no_path "$L" "$SRVM.env[$v]" || return 1
  done
}
run_case "no tunable or OTEL_ variable set by default (the server's defaults apply)" no_tunables
run_case "OPAMP_SERVER_URL is server.publicUrl" eq "$L" "$SRV.env[OPAMP_SERVER_URL].value" '"https://opamp.example.com/opamp/api/v1"'
run_case "the management API's pods carry no OPAMP_SERVER_URL" no_path "$L" "$SRVM.env[OPAMP_SERVER_URL]"

# ---------------------------------------------------------------------------
echo "# the security contexts"

pod_context() { # <listing> <pod path> <uid>
  eq "$1" "$2.securityContext.runAsNonRoot" true && eq "$1" "$2.securityContext.runAsUser" "$3" &&
    eq "$1" "$2.securityContext.runAsGroup" "$3" && eq "$1" "$2.securityContext.fsGroup" "$3" &&
    eq "$1" "$2.securityContext.seccompProfile.type" '"RuntimeDefault"'
}
container_context() { # <listing> <container path> <uid>
  eq "$1" "$2.securityContext.allowPrivilegeEscalation" false && eq "$1" "$2.securityContext.readOnlyRootFilesystem" true &&
    eq "$1" "$2.securityContext.runAsNonRoot" true && eq "$1" "$2.securityContext.runAsUser" "$3" &&
    eq "$1" "$2.securityContext.runAsGroup" "$3" && eq "$1" "$2.securityContext.capabilities.drop[0]" '"ALL"' &&
    no_path "$1" "$2.securityContext.capabilities.add" && no_path "$1" "$2.securityContext.privileged"
}
run_case "the server's pod: non-root 65532 (numbers), fsGroup 65532, RuntimeDefault seccomp" pod_context "$L" "$POD" 65532
run_case "serve: no escalation, read-only root, every capability dropped, uid 65532" container_context "$L" "$SRV" 65532
run_case "migrate: no escalation, read-only root, every capability dropped, uid 65532" container_context "$L" "$MIG" 65532
run_case "wait-for-postgres: no escalation, read-only root, every capability dropped, uid 999" container_context "$L" "$WAIT" 999
run_case "the management API's pod: non-root 65532, fsGroup 65532, RuntimeDefault seccomp" pod_context "$L" "$PODM" 65532
run_case "its serve: no escalation, read-only root, every capability dropped, uid 65532" container_context "$L" "$SRVM" 65532
run_case "its migrate: likewise, uid 65532" container_context "$L" "$MIGM" 65532
run_case "its wait-for-postgres: likewise, uid 999" container_context "$L" "$WAITM" 999
run_case "the Postgres pod: non-root 999, fsGroup 999, RuntimeDefault seccomp" pod_context "$L" "$PGPOD" 999
run_case "postgres: no escalation, read-only root, every capability dropped, uid 999" container_context "$L" "$PG" 999
all_contexts_full() {
  local d
  for d in Deployment/opamp-agent Deployment/opamp-management; do
    pod_context "$tmp/full" "$d.spec.template.spec" 65532 &&
      container_context "$tmp/full" "$d.spec.template.spec.containers[serve]" 65532 &&
      container_context "$tmp/full" "$d.spec.template.spec.initContainers[migrate]" 65532 || return 1
  done
}
run_case "the security contexts hold with every option on" all_contexts_full

# ---------------------------------------------------------------------------
echo "# each container holds only its own credentials"

lines() { printf '%s\n' "$@"; }
mig_env() { # <migrate path>
  [ "$(env_names "$L" "$1")" = "$(lines OPAMP_OWNER_PASSWORD OPAMP_MIGRATION_DATABASE_URL)" ]
}
run_case "migrate's variables: OPAMP_OWNER_PASSWORD, then OPAMP_MIGRATION_DATABASE_URL, nothing else (agent)" mig_env "$MIG"
run_case "migrate's variables likewise (management)" mig_env "$MIGM"
mig_keys() { local m; for m in "$MIG" "$MIGM"; do [ "$(secret_keys "$L" "$m")" = '"owner-password"' ] && no_path "$L" "$m.volumeMounts" || return 1; done; }
run_case "migrate reads owner-password only, and mounts no Secret (both)" mig_keys
srv_env() {
  [ "$(env_names "$L" "$SRV")" = "$(lines OPAMP_RUNTIME_PASSWORD OPAMP_DATABASE_URL OPAMP_ENCRYPTION_KEY_FILE OPAMP_SERVER_URL)" ]
}
run_case "the agent's serve variables: the runtime password, its URL, the key file, the server URL" srv_env
srvm_env() {
  [ "$(env_names "$L" "$SRVM")" = "$(lines OPAMP_RUNTIME_PASSWORD OPAMP_DATABASE_URL OPAMP_MANAGEMENT_SECRET \
    OPAMP_MANAGEMENT_SECRETS_PREVIOUS OPAMP_ENCRYPTION_KEY_FILE)" ]
}
run_case "the management API's serve variables: the runtime password, its URL, the management secrets, the key file" srvm_env
srv_keys() {
  [ "$(secret_keys "$L" "$SRV" | tr '\n' ' ')" = '"runtime-password" ' ] &&
    [ "$(secret_keys "$L" "$SRVM" | tr '\n' ' ')" = '"management-secret" "management-secrets-previous" "runtime-password" ' ]
}
run_case "the agent's serve reads runtime-password only; the management API's also management-secret and management-secrets-previous" srv_keys
agent_no_management_secret() { # in every profile, nothing under the agent's Deployment names a management secret
  local l
  for l in $ALL_LISTINGS; do
    awk '$1 ~ /^Deployment\/[^.]*-agent\./' "$l" >"$l.agent" || return 1
    [ -s "$l.agent" ] || return 1
    grep -qi 'management-secret\|management_secret' "$l.agent" && return 1
  done
  return 0
}
run_case "the agent endpoint's pods hold no management secret, in any profile" agent_no_management_secret
srv_no_owner() { ! awk -v p="$SRV" 'index($0, p) == 1' "$L" | grep -q 'owner' && ! awk -v p="$SRVM" 'index($0, p) == 1' "$L" | grep -q 'owner'; }
run_case "the owner's password is nowhere in either serve container" srv_no_owner
key_file() { # <pod path> <serve path>
  eq "$L" "$2.env[OPAMP_ENCRYPTION_KEY_FILE].value" '"/run/secrets/opamp/encryption-key"' &&
    eq "$L" "$2.volumeMounts[encryption-key].mountPath" '"/run/secrets/opamp"' &&
    eq "$L" "$2.volumeMounts[encryption-key].readOnly" true &&
    eq "$L" "$1.volumes[encryption-key].secret.items[0].key" '"encryption-key"' &&
    eq "$L" "$1.volumes[encryption-key].secret.items[0].path" '"encryption-key"' &&
    no_path "$L" "$1.volumes[encryption-key].secret.items[1]" &&
    eq "$L" "$1.volumes[encryption-key].secret.defaultMode" 288 &&
    no_path "$L" "$1.volumes[encryption-key].secret.optional" &&
    no_path "$L" "$2.env[OPAMP_ENCRYPTION_KEYS_PREVIOUS]" && no_path "$L" "$2.env[OPAMP_ENCRYPTION_KEYS_PREVIOUS_FILE]" &&
    ! grep -q 'encryption-keys-previous' "$L"
}
run_case "the encryption key is a mounted file (only that item, mode 0440, not optional); nothing of previous keys by default (agent)" key_file "$POD" "$SRV"
run_case "likewise (management)" key_file "$PODM" "$SRVM"
previous_keys() {
  next_file
  render_flat "$f" "${B[@]}" --set auth.encryptionKeysPrevious.enabled=true || return 1
  local pod srv mig
  for pod in "$POD" "$PODM"; do
    srv="$pod.containers[serve]" mig="$pod.initContainers[migrate]"
    eq "$f" "$srv.env[OPAMP_ENCRYPTION_KEYS_PREVIOUS_FILE].value" '"/run/secrets/opamp/encryption-keys-previous"' &&
      eq "$f" "$pod.volumes[encryption-key].secret.items[0].key" '"encryption-key"' &&
      eq "$f" "$pod.volumes[encryption-key].secret.items[1].key" '"encryption-keys-previous"' &&
      eq "$f" "$pod.volumes[encryption-key].secret.items[1].path" '"encryption-keys-previous"' &&
      no_path "$f" "$pod.volumes[encryption-key].secret.items[2]" &&
      eq "$f" "$pod.volumes[encryption-key].secret.defaultMode" 288 &&
      no_path "$f" "$pod.volumes[encryption-key].secret.optional" &&
      no_path "$f" "$pod.volumes[encryption-key].secret.items[1].mode" &&
      no_path "$f" "$srv.env[OPAMP_ENCRYPTION_KEYS_PREVIOUS]" &&
      ! awk -v p="$mig" 'index($0, p) == 1' "$f" | grep -q 'encryption' || return 1
  done
}
run_case "auth.encryptionKeysPrevious.enabled: encryption-keys-previous mounted as a required 0440 file, OPAMP_ENCRYPTION_KEYS_PREVIOUS_FILE set, no variable holds it (both)" previous_keys
wait_holds_nothing() {
  local w
  for w in "$WAIT" "$WAITM"; do no_path "$L" "$w.env" && no_path "$L" "$w.envFrom" && no_path "$L" "$w.volumeMounts" || return 1; done
}
run_case "wait-for-postgres holds no credential" wait_holds_nothing
pg_env() {
  [ "$(env_names "$L" "$PG")" = "$(lines POSTGRES_USER POSTGRES_PASSWORD OPAMP_OWNER_PASSWORD OPAMP_RUNTIME_PASSWORD OPAMP_DATABASE_NAME)" ] &&
    [ "$(secret_keys "$L" "$PG" | tr '\n' ' ')" = '"owner-password" "postgres-password" "runtime-password" ' ]
}
run_case "postgres holds the superuser's password and the two bootstrap passwords" pg_env
superuser_only_in_pg() { [ "$(grep -c 'postgres-password"$' "$L")" = 1 ] && has_line "$L" "$PG.env[POSTGRES_PASSWORD].valueFrom.secretKeyRef.key \"postgres-password\""; }
run_case "the superuser's password is read in the Postgres container only" superuser_only_in_pg
optional_flags() { # <listing>: optional: true exactly on the one optional key
  local l="$1"
  [ "$(awk '$1 ~ /\.optional$/ { print $1 " " $2 }' "$l" | sed 's/.*\.env\[\([^]]*\)\]\.valueFrom\.secretKeyRef\.optional /\1 /')" = "OPAMP_MANAGEMENT_SECRETS_PREVIOUS true" ] &&
    eq "$l" "$(awk '$1 ~ /env\[OPAMP_MANAGEMENT_SECRETS_PREVIOUS\]\.valueFrom\.secretKeyRef\.key$/ { print $1 }' "$l")" '"management-secrets-previous"'
}
run_case "optional: true only on management-secrets-previous (bundled)" optional_flags "$L"
run_case "optional: true only on management-secrets-previous (every option on, previous keys mounted)" optional_flags "$tmp/full"
password_before_url() { # <listing> <container path> <password var> <url var>
  local pw url
  pw="$(awk -v p="$2.env[$3]#index " 'index($0, p) == 1 { print substr($0, length(p) + 1) }' "$1")"
  url="$(awk -v p="$2.env[$4]#index " 'index($0, p) == 1 { print substr($0, length(p) + 1) }' "$1")"
  [ -n "$pw" ] && [ -n "$url" ] && [ "$pw" -lt "$url" ]
}
run_case "migrate lists OPAMP_OWNER_PASSWORD before the URL that expands it" password_before_url "$L" "$MIG" OPAMP_OWNER_PASSWORD OPAMP_MIGRATION_DATABASE_URL
run_case "serve lists OPAMP_RUNTIME_PASSWORD before the URL that expands it" password_before_url "$L" "$SRV" OPAMP_RUNTIME_PASSWORD OPAMP_DATABASE_URL
run_case "the management API's migrate likewise" password_before_url "$L" "$MIGM" OPAMP_OWNER_PASSWORD OPAMP_MIGRATION_DATABASE_URL
run_case "the management API's serve likewise" password_before_url "$L" "$SRVM" OPAMP_RUNTIME_PASSWORD OPAMP_DATABASE_URL
run_case "the order holds with an external Postgres too (migrate)" password_before_url "$tmp/external" "$MIG" OPAMP_OWNER_PASSWORD OPAMP_MIGRATION_DATABASE_URL
run_case "the order holds with an external Postgres too (serve)" password_before_url "$tmp/external" "$SRV" OPAMP_RUNTIME_PASSWORD OPAMP_DATABASE_URL
bundled_urls() {
  eq "$L" "$MIG.env[OPAMP_MIGRATION_DATABASE_URL].value" "\"postgresql://telflo_opamp_owner:\$(OPAMP_OWNER_PASSWORD)@$FULL-postgres:5432/telflo_opamp?sslmode=disable\"" &&
    eq "$L" "$SRV.env[OPAMP_DATABASE_URL].value" "\"postgresql://telflo_opamp:\$(OPAMP_RUNTIME_PASSWORD)@$FULL-postgres:5432/telflo_opamp?sslmode=disable\"" &&
    eq "$L" "$MIGM.env[OPAMP_MIGRATION_DATABASE_URL].value" "$(value "$L" "$MIG.env[OPAMP_MIGRATION_DATABASE_URL].value")" &&
    eq "$L" "$SRVM.env[OPAMP_DATABASE_URL].value" "$(value "$L" "$SRV.env[OPAMP_DATABASE_URL].value")"
}
run_case "the bundled Postgres's URLs: owner for migrate, telflo_opamp for serve" bundled_urls
srv_extra_env_last() {
  local names d
  for d in Deployment/opamp-agent Deployment/opamp-management; do
    names="$(env_names "$tmp/full" "$d.spec.template.spec.containers[serve]")"
    [ "$(printf '%s\n' "$names" | tail -2 | tr '\n' ' ')" = "EXAMPLE_PLAIN EXAMPLE_FROM_SECRET " ] &&
      eq "$tmp/full" "$d.spec.template.spec.containers[serve].env[EXAMPLE_FROM_SECRET].valueFrom.secretKeyRef.name" '"example-extra"' &&
      no_path "$tmp/full" "$d.spec.template.spec.initContainers[migrate].env[EXAMPLE_PLAIN]" || return 1
  done
}
run_case "extraEnv goes to both serve containers only, after the chart's variables, valueFrom kept" srv_extra_env_last

# ---------------------------------------------------------------------------
echo "# the bundled Postgres"

run_case "postgres:18 pinned by its digest" eq "$L" "$PG.image" "\"postgres:18@$PG_DIGEST\""
run_case "wait-for-postgres runs the same pinned image" eq "$L" "$WAIT.image" "\"postgres:18@$PG_DIGEST\""
pg_no_digest() {
  next_file
  render_flat "$f" "${B[@]}" --set-string postgres.image.digest= --set-string postgres.image.pullPolicy=Never || return 1
  eq "$f" "$PG.image" '"postgres:18"' && eq "$f" "$WAIT.image" '"postgres:18"' && eq "$f" "$PG.imagePullPolicy" '"Never"' &&
    eq "$f" "$WAIT.imagePullPolicy" '"Never"'
}
run_case "an empty postgres.image.digest is the tag alone (postgres:18), with pullPolicy Never" pg_no_digest
run_case "postgres.image.pullPolicy defaults to IfNotPresent" eq "$L" "$PG.imagePullPolicy" '"IfNotPresent"'
pg_guard() {
  local cmd
  eq "$L" "$PG.command[0]" '"sh"' && eq "$L" "$PG.command[1]" '"-c"' || return 1
  cmd="$(value "$L" "$PG.command[2]")"
  case "$cmd" in
    *'url_safe() { case \"$1\" in \"\" | *[!A-Za-z0-9._~-]*) return 1 ;; esac; }'*) ;;
    *) return 1 ;;
  esac
  # In order: the check of each password, the entrypoint only if both pass,
  # then the fixed message and exit 1.
  case "$cmd" in
    *'if url_safe \"${OPAMP_OWNER_PASSWORD-}\" '*' url_safe \"${OPAMP_RUNTIME_PASSWORD-}\"; then\n  exec docker-entrypoint.sh postgres\nfi\necho \"opamp-server: owner-password and runtime-password must be set, non-empty and URL-safe'*'exit 1\n"') ;;
    *) return 1 ;;
  esac
}
run_case "postgres's command checks both passwords (set, non-empty, URL-safe) before docker-entrypoint.sh, else exits 1" pg_guard

# The guard itself, run: the rendered script under sh, with a stand-in
# docker-entrypoint.sh first on PATH that only records that it ran. The
# passwords are fixed test strings, never secrets.
mkdir -p "$tmp/guard/templates" "$tmp/standins" || exit 2
printf 'apiVersion: v2\nname: guard\nversion: 0.0.0\n' >"$tmp/guard/Chart.yaml"
cat >"$tmp/guard/templates/g.yaml" <<'TEMPLATE'
{{- range $raw := regexSplit "(?m)^---$" .Values.doc -1 }}
{{- $d := fromYaml $raw }}
{{- if eq (toString $d.kind) "StatefulSet" }}
{{- range $c := $d.spec.template.spec.containers }}
{{- if eq $c.name "postgres" }}
{{- range $line := splitList "\n" (index $c.command 2) }}
#G {{ $line }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
TEMPLATE
cat >"$tmp/standins/docker-entrypoint.sh" <<'STANDIN'
#!/bin/sh
echo "entrypoint ran: $*" >"$GUARD_LOG"
exit 0
STANDIN
chmod +x "$tmp/standins/docker-entrypoint.sh"
helm template g "$tmp/guard" --set-file "doc=$tmp/bundled.yaml" 2>/dev/null | sed -n 's/^#G //p' >"$tmp/guard.sh"
guard_run() { # <expected: start|refuse> <owner password or "-unset-"> <runtime password or "-unset-">
  next_file
  [ -s "$tmp/guard.sh" ] || return 1
  rm -f "$f.log"
  local status
  (
    unset OPAMP_OWNER_PASSWORD OPAMP_RUNTIME_PASSWORD
    [ "$2" = "-unset-" ] || export OPAMP_OWNER_PASSWORD="$2"
    [ "$3" = "-unset-" ] || export OPAMP_RUNTIME_PASSWORD="$3"
    GUARD_LOG="$f.log" PATH="$tmp/standins:$PATH" sh -c "$(cat "$tmp/guard.sh")" >"$f.out" 2>"$f.err"
  )
  status=$?
  if [ "$1" = start ]; then
    [ "$status" = 0 ] && grep -qx 'entrypoint ran: postgres' "$f.log"
  else
    [ "$status" = 1 ] && [ ! -e "$f.log" ] &&
      grep -qxF 'opamp-server: owner-password and runtime-password must be set, non-empty and URL-safe (only A-Z a-z 0-9 . _ ~ -); Postgres was not started' "$f.err" &&
      [ "$(wc -l <"$f.err" | tr -d ' ')" = 1 ] && [ ! -s "$f.out" ]
  fi
}
run_case "the guard starts Postgres when both passwords are URL-safe" guard_run start 'Owner.pw_1~-' 'runtime-PW.2_~'
run_case "the guard starts Postgres with hex passwords (openssl rand -hex 32's shape)" guard_run start 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210
run_case "the guard refuses an unset owner password, before the entrypoint" guard_run refuse -unset- runtime-ok
run_case "the guard refuses an unset runtime password" guard_run refuse owner-ok -unset-
run_case "the guard refuses an empty owner password" guard_run refuse '' runtime-ok
run_case "the guard refuses an empty runtime password" guard_run refuse owner-ok ''
run_case "the guard refuses a password holding @" guard_run refuse 'owner@x' runtime-ok
run_case "the guard refuses a password holding /" guard_run refuse owner-ok 'run/time'
run_case "the guard refuses a password holding %" guard_run refuse 'own%40er' runtime-ok
run_case "the guard refuses a password holding a space" guard_run refuse owner-ok 'run time'
run_case "the guard refuses a password holding a newline" guard_run refuse "$(printf 'own\ner')" runtime-ok
run_case "the guard refuses a password holding \$(…)" guard_run refuse 'a$(echo EXPANDED)b' runtime-ok

# initdb.sh, run with a stand-in psql that records its arguments and
# PGOPTIONS, never reading stdin or the passwords.
cat >"$tmp/standins/psql" <<'STANDIN'
#!/bin/sh
printf '%s\n' "$@" >"$PSQL_LOG"
printf 'PGOPTIONS=%s\n' "$PGOPTIONS" >>"$PSQL_LOG"
exit 0
STANDIN
chmod +x "$tmp/standins/psql"
initdb_run() { # <OPAMP_DATABASE_NAME or "-unset-"> <expected database>
  next_file
  (
    unset OPAMP_DATABASE_NAME PGOPTIONS
    [ "$1" = "-unset-" ] || export OPAMP_DATABASE_NAME="$1"
    POSTGRES_USER=postgres PSQL_LOG="$f.log" PATH="$tmp/standins:$PATH" sh "$CHART_DIR/files/initdb.sh" >"$f.out" 2>"$f.err"
  ) || return 1
  [ "$(cat "$f.log")" = "$(printf '%s\n' -X -v ON_ERROR_STOP=1 --no-password --username postgres --dbname postgres -v "database=$2" -f /opamp-bootstrap/bootstrap.sql 'PGOPTIONS=-c log_min_error_statement=panic')" ]
}
run_case "initdb.sh runs psql on /opamp-bootstrap/bootstrap.sql as POSTGRES_USER, with PGOPTIONS set" initdb_run -unset- telflo_opamp
run_case "initdb.sh passes OPAMP_DATABASE_NAME as -v database" initdb_run opamp_db opamp_db
pg_mounts() {
  eq "$L" "$PG.volumeMounts[bootstrap-initdb].mountPath" '"/docker-entrypoint-initdb.d"' &&
    eq "$L" "$PGPOD.volumes[bootstrap-initdb].configMap.items[0].key" '"initdb.sh"' &&
    eq "$L" "$PGPOD.volumes[bootstrap-initdb].configMap.items[0].mode" 365 &&
    no_path "$L" "$PGPOD.volumes[bootstrap-initdb].configMap.items[1]" &&
    eq "$L" "$PG.volumeMounts[bootstrap-sql].mountPath" '"/opamp-bootstrap"' &&
    eq "$L" "$PGPOD.volumes[bootstrap-sql].configMap.items[0].key" '"bootstrap.sql"' &&
    no_path "$L" "$PGPOD.volumes[bootstrap-sql].configMap.items[1]" &&
    eq "$L" "$PGPOD.volumes[bootstrap-initdb].configMap.name" "\"$FULL-bootstrap\"" &&
    eq "$L" "$PGPOD.volumes[bootstrap-sql].configMap.name" "\"$FULL-bootstrap\""
}
run_case "the ConfigMap mounted twice: initdb.sh alone (0555) in the init folder, bootstrap.sql in /opamp-bootstrap" pg_mounts
pg_writable() {
  eq "$L" "$PG.volumeMounts[run].mountPath" '"/var/run/postgresql"' && eq "$L" "$PGPOD.volumes[run].emptyDir" '{}' &&
    eq "$L" "$PG.volumeMounts[tmp].mountPath" '"/tmp"' && eq "$L" "$PGPOD.volumes[tmp].emptyDir" '{}' &&
    eq "$L" "$PG.volumeMounts[data].mountPath" '"/var/lib/postgresql"'
}
run_case "postgres writes only to emptyDirs (/var/run/postgresql, /tmp) and its volume" pg_writable
pg_claim() {
  eq "$L" "$SS.spec.volumeClaimTemplates[0].spec.accessModes[0]" '"ReadWriteOnce"' &&
    eq "$L" "$SS.spec.volumeClaimTemplates[0].spec.resources.requests.storage" '"8Gi"' &&
    no_path "$L" "$SS.spec.volumeClaimTemplates[0].spec.storageClassName"
}
run_case "one RWO claim, no storageClassName (the cluster's default class)" pg_claim
pg_class() { eq "$tmp/full" "StatefulSet/opamp-postgres.spec.volumeClaimTemplates[0].spec.storageClassName" '"fast"'; }
run_case "postgres.persistence.storageClassName is passed through when set" pg_class
pg_probes() {
  eq "$L" "$PG.readinessProbe.exec.command[0]" '"pg_isready"' && eq "$L" "$PG.livenessProbe.exec.command[0]" '"pg_isready"'
}
run_case "pg_isready probes" pg_probes
pg_headless() { eq "$L" "Service/$FULL-postgres.spec.clusterIP" '"None"' && eq "$L" "$SS.spec.serviceName" "\"$FULL-postgres\""; }
run_case "a headless Service names the StatefulSet's pod" pg_headless
pg_dbname() { eq "$tmp/full" "StatefulSet/opamp-postgres.spec.template.spec.containers[postgres].env[OPAMP_DATABASE_NAME].value" '"opamp_db"' &&
  eq "$tmp/full" "Deployment/opamp-agent.spec.template.spec.containers[serve].env[OPAMP_DATABASE_URL].value" '"postgresql://telflo_opamp:$(OPAMP_RUNTIME_PASSWORD)@opamp-postgres:5432/opamp_db?sslmode=disable"'; }
run_case "postgres.database names the bootstrap's database and the URLs'" pg_dbname

# ---------------------------------------------------------------------------
echo "# an external Postgres"

X="$tmp/external"
ext_kinds() {
  [ "$(kinds "$X" | tr '\n' ' ')" = "ConfigMap/$FULL-bootstrap $D $DM Service/$FULL-agent Service/$FULL-management ServiceAccount/$FULL " ]
}
run_case "no StatefulSet or postgres Service; the bootstrap ConfigMap still rendered" ext_kinds
ext_no_wait() { no_path "$X" "$WAIT" && no_path "$X" "$WAITM"; }
run_case "no wait-for-postgres init container (both)" ext_no_wait
ext_urls() {
  eq "$X" "$MIG.env[OPAMP_MIGRATION_DATABASE_URL].value" '"postgresql://telflo_opamp_owner:$(OPAMP_OWNER_PASSWORD)@db.example.com:5433/opamp_db?sslmode=verify-full"' &&
    eq "$X" "$SRV.env[OPAMP_DATABASE_URL].value" '"postgresql://telflo_opamp:$(OPAMP_RUNTIME_PASSWORD)@db.example.com:5433/opamp_db?sslmode=verify-full"' &&
    eq "$X" "$SRVM.env[OPAMP_DATABASE_URL].value" '"postgresql://telflo_opamp:$(OPAMP_RUNTIME_PASSWORD)@db.example.com:5433/opamp_db?sslmode=verify-full"'
}
run_case "the URLs take externalDatabase's host, port, database and sslmode" ext_urls
ext_keys() {
  [ "$(secret_keys "$X" "$MIG")" = '"owner-password"' ] && secret_keys "$X" "$SRV" | grep -qx '"runtime-password"' &&
    [ "$(secret_keys "$X" "$MIGM")" = '"owner-password"' ] && secret_keys "$X" "$SRVM" | grep -qx '"runtime-password"'
}
run_case "the passwords still come from owner-password and runtime-password" ext_keys
U="$tmp/urls"
urls_refs() {
  [ "$(env_names "$U" "$MIG")" = OPAMP_MIGRATION_DATABASE_URL ] &&
    eq "$U" "$MIG.env[OPAMP_MIGRATION_DATABASE_URL].valueFrom.secretKeyRef.key" '"migration-url"' &&
    eq "$U" "$SRV.env[OPAMP_DATABASE_URL].valueFrom.secretKeyRef.key" '"runtime-url"' &&
    no_path "$U" "$SRV.env[OPAMP_RUNTIME_PASSWORD]" && no_path "$U" "$SRV.env[OPAMP_DATABASE_URL].value" &&
    no_path "$U" "$MIG.env[OPAMP_MIGRATION_DATABASE_URL].value" &&
    eq "$U" "$MIGM.env[OPAMP_MIGRATION_DATABASE_URL].valueFrom.secretKeyRef.key" '"migration-url"' &&
    eq "$U" "$SRVM.env[OPAMP_DATABASE_URL].valueFrom.secretKeyRef.key" '"runtime-url"' && no_path "$U" "$SRVM.env[OPAMP_RUNTIME_PASSWORD]"
}
run_case "whole-URL keys: each URL from its key, no password variable" urls_refs
urls_no_passwords() { ! grep -qE '"(owner|runtime)-password"' "$U" && ! grep -qF '$(' "$U"; }
run_case "whole-URL keys: no secretKeyRef for the two passwords, no \$(…) anywhere" urls_no_passwords
urls_optional() { optional_flags "$U"; }
run_case "whole-URL keys: optional: true still only on management-secrets-previous" urls_optional
ext_no_pg_secret() { ! grep -q 'postgres-password' "$X" "$U"; }
run_case "an external Postgres needs no postgres.auth.existingSecret, and reads none" ext_no_pg_secret

# ---------------------------------------------------------------------------
echo "# TLS on each listener"

agent_tls() {
  next_file
  render_flat "$f" "${B[@]}" --set-string server.agent.tls.secretName=agent-tls || return 1
  eq "$f" "$SRV.env[OPAMP_AGENT_TLS_CERT_FILE].value" '"/run/secrets/opamp-agent-tls/tls.crt"' &&
    eq "$f" "$SRV.env[OPAMP_AGENT_TLS_KEY_FILE].value" '"/run/secrets/opamp-agent-tls/tls.key"' &&
    eq "$f" "$POD.volumes[agent-tls].secret.secretName" '"agent-tls"' &&
    eq "$f" "$POD.volumes[agent-tls].secret.items[0].key" '"tls.crt"' && eq "$f" "$POD.volumes[agent-tls].secret.items[1].key" '"tls.key"' &&
    eq "$f" "$POD.volumes[agent-tls].secret.defaultMode" 288 &&
    eq "$f" "$SRV.volumeMounts[agent-tls].readOnly" true &&
    eq "$f" "$SRV.readinessProbe.httpGet.scheme" '"HTTPS"' && eq "$f" "$SRV.livenessProbe.httpGet.scheme" '"HTTPS"' &&
    no_path "$f" "$SRV.env[OPAMP_MANAGEMENT_TLS_CERT_FILE]" && no_path "$f" "$MIG.volumeMounts" &&
    no_path "$f" "$PODM.volumes[agent-tls]" && no_path "$f" "$SRVM.env[OPAMP_AGENT_TLS_CERT_FILE]" &&
    eq "$f" "$SRVM.readinessProbe.httpGet.scheme" '"HTTP"'
}
run_case "agent TLS: the tls Secret's two files mounted (0440) in the agent's pods only, both variables, its probes over HTTPS" agent_tls
management_tls() {
  next_file
  render_flat "$f" "${B[@]}" --set-string server.management.tls.secretName=management-tls || return 1
  eq "$f" "$SRVM.env[OPAMP_MANAGEMENT_TLS_CERT_FILE].value" '"/run/secrets/opamp-management-tls/tls.crt"' &&
    eq "$f" "$SRVM.env[OPAMP_MANAGEMENT_TLS_KEY_FILE].value" '"/run/secrets/opamp-management-tls/tls.key"' &&
    eq "$f" "$PODM.volumes[management-tls].secret.secretName" '"management-tls"' &&
    eq "$f" "$PODM.volumes[management-tls].secret.defaultMode" 288 &&
    eq "$f" "$SRVM.volumeMounts[management-tls].readOnly" true &&
    eq "$f" "$SRVM.livenessProbe.httpGet.scheme" '"HTTPS"' && eq "$f" "$SRVM.readinessProbe.httpGet.scheme" '"HTTPS"' &&
    no_path "$f" "$SRVM.env[OPAMP_AGENT_TLS_CERT_FILE]" &&
    no_path "$f" "$POD.volumes[management-tls]" && no_path "$f" "$SRV.env[OPAMP_MANAGEMENT_TLS_CERT_FILE]" &&
    eq "$f" "$SRV.readinessProbe.httpGet.scheme" '"HTTP"'
}
run_case "management TLS: its files mounted in the management API's pods only, both variables, its probes over HTTPS" management_tls

# ---------------------------------------------------------------------------
echo "# the Services"

svc_default() {
  eq "$L" "$AGENT_SVC.spec.type" '"ClusterIP"' && eq "$L" "$AGENT_SVC.spec.ports[agent].port" 51234 &&
    eq "$L" "$AGENT_SVC.spec.ports[agent].targetPort" '"agent"' && no_path "$L" "$AGENT_SVC.spec.ports[agent].nodePort" &&
    no_path "$L" "$AGENT_SVC.metadata.annotations" && no_path "$L" "$AGENT_SVC.spec.loadBalancerSourceRanges"
}
run_case "the agent Service: ClusterIP on 51234 by default, no annotations" svc_default
mgmt_clusterip() { # <listing> <fullname>
  eq "$1" "Service/$2-management.spec.type" '"ClusterIP"' && eq "$1" "Service/$2-management.spec.ports[management].port" 51235 &&
    eq "$1" "Service/$2-management.spec.ports[management].targetPort" '"management"' &&
    no_path "$1" "Service/$2-management.spec.ports[management].nodePort" &&
    no_path "$1" "Service/$2-management.spec.loadBalancerSourceRanges"
}
run_case "the management Service is ClusterIP (defaults)" mgmt_clusterip "$L" "$FULL"
svc_selectors() {
  eq "$L" "$AGENT_SVC.spec.selector.app.kubernetes.io/component" '"agent"' && eq "$L" "$MGMT_SVC.spec.selector.app.kubernetes.io/component" '"management"' &&
    eq "$L" "$D.spec.selector.matchLabels.app.kubernetes.io/component" '"agent"' && eq "$L" "$DM.spec.selector.matchLabels.app.kubernetes.io/component" '"management"' &&
    eq "$L" "$D.spec.template.metadata.labels.app.kubernetes.io/component" '"agent"' && eq "$L" "$DM.spec.template.metadata.labels.app.kubernetes.io/component" '"management"'
}
run_case "each Service selects its own component's pods: agent and management" svc_selectors
agent_type() { # <type>
  next_file
  render_flat "$f" "${B[@]}" --set-string "service.agent.type=$1" || return 1
  eq "$f" "$AGENT_SVC.spec.type" "\"$1\"" && mgmt_clusterip "$f" "$FULL"
}
run_case "agent Service type NodePort; the management Service stays ClusterIP" agent_type NodePort
run_case "agent Service type LoadBalancer; the management Service stays ClusterIP" agent_type LoadBalancer
svc_full() {
  local F="$tmp/full"
  eq "$F" "Service/opamp-agent.spec.type" '"LoadBalancer"' && eq "$F" "Service/opamp-agent.spec.ports[agent].port" 443 &&
    eq "$F" "Service/opamp-agent.spec.ports[agent].nodePort" 31234 &&
    eq "$F" "Service/opamp-agent.spec.loadBalancerSourceRanges[0]" '"10.0.0.0/8"' &&
    eq "$F" "Service/opamp-agent.metadata.annotations.example.com/load-balancer" '"public"' &&
    eq "$F" "Service/opamp-management.metadata.annotations.example.com/scrape" '"false"' &&
    mgmt_clusterip "$F" opamp
}
run_case "agent Service values (port, nodePort, ranges, annotations) passed through; management annotations too, still ClusterIP" svc_full
nodeport() {
  next_file
  render_flat "$f" "${B[@]}" --set-string service.agent.type=NodePort --set service.agent.nodePort=30123 || return 1
  eq "$f" "$AGENT_SVC.spec.ports[agent].nodePort" 30123
}
run_case "service.agent.nodePort with type NodePort" nodeport
mgmt_type_refused() { schema_refused /service/management "additional properties 'type' not allowed" "${B[@]}" --set-string service.management.type=LoadBalancer; }
run_case "service.management.type is refused (the management Service has no type)" mgmt_type_refused
mgmt_port_refused() { schema_refused /service/management "additional properties 'port' not allowed" "${B[@]}" --set service.management.port=8443; }
run_case "service.management.port is refused" mgmt_port_refused
run_case "service.agent.type outside the enum refused" schema_refused /service/agent/type "value must be one of" "${B[@]}" --set-string service.agent.type=CnryType

# ---------------------------------------------------------------------------
echo "# NetworkPolicy, PodDisruptionBudget, replicas, pull secrets"

F="$tmp/full"
NP="NetworkPolicy/opamp-agent"
NPM="NetworkPolicy/opamp-management"
np_server() {
  eq "$F" "$NP.spec.policyTypes[0]" '"Ingress"' && eq "$F" "$NP.spec.podSelector.matchLabels.app.kubernetes.io/component" '"agent"' &&
    eq "$F" "$NP.spec.ingress[0].ports[0].port" 51234 && no_path "$F" "$NP.spec.ingress[0].from" && no_path "$F" "$NP.spec.ingress[1]" &&
    eq "$F" "$NPM.spec.policyTypes[0]" '"Ingress"' && eq "$F" "$NPM.spec.podSelector.matchLabels.app.kubernetes.io/component" '"management"' &&
    eq "$F" "$NPM.spec.ingress[0].ports[0].port" 51235 &&
    eq "$F" "$NPM.spec.ingress[0].from[0].podSelector.matchLabels.app.kubernetes.io/name" '"management-client"' &&
    eq "$F" "$NPM.spec.ingress[0].from[1].namespaceSelector.matchLabels.kubernetes.io/metadata.name" '"platform"' &&
    no_path "$F" "$NPM.spec.ingress[1]"
}
run_case "NetworkPolicies on: the agent's port from anywhere, the management API's only from networkPolicy.management.from" np_server
np_postgres() {
  local P="NetworkPolicy/opamp-postgres"
  eq "$F" "$P.spec.podSelector.matchLabels.app.kubernetes.io/component" '"postgres"' &&
    eq "$F" "$P.spec.ingress[0].ports[0].port" 5432 &&
    eq "$F" "$P.spec.ingress[0].from[0].podSelector.matchLabels.app.kubernetes.io/component" '"agent"' &&
    eq "$F" "$P.spec.ingress[0].from[0].podSelector.matchLabels.app.kubernetes.io/instance" "\"$REL\"" &&
    eq "$F" "$P.spec.ingress[0].from[1].podSelector.matchLabels.app.kubernetes.io/component" '"management"' &&
    eq "$F" "$P.spec.ingress[0].from[1].podSelector.matchLabels.app.kubernetes.io/instance" "\"$REL\"" &&
    no_path "$F" "$P.spec.ingress[0].from[2]" && no_path "$F" "$P.spec.ingress[1]"
}
run_case "NetworkPolicy on: Postgres only from the two components' pods" np_postgres
np_no_from() {
  next_file
  render_flat "$f" "${B[@]}" --set networkPolicy.enabled=true || return 1
  eq "$f" "NetworkPolicy/$FULL-agent.spec.ingress[0].ports[0].port" 51234 && no_path "$f" "NetworkPolicy/$FULL-agent.spec.ingress[1]" &&
    eq "$f" "NetworkPolicy/$FULL-management.spec.policyTypes[0]" '"Ingress"' && no_path "$f" "NetworkPolicy/$FULL-management.spec.ingress" &&
    ! grep '^NetworkPolicy/' "$f" | grep -q 51235
}
run_case "NetworkPolicy on with no management.from: the management API's pods admit nothing, with no rule at all (an empty from admits everyone)" np_no_from
np_external() {
  next_file
  render_flat "$f" "${EXT[@]}" --set networkPolicy.enabled=true || return 1
  kinds "$f" | grep -qx "NetworkPolicy/$FULL-agent" && kinds "$f" | grep -qx "NetworkPolicy/$FULL-management" &&
    ! kinds "$f" | grep -q "NetworkPolicy/$FULL-postgres"
}
run_case "NetworkPolicy with an external Postgres: no policy for a Postgres the chart doesn't run" np_external
pdb_each() {
  eq "$F" "PodDisruptionBudget/opamp-agent.spec.minAvailable" 2 && no_path "$F" "PodDisruptionBudget/opamp-agent.spec.maxUnavailable" &&
    eq "$F" "PodDisruptionBudget/opamp-agent.spec.selector.matchLabels.app.kubernetes.io/component" '"agent"' &&
    eq "$F" "PodDisruptionBudget/opamp-management.spec.maxUnavailable" 1 && no_path "$F" "PodDisruptionBudget/opamp-management.spec.minAvailable" &&
    eq "$F" "PodDisruptionBudget/opamp-management.spec.selector.matchLabels.app.kubernetes.io/component" '"management"'
}
run_case "a PDB per component: the agent's with minAvailable, the management API's with its default maxUnavailable 1" pdb_each
pdb_one() {
  next_file
  render_flat "$f" "${B[@]}" --set server.agent.podDisruptionBudget.enabled=true || return 1
  eq "$f" "PodDisruptionBudget/$FULL-agent.spec.maxUnavailable" 1 && no_path "$f" "PodDisruptionBudget/$FULL-agent.spec.minAvailable" &&
    ! kinds "$f" | grep -q "PodDisruptionBudget/$FULL-management"
}
run_case "one component's PDB on alone, with its default maxUnavailable 1" pdb_one
replicas_each() { eq "$F" "Deployment/opamp-agent.spec.replicas" 3 && eq "$F" "Deployment/opamp-management.spec.replicas" 2; }
run_case "each component's replicas" replicas_each
replicas_zero() {
  next_file
  render_flat "$f" "${B[@]}" --set server.management.replicas=0 || return 1
  eq "$f" "$DM.spec.replicas" 0 && eq "$f" "$D.spec.replicas" 1
}
run_case "replicas 0 stops one component, the other untouched" replicas_zero
pull_secrets() {
  eq "$F" "Deployment/opamp-agent.spec.template.spec.imagePullSecrets[registry-pull].name" '"registry-pull"' &&
    eq "$F" "Deployment/opamp-management.spec.template.spec.imagePullSecrets[registry-pull].name" '"registry-pull"' &&
    eq "$F" "StatefulSet/opamp-postgres.spec.template.spec.imagePullSecrets[registry-pull].name" '"registry-pull"'
}
run_case "imagePullSecrets on both servers' and Postgres's pods" pull_secrets

# ---------------------------------------------------------------------------
echo "# the other values"

FS="Deployment/opamp-agent.spec.template.spec"
FSM="Deployment/opamp-management.spec.template.spec"
full_image() {
  local s
  for s in "$FS" "$FSM"; do
    eq "$F" "$s.containers[serve].image" '"ghcr.io/telflo/opamp-server:0.1.0@sha256:0000000000000000000000000000000000000000000000000000000000000001"' &&
      eq "$F" "$s.containers[serve].imagePullPolicy" '"Always"' || return 1
  done
}
run_case "image.digest pins both servers' image" full_image
image_values() {
  next_file
  render_flat "$f" "${B[@]}" --set-string image.repository=registry.invalid/mirror/opamp-server,image.tag=9.9.9 || return 1
  eq "$f" "$SRV.image" '"registry.invalid/mirror/opamp-server:9.9.9"' && eq "$f" "$MIG.image" '"registry.invalid/mirror/opamp-server:9.9.9"' &&
    eq "$f" "$SRVM.image" '"registry.invalid/mirror/opamp-server:9.9.9"' && eq "$f" "$MIGM.image" '"registry.invalid/mirror/opamp-server:9.9.9"'
}
run_case "image.repository and image.tag pick the image" image_values
tunables() {
  local S="$FS.containers[serve]"
  eq "$F" "$S.env[OPAMP_HEARTBEAT_INTERVAL_SECONDS].value" '"60"' && eq "$F" "$S.env[OPAMP_DEFAULT_REQUESTS_PER_MINUTE].value" '"24000"' &&
    eq "$F" "$S.env[OPAMP_COLLECTOR_REQUESTS_PER_MINUTE].value" '"120"' && eq "$F" "$S.env[OPAMP_MAX_CONCURRENT_REQUESTS].value" '"200"' &&
    eq "$F" "$S.env[OPAMP_MAX_CONCURRENT_REQUESTS_PER_ORGANIZATION].value" '"50"' && eq "$F" "$S.env[OPAMP_SHUTDOWN_DRAIN_SECONDS].value" '"10"' &&
    eq "$F" "$S.env[OTEL_EXPORTER_OTLP_ENDPOINT].value" '"http://otel-collector.observability.svc:4317"' &&
    eq "$F" "$S.env[OTEL_SERVICE_NAME].value" '"opamp-server"' && eq "$F" "$S.env[OTEL_RESOURCE_ATTRIBUTES].value" '"deployment.environment=test"' &&
    eq "$F" "$FS.terminationGracePeriodSeconds" 60
}
run_case "the tunables and telemetry reach the agent's serve variables (strings)" tunables
tunables_management() {
  local S="$FSM.containers[serve]" v
  eq "$F" "$S.env[OPAMP_SHUTDOWN_DRAIN_SECONDS].value" '"10"' &&
    eq "$F" "$S.env[OTEL_EXPORTER_OTLP_ENDPOINT].value" '"http://otel-collector.observability.svc:4317"' &&
    eq "$F" "$S.env[OTEL_SERVICE_NAME].value" '"opamp-server"' && eq "$F" "$FSM.terminationGracePeriodSeconds" 60 || return 1
  for v in OPAMP_HEARTBEAT_INTERVAL_SECONDS OPAMP_DEFAULT_REQUESTS_PER_MINUTE OPAMP_COLLECTOR_REQUESTS_PER_MINUTE \
    OPAMP_MAX_CONCURRENT_REQUESTS OPAMP_MAX_CONCURRENT_REQUESTS_PER_ORGANIZATION OPAMP_SERVER_URL; do
    no_path "$F" "$S.env[$v]" || return 1
  done
}
run_case "the management API gets the drain and telemetry, never the agent endpoint's heartbeat, limits or server URL" tunables_management
large_tunable() {
  next_file
  render_flat "$f" "${B[@]}" --set server.defaultRequestsPerMinute=12000000 || return 1
  eq "$f" "$SRV.env[OPAMP_DEFAULT_REQUESTS_PER_MINUTE].value" '"12000000"'
}
run_case "a large tunable renders as digits, never in exponent form" large_tunable
scheduling() {
  eq "$F" "$FS.nodeSelector.kubernetes.io/os" '"linux"' && eq "$F" "$FS.tolerations[0].key" '"example.com/dedicated"' &&
    eq "$F" "$FS.affinity.podAntiAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].weight" 100 &&
    eq "$F" "$FS.topologySpreadConstraints[0].topologyKey" '"topology.kubernetes.io/zone"' &&
    eq "$F" "$FS.priorityClassName" '"example-high"' &&
    eq "$F" "$FSM.nodeSelector.kubernetes.io/os" '"linux"' && eq "$F" "$FSM.priorityClassName" '"example-high"' &&
    eq "$F" "Deployment/opamp-agent.spec.template.metadata.annotations.example.com/team" '"fleet"' &&
    eq "$F" "Deployment/opamp-agent.spec.template.metadata.labels.example.com/tier" '"backend"' &&
    eq "$F" "Deployment/opamp-management.spec.template.metadata.labels.example.com/tier" '"backend"' &&
    eq "$F" "$FS.containers[serve].resources.limits.memory" '"256Mi"' &&
    eq "$F" "$FS.initContainers[migrate].resources.limits.memory" '"256Mi"' &&
    eq "$F" "$FSM.containers[serve].resources.limits.memory" '"128Mi"' &&
    eq "$F" "$FSM.initContainers[migrate].resources.limits.memory" '"128Mi"' &&
    eq "$F" "StatefulSet/opamp-postgres.spec.template.metadata.annotations.example.com/backup" '"daily"' &&
    eq "$F" "StatefulSet/opamp-postgres.spec.template.spec.nodeSelector.kubernetes.io/os" '"linux"' &&
    eq "$F" "StatefulSet/opamp-postgres.spec.template.spec.containers[postgres].resources.requests.memory" '"512Mi"'
}
run_case "scheduling, pod annotations and labels passed through to both; each component's resources to its own containers" scheduling
pod_labels_not_selector() { no_path "$F" "Deployment/opamp-agent.spec.selector.matchLabels.example.com/tier" && no_path "$F" "Deployment/opamp-management.spec.selector.matchLabels.example.com/tier"; }
run_case "podLabels stay out of the selector" pod_labels_not_selector
sa_values() {
  eq "$F" "ServiceAccount/opamp-sa.metadata.annotations.example.com/purpose" '"opamp"' && eq "$F" "$FS.serviceAccountName" '"opamp-sa"' &&
    eq "$F" "$FSM.serviceAccountName" '"opamp-sa"'
}
run_case "serviceAccount.name and annotations" sa_values
sa_off() {
  next_file
  render_flat "$f" "${B[@]}" --set serviceAccount.create=false || return 1
  ! kinds "$f" | grep -q '^ServiceAccount/' && eq "$f" "$POD.serviceAccountName" '"default"' && eq "$f" "$POD.automountServiceAccountToken" false &&
    eq "$f" "$PODM.serviceAccountName" '"default"' && eq "$f" "$PODM.automountServiceAccountToken" false
}
run_case "serviceAccount.create false: no account rendered, the default one used, still no token" sa_off
names_override() {
  next_file
  render_flat "$f" "${B[@]}" --set-string fullnameOverride=edge || return 1
  [ "$(kinds "$f" | tr '\n' ' ')" = "ConfigMap/edge-bootstrap Deployment/edge-agent Deployment/edge-management Service/edge-agent Service/edge-management Service/edge-postgres ServiceAccount/edge StatefulSet/edge-postgres " ] &&
    eq "$f" "Deployment/edge-agent.spec.template.spec.containers[serve].env[OPAMP_DATABASE_URL].value" '"postgresql://telflo_opamp:$(OPAMP_RUNTIME_PASSWORD)@edge-postgres:5432/telflo_opamp?sslmode=disable"'
}
run_case "fullnameOverride names every resource and the Postgres host" names_override
name_override() {
  next_file
  render_flat "$f" "${B[@]}" --set-string nameOverride=opamp || return 1
  kinds "$f" | grep -qx 'Deployment/rel-opamp-agent' && eq "$f" "Deployment/rel-opamp-agent.spec.selector.matchLabels.app.kubernetes.io/name" '"opamp"'
}
run_case "nameOverride" name_override
release_holds_name() {
  next_file
  helm template my-opamp-server "$CHART_DIR" -n "$NS" "${B[@]}" >"$f.yaml" 2>"$f.err" && flat "$f.yaml" "$f" || return 1
  kinds "$f" | grep -qx 'Deployment/my-opamp-server-agent'
}
run_case "a release whose name holds the chart's is its own fullname" release_holds_name
long_name() {
  next_file
  helm template "$(printf 'r%.0s' 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25 26 27 28 29 30 31 32 33 34 35 36 37 38 39 40 41 42 43 44 45 46 47 48 49 50)" "$CHART_DIR" -n "$NS" "${B[@]}" >"$f.yaml" 2>"$f.err" && flat "$f.yaml" "$f" || return 1
  local n
  for n in $(kinds "$f" | cut -d/ -f2); do [ "${#n}" -le 63 ] || return 1; done
  [ "$(kinds "$f" | sed -n 's/^StatefulSet\///p' | awk '{ print length($0) }')" -le 52 ]
}
run_case "a long release name: every name fits (the StatefulSet's in 52)" long_name

# ---------------------------------------------------------------------------
echo "# as a dependency of another chart"

subchart() { # <extra parent values...>
  next_file
  local parent="$f.parent"
  mkdir -p "$parent/charts" || return 1
  cp -R "$CHART_DIR" "$parent/charts/opamp-server" || return 1
  rm -rf "$parent/charts/opamp-server/tests"
  printf 'apiVersion: v2\nname: parent\nversion: 0.0.0\ndependencies:\n  - name: opamp-server\n    version: %s\n    condition: opamp-server.enabled\n' "$VERSION" >"$parent/Chart.yaml"
  cat >"$parent/values.yaml" <<'VALUES'
global:
  example: shared
opamp-server:
  enabled: true
  server:
    publicUrl: https://opamp.example.com/opamp/api/v1
  auth:
    existingSecret: opamp-server-credentials
  postgres:
    enabled: false
  externalDatabase:
    host: db.example.com
    sslmode: require
VALUES
  helm template p "$parent" -n "$NS" "$@" >"$f.yaml" 2>"$f.err" && flat "$f.yaml" "$f" || return 1
  kinds "$f" | grep -qx 'Deployment/p-opamp-server-agent' && kinds "$f" | grep -qx 'Deployment/p-opamp-server-management' &&
    ! kinds "$f" | grep -q '^StatefulSet/'
}
run_case "renders as a subchart (the parent sets global and enabled; the schema allows both at its root)" subchart
sub_disabled() { next_file; subchart --set opamp-server.enabled=false; [ $? -ne 0 ] && [ -s "$f.yaml" ] && ! grep -q 'kind: Deployment' "$f.yaml"; }
run_case "a parent's condition (enabled false) leaves it out" sub_disabled
run_case "enabled must be a boolean" schema_refused /enabled "want boolean" "${B[@]}" --set-string enabled=cnry
run_case "global must be an object" schema_refused /global "want object" "${B[@]}" --set-string global=cnry

# ---------------------------------------------------------------------------
echo "# required values and the template's refusals"

M_AUTH="auth.existingSecret is required: the name of the Secret holding the server's credentials"
M_URL="server.publicUrl is required: the agent endpoint's URL as collectors reach it, such as https://opamp.example.com/opamp/api/v1"
M_PG="postgres.auth.existingSecret is required while postgres.enabled is true: the name of the Secret holding postgres-password"
M_HOST="externalDatabase.host is required while postgres.enabled is false"
M_DB="externalDatabase.database is required while postgres.enabled is false"
M_SSL="externalDatabase.sslmode is required while postgres.enabled is false"
M_KEYS="externalDatabase.urlKeys.runtime and externalDatabase.urlKeys.migration must be set together, or neither"
M_GRACE="terminationGracePeriodSeconds must be at least server.shutdownDrainSeconds plus 22"
M_NODEPORT="service.agent.nodePort needs service.agent.type NodePort or LoadBalancer"
M_RANGES="service.agent.loadBalancerSourceRanges needs service.agent.type LoadBalancer"
M_PDB="server.agent.podDisruptionBudget needs exactly one of minAvailable and maxUnavailable"
M_PDBM="server.management.podDisruptionBudget needs exactly one of minAvailable and maxUnavailable"

run_case "no values at all: refused, naming auth.existingSecret" refused "$M_AUTH" ""
run_case "server.publicUrl missing: refused, naming it" refused "$M_URL" "" "${B[@]}" --set-string server.publicUrl=
run_case "auth.existingSecret missing: refused" refused "$M_AUTH" "" "${B[@]}" --set-string auth.existingSecret=
run_case "postgres.auth.existingSecret missing with the bundled Postgres: refused" refused "$M_PG" "" "${B[@]}" --set-string postgres.auth.existingSecret=
run_case "externalDatabase.host missing: refused" refused "$M_HOST" "" "${EXT[@]}" --set-string externalDatabase.host=
run_case "externalDatabase.database empty: refused" refused "$M_DB" "" "${EXT[@]}" --set-string externalDatabase.database=
run_case "externalDatabase.sslmode missing: refused" refused "$M_SSL" "" "${EXT[@]}" --set-string externalDatabase.sslmode=
run_case "urlKeys.runtime alone: refused" refused "$M_KEYS" cnry-runtime-only "${URLS[@]}" --set-string externalDatabase.urlKeys.runtime=cnry-runtime-only,externalDatabase.urlKeys.migration=
run_case "urlKeys.migration alone: refused" refused "$M_KEYS" cnry-migration-only "${URLS[@]}" --set-string externalDatabase.urlKeys.runtime=,externalDatabase.urlKeys.migration=cnry-migration-only
run_case "a grace period shorter than the drain plus 22: refused" refused "$M_GRACE" "" "${B[@]}" --set server.shutdownDrainSeconds=10
grace_ok() { renders "${B[@]}" --set server.shutdownDrainSeconds=10 --set terminationGracePeriodSeconds=32; }
run_case "a grace period of the drain plus 22 renders" grace_ok
run_case "a nodePort with a ClusterIP agent Service: refused" refused "$M_NODEPORT" "" "${B[@]}" --set service.agent.nodePort=30111
run_case "loadBalancerSourceRanges without a LoadBalancer: refused" refused "$M_RANGES" "" "${B[@]}" --set-string service.agent.type=NodePort --set 'service.agent.loadBalancerSourceRanges={10.9.9.0/24}'
run_case "a PDB with both minAvailable and maxUnavailable: refused, naming its component" refused "$M_PDB" "" "${B[@]}" --set server.agent.podDisruptionBudget.enabled=true --set server.agent.podDisruptionBudget.minAvailable=1
run_case "a PDB with neither: refused, naming its component" refused "$M_PDBM" "" "${B[@]}" --set server.management.podDisruptionBudget.enabled=true --set server.management.podDisruptionBudget.maxUnavailable=null
show_only_guarded() { refused "$M_URL" "" "${B[@]}" --set-string server.publicUrl= --show-only templates/serviceaccount.yaml; }
run_case "a guard stops a render of one template alone" show_only_guarded

# ---------------------------------------------------------------------------
echo "# the schema's refusals (canary values)"

run_case "an unknown key at the root refused" schema_refused "" "additional properties 'cnryKey' not allowed" "${B[@]}" --set cnryKey=1
run_case "a typo under server refused" schema_refused /server "additional properties 'publicUrll' not allowed" "${B[@]}" --set-string server.publicUrll=cnry
run_case "a typo under postgres.image refused" schema_refused /postgres/image "additional properties 'digests' not allowed" "${B[@]}" --set-string postgres.image.digests=cnry
run_case "a typo under externalDatabase refused" schema_refused /externalDatabase "additional properties 'hostname' not allowed" "${EXT[@]}" --set-string externalDatabase.hostname=cnry
run_case "a typo under service.agent refused" schema_refused /service/agent "additional properties 'annotation' not allowed" "${B[@]}" --set-string service.agent.annotation.x=cnry
run_case "server.publicUrl not http(s) refused" schema_refused /server/publicUrl "does not match pattern" "${B[@]}" --set-string server.publicUrl=ftp://cnry.invalid/x
run_case "server.publicUrl with a space refused" schema_refused /server/publicUrl "does not match pattern" "${B[@]}" --set-string 'server.publicUrl=https://cnry.invalid/a b'
run_case "service.agent.port over 65535 refused" schema_refused /service/agent/port "maximum" "${B[@]}" --set service.agent.port=70000
run_case "service.agent.port 0 refused" schema_refused /service/agent/port "minimum" "${B[@]}" --set service.agent.port=0
run_case "externalDatabase.port over 65535 refused" schema_refused /externalDatabase/port "maximum" "${EXT[@]}" --set externalDatabase.port=65536
run_case "server.shutdownDrainSeconds over 30 refused" schema_refused /server/shutdownDrainSeconds "maximum" "${B[@]}" --set server.shutdownDrainSeconds=31
run_case "server.heartbeatIntervalSeconds under 10 refused" schema_refused /server/heartbeatIntervalSeconds "minimum" "${B[@]}" --set server.heartbeatIntervalSeconds=5
run_case "terminationGracePeriodSeconds under 30 refused" schema_refused /terminationGracePeriodSeconds "minimum" "${B[@]}" --set terminationGracePeriodSeconds=29
run_case "image.digest not a sha256 refused" schema_refused /image/digest "does not match pattern" "${B[@]}" --set-string image.digest=sha256:cnry
run_case "postgres.image.pullPolicy outside the enum refused" schema_refused /postgres/image/pullPolicy "value must be one of" "${B[@]}" --set-string postgres.image.pullPolicy=Cnry
run_case "externalDatabase.sslmode outside the enum refused" schema_refused /externalDatabase/sslmode "value must be one of" "${EXT[@]}" --set-string externalDatabase.sslmode=cnry
run_case "externalDatabase.host with a user part refused" schema_refused /externalDatabase/host "does not match pattern" "${EXT[@]}" --set-string externalDatabase.host=cnry@db.invalid
run_case "postgres.database not an identifier refused" schema_refused /postgres/database "does not match pattern" "${B[@]}" --set-string 'postgres.database=cnry-db'
run_case "auth.existingSecret not a Kubernetes name refused" schema_refused /auth/existingSecret "does not match pattern" "${B[@]}" --set-string auth.existingSecret=Cnry_Secret
run_case "fullnameOverride over 43 characters refused" schema_refused /fullnameOverride "maxLength" "${B[@]}" --set-string fullnameOverride=cnry-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
run_case "an extraEnv entry with an unknown key refused" schema_refused /extraEnv/0 "additional properties 'secret' not allowed" "${B[@]}" --set-string 'extraEnv[0].name=CNRY,extraEnv[0].secret=cnry'
run_case "a negative replicas refused" schema_refused /server/agent/replicas "minimum" "${B[@]}" --set server.agent.replicas=-1
for old_key in replicas resources podDisruptionBudget; do
  run_case "the values before the split: a top-level $old_key refused (it is server.agent's and server.management's now)" \
    schema_refused "" "additional properties '$old_key' not allowed" "${B[@]}" --set-json "$old_key={}"
done
run_case "a typo under server.management refused" schema_refused /server/management "additional properties 'replica' not allowed" "${B[@]}" --set server.management.replica=2
run_case "image.repository not a repository refused" schema_refused /image/repository "does not match pattern" "${B[@]}" --set-string 'image.repository=cnry/UPPER'
run_case "image.tag with a colon refused" schema_refused /image/tag "does not match pattern" "${B[@]}" --set-string 'image.tag=cn:ry'
run_case "postgres.image.repository with a space refused" schema_refused /postgres/image/repository "does not match pattern" "${B[@]}" --set-string 'postgres.image.repository=cnry repo'
run_case "postgres.image.tag empty refused" schema_refused /postgres/image/tag "minLength" "${B[@]}" --set-string postgres.image.tag=
run_case "postgres.image.digest not a sha256 refused" schema_refused /postgres/image/digest "does not match pattern" "${B[@]}" --set-string postgres.image.digest=cnry
run_case "priorityClassName not a Kubernetes name refused" schema_refused /priorityClassName "does not match pattern" "${B[@]}" --set-string priorityClassName=Cnry_Class
run_case "auth.encryptionKeysPrevious with an unknown key refused" schema_refused /auth/encryptionKeysPrevious "additional properties 'enable' not allowed" "${B[@]}" --set auth.encryptionKeysPrevious.enable=true
for selector_key in app.kubernetes.io/name app.kubernetes.io/instance app.kubernetes.io/component; do
  run_case "podLabels may not set the selector's $selector_key" schema_refused "" "invalid propertyName '$selector_key'" "${B[@]}" \
    --set-string "podLabels.$(printf '%s' "$selector_key" | sed 's/\./\\./g')=cnry"
done
other_pod_label() { renders "${B[@]}" --set-string 'podLabels.app\.kubernetes\.io/part-of=cnry'; }
run_case "podLabels may set another app.kubernetes.io/ label" other_pod_label

# ---------------------------------------------------------------------------
echo "# no value can add YAML structure"

# Every string the chart's values hold (values.yaml's, and those of a few
# map and list entries added for the purpose) is set, by a throwaway chart
# that walks them, to one holding a newline and YAML. Rendered without the
# schema (which refuses most such values), the parsed objects must have
# exactly the shape the same values set to a plain word give: no key added
# anywhere, none moved, nothing that fails to parse.
mkdir -p "$tmp/inject/templates" "$tmp/shape/templates" || exit 2
printf 'apiVersion: v2\nname: inject\nversion: 0.0.0\n' >"$tmp/inject/Chart.yaml"
cp "$CHART_DIR/values.yaml" "$tmp/inject/values.yaml" || exit 2
cat >"$tmp/inject/extras.yaml" <<'VALUES'
podAnnotations: {example.com/a: ""}
podLabels: {example.com/b: ""}
nodeSelector: {example.com/c: ""}
imagePullSecrets: [{name: ""}]
extraEnv: [{name: EXTRA, value: ""}]
tolerations: [{key: example.com/d, operator: Equal, value: "", effect: NoSchedule}]
server: {agent: {resources: {limits: {memory: ""}}, podDisruptionBudget: {enabled: true}}, management: {resources: {limits: {memory: ""}}, podDisruptionBudget: {enabled: true}}}
priorityClassName: ""
serviceAccount: {annotations: {example.com/e: ""}}
service: {agent: {annotations: {example.com/f: ""}}, management: {annotations: {example.com/g: ""}}}
networkPolicy: {enabled: true, management: {from: [{podSelector: {matchLabels: {example.com/h: ""}}}]}}
auth: {encryptionKeysPrevious: {enabled: true}}
postgres: {podAnnotations: {example.com/i: ""}, nodeSelector: {example.com/j: ""}, resources: {limits: {memory: ""}}}
VALUES
cat >"$tmp/inject/templates/values.yaml" <<'TEMPLATE'
{{- define "inject" }}
{{- $v := index . 0 }}
{{- $s := index . 1 }}
{{- if kindIs "map" $v }}
{{- range $k, $x := $v }}
{{- if kindIs "string" $x }}{{ $_ := set $v $k $s }}{{ else }}{{ $_ := include "inject" (list $x $s) }}{{ end }}
{{- end }}
{{- else if kindIs "slice" $v }}
{{- range $x := $v }}{{ $_ := include "inject" (list $x $s) }}{{ end }}
{{- end }}
{{- end }}
{{- $values := deepCopy (omit .Values "inject") }}
{{- $_ := include "inject" (list $values .Values.inject) }}
{{ toYaml $values }}
TEMPLATE
# The shape of a manifest stream: every path (list elements by index, the
# documents by their order and kind) and its leaf's type, never a value.
printf 'apiVersion: v2\nname: shape\nversion: 0.0.0\n' >"$tmp/shape/Chart.yaml"
cat >"$tmp/shape/templates/list.yaml" <<'TEMPLATE'
{{- define "walk" }}
{{- $p := index . 0 }}
{{- $v := index . 1 }}
{{- if kindIs "map" $v }}
# S {{ $p }} map
{{- range $k, $x := $v }}{{ include "walk" (list (printf "%s.%s" $p $k) $x) }}{{ end }}
{{- else if kindIs "slice" $v }}
# S {{ $p }} list
{{- range $i, $x := $v }}{{ include "walk" (list (printf "%s[%d]" $p $i) $x) }}{{ end }}
{{- else }}
# S {{ $p }} {{ kindOf $v }}
{{- end }}
{{- end }}
{{- range $i, $raw := regexSplit "(?m)^---$" .Values.doc -1 }}
{{- $d := fromYaml $raw }}
{{- if hasKey $d "Error" }}
# S ERROR
{{- else if hasKey $d "kind" }}
{{- include "walk" (list (printf "%d:%s" $i $d.kind) $d) }}
{{- end }}
{{- end }}
TEMPLATE
# values_with <file holding the string> <values out>
values_with() {
  helm template v "$tmp/inject" -f "$tmp/inject/extras.yaml" --set-file "inject=$1" >"$2.raw" 2>"$2.err" || return 1
  sed '1,/^# Source: /d' "$2.raw" >"$2"
  [ -s "$2" ]
}
# shape_of <values> <out> <extra helm args...>
shape_of() {
  local values="$1" out="$2"; shift 2
  helm template "$REL" "$CHART_DIR" -n "$NS" --skip-schema-validation -f "$values" "$@" >"$out.yaml" 2>"$out.err" || return 1
  helm template s "$tmp/shape" --set-file "doc=$out.yaml" >"$out.raw" 2>"$out.raw.err" || return 1
  sed -n 's/^# S //p' "$out.raw" | sort >"$out"
  [ -s "$out" ] && ! grep -qx 'ERROR' "$out"
}
printf 'plain' >"$tmp/word.txt"
values_with "$tmp/word.txt" "$tmp/word.values"
injected_values() { # <name> <string to inject>: the values file holding it everywhere
  printf '%s' "$2" >"$tmp/inject-$1.txt"
  values_with "$tmp/inject-$1.txt" "$tmp/inject-$1.values"
}
injected_values shallow 'x
  injected: true'
injected_values pod 'x
      injected: true
      automountServiceAccountToken: true'
injected_values root 'x
injected: true'
enough_strings() { # every one of at least 40 string values was replaced
  [ "$(grep -c 'plain' "$tmp/word.values")" -ge 40 ] && ! grep -q '""' "$tmp/word.values" &&
    [ "$(grep -c 'injected: true' "$tmp/inject-shallow.values")" -ge 40 ]
}
run_case "the injection values replace every string value (at least 40)" enough_strings
same_shape() { # <injection name> <automountServiceAccountToken fields expected> <extra helm args...>
  local name="$1" automount="$2"; shift 2
  next_file
  shape_of "$tmp/word.values" "$f.word" "$@" && shape_of "$tmp/inject-$name.values" "$f.injected" "$@" || return 1
  cmp -s "$f.word" "$f.injected" && ! grep -q 'injected' "$f.injected" &&
    [ "$(grep -c '\.automountServiceAccountToken bool$' "$f.injected")" = "$automount" ]
}
EXTERNAL_COMPOSED=(--set postgres.enabled=false --set-string 'externalDatabase.urlKeys.runtime=,externalDatabase.urlKeys.migration=')
for inj in shallow pod root; do
  run_case "a newline and YAML ($inj) in every string value adds no structure: the bundled Postgres" same_shape "$inj" 4
  run_case "a newline and YAML ($inj) in every string value adds no structure: an external Postgres" same_shape "$inj" 3 "${EXTERNAL_COMPOSED[@]}"
  run_case "a newline and YAML ($inj) in every string value adds no structure: whole-URL keys" same_shape "$inj" 3 --set postgres.enabled=false
done

# ---------------------------------------------------------------------------
echo "# no env value can name another variable"

# Kubernetes expands $(NAME) in a container's env values from the variables
# listed before it, and reads "$$" as one "$". env_refs <listing> prints, for
# each env value, its path and the names of the references Kubernetes would
# expand in it, read as Kubernetes' own expansion does: "$$" is one "$", "$("
# up to the next ")" a reference, an unclosed "$(" and any other "$x" literal.
env_refs() {
  awk '$1 ~ /\.env\[[^]]*\]\.value$/ {
    s = substr($0, length($1) + 2); sub(/^"/, "", s); sub(/"$/, "", s)
    n = length(s); r = ""
    for (i = 1; i < n; i++) {
      if (substr(s, i, 1) != "$") continue
      if (substr(s, i + 1, 1) == "(") {
        j = index(substr(s, i + 2), ")")
        if (j > 0) { r = r " " substr(s, i + 2, j - 1); i = i + 1 + j; continue }
      }
      i++
    }
    print $1 r
  }' "$1"
}
# only_own_refs <listing>: the one expansion is a composed URL's own password
# (migrate's owner, serve's runtime); every other env value names nothing.
only_own_refs() {
  env_refs "$1" >"$1.refs" || return 1
  [ -s "$1.refs" ] || return 1
  awk '
    $1 ~ /\.initContainers\[migrate\]\.env\[OPAMP_MIGRATION_DATABASE_URL\]\.value$/ { if (NF != 2 || $2 != "OPAMP_OWNER_PASSWORD") bad = 1; next }
    $1 ~ /\.containers\[serve\]\.env\[OPAMP_DATABASE_URL\]\.value$/ { if (NF != 2 || $2 != "OPAMP_RUNTIME_PASSWORD") bad = 1; next }
    NF > 1 { bad = 1 }
    END { exit bad }' "$1.refs"
}
env_refs_reads() { # the reader itself, on known strings
  printf '%s\n' 'A.env[a].value "x$(ONE)y"' 'A.env[b].value "x$$(TWO)"' 'A.env[c].value "$$$(THREE)$"' \
    'A.env[d].value "$(unclosed"' 'A.env[e].value "a $x $$ $(FOUR) b"' >"$tmp/refs-known"
  [ "$(env_refs "$tmp/refs-known" | tr '\n' '|')" = 'A.env[a].value ONE|A.env[b].value|A.env[c].value THREE|A.env[d].value|A.env[e].value FOUR|' ]
}
run_case "the reference reader reads \$(…), \$\$ and an unclosed \$( as Kubernetes does" env_refs_reads
own_refs_profiles() { local l; for l in $ALL_LISTINGS; do only_own_refs "$l" || return 1; done; }
run_case "every profile: the composed URLs' own password is the only expansion" own_refs_profiles

# Each plain value from values set to one naming the management secret and
# the runtime password: it renders with each "$" doubled, so Kubernetes
# expands nothing, and the composed URLs keep their one reference.
DOLLAR='x$(OPAMP_MANAGEMENT_SECRET)-$(OPAMP_RUNTIME_PASSWORD)'
DOLLAR_ESCAPED='x$$(OPAMP_MANAGEMENT_SECRET)-$$(OPAMP_RUNTIME_PASSWORD)'
dollar_values() {
  next_file
  render_flat "$f" "${FULLP[@]}" \
    --set-string "server.publicUrl=https://opamp.example.com/$DOLLAR" \
    --set-string "server.telemetry.otlpEndpoint=http://otel.example:4317/$DOLLAR" \
    --set-string "server.telemetry.serviceName=$DOLLAR,server.telemetry.resourceAttributes=k=$DOLLAR" \
    --set-json "extraEnv=[{\"name\":\"EXAMPLE_PLAIN\",\"value\":\"$DOLLAR\"},{\"name\":\"EXAMPLE_EMPTY\",\"value\":\"\"},{\"name\":\"EXAMPLE_FROM_SECRET\",\"valueFrom\":{\"secretKeyRef\":{\"name\":\"example-extra\",\"key\":\"example-key\"}}}]" || return 1
  local S="$FS.containers[serve]"
  eq "$f" "$S.env[OPAMP_SERVER_URL].value" "\"https://opamp.example.com/$DOLLAR_ESCAPED\"" &&
    eq "$f" "$S.env[OTEL_EXPORTER_OTLP_ENDPOINT].value" "\"http://otel.example:4317/$DOLLAR_ESCAPED\"" &&
    eq "$f" "$S.env[OTEL_SERVICE_NAME].value" "\"$DOLLAR_ESCAPED\"" &&
    eq "$f" "$S.env[OTEL_RESOURCE_ATTRIBUTES].value" "\"k=$DOLLAR_ESCAPED\"" &&
    eq "$f" "$S.env[EXAMPLE_PLAIN].value" "\"$DOLLAR_ESCAPED\"" &&
    eq "$f" "$S.env[EXAMPLE_EMPTY].value" '""' &&
    eq "$f" "$S.env[EXAMPLE_FROM_SECRET].valueFrom.secretKeyRef.name" '"example-extra"' && no_path "$f" "$S.env[EXAMPLE_FROM_SECRET].value" &&
    eq "$f" "$S.env[OPAMP_DATABASE_URL].value" '"postgresql://telflo_opamp:$(OPAMP_RUNTIME_PASSWORD)@opamp-postgres:5432/opamp_db?sslmode=disable"' &&
    eq "$f" "$FS.initContainers[migrate].env[OPAMP_MIGRATION_DATABASE_URL].value" '"postgresql://telflo_opamp_owner:$(OPAMP_OWNER_PASSWORD)@opamp-postgres:5432/opamp_db?sslmode=disable"' &&
    eq "$f" "$FSM.containers[serve].env[OTEL_SERVICE_NAME].value" "\"$DOLLAR_ESCAPED\"" &&
    eq "$f" "$FSM.containers[serve].env[EXAMPLE_PLAIN].value" "\"$DOLLAR_ESCAPED\"" &&
    only_own_refs "$f"
}
run_case "publicUrl, the telemetry values and extraEnv's values holding \$(…) render as \$\$(…); the composed URLs keep \$(OPAMP_…_PASSWORD)" dollar_values
dollar_escaped_input() { # a value already holding "$$(" and a last "$" stays literal too
  next_file
  render_flat "$f" "${B[@]}" --set-string 'server.publicUrl=https://opamp.example.com/a$$(OPAMP_MANAGEMENT_SECRET)$' || return 1
  eq "$f" "$SRV.env[OPAMP_SERVER_URL].value" '"https://opamp.example.com/a$$$$(OPAMP_MANAGEMENT_SECRET)$$"' && only_own_refs "$f"
}
run_case "a value holding \$\$(…) renders as \$\$\$\$(…): every \"\$\" doubled, never only the \"\$(\"" dollar_escaped_input

# The class: every string value (as above, without the schema, which refuses
# a "$" in most of them) set to one holding references, an escaped one and a
# last "$". In every container, no env value names a variable but a composed
# URL's own password.
printf '%s' 'x$(OPAMP_MANAGEMENT_SECRET)$(OPAMP_RUNTIME_PASSWORD)$$(OPAMP_OWNER_PASSWORD)$' >"$tmp/dollar.txt"
values_with "$tmp/dollar.txt" "$tmp/dollar.values"
dollar_everywhere() { # <extra helm args...>
  next_file
  helm template "$REL" "$CHART_DIR" -n "$NS" --skip-schema-validation -f "$tmp/dollar.values" "$@" >"$f.yaml" 2>"$f.err" &&
    flat "$f.yaml" "$f" || return 1
  # They reached serve's values: OPAMP_SERVER_URL, the three OTEL_ values and
  # extraEnv's (the resources' names hold the string too).
  [ "$(awk '$1 ~ /\.containers\[serve\]\.env\[[^]]*\]\.value$/ && index($0, " \"x$$(OPAMP_MANAGEMENT_SECRET)$$(OPAMP_RUNTIME_PASSWORD)$$$$(OPAMP_OWNER_PASSWORD)$$\"")' "$f" | wc -l)" -ge 5 ] &&
    only_own_refs "$f"
}
run_case "every string value holding \$(…): no env value names a variable (the bundled Postgres)" dollar_everywhere
run_case "every string value holding \$(…): no env value names a variable (an external Postgres)" dollar_everywhere "${EXTERNAL_COMPOSED[@]}"
run_case "every string value holding \$(…): no env value names a variable (whole-URL keys)" dollar_everywhere --set postgres.enabled=false

# ---------------------------------------------------------------------------
echo "# helm lint --strict with complete profiles"

lints() { next_file; helm lint --strict "$CHART_DIR" -n "$NS" "$@" >"$f" 2>&1; }
run_case "helm lint --strict passes: the bundled Postgres" lints "${B[@]}"
run_case "helm lint --strict passes: an external Postgres" lints "${EXT[@]}"
run_case "helm lint --strict passes: whole-URL keys" lints "${URLS[@]}"
run_case "helm lint --strict passes: every option on" lints "${FULLP[@]}"

echo "# $passed passed, $failed failed"
[ "$failed" -eq 0 ]
