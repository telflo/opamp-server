package middleware

import (
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// DefaultMaxConcurrentRequests is OPAMP_MAX_CONCURRENT_REQUESTS's default:
// the most agent requests one pod handles at once.
const DefaultMaxConcurrentRequests = 100

// Retry-After band (seconds) for a shed 429. Short because admission rejection
// signals TRANSIENT overload (the server is momentarily at capacity), unlike
// the rate limiter's per-org policy window — a supervisor that backs off a few
// seconds and re-polls usually finds the burst drained, and the band stays well
// under a typical poll interval so a shed poll is effectively just deferred to
// its next cadence.
//
// The value is JITTERED across [min, min+jitter] per response so a cohort shed
// in the same instant does not all retry at the same moment and re-synchronize
// into the next peak. Independent randomness per response is what breaks the
// correlation; a constant Retry-After would just move the thundering herd, not
// disperse it.
const (
	retryAfterMinSeconds    = 3
	retryAfterJitterSeconds = 5 // range: [3, 8]
)

// shedLogInterval bounds how often admission sheds are logged: one WARN per
// interval carrying the number of sheds since the previous line. Overload is
// precisely when the process can least afford a log record per rejected poll.
const shedLogInterval = time.Second

// inFlightStaleThreshold is the age past which a held slot is reported as
// stale in the shed WARN: twice a request's 5 s database deadline
// (DatabaseDeadline), so a slot older than this is either chaining several
// slow calls (throughput ceiling) or blocked on something with no deadline
// (a wedge).
const inFlightStaleThreshold = 10 * time.Second

// inFlightSummary reports how many slots are held, the oldest age among them,
// and how many exceed stale. A zero start marks a free slot and is skipped.
// Pure so it can be tested against fixed clocks; called only when a shed WARN
// is emitted or a Snapshot is requested, never on the per-request fast path.
func inFlightSummary(starts []atomic.Int64, now time.Time, stale time.Duration) (inFlight int, oldest time.Duration, staleCount int) {
	for i := range starts {
		startNanos := starts[i].Load()
		if startNanos == 0 {
			continue
		}
		inFlight++
		age := now.Sub(time.Unix(0, startNanos))
		if age > oldest {
			oldest = age
		}
		if age > stale {
			staleCount++
		}
	}
	return inFlight, oldest, staleCount
}

// jitteredRetryAfterSeconds returns a per-call random Retry-After in
// [retryAfterMinSeconds, retryAfterMinSeconds+retryAfterJitterSeconds].
// math/rand/v2's IntN is lock-free and safe for concurrent use, so it adds no
// contention on the shed path.
func jitteredRetryAfterSeconds() int {
	return jitter(retryAfterMinSeconds, retryAfterMinSeconds+retryAfterJitterSeconds)
}

// AdmissionSnapshot is the on-demand state of the gate, served by the
// admission view (package diagnostics: behind the management secret, or on `serve agent`'s loopback diagnostics listener) so an operator can watch in-flight climb
// before saturation, and — after stopping a load generator — see whether the
// slots drained (a throughput ceiling) or are still held (a wedge).
type AdmissionSnapshot struct {
	// Enabled is false when maxInFlight <= 0; every other field is then zero.
	Enabled            bool          `json:"enabled"`
	MaxInFlight        int           `json:"max_in_flight"`
	InFlight           int           `json:"in_flight"`
	OldestInFlightAge  time.Duration `json:"-"`
	OverStaleThreshold int           `json:"in_flight_over_stale_threshold"`
	StaleThreshold     time.Duration `json:"-"`
	// ShedTotal is cumulative since process start; it never resets.
	ShedTotal int64 `json:"shed_total"`
}

// ConcurrencyLimiter bounds how many requests the process handles
// CONCURRENTLY, shedding the overflow with an immediate 429 + Retry-After
// rather than admitting it. This is server self-protection, distinct from the
// per-org rate limiter (tenant policy): without it, an overload has no ceiling,
// so handler goroutines and their in-flight DB-connection waits pile up until
// polls breach the client's timeout — a slow, self-amplifying collapse. A fast
// 429 is lossless here because OpAMP polls are idempotent and retried on the
// next cadence (Retry-After honored), so shedding defers work instead of
// losing it.
//
// The gate is a buffered channel of maxInFlight slot indices. Acquisition is
// NON-BLOCKING (select/default): a request that cannot grab a slot is rejected
// at once, never parked — a queue of waiting polls would just be memory
// competing with the drain and would serve requests the client has already
// abandoned. The slot is released on handler return via defer, so a panicking
// or slow handler still frees it.
//
// Each slot records when it was taken (starts[i], zero = free) so the shed
// WARN and Snapshot can report how long the slots have been held. That
// distinguishes a throughput ceiling (slots turning over in seconds) from a
// wedge (slots held for minutes, never released) without a goroutine dump.
// The stamp is one atomic store per admitted request; the scan over all
// slots happens only when a WARN is emitted (≤ once per second) or a
// Snapshot is requested.
//
// A shed poll is stamped opamp.outcome=shed on the server span so spanmetrics
// counts it separately from rate-limit 429s, and logged at WARN at most once
// per shedLogInterval with the shed count since the previous line.
//
// maxInFlight <= 0 disables the gate (Middleware returns the handler
// unwrapped), so a misconfiguration or a test path is never accidentally
// throttled to zero. Per-process and in-memory, like the rate limiter: each
// replica protects itself, which is exactly the intent.
type ConcurrencyLimiter struct {
	maxInFlight int
	now         func() time.Time
	logger      *slog.Logger

	// Free-list of slot indices; a slot's start time lives in starts[i].
	sem    chan int
	starts []atomic.Int64

	shedTotal    atomic.Int64
	shedSinceLog atomic.Int64
	lastLogNanos atomic.Int64
}

// NewConcurrencyLimiter builds the gate on the clock now, logging to logger
// (nil logs nothing); see ConcurrencyLimiter for semantics.
func NewConcurrencyLimiter(maxInFlight int, now func() time.Time, logger *slog.Logger) *ConcurrencyLimiter {
	l := &ConcurrencyLimiter{maxInFlight: maxInFlight, now: now, logger: logger}
	if maxInFlight <= 0 {
		return l
	}
	l.sem = make(chan int, maxInFlight)
	for i := 0; i < maxInFlight; i++ {
		l.sem <- i
	}
	l.starts = make([]atomic.Int64, maxInFlight)
	return l
}

// Snapshot reads the gate's current state. Safe to call concurrently with
// admissions; the scan is over at most maxInFlight atomics.
func (l *ConcurrencyLimiter) Snapshot() AdmissionSnapshot {
	if l.maxInFlight <= 0 {
		return AdmissionSnapshot{}
	}
	inFlight, oldest, stale := inFlightSummary(l.starts, l.now(), inFlightStaleThreshold)
	return AdmissionSnapshot{
		Enabled:            true,
		MaxInFlight:        l.maxInFlight,
		InFlight:           inFlight,
		OldestInFlightAge:  oldest,
		OverStaleThreshold: stale,
		StaleThreshold:     inFlightStaleThreshold,
		ShedTotal:          l.shedTotal.Load(),
	}
}

// Middleware returns the gate as a Middleware. A disabled gate returns the
// handler unwrapped.
func (l *ConcurrencyLimiter) Middleware() Middleware {
	if l.maxInFlight <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case slot := <-l.sem:
				l.starts[slot].Store(l.now().UnixNano())
				defer func() {
					l.starts[slot].Store(0)
					l.sem <- slot
				}()
				next.ServeHTTP(w, r)
			default:
				trace.SpanFromContext(r.Context()).SetAttributes(
					attribute.String("opamp.outcome", "shed"),
					attribute.Int("opamp.admission.max_in_flight", l.maxInFlight),
				)
				l.shedTotal.Add(1)
				shed := l.shedSinceLog.Add(1)
				if l.logger != nil {
					now := l.now()
					last := l.lastLogNanos.Load()
					if now.UnixNano()-last >= int64(shedLogInterval) && l.lastLogNanos.CompareAndSwap(last, now.UnixNano()) {
						l.shedSinceLog.Add(-shed)
						_, oldest, stale := inFlightSummary(l.starts, now, inFlightStaleThreshold)
						l.logger.Warn("admission control shedding polls: process at max in-flight requests",
							"route", AgentRoute,
							"outcome", "shed",
							"max_in_flight", l.maxInFlight,
							"shed_since_last_log", shed,
							"oldest_in_flight_age", oldest.Round(time.Millisecond).String(),
							"in_flight_over_stale_threshold", stale)
					}
				}
				WriteTooManyRequests(w, jitteredRetryAfterSeconds())
			}
		})
	}
}
