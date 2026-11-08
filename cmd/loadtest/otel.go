package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// Phase is the load-test phase the fleet is in. The two phases have very
// different server cost profiles: registration is the default-token to
// rotated-token handoff plus the first full-state poll (config resolution,
// instance row insert, token rotation); steady is the compact heartbeat every
// poll interval. The report slices every server-side metric by these windows,
// so the tool (the only party that knows when the last client finished its
// handshake) publishes the boundary itself.
type Phase int32

const (
	PhaseRegistration Phase = iota
	PhaseSteady
)

func (p Phase) String() string {
	switch p {
	case PhaseRegistration:
		return "registration"
	case PhaseSteady:
		return "steady"
	default:
		return "unknown"
	}
}

// Telemetry exports the tool's own view of a run over OTLP so it lands in the
// same ClickHouse as the server's metrics: offered load (polls, rps, status
// mix, client-side latency), the active client count, and the phase markers.
// All methods are nil-safe: with no endpoint configured, main keeps a nil
// *Telemetry and every hook is a no-op.
type Telemetry struct {
	runID    string
	count    int
	launched atomic.Int64
	phase    atomic.Int32

	polls     metric.Int64Counter
	pollDur   metric.Float64Histogram
	rotations metric.Int64Counter
	configs   metric.Int64Counter

	logger   otellog.Logger
	shutdown func(context.Context) error
}

// telemetryConfig is the subset of the CLI config the exporter needs.
type telemetryConfig struct {
	endpoint       string
	runID          string
	count          int
	pollInterval   time.Duration
	deployInterval time.Duration
	target         string // the OpAMP endpoint under test (host only is kept)
	groupID        string
}

// latencyBucketsMs are the explicit histogram bounds for loadtest.poll.duration.
// Chosen for an HTTP poll that is ~10ms on a healthy server and ~30s at the
// client timeout, so p50/p95/p99 interpolate cleanly across the whole range.
var latencyBucketsMs = []float64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000}

// newTelemetry builds the OTLP exporters. Returns (nil, nil) when no endpoint
// is configured (the no-op gate) so callers can pass the nil pointer around.
func newTelemetry(ctx context.Context, tc telemetryConfig) (*Telemetry, error) {
	endpoint := strings.TrimSpace(tc.endpoint)
	if endpoint == "" {
		return nil, nil
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "http://" + endpoint
	}

	res, err := resource.New(ctx,
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			attribute.String("service.name", "telflo-loadtest"),
			attribute.String("service.namespace", "telflo"),
			attribute.String("loadtest.run_id", tc.runID),
			attribute.Int("loadtest.count", tc.count),
			attribute.String("loadtest.poll_interval", tc.pollInterval.String()),
			attribute.String("loadtest.deploy_interval", tc.deployInterval.String()),
			attribute.String("loadtest.target", targetHost(tc.target)),
			// Always true since --collector_group_id is required; kept for
			// the reports that slice by it.
			attribute.Bool("loadtest.config_delivery", tc.groupID != ""),
		),
	)
	if err != nil && res == nil {
		return nil, err
	}

	t := &Telemetry{runID: tc.runID, count: tc.count}
	var shutdowns []func(context.Context) error

	// --- Metrics ---
	metricExp, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpointURL(endpoint))
	if err != nil {
		return nil, fmt.Errorf("otlp metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		// 5s so a short registration window still has several points.
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp, sdkmetric.WithInterval(5*time.Second))),
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Name: "loadtest.poll.duration"},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{Boundaries: latencyBucketsMs}},
		)),
	)
	shutdowns = append(shutdowns, mp.Shutdown)
	meter := mp.Meter("telflo-loadtest")

	if t.polls, err = meter.Int64Counter("loadtest.polls",
		metric.WithDescription("OpAMP polls completed, by outcome and phase"),
		metric.WithUnit("{poll}")); err != nil {
		return nil, err
	}
	if t.pollDur, err = meter.Float64Histogram("loadtest.poll.duration",
		metric.WithDescription("Client-side round-trip of one OpAMP poll"),
		metric.WithUnit("ms")); err != nil {
		return nil, err
	}
	if t.rotations, err = meter.Int64Counter("loadtest.rotations",
		metric.WithDescription("Rotated tokens adopted (default-to-rotated handoffs completed)"),
		metric.WithUnit("{token}")); err != nil {
		return nil, err
	}
	if t.configs, err = meter.Int64Counter("loadtest.configs.delivered",
		metric.WithDescription("Remote configs delivered by the server and stored by a client"),
		metric.WithUnit("{config}")); err != nil {
		return nil, err
	}
	if _, err = meter.Int64ObservableGauge("loadtest.clients.active",
		metric.WithDescription("Virtual supervisors launched and polling"),
		metric.WithUnit("{client}"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(t.launched.Load())
			return nil
		})); err != nil {
		return nil, err
	}
	// loadtest.phase is 1 for the current phase and 0 for the other, keyed by
	// the phase attribute, so a chart can shade the phase bands; the index form
	// is a single step series for tools that cannot group by attribute.
	if _, err = meter.Int64ObservableGauge("loadtest.phase",
		metric.WithDescription("1 for the phase currently in progress, 0 otherwise"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			cur := Phase(t.phase.Load())
			for _, p := range []Phase{PhaseRegistration, PhaseSteady} {
				v := int64(0)
				if p == cur {
					v = 1
				}
				o.Observe(v, metric.WithAttributes(attribute.String("phase", p.String())))
			}
			return nil
		})); err != nil {
		return nil, err
	}
	if _, err = meter.Int64ObservableGauge("loadtest.phase.index",
		metric.WithDescription("0 = registration, 1 = steady"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(t.phase.Load()))
			return nil
		})); err != nil {
		return nil, err
	}

	// --- Logs (phase markers) ---
	logExp, err := otlploggrpc.New(ctx, otlploggrpc.WithEndpointURL(endpoint))
	if err != nil {
		return nil, fmt.Errorf("otlp log exporter: %w", err)
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
	)
	shutdowns = append(shutdowns, lp.Shutdown)
	t.logger = lp.Logger("telflo-loadtest")

	t.shutdown = func(ctx context.Context) error {
		var errs []error
		for i := len(shutdowns) - 1; i >= 0; i-- {
			if e := shutdowns[i](ctx); e != nil {
				errs = append(errs, e)
			}
		}
		return errors.Join(errs...)
	}
	log.Printf("telemetry: exporting run_id=%s to %s", tc.runID, endpoint)
	return t, nil
}

// Shutdown flushes the exporters. Nil-safe.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t == nil || t.shutdown == nil {
		return nil
	}
	return t.shutdown(ctx)
}

// CurrentPhase returns the phase in progress (registration until the tracker
// flips it). Nil-safe: a disabled telemetry is always "registration".
func (t *Telemetry) CurrentPhase() Phase {
	if t == nil {
		return PhaseRegistration
	}
	return Phase(t.phase.Load())
}

func (t *Telemetry) clientLaunched() {
	if t != nil {
		t.launched.Add(1)
	}
}

// onPoll records one completed HTTP round-trip. outcome is one of
// 2xx / 401 / 429/ratelimit / 429/shed / 429 (unattributed) / other.
func (t *Telemetry) onPoll(outcome string, latency time.Duration) {
	if t == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("outcome", outcome),
		attribute.String("phase", t.CurrentPhase().String()),
	)
	ctx := context.Background()
	t.polls.Add(ctx, 1, attrs)
	t.pollDur.Record(ctx, float64(latency)/float64(time.Millisecond), attrs)
}

// onTransportError records a poll that never got a response; kind is the
// classifyTransportError bucket name (refused/reset/timeout/other).
func (t *Telemetry) onTransportError(kind string) {
	if t == nil {
		return
	}
	t.polls.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("outcome", "error"),
		attribute.String("error.kind", kind),
		attribute.String("phase", t.CurrentPhase().String()),
	))
}

func (t *Telemetry) onRotation() {
	if t != nil {
		t.rotations.Add(context.Background(), 1)
	}
}

func (t *Telemetry) onConfigDelivered() {
	if t != nil {
		t.configs.Add(context.Background(), 1)
	}
}

// event emits one phase-marker log record. The report reads these back from
// otel_logs (LogAttributes['loadtest.event']) to build the per-phase windows.
func (t *Telemetry) event(name string, attrs ...attribute.KeyValue) {
	if t == nil {
		return
	}
	var r otellog.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(otellog.SeverityInfo)
	r.SetSeverityText("INFO")
	r.SetBody(attribute.StringValue("loadtest " + name))
	r.AddAttributes(
		attribute.String("loadtest.event", name),
		attribute.String("loadtest.run_id", t.runID),
		attribute.Int64("loadtest.count", int64(t.count)),
	)
	r.AddAttributes(attrs...)
	t.logger.Emit(context.Background(), r)
}

// runStart marks t0 of the run. Called once the launch loop begins.
func (t *Telemetry) runStart() {
	t.event("run.start", attribute.String("phase", PhaseRegistration.String()))
	if t != nil {
		logAlways("phase: registration started (run_id=%s)", t.runID)
	} else {
		logAlways("phase: registration started")
	}
}

// enterSteady flips the phase to steady and publishes the boundary with the
// counters that justified it.
func (t *Telemetry) enterSteady(reason string, s Snapshot, elapsed time.Duration) {
	if t != nil {
		t.phase.Store(int32(PhaseSteady))
		t.event("phase.start",
			attribute.String("phase", PhaseSteady.String()),
			attribute.String("reason", reason),
			attribute.Float64("elapsed_seconds", elapsed.Seconds()),
			attribute.Int64("rotations", int64(s.Rotations)),
			attribute.Int64("configs", int64(s.Configs)),
			attribute.Int64("polls", int64(s.Total)),
		)
	}
	logAlways("phase: steady state reached after %s (%s; rot=%d cfg=%d polls=%d)",
		elapsed.Round(time.Second), reason, s.Rotations, s.Configs, s.Total)
}

// runEnd publishes the final counters and the end-of-run marker.
func (t *Telemetry) runEnd(s Snapshot, elapsed time.Duration) {
	t.event("run.end",
		attribute.String("phase", t.CurrentPhase().String()),
		attribute.Float64("elapsed_seconds", elapsed.Seconds()),
		attribute.Int64("polls", int64(s.Total)),
		attribute.Int64("status_2xx", int64(s.Status2xx)),
		attribute.Int64("status_401", int64(s.Status401)),
		attribute.Int64("status_429", int64(s.Status429)),
		attribute.Int64("status_429_ratelimit", int64(s.Status429RateLimit)),
		attribute.Int64("status_429_shed", int64(s.Status429Shed)),
		attribute.Int64("status_other", int64(s.StatusOther)),
		attribute.Int64("transport_errors", int64(s.Transport)),
		attribute.Int64("rotations", int64(s.Rotations)),
		attribute.Int64("configs", int64(s.Configs)),
	)
}

// registrationDone decides whether the fleet has finished its handshake.
// Primary signal: every client adopted a rotated token and (when config
// delivery is enabled) stored a config. Fallback: the deadline passed. The
// fleet is on steady cadence by then even if some polls were shed with 429, so
// the phase flips anyway and the reason records the shortfall.
func registrationDone(s Snapshot, count int, configDelivery bool, elapsed, deadline time.Duration) (bool, string) {
	c := uint64(count)
	if s.Rotations >= c && (!configDelivery || s.Configs >= c) {
		return true, "all clients registered"
	}
	if elapsed >= deadline {
		return true, fmt.Sprintf("deadline %s passed with rot=%d/%d cfg=%d/%d", deadline.Round(time.Second), s.Rotations, c, s.Configs, c)
	}
	return false, ""
}

// registrationDeadline bounds the registration phase: the deploy window (first
// polls) plus two jittered poll intervals (the rotated full-state poll and one
// more for stragglers) plus a small fixed grace.
func registrationDeadline(deployInterval, pollInterval time.Duration, jitter float64) time.Duration {
	return deployInterval + time.Duration(2*float64(pollInterval)*(1+jitter)) + 5*time.Second
}

// runPhaseTracker polls the collector once a second and flips the run to the
// steady phase the moment registrationDone says so. Runs until the phase flips
// or ctx is cancelled. Works with a nil *Telemetry too (still prints the line).
func runPhaseTracker(ctx context.Context, t *Telemetry, m *Collector, count int, configDelivery bool, deadline time.Duration, start time.Time) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s := m.Peek()
			elapsed := time.Since(start)
			if done, reason := registrationDone(s, count, configDelivery, elapsed, deadline); done {
				t.enterSteady(reason, s, elapsed)
				return
			}
		}
	}
}

// defaultRunID names a run by its size and start time, e.g. opamp-50-20260902-143000.
func defaultRunID(count int, now time.Time) string {
	return fmt.Sprintf("opamp-%d-%s", count, now.UTC().Format("20060102-150405"))
}

// targetHost reduces the endpoint URL to its host for the resource attribute
// (no path, no scheme, no query).
func targetHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return endpoint
	}
	return u.Host
}

// envOr returns the env var value or the fallback when unset/empty.
func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// logAlways prints regardless of --verbose: phase transitions are the one piece
// of non-error output an operator always wants to see on the console.
func logAlways(format string, args ...any) {
	log.Printf(format, args...)
}
