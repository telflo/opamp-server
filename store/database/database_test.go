package database_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	"github.com/telflo/opamp-server/migrations"
	"github.com/telflo/opamp-server/store/database"
	"github.com/telflo/opamp-server/store/database/databasetest"
	"github.com/telflo/opamp-server/store/organizations"
)

// This package's own test database (CONTRIBUTING.md, "Tests"). Every test
// that calls databasetest.New gets it dropped and made again.
const testDatabase = "telflo_opamp_server_database_test"

func TestLatestMigrationIsTheNewestEmbeddedFile(t *testing.T) {
	latest, err := database.LatestMigration()
	if err != nil {
		t.Fatal(err)
	}
	// The newest file in migrations/, read from the directory rather than
	// the embedded files LatestMigration reads.
	entries, err := os.ReadDir("../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	var newest int64
	var newestName string
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, err := goose.NumericComponent(entry.Name())
		if err != nil {
			t.Fatalf("migration %s: %v", entry.Name(), err)
		}
		if version > newest {
			newest, newestName = version, entry.Name()
		}
	}
	if latest != newest {
		t.Fatalf("LatestMigration() = %d, want %d (%s)", latest, newest, newestName)
	}
}

// Every embedded migration is ASCII. An editor or a heredoc can turn an
// escape such as \u2028 into the character itself, which a check's regular
// expression then holds literally; any byte of 0x80 or more is named by its
// file and offset.
func TestEveryMigrationIsASCII(t *testing.T) {
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("no embedded migrations")
	}
	for _, name := range names {
		content, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		for offset, b := range content {
			if b >= 0x80 {
				t.Errorf("%s: byte 0x%02x at offset %d is not ASCII", name, b, offset)
				break
			}
		}
	}
}

// latestMigration is the version every test that names a migration number
// derives it from (CONTRIBUTING.md, "Tests"), so a new migration doesn't
// break them.
func latestMigration(t *testing.T) int64 {
	t.Helper()
	latest, err := database.LatestMigration()
	if err != nil {
		t.Fatal(err)
	}
	return latest
}

// versionsUpTo is 1 to version, in order: what an up from empty applies.
func versionsUpTo(version int64) []int64 {
	versions := make([]int64, 0, version)
	for v := int64(1); v <= version; v++ {
		versions = append(versions, v)
	}
	return versions
}

func TestMigrateUpFromAnEmptyDatabaseThenDownAndUpAgain(t *testing.T) {
	d := databasetest.New(t, testDatabase, databasetest.Unmigrated())
	ctx := context.Background()
	latest := latestMigration(t)
	for version := int64(1); version <= latest; version++ {
		if _, ok := migrationSteps[version]; !ok {
			t.Fatalf("migration %d has no entry in migrationSteps: say what it creates and grants", version)
		}
	}
	if got := grantsAt(latest); !sameGrants(got, wantRuntimeGrants) {
		t.Fatalf("migrationSteps grant %v at %d, but wantRuntimeGrants is %v", got, latest, wantRuntimeGrants)
	}

	migrator, err := database.OpenMigrator(ctx, d.OwnerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = migrator.Close() }()

	results, err := migrator.Up(ctx)
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if got := appliedVersions(results); !slices.Equal(got, versionsUpTo(latest)) {
		t.Fatalf("up applied %v, want %v", got, versionsUpTo(latest))
	}
	assertStatus(t, migrator, latest)
	assertTablesAt(t, d.Owner, latest)
	// migrate made the schema, as the owner, and goose's version table in it.
	var schemaOwner, versionTableOwner string
	if err := d.Admin.QueryRow(ctx, `
		select pg_get_userbyid(n.nspowner), pg_get_userbyid(c.relowner)
		  from pg_namespace n join pg_class c on c.relnamespace = n.oid
		 where n.nspname = 'opamp_server' and c.relname = 'goose_db_version'`,
	).Scan(&schemaOwner, &versionTableOwner); err != nil {
		t.Fatal(err)
	}
	if schemaOwner != database.OwnerRole || versionTableOwner != database.OwnerRole {
		t.Fatalf("schema owned by %s and version table by %s, want both %s",
			schemaOwner, versionTableOwner, database.OwnerRole)
	}
	// Every table is the owner's, so the server's role owns nothing it could
	// act on above its grants.
	var notOwned []string
	rows, err := d.Admin.Query(ctx, `
		select c.relname from pg_class c join pg_namespace n on n.oid = c.relnamespace
		 where n.nspname = 'opamp_server' and pg_get_userbyid(c.relowner) <> $1
		 order by 1`, database.OwnerRole)
	if err != nil {
		t.Fatal(err)
	}
	if notOwned, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	if len(notOwned) != 0 {
		t.Fatalf("not owned by %s: %v", database.OwnerRole, notOwned)
	}

	// Down rolls back one migration at a time, each dropping what its Up made
	// and revoking every grant its Up made, through 00003 and on to an empty
	// schema.
	for version := latest; version >= 1; version-- {
		result, err := migrator.Down(ctx)
		if err != nil {
			t.Fatalf("down from %d: %v", version, err)
		}
		if result.Source.Version != version {
			t.Fatalf("down rolled back %d, want %d", result.Source.Version, version)
		}
		assertStatus(t, migrator, version-1)
		assertTablesAt(t, d.Owner, version-1)
		if got := runtimeGrants(t, d.Admin, database.RuntimeRole); !sameGrants(got, grantsAt(version-1)) {
			t.Fatalf("after rolling back %d, %s holds %v, want %v", version, database.RuntimeRole, got, grantsAt(version-1))
		}
	}

	results, err = migrator.Up(ctx)
	if err != nil {
		t.Fatalf("up again: %v", err)
	}
	if got := appliedVersions(results); !slices.Equal(got, versionsUpTo(latest)) {
		t.Fatalf("up again applied %v, want %v", got, versionsUpTo(latest))
	}
	assertStatus(t, migrator, latest)
	assertTablesAt(t, d.Owner, latest)
	if got := runtimeGrants(t, d.Admin, database.RuntimeRole); !sameGrants(got, wantRuntimeGrants) {
		t.Fatalf("after up again, %s holds %v, want %v", database.RuntimeRole, got, wantRuntimeGrants)
	}
}

// Migration 00002 is expand-only. Publications stored before it keep their
// organizations, which it registers first, named by their ids' text and
// dated by their first publication's received_at, then adds the foreign key
// over them. Down drops the registry and the key and leaves every
// publication, with the table's comments as 00001 wrote them; up again
// registers them again.
func TestMigration2RegistersTheOrganizationsOfExistingPublications(t *testing.T) {
	d := databasetest.New(t, testDatabase, databasetest.Unmigrated())
	ctx := context.Background()
	migrator, err := database.OpenMigrator(ctx, d.OwnerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = migrator.Close() }()
	if _, err := database.MigrateUpTo(ctx, migrator, 1); err != nil {
		t.Fatalf("up to 1: %v", err)
	}
	assertStatus(t, migrator, 1)
	comments1 := publicationsComments(t, d.Admin)

	// Two organizations' publications, as a release before 00002 stored
	// them: X's first received at 10:00 (stored second), Y's at 11:00.
	const (
		orgX = "0a0a0a0a-0000-4000-8000-00000000000a"
		orgY = "0b0b0b0b-0000-4000-8000-00000000000b"
	)
	for _, row := range []struct{ organization, receivedAt string }{
		{orgX, "2026-10-01T12:00:00.000Z"},
		{orgX, "2026-10-01T10:00:00.000Z"},
		{orgY, "2026-10-02T11:00:00.000Z"},
	} {
		if _, err := d.Owner.Exec(ctx, `
			insert into opamp_server.publications (id, organization_id, configuration_id, configuration_name,
			  version_id, version_number, version_label, yaml, sha256, published_by_id, published_by_email,
			  published_at, received_at)
			values (gen_random_uuid(), $1, gen_random_uuid(), 'payments-gateway', gen_random_uuid(), 1, null,
			  'receivers: {}', repeat('ab', 32), null, null, $2, $2)`,
			row.organization, row.receivedAt); err != nil {
			t.Fatal(err)
		}
	}
	before := tableRows(t, d.Admin, "publications")

	wantOrganizations := []string{
		orgX + " named " + orgX + ", created 2026-10-01 10:00:00.000, updated 2026-10-01 10:00:00.000",
		orgY + " named " + orgY + ", created 2026-10-02 11:00:00.000, updated 2026-10-02 11:00:00.000",
	}
	for round := 1; round <= 2; round++ {
		results, err := database.MigrateUpTo(ctx, migrator, 2)
		if err != nil {
			t.Fatalf("round %d: up: %v", round, err)
		}
		if got := appliedVersions(results); !slices.Equal(got, []int64{2}) {
			t.Fatalf("round %d: up applied %v, want [2]", round, got)
		}
		if got := organizationRows(t, d.Admin); !slices.Equal(got, wantOrganizations) {
			t.Fatalf("round %d: organizations %v, want %v", round, got, wantOrganizations)
		}
		if got := tableRows(t, d.Admin, "publications"); !slices.Equal(got, before) {
			t.Fatalf("round %d: the publications changed:\n before %v\n after  %v", round, before, got)
		}
		if all, _ := foreignKeys(t, d.Admin); !slices.Equal(all, []string{"fk_publications_organization"}) {
			t.Fatalf("round %d: foreign keys %v, want the organization's", round, all)
		}

		result, err := migrator.Down(ctx)
		if err != nil || result.Source.Version != 2 {
			t.Fatalf("round %d: down = %v, %v; want 2 rolled back", round, result, err)
		}
		assertTableExists(t, d.Owner, "organizations", false)
		if all, _ := foreignKeys(t, d.Admin); len(all) != 0 {
			t.Fatalf("round %d: after down, foreign keys %v", round, all)
		}
		if got := tableRows(t, d.Admin, "publications"); !slices.Equal(got, before) {
			t.Fatalf("round %d: down changed the publications:\n before %v\n after  %v", round, before, got)
		}
		if got := publicationsComments(t, d.Admin); !slices.Equal(got, comments1) {
			t.Fatalf("round %d: after down, the publications comments are\n %v\nwant 00001's\n %v", round, got, comments1)
		}
		if got := runtimeGrants(t, d.Admin, database.RuntimeRole); !sameGrants(got, grantsAt(1)) {
			t.Fatalf("round %d: after down, %s holds %v", round, database.RuntimeRole, got)
		}
	}
}

// Migration 00003 is expand-only. Publications stored before it read
// OpenTelemetry Collector Contrib 0.162.0, every other column unchanged; a
// server of the previous release, which inserts without the two columns,
// keeps storing publications (the defaults); a collector written explicitly
// is kept as written; the checks are openapi.yaml's formats; the runtime
// role's grants don't change. Down drops the two columns, leaving every
// publication as 00002 had it, with 00002's comments; up again fills them
// again. It migrates to 00003 only: 00004 drops the defaults.
func TestMigration3GivesExistingPublicationsTheContribCollector(t *testing.T) {
	d := databasetest.New(t, testDatabase, databasetest.Unmigrated())
	ctx := context.Background()
	migrator, err := database.OpenMigrator(ctx, d.OwnerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = migrator.Close() }()
	if _, err := database.MigrateUpTo(ctx, migrator, 2); err != nil {
		t.Fatalf("up to 2: %v", err)
	}
	assertStatus(t, migrator, 2)
	comments2 := publicationsComments(t, d.Admin)

	// Two publications, as a release before 00003 stored them.
	const orgX = "0a0a0a0a-0000-4000-8000-00000000000a"
	if _, err := d.Owner.Exec(ctx, `
		insert into opamp_server.organizations (id, name, created_at, updated_at)
		values ($1, 'an organization', '2026-10-01T10:00:00Z', '2026-10-01T10:00:00Z')`, orgX); err != nil {
		t.Fatal(err)
	}
	// previousRelease is the insert a server of the previous release makes:
	// every column 00001 made, and not the collector's.
	const previousRelease = `
		insert into opamp_server.publications (id, organization_id, configuration_id, configuration_name,
		  version_id, version_number, version_label, yaml, sha256, published_by_id, published_by_email,
		  published_at, received_at)
		values ($1, $2, gen_random_uuid(), 'payments-gateway', gen_random_uuid(), 1, null,
		  'receivers: {}', repeat('ab', 32), null, null, '2026-10-01T12:00:00Z', '2026-10-01T12:00:00Z')`
	existing := []string{"1a1a1a1a-0000-4000-8000-00000000001a", "1b1b1b1b-0000-4000-8000-00000000001b"}
	for _, id := range existing {
		if _, err := d.Runtime.Exec(ctx, previousRelease, id, orgX); err != nil {
			t.Fatal(err)
		}
	}
	before := tableRows(t, d.Admin, "publications")

	for round := 1; round <= 2; round++ {
		results, err := database.MigrateUpTo(ctx, migrator, 3)
		if err != nil {
			t.Fatalf("round %d: up: %v", round, err)
		}
		if got := appliedVersions(results); !slices.Equal(got, []int64{3}) {
			t.Fatalf("round %d: up applied %v, want [3]", round, got)
		}
		// Every existing publication: Contrib 0.162.0, and nothing else changed.
		want := make([]string, 0, len(before))
		for _, row := range before {
			want = append(want, strings.TrimSuffix(row, ")")+",otelcol-contrib,0.162.0)")
		}
		if got := tableRows(t, d.Admin, "publications"); !slices.Equal(got, want) {
			t.Fatalf("round %d: the publications after up:\n got  %v\n want %v", round, got, want)
		}

		// A server of the previous release still stores a publication: the
		// defaults fill the columns it doesn't know.
		older := "1c1c1c1c-0000-4000-8000-00000000001c"
		if _, err := d.Runtime.Exec(ctx, previousRelease, older, orgX); err != nil {
			t.Fatalf("round %d: the previous release's insert: %v", round, err)
		}
		// This release's store writes both, and they are kept as written.
		written := "1d1d1d1d-0000-4000-8000-00000000001d"
		if _, err := d.Runtime.Exec(ctx, `
			insert into opamp_server.publications (id, organization_id, configuration_id, configuration_name,
			  version_id, version_number, version_label, yaml, sha256, published_by_id, published_by_email,
			  published_at, received_at, collector_distribution, collector_version)
			values ($1, $2, gen_random_uuid(), 'payments-gateway', gen_random_uuid(), 1, null,
			  'receivers: {}', repeat('ab', 32), null, null, now(), now(), 'otelcol-sample', '0.155.3')`,
			written, orgX); err != nil {
			t.Fatalf("round %d: an insert with a collector: %v", round, err)
		}
		if got := collectorsOf(t, d.Admin, older, written); !slices.Equal(got, []string{
			older + " otelcol-contrib 0.162.0", written + " otelcol-sample 0.155.3",
		}) {
			t.Fatalf("round %d: collectors %v", round, got)
		}

		// The checks: openapi.yaml's formats, as the runtime role meets them.
		for _, c := range []struct{ distribution, version string }{
			{"Otelcol", "0.162.0"},
			{"otelcol_contrib", "0.162.0"},
			{"otelcol-", "0.162.0"},
			{"", "0.162.0"},
			{strings.Repeat("a", 65), "0.162.0"},
			{"otelcol-contrib", "0.162"},
			{"otelcol-contrib", "v0.162.0"},
			{"otelcol-contrib", "0.162.0-rc.1"},
			{"otelcol-contrib", "0.162.0\n"},
			{"otelcol-contrib", "0.\u0661.0"},
			{"otelcol-contrib", strings.Repeat("1", 61) + ".2.3"},
		} {
			_, err := d.Runtime.Exec(ctx, `
				insert into opamp_server.publications (id, organization_id, configuration_id, configuration_name,
				  version_id, version_number, version_label, yaml, sha256, published_by_id, published_by_email,
				  published_at, received_at, collector_distribution, collector_version)
				values (gen_random_uuid(), $1, gen_random_uuid(), 'payments-gateway', gen_random_uuid(), 1, null,
				  'receivers: {}', repeat('ab', 32), null, null, now(), now(), $2, $3)`,
				orgX, c.distribution, c.version)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
				t.Errorf("round %d: collector %q %q: err = %v, want 23514 check_violation", round, c.distribution, c.version, err)
			}
		}
		for _, c := range []struct{ distribution, version string }{
			{strings.Repeat("a", 64), strings.Repeat("1", 60) + ".2.3"},
			{"otelcol", "10.20.30"},
		} {
			if _, err := d.Runtime.Exec(ctx, `
				insert into opamp_server.publications (id, organization_id, configuration_id, configuration_name,
				  version_id, version_number, version_label, yaml, sha256, published_by_id, published_by_email,
				  published_at, received_at, collector_distribution, collector_version)
				values (gen_random_uuid(), $1, gen_random_uuid(), 'payments-gateway', gen_random_uuid(), 1, null,
				  'receivers: {}', repeat('ab', 32), null, null, now(), now(), $2, $3)`,
				orgX, c.distribution, c.version); err != nil {
				t.Errorf("round %d: collector %q %q: %v, want it stored", round, c.distribution, c.version, err)
			}
		}

		if got := runtimeGrants(t, d.Admin, database.RuntimeRole); !sameGrants(got, grantsAt(2)) {
			t.Fatalf("round %d: after up, %s holds %v, want 00002's %v", round, database.RuntimeRole, got, grantsAt(2))
		}
		comments := strings.Join(publicationsComments(t, d.Admin), "\n")
		for _, want := range []string{"collector_distribution: the collector distribution", "collector_version: the collector version"} {
			if !strings.Contains(comments, want) {
				t.Fatalf("round %d: no comment %q among %s", round, want, comments)
			}
		}

		// The rows this round added go, so down can be compared with before.
		if _, err := d.Owner.Exec(ctx, `delete from opamp_server.publications where not (id = any ($1))`, existing); err != nil {
			t.Fatal(err)
		}
		result, err := migrator.Down(ctx)
		if err != nil || result.Source.Version != 3 {
			t.Fatalf("round %d: down = %v, %v; want 3 rolled back", round, result, err)
		}
		if got := tableRows(t, d.Admin, "publications"); !slices.Equal(got, before) {
			t.Fatalf("round %d: down left the publications\n %v\nwant\n %v", round, got, before)
		}
		if got := publicationsComments(t, d.Admin); !slices.Equal(got, comments2) {
			t.Fatalf("round %d: after down, the publications comments are\n %v\nwant 00002's\n %v", round, got, comments2)
		}
		if got := runtimeGrants(t, d.Admin, database.RuntimeRole); !sameGrants(got, grantsAt(2)) {
			t.Fatalf("round %d: after down, %s holds %v", round, database.RuntimeRole, got)
		}
		assertStatus(t, migrator, 2)
	}
}

// Migration 00004 is 00003's contract step. Every server since 00003 writes
// both collector columns, so the store's insert still works; an insert
// leaving them out, as a release from before 00003 does, now fails 23502
// rather than recording a collector nobody chose. Nothing else changes: no
// row, no grant. Down puts the defaults and 00003's comments back; up again
// drops them again.
func TestMigration4DropsThePublicationCollectorsDefaults(t *testing.T) {
	d := databasetest.New(t, testDatabase, databasetest.Unmigrated())
	ctx := context.Background()
	migrator, err := database.OpenMigrator(ctx, d.OwnerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = migrator.Close() }()
	if _, err := database.MigrateUpTo(ctx, migrator, 3); err != nil {
		t.Fatalf("up to 3: %v", err)
	}
	comments3 := publicationsComments(t, d.Admin)

	orgX := uuid.MustParse("0a0a0a0a-0000-4000-8000-00000000000a")
	if _, err := d.Owner.Exec(ctx, `
		insert into opamp_server.organizations (id, name, created_at, updated_at)
		values ($1, 'an organization', '2026-10-01T10:00:00Z', '2026-10-01T10:00:00Z')`, orgX); err != nil {
		t.Fatal(err)
	}
	// withoutCollector is the insert of a release from before 00003: no
	// collector. $1 is the publication's id, $2 its organization's.
	const withoutCollector = `
		insert into opamp_server.publications (id, organization_id, configuration_id, configuration_name,
		  version_id, version_number, version_label, yaml, sha256, published_by_id, published_by_email,
		  published_at, received_at)
		values ($1, $2, gen_random_uuid(), 'payments-gateway', gen_random_uuid(), 1, null,
		  'receivers: {}', repeat('ab', 32), null, null, '2026-10-01T12:00:00Z', '2026-10-01T12:00:00Z')`
	existing := uuid.MustParse("1a1a1a1a-0000-4000-8000-00000000001a")
	if _, err := d.Runtime.Exec(ctx, withoutCollector, existing, orgX); err != nil {
		t.Fatal(err)
	}
	before := tableRows(t, d.Admin, "publications")
	// withCollector is the store's insert as of 00003: every column, the
	// collector's two included. Today's Put also writes the names its YAML
	// references (migration 00012), so it needs today's schema; no previous
	// release runs. $1 is the publication's id, $2 its organization's.
	const withCollector = `
		insert into opamp_server.publications (id, organization_id, configuration_id, configuration_name,
		  version_id, version_number, version_label, yaml, sha256, published_by_id, published_by_email,
		  published_at, received_at, collector_distribution, collector_version)
		values ($1, $2, gen_random_uuid(), 'payments-gateway', gen_random_uuid(), 2, null,
		  $3, $4, null, null, '2026-10-12T09:00:00Z', '2026-10-12T09:00:01Z', 'otelcol-sample', '0.155.3')`

	for round := 1; round <= 2; round++ {
		results, err := database.MigrateUpTo(ctx, migrator, 4)
		if err != nil {
			t.Fatalf("round %d: up: %v", round, err)
		}
		if got := appliedVersions(results); !slices.Equal(got, []int64{4}) {
			t.Fatalf("round %d: up applied %v, want [4]", round, got)
		}
		if got := tableRows(t, d.Admin, "publications"); !slices.Equal(got, before) {
			t.Fatalf("round %d: up changed the publications:\n before %v\n after  %v", round, before, got)
		}
		if got := collectorDefaults(t, d.Admin); !slices.Equal(got, []string{"collector_distribution: none", "collector_version: none"}) {
			t.Fatalf("round %d: after up, the defaults are %v, want none", round, got)
		}

		// The store's insert of that release names both columns, so it
		// still stores.
		yaml := "receivers: {}\n"
		sum := sha256.Sum256([]byte(yaml))
		stored := uuid.New()
		if tag, err := d.Runtime.Exec(ctx, withCollector, stored, orgX, yaml, hex.EncodeToString(sum[:])); err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("round %d: the store's insert = %v, %v; want one row", round, tag, err)
		}
		if got := collectorsOf(t, d.Admin, stored.String()); !slices.Equal(got, []string{stored.String() + " otelcol-sample 0.155.3"}) {
			t.Fatalf("round %d: the store's publication reads %v", round, got)
		}
		// An insert without the collector is refused, naming the column.
		_, err = d.Runtime.Exec(ctx, withoutCollector, uuid.New(), orgX)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23502" || pgErr.ColumnName != "collector_distribution" {
			t.Fatalf("round %d: an insert without the collector: err = %v, want 23502 on collector_distribution", round, err)
		}
		if got := runtimeGrants(t, d.Admin, database.RuntimeRole); !sameGrants(got, grantsAt(4)) {
			t.Fatalf("round %d: after up, %s holds %v, want %v", round, database.RuntimeRole, got, grantsAt(4))
		}
		comments := strings.Join(publicationsComments(t, d.Admin), "\n")
		for _, want := range []string{"collector_distribution: the collector distribution", "No default: the store writes it on every insert"} {
			if !strings.Contains(comments, want) {
				t.Fatalf("round %d: no comment %q among %s", round, want, comments)
			}
		}
		if _, err := d.Owner.Exec(ctx, `delete from opamp_server.publications where id = $1`, stored); err != nil {
			t.Fatal(err)
		}

		result, err := migrator.Down(ctx)
		if err != nil || result.Source.Version != 4 {
			t.Fatalf("round %d: down = %v, %v; want 4 rolled back", round, result, err)
		}
		assertStatus(t, migrator, 3)
		if got := collectorDefaults(t, d.Admin); !slices.Equal(got, []string{
			"collector_distribution: 'otelcol-contrib'::text", "collector_version: '0.162.0'::text",
		}) {
			t.Fatalf("round %d: after down, the defaults are %v, want 00003's", round, got)
		}
		if got := tableRows(t, d.Admin, "publications"); !slices.Equal(got, before) {
			t.Fatalf("round %d: down changed the publications:\n before %v\n after  %v", round, before, got)
		}
		if got := publicationsComments(t, d.Admin); !slices.Equal(got, comments3) {
			t.Fatalf("round %d: after down, the publications comments are\n %v\nwant 00003's\n %v", round, got, comments3)
		}
		if got := runtimeGrants(t, d.Admin, database.RuntimeRole); !sameGrants(got, grantsAt(3)) {
			t.Fatalf("round %d: after down, %s holds %v", round, database.RuntimeRole, got)
		}
	}
}

// Migration 00012 adds only. On a schema at 11 holding two organizations'
// fleet models, with published versions assigned, pinned and served, it
// adds the four tables empty, the counter at 0 for every organization, each
// organization a generation of its own (the column then without a default),
// and the bindings, their stamps and the markers null, changing no column
// any row had; the store still registers an organization (its insert names
// the counter and a generation). Down leaves nothing of it and every row as
// it was, and up again adds it again. No previous release's statements are
// replayed: none runs in production.
func TestMigration12AddsOnlyAndRollsBack(t *testing.T) {
	d := databasetest.New(t, testDatabase, databasetest.Unmigrated())
	ctx := context.Background()
	migrator, err := database.OpenMigrator(ctx, d.OwnerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = migrator.Close() }()
	if _, err := database.MigrateUpTo(ctx, migrator, 11); err != nil {
		t.Fatalf("up to 11: %v", err)
	}
	if _, err := d.Owner.Exec(ctx, fill(fixture+linkPublications)); err != nil {
		t.Fatalf("the fixture: %v", err)
	}
	existing := []string{"collector_errors", "collector_groups", "collector_instances", "enrollment_token_events",
		"enrollment_tokens", "fleets", "organizations", "publications"}
	added := map[string][]string{
		"organizations":       {"variables_revision", "generation"},
		"collector_groups":    {"variable_group_id", "variable_group_stamp"},
		"collector_instances": {"pinned_variable_group_id", "pinned_variable_group_stamp", "served_variables_stamp"},
	}
	// rowsWithout lists table's rows as JSON without the columns 00012
	// adds, so a row reads the same at 11 and at 12.
	rowsWithout := func(table string) []string {
		t.Helper()
		columns := added[table]
		if columns == nil {
			columns = []string{}
		}
		rows, err := d.Admin.Query(ctx, "select (to_jsonb(r) - $1::text[])::text from opamp_server."+
			pgx.Identifier{table}.Sanitize()+" r order by 1", columns)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	beforeText, beforeJSON := map[string][]string{}, map[string][]string{}
	for _, table := range existing {
		beforeText[table], beforeJSON[table] = tableRows(t, d.Admin, table), rowsWithout(table)
		if len(beforeText[table]) == 0 {
			t.Fatalf("the fixture holds no row in %s", table)
		}
	}
	count := func(query string) int {
		t.Helper()
		var n int
		if err := d.Admin.QueryRow(ctx, query).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}
	// what00012Made counts the objects in opamp_server whose name says
	// variable: tables, columns, constraints and indexes.
	const what00012Made = `
		select (select count(*) from pg_class
		         where relnamespace = 'opamp_server'::regnamespace and relname like '%variable%')
		     + (select count(*) from pg_constraint
		         where connamespace = 'opamp_server'::regnamespace and conname like '%variable%')
		     + (select count(*) from pg_attribute a join pg_class c on c.oid = a.attrelid
		         where c.relnamespace = 'opamp_server'::regnamespace and a.attname like '%variable%'
		           and a.attnum > 0 and not a.attisdropped)`

	for round := 1; round <= 2; round++ {
		results, err := database.MigrateUpTo(ctx, migrator, 12)
		if err != nil {
			t.Fatalf("round %d: up: %v", round, err)
		}
		if got := appliedVersions(results); !slices.Equal(got, []int64{12}) {
			t.Fatalf("round %d: up applied %v, want [12]", round, got)
		}
		assertStatus(t, migrator, 12)
		assertTablesAt(t, d.Owner, 12)
		if got := runtimeGrants(t, d.Admin, database.RuntimeRole); !sameGrants(got, grantsAt(12)) {
			t.Fatalf("round %d: after up, %s holds %v, want %v", round, database.RuntimeRole, got, grantsAt(12))
		}
		for _, table := range existing {
			if got := rowsWithout(table); !slices.Equal(got, beforeJSON[table]) {
				t.Fatalf("round %d: up changed %s:\n before %v\n after  %v", round, table, beforeJSON[table], got)
			}
		}
		for _, table := range migrationSteps[12].tables {
			if got := tableRows(t, d.Admin, table); len(got) != 0 {
				t.Fatalf("round %d: up left rows in %s: %v", round, table, got)
			}
		}
		for query, want := range map[string]int{
			"select count(*) from opamp_server.organizations where variables_revision = 0":  2,
			"select count(*) from opamp_server.organizations where variables_revision <> 0": 0,
			// Each existing organization a generation of its own, and the
			// column left without a default.
			"select count(distinct generation) from opamp_server.organizations": 2,
			`select count(*) from information_schema.columns
			  where table_schema = 'opamp_server' and table_name = 'organizations'
			    and column_name = 'generation' and (column_default is not null or is_nullable = 'YES')`: 0,
			`select count(*) from opamp_server.collector_groups
			  where variable_group_id is not null or variable_group_stamp is not null`: 0,
			`select count(*) from opamp_server.collector_instances
			  where pinned_variable_group_id is not null or pinned_variable_group_stamp is not null
			     or served_variables_stamp is not null`: 0,
		} {
			if got := count(query); got != want {
				t.Fatalf("round %d: %s = %d, want %d", round, query, got, want)
			}
		}
		// The store's insert names the counter, so it still registers: with
		// the column's default dropped, an insert that left the counter out
		// would be refused (23502). The default is put back before Down.
		if _, err := d.Admin.Exec(ctx, `alter table opamp_server.organizations
			alter column variables_revision drop default`); err != nil {
			t.Fatal(err)
		}
		registered := organizations.Organization{ID: uuid.New(), Name: "Initech",
			CreatedAt: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)}
		store := organizations.NewPostgres(d.Runtime)
		if result, _, err := store.Create(ctx, registered); err != nil || result != organizations.Created {
			t.Fatalf("round %d: the store's Create = %v, %v; want created", round, result, err)
		}
		var revision int64
		var generation *uuid.UUID
		if err := d.Admin.QueryRow(ctx, `select variables_revision, generation from opamp_server.organizations where id = $1`,
			registered.ID).Scan(&revision, &generation); err != nil || revision != 0 || generation == nil || *generation == uuid.Nil {
			t.Fatalf("round %d: the registered organization's counter = %d, generation %v, %v; want 0 and a generation",
				round, revision, generation, err)
		}
		if deleted, err := store.Delete(ctx, registered.ID); err != nil || !deleted {
			t.Fatalf("round %d: the store's Delete = %v, %v", round, deleted, err)
		}
		if _, err := d.Admin.Exec(ctx, `alter table opamp_server.organizations
			alter column variables_revision set default 0`); err != nil {
			t.Fatal(err)
		}

		result, err := migrator.Down(ctx)
		if err != nil || result.Source.Version != 12 {
			t.Fatalf("round %d: down = %v, %v; want 12 rolled back", round, result, err)
		}
		assertStatus(t, migrator, 11)
		assertTablesAt(t, d.Owner, 11)
		if got := count(what00012Made); got != 0 {
			t.Fatalf("round %d: down left %d objects of 00012's", round, got)
		}
		if got := runtimeGrants(t, d.Admin, database.RuntimeRole); !sameGrants(got, grantsAt(11)) {
			t.Fatalf("round %d: after down, %s holds %v, want %v", round, database.RuntimeRole, got, grantsAt(11))
		}
		for _, table := range existing {
			if got := tableRows(t, d.Admin, table); !slices.Equal(got, beforeText[table]) {
				t.Fatalf("round %d: down changed %s:\n before %v\n after  %v", round, table, beforeText[table], got)
			}
		}
	}
}

// collectorDefaults lists the publications' two collector columns with
// their defaults, "none" for none.
func collectorDefaults(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		select a.attname || ': ' || coalesce(pg_get_expr(d.adbin, d.adrelid), 'none')
		  from pg_attribute a
		  left join pg_attrdef d on d.adrelid = a.attrelid and d.adnum = a.attnum
		 where a.attrelid = 'opamp_server.publications'::regclass
		   and a.attname in ('collector_distribution', 'collector_version')
		 order by a.attname`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// collectorsOf lists each publication's id, distribution and version, in
// the order of ids given.
func collectorsOf(t *testing.T, pool *pgxpool.Pool, ids ...string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		select id::text || ' ' || collector_distribution || ' ' || collector_version
		  from opamp_server.publications
		 where id = any ($1::uuid[])
		 order by array_position($1::uuid[], id)`, ids)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// tableRows lists every row of a opamp_server table as text, in order.
func tableRows(t *testing.T, pool *pgxpool.Pool, table string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		"select r::text from opamp_server."+pgx.Identifier{table}.Sanitize()+" r order by r::text")
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// organizationRows lists every organization, its times in UTC, by id.
func organizationRows(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		select id::text || ' named ' || name
		       || ', created ' || to_char(created_at at time zone 'UTC', 'YYYY-MM-DD HH24:MI:SS.MS')
		       || ', updated ' || to_char(updated_at at time zone 'UTC', 'YYYY-MM-DD HH24:MI:SS.MS')
		  from opamp_server.organizations
		 order by id`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// publicationsComments lists the publications table's comment and its
// columns', each as "<column>: <comment>".
func publicationsComments(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		select coalesce(a.attname, '(table)') || ': ' || coalesce(d.description, '')
		  from pg_description d
		  join pg_class c on c.oid = d.objoid and d.classoid = 'pg_class'::regclass
		  join pg_namespace n on n.oid = c.relnamespace
		  left join pg_attribute a on a.attrelid = c.oid and a.attnum = d.objsubid and d.objsubid > 0
		 where n.nspname = 'opamp_server' and c.relname = 'publications'
		 order by d.objsubid`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("the publications table has no comments")
	}
	return got
}

func TestMigrateRefusesAServerWithoutTheRuntimeRoleNamingBootstrap(t *testing.T) {
	d := databasetest.New(t, testDatabase, databasetest.Unmigrated())
	ctx := context.Background()
	missing := "telflo_opamp_server_runtime_missing_from_this_server"

	migrator, err := database.OpenMigratorRequiring(ctx, d.OwnerURL, missing)
	if err == nil {
		_ = migrator.Close()
		t.Fatal("OpenMigrator succeeded without the runtime role")
	}
	for _, want := range []string{missing, "doesn't exist", "deploy/bootstrap.sql"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q doesn't mention %q", err, want)
		}
	}
	// It refused before creating anything.
	var schemas int
	if err := d.Admin.QueryRow(ctx,
		"select count(*) from pg_namespace where nspname = 'opamp_server'").Scan(&schemas); err != nil {
		t.Fatal(err)
	}
	if schemas != 0 {
		t.Fatal("the refused migrate created the opamp_server schema")
	}
}

// `migrate` runs only as the database's owner. Any other role (the
// server's, a superuser, a member of the owner) is refused with a message
// naming the setting and the owner role, before anything is made, rather
// than with the bare 42501 its first `create schema` would end on.
func TestMigrateRefusesARoleThatDoesNotOwnTheDatabase(t *testing.T) {
	d := databasetest.New(t, testDatabase, databasetest.Unmigrated())
	ctx := context.Background()
	superuser, superuserURL := d.ThrowawayRole(t, "superuser")
	member, memberURL := d.ThrowawayRole(t, "nosuperuser")
	if _, err := d.Admin.Exec(ctx, "grant telflo_opamp_server_owner to "+member); err != nil {
		t.Fatal(err)
	}
	// Should a refusal fail, the schema that role made must go before the
	// roles do (cleanups run last first), or a role owning it can't be
	// dropped and outlives the test on the shared server.
	t.Cleanup(func() {
		if _, err := d.Admin.Exec(context.Background(), "drop schema if exists opamp_server cascade"); err != nil {
			t.Errorf("dropping the schema: %v", err)
		}
	})

	for _, c := range []struct{ role, url string }{
		{database.ServerRole, d.RuntimeURL},
		{superuser, superuserURL},
		{member, memberURL},
	} {
		migrator, err := database.OpenMigrator(ctx, c.url)
		if err == nil {
			_ = migrator.Close()
			t.Fatalf("OpenMigrator as %s succeeded", c.role)
		}
		want := `refusing to migrate: the role "` + c.role + `" doesn't own the database "` + testDatabase +
			`" ("telflo_opamp_server_owner" does). Migrations run as the database's owner, telflo_opamp_server_owner, ` +
			`which deploy/bootstrap.sql makes: set OPAMP_MIGRATION_DATABASE_URL to connect as it`
		if err.Error() != want {
			t.Errorf("as %s: error %q, want %q", c.role, err, want)
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			t.Errorf("as %s: the refusal wraps the database's error %s", c.role, pgErr.Code)
		}
	}

	var schemas int
	if err := d.Admin.QueryRow(ctx,
		"select count(*) from pg_namespace where nspname = 'opamp_server'").Scan(&schemas); err != nil {
		t.Fatal(err)
	}
	if schemas != 0 {
		t.Fatal("a refused migrate created the opamp_server schema")
	}
}

func TestMigrateRefusesAURLThatDoesNotParseWithoutQuotingIt(t *testing.T) {
	_, err := database.OpenMigrator(context.Background(), "postgresql://someone:a-secret@localhost:notaport/telflo_opamp_server")
	if !errors.Is(err, database.ErrInvalidURL) {
		t.Fatalf("error = %v, want ErrInvalidURL", err)
	}
	if strings.Contains(err.Error(), "a-secret") || strings.Contains(err.Error(), "someone") {
		t.Fatalf("error %q quotes the URL", err)
	}
}

// OpenMigrator's errors, and every startup check's, quote no part of the
// URL's password, however the driver takes the URL apart (a password
// holding `@` ends the user part early, and pgx then fails to resolve the
// rest as the host). Each layer guards itself: these are the errors as this
// package returns them, before any caller describes them again. Each
// password is two run-time canaries around the character, searched for by
// boolean and never printed; what each error says shows it reached the
// driver.
func TestDatabaseErrorsQuoteNoPartOfThePassword(t *testing.T) {
	u, err := url.Parse(databasetest.AdminURL(t))
	if err != nil {
		t.Fatal("the admin URL doesn't parse")
	}
	const (
		unreachable    = "the database's host can't be reached or its name resolved"
		authentication = "authentication failed (SQLSTATE 28P01)"
	)
	ctx := context.Background()
	for _, c := range []struct{ separator, want string }{
		{"@", unreachable},
		{"?", authentication},
		{"#", authentication},
		{"%40", authentication},
	} {
		before, after := canary(t), canary(t)
		databaseURL := "postgresql://" + database.ServerRole + ":" + before + c.separator + after + "@" + u.Host + "/" + testDatabase
		pool, err := database.OpenPool(ctx, databaseURL)
		if err != nil {
			t.Fatalf("OpenPool with a password holding %q failed; it connects lazily", c.separator)
		}
		checks := []struct {
			name string
			run  func() error
		}{
			{"OpenMigrator", func() error {
				migrator, err := database.OpenMigrator(ctx, databaseURL)
				if err == nil {
					_ = migrator.Close()
				}
				return err
			}},
			{"CheckRole", func() error { return database.CheckRole(ctx, pool) }},
			{"CheckEncoding", func() error { return database.CheckEncoding(ctx, pool) }},
			{"CheckBootstrapShape", func() error { return database.CheckBootstrapShape(ctx, pool) }},
			{"CheckNoImportsTable", func() error { return database.CheckNoImportsTable(ctx, pool) }},
			{"CheckSchemaVersion", func() error { return database.CheckSchemaVersion(ctx, pool, 1) }},
		}
		for _, check := range checks {
			err := check.run()
			if err == nil {
				t.Errorf("%s with a password holding %q succeeded", check.name, c.separator)
				continue
			}
			if strings.Contains(err.Error(), before) || strings.Contains(err.Error(), after) {
				t.Errorf("%s's error quotes a part of a password holding %q", check.name, c.separator)
				continue
			}
			if !strings.HasSuffix(err.Error(), ": "+c.want) {
				t.Errorf("%s with a password holding %q doesn't end %q", check.name, c.separator, c.want)
			}
		}
		pool.Close()
	}
}

// foreignKeys lists every foreign key on a table in opamp_server, and
// whether it points outside the schema.
func foreignKeys(t *testing.T, pool *pgxpool.Pool) (all, leaving []string) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		select c.conname, rn.nspname <> 'opamp_server'
		  from pg_constraint c
		  join pg_class t on t.oid = c.conrelid
		  join pg_namespace n on n.oid = t.relnamespace
		  join pg_class r on r.oid = c.confrelid
		  join pg_namespace rn on rn.oid = r.relnamespace
		 where c.contype = 'f' and n.nspname = 'opamp_server'
		 order by c.conname`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var isLeaving bool
		if err := rows.Scan(&name, &isLeaving); err != nil {
			t.Fatal(err)
		}
		all = append(all, name)
		if isLeaving {
			leaving = append(leaving, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return all, leaving
}

// foreignKeyDefinitions maps every foreign key on a table in opamp_server to
// its definition, as pg_get_constraintdef prints it.
func foreignKeyDefinitions(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		select c.conname, pg_get_constraintdef(c.oid)
		  from pg_constraint c
		  join pg_class t on t.oid = c.conrelid
		  join pg_namespace n on n.oid = t.relnamespace
		 where c.contype = 'f' and n.nspname = 'opamp_server'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	definitions := map[string]string{}
	for rows.Next() {
		var name, definition string
		if err := rows.Scan(&name, &definition); err != nil {
			t.Fatal(err)
		}
		definitions[name] = definition
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return definitions
}

// foreignKeyProblems checks the rule every foreign key in opamp_server
// keeps (docs/decisions/0001-reached-only-through-its-api.md) and answers
// what breaks it, one finding per key or table:
//   - no key leaves the schema;
//   - every key carries the organization: its first column is
//     organization_id on both sides (a composite key, so no link can reach
//     another organization's row), or it is (organization_id) ->
//     organizations(id) on delete cascade;
//   - every table holding an organization's rows has that cascade of its
//     own, so an organization's delete runs every cascade before any NO
//     ACTION check.
//
// The server's own table (database.ServerTable) holds no organization's
// rows: it needs no cascade, and a key to it carries no organization.
func foreignKeyProblems(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	ctx := context.Background()
	// isOrganizationCascade is the key (organization_id) ->
	// opamp_server.organizations(id) on delete cascade.
	const isOrganizationCascade = `
		cardinality(c.conkey) = 1
		and (select attname from pg_attribute where attrelid = c.conrelid and attnum = c.conkey[1]) = 'organization_id'
		and (select attname from pg_attribute where attrelid = c.confrelid and attnum = c.confkey[1]) = 'id'
		and rn.nspname = 'opamp_server' and r.relname = 'organizations'
		and c.confdeltype = 'c'`
	rows, err := pool.Query(ctx, `
		select c.conname, n.nspname || '.' || t.relname,
		       rn.nspname <> 'opamp_server',
		       (cardinality(c.conkey) > 1
		        and (select attname from pg_attribute where attrelid = c.conrelid and attnum = c.conkey[1]) = 'organization_id'
		        and (select attname from pg_attribute where attrelid = c.confrelid and attnum = c.confkey[1]) = 'organization_id')
		       or (`+isOrganizationCascade+`)
		       or (rn.nspname = 'opamp_server' and r.relname = '`+database.ServerTable+`')
		  from pg_constraint c
		  join pg_class t on t.oid = c.conrelid
		  join pg_namespace n on n.oid = t.relnamespace
		  join pg_class r on r.oid = c.confrelid
		  join pg_namespace rn on rn.oid = r.relnamespace
		 where c.contype = 'f' and n.nspname = 'opamp_server'
		 order by c.conname`)
	if err != nil {
		t.Fatal(err)
	}
	var problems []string
	for rows.Next() {
		var name, table string
		var isLeaving, carriesOrganization bool
		if err := rows.Scan(&name, &table, &isLeaving, &carriesOrganization); err != nil {
			t.Fatal(err)
		}
		if isLeaving {
			problems = append(problems, name+" on "+table+" leaves opamp_server")
		}
		if !carriesOrganization {
			problems = append(problems, name+" on "+table+" doesn't carry organization_id")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	rows, err = pool.Query(ctx, `
		select n.nspname || '.' || t.relname
		  from pg_class t join pg_namespace n on n.oid = t.relnamespace
		 where n.nspname = 'opamp_server' and t.relkind = 'r'
		   and t.relname not in ('goose_db_version', 'organizations', '`+database.ServerTable+`')
		   and not exists (
		     select 1
		       from pg_constraint c
		       join pg_class r on r.oid = c.confrelid
		       join pg_namespace rn on rn.oid = r.relnamespace
		      where c.contype = 'f' and c.conrelid = t.oid and `+isOrganizationCascade+`)
		 order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		problems = append(problems, table+" has no (organization_id) -> organizations(id) on delete cascade")
	}
	return problems
}

// organizationCascade is the definition of every table's own key to its
// organization.
const organizationCascade = "FOREIGN KEY (organization_id) REFERENCES opamp_server.organizations(id) ON DELETE CASCADE"

// wantForeignKeys is every foreign key in opamp_server at the newest
// migration, pinned by name and definition as the grants are: each table's
// cascade from its organization, and every link between an organization's
// rows a composite key carrying organization_id. NO ACTION prints no
// action.
var wantForeignKeys = map[string]string{
	"fk_publications_organization":     organizationCascade,
	"fk_fleets_organization":           organizationCascade,
	"fk_collector_groups_organization": organizationCascade,
	"fk_collector_groups_fleet": "FOREIGN KEY (organization_id, fleet_id) " +
		"REFERENCES opamp_server.fleets(organization_id, id) ON DELETE CASCADE",
	"fk_collector_groups_assigned_publication": "FOREIGN KEY (organization_id, assigned_publication_id) " +
		"REFERENCES opamp_server.publications(organization_id, id)",
	"fk_enrollment_tokens_organization": organizationCascade,
	"fk_enrollment_tokens_fleet": "FOREIGN KEY (organization_id, fleet_id) " +
		"REFERENCES opamp_server.fleets(organization_id, id) ON DELETE CASCADE",
	"fk_enrollment_token_events_organization": organizationCascade,
	"fk_enrollment_token_events_fleet": "FOREIGN KEY (organization_id, fleet_id) " +
		"REFERENCES opamp_server.fleets(organization_id, id) ON DELETE CASCADE",
	"fk_enrollment_token_events_enrollment_token": "FOREIGN KEY (organization_id, fleet_id, enrollment_token_id) " +
		"REFERENCES opamp_server.enrollment_tokens(organization_id, fleet_id, id) ON DELETE CASCADE",
	"fk_collector_instances_organization": organizationCascade,
	"fk_collector_instances_fleet": "FOREIGN KEY (organization_id, fleet_id) " +
		"REFERENCES opamp_server.fleets(organization_id, id) ON DELETE CASCADE",
	"fk_collector_instances_collector_group": "FOREIGN KEY (organization_id, fleet_id, collector_group_id) " +
		"REFERENCES opamp_server.collector_groups(organization_id, fleet_id, id)",
	"fk_collector_instances_pinned_publication": "FOREIGN KEY (organization_id, pinned_publication_id) " +
		"REFERENCES opamp_server.publications(organization_id, id)",
	"fk_collector_instances_served_publication": "FOREIGN KEY (organization_id, served_publication_id) " +
		"REFERENCES opamp_server.publications(organization_id, id) ON DELETE SET NULL (served_publication_id)",
	"fk_collector_instances_enrollment_token": "FOREIGN KEY (organization_id, fleet_id, enrollment_token_id) " +
		"REFERENCES opamp_server.enrollment_tokens(organization_id, fleet_id, id) ON DELETE SET NULL (enrollment_token_id)",
	"fk_collector_errors_organization": organizationCascade,
	"fk_collector_errors_collector_instance": "FOREIGN KEY (organization_id, collector_instance_id) " +
		"REFERENCES opamp_server.collector_instances(organization_id, id) ON DELETE CASCADE",
	// Migration 00012: the variables' tables, and the two bindings, NO ACTION,
	// so a variable group in use can't go.
	"fk_variables_organization":                organizationCascade,
	"fk_variable_groups_organization":          organizationCascade,
	"fk_variable_group_overrides_organization": organizationCascade,
	"fk_variable_group_overrides_variable_group": "FOREIGN KEY (organization_id, variable_group_id) " +
		"REFERENCES opamp_server.variable_groups(organization_id, id) ON DELETE CASCADE",
	"fk_variable_group_overrides_variable": "FOREIGN KEY (organization_id, variable_id) " +
		"REFERENCES opamp_server.variables(organization_id, id) ON DELETE CASCADE",
	"fk_publication_variable_names_organization": organizationCascade,
	"fk_publication_variable_names_publication": "FOREIGN KEY (organization_id, publication_id) " +
		"REFERENCES opamp_server.publications(organization_id, id) ON DELETE CASCADE",
	"fk_collector_groups_variable_group": "FOREIGN KEY (organization_id, variable_group_id) " +
		"REFERENCES opamp_server.variable_groups(organization_id, id)",
	"fk_collector_instances_pinned_variable_group": "FOREIGN KEY (organization_id, pinned_variable_group_id) " +
		"REFERENCES opamp_server.variable_groups(organization_id, id)",
	// Migration 00013: a key's organizations, and the key, the server's own
	// row, which carries no organization.
	"fk_api_key_organizations_organization": organizationCascade,
	"fk_api_key_organizations_api_key": "FOREIGN KEY (api_key_id) " +
		"REFERENCES opamp_server.api_keys(id) ON DELETE CASCADE",
}

// No foreign key leaves opamp_server
// (docs/decisions/0001-reached-only-through-its-api.md), and every one
// inside it carries the organization: each table's own cascade from its
// organization, and every other link a composite key whose first column is
// organization_id on both sides, so no row can name another organization's.
// Each key is pinned by name and definition.
func TestNoForeignKeyLeavesTheSchemaAndEveryKeyCarriesTheOrganization(t *testing.T) {
	d := databasetest.New(t, testDatabase)

	if problems := foreignKeyProblems(t, d.Admin); len(problems) != 0 {
		t.Errorf("foreign keys breaking the rule:\n  %s", strings.Join(problems, "\n  "))
	}
	got := foreignKeyDefinitions(t, d.Admin)
	for name, want := range wantForeignKeys {
		if got[name] != want {
			t.Errorf("%s is %q, want %q", name, got[name], want)
		}
	}
	for name, definition := range got {
		if _, ok := wantForeignKeys[name]; !ok {
			t.Errorf("an unexpected foreign key %s: %s", name, definition)
		}
	}
}

// The deliberate break, run every time: a foreign key from publications to a
// table in public is what the test above must see.
func TestForeignKeyCheckSeesAForeignKeyIntoPublic(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	ctx := context.Background()

	for _, statement := range []string{
		"create table public.platform_like (id uuid primary key)",
		`alter table opamp_server.publications add constraint fk_publications_platform_like
		   foreign key (organization_id) references public.platform_like (id)`,
	} {
		if _, err := d.Admin.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	all, leaving := foreignKeys(t, d.Admin)
	want := []string{"fk_publications_platform_like"}
	for name := range wantForeignKeys {
		want = append(want, name)
	}
	slices.Sort(want)
	if !slices.Equal(leaving, []string{"fk_publications_platform_like"}) || !slices.Equal(all, want) {
		t.Fatalf("the check saw all=%v leaving=%v, want the one foreign key into public among the schema's", all, leaving)
	}
	if problems := foreignKeyProblems(t, d.Admin); !slices.Equal(problems, []string{
		"fk_publications_platform_like on opamp_server.publications leaves opamp_server",
		"fk_publications_platform_like on opamp_server.publications doesn't carry organization_id",
	}) {
		t.Fatalf("the rule saw %v, want the key into public", problems)
	}
	if _, ok := foreignKeyDefinitions(t, d.Admin)["fk_publications_platform_like"]; !ok {
		t.Fatal("the pinned list didn't see the key into public")
	}
}

// The deliberate break, run every time: a link between two of an
// organization's tables that leaves organization_id out (a collector to its
// fleet by fleet_id alone, which could name another organization's fleet)
// is what the rule must see.
func TestForeignKeyCheckSeesAKeyWithoutTheOrganization(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	if _, err := d.Admin.Exec(context.Background(), `
		alter table opamp_server.collector_instances add constraint fk_collector_instances_fleet_alone
		  foreign key (fleet_id) references opamp_server.fleets (id) on delete cascade`); err != nil {
		t.Fatal(err)
	}
	if problems := foreignKeyProblems(t, d.Admin); !slices.Equal(problems, []string{
		"fk_collector_instances_fleet_alone on opamp_server.collector_instances doesn't carry organization_id",
	}) {
		t.Fatalf("the rule saw %v, want the key without organization_id", problems)
	}
}

// The deliberate break, run every time: a table without its own cascade
// from its organization, and one whose key to its organization doesn't
// cascade, are what the rule must see.
func TestForeignKeyCheckSeesATableWithoutItsOrganizationsCascade(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	for _, statement := range []string{
		"alter table opamp_server.enrollment_token_events drop constraint fk_enrollment_token_events_organization",
		"alter table opamp_server.collector_errors drop constraint fk_collector_errors_organization",
		`alter table opamp_server.collector_errors add constraint fk_collector_errors_organization
		   foreign key (organization_id) references opamp_server.organizations (id)`,
	} {
		if _, err := d.Admin.Exec(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
	if problems := foreignKeyProblems(t, d.Admin); !slices.Equal(problems, []string{
		"fk_collector_errors_organization on opamp_server.collector_errors doesn't carry organization_id",
		"opamp_server.collector_errors has no (organization_id) -> organizations(id) on delete cascade",
		"opamp_server.enrollment_token_events has no (organization_id) -> organizations(id) on delete cascade",
	}) {
		t.Fatalf("the rule saw %v, want both tables", problems)
	}
}

// runtimeGrants lists every privilege granted directly to role in this
// database: on the database, on schemas, on relations and on columns, in
// byte order.
func runtimeGrants(t *testing.T, pool *pgxpool.Pool, role string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		with grantee as (select oid from pg_roles where rolname = $1)
		select 'database ' || d.datname || ' ' || a.privilege_type
		  from pg_database d, aclexplode(d.datacl) a
		 where d.datname = current_database() and a.grantee = (select oid from grantee)
		union all
		select 'schema ' || n.nspname || ' ' || a.privilege_type
		  from pg_namespace n, aclexplode(n.nspacl) a
		 where a.grantee = (select oid from grantee)
		union all
		select 'relation ' || n.nspname || '.' || c.relname || ' ' || a.privilege_type
		  from pg_class c join pg_namespace n on n.oid = c.relnamespace, aclexplode(c.relacl) a
		 where a.grantee = (select oid from grantee)
		union all
		select 'column ' || n.nspname || '.' || c.relname || '.' || at.attname || ' ' || a.privilege_type
		  from pg_attribute at
		  join pg_class c on c.oid = at.attrelid
		  join pg_namespace n on n.oid = c.relnamespace, aclexplode(at.attacl) a
		 where a.grantee = (select oid from grantee)
		union all
		select 'default ' || coalesce(n.nspname, '(all schemas)') || ' ' || a.privilege_type
		  from pg_default_acl da left join pg_namespace n on n.oid = da.defaclnamespace,
		       aclexplode(da.defaclacl) a
		 where a.grantee = (select oid from grantee)`, role)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(grants)
	return grants
}

// sameGrants compares two lists of grants as sets, in byte order.
func sameGrants(got, want []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want)))
}

// migrationStep is what one migration makes: the tables it creates and the
// grants it gives telflo_opamp_server_runtime, each of which its Down takes back.
// Every embedded migration has an entry, so a new one must say here what it
// makes and grants.
type migrationStep struct {
	tables []string
	grants []string
}

// relationGrants is each privilege on table, as runtimeGrants prints them.
func relationGrants(table string, privileges ...string) []string {
	grants := make([]string, 0, len(privileges))
	for _, privilege := range privileges {
		grants = append(grants, "relation opamp_server."+table+" "+privilege)
	}
	return grants
}

// columnUpdates is UPDATE on each column of table, as runtimeGrants prints
// them.
func columnUpdates(table string, columns ...string) []string {
	grants := make([]string, 0, len(columns))
	for _, column := range columns {
		grants = append(grants, "column opamp_server."+table+"."+column+" UPDATE")
	}
	return grants
}

var migrationSteps = map[int64]migrationStep{
	1: {[]string{"publications"}, slices.Concat(
		[]string{"schema opamp_server USAGE", "relation opamp_server.goose_db_version SELECT"},
		relationGrants("publications", "SELECT", "INSERT", "DELETE"))},
	2: {[]string{"organizations"}, slices.Concat(
		relationGrants("organizations", "SELECT", "INSERT", "DELETE"),
		columnUpdates("organizations", "name", "updated_at"))},
	3: {},
	4: {},
	5: {nil, columnUpdates("organizations", "max_collectors", "requests_per_minute")},
	6: {[]string{"fleets"}, slices.Concat(
		relationGrants("fleets", "SELECT", "INSERT", "DELETE"),
		columnUpdates("fleets", "label", "status", "updated_at"))},
	7: {[]string{"collector_groups"}, slices.Concat(
		relationGrants("collector_groups", "SELECT", "INSERT", "DELETE"),
		columnUpdates("collector_groups", "name", "assigned_publication_id", "updated_at"))},
	8: {[]string{"enrollment_tokens", "enrollment_token_events"}, slices.Concat(
		relationGrants("enrollment_tokens", "SELECT", "INSERT"),
		columnUpdates("enrollment_tokens", "expires_at", "revoked_at", "do_refresh"),
		relationGrants("enrollment_token_events", "SELECT", "INSERT"))},
	9: {[]string{"collector_instances", "collector_errors"}, slices.Concat(
		relationGrants("collector_instances", "SELECT", "INSERT", "DELETE"),
		columnUpdates("collector_instances", "name", "collector_group_id",
			"pinned_publication_id", "served_publication_id",
			"served_sha256", "served_at", "effective_sha256", "effective_status", "effective_error",
			"effective_at", "full_state_requested_at", "reported_collector_version", "reported_ip",
			"reported_host_ip", "reported_attributes", "enrollment_token_id", "last_seen_at", "updated_at"),
		relationGrants("collector_errors", "SELECT", "INSERT", "DELETE"))},
	10: {},
	11: {nil, columnUpdates("collector_groups", "annotations")},
	12: {[]string{"variables", "variable_groups", "variable_group_overrides", "publication_variable_names"}, slices.Concat(
		columnUpdates("organizations", "variables_revision"),
		relationGrants("variables", "SELECT", "INSERT", "DELETE"),
		columnUpdates("variables", "name", "description", "value", "changed_by_id", "changed_by_email", "changed_at", "stamp"),
		relationGrants("variable_groups", "SELECT", "INSERT", "DELETE"),
		columnUpdates("variable_groups", "name", "description", "changed_by_id", "changed_by_email", "changed_at"),
		relationGrants("variable_group_overrides", "SELECT", "INSERT", "DELETE"),
		columnUpdates("variable_group_overrides", "value", "changed_by_id", "changed_by_email", "changed_at", "stamp"),
		columnUpdates("collector_groups", "variable_group_id", "variable_group_stamp"),
		columnUpdates("collector_instances", "pinned_variable_group_id", "pinned_variable_group_stamp", "served_variables_stamp"),
		relationGrants("publication_variable_names", "SELECT", "INSERT"))},
	13: {[]string{"api_keys", "api_key_organizations"}, slices.Concat(
		relationGrants("api_keys", "SELECT", "INSERT"),
		columnUpdates("api_keys", "name", "description", "revoked_at", "last_used_at"),
		relationGrants("api_key_organizations", "SELECT", "INSERT"))},
}

// grantsAt is what telflo_opamp_server_runtime holds with the schema at version:
// every grant of the migrations up to it, in byte order.
func grantsAt(version int64) []string {
	var grants []string
	for v, step := range migrationSteps {
		if v <= version {
			grants = append(grants, step.grants...)
		}
	}
	slices.Sort(grants)
	return grants
}

// assertTablesAt checks each table a migration makes exists exactly when the
// schema is at or past that migration.
func assertTablesAt(t *testing.T, pool *pgxpool.Pool, version int64) {
	t.Helper()
	for v, step := range migrationSteps {
		for _, table := range step.tables {
			assertTableExists(t, pool, table, v <= version)
		}
	}
}

// wantRuntimeGrants is what telflo_opamp_server_runtime holds at the newest
// migration, in byte order: select, insert and delete where the server adds
// and removes rows; update only on the columns it changes, so the ids,
// organizations, fleets, a fleet's type, a group's is_default, a token's
// kind, a collector's instance uid and every created_at never change; no
// update or delete at all on token events, nor delete on tokens (they go
// with their fleet, by the cascade, which runs as the owner). Migration
// 00012: the variables, variable groups and overrides are added, changed
// and removed, but never an id, an organization or an override's variable
// group or variable; the names a published version references are only ever
// inserted (they go with it, by the cascade); the counter, the bindings,
// their stamps and the marker may be updated. Migration 00013: a key and
// its organizations are inserted and never deleted; only its name, its
// description, its revocation and its last use change, never its secret,
// its organizations or its expiry.
var wantRuntimeGrants = []string{
	"column opamp_server.api_keys.description UPDATE",
	"column opamp_server.api_keys.last_used_at UPDATE",
	"column opamp_server.api_keys.name UPDATE",
	"column opamp_server.api_keys.revoked_at UPDATE",
	"column opamp_server.collector_groups.annotations UPDATE",
	"column opamp_server.collector_groups.assigned_publication_id UPDATE",
	"column opamp_server.collector_groups.name UPDATE",
	"column opamp_server.collector_groups.updated_at UPDATE",
	"column opamp_server.collector_groups.variable_group_id UPDATE",
	"column opamp_server.collector_groups.variable_group_stamp UPDATE",
	"column opamp_server.collector_instances.collector_group_id UPDATE",
	"column opamp_server.collector_instances.effective_at UPDATE",
	"column opamp_server.collector_instances.effective_error UPDATE",
	"column opamp_server.collector_instances.effective_sha256 UPDATE",
	"column opamp_server.collector_instances.effective_status UPDATE",
	"column opamp_server.collector_instances.enrollment_token_id UPDATE",
	"column opamp_server.collector_instances.full_state_requested_at UPDATE",
	"column opamp_server.collector_instances.last_seen_at UPDATE",
	"column opamp_server.collector_instances.name UPDATE",
	"column opamp_server.collector_instances.pinned_publication_id UPDATE",
	"column opamp_server.collector_instances.pinned_variable_group_id UPDATE",
	"column opamp_server.collector_instances.pinned_variable_group_stamp UPDATE",
	"column opamp_server.collector_instances.reported_attributes UPDATE",
	"column opamp_server.collector_instances.reported_collector_version UPDATE",
	"column opamp_server.collector_instances.reported_host_ip UPDATE",
	"column opamp_server.collector_instances.reported_ip UPDATE",
	"column opamp_server.collector_instances.served_at UPDATE",
	"column opamp_server.collector_instances.served_publication_id UPDATE",
	"column opamp_server.collector_instances.served_sha256 UPDATE",
	"column opamp_server.collector_instances.served_variables_stamp UPDATE",
	"column opamp_server.collector_instances.updated_at UPDATE",
	"column opamp_server.enrollment_tokens.do_refresh UPDATE",
	"column opamp_server.enrollment_tokens.expires_at UPDATE",
	"column opamp_server.enrollment_tokens.revoked_at UPDATE",
	"column opamp_server.fleets.label UPDATE",
	"column opamp_server.fleets.status UPDATE",
	"column opamp_server.fleets.updated_at UPDATE",
	"column opamp_server.organizations.max_collectors UPDATE",
	"column opamp_server.organizations.name UPDATE",
	"column opamp_server.organizations.requests_per_minute UPDATE",
	"column opamp_server.organizations.updated_at UPDATE",
	"column opamp_server.organizations.variables_revision UPDATE",
	"column opamp_server.variable_group_overrides.changed_at UPDATE",
	"column opamp_server.variable_group_overrides.changed_by_email UPDATE",
	"column opamp_server.variable_group_overrides.changed_by_id UPDATE",
	"column opamp_server.variable_group_overrides.stamp UPDATE",
	"column opamp_server.variable_group_overrides.value UPDATE",
	"column opamp_server.variable_groups.changed_at UPDATE",
	"column opamp_server.variable_groups.changed_by_email UPDATE",
	"column opamp_server.variable_groups.changed_by_id UPDATE",
	"column opamp_server.variable_groups.description UPDATE",
	"column opamp_server.variable_groups.name UPDATE",
	"column opamp_server.variables.changed_at UPDATE",
	"column opamp_server.variables.changed_by_email UPDATE",
	"column opamp_server.variables.changed_by_id UPDATE",
	"column opamp_server.variables.description UPDATE",
	"column opamp_server.variables.name UPDATE",
	"column opamp_server.variables.stamp UPDATE",
	"column opamp_server.variables.value UPDATE",
	"relation opamp_server.api_key_organizations INSERT",
	"relation opamp_server.api_key_organizations SELECT",
	"relation opamp_server.api_keys INSERT",
	"relation opamp_server.api_keys SELECT",
	"relation opamp_server.collector_errors DELETE",
	"relation opamp_server.collector_errors INSERT",
	"relation opamp_server.collector_errors SELECT",
	"relation opamp_server.collector_groups DELETE",
	"relation opamp_server.collector_groups INSERT",
	"relation opamp_server.collector_groups SELECT",
	"relation opamp_server.collector_instances DELETE",
	"relation opamp_server.collector_instances INSERT",
	"relation opamp_server.collector_instances SELECT",
	"relation opamp_server.enrollment_token_events INSERT",
	"relation opamp_server.enrollment_token_events SELECT",
	"relation opamp_server.enrollment_tokens INSERT",
	"relation opamp_server.enrollment_tokens SELECT",
	"relation opamp_server.fleets DELETE",
	"relation opamp_server.fleets INSERT",
	"relation opamp_server.fleets SELECT",
	"relation opamp_server.goose_db_version SELECT",
	"relation opamp_server.organizations DELETE",
	"relation opamp_server.organizations INSERT",
	"relation opamp_server.organizations SELECT",
	"relation opamp_server.publication_variable_names INSERT",
	"relation opamp_server.publication_variable_names SELECT",
	"relation opamp_server.publications DELETE",
	"relation opamp_server.publications INSERT",
	"relation opamp_server.publications SELECT",
	"relation opamp_server.variable_group_overrides DELETE",
	"relation opamp_server.variable_group_overrides INSERT",
	"relation opamp_server.variable_group_overrides SELECT",
	"relation opamp_server.variable_groups DELETE",
	"relation opamp_server.variable_groups INSERT",
	"relation opamp_server.variable_groups SELECT",
	"relation opamp_server.variables DELETE",
	"relation opamp_server.variables INSERT",
	"relation opamp_server.variables SELECT",
	"schema opamp_server USAGE",
}

func TestRuntimeRolePrivilegesAreExactlyWhatTheServerNeeds(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	ctx := context.Background()

	if got := runtimeGrants(t, d.Admin, database.RuntimeRole); !sameGrants(got, wantRuntimeGrants) {
		t.Errorf("%s holds %v, want exactly %v", database.RuntimeRole, got, wantRuntimeGrants)
	}
	// The server's role holds nothing of its own beyond logging in.
	if got := runtimeGrants(t, d.Admin, database.ServerRole); !slices.Equal(got, []string{"database telflo_opamp_server_database_test CONNECT"}) {
		t.Errorf("%s holds %v directly, want only CONNECT", database.ServerRole, got)
	}
	// And PUBLIC, which every role is a member of, holds nothing on the
	// database: neither CONNECT nor TEMPORARY.
	if got := publicDatabaseGrants(t, d.Admin); len(got) != 0 {
		t.Errorf("PUBLIC holds %v on the database, want nothing", got)
	}

	// As Postgres decides it, through the server role's membership.
	checks := []struct {
		query string
		want  bool
	}{
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.publications', 'SELECT, INSERT, DELETE')", true},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.publications', 'UPDATE')", false},
		{"select has_table_privilege('telflo_opamp_server_runtime', 'opamp_server.publications', 'UPDATE')", false},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.publications', 'TRUNCATE')", false},
		// Organizations may be read, registered, renamed and deleted; a rename
		// touches name and updated_at only. The limits may be set.
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.organizations', 'SELECT, INSERT, DELETE')", true},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.organizations', 'UPDATE')", false},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.organizations', 'TRUNCATE')", false},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.organizations', 'name', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.organizations', 'updated_at', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.organizations', 'max_collectors', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.organizations', 'requests_per_minute', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.organizations', 'id', 'UPDATE')", false},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.organizations', 'created_at', 'UPDATE')", false},
		{"select has_any_column_privilege('telflo_opamp_server', 'opamp_server.publications', 'UPDATE')", false},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.goose_db_version', 'SELECT')", true},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.goose_db_version', 'INSERT, UPDATE, DELETE')", false},
		// The fleet model. Rows the server adds and removes; tokens and token
		// events it never deletes.
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.fleets', 'SELECT, INSERT, DELETE')", true},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.collector_groups', 'SELECT, INSERT, DELETE')", true},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.collector_instances', 'SELECT, INSERT, DELETE')", true},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.collector_errors', 'SELECT, INSERT, DELETE')", true},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.enrollment_tokens', 'SELECT, INSERT')", true},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.enrollment_token_events', 'SELECT, INSERT')", true},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.enrollment_tokens', 'DELETE')", false},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.enrollment_token_events', 'DELETE')", false},
		{"select has_any_column_privilege('telflo_opamp_server', 'opamp_server.enrollment_token_events', 'UPDATE')", false},
		{"select has_any_column_privilege('telflo_opamp_server', 'opamp_server.collector_errors', 'UPDATE')", false},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.fleets', 'type', 'UPDATE')", false},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.collector_groups', 'is_default', 'UPDATE')", false},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.collector_instances', 'fleet_id', 'UPDATE')", false},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.enrollment_tokens', 'kind', 'UPDATE')", false},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.collector_instances', 'last_seen_at', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.enrollment_tokens', 'revoked_at', 'UPDATE')", true},
		{"select has_schema_privilege('telflo_opamp_server', 'opamp_server', 'CREATE')", false},
		{"select has_schema_privilege('telflo_opamp_server', 'public', 'CREATE')", false},
		{"select has_database_privilege('telflo_opamp_server', current_database(), 'CREATE')", false},
		// No temporary tables either, through PUBLIC or not.
		{"select has_database_privilege('telflo_opamp_server', current_database(), 'TEMPORARY')", false},
		{"select has_database_privilege('telflo_opamp_server_runtime', current_database(), 'TEMPORARY')", false},
		{"select has_database_privilege('telflo_opamp_server', current_database(), 'CONNECT')", true},
		// Migration 00012: the variables' tables. Rows the server adds,
		// changes and removes; the names a published version references it
		// only inserts. Each column grant, and none on an id, an
		// organization, an override's keys or any column of the names table.
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.organizations', 'variables_revision', 'UPDATE')", true},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.variables', 'SELECT, INSERT, DELETE')", true},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.variable_groups', 'SELECT, INSERT, DELETE')", true},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.variable_group_overrides', 'SELECT, INSERT, DELETE')", true},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.publication_variable_names', 'SELECT, INSERT')", true},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.publication_variable_names', 'DELETE')", false},
		{"select has_any_column_privilege('telflo_opamp_server', 'opamp_server.publication_variable_names', 'UPDATE')", false},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.variables', 'UPDATE')", false},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.variable_groups', 'UPDATE')", false},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.variable_group_overrides', 'UPDATE')", false},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.variables', 'TRUNCATE')", false},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.variable_groups', 'TRUNCATE')", false},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.variable_group_overrides', 'TRUNCATE')", false},
		{"select has_table_privilege('telflo_opamp_server', 'opamp_server.publication_variable_names', 'TRUNCATE')", false},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variables', 'name', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variables', 'description', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variables', 'value', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variables', 'changed_by_id', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variables', 'changed_by_email', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variables', 'changed_at', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variables', 'stamp', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variables', 'id', 'UPDATE')", false},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variables', 'organization_id', 'UPDATE')", false},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variable_groups', 'name', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variable_groups', 'description', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variable_groups', 'changed_by_id', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variable_groups', 'changed_by_email', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variable_groups', 'changed_at', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variable_groups', 'id', 'UPDATE')", false},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variable_groups', 'organization_id', 'UPDATE')", false},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variable_group_overrides', 'value', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variable_group_overrides', 'changed_by_id', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variable_group_overrides', 'changed_by_email', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variable_group_overrides', 'changed_at', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variable_group_overrides', 'stamp', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variable_group_overrides', 'organization_id', 'UPDATE')", false},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variable_group_overrides', 'variable_group_id', 'UPDATE')", false},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.variable_group_overrides', 'variable_id', 'UPDATE')", false},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.collector_groups', 'variable_group_id', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.collector_groups', 'variable_group_stamp', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.collector_instances', 'pinned_variable_group_id', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.collector_instances', 'pinned_variable_group_stamp', 'UPDATE')", true},
		{"select has_column_privilege('telflo_opamp_server', 'opamp_server.collector_instances', 'served_variables_stamp', 'UPDATE')", true},
	}
	for _, check := range checks {
		var got bool
		if err := d.Admin.QueryRow(ctx, check.query).Scan(&got); err != nil {
			t.Fatalf("%s: %v", check.query, err)
		}
		if got != check.want {
			t.Errorf("%s = %v, want %v", check.query, got, check.want)
		}
	}

	// And as the server: no DDL, no update of a stored publication, an
	// organization's id or created_at, and none of the fleet model's immutable
	// columns: a fleet's type, a group's is_default and fleet, a collector's
	// fleet and instance uid, a token's kind and fleet, any id, organization
	// or created_at; no update or delete of a token event, no delete of a
	// token, no truncate.
	for _, statement := range []string{
		"create table opamp_server.not_allowed (id integer)",
		"create table public.not_allowed (id integer)",
		"update opamp_server.publications set yaml = ''",
		"update opamp_server.organizations set id = gen_random_uuid()",
		"update opamp_server.organizations set created_at = updated_at",
		"truncate opamp_server.organizations",
		"update opamp_server.fleets set type = 'server'",
		"update opamp_server.fleets set id = id",
		"update opamp_server.fleets set organization_id = organization_id",
		"update opamp_server.fleets set created_at = updated_at",
		"update opamp_server.collector_groups set is_default = false",
		"update opamp_server.collector_groups set fleet_id = fleet_id",
		"update opamp_server.collector_groups set organization_id = organization_id",
		"update opamp_server.collector_groups set created_at = updated_at",
		"update opamp_server.enrollment_tokens set kind = 'rotate'",
		"update opamp_server.enrollment_tokens set fleet_id = fleet_id",
		"update opamp_server.enrollment_tokens set organization_id = organization_id",
		"update opamp_server.enrollment_tokens set created_at = created_at",
		"delete from opamp_server.enrollment_tokens",
		"update opamp_server.enrollment_token_events set event = 'viewed'",
		"delete from opamp_server.enrollment_token_events",
		"update opamp_server.collector_instances set fleet_id = fleet_id",
		"update opamp_server.collector_instances set instance_uid = instance_uid",
		"update opamp_server.collector_instances set organization_id = organization_id",
		"update opamp_server.collector_instances set created_at = updated_at",
		"update opamp_server.collector_errors set message = 'x'",
		"truncate opamp_server.fleets",
		"truncate opamp_server.collector_instances",
		"truncate opamp_server.collector_errors",
		// Migration 00012.
		"update opamp_server.variables set organization_id = organization_id",
		"update opamp_server.variables set id = id",
		"update opamp_server.variable_groups set organization_id = organization_id",
		"update opamp_server.variable_groups set id = id",
		"update opamp_server.variable_group_overrides set organization_id = organization_id",
		"update opamp_server.variable_group_overrides set variable_group_id = variable_group_id",
		"update opamp_server.variable_group_overrides set variable_id = variable_id",
		"update opamp_server.publication_variable_names set name = 'X'",
		"update opamp_server.publication_variable_names set publication_id = publication_id",
		"delete from opamp_server.publication_variable_names",
		"truncate opamp_server.variables",
		"truncate opamp_server.variable_groups",
		"truncate opamp_server.variable_group_overrides",
		"truncate opamp_server.publication_variable_names",
	} {
		_, err := d.Runtime.Exec(ctx, statement)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%q as telflo_opamp_server: err = %v, want 42501 insufficient_privilege", statement, err)
		}
	}
}

// publicDatabaseGrants lists what PUBLIC holds on this database (its ACL
// entries with grantee 0).
func publicDatabaseGrants(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		select a.privilege_type
		  from pg_database d, aclexplode(d.datacl) a
		 where d.datname = current_database() and a.grantee = 0
		 order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return grants
}

// The deliberate break, run every time: TEMPORARY given back to PUBLIC is
// what the test above must see.
func TestPrivilegeCheckSeesTemporaryGrantedToPublic(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	ctx := context.Background()
	if _, err := d.Admin.Exec(ctx, "grant temporary on database "+testDatabase+" to public"); err != nil {
		t.Fatal(err)
	}
	if got := publicDatabaseGrants(t, d.Admin); !slices.Equal(got, []string{"TEMPORARY"}) {
		t.Fatalf("the check saw %v, want TEMPORARY", got)
	}
	var canTemp bool
	if err := d.Admin.QueryRow(ctx,
		"select has_database_privilege('telflo_opamp_server', current_database(), 'TEMPORARY')").Scan(&canTemp); err != nil || !canTemp {
		t.Fatalf("has_database_privilege = %v, %v; want the server's role to see it through PUBLIC", canTemp, err)
	}
}

// The deliberate break, run every time: UPDATE or DELETE granted to the
// runtime role on a table it never changes that way is what the test above
// must see.
func TestPrivilegeCheckSeesAGrantedUpdate(t *testing.T) {
	for _, c := range []struct{ grant, want string }{
		{"grant update on opamp_server.publications to telflo_opamp_server_runtime", "relation opamp_server.publications UPDATE"},
		{"grant update on opamp_server.enrollment_token_events to telflo_opamp_server_runtime", "relation opamp_server.enrollment_token_events UPDATE"},
		{"grant delete on opamp_server.enrollment_tokens to telflo_opamp_server_runtime", "relation opamp_server.enrollment_tokens DELETE"},
	} {
		t.Run(c.want, func(t *testing.T) {
			d := databasetest.New(t, testDatabase)
			if _, err := d.Admin.Exec(context.Background(), c.grant); err != nil {
				t.Fatal(err)
			}
			got := runtimeGrants(t, d.Admin, database.RuntimeRole)
			if sameGrants(got, wantRuntimeGrants) || !slices.Contains(got, c.want) {
				t.Fatalf("the check saw %v, want %q among them", got, c.want)
			}
		})
	}
}

// The deliberate break, run every time: UPDATE on a column that never
// changes (an organization's created_at, which a rename never touches, and
// the fleet model's immutable columns) is what the test above must see.
func TestPrivilegeCheckSeesAGrantedColumnUpdate(t *testing.T) {
	for _, column := range []string{
		"organizations.created_at",
		"fleets.type",
		"collector_groups.is_default",
		"collector_instances.fleet_id",
		"enrollment_tokens.kind",
	} {
		t.Run(column, func(t *testing.T) {
			d := databasetest.New(t, testDatabase)
			table, name, _ := strings.Cut(column, ".")
			if _, err := d.Admin.Exec(context.Background(),
				"grant update ("+name+") on opamp_server."+table+" to telflo_opamp_server_runtime"); err != nil {
				t.Fatal(err)
			}
			got := runtimeGrants(t, d.Admin, database.RuntimeRole)
			want := "column opamp_server." + column + " UPDATE"
			if sameGrants(got, wantRuntimeGrants) || !slices.Contains(got, want) {
				t.Fatalf("the check saw %v, want %q among them", got, want)
			}
		})
	}
}

func TestStartupChecksPassAsTheServerRoleOnACurrentSchema(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	if err := database.CheckNoImportsTable(context.Background(), d.Runtime); err != nil {
		t.Fatalf("CheckNoImportsTable as telflo_opamp_server: %v", err)
	}
	if err := database.CheckStartup(context.Background(), d.Runtime); err != nil {
		t.Fatalf("CheckStartup as telflo_opamp_server: %v", err)
	}
}

// importsTableDatabase makes the package's database look as one the old
// migrations made, at the old version recorded (9: the imports; 10: the
// collectors, with import columns): migrations 1 to 8 as they were (the
// same as today's), today's 9 for the collectors and their errors, the old
// 9's imports table, which marks such a database, and its version rows. It
// answers the migrator, open as the owner.
func importsTableDatabase(t *testing.T, recorded int64) (*databasetest.Databases, *database.Migrator) {
	t.Helper()
	d := databasetest.New(t, testDatabase, databasetest.Unmigrated())
	ctx := context.Background()
	migrator, err := database.OpenMigrator(ctx, d.OwnerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = migrator.Close() })
	upTo := int64(8)
	if recorded == 10 {
		upTo = 9
	}
	if _, err := database.MigrateUpTo(ctx, migrator, upTo); err != nil {
		t.Fatal(err)
	}
	statements := []string{`
		create table opamp_server.imports (
		  id               uuid  primary key,
		  organization_id  uuid  not null,
		  fleet_id         uuid  not null,
		  constraint fk_imports_fleet
		    foreign key (organization_id, fleet_id)
		    references opamp_server.fleets (organization_id, id) on delete cascade
		)`,
		"grant select, insert on opamp_server.imports to telflo_opamp_server_runtime",
	}
	for v := upTo + 1; v <= recorded; v++ {
		statements = append(statements, fmt.Sprintf(
			"insert into opamp_server.goose_db_version (version_id, is_applied) values (%d, true)", v))
	}
	for _, statement := range statements {
		if _, err := d.Owner.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	return d, migrator
}

// assertNoAnnotations checks migration 11 left nothing behind.
func assertNoAnnotations(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(), `
		select exists (select 1 from information_schema.columns
		  where table_schema = 'opamp_server' and table_name = 'collector_groups'
		    and column_name = 'annotations')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("collector_groups.annotations exists: migration 11 went in on a database the old migrations made")
	}
}

// A database the old migrations made is at goose version 10 (the old 9,
// imports, and the old 10, the collectors with import columns). Today's
// migration 10 reuses that number, so goose finds only 11 pending, and the
// version alone would pass serve's check once it went in. Migration 11
// refuses it, leaving it at 10, and serve refuses it before its version
// check (which would say to run `migrate up`).
func TestAPreD16cDatabaseIsRefusedByMigrateUpAndServe(t *testing.T) {
	d, migrator := importsTableDatabase(t, 10)
	ctx := context.Background()

	if err := database.CheckStartup(ctx, d.Runtime); err == nil || err.Error() != database.ImportsTableRefusal {
		t.Fatalf("CheckStartup() before migrate up = %v, want %q", err, database.ImportsTableRefusal)
	}
	results, err := migrator.Up(ctx)
	const want = `this database holds the old imports table (opamp_server.imports): recreate it (see the opamp-server README, "A database holding the old imports table")`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("up = %v (applied %v), want the refusal %q", err, appliedVersions(results), want)
	}
	if got := appliedVersions(results); len(got) != 0 {
		t.Fatalf("up applied %v, want nothing", got)
	}
	assertStatus(t, migrator, 10)
	assertNoAnnotations(t, d.Owner)
	if err := database.CheckStartup(ctx, d.Runtime); err == nil || err.Error() != database.ImportsTableRefusal {
		t.Fatalf("CheckStartup() after migrate up = %v, want %q", err, database.ImportsTableRefusal)
	}
}

// One left at the old 9 (the imports only) finds today's 10 pending, which
// refuses it with 11's guard, untouched: still at 9, the default token's
// index not narrowed; serve refuses it as well.
func TestAPreD16cDatabaseAtTheOldNineIsRefusedUntouched(t *testing.T) {
	d, migrator := importsTableDatabase(t, 9)
	ctx := context.Background()

	results, err := migrator.Up(ctx)
	const want = `this database holds the old imports table (opamp_server.imports): recreate it (see the opamp-server README, "A database holding the old imports table")`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("up = %v (applied %v), want the refusal %q", err, appliedVersions(results), want)
	}
	if got := appliedVersions(results); len(got) != 0 {
		t.Fatalf("up applied %v, want nothing", got)
	}
	assertStatus(t, migrator, 9)
	assertNoAnnotations(t, d.Owner)
	var indexes []string
	rows, err := d.Owner.Query(ctx, `select indexname from pg_indexes
		where schemaname = 'opamp_server' and tablename = 'enrollment_tokens'
		  and indexname like 'uq_enrollment_tokens_fleet_id_where_%' order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	if indexes, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(indexes, []string{"uq_enrollment_tokens_fleet_id_where_kind_default"}) {
		t.Fatalf("the default token's indexes are %v, want 00008's alone (migration 10 never went in)", indexes)
	}
	if err := database.CheckStartup(ctx, d.Runtime); err == nil || err.Error() != database.ImportsTableRefusal {
		t.Fatalf("CheckStartup() = %v, want %q", err, database.ImportsTableRefusal)
	}
}

func TestAMissingVersionTableCountsAsVersionZero(t *testing.T) {
	d := databasetest.New(t, testDatabase, databasetest.Unmigrated())
	ctx := context.Background()

	version, err := database.SchemaVersion(ctx, d.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	if version != 0 {
		t.Fatalf("SchemaVersion() = %d on an empty database, want 0", version)
	}
	err = database.CheckStartup(ctx, d.Runtime)
	want := fmt.Sprintf("Refusing to start: the schema is at 0, this build needs %d: run `opamp-server migrate up`",
		latestMigration(t))
	if err == nil || err.Error() != want {
		t.Fatalf("CheckStartup() = %v, want %q", err, want)
	}
}

// Goose's older API and CLI record a rolled-back migration as a row with
// is_applied false. Such a row, even for a higher version, is not the
// schema's version.
func TestSchemaVersionCountsOnlyAppliedRows(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	ctx := context.Background()
	latest := latestMigration(t)
	if _, err := d.Owner.Exec(ctx,
		"insert into opamp_server.goose_db_version (version_id, is_applied) values ($1, false)", latest+1); err != nil {
		t.Fatal(err)
	}

	version, err := database.SchemaVersion(ctx, d.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	if version != latest {
		t.Fatalf("SchemaVersion() = %d with a not-applied row for %d, want %d", version, latest+1, latest)
	}
	if err := database.CheckStartup(ctx, d.Runtime); err != nil {
		t.Fatalf("CheckStartup() = %v, want it to pass", err)
	}
}

func TestCheckRoleRefusesARoleWithoutTheRuntimeRolesPrivileges(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	ctx := context.Background()
	name, roleURL := d.ThrowawayRole(t, "nosuperuser nobypassrls")

	pool, err := database.OpenPool(ctx, roleURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	err = database.CheckRole(ctx, pool)
	want := `Refusing to start: the database role "` + name + `" doesn't inherit telflo_opamp_server_runtime's privileges.`
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("CheckRole() = %v, want it to start %q", err, want)
	}
}

// A role that may create objects, in the database or in the schema,
// directly or through PUBLIC, is refused: the server runs no DDL.
func TestCheckRoleRefusesARoleThatMayCreateInTheDatabaseOrTheSchema(t *testing.T) {
	cases := []struct {
		name, grant, revoke, want string
	}{
		{"create on the database", "grant create on database {database} to {role}", "revoke create on database {database} from {role}",
			"may create objects in the database"},
		{"create on the schema", "grant create on schema opamp_server to {role}", "revoke create on schema opamp_server from {role}",
			"may create objects in the opamp_server schema"},
		{"create on the schema, through PUBLIC", "grant create on schema opamp_server to public", "revoke create on schema opamp_server from public",
			"may create objects in the opamp_server schema"},
		{"both", "grant create on database {database} to {role}; grant create on schema opamp_server to {role}",
			"revoke create on database {database} from {role}; revoke create on schema opamp_server from {role}",
			"may create objects in the database and in the opamp_server schema"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := databasetest.New(t, testDatabase)
			ctx := context.Background()
			// Otherwise a role serve would accept: only the grant refuses it.
			name, roleURL := d.ThrowawayRole(t, "nosuperuser nobypassrls")
			if _, err := d.Admin.Exec(ctx, "grant telflo_opamp_server_runtime to "+name+" with inherit true"); err != nil {
				t.Fatal(err)
			}
			pool, err := database.OpenPool(ctx, roleURL)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			if err := database.CheckRole(ctx, pool); err != nil {
				t.Fatalf("before the grant, CheckRole() = %v", err)
			}
			fill := strings.NewReplacer("{database}", testDatabase, "{role}", name).Replace

			// Revoked before the throwaway role's own cleanup drops it: a
			// role holding a privilege can't be dropped.
			t.Cleanup(func() {
				if _, err := d.Admin.Exec(context.Background(), fill(c.revoke)); err != nil {
					t.Errorf("revoking: %v", err)
				}
			})
			if _, err := d.Admin.Exec(ctx, fill(c.grant)); err != nil {
				t.Fatal(err)
			}
			err = database.CheckRole(ctx, pool)
			want := `Refusing to start: the database role "` + name + `" ` + c.want + `.`
			if err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("CheckRole() = %v, want it to start %q", err, want)
			}
		})
	}
}

// `serve` refuses a database that isn't UTF8. The server lets a superuser
// make a SQL_ASCII one (it stores bytes unchecked), which is the case shown
// here, on a database made for this test.
func TestCheckStartupRefusesADatabaseThatIsNotUTF8(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	ctx := context.Background()
	const asciiDatabase = "telflo_opamp_server_database_ascii_test"
	if err := databasetest.CheckName(asciiDatabase); err != nil {
		t.Fatal(err)
	}
	drop := func() error {
		_, err := d.Admin.Exec(context.Background(), "drop database if exists "+asciiDatabase+" with (force)")
		return err
	}
	if err := drop(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := drop(); err != nil {
			t.Errorf("dropping %s: %v", asciiDatabase, err)
		}
	})
	for _, statement := range []string{
		"create database " + asciiDatabase + " owner telflo_opamp_server_owner encoding 'SQL_ASCII' locale 'C' template template0",
		"grant connect on database " + asciiDatabase + " to telflo_opamp_server",
	} {
		if _, err := d.Admin.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	runtimeURL, err := databasetest.URLFor(d.AdminURL, database.ServerRole, databasetest.RuntimePassword, asciiDatabase)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := database.OpenPool(ctx, runtimeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	want := "Refusing to start: the database's encoding is SQL_ASCII, and the opamp-server needs UTF8. " +
		"Create the database with deploy/bootstrap.sql, which asks for UTF8 whatever the server's default"
	if err := database.CheckEncoding(ctx, pool); err == nil || err.Error() != want {
		t.Fatalf("CheckEncoding() = %v, want %q", err, want)
	}
	if err := database.CheckStartup(ctx, pool); err == nil || err.Error() != want {
		t.Fatalf("CheckStartup() = %v, want %q", err, want)
	}
	// The harness's own database is UTF8, as bootstrap.sql makes it.
	if err := database.CheckEncoding(ctx, d.Runtime); err != nil {
		t.Fatalf("CheckEncoding() on the test database = %v", err)
	}
}

// shapedRole is a throwaway login role serve would accept, as bootstrap.sql
// shapes telflo_opamp_server: no attribute but LOGIN, a member of
// telflo_opamp_server_runtime with inheritance, its errors terse. attributes add
// only what a test is about. The shared telflo_opamp_server is never altered:
// other packages' tests use it at the same time. The pool is closed before
// the role is dropped.
func shapedRole(t *testing.T, d *databasetest.Databases, attributes string) (string, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	name, roleURL := d.ThrowawayRole(t, "nosuperuser nobypassrls "+attributes)
	for _, statement := range []string{
		"grant telflo_opamp_server_runtime to " + name + " with inherit true",
		"alter role " + name + " set log_error_verbosity = 'terse'",
	} {
		if _, err := d.Admin.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := database.OpenPool(ctx, roleURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return name, pool
}

// assertShapeRefused checks that CheckBootstrapShape, and CheckStartup
// through it, refuse with exactly want, quoting neither the role's name nor
// the database's (nothing read from the database).
func assertShapeRefused(t *testing.T, pool *pgxpool.Pool, want string, unquoted ...string) {
	t.Helper()
	ctx := context.Background()
	for _, check := range []struct {
		name string
		run  func(context.Context, *pgxpool.Pool) error
	}{{"CheckBootstrapShape", database.CheckBootstrapShape}, {"CheckStartup", database.CheckStartup}} {
		err := check.run(ctx, pool)
		if err == nil {
			t.Fatalf("%s() passed, want %q", check.name, want)
		}
		for _, text := range append(unquoted, testDatabase, "SQLSTATE") {
			if strings.Contains(err.Error(), text) {
				t.Fatalf("%s()'s refusal quotes %q", check.name, text)
			}
		}
		if err.Error() != want {
			t.Fatalf("%s() = %q, want %q", check.name, err, want)
		}
	}
}

// A role serve would accept passes the shape checks: the harness's
// database and roles are bootstrap.sql's shape.
func TestCheckBootstrapShapePassesABootstrapShapedRoleAndDatabase(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	ctx := context.Background()
	_, pool := shapedRole(t, d, "")
	for _, p := range []*pgxpool.Pool{d.Runtime, pool} {
		if err := database.CheckBootstrapShape(ctx, p); err != nil {
			t.Fatalf("CheckBootstrapShape() = %v", err)
		}
		if err := database.CheckStartup(ctx, p); err != nil {
			t.Fatalf("CheckStartup() = %v", err)
		}
	}
}

func TestCheckStartupRefusesARoleWithCreateDB(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	name, pool := shapedRole(t, d, "createdb")
	assertShapeRefused(t, pool, database.CreateDBRefusal, name)
}

func TestCheckStartupRefusesARoleWithCreateRole(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	name, pool := shapedRole(t, d, "createrole")
	assertShapeRefused(t, pool, database.CreateRoleRefusal, name)
}

// PUBLIC's grants are the package's own database's: granting them alters
// no shared role.
func TestCheckStartupRefusesADatabasePublicMayConnectTo(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	if _, err := d.Admin.Exec(context.Background(), "grant connect on database "+testDatabase+" to public"); err != nil {
		t.Fatal(err)
	}
	assertShapeRefused(t, d.Runtime, database.PublicConnectRefusal, database.ServerRole)
}

func TestCheckStartupRefusesADatabasePublicMayMakeTemporaryTablesIn(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	if _, err := d.Admin.Exec(context.Background(), "grant temporary on database "+testDatabase+" to public"); err != nil {
		t.Fatal(err)
	}
	assertShapeRefused(t, d.Runtime, database.PublicTemporaryRefusal, database.ServerRole)
}

// A role setting applies when a session starts, so the pool is reset after
// it changes. 'default' rather than a reset: a server whose own setting is
// terse would pass a role without one, as it should.
func TestCheckStartupRefusesARoleWhoseErrorsArentTerse(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	ctx := context.Background()
	name, pool := shapedRole(t, d, "")
	if err := database.CheckStartup(ctx, pool); err != nil {
		t.Fatalf("CheckStartup() with terse errors = %v", err)
	}
	if _, err := d.Admin.Exec(ctx, "alter role "+name+" set log_error_verbosity = 'default'"); err != nil {
		t.Fatal(err)
	}
	pool.Reset()
	assertShapeRefused(t, pool, database.VerbosityRefusal, name)
}

func appliedVersions(results []*goose.MigrationResult) []int64 {
	var versions []int64
	for _, result := range results {
		versions = append(versions, result.Source.Version)
	}
	return versions
}

// assertStatus checks the schema is at wantCurrent: every embedded
// migration listed, those up to it applied and the rest pending.
func assertStatus(t *testing.T, migrator *database.Migrator, wantCurrent int64) {
	t.Helper()
	current, statuses, err := migrator.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if current != wantCurrent {
		t.Errorf("status: current version %d, want %d", current, wantCurrent)
	}
	if latest := latestMigration(t); int64(len(statuses)) != latest {
		t.Fatalf("status: %d migrations, want %d", len(statuses), latest)
	}
	for i, status := range statuses {
		want := goose.StatePending
		if status.Source.Version <= wantCurrent {
			want = goose.StateApplied
		}
		if status.Source.Version != int64(i+1) || status.State != want {
			t.Errorf("status: migration %d is %s, want %s", status.Source.Version, status.State, want)
		}
	}
}

func assertTableExists(t *testing.T, pool *pgxpool.Pool, table string, want bool) {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(),
		"select to_regclass('opamp_server.' || $1) is not null", table).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists != want {
		t.Fatalf("opamp_server.%s exists = %v, want %v", table, exists, want)
	}
}
