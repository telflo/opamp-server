package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"

	"github.com/telflo/opamp-server/agent"
	"github.com/telflo/opamp-server/agent/agenttest"
	"github.com/telflo/opamp-server/management"
	"github.com/telflo/opamp-server/management/managementtest"
	"github.com/telflo/opamp-server/store/database/databasetest"
	"github.com/telflo/opamp-server/tokens"
)

// testDatabase is this package's test database: each package has its own
// name, since packages' tests run at once.
const testDatabase = "telflo_opamp_serve_test"

// The test credentials: the management secret, the server URL collectors
// are handed, and the enrollment tokens' keys (the current one mints, the
// previous one is still accepted).
const (
	testSecret                = "the-current-secret-0123456789abcdef"
	testServerURL             = "https://opamp.test.invalid/opamp/api/v1"
	testEncryptionKey         = "serve-test-encryption-key-0123456789abcde"
	testPreviousEncryptionKey = "serve-test-previous-key-0123456789abcdefg"
	jsonType                  = "application/json"
)

// envOf is a getenv over values.
func envOf(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// unsetNothing is an unsetenv for tests that don't watch it.
func unsetNothing(string) error { return nil }

// testDatabases is the running test's database (testDB), under its lock.
var testDatabases struct {
	sync.Mutex
	current *databasetest.Databases
}

// testDB is the running test's Postgres database: the first call in a test
// makes it fresh (databasetest.New), and every later call until that test
// ends answers the same one. The package has one database name, so a
// second New would drop the first.
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

// syncBuffer is a log the handlers' goroutines and the test can share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
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

// replicaSettings are serve's settings for a test replica: the test secret
// and keys, both listeners on 127.0.0.1 and a free port, the test server
// URL, a heartbeat of 45 s and a concurrency cap of 7 (neither a default,
// so a test sees they reach the replica), the other limits' defaults.
func replicaSettings(t *testing.T, keys *tokens.Keyring) Settings {
	t.Helper()
	secrets, err := management.NewSecrets(testSecret, nil)
	if err != nil {
		t.Fatal(err)
	}
	agentSettings, err := agent.LoadSettings(envOf(map[string]string{
		agent.ServerURLVariable: testServerURL, agent.HostVariable: "127.0.0.1", agent.PortVariable: "0",
		agent.HeartbeatIntervalVariable: "45", agent.MaxConcurrentRequestsVariable: "7",
	}))
	if err != nil {
		t.Fatal(err)
	}
	return Settings{Keys: keys, Management: management.Settings{Host: "127.0.0.1", Port: 0, Secrets: secrets}, Agent: agentSettings}
}

// testReplicaStores are every Postgres store, serve's own, on the test's
// database (testDB).
func testReplicaStores(t *testing.T) replicaStores {
	t.Helper()
	return postgresStores(testDB(t).Runtime)
}

// testReplica is a replica on stores, pinging with ping, on clock, logging
// to log.
func testReplica(t *testing.T, settings Settings, stores replicaStores, ping func(context.Context) error, clock *testClock, log *syncBuffer) *replica {
	t.Helper()
	r, err := newReplica(settings, stores, ping, clock.Now, slog.New(slog.NewJSONHandler(log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// api is the replica's management API, with the test secret.
func api(r *replica) managementtest.API {
	return managementtest.API{Handler: r.management.Handler(), Bearer: testSecret}
}

// The flow's markers: what must never be written down.
const (
	yamlMarker  = "receivers:\n  otlp: # yaml-marker-6c0d\n    protocols: {grpc: {}}\nexporters:\n  debug: {}\nservice:\n  pipelines:\n    traces: {receivers: [otlp], exporters: [debug]}\n"
	labelMarker = "fleet-label-marker-2f8a"
)

// agentMessage is a poll's AgentToServer naming the group (agenttest).
func agentMessage(t *testing.T, uid uuid.UUID, sequence uint64, group string) []byte {
	t.Helper()
	return agenttest.Message(t, uid, sequence, group)
}

// pollThrough sends one poll with bearer ("" for none) through an agent
// listener's handler (agenttest.Poll).
func pollThrough(t *testing.T, handler http.Handler, bearer string, message []byte) (*httptest.ResponseRecorder, *protobufs.ServerToAgent) {
	t.Helper()
	return agenttest.Poll(t, handler, bearer, message)
}

// pushedBearer is the bearer an answer's connection settings carry, or "".
func pushedBearer(answer *protobufs.ServerToAgent) string { return agenttest.PushedBearer(answer) }

// organizationsPath is the organization registry's route.
const organizationsPath = "/api/v1/organizations"

// fleetsTarget is an organization's fleets' route.
func fleetsTarget(organizationID string) string {
	return organizationsPath + "/" + organizationID + "/fleets"
}

// body is a JSON request body.
type body map[string]any

func (b body) bytes() []byte {
	encoded, err := json.Marshal(b)
	if err != nil {
		panic(err)
	}
	return encoded
}

// fleetBody is a fleet's create body, with no id when id is "".
func fleetBody(id, label, typ string) body {
	b := body{"label": label, "type": typ, "rotateExpiresAt": nil, "actor": managementtest.Actor}
	if id != "" {
		b["id"] = id
	}
	return b
}

// created is a fleet's create reply, decoded.
type created = managementtest.Fleet

// decodeInto decodes a JSON reply into into, failing the test otherwise.
func decodeInto(t *testing.T, reply []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(reply, into); err != nil {
		t.Fatalf("the reply isn't JSON: %v: %s", err, reply)
	}
}

// replicaFleet is a fleet made through a replica's management API.
type replicaFleet struct {
	organization string
	created      created
}

// fleetThrough registers an organization, publishes yamlMarker, creates a
// server fleet labelled labelMarker and assigns the published version to
// its Default group, all through r's management API.
func fleetThrough(t *testing.T, r *replica) replicaFleet {
	t.Helper()
	setup := api(r).SetUp(t, yamlMarker, labelMarker)
	return replicaFleet{organization: setup.Organization, created: setup.Fleet}
}
