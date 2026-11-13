package collectorgroups_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/collectorgroups"
	"github.com/telflo/opamp-server/store/collectors"
	"github.com/telflo/opamp-server/store/fleets"
	"github.com/telflo/opamp-server/store/publications"
	. "github.com/telflo/opamp-server/store/storetest"
	"github.com/telflo/opamp-server/store/variables"
	"github.com/telflo/opamp-server/store/variableuse"
)

// This package's own test database (CONTRIBUTING.md, "Tests").
const testDatabase = "telflo_opamp_server_collectorgroups_test"

var ctx = context.Background()

func named(org, fleet uuid.UUID, name string) collectorgroups.NewGroup {
	return collectorgroups.NewGroup{OrganizationID: org, FleetID: fleet, Name: name, At: Now}
}

// Create: a name and an assignment, the organization's own published
// version, on either fleet type (a kubernetes-supervisor fleet's group keeps
// its install fields in its annotations: no column of its own).
func TestCreateMakesAGroupWithItsAssignment(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		server := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		g := named(OrgA, server.Fleet.ID, "gateways ☃")
		g.AssignedPublicationID = Ptr(PubA)
		created, err := s.Groups.Create(ctx, g)
		at := Millisecond(Now)
		if err != nil || created.Name != "gateways ☃" || created.IsDefault || created.FleetID != server.Fleet.ID ||
			created.OrganizationID != OrgA || created.AssignedPublicationID == nil || *created.AssignedPublicationID != PubA ||
			created.Annotations != nil || !created.CreatedAt.Equal(at) || !created.UpdatedAt.Equal(at) ||
			created.CreatedAt.Location() != time.UTC {
			t.Fatalf("Create = %+v, %v", created, err)
		}
		got, err := s.Groups.Get(ctx, OrgA, server.Fleet.ID, created.ID)
		if err != nil || Describe(got) != Describe(created) {
			t.Fatalf("Get = %+v, %v; want %+v", got, err, created)
		}

		k8s := s.CreateFleet(t, OrgA, "edge-k8s", fleets.TypeKubernetesSupervisor)
		k := named(OrgA, k8s.Fleet.ID, "traces-sampling")
		k.AssignedPublicationID = Ptr(PubA)
		created, err = s.Groups.Create(ctx, k)
		if err != nil || created.FleetID != k8s.Fleet.ID || created.IsDefault || *created.AssignedPublicationID != PubA {
			t.Fatalf("a Kubernetes fleet's group = %+v, %v", created, err)
		}
		got, err = s.Groups.Get(ctx, OrgA, k8s.Fleet.ID, created.ID)
		if err != nil || Describe(got) != Describe(created) {
			t.Fatalf("Get = %+v, %v; want %+v", got, err, created)
		}
	})
}

// What Create refuses: a name the fleet has (the Default group's included),
// another organization's or an unknown published version, another
// organization's fleet, and what the table bounds; storing nothing.
func TestCreateRefusesWhatTheSchemaRefuses(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		server := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		k8s := s.CreateFleet(t, OrgA, "edge-k8s", fleets.TypeKubernetesSupervisor)
		theirs := s.CreateFleet(t, OrgB, "globex", fleets.TypeServer)
		if _, err := s.Groups.Create(ctx, named(OrgA, server.Fleet.ID, "gateways")); err != nil {
			t.Fatal(err)
		}
		before, beforeB := s.Dump(t, OrgA), s.Dump(t, OrgB)

		for _, c := range []struct {
			name   string
			fleet  uuid.UUID
			change func(g *collectorgroups.NewGroup)
			want   error
		}{
			{"a name the fleet has", server.Fleet.ID, func(g *collectorgroups.NewGroup) { g.Name = "gateways" }, collectorgroups.ErrNameTaken},
			{"the Default group's name", server.Fleet.ID, func(g *collectorgroups.NewGroup) { g.Name = "Default" }, collectorgroups.ErrNameTaken},
			{"another organization's published version", server.Fleet.ID, func(g *collectorgroups.NewGroup) { g.AssignedPublicationID = Ptr(PubB) }, collectorgroups.ErrPublicationNotFound},
			{"an unknown published version", server.Fleet.ID, func(g *collectorgroups.NewGroup) { g.AssignedPublicationID = Ptr(uuid.New()) }, collectorgroups.ErrPublicationNotFound},
			{"another organization's fleet", theirs.Fleet.ID, func(*collectorgroups.NewGroup) {}, collectorgroups.ErrFleetNotFound},
			{"an unknown fleet", uuid.New(), func(*collectorgroups.NewGroup) {}, collectorgroups.ErrFleetNotFound},
			{"an empty name", server.Fleet.ID, func(g *collectorgroups.NewGroup) { g.Name = "" }, collectorgroups.ErrInvalid},
			{"a name with a trailing tab", server.Fleet.ID, func(g *collectorgroups.NewGroup) { g.Name = "gw\t" }, collectorgroups.ErrInvalid},
			// No control character anywhere, as a collector's name.
			{"a name with a newline", server.Fleet.ID, func(g *collectorgroups.NewGroup) { g.Name = "gate\nways" }, collectorgroups.ErrInvalid},
			{"a name with a tab", server.Fleet.ID, func(g *collectorgroups.NewGroup) { g.Name = "gate\tways" }, collectorgroups.ErrInvalid},
			{"a name with DEL", server.Fleet.ID, func(g *collectorgroups.NewGroup) { g.Name = "gate\x7fways" }, collectorgroups.ErrInvalid},
			{"a name with a C1 character", server.Fleet.ID, func(g *collectorgroups.NewGroup) { g.Name = "gate\u0085ways" }, collectorgroups.ErrInvalid},
			{"a name of 81 characters", server.Fleet.ID, func(g *collectorgroups.NewGroup) { g.Name = strings.Repeat("g", 81) }, collectorgroups.ErrInvalid},
		} {
			g := named(OrgA, c.fleet, "new")
			c.change(&g)
			if _, err := s.Groups.Create(ctx, g); !errors.Is(err, c.want) {
				t.Errorf("%s: Create = %v, want %v", c.name, err, c.want)
			}
		}
		s.Unchanged(t, "the refused Creates", OrgA, before)
		s.Unchanged(t, "the refused Creates", OrgB, beforeB)

		if _, err := s.Groups.Create(ctx, named(OrgA, k8s.Fleet.ID, strings.Repeat("g", 80))); err != nil {
			t.Fatalf("a name at its limit: %v", err)
		}
		// A name of another fleet's is free here.
		if _, err := s.Groups.Create(ctx, named(OrgA, k8s.Fleet.ID, "gateways")); err != nil {
			t.Fatalf("another fleet's name: %v", err)
		}
	})
}

// List: the Default group first, then by name in byte order, then id; the
// organization's fleet only.
func TestListIsTheDefaultGroupFirstThenByName(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		for _, name := range []string{"b-gateways", "B-gateways", "a", "Zebra", "é"} {
			if _, err := s.Groups.Create(ctx, named(OrgA, c.Fleet.ID, name)); err != nil {
				t.Fatal(err)
			}
		}
		s.CreateFleet(t, OrgA, "edge-dev", fleets.TypeServer)
		groups, err := s.Groups.List(ctx, OrgA, c.Fleet.ID)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, g := range groups {
			names = append(names, g.Name)
		}
		if strings.Join(names, ",") != "Default,B-gateways,Zebra,a,b-gateways,é" {
			t.Fatalf("listed %v", names)
		}
		if _, err := s.Groups.List(ctx, OrgA, uuid.New()); !errors.Is(err, collectorgroups.ErrFleetNotFound) {
			t.Fatalf("an unknown fleet = %v", err)
		}
	})
}

// Update: a rename, an assignment set and cleared, on either fleet type's
// group; UpdatedAt moves only on a change; the Default group keeps its
// name; each refusal changes nothing.
func TestUpdateChangesWhatItIsGivenAndRefusesTheRest(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		server := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		k8s := s.CreateFleet(t, OrgA, "edge-k8s", fleets.TypeKubernetesSupervisor)
		g, err := s.Groups.Create(ctx, named(OrgA, server.Fleet.ID, "gateways"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Groups.Create(ctx, named(OrgA, server.Fleet.ID, "edge")); err != nil {
			t.Fatal(err)
		}
		later := Now.Add(time.Hour)

		got, err := s.Groups.Update(ctx, OrgA, server.Fleet.ID, g.ID, collectorgroups.Changes{
			Name: Ptr("gateways-eu"), AssignedPublicationID: collectorgroups.SetTo(PubA)}, later)
		if err != nil || got.Name != "gateways-eu" || *got.AssignedPublicationID != PubA ||
			!got.UpdatedAt.Equal(Millisecond(later)) || !got.CreatedAt.Equal(Millisecond(Now)) {
			t.Fatalf("rename and assign = %+v, %v", got, err)
		}
		// The same values again, and clearing what is already clear: nothing
		// changes, UpdatedAt stays.
		got, err = s.Groups.Update(ctx, OrgA, server.Fleet.ID, g.ID, collectorgroups.Changes{
			Name: Ptr("gateways-eu"), AssignedPublicationID: collectorgroups.SetTo(PubA),
			Annotations: collectorgroups.Clear[json.RawMessage]()}, later.Add(time.Hour))
		if err != nil || !got.UpdatedAt.Equal(Millisecond(later)) {
			t.Fatalf("a no-op update = %+v, %v", got, err)
		}
		got, err = s.Groups.Update(ctx, OrgA, server.Fleet.ID, g.ID, collectorgroups.Changes{
			AssignedPublicationID: collectorgroups.Clear[uuid.UUID]()}, later.Add(2*time.Hour))
		if err != nil || got.AssignedPublicationID != nil || !got.UpdatedAt.Equal(Millisecond(later.Add(2*time.Hour))) {
			t.Fatalf("clearing the assignment = %+v, %v", got, err)
		}

		// The Default group: assigned, never renamed.
		def := server.DefaultGroupID
		if got, err := s.Groups.Update(ctx, OrgA, server.Fleet.ID, def, collectorgroups.Changes{
			Name: Ptr("Default"), AssignedPublicationID: collectorgroups.SetTo(PubA)}, later); err != nil ||
			got.Name != "Default" || *got.AssignedPublicationID != PubA {
			t.Fatalf("assigning the Default group = %+v, %v", got, err)
		}

		kg, err := s.Groups.Create(ctx, named(OrgA, k8s.Fleet.ID, "traces-sampling"))
		if err != nil {
			t.Fatal(err)
		}
		got, err = s.Groups.Update(ctx, OrgA, k8s.Fleet.ID, kg.ID, collectorgroups.Changes{
			AssignedPublicationID: collectorgroups.SetTo(PubA)}, later)
		if err != nil || *got.AssignedPublicationID != PubA || !got.UpdatedAt.Equal(Millisecond(later)) {
			t.Fatalf("assigning a Kubernetes fleet's group = %+v, %v", got, err)
		}

		before := s.Dump(t, OrgA)
		for _, c := range []struct {
			name    string
			fleet   uuid.UUID
			group   uuid.UUID
			changes collectorgroups.Changes
			want    error
		}{
			{"renaming the Default group", server.Fleet.ID, def, collectorgroups.Changes{Name: Ptr("All")}, collectorgroups.ErrIsDefault},
			{"a name the fleet has", server.Fleet.ID, g.ID, collectorgroups.Changes{Name: Ptr("edge")}, collectorgroups.ErrNameTaken},
			{"the Default group's name", server.Fleet.ID, g.ID, collectorgroups.Changes{Name: Ptr("Default")}, collectorgroups.ErrNameTaken},
			{"another organization's published version", server.Fleet.ID, g.ID, collectorgroups.Changes{AssignedPublicationID: collectorgroups.SetTo(PubB)}, collectorgroups.ErrPublicationNotFound},
			{"an empty name", server.Fleet.ID, g.ID, collectorgroups.Changes{Name: Ptr("")}, collectorgroups.ErrInvalid},
			{"a name of 81 characters", k8s.Fleet.ID, kg.ID, collectorgroups.Changes{Name: Ptr(strings.Repeat("g", 81))}, collectorgroups.ErrInvalid},
			{"a group of another fleet", k8s.Fleet.ID, g.ID, collectorgroups.Changes{Name: Ptr("x")}, collectorgroups.ErrNotFound},
			{"an unknown group", server.Fleet.ID, uuid.New(), collectorgroups.Changes{Name: Ptr("x")}, collectorgroups.ErrNotFound},
		} {
			if _, err := s.Groups.Update(ctx, OrgA, c.fleet, c.group, c.changes, later.Add(3*time.Hour)); !errors.Is(err, c.want) {
				t.Errorf("%s: Update = %v, want %v", c.name, err, c.want)
			}
		}
		s.Unchanged(t, "the refused updates", OrgA, before)
	})
}

// Delete never deletes the Default group; any other's collectors are deleted
// with it, their error history too, in the same transaction; another group's
// collectors and errors stay.
func TestDeleteDeletesTheGroupsCollectorsAndNeverDeletesDefault(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		fleet := c.Fleet.ID
		gateways, err := s.Groups.Create(ctx, named(OrgA, fleet, "gateways"))
		if err != nil {
			t.Fatal(err)
		}
		edge, err := s.Groups.Create(ctx, named(OrgA, fleet, "edge"))
		if err != nil {
			t.Fatal(err)
		}
		deletedWith := []uuid.UUID{uuid.New(), uuid.New()}
		for i, id := range deletedWith {
			s.InsertCollector(t, Collector{ID: id, OrganizationID: OrgA, FleetID: fleet, GroupID: gateways.ID,
				Name: "gw-0" + string(rune('1'+i)), LastSeenAt: Now})
		}
		stays := uuid.New()
		s.InsertCollector(t, Collector{ID: stays, OrganizationID: OrgA, FleetID: fleet, GroupID: edge.ID, Name: "edge-01", LastSeenAt: Now})
		for _, id := range []uuid.UUID{deletedWith[0], stays} {
			if err := s.Collectors.RecordError(ctx, OrgA, fleet, id, collectors.ErrorReport{Message: "failed", At: Now}); err != nil {
				t.Fatal(err)
			}
		}
		errorRows := func() []string {
			var rows []string
			for _, line := range strings.Split(s.Dump(t, OrgA), "\n") {
				if strings.HasPrefix(line, "collector_errors ") {
					rows = append(rows, line)
				}
			}
			return rows
		}
		if rows := errorRows(); len(rows) != 2 {
			t.Fatalf("the error rows before = %v", rows)
		}

		before := s.Dump(t, OrgA)
		if _, err := s.Groups.Delete(ctx, OrgA, fleet, c.DefaultGroupID); !errors.Is(err, collectorgroups.ErrIsDefault) {
			t.Fatalf("deleting the Default group = %v", err)
		}
		s.Unchanged(t, "the Default group's refused delete", OrgA, before)

		if deleted, err := s.Groups.Delete(ctx, OrgA, fleet, gateways.ID); err != nil || !deleted {
			t.Fatalf("Delete = %v, %v", deleted, err)
		}
		for _, id := range deletedWith {
			if got, err := s.Collectors.Get(ctx, OrgA, fleet, id); !errors.Is(err, collectors.ErrNotFound) {
				t.Fatalf("collector %s after its group's delete = %+v, %v; want it deleted", id, got.Collector, err)
			}
		}
		if rows := errorRows(); len(rows) != 1 || !strings.Contains(rows[0], stays.String()) {
			t.Fatalf("the error rows after = %v; want only the other group's collector's", rows)
		}
		if group, updatedAt := s.CollectorGroup(t, OrgA, stays); group != edge.ID || !updatedAt.Equal(Millisecond(Now)) {
			t.Fatalf("another group's collector changed: %s, %s", group, updatedAt)
		}
		if _, err := s.Groups.Get(ctx, OrgA, fleet, gateways.ID); !errors.Is(err, collectorgroups.ErrNotFound) {
			t.Fatalf("Get after the delete = %v", err)
		}
		if deleted, err := s.Groups.Delete(ctx, OrgA, fleet, gateways.ID); err != nil || deleted {
			t.Fatalf("a second Delete = %v, %v", deleted, err)
		}
		// The Default group is still there, and still the only one.
		groups, err := s.Groups.List(ctx, OrgA, fleet)
		if err != nil || len(groups) != 2 || !groups[0].IsDefault || groups[0].ID != c.DefaultGroupID {
			t.Fatalf("groups after = %+v, %v", groups, err)
		}
	})
}

// Every method, called with another organization's fleet or group (or a
// group under another fleet), answers not found and changes nothing of
// either organization's.
func TestAnotherOrganizationsGroupIsNotFoundAndUnchanged(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		mine := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		theirs := s.CreateFleet(t, OrgB, "globex", fleets.TypeKubernetesSupervisor)
		theirGroup := named(OrgB, theirs.Fleet.ID, "gateways")
		theirGroup.Annotations = json.RawMessage(`{"telflo":{"dockerMounts":[]}}`)
		their, err := s.Groups.Create(ctx, theirGroup)
		if err != nil {
			t.Fatal(err)
		}
		s.InsertCollector(t, Collector{ID: uuid.New(), OrganizationID: OrgB, FleetID: theirs.Fleet.ID, GroupID: their.ID,
			Name: "globex-01", LastSeenAt: Now})
		beforeA, beforeB := s.Dump(t, OrgA), s.Dump(t, OrgB)

		for _, fleet := range []uuid.UUID{theirs.Fleet.ID, mine.Fleet.ID} {
			for _, group := range []uuid.UUID{their.ID, theirs.DefaultGroupID} {
				if _, err := s.Groups.Get(ctx, OrgA, fleet, group); !errors.Is(err, collectorgroups.ErrNotFound) {
					t.Errorf("Get = %v", err)
				}
				if _, err := s.Groups.Update(ctx, OrgA, fleet, group, collectorgroups.Changes{Name: Ptr("mine"),
					AssignedPublicationID: collectorgroups.SetTo(PubA)}, Now.Add(time.Hour)); !errors.Is(err, collectorgroups.ErrNotFound) {
					t.Errorf("Update = %v", err)
				}
				// Nor their annotations, set or cleared.
				for _, annotations := range []collectorgroups.Change[json.RawMessage]{
					collectorgroups.SetTo(json.RawMessage(`{"mine":true}`)), collectorgroups.Clear[json.RawMessage](),
				} {
					if _, err := s.Groups.Update(ctx, OrgA, fleet, group, collectorgroups.Changes{Annotations: annotations},
						Now.Add(time.Hour)); !errors.Is(err, collectorgroups.ErrNotFound) {
						t.Errorf("Update of the annotations = %v", err)
					}
				}
				if deleted, err := s.Groups.Delete(ctx, OrgA, fleet, group); err != nil || deleted {
					t.Errorf("Delete = %v, %v", deleted, err)
				}
			}
		}
		if _, err := s.Groups.List(ctx, OrgA, theirs.Fleet.ID); !errors.Is(err, collectorgroups.ErrFleetNotFound) {
			t.Errorf("List = %v", err)
		}
		if _, err := s.Groups.Create(ctx, named(OrgA, theirs.Fleet.ID, "mine")); !errors.Is(err, collectorgroups.ErrFleetNotFound) {
			t.Errorf("Create in their fleet = %v", err)
		}
		// B can't be assigned A's published version either.
		if _, err := s.Groups.Update(ctx, OrgB, theirs.Fleet.ID, their.ID, collectorgroups.Changes{
			AssignedPublicationID: collectorgroups.SetTo(PubA)}, Now.Add(time.Hour)); !errors.Is(err, collectorgroups.ErrPublicationNotFound) {
			t.Errorf("B assigned A's published version: %v", err)
		}
		s.Unchanged(t, "A's calls", OrgA, beforeA)
		s.Unchanged(t, "A's calls", OrgB, beforeB)
	})
}

// Within one organization, a group of another fleet is not found through
// this fleet, and nothing changes: Get, Update and Delete name the fleet in
// their filter, not only the organization.
func TestAnotherFleetsGroupIsNotFoundAndUnchanged(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		mine := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		other := s.CreateFleet(t, OrgA, "edge-dev", fleets.TypeServer)
		theirs, err := s.Groups.Create(ctx, named(OrgA, other.Fleet.ID, "gateways"))
		if err != nil {
			t.Fatal(err)
		}
		s.InsertCollector(t, Collector{ID: uuid.New(), OrganizationID: OrgA, FleetID: other.Fleet.ID, GroupID: theirs.ID,
			Name: "dev-01", LastSeenAt: Now})
		before := s.Dump(t, OrgA)
		for _, id := range []uuid.UUID{theirs.ID, other.DefaultGroupID} {
			if _, err := s.Groups.Get(ctx, OrgA, mine.Fleet.ID, id); !errors.Is(err, collectorgroups.ErrNotFound) {
				t.Errorf("Get through another fleet = %v, want ErrNotFound", err)
			}
			if _, err := s.Groups.Update(ctx, OrgA, mine.Fleet.ID, id, collectorgroups.Changes{Name: Ptr("renamed"),
				AssignedPublicationID: collectorgroups.SetTo(PubA)}, Now.Add(time.Hour)); !errors.Is(err, collectorgroups.ErrNotFound) {
				t.Errorf("Update through another fleet = %v, want ErrNotFound", err)
			}
			if deleted, err := s.Groups.Delete(ctx, OrgA, mine.Fleet.ID, id); deleted || err != nil {
				t.Errorf("Delete through another fleet = %v, %v; want false, nil", deleted, err)
			}
		}
		s.Unchanged(t, "calls naming another fleet", OrgA, before)
	})
}

// The trimmed-name rule, pinned at Unicode's edges: a name with an
// ideographic space (U+3000) or a no-break space (U+00A0) at either end is
// refused by Create and Update.
func TestANameWithUnicodeEdgeWhitespaceIsRefused(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		g, err := s.Groups.Create(ctx, named(OrgA, c.Fleet.ID, "gateways"))
		if err != nil {
			t.Fatal(err)
		}
		before := s.Dump(t, OrgA)
		for _, name := range []string{"\u3000gateways", "gateways\u3000", "\u00a0gateways", "gateways\u00a0"} {
			if _, err := s.Groups.Create(ctx, named(OrgA, c.Fleet.ID, name)); !errors.Is(err, collectorgroups.ErrInvalid) {
				t.Errorf("Create %q = %v, want ErrInvalid", name, err)
			}
			if _, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, g.ID, collectorgroups.Changes{Name: Ptr(name)}, Now); !errors.Is(err, collectorgroups.ErrInvalid) {
				t.Errorf("Update to %q = %v, want ErrInvalid", name, err)
			}
		}
		s.Unchanged(t, "the refused names", OrgA, before)
		// Inside a name, either space is a character like any other.
		if _, err := s.Groups.Create(ctx, named(OrgA, c.Fleet.ID, "edge\u3000gate\u00a0ways")); err != nil {
			t.Fatalf("spaces inside a name: %v", err)
		}
	})
}

// A group's Delete racing its fleet's Delete. A group's Delete that locked
// the group first, then waited (here, on a collector a third session holds)
// before deleting its collectors, while the fleet's Delete held the fleet
// and cascaded into the groups, would deadlock (40P01). Fleet first, both
// finish: the group's Delete, there first, deletes the group, and the
// fleet's then takes the rest.
func TestAGroupsDeleteRacingItsFleetsDeleteDoesNotDeadlock(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		g, err := s.Groups.Create(ctx, named(OrgA, c.Fleet.ID, "gateways"))
		if err != nil {
			t.Fatal(err)
		}
		collector := uuid.New()
		s.InsertCollector(t, Collector{ID: collector, OrganizationID: OrgA, FleetID: c.Fleet.ID, GroupID: g.ID,
			Name: "edge-01", LastSeenAt: Now})

		blocker, err := s.Databases.Owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = blocker.Rollback(ctx) }()
		if _, err := blocker.Exec(ctx, `select 1 from opamp_server.collector_instances
			where organization_id = $1 and id = $2 for update`, OrgA, collector); err != nil {
			t.Fatal(err)
		}
		release := func() {
			s.WaitForLockWaiters(t, 2)
			if err := blocker.Commit(ctx); err != nil {
				t.Error(err)
			}
		}
		var groupDeleted, fleetDeleted bool
		var groupErr, fleetErr error
		groupDone, fleetDone := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(groupDone)
			groupDeleted, groupErr = s.Groups.Delete(ctx, OrgA, c.Fleet.ID, g.ID)
		}()
		s.WaitForLockWaiters(t, 1)
		go func() {
			defer close(fleetDone)
			fleetDeleted, fleetErr = s.Fleets.Delete(ctx, OrgA, c.Fleet.ID)
		}()
		release()
		<-groupDone
		<-fleetDone
		if groupErr != nil || fleetErr != nil {
			t.Fatalf("the group's Delete = %v, %v; the fleet's = %v, %v; want both to finish", groupDeleted, groupErr, fleetDeleted, fleetErr)
		}
		if !groupDeleted || !fleetDeleted {
			t.Fatalf("the group's Delete = %v, the fleet's = %v; want both (the group's first)", groupDeleted, fleetDeleted)
		}
		for table, n := range s.FleetRows(t, c.Fleet.ID) {
			if n != 0 {
				t.Errorf("%d rows of %s left", n, table)
			}
		}
	})
}

// An Update that doesn't rename takes the group's row `for no key update`,
// so it doesn't wait for an enrollment holding a key-share lock on the group
// (a collector inserted into it, not yet committed); a rename, which changes
// a key, takes `for update` and waits.
func TestAnUpdateWithoutARenameDoesNotWaitForAnEnrollment(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		g, err := s.Groups.Create(ctx, named(OrgA, c.Fleet.ID, "gateways"))
		if err != nil {
			t.Fatal(err)
		}
		enrollment, err := s.Databases.Runtime.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = enrollment.Rollback(ctx) }()
		if _, err := enrollment.Exec(ctx, `
			insert into opamp_server.collector_instances (id, organization_id, fleet_id, collector_group_id,
			  instance_uid, name, last_seen_at, created_at, updated_at)
			values ($1, $2, $3, $4, $5, 'edge-01', $6, $6, $6)`,
			uuid.New(), OrgA, c.Fleet.ID, g.ID, uuid.New(), Millisecond(Now)); err != nil {
			t.Fatal(err)
		}

		// Done, or waiting on a lock: whichever comes first.
		finishes := func(update func() error) bool {
			done := make(chan error, 1)
			go func() { done <- update() }()
			for {
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
					return true
				default:
				}
				var waiting int
				if err := s.Databases.Admin.QueryRow(ctx, `select count(*) from pg_stat_activity
					where datname = $1 and wait_event_type = 'Lock'`, s.Databases.Name).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting > 0 {
					return false
				}
			}
		}
		if !finishes(func() error {
			_, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, g.ID,
				collectorgroups.Changes{AssignedPublicationID: collectorgroups.SetTo(PubA)}, Now.Add(time.Hour))
			return err
		}) {
			t.Fatal("an assignment waited for an enrollment's key-share lock on the group")
		}
		renamed := make(chan error, 1)
		if finishes(func() error {
			_, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, g.ID, collectorgroups.Changes{Name: Ptr("edges")}, Now.Add(time.Hour))
			renamed <- err
			return err
		}) {
			t.Fatal("a rename didn't wait for the enrollment: it should take the row for update")
		}
		if err := enrollment.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-renamed; err != nil {
			t.Fatalf("the rename after the enrollment: %v", err)
		}
	})
}

// Annotations on any fleet's group (a server fleet's and a
// kubernetes-supervisor fleet's), created, replaced whole, cleared, and
// answered unchanged in canonical form (keys sorted at every level, nested
// values and numbers as written); UpdatedAt moves only on a change.
func TestAnnotationsAreStoredAndAnsweredUnchangedOnAnyFleetsGroup(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		for _, fleetType := range []fleets.Type{fleets.TypeServer, fleets.TypeKubernetesSupervisor} {
			c := s.CreateFleet(t, OrgA, "edge-"+string(fleetType), fleetType)
			g := named(OrgA, c.Fleet.ID, "gateways")
			g.Annotations = json.RawMessage(`{"telflo": {"dockerMounts": [{"readOnly": true, "hostPath": "/var/log/nginx", "containerPath": "/var/log/nginx"}]}, "acme.io/owner": "ops", "n": 1.50}`)
			created, err := s.Groups.Create(ctx, g)
			const want = `{"acme.io/owner":"ops","n":1.50,"telflo":{"dockerMounts":[{"containerPath":"/var/log/nginx","hostPath":"/var/log/nginx","readOnly":true}]}}`
			if err != nil || string(created.Annotations) != want {
				t.Fatalf("%s: Create = %s, %v; want %s", fleetType, created.Annotations, err, want)
			}
			if got, err := s.Groups.Get(ctx, OrgA, c.Fleet.ID, created.ID); err != nil || string(got.Annotations) != want {
				t.Fatalf("%s: Get = %s, %v", fleetType, got.Annotations, err)
			}
			same, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, created.ID,
				collectorgroups.Changes{Annotations: collectorgroups.SetTo(json.RawMessage(want))}, Now.Add(time.Hour))
			if err != nil || !same.UpdatedAt.Equal(created.UpdatedAt) {
				t.Fatalf("%s: the same annotations = %+v, %v; want no change", fleetType, same, err)
			}
			replaced, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, created.ID,
				collectorgroups.Changes{Annotations: collectorgroups.SetTo(json.RawMessage(`{"other": []}`))}, Now.Add(time.Hour))
			if err != nil || string(replaced.Annotations) != `{"other":[]}` || !replaced.UpdatedAt.Equal(Millisecond(Now.Add(time.Hour))) {
				t.Fatalf("%s: replaced = %s, %v", fleetType, replaced.Annotations, err)
			}
			// A change without annotations leaves them.
			renamed, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, created.ID,
				collectorgroups.Changes{Name: Ptr("edges")}, Now.Add(90*time.Minute))
			if err != nil || string(renamed.Annotations) != `{"other":[]}` {
				t.Fatalf("%s: renamed = %s, %v; want the annotations kept", fleetType, renamed.Annotations, err)
			}
			cleared, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, created.ID,
				collectorgroups.Changes{Annotations: collectorgroups.Clear[json.RawMessage]()}, Now.Add(2*time.Hour))
			if err != nil || cleared.Annotations != nil {
				t.Fatalf("%s: cleared = %s, %v", fleetType, cleared.Annotations, err)
			}
			if got, err := s.Groups.Get(ctx, OrgA, c.Fleet.ID, created.ID); err != nil || got.Annotations != nil {
				t.Fatalf("%s: Get after the clear = %s, %v", fleetType, got.Annotations, err)
			}
			// The Default group takes them too.
			groups, err := s.Groups.List(ctx, OrgA, c.Fleet.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, groups[0].ID,
				collectorgroups.Changes{Annotations: collectorgroups.SetTo(json.RawMessage(`{}`))}, Now); err != nil || string(got.Annotations) != `{}` {
				t.Fatalf("%s: the Default group's = %s, %v", fleetType, got.Annotations, err)
			}
		}
	})
}

// Annotations that aren't a JSON object, or are over the bound, or hold
// what jsonb would rewrite, are refused and change nothing.
func TestAnnotationsRefusedChangeNothing(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		before := s.Dump(t, OrgA)
		for _, annotations := range []string{
			`[]`, `"telflo"`, `1`, `null`, `{`, `{"a":1}{}`, `{"a":1}}`, `{"a":1}]`, `{"a":1} 2`, `{"a":1e2}`, `{"a":-0}`, `{"a\u0000":1}`,
			`{"a":"` + strings.Repeat("v", collectorgroups.MaxAnnotationsBytes) + `"}`,
		} {
			g := named(OrgA, c.Fleet.ID, "gateways")
			g.Annotations = json.RawMessage(annotations)
			if _, err := s.Groups.Create(ctx, g); !errors.Is(err, collectorgroups.ErrInvalid) {
				t.Errorf("annotations %.20s: Create = %v, want ErrInvalid", annotations, err)
			}
			if _, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, c.DefaultGroupID,
				collectorgroups.Changes{Annotations: collectorgroups.SetTo(json.RawMessage(annotations))}, Now); !errors.Is(err, collectorgroups.ErrInvalid) {
				t.Errorf("annotations %.20s: Update = %v, want ErrInvalid", annotations, err)
			}
		}
		s.Unchanged(t, "the refused annotations", OrgA, before)
	})
}

// U+2028 and U+2029 are six bytes in the canonical form (Go escapes them)
// and three in jsonb's text (Postgres writes them as they are), so for such
// an object the canonical bound is the one that holds: at it, the store
// keeps it (jsonb's count is well under); a byte over, the store refuses it,
// though Postgres' own check would take it. The platform's contract checks
// the same pair (annotationsTextBytes).
func TestAnnotationsWithLineSeparatorsAtTheCanonicalBoundAreAlikeInBothStores(t *testing.T) {
	// {"z":"…"}: 8 bytes, 1,000 U+2028 and U+2029 escaped (6 each), then padding.
	separators := strings.Repeat(`\u2028\u2029`, 500)
	withCanonical := func(length int) string {
		return `{"z":"` + separators + strings.Repeat("v", length-8-6000) + `"}`
	}
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		before := s.Dump(t, OrgA)
		over := withCanonical(collectorgroups.MaxAnnotationsBytes + 1)
		if _, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, c.DefaultGroupID,
			collectorgroups.Changes{Annotations: collectorgroups.SetTo(json.RawMessage(over))}, Now); !errors.Is(err, collectorgroups.ErrInvalidAnnotations) {
			t.Fatalf("a byte over the canonical bound: Update = %v, want ErrInvalidAnnotations", err)
		}
		s.Unchanged(t, "the refused annotations", OrgA, before)
		at := withCanonical(collectorgroups.MaxAnnotationsBytes)
		updated, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, c.DefaultGroupID,
			collectorgroups.Changes{Annotations: collectorgroups.SetTo(json.RawMessage(at))}, Now)
		if err != nil || string(updated.Annotations) != at {
			t.Fatalf("at the canonical bound: Update = %.40s, %v; want it stored", updated.Annotations, err)
		}
		if got, err := s.Groups.Get(ctx, OrgA, c.Fleet.ID, c.DefaultGroupID); err != nil || string(got.Annotations) != at {
			t.Fatalf("at the canonical bound: Get = %.40s, %v", got.Annotations, err)
		}
		var jsonbBytes int
		if err := s.Databases.Owner.QueryRow(ctx, `select octet_length(annotations::text) from opamp_server.collector_groups
			where organization_id = $1 and id = $2`, OrgA, c.DefaultGroupID).Scan(&jsonbBytes); err != nil || jsonbBytes != 8+1+3000+(collectorgroups.MaxAnnotationsBytes-8-6000) {
			t.Fatalf("Postgres counts %d, %v; want jsonb's text of the raw characters", jsonbBytes, err)
		}
	})
}

// A stored row reads back whatever the write bound says: a row the table's
// check admitted (here 3,000 U+2028, 9,009 bytes as jsonb text, 18,008 in
// the canonical form a write bounds) is listed and read, never a failure for
// the fleet's every group.
func TestAStoredRowTheTableAdmittedReadsBack(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		stored := `{"z": "` + strings.Repeat("\u2028", 3000) + `"}`
		if _, err := s.Databases.Owner.Exec(ctx, `update opamp_server.collector_groups set annotations = $3::jsonb
			where organization_id = $1 and id = $2`, OrgA, c.DefaultGroupID, stored); err != nil {
			t.Fatalf("the table refused the row: %v", err)
		}
		want := `{"z":"` + strings.Repeat(`\u2028`, 3000) + `"}`
		if _, err := collectorgroups.CanonicalAnnotations(json.RawMessage(want)); !errors.Is(err, collectorgroups.ErrInvalidAnnotations) {
			t.Fatalf("the fixture must be one a write refuses: %v", err)
		}
		got, err := s.Groups.Get(ctx, OrgA, c.Fleet.ID, c.DefaultGroupID)
		if err != nil || string(got.Annotations) != want {
			t.Fatalf("Get = %.40s, %v; want the stored row", got.Annotations, err)
		}
		listed, err := s.Groups.List(ctx, OrgA, c.Fleet.ID)
		if err != nil || len(listed) != 1 || string(listed[0].Annotations) != want {
			t.Fatalf("List = %d groups, %v", len(listed), err)
		}
	})
}

// The annotations' bound is the table check's own, jsonb's text (a space
// after every comma and colon outside a string), so the store keeps an
// object at the bound and refuses one a byte over it, as
// ErrInvalidAnnotations, never a failure: comma-heavy and colon-heavy
// objects, whose canonical form is far shorter than their jsonb text.
func TestAnnotationsAtTheJSONBBoundAreAlikeInBothStores(t *testing.T) {
	// head, then a string of padding, then `"}`: jsonb text exactly `length`.
	atLength := func(t *testing.T, head string, length int) string {
		t.Helper()
		separators := strings.Count(head, ",") + strings.Count(head, ":")
		padding := length - len(head) - len(`"}`) - separators
		if padding < 0 {
			t.Fatalf("the head alone is over %d bytes", length)
		}
		return head + strings.Repeat("v", padding) + `"}`
	}
	zeros := make([]string, 4000)
	for i := range zeros {
		zeros[i] = "0"
	}
	keys := make([]string, 1200)
	for i := range keys {
		keys[i] = fmt.Sprintf(`"k%04d":0`, i)
	}
	heads := map[string]string{
		"comma-heavy": `{"a":[` + strings.Join(zeros, ",") + `],"z":"`,
		"colon-heavy": `{` + strings.Join(keys, ",") + `,"z":"`,
	}
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		for shape, head := range heads {
			over := atLength(t, head, collectorgroups.MaxAnnotationsBytes+1)
			before := s.Dump(t, OrgA)
			g := named(OrgA, c.Fleet.ID, "gateways")
			g.Annotations = json.RawMessage(over)
			if _, err := s.Groups.Create(ctx, g); !errors.Is(err, collectorgroups.ErrInvalidAnnotations) {
				t.Fatalf("%s, a byte over: Create = %v, want ErrInvalidAnnotations", shape, err)
			}
			if _, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, c.DefaultGroupID,
				collectorgroups.Changes{Annotations: collectorgroups.SetTo(json.RawMessage(over))}, Now); !errors.Is(err, collectorgroups.ErrInvalidAnnotations) {
				t.Fatalf("%s, a byte over: Update = %v, want ErrInvalidAnnotations", shape, err)
			}
			s.Unchanged(t, shape+": the refused annotations", OrgA, before)

			at := atLength(t, head, collectorgroups.MaxAnnotationsBytes)
			if len(at) >= collectorgroups.MaxAnnotationsBytes-1000 {
				t.Fatalf("%s: the fixture's canonical form is %d bytes, too near the bound to tell the two apart", shape, len(at))
			}
			updated, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, c.DefaultGroupID,
				collectorgroups.Changes{Annotations: collectorgroups.SetTo(json.RawMessage(at))}, Now)
			if err != nil || string(updated.Annotations) != at {
				t.Fatalf("%s, at the bound: Update = %.40s, %v; want it stored", shape, updated.Annotations, err)
			}
		}

		// Comma-heavy with U+2028 too: 1,501 commas and 2 colons (a space
		// after each in jsonb's text) and 100 U+2028 (six bytes each
		// escaped, as the canonical form writes them, three as jsonb does),
		// so the canonical form is 1,203 bytes shorter than the jsonb text
		// and the jsonb bound decides. Each value is padded to a jsonb text
		// of exactly `length`, counted here by hand; jsonbTextBytes' count
		// and Postgres' octet_length must agree with it.
		u2028Head := `{"a":[` + strings.Join(zeros[:1501], ",") + `],"z":"`
		u2028At := func(length int) string {
			separators := strings.Count(u2028Head, ",") + strings.Count(u2028Head, ":")
			// Each U+2028 counts as the character itself, as jsonb writes it.
			padding := length - len(u2028Head) - separators - 100*len("\u2028") - len(`"}`)
			return u2028Head + strings.Repeat(`\u2028`, 100) + strings.Repeat("v", padding) + `"}`
		}
		for _, length := range []int{collectorgroups.MaxAnnotationsBytes - 1, collectorgroups.MaxAnnotationsBytes, collectorgroups.MaxAnnotationsBytes + 1} {
			value := u2028At(length)
			if len(value) > collectorgroups.MaxAnnotationsBytes-1000 {
				t.Fatalf("U+2028 at %d: the canonical form is %d bytes, too near the bound to tell the two apart", length, len(value))
			}
			if got := collectorgroups.JSONBTextBytes([]byte(value)); got != length {
				t.Fatalf("U+2028 at %d: jsonbTextBytes counts %d", length, got)
			}
			var octets int
			if err := s.Databases.Owner.QueryRow(ctx, `select octet_length($1::jsonb::text)`, value).Scan(&octets); err != nil || octets != length {
				t.Fatalf("U+2028 at %d: Postgres counts %d, %v", length, octets, err)
			}
			if length > collectorgroups.MaxAnnotationsBytes {
				before := s.Dump(t, OrgA)
				g := named(OrgA, c.Fleet.ID, "gateways")
				g.Annotations = json.RawMessage(value)
				if _, err := s.Groups.Create(ctx, g); !errors.Is(err, collectorgroups.ErrInvalidAnnotations) {
					t.Fatalf("U+2028, a byte over: Create = %v, want ErrInvalidAnnotations", err)
				}
				if _, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, c.DefaultGroupID,
					collectorgroups.Changes{Annotations: collectorgroups.SetTo(json.RawMessage(value))}, Now); !errors.Is(err, collectorgroups.ErrInvalidAnnotations) {
					t.Fatalf("U+2028, a byte over: Update = %v, want ErrInvalidAnnotations", err)
				}
				s.Unchanged(t, "U+2028: the refused annotations", OrgA, before)
				continue
			}
			updated, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, c.DefaultGroupID,
				collectorgroups.Changes{Annotations: collectorgroups.SetTo(json.RawMessage(value))}, Now)
			if err != nil || string(updated.Annotations) != value {
				t.Fatalf("U+2028 at %d: Update = %.40s, %v; want it stored", length, updated.Annotations, err)
			}
			if got, err := s.Groups.Get(ctx, OrgA, c.Fleet.ID, c.DefaultGroupID); err != nil || string(got.Annotations) != value {
				t.Fatalf("U+2028 at %d: Get = %.40s, %v", length, got.Annotations, err)
			}
			var stored int
			if err := s.Databases.Owner.QueryRow(ctx, `select octet_length(annotations::text) from opamp_server.collector_groups
				where organization_id = $1 and id = $2`, OrgA, c.DefaultGroupID).Scan(&stored); err != nil || stored != length {
				t.Fatalf("U+2028 at %d: the stored row's jsonb text is %d, %v", length, stored, err)
			}
		}
	})
}

// Production wiring makes the store with no option (management's serve), so
// its Delete runs again after a 40P01 DeadlockRetries times by default: the
// retry test passes its retries explicitly, so it can't see the default.
func TestNewPostgresRetriesADeadlockByDefault(t *testing.T) {
	if got := collectorgroups.DeadlockRetriesOf(collectorgroups.NewPostgres(nil)); got != collectorgroups.DeadlockRetries || got < 1 {
		t.Fatalf("NewPostgres's Delete runs again %d times after a 40P01, want DeadlockRetries (%d)", got, collectorgroups.DeadlockRetries)
	}
	if got := collectorgroups.DeadlockRetriesOf(collectorgroups.NewPostgres(nil, collectorgroups.WithDeadlockRetries(0))); got != 0 {
		t.Fatalf("WithDeadlockRetries(0) leaves %d retries, want 0", got)
	}
}

// The variables' helpers: a published version of yaml (the store writes the
// names it references), a variable, a variable group, and the organization's
// counter.

func publishYAML(t *testing.T, s *Stores, org uuid.UUID, yaml string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if result, _, err := s.Publications.Put(ctx, publications.Publication{
		ID: id, OrganizationID: org, ConfigurationID: uuid.New(), ConfigurationName: "edge", VersionID: uuid.New(),
		VersionNumber: 1, YAML: yaml, SHA256: strings.Repeat("cd", 32), PublishedAt: Now, ReceivedAt: Now,
		Collector: publications.Collector{Distribution: "otelcol-contrib", Version: "0.162.0"},
	}); err != nil || result != publications.Created {
		t.Fatalf("publishing: %v, %v", result, err)
	}
	return id
}

func defineVariable(t *testing.T, s *Stores, org uuid.UUID, name string) variables.Variable {
	t.Helper()
	result, v, err := s.Variables.CreateVariable(ctx, variables.NewVariable{ID: uuid.New(), OrganizationID: org,
		Name: name, Value: "v", At: Now})
	if err != nil || result != variables.Created {
		t.Fatalf("creating variable %s: %v, %v", name, result, err)
	}
	return v
}

func variableGroup(t *testing.T, s *Stores, org uuid.UUID, name string) uuid.UUID {
	t.Helper()
	result, g, err := s.Variables.CreateGroup(ctx, variables.NewGroup{ID: uuid.New(), OrganizationID: org, Name: name, At: Now})
	if err != nil || result != variables.Created {
		t.Fatalf("creating variable group %s: %v, %v", name, result, err)
	}
	return g.ID
}

// revision is the organization's counter, variables_revision.
func revision(t *testing.T, s *Stores, org uuid.UUID) int64 {
	t.Helper()
	var r int64
	if err := s.Databases.Owner.QueryRow(ctx, `select variables_revision from opamp_server.organizations where id = $1`,
		org).Scan(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

// A group's variable group is set (also at its create), changed and cleared,
// each moving the organization's counter on once and stamping the group's
// setting with the new value; the same variable group again changes nothing
// and stamps nothing; a clear keeps the stamp. Get answers what the write
// answered.
func TestAGroupsVariableGroupIsSetChangedAndClearedWithAStamp(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		eu, us := variableGroup(t, s, OrgA, "eu"), variableGroup(t, s, OrgA, "us")
		check := func(label string, g collectorgroups.Group, err error, wantGroup *uuid.UUID, wantStamp *int64) {
			t.Helper()
			if err != nil || !sameUUID(g.VariableGroupID, wantGroup) || !sameStamp(g.VariableGroupStamp, wantStamp) {
				t.Fatalf("%s = %+v (variable group %v, stamp %v), %v; want %v, %v", label, g, deref(g.VariableGroupID),
					derefStamp(g.VariableGroupStamp), err, deref(wantGroup), derefStamp(wantStamp))
			}
			got, err := s.Groups.Get(ctx, OrgA, c.Fleet.ID, g.ID)
			if err != nil || !reflect.DeepEqual(got, g) {
				t.Fatalf("%s: Get = %+v, %v; want %+v", label, got, err, g)
			}
		}

		start := revision(t, s, OrgA)
		created, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID, Name: "gateways",
			VariableGroupID: &eu, At: Now})
		check("a create naming a variable group", created, err, &eu, Ptr(start+1))
		plain, err := s.Groups.Create(ctx, named(OrgA, c.Fleet.ID, "plain"))
		check("a create naming none", plain, err, nil, nil)
		if got := revision(t, s, OrgA); got != start+1 {
			t.Fatalf("the counter after a create without a variable group = %d, want %d", got, start+1)
		}

		later := Now.Add(time.Hour)
		set := func(label string, id uuid.UUID, change collectorgroups.Change[uuid.UUID], wantGroup *uuid.UUID, wantStamp *int64) collectorgroups.Group {
			t.Helper()
			g, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, id, collectorgroups.Changes{VariableGroupID: change}, later)
			check(label, g, err, wantGroup, wantStamp)
			return g
		}
		set("set", plain.ID, collectorgroups.SetTo(us), &us, Ptr(start+2))
		set("changed", created.ID, collectorgroups.SetTo(us), &us, Ptr(start+3))
		same := set("the same again", created.ID, collectorgroups.SetTo(us), &us, Ptr(start+3))
		if !same.UpdatedAt.Equal(Millisecond(later)) {
			t.Fatalf("UpdatedAt = %v", same.UpdatedAt)
		}
		cleared := set("cleared", created.ID, collectorgroups.Clear[uuid.UUID](), nil, Ptr(start+4))
		set("cleared again", created.ID, collectorgroups.Clear[uuid.UUID](), nil, Ptr(start+4))
		if got := revision(t, s, OrgA); got != start+4 {
			t.Fatalf("the counter = %d, want %d: one move per change, none for an unchanged one", got, start+4)
		}
		if !cleared.UpdatedAt.Equal(Millisecond(later)) {
			t.Fatalf("the clear's UpdatedAt = %v", cleared.UpdatedAt)
		}
		// Listed as Get answers it.
		list, err := s.Groups.List(ctx, OrgA, c.Fleet.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range list {
			if got, _ := s.Groups.Get(ctx, OrgA, c.Fleet.ID, g.ID); !reflect.DeepEqual(got, g) {
				t.Fatalf("List answered %+v, Get %+v", g, got)
			}
		}
	})
}

func sameUUID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameStamp(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func deref(p *uuid.UUID) string {
	if p == nil {
		return "none"
	}
	return p.String()
}

func derefStamp(p *int64) string {
	if p == nil {
		return "none"
	}
	return fmt.Sprint(*p)
}

// A group's variable group is its organization's own: another
// organization's, or an unknown one, is ErrVariableGroupNotFound, on a
// create and an update, and changes nothing (the counter included); B's
// group can't use A's either.
func TestAGroupsVariableGroupIsTheOrganizationsOwn(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		theirs := s.CreateFleet(t, OrgB, "globex", fleets.TypeServer)
		mine, theirVariableGroup := variableGroup(t, s, OrgA, "eu"), variableGroup(t, s, OrgB, "eu")
		g, err := s.Groups.Create(ctx, named(OrgA, c.Fleet.ID, "gateways"))
		if err != nil {
			t.Fatal(err)
		}
		beforeA, beforeB := s.Dump(t, OrgA), s.Dump(t, OrgB)
		for label, id := range map[string]uuid.UUID{"another organization's": theirVariableGroup, "an unknown": uuid.New()} {
			create := named(OrgA, c.Fleet.ID, "new")
			create.VariableGroupID = &id
			if _, err := s.Groups.Create(ctx, create); !errors.Is(err, collectorgroups.ErrVariableGroupNotFound) {
				t.Errorf("%s variable group: Create = %v", label, err)
			}
			if _, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, g.ID, collectorgroups.Changes{Name: Ptr("renamed"),
				VariableGroupID: collectorgroups.SetTo(id)}, Now.Add(time.Hour)); !errors.Is(err, collectorgroups.ErrVariableGroupNotFound) {
				t.Errorf("%s variable group: Update = %v", label, err)
			}
		}
		if _, err := s.Groups.Update(ctx, OrgB, theirs.Fleet.ID, theirs.DefaultGroupID,
			collectorgroups.Changes{VariableGroupID: collectorgroups.SetTo(mine)}, Now.Add(time.Hour)); !errors.Is(err, collectorgroups.ErrVariableGroupNotFound) {
			t.Errorf("B's group given A's variable group: %v", err)
		}
		s.Unchanged(t, "the refused bindings", OrgA, beforeA)
		s.Unchanged(t, "the refused bindings", OrgB, beforeB)
		if _, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, g.ID, collectorgroups.Changes{
			VariableGroupID: collectorgroups.SetTo(mine)}, Now.Add(time.Hour)); err != nil {
			t.Fatalf("its own: %v", err)
		}
	})
}

// A rename, the annotations and an assignment's change move no counter and
// stamp nothing: only the variable-group setting is stamped on a group.
func TestARenameOrAnAnnotationsChangeStampsNothing(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		eu := variableGroup(t, s, OrgA, "eu")
		g, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID, Name: "gateways",
			VariableGroupID: &eu, At: Now})
		if err != nil {
			t.Fatal(err)
		}
		start := revision(t, s, OrgA)
		for label, changes := range map[string]collectorgroups.Changes{
			"a rename":          {Name: Ptr("edges")},
			"annotations":       {Annotations: collectorgroups.SetTo(json.RawMessage(`{"a":1}`))},
			"annotations clear": {Annotations: collectorgroups.Clear[json.RawMessage]()},
			"an assignment":     {AssignedPublicationID: collectorgroups.SetTo(PubA)},
			"an unassignment":   {AssignedPublicationID: collectorgroups.Clear[uuid.UUID]()},
		} {
			got, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, g.ID, changes, Now.Add(time.Hour))
			if err != nil || !sameStamp(got.VariableGroupStamp, g.VariableGroupStamp) || !sameUUID(got.VariableGroupID, &eu) {
				t.Errorf("%s = %+v, %v; want the stamp %s kept", label, got, err, derefStamp(g.VariableGroupStamp))
			}
		}
		if got := revision(t, s, OrgA); got != start {
			t.Fatalf("the counter moved from %d to %d", start, got)
		}
	})
}

// Assigning a published version, on a create or an update, that references
// names no variable of the organization defines is refused with those names,
// at most 100 in byte order with the total, and changes nothing; another
// organization's variable of the name doesn't define it, and the error never
// quotes a name.
func TestAnAssignmentNamingAnUndefinedVariableIsRefusedWithItsNames(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		defineVariable(t, s, OrgA, "REGION")
		defineVariable(t, s, OrgB, "TENANT")
		yaml := "a: ${org:REGION}\nb: ${org:TENANT}\nc: ${org:A_FIRST}\n"
		for i := range variableuse.MaxListed + 1 {
			yaml += fmt.Sprintf("n%d: ${org:N_%03d}\n", i, i)
		}
		version := publishYAML(t, s, OrgA, yaml)
		g, err := s.Groups.Create(ctx, named(OrgA, c.Fleet.ID, "gateways"))
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"A_FIRST"}
		for i := range variableuse.MaxListed - 1 {
			want = append(want, fmt.Sprintf("N_%03d", i))
		}
		before := s.Dump(t, OrgA)
		check := func(label string, err error) {
			t.Helper()
			var undefined *collectorgroups.UndefinedError
			if !errors.As(err, &undefined) || !errors.Is(err, collectorgroups.ErrVariablesUndefined) ||
				undefined.Undefined.Total != variableuse.MaxListed+3 || Describe(undefined.Undefined.Names) != Describe(want) {
				t.Fatalf("%s = %v; want the first %d of %d undefined names", label, err, variableuse.MaxListed, variableuse.MaxListed+3)
			}
			if strings.Contains(err.Error(), "N_0") || strings.Contains(err.Error(), "TENANT") {
				t.Fatalf("%s: the error %q quotes a name", label, err)
			}
		}
		create := named(OrgA, c.Fleet.ID, "new")
		create.AssignedPublicationID = &version
		_, err = s.Groups.Create(ctx, create)
		check("Create", err)
		_, err = s.Groups.Update(ctx, OrgA, c.Fleet.ID, g.ID, collectorgroups.Changes{Name: Ptr("renamed"),
			AssignedPublicationID: collectorgroups.SetTo(version)}, Now.Add(time.Hour))
		check("Update", err)
		s.Unchanged(t, "the refused assignments", OrgA, before)
	})
}

// An assignment whose every referenced name is defined passes (a group's
// override needs no more: "defined" means an organization variable), as does
// a version referencing none; a change of the variable group alone re-checks
// nothing.
func TestAnAssignmentWithEveryNameDefinedPasses(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		defineVariable(t, s, OrgA, "REGION")
		defineVariable(t, s, OrgA, "TENANT")
		version := publishYAML(t, s, OrgA, "a: ${org:REGION}\nb: ${org:TENANT}\nc: $${org:LITERAL}\nd: $orgvar:LEGACY\n")
		eu := variableGroup(t, s, OrgA, "eu")
		create := named(OrgA, c.Fleet.ID, "gateways")
		create.AssignedPublicationID, create.VariableGroupID = &version, &eu
		g, err := s.Groups.Create(ctx, create)
		if err != nil || !sameUUID(g.AssignedPublicationID, &version) {
			t.Fatalf("Create = %+v, %v", g, err)
		}
		if got, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, c.DefaultGroupID, collectorgroups.Changes{
			AssignedPublicationID: collectorgroups.SetTo(version)}, Now); err != nil || !sameUUID(got.AssignedPublicationID, &version) {
			t.Fatalf("Update = %+v, %v", got, err)
		}
		if _, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, c.DefaultGroupID, collectorgroups.Changes{
			AssignedPublicationID: collectorgroups.SetTo(PubA)}, Now); err != nil {
			t.Fatalf("a version referencing no name: %v", err)
		}
		// A version of undefined names already assigned (written past the
		// check, as only a test can): a change of the variable group alone
		// passes.
		undefinedVersion := publishYAML(t, s, OrgA, "a: ${org:UNDEFINED}\n")
		assignPastTheCheck(t, s, g.ID, undefinedVersion)
		if _, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, g.ID, collectorgroups.Changes{
			VariableGroupID: collectorgroups.Clear[uuid.UUID]()}, Now); err != nil {
			t.Fatalf("a change of the variable group alone: %v", err)
		}
	})
}

// assignPastTheCheck assigns the published version under the store, as
// only a test can.
func assignPastTheCheck(t *testing.T, s *Stores, groupID, publicationID uuid.UUID) {
	t.Helper()
	if _, err := s.Databases.Owner.Exec(ctx, `update opamp_server.collector_groups set assigned_publication_id = $2
		where id = $1`, groupID, publicationID); err != nil {
		t.Fatal(err)
	}
}

// Clearing an assignment checks nothing, even when the published version it
// held references undefined names, and needs no organization lock beyond the
// key share.
func TestClearingAnAssignmentChecksNothing(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		g, err := s.Groups.Create(ctx, named(OrgA, c.Fleet.ID, "gateways"))
		if err != nil {
			t.Fatal(err)
		}
		assignPastTheCheck(t, s, g.ID, publishYAML(t, s, OrgA, "a: ${org:UNDEFINED}\n"))
		start := revision(t, s, OrgA)
		got, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, g.ID, collectorgroups.Changes{
			AssignedPublicationID: collectorgroups.Clear[uuid.UUID]()}, Now.Add(time.Hour))
		if err != nil || got.AssignedPublicationID != nil {
			t.Fatalf("clearing = %+v, %v", got, err)
		}
		if revision(t, s, OrgA) != start {
			t.Fatal("clearing an assignment moved the counter")
		}
	})
}
