package management

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/telflo/opamp-server/store/apikeys"
	"github.com/telflo/opamp-server/store/collectorgroups"
	"github.com/telflo/opamp-server/store/collectors"
	"github.com/telflo/opamp-server/store/database/databasetest"
	"github.com/telflo/opamp-server/store/enrollmenttokens"
	"github.com/telflo/opamp-server/store/fleets"
	"github.com/telflo/opamp-server/store/organizations"
	"github.com/telflo/opamp-server/store/publications"
	"github.com/telflo/opamp-server/store/variables"
	"github.com/telflo/opamp-server/tokens"
)

// The ids the publication tests use. Organization B is the one that must
// never see or change A's rows; organization C is registered nowhere.
const (
	orgA           = "0a0a0a0a-0000-4000-8000-00000000000a"
	orgB           = "0b0b0b0b-0000-4000-8000-00000000000b"
	orgC           = "0c0c0c0c-0000-4000-8000-00000000000c"
	publicationA   = "1a1a1a1a-0000-4000-8000-00000000001a"
	configurationA = "2a2a2a2a-0000-4000-8000-00000000002a"
	configurationB = "2b2b2b2b-0000-4000-8000-00000000002b"
	versionA       = "3a3a3a3a-0000-4000-8000-00000000003a"
	actorA         = "4a4a4a4a-0000-4000-8000-00000000004a"
	sampleYAML     = "receivers:\n  otlp:\n    protocols:\n      grpc: {}\n"
	samplePublish  = "2026-10-03T14:02:11.000Z"
	jsonType       = "application/json"
	// sampleDistribution and sampleCollectorVersion are the body's
	// collector. Not migration 00003's defaults, so a store that left the
	// columns to them would answer another collector.
	sampleDistribution     = "otelcol-sample"
	sampleCollectorVersion = "0.155.3"
)

// testClock is a clock a test moves by hand.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 10, 3, 15, 0, 0, 123456789, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// The enrollment tokens' keys the tests' servers hold: the current one
// mints, the previous one is still accepted.
const (
	testEncryptionKey         = "management-test-encryption-key-0123456789"
	testPreviousEncryptionKey = "management-test-previous-key-0123456789ab"
)

// testKeys is a keyring of the test keys, destroyed when the test ends.
func testKeys(t *testing.T) *tokens.Keyring {
	t.Helper()
	keys, err := tokens.NewKeyring(testEncryptionKey, []string{testPreviousEncryptionKey})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(keys.Destroy)
	return keys
}

// fleetStores are the fleet model's stores a Server is built with, and the
// API keys' (nil: newServerWith's Postgres store).
type fleetStores struct {
	fleets     fleets.Store
	groups     collectorgroups.Store
	tokens     enrollmenttokens.Store
	collectors collectors.Store
	variables  variables.Store
	apiKeys    apikeys.Store
}

func postgresFleetStores(pool *pgxpool.Pool) fleetStores {
	return fleetStores{fleets: fleets.NewPostgres(pool), groups: collectorgroups.NewPostgres(pool), tokens: enrollmenttokens.NewPostgres(pool),
		collectors: collectors.NewPostgres(pool), variables: variables.NewPostgres(pool), apiKeys: apikeys.NewPostgres(pool)}
}

// testDatabases is the running test's database (testDB), under its lock.
var testDatabases struct {
	sync.Mutex
	current *databasetest.Databases
}

// testDB is the running test's Postgres database: the first call in a test
// makes it fresh (databasetest.New), and every later call until that test
// ends, its subtests' and helpers' included, answers the same one. The
// package has one database name, so a second New would drop the first.
func testDB(t *testing.T) *databasetest.Databases {
	t.Helper()
	testDatabases.Lock()
	defer testDatabases.Unlock()
	if testDatabases.current == nil {
		testDatabases.current = databasetest.New(t, testDatabase)
		// Registered after New's drop, so it runs first.
		t.Cleanup(func() {
			testDatabases.Lock()
			defer testDatabases.Unlock()
			testDatabases.current = nil
		})
	}
	return testDatabases.current
}

// emptyTestDB empties the test's database (testDB), for a test that builds
// a server per case, each case on fresh rows (databasetest's Empty).
func emptyTestDB(t *testing.T) {
	t.Helper()
	if err := testDB(t).Empty(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// newServer is a Server over the two stores, with the test secret, a clock
// the test moves and a log it reads. Its fleet stores are on the test's
// database (testDB).
func newServer(t *testing.T, registry organizations.Store, store publications.Store) (*Server, *testClock, *syncBuffer) {
	t.Helper()
	return newServerWith(t, registry, store, postgresFleetStores(testDB(t).Runtime))
}

// newServerWith is newServer with the fleet stores given.
func newServerWith(t *testing.T, registry organizations.Store, store publications.Store, fs fleetStores) (*Server, *testClock, *syncBuffer) {
	t.Helper()
	return newServerAdjusted(t, registry, store, fs, nil)
}

// newServerAdjusted is newServerWith, its options changed by adjust (when
// not nil) before the server is built.
func newServerAdjusted(t *testing.T, registry organizations.Store, store publications.Store, fs fleetStores,
	adjust func(*Options)) (*Server, *testClock, *syncBuffer) {
	t.Helper()
	secrets, err := NewSecrets(testSecret, []string{testPrevious})
	if err != nil {
		t.Fatal(err)
	}
	clock := newTestClock()
	log := &syncBuffer{}
	if fs.apiKeys == nil {
		fs.apiKeys = apikeys.NewPostgres(testDB(t).Runtime)
	}
	options := Options{
		Secrets:          secrets,
		Organizations:    registry,
		Publications:     store,
		Fleets:           fs.fleets,
		CollectorGroups:  fs.groups,
		EnrollmentTokens: fs.tokens,
		Collectors:       fs.collectors,
		Variables:        fs.variables,
		APIKeys:          fs.apiKeys,
		Keys:             testKeys(t),
		Clock:            clock.Now,
		Logger:           slog.New(slog.NewJSONHandler(log, nil)),
	}
	if adjust != nil {
		adjust(&options)
	}
	s, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return s, clock, log
}

// apiServer is a Server on every Postgres store of the test's database
// (testDB), with organizations A and B registered, and not C.
func apiServer(t *testing.T) (*Server, *testClock, *syncBuffer) {
	t.Helper()
	return publicationsServer(t, publications.NewPostgres(testDB(t).Runtime))
}

// publicationsServer is a Server over store and the Postgres registry of the
// test's database (testDB), with organizations A and B registered, and not
// C.
func publicationsServer(t *testing.T, store publications.Store) (*Server, *testClock, *syncBuffer) {
	t.Helper()
	registry := organizations.NewPostgres(testDB(t).Runtime)
	register(t, registry, orgA, orgB)
	return newServer(t, registry, store)
}

// register registers each organization in registry, named after its id.
func register(t *testing.T, registry organizations.Store, ids ...string) {
	t.Helper()
	at := newTestClock().Now()
	for _, id := range ids {
		result, _, err := registry.Create(context.Background(), organizations.Organization{
			ID: uuid.MustParse(id), Name: "organization " + id, CreatedAt: at, UpdatedAt: at,
		})
		if err != nil || result != organizations.Created {
			t.Fatalf("registering %s: %v, %v", id, result, err)
		}
	}
}

func sha256Hex(s string) string {
	digest := sha256.Sum256([]byte(s))
	return hex.EncodeToString(digest[:])
}

// body is a POST body as a tree the tests change field by field.
type body map[string]any

// validBody is a POST body that is valid for yaml.
func validBody(yaml string) body {
	return body{
		"configuration": map[string]any{"id": configurationA, "name": "payments-gateway"},
		"version":       map[string]any{"id": versionA, "number": 3, "label": "TLS on"},
		"yaml":          yaml,
		"sha256":        sha256Hex(yaml),
		"publishedBy":   map[string]any{"id": actorA, "email": "ana@acme.io"},
		"publishedAt":   samplePublish,
		"collector":     map[string]any{"distribution": sampleDistribution, "version": sampleCollectorVersion},
	}
}

// with returns a deep copy of b with the field at path ("version.label")
// set to value.
func (b body) with(path string, value any) body {
	c := b.copy()
	parent, field := c.parent(path)
	parent[field] = value
	return c
}

// without returns a deep copy of b without the field at path.
func (b body) without(path string) body {
	c := b.copy()
	parent, field := c.parent(path)
	delete(parent, field)
	return c
}

// withYAML returns b with yaml and its matching sha256.
func (b body) withYAML(yaml string) body {
	return b.with("yaml", yaml).with("sha256", sha256Hex(yaml))
}

func (b body) parent(path string) (map[string]any, string) {
	parts := strings.Split(path, ".")
	node := map[string]any(b)
	for _, part := range parts[:len(parts)-1] {
		node = node[part].(map[string]any)
	}
	return node, parts[len(parts)-1]
}

func (b body) copy() body {
	raw, err := json.Marshal(b)
	if err != nil {
		panic(err)
	}
	var c body
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&c); err != nil {
		panic(err)
	}
	return c
}

func (b body) bytes() []byte {
	raw, err := json.Marshal(b)
	if err != nil {
		panic(err)
	}
	return raw
}

func publicationTarget(organizationID, publicationID string) string {
	return "/api/v1/organizations/" + organizationID + "/publications/" + publicationID
}

func listTarget(organizationID, query string) string {
	return "/api/v1/organizations/" + organizationID + "/publications" + query
}

func organizationTarget(organizationID string) string {
	return "/api/v1/organizations/" + organizationID
}

func configurationTarget(organizationID, configurationID string) string {
	return "/api/v1/organizations/" + organizationID + "/configurations/" + configurationID + "/publications"
}

// call sends one authenticated request through the server's handler.
func call(s *Server, method, target string, payload []byte, contentType string) *httptest.ResponseRecorder {
	var request *http.Request
	if payload == nil {
		request = httptest.NewRequest(method, target, nil)
	} else {
		request = httptest.NewRequest(method, target, bytes.NewReader(payload))
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	request.Header.Set("Authorization", "Bearer "+testSecret)
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, request)
	return recorder
}

// publish POSTs b under the caller's id publicationID.
func publish(s *Server, organizationID, publicationID string, b body) *httptest.ResponseRecorder {
	return post(s, organizationID, b.with("id", publicationID))
}

// post POSTs b as it is: without an id, the server makes one.
func post(s *Server, organizationID string, b body) *httptest.ResponseRecorder {
	return call(s, http.MethodPost, listTarget(organizationID, ""), b.bytes(), jsonType)
}

// reply is a publication as the API answers it, decoded loosely so a test
// can see a field that shouldn't be there.
type reply map[string]any

func decodeReply(t *testing.T, recorder *httptest.ResponseRecorder) reply {
	t.Helper()
	var decoded reply
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("the reply isn't JSON: %v: %s", err, recorder.Body.String())
	}
	return decoded
}

func expectStatus(t *testing.T, label string, recorder *httptest.ResponseRecorder, status int) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("%s: %d %s, want %d", label, recorder.Code, recorder.Body.String(), status)
	}
}

// expectError checks an error answer: its status, code and exact message.
func expectError(t *testing.T, label string, recorder *httptest.ResponseRecorder, status int, code errorCode, message string) {
	t.Helper()
	want, _ := json.Marshal(envelope{Error: envelopeError{Code: string(code), Message: message}})
	if recorder.Code != status || recorder.Body.String() != string(want) {
		t.Fatalf("%s: %d %s, want %d %s", label, recorder.Code, recorder.Body.String(), status, want)
	}
	if recorder.Header().Get("Content-Type") != jsonType {
		t.Fatalf("%s: Content-Type %q", label, recorder.Header().Get("Content-Type"))
	}
}

// failingOrganizations fails every call, as a database that is down would.
type failingOrganizations struct{}

func (failingOrganizations) Create(context.Context, organizations.Organization) (organizations.CreateResult, organizations.Organization, error) {
	return 0, organizations.Organization{}, errStoreDown
}

func (failingOrganizations) Get(context.Context, uuid.UUID) (organizations.Organization, error) {
	return organizations.Organization{}, errStoreDown
}

func (failingOrganizations) Exists(context.Context, uuid.UUID) (bool, error) {
	return false, errStoreDown
}

func (failingOrganizations) Rename(context.Context, uuid.UUID, string, time.Time) (organizations.Organization, error) {
	return organizations.Organization{}, errStoreDown
}

func (failingOrganizations) Delete(context.Context, uuid.UUID) (bool, error) {
	return false, errStoreDown
}

func (failingOrganizations) Limits(context.Context, uuid.UUID) (organizations.Limits, error) {
	return organizations.Limits{}, errStoreDown
}

func (failingOrganizations) SetLimits(context.Context, uuid.UUID, organizations.Limits) (organizations.Limits, error) {
	return organizations.Limits{}, errStoreDown
}

// failingStore fails every call, as a database that is down would.
type failingStore struct{}

var errStoreDown = errors.New("the store is down")

func (failingStore) Put(context.Context, publications.Publication) (publications.PutResult, publications.Publication, error) {
	return 0, publications.Publication{}, errStoreDown
}

func (failingStore) Get(context.Context, uuid.UUID, uuid.UUID) (publications.Publication, error) {
	return publications.Publication{}, errStoreDown
}

func (failingStore) List(context.Context, uuid.UUID, uuid.UUID, int, *publications.Cursor) (publications.Page, error) {
	return publications.Page{}, errStoreDown
}

func (failingStore) Delete(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	return false, errStoreDown
}

func (failingStore) DeleteForConfiguration(context.Context, uuid.UUID, uuid.UUID) (int, error) {
	return 0, errStoreDown
}

func (failingStore) Usage(context.Context, uuid.UUID, uuid.UUID) ([]publications.Usage, error) {
	return nil, errStoreDown
}
