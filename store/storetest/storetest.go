// Package storetest runs the fleet model's contract tests against its
// Postgres stores, on a fresh, migrated database, as telflo_opamp_server (the
// server's role, with its real grants). Only tests import it.
//
// Each test gets two registered organizations, A and B, each holding one
// published version, and helpers that reach under the stores: inserting a
// collector directly (without the collectors' store), reading a collector's
// group, counting a fleet's rows, and dumping an organization's rows to show
// a call changed nothing.
package storetest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/telflo/opamp-server/store/apikeys"
	"github.com/telflo/opamp-server/store/collectorgroups"
	"github.com/telflo/opamp-server/store/collectors"
	"github.com/telflo/opamp-server/store/database"
	"github.com/telflo/opamp-server/store/database/databasetest"
	"github.com/telflo/opamp-server/store/enrollmenttokens"
	"github.com/telflo/opamp-server/store/fleets"
	"github.com/telflo/opamp-server/store/organizations"
	"github.com/telflo/opamp-server/store/publications"
	"github.com/telflo/opamp-server/store/variables"
)

// The fixture's ids and clock.
var (
	OrgA = uuid.MustParse("0a0a0a0a-0000-4000-8000-00000000000a")
	OrgB = uuid.MustParse("0b0b0b0b-0000-4000-8000-00000000000b")
	// OrgUnregistered is registered nowhere.
	OrgUnregistered = uuid.MustParse("0c0c0c0c-0000-4000-8000-00000000000c")
	// PubA and PubB are A's and B's published versions.
	PubA = uuid.MustParse("aaaaaaaa-1111-4000-8000-00000000000a")
	PubB = uuid.MustParse("bbbbbbbb-1111-4000-8000-00000000000b")
	// Now is a test clock's reading, finer than a millisecond.
	Now = time.Date(2026, 10, 12, 9, 30, 0, 123456789, time.UTC)
)

// Millisecond is t as a store keeps it.
func Millisecond(t time.Time) time.Time {
	return t.UTC().Truncate(time.Millisecond)
}

// Stores is every store of the fleet model on one database, and the
// helpers that reach under them.
type Stores struct {
	Organizations organizations.Store
	Publications  publications.Store
	Fleets        fleets.Store
	Groups        collectorgroups.Store
	Tokens        enrollmenttokens.Store
	Collectors    collectors.Store
	Variables     variables.Store
	// APIKeys is the management API's keys' store, on the same database.
	APIKeys apikeys.Store

	// Databases are the stores' database, reachable as each role.
	Databases *databasetest.Databases
}

// Option configures the stores Each makes.
type Option func(*options)

type options struct {
	collectors []collectors.Option
}

// WithCollectors passes opts to the collectors store.
func WithCollectors(opts ...collectors.Option) Option {
	return func(o *options) { o.collectors = append(o.collectors, opts...) }
}

// Each runs test against the Postgres stores on the database name (which
// must end in _test), in a subtest named "postgres".
func Each(t *testing.T, name string, test func(t *testing.T, s *Stores), opt ...Option) {
	t.Run("postgres", func(t *testing.T) {
		test(t, Postgres(t, name, opt...))
	})
}

// Postgres is every store on a fresh, migrated database name (which must end
// in _test), as telflo_opamp_server (the server's role, with its real grants),
// seeded with A and B. It skips the test when OPAMP_TEST_ADMIN_DATABASE_URL
// is unset.
func Postgres(t *testing.T, name string, opt ...Option) *Stores {
	t.Helper()
	var opts options
	for _, o := range opt {
		o(&opts)
	}
	d := databasetest.New(t, name)
	s := &Stores{
		Organizations: organizations.NewPostgres(d.Runtime),
		Publications:  publications.NewPostgres(d.Runtime),
		Fleets:        fleets.NewPostgres(d.Runtime),
		Groups:        collectorgroups.NewPostgres(d.Runtime),
		Tokens:        enrollmenttokens.NewPostgres(d.Runtime),
		Collectors:    collectors.NewPostgres(d.Runtime, opts.collectors...),
		Variables:     variables.NewPostgres(d.Runtime),
		APIKeys:       apikeys.NewPostgres(d.Runtime),
		Databases:     d,
	}
	s.seed(t)
	return s
}

// seed registers A and B, each with its published version.
func (s *Stores) seed(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, org := range []struct {
		id, publication uuid.UUID
	}{{OrgA, PubA}, {OrgB, PubB}} {
		if result, _, err := s.Organizations.Create(ctx, organizations.Organization{
			ID: org.id, Name: "organization " + org.id.String(), CreatedAt: Now, UpdatedAt: Now,
		}); err != nil || result != organizations.Created {
			t.Fatalf("registering %s: %v, %v", org.id, result, err)
		}
		if result, _, err := s.Publications.Put(ctx, publications.Publication{
			ID: org.publication, OrganizationID: org.id, ConfigurationID: uuid.New(),
			ConfigurationName: "payments-gateway", VersionID: uuid.New(), VersionNumber: 3,
			YAML: "receivers: {}\n", SHA256: strings.Repeat("ab", 32), PublishedAt: Now, ReceivedAt: Now,
			Collector: publications.Collector{Distribution: "otelcol-contrib", Version: "0.162.0"},
		}); err != nil || result != publications.Created {
			t.Fatalf("publishing for %s: %v, %v", org.id, result, err)
		}
	}
}

// Collector is what InsertCollector needs of a collector.
type Collector struct {
	ID, OrganizationID, FleetID, GroupID uuid.UUID
	Name                                 string
	TokenID                              *uuid.UUID
	PinnedPublicationID                  *uuid.UUID
	// PinnedVariableGroupID is a variable group pinned on it, with no stamp:
	// the fleet stores' writes stamp it.
	PinnedVariableGroupID *uuid.UUID
	LastSeenAt            time.Time
}

// InsertCollector inserts a collector as the server's role would, its times
// at its LastSeenAt.
func (s *Stores) InsertCollector(t *testing.T, c Collector) {
	t.Helper()
	seen := Millisecond(c.LastSeenAt)
	if _, err := s.Databases.Runtime.Exec(context.Background(), `
		insert into opamp_server.collector_instances (id, organization_id, fleet_id, collector_group_id,
		  instance_uid, name, pinned_publication_id, enrollment_token_id, pinned_variable_group_id,
		  last_seen_at, created_at, updated_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10, $10)`,
		c.ID, c.OrganizationID, c.FleetID, c.GroupID, uuid.New(), c.Name, c.PinnedPublicationID, c.TokenID,
		c.PinnedVariableGroupID, seen); err != nil {
		t.Fatalf("inserting collector %s: %v", c.Name, err)
	}
}

// CollectorGroup returns the group of the organization's collector id and
// its UpdatedAt.
func (s *Stores) CollectorGroup(t *testing.T, organizationID, id uuid.UUID) (uuid.UUID, time.Time) {
	t.Helper()
	var group uuid.UUID
	var updatedAt time.Time
	if err := s.Databases.Owner.QueryRow(context.Background(), `
		select collector_group_id, updated_at from opamp_server.collector_instances
		 where organization_id = $1 and id = $2`, organizationID, id).Scan(&group, &updatedAt); err != nil {
		t.Fatalf("reading collector %s: %v", id, err)
	}
	return group, updatedAt.UTC()
}

// FleetRows counts, table by table, the rows of the fleet id: what its
// delete must take.
func (s *Stores) FleetRows(t *testing.T, fleetID uuid.UUID) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, table := range []string{"fleets", "collector_groups", "enrollment_tokens", "enrollment_token_events", "collector_instances"} {
		column := "fleet_id"
		if table == "fleets" {
			column = "id"
		}
		var n int
		if err := s.Databases.Owner.QueryRow(context.Background(),
			"select count(*) from opamp_server."+table+" where "+column+" = $1", fleetID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		counts[table] = n
	}
	return counts
}

// Dump is every row the organization holds, in every table, as text in a
// stable order: compared before and after a call, it shows the call
// changed nothing of the organization's. It walks the catalogue (every
// table of opamp_server but goose's and the server's own, as the schema
// tests' organizationRowsByTable does), so a new table can't be missed; a
// table without organization_id fails the test.
func (s *Stores) Dump(t *testing.T, organizationID uuid.UUID) string {
	t.Helper()
	var lines []string
	ctx := context.Background()
	rows, err := s.Databases.Owner.Query(ctx, `
		select c.relname,
		       exists (select 1 from pg_attribute a
		                where a.attrelid = c.oid and a.attname = 'organization_id' and not a.attisdropped)
		  from pg_class c
		 where c.relnamespace = 'opamp_server'::regnamespace and c.relkind = 'r'
		   and c.relname not in ('goose_db_version', '`+database.ServerTable+`')
		 order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	type table struct {
		name              string
		hasOrganizationID bool
	}
	tables, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (table, error) {
		var tb table
		err := row.Scan(&tb.name, &tb.hasOrganizationID)
		return tb, err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tb := range tables {
		column := "organization_id"
		if tb.name == "organizations" {
			column = "id"
		} else if !tb.hasOrganizationID {
			t.Fatalf("opamp_server.%s has no organization_id", tb.name)
		}
		rows, err := s.Databases.Owner.Query(ctx, "select r::text from opamp_server."+pgx.Identifier{tb.name}.Sanitize()+
			" r where "+column+" = $1", organizationID)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range got {
			lines = append(lines, tb.name+" "+row)
		}
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

// Unchanged fails the test when the organization's rows differ from before
// (a Dump), naming what was called.
func (s *Stores) Unchanged(t *testing.T, call string, organizationID uuid.UUID, before string) {
	t.Helper()
	if after := s.Dump(t, organizationID); after != before {
		t.Fatalf("%s changed the organization's rows:\n before:\n%s\n after:\n%s", call, before, after)
	}
}

// Ptr returns a pointer to v.
func Ptr[T any](v T) *T { return &v }

// Describe names a fixture value for a failure message.
func Describe(v any) string { return fmt.Sprintf("%+v", v) }

// Ana is the actor the fixture's changes name.
var Ana = &fleets.Actor{ID: uuid.MustParse("a0a0a0a0-0000-4000-8000-0000000000a0"), Email: "ana@acme.io"}

// CreateFleet creates a fleet of the organization at Now, by Ana, its
// rotate token expiring a year later, and fails the test unless it is
// Created.
func (s *Stores) CreateFleet(t *testing.T, organizationID uuid.UUID, label string, typ fleets.Type) fleets.Creation {
	t.Helper()
	result, created, err := s.Fleets.Create(context.Background(), fleets.NewFleet{
		ID: uuid.New(), OrganizationID: organizationID, Label: label, Type: typ, CreatedBy: Ana,
		RotateExpiresAt: Ptr(Now.AddDate(1, 0, 0)), At: Now,
	})
	if err != nil || result != fleets.Created {
		t.Fatalf("creating fleet %q: %v, %v", label, result, err)
	}
	return created
}

// WaitForLockWaiters waits until n sessions of the Postgres test database
// wait on a lock, or fails the test after a generous hang guard: how a
// two-session test knows both sessions are in the race before it lets
// them go.
func (s *Stores) WaitForLockWaiters(t *testing.T, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for {
		var waiting int
		err := s.Databases.Admin.QueryRow(ctx, `
			select count(*) from pg_stat_activity
			 where datname = $1 and wait_event_type = 'Lock'`, s.Databases.Name).Scan(&waiting)
		if err != nil {
			t.Fatalf("waiting for %d sessions to wait on a lock: %v", n, err)
		}
		if waiting >= n {
			return
		}
	}
}
