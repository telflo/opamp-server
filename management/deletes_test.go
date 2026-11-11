package management

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/telflo-opamp-server/store/fleets"
	"github.com/telflo/telflo-opamp-server/store/organizations"
	"github.com/telflo/telflo-opamp-server/store/publications"
)

// deleteSeen is what a store's Delete saw of its context: whether it had
// ended, and its deadline.
type deleteSeen struct {
	err         error
	deadline    time.Time
	hasDeadline bool
}

func see(ctx context.Context) deleteSeen {
	deadline, ok := ctx.Deadline()
	return deleteSeen{err: ctx.Err(), deadline: deadline, hasDeadline: ok}
}

// seeingOrganizations and seeingFleets record their Delete's context.
type seeingOrganizations struct {
	organizations.Store
	seen *deleteSeen
}

func (s seeingOrganizations) Delete(ctx context.Context, id uuid.UUID) (bool, error) {
	*s.seen = see(ctx)
	return s.Store.Delete(ctx, id)
}

type seeingFleets struct {
	fleets.Store
	seen *deleteSeen
}

func (s seeingFleets) Delete(ctx context.Context, organizationID, id uuid.UUID) (bool, error) {
	*s.seen = see(ctx)
	return s.Store.Delete(ctx, organizationID, id)
}

// hangingUpOrganizations calls hangUp once an Exists returns: a caller that
// goes away after the handler's check has read the organization, before
// the delete.
type hangingUpOrganizations struct {
	organizations.Store
	hangUp func()
}

func (h hangingUpOrganizations) Exists(ctx context.Context, id uuid.UUID) (bool, error) {
	exists, err := h.Store.Exists(ctx, id)
	h.hangUp()
	return exists, err
}

// The bound is 5 minutes unless Options say otherwise; a negative one is
// refused.
func TestTheDeleteTimeoutIsFiveMinutesUnlessGiven(t *testing.T) {
	if DeleteTimeout != 5*time.Minute {
		t.Fatalf("DeleteTimeout is %v, want 5m", DeleteTimeout)
	}
	s, _, _ := fleetServer(t)
	if s.deleteTimeout != DeleteTimeout {
		t.Fatalf("a server's delete timeout is %v by default, want %v", s.deleteTimeout, DeleteTimeout)
	}
	options := func(timeout time.Duration) Options {
		secrets, err := NewSecrets(testSecret, nil)
		if err != nil {
			t.Fatal(err)
		}
		d := testDB(t)
		fs := postgresFleetStores(d.Runtime)
		return Options{
			Secrets: secrets, Organizations: organizations.NewPostgres(d.Runtime), Publications: publications.NewPostgres(d.Runtime),
			Fleets: fs.fleets, CollectorGroups: fs.groups, EnrollmentTokens: fs.tokens,
			Collectors: fs.collectors, Variables: fs.variables, APIKeys: fs.apiKeys, Keys: testKeys(t),
			Clock: newTestClock().Now, Logger: slog.New(slog.DiscardHandler), DeleteTimeout: timeout,
		}
	}
	given, err := New(options(7 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if given.deleteTimeout != 7*time.Minute {
		t.Fatalf("a server given 7m has %v", given.deleteTimeout)
	}
	if _, err := New(options(-time.Second)); err == nil {
		t.Fatal("a negative delete timeout was accepted")
	}
}

// Each delete's store call runs on a context its caller's hang-up didn't
// end, bounded by the server's delete timeout from when it started: here
// the request's context ends before the store's Delete (before the handler
// runs for an organization's, which reads nothing first; once its check has
// read the organization for a fleet's, since that check reads the request's
// own context), and the store still sees a live context with the 5-minute
// deadline.
func TestEachDeleteRunsOnALiveContextBoundedByTheDeleteTimeout(t *testing.T) {
	for _, c := range []struct {
		name   string
		target string
		// checksFirst: the handler reads the organization, on the request's
		// own context, before the delete, so the caller hangs up after that
		// check (an ended context would refuse the check's query).
		checksFirst bool
	}{
		{"an organization's", organizationTarget(orgA), false},
		{"a fleet's", fleetTarget(orgA, fleetA), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var seen deleteSeen
			ctx, hangUp := context.WithCancel(context.Background())
			var deleting atomic.Bool
			d := testDB(t)
			registry := organizations.NewPostgres(d.Runtime)
			register(t, registry, orgA)
			fs := postgresFleetStores(d.Runtime)
			fs.fleets = seeingFleets{fs.fleets, &seen}
			s, _, _ := newServerWith(t, hangingUpOrganizations{seeingOrganizations{registry, &seen}, func() {
				if deleting.Load() {
					hangUp()
				}
			}}, publications.NewPostgres(d.Runtime), fs)
			createFleet(t, s, orgA, fleetA, "edge-prod", "server")

			if c.checksFirst {
				deleting.Store(true)
			} else {
				hangUp()
			}
			request := httptest.NewRequest(http.MethodDelete, c.target, nil).WithContext(ctx)
			request.Header.Set("Authorization", "Bearer "+testSecret)
			recorder := httptest.NewRecorder()
			before := time.Now()
			s.Handler().ServeHTTP(recorder, request)
			after := time.Now()

			if !seen.hasDeadline {
				t.Fatal("the store's Delete ran without a deadline")
			}
			if seen.err != nil {
				t.Fatalf("the store's Delete ran on an ended context: %v", seen.err)
			}
			if seen.deadline.Before(before.Add(DeleteTimeout)) || seen.deadline.After(after.Add(DeleteTimeout)) {
				t.Fatalf("the deadline is %v after the request, want %v", seen.deadline.Sub(before), DeleteTimeout)
			}
			expectStatus(t, "the delete", recorder, http.StatusNoContent)
		})
	}
}

// seedOrganizationDeleteRunning and seedFleetDeleteRunning mark a delete
// running on the server, as a first delete still in flight would
// (deletes.go): for the conformance cases and the tests below.
func seedOrganizationDeleteRunning(organizationID string) func(t *testing.T, s *Server) {
	return func(t *testing.T, s *Server) {
		t.Helper()
		if _, started := s.deleting.startOrganization(uuid.MustParse(organizationID)); !started {
			t.Fatal("the organization's delete was already marked running")
		}
	}
}

func seedFleetDeleteRunning(organizationID, fleetID string) func(t *testing.T, s *Server) {
	return func(t *testing.T, s *Server) {
		t.Helper()
		if _, started := s.deleting.startFleet(uuid.MustParse(organizationID), uuid.MustParse(fleetID)); !started {
			t.Fatal("the fleet's delete was already marked running")
		}
	}
}

// expectDeleteInProgress checks the 503 a delete gets while this server is
// already running it: the code, the message and the Retry-After.
func expectDeleteInProgress(t *testing.T, label string, got *httptest.ResponseRecorder) {
	t.Helper()
	expectError(t, label, got, http.StatusServiceUnavailable, codeDeleteInProgress, messageDeleteInProgress)
	if retryAfter := got.Header().Get("Retry-After"); retryAfter != "10" {
		t.Fatalf("%s: Retry-After %q, want 10", label, retryAfter)
	}
}

// While this server runs an organization's delete, a second delete of it
// answers 503 delete_in_progress at once, and so does a delete of any of its
// fleets; another organization's deletes go on. Once the first has
// returned, the organization's delete runs again.
func TestADeleteOfAnOrganizationBeingDeletedAnswers503AtOnce(t *testing.T) {
	s, _, log := fleetServer(t)
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	createFleet(t, s, orgB, fleetB, "edge-prod", "server")
	done, started := s.deleting.startOrganization(uuid.MustParse(orgA))
	if !started {
		t.Fatal("nothing was running, yet the organization's delete didn't start")
	}

	expectDeleteInProgress(t, "the organization's delete again", call(s, http.MethodDelete, organizationTarget(orgA), nil, ""))
	expectDeleteInProgress(t, "a delete of one of its fleets", call(s, http.MethodDelete, fleetTarget(orgA, fleetA), nil, ""))
	// Nothing was deleted by either.
	expectStatus(t, "its fleet", call(s, http.MethodGet, fleetTarget(orgA, fleetA), nil, ""), http.StatusOK)
	// Another organization's fleet, and the organization itself, are deleted as ever.
	expectStatus(t, "another organization's fleet", call(s, http.MethodDelete, fleetTarget(orgB, fleetB), nil, ""), http.StatusNoContent)
	expectStatus(t, "another organization", call(s, http.MethodDelete, organizationTarget(orgB), nil, ""), http.StatusNoContent)

	done()
	expectStatus(t, "its fleet, the first delete returned", call(s, http.MethodDelete, fleetTarget(orgA, fleetA), nil, ""), http.StatusNoContent)
	expectStatus(t, "the organization, the first delete returned", call(s, http.MethodDelete, organizationTarget(orgA), nil, ""), http.StatusNoContent)
	expectError(t, "a retry once it is gone", call(s, http.MethodDelete, organizationTarget(orgA), nil, ""), http.StatusNotFound, codeNotFound, messageNotFound)
	if strings.Contains(log.String(), orgA) || strings.Contains(log.String(), fleetA) {
		t.Fatalf("a 503's log line names an id: %s", log.String())
	}
}

// While this server runs a fleet's delete, a second delete of that fleet
// answers 503 delete_in_progress at once; another fleet's delete, and the
// organization's own (it isn't a repeat: it waits behind the fleet's rows),
// go on. Once the first has returned, the fleet's delete runs again.
func TestADeleteOfAFleetBeingDeletedAnswers503AtOnce(t *testing.T) {
	s, _, _ := fleetServer(t)
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	createFleet(t, s, orgA, fleetA2, "edge-dev", "server")
	done, started := s.deleting.startFleet(uuid.MustParse(orgA), uuid.MustParse(fleetA))
	if !started {
		t.Fatal("nothing was running, yet the fleet's delete didn't start")
	}

	expectDeleteInProgress(t, "the fleet's delete again", call(s, http.MethodDelete, fleetTarget(orgA, fleetA), nil, ""))
	expectStatus(t, "the fleet", call(s, http.MethodGet, fleetTarget(orgA, fleetA), nil, ""), http.StatusOK)
	expectStatus(t, "another fleet of the organization", call(s, http.MethodDelete, fleetTarget(orgA, fleetA2), nil, ""), http.StatusNoContent)

	done()
	expectStatus(t, "the fleet, the first delete returned", call(s, http.MethodDelete, fleetTarget(orgA, fleetA), nil, ""), http.StatusNoContent)
	expectError(t, "a retry once it is gone", call(s, http.MethodDelete, fleetTarget(orgA, fleetA), nil, ""), http.StatusNotFound, codeFleetNotFound, messageFleetNotFound)

	// The organization's delete isn't refused while one of its fleets' runs.
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	done, _ = s.deleting.startFleet(uuid.MustParse(orgA), uuid.MustParse(fleetA))
	defer done()
	expectStatus(t, "the organization, one of its fleets being deleted", call(s, http.MethodDelete, organizationTarget(orgA), nil, ""), http.StatusNoContent)
}

// The in-progress set's own rules: one mark per organization and per
// fleet, a fleet refused while its organization is marked, each unmarked by
// its own release only, and the path ids read before it (a malformed id is
// still 400).
func TestTheDeletesInProgressSet(t *testing.T) {
	var d deletesInProgress
	a, b := uuid.MustParse(orgA), uuid.MustParse(orgB)
	f, f2 := uuid.MustParse(fleetA), uuid.MustParse(fleetA2)

	releaseF, ok := d.startFleet(a, f)
	if !ok {
		t.Fatal("the first fleet delete didn't start")
	}
	if _, ok := d.startFleet(a, f); ok {
		t.Fatal("a second delete of the same fleet started")
	}
	if _, ok := d.startFleet(b, f); !ok {
		t.Fatal("the same fleet id under another organization was refused")
	}
	releaseA, ok := d.startOrganization(a)
	if !ok {
		t.Fatal("the organization's delete was refused while only one of its fleets' runs")
	}
	if _, ok := d.startOrganization(a); ok {
		t.Fatal("a second delete of the same organization started")
	}
	if _, ok := d.startFleet(a, f2); ok {
		t.Fatal("a fleet's delete started while its organization's runs")
	}
	releaseA()
	if _, ok := d.startFleet(a, f); ok {
		t.Fatal("the organization's release unmarked the fleet's delete")
	}
	releaseF()
	if _, ok := d.startFleet(a, f); !ok {
		t.Fatal("the fleet's delete didn't start again after its release")
	}

	s, _, _ := fleetServer(t)
	seedOrganizationDeleteRunning(orgA)(t, s)
	expectStatus(t, "a malformed fleet id", call(s, http.MethodDelete, fleetTarget(orgA, "1"), nil, ""), http.StatusBadRequest)
	expectStatus(t, "a malformed organization id", call(s, http.MethodDelete, organizationTarget("1"), nil, ""), http.StatusBadRequest)
}
