package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/internal/diagnostics"
	"github.com/telflo/opamp-server/store/apikeys"
	"github.com/telflo/opamp-server/store/organizations"
	"github.com/telflo/opamp-server/store/publications"
)

func apiKeyTarget(id string) string       { return apiKeysPath + "/" + id }
func revokeAPIKeyTarget(id string) string { return apiKeysPath + "/" + id + "/revoke" }

// apiKeyBody is a key to mint for orgs (none: every organization), expiring
// at expiresAt.
func apiKeyBody(expiresAt string, orgs ...string) body {
	var organizationIDs any
	if len(orgs) > 0 {
		organizationIDs = orgs
	}
	return body{"name": "deploy pipeline", "description": "Publishes from CI.",
		"organizationIds": organizationIDs, "expiresAt": expiresAt}
}

// inAYear is a year after the test clock's start: a valid expiry.
const inAYear = "2027-10-03T15:00:00.000Z"

// mintKey mints a key for orgs (none: every organization) on the secret,
// and returns its id and the key itself.
func mintKey(t *testing.T, s *Server, orgs ...string) (id, key string) {
	t.Helper()
	got := sendWith(s, http.MethodPost, apiKeysPath, apiKeyBody(inAYear, orgs...))
	expectStatus(t, "minting a key", got, http.StatusCreated)
	r := decodeReply(t, got)
	return r["id"].(string), r["key"].(string)
}

// keyOf is a conformance case's key, minted after its seed.
func keyOf(orgs ...string) func(t *testing.T, s *Server) string {
	return func(t *testing.T, s *Server) string {
		_, key := mintKey(t, s, orgs...)
		return key
	}
}

// callWithKey sends one request authenticated with bearer.
func callWithKey(s *Server, bearer, method, target string, payload []byte, contentType string) *httptest.ResponseRecorder {
	var request *http.Request
	if payload == nil {
		request = httptest.NewRequest(method, target, nil)
	} else {
		request = httptest.NewRequest(method, target, strings.NewReader(string(payload)))
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, request)
	return recorder
}

// failingAPIKeys fails every call, as a database that is down would.
type failingAPIKeys struct{}

func (failingAPIKeys) Create(context.Context, apikeys.NewKey) (apikeys.Key, error) {
	return apikeys.Key{}, errStoreDown
}
func (failingAPIKeys) List(context.Context) ([]apikeys.Key, error) { return nil, errStoreDown }
func (failingAPIKeys) Get(context.Context, uuid.UUID) (apikeys.Key, error) {
	return apikeys.Key{}, errStoreDown
}
func (failingAPIKeys) Update(context.Context, uuid.UUID, apikeys.Changes) (apikeys.Key, error) {
	return apikeys.Key{}, errStoreDown
}
func (failingAPIKeys) Revoke(context.Context, uuid.UUID, time.Time) (apikeys.Key, error) {
	return apikeys.Key{}, errStoreDown
}
func (failingAPIKeys) Lookup(context.Context, uuid.UUID) (apikeys.Authenticated, error) {
	return apikeys.Authenticated{}, errStoreDown
}
func (failingAPIKeys) Touch(context.Context, uuid.UUID, time.Time) error { return errStoreDown }

// failingAPIKeysServer is a Server whose API keys' store fails, A and B
// registered.
func failingAPIKeysServer(t *testing.T) *Server {
	t.Helper()
	d := testDB(t)
	registry := organizations.NewPostgres(d.Runtime)
	register(t, registry, orgA, orgB)
	fs := postgresFleetStores(d.Runtime)
	fs.apiKeys = failingAPIKeys{}
	s, _, _ := newServerWith(t, registry, publications.NewPostgres(d.Runtime), fs)
	return s
}

// apiKeyConformanceCases are the conformance cases of the API key routes,
// of a key on an organization's route, and of an organization's register
// with a key: every (method, path, status) api/openapi.yaml documents for
// each, and one case for each code a documented status names.
func apiKeyConformanceCases() []conformanceCase {
	post, get, patch, put := http.MethodPost, http.MethodGet, http.MethodPatch, http.MethodPut
	created := apiKeyBody(inAYear, orgA).bytes()
	tooLarge := []byte(`{"name":"` + strings.Repeat("n", MaxBodyBytes) + `"}`)
	minted := func(target func(id string) string) func(t *testing.T, s *Server) string {
		return func(t *testing.T, s *Server) string {
			id, _ := mintKey(t, s, orgA)
			return target(id)
		}
	}
	fullOfKeys := func(t *testing.T, s *Server) {
		for range apikeys.MaxActive {
			mintKey(t, s)
		}
	}
	renamed := body{"name": "release pipeline"}.bytes()
	return []conformanceCase{
		// POST, GET /api/v1/api-keys.
		{name: "mint a key", method: post, target: apiKeysPath, contentType: jsonType, payload: created, status: 201, conforms: true},
		{name: "mint a key for every organization", method: post, target: apiKeysPath, contentType: jsonType, payload: apiKeyBody(inAYear).bytes(), status: 201, conforms: true},
		{name: "mint a key expiring in 3 years", method: post, target: apiKeysPath, contentType: jsonType, payload: apiKeyBody("2029-10-04T00:00:00.000Z", orgA).bytes(), status: 400, conforms: true},
		{name: "mint a key naming an organization twice", method: post, target: apiKeysPath, contentType: jsonType, payload: apiKeyBody(inAYear, orgA, orgA).bytes(), status: 400, conforms: false},
		{name: "mint a key without the secret", method: post, target: apiKeysPath, contentType: jsonType, payload: created, unauthenticated: true, status: 401, conforms: false},
		{name: "mint a key with a key", method: post, target: apiKeysPath, contentType: jsonType, payload: created, apiKeyOf: keyOf(), status: 403, conforms: true},
		{name: "mint a key for an unregistered organization", method: post, target: apiKeysPath, contentType: jsonType, payload: apiKeyBody(inAYear, orgC).bytes(), status: 404, conforms: true},
		{name: "mint a key past the limit", method: post, target: apiKeysPath, contentType: jsonType, payload: created, seed: fullOfKeys, status: 409, conforms: true},
		{name: "mint a key, too large", method: post, target: apiKeysPath, contentType: jsonType, payload: tooLarge, status: 413, conforms: false},
		{name: "mint a key, store down", method: post, target: apiKeysPath, contentType: jsonType, payload: created, failingAPIKeys: true, status: 500, conforms: true},
		{name: "list keys", method: get, target: apiKeysPath, seed: func(t *testing.T, s *Server) { mintKey(t, s, orgA); mintKey(t, s) }, status: 200, conforms: true},
		{name: "list keys without the secret", method: get, target: apiKeysPath, unauthenticated: true, status: 401, conforms: false},
		{name: "list keys with a key", method: get, target: apiKeysPath, apiKeyOf: keyOf(), status: 403, conforms: true},
		{name: "list keys, store down", method: get, target: apiKeysPath, failingAPIKeys: true, status: 500, conforms: true},

		// GET, PATCH /api/v1/api-keys/{apiKeyId}.
		{name: "get a key", method: get, targetOf: minted(apiKeyTarget), status: 200, conforms: true},
		{name: "get a key, an invalid id", method: get, target: apiKeyTarget("x"), status: 400, conforms: false},
		{name: "get a key without the secret", method: get, target: apiKeyTarget(unknownID), unauthenticated: true, status: 401, conforms: false},
		{name: "get a key with a key", method: get, target: apiKeyTarget(unknownID), apiKeyOf: keyOf(), status: 403, conforms: true},
		{name: "get an unknown key", method: get, target: apiKeyTarget(unknownID), status: 404, conforms: true},
		{name: "get a key, store down", method: get, target: apiKeyTarget(unknownID), failingAPIKeys: true, status: 500, conforms: true},
		{name: "rename a key", method: patch, targetOf: minted(apiKeyTarget), contentType: jsonType, payload: renamed, status: 200, conforms: true},
		{name: "clear a key's description", method: patch, targetOf: minted(apiKeyTarget), contentType: jsonType, payload: []byte(`{"description":null}`), status: 200, conforms: true},
		{name: "change a key, nothing named", method: patch, target: apiKeyTarget(unknownID), contentType: jsonType, payload: []byte(`{}`), status: 400, conforms: false},
		{name: "change a key without the secret", method: patch, target: apiKeyTarget(unknownID), contentType: jsonType, payload: renamed, unauthenticated: true, status: 401, conforms: false},
		{name: "change a key with a key", method: patch, target: apiKeyTarget(unknownID), contentType: jsonType, payload: renamed, apiKeyOf: keyOf(), status: 403, conforms: true},
		{name: "change an unknown key", method: patch, target: apiKeyTarget(unknownID), contentType: jsonType, payload: renamed, status: 404, conforms: true},
		{name: "change a key, too large", method: patch, target: apiKeyTarget(unknownID), contentType: jsonType, payload: tooLarge, status: 413, conforms: false},
		{name: "change a key, store down", method: patch, target: apiKeyTarget(unknownID), contentType: jsonType, payload: renamed, failingAPIKeys: true, status: 500, conforms: true},

		// POST /api/v1/api-keys/{apiKeyId}/revoke.
		{name: "revoke a key", method: post, targetOf: minted(revokeAPIKeyTarget), status: 200, conforms: true},
		{name: "revoke a key, an invalid id", method: post, target: revokeAPIKeyTarget("x"), status: 400, conforms: false},
		{name: "revoke a key without the secret", method: post, target: revokeAPIKeyTarget(unknownID), unauthenticated: true, status: 401, conforms: false},
		{name: "revoke a key with a key", method: post, target: revokeAPIKeyTarget(unknownID), apiKeyOf: keyOf(), status: 403, conforms: true},
		{name: "revoke an unknown key", method: post, target: revokeAPIKeyTarget(unknownID), status: 404, conforms: true},
		{name: "revoke a key, store down", method: post, target: revokeAPIKeyTarget(unknownID), failingAPIKeys: true, status: 500, conforms: true},

		// A key on the organizations' routes.
		{name: "register an organization with a key for every organization", method: post, target: organizationsPath, contentType: jsonType,
			payload: []byte(`{"id":"` + orgC + `","name":"Ana's workspace"}`), apiKeyOf: keyOf(), status: 201, conforms: true},
		{name: "register an organization with a key for some", method: post, target: organizationsPath, contentType: jsonType,
			payload: []byte(`{"id":"` + orgC + `","name":"Ana's workspace"}`), apiKeyOf: keyOf(orgA), status: 403, conforms: true},
		{name: "read an organization's limits with its key", method: get, target: "/api/v1/organizations/" + orgA + "/limits",
			apiKeyOf: keyOf(orgA), status: 200, conforms: true},
		{name: "read another organization's limits with a key", method: get, target: "/api/v1/organizations/" + orgB + "/limits",
			apiKeyOf: keyOf(orgA), status: 404, conforms: true},
		{name: "set an organization's limits with a key for every organization", method: put, target: "/api/v1/organizations/" + orgA + "/limits",
			contentType: jsonType, payload: []byte(`{"maxCollectors":10,"requestsPerMinute":600}`), apiKeyOf: keyOf(), status: 200, conforms: true},
		{name: "set an organization's limits with its own key", method: put, target: "/api/v1/organizations/" + orgA + "/limits",
			contentType: jsonType, payload: []byte(`{"maxCollectors":null,"requestsPerMinute":2147483647}`), apiKeyOf: keyOf(orgA), status: 403, conforms: true},
		{name: "set another organization's limits with a key", method: put, target: "/api/v1/organizations/" + orgB + "/limits",
			contentType: jsonType, payload: []byte(`{"maxCollectors":null,"requestsPerMinute":600}`), apiKeyOf: keyOf(orgA), status: 404, conforms: true},
		{name: "read limits with a revoked key", method: get, target: "/api/v1/organizations/" + orgA + "/limits",
			apiKeyOf: func(t *testing.T, s *Server) string {
				id, key := mintKey(t, s, orgA)
				expectStatus(t, "revoking", call(s, post, revokeAPIKeyTarget(id), nil, ""), http.StatusOK)
				return key
			}, status: 401, conforms: true},
	}
}

// A key opens its organizations' routes, but not setting their limits;
// another organization's answer is the unregistered organization's, the API
// key routes and the diagnostics answer it 403, and an unmatched path the
// same 404 as for the secret.
func TestAKeyOpensItsOrganizationsRoutesOnly(t *testing.T) {
	emptyTestDB(t)
	s, _, _ := apiServer(t)
	_, key := mintKey(t, s, orgA)

	expectStatus(t, "A's limits", callWithKey(s, key, http.MethodGet, "/api/v1/organizations/"+orgA+"/limits", nil, ""), http.StatusOK)
	expectError(t, "setting A's limits", callWithKey(s, key, http.MethodPut, "/api/v1/organizations/"+orgA+"/limits",
		[]byte(`{"maxCollectors":null,"requestsPerMinute":2147483647}`), "application/json"),
		http.StatusForbidden, codeForbidden, messageSetLimits)
	expectError(t, "B's limits", callWithKey(s, key, http.MethodGet, "/api/v1/organizations/"+orgB+"/limits", nil, ""),
		http.StatusNotFound, codeOrganizationNotFound, messageOrganizationNotFound)
	expectError(t, "the keys", callWithKey(s, key, http.MethodGet, apiKeysPath, nil, ""),
		http.StatusForbidden, codeForbidden, messageSecretOnly)
	expectError(t, "the profiles", callWithKey(s, key, http.MethodGet, diagnostics.PprofPrefix, nil, ""),
		http.StatusForbidden, codeForbidden, messageSecretOnly)
	expectError(t, "the admission view", callWithKey(s, key, http.MethodGet, diagnostics.AdmissionPath, nil, ""),
		http.StatusForbidden, codeForbidden, messageSecretOnly)
	expectError(t, "a register", callWithKey(s, key, http.MethodPost, organizationsPath,
		[]byte(`{"id":"`+orgC+`","name":"C"}`), jsonType), http.StatusForbidden, codeForbidden, messageAllOrganizations)
	expectError(t, "an unmatched path", callWithKey(s, key, http.MethodGet, "/api/v1/nothing", nil, ""),
		http.StatusNotFound, codeNotFound, messageNotFound)
	// A path id that isn't a UUID is the route's own 400, as for the secret.
	expectStatus(t, "B's fleet x", callWithKey(s, key, http.MethodGet, "/api/v1/organizations/"+orgB+"/fleets/x", nil, ""),
		http.StatusBadRequest)
}

// wildcardsIn matches a pattern's wildcards.
var wildcardsIn = regexp.MustCompile(`\{([A-Za-z]+)\}`)

// Every route under an organization answers a key for another organization
// exactly as the secret's request under an organization that isn't
// registered; every other route but the catch-all answers it 403, but a
// register, which a key for every organization may make.
func TestEveryRouteAnswersAKeyAsItsRuleSays(t *testing.T) {
	emptyTestDB(t)
	s, _, _ := apiServer(t)
	_, keyA := mintKey(t, s, orgA)
	_, keyAll := mintKey(t, s)
	checked := 0
	for _, pattern := range s.api.patterns {
		method, path, found := strings.Cut(pattern, " ")
		if !found {
			continue
		}
		var payload []byte
		contentType := ""
		if method != http.MethodGet && method != http.MethodDelete {
			payload, contentType = []byte(`{}`), jsonType
		}
		if pattern == http.MethodPatch+" "+organizationPath {
			// A rename reads its body before the organization: a valid one,
			// so the secret's answer is the organization's.
			payload = []byte(`{"name":"B"}`)
		}
		if strings.HasPrefix(path, organizationPath) {
			target := func(organizationID string) string {
				return wildcardsIn.ReplaceAllStringFunc(path, func(w string) string {
					if w == "{organizationId}" {
						return organizationID
					}
					return unknownID
				})
			}
			withKey := callWithKey(s, keyA, method, target(orgB), payload, contentType)
			unregistered := call(s, method, target(orgC), payload, contentType)
			if withKey.Code != unregistered.Code || withKey.Body.String() != unregistered.Body.String() {
				t.Errorf("%s: a key for A on B answers %d %s; the secret on an unregistered organization %d %s",
					pattern, withKey.Code, withKey.Body.String(), unregistered.Code, unregistered.Body.String())
			}
			checked++
			continue
		}
		target := wildcardsIn.ReplaceAllString(path, unknownID)
		if pattern == http.MethodPost+" "+organizationsPath {
			expectError(t, pattern, callWithKey(s, keyA, method, target, payload, contentType),
				http.StatusForbidden, codeForbidden, messageAllOrganizations)
			// A new organization, not C, which the walk needs unregistered.
			expectStatus(t, pattern+" for every organization", callWithKey(s, keyAll, method, target,
				[]byte(`{"name":"D"}`), jsonType), http.StatusCreated)
			continue
		}
		for _, key := range []string{keyA, keyAll} {
			expectError(t, pattern, callWithKey(s, key, method, target, payload, contentType),
				http.StatusForbidden, codeForbidden, messageSecretOnly)
		}
	}
	if checked < 40 {
		t.Errorf("checked %d organization routes; the walk missed them", checked)
	}
}

// A key for every organization opens an organization registered after it.
func TestAKeyForEveryOrganizationOpensOneRegisteredLater(t *testing.T) {
	emptyTestDB(t)
	s, _, _ := apiServer(t)
	_, key := mintKey(t, s)
	expectStatus(t, "registering C", callWithKey(s, key, http.MethodPost, organizationsPath,
		[]byte(`{"id":"`+orgC+`","name":"C"}`), jsonType), http.StatusCreated)
	expectStatus(t, "C's limits", callWithKey(s, key, http.MethodGet, "/api/v1/organizations/"+orgC+"/limits", nil, ""), http.StatusOK)
}

// A revoked key, an expired one, an unknown one, one whose secret is
// another's, and a malformed one each get the one 401 every bad
// credential gets.
func TestABadKeyGetsTheOne401(t *testing.T) {
	emptyTestDB(t)
	s, clock, _ := apiServer(t)
	limits := "/api/v1/organizations/" + orgA + "/limits"
	revokedID, revoked := mintKey(t, s, orgA)
	expectStatus(t, "revoking", call(s, http.MethodPost, revokeAPIKeyTarget(revokedID), nil, ""), http.StatusOK)
	_, other := mintKey(t, s, orgA)
	_, valid := mintKey(t, s, orgA)
	expectStatus(t, "a valid key", callWithKey(s, valid, http.MethodGet, limits, nil, ""), http.StatusOK)

	for name, bearer := range map[string]string{
		"revoked":           revoked,
		"unknown":           apikeys.Prefix + strings.Repeat("0", 32) + valid[36:],
		"another's secret":  valid[:37] + other[37:],
		"malformed":         valid[:len(valid)-1],
		"the key lowercase": strings.ToLower(valid),
	} {
		if bearer == valid {
			continue
		}
		expectError(t, name, callWithKey(s, bearer, http.MethodGet, limits, nil, ""),
			http.StatusUnauthorized, codeUnauthorized, messageUnauthorized)
	}
	clock.Advance(366 * 24 * time.Hour)
	expectError(t, "expired", callWithKey(s, valid, http.MethodGet, limits, nil, ""),
		http.StatusUnauthorized, codeUnauthorized, messageUnauthorized)
}

// Minting answers the key once, with no-store; every read answers the key
// without it, with its status and its last use at the server's clock.
func TestTheKeyIsAnsweredOnceAndItsUseIsRecorded(t *testing.T) {
	emptyTestDB(t)
	s, clock, log := apiServer(t)
	got := sendWith(s, http.MethodPost, apiKeysPath, apiKeyBody(inAYear, orgB, orgA))
	expectStatus(t, "minting", got, http.StatusCreated)
	if got.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", got.Header().Get("Cache-Control"))
	}
	minted := decodeReply(t, got)
	id, key := minted["id"].(string), minted["key"].(string)
	if minted["status"] != "active" || minted["lastUsedAt"] != nil {
		t.Errorf("minted %v; want active, never used", minted)
	}
	if ids := minted["organizationIds"].([]any); len(ids) != 2 || ids[0] != orgA || ids[1] != orgB {
		t.Errorf("organizationIds %v, want A and B in byte order", ids)
	}

	clock.Advance(time.Minute)
	expectStatus(t, "using it", callWithKey(s, key, http.MethodGet, "/api/v1/organizations/"+orgA+"/limits", nil, ""), http.StatusOK)
	read := decodeReply(t, call(s, http.MethodGet, apiKeyTarget(id), nil, ""))
	if _, has := read["key"]; has {
		t.Error("a read answers the key")
	}
	if read["lastUsedAt"] != formatTime(clock.Now()) {
		t.Errorf("lastUsedAt %v, want %s", read["lastUsedAt"], formatTime(clock.Now()))
	}
	if strings.Contains(log.String(), key) || strings.Contains(log.String(), key[37:]) {
		t.Error("the log holds the key")
	}
	var keyLines int
	for _, line := range logLines(t, log.String()) {
		if line["apiKeyId"] == id {
			keyLines++
		}
	}
	if keyLines != 1 {
		t.Errorf("%d log lines name the key's id, want the one request it made", keyLines)
	}

	revoked := decodeReply(t, call(s, http.MethodPost, revokeAPIKeyTarget(id), nil, ""))
	if revoked["status"] != "revoked" || revoked["revokedAt"] != formatTime(clock.Now()) {
		t.Errorf("revoked %v", revoked)
	}
	mintKey(t, s, orgA)
	clock.Advance(366 * 24 * time.Hour)
	list := decodeReply(t, call(s, http.MethodGet, apiKeysPath, nil, ""))["apiKeys"].([]any)
	if len(list) != 2 || list[0].(map[string]any)["status"] != "expired" || list[1].(map[string]any)["status"] != "revoked" {
		t.Errorf("the list %v; want the expired key, then the revoked one", list)
	}
}

// What a mint refuses, naming the field, and changing nothing.
func TestAMintRefusesWhatTheRulesRefuse(t *testing.T) {
	emptyTestDB(t)
	s, _, _ := apiServer(t)
	for name, c := range map[string]struct {
		body    body
		message string
	}{
		"an expiry passed":       {apiKeyBody("2026-10-03T14:00:00.000Z", orgA), messageAPIKeyExpiresAt},
		"an expiry past 2 years": {apiKeyBody("2028-10-03T15:00:00.124Z", orgA), messageAPIKeyExpiresAt},
		"no organization":        {body{"name": "n", "description": nil, "organizationIds": []string{}, "expiresAt": inAYear}, messageAPIKeyOrganizationIDs},
		"an organization twice":  {apiKeyBody(inAYear, orgA, orgA), messageAPIKeyOrganizationIDs},
		"an organization not a UUID": {
			body{"name": "n", "description": nil, "organizationIds": []string{"x"}, "expiresAt": inAYear}, messageAPIKeyOrganizationIDs},
		"a name with a newline": {body{"name": "a\nb", "description": nil, "organizationIds": nil, "expiresAt": inAYear}, messageAPIKeyName},
		"an empty description":  {body{"name": "n", "description": "", "organizationIds": nil, "expiresAt": inAYear}, messageAPIKeyDescription},
		"no expiry":             {body{"name": "n", "description": nil, "organizationIds": nil}, "expiresAt is required."},
	} {
		expectError(t, name, sendWith(s, http.MethodPost, apiKeysPath, c.body), http.StatusBadRequest, codeInvalidRequest, c.message)
	}
	// Exactly 2 years is allowed.
	expectStatus(t, "an expiry 2 years away", sendWith(s, http.MethodPost, apiKeysPath, apiKeyBody("2028-10-03T15:00:00.123Z", orgA)), http.StatusCreated)
	expectError(t, "an unregistered organization", sendWith(s, http.MethodPost, apiKeysPath, apiKeyBody(inAYear, orgA, orgC)),
		http.StatusNotFound, codeOrganizationNotFound, messageAPIKeysUnregistered)
	if list := decodeReply(t, call(s, http.MethodGet, apiKeysPath, nil, ""))["apiKeys"].([]any); len(list) != 1 {
		t.Errorf("%d keys, want the one minted", len(list))
	}
}

// An organization's delete takes it from its keys: registered again, the
// key doesn't open it.
func TestADeletedOrganizationLeavesItsKeys(t *testing.T) {
	emptyTestDB(t)
	s, _, _ := apiServer(t)
	id, key := mintKey(t, s, orgA)
	expectStatus(t, "deleting A", call(s, http.MethodDelete, organizationTarget(orgA), nil, ""), http.StatusNoContent)
	expectStatus(t, "registering A again", call(s, http.MethodPost, organizationsPath, []byte(`{"id":"`+orgA+`","name":"A"}`), jsonType), http.StatusCreated)
	expectError(t, "A's limits", callWithKey(s, key, http.MethodGet, "/api/v1/organizations/"+orgA+"/limits", nil, ""),
		http.StatusNotFound, codeOrganizationNotFound, messageOrganizationNotFound)
	if ids := decodeReply(t, call(s, http.MethodGet, apiKeyTarget(id), nil, ""))["organizationIds"].([]any); len(ids) != 0 {
		t.Errorf("the key still names %v", ids)
	}
}

// A key's request on a store that fails answers 500, as any route's would.
func TestAKeyOnAFailingStoreAnswers500(t *testing.T) {
	emptyTestDB(t)
	s := failingAPIKeysServer(t)
	_, key, _, err := apikeys.Generate()
	if err != nil {
		t.Fatal(err)
	}
	expectError(t, "a key", callWithKey(s, key, http.MethodGet, "/api/v1/organizations/"+orgA+"/limits", nil, ""),
		http.StatusInternalServerError, codeInternalError, messageInternalError)
	expectStatus(t, "the secret", call(s, http.MethodGet, "/api/v1/organizations/"+orgA+"/limits", nil, ""), http.StatusOK)
}
