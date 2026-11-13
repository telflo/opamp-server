package variableuse_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/telflo/opamp-server/store/collectorgroups"
	"github.com/telflo/opamp-server/store/fleets"
	"github.com/telflo/opamp-server/store/publications"
	"github.com/telflo/opamp-server/store/storetest"
	"github.com/telflo/opamp-server/store/variableuse"
)

// This package's own test database (CONTRIBUTING.md, "Tests").
const testDatabase = "telflo_opamp_server_variableuse_test"

// helpers calls variableuse, each call in its own transaction of the
// server's role, the organization's row locked first as a caller does
// (committed only for a bump).
type helpers struct {
	lock      func(organizationID uuid.UUID) bool
	bump      func(organizationID uuid.UUID) (int64, error)
	undefined func(organizationID, publicationID uuid.UUID) variableuse.UndefinedNames
	usedBy    func(organizationID uuid.UUID, name string) variableuse.Uses
	groupUse  func(organizationID, variableGroupID uuid.UUID) variableuse.GroupUses
}

func on(t *testing.T, s *storetest.Stores) helpers {
	ctx := context.Background()
	inTx := func(org uuid.UUID, commit bool, fn func(tx pgx.Tx) error) bool {
		t.Helper()
		tx, err := s.Databases.Runtime.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		found, err := variableuse.LockOrganization(ctx, tx, org)
		if err != nil {
			t.Fatal(err)
		}
		if err := fn(tx); err != nil {
			t.Fatal(err)
		}
		if commit {
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
		return found
	}
	return helpers{
		lock: func(org uuid.UUID) bool {
			return inTx(org, false, func(pgx.Tx) error { return nil })
		},
		bump: func(org uuid.UUID) (n int64, bumpErr error) {
			inTx(org, true, func(tx pgx.Tx) error {
				n, bumpErr = variableuse.Bump(ctx, tx, org)
				return nil
			})
			return n, bumpErr
		},
		undefined: func(org, pub uuid.UUID) (u variableuse.UndefinedNames) {
			inTx(org, false, func(tx pgx.Tx) (err error) {
				u, err = variableuse.Undefined(ctx, tx, org, pub)
				return err
			})
			return u
		},
		usedBy: func(org uuid.UUID, name string) (u variableuse.Uses) {
			inTx(org, false, func(tx pgx.Tx) (err error) {
				u, err = variableuse.UsedBy(ctx, tx, org, name)
				return err
			})
			return u
		},
		groupUse: func(org, vg uuid.UUID) (u variableuse.GroupUses) {
			inTx(org, false, func(tx pgx.Tx) (err error) {
				u, err = variableuse.GroupUse(ctx, tx, org, vg)
				return err
			})
			return u
		},
	}
}

// publish publishes yaml as the organization's version id, through the
// store, which writes its names.
func publish(t *testing.T, s *storetest.Stores, org, id uuid.UUID, yaml string) {
	t.Helper()
	if result, _, err := s.Publications.Put(context.Background(), publications.Publication{
		ID: id, OrganizationID: org, ConfigurationID: uuid.New(),
		ConfigurationName: "payments-gateway", VersionID: uuid.New(), VersionNumber: 3,
		YAML: yaml, SHA256: strings.Repeat("ab", 32), PublishedAt: storetest.Now, ReceivedAt: storetest.Now,
		Collector: publications.Collector{Distribution: "otelcol-contrib", Version: "0.162.0"},
	}); err != nil || result != publications.Created {
		t.Fatalf("publishing %s: %v, %v", id, result, err)
	}
}

// publishWithoutNames stores a published version as a server before migration
// 00012 did: its row alone, with no names (none until it is republished).
func publishWithoutNames(t *testing.T, s *storetest.Stores, org, id uuid.UUID, yaml string) {
	t.Helper()
	if _, err := s.Databases.Runtime.Exec(context.Background(), `
		insert into opamp_server.publications (id, organization_id, configuration_id, configuration_name,
		  version_id, version_number, yaml, sha256, published_at, received_at, collector_distribution, collector_version)
		values ($1, $2, $3, 'old', $4, 1, $5, $6, $7, $7, 'otelcol-contrib', '0.162.0')`,
		id, org, uuid.New(), uuid.New(), yaml, strings.Repeat("ab", 32), storetest.Now); err != nil {
		t.Fatal(err)
	}
}

// define stores an organization variable named name, as the variables store
// does.
func define(t *testing.T, s *storetest.Stores, org uuid.UUID, name string) {
	t.Helper()
	id := uuid.New()
	if _, err := s.Databases.Runtime.Exec(context.Background(), `
		insert into opamp_server.variables (id, organization_id, name, description, value,
		  changed_by_id, changed_by_email, changed_at, stamp)
		values ($1, $2, $3, null, 'v', null, null, $4, 1)`, id, org, name, storetest.Now); err != nil {
		t.Fatal(err)
	}
}

// newVariableGroup stores a variable group of the organization's.
func newVariableGroup(t *testing.T, s *storetest.Stores, org uuid.UUID, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := s.Databases.Runtime.Exec(context.Background(), `
		insert into opamp_server.variable_groups (id, organization_id, name, description,
		  changed_by_id, changed_by_email, changed_at)
		values ($1, $2, $3, null, null, null, $4)`, id, org, name, storetest.Now); err != nil {
		t.Fatal(err)
	}
	return id
}

// bindGroup sets the collector group's variable group, as the groups' store
// does.
func bindGroup(t *testing.T, s *storetest.Stores, org, groupID, vg uuid.UUID) {
	t.Helper()
	if tag, err := s.Databases.Runtime.Exec(context.Background(), `
		update opamp_server.collector_groups set variable_group_id = $3, variable_group_stamp = 1
		 where organization_id = $1 and id = $2`, org, groupID, vg); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("binding group %s: %v, %v", groupID, tag, err)
	}
}

// pinVariableGroup pins a variable group on the collector, as the
// collectors' store does.
func pinVariableGroup(t *testing.T, s *storetest.Stores, org, collectorID, vg uuid.UUID) {
	t.Helper()
	if tag, err := s.Databases.Runtime.Exec(context.Background(), `
		update opamp_server.collector_instances set pinned_variable_group_id = $3, pinned_variable_group_stamp = 1
		 where organization_id = $1 and id = $2`, org, collectorID, vg); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("pinning a variable group on %s: %v, %v", collectorID, tag, err)
	}
}

// fleet is a fleet of the fixture's, with what creates its groups and
// collectors.
type fleet struct {
	t       *testing.T
	s       *storetest.Stores
	org     uuid.UUID
	id      uuid.UUID
	deflt   uuid.UUID
	created int
}

func newFleet(t *testing.T, s *storetest.Stores, org uuid.UUID, label string) *fleet {
	t.Helper()
	c := s.CreateFleet(t, org, label, fleets.TypeServer)
	return &fleet{t: t, s: s, org: org, id: c.Fleet.ID, deflt: c.DefaultGroupID}
}

// group creates a collector group assigned the published version (none when
// nil). The assignment is written under the groups' store, which refuses one
// whose names are undefined, and these tests read what such rows hold.
func (f *fleet) group(assigned *uuid.UUID) variableuse.GroupRef {
	f.t.Helper()
	f.created++
	g, err := f.s.Groups.Create(context.Background(), collectorgroups.NewGroup{OrganizationID: f.org, FleetID: f.id,
		Name: fmt.Sprintf("group-%d", f.created), At: storetest.Now})
	if err != nil {
		f.t.Fatal(err)
	}
	if assigned != nil {
		f.assign(g.ID, *assigned)
	}
	return variableuse.GroupRef{FleetID: f.id, GroupID: g.ID}
}

// assign assigns the published version to the fleet's group, under the
// groups' store.
func (f *fleet) assign(groupID, publicationID uuid.UUID) {
	f.t.Helper()
	if tag, err := f.s.Databases.Runtime.Exec(context.Background(), `
		update opamp_server.collector_groups set assigned_publication_id = $3
		 where organization_id = $1 and id = $2`, f.org, groupID, publicationID); err != nil || tag.RowsAffected() != 1 {
		f.t.Fatalf("assigning group %s: %v, %v", groupID, tag, err)
	}
}

// collector creates a collector in the fleet's Default group, pinned to
// the published version (none when nil).
func (f *fleet) collector(pinned *uuid.UUID) uuid.UUID {
	f.t.Helper()
	f.created++
	id := uuid.New()
	f.s.InsertCollector(f.t, storetest.Collector{ID: id, OrganizationID: f.org, FleetID: f.id, GroupID: f.deflt,
		Name: "collector-" + id.String(), PinnedPublicationID: pinned, LastSeenAt: storetest.Now})
	return id
}

// pubID is a published version's id that sorts by n (bytewise, as Postgres
// compares uuids) within the organization's prefix.
func pubID(org uuid.UUID, n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("%02x%06x-2222-4000-8000-%012x", org[0], n, n))
}

func sortedGroups(groups []variableuse.GroupRef) []variableuse.GroupRef {
	groups = slices.Clone(groups)
	slices.SortFunc(groups, func(a, b variableuse.GroupRef) int {
		if c := bytes.Compare(a.FleetID[:], b.FleetID[:]); c != 0 {
			return c
		}
		return bytes.Compare(a.GroupID[:], b.GroupID[:])
	})
	return groups
}

func describeUses(u variableuse.Uses) string {
	var b strings.Builder
	fmt.Fprintf(&b, "totals %d/%d/%d;", u.Publications, u.Groups, u.PinnedCollectors)
	for _, use := range u.List {
		fmt.Fprintf(&b, " %s: %d groups, %d pinned;", use.PublicationID, len(use.Groups), use.PinnedCollectors)
	}
	return b.String()
}

func equalUses(a, b variableuse.Uses) bool {
	return a.Publications == b.Publications && a.Groups == b.Groups && a.PinnedCollectors == b.PinnedCollectors &&
		slices.EqualFunc(a.List, b.List, func(x, y variableuse.Use) bool {
			return x.PublicationID == y.PublicationID && x.PinnedCollectors == y.PinnedCollectors && slices.Equal(x.Groups, y.Groups)
		})
}

// The names a version references that no variable of its organization has,
// compared as written, in byte order; a version published before migration
// 00012 has none.
func TestUndefinedIsTheReferencedNamesWithoutAVariable(t *testing.T) {
	storetest.Each(t, testDatabase, func(t *testing.T, s *storetest.Stores) {
		h := on(t, s)
		pub := pubID(storetest.OrgA, 1)
		publish(t, s, storetest.OrgA, pub, "a: ${org:zeta}\nb: ${org:DEFINED}\nc: ${org:Alpha}\nd: ${org:_under}\ne: $${org:ESCAPED}\n")
		define(t, s, storetest.OrgA, "DEFINED")
		define(t, s, storetest.OrgA, "alpha") // not Alpha: names are compared as written

		got := h.undefined(storetest.OrgA, pub)
		if want := []string{"Alpha", "_under", "zeta"}; !slices.Equal(got.Names, want) || got.Total != 3 {
			t.Fatalf("Undefined = %q, total %d; want %q, total 3", got.Names, got.Total, want)
		}

		// Every name defined: none.
		for _, name := range []string{"Alpha", "_under", "zeta"} {
			define(t, s, storetest.OrgA, name)
		}
		if got := h.undefined(storetest.OrgA, pub); len(got.Names) != 0 || got.Total != 0 || got.Names == nil {
			t.Fatalf("Undefined with every name defined = %q, total %d; want an empty list", got.Names, got.Total)
		}

		// A version with no reference, and one published before migration
		// 00012 (its names never written), have none; so has an id the
		// organization doesn't hold.
		plain, old := pubID(storetest.OrgA, 2), pubID(storetest.OrgA, 3)
		publish(t, s, storetest.OrgA, plain, "receivers: {}\n")
		publishWithoutNames(t, s, storetest.OrgA, old, "a: ${org:NEVER_DEFINED}\n")
		for label, id := range map[string]uuid.UUID{"no reference": plain, "published before migration 00012": old, "unknown": uuid.New()} {
			if got := h.undefined(storetest.OrgA, id); len(got.Names) != 0 || got.Total != 0 {
				t.Fatalf("Undefined of a version with %s = %q, total %d", label, got.Names, got.Total)
			}
		}
	})
}

// At most the first 100 names, in byte order, and the exact total.
func TestUndefinedListsTheFirstHundredNamesAndTheTotal(t *testing.T) {
	storetest.Each(t, testDatabase, func(t *testing.T, s *storetest.Stores) {
		h := on(t, s)
		var yaml strings.Builder
		for i := 149; i >= 0; i-- { // referenced in reverse, listed in byte order
			fmt.Fprintf(&yaml, "k%d: ${org:V_%03d}\n", i, i)
		}
		pub := pubID(storetest.OrgA, 1)
		publish(t, s, storetest.OrgA, pub, yaml.String())

		var want []string
		for i := range 100 {
			want = append(want, fmt.Sprintf("V_%03d", i))
		}
		if got := h.undefined(storetest.OrgA, pub); !slices.Equal(got.Names, want) || got.Total != 150 {
			t.Fatalf("Undefined = %d names, total %d; want V_000 to V_099, total 150", len(got.Names), got.Total)
		}

		// Ten defined: the next hundred, and 140.
		for i := range 10 {
			define(t, s, storetest.OrgA, fmt.Sprintf("V_%03d", i))
		}
		want = want[:0]
		for i := 10; i < 110; i++ {
			want = append(want, fmt.Sprintf("V_%03d", i))
		}
		if got := h.undefined(storetest.OrgA, pub); !slices.Equal(got.Names, want) || got.Total != 140 {
			t.Fatalf("Undefined with ten defined = %d names, total %d; want V_010 to V_109, total 140", len(got.Names), got.Total)
		}
	})
}

// A name is held by a published version that references it and that a
// collector group is assigned or a collector pinned to. An unassigned
// version, one published before migration 00012 (no names), and a version
// referencing another name don't count.
func TestUsedByCountsOnlyAssignedOrPinnedPublications(t *testing.T) {
	storetest.Each(t, testDatabase, func(t *testing.T, s *storetest.Stores) {
		h := on(t, s)
		org := storetest.OrgA
		unassigned, assigned, pinned, other, old, both := pubID(org, 1), pubID(org, 2), pubID(org, 3), pubID(org, 4), pubID(org, 5), pubID(org, 6)
		for _, id := range []uuid.UUID{unassigned, assigned, pinned, both} {
			publish(t, s, org, id, "a: ${org:HELD}\n")
		}
		publish(t, s, org, other, "a: ${org:OTHER}\nb: $${org:HELD}\n")
		publishWithoutNames(t, s, org, old, "a: ${org:HELD}\n")

		f := newFleet(t, s, org, "edge")
		assignedGroup := f.group(&assigned)
		f.group(&other)
		f.group(&old)
		f.group(nil)
		f.collector(&pinned)
		f.collector(&other)
		f.collector(&old)
		f.collector(nil)
		bothGroups := sortedGroups([]variableuse.GroupRef{f.group(&both), f.group(&both)})
		f.collector(&both)
		f.collector(&both)

		got := h.usedBy(org, "HELD")
		want := variableuse.Uses{
			List: []variableuse.Use{
				{PublicationID: assigned, Groups: []variableuse.GroupRef{assignedGroup}},
				{PublicationID: pinned, Groups: []variableuse.GroupRef{}, PinnedCollectors: 1},
				{PublicationID: both, Groups: bothGroups, PinnedCollectors: 2},
			},
			Publications: 3, Groups: 3, PinnedCollectors: 3,
		}
		if !equalUses(got, want) || !got.InUse() {
			t.Fatalf("UsedBy(HELD) = %s\nwant %s", describeUses(got), describeUses(want))
		}

		// No use: an empty list and zero totals, for a name only an
		// unassigned version references, a name written in another case,
		// and a name nothing references.
		publish(t, s, org, pubID(org, 7), "a: ${org:LONELY}\n")
		for _, name := range []string{"LONELY", "held", "NOWHERE"} {
			if got := h.usedBy(org, name); got.InUse() || len(got.List) != 0 || got.List == nil || got.Groups != 0 || got.PinnedCollectors != 0 {
				t.Fatalf("UsedBy(%s) = %s; want nothing", name, describeUses(got))
			}
		}
	})
}

// At most 100 entries in all (a group row, or a version pinned only), by
// publication, fleet and group; a listed version, a cut-short one included,
// keeps its exact pinned count, and the totals are exact.
func TestUsedByListsAtMostAHundredEntriesWithExactTotals(t *testing.T) {
	storetest.Each(t, testDatabase, func(t *testing.T, s *storetest.Stores) {
		h := on(t, s)
		org := storetest.OrgA
		first, second, third := pubID(org, 1), pubID(org, 2), pubID(org, 3)
		fl := []*fleet{newFleet(t, s, org, "edge"), newFleet(t, s, org, "core")}
		groups := map[uuid.UUID][]variableuse.GroupRef{}
		for _, id := range []uuid.UUID{first, second, third} {
			publish(t, s, org, id, "a: ${org:BUSY}\n")
			for i := range 60 {
				groups[id] = append(groups[id], fl[i%2].group(&id))
			}
			groups[id] = sortedGroups(groups[id])
		}
		// The first is also pinned on two collectors, the second (cut
		// short) on one, the third (not listed) on one.
		fl[0].collector(&first)
		fl[1].collector(&first)
		fl[1].collector(&second)
		fl[0].collector(&third)

		got := h.usedBy(org, "BUSY")
		want := variableuse.Uses{
			List: []variableuse.Use{
				{PublicationID: first, Groups: groups[first], PinnedCollectors: 2},
				{PublicationID: second, Groups: groups[second][:40], PinnedCollectors: 1},
			},
			Publications: 3, Groups: 180, PinnedCollectors: 4,
		}
		if !equalUses(got, want) {
			t.Fatalf("UsedBy(BUSY) = %s\nwant %s", describeUses(got), describeUses(want))
		}

		// 150 versions pinned only: 100 listed, each one entry, and 150.
		var pinnedOnly []variableuse.Use
		for i := range 150 {
			id := pubID(org, 100+i)
			publish(t, s, org, id, "a: ${org:PINNED_ONLY}\n")
			fl[i%2].collector(&id)
			pinnedOnly = append(pinnedOnly, variableuse.Use{PublicationID: id, Groups: []variableuse.GroupRef{}, PinnedCollectors: 1})
		}
		got = h.usedBy(org, "PINNED_ONLY")
		want = variableuse.Uses{List: pinnedOnly[:100], Publications: 150, Groups: 0, PinnedCollectors: 150}
		if !equalUses(got, want) {
			t.Fatalf("UsedBy(PINNED_ONLY) = %s\nwant %s", describeUses(got), describeUses(want))
		}
	})
}

// What uses a variable group, for its delete: at most the first 100
// collector groups bound to it, by fleet and then group, the exact total,
// and the collectors it is pinned on.
func TestGroupUseListsAtMostAHundredGroupsWithTheTotal(t *testing.T) {
	storetest.Each(t, testDatabase, func(t *testing.T, s *storetest.Stores) {
		h := on(t, s)
		org := storetest.OrgA
		busy, other, unused := newVariableGroup(t, s, org, "busy"), newVariableGroup(t, s, org, "other"), newVariableGroup(t, s, org, "unused")
		fl := []*fleet{newFleet(t, s, org, "edge"), newFleet(t, s, org, "core")}
		var bound []variableuse.GroupRef
		for i := range 150 {
			g := fl[i%2].group(nil)
			bindGroup(t, s, org, g.GroupID, busy)
			bound = append(bound, g)
		}
		bindGroup(t, s, org, fl[0].group(nil).GroupID, other)
		for i := range 3 {
			pinVariableGroup(t, s, org, fl[i%2].collector(nil), busy)
		}
		pinVariableGroup(t, s, org, fl[0].collector(nil), other)

		got := h.groupUse(org, busy)
		if want := sortedGroups(bound)[:100]; !slices.Equal(got.CollectorGroups, want) ||
			got.TotalCollectorGroups != 150 || got.PinnedCollectors != 3 || !got.InUse() {
			t.Fatalf("GroupUse(busy) = %d groups, total %d, pinned %d; want the first 100 by fleet and group, 150, 3",
				len(got.CollectorGroups), got.TotalCollectorGroups, got.PinnedCollectors)
		}
		if got := h.groupUse(org, other); len(got.CollectorGroups) != 1 || got.TotalCollectorGroups != 1 || got.PinnedCollectors != 1 {
			t.Fatalf("GroupUse(other) = %+v", got)
		}
		if got := h.groupUse(org, unused); got.InUse() || len(got.CollectorGroups) != 0 || got.CollectorGroups == nil {
			t.Fatalf("GroupUse(unused) = %+v; want nothing", got)
		}
	})
}

// Bump moves the organization's counter on by one, its values the stamps;
// on Postgres it runs under LockOrganization's `for no key update`, which
// a second such lock waits for while a `for key share` (a fleet write's)
// doesn't. An unregistered organization isn't locked or bumped.
func TestBumpMovesTheCounterByOneUnderTheOrganizationsLock(t *testing.T) {
	storetest.Each(t, testDatabase, func(t *testing.T, s *storetest.Stores) {
		h := on(t, s)
		for want := int64(1); want <= 3; want++ {
			if got, err := h.bump(storetest.OrgA); err != nil || got != want {
				t.Fatalf("Bump = %d, %v; want %d", got, err, want)
			}
		}
		if got, err := h.bump(storetest.OrgB); err != nil || got != 1 {
			t.Fatalf("B's first Bump = %d, %v; want 1", got, err)
		}
		if !h.lock(storetest.OrgA) || h.lock(storetest.OrgUnregistered) {
			t.Fatal("LockOrganization found the unregistered organization, or not A")
		}
		if _, err := h.bump(storetest.OrgUnregistered); !errors.Is(err, variableuse.ErrOrganizationNotFound) {
			t.Fatalf("Bump of an unregistered organization = %v, want ErrOrganizationNotFound", err)
		}

		ctx := context.Background()
		holder, err := s.Databases.Runtime.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = holder.Rollback(ctx) }()
		if found, err := variableuse.LockOrganization(ctx, holder, storetest.OrgA); err != nil || !found {
			t.Fatalf("LockOrganization = %v, %v", found, err)
		}
		probe := func(mode string) error {
			tx, err := s.Databases.Runtime.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			_, err = tx.Exec(ctx, `select 1 from opamp_server.organizations where id = $1 for `+mode+` nowait`, storetest.OrgA)
			return err
		}
		var pgErr *pgconn.PgError
		if err := probe("no key update"); !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("a second `for no key update` while LockOrganization holds the row = %v, want 55P03", err)
		}
		if err := probe("key share"); err != nil {
			t.Fatalf("a `for key share` while LockOrganization holds the row = %v, want no wait", err)
		}
	})
}

// Isolation (CONTRIBUTING.md, "SQL": organization_id is every query's first
// filter): every answer is the organization's own. B holds the same names,
// variables, uses and variable groups as A, and neither sees the other's.
func TestVariableUseIsTheOrganizationsOwn(t *testing.T) {
	storetest.Each(t, testDatabase, func(t *testing.T, s *storetest.Stores) {
		h := on(t, s)
		type world struct {
			pub, vg uuid.UUID
			group   variableuse.GroupRef
		}
		build := func(org uuid.UUID) world {
			w := world{pub: pubID(org, 1), vg: newVariableGroup(t, s, org, "shared")}
			publish(t, s, org, w.pub, "a: ${org:SHARED}\nb: ${org:ONLY_"+fmt.Sprintf("%02X", org[0])+"}\n")
			f := newFleet(t, s, org, "edge")
			w.group = f.group(&w.pub)
			bindGroup(t, s, org, w.group.GroupID, w.vg)
			pinVariableGroup(t, s, org, f.collector(&w.pub), w.vg)
			return w
		}
		a, b := build(storetest.OrgA), build(storetest.OrgB)
		// B defines both of A's names; A defines nothing.
		define(t, s, storetest.OrgB, "SHARED")
		define(t, s, storetest.OrgB, "ONLY_0A")

		// A's names are undefined in A, whatever B defines; B's version and
		// A's are each answered only to its own organization.
		if got := h.undefined(storetest.OrgA, a.pub); !slices.Equal(got.Names, []string{"ONLY_0A", "SHARED"}) || got.Total != 2 {
			t.Fatalf("A's Undefined = %q, total %d; want [ONLY_0A SHARED], 2", got.Names, got.Total)
		}
		if got := h.undefined(storetest.OrgA, b.pub); len(got.Names) != 0 || got.Total != 0 {
			t.Fatalf("A's Undefined of B's version = %q, total %d; want none", got.Names, got.Total)
		}
		if got := h.undefined(storetest.OrgB, a.pub); len(got.Names) != 0 || got.Total != 0 {
			t.Fatalf("B's Undefined of A's version = %q, total %d; want none", got.Names, got.Total)
		}

		// Each organization's use of SHARED is its own version only.
		for _, c := range []struct {
			org uuid.UUID
			w   world
		}{{storetest.OrgA, a}, {storetest.OrgB, b}} {
			want := variableuse.Uses{List: []variableuse.Use{{PublicationID: c.w.pub, Groups: []variableuse.GroupRef{c.w.group}, PinnedCollectors: 1}},
				Publications: 1, Groups: 1, PinnedCollectors: 1}
			if got := h.usedBy(c.org, "SHARED"); !equalUses(got, want) {
				t.Fatalf("UsedBy(SHARED) of %s = %s\nwant %s", c.org, describeUses(got), describeUses(want))
			}
			if got := h.groupUse(c.org, c.w.vg); !slices.Equal(got.CollectorGroups, []variableuse.GroupRef{c.w.group}) ||
				got.TotalCollectorGroups != 1 || got.PinnedCollectors != 1 {
				t.Fatalf("GroupUse of %s's own variable group = %+v", c.org, got)
			}
		}
		if got := h.usedBy(storetest.OrgB, "ONLY_0A"); got.InUse() {
			t.Fatalf("B's UsedBy of A's name = %s; want nothing", describeUses(got))
		}
		// Another organization's variable group has no use here.
		if got := h.groupUse(storetest.OrgB, a.vg); got.InUse() || len(got.CollectorGroups) != 0 {
			t.Fatalf("B's GroupUse of A's variable group = %+v; want nothing", got)
		}
		if got := h.groupUse(storetest.OrgA, b.vg); got.InUse() || len(got.CollectorGroups) != 0 {
			t.Fatalf("A's GroupUse of B's variable group = %+v; want nothing", got)
		}

		// A bump moves only its own organization's counter.
		if got, err := h.bump(storetest.OrgA); err != nil || got != 1 {
			t.Fatalf("A's Bump = %d, %v", got, err)
		}
		if got, err := h.bump(storetest.OrgB); err != nil || got != 1 {
			t.Fatalf("B's Bump after A's = %d, %v; want 1", got, err)
		}
	})
}
