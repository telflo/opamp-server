package main

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCollector_StatusCounters(t *testing.T) {
	c := &Collector{}
	c.recordSuccess(200, 5*time.Millisecond)
	c.recordSuccess(200, 5*time.Millisecond)
	c.recordSuccess(401, time.Millisecond)
	c.recordSuccess(429, time.Millisecond)
	c.recordSuccess(503, time.Millisecond)
	c.recordTransportError(syscall.ECONNREFUSED)
	c.recordRotation()
	c.recordConfigDelivered()
	c.recordConfigDelivered()

	s := c.Snapshot()
	if s.Total != 6 {
		t.Errorf("Total = %d, want 6", s.Total)
	}
	if s.Status2xx != 2 {
		t.Errorf("Status2xx = %d, want 2", s.Status2xx)
	}
	if s.Status401 != 1 {
		t.Errorf("Status401 = %d, want 1", s.Status401)
	}
	if s.Status429 != 1 {
		t.Errorf("Status429 = %d, want 1", s.Status429)
	}
	if s.StatusOther != 1 {
		t.Errorf("StatusOther = %d, want 1", s.StatusOther)
	}
	if s.Transport != 1 {
		t.Errorf("Transport = %d, want 1", s.Transport)
	}
	if s.Rotations != 1 {
		t.Errorf("Rotations = %d, want 1", s.Rotations)
	}
	if s.Configs != 2 {
		t.Errorf("Configs = %d, want 2", s.Configs)
	}
}

// recordTransportError buckets by cause so the metrics separate a generator/OS
// limit (refused/reset) from a slow server (timeout).
func TestCollector_TransportErrorBreakdown(t *testing.T) {
	c := &Collector{}
	c.recordTransportError(syscall.ECONNREFUSED)
	c.recordTransportError(syscall.ECONNREFUSED)
	c.recordTransportError(syscall.ECONNRESET)
	c.recordTransportError(errors.New("something unclassified")) // -> other
	c.recordTransportError(context.DeadlineExceeded)             // -> timeout

	s := c.Snapshot()
	if s.Transport != 5 {
		t.Errorf("Transport = %d, want 5", s.Transport)
	}
	if s.ErrRefused != 2 {
		t.Errorf("ErrRefused = %d, want 2", s.ErrRefused)
	}
	if s.ErrReset != 1 {
		t.Errorf("ErrReset = %d, want 1", s.ErrReset)
	}
	if s.ErrTimeout != 1 {
		t.Errorf("ErrTimeout = %d, want 1", s.ErrTimeout)
	}
	if s.ErrOther != 1 {
		t.Errorf("ErrOther = %d, want 1", s.ErrOther)
	}
}

func TestCollector_Percentiles(t *testing.T) {
	c := &Collector{}
	// 100 latencies: 1ms..100ms. Nearest-rank p50→51ms, p95→96ms, p99→100ms.
	for i := 1; i <= 100; i++ {
		c.recordSuccess(200, time.Duration(i)*time.Millisecond)
	}
	s := c.Snapshot()
	if s.WindowCount != 100 {
		t.Fatalf("WindowCount = %d, want 100", s.WindowCount)
	}
	if s.P50 != 51*time.Millisecond {
		t.Errorf("P50 = %s, want 51ms", s.P50)
	}
	if s.P95 != 96*time.Millisecond {
		t.Errorf("P95 = %s, want 96ms", s.P95)
	}
	if s.P99 != 100*time.Millisecond {
		t.Errorf("P99 = %s, want 100ms", s.P99)
	}
	if s.Max != 100*time.Millisecond {
		t.Errorf("Max = %s, want 100ms", s.Max)
	}
}

// Snapshot drains the latency window so percentiles reflect only the requests
// since the last snapshot; counters remain cumulative.
func TestCollector_WindowDrains(t *testing.T) {
	c := &Collector{}
	c.recordSuccess(200, 10*time.Millisecond)
	_ = c.Snapshot()

	s := c.Snapshot()
	if s.WindowCount != 0 {
		t.Errorf("second window count = %d, want 0 (drained)", s.WindowCount)
	}
	if s.Total != 1 {
		t.Errorf("Total = %d, want 1 (cumulative)", s.Total)
	}
}

// The server has two different 429s and they mean opposite things: the
// per-token rate limiter (tenant policy, Retry-After: 60) and the admission
// gate shedding overload (process self-protection, Retry-After: 3–8s). The
// tool must count them apart, keyed on that header, or a run cannot tell
// "fleet is over its rpm cap" from "server is at max in-flight".
func TestCollector_Splits429ByRetryAfter(t *testing.T) {
	c := &Collector{}
	c.recordResponse(429, "60", time.Millisecond) // rate limiter
	c.recordResponse(429, "5", time.Millisecond)  // admission shed
	c.recordResponse(429, "3", time.Millisecond)  // admission shed
	c.recordResponse(429, "", time.Millisecond)   // unknown (older server / proxy stripped it)
	c.recordResponse(200, "", time.Millisecond)

	s := c.Snapshot()
	if s.Status429 != 4 {
		t.Errorf("Status429 = %d, want 4 (total across kinds)", s.Status429)
	}
	if s.Status429RateLimit != 1 {
		t.Errorf("Status429RateLimit = %d, want 1", s.Status429RateLimit)
	}
	if s.Status429Shed != 2 {
		t.Errorf("Status429Shed = %d, want 2", s.Status429Shed)
	}
	if line := s.Line(1, 1); !strings.Contains(line, "429=4(rl=1 shed=2)") {
		t.Errorf("Line() = %q, want it to carry the 429 split", line)
	}
}

func TestClassify429(t *testing.T) {
	cases := map[string]string{
		"60": "ratelimit", "120": "ratelimit", "30": "ratelimit",
		"3": "shed", "8": "shed", "29": "shed",
		"": "unknown", "abc": "unknown", "0": "unknown",
	}
	for in, want := range cases {
		if got := classify429(in); got != want {
			t.Errorf("classify429(%q) = %q, want %q", in, got, want)
		}
	}
}
