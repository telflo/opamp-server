# The minikube smoke: the chart installed for real on a throwaway minikube
# profile, checked end to end, then removed (scripts/smoke-minikube.sh says
# what it checks). Not a gate (not in CHECK_TARGETS): it needs Docker and
# minikube and takes a few minutes. Run it by hand before a release, and
# after a change to the chart or the image.

# The profile it creates and deletes; it refuses one that exists already.
SMOKE_PROFILE ?= opamp-smoke
# Its work folder (logs; the generated secrets, deleted at the end). Empty:
# a new one under TMPDIR.
SMOKE_WORK ?=

.PHONY: smoke

# `make image` first, so the smoke loads the image built from this tree.
smoke: image
	@set -euo pipefail; \
	args=(--profile '$(SMOKE_PROFILE)'); \
	if [ -n '$(SMOKE_WORK)' ]; then args+=(--work '$(SMOKE_WORK)'); fi; \
	./scripts/smoke-minikube.sh "$${args[@]}"
