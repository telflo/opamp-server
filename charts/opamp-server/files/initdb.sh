#!/bin/sh
# Runs bootstrap.sql once, when the postgres image's entrypoint creates a new
# data directory (it runs the executable *.sh files in
# /docker-entrypoint-initdb.d, and only then). bootstrap.sql itself is mounted
# outside that folder, at /opamp-bootstrap, so the entrypoint never runs it a
# second time as a *.sql file.
#
# The two passwords reach bootstrap.sql through this process's environment
# (OPAMP_OWNER_PASSWORD and OPAMP_RUNTIME_PASSWORD, read with \getenv), never
# a command line. The database's name is OPAMP_DATABASE_NAME, telflo_opamp_server
# when unset.
#
# log_min_error_statement=panic keeps a failing statement out of Postgres's
# log: the statements that set the passwords take no bind parameters, so a
# failed one would be logged whole, password and all.
set -eu
PGOPTIONS='-c log_min_error_statement=panic'
export PGOPTIONS
exec psql -X -v ON_ERROR_STOP=1 --no-password --username "$POSTGRES_USER" --dbname postgres \
  -v database="${OPAMP_DATABASE_NAME:-telflo_opamp_server}" -f /opamp-bootstrap/bootstrap.sql
