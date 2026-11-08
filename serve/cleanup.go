package serve

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/telflo/opamp-server/store/collectors"
)

// The error history's cleanup: a collector's error rows are kept
// collectors.ErrorRetention (30 days). Every replica runs the job, once at
// start and then every CleanupInterval; the store's advisory lock
// (collectors.CleanupLockKey) means one replica at a time deletes, and the
// others report they didn't hold it.
const (
	// CleanupInterval is how often the job runs, after its run at start.
	CleanupInterval = time.Hour
	// CleanupRunTimeout bounds one run, well under CleanupInterval, so a
	// stuck run never holds a connection into the next.
	CleanupRunTimeout = 10 * time.Minute
)

// ErrorCleaner is what the job runs: collectors.Store's Cleanup.
type ErrorCleaner interface {
	Cleanup(ctx context.Context, cutoff time.Time) (collectors.CleanupResult, error)
}

// cleanupJob deletes the error rows older than the retention, by the
// injected clock.
type cleanupJob struct {
	store  ErrorCleaner
	now    func() time.Time
	logger *slog.Logger
}

// run runs the job once, then once per tick, until ctx ends (serve's
// shutdown); it returns then. ticks is serve's ticker of CleanupInterval
// (a test's channel).
func (j cleanupJob) run(ctx context.Context, ticks <-chan time.Time) {
	j.once(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			j.once(ctx)
		}
	}
}

// once is one run: Cleanup(now − ErrorRetention), and one log line saying
// what it did (how many rows it deleted, or that another replica held the
// lock), or that it failed (the SQLSTATE, never the error's text). A run cut
// short by serve's shutdown writes nothing.
func (j cleanupJob) once(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, CleanupRunTimeout)
	defer cancel()
	result, err := j.store.Cleanup(ctx, j.now().Add(-collectors.ErrorRetention))
	switch {
	case err != nil && errors.Is(ctx.Err(), context.Canceled):
		return
	case err != nil:
		attrs := []any{}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			attrs = append(attrs, "sqlstate", pgErr.Code)
		}
		j.logger.Warn("the error history's cleanup failed; it runs again within the hour", attrs...)
	case !result.Held:
		j.logger.Info("the error history's cleanup didn't run here: another replica holds its lock", "held", false)
	default:
		j.logger.Info("the error history's cleanup ran", "held", true, "deleted", result.Deleted)
	}
}
