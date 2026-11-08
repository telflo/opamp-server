// Package managementtest is what tests outside package management use to
// set up data through a management API's handler: an organization, a
// published configuration and a fleet whose Default group is assigned it.
// It speaks only the API's HTTP, as the platform does, so it imports
// nothing of the server.
package managementtest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// Actor is the actor every call names.
var Actor = map[string]any{"id": "4a4a4a4a-0000-4000-8000-00000000004a", "email": "ana@acme.io"}

// API sends requests through a management API's handler with a bearer:
// the secret, or an API key.
type API struct {
	Handler http.Handler
	Bearer  string
}

// Call sends one request with a JSON body (none when body is nil) and
// answers the reply.
func (a API) Call(t testing.TB, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var request *http.Request
	if body == nil {
		request = httptest.NewRequest(method, target, nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request = httptest.NewRequest(method, target, bytes.NewReader(encoded))
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+a.Bearer)
	recorder := httptest.NewRecorder()
	a.Handler.ServeHTTP(recorder, request)
	return recorder
}

// expect sends one request and fails the test unless it answers want,
// decoding the reply into into when it isn't nil.
func (a API) expect(t testing.TB, what, method, target string, body any, want int, into any) {
	t.Helper()
	got := a.Call(t, method, target, body)
	if got.Code != want {
		t.Fatalf("%s: %d %s; want %d", what, got.Code, got.Body.String(), want)
	}
	if into != nil {
		if err := json.Unmarshal(got.Body.Bytes(), into); err != nil {
			t.Fatalf("%s: the reply isn't JSON: %v", what, err)
		}
	}
}

// Fleet is a fleet's create reply, decoded: its id, its Default group and
// both tokens' ids and bearers.
type Fleet struct {
	Fleet struct {
		ID string `json:"id"`
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
			ID     string `json:"id"`
			Bearer string `json:"bearer"`
		} `json:"rotate"`
	} `json:"tokens"`
}

// Setup is what SetUp made.
type Setup struct {
	Organization string
	Publication  string
	Fleet        Fleet
}

// SetUp registers an organization, publishes yaml, creates a server fleet
// labelled label and assigns the published version to its Default group.
func (a API) SetUp(t testing.TB, yaml, label string) Setup {
	t.Helper()
	organization := uuid.NewString()
	a.expect(t, "registering an organization", http.MethodPost, "/api/v1/organizations",
		map[string]any{"id": organization, "name": "an organization"}, http.StatusCreated, nil)

	publication := uuid.NewString()
	sum := sha256.Sum256([]byte(yaml))
	a.expect(t, "publishing", http.MethodPost, "/api/v1/organizations/"+organization+"/publications", map[string]any{
		"id":            publication,
		"configuration": map[string]any{"id": uuid.NewString(), "name": "payments-gateway"},
		"version":       map[string]any{"id": uuid.NewString(), "number": 1, "label": nil},
		"yaml":          yaml,
		"sha256":        hex.EncodeToString(sum[:]),
		"publishedBy":   Actor,
		"publishedAt":   "2026-10-03T14:02:11.000Z",
		"collector":     map[string]any{"distribution": "otelcol-contrib", "version": "0.162.0"},
	}, http.StatusCreated, nil)

	var fleet Fleet
	a.expect(t, "creating a fleet", http.MethodPost, "/api/v1/organizations/"+organization+"/fleets",
		map[string]any{"label": label, "type": "server", "rotateExpiresAt": nil, "actor": Actor}, http.StatusCreated, &fleet)
	a.expect(t, "assigning", http.MethodPatch,
		"/api/v1/organizations/"+organization+"/fleets/"+fleet.Fleet.ID+"/groups/"+fleet.DefaultGroup.ID,
		map[string]any{"assignedPublicationId": publication}, http.StatusOK, nil)
	return Setup{Organization: organization, Publication: publication, Fleet: fleet}
}
