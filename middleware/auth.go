package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/telflo/telflo-opamp-server/internal/lru"
	"github.com/telflo/telflo-opamp-server/store/collectors"
	"github.com/telflo/telflo-opamp-server/store/enrollmenttokens"
	"github.com/telflo/telflo-opamp-server/store/fleets"
	"github.com/telflo/telflo-opamp-server/tokens"
)

// The token cache's bounds: a token row is discovered in memory for at most
// TokenCacheTTL, so a revocation holds on every replica within it; at most
// TokenCacheEntries rows are kept.
const (
	TokenCacheTTL     = 10 * time.Second
	TokenCacheEntries = 20_000
)

// MaxBearerBytes is the longest bearer the authentication decodes: a real
// one is about 160 characters (base64 of a nonce, the sealed claims and a
// tag), so a longer one is refused 401 before any base64 decoding or
// decryption is spent on it.
const MaxBearerBytes = 1024

// TokenLookup is what authentication reads: enrollmenttokens.Store's Lookup,
// the token's row by its id with its fleet's type and status.
type TokenLookup interface {
	Lookup(ctx context.Context, id uuid.UUID) (enrollmenttokens.Authenticated, error)
}

// Authenticator is the agent endpoint's authentication. A request
// authenticates when:
//
//   - it carries `Authorization: Bearer <bearer>`, and the bearer opens
//     under one of the accepted keys (tokens.Keyring: the current key, then
//     each previous one) as claims {token_id, deployment_id};
//   - the token's row exists (enrollmenttokens Lookup by the token's id,
//     through the token cache);
//   - the bearer's fleet EQUALS the row's: the bearer names the fleet it
//     was minted for, and only the row's fleet counts;
//   - the row isn't revoked and isn't past expires_at, both read against
//     the injected clock at each use (a cached row's expiry still counts);
//   - its fleet is active and server-class ('server' or
//     'kubernetes-supervisor').
//
// It then puts one Principal in the request's context, every field
// from the row but the client's address. A missing, bad, unknown, foreign,
// revoked or expired credential answers 401 with one fixed body; an
// archived fleet or one of another type answers 403 with one fixed body; a
// database failure, or keys already destroyed, answers 503 with
// Retry-After.
//
// The token cache is keyed by the token's id, never the bearer (a bearer is
// a secret, and every reveal mints a new one for the same token), keeps
// found rows only (an unknown id is read again next time), and holds each
// row TokenCacheTTL by the injected clock, counted from the time taken
// before the lookup, so the time a slow lookup takes counts against it. So
// a revocation is refused by every replica within TokenCacheTTL of its
// commit.
//
// What it writes down: opamp.outcome on the request's span, and a log line
// (the route, the outcome, and the ids it learned: the token's once the
// bearer opened, the organization's and the fleet's once the row was read;
// the SQLSTATE of a database failure). Its 401s, which come before the
// request has a Principal and which anyone can cause, are logged once a
// minute per outcome with the count since the last line; its
// 403s and 503s one line each, a lookup that failed because the client
// went away at Info, never as an error. Never the bearer, a key, the
// Authorization header or an error's text.
type Authenticator struct {
	keyring *tokens.Keyring
	lookup  TokenLookup
	cache   *lru.Cache[uuid.UUID, enrollmenttokens.Authenticated]
	now     func() time.Time
	logger  *slog.Logger
	// unauthenticated spaces the log lines of the 401s per outcome;
	// forbidden the 403s', per outcome and organization.
	unauthenticated *RefusalSampler
	forbidden       *ForbiddenSampler
}

// NewAuthenticator returns the authentication reading bearers with keyring
// and token rows through lookup, on the clock now, logging to logger.
func NewAuthenticator(keyring *tokens.Keyring, lookup TokenLookup, now func() time.Time, logger *slog.Logger) *Authenticator {
	a := &Authenticator{
		keyring: keyring,
		lookup:  lookup,
		cache: lru.New(lru.Options[uuid.UUID, enrollmenttokens.Authenticated]{
			MaxEntries: TokenCacheEntries, TTL: TokenCacheTTL, Now: now,
		}),
		now:             now,
		logger:          logger,
		unauthenticated: NewRefusalSampler(now),
	}
	a.forbidden = NewForbiddenSampler(now, func(f ForbiddenFlush) {
		a.logger.Warn("agent request refused", append([]any{"route", AgentRoute, "status", http.StatusForbidden}, f.Attrs()...)...)
	})
	return a
}

// The outcomes a refusal records (opamp.outcome and the log line's
// outcome).
const (
	outcomeNoCredentials    = "missing_credentials"
	outcomeBadBearer        = "invalid_bearer"
	outcomeBearerTooLong    = "bearer_too_long"
	outcomeUnknownToken     = "unknown_token"
	outcomeFleetMismatch    = "bearer_fleet_mismatch"
	outcomeRevoked          = "token_revoked"
	outcomeExpired          = "token_expired"
	outcomeArchived         = "fleet_archived"
	outcomeFleetType        = "fleet_type_refused"
	outcomeKeysUnavailable  = "keys_unavailable"
	outcomeLookupFailed     = "token_lookup_failed"
	outcomeCancelled        = "request_cancelled"
	outcomeCredentialLost   = "credential_lost"
	outcomeRateLimited      = "rate_limited"
	outcomeCollectorLimited = "collector_rate_limited"
	outcomeTooLarge         = "body_too_large"
	outcomeUnreadable       = "body_unreadable"
	// outcomeBodyTimeout: an authenticated request's body not read within
	// BodyReadTimeout.
	outcomeBodyTimeout = "body_read_timeout"
	// outcomeOrganizationShed: the organization holds its share of the
	// pod's concurrent requests (OrganizationConcurrency).
	outcomeOrganizationShed = "organization_shed"
)

// OutcomeCancelled is the outcome of a request whose client went away (its
// context cancelled) before it was answered, on every path: the
// authentication's lookup, the body's read, and the poll's store calls (the
// supervisor cancels its in-flight default-token poll when it applies the
// hand-off). Nothing failed on the server's side, so it is logged at Info,
// never as an error or a warning.
const OutcomeCancelled = outcomeCancelled

// ClientGone reports whether the request's context was cancelled: its
// client went away (or the server is cutting it off). A deadline that
// passed (the database deadline, DeadlineExceeded) is not that: it is the
// server's failure to answer in time.
func ClientGone(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled)
}

// refusal is why a request was refused, and what it learned on the way.
type refusal struct {
	status   int
	outcome  string
	tokenID  uuid.UUID
	row      *enrollmenttokens.Authenticated
	sqlState string
	// lost is a credential lost after the request had its Principal
	// (RefuseLost): logged one line each, not sampled.
	lost bool
}

// Middleware is the authentication as a Middleware.
func (a *Authenticator) Middleware() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal, refused := a.authenticate(r)
			if refused != nil {
				a.refuse(w, r, *refused)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
		})
	}
}

// authenticate checks the request's credential (see Authenticator).
func (a *Authenticator) authenticate(r *http.Request) (Principal, *refusal) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return Principal{}, &refusal{status: http.StatusUnauthorized, outcome: outcomeNoCredentials}
	}
	bearer, found := strings.CutPrefix(header, "Bearer ")
	switch {
	case !found || bearer == "":
		return Principal{}, &refusal{status: http.StatusUnauthorized, outcome: outcomeBadBearer}
	case len(bearer) > MaxBearerBytes:
		return Principal{}, &refusal{status: http.StatusUnauthorized, outcome: outcomeBearerTooLong}
	}
	claims, err := a.keyring.Read(bearer)
	switch {
	case errors.Is(err, tokens.ErrNoKey):
		return Principal{}, &refusal{status: http.StatusServiceUnavailable, outcome: outcomeKeysUnavailable}
	case err != nil:
		return Principal{}, &refusal{status: http.StatusUnauthorized, outcome: outcomeBadBearer}
	}

	row, ok := a.cache.Get(claims.TokenID)
	if !ok {
		// The time is taken before the lookup, and the row cached as of
		// it: a revocation committed while a slow lookup ran is refused
		// within TokenCacheTTL of that commit.
		readAt := a.now()
		row, err = a.lookupWithin(r.Context(), claims.TokenID)
		switch {
		case errors.Is(err, enrollmenttokens.ErrNotFound):
			// Not cached: an unknown id is read again next time.
			return Principal{}, &refusal{status: http.StatusUnauthorized, outcome: outcomeUnknownToken, tokenID: claims.TokenID}
		case err != nil && errors.Is(r.Context().Err(), context.Canceled):
			// The client went away mid-lookup: nothing failed here.
			return Principal{}, &refusal{status: http.StatusServiceUnavailable, outcome: outcomeCancelled, tokenID: claims.TokenID}
		case err != nil:
			return Principal{}, &refusal{status: http.StatusServiceUnavailable, outcome: outcomeLookupFailed,
				tokenID: claims.TokenID, sqlState: sqlStateOf(err)}
		}
		a.cache.PutAt(claims.TokenID, row, readAt)
	}
	refuse := func(status int, outcome string) (Principal, *refusal) {
		return Principal{}, &refusal{status: status, outcome: outcome, tokenID: claims.TokenID, row: &row}
	}
	now := a.now()
	switch {
	case row.ID != claims.TokenID:
		// The cache and the store key rows by their id; anything else is a
		// bug, refused rather than trusted.
		return refuse(http.StatusUnauthorized, outcomeUnknownToken)
	case claims.FleetID != row.FleetID:
		// The bearer names the fleet it was minted for, and only the row's
		// fleet counts.
		return refuse(http.StatusUnauthorized, outcomeFleetMismatch)
	case row.RevokedAt != nil:
		return refuse(http.StatusUnauthorized, outcomeRevoked)
	case !row.ValidAt(now):
		return refuse(http.StatusUnauthorized, outcomeExpired)
	case row.FleetStatus != fleets.StatusActive:
		return refuse(http.StatusForbidden, outcomeArchived)
	case !row.FleetType.Valid():
		return refuse(http.StatusForbidden, outcomeFleetType)
	}
	return Principal{
		OrganizationID: row.OrganizationID,
		FleetID:        row.FleetID,
		FleetType:      row.FleetType,
		TokenID:        row.ID,
		TokenKind:      row.Kind,
		DoRefresh:      row.DoRefresh,
		ClientIP:       ClientIP(r),
	}, nil
}

// lookupWithin is the token's row, read within DatabaseDeadline: a lookup
// past it fails the request 503.
func (a *Authenticator) lookupWithin(ctx context.Context, id uuid.UUID) (enrollmenttokens.Authenticated, error) {
	ctx, cancel := context.WithTimeout(ctx, DatabaseDeadline)
	defer cancel()
	return a.lookup.Lookup(ctx, id)
}

// refuse answers a refusal, records its outcome on the span and logs it. A
// request whose client went away is request_cancelled, at Info, whatever
// refused it; before a Principal, its line is sampled with the other
// unauthenticated refusals', once a minute with the count.
func (a *Authenticator) refuse(w http.ResponseWriter, r *http.Request, f refusal) {
	if ClientGone(r.Context()) {
		f.status, f.outcome = http.StatusServiceUnavailable, outcomeCancelled
	}
	setOutcome(r.Context(), f.outcome)
	attrs := []any{"route", AgentRoute, "status", f.status, "outcome", f.outcome}
	if f.tokenID != uuid.Nil {
		attrs = append(attrs, "token_id", f.tokenID.String())
	}
	if f.row != nil {
		attrs = append(attrs, "organization_id", f.row.OrganizationID.String(), "fleet_id", f.row.FleetID.String())
	}
	if f.sqlState != "" {
		attrs = append(attrs, "sqlstate", f.sqlState)
	}
	switch {
	case f.outcome == outcomeCancelled && !f.lost:
		// Before a Principal, as the 401s: anyone can open a request and go
		// away, so a line a minute, with the count.
		if log, count := a.unauthenticated.Note(f.outcome); log {
			a.logger.Info("agent request abandoned by its client", append(attrs, "refused_since_last_log", count)...)
		}
		WriteUnavailable(w)
	case f.outcome == outcomeCancelled:
		a.logger.Info("agent request abandoned by its client", attrs...)
		WriteUnavailable(w)
	case f.status == http.StatusServiceUnavailable:
		a.logger.Error("agent request refused", attrs...)
		WriteUnavailable(w)
	case f.status == http.StatusForbidden:
		// An archived fleet's or a fleet of another type's: its
		// supervisors keep polling through a 403, so a line a minute per
		// outcome and organization, with the count.
		var organization uuid.UUID
		if f.row != nil {
			organization = f.row.OrganizationID
		}
		if log, count, full := a.forbidden.Note(f.outcome, organization); log {
			a.logger.Warn("agent request refused", append(attrs, ForbiddenCountAttrs(count, full)...)...)
		}
		WriteForbidden(w)
	case f.lost:
		a.logger.Warn("agent request refused", attrs...)
		WriteUnauthorized(w)
	default:
		// Before a Principal: a line a minute per outcome, the ids of the
		// refusal it was written for, and the count since the last.
		if log, count := a.unauthenticated.Note(f.outcome); log {
			a.logger.Warn("agent request refused", append(attrs, "refused_since_last_log", count)...)
		}
		WriteUnauthorized(w)
	}
}

// CredentialLost reports whether a store's answer during an authenticated
// request means its token or its fleet is gone since the authentication (a
// revocation can't do that; a delete of the fleet, which takes its tokens,
// can): the request is then refused 401, as a credential that no longer
// exists.
func CredentialLost(err error) bool {
	return errors.Is(err, collectors.ErrFleetNotFound) || errors.Is(err, collectors.ErrTokenNotFound)
}

// RefuseLost answers an authenticated request whose token or fleet vanished
// mid-request (CredentialLost) with the 401 every bad credential gets, and
// forgets the token's cached row, so this replica's next request with it
// reads the store again.
func (a *Authenticator) RefuseLost(w http.ResponseWriter, r *http.Request) {
	f := refusal{status: http.StatusUnauthorized, outcome: outcomeCredentialLost, lost: true}
	if p, ok := PrincipalFrom(r.Context()); ok {
		a.cache.Remove(p.TokenID)
		f.tokenID = p.TokenID
		f.row = &enrollmenttokens.Authenticated{Token: enrollmenttokens.Token{
			OrganizationID: p.OrganizationID, FleetID: p.FleetID}}
	}
	a.refuse(w, r, f)
}

// sqlStateOf is the SQLSTATE of the Postgres error err wraps, or "".
func sqlStateOf(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
