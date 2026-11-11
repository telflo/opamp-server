package management

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-telemetry/opamp-go/protobufs"

	"github.com/telflo/telflo-opamp-server/agent"
	"github.com/telflo/telflo-opamp-server/agent/agenttest"
	"github.com/telflo/telflo-opamp-server/store/apikeys"
	"github.com/telflo/telflo-opamp-server/store/collectorgroups"
	"github.com/telflo/telflo-opamp-server/store/collectors"
	"github.com/telflo/telflo-opamp-server/store/enrollmenttokens"
	"github.com/telflo/telflo-opamp-server/store/fleets"
	"github.com/telflo/telflo-opamp-server/store/organizations"
	"github.com/telflo/telflo-opamp-server/store/publications"
	"github.com/telflo/telflo-opamp-server/store/variables"
	"github.com/telflo/telflo-opamp-server/tokens"
)

// The agent endpoint's tests that need data made through this API (a
// fleet, its tokens, a published version) build both components on one
// database, as serve builds them for one process, without the process
// (package serve tests that).

// pairSettings are the two components' settings for a test replica.
type pairSettings struct {
	Keys  *tokens.Keyring
	Agent agent.Settings
}

// replicaSettings are a test replica's settings: the test keys, the test
// server URL, a heartbeat of 45 s and a concurrency cap of 7 (neither a
// default, so a test sees they reach the endpoint), the other limits'
// defaults.
func replicaSettings(t *testing.T, keys *tokens.Keyring) pairSettings {
	t.Helper()
	settings, err := agent.LoadSettings(envOf(map[string]string{
		agent.ServerURLVariable: testServerURL, agent.HeartbeatIntervalVariable: "45", agent.MaxConcurrentRequestsVariable: "7",
	}))
	if err != nil {
		t.Fatal(err)
	}
	return pairSettings{Keys: keys, Agent: settings}
}

// replicaStores are every Postgres store, on one pool.
type replicaStores struct {
	organizations    organizations.Store
	publications     publications.Store
	fleets           fleets.Store
	collectorGroups  collectorgroups.Store
	enrollmentTokens enrollmenttokens.Store
	collectors       collectors.Store
	variables        variables.Store
	apiKeys          apikeys.Store
}

// postgresStores are every store on pool.
func postgresStores(pool *pgxpool.Pool) replicaStores {
	return replicaStores{
		organizations:    organizations.NewPostgres(pool),
		publications:     publications.NewPostgres(pool),
		fleets:           fleets.NewPostgres(pool),
		collectorGroups:  collectorgroups.NewPostgres(pool),
		enrollmentTokens: enrollmenttokens.NewPostgres(pool),
		collectors:       collectors.NewPostgres(pool),
		variables:        variables.NewPostgres(pool),
		apiKeys:          apikeys.NewPostgres(pool),
	}
}

// testReplicaStores are every store on the test's database (testDB).
func testReplicaStores(t *testing.T) replicaStores {
	t.Helper()
	return postgresStores(testDB(t).Runtime)
}

// replica is the management API and the agent endpoint's handler, on the
// same stores, the API's admission view reading the endpoint's gate.
type replica struct {
	management *Server
	agent      http.Handler
}

// testReplica is a replica on stores, on clock, logging to log.
func testReplica(t *testing.T, settings pairSettings, stores replicaStores, clock *testClock, log *syncBuffer) *replica {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(log, nil))
	endpoint, err := agent.New(agent.Options{
		Settings: settings.Agent,
		Stores: agent.Stores{Organizations: stores.organizations, Collectors: stores.collectors,
			Tokens: stores.enrollmentTokens, Variables: stores.variables},
		Keys: settings.Keys, Ready: http.HandlerFunc(alwaysReady), Now: clock.Now, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := NewSecrets(testSecret, nil)
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Options{
		Secrets: secrets, Organizations: stores.organizations, Publications: stores.publications, Fleets: stores.fleets,
		CollectorGroups: stores.collectorGroups, EnrollmentTokens: stores.enrollmentTokens, Collectors: stores.collectors,
		Variables: stores.variables, APIKeys: stores.apiKeys, Keys: settings.Keys, Clock: clock.Now, Logger: logger,
		Admission: endpoint.Admission,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &replica{management: server, agent: endpoint.Handler()}
}

// registerOrganization registers id through the replica's management API.
func registerOrganization(t *testing.T, s *Server, id, name string) {
	t.Helper()
	got := call(s, http.MethodPost, organizationsPath, body{"id": id, "name": name}.bytes(), jsonType)
	expectStatus(t, "registering an organization", got, http.StatusCreated)
}

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

// pushedYAML is the configuration an answer pushes, or "".
func pushedYAML(answer *protobufs.ServerToAgent) string { return agenttest.PushedYAML(answer) }
