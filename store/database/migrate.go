package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/telflo/opamp-server/migrations"
)

// Migrator applies the embedded migrations. It connects as the owner, with
// OPAMP_MIGRATION_DATABASE_URL; the server's own role can't run DDL.
type Migrator struct {
	db       *sql.DB
	provider *goose.Provider
}

// The bounds of one OpenMigrator: pgx's connect_timeout when the URL sets
// none (it bounds the whole handshake: the dial, TLS and authentication),
// and a deadline on the open as a whole, the connection and the checks
// after it. Without them a server that takes the connection but never
// answers (a frozen process, a peer gone without a reset) holds migrate
// for as long as TCP keeps the connection, unbounded or many minutes.
// Variables, so a test can shorten them.
var (
	connectTimeout = 10 * time.Second
	openTimeout    = 30 * time.Second
)

// ErrUnavailable marks an OpenMigrator error that says the database can't
// be reached or can't answer yet (unavailable): worth another attempt,
// where any other error (a URL that doesn't parse, a failed
// authentication, a refusal) is one no retry changes.
var ErrUnavailable = errors.New("the database is unavailable")

// unavailableError is an error marked ErrUnavailable.
type unavailableError struct{ message string }

func (e *unavailableError) Error() string        { return e.message }
func (e *unavailableError) Is(target error) bool { return target == ErrUnavailable }

// failure is message followed by err's description (DescribeError), marked
// ErrUnavailable when err is one a later attempt may not meet.
func failure(message string, err error) error {
	text := message + DescribeError(err)
	if unavailable(err) {
		return &unavailableError{text}
	}
	return errors.New(text)
}

// OpenMigrator connects to the database named by url and prepares goose:
// it refuses a server where telflo_opamp_server_runtime doesn't exist (the
// migrations grant to it, so bootstrap.sql must have run) and a role that
// doesn't own the database (checkOwnsDatabase), creates the
// opamp_server schema if missing (goose's version table lives in it), and
// takes goose's Postgres session lock while it migrates, so two `migrate`
// runs at once take turns. Each migration runs in a transaction. It gives
// up after openTimeout, its connection after connect_timeout (connectTimeout
// when the URL sets none or 0), and marks an error that another attempt may
// not meet ErrUnavailable; it never retries itself. Its errors never quote
// the URL or the database driver's text (DescribeError); the errors of Up,
// Down and Status are goose's, for the caller to describe.
func OpenMigrator(ctx context.Context, url string) (*Migrator, error) {
	return openMigrator(ctx, url, RuntimeRole)
}

// openMigrator is OpenMigrator with the role whose absence it refuses named,
// so a test can show the refusal without dropping the server-wide role that
// other test packages are using.
func openMigrator(ctx context.Context, url, runtimeRole string) (*Migrator, error) {
	config, err := parseConfig(url)
	if err != nil {
		return nil, err
	}
	config.RuntimeParams["application_name"] = applicationName + " migrate"
	if config.ConnectTimeout == 0 {
		config.ConnectTimeout = connectTimeout
	}
	db := stdlib.OpenDB(*config)

	// The deadline bounds the open only: the connection it leaves open, and
	// goose's work on it, outlive it.
	openCtx, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	migrator, err := newMigrator(openCtx, db, runtimeRole)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return migrator, nil
}

func newMigrator(ctx context.Context, db *sql.DB, runtimeRole string) (*Migrator, error) {
	if err := db.PingContext(ctx); err != nil {
		return nil, failure("can't connect to the database: ", err)
	}

	var runtimeRoleExists bool
	if err := db.QueryRowContext(ctx,
		`select exists (select 1 from pg_roles where rolname = $1)`, runtimeRole,
	).Scan(&runtimeRoleExists); err != nil {
		return nil, failure("can't look up the role "+runtimeRole+": ", err)
	}
	if !runtimeRoleExists {
		return nil, fmt.Errorf(
			"refusing to migrate: the role %s doesn't exist on this server; "+
				"run %s as a superuser first (it creates the database and its roles)",
			runtimeRole, BootstrapFile)
	}

	if err := checkOwnsDatabase(ctx, db); err != nil {
		return nil, err
	}

	// goose creates its version table but not the schema it names. The owner
	// owns the database, so it may create the schema, and so owns it too.
	if _, err := db.ExecContext(ctx,
		"create schema if not exists "+pgx.Identifier{Schema}.Sanitize(),
	); err != nil {
		return nil, failure("can't create the schema "+Schema+": ", err)
	}

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, errors.New("can't prepare goose's lock: " + DescribeError(err))
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS,
		goose.WithTableName(VersionTable),
		goose.WithSessionLocker(locker),
		// Only the embedded files: nothing registered in Go by any package.
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		return nil, errors.New("can't prepare the migrations: " + DescribeError(err))
	}
	return &Migrator{db: db, provider: provider}, nil
}

// Up applies every pending migration, in order.
func (m *Migrator) Up(ctx context.Context) ([]*goose.MigrationResult, error) {
	return m.provider.Up(ctx)
}

// Down rolls back the newest applied migration (for development: rolling
// back production is a restore).
func (m *Migrator) Down(ctx context.Context) (*goose.MigrationResult, error) {
	return m.provider.Down(ctx)
}

// Status reports the database's current version and each embedded
// migration's state.
func (m *Migrator) Status(ctx context.Context) (int64, []*goose.MigrationStatus, error) {
	statuses, err := m.provider.Status(ctx)
	if err != nil {
		return 0, nil, err
	}
	current, err := m.provider.GetDBVersion(ctx)
	if err != nil {
		return 0, nil, err
	}
	return current, statuses, nil
}

// Close closes the connection.
func (m *Migrator) Close() error {
	return m.db.Close()
}

// LatestMigration is the newest version this build embeds: the version
// `serve` needs the database to be at.
func LatestMigration() (int64, error) {
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return 0, err
	}
	var latest int64
	for _, name := range names {
		version, err := goose.NumericComponent(name)
		if err != nil {
			return 0, fmt.Errorf("migration %s: %w", name, err)
		}
		latest = max(latest, version)
	}
	if latest == 0 {
		return 0, fmt.Errorf("this build embeds no migrations")
	}
	return latest, nil
}

// checkOwnsDatabase refuses to migrate as any role but the database's owner
// (telflo_opamp_server_owner, which bootstrap.sql makes the owner), with a message
// saying what to set, rather than the bare "permission denied" (42501) the
// first `create schema` would otherwise end on.
//
// The rule is "is the database's owner", exactly: not a member of it, and
// not a superuser. Whoever runs the migrations owns what they create, and
// every later migration must be able to alter it, so it must always be the
// same role: a superuser's or a member's run would leave the schema and its
// tables owned by that role, out of the owner's reach (and a superuser
// would hide a missing grant that the real owner then hits in production).
// Checking the database's owner rather than the name telflo_opamp_server_owner
// also serves a self-hoster whose bootstrap names the owner differently.
func checkOwnsDatabase(ctx context.Context, db *sql.DB) error {
	var role, databaseName, owner string
	if err := db.QueryRowContext(ctx, `
		select current_user, d.datname, pg_get_userbyid(d.datdba)
		  from pg_database d
		 where d.datname = current_database()`,
	).Scan(&role, &databaseName, &owner); err != nil {
		return failure("can't look up the database's owner: ", err)
	}
	if role != owner {
		return fmt.Errorf(
			"refusing to migrate: the role %q doesn't own the database %q (%q does). "+
				"Migrations run as the database's owner, %s, which %s makes: "+
				"set OPAMP_MIGRATION_DATABASE_URL to connect as it",
			role, databaseName, owner, OwnerRole, BootstrapFile)
	}
	return nil
}
