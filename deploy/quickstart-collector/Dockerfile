# The quick start's collector: OpenTelemetry Collector Contrib with the
# OpAMP supervisor beside it, for compose.yaml's `collector` service and
# kubernetes.yaml. Never a deployment image.
#
# The supervisor is the container's process: it connects to the
# opamp-server, then runs otelcol-contrib with the configuration the server
# sends. Both come from upstream images at the same version, the collector
# version the server is tested against.

FROM otel/opentelemetry-collector-opampsupervisor:0.162.0 AS supervisor
# The contrib image has no shell, so the supervisor's state directory is
# made here and copied across, owned by the contrib image's user.
USER root
RUN mkdir -p /state/var/lib/opampsupervisor

FROM otel/opentelemetry-collector-contrib:0.162.0
COPY --from=supervisor /usr/local/bin/opampsupervisor /opampsupervisor
COPY --from=supervisor --chown=10001:10001 /state/var/lib/opampsupervisor /var/lib/opampsupervisor
# Readable by the contrib image's user whatever mode the checkout gave it.
# 0755, not 0444: --chmod also sets the folder the COPY creates, which the
# user can enter only with its execute bit.
COPY --chmod=0755 supervisor.yaml /etc/opampsupervisor/supervisor.yaml
ENTRYPOINT ["/opampsupervisor"]
CMD ["--config", "/etc/opampsupervisor/supervisor.yaml"]
