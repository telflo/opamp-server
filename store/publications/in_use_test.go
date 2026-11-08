package publications_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/collectorgroups"
	"github.com/telflo/opamp-server/store/fleets"
	"github.com/telflo/opamp-server/store/publications"
	"github.com/telflo/opamp-server/store/storetest"
)

// A published version a collector group is assigned, or a collector pinned
// to, is not deleted, alone or with its configuration's (ErrInUse, deleting
// nothing of either). Once nothing uses it, it deletes.
func TestDeleteRefusesAPublishedVersionAGroupOrACollectorUses(t *testing.T) {
	storetest.Each(t, testDatabase, func(t *testing.T, s *storetest.Stores) {
		ctx := context.Background()
		used, err := s.Publications.Get(ctx, storetest.OrgA, storetest.PubA)
		if err != nil {
			t.Fatal(err)
		}
		// A second published version of the same configuration, unused.
		sibling := used
		sibling.ID, sibling.VersionID, sibling.VersionNumber = uuid.New(), uuid.New(), 4
		if result, _, err := s.Publications.Put(ctx, sibling); err != nil || result != publications.Created {
			t.Fatalf("Put = %v, %v", result, err)
		}

		c := s.CreateFleet(t, storetest.OrgA, "edge-prod", fleets.TypeServer)
		group, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: storetest.OrgA, FleetID: c.Fleet.ID,
			Name: "gateways", AssignedPublicationID: &used.ID, At: storetest.Now})
		if err != nil {
			t.Fatal(err)
		}
		assertRefused := func(by string) {
			t.Helper()
			before := s.Dump(t, storetest.OrgA)
			if deleted, err := s.Publications.Delete(ctx, storetest.OrgA, used.ID); !errors.Is(err, publications.ErrInUse) || deleted {
				t.Fatalf("Delete while %s = %v, %v; want ErrInUse", by, deleted, err)
			}
			if deleted, err := s.Publications.DeleteForConfiguration(ctx, storetest.OrgA, used.ConfigurationID); !errors.Is(err, publications.ErrInUse) || deleted != 0 {
				t.Fatalf("DeleteForConfiguration while %s = %v, %v; want ErrInUse", by, deleted, err)
			}
			s.Unchanged(t, "the refused deletes", storetest.OrgA, before)
		}
		assertRefused("a group is assigned it")

		if _, err := s.Groups.Update(ctx, storetest.OrgA, c.Fleet.ID, group.ID, collectorgroups.Changes{
			AssignedPublicationID: collectorgroups.Clear[uuid.UUID]()}, storetest.Now); err != nil {
			t.Fatal(err)
		}
		s.InsertCollector(t, storetest.Collector{ID: uuid.New(), OrganizationID: storetest.OrgA, FleetID: c.Fleet.ID,
			GroupID: group.ID, Name: "edge-01", PinnedPublicationID: &used.ID, LastSeenAt: storetest.Now})
		assertRefused("a collector is pinned to it")

		// B's delete of A's id is no answer about A's use: not found.
		if deleted, err := s.Publications.Delete(ctx, storetest.OrgB, used.ID); err != nil || deleted {
			t.Fatalf("B's Delete of A's published version = %v, %v", deleted, err)
		}

		// The unused sibling deletes alone; with the fleet gone (its
		// collector with it), so does the one it used.
		if deleted, err := s.Publications.Delete(ctx, storetest.OrgA, sibling.ID); err != nil || !deleted {
			t.Fatalf("the unused sibling's Delete = %v, %v", deleted, err)
		}
		if _, err := s.Fleets.Delete(ctx, storetest.OrgA, c.Fleet.ID); err != nil {
			t.Fatal(err)
		}
		if deleted, err := s.Publications.DeleteForConfiguration(ctx, storetest.OrgA, used.ConfigurationID); err != nil || deleted != 1 {
			t.Fatalf("DeleteForConfiguration once unused = %v, %v", deleted, err)
		}
		if !strings.Contains(s.Dump(t, storetest.OrgB), storetest.PubB.String()) {
			t.Fatal("B's published version went")
		}
	})
}

// A configuration's usage answers each of its published versions, newest
// first and without the YAML, with the groups assigned it (by fleet, then
// group) and a count of the collectors pinned to it. Another organization's
// groups and collectors, and its configurations, are never counted.
func TestUsageAnswersEachPublishedVersionWithWhatUsesIt(t *testing.T) {
	storetest.Each(t, testDatabase, func(t *testing.T, s *storetest.Stores) {
		ctx := context.Background()
		used, err := s.Publications.Get(ctx, storetest.OrgA, storetest.PubA)
		if err != nil {
			t.Fatal(err)
		}
		// A newer, unused published version of the same configuration.
		newer := used
		newer.ID, newer.VersionID, newer.VersionNumber = uuid.New(), uuid.New(), 4
		newer.PublishedAt = used.PublishedAt.Add(time.Hour)
		if result, _, err := s.Publications.Put(ctx, newer); err != nil || result != publications.Created {
			t.Fatalf("Put = %v, %v", result, err)
		}

		empty, err := s.Publications.Usage(ctx, storetest.OrgA, used.ConfigurationID)
		if err != nil || len(empty) != 2 || empty[0].InUse() || empty[1].InUse() {
			t.Fatalf("the usage of nothing in use = %+v, %v", empty, err)
		}

		edge := s.CreateFleet(t, storetest.OrgA, "edge-prod", fleets.TypeServer)
		dev := s.CreateFleet(t, storetest.OrgA, "edge-dev", fleets.TypeServer)
		var groups []publications.GroupRef
		for _, f := range []fleets.Creation{edge, dev} {
			g, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: storetest.OrgA, FleetID: f.Fleet.ID,
				Name: "gateways", AssignedPublicationID: &used.ID, At: storetest.Now})
			if err != nil {
				t.Fatal(err)
			}
			groups = append(groups, publications.GroupRef{FleetID: f.Fleet.ID, GroupID: g.ID})
		}
		slices.SortFunc(groups, func(a, b publications.GroupRef) int {
			if c := strings.Compare(string(a.FleetID[:]), string(b.FleetID[:])); c != 0 {
				return c
			}
			return strings.Compare(string(a.GroupID[:]), string(b.GroupID[:]))
		})
		for i, name := range []string{"edge-01", "edge-02"} {
			s.InsertCollector(t, storetest.Collector{ID: uuid.New(), OrganizationID: storetest.OrgA, FleetID: edge.Fleet.ID,
				GroupID: edge.DefaultGroupID, Name: name, PinnedPublicationID: &used.ID, LastSeenAt: storetest.Now.Add(time.Duration(i) * time.Second)})
		}
		// B's own use of its own published version.
		theirs := s.CreateFleet(t, storetest.OrgB, "edge-b", fleets.TypeServer)
		if _, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: storetest.OrgB, FleetID: theirs.Fleet.ID,
			Name: "gateways", AssignedPublicationID: &storetest.PubB, At: storetest.Now}); err != nil {
			t.Fatal(err)
		}

		usage, err := s.Publications.Usage(ctx, storetest.OrgA, used.ConfigurationID)
		if err != nil {
			t.Fatal(err)
		}
		if len(usage) != 2 || usage[0].Publication.ID != newer.ID || usage[1].Publication.ID != used.ID {
			t.Fatalf("the usage = %+v; want the newer version, then the used one", usage)
		}
		if usage[0].InUse() || len(usage[0].Groups) != 0 || usage[0].PinnedCollectors != 0 {
			t.Fatalf("the unused version's usage = %+v", usage[0])
		}
		if got := usage[1]; !got.InUse() || !slices.Equal(got.Groups, groups) || got.PinnedCollectors != 2 || got.Publication.YAML != "" ||
			got.Publication.VersionNumber != used.VersionNumber || got.Publication.ConfigurationID != used.ConfigurationID {
			t.Fatalf("the used version's usage = %+v; want groups %v and 2 pinned collectors, without YAML", got, groups)
		}

		// B asking for A's configuration sees nothing; B's own counts only
		// its own group.
		if theirsOfA, err := s.Publications.Usage(ctx, storetest.OrgB, used.ConfigurationID); err != nil || len(theirsOfA) != 0 {
			t.Fatalf("B's usage of A's configuration = %+v, %v", theirsOfA, err)
		}
		b, err := s.Publications.Get(ctx, storetest.OrgB, storetest.PubB)
		if err != nil {
			t.Fatal(err)
		}
		if ofB, err := s.Publications.Usage(ctx, storetest.OrgB, b.ConfigurationID); err != nil || len(ofB) != 1 ||
			len(ofB[0].Groups) != 1 || ofB[0].Groups[0].FleetID != theirs.Fleet.ID || ofB[0].PinnedCollectors != 0 {
			t.Fatalf("B's usage of its own = %+v, %v", ofB, err)
		}
	})
}
