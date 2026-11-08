package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

// AgentStack is the agent endpoint's middleware. The order of its layers is
// a design constraint, stated here in full; other comments refer to it.
// Outermost first:
//
//  0. the request's server span (ServerSpan), which every layer below
//     stamps with its outcome;
//  1. the security headers, so every answer carries them;
//  2. the declared length (DeclaredLength at MaxBodyBytes): a
//     Content-Length over the cap answers 413 at once, nothing read;
//  3. the pod's concurrency cap (Concurrency), shedding past its slots at
//     once, before any database work;
//  4. the authentication (Authenticator), which reads only the
//     Authorization header (its token lookup within DatabaseDeadline) and
//     puts the Principal in the context;
//  5. the organization's share of the pod's slots
//     (OrganizationConcurrency), shedding past it at once: it needs the
//     Principal's organization;
//  6. the per-collector guard (Guard's Middleware) by the request's
//     InstanceUIDHeader, when it has one: a runaway collector is refused
//     before it spends its organization's rate;
//  7. the organization's rate limit (Organizations; its limits read within
//     DatabaseDeadline), which needs only the Principal, so a request over
//     its rate is refused before its body is waited for;
//  8. the body (BodyLimit at MaxBodyBytes, within BodyReadTimeout): read
//     only now, for an authenticated request within its organization's
//     limits, capped while read (chunked) and after decompression (gzip);
//  9. the database deadline (Deadline at DatabaseDeadline) for the rest:
//     the body read is behind it, so its time doesn't count.
//
// So an unauthenticated caller never has its body read, and holds a
// concurrency slot only while its header is checked: a hundred stalled
// uploads without a credential can't take the slots a real poll needs, nor
// the memory their bodies would. And an authenticated caller that stalls
// its bodies holds at most its organization's share of the slots, each for
// at most BodyReadTimeout: one organization's credential, a never-expiring
// default token say, can't take every other organization's slots.
//
// A request without the header has the per-collector guard checked by the
// handler, after it decodes the body (the handler shares the stack's
// CollectorGuard). The agent endpoint (agent.New) builds the stack around
// the agent route, POST /opamp/api/v1.
type AgentStack struct {
	Concurrency             *ConcurrencyLimiter
	Authenticator           *Authenticator
	OrganizationConcurrency *OrganizationConcurrency
	// Guard is the per-collector guard the handler shares; nil leaves it
	// to the handler alone.
	Guard         *CollectorGuard
	Organizations *OrganizationLimiter
	// BodyReadTimeout bounds the body's read; zero is BodyReadTimeout (a
	// test passes a shorter one).
	BodyReadTimeout time.Duration
	// Now is the injected clock: the declared-length refusals' log lines
	// are spaced by it.
	Now    func() time.Time
	Logger *slog.Logger
}

// Handler wraps next in the stack.
func (s AgentStack) Handler(next http.Handler) http.Handler {
	readTimeout := s.BodyReadTimeout
	if readTimeout <= 0 {
		readTimeout = BodyReadTimeout
	}
	chain := []Middleware{
		ServerSpan,
		SecurityHeadersMiddleware,
		DeclaredLength(MaxBodyBytes, s.Now, s.Logger),
		s.Concurrency.Middleware(),
		s.Authenticator.Middleware(),
		s.OrganizationConcurrency.Middleware(),
	}
	if s.Guard != nil {
		chain = append(chain, s.Guard.Middleware())
	}
	chain = append(chain,
		s.Organizations.Middleware(),
		BodyLimit(MaxBodyBytes, readTimeout, s.Logger),
		Deadline(DatabaseDeadline),
	)
	return ChainMiddleware(next, chain...)
}
