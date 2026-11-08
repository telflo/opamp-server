// Package telemetry configures OpenTelemetry (traces, metrics, logs) for the
// opamp-server. It sets the global providers and the W3C propagator, and is a
// NO-OP when no OTLP endpoint is configured — so local dev, tests, and CI
// neither export nor spam connection errors.
package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// defaultServiceName is used when neither OTEL_SERVICE_NAME nor a service.name
// in OTEL_RESOURCE_ATTRIBUTES is provided.
const defaultServiceName = "telflo-opamp-server"

// The variables Setup reads, through the getenv it is given. The OTLP
// exporters and the SDK still read their own `OTEL_*` tuning options
// (`OTEL_EXPORTER_OTLP_*` headers/TLS/timeouts, `OTEL_BSP_*`, `OTEL_BLRP_*`,
// `OTEL_METRIC_EXPORT_*`, `OTEL_TRACES_SAMPLER*`) from the process's
// environment.
const (
	endpointVariable           = "OTEL_EXPORTER_OTLP_ENDPOINT"
	legacyAddressVariable      = "OTEL_COLLECTOR_ADDR"
	serviceNameVariable        = "OTEL_SERVICE_NAME"
	resourceAttributesVariable = "OTEL_RESOURCE_ATTRIBUTES"
)

// Telemetry holds the configured providers behind a single combined Shutdown.
type Telemetry struct {
	shutdown func(context.Context) error
}

// Shutdown flushes and stops all providers. Safe to call on a no-op Telemetry.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t == nil || t.shutdown == nil {
		return nil
	}
	return t.shutdown(ctx)
}

// Noop returns a Telemetry whose Shutdown does nothing.
func Noop() *Telemetry {
	return &Telemetry{shutdown: func(context.Context) error { return nil }}
}

// exporters builds the three OTLP exporters for an endpoint. Setup uses the
// gRPC ones; tests pass their own, to see what a partial failure leaves.
type exporters struct {
	trace  func(ctx context.Context, endpoint string) (sdktrace.SpanExporter, error)
	metric func(ctx context.Context, endpoint string) (sdkmetric.Exporter, error)
	log    func(ctx context.Context, endpoint string) (sdklog.Exporter, error)
}

var grpcExporters = exporters{
	trace: func(ctx context.Context, endpoint string) (sdktrace.SpanExporter, error) {
		return otlptracegrpc.New(ctx, otlptracegrpc.WithEndpointURL(endpoint))
	},
	metric: func(ctx context.Context, endpoint string) (sdkmetric.Exporter, error) {
		return otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpointURL(endpoint))
	},
	log: func(ctx context.Context, endpoint string) (sdklog.Exporter, error) {
		return otlploggrpc.New(ctx, otlploggrpc.WithEndpointURL(endpoint))
	},
}

// Setup initializes traces, metrics, and logs from the standard OTLP
// variables, read through getenv, sets the global providers + W3C
// propagator, and returns a handle whose Shutdown flushes everything. When
// no endpoint is configured it returns a no-op Telemetry (nil error).
// Transport is OTLP over gRPC; TLS is derived from the endpoint scheme
// (http:// = plaintext, https:// = TLS). Its few log lines go to logger;
// none quotes the endpoint, which can carry a password.
//
// An endpoint that isn't a URL with a scheme and a host turns telemetry off,
// with a warning that doesn't quote it.
//
// Before it builds anything it points OpenTelemetry's own logger and error
// handler at logger (see quietSDK): the SDK's defaults print to stderr, and
// what they print can quote an endpoint, password and all. It sets the
// providers and the propagator only once every provider is built: on a
// failure it shuts down the providers it had already built and leaves those
// globals as they were.
//
// Endpoint resolution (first non-empty wins):
//   - OTEL_EXPORTER_OTLP_ENDPOINT  (standard; e.g. http://telflo-collector:4317)
//   - OTEL_COLLECTOR_ADDR          (legacy host:port; migrated with a warning)
func Setup(ctx context.Context, getenv func(string) string, logger *slog.Logger) (*Telemetry, error) {
	return setup(ctx, getenv, logger, grpcExporters)
}

func setup(ctx context.Context, getenv func(string) string, logger *slog.Logger, build exporters) (*Telemetry, error) {
	endpoint := resolveEndpoint(getenv, logger)
	if endpoint == "" {
		return Noop(), nil // the no-op gate
	}

	// Before anything that can log: the exporters read the process's
	// environment and report what they can't parse, value included.
	quietSDK(logger)

	res, err := newResource(ctx, getenv, logger)
	if err != nil {
		return nil, err
	}

	// Each provider's Shutdown, in the order built; on a failure the ones
	// built so far are shut down, last first, so no exporter is left running.
	var shutdowns []func(context.Context) error
	shutdownAll := func(ctx context.Context) error {
		var errs []error
		for i := len(shutdowns) - 1; i >= 0; i-- {
			if e := shutdowns[i](ctx); e != nil {
				errs = append(errs, e)
			}
		}
		return errors.Join(errs...)
	}
	fail := func(err error) (*Telemetry, error) {
		_ = shutdownAll(ctx)
		return nil, err
	}

	// --- Traces ---
	traceExp, err := build.trace(ctx, endpoint)
	if err != nil {
		return fail(err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExp),
	)
	shutdowns = append(shutdowns, tp.Shutdown)

	// --- Metrics (runtime/process only) ---
	// No http.server.* metrics are emitted: the collector's spanmetrics
	// connector owns the golden signals.
	metricExp, err := build.metric(ctx, endpoint)
	if err != nil {
		return fail(err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
	)
	shutdowns = append(shutdowns, mp.Shutdown)

	// --- Logs ---
	logExp, err := build.log(ctx, endpoint)
	if err != nil {
		return fail(err)
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
	)
	shutdowns = append(shutdowns, lp.Shutdown)

	// Runtime metrics are a nicety: without them the rest still exports.
	if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
		logger.Warn("telemetry: runtime metrics failed to start")
	}

	// Everything is built: only now do the globals change.
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetLoggerProvider(lp)
	// W3C trace-context + baggage: extract inbound traceparent, inject outbound.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	serviceName, _ := res.Set().Value("service.name")
	logger.Info("telemetry: OpenTelemetry initialized", "service", serviceName.String())

	return &Telemetry{shutdown: shutdownAll}, nil
}

// quietSDK sends OpenTelemetry's own diagnostics to logger without their
// details. The SDK reports through two globals, and each one's default
// prints to stderr: its internal logr logger (stdr), which an exporter
// gives the value it couldn't parse ("parse url", "input", <the URL>), and
// its error handler (the log package), whose errors can quote an endpoint
// or a variable's value. Only the logr call's fixed message is kept, and
// only for errors; the error handler logs a fixed line. Neither global is a
// provider: setting them early changes nothing a failed Setup would need
// to undo.
func quietSDK(logger *slog.Logger) {
	otel.SetLogger(logr.New(messageOnlySink{logger: logger}))
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {
		logger.Warn("telemetry: OpenTelemetry reported an error (not quoted: it can name the endpoint)")
	}))
}

// messageOnlySink is a logr.LogSink that drops the SDK's info, warning and
// debug lines, and logs an error by its message alone: the message is a
// fixed string in the SDK's code, while the error and the key/value pairs
// can carry a URL or a variable's value.
type messageOnlySink struct{ logger *slog.Logger }

func (messageOnlySink) Init(logr.RuntimeInfo)            {}
func (messageOnlySink) Enabled(int) bool                 { return false }
func (messageOnlySink) Info(int, string, ...any)         {}
func (s messageOnlySink) WithValues(...any) logr.LogSink { return s }
func (s messageOnlySink) WithName(string) logr.LogSink   { return s }
func (s messageOnlySink) Error(_ error, msg string, _ ...any) {
	s.logger.Warn("telemetry: OpenTelemetry logged an error (its details aren't quoted: they can name the endpoint)",
		"otel_message", msg)
}

// resolveEndpoint returns the OTLP endpoint URL (with scheme), preferring the
// standard variable and falling back to the legacy OTEL_COLLECTOR_ADDR, or ""
// for no telemetry: when neither is set, or when the one set isn't a URL
// with a scheme and a host (the exporters would quietly fall back to
// localhost:4317). No warning quotes the value: it can carry a password.
func resolveEndpoint(getenv func(string) string, logger *slog.Logger) string {
	if ep := strings.TrimSpace(getenv(endpointVariable)); ep != "" {
		if !isURL(ep) {
			logger.Warn("telemetry: OTEL_EXPORTER_OTLP_ENDPOINT isn't a URL; telemetry is off")
			return ""
		}
		return ep
	}
	if addr := strings.TrimSpace(getenv(legacyAddressVariable)); addr != "" {
		logger.Warn("telemetry: OTEL_COLLECTOR_ADDR is deprecated; " +
			"set OTEL_EXPORTER_OTLP_ENDPOINT=http://<the same address> instead")
		if !strings.Contains(addr, "://") {
			addr = "http://" + addr
		}
		if !isURL(addr) {
			logger.Warn("telemetry: OTEL_COLLECTOR_ADDR isn't an address; telemetry is off")
			return ""
		}
		return addr
	}
	return ""
}

// isURL reports whether s parses as a URL with a scheme and a host. The
// parser's error isn't kept: it quotes s.
func isURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme != "" && u.Host != ""
}

// newResource describes this process: telemetry.sdk.* and process.*, then
// OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME read through getenv (the
// SDK's own detector would read the process's environment), with
// service.name defaulting to telflo-opamp-server. The default is a resource
// attribute, never written into the environment.
func newResource(ctx context.Context, getenv func(string) string, logger *slog.Logger) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{attribute.String("service.name", defaultServiceName)}
	attrs = append(attrs, resourceAttributes(getenv(resourceAttributesVariable), logger)...)
	if name := strings.TrimSpace(getenv(serviceNameVariable)); name != "" {
		// OTEL_SERVICE_NAME wins over a service.name in the attributes, as
		// the specification says.
		attrs = append(attrs, attribute.String("service.name", name))
	}
	res, err := resource.New(ctx,
		resource.WithTelemetrySDK(), // telemetry.sdk.*
		resource.WithProcess(),      // process.*
		// Last, so these win; within them, a later duplicate key wins.
		resource.WithAttributes(attrs...),
	)
	// A detector that fails partly still yields a resource; prefer it over
	// failing telemetry entirely.
	if err != nil && res == nil {
		return nil, err
	}
	return res, nil
}

// resourceAttributes parses OTEL_RESOURCE_ATTRIBUTES ("k1=v1,k2=v2,..."),
// as the SDK's environment detector does: keys and values trimmed, values
// percent-decoded (kept as written when that fails). An entry without "="
// is skipped with a warning that doesn't quote it.
func resourceAttributes(raw string, logger *slog.Logger) []attribute.KeyValue {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var attrs []attribute.KeyValue
	for _, pair := range strings.Split(raw, ",") {
		key, value, found := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !found || key == "" {
			logger.Warn("telemetry: OTEL_RESOURCE_ATTRIBUTES has an entry that isn't key=value; it is ignored")
			continue
		}
		value = strings.TrimSpace(value)
		if decoded, err := url.PathUnescape(value); err == nil {
			value = decoded
		}
		attrs = append(attrs, attribute.String(key, value))
	}
	return attrs
}
