package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

// VirtualSupervisor impersonates one OTel supervisor: a stable instance identity
// polling the OpAMP server on its own cadence. It holds no collector — it just
// replays the poll a real supervisor would send, and carries the two pieces of
// state a real supervisor persists across polls: the token it authenticates with
// (which flips from the fleet default to a rotated per-instance token after the
// server's handoff) and the config the server last delivered (held in memory,
// its hash echoed back so the server marks the instance in-sync).
type VirtualSupervisor struct {
	name        string
	token       string // full "Bearer <ciphertext>" header value; mutated on rotation
	instanceUID [16]byte
	hostName    string
	hostIP      string
	groupID     string // telflo.collector_group_id; "" to omit
	seq         uint64

	endpoint string
	// deployInterval is the window the first poll is randomized across, spreading
	// the fleet's initial deployment; pollInterval is the steady-state cadence
	// between every poll thereafter. Split so a slow, staggered rollout can still
	// poll at a tight cadence once up (and vice versa).
	deployInterval time.Duration
	pollInterval   time.Duration
	jitter         float64
	client         *http.Client
	metrics        *Collector
	rng            *rand.Rand
	sem            chan struct{} // shared --max-inflight limiter; nil = unlimited

	// Config state the server delivers and we hold in memory. nil until the
	// first delivery; after that, configBody is reported as EffectiveConfig on
	// full-state polls and configHash is echoed in RemoteConfigStatus.
	configBody []byte
	configHash []byte
}

// Run polls until ctx is cancelled. Startup is deliberately phase-spread: the
// caller staggers goroutine launch, and the first sleep is randomized across the
// deploy interval, so N clients don't stampede the server in lockstep.
func (vs *VirtualSupervisor) Run(ctx context.Context) {
	// Randomized initial delay across [0, deployInterval) to de-sync the fleet:
	// this window is how long the whole fleet takes to finish its first poll.
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Duration(vs.rng.Int63n(int64(vs.deployInterval) + 1))):
	}

	// The first poll reports full state; later polls send the compact message
	// unless the server asks for full state again.
	fullState := true
	for {
		fullState = vs.poll(ctx, fullState)

		select {
		case <-ctx.Done():
			return
		case <-time.After(vs.nextSleep()):
		}
	}
}

// poll performs a single request/response cycle and records the outcome.
// It returns whether the *next* poll should report full state (true when the
// server set the ReportFullState flag on this response).
func (vs *VirtualSupervisor) poll(ctx context.Context, fullState bool) bool {
	vs.seq++
	// For --poll-timing, note when this poll started (elapsed since launch) so we
	// can print it below alongside the request's own round-trip duration.
	var pollStartedAt time.Duration
	if pollTiming {
		pollStartedAt = time.Since(runStart)
	}
	body, err := buildAgentToServer(pollMessage{
		id: identity{
			instanceUID: vs.instanceUID,
			hostName:    vs.hostName,
			hostIP:      vs.hostIP,
			groupID:     vs.groupID,
		},
		seq:              vs.seq,
		fullState:        fullState,
		effectiveConfig:  vs.configBody,
		remoteConfigHash: vs.configHash,
	})
	if err != nil {
		// A marshal failure is a bug in message building, not server load;
		// count it as a transport error and move on.
		logError("[%s] marshal AgentToServer failed: %v", vs.name, err)
		vs.metrics.recordTransportError(err)
		return false
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, vs.endpoint, bytes.NewReader(body))
	if err != nil {
		logError("[%s] build request failed: %v", vs.name, err)
		vs.metrics.recordTransportError(err)
		return false
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Authorization", vs.token)
	req.Header.Set("ServerName", vs.name)

	logInfo("[%s] → poll seq=%d fullState=%v token=%s ackHash=%s",
		vs.name, vs.seq, fullState, tokenTail(vs.token), hashTail(vs.configHash))

	// --max-inflight: block until a slot frees so no more than N polls are in
	// flight across the whole run. nil sem means unlimited (the default).
	if vs.sem != nil {
		select {
		case vs.sem <- struct{}{}:
		case <-ctx.Done():
			return false
		}
		defer func() { <-vs.sem }()
	}

	start := time.Now()
	resp, err := vs.client.Do(req)
	latency := time.Since(start)
	if err != nil {
		logError("[%s] poll transport error: %v", vs.name, err)
		vs.metrics.recordTransportError(err)
		return false
	}
	defer resp.Body.Close()

	if pollTiming {
		// started = elapsed since launch when this poll fired; took = its
		// round-trip (the server's per-poll latency).
		log.Printf("[%s] poll seq=%d: started +%s, took %s (status %d)",
			vs.name, vs.seq, pollStartedAt.Round(time.Millisecond), latency.Round(time.Millisecond), resp.StatusCode)
	}

	respBody, _ := io.ReadAll(resp.Body)

	nextFullState := false
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if parsed, perr := parseServerToAgent(respBody); perr == nil {
			nextFullState = parsed.reportFullState
			logInfo("[%s] ← resp status=%d reportFullState=%v offerToken=%s remoteConfig=%dB hash=%s",
				vs.name, resp.StatusCode, parsed.reportFullState,
				tokenTail(parsed.newToken), len(parsed.remoteConfigBody), hashTail(parsed.remoteConfigHash))
			vs.applyResponse(parsed)
		} else {
			logError("[%s] ← resp status=%d parse error: %v", vs.name, resp.StatusCode, perr)
		}
	} else {
		logError("[%s] ← non-2xx response status=%d body=%q", vs.name, resp.StatusCode, truncate(respBody, 200))
	}
	vs.metrics.recordResponse(resp.StatusCode, resp.Header.Get("Retry-After"), latency)
	return nextFullState
}

// applyResponse mutates the client's persistent state from a decoded response:
// it adopts a rotated token (the default→rotate handoff, counted once) and
// stores any delivered remote config in memory (counted per delivery). This is
// the whole "apply" — no collector restart, no config squashing — but the server
// sees the same token switch and hash acknowledgement a real supervisor produces.
func (vs *VirtualSupervisor) applyResponse(r serverResponse) {
	if r.newToken != "" && r.newToken != vs.token {
		vs.token = r.newToken
		vs.metrics.recordRotation()
		logInfo("[%s] ✱ adopted rotated token %s", vs.name, tokenTail(vs.token))
	}
	if len(r.remoteConfigHash) > 0 {
		vs.configBody = r.remoteConfigBody
		vs.configHash = r.remoteConfigHash
		vs.metrics.recordConfigDelivered()
		logInfo("[%s] ✱ stored remote config %dB hash=%s", vs.name, len(vs.configBody), hashTail(vs.configHash))
	}
}

// tokenTail renders the last 8 chars of a bearer's ciphertext for verbose logs —
// enough to see the token change on rotation without dumping the credential.
// Returns "-" for the empty token (no offer).
func tokenTail(t string) string {
	t = strings.TrimPrefix(t, "Bearer ")
	if t == "" {
		return "-"
	}
	if len(t) <= 8 {
		return t
	}
	return "…" + t[len(t)-8:]
}

// hashTail renders the first 4 bytes of a config hash as hex for verbose logs, or
// "-" when no config is held yet.
func hashTail(h []byte) string {
	if len(h) == 0 {
		return "-"
	}
	n := len(h)
	if n > 4 {
		n = 4
	}
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, n*2+1)
	for _, b := range h[:n] {
		out = append(out, hexdigits[b>>4], hexdigits[b&0x0f])
	}
	return string(out) + "…"
}

// truncate caps a byte slice for readable non-2xx body logging.
func truncate(b []byte, max int) string {
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return string(b)
}

// nextSleep returns the base poll interval jittered by ±jitter fraction.
func (vs *VirtualSupervisor) nextSleep() time.Duration {
	if vs.jitter <= 0 {
		return vs.pollInterval
	}
	// Uniform in [1-jitter, 1+jitter] × pollInterval.
	factor := 1 + vs.jitter*(2*vs.rng.Float64()-1)
	return time.Duration(float64(vs.pollInterval) * factor)
}
