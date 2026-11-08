package main

import (
	"context"
	"errors"
	"log/slog"
	"path"

	"github.com/pressly/goose/v3"

	"github.com/telflo/opamp-server/store/database"
)

// yesDropData is the flag `migrate down` needs.
const yesDropData = "--yes-drop-data"

// refuseDown is `migrate down`'s refusal without yesDropData.
const refuseDown = "Refusing to migrate down: it rolls back the newest migration, dropping its tables " +
	"and every row in them. down is for development; to roll back a release, restore the backup " +
	"taken before `migrate up`. To roll back anyway, run `opamp-server migrate down " + yesDropData + "`."

// migrate runs `opamp-server migrate up|status` or `migrate down
// --yes-drop-data` as the owner, with OPAMP_MIGRATION_DATABASE_URL: a
// separate step, in a one-shot container before the server, so the server
// never needs the owner's rights. args are the words after `migrate`. No
// line it logs quotes the database driver's text (database.DescribeError).
func migrate(ctx context.Context, args []string, getenv func(string) string, logger *slog.Logger) int {
	if len(args) == 0 {
		logger.Error(usage)
		return 2
	}
	command := args[0]
	switch {
	case (command == "up" || command == "status") && len(args) == 1:
	case command == "down" && len(args) == 1:
		// Refused before anything connects.
		logger.Error(refuseDown)
		return 1
	case command == "down" && len(args) == 2 && args[1] == yesDropData:
	default:
		logger.Error(usage)
		return 2
	}
	url, err := requiredURL(getenv, migrationDatabaseURLVariable)
	if err != nil {
		logger.Error("migrate: " + database.DescribeError(err))
		return 1
	}

	migrator, err := database.OpenMigrator(ctx, url)
	if err != nil {
		logger.Error("migrate: " + describeOpenError(err))
		return 1
	}
	defer func() { _ = migrator.Close() }()

	switch command {
	case "up":
		results, err := migrator.Up(ctx)
		logResults(logger, results...)
		if err != nil {
			logger.Error("migrate up failed", "error", database.DescribeError(err))
			return 1
		}
		if len(results) == 0 {
			logger.Info("no migration to apply")
		}
	case "down":
		result, err := migrator.Down(ctx)
		if errors.Is(err, goose.ErrNoNextVersion) {
			logger.Info("no migration to roll back")
			return 0
		}
		if result != nil {
			logResults(logger, result)
		}
		if err != nil {
			logger.Error("migrate down failed", "error", database.DescribeError(err))
			return 1
		}
	case "status":
		current, statuses, err := migrator.Status(ctx)
		if err != nil {
			logger.Error("migrate status failed", "error", database.DescribeError(err))
			return 1
		}
		for _, status := range statuses {
			attributes := []any{
				"version", status.Source.Version,
				"file", path.Base(status.Source.Path),
				"state", string(status.State),
			}
			if !status.AppliedAt.IsZero() {
				attributes = append(attributes, "appliedAt", status.AppliedAt)
			}
			logger.Info("migration", attributes...)
		}
		latest, err := database.LatestMigration()
		if err != nil {
			logger.Error("migrate status failed", "error", database.DescribeError(err))
			return 1
		}
		logger.Info("schema version", "current", current, "latest", latest)
	}
	return 0
}

// describeOpenError names what went wrong opening the migrator. Its errors
// never carry the URL or the database driver's text (database.OpenMigrator's
// contract); DescribeError keeps it so should one ever wrap a driver error.
func describeOpenError(err error) string {
	if errors.Is(err, database.ErrInvalidURL) {
		return migrationDatabaseURLVariable + " " + database.DescribeError(err)
	}
	return database.DescribeError(err)
}

func logResults(logger *slog.Logger, results ...*goose.MigrationResult) {
	for _, result := range results {
		if result == nil || result.Source == nil {
			continue
		}
		attributes := []any{
			"version", result.Source.Version,
			"file", path.Base(result.Source.Path),
			"direction", result.Direction,
			"duration", result.Duration.String(),
		}
		if result.Error != nil {
			logger.Error("migration failed", append(attributes, "error", database.DescribeError(result.Error))...)
			continue
		}
		logger.Info("migration run", attributes...)
	}
}
