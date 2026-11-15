#!/usr/bin/env bash
# Runs deploy/bootstrap.sql twice and fails if the second run changed
# anything it shouldn't: the three roles' attributes, memberships and
# settings, the database's owner, encoding and ACL, and the role settings in
# it. It also fails unless the database is UTF8, PUBLIC holds nothing on it,
# and the server's role logs its errors terse (no DETAIL, so no name a
# unique key refused, in Postgres's own log). The passwords are set again on
# every run by design, so they are left out of the comparison.
# First it checks that bootstrap.sql refuses to run, before changing
# anything, when either password is missing from its environment or empty,
# and that a password given as a `-v` argument doesn't count.
# `make check` runs this before the Go tests, which then find the roles
# already there.
#
# Usage: scripts/check-bootstrap.sh [database]
#
#   database  the database bootstrap.sql creates; defaults to telflo_opamp_server.
#             A name ending in `_test` is a throwaway: the script drops it
#             when it exits, whether the check passed or failed. Any other
#             database is left in place, never dropped. The roles are
#             server-wide and shared, so they always stay.
#
# PSQL is the psql command to run, as a superuser on the server's `postgres`
# database (default: psql). A machine with no psql of its own runs the
# server's, e.g.
#   PSQL="docker exec -i -e OPAMP_OWNER_PASSWORD -e OPAMP_RUNTIME_PASSWORD \
#     <container> psql -U telflo" scripts/check-bootstrap.sh
# The script is passed on psql's standard input, so `docker exec -i` works.
# bootstrap.sql reads the two passwords from its environment, never from the
# command line (which the process list shows): a bare `-e NAME` makes docker
# copy that variable's value from its own environment into the container.
#
# OPAMP_OWNER_PASSWORD and OPAMP_RUNTIME_PASSWORD default to the development
# passwords the Go test harness uses (store/database/databasetest); this
# script is for development and test servers only.
#
# Exits 0 when the second run changed nothing, 1 when it did, 2 when a run
# failed or a throwaway database couldn't be dropped.
set -euo pipefail

database=${1:-telflo_opamp_server}
read -r -a psql <<<"${PSQL:-psql}"
service_dir=$(cd "$(dirname "$0")/.." && pwd)
bootstrap="$service_dir/deploy/bootstrap.sql"

export OPAMP_OWNER_PASSWORD=${OPAMP_OWNER_PASSWORD:-telflo-opamp-owner-local}
export OPAMP_RUNTIME_PASSWORD=${OPAMP_RUNTIME_PASSWORD:-telflo-opamp-local}

# macOS's mktemp ignores TMPDIR without a template, so name one under it.
work=$(mktemp -d "${TMPDIR:-/tmp}/check-bootstrap.XXXXXX")

# On every exit, passed or failed: remove the scratch files and, for a
# throwaway `_test` database only, drop it. A failed drop turns a pass into
# exit 2, so a leftover database is never silent.
cleanup() {
  local status=$?
  rm -rf "$work"
  if [[ $database == *_test ]]; then
    if "${psql[@]}" -X -q -d postgres -v ON_ERROR_STOP=1 -v database="$database" \
      <<<'drop database if exists :"database";'; then
      echo "Dropped the throwaway database $database."
    else
      echo "ERROR: couldn't drop the throwaway database $database" >&2
      [[ $status -ne 0 ]] || status=2
    fi
  fi
  exit "$status"
}
trap cleanup EXIT

run_bootstrap() {
  if ! "${psql[@]}" -X -q -d postgres -v ON_ERROR_STOP=1 \
    -v database="$database" <"$bootstrap"; then
    echo "ERROR: bootstrap.sql failed" >&2
    exit 2
  fi
}

# expect_refusal <label> <message> <env…>: bootstrap.sql, run with the
# environment changed by `env <env…>`, must exit non-zero, saying <message>.
expect_refusal() {
  local label=$1 message=$2 output
  shift 2
  if output=$(env "$@" "${psql[@]}" -X -q -d postgres -v ON_ERROR_STOP=1 \
    -v owner_password=given-on-the-command-line \
    -v runtime_password=given-on-the-command-line \
    -v database="$database" <"$bootstrap" 2>&1); then
    echo "ERROR: bootstrap.sql ran with $label" >&2
    exit 1
  fi
  if [[ $output != *"$message"* ]]; then
    echo "ERROR: bootstrap.sql, with $label, failed without saying \"$message\":" >&2
    echo "$output" >&2
    exit 1
  fi
  echo "bootstrap.sql refuses to run with $label."
}

# Everything bootstrap.sql decides, one fact per line, in a stable order.
catalog() {
  "${psql[@]}" -X -q -A -t -d postgres -v ON_ERROR_STOP=1 -v database="$database" <<'SQL'
select 'role', rolname, rolsuper, rolinherit, rolcreaterole, rolcreatedb, rolcanlogin,
       rolreplication, rolbypassrls, rolconnlimit, rolvaliduntil
  from pg_roles
 where rolname in ('telflo_opamp_server_owner', 'telflo_opamp_server_runtime', 'telflo_opamp_server')
 order by rolname;
select 'membership', m.roleid::regrole, m.member::regrole, m.grantor::regrole,
       m.admin_option, m.inherit_option, m.set_option
  from pg_auth_members m
 where m.roleid::regrole::text in ('telflo_opamp_server_owner', 'telflo_opamp_server_runtime', 'telflo_opamp_server')
    or m.member::regrole::text in ('telflo_opamp_server_owner', 'telflo_opamp_server_runtime', 'telflo_opamp_server')
 order by 2, 3, 4;
select 'database', datname, pg_get_userbyid(datdba), pg_encoding_to_char(encoding), datacl
  from pg_database
 where datname = :'database';
select 'setting', coalesce(s.setrole::regrole::text, '(all roles)'), s.setconfig
  from pg_db_role_setting s
  join pg_database d on d.oid = s.setdatabase
 where d.datname = :'database'
 order by 2;
select 'role setting', s.setrole::regrole::text, s.setconfig
  from pg_db_role_setting s
 where s.setdatabase = 0
   and s.setrole::regrole::text in ('telflo_opamp_server_owner', 'telflo_opamp_server_runtime', 'telflo_opamp_server')
 order by 2;
SQL
}

# The refusals come first: each stops bootstrap.sql before its first role or
# database statement. The `-v` passwords expect_refusal passes must be
# ignored, or the unset cases would run.
expect_refusal "OPAMP_OWNER_PASSWORD unset" "needs OPAMP_OWNER_PASSWORD in its environment" \
  -u OPAMP_OWNER_PASSWORD
expect_refusal "OPAMP_RUNTIME_PASSWORD unset" "needs OPAMP_RUNTIME_PASSWORD in its environment" \
  -u OPAMP_RUNTIME_PASSWORD
expect_refusal "OPAMP_OWNER_PASSWORD empty" "needs OPAMP_OWNER_PASSWORD to be non-empty" \
  OPAMP_OWNER_PASSWORD=
expect_refusal "OPAMP_RUNTIME_PASSWORD empty" "needs OPAMP_RUNTIME_PASSWORD to be non-empty" \
  OPAMP_RUNTIME_PASSWORD=

run_bootstrap
catalog >"$work/first"
run_bootstrap
catalog >"$work/second"

echo "After the first run of bootstrap.sql (database $database):"
sed 's/^/  /' "$work/first"
# The database is UTF8 whatever the server's default, and
# PUBLIC holds nothing on it: no CONNECT, no TEMPORARY (an ACL entry for
# PUBLIC starts with "=").
if ! grep -q "^database|$database|telflo_opamp_server_owner|UTF8|" "$work/first"; then
  echo "ERROR: bootstrap.sql didn't make $database a UTF8 database owned by telflo_opamp_server_owner." >&2
  exit 1
fi
if grep "^database|" "$work/first" | grep -Eq '[{,]='; then
  echo "ERROR: PUBLIC still holds a privilege on $database." >&2
  exit 1
fi
if ! grep "^role setting|telflo_opamp_server|" "$work/first" | grep -Eq '[{,]log_error_verbosity=terse[,}]'; then
  echo "ERROR: bootstrap.sql didn't set log_error_verbosity to terse for opamp_server." >&2
  exit 1
fi
if ! diff -u "$work/first" "$work/second"; then
  echo "ERROR: running bootstrap.sql a second time changed the catalog (above)." >&2
  exit 1
fi
echo "A second run changed nothing."
