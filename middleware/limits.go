package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/internal/lru"
	"github.com/telflo/opamp-server/store/organizations"
)

// The agent side's limits, each counted per pod: an organization's
// requests a minute (its own requests_per_minute, else the server's
// default), a per-collector guard, and the concurrency cap
// (concurrency_limit.go).
const (
	// DefaultRequestsPerMinute is OPAMP_DEFAULT_REQUESTS_PER_MINUTE's
	// default: an organization's requests a minute to each pod when it has
	// none set (about 6,000 collectors at the 30 s heartbeat).
	DefaultRequestsPerMinute = 12_000
	// DefaultCollectorRequestsPerMinute is
	// OPAMP_COLLECTOR_REQUESTS_PER_MINUTE's default: one collector's
	// requests a minute (a supervisor's steady 2, its 6 on connecting, 12 in
	// a crash loop, all well under it).
	DefaultCollectorRequestsPerMinute = 60
	// CollectorBurst is how many requests a collector may make at once
	// before its guard counts the rate.
	CollectorBurst = 20
	// MaxLimiterKeys bounds each limiter's keys: past it, a new key passes
	// untracked, and one line a minute says so.
	MaxLimiterKeys = 200_000
	// MaxCollectorKeysPerOrganization bounds the collector guard's keys of
	// one organization: instance uids are the caller's to choose, so
	// without it one organization could fill the guard's MaxLimiterKeys and
	// leave every other's collectors untracked. Past its share, an
	// organization's new instance uids are refused (429) until a sweep
	// frees some; only that organization pays.
	MaxCollectorKeysPerOrganization = 20_000
	// LimiterSweepInterval is how often, by the injected clock, a limiter
	// drops keys whose allowance is full again (they hold nothing a new key
	// wouldn't).
	LimiterSweepInterval = time.Minute
	// limiterLogInterval spaces a limiter's log lines: a saturated
	// organization's refusals are one line a minute with their count, not a
	// line each.
	limiterLogInterval = time.Minute
	// LimitsCacheTTL and LimitsCacheEntries bound the organizations' limits
	// cache: a new requests_per_minute reaches each pod within
	// LimitsCacheTTL.
	LimitsCacheTTL     = 10 * time.Second
	LimitsCacheEntries = 10_000
)

// gcra is the generic cell rate algorithm: one value per key, its
// theoretical arrival time (TAT). A request at now passes when the TAT is
// within the burst's tolerance of now, and moves the TAT one interval on.
// Keys whose TAT has passed hold nothing a new key wouldn't, so a sweep
// every LimiterSweepInterval drops them; at most maxKeys are kept, and,
// when groupOf is set, at most maxPerGroup of one group's (its share: a new
// key of a group at its share is refused, overShare).
type gcra[K comparable] struct {
	name    string
	maxKeys int
	now     func() time.Time
	logger  *slog.Logger

	// groupOf, when set, is a key's group, of which at most maxPerGroup
	// keys are tracked.
	groupOf     func(K) uuid.UUID
	maxPerGroup int

	mu        sync.Mutex
	tat       map[K]time.Time
	groups    map[uuid.UUID]int
	lastSweep time.Time

	// untracked counts the requests passed untracked since the last line
	// saying so; refused counts refusals since the last line.
	untracked, refused         int
	untrackedSince, refusedLog time.Time
}

func newGCRA[K comparable](name string, maxKeys int, now func() time.Time, logger *slog.Logger) *gcra[K] {
	return &gcra[K]{name: name, maxKeys: maxKeys, now: now, logger: logger, tat: map[K]time.Time{}, groups: map[uuid.UUID]int{}}
}

// verdict is a gcra's answer to a request.
type verdict int

const (
	// pass: the request passes (tracked, or untracked past maxKeys).
	pass verdict = iota
	// overRate: the key is past its allowance.
	overRate
	// overShare: a new key of a group already holding maxPerGroup keys.
	overShare
)

// allow reports whether key's request passes at perMinute requests a minute
// with a burst of burst.
func (g *gcra[K]) allow(key K, perMinute, burst int) bool {
	return g.decide(key, perMinute, burst) == pass
}

// decide is allow's verdict, telling a key past its rate from a new key
// refused for its group's share.
func (g *gcra[K]) decide(key K, perMinute, burst int) verdict {
	perMinute, burst = max(perMinute, 1), max(burst, 1)
	interval := time.Minute / time.Duration(perMinute)
	if interval <= 0 {
		interval = time.Nanosecond
	}
	tolerance := interval * time.Duration(burst-1)
	now := g.now()

	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweep(now)
	tat, tracked := g.tat[key]
	if !tracked {
		if g.groupOf != nil && g.groups[g.groupOf(key)] >= g.maxPerGroup {
			return overShare
		}
		if len(g.tat) >= g.maxKeys {
			g.untracked++
			if now.Sub(g.untrackedSince) >= limiterLogInterval {
				g.logger.Warn("a limiter is full: requests of new keys pass untracked",
					"limiter", g.name, "max_keys", g.maxKeys, "untracked_since_last_log", g.untracked)
				g.untracked, g.untrackedSince = 0, now
			}
			return pass
		}
		tat = now
	}
	if tat.Before(now) {
		tat = now
	}
	if tat.Sub(now) > tolerance {
		return overRate
	}
	if !tracked && g.groupOf != nil {
		g.groups[g.groupOf(key)]++
	}
	g.tat[key] = tat.Add(interval)
	return pass
}

// sweep drops the keys whose TAT has passed, once per LimiterSweepInterval;
// the caller holds mu.
func (g *gcra[K]) sweep(now time.Time) {
	if g.lastSweep.IsZero() {
		g.lastSweep = now
		return
	}
	if now.Sub(g.lastSweep) < LimiterSweepInterval {
		return
	}
	for key, tat := range g.tat {
		if !tat.After(now) {
			delete(g.tat, key)
			if g.groupOf != nil {
				group := g.groupOf(key)
				if g.groups[group]--; g.groups[group] <= 0 {
					delete(g.groups, group)
				}
			}
		}
	}
	g.lastSweep = now
}

// noteRefusal reports whether this refusal should be logged: the first
// since limiterLogInterval passed, with the count since the last line.
func (g *gcra[K]) noteRefusal() (log bool, count int) {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refused++
	if now.Sub(g.refusedLog) < limiterLogInterval {
		return false, 0
	}
	count, g.refused, g.refusedLog = g.refused, 0, now
	return true, count
}

// keys is how many keys the limiter holds: for tests.
func (g *gcra[K]) keys() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.tat)
}

// groupKeys is how many keys of group the limiter holds: for tests.
func (g *gcra[K]) groupKeys(group uuid.UUID) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.groups[group]
}

// LimitsReader is what the organizations' limiter reads:
// organizations.Store's Limits.
type LimitsReader interface {
	Limits(ctx context.Context, id uuid.UUID) (organizations.Limits, error)
}

// OrganizationLimiter is the per-organization rate limit: each
// organization's agent requests a minute to this pod, its own
// requests_per_minute (read through a cache of LimitsCacheTTL), else the
// server's default. Across pods the limit is the limit × the number of
// pods; a fleet-wide limit is the load balancer's. A refusal answers 429
// with a Retry-After of 30 to 60 seconds, jittered.
type OrganizationLimiter struct {
	limits     LimitsReader
	cache      *lru.Cache[uuid.UUID, organizations.Limits]
	defaultRPM int
	gcra       *gcra[uuid.UUID]
	now        func() time.Time
	logger     *slog.Logger
}

// NewOrganizationLimiter returns the limiter reading limits, with
// defaultRPM for an organization without its own, on the clock now.
func NewOrganizationLimiter(limits LimitsReader, defaultRPM int, now func() time.Time, logger *slog.Logger) *OrganizationLimiter {
	return &OrganizationLimiter{
		limits: limits,
		cache: lru.New(lru.Options[uuid.UUID, organizations.Limits]{
			MaxEntries: LimitsCacheEntries, TTL: LimitsCacheTTL, Now: now,
		}),
		defaultRPM: defaultRPM,
		gcra:       newGCRA[uuid.UUID]("organization", MaxLimiterKeys, now, logger),
		now:        now,
		logger:     logger,
	}
}

// requestsPerMinute is the organization's limit: its own, else the
// default. A failed read is the default, not cached (the request's own
// database work answers the failure).
func (l *OrganizationLimiter) requestsPerMinute(ctx context.Context, organizationID uuid.UUID) int {
	limits, ok := l.cache.Get(organizationID)
	if !ok {
		// The time is taken before the read, and the limits cached as of it
		// (PutAt, as the token cache does): a change committed while a slow
		// read ran reaches this pod within LimitsCacheTTL of that commit.
		readAt := l.now()
		// Within DatabaseDeadline of its own: the limiter runs before the
		// body is read, outside the stack's Deadline (see AgentStack).
		readCtx, cancel := context.WithTimeout(ctx, DatabaseDeadline)
		read, err := l.limits.Limits(readCtx, organizationID)
		cancel()
		if err != nil {
			return l.defaultRPM
		}
		limits = read
		l.cache.PutAt(organizationID, limits, readAt)
	}
	if limits.RequestsPerMinute != nil && *limits.RequestsPerMinute >= 1 {
		return int(*limits.RequestsPerMinute)
	}
	return l.defaultRPM
}

// Allow reports whether the organization's request passes now. The burst
// is a minute's worth: an organization's collectors may all poll at once.
func (l *OrganizationLimiter) Allow(ctx context.Context, organizationID uuid.UUID) bool {
	rpm := l.requestsPerMinute(ctx, organizationID)
	return l.gcra.allow(organizationID, rpm, rpm)
}

// Middleware is the limiter as a Middleware, after the authentication and
// before the body is read (it needs only the Principal, so a request over
// its organization's rate is refused before its body is waited for): it
// reads the request's Principal (a request without one passes, which only a
// misordered chain would send).
func (l *OrganizationLimiter) Middleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok || l.Allow(r.Context(), p.OrganizationID) {
				next.ServeHTTP(w, r)
				return
			}
			setOutcome(r.Context(), outcomeRateLimited)
			if log, count := l.gcra.noteRefusal(); log {
				l.logger.Warn("agent requests refused: the organization is over its requests a minute",
					"route", AgentRoute, "outcome", outcomeRateLimited,
					"organization_id", p.OrganizationID.String(), "refused_since_last_log", count)
			}
			WriteTooManyRequests(w, jitter(OrganizationRetryAfterMin, OrganizationRetryAfterMax))
		})
	}
}

// collectorKey is a collector's guard's key: its organization and the
// instance uid it reports.
type collectorKey struct {
	organizationID, instanceUID uuid.UUID
}

// CollectorGuard is the per-collector guard: perMinute requests a minute
// per collector, with a burst of CollectorBurst, keyed by the organization
// and the collector's instance uid. Where the request carries the instance
// uid in its InstanceUIDHeader (the supervisor sends it on every request),
// the stack checks the guard by it (Middleware), before the organization's
// rate, so a runaway collector's refused requests never spend its
// organization's rate (the handler then refuses a body whose instance uid
// isn't the header's). Without the header, the handler calls Allow once it
// has decoded the body. A refusal is answered by Refuse: 429 with a
// Retry-After of 60 seconds.
//
// It tracks at most MaxCollectorKeysPerOrganization keys of one
// organization: an organization's new instance uid past that share is
// refused like a collector over its rate, and a line a minute says so; the
// other organizations' collectors are untouched. The MaxLimiterKeys bound
// across organizations stays as the backstop (new keys pass untracked past
// it, a line a minute).
type CollectorGuard struct {
	perMinute int
	gcra      *gcra[collectorKey]
	logger    *slog.Logger
	// invalidHeader spaces the log lines of requests refused for an
	// InstanceUIDHeader that isn't a UUID.
	invalidHeader *RefusalSampler
	// overShare spaces the share's refusal lines: a line a minute per
	// outcome and organization, with the count.
	overShare *ForbiddenSampler
}

// NewCollectorGuard returns the guard at perMinute requests a minute, on
// the clock now.
func NewCollectorGuard(perMinute int, now func() time.Time, logger *slog.Logger) *CollectorGuard {
	return newCollectorGuard(perMinute, MaxLimiterKeys, MaxCollectorKeysPerOrganization, now, logger)
}

// newCollectorGuard is NewCollectorGuard with its bounds: tests pass small
// ones.
func newCollectorGuard(perMinute, maxKeys, maxPerOrganization int, now func() time.Time, logger *slog.Logger) *CollectorGuard {
	g := newGCRA[collectorKey]("collector", maxKeys, now, logger)
	g.groupOf = func(k collectorKey) uuid.UUID { return k.organizationID }
	g.maxPerGroup = maxPerOrganization
	return &CollectorGuard{perMinute: perMinute, gcra: g, logger: logger, invalidHeader: NewRefusalSampler(now),
		overShare: NewForbiddenSampler(now, func(f ForbiddenFlush) {
			logger.Warn(overShareMessage, append([]any{"route", AgentRoute, "max_keys_per_organization", maxPerOrganization}, f.Attrs()...)...)
		})}
}

// overShareMessage is the line of the refusals of an organization's new
// collectors past its share of the guard.
const overShareMessage = "an organization's collectors hold its share of the collector guard: its new collectors are refused"

// InstanceUIDHeader is the header the OpAMP supervisor sends its instance
// uid in, as a UUID's text, on every request.
const InstanceUIDHeader = "OpAMP-Instance-Uid"

// outcomeInvalidInstanceUIDHeader: an InstanceUIDHeader that isn't one
// UUID, refused 400 before the body is read.
const outcomeInvalidInstanceUIDHeader = "invalid_instance_uid_header"

// HeaderInstanceUID is r's InstanceUIDHeader: present reports whether it
// has one, and valid whether it is exactly one value that parses as a
// UUID, then uid.
func HeaderInstanceUID(r *http.Request) (uid uuid.UUID, present, valid bool) {
	values := r.Header.Values(InstanceUIDHeader)
	if len(values) == 0 {
		return uuid.Nil, false, false
	}
	if len(values) != 1 {
		return uuid.Nil, true, false
	}
	uid, err := uuid.Parse(strings.TrimSpace(values[0]))
	return uid, true, err == nil
}

// guardedKey keys, in a request's context, the instance uid the guard's
// Middleware checked it by.
type guardedKey struct{}

// GuardedInstanceUID is the instance uid the guard's Middleware checked the
// request by (its InstanceUIDHeader), and whether it did: the handler
// then doesn't check the guard again.
func GuardedInstanceUID(ctx context.Context) (uuid.UUID, bool) {
	uid, ok := ctx.Value(guardedKey{}).(uuid.UUID)
	return uid, ok
}

// Middleware is the guard as a Middleware, after the authentication and
// before the organization's rate (see AgentStack): a request carrying the
// InstanceUIDHeader is checked by it (the Principal's organization and the
// header's uid), refused 429 past its collector's allowance without
// spending its organization's rate, and refused 400 when the header isn't
// one UUID (a line a minute with the count). A request without the header
// passes to the handler, which checks the guard by the body's instance uid;
// so does a request without a Principal, which only a misordered chain
// would send.
func (g *CollectorGuard) Middleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			uid, present, valid := HeaderInstanceUID(r)
			switch {
			case !ok || !present:
				next.ServeHTTP(w, r)
			case !valid:
				setOutcome(r.Context(), outcomeInvalidInstanceUIDHeader)
				if log, count := g.invalidHeader.Note(outcomeInvalidInstanceUIDHeader); log {
					g.logger.Warn("agent requests refused: the instance uid header isn't a UUID",
						"route", AgentRoute, "status", http.StatusBadRequest, "outcome", outcomeInvalidInstanceUIDHeader,
						"organization_id", p.OrganizationID.String(), "fleet_id", p.FleetID.String(),
						"refused_since_last_log", count)
				}
				WriteBadRequest(w)
			case !g.Allow(p.OrganizationID, uid):
				g.Refuse(w, r)
			default:
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), guardedKey{}, uid)))
			}
		})
	}
}

// Allow reports whether the collector's request passes now.
func (g *CollectorGuard) Allow(organizationID, instanceUID uuid.UUID) bool {
	switch g.gcra.decide(collectorKey{organizationID, instanceUID}, g.perMinute, CollectorBurst) {
	case pass:
		return true
	case overShare:
		g.noteOverShare(organizationID)
	}
	return false
}

// noteOverShare logs a share's refusal: a line a minute at most per outcome
// and organization, with the count since the last (so one organization at
// its share never hides another's line).
func (g *CollectorGuard) noteOverShare(organizationID uuid.UUID) {
	if log, count, full := g.overShare.Note(outcomeCollectorLimited, organizationID); log {
		g.logger.Warn(overShareMessage, append([]any{"route", AgentRoute, "outcome", outcomeCollectorLimited,
			"organization_id", organizationID.String(), "max_keys_per_organization", g.gcra.maxPerGroup},
			ForbiddenCountAttrs(count, full)...)...)
	}
}

// Refuse answers a request Allow refused: 429, Retry-After 60, the span's
// outcome, and a log line a minute at most with the refusals' count.
func (g *CollectorGuard) Refuse(w http.ResponseWriter, r *http.Request) {
	setOutcome(r.Context(), outcomeCollectorLimited)
	if log, count := g.gcra.noteRefusal(); log {
		attrs := []any{"route", AgentRoute, "outcome", outcomeCollectorLimited, "refused_since_last_log", count}
		if p, ok := PrincipalFrom(r.Context()); ok {
			attrs = append(attrs, "organization_id", p.OrganizationID.String(), "fleet_id", p.FleetID.String())
		}
		g.logger.Warn("agent requests refused: a collector is over its requests a minute", attrs...)
	}
	WriteTooManyRequests(w, CollectorRetryAfter)
}
