// Command loadtest impersonates many OpAMP supervisor clients against the Telflo
// OpAMP server to measure how much polling load the current server
// configuration can absorb.
//
// It is a pure client: each virtual supervisor authenticates with a fleet's
// default token (which already encodes {token_id, deployment_id}: the fleet's
// id, under the key deployment_id), so the tool needs no encryption key and no
// database access. See cmd/loadtest/README.md.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

type config struct {
	endpoint        string
	tokens          string
	tokensFile      string
	count           int
	deployInterval  time.Duration
	pollInterval    time.Duration
	jitter          float64
	namePrefix      string
	collectorGroup  string
	ramp            time.Duration
	duration        time.Duration
	metricsInterval time.Duration
	maxInflight     int
	insecure        bool
	sharedTransport bool
	verbose         bool
	pollTiming      bool
	otlpEndpoint    string
	runID           string
}

func parseFlags() config {
	var c config
	flag.StringVar(&c.endpoint, "endpoint", "", "OpAMP server URL, e.g. http://localhost:51234/opamp/api/v1 (required)")
	flag.StringVar(&c.tokens, "tokens", "", "comma-separated fleet default token(s)")
	flag.StringVar(&c.tokensFile, "tokens-file", "", "path to a file of fleet tokens, one per line (# comments allowed)")
	flag.IntVar(&c.count, "count", 1, "number of virtual supervisors")
	flag.DurationVar(&c.pollInterval, "poll_interval", 30*time.Second, "steady-state cadence: base delay between a client's polls after it has deployed")
	flag.DurationVar(&c.deployInterval, "deploy_interval", 0, "window over which the first poll of all --count clients is spread, i.e. how long the fleet takes to fully deploy (0 = same as --poll_interval)")
	flag.Float64Var(&c.jitter, "jitter", 0.1, "±fraction of jitter applied to each poll interval")
	flag.StringVar(&c.namePrefix, "name-prefix", "col-", "prefix for generated ServerName values")
	flag.StringVar(&c.collectorGroup, "collector_group_id", "", "collector_group_id each client reports (telflo.collector_group_id), a group of the tokens' fleet (required: the server refuses a collector naming no group); with a published config on it, the config-delivery path runs")
	flag.DurationVar(&c.ramp, "ramp", 0, "spread client startup over this window (0 = launch all at once)")
	flag.DurationVar(&c.duration, "duration", 0, "total run time (0 = run until SIGINT)")
	flag.DurationVar(&c.metricsInterval, "metrics-interval", 10*time.Second, "summary print cadence")
	flag.IntVar(&c.maxInflight, "max-inflight", 0, "cap on polls in flight at once across all clients (0 = unlimited); a client-side throttle on offered concurrency, useful for finding the server's ceiling")
	flag.BoolVar(&c.insecure, "insecure", false, "skip TLS certificate verification")
	flag.BoolVar(&c.sharedTransport, "shared-transport", false, "share one HTTP transport across all clients (default: one per client)")
	flag.BoolVar(&c.verbose, "verbose", false, "print all non-error output (startup banner, metrics summaries, per-poll lifecycle); errors and non-2xx responses always print")
	flag.BoolVar(&c.pollTiming, "poll-timing", false, "print a timing line for every poll: when it started (elapsed since launch) and how long it took (server round-trip)")
	flag.StringVar(&c.otlpEndpoint, "otlp-endpoint", envOr("OTEL_EXPORTER_OTLP_ENDPOINT", ""), "OTLP gRPC endpoint to export the tool's own metrics + phase markers to (e.g. http://localhost:4317); empty = no export. Defaults to $OTEL_EXPORTER_OTLP_ENDPOINT")
	flag.StringVar(&c.runID, "run-id", "", "label stamped on every exported metric/log as loadtest.run_id (default opamp-<count>-<UTC timestamp>)")
	flag.Parse()
	// deploy_interval defaults to poll_interval: unset, the fleet's first polls
	// spread over one poll interval — exactly what the old single --interval did.
	if c.deployInterval <= 0 {
		c.deployInterval = c.pollInterval
	}
	return c
}

// configProblem is what makes cfg unusable, "" for nothing: an endpoint and
// a collector group are required (the server refuses, 403, a collector
// whose description names no group), and a count of at least one.
func configProblem(cfg config) string {
	switch {
	case cfg.endpoint == "":
		return "--endpoint is required"
	case strings.TrimSpace(cfg.collectorGroup) == "":
		return "--collector_group_id is required (the server refuses a collector naming no group)"
	case cfg.count < 1:
		return "--count must be >= 1"
	}
	return ""
}

func main() {
	cfg := parseFlags()
	if problem := configProblem(cfg); problem != "" {
		fmt.Fprintln(os.Stderr, "error: "+problem)
		flag.Usage()
		os.Exit(2)
	}
	verbose = cfg.verbose
	pollTiming = cfg.pollTiming

	tokens, err := loadTokens(cfg.tokens, cfg.tokensFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}

	if cfg.runID == "" {
		cfg.runID = defaultRunID(cfg.count, time.Now())
	}

	// Shutdown on SIGINT/SIGTERM, or after --duration if set.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Optional OTLP export of the tool's own view (offered load, latency, phase
	// markers). nil when --otlp-endpoint is unset; every hook is nil-safe.
	tel, err := newTelemetry(ctx, telemetryConfig{
		endpoint:       cfg.otlpEndpoint,
		runID:          cfg.runID,
		count:          cfg.count,
		pollInterval:   cfg.pollInterval,
		deployInterval: cfg.deployInterval,
		target:         cfg.endpoint,
		groupID:        cfg.collectorGroup,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: telemetry: %v\n", err)
		os.Exit(2)
	}
	metrics := &Collector{tel: tel}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-sig:
			logInfo("shutdown signal received, stopping clients...")
		case <-ctx.Done():
		}
		cancel()
	}()
	if cfg.duration > 0 {
		go func() {
			select {
			case <-time.After(cfg.duration):
				logInfo("duration %s elapsed, stopping clients...", cfg.duration)
				cancel()
			case <-ctx.Done():
			}
		}()
	}

	logInfo("starting %d virtual supervisors → %s | poll_interval=%s deploy_interval=%s tokens=%d transport=%s group=%s max-inflight=%s",
		cfg.count, cfg.endpoint, cfg.pollInterval, cfg.deployInterval, len(tokens), transportMode(cfg.sharedTransport), cfg.collectorGroup, inflightLabel(cfg.maxInflight))

	var sharedClient *http.Client
	if cfg.sharedTransport {
		sharedClient = newHTTPClient(cfg, cfg.count)
	}

	// Shared across all clients so the cap is global, not per-client. nil when
	// --max-inflight is 0 (unlimited), which every VirtualSupervisor treats as
	// "no limiter".
	var sem chan struct{}
	if cfg.maxInflight > 0 {
		sem = make(chan struct{}, cfg.maxInflight)
	}

	width := nameWidth(cfg.count)
	runStart = time.Now() // t0 for --first-poll-timing (start of the launch loop)
	tel.runStart()

	// Phase tracker: flips registration → steady once every client has
	// completed its handshake (or the deadline passes), publishing the boundary
	// the report slices server-side metrics by.
	trackerDone := make(chan struct{})
	go func() {
		defer close(trackerDone)
		// Every client names its group (required), so registration waits
		// for each one's delivered config; a group with none published
		// leaves the deadline to end it.
		runPhaseTracker(ctx, tel, metrics, cfg.count, true,
			registrationDeadline(cfg.deployInterval, cfg.pollInterval, cfg.jitter), runStart)
	}()

	var wg sync.WaitGroup
	for i := 0; i < cfg.count; i++ {
		client := sharedClient
		if client == nil {
			client = newHTTPClient(cfg, 1)
		}
		name := fmt.Sprintf("%s%0*d", cfg.namePrefix, width, i+1)
		vs := &VirtualSupervisor{
			name:           name,
			token:          tokenFor(tokens, i),
			instanceUID:    newInstanceUID(),
			hostName:       name,
			hostIP:         hostIPForIndex(i),
			groupID:        cfg.collectorGroup,
			endpoint:       cfg.endpoint,
			deployInterval: cfg.deployInterval,
			pollInterval:   cfg.pollInterval,
			jitter:         cfg.jitter,
			client:         client,
			metrics:        metrics,
			rng:            rand.New(rand.NewSource(int64(i)*2862933555777941757 + 3037000493)),
			sem:            sem,
		}

		if err := rampWait(ctx, cfg.ramp, i, cfg.count); err != nil {
			break // context cancelled during ramp
		}

		wg.Add(1)
		tel.clientLaunched()
		go func() {
			defer wg.Done()
			vs.Run(ctx)
		}()
	}

	// Metrics printer runs until context cancellation, then a final summary.
	printerDone := make(chan struct{})
	go func() {
		defer close(printerDone)
		runMetricsPrinter(ctx, metrics, cfg.metricsInterval, cfg.count)
	}()

	wg.Wait()
	cancel()
	<-printerDone
	<-trackerDone

	final := printFinal(metrics, cfg.count)
	tel.runEnd(final, time.Since(runStart))
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer flushCancel()
	if err := tel.Shutdown(flushCtx); err != nil {
		logError("telemetry: flush failed: %v", err)
	}
}

// runMetricsPrinter emits a windowed summary every interval until ctx is done.
func runMetricsPrinter(ctx context.Context, m *Collector, interval time.Duration, active int) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s := m.Snapshot()
			elapsed := now.Sub(last).Seconds()
			last = now
			rps := 0.0
			if elapsed > 0 {
				rps = float64(s.WindowCount) / elapsed
			}
			logInfo("%s", s.Line(rps, active))
		}
	}
}

// printFinal drains any remaining window, prints the cumulative totals, and
// returns them for the end-of-run marker.
func printFinal(m *Collector, active int) Snapshot {
	s := m.Snapshot()
	logInfo("FINAL | %s", s.Line(0, active))
	return s
}

// newHTTPClient builds an HTTP client. connsPerHost caps idle keep-alive
// connections: 1 for a per-client transport (each virtual supervisor holds its
// own connection, closer to N real supervisors), higher for the shared case.
func newHTTPClient(cfg config, connsPerHost int) *http.Client {
	transport := &http.Transport{
		MaxIdleConns:        connsPerHost,
		MaxIdleConnsPerHost: connsPerHost,
		IdleConnTimeout:     90 * time.Second,
	}
	if cfg.insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}
}

// rampWait spaces client launch i of total over the ramp window. With ramp=0 it
// returns immediately (all clients launch at once; Run still phase-spreads the
// first poll). Returns ctx.Err() if cancelled mid-ramp.
func rampWait(ctx context.Context, ramp time.Duration, i, total int) error {
	if ramp <= 0 || total <= 1 {
		return nil
	}
	perClient := ramp / time.Duration(total)
	if perClient <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(perClient):
		return nil
	}
}

// nameWidth returns the zero-pad width for generated names: at least 4, wider if
// count needs it (so col-0001 for small runs, col-0000123 for large ones).
func nameWidth(count int) int {
	width := len(fmt.Sprintf("%d", count))
	if width < 4 {
		return 4
	}
	return width
}

// hostIPForIndex maps a client index to a distinct 10.x.x.x address so each
// virtual supervisor reports its own host.ip and the fleet's collector list
// shows separate machines. The 24 low bits of i fill the last three octets, giving ~16M unique
// addresses before wrap — far past any realistic --count.
func hostIPForIndex(i int) string {
	return fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff)
}

func transportMode(shared bool) string {
	if shared {
		return "shared"
	}
	return "per-client"
}

// inflightLabel renders the --max-inflight cap for the startup banner.
func inflightLabel(max int) string {
	if max <= 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%d", max)
}
