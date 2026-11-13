package fleets_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/collectorgroups"
	"github.com/telflo/opamp-server/store/enrollmenttokens"
	"github.com/telflo/opamp-server/store/fleets"
	. "github.com/telflo/opamp-server/store/storetest"
)

// This package's own test database (CONTRIBUTING.md, "Tests").
const testDatabase = "telflo_opamp_server_fleets_test"

var ctx = context.Background()

func newFleet(org uuid.UUID, label string, typ fleets.Type) fleets.NewFleet {
	return fleets.NewFleet{
		ID: uuid.New(), OrganizationID: org, Label: label, Type: typ, CreatedBy: Ana,
		RotateExpiresAt: Ptr(Now.AddDate(1, 0, 0)), At: Now,
	}
}

// One Create makes the fleet, its Default group named "Default", a default
// token that never expires and a rotate token with the caller's expiry, and
// an `issued` event for each, by the creator.
func TestCreateMakesTheFleetItsDefaultGroupItsTwoTokensAndTheirEvents(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		f := newFleet(OrgA, "edge-prod ☃", fleets.TypeServer)
		result, c, err := s.Fleets.Create(ctx, f)
		if err != nil || result != fleets.Created {
			t.Fatalf("Create = %v, %v", result, err)
		}
		at := Millisecond(Now)
		want := fleets.Fleet{ID: f.ID, OrganizationID: OrgA, Label: "edge-prod ☃", Type: fleets.TypeServer,
			Status: fleets.StatusActive, CreatedBy: Ana, CreatedAt: at, UpdatedAt: at}
		assertFleet(t, "the created fleet", c.Fleet, want)
		if got, err := s.Fleets.Get(ctx, OrgA, f.ID); err != nil {
			t.Fatal(err)
		} else {
			assertFleet(t, "the stored fleet", got, want)
		}
		if c.RotateExpiresAt == nil || !c.RotateExpiresAt.Equal(Millisecond(Now.AddDate(1, 0, 0))) {
			t.Fatalf("RotateExpiresAt = %v", c.RotateExpiresAt)
		}

		groups, err := s.Groups.List(ctx, OrgA, f.ID)
		if err != nil || len(groups) != 1 {
			t.Fatalf("groups = %+v, %v; want the Default group alone", groups, err)
		}
		g := groups[0]
		if g.ID != c.DefaultGroupID || g.Name != "Default" || !g.IsDefault || g.FleetID != f.ID ||
			g.Annotations != nil ||
			g.AssignedPublicationID != nil || !g.CreatedAt.Equal(at) || !g.UpdatedAt.Equal(at) {
			t.Fatalf("the Default group = %+v", g)
		}

		tokens, err := s.Tokens.List(ctx, OrgA, f.ID, Now)
		if err != nil || len(tokens) != 2 {
			t.Fatalf("tokens = %+v, %v", tokens, err)
		}
		byID := map[uuid.UUID]enrollmenttokens.Token{}
		for _, l := range tokens {
			byID[l.ID] = l.Token
		}
		def, rot := byID[c.DefaultTokenID], byID[c.RotateTokenID]
		if def.Kind != enrollmenttokens.KindDefault || def.ExpiresAt != nil || def.RevokedAt != nil || def.DoRefresh ||
			*def.CreatedBy != *Ana || !def.CreatedAt.Equal(at) || def.FleetID != f.ID || def.OrganizationID != OrgA {
			t.Fatalf("the default token = %+v", def)
		}
		if rot.Kind != enrollmenttokens.KindRotate || rot.ExpiresAt == nil || !rot.ExpiresAt.Equal(*c.RotateExpiresAt) ||
			rot.RevokedAt != nil || rot.DoRefresh || *rot.CreatedBy != *Ana || !rot.CreatedAt.Equal(at) {
			t.Fatalf("the rotate token = %+v", rot)
		}

		page, err := s.Tokens.ListEvents(ctx, OrgA, f.ID, enrollmenttokens.MaxListLimit, nil)
		events := page.Events
		if err != nil || len(events) != 2 || page.Next != nil {
			t.Fatalf("events = %+v, %v", events, err)
		}
		issued := map[uuid.UUID]bool{}
		for _, e := range events {
			if e.Kind != enrollmenttokens.EventIssued || e.Actor == nil || *e.Actor != *Ana || !e.OccurredAt.Equal(at) {
				t.Fatalf("an event = %+v", e)
			}
			issued[e.TokenID] = true
		}
		if !issued[c.DefaultTokenID] || !issued[c.RotateTokenID] {
			t.Fatalf("the events name %v, want both tokens", issued)
		}

		// No creator and a rotate token that never expires: both tokens
		// never expire, and the events have no actor.
		anonymous := newFleet(OrgA, "edge-k8s", fleets.TypeKubernetesSupervisor)
		anonymous.CreatedBy, anonymous.RotateExpiresAt = nil, nil
		result, c, err = s.Fleets.Create(ctx, anonymous)
		if err != nil || result != fleets.Created || c.Fleet.CreatedBy != nil || c.RotateExpiresAt != nil ||
			c.Fleet.Type != fleets.TypeKubernetesSupervisor {
			t.Fatalf("an anonymous Create = %v, %+v, %v", result, c, err)
		}
		rotate, err := s.Tokens.Get(ctx, OrgA, anonymous.ID, c.RotateTokenID)
		if err != nil || rotate.ExpiresAt != nil || rotate.CreatedBy != nil {
			t.Fatalf("its rotate token = %+v, %v", rotate, err)
		}
		page, err = s.Tokens.ListEvents(ctx, OrgA, anonymous.ID, enrollmenttokens.MaxListLimit, nil)
		events = page.Events
		if err != nil || len(events) != 2 || events[0].Actor != nil || events[1].Actor != nil {
			t.Fatalf("its events = %+v, %v", events, err)
		}
	})
}

func assertFleet(t *testing.T, label string, got, want fleets.Fleet) {
	t.Helper()
	sameActor := (got.CreatedBy == nil && want.CreatedBy == nil) ||
		(got.CreatedBy != nil && want.CreatedBy != nil && *got.CreatedBy == *want.CreatedBy)
	if got.ID != want.ID || got.OrganizationID != want.OrganizationID || got.Label != want.Label ||
		got.Type != want.Type || got.Status != want.Status || !sameActor ||
		!got.CreatedAt.Equal(want.CreatedAt) || !got.UpdatedAt.Equal(want.UpdatedAt) ||
		got.CreatedAt.Location() != time.UTC || got.UpdatedAt.Location() != time.UTC {
		t.Fatalf("%s = %+v, want %+v", label, got, want)
	}
}

// The caller's id makes Create idempotent. The same id, label and type again
// is Unchanged (the stored fleet, nothing new made); any other difference
// under the id is a Conflict; another fleet's label is LabelTaken; an id
// another organization holds is a Conflict that changes nothing of theirs;
// an unregistered organization is refused.
func TestCreateIsIdempotentOnTheCallersID(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		f := newFleet(OrgA, "edge-prod", fleets.TypeServer)
		if result, _, err := s.Fleets.Create(ctx, f); err != nil || result != fleets.Created {
			t.Fatalf("Create = %v, %v", result, err)
		}
		before := s.Dump(t, OrgA)

		again := f
		again.At, again.CreatedBy, again.RotateExpiresAt = Now.Add(time.Hour), nil, nil
		result, c, err := s.Fleets.Create(ctx, again)
		if err != nil || result != fleets.Unchanged {
			t.Fatalf("the same id, label and type again = %v, %v; want unchanged", result, err)
		}
		at := Millisecond(Now)
		assertFleet(t, "the unchanged fleet", c.Fleet, fleets.Fleet{ID: f.ID, OrganizationID: OrgA, Label: "edge-prod",
			Type: fleets.TypeServer, Status: fleets.StatusActive, CreatedBy: Ana, CreatedAt: at, UpdatedAt: at})
		if c.DefaultGroupID != uuid.Nil || c.DefaultTokenID != uuid.Nil || c.RotateTokenID != uuid.Nil {
			t.Fatalf("Unchanged revealed ids: %+v", c)
		}
		s.Unchanged(t, "an unchanged Create", OrgA, before)

		for label, other := range map[string]fleets.NewFleet{
			"another label":       {ID: f.ID, OrganizationID: OrgA, Label: "edge-prod-2", Type: fleets.TypeServer, At: Now},
			"another type":        {ID: f.ID, OrganizationID: OrgA, Label: "edge-prod", Type: fleets.TypeKubernetesSupervisor, At: Now},
			"the label in a case": {ID: f.ID, OrganizationID: OrgA, Label: "Edge-Prod", Type: fleets.TypeServer, At: Now},
		} {
			result, c, err := s.Fleets.Create(ctx, other)
			if err != nil || result != fleets.Conflict || c != (fleets.Creation{}) {
				t.Fatalf("%s: Create = %v, %+v, %v; want a bare conflict", label, result, c, err)
			}
		}
		result, c, err = s.Fleets.Create(ctx, newFleet(OrgA, "edge-prod", fleets.TypeServer))
		if err != nil || result != fleets.LabelTaken || c != (fleets.Creation{}) {
			t.Fatalf("another id with the label = %v, %+v, %v; want label taken", result, c, err)
		}
		s.Unchanged(t, "the refused Creates", OrgA, before)

		// A's id, label and type, under B: B's Create can't take A's id, and
		// A's fleet is untouched.
		theirs := f
		theirs.OrganizationID = OrgB
		result, c, err = s.Fleets.Create(ctx, theirs)
		if err != nil || result != fleets.Conflict || c != (fleets.Creation{}) {
			t.Fatalf("A's id under B = %v, %+v, %v; want a bare conflict", result, c, err)
		}
		s.Unchanged(t, "B's Create of A's id", OrgA, before)
		if _, err := s.Fleets.Get(ctx, OrgB, f.ID); !errors.Is(err, fleets.ErrNotFound) {
			t.Fatalf("B reads A's fleet: %v", err)
		}
		// The label is A's alone: B may use it.
		if result, _, err := s.Fleets.Create(ctx, newFleet(OrgB, "edge-prod", fleets.TypeServer)); err != nil || result != fleets.Created {
			t.Fatalf("A's label under B = %v, %v; want created", result, err)
		}

		if _, _, err := s.Fleets.Create(ctx, newFleet(OrgUnregistered, "edge", fleets.TypeServer)); !errors.Is(err, fleets.ErrOrganizationNotFound) {
			t.Fatalf("an unregistered organization's Create = %v, want ErrOrganizationNotFound", err)
		}
	})
}

// Idempotency under a race: two identical Creates at once are one Created
// and one Unchanged, never an error, on two sessions on Postgres. The
// fleet's insert is `on conflict do nothing` with no target: its id is
// unique twice (the primary key and uq_fleets_organization_id_id), and a
// target naming one lets the loser of a race that passes the target's check
// raise 23505 on the other. The first round holds both in the race (a third
// session locks A's row, so the first insert waits on its foreign key's
// check, its uniqueness entries made, and the second waits on the first);
// the next rounds start both at once, many times, which is how the targeted
// conflict's 23505 shows.
func TestTwoSimultaneousIdenticalCreatesAreOneCreatedAndOneUnchanged(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		const rounds = 40
		for round := range rounds {
			f := newFleet(OrgA, "edge-"+uuid.NewString(), fleets.TypeServer)
			release := func() {}
			if round == 0 {
				blocker, err := s.Databases.Owner.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = blocker.Rollback(ctx) }()
				if _, err := blocker.Exec(ctx, `select 1 from opamp_server.organizations where id = $1 for update`, OrgA); err != nil {
					t.Fatal(err)
				}
				release = func() {
					s.WaitForLockWaiters(t, 2)
					if err := blocker.Commit(ctx); err != nil {
						t.Error(err)
					}
				}
			}
			results := make([]fleets.CreateResult, 2)
			errs := make([]error, 2)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range 2 {
				wg.Go(func() {
					<-start
					results[i], _, errs[i] = s.Fleets.Create(ctx, f)
				})
			}
			close(start)
			release()
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Fatalf("round %d: Create %d failed: %v", round, i, err)
				}
			}
			if !(results[0] == fleets.Created && results[1] == fleets.Unchanged) &&
				!(results[0] == fleets.Unchanged && results[1] == fleets.Created) {
				t.Fatalf("round %d: the two Creates = %v; want one created and one unchanged", round, results)
			}
			if rows := s.FleetRows(t, f.ID); rows["collector_groups"] != 1 || rows["enrollment_tokens"] != 2 || rows["enrollment_token_events"] != 2 {
				t.Fatalf("round %d: the fleet's rows = %v; want one group, two tokens, two events", round, rows)
			}
		}
	})
}

// What Create refuses as the tables would (ErrInvalid), storing nothing.
func TestCreateRefusesWhatTheTablesRefuse(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		before := s.Dump(t, OrgA)
		valid := newFleet(OrgA, "edge", fleets.TypeServer)
		for label, change := range map[string]func(f *fleets.NewFleet){
			"an empty label":                          func(f *fleets.NewFleet) { f.Label = "" },
			"a label of a space":                      func(f *fleets.NewFleet) { f.Label = " " },
			"a label with a leading space":            func(f *fleets.NewFleet) { f.Label = " edge" },
			"a label with a trailing tab":             func(f *fleets.NewFleet) { f.Label = "edge\t" },
			"a label with a leading line break":       func(f *fleets.NewFleet) { f.Label = "\nedge" },
			"a label of 81 characters":                func(f *fleets.NewFleet) { f.Label = strings.Repeat("é", 81) },
			"a label that isn't UTF-8":                func(f *fleets.NewFleet) { f.Label = "edge\xff" },
			"a label with a NUL":                      func(f *fleets.NewFleet) { f.Label = "ed\x00ge" },
			"a label with a tab inside":               func(f *fleets.NewFleet) { f.Label = "edge prod\tEU" },
			"the bridge's type":                       func(f *fleets.NewFleet) { f.Type = "kubernetes" },
			"no type":                                 func(f *fleets.NewFleet) { f.Type = "" },
			"a creator with an empty email":           func(f *fleets.NewFleet) { f.CreatedBy = &fleets.Actor{ID: uuid.New()} },
			"a creator with an email of 321":          func(f *fleets.NewFleet) { f.CreatedBy = &fleets.Actor{ID: uuid.New(), Email: strings.Repeat("e", 321)} },
			"a rotate token expiring now":             func(f *fleets.NewFleet) { f.RotateExpiresAt = Ptr(Now) },
			"a rotate token expired before it's made": func(f *fleets.NewFleet) { f.RotateExpiresAt = Ptr(Now.Add(-time.Hour)) },
		} {
			f := valid
			change(&f)
			if _, _, err := s.Fleets.Create(ctx, f); !errors.Is(err, fleets.ErrInvalid) {
				t.Errorf("%s: Create = %v, want ErrInvalid", label, err)
			}
		}
		s.Unchanged(t, "the refused Creates", OrgA, before)

		for _, label := range []string{strings.Repeat("é", 80), "edge prod EU", "x"} {
			if result, _, err := s.Fleets.Create(ctx, newFleet(OrgA, label, fleets.TypeServer)); err != nil || result != fleets.Created {
				t.Errorf("the label %q: Create = %v, %v; want created", label, result, err)
			}
		}
	})
}

// List: newest first by creation, ties by id descending, every fleet once
// across pages, the status filter, the organization's alone.
func TestListIsNewestFirstPagesEveryFleetOnceAndFilters(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		var want []uuid.UUID // newest first
		times := []time.Time{Now, Now.Add(time.Minute), Now.Add(time.Minute), Now.Add(2 * time.Minute), Now.Add(3 * time.Minute)}
		created := make([]fleets.NewFleet, len(times))
		for i, at := range times {
			f := newFleet(OrgA, "edge-"+string(rune('a'+i)), fleets.TypeServer)
			f.At = at
			if result, _, err := s.Fleets.Create(ctx, f); err != nil || result != fleets.Created {
				t.Fatal(result, err)
			}
			created[i] = f
		}
		// B's fleets never appear in A's list.
		s.CreateFleet(t, OrgB, "globex", fleets.TypeServer)
		for i := len(created) - 1; i >= 0; i-- {
			want = append(want, created[i].ID)
		}
		// The two of one instant: the greater id first.
		if strings.Compare(string(want[2][:]), string(want[3][:])) < 0 {
			want[2], want[3] = want[3], want[2]
		}

		var got []uuid.UUID
		var after *fleets.Cursor
		for pages := 0; ; pages++ {
			page, err := s.Fleets.List(ctx, OrgA, fleets.ListOptions{Limit: 2, After: after})
			if err != nil || pages > 5 {
				t.Fatalf("List: %v (page %d)", err, pages)
			}
			for _, f := range page.Fleets {
				got = append(got, f.ID)
			}
			if page.Next == nil {
				break
			}
			parsed, err := fleets.ParseCursor(page.Next.String())
			if err != nil {
				t.Fatal(err)
			}
			after = &parsed
		}
		if len(got) != len(want) {
			t.Fatalf("listed %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("listed %v, want %v", got, want)
			}
		}

		archived := fleets.StatusArchived
		if _, err := s.Fleets.Update(ctx, OrgA, created[1].ID, fleets.Changes{Status: &archived}, Now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		page, err := s.Fleets.List(ctx, OrgA, fleets.ListOptions{Limit: 100, Status: &archived})
		if err != nil || len(page.Fleets) != 1 || page.Fleets[0].ID != created[1].ID || page.Next != nil {
			t.Fatalf("the archived = %+v, %v", page, err)
		}
		active := fleets.StatusActive
		page, err = s.Fleets.List(ctx, OrgA, fleets.ListOptions{Limit: 100, Status: &active})
		if err != nil || len(page.Fleets) != 4 {
			t.Fatalf("the active = %+v, %v", page, err)
		}
		page, err = s.Fleets.List(ctx, OrgUnregistered, fleets.ListOptions{Limit: 100})
		if err != nil || page.Fleets == nil || len(page.Fleets) != 0 || page.Next != nil {
			t.Fatalf("an empty list = %+v, %v", page, err)
		}
		for _, limit := range []int{0, 101} {
			if _, err := s.Fleets.List(ctx, OrgA, fleets.ListOptions{Limit: limit}); err == nil {
				t.Fatalf("a limit of %d was accepted", limit)
			}
		}
		if _, err := s.Fleets.List(ctx, OrgA, fleets.ListOptions{Limit: 1, Status: Ptr(fleets.Status("deleted"))}); !errors.Is(err, fleets.ErrInvalid) {
			t.Fatalf("an unknown status = %v", err)
		}
	})
}

func TestACursorRoundTripsAndNothingElseParses(t *testing.T) {
	c := fleets.Cursor{CreatedAt: Millisecond(Now), ID: uuid.New()}
	parsed, err := fleets.ParseCursor(c.String())
	if err != nil || !parsed.CreatedAt.Equal(c.CreatedAt) || parsed.ID != c.ID {
		t.Fatalf("round trip = %+v, %v", parsed, err)
	}
	for _, s := range []string{"", "x", c.String() + "A", fleets.Cursor{CreatedAt: Now, ID: c.ID}.String()} {
		if _, err := fleets.ParseCursor(s); !errors.Is(err, fleets.ErrInvalidCursor) {
			t.Fatalf("ParseCursor(%q) = %v", s, err)
		}
	}
}

// Update: the label and the status (archive, unarchive); UpdatedAt moves
// only on a change; a label taken is refused, and so is a value the table
// refuses; CreatedAt and the type never change.
func TestUpdateRenamesArchivesAndUnarchives(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		s.CreateFleet(t, OrgA, "edge-dev", fleets.TypeServer)
		id := c.Fleet.ID
		later := Now.Add(time.Hour)

		got, err := s.Fleets.Update(ctx, OrgA, id, fleets.Changes{Label: Ptr("edge-production")}, later)
		if err != nil || got.Label != "edge-production" || !got.UpdatedAt.Equal(Millisecond(later)) ||
			!got.CreatedAt.Equal(Millisecond(Now)) || got.Status != fleets.StatusActive {
			t.Fatalf("rename = %+v, %v", got, err)
		}
		got, err = s.Fleets.Update(ctx, OrgA, id, fleets.Changes{Label: Ptr("edge-production"), Status: Ptr(fleets.StatusActive)}, later.Add(time.Hour))
		if err != nil || !got.UpdatedAt.Equal(Millisecond(later)) {
			t.Fatalf("a no-op update moved UpdatedAt: %+v, %v", got, err)
		}
		got, err = s.Fleets.Update(ctx, OrgA, id, fleets.Changes{Status: Ptr(fleets.StatusArchived)}, later.Add(2*time.Hour))
		if err != nil || got.Status != fleets.StatusArchived || !got.UpdatedAt.Equal(Millisecond(later.Add(2*time.Hour))) {
			t.Fatalf("archive = %+v, %v", got, err)
		}
		if typ, status, err := s.Fleets.TypeAndStatus(ctx, OrgA, id); err != nil || typ != fleets.TypeServer || status != fleets.StatusArchived {
			t.Fatalf("TypeAndStatus = %v, %v, %v", typ, status, err)
		}
		got, err = s.Fleets.Update(ctx, OrgA, id, fleets.Changes{Status: Ptr(fleets.StatusActive)}, later.Add(3*time.Hour))
		if err != nil || got.Status != fleets.StatusActive {
			t.Fatalf("unarchive = %+v, %v", got, err)
		}

		before := s.Dump(t, OrgA)
		if _, err := s.Fleets.Update(ctx, OrgA, id, fleets.Changes{Label: Ptr("edge-dev")}, later); !errors.Is(err, fleets.ErrLabelTaken) {
			t.Fatalf("a taken label = %v", err)
		}
		for label, changes := range map[string]fleets.Changes{
			"an empty label":    {Label: Ptr("")},
			"a padded label":    {Label: Ptr("edge\t")},
			"an unknown status": {Status: Ptr(fleets.Status("deleted"))},
		} {
			if _, err := s.Fleets.Update(ctx, OrgA, id, changes, later); !errors.Is(err, fleets.ErrInvalid) {
				t.Fatalf("%s = %v, want ErrInvalid", label, err)
			}
		}
		if _, err := s.Fleets.Update(ctx, OrgA, uuid.New(), fleets.Changes{Label: Ptr("x")}, later); !errors.Is(err, fleets.ErrNotFound) {
			t.Fatalf("an unknown fleet = %v", err)
		}
		s.Unchanged(t, "the refused updates", OrgA, before)
	})
}

// Delete takes the fleet with everything it holds: its groups, tokens,
// token events and collectors (the cascades), and
// nothing of another fleet's.
func TestDeleteTakesTheFleetsGroupsTokensEventsAndCollectors(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		kept := s.CreateFleet(t, OrgA, "edge-dev", fleets.TypeServer)
		id := c.Fleet.ID
		if _, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: id, Name: "gateways",
			AssignedPublicationID: Ptr(PubA), At: Now}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Tokens.Rotate(ctx, enrollmenttokens.Rotation{OrganizationID: OrgA, FleetID: id,
			GraceEndsAt: Now.Add(time.Hour), At: Now}); err != nil {
			t.Fatal(err)
		}
		s.InsertCollector(t, Collector{ID: uuid.New(), OrganizationID: OrgA, FleetID: id, GroupID: c.DefaultGroupID,
			Name: "edge-01", TokenID: Ptr(c.RotateTokenID), PinnedPublicationID: Ptr(PubA), LastSeenAt: Now})
		rows := s.FleetRows(t, id)
		if rows["fleets"] != 1 || rows["collector_groups"] != 2 || rows["enrollment_tokens"] != 3 ||
			rows["enrollment_token_events"] != 4 || rows["collector_instances"] != 1 {
			t.Fatalf("before the delete: %v", rows)
		}
		keptBefore := s.FleetRows(t, kept.Fleet.ID)
		otherOrganization := s.Dump(t, OrgB)

		if deleted, err := s.Fleets.Delete(ctx, OrgA, id); err != nil || !deleted {
			t.Fatalf("Delete = %v, %v", deleted, err)
		}
		for table, n := range s.FleetRows(t, id) {
			if n != 0 {
				t.Errorf("%s still holds %d of the fleet's rows", table, n)
			}
		}
		if after := s.FleetRows(t, kept.Fleet.ID); Describe(after) != Describe(keptBefore) {
			t.Fatalf("the other fleet's rows changed: %v, then %v", keptBefore, after)
		}
		s.Unchanged(t, "A's fleet delete", OrgB, otherOrganization)
		if _, err := s.Fleets.Get(ctx, OrgA, id); !errors.Is(err, fleets.ErrNotFound) {
			t.Fatalf("Get after the delete = %v", err)
		}
		if _, err := s.Tokens.Lookup(ctx, c.RotateTokenID); !errors.Is(err, enrollmenttokens.ErrNotFound) {
			t.Fatalf("the deleted fleet's token still authenticates: %v", err)
		}
		// The published version the group and the collector used is free.
		if deleted, err := s.Publications.Delete(ctx, OrgA, PubA); err != nil || !deleted {
			t.Fatalf("deleting the freed published version = %v, %v", deleted, err)
		}
		if deleted, err := s.Fleets.Delete(ctx, OrgA, id); err != nil || deleted {
			t.Fatalf("a second Delete = %v, %v", deleted, err)
		}
	})
}

// TypeAndStatus is what the agent side asks of a fleet.
func TestTypeAndStatusAnswersTheOrganizationsFleetOnly(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-k8s", fleets.TypeKubernetesSupervisor)
		if typ, status, err := s.Fleets.TypeAndStatus(ctx, OrgA, c.Fleet.ID); err != nil ||
			typ != fleets.TypeKubernetesSupervisor || status != fleets.StatusActive {
			t.Fatalf("TypeAndStatus = %v, %v, %v", typ, status, err)
		}
		if _, _, err := s.Fleets.TypeAndStatus(ctx, OrgA, uuid.New()); !errors.Is(err, fleets.ErrNotFound) {
			t.Fatalf("an unknown fleet = %v", err)
		}
	})
}

// Every method, called with another organization's fleet, answers as if it
// didn't exist and changes nothing of either's.
func TestAnotherOrganizationsFleetIsNotFoundAndUnchanged(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		theirs := s.CreateFleet(t, OrgB, "globex-prod", fleets.TypeServer)
		s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		id := theirs.Fleet.ID
		beforeA, beforeB := s.Dump(t, OrgA), s.Dump(t, OrgB)

		if _, err := s.Fleets.Get(ctx, OrgA, id); !errors.Is(err, fleets.ErrNotFound) {
			t.Errorf("Get = %v", err)
		}
		if _, _, err := s.Fleets.TypeAndStatus(ctx, OrgA, id); !errors.Is(err, fleets.ErrNotFound) {
			t.Errorf("TypeAndStatus = %v", err)
		}
		if _, err := s.Fleets.Update(ctx, OrgA, id, fleets.Changes{Label: Ptr("mine"), Status: Ptr(fleets.StatusArchived)}, Now.Add(time.Hour)); !errors.Is(err, fleets.ErrNotFound) {
			t.Errorf("Update = %v", err)
		}
		if deleted, err := s.Fleets.Delete(ctx, OrgA, id); err != nil || deleted {
			t.Errorf("Delete = %v, %v", deleted, err)
		}
		page, err := s.Fleets.List(ctx, OrgA, fleets.ListOptions{Limit: 100})
		if err != nil || len(page.Fleets) != 1 || page.Fleets[0].ID == id {
			t.Errorf("List = %+v, %v", page, err)
		}
		f := newFleet(OrgA, "globex-prod", fleets.TypeServer)
		f.ID = id
		if result, _, err := s.Fleets.Create(ctx, f); err != nil || result != fleets.Conflict {
			t.Errorf("Create with their id = %v, %v", result, err)
		}
		s.Unchanged(t, "A's calls", OrgA, beforeA)
		s.Unchanged(t, "A's calls", OrgB, beforeB)
	})
}

// The trimmed-label rule, pinned at Unicode's edges: a label with an
// ideographic space (U+3000) or a no-break space (U+00A0) at either end is
// refused by Create and Update.
func TestALabelWithUnicodeEdgeWhitespaceIsRefused(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		before := s.Dump(t, OrgA)
		for _, label := range []string{"\u3000edge", "edge\u3000", "\u00a0edge", "edge\u00a0",
			// No control character anywhere, as a collector's name.
			"edge\nprod", "edge\tprod", "edge\x1b[31mprod", "edge\x7fprod", "edge\u0085prod"} {
			if _, _, err := s.Fleets.Create(ctx, fleets.NewFleet{ID: uuid.New(), OrganizationID: OrgA, Label: label,
				Type: fleets.TypeServer, At: Now}); !errors.Is(err, fleets.ErrInvalid) {
				t.Errorf("Create %q = %v, want ErrInvalid", label, err)
			}
			if _, err := s.Fleets.Update(ctx, OrgA, c.Fleet.ID, fleets.Changes{Label: Ptr(label)}, Now); !errors.Is(err, fleets.ErrInvalid) {
				t.Errorf("Update to %q = %v, want ErrInvalid", label, err)
			}
		}
		s.Unchanged(t, "the refused labels", OrgA, before)
		if _, _, err := s.Fleets.Create(ctx, fleets.NewFleet{ID: uuid.New(), OrganizationID: OrgA, Label: "edge\u3000prod\u00a0eu",
			Type: fleets.TypeServer, At: Now}); err != nil {
			t.Fatalf("spaces inside a label: %v", err)
		}
	})
}

// Production wiring makes the store with no option (management's serve), so
// its Delete runs again after a 40P01 DeadlockRetries times by default: the
// retry test passes its retries explicitly, so it can't see the default.
func TestNewPostgresRetriesADeadlockByDefault(t *testing.T) {
	if got := fleets.DeadlockRetriesOf(fleets.NewPostgres(nil)); got != fleets.DeadlockRetries || got < 1 {
		t.Fatalf("NewPostgres's Delete runs again %d times after a 40P01, want DeadlockRetries (%d)", got, fleets.DeadlockRetries)
	}
	if got := fleets.DeadlockRetriesOf(fleets.NewPostgres(nil, fleets.WithDeadlockRetries(0))); got != 0 {
		t.Fatalf("WithDeadlockRetries(0) leaves %d retries, want 0", got)
	}
}
