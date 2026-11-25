# The secret scan, first of the gates: it needs neither Postgres nor the
# network once gitleaks is in the module cache.

SECRETS_CHECK_TARGETS := secrets
CHECK_TARGETS += $(SECRETS_CHECK_TARGETS)

.PHONY: $(SECRETS_CHECK_TARGETS)

# gitleaks over every file git would commit, with .gitleaks.toml's reviewed
# allowances only; then the check's self-test, which plants a run-time fake
# key in throwaway checkouts.
secrets:
	@set -euo pipefail; \
	./scripts/check-secrets.sh; \
	./scripts/check-secrets.test.sh
