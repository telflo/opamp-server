package management

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/collectors"
	"github.com/telflo/opamp-server/store/enrollmenttokens"
)

// The collector, limits and usage routes' targets.
func collectorsTarget(organizationID, fleetID string) string {
	return fleetTarget(organizationID, fleetID) + "/collectors"
}

func collectorsPinTarget(organizationID, fleetID string) string {
	return collectorsTarget(organizationID, fleetID) + "/pin"
}

func collectorTarget(organizationID, fleetID, collectorID string) string {
	return collectorsTarget(organizationID, fleetID) + "/" + collectorID
}

func collectorErrorsTarget(organizationID, fleetID, collectorID string) string {
	return collectorTarget(organizationID, fleetID, collectorID) + "/errors"
}

func limitsTarget(organizationID string) string {
	return organizationTarget(organizationID) + "/limits"
}

func usageTarget(organizationID, configurationID string) string {
	return "/api/v1/organizations/" + organizationID + "/configurations/" + configurationID + "/usage"
}

// enrollCollector enrolls a collector named name into the fleet, on its
// rotate token, at the server's clock, through the collectors' store as the
// agent side does; it fails the test unless the collector is created.
func enrollCollector(t *testing.T, s *Server, organizationID, fleetID, name string) collectors.Collector {
	t.Helper()
	enrolled, err := s.collectors.Enroll(context.Background(), collectors.Enrollment{
		OrganizationID: uuid.MustParse(organizationID), FleetID: uuid.MustParse(fleetID), InstanceUID: uuid.New(),
		TokenID:     uuid.MustParse(tokenID(t, s, organizationID, fleetID, enrollmenttokens.KindRotate)),
		DesiredName: name, At: s.clock(),
	})
	if err != nil || !enrolled.Created || enrolled.Collector.Name != name {
		t.Fatalf("enrolling %q: %+v, %v", name, enrolled, err)
	}
	return enrolled.Collector
}

func seedCollector(organizationID, fleetID, name string) func(t *testing.T, s *Server) {
	return func(t *testing.T, s *Server) {
		t.Helper()
		enrollCollector(t, s, organizationID, fleetID, name)
	}
}

// collectorID is the id of the fleet's collector named name, read from the
// store.
func collectorID(t *testing.T, s *Server, organizationID, fleetID, name string) string {
	t.Helper()
	page, err := s.collectors.List(context.Background(), uuid.MustParse(organizationID), uuid.MustParse(fleetID),
		collectors.ListOptions{Q: name, Sort: collectors.SortName, Order: collectors.Ascending, Limit: collectors.MaxListLimit})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range page.Collectors {
		if l.Name == name {
			return l.ID.String()
		}
	}
	t.Fatalf("no collector named %q", name)
	return ""
}

// collectorTargetOf is a targetOf for the fleet's collector named name,
// through target (collectorTarget or collectorErrorsTarget).
func collectorTargetOf(organizationID, fleetID, name string, target func(o, f, c string) string) func(t *testing.T, s *Server) string {
	return func(t *testing.T, s *Server) string {
		return target(organizationID, fleetID, collectorID(t, s, organizationID, fleetID, name))
	}
}

// seedReported enrolls a collector named name into the fleet and records,
// as the agent side does, the published version publicationID (of yaml)
// served to it, its report of failing to apply it, and its reported
// fields with attributes.
func seedReported(organizationID, fleetID, name, publicationID, yaml string) func(t *testing.T, s *Server) {
	return func(t *testing.T, s *Server) {
		t.Helper()
		c := enrollCollector(t, s, organizationID, fleetID, name)
		version, ip := "0.162.0", "10.1.2.3"
		written, err := s.collectors.Report(context.Background(), collectors.Report{
			OrganizationID: c.OrganizationID, FleetID: c.FleetID, CollectorID: c.ID,
			Served: &collectors.Served{Ref: collectors.Ref{Source: collectors.SourcePublication, ID: uuid.MustParse(publicationID)},
				SHA256: sha256Hex(yaml), At: s.clock()},
			Effective: &collectors.Effective{SHA256: sha256Hex(yaml), Status: collectors.StatusFailed, Error: "failed", At: s.clock()},
			Reported: collectors.Reported{CollectorVersion: &version, IP: &ip, HostIP: &ip,
				Attributes: map[string]string{"os.type": "linux", "service.name": "otelcol-contrib"}},
		})
		if err != nil || !written {
			t.Fatalf("reporting: %v, %v", written, err)
		}
	}
}

// seedAssignedGroup creates the fleet's group name, assigned the published
// version publicationID.
func seedAssignedGroup(organizationID, fleetID, name, publicationID string) func(t *testing.T, s *Server) {
	return func(t *testing.T, s *Server) {
		t.Helper()
		got := call(s, http.MethodPost, groupsTarget(organizationID, fleetID),
			[]byte(`{"name":"`+name+`","assignedPublicationId":"`+publicationID+`"}`), jsonType)
		expectStatus(t, "creating an assigned group", got, http.StatusCreated)
	}
}

// seedPin pins the fleet's collector named name to the published version
// publicationID.
func seedPin(organizationID, fleetID, name, publicationID string) func(t *testing.T, s *Server) {
	return func(t *testing.T, s *Server) {
		t.Helper()
		got := call(s, http.MethodPatch, collectorTarget(organizationID, fleetID, collectorID(t, s, organizationID, fleetID, name)),
			[]byte(`{"pin":{"kind":"publication","id":"`+publicationID+`"}}`), jsonType)
		expectStatus(t, "pinning", got, http.StatusOK)
	}
}

// seedCollectorError records an error row for the fleet's collector named
// name.
func seedCollectorError(organizationID, fleetID, name, message string) func(t *testing.T, s *Server) {
	return func(t *testing.T, s *Server) {
		t.Helper()
		id := uuid.MustParse(collectorID(t, s, organizationID, fleetID, name))
		if err := s.collectors.RecordError(context.Background(), uuid.MustParse(organizationID), uuid.MustParse(fleetID), id,
			collectors.ErrorReport{Message: message, At: s.clock()}); err != nil {
			t.Fatal(err)
		}
	}
}

// failingCollectors fails every call, as a database that is down would.
type failingCollectors struct{}

func (failingCollectors) Enroll(context.Context, collectors.Enrollment) (collectors.Enrolled, error) {
	return collectors.Enrolled{}, errStoreDown
}
func (failingCollectors) Poll(context.Context, collectors.PollRequest) (collectors.PollResult, error) {
	return collectors.PollResult{}, errStoreDown
}
func (failingCollectors) YAML(context.Context, uuid.UUID, collectors.Ref) (collectors.YAMLResult, error) {
	return collectors.YAMLResult{}, errStoreDown
}
func (failingCollectors) Report(context.Context, collectors.Report) (bool, error) {
	return false, errStoreDown
}
func (failingCollectors) RecordError(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, collectors.ErrorReport) error {
	return errStoreDown
}
func (failingCollectors) ListErrors(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int, *collectors.ErrorCursor) (collectors.ErrorPage, error) {
	return collectors.ErrorPage{}, errStoreDown
}
func (failingCollectors) Cleanup(context.Context, time.Time) (collectors.CleanupResult, error) {
	return collectors.CleanupResult{}, errStoreDown
}
func (failingCollectors) List(context.Context, uuid.UUID, uuid.UUID, collectors.ListOptions) (collectors.Page, error) {
	return collectors.Page{}, errStoreDown
}
func (failingCollectors) Get(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (collectors.Listed, error) {
	return collectors.Listed{}, errStoreDown
}
func (failingCollectors) Rename(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, time.Time) (collectors.Collector, error) {
	return collectors.Collector{}, errStoreDown
}
func (failingCollectors) Move(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, time.Time) (collectors.Collector, error) {
	return collectors.Collector{}, errStoreDown
}
func (failingCollectors) Pin(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, *collectors.Ref, time.Time) (collectors.Collector, error) {
	return collectors.Collector{}, errStoreDown
}
func (failingCollectors) Update(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, collectors.Changes, time.Time) (collectors.Listed, error) {
	return collectors.Listed{}, errStoreDown
}
func (failingCollectors) BulkPin(context.Context, uuid.UUID, uuid.UUID, []uuid.UUID, *collectors.PinChange, *collectors.VariableGroupChange, time.Time) (collectors.BulkPinResult, error) {
	return collectors.BulkPinResult{}, errStoreDown
}
func (failingCollectors) Delete(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error) {
	return false, errStoreDown
}
func (failingCollectors) SummariesForFleets(context.Context, uuid.UUID, []uuid.UUID, time.Time) (map[uuid.UUID]collectors.Summary, error) {
	return nil, errStoreDown
}
