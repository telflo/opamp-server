package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/telflo-opamp-server/store/collectorgroups"
	"github.com/telflo/telflo-opamp-server/store/enrollmenttokens"
	"github.com/telflo/telflo-opamp-server/store/fleets"
)

// The fleet routes' fixture ids: fleet A is organization A's, fleet B
// organization B's.
const (
	fleetA        = "5a5a5a5a-0000-4000-8000-00000000005a"
	fleetA2       = "5a5a5a5a-0000-4000-8000-0000000005a2"
	fleetB        = "5b5b5b5b-0000-4000-8000-00000000005b"
	unknownID     = "9f9f9f9f-0000-4000-8000-00000000009f"
	futureTime    = "2027-10-03T00:00:00.000Z"
	sampleEmail   = "ana@acme.io"
	sampleActorID = actorA
)

func fleetsTarget(organizationID string) string {
	return "/api/v1/organizations/" + organizationID + "/fleets"
}

func fleetTarget(organizationID, fleetID string) string {
	return fleetsTarget(organizationID) + "/" + fleetID
}

func groupsTarget(organizationID, fleetID string) string {
	return fleetTarget(organizationID, fleetID) + "/groups"
}

func groupTarget(organizationID, fleetID, groupID string) string {
	return groupsTarget(organizationID, fleetID) + "/" + groupID
}

func tokensTarget(organizationID, fleetID string) string {
	return fleetTarget(organizationID, fleetID) + "/tokens"
}

func tokenEventsTarget(organizationID, fleetID string) string {
	return tokensTarget(organizationID, fleetID) + "/events"
}

func rotateTarget(organizationID, fleetID string) string {
	return tokensTarget(organizationID, fleetID) + "/rotate"
}

func issueDefaultTarget(organizationID, fleetID string) string {
	return tokensTarget(organizationID, fleetID) + "/default"
}

func revealTarget(organizationID, fleetID, tokenID string) string {
	return tokensTarget(organizationID, fleetID) + "/" + tokenID + "/reveal"
}

func revokeTarget(organizationID, fleetID, tokenID string) string {
	return tokensTarget(organizationID, fleetID) + "/" + tokenID + "/revoke"
}

func actorJSONBody() map[string]any {
	return map[string]any{"id": sampleActorID, "email": sampleEmail}
}

// fleetBody is a create's body: the id (none when ""), the label and type,
// a rotate token expiring in 2027, and Ana as the actor.
func fleetBody(id, label, typ string) body {
	b := body{"label": label, "type": typ, "rotateExpiresAt": futureTime, "actor": actorJSONBody()}
	if id != "" {
		b["id"] = id
	}
	return b
}

func rotationBody(expiresAt any, graceEndsAt string) body {
	return body{"expiresAt": expiresAt, "graceEndsAt": graceEndsAt, "actor": actorJSONBody()}
}

func actorBody() body {
	return body{"actor": actorJSONBody()}
}

// created is a create's 201, decoded.
type created struct {
	Fleet struct {
		ID     string `json:"id"`
		Label  string `json:"label"`
		Status string `json:"status"`
	} `json:"fleet"`
	DefaultGroup struct {
		ID string `json:"id"`
	} `json:"defaultGroup"`
	Tokens struct {
		Default struct {
			ID     string `json:"id"`
			Bearer string `json:"bearer"`
		} `json:"default"`
		Rotate struct {
			ID        string  `json:"id"`
			Bearer    string  `json:"bearer"`
			ExpiresAt *string `json:"expiresAt"`
		} `json:"rotate"`
	} `json:"tokens"`
}

// createFleet creates a fleet through the API and returns its 201.
func createFleet(t *testing.T, s *Server, organizationID, fleetID, label, typ string) created {
	t.Helper()
	got := call(s, http.MethodPost, fleetsTarget(organizationID), fleetBody(fleetID, label, typ).bytes(), jsonType)
	expectStatus(t, "creating a fleet", got, http.StatusCreated)
	var c created
	if err := json.Unmarshal(got.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func seedFleet(organizationID, fleetID, label, typ string) func(t *testing.T, s *Server) {
	return func(t *testing.T, s *Server) {
		t.Helper()
		createFleet(t, s, organizationID, fleetID, label, typ)
	}
}

func seedGroup(organizationID, fleetID, name string) func(t *testing.T, s *Server) {
	return func(t *testing.T, s *Server) {
		t.Helper()
		got := call(s, http.MethodPost, groupsTarget(organizationID, fleetID), []byte(`{"name":"`+name+`"}`), jsonType)
		expectStatus(t, "creating a group", got, http.StatusCreated)
	}
}

// seeds runs each seed in turn.
func seeds(each ...func(t *testing.T, s *Server)) func(t *testing.T, s *Server) {
	return func(t *testing.T, s *Server) {
		t.Helper()
		for _, seed := range each {
			seed(t, s)
		}
	}
}

// groupID is the id of the fleet's group named name, read from the store.
func groupID(t *testing.T, s *Server, organizationID, fleetID, name string) string {
	t.Helper()
	groups, err := s.groups.List(context.Background(), uuid.MustParse(organizationID), uuid.MustParse(fleetID))
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g.Name == name {
			return g.ID.String()
		}
	}
	t.Fatalf("no group named %q", name)
	return ""
}

// tokenID is the id of the fleet's newest token of kind, read from the
// store.
func tokenID(t *testing.T, s *Server, organizationID, fleetID string, kind enrollmenttokens.Kind) string {
	t.Helper()
	listed, err := s.enrollmentTokens.List(context.Background(), uuid.MustParse(organizationID), uuid.MustParse(fleetID), s.clock())
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range listed {
		if l.Kind == kind {
			return l.ID.String()
		}
	}
	t.Fatalf("no %s token", kind)
	return ""
}

// groupTargetOf is a targetOf for the fleet's group named name.
func groupTargetOf(organizationID, fleetID, name string) func(t *testing.T, s *Server) string {
	return func(t *testing.T, s *Server) string {
		return groupTarget(organizationID, fleetID, groupID(t, s, organizationID, fleetID, name))
	}
}

// tokenTargetOf is a targetOf for the fleet's token of kind, through
// target (revealTarget or revokeTarget).
func tokenTargetOf(organizationID, fleetID string, kind enrollmenttokens.Kind, target func(o, f, t string) string) func(t *testing.T, s *Server) string {
	return func(t *testing.T, s *Server) string {
		return target(organizationID, fleetID, tokenID(t, s, organizationID, fleetID, kind))
	}
}

// revokeSeed revokes the fleet's token of kind.
func revokeSeed(organizationID, fleetID string, kind enrollmenttokens.Kind) func(t *testing.T, s *Server) {
	return func(t *testing.T, s *Server) {
		t.Helper()
		got := call(s, http.MethodPost, revokeTarget(organizationID, fleetID, tokenID(t, s, organizationID, fleetID, kind)), actorBody().bytes(), jsonType)
		expectStatus(t, "revoking", got, http.StatusOK)
	}
}

// rotateSeed rotates the fleet's rotate token by the API.
func rotateSeed(organizationID, fleetID string, expiresAt any, graceEndsAt string) func(t *testing.T, s *Server) {
	return func(t *testing.T, s *Server) {
		t.Helper()
		got := call(s, http.MethodPost, rotateTarget(organizationID, fleetID), rotationBody(expiresAt, graceEndsAt).bytes(), jsonType)
		expectStatus(t, "rotating", got, http.StatusCreated)
	}
}

// laterSeed moves the server's clock on by d (past a token's expiry).
func laterSeed(d time.Duration) func(t *testing.T, s *Server) {
	return func(_ *testing.T, s *Server) {
		now := s.clock().Add(d)
		s.clock = func() time.Time { return now }
	}
}

// archiveSeed archives the fleet.
func archiveSeed(organizationID, fleetID string) func(t *testing.T, s *Server) {
	return func(t *testing.T, s *Server) {
		t.Helper()
		got := call(s, http.MethodPatch, fleetTarget(organizationID, fleetID), []byte(`{"status":"archived"}`), jsonType)
		expectStatus(t, "archiving", got, http.StatusOK)
	}
}

// The fleet model's stores, failing every call, as a database that is down
// would: each route's store failure answers 500 with nothing of the error.
type failingFleets struct{}

func (failingFleets) Create(context.Context, fleets.NewFleet) (fleets.CreateResult, fleets.Creation, error) {
	return 0, fleets.Creation{}, errStoreDown
}
func (failingFleets) List(context.Context, uuid.UUID, fleets.ListOptions) (fleets.Page, error) {
	return fleets.Page{}, errStoreDown
}
func (failingFleets) Get(context.Context, uuid.UUID, uuid.UUID) (fleets.Fleet, error) {
	return fleets.Fleet{}, errStoreDown
}
func (failingFleets) Update(context.Context, uuid.UUID, uuid.UUID, fleets.Changes, time.Time) (fleets.Fleet, error) {
	return fleets.Fleet{}, errStoreDown
}
func (failingFleets) Delete(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	return false, errStoreDown
}
func (failingFleets) TypeAndStatus(context.Context, uuid.UUID, uuid.UUID) (fleets.Type, fleets.Status, error) {
	return "", "", errStoreDown
}

type failingGroups struct{}

func (failingGroups) Create(context.Context, collectorgroups.NewGroup) (collectorgroups.Group, error) {
	return collectorgroups.Group{}, errStoreDown
}
func (failingGroups) List(context.Context, uuid.UUID, uuid.UUID) ([]collectorgroups.Group, error) {
	return nil, errStoreDown
}
func (failingGroups) Get(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (collectorgroups.Group, error) {
	return collectorgroups.Group{}, errStoreDown
}
func (failingGroups) Update(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, collectorgroups.Changes, time.Time) (collectorgroups.Group, error) {
	return collectorgroups.Group{}, errStoreDown
}
func (failingGroups) Delete(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error) {
	return false, errStoreDown
}

type failingTokens struct{}

func (failingTokens) List(context.Context, uuid.UUID, uuid.UUID, time.Time) ([]enrollmenttokens.Listed, error) {
	return nil, errStoreDown
}
func (failingTokens) Get(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (enrollmenttokens.Token, error) {
	return enrollmenttokens.Token{}, errStoreDown
}
func (failingTokens) Rotate(context.Context, enrollmenttokens.Rotation) (enrollmenttokens.RotateResult, error) {
	return enrollmenttokens.RotateResult{}, errStoreDown
}
func (failingTokens) IssueDefault(context.Context, uuid.UUID, uuid.UUID, *fleets.Actor, time.Time) (enrollmenttokens.Token, error) {
	return enrollmenttokens.Token{}, errStoreDown
}
func (failingTokens) Revoke(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, *fleets.Actor, time.Time) (enrollmenttokens.RevokeResult, enrollmenttokens.Token, error) {
	return 0, enrollmenttokens.Token{}, errStoreDown
}
func (failingTokens) RecordViewed(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, *fleets.Actor, time.Time) (enrollmenttokens.Token, error) {
	return enrollmenttokens.Token{}, errStoreDown
}
func (failingTokens) ListEvents(context.Context, uuid.UUID, uuid.UUID, int, *enrollmenttokens.EventCursor) (enrollmenttokens.EventPage, error) {
	return enrollmenttokens.EventPage{}, errStoreDown
}
func (failingTokens) GetActiveRotate(context.Context, uuid.UUID, uuid.UUID, time.Time) (enrollmenttokens.Token, error) {
	return enrollmenttokens.Token{}, errStoreDown
}
func (failingTokens) RotateStates(context.Context, uuid.UUID, []uuid.UUID, time.Time) (map[uuid.UUID]enrollmenttokens.RotateState, error) {
	return nil, errStoreDown
}
func (failingTokens) Lookup(context.Context, uuid.UUID) (enrollmenttokens.Authenticated, error) {
	return enrollmenttokens.Authenticated{}, errStoreDown
}

// fleetServer is apiServer: every Postgres store, organizations A and B
// registered.
func fleetServer(t *testing.T) (*Server, *testClock, *syncBuffer) {
	t.Helper()
	return apiServer(t)
}

// sendWith sends one authenticated request with a body of contentType.
func sendWith(s *Server, method, target string, b body) *httptest.ResponseRecorder {
	var payload []byte
	contentType := ""
	if b != nil {
		payload, contentType = b.bytes(), jsonType
	}
	return call(s, method, target, payload, contentType)
}

// logLines decodes each JSON log line.
func logLines(t *testing.T, log string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		if line == "" {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("log line %q isn't JSON: %v", line, err)
		}
		lines = append(lines, decoded)
	}
	return lines
}

// describeSummary is a summary as its JSON, for a failure message (its
// pointers printed as values).
func describeSummary(m summaryReply) string {
	encoded, err := json.Marshal(m)
	if err != nil {
		return err.Error()
	}
	return string(encoded)
}
