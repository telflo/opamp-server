package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/telflo/opamp-server/store/database"
)

// yesDropData is the flag `migrate down` needs.
const yesDropData = "--yes-drop-data"

// refuseDown is `migrate down`'s refusal without yesDropData.
const refuseDown = "Refusing to migrate down: it rolls back the newest migration, dropping its tables " +
	"and every row in them. down is for development; to roll back a release, restore the backup " +
	"taken before `migrate up`. To roll back anyway, run `opamp-server migrate down " + yesDropData + "`."

// How long migrate keeps trying a database it can't reach or that can't
// answer yet (database.ErrUnavailable), as on a cluster's cold start, where
// the database may come up after migrate does: attempts first
// connectRetryFirst apart, the wait doubling up to connectRetryMax, none
// started after connectRetryBudget from the first and none running past it. Then it
// exits 1, and whatever runs it (Kubernetes restarts an init container)
// decides whether to run it again. Variables, so a test can shorten them.
var (
	connectRetryBudget = 2 * time.Minute
	connectRetryFirst  = time.Second
	connectRetryMax    = 10 * time.Second
)

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

	migrator, err := openMigrator(ctx, url, logger)
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

// openMigrator opens the migrator, trying again while the database is
// unavailable (database.ErrUnavailable) for up to connectRetryBudget, and
// logs each failed attempt with the wait before the next. Any other error,
// or a cancelled ctx, ends it at once. A last error that ended the retries
// says how many attempts it made and for how long.
func openMigrator(ctx context.Context, url string, logger *slog.Logger) (*database.Migrator, error) {
	start := time.Now()
	// No attempt runs past the budget: one still connecting when it ends
	// fails as a timeout.
	budgetCtx, cancel := context.WithDeadline(ctx, start.Add(connectRetryBudget))
	defer cancel()
	wait := connectRetryFirst
	for attempt := 1; ; attempt++ {
		migrator, err := database.OpenMigrator(budgetCtx, url)
		if err == nil || !errors.Is(err, database.ErrUnavailable) || ctx.Err() != nil {
			return migrator, err
		}
		// Stop rather than start an attempt the budget would cut short: its
		// timeout would hide why the attempts before it failed.
		if time.Until(start.Add(connectRetryBudget)) <= wait {
			return nil, &retriesError{err: err, attempts: attempt, elapsed: time.Since(start)}
		}
		logger.Warn("migrate: "+describeOpenError(err)+"; trying again",
			"attempt", attempt, "retryIn", wait.Round(time.Millisecond).String())
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, err
		case <-timer.C:
		}
		wait = min(2*wait, connectRetryMax)
	}
}

// retriesError is the last error of an openMigrator that ran out of time,
// with how many attempts it made and for how long.
type retriesError struct {
	err      error
	attempts int
	elapsed  time.Duration
}

func (e *retriesError) Error() string {
	return fmt.Sprintf("%s (gave up after %d attempts in %s)",
		e.err.Error(), e.attempts, e.elapsed.Round(time.Second))
}

func (e *retriesError) Unwrap() error { return e.err }

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
