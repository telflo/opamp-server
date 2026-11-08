// Package database is the opamp-server's own Postgres database
// (docs/decisions/0001-reached-only-through-its-api.md): the connection
// pool `serve` uses, the embedded migrations `migrate` applies, and the
// checks `serve` runs before it opens any listener.
//
// The database, its schema and its roles:
//   - database telflo_opamp, created with its roles by deploy/bootstrap.sql,
//     run by a superuser;
//   - schema telflo_opamp, created by `migrate`, named in every statement;
//   - telflo_opamp_owner owns everything and runs `migrate`
//     (OPAMP_MIGRATION_DATABASE_URL);
//   - telflo_opamp_runtime (NOLOGIN) holds what the server needs, granted by
//     the migrations;
//   - telflo_opamp logs in for `serve` (OPAMP_DATABASE_URL), a member of
//     telflo_opamp_runtime with no DDL rights.
//
// No URL or password from either setting is ever put in an error or a log
// line: a URL that doesn't parse is reported without its text.
package database

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// Schema holds every table the opamp-server has.
	Schema = "telflo_opamp"
	// OwnerRole owns the database, the schema and every table.
	OwnerRole = "telflo_opamp_owner"
	// RuntimeRole holds exactly the privileges `serve` needs.
	RuntimeRole = "telflo_opamp_runtime"
	// ServerRole is the login role bootstrap.sql makes for `serve`. Any login
	// role that is a member of RuntimeRole will do.
	ServerRole = "telflo_opamp"
	// VersionTable is goose's record of the applied migrations.
	VersionTable = Schema + ".goose_db_version"
	// ServerTable is the one table of the server's own, of no
	// organization: the management API's keys (migration 00013). Every
	// other table but goose's holds an organization's rows, with
	// organization_id and its cascade.
	ServerTable = "api_keys"

	// BootstrapFile is the script that creates the database and the roles.
	BootstrapFile = "deploy/bootstrap.sql"

	// applicationName labels the server's connections in pg_stat_activity.
	applicationName = "opamp-server"
)

// ErrInvalidURL is reported for a connection string that doesn't parse. The
// parser's own error quotes the string (only the password redacted), so it
// is never passed on.
var ErrInvalidURL = errors.New("is not a valid PostgreSQL connection URL")

// parseConfig parses a connection string without letting it reach an error.
func parseConfig(url string) (*pgx.ConnConfig, error) {
	config, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, ErrInvalidURL
	}
	return config, nil
}

// OpenPool returns a pool on the database named by url, which `serve`
// passes from OPAMP_DATABASE_URL. Connections open on first use, so a server
// that can't be reached is reported by the first query (CheckStartup's).
// Its size is the URL's pool_max_conns, else pgx's default, the greater of
// 4 and the number of CPUs the process sees.
func OpenPool(ctx context.Context, url string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, ErrInvalidURL
	}
	config.ConnConfig.RuntimeParams["application_name"] = applicationName
	return pgxpool.NewWithConfig(ctx, config)
}

// OpenReadinessPool returns a pool of one connection on the database named
// by url, for the agent listener's /ready alone: a probe that waited for
// one of the request pool's connections would call a pod unready whenever
// its requests were busy or slow, and every pod at once. Whatever the URL's
// pool_max_conns, it holds at most one connection: none until the first
// ping (MinConns 0), then that one kept open, idle between pings, for pgx's
// MaxConnIdleTime (30 minutes) after the last (so a ready pod holds it);
// each replica holds at most pool_max_conns + 1 connections. Its
// connections are labelled "opamp-server readiness" in pg_stat_activity.
func OpenReadinessPool(ctx context.Context, url string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, ErrInvalidURL
	}
	config.MaxConns, config.MinConns = 1, 0
	config.ConnConfig.RuntimeParams["application_name"] = applicationName + " readiness"
	return pgxpool.NewWithConfig(ctx, config)
}
