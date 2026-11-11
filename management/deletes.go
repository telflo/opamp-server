package management

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/telflo/telflo-opamp-server/internal/shutdown"
)

// DeleteTimeout bounds an organization's, a fleet's or a group's delete
// once it has started. The delete runs on a context that the caller hanging
// up doesn't cancel, so a cascade that outlasts the caller's own timeout
// (the platform waits 25 s; an organization of 40,000 collectors with their
// error history took about 8 s) still commits rather than rolling back and
// being repeated, in vain, by every retry. A retry that arrives once it has
// committed answers 404, which the platform counts as done. This bound is
// the one wall-clock limit such a delete has: the listener's 30 s write
// timeout only ends the reply's write, and nothing else would stop a delete
// that never finishes.
// A delete still running when serve is stopped and its in-flight requests
// are cut off (ShutdownTimeout) is cancelled with them and rolls back: the
// stop stays bounded (the pool's close waits for every connection), and
// the platform's retry runs it again on a replica that is serving.
const DeleteTimeout = 5 * time.Minute

// deleteInProgressRetryAfter is the Retry-After, in seconds, of the 503 a
// delete gets while this replica is already running the same one: the lower
// end of the 503's band elsewhere in the server
// (middleware.UnavailableRetryAfterMin). The platform's retries keep their
// own schedule and don't read it.
const deleteInProgressRetryAfter = 10

// The causes a delete's context ends with, which its failure's log line
// names (deleteFailed).
var (
	errDeleteTimedOut  = errors.New("management: the delete's own bound passed")
	errDeleteForcedOff = errors.New("management: serve's forced stop cut the delete off")
)

// deleteContext is the context an organization's, a fleet's or a group's
// delete runs on (DeleteTimeout; a group's too, as it deletes the group's
// collectors): the request's values (its span, for the database span's
// parent) without its cancellation, bounded by the server's delete
// timeout, and still cancelled by serve's forced stop. Its cause says which
// ended it (errDeleteTimedOut, errDeleteForcedOff). The reply is written if
// the caller is still there. The returned func releases it.
func (s *Server) deleteContext(r *http.Request) (context.Context, context.CancelFunc) {
	bounded, cancelBound := context.WithTimeoutCause(context.WithoutCancel(r.Context()), s.deleteTimeout, errDeleteTimedOut)
	ctx, cancel := context.WithCancelCause(bounded)
	release := func() {
		cancel(nil)
		cancelBound()
	}
	stop, ok := shutdown.ForcedStop(r.Context())
	if !ok {
		return ctx, release
	}
	unhook := context.AfterFunc(stop, func() { cancel(errDeleteForcedOff) })
	return ctx, func() {
		unhook()
		release()
	}
}

// deleteFailed is storeFailed for a delete that ran on deleteContext: when
// that context had ended, the log line's cause says what ended it (the
// delete's own bound, or serve's forced stop), never the request's context,
// which no longer bounds it.
func (s *Server) deleteFailed(w http.ResponseWriter, r *http.Request, store string, ctx context.Context, err error) {
	attributes := []any{"route", s.routePattern(r)}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		attributes = append(attributes, "sqlstate", pgErr.Code)
	}
	if ctx.Err() != nil {
		switch cause := context.Cause(ctx); {
		case errors.Is(cause, errDeleteTimedOut):
			attributes = append(attributes, "cause", "the delete's own bound passed", "deleteTimeout", s.deleteTimeout.String())
		case errors.Is(cause, errDeleteForcedOff):
			attributes = append(attributes, "cause", "serve's forced stop cut the delete off")
		default:
			attributes = append(attributes, "cause", "the delete's context ended")
		}
	}
	s.logger.Error("the "+store+" store failed", attributes...)
	writeError(w, http.StatusInternalServerError, codeInternalError, messageInternalError)
}

// fleetDeleteKey names a fleet's delete in deletesInProgress.
type fleetDeleteKey struct {
	organizationID uuid.UUID
	fleetID        uuid.UUID
}

// deletesInProgress is the organizations and fleets this replica is
// deleting. A second delete of one of them, a retry from a caller that gave
// up on the first, would otherwise wait behind the first's row lock holding
// a pool connection for as long as the first runs (up to DeleteTimeout); it
// is answered 503 delete_in_progress at once instead, and its retry, once
// the first has committed, answers 404. A fleet's delete is refused too
// while its organization's delete runs (that delete is deleting the fleet).
// An organization's delete isn't refused while one of its fleets' runs: it
// isn't a repeat, it waits behind that one fleet's rows, and a repeat of it
// is refused in turn. Only this replica's deletes are known: one running on
// another replica is still waited for.
type deletesInProgress struct {
	mu            sync.Mutex
	organizations map[uuid.UUID]struct{}
	fleets        map[fleetDeleteKey]struct{}
}

// startOrganization marks the organization's delete started, or reports
// false when this replica is already deleting it. The returned func,
// deferred by the handler (so it runs once the reply is written), unmarks it.
func (d *deletesInProgress) startOrganization(organizationID uuid.UUID) (func(), bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, running := d.organizations[organizationID]; running {
		return nil, false
	}
	if d.organizations == nil {
		d.organizations = map[uuid.UUID]struct{}{}
	}
	d.organizations[organizationID] = struct{}{}
	return func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		delete(d.organizations, organizationID)
	}, true
}

// startFleet marks the fleet's delete started, or reports false when this
// replica is already deleting it or its organization. The returned func,
// deferred by the handler (so it runs once the reply is written), unmarks it.
func (d *deletesInProgress) startFleet(organizationID, fleetID uuid.UUID) (func(), bool) {
	key := fleetDeleteKey{organizationID: organizationID, fleetID: fleetID}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, running := d.organizations[organizationID]; running {
		return nil, false
	}
	if _, running := d.fleets[key]; running {
		return nil, false
	}
	if d.fleets == nil {
		d.fleets = map[fleetDeleteKey]struct{}{}
	}
	d.fleets[key] = struct{}{}
	return func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		delete(d.fleets, key)
	}, true
}

// writeDeleteInProgress answers 503 delete_in_progress with its
// Retry-After, and logs the route (never an id: the route's pattern).
func (s *Server) writeDeleteInProgress(w http.ResponseWriter, r *http.Request) {
	s.logger.Info("a delete this replica is already running was asked for again: answered 503", "route", s.routePattern(r))
	w.Header().Set("Retry-After", strconv.Itoa(deleteInProgressRetryAfter))
	writeError(w, http.StatusServiceUnavailable, codeDeleteInProgress, messageDeleteInProgress)
}
