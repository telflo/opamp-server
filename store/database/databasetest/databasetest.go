// Package databasetest gives tests a real, freshly made opamp-server
// database.
//
//   - One setting: OPAMP_TEST_ADMIN_DATABASE_URL, a superuser URL to the
//     server (locally postgresql://telflo:telflo-local@localhost:55432/postgres).
//     Without it, tests that need a database skip; CI sets it and fails on any
//     skipped test.
//   - One database per package, telflo_opamp_<package>_test, since `go test
//     ./...` runs packages in parallel. New drops and recreates it, so each
//     test that calls New starts from an empty, migrated database; a test
//     package's tests don't run in parallel with each other. The migrated
//     database is a copy of the package's template (TemplateName), which
//     the test process migrates once, on its first New.
//   - The roles are bootstrap.sql's, created here (as statements) when
//     missing, with the development passwords docker-compose.yml uses: roles
//     belong to the whole server, which the developer's own stack shares.
//
// Only a database whose name ends in _test is ever dropped or created.
package databasetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/telflo/opamp-server/store/database"
)

const (
	// AdminURLVariable names the superuser URL the harness starts from.
	AdminURLVariable = "OPAMP_TEST_ADMIN_DATABASE_URL"

	// OwnerPassword and RuntimePassword are the development passwords of
	// telflo_opamp_owner and telflo_opamp, the same as docker-compose.yml's.
	OwnerPassword   = "telflo-opamp-owner-local"
	RuntimePassword = "telflo-opamp-local"

	// PoolSize is each pool's most connections, whatever the machine:
	// pgx's default follows the CPU count (at least 4), and a race test
	// needs every racer waiting in Postgres at once, not some waiting in
	// the pool for a connection. The widest race has 5 racers; 10 is what
	// the suite has run with on a 10-core machine.
	PoolSize = 10

	testSuffix = "_test"

	// provisioningLock serializes role creation across test packages running
	// at once: a concurrent `create role` fails with 23505 or 42710.
	provisioningLock = "telflo_opamp.provision_test_roles"

	// setupTimeout bounds creating, migrating and dropping a database.
	setupTimeout = 60 * time.Second

	// maxIdentifierBytes is the longest name Postgres keeps whole.
	maxIdentifierBytes = 63
)

// Databases is one test database, reachable as each of the three roles.
type Databases struct {
	// Name is the database's name, ending in _test.
	Name string

	// AdminURL, OwnerURL and RuntimeURL name this database as the superuser,
	// telflo_opamp_owner and telflo_opamp.
	AdminURL   string
	OwnerURL   string
	RuntimeURL string

	// Admin is a superuser pool, for what neither role may do: throwaway
	// roles, deliberate catalog changes.
	Admin *pgxpool.Pool
	// Owner is telflo_opamp_owner's pool, for fixtures and reading back.
	Owner *pgxpool.Pool
	// Runtime is telflo_opamp's pool: what `serve` connects as.
	Runtime *pgxpool.Pool

	maintenanceURL string
}

type options struct {
	migrate bool
}

// Option changes what New prepares.
type Option func(*options)

// Unmigrated leaves the new database empty: no schema, no version table.
func Unmigrated() Option {
	return func(o *options) { o.migrate = false }
}

// AdminURL reads OPAMP_TEST_ADMIN_DATABASE_URL, skipping the test when it is
// unset.
func AdminURL(t testing.TB) string {
	t.Helper()
	adminURL := os.Getenv(AdminURLVariable)
	if adminURL == "" {
		t.Skipf("%s is not set; this test needs a Postgres server (CI sets it and fails on a skip)", AdminURLVariable)
	}
	return adminURL
}

// New drops and recreates the database name (which must end in _test),
// migrated to the newest migration unless Unmigrated is given, and drops it
// again when the test ends. It skips the test when OPAMP_TEST_ADMIN_DATABASE_URL
// is unset.
func New(t testing.TB, name string, opts ...Option) *Databases {
	t.Helper()
	adminURL := AdminURL(t)
	o := options{migrate: true}
	for _, opt := range opts {
		opt(&o)
	}

	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	databases, err := Prepare(ctx, adminURL, name, o.migrate)
	if err != nil {
		t.Fatalf("preparing the test database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
		defer cancel()
		if err := databases.Drop(ctx); err != nil {
			t.Errorf("dropping the test database: %v", err)
		}
	})
	return databases
}

// CheckName refuses a database name that doesn't end in _test: the guard
// that keeps the harness's `drop database` off any database not made for
// tests.
func CheckName(name string) error {
	if !strings.HasSuffix(name, testSuffix) || name == testSuffix {
		return fmt.Errorf("refusing to use %q as a test database: its name must end in %q", name, testSuffix)
	}
	return nil
}

// Prepare is New without a testing.TB: it creates the roles if missing,
// drops and recreates the database owned by telflo_opamp_owner, as
// bootstrap.sql would set it up, migrates it if migrate is true, and opens a
// pool as each role. The caller drops it with Drop.
func Prepare(ctx context.Context, adminURL, name string, migrate bool) (*Databases, error) {
	if err := CheckName(name); err != nil {
		return nil, err
	}
	maintenanceURL, err := withDatabase(adminURL, "postgres")
	if err != nil {
		return nil, err
	}

	maintenance, err := pgx.Connect(ctx, maintenanceURL)
	if err != nil {
		return nil, fmt.Errorf("connecting to the server: %w", err)
	}
	defer func() { _ = maintenance.Close(context.Background()) }()

	if err := createRoles(ctx, maintenance); err != nil {
		return nil, err
	}
	// A migrated database is a copy of the package's migrated template,
	// which this process builds once; an unmigrated one starts empty.
	template := "template0"
	if migrate {
		if template, err = migratedTemplate(ctx, maintenance, adminURL, name); err != nil {
			return nil, err
		}
	}
	if err := recreateDatabase(ctx, maintenance, name, template); err != nil {
		return nil, err
	}

	d := &Databases{Name: name, maintenanceURL: maintenanceURL}
	if d.AdminURL, err = withDatabase(adminURL, name); err != nil {
		return nil, err
	}
	if d.OwnerURL, err = URLFor(adminURL, database.OwnerRole, OwnerPassword, name); err != nil {
		return nil, err
	}
	if d.RuntimeURL, err = URLFor(adminURL, database.ServerRole, RuntimePassword, name); err != nil {
		return nil, err
	}

	for _, pool := range []struct {
		url    string
		target **pgxpool.Pool
	}{{d.AdminURL, &d.Admin}, {d.OwnerURL, &d.Owner}, {d.RuntimeURL, &d.Runtime}} {
		config, err := pgxpool.ParseConfig(pool.url)
		if err != nil {
			_ = d.Drop(ctx)
			return nil, fmt.Errorf("opening a pool: %w", err)
		}
		config.MaxConns = PoolSize
		if *pool.target, err = pgxpool.NewWithConfig(ctx, config); err != nil {
			_ = d.Drop(ctx)
			return nil, fmt.Errorf("opening a pool: %w", err)
		}
	}
	return d, nil
}

// Empty deletes every row of every table in the schema but goose's, as the
// owner: the rows of a freshly migrated database, which hold none, for a
// test that needs a fresh database once per case of many (a copy takes far
// longer). The schema, its grants and the roles stay as they are, so a
// test that changed any of those needs New instead.
func (d *Databases) Empty(ctx context.Context) error {
	rows, err := d.Owner.Query(ctx, `
		select format('%I.%I', schemaname, tablename)
		  from pg_tables
		 where schemaname = $1 and tablename <> 'goose_db_version'
		 order by 1`, database.Schema)
	if err != nil {
		return fmt.Errorf("listing the tables to empty: %w", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("listing the tables to empty: %w", err)
	}
	if _, err := d.Owner.Exec(ctx, "truncate table "+strings.Join(tables, ", ")+" restart identity"); err != nil {
		return fmt.Errorf("emptying the tables: %w", err)
	}
	return nil
}

// Drop closes the pools and drops the database, ending any connection still
// open to it.
func (d *Databases) Drop(ctx context.Context) error {
	for _, pool := range []*pgxpool.Pool{d.Admin, d.Owner, d.Runtime} {
		if pool != nil {
			pool.Close()
		}
	}
	if err := CheckName(d.Name); err != nil {
		return err
	}
	maintenance, err := pgx.Connect(ctx, d.maintenanceURL)
	if err != nil {
		return fmt.Errorf("connecting to the server: %w", err)
	}
	defer func() { _ = maintenance.Close(context.Background()) }()
	_, err = maintenance.Exec(ctx, "drop database if exists "+pgx.Identifier{d.Name}.Sanitize()+" with (force)")
	return err
}

// URLFor returns base with its user, password and database replaced.
func URLFor(base, user, password, databaseName string) (string, error) {
	u, err := parseURL(base)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword(user, password)
	u.Path = "/" + databaseName
	return u.String(), nil
}

func withDatabase(base, databaseName string) (string, error) {
	u, err := parseURL(base)
	if err != nil {
		return "", err
	}
	u.Path = "/" + databaseName
	return u.String(), nil
}

func parseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		// Never the URL itself: it holds the superuser's password.
		return nil, fmt.Errorf("%s must be a postgres:// or postgresql:// URL", AdminURLVariable)
	}
	return u, nil
}

// createRoles is bootstrap.sql's role part: the three roles, created only
// when missing, the server role's membership of the runtime role, and its
// log_error_verbosity.
func createRoles(ctx context.Context, conn *pgx.Conn) error {
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock(hashtext($1))", provisioningLock); err != nil {
			return err
		}
		roles := []struct{ name, create string }{
			{database.OwnerRole, "create role telflo_opamp_owner login nosuperuser nocreatedb nocreaterole nobypassrls password " + literal(OwnerPassword)},
			{database.RuntimeRole, "create role telflo_opamp_runtime nologin nosuperuser nocreatedb nocreaterole nobypassrls"},
			{database.ServerRole, "create role telflo_opamp login nosuperuser nocreatedb nocreaterole nobypassrls inherit password " + literal(RuntimePassword)},
		}
		for _, role := range roles {
			var exists bool
			if err := tx.QueryRow(ctx, "select exists (select 1 from pg_roles where rolname = $1)", role.name).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				if _, err := tx.Exec(ctx, role.create); err != nil {
					return fmt.Errorf("creating the role %s: %w", role.name, err)
				}
			}
		}
		var isMember bool
		if err := tx.QueryRow(ctx,
			"select exists (select 1 from pg_auth_members where roleid = $1::regrole and member = $2::regrole)",
			database.RuntimeRole, database.ServerRole,
		).Scan(&isMember); err != nil {
			return err
		}
		if !isMember {
			if _, err := tx.Exec(ctx, "grant telflo_opamp_runtime to telflo_opamp with inherit true"); err != nil {
				return fmt.Errorf("granting %s to %s: %w", database.RuntimeRole, database.ServerRole, err)
			}
		}
		// Its errors logged terse, as bootstrap.sql sets it.
		if _, err := tx.Exec(ctx, "alter role telflo_opamp set log_error_verbosity = 'terse'"); err != nil {
			return fmt.Errorf("setting %s's log_error_verbosity: %w", database.ServerRole, err)
		}
		return nil
	})
}

// templates are the migrated templates this process has built. Each is
// built on its first use in the process, so it always holds this binary's
// migrations, however old a template a previous run left behind.
var templates = struct {
	sync.Mutex
	built map[string]bool
}{built: map[string]bool{}}

// TemplateName is the name of the migrated template that the test database
// name is copied from: name with _template before its _test suffix.
func TemplateName(name string) string {
	return strings.TrimSuffix(name, testSuffix) + "_template" + testSuffix
}

// migratedTemplate returns the name of name's migrated template, building
// it first if this process hasn't: made as bootstrap.sql makes a database,
// migrated to the newest migration as the owner, and left with no session
// connected, since Postgres copies only a template nobody is connected to.
func migratedTemplate(ctx context.Context, conn *pgx.Conn, adminURL, name string) (string, error) {
	template := TemplateName(name)
	if len(template) > maxIdentifierBytes {
		return "", fmt.Errorf("the template name %q is over Postgres's %d bytes", template, maxIdentifierBytes)
	}
	templates.Lock()
	defer templates.Unlock()
	if templates.built[template] {
		return template, nil
	}
	if err := recreateDatabase(ctx, conn, template, "template0"); err != nil {
		return "", err
	}
	ownerURL, err := URLFor(adminURL, database.OwnerRole, OwnerPassword, template)
	if err != nil {
		return "", err
	}
	if err := migrateUp(ctx, ownerURL); err != nil {
		return "", err
	}
	if err := disconnectAll(ctx, conn, template); err != nil {
		return "", err
	}
	templates.built[template] = true
	return template, nil
}

// disconnectAll ends every session connected to the database name and waits
// until none is: a closed pool's backends can outlive its Close briefly.
func disconnectAll(ctx context.Context, conn *pgx.Conn, name string) error {
	for {
		var connected int
		err := conn.QueryRow(ctx, `
			select count(pg_terminate_backend(pid))
			  from pg_stat_activity
			 where datname = $1 and pid <> pg_backend_pid()`, name).Scan(&connected)
		if err != nil {
			return fmt.Errorf("disconnecting from the template %s: %w", name, err)
		}
		if connected == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("disconnecting from the template %s: %w", name, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// recreateDatabase is bootstrap.sql's database part, on a database dropped
// first, copied from template. A copy keeps the template's objects, their
// owners and their grants, but not the database's own privileges or its
// role settings, so these are set on every database, copy or not.
func recreateDatabase(ctx context.Context, conn *pgx.Conn, name, template string) error {
	if err := CheckName(name); err != nil {
		return err
	}
	quoted := pgx.Identifier{name}.Sanitize()
	for _, statement := range []string{
		"drop database if exists " + quoted + " with (force)",
		"create database " + quoted + " owner telflo_opamp_owner encoding 'UTF8' template " +
			pgx.Identifier{template}.Sanitize(),
		"revoke connect on database " + quoted + " from public",
		"grant connect on database " + quoted + " to telflo_opamp_owner, telflo_opamp",
		"revoke temporary on database " + quoted + " from public",
		"alter role telflo_opamp in database " + quoted + " set search_path = telflo_opamp",
	} {
		if _, err := conn.Exec(ctx, statement); err != nil {
			return fmt.Errorf("preparing the database %s: %w", name, err)
		}
	}
	return nil
}

func migrateUp(ctx context.Context, ownerURL string) error {
	migrator, err := database.OpenMigrator(ctx, ownerURL)
	if err != nil {
		return fmt.Errorf("migrating the test database: %w", err)
	}
	defer func() { _ = migrator.Close() }()
	if _, err := migrator.Up(ctx); err != nil {
		return fmt.Errorf("migrating the test database: %w", err)
	}
	return nil
}

// literal quotes s as an SQL string literal (standard_conforming_strings,
// Postgres's default). Only for the constant development passwords above:
// `create role … password` takes no bind parameter.
func literal(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// ThrowawayRole creates a login role with a unique name, a random password
// and the given attributes (say "superuser" or "bypassrls"), lets it connect
// to this database, and drops it when the test ends. It returns the role's
// name and a URL to this database as it. Roles belong to the whole server,
// so no two tests share one.
func (d *Databases) ThrowawayRole(t testing.TB, attributes string) (name, roleURL string) {
	t.Helper()
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name = "telflo_opamp_throwaway_" + hex.EncodeToString(suffix)
	password := hex.EncodeToString(suffix) + "-password"
	ctx := context.Background()

	quotedRole := pgx.Identifier{name}.Sanitize()
	quotedDatabase := pgx.Identifier{d.Name}.Sanitize()
	if _, err := d.Admin.Exec(ctx,
		"create role "+quotedRole+" login "+attributes+" password "+literal(password)); err != nil {
		t.Fatalf("creating a throwaway role: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
		defer cancel()
		// A role holding a privilege can't be dropped, and the database's
		// CONNECT is one.
		if _, err := d.Admin.Exec(ctx, "revoke connect on database "+quotedDatabase+" from "+quotedRole); err != nil {
			t.Errorf("revoking the throwaway role's CONNECT: %v", err)
		}
		if _, err := d.Admin.Exec(ctx, "drop role if exists "+quotedRole); err != nil {
			t.Errorf("dropping the throwaway role: %v", err)
		}
	})
	if _, err := d.Admin.Exec(ctx, "grant connect on database "+quotedDatabase+" to "+quotedRole); err != nil {
		t.Fatalf("granting the throwaway role CONNECT: %v", err)
	}

	roleURL, err := URLFor(d.AdminURL, name, password, d.Name)
	if err != nil {
		t.Fatal(err)
	}
	return name, roleURL
}
