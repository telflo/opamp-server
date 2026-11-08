#!/usr/bin/env bash
# The chart's smoke test: a real install of charts/opamp-server on a
# throwaway minikube profile, checked end to end, then removed.
#
#   scripts/smoke-minikube.sh [--profile <name>] [--work <dir>] [--image <ref>] [--timeout <seconds>]
#   scripts/smoke-minikube.sh --published <version> [--profile <name>] [--work <dir>] [--timeout <seconds>]
#
# `make smoke` builds the image (`make image`) and runs the first form.
#
# What it needs: Docker, minikube, kubectl, helm, openssl, curl and jq; the
# images opamp-server:dev (`make image`; another with --image) and
# postgres:18 already in the local Docker engine (nothing is pulled from a
# registry, except by --published, below); and 6 GB free in Docker's disk.
#
# The first form installs this tree's chart (charts/opamp-server next to
# this script; the tree needn't be a git checkout) with the local image:
#   1. minikube start -p <profile> --driver=docker, a profile of its own
#      (default opamp-smoke). It refuses the profile `minikube` and any profile
#      that exists before it starts.
#   2. minikube image load of the server's image and postgres:18, so the
#      pods pull nothing (image.pullPolicy and postgres.image.pullPolicy
#      Never, both digests empty: a loaded image has no RepoDigest).
#   3. The two Secrets the chart reads (README.md, "Install"), from values
#      made at run time with `openssl rand -hex 32` into files of mode 0600
#      in the work folder, passed to kubectl with --from-file: no value is
#      ever on a command line, printed or in the chart's values.
#   4. helm install, then every pod ready: the agent endpoint's, the
#      management API's and Postgres's. A pod that can't start fails the
#      run at once, naming the pod, its phase and the reason
#      (CreateContainerConfigError, ErrImage*, CrashLoopBackOff, a volume
#      that can't be mounted) and never anything from its environment.
#   5. On the cluster: the Postgres pod's volume claim (owner, group, mode),
#      fsGroup 999, the bootstrap ConfigMap's file modes, and local-socket
#      trust (which the README's bootstrap re-run relies on).
#   6. Through kubectl port-forward on free local ports (never 51234 or
#      51235): /health and /ready on both ports; the
#      management API's 401 without the secret; an organization and a fleet
#      created and read back, the secret sent from a file descriptor; the
#      agent endpoint's 401 without a bearer.
#   7. The agent endpoint's pod deleted, then the management API's: the
#      organization is still there once each new one is ready. The same for
#      the Postgres pod.
#   8. The README's bootstrap re-run command applies bootstrap.sql: a
#      password and a grant the bootstrap sets are undone first (the login
#      and the grant then fail), and after the re-run both are back and the
#      database's roles, grants and settings are as they were before.
#   9. helm upgrade with a changed value of the agent endpoint's, then helm
#      rollback, each rolled out and checked: only the agent's pod rolls,
#      the management API's stays.
#  10. No generated value (nor a bearer the server answered) in any
#      container's log, the stored objects, or Helm's release record; no
#      registry credential Secret in the cluster.
#
# --published <version> is the check after a release is published: on the
# host, it pulls ghcr.io/telflo/opamp-server:<version> and the chart
# oci://ghcr.io/telflo/charts/opamp-server --version <version>, through the
# DOCKER_CONFIG and HELM_REGISTRY_CONFIG the caller exports (a temporary
# login's, its credentials inline; never Docker's or Helm's default ones),
# loads that image into the profile and installs the pulled chart's .tgz.
# No registry credential ever enters the cluster: no pull secret is made.
# The cleanup removes the pulled image only when it wasn't in the local
# engine before the run, judged by its image id and its RepoDigests against
# the engine's images listed before the pull, never by its tag: an image
# pulled earlier by digest has no tag, yet is the same image, and is kept.
#
# Its own Docker and Helm calls use a Docker config and a Helm registry
# config of its own, holding no credential, and refusing stand-ins for the
# credential helpers (the macOS keychain's among them) come first on PATH:
# a call to one fails the run.
#
# It writes nothing under ~/.kube: KUBECONFIG is a file in the work folder
# for minikube start, kubectl, helm and minikube delete, and KUBECACHEDIR
# (kubectl's and Helm's discovery and HTTP caches) a folder there too. As a
# backstop it records your current context, your kubeconfig's sha256 and
# the entries of ~/.kube/cache before it starts, restores the context if it
# changed, and fails if the file changed or ~/.kube/cache gained an entry
# (it only reads that folder). minikube's own logs (--log_dir) go to the
# work folder as well.
#
# A second Ctrl-C (or TERM) during the cleanup is ignored: the cleanup
# always runs to its end.
#
# Cleanup, on every exit: helm uninstall, the volume claims and Secrets
# deleted, minikube delete -p <profile>, and the image-load cache files the
# run created under minikube's cache/images/<arch>/ (only those). Then the
# work folder is searched for every generated value, and a bearer the
# server answered, each also in its base64 form (by path only; the values
# never printed), and the files
# holding them are deleted. The work folder (default a new one under
# TMPDIR) keeps the logs.
#
# Exit status: 0 every check passed and the cleanup completed; 1 a check or
# the cleanup failed; 2 refused, before anything was started.
#
# Bash 3.2 (macOS's).

set -euo pipefail

PROGRAM=smoke-minikube
DEFAULT_PROFILE=opamp-smoke
RELEASE=opamp
NS=opamp
FULLNAME=opamp-opamp-server
SERVER_SECRET=opamp-server
POSTGRES_SECRET=opamp-server-postgres
POSTGRES_POD=$FULLNAME-postgres-0
DATABASE=telflo_opamp
REPLICAS=1
PUBLISHED_IMAGE_REPOSITORY=ghcr.io/telflo/opamp-server
PUBLISHED_CHART=oci://ghcr.io/telflo/charts/opamp-server
# The README's bootstrap re-run (README.md, "Upgrading"), run as written:
# the run fails if the README no longer holds this line.
RERUN_COMMAND="kubectl -n $NS exec $POSTGRES_POD -c postgres -- /docker-entrypoint-initdb.d/initdb.sh"
# 6 GB free in Docker's disk, in 1024-byte blocks.
MIN_DOCKER_FREE_KB=$((6 * 1024 * 1024))

refuse() { echo "$PROGRAM: refused: $*" >&2; exit 2; }
fail() { echo "FAIL: $*" >&2; exit 1; }
usage() { sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//'; }

# ---- The arguments. Nothing is touched before they, and the checks after
# them, pass.
PROFILE=$DEFAULT_PROFILE
WORK=""
IMAGE=""
PUBLISHED=""
WAIT_TIMEOUT=300
while [ $# -gt 0 ]; do
  case "$1" in
    --profile | --work | --image | --published | --timeout)
      [ $# -ge 2 ] || refuse "$1 needs a value"
      case "$1" in
        --profile) PROFILE=$2 ;;
        --work) WORK=$2 ;;
        --image) IMAGE=$2 ;;
        --published) PUBLISHED=$2 ;;
        --timeout) WAIT_TIMEOUT=$2 ;;
      esac
      shift 2
      ;;
    -h | --help) usage; exit 0 ;;
    *) refuse "unknown argument '$1' (--help lists them)" ;;
  esac
done

[[ "$PROFILE" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] ||
  refuse "--profile must be a minikube profile's name: lowercase letters, digits and '-', at most 63 (default $DEFAULT_PROFILE)"
[ "$PROFILE" != minikube ] || refuse "--profile minikube is minikube's own default profile: name a throwaway one (default $DEFAULT_PROFILE)"
[[ "$WAIT_TIMEOUT" =~ ^[1-9][0-9]{0,4}$ ]] || refuse "--timeout must be a number of seconds (default 300)"
if [ -n "$PUBLISHED" ]; then
  [[ "$PUBLISHED" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || refuse "--published takes a released version, such as 0.1.0"
  [ -z "$IMAGE" ] || refuse "--image and --published can't go together: --published installs the published image"
fi
[ -n "$IMAGE" ] || IMAGE=opamp-server:dev
case "$IMAGE" in *@*) refuse "--image takes a tag, not a digest (a loaded image has none)" ;; esac
case "${IMAGE##*/}" in
  *:*) ;;
  *) refuse "--image needs a tag, such as opamp-server:dev" ;;
esac

for tool in minikube kubectl helm docker openssl curl jq; do
  command -v "$tool" >/dev/null 2>&1 || refuse "$tool isn't on PATH"
done
if command -v shasum >/dev/null 2>&1; then
  sha256_of() { shasum -a 256 "$1" | cut -d' ' -f1; }
elif command -v sha256sum >/dev/null 2>&1; then
  sha256_of() { sha256sum "$1" | cut -d' ' -f1; }
else
  refuse "neither shasum nor sha256sum is on PATH"
fi

REPO=$(cd "$(dirname "$0")/.." && pwd -P)
CHART_DIR=$REPO/charts/opamp-server
[ -n "$PUBLISHED" ] || [ -f "$CHART_DIR/Chart.yaml" ] || refuse "no chart at $CHART_DIR"

# The caller's registry configs, for --published's two pulls only: read here,
# before HELM_* is cleared below. Never Docker's or Helm's default ones,
# which may hold your own login or name a credential store.
CALLER_DOCKER_CONFIG=${DOCKER_CONFIG:-}
CALLER_HELM_REGISTRY_CONFIG=${HELM_REGISTRY_CONFIG:-}
if [ -n "$PUBLISHED" ]; then
  [ -n "$CALLER_DOCKER_CONFIG" ] && [ -n "$CALLER_HELM_REGISTRY_CONFIG" ] ||
    refuse "--published pulls through your temporary login's DOCKER_CONFIG and HELM_REGISTRY_CONFIG: export both"
  [ -f "$CALLER_DOCKER_CONFIG/config.json" ] || refuse "DOCKER_CONFIG holds no config.json"
  [ -f "$CALLER_HELM_REGISTRY_CONFIG" ] || refuse "HELM_REGISTRY_CONFIG isn't a file"
  caller_docker_dir=$(cd "$CALLER_DOCKER_CONFIG" && pwd -P)
  [ "$caller_docker_dir" != "$(cd "$HOME" && pwd -P)/.docker" ] ||
    refuse "DOCKER_CONFIG is Docker's default folder: use a temporary login's own folder"
  # Helm's own folders (macOS's, and XDG's), each resolved, against the
  # file's resolved folder: a relative path or a link can't hide one.
  resolved() { if [ -d "$1" ]; then (cd "$1" && pwd -P); else echo "$1"; fi; }
  caller_helm_dir=$(cd "$(dirname "$CALLER_HELM_REGISTRY_CONFIG")" && pwd -P)
  for helm_dir in "$HOME/Library/Preferences/helm" "${XDG_CONFIG_HOME:-$HOME/.config}/helm" "$HOME/.config/helm"; do
    case "$caller_helm_dir/" in
      "$(resolved "$helm_dir")/"*)
        refuse "HELM_REGISTRY_CONFIG is in Helm's own folder ($helm_dir): use a temporary login's own file" ;;
    esac
  done
  # A credential store or helper would be asked for the credentials: the
  # login must keep them inline. Keys only are read, never a value. Docker
  # and Helm match keys case-insensitively (Go's encoding/json, whose folding
  # also takes U+017F for "s"), so a key is compared folded: a store or
  # helper in any spelling is refused, and so is a key that folds to "auths"
  # but isn't "auths".
  for file in "$CALLER_DOCKER_CONFIG/config.json" "$CALLER_HELM_REGISTRY_CONFIG"; do
    jq -e 'type == "object" and (.auths | type) == "object" and (.auths | length) > 0' "$file" >/dev/null 2>&1 ||
      refuse "$file isn't JSON with a non-empty \"auths\" object (a login's credentials, inline)"
    if ! jq -e '[keys[] | ascii_downcase | gsub("\u017f"; "s") | select(. == "credsstore" or . == "credhelpers")] | length == 0' "$file" >/dev/null 2>&1; then
      refuse "$file names a credential store or helper (credsStore or credHelpers, in any spelling): the login must keep its credentials inline"
    fi
    if ! jq -e '[keys[] | select(. != "auths") | ascii_downcase | gsub("\u017f"; "s") | select(. == "auths")] | length == 0' "$file" >/dev/null 2>&1; then
      refuse "$file has a key spelled like \"auths\" that isn't \"auths\""
    fi
  done
fi

# The work folder: a new one, or an empty one, never inside this tree.
if [ -z "$WORK" ]; then
  WORK=$(mktemp -d "${TMPDIR:-/tmp}/opamp-smoke.XXXXXX")
else
  if [ -e "$WORK" ]; then
    [ -d "$WORK" ] && [ ! -L "$WORK" ] || refuse "--work $WORK isn't a directory"
    [ -z "$(ls -A "$WORK")" ] || refuse "--work $WORK isn't empty: name a new or an empty folder"
  else
    mkdir -p "$WORK"
  fi
fi
WORK=$(cd "$WORK" && pwd -P)
case "$WORK/" in "$REPO/"*) refuse "--work $WORK is inside this tree: the generated secrets never go in it" ;; esac
chmod 0700 "$WORK"

# ---- The backstop for your kubeconfig, recorded before anything else runs:
# each file kubectl reads for you (KUBECONFIG's, else ~/.kube/config), its
# sha256, and the current context. kubectl only reads it here (`config
# current-context` asks no server), and only when a file exists.
GUARDED_FILES=""
add_guarded() {
  local each
  for each in $GUARDED_FILES; do [ "$each" != "$1" ] || return 0; done
  GUARDED_FILES="$GUARDED_FILES $1"
}
case "$HOME" in *[[:space:]]*) refuse "HOME holds a space, which this script doesn't handle" ;; esac
add_guarded "$HOME/.kube/config"
if [ -n "${KUBECONFIG:-}" ]; then
  old_ifs=$IFS
  IFS=:
  for file in $KUBECONFIG; do
    case "$file" in *[[:space:]]*) refuse "KUBECONFIG names a file with a space, which this script doesn't handle" ;; esac
    [ -z "$file" ] || add_guarded "$file"
  done
  IFS=$old_ifs
fi
CALLER_KUBECONFIG=${KUBECONFIG:-}
guarded_state() {
  local file
  for file in $GUARDED_FILES; do
    if [ -f "$file" ]; then echo "$file $(sha256_of "$file")"; else echo "$file absent"; fi
  done
}
caller_context() {
  local file any=no
  for file in $GUARDED_FILES; do [ -s "$file" ] && any=yes; done
  [ "$any" = yes ] || return 0
  if [ -n "$CALLER_KUBECONFIG" ]; then
    KUBECONFIG=$CALLER_KUBECONFIG kubectl config current-context 2>/dev/null || true
  else
    env -u KUBECONFIG kubectl config current-context 2>/dev/null || true
  fi
}
GUARDED_BEFORE=$(guarded_state)
CONTEXT_BEFORE=$(caller_context)
# kubectl's and Helm's caches for you (KUBECACHEDIR's, else ~/.kube/cache):
# their entries two levels down (discovery/<host>_<port>, http/<entry>),
# listed only, never written.
CALLER_KUBE_CACHE=${KUBECACHEDIR:-$HOME/.kube/cache}
kube_cache_entries() {
  if [ -d "$CALLER_KUBE_CACHE" ]; then find "$CALLER_KUBE_CACHE" -mindepth 1 -maxdepth 2 2>/dev/null | LC_ALL=C sort; fi
  return 0
}
KUBE_CACHE_BEFORE=$(kube_cache_entries)

# ---- The run's own folders and settings.
umask 077
STATE=$WORK/state
LOGS=$WORK/logs
SECRETS_DIR=$WORK/secrets
VALUES=$SECRETS_DIR/values
HEADER=$SECRETS_DIR/management-header
REPLY_BODY=$SECRETS_DIR/reply-body
REPLY_HEADERS=$SECRETS_DIR/reply-headers
STEPS=$WORK/steps.txt
mkdir -p "$STATE" "$LOGS" "$SECRETS_DIR" "$WORK/release" "$WORK/published"
: >"$STEPS"

# Helm reads where it connects, its storage, plugins and homes from HELM_*
# variables: each one set is unset (its name noted, never its value), so
# Helm reads only this run's KUBECONFIG and homes.
helm_unset=""
for name in ${!HELM_@}; do
  helm_unset="$helm_unset $name"
  unset "$name"
done
export HELM_CACHE_HOME="$WORK/helm/cache" HELM_CONFIG_HOME="$WORK/helm/config" HELM_DATA_HOME="$WORK/helm/data"
export HELM_REGISTRY_CONFIG="$WORK/helm/registry.json" HELM_REPOSITORY_CACHE="$WORK/helm/cache/repository"
export HELM_REPOSITORY_CONFIG="$WORK/helm/config/repositories.yaml" HELM_PLUGINS="$WORK/helm/data/plugins"
mkdir -p "$HELM_CACHE_HOME" "$HELM_CONFIG_HOME" "$HELM_DATA_HOME" "$WORK/docker-config"
# A Docker config and a Helm registry config holding an auth entry for a
# name that resolves nowhere, so neither looks for a credential store, and
# refusing stand-ins for the credential helpers first on PATH: each logs its
# name and first argument only, never reads its input, and exits 1.
printf '{"auths":{"opamp-smoke.invalid":{}}}\n' >"$WORK/docker-config/config.json"
printf '{"auths":{"opamp-smoke.invalid":{}}}\n' >"$HELM_REGISTRY_CONFIG"
CALLER_DOCKER_CONFIG_SET=${DOCKER_CONFIG+set}
export DOCKER_CONFIG="$WORK/docker-config"
HELPERS_LOG=$WORK/credential-helpers.log
mkdir -p "$WORK/bin"
: >"$HELPERS_LOG"
for helper in docker-credential-osxkeychain docker-credential-desktop docker-credential-ecr-login; do
  printf '#!/bin/sh\n# A refusing stand-in (smoke-minikube.sh): logs its name and first argument, never reads its input.\nprintf "%%s %%s\\n" "%s" "$1" >>"%s"\nexit 1\n' \
    "$helper" "$HELPERS_LOG" >"$WORK/bin/$helper"
  chmod 0700 "$WORK/bin/$helper"
done
export PATH="$WORK/bin:$PATH"
export KUBECONFIG="$WORK/kubeconfig"
# kubectl and Helm keep their discovery and HTTP caches here, not under
# ~/.kube (KUBECONFIG doesn't move them).
export KUBECACHEDIR="$WORK/kube-cache"
export MINIKUBE_IN_STYLE=false
# minikube's own log files (--log_dir), in the work folder, where the
# search reaches them.
MINIKUBE_LOGS=$LOGS/minikube
mkdir -p "$MINIKUBE_LOGS"
mk() { minikube --log_dir "$MINIKUBE_LOGS" "$@"; }
MINIKUBE_DIR=${MINIKUBE_HOME:-$HOME}
[ "$(basename "$MINIKUBE_DIR")" = .minikube ] || MINIKUBE_DIR=$MINIKUBE_DIR/.minikube
MINIKUBE_CACHE=$MINIKUBE_DIR/cache/images

# ---- The run's state, which the cleanup reads.
STARTED_AT=$SECONDS
STEP=0
CURRENT_STEP=""
profile_created=no
release_installed=no
pulled_image=no
FORWARD_PIDS=""
CLEANUP_FAILURES=""
LOADED_CACHE_NAMES=""

elapsed() { local seconds=$((SECONDS - STARTED_AT)); echo "$((seconds / 60)) min $((seconds % 60)) s"; }
step() {
  STEP=$((STEP + 1))
  CURRENT_STEP="$STEP. $*"
  echo "--- $CURRENT_STEP (at $(elapsed))"
}
pass() {
  echo "    ok: $*"
  echo "PASS $STEP. $*" >>"$STEPS"
}
note() { echo "    $*"; }
# redact: anything that could be a value (32 or more characters of base64,
# base64url or hex: the generated values are 64, a bearer longer) is masked
# in what's shown.
redact() { LC_ALL=C sed -E 's#[A-Za-z0-9+/_=-]{32,}#[redacted]#g'; }
shown() { tail -n "${2:-15}" "$1" 2>/dev/null | redact | sed 's/^/      /'; }
# holds_value <file>: true when the file holds a generated value or a
# bearer, or its base64 form (never printed).
holds_value() { [ -s "$VALUES" ] && LC_ALL=C grep -qaFf "$VALUES" "$1"; }
# add_values <file>: each non-empty line of the file, and its base64 form
# (as a Secret's data holds it), into the values searched for. The values
# go through files, a pipe and printf (a shell builtin), never a command
# line.
add_values() {
  local line
  while IFS= read -r line || [ -n "$line" ]; do
    [ -n "$line" ] || continue
    printf '%s\n' "$line" >>"$VALUES"
    { printf '%s' "$line" | base64 | tr -d '\n'; printf '\n'; } >>"$VALUES"
    VALUE_COUNT=$((VALUE_COUNT + 1))
  done <"$1"
}
VALUE_COUNT=0

# kube and helmk: kubectl and helm only ever against this run's profile, by
# its context in this run's kubeconfig, which must exist and be non-empty
# (kubectl without one would ask http://localhost:8080).
kube() {
  [ -s "$KUBECONFIG" ] || fail "this run's kubeconfig ($KUBECONFIG) is missing or empty: kubectl isn't run without it"
  kubectl --kubeconfig "$KUBECONFIG" --context "$PROFILE" -n "$NS" "$@"
}
helmk() {
  [ -s "$KUBECONFIG" ] || fail "this run's kubeconfig ($KUBECONFIG) is missing or empty: helm isn't run without it"
  helm --kubeconfig "$KUBECONFIG" --kube-context "$PROFILE" -n "$NS" "$@"
}
helpers_called() {
  [ -s "$HELPERS_LOG" ] || return 1
  sort -u "$HELPERS_LOG" | tr '\n' ';'
}

# ---- Cleanup, on every exit.
cleanup_failed() {
  echo "    cleanup FAILED: $*" >&2
  CLEANUP_FAILURES="$CLEANUP_FAILURES; $*"
}
stop_forwards() {
  local pid
  for pid in $FORWARD_PIDS; do
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  done
  FORWARD_PIDS=""
}
cleanup_cluster() {
  local left
  if [ "$release_installed" = yes ]; then
    echo "--- cleanup: helm uninstall $RELEASE (at $(elapsed))"
    # No --wait: Helm 4's watcher sits out its whole timeout here. The claims'
    # delete below waits for the pods using them to be gone.
    if helmk uninstall "$RELEASE" >"$LOGS/cleanup-uninstall.log" 2>&1; then
      release_installed=no
    else
      cleanup_failed "helm uninstall $RELEASE (log: $LOGS/cleanup-uninstall.log)"
    fi
  fi
  echo "--- cleanup: the volume claims, the Secrets and the namespace $NS"
  kube delete pvc -l "app.kubernetes.io/instance=$RELEASE" --ignore-not-found --wait=true --timeout=120s >"$LOGS/cleanup-pvc.log" 2>&1 ||
    cleanup_failed "deleting the volume claims (log: $LOGS/cleanup-pvc.log)"
  left=$(kube get pvc -o name 2>>"$LOGS/cleanup-pvc.log" | wc -l | tr -d ' ')
  [ "$left" = 0 ] || cleanup_failed "$left volume claims are left in $NS"
  kube delete secret "$SERVER_SECRET" "$POSTGRES_SECRET" --ignore-not-found >"$LOGS/cleanup-secrets.log" 2>&1 ||
    cleanup_failed "deleting the Secrets (log: $LOGS/cleanup-secrets.log)"
  kube delete namespace "$NS" --ignore-not-found --wait=true --timeout=120s >"$LOGS/cleanup-namespace.log" 2>&1 ||
    cleanup_failed "deleting the namespace $NS (log: $LOGS/cleanup-namespace.log)"
}
cleanup_profile() {
  echo "--- cleanup: minikube delete -p $PROFILE (at $(elapsed))"
  if ! mk delete -p "$PROFILE" >"$LOGS/minikube-delete.log" 2>&1; then
    cleanup_failed "minikube delete -p $PROFILE (log: $LOGS/minikube-delete.log)"
    return 0
  fi
  if docker container inspect "$PROFILE" >/dev/null 2>&1 || [ -e "$MINIKUBE_DIR/profiles/$PROFILE" ]; then
    cleanup_failed "the profile $PROFILE is still there after minikube delete"
  else
    profile_created=no
  fi
}
# The cache files minikube image load wrote for this run's images, under
# cache/images/<arch>/: those that weren't there before the run (a file
# there before, from another run, stays), with their temporary siblings.
cleanup_cache() {
  local path rel name each
  [ -n "$LOADED_CACHE_NAMES" ] || return 0
  echo "--- cleanup: the image-load cache files this run made under $MINIKUBE_CACHE"
  if [ -d "$MINIKUBE_CACHE" ]; then find "$MINIKUBE_CACHE" -type f >"$STATE/cache-after.txt" 2>/dev/null || true; else : >"$STATE/cache-after.txt"; fi
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    if LC_ALL=C grep -Fxq -- "$path" "$STATE/cache-before.txt"; then continue; fi
    rel=${path#"$MINIKUBE_CACHE"/}
    name=${rel#*/}
    for each in $LOADED_CACHE_NAMES; do
      case "$name" in
        "$each" | "$each".*.tmp)
          if rm -f -- "$path"; then echo "    removed $path"; else cleanup_failed "removing $path"; fi
          ;;
      esac
    done
  done <"$STATE/cache-after.txt"
  for each in $LOADED_CACHE_NAMES; do
    for path in "$MINIKUBE_CACHE"/*/"$each"; do
      [ -e "$path" ] || continue
      LC_ALL=C grep -Fxq -- "$path" "$STATE/cache-before.txt" && note "kept $path: it was there before this run"
    done
  done
}
cleanup_kubeconfig() {
  local now context
  echo "--- cleanup: your kubeconfig and its context"
  context=$(caller_context)
  if [ "$context" != "$CONTEXT_BEFORE" ] && [ -n "$CONTEXT_BEFORE" ]; then
    echo "    your current context changed to '${context:-none}': restoring '$CONTEXT_BEFORE'"
    if [ -n "$CALLER_KUBECONFIG" ]; then
      KUBECONFIG=$CALLER_KUBECONFIG kubectl config use-context "$CONTEXT_BEFORE" >/dev/null 2>&1 || true
    else
      env -u KUBECONFIG kubectl config use-context "$CONTEXT_BEFORE" >/dev/null 2>&1 || true
    fi
    cleanup_failed "your kubectl context had changed (restored to '$CONTEXT_BEFORE')"
    context=$(caller_context)
  fi
  [ "$context" = "$CONTEXT_BEFORE" ] || cleanup_failed "your current context is '${context:-none}', not '${CONTEXT_BEFORE:-none}'"
  now=$(guarded_state)
  if [ "$now" = "$GUARDED_BEFORE" ]; then
    echo "    unchanged: the context ${CONTEXT_BEFORE:-(none)}, and the sha256 of$GUARDED_FILES"
  else
    cleanup_failed "your kubeconfig changed during the run (its sha256 before and after differ:$GUARDED_FILES)"
  fi
  check_kube_cache "the run"
}
# check_kube_cache <when>: no new entry in your kubectl cache folder (only
# listed). Another kubectl of yours running meanwhile could add one too:
# the entries are named so you can tell.
kube_cache_added() {
  kube_cache_entries | LC_ALL=C comm -13 <(printf '%s\n' "$KUBE_CACHE_BEFORE" | LC_ALL=C sort) - | grep . || true
}
check_kube_cache() {
  local added
  added=$(kube_cache_added)
  if [ -z "$added" ]; then
    echo "    unchanged: the entries of $CALLER_KUBE_CACHE ($(printf '%s\n' "$KUBE_CACHE_BEFORE" | grep -c .) before and after; this run's caches are in $KUBECACHEDIR)"
  else
    printf '%s\n' "$added" | sed 's/^/      new: /' >&2
    cleanup_failed "$CALLER_KUBE_CACHE gained $(printf '%s\n' "$added" | grep -c .) entries during $1 (above; left for you to judge)"
  fi
}
# The search: every file left in the work folder, outside the secrets
# folder, for each generated value and bearer (boolean; a file holding one
# is named, never shown). Then the secrets folder is deleted.
cleanup_search() {
  local file searched=0 named=0 count=$VALUE_COUNT
  echo "--- cleanup: the work folder searched for the generated values (at $(elapsed))"
  if [ "$count" -lt 1 ] || [ ! -s "$VALUES" ]; then
    echo "    no value was generated yet: nothing to search for"
  else
    find "$WORK" -path "$SECRETS_DIR" -prune -o -type f -print >"$STATE/searched-files.txt"
    while IFS= read -r file; do
      [ -n "$file" ] || continue
      searched=$((searched + 1))
      if LC_ALL=C grep -qaFf "$VALUES" "$file"; then
        echo "    $file holds a generated value or a bearer (not shown)" >&2
        named=$((named + 1))
      fi
    done <"$STATE/searched-files.txt"
    if [ "$named" = 0 ]; then
      echo "    none of the $count values, nor their base64 forms, in any of the $searched files searched"
    else
      cleanup_failed "$named of $searched files hold a generated value or a bearer"
    fi
  fi
  rm -f -- "$SECRETS_DIR"/* && rmdir "$SECRETS_DIR" || cleanup_failed "removing $SECRETS_DIR"
  [ ! -e "$SECRETS_DIR" ] && echo "    removed $SECRETS_DIR (the generated values, the header and the replies)"
}
summary() {
  local code=$1 elapsed=$((SECONDS - STARTED_AT))
  echo
  echo "=== $PROGRAM: the steps that passed"
  sed 's/^/  /' "$STEPS"
  if [ "$code" = 0 ]; then
    echo "=== $PROGRAM: PASS ($(grep -c . "$STEPS") checks) in $((elapsed / 60)) min $((elapsed % 60)) s; work folder $WORK"
  elif [ "$code" = 2 ]; then
    echo "=== $PROGRAM: REFUSED (exit 2) before anything was started; work folder $WORK" >&2
  else
    echo "=== $PROGRAM: FAIL (exit $code) at step ${CURRENT_STEP:-0} after $((elapsed / 60)) min $((elapsed % 60)) s; work folder $WORK" >&2
  fi
}
on_exit() {
  local code=$? called
  # The cleanup always runs to its end: a second Ctrl-C or TERM is ignored
  # (and so are they by the commands it runs, which inherit that).
  trap - EXIT
  trap '' INT TERM HUP
  set +e
  [ "$code" = 0 ] || [ "$code" = 2 ] || echo "--- the run failed (exit $code): cleaning up (at $(elapsed))"
  stop_forwards
  if [ "$profile_created" = yes ]; then
    [ -s "$KUBECONFIG" ] && cleanup_cluster
    cleanup_profile
  fi
  cleanup_cache
  if [ "$pulled_image" = yes ]; then
    echo "--- cleanup: the pulled image $PUBLISHED_IMAGE_REPOSITORY:$PUBLISHED (not here before this run)"
    docker image rm "$PUBLISHED_IMAGE_REPOSITORY:$PUBLISHED" >"$LOGS/cleanup-image.log" 2>&1 ||
      cleanup_failed "removing $PUBLISHED_IMAGE_REPOSITORY:$PUBLISHED (log: $LOGS/cleanup-image.log)"
  fi
  cleanup_kubeconfig
  if called=$(helpers_called); then cleanup_failed "a credential helper's stand-in was called: $called"; fi
  cleanup_search
  if [ -n "$CLEANUP_FAILURES" ]; then
    echo "FAIL: the cleanup:${CLEANUP_FAILURES#;}" >&2
    [ "$code" != 0 ] || code=1
  fi
  summary "$code"
  exit "$code"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP

# ---- 1. The checks before anything starts (still exit 2).
step "preflight: the tools, the profile, the images and Docker's disk"
[ -z "$helm_unset" ] || note "unset, so helm reads only this run's kubeconfig and homes:$helm_unset"
note "minikube $(mk version --short 2>/dev/null), $(kubectl version --client 2>/dev/null | head -n 1), helm $(helm version --short 2>/dev/null), Docker $(docker version --format '{{.Server.Version}}' 2>/dev/null || echo '?')"
note "work folder $WORK; your context ${CONTEXT_BEFORE:-(none)}, its file(s) recorded:$GUARDED_FILES"
# This run's Docker config names no context, so docker and minikube use the
# default one: it must be the engine yours reaches, or the image found here
# isn't the one minikube loads.
if [ "$CALLER_DOCKER_CONFIG_SET" = set ]; then
  engine=$(DOCKER_CONFIG=$CALLER_DOCKER_CONFIG docker info --format '{{.ID}}' 2>/dev/null || true)
else
  engine=$(env -u DOCKER_CONFIG docker info --format '{{.ID}}' 2>/dev/null || true)
fi
own_engine=$(docker info --format '{{.ID}}' 2>/dev/null || true)
[ -n "$engine" ] || refuse "Docker doesn't answer (docker info)"
[ "$own_engine" = "$engine" ] ||
  refuse "this run's Docker config (no context) reaches another Docker engine than yours: set DOCKER_HOST to your engine's socket"
profiles=$(mk profile list --light -o json 2>/dev/null || true)
if printf '%s' "$profiles" | jq -e --arg p "$PROFILE" '[(.valid // [])[], (.invalid // [])[]] | map(.Name) | index($p) != null' >/dev/null 2>&1; then
  refuse "the minikube profile $PROFILE exists already: this script only uses a profile it creates (name another with --profile, or delete it yourself)"
fi
[ ! -e "$MINIKUBE_DIR/profiles/$PROFILE" ] && [ ! -e "$MINIKUBE_DIR/machines/$PROFILE" ] ||
  refuse "minikube holds files for a profile $PROFILE ($MINIKUBE_DIR/profiles or machines): name another with --profile"
for kind in container volume network; do
  if docker "$kind" inspect "$PROFILE" >/dev/null 2>&1; then
    refuse "a Docker $kind named $PROFILE exists (a profile's?): name another profile with --profile"
  fi
done
docker image inspect postgres:18 >/dev/null 2>&1 || refuse "postgres:18 isn't in the local Docker engine (this script never pulls it)"
if [ -z "$PUBLISHED" ]; then
  docker image inspect "$IMAGE" >/dev/null 2>&1 || refuse "$IMAGE isn't in the local Docker engine: build it with make image"
fi
free_kb=$(docker run --rm --pull never --network none --entrypoint df postgres:18 -Pk / 2>/dev/null | awk 'NR == 2 { print $4 }' || true)
[[ "$free_kb" =~ ^[0-9]+$ ]] || refuse "couldn't read Docker's free disk space"
[ "$free_kb" -ge "$MIN_DOCKER_FREE_KB" ] ||
  refuse "Docker's disk has $((free_kb / 1024 / 1024)) GB free, under 6 GB: free some first"
if [ -z "$PUBLISHED" ]; then
  grep -Fxq -- "$RERUN_COMMAND" "$CHART_DIR/README.md" ||
    refuse "the chart's README no longer holds the bootstrap re-run command this smoke runs: $RERUN_COMMAND"
fi
if [ -z "$PUBLISHED" ]; then local_images="$IMAGE and postgres:18 are local"; else local_images="postgres:18 is local"; fi
pass "preflight: the profile $PROFILE is new, the engine is yours, $((free_kb / 1024 / 1024)) GB free in Docker's disk, $local_images"

# ---- --published: the pulls, on the host.
# image_was_here_before <reference>: whether the image the reference names
# after the pull was in the local engine before it, judged by its image id
# and its RepoDigests against the lists recorded before the pull
# ($STATE/images-before-ids.txt, $STATE/images-before-digests.txt), never
# by its tag: an image pulled earlier by digest has no tag, yet is the same
# image. Exit 0 (printing how it was found), 1 it wasn't there, 2 the
# image's id can't be read.
image_was_here_before() {
  local id digest
  id=$(docker image inspect "$1" --format '{{.Id}}' 2>/dev/null) || return 2
  [ -n "$id" ] || return 2
  if LC_ALL=C grep -Fxq -- "$id" "$STATE/images-before-ids.txt"; then
    echo "its id $id"
    return 0
  fi
  for digest in $(docker image inspect "$1" --format '{{range .RepoDigests}}{{.}} {{end}}' 2>/dev/null); do
    if LC_ALL=C grep -Fxq -- "$digest" "$STATE/images-before-digests.txt"; then
      echo "its digest $digest"
      return 0
    fi
  done
  return 1
}
if [ -n "$PUBLISHED" ]; then
  step "the published image and chart, pulled on the host with your temporary login"
  # The engine's images before the pull: every image id, and the
  # repository's digests (an image pulled by digest is listed with no tag).
  docker image ls --all --no-trunc --quiet >"$STATE/images-before-ids.txt" 2>"$LOGS/images-before.log" ||
    fail "couldn't list the local images before the pull (log: $LOGS/images-before.log)"
  docker image ls --digests --no-trunc --format '{{.Repository}}@{{.Digest}}' "$PUBLISHED_IMAGE_REPOSITORY" \
    >"$STATE/images-before-digests.txt" 2>>"$LOGS/images-before.log" ||
    fail "couldn't list the local images' digests before the pull (log: $LOGS/images-before.log)"
  DOCKER_CONFIG=$CALLER_DOCKER_CONFIG docker pull "$PUBLISHED_IMAGE_REPOSITORY:$PUBLISHED" >"$LOGS/pull-image.log" 2>&1 ||
    fail "docker pull $PUBLISHED_IMAGE_REPOSITORY:$PUBLISHED failed:
$(shown "$LOGS/pull-image.log" 10)"
  here_status=0
  here_how=$(image_was_here_before "$PUBLISHED_IMAGE_REPOSITORY:$PUBLISHED") || here_status=$?
  case "$here_status" in
    0) note "$PUBLISHED_IMAGE_REPOSITORY:$PUBLISHED is an image that was here before this run ($here_how): kept afterwards" ;;
    1)
      pulled_image=yes
      note "$PUBLISHED_IMAGE_REPOSITORY:$PUBLISHED wasn't here before this run (neither its id nor its digest): removed afterwards"
      ;;
    *) fail "couldn't read the pulled image's id (docker image inspect $PUBLISHED_IMAGE_REPOSITORY:$PUBLISHED): it is kept; remove it yourself if it wasn't here before this run" ;;
  esac
  HELM_REGISTRY_CONFIG=$CALLER_HELM_REGISTRY_CONFIG helm pull "$PUBLISHED_CHART" --version "$PUBLISHED" -d "$WORK/published" >"$LOGS/pull-chart.log" 2>&1 ||
    fail "helm pull $PUBLISHED_CHART --version $PUBLISHED failed:
$(shown "$LOGS/pull-chart.log" 10)"
  if called=$(helpers_called); then fail "a credential helper was called during the pulls ($called): the login must keep its credentials inline"; fi
  CHART_REF=$WORK/published/opamp-server-$PUBLISHED.tgz
  [ -f "$CHART_REF" ] || fail "helm pull wrote no $CHART_REF"
  helm show chart "$CHART_REF" >"$STATE/published-chart.yaml" 2>"$LOGS/pull-chart.log" || fail "helm show chart $CHART_REF failed"
  chart_version=$(sed -n 's/^version: *"\{0,1\}\([^"]*\)"\{0,1\} *$/\1/p' "$STATE/published-chart.yaml")
  chart_app_version=$(sed -n 's/^appVersion: *"\{0,1\}\([^"]*\)"\{0,1\} *$/\1/p' "$STATE/published-chart.yaml")
  [ "$chart_version" = "$PUBLISHED" ] && [ "$chart_app_version" = "$PUBLISHED" ] ||
    fail "the pulled chart's version is '$chart_version' and its appVersion '$chart_app_version', not $PUBLISHED"
  helm show readme "$CHART_REF" 2>/dev/null | grep -Fxq -- "$RERUN_COMMAND" ||
    fail "the pulled chart's README doesn't hold the bootstrap re-run command this smoke runs: $RERUN_COMMAND"
  note "pulled $(docker image inspect "$PUBLISHED_IMAGE_REPOSITORY:$PUBLISHED" --format '{{join .RepoDigests " "}}' 2>/dev/null)"
  SERVER_IMAGE=$PUBLISHED_IMAGE_REPOSITORY:$PUBLISHED
  pass "pulled $SERVER_IMAGE and the chart $PUBLISHED (version and appVersion $PUBLISHED), no credential helper called"
else
  CHART_REF=$CHART_DIR
  SERVER_IMAGE=$IMAGE
  revision=$(docker image inspect "$IMAGE" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' 2>/dev/null || true)
  note "the image $IMAGE (revision label ${revision:-none}) and the chart $CHART_DIR"
fi

# ---- 2. The profile.
step "minikube start -p $PROFILE --driver=docker, with this run's KUBECONFIG"
if [ -d "$MINIKUBE_CACHE" ]; then find "$MINIKUBE_CACHE" -type f >"$STATE/cache-before.txt" 2>/dev/null || true; else : >"$STATE/cache-before.txt"; fi
profile_created=yes
mk start -p "$PROFILE" --driver=docker --interactive=false >"$LOGS/minikube-start.log" 2>&1 ||
  fail "minikube start failed:
$(shown "$LOGS/minikube-start.log" 15)"
[ -s "$KUBECONFIG" ] || fail "minikube start wrote no kubeconfig at $KUBECONFIG"
[ "$(kubectl --kubeconfig "$KUBECONFIG" config current-context 2>/dev/null)" = "$PROFILE" ] ||
  fail "this run's kubeconfig's current context isn't $PROFILE"
server=$(kubectl --kubeconfig "$KUBECONFIG" config view --minify --context "$PROFILE" -o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null || true)
[[ "$server" =~ ^https://127\.0\.0\.1:[0-9]+$ ]] && [ "${server##*:}" != 8080 ] ||
  fail "the profile's API server in this run's kubeconfig isn't https://127.0.0.1:<port>"
[ "$(guarded_state)" = "$GUARDED_BEFORE" ] || fail "your kubeconfig changed when minikube started (its sha256 differs)"
[ "$(caller_context)" = "$CONTEXT_BEFORE" ] || fail "your current context changed when minikube started"
nodes=$(kube get nodes -o name 2>"$LOGS/kubectl.err" | wc -l | tr -d ' ')
[ "$nodes" -ge 1 ] || fail "the profile answers no node"
[ -d "$KUBECACHEDIR" ] || fail "kubectl wrote no cache in this run's KUBECACHEDIR ($KUBECACHEDIR): it may be writing yours"
[ -z "$(kube_cache_added)" ] || fail "$CALLER_KUBE_CACHE gained an entry once kubectl reached the profile: $(kube_cache_added | tr '\n' ' ')"
pass "the profile $PROFILE started ($server, $nodes node); your kubeconfig, context and $CALLER_KUBE_CACHE untouched, kubectl's cache in $KUBECACHEDIR"

# ---- 3. The images.
step "minikube image load: $SERVER_IMAGE and postgres:18"
in_cluster_name() {
  local first=${1%%/*}
  if [ "$first" = "$1" ]; then
    echo "docker.io/library/$1"
  elif [[ "$first" == *.* || "$first" == *:* || "$first" == localhost ]]; then
    echo "$1"
  else
    echo "docker.io/$1"
  fi
}
for image in "$SERVER_IMAGE" postgres:18; do
  LOADED_CACHE_NAMES="$LOADED_CACHE_NAMES ${image//:/_}"
  mk -p "$PROFILE" image load "$image" >>"$LOGS/image-load.log" 2>&1 ||
    fail "minikube image load $image failed:
$(shown "$LOGS/image-load.log" 10)"
done
mk -p "$PROFILE" image ls >"$STATE/images.txt" 2>>"$LOGS/image-load.log" || fail "minikube image ls failed"
for image in "$SERVER_IMAGE" postgres:18; do
  grep -Fxq "$(in_cluster_name "$image")" "$STATE/images.txt" || fail "$image isn't in the profile after its load"
done
if called=$(helpers_called); then fail "a credential helper was called while the images loaded ($called)"; fi
pass "both images loaded into the profile, no credential helper called"

# ---- 4. The Secrets.
step "the two Secrets, from values generated now (files of mode 0600, --from-file)"
for name in management-secret encryption-key owner-password runtime-password postgres-password; do
  openssl rand -hex 32 | tr -d '\n' >"$SECRETS_DIR/$name"
  [ "$(wc -c <"$SECRETS_DIR/$name" | tr -d ' ')" = 64 ] || fail "openssl rand made no 64-character value for $name"
  add_values "$SECRETS_DIR/$name"
done
# The management API's header, from the secret's file: the value is never in
# a variable or a command line.
{ printf 'Authorization: Bearer '; cat "$SECRETS_DIR/management-secret"; printf '\n'; } >"$HEADER"
kube create namespace "$NS" >"$LOGS/secrets.log" 2>&1 || fail "kubectl create namespace $NS failed"
kube create secret generic "$SERVER_SECRET" \
  --from-file=management-secret="$SECRETS_DIR/management-secret" \
  --from-file=encryption-key="$SECRETS_DIR/encryption-key" \
  --from-file=owner-password="$SECRETS_DIR/owner-password" \
  --from-file=runtime-password="$SECRETS_DIR/runtime-password" >>"$LOGS/secrets.log" 2>&1 ||
  fail "kubectl create secret $SERVER_SECRET failed"
kube create secret generic "$POSTGRES_SECRET" \
  --from-file=postgres-password="$SECRETS_DIR/postgres-password" >>"$LOGS/secrets.log" 2>&1 ||
  fail "kubectl create secret $POSTGRES_SECRET failed"
# The Secrets' keys, never their values (the template prints the keys only).
secret_keys() { kube get secret "$1" -o go-template='{{range $key, $value := .data}}{{$key}} {{end}}' 2>>"$LOGS/kubectl.err" | sed 's/ $//'; }
pass "Secrets $SERVER_SECRET (keys: $(secret_keys "$SERVER_SECRET")) and $POSTGRES_SECRET (keys: $(secret_keys "$POSTGRES_SECRET"))"

# ---- 5. The install.
step "helm install $RELEASE (pullPolicy Never, both digests empty)"
VALUES_FILE=$STATE/install-values.yaml
{
  echo "# The smoke's values: names and plain settings only."
  echo "image:"
  if [ -z "$PUBLISHED" ]; then
    echo "  repository: \"${IMAGE%:*}\""
    echo "  tag: \"${IMAGE##*:}\""
  fi
  echo "  digest: \"\""
  echo "  pullPolicy: Never"
  echo "auth:"
  echo "  existingSecret: $SERVER_SECRET"
  echo "server:"
  echo "  publicUrl: http://$FULLNAME-agent.$NS.svc:51234/opamp/api/v1"
  echo "postgres:"
  echo "  image:"
  echo "    digest: \"\""
  echo "    pullPolicy: Never"
  echo "  auth:"
  echo "    existingSecret: $POSTGRES_SECRET"
  echo "  persistence:"
  echo "    size: 1Gi"
} >"$VALUES_FILE"
release_installed=yes
helmk install "$RELEASE" "$CHART_REF" -f "$VALUES_FILE" >"$LOGS/helm-install.log" 2>&1 ||
  fail "helm install failed:
$(shown "$LOGS/helm-install.log" 15)"
pass "helm install $RELEASE from $CHART_REF"

# ---- Waiting for the pods, failing fast.
# The pods' problems: a container waiting for a reason that never clears by
# itself, a failed pod, or a volume that can't be mounted (an event). Each
# line: the pod, its phase, the container and the reason; never a message
# or anything from the environment. A failed mount counts once repeated
# (three times): a pod's first mount may fail once while the kubelet hasn't
# seen a new ConfigMap or Secret yet.
PROBLEMS_JQ='
($events[0].items // []) as $events
| [.items[] | select(.metadata.deletionTimestamp == null)] as $pods
| ($pods | map(.metadata.name)) as $names
| ( $pods[] | .metadata.name as $pod | .status.phase as $phase
    | ((.status.initContainerStatuses // []) + (.status.containerStatuses // []))[]
    | (.state.waiting.reason // "") as $reason
    | select($reason | test("^(CreateContainerConfigError|CreateContainerError|ErrImage.*|ImagePullBackOff|InvalidImageName|CrashLoopBackOff|RunContainerError)$"))
    | "\($pod) \($phase) \(.name) \($reason)" ),
  ( $pods[] | select(.status.phase == "Failed") | "\(.metadata.name) Failed - \(.status.reason // "Failed")" ),
  ( $events[] | select(.reason == "FailedMount" and (.count // 1) >= 3 and .involvedObject.kind == "Pod" and (.involvedObject.name | IN($names[])))
    | "\(.involvedObject.name) \(.involvedObject.name as $n | ($pods[] | select(.metadata.name == $n) | .status.phase)) - FailedMount" )
'
# Settled: every pod there is live (none terminating) and ready, the two
# Deployments and the StatefulSet rolled out, with REPLICAS pods of each
# component and one Postgres pod.
SETTLED_JQ='
def ready: any(.status.conditions[]?; .type == "Ready" and .status == "True");
def rolled: (.status.observedGeneration // 0) >= .metadata.generation
  and (.status.replicas // 0) == .spec.replicas
  and (.status.updatedReplicas // 0) == .spec.replicas
  and (.status.readyReplicas // 0) == .spec.replicas;
($workloads[0].items) as $workloads
| [.items[] | select(.metadata.deletionTimestamp == null)] as $live
| ([.items[] | select(.metadata.deletionTimestamp != null)] | length) == 0
  and ($workloads | length) == 3
  and all($workloads[]; rolled)
  and all($workloads[] | select(.kind == "StatefulSet"); (.status.currentRevision // "") == (.status.updateRevision // "-"))
  and ([$live[] | select(.metadata.labels["app.kubernetes.io/component"] == "agent")] | length) == $replicas
  and ([$live[] | select(.metadata.labels["app.kubernetes.io/component"] == "management")] | length) == $replicas
  and ([$live[] | select(.metadata.labels["app.kubernetes.io/component"] == "postgres")] | length) == 1
  and all($live[]; ready)
'
SUMMARY_JQ='
.items[] | "\(.metadata.name): phase \(.status.phase)\(if .metadata.deletionTimestamp then " (terminating)" else "" end); "
  + ([((.status.initContainerStatuses // []) + (.status.containerStatuses // []))[]
      | "\(.name) " + (if .state.waiting then "waiting (\(.state.waiting.reason // "-"))"
          elif .state.running then (if .ready then "ready" else "running, not ready" end)
          elif .state.terminated then "terminated (\(.state.terminated.reason // "-"))"
          else "-" end)] | join(", "))
'
reason_hint() {
  case "$1" in
    CreateContainerConfigError) echo "a Secret, or a key of one, that the container reads is missing (the chart README's \"The Secrets it reads\")" ;;
    CreateContainerError | RunContainerError) echo "the container runtime couldn't create or start the container" ;;
    ErrImage* | ImagePullBackOff | InvalidImageName) echo "the image isn't in the profile under the name the pod uses (pullPolicy Never: nothing is pulled)" ;;
    CrashLoopBackOff) echo "the container keeps exiting: its log is saved in $LOGS (not shown here)" ;;
    FailedMount) echo "a volume the pod mounts can't be mounted (a Secret's key named in a volume's items, or a Secret or ConfigMap, is missing)" ;;
    *) echo "the pod failed" ;;
  esac
}
pods_summary() {
  kube get pods -l "app.kubernetes.io/instance=$RELEASE" -o json 2>/dev/null | jq -r "$SUMMARY_JQ" 2>/dev/null | sed 's/^/      /'
}
save_container_log() { # <pod> <container> <label>
  kube logs "$1" -c "$2" >"$LOGS/$3-$1-$2.log" 2>&1 || true
  kube logs "$1" -c "$2" --previous >"$LOGS/$3-$1-$2.previous.log" 2>&1 || true
}
wait_ready() { # <what>
  local what=$1 deadline=$((SECONDS + WAIT_TIMEOUT)) problems line pod phase container reason
  while :; do
    if kube get pods -l "app.kubernetes.io/instance=$RELEASE" -o json >"$STATE/pods.json" 2>"$LOGS/kubectl.err" &&
      kube get deployment,statefulset -l "app.kubernetes.io/instance=$RELEASE" -o json >"$STATE/workloads.json" 2>>"$LOGS/kubectl.err"; then
      kube get events -o json >"$STATE/events.json" 2>>"$LOGS/kubectl.err" || echo '{"items":[]}' >"$STATE/events.json"
      problems=$(jq -r --slurpfile events "$STATE/events.json" "$PROBLEMS_JQ" "$STATE/pods.json" | sort -u)
      if [ -n "$problems" ]; then
        echo "    $what: a pod can't start:" >&2
        while read -r pod phase container reason; do
          [ -n "$pod" ] || continue
          if [ "$container" = - ]; then
            echo "      pod $pod (phase $phase): $reason: $(reason_hint "$reason")" >&2
          else
            echo "      pod $pod (phase $phase): container $container is waiting: $reason: $(reason_hint "$reason")" >&2
            [ "$reason" != CrashLoopBackOff ] || save_container_log "$pod" "$container" failed
          fi
        done <<<"$problems"
        line=$(printf '%s\n' "$problems" | head -n 1)
        fail "$what: pod ${line%% *} can't start ($(echo "$line" | awk '{ print $4 }')), so the run stops here rather than wait for a timeout"
      fi
      if jq -e --slurpfile workloads "$STATE/workloads.json" --argjson replicas "$REPLICAS" "$SETTLED_JQ" "$STATE/pods.json" >/dev/null; then
        return 0
      fi
    fi
    if [ "$SECONDS" -ge "$deadline" ]; then
      echo "    the pods after $WAIT_TIMEOUT s:" >&2
      pods_summary >&2
      fail "$what: the pods weren't all ready and rolled out after $WAIT_TIMEOUT s (--timeout)"
    fi
    sleep 3
  done
}
pod_of() { # <component>: the live (not terminating) pod's name
  kube get pods -l "app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/component=$1" -o json 2>>"$LOGS/kubectl.err" |
    jq -r '[.items[] | select(.metadata.deletionTimestamp == null)][0].metadata.name // empty'
}

step "every pod ready (failing fast on a pod that can't start)"
wait_ready "the install"
pods_summary
pass "the agent endpoint's pod, the management API's pod and the Postgres pod are ready"

# ---- What the run keeps for the searches: the containers' logs, the stored
# objects and Helm's release record. Each must hold no generated value.
collect_logs() { # <label>
  local pod container
  for pod in $(kube get pods -l "app.kubernetes.io/instance=$RELEASE" -o jsonpath='{.items[*].metadata.name}' 2>>"$LOGS/kubectl.err"); do
    for container in $(kube get pod "$pod" -o jsonpath='{.spec.initContainers[*].name} {.spec.containers[*].name}' 2>>"$LOGS/kubectl.err"); do
      kube logs "$pod" -c "$container" >"$LOGS/$1-$pod-$container.log" 2>&1 || true
    done
  done
}
check_kept() { # <label> <file>...: true when each exists and none holds a value
  local label=$1 file
  shift
  for file in "$@"; do
    # An unmatched glob arrives as itself: nothing was searched, which fails.
    [ -f "$file" ] || fail "$label: no such file to search ($file)"
    if holds_value "$file"; then fail "$label: $file holds a generated value or a bearer (not shown)"; fi
  done
}
collect_logs installed
check_kept "the containers' logs at the first start (the bootstrap's among them)" "$LOGS"/installed-*.log
pass "no generated value in any container's log at the first start (Postgres's bootstrap, migrate, serve)"

# ---- 5b. The open points on the cluster: the claim, the modes, local trust.
step "on the cluster: the Postgres pod's claim and fsGroup, the bootstrap files' modes, local-socket trust"
fs_group=$(kube get pod "$POSTGRES_POD" -o jsonpath='{.spec.securityContext.fsGroup}' 2>>"$LOGS/kubectl.err")
[ "$fs_group" = 999 ] || fail "the Postgres pod's fsGroup is '${fs_group:-none}', not 999"
kube exec "$POSTGRES_POD" -c postgres -- sh -c '
  echo "uid $(id -u) groups $(id -G)"
  echo "claim $(stat -c "%u:%g %a" /var/lib/postgresql)"
  echo "data $(stat -c "%u:%g %a" "$PGDATA")"
  echo "initdb.sh $(stat -L -c "%a" /docker-entrypoint-initdb.d/initdb.sh)"
  echo "bootstrap.sql $(stat -L -c "%a" /opamp-bootstrap/bootstrap.sql)"
  echo "init-folder $(ls /docker-entrypoint-initdb.d | tr "\n" " ")"
  echo "sql-folder $(ls /opamp-bootstrap | tr "\n" " ")"
' >"$STATE/postgres-files.txt" 2>"$LOGS/postgres-files.err" || fail "reading the Postgres pod's files failed"
sed 's/^/      /' "$STATE/postgres-files.txt"
field() { sed -n "s/^$1 //p" "$STATE/postgres-files.txt"; }
case " $(field uid | sed 's/^[0-9]* groups //') " in *" 999 "*) ;; *) fail "the postgres container's groups don't include 999 (fsGroup)" ;; esac
[ "$(field uid | cut -d' ' -f1)" = 999 ] || fail "the postgres container doesn't run as uid 999"
claim=$(field claim)
data=$(field data)
[ "${data% *}" = 999:999 ] && [ "${data#* }" = 700 ] || fail "Postgres's data folder is $data, not 999:999 700"
[ "$(field initdb.sh)" = 555 ] || fail "initdb.sh's mode is $(field initdb.sh), not 555"
[ "$(field bootstrap.sql)" = 444 ] || fail "bootstrap.sql's mode is $(field bootstrap.sql), not 444"
[ "$(field init-folder)" = "initdb.sh " ] || fail "the entrypoint's folder holds more than initdb.sh: $(field init-folder)"
[ "$(field sql-folder)" = "bootstrap.sql " ] || fail "/opamp-bootstrap holds more than bootstrap.sql: $(field sql-folder)"
# fsGroup reaches a claim only where its volume type supports ownership
# management (most block and CSI volumes do; minikube's hostPath
# provisioner doesn't, and makes the folder 0777 instead).
case "$claim" in
  *:999\ *) claim_note="fsGroup applied to it" ;;
  *\ 777) claim_note="fsGroup not applied: this provisioner's volume type doesn't support it, and the folder is 0777" ;;
  *) fail "the claim's root is $claim: neither group 999 (fsGroup) nor 0777, so Postgres may not write it" ;;
esac
pass "fsGroup 999 in the pod and in the container's groups; the claim's root is $claim ($claim_note); Postgres's data folder $data"
pass "the ConfigMap's items: initdb.sh 0555, alone in /docker-entrypoint-initdb.d; bootstrap.sql 0444, alone in /opamp-bootstrap"
psql_postgres() { # <database>: the SQL on stdin, run by the superuser over the local socket
  kube exec -i "$POSTGRES_POD" -c postgres -- psql -X -At -v ON_ERROR_STOP=1 --no-password -U postgres -d "$1" -f -
}
echo "select type || ' ' || auth_method from pg_hba_file_rules where type = 'local' order by line_number;" |
  psql_postgres postgres >"$STATE/hba-local.txt" 2>"$LOGS/hba.err" || fail "reading pg_hba_file_rules over the local socket failed"
[ -s "$STATE/hba-local.txt" ] && ! grep -vqx 'local trust' "$STATE/hba-local.txt" ||
  fail "the local socket's pg_hba rules aren't all trust: $(tr '\n' ';' <"$STATE/hba-local.txt")"
pass "the local socket is trusted (pg_hba: $(sort -u "$STATE/hba-local.txt" | tr '\n' ' ' | sed 's/ $//')): the superuser connects with no password, as initdb.sh does"

# ---- 6. The endpoints, through port-forwards.
start_forward() { # <service> <port>: sets FORWARD_PORT
  local service=$1 remote=$2 log="$LOGS/port-forward-$1.log" try i port pid
  for try in 1 2 3; do
    : >"$log"
    kubectl --kubeconfig "$KUBECONFIG" --context "$PROFILE" -n "$NS" port-forward --address 127.0.0.1 "svc/$service" ":$remote" >"$log" 2>&1 &
    pid=$!
    FORWARD_PIDS="$FORWARD_PIDS $pid"
    port=""
    for i in $(seq 1 60); do
      port=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9][0-9]*\) -> .*/\1/p' "$log" | head -n 1)
      [ -z "$port" ] || break
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.5
    done
    [ -n "$port" ] || fail "kubectl port-forward to svc/$service didn't start:
$(shown "$log" 5)"
    case "$port" in
      51234 | 51235) kill "$pid" 2>/dev/null || true ;;
      *) FORWARD_PORT=$port; return 0 ;;
    esac
  done
  fail "kubectl port-forward to svc/$service kept picking 51234 or 51235"
}
restart_forwards() {
  stop_forwards
  start_forward "$FULLNAME-agent" 51234
  AGENT=http://127.0.0.1:$FORWARD_PORT
  start_forward "$FULLNAME-management" 51235
  MANAGEMENT=http://127.0.0.1:$FORWARD_PORT
}
# http <method> <url> [curl arguments...]: STATUS, and the reply in
# REPLY_BODY and REPLY_HEADERS (the secrets folder: a reply may hold a
# bearer). curl reads no .curlrc (-q).
http() {
  local method=$1 url=$2
  shift 2
  STATUS=$(curl -q -sS --max-time 15 -X "$method" -o "$REPLY_BODY" -D "$REPLY_HEADERS" -w '%{http_code}' "$@" "$url" 2>"$LOGS/curl.err") || STATUS=000
}
reply_code() { local code; code=$(jq -r '.error.code // empty' "$REPLY_BODY" 2>/dev/null || true); echo "${code:-none}"; }
expect() { # <status> <what>
  [ "$STATUS" = "$1" ] || fail "$2 answered HTTP $STATUS (error code: $(reply_code)), not $1"
}
# authed: the management API's secret, as a header read from file
# descriptor 3 (`curl -H @/dev/fd/3 3<header`), never on a command line.
authed() { # <method> <url> [curl arguments...]
  local method=$1 url=$2
  shift 2
  http "$method" "$url" -H @/dev/fd/3 "$@" 3<"$HEADER"
}
wait_health() { # the forwards answer (a new forward may take a moment)
  local i
  for i in $(seq 1 20); do
    http GET "$MANAGEMENT/health"
    [ "$STATUS" != 200 ] || return 0
    sleep 1
  done
  expect 200 "GET /health on the management port"
}
read_back() { # <when>
  authed GET "$MANAGEMENT/api/v1/organizations/$ORGANIZATION_ID"
  expect 200 "reading the organization back ($1)"
  [ "$(jq -r '.organizationId' "$REPLY_BODY")" = "$ORGANIZATION_ID" ] && [ "$(jq -r '.name' "$REPLY_BODY")" = "$ORGANIZATION_NAME" ] ||
    fail "the organization read back ($1) isn't the one created"
  authed GET "$MANAGEMENT/api/v1/organizations/$ORGANIZATION_ID/fleets/$FLEET_ID"
  expect 200 "reading the fleet back ($1)"
  [ "$(jq -r '.id' "$REPLY_BODY")" = "$FLEET_ID" ] && [ "$(jq -r '.label' "$REPLY_BODY")" = "$FLEET_LABEL" ] ||
    fail "the fleet read back ($1) isn't the one created"
}

step "the endpoints, through kubectl port-forward on free local ports"
restart_forwards
note "agent ${AGENT#http://127.0.0.1:} -> 51234, management ${MANAGEMENT#http://127.0.0.1:} -> 51235"
wait_health
http GET "$AGENT/health"
expect 200 "GET /health on the agent port"
http GET "$AGENT/ready"
expect 200 "GET /ready on the agent port"
http GET "$MANAGEMENT/ready"
expect 200 "GET /ready on the management port"
pass "/health and /ready 200 on both ports"
http GET "$MANAGEMENT/api/v1/organizations/00000000-0000-4000-8000-000000000000"
expect 401 "the management API without the secret"
pass "the management API answers 401 without the secret ($(reply_code))"
ORGANIZATION_NAME="Smoke test"
FLEET_LABEL="smoke-fleet"
authed POST "$MANAGEMENT/api/v1/organizations" -H 'Content-Type: application/json' --data "{\"name\": \"$ORGANIZATION_NAME\"}"
expect 201 "creating an organization with the management secret"
ORGANIZATION_ID=$(jq -r '.organizationId // empty' "$REPLY_BODY")
[[ "$ORGANIZATION_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "the organization's create answered no organizationId"
authed POST "$MANAGEMENT/api/v1/organizations/$ORGANIZATION_ID/fleets" -H 'Content-Type: application/json' \
  --data "{\"label\": \"$FLEET_LABEL\", \"type\": \"server\", \"rotateExpiresAt\": null, \"actor\": null}"
expect 201 "creating a fleet with the management secret"
# The reply holds both tokens' bearers: added to the values the run searches
# for, never shown.
jq -r '.tokens.default.bearer // empty, .tokens.rotate.bearer // empty' "$REPLY_BODY" >"$SECRETS_DIR/bearers"
[ "$(grep -c . "$SECRETS_DIR/bearers")" = 2 ] || fail "the fleet's create answered without both tokens' bearers"
add_values "$SECRETS_DIR/bearers"
grep -qi '^cache-control:[[:space:]]*no-store' "$REPLY_HEADERS" || fail "the fleet's create (it holds bearers) answered without Cache-Control: no-store"
FLEET_ID=$(jq -r '.fleet.id // empty' "$REPLY_BODY")
[[ "$FLEET_ID" =~ ^[0-9a-f-]{36}$ ]] || fail "the fleet's create answered no fleet id"
read_back "after creating them"
pass "an organization ($ORGANIZATION_ID) and a fleet ($FLEET_ID) created (201, the fleet's reply no-store, with both bearers) and read back (200), the secret from a file descriptor"
http POST "$AGENT/opamp/api/v1" -H 'Content-Type: application/x-protobuf' --data-binary ''
expect 401 "the agent endpoint without a bearer"
pass "the agent endpoint answers 401 without a bearer"

# ---- 7. The pods deleted.
for component in agent management; do
  step "the $component pod deleted: the organization is still there"
  server_pod=$(pod_of "$component")
  [ -n "$server_pod" ] || fail "no $component pod found"
  stop_forwards
  kube delete pod "$server_pod" --wait=true --timeout=120s >"$LOGS/delete-$component-pod.log" 2>&1 || fail "deleting the $component pod failed"
  wait_ready "after deleting the $component pod"
  new_server_pod=$(pod_of "$component")
  [ -n "$new_server_pod" ] && [ "$new_server_pod" != "$server_pod" ] || fail "no new $component pod after deleting $server_pod"
  restart_forwards
  wait_health
  read_back "after deleting the $component pod"
  pass "the $component pod $server_pod deleted, $new_server_pod ready, the organization and the fleet read back"
done

step "the Postgres pod deleted: the organization is still there"
postgres_uid=$(kube get pod "$POSTGRES_POD" -o jsonpath='{.metadata.uid}' 2>>"$LOGS/kubectl.err")
stop_forwards
kube delete pod "$POSTGRES_POD" --wait=true --timeout=120s >"$LOGS/delete-postgres-pod.log" 2>&1 || fail "deleting the Postgres pod failed"
wait_ready "after deleting the Postgres pod"
[ "$(kube get pod "$POSTGRES_POD" -o jsonpath='{.metadata.uid}' 2>>"$LOGS/kubectl.err")" != "$postgres_uid" ] ||
  fail "the Postgres pod wasn't replaced"
restart_forwards
wait_health
http GET "$AGENT/ready"
expect 200 "GET /ready on the agent port, after the Postgres pod came back"
read_back "after deleting the Postgres pod"
pass "the Postgres pod deleted and back (a new pod on the same claim), /ready 200, the organization and the fleet read back"

# ---- 8. The README's bootstrap re-run.
step "the README's bootstrap re-run applies bootstrap.sql"
# The catalog's shape: the roles (never a password, only whether one is
# set), memberships, the database and its settings, the schemas, the
# objects' owners and grants, default privileges. A password set again to
# the same value changes nothing here (its verifier's salt aside, which this
# never reads).
CATALOG_POSTGRES_SQL="
select 'role', rolname, rolsuper, rolinherit, rolcreaterole, rolcreatedb, rolcanlogin, rolreplication, rolbypassrls, rolconnlimit, rolvaliduntil, rolpassword is not null from pg_authid where rolname like 'telflo_opamp%' order by rolname;
select 'member', roleid::regrole, member::regrole, grantor::regrole, admin_option, inherit_option, set_option from pg_auth_members where roleid::regrole::text like 'telflo_opamp%' or member::regrole::text like 'telflo_opamp%' order by 2, 3;
select 'database', datname, pg_get_userbyid(datdba), datacl, pg_encoding_to_char(encoding), datcollate, datctype, datallowconn, datconnlimit from pg_database order by datname;
select 'setting', coalesce(d.datname, '-'), coalesce(r.rolname, '-'), s.setconfig from pg_db_role_setting s left join pg_database d on d.oid = s.setdatabase left join pg_roles r on r.oid = s.setrole order by 2, 3;
"
CATALOG_DATABASE_SQL="
select 'schema', nspname, pg_get_userbyid(nspowner), nspacl from pg_namespace where nspname not like 'pg\\_%' and nspname <> 'information_schema' order by nspname;
select 'object', n.nspname, c.relname, c.relkind, pg_get_userbyid(c.relowner), c.relacl from pg_class c join pg_namespace n on n.oid = c.relnamespace where n.nspname not like 'pg\\_%' and n.nspname <> 'information_schema' order by 2, 3;
select 'default-acl', pg_get_userbyid(defaclrole), coalesce(defaclnamespace::regnamespace::text, '-'), defaclobjtype, defaclacl from pg_default_acl order by 2, 3, 4;
select 'function', n.nspname, p.proname, pg_get_userbyid(p.proowner), p.proacl from pg_proc p join pg_namespace n on n.oid = p.pronamespace where n.nspname not like 'pg\\_%' and n.nspname <> 'information_schema' order by 2, 3;
"
catalog() { # <file>
  { printf '%s' "$CATALOG_POSTGRES_SQL" | psql_postgres postgres && printf '%s' "$CATALOG_DATABASE_SQL" | psql_postgres "$DATABASE"; } >"$1" 2>"$LOGS/catalog.err" ||
    fail "reading the catalog failed:
$(shown "$LOGS/catalog.err" 5)"
}
# tcp_login <password variable> <role>: true when the role logs in over TCP
# (scram) with the password the pod's own environment holds, read inside
# the pod: never on a command line.
tcp_login() {
  local answer
  answer=$(kube exec "$POSTGRES_POD" -c postgres -- sh -c \
    'PGPASSWORD=$(printenv "$1"); export PGPASSWORD; exec psql -X -At -h 127.0.0.1 -U "$2" -d "$3" -c "select 1"' \
    sh "$1" "$2" "$DATABASE" 2>>"$LOGS/password-check.err" || true)
  [ "$answer" = 1 ]
}
# runtime_connect: the runtime role's CONNECT on the database (t or f).
runtime_connect() {
  echo "select has_database_privilege('telflo_opamp', '$DATABASE', 'CONNECT');" | psql_postgres postgres 2>>"$LOGS/catalog.err"
}
catalog "$STATE/catalog-before.txt"
[ "$(grep -c '^role|' "$STATE/catalog-before.txt")" -ge 2 ] || fail "the catalog shows fewer than the bootstrap's roles"
tcp_login OPAMP_OWNER_PASSWORD telflo_opamp_owner && tcp_login OPAMP_RUNTIME_PASSWORD telflo_opamp ||
  fail "a role doesn't log in over TCP with its password before the re-run"
[ "$(runtime_connect)" = t ] || fail "telflo_opamp has no CONNECT on $DATABASE before the re-run"
# Undo two things bootstrap.sql sets, as the superuser: the runtime login's
# password and its CONNECT. A re-run that didn't apply bootstrap.sql would
# leave both undone.
echo "alter role telflo_opamp password null; revoke connect on database $DATABASE from telflo_opamp;" |
  psql_postgres postgres >"$LOGS/undo.log" 2>&1 || fail "undoing the runtime role's password and CONNECT failed"
catalog "$STATE/catalog-undone.txt"
! cmp -s "$STATE/catalog-before.txt" "$STATE/catalog-undone.txt" || fail "undoing the password and the grant changed nothing in the catalog"
! tcp_login OPAMP_RUNTIME_PASSWORD telflo_opamp || fail "telflo_opamp still logs in over TCP with its password set to null"
[ "$(runtime_connect)" = f ] || fail "telflo_opamp still has CONNECT on $DATABASE after the revoke"
pass "undone first, as the superuser: telflo_opamp's password (its TCP login now fails) and its CONNECT on $DATABASE (now f)"
note "running, as the README says: $RERUN_COMMAND"
rerun_status=0
kube exec "$POSTGRES_POD" -c postgres -- /docker-entrypoint-initdb.d/initdb.sh >"$SECRETS_DIR/bootstrap-rerun.out" 2>&1 || rerun_status=$?
if holds_value "$SECRETS_DIR/bootstrap-rerun.out"; then
  fail "the re-run's output holds a generated value (not shown)"
fi
note "its output ($(grep -c . "$SECRETS_DIR/bootstrap-rerun.out") lines, none holding a generated value):"
redact <"$SECRETS_DIR/bootstrap-rerun.out" | sed 's/^/      /'
[ "$rerun_status" = 0 ] || fail "the README's bootstrap re-run exited $rerun_status"
[ -s "$SECRETS_DIR/bootstrap-rerun.out" ] || fail "the README's bootstrap re-run printed nothing: bootstrap.sql didn't run"
tcp_login OPAMP_RUNTIME_PASSWORD telflo_opamp || fail "after the re-run, telflo_opamp still doesn't log in over TCP: the re-run didn't apply bootstrap.sql"
tcp_login OPAMP_OWNER_PASSWORD telflo_opamp_owner || fail "after the re-run, telflo_opamp_owner doesn't log in over TCP"
[ "$(runtime_connect)" = t ] || fail "after the re-run, telflo_opamp still has no CONNECT on $DATABASE: the re-run didn't apply bootstrap.sql"
catalog "$STATE/catalog-after.txt"
if ! cmp -s "$STATE/catalog-before.txt" "$STATE/catalog-after.txt"; then
  diff "$STATE/catalog-before.txt" "$STATE/catalog-after.txt" | sed 's/^/      /' >&2 || true
  fail "after the re-run the catalog isn't as it was before the undo (above: roles, grants, settings; no password)"
fi
pass "the re-run (exit 0, $(grep -c . "$SECRETS_DIR/bootstrap-rerun.out") lines) restored the password (both roles log in over TCP) and the CONNECT, and the catalog equals the one before the undo ($(grep -c . "$STATE/catalog-after.txt") lines: roles, memberships, databases, settings, schemas, owners, grants)"
read_back "after the bootstrap re-run"
pass "the organization and the fleet read back after the re-run"

# ---- 9. helm upgrade, then helm rollback.
step "helm upgrade with a changed value, then helm rollback"
heartbeat() {
  kube get deployment "$FULLNAME-agent" \
    -o jsonpath='{.spec.template.spec.containers[?(@.name=="serve")].env[?(@.name=="OPAMP_HEARTBEAT_INTERVAL_SECONDS")].value}' 2>>"$LOGS/kubectl.err"
}
helmk get all "$RELEASE" --revision 1 >"$WORK/release/revision-1.txt" 2>>"$LOGS/helm.err" || fail "helm get all --revision 1 failed"
before_pod=$(pod_of agent)
before_management_pod=$(pod_of management)
stop_forwards
helmk upgrade "$RELEASE" "$CHART_REF" -f "$VALUES_FILE" --set server.heartbeatIntervalSeconds=45 >"$LOGS/helm-upgrade.log" 2>&1 ||
  fail "helm upgrade failed:
$(shown "$LOGS/helm-upgrade.log" 15)"
wait_ready "after helm upgrade"
[ "$(heartbeat)" = 45 ] || fail "after the upgrade, serve's OPAMP_HEARTBEAT_INTERVAL_SECONDS is '$(heartbeat)', not 45"
[ "$(pod_of agent)" != "$before_pod" ] || fail "the upgrade didn't roll the agent endpoint's pod"
[ "$(pod_of management)" = "$before_management_pod" ] || fail "the upgrade of an agent-only value rolled the management API's pod too"
restart_forwards
wait_health
read_back "after helm upgrade"
helmk get all "$RELEASE" --revision 2 >"$WORK/release/revision-2.txt" 2>>"$LOGS/helm.err" || fail "helm get all --revision 2 failed"
pass "helm upgrade (revision 2, server.heartbeatIntervalSeconds=45) rolled out the agent endpoint alone: its variable is \"45\", the management API's pod untouched, the organization read back"
stop_forwards
helmk rollback "$RELEASE" 1 >"$LOGS/helm-rollback.log" 2>&1 ||
  fail "helm rollback failed:
$(shown "$LOGS/helm-rollback.log" 15)"
wait_ready "after helm rollback"
[ -z "$(heartbeat)" ] || fail "after the rollback, serve still has OPAMP_HEARTBEAT_INTERVAL_SECONDS"
restart_forwards
wait_health
read_back "after helm rollback"
helmk history "$RELEASE" -o json >"$STATE/history.json" 2>>"$LOGS/helm.err" || fail "helm history failed"
jq -e 'length == 3 and .[2].revision == 3 and .[2].status == "deployed" and (.[2].description | test("Rollback to 1"))' "$STATE/history.json" >/dev/null ||
  fail "helm history isn't three revisions with the last one 'Rollback to 1', deployed"
helmk get all "$RELEASE" --revision 3 >"$WORK/release/revision-3.txt" 2>>"$LOGS/helm.err" || fail "helm get all --revision 3 failed"
pass "helm rollback to 1 (revision 3, deployed) rolled out: the variable gone, the organization read back"

# ---- 10. What the cluster and Helm hold.
step "no generated value or bearer in the logs, the stored objects or Helm's release record; no registry credential in the cluster"
collect_logs final
kube get pods,deployments,statefulsets,replicasets,services,configmaps,serviceaccounts,persistentvolumeclaims -o yaml >"$STATE/objects.yaml" 2>>"$LOGS/kubectl.err" ||
  fail "reading the stored objects failed"
check_kept "the containers' logs" "$LOGS"/final-*.log
check_kept "the stored objects" "$STATE/objects.yaml"
check_kept "Helm's release record" "$WORK"/release/revision-*.txt
grep -q 'OPAMP_RUNTIME_PASSWORD)' "$STATE/objects.yaml" || fail "the stored Deployment doesn't compose the URL from \$(OPAMP_RUNTIME_PASSWORD)"
kinds=$(kube get secrets --all-namespaces -o json 2>>"$LOGS/kubectl.err" |
  jq '[.items[] | select(.type == "kubernetes.io/dockerconfigjson" or .type == "kubernetes.io/dockercfg")] | length')
[ "$kinds" = 0 ] || fail "the cluster holds $kinds registry credential Secrets"
pull_secrets=$(kube get pods -o json 2>>"$LOGS/kubectl.err" | jq '[.items[] | (.spec.imagePullSecrets // []) | length] | add // 0')
[ "$pull_secrets" = 0 ] || fail "a pod names an image pull secret"
pass "none of the generated values or bearers in the containers' logs, the stored objects (the URLs hold \$(VAR) references) or the three revisions' release records"
pass "no registry credential Secret in the cluster, and no pod names a pull secret"
[ -z "$(kube_cache_added)" ] || fail "$CALLER_KUBE_CACHE gained entries during the run: $(kube_cache_added | tr '\n' ' ')"
pass "no new entry in $CALLER_KUBE_CACHE after every kubectl and helm call ($(find "$KUBECACHEDIR" -mindepth 2 -maxdepth 2 | wc -l | tr -d ' ') entries in this run's own cache)"

CURRENT_STEP=""
echo "--- every check passed: cleaning up (at $(elapsed))"
exit 0
