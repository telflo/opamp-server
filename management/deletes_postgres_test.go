package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/telflo/opamp-server/internal/shutdown"
	"github.com/telflo/opamp-server/store/database/databasetest"
)

// groupUnderDelete is the group the group's case of deletesUnderLock
// deletes: fleet A's, with one collector.
const groupUnderDelete = "6c6c6c6c-0000-4000-8000-00000000006c"

// deleteUnderLock is one of the deletes that finish once started (a
// group's among them, which deletes its collectors): the DELETE's target,
// the statements (the superuser's) that make what it deletes beyond
// organization A's fleet A, a statement that holds a lock the delete's
// transaction waits for once it has begun, the pg_stat_activity query text
// of the statement that waits, and how many of the rows it deletes remain
// (2 before it).
type deleteUnderLock struct {
	name    string
	target  string
	setup   []string
	lock    string
	waiting string
	remains string
	// notFound and notFoundMessage are a retry's answer once the delete
	// is done.
	notFound        errorCode
	notFoundMessage string
	// waitsBehind: a repeat of the delete while it runs waits behind it
	// (a group's) rather than being answered 503 delete_in_progress at
	// once (an organization's and a fleet's).
	waitsBehind bool
}

func deletesUnderLock() []deleteUnderLock {
	return []deleteUnderLock{{
		name:   "an organization's",
		target: organizationTarget(orgA),
		// FOR KEY SHARE conflicts with the delete's FOR UPDATE on the row.
		lock:            `select 1 from opamp_server.organizations where id = '` + orgA + `' for key share`,
		waiting:         `%from opamp_server.organizations%for update%`,
		remains:         `select (select count(*) from opamp_server.organizations where id = '` + orgA + `') + (select count(*) from opamp_server.fleets where organization_id = '` + orgA + `')`,
		notFound:        codeNotFound,
		notFoundMessage: messageNotFound,
	}, {
		name:            "a fleet's",
		target:          fleetTarget(orgA, fleetA),
		lock:            `select 1 from opamp_server.fleets where organization_id = '` + orgA + `' and id = '` + fleetA + `' for key share`,
		waiting:         `%from opamp_server.fleets%for update%`,
		remains:         `select (select count(*) from opamp_server.fleets where organization_id = '` + orgA + `' and id = '` + fleetA + `') + (select count(*) from opamp_server.collector_groups where organization_id = '` + orgA + `' and fleet_id = '` + fleetA + `')`,
		notFound:        codeFleetNotFound,
		notFoundMessage: messageFleetNotFound,
	}, {
		name:   "a group's",
		target: groupTarget(orgA, fleetA, groupUnderDelete),
		setup: []string{
			`insert into opamp_server.collector_groups (id, organization_id, fleet_id, name, is_default, created_at, updated_at)
			 values ('` + groupUnderDelete + `', '` + orgA + `', '` + fleetA + `', 'doomed', false, now(), now())`,
			`insert into opamp_server.collector_instances (id, organization_id, fleet_id, collector_group_id,
			   instance_uid, name, last_seen_at, created_at, updated_at)
			 values (gen_random_uuid(), '` + orgA + `', '` + fleetA + `', '` + groupUnderDelete + `', gen_random_uuid(), 'edge-01', now(), now(), now())`,
		},
		lock:            `select 1 from opamp_server.collector_groups where organization_id = '` + orgA + `' and id = '` + groupUnderDelete + `' for key share`,
		waiting:         `%from opamp_server.collector_groups%for update%`,
		remains:         `select (select count(*) from opamp_server.collector_groups where organization_id = '` + orgA + `' and id = '` + groupUnderDelete + `') + (select count(*) from opamp_server.collector_instances where organization_id = '` + orgA + `' and collector_group_id = '` + groupUnderDelete + `')`,
		notFound:        codeCollectorGroupNotFound,
		notFoundMessage: messageCollectorGroupNotFound,
		waitsBehind:     true,
	}}
}

// prepareDeleteUnderLock creates organization A's fleet A and runs c's
// setup.
func prepareDeleteUnderLock(t *testing.T, s *Server, d *databasetest.Databases, c deleteUnderLock) {
	t.Helper()
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	for _, statement := range c.setup {
		if _, err := d.Admin.Exec(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
}

// startDeleteUnderLock prepares c (prepareDeleteUnderLock), takes its lock in
// a transaction of the superuser's, sends c's DELETE on ctx in the
// background, and, when waitForIt, waits until the delete's transaction is
// blocked on the lock (bounded by attempts). It returns the lock's
// transaction and the channel that answers the DELETE.
func startDeleteUnderLock(t *testing.T, s *Server, d *databasetest.Databases, c deleteUnderLock, ctx context.Context, waitForIt bool) (pgx.Tx, <-chan *httptest.ResponseRecorder) {
	t.Helper()
	prepareDeleteUnderLock(t, s, d, c)
	background := context.Background()
	lock, err := d.Admin.Begin(background)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Rollback(background) })
	if _, err := lock.Exec(background, c.lock); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodDelete, c.target, nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+testSecret)
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		s.Handler().ServeHTTP(recorder, request)
		answered <- recorder
	}()

	if !waitForIt {
		return lock, answered
	}
	awaitWaitingDeletes(t, d, c, 1)
	return lock, answered
}

// waitingDeletes is how many of the runtime role's sessions wait on a lock
// running c's delete statement.
func waitingDeletes(t *testing.T, d *databasetest.Databases, c deleteUnderLock) int {
	t.Helper()
	var n int
	if err := d.Admin.QueryRow(context.Background(), `
		select count(*) from pg_stat_activity
		 where datname = current_database()
		   and usename = 'telflo_opamp_server'
		   and wait_event_type = 'Lock'
		   and query like $1`, c.waiting).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// awaitWaitingDeletes waits until want of c's deletes wait on the lock,
// bounded by attempts, not a fixed wait (each one sleeps briefly).
func awaitWaitingDeletes(t *testing.T, d *databasetest.Databases, c deleteUnderLock, want int) {
	t.Helper()
	n := -1
	for attempt := 0; attempt < 500; attempt++ {
		if n = waitingDeletes(t, d, c); n == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%d deletes waited on the lock, want %d", n, want)
}

// awaitAnswer is the DELETE's answer, with a hang guard.
func awaitAnswer(t *testing.T, answered <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case got := <-answered:
		return got
	case <-time.After(30 * time.Second):
		t.Fatal("the DELETE never answered")
		return nil
	}
}

func remainingRows(t *testing.T, d *databasetest.Databases, c deleteUnderLock) int {
	t.Helper()
	var n int
	if err := d.Admin.QueryRow(context.Background(), c.remains).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// An organization's, a fleet's or a group's delete whose caller hangs up
// while its transaction waits still commits (a group's with its
// collector): the request's context is cancelled mid-transaction, the lock
// is then released, and the delete answers 204 to a caller still there
// (the recorder) with everything gone; a retry answers 404, which the
// platform counts as done. On the request's own context, the cancellation
// would have rolled it back (500, every row still there).
func TestADeleteWhoseCallerHangsUpStillCommits(t *testing.T) {
	for _, c := range deletesUnderLock() {
		t.Run(c.name, func(t *testing.T) {
			s, _, log, d := postgresFleetServer(t)
			ctx, hangUp := context.WithCancel(context.Background())
			defer hangUp()
			lock, answered := startDeleteUnderLock(t, s, d, c, ctx, true)

			hangUp()
			if err := lock.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := awaitAnswer(t, answered)
			if got.Code != http.StatusNoContent {
				t.Fatalf("the DELETE answered %d %s, want 204; log: %s", got.Code, got.Body.String(), log.String())
			}
			if n := remainingRows(t, d, c); n != 0 {
				t.Fatalf("%d rows remain after the delete, want 0", n)
			}
			retry := call(s, http.MethodDelete, c.target, nil, "")
			expectError(t, "a retry", retry, http.StatusNotFound, c.notFound, c.notFoundMessage)
		})
	}
}

// A group's delete that finds the group gone once it gets the row (here
// deleted by a superuser's transaction it waited for) answers 404 from a
// read of the fleet on the delete's own context too: its caller hanging up
// meanwhile neither fails that read nor logs a failure. On the request's
// context, the read would fail (500, "the fleets store failed").
func TestAGroupsDeleteFindingTheGroupGoneAnswers404AfterItsCallerHangsUp(t *testing.T) {
	s, _, log, d := postgresFleetServer(t)
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	expectStatus(t, "the group", call(s, http.MethodPost, groupsTarget(orgA, fleetA), []byte(`{"name":"doomed"}`), jsonType),
		http.StatusCreated)
	group := groupID(t, s, orgA, fleetA, "doomed")
	c := deleteUnderLock{target: groupTarget(orgA, fleetA, group), waiting: `%from opamp_server.collector_groups%for update%`}
	background := context.Background()
	gone, err := d.Admin.Begin(background)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gone.Rollback(background) })
	if _, err := gone.Exec(background, `delete from opamp_server.collector_groups where organization_id = $1 and id = $2`,
		orgA, group); err != nil {
		t.Fatal(err)
	}
	ctx, hangUp := context.WithCancel(background)
	defer hangUp()
	request := httptest.NewRequest(http.MethodDelete, c.target, nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+testSecret)
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		s.Handler().ServeHTTP(recorder, request)
		answered <- recorder
	}()
	awaitWaitingDeletes(t, d, c, 1)

	hangUp()
	if err := gone.Commit(background); err != nil {
		t.Fatal(err)
	}
	expectError(t, "the delete", awaitAnswer(t, answered), http.StatusNotFound, codeCollectorGroupNotFound, messageCollectorGroupNotFound)
	if strings.Contains(log.String(), "store failed") {
		t.Fatalf("a failure was logged: %s", log.String())
	}
}

// serve's forced stop (requests still running at ShutdownTimeout) still
// cancels a delete in flight, so the stop stays bounded: it rolls back,
// answers 500, and leaves every row for the platform's retry.
func TestAForcedStopStillCancelsADeleteInFlight(t *testing.T) {
	for _, c := range deletesUnderLock() {
		t.Run(c.name, func(t *testing.T) {
			s, _, log, d := postgresFleetServer(t)
			base, forceStop := context.WithCancel(context.Background())
			defer forceStop()
			lock, answered := startDeleteUnderLock(t, s, d, c, shutdown.WithForcedStop(base), true)

			forceStop()
			// The delete ends on its own, the lock still held.
			got := awaitAnswer(t, answered)
			if err := lock.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}
			expectError(t, "a cut-off delete", got, http.StatusInternalServerError, codeInternalError, messageInternalError)
			if n := remainingRows(t, d, c); n != 2 {
				t.Fatalf("%d rows remain after a cut-off delete, want 2 (nothing deleted)", n)
			}
			expectFailureCause(t, log, "serve's forced stop cut the delete off")
		})
	}
}

// expectFailureCause fails unless a store failure's log line names cause.
func expectFailureCause(t *testing.T, log *syncBuffer, cause string) {
	t.Helper()
	for _, line := range logLines(t, log.String()) {
		if line["cause"] == cause {
			return
		}
	}
	t.Fatalf("no failure logged the cause %q: %s", cause, log.String())
}

// The delete's own bound ends a delete that never finishes: with the
// timeout made short, a delete blocked on a lock nobody releases answers
// 500 on its own, the lock still held, and nothing is deleted. The bound
// is the one wall-clock limit (deletes.go); the hang guard is the test's.
func TestTheDeleteTimeoutEndsADeleteThatNeverFinishes(t *testing.T) {
	for _, c := range deletesUnderLock() {
		t.Run(c.name, func(t *testing.T) {
			s, _, log, d := postgresFleetServer(t)
			// Short, so the test is quick; whether the delete reached the
			// lock first or not, nothing but the bound can end it while the
			// lock is held.
			s.deleteTimeout = 200 * time.Millisecond
			lock, answered := startDeleteUnderLock(t, s, d, c, context.Background(), false)

			got := awaitAnswer(t, answered)
			if err := lock.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}
			expectError(t, "a timed-out delete", got, http.StatusInternalServerError, codeInternalError, messageInternalError)
			if n := remainingRows(t, d, c); n != 2 {
				t.Fatalf("%d rows remain after a timed-out delete, want 2 (nothing deleted)", n)
			}
			// The cause names the delete's own bound, not the request's
			// context, which no longer bounds it.
			expectFailureCause(t, log, "the delete's own bound passed")
			if !strings.Contains(log.String(), `"deleteTimeout":"200ms"`) {
				t.Fatalf("the failure doesn't name the bound: %s", log.String())
			}
		})
	}
}

// sendDelete sends an authenticated DELETE of target to s in the
// background and returns the channel that answers it.
func sendDelete(s *Server, target string) <-chan *httptest.ResponseRecorder {
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- call(s, http.MethodDelete, target, nil, "") }()
	return answered
}

// While this server runs an organization's or a fleet's delete (blocked
// here on a lock), a second delete of it is answered 503
// delete_in_progress at once, the first still waiting and the lock still
// held, rather than waiting behind the first with a pool connection of its
// own; so is a delete of a fleet of an organization being deleted. Once
// the first has committed, a retry answers 404.
func TestASecondDeleteOfOneRunningAnswers503WithoutWaiting(t *testing.T) {
	for _, c := range deletesUnderLock() {
		if c.waitsBehind {
			continue // TestASecondDeleteOfARunningGroupDeleteWaitsBehindIt
		}
		t.Run(c.name, func(t *testing.T) {
			s, _, _, d := postgresFleetServer(t)
			lock, first := startDeleteUnderLock(t, s, d, c, context.Background(), true)

			// The lock is held throughout: a second delete that waited for
			// it would never answer before the hang guard.
			expectDeleteInProgress(t, "the second delete", awaitAnswer(t, sendDelete(s, c.target)))
			if c.target == organizationTarget(orgA) {
				expectDeleteInProgress(t, "a delete of one of its fleets", awaitAnswer(t, sendDelete(s, fleetTarget(orgA, fleetA))))
			}
			if n := waitingDeletes(t, d, c); n != 1 {
				t.Fatalf("%d deletes wait on the lock, want only the first", n)
			}
			select {
			case got := <-first:
				t.Fatalf("the first delete answered %d while the lock was held", got.Code)
			default:
			}

			if err := lock.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}
			expectStatus(t, "the first delete", awaitAnswer(t, first), http.StatusNoContent)
			retry := call(s, http.MethodDelete, c.target, nil, "")
			expectError(t, "a retry once it is done", retry, http.StatusNotFound, c.notFound, c.notFoundMessage)
		})
	}
}

// A group's delete has no 503 delete_in_progress (docs/management-api.md,
// "Long deletes": a retry of a group's delete waits). While one runs
// (blocked here on a lock), a second delete of the group waits behind it
// on the group's row, holding its own connection; once the first has
// committed, the second finds the group gone and answers 404, as a later
// retry does.
func TestASecondDeleteOfARunningGroupDeleteWaitsBehindIt(t *testing.T) {
	for _, c := range deletesUnderLock() {
		if !c.waitsBehind {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			s, _, _, d := postgresFleetServer(t)
			lock, first := startDeleteUnderLock(t, s, d, c, context.Background(), true)

			second := sendDelete(s, c.target)
			awaitWaitingDeletes(t, d, c, 2)
			select {
			case got := <-first:
				t.Fatalf("the first delete answered %d while the lock was held", got.Code)
			case got := <-second:
				t.Fatalf("the second delete answered %d while the lock was held", got.Code)
			default:
			}

			if err := lock.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}
			expectStatus(t, "the first delete", awaitAnswer(t, first), http.StatusNoContent)
			expectError(t, "the second delete", awaitAnswer(t, second), http.StatusNotFound, c.notFound, c.notFoundMessage)
			if n := remainingRows(t, d, c); n != 0 {
				t.Fatalf("%d rows remain after the deletes, want 0", n)
			}
		})
	}
}
