package collectors_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/telflo/opamp-server/store/collectorgroups"
	"github.com/telflo/opamp-server/store/collectors"
	"github.com/telflo/opamp-server/store/fleets"
	"github.com/telflo/opamp-server/store/organizations"
	. "github.com/telflo/opamp-server/store/storetest"
)

// An organization's or a fleet's delete racing a poll that changes its
// collector's token never deadlocks, and finishes. The poll's one
// statement locks the collector's row and then key-shares the token it
// names; a cascade that deleted the fleet's tokens before it reached the
// collector would wait on the poll while the poll waited on it (40P01, the
// delete the victim: a 500 the platform's hook never retries), so the
// deletes lock the collectors first.
//
// On Postgres a third session holds the collector; the poll waits on it,
// then the delete; released, the two finish one after the other. The
// deletes run with no 40P01 retry, so a deadlock fails the test rather
// than being retried away (with the real Poll).
func TestAnOrganizationsDeleteRacingAPollThatChangesTheTokenFinishes(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		deleteRacingAPoll(t, s, func(c fleets.Creation) (bool, error) {
			store := organizations.NewPostgres(s.Databases.Runtime, organizations.WithDeadlockRetries(0))
			return store.Delete(ctx, OrgA)
		})
		if dump := s.Dump(t, OrgA); dump != "" {
			t.Fatalf("the organization's rows survived its delete:\n%s", dump)
		}
	})
}

func TestAFleetsDeleteRacingAPollThatChangesTheTokenFinishes(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		var fleet fleets.Creation
		deleteRacingAPoll(t, s, func(c fleets.Creation) (bool, error) {
			fleet = c
			store := fleets.NewPostgres(s.Databases.Runtime, fleets.WithDeadlockRetries(0))
			return store.Delete(ctx, OrgA, c.Fleet.ID)
		})
		for table, n := range s.FleetRows(t, fleet.Fleet.ID) {
			if n != 0 {
				t.Errorf("%s still holds %d of the fleet's rows", table, n)
			}
		}
	})
}

// deleteRacingAPoll enrolls a collector on the fleet's rotate token, then
// runs a poll on its default token (a token change) beside del, and fails
// the test unless both finish without an error and del deleted.
func deleteRacingAPoll(t *testing.T, s *Stores, del func(fleets.Creation) (bool, error)) {
	t.Helper()
	c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
	got := enroll(t, s, c, "edge-01", Now)
	// More collectors around it, so the delete's lock walks several rows.
	enroll(t, s, c, "edge-02", Now)
	enroll(t, s, c, "edge-03", Now)
	release := blockOn(t, s, 2, `select 1 from opamp_server.collector_instances
		where organization_id = $1 and id = $2 for no key update`, OrgA, got.ID)

	var polled collectors.PollResult
	var pollErr, deleteErr error
	var deleted bool
	pollDone, deleteDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(pollDone)
		polled, pollErr = s.Collectors.Poll(ctx, collectors.PollRequest{OrganizationID: OrgA, FleetID: c.Fleet.ID,
			InstanceUID: got.InstanceUID, TokenID: c.DefaultTokenID, At: Now.Add(time.Minute)})
	}()
	s.WaitForLockWaiters(t, 1)
	go func() {
		defer close(deleteDone)
		deleted, deleteErr = del(c)
	}()
	release()
	<-pollDone
	<-deleteDone
	if pollErr != nil || deleteErr != nil || !deleted {
		t.Fatalf("Poll = %+v, %v; Delete = %v, %v; want both to finish", polled, pollErr, deleted, deleteErr)
	}
}

// The deletes' backstop: a delete refused 40P01 runs again, at most
// DeadlockRetries times. On Postgres a third session makes a cycle no store
// makes (it holds a collector, then asks for the organization's or the
// fleet's row the delete holds); the delete, waiting first, is the victim.
// With no retry it fails 40P01 (the cycle is real): the third session's
// deadlock_timeout is then 30 s, so the delete's check (at 1 s) always runs
// first (with both at 1 s, a check run a few milliseconds apart made the
// third session the victim 3 times in 100). With the default it runs again,
// and the test accepts either way the race ends: the retry waits for the
// third session, which commits, and deletes; or the retry takes the row
// before the third session's woken wait does, the cycle closes again with
// the third session waiting first, and the third session is the victim,
// then the delete deletes. The third session is always rolled back (a
// cleanup registered before any Fatal), so a failure never leaves a
// transaction the pool's Close would wait on; a hang guard bounds every
// wait. Each case has an organization of its own.
//
// The default-retries subtests' power: a delete that never runs again
// still passes one of them about 5% of the time, by the accepted ending
// (the third session the victim, then the delete deletes); it must win
// that race in both, the organization's and the fleet's, so the two
// together pass it about 0.25% of the time. The 0-retries subtests pin the
// cycle itself, deterministically.
//
// A group's delete retries alike: the third session holds the group's
// collector, then key-shares the group the delete holds `for update`.
func TestADeleteRefusedForADeadlockRunsAgain(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		// group is the subtest's group, for the group's case.
		var group uuid.UUID
		for _, tc := range []struct {
			name   string
			lock   string
			delete func(c fleets.Creation, retries int) (bool, error)
		}{
			{"organization", `select 1 from opamp_server.organizations where id = $1 for key share`,
				func(c fleets.Creation, retries int) (bool, error) {
					store := organizations.NewPostgres(s.Databases.Runtime, organizations.WithDeadlockRetries(retries))
					return store.Delete(ctx, c.Fleet.OrganizationID)
				}},
			{"fleet", `select 1 from opamp_server.fleets where organization_id = $1 and id = $2 for key share`,
				func(c fleets.Creation, retries int) (bool, error) {
					store := fleets.NewPostgres(s.Databases.Runtime, fleets.WithDeadlockRetries(retries))
					return store.Delete(ctx, c.Fleet.OrganizationID, c.Fleet.ID)
				}},
			{"group", `select 1 from opamp_server.collector_groups where organization_id = $1 and id = $2 for key share`,
				func(c fleets.Creation, retries int) (bool, error) {
					store := collectorgroups.NewPostgres(s.Databases.Runtime, collectorgroups.WithDeadlockRetries(retries))
					return store.Delete(ctx, c.Fleet.OrganizationID, c.Fleet.ID, group)
				}},
		} {
			for _, retries := range []int{0, organizations.DeadlockRetries} {
				t.Run(fmt.Sprintf("%s, %d retries", tc.name, retries), func(t *testing.T) {
					org := uuid.New()
					if result, _, err := s.Organizations.Create(ctx, organizations.Organization{
						ID: org, Name: "deadlocks", CreatedAt: Now, UpdatedAt: Now}); err != nil || result != organizations.Created {
						t.Fatalf("registering: %v, %v", result, err)
					}
					c := s.CreateFleet(t, org, "edge-prod", fleets.TypeServer)
					got := enroll(t, s, c, "edge-01", Now)
					if tc.name == "group" {
						g, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: org, FleetID: c.Fleet.ID, Name: "gateways", At: Now})
						if err != nil {
							t.Fatal(err)
						}
						if _, err := s.Collectors.Move(ctx, org, c.Fleet.ID, got.ID, g.ID, Now); err != nil {
							t.Fatal(err)
						}
						group = g.ID
					}
					args := []any{org}
					switch tc.name {
					case "fleet":
						args = append(args, c.Fleet.ID)
					case "group":
						args = append(args, group)
					}
					deleted, deleteErr, thirdErr := deleteBesideAThirdSession(t, s, org, got.ID, tc.lock, args, retries == 0,
						func() (bool, error) { return tc.delete(c, retries) })
					switch {
					case retries == 0:
						if !isDeadlock(deleteErr) || thirdErr != nil {
							t.Fatalf("Delete = %v, %v (the third session: %v); want the delete refused 40P01", deleted, deleteErr, thirdErr)
						}
					case deleteErr != nil || !deleted:
						t.Fatalf("Delete = %v, %v (the third session: %v); want deleted", deleted, deleteErr, thirdErr)
					case thirdErr == nil:
						t.Log("the delete was the victim, retried and deleted")
					case isDeadlock(thirdErr):
						t.Log("the third session was the victim, then the delete deleted")
					default:
						t.Fatalf("the third session: %v; want it answered or refused 40P01", thirdErr)
					}
				})
			}
		}
	})
}

// deleteBesideAThirdSession holds the collector in a third session (a
// superuser's, which may set its own deadlock_timeout), runs del, and once
// del waits on it, asks for lock's row in the third session (closing the
// cycle), then commits the third session if it was answered. When
// delFirst, the third session's deadlock_timeout is 30 s, so del's check
// runs first and del is the victim. It returns del's results and the
// third session's error. The third session is rolled back in a cleanup
// registered before anything can fail, and every wait has a hang guard.
func deleteBesideAThirdSession(t *testing.T, s *Stores, org, collector uuid.UUID, lock string, args []any,
	delFirst bool, del func() (bool, error)) (deleted bool, deleteErr, thirdErr error) {
	t.Helper()
	guard, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	third, err := s.Databases.Admin.Begin(guard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = third.Rollback(context.Background()) })
	if delFirst {
		if _, err := third.Exec(guard, `set local deadlock_timeout = '30s'`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := third.Exec(guard, `select 1 from opamp_server.collector_instances
		where organization_id = $1 and id = $2 for update`, org, collector); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		deleted, deleteErr = del()
	}()
	s.WaitForLockWaiters(t, 1)
	if _, thirdErr = third.Exec(guard, lock, args...); thirdErr == nil {
		thirdErr = third.Commit(guard)
	} else if err := third.Rollback(guard); err != nil {
		t.Errorf("rolling the third session back: %v", err)
	}
	select {
	case <-done:
	case <-guard.Done():
		t.Fatal("the delete never finished (the hang guard)")
	}
	return deleted, deleteErr, thirdErr
}

// isDeadlock reports whether err is Postgres's refusal 40P01.
func isDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}
