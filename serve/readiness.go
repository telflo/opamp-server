package serve

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ReadyTimeout bounds /ready's database ping.
const ReadyTimeout = 2 * time.Second

// The readiness states.
const (
	stateStarting int32 = iota
	stateServing
	stateDraining
)

// ReadyCacheTTL is how long a /ready answer's ping holds: every /ready in
// that time, from any number of callers, is answered from it, counted from
// the time the ping began, on the injected clock. The agent port is public,
// so anyone can ask /ready as often as they like: were each ask to ping on
// the readiness pool's one connection, a flood of them would queue the
// kubelet's own probe past ReadyTimeout, and pods would flap unready.
const ReadyCacheTTL = time.Second

// readiness is the agent listener's /ready: a pod is ready only once the
// database answers, and stops being ready the moment it is told to stop, so
// the load balancer sends it nothing new while it drains. It writes no log
// line: probes come every few seconds.
//
// At most one ping is in flight: concurrent callers share it (a single
// flight), and its answer stands for ReadyCacheTTL. The ping runs on a
// context of its own (bounded by ReadyTimeout, never by the caller that
// started it, whose client may go away while others wait); a caller whose
// own request ends first is answered 503.
type readiness struct {
	ping  func(context.Context) error
	now   func() time.Time
	state atomic.Int32

	mu       sync.Mutex
	inFlight *readyPing
	// checked is the last ping's answer, from when it began (checkedAt);
	// none yet when !haveChecked.
	checked, haveChecked bool
	checkedAt            time.Time
}

// readyPing is one ping, shared by every caller waiting on it.
type readyPing struct {
	done chan struct{}
	ok   bool
}

// newReadiness is a readiness that answers 503 until serving, pinging with
// ping, on the clock now.
func newReadiness(ping func(context.Context) error, now func() time.Time) *readiness {
	return &readiness{ping: ping, now: now}
}

// serving marks the listeners open: /ready now asks the database.
func (r *readiness) serving() {
	r.state.CompareAndSwap(stateStarting, stateServing)
}

// drain marks the server stopping: /ready answers 503 from now on.
func (r *readiness) drain() {
	r.state.Store(stateDraining)
}

// databaseAnswers reports whether the database answered a ping: the last
// one's answer while it is younger than ReadyCacheTTL, else the one in
// flight's, else a new one's, which this caller runs while any others wait
// on it. A caller whose ctx ends first is told no.
func (r *readiness) databaseAnswers(ctx context.Context) bool {
	r.mu.Lock()
	now := r.now()
	if r.haveChecked && !now.Before(r.checkedAt) && now.Sub(r.checkedAt) < ReadyCacheTTL {
		ok := r.checked
		r.mu.Unlock()
		return ok
	}
	p := r.inFlight
	if p != nil {
		r.mu.Unlock()
		select {
		case <-p.done:
			return p.ok
		case <-ctx.Done():
			return false
		}
	}
	p = &readyPing{done: make(chan struct{})}
	r.inFlight = p
	r.mu.Unlock()

	// Whatever the ping does, panicking included (net/http recovers the
	// request's panic), the ping in flight is cleared and its waiters
	// answered: a ping that never finished leaves no answer cached, and
	// they are told no.
	finished := false
	defer func() {
		r.mu.Lock()
		if finished {
			r.checked, r.haveChecked, r.checkedAt = p.ok, true, now
		}
		r.inFlight = nil
		r.mu.Unlock()
		close(p.done)
	}()
	pingCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ReadyTimeout)
	defer cancel()
	p.ok = r.ping(pingCtx) == nil
	finished = true
	return p.ok
}

func (r *readiness) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ready := r.state.Load() == stateServing && r.databaseAnswers(req.Context()) && r.state.Load() == stateServing
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	text := "ready\n"
	if ready {
		w.WriteHeader(http.StatusOK)
	} else {
		text = "not ready\n"
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	if req.Method == http.MethodGet {
		_, _ = io.WriteString(w, text)
	}
}
