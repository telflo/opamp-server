# The chart's gates, in the order `make check` runs them. All run offline
# (the Helm ones template, lint and package only, with Helm's homes in
# temporary directories); none needs Postgres.

CHART_CHECK_TARGETS := chart-bootstrap-copy chart-test chart-cloud-neutral
CHECK_TARGETS += $(CHART_CHECK_TARGETS)

.PHONY: $(CHART_CHECK_TARGETS)

# The chart's bootstrap.sql is deploy/bootstrap.sql byte for byte: the chart
# runs the same bootstrap the README does.
chart-bootstrap-copy:
	@cmp deploy/bootstrap.sql charts/telflo-opamp-server/files/bootstrap.sql || { \
	  echo "charts/telflo-opamp-server/files/bootstrap.sql differs from deploy/bootstrap.sql: copy it again"; exit 1; }

# The render tests: every value, guard, refusal and credential reference,
# by a real `helm template`, then `helm lint --strict` with each complete
# profile in charts/telflo-opamp-server/tests/profiles/.
chart-test:
	bash charts/telflo-opamp-server/tests/render-tests.sh

# Nothing cloud-specific in the chart, its packaged files or its output, and
# no annotation key written by a template; then the check's self-test, which
# breaks each of those on a copy of the chart.
chart-cloud-neutral:
	@set -euo pipefail; \
	./scripts/check-chart-cloud-neutral.sh; \
	./scripts/check-chart-cloud-neutral.test.sh
