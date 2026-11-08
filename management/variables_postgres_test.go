package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// On Postgres too: the routes answer as documented; a create is safe to
// retry; the in-use guards answer what uses a variable or a variable
// group; an override is set, replaced, left as it is, reset and set again;
// another organization's ids are 404 and change neither organization's
// rows.
func TestTheVariableRoutesOnPostgres(t *testing.T) {
	t.Run("the routes answer as documented", func(t *testing.T) {
		s, clock, _, _ := postgresFleetServer(t)
		assertTheVariableRoutesAnswerAsDocumented(t, s, clock)
	})
	t.Run("a create is safe to retry", func(t *testing.T) {
		s, clock, _, _ := postgresFleetServer(t)
		assertAVariablesCreateIsSafeToRetry(t, s, clock)
	})
	t.Run("a variable in use", func(t *testing.T) {
		s, _, _, _ := postgresFleetServer(t)
		assertADeleteOrRenameInUseAnswers409WithWhatUsesIt(t, s)
	})
	t.Run("a variable group in use", func(t *testing.T) {
		s, _, _, _ := postgresFleetServer(t)
		assertAVariableGroupInUseAnswers409WithItsGroupsAndPins(t, s)
	})
	t.Run("an override's life", func(t *testing.T) {
		s, clock, _, _ := postgresFleetServer(t)
		assertAnOverrideIsSetReplacedResetAndSetAgain(t, s, clock)
	})
	t.Run("another organization's ids", func(t *testing.T) {
		s, _, _, d := postgresFleetServer(t)
		assertAnotherOrganizationsVariablesAreNotFound(t, s, func(org string) string { return pgDump(t, d, org) })
	})
	t.Run("an assignment and a pin of undefined names", func(t *testing.T) {
		s, _, _, _ := postgresFleetServer(t)
		createFleet(t, s, orgA, fleetA, "edge-prod", "server")
		enrollCollector(t, s, orgA, fleetA, "edge-01")
		seedPublication(orgA, publicationA, referencing("TOKEN_HEADER", "API_KEY"))(t, s)
		want := variablesUndefinedDetails{Names: []string{"API_KEY", "TOKEN_HEADER"}, TotalNames: 2}
		expectDetails(t, "the assignment", sendWith(s, http.MethodPost, groupsTarget(orgA, fleetA),
			body{"name": "g", "assignedPublicationId": publicationA}), http.StatusUnprocessableEntity, codeVariablesUndefined,
			messageVariablesUndefined, want)
		edge01 := collectorID(t, s, orgA, fleetA, "edge-01")
		expectDetails(t, "the bulk pin", sendWith(s, http.MethodPost, collectorsPinTarget(orgA, fleetA),
			body{"collectorIds": []string{edge01}, "pin": map[string]any{"kind": "publication", "id": publicationA}}),
			http.StatusUnprocessableEntity, codeVariablesUndefined, messageVariablesUndefined, want)
		expectError(t, "an unknown variable group", sendWith(s, http.MethodPatch, collectorTarget(orgA, fleetA, edge01),
			body{"pinnedVariableGroupId": unknownID}), http.StatusUnprocessableEntity, codeVariableGroupNotFound, messageVariableGroupNotFound)
	})
}

// On Postgres so the database spans are seen too: a variable's name, value
// and description, a variable group's name and description, an override's
// value and an actor's email reach no log line and no span, through every
// variables route and every refusal; they are answered by the variables
// routes' replies only, and a guard's details (the undefined names among
// them) by its error answer only.
func TestTheVariableRoutesLogAndTraceNoNameValueDescriptionOrEmail(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	s, clock, log, _ := postgresFleetServer(t)
	const (
		name             = "name_canary_5e1d"
		renamed          = "renamed_canary_1a2b"
		value            = "value-canary-2b9f $x"
		description      = "description-canary-7c3a"
		groupName        = "group-name-canary-4d6e"
		groupDescription = "group-description-canary-9a1b"
		overrideValue    = "override-canary-3f8c"
		email            = "email-canary@acme.io"
		undefinedName    = "undefined_canary_8e4f"
		refusedValue     = "refused-canary-0c5d"
	)
	actor := map[string]any{"id": sampleActorID, "email": email}
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	enrollCollector(t, s, orgA, fleetA, "edge-01")
	edge01 := collectorID(t, s, orgA, fleetA, "edge-01")

	type answer struct {
		route string
		got   *httptest.ResponseRecorder
	}
	var answers []answer
	send := func(route, method, target string, b body) {
		answers = append(answers, answer{route, sendWith(s, method, target, b)})
	}
	clock.Advance(time.Minute)
	send("create variable", http.MethodPost, variablesTarget(orgA),
		body{"id": variableA, "name": name, "value": value, "description": description, "actor": actor})
	send("create variable", http.MethodPost, variablesTarget(orgA),
		body{"id": variableA, "name": name, "value": value, "description": description, "actor": actor}) // 200
	send("create variable", http.MethodPost, variablesTarget(orgA),
		body{"id": variableA, "name": name, "value": refusedValue, "description": nil, "actor": actor}) // 409 conflict
	send("create variable", http.MethodPost, variablesTarget(orgA),
		body{"name": name, "value": refusedValue, "description": nil, "actor": actor}) // 409 name taken
	send("create variable", http.MethodPost, variablesTarget(orgA),
		body{"name": name + "-", "value": refusedValue + "\n", "description": " " + refusedValue, "actor": actor}) // 400
	send("list variables", http.MethodGet, variablesTarget(orgA), nil)
	send("create variable group", http.MethodPost, variableGroupsTarget(orgA),
		body{"id": variableGroupA, "name": groupName, "description": groupDescription, "actor": actor})
	send("create variable group", http.MethodPost, variableGroupsTarget(orgA),
		body{"name": groupName, "description": nil, "actor": actor}) // 409
	send("set override", http.MethodPut, overrideTarget(orgA, variableGroupA, variableA), body{"value": overrideValue, "actor": actor})
	send("set override", http.MethodPut, overrideTarget(orgA, variableGroupA, variableA), body{"value": refusedValue + "\r", "actor": actor}) // 400
	send("get variable group", http.MethodGet, variableGroupTarget(orgA, variableGroupA), nil)
	send("list variable groups", http.MethodGet, variableGroupsTarget(orgA), nil)
	send("patch variable group", http.MethodPatch, variableGroupTarget(orgA, variableGroupA), body{"description": nil, "actor": actor})
	// In use: a published version referencing the name, assigned to a
	// group; the group bound to the variable group; a collector pinned.
	seedPublication(orgA, publicationA, referencing(name))(t, s)
	send("create group", http.MethodPost, groupsTarget(orgA, fleetA),
		body{"name": "gateways", "assignedPublicationId": publicationA, "variableGroupId": variableGroupA})
	send("patch collector", http.MethodPatch, collectorTarget(orgA, fleetA, edge01), body{"pinnedVariableGroupId": variableGroupA})
	send("pin collectors", http.MethodPost, collectorsPinTarget(orgA, fleetA),
		body{"collectorIds": []string{edge01}, "pinnedVariableGroupId": variableGroupA})
	send("patch variable", http.MethodPatch, variableTarget(orgA, variableA), body{"name": renamed, "actor": actor}) // 409 in use
	send("delete variable", http.MethodDelete, variableTarget(orgA, variableA), nil)                                 // 409 in use
	send("delete variable group", http.MethodDelete, variableGroupTarget(orgA, variableGroupA), nil)                 // 409 in use
	send("patch variable", http.MethodPatch, variableTarget(orgA, variableA), body{"value": value + " changed", "actor": actor})
	// Undefined: a published version referencing a name no variable has.
	seedPublication(orgA, publicationA2, referencing(undefinedName))(t, s)
	send("patch collector", http.MethodPatch, collectorTarget(orgA, fleetA, edge01),
		body{"pin": map[string]any{"kind": "publication", "id": publicationA2}}) // 422
	send("pin collectors", http.MethodPost, collectorsPinTarget(orgA, fleetA),
		body{"collectorIds": []string{edge01}, "pin": map[string]any{"kind": "publication", "id": publicationA2}}) // 422
	send("create group", http.MethodPost, groupsTarget(orgA, fleetA), body{"name": "edges", "assignedPublicationId": publicationA2}) // 422
	// Out of use, then renamed, reset and deleted.
	send("patch group", http.MethodPatch, groupTarget(orgA, fleetA, groupID(t, s, orgA, fleetA, "gateways")),
		body{"assignedPublicationId": nil, "variableGroupId": nil})
	send("pin collectors", http.MethodPost, collectorsPinTarget(orgA, fleetA), body{"collectorIds": []string{edge01}, "pinnedVariableGroupId": nil})
	send("patch variable", http.MethodPatch, variableTarget(orgA, variableA), body{"name": renamed, "description": nil, "actor": actor})
	send("reset override", http.MethodDelete, overrideTarget(orgA, variableGroupA, variableA), body{"actor": actor})
	send("get variable group", http.MethodGet, variableGroupTarget(orgA, variableGroupA), nil) // the reset's actor
	send("delete variable group", http.MethodDelete, variableGroupTarget(orgA, variableGroupA), nil)
	send("delete variable", http.MethodDelete, variableTarget(orgA, variableA), nil)
	send("list variables", http.MethodGet, variablesTarget(orgA), nil)

	statuses := map[int]int{}
	for _, a := range answers {
		statuses[a.got.Code]++
	}
	if statuses[http.StatusConflict] < 6 || statuses[http.StatusBadRequest] < 2 || statuses[http.StatusUnprocessableEntity] != 3 ||
		statuses[http.StatusNoContent] != 3 {
		t.Fatalf("the answers' statuses are %v: the test doesn't reach every refusal", statuses)
	}

	// Where each canary may be answered, and nowhere else.
	variableRoutes := []string{"create variable", "list variables", "patch variable"}
	groupRoutes := []string{"create variable group", "get variable group", "list variable groups", "patch variable group"}
	allowed := map[string][]string{
		name:             variableRoutes,
		renamed:          variableRoutes,
		value:            variableRoutes,
		description:      variableRoutes,
		groupName:        groupRoutes,
		groupDescription: groupRoutes,
		overrideValue:    {"set override", "get variable group"},
		email:            append(append([]string{"set override"}, variableRoutes...), groupRoutes...),
	}
	canaries := []string{name, renamed, value, description, groupName, groupDescription, overrideValue, email, undefinedName, refusedValue}
	for _, a := range answers {
		if a.got.Code >= 400 {
			for _, canary := range canaries {
				// The undefined names are answered in variables_undefined's
				// details, and only there.
				if canary == undefinedName && strings.Contains(a.got.Body.String(), `"code":"variables_undefined"`) {
					continue
				}
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
		for _, canary := range []string{undefinedName, refusedValue} {
			if strings.Contains(a.got.Body.String(), canary) {
				t.Errorf("%s's answer holds %q", a.route, canary)
			}
		}
	}
	// Each canary was answered where it belongs: the test sees them.
	seen := map[string]bool{}
	for _, a := range answers {
		for _, canary := range canaries {
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
	if !seen[undefinedName] {
		t.Error("no variables_undefined answer names the undefined name: the test doesn't reach it")
	}

	logged := log.String()
	for _, pattern := range []string{variablesPath, variablePath, variableGroupsPath, variableGroupPath, overridePath} {
		if !strings.Contains(logged, strings.TrimPrefix(pattern, "/")) {
			t.Fatalf("the log doesn't name the route %s: %s", pattern, logged)
		}
	}
	spans := recorder.Ended()
	if len(spans) < 40 {
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
	for _, canary := range append(canaries, "canary", testSecret) {
		if strings.Contains(logged, canary) {
			t.Errorf("the log holds %q", canary)
		}
		if strings.Contains(traced.String(), canary) {
			t.Errorf("a span holds %q", canary)
		}
	}
}
