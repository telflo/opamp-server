package middleware

import (
	"context"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// AgentRoute is the agent endpoint's route, as the agent side's log lines and
// spans name it (never the path or the query).
const AgentRoute = "/opamp/api/v1"

// The agent endpoint's answers: each refusal's body is fixed text naming no
// id, count or reason, so a refusal tells a caller nothing about the token,
// the fleet or the organization. Log lines and the span's opamp.outcome
// carry the reason.
const (
	bodyUnauthorized = "Unauthorized"
	bodyForbidden    = "Forbidden"
	// bodyCollectorLimit is the cap's refusal: it says why, and names no
	// organization, id or count.
	bodyCollectorLimit  = "Forbidden: collector limit reached"
	bodyTooManyRequests = "Too Many Requests"
	bodyTooLarge        = "Request Entity Too Large"
	bodyBadRequest      = "Bad Request"
	bodyUnavailable     = "Service Unavailable"
)

// The Retry-After bands, in seconds: an organization over its rate is told
// 30 to 60, jittered so its collectors don't come back together; a
// collector over its guard, 60; a database failure, 10 to 30.
const (
	OrganizationRetryAfterMin = 30
	OrganizationRetryAfterMax = 60
	CollectorRetryAfter       = 60
	UnavailableRetryAfterMin  = 10
	UnavailableRetryAfterMax  = 30
)

// writeFixed answers status with text as a plain-text body.
func writeFixed(w http.ResponseWriter, status int, text string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, text+"\n")
}

// WriteUnauthorized answers 401 with the one body every missing, bad or
// lost credential gets.
func WriteUnauthorized(w http.ResponseWriter) {
	writeFixed(w, http.StatusUnauthorized, bodyUnauthorized)
}

// WriteForbidden answers 403 with the one fixed body: an archived fleet, a
// fleet that isn't server-class, and (from the poll) a collector enrolled
// in another fleet or a group that isn't the collector's fleet's.
func WriteForbidden(w http.ResponseWriter) {
	writeFixed(w, http.StatusForbidden, bodyForbidden)
}

// WriteCollectorLimitReached answers the poll of a collector the
// organization's cap (max_collectors) refuses to enroll: 403 with a fixed
// body saying so, naming no organization and no count.
func WriteCollectorLimitReached(w http.ResponseWriter) {
	writeFixed(w, http.StatusForbidden, bodyCollectorLimit)
}

// WriteTooManyRequests answers 429 telling the caller to retry after
// retryAfter seconds.
func WriteTooManyRequests(w http.ResponseWriter, retryAfter int) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	writeFixed(w, http.StatusTooManyRequests, bodyTooManyRequests)
}

// WriteUnavailable answers 503 with a jittered Retry-After of 10 to 30
// seconds (always sent: without it the supervisor retries after 3 s), for a
// database failure or a server that can't verify credentials.
func WriteUnavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", strconv.Itoa(jitter(UnavailableRetryAfterMin, UnavailableRetryAfterMax)))
	writeFixed(w, http.StatusServiceUnavailable, bodyUnavailable)
}

// WriteTooLarge answers 413: a body over the cap.
func WriteTooLarge(w http.ResponseWriter) {
	writeFixed(w, http.StatusRequestEntityTooLarge, bodyTooLarge)
}

// WriteBadRequest answers 400: a request the server can't read.
func WriteBadRequest(w http.ResponseWriter) {
	writeFixed(w, http.StatusBadRequest, bodyBadRequest)
}

// jitter is a whole number of seconds from low to high, both included,
// drawn afresh for each answer so a cohort refused together doesn't return
// together.
func jitter(low, high int) int {
	return low + rand.IntN(high-low+1)
}

// setOutcome stamps opamp.outcome on the request's server span: the reason,
// never an error's text.
func setOutcome(ctx context.Context, outcome string) {
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("opamp.outcome", outcome))
}

// refusalLogInterval spaces the log lines of the refusals made before a
// request has a Principal: one line a minute per outcome, with the count
// since the last; and of the 403s, one line a minute per outcome and
// organization.
const refusalLogInterval = time.Minute

// MaxForbiddenSamplerKeys bounds the (outcome, organization) pairs a
// ForbiddenSampler tracks: past it, the 403s of a new pair are counted
// together, one line a minute saying the sampler is full.
const MaxForbiddenSamplerKeys = 10_000

// RefusalSampler decides which refusals are logged: per outcome, the
// first, then the first once refusalLogInterval has passed since the last
// line, by the injected clock. A flood of bad credentials (anyone can send
// them) is a line a minute per outcome, not a line each, each line with
// the count since the last. The outcomes are a fixed set, so its map stays
// small. The 403s, which name an organization, are a ForbiddenSampler's.
type RefusalSampler struct {
	now func() time.Time

	mu      sync.Mutex
	windows map[string]*refusalWindow
}

// refusalWindow is one key's refusals since its last line.
type refusalWindow struct {
	count   int
	lastLog time.Time
	logged  bool
}

// note counts a refusal at now and reports whether to log it, with the
// refusals since the last line, this one included; the caller holds the
// sampler's mu.
func (w *refusalWindow) note(now time.Time) (log bool, count int) {
	w.count++
	if w.logged && now.Sub(w.lastLog) < refusalLogInterval {
		return false, 0
	}
	count, w.count, w.lastLog, w.logged = w.count, 0, now, true
	return true, count
}

// NewRefusalSampler returns a sampler on the clock now.
func NewRefusalSampler(now func() time.Time) *RefusalSampler {
	return &RefusalSampler{now: now, windows: map[string]*refusalWindow{}}
}

// Note counts a refusal of outcome and reports whether to log it, with the
// refusals of that outcome since the last line, this one included.
func (s *RefusalSampler) Note(outcome string) (log bool, count int) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.windows[outcome]
	if w == nil {
		w = &refusalWindow{}
		s.windows[outcome] = w
	}
	return w.note(now)
}

// ForbiddenCountAttrs are the attributes a ForbiddenSampler's line adds to
// the refusal's: the count since the last line and, when the sampler is
// full, that the count is every new pair's together.
func ForbiddenCountAttrs(count int, full bool) []any {
	attrs := []any{"refused_since_last_log", count}
	if full {
		attrs = append(attrs, "sampler_full", true, "max_keys", MaxForbiddenSamplerKeys)
	}
	return attrs
}

// ForbiddenSampler decides which of an authenticated request's 403s are
// logged (an archived fleet's, a fleet of another type's, the cap's, a
// foreign group's, a collector of another fleet's; the supervisor keeps
// polling through a 403): per outcome and organization, the first, then the
// first once refusalLogInterval has passed since the last line, by the
// injected clock, each line with the count since the last. Keyed by the
// organization too, one organization's flood never hides another's 403s.
//
// The collector guard's over-share refusals are sampled the same way, per
// outcome and organization.
//
// A pair's refusals after its last line are written as a trailing count: at
// its next refusal a minute or more after that line, else by the first
// sweep at least refusalLogInterval after that line (a sweep runs at the
// next refusal the sampler counts on that pod, of any pair, once
// refusalLogInterval has passed since the last sweep, so a count can wait
// up to about two intervals of the pod's activity); a pod whose sampler
// counts nothing more writes nothing more. The sweep drops the pairs whose
// last line is older than refusalLogInterval, handing flush the count of
// those with refusals not yet logged, and flushes the full sampler's count
// the same way. At most maxKeys pairs are kept, and the refusals of a new
// pair past it are counted together, a line a minute (Note's full).
type ForbiddenSampler struct {
	now     func() time.Time
	flush   func(ForbiddenFlush)
	maxKeys int

	mu        sync.Mutex
	windows   map[forbiddenKey]*refusalWindow
	full      refusalWindow
	lastSweep time.Time
}

// forbiddenKey is a ForbiddenSampler's pair.
type forbiddenKey struct {
	outcome      string
	organization uuid.UUID
}

// ForbiddenFlush is a sweep's trailing count: a dropped pair's refusals
// not yet logged, or (Full) the full sampler's, every new pair's together,
// which names no outcome or organization.
type ForbiddenFlush struct {
	Outcome      string
	Organization uuid.UUID
	Count        int
	Full         bool
}

// Attrs are the flush's log attributes: the pair's outcome and
// organization (none when Full), then ForbiddenCountAttrs.
func (f ForbiddenFlush) Attrs() []any {
	if f.Full {
		return ForbiddenCountAttrs(f.Count, true)
	}
	return append([]any{"outcome", f.Outcome, "organization_id", f.Organization.String()}, ForbiddenCountAttrs(f.Count, false)...)
}

// NewForbiddenSampler returns a sampler on the clock now, handing flush
// each trailing count a sweep writes (flush logs it; it runs outside the
// sampler's lock).
func NewForbiddenSampler(now func() time.Time, flush func(ForbiddenFlush)) *ForbiddenSampler {
	return newForbiddenSampler(now, flush, MaxForbiddenSamplerKeys)
}

func newForbiddenSampler(now func() time.Time, flush func(ForbiddenFlush), maxKeys int) *ForbiddenSampler {
	return &ForbiddenSampler{now: now, flush: flush, maxKeys: maxKeys, windows: map[forbiddenKey]*refusalWindow{}}
}

// Note counts a 403 of outcome for organization and reports whether to log
// it, with the refusals since the last line, this one included. full says
// the sampler holds maxKeys pairs and this pair isn't one: its count is the
// full sampler's, every new pair's together.
func (s *ForbiddenSampler) Note(outcome string, organization uuid.UUID) (log bool, count int, full bool) {
	now := s.now()
	key := forbiddenKey{outcome: outcome, organization: organization}
	s.mu.Lock()
	swept := s.sweep(now, key)
	w := s.windows[key]
	switch {
	case w != nil:
	case len(s.windows) >= s.maxKeys:
		w, full = &s.full, true
	default:
		w = &refusalWindow{}
		s.windows[key] = w
	}
	log, count = w.note(now)
	s.mu.Unlock()
	for _, f := range swept {
		s.flush(f)
	}
	return log, count, full
}

// sweep drops, once per refusalLogInterval, the pairs whose last line is
// older than it (they hold nothing a new pair wouldn't, but their count),
// and returns the counts not yet logged, the full sampler's too once its
// last line is as old. It keeps the pair being noted (note logs its
// count). The caller holds mu.
func (s *ForbiddenSampler) sweep(now time.Time, noting forbiddenKey) []ForbiddenFlush {
	if s.lastSweep.IsZero() {
		s.lastSweep = now
		return nil
	}
	if now.Sub(s.lastSweep) < refusalLogInterval {
		return nil
	}
	s.lastSweep = now
	var swept []ForbiddenFlush
	for key, w := range s.windows {
		if key == noting || now.Sub(w.lastLog) < refusalLogInterval {
			continue
		}
		if w.count > 0 {
			swept = append(swept, ForbiddenFlush{Outcome: key.outcome, Organization: key.organization, Count: w.count})
		}
		delete(s.windows, key)
	}
	if s.full.count > 0 && now.Sub(s.full.lastLog) >= refusalLogInterval {
		swept, s.full = append(swept, ForbiddenFlush{Count: s.full.count, Full: true}), refusalWindow{}
	}
	return swept
}
