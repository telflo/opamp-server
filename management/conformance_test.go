package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"

	"github.com/telflo/telflo-opamp-server/internal/diagnostics"
	"github.com/telflo/telflo-opamp-server/store/organizations"
	"github.com/telflo/telflo-opamp-server/store/publications"
)

// The contract test: api/openapi.yaml is what the platform's client and
// its test double are written against, so every documented operation and
// answer is sent through the real handler here, and the request and the
// reply are both validated against the file with kin-openapi. A route the
// server registers that the file doesn't document fails too.

const (
	specPath = "../api/openapi.yaml"
	// specServer is the file's server URL: the router matches on it.
	specServer = "http://localhost:51235"
)

// catchAlls are the server's patterns that are no route of their own: they
// answer 404 for whatever no route matches.
var catchAlls = []string{"/", apiPrefix}

// debugRoutes are the diagnostic routes (debug.go): behind the secret, an
// operator's and not the platform's, so the file leaves them out. The walk
// skips exactly these two patterns, by name: any other route under
// /api/v1/debug/, or another method on these paths, is still undocumented.
var debugRoutes = []string{diagnostics.ProfilesPattern, diagnostics.AdmissionPattern}

func init() {
	// The file's "uuid" format, which kin-openapi doesn't define itself.
	openapi3.DefineStringFormatValidator("uuid", openapi3.NewCallbackValidator(func(value string) error {
		if _, ok := parseUUID(value); !ok {
			return fmt.Errorf("not a UUID")
		}
		return nil
	}))
	// The file's "date-time" is JSON Schema's, RFC 3339's, which allows a
	// lowercase "t" and "z" (its section 5.6). kin-openapi's own pattern
	// wants them upper-case; this is that pattern without the case, so the
	// server is held to accept what the file allows.
	openapi3.DefineStringFormatValidator("date-time",
		openapi3.NewRegexpFormatValidator("(?i)"+openapi3.FormatOfStringDateTime))
}

// loadSpec loads and validates api/openapi.yaml, and inlines its schema
// references (see inlineSchemaRefs).
func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile(specPath)
	if err != nil {
		t.Fatalf("loading %s: %v", specPath, err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("%s isn't a valid OpenAPI document: %v", specPath, err)
	}
	if !doc.IsOpenAPI31OrLater() {
		t.Fatalf("%s is OpenAPI %s, want 3.1", specPath, doc.OpenAPI)
	}
	inlineSchemaRefs(doc)
	inlined, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(inlined), "#/components/schemas/") {
		t.Fatal("a schema reference survived inlineSchemaRefs: kin-openapi would validate it with its fallback validator")
	}
	return doc
}

// inlineSchemaRefs clears every schema's $ref, keeping the schema the
// loader resolved it to. kin-openapi validates OpenAPI 3.1 schemas with a
// JSON Schema 2020-12 validator that compiles each schema on its own, where
// a "#/components/…" reference can't resolve; it would then fall back,
// silently, to its older validator.
func inlineSchemaRefs(doc *openapi3.T) {
	seen := map[*openapi3.Schema]bool{}
	var walk func(ref *openapi3.SchemaRef)
	walk = func(ref *openapi3.SchemaRef) {
		if ref == nil {
			return
		}
		ref.Ref = ""
		schema := ref.Value
		if schema == nil || seen[schema] {
			return
		}
		seen[schema] = true
		for _, property := range schema.Properties {
			walk(property)
		}
		walk(schema.Items)
		walk(schema.Not)
		walk(schema.AdditionalProperties.Schema)
		for _, list := range []openapi3.SchemaRefs{schema.AllOf, schema.AnyOf, schema.OneOf} {
			for _, item := range list {
				walk(item)
			}
		}
	}
	walkContent := func(content openapi3.Content) {
		for _, media := range content {
			walk(media.Schema)
		}
	}
	walkResponses := func(responses *openapi3.Responses) {
		if responses == nil {
			return
		}
		for _, response := range responses.Map() {
			if response.Value == nil {
				continue
			}
			walkContent(response.Value.Content)
			for _, header := range response.Value.Headers {
				if header.Value != nil {
					walk(header.Value.Schema)
				}
			}
		}
	}
	for _, schema := range doc.Components.Schemas {
		walk(schema)
	}
	for _, parameter := range doc.Components.Parameters {
		walk(parameter.Value.Schema)
	}
	for _, response := range doc.Components.Responses {
		walkContent(response.Value.Content)
		for _, header := range response.Value.Headers {
			walk(header.Value.Schema)
		}
	}
	for _, item := range doc.Paths.Map() {
		for _, parameter := range item.Parameters {
			walk(parameter.Value.Schema)
		}
		for _, operation := range item.Operations() {
			for _, parameter := range operation.Parameters {
				walk(parameter.Value.Schema)
			}
			if operation.RequestBody != nil && operation.RequestBody.Value != nil {
				walkContent(operation.RequestBody.Value.Content)
			}
			walkResponses(operation.Responses)
		}
	}
}

// validationOptions are kin-openapi's, with the documented bearer scheme
// checked: a request passes only with "Bearer <something>". (Whether the
// secret is right is the server's business; the file says how to send it.)
func validationOptions() *openapi3filter.Options {
	return &openapi3filter.Options{
		IncludeResponseStatus: true,
		AuthenticationFunc: func(_ context.Context, input *openapi3filter.AuthenticationInput) error {
			scheme := input.SecurityScheme
			if scheme == nil || scheme.Type != "http" || !strings.EqualFold(scheme.Scheme, "bearer") {
				return fmt.Errorf("an undocumented security scheme")
			}
			token, ok := bearerToken(input.RequestValidationInput.Request.Header)
			if !ok || token == "" {
				return fmt.Errorf("no bearer token")
			}
			return nil
		},
	}
}

// documentedAnswers lists every "METHOD path status" the file documents.
func documentedAnswers(doc *openapi3.T) []string {
	var answers []string
	for path, item := range doc.Paths.Map() {
		for method, operation := range item.Operations() {
			for status := range operation.Responses.Map() {
				answers = append(answers, method+" "+path+" "+status)
			}
		}
	}
	slices.Sort(answers)
	return answers
}

// documentedErrorCodes lists every "METHOD path status code" the file documents:
// each error code a documented response names in its example or examples
// (one case for each code a status documents).
func documentedErrorCodes(doc *openapi3.T) []string {
	var answers []string
	for path, item := range doc.Paths.Map() {
		for method, operation := range item.Operations() {
			for status, response := range operation.Responses.Map() {
				if response.Value == nil {
					continue
				}
				media := response.Value.Content.Get(jsonType)
				if media == nil {
					continue
				}
				for _, code := range documentedCodes(media) {
					answers = append(answers, method+" "+path+" "+status+" "+code)
				}
			}
		}
	}
	slices.Sort(answers)
	return slices.Compact(answers)
}

// undocumentedRoutes lists every route the server registers that the file
// doesn't document (as a path with that method), catch-alls aside.
func undocumentedRoutes(doc *openapi3.T, s *Server) []string {
	var undocumented []string
	for _, pattern := range s.routes() {
		if slices.Contains(catchAlls, pattern) || slices.Contains(debugRoutes, pattern) {
			continue
		}
		method, path, found := strings.Cut(pattern, " ")
		if !found {
			// A pattern without a method answers every method.
			undocumented = append(undocumented, "every method "+pattern)
			continue
		}
		item := doc.Paths.Value(path)
		if item == nil || item.GetOperation(method) == nil {
			undocumented = append(undocumented, pattern)
		}
	}
	return undocumented
}

// unregisteredOperations lists every operation the file documents that the
// server doesn't register.
func unregisteredOperations(doc *openapi3.T, s *Server) []string {
	var missing []string
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			if !slices.Contains(s.routes(), method+" "+path) {
				missing = append(missing, method+" "+path)
			}
		}
	}
	return missing
}

// conformanceCase is one request and the documented answer it must get.
type conformanceCase struct {
	name        string
	method      string
	target      string
	contentType string
	payload     []byte
	// unauthenticated sends no Authorization header.
	unauthenticated bool
	// failingStore serves the request from a publications store that fails
	// (for 500s).
	failingStore bool
	// failingOrganizations serves it from a registry that fails.
	failingOrganizations bool
	// failingAPIKeys serves it from an API keys' store that fails.
	failingAPIKeys bool
	// notReady serves it from a server whose process isn't ready.
	notReady bool
	// apiKeyOf, when set, mints the API key the request authenticates with,
	// after the seed, in place of the secret.
	apiKeyOf func(t *testing.T, s *Server) string
	// apiKey is that key, once minted.
	apiKey string
	// seed runs against the store before the request.
	seed func(t *testing.T, s *Server)
	// targetOf, when set, is the request's target, read after the seed
	// (for an id the server made, such as a group's or a token's).
	targetOf func(t *testing.T, s *Server) string
	// payloadOf, when set, is the request's body, read after the seed (for
	// an id the server made, such as a collector's).
	payloadOf func(t *testing.T, s *Server) []byte
	// status is the answer the request must get.
	status int
	// conforms says whether the request itself follows the file. A request
	// that doesn't (a too-long name, a missing secret) must fail kin-openapi's
	// request validation too: the limit or the scheme is documented.
	conforms bool
}

// The operation's path in the file, for the coverage check.
func (c conformanceCase) answer(path string) string {
	return c.method + " " + path + " " + strconv.Itoa(c.status)
}

func seedPublication(organizationID, publicationID string, b body) func(t *testing.T, s *Server) {
	return func(t *testing.T, s *Server) {
		t.Helper()
		expectStatus(t, "seeding", publish(s, organizationID, publicationID, b), http.StatusCreated)
	}
}

func seedOrganization(id, name string) func(t *testing.T, s *Server) {
	return func(t *testing.T, s *Server) {
		t.Helper()
		got := call(s, http.MethodPost, organizationsPath, []byte(`{"id":"`+id+`","name":"`+name+`"}`), jsonType)
		expectStatus(t, "seeding an organization", got, http.StatusCreated)
	}
}

func conformanceCases() []conformanceCase {
	valid := validBody(sampleYAML)
	publication := publicationTarget(orgA, publicationA)
	posted := listTarget(orgA, "")
	withID := valid.with("id", publicationA)
	list := listTarget(orgA, "?configurationId="+configurationA)
	configuration := configurationTarget(orgA, configurationA)
	tooLarge := []byte(`{"yaml":"` + strings.Repeat("y", MaxBodyBytes) + `"}`)
	organizationsTarget := organizationsPath
	organization := organizationTarget(orgA)
	unknown := organizationTarget(orgC)
	created := []byte(`{"id":"` + orgC + `","name":"Ana's workspace"}`)
	renamed := []byte(`{"name":"Ana's renamed workspace"}`)
	tooLong := []byte(`{"name":"` + strings.Repeat("n", 401) + `"}`)
	tooLargeName := []byte(`{"name":"` + strings.Repeat("n", MaxBodyBytes) + `"}`)
	return append([]conformanceCase{
		{name: "health", method: http.MethodGet, target: "/health", unauthenticated: true, status: 200, conforms: true},
		{name: "ready", method: http.MethodGet, target: "/ready", unauthenticated: true, status: 200, conforms: true},
		{name: "not ready", method: http.MethodGet, target: "/ready", unauthenticated: true, notReady: true, status: 503, conforms: true},

		// The organization routes.
		{name: "post created", method: http.MethodPost, target: organizationsTarget, contentType: jsonType, payload: created, status: 201, conforms: true},
		{name: "post created without an id", method: http.MethodPost, target: organizationsTarget, contentType: jsonType,
			payload: []byte(`{"name":"Bo's workspace"}`), status: 201, conforms: true},
		{name: "post the same id and name", method: http.MethodPost, target: organizationsTarget, contentType: jsonType, payload: created,
			seed: seedOrganization(orgC, "Ana's workspace"), status: 200, conforms: true},
		{name: "post the same id, another name", method: http.MethodPost, target: organizationsTarget, contentType: jsonType, payload: created,
			seed: seedOrganization(orgC, "Bo's workspace"), status: 409, conforms: true},
		{name: "post a name over 400 characters", method: http.MethodPost, target: organizationsTarget, contentType: jsonType, payload: tooLong,
			status: 400, conforms: false},
		{name: "post an unknown field", method: http.MethodPost, target: organizationsTarget, contentType: jsonType,
			payload: []byte(`{"name":"a","members":[]}`), status: 400, conforms: false},
		{name: "post without the secret", method: http.MethodPost, target: organizationsTarget, contentType: jsonType, payload: created,
			unauthenticated: true, status: 401, conforms: false},
		{name: "post too large", method: http.MethodPost, target: organizationsTarget, contentType: jsonType, payload: tooLargeName, status: 413, conforms: false},
		{name: "post store down", method: http.MethodPost, target: organizationsTarget, contentType: jsonType, payload: created,
			failingOrganizations: true, status: 500, conforms: true},

		{name: "get organization", method: http.MethodGet, target: organization, status: 200, conforms: true},
		{name: "get organization invalid path id", method: http.MethodGet, target: organizationTarget("x"), status: 400, conforms: false},
		{name: "get organization without the secret", method: http.MethodGet, target: organization, unauthenticated: true, status: 401, conforms: false},
		{name: "get organization unknown", method: http.MethodGet, target: unknown, status: 404, conforms: true},
		{name: "get organization store down", method: http.MethodGet, target: organization, failingOrganizations: true, status: 500, conforms: true},

		{name: "patch organization", method: http.MethodPatch, target: organization, contentType: jsonType, payload: renamed, status: 200, conforms: true},
		{name: "patch organization invalid name", method: http.MethodPatch, target: organization, contentType: jsonType, payload: tooLong,
			status: 400, conforms: false},
		{name: "patch organization without the secret", method: http.MethodPatch, target: organization, contentType: jsonType, payload: renamed,
			unauthenticated: true, status: 401, conforms: false},
		{name: "patch organization unknown", method: http.MethodPatch, target: unknown, contentType: jsonType, payload: renamed, status: 404, conforms: true},
		{name: "patch organization too large", method: http.MethodPatch, target: organization, contentType: jsonType, payload: tooLargeName,
			status: 413, conforms: false},
		{name: "patch organization store down", method: http.MethodPatch, target: organization, contentType: jsonType, payload: renamed,
			failingOrganizations: true, status: 500, conforms: true},

		{name: "delete organization", method: http.MethodDelete, target: organization, seed: seedPublication(orgA, publicationA, valid), status: 204, conforms: true},
		{name: "delete organization invalid path id", method: http.MethodDelete, target: organizationTarget("x"), status: 400, conforms: false},
		{name: "delete organization without the secret", method: http.MethodDelete, target: organization, unauthenticated: true, status: 401, conforms: false},
		{name: "delete organization unknown", method: http.MethodDelete, target: unknown, status: 404, conforms: true},
		{name: "delete organization store down", method: http.MethodDelete, target: organization, failingOrganizations: true, status: 500, conforms: true},
		{name: "delete organization, already being deleted", method: http.MethodDelete, target: organization,
			seed: seedOrganizationDeleteRunning(orgA), status: 503, conforms: true},

		// Every publication route under an organization that isn't
		// registered, and with a registry that fails.
		{name: "post under an unregistered organization", method: http.MethodPost, target: listTarget(orgC, ""), contentType: jsonType,
			payload: valid.bytes(), status: 404, conforms: true},
		{name: "post registry down", method: http.MethodPost, target: posted, contentType: jsonType, payload: valid.bytes(),
			failingOrganizations: true, status: 500, conforms: true},
		{name: "get under an unregistered organization", method: http.MethodGet, target: publicationTarget(orgC, publicationA), status: 404, conforms: true},
		{name: "get registry down", method: http.MethodGet, target: publication, failingOrganizations: true, status: 500, conforms: true},
		{name: "delete under an unregistered organization", method: http.MethodDelete, target: publicationTarget(orgC, publicationA), status: 404, conforms: true},
		{name: "list under an unregistered organization", method: http.MethodGet, target: listTarget(orgC, "?configurationId="+configurationA),
			status: 404, conforms: true},
		{name: "list registry down", method: http.MethodGet, target: list, failingOrganizations: true, status: 500, conforms: true},
		{name: "delete configuration under an unregistered organization", method: http.MethodDelete, target: configurationTarget(orgC, configurationA),
			status: 404, conforms: true},
		{name: "delete configuration registry down", method: http.MethodDelete, target: configuration, failingOrganizations: true, status: 500, conforms: true},

		{name: "post created", method: http.MethodPost, target: posted, contentType: jsonType, payload: valid.bytes(), status: 201, conforms: true},
		{name: "post created, nullable fields null", method: http.MethodPost, target: posted, contentType: jsonType,
			payload: valid.with("version.label", nil).with("publishedBy", nil).bytes(), status: 201, conforms: true},
		// The server is no stricter than the file. JSON Schema's "integer"
		// includes 3.0, and RFC 3339 a lowercase t and z.
		{name: "post created, version.number 3.0", method: http.MethodPost, target: posted, contentType: jsonType,
			payload: valid.with("version.number", json.Number("3.0")).bytes(), status: 201, conforms: true},
		{name: "post created, publishedAt with a lowercase t and z", method: http.MethodPost, target: posted, contentType: jsonType,
			payload: valid.with("publishedAt", "2026-10-03t14:02:11.000z").bytes(), status: 201, conforms: true},
		{name: "post version.number 3.5", method: http.MethodPost, target: posted, contentType: jsonType,
			payload: valid.with("version.number", json.Number("3.5")).bytes(), status: 400, conforms: false},
		// The file refuses what the server refuses.
		{name: "post collector null", method: http.MethodPost, target: posted, contentType: jsonType,
			payload: valid.with("collector", nil).bytes(), status: 400, conforms: false},
		{name: "post collector without version", method: http.MethodPost, target: posted, contentType: jsonType,
			payload: valid.without("collector.version").bytes(), status: 400, conforms: false},
		{name: "post collector with an extra field", method: http.MethodPost, target: posted, contentType: jsonType,
			payload: valid.with("collector.image", "otel/x:1").bytes(), status: 400, conforms: false},
		{name: "post collector.version with a v", method: http.MethodPost, target: posted, contentType: jsonType,
			payload: valid.with("collector.version", "v0.162.0").bytes(), status: 400, conforms: false},
		{name: "post collector.distribution with an underscore", method: http.MethodPost, target: posted, contentType: jsonType,
			payload: valid.with("collector.distribution", "otelcol_contrib").bytes(), status: 400, conforms: false},
		{name: "post collector.distribution 65 characters", method: http.MethodPost, target: posted, contentType: jsonType,
			payload: valid.with("collector.distribution", strings.Repeat("a", 65)).bytes(), status: 400, conforms: false},
		{name: "post created, collector.distribution 64 characters", method: http.MethodPost, target: posted, contentType: jsonType,
			payload: valid.with("collector.distribution", strings.Repeat("a", 64)).bytes(), status: 201, conforms: true},
		{name: "post identical", method: http.MethodPost, target: posted, contentType: jsonType, payload: withID.bytes(),
			seed: seedPublication(orgA, publicationA, valid), status: 200, conforms: true},
		{name: "post created with an id", method: http.MethodPost, target: posted, contentType: jsonType, payload: withID.bytes(),
			status: 201, conforms: true},
		{name: "post an id that isn't a UUID", method: http.MethodPost, target: posted, contentType: jsonType,
			payload: valid.with("id", "not-a-uuid").bytes(), status: 400, conforms: false},
		{name: "post invalid name", method: http.MethodPost, target: posted, contentType: jsonType,
			payload: valid.with("configuration.name", strings.Repeat("n", 201)).bytes(), status: 400, conforms: false},
		// The file states no bound on a reference's NAME, so the request
		// follows it; the store refuses it before writing.
		{name: "post yaml referencing a name over 128 characters", method: http.MethodPost, target: posted, contentType: jsonType,
			payload: valid.withYAML("a: ${org:N_" + strings.Repeat("A", 127) + "}\n").bytes(), status: 400, conforms: true},
		{name: "post invalid path id", method: http.MethodPost, target: listTarget("not-a-uuid", ""), contentType: jsonType,
			payload: valid.bytes(), status: 400, conforms: false},
		{name: "post without the secret", method: http.MethodPost, target: posted, contentType: jsonType, payload: valid.bytes(),
			unauthenticated: true, status: 401, conforms: false},
		{name: "post conflict", method: http.MethodPost, target: posted, contentType: jsonType,
			payload: withID.withYAML(sampleYAML + "# changed\n").bytes(),
			seed:    seedPublication(orgA, publicationA, valid), status: 409, conforms: true},
		{name: "post too large", method: http.MethodPost, target: posted, contentType: jsonType, payload: tooLarge, status: 413, conforms: false},
		{name: "post sha256 mismatch", method: http.MethodPost, target: posted, contentType: jsonType,
			payload: valid.with("sha256", sha256Hex("something else")).bytes(), status: 422, conforms: true},
		{name: "post store down", method: http.MethodPost, target: posted, contentType: jsonType, payload: valid.bytes(),
			failingStore: true, status: 500, conforms: true},

		{name: "get", method: http.MethodGet, target: publication, seed: seedPublication(orgA, publicationA, valid), status: 200, conforms: true},
		{name: "get invalid path id", method: http.MethodGet, target: publicationTarget("not-a-uuid", publicationA), status: 400, conforms: false},
		{name: "get without the secret", method: http.MethodGet, target: publication, unauthenticated: true, status: 401, conforms: false},
		{name: "get unknown", method: http.MethodGet, target: publication, status: 404, conforms: true},
		{name: "get store down", method: http.MethodGet, target: publication, failingStore: true, status: 500, conforms: true},

		{name: "delete", method: http.MethodDelete, target: publication, seed: seedPublication(orgA, publicationA, valid), status: 204, conforms: true},
		{name: "delete invalid path id", method: http.MethodDelete, target: publicationTarget(orgA, "1"), status: 400, conforms: false},
		{name: "delete without the secret", method: http.MethodDelete, target: publication, unauthenticated: true, status: 401, conforms: false},
		{name: "delete unknown", method: http.MethodDelete, target: publication, status: 404, conforms: true},
		{name: "delete store down", method: http.MethodDelete, target: publication, failingStore: true, status: 500, conforms: true},

		{name: "list", method: http.MethodGet, target: list + "&limit=1", status: 200, conforms: true, seed: func(t *testing.T, s *Server) {
			seedPublication(orgA, publicationA, valid)(t, s)
			seedPublication(orgA, uuid.NewString(), valid.with("publishedAt", "2026-10-04T00:00:00Z"))(t, s)
		}},
		{name: "list, the last page", method: http.MethodGet, target: list, seed: seedPublication(orgA, publicationA, valid), status: 200, conforms: true},
		{name: "list without configurationId", method: http.MethodGet, target: listTarget(orgA, ""), status: 400, conforms: false},
		{name: "list with limit over 100", method: http.MethodGet, target: list + "&limit=101", status: 400, conforms: false},
		{name: "list without the secret", method: http.MethodGet, target: list, unauthenticated: true, status: 401, conforms: false},
		{name: "list store down", method: http.MethodGet, target: list, failingStore: true, status: 500, conforms: true},

		{name: "delete configuration", method: http.MethodDelete, target: configuration, seed: seedPublication(orgA, publicationA, valid), status: 204, conforms: true},
		{name: "delete configuration, none", method: http.MethodDelete, target: configuration, status: 204, conforms: true},
		{name: "delete configuration invalid path id", method: http.MethodDelete, target: configurationTarget(orgA, "x"), status: 400, conforms: false},
		{name: "delete configuration without the secret", method: http.MethodDelete, target: configuration, unauthenticated: true, status: 401, conforms: false},
		{name: "delete configuration store down", method: http.MethodDelete, target: configuration, failingStore: true, status: 500, conforms: true},
	}, slices.Concat(fleetConformanceCases(), collectorConformanceCases(), variableConformanceCases(), codeConformanceCases(),
		apiKeyConformanceCases())...)
}

func newRequest(c conformanceCase) *http.Request {
	var request *http.Request
	if c.payload == nil {
		request = httptest.NewRequest(c.method, specServer+c.target, nil)
	} else {
		request = httptest.NewRequest(c.method, specServer+c.target, bytes.NewReader(c.payload))
	}
	if c.contentType != "" {
		request.Header.Set("Content-Type", c.contentType)
	}
	switch {
	case c.apiKey != "":
		request.Header.Set("Authorization", "Bearer "+c.apiKey)
	case !c.unauthenticated:
		request.Header.Set("Authorization", "Bearer "+testSecret)
	}
	return request
}

// validateExchange validates one request against the file (expecting it to
// conform or not), sends it, and validates the reply. It returns the
// documented path the request matched, and the error reply's code ("" for
// a success).
func validateExchange(t *testing.T, doc *openapi3.T, router routers.Router, s *Server, c conformanceCase) (string, string) {
	t.Helper()
	ctx := context.Background()
	options := validationOptions()

	route, pathParams, err := router.FindRoute(newRequest(c))
	if err != nil {
		t.Fatalf("%s: the file has no operation for %s %s: %v", c.name, c.method, c.target, err)
	}
	requestInput := &openapi3filter.RequestValidationInput{
		Request:    newRequest(c),
		PathParams: pathParams,
		Route:      route,
		Options:    options,
	}
	requestErr := openapi3filter.ValidateRequest(ctx, requestInput)
	switch {
	case c.conforms && requestErr != nil:
		t.Fatalf("%s: the request doesn't follow the file: %v", c.name, requestErr)
	case !c.conforms && requestErr == nil:
		t.Fatalf("%s: the request breaks a rule the file doesn't state (kin-openapi accepted it)", c.name)
	}

	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, newRequest(c))
	if recorder.Code != c.status {
		t.Fatalf("%s: %d %s, want %d", c.name, recorder.Code, recorder.Body.String(), c.status)
	}
	responseInput := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: requestInput,
		Status:                 recorder.Code,
		Header:                 recorder.Header(),
		Body:                   io.NopCloser(bytes.NewReader(recorder.Body.Bytes())),
		Options:                options,
	}
	if err := openapi3filter.ValidateResponse(ctx, responseInput); err != nil {
		t.Fatalf("%s: the reply doesn't follow the file: %v\nreply: %.500s", c.name, err, recorder.Body.String())
	}
	if err := undocumentedErrorCode(route.Operation, recorder.Code, recorder.Body.Bytes()); err != nil {
		t.Fatalf("%s: %v", c.name, err)
	}
	var reply envelope
	if recorder.Code >= 400 {
		_ = json.Unmarshal(recorder.Body.Bytes(), &reply)
	}
	return route.Path, string(reply.Error.Code)
}

// undocumentedErrorCode fails when an error reply's code isn't one the
// documented response names in its example or examples: the schema allows
// every code everywhere, so this is what tells an organization_not_found
// from a not_found on a route that documents only one of them. A response
// documented as plain text only (a probe's, /ready's 503) carries no
// envelope; the schema validation checks its text.
func undocumentedErrorCode(operation *openapi3.Operation, status int, body []byte) error {
	if status < 400 {
		return nil
	}
	response := operation.Responses.Status(status)
	if response == nil || response.Value == nil {
		return fmt.Errorf("no documented response for %d", status)
	}
	media := response.Value.Content.Get(jsonType)
	if media == nil && response.Value.Content.Get("text/plain") != nil {
		return nil
	}
	if media == nil {
		return fmt.Errorf("the documented %d has no JSON content", status)
	}
	var reply envelope
	if err := json.Unmarshal(body, &reply); err != nil {
		return fmt.Errorf("the error reply isn't the envelope: %v", err)
	}
	documented := documentedCodes(media)
	if !slices.Contains(documented, string(reply.Error.Code)) {
		return fmt.Errorf("answered %d %s, but the documented response names only %v", status, reply.Error.Code, documented)
	}
	return nil
}

// documentedCodes lists the error codes a response's example and examples
// show.
func documentedCodes(media *openapi3.MediaType) []string {
	var examples []any
	if media.Example != nil {
		examples = append(examples, media.Example)
	}
	for _, example := range media.Examples {
		if example != nil && example.Value != nil {
			examples = append(examples, example.Value.Value)
		}
	}
	var codes []string
	for _, example := range examples {
		raw, err := json.Marshal(example)
		if err != nil {
			continue
		}
		var reply envelope
		if json.Unmarshal(raw, &reply) == nil && reply.Error.Code != "" {
			codes = append(codes, string(reply.Error.Code))
		}
	}
	return codes
}

func TestEveryDocumentedAnswerConformsToOpenAPI(t *testing.T) {
	doc := loadSpec(t)
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}

	answered, answeredCodes := map[string]bool{}, map[string]bool{}
	for _, c := range conformanceCases() {
		emptyTestDB(t)
		var s *Server
		switch {
		case c.failingStore:
			s, _, _ = publicationsServer(t, failingStore{})
		case c.failingOrganizations:
			s, _, _ = newServer(t, failingOrganizations{}, publications.NewPostgres(testDB(t).Runtime))
		case c.failingAPIKeys:
			s = failingAPIKeysServer(t)
		case c.notReady:
			s, _, _ = newServerAdjusted(t, organizations.NewPostgres(testDB(t).Runtime), publications.NewPostgres(testDB(t).Runtime),
				postgresFleetStores(testDB(t).Runtime), func(o *Options) { o.Ready = notReady })
		default:
			s, _, _ = apiServer(t)
		}
		if c.seed != nil {
			c.seed(t, s)
		}
		if c.targetOf != nil {
			c.target = c.targetOf(t, s)
		}
		if c.payloadOf != nil {
			c.payload = c.payloadOf(t, s)
		}
		if c.apiKeyOf != nil {
			c.apiKey = c.apiKeyOf(t, s)
		}
		path, code := validateExchange(t, doc, router, s, c)
		answered[c.answer(path)] = true
		answeredCodes[c.answer(path)+" "+code] = true
	}

	for _, answer := range documentedAnswers(doc) {
		if !answered[answer] {
			t.Errorf("no case gets the documented answer %s", answer)
		}
	}
	for _, answer := range documentedErrorCodes(doc) {
		if !answeredCodes[answer] {
			t.Errorf("no case gets the documented code %s", answer)
		}
	}
}

func TestEveryRegisteredRouteIsDocumentedAndEveryOperationRegistered(t *testing.T) {
	doc := loadSpec(t)
	s, _, _ := apiServer(t)
	if undocumented := undocumentedRoutes(doc, s); len(undocumented) != 0 {
		t.Errorf("routes %s doesn't document: %v", specPath, undocumented)
	}
	if missing := unregisteredOperations(doc, s); len(missing) != 0 {
		t.Errorf("operations %s documents that the server doesn't register: %v", specPath, missing)
	}
	// The walk sees the real routes: the four organization routes, the five
	// publication routes and a configuration's usage, the sixteen fleet,
	// group and token routes, the six collector routes and the two limits
	// routes, the eleven variables routes, the five API key routes, /health
	// and /ready, beside the two catch-alls and the two diagnostic routes the
	// file leaves out by name.
	var routes []string
	for _, pattern := range s.routes() {
		if !slices.Contains(catchAlls, pattern) && !slices.Contains(debugRoutes, pattern) {
			routes = append(routes, pattern)
		}
	}
	for _, pattern := range debugRoutes {
		if !slices.Contains(s.routes(), pattern) {
			t.Errorf("the diagnostic route %s isn't registered", pattern)
		}
	}
	if len(routes) != 52 {
		t.Errorf("the server registers %v, want the four organization routes, the six publication routes, "+
			"the sixteen fleet routes, the six collector routes, the two limits routes, the eleven variables routes, "+
			"the five API key routes, /health and /ready", routes)
	}
}

// The check above must fail on a route the file doesn't document: a server
// with one extra route, and one with an extra method on a documented path.
func TestTheRouteWalkFailsOnAnUndocumentedRoute(t *testing.T) {
	doc := loadSpec(t)
	for _, extra := range []string{
		"GET /api/v1/organizations/{organizationId}/undocumented",
		"PATCH " + publicationPath,
		"PUT " + organizationPath,
		"GET " + organizationsPath,
		"PUT " + fleetPath,
		"GET " + rotatePath,
		"POST " + enrollmentTokensPath + "/{tokenId}",
		"POST " + collectorPath,
		"GET " + collectorsPinPath,
		"DELETE " + collectorErrorsPath,
		"PATCH " + limitsPath,
		"DELETE " + usagePath,
		"GET " + variablePath,
		"PATCH " + variablesPath,
		"PUT " + variableGroupPath,
		"GET " + overridePath,
		"/api/v1/any-method",
		// The diagnostic routes' exclusion is by name, not by prefix.
		"GET /api/v1/debug/another",
		"POST " + diagnostics.AdmissionPath,
		"GET " + diagnostics.PprofPrefix + "heap",
	} {
		emptyTestDB(t)
		s, _, _ := apiServer(t)
		s.api.handle(extra, http.HandlerFunc(notFound))
		undocumented := undocumentedRoutes(doc, s)
		if len(undocumented) != 1 || !strings.HasSuffix(undocumented[0], extra) {
			t.Errorf("with %q registered, the walk found %v", extra, undocumented)
		}
	}
}

// The validation the conformance test relies on is strict: a reply with a
// field missing, a field too many, or a null where none is allowed fails.
func TestTheReplyValidationCatchesAMalformedReply(t *testing.T) {
	doc := loadSpec(t)
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	s, _, _ := apiServer(t)
	seedPublication(orgA, publicationA, validBody(sampleYAML))(t, s)
	c := conformanceCase{method: http.MethodGet, target: publicationTarget(orgA, publicationA), status: 200}
	route, pathParams, err := router.FindRoute(newRequest(c))
	if err != nil {
		t.Fatal(err)
	}
	good := decodeReply(t, call(s, http.MethodGet, c.target, nil, ""))

	variants := map[string]func(reply){
		"yaml missing":           func(r reply) { delete(r, "yaml") },
		"an extra field":         func(r reply) { r["layout"] = map[string]any{} },
		"sha256 in capitals":     func(r reply) { r["sha256"] = strings.ToUpper(r["sha256"].(string)) },
		"version.number null":    func(r reply) { r["version"].(map[string]any)["number"] = nil },
		"version.label empty":    func(r reply) { r["version"].(map[string]any)["label"] = "" },
		"publishedBy without id": func(r reply) { delete(r["publishedBy"].(map[string]any), "id") },
		// The collector is an object of exactly its two fields, each in its
		// format.
		"collector without version":     func(r reply) { delete(r["collector"].(map[string]any), "version") },
		"collector null":                func(r reply) { r["collector"] = nil },
		"collector with an extra field": func(r reply) { r["collector"].(map[string]any)["image"] = "otel/x:1" },
		"collector.version two parts":   func(r reply) { r["collector"].(map[string]any)["version"] = "0.162" },
		"collector.distribution upper":  func(r reply) { r["collector"].(map[string]any)["distribution"] = "Otelcol" },
		"a non-UUID id":                 func(r reply) { r["id"] = "not-a-uuid" },
		"publishedAt not a time":        func(r reply) { r["publishedAt"] = "yesterday" },
	}
	for label, change := range variants {
		bad := body(good).copy()
		change(reply(bad))
		input := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{
				Request: newRequest(c), PathParams: pathParams, Route: route, Options: validationOptions(),
			},
			Status:  http.StatusOK,
			Header:  http.Header{"Content-Type": {jsonType}},
			Body:    io.NopCloser(bytes.NewReader(bad.bytes())),
			Options: validationOptions(),
		}
		if err := openapi3filter.ValidateResponse(context.Background(), input); err == nil {
			t.Errorf("a reply with %s passed validation", label)
		}
	}
}

// The code check above must fail on a code the documented response doesn't
// name: organization_not_found from GET …/organizations/{id}, whose 404 is
// not_found, and not_found from POST …/publications, whose 404 is
// organization_not_found. The documented ones pass.
func TestTheCodeCheckSeesAnUndocumentedErrorCode(t *testing.T) {
	doc := loadSpec(t)
	organizationNotFound, _ := json.Marshal(envelope{Error: envelopeError{Code: string(codeOrganizationNotFound), Message: messageOrganizationNotFound}})
	notFound, _ := json.Marshal(envelope{Error: envelopeError{Code: string(codeNotFound), Message: messageNotFound}})
	for _, c := range []struct {
		path, method string
		body         []byte
		documented   bool
	}{
		{organizationPath, http.MethodGet, organizationNotFound, false},
		{organizationPath, http.MethodGet, notFound, true},
		{publicationsPath, http.MethodPost, notFound, false},
		{publicationsPath, http.MethodPost, organizationNotFound, true},
		{publicationPath, http.MethodGet, notFound, true},
		{publicationPath, http.MethodGet, organizationNotFound, true},
	} {
		operation := doc.Paths.Value(c.path).GetOperation(c.method)
		err := undocumentedErrorCode(operation, http.StatusNotFound, c.body)
		if (err == nil) != c.documented {
			t.Errorf("%s %s with %s: %v, want documented=%v", c.method, c.path, c.body, err, c.documented)
		}
	}
}

// notReady is a process's readiness while it starts, drains or can't reach
// its database, as package serve answers it.
var notReady = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("not ready\n"))
})
