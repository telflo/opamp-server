package collectors_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/telflo/opamp-server/store/collectorgroups"
	"github.com/telflo/opamp-server/store/collectors"
	"github.com/telflo/opamp-server/store/fleets"
	. "github.com/telflo/opamp-server/store/storetest"
	"github.com/telflo/opamp-server/store/variables"
)

// The lock order on Postgres, the error history's exact bound under
// concurrent inserts, a reconcile's served time, a heartbeat that never
// moves back, and List after a cursor.

// An organization's delete racing RecordError never deadlocks. On Postgres
// RecordError holds the organization's row and the fleet's `for key share`
// while it waits on its collector (a third session holds it), and the
// organization's delete waits on the organization's row; released,
// RecordError writes its row and the delete then takes it. Locking the
// fleet first and key-sharing the organization only at its insert's foreign
// key check, RecordError would deadlock (40P01) with the delete holding the
// organization and waiting on the fleet or the collector.
func TestAnOrganizationsDeleteRacingRecordErrorDoesNotDeadlock(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		got := enroll(t, s, c, "edge-01", Now)
		release := blockOn(t, s, 2, `select 1 from opamp_server.collector_instances
			where organization_id = $1 and id = $2 for update`, OrgA, got.ID)
		var recordErr, deleteErr error
		var deleted bool
		recordDone, deleteDone := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(recordDone)
			recordErr = s.Collectors.RecordError(ctx, OrgA, c.Fleet.ID, got.ID, collectors.ErrorReport{Message: "boom", At: Now})
		}()
		s.WaitForLockWaiters(t, 1)
		go func() {
			defer close(deleteDone)
			deleted, deleteErr = s.Organizations.Delete(ctx, OrgA)
		}()
		release()
		<-recordDone
		<-deleteDone
		if recordErr != nil || deleteErr != nil || !deleted {
			t.Fatalf("RecordError = %v; the organization's Delete = %v, %v; want both to finish", recordErr, deleted, deleteErr)
		}
		if dump := s.Dump(t, OrgA); dump != "" {
			t.Fatalf("the organization's rows survived its delete:\n%s", dump)
		}
	})
}

// BulkPin racing a group's Delete never deadlocks. Two collectors of the
// group, the higher id inserted first (so a scan of the group meets it
// first); a third session holds the higher. The group's Delete locks the
// group's collectors in id order, the lower first, then waits on the
// higher; BulkPin, locking in id order too, waits on the lower. Released,
// the Delete deletes both with the group and BulkPin then finds neither.
// Locking in scan order, the Delete would meet the higher first while
// BulkPin took the lower: each would wait on the other (40P01).
func TestBulkPinRacingAGroupsDeleteDoesNotDeadlock(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		g, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID, Name: "gateways", At: Now})
		if err != nil {
			t.Fatal(err)
		}
		high := uuid.MustParse("ffffffff-0000-4000-8000-000000000001")
		low := uuid.MustParse("00000000-0000-4000-8000-000000000001")
		s.InsertCollector(t, Collector{ID: high, OrganizationID: OrgA, FleetID: c.Fleet.ID, GroupID: g.ID, Name: "c-high", LastSeenAt: Now})
		s.InsertCollector(t, Collector{ID: low, OrganizationID: OrgA, FleetID: c.Fleet.ID, GroupID: g.ID, Name: "c-low", LastSeenAt: Now})
		release := blockOn(t, s, 2, `select 1 from opamp_server.collector_instances
			where organization_id = $1 and id = $2 for no key update`, OrgA, high)
		var deleted bool
		var pinned collectors.BulkPinResult
		var deleteErr, pinErr error
		deleteDone, pinDone := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(deleteDone)
			deleted, deleteErr = s.Groups.Delete(ctx, OrgA, c.Fleet.ID, g.ID)
		}()
		s.WaitForLockWaiters(t, 1)
		go func() {
			defer close(pinDone)
			pinned, pinErr = s.Collectors.BulkPin(ctx, OrgA, c.Fleet.ID, []uuid.UUID{low, high},
				&collectors.PinChange{Ref: &collectors.Ref{Source: collectors.SourcePublication, ID: PubA}}, nil, Now.Add(time.Minute))
		}()
		release()
		<-deleteDone
		<-pinDone
		if deleteErr != nil || pinErr != nil || !deleted || len(pinned.Updated) != 0 || len(pinned.NotFound) != 2 {
			t.Fatalf("the group's Delete = %v, %v; BulkPin = %+v, %v; want both to finish, BulkPin finding neither",
				deleted, deleteErr, pinned, pinErr)
		}
		for _, id := range []uuid.UUID{low, high} {
			if got, err := s.Collectors.Get(ctx, OrgA, c.Fleet.ID, id); !errors.Is(err, collectors.ErrNotFound) {
				t.Fatalf("collector %s = %+v, %v; want deleted with its group", id, got.Collector, err)
			}
		}
	})
}

// Move racing a group's Delete (which deletes the group's collectors) never
// deadlocks and never leaves a collector in the deleted group, both ways. A
// third session holds x, the group's collector, so the Delete (after the
// fleet and the group) waits on it; then the Move starts. Out of the group:
// Move of x waits on x too; released, the Delete, queued first, deletes x,
// and Move finds no collector (or, had Move taken x first, x is kept in the
// other group). Into the group: Move of y, the other group's, waits on the
// group the Delete holds; released, the group is gone: ErrGroupNotFound, y
// kept where it was.
func TestMoveRacingAGroupsDeleteDoesNotDeadlock(t *testing.T) {
	for _, into := range []bool{false, true} {
		Each(t, testDatabase, func(t *testing.T, s *Stores) {
			c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
			g, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID, Name: "gateways", At: Now})
			if err != nil {
				t.Fatal(err)
			}
			h, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID, Name: "others", At: Now})
			if err != nil {
				t.Fatal(err)
			}
			x := uuid.MustParse("00000000-0000-4000-8000-0000000000aa")
			y := uuid.MustParse("00000000-0000-4000-8000-0000000000bb")
			s.InsertCollector(t, Collector{ID: x, OrganizationID: OrgA, FleetID: c.Fleet.ID, GroupID: g.ID, Name: "c-x", LastSeenAt: Now})
			s.InsertCollector(t, Collector{ID: y, OrganizationID: OrgA, FleetID: c.Fleet.ID, GroupID: h.ID, Name: "c-y", LastSeenAt: Now})
			moving, target := x, h.ID
			if into {
				moving, target = y, g.ID
			}
			release := blockOn(t, s, 2, `select 1 from opamp_server.collector_instances
				where organization_id = $1 and id = $2 for no key update`, OrgA, x)
			var deleted bool
			var deleteErr, moveErr error
			deleteDone, moveDone := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(deleteDone)
				deleted, deleteErr = s.Groups.Delete(ctx, OrgA, c.Fleet.ID, g.ID)
			}()
			s.WaitForLockWaiters(t, 1)
			go func() {
				defer close(moveDone)
				_, moveErr = s.Collectors.Move(ctx, OrgA, c.Fleet.ID, moving, target, Now.Add(time.Minute))
			}()
			release()
			<-deleteDone
			<-moveDone
			if deleteErr != nil || !deleted {
				t.Fatalf("into %v: the group's Delete = %v, %v; want it deleted", into, deleted, deleteErr)
			}
			got, getErr := s.Collectors.Get(ctx, OrgA, c.Fleet.ID, moving)
			switch {
			case into && errors.Is(moveErr, collectors.ErrGroupNotFound) && getErr == nil && got.GroupID == h.ID:
				if _, err := s.Collectors.Get(ctx, OrgA, c.Fleet.ID, x); !errors.Is(err, collectors.ErrNotFound) {
					t.Fatalf("into: x = %v; want deleted with its group", err)
				}
			case !into && errors.Is(moveErr, collectors.ErrNotFound) && errors.Is(getErr, collectors.ErrNotFound):
			case !into && moveErr == nil && getErr == nil && got.GroupID == h.ID:
			default:
				t.Fatalf("into %v: Move = %v; the collector after = %+v, %v", into, moveErr, got.Collector, getErr)
			}
		})
	}
}

// Eight RecordErrors of one collector at once leave exactly its newest
// MaxErrors rows, round after round: each locks the collector's row before
// its insert and trim, so each trim sees the others' rows. Without the lock
// a trim misses the rows inserted beside it, and more than MaxErrors stay.
func TestConcurrentRecordErrorsKeepExactlyTheNewestHundred(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		got := enroll(t, s, c, "edge-01", Now)
		for i := range collectors.MaxErrors {
			if err := s.Collectors.RecordError(ctx, OrgA, c.Fleet.ID, got.ID, collectors.ErrorReport{
				Message: "m", At: Now.Add(time.Duration(i) * time.Millisecond)}); err != nil {
				t.Fatal(err)
			}
		}
		const rounds, writers = 20, 8
		for round := range rounds {
			start := make(chan struct{})
			var wg sync.WaitGroup
			for w := range writers {
				wg.Go(func() {
					<-start
					if err := s.Collectors.RecordError(ctx, OrgA, c.Fleet.ID, got.ID, collectors.ErrorReport{Message: "m",
						At: Now.Add(time.Hour + time.Duration(round*writers+w)*time.Millisecond)}); err != nil {
						t.Error(err)
					}
				})
			}
			close(start)
			wg.Wait()
			page, err := s.Collectors.ListErrors(ctx, OrgA, c.Fleet.ID, got.ID, collectors.MaxListLimit, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Errors) != collectors.MaxErrors || page.Next != nil {
				t.Fatalf("round %d: %d errors and more (%v) after %d at once; want exactly %d",
					round, len(page.Errors), page.Next != nil, writers, collectors.MaxErrors)
			}
			newest := Millisecond(Now.Add(time.Hour + time.Duration(round*writers+writers-1)*time.Millisecond))
			if !page.Errors[0].ReportedAt.Equal(newest) {
				t.Fatalf("round %d: the newest kept is %v, want %v", round, page.Errors[0].ReportedAt, newest)
			}
		}
	})
}

// A reconcile of a collector never served sets ServedAt to the time it is
// given (else its sync would read pending for ever); a reconcile of one
// served before keeps its ServedAt.
func TestAReconcileSetsServedAtOnlyWhenItWasNeverSet(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		got := enroll(t, s, c, "edge-01", Now)
		reconcile := func(ref uuid.UUID, sha string, at time.Time) {
			t.Helper()
			written, err := s.Collectors.Report(ctx, collectors.Report{OrganizationID: OrgA, FleetID: c.Fleet.ID, CollectorID: got.ID,
				Reconciled: &collectors.Reconciled{Ref: collectors.Ref{Source: collectors.SourcePublication, ID: ref}, SHA256: sha, At: at}})
			if err != nil || !written {
				t.Fatalf("the reconcile = %v, %v", written, err)
			}
		}
		first := Now.Add(time.Minute)
		reconcile(PubA, pubSHA256, first)
		row := get(t, s, got)
		if row.ServedAt == nil || !row.ServedAt.Equal(Millisecond(first)) || *row.ServedSHA256 != pubSHA256 {
			t.Fatalf("after reconciling a collector never served: ServedAt %v, %+v", row.ServedAt, row.Collector)
		}
		other := publish(t, s, OrgA, "exporters: {}\n")
		reconcile(other, sumOf("exporters: {}\n"), Now.Add(time.Hour))
		row = get(t, s, got)
		if !row.ServedAt.Equal(Millisecond(first)) || row.Served.ID != other {
			t.Fatalf("after a second reconcile: ServedAt %v, served %+v; want ServedAt kept", row.ServedAt, row.Served)
		}
	})
}

// A poll on another token from a replica whose clock is behind writes the
// token but never moves LastSeenAt back.
func TestATokenChangeNeverMovesLastSeenBack(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		got := enroll(t, s, c, "edge-01", Now)
		later := Now.Add(time.Hour)
		if r, err := s.Collectors.Poll(ctx, collectors.PollRequest{OrganizationID: OrgA, FleetID: c.Fleet.ID,
			InstanceUID: got.InstanceUID, TokenID: c.RotateTokenID, At: later}); err != nil || !r.Touched {
			t.Fatalf("the heartbeat an hour on = %+v, %v", r, err)
		}
		r, err := s.Collectors.Poll(ctx, collectors.PollRequest{OrganizationID: OrgA, FleetID: c.Fleet.ID,
			InstanceUID: got.InstanceUID, TokenID: c.DefaultTokenID, At: Now.Add(30 * time.Minute)})
		if err != nil || !r.Touched {
			t.Fatalf("a poll on another token from a clock behind = %+v, %v; want touched", r, err)
		}
		row := get(t, s, got)
		if !row.LastSeenAt.Equal(Millisecond(later)) || *row.EnrollmentTokenID != c.DefaultTokenID {
			t.Fatalf("after it: last seen %v on %v; want %v on the default token", row.LastSeenAt, *row.EnrollmentTokenID, Millisecond(later))
		}
	})
}

// List after a cursor reads no fleet check (an empty page for a fleet the
// organization doesn't hold); a first page checks the fleet
// (ErrFleetNotFound).
func TestListAfterACursorForAFleetNotHeldIsEmptyOnBothStores(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		enroll(t, s, c, "edge-01", Now)
		enroll(t, s, c, "edge-02", Now)
		o := collectors.ListOptions{Sort: collectors.SortName, Order: collectors.Ascending, Limit: 1}
		first, err := s.Collectors.List(ctx, OrgA, c.Fleet.ID, o)
		if err != nil || first.Next == nil {
			t.Fatalf("the first page = %+v, %v", first, err)
		}
		o.After = first.Next
		for label, ids := range map[string][2]uuid.UUID{
			"an unknown fleet":            {OrgA, uuid.New()},
			"the fleet under another org": {OrgB, c.Fleet.ID},
		} {
			page, err := s.Collectors.List(ctx, ids[0], ids[1], o)
			if err != nil || page.Collectors == nil || len(page.Collectors) != 0 || page.Next != nil {
				t.Errorf("%s after a cursor: %+v, %v; want an empty page", label, page, err)
			}
		}
		o.After = nil
		if _, err := s.Collectors.List(ctx, OrgA, uuid.New(), o); err != collectors.ErrFleetNotFound {
			t.Errorf("an unknown fleet's first page = %v, want ErrFleetNotFound", err)
		}
	})
}

// The binding writes and the variable writes take the organization's row
// `for no key update` first, so they serialize. A variable's delete racing
// an assignment (a group's) or a pin (a collector's) of a published version
// that references it never leaves a used published version referencing an
// undefined name: one of the two is refused, the delete as in use or the
// assignment as undefined, round after round. On Postgres a third session
// holds the organization's row until both wait on it.
func TestAVariableDeleteRacingAnAssignmentNeverLeavesAnUndefinedName(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		version := publish(t, s, OrgA, "a: ${org:REGION}\n")
		for round := range 6 {
			viaPin := round%2 == 1
			region := defineVariable(t, s, OrgA, "REGION", "eu")
			g, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID,
				Name: fmt.Sprintf("round-%d", round), At: Now})
			if err != nil {
				t.Fatal(err)
			}
			collector := enroll(t, s, c, fmt.Sprintf("edge-%d", round), Now)
			release := blockOn(t, s, 2, `select 1 from opamp_server.organizations where id = $1 for update`, OrgA)
			errs := both(release, func() error {
				if viaPin {
					_, err := s.Collectors.Pin(ctx, OrgA, c.Fleet.ID, collector.ID,
						&collectors.Ref{Source: collectors.SourcePublication, ID: version}, Now)
					return err
				}
				_, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, g.ID,
					collectorgroups.Changes{AssignedPublicationID: collectorgroups.SetTo(version)}, Now)
				return err
			}, func() error {
				return s.Variables.DeleteVariable(ctx, OrgA, region.ID)
			})
			assigned, deleted := errs[0], errs[1]
			undefinedRefusal := errors.Is(assigned, collectors.ErrVariablesUndefined) || errors.Is(assigned, collectorgroups.ErrVariablesUndefined)
			switch {
			case assigned == nil && errors.Is(deleted, variables.ErrInUse):
			case deleted == nil && undefinedRefusal:
			default:
				t.Fatalf("round %d (pin %v): the assignment = %v, the delete = %v; want exactly one refused", round, viaPin, assigned, deleted)
			}
			// Clear the round: unassign, unpin, and the variable gone.
			if _, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, g.ID,
				collectorgroups.Changes{AssignedPublicationID: collectorgroups.Clear[uuid.UUID]()}, Now); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Collectors.Pin(ctx, OrgA, c.Fleet.ID, collector.ID, nil, Now); err != nil {
				t.Fatal(err)
			}
			if deleted != nil {
				if err := s.Variables.DeleteVariable(ctx, OrgA, region.ID); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
}

// waitingQueries is what the sessions waiting on a lock in the test
// database run.
func waitingQueries(t *testing.T, s *Stores) []string {
	t.Helper()
	rows, err := s.Databases.Admin.Query(ctx, `select query from pg_stat_activity
		where datname = $1 and wait_event_type = 'Lock'`, s.Databases.Name)
	if err != nil {
		t.Fatal(err)
	}
	queries, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return queries
}

// heldAt is each lock the binding race tests hold in turn from a third
// session: the organization's row, the fleet's and the group's, each `for
// update`, which every write of theirs waits on, or behind one that does.
func heldAt(c fleets.Creation, group uuid.UUID) map[string][]any {
	return map[string][]any{
		"the organization": {`select 1 from opamp_server.organizations where id = $1 for update`, OrgA},
		"the fleet":        {`select 1 from opamp_server.fleets where organization_id = $1 and id = $2 for update`, OrgA, c.Fleet.ID},
		"the group": {`select 1 from opamp_server.collector_groups where organization_id = $1 and fleet_id = $2 and id = $3 for update`,
			OrgA, c.Fleet.ID, group},
	}
}

// A group's variable-group change racing an enrollment into that group
// never deadlocks: both take the organization's row `for no key update`
// first, then the fleet and the group, so they queue; held at each of those
// locks in turn, both finish.
func TestABindingChangeRacingAnEnrollmentDoesNotDeadlock(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		g, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID, Name: "gateways", At: Now})
		if err != nil {
			t.Fatal(err)
		}
		i := 0
		for at, hold := range heldAt(c, g.ID) {
			vg := newVariableGroup(t, s, OrgA, "vg-"+at)
			release := blockOn(t, s, 2, hold[0].(string), hold[1:]...)
			i++
			e := enrollment(c, fmt.Sprintf("edge-%d", i), Now)
			e.GroupID = &g.ID
			errs := both(release, func() error {
				_, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, g.ID,
					collectorgroups.Changes{VariableGroupID: collectorgroups.SetTo(vg)}, Now)
				return err
			}, func() error {
				_, err := s.Collectors.Enroll(ctx, e)
				return err
			})
			if errs[0] != nil || errs[1] != nil {
				t.Fatalf("held at %s: the binding = %v (deadlock %v), the enrollment = %v (deadlock %v); want both to finish",
					at, errs[0], isDeadlock(errs[0]), errs[1], isDeadlock(errs[1]))
			}
		}
	})
}

// A bulk pin of a variable group racing that variable group's delete never
// deadlocks and never deletes a pinned variable group. Held at a collector
// the bulk pin locks (it holds the organization's row already, so the
// delete queues behind it): the pin finishes, then the delete is refused as
// in use. Held at the organization's row: whichever goes first wins, the
// other refused (in use, or the variable group gone).
func TestABulkPinRacingAVariableGroupsDeleteDoesNotDeadlock(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		a, b := enroll(t, s, c, "a", Now), enroll(t, s, c, "b", Now)
		for _, atCollector := range []bool{true, false} {
			vg := newVariableGroup(t, s, OrgA, fmt.Sprintf("vg-%v", atCollector))
			hold := []any{`select 1 from opamp_server.organizations where id = $1 for update`, OrgA}
			if atCollector {
				hold = []any{`select 1 from opamp_server.collector_instances
					where organization_id = $1 and id = $2 for no key update`, OrgA, b.ID}
			}
			release := blockOn(t, s, 2, hold[0].(string), hold[1:]...)
			var pinned collectors.BulkPinResult
			pinDone := make(chan error, 1)
			go func() {
				var err error
				pinned, err = s.Collectors.BulkPin(ctx, OrgA, c.Fleet.ID, []uuid.UUID{a.ID, b.ID}, nil,
					&collectors.VariableGroupChange{ID: &vg}, Now)
				pinDone <- err
			}()
			s.WaitForLockWaiters(t, 1)
			deleteDone := make(chan error, 1)
			go func() { deleteDone <- s.Variables.DeleteGroup(ctx, OrgA, vg) }()
			release()
			pinErr, deleteErr := <-pinDone, <-deleteDone
			switch {
			case pinErr == nil && errors.Is(deleteErr, variables.ErrGroupInUse) && pinned.Changed == 2:
			case !atCollector && deleteErr == nil && errors.Is(pinErr, collectors.ErrVariableGroupNotFound):
			default:
				t.Fatalf("held at the collector %v: BulkPin = %+v, %v (deadlock %v); the delete = %v (deadlock %v)",
					atCollector, pinned, pinErr, isDeadlock(pinErr), deleteErr, isDeadlock(deleteErr))
			}
			if _, err := s.Collectors.BulkPin(ctx, OrgA, c.Fleet.ID, []uuid.UUID{a.ID, b.ID}, nil,
				&collectors.VariableGroupChange{}, Now); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// No write upgrades its organization lock. Each write's first lock on the
// organization's row is the mode the lock order on Postgres names, chosen
// from the request (on Postgres a third session holds the row `for update`,
// and the write's waiting statement shows its mode): `for no key update`
// for a binding, a pin, a move, an enrollment and a variable write, `for
// key share` for the rest. Then three sessions, a group's Update that names
// a variable group, an enrollment and a variable's value change, held at
// each lock in turn (the organization's row, the fleet's, the group's):
// none answers 40P01, and all finish.
func TestNoWriteUpgradesItsOrganizationLock(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		g, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID, Name: "gateways", At: Now})
		if err != nil {
			t.Fatal(err)
		}
		vg := newVariableGroup(t, s, OrgA, "eu")
		region := defineVariable(t, s, OrgA, "REGION", "eu")
		got := enroll(t, s, c, "edge-01", Now)
		pin := &collectors.Ref{Source: collectors.SourcePublication, ID: PubA}
		const noKeyUpdate, keyShare = "for no key update", "for key share"
		n := 0
		for _, w := range []struct {
			label string
			mode  string
			write func() error
		}{
			{"a group's create naming a published version", noKeyUpdate, func() error {
				_, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID, Name: "p",
					AssignedPublicationID: &PubA, At: Now})
				return err
			}},
			{"a group's create naming a variable group", noKeyUpdate, func() error {
				_, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID, Name: "v",
					VariableGroupID: &vg, At: Now})
				return err
			}},
			{"a group's create naming neither", keyShare, func() error {
				_, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID, Name: "n", At: Now})
				return err
			}},
			{"a group's assignment", noKeyUpdate, func() error {
				_, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, g.ID, collectorgroups.Changes{AssignedPublicationID: collectorgroups.SetTo(PubA)}, Now)
				return err
			}},
			{"a group's variable group set", noKeyUpdate, func() error {
				_, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, g.ID, collectorgroups.Changes{VariableGroupID: collectorgroups.SetTo(vg)}, Now)
				return err
			}},
			{"a group's variable group set to what it is", noKeyUpdate, func() error {
				_, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, g.ID, collectorgroups.Changes{VariableGroupID: collectorgroups.SetTo(vg)}, Now)
				return err
			}},
			{"a group's variable group cleared", noKeyUpdate, func() error {
				_, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, g.ID, collectorgroups.Changes{VariableGroupID: collectorgroups.Clear[uuid.UUID]()}, Now)
				return err
			}},
			{"a group's rename", keyShare, func() error {
				_, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, g.ID, collectorgroups.Changes{Name: Ptr("edges")}, Now)
				return err
			}},
			{"a group's annotations", keyShare, func() error {
				_, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, g.ID, collectorgroups.Changes{Annotations: collectorgroups.Clear[json.RawMessage]()}, Now)
				return err
			}},
			{"a group's assignment cleared", keyShare, func() error {
				_, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, g.ID, collectorgroups.Changes{AssignedPublicationID: collectorgroups.Clear[uuid.UUID]()}, Now)
				return err
			}},
			{"a collector's rename", keyShare, func() error {
				_, err := s.Collectors.Update(ctx, OrgA, c.Fleet.ID, got.ID, collectors.Changes{Name: Ptr("edge-renamed")}, Now)
				return err
			}},
			{"a collector's pin by Update", noKeyUpdate, func() error {
				_, err := s.Collectors.Update(ctx, OrgA, c.Fleet.ID, got.ID, collectors.Changes{Pin: &collectors.PinChange{Ref: pin}}, Now)
				return err
			}},
			{"a collector's pinned variable group", noKeyUpdate, func() error {
				_, err := s.Collectors.Update(ctx, OrgA, c.Fleet.ID, got.ID, collectors.Changes{PinnedVariableGroup: &collectors.VariableGroupChange{ID: &vg}}, Now)
				return err
			}},
			{"a collector's move by Update, to its own group", noKeyUpdate, func() error {
				_, err := s.Collectors.Update(ctx, OrgA, c.Fleet.ID, got.ID, collectors.Changes{GroupID: &got.GroupID}, Now)
				return err
			}},
			{"a collector's Pin", noKeyUpdate, func() error {
				_, err := s.Collectors.Pin(ctx, OrgA, c.Fleet.ID, got.ID, nil, Now)
				return err
			}},
			{"a collector's Move", noKeyUpdate, func() error {
				_, err := s.Collectors.Move(ctx, OrgA, c.Fleet.ID, got.ID, g.ID, Now)
				return err
			}},
			{"a bulk pin of a version", noKeyUpdate, func() error {
				_, err := s.Collectors.BulkPin(ctx, OrgA, c.Fleet.ID, []uuid.UUID{got.ID}, &collectors.PinChange{Ref: pin}, nil, Now)
				return err
			}},
			{"a bulk pin of a variable group", noKeyUpdate, func() error {
				_, err := s.Collectors.BulkPin(ctx, OrgA, c.Fleet.ID, []uuid.UUID{got.ID}, nil, &collectors.VariableGroupChange{}, Now)
				return err
			}},
			{"an enrollment", noKeyUpdate, func() error {
				n++
				_, err := s.Collectors.Enroll(ctx, enrollment(c, fmt.Sprintf("enrolled-%d", n), Now))
				return err
			}},
			{"a variable's value change", noKeyUpdate, func() error {
				value := fmt.Sprintf("v%d", n)
				n++
				_, err := s.Variables.UpdateVariable(ctx, OrgA, region.ID, variables.VariableChanges{Value: &value}, nil, Now)
				return err
			}},
			{"an error row", keyShare, func() error {
				return s.Collectors.RecordError(ctx, OrgA, c.Fleet.ID, got.ID, collectors.ErrorReport{Message: "m", At: Now})
			}},
		} {
			release := blockOn(t, s, 1, `select 1 from opamp_server.organizations where id = $1 for update`, OrgA)
			done := make(chan error, 1)
			go func() { done <- w.write() }()
			s.WaitForLockWaiters(t, 1)
			waiting := waitingQueries(t, s)
			release()
			if err := <-done; err != nil {
				t.Fatalf("%s: %v", w.label, err)
			}
			if len(waiting) != 1 || !strings.Contains(waiting[0], "opamp_server.organizations") || !strings.Contains(waiting[0], w.mode) {
				t.Fatalf("%s first waited on %q; want the organization's row %s", w.label, waiting, w.mode)
			}
			other := keyShare
			if w.mode == keyShare {
				other = noKeyUpdate
			}
			if strings.Contains(waiting[0], other) {
				t.Fatalf("%s first waited on %q, which takes the row %s", w.label, waiting, other)
			}
		}

		// The three sessions, held at each lock in turn.
		i := 0
		for at, hold := range heldAt(c, g.ID) {
			i++
			next := newVariableGroup(t, s, OrgA, fmt.Sprintf("held-%d", i))
			value := fmt.Sprintf("held-%d", i)
			e := enrollment(c, fmt.Sprintf("held-%d", i), Now)
			e.GroupID = &g.ID
			// Held at the fleet or the group, the variable write (which locks
			// neither) finishes if it takes the organization's row first: the
			// release waits until each of the three waits on a lock or is done.
			release := blockOn(t, s, 0, hold[0].(string), hold[1:]...)
			var finished atomic.Int32
			start := make(chan struct{})
			var wg sync.WaitGroup
			var errs [3]error
			for k, write := range []func() error{
				func() error {
					_, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, g.ID, collectorgroups.Changes{VariableGroupID: collectorgroups.SetTo(next)}, Now)
					return err
				},
				func() error {
					_, err := s.Collectors.Enroll(ctx, e)
					return err
				},
				func() error {
					_, err := s.Variables.UpdateVariable(ctx, OrgA, region.ID, variables.VariableChanges{Value: &value}, nil, Now)
					return err
				},
			} {
				wg.Go(func() {
					<-start
					errs[k] = write()
					finished.Add(1)
				})
			}
			close(start)
			guard := time.Now().Add(time.Minute) // a hang guard only
			for len(waitingQueries(t, s))+int(finished.Load()) < 3 {
				if time.Now().After(guard) {
					t.Fatalf("held at %s: the three sessions never all waited or finished", at)
				}
			}
			release()
			wg.Wait()
			for k, err := range errs {
				if err != nil {
					t.Fatalf("held at %s: session %d = %v (deadlock %v); want all three to finish", at, k, err, isDeadlock(err))
				}
			}
		}
	})
}
