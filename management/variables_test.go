package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/telflo-opamp-server/store/fleets"
	"github.com/telflo/telflo-opamp-server/store/organizations"
	"github.com/telflo/telflo-opamp-server/store/orgvariables"
	"github.com/telflo/telflo-opamp-server/store/publications"
	"github.com/telflo/telflo-opamp-server/store/variables"
)

// The variables routes' fixture ids: A's are organization A's.
const (
	variableA       = "7a7a7a7a-0000-4000-8000-00000000007a"
	variableA2      = "7a7a7a7a-0000-4000-8000-0000000007a2"
	variableGroupA  = "8a8a8a8a-0000-4000-8000-00000000008a"
	variableGroupA2 = "8a8a8a8a-0000-4000-8000-0000000008a2"
	publicationA2   = "1a1a1a1a-0000-4000-8000-0000000001a2"
)

func variablesTarget(organizationID string) string {
	return organizationTarget(organizationID) + "/variables"
}

func variableTarget(organizationID, variableID string) string {
	return variablesTarget(organizationID) + "/" + variableID
}

func variableGroupsTarget(organizationID string) string {
	return organizationTarget(organizationID) + "/variable-groups"
}

func variableGroupTarget(organizationID, variableGroupID string) string {
	return variableGroupsTarget(organizationID) + "/" + variableGroupID
}

func overrideTarget(organizationID, variableGroupID, variableID string) string {
	return variableGroupTarget(organizationID, variableGroupID) + "/overrides/" + variableID
}

// variableBody is a variable create's body, with no description and Ana as
// the actor; id "" leaves the id out.
func variableBody(id, name, value string) body {
	b := body{"name": name, "value": value, "description": nil, "actor": actorJSONBody()}
	if id != "" {
		b["id"] = id
	}
	return b
}

// variableGroupBody is a variable group create's body, with no description
// and Ana as the actor.
func variableGroupBody(id, name string) body {
	b := body{"name": name, "description": nil, "actor": actorJSONBody()}
	if id != "" {
		b["id"] = id
	}
	return b
}

func seedVariable(organizationID, id, name, value string) func(t *testing.T, s *Server) {
	return func(t *testing.T, s *Server) {
		t.Helper()
		expectStatus(t, "creating a variable", sendWith(s, http.MethodPost, variablesTarget(organizationID), variableBody(id, name, value)),
			http.StatusCreated)
	}
}

// seedVariables creates n variables named V0000 on.
func seedVariables(organizationID string, n int) func(t *testing.T, s *Server) {
	return func(t *testing.T, s *Server) {
		t.Helper()
		for i := range n {
			seedVariable(organizationID, "", fmt.Sprintf("V%04d", i), "x")(t, s)
		}
	}
}

func seedVariableGroup(organizationID, id, name string) func(t *testing.T, s *Server) {
	return func(t *testing.T, s *Server) {
		t.Helper()
		expectStatus(t, "creating a variable group",
			sendWith(s, http.MethodPost, variableGroupsTarget(organizationID), variableGroupBody(id, name)), http.StatusCreated)
	}
}

// seedVariableGroups creates n variable groups named group 000 on.
func seedVariableGroups(organizationID string, n int) func(t *testing.T, s *Server) {
	return func(t *testing.T, s *Server) {
		t.Helper()
		for i := range n {
			seedVariableGroup(organizationID, "", fmt.Sprintf("group %03d", i))(t, s)
		}
	}
}

func seedOverride(organizationID, variableGroupID, variableID, value string) func(t *testing.T, s *Server) {
	return func(t *testing.T, s *Server) {
		t.Helper()
		expectStatus(t, "setting an override", sendWith(s, http.MethodPut, overrideTarget(organizationID, variableGroupID, variableID),
			body{"value": value, "actor": actorJSONBody()}), http.StatusCreated)
	}
}

// seedBoundGroup creates the fleet's group name, bound to the variable
// group variableGroupID.
func seedBoundGroup(organizationID, fleetID, name, variableGroupID string) func(t *testing.T, s *Server) {
	return func(t *testing.T, s *Server) {
		t.Helper()
		expectStatus(t, "creating a bound group", sendWith(s, http.MethodPost, groupsTarget(organizationID, fleetID),
			body{"name": name, "variableGroupId": variableGroupID}), http.StatusCreated)
	}
}

// seedPinnedVariableGroup pins the variable group on the fleet's collector
// named name.
func seedPinnedVariableGroup(organizationID, fleetID, name, variableGroupID string) func(t *testing.T, s *Server) {
	return func(t *testing.T, s *Server) {
		t.Helper()
		expectStatus(t, "pinning a variable group", sendWith(s, http.MethodPatch,
			collectorTarget(organizationID, fleetID, collectorID(t, s, organizationID, fleetID, name)),
			body{"pinnedVariableGroupId": variableGroupID}), http.StatusOK)
	}
}

// detailsReply is an error answer with its details decoded as T.
type detailsReply[T any] struct {
	Error struct {
		Code    errorCode `json:"code"`
		Message string    `json:"message"`
		Details T         `json:"details"`
	} `json:"error"`
}

// expectDetails checks an error answer with details: its status, code,
// exact message, and its details, decoded as T, equal to want.
func expectDetails[T any](t *testing.T, label string, got *httptest.ResponseRecorder, status int, code errorCode, message string, want T) {
	t.Helper()
	var reply detailsReply[T]
	decodeInto(t, got.Body.Bytes(), &reply)
	if got.Code != status || reply.Error.Code != code || reply.Error.Message != message {
		t.Fatalf("%s: %d %s, want %d %s %q", label, got.Code, got.Body.String(), status, code, message)
	}
	gotJSON, _ := json.Marshal(reply.Error.Details)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("%s: the details are\n%s\nwant\n%s", label, gotJSON, wantJSON)
	}
}

// sortedIDs is ids in byte order, as the details list them.
func sortedIDs(ids ...string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}

func refsOf(fleetID string, groupIDs ...string) []groupRefReply {
	refs := []groupRefReply{}
	for _, id := range sortedIDs(groupIDs...) {
		refs = append(refs, groupRefReply{FleetID: fleetID, GroupID: id})
	}
	return refs
}

// The whole life of a variable, a variable group and an override through
// the routes, with each reply as documented: the fields, the actor and the
// server's clock, the lists' order, and each refusal's code and message.
func TestTheVariableRoutesAnswerAsDocumented(t *testing.T) {
	s, clock, _ := fleetServer(t)
	assertTheVariableRoutesAnswerAsDocumented(t, s, clock)
}

func assertTheVariableRoutesAnswerAsDocumented(t *testing.T, s *Server, clock *testClock) {
	t.Helper()
	at := formatTime(clock.Now())
	ana := &actorReply{ID: sampleActorID, Email: sampleEmail}

	// Every reply is written with HTML escaping off: a value's, a
	// description's or an override's <, > and & take one byte each, which
	// the platform's reply caps for the list and a group's read assume.
	unescaped := func(label string, got *httptest.ResponseRecorder, want ...string) {
		t.Helper()
		for _, text := range want {
			if !strings.Contains(got.Body.String(), text) {
				t.Fatalf("%s's reply doesn't hold %s as written (HTML escaped?): %s", label, text, got.Body.String())
			}
		}
	}
	var v variableReply
	got := sendWith(s, http.MethodPost, variablesTarget(orgA), variableBody(variableA, "REGION", "eu-west-1 $HOME <&>").with("description", "Where it runs <&>"))
	expectStatus(t, "create", got, http.StatusCreated)
	unescaped("the create", got, `"value":"eu-west-1 $HOME <&>"`, `"description":"Where it runs <&>"`)
	decodeInto(t, got.Body.Bytes(), &v)
	description := "Where it runs <&>"
	want := variableReply{ID: variableA, Name: "REGION", Value: "eu-west-1 $HOME <&>", Description: &description, ChangedBy: ana, ChangedAt: at}
	if !sameJSON(v, want) {
		t.Fatalf("created %+v, want %+v", v, want)
	}
	if keys := replyKeys(t, got); !slices.Equal(keys, []string{"changedAt", "changedBy", "description", "id", "name", "value"}) {
		t.Fatalf("a variable's fields are %v", keys)
	}
	seedVariable(orgA, variableA2, "API_KEY_HEADER", "x-api-key")(t, s)

	// The list is by name, in byte order.
	var list variableListReply
	listed := call(s, http.MethodGet, variablesTarget(orgA), nil, "")
	unescaped("the list", listed, `"value":"eu-west-1 $HOME <&>"`, `"description":"Where it runs <&>"`)
	decodeInto(t, listed.Body.Bytes(), &list)
	if len(list.Variables) != 2 || list.Variables[0].Name != "API_KEY_HEADER" || list.Variables[1].Name != "REGION" {
		t.Fatalf("the list is %+v", list)
	}

	// A PATCH moves changedBy and changedAt; null removes the description.
	clock.Advance(time.Minute)
	got = sendWith(s, http.MethodPatch, variableTarget(orgA, variableA), body{"name": "AWS_REGION", "description": nil, "actor": nil})
	expectStatus(t, "patch", got, http.StatusOK)
	decodeInto(t, got.Body.Bytes(), &v)
	want = variableReply{ID: variableA, Name: "AWS_REGION", Value: "eu-west-1 $HOME <&>", ChangedAt: formatTime(clock.Now())}
	if !sameJSON(v, want) {
		t.Fatalf("patched %+v, want %+v", v, want)
	}
	expectError(t, "patch, the name taken", sendWith(s, http.MethodPatch, variableTarget(orgA, variableA), body{"name": "API_KEY_HEADER", "actor": nil}),
		http.StatusConflict, codeVariableNameTaken, messageVariableNameTaken)
	expectError(t, "create, the name taken", sendWith(s, http.MethodPost, variablesTarget(orgA), variableBody("", "AWS_REGION", "x")),
		http.StatusConflict, codeVariableNameTaken, messageVariableNameTaken)
	expectError(t, "patch, nothing", sendWith(s, http.MethodPatch, variableTarget(orgA, variableA), body{"actor": nil}),
		http.StatusBadRequest, codeInvalidRequest, messageNoVariableChange)
	expectError(t, "patch, unknown", sendWith(s, http.MethodPatch, variableTarget(orgA, unknownID), body{"value": "x", "actor": nil}),
		http.StatusNotFound, codeVariableNotFound, messageVariableNotFound)

	// A variable group, its override, and the group's count and changes.
	got = sendWith(s, http.MethodPost, variableGroupsTarget(orgA), variableGroupBody(variableGroupA, "production"))
	expectStatus(t, "create a group", got, http.StatusCreated)
	var g variableGroupReply
	decodeInto(t, got.Body.Bytes(), &g)
	wantGroup := variableGroupReply{ID: variableGroupA, Name: "production", ChangedBy: ana, ChangedAt: formatTime(clock.Now())}
	if !sameJSON(g, wantGroup) {
		t.Fatalf("created %+v, want %+v", g, wantGroup)
	}
	clock.Advance(time.Minute)
	got = sendWith(s, http.MethodPut, overrideTarget(orgA, variableGroupA, variableA), body{"value": "eu-central-1 <&>", "actor": actorJSONBody()})
	expectStatus(t, "set an override", got, http.StatusCreated)
	unescaped("the override's PUT", got, `"value":"eu-central-1 <&>"`)
	var o overrideReply
	decodeInto(t, got.Body.Bytes(), &o)
	wantOverride := overrideReply{VariableID: variableA, Value: "eu-central-1 <&>", ChangedBy: ana, ChangedAt: formatTime(clock.Now())}
	if !sameJSON(o, wantOverride) {
		t.Fatalf("the override is %+v, want %+v", o, wantOverride)
	}
	var detail variableGroupDetailReply
	read := call(s, http.MethodGet, variableGroupTarget(orgA, variableGroupA), nil, "")
	unescaped("the group's read", read, `"value":"eu-central-1 <&>"`)
	decodeInto(t, read.Body.Bytes(), &detail)
	wantGroup.OverrideCount, wantGroup.ChangedAt = 1, formatTime(clock.Now())
	if !sameJSON(detail, variableGroupDetailReply{VariableGroup: wantGroup, Overrides: []overrideReply{wantOverride}}) {
		t.Fatalf("the group's read is %+v", detail)
	}
	seedVariableGroup(orgA, variableGroupA2, "staging")(t, s)
	var groups variableGroupListReply
	decodeInto(t, call(s, http.MethodGet, variableGroupsTarget(orgA), nil, "").Body.Bytes(), &groups)
	if len(groups.VariableGroups) != 2 || groups.VariableGroups[0].Name != "production" || groups.VariableGroups[0].OverrideCount != 1 ||
		groups.VariableGroups[1].Name != "staging" {
		t.Fatalf("the groups are %+v", groups)
	}
	got = sendWith(s, http.MethodPatch, variableGroupTarget(orgA, variableGroupA), body{"description": "Live", "actor": nil})
	expectStatus(t, "patch a group", got, http.StatusOK)
	decodeInto(t, got.Body.Bytes(), &g)
	if g.Description == nil || *g.Description != "Live" || g.ChangedBy != nil {
		t.Fatalf("patched %+v", g)
	}
	expectError(t, "patch a group, the name taken", sendWith(s, http.MethodPatch, variableGroupTarget(orgA, variableGroupA),
		body{"name": "staging", "actor": nil}), http.StatusConflict, codeVariableGroupNameTaken, messageVariableGroupNameTaken)
	expectError(t, "patch a group, nothing", sendWith(s, http.MethodPatch, variableGroupTarget(orgA, variableGroupA), body{"actor": nil}),
		http.StatusBadRequest, codeInvalidRequest, messageNoVariableGroupChange)
	expectError(t, "get a group, unknown", call(s, http.MethodGet, variableGroupTarget(orgA, unknownID), nil, ""),
		http.StatusNotFound, codeVariableGroupNotFound, messageVariableGroupNotFound)
	expectError(t, "an override of an unknown variable", sendWith(s, http.MethodPut, overrideTarget(orgA, variableGroupA, unknownID),
		body{"value": "x", "actor": nil}), http.StatusNotFound, codeVariableNotFound, messageVariableNotFound)
	expectError(t, "an override in an unknown group", sendWith(s, http.MethodPut, overrideTarget(orgA, unknownID, variableA),
		body{"value": "x", "actor": nil}), http.StatusNotFound, codeVariableGroupNotFound, messageVariableGroupNotFound)
	expectError(t, "a reset in an unknown group", sendWith(s, http.MethodDelete, overrideTarget(orgA, unknownID, variableA), body{"actor": nil}),
		http.StatusNotFound, codeVariableGroupNotFound, messageVariableGroupNotFound)

	// Deletes: the variable takes its overrides with it.
	expectStatus(t, "delete the variable", call(s, http.MethodDelete, variableTarget(orgA, variableA), nil, ""), http.StatusNoContent)
	decodeInto(t, call(s, http.MethodGet, variableGroupTarget(orgA, variableGroupA), nil, "").Body.Bytes(), &detail)
	if detail.VariableGroup.OverrideCount != 0 || len(detail.Overrides) != 0 {
		t.Fatalf("the deleted variable's override is still there: %+v", detail)
	}
	expectError(t, "delete it again", call(s, http.MethodDelete, variableTarget(orgA, variableA), nil, ""),
		http.StatusNotFound, codeVariableNotFound, messageVariableNotFound)
	expectStatus(t, "delete the group", call(s, http.MethodDelete, variableGroupTarget(orgA, variableGroupA), nil, ""), http.StatusNoContent)
	expectError(t, "delete the group again", call(s, http.MethodDelete, variableGroupTarget(orgA, variableGroupA), nil, ""),
		http.StatusNotFound, codeVariableGroupNotFound, messageVariableGroupNotFound)
}

// sameJSON reports whether a and b encode to the same JSON.
func sameJSON(a, b any) bool {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

// replyKeys is a reply object's keys, sorted.
func replyKeys(t *testing.T, got *httptest.ResponseRecorder) []string {
	t.Helper()
	var decoded map[string]json.RawMessage
	decodeInto(t, got.Body.Bytes(), &decoded)
	keys := make([]string, 0, len(decoded))
	for key := range decoded {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// A create with the caller's id is safe to retry (200 with the same
// variable, nothing moved); the same id with anything else is a conflict
// saying nothing; another organization's id is a conflict only when the
// name is free. The same for a variable group.
func TestAVariablesCreateIsSafeToRetry(t *testing.T) {
	s, clock, _ := fleetServer(t)
	assertAVariablesCreateIsSafeToRetry(t, s, clock)
}

func assertAVariablesCreateIsSafeToRetry(t *testing.T, s *Server, clock *testClock) {
	t.Helper()
	first := sendWith(s, http.MethodPost, variablesTarget(orgA), variableBody(variableA, "REGION", "eu-west-1"))
	expectStatus(t, "create", first, http.StatusCreated)
	clock.Advance(time.Minute)
	again := sendWith(s, http.MethodPost, variablesTarget(orgA), variableBody(variableA, "REGION", "eu-west-1").with("actor", nil))
	expectStatus(t, "the retry", again, http.StatusOK)
	if again.Body.String() != first.Body.String() {
		t.Fatalf("the retry answered %s, want the stored %s", again.Body.String(), first.Body.String())
	}
	for label, b := range map[string]body{
		"another value":       variableBody(variableA, "REGION", "us-east-1"),
		"another name":        variableBody(variableA, "AWS_REGION", "eu-west-1"),
		"another description": variableBody(variableA, "REGION", "eu-west-1").with("description", "x"),
	} {
		got := sendWith(s, http.MethodPost, variablesTarget(orgA), b)
		expectError(t, label, got, http.StatusConflict, codeVariableConflict, messageVariableConflict)
	}
	// Another organization's id: a conflict while the name is free there,
	// the name taken first otherwise.
	expectError(t, "B with A's id", sendWith(s, http.MethodPost, variablesTarget(orgB), variableBody(variableA, "REGION", "eu-west-1")),
		http.StatusConflict, codeVariableConflict, messageVariableConflict)
	seedVariable(orgB, "", "REGION", "x")(t, s)
	expectError(t, "B with A's id and a name B has", sendWith(s, http.MethodPost, variablesTarget(orgB), variableBody(variableA, "REGION", "y")),
		http.StatusConflict, codeVariableNameTaken, messageVariableNameTaken)

	firstGroup := sendWith(s, http.MethodPost, variableGroupsTarget(orgA), variableGroupBody(variableGroupA, "production"))
	expectStatus(t, "create a group", firstGroup, http.StatusCreated)
	clock.Advance(time.Minute)
	againGroup := sendWith(s, http.MethodPost, variableGroupsTarget(orgA), variableGroupBody(variableGroupA, "production"))
	expectStatus(t, "the group's retry", againGroup, http.StatusOK)
	if againGroup.Body.String() != firstGroup.Body.String() {
		t.Fatalf("the group's retry answered %s, want %s", againGroup.Body.String(), firstGroup.Body.String())
	}
	expectError(t, "a group, another name", sendWith(s, http.MethodPost, variableGroupsTarget(orgA), variableGroupBody(variableGroupA, "staging")),
		http.StatusConflict, codeVariableGroupConflict, messageVariableGroupConflict)
	expectError(t, "B with A's group id", sendWith(s, http.MethodPost, variableGroupsTarget(orgB), variableGroupBody(variableGroupA, "production")),
		http.StatusConflict, codeVariableGroupConflict, messageVariableGroupConflict)
	expectError(t, "a group, the name taken", sendWith(s, http.MethodPost, variableGroupsTarget(orgA), variableGroupBody("", "production")),
		http.StatusConflict, codeVariableGroupNameTaken, messageVariableGroupNameTaken)
}

// Every refused field is a 400 naming the field, and the answer never
// quotes what was sent: the canary in each is nowhere in it.
func TestEveryVariableFieldIsRefusedByNameNeverByValue(t *testing.T) {
	s, _, log := fleetServer(t)
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	enrollCollector(t, s, orgA, fleetA, "edge-01")
	seedVariable(orgA, variableA, "REGION", "x")(t, s)
	seedVariableGroup(orgA, variableGroupA, "production")(t, s)
	edge01 := collectorTarget(orgA, fleetA, collectorID(t, s, orgA, fleetA, "edge-01"))
	const canary = "canary"
	long := func(n int) string { return canary + strings.Repeat("c", n-len(canary)) }
	cases := []struct {
		label, method, target string
		b                     body
		message               string
	}{
		{"a name with a hyphen", http.MethodPost, variablesTarget(orgA), variableBody("", "NAME-"+canary, "x"), messageVariableName},
		{"a name starting with a digit", http.MethodPost, variablesTarget(orgA), variableBody("", "9"+canary, "x"), messageVariableName},
		{"a name of 129 characters", http.MethodPost, variablesTarget(orgA), variableBody("", long(129), "x"), messageVariableName},
		{"a name that isn't a string", http.MethodPost, variablesTarget(orgA), variableBody("", "x", "x").with("name", 7), messageVariableName},
		{"a value with a line feed", http.MethodPost, variablesTarget(orgA), variableBody("", "N", canary+"\n"), messageVariableValue},
		{"a value with a carriage return", http.MethodPost, variablesTarget(orgA), variableBody("", "N", canary+"\r"), messageVariableValue},
		{"a value with a tab", http.MethodPost, variablesTarget(orgA), variableBody("", "N", canary+"\t"), messageVariableValue},
		{"a value with DEL", http.MethodPost, variablesTarget(orgA), variableBody("", "N", canary+string(rune(0x7f))), messageVariableValue},
		{"a value with NEL", http.MethodPost, variablesTarget(orgA), variableBody("", "N", canary+string(rune(0x85))), messageVariableValue},
		{"a value with U+2028", http.MethodPost, variablesTarget(orgA), variableBody("", "N", canary+string(rune(0x2028))), messageVariableValue},
		{"a value with U+2029", http.MethodPost, variablesTarget(orgA), variableBody("", "N", canary+string(rune(0x2029))), messageVariableValue},
		{"a value with U+0000", http.MethodPost, variablesTarget(orgA), variableBody("", "N", canary+string(rune(0))), messageVariableValue},
		{"a value of 1025 characters", http.MethodPost, variablesTarget(orgA), variableBody("", "N", long(1025)), messageVariableValue},
		{"a null value", http.MethodPost, variablesTarget(orgA), variableBody("", "N", "x").with("value", nil), messageVariableValue},
		{"a description with a trailing space", http.MethodPost, variablesTarget(orgA),
			variableBody("", "N", "x").with("description", canary+" "), messageVariableDescription},
		{"an empty description", http.MethodPost, variablesTarget(orgA), variableBody("", "N", "x").with("description", ""), messageVariableDescription},
		{"a description of 1001 characters", http.MethodPost, variablesTarget(orgA),
			variableBody("", "N", "x").with("description", long(1001)), messageVariableDescription},
		{"a description with a line feed", http.MethodPost, variablesTarget(orgA),
			variableBody("", "N", "x").with("description", canary+"\nx"), messageVariableDescription},
		{"a rename with a space", http.MethodPatch, variableTarget(orgA, variableA), body{"name": "A " + canary, "actor": nil}, messageVariableName},
		{"a new value with a line feed", http.MethodPatch, variableTarget(orgA, variableA), body{"value": canary + "\n", "actor": nil}, messageVariableValue},
		{"a new description with a tab", http.MethodPatch, variableTarget(orgA, variableA),
			body{"description": "\t" + canary, "actor": nil}, messageVariableDescription},
		{"a group name with a leading space", http.MethodPost, variableGroupsTarget(orgA), variableGroupBody("", " "+canary), messageVariableGroupName},
		{"a group name of 201 characters", http.MethodPost, variableGroupsTarget(orgA), variableGroupBody("", long(201)), messageVariableGroupName},
		{"an empty group name", http.MethodPost, variableGroupsTarget(orgA), variableGroupBody("", ""), messageVariableGroupName},
		{"a group description with a control character", http.MethodPost, variableGroupsTarget(orgA),
			variableGroupBody("", "g").with("description", canary+string(rune(0x1b))), messageVariableDescription},
		{"a group's new name with a line feed", http.MethodPatch, variableGroupTarget(orgA, variableGroupA),
			body{"name": canary + "\n", "actor": nil}, messageVariableGroupName},
		{"an override with a line feed", http.MethodPut, overrideTarget(orgA, variableGroupA, variableA),
			body{"value": canary + "\n", "actor": nil}, messageVariableValue},
		{"an override of 1025 characters", http.MethodPut, overrideTarget(orgA, variableGroupA, variableA),
			body{"value": long(1025), "actor": nil}, messageVariableValue},
		{"a group's variableGroupId that isn't a UUID", http.MethodPost, groupsTarget(orgA, fleetA),
			body{"name": "g", "variableGroupId": canary}, messageVariableGroupID},
		{"a pinnedVariableGroupId that isn't a UUID", http.MethodPatch, edge01, body{"pinnedVariableGroupId": canary}, messagePinnedVariableGroupID},
		{"a bulk pin's pinnedVariableGroupId that isn't a UUID", http.MethodPost, collectorsPinTarget(orgA, fleetA),
			body{"collectorIds": []string{unknownID}, "pinnedVariableGroupId": canary}, messagePinnedVariableGroupID},
	}
	for _, c := range cases {
		got := sendWith(s, c.method, c.target, c.b)
		expectError(t, c.label, got, http.StatusBadRequest, codeInvalidRequest, c.message)
		if strings.Contains(got.Body.String(), canary) {
			t.Errorf("%s: the answer quotes what was sent: %s", c.label, got.Body.String())
		}
	}
	if strings.Contains(log.String(), canary) {
		t.Fatalf("the log holds what was sent: %s", log.String())
	}
	// Nothing was stored.
	var list variableListReply
	decodeInto(t, call(s, http.MethodGet, variablesTarget(orgA), nil, "").Body.Bytes(), &list)
	if len(list.Variables) != 1 || list.Variables[0].Value != "x" {
		t.Fatalf("a refused request stored something: %+v", list)
	}
}

// inUseFixture is a variable REGION in use: publicationA references it and
// is assigned to two groups of fleet A, and publicationA2 references it
// and is pinned on one collector. It answers the groups' ids.
func inUseFixture(t *testing.T, s *Server) (gateways, edges string) {
	t.Helper()
	seedVariable(orgA, variableA, "REGION", "eu-west-1")(t, s)
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	seedPublication(orgA, publicationA, referencing("REGION"))(t, s)
	seedPublication(orgA, publicationA2, referencing("REGION", "REGION"))(t, s)
	seedAssignedGroup(orgA, fleetA, "gateways", publicationA)(t, s)
	seedAssignedGroup(orgA, fleetA, "edges", publicationA)(t, s)
	enrollCollector(t, s, orgA, fleetA, "edge-01")
	seedPin(orgA, fleetA, "edge-01", publicationA2)(t, s)
	return groupID(t, s, orgA, fleetA, "gateways"), groupID(t, s, orgA, fleetA, "edges")
}

// A variable's rename or delete is refused while a published version that
// a group or a pin uses references its name, answering what uses it (ids
// and counts only); a value change is never refused; once nothing uses
// it, the delete goes through.
func TestADeleteOrRenameInUseAnswers409WithWhatUsesIt(t *testing.T) {
	s, _, _ := fleetServer(t)
	assertADeleteOrRenameInUseAnswers409WithWhatUsesIt(t, s)
}

func assertADeleteOrRenameInUseAnswers409WithWhatUsesIt(t *testing.T, s *Server) {
	t.Helper()
	gateways, edges := inUseFixture(t, s)
	want := variableInUseDetails{
		UsedBy: []variableUseReply{
			{PublicationID: publicationA, Groups: refsOf(fleetA, gateways, edges), PinnedCollectors: 0},
			{PublicationID: publicationA2, Groups: []groupRefReply{}, PinnedCollectors: 1},
		},
		TotalPublications: 2, TotalGroups: 2, TotalPinnedCollectors: 1,
	}
	expectDetails(t, "the delete", call(s, http.MethodDelete, variableTarget(orgA, variableA), nil, ""),
		http.StatusConflict, codeVariableInUse, messageVariableInUse, want)
	expectDetails(t, "the rename", sendWith(s, http.MethodPatch, variableTarget(orgA, variableA), body{"name": "AWS_REGION", "actor": nil}),
		http.StatusConflict, codeVariableInUse, messageVariableInUse, want)
	expectStatus(t, "a value change", sendWith(s, http.MethodPatch, variableTarget(orgA, variableA), body{"value": "us-east-1", "actor": nil}),
		http.StatusOK)
	// A rename to its own name changes nothing and is no rename.
	expectStatus(t, "the same name", sendWith(s, http.MethodPatch, variableTarget(orgA, variableA), body{"name": "REGION", "actor": nil}),
		http.StatusOK)

	// Nothing uses it once the groups and the pin move off.
	for _, id := range []string{gateways, edges} {
		expectStatus(t, "unassigning", sendWith(s, http.MethodPatch, groupTarget(orgA, fleetA, id), body{"assignedPublicationId": nil}), http.StatusOK)
	}
	expectStatus(t, "unpinning", sendWith(s, http.MethodPatch, collectorTarget(orgA, fleetA, collectorID(t, s, orgA, fleetA, "edge-01")),
		body{"pin": nil}), http.StatusOK)
	expectStatus(t, "the rename", sendWith(s, http.MethodPatch, variableTarget(orgA, variableA), body{"name": "AWS_REGION", "actor": nil}),
		http.StatusOK)
	expectStatus(t, "the delete", call(s, http.MethodDelete, variableTarget(orgA, variableA), nil, ""), http.StatusNoContent)
}

// A variable group bound to collector groups or pinned on collectors isn't
// deleted, and the answer lists the groups and counts the pins.
func TestAVariableGroupInUseAnswers409WithItsGroupsAndPins(t *testing.T) {
	s, _, _ := fleetServer(t)
	assertAVariableGroupInUseAnswers409WithItsGroupsAndPins(t, s)
}

func assertAVariableGroupInUseAnswers409WithItsGroupsAndPins(t *testing.T, s *Server) {
	t.Helper()
	seedVariableGroup(orgA, variableGroupA, "production")(t, s)
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	createFleet(t, s, orgA, fleetA2, "edge-dev", "server")
	seedBoundGroup(orgA, fleetA, "gateways", variableGroupA)(t, s)
	seedBoundGroup(orgA, fleetA2, "gateways", variableGroupA)(t, s)
	enrollCollector(t, s, orgA, fleetA, "edge-01")
	enrollCollector(t, s, orgA, fleetA, "edge-02")
	seedPinnedVariableGroup(orgA, fleetA, "edge-01", variableGroupA)(t, s)
	seedPinnedVariableGroup(orgA, fleetA, "edge-02", variableGroupA)(t, s)
	groups := []groupRefReply{
		{FleetID: fleetA, GroupID: groupID(t, s, orgA, fleetA, "gateways")},
		{FleetID: fleetA2, GroupID: groupID(t, s, orgA, fleetA2, "gateways")},
	}
	slices.SortFunc(groups, func(a, b groupRefReply) int { return strings.Compare(a.FleetID+a.GroupID, b.FleetID+b.GroupID) })
	expectDetails(t, "the delete", call(s, http.MethodDelete, variableGroupTarget(orgA, variableGroupA), nil, ""),
		http.StatusConflict, codeVariableGroupInUse, messageVariableGroupInUse,
		variableGroupInUseDetails{CollectorGroups: groups, TotalCollectorGroups: 2, PinnedCollectors: 2})
	var list variableGroupListReply
	decodeInto(t, call(s, http.MethodGet, variableGroupsTarget(orgA), nil, "").Body.Bytes(), &list)
	if len(list.VariableGroups) != 1 || list.VariableGroups[0].Usage.CollectorGroups != 2 || list.VariableGroups[0].Usage.PinnedCollectors != 2 {
		t.Fatalf("the list is %+v", list)
	}

	// Pinned only, it is still in use.
	for _, f := range []string{fleetA, fleetA2} {
		expectStatus(t, "unbinding", sendWith(s, http.MethodPatch, groupTarget(orgA, f, groupID(t, s, orgA, f, "gateways")),
			body{"variableGroupId": nil}), http.StatusOK)
	}
	expectDetails(t, "pinned only", call(s, http.MethodDelete, variableGroupTarget(orgA, variableGroupA), nil, ""),
		http.StatusConflict, codeVariableGroupInUse, messageVariableGroupInUse,
		variableGroupInUseDetails{CollectorGroups: []groupRefReply{}, TotalCollectorGroups: 0, PinnedCollectors: 2})
	expectStatus(t, "unpinning", sendWith(s, http.MethodPost, collectorsPinTarget(orgA, fleetA), body{"collectorIds": []string{
		collectorID(t, s, orgA, fleetA, "edge-01"), collectorID(t, s, orgA, fleetA, "edge-02")}, "pinnedVariableGroupId": nil}), http.StatusOK)
	expectStatus(t, "the delete", call(s, http.MethodDelete, variableGroupTarget(orgA, variableGroupA), nil, ""), http.StatusNoContent)
}

// Each guard's details list at most 100 entries, in their order, with
// exact totals.
func TestTheDetailsListAtMostAHundredWithExactTotals(t *testing.T) {
	t.Run("variable_in_use", func(t *testing.T) {
		s, _, _ := fleetServer(t)
		seedVariable(orgA, variableA, "REGION", "x")(t, s)
		createFleet(t, s, orgA, fleetA, "edge-prod", "server")
		seedPublication(orgA, publicationA, referencing("REGION"))(t, s)
		seedPublication(orgA, publicationA2, referencing("REGION", "REGION"))(t, s)
		var ids []string
		for i := range 101 {
			name := fmt.Sprintf("g%03d", i)
			seedAssignedGroup(orgA, fleetA, name, publicationA)(t, s)
			ids = append(ids, groupID(t, s, orgA, fleetA, name))
		}
		enrollCollector(t, s, orgA, fleetA, "edge-01")
		seedPin(orgA, fleetA, "edge-01", publicationA2)(t, s)
		// publicationA's 101 groups fill the list at 100; publicationA2,
		// pinned only, is counted but not listed.
		want := variableInUseDetails{
			UsedBy:            []variableUseReply{{PublicationID: publicationA, Groups: refsOf(fleetA, ids...)[:100], PinnedCollectors: 0}},
			TotalPublications: 2, TotalGroups: 101, TotalPinnedCollectors: 1,
		}
		expectDetails(t, "the delete", call(s, http.MethodDelete, variableTarget(orgA, variableA), nil, ""),
			http.StatusConflict, codeVariableInUse, messageVariableInUse, want)
	})
	t.Run("variable_group_in_use", func(t *testing.T) {
		s, _, _ := fleetServer(t)
		seedVariableGroup(orgA, variableGroupA, "production")(t, s)
		createFleet(t, s, orgA, fleetA, "edge-prod", "server")
		var ids []string
		for i := range 101 {
			name := fmt.Sprintf("g%03d", i)
			seedBoundGroup(orgA, fleetA, name, variableGroupA)(t, s)
			ids = append(ids, groupID(t, s, orgA, fleetA, name))
		}
		expectDetails(t, "the delete", call(s, http.MethodDelete, variableGroupTarget(orgA, variableGroupA), nil, ""),
			http.StatusConflict, codeVariableGroupInUse, messageVariableGroupInUse,
			variableGroupInUseDetails{CollectorGroups: refsOf(fleetA, ids...)[:100], TotalCollectorGroups: 101, PinnedCollectors: 0})
	})
	t.Run("variables_undefined", func(t *testing.T) {
		s, _, _ := fleetServer(t)
		createFleet(t, s, orgA, fleetA, "edge-prod", "server")
		var names []string
		for i := range 101 {
			names = append(names, fmt.Sprintf("N%03d", 100-i))
		}
		seedPublication(orgA, publicationA, referencing(names...))(t, s)
		sorted := sortedIDs(names...)
		expectDetails(t, "the assignment", sendWith(s, http.MethodPost, groupsTarget(orgA, fleetA),
			body{"name": "g", "assignedPublicationId": publicationA}), http.StatusUnprocessableEntity, codeVariablesUndefined,
			messageVariablesUndefined, variablesUndefinedDetails{Names: sorted[:100], TotalNames: 101})
	})
}

// An override set (201), replaced (200), set to the same value (200,
// nothing moves), reset (204, again 204), and set again on the reset row
// (201); the group's count follows.
func TestAnOverrideIsSetReplacedResetAndSetAgain(t *testing.T) {
	s, clock, _ := fleetServer(t)
	assertAnOverrideIsSetReplacedResetAndSetAgain(t, s, clock)
}

func assertAnOverrideIsSetReplacedResetAndSetAgain(t *testing.T, s *Server, clock *testClock) {
	t.Helper()
	seedVariable(orgA, variableA, "REGION", "eu-west-1")(t, s)
	seedVariableGroup(orgA, variableGroupA, "production")(t, s)
	target := overrideTarget(orgA, variableGroupA, variableA)
	count := func(label string, want int) variableGroupDetailReply {
		t.Helper()
		var detail variableGroupDetailReply
		decodeInto(t, call(s, http.MethodGet, variableGroupTarget(orgA, variableGroupA), nil, "").Body.Bytes(), &detail)
		if detail.VariableGroup.OverrideCount != want || len(detail.Overrides) != want {
			t.Fatalf("%s: the group counts %d and answers %d overrides, want %d", label, detail.VariableGroup.OverrideCount, len(detail.Overrides), want)
		}
		return detail
	}
	expectStatus(t, "set", sendWith(s, http.MethodPut, target, body{"value": "eu-central-1", "actor": actorJSONBody()}), http.StatusCreated)
	count("set", 1)
	clock.Advance(time.Minute)
	replaced := sendWith(s, http.MethodPut, target, body{"value": "us-east-1", "actor": nil})
	expectStatus(t, "replaced", replaced, http.StatusOK)
	var o overrideReply
	decodeInto(t, replaced.Body.Bytes(), &o)
	if o.Value != "us-east-1" || o.ChangedBy != nil || o.ChangedAt != formatTime(clock.Now()) {
		t.Fatalf("replaced %+v", o)
	}
	before := count("replaced", 1)
	clock.Advance(time.Minute)
	same := sendWith(s, http.MethodPut, target, body{"value": "us-east-1", "actor": actorJSONBody()})
	expectStatus(t, "the same value", same, http.StatusOK)
	if same.Body.String() != replaced.Body.String() {
		t.Fatalf("the same value answered %s, want the override as it was, %s", same.Body.String(), replaced.Body.String())
	}
	if after := count("the same value", 1); !sameJSON(after, before) {
		t.Fatalf("the same value moved the group: %+v, was %+v", after, before)
	}
	// A Reset needs its actor, and records it on the group.
	expectError(t, "a reset without a body", call(s, http.MethodDelete, target, nil, ""),
		http.StatusBadRequest, codeInvalidRequest, "Content-Type must be application/json.")
	expectError(t, "a reset without an actor", call(s, http.MethodDelete, target, []byte(`{}`), jsonType),
		http.StatusBadRequest, codeInvalidRequest, "actor is required.")
	count("refused resets", 1)
	clock.Advance(time.Minute)
	expectStatus(t, "reset", sendWith(s, http.MethodDelete, target, body{"actor": actorJSONBody()}), http.StatusNoContent)
	reset := count("reset", 0)
	if !sameJSON(reset.VariableGroup.ChangedBy, &actorReply{ID: sampleActorID, Email: sampleEmail}) ||
		reset.VariableGroup.ChangedAt != formatTime(clock.Now()) {
		t.Fatalf("the reset didn't record its actor and time on the group: %+v", reset.VariableGroup)
	}
	// A reset of nothing changes nothing, and still needs a valid actor.
	expectError(t, "reset again, a malformed actor", call(s, http.MethodDelete, target, []byte(`{"actor":{"id":"x","email":"e"}}`), jsonType),
		http.StatusBadRequest, codeInvalidRequest, "actor.id must be a UUID.")
	clock.Advance(time.Minute)
	expectStatus(t, "reset again", sendWith(s, http.MethodDelete, target, body{"actor": nil}), http.StatusNoContent)
	if again := count("reset again", 0); !sameJSON(again, reset) {
		t.Fatalf("a reset of nothing moved the group: %+v, was %+v", again, reset)
	}
	expectStatus(t, "set again on the reset row", sendWith(s, http.MethodPut, target, body{"value": "us-east-1", "actor": nil}), http.StatusCreated)
	count("set again", 1)
}

// The 151st variable and the 101st variable group are refused; a deleted
// one frees its place.
func TestTheLimitsAreAnswered(t *testing.T) {
	s, _, _ := fleetServer(t)
	seedVariables(orgA, variables.MaxVariables)(t, s)
	expectError(t, "the 151st variable", sendWith(s, http.MethodPost, variablesTarget(orgA), variableBody("", "ONE_TOO_MANY", "x")),
		http.StatusConflict, codeVariableLimitReached, messageVariableLimitReached)
	var list variableListReply
	decodeInto(t, call(s, http.MethodGet, variablesTarget(orgA), nil, "").Body.Bytes(), &list)
	if len(list.Variables) != variables.MaxVariables {
		t.Fatalf("%d variables, want %d", len(list.Variables), variables.MaxVariables)
	}
	expectStatus(t, "a delete", call(s, http.MethodDelete, variableTarget(orgA, list.Variables[0].ID), nil, ""), http.StatusNoContent)
	seedVariable(orgA, "", "ONE_MORE", "x")(t, s)
	// Another organization's count is its own.
	seedVariable(orgB, "", "ONE_TOO_MANY", "x")(t, s)

	seedVariableGroups(orgA, variables.MaxGroups)(t, s)
	expectError(t, "the 101st variable group", sendWith(s, http.MethodPost, variableGroupsTarget(orgA), variableGroupBody("", "one too many")),
		http.StatusConflict, codeVariableGroupLimitReached, messageVariableGroupLimitReached)
	seedVariableGroup(orgB, "", "one too many")(t, s)
}

// variableRoutes are the variables routes with organization A's ids, and a
// body each would accept.
func variableRoutes() []fleetRoute {
	return []fleetRoute{
		{"list variables", http.MethodGet, func(o, _, _, _ string) string { return variablesTarget(o) }, nil},
		{"patch variable", http.MethodPatch, func(o, _, _, _ string) string { return variableTarget(o, variableA) },
			body{"name": "STOLEN", "value": "x", "description": nil, "actor": nil}},
		{"delete variable", http.MethodDelete, func(o, _, _, _ string) string { return variableTarget(o, variableA) }, nil},
		{"list variable groups", http.MethodGet, func(o, _, _, _ string) string { return variableGroupsTarget(o) }, nil},
		{"get variable group", http.MethodGet, func(o, _, _, _ string) string { return variableGroupTarget(o, variableGroupA) }, nil},
		{"patch variable group", http.MethodPatch, func(o, _, _, _ string) string { return variableGroupTarget(o, variableGroupA) },
			body{"name": "stolen", "actor": nil}},
		{"delete variable group", http.MethodDelete, func(o, _, _, _ string) string { return variableGroupTarget(o, variableGroupA) }, nil},
		{"set override", http.MethodPut, func(o, _, _, _ string) string { return overrideTarget(o, variableGroupA, variableA) },
			body{"value": "stolen", "actor": nil}},
		{"reset override", http.MethodDelete, func(o, _, _, _ string) string { return overrideTarget(o, variableGroupA, variableA) },
			body{"actor": nil}},
	}
}

// Organization B naming A's variable, variable group or override on every
// route is a plain 404 that changes neither organization's rows; B's lists
// hold none of A's; B can't bind A's variable group to its groups or pin it
// on its collectors (422), nor create with A's id (409, saying nothing).
func TestAnotherOrganizationsVariablesAreNotFound(t *testing.T) {
	s, _, _ := fleetServer(t)
	assertAnotherOrganizationsVariablesAreNotFound(t, s, func(org string) string { return dump(t, s, org) })
}

func assertAnotherOrganizationsVariablesAreNotFound(t *testing.T, s *Server, snapshot func(org string) string) {
	t.Helper()
	seedVariable(orgA, variableA, "REGION", "eu-west-1")(t, s)
	seedVariableGroup(orgA, variableGroupA, "production")(t, s)
	seedOverride(orgA, variableGroupA, variableA, "eu-central-1")(t, s)
	createFleet(t, s, orgB, fleetB, "edge-b", "server")
	enrollCollector(t, s, orgB, fleetB, "edge-b1")
	theirCollector := collectorID(t, s, orgB, fleetB, "edge-b1")
	beforeA, beforeB := snapshot(orgA), snapshot(orgB)

	for _, r := range variableRoutes() {
		got := sendWith(s, r.method, r.target(orgB, "", "", ""), r.body)
		switch r.name {
		case "list variables":
			expectStatus(t, r.name, got, http.StatusOK)
			if got.Body.String() != `{"variables":[]}` {
				t.Errorf("B's list holds %s", got.Body.String())
			}
		case "list variable groups":
			expectStatus(t, r.name, got, http.StatusOK)
			if got.Body.String() != `{"variableGroups":[]}` {
				t.Errorf("B's list holds %s", got.Body.String())
			}
		case "patch variable", "delete variable":
			expectError(t, r.name, got, http.StatusNotFound, codeVariableNotFound, messageVariableNotFound)
		default:
			expectError(t, r.name, got, http.StatusNotFound, codeVariableGroupNotFound, messageVariableGroupNotFound)
		}
	}
	expectError(t, "B creating A's variable id", sendWith(s, http.MethodPost, variablesTarget(orgB), variableBody(variableA, "REGION", "eu-west-1")),
		http.StatusConflict, codeVariableConflict, messageVariableConflict)
	expectError(t, "B creating A's variable group id", sendWith(s, http.MethodPost, variableGroupsTarget(orgB),
		variableGroupBody(variableGroupA, "production")), http.StatusConflict, codeVariableGroupConflict, messageVariableGroupConflict)
	bGroup := groupID(t, s, orgB, fleetB, "Default")
	for label, got := range map[string]*httptest.ResponseRecorder{
		"B binding A's variable group to a group": sendWith(s, http.MethodPatch, groupTarget(orgB, fleetB, bGroup),
			body{"variableGroupId": variableGroupA}),
		"B creating a group bound to A's variable group": sendWith(s, http.MethodPost, groupsTarget(orgB, fleetB),
			body{"name": "theirs", "variableGroupId": variableGroupA}),
		"B pinning A's variable group": sendWith(s, http.MethodPatch, collectorTarget(orgB, fleetB, theirCollector),
			body{"pinnedVariableGroupId": variableGroupA}),
		"B bulk-pinning A's variable group": sendWith(s, http.MethodPost, collectorsPinTarget(orgB, fleetB),
			body{"collectorIds": []string{theirCollector}, "pinnedVariableGroupId": variableGroupA}),
	} {
		expectError(t, label, got, http.StatusUnprocessableEntity, codeVariableGroupNotFound, messageVariableGroupNotFound)
	}
	if after := snapshot(orgA); after != beforeA {
		t.Fatalf("B's calls changed A's rows:\n%s\n%s", beforeA, after)
	}
	if after := snapshot(orgB); after != beforeB {
		t.Fatalf("B's calls changed B's rows:\n%s\n%s", beforeB, after)
	}
	// A's own calls still see A's rows.
	expectStatus(t, "A's read", call(s, http.MethodGet, variableGroupTarget(orgA, variableGroupA), nil, ""), http.StatusOK)
}

// A group's create or PATCH assigning a published version that references
// names no variable of the organization has answers 422 with the names,
// changing nothing; defined, the assignment goes through.
func TestAGroupsAssignmentNamingAnUndefinedVariableAnswers422WithTheNames(t *testing.T) {
	s, _, _ := fleetServer(t)
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	seedVariable(orgA, variableA, "REGION", "x")(t, s)
	seedPublication(orgA, publicationA, referencing("REGION", "TOKEN_HEADER", "API_KEY", "region"))(t, s)
	want := variablesUndefinedDetails{Names: []string{"API_KEY", "TOKEN_HEADER", "region"}, TotalNames: 3}
	before := dump(t, s, orgA)
	expectDetails(t, "the create", sendWith(s, http.MethodPost, groupsTarget(orgA, fleetA), body{"name": "g", "assignedPublicationId": publicationA}),
		http.StatusUnprocessableEntity, codeVariablesUndefined, messageVariablesUndefined, want)
	defaultGroup := groupTarget(orgA, fleetA, groupID(t, s, orgA, fleetA, "Default"))
	expectDetails(t, "the PATCH", sendWith(s, http.MethodPatch, defaultGroup, body{"annotations": nil, "assignedPublicationId": publicationA}),
		http.StatusUnprocessableEntity, codeVariablesUndefined, messageVariablesUndefined, want)
	if after := dump(t, s, orgA); after != before {
		t.Fatalf("a refused assignment changed something:\n%s\n%s", before, after)
	}
	// A variable group's override defines nothing: "defined" is an
	// organization variable.
	seedVariableGroup(orgA, variableGroupA, "production")(t, s)
	expectDetails(t, "with a variable group", sendWith(s, http.MethodPatch, defaultGroup,
		body{"assignedPublicationId": publicationA, "variableGroupId": variableGroupA}),
		http.StatusUnprocessableEntity, codeVariablesUndefined, messageVariablesUndefined, want)
	for _, name := range want.Names {
		seedVariable(orgA, "", name, "x")(t, s)
	}
	expectStatus(t, "defined", sendWith(s, http.MethodPatch, defaultGroup, body{"assignedPublicationId": publicationA}), http.StatusOK)
	expectStatus(t, "defined, a create", sendWith(s, http.MethodPost, groupsTarget(orgA, fleetA),
		body{"name": "g", "assignedPublicationId": publicationA}), http.StatusCreated)
}

// A collector's pin and the bulk pin of a published version referencing
// undefined names answer 422 with the names, pinning nothing.
func TestAPinAndABulkPinNamingAnUndefinedVariableAnswer422(t *testing.T) {
	s, _, _ := fleetServer(t)
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	enrollCollector(t, s, orgA, fleetA, "edge-01")
	enrollCollector(t, s, orgA, fleetA, "edge-02")
	seedVariableGroup(orgA, variableGroupA, "production")(t, s)
	seedPublication(orgA, publicationA, referencing("TOKEN_HEADER"))(t, s)
	want := variablesUndefinedDetails{Names: []string{"TOKEN_HEADER"}, TotalNames: 1}
	pin := map[string]any{"kind": "publication", "id": publicationA}
	edge01, edge02 := collectorID(t, s, orgA, fleetA, "edge-01"), collectorID(t, s, orgA, fleetA, "edge-02")
	before := dump(t, s, orgA)
	expectDetails(t, "the pin", sendWith(s, http.MethodPatch, collectorTarget(orgA, fleetA, edge01),
		body{"pin": pin, "pinnedVariableGroupId": variableGroupA}),
		http.StatusUnprocessableEntity, codeVariablesUndefined, messageVariablesUndefined, want)
	expectDetails(t, "the bulk pin", sendWith(s, http.MethodPost, collectorsPinTarget(orgA, fleetA),
		body{"collectorIds": []string{edge01, edge02}, "pin": pin, "pinnedVariableGroupId": variableGroupA}),
		http.StatusUnprocessableEntity, codeVariablesUndefined, messageVariablesUndefined, want)
	if after := dump(t, s, orgA); after != before {
		t.Fatalf("a refused pin changed something:\n%s\n%s", before, after)
	}
	seedVariable(orgA, "", "TOKEN_HEADER", "x")(t, s)
	expectStatus(t, "defined, the bulk pin", sendWith(s, http.MethodPost, collectorsPinTarget(orgA, fleetA),
		body{"collectorIds": []string{edge01, edge02}, "pin": pin}), http.StatusOK)
}

// A group's variable group and a collector's pinned one are set and cleared
// through the routes and answered; a pinned variable group is independent
// of the pin; the bulk pin leaves an absent choice as it is, and null
// returns each collector to its group's.
func TestAGroupsAndAPinsVariableGroupAreSetAndCleared(t *testing.T) {
	s, _, _ := fleetServer(t)
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	seedVariableGroup(orgA, variableGroupA, "production")(t, s)
	seedVariableGroup(orgA, variableGroupA2, "staging")(t, s)
	seedPublication(orgA, publicationA, validBody(sampleYAML))(t, s)

	var g groupReply
	got := sendWith(s, http.MethodPost, groupsTarget(orgA, fleetA), body{"name": "gateways", "variableGroupId": variableGroupA})
	expectStatus(t, "a bound group", got, http.StatusCreated)
	decodeInto(t, got.Body.Bytes(), &g)
	if g.VariableGroupID == nil || *g.VariableGroupID != variableGroupA {
		t.Fatalf("the group's variable group is %v", g.VariableGroupID)
	}
	gateways := groupTarget(orgA, fleetA, g.ID)
	got = sendWith(s, http.MethodPatch, gateways, body{"variableGroupId": variableGroupA2})
	expectStatus(t, "changed", got, http.StatusOK)
	decodeInto(t, got.Body.Bytes(), &g)
	if g.VariableGroupID == nil || *g.VariableGroupID != variableGroupA2 {
		t.Fatalf("changed to %v", g.VariableGroupID)
	}
	// Another field's change leaves it as it is.
	got = sendWith(s, http.MethodPatch, gateways, body{"name": "edges"})
	decodeInto(t, got.Body.Bytes(), &g)
	if g.VariableGroupID == nil || *g.VariableGroupID != variableGroupA2 {
		t.Fatalf("a rename moved it to %v", g.VariableGroupID)
	}
	got = sendWith(s, http.MethodPatch, gateways, body{"variableGroupId": nil})
	decodeInto(t, got.Body.Bytes(), &g)
	if g.VariableGroupID != nil {
		t.Fatalf("cleared to %v", *g.VariableGroupID)
	}
	if !strings.Contains(call(s, http.MethodGet, gateways, nil, "").Body.String(), `"variableGroupId":null`) {
		t.Fatal("a group with none doesn't answer null")
	}

	enrollCollector(t, s, orgA, fleetA, "edge-01")
	enrollCollector(t, s, orgA, fleetA, "edge-02")
	edge01, edge02 := collectorID(t, s, orgA, fleetA, "edge-01"), collectorID(t, s, orgA, fleetA, "edge-02")
	read := func(id string) collectorInstanceReply { return collectorReplyOf(t, s, orgA, fleetA, id) }
	var c collectorInstanceReply
	got = sendWith(s, http.MethodPatch, collectorTarget(orgA, fleetA, edge01), body{"pinnedVariableGroupId": variableGroupA})
	expectStatus(t, "pinned", got, http.StatusOK)
	decodeInto(t, got.Body.Bytes(), &c)
	if c.PinnedVariableGroupID == nil || *c.PinnedVariableGroupID != variableGroupA || c.Pin != nil {
		t.Fatalf("pinned %v, pin %+v", c.PinnedVariableGroupID, c.Pin)
	}
	got = sendWith(s, http.MethodPatch, collectorTarget(orgA, fleetA, edge01), body{"pin": map[string]any{"kind": "publication", "id": publicationA}})
	decodeInto(t, got.Body.Bytes(), &c)
	if c.PinnedVariableGroupID == nil || *c.PinnedVariableGroupID != variableGroupA || c.Pin == nil {
		t.Fatalf("the pin moved the pinned variable group: %v, pin %+v", c.PinnedVariableGroupID, c.Pin)
	}

	// The bulk pin: a variable group only leaves the pins as they are.
	expectStatus(t, "bulk, a variable group", sendWith(s, http.MethodPost, collectorsPinTarget(orgA, fleetA),
		body{"collectorIds": []string{edge01, edge02}, "pinnedVariableGroupId": variableGroupA2}), http.StatusOK)
	if c = read(edge01); c.Pin == nil || *c.PinnedVariableGroupID != variableGroupA2 {
		t.Fatalf("edge-01 after the bulk variable group: pin %+v, %v", c.Pin, c.PinnedVariableGroupID)
	}
	if c = read(edge02); c.Pin != nil || *c.PinnedVariableGroupID != variableGroupA2 {
		t.Fatalf("edge-02 after the bulk variable group: pin %+v, %v", c.Pin, c.PinnedVariableGroupID)
	}
	// A pin only leaves the variable groups as they are.
	expectStatus(t, "bulk, unpin", sendWith(s, http.MethodPost, collectorsPinTarget(orgA, fleetA),
		body{"collectorIds": []string{edge01, edge02}, "pin": nil}), http.StatusOK)
	if c = read(edge01); c.Pin != nil || c.PinnedVariableGroupID == nil || *c.PinnedVariableGroupID != variableGroupA2 {
		t.Fatalf("edge-01 after the bulk unpin: pin %+v, %v", c.Pin, c.PinnedVariableGroupID)
	}
	// Null returns each to its group's.
	expectStatus(t, "bulk, follow the group", sendWith(s, http.MethodPost, collectorsPinTarget(orgA, fleetA),
		body{"collectorIds": []string{edge01, edge02}, "pinnedVariableGroupId": nil}), http.StatusOK)
	for _, id := range []string{edge01, edge02} {
		if c = read(id); c.PinnedVariableGroupID != nil {
			t.Fatalf("%s still pins %v", id, *c.PinnedVariableGroupID)
		}
	}
	if !strings.Contains(call(s, http.MethodGet, collectorTarget(orgA, fleetA, edge01), nil, "").Body.String(), `"pinnedVariableGroupId":null`) {
		t.Fatal("a collector with none doesn't answer null")
	}
	expectStatus(t, "clear", sendWith(s, http.MethodPatch, collectorTarget(orgA, fleetA, edge01), body{"pinnedVariableGroupId": nil}), http.StatusOK)
}

// The bulk pin needs at least one of its two choices, and a collector's
// PATCH at least one field.
func TestABulkPinNeedsAPinOrAVariableGroup(t *testing.T) {
	s, _, _ := fleetServer(t)
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	expectError(t, "the bulk pin with neither", sendWith(s, http.MethodPost, collectorsPinTarget(orgA, fleetA),
		body{"collectorIds": []string{unknownID}}), http.StatusBadRequest, codeInvalidRequest, messageNoBulkPinChange)
	expectError(t, "the bulk pin without ids", sendWith(s, http.MethodPost, collectorsPinTarget(orgA, fleetA),
		body{"pinnedVariableGroupId": nil}), http.StatusBadRequest, codeInvalidRequest, "collectorIds is required.")
	expectError(t, "a PATCH with nothing", sendWith(s, http.MethodPatch, collectorTarget(orgA, fleetA, unknownID), body{}),
		http.StatusBadRequest, codeInvalidRequest, messageNoCollectorChange)
	expectStatus(t, "the bulk pin with a variable group only", sendWith(s, http.MethodPost, collectorsPinTarget(orgA, fleetA),
		body{"collectorIds": []string{unknownID}, "pinnedVariableGroupId": nil}), http.StatusOK)
}

// Every variables route answers 404 organization_not_found under an
// unregistered organization, before it reads the body.
func TestEveryVariableRouteAnswersOrganizationNotFoundFirst(t *testing.T) {
	s, _, _ := fleetServer(t)
	for _, r := range variableRoutes() {
		got := sendWith(s, r.method, r.target(orgC, "", "", ""), r.body)
		expectError(t, r.name, got, http.StatusNotFound, codeOrganizationNotFound, messageOrganizationNotFound)
		if r.body != nil {
			got = call(s, r.method, r.target(orgC, "", "", ""), []byte("not json"), jsonType)
			expectError(t, r.name+" with a bad body", got, http.StatusNotFound, codeOrganizationNotFound, messageOrganizationNotFound)
		}
	}
	for _, target := range []string{variablesTarget(orgC), variableGroupsTarget(orgC)} {
		got := call(s, http.MethodPost, target, []byte("not json"), jsonType)
		expectError(t, "a create", got, http.StatusNotFound, codeOrganizationNotFound, messageOrganizationNotFound)
	}
}

// A variables store that fails answers 500 internal_error on every route,
// and the log line names the route and the store, nothing else.
func TestAFailingVariablesStoreAnswers500AndLogsNoDetail(t *testing.T) {
	d := testDB(t)
	registry := organizations.NewPostgres(d.Runtime)
	register(t, registry, orgA)
	fs := postgresFleetStores(d.Runtime)
	fs.variables = failingVariables{}
	s, _, log := newServerWith(t, registry, publications.NewPostgres(d.Runtime), fs)
	routes := append(variableRoutes(),
		fleetRoute{"create variable", http.MethodPost, func(o, _, _, _ string) string { return variablesTarget(o) }, variableBody("", "N", "v")},
		fleetRoute{"create variable group", http.MethodPost, func(o, _, _, _ string) string { return variableGroupsTarget(o) },
			variableGroupBody("", "g")})
	for _, r := range routes {
		got := sendWith(s, r.method, r.target(orgA, "", "", ""), r.body)
		expectError(t, r.name, got, http.StatusInternalServerError, codeInternalError, messageInternalError)
	}
	stores := map[string]bool{}
	for _, line := range logLines(t, log.String()) {
		if line["msg"] == "request" {
			continue
		}
		stores[line["msg"].(string)] = true
		for key := range line {
			if !slices.Contains([]string{"time", "level", "msg", "route"}, key) {
				t.Fatalf("the failure's log line has %q: %v", key, line)
			}
		}
	}
	if !stores["the variables store failed"] || !stores["the variable groups store failed"] || len(stores) != 2 {
		t.Fatalf("the failures logged %v", stores)
	}
	if strings.Contains(log.String(), errStoreDown.Error()) {
		t.Fatal("the store's error text reached the log")
	}
}

// failingVariables fails every call, as a database that is down would.
type failingVariables struct{}

var _ variables.Store = failingVariables{}

func (failingVariables) Variables(context.Context, uuid.UUID, *uuid.UUID, *uuid.UUID) (orgvariables.Resolved, error) {
	return orgvariables.Resolved{}, errStoreDown
}
func (failingVariables) ListVariables(context.Context, uuid.UUID) ([]variables.Variable, error) {
	return nil, errStoreDown
}
func (failingVariables) CreateVariable(context.Context, variables.NewVariable) (variables.CreateResult, variables.Variable, error) {
	return 0, variables.Variable{}, errStoreDown
}
func (failingVariables) UpdateVariable(context.Context, uuid.UUID, uuid.UUID, variables.VariableChanges, *fleets.Actor, time.Time) (variables.Variable, error) {
	return variables.Variable{}, errStoreDown
}
func (failingVariables) DeleteVariable(context.Context, uuid.UUID, uuid.UUID) error {
	return errStoreDown
}
func (failingVariables) ListGroups(context.Context, uuid.UUID) ([]variables.Group, error) {
	return nil, errStoreDown
}
func (failingVariables) CreateGroup(context.Context, variables.NewGroup) (variables.CreateResult, variables.Group, error) {
	return 0, variables.Group{}, errStoreDown
}
func (failingVariables) GetGroup(context.Context, uuid.UUID, uuid.UUID) (variables.Group, []variables.Override, error) {
	return variables.Group{}, nil, errStoreDown
}
func (failingVariables) UpdateGroup(context.Context, uuid.UUID, uuid.UUID, variables.GroupChanges, *fleets.Actor, time.Time) (variables.Group, error) {
	return variables.Group{}, errStoreDown
}
func (failingVariables) DeleteGroup(context.Context, uuid.UUID, uuid.UUID) error { return errStoreDown }
func (failingVariables) SetOverride(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, *fleets.Actor, time.Time) (variables.Override, bool, error) {
	return variables.Override{}, false, errStoreDown
}
func (failingVariables) ResetOverride(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, *fleets.Actor, time.Time) (bool, error) {
	return false, errStoreDown
}
