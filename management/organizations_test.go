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

	"github.com/telflo/telflo-opamp-server/store/organizations"
	"github.com/telflo/telflo-opamp-server/store/publications"
)

const (
	organizationConflictBody = `{"error":{"code":"organization_conflict","message":"An organization with this id is already registered under another name."}}`
	organizationNotFoundBody = `{"error":{"code":"organization_not_found","message":"No organization with this id is registered: register it with POST /api/v1/organizations first."}}`
)

// registryServer is a Server over the Postgres stores of the test's
// database (testDB), with no organization registered.
func registryServer(t *testing.T) (*Server, *testClock, *syncBuffer) {
	t.Helper()
	d := testDB(t)
	return newServer(t, organizations.NewPostgres(d.Runtime), publications.NewPostgres(d.Runtime))
}

// eachServer runs test against registryServer, in a subtest named
// "postgres".
func eachServer(t *testing.T, test func(t *testing.T, s *Server, clock *testClock)) {
	t.Run("postgres", func(t *testing.T) {
		s, clock, _ := registryServer(t)
		test(t, s, clock)
	})
}

func postOrganization(s *Server, payload string) *httptest.ResponseRecorder {
	return call(s, http.MethodPost, organizationsPath, []byte(payload), jsonType)
}

func createBody(id, name string) string {
	raw, _ := json.Marshal(map[string]string{"id": id, "name": name})
	return string(raw)
}

func nameBody(name string) string {
	raw, _ := json.Marshal(map[string]string{"name": name})
	return string(raw)
}

// organizationReplyOf is an organization as GET or PATCH answered it.
func organizationReplyOf(t *testing.T, recorder *httptest.ResponseRecorder) organizationReply {
	t.Helper()
	var got organizationReply
	decoder := json.NewDecoder(strings.NewReader(recorder.Body.String()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&got); err != nil {
		t.Fatalf("the reply isn't an organization: %v: %s", err, recorder.Body.String())
	}
	return got
}

func getOrganizationReply(t *testing.T, s *Server, id string) organizationReply {
	t.Helper()
	got := call(s, http.MethodGet, organizationTarget(id), nil, "")
	expectStatus(t, "GET the organization", got, http.StatusOK)
	return organizationReplyOf(t, got)
}

// A POST registers the id (201), the same id and name again is a safe
// retry (200, nothing changes), and the same id with another name is 409
// organization_conflict, which renames nothing and says nothing more. Each
// success answers exactly {"organizationId"}.
func TestPostOrganizationIsCreatedThenUnchangedThenConflicting(t *testing.T) {
	eachServer(t, func(t *testing.T, s *Server, clock *testClock) {
		const name = "ana@acme.io's workspace"
		want := `{"organizationId":"` + orgA + `"}`
		created := postOrganization(s, createBody(orgA, name))
		if created.Code != http.StatusCreated || created.Body.String() != want || created.Header().Get("Content-Type") != jsonType {
			t.Fatalf("the first POST: %d %s, want 201 %s", created.Code, created.Body.String(), want)
		}
		registered := organizationReply{OrganizationID: orgA, Name: name,
			CreatedAt: "2026-10-03T15:00:00.123Z", UpdatedAt: "2026-10-03T15:00:00.123Z"}
		if got := getOrganizationReply(t, s, orgA); got != registered {
			t.Fatalf("GET after the POST = %+v, want %+v", got, registered)
		}

		clock.Advance(time.Hour)
		again := postOrganization(s, createBody(orgA, name))
		if again.Code != http.StatusOK || again.Body.String() != want {
			t.Fatalf("the same POST again: %d %s, want 200 %s", again.Code, again.Body.String(), want)
		}
		// An uppercase id is the same id.
		if upper := postOrganization(s, createBody(strings.ToUpper(orgA), name)); upper.Code != http.StatusOK || upper.Body.String() != want {
			t.Fatalf("the POST with the id in capitals: %d %s, want 200 %s", upper.Code, upper.Body.String(), want)
		}

		for label, other := range map[string]string{
			"another name":           "bo@acme.io's workspace",
			"the name with a space":  name + " ",
			"the name in capitals":   strings.ToUpper(name),
			"the name one character": name[:len(name)-1],
		} {
			got := postOrganization(s, createBody(orgA, other))
			if got.Code != http.StatusConflict || got.Body.String() != organizationConflictBody {
				t.Fatalf("%s: %d %s, want 409 %s", label, got.Code, got.Body.String(), organizationConflictBody)
			}
		}
		if got := getOrganizationReply(t, s, orgA); got != registered {
			t.Fatalf("GET after the retry and the conflicts = %+v, want %+v", got, registered)
		}
	})
}

// A POST without an id registers a new organization under a new UUID each
// time, even with the same name: it can't be safely retried.
func TestPostWithoutAnIdRegistersANewUUIDEachTime(t *testing.T) {
	eachServer(t, func(t *testing.T, s *Server, _ *testClock) {
		seen := map[string]bool{}
		for range 3 {
			got := postOrganization(s, nameBody("the same name"))
			expectStatus(t, "a POST without an id", got, http.StatusCreated)
			var reply organizationIDReply
			if err := json.Unmarshal(got.Body.Bytes(), &reply); err != nil {
				t.Fatal(err)
			}
			id, ok := parseUUID(reply.OrganizationID)
			if !ok || id.String() != reply.OrganizationID || id.Version() != 4 {
				t.Fatalf("the generated id %q isn't a canonical random UUID", reply.OrganizationID)
			}
			if seen[reply.OrganizationID] {
				t.Fatalf("the id %s was generated twice", reply.OrganizationID)
			}
			seen[reply.OrganizationID] = true
			if registered := getOrganizationReply(t, s, reply.OrganizationID); registered.Name != "the same name" {
				t.Fatalf("the generated organization is %+v", registered)
			}
		}
	})
}

// The bodies of POST and PATCH: the name is 1 to 400 characters (code
// points) with no U+0000, kept exactly as sent; POST's id is optional but a
// UUID when present; no other field; and the same body rules as a POST of a
// publication (UTF-8, one JSON object, the Content-Type). Each failure is a
// 400 naming the field, never its value.
func TestPostAndPatchValidateTheBody(t *testing.T) {
	accent := "é" // two bytes, one character
	type validation struct {
		name        string
		payload     string
		contentType string
		status      int
		message     string
		value       string
	}
	shared := []validation{
		{name: "name 1 character", payload: nameBody("n"), status: 0},
		{name: "name 400 characters", payload: nameBody(strings.Repeat(accent, 400)), status: 0},
		{name: "name with surrounding whitespace", payload: nameBody("  a name \t"), status: 0},
		{name: "name missing", payload: `{}`, status: 400, message: "name is required."},
		{name: "name null", payload: `{"name":null}`, status: 400, message: "name must be a string."},
		{name: "name a number", payload: `{"name":7}`, status: 400, message: "name must be a string."},
		{name: "name empty", payload: nameBody(""), status: 400, message: "name must be 1 to 400 characters."},
		{name: "name 401 characters", payload: nameBody(strings.Repeat("name-marker", 36) + "abcde"), status: 400,
			message: "name must be 1 to 400 characters.", value: "name-marker"},
		{name: "name with U+0000", payload: `{"name":"a\u0000b"}`, status: 400, message: "name must not contain U+0000."},
		{name: "an unknown field", payload: `{"name":"a","members":[]}`, status: 400, message: "The request body has a field this API doesn't know."},
		{name: "not JSON", payload: "name: a", status: 400, message: "The request body must be a JSON object."},
		{name: "a JSON array", payload: `[]`, status: 400, message: "The request body must be a JSON object."},
		{name: "JSON null", payload: `null`, status: 400, message: "The request body must be a JSON object."},
		{name: "no body", payload: ``, status: 400, message: "The request body must be a JSON object."},
		{name: "two objects", payload: nameBody("a") + nameBody("b"), status: 400, message: "The request body must be one JSON object."},
		{name: "the object followed by xyz", payload: nameBody("a") + "xyz", status: 400, message: "The request body must be one JSON object."},
		{name: "whitespace after the object", payload: nameBody("a") + " \r\n", status: 0},
		{name: "invalid UTF-8", payload: `{"name":"` + "\xff" + `"}`, status: 400, message: "The request body must be UTF-8."},
		{name: "Content-Type text/plain", payload: nameBody("a"), contentType: "text/plain", status: 400, message: "Content-Type must be application/json."},
		{name: "no Content-Type", payload: nameBody("a"), contentType: "-", status: 400, message: "Content-Type must be application/json."},
		{name: "Content-Type merge-patch", payload: nameBody("a"), contentType: "application/merge-patch+json", status: 400,
			message: "Content-Type must be application/json."},
		{name: "Content-Type with charset=utf-8", payload: nameBody("a"), contentType: "application/json; charset=utf-8", status: 0},
	}
	postOnly := []validation{
		{name: "an id", payload: createBody(uuid.NewString(), "a"), status: http.StatusCreated},
		{name: "id null", payload: `{"id":null,"name":"a"}`, status: 400, message: "id must be a UUID."},
		{name: "id not a UUID", payload: `{"id":"id-marker-9a1","name":"a"}`, status: 400, message: "id must be a UUID.", value: "id-marker-9a1"},
		{name: "id in braces", payload: `{"id":"{` + orgA + `}","name":"a"}`, status: 400, message: "id must be a UUID."},
		{name: "id a number", payload: `{"id":7,"name":"a"}`, status: 400, message: "id must be a UUID."},
		{name: "id without a name", payload: `{"id":"` + orgA + `"}`, status: 400, message: "name is required."},
	}
	patchOnly := []validation{
		{name: "an id beside the name", payload: createBody(orgA, "a"), status: 400, message: "The request body has a field this API doesn't know."},
	}

	check := func(t *testing.T, method string, c validation, success int) {
		t.Helper()
		emptyTestDB(t)
		s, _, _ := registryServer(t)
		register(t, s.organizations, orgA)
		target := organizationsPath
		if method == http.MethodPatch {
			target = organizationTarget(orgA)
		}
		contentType := c.contentType
		switch contentType {
		case "":
			contentType = jsonType
		case "-":
			contentType = ""
		}
		got := call(s, method, target, []byte(c.payload), contentType)
		if c.status == 0 || c.status == success {
			expectStatus(t, method+" "+c.name, got, success)
			return
		}
		expectError(t, method+" "+c.name, got, c.status, codeInvalidRequest, c.message)
		if c.value != "" && strings.Contains(got.Body.String(), c.value) {
			t.Fatalf("%s %s: the answer quotes the value: %s", method, c.name, got.Body.String())
		}
	}
	for _, c := range append(shared, postOnly...) {
		check(t, http.MethodPost, c, http.StatusCreated)
	}
	for _, c := range append(shared, patchOnly...) {
		check(t, http.MethodPatch, c, http.StatusOK)
	}

	// The name is kept exactly: neither trimmed nor normalized.
	s, _, _ := registryServer(t)
	for _, name := range []string{"  a name \t", "é", " "} {
		id := uuid.NewString()
		expectStatus(t, "POST "+name, postOrganization(s, createBody(id, name)), http.StatusCreated)
		if got := getOrganizationReply(t, s, id); got.Name != name {
			t.Fatalf("the name %q came back as %q", name, got.Name)
		}
	}
}

func TestGetOrganizationIsTheOrganizationOr404(t *testing.T) {
	s, _, _ := registryServer(t)
	expectStatus(t, "POST", postOrganization(s, createBody(orgA, "<A> & \"A\"")), http.StatusCreated)

	got := call(s, http.MethodGet, organizationTarget(orgA), nil, "")
	want := `{"organizationId":"` + orgA + `","name":"<A> & \"A\"","createdAt":"2026-10-03T15:00:00.123Z","updatedAt":"2026-10-03T15:00:00.123Z"}`
	if got.Code != http.StatusOK || got.Body.String() != want {
		t.Fatalf("GET: %d %s, want 200 %s", got.Code, got.Body.String(), want)
	}
	expectError(t, "an unknown id", call(s, http.MethodGet, organizationTarget(orgB), nil, ""), http.StatusNotFound, codeNotFound, messageNotFound)
	expectError(t, "a non-UUID id", call(s, http.MethodGet, organizationTarget("org-marker"), nil, ""),
		http.StatusBadRequest, codeInvalidRequest, "organizationId must be a UUID.")
	// HEAD isn't documented: like any unmatched method, a 404.
	expectStatus(t, "HEAD", call(s, http.MethodHead, organizationTarget(orgA), nil, ""), http.StatusNotFound)
	// Nor is a list of organizations, or a PUT of one.
	expectError(t, "GET the collection", call(s, http.MethodGet, organizationsPath, nil, ""), http.StatusNotFound, codeNotFound, messageNotFound)
	expectError(t, "PUT an organization", call(s, http.MethodPut, organizationTarget(orgA), []byte(nameBody("a")), jsonType),
		http.StatusNotFound, codeNotFound, messageNotFound)
}

// PATCH renames: 200 with the organization, updatedAt by the clock and
// createdAt kept; the same name again changes nothing; an unknown id is 404.
// A later POST compares against the new name.
func TestPatchRenamesAndMovesUpdatedAtOnlyWhenTheNameChanges(t *testing.T) {
	eachServer(t, func(t *testing.T, s *Server, clock *testClock) {
		expectStatus(t, "POST A", postOrganization(s, createBody(orgA, "A")), http.StatusCreated)
		expectStatus(t, "POST B", postOrganization(s, createBody(orgB, "B")), http.StatusCreated)

		clock.Advance(time.Hour)
		got := call(s, http.MethodPatch, organizationTarget(orgA), []byte(nameBody("A, renamed")), jsonType)
		expectStatus(t, "PATCH", got, http.StatusOK)
		renamed := organizationReply{OrganizationID: orgA, Name: "A, renamed",
			CreatedAt: "2026-10-03T15:00:00.123Z", UpdatedAt: "2026-10-03T16:00:00.123Z"}
		if reply := organizationReplyOf(t, got); reply != renamed {
			t.Fatalf("PATCH answered %+v, want %+v", reply, renamed)
		}

		clock.Advance(time.Hour)
		same := call(s, http.MethodPatch, organizationTarget(orgA), []byte(nameBody("A, renamed")), jsonType)
		expectStatus(t, "PATCH to the same name", same, http.StatusOK)
		if reply := organizationReplyOf(t, same); reply != renamed {
			t.Fatalf("PATCH to the same name answered %+v, want %+v", reply, renamed)
		}
		if reply := getOrganizationReply(t, s, orgA); reply != renamed {
			t.Fatalf("GET after the PATCHes = %+v, want %+v", reply, renamed)
		}
		if reply := getOrganizationReply(t, s, orgB); reply.Name != "B" || reply.UpdatedAt != "2026-10-03T15:00:00.123Z" {
			t.Fatalf("B changed: %+v", reply)
		}

		expectError(t, "PATCH an unknown id", call(s, http.MethodPatch, organizationTarget(orgC), []byte(nameBody("C")), jsonType),
			http.StatusNotFound, codeNotFound, messageNotFound)
		expectError(t, "PATCH a non-UUID id", call(s, http.MethodPatch, organizationTarget("x"), []byte(nameBody("C")), jsonType),
			http.StatusBadRequest, codeInvalidRequest, "organizationId must be a UUID.")
		expectStatus(t, "POST with the new name", postOrganization(s, createBody(orgA, "A, renamed")), http.StatusOK)
		expectStatus(t, "POST with the old name", postOrganization(s, createBody(orgA, "A")), http.StatusConflict)
	})
}

// DELETE removes the organization and, by the cascade, every publication
// it owns, and nothing of another's. Its publication routes then answer
// organization_not_found; registered again, it is empty.
func TestDeleteOrganizationDeletesItsPublicationsAndNoOneElses(t *testing.T) {
	eachServer(t, func(t *testing.T, s *Server, _ *testClock) {
		for _, id := range []string{orgA, orgB} {
			expectStatus(t, "POST", postOrganization(s, createBody(id, "organization")), http.StatusCreated)
		}
		var ours, theirs []string
		for range 3 {
			ours = append(ours, uuid.NewString())
			theirs = append(theirs, uuid.NewString())
			expectStatus(t, "A's POST", publish(s, orgA, ours[len(ours)-1], validBody(sampleYAML)), http.StatusCreated)
			expectStatus(t, "B's POST", publish(s, orgB, theirs[len(theirs)-1], validBody(sampleYAML)), http.StatusCreated)
		}

		got := call(s, http.MethodDelete, organizationTarget(orgA), nil, "")
		if got.Code != http.StatusNoContent || got.Body.Len() != 0 {
			t.Fatalf("DELETE: %d %q, want 204 and no body", got.Code, got.Body.String())
		}
		for _, id := range ours {
			expectError(t, "GET A's publication", call(s, http.MethodGet, publicationTarget(orgA, id), nil, ""),
				http.StatusNotFound, codeOrganizationNotFound, messageOrganizationNotFound)
		}
		for _, id := range theirs {
			expectStatus(t, "GET B's publication", call(s, http.MethodGet, publicationTarget(orgB, id), nil, ""), http.StatusOK)
		}
		if page := list(t, s, orgB, "?configurationId="+configurationA); len(page.Publications) != 3 {
			t.Fatalf("B lists %d publications, want its 3", len(page.Publications))
		}
		expectError(t, "a second DELETE", call(s, http.MethodDelete, organizationTarget(orgA), nil, ""), http.StatusNotFound, codeNotFound, messageNotFound)
		expectError(t, "GET the deleted organization", call(s, http.MethodGet, organizationTarget(orgA), nil, ""), http.StatusNotFound, codeNotFound, messageNotFound)
		expectError(t, "a non-UUID id", call(s, http.MethodDelete, organizationTarget("x"), nil, ""),
			http.StatusBadRequest, codeInvalidRequest, "organizationId must be a UUID.")

		// Registered again, it is empty: its publications are gone, not
		// hidden.
		expectStatus(t, "POST A again", postOrganization(s, createBody(orgA, "organization")), http.StatusCreated)
		for _, id := range ours {
			expectError(t, "GET A's old publication", call(s, http.MethodGet, publicationTarget(orgA, id), nil, ""),
				http.StatusNotFound, codeNotFound, messageNotFound)
		}
		if page := list(t, s, orgA, "?configurationId="+configurationA); len(page.Publications) != 0 {
			t.Fatalf("A lists %d publications after its delete, want none", len(page.Publications))
		}
	})
}

// Every publication route under an organization that isn't registered
// answers 404 organization_not_found once its path ids are read, before
// anything else: an invalid body, query or Content-Type is not looked at,
// and nothing is stored. The configuration-wide delete, which answers 204
// whether or not any publication existed, answers 404 here. A path id that
// isn't a UUID is still a 400.
func TestEveryPublicationRouteAnswersOrganizationNotFoundFirst(t *testing.T) {
	eachServer(t, func(t *testing.T, s *Server, _ *testClock) {
		expectStatus(t, "POST A", postOrganization(s, createBody(orgA, "A")), http.StatusCreated)
		for _, c := range []struct {
			name, method, target, contentType string
			payload                           []byte
		}{
			{"POST", http.MethodPost, listTarget(orgC, ""), jsonType, validBody(sampleYAML).with("id", publicationA).bytes()},
			{"POST an invalid body", http.MethodPost, listTarget(orgC, ""), jsonType, []byte("not json")},
			{"POST with no Content-Type", http.MethodPost, listTarget(orgC, ""), "", validBody(sampleYAML).with("id", publicationA).bytes()},
			{"GET", http.MethodGet, publicationTarget(orgC, publicationA), "", nil},
			{"DELETE", http.MethodDelete, publicationTarget(orgC, publicationA), "", nil},
			{"list", http.MethodGet, listTarget(orgC, "?configurationId="+configurationA), "", nil},
			{"list without configurationId", http.MethodGet, listTarget(orgC, ""), "", nil},
			{"list with a bad limit and cursor", http.MethodGet, listTarget(orgC, "?configurationId=x&limit=0&cursor=y"), "", nil},
			{"DELETE a configuration's", http.MethodDelete, configurationTarget(orgC, configurationA), "", nil},
		} {
			got := call(s, c.method, c.target, c.payload, c.contentType)
			if got.Code != http.StatusNotFound || got.Body.String() != organizationNotFoundBody {
				t.Errorf("%s under an unregistered organization: %d %s, want 404 %s", c.name, got.Code, got.Body.String(), organizationNotFoundBody)
			}
		}
		// Nothing was stored under it: registered now, it holds nothing.
		expectStatus(t, "POST C", postOrganization(s, createBody(orgC, "C")), http.StatusCreated)
		expectError(t, "GET the POST's publication", call(s, http.MethodGet, publicationTarget(orgC, publicationA), nil, ""),
			http.StatusNotFound, codeNotFound, messageNotFound)
		expectStatus(t, "DELETE a configuration's, registered", call(s, http.MethodDelete, configurationTarget(orgC, configurationA), nil, ""),
			http.StatusNoContent)

		// Path ids are read first.
		expectError(t, "a non-UUID publication id under an unregistered organization",
			call(s, http.MethodGet, publicationTarget(orgB, "x"), nil, ""), http.StatusBadRequest, codeInvalidRequest, "publicationId must be a UUID.")
		expectError(t, "a non-UUID configuration id under an unregistered organization",
			call(s, http.MethodDelete, configurationTarget(orgB, "x"), nil, ""), http.StatusBadRequest, codeInvalidRequest, "configurationId must be a UUID.")
	})
}

// registeredBeforeDeleted says every organization is registered, as a
// registry would just before a concurrent DELETE removed one: what the
// handler asks first, before the store then refuses the POST.
type registeredBeforeDeleted struct{ organizations.Store }

func (registeredBeforeDeleted) Exists(context.Context, uuid.UUID) (bool, error) { return true, nil }

// An organization deleted between the handler's check and the POST's insert
// is refused by the store (the foreign key), and the POST answers 404
// organization_not_found, not 500.
func TestAPublicationPostWhoseOrganizationIsDeletedMeanwhileIsOrganizationNotFound(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		d := testDB(t)
		s, _, log := newServer(t, registeredBeforeDeleted{organizations.NewPostgres(d.Runtime)}, publications.NewPostgres(d.Runtime))
		expectError(t, "POST", publish(s, orgC, publicationA, validBody(sampleYAML)), http.StatusNotFound, codeOrganizationNotFound, messageOrganizationNotFound)
		if strings.Contains(log.String(), "store failed") {
			t.Fatalf("the refusal was logged as a failure: %s", log.String())
		}
	})
}

// A failing registry answers 500 internal_error on every route that asks
// it, the organization routes and every publication route, and the log
// line says which store and route failed without the error's text.
func TestAFailingRegistryIs500WithoutDetail(t *testing.T) {
	s, _, log := newServer(t, failingOrganizations{}, publications.NewPostgres(testDB(t).Runtime))
	requests := []struct {
		method, target string
		payload        []byte
	}{
		{http.MethodPost, organizationsPath, []byte(createBody(orgA, "A"))},
		{http.MethodGet, organizationTarget(orgA), nil},
		{http.MethodPatch, organizationTarget(orgA), []byte(nameBody("A"))},
		{http.MethodDelete, organizationTarget(orgA), nil},
		{http.MethodPost, listTarget(orgA, ""), validBody(sampleYAML).with("id", publicationA).bytes()},
		{http.MethodGet, publicationTarget(orgA, publicationA), nil},
		{http.MethodDelete, publicationTarget(orgA, publicationA), nil},
		{http.MethodGet, listTarget(orgA, "?configurationId="+configurationA), nil},
		{http.MethodDelete, configurationTarget(orgA, configurationA), nil},
	}
	for _, request := range requests {
		got := call(s, request.method, request.target, request.payload, jsonType)
		expectError(t, request.method+" "+request.target, got, http.StatusInternalServerError, codeInternalError, messageInternalError)
	}
	logged := log.String()
	if strings.Count(logged, `"msg":"the organizations store failed"`) != len(requests) {
		t.Fatalf("the failures weren't each logged: %s", logged)
	}
	if strings.Contains(logged, errStoreDown.Error()) {
		t.Fatalf("the log holds the store's error: %s", logged)
	}
}

// Through the real routes: the log holds no organization's name, no id from
// a path and no secret.
func TestTheOrganizationRoutesLogNoNames(t *testing.T) {
	s, _, log := registryServer(t)
	postOrganization(s, createBody(orgA, "name-marker-1d4's workspace"))
	postOrganization(s, createBody(orgA, "name-marker-1d4 conflicting"))
	postOrganization(s, nameBody(strings.Repeat("name-marker-1d4", 30)))
	call(s, http.MethodPatch, organizationTarget(orgA), []byte(nameBody("name-marker-1d4 renamed")), jsonType)
	call(s, http.MethodGet, organizationTarget(orgA), nil, "")
	publish(s, orgC, publicationA, validBody(sampleYAML))
	call(s, http.MethodDelete, organizationTarget(orgA), nil, "")

	logged := log.String()
	for _, leaked := range []string{"name-marker", testSecret, orgA, orgC, publicationA} {
		if strings.Contains(logged, leaked) {
			t.Errorf("the log holds %q: %s", leaked, logged)
		}
	}
	for _, route := range []string{organizationsPath, organizationPath} {
		if !strings.Contains(logged, `"route":"`+route+`"`) {
			t.Errorf("the log doesn't name the route pattern %s: %s", route, logged)
		}
	}
}
