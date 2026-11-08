package collectors_test

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/collectorgroups"
	"github.com/telflo/opamp-server/store/collectors"
	"github.com/telflo/opamp-server/store/fleets"
	. "github.com/telflo/opamp-server/store/storetest"
)

// listAll pages through List with o, checking each cursor round-trips,
// and returns the names in order.
func listAll(t *testing.T, s *Stores, org, fleet uuid.UUID, o collectors.ListOptions) []string {
	t.Helper()
	var names []string
	for pages := 0; ; pages++ {
		page, err := s.Collectors.List(ctx, org, fleet, o)
		if err != nil || pages > 20 {
			t.Fatalf("List %+v: %v (page %d)", o, err, pages)
		}
		for _, l := range page.Collectors {
			names = append(names, l.Name)
		}
		if page.Next == nil {
			return names
		}
		parsed, err := collectors.ParseCursor(page.Next.String())
		if err != nil {
			t.Fatal(err)
		}
		o.After = &parsed
	}
}

// List sorts by creation, last seen or name, either way, ties by id; pages
// every collector once; filters by group and by q over the name and both
// addresses, with %, _ and \ taken literally; lists the fleet's collectors
// only.
func TestListSortsPagesSearchesAndFilters(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		g, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID, Name: "gateways", At: Now})
		if err != nil {
			t.Fatal(err)
		}
		// Created a minute apart, in this order; last seen in the reverse.
		specs := []struct{ name, ip, hostIP string }{
			{"bravo", "10.0.0.1", "192.168.0.1"},
			{"Alpha", "10.0.0.2", "192.168.0.2"},
			{"charlie_50%", "10.0.0.3", "172.16.0.9"},
			{`delta\x`, "10.1.0.4", "192.168.0.4"},
			{"echo", "10.0.0.5", "192.168.0.5"},
		}
		ids := map[string]uuid.UUID{}
		for i, spec := range specs {
			e := enrollment(c, spec.name, Now.Add(time.Duration(i)*time.Minute))
			if i%2 == 1 {
				e.GroupID = &g.ID
			}
			r, err := s.Collectors.Enroll(ctx, e)
			if err != nil {
				t.Fatal(err)
			}
			ids[spec.name] = r.Collector.ID
			if _, err := s.Collectors.Poll(ctx, collectors.PollRequest{OrganizationID: OrgA, FleetID: c.Fleet.ID,
				InstanceUID: e.InstanceUID, TokenID: c.RotateTokenID, At: Now.Add(time.Hour - time.Duration(i)*time.Minute)}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Collectors.Report(ctx, collectors.Report{OrganizationID: OrgA, FleetID: c.Fleet.ID, CollectorID: r.Collector.ID,
				Reported: collectors.Reported{IP: Ptr(spec.ip), HostIP: Ptr(spec.hostIP)}}); err != nil {
				t.Fatal(err)
			}
		}
		// Another fleet's and organization's collectors never show.
		enroll(t, s, s.CreateFleet(t, OrgA, "edge-dev", fleets.TypeServer), "bravo-dev", Now)
		enroll(t, s, s.CreateFleet(t, OrgB, "globex", fleets.TypeServer), "bravo-b", Now)

		byCreation := []string{"bravo", "Alpha", "charlie_50%", `delta\x`, "echo"}
		reversed := slices.Clone(byCreation)
		slices.Reverse(reversed)
		byName := []string{"Alpha", "bravo", "charlie_50%", `delta\x`, "echo"} // byte order: capitals first
		reversedNames := slices.Clone(byName)
		slices.Reverse(reversedNames)
		for _, want := range []struct {
			sort  collectors.Sort
			order collectors.Order
			names []string
		}{
			{collectors.SortCreated, collectors.Descending, reversed},
			{collectors.SortCreated, collectors.Ascending, byCreation},
			{collectors.SortLastSeen, collectors.Descending, byCreation},
			{collectors.SortLastSeen, collectors.Ascending, reversed},
			{collectors.SortName, collectors.Ascending, byName},
			{collectors.SortName, collectors.Descending, reversedNames},
		} {
			for _, limit := range []int{2, 100} {
				got := listAll(t, s, OrgA, c.Fleet.ID, collectors.ListOptions{Sort: want.sort, Order: want.order, Limit: limit})
				if Describe(got) != Describe(want.names) {
					t.Fatalf("%s %s by %d: %v, want %v", want.sort, want.order, limit, got, want.names)
				}
			}
		}

		search := func(q string, group *uuid.UUID) []string {
			t.Helper()
			names := listAll(t, s, OrgA, c.Fleet.ID, collectors.ListOptions{Q: q, GroupID: group,
				Sort: collectors.SortName, Order: collectors.Ascending, Limit: 2})
			if names == nil {
				names = []string{}
			}
			return names
		}
		for q, want := range map[string][]string{
			"ALPHA":       {"Alpha"},
			"a":           {"Alpha", "bravo", "charlie_50%", `delta\x`},
			"%":           {"charlie_50%"},
			"_":           {"charlie_50%"},
			"o_":          {},
			"e_":          {"charlie_50%"},
			"50%":         {"charlie_50%"},
			`\`:           {`delta\x`},
			`\x`:          {`delta\x`},
			"10.0.0.":     {"Alpha", "bravo", "charlie_50%", "echo"},
			"172.16":      {"charlie_50%"},
			"192.168.0.4": {`delta\x`},
			"dev":         {},
		} {
			if got := search(q, nil); Describe(got) != Describe(want) {
				t.Errorf("q %q: %v, want %v", q, got, want)
			}
		}
		if got := search("", &g.ID); Describe(got) != Describe([]string{"Alpha", `delta\x`}) {
			t.Errorf("the group's: %v", got)
		}
		if got := search("", &c.DefaultGroupID); Describe(got) != Describe([]string{"bravo", "charlie_50%", "echo"}) {
			t.Errorf("the Default group's: %v", got)
		}
		if got := search("a", &g.ID); Describe(got) != Describe([]string{"Alpha", `delta\x`}) {
			t.Errorf("q and group: %v", got)
		}
		if got := search(strings.Repeat("é", collectors.MaxQueryLength), nil); len(got) != 0 {
			t.Errorf("a q of 100 characters: %v", got)
		}

		// A listed collector carries what resolves for it.
		if _, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, g.ID,
			collectorgroups.Changes{AssignedPublicationID: collectorgroups.SetTo(PubA)}, Now); err != nil {
			t.Fatal(err)
		}
		page, err := s.Collectors.List(ctx, OrgA, c.Fleet.ID, collectors.ListOptions{GroupID: &g.ID, Sort: collectors.SortName,
			Order: collectors.Ascending, Limit: 1})
		if err != nil || len(page.Collectors) != 1 || page.Collectors[0].ID != ids["Alpha"] ||
			page.Collectors[0].Resolution.ID != PubA || page.Collectors[0].Sync() != collectors.Pending ||
			*page.Collectors[0].ReportedIP != "10.0.0.2" {
			t.Fatalf("the first of the group = %+v, %v", page, err)
		}

		// Refusals.
		valid := collectors.ListOptions{Sort: collectors.SortCreated, Order: collectors.Descending, Limit: 10}
		for label, change := range map[string]func(o *collectors.ListOptions){
			"a limit of 0":          func(o *collectors.ListOptions) { o.Limit = 0 },
			"a limit of 101":        func(o *collectors.ListOptions) { o.Limit = 101 },
			"an unknown sort":       func(o *collectors.ListOptions) { o.Sort = "uid" },
			"an unknown order":      func(o *collectors.ListOptions) { o.Order = "up" },
			"a q of 101 characters": func(o *collectors.ListOptions) { o.Q = strings.Repeat("q", 101) },
			"a q with a NUL":        func(o *collectors.ListOptions) { o.Q = "a\x00" },
			"a q that isn't UTF-8":  func(o *collectors.ListOptions) { o.Q = "a\xff" },
		} {
			o := valid
			change(&o)
			if _, err := s.Collectors.List(ctx, OrgA, c.Fleet.ID, o); !errors.Is(err, collectors.ErrInvalid) {
				t.Errorf("%s: List = %v, want ErrInvalid", label, err)
			}
		}
		o := valid
		o.After = &collectors.Cursor{Sort: collectors.SortName, Order: collectors.Descending, Name: "x", ID: uuid.New()}
		if _, err := s.Collectors.List(ctx, OrgA, c.Fleet.ID, o); !errors.Is(err, collectors.ErrInvalidCursor) {
			t.Errorf("another order's cursor: %v", err)
		}
		for label, fleet := range map[string]struct{ org, id uuid.UUID }{
			"another organization's fleet": {OrgB, c.Fleet.ID},
			"an unknown fleet":             {OrgA, uuid.New()},
		} {
			if _, err := s.Collectors.List(ctx, fleet.org, fleet.id, valid); !errors.Is(err, collectors.ErrFleetNotFound) {
				t.Errorf("%s: List = %v, want ErrFleetNotFound", label, err)
			}
		}
		empty := s.CreateFleet(t, OrgA, "edge-empty", fleets.TypeServer)
		if page, err := s.Collectors.List(ctx, OrgA, empty.Fleet.ID, valid); err != nil || page.Collectors == nil ||
			len(page.Collectors) != 0 || page.Next != nil {
			t.Errorf("an empty fleet = %+v, %v", page, err)
		}
	})
}

func TestACursorRoundTripsAndNothingElseParses(t *testing.T) {
	for _, c := range []collectors.Cursor{
		{Sort: collectors.SortCreated, Order: collectors.Descending, Time: Millisecond(Now), ID: uuid.New()},
		{Sort: collectors.SortLastSeen, Order: collectors.Ascending, Time: Millisecond(Now), ID: uuid.New()},
		{Sort: collectors.SortName, Order: collectors.Ascending, Name: "edge-01 ☃", ID: uuid.New()},
	} {
		parsed, err := collectors.ParseCursor(c.String())
		if err != nil || parsed.Sort != c.Sort || parsed.Order != c.Order || !parsed.Time.Equal(c.Time) ||
			parsed.Name != c.Name || parsed.ID != c.ID {
			t.Fatalf("round trip of %+v = %+v, %v", c, parsed, err)
		}
	}
	valid := collectors.Cursor{Sort: collectors.SortCreated, Order: collectors.Descending, Time: Millisecond(Now), ID: uuid.New()}
	for _, s := range []string{"", "x", valid.String() + "A",
		collectors.Cursor{Sort: collectors.SortCreated, Order: collectors.Descending, Time: Now, ID: valid.ID}.String(),
		collectors.Cursor{Sort: collectors.SortName, Order: collectors.Ascending, Name: " padded", ID: valid.ID}.String(),
		collectors.Cursor{Sort: collectors.SortName, Order: collectors.Ascending, Name: "", ID: valid.ID}.String(),
	} {
		if _, err := collectors.ParseCursor(s); !errors.Is(err, collectors.ErrInvalidCursor) {
			t.Fatalf("ParseCursor(%q) = %v", s, err)
		}
	}
	e := collectors.ErrorCursor{ReportedAt: Millisecond(Now), ID: uuid.New()}
	if parsed, err := collectors.ParseErrorCursor(e.String()); err != nil || parsed != e {
		t.Fatalf("an error cursor's round trip = %+v, %v", parsed, err)
	}
	if _, err := collectors.ParseErrorCursor(valid.String()); !errors.Is(err, collectors.ErrInvalidCursor) {
		t.Fatalf("a list cursor parsed as an error cursor: %v", err)
	}
}

// Rename (unique per organization as written; UpdatedAt only on a change),
// Move (to a group of the same fleet), Pin and Unpin (a published version
// of the organization; one pin at a time), Delete (it re-enrolls as new).
func TestRenameMovePinAndDelete(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		other := s.CreateFleet(t, OrgA, "edge-dev", fleets.TypeServer)
		got := enroll(t, s, c, "edge-01", Now)
		enroll(t, s, other, "edge-dev-01", Now)
		later := Now.Add(time.Hour)

		renamed, err := s.Collectors.Rename(ctx, OrgA, c.Fleet.ID, got.ID, "Edge-01", later)
		if err != nil || renamed.Name != "Edge-01" || !renamed.UpdatedAt.Equal(Millisecond(later)) || !renamed.CreatedAt.Equal(Millisecond(Now)) {
			t.Fatalf("Rename = %+v, %v", renamed, err)
		}
		if again, err := s.Collectors.Rename(ctx, OrgA, c.Fleet.ID, got.ID, "Edge-01", later.Add(time.Hour)); err != nil ||
			!again.UpdatedAt.Equal(Millisecond(later)) {
			t.Fatalf("a rename to the same name moved UpdatedAt: %+v, %v", again, err)
		}
		before := s.Dump(t, OrgA)
		if _, err := s.Collectors.Rename(ctx, OrgA, c.Fleet.ID, got.ID, "edge-dev-01", later); !errors.Is(err, collectors.ErrNameTaken) {
			t.Fatalf("another fleet's collector's name = %v, want ErrNameTaken (unique per organization)", err)
		}
		for _, name := range []string{"", " x", "x\n", " x", strings.Repeat("x", 201), "x\x00",
			// A control character anywhere: the name is pushed back as the
			// ServerName header, which can't hold one.
			"edge-01\nrack-7", "edge\t01", "edge\x7f01", "edge\u008501", "edge\x1b[0m"} {
			if _, err := s.Collectors.Rename(ctx, OrgA, c.Fleet.ID, got.ID, name, later); !errors.Is(err, collectors.ErrInvalid) {
				t.Fatalf("the name %q = %v, want ErrInvalid", name, err)
			}
		}
		s.Unchanged(t, "the refused renames", OrgA, before)

		g, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID, Name: "gateways", At: Now})
		if err != nil {
			t.Fatal(err)
		}
		moved, err := s.Collectors.Move(ctx, OrgA, c.Fleet.ID, got.ID, g.ID, later.Add(time.Hour))
		if err != nil || moved.GroupID != g.ID || !moved.UpdatedAt.Equal(Millisecond(later.Add(time.Hour))) {
			t.Fatalf("Move = %+v, %v", moved, err)
		}
		if again, err := s.Collectors.Move(ctx, OrgA, c.Fleet.ID, got.ID, g.ID, later.Add(2*time.Hour)); err != nil ||
			!again.UpdatedAt.Equal(moved.UpdatedAt) {
			t.Fatalf("a move to the same group = %+v, %v", again, err)
		}
		before = s.Dump(t, OrgA)
		for label, group := range map[string]uuid.UUID{
			"another fleet's group":        other.DefaultGroupID,
			"another organization's group": s.CreateFleet(t, OrgB, "globex", fleets.TypeServer).DefaultGroupID,
			"an unknown group":             uuid.New(),
		} {
			if _, err := s.Collectors.Move(ctx, OrgA, c.Fleet.ID, got.ID, group, later); !errors.Is(err, collectors.ErrGroupNotFound) {
				t.Errorf("%s: Move = %v, want ErrGroupNotFound", label, err)
			}
		}
		s.Unchanged(t, "the refused moves", OrgA, before)

		// Pins: a published version, replaced by another; then none.
		pin := func(ref *collectors.Ref, at time.Time) collectors.Collector {
			t.Helper()
			pinned, err := s.Collectors.Pin(ctx, OrgA, c.Fleet.ID, got.ID, ref, at)
			if err != nil {
				t.Fatalf("Pin %+v = %v", ref, err)
			}
			return pinned
		}
		publication := &collectors.Ref{Source: collectors.SourcePublication, ID: PubA}
		if p := pin(publication, later.Add(3*time.Hour)); *p.Pinned != *publication || !p.UpdatedAt.Equal(Millisecond(later.Add(3*time.Hour))) {
			t.Fatalf("pinned = %+v", p)
		}
		if p := pin(publication, later.Add(4*time.Hour)); !p.UpdatedAt.Equal(Millisecond(later.Add(3 * time.Hour))) {
			t.Fatalf("the same pin moved UpdatedAt: %+v", p)
		}
		replacement := &collectors.Ref{Source: collectors.SourcePublication, ID: publish(t, s, OrgA, "receivers: {}\n")}
		if p := pin(replacement, later.Add(5*time.Hour)); *p.Pinned != *replacement {
			t.Fatalf("pinned another published version = %+v", p)
		}
		if deleted, err := s.Publications.Delete(ctx, OrgA, PubA); err != nil || !deleted {
			t.Fatalf("PubA, no longer pinned, couldn't be deleted: %v, %v", deleted, err)
		}
		if p := pin(nil, later.Add(6*time.Hour)); p.Pinned != nil || !p.UpdatedAt.Equal(Millisecond(later.Add(6*time.Hour))) {
			t.Fatalf("unpinned = %+v", p)
		}
		before = s.Dump(t, OrgA)
		for label, want := range map[string]struct {
			ref collectors.Ref
			err error
		}{
			"B's published version": {collectors.Ref{Source: collectors.SourcePublication, ID: PubB}, collectors.ErrPublicationNotFound},
			"a deleted one":         {collectors.Ref{Source: collectors.SourcePublication, ID: PubA}, collectors.ErrPublicationNotFound},
			"a nil id":              {collectors.Ref{Source: collectors.SourcePublication}, collectors.ErrInvalid},
			"an unknown source":     {collectors.Ref{Source: "config", ID: PubA}, collectors.ErrInvalid},
			// An import is no kind of pin.
			"an import": {collectors.Ref{Source: "import", ID: PubB}, collectors.ErrInvalid},
		} {
			if _, err := s.Collectors.Pin(ctx, OrgA, c.Fleet.ID, got.ID, &want.ref, later); !errors.Is(err, want.err) {
				t.Errorf("pinning %s = %v, want %v", label, err, want.err)
			}
		}
		s.Unchanged(t, "the refused pins", OrgA, before)

		if deleted, err := s.Collectors.Delete(ctx, OrgA, c.Fleet.ID, got.ID); err != nil || !deleted {
			t.Fatalf("Delete = %v, %v", deleted, err)
		}
		if _, err := s.Collectors.Get(ctx, OrgA, c.Fleet.ID, got.ID); !errors.Is(err, collectors.ErrNotFound) {
			t.Fatalf("Get after the delete = %v", err)
		}
		if deleted, err := s.Collectors.Delete(ctx, OrgA, c.Fleet.ID, got.ID); err != nil || deleted {
			t.Fatalf("a second Delete = %v, %v", deleted, err)
		}
		again := enrollment(c, "Edge-01", Now.Add(time.Hour))
		again.InstanceUID = got.InstanceUID
		if r, err := s.Collectors.Enroll(ctx, again); err != nil || !r.Created || r.Collector.ID == got.ID || r.Collector.Name != "Edge-01" {
			t.Fatalf("re-enrolling after the delete = %+v, %v", r, err)
		}
	})
}

// BulkPin pins (or unpins) up to MaxBulkPin of the fleet's collectors in
// one transaction, answering the ids it pinned and those the fleet doesn't
// hold (sorted, without duplicates) and how many changed; another
// organization's or fleet's collectors are left alone.
func TestBulkPinPinsTheFleetsCollectorsAndReportsTheRest(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		other := s.CreateFleet(t, OrgA, "edge-dev", fleets.TypeServer)
		theirs := s.CreateFleet(t, OrgB, "globex", fleets.TypeServer)
		a1, a2, a3 := enroll(t, s, c, "a1", Now), enroll(t, s, c, "a2", Now), enroll(t, s, c, "a3", Now)
		foreign := enroll(t, s, other, "dev-1", Now)
		b := enroll(t, s, theirs, "b1", Now)
		unknown := uuid.New()
		publication := &collectors.Ref{Source: collectors.SourcePublication, ID: PubA}
		if _, err := s.Collectors.Pin(ctx, OrgA, c.Fleet.ID, a3.ID, publication, Now); err != nil {
			t.Fatal(err)
		}
		beforeB := s.Dump(t, OrgB)
		foreignBefore := get(t, s, foreign)

		ids := []uuid.UUID{a2.ID, a1.ID, foreign.ID, b.ID, unknown, a1.ID, a3.ID}
		later := Now.Add(time.Hour)
		result, err := s.Collectors.BulkPin(ctx, OrgA, c.Fleet.ID, ids, &collectors.PinChange{Ref: publication}, nil, later)
		if err != nil || result.Changed != 2 {
			t.Fatalf("BulkPin = %+v, %v", result, err)
		}
		sorted := func(ids ...uuid.UUID) []uuid.UUID {
			slices.SortFunc(ids, func(a, b uuid.UUID) int { return strings.Compare(string(a[:]), string(b[:])) })
			return ids
		}
		if Describe(result.Updated) != Describe(sorted(a1.ID, a2.ID, a3.ID)) ||
			Describe(result.NotFound) != Describe(sorted(foreign.ID, b.ID, unknown)) {
			t.Fatalf("BulkPin answered %+v", result)
		}
		for _, got := range []collectors.Collector{a1, a2, a3} {
			row := get(t, s, got)
			if row.Pinned == nil || *row.Pinned != *publication {
				t.Fatalf("%s isn't pinned: %+v", got.Name, row.Collector)
			}
		}
		if row := get(t, s, a3); !row.UpdatedAt.Equal(Millisecond(Now)) {
			t.Fatalf("an already pinned collector's UpdatedAt moved: %+v", row.Collector)
		}
		if row := get(t, s, a1); !row.UpdatedAt.Equal(Millisecond(later)) {
			t.Fatalf("a newly pinned collector's UpdatedAt: %+v", row.Collector)
		}
		if after := get(t, s, foreign); !reflect.DeepEqual(after, foreignBefore) {
			t.Fatalf("another fleet's collector changed: %+v", after)
		}
		s.Unchanged(t, "A's BulkPin", OrgB, beforeB)

		result, err = s.Collectors.BulkPin(ctx, OrgA, c.Fleet.ID, []uuid.UUID{a1.ID, a2.ID}, &collectors.PinChange{}, nil, later)
		if err != nil || result.Changed != 2 || len(result.NotFound) != 0 || len(result.Updated) != 2 {
			t.Fatalf("a bulk unpin = %+v, %v", result, err)
		}
		if row := get(t, s, a1); row.Pinned != nil {
			t.Fatalf("still pinned: %+v", row.Collector)
		}

		before := s.Dump(t, OrgA)
		many := make([]uuid.UUID, collectors.MaxBulkPin+1)
		for i := range many {
			many[i] = a1.ID
		}
		for label, call := range map[string]func() error{
			"no ids": func() error {
				_, err := s.Collectors.BulkPin(ctx, OrgA, c.Fleet.ID, nil, &collectors.PinChange{Ref: publication}, nil, later)
				return err
			},
			"501 ids": func() error {
				_, err := s.Collectors.BulkPin(ctx, OrgA, c.Fleet.ID, many, &collectors.PinChange{Ref: publication}, nil, later)
				return err
			},
			"an invalid ref": func() error {
				_, err := s.Collectors.BulkPin(ctx, OrgA, c.Fleet.ID, []uuid.UUID{a1.ID}, &collectors.PinChange{Ref: &collectors.Ref{ID: PubA}}, nil, later)
				return err
			},
		} {
			if err := call(); !errors.Is(err, collectors.ErrInvalid) {
				t.Errorf("%s: BulkPin = %v, want ErrInvalid", label, err)
			}
		}
		if _, err := s.Collectors.BulkPin(ctx, OrgA, c.Fleet.ID, []uuid.UUID{a1.ID},
			&collectors.PinChange{Ref: &collectors.Ref{Source: collectors.SourcePublication, ID: PubB}}, nil, later); !errors.Is(err, collectors.ErrPublicationNotFound) {
			t.Errorf("B's published version: %v", err)
		}
		if _, err := s.Collectors.BulkPin(ctx, OrgB, c.Fleet.ID, []uuid.UUID{a1.ID}, &collectors.PinChange{}, nil, later); !errors.Is(err, collectors.ErrFleetNotFound) {
			t.Errorf("A's fleet under B: %v", err)
		}
		if result, err := s.Collectors.BulkPin(ctx, OrgA, c.Fleet.ID, many[:collectors.MaxBulkPin], &collectors.PinChange{}, nil, later); err != nil ||
			result.Changed != 0 || len(result.Updated) != 1 {
			t.Errorf("500 ids, one distinct, already unpinned = %+v, %v", result, err)
		}
		s.Unchanged(t, "the refused bulk pins", OrgA, before)
	})
}

// Every method, called with another organization's ids, or with the
// organization's own collector through another of its fleets, answers as if
// it didn't exist and changes nothing.
func TestAnotherOrganizationsOrFleetsCollectorIsNotFoundAndUnchanged(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		mine := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		sibling := s.CreateFleet(t, OrgA, "edge-dev", fleets.TypeServer)
		theirs := s.CreateFleet(t, OrgB, "globex", fleets.TypeServer)
		theirCollector := enroll(t, s, theirs, "globex-01", Now)
		siblingCollector := enroll(t, s, sibling, "edge-dev-01", Now)
		enroll(t, s, mine, "edge-01", Now)
		beforeA, beforeB := s.Dump(t, OrgA), s.Dump(t, OrgB)

		for label, target := range map[string]struct {
			org, fleet uuid.UUID
			collector  collectors.Collector
			group      uuid.UUID
		}{
			"another organization's":               {OrgA, theirs.Fleet.ID, theirCollector, theirs.DefaultGroupID},
			"another organization's, through mine": {OrgA, mine.Fleet.ID, theirCollector, mine.DefaultGroupID},
			"another fleet's, through mine":        {OrgA, mine.Fleet.ID, siblingCollector, mine.DefaultGroupID},
			"B's, under B through A's fleet":       {OrgB, mine.Fleet.ID, theirCollector, mine.DefaultGroupID},
		} {
			id := target.collector.ID
			if _, err := s.Collectors.Get(ctx, target.org, target.fleet, id); !errors.Is(err, collectors.ErrNotFound) {
				t.Errorf("%s: Get = %v", label, err)
			}
			if _, err := s.Collectors.Rename(ctx, target.org, target.fleet, id, "mine", Now); !errors.Is(err, collectors.ErrNotFound) {
				t.Errorf("%s: Rename = %v", label, err)
			}
			if _, err := s.Collectors.Move(ctx, target.org, target.fleet, id, target.group, Now); !errors.Is(err, collectors.ErrNotFound) {
				t.Errorf("%s: Move = %v", label, err)
			}
			if _, err := s.Collectors.Pin(ctx, target.org, target.fleet, id, nil, Now); !errors.Is(err, collectors.ErrNotFound) {
				t.Errorf("%s: Pin = %v", label, err)
			}
			if _, err := s.Collectors.Update(ctx, target.org, target.fleet, id, collectors.Changes{Name: Ptr("mine"),
				GroupID: &target.group, Pin: &collectors.PinChange{}}, Now); !errors.Is(err, collectors.ErrNotFound) {
				t.Errorf("%s: Update = %v", label, err)
			}
			if deleted, err := s.Collectors.Delete(ctx, target.org, target.fleet, id); err != nil || deleted {
				t.Errorf("%s: Delete = %v, %v", label, deleted, err)
			}
			if err := s.Collectors.RecordError(ctx, target.org, target.fleet, id, collectors.ErrorReport{Message: "x", At: Now}); !errors.Is(err, collectors.ErrNotFound) {
				t.Errorf("%s: RecordError = %v", label, err)
			}
			if _, err := s.Collectors.ListErrors(ctx, target.org, target.fleet, id, 10, nil); !errors.Is(err, collectors.ErrNotFound) {
				t.Errorf("%s: ListErrors = %v", label, err)
			}
			if written, err := s.Collectors.Report(ctx, collectors.Report{OrganizationID: target.org, FleetID: target.fleet,
				CollectorID: id, Reported: collectors.Reported{IP: Ptr("10.9.9.9")}}); err != nil || written {
				t.Errorf("%s: Report = %v, %v", label, written, err)
			}
			if result, err := s.Collectors.BulkPin(ctx, target.org, target.fleet, []uuid.UUID{id}, &collectors.PinChange{}, nil, Now); err == nil &&
				(len(result.Updated) != 0 || len(result.NotFound) != 1) {
				t.Errorf("%s: BulkPin = %+v", label, result)
			}
			if page, err := s.Collectors.List(ctx, target.org, target.fleet, collectors.ListOptions{Sort: collectors.SortCreated,
				Order: collectors.Descending, Limit: 100}); err == nil && slices.ContainsFunc(page.Collectors, func(l collectors.Listed) bool { return l.ID == id }) {
				t.Errorf("%s: List shows it", label)
			}
		}
		// Pinning B's published version on my collector is refused.
		row, err := s.Collectors.List(ctx, OrgA, mine.Fleet.ID, collectors.ListOptions{Sort: collectors.SortCreated, Order: collectors.Descending, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Collectors.Pin(ctx, OrgA, mine.Fleet.ID, row.Collectors[0].ID,
			&collectors.Ref{Source: collectors.SourcePublication, ID: PubB}, Now); !errors.Is(err, collectors.ErrPublicationNotFound) {
			t.Errorf("B's published version: Pin = %v", err)
		}
		if summaries, err := s.Collectors.SummariesForFleets(ctx, OrgA, []uuid.UUID{theirs.Fleet.ID, mine.Fleet.ID}, Now); err != nil ||
			len(summaries) != 1 || summaries[mine.Fleet.ID].Collectors != 1 {
			t.Errorf("Summaries with B's fleet = %+v, %v", summaries, err)
		}
		s.Unchanged(t, "the calls", OrgA, beforeA)
		s.Unchanged(t, "the calls", OrgB, beforeB)
	})
}

// Update (the management API's PATCH) renames, moves and pins (or unpins)
// in one transaction, any of them, answering the collector with what now
// resolves for it; UpdatedAt moves only when something changes. A refusal
// of any part (an unknown group, a pin target the organization's fleet
// doesn't hold, a name taken), in that order, changes nothing, not even the
// parts that would have passed.
func TestUpdateChangesAllOrNothing(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		other := s.CreateFleet(t, OrgA, "edge-dev", fleets.TypeServer)
		got := enroll(t, s, c, "edge-01", Now)
		enroll(t, s, other, "edge-dev-01", Now)
		g, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID, Name: "gateways",
			AssignedPublicationID: Ptr(PubA), At: Now})
		if err != nil {
			t.Fatal(err)
		}
		later := Now.Add(time.Hour)

		updated, err := s.Collectors.Update(ctx, OrgA, c.Fleet.ID, got.ID, collectors.Changes{Name: Ptr("Edge-01"), GroupID: &g.ID}, later)
		if err != nil || updated.Name != "Edge-01" || updated.GroupID != g.ID || !updated.UpdatedAt.Equal(Millisecond(later)) ||
			updated.Resolution.From != collectors.FromGroupPublication || updated.Resolution.ID != PubA {
			t.Fatalf("Update (rename and move) = %+v, %v", updated, err)
		}
		publication := &collectors.Ref{Source: collectors.SourcePublication, ID: publish(t, s, OrgA, "exporters: {}\n")}
		pinned, err := s.Collectors.Update(ctx, OrgA, c.Fleet.ID, got.ID, collectors.Changes{Pin: &collectors.PinChange{Ref: publication}}, later.Add(time.Hour))
		if err != nil || pinned.Pinned == nil || *pinned.Pinned != *publication || pinned.Name != "Edge-01" ||
			pinned.Resolution.From != collectors.FromPinnedPublication || pinned.Resolution.ID != publication.ID ||
			!pinned.UpdatedAt.Equal(Millisecond(later.Add(time.Hour))) {
			t.Fatalf("Update (pin) = %+v, %v", pinned, err)
		}
		same, err := s.Collectors.Update(ctx, OrgA, c.Fleet.ID, got.ID, collectors.Changes{Name: Ptr("Edge-01"), GroupID: &g.ID,
			Pin: &collectors.PinChange{Ref: publication}}, later.Add(2*time.Hour))
		if err != nil || !same.UpdatedAt.Equal(pinned.UpdatedAt) {
			t.Fatalf("Update to what it is moved UpdatedAt: %+v, %v", same, err)
		}
		unpinned, err := s.Collectors.Update(ctx, OrgA, c.Fleet.ID, got.ID, collectors.Changes{Pin: &collectors.PinChange{}}, later.Add(3*time.Hour))
		if err != nil || unpinned.Pinned != nil || unpinned.Resolution.From != collectors.FromGroupPublication {
			t.Fatalf("Update (unpin) = %+v, %v", unpinned, err)
		}

		theirGroup := s.CreateFleet(t, OrgB, "globex", fleets.TypeServer).DefaultGroupID
		before := s.Dump(t, OrgA)
		valid := collectors.Changes{Name: Ptr("renamed"), GroupID: &c.DefaultGroupID, Pin: &collectors.PinChange{Ref: publication}}
		for label, want := range map[string]struct {
			changes collectors.Changes
			err     error
		}{
			"another fleet's group, with a valid name and pin": {collectors.Changes{Name: valid.Name, GroupID: &other.DefaultGroupID, Pin: valid.Pin},
				collectors.ErrGroupNotFound},
			"another organization's group": {collectors.Changes{Name: valid.Name, GroupID: &theirGroup}, collectors.ErrGroupNotFound},
			"B's published version, with a valid name and group": {collectors.Changes{Name: valid.Name, GroupID: valid.GroupID,
				Pin: &collectors.PinChange{Ref: &collectors.Ref{Source: collectors.SourcePublication, ID: PubB}}}, collectors.ErrPublicationNotFound},
			"an unknown group before an unknown published version": {collectors.Changes{GroupID: Ptr(uuid.New()),
				Pin: &collectors.PinChange{Ref: &collectors.Ref{Source: collectors.SourcePublication, ID: uuid.New()}}}, collectors.ErrGroupNotFound},
			"a name another fleet's collector has, with a valid group and pin": {collectors.Changes{Name: Ptr("edge-dev-01"),
				GroupID: valid.GroupID, Pin: valid.Pin}, collectors.ErrNameTaken},
			"an invalid name":   {collectors.Changes{Name: Ptr(" edge"), GroupID: valid.GroupID}, collectors.ErrInvalid},
			"an invalid ref":    {collectors.Changes{Pin: &collectors.PinChange{Ref: &collectors.Ref{Source: collectors.SourcePublication}}}, collectors.ErrInvalid},
			"an import's ref":   {collectors.Changes{Pin: &collectors.PinChange{Ref: &collectors.Ref{Source: "import", ID: PubA}}}, collectors.ErrInvalid},
			"nothing to change": {collectors.Changes{}, collectors.ErrInvalid},
		} {
			if _, err := s.Collectors.Update(ctx, OrgA, c.Fleet.ID, got.ID, want.changes, later.Add(4*time.Hour)); !errors.Is(err, want.err) {
				t.Errorf("%s: Update = %v, want %v", label, err, want.err)
			}
		}
		s.Unchanged(t, "the refused updates", OrgA, before)
		if _, err := s.Collectors.Update(ctx, OrgA, c.Fleet.ID, uuid.New(), valid, later); !errors.Is(err, collectors.ErrNotFound) {
			t.Fatalf("an unknown collector: Update = %v", err)
		}
		if _, err := s.Collectors.Update(ctx, OrgA, uuid.New(), got.ID, valid, later); !errors.Is(err, collectors.ErrNotFound) {
			t.Fatalf("an unknown fleet: Update = %v", err)
		}
	})
}

// pinTo is a pin change to the published version id.
func pinTo(id uuid.UUID) *collectors.PinChange {
	return &collectors.PinChange{Ref: &collectors.Ref{Source: collectors.SourcePublication, ID: id}}
}

// sameVariableGroup reports whether the nullable ids are the same.
func sameVariableGroup(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// A pin's variable group is independent of its pinned published version:
// either, both or neither, set and cleared apart; each change of the
// variable group (never of the version alone) moves the counter on once and
// stamps the collector. Another organization's or an unknown variable group
// is ErrVariableGroupNotFound and changes nothing.
func TestAPinsVariableGroupIsIndependentOfItsPublishedVersion(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		got := enroll(t, s, c, "edge-01", Now)
		vg := newVariableGroup(t, s, OrgA, "eu")
		theirs := newVariableGroup(t, s, OrgB, "eu")
		check := func(label string, l collectors.Listed, err error, pinned *uuid.UUID, variableGroup *uuid.UUID, stamp int64) {
			t.Helper()
			var pin *uuid.UUID
			if l.Pinned != nil {
				pin = &l.Pinned.ID
			}
			if err != nil || !sameVariableGroup(pin, pinned) || !sameVariableGroup(l.PinnedVariableGroupID, variableGroup) ||
				stampOf(l.PinnedVariableGroupStamp) != stamp {
				t.Fatalf("%s = %+v (pin %v, variable group %v, stamp %d), %v", label, l.Collector, pin, l.PinnedVariableGroupID,
					stampOf(l.PinnedVariableGroupStamp), err)
			}
			if again := get(t, s, got); !reflect.DeepEqual(again, l) {
				t.Fatalf("%s: Get = %+v; want %+v", label, again, l)
			}
		}
		start := revision(t, s, OrgA)
		update := func(changes collectors.Changes) (collectors.Listed, error) {
			return s.Collectors.Update(ctx, OrgA, c.Fleet.ID, got.ID, changes, Now.Add(time.Hour))
		}
		l, err := update(collectors.Changes{PinnedVariableGroup: &collectors.VariableGroupChange{ID: &vg}})
		check("the variable group alone", l, err, nil, &vg, start+1)
		l, err = update(collectors.Changes{Pin: pinTo(PubA)})
		check("the version alone, the variable group kept", l, err, &PubA, &vg, start+1)
		l, err = update(collectors.Changes{PinnedVariableGroup: &collectors.VariableGroupChange{}})
		check("the variable group cleared, the version kept", l, err, &PubA, nil, start+2)
		l, err = update(collectors.Changes{Pin: pinTo(PubA), PinnedVariableGroup: &collectors.VariableGroupChange{ID: &vg}})
		check("both", l, err, &PubA, &vg, start+3)
		l, err = update(collectors.Changes{Pin: &collectors.PinChange{}, PinnedVariableGroup: &collectors.VariableGroupChange{}})
		check("neither", l, err, nil, nil, start+4)
		l, err = update(collectors.Changes{PinnedVariableGroup: &collectors.VariableGroupChange{}})
		check("cleared again: no change", l, err, nil, nil, start+4)
		if r := revision(t, s, OrgA); r != start+4 {
			t.Fatalf("the counter = %d, want %d", r, start+4)
		}

		before, beforeB := s.Dump(t, OrgA), s.Dump(t, OrgB)
		for label, id := range map[string]uuid.UUID{"another organization's": theirs, "an unknown": uuid.New()} {
			if _, err := update(collectors.Changes{Name: Ptr("renamed"), Pin: pinTo(PubA),
				PinnedVariableGroup: &collectors.VariableGroupChange{ID: &id}}); !errors.Is(err, collectors.ErrVariableGroupNotFound) {
				t.Errorf("%s variable group: Update = %v", label, err)
			}
			if _, err := s.Collectors.BulkPin(ctx, OrgA, c.Fleet.ID, []uuid.UUID{got.ID}, nil,
				&collectors.VariableGroupChange{ID: &id}, Now); !errors.Is(err, collectors.ErrVariableGroupNotFound) {
				t.Errorf("%s variable group: BulkPin = %v", label, err)
			}
		}
		s.Unchanged(t, "the refused variable groups", OrgA, before)
		s.Unchanged(t, "the refused variable groups", OrgB, beforeB)
	})
}

// Clearing a pin's variable group returns the collector to its group's: its
// resolution answers the group's variable group under a pin and without,
// and what it is filled in with comes from the group's override again.
func TestClearingAPinsVariableGroupReturnsTheCollectorToItsGroups(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		region := defineVariable(t, s, OrgA, "REGION", "organization")
		groupLevel, pinLevel := newVariableGroup(t, s, OrgA, "group-level"), newVariableGroup(t, s, OrgA, "pin-level")
		setOverride(t, s, OrgA, groupLevel, region.ID, "from-group")
		setOverride(t, s, OrgA, pinLevel, region.ID, "from-pin")
		bindGroup(t, s, c, c.DefaultGroupID, &groupLevel)
		assignGroup(t, s, c, c.DefaultGroupID, publish(t, s, OrgA, "a: ${org:REGION}\n"))
		got := enroll(t, s, c, "edge-01", Now)
		value := func(l collectors.Listed) string {
			t.Helper()
			r, err := s.Variables.Variables(ctx, OrgA, l.Resolution.GroupVariableGroupID, l.PinnedVariableGroupID)
			if err != nil {
				t.Fatal(err)
			}
			return r.Values["REGION"]
		}
		l := pinVariableGroup(t, s, got, &pinLevel)
		if !sameVariableGroup(l.Resolution.GroupVariableGroupID, &groupLevel) || value(l) != "from-pin" {
			t.Fatalf("pinned: the group's variable group %v, REGION %q", l.Resolution.GroupVariableGroupID, value(l))
		}
		// Under a pinned version too, the group's variable group applies.
		if _, err := s.Collectors.Pin(ctx, OrgA, c.Fleet.ID, got.ID, &collectors.Ref{Source: collectors.SourcePublication,
			ID: publish(t, s, OrgA, "b: ${org:REGION}\n")}, Now); err != nil {
			t.Fatal(err)
		}
		if l := get(t, s, got); l.Resolution.From != collectors.FromPinnedPublication ||
			!sameVariableGroup(l.Resolution.GroupVariableGroupID, &groupLevel) || l.Resolution.GroupBindingStamp == 0 {
			t.Fatalf("under a pinned version: %+v", l.Resolution)
		}
		l = pinVariableGroup(t, s, got, nil)
		if l.PinnedVariableGroupID != nil || value(l) != "from-group" {
			t.Fatalf("cleared: pinned variable group %v, REGION %q; want the group's", l.PinnedVariableGroupID, value(l))
		}
	})
}

// A pin to a published version that references names no variable of the
// organization defines is refused with the names (at most 100 in byte
// order, the total exact), by Update, Pin and BulkPin, changing nothing
// (the counter included, though BulkPin also asked for a variable group);
// an unpin checks nothing.
func TestAPinNamingAnUndefinedVariableIsRefused(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		got := enroll(t, s, c, "edge-01", Now)
		defineVariable(t, s, OrgA, "REGION", "eu")
		defineVariable(t, s, OrgB, "TENANT", "theirs")
		yaml := "a: ${org:REGION}\nb: ${org:TENANT}\n"
		for i := range 101 {
			yaml += fmt.Sprintf("n%d: ${org:N_%03d}\n", i, i)
		}
		version := publish(t, s, OrgA, yaml)
		vg := newVariableGroup(t, s, OrgA, "eu")
		var want []string
		for i := range 99 {
			want = append(want, fmt.Sprintf("N_%03d", i))
		}
		want = append(want, "N_099")
		before := s.Dump(t, OrgA)
		check := func(label string, err error) {
			t.Helper()
			var undefined *collectors.UndefinedError
			if !errors.As(err, &undefined) || !errors.Is(err, collectors.ErrVariablesUndefined) ||
				undefined.Undefined.Total != 102 || !reflect.DeepEqual(undefined.Undefined.Names, want) {
				t.Fatalf("%s = %v; want the first 100 of 102 undefined names", label, err)
			}
			if strings.Contains(err.Error(), "N_0") || strings.Contains(err.Error(), "TENANT") {
				t.Fatalf("%s: the error %q quotes a name", label, err)
			}
		}
		_, err := s.Collectors.Update(ctx, OrgA, c.Fleet.ID, got.ID, collectors.Changes{Name: Ptr("renamed"), Pin: pinTo(version),
			PinnedVariableGroup: &collectors.VariableGroupChange{ID: &vg}}, Now)
		check("Update", err)
		_, err = s.Collectors.Pin(ctx, OrgA, c.Fleet.ID, got.ID, &collectors.Ref{Source: collectors.SourcePublication, ID: version}, Now)
		check("Pin", err)
		_, err = s.Collectors.BulkPin(ctx, OrgA, c.Fleet.ID, []uuid.UUID{got.ID}, pinTo(version),
			&collectors.VariableGroupChange{ID: &vg}, Now)
		check("BulkPin", err)
		s.Unchanged(t, "the refused pins", OrgA, before)

		pinPastTheCheck(t, s, got.ID, version)
		if _, err := s.Collectors.Pin(ctx, OrgA, c.Fleet.ID, got.ID, nil, Now); err != nil {
			t.Fatalf("an unpin: %v", err)
		}
	})
}

// The bulk pin's variable group set on every matched collector whose pinned
// variable group differs, with one stamp for the call (the counter moved on
// once), the others left as they were; the same again changes nothing and
// moves no counter.
func TestBulkPinSetsTheVariableGroupWithOneStamp(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		a, b, already := enroll(t, s, c, "a", Now), enroll(t, s, c, "b", Now), enroll(t, s, c, "already", Now)
		vg := newVariableGroup(t, s, OrgA, "eu")
		alreadyStamp := stampOf(pinVariableGroup(t, s, already, &vg).PinnedVariableGroupStamp)
		start := revision(t, s, OrgA)
		later := Now.Add(time.Hour)
		result, err := s.Collectors.BulkPin(ctx, OrgA, c.Fleet.ID, []uuid.UUID{a.ID, b.ID, already.ID, uuid.New()}, nil,
			&collectors.VariableGroupChange{ID: &vg}, later)
		if err != nil || result.Changed != 2 || len(result.Updated) != 3 || len(result.NotFound) != 1 {
			t.Fatalf("BulkPin = %+v, %v", result, err)
		}
		if r := revision(t, s, OrgA); r != start+1 {
			t.Fatalf("the counter = %d, want one move to %d", r, start+1)
		}
		for _, got := range []collectors.Collector{a, b} {
			l := get(t, s, got)
			if !sameVariableGroup(l.PinnedVariableGroupID, &vg) || stampOf(l.PinnedVariableGroupStamp) != start+1 ||
				!l.UpdatedAt.Equal(Millisecond(later)) || l.Pinned != nil {
				t.Fatalf("%s: %+v; want the variable group with the call's stamp", got.Name, l.Collector)
			}
		}
		if l := get(t, s, already); stampOf(l.PinnedVariableGroupStamp) != alreadyStamp || !l.UpdatedAt.Equal(Millisecond(Now)) {
			t.Fatalf("the collector that had it: %+v; want it as it was", l.Collector)
		}
		result, err = s.Collectors.BulkPin(ctx, OrgA, c.Fleet.ID, []uuid.UUID{a.ID, b.ID}, nil,
			&collectors.VariableGroupChange{ID: &vg}, later.Add(time.Hour))
		if err != nil || result.Changed != 0 || revision(t, s, OrgA) != start+1 {
			t.Fatalf("the same again = %+v, %v, the counter %d", result, err, revision(t, s, OrgA))
		}
	})
}

// The bulk pin needs a pin or a variable group (or both); with neither it
// is ErrInvalid and changes nothing.
func TestBulkPinNeedsAPinOrAVariableGroup(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		a := enroll(t, s, c, "a", Now)
		before := s.Dump(t, OrgA)
		if _, err := s.Collectors.BulkPin(ctx, OrgA, c.Fleet.ID, []uuid.UUID{a.ID}, nil, nil, Now); !errors.Is(err, collectors.ErrInvalid) {
			t.Fatalf("neither = %v, want ErrInvalid", err)
		}
		s.Unchanged(t, "a bulk pin of nothing", OrgA, before)
		vg := newVariableGroup(t, s, OrgA, "eu")
		if _, err := s.Collectors.BulkPin(ctx, OrgA, c.Fleet.ID, []uuid.UUID{a.ID}, pinTo(PubA),
			&collectors.VariableGroupChange{ID: &vg}, Now); err != nil {
			t.Fatalf("both = %v", err)
		}
		if l := get(t, s, a); l.Pinned == nil || !sameVariableGroup(l.PinnedVariableGroupID, &vg) {
			t.Fatalf("both: %+v", l.Collector)
		}
	})
}

// Absent and null differ. A nil pin keeps each collector's pin as it is,
// &PinChange{} unpins ("follow each collector's group"); a nil variable
// group keeps each one's, and &VariableGroupChange{} clears it.
func TestBulkPinLeavesAnAbsentChoiceAsIs(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		a, b := enroll(t, s, c, "a", Now), enroll(t, s, c, "b", Now)
		eu, us := newVariableGroup(t, s, OrgA, "eu"), newVariableGroup(t, s, OrgA, "us")
		other := publish(t, s, OrgA, "exporters: {}\n")
		if _, err := s.Collectors.Update(ctx, OrgA, c.Fleet.ID, a.ID, collectors.Changes{Pin: pinTo(PubA),
			PinnedVariableGroup: &collectors.VariableGroupChange{ID: &eu}}, Now); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Collectors.Update(ctx, OrgA, c.Fleet.ID, b.ID, collectors.Changes{Pin: pinTo(other)}, Now); err != nil {
			t.Fatal(err)
		}
		ids := []uuid.UUID{a.ID, b.ID}
		state := func() [2]string {
			t.Helper()
			var out [2]string
			for i, got := range []collectors.Collector{a, b} {
				l := get(t, s, got)
				pin, variableGroup := "none", "none"
				if l.Pinned != nil {
					pin = l.Pinned.ID.String()
				}
				if l.PinnedVariableGroupID != nil {
					variableGroup = l.PinnedVariableGroupID.String()
				}
				out[i] = pin + " " + variableGroup
			}
			return out
		}
		expect := func(label string, want [2]string) {
			t.Helper()
			if got := state(); got != want {
				t.Fatalf("%s: %v, want %v", label, got, want)
			}
		}
		bulk := func(pin *collectors.PinChange, vg *collectors.VariableGroupChange) {
			t.Helper()
			if _, err := s.Collectors.BulkPin(ctx, OrgA, c.Fleet.ID, ids, pin, vg, Now); err != nil {
				t.Fatal(err)
			}
		}
		bulk(nil, &collectors.VariableGroupChange{ID: &us})
		expect("the variable group set, the pins left", [2]string{PubA.String() + " " + us.String(), other.String() + " " + us.String()})
		bulk(pinTo(other), nil)
		expect("a pin set, the variable groups left", [2]string{other.String() + " " + us.String(), other.String() + " " + us.String()})
		bulk(nil, &collectors.VariableGroupChange{})
		expect("the variable groups cleared, the pins left", [2]string{other.String() + " none", other.String() + " none"})
		bulk(&collectors.PinChange{}, nil)
		expect("unpinned, the variable groups left", [2]string{"none none", "none none"})
	})
}

// A move between groups whose variable groups differ moves the counter on
// once and stamps the collector's row, by Move and by Update; a move
// between groups of the same variable group (or none), or into its own
// group, stamps nothing.
func TestAMoveBetweenGroupsOfDifferentVariableGroupsStampsTheCollector(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		eu, us := newVariableGroup(t, s, OrgA, "eu"), newVariableGroup(t, s, OrgA, "us")
		makeGroup := func(name string, vg *uuid.UUID) uuid.UUID {
			g, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID, Name: name,
				VariableGroupID: vg, At: Now})
			if err != nil {
				t.Fatal(err)
			}
			return g.ID
		}
		euA, euB, usA, plainA, plainB := makeGroup("eu-a", &eu), makeGroup("eu-b", &eu), makeGroup("us-a", &us),
			makeGroup("plain-a", nil), makeGroup("plain-b", nil)
		got := enroll(t, s, c, "edge-01", Now)
		move := func(label string, byUpdate bool, group uuid.UUID, stamps bool) {
			t.Helper()
			before := revision(t, s, OrgA)
			old := stampOf(get(t, s, got).PinnedVariableGroupStamp)
			var err error
			if byUpdate {
				_, err = s.Collectors.Update(ctx, OrgA, c.Fleet.ID, got.ID, collectors.Changes{GroupID: &group}, Now)
			} else {
				_, err = s.Collectors.Move(ctx, OrgA, c.Fleet.ID, got.ID, group, Now)
			}
			l := get(t, s, got)
			switch {
			case err != nil || l.GroupID != group:
				t.Fatalf("%s: %v, in %v", label, err, l.GroupID)
			case stamps && (revision(t, s, OrgA) != before+1 || stampOf(l.PinnedVariableGroupStamp) != before+1):
				t.Fatalf("%s: the counter %d (from %d), the stamp %d; want one move, stamped", label,
					revision(t, s, OrgA), before, stampOf(l.PinnedVariableGroupStamp))
			case !stamps && (revision(t, s, OrgA) != before || stampOf(l.PinnedVariableGroupStamp) != old):
				t.Fatalf("%s: the counter %d (from %d), the stamp %d (from %d); want neither moved", label,
					revision(t, s, OrgA), before, stampOf(l.PinnedVariableGroupStamp), old)
			}
		}
		for _, byUpdate := range []bool{false, true} {
			move("from Default (none) to plain-a (none)", byUpdate, plainA, false)
			move("plain-a to plain-b", byUpdate, plainB, false)
			move("none to eu", byUpdate, euA, true)
			move("eu to eu", byUpdate, euB, false)
			move("into its own group", byUpdate, euB, false)
			move("eu to us", byUpdate, usA, true)
			move("us to none", byUpdate, c.DefaultGroupID, true)
		}
		// Under a pinned variable group the move is still stamped: the
		// group's variable group applies under the pin's.
		pinVariableGroup(t, s, got, &us)
		move("pinned: none to eu", true, euA, true)
	})
}
