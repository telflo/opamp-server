package enrollmenttokens_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/enrollmenttokens"
	"github.com/telflo/opamp-server/store/fleets"
	"github.com/telflo/opamp-server/store/organizations"
	. "github.com/telflo/opamp-server/store/storetest"
)

// This package's own test database (CONTRIBUTING.md, "Tests").
const testDatabase = "telflo_opamp_enrollmenttokens_test"

var ctx = context.Background()

func rotation(fleet uuid.UUID, at, graceEndsAt time.Time, expiresAt *time.Time) enrollmenttokens.Rotation {
	return enrollmenttokens.Rotation{OrganizationID: OrgA, FleetID: fleet, ExpiresAt: expiresAt,
		GraceEndsAt: graceEndsAt, Actor: Ana, At: at}
}

func eventsOf(t *testing.T, s *Stores, org, fleet uuid.UUID) []enrollmenttokens.Event {
	t.Helper()
	page, err := s.Tokens.ListEvents(ctx, org, fleet, enrollmenttokens.MaxListLimit, nil)
	if err != nil || page.Next != nil {
		t.Fatalf("ListEvents = %+v, %v; want one page", page, err)
	}
	return page.Events
}

// List: the fleet's tokens newest first (ties by id, descending), each with
// the collectors last seen on it within the last 5 minutes of the caller's
// clock (strictly: one seen exactly 5 minutes ago no longer counts).
func TestListCountsEachTokensCollectorsSeenInTheLastFiveMinutes(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		other := s.CreateFleet(t, OrgA, "edge-dev", fleets.TypeServer)
		now := Now.Add(time.Hour)
		for i, seen := range []struct {
			token *uuid.UUID
			ago   time.Duration
		}{
			{&c.RotateTokenID, 0}, {&c.RotateTokenID, 4*time.Minute + 59*time.Second},
			{&c.RotateTokenID, 5 * time.Minute}, {&c.RotateTokenID, time.Hour},
			{&c.DefaultTokenID, time.Minute}, {nil, 0},
		} {
			s.InsertCollector(t, Collector{ID: uuid.New(), OrganizationID: OrgA, FleetID: c.Fleet.ID, GroupID: c.DefaultGroupID,
				Name: "edge-0" + string(rune('1'+i)), TokenID: seen.token, LastSeenAt: now.Add(-seen.ago)})
		}
		s.InsertCollector(t, Collector{ID: uuid.New(), OrganizationID: OrgA, FleetID: other.Fleet.ID, GroupID: other.DefaultGroupID,
			Name: "dev-01", TokenID: &other.RotateTokenID, LastSeenAt: now})
		rotated, err := s.Tokens.Rotate(ctx, rotation(c.Fleet.ID, Now.Add(time.Minute), Now.Add(2*time.Hour), nil))
		if err != nil {
			t.Fatal(err)
		}

		listed, err := s.Tokens.List(ctx, OrgA, c.Fleet.ID, now)
		if err != nil || len(listed) != 3 {
			t.Fatalf("List = %+v, %v", listed, err)
		}
		first, second := c.DefaultTokenID, c.RotateTokenID
		if strings.Compare(string(first[:]), string(second[:])) < 0 {
			first, second = second, first
		}
		want := []struct {
			id     uuid.UUID
			active int
		}{{rotated.Token.ID, 0}, {first, map[uuid.UUID]int{c.RotateTokenID: 2, c.DefaultTokenID: 1}[first]},
			{second, map[uuid.UUID]int{c.RotateTokenID: 2, c.DefaultTokenID: 1}[second]}}
		for i, w := range want {
			if listed[i].ID != w.id || listed[i].ActiveCollectors != w.active {
				t.Fatalf("listed[%d] = %s with %d, want %s with %d", i, listed[i].ID, listed[i].ActiveCollectors, w.id, w.active)
			}
		}
		if _, err := s.Tokens.List(ctx, OrgA, uuid.New(), now); !errors.Is(err, enrollmenttokens.ErrFleetNotFound) {
			t.Fatalf("an unknown fleet = %v", err)
		}
	})
}

// A rotation puts the fleet's current rotate token in its grace (ExpiresAt
// the grace end, DoRefresh), makes a new rotate token with the caller's
// expiry, and writes `rotated` for the old and `issued` for the new; the
// next rotation's predecessor is the new token.
func TestRotatePutsTheCurrentTokenInItsGraceAndMakesANewOne(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		at, grace, expires := Now.Add(time.Hour), Now.Add(25*time.Hour), Now.AddDate(0, 6, 0)
		result, err := s.Tokens.Rotate(ctx, rotation(c.Fleet.ID, at, grace, &expires))
		if err != nil || result.PredecessorID == nil || *result.PredecessorID != c.RotateTokenID {
			t.Fatalf("Rotate = %+v, %v", result, err)
		}
		n := result.Token
		if n.Kind != enrollmenttokens.KindRotate || n.ExpiresAt == nil || !n.ExpiresAt.Equal(Millisecond(expires)) ||
			n.DoRefresh || n.RevokedAt != nil || *n.CreatedBy != *Ana || !n.CreatedAt.Equal(Millisecond(at)) ||
			n.FleetID != c.Fleet.ID || n.OrganizationID != OrgA {
			t.Fatalf("the new token = %+v", n)
		}
		old, err := s.Tokens.Get(ctx, OrgA, c.Fleet.ID, c.RotateTokenID)
		if err != nil || !old.DoRefresh || old.ExpiresAt == nil || !old.ExpiresAt.Equal(Millisecond(grace)) || old.RevokedAt != nil {
			t.Fatalf("the predecessor = %+v, %v", old, err)
		}
		if def, err := s.Tokens.Get(ctx, OrgA, c.Fleet.ID, c.DefaultTokenID); err != nil || def.DoRefresh || def.ExpiresAt != nil {
			t.Fatalf("the default token changed: %+v, %v", def, err)
		}
		events := eventsOf(t, s, OrgA, c.Fleet.ID)
		if len(events) != 4 {
			t.Fatalf("events = %+v", events)
		}
		kinds := map[enrollmenttokens.EventKind]uuid.UUID{}
		for _, e := range events[:2] {
			if !e.OccurredAt.Equal(Millisecond(at)) || *e.Actor != *Ana {
				t.Fatalf("a rotation's event = %+v", e)
			}
			kinds[e.Kind] = e.TokenID
		}
		if kinds[enrollmenttokens.EventRotated] != c.RotateTokenID || kinds[enrollmenttokens.EventIssued] != n.ID {
			t.Fatalf("the rotation's events = %v", kinds)
		}
		if active, err := s.Tokens.GetActiveRotate(ctx, OrgA, c.Fleet.ID, at); err != nil || active.ID != n.ID {
			t.Fatalf("GetActiveRotate = %+v, %v; want the new token", active, err)
		}

		// A second rotation, at the same instant as the first: its
		// predecessor is the first's new token, never the one already in its
		// grace (preferring a token not yet rotated).
		second, err := s.Tokens.Rotate(ctx, rotation(c.Fleet.ID, at, grace, nil))
		if err != nil || second.PredecessorID == nil || *second.PredecessorID != n.ID || second.Token.ExpiresAt != nil {
			t.Fatalf("the second Rotate = %+v, %v", second, err)
		}
		if active, err := s.Tokens.GetActiveRotate(ctx, OrgA, c.Fleet.ID, at); err != nil || active.ID != second.Token.ID {
			t.Fatalf("GetActiveRotate = %+v, %v; want the second's token", active, err)
		}
		// After the grace, the predecessors no longer authenticate; the
		// current token does.
		for id, valid := range map[uuid.UUID]bool{c.RotateTokenID: false, n.ID: false, second.Token.ID: true} {
			tok, err := s.Tokens.Get(ctx, OrgA, c.Fleet.ID, id)
			if err != nil || tok.ValidAt(grace) != valid {
				t.Fatalf("token %s valid at the grace end = %v, want %v", id, tok.ValidAt(grace), valid)
			}
		}
	})
}

// A rotation never lengthens its predecessor's life. A predecessor that
// would expire before the grace end keeps its own expiry; one that never
// expires gets the grace end. Either way it is marked DoRefresh.
func TestRotateNeverLengthensThePredecessorsLife(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		own := Millisecond(Now.AddDate(1, 0, 0)) // CreateFleet's rotate token expiry
		at := Now.Add(time.Hour)

		// A grace end two years away, past the predecessor's own expiry: the
		// new token never expires, so the grace end is allowed.
		first, err := s.Tokens.Rotate(ctx, rotation(c.Fleet.ID, at, Now.AddDate(2, 0, 0), nil))
		if err != nil || first.PredecessorID == nil || *first.PredecessorID != c.RotateTokenID {
			t.Fatalf("Rotate = %+v, %v", first, err)
		}
		old, err := s.Tokens.Get(ctx, OrgA, c.Fleet.ID, c.RotateTokenID)
		if err != nil || old.ExpiresAt == nil || !old.ExpiresAt.Equal(own) || !old.DoRefresh {
			t.Fatalf("the predecessor = %+v, %v; want its own expiry %s kept, and DoRefresh", old, err, own)
		}

		// The new token never expires: its rotation gives it the grace end.
		grace := at.Add(3 * time.Hour)
		second, err := s.Tokens.Rotate(ctx, rotation(c.Fleet.ID, at.Add(time.Minute), grace, nil))
		if err != nil || second.PredecessorID == nil || *second.PredecessorID != first.Token.ID {
			t.Fatalf("the second Rotate = %+v, %v", second, err)
		}
		never, err := s.Tokens.Get(ctx, OrgA, c.Fleet.ID, first.Token.ID)
		if err != nil || never.ExpiresAt == nil || !never.ExpiresAt.Equal(Millisecond(grace)) || !never.DoRefresh {
			t.Fatalf("the never-expiring predecessor = %+v, %v; want the grace end %s", never, err, Millisecond(grace))
		}
		// The first predecessor, already in its grace and not current, is
		// untouched by the second rotation.
		if again, _ := s.Tokens.Get(ctx, OrgA, c.Fleet.ID, c.RotateTokenID); !again.ExpiresAt.Equal(own) {
			t.Fatalf("the first predecessor changed: %+v", again)
		}
	})
}

// What a rotation refuses, changing nothing: a grace end not in the
// future, or after the new token's expiry; an archived fleet; another
// organization's or an unknown fleet; an actor the table refuses.
func TestRotateRefusesAndChangesNothing(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		theirs := s.CreateFleet(t, OrgB, "globex", fleets.TypeServer)
		archived := s.CreateFleet(t, OrgA, "edge-old", fleets.TypeServer)
		if _, err := s.Fleets.Update(ctx, OrgA, archived.Fleet.ID, fleets.Changes{Status: Ptr(fleets.StatusArchived)}, Now); err != nil {
			t.Fatal(err)
		}
		at := Now.Add(time.Hour)
		before, beforeB := s.Dump(t, OrgA), s.Dump(t, OrgB)
		for _, r := range []struct {
			name     string
			rotation enrollmenttokens.Rotation
			want     error
		}{
			{"a grace end now", rotation(c.Fleet.ID, at, at, nil), enrollmenttokens.ErrInvalid},
			{"a grace end past", rotation(c.Fleet.ID, at, at.Add(-time.Minute), nil), enrollmenttokens.ErrInvalid},
			{"a grace end after the new token's expiry", rotation(c.Fleet.ID, at, at.Add(2*time.Hour), Ptr(at.Add(time.Hour))), enrollmenttokens.ErrInvalid},
			{"an actor without an email", func() enrollmenttokens.Rotation {
				r := rotation(c.Fleet.ID, at, at.Add(time.Hour), nil)
				r.Actor = &fleets.Actor{ID: uuid.New()}
				return r
			}(), enrollmenttokens.ErrInvalid},
			{"an archived fleet", rotation(archived.Fleet.ID, at, at.Add(time.Hour), nil), enrollmenttokens.ErrFleetArchived},
			{"another organization's fleet", rotation(theirs.Fleet.ID, at, at.Add(time.Hour), nil), enrollmenttokens.ErrFleetNotFound},
			{"an unknown fleet", rotation(uuid.New(), at, at.Add(time.Hour), nil), enrollmenttokens.ErrFleetNotFound},
		} {
			if _, err := s.Tokens.Rotate(ctx, r.rotation); !errors.Is(err, r.want) {
				t.Errorf("%s: Rotate = %v, want %v", r.name, err, r.want)
			}
		}
		s.Unchanged(t, "the refused rotations", OrgA, before)
		s.Unchanged(t, "the refused rotations", OrgB, beforeB)

		// A grace end exactly at the new token's expiry is allowed.
		if _, err := s.Tokens.Rotate(ctx, rotation(c.Fleet.ID, at, at.Add(time.Hour), Ptr(at.Add(time.Hour)))); err != nil {
			t.Fatalf("a grace end at the expiry: %v", err)
		}
	})
}

// A fleet with no valid rotate token (revoked, or expired) rotates into a
// new one with no predecessor: an expired or revoked token is never put
// back in a grace.
func TestRotateWithNoValidRotateTokenMakesOneWithNoPredecessor(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		if _, _, err := s.Tokens.Revoke(ctx, OrgA, c.Fleet.ID, c.RotateTokenID, Ana, Now); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Tokens.GetActiveRotate(ctx, OrgA, c.Fleet.ID, Now); !errors.Is(err, enrollmenttokens.ErrNotFound) {
			t.Fatalf("GetActiveRotate with none = %v", err)
		}
		at := Now.Add(time.Minute)
		r, err := s.Tokens.Rotate(ctx, rotation(c.Fleet.ID, at, at.Add(time.Hour), Ptr(at.Add(2*time.Hour))))
		if err != nil || r.PredecessorID != nil {
			t.Fatalf("Rotate = %+v, %v", r, err)
		}
		revoked, _ := s.Tokens.Get(ctx, OrgA, c.Fleet.ID, c.RotateTokenID)
		if revoked.DoRefresh || !revoked.ExpiresAt.Equal(Millisecond(Now.AddDate(1, 0, 0))) {
			t.Fatalf("the revoked token was rotated: %+v", revoked)
		}
		// Past its expiry, r's token is no longer current either.
		later := at.Add(3 * time.Hour)
		r2, err := s.Tokens.Rotate(ctx, rotation(c.Fleet.ID, later, later.Add(time.Hour), nil))
		if err != nil || r2.PredecessorID != nil {
			t.Fatalf("Rotate after the expiry = %+v, %v", r2, err)
		}
		expired, _ := s.Tokens.Get(ctx, OrgA, c.Fleet.ID, r.Token.ID)
		if expired.DoRefresh || !expired.ExpiresAt.Equal(Millisecond(at.Add(2*time.Hour))) {
			t.Fatalf("the expired token was rotated: %+v", expired)
		}
	})
}

// Two rotations of one fleet at once serialize under the fleet's lock: the
// second's predecessor is the first's new token, so exactly one rotate
// token is current (valid and not in a grace), the first's new token is in
// its grace, and each rotation wrote its own `rotated`. On Postgres the two
// run in two sessions, both held in the race: a third session locks the
// fleet's current rotate token's row, so the first rotation waits there
// with the fleet's lock held and the second waits for the fleet; without
// the fleet's lock, both would wait on the token's row, then both put it in
// its grace, leaving two current tokens.
func TestTwoConcurrentRotationsLeaveOneCurrentTokenAndOnePredecessorInItsGrace(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		blocker, err := s.Databases.Owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = blocker.Rollback(ctx) }()
		if _, err := blocker.Exec(ctx, `select 1 from telflo_opamp.enrollment_tokens
			where organization_id = $1 and id = $2 for update`, OrgA, c.RotateTokenID); err != nil {
			t.Fatal(err)
		}
		release := func() {
			s.WaitForLockWaiters(t, 2)
			if err := blocker.Commit(ctx); err != nil {
				t.Error(err)
			}
		}
		results := make([]enrollmenttokens.RotateResult, 2)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range 2 {
			at := Now.Add(time.Duration(i+1) * time.Minute)
			wg.Go(func() { results[i], errs[i] = s.Tokens.Rotate(ctx, rotation(c.Fleet.ID, at, Now.Add(time.Hour), nil)) })
		}
		release()
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("rotation %d: %v", i, err)
			}
		}

		at := Now.Add(3 * time.Minute)
		listed, err := s.Tokens.List(ctx, OrgA, c.Fleet.ID, at)
		if err != nil {
			t.Fatal(err)
		}
		var current, inGrace []uuid.UUID
		for _, l := range listed {
			if l.Kind != enrollmenttokens.KindRotate || !l.ValidAt(at) {
				continue
			}
			if l.DoRefresh {
				inGrace = append(inGrace, l.ID)
			} else {
				current = append(current, l.ID)
			}
		}
		if len(current) != 1 || len(inGrace) != 2 {
			t.Fatalf("current %v, in grace %v; want one current and two in grace (the first token and the first rotation's)", current, inGrace)
		}
		predecessors := map[uuid.UUID]bool{}
		for _, r := range results {
			if r.PredecessorID == nil {
				t.Fatalf("a rotation had no predecessor: %+v", results)
			}
			predecessors[*r.PredecessorID] = true
		}
		first, second := results[0], results[1]
		if *first.PredecessorID != c.RotateTokenID {
			first, second = second, first
		}
		if *first.PredecessorID != c.RotateTokenID || *second.PredecessorID != first.Token.ID || current[0] != second.Token.ID {
			t.Fatalf("the rotations' predecessors %v; want the first token, then the first rotation's", predecessors)
		}
		if active, err := s.Tokens.GetActiveRotate(ctx, OrgA, c.Fleet.ID, at); err != nil || active.ID != second.Token.ID {
			t.Fatalf("GetActiveRotate = %+v, %v", active, err)
		}
		rotated := map[uuid.UUID]int{}
		for _, e := range eventsOf(t, s, OrgA, c.Fleet.ID) {
			if e.Kind == enrollmenttokens.EventRotated {
				rotated[e.TokenID]++
			}
		}
		if len(rotated) != 2 || rotated[c.RotateTokenID] != 1 || rotated[first.Token.ID] != 1 {
			t.Fatalf("rotated events = %v", rotated)
		}
	})
}

// A rotation racing the current token's revocation never puts the token just
// revoked in a grace as its predecessor; it answers as when no rotate token
// is valid, and the revoked token keeps its expiry and no refresh. A third
// session revokes the token and holds its row (the revoke's transaction);
// the rotation's update waits on the row, then re-checks its where clause
// against what the revoke left.
func TestARotationRacingTheCurrentTokensRevokeNeverTakesItAsThePredecessor(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		revokedAt := Now.Add(time.Minute)
		blocker, err := s.Databases.Owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = blocker.Rollback(ctx) }()
		if _, err := blocker.Exec(ctx, `update telflo_opamp.enrollment_tokens set revoked_at = $3
			where organization_id = $1 and id = $2 and revoked_at is null`, OrgA, c.RotateTokenID, revokedAt); err != nil {
			t.Fatal(err)
		}
		release := func() {
			s.WaitForLockWaiters(t, 1)
			if err := blocker.Commit(ctx); err != nil {
				t.Error(err)
			}
		}
		var rotated enrollmenttokens.RotateResult
		var rotateErr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			rotated, rotateErr = s.Tokens.Rotate(ctx, rotation(c.Fleet.ID, Now.Add(2*time.Minute), Now.Add(time.Hour), nil))
		}()
		release()
		<-done
		if rotateErr != nil || rotated.PredecessorID != nil {
			t.Fatalf("Rotate = %+v, %v; want a new token and no predecessor", rotated, rotateErr)
		}
		revoked, err := s.Tokens.Get(ctx, OrgA, c.Fleet.ID, c.RotateTokenID)
		if err != nil || revoked.RevokedAt == nil || revoked.DoRefresh ||
			revoked.ExpiresAt == nil || !revoked.ExpiresAt.Equal(Millisecond(*c.RotateExpiresAt)) {
			t.Fatalf("the revoked token = %+v, %v; want its expiry kept and no refresh", revoked, err)
		}
		for _, e := range eventsOf(t, s, OrgA, c.Fleet.ID) {
			if e.Kind == enrollmenttokens.EventRotated {
				t.Fatalf("a rotated event for %s", e.TokenID)
			}
		}
	})
}

// Revoke: once, with an event; again AlreadyRevoked, no event, RevokedAt
// unchanged (one-way); the default token too.
func TestRevokeIsOnceAndAllowedOnTheDefaultToken(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		at := Now.Add(time.Hour)
		for _, id := range []uuid.UUID{c.RotateTokenID, c.DefaultTokenID} {
			result, tok, err := s.Tokens.Revoke(ctx, OrgA, c.Fleet.ID, id, Ana, at)
			if err != nil || result != enrollmenttokens.Revoked || tok.RevokedAt == nil || !tok.RevokedAt.Equal(Millisecond(at)) || tok.ValidAt(at) {
				t.Fatalf("Revoke = %v, %+v, %v", result, tok, err)
			}
			before := s.Dump(t, OrgA)
			result, tok, err = s.Tokens.Revoke(ctx, OrgA, c.Fleet.ID, id, Ana, at.Add(time.Hour))
			if err != nil || result != enrollmenttokens.AlreadyRevoked || !tok.RevokedAt.Equal(Millisecond(at)) {
				t.Fatalf("a second Revoke = %v, %+v, %v", result, tok, err)
			}
			s.Unchanged(t, "a second Revoke", OrgA, before)
		}
		revoked := 0
		for _, e := range eventsOf(t, s, OrgA, c.Fleet.ID) {
			if e.Kind == enrollmenttokens.EventRevoked {
				revoked++
				if !e.OccurredAt.Equal(Millisecond(at)) || *e.Actor != *Ana {
					t.Fatalf("a revoked event = %+v", e)
				}
			}
		}
		if revoked != 2 {
			t.Fatalf("%d revoked events, want 2", revoked)
		}
		if _, _, err := s.Tokens.Revoke(ctx, OrgA, c.Fleet.ID, uuid.New(), nil, at); !errors.Is(err, enrollmenttokens.ErrNotFound) {
			t.Fatalf("an unknown token = %v", err)
		}
		if _, _, err := s.Tokens.Revoke(ctx, OrgA, c.Fleet.ID, c.RotateTokenID, &fleets.Actor{ID: uuid.New()}, at); !errors.Is(err, enrollmenttokens.ErrInvalid) {
			t.Fatalf("an actor without an email = %v", err)
		}
	})
}

// RecordViewed: a `viewed` event for a valid token; a revoked or expired
// one is refused, writing nothing.
func TestRecordViewedAuditsAValidTokenOnly(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		at := Now.Add(time.Hour)
		tok, err := s.Tokens.RecordViewed(ctx, OrgA, c.Fleet.ID, c.RotateTokenID, Ana, at)
		if err != nil || tok.ID != c.RotateTokenID {
			t.Fatalf("RecordViewed = %+v, %v", tok, err)
		}
		if tok, err := s.Tokens.RecordViewed(ctx, OrgA, c.Fleet.ID, c.DefaultTokenID, nil, at); err != nil || tok.ID != c.DefaultTokenID {
			t.Fatalf("RecordViewed of the default token, by no one = %+v, %v", tok, err)
		}
		events := eventsOf(t, s, OrgA, c.Fleet.ID)
		viewed := map[uuid.UUID]*fleets.Actor{}
		for _, e := range events {
			if e.Kind == enrollmenttokens.EventViewed {
				if !e.OccurredAt.Equal(Millisecond(at)) {
					t.Fatalf("a viewed event = %+v", e)
				}
				viewed[e.TokenID] = e.Actor
			}
		}
		if len(viewed) != 2 || *viewed[c.RotateTokenID] != *Ana || viewed[c.DefaultTokenID] != nil {
			t.Fatalf("viewed events = %v", viewed)
		}

		before := s.Dump(t, OrgA)
		expiry := Now.AddDate(1, 0, 0)
		if _, err := s.Tokens.RecordViewed(ctx, OrgA, c.Fleet.ID, c.RotateTokenID, Ana, expiry); !errors.Is(err, enrollmenttokens.ErrExpired) {
			t.Fatalf("at its expiry = %v", err)
		}
		if _, err := s.Tokens.RecordViewed(ctx, OrgA, c.Fleet.ID, uuid.New(), Ana, at); !errors.Is(err, enrollmenttokens.ErrNotFound) {
			t.Fatalf("an unknown token = %v", err)
		}
		s.Unchanged(t, "the refused views", OrgA, before)
		if _, _, err := s.Tokens.Revoke(ctx, OrgA, c.Fleet.ID, c.RotateTokenID, Ana, at); err != nil {
			t.Fatal(err)
		}
		before = s.Dump(t, OrgA)
		if _, err := s.Tokens.RecordViewed(ctx, OrgA, c.Fleet.ID, c.RotateTokenID, Ana, at); !errors.Is(err, enrollmenttokens.ErrRevoked) {
			t.Fatalf("a revoked token = %v", err)
		}
		s.Unchanged(t, "a revoked token's view", OrgA, before)
	})
}

// Lookup: by the token's id alone, the row with its organization and fleet
// and the fleet's type and status, whatever the organization; ErrNotFound
// for an unknown id. Validity is the caller's to judge (ValidAt).
func TestLookupFindsATokenByItsIDWithItsFleetsTypeAndStatus(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		a := s.CreateFleet(t, OrgA, "edge-k8s", fleets.TypeKubernetesSupervisor)
		b := s.CreateFleet(t, OrgB, "globex", fleets.TypeServer)
		if _, err := s.Fleets.Update(ctx, OrgB, b.Fleet.ID, fleets.Changes{Status: Ptr(fleets.StatusArchived)}, Now); err != nil {
			t.Fatal(err)
		}
		got, err := s.Tokens.Lookup(ctx, a.RotateTokenID)
		if err != nil || got.ID != a.RotateTokenID || got.OrganizationID != OrgA || got.FleetID != a.Fleet.ID ||
			got.Kind != enrollmenttokens.KindRotate || got.FleetType != fleets.TypeKubernetesSupervisor ||
			got.FleetStatus != fleets.StatusActive || !got.ValidAt(Now) {
			t.Fatalf("Lookup(A's) = %+v, %v", got, err)
		}
		got, err = s.Tokens.Lookup(ctx, b.DefaultTokenID)
		if err != nil || got.OrganizationID != OrgB || got.FleetID != b.Fleet.ID || got.Kind != enrollmenttokens.KindDefault ||
			got.FleetType != fleets.TypeServer || got.FleetStatus != fleets.StatusArchived {
			t.Fatalf("Lookup(B's) = %+v, %v", got, err)
		}
		if _, _, err := s.Tokens.Revoke(ctx, OrgA, a.Fleet.ID, a.RotateTokenID, nil, Now); err != nil {
			t.Fatal(err)
		}
		if got, err := s.Tokens.Lookup(ctx, a.RotateTokenID); err != nil || got.ValidAt(Now) {
			t.Fatalf("a revoked token's Lookup = %+v, %v; want found and not valid", got, err)
		}
		if _, err := s.Tokens.Lookup(ctx, uuid.New()); !errors.Is(err, enrollmenttokens.ErrNotFound) {
			t.Fatalf("an unknown id = %v", err)
		}
	})
}

// Every method but Lookup, called with another organization's fleet or token
// (or a token under another fleet), answers not found and changes nothing of
// either organization's.
func TestAnotherOrganizationsTokenIsNotFoundAndUnchanged(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		mine := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		theirs := s.CreateFleet(t, OrgB, "globex", fleets.TypeServer)
		s.InsertCollector(t, Collector{ID: uuid.New(), OrganizationID: OrgB, FleetID: theirs.Fleet.ID, GroupID: theirs.DefaultGroupID,
			Name: "globex-01", TokenID: &theirs.RotateTokenID, LastSeenAt: Now})
		beforeA, beforeB := s.Dump(t, OrgA), s.Dump(t, OrgB)
		at := Now.Add(time.Hour)

		for _, fleet := range []uuid.UUID{theirs.Fleet.ID, mine.Fleet.ID} {
			for _, token := range []uuid.UUID{theirs.RotateTokenID, theirs.DefaultTokenID} {
				if _, err := s.Tokens.Get(ctx, OrgA, fleet, token); !errors.Is(err, enrollmenttokens.ErrNotFound) {
					t.Errorf("Get = %v", err)
				}
				if _, _, err := s.Tokens.Revoke(ctx, OrgA, fleet, token, Ana, at); !errors.Is(err, enrollmenttokens.ErrNotFound) {
					t.Errorf("Revoke = %v", err)
				}
				if _, err := s.Tokens.RecordViewed(ctx, OrgA, fleet, token, Ana, at); !errors.Is(err, enrollmenttokens.ErrNotFound) {
					t.Errorf("RecordViewed = %v", err)
				}
			}
		}
		// A's own tokens, named under B's fleet, are not found either.
		if _, _, err := s.Tokens.Revoke(ctx, OrgA, theirs.Fleet.ID, mine.RotateTokenID, Ana, at); !errors.Is(err, enrollmenttokens.ErrNotFound) {
			t.Errorf("Revoke of A's token under B's fleet = %v", err)
		}
		if _, err := s.Tokens.List(ctx, OrgA, theirs.Fleet.ID, at); !errors.Is(err, enrollmenttokens.ErrFleetNotFound) {
			t.Errorf("List = %v", err)
		}
		if _, err := s.Tokens.ListEvents(ctx, OrgA, theirs.Fleet.ID, enrollmenttokens.MaxListLimit, nil); !errors.Is(err, enrollmenttokens.ErrFleetNotFound) {
			t.Errorf("ListEvents = %v", err)
		}
		// Nor past a cursor: B's fleet's events, named by A, are none of A's.
		theirCursor := &enrollmenttokens.EventCursor{OccurredAt: Now.Add(time.Hour), ID: uuid.Max}
		if _, err := s.Tokens.ListEvents(ctx, OrgA, theirs.Fleet.ID, 1, theirCursor); !errors.Is(err, enrollmenttokens.ErrFleetNotFound) {
			t.Errorf("ListEvents past a cursor = %v", err)
		}
		if _, err := s.Tokens.GetActiveRotate(ctx, OrgA, theirs.Fleet.ID, at); !errors.Is(err, enrollmenttokens.ErrNotFound) {
			t.Errorf("GetActiveRotate = %v", err)
		}
		// RotateStates leaves B's fleet out, and answers A's.
		if states, err := s.Tokens.RotateStates(ctx, OrgA, []uuid.UUID{theirs.Fleet.ID, mine.Fleet.ID}, at); err != nil ||
			len(states) != 1 || states[mine.Fleet.ID].ValidCount != 1 {
			t.Errorf("RotateStates with B's fleet = %+v, %v; want A's alone", states, err)
		}
		if _, err := s.Tokens.Rotate(ctx, rotation(theirs.Fleet.ID, at, at.Add(time.Hour), nil)); !errors.Is(err, enrollmenttokens.ErrFleetNotFound) {
			t.Errorf("Rotate = %v", err)
		}
		// A's own list counts none of B's collectors.
		listed, err := s.Tokens.List(ctx, OrgA, mine.Fleet.ID, Now)
		if err != nil || len(listed) != 2 || listed[0].ActiveCollectors+listed[1].ActiveCollectors != 0 {
			t.Errorf("A's List = %+v, %v", listed, err)
		}
		s.Unchanged(t, "A's calls", OrgA, beforeA)
		s.Unchanged(t, "A's calls", OrgB, beforeB)
	})
}

// Within one organization, a token of another fleet is not found through
// this fleet, and nothing changes: Get, Revoke and RecordViewed name the
// fleet in their filter, not only the organization.
func TestAnotherFleetsTokenIsNotFoundAndUnchanged(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		mine := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		other := s.CreateFleet(t, OrgA, "edge-dev", fleets.TypeServer)
		before := s.Dump(t, OrgA)
		for _, id := range []uuid.UUID{other.DefaultTokenID, other.RotateTokenID} {
			if _, err := s.Tokens.Get(ctx, OrgA, mine.Fleet.ID, id); !errors.Is(err, enrollmenttokens.ErrNotFound) {
				t.Errorf("Get through another fleet = %v, want ErrNotFound", err)
			}
			if _, _, err := s.Tokens.Revoke(ctx, OrgA, mine.Fleet.ID, id, Ana, Now.Add(time.Hour)); !errors.Is(err, enrollmenttokens.ErrNotFound) {
				t.Errorf("Revoke through another fleet = %v, want ErrNotFound", err)
			}
			if _, err := s.Tokens.RecordViewed(ctx, OrgA, mine.Fleet.ID, id, Ana, Now.Add(time.Hour)); !errors.Is(err, enrollmenttokens.ErrNotFound) {
				t.Errorf("RecordViewed through another fleet = %v, want ErrNotFound", err)
			}
		}
		s.Unchanged(t, "calls naming another fleet", OrgA, before)
	})
}

// A token's Revoke racing its fleet's Delete. A Revoke that locked the token
// first (here, after waiting on a third session holding it) and then
// key-shared the fleet for its event, while the fleet's Delete held the
// fleet and cascaded into the tokens, would deadlock (40P01). Fleet first,
// both finish: the Revoke, there first, revokes, and the fleet's Delete then
// takes everything.
func TestARevokeRacingItsFleetsDeleteDoesNotDeadlock(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		blocker, err := s.Databases.Owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = blocker.Rollback(ctx) }()
		if _, err := blocker.Exec(ctx, `select 1 from telflo_opamp.enrollment_tokens
			where organization_id = $1 and id = $2 for update`, OrgA, c.RotateTokenID); err != nil {
			t.Fatal(err)
		}
		release := func() {
			s.WaitForLockWaiters(t, 2)
			if err := blocker.Commit(ctx); err != nil {
				t.Error(err)
			}
		}
		var result enrollmenttokens.RevokeResult
		var fleetDeleted bool
		var revokeErr, fleetErr error
		revokeDone, fleetDone := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(revokeDone)
			result, _, revokeErr = s.Tokens.Revoke(ctx, OrgA, c.Fleet.ID, c.RotateTokenID, Ana, Now.Add(time.Hour))
		}()
		s.WaitForLockWaiters(t, 1)
		go func() {
			defer close(fleetDone)
			fleetDeleted, fleetErr = s.Fleets.Delete(ctx, OrgA, c.Fleet.ID)
		}()
		release()
		<-revokeDone
		<-fleetDone
		if revokeErr != nil || fleetErr != nil {
			t.Fatalf("the Revoke = %v, %v; the fleet's Delete = %v, %v; want both to finish", result, revokeErr, fleetDeleted, fleetErr)
		}
		if result != enrollmenttokens.Revoked || !fleetDeleted {
			t.Fatalf("the Revoke = %v, the fleet's Delete = %v; want both (the Revoke first)", result, fleetDeleted)
		}
		for table, n := range s.FleetRows(t, c.Fleet.ID) {
			if n != 0 {
				t.Errorf("%d rows of %s left", n, table)
			}
		}
	})
}

// A token's reveal (RecordViewed) racing its fleet's Delete, as the Revoke
// above, in both orders, and never refused 40P01 or 23503:
//
//   - the reveal first: it waits on a third session holding the token
//     (its event's insert key-shares the token), with the fleet already
//     key-shared; the fleet's Delete queues behind it; the reveal records
//     its view, and the Delete then takes everything;
//   - the Delete first: it holds the fleet and waits to cascade into the
//     token the third session holds; the reveal, fleet first, queues behind
//     it and then finds no token (ErrNotFound, the route's 404). Without the
//     fleet's lock first, the reveal's event would be refused by its fleet's
//     key once the Delete commits (23503, a 500).
func TestARevealRacingItsFleetsDeleteDoesNotDeadlock(t *testing.T) {
	for _, revealFirst := range []bool{true, false} {
		name := "the Delete first"
		if revealFirst {
			name = "the reveal first"
		}
		t.Run(name, func(t *testing.T) {
			Each(t, testDatabase, func(t *testing.T, s *Stores) {
				raceARevealAndItsFleetsDelete(t, s, revealFirst)
			})
		})
	}
}

func raceARevealAndItsFleetsDelete(t *testing.T, s *Stores, revealFirst bool) {
	c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
	blocker, err := s.Databases.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, `select 1 from telflo_opamp.enrollment_tokens
		where organization_id = $1 and id = $2 for update`, OrgA, c.RotateTokenID); err != nil {
		t.Fatal(err)
	}
	release := func() {
		s.WaitForLockWaiters(t, 2)
		if err := blocker.Commit(ctx); err != nil {
			t.Error(err)
		}
	}
	var viewed enrollmenttokens.Token
	var fleetDeleted bool
	var viewErr, fleetErr error
	viewDone, fleetDone := make(chan struct{}), make(chan struct{})
	reveal := func() {
		defer close(viewDone)
		viewed, viewErr = s.Tokens.RecordViewed(ctx, OrgA, c.Fleet.ID, c.RotateTokenID, Ana, Now.Add(time.Hour))
	}
	deleteFleet := func() {
		defer close(fleetDone)
		fleetDeleted, fleetErr = s.Fleets.Delete(ctx, OrgA, c.Fleet.ID)
	}
	first, second := reveal, deleteFleet
	if !revealFirst {
		first, second = deleteFleet, reveal
	}
	go first()
	s.WaitForLockWaiters(t, 1)
	go second()
	release()
	<-viewDone
	<-fleetDone
	if fleetErr != nil || !fleetDeleted {
		t.Fatalf("the fleet's Delete = %v, %v; want it done", fleetDeleted, fleetErr)
	}
	switch {
	case revealFirst && (viewErr != nil || viewed.ID != c.RotateTokenID):
		t.Fatalf("the reveal, first = %+v, %v; want the view recorded", viewed, viewErr)
	case !revealFirst && !errors.Is(viewErr, enrollmenttokens.ErrNotFound):
		t.Fatalf("the reveal, after the Delete = %v; want ErrNotFound", viewErr)
	}
	for table, n := range s.FleetRows(t, c.Fleet.ID) {
		if n != 0 {
			t.Errorf("%d rows of %s left", n, table)
		}
	}
}

// rotateStateText is states as text, each fleet by its name, its times in
// UTC: RotateState holds pointers, which Describe would print as addresses.
func rotateStateText(states map[uuid.UUID]enrollmenttokens.RotateState, names map[uuid.UUID]string) string {
	text := func(t *time.Time) string {
		if t == nil {
			return "null"
		}
		return t.UTC().Format(time.RFC3339Nano)
	}
	lines := make([]string, 0, len(states))
	for id, s := range states {
		name, found := names[id]
		if !found {
			name = id.String()
		}
		lines = append(lines, fmt.Sprintf("%s: expires %s, grace ends %s, %d valid", name, text(s.ExpiresAt), text(s.GraceEndsAt), s.ValidCount))
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

// RotateStates answers, for each of the organization's fleets with a valid
// rotate token, the current token's expiry (GetActiveRotate's), the latest
// expiry among the other valid tokens in their grace, and how many rotate
// tokens are valid, at the caller's clock (a token is valid strictly before
// its expiry instant); a fleet with none, another organization's fleet and
// an unknown id have no entry, and a default token never counts.
func TestRotateStatesAnswerTheCurrentTokenItsGraceAndTheValidCount(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		names := map[uuid.UUID]string{}
		fleet := func(org uuid.UUID, label string, expires *time.Time) uuid.UUID {
			t.Helper()
			result, created, err := s.Fleets.Create(ctx, fleets.NewFleet{ID: uuid.New(), OrganizationID: org, Label: label,
				Type: fleets.TypeServer, CreatedBy: Ana, RotateExpiresAt: expires, At: Now})
			if err != nil || result != fleets.Created {
				t.Fatalf("creating %s: %v, %v", label, result, err)
			}
			names[created.Fleet.ID] = label
			return created.Fleet.ID
		}
		rotate := func(id uuid.UUID, at, grace time.Time, expires *time.Time) enrollmenttokens.RotateResult {
			t.Helper()
			r, err := s.Tokens.Rotate(ctx, rotation(id, at, grace, expires))
			if err != nil {
				t.Fatal(err)
			}
			return r
		}
		year := Now.AddDate(1, 0, 0)
		dated := fleet(OrgA, "dated", &year)
		never := fleet(OrgA, "never", nil)
		revoked := fleet(OrgA, "revoked", &year)
		theirs := fleet(OrgB, "theirs", &year)
		if _, _, err := s.Tokens.Revoke(ctx, OrgA, revoked, rotateTokenOf(t, s, revoked), Ana, Now); err != nil {
			t.Fatal(err)
		}

		// Several rotations: the newest is current, the grace the latest of
		// its predecessors'.
		several := fleet(OrgA, "several", &year)
		half := Now.AddDate(0, 6, 0)
		rotate(several, Now.Add(time.Minute), Now.Add(2*time.Hour), &half)
		rotate(several, Now.Add(90*time.Minute), Now.Add(3*time.Hour), nil)

		// A grace end past the predecessor's own expiry is capped by it.
		capped := fleet(OrgA, "capped", &year)
		rotate(capped, Now.Add(time.Minute), Now.AddDate(2, 0, 0), nil)

		// The current token revoked mid-grace: the predecessor in its grace
		// is current again, and no grace is shown (nothing rotates to it).
		fallback := fleet(OrgA, "fallback", &year)
		successor := rotate(fallback, Now.Add(time.Minute), Now.Add(2*time.Hour), &half)
		if _, _, err := s.Tokens.Revoke(ctx, OrgA, fallback, successor.Token.ID, Ana, Now.Add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}

		// A rotation whose new token expires soon after its grace.
		expiring := fleet(OrgA, "expiring", &year)
		rotate(expiring, Now.Add(time.Minute), Now.Add(time.Hour), Ptr(Now.Add(2*time.Hour)))

		all := []uuid.UUID{dated, never, revoked, theirs, several, capped, fallback, expiring, uuid.New(), dated}
		ms := func(t time.Time) *time.Time { return Ptr(Millisecond(t)) }
		for _, step := range []struct {
			at   time.Time
			want map[uuid.UUID]enrollmenttokens.RotateState
		}{
			{Now.Add(100 * time.Minute), map[uuid.UUID]enrollmenttokens.RotateState{
				dated:    {ExpiresAt: ms(year), ValidCount: 1},
				never:    {ValidCount: 1},
				several:  {GraceEndsAt: ms(Now.Add(3 * time.Hour)), ValidCount: 3},
				capped:   {GraceEndsAt: ms(year), ValidCount: 2},
				fallback: {ExpiresAt: ms(Now.Add(2 * time.Hour)), ValidCount: 1},
				expiring: {ExpiresAt: ms(Now.Add(2 * time.Hour)), ValidCount: 1},
			}},
			// The first grace over: the second's still running.
			{Now.Add(2 * time.Hour), map[uuid.UUID]enrollmenttokens.RotateState{
				dated:   {ExpiresAt: ms(year), ValidCount: 1},
				never:   {ValidCount: 1},
				several: {GraceEndsAt: ms(Now.Add(3 * time.Hour)), ValidCount: 2},
				capped:  {GraceEndsAt: ms(year), ValidCount: 2},
			}},
			// After every grace.
			{Now.Add(3 * time.Hour), map[uuid.UUID]enrollmenttokens.RotateState{
				dated:   {ExpiresAt: ms(year), ValidCount: 1},
				never:   {ValidCount: 1},
				several: {ValidCount: 1},
				capped:  {GraceEndsAt: ms(year), ValidCount: 2},
			}},
			// A millisecond before a year: still valid; at the instant: not.
			{year.Add(-time.Millisecond), map[uuid.UUID]enrollmenttokens.RotateState{
				dated:   {ExpiresAt: ms(year), ValidCount: 1},
				never:   {ValidCount: 1},
				several: {ValidCount: 1},
				capped:  {GraceEndsAt: ms(year), ValidCount: 2},
			}},
			{year, map[uuid.UUID]enrollmenttokens.RotateState{
				never:   {ValidCount: 1},
				several: {ValidCount: 1},
				capped:  {ValidCount: 1},
			}},
		} {
			got, err := s.Tokens.RotateStates(ctx, OrgA, all, step.at)
			if err != nil {
				t.Fatal(err)
			}
			if rotateStateText(got, names) != rotateStateText(step.want, names) {
				t.Fatalf("at %s, RotateStates =\n%s\nwant\n%s", step.at, rotateStateText(got, names), rotateStateText(step.want, names))
			}
			// The current token is GetActiveRotate's.
			for _, id := range all[:8] {
				active, err := s.Tokens.GetActiveRotate(ctx, OrgA, id, step.at)
				state, found := got[id]
				switch {
				case errors.Is(err, enrollmenttokens.ErrNotFound) && !found:
				case err != nil || !found:
					t.Fatalf("at %s, %s: GetActiveRotate = %v, an entry: %v", step.at, names[id], err, found)
				case (active.ExpiresAt == nil) != (state.ExpiresAt == nil) ||
					(active.ExpiresAt != nil && !active.ExpiresAt.Equal(*state.ExpiresAt)):
					t.Fatalf("at %s, %s: GetActiveRotate's expiry %v, the state's %v", step.at, names[id], active.ExpiresAt, state.ExpiresAt)
				}
			}
		}

		if got, err := s.Tokens.RotateStates(ctx, OrgB, []uuid.UUID{theirs, dated}, Now); err != nil ||
			rotateStateText(got, names) != "theirs: expires "+year.Truncate(time.Millisecond).Format(time.RFC3339Nano)+", grace ends null, 1 valid" {
			t.Fatalf("B's = %s, %v", rotateStateText(got, names), err)
		}
		if got, err := s.Tokens.RotateStates(ctx, OrgA, nil, Now); err != nil || got == nil || len(got) != 0 {
			t.Fatalf("no fleets = %v, %v", got, err)
		}
		many := make([]uuid.UUID, fleets.MaxListLimit+1)
		if _, err := s.Tokens.RotateStates(ctx, OrgA, many, Now); !errors.Is(err, enrollmenttokens.ErrInvalid) {
			t.Fatalf("%d fleets = %v", len(many), err)
		}
		if _, err := s.Tokens.RotateStates(ctx, OrgA, many[:fleets.MaxListLimit], Now); err != nil {
			t.Fatalf("%d fleets = %v", fleets.MaxListLimit, err)
		}
	})
}

// rotateTokenOf is the fleet's first rotate token's id.
func rotateTokenOf(t *testing.T, s *Stores, fleetID uuid.UUID) uuid.UUID {
	t.Helper()
	listed, err := s.Tokens.List(ctx, OrgA, fleetID, Now)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range listed {
		if l.Kind == enrollmenttokens.KindRotate {
			return l.ID
		}
	}
	t.Fatal("no rotate token")
	return uuid.Nil
}

// ListEvents: newest first by (OccurredAt, id), both descending, by keyset:
// pages of any size join into the whole list with no event twice or
// missed, ties at one instant (a fleet's two `issued`) broken by id; the
// last page's Next is nil; a limit outside 1 to 100 is ErrInvalid; another
// fleet's events never show.
func TestListEventsPagesNewestFirstByKeyset(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		other := s.CreateFleet(t, OrgA, "edge-dev", fleets.TypeServer)
		at := Now.Add(time.Minute)
		if _, err := s.Tokens.RecordViewed(ctx, OrgA, c.Fleet.ID, c.DefaultTokenID, Ana, at); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Tokens.Rotate(ctx, rotation(c.Fleet.ID, at.Add(time.Minute), at.Add(time.Hour), nil)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Tokens.Revoke(ctx, OrgA, c.Fleet.ID, c.DefaultTokenID, Ana, at.Add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Tokens.RecordViewed(ctx, OrgA, other.Fleet.ID, other.DefaultTokenID, Ana, at); err != nil {
			t.Fatal(err)
		}

		whole := eventsOf(t, s, OrgA, c.Fleet.ID)
		// issued ×2 (create), viewed, rotated + issued (rotate), revoked.
		if len(whole) != 6 {
			t.Fatalf("the fleet's events = %s", Describe(whole))
		}
		for i := 1; i < len(whole); i++ {
			a, b := whole[i-1], whole[i]
			if a.OccurredAt.Before(b.OccurredAt) || (a.OccurredAt.Equal(b.OccurredAt) && strings.Compare(string(a.ID[:]), string(b.ID[:])) <= 0) {
				t.Fatalf("events %d and %d out of order: %s", i-1, i, Describe(whole))
			}
		}
		if whole[0].Kind != enrollmenttokens.EventRevoked || whole[5].Kind != enrollmenttokens.EventIssued {
			t.Fatalf("newest %s, oldest %s", whole[0].Kind, whole[5].Kind)
		}
		for limit := 1; limit <= 7; limit++ {
			var joined []enrollmenttokens.Event
			var after *enrollmenttokens.EventCursor
			for pages := 0; ; pages++ {
				if pages > len(whole) {
					t.Fatalf("limit %d: no last page", limit)
				}
				page, err := s.Tokens.ListEvents(ctx, OrgA, c.Fleet.ID, limit, after)
				if err != nil || len(page.Events) > limit {
					t.Fatalf("limit %d: page = %s, %v", limit, Describe(page), err)
				}
				joined = append(joined, page.Events...)
				if page.Next == nil {
					break
				}
				parsed, err := enrollmenttokens.ParseEventCursor(page.Next.String())
				if err != nil || parsed != *page.Next {
					t.Fatalf("limit %d: the cursor round trip = %+v, %v", limit, parsed, err)
				}
				after = &parsed
			}
			if !slices.EqualFunc(joined, whole, sameEvent) {
				t.Fatalf("limit %d: joined pages = %s, want %s", limit, Describe(joined), Describe(whole))
			}
		}
		// Past the last event: an empty page of a fleet that is the organization's.
		last := whole[len(whole)-1]
		page, err := s.Tokens.ListEvents(ctx, OrgA, c.Fleet.ID, 1, &enrollmenttokens.EventCursor{OccurredAt: last.OccurredAt, ID: last.ID})
		if err != nil || len(page.Events) != 0 || page.Next != nil || page.Events == nil {
			t.Fatalf("past the last = %s, %v", Describe(page), err)
		}
		for _, limit := range []int{0, -1, enrollmenttokens.MaxListLimit + 1} {
			if _, err := s.Tokens.ListEvents(ctx, OrgA, c.Fleet.ID, limit, nil); !errors.Is(err, enrollmenttokens.ErrInvalid) {
				t.Errorf("limit %d = %v", limit, err)
			}
		}
		if _, err := s.Tokens.ListEvents(ctx, OrgA, uuid.New(), 1, nil); !errors.Is(err, enrollmenttokens.ErrFleetNotFound) {
			t.Errorf("an unknown fleet = %v", err)
		}
	})
}

// An event cursor round-trips, and nothing else parses: not base64url, the
// wrong length, a time not a whole millisecond or outside 0001 to 9999.
func TestAnEventCursorRoundTripsAndNothingElseParses(t *testing.T) {
	c := enrollmenttokens.EventCursor{OccurredAt: Millisecond(Now), ID: uuid.New()}
	if parsed, err := enrollmenttokens.ParseEventCursor(c.String()); err != nil || parsed != c {
		t.Fatalf("round trip = %+v, %v", parsed, err)
	}
	if n := len(c.String()); n != 32 {
		t.Fatalf("a cursor is %d characters, want 32", n)
	}
	micro := enrollmenttokens.EventCursor{OccurredAt: Millisecond(Now).Add(time.Microsecond), ID: c.ID}
	early := enrollmenttokens.EventCursor{OccurredAt: time.Date(0, time.December, 31, 0, 0, 0, 0, time.UTC), ID: c.ID}
	late := enrollmenttokens.EventCursor{OccurredAt: time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC), ID: c.ID}
	for _, bad := range []string{"", "x", c.String() + "A", c.String()[:31], c.String() + "=", "!" + c.String()[1:],
		micro.String(), early.String(), late.String()} {
		if _, err := enrollmenttokens.ParseEventCursor(bad); !errors.Is(err, enrollmenttokens.ErrInvalidCursor) {
			t.Errorf("ParseEventCursor(%q) = %v", bad, err)
		}
	}
}

// sameEvent compares two events by value, their actors' too.
func sameEvent(a, b enrollmenttokens.Event) bool {
	sameActor := (a.Actor == nil) == (b.Actor == nil) && (a.Actor == nil || *a.Actor == *b.Actor)
	return a.ID == b.ID && a.OrganizationID == b.OrganizationID && a.FleetID == b.FleetID && a.TokenID == b.TokenID &&
		a.Kind == b.Kind && a.OccurredAt.Equal(b.OccurredAt) && sameActor
}

// IssueDefault makes a new default token only when the fleet's default
// tokens are all revoked, with an `issued` event naming the actor; the
// revoked one stays revoked; the new one authenticates.
func TestIssueDefaultFollowsARevokedDefaultTokenOnly(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		at := Now.Add(time.Hour)
		before := s.Dump(t, OrgA)
		if _, err := s.Tokens.IssueDefault(ctx, OrgA, c.Fleet.ID, Ana, at); !errors.Is(err, enrollmenttokens.ErrDefaultExists) {
			t.Fatalf("IssueDefault with a valid default token = %v, want ErrDefaultExists", err)
		}
		s.Unchanged(t, "a refused IssueDefault", OrgA, before)

		if _, _, err := s.Tokens.Revoke(ctx, OrgA, c.Fleet.ID, c.DefaultTokenID, Ana, at); err != nil {
			t.Fatal(err)
		}
		issued, err := s.Tokens.IssueDefault(ctx, OrgA, c.Fleet.ID, Ana, at.Add(time.Minute))
		if err != nil || issued.Kind != enrollmenttokens.KindDefault || issued.ExpiresAt != nil || issued.RevokedAt != nil ||
			issued.DoRefresh || issued.ID == c.DefaultTokenID || *issued.CreatedBy != *Ana || !issued.CreatedAt.Equal(Millisecond(at.Add(time.Minute))) {
			t.Fatalf("IssueDefault after the revoke = %+v, %v", issued, err)
		}
		if got, err := s.Tokens.Lookup(ctx, issued.ID); err != nil || got.Kind != enrollmenttokens.KindDefault || !got.ValidAt(at.Add(time.Hour)) {
			t.Fatalf("Lookup of the new default token = %+v, %v", got, err)
		}
		if old, err := s.Tokens.Get(ctx, OrgA, c.Fleet.ID, c.DefaultTokenID); err != nil || old.RevokedAt == nil {
			t.Fatalf("the revoked default token = %+v, %v; want it still revoked", old, err)
		}
		var issuedEvents int
		for _, e := range eventsOf(t, s, OrgA, c.Fleet.ID) {
			if e.Kind == enrollmenttokens.EventIssued && e.TokenID == issued.ID {
				issuedEvents++
				if *e.Actor != *Ana || !e.OccurredAt.Equal(Millisecond(at.Add(time.Minute))) {
					t.Fatalf("the issued event = %+v", e)
				}
			}
		}
		if issuedEvents != 1 {
			t.Fatalf("%d issued events for the new token, want 1", issuedEvents)
		}
		// Now the fleet has a valid default token again.
		if _, err := s.Tokens.IssueDefault(ctx, OrgA, c.Fleet.ID, nil, at.Add(2*time.Minute)); !errors.Is(err, enrollmenttokens.ErrDefaultExists) {
			t.Fatalf("a second IssueDefault = %v, want ErrDefaultExists", err)
		}
		// And after revoking the new one, a third may follow.
		if _, _, err := s.Tokens.Revoke(ctx, OrgA, c.Fleet.ID, issued.ID, nil, at.Add(3*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Tokens.IssueDefault(ctx, OrgA, c.Fleet.ID, nil, at.Add(4*time.Minute)); err != nil {
			t.Fatalf("a third default token: %v", err)
		}
	})
}

// IssueDefault's refusals change nothing: an archived fleet (before its
// valid default token is looked at), another organization's fleet (its
// tokens all revoked), an unknown fleet, an actor without an email.
func TestIssueDefaultRefusesAndChangesNothing(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		archived := s.CreateFleet(t, OrgA, "edge-old", fleets.TypeServer)
		if _, err := s.Fleets.Update(ctx, OrgA, archived.Fleet.ID, fleets.Changes{Status: Ptr(fleets.StatusArchived)}, Now); err != nil {
			t.Fatal(err)
		}
		mine := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		if _, _, err := s.Tokens.Revoke(ctx, OrgA, mine.Fleet.ID, mine.DefaultTokenID, nil, Now); err != nil {
			t.Fatal(err)
		}
		theirs := s.CreateFleet(t, OrgB, "globex", fleets.TypeServer)
		if _, _, err := s.Tokens.Revoke(ctx, OrgB, theirs.Fleet.ID, theirs.DefaultTokenID, nil, Now); err != nil {
			t.Fatal(err)
		}
		at := Now.Add(time.Hour)
		before, beforeB := s.Dump(t, OrgA), s.Dump(t, OrgB)
		for _, r := range []struct {
			name         string
			organization uuid.UUID
			fleet        uuid.UUID
			actor        *fleets.Actor
			want         error
		}{
			{"an archived fleet with a valid default token", OrgA, archived.Fleet.ID, Ana, enrollmenttokens.ErrFleetArchived},
			{"another organization's fleet", OrgA, theirs.Fleet.ID, Ana, enrollmenttokens.ErrFleetNotFound},
			{"an unknown fleet", OrgA, uuid.New(), Ana, enrollmenttokens.ErrFleetNotFound},
			{"an actor without an email", OrgA, mine.Fleet.ID, &fleets.Actor{ID: uuid.New()}, enrollmenttokens.ErrInvalid},
		} {
			if _, err := s.Tokens.IssueDefault(ctx, r.organization, r.fleet, r.actor, at); !errors.Is(err, r.want) {
				t.Errorf("%s: IssueDefault = %v, want %v", r.name, err, r.want)
			}
		}
		s.Unchanged(t, "the refused issues", OrgA, before)
		s.Unchanged(t, "the refused issues", OrgB, beforeB)
	})
}

// Two issues racing for one fleet: one makes the token, the other answers
// ErrDefaultExists, and the fleet holds one valid default token. On
// Postgres both wait on a third session's lock on the fleet's row, so they
// really race once it lets go.
func TestTwoConcurrentIssuesMakeOneDefaultToken(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		if _, _, err := s.Tokens.Revoke(ctx, OrgA, c.Fleet.ID, c.DefaultTokenID, nil, Now); err != nil {
			t.Fatal(err)
		}
		blocker, err := s.Databases.Owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = blocker.Rollback(ctx) }()
		if _, err := blocker.Exec(ctx, `select 1 from telflo_opamp.fleets
			where organization_id = $1 and id = $2 for no key update`, OrgA, c.Fleet.ID); err != nil {
			t.Fatal(err)
		}
		release := func() {
			s.WaitForLockWaiters(t, 2)
			if err := blocker.Commit(ctx); err != nil {
				t.Error(err)
			}
		}
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range 2 {
			wg.Go(func() { _, errs[i] = s.Tokens.IssueDefault(ctx, OrgA, c.Fleet.ID, Ana, Now.Add(time.Minute)) })
		}
		release()
		wg.Wait()
		made, exists := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				made++
			case errors.Is(err, enrollmenttokens.ErrDefaultExists):
				exists++
			default:
				t.Fatalf("an issue: %v", err)
			}
		}
		if made != 1 || exists != 1 {
			t.Fatalf("errors %v; want one made and one ErrDefaultExists", errs)
		}
		listed, err := s.Tokens.List(ctx, OrgA, c.Fleet.ID, Now.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		valid := 0
		for _, l := range listed {
			if l.Kind == enrollmenttokens.KindDefault && l.RevokedAt == nil {
				valid++
			}
		}
		if valid != 1 {
			t.Fatalf("%d valid default tokens, want 1", valid)
		}
	})
}

// A revoke of the valid default token racing an issue: on Postgres the
// revoke holds the token's row uncommitted; the issue's insert waits on
// the index's conflict check and, once the revoke commits, goes in.
func TestAnIssueRacingTheDefaultTokensRevokeWaitsAndIssues(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		blocker, err := s.Databases.Owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = blocker.Rollback(ctx) }()
		if _, err := blocker.Exec(ctx, `update telflo_opamp.enrollment_tokens set revoked_at = $3
			where organization_id = $1 and id = $2`, OrgA, c.DefaultTokenID, Now); err != nil {
			t.Fatal(err)
		}
		var issueErr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, issueErr = s.Tokens.IssueDefault(ctx, OrgA, c.Fleet.ID, Ana, Now.Add(time.Minute))
		}()
		s.WaitForLockWaiters(t, 1)
		if err := blocker.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		<-done
		if issueErr != nil {
			t.Fatalf("the issue racing the revoke = %v, want a new token", issueErr)
		}
	})
}

// The lock order rule's other exception (CONTRIBUTING.md, "SQL"):
// IssueDefault takes the fleet's row `for no key update`, as Rotate does,
// after the organization's `for key share`. Racing its fleet's or its
// organization's delete, in both orders, it never deadlocks (40P01). On
// Postgres a third session holds the fleet's row `for no key update` (as
// another issue or a rotation would); the first of the two waits on it,
// then the second: on the fleet's row too in the fleet's two cases, but on
// the ORGANIZATION's row in the organization's two (its delete's `for
// update` behind the issue's `for key share`, or the issue's `for key
// share` behind its delete's `for update`). Released, they finish one after
// the other. The deletes run with no 40P01 retry, so a deadlock fails the
// test rather than being retried away.
//
//   - the issue first: it issues the new default token, and the delete then
//     takes everything;
//   - the delete first: it deletes, and the issue then finds no fleet
//     (ErrFleetNotFound, the route's 404).
func TestAnIssueRacingItsFleetsOrOrganizationsDeleteDoesNotDeadlock(t *testing.T) {
	for _, deleted := range []string{"fleet", "organization"} {
		for _, issueFirst := range []bool{true, false} {
			name := "the " + deleted + "'s delete first"
			if issueFirst {
				name = "the issue first, then the " + deleted + "'s delete"
			}
			t.Run(name, func(t *testing.T) {
				Each(t, testDatabase, func(t *testing.T, s *Stores) {
					raceAnIssueAndADelete(t, s, deleted, issueFirst)
				})
			})
		}
	}
}

func raceAnIssueAndADelete(t *testing.T, s *Stores, deleted string, issueFirst bool) {
	c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
	if _, _, err := s.Tokens.Revoke(ctx, OrgA, c.Fleet.ID, c.DefaultTokenID, Ana, Now); err != nil {
		t.Fatal(err)
	}
	blocker, err := s.Databases.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, `select 1 from telflo_opamp.fleets
		where organization_id = $1 and id = $2 for no key update`, OrgA, c.Fleet.ID); err != nil {
		t.Fatal(err)
	}
	release := func() {
		s.WaitForLockWaiters(t, 2)
		if err := blocker.Commit(ctx); err != nil {
			t.Error(err)
		}
	}
	var issued enrollmenttokens.Token
	var wasDeleted bool
	var issueErr, deleteErr error
	issueDone, deleteDone := make(chan struct{}), make(chan struct{})
	issue := func() {
		defer close(issueDone)
		issued, issueErr = s.Tokens.IssueDefault(ctx, OrgA, c.Fleet.ID, Ana, Now.Add(time.Minute))
	}
	del := func() {
		defer close(deleteDone)
		if deleted == "fleet" {
			store := fleets.NewPostgres(s.Databases.Runtime, fleets.WithDeadlockRetries(0))
			wasDeleted, deleteErr = store.Delete(ctx, OrgA, c.Fleet.ID)
			return
		}
		store := organizations.NewPostgres(s.Databases.Runtime, organizations.WithDeadlockRetries(0))
		wasDeleted, deleteErr = store.Delete(ctx, OrgA)
	}
	first, second := issue, del
	if !issueFirst {
		first, second = del, issue
	}
	go first()
	s.WaitForLockWaiters(t, 1)
	go second()
	release()
	<-issueDone
	<-deleteDone
	if deleteErr != nil || !wasDeleted {
		t.Fatalf("the %s's delete = %v, %v; want it done", deleted, wasDeleted, deleteErr)
	}
	switch {
	case issueFirst && (issueErr != nil || issued.Kind != enrollmenttokens.KindDefault):
		t.Fatalf("the issue, first = %+v, %v; want a new default token", issued, issueErr)
	case !issueFirst && !errors.Is(issueErr, enrollmenttokens.ErrFleetNotFound):
		t.Fatalf("the issue, after the delete = %v, want ErrFleetNotFound", issueErr)
	}
	for table, n := range s.FleetRows(t, c.Fleet.ID) {
		if n != 0 {
			t.Errorf("%d rows of %s left", n, table)
		}
	}
}
