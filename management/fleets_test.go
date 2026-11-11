package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/telflo-opamp-server/store/enrollmenttokens"
	"github.com/telflo/telflo-opamp-server/store/organizations"
	"github.com/telflo/telflo-opamp-server/store/publications"
	"github.com/telflo/telflo-opamp-server/store/storetest"
	"github.com/telflo/telflo-opamp-server/tokens"
)

// dump is every row the organization holds in the test's database
// (testDB), as text: compared before and after, it shows nothing changed.
// The server is the test's: its stores are on that database.
func dump(t *testing.T, _ *Server, organizationID string) string {
	t.Helper()
	return (&storetest.Stores{Databases: testDB(t)}).Dump(t, uuid.MustParse(organizationID))
}

// readClaims opens a bearer with the test keys.
func readClaims(t *testing.T, bearer string) tokens.Claims {
	t.Helper()
	keys, err := tokens.NewKeyring(testEncryptionKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Destroy()
	claims, err := keys.Read(bearer)
	if err != nil {
		t.Fatalf("the bearer doesn't open with the current key: %v", err)
	}
	return claims
}

// tokenEvents are the fleet's token events, newest first, read from the
// store.
func tokenEvents(t *testing.T, s *Server, organizationID, fleetID string) []enrollmenttokens.Event {
	t.Helper()
	page, err := s.enrollmentTokens.ListEvents(context.Background(), uuid.MustParse(organizationID), uuid.MustParse(fleetID),
		enrollmenttokens.MaxListLimit, nil)
	if err != nil || page.Next != nil {
		t.Fatalf("ListEvents = %+v, %v; want one page", page, err)
	}
	return page.Events
}

func decodeInto(t *testing.T, raw []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("the reply isn't the JSON expected: %v: %s", err, raw)
	}
}

// A create answers the fleet, its Default group and both tokens' bearers,
// minted under the current key for the store's token ids and the fleet;
// the Default group answered is the one stored; each token has its
// `issued` event naming the actor.
func TestCreateFleetAnswersTheFleetItsDefaultGroupAndBothBearers(t *testing.T) {
	s, clock, _ := fleetServer(t)
	got := call(s, http.MethodPost, fleetsTarget(orgA), fleetBody(fleetA, "edge-prod", "server").bytes(), jsonType)
	expectStatus(t, "create", got, http.StatusCreated)
	var reply struct {
		Fleet        fleetReply `json:"fleet"`
		DefaultGroup groupReply `json:"defaultGroup"`
		Tokens       struct {
			Default bearerReply       `json:"default"`
			Rotate  rotateBearerReply `json:"rotate"`
		} `json:"tokens"`
	}
	decodeInto(t, got.Body.Bytes(), &reply)
	now := formatTime(clock.Now())
	if reply.Fleet.ID != fleetA || reply.Fleet.OrganizationID != orgA || reply.Fleet.Label != "edge-prod" || reply.Fleet.Type != "server" ||
		reply.Fleet.Status != "active" || reply.Fleet.CreatedAt != now || reply.Fleet.UpdatedAt != now ||
		reply.Fleet.CreatedBy == nil || reply.Fleet.CreatedBy.Email != sampleEmail {
		t.Fatalf("the fleet = %+v", reply.Fleet)
	}
	stored := call(s, http.MethodGet, groupTarget(orgA, fleetA, reply.DefaultGroup.ID), nil, "")
	expectStatus(t, "the Default group", stored, http.StatusOK)
	answered, _ := json.Marshal(reply.DefaultGroup)
	if string(answered) != stored.Body.String() || !reply.DefaultGroup.IsDefault || reply.DefaultGroup.Name != "Default" {
		t.Fatalf("the Default group answered %s, stored %s", answered, stored.Body.String())
	}

	tokensReply := reply.Tokens
	if c := readClaims(t, tokensReply.Default.Bearer); c.TokenID.String() != tokensReply.Default.ID || c.FleetID.String() != fleetA {
		t.Fatalf("the default bearer carries %+v", c)
	}
	if c := readClaims(t, tokensReply.Rotate.Bearer); c.TokenID.String() != tokensReply.Rotate.ID || c.FleetID.String() != fleetA {
		t.Fatalf("the rotate bearer carries %+v", c)
	}
	if tokensReply.Rotate.ExpiresAt == nil || *tokensReply.Rotate.ExpiresAt != futureTime {
		t.Fatalf("the rotate token expires %v, want %s", tokensReply.Rotate.ExpiresAt, futureTime)
	}
	if tokensReply.Default.ID != tokenID(t, s, orgA, fleetA, enrollmenttokens.KindDefault) ||
		tokensReply.Rotate.ID != tokenID(t, s, orgA, fleetA, enrollmenttokens.KindRotate) {
		t.Fatal("the bearers' ids aren't the stored tokens'")
	}
	events := tokenEvents(t, s, orgA, fleetA)
	if len(events) != 2 {
		t.Fatalf("events = %+v, want two issued", events)
	}
	for _, e := range events {
		if e.Kind != enrollmenttokens.EventIssued || e.Actor == nil || e.Actor.Email != sampleEmail {
			t.Fatalf("event = %+v", e)
		}
	}
}

// A create's idempotence: the same id, label and type again answers 200
// with the fleet and no bearer, changing nothing; the id with another label
// or type, or another organization's, is a 409 fleet_conflict; a label
// taken is a 409 fleet_label_taken. Without an id the server makes one.
func TestCreateFleetIsIdempotentOnTheCallersID(t *testing.T) {
	s, _, _ := fleetServer(t)
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	before := dump(t, s, orgA)

	again := call(s, http.MethodPost, fleetsTarget(orgA), fleetBody(fleetA, "edge-prod", "server").with("actor", nil).bytes(), jsonType)
	expectStatus(t, "the same create", again, http.StatusOK)
	if strings.Contains(again.Body.String(), "bearer") || strings.Contains(again.Body.String(), "tokens") {
		t.Fatalf("a retry answered bearers: %s", again.Body.String())
	}
	var unchanged fleetUnchangedReply
	decodeInto(t, again.Body.Bytes(), &unchanged)
	if unchanged.Fleet.ID != fleetA || unchanged.Fleet.Label != "edge-prod" {
		t.Fatalf("the retry's fleet = %+v", unchanged.Fleet)
	}
	for _, c := range []struct {
		label, target string
		body          body
		code          errorCode
		message       string
	}{
		{"another label", fleetsTarget(orgA), fleetBody(fleetA, "edge-dev", "server"), codeFleetConflict, messageFleetConflict},
		{"another type", fleetsTarget(orgA), fleetBody(fleetA, "edge-prod", "kubernetes-supervisor"), codeFleetConflict, messageFleetConflict},
		{"another organization's id", fleetsTarget(orgB), fleetBody(fleetA, "edge-prod", "server"), codeFleetConflict, messageFleetConflict},
		{"the label taken", fleetsTarget(orgA), fleetBody(fleetA2, "edge-prod", "server"), codeFleetLabelTaken, messageFleetLabelTaken},
	} {
		expectError(t, c.label, call(s, http.MethodPost, c.target, c.body.bytes(), jsonType), http.StatusConflict, c.code, c.message)
	}
	if after := dump(t, s, orgA); after != before {
		t.Fatalf("the retries changed organization A's rows")
	}
	if theirs := dump(t, s, orgB); strings.Contains(theirs, "fleets ") {
		t.Fatalf("a refused create stored a fleet under B: %s", theirs)
	}

	generated := call(s, http.MethodPost, fleetsTarget(orgA), fleetBody("", "edge-dev", "server").bytes(), jsonType)
	expectStatus(t, "a create without an id", generated, http.StatusCreated)
	var c created
	decodeInto(t, generated.Body.Bytes(), &c)
	if _, err := uuid.Parse(c.Fleet.ID); err != nil || c.Fleet.ID == fleetA {
		t.Fatalf("the generated id = %q", c.Fleet.ID)
	}
}

// Every refusal of a create's body names the field and never its value.
func TestCreateFleetRefusesABodyItCannotStore(t *testing.T) {
	s, _, _ := fleetServer(t)
	valid := fleetBody(fleetA, "edge-prod", "server")
	for _, c := range []struct {
		label   string
		body    body
		message string
	}{
		{"a leading space", valid.with("label", " edge-prod"), messageLabel},
		{"a trailing ideographic space", valid.with("label", "edge-prod　"), messageLabel},
		{"an empty label", valid.with("label", ""), messageLabel},
		{"a label of 81 characters", valid.with("label", strings.Repeat("é", 81)), messageLabel},
		{"a label with U+0000", valid.with("label", "edge\u0000prod"), messageLabel},
		// No control character, as a collector's name.
		{"a label with a newline", valid.with("label", "edge-prod\nrack-7"), messageLabel},
		{"a label with an escape", valid.with("label", "edge\u001b[31mprod"), messageLabel},
		{"a label with a C1 character", valid.with("label", "edge\u0085prod"), messageLabel},
		{"a null label", valid.with("label", nil), messageLabel},
		{"the bridge's type", valid.with("type", "kubernetes"), messageFleetType},
		{"a past expiry", valid.with("rotateExpiresAt", "2026-10-03T15:00:00.123Z"), "rotateExpiresAt must be in the future, or null."},
		{"an expiry that isn't a time", valid.with("rotateExpiresAt", "next year"), "rotateExpiresAt must be an RFC 3339 date-time."},
		{"an actor without an email", valid.with("actor", map[string]any{"id": actorA}), "actor.email is required."},
		{"an actor email of 321 characters", valid.with("actor", map[string]any{"id": actorA, "email": strings.Repeat("e", 321)}),
			"actor.email must be 1 to 320 characters."},
		{"a null id", valid.with("id", nil), "id must be a UUID."},
		{"no rotateExpiresAt", valid.without("rotateExpiresAt"), "rotateExpiresAt is required."},
	} {
		expectError(t, c.label, call(s, http.MethodPost, fleetsTarget(orgA), c.body.bytes(), jsonType),
			http.StatusBadRequest, codeInvalidRequest, c.message)
	}
	if strings.Contains(dump(t, s, orgA), "fleets ") {
		t.Fatal("a refused create stored a fleet")
	}
}

// GET …/fleets pages newest first and filters by status; PATCH renames,
// archives and unarchives (updatedAt moving only on a change); DELETE takes
// the fleet with everything it holds.
func TestFleetsListUpdateAndDelete(t *testing.T) {
	s, clock, _ := fleetServer(t)
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	clock.Advance(time.Minute)
	createFleet(t, s, orgA, fleetA2, "edge-dev", "kubernetes-supervisor")
	createFleet(t, s, orgB, fleetB, "edge-prod", "server")

	var page fleetListReply
	decodeInto(t, call(s, http.MethodGet, fleetsTarget(orgA)+"?limit=1", nil, "").Body.Bytes(), &page)
	if len(page.Fleets) != 1 || page.Fleets[0].ID != fleetA2 || page.NextCursor == nil {
		t.Fatalf("the first page = %+v", page)
	}
	decodeInto(t, call(s, http.MethodGet, fleetsTarget(orgA)+"?limit=1&cursor="+*page.NextCursor, nil, "").Body.Bytes(), &page)
	if len(page.Fleets) != 1 || page.Fleets[0].ID != fleetA || page.NextCursor != nil {
		t.Fatalf("the last page = %+v", page)
	}

	clock.Advance(time.Minute)
	renamed := call(s, http.MethodPatch, fleetTarget(orgA, fleetA), []byte(`{"label":"edge-eu","status":"archived"}`), jsonType)
	expectStatus(t, "rename and archive", renamed, http.StatusOK)
	var fleet fleetReply
	decodeInto(t, renamed.Body.Bytes(), &fleet)
	if fleet.Label != "edge-eu" || fleet.Status != "archived" || fleet.UpdatedAt != formatTime(clock.Now()) {
		t.Fatalf("the renamed fleet = %+v", fleet)
	}
	clock.Advance(time.Minute)
	decodeInto(t, call(s, http.MethodPatch, fleetTarget(orgA, fleetA), []byte(`{"label":"edge-eu"}`), jsonType).Body.Bytes(), &fleet)
	if fleet.UpdatedAt == formatTime(clock.Now()) {
		t.Fatal("an unchanged label moved updatedAt")
	}
	decodeInto(t, call(s, http.MethodGet, fleetsTarget(orgA)+"?status=archived", nil, "").Body.Bytes(), &page)
	if len(page.Fleets) != 1 || page.Fleets[0].ID != fleetA {
		t.Fatalf("the archived = %+v", page)
	}
	decodeInto(t, call(s, http.MethodPatch, fleetTarget(orgA, fleetA), []byte(`{"status":"active"}`), jsonType).Body.Bytes(), &fleet)
	if fleet.Status != "active" {
		t.Fatalf("unarchived = %+v", fleet)
	}
	expectError(t, "a label taken", call(s, http.MethodPatch, fleetTarget(orgA, fleetA), []byte(`{"label":"edge-dev"}`), jsonType),
		http.StatusConflict, codeFleetLabelTaken, messageFleetLabelTaken)

	expectStatus(t, "delete", call(s, http.MethodDelete, fleetTarget(orgA, fleetA), nil, ""), http.StatusNoContent)
	expectError(t, "deleted", call(s, http.MethodGet, fleetTarget(orgA, fleetA), nil, ""), http.StatusNotFound, codeFleetNotFound, messageFleetNotFound)
	if strings.Contains(dump(t, s, orgA), fleetA) {
		t.Fatal("the fleet's rows survived its delete")
	}
	expectError(t, "deleted again", call(s, http.MethodDelete, fleetTarget(orgA, fleetA), nil, ""), http.StatusNotFound, codeFleetNotFound, messageFleetNotFound)
}

// Annotations on any fleet's group, created, answered in canonical form
// (keys sorted at every level, nested values and numbers as written),
// replaced whole by an object, kept by a PATCH without them, cleared by
// null, on the Default group too, and in every group reply (null when
// unset); what isn't an object, or is over 16,384 bytes, is a 400 that
// changes nothing.
func TestCollectorGroupAnnotationsOnAnyFleetsGroup(t *testing.T) {
	s, _, _ := fleetServer(t)
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	createFleet(t, s, orgA, fleetA2, "edge-k8s", "kubernetes-supervisor")
	const (
		sent = `{"telflo": {"dockerMounts": [{"readOnly": true, "hostPath": "/var/log/nginx", "containerPath": "/logs"}]},` +
			` "acme.io/owner": "ops", "n": [1.50, 100, -0.5]}`
		want = `{"acme.io/owner":"ops","n":[1.50,100,-0.5],"telflo":{"dockerMounts":[{"containerPath":"/logs","hostPath":"/var/log/nginx","readOnly":true}]}}`
	)
	for _, fleet := range []string{fleetA, fleetA2} {
		created := call(s, http.MethodPost, groupsTarget(orgA, fleet), []byte(`{"name":"gateways","annotations":`+sent+`}`), jsonType)
		expectStatus(t, "create with annotations", created, http.StatusCreated)
		var g groupReply
		decodeInto(t, created.Body.Bytes(), &g)
		if string(g.Annotations) != want {
			t.Fatalf("%s: created %s, want %s", fleet, g.Annotations, want)
		}
		if got := call(s, http.MethodGet, groupTarget(orgA, fleet, g.ID), nil, ""); !strings.Contains(got.Body.String(), `"annotations":`+want+`,`) {
			t.Fatalf("%s: the group read back %s", fleet, got.Body.String())
		}
		if got := call(s, http.MethodGet, groupsTarget(orgA, fleet), nil, ""); !strings.Contains(got.Body.String(), `"annotations":`+want+`,`) ||
			!strings.Contains(got.Body.String(), `"annotations":null,`) {
			t.Fatalf("%s: the list = %s, want the group's annotations and the Default group's null", fleet, got.Body.String())
		}

		replaced := call(s, http.MethodPatch, groupTarget(orgA, fleet, g.ID), []byte(`{"annotations":{"other":{"b":2,"a":1}}}`), jsonType)
		expectStatus(t, "replace", replaced, http.StatusOK)
		decodeInto(t, replaced.Body.Bytes(), &g)
		if string(g.Annotations) != `{"other":{"a":1,"b":2}}` {
			t.Fatalf("%s: replaced %s, want the object whole", fleet, g.Annotations)
		}
		renamed := call(s, http.MethodPatch, groupTarget(orgA, fleet, g.ID), []byte(`{"name":"edges"}`), jsonType)
		expectStatus(t, "rename", renamed, http.StatusOK)
		decodeInto(t, renamed.Body.Bytes(), &g)
		if string(g.Annotations) != `{"other":{"a":1,"b":2}}` {
			t.Fatalf("%s: a rename left %s, want the annotations kept", fleet, g.Annotations)
		}
		cleared := call(s, http.MethodPatch, groupTarget(orgA, fleet, g.ID), []byte(`{"annotations":null}`), jsonType)
		expectStatus(t, "clear", cleared, http.StatusOK)
		if !strings.Contains(cleared.Body.String(), `"annotations":null,`) {
			t.Fatalf("%s: cleared %s", fleet, cleared.Body.String())
		}
		defaultID := groupID(t, s, orgA, fleet, "Default")
		onDefault := call(s, http.MethodPatch, groupTarget(orgA, fleet, defaultID), []byte(`{"annotations":{}}`), jsonType)
		expectStatus(t, "the Default group's", onDefault, http.StatusOK)
		if !strings.Contains(onDefault.Body.String(), `"annotations":{},`) {
			t.Fatalf("%s: the Default group's %s", fleet, onDefault.Body.String())
		}

		before := dump(t, s, orgA)
		// The bound is the database's text: a space after the colon.
		over := `{"a":"` + strings.Repeat("v", 16384-8) + `"}`
		for _, annotations := range []string{`[]`, `["telflo"]`, `"telflo"`, `1`, `true`, `{"n":1e3}`, `{"n":-0}`, `{"a":"\u0000"}`, over} {
			expectError(t, "create, annotations "+annotations[:min(len(annotations), 12)],
				call(s, http.MethodPost, groupsTarget(orgA, fleet), []byte(`{"name":"refused","annotations":`+annotations+`}`), jsonType),
				http.StatusBadRequest, codeInvalidRequest, messageAnnotations)
			expectError(t, "update, annotations "+annotations[:min(len(annotations), 12)],
				call(s, http.MethodPatch, groupTarget(orgA, fleet, defaultID), []byte(`{"annotations":`+annotations+`}`), jsonType),
				http.StatusBadRequest, codeInvalidRequest, messageAnnotations)
		}
		if after := dump(t, s, orgA); after != before {
			t.Fatalf("%s: the refusals changed rows:\n%s\n%s", fleet, before, after)
		}
		// At the bound, it is stored.
		atBound := `{"a":"` + strings.Repeat("v", 16384-9) + `"}`
		expectStatus(t, "annotations at the bound", call(s, http.MethodPatch, groupTarget(orgA, fleet, defaultID),
			[]byte(`{"annotations":`+atBound+`}`), jsonType), http.StatusOK)
	}
}

// messageUnknownField is decodeObjectWithOptional's answer for a field the
// route doesn't take.
const messageUnknownField = "The request body has a field this API doesn't know."

// A group has no role, namespace, replicas or volumes in the API (their
// data lives in the platform's annotations, as any client's). Each, set or
// null, is an unknown field on a create and an update, on either fleet
// type's group, and changes nothing; no reply carries them; a Kubernetes
// group's fields under annotations are stored and answered as any
// annotations.
func TestTheOldKubernetesFieldsAreUnknownFields(t *testing.T) {
	s, _, _ := fleetServer(t)
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	createFleet(t, s, orgA, fleetA2, "edge-k8s", "kubernetes-supervisor")
	for _, fleet := range []string{fleetA, fleetA2} {
		defaultID := groupID(t, s, orgA, fleet, "Default")
		before := dump(t, s, orgA)
		for _, field := range []string{`"role":"gateway"`, `"role":null`, `"namespace":"telemetry"`, `"namespace":null`,
			`"replicas":3`, `"replicas":null`, `"volumes":[]`, `"volumes":null`} {
			expectError(t, fleet+": create with "+field,
				call(s, http.MethodPost, groupsTarget(orgA, fleet), []byte(`{"name":"nodes",`+field+`}`), jsonType),
				http.StatusBadRequest, codeInvalidRequest, messageUnknownField)
			expectError(t, fleet+": update with "+field,
				call(s, http.MethodPatch, groupTarget(orgA, fleet, defaultID), []byte(`{`+field+`}`), jsonType),
				http.StatusBadRequest, codeInvalidRequest, messageUnknownField)
			expectError(t, fleet+": update with "+field+" beside annotations",
				call(s, http.MethodPatch, groupTarget(orgA, fleet, defaultID), []byte(`{"annotations":{},`+field+`}`), jsonType),
				http.StatusBadRequest, codeInvalidRequest, messageUnknownField)
		}
		if after := dump(t, s, orgA); after != before {
			t.Fatalf("%s: the refusals changed rows:\n%s\n%s", fleet, before, after)
		}
	}

	const (
		sent = `{"telflo": {"kubernetes": {"role": "gateway", "namespace": "telemetry", "replicas": 3,` +
			` "volumes": [{"readOnly": true, "mountPath": "/var/log/journal", "hostPathType": "Directory", "hostPath": "/var/log/journal"}]}}}`
		want = `{"telflo":{"kubernetes":{"namespace":"telemetry","replicas":3,"role":"gateway",` +
			`"volumes":[{"hostPath":"/var/log/journal","hostPathType":"Directory","mountPath":"/var/log/journal","readOnly":true}]}}}`
	)
	created := call(s, http.MethodPost, groupsTarget(orgA, fleetA2), []byte(`{"name":"traces-sampling","annotations":`+sent+`}`), jsonType)
	expectStatus(t, "a Kubernetes group with its fields in annotations", created, http.StatusCreated)
	var g groupReply
	decodeInto(t, created.Body.Bytes(), &g)
	if string(g.Annotations) != want {
		t.Fatalf("created %s, want %s", g.Annotations, want)
	}
	updated := call(s, http.MethodPatch, groupTarget(orgA, fleetA2, g.ID), []byte(`{"annotations":`+sent+`}`), jsonType)
	expectStatus(t, "the same annotations again", updated, http.StatusOK)
	for _, reply := range []string{created.Body.String(), updated.Body.String(),
		call(s, http.MethodGet, groupTarget(orgA, fleetA2, g.ID), nil, "").Body.String(),
		call(s, http.MethodGet, groupsTarget(orgA, fleetA2), nil, "").Body.String()} {
		var objects []map[string]json.RawMessage
		var one map[string]json.RawMessage
		var list struct {
			Groups []map[string]json.RawMessage `json:"groups"`
		}
		if json.Unmarshal([]byte(reply), &list) == nil && list.Groups != nil {
			objects = list.Groups
		} else if err := json.Unmarshal([]byte(reply), &one); err == nil {
			objects = []map[string]json.RawMessage{one}
		} else {
			t.Fatalf("a reply that isn't JSON: %s", reply)
		}
		for _, object := range objects {
			for _, field := range []string{"role", "namespace", "replicas", "volumes"} {
				if _, found := object[field]; found {
					t.Fatalf("a reply carries %s: %s", field, reply)
				}
			}
		}
	}
}

// The group routes: create with an assignment, update and clear, the
// Default group kept, delete moving nothing else; another organization's
// published version is a 422 publication_not_found.
func TestCollectorGroupRoutes(t *testing.T) {
	s, _, _ := fleetServer(t)
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	createFleet(t, s, orgA, fleetA2, "edge-k8s", "kubernetes-supervisor")
	expectStatus(t, "A's published version", publish(s, orgA, publicationA, validBody(sampleYAML)), http.StatusCreated)
	theirs := uuid.NewString()
	expectStatus(t, "B's published version", publish(s, orgB, theirs, validBody(sampleYAML)), http.StatusCreated)

	got := call(s, http.MethodPost, groupsTarget(orgA, fleetA), []byte(`{"name":"gateways","assignedPublicationId":"`+publicationA+`"}`), jsonType)
	expectStatus(t, "create", got, http.StatusCreated)
	var g groupReply
	decodeInto(t, got.Body.Bytes(), &g)
	if g.Name != "gateways" || g.IsDefault || g.FleetID != fleetA || g.AssignedPublicationID == nil || *g.AssignedPublicationID != publicationA {
		t.Fatalf("the group = %+v", g)
	}
	expectError(t, "another organization's published version",
		call(s, http.MethodPost, groupsTarget(orgA, fleetA), []byte(`{"name":"other","assignedPublicationId":"`+theirs+`"}`), jsonType),
		http.StatusUnprocessableEntity, codePublicationNotFound, messagePublicationNotFound)
	expectError(t, "the name taken", call(s, http.MethodPost, groupsTarget(orgA, fleetA), []byte(`{"name":"gateways"}`), jsonType),
		http.StatusConflict, codeCollectorGroupNameTaken, messageCollectorGroupNameTaken)
	// No control character in a group's name, as a collector's.
	for _, name := range []string{`gate\nways`, `gate\tways`, `gate\u007fways`, `gate\u009bways`} {
		expectError(t, "a name with a control character", call(s, http.MethodPost, groupsTarget(orgA, fleetA),
			[]byte(`{"name":"`+name+`"}`), jsonType), http.StatusBadRequest, codeInvalidRequest, messageGroupName)
		expectError(t, "a rename with a control character", call(s, http.MethodPatch, groupTarget(orgA, fleetA, g.ID),
			[]byte(`{"name":"`+name+`"}`), jsonType), http.StatusBadRequest, codeInvalidRequest, messageGroupName)
	}

	updated := call(s, http.MethodPatch, groupTarget(orgA, fleetA, g.ID), []byte(`{"name":"edges","assignedPublicationId":null}`), jsonType)
	expectStatus(t, "update", updated, http.StatusOK)
	decodeInto(t, updated.Body.Bytes(), &g)
	if g.Name != "edges" || g.AssignedPublicationID != nil {
		t.Fatalf("the updated group = %+v", g)
	}
	defaultID := groupID(t, s, orgA, fleetA, "Default")
	expectError(t, "renaming Default", call(s, http.MethodPatch, groupTarget(orgA, fleetA, defaultID), []byte(`{"name":"Main"}`), jsonType),
		http.StatusConflict, codeCollectorGroupIsDefault, messageCollectorGroupIsDefault)
	expectStatus(t, "assigning Default", call(s, http.MethodPatch, groupTarget(orgA, fleetA, defaultID),
		[]byte(`{"name":"Default","assignedPublicationId":"`+publicationA+`"}`), jsonType), http.StatusOK)
	expectError(t, "deleting Default", call(s, http.MethodDelete, groupTarget(orgA, fleetA, defaultID), nil, ""),
		http.StatusConflict, codeCollectorGroupIsDefault, messageCollectorGroupIsDefault)

	var list groupListReply
	decodeInto(t, call(s, http.MethodGet, groupsTarget(orgA, fleetA), nil, "").Body.Bytes(), &list)
	if len(list.Groups) != 2 || !list.Groups[0].IsDefault || list.Groups[1].Name != "edges" {
		t.Fatalf("the groups = %+v", list.Groups)
	}
	expectStatus(t, "delete", call(s, http.MethodDelete, groupTarget(orgA, fleetA, g.ID), nil, ""), http.StatusNoContent)
	expectError(t, "the deleted group", call(s, http.MethodGet, groupTarget(orgA, fleetA, g.ID), nil, ""),
		http.StatusNotFound, codeCollectorGroupNotFound, messageCollectorGroupNotFound)
	expectError(t, "a group of an unknown fleet", call(s, http.MethodGet, groupTarget(orgA, unknownID, g.ID), nil, ""),
		http.StatusNotFound, codeFleetNotFound, messageFleetNotFound)
}

// Through the API, a rotation answers the new token's bearer and its
// predecessor, writes `rotated` and `issued`, and the predecessor is pushed
// its successor (doRefresh) until its grace ends; an archived fleet's
// tokens can't be rotated.
func TestRotateAnswersTheNewBearerAndAudits(t *testing.T) {
	s, clock, _ := fleetServer(t)
	c := createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	clock.Advance(time.Minute)
	got := call(s, http.MethodPost, rotateTarget(orgA, fleetA), rotationBody(nil, "2026-10-04T15:00:00.000Z").bytes(), jsonType)
	expectStatus(t, "rotate", got, http.StatusCreated)
	var r rotatedReply
	decodeInto(t, got.Body.Bytes(), &r)
	if claims := readClaims(t, r.Bearer); claims.TokenID.String() != r.ID || claims.FleetID.String() != fleetA {
		t.Fatalf("the rotated bearer carries %+v", claims)
	}
	if r.ExpiresAt != nil || r.PredecessorID == nil || *r.PredecessorID != c.Tokens.Rotate.ID {
		t.Fatalf("the rotation = %+v", r)
	}
	events := tokenEvents(t, s, orgA, fleetA)
	kinds := map[enrollmenttokens.EventKind]string{}
	for _, e := range events[:2] {
		kinds[e.Kind] = e.TokenID.String()
		if e.Actor == nil || e.Actor.Email != sampleEmail {
			t.Fatalf("a rotation event's actor = %+v", e.Actor)
		}
	}
	if kinds[enrollmenttokens.EventRotated] != c.Tokens.Rotate.ID || kinds[enrollmenttokens.EventIssued] != r.ID {
		t.Fatalf("the rotation's events = %v", kinds)
	}
	var list enrollmentTokenListReply
	decodeInto(t, call(s, http.MethodGet, tokensTarget(orgA, fleetA), nil, "").Body.Bytes(), &list)
	if len(list.Tokens) != 3 || list.Tokens[0].ID != r.ID {
		t.Fatalf("the tokens = %+v", list.Tokens)
	}
	for _, l := range list.Tokens {
		if l.ID == c.Tokens.Rotate.ID && (!l.DoRefresh || l.ExpiresAt == nil || *l.ExpiresAt != "2026-10-04T15:00:00.000Z") {
			t.Fatalf("the predecessor = %+v", l)
		}
	}

	archiveSeed(orgA, fleetA)(t, s)
	expectError(t, "an archived fleet", call(s, http.MethodPost, rotateTarget(orgA, fleetA), rotationBody(nil, "2026-10-04T15:00:00.000Z").bytes(), jsonType),
		http.StatusConflict, codeFleetArchived, messageFleetArchived)
	expectError(t, "a grace end after the expiry",
		call(s, http.MethodPost, rotateTarget(orgA, fleetA), rotationBody("2026-10-04T00:00:00.000Z", "2026-10-05T00:00:00.000Z").bytes(), jsonType),
		http.StatusBadRequest, codeInvalidRequest, "graceEndsAt must not be after expiresAt.")
}

// A fleet's reads (one fleet and the list alike) answer its collectors'
// latest last seen (null with none) and its rotate tokens' state at the
// server's clock: the current token's expiry, the grace of the one it
// replaced, the count of valid ones; the current token revoked mid-grace
// leaves its predecessor current with no grace shown; no valid rotate
// token is null. Another organization's fleets change nothing.
func TestTheFleetReadsAnswerLastSeenAndTheRotateTokensState(t *testing.T) {
	s, clock, _ := fleetServer(t)
	c := createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	createFleet(t, s, orgB, fleetB, "globex", "server")
	check := func(label string, want summaryReply) {
		t.Helper()
		if got := fleetSummaryOf(t, s, orgA, fleetA); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: the summary is %s, want %s", label, describeSummary(got), describeSummary(want))
		}
	}
	check("a new fleet", summaryReply{RotateToken: &rotateTokenReply{ExpiresAt: ptr(futureTime), ValidCount: 1}})

	// The latest is neither the last enrolled nor another organization's.
	clock.Advance(2 * time.Minute)
	latest := clock.Now()
	enrollCollector(t, s, orgA, fleetA, "edge-01")
	clock.Advance(-time.Minute)
	enrollCollector(t, s, orgA, fleetA, "edge-02")
	clock.Advance(2 * time.Minute)
	enrollCollector(t, s, orgB, fleetB, "globex-01")
	want := summaryReply{Collectors: 2, Warning: 1, Unknown: 1, Unassigned: 2, LastSeenAt: ptr(formatTime(latest))}
	want.RotateToken = &rotateTokenReply{ExpiresAt: ptr(futureTime), ValidCount: 1}
	check("two collectors", want)

	got := call(s, http.MethodPost, rotateTarget(orgA, fleetA), rotationBody(nil, "2026-10-04T15:00:00.000Z").bytes(), jsonType)
	expectStatus(t, "rotate", got, http.StatusCreated)
	var r rotatedReply
	decodeInto(t, got.Body.Bytes(), &r)
	want.RotateToken = &rotateTokenReply{GraceEndsAt: ptr("2026-10-04T15:00:00.000Z"), ValidCount: 2}
	check("in the grace", want)

	expectStatus(t, "revoke the new token", call(s, http.MethodPost, revokeTarget(orgA, fleetA, r.ID), actorBody().bytes(), jsonType), http.StatusOK)
	want.RotateToken = &rotateTokenReply{ExpiresAt: ptr("2026-10-04T15:00:00.000Z"), ValidCount: 1}
	check("the new token revoked mid-grace", want)

	expectStatus(t, "revoke the old token", call(s, http.MethodPost, revokeTarget(orgA, fleetA, c.Tokens.Rotate.ID), actorBody().bytes(), jsonType), http.StatusOK)
	want.RotateToken = nil
	check("no valid rotate token", want)

	// Null, not left out.
	var raw struct {
		Fleets []struct {
			Summary map[string]any `json:"summary"`
		} `json:"fleets"`
	}
	decodeInto(t, call(s, http.MethodGet, fleetsTarget(orgA), nil, "").Body.Bytes(), &raw)
	if value, present := raw.Fleets[0].Summary["rotateToken"]; len(raw.Fleets) != 1 || !present || value != nil {
		t.Fatalf("rotateToken is %v (present %v), want null", value, present)
	}
}

// A failing tokens store fails the fleet's reads, which read the rotate
// tokens' state after the summaries: 500, the log naming the store.
func TestAFailingTokensStoreFailsTheFleetReads(t *testing.T) {
	d := testDB(t)
	registry := organizations.NewPostgres(d.Runtime)
	register(t, registry, orgA, orgB)
	stores := postgresFleetStores(d.Runtime)
	stores.tokens = failingTokens{}
	s, _, log := newServerWith(t, registry, publications.NewPostgres(d.Runtime), stores)
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	for _, target := range []string{fleetsTarget(orgA), fleetTarget(orgA, fleetA)} {
		expectError(t, target, call(s, http.MethodGet, target, nil, ""), http.StatusInternalServerError, codeInternalError, messageInternalError)
	}
	if !strings.Contains(log.String(), `"msg":"the enrollment tokens store failed"`) || strings.Contains(log.String(), errStoreDown.Error()) {
		t.Fatalf("the log: %s", log.String())
	}
}

// A reveal answers a fresh bearer (another nonce each time) for a valid
// token and writes `viewed`; a revoked or expired token is a 409 writing
// nothing. A revoke writes `revoked` once: revoking again answers the token
// as it is, with no event.
func TestRevealAndRevoke(t *testing.T) {
	s, clock, _ := fleetServer(t)
	c := createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	reveal := func() *revealedReply {
		got := call(s, http.MethodPost, revealTarget(orgA, fleetA, c.Tokens.Rotate.ID), actorBody().bytes(), jsonType)
		if got.Code != http.StatusOK {
			return nil
		}
		var r revealedReply
		decodeInto(t, got.Body.Bytes(), &r)
		return &r
	}
	first, second := reveal(), reveal()
	if first == nil || second == nil || first.Bearer == second.Bearer || first.Bearer == c.Tokens.Rotate.Bearer {
		t.Fatal("two reveals didn't answer two fresh bearers")
	}
	if claims := readClaims(t, first.Bearer); claims.TokenID.String() != c.Tokens.Rotate.ID || claims.FleetID.String() != fleetA ||
		first.Kind != "rotate" || first.ExpiresAt == nil || *first.ExpiresAt != futureTime {
		t.Fatalf("the reveal = %+v, carrying %+v", first, claims)
	}
	viewed := 0
	for _, e := range tokenEvents(t, s, orgA, fleetA) {
		if e.Kind == enrollmenttokens.EventViewed && e.TokenID.String() == c.Tokens.Rotate.ID && e.Actor != nil {
			viewed++
		}
	}
	if viewed != 2 {
		t.Fatalf("%d viewed events, want 2", viewed)
	}

	revoked := call(s, http.MethodPost, revokeTarget(orgA, fleetA, c.Tokens.Rotate.ID), actorBody().bytes(), jsonType)
	expectStatus(t, "revoke", revoked, http.StatusOK)
	var tok enrollmentTokenReply
	decodeInto(t, revoked.Body.Bytes(), &tok)
	if tok.RevokedAt == nil || *tok.RevokedAt != formatTime(clock.Now()) || tok.ID != c.Tokens.Rotate.ID {
		t.Fatalf("the revoked token = %+v", tok)
	}
	events := len(tokenEvents(t, s, orgA, fleetA))
	clock.Advance(time.Hour)
	again := call(s, http.MethodPost, revokeTarget(orgA, fleetA, c.Tokens.Rotate.ID), actorBody().bytes(), jsonType)
	expectStatus(t, "revoke again", again, http.StatusOK)
	if again.Body.String() != revoked.Body.String() || len(tokenEvents(t, s, orgA, fleetA)) != events {
		t.Fatalf("revoking again changed something: %s", again.Body.String())
	}
	expectError(t, "reveal a revoked token", call(s, http.MethodPost, revealTarget(orgA, fleetA, c.Tokens.Rotate.ID), actorBody().bytes(), jsonType),
		http.StatusConflict, codeEnrollmentTokenRevoked, messageEnrollmentTokenRevoked)
	if len(tokenEvents(t, s, orgA, fleetA)) != events {
		t.Fatal("a refused reveal wrote an event")
	}

	// The default token may be revoked too.
	expectStatus(t, "revoke the default token", call(s, http.MethodPost, revokeTarget(orgA, fleetA, c.Tokens.Default.ID), []byte(`{"actor":null}`), jsonType), http.StatusOK)

	other := createFleet(t, s, orgA, fleetA2, "edge-dev", "server")
	clock.Advance(2 * 365 * 24 * time.Hour)
	expectError(t, "reveal an expired token", call(s, http.MethodPost, revealTarget(orgA, fleetA2, other.Tokens.Rotate.ID), actorBody().bytes(), jsonType),
		http.StatusConflict, codeEnrollmentTokenExpired, messageEnrollmentTokenExpired)
	expectError(t, "reveal an unknown token", call(s, http.MethodPost, revealTarget(orgA, fleetA2, unknownID), actorBody().bytes(), jsonType),
		http.StatusNotFound, codeEnrollmentTokenNotFound, messageEnrollmentTokenNotFound)
	expectError(t, "another fleet's token", call(s, http.MethodPost, revealTarget(orgA, fleetA2, c.Tokens.Default.ID), actorBody().bytes(), jsonType),
		http.StatusNotFound, codeEnrollmentTokenNotFound, messageEnrollmentTokenNotFound)
}

// POST …/tokens/default answers 409 enrollment_token_default_exists while
// the fleet's default token is valid, writing nothing; once it is revoked,
// 201 with a new default token's bearer (no-store, the reveal's shape) and
// an `issued` event naming the actor, the revoked one staying revoked;
// then 409 again. Of two issues racing, one makes the token and the other
// answers 409; an archived fleet answers fleet_archived first; an unknown
// fleet 404.
func TestIssueDefaultFollowsARevokedDefaultTokenAndAudits(t *testing.T) {
	s, clock, _ := fleetServer(t)
	c := createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	issue := func() *httptest.ResponseRecorder {
		return call(s, http.MethodPost, issueDefaultTarget(orgA, fleetA), actorBody().bytes(), jsonType)
	}
	before := dump(t, s, orgA)
	expectError(t, "a valid default token", issue(), http.StatusConflict, codeEnrollmentTokenDefaultExists, messageDefaultTokenExists)
	if dump(t, s, orgA) != before {
		t.Fatal("a refused issue changed something")
	}

	expectStatus(t, "revoke the default token",
		call(s, http.MethodPost, revokeTarget(orgA, fleetA, c.Tokens.Default.ID), []byte(`{"actor":null}`), jsonType), http.StatusOK)
	clock.Advance(time.Minute)
	got := issue()
	expectStatus(t, "issue after the revoke", got, http.StatusCreated)
	if value := got.Header().Get("Cache-Control"); value != "no-store" {
		t.Fatalf("Cache-Control %q, want no-store", value)
	}
	var r revealedReply
	decodeInto(t, got.Body.Bytes(), &r)
	if claims := readClaims(t, r.Bearer); claims.TokenID.String() != r.ID || claims.FleetID.String() != fleetA ||
		r.Kind != "default" || r.ExpiresAt != nil || r.ID == c.Tokens.Default.ID {
		t.Fatalf("the new default token = %+v, carrying %+v", r, claims)
	}
	if newest := tokenEvents(t, s, orgA, fleetA)[0]; newest.Kind != enrollmenttokens.EventIssued || newest.TokenID.String() != r.ID ||
		newest.Actor == nil || newest.Actor.Email != sampleEmail || formatTime(newest.OccurredAt) != formatTime(clock.Now()) {
		t.Fatalf("the newest event = %+v", newest)
	}
	var list enrollmentTokenListReply
	decodeInto(t, call(s, http.MethodGet, tokensTarget(orgA, fleetA), nil, "").Body.Bytes(), &list)
	states := map[string]bool{}
	for _, l := range list.Tokens {
		if l.Kind == "default" {
			states[l.ID] = l.RevokedAt != nil
		}
	}
	if len(states) != 2 || !states[c.Tokens.Default.ID] || states[r.ID] {
		t.Fatalf("the default tokens (id: revoked) = %v", states)
	}
	events := len(tokenEvents(t, s, orgA, fleetA))
	expectError(t, "a second issue", issue(), http.StatusConflict, codeEnrollmentTokenDefaultExists, messageDefaultTokenExists)
	if len(tokenEvents(t, s, orgA, fleetA)) != events {
		t.Fatal("a refused issue wrote an event")
	}

	// Two racing: one 201, one 409.
	expectStatus(t, "revoke the new default token",
		call(s, http.MethodPost, revokeTarget(orgA, fleetA, r.ID), actorBody().bytes(), jsonType), http.StatusOK)
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() { codes[i] = issue().Code })
	}
	wg.Wait()
	if slices.Sort(codes); !slices.Equal(codes, []int{http.StatusCreated, http.StatusConflict}) {
		t.Fatalf("two racing issues answered %v, want one 201 and one 409", codes)
	}

	archiveSeed(orgA, fleetA)(t, s)
	expectError(t, "an archived fleet", issue(), http.StatusConflict, codeFleetArchived, messageFleetArchived)
	expectError(t, "an unknown fleet", call(s, http.MethodPost, issueDefaultTarget(orgA, unknownID), actorBody().bytes(), jsonType),
		http.StatusNotFound, codeFleetNotFound, messageFleetNotFound)
}

// GET …/tokens/events: the fleet's events newest first, each its id, its
// token's id, its kind, its actor (null for none) and its time, never a
// bearer; a page of one and its cursor reach the rest; limit and cursor
// refused 400 before the store; another fleet's events never listed.
func TestListTokenEventsPagesTheFleetsEventsNewestFirst(t *testing.T) {
	s, clock, _ := fleetServer(t)
	c := createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	createFleet(t, s, orgA, fleetA2, "edge-dev", "server")
	clock.Advance(time.Minute)
	revealed := call(s, http.MethodPost, revealTarget(orgA, fleetA, c.Tokens.Default.ID), actorBody().bytes(), jsonType)
	expectStatus(t, "reveal", revealed, http.StatusOK)
	clock.Advance(time.Minute)
	rotated := call(s, http.MethodPost, rotateTarget(orgA, fleetA), rotationBody(nil, formatTime(clock.Now().Add(time.Hour))).bytes(), jsonType)
	expectStatus(t, "rotate", rotated, http.StatusCreated)
	clock.Advance(time.Minute)
	expectStatus(t, "revoke", call(s, http.MethodPost, revokeTarget(orgA, fleetA, c.Tokens.Rotate.ID), []byte(`{"actor":null}`), jsonType), http.StatusOK)

	got := call(s, http.MethodGet, tokenEventsTarget(orgA, fleetA), nil, "")
	expectStatus(t, "list", got, http.StatusOK)
	var whole tokenEventListReply
	decodeInto(t, got.Body.Bytes(), &whole)
	kinds := []string{}
	for _, e := range whole.Events {
		kinds = append(kinds, e.Event)
	}
	if strings.Join(kinds, " ") != "revoked rotated issued viewed issued issued" && strings.Join(kinds, " ") != "revoked issued rotated viewed issued issued" ||
		whole.NextCursor != nil {
		t.Fatalf("the events = %s", got.Body.String())
	}
	if whole.Events[0].Actor != nil || whole.Events[0].TokenID != c.Tokens.Rotate.ID || whole.Events[3].Actor == nil ||
		whole.Events[3].TokenID != c.Tokens.Default.ID || whole.Events[0].OccurredAt != formatTime(clock.Now()) {
		t.Fatalf("the events = %s", got.Body.String())
	}
	var bearers revealedReply
	decodeInto(t, revealed.Body.Bytes(), &bearers)
	for _, bearer := range []string{bearers.Bearer, c.Tokens.Default.Bearer, c.Tokens.Rotate.Bearer} {
		if strings.Contains(got.Body.String(), bearer) {
			t.Fatal("an event list carries a bearer")
		}
	}

	var joined []tokenEventReply
	target := tokenEventsTarget(orgA, fleetA) + "?limit=1"
	for i := 0; ; i++ {
		if i > len(whole.Events) {
			t.Fatal("no last page")
		}
		page := call(s, http.MethodGet, target, nil, "")
		expectStatus(t, "a page of one", page, http.StatusOK)
		var p tokenEventListReply
		decodeInto(t, page.Body.Bytes(), &p)
		joined = append(joined, p.Events...)
		if p.NextCursor == nil {
			break
		}
		target = tokenEventsTarget(orgA, fleetA) + "?limit=1&cursor=" + *p.NextCursor
	}
	joinedJSON, _ := json.Marshal(joined)
	wholeJSON, _ := json.Marshal(whole.Events)
	if string(joinedJSON) != string(wholeJSON) {
		t.Fatalf("pages of one = %v, want %v", joined, whole.Events)
	}

	for _, query := range []string{"?limit=0", "?limit=101", "?limit=x", "?cursor=x", "?limit=1&limit=2"} {
		got := call(s, http.MethodGet, tokenEventsTarget(orgA, fleetA)+query, nil, "")
		if got.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", query, got.Code, got.Body.String())
		}
	}
	expectError(t, "an unknown fleet", call(s, http.MethodGet, tokenEventsTarget(orgA, unknownID), nil, ""),
		http.StatusNotFound, codeFleetNotFound, messageFleetNotFound)
	var others tokenEventListReply
	decodeInto(t, call(s, http.MethodGet, tokenEventsTarget(orgA, fleetA2), nil, "").Body.Bytes(), &others)
	if len(others.Events) != 2 {
		t.Fatalf("the other fleet's events = %+v", others)
	}
}

// fleetRoute is one of the fleet model's routes, named for a test.
type fleetRoute struct {
	name, method string
	target       func(organizationID, fleetID, groupID, tokenID string) string
	body         body
}

// fleetRoutes are all sixteen, each with a body it accepts.
func fleetRoutes() []fleetRoute {
	return []fleetRoute{
		{"list fleets", http.MethodGet, func(o, _, _, _ string) string { return fleetsTarget(o) }, nil},
		{"create fleet", http.MethodPost, func(o, _, _, _ string) string { return fleetsTarget(o) }, fleetBody(fleetA, "edge-prod", "server")},
		{"get fleet", http.MethodGet, func(o, f, _, _ string) string { return fleetTarget(o, f) }, nil},
		{"patch fleet", http.MethodPatch, func(o, f, _, _ string) string { return fleetTarget(o, f) }, body{"label": "renamed", "status": "archived"}},
		{"delete fleet", http.MethodDelete, func(o, f, _, _ string) string { return fleetTarget(o, f) }, nil},
		{"list groups", http.MethodGet, func(o, f, _, _ string) string { return groupsTarget(o, f) }, nil},
		{"create group", http.MethodPost, func(o, f, _, _ string) string { return groupsTarget(o, f) }, body{"name": "intruders"}},
		{"get group", http.MethodGet, func(o, f, g, _ string) string { return groupTarget(o, f, g) }, nil},
		{"patch group", http.MethodPatch, func(o, f, g, _ string) string { return groupTarget(o, f, g) },
			body{"name": "renamed", "assignedPublicationId": nil, "annotations": map[string]any{"telflo": map[string]any{"dockerMounts": []any{}}}}},
		{"delete group", http.MethodDelete, func(o, f, g, _ string) string { return groupTarget(o, f, g) }, nil},
		{"list tokens", http.MethodGet, func(o, f, _, _ string) string { return tokensTarget(o, f) }, nil},
		{"list token events", http.MethodGet, func(o, f, _, _ string) string { return tokenEventsTarget(o, f) }, nil},
		{"rotate", http.MethodPost, func(o, f, _, _ string) string { return rotateTarget(o, f) }, rotationBody(nil, "2026-10-04T00:00:00.000Z")},
		{"issue default", http.MethodPost, func(o, f, _, _ string) string { return issueDefaultTarget(o, f) }, actorBody()},
		{"reveal", http.MethodPost, func(o, f, _, t string) string { return revealTarget(o, f, t) }, actorBody()},
		{"revoke", http.MethodPost, func(o, f, _, t string) string { return revokeTarget(o, f, t) }, actorBody()},
	}
}

// namesGroupOrToken reports whether the route names a group or a token in
// its path.
func namesGroupOrToken(route string) bool {
	return slices.Contains([]string{"get group", "patch group", "delete group", "reveal", "revoke"}, route)
}

// Every route, called with another organization's ids (its fleet, or this
// organization's fleet with another's group or token), answers a plain 404
// and changes nothing of either organization's; the lists under one
// organization show nothing of the other's.
func TestAnotherOrganizationsIdsAreNotFoundOnEveryRouteAndChangeNothing(t *testing.T) {
	s, _, _ := fleetServer(t)
	theirs := createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	seedGroup(orgA, fleetA, "gateways")(t, s)
	theirGroup := groupID(t, s, orgA, fleetA, "gateways")
	mine := createFleet(t, s, orgB, fleetB, "edge-b", "server")
	beforeA, beforeB := dump(t, s, orgA), dump(t, s, orgB)

	routes := fleetRoutes()
	if len(routes) != 16 {
		t.Fatalf("%d routes, want the sixteen", len(routes))
	}
	for _, r := range routes {
		if r.name == "create fleet" || r.name == "list fleets" {
			continue // they name no fleet: their checks are below
		}
		cases := []struct {
			label, fleet, group, token string
			code                       errorCode
		}{{"A's fleet under B", theirs.Fleet.ID, theirGroup, theirs.Tokens.Rotate.ID, codeFleetNotFound}}
		if namesGroupOrToken(r.name) {
			cases = append(cases, struct {
				label, fleet, group, token string
				code                       errorCode
			}{"A's group or token under B's fleet", mine.Fleet.ID, theirGroup, theirs.Tokens.Rotate.ID, ""})
		}
		for _, ids := range cases {
			got := sendWith(s, r.method, r.target(orgB, ids.fleet, ids.group, ids.token), r.body)
			if got.Code != http.StatusNotFound {
				t.Errorf("%s with %s: %d %s, want 404", r.name, ids.label, got.Code, got.Body.String())
				continue
			}
			var reply envelope
			decodeInto(t, got.Body.Bytes(), &reply)
			want := ids.code
			if want == "" {
				want = codeCollectorGroupNotFound
				if r.name == "reveal" || r.name == "revoke" {
					want = codeEnrollmentTokenNotFound
				}
			}
			if reply.Error.Code != string(want) {
				t.Errorf("%s with %s: %s, want %s", r.name, ids.label, reply.Error.Code, want)
			}
		}
	}
	if after := dump(t, s, orgA); after != beforeA {
		t.Fatalf("B's calls changed A's rows:\n%s\n%s", beforeA, after)
	}
	// B's fleet routes with A's group or token (under B's own fleet) answer
	// 404 and leave B's rows too.
	if after := dump(t, s, orgB); after != beforeB {
		t.Fatalf("the calls changed B's rows:\n%s\n%s", beforeB, after)
	}

	var page fleetListReply
	decodeInto(t, call(s, http.MethodGet, fleetsTarget(orgB), nil, "").Body.Bytes(), &page)
	if len(page.Fleets) != 1 || page.Fleets[0].ID != fleetB {
		t.Fatalf("B's list = %+v", page)
	}
	// B creating a fleet with A's id is a conflict that stores nothing and
	// says nothing about A's.
	expectError(t, "B creating A's id", sendWith(s, http.MethodPost, fleetsTarget(orgB), fleetBody(fleetA, "edge-b2", "server")),
		http.StatusConflict, codeFleetConflict, messageFleetConflict)
	if after := dump(t, s, orgA); after != beforeA {
		t.Fatal("B's create changed A's rows")
	}
}

// Every route answers 404 organization_not_found under an unregistered
// organization, before it reads anything else, and stores nothing.
func TestEveryFleetRouteAnswersOrganizationNotFoundFirst(t *testing.T) {
	s, _, _ := fleetServer(t)
	for _, r := range fleetRoutes() {
		got := sendWith(s, r.method, r.target(orgC, fleetA, unknownID, unknownID), r.body)
		expectError(t, r.name, got, http.StatusNotFound, codeOrganizationNotFound, messageOrganizationNotFound)
		// Before the body: a body that isn't JSON doesn't change the answer.
		if r.body != nil {
			got = call(s, r.method, r.target(orgC, fleetA, unknownID, unknownID), []byte("not json"), jsonType)
			expectError(t, r.name+" with a bad body", got, http.StatusNotFound, codeOrganizationNotFound, messageOrganizationNotFound)
		}
	}
	if strings.Contains(dump(t, s, orgC), "fleets ") {
		t.Fatal("something was stored under an unregistered organization")
	}
}

// A store that fails answers 500 internal_error on every route, and the log
// line names the route's pattern, the store and nothing else.
func TestAFailingFleetStoreAnswers500AndLogsNoDetail(t *testing.T) {
	d := testDB(t)
	registry := organizations.NewPostgres(d.Runtime)
	register(t, registry, orgA, orgB)
	s, _, log := newServerWith(t, registry, publications.NewPostgres(d.Runtime), fleetStores{fleets: failingFleets{}, groups: failingGroups{}, tokens: failingTokens{},
		collectors: failingCollectors{}, variables: failingVariables{}})
	for _, r := range fleetRoutes() {
		got := sendWith(s, r.method, r.target(orgA, fleetA, unknownID, unknownID), r.body)
		expectError(t, r.name, got, http.StatusInternalServerError, codeInternalError, messageInternalError)
	}
	for _, line := range logLines(t, log.String()) {
		if line["msg"] == "request" {
			continue
		}
		if !strings.HasPrefix(line["msg"].(string), "the ") || !strings.HasSuffix(line["msg"].(string), " store failed") {
			t.Fatalf("an unexpected log line: %v", line)
		}
		for key := range line {
			if !slices.Contains([]string{"time", "level", "msg", "route"}, key) {
				t.Fatalf("the failure's log line has %q: %v", key, line)
			}
		}
		if !strings.HasPrefix(line["route"].(string), "/api/v1/organizations/{organizationId}/fleets") {
			t.Fatalf("the failure's route = %v", line["route"])
		}
	}
	if strings.Contains(log.String(), errStoreDown.Error()) {
		t.Fatal("the store's error text reached the log")
	}
}

// Keys that can't mint (destroyed, as on exit) answer 500 from a create, a
// rotation, a new default token and a reveal, and the log names the route
// only; a reveal then writes no `viewed`.
func TestABearerThatCannotBeMintedIs500WithoutAnEvent(t *testing.T) {
	s, _, log := fleetServer(t)
	c := createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	expectStatus(t, "revoke the default token",
		call(s, http.MethodPost, revokeTarget(orgA, fleetA, c.Tokens.Default.ID), actorBody().bytes(), jsonType), http.StatusOK)
	events := len(tokenEvents(t, s, orgA, fleetA))
	s.keys.Destroy()
	for _, r := range []struct {
		label, target string
		b             body
	}{
		{"create", fleetsTarget(orgA), fleetBody(fleetA2, "edge-dev", "server")},
		{"rotate", rotateTarget(orgA, fleetA), rotationBody(nil, "2026-10-04T00:00:00.000Z")},
		{"issue default", issueDefaultTarget(orgA, fleetA), actorBody()},
		{"reveal", revealTarget(orgA, fleetA, c.Tokens.Rotate.ID), actorBody()},
	} {
		expectError(t, r.label, sendWith(s, http.MethodPost, r.target, r.b), http.StatusInternalServerError, codeInternalError, messageInternalError)
	}
	if n := len(tokenEvents(t, s, orgA, fleetA)); n != events+3 {
		t.Fatalf("%d events, want the rotation's two, the new default token's issued and no viewed", n-events)
	}
	if !strings.Contains(log.String(), messageMintFailed) {
		t.Fatalf("the log doesn't say a bearer couldn't be minted: %s", log.String())
	}
}

// Every reply carrying a bearer (a create's 201, a rotation's 201, a new
// default token's 201, a reveal's 200) says Cache-Control: no-store, so no
// proxy or client cache keeps the credential; a reply without one (a
// create's retry, the token list) needn't. A reveal on an archived fleet
// is still a view: answered, and not cached either.
func TestRepliesCarryingABearerAreNeverCached(t *testing.T) {
	s, _, _ := fleetServer(t)
	creation := call(s, http.MethodPost, fleetsTarget(orgA), fleetBody(fleetA, "edge-prod", "server").bytes(), jsonType)
	var c created
	decodeInto(t, creation.Body.Bytes(), &c)
	check := func(label string, got *httptest.ResponseRecorder, status int, noStore bool) {
		t.Helper()
		expectStatus(t, label, got, status)
		if value := got.Header().Get("Cache-Control"); (value == "no-store") != noStore {
			t.Errorf("%s: Cache-Control %q", label, value)
		}
	}
	check("the create", creation, http.StatusCreated, true)
	check("the create's retry", call(s, http.MethodPost, fleetsTarget(orgA), fleetBody(fleetA, "edge-prod", "server").bytes(), jsonType),
		http.StatusOK, false)
	check("the rotation", sendWith(s, http.MethodPost, rotateTarget(orgA, fleetA), rotationBody(nil, "2026-10-04T00:00:00.000Z")),
		http.StatusCreated, true)
	check("the reveal", sendWith(s, http.MethodPost, revealTarget(orgA, fleetA, c.Tokens.Default.ID), actorBody()), http.StatusOK, true)
	other := createFleet(t, s, orgA, fleetA2, "edge-dev", "server")
	check("revoking a default token", sendWith(s, http.MethodPost, revokeTarget(orgA, fleetA2, other.Tokens.Default.ID), actorBody()),
		http.StatusOK, false)
	check("the new default token", sendWith(s, http.MethodPost, issueDefaultTarget(orgA, fleetA2), actorBody()), http.StatusCreated, true)
	check("the new default token's refusal", sendWith(s, http.MethodPost, issueDefaultTarget(orgA, fleetA2), actorBody()),
		http.StatusConflict, false)
	check("the token list", call(s, http.MethodGet, tokensTarget(orgA, fleetA), nil, ""), http.StatusOK, false)
	check("archiving", call(s, http.MethodPatch, fleetTarget(orgA, fleetA), []byte(`{"status":"archived"}`), jsonType), http.StatusOK, false)
	check("the reveal on the archived fleet", sendWith(s, http.MethodPost, revealTarget(orgA, fleetA, c.Tokens.Default.ID), actorBody()),
		http.StatusOK, true)
}
