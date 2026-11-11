package publications_test

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/telflo/telflo-opamp-server/store/database/databasetest"
	"github.com/telflo/telflo-opamp-server/store/orgvariables"
	"github.com/telflo/telflo-opamp-server/store/publications"
	"github.com/telflo/telflo-opamp-server/store/variableuse"
)

// This package's own test database (CONTRIBUTING.md, "Tests").
const testDatabase = "telflo_opamp_publications_test"

// eachStore runs test against the Postgres store on a fresh, migrated
// database, as telflo_opamp (the server's role, with its real grants), in a
// subtest named "postgres". Organizations A and B are registered, and no
// other.
func eachStore(t *testing.T, test func(t *testing.T, store publications.Store)) {
	t.Run("postgres", func(t *testing.T) {
		d := databasetest.New(t, testDatabase)
		for _, id := range []uuid.UUID{orgA, orgB} {
			if _, err := d.Owner.Exec(context.Background(), `
				insert into telflo_opamp.organizations (id, name, created_at, updated_at, generation)
				values ($1, 'an organization', $2, $2, gen_random_uuid())`, id, received); err != nil {
				t.Fatal(err)
			}
		}
		test(t, publications.NewPostgres(d.Runtime))
	})
}

var (
	orgA = uuid.MustParse("0a0a0a0a-0000-4000-8000-00000000000a")
	orgB = uuid.MustParse("0b0b0b0b-0000-4000-8000-00000000000b")
	// orgC is registered nowhere.
	orgC     = uuid.MustParse("0c0c0c0c-0000-4000-8000-00000000000c")
	configA  = uuid.MustParse("2a2a2a2a-0000-4000-8000-00000000002a")
	configB  = uuid.MustParse("2b2b2b2b-0000-4000-8000-00000000002b")
	received = time.Date(2026, 10, 3, 15, 0, 0, 123000000, time.UTC)
	// tricky is YAML no store may change by a byte: HTML characters,
	// multi-byte text, a carriage return, a tab, trailing spaces, U+2028.
	tricky = "receivers:\r\n  otlp: # <tls> & \"quotes\" 'single'\n\tname: café ☃ 日本 🚀  \n  note: \u2028end\n"
)

func label(s string) *string { return &s }

// sample is a valid publication of configuration in organization.
func sample(organization, configuration uuid.UUID, publishedAt time.Time) publications.Publication {
	return publications.Publication{
		ID:                uuid.New(),
		OrganizationID:    organization,
		ConfigurationID:   configuration,
		ConfigurationName: "payments-gateway",
		VersionID:         uuid.New(),
		VersionNumber:     3,
		VersionLabel:      label("TLS on"),
		YAML:              tricky,
		SHA256:            strings.Repeat("ab", 32),
		PublishedBy:       &publications.Actor{ID: uuid.New(), Email: "ana@acme.io"},
		PublishedAt:       publishedAt.UTC().Truncate(publications.Precision),
		ReceivedAt:        received,
		Collector:         sampleCollector,
	}
}

// sampleCollector is sample's collector: not migration 00003's defaults
// (otelcol-contrib, 0.162.0), so a store that left the columns to them would
// read back another collector.
var sampleCollector = publications.Collector{Distribution: "otelcol-sample", Version: "0.155.3"}

func mustPut(t *testing.T, store publications.Store, p publications.Publication) publications.Publication {
	t.Helper()
	result, stored, err := store.Put(context.Background(), p)
	if err != nil || result != publications.Created {
		t.Fatalf("Put: %v, %v; want created", result, err)
	}
	return stored
}

func assertSame(t *testing.T, label string, got, want publications.Publication) {
	t.Helper()
	if !got.SameAs(want) || !got.ReceivedAt.Equal(want.ReceivedAt) {
		t.Fatalf("%s: got %+v, want %+v", label, got, want)
	}
}

func TestPutCreatesThenFindsTheIdenticalPublicationUnchanged(t *testing.T) {
	eachStore(t, func(t *testing.T, store publications.Store) {
		ctx := context.Background()
		p := sample(orgA, configA, time.Date(2026, 10, 3, 14, 2, 11, 0, time.UTC))

		result, stored, err := store.Put(ctx, p)
		if err != nil || result != publications.Created {
			t.Fatalf("first Put: %v, %v", result, err)
		}
		assertSame(t, "the created publication", stored, p)
		if stored.YAML != tricky {
			t.Fatalf("the stored YAML changed: %q", stored.YAML)
		}

		// The same publication again, received later: nothing changes, and
		// the stored one (first received) comes back.
		again := p
		again.ReceivedAt = received.Add(time.Hour)
		result, stored, err = store.Put(ctx, again)
		if err != nil || result != publications.Unchanged {
			t.Fatalf("second Put: %v, %v; want unchanged", result, err)
		}
		assertSame(t, "the unchanged publication", stored, p)

		got, err := store.Get(ctx, orgA, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		assertSame(t, "the stored publication", got, p)
		if got.YAML != tricky {
			t.Fatalf("Get's YAML differs from what was put: %q", got.YAML)
		}
	})
}

// The collector comes back exactly as it was put, from Put, Get and List,
// whatever it is: the Postgres store writes both columns rather than leave
// them to 00003's defaults, which a collector of its own would otherwise
// read as.
func TestTheCollectorRoundTripsAsWritten(t *testing.T) {
	eachStore(t, func(t *testing.T, store publications.Store) {
		ctx := context.Background()
		for i, collector := range []publications.Collector{
			sampleCollector,
			{Distribution: "otelcol-contrib", Version: "0.162.0"},
			{Distribution: strings.Repeat("a", 31) + "-" + strings.Repeat("b", 32), Version: "10.200.3000"},
		} {
			p := sample(orgA, configA, time.Date(2026, 10, 3, 14, 2, 11, 0, time.UTC).Add(time.Duration(i)*time.Minute))
			p.Collector = collector
			stored := mustPut(t, store, p)
			if stored.Collector != collector {
				t.Fatalf("Put answered %+v, want %+v", stored.Collector, collector)
			}
			got, err := store.Get(ctx, orgA, p.ID)
			if err != nil || got.Collector != collector {
				t.Fatalf("Get = %+v, %v; want %+v", got.Collector, err, collector)
			}
		}
		page, err := store.List(ctx, orgA, configA, publications.MaxListLimit, nil)
		if err != nil {
			t.Fatal(err)
		}
		var listed []string
		for _, p := range page.Publications {
			listed = append(listed, p.Collector.Distribution+" "+p.Collector.Version)
		}
		want := []string{
			strings.Repeat("a", 31) + "-" + strings.Repeat("b", 32) + " 10.200.3000",
			"otelcol-contrib 0.162.0",
			"otelcol-sample 0.155.3",
		}
		if !slices.Equal(listed, want) {
			t.Fatalf("List's collectors %v, want %v", listed, want)
		}
	})
}

func TestPutConflictsOnAnyDifferenceAndChangesNothing(t *testing.T) {
	changes := map[string]func(p *publications.Publication){
		"yaml":               func(p *publications.Publication) { p.YAML += "# changed\n" },
		"sha256":             func(p *publications.Publication) { p.SHA256 = strings.Repeat("cd", 32) },
		"configuration name": func(p *publications.Publication) { p.ConfigurationName = "renamed" },
		"configuration id":   func(p *publications.Publication) { p.ConfigurationID = configB },
		"version id":         func(p *publications.Publication) { p.VersionID = uuid.New() },
		"version number":     func(p *publications.Publication) { p.VersionNumber = 4 },
		"label to another":   func(p *publications.Publication) { p.VersionLabel = label("TLS off") },
		"label to none":      func(p *publications.Publication) { p.VersionLabel = nil },
		"label from none":    func(p *publications.Publication) { p.VersionLabel = label("TLS on") },
		"actor to none":      func(p *publications.Publication) { p.PublishedBy = nil },
		"actor's email": func(p *publications.Publication) {
			p.PublishedBy = &publications.Actor{ID: p.PublishedBy.ID, Email: "bo@acme.io"}
		},
		"actor's id": func(p *publications.Publication) {
			p.PublishedBy = &publications.Actor{ID: uuid.New(), Email: p.PublishedBy.Email}
		},
		"publishedAt by 1 ms":    func(p *publications.Publication) { p.PublishedAt = p.PublishedAt.Add(time.Millisecond) },
		"publishedAt by a month": func(p *publications.Publication) { p.PublishedAt = p.PublishedAt.AddDate(0, 1, 0) },
		// The collector is compared for the same id.
		"collector distribution": func(p *publications.Publication) { p.Collector.Distribution = "otelcol-contrib" },
		"collector version":      func(p *publications.Publication) { p.Collector.Version = "0.155.4" },
	}
	eachStore(t, func(t *testing.T, store publications.Store) {
		ctx := context.Background()
		for name, change := range changes {
			p := sample(orgA, configA, time.Date(2026, 10, 3, 14, 2, 11, 0, time.UTC))
			if name == "label from none" {
				p.VersionLabel = nil
			}
			mustPut(t, store, p)

			different := p
			change(&different)
			result, stored, err := store.Put(ctx, different)
			if err != nil || result != publications.Conflict {
				t.Fatalf("%s: Put = %v, %v; want conflict", name, result, err)
			}
			if stored != (publications.Publication{}) {
				t.Fatalf("%s: a conflict returned %+v", name, stored)
			}
			got, err := store.Get(ctx, orgA, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			assertSame(t, name+": the stored publication", got, p)
		}
	})
}

// "Identical" compares publishedAt as an instant: the same instant in
// another zone is the same publication.
func TestPutComparesPublishedAtAsAnInstant(t *testing.T) {
	eachStore(t, func(t *testing.T, store publications.Store) {
		p := sample(orgA, configA, time.Date(2026, 10, 3, 14, 2, 11, 654321000, time.UTC))
		mustPut(t, store, p)
		elsewhere := p
		elsewhere.PublishedAt = p.PublishedAt.In(time.FixedZone("UTC+2", 2*60*60))
		result, _, err := store.Put(context.Background(), elsewhere)
		if err != nil || result != publications.Unchanged {
			t.Fatalf("Put = %v, %v; want unchanged", result, err)
		}
	})
}

// Times are kept to the millisecond: a publishedAt with finer digits is the
// same publication as its millisecond, either way round, what a store
// returns is the millisecond, and a millisecond's difference is a conflict.
// ReceivedAt is kept to the millisecond too.
func TestPutKeepsAndComparesPublishedAtToTheMillisecond(t *testing.T) {
	eachStore(t, func(t *testing.T, store publications.Store) {
		ctx := context.Background()
		millisecond := time.Date(2026, 10, 3, 14, 2, 11, 123000000, time.UTC)
		finer := time.Date(2026, 10, 3, 14, 2, 11, 123456000, time.UTC)

		for label, order := range map[string][2]time.Time{
			"milliseconds first": {millisecond, finer},
			"microseconds first": {finer, millisecond},
		} {
			p := sample(orgA, configA, millisecond)
			p.PublishedAt = order[0]
			p.ReceivedAt = received.Add(456 * time.Microsecond)
			result, stored, err := store.Put(ctx, p)
			if err != nil || result != publications.Created {
				t.Fatalf("%s: the first Put = %v, %v", label, result, err)
			}
			if !stored.PublishedAt.Equal(millisecond) || !stored.ReceivedAt.Equal(received) {
				t.Fatalf("%s: stored at %s, received %s; want %s and %s", label, stored.PublishedAt, stored.ReceivedAt, millisecond, received)
			}

			again := p
			again.PublishedAt = order[1]
			result, stored, err = store.Put(ctx, again)
			if err != nil || result != publications.Unchanged {
				t.Fatalf("%s: the second Put = %v, %v; want unchanged", label, result, err)
			}
			if !stored.PublishedAt.Equal(millisecond) {
				t.Fatalf("%s: Unchanged returned %s, want %s", label, stored.PublishedAt, millisecond)
			}

			got, err := store.Get(ctx, orgA, p.ID)
			if err != nil || !got.PublishedAt.Equal(millisecond) || !got.ReceivedAt.Equal(received) {
				t.Fatalf("%s: Get = %s, %s, %v; want %s and %s", label, got.PublishedAt, got.ReceivedAt, err, millisecond, received)
			}

			later := p
			later.PublishedAt = millisecond.Add(time.Millisecond)
			if result, _, err := store.Put(ctx, later); err != nil || result != publications.Conflict {
				t.Fatalf("%s: a Put 1 ms later = %v, %v; want conflict", label, result, err)
			}
		}
	})
}

// A publication's id is unique across organizations: another organization
// putting it, even with the identical body, is a conflict, sees nothing of
// the row, and changes nothing.
func TestPutConflictsWithAnotherOrganizationsId(t *testing.T) {
	eachStore(t, func(t *testing.T, store publications.Store) {
		ctx := context.Background()
		p := sample(orgA, configA, time.Date(2026, 10, 3, 14, 2, 11, 0, time.UTC))
		mustPut(t, store, p)

		theirs := p
		theirs.OrganizationID = orgB
		result, stored, err := store.Put(ctx, theirs)
		if err != nil || result != publications.Conflict || stored != (publications.Publication{}) {
			t.Fatalf("Put as B = %v, %+v, %v; want a bare conflict", result, stored, err)
		}
		if _, err := store.Get(ctx, orgB, p.ID); !errors.Is(err, publications.ErrNotFound) {
			t.Fatalf("Get as B = %v, want ErrNotFound", err)
		}
		got, err := store.Get(ctx, orgA, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		assertSame(t, "A's publication", got, p)
	})
}

func TestGetFindsOnlyTheOrganizationsPublication(t *testing.T) {
	eachStore(t, func(t *testing.T, store publications.Store) {
		ctx := context.Background()
		p := mustPut(t, store, sample(orgA, configA, time.Date(2026, 10, 3, 14, 2, 11, 0, time.UTC)))
		for label, lookup := range map[string]struct{ organization, id uuid.UUID }{
			"an unknown id":              {orgA, uuid.New()},
			"another organization's id":  {orgB, p.ID},
			"an id unknown to every org": {orgB, uuid.New()},
		} {
			if _, err := store.Get(ctx, lookup.organization, lookup.id); !errors.Is(err, publications.ErrNotFound) {
				t.Errorf("Get %s = %v, want ErrNotFound", label, err)
			}
		}
	})
}

func TestListIsNewestFirstWithTiesByIdAndPagesEveryRowOnce(t *testing.T) {
	eachStore(t, func(t *testing.T, store publications.Store) {
		ctx := context.Background()
		base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
		var want []publications.Publication
		// Five rows of A's configuration, three of them at one instant.
		for _, offset := range []time.Duration{0, time.Hour, time.Hour, time.Hour, 2 * time.Hour} {
			want = append(want, mustPut(t, store, sample(orgA, configA, base.Add(offset))))
		}
		// Rows that must never appear: another configuration of A's, and
		// B's rows of the same configuration id.
		mustPut(t, store, sample(orgA, configB, base.Add(3*time.Hour)))
		mustPut(t, store, sample(orgB, configA, base.Add(3*time.Hour)))
		mustPut(t, store, sample(orgB, configA, base.Add(time.Hour)))

		slices.SortFunc(want, func(a, b publications.Publication) int {
			if c := b.PublishedAt.Compare(a.PublishedAt); c != 0 {
				return c
			}
			return strings.Compare(b.ID.String(), a.ID.String())
		})

		var got []publications.Publication
		var after *publications.Cursor
		for page := 1; ; page++ {
			if page > 5 {
				t.Fatal("the pages never end")
			}
			result, err := store.List(ctx, orgA, configA, 2, after)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range result.Publications {
				if p.YAML != "" {
					t.Fatalf("List returned YAML for %s", p.ID)
				}
			}
			got = append(got, result.Publications...)
			if result.Next == nil {
				if page != 3 || len(result.Publications) != 1 {
					t.Fatalf("the last page is page %d with %d rows, want page 3 with 1", page, len(result.Publications))
				}
				break
			}
			if len(result.Publications) != 2 {
				t.Fatalf("page %d has %d rows, want 2", page, len(result.Publications))
			}
			// The cursor goes through its string form, as a client sends it.
			cursor, err := publications.ParseCursor(result.Next.String())
			if err != nil {
				t.Fatal(err)
			}
			after = &cursor
		}
		if len(got) != len(want) {
			t.Fatalf("listed %d rows, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i].ID != want[i].ID || !got[i].PublishedAt.Equal(want[i].PublishedAt) {
				t.Fatalf("row %d is %s at %s, want %s at %s", i, got[i].ID, got[i].PublishedAt, want[i].ID, want[i].PublishedAt)
			}
			expected := want[i]
			expected.YAML = ""
			assertSame(t, "a listed row", got[i], expected)
		}

		// A page that ends exactly at the last row has no next cursor.
		exact, err := store.List(ctx, orgA, configA, 5, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(exact.Publications) != 5 || exact.Next != nil {
			t.Fatalf("limit 5 over 5 rows: %d rows, next %v", len(exact.Publications), exact.Next)
		}

		// Nothing of A's for an organization or configuration with none.
		for label, scope := range map[string]struct{ organization, configuration uuid.UUID }{
			"B's configuration B":      {orgB, configB},
			"an unknown configuration": {orgA, uuid.New()},
		} {
			empty, err := store.List(ctx, scope.organization, scope.configuration, 100, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(empty.Publications) != 0 || empty.Next != nil || empty.Publications == nil {
				t.Fatalf("%s: %+v, want an empty, non-nil page", label, empty)
			}
		}
		theirs, err := store.List(ctx, orgB, configA, 100, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(theirs.Publications) != 2 {
			t.Fatalf("B's list has %d rows, want its own 2", len(theirs.Publications))
		}
		for _, p := range theirs.Publications {
			if p.OrganizationID != orgB {
				t.Fatalf("B's list holds %s's row", p.OrganizationID)
			}
		}
	})
}

func TestListRefusesALimitOutOfRange(t *testing.T) {
	eachStore(t, func(t *testing.T, store publications.Store) {
		for _, limit := range []int{0, -1, publications.MaxListLimit + 1} {
			if _, err := store.List(context.Background(), orgA, configA, limit, nil); err == nil {
				t.Errorf("List with limit %d succeeded", limit)
			}
		}
	})
}

func TestDeleteRemovesOnlyTheOrganizationsPublication(t *testing.T) {
	eachStore(t, func(t *testing.T, store publications.Store) {
		ctx := context.Background()
		p := mustPut(t, store, sample(orgA, configA, time.Date(2026, 10, 3, 14, 2, 11, 0, time.UTC)))

		if deleted, err := store.Delete(ctx, orgB, p.ID); err != nil || deleted {
			t.Fatalf("Delete as B = %v, %v; want false", deleted, err)
		}
		if _, err := store.Get(ctx, orgA, p.ID); err != nil {
			t.Fatalf("A's publication didn't survive B's delete: %v", err)
		}
		if deleted, err := store.Delete(ctx, orgA, p.ID); err != nil || !deleted {
			t.Fatalf("Delete = %v, %v; want true", deleted, err)
		}
		if deleted, err := store.Delete(ctx, orgA, p.ID); err != nil || deleted {
			t.Fatalf("a second Delete = %v, %v; want false", deleted, err)
		}
		if _, err := store.Get(ctx, orgA, p.ID); !errors.Is(err, publications.ErrNotFound) {
			t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
		}
	})
}

func TestDeleteForConfigurationRemovesExactlyThatConfigurationsRowsInTheOrganization(t *testing.T) {
	eachStore(t, func(t *testing.T, store publications.Store) {
		ctx := context.Background()
		base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
		for range 3 {
			mustPut(t, store, sample(orgA, configA, base))
		}
		keepOther := mustPut(t, store, sample(orgA, configB, base))
		keepTheirs := mustPut(t, store, sample(orgB, configA, base))

		deleted, err := store.DeleteForConfiguration(ctx, orgA, configA)
		if err != nil || deleted != 3 {
			t.Fatalf("DeleteForConfiguration = %d, %v; want 3", deleted, err)
		}
		left, err := store.List(ctx, orgA, configA, 100, nil)
		if err != nil || len(left.Publications) != 0 {
			t.Fatalf("after the delete, A's configuration lists %d, %v", len(left.Publications), err)
		}
		for label, kept := range map[string]publications.Publication{
			"A's other configuration's":         keepOther,
			"B's, of the same configuration id": keepTheirs,
		} {
			if _, err := store.Get(ctx, kept.OrganizationID, kept.ID); err != nil {
				t.Errorf("%s publication didn't survive: %v", label, err)
			}
		}

		deleted, err = store.DeleteForConfiguration(ctx, orgA, configA)
		if err != nil || deleted != 0 {
			t.Fatalf("a second DeleteForConfiguration = %d, %v; want 0", deleted, err)
		}
	})
}

func TestACursorRoundTripsAndNothingElseParses(t *testing.T) {
	id := uuid.MustParse("1a1a1a1a-0000-4000-8000-00000000001a")
	// A stored time, its first and its last possible value: each comes back.
	for _, publishedAt := range []time.Time{
		time.Date(2026, 10, 3, 14, 2, 11, 123000000, time.UTC),
		time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(9999, 12, 31, 23, 59, 59, 999000000, time.UTC),
	} {
		cursor := publications.Cursor{PublishedAt: publishedAt, ID: id}
		parsed, err := publications.ParseCursor(cursor.String())
		if err != nil || !parsed.PublishedAt.Equal(cursor.PublishedAt) || parsed.ID != cursor.ID {
			t.Fatalf("ParseCursor(String()) = %+v, %v; want %+v", parsed, err, cursor)
		}
	}
	valid := publications.Cursor{PublishedAt: time.Date(2026, 10, 3, 14, 2, 11, 123000000, time.UTC), ID: id}.String()
	bad := []string{
		"", "not a cursor", valid[:len(valid)-1], valid + "A", valid + "=", strings.ToUpper(valid) + "!",
		"2026-10-03T14:02:11Z|1a1a1a1a-0000-4000-8000-00000000001a",
		// A time in the year -7537, out of timestamptz's range.
		"-9YvludiAAAAAAAAAAAAAAAAAAAAAAAA",
	}
	// Times no stored publication has: finer than a millisecond, or outside
	// the years 0001 to 9999.
	for _, publishedAt := range []time.Time{
		time.Date(2026, 10, 3, 14, 2, 11, 123456000, time.UTC),
		time.Date(2026, 10, 3, 14, 2, 11, 123001000, time.UTC),
		time.Date(0, 12, 31, 23, 59, 59, 999000000, time.UTC),
		time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
	} {
		bad = append(bad, publications.Cursor{PublishedAt: publishedAt, ID: id}.String())
	}
	for _, cursor := range bad {
		if _, err := publications.ParseCursor(cursor); !errors.Is(err, publications.ErrInvalidCursor) {
			t.Errorf("ParseCursor(%q) = %v, want ErrInvalidCursor", cursor, err)
		}
	}
}

func TestInRangeIsTheYears0001To9999InUTC(t *testing.T) {
	plusOne := time.FixedZone("+01:00", 3600)
	for _, c := range []struct {
		t    time.Time
		want bool
	}{
		{time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC), true},
		{time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC), true},
		{time.Date(0, 12, 31, 23, 59, 59, 999999999, time.UTC), false},
		{time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), false},
		// The year 0001 here, but 0000 in UTC.
		{time.Date(1, 1, 1, 0, 30, 0, 0, plusOne), false},
	} {
		if got := publications.InRange(c.t); got != c.want {
			t.Errorf("InRange(%v) = %v, want %v", c.t, got, c.want)
		}
	}
}

// A publication belongs to a registered organization. A new id under one
// that isn't is ErrOrganizationNotFound and stores nothing; an id already
// taken is the conflict it always was, whatever the organization.
func TestPutUnderAnUnregisteredOrganizationIsRefused(t *testing.T) {
	eachStore(t, func(t *testing.T, store publications.Store) {
		ctx := context.Background()
		p := sample(orgC, configA, time.Date(2026, 10, 3, 14, 2, 11, 0, time.UTC))
		result, stored, err := store.Put(ctx, p)
		if !errors.Is(err, publications.ErrOrganizationNotFound) || result != 0 || stored != (publications.Publication{}) {
			t.Fatalf("Put under an unregistered organization = %v, %+v, %v; want ErrOrganizationNotFound", result, stored, err)
		}
		if _, err := store.Get(ctx, orgC, p.ID); !errors.Is(err, publications.ErrNotFound) {
			t.Fatalf("Get after the refused Put = %v, want ErrNotFound", err)
		}
		if page, err := store.List(ctx, orgC, configA, 100, nil); err != nil || len(page.Publications) != 0 {
			t.Fatalf("List after the refused Put = %d, %v; want none", len(page.Publications), err)
		}

		// A's id, put under the unregistered organization: a conflict.
		theirs := mustPut(t, store, sample(orgA, configA, time.Date(2026, 10, 3, 14, 2, 11, 0, time.UTC)))
		taken := theirs
		taken.OrganizationID = orgC
		if result, _, err := store.Put(ctx, taken); err != nil || result != publications.Conflict {
			t.Fatalf("Put of A's id under an unregistered organization = %v, %v; want conflict", result, err)
		}
	})
}

// The names a published version references, written by Put at publish only.

// namesStore is a store and what reads back, under it, the names it wrote.
type namesStore struct {
	publications.Store
	// names is the organization's published version's names, sorted.
	names func(t *testing.T, organizationID, publicationID uuid.UUID) []string
	// allNames counts every name row, of every organization.
	allNames func(t *testing.T) int
	// undefined is variableuse's Undefined, in its own transaction.
	undefined func(t *testing.T, organizationID, publicationID uuid.UUID) variableuse.UndefinedNames
	// d is the Postgres store's databases.
	d *databasetest.Databases
}

// eachNamesStore runs test against the Postgres store, as eachStore does,
// with what reads its names back.
func eachNamesStore(t *testing.T, test func(t *testing.T, s namesStore)) {
	t.Run("postgres", func(t *testing.T) {
		d := databasetest.New(t, testDatabase)
		for _, id := range []uuid.UUID{orgA, orgB} {
			if _, err := d.Owner.Exec(context.Background(), `
				insert into telflo_opamp.organizations (id, name, created_at, updated_at, generation)
				values ($1, 'an organization', $2, $2, gen_random_uuid())`, id, received); err != nil {
				t.Fatal(err)
			}
		}
		ctx := context.Background()
		test(t, namesStore{
			Store: publications.NewPostgres(d.Runtime),
			names: func(t *testing.T, organizationID, publicationID uuid.UUID) []string {
				t.Helper()
				rows, err := d.Owner.Query(ctx, `
					select name from telflo_opamp.publication_variable_names
					 where organization_id = $1 and publication_id = $2
					 order by name collate "C"`, organizationID, publicationID)
				if err != nil {
					t.Fatal(err)
				}
				names, err := pgx.CollectRows(rows, pgx.RowTo[string])
				if err != nil {
					t.Fatal(err)
				}
				return names
			},
			allNames: func(t *testing.T) (n int) {
				t.Helper()
				if err := d.Owner.QueryRow(ctx, `select count(*) from telflo_opamp.publication_variable_names`).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			},
			undefined: func(t *testing.T, organizationID, publicationID uuid.UUID) variableuse.UndefinedNames {
				t.Helper()
				tx, err := d.Runtime.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(ctx) }()
				u, err := variableuse.Undefined(ctx, tx, organizationID, publicationID)
				if err != nil {
					t.Fatal(err)
				}
				return u
			},
			d: d,
		})
	})
}

// withYAML is p with yaml as its text.
func withYAML(p publications.Publication, yaml string) publications.Publication {
	p.YAML = yaml
	return p
}

// Only live references are names: an escaped one, the legacy form and a
// default (`${org:X:-d}`, not the grammar) are left out, and a name
// referenced twice is stored once.
func TestPutStoresTheNamesItsYAMLReferences(t *testing.T) {
	eachNamesStore(t, func(t *testing.T, s namesStore) {
		yaml := "exporters:\n" +
			"  otlp:\n" +
			"    endpoint: ${org:OTLP_ENDPOINT}\n" +
			"    escaped: $${org:ESCAPED}\n" +
			"    legacy: $orgvar:LEGACY\n" +
			"    defaulted: ${org:DEFAULTED:-d}\n" +
			"    again: ${org:OTLP_ENDPOINT}/v1\n" +
			"    region: ${org:region_2}\n" +
			"    env: ${env:HOME}\n"
		p := mustPut(t, s, withYAML(sample(orgA, configA, received), yaml))
		if got, want := s.names(t, orgA, p.ID), []string{"OTLP_ENDPOINT", "region_2"}; !slices.Equal(got, want) {
			t.Fatalf("the stored names %q, want %q", got, want)
		}
		if n := s.allNames(t); n != 2 {
			t.Fatalf("%d name rows in all, want 2", n)
		}

		// A version with no live reference stores none.
		plain := mustPut(t, s, sample(orgA, configA, received.Add(time.Minute)))
		if got := s.names(t, orgA, plain.ID); len(got) != 0 {
			t.Fatalf("a version with no reference stored names %q", got)
		}
		if n := s.allNames(t); n != 2 {
			t.Fatalf("%d name rows in all, want 2", n)
		}
	})
}

// A Put that stores nothing stores no names: Unchanged, Conflict (another
// text, or another organization's id). On Postgres, a Put racing another
// insert of the same id waits for it and then inserts nothing, names
// included, and raises nothing; when the other rolls back, it inserts both.
func TestAPutThatStoresNothingStoresNoNames(t *testing.T) {
	eachNamesStore(t, func(t *testing.T, s namesStore) {
		ctx := context.Background()
		p := mustPut(t, s, withYAML(sample(orgA, configA, received), "a: ${org:FIRST}\n"))

		// The same publication again: Unchanged, its names as they were.
		if result, _, err := s.Put(ctx, p); err != nil || result != publications.Unchanged {
			t.Fatalf("the identical Put = %v, %v; want unchanged", result, err)
		}
		// The same id with a text of other names: a Conflict, no names.
		if result, _, err := s.Put(ctx, withYAML(p, "a: ${org:SECOND}\n")); err != nil || result != publications.Conflict {
			t.Fatalf("the conflicting Put = %v, %v; want conflict", result, err)
		}
		// A's id under B: a Conflict, and B holds no names.
		theirs := withYAML(p, "a: ${org:THIRD}\n")
		theirs.OrganizationID = orgB
		if result, _, err := s.Put(ctx, theirs); err != nil || result != publications.Conflict {
			t.Fatalf("B's Put of A's id = %v, %v; want conflict", result, err)
		}
		if got := s.names(t, orgA, p.ID); !slices.Equal(got, []string{"FIRST"}) {
			t.Fatalf("A's names after the refused Puts %q, want [FIRST]", got)
		}
		if got := s.names(t, orgB, p.ID); len(got) != 0 {
			t.Fatalf("B holds names %q under A's id", got)
		}
		if n := s.allNames(t); n != 1 {
			t.Fatalf("%d name rows in all, want 1", n)
		}

		// The race, both ways: another transaction inserts the identical
		// publication with no names and holds it; the Put waits on its key.
		for _, commit := range []bool{true, false} {
			racing := withYAML(sample(orgA, configA, received.Add(time.Hour)), "a: ${org:RACE}\nb: ${org:RACE}\n")
			tx, err := s.d.Runtime.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `
				insert into telflo_opamp.publications (id, organization_id, configuration_id, configuration_name,
				  version_id, version_number, version_label, yaml, sha256, published_by_id, published_by_email,
				  published_at, received_at, collector_distribution, collector_version)
				values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
				racing.ID, racing.OrganizationID, racing.ConfigurationID, racing.ConfigurationName,
				racing.VersionID, racing.VersionNumber, racing.VersionLabel, racing.YAML, racing.SHA256,
				racing.PublishedBy.ID, racing.PublishedBy.Email, racing.PublishedAt, racing.ReceivedAt,
				racing.Collector.Distribution, racing.Collector.Version); err != nil {
				t.Fatal(err)
			}
			type answer struct {
				result publications.PutResult
				err    error
			}
			answered := make(chan answer, 1)
			go func() {
				result, _, err := s.Put(ctx, racing)
				answered <- answer{result, err}
			}()
			waitForALockWaiter(t, s.d)
			if commit {
				err = tx.Commit(ctx)
			} else {
				err = tx.Rollback(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			got := <-answered
			want, wantNames := publications.Unchanged, []string(nil)
			if !commit {
				want, wantNames = publications.Created, []string{"RACE"}
			}
			if got.err != nil || got.result != want {
				t.Fatalf("the racing Put (the other committed: %t) = %v, %v; want %v and no error (no 23505)", commit, got.result, got.err, want)
			}
			if names := s.names(t, orgA, racing.ID); !slices.Equal(names, wantNames) {
				t.Fatalf("the racing Put (the other committed: %t) left names %q, want %q", commit, names, wantNames)
			}
		}
	})
}

// waitForALockWaiter waits until a session of the test database waits on a
// lock, or fails the test after a generous hang guard.
func waitForALockWaiter(t *testing.T, d *databasetest.Databases) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for {
		var waiting int
		if err := d.Admin.QueryRow(ctx, `
			select count(*) from pg_stat_activity
			 where datname = $1 and wait_event_type = 'Lock'`, d.Name).Scan(&waiting); err != nil {
			t.Fatalf("waiting for a session to wait on a lock: %v", err)
		}
		if waiting > 0 {
			return
		}
	}
}

// Under an unregistered organization, Put answers ErrOrganizationNotFound
// whichever key refuses it: the publication's (it fires first), or the
// names' (on Postgres, with the publication's key's triggers turned off
// for the test, so the names' key is the one that finds the organization
// missing). Nothing is stored.
func TestAPutForAnUnregisteredOrganizationIsOrganizationNotFound(t *testing.T) {
	eachNamesStore(t, func(t *testing.T, s namesStore) {
		ctx := context.Background()
		check := func(by string) {
			t.Helper()
			p := withYAML(sample(orgC, configA, received), "a: ${org:SOME_NAME}\n")
			if result, stored, err := s.Put(ctx, p); !errors.Is(err, publications.ErrOrganizationNotFound) ||
				result != 0 || stored != (publications.Publication{}) {
				t.Fatalf("Put under an unregistered organization (%s) = %v, %+v, %v; want ErrOrganizationNotFound", by, result, stored, err)
			}
			if _, err := s.Get(ctx, orgC, p.ID); !errors.Is(err, publications.ErrNotFound) {
				t.Fatalf("Get after the refused Put (%s) = %v, want ErrNotFound", by, err)
			}
			if n := s.allNames(t); n != 0 {
				t.Fatalf("the refused Put (%s) left %d name rows", by, n)
			}
		}
		check("the publication's key")
		var ref string
		if err := s.d.Admin.QueryRow(ctx, `
			select confrelid::regclass::text from pg_constraint
			 where conname = 'fk_publication_variable_names_organization'`).Scan(&ref); err != nil || ref != "telflo_opamp.organizations" {
			t.Fatalf("the names' key to the organization: %q, %v", ref, err)
		}
		if _, err := s.d.Admin.Exec(ctx, `alter table telflo_opamp.publications disable trigger all`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := s.d.Admin.Exec(context.Background(), `alter table telflo_opamp.publications enable trigger all`); err != nil {
				t.Errorf("turning the publications' triggers back on: %v", err)
			}
		})
		check("the names' key")
	})
}

// A live reference to a name over 128 characters, which no variable can
// have, is refused before anything is written, a 129-character one (the
// corpus row name_over_128_characters_is_a_live_reference) and an
// incompressible one of 3,000 characters alike (which would otherwise
// overflow the names' index entry limit, a 54000). A 128-character name
// publishes, and Undefined lists it.
func TestAVersionReferencingANameOver128CharactersIsRefused(t *testing.T) {
	longRow := corpusRenderRow(t, "name_over_128_characters_is_a_live_reference")
	names := orgvariables.References(longRow)
	if len(names) != 1 || len(names[0]) != 129 {
		t.Fatalf("the corpus row's references %q: want one name of 129 characters", names)
	}
	random := rand.New(rand.NewPCG(20, 51))
	const grammar = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_"
	incompressible := []byte{'N'}
	for len(incompressible) < 3000 {
		incompressible = append(incompressible, grammar[random.IntN(len(grammar))])
	}
	if got := orgvariables.References("a: ${org:" + string(incompressible) + "}\n"); len(got) != 1 || got[0] != string(incompressible) {
		t.Fatalf("the incompressible name isn't a live reference: %d names", len(got))
	}

	eachNamesStore(t, func(t *testing.T, s namesStore) {
		ctx := context.Background()
		for label, yaml := range map[string]string{
			"129 characters (the corpus row)": longRow,
			"3,000 incompressible characters": "a: ${org:SHORT}\nb: ${org:" + string(incompressible) + "}\n",
		} {
			p := withYAML(sample(orgA, configA, received), yaml)
			if result, stored, err := s.Put(ctx, p); !errors.Is(err, publications.ErrReferenceTooLong) ||
				result != 0 || stored != (publications.Publication{}) {
				t.Fatalf("Put referencing %s = %v, %+v, %v; want ErrReferenceTooLong", label, result, stored, err)
			}
			if _, err := s.Get(ctx, orgA, p.ID); !errors.Is(err, publications.ErrNotFound) {
				t.Fatalf("Get after the refused Put (%s) = %v, want ErrNotFound", label, err)
			}
			if n := s.allNames(t); n != 0 {
				t.Fatalf("the refused Put (%s) left %d name rows", label, n)
			}
		}

		name128 := "a_" + strings.Repeat("b", 126)
		p := mustPut(t, s, withYAML(sample(orgA, configA, received), "a: ${org:"+name128+"}\n"))
		if got := s.names(t, orgA, p.ID); !slices.Equal(got, []string{name128}) {
			t.Fatalf("the 128-character name wasn't stored: %d names", len(got))
		}
		if got := s.undefined(t, orgA, p.ID); got.Total != 1 || !slices.Equal(got.Names, []string{name128}) {
			t.Fatalf("Undefined = %d names, total %d; want the 128-character name", len(got.Names), got.Total)
		}
	})
}

// corpusRenderRow is the text of the shared corpus's render row name.
func corpusRenderRow(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "handlers", "testdata", "org-token-corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Render []struct {
			Name string `json:"name"`
			Text string `json:"text"`
		} `json:"render"`
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	for _, row := range corpus.Render {
		if row.Name == name {
			return row.Text
		}
	}
	t.Fatalf("the corpus has no render row %s", name)
	return ""
}

// Deleting a published version deletes its names, and only its own.
func TestDeletingAPublicationDeletesItsNames(t *testing.T) {
	eachNamesStore(t, func(t *testing.T, s namesStore) {
		ctx := context.Background()
		gone := mustPut(t, s, withYAML(sample(orgA, configA, received), "a: ${org:GONE}\nb: ${org:KEPT}\n"))
		kept := mustPut(t, s, withYAML(sample(orgA, configB, received), "b: ${org:KEPT}\n"))
		theirs := mustPut(t, s, withYAML(sample(orgB, configA, received), "a: ${org:GONE}\n"))

		if deleted, err := s.Delete(ctx, orgB, gone.ID); err != nil || deleted {
			t.Fatalf("B's Delete of A's version = %v, %v", deleted, err)
		}
		if got := s.names(t, orgA, gone.ID); len(got) != 2 {
			t.Fatalf("B's delete took A's names: %q", got)
		}
		if deleted, err := s.Delete(ctx, orgA, gone.ID); err != nil || !deleted {
			t.Fatalf("Delete = %v, %v", deleted, err)
		}
		if got := s.names(t, orgA, gone.ID); len(got) != 0 {
			t.Fatalf("the deleted version's names remain: %q", got)
		}
		if got := s.names(t, orgA, kept.ID); !slices.Equal(got, []string{"KEPT"}) {
			t.Fatalf("another version's names %q, want [KEPT]", got)
		}
		if got := s.names(t, orgB, theirs.ID); !slices.Equal(got, []string{"GONE"}) {
			t.Fatalf("B's names %q, want [GONE]", got)
		}

		// A configuration's delete takes its versions' names too.
		if deleted, err := s.DeleteForConfiguration(ctx, orgA, configB); err != nil || deleted != 1 {
			t.Fatalf("DeleteForConfiguration = %v, %v", deleted, err)
		}
		if n := s.allNames(t); n != 1 {
			t.Fatalf("%d name rows left, want B's one", n)
		}
	})
}
