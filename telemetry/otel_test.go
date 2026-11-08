package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// endpointWithPassword is an endpoint no log line may quote.
const endpointWithPassword = "http://collector-user:collector-password-7e1@collector.invalid:4317"

func envOf(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// syncBuffer is a log the SDK's goroutines and the test can share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// shutdowns records which fake exporters were shut down.
type shutdowns struct {
	mu   sync.Mutex
	done []string
}

func (s *shutdowns) record(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = append(s.done, name)
}

func (s *shutdowns) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.done...)
}

type fakeSpanExporter struct{ shut *shutdowns }

func (fakeSpanExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (e fakeSpanExporter) Shutdown(context.Context) error {
	e.shut.record("trace")
	return nil
}

type fakeMetricExporter struct{ shut *shutdowns }

func (fakeMetricExporter) Temporality(kind sdkmetric.InstrumentKind) metricdata.Temporality {
	return sdkmetric.DefaultTemporalitySelector(kind)
}

func (fakeMetricExporter) Aggregation(kind sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return sdkmetric.DefaultAggregationSelector(kind)
}

func (fakeMetricExporter) Export(context.Context, *metricdata.ResourceMetrics) error { return nil }
func (fakeMetricExporter) ForceFlush(context.Context) error                          { return nil }
func (e fakeMetricExporter) Shutdown(context.Context) error {
	e.shut.record("metric")
	return nil
}

type fakeLogExporter struct{ shut *shutdowns }

func (fakeLogExporter) Export(context.Context, []sdklog.Record) error { return nil }
func (fakeLogExporter) ForceFlush(context.Context) error              { return nil }
func (e fakeLogExporter) Shutdown(context.Context) error {
	e.shut.record("log")
	return nil
}

var errExporter = errors.New("the exporter couldn't be built")

// fakeExporters builds recording exporters, failing the one named failing.
func fakeExporters(shut *shutdowns, failing string) exporters {
	return exporters{
		trace: func(context.Context, string) (sdktrace.SpanExporter, error) {
			if failing == "trace" {
				return nil, errExporter
			}
			return fakeSpanExporter{shut}, nil
		},
		metric: func(context.Context, string) (sdkmetric.Exporter, error) {
			if failing == "metric" {
				return nil, errExporter
			}
			return fakeMetricExporter{shut}, nil
		},
		log: func(context.Context, string) (sdklog.Exporter, error) {
			if failing == "log" {
				return nil, errExporter
			}
			return fakeLogExporter{shut}, nil
		},
	}
}

// globals are OpenTelemetry's global providers, propagator and error
// handler.
type globals struct {
	tracer     trace.TracerProvider
	meter      metric.MeterProvider
	logger     otellog.LoggerProvider
	propagator propagation.TextMapPropagator
	handler    otel.ErrorHandler
}

func currentGlobals() globals {
	return globals{
		tracer:     otel.GetTracerProvider(),
		meter:      otel.GetMeterProvider(),
		logger:     otel.GetLoggerProvider(),
		propagator: otel.GetTextMapPropagator(),
		handler:    otel.GetErrorHandler(),
	}
}

// same reports whether a and b are the same value: equal when their type is
// comparable, else (a propagator is a slice) the same backing array.
func same(a, b any) bool {
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ta != tb {
		return false
	}
	if ta == nil || ta.Comparable() {
		return a == b
	}
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

// changed names every global that differs between a and b.
func changed(a, b globals) []string {
	var names []string
	for _, pair := range []struct {
		name string
		a, b any
	}{
		{"tracer provider", a.tracer, b.tracer},
		{"meter provider", a.meter, b.meter},
		{"logger provider", a.logger, b.logger},
		{"propagator", a.propagator, b.propagator},
		{"error handler", a.handler, b.handler},
	} {
		if !same(pair.a, pair.b) {
			names = append(names, pair.name)
		}
	}
	return names
}

// keepGlobals puts back, when the test ends, every global it changed, so a
// test's providers don't leak into the next.
func keepGlobals(t *testing.T) globals {
	t.Helper()
	before := currentGlobals()
	t.Cleanup(func() {
		now := currentGlobals()
		if !same(now.tracer, before.tracer) {
			otel.SetTracerProvider(before.tracer)
		}
		if !same(now.meter, before.meter) {
			otel.SetMeterProvider(before.meter)
		}
		if !same(now.logger, before.logger) {
			otel.SetLoggerProvider(before.logger)
		}
		if !same(now.propagator, before.propagator) {
			otel.SetTextMapPropagator(before.propagator)
		}
		if !same(now.handler, before.handler) {
			otel.SetErrorHandler(before.handler)
		}
		// The SDK's logger can't be read back, and isn't put back: one
		// writing to an ended test's buffer is harmless, and a test that
		// replaced it with a sink of its own would hide, from the tests
		// after it, a Setup that no longer quiets the SDK.
	})
	return before
}

func TestSetupIsANoOpWithoutAnEndpoint(t *testing.T) {
	before := keepGlobals(t)
	var log syncBuffer
	shut := &shutdowns{}
	tel, err := setup(context.Background(), envOf(nil), slog.New(slog.NewJSONHandler(&log, nil)), fakeExporters(shut, ""))
	if err != nil || tel == nil {
		t.Fatalf("setup() = %v, %v; want a no-op Telemetry", tel, err)
	}
	if names := changed(before, currentGlobals()); len(names) != 0 {
		t.Fatalf("setup without an endpoint changed the globals: %v", names)
	}
	if log.String() != "" {
		t.Fatalf("setup without an endpoint logged %s", log.String())
	}
}

// A failure part way leaves no exporter running and the providers and the
// propagator as they were, and nothing logged quotes the endpoint. The
// error handler is the quiet one Setup sets before it builds anything.
func TestSetupShutsDownWhatItBuiltAndLeavesTheGlobalsOnAFailure(t *testing.T) {
	for failing, wantShut := range map[string][]string{
		"trace":  nil,
		"metric": {"trace"},
		"log":    {"metric", "trace"},
	} {
		t.Run(failing, func(t *testing.T) {
			before := keepGlobals(t)
			var log syncBuffer
			shut := &shutdowns{}
			env := envOf(map[string]string{endpointVariable: endpointWithPassword})
			tel, err := setup(context.Background(), env, slog.New(slog.NewJSONHandler(&log, nil)), fakeExporters(shut, failing))
			if !errors.Is(err, errExporter) || tel != nil {
				t.Fatalf("setup() = %v, %v; want the exporter's error", tel, err)
			}
			if got := shut.list(); strings.Join(got, ",") != strings.Join(wantShut, ",") {
				t.Fatalf("shut down %v, want %v", got, wantShut)
			}
			if names := changed(before, currentGlobals()); len(names) != 1 || names[0] != "error handler" {
				t.Fatalf("a failed setup changed %v, want only the error handler", names)
			}
			if strings.Contains(log.String(), "collector-password-7e1") {
				t.Fatalf("the log quotes the endpoint: %s", log.String())
			}
		})
	}
}

func TestSetupSetsTheGlobalsOnlyOnceEverythingIsBuilt(t *testing.T) {
	before := keepGlobals(t)
	var log syncBuffer
	shut := &shutdowns{}
	env := envOf(map[string]string{endpointVariable: endpointWithPassword})
	tel, err := setup(context.Background(), env, slog.New(slog.NewJSONHandler(&log, nil)), fakeExporters(shut, ""))
	if err != nil {
		t.Fatal(err)
	}
	after := currentGlobals()
	if names := changed(before, after); len(names) != 5 {
		t.Errorf("setup changed only %v, want every global", names)
	}
	if _, ok := after.tracer.(*sdktrace.TracerProvider); !ok {
		t.Errorf("the tracer provider is %T, want the SDK's", after.tracer)
	}
	if _, ok := after.meter.(*sdkmetric.MeterProvider); !ok {
		t.Errorf("the meter provider is %T, want the SDK's", after.meter)
	}
	if _, ok := after.logger.(*sdklog.LoggerProvider); !ok {
		t.Errorf("the logger provider is %T, want the SDK's", after.logger)
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := shut.list(); strings.Join(got, ",") != "log,metric,trace" {
		t.Fatalf("Shutdown shut down %v, want every exporter, last built first", got)
	}
	logged := log.String()
	if !strings.Contains(logged, `"msg":"telemetry: OpenTelemetry initialized"`) ||
		!strings.Contains(logged, `"service":"telflo-opamp-server"`) {
		t.Fatalf("setup logged %s, want its JSON line with the service name", logged)
	}
	if strings.Contains(logged, "collector-password-7e1") || strings.Contains(logged, "collector.invalid") {
		t.Fatalf("the log quotes the endpoint: %s", logged)
	}
}

// The service name is a resource attribute: OTEL_SERVICE_NAME, else a
// service.name in OTEL_RESOURCE_ATTRIBUTES, else the default. Both are read
// through getenv, and the process's environment is never written.
func TestTheServiceNameIsAResourceAttributeAndTheEnvironmentIsUntouched(t *testing.T) {
	t.Setenv(serviceNameVariable, "")
	if err := os.Unsetenv(serviceNameVariable); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"the default", nil, defaultServiceName},
		{"OTEL_SERVICE_NAME", map[string]string{serviceNameVariable: "  named  "}, "named"},
		{"service.name in the attributes", map[string]string{resourceAttributesVariable: "service.namespace=x, service.name=from%20attributes"}, "from attributes"},
		{"OTEL_SERVICE_NAME over the attributes", map[string]string{
			serviceNameVariable: "named", resourceAttributesVariable: "service.name=from-attributes",
		}, "named"},
		{"service.namespace isn't service.name", map[string]string{resourceAttributesVariable: "service.namespace=team"}, defaultServiceName},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var log syncBuffer
			res, err := newResource(context.Background(), envOf(c.env), slog.New(slog.NewJSONHandler(&log, nil)))
			if err != nil {
				t.Fatal(err)
			}
			got, _ := res.Set().Value("service.name")
			if got.AsString() != c.want {
				t.Fatalf("service.name = %q, want %q", got.AsString(), c.want)
			}
			if _, set := os.LookupEnv(serviceNameVariable); set {
				t.Fatal("newResource wrote OTEL_SERVICE_NAME into the environment")
			}
		})
	}
}

func TestResourceAttributesSkipAMalformedEntryWithoutQuotingIt(t *testing.T) {
	var log syncBuffer
	attrs := resourceAttributes("a=1,secret-marker-41c, b = two ,=x", slog.New(slog.NewJSONHandler(&log, nil)))
	if len(attrs) != 2 || attrs[0].Value.AsString() != "1" || string(attrs[1].Key) != "b" || attrs[1].Value.AsString() != "two" {
		t.Fatalf("attributes = %v, want a=1 and b=two", attrs)
	}
	if strings.Count(log.String(), "isn't key=value") != 2 || strings.Contains(log.String(), "secret-marker-41c") {
		t.Fatalf("log = %s, want two warnings quoting nothing", log.String())
	}
}

func TestTheLegacyAddressIsMigratedWithAWarningThatDoesNotQuoteIt(t *testing.T) {
	var log syncBuffer
	logger := slog.New(slog.NewJSONHandler(&log, nil))
	if got := resolveEndpoint(envOf(map[string]string{legacyAddressVariable: "collector-host-9d2:4317"}), logger); got != "http://collector-host-9d2:4317" {
		t.Fatalf("resolveEndpoint() = %q", got)
	}
	if !strings.Contains(log.String(), "deprecated") || strings.Contains(log.String(), "collector-host-9d2") {
		t.Fatalf("log = %s, want a warning that doesn't quote the address", log.String())
	}
	if got := resolveEndpoint(envOf(map[string]string{
		endpointVariable: "https://standard:4317", legacyAddressVariable: "legacy:4317",
	}), logger); got != "https://standard:4317" {
		t.Fatalf("resolveEndpoint() = %q, want the standard variable", got)
	}
}

// captureStderr runs fn with file descriptor 2 pointed at a file, and
// returns what was written there. The descriptor, not the os.Stderr
// variable: the SDK's default logger and the log package took os.Stderr
// when the program started.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	saved, err := syscall.Dup(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := dupOnto(int(file.Fd()), 2); err != nil {
		_ = syscall.Close(saved)
		if errors.Is(err, errors.ErrUnsupported) {
			t.Skip("stderr can't be captured on this platform")
		}
		t.Fatal(err)
	}
	func() {
		defer func() {
			if err := dupOnto(saved, 2); err != nil {
				panic(err)
			}
			_ = syscall.Close(saved)
		}()
		fn()
	}()
	written, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(written)
}

// endpointThatIsNotAURL has a password and an invalid escape: url.Parse
// fails on it, and its error, like the SDK's own log line, quotes it.
const endpointThatIsNotAURL = "http://user:SECRETPW@collector.invalid:4317/%zz"

// Unguarded, the SDK's own logger (stdr, on stderr) prints a malformed
// OTEL_EXPORTER_OTLP_ENDPOINT whole, password included, and the exporters
// send to localhost:4317. Neither stderr nor the JSON log may quote it,
// whether Setup reads it or an exporter reads the process's environment.
func TestAnEndpointThatIsNotAURLIsQuotedNowhere(t *testing.T) {
	t.Run("Setup reads it: telemetry is off", func(t *testing.T) {
		before := keepGlobals(t)
		t.Setenv(endpointVariable, endpointThatIsNotAURL)
		var log syncBuffer
		var tel *Telemetry
		var err error
		stderr := captureStderr(t, func() {
			tel, err = Setup(context.Background(), os.Getenv, slog.New(slog.NewJSONHandler(&log, nil)))
		})
		if err != nil || tel == nil {
			t.Fatalf("Setup() = %v, %v; want a no-op Telemetry", tel, err)
		}
		if names := changed(before, currentGlobals()); len(names) != 0 {
			t.Fatalf("telemetry isn't off: Setup changed %v", names)
		}
		if !strings.Contains(log.String(), "OTEL_EXPORTER_OTLP_ENDPOINT isn't a URL; telemetry is off") {
			t.Fatalf("the log doesn't say telemetry is off: %s", log.String())
		}
		for where, text := range map[string]string{"stderr": stderr, "the log": log.String()} {
			if strings.Contains(text, "SECRETPW") {
				t.Fatalf("%s quotes the endpoint: %s", where, text)
			}
		}
	})

	t.Run("an exporter reads it from the process's environment", func(t *testing.T) {
		keepGlobals(t)
		t.Setenv(endpointVariable, endpointThatIsNotAURL)
		// Setup's own endpoint is valid (nothing listens there): the
		// exporters still parse the process's variable, and fail.
		env := envOf(map[string]string{endpointVariable: "http://127.0.0.1:1"})
		var log syncBuffer
		stderr := captureStderr(t, func() {
			tel, err := Setup(context.Background(), env, slog.New(slog.NewJSONHandler(&log, nil)))
			if err != nil {
				t.Errorf("Setup() = %v", err)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			_ = tel.Shutdown(ctx)
		})
		for where, text := range map[string]string{"stderr": stderr, "the log": log.String()} {
			if strings.Contains(text, "SECRETPW") {
				t.Fatalf("%s quotes the endpoint: %s", where, text)
			}
		}
		// The exporter's complaint arrived, by its fixed message alone.
		if !strings.Contains(log.String(), `"otel_message":"parse url"`) {
			t.Fatalf("the SDK's parse error didn't reach the log by its message: %s", log.String())
		}
	})
}

func TestTheSDKsLoggerKeepsAnErrorsMessageOnly(t *testing.T) {
	var log syncBuffer
	logger := logr.New(messageOnlySink{logger: slog.New(slog.NewJSONHandler(&log, nil))}).
		WithName("SECRETPW-name").WithValues("k", "SECRETPW-value")
	logger.Info("SECRETPW-info")
	logger.V(1).Info("SECRETPW-warning")
	logger.Error(errors.New("SECRETPW-error"), "parse url", "input", "SECRETPW-input")
	logged := log.String()
	if strings.Count(logged, "\n") != 1 || !strings.Contains(logged, `"otel_message":"parse url"`) {
		t.Fatalf("log = %s, want one line with the error's message", logged)
	}
	if strings.Contains(logged, "SECRETPW") {
		t.Fatalf("the log quotes what the SDK passed: %s", logged)
	}
}

// An endpoint is a URL with a scheme and a host, or there is no telemetry;
// the warning never quotes it.
func TestAnEndpointWithoutASchemeAndAHostTurnsTelemetryOff(t *testing.T) {
	for _, c := range []struct {
		variable, value, want string
	}{
		{endpointVariable, "http://collector:4317", "http://collector:4317"},
		{endpointVariable, "https://user:pw@collector:4317", "https://user:pw@collector:4317"},
		{endpointVariable, "collector-SECRETPW:4317", ""}, // a scheme, no host
		{endpointVariable, "/SECRETPW", ""},
		{endpointVariable, "http://", ""},
		{endpointVariable, endpointThatIsNotAURL, ""},
		{legacyAddressVariable, "collector:4317", "http://collector:4317"},
		{legacyAddressVariable, "collector-SECRETPW:43%zz", ""},
		{legacyAddressVariable, "http://", ""},
	} {
		var log syncBuffer
		got := resolveEndpoint(envOf(map[string]string{c.variable: c.value}), slog.New(slog.NewJSONHandler(&log, nil)))
		if got != c.want {
			t.Errorf("%s=%s: resolveEndpoint() = %q, want %q", c.variable, c.value, got, c.want)
		}
		if c.want == "" && !strings.Contains(log.String(), "telemetry is off") {
			t.Errorf("%s: the log doesn't say telemetry is off: %s", c.variable, log.String())
		}
		if strings.Contains(log.String(), "SECRETPW") || strings.Contains(log.String(), "pw@") {
			t.Errorf("%s: the log quotes the value: %s", c.variable, log.String())
		}
	}
}
