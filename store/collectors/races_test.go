package collectors_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/telflo/opamp-server/store/collectorgroups"
	"github.com/telflo/opamp-server/store/collectors"
	"github.com/telflo/opamp-server/store/fleets"
	. "github.com/telflo/opamp-server/store/storetest"
)

// blockOn has a third session (the owner's) take a lock with statement,
// and returns its release: once n sessions wait on a lock, it commits.
func blockOn(t *testing.T, s *Stores, n int, statement string, args ...any) (release func()) {
	t.Helper()
	blocker, err := s.Databases.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Rollback(ctx) })
	if _, err := blocker.Exec(ctx, statement, args...); err != nil {
		t.Fatal(err)
	}
	return func() {
		s.WaitForLockWaiters(t, n)
		if err := blocker.Commit(ctx); err != nil {
			t.Error(err)
		}
	}
}

// both runs two calls at once, after release (which, on Postgres, waits
// until both wait on its lock), and returns their errors.
func both(release func(), first, second func() error) [2]error {
	var errs [2]error
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, call := range []func() error{first, second} {
		wg.Go(func() {
			<-start
			errs[i] = call()
		})
	}
	close(start)
	release()
	wg.Wait()
	return errs
}

// countRows counts the organization's collectors (with the owner's role on
// Postgres).
func countRows(t *testing.T, s *Stores, c fleets.Creation) int {
	t.Helper()
	page, err := s.Collectors.List(ctx, c.Fleet.OrganizationID, c.Fleet.ID, collectors.ListOptions{
		Sort: collectors.SortCreated, Order: collectors.Descending, Limit: collectors.MaxListLimit})
	if err != nil {
		t.Fatal(err)
	}
	return len(page.Collectors)
}

// Two sessions enrolling one instance uid at once make one row; the loser
// reads the winner's and answers it, never an error. The first round holds
// both in the race behind a third session holding the organization's row;
// the others start both at once.
func TestTwoSessionsEnrollingOneInstanceUIDMakeOneRow(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		const rounds = 10
		for round := range rounds {
			e := enrollment(c, fmt.Sprintf("edge-%02d", round), Now)
			release := func() {}
			if round == 0 {
				release = blockOn(t, s, 2, `select 1 from telflo_opamp.organizations where id = $1 for update`, OrgA)
			}
			var results [2]collectors.Enrolled
			errs := both(release,
				func() (err error) { results[0], err = s.Collectors.Enroll(ctx, e); return err },
				func() (err error) { results[1], err = s.Collectors.Enroll(ctx, e); return err })
			if errs[0] != nil || errs[1] != nil {
				t.Fatalf("round %d: the enrollments failed: %v", round, errs)
			}
			if results[0].Created == results[1].Created || results[0].Collector.ID != results[1].Collector.ID ||
				results[0].Collector.Name != e.DesiredName || results[1].Collector.Name != e.DesiredName {
				t.Fatalf("round %d: %+v and %+v; want one created, both the same row", round, results[0], results[1])
			}
			if n := countRows(t, s, c); n != round+1 {
				t.Fatalf("round %d: %d rows", round, n)
			}
		}
	})
}

// Two sessions enrolling two collectors into an organization's last slot:
// one enrolls, the other is ErrCapReached; the count never passes the cap.
func TestTwoSessionsEnrollingIntoTheLastSlotOneIsRefused(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		enroll(t, s, c, "edge-00", Now)
		setCap(t, s, OrgA, 2)
		release := blockOn(t, s, 2, `select 1 from telflo_opamp.organizations where id = $1 for update`, OrgA)
		var results [2]collectors.Enrolled
		errs := both(release,
			func() (err error) {
				results[0], err = s.Collectors.Enroll(ctx, enrollment(c, "edge-01", Now))
				return err
			},
			func() (err error) {
				results[1], err = s.Collectors.Enroll(ctx, enrollment(c, "edge-02", Now))
				return err
			})
		refused := 0
		for i, err := range errs {
			switch {
			case errors.Is(err, collectors.ErrCapReached):
				refused++
			case err != nil || !results[i].Created:
				t.Fatalf("enrollment %d = %+v, %v", i, results[i], err)
			}
		}
		if refused != 1 {
			t.Fatalf("the two enrollments = %v; want one refused", errs)
		}
		if n := countRows(t, s, c); n != 2 {
			t.Fatalf("%d collectors under a cap of 2", n)
		}
	})
}

// An enrollment naming a group that its delete removes meanwhile is
// ErrForeignGroup (the agent's 403), not a failure. On Postgres the group's
// delete holds the group (waiting on a collector a third session holds)
// while the enrollment waits for it. The group's member goes with the
// group: no collector is left.
func TestAnEnrollmentRacingItsGroupsDeleteIsAForeignGroup(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		g, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID, Name: "gateways", At: Now})
		if err != nil {
			t.Fatal(err)
		}
		e := enrollment(c, "edge-01", Now)
		e.GroupID = &g.ID
		member := enroll(t, s, c, "member", Now)
		if _, err := s.Collectors.Move(ctx, OrgA, c.Fleet.ID, member.ID, g.ID, Now); err != nil {
			t.Fatal(err)
		}
		release := blockOn(t, s, 2, `select 1 from telflo_opamp.collector_instances
			where organization_id = $1 and id = $2 for update`, OrgA, member.ID)

		var deleted bool
		var deleteErr, enrollErr error
		deleteDone, enrollDone := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(deleteDone)
			deleted, deleteErr = s.Groups.Delete(ctx, OrgA, c.Fleet.ID, g.ID)
		}()
		s.WaitForLockWaiters(t, 1)
		go func() {
			defer close(enrollDone)
			_, enrollErr = s.Collectors.Enroll(ctx, e)
		}()
		release()
		<-deleteDone
		<-enrollDone
		if deleteErr != nil || !deleted {
			t.Fatalf("the group's Delete = %v, %v", deleted, deleteErr)
		}
		if !errors.Is(enrollErr, collectors.ErrForeignGroup) {
			t.Fatalf("the enrollment = %v, want ErrForeignGroup", enrollErr)
		}
		if n := countRows(t, s, c); n != 0 {
			t.Fatalf("%d collectors, want none (the member deleted with its group, the enrollment refused)", n)
		}
	})
}

// An enrollment racing its fleet's delete never deadlocks. On Postgres the
// enrollment, holding the fleet `for key share`, waits on the Default
// group (a third session holds it) while the fleet's delete waits on the
// fleet; fleet first, the enrollment finishes and the delete then takes
// its collector. Without the fleet first (the lock order on Postgres), the
// insert would key-share the fleet only at its foreign key check, after
// the delete held the fleet and waited on the group: a deadlock (40P01).
func TestAnEnrollmentRacingItsFleetsDeleteDoesNotDeadlock(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		release := blockOn(t, s, 2, `select 1 from telflo_opamp.collector_groups
			where organization_id = $1 and id = $2 for update`, OrgA, c.DefaultGroupID)
		var r collectors.Enrolled
		var fleetDeleted bool
		var enrollErr, fleetErr error
		enrollDone, fleetDone := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(enrollDone)
			r, enrollErr = s.Collectors.Enroll(ctx, enrollment(c, "edge-01", Now))
		}()
		s.WaitForLockWaiters(t, 1)
		go func() {
			defer close(fleetDone)
			fleetDeleted, fleetErr = s.Fleets.Delete(ctx, OrgA, c.Fleet.ID)
		}()
		release()
		<-enrollDone
		<-fleetDone
		if enrollErr != nil || fleetErr != nil {
			t.Fatalf("the enrollment = %v; the fleet's Delete = %v; want both to finish", enrollErr, fleetErr)
		}
		if !r.Created || !fleetDeleted {
			t.Fatalf("the enrollment = %+v, the fleet's Delete = %v; want both (the enrollment first)", r, fleetDeleted)
		}
		for table, n := range s.FleetRows(t, c.Fleet.ID) {
			if n != 0 {
				t.Errorf("%d rows of %s left", n, table)
			}
		}
	})
}

// Two cleanups at once: the one holding the advisory lock deletes, the
// other reports it didn't hold it and deletes nothing. The first holds the
// lock while it waits on an error row a third session holds.
func TestTwoCleanupsAtOnceOneDeletesTheOtherDoesNotHoldTheLock(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		got := enroll(t, s, s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer), "edge-01", Now)
		cutoff := Now.Add(-collectors.ErrorRetention)
		insertErrors(t, s, got, 5, cutoff.Add(-time.Hour))

		blocker, err := s.Databases.Owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = blocker.Rollback(ctx) }()
		if _, err := blocker.Exec(ctx, `select 1 from telflo_opamp.collector_errors
			where organization_id = $1 and collector_instance_id = $2 limit 1 for update`, OrgA, got.ID); err != nil {
			t.Fatal(err)
		}
		var first collectors.CleanupResult
		var firstErr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			first, firstErr = s.Collectors.Cleanup(ctx, cutoff)
		}()
		s.WaitForLockWaiters(t, 1)
		second, err := s.Collectors.Cleanup(ctx, cutoff)
		if err != nil || second.Held || second.Deleted != 0 {
			t.Fatalf("the second Cleanup = %+v, %v; want the lock not held, nothing deleted", second, err)
		}
		if err := blocker.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		<-done
		if firstErr != nil || !first.Held || first.Deleted != 5 {
			t.Fatalf("the first Cleanup = %+v, %v", first, firstErr)
		}
		// The key is the documented one: a session holding it keeps every
		// Cleanup out.
		holder, err := s.Databases.Owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = holder.Rollback(ctx) }()
		var held bool
		if err := holder.QueryRow(ctx, `select pg_try_advisory_xact_lock($1)`, collectors.CleanupLockKey).Scan(&held); err != nil || !held {
			t.Fatal(held, err)
		}
		insertErrors(t, s, got, 1, cutoff.Add(-time.Hour))
		if result, err := s.Collectors.Cleanup(ctx, cutoff); err != nil || result.Held || result.Deleted != 0 {
			t.Fatalf("a Cleanup while a session holds the key = %+v, %v", result, err)
		}
	})
}
