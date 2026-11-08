package main

import (
	"fmt"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Collector is the thread-safe sink every virtual supervisor reports into.
//
// Counters are plain atomics (hot path, one add per poll). Latencies land in a
// mutex-guarded slice that Snapshot drains each window, so percentiles are
// computed over just the requests in that window and memory stays bounded by
// throughput × window length rather than by total run time.
type Collector struct {
	total     atomic.Uint64
	status2xx atomic.Uint64
	status401 atomic.Uint64
	status429 atomic.Uint64
	// status429 is split by the Retry-After the server sent, because the two
	// 429s mean opposite things (see classify429). Both add to status429.
	status429RateLimit atomic.Uint64
	status429Shed      atomic.Uint64
	statusOther        atomic.Uint64
	transportEr        atomic.Uint64
	errRefused         atomic.Uint64
	errReset           atomic.Uint64
	errTimeout         atomic.Uint64
	errOther           atomic.Uint64
	rotations          atomic.Uint64
	configs            atomic.Uint64

	mu        sync.Mutex
	latWindow []time.Duration

	// tel mirrors every count into the OTLP exporter when --otlp-endpoint is
	// set. nil (the default) means no export; every hook is nil-safe.
	tel *Telemetry
}

// recordSuccess records a completed HTTP round-trip (any status code) with no
// Retry-After header. See recordResponse.
func (c *Collector) recordSuccess(status int, latency time.Duration) {
	c.recordResponse(status, "", latency)
}

// classify429 tells the server's two 429s apart from the Retry-After it sent.
// The per-token rate limiter (tenant policy: the fleet is over its
// opamp_rate_limit_rpm) always sends a fixed 60; the admission gate (process
// self-protection: max in-flight requests reached) sends a jittered 3–8s.
// Splitting at 30 keeps both well clear of the threshold. A missing or
// unparseable header (older server, proxy stripped it) is "unknown" — still
// counted as a 429, just not attributed.
func classify429(retryAfter string) string {
	secs, err := strconv.Atoi(retryAfter)
	switch {
	case err != nil || secs <= 0:
		return "unknown"
	case secs >= 30:
		return "ratelimit"
	default:
		return "shed"
	}
}

// recordResponse records a completed HTTP round-trip (any status code).
// retryAfter is the response's Retry-After header, consulted only for 429s.
func (c *Collector) recordResponse(status int, retryAfter string, latency time.Duration) {
	c.total.Add(1)
	outcome := "other"
	switch {
	case status >= 200 && status < 300:
		c.status2xx.Add(1)
		outcome = "2xx"
	case status == 401:
		c.status401.Add(1)
		outcome = "401"
	case status == 429:
		c.status429.Add(1)
		outcome = "429"
		switch classify429(retryAfter) {
		case "ratelimit":
			c.status429RateLimit.Add(1)
			outcome = "429/ratelimit"
		case "shed":
			c.status429Shed.Add(1)
			outcome = "429/shed"
		}
	default:
		c.statusOther.Add(1)
	}
	c.mu.Lock()
	c.latWindow = append(c.latWindow, latency)
	c.mu.Unlock()
	c.tel.onPoll(outcome, latency)
}

// recordRotation counts a token the client actually adopted from a rotation
// offer (not merely offered) — one per handoff, so it tallies how many virtual
// supervisors completed the default→rotate switch.
func (c *Collector) recordRotation() {
	c.rotations.Add(1)
	c.tel.onRotation()
}

// recordConfigDelivered counts a remote config the server delivered and the
// client stored in memory — the signal that the config-delivery path (not just
// bare acks) is being exercised under load.
func (c *Collector) recordConfigDelivered() {
	c.configs.Add(1)
	c.tel.onConfigDelivered()
}

// Peek returns the cumulative counters WITHOUT draining the latency window, so
// the phase tracker can read progress without stealing the printer's
// percentiles.
func (c *Collector) Peek() Snapshot {
	return Snapshot{
		Total:       c.total.Load(),
		Status2xx:   c.status2xx.Load(),
		Status401:   c.status401.Load(),
		Status429:   c.status429.Load(),
		StatusOther: c.statusOther.Load(),
		Transport:   c.transportEr.Load(),
		ErrRefused:  c.errRefused.Load(),
		ErrReset:    c.errReset.Load(),
		ErrTimeout:  c.errTimeout.Load(),
		ErrOther:    c.errOther.Load(),
		Rotations:   c.rotations.Load(),
		Configs:     c.configs.Load(),

		Status429RateLimit: c.status429RateLimit.Load(),
		Status429Shed:      c.status429Shed.Load(),
	}
}

// recordTransportError records a request that never produced an HTTP response
// (connection refused, reset, timeout). These carry no latency worth
// aggregating, so they are counted but not added to the window. err is bucketed
// by cause so the summary separates a generator/OS limit (refused/reset) from a
// slow server (timeout).
func (c *Collector) recordTransportError(err error) {
	c.total.Add(1)
	c.transportEr.Add(1)
	kind := classifyTransportError(err)
	switch kind {
	case errKindRefused:
		c.errRefused.Add(1)
	case errKindReset:
		c.errReset.Add(1)
	case errKindTimeout:
		c.errTimeout.Add(1)
	default:
		c.errOther.Add(1)
	}
	c.tel.onTransportError(kind)
}

// Snapshot is the per-window view: it drains the latency window and returns the
// percentiles over it alongside the current cumulative counters.
type Snapshot struct {
	Total       uint64
	Status2xx   uint64
	Status401   uint64
	Status429   uint64
	StatusOther uint64
	Transport   uint64
	// Status429RateLimit + Status429Shed <= Status429; the remainder had no
	// attributable Retry-After.
	Status429RateLimit uint64
	Status429Shed      uint64
	ErrRefused         uint64
	ErrReset           uint64
	ErrTimeout         uint64
	ErrOther           uint64
	Rotations          uint64
	Configs            uint64

	WindowCount int
	P50         time.Duration
	P95         time.Duration
	P99         time.Duration
	Max         time.Duration
}

// Snapshot reads the cumulative counters and drains the latency window,
// computing percentiles over the requests seen since the previous call.
func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	window := c.latWindow
	c.latWindow = nil
	c.mu.Unlock()

	s := Snapshot{
		Total:       c.total.Load(),
		Status2xx:   c.status2xx.Load(),
		Status401:   c.status401.Load(),
		Status429:   c.status429.Load(),
		StatusOther: c.statusOther.Load(),
		Transport:   c.transportEr.Load(),
		ErrRefused:  c.errRefused.Load(),
		ErrReset:    c.errReset.Load(),
		ErrTimeout:  c.errTimeout.Load(),
		ErrOther:    c.errOther.Load(),
		Rotations:   c.rotations.Load(),
		Configs:     c.configs.Load(),
		WindowCount: len(window),

		Status429RateLimit: c.status429RateLimit.Load(),
		Status429Shed:      c.status429Shed.Load(),
	}
	if len(window) > 0 {
		sort.Slice(window, func(i, j int) bool { return window[i] < window[j] })
		s.P50 = percentile(window, 0.50)
		s.P95 = percentile(window, 0.95)
		s.P99 = percentile(window, 0.99)
		s.Max = window[len(window)-1]
	}
	return s
}

// percentile returns the p-quantile of an already-sorted slice using the
// nearest-rank method. p is in [0,1]; sorted must be non-empty.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(p * float64(len(sorted)))
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

// Line renders a single-line summary. rps is requests/sec over the window,
// active is the number of virtual supervisors currently running.
func (s Snapshot) Line(rps float64, active int) string {
	return fmt.Sprintf(
		"active=%d total=%d rps=%.1f | 2xx=%d 401=%d 429=%d(rl=%d shed=%d) other=%d "+
			"err=%d(ref=%d rst=%d to=%d oth=%d) rot=%d cfg=%d | "+
			"p50=%s p95=%s p99=%s max=%s (n=%d)",
		active, s.Total, rps,
		s.Status2xx, s.Status401, s.Status429, s.Status429RateLimit, s.Status429Shed, s.StatusOther,
		s.Transport, s.ErrRefused, s.ErrReset, s.ErrTimeout, s.ErrOther, s.Rotations, s.Configs,
		round(s.P50), round(s.P95), round(s.P99), round(s.Max), s.WindowCount,
	)
}

// round trims latency to milliseconds for readable output.
func round(d time.Duration) time.Duration {
	return d.Round(time.Millisecond)
}
