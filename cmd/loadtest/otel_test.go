package main

import (
	"strings"
	"syscall"
	"testing"
	"time"
)

// Registration ends the moment every client has rotated AND (with config
// delivery on) stored a config — not before.
func TestRegistrationDone_AllClients(t *testing.T) {
	deadline := 10 * time.Minute
	cases := []struct {
		name     string
		s        Snapshot
		delivery bool
		want     bool
	}{
		{"nothing yet", Snapshot{}, true, false},
		{"rotated but no config", Snapshot{Rotations: 50}, true, false},
		{"rotated, config disabled", Snapshot{Rotations: 50}, false, true},
		{"rotated and configured", Snapshot{Rotations: 50, Configs: 50}, true, true},
		{"one short", Snapshot{Rotations: 50, Configs: 49}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			done, reason := registrationDone(tc.s, 50, tc.delivery, time.Minute, deadline)
			if done != tc.want {
				t.Fatalf("done = %v (reason %q), want %v", done, reason, tc.want)
			}
			if done && reason != "all clients registered" {
				t.Errorf("reason = %q, want the all-clients reason", reason)
			}
		})
	}
}

// Past the deadline the phase flips regardless, and the reason records how far
// short the fleet fell so the report can flag a partial registration.
func TestRegistrationDone_Deadline(t *testing.T) {
	s := Snapshot{Rotations: 40, Configs: 10}
	done, reason := registrationDone(s, 50, true, 3*time.Minute, 2*time.Minute)
	if !done {
		t.Fatal("expected the deadline to end registration")
	}
	if !strings.Contains(reason, "rot=40/50") || !strings.Contains(reason, "cfg=10/50") {
		t.Errorf("reason %q should carry the shortfall", reason)
	}
}

// The deadline covers the deploy window plus two jittered polls plus grace.
func TestRegistrationDeadline(t *testing.T) {
	got := registrationDeadline(2*time.Minute, 30*time.Second, 0.1)
	want := 2*time.Minute + 66*time.Second + 5*time.Second
	if got != want {
		t.Errorf("deadline = %s, want %s", got, want)
	}
}

// A nil *Telemetry (no --otlp-endpoint) must be safe on every hook — the
// Collector calls them unconditionally on the hot path.
func TestTelemetry_NilSafe(t *testing.T) {
	var tel *Telemetry
	tel.clientLaunched()
	tel.onPoll("2xx", time.Millisecond)
	tel.onTransportError(errKindTimeout)
	tel.onRotation()
	tel.onConfigDelivered()
	tel.runStart()
	tel.enterSteady("test", Snapshot{}, time.Second)
	tel.runEnd(Snapshot{}, time.Second)
	if tel.CurrentPhase() != PhaseRegistration {
		t.Errorf("nil telemetry phase = %v, want registration", tel.CurrentPhase())
	}
	if err := tel.Shutdown(t.Context()); err != nil {
		t.Errorf("Shutdown on nil = %v", err)
	}

	// And the Collector hooks route through it without panicking.
	c := &Collector{}
	c.recordSuccess(200, time.Millisecond)
	c.recordTransportError(syscall.ECONNRESET)
	c.recordRotation()
	c.recordConfigDelivered()
	if got := c.Peek().Total; got != 2 {
		t.Errorf("Peek().Total = %d, want 2", got)
	}
}

// Peek must not drain the latency window that Snapshot's percentiles depend on.
func TestCollector_PeekDoesNotDrain(t *testing.T) {
	c := &Collector{}
	c.recordSuccess(200, 7*time.Millisecond)
	_ = c.Peek()
	if s := c.Snapshot(); s.WindowCount != 1 {
		t.Errorf("WindowCount after Peek = %d, want 1", s.WindowCount)
	}
}

func TestDefaultRunID(t *testing.T) {
	now := time.Date(2026, 9, 2, 14, 30, 0, 0, time.UTC)
	if got := defaultRunID(50, now); got != "opamp-50-20260902-143000" {
		t.Errorf("defaultRunID = %q", got)
	}
}

func TestTargetHost(t *testing.T) {
	if got := targetHost("https://telflo.com/opamp/api/v1"); got != "telflo.com" {
		t.Errorf("targetHost = %q, want telflo.com", got)
	}
	if got := targetHost("not a url"); got != "not a url" {
		t.Errorf("targetHost fallback = %q", got)
	}
}

// With no endpoint newTelemetry is the no-op gate: nil, nil.
func TestNewTelemetry_NoEndpointIsNoop(t *testing.T) {
	tel, err := newTelemetry(t.Context(), telemetryConfig{endpoint: "  "})
	if err != nil || tel != nil {
		t.Fatalf("newTelemetry(empty) = %v, %v; want nil, nil", tel, err)
	}
}
