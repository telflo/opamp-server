package middleware

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gate is a concurrency limiter on a test clock, logging nowhere.
func gate(maxInFlight int) (*ConcurrencyLimiter, *testClock) {
	clock := newTestClock()
	return NewConcurrencyLimiter(maxInFlight, clock.Now, nil), clock
}

// blockingHandler holds each request open until release is closed, and signals
// on started when a request has entered (i.e. has acquired a slot). Lets a test
// pin N slots as "in flight" deterministically.
func blockingHandler(started chan<- struct{}, release <-chan struct{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})
}

func TestConcurrencyLimit_AllowsRequestUnderLimit(t *testing.T) {
	h := mustGate(2)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/opamp/api/v1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (request under the limit must pass)", rec.Code)
	}
}

func TestConcurrencyLimit_RejectsOverLimitWith429AndRetryAfter(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	h := mustGate(1)(blockingHandler(started, release))

	// Occupy the single slot with a request parked in the handler.
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/opamp/api/v1", nil))
	<-started

	// The gate is now full: the next request must be shed immediately.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/opamp/api/v1", nil))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429 (gate full must reject)", rec.Code)
	}
	ra := rec.Header().Get("Retry-After")
	if secs, err := strconv.Atoi(ra); err != nil || secs <= 0 {
		t.Fatalf("Retry-After = %q, want a positive integer seconds value", ra)
	}

	close(release)
}

func TestConcurrencyLimit_ReleasesSlotOnCompletion(t *testing.T) {
	// Buffered so the final "slot is reusable" request — which runs through the
	// same blocking handler — can send its started signal with no receiver
	// waiting (the first request's signal was already drained).
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	done := make(chan struct{})
	h := mustGate(1)(blockingHandler(started, release))

	go func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/opamp/api/v1", nil))
		close(done)
	}()
	<-started
	close(release) // let the in-flight request finish and free its slot
	<-done

	// Slot is free again; a fresh request must be admitted, not 429'd.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/opamp/api/v1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (slot freed on completion must be reusable)", rec.Code)
	}
}

func TestJitteredRetryAfter_WithinBand(t *testing.T) {
	lo := retryAfterMinSeconds
	hi := retryAfterMinSeconds + retryAfterJitterSeconds
	for i := 0; i < 200; i++ {
		v := jitteredRetryAfterSeconds()
		if v < lo || v > hi {
			t.Fatalf("jitteredRetryAfterSeconds()=%d, want within [%d,%d]", v, lo, hi)
		}
	}
}

func TestJitteredRetryAfter_Varies(t *testing.T) {
	// The whole point is decorrelation: a shed cohort must NOT all get the same
	// Retry-After, or they re-synchronize into the next peak. Over many samples
	// we must see more than one distinct value.
	seen := map[int]struct{}{}
	for i := 0; i < 200; i++ {
		seen[jitteredRetryAfterSeconds()] = struct{}{}
	}
	if len(seen) < 2 {
		t.Fatalf("jitteredRetryAfterSeconds() produced only %d distinct value(s) over 200 samples; jitter is not decorrelating", len(seen))
	}
}

func TestConcurrencyLimit_ZeroDisablesGate(t *testing.T) {
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	h := mustGate(0)(blockingHandler(started, release))

	// With the gate disabled, many concurrent requests all get in — none shed.
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/opamp/api/v1", nil))
			if rec.Code != http.StatusOK {
				t.Errorf("got %d, want 200 (disabled gate must not throttle)", rec.Code)
			}
		}()
	}
	// All three must be able to enter the handler simultaneously.
	for i := 0; i < 3; i++ {
		<-started
	}
	close(release)
	wg.Wait()
}

// When the gate sheds, the sampled WARN must say how long the slots have been
// held: the oldest in-flight request's age and how many exceed the stale
// threshold (10 s, twice the database deadline). This is what separates a
// throughput ceiling (ages of seconds, slots turning over slowly) from a
// wedge (ages of minutes, slots never released) from the log line alone,
// with no goroutine dump. The ages are read from the injected clock.
func TestConcurrencyLimit_ShedLogReportsOldestInFlightAge(t *testing.T) {
	logs := &strings.Builder{}
	clock := newTestClock()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	h := NewConcurrencyLimiter(1, clock.Now, captureLogs(logs)).Middleware()(blockingHandler(started, release))

	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/opamp/api/v1", nil))
	<-started
	clock.Advance(11 * time.Second)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/opamp/api/v1", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429", rec.Code)
	}
	close(release)

	lines := logLines(t, logs)
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want exactly 1 shed WARN: %v", len(lines), lines)
	}
	if got := lines[0]["oldest_in_flight_age"]; got != "11s" {
		t.Fatalf("oldest_in_flight_age = %v, want 11s by the clock", got)
	}
	if got := lines[0]["in_flight_over_stale_threshold"]; got != float64(1) {
		t.Fatalf("in_flight_over_stale_threshold = %v, want 1 (held 11 s, past 10 s)", got)
	}
}

// inFlightSummary is the pure core of the age report: a zero start means the
// slot is free and is ignored; oldest is measured against now; stale counts
// slots older than the threshold.
func TestInFlightSummary(t *testing.T) {
	now := time.Unix(1_000, 0)
	starts := make([]atomic.Int64, 4)
	starts[0].Store(0)                                     // free slot
	starts[1].Store(now.Add(-2 * time.Second).UnixNano())  // young
	starts[2].Store(now.Add(-40 * time.Second).UnixNano()) // stale
	starts[3].Store(now.Add(-16 * time.Second).UnixNano()) // just over threshold

	inFlight, oldest, stale := inFlightSummary(starts, now, 15*time.Second)
	if inFlight != 3 {
		t.Fatalf("inFlight = %d, want 3 (the free slot is not counted)", inFlight)
	}
	if oldest != 40*time.Second {
		t.Fatalf("oldest = %s, want 40s", oldest)
	}
	if stale != 2 {
		t.Fatalf("stale = %d, want 2 (40s and 16s exceed the 15s threshold)", stale)
	}
}

func TestInFlightSummary_AllFree(t *testing.T) {
	starts := make([]atomic.Int64, 3)
	inFlight, oldest, stale := inFlightSummary(starts, time.Now(), 15*time.Second)
	if inFlight != 0 || oldest != 0 || stale != 0 {
		t.Fatalf("got inFlight=%d oldest=%s stale=%d, want 0/0/0 when every slot is free", inFlight, oldest, stale)
	}
}

// Snapshot is the on-demand view of the gate for the /opamp/api/v1/debug/admission route:
// how many slots are held right now, for how long, and how many polls have
// been shed in total. It must be readable at any time, not only when a shed
// happens to emit a WARN.
func TestConcurrencyLimiter_SnapshotReportsInFlightAndShed(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	lim, clock := gate(1)
	h := lim.Middleware()(blockingHandler(started, release))

	if s := lim.Snapshot(); !s.Enabled || s.MaxInFlight != 1 || s.InFlight != 0 || s.ShedTotal != 0 {
		t.Fatalf("idle snapshot = %+v, want enabled, max 1, nothing in flight, nothing shed", s)
	}

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/opamp/api/v1", nil))
		close(done)
	}()
	<-started
	clock.Advance(10 * time.Millisecond)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/opamp/api/v1", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429", rec.Code)
	}

	s := lim.Snapshot()
	if s.InFlight != 1 {
		t.Fatalf("InFlight = %d, want 1 while the slot is held", s.InFlight)
	}
	if s.OldestInFlightAge < 10*time.Millisecond {
		t.Fatalf("OldestInFlightAge = %s, want >= 10ms", s.OldestInFlightAge)
	}
	if s.ShedTotal != 1 {
		t.Fatalf("ShedTotal = %d, want 1", s.ShedTotal)
	}
	if s.OverStaleThreshold != 0 {
		t.Fatalf("OverStaleThreshold = %d, want 0", s.OverStaleThreshold)
	}

	close(release)
	<-done
	if s := lim.Snapshot(); s.InFlight != 0 || s.OldestInFlightAge != 0 {
		t.Fatalf("after release snapshot = %+v, want nothing in flight", s)
	}
	if s := lim.Snapshot(); s.ShedTotal != 1 {
		t.Fatalf("ShedTotal after release = %d, want 1 (cumulative, never reset)", s.ShedTotal)
	}
}

func TestConcurrencyLimiter_SnapshotWhenDisabled(t *testing.T) {
	lim, _ := gate(0)
	if s := lim.Snapshot(); s.Enabled {
		t.Fatalf("snapshot = %+v, want Enabled=false for a disabled gate", s)
	}
}

// mustGate is a concurrency limiter's middleware on a test clock.
func mustGate(maxInFlight int) Middleware {
	lim, _ := gate(maxInFlight)
	return lim.Middleware()
}

// Admission sheds are logged at most once per shedLogInterval, and the line
// carries how many polls were shed since the previous line, so a burst of 20
// simultaneous overflows yields one WARN reporting a positive count.
func TestConcurrencyLimit_ShedLogIsSampled(t *testing.T) {
	logs := &strings.Builder{}
	clock := newTestClock()
	started := make(chan struct{})
	release := make(chan struct{})
	chain := NewConcurrencyLimiter(1, clock.Now, captureLogs(logs)).Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	go chain.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/opamp/api/v1", nil))
	<-started

	const burst = 20
	var wg sync.WaitGroup
	codes := make(chan int, burst)
	for range burst {
		wg.Go(func() {
			rr := httptest.NewRecorder()
			chain.ServeHTTP(rr, httptest.NewRequest("POST", "/opamp/api/v1", nil))
			codes <- rr.Code
		})
	}
	wg.Wait()
	close(release)
	close(codes)
	for code := range codes {
		if code != http.StatusTooManyRequests {
			t.Fatalf("a shed request answered %d", code)
		}
	}
	lines := logLines(t, logs)
	if len(lines) != 1 {
		t.Fatalf("log lines = %d, want exactly 1 within one shedLogInterval", len(lines))
	}
	if lines[0]["level"] != "WARN" || lines[0]["max_in_flight"] != float64(1) {
		t.Errorf("the shed line = %v", lines[0])
	}
	if n, _ := lines[0]["shed_since_last_log"].(float64); n < 1 {
		t.Errorf("shed_since_last_log = %v, want a positive count", lines[0]["shed_since_last_log"])
	}
}
