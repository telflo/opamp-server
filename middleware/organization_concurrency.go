package middleware

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

// DefaultMaxConcurrentRequestsPerOrganization is
// OPAMP_MAX_CONCURRENT_REQUESTS_PER_ORGANIZATION's default: the most agent
// requests of one organization a pod handles at once, a quarter of the
// pod's default 100.
const DefaultMaxConcurrentRequestsPerOrganization = 25

// OrganizationConcurrency bounds how many requests of one organization a
// pod handles at once. The pod's ConcurrencyLimiter bounds them all
// together; without this, one organization's credential (any fleet's
// default token never expires) could hold every slot of the pod with
// requests whose bodies it never finishes sending, and every other
// organization's polls would be shed. With it, an organization holds at
// most max slots, the rest stay the others'.
//
// It runs after the authentication (it needs the Principal) and before the
// body is read: a request over its organization's share answers 429 at
// once, with the shed's jittered Retry-After of 3 to 8 seconds (the
// overload is transient, as the pod's shed is), nothing of its body read.
// Its refusals are logged at most once a second, with their count since the
// last line and the organization of the refusal the line is written for.
// Its map holds only organizations with a request in flight, so it is
// bounded by the pod's own concurrency cap.
type OrganizationConcurrency struct {
	max    int
	now    func() time.Time
	logger *slog.Logger

	mu       sync.Mutex
	inFlight map[uuid.UUID]int
	// refused counts the refusals since the last line, lastLog its time.
	refused int
	lastLog time.Time
	logged  bool
}

// NewOrganizationConcurrency returns the limiter at limit requests of one
// organization in flight (at least 1), logging on the clock now.
func NewOrganizationConcurrency(limit int, now func() time.Time, logger *slog.Logger) *OrganizationConcurrency {
	return &OrganizationConcurrency{max: max(limit, 1), now: now, logger: logger, inFlight: map[uuid.UUID]int{}}
}

// acquire takes one of the organization's slots, reporting whether one was
// free.
func (c *OrganizationConcurrency) acquire(organizationID uuid.UUID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inFlight[organizationID] >= c.max {
		return false
	}
	c.inFlight[organizationID]++
	return true
}

// release gives the organization's slot back.
func (c *OrganizationConcurrency) release(organizationID uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inFlight[organizationID]--; c.inFlight[organizationID] <= 0 {
		delete(c.inFlight, organizationID)
	}
}

// InFlight is how many of the organization's requests hold a slot: for the
// tests and the admission view.
func (c *OrganizationConcurrency) InFlight(organizationID uuid.UUID) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inFlight[organizationID]
}

// noteRefusal counts a refusal and reports whether to log it: the first,
// then the first once shedLogInterval has passed, with the count since the
// last line.
func (c *OrganizationConcurrency) noteRefusal() (log bool, count int) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refused++
	if c.logged && now.Sub(c.lastLog) < shedLogInterval {
		return false, 0
	}
	count, c.refused, c.lastLog, c.logged = c.refused, 0, now, true
	return true, count
}

// Middleware is the limiter as a Middleware, after the authentication: a
// request without a Principal passes (only a misordered chain sends one).
func (c *OrganizationConcurrency) Middleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			if c.acquire(p.OrganizationID) {
				defer c.release(p.OrganizationID)
				next.ServeHTTP(w, r)
				return
			}
			setOutcome(r.Context(), outcomeOrganizationShed)
			if log, count := c.noteRefusal(); log {
				c.logger.Warn("agent requests shed: the organization holds its share of the pod's concurrent requests",
					"route", AgentRoute, "outcome", outcomeOrganizationShed,
					"organization_id", p.OrganizationID.String(),
					"max_in_flight_per_organization", c.max, "shed_since_last_log", count)
			}
			WriteTooManyRequests(w, jitteredRetryAfterSeconds())
		})
	}
}
