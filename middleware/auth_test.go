package middleware

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/collectors"
	"github.com/telflo/opamp-server/store/enrollmenttokens"
	"github.com/telflo/opamp-server/store/fleets"
	. "github.com/telflo/opamp-server/store/storetest"
	"github.com/telflo/opamp-server/tokens"
)

// This package's own test database (CONTRIBUTING.md, "Tests").
const testDatabase = "telflo_opamp_middleware_test"

var ctx = context.Background()

// The keys' settings: canaries, so a test can search what was written down
// for them.
const (
	currentKeySetting  = "current-key-canary-6f1d-0123456789abcdef0123456789"
	previousKeySetting = "previous-key-canary-2b7e-0123456789abcdef01234567"
	strangerKeySetting = "stranger-key-canary-91c4-0123456789abcdef01234567"
)

func keyring(t *testing.T, current string, previous ...string) *tokens.Keyring {
	t.Helper()
	k, err := tokens.NewKeyring(current, previous)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(k.Destroy)
	return k
}

func mint(t *testing.T, k *tokens.Keyring, tokenID, fleetID uuid.UUID) string {
	t.Helper()
	bearer, err := k.Mint(tokens.Claims{TokenID: tokenID, FleetID: fleetID})
	if err != nil {
		t.Fatal(err)
	}
	return bearer
}

// countingLookup counts the lookups that reach the store.
type countingLookup struct {
	TokenLookup
	mu sync.Mutex
	n  int
}

func (c *countingLookup) Lookup(ctx context.Context, id uuid.UUID) (enrollmenttokens.Authenticated, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.TokenLookup.Lookup(ctx, id)
}

func (c *countingLookup) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// authSetup is one authentication over a fixture's stores.
type authSetup struct {
	auth    *Authenticator
	clock   *testClock
	lookups *countingLookup
	logs    *strings.Builder
}

func newAuth(t *testing.T, s *Stores, k *tokens.Keyring) authSetup {
	t.Helper()
	clock := newTestClock()
	clock.now = Now
	logs := &strings.Builder{}
	lookups := &countingLookup{TokenLookup: s.Tokens}
	return authSetup{auth: NewAuthenticator(k, lookups, clock.Now, captureLogs(logs)), clock: clock, lookups: lookups, logs: logs}
}

// send runs one request through the authentication with the Authorization
// header given ("" for none), and returns the answer and the Principal the
// next handler saw, if it was reached.
func send(a *Authenticator, authorization string) (*httptest.ResponseRecorder, *Principal) {
	return sendThrough(a, authorization, nil)
}

func sendThrough(a *Authenticator, authorization string, next func(w http.ResponseWriter, r *http.Request)) (*httptest.ResponseRecorder, *Principal) {
	var seen *Principal
	h := a.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFrom(r.Context())
		if ok {
			seen = &p
		}
		if next != nil {
			next(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, AgentRoute, nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, seen
}

func expectRefused(t *testing.T, label string, rec *httptest.ResponseRecorder, seen *Principal, status int) {
	t.Helper()
	want := map[int]string{http.StatusUnauthorized: "Unauthorized\n", http.StatusForbidden: "Forbidden\n"}[status]
	if rec.Code != status || rec.Body.String() != want || seen != nil {
		t.Errorf("%s: %d %q (handler reached: %v); want %d %q", label, rec.Code, rec.Body.String(), seen != nil, status, want)
	}
}

// A valid bearer of either kind, under the current or a previous key, puts
// the row's Principal in the context: its organization, its fleet and type,
// the token, its kind and do_refresh, and the client's address (for
// display).
func TestAValidBearerPutsTheRowsPrincipalInTheContext(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeKubernetesSupervisor)
		current, previous := keyring(t, currentKeySetting, previousKeySetting), keyring(t, previousKeySetting)
		a := newAuth(t, s, current)
		for label, want := range map[string]struct {
			bearer string
			token  uuid.UUID
			kind   enrollmenttokens.Kind
		}{
			"the rotate token":                   {mint(t, current, c.RotateTokenID, c.Fleet.ID), c.RotateTokenID, enrollmenttokens.KindRotate},
			"the default token":                  {mint(t, current, c.DefaultTokenID, c.Fleet.ID), c.DefaultTokenID, enrollmenttokens.KindDefault},
			"a bearer minted under the previous": {mint(t, previous, c.RotateTokenID, c.Fleet.ID), c.RotateTokenID, enrollmenttokens.KindRotate},
		} {
			rec, seen := send(a.auth, "Bearer "+want.bearer)
			if rec.Code != http.StatusOK || seen == nil {
				t.Fatalf("%s: %d %q", label, rec.Code, rec.Body.String())
			}
			expected := Principal{OrganizationID: OrgA, FleetID: c.Fleet.ID, FleetType: fleets.TypeKubernetesSupervisor,
				TokenID: want.token, TokenKind: want.kind, DoRefresh: false, ClientIP: "203.0.113.7"}
			if *seen != expected {
				t.Fatalf("%s: the Principal = %+v, want %+v", label, *seen, expected)
			}
		}
	})
}

// Every missing or bad credential answers 401 with one fixed body, and the
// next handler is never reached: no header; another scheme; an empty
// bearer; a bearer that isn't base64, or too short; one sealed under a key
// the server doesn't accept (every key failing); a sealed plaintext that
// isn't a token's claims; a token id with no row; a bearer naming another
// fleet than its row's, of the same organization or another; a revoked
// token; an expired one.
func TestEveryBadCredentialIsRefused401WithOneBody(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		other := s.CreateFleet(t, OrgA, "edge-dev", fleets.TypeServer)
		theirs := s.CreateFleet(t, OrgB, "globex", fleets.TypeServer)
		k := keyring(t, currentKeySetting, previousKeySetting)
		stranger := keyring(t, strangerKeySetting)
		if _, _, err := s.Tokens.Revoke(ctx, OrgA, other.Fleet.ID, other.RotateTokenID, Ana, Now); err != nil {
			t.Fatal(err)
		}
		notClaims, err := tokens.Seal(k.Keys()[0], []byte(`{"token_id":"`+c.RotateTokenID.String()+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		for label, authorization := range map[string]string{
			"no Authorization header":             "",
			"another scheme":                      "Basic " + mint(t, k, c.RotateTokenID, c.Fleet.ID),
			"a lower-case scheme":                 "bearer " + mint(t, k, c.RotateTokenID, c.Fleet.ID),
			"an empty bearer":                     "Bearer ",
			"a bearer that isn't base64":          "Bearer not*base64!",
			"a bearer too short":                  "Bearer " + base64.StdEncoding.EncodeToString([]byte("short")),
			"a bearer under no accepted key":      "Bearer " + mint(t, stranger, c.RotateTokenID, c.Fleet.ID),
			"a sealed plaintext not claims":       "Bearer " + notClaims,
			"a token id with no row":              "Bearer " + mint(t, k, uuid.New(), c.Fleet.ID),
			"a bearer naming another fleet":       "Bearer " + mint(t, k, c.RotateTokenID, other.Fleet.ID),
			"a bearer naming another org's fleet": "Bearer " + mint(t, k, c.RotateTokenID, theirs.Fleet.ID),
			"another org's token in our fleet":    "Bearer " + mint(t, k, theirs.RotateTokenID, c.Fleet.ID),
			"a revoked token":                     "Bearer " + mint(t, k, other.RotateTokenID, other.Fleet.ID),
		} {
			a := newAuth(t, s, k)
			rec, seen := send(a.auth, authorization)
			expectRefused(t, label, rec, seen, http.StatusUnauthorized)
		}

		// Expired: the rotate token expires a year on; by the clock a year
		// and a millisecond on it is refused.
		a := newAuth(t, s, k)
		a.clock.Advance(365*24*time.Hour + time.Millisecond)
		rec, seen := send(a.auth, "Bearer "+mint(t, k, c.RotateTokenID, c.Fleet.ID))
		expectRefused(t, "an expired token", rec, seen, http.StatusUnauthorized)
	})
}

// The fleet check alone: the same token is accepted with its own fleet in
// the bearer and refused with another's.
func TestABearerNamingAnotherFleetThanItsRowsIsRefused(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		other := s.CreateFleet(t, OrgA, "edge-dev", fleets.TypeServer)
		k := keyring(t, currentKeySetting)
		a := newAuth(t, s, k)
		if rec, seen := send(a.auth, "Bearer "+mint(t, k, c.RotateTokenID, c.Fleet.ID)); rec.Code != http.StatusOK || seen.FleetID != c.Fleet.ID {
			t.Fatalf("its own fleet: %d", rec.Code)
		}
		// Cached by now: the comparison holds on a cached row too.
		rec, seen := send(a.auth, "Bearer "+mint(t, k, c.RotateTokenID, other.Fleet.ID))
		expectRefused(t, "the token with another fleet in its bearer", rec, seen, http.StatusUnauthorized)
		lines := logLines(t, a.logs)
		if len(lines) == 0 {
			t.Fatal("the refusal wasn't logged")
		}
		if last := lines[len(lines)-1]; last["outcome"] != outcomeFleetMismatch || last["fleet_id"] != c.Fleet.ID.String() {
			t.Fatalf("the refusal's line = %v", last)
		}
	})
}

// staticLookup answers one row, or an error.
type staticLookup struct {
	row enrollmenttokens.Authenticated
	err error
}

func (s staticLookup) Lookup(context.Context, uuid.UUID) (enrollmenttokens.Authenticated, error) {
	return s.row, s.err
}

// An archived fleet's valid token, and a fleet of a type that isn't
// server-class (the operator bridge's `kubernetes`, which the schema
// refuses, so a stub lookup stands in), answer 403 with one fixed body.
func TestAnArchivedFleetOrAnotherTypeIsRefused403(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		archived := fleets.StatusArchived
		if _, err := s.Fleets.Update(ctx, OrgA, c.Fleet.ID, fleets.Changes{Status: &archived}, Now); err != nil {
			t.Fatal(err)
		}
		k := keyring(t, currentKeySetting)
		a := newAuth(t, s, k)
		rec, seen := send(a.auth, "Bearer "+mint(t, k, c.RotateTokenID, c.Fleet.ID))
		expectRefused(t, "an archived fleet", rec, seen, http.StatusForbidden)
	})
	k := keyring(t, currentKeySetting)
	token, fleet := uuid.New(), uuid.New()
	bridge := NewAuthenticator(k, staticLookup{row: enrollmenttokens.Authenticated{
		Token:     enrollmenttokens.Token{ID: token, OrganizationID: OrgA, FleetID: fleet, Kind: enrollmenttokens.KindRotate},
		FleetType: "kubernetes", FleetStatus: fleets.StatusActive,
	}}, newTestClock().Now, discard())
	rec, seen := send(bridge, "Bearer "+mint(t, k, token, fleet))
	expectRefused(t, "the bridge's fleet type", rec, seen, http.StatusForbidden)
}

// A lookup that fails (the database) answers 503 with a Retry-After of 10
// to 30 seconds, never 400 or 401; keys already destroyed (a shutting-down
// server) answer 503 too.
func TestALookupFailureOrDestroyedKeysAnswer503(t *testing.T) {
	k := keyring(t, currentKeySetting)
	failing := NewAuthenticator(k, staticLookup{err: errors.New("the database is down")}, newTestClock().Now, discard())
	bearer := mint(t, k, uuid.New(), uuid.New())
	rec, seen := send(failing, "Bearer "+bearer)
	retry, _ := strconv.Atoi(rec.Header().Get("Retry-After"))
	if rec.Code != http.StatusServiceUnavailable || seen != nil || retry < UnavailableRetryAfterMin || retry > UnavailableRetryAfterMax {
		t.Fatalf("a failed lookup: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	gone, err := tokens.NewKeyring(currentKeySetting, nil)
	if err != nil {
		t.Fatal(err)
	}
	gone.Destroy()
	rec, _ = send(NewAuthenticator(gone, staticLookup{}, newTestClock().Now, discard()), "Bearer "+bearer)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("destroyed keys: %d", rec.Code)
	}
}

// A revocation is seen once the token cache's 10 s pass by the injected
// clock, not before: and two authentications on one store (two replicas,
// each its own cache) both refuse it then.
func TestARevocationIsRefusedByEveryReplicaOnceTheCachesTTLPasses(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		k := keyring(t, currentKeySetting)
		clock := newTestClock()
		clock.now = Now
		replicas := []*Authenticator{
			NewAuthenticator(k, s.Tokens, clock.Now, discard()),
			NewAuthenticator(k, s.Tokens, clock.Now, discard()),
		}
		bearer := "Bearer " + mint(t, k, c.RotateTokenID, c.Fleet.ID)
		for i, a := range replicas {
			if rec, _ := send(a, bearer); rec.Code != http.StatusOK {
				t.Fatalf("replica %d before the revocation: %d", i, rec.Code)
			}
		}
		if _, _, err := s.Tokens.Revoke(ctx, OrgA, c.Fleet.ID, c.RotateTokenID, Ana, Now); err != nil {
			t.Fatal(err)
		}
		clock.Advance(10*time.Second - time.Nanosecond)
		for i, a := range replicas {
			if rec, _ := send(a, bearer); rec.Code != http.StatusOK {
				t.Fatalf("replica %d within the cache's TTL: %d (the cache wasn't used)", i, rec.Code)
			}
		}
		clock.Advance(time.Nanosecond)
		for i, a := range replicas {
			rec, seen := send(a, bearer)
			expectRefused(t, "replica "+strconv.Itoa(i)+" at the TTL", rec, seen, http.StatusUnauthorized)
		}
	})
}

// Expiry is read against the clock at each use, a cached row's too. A
// rotation's predecessor expires at its grace end, 5 s on: accepted (and
// flagged do_refresh) until then from one lookup, refused at it with no new
// lookup.
func TestACachedRowsExpiryIsReadAgainstTheClockAtEachUse(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		if _, err := s.Tokens.Rotate(ctx, enrollmenttokens.Rotation{OrganizationID: OrgA, FleetID: c.Fleet.ID,
			GraceEndsAt: Now.Add(5 * time.Second), Actor: Ana, At: Now}); err != nil {
			t.Fatal(err)
		}
		k := keyring(t, currentKeySetting)
		a := newAuth(t, s, k)
		bearer := "Bearer " + mint(t, k, c.RotateTokenID, c.Fleet.ID)
		rec, seen := send(a.auth, bearer)
		if rec.Code != http.StatusOK || !seen.DoRefresh {
			t.Fatalf("the predecessor in its grace: %d, %+v", rec.Code, seen)
		}
		a.clock.Advance(5*time.Second - time.Millisecond)
		if rec, _ := send(a.auth, bearer); rec.Code != http.StatusOK {
			t.Fatalf("just before the grace end: %d", rec.Code)
		}
		a.clock.Advance(time.Millisecond)
		rec, seen = send(a.auth, bearer)
		expectRefused(t, "at the grace end", rec, seen, http.StatusUnauthorized)
		if n := a.lookups.count(); n != 1 {
			t.Fatalf("%d lookups; want 1 (the row cached, its expiry read at each use)", n)
		}
	})
}

// The token cache is keyed by the token's id, never the bearer: two bearers
// of one token (each reveal mints a new one) are one lookup. An unknown
// token id is never cached: each request reads it.
func TestTheTokenCacheKeysByTheTokensIDAndKeepsFoundRowsOnly(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		k := keyring(t, currentKeySetting)
		a := newAuth(t, s, k)
		first, second := mint(t, k, c.RotateTokenID, c.Fleet.ID), mint(t, k, c.RotateTokenID, c.Fleet.ID)
		if first == second {
			t.Fatal("two mints made one bearer")
		}
		for _, bearer := range []string{first, second, first} {
			if rec, _ := send(a.auth, "Bearer "+bearer); rec.Code != http.StatusOK {
				t.Fatalf("a bearer of the token: %d", rec.Code)
			}
		}
		if n := a.lookups.count(); n != 1 {
			t.Fatalf("%d lookups for two bearers of one token; want 1", n)
		}
		unknown := "Bearer " + mint(t, k, uuid.New(), c.Fleet.ID)
		send(a.auth, unknown)
		send(a.auth, unknown)
		if n := a.lookups.count(); n != 3 {
			t.Fatalf("%d lookups after two requests of an unknown token; want 3 (a miss is never cached)", n)
		}
	})
}

// A request whose fleet (and so its token) is deleted after its
// authentication, which the store answers ErrFleetNotFound, is refused 401
// with the same body as any bad credential, and the token's cached row is
// forgotten: the next request reads the store again and is refused.
func TestACredentialLostMidRequestIsRefused401AndForgotten(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		k := keyring(t, currentKeySetting)
		a := newAuth(t, s, k)
		bearer := "Bearer " + mint(t, k, c.RotateTokenID, c.Fleet.ID)
		if rec, _ := send(a.auth, bearer); rec.Code != http.StatusOK {
			t.Fatalf("before: %d", rec.Code)
		}
		var storeErr error
		rec, seen := sendThrough(a.auth, bearer, func(w http.ResponseWriter, r *http.Request) {
			p, _ := PrincipalFrom(r.Context())
			if _, err := s.Fleets.Delete(r.Context(), p.OrganizationID, p.FleetID); err != nil {
				t.Error(err)
			}
			_, storeErr = s.Collectors.Enroll(r.Context(), collectors.Enrollment{OrganizationID: p.OrganizationID,
				FleetID: p.FleetID, InstanceUID: uuid.New(), TokenID: p.TokenID, At: a.clock.Now()})
			if CredentialLost(storeErr) {
				a.auth.RefuseLost(w, r)
				return
			}
			w.WriteHeader(http.StatusOK)
		})
		if !errors.Is(storeErr, collectors.ErrFleetNotFound) || rec.Code != http.StatusUnauthorized ||
			rec.Body.String() != "Unauthorized\n" || seen == nil {
			t.Fatalf("mid-request: the store said %v; answered %d %q", storeErr, rec.Code, rec.Body.String())
		}
		rec, seen = send(a.auth, bearer)
		expectRefused(t, "the next request", rec, seen, http.StatusUnauthorized)
		if n := a.lookups.count(); n != 2 {
			t.Fatalf("%d lookups; want 2 (the lost row forgotten)", n)
		}
	})
	for err, lost := range map[error]bool{
		collectors.ErrFleetNotFound: true, collectors.ErrTokenNotFound: true,
		collectors.ErrNotFound: false, collectors.ErrCapReached: false, enrollmenttokens.ErrNotFound: false,
		errors.New("a failure"): false,
	} {
		if CredentialLost(err) != lost {
			t.Errorf("CredentialLost(%v) = %v", err, !lost)
		}
	}
}

// slowLookup answers its row as of the moment it is called, and only
// delay later by the injected clock: a lookup whose snapshot is taken
// before a revocation commits and whose answer arrives after.
type slowLookup struct {
	clock   *testClock
	delay   time.Duration
	mu      sync.Mutex
	row     enrollmenttokens.Authenticated
	revoked bool
}

func (s *slowLookup) Lookup(context.Context, uuid.UUID) (enrollmenttokens.Authenticated, error) {
	s.mu.Lock()
	row := s.row
	if s.revoked {
		at := s.clock.Now()
		row.RevokedAt = &at
	}
	s.mu.Unlock()
	s.clock.Advance(s.delay)
	return row, nil
}

func (s *slowLookup) revoke() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revoked = true
}

// The token cache counts a row's age from the time taken before its
// lookup, so a revocation committed just after a slow lookup's snapshot is
// refused TokenCacheTTL after that read, not TokenCacheTTL after the slow
// answer arrived; and the cached row is still answered just before.
func TestARevocationDuringASlowLookupIsRefusedWithinTheTTLOfTheRead(t *testing.T) {
	k := keyring(t, currentKeySetting)
	token, organization, fleet := uuid.New(), uuid.New(), uuid.New()
	clock := newTestClock()
	lookup := &slowLookup{clock: clock, delay: 4 * time.Second, row: activeRow(token, organization, fleet)}
	a := NewAuthenticator(k, lookup, clock.Now, discard())
	bearer := "Bearer " + mint(t, k, token, fleet)
	readAt := clock.Now()
	if rec, _ := send(a, bearer); rec.Code != http.StatusOK {
		t.Fatalf("before the revocation: %d", rec.Code)
	}
	lookup.revoke() // committed just after the lookup's snapshot
	clock.Advance(readAt.Add(TokenCacheTTL - time.Nanosecond).Sub(clock.Now()))
	if rec, _ := send(a, bearer); rec.Code != http.StatusOK {
		t.Fatalf("just under the TTL from the read: %d (the cache wasn't used)", rec.Code)
	}
	clock.Advance(time.Nanosecond)
	rec, seen := send(a, bearer)
	expectRefused(t, "the TTL from the read", rec, seen, http.StatusUnauthorized)
}

// A bearer longer than MaxBearerBytes is refused 401 before it is decoded:
// with the keys destroyed, a decoded bearer would answer 503 (no key to
// try), so the long one's 401 shows nothing was decoded; one of exactly
// MaxBearerBytes is decoded.
func TestABearerOver1KiBIsRefusedBeforeItIsDecoded(t *testing.T) {
	gone, err := tokens.NewKeyring(currentKeySetting, nil)
	if err != nil {
		t.Fatal(err)
	}
	gone.Destroy()
	logs := &strings.Builder{}
	lookups := &countingLookup{TokenLookup: staticLookup{}}
	a := NewAuthenticator(gone, lookups, newTestClock().Now, captureLogs(logs))
	rec, seen := send(a, "Bearer "+strings.Repeat("A", MaxBearerBytes+4))
	expectRefused(t, "a bearer over 1 KiB", rec, seen, http.StatusUnauthorized)
	if lines := logLines(t, logs); len(lines) != 1 || lines[0]["outcome"] != outcomeBearerTooLong {
		t.Fatalf("the refusal's lines: %v", lines)
	}
	if rec, _ := send(a, "Bearer "+strings.Repeat("A", MaxBearerBytes)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a bearer of exactly 1 KiB: %d; want it decoded (503: no live key)", rec.Code)
	}
	if n := lookups.count(); n != 0 {
		t.Fatalf("%d lookups", n)
	}
}

// cancellingLookup is a lookup during which the request's client goes
// away: it cancels the request's context and fails as pgx then does.
type cancellingLookup struct {
	cancel context.CancelFunc
}

func (c cancellingLookup) Lookup(ctx context.Context, _ uuid.UUID) (enrollmenttokens.Authenticated, error) {
	c.cancel()
	<-ctx.Done()
	return enrollmenttokens.Authenticated{}, fmt.Errorf("lookup: %w", ctx.Err())
}

// A lookup that failed because the request was cancelled (its client went
// away) is no ERROR line; a lookup that failed on its own is.
func TestALookupAbandonedByItsClientIsNoErrorLine(t *testing.T) {
	k := keyring(t, currentKeySetting)
	bearer := "Bearer " + mint(t, k, uuid.New(), uuid.New())
	logs := &strings.Builder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := NewAuthenticator(k, cancellingLookup{cancel: cancel}, newTestClock().Now, captureLogs(logs))
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, AgentRoute, nil)
	req.Header.Set("Authorization", bearer)
	rec := httptest.NewRecorder()
	a.Middleware()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("the handler was reached") })).ServeHTTP(rec, req)
	lines := logLines(t, logs)
	if rec.Code != http.StatusServiceUnavailable || len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["outcome"] != outcomeCancelled {
		t.Fatalf("an abandoned lookup: %d, lines %v", rec.Code, lines)
	}

	logs.Reset()
	failing := NewAuthenticator(k, staticLookup{err: errors.New("the database is down")}, newTestClock().Now, captureLogs(logs))
	send(failing, bearer)
	if lines := logLines(t, logs); len(lines) != 1 || lines[0]["level"] != "ERROR" || lines[0]["outcome"] != outcomeLookupFailed {
		t.Fatalf("a failed lookup's lines: %v", lines)
	}

	// Whatever refused it, a request whose client already went away is
	// request_cancelled at Info (here a lookup that failed, and an unknown
	// token).
	for label, lookup := range map[string]TokenLookup{
		"a failed lookup":  staticLookup{err: errors.New("the database is down")},
		"an unknown token": staticLookup{err: enrollmenttokens.ErrNotFound},
	} {
		logs.Reset()
		gone, cancelGone := context.WithCancel(context.Background())
		cancelGone()
		req := httptest.NewRequestWithContext(gone, http.MethodPost, AgentRoute, nil)
		req.Header.Set("Authorization", bearer)
		rec := httptest.NewRecorder()
		NewAuthenticator(k, lookup, newTestClock().Now, captureLogs(logs)).Middleware()(
			http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("the handler was reached") })).ServeHTTP(rec, req)
		if lines := logLines(t, logs); len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["outcome"] != outcomeCancelled {
			t.Fatalf("%s, its client gone: %d, lines %v", label, rec.Code, lines)
		}
	}
}

// A request refused before it has a Principal whose client already went
// away (anyone can open one and go) is request_cancelled at Info, sampled
// with the other unauthenticated refusals: a line a minute, with the count
// since the last. A credential lost after the Principal, its client gone,
// stays a line each.
func TestUnauthenticatedRequestsAbandonedByTheirClientsAreLoggedOnceAMinute(t *testing.T) {
	k := keyring(t, currentKeySetting)
	clock := newTestClock()
	logs := &strings.Builder{}
	a := NewAuthenticator(k, staticLookup{err: enrollmenttokens.ErrNotFound}, clock.Now, captureLogs(logs))
	gone, cancelGone := context.WithCancel(context.Background())
	cancelGone()
	abandon := func(bearer string) {
		req := httptest.NewRequestWithContext(gone, http.MethodPost, AgentRoute, nil)
		if bearer != "" {
			req.Header.Set("Authorization", bearer)
		}
		rec := httptest.NewRecorder()
		a.Middleware()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("the handler was reached") })).ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("an abandoned request: %d", rec.Code)
		}
	}
	unknown := "Bearer " + mint(t, k, uuid.New(), uuid.New())
	for range 250 {
		abandon("")
		abandon("Bearer not*base64!")
		abandon(unknown)
	}
	lines := logLines(t, logs)
	if len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["outcome"] != outcomeCancelled ||
		lines[0]["refused_since_last_log"] != 1.0 {
		t.Fatalf("750 abandoned requests' lines: %v", lines)
	}
	clock.Advance(time.Minute)
	abandon("")
	if lines := logLines(t, logs); len(lines) != 2 || lines[1]["refused_since_last_log"] != 750.0 {
		t.Fatalf("a minute on: %v", lines)
	}

	logs.Reset()
	token, organization, fleet := uuid.New(), uuid.New(), uuid.New()
	accepted := NewAuthenticator(k, staticLookup{row: activeRow(token, organization, fleet)}, clock.Now, captureLogs(logs))
	bearer := "Bearer " + mint(t, k, token, fleet)
	for range 3 {
		sendThrough(accepted, bearer, func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithCancel(r.Context())
			cancel()
			accepted.RefuseLost(w, r.WithContext(ctx))
		})
	}
	if lines := logLines(t, logs); len(lines) != 3 || lines[0]["outcome"] != outcomeCancelled || lines[0]["level"] != "INFO" {
		t.Fatalf("three lost credentials, their clients gone: %v", lines)
	}
}

// The 401s made before a request has a Principal (anyone can send them) are
// logged once a minute per outcome, with the count since the last line; a
// 403, and a credential lost after the Principal, stay one line each.
func TestRefusalsBeforeAPrincipalAreLoggedOnceAMinutePerOutcome(t *testing.T) {
	k := keyring(t, currentKeySetting)
	clock := newTestClock()
	logs := &strings.Builder{}
	a := NewAuthenticator(k, staticLookup{err: enrollmenttokens.ErrNotFound}, clock.Now, captureLogs(logs))
	unknown := "Bearer " + mint(t, k, uuid.New(), uuid.New())
	burst := func() {
		for range 50 {
			send(a, "")
			send(a, "Bearer not*base64!")
			send(a, unknown)
		}
	}
	counts := func(lines []map[string]any) map[string]float64 {
		got := map[string]float64{}
		for _, line := range lines {
			got[line["outcome"].(string)] = line["refused_since_last_log"].(float64)
		}
		return got
	}
	burst()
	lines := logLines(t, logs)
	if len(lines) != 3 || fmt.Sprint(counts(lines)) != fmt.Sprint(map[string]float64{
		outcomeNoCredentials: 1, outcomeBadBearer: 1, outcomeUnknownToken: 1}) {
		t.Fatalf("the first minute's lines: %v", lines)
	}
	clock.Advance(time.Minute - time.Nanosecond)
	send(a, "")
	if n := len(logLines(t, logs)); n != 3 {
		t.Fatalf("%d lines before the minute passed", n)
	}
	clock.Advance(time.Nanosecond)
	burst()
	lines = logLines(t, logs)[3:]
	if len(lines) != 3 || fmt.Sprint(counts(lines)) != fmt.Sprint(map[string]float64{
		outcomeNoCredentials: 51, outcomeBadBearer: 50, outcomeUnknownToken: 50}) {
		t.Fatalf("the next minute's lines: %v", lines)
	}

	logs.Reset()
	token, organization, fleet := uuid.New(), uuid.New(), uuid.New()
	archived := activeRow(token, organization, fleet)
	archived.FleetStatus = fleets.StatusArchived
	refused := NewAuthenticator(k, staticLookup{row: archived}, clock.Now, captureLogs(logs))
	accepted := NewAuthenticator(k, staticLookup{row: activeRow(token, organization, fleet)}, clock.Now, captureLogs(logs))
	bearer := "Bearer " + mint(t, k, token, fleet)
	for range 3 {
		send(refused, bearer)
		sendThrough(accepted, bearer, func(w http.ResponseWriter, r *http.Request) { accepted.RefuseLost(w, r) })
	}
	// The 403s are sampled too (a supervisor polls through a 403), a line a
	// minute per outcome; a lost credential is a line each.
	if n := len(logLines(t, logs)); n != 4 {
		t.Fatalf("%d lines for three 403s and three lost credentials; want 4 (one for the 403s)", n)
	}
}
