package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/organizations"
)

// limitsStub answers each organization's limits from a map, counting reads.
type limitsStub struct {
	mu    sync.Mutex
	rpm   map[uuid.UUID]*int32
	reads int
}

func (l *limitsStub) Limits(_ context.Context, id uuid.UUID) (organizations.Limits, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reads++
	rpm, found := l.rpm[id]
	if !found {
		return organizations.Limits{}, organizations.ErrNotFound
	}
	return organizations.Limits{RequestsPerMinute: rpm}, nil
}

func (l *limitsStub) set(id uuid.UUID, rpm *int32) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rpm[id] = rpm
}

func ptr32(v int32) *int32 { return &v }

var (
	limitedOrg = uuid.MustParse("0a0a0a0a-0000-4000-8000-00000000000a")
	defaultOrg = uuid.MustParse("0b0b0b0b-0000-4000-8000-00000000000b")
)

// poll sends one request as the organization through the limiter.
func poll(l *OrganizationLimiter, organizationID uuid.UUID) *httptest.ResponseRecorder {
	h := l.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodPost, AgentRoute, nil)
	req = req.WithContext(WithPrincipal(req.Context(), Principal{OrganizationID: organizationID, FleetID: uuid.New()}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// passes counts how many of n requests at one instant pass.
func passes(l *OrganizationLimiter, organizationID uuid.UUID, n int) int {
	passed := 0
	for range n {
		if poll(l, organizationID).Code == http.StatusOK {
			passed++
		}
	}
	return passed
}

// An organization's requests a minute are its own requests_per_minute, else
// the server's default; a minute's worth may come at once, then one per
// 60 s / rate. A refusal is 429 with a Retry-After from 30 to 60 seconds;
// one organization's refusals don't touch another's allowance.
func TestTheOrganizationsLimitIsItsOwnElseTheDefault(t *testing.T) {
	clock := newTestClock()
	stub := &limitsStub{rpm: map[uuid.UUID]*int32{limitedOrg: ptr32(5), defaultOrg: nil}}
	l := NewOrganizationLimiter(stub, 7, clock.Now, discard())
	if got := passes(l, limitedOrg, 10); got != 5 {
		t.Fatalf("the organization limited to 5 a minute passed %d of 10 at once", got)
	}
	if got := passes(l, defaultOrg, 10); got != 7 {
		t.Fatalf("the organization on the default of 7 passed %d of 10 at once", got)
	}
	rec := poll(l, limitedOrg)
	retry, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if rec.Code != http.StatusTooManyRequests || rec.Body.String() != "Too Many Requests\n" || err != nil ||
		retry < OrganizationRetryAfterMin || retry > OrganizationRetryAfterMax {
		t.Fatalf("a refusal: %d %q, Retry-After %q", rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"))
	}
	clock.Advance(12*time.Second - time.Millisecond)
	if got := passes(l, limitedOrg, 1); got != 0 {
		t.Fatal("a request passed before the next 12 s slot of 5 a minute")
	}
	clock.Advance(time.Millisecond)
	if got := passes(l, limitedOrg, 2); got != 1 {
		t.Fatalf("12 s on, %d of 2 passed; want 1", got)
	}
}

// Both caches keep a row 10 s, the bound docs/agent-endpoint.md and
// docs/decisions/0002-the-agent-tier.md promise for a revocation's or a
// limit's change to reach every pod; the tests above advance a literal
// 10 s, so a longer TTL fails them too.
func TestTheTokenAndLimitsCachesKeepARowTenSeconds(t *testing.T) {
	if TokenCacheTTL != 10*time.Second || LimitsCacheTTL != 10*time.Second {
		t.Fatalf("TokenCacheTTL %v, LimitsCacheTTL %v; want 10 s each", TokenCacheTTL, LimitsCacheTTL)
	}
}

// An organization's limits are read through a cache of 10 s by the injected
// clock: a change reaches the pod within it.
func TestTheOrganizationsLimitsAreReadThroughA10SecondCache(t *testing.T) {
	clock := newTestClock()
	stub := &limitsStub{rpm: map[uuid.UUID]*int32{limitedOrg: ptr32(1000)}}
	l := NewOrganizationLimiter(stub, 7, clock.Now, discard())
	passes(l, limitedOrg, 3)
	clock.Advance(10*time.Second - time.Nanosecond)
	passes(l, limitedOrg, 1)
	if stub.reads != 1 {
		t.Fatalf("%d reads within the cache's TTL; want 1", stub.reads)
	}
	clock.Advance(time.Nanosecond)
	passes(l, limitedOrg, 1)
	if stub.reads != 2 {
		t.Fatalf("%d reads at the TTL; want 2", stub.reads)
	}
	// A failed read is the default, and isn't cached.
	unknown := uuid.New()
	passes(l, unknown, 2)
	if stub.reads != 4 {
		t.Fatalf("%d reads after two of an unknown organization; want 4 (not cached)", stub.reads)
	}
}

// The per-collector guard lets a collector make 20 requests at once, then
// one a second (60 a minute); its key is the organization and the instance
// uid, so another collector, or the same uid in another organization, keeps
// its own allowance. Refuse answers 429 with Retry-After 60.
func TestTheCollectorGuardAllowsABurstOf20Then60AMinute(t *testing.T) {
	clock := newTestClock()
	g := NewCollectorGuard(DefaultCollectorRequestsPerMinute, clock.Now, discard())
	uid := uuid.New()
	passed := 0
	for range 30 {
		if g.Allow(limitedOrg, uid) {
			passed++
		}
	}
	if passed != CollectorBurst {
		t.Fatalf("%d of 30 at once passed; want the burst of %d", passed, CollectorBurst)
	}
	if !g.Allow(limitedOrg, uuid.New()) || !g.Allow(defaultOrg, uid) {
		t.Fatal("another collector, or the uid in another organization, was refused")
	}
	clock.Advance(time.Second - time.Millisecond)
	if g.Allow(limitedOrg, uid) {
		t.Fatal("a request passed before its second")
	}
	clock.Advance(time.Millisecond)
	if !g.Allow(limitedOrg, uid) || g.Allow(limitedOrg, uid) {
		t.Fatal("a second on, not exactly one passed")
	}
	rec := httptest.NewRecorder()
	g.Refuse(rec, httptest.NewRequest(http.MethodPost, AgentRoute, nil))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "60" {
		t.Fatalf("Refuse: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

// A limiter keeps one value per key and sweeps the keys whose allowance is
// full again once a minute by the injected clock.
func TestALimiterSweepsDrainedKeysEveryMinute(t *testing.T) {
	clock := newTestClock()
	g := newGCRA[uuid.UUID]("test", MaxLimiterKeys, clock.Now, discard())
	for range 100 {
		g.allow(uuid.New(), 60, 20)
	}
	if g.keys() != 100 {
		t.Fatalf("%d keys after 100", g.keys())
	}
	busy := uuid.New()
	for range 30 {
		g.allow(busy, 1, 30) // its allowance runs 30 minutes ahead
	}
	clock.Advance(LimiterSweepInterval - time.Nanosecond)
	g.allow(uuid.New(), 60, 20)
	if g.keys() != 102 {
		t.Fatalf("%d keys before the minute; want 102 (no sweep yet)", g.keys())
	}
	clock.Advance(time.Nanosecond)
	g.allow(uuid.New(), 60, 20)
	if g.keys() != 3 {
		t.Fatalf("%d keys after the sweep; want 3 (the busy key, the one before and the new one)", g.keys())
	}
}

// Past its key bound a limiter lets a new key's requests pass untracked,
// and says so in one line a minute.
func TestALimiterPastItsBoundPassesNewKeysUntrackedAndLogsOnceAMinute(t *testing.T) {
	clock := newTestClock()
	logs := &strings.Builder{}
	g := newGCRA[int]("test", 3, clock.Now, captureLogs(logs))
	// Three keys whose allowance runs 100 minutes ahead, so no sweep frees
	// them during the test.
	for key := range 3 {
		for range 100 {
			g.allow(key, 1, 100)
		}
	}
	for range 10 {
		if !g.allow(99, 1, 1) {
			t.Fatal("an untracked key's request was refused")
		}
	}
	if g.allow(0, 1, 100) {
		t.Fatal("a tracked key past its allowance passed")
	}
	if g.keys() != 3 {
		t.Fatalf("%d keys under a bound of 3", g.keys())
	}
	if lines := logLines(t, logs); len(lines) != 1 || lines[0]["max_keys"] != float64(3) {
		t.Fatalf("log lines within the minute: %v", lines)
	}
	clock.Advance(30 * time.Second)
	g.allow(98, 1, 1)
	clock.Advance(30 * time.Second)
	g.allow(97, 1, 1)
	lines := logLines(t, logs)
	if len(lines) != 2 || lines[1]["untracked_since_last_log"] != float64(11) {
		t.Fatalf("log lines a minute on: %v", lines)
	}
}

// The organization's refusals are logged once a minute with their count,
// naming the organization and nothing of the request.
func TestTheOrganizationsRefusalsAreLoggedOnceAMinute(t *testing.T) {
	clock := newTestClock()
	logs := &strings.Builder{}
	stub := &limitsStub{rpm: map[uuid.UUID]*int32{limitedOrg: ptr32(1)}}
	l := NewOrganizationLimiter(stub, 7, clock.Now, captureLogs(logs))
	passes(l, limitedOrg, 50)
	lines := logLines(t, logs)
	if len(lines) != 1 || lines[0]["organization_id"] != limitedOrg.String() || lines[0]["outcome"] != outcomeRateLimited {
		t.Fatalf("log lines: %v", lines)
	}
	clock.Advance(time.Minute)
	passes(l, limitedOrg, 3)
	lines = logLines(t, logs)
	if len(lines) != 2 || lines[1]["refused_since_last_log"] != float64(49) {
		t.Fatalf("log lines a minute on: %v", lines)
	}
}

// The collector guard keeps at most MaxCollectorKeysPerOrganization keys of
// one organization: past its share, the organization's new instance uids
// are refused (a line a minute says so), its tracked collectors and every
// other organization's still pass, and a sweep frees the share. The bound
// across organizations stays the backstop: past it new keys pass untracked.
func TestTheCollectorGuardKeepsEachOrganizationToItsShare(t *testing.T) {
	clock := newTestClock()
	logs := &strings.Builder{}
	g := NewCollectorGuard(DefaultCollectorRequestsPerMinute, clock.Now, captureLogs(logs))
	tracked := uuid.New()
	if !g.Allow(limitedOrg, tracked) {
		t.Fatal("the first collector was refused")
	}
	for i := 1; i < MaxCollectorKeysPerOrganization; i++ {
		if !g.Allow(limitedOrg, uuid.New()) {
			t.Fatalf("collector %d of the organization's share was refused", i+1)
		}
	}
	for range 3 {
		if g.Allow(limitedOrg, uuid.New()) {
			t.Fatal("a new collector past the organization's share passed")
		}
	}
	if !g.Allow(limitedOrg, tracked) {
		t.Fatal("a tracked collector of the full organization was refused")
	}
	if !g.Allow(defaultOrg, uuid.New()) {
		t.Fatal("another organization's new collector was refused")
	}
	if n := g.gcra.groupKeys(limitedOrg); n != MaxCollectorKeysPerOrganization {
		t.Fatalf("the organization holds %d keys", n)
	}
	lines := logLines(t, logs)
	if len(lines) != 1 || lines[0]["organization_id"] != limitedOrg.String() || lines[0]["refused_since_last_log"] != float64(1) {
		t.Fatalf("the share's lines: %v", lines)
	}
	clock.Advance(LimiterSweepInterval)
	if !g.Allow(limitedOrg, uuid.New()) {
		t.Fatal("after a sweep, the organization's new collector was refused")
	}
	if n := g.gcra.groupKeys(limitedOrg); n != 1 {
		t.Fatalf("after the sweep the organization holds %d keys, want 1", n)
	}
	if lines := logLines(t, logs); len(lines) != 1 {
		t.Fatalf("lines after the sweep: %v", lines)
	}

	// The backstop: 3 keys in all, 2 an organization.
	logs.Reset()
	small := newCollectorGuard(DefaultCollectorRequestsPerMinute, 3, 2, clock.Now, captureLogs(logs))
	third := uuid.New()
	for _, c := range []struct {
		org  uuid.UUID
		pass bool
	}{{limitedOrg, true}, {limitedOrg, true}, {limitedOrg, false}, {defaultOrg, true}, {third, true}, {third, true},
		// Full across organizations: an organization at its share still pays.
		{limitedOrg, false}} {
		if small.Allow(c.org, uuid.New()) != c.pass {
			t.Fatalf("organization %s: want pass %v", c.org, c.pass)
		}
	}
	if small.gcra.keys() != 3 || small.gcra.groupKeys(third) != 0 {
		t.Fatalf("%d keys, the third organization's %d", small.gcra.keys(), small.gcra.groupKeys(third))
	}
	if !strings.Contains(logs.String(), "pass untracked") {
		t.Fatalf("no line says the guard is full: %s", logs.String())
	}
}

// The share's refusal lines are sampled per outcome and organization, as
// the 403s are, so one organization at its share never hides another's
// line. Two organizations over their shares in one minute are a line each;
// more refusals in that minute add none; a minute on, another
// organization's refusal sweeps, and each of the two gets a line with its
// trailing count.
func TestTheGuardsShareLinesArePerOrganization(t *testing.T) {
	clock := newTestClock()
	logs := &strings.Builder{}
	g := newCollectorGuard(DefaultCollectorRequestsPerMinute, 100, 2, clock.Now, captureLogs(logs))
	overShare := func(organization uuid.UUID, refusals int) {
		t.Helper()
		for range 2 {
			if !g.Allow(organization, uuid.New()) {
				t.Fatal("a collector within its organization's share was refused")
			}
		}
		for range refusals {
			if g.Allow(organization, uuid.New()) {
				t.Fatal("a new collector past its organization's share passed")
			}
		}
	}
	first, second, third := uuid.New(), uuid.New(), uuid.New()
	overShare(first, 3)
	overShare(second, 2)
	lines := logLines(t, logs)
	if len(lines) != 2 || lines[0]["organization_id"] != first.String() || lines[1]["organization_id"] != second.String() ||
		lines[0]["refused_since_last_log"] != float64(1) || lines[1]["refused_since_last_log"] != float64(1) {
		t.Fatalf("two organizations over their shares in one minute logged %v; want a line naming each", lines)
	}
	clock.Advance(LimiterSweepInterval)
	overShare(third, 1)
	trailing := map[any]any{}
	for _, line := range logLines(t, logs)[2:] {
		if line["msg"] != overShareMessage || line["outcome"] != outcomeCollectorLimited {
			t.Fatalf("a line a minute on: %v", line)
		}
		trailing[line["organization_id"]] = line["refused_since_last_log"]
	}
	if len(trailing) != 3 || trailing[first.String()] != float64(2) || trailing[second.String()] != float64(1) ||
		trailing[third.String()] != float64(1) {
		t.Fatalf("a minute on, the counts by organization %v; want the first's 2 and the second's 1 flushed, the third's 1", trailing)
	}
}

// slowLimits answers an organization's limits after delay on the test's
// clock (a slow read), counting reads.
type slowLimits struct {
	clock *testClock
	delay time.Duration
	stub  *limitsStub
}

func (s *slowLimits) Limits(ctx context.Context, id uuid.UUID) (organizations.Limits, error) {
	l, err := s.stub.Limits(ctx, id)
	s.clock.Advance(s.delay)
	return l, err
}

// As the token cache does, the limits cache counts an entry's age from the
// time taken before its read, so a change committed just after a slow
// read's snapshot reaches the pod LimitsCacheTTL after that read, not after
// the slow answer came.
func TestALimitChangeDuringASlowReadIsReadAgainWithinTheTTLOfTheRead(t *testing.T) {
	clock := newTestClock()
	stub := &limitsStub{rpm: map[uuid.UUID]*int32{limitedOrg: ptr32(1000)}}
	l := NewOrganizationLimiter(&slowLimits{clock: clock, delay: 4 * time.Second, stub: stub}, 7, clock.Now, discard())
	readAt := clock.Now()
	passes(l, limitedOrg, 1)
	clock.Advance(readAt.Add(10*time.Second - time.Nanosecond).Sub(clock.Now()))
	passes(l, limitedOrg, 1)
	if stub.reads != 1 {
		t.Fatalf("%d reads just under the TTL from the read; want 1 (cached)", stub.reads)
	}
	clock.Advance(time.Nanosecond)
	passes(l, limitedOrg, 1)
	if stub.reads != 2 {
		t.Fatalf("%d reads at the TTL from the read; want 2 (the entry aged from the read, not the answer)", stub.reads)
	}
}
