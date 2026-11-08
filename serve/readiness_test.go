package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingPing is a database ping that waits until released (or its
// context ends, failing), counting the pings and remembering whether one's
// context was cancelled.
type blockingPing struct {
	calls     atomic.Int32
	started   chan struct{}
	release   chan struct{}
	err       error
	cancelled atomic.Bool
}

func newBlockingPing() *blockingPing {
	return &blockingPing{started: make(chan struct{}, 1), release: make(chan struct{})}
}

func (p *blockingPing) ping(ctx context.Context) error {
	p.calls.Add(1)
	select {
	case p.started <- struct{}{}:
	default:
	}
	select {
	case <-p.release:
	case <-ctx.Done(): // a ping on a request that ended: never a hang
	}
	if ctx.Err() != nil {
		p.cancelled.Store(true)
		return ctx.Err()
	}
	return p.err
}

// readyCode is /ready's status through r for a request on ctx.
func readyCode(r *readiness, ctx context.Context) int {
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/ready", nil))
	return rec.Code
}

// /ready runs at most one ping at a time, every concurrent caller sharing
// its answer, and keeps that answer ReadyCacheTTL by the injected clock,
// whoever asks and however often. Two thousand callers at once while the
// ping is in flight make one ping and are all answered 200; so are the
// callers after it, until the clock moves past the TTL. The ping isn't the
// caller's: the request that started it may go away (its context cancelled)
// without failing the ping or the others; a caller whose own request ends
// while it waits is answered 503 at once. A failed ping's 503 is kept the
// TTL too, and draining answers 503 without a ping.
func TestReadyPingsOnceForEveryCallerAndKeepsTheAnswerASecond(t *testing.T) {
	clock := newTestClock()
	p := newBlockingPing()
	r := newReadiness(p.ping, clock.Now)
	r.serving()

	leaderCtx, leaderGone := context.WithCancel(context.Background())
	leader := make(chan int, 1)
	go func() { leader <- readyCode(r, leaderCtx) }()
	<-p.started
	leaderGone() // the request that started the ping goes away

	const callers = 2000
	codes := make(chan int, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- readyCode(r, context.Background())
		}()
	}
	impatientCtx, impatientGone := context.WithCancel(context.Background())
	impatient := make(chan int, 1)
	go func() { impatient <- readyCode(r, impatientCtx) }()
	impatientGone()
	if got := <-impatient; got != http.StatusServiceUnavailable {
		t.Fatalf("a caller whose request ended while waiting: %d", got)
	}
	close(p.release)
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Fatalf("a caller during the ping: %d", code)
		}
	}
	<-leader // answered too, though no one reads it
	if n := p.calls.Load(); n != 1 || p.cancelled.Load() {
		t.Fatalf("%d pings for %d callers (cancelled %v); want 1, not cancelled", n, callers+2, p.cancelled.Load())
	}

	for range 100 {
		if got := readyCode(r, context.Background()); got != http.StatusOK {
			t.Fatalf("within the TTL: %d", got)
		}
	}
	if n := p.calls.Load(); n != 1 {
		t.Fatalf("%d pings within the TTL; want 1", n)
	}

	p.err = failingPing(context.Background())
	clock.Advance(ReadyCacheTTL)
	if got := readyCode(r, context.Background()); got != http.StatusServiceUnavailable || p.calls.Load() != 2 {
		t.Fatalf("past the TTL, the database down: %d after %d pings", got, p.calls.Load())
	}
	p.err = nil
	clock.Advance(ReadyCacheTTL - 1)
	if got := readyCode(r, context.Background()); got != http.StatusServiceUnavailable || p.calls.Load() != 2 {
		t.Fatalf("the failure within its TTL: %d after %d pings", got, p.calls.Load())
	}
	clock.Advance(1)
	if got := readyCode(r, context.Background()); got != http.StatusOK || p.calls.Load() != 3 {
		t.Fatalf("past the failure's TTL, the database back: %d after %d pings", got, p.calls.Load())
	}
	r.drain()
	if got := readyCode(r, context.Background()); got != http.StatusServiceUnavailable || p.calls.Load() != 3 {
		t.Fatalf("draining, the answer cached: %d after %d pings", got, p.calls.Load())
	}
}

// A ping that panics (net/http recovers a request's panic, so the pod
// lives on) still clears the ping in flight and answers whoever waits on
// it (told no), caching nothing: every later /ready is answered, the next
// by a new ping. A panicked ping left in flight would never be done, and
// every later /ready would wait on it until its request ended: 503 for
// good.
func TestAPanickingReadyPingDoesNotWedgeReadiness(t *testing.T) {
	clock := newTestClock()
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	r := newReadiness(func(context.Context) error {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			panic("the ping panicked")
		}
		return nil
	}, clock.Now)
	r.serving()

	leader := make(chan any, 1)
	go func() {
		defer func() { leader <- recover() }()
		readyCode(r, context.Background())
	}()
	<-started
	// A caller during the ping (it waits on it) or just after it (it pings
	// again): answered either way, 503 or 200, never left waiting.
	waiter := make(chan int, 1)
	go func() { waiter <- readyCode(r, context.Background()) }()
	close(release)
	if got := <-leader; got != "the ping panicked" {
		t.Fatalf("the leader recovered %v", got)
	}
	guard := time.NewTimer(5 * time.Second) // a hang guard
	defer guard.Stop()
	select {
	case got := <-waiter:
		if got != http.StatusServiceUnavailable && got != http.StatusOK {
			t.Fatalf("a caller during the panicked ping: %d", got)
		}
	case <-guard.C:
		t.Fatal("a caller during the panicked ping was never answered")
	}
	answered := make(chan int, 1)
	go func() { answered <- readyCode(r, context.Background()) }()
	select {
	case got := <-answered:
		if got != http.StatusOK || calls.Load() != 2 {
			t.Fatalf("after the panic: %d after %d pings; want 200 from a second ping", got, calls.Load())
		}
	case <-guard.C:
		t.Fatal("a /ready after the panicked ping was never answered")
	}
}
