# The opamp-server's gates. `make check` runs every gate, in order, and is
# what must pass before a change is pushed (CONTRIBUTING.md).
#
# Each mk/NN-*.mk fragment adds its own targets and appends the ones that are
# gates to CHECK_TARGETS; the fragments are read in their numbered order, so
# that is the order `make check` runs them in. Every gate can also run alone
# (`make go-vet`).
#
# `make check` needs a superuser Postgres it never starts itself: set
# OPAMP_TEST_ADMIN_DATABASE_URL (a superuser URL to the server's `postgres`
# database) and PSQL (the psql command to run as that superuser), as
# CONTRIBUTING.md shows.

# make 3.81 (macOS's) ignores .SHELLFLAGS, so every recipe with more than one
# command starts with `set -euo pipefail;` itself.
SHELL := /bin/bash

# One database: the gates never run side by side, even under `make -j`.
.NOTPARALLEL:

.DEFAULT_GOAL := help

# Where the gates write their reports (git-ignored).
CHECK_DIR := .check

CHECK_TARGETS :=
include $(sort $(wildcard mk/*.mk))

.PHONY: help check require-postgres

help:
	@echo "make check             every gate, in order: $(strip $(CHECK_TARGETS))"
	@echo "make <gate>            one of those gates alone"
	@echo "make require-postgres  checks that OPAMP_TEST_ADMIN_DATABASE_URL and PSQL are set"

# The prerequisites run in the order listed (.NOTPARALLEL), and the first
# failure stops the rest.
check: require-postgres $(CHECK_TARGETS)
	@echo "make check: every gate passed ($(strip $(CHECK_TARGETS)))."

# Refuses (exit 2) unless both variables are set and non-empty. Their values
# are never printed: the URL holds a password.
require-postgres:
	@set -euo pipefail; \
	missing=""; \
	if [ -z "$${OPAMP_TEST_ADMIN_DATABASE_URL:-}" ]; then missing="$$missing OPAMP_TEST_ADMIN_DATABASE_URL"; fi; \
	if [ -z "$${PSQL:-}" ]; then missing="$$missing PSQL"; fi; \
	if [ -n "$$missing" ]; then \
	  echo "ERROR: the gates need a superuser Postgres: set OPAMP_TEST_ADMIN_DATABASE_URL and PSQL (unset or empty:$$missing). See CONTRIBUTING.md." >&2; \
	  exit 2; \
	fi
