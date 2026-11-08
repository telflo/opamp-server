package serve

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/telflo/opamp-server/agent"
	"github.com/telflo/opamp-server/middleware"
	"github.com/telflo/opamp-server/store/collectors"
)

// probe sends method target through handler, with a user agent and a
// forwarded address (markers), and answers the reply.
func probe(handler http.Handler, method, target string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, nil)
	request.Header.Set("User-Agent", "user-agent-marker-41c2")
	request.Header.Set("X-Forwarded-For", "203.0.113.99")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// The agent listener answers the poll on POST /opamp/api/v1 (through the
// stack: no credential is 401), /health without the database, /ready, and
// 404 for anything else: another method on the poll's path, another path,
// the debug routes, a path that would be cleaned or redirected. A 404
// writes no log line and makes no span: nothing of the path or the user
// agent is written down.
func TestTheAgentListenerServesThePollHealthReadyAndNothingElse(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	log := &syncBuffer{}
	databaseCalls := 0
	r := testReplica(t, replicaSettings(t, testKeys(t)), testReplicaStores(t), func(context.Context) error {
		databaseCalls++
		return nil
	}, newTestClock(), log)
	r.readiness.serving()

	if rec := probe(r.agent, http.MethodGet, "/health"); rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Fatalf("GET /health: %d %q", rec.Code, rec.Body.String())
	}
	if rec := probe(r.agent, http.MethodHead, "/health"); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD /health: %d %q", rec.Code, rec.Body.String())
	}
	if databaseCalls != 0 {
		t.Fatalf("/health asked the database %d times", databaseCalls)
	}
	if rec := probe(r.agent, http.MethodGet, "/ready"); rec.Code != http.StatusOK || rec.Body.String() != "ready\n" || databaseCalls != 1 {
		t.Fatalf("GET /ready: %d %q, %d pings", rec.Code, rec.Body.String(), databaseCalls)
	}
	if rec := probe(r.agent, http.MethodPost, middleware.AgentRoute); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a poll without a credential: %d", rec.Code)
	}
	before := len(recorder.Ended())
	for _, c := range []struct{ method, target string }{
		{http.MethodGet, middleware.AgentRoute},
		{http.MethodPut, middleware.AgentRoute},
		{http.MethodPost, middleware.AgentRoute + "/"},
		{http.MethodPost, "/opamp/api/v1/kubernetes"},
		{http.MethodGet, "/opamp/api/v1/debug/pprof/goroutine?debug=2"},
		{http.MethodGet, "/opamp/api/v1/debug/admission"},
		{http.MethodGet, "/api/v1/debug/pprof/"},
		{http.MethodGet, "/path-marker-9e1f?query-marker=1"},
		{http.MethodGet, "//health"},
		{http.MethodGet, "/ready/"},
		{http.MethodPost, "/health"},
		{http.MethodDelete, "/ready"},
	} {
		rec := probe(r.agent, c.method, c.target)
		if rec.Code != http.StatusNotFound || rec.Body.String() != "Not Found\n" || rec.Header().Get("Location") != "" {
			t.Errorf("%s %s: %d %q %v", c.method, c.target, rec.Code, rec.Body.String(), rec.Header())
		}
	}
	if spans := recorder.Ended()[before:]; len(spans) != 0 {
		t.Fatalf("the 404s made %d spans", len(spans))
	}
	logged := log.String()
	for _, marker := range []string{"path-marker", "query-marker", "user-agent-marker", "203.0.113.99", "kubernetes", "debug"} {
		if strings.Contains(logged, marker) {
			t.Fatalf("the log holds %q: %s", marker, logged)
		}
	}
	// The poll's 401 is the one line, and the poll's span the one span.
	if lines := strings.Count(strings.TrimSpace(logged), "\n") + 1; lines != 1 || !strings.Contains(logged, "missing_credentials") {
		t.Fatalf("the log: %s", logged)
	}
	if spans := recorder.Ended(); len(spans) != 1 || spans[0].Name() != "POST /opamp/api/v1" {
		t.Fatalf("spans: %d", len(spans))
	}
}

// failingPing is a database that doesn't answer, as pgx reports it.
func failingPing(context.Context) error { return errors.New("dial tcp: connection refused") }

// /ready answers 503 while serve starts (the listeners not yet all open),
// 200 once serving while the database answers, 503 while it doesn't, and
// 503 from the moment serve is told to stop, the database answering or
// not. It writes no log line.
func TestReadinessDuringStartupADatabaseOutageAndTheStop(t *testing.T) {
	log := &syncBuffer{}
	var mu sync.Mutex
	down := false
	clock := newTestClock()
	r := testReplica(t, replicaSettings(t, testKeys(t)), testReplicaStores(t), func(ctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if down {
			return failingPing(ctx)
		}
		return nil
	}, clock, log)
	ready := func(label string, want int) {
		t.Helper()
		clock.Advance(ReadyCacheTTL) // past the last ping's answer
		if rec := probe(r.agent, http.MethodGet, "/ready"); rec.Code != want || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: /ready answered %d, want %d", label, rec.Code, want)
		}
	}
	ready("starting", http.StatusServiceUnavailable)
	r.readiness.serving()
	ready("serving", http.StatusOK)
	mu.Lock()
	down = true
	mu.Unlock()
	ready("the database down", http.StatusServiceUnavailable)
	mu.Lock()
	down = false
	mu.Unlock()
	ready("the database back", http.StatusOK)
	r.readiness.drain()
	ready("draining", http.StatusServiceUnavailable)
	r.readiness.serving() // a late serving() never undoes the drain
	ready("still draining", http.StatusServiceUnavailable)
	if logged := log.String(); logged != "" {
		t.Fatalf("readiness logged: %s", logged)
	}
}

// fakeCleaner records the cutoffs it is asked for, and answers each from
// results (the last again once they run out).
type fakeCleaner struct {
	mu      sync.Mutex
	cutoffs []time.Time
	results []collectors.CleanupResult
	err     error
	calls   chan struct{}
}

func (f *fakeCleaner) Cleanup(ctx context.Context, cutoff time.Time) (collectors.CleanupResult, error) {
	f.mu.Lock()
	f.cutoffs = append(f.cutoffs, cutoff)
	result := f.results[min(len(f.cutoffs), len(f.results))-1]
	f.mu.Unlock()
	f.calls <- struct{}{}
	return result, f.err
}

// The cleanup job runs at start, then on each tick, asking for the rows
// older than 30 days by the injected clock, and returns when its context
// ends; each run is one line saying what it did.
func TestTheCleanupJobRunsAtStartThenEachTickUntilStopped(t *testing.T) {
	clock := newTestClock()
	log := &syncBuffer{}
	cleaner := &fakeCleaner{calls: make(chan struct{}, 4), results: []collectors.CleanupResult{
		{Held: true, Deleted: 3}, {Held: false}, {Held: true}}}
	job := cleanupJob{store: cleaner, now: clock.Now, logger: slog.New(slog.NewJSONHandler(log, nil))}
	ticks := make(chan time.Time)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		job.run(ctx, ticks)
	}()
	// Each run awaited under a generous hang guard.
	ran := func(which string) {
		t.Helper()
		select {
		case <-cleaner.calls:
		case <-time.After(30 * time.Second):
			t.Fatalf("the job didn't run %s", which)
		}
	}
	ran("at start, with no tick")
	for _, which := range []string{"on the first tick", "on the second tick"} {
		clock.Advance(time.Hour)
		select {
		case ticks <- clock.Now():
		case <-time.After(30 * time.Second):
			t.Fatalf("the job took no tick %s", which)
		}
		ran(which)
	}
	stop()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the job didn't return once stopped")
	}
	start := newTestClock().Now()
	want := []time.Time{start.Add(-30 * 24 * time.Hour), start.Add(time.Hour - 30*24*time.Hour), start.Add(2*time.Hour - 30*24*time.Hour)}
	if fmt.Sprint(cleaner.cutoffs) != fmt.Sprint(want) {
		t.Fatalf("cutoffs %v, want %v", cleaner.cutoffs, want)
	}
	lines := logLines(t, log.String())
	if len(lines) != 3 || lines[0]["deleted"] != float64(3) || lines[0]["held"] != true || lines[1]["held"] != false ||
		!strings.Contains(lines[1]["msg"].(string), "another replica holds its lock") || lines[2]["deleted"] != float64(0) {
		t.Fatalf("the job's lines: %v", lines)
	}
}

// A failed run is one warning without the error's text (it may quote a
// value); a run cut short by the stop writes nothing.
func TestAFailedCleanupIsAWarningWithoutItsText(t *testing.T) {
	log := &syncBuffer{}
	cleaner := &fakeCleaner{calls: make(chan struct{}, 2), results: []collectors.CleanupResult{{}},
		err: errors.New("collectors.cleanup: secret-text-marker")}
	job := cleanupJob{store: cleaner, now: newTestClock().Now, logger: slog.New(slog.NewJSONHandler(log, nil))}
	job.once(context.Background())
	stopped, stop := context.WithCancel(context.Background())
	stop()
	job.once(stopped)
	lines := logLines(t, log.String())
	if len(lines) != 1 || lines[0]["level"] != "WARN" || strings.Contains(log.String(), "secret-text-marker") {
		t.Fatalf("the lines: %s", log.String())
	}
}

// With both TLS files set, the agent listener speaks TLS 1.2 or later with
// its certificate, apart from the management listener's plain HTTP.
func TestTheAgentListenerSpeaksTLSWithItsOwnCertificate(t *testing.T) {
	settings, files := tlsSettings(t)
	settings.Agent.Host, settings.Agent.TLS = "127.0.0.1", settings.Management.TLS
	settings.Management.TLS = nil
	log := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan int, 1)
	go func() {
		exited <- serveListeners(ctx, []listenerSpec{managementSpec(settings, healthOnly()), agentSpec(settings, healthOnly())},
			lifecycle{}, slog.New(slog.NewJSONHandler(log, nil)), ShutdownTimeout)
	}()
	agent := waitForLine(t, log, "serving the agent endpoint")
	management := waitForLine(t, log, "serving the management API")
	if agent["tls"] != true || management["tls"] != false {
		t.Fatalf("the serving lines: %v, %v", agent, management)
	}
	client := httpsClient(&tls.Config{RootCAs: files.Pool})
	if status, _, err := get(t, client, "https://"+agent["address"].(string)+"/health", ""); err != nil || status != http.StatusOK {
		t.Fatalf("the agent listener over TLS: %d, %v", status, err)
	}
	old := httpsClient(&tls.Config{RootCAs: files.Pool, MaxVersion: tls.VersionTLS11})
	if _, _, err := get(t, old, "https://"+agent["address"].(string)+"/health", ""); err == nil {
		t.Fatal("TLS 1.1 was answered")
	}
	plain := &http.Client{Transport: &http.Transport{Proxy: nil}}
	if status, _, err := get(t, plain, "http://"+management["address"].(string)+"/health", ""); err != nil || status != http.StatusOK {
		t.Fatalf("the management listener in plain HTTP: %d, %v", status, err)
	}
	cancel()
	if code := <-exited; code != 0 {
		t.Fatalf("exited %d", code)
	}
}

// waitForLine waits, bounded by attempts, for the log line whose message is
// msg, and answers it decoded.
func waitForLine(t *testing.T, log *syncBuffer, msg string) map[string]any {
	t.Helper()
	for attempt := 0; attempt < 1000; attempt++ {
		for _, line := range logLines(t, log.String()) {
			if line["msg"] == msg {
				return line
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no line %q in %s", msg, log.String())
	return nil
}

// A request still running at the shutdown timeout is cut off, and serve
// waits at most CloseWait for its handler to return: a handler that
// ignores its cancelled context doesn't hold serve. Both listeners stop
// together.
func TestAForcedStopWaitsForHandlersAtMostCloseWait(t *testing.T) {
	settings, _ := tlsSettings(t)
	settings.Management.TLS = nil
	settings.Agent.Host = "127.0.0.1"
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	stubborn := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release // ignores its context
	})
	log := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan int, 1)
	go func() {
		exited <- serveListeners(ctx, []listenerSpec{managementSpec(settings, healthOnly()), agentSpec(settings, stubborn)},
			lifecycle{}, slog.New(slog.NewJSONHandler(log, nil)), 0)
	}()
	agent := waitForLine(t, log, "serving the agent endpoint")
	go func() {
		plain := &http.Client{Transport: &http.Transport{Proxy: nil}}
		_, _, _ = get(t, plain, "http://"+agent["address"].(string)+"/stuck", "")
	}()
	<-started
	cancel()
	select {
	case code := <-exited:
		if code != 1 || !strings.Contains(log.String(), "requests were still running") {
			t.Fatalf("exited %d; log %s", code, log.String())
		}
	case <-time.After(CloseWait + 30*time.Second):
		t.Fatal("serve waited for a handler that never returns")
	}
}

// The agent listener faces the collectors' networks before any request is
// authenticated, so it bounds what a connection may hold: headers past
// agent.MaxHeaderBytes are answered 431 before any handler runs, and past
// OPAMP_AGENT_MAX_CONNECTIONS open connections a new one isn't answered
// until one of them closes.
func TestTheAgentListenerCapsHeadersAndConnections(t *testing.T) {
	settings := Settings{Agent: agent.Settings{Host: "127.0.0.1", MaxConnections: 1}}
	log := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan int, 1)
	go func() {
		exited <- serveListeners(ctx, []listenerSpec{agentSpec(settings, healthOnly())},
			lifecycle{}, slog.New(slog.NewJSONHandler(log, nil)), ShutdownTimeout)
	}()
	serving := waitForLine(t, log, "serving the agent endpoint")
	address := serving["address"].(string)
	if serving["maxConnections"] != float64(1) {
		t.Fatalf("the serving line: %v", serving)
	}

	// ask sends GET /health with filler headers on a connection of its own,
	// and answers the reply's status line.
	ask := func(filler int, timeout time.Duration) (string, error) {
		conn, err := net.DialTimeout("tcp", address, timeout)
		if err != nil {
			return "", err
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(timeout))
		if _, err := fmt.Fprintf(conn, "GET /health HTTP/1.1\r\nHost: opamp\r\nX-Filler: %s\r\nConnection: close\r\n\r\n",
			strings.Repeat("h", filler)); err != nil {
			return "", err
		}
		return bufio.NewReader(conn).ReadString('\n')
	}
	if status, err := ask(8<<10, 5*time.Second); err != nil || !strings.HasPrefix(status, "HTTP/1.1 200") {
		t.Fatalf("8 KiB of headers: %q, %v", status, err)
	}
	if status, err := ask(2*agent.MaxHeaderBytes, 5*time.Second); err != nil || !strings.HasPrefix(status, "HTTP/1.1 431") {
		t.Fatalf("headers past the cap: %q, %v", status, err)
	}

	// A connection that sends nothing holds the only slot: the next isn't
	// answered while it is open, and is once it closes.
	held, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	if status, err := ask(0, time.Second); err == nil || status != "" {
		t.Fatalf("a connection past the cap was answered: %q", status)
	}
	_ = held.Close()
	if status, err := ask(0, 5*time.Second); err != nil || !strings.HasPrefix(status, "HTTP/1.1 200") {
		t.Fatalf("once the held connection closed: %q, %v", status, err)
	}

	cancel()
	if code := <-exited; code != 0 {
		t.Fatalf("exited %d; log %s", code, log.String())
	}
}
