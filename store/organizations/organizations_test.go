package organizations_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/telflo-opamp-server/store/collectorgroups"
	"github.com/telflo/telflo-opamp-server/store/database/databasetest"
	"github.com/telflo/telflo-opamp-server/store/fleets"
	"github.com/telflo/telflo-opamp-server/store/organizations"
	"github.com/telflo/telflo-opamp-server/store/publications"
	"github.com/telflo/telflo-opamp-server/store/storetest"
)

// This package's own test database (CONTRIBUTING.md, "Tests").
const testDatabase = "telflo_opamp_organizations_test"

// stores is a registry and the publications store it cascades to.
type stores struct {
	organizations organizations.Store
	publications  publications.Store
}

// eachStore runs test against the Postgres registry and its publications
// store on a fresh, migrated database, as telflo_opamp (the server's role,
// with its real grants), in a subtest named "postgres".
func eachStore(t *testing.T, test func(t *testing.T, s stores)) {
	t.Run("postgres", func(t *testing.T) {
		d := databasetest.New(t, testDatabase)
		test(t, stores{organizations.NewPostgres(d.Runtime), publications.NewPostgres(d.Runtime)})
	})
}

var (
	orgA = uuid.MustParse("0a0a0a0a-0000-4000-8000-00000000000a")
	orgB = uuid.MustParse("0b0b0b0b-0000-4000-8000-00000000000b")
	// now is a test clock's reading, finer than a millisecond.
	now = time.Date(2026, 10, 4, 9, 30, 0, 123456789, time.UTC)
	// atMillisecond is now as a store keeps it.
	atMillisecond = time.Date(2026, 10, 4, 9, 30, 0, 123000000, time.UTC)
)

func organization(id uuid.UUID, name string) organizations.Organization {
	return organizations.Organization{ID: id, Name: name, CreatedAt: now, UpdatedAt: now}
}

func mustCreate(t *testing.T, store organizations.Store, o organizations.Organization) organizations.Organization {
	t.Helper()
	result, stored, err := store.Create(context.Background(), o)
	if err != nil || result != organizations.Created {
		t.Fatalf("Create: %v, %v; want created", result, err)
	}
	return stored
}

func assertOrganization(t *testing.T, label string, got organizations.Organization, id uuid.UUID, name string, createdAt, updatedAt time.Time) {
	t.Helper()
	if got.ID != id || got.Name != name || !got.CreatedAt.Equal(createdAt) || !got.UpdatedAt.Equal(updatedAt) ||
		got.CreatedAt.Location() != time.UTC || got.UpdatedAt.Location() != time.UTC {
		t.Fatalf("%s: got %+v, want %s %q created %s updated %s (UTC)", label, got, id, name, createdAt, updatedAt)
	}
}

// Create registers an id once; the same id and name again is Unchanged (a
// safe retry: the stored organization comes back, first registered), and
// another name is a Conflict that renames nothing and says nothing.
func TestCreateIsCreatedThenUnchangedThenConflicting(t *testing.T) {
	eachStore(t, func(t *testing.T, s stores) {
		ctx := context.Background()
		// A name the store must keep exactly: multi-byte, with surrounding
		// whitespace (not trimmed), at the 400-character limit.
		name := " Ana's workspace ☃ " + strings.Repeat("é", 381)
		stored := mustCreate(t, s.organizations, organization(orgA, name))
		assertOrganization(t, "the created organization", stored, orgA, name, atMillisecond, atMillisecond)

		later := organization(orgA, name)
		later.CreatedAt, later.UpdatedAt = now.Add(time.Hour), now.Add(time.Hour)
		result, again, err := s.organizations.Create(ctx, later)
		if err != nil || result != organizations.Unchanged {
			t.Fatalf("the same id and name again = %v, %v; want unchanged", result, err)
		}
		assertOrganization(t, "the unchanged organization", again, orgA, name, atMillisecond, atMillisecond)

		for label, other := range map[string]string{
			"another name":                     "Bo's workspace",
			"the name without its whitespace":  strings.TrimSpace(name),
			"the name in another letter case":  strings.ToUpper(name),
			"the name with one character less": name[:len(name)-2],
		} {
			result, conflicting, err := s.organizations.Create(ctx, organization(orgA, other))
			if err != nil || result != organizations.Conflict || conflicting != (organizations.Organization{}) {
				t.Fatalf("%s: Create = %v, %+v, %v; want a bare conflict", label, result, conflicting, err)
			}
		}
		got, err := s.organizations.Get(ctx, orgA)
		if err != nil {
			t.Fatal(err)
		}
		assertOrganization(t, "after the conflicts", got, orgA, name, atMillisecond, atMillisecond)
	})
}

func TestGetAndExistsFindOnlyARegisteredOrganization(t *testing.T) {
	eachStore(t, func(t *testing.T, s stores) {
		ctx := context.Background()
		mustCreate(t, s.organizations, organization(orgA, "A"))

		if got, err := s.organizations.Get(ctx, orgA); err != nil || got.Name != "A" {
			t.Fatalf("Get(A) = %+v, %v", got, err)
		}
		if exists, err := s.organizations.Exists(ctx, orgA); err != nil || !exists {
			t.Fatalf("Exists(A) = %v, %v; want true", exists, err)
		}
		if _, err := s.organizations.Get(ctx, orgB); !errors.Is(err, organizations.ErrNotFound) {
			t.Fatalf("Get(B) = %v, want ErrNotFound", err)
		}
		if exists, err := s.organizations.Exists(ctx, orgB); err != nil || exists {
			t.Fatalf("Exists(B) = %v, %v; want false", exists, err)
		}
	})
}

// Rename changes the name and moves UpdatedAt, never CreatedAt; renaming to
// the name it has changes nothing; an unknown id is ErrNotFound. Another
// organization is untouched.
func TestRenameMovesUpdatedAtOnlyWhenTheNameChanges(t *testing.T) {
	eachStore(t, func(t *testing.T, s stores) {
		ctx := context.Background()
		mustCreate(t, s.organizations, organization(orgA, "A"))
		mustCreate(t, s.organizations, organization(orgB, "B"))

		renamedAt := now.Add(time.Hour)
		renamed, err := s.organizations.Rename(ctx, orgA, "A, renamed", renamedAt)
		if err != nil {
			t.Fatal(err)
		}
		assertOrganization(t, "the renamed organization", renamed, orgA, "A, renamed", atMillisecond, atMillisecond.Add(time.Hour))

		same, err := s.organizations.Rename(ctx, orgA, "A, renamed", renamedAt.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		assertOrganization(t, "renamed to its own name", same, orgA, "A, renamed", atMillisecond, atMillisecond.Add(time.Hour))

		got, err := s.organizations.Get(ctx, orgA)
		if err != nil {
			t.Fatal(err)
		}
		assertOrganization(t, "A after the renames", got, orgA, "A, renamed", atMillisecond, atMillisecond.Add(time.Hour))
		other, err := s.organizations.Get(ctx, orgB)
		if err != nil {
			t.Fatal(err)
		}
		assertOrganization(t, "B", other, orgB, "B", atMillisecond, atMillisecond)

		if _, err := s.organizations.Rename(ctx, uuid.New(), "nobody", renamedAt); !errors.Is(err, organizations.ErrNotFound) {
			t.Fatalf("Rename of an unknown id = %v, want ErrNotFound", err)
		}
		// A create after the rename compares against the new name.
		if result, _, err := s.organizations.Create(ctx, organization(orgA, "A, renamed")); err != nil || result != organizations.Unchanged {
			t.Fatalf("Create with the new name = %v, %v; want unchanged", result, err)
		}
		if result, _, err := s.organizations.Create(ctx, organization(orgA, "A")); err != nil || result != organizations.Conflict {
			t.Fatalf("Create with the old name = %v, %v; want conflict", result, err)
		}
	})
}

func publication(organization uuid.UUID) publications.Publication {
	return publications.Publication{
		ID:                uuid.New(),
		OrganizationID:    organization,
		ConfigurationID:   uuid.New(),
		ConfigurationName: "payments-gateway",
		VersionID:         uuid.New(),
		VersionNumber:     1,
		YAML:              "receivers: {}\n",
		SHA256:            strings.Repeat("ab", 32),
		PublishedAt:       now,
		ReceivedAt:        now,
		Collector:         publications.Collector{Distribution: "otelcol-contrib", Version: "0.162.0"},
	}
}

// Delete removes the organization and, by the cascade, every publication it
// owns, and nothing of another's. A second Delete finds nothing, and the
// organization can be registered again.
func TestDeleteRemovesTheOrganizationAndOnlyItsPublications(t *testing.T) {
	eachStore(t, func(t *testing.T, s stores) {
		ctx := context.Background()
		mustCreate(t, s.organizations, organization(orgA, "A"))
		mustCreate(t, s.organizations, organization(orgB, "B"))
		var ours, theirs []publications.Publication
		for range 3 {
			for _, list := range []struct {
				org  uuid.UUID
				into *[]publications.Publication
			}{{orgA, &ours}, {orgB, &theirs}} {
				p := publication(list.org)
				if result, _, err := s.publications.Put(ctx, p); err != nil || result != publications.Created {
					t.Fatalf("Put: %v, %v", result, err)
				}
				*list.into = append(*list.into, p)
			}
		}

		if deleted, err := s.organizations.Delete(ctx, orgA); err != nil || !deleted {
			t.Fatalf("Delete(A) = %v, %v; want true", deleted, err)
		}
		for _, p := range ours {
			if _, err := s.publications.Get(ctx, orgA, p.ID); !errors.Is(err, publications.ErrNotFound) {
				t.Fatalf("A's publication survived its organization: %v", err)
			}
		}
		for _, p := range theirs {
			if _, err := s.publications.Get(ctx, orgB, p.ID); err != nil {
				t.Fatalf("B's publication didn't survive A's delete: %v", err)
			}
		}
		if exists, err := s.organizations.Exists(ctx, orgA); err != nil || exists {
			t.Fatalf("Exists(A) after its delete = %v, %v", exists, err)
		}
		if exists, err := s.organizations.Exists(ctx, orgB); err != nil || !exists {
			t.Fatalf("Exists(B) after A's delete = %v, %v", exists, err)
		}
		// Gone: a publication can't be put under it any more.
		if _, _, err := s.publications.Put(ctx, publication(orgA)); !errors.Is(err, publications.ErrOrganizationNotFound) {
			t.Fatalf("Put under the deleted organization = %v, want ErrOrganizationNotFound", err)
		}
		if deleted, err := s.organizations.Delete(ctx, orgA); err != nil || deleted {
			t.Fatalf("a second Delete(A) = %v, %v; want false", deleted, err)
		}
		mustCreate(t, s.organizations, organization(orgA, "A, again"))
	})
}

// An organization's limits, each null (no cap, the server's default) until
// set; set both at once, null unsetting; refused below their bounds, setting
// nothing; not found for an unregistered organization; another
// organization's untouched.
func TestLimitsAreUnsetUntilSetAndBounded(t *testing.T) {
	eachStore(t, func(t *testing.T, s stores) {
		ctx := context.Background()
		mustCreate(t, s.organizations, organization(orgA, "A"))
		mustCreate(t, s.organizations, organization(orgB, "B"))
		if l, err := s.organizations.Limits(ctx, orgA); err != nil || l.MaxCollectors != nil || l.RequestsPerMinute != nil {
			t.Fatalf("Limits before any = %+v, %v", l, err)
		}
		zero, one := int32(0), int32(1)
		set, err := s.organizations.SetLimits(ctx, orgA, organizations.Limits{MaxCollectors: &zero, RequestsPerMinute: &one})
		if err != nil || *set.MaxCollectors != 0 || *set.RequestsPerMinute != 1 {
			t.Fatalf("SetLimits = %+v, %v", set, err)
		}
		zero = 99 // the store kept its own copy
		if l, err := s.organizations.Limits(ctx, orgA); err != nil || *l.MaxCollectors != 0 || *l.RequestsPerMinute != 1 {
			t.Fatalf("Limits = %+v, %v", l, err)
		}
		for label, bad := range map[string]organizations.Limits{
			"a negative cap":             {MaxCollectors: ptr(int32(-1))},
			"no requests a minute":       {RequestsPerMinute: ptr(int32(0))},
			"negative requests a minute": {RequestsPerMinute: ptr(int32(-5))},
		} {
			if _, err := s.organizations.SetLimits(ctx, orgA, bad); !errors.Is(err, organizations.ErrInvalidLimits) {
				t.Fatalf("%s: SetLimits = %v, want ErrInvalidLimits", label, err)
			}
		}
		if l, err := s.organizations.Limits(ctx, orgA); err != nil || *l.MaxCollectors != 0 || *l.RequestsPerMinute != 1 {
			t.Fatalf("a refused SetLimits changed them: %+v, %v", l, err)
		}
		set, err = s.organizations.SetLimits(ctx, orgA, organizations.Limits{RequestsPerMinute: ptr(int32(600))})
		if err != nil || set.MaxCollectors != nil || *set.RequestsPerMinute != 600 {
			t.Fatalf("unsetting the cap = %+v, %v", set, err)
		}
		if got, err := s.organizations.Get(ctx, orgA); err != nil || !got.UpdatedAt.Equal(atMillisecond) {
			t.Fatalf("SetLimits moved UpdatedAt: %+v, %v", got, err)
		}
		if l, err := s.organizations.Limits(ctx, orgB); err != nil || l.MaxCollectors != nil || l.RequestsPerMinute != nil {
			t.Fatalf("B's limits = %+v, %v", l, err)
		}
		if _, err := s.organizations.Limits(ctx, uuid.New()); !errors.Is(err, organizations.ErrNotFound) {
			t.Fatalf("an unregistered organization's Limits = %v", err)
		}
		if _, err := s.organizations.SetLimits(ctx, uuid.New(), organizations.Limits{}); !errors.Is(err, organizations.ErrNotFound) {
			t.Fatalf("an unregistered organization's SetLimits = %v", err)
		}
	})
}

func ptr[T any](v T) *T { return &v }

// Deleting an organization deletes its whole fleet model, a published
// version in use included, and nothing of another's.
func TestDeleteRemovesTheOrganizationsFleetModel(t *testing.T) {
	storetest.Each(t, testDatabase, func(t *testing.T, s *storetest.Stores) {
		ctx := context.Background()
		for _, org := range []uuid.UUID{storetest.OrgA, storetest.OrgB} {
			pub := storetest.PubA
			if org == storetest.OrgB {
				pub = storetest.PubB
			}
			c := s.CreateFleet(t, org, "edge-prod", fleets.TypeServer)
			if _, err := s.Groups.Update(ctx, org, c.Fleet.ID, c.DefaultGroupID, collectorgroups.Changes{
				AssignedPublicationID: collectorgroups.SetTo(pub)}, storetest.Now); err != nil {
				t.Fatal(err)
			}
			s.InsertCollector(t, storetest.Collector{ID: uuid.New(), OrganizationID: org, FleetID: c.Fleet.ID,
				GroupID: c.DefaultGroupID, Name: "edge-01", TokenID: &c.RotateTokenID, PinnedPublicationID: &pub, LastSeenAt: storetest.Now})
		}
		keptB := s.Dump(t, storetest.OrgB)
		if deleted, err := s.Organizations.Delete(ctx, storetest.OrgA); err != nil || !deleted {
			t.Fatalf("Delete = %v, %v", deleted, err)
		}
		if left := s.Dump(t, storetest.OrgA); left != "" {
			t.Fatalf("A's rows left:\n%s", left)
		}
		s.Unchanged(t, "A's delete", storetest.OrgB, keptB)
	})
}

// Production wiring makes the store with no option (management's serve), so
// its Delete runs again after a 40P01 DeadlockRetries times by default: the
// retry test passes its retries explicitly, so it can't see the default.
func TestNewPostgresRetriesADeadlockByDefault(t *testing.T) {
	if got := organizations.DeadlockRetriesOf(organizations.NewPostgres(nil)); got != organizations.DeadlockRetries || got < 1 {
		t.Fatalf("NewPostgres's Delete runs again %d times after a 40P01, want DeadlockRetries (%d)", got, organizations.DeadlockRetries)
	}
	if got := organizations.DeadlockRetriesOf(organizations.NewPostgres(nil, organizations.WithDeadlockRetries(0))); got != 0 {
		t.Fatalf("WithDeadlockRetries(0) leaves %d retries, want 0", got)
	}
}
