//lint:file-ignore ST1005 each refusal is the whole message serve logs before it exits, written in sentences for the operator

package database

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CheckStartup runs every check `serve` makes before it opens a listener:
// first the role it connects as, then the database's encoding, then the
// rest of the bootstrap's shape, then that it wasn't migrated by the old
// migrations (CheckNoImportsTable), then the schema's version. Each
// failure is one error naming the problem and what to do, never the URL,
// and never the database driver's text (DescribeError): the pool connects
// lazily, so the first query here is also where a connection that fails is
// reported.
func CheckStartup(ctx context.Context, pool *pgxpool.Pool) error {
	if err := CheckRole(ctx, pool); err != nil {
		return err
	}
	if err := CheckEncoding(ctx, pool); err != nil {
		return err
	}
	if err := CheckBootstrapShape(ctx, pool); err != nil {
		return err
	}
	if err := CheckNoImportsTable(ctx, pool); err != nil {
		return err
	}
	latest, err := LatestMigration()
	if err != nil {
		return err
	}
	return CheckSchemaVersion(ctx, pool, latest)
}

// roleHint ends every role refusal: what OPAMP_DATABASE_URL should name.
const roleHint = "Set OPAMP_DATABASE_URL to the server's own login role " +
	"(" + ServerRole + ", made by " + BootstrapFile + "), a member of " + RuntimeRole +
	", and OPAMP_MIGRATION_DATABASE_URL to " + OwnerRole + "."

// CheckRole refuses a role that would put the server above its grants: a
// superuser, a role with BYPASSRLS (no table has row-level security yet,
// but the server's role must never be one a later policy can't hold), the
// owner or a member of it, a role owning anything in the schema (an owner
// skips every grant on what it owns), or a role that may create in the
// database or in the schema (the server runs no DDL, so a right to make
// objects is one it must not hold). As the platform's assertApplicationRole
// does, it also refuses a role that doesn't inherit telflo_opamp_server_runtime's
// privileges, since the server can do nothing without them.
func CheckRole(ctx context.Context, pool *pgxpool.Pool) error {
	var (
		name           string
		isSuperuser    bool
		bypassesRLS    bool
		isOwnerMember  bool
		ownsObject     bool
		isRuntimeGrant bool
		createsInDB    bool
		createsInSch   bool
	)
	// pg_has_role raises for a role that doesn't exist, so each call is
	// guarded. MEMBER covers a role that could SET ROLE to the owner even
	// without inheriting from it; USAGE (inherited privileges) is what the
	// runtime role's grants need.
	err := pool.QueryRow(ctx, `
		select
		  r.rolname,
		  r.rolsuper,
		  r.rolbypassrls,
		  case when exists (select 1 from pg_roles where rolname = $1)
		    then pg_has_role(current_user, $1::name, 'MEMBER')
		    else false
		  end,
		  exists (
		    select 1 from pg_namespace n
		     where n.nspname = $3 and pg_has_role(current_user, n.nspowner, 'MEMBER')
		    union all
		    select 1 from pg_class c join pg_namespace n on n.oid = c.relnamespace
		     where n.nspname = $3 and pg_has_role(current_user, c.relowner, 'MEMBER')
		    union all
		    select 1 from pg_proc p join pg_namespace n on n.oid = p.pronamespace
		     where n.nspname = $3 and pg_has_role(current_user, p.proowner, 'MEMBER')
		    union all
		    select 1 from pg_type t join pg_namespace n on n.oid = t.typnamespace
		     where n.nspname = $3 and pg_has_role(current_user, t.typowner, 'MEMBER')
		  ),
		  case when exists (select 1 from pg_roles where rolname = $2)
		    then pg_has_role(current_user, $2::name, 'USAGE')
		    else false
		  end,
		  has_database_privilege(current_database(), 'CREATE'),
		  coalesce((select has_schema_privilege(n.oid, 'CREATE') from pg_namespace n where n.nspname = $3), false)
		from pg_roles r
		where r.rolname = current_user`,
		OwnerRole, RuntimeRole, Schema,
	).Scan(&name, &isSuperuser, &bypassesRLS, &isOwnerMember, &ownsObject, &isRuntimeGrant, &createsInDB, &createsInSch)
	if err != nil {
		return errors.New("Refusing to start: can't check the database role: " + DescribeError(err))
	}

	var problems []string
	if isSuperuser {
		// A superuser is a member of every role and passes every privilege
		// check, so the other findings would only repeat this one.
		problems = append(problems, "is a superuser")
	} else {
		if bypassesRLS {
			problems = append(problems, "has BYPASSRLS")
		}
		if isOwnerMember {
			problems = append(problems, "is "+OwnerRole+" or a member of it")
		}
		if ownsObject {
			problems = append(problems, "owns objects in the "+Schema+" schema, or is a member of their owner")
		}
		// The owner may create anywhere in its database, which the finding
		// above already says.
		if !isOwnerMember {
			switch {
			case createsInDB && createsInSch:
				problems = append(problems, "may create objects in the database and in the "+Schema+" schema")
			case createsInDB:
				problems = append(problems, "may create objects in the database")
			case createsInSch:
				problems = append(problems, "may create objects in the "+Schema+" schema")
			}
		}
		if !isRuntimeGrant {
			problems = append(problems, "doesn't inherit "+RuntimeRole+"'s privileges")
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("Refusing to start: the database role %q %s. %s",
			name, strings.Join(problems, ", "), roleHint)
	}
	return nil
}

// RequiredEncoding is the only server_encoding `serve` accepts: the API
// takes any Unicode text but U+0000, which a database in another encoding
// would refuse or, as SQL_ASCII does, store without checking.
const RequiredEncoding = "UTF8"

// CheckEncoding refuses a database whose encoding isn't UTF8.
func CheckEncoding(ctx context.Context, pool *pgxpool.Pool) error {
	var encoding string
	if err := pool.QueryRow(ctx, "select current_setting('server_encoding')").Scan(&encoding); err != nil {
		return errors.New("Refusing to start: can't read the database's encoding: " + DescribeError(err))
	}
	return checkEncoding(encoding)
}

// checkEncoding is CheckEncoding's decision, on the server_encoding read.
func checkEncoding(encoding string) error {
	if encoding != RequiredEncoding {
		return fmt.Errorf("Refusing to start: the database's encoding is %s, and the opamp-server needs %s. "+
			"Create the database with %s, which asks for %s whatever the server's default",
			encoding, RequiredEncoding, BootstrapFile, RequiredEncoding)
	}
	return nil
}

// The refusals of CheckBootstrapShape, one per finding. Each names the
// setting and the statement that fixes it, with the role and the database
// as words: nothing read from the database is quoted.
const (
	CreateDBRefusal = "Refusing to start: the server's database role has CREATEDB, so it may create databases, " +
		"which it never needs. Remove it with `alter role <the server's role> nocreatedb`, " +
		"as a superuser or your provider's admin role"
	CreateRoleRefusal = "Refusing to start: the server's database role has CREATEROLE, so it may create and change roles, " +
		"which it never needs. Remove it with `alter role <the server's role> nocreaterole`, " +
		"as a superuser or your provider's admin role"
	PublicConnectRefusal = "Refusing to start: PUBLIC holds CONNECT on the database, so every role on the Postgres server " +
		"may connect to it. Revoke it with `revoke connect on database <the database> from public`, " +
		"then `grant connect on database <the database> to <the server's role>`, as the database's owner or a superuser"
	PublicTemporaryRefusal = "Refusing to start: PUBLIC holds TEMPORARY on the database, so every role that connects " +
		"may create temporary tables in it. Revoke it with `revoke temporary on database <the database> from public`, " +
		"as the database's owner or a superuser"
	VerbosityRefusal = "Refusing to start: log_error_verbosity isn't terse for the server's database role, so Postgres's " +
		"own log may quote a value an error refused. Set it with " +
		"`alter role <the server's role> set log_error_verbosity = terse`, as a superuser, " +
		"or server-wide in your provider's parameter group " +
		`(see the opamp-server README, "A managed Postgres without a superuser")`
)

// CheckBootstrapShape refuses what the bootstrap leaves out and CheckRole
// doesn't look at: a role with CREATEDB or CREATEROLE, a database PUBLIC
// may connect to or make temporary tables in (a NULL acl, the defaults,
// gives PUBLIC both), and errors not logged terse (a verbose error's DETAIL
// can quote a value a unique key refused). The verbosity is the session's,
// so a provider's server-wide setting passes. It reads the catalogs only,
// as the running role, and answers the first finding, in that order.
func CheckBootstrapShape(ctx context.Context, pool *pgxpool.Pool) error {
	var createsDatabases, createsRoles, publicConnects, publicTemporary, terse bool
	if err := pool.QueryRow(ctx, `
		select
		  r.rolcreatedb,
		  r.rolcreaterole,
		  has_database_privilege('public', current_database(), 'CONNECT'),
		  has_database_privilege('public', current_database(), 'TEMPORARY'),
		  current_setting('log_error_verbosity') = 'terse'
		from pg_roles r
		where r.rolname = current_user`,
	).Scan(&createsDatabases, &createsRoles, &publicConnects, &publicTemporary, &terse); err != nil {
		return errors.New("Refusing to start: can't check the database role's attributes and the database's grants: " +
			DescribeError(err))
	}
	switch {
	case createsDatabases:
		return errors.New(CreateDBRefusal)
	case createsRoles:
		return errors.New(CreateRoleRefusal)
	case publicConnects:
		return errors.New(PublicConnectRefusal)
	case publicTemporary:
		return errors.New(PublicTemporaryRefusal)
	case !terse:
		return errors.New(VerbosityRefusal)
	}
	return nil
}

// ImportsTableRefusal is the refusal of a database the old migrations made:
// what CheckNoImportsTable answers.
const ImportsTableRefusal = "Refusing to start: this database holds the old imports table " +
	"(" + Schema + ".imports), and this release can't upgrade it: recreate it " +
	`(see the opamp-server README, "A database holding the old imports table")`

// CheckNoImportsTable refuses a database the old migrations made, an
// earlier set whose 9 made an imports table (README.md explains the
// upgrade). Those had the collectors at 10; today's make them 9, and 10 is
// another, so such a database's version alone can't tell it apart: at 10 it
// would be taken for one missing only migration 11 (which refuses it too).
// Its imports table, which no migration makes today, marks it. It is looked
// up through the catalog, which every role may read, as SchemaVersion does.
func CheckNoImportsTable(ctx context.Context, pool *pgxpool.Pool) error {
	var migratedBefore bool
	if err := pool.QueryRow(ctx, `
		select exists (
		  select 1 from pg_class c join pg_namespace n on n.oid = c.relnamespace
		   where n.nspname = $1 and c.relname = 'imports'
		)`,
		Schema,
	).Scan(&migratedBefore); err != nil {
		return errors.New("Refusing to start: can't check the schema's history: " + DescribeError(err))
	}
	if migratedBefore {
		return errors.New(ImportsTableRefusal)
	}
	return nil
}

// CheckSchemaVersion refuses a schema that isn't at exactly latest, the
// newest migration this build embeds. A version table that is missing, or
// that the role can't read (the schema before any migration, or after the
// last one is rolled back, which revokes the grant), counts as version 0.
func CheckSchemaVersion(ctx context.Context, pool *pgxpool.Pool, latest int64) error {
	current, err := SchemaVersion(ctx, pool)
	if err != nil {
		return errors.New("Refusing to start: can't read the schema's version: " + DescribeError(err))
	}
	switch {
	case current < latest:
		return fmt.Errorf("Refusing to start: the schema is at %d, this build needs %d: run `opamp-server migrate up`",
			current, latest)
	case current > latest:
		return fmt.Errorf("Refusing to start: the schema is at %d, which is newer than this build (it needs %d): "+
			"run the opamp-server release that migrated it", current, latest)
	}
	return nil
}

// SchemaVersion is the newest migration goose's version table records as
// applied, as the connected role sees it: 0 when the table is missing or the
// role can't read it.
func SchemaVersion(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	// Looked up through the catalog, which every role may read: to_regclass
	// raises, rather than answering null, for a schema the role has no
	// USAGE on.
	var readable bool
	if err := pool.QueryRow(ctx, `
		select coalesce((
		  select has_schema_privilege(n.oid, 'USAGE') and has_table_privilege(c.oid, 'SELECT')
		    from pg_class c join pg_namespace n on n.oid = c.relnamespace
		   where n.nspname = $1 and c.relname = 'goose_db_version'
		), false)`,
		Schema,
	).Scan(&readable); err != nil {
		return 0, err
	}
	if !readable {
		return 0, nil
	}
	// goose's Provider deletes a rolled-back migration's row, but goose's
	// older API (and its CLI) records a rollback as a row with is_applied
	// false instead, so only the rows marked applied count.
	var version int64
	if err := pool.QueryRow(ctx,
		`select coalesce(max(version_id), 0) from `+VersionTable+` where is_applied`,
	).Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
}
