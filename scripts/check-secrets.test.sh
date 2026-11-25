#!/usr/bin/env bash
# Tests for scripts/check-secrets.sh. Each case is a throwaway git checkout
# in a temporary directory removed on exit, scanned with --root. No fake
# secret is ever committed here: each is made at run time (a Stripe-shaped
# key, gitleaks' stripe-access-token rule), and the checks on the output are
# boolean (grep -q), never printing one. git runs with no global or system
# configuration.
#
# Run: scripts/check-secrets.test.sh
# bash 3.2 compatible (macOS's /bin/bash).
set -u
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
script="$SCRIPT_DIR/check-secrets.sh"

failures=0
check() { # <name> <shell-expr that must succeed>
  if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; failures=$((failures + 1)); fi
}

tmp="$(mktemp -d "${TMPDIR:-/tmp}/check-secrets-test.XXXXXX")" || exit 1
trap 'rm -rf "$tmp"' EXIT
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

# Two fake keys, made here, so this file never holds one.
prefix="sk_"
make_key() { printf '%slive_%s' "$prefix" "$(openssl rand -base64 48 | LC_ALL=C tr -dc 'A-Za-z0-9' | cut -c1-24)"; }
fake=$(make_key)
other=$(make_key)
check "the run-time keys have the shape" \
  "printf '%s\n%s\n' '$fake' '$other' | grep -cxE 'sk_live_[A-Za-z0-9]{24}' | grep -qx 2"

# checkout <name>: a new git checkout at $tmp/<name> with one harmless tracked file.
checkout() {
  local dir="$tmp/$1"
  mkdir -p "$dir/src" && git -C "$dir" init -q || return 1
  echo 'package src // the answer is 42' >"$dir/src/plain.go"
  git -C "$dir" add -A
}
# with_key <dir> <path>: <path> holds the fake key on its line 2.
with_key() { mkdir -p "$(dirname "$1/$2")" && printf '// a fixture\nconst key = "%s"\n' "$fake" >"$1/$2"; }
# allow_fake <dir>: a .gitleaks.toml, as the repository's, allowing the fake key by its value.
allow_fake() {
  printf '[extend]\nuseDefault = true\n\n[[allowlists]]\ndescription = "a test value"\nregexes = [%s]\n' \
    "'''$fake'''" >"$1/.gitleaks.toml"
}
run() { out="$(bash "$script" "$@" 2>&1)"; rc=$?; }
has() { printf '%s\n' "$out" | grep -qF -- "$1"; }
no_key() { ! printf '%s\n' "$out" | grep -qF -e "$fake" -e "$other"; }
FP="src/fixture_test.go:stripe-access-token:2"

checkout clean
run --root "$tmp/clean"
check "a checkout with no finding passes (exit 0)" "[ $rc -eq 0 ] && has 'no finding'"

checkout tracked
with_key "$tmp/tracked" src/fixture_test.go
git -C "$tmp/tracked" add -A
run --root "$tmp/tracked"
check "a tracked key fails (exit 1), naming its fingerprint" "[ $rc -eq 1 ] && has '$FP'"
check "the key itself is never printed" "no_key"

checkout untracked
with_key "$tmp/untracked" src/fixture_test.go
run --root "$tmp/untracked"
check "an untracked, not ignored file is scanned too (exit 1)" "[ $rc -eq 1 ] && has '$FP' && no_key"

checkout ignored
with_key "$tmp/ignored" local/secrets.env
echo 'local/' >"$tmp/ignored/.gitignore"
git -C "$tmp/ignored" add .gitignore
run --root "$tmp/ignored"
check "a git-ignored file is never scanned (exit 0)" "[ $rc -eq 0 ] && no_key"

checkout allowed
with_key "$tmp/allowed" src/fixture_test.go
allow_fake "$tmp/allowed"
git -C "$tmp/allowed" add -A
run --root "$tmp/allowed"
check "a value .gitleaks.toml allows passes (exit 0)" "[ $rc -eq 0 ] && has 'no finding' && no_key"

checkout beside
with_key "$tmp/beside" src/fixture_test.go
printf 'const another = "%s"\n' "$other" >>"$tmp/beside/src/fixture_test.go"
allow_fake "$tmp/beside"
git -C "$tmp/beside" add -A
run --root "$tmp/beside"
check "a key beside an allowed one, in the same file, still fails (exit 1)" \
  "[ $rc -eq 1 ] && has 'src/fixture_test.go:stripe-access-token:3' && ! has '$FP' && no_key"

checkout env
with_key "$tmp/env" src/fixture_test.go
git -C "$tmp/env" add -A
allow_fake "$tmp"
out="$(GITLEAKS_CONFIG="$tmp/.gitleaks.toml" bash "$script" --root "$tmp/env" 2>&1)"; rc=$?
check "GITLEAKS_CONFIG is never read (exit 1)" "[ $rc -eq 1 ] && has '$FP' && no_key"
out="$(GITLEAKS_CONFIG_TOML="$(cat "$tmp/.gitleaks.toml")" bash "$script" --root "$tmp/env" 2>&1)"; rc=$?
check "GITLEAKS_CONFIG_TOML is never read (exit 1)" "[ $rc -eq 1 ] && has '$FP' && no_key"

mkdir -p "$tmp/not-git"
run --root "$tmp/not-git"
check "a --root that isn't a git checkout's root is refused (exit 2)" "[ $rc -eq 2 ]"
run --root "$tmp/clean/src"
check "a --root inside a checkout, not its root, is refused (exit 2)" "[ $rc -eq 2 ]"
run --nope
check "an unknown argument is refused (exit 2)" "[ $rc -eq 2 ]"

if [ "$failures" -gt 0 ]; then
  echo "check-secrets.test: $failures failure(s)"
  exit 1
fi
echo "check-secrets.test: all passed"
