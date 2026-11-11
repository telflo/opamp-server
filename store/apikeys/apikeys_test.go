package apikeys_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/telflo-opamp-server/store/apikeys"
	"github.com/telflo/telflo-opamp-server/store/collectorgroups"
	"github.com/telflo/telflo-opamp-server/store/organizations"
	. "github.com/telflo/telflo-opamp-server/store/storetest"
)

// This package's own test database (CONTRIBUTING.md, "Tests").
const testDatabase = "telflo_opamp_apikeys_test"

var ctx = context.Background()

// newKey is a key limited to orgs (none: every organization), made at Now
// and expiring a year later, its secret's hash taken from its id.
func newKey(orgs ...uuid.UUID) apikeys.NewKey {
	id := uuid.New()
	return apikeys.NewKey{
		ID: id, Name: "deploy pipeline", SecretSHA256: sha256.Sum256(id[:]),
		AllOrganizations: len(orgs) == 0, OrganizationIDs: orgs,
		ExpiresAt: Now.AddDate(1, 0, 0), CreatedAt: Now,
	}
}

func create(t *testing.T, s *Stores, k apikeys.NewKey) apikeys.Key {
	t.Helper()
	stored, err := s.APIKeys.Create(ctx, k)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return stored
}

func keyCount(t *testing.T, s *Stores) int {
	t.Helper()
	keys, err := s.APIKeys.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return len(keys)
}

// Create stores every field, times to the millisecond in UTC, and a
// limited key's organizations in byte order; Get, List and Lookup read it
// back, Lookup with the secret's hash.
func TestCreateStoresTheKeyAndEveryReadAnswersIt(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		description := "Pushes the edge configurations."
		k := newKey(OrgB, OrgA)
		k.Description = &description
		k.ExpiresAt = Now.Add(time.Hour).In(time.FixedZone("CET", 3600))

		stored := create(t, s, k)
		want := apikeys.Key{
			ID: k.ID, Name: k.Name, Description: &description,
			OrganizationIDs: []uuid.UUID{OrgA, OrgB},
			ExpiresAt:       Millisecond(Now.Add(time.Hour)), CreatedAt: Millisecond(Now),
		}
		if !reflect.DeepEqual(stored, want) {
			t.Fatalf("Create = %+v, want %+v", stored, want)
		}
		if got, err := s.APIKeys.Get(ctx, k.ID); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("Get = %+v, %v; want %+v", got, err, want)
		}
		got, err := s.APIKeys.Lookup(ctx, k.ID)
		if err != nil || !reflect.DeepEqual(got.Key, want) || got.SecretSHA256 != k.SecretSHA256 {
			t.Errorf("Lookup = %+v, %v; want %+v with the secret's hash", got, err, want)
		}

		laterKey := newKey()
		laterKey.CreatedAt = Now.Add(time.Second)
		all := create(t, s, laterKey)
		if !all.AllOrganizations || all.OrganizationIDs != nil {
			t.Errorf("a key for every organization = %+v, want AllOrganizations and no ids", all)
		}
		keys, err := s.APIKeys.List(ctx)
		if err != nil || len(keys) != 2 || keys[0].ID != all.ID || keys[1].ID != k.ID {
			t.Errorf("List = %+v, %v; want both, newest first", keys, err)
		}
	})
}

// A key's opens are its organizations, or every one.
func TestOpensIsTheKeysOrganizations(t *testing.T) {
	limited := apikeys.Key{OrganizationIDs: []uuid.UUID{OrgA, OrgB}}
	all := apikeys.Key{AllOrganizations: true}
	for _, c := range []struct {
		key  apikeys.Key
		org  uuid.UUID
		want bool
	}{
		{limited, OrgA, true}, {limited, OrgB, true}, {limited, OrgUnregistered, false},
		{apikeys.Key{OrganizationIDs: []uuid.UUID{}}, OrgA, false},
		{all, OrgUnregistered, true},
	} {
		if got := c.key.Opens(c.org); got != c.want {
			t.Errorf("%+v Opens(%s) = %v, want %v", c.key, c.org, got, c.want)
		}
	}
}

// A key naming an unregistered organization is refused, and nothing is
// stored.
func TestCreateRefusesAnUnregisteredOrganization(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		if _, err := s.APIKeys.Create(ctx, newKey(OrgA, OrgUnregistered)); !errors.Is(err, apikeys.ErrOrganizationNotFound) {
			t.Fatalf("Create = %v, want ErrOrganizationNotFound", err)
		}
		if n := keyCount(t, s); n != 0 {
			t.Errorf("%d keys stored, want none", n)
		}
	})
}

// What the table's checks refuse, and a key naming no organization, one
// twice, or some besides every one, is refused before any statement.
func TestCreateAndUpdateRefuseAnInvalidKey(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		blank, long := "", strings.Repeat("d", apikeys.MaxDescriptionLength+1)
		for name, change := range map[string]func(*apikeys.NewKey){
			"an empty name":               func(k *apikeys.NewKey) { k.Name = "" },
			"a name too long":             func(k *apikeys.NewKey) { k.Name = strings.Repeat("n", apikeys.MaxNameLength+1) },
			"a name with a leading space": func(k *apikeys.NewKey) { k.Name = " deploy" },
			"a name with a newline":       func(k *apikeys.NewKey) { k.Name = "deploy\npipeline" },
			"an empty description":        func(k *apikeys.NewKey) { k.Description = &blank },
			"a description too long":      func(k *apikeys.NewKey) { k.Description = &long },
			"an expiry at its creation":   func(k *apikeys.NewKey) { k.ExpiresAt = k.CreatedAt },
			"no organization":             func(k *apikeys.NewKey) { k.AllOrganizations, k.OrganizationIDs = false, []uuid.UUID{} },
			"an organization twice":       func(k *apikeys.NewKey) { k.AllOrganizations, k.OrganizationIDs = false, []uuid.UUID{OrgA, OrgA} },
			"organizations and every one": func(k *apikeys.NewKey) { k.OrganizationIDs = []uuid.UUID{OrgA} },
			"more organizations than allowed": func(k *apikeys.NewKey) {
				k.AllOrganizations, k.OrganizationIDs = false, make([]uuid.UUID, apikeys.MaxOrganizations+1)
				for i := range k.OrganizationIDs {
					k.OrganizationIDs[i] = uuid.New()
				}
			},
		} {
			k := newKey()
			change(&k)
			if _, err := s.APIKeys.Create(ctx, k); !errors.Is(err, apikeys.ErrInvalid) {
				t.Errorf("%s: Create = %v, want ErrInvalid", name, err)
			}
		}
		if n := keyCount(t, s); n != 0 {
			t.Errorf("%d keys stored, want none", n)
		}

		stored := create(t, s, newKey())
		for name, c := range map[string]apikeys.Changes{
			"an empty name":        {Name: &blank},
			"an empty description": {Description: collectorgroups.SetTo("")},
		} {
			if _, err := s.APIKeys.Update(ctx, stored.ID, c); !errors.Is(err, apikeys.ErrInvalid) {
				t.Errorf("%s: Update = %v, want ErrInvalid", name, err)
			}
		}
	})
}

// MaxActive valid keys refuse another; a revoked or expired key doesn't
// count.
func TestCreateRefusesAKeyPastTheActiveLimit(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		expiring := newKey()
		expiring.ExpiresAt = Now.Add(time.Minute)
		create(t, s, expiring)
		revoked := create(t, s, newKey())
		if _, err := s.APIKeys.Revoke(ctx, revoked.ID, Now); err != nil {
			t.Fatal(err)
		}
		for range apikeys.MaxActive - 1 {
			create(t, s, newKey())
		}
		// Valid at Now: the expiring one and MaxActive-1 more.
		if _, err := s.APIKeys.Create(ctx, newKey()); !errors.Is(err, apikeys.ErrLimitReached) {
			t.Fatalf("Create past the limit = %v, want ErrLimitReached", err)
		}
		// A minute later the expiring one no longer counts.
		later := newKey()
		later.CreatedAt = Now.Add(time.Minute)
		create(t, s, later)
	})
}

// Creates racing for the last place: exactly one gets it.
func TestCreatesRacingForTheLastPlaceLetOneThrough(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		for range apikeys.MaxActive - 1 {
			create(t, s, newKey())
		}
		const racers = 8
		errs := make([]error, racers)
		var wg sync.WaitGroup
		for i := range racers {
			wg.Go(func() {
				_, errs[i] = s.APIKeys.Create(ctx, newKey(OrgA))
			})
		}
		wg.Wait()
		created := 0
		for _, err := range errs {
			switch {
			case err == nil:
				created++
			case !errors.Is(err, apikeys.ErrLimitReached):
				t.Errorf("a racing Create = %v, want nil or ErrLimitReached", err)
			}
		}
		if created != 1 {
			t.Errorf("%d racing Creates got through, want 1", created)
		}
	})
}

// Revoke keeps the first revocation's time; an unknown key is ErrNotFound.
func TestRevokeKeepsTheFirstTime(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		stored := create(t, s, newKey(OrgA))
		first, err := s.APIKeys.Revoke(ctx, stored.ID, Now.Add(time.Minute))
		if err != nil || first.RevokedAt == nil || !first.RevokedAt.Equal(Millisecond(Now.Add(time.Minute))) {
			t.Fatalf("Revoke = %+v, %v; want revoked a minute after Now", first, err)
		}
		if first.ValidAt(Now.Add(2 * time.Minute)) {
			t.Error("a revoked key is valid")
		}
		again, err := s.APIKeys.Revoke(ctx, stored.ID, Now.Add(time.Hour))
		if err != nil || !again.RevokedAt.Equal(*first.RevokedAt) {
			t.Errorf("a second Revoke = %+v, %v; want the first's time", again, err)
		}
		if !reflect.DeepEqual(again.OrganizationIDs, []uuid.UUID{OrgA}) {
			t.Errorf("Revoke answered the organizations %v, want [A]", again.OrganizationIDs)
		}
		if _, err := s.APIKeys.Revoke(ctx, uuid.New(), Now); !errors.Is(err, apikeys.ErrNotFound) {
			t.Errorf("Revoke of an unknown key = %v, want ErrNotFound", err)
		}
	})
}

// Update changes the name, the description, both or neither, and clears
// the description; an unknown key is ErrNotFound.
func TestUpdateChangesOnlyWhatItNames(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		description := "Pushes the edge configurations."
		k := newKey()
		k.Description = &description
		stored := create(t, s, k)

		renamed := "release pipeline"
		got, err := s.APIKeys.Update(ctx, stored.ID, apikeys.Changes{Name: &renamed})
		if err != nil || got.Name != renamed || got.Description == nil || *got.Description != description {
			t.Fatalf("a rename = %+v, %v; want the name changed and the description kept", got, err)
		}
		got, err = s.APIKeys.Update(ctx, stored.ID, apikeys.Changes{Description: collectorgroups.Clear[string]()})
		if err != nil || got.Name != renamed || got.Description != nil {
			t.Fatalf("a clear = %+v, %v; want the description null and the name kept", got, err)
		}
		if got, err = s.APIKeys.Update(ctx, stored.ID, apikeys.Changes{}); err != nil || got.Name != renamed {
			t.Errorf("no change = %+v, %v; want the key as it was", got, err)
		}
		if _, err := s.APIKeys.Update(ctx, uuid.New(), apikeys.Changes{Name: &renamed}); !errors.Is(err, apikeys.ErrNotFound) {
			t.Errorf("Update of an unknown key = %v, want ErrNotFound", err)
		}
		if _, err := s.APIKeys.Get(ctx, uuid.New()); !errors.Is(err, apikeys.ErrNotFound) {
			t.Errorf("Get of an unknown key = %v, want ErrNotFound", err)
		}
		if _, err := s.APIKeys.Lookup(ctx, uuid.New()); !errors.Is(err, apikeys.ErrNotFound) {
			t.Errorf("Lookup of an unknown key = %v, want ErrNotFound", err)
		}
	})
}

// Touch sets the last use, never back.
func TestTouchNeverMovesTheLastUseBack(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		stored := create(t, s, newKey())
		for _, at := range []time.Time{Now.Add(time.Hour), Now.Add(time.Minute)} {
			if err := s.APIKeys.Touch(ctx, stored.ID, at); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.APIKeys.Get(ctx, stored.ID)
		if err != nil || got.LastUsedAt == nil || !got.LastUsedAt.Equal(Millisecond(Now.Add(time.Hour))) {
			t.Errorf("after two touches = %+v, %v; want the later one", got, err)
		}
	})
}

// An organization's delete takes it from every key, and registering its id
// again doesn't give it back.
func TestAnOrganizationsDeleteTakesItFromEveryKey(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		stored := create(t, s, newKey(OrgA, OrgB))
		if deleted, err := s.Organizations.Delete(ctx, OrgA); err != nil || !deleted {
			t.Fatalf("deleting A: %v, %v", deleted, err)
		}
		if _, _, err := s.Organizations.Create(ctx, organizations.Organization{
			ID: OrgA, Name: "A again", CreatedAt: Now, UpdatedAt: Now,
		}); err != nil {
			t.Fatal(err)
		}
		got, err := s.APIKeys.Get(ctx, stored.ID)
		if err != nil || !reflect.DeepEqual(got.OrganizationIDs, []uuid.UUID{OrgB}) || got.Opens(OrgA) {
			t.Errorf("after A's delete = %+v, %v; want only B", got, err)
		}
	})
}

// The store's errors quote nothing it was given.
func TestErrorsQuoteNoName(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		k := newKey()
		k.Name = "a name that must not leak"
		s.Databases.Runtime.Close()
		_, err := s.APIKeys.Create(ctx, k)
		if err == nil || strings.Contains(fmt.Sprint(err), k.Name) {
			t.Errorf("Create on a closed pool = %v; want an error that doesn't quote the name", err)
		}
	})
}
