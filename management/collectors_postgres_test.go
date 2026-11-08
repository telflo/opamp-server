package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/telflo/opamp-server/store/collectors"
)

// On Postgres too: another organization's ids are 404 on every collector
// route and change neither organization's rows; another fleet's
// are 404 through this fleet; the fleet reads' summaries and a collector's
// liveness and sync come from the server's clock; an in-use published
// version isn't deleted.
func TestTheCollectorRoutesOnPostgres(t *testing.T) {
	t.Run("another organization's ids", func(t *testing.T) {
		s, _, _, d := postgresFleetServer(t)
		theirCollector := fleetWithCollector(t, s, orgA, fleetA, "edge-prod")
		createFleet(t, s, orgB, fleetB, "edge-b", "server")
		beforeA, beforeB := pgDump(t, d, orgA), pgDump(t, d, orgB)
		assertAnotherOrganizationsIdsAreNotFound(t, s, theirCollector)
		if after := pgDump(t, d, orgA); after != beforeA {
			t.Fatalf("B's calls changed A's rows:\n%s\n%s", beforeA, after)
		}
		if after := pgDump(t, d, orgB); after != beforeB {
			t.Fatalf("the calls changed B's rows:\n%s\n%s", beforeB, after)
		}
	})
	t.Run("another fleet's ids", func(t *testing.T) {
		s, _, _, d := postgresFleetServer(t)
		theirCollector := fleetWithCollector(t, s, orgA, fleetA, "edge-prod")
		createFleet(t, s, orgA, fleetA2, "edge-dev", "server")
		before := pgDump(t, d, orgA)
		assertAnotherFleetsIdsAreNotFound(t, s, theirCollector)
		if after := pgDump(t, d, orgA); after != before {
			t.Fatalf("the calls through another fleet changed the organization's rows:\n%s\n%s", before, after)
		}
	})
	t.Run("liveness at the ladder's edges", func(t *testing.T) {
		s, _, _, _ := postgresFleetServer(t)
		assertLivenessEdges(t, s)
	})
	t.Run("sync, liveness and the summary", func(t *testing.T) {
		s, clock, _, _ := postgresFleetServer(t)
		assertSyncAndLivenessReplies(t, s, clock)
	})
	t.Run("an in-use published version", func(t *testing.T) {
		s, _, _, d := postgresFleetServer(t)
		assertPublishedVersionsInUseAreKept(t, s, func() string { return pgDump(t, d, orgA) })
	})
}

// The privacy rule on the collector routes, on Postgres so the
// database spans are seen too: a collector's name (its ServerName at
// enrollment, and a rename), its reported attributes' values, its
// effective configuration's error text, its error rows' text, a fleet's
// label and a group's name reach no log line, no span and no error
// answer. The attributes and the error text are answered only by the
// collector routes (the error rows' by the error history).
func TestTheCollectorRoutesLogAndTraceNoNameAttributeOrError(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	s, clock, log, _ := postgresFleetServer(t)
	const (
		label       = "fleet-label-canary-6a1f"
		groupName   = "group-name-canary-2c7e"
		serverName  = "server-name-canary-0d93"
		renamed     = "collector-name-canary-8b14"
		attribute   = "attribute-value-canary-55e0"
		effective   = "effective-error-canary-c3a9"
		errorRow    = "error-row-canary-91d2"
		pinnedName  = "pinned-name-canary-1e66"
		bulkPinName = "bulk-name-canary-7d02"
	)
	ctx := context.Background()
	org, fleet := uuid.MustParse(orgA), uuid.MustParse(fleetA)
	createFleet(t, s, orgA, fleetA, label, "server")
	seedPublication(orgA, publicationA, validBody(sampleYAML))(t, s)
	seedAssignedGroup(orgA, fleetA, groupName, publicationA)(t, s)
	c := enrollCollector(t, s, orgA, fleetA, serverName)
	enrollCollector(t, s, orgA, fleetA, pinnedName)
	seedPin(orgA, fleetA, pinnedName, publicationA)(t, s)
	other := enrollCollector(t, s, orgA, fleetA, bulkPinName)
	if _, err := s.collectors.Report(ctx, collectors.Report{OrganizationID: org, FleetID: fleet, CollectorID: c.ID,
		Served: &collectors.Served{Ref: collectors.Ref{Source: collectors.SourcePublication, ID: uuid.MustParse(publicationA)},
			SHA256: sha256Hex(sampleYAML), At: clock.Now()},
		Effective: &collectors.Effective{SHA256: sha256Hex(sampleYAML), Status: collectors.StatusFailed, Error: effective, At: clock.Now()},
		Reported:  collectors.Reported{Attributes: map[string]string{"host.name": attribute}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.collectors.RecordError(ctx, org, fleet, c.ID, collectors.ErrorReport{Message: errorRow, At: clock.Now()}); err != nil {
		t.Fatal(err)
	}

	type answer struct {
		route string
		got   *httptest.ResponseRecorder
	}
	var answers []answer
	send := func(route, method, target string, b body) {
		answers = append(answers, answer{route, sendWith(s, method, target, b)})
	}
	id, otherID := c.ID.String(), other.ID.String()
	clock.Advance(time.Minute)
	send("list collectors", http.MethodGet, collectorsTarget(orgA, fleetA)+"?q=canary&sort=name", nil)
	send("get collector", http.MethodGet, collectorTarget(orgA, fleetA, id), nil)
	send("list collector errors", http.MethodGet, collectorErrorsTarget(orgA, fleetA, id), nil)
	send("patch collector", http.MethodPatch, collectorTarget(orgA, fleetA, id), body{"name": renamed, "pin": nil})
	send("patch collector", http.MethodPatch, collectorTarget(orgA, fleetA, id), body{"name": bulkPinName})                                           // 409
	send("patch collector", http.MethodPatch, collectorTarget(orgA, fleetA, id), body{"name": " " + renamed})                                         // 400
	send("patch collector", http.MethodPatch, collectorTarget(orgA, fleetA, id), body{"pin": map[string]any{"kind": "publication", "id": unknownID}}) // 422
	send("patch collector", http.MethodPatch, collectorTarget(orgA, fleetA, id), body{"pin": map[string]any{"kind": "import", "id": unknownID}})      // 400
	send("pin collectors", http.MethodPost, collectorsPinTarget(orgA, fleetA), body{"collectorIds": []string{otherID, unknownID},
		"pin": map[string]any{"kind": "publication", "id": publicationA}})
	send("list fleets", http.MethodGet, fleetsTarget(orgA), nil)
	send("get fleet", http.MethodGet, fleetTarget(orgA, fleetA), nil)
	send("usage", http.MethodGet, usageTarget(orgA, configurationA), nil)
	send("delete publication", http.MethodDelete, publicationTarget(orgA, publicationA), nil) // 409
	send("delete collector", http.MethodDelete, collectorTarget(orgA, fleetA, otherID), nil)

	// Where each canary may be answered, and nowhere else.
	allowed := map[string][]string{
		attribute: {"list collectors", "get collector", "patch collector"},
		effective: {"list collectors", "get collector", "patch collector"},
		errorRow:  {"list collector errors"},
	}
	for _, a := range answers {
		if a.got.Code >= 400 {
			for _, canary := range []string{label, groupName, serverName, renamed, attribute, effective, errorRow, pinnedName, bulkPinName} {
				if strings.Contains(a.got.Body.String(), canary) {
					t.Errorf("%s's error answer holds %q: %s", a.route, canary, a.got.Body.String())
				}
			}
			continue
		}
		for canary, routes := range allowed {
			if strings.Contains(a.got.Body.String(), canary) && !containsString(routes, a.route) {
				t.Errorf("%s's answer holds %q", a.route, canary)
			}
		}
	}
	// Each canary was answered where it belongs: the test sees them.
	seen := map[string]bool{}
	for _, a := range answers {
		for canary := range allowed {
			if strings.Contains(a.got.Body.String(), canary) {
				seen[canary] = true
			}
		}
	}
	for canary := range allowed {
		if !seen[canary] {
			t.Errorf("no answer holds %q: the test doesn't reach it", canary)
		}
	}

	logged := log.String()
	if !strings.Contains(logged, "/api/v1/organizations/{organizationId}/fleets/{fleetId}/collectors/{collectorId}") {
		t.Fatalf("the log doesn't name the routes: %s", logged)
	}
	spans := recorder.Ended()
	if len(spans) < 20 {
		t.Fatalf("%d spans, want the server's and the database's", len(spans))
	}
	var traced strings.Builder
	for _, span := range spans {
		traced.WriteString(span.Name() + "\n" + span.Status().Description + "\n")
		for _, attr := range span.Attributes() {
			traced.WriteString(string(attr.Key) + "=" + attr.Value.String() + "\n")
		}
		for _, event := range span.Events() {
			traced.WriteString(event.Name + "\n")
			for _, attr := range event.Attributes {
				traced.WriteString(string(attr.Key) + "=" + attr.Value.String() + "\n")
			}
		}
	}
	for _, canary := range []string{label, groupName, serverName, renamed, attribute, effective, errorRow, pinnedName,
		bulkPinName, "canary", testSecret} {
		if strings.Contains(logged, canary) {
			t.Errorf("the log holds %q", canary)
		}
		if strings.Contains(traced.String(), canary) {
			t.Errorf("a span holds %q", canary)
		}
	}
}

func containsString(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
