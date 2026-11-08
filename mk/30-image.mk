# The image: a local build and its check. Neither is a gate (not in
# CHECK_TARGETS); CI runs both. A release's image is built and pushed by the
# release workflow (.github/workflows/release.yml).

# `make image`'s builder: one using the `docker` driver, so --load puts the
# image in the local engine (a docker-container builder can't load it).
BUILDER ?= desktop-linux
# The local image's labels: `dev` and this checkout's HEAD (`unknown`
# outside a git checkout). A release's come from the release workflow.
IMAGE_VERSION ?= dev
IMAGE_REVISION ?= $(shell git rev-parse --verify --quiet HEAD 2>/dev/null || echo unknown)

.PHONY: image image-check

# A local, single-platform opamp-server:dev, loaded into the engine. It uses
# no registry but the base images' (pinned by digest in the Dockerfile).
# A builder named after a Docker context (desktop-linux) is that context's
# docker-driver builder, and buildx 0.8.2 uses it only from that context
# ("use `docker --context=desktop-linux buildx`"), so the context is named
# when one exists.
image:
	@set -euo pipefail; \
	cli=(docker); \
	if docker context inspect '$(BUILDER)' >/dev/null 2>&1; then cli=(docker --context '$(BUILDER)'); fi; \
	command=("$${cli[@]}" buildx --builder '$(BUILDER)' build --load \
	  --build-arg 'VERSION=$(IMAGE_VERSION)' --build-arg 'REVISION=$(IMAGE_REVISION)' \
	  -t opamp-server:dev .); \
	echo "$${command[*]}"; \
	"$${command[@]}"

# The loaded opamp-server:dev's healthcheck subcommand, with no server
# answering: it exits 1 with its fixed message. It has no help: given an
# argument, it prints the usage line and exits 2. It runs in the engine
# `make image` loaded into (the same context), and never pulls.
image-check:
	@set -euo pipefail; \
	cli=(docker); \
	if docker context inspect '$(BUILDER)' >/dev/null 2>&1; then cli=(docker --context '$(BUILDER)'); fi; \
	status=0; out=$$("$${cli[@]}" run --rm --pull never --network none opamp-server:dev healthcheck 2>&1) || status=$$?; \
	if [ "$$status" -ne 1 ] || ! printf '%s\n' "$$out" | grep -q 'healthcheck failed'; then \
	  echo "image-check: healthcheck with no server: expected exit 1 and \"healthcheck failed\", got exit $$status:" >&2; \
	  printf '%s\n' "$$out" >&2; exit 1; \
	fi; \
	status=0; out=$$("$${cli[@]}" run --rm --pull never --network none opamp-server:dev healthcheck --help 2>&1) || status=$$?; \
	if [ "$$status" -ne 2 ] || ! printf '%s\n' "$$out" | grep -q 'usage: opamp-server'; then \
	  echo "image-check: healthcheck --help: expected exit 2 and the usage line, got exit $$status:" >&2; \
	  printf '%s\n' "$$out" >&2; exit 1; \
	fi; \
	echo "image-check: healthcheck exits 1 with no server, and 2 with the usage line given an argument."

