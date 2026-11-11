package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/telflo/telflo-opamp-server/store/database/databasetest"
	"github.com/telflo/telflo-opamp-server/store/organizations"
	"github.com/telflo/telflo-opamp-server/store/publications"
	"github.com/telflo/telflo-opamp-server/store/storetest"
)

// postgresFleetServer is a Server over every Postgres store on a fresh,
// migrated database, as telflo_opamp, with organizations A and B
// registered; and its log.
func postgresFleetServer(t *testing.T) (*Server, *testClock, *syncBuffer, *databasetest.Databases) {
	t.Helper()
	d := testDB(t)
	registry := organizations.NewPostgres(d.Runtime)
	register(t, registry, orgA, orgB)
	s, clock, log := newServerWith(t, registry, publications.NewPostgres(d.Runtime), postgresFleetStores(d.Runtime))
	return s, clock, log, d
}

func pgDump(t *testing.T, d *databasetest.Databases, organizationID string) string {
	t.Helper()
	return (&storetest.Stores{Databases: d}).Dump(t, uuid.MustParse(organizationID))
}

// eventRows are the fleet's token events in the database, read as the
// superuser: kind, token and actor's email, newest first.
func eventRows(t *testing.T, d *databasetest.Databases, fleetID string) []string {
	t.Helper()
	rows, err := d.Admin.Query(context.Background(), `
		select event || ' ' || enrollment_token_id || ' ' || coalesce(actor_email, '-')
		  from telflo_opamp.enrollment_token_events
		 where fleet_id = $1
		 order by occurred_at desc, event`, fleetID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	return events
}

// On Postgres: a create makes the fleet, its Default group, two tokens and
// two `issued` events in the database, and both bearers open to them; a
// rotation, a reveal, a revoke and a new default token after the default
// token's revoke answer and audit; the fleet's delete takes everything.
func TestFleetCreateRotateRevealAndRevokeOnPostgres(t *testing.T) {
	s, clock, _, d := postgresFleetServer(t)
	c := createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	if claims := readClaims(t, c.Tokens.Default.Bearer); claims.TokenID.String() != c.Tokens.Default.ID || claims.FleetID.String() != fleetA {
		t.Fatalf("the default bearer carries %+v", claims)
	}
	if claims := readClaims(t, c.Tokens.Rotate.Bearer); claims.TokenID.String() != c.Tokens.Rotate.ID || claims.FleetID.String() != fleetA {
		t.Fatalf("the rotate bearer carries %+v", claims)
	}
	var groups, tokenRows int
	if err := d.Admin.QueryRow(context.Background(), `select
		(select count(*) from telflo_opamp.collector_groups where fleet_id = $1 and is_default and name = 'Default' and id = $2),
		(select count(*) from telflo_opamp.enrollment_tokens where fleet_id = $1)`, fleetA, c.DefaultGroup.ID).Scan(&groups, &tokenRows); err != nil {
		t.Fatal(err)
	}
	if groups != 1 || tokenRows != 2 || len(eventRows(t, d, fleetA)) != 2 {
		t.Fatalf("the database holds %d Default groups, %d tokens, events %v", groups, tokenRows, eventRows(t, d, fleetA))
	}
	retry := call(s, http.MethodPost, fleetsTarget(orgA), fleetBody(fleetA, "edge-prod", "server").bytes(), jsonType)
	expectStatus(t, "the same create", retry, http.StatusOK)

	clock.Advance(time.Minute)
	rotated := call(s, http.MethodPost, rotateTarget(orgA, fleetA), rotationBody(nil, "2026-10-04T15:00:00.000Z").bytes(), jsonType)
	expectStatus(t, "rotate", rotated, http.StatusCreated)
	var r rotatedReply
	decodeInto(t, rotated.Body.Bytes(), &r)
	if claims := readClaims(t, r.Bearer); claims.TokenID.String() != r.ID || r.PredecessorID == nil || *r.PredecessorID != c.Tokens.Rotate.ID {
		t.Fatalf("the rotation = %+v carrying %+v", r, claims)
	}
	events := eventRows(t, d, fleetA)
	if len(events) != 4 || !strings.HasPrefix(events[0], "issued "+r.ID) || !strings.HasPrefix(events[1], "rotated "+c.Tokens.Rotate.ID) {
		t.Fatalf("the events after the rotation = %v", events)
	}
	// The fleet's reads answer the rotation's grace from Postgres too.
	if got, want := fleetSummaryOf(t, s, orgA, fleetA), (summaryReply{
		RotateToken: &rotateTokenReply{GraceEndsAt: ptr("2026-10-04T15:00:00.000Z"), ValidCount: 2}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("the summary is %s, want %s", describeSummary(got), describeSummary(want))
	}

	clock.Advance(time.Minute)
	revealed := call(s, http.MethodPost, revealTarget(orgA, fleetA, r.ID), actorBody().bytes(), jsonType)
	expectStatus(t, "reveal", revealed, http.StatusOK)
	var v revealedReply
	decodeInto(t, revealed.Body.Bytes(), &v)
	if claims := readClaims(t, v.Bearer); claims.TokenID.String() != r.ID || v.Bearer == r.Bearer {
		t.Fatalf("the reveal = %+v", v)
	}
	if events := eventRows(t, d, fleetA); events[0] != "viewed "+r.ID+" "+sampleEmail {
		t.Fatalf("the reveal's event = %v", events[0])
	}

	clock.Advance(time.Minute)
	expectStatus(t, "revoke", call(s, http.MethodPost, revokeTarget(orgA, fleetA, r.ID), actorBody().bytes(), jsonType), http.StatusOK)
	expectStatus(t, "revoke again", call(s, http.MethodPost, revokeTarget(orgA, fleetA, r.ID), actorBody().bytes(), jsonType), http.StatusOK)
	events = eventRows(t, d, fleetA)
	if len(events) != 6 || events[0] != "revoked "+r.ID+" "+sampleEmail {
		t.Fatalf("the events after two revokes = %v", events)
	}
	expectError(t, "reveal the revoked", call(s, http.MethodPost, revealTarget(orgA, fleetA, r.ID), actorBody().bytes(), jsonType),
		http.StatusConflict, codeEnrollmentTokenRevoked, messageEnrollmentTokenRevoked)

	clock.Advance(time.Minute)
	expectError(t, "a new default token beside a valid one",
		call(s, http.MethodPost, issueDefaultTarget(orgA, fleetA), actorBody().bytes(), jsonType),
		http.StatusConflict, codeEnrollmentTokenDefaultExists, messageDefaultTokenExists)
	expectStatus(t, "revoke the default token",
		call(s, http.MethodPost, revokeTarget(orgA, fleetA, c.Tokens.Default.ID), actorBody().bytes(), jsonType), http.StatusOK)
	issued := call(s, http.MethodPost, issueDefaultTarget(orgA, fleetA), actorBody().bytes(), jsonType)
	expectStatus(t, "a new default token", issued, http.StatusCreated)
	var n revealedReply
	decodeInto(t, issued.Body.Bytes(), &n)
	if claims := readClaims(t, n.Bearer); claims.TokenID.String() != n.ID || claims.FleetID.String() != fleetA ||
		n.Kind != "default" || n.ExpiresAt != nil || n.ID == c.Tokens.Default.ID {
		t.Fatalf("the new default token = %+v carrying %+v", n, claims)
	}
	if events := eventRows(t, d, fleetA); len(events) != 8 || events[0] != "issued "+n.ID+" "+sampleEmail {
		t.Fatalf("the events after the new default token = %v", events)
	}

	expectStatus(t, "delete the fleet", call(s, http.MethodDelete, fleetTarget(orgA, fleetA), nil, ""), http.StatusNoContent)
	if strings.Contains(pgDump(t, d, orgA), fleetA) {
		t.Fatal("the fleet's rows survived its delete")
	}
}

// On Postgres too, every route with another organization's ids answers
// 404 and changes neither organization's rows.
func TestAnotherOrganizationsIdsAreNotFoundOnPostgres(t *testing.T) {
	s, _, _, d := postgresFleetServer(t)
	theirs := createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	seedGroup(orgA, fleetA, "gateways")(t, s)
	theirGroup := groupID(t, s, orgA, fleetA, "gateways")
	mine := createFleet(t, s, orgB, fleetB, "edge-b", "server")
	beforeA, beforeB := pgDump(t, d, orgA), pgDump(t, d, orgB)
	for _, r := range fleetRoutes() {
		if r.name == "create fleet" || r.name == "list fleets" {
			continue
		}
		if got := sendWith(s, r.method, r.target(orgB, theirs.Fleet.ID, theirGroup, theirs.Tokens.Rotate.ID), r.body); got.Code != http.StatusNotFound {
			t.Errorf("%s with A's fleet under B: %d %s", r.name, got.Code, got.Body.String())
		}
		if namesGroupOrToken(r.name) {
			if got := sendWith(s, r.method, r.target(orgB, mine.Fleet.ID, theirGroup, theirs.Tokens.Rotate.ID), r.body); got.Code != http.StatusNotFound {
				t.Errorf("%s with A's group or token under B's fleet: %d %s", r.name, got.Code, got.Body.String())
			}
		}
	}
	if after := pgDump(t, d, orgA); after != beforeA {
		t.Fatalf("B's calls changed A's rows:\n%s\n%s", beforeA, after)
	}
	if after := pgDump(t, d, orgB); after != beforeB {
		t.Fatalf("the calls changed B's rows:\n%s\n%s", beforeB, after)
	}
}

// The privacy rule on the fleet routes: no label, group name,
// email, bearer, key or secret reaches a log line, an error answer, or a
// span (its name, attributes, status or events), on Postgres so the
// database spans are seen too. The canaries go through every route,
// refusals included.
func TestTheFleetRoutesLogAndTraceNoLabelNameEmailBearerOrKey(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	s, clock, log, _ := postgresFleetServer(t)
	const (
		label     = "fleet-label-canary-3e8f"
		renamed   = "fleet-label-canary-renamed-91b0"
		groupName = "group-name-canary-77aa"
		groupNew  = "group-name-canary-renamed-5d0c"
		email     = "email-canary-5c2d@acme.io"
	)
	actor := map[string]any{"id": actorA, "email": email}
	var bearers []string
	var errorAnswers []string
	send := func(method, target string, b body) *httptest.ResponseRecorder {
		got := sendWith(s, method, target, b)
		if got.Code >= 400 {
			errorAnswers = append(errorAnswers, got.Body.String())
		}
		var reply map[string]any
		if json.Unmarshal(got.Body.Bytes(), &reply) == nil {
			collectBearers(reply, &bearers)
		}
		return got
	}

	create := fleetBody(fleetA, label, "server").with("actor", actor)
	expectStatus(t, "create", send(http.MethodPost, fleetsTarget(orgA), create), http.StatusCreated)
	send(http.MethodPost, fleetsTarget(orgA), create)                                            // 200
	send(http.MethodPost, fleetsTarget(orgA), create.with("id", fleetA2))                        // 409 label taken
	send(http.MethodPost, fleetsTarget(orgA), create.with("label", " "+label))                   // 400
	send(http.MethodPost, fleetsTarget(orgC), create)                                            // 404
	send(http.MethodPost, fleetsTarget(orgA), create.with("id", fleetA2).with("label", renamed)) // 201
	send(http.MethodGet, fleetsTarget(orgA), nil)
	send(http.MethodGet, fleetTarget(orgA, fleetA), nil)
	send(http.MethodPatch, fleetTarget(orgA, fleetA), body{"label": renamed}) // 409
	send(http.MethodPatch, fleetTarget(orgA, fleetA2), body{"label": renamed + "-2"})
	send(http.MethodPost, groupsTarget(orgA, fleetA), body{"name": groupName})
	send(http.MethodPost, groupsTarget(orgA, fleetA), body{"name": groupName})        // 409
	send(http.MethodPost, groupsTarget(orgA, fleetA), body{"name": groupName + "\t"}) // 400
	g := groupID(t, s, orgA, fleetA, groupName)
	send(http.MethodPatch, groupTarget(orgA, fleetA, g), body{"name": groupNew})
	// Annotations are configuration text (they may hold paths): stored,
	// refused (not an object; over the bound; near it, where Postgres' own
	// check refuses), never logged or traced.
	mounts := map[string]any{"telflo": map[string]any{"dockerMounts": []any{
		map[string]any{"hostPath": "/annotation-canary-host", "containerPath": "/annotation-canary-container", "readOnly": true}}}}
	expectStatus(t, "annotations", send(http.MethodPatch, groupTarget(orgA, fleetA, g), body{"annotations": mounts}), http.StatusOK)
	send(http.MethodPost, groupsTarget(orgA, fleetA), body{"name": groupName + "-2", "annotations": []any{"annotation-canary-array"}})         // 400
	send(http.MethodPatch, groupTarget(orgA, fleetA, g), body{"annotations": map[string]any{"annotation-canary": strings.Repeat("v", 16384)}}) // 400
	nearBound := map[string]any{}
	for i := range 32 {
		nearBound[fmt.Sprintf("canary-%02d", i)] = strings.Repeat("v", 496)
	}
	if got := send(http.MethodPatch, groupTarget(orgA, fleetA, g), body{"annotations": nearBound}); got.Code != http.StatusBadRequest {
		t.Fatalf("annotations Postgres' check refuses: %d %s", got.Code, got.Body.String())
	}
	send(http.MethodGet, groupsTarget(orgA, fleetA), nil)
	send(http.MethodGet, groupTarget(orgA, fleetA, g), nil)
	send(http.MethodGet, tokensTarget(orgA, fleetA), nil)
	clock.Advance(time.Minute)
	send(http.MethodPost, rotateTarget(orgA, fleetA), rotationBody(nil, "2026-10-04T15:00:00.000Z").with("actor", actor))
	send(http.MethodPost, rotateTarget(orgA, fleetA), rotationBody(nil, "2026-10-01T15:00:00.000Z").with("actor", actor)) // 400
	rotate := tokenID(t, s, orgA, fleetA, "rotate")
	send(http.MethodPost, revealTarget(orgA, fleetA, rotate), body{"actor": actor})
	send(http.MethodPost, revokeTarget(orgA, fleetA, rotate), body{"actor": actor})
	send(http.MethodPost, revealTarget(orgA, fleetA, rotate), body{"actor": actor}) // 409
	send(http.MethodPost, revealTarget(orgA, fleetA, unknownID), body{"actor": actor})
	send(http.MethodPost, issueDefaultTarget(orgA, fleetA), body{"actor": actor}) // 409
	send(http.MethodPost, revokeTarget(orgA, fleetA, tokenID(t, s, orgA, fleetA, "default")), body{"actor": actor})
	expectStatus(t, "a new default token", send(http.MethodPost, issueDefaultTarget(orgA, fleetA), body{"actor": actor}), http.StatusCreated)
	send(http.MethodDelete, groupTarget(orgA, fleetA, g), nil)
	send(http.MethodDelete, fleetTarget(orgA, fleetA2), nil)
	if len(bearers) < 6 {
		t.Fatalf("the routes answered %d bearers, want the two creates' four, the rotation's, the reveal's and the new default token's", len(bearers))
	}

	markers := append([]string{label, renamed, groupName, groupNew, email, "canary", testEncryptionKey, testPreviousEncryptionKey, testSecret}, bearers...)
	logged := log.String()
	if !strings.Contains(logged, "/api/v1/organizations/{organizationId}/fleets/{fleetId}/tokens/rotate") {
		t.Fatalf("the log doesn't name the routes: %s", logged)
	}
	spans := recorder.Ended()
	if len(spans) < 20 {
		t.Fatalf("%d spans, want the server's and the database's", len(spans))
	}
	var traced strings.Builder
	for _, span := range spans {
		traced.WriteString(span.Name() + "\n" + span.Status().Description + "\n")
		for _, attribute := range span.Attributes() {
			traced.WriteString(string(attribute.Key) + "=" + attribute.Value.String() + "\n")
		}
		for _, event := range span.Events() {
			traced.WriteString(event.Name + "\n")
			for _, attribute := range event.Attributes {
				traced.WriteString(string(attribute.Key) + "=" + attribute.Value.String() + "\n")
			}
		}
	}
	for _, marker := range markers {
		if strings.Contains(logged, marker) {
			t.Errorf("the log holds %q", marker)
		}
		if strings.Contains(traced.String(), marker) {
			t.Errorf("a span holds %q", marker)
		}
		for _, answer := range errorAnswers {
			if strings.Contains(answer, marker) {
				t.Errorf("an error answer holds %q: %s", marker, answer)
			}
		}
	}
}

// collectBearers appends every "bearer" field's value in a reply.
func collectBearers(node any, bearers *[]string) {
	switch v := node.(type) {
	case map[string]any:
		for key, value := range v {
			if text, ok := value.(string); ok && key == "bearer" {
				*bearers = append(*bearers, text)
				continue
			}
			collectBearers(value, bearers)
		}
	case []any:
		for _, item := range v {
			collectBearers(item, bearers)
		}
	}
}
