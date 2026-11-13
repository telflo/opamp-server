# The opamp-server's image, built from the repository's root, as
# compose.yaml and `make image` do:
#
#   docker build -t opamp-server:dev .
#
# The release workflow (.github/workflows/release.yml) builds it for
# linux/amd64 and linux/arm64 at once, from the release's tag. The build
# stage runs on the builder's own platform and cross-compiles (GOOS and
# GOARCH), so neither platform's compile runs under emulation; only the
# runtime stage is per platform.
#
# One binary, three subcommands (main.go):
#
#   serve                   the default: the management API on
#                           OPAMP_MANAGEMENT_PORT and the agent endpoint
#                           (POST /opamp/api/v1, where collectors poll) on
#                           OPAMP_AGENT_PORT
#   migrate up|status       the embedded migrations, as telflo_opamp_server_owner
#                           (`migrate down --yes-drop-data` is for
#                           development only)
#   healthcheck             asks this container's own /health, for the
#                           container health check (no shell or curl here)
#
# deploy/bootstrap.sql (the database and its roles) is not in the image: a
# superuser runs it with psql, once, before the first `migrate up`.

# ---- build stage ----------------------------------------------------------
# The Go 1.27 line go.mod names (go 1.27.3), pinned by its index digest:
# golang:1.27-alpine was Go 1.27.3 on Alpine 3.24 when resolved, on
# 2026-10-31. To move it, take the new digest from
# `docker buildx imagetools inspect golang:1.27-alpine`.
# Alpine keeps the build image small; CGO is disabled in the build step so
# the binary is fully static and runs on distroless/static without libc.
# --platform=$BUILDPLATFORM: this stage is the builder's platform's, whatever
# the target.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS builder

WORKDIR /src

# Cache module downloads across layer rebuilds when only source files change.
# RUN --mount is BuildKit's (Docker's default builder), in its built-in
# Dockerfile frontend, so this file names no `# syntax=` image to fetch.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# The target's OS and architecture (BuildKit sets them per platform),
# declared after the download so its layer is the same for every platform.
ARG TARGETOS TARGETARCH

# Copy the rest of the source. .dockerignore keeps local binaries, .env
# files, docs, the chart, deploy/, the scripts, the Makefile and the tests
# out of the build context.
COPY . .

# -trimpath strips local paths from the binary; -ldflags '-s -w' drops the
# symbol table and DWARF info to shrink it. `.` is the main package. It goes
# in /out/app, the runtime stage's /app.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags='-s -w' -o /out/app/opamp-server .

# ---- runtime stage --------------------------------------------------------
# distroless/static: no shell, no package manager, ~2MB base. The :nonroot
# tag runs as uid 65532. Pinned by its index digest, as a reviewed
# component; resolved on 2026-10-07. To move it, take the
# new digest from `docker buildx imagetools inspect
# gcr.io/distroless/static-debian12:nonroot`.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

# The release's version and its commit (the release workflow passes both);
# the defaults mark a local build.
ARG VERSION=dev
ARG REVISION=unknown

LABEL org.opencontainers.image.title="opamp-server" \
      org.opencontainers.image.description="An OpAMP server for OpenTelemetry Collector fleets, with a management API" \
      org.opencontainers.image.vendor="Telflo" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.source="https://github.com/telflo/opamp-server" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$REVISION"

# /app and the binary belong to root, mode 0755: the runtime user (uid
# 65532) can't write either. Copied before WORKDIR, which would otherwise
# make /app as the base image's user, 65532.
COPY --from=builder --chown=0:0 --chmod=0755 /out/app /app

WORKDIR /app

# The agent endpoint's port and the management API's (OPAMP_AGENT_PORT's
# and OPAMP_MANAGEMENT_PORT's defaults). The variables each subcommand
# reads, all supplied by the orchestrator (docs/configuration.md documents each):
#
#   serve        OPAMP_DATABASE_URL (as telflo_opamp_server, a member of
#                telflo_opamp_server_runtime; its pool_max_conns parameter caps the
#                connection pool, by default the greater of 4 and the CPUs,
#                and /ready pings on one connection more),
#                the management API's: OPAMP_MANAGEMENT_SECRET (at least 32
#                bytes), OPAMP_MANAGEMENT_SECRETS_PREVIOUS (comma-separated,
#                during a rotation), OPAMP_MANAGEMENT_PORT (default 51235),
#                OPAMP_MANAGEMENT_HOST (an IP address; default every
#                interface, which a container needs),
#                OPAMP_MANAGEMENT_TLS_CERT_FILE and
#                OPAMP_MANAGEMENT_TLS_KEY_FILE (both or neither: HTTPS, TLS
#                1.2 or later; mount them readable by uid 65532);
#                the agent endpoint's: OPAMP_SERVER_URL (required: its public
#                URL, handed to collectors), OPAMP_ENCRYPTION_KEY_FILE or
#                OPAMP_ENCRYPTION_KEY (exactly one, required: the bearers'
#                key, at least 32 bytes; a file mounted as a secret keeps it
#                out of the environment), OPAMP_ENCRYPTION_KEYS_PREVIOUS_FILE
#                or OPAMP_ENCRYPTION_KEYS_PREVIOUS (during a rotation),
#                OPAMP_AGENT_PORT (default 51234), OPAMP_AGENT_HOST,
#                OPAMP_AGENT_MAX_CONNECTIONS (10000),
#                OPAMP_AGENT_TLS_CERT_FILE and OPAMP_AGENT_TLS_KEY_FILE (both
#                or neither; without them a TLS-terminating load balancer is
#                expected in front), OPAMP_HEARTBEAT_INTERVAL_SECONDS (30),
#                OPAMP_MAX_CONCURRENT_REQUESTS (100),
#                OPAMP_MAX_CONCURRENT_REQUESTS_PER_ORGANIZATION (25),
#                OPAMP_DEFAULT_REQUESTS_PER_MINUTE (12000, per pod) and
#                OPAMP_COLLECTOR_REQUESTS_PER_MINUTE (60);
#                for telemetry, OTEL_EXPORTER_OTLP_ENDPOINT (export is off
#                when it is unset; OTEL_COLLECTOR_ADDR is its deprecated
#                spelling), OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES
#   migrate      OPAMP_MIGRATION_DATABASE_URL (as telflo_opamp_server_owner)
#   healthcheck  serve's OPAMP_MANAGEMENT_PORT, OPAMP_MANAGEMENT_HOST and
#                the two management TLS variables (https when they are set)
EXPOSE 51234 51235

USER nonroot:nonroot

ENTRYPOINT ["/app/opamp-server"]
CMD ["serve"]
