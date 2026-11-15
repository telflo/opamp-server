package database

import (
	"context"

	"github.com/pressly/goose/v3"
)

// OpenMigratorRequiring is OpenMigrator refusing when runtimeRole is missing,
// so a test can show the refusal with a role name nobody has, rather than
// drop the server-wide telflo_opamp_server_runtime other test packages are using.
var OpenMigratorRequiring = openMigrator

// MigrateUpTo applies the embedded migrations up to and including version,
// so a test can put rows in an older schema before migrating it further. It
// answers what it applied.
func MigrateUpTo(ctx context.Context, m *Migrator, version int64) ([]*goose.MigrationResult, error) {
	return m.provider.UpTo(ctx, version)
}
