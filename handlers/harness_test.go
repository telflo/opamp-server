package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"google.golang.org/protobuf/proto"

	"github.com/telflo/opamp-server/middleware"
	"github.com/telflo/opamp-server/store/collectorgroups"
	"github.com/telflo/opamp-server/store/collectors"
	"github.com/telflo/opamp-server/store/enrollmenttokens"
	"github.com/telflo/opamp-server/store/fleets"
	"github.com/telflo/opamp-server/store/orgvariables"
	"github.com/telflo/opamp-server/store/publications"
	"github.com/telflo/opamp-server/store/storetest"
	"github.com/telflo/opamp-server/store/variables"
	"github.com/telflo/opamp-server/tokens"
)

// testDatabase is the Postgres tests' database (storetest.Each).
const testDatabase = "telflo_opamp_handlers_test"

// keySetting is the test keyring's current key setting.
const keySetting = "handlers-test-key-setting-0123456789abcdef"

// serverURL is the OPAMP_SERVER_URL the tests' handler pushes.
const serverURL = "https://opamp.test.invalid/opamp/api/v1"

// clientIP is the address the tests' Principal carries.
const clientIP = "203.0.113.7"

// testClock is an injected clock a test moves by hand.
type testClock struct {
	mu  sync.Mutex
	now time.Time
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

// syncWriter serializes writes: a handler logs from several goroutines.
type syncWriter struct {
	mu sync.Mutex
	b  *strings.Builder
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// testStores is the fleet model on this package's fresh Postgres database,
// with organizations A and B registered (storetest.Postgres).
func testStores(t *testing.T) *storetest.Stores {
	t.Helper()
	return storetest.Postgres(t, testDatabase)
}

// harness is a poll on a set of stores, with its clock, logs, keyring,
// authentication, guard and caches. The values it fills in come from the
// stores' variables store.
type harness struct {
	t       *testing.T
	s       *storetest.Stores
	keyring *tokens.Keyring
	clock   *testClock
	logs    *strings.Builder
	auth    *middleware.Authenticator
	guard   *middleware.CollectorGuard
	cache   *YamlCache
	vars    *VariablesCache
	h       *AgentHandler
}

// newHarness builds the poll on s; each change may replace an option (a
// store wrapped to fail or count).
func newHarness(t *testing.T, s *storetest.Stores, changes ...func(*Options)) *harness {
	t.Helper()
	k, err := tokens.NewKeyring(keySetting, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(k.Destroy)
	x := &harness{t: t, s: s, keyring: k, clock: &testClock{now: storetest.Now}, logs: &strings.Builder{},
		cache: NewYamlCache(), vars: NewVariablesCache()}
	logger := slog.New(slog.NewJSONHandler(&syncWriter{b: x.logs}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	x.auth = middleware.NewAuthenticator(k, s.Tokens, x.clock.Now, logger)
	x.guard = middleware.NewCollectorGuard(middleware.DefaultCollectorRequestsPerMinute, x.clock.Now, logger)
	o := Options{
		Collectors: s.Collectors, Tokens: s.Tokens, Variables: s.Variables,
		Keyring: k, Authenticator: x.auth, Guard: x.guard, ServerURL: serverURL,
		HeartbeatInterval: 30 * time.Second, Cache: x.cache, VariablesCache: x.vars, Now: x.clock.Now, Logger: logger,
	}
	for _, change := range changes {
		change(&o)
	}
	if x.h, err = NewAgentHandler(o); err != nil {
		t.Fatal(err)
	}
	return x
}

// replica is a second poll on the same stores, with its own caches (YAML
// and variables), clock reading and guard: another pod. Each change may
// replace an option, as newHarness's.
func (x *harness) replica(changes ...func(*Options)) *harness {
	y := *x
	y.cache = NewYamlCache()
	y.vars = NewVariablesCache()
	logger := slog.New(slog.NewJSONHandler(&syncWriter{b: x.logs}, nil))
	o := Options{
		Collectors: x.s.Collectors, Tokens: x.s.Tokens, Variables: x.s.Variables,
		Keyring: x.keyring, Authenticator: middleware.NewAuthenticator(x.keyring, x.s.Tokens, x.clock.Now, logger),
		Guard:     middleware.NewCollectorGuard(middleware.DefaultCollectorRequestsPerMinute, x.clock.Now, logger),
		ServerURL: serverURL, HeartbeatInterval: 30 * time.Second, Cache: y.cache, VariablesCache: y.vars,
		Now: x.clock.Now, Logger: logger,
	}
	for _, change := range changes {
		change(&o)
	}
	h, err := NewAgentHandler(o)
	if err != nil {
		x.t.Fatal(err)
	}
	y.h = h
	return &y
}

// fleet creates a fleet of the organization at the clock's now.
func (x *harness) fleet(org uuid.UUID, label string, typ fleets.Type) fleets.Creation {
	x.t.Helper()
	result, created, err := x.s.Fleets.Create(context.Background(), fleets.NewFleet{
		ID: uuid.New(), OrganizationID: org, Label: label, Type: typ, CreatedBy: storetest.Ana,
		RotateExpiresAt: storetest.Ptr(x.clock.Now().AddDate(1, 0, 0)), At: x.clock.Now(),
	})
	if err != nil || result != fleets.Created {
		x.t.Fatalf("creating fleet %q: %v, %v", label, result, err)
	}
	return created
}

// publish stores a published version of yaml for the organization.
func (x *harness) publish(org uuid.UUID, yaml string) uuid.UUID {
	x.t.Helper()
	id := uuid.New()
	if result, _, err := x.s.Publications.Put(context.Background(), publications.Publication{
		ID: id, OrganizationID: org, ConfigurationID: uuid.New(), ConfigurationName: "payments-gateway",
		VersionID: uuid.New(), VersionNumber: 3, YAML: yaml, SHA256: sha(yaml),
		PublishedAt: x.clock.Now(), ReceivedAt: x.clock.Now(),
		Collector: publications.Collector{Distribution: "otelcol-contrib", Version: "0.162.0"},
	}); err != nil || result != publications.Created {
		x.t.Fatalf("publishing: %v, %v", result, err)
	}
	return id
}

// variable creates the organization's variable name of value through the
// variables store, at the clock's now, and returns its id.
func (x *harness) variable(org uuid.UUID, name, value string) uuid.UUID {
	x.t.Helper()
	id := uuid.New()
	if result, _, err := x.s.Variables.CreateVariable(context.Background(), variables.NewVariable{
		ID: id, OrganizationID: org, Name: name, Value: value, Actor: storetest.Ana, At: x.clock.Now(),
	}); err != nil || result != variables.Created {
		x.t.Fatalf("creating a variable: %v, %v", result, err)
	}
	return id
}

// setValue changes the organization's variable's value.
func (x *harness) setValue(org, id uuid.UUID, value string) {
	x.t.Helper()
	if _, err := x.s.Variables.UpdateVariable(context.Background(), org, id, variables.VariableChanges{Value: &value},
		storetest.Ana, x.clock.Now()); err != nil {
		x.t.Fatalf("changing a value: %v", err)
	}
}

// variableGroup creates a variable group of the organization and returns
// its id.
func (x *harness) variableGroup(org uuid.UUID, name string) uuid.UUID {
	x.t.Helper()
	id := uuid.New()
	if result, _, err := x.s.Variables.CreateGroup(context.Background(), variables.NewGroup{
		ID: id, OrganizationID: org, Name: name, Actor: storetest.Ana, At: x.clock.Now(),
	}); err != nil || result != variables.Created {
		x.t.Fatalf("creating a variable group: %v, %v", result, err)
	}
	return id
}

// override sets the variable group's override of the variable.
func (x *harness) override(org, group, variable uuid.UUID, value string) {
	x.t.Helper()
	if _, _, err := x.s.Variables.SetOverride(context.Background(), org, group, variable, value, storetest.Ana, x.clock.Now()); err != nil {
		x.t.Fatalf("setting an override: %v", err)
	}
}

// resetOverride resets the variable group's override of the variable.
func (x *harness) resetOverride(org, group, variable uuid.UUID) {
	x.t.Helper()
	if reset, err := x.s.Variables.ResetOverride(context.Background(), org, group, variable, storetest.Ana, x.clock.Now()); err != nil || !reset {
		x.t.Fatalf("resetting an override: %v, %v", reset, err)
	}
}

// bind sets the fleet's collector group's variable group (nil clears it).
func (x *harness) bind(c fleets.Creation, group uuid.UUID, variableGroup *uuid.UUID) {
	x.t.Helper()
	change := collectorgroups.Clear[uuid.UUID]()
	if variableGroup != nil {
		change = collectorgroups.SetTo(*variableGroup)
	}
	if _, err := x.s.Groups.Update(context.Background(), c.Fleet.OrganizationID, c.Fleet.ID, group,
		collectorgroups.Changes{VariableGroupID: change}, x.clock.Now()); err != nil {
		x.t.Fatalf("binding a variable group: %v", err)
	}
}

// forgetNames removes the names stored at publish for the published
// version, as a version published before migration 00012 has none: written
// under the stores, past Put (the server's role may not delete them).
func (x *harness) forgetNames(publication uuid.UUID) {
	x.t.Helper()
	if _, err := x.s.Databases.Owner.Exec(context.Background(), `
		delete from telflo_opamp.publication_variable_names where publication_id = $1`, publication); err != nil {
		x.t.Fatal(err)
	}
}

// deleteVariable deletes the organization's variable.
func (x *harness) deleteVariable(org, id uuid.UUID) {
	x.t.Helper()
	if err := x.s.Variables.DeleteVariable(context.Background(), org, id); err != nil {
		x.t.Fatalf("deleting a variable: %v", err)
	}
}

// pinVariableGroup pins the variable group on the collector (nil clears
// it).
func (x *harness) pinVariableGroup(c fleets.Creation, collector uuid.UUID, variableGroup *uuid.UUID) {
	x.t.Helper()
	if _, err := x.s.Collectors.Update(context.Background(), c.Fleet.OrganizationID, c.Fleet.ID, collector,
		collectors.Changes{PinnedVariableGroup: &collectors.VariableGroupChange{ID: variableGroup}}, x.clock.Now()); err != nil {
		x.t.Fatalf("pinning a variable group: %v", err)
	}
}

// assign assigns the published version to the fleet's group.
func (x *harness) assign(c fleets.Creation, group, publication uuid.UUID) {
	x.t.Helper()
	if _, err := x.s.Groups.Update(context.Background(), c.Fleet.OrganizationID, c.Fleet.ID, group,
		collectorgroups.Changes{AssignedPublicationID: collectorgroups.SetTo(publication)}, x.clock.Now()); err != nil {
		x.t.Fatalf("assigning: %v", err)
	}
}

// pin pins ref on the collector.
func (x *harness) pin(c fleets.Creation, collector uuid.UUID, ref *collectors.Ref) {
	x.t.Helper()
	if _, err := x.s.Collectors.Pin(context.Background(), c.Fleet.OrganizationID, c.Fleet.ID, collector, ref, x.clock.Now()); err != nil {
		x.t.Fatalf("pinning: %v", err)
	}
}

// principal is the Principal a token of the fleet authenticates as.
func principal(c fleets.Creation, kind enrollmenttokens.Kind) middleware.Principal {
	token := c.RotateTokenID
	if kind == enrollmenttokens.KindDefault {
		token = c.DefaultTokenID
	}
	return middleware.Principal{OrganizationID: c.Fleet.OrganizationID, FleetID: c.Fleet.ID, FleetType: c.Fleet.Type,
		TokenID: token, TokenKind: kind, ClientIP: clientIP}
}

func rotate(c fleets.Creation) middleware.Principal { return principal(c, enrollmenttokens.KindRotate) }

func defaultToken(c fleets.Creation) middleware.Principal {
	return principal(c, enrollmenttokens.KindDefault)
}

// send runs a raw request through the poll with the Principal p (nil for
// none).
func (x *harness) send(p *middleware.Principal, method string, body []byte, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, middleware.AgentRoute, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-protobuf")
	for k, v := range header {
		req.Header[k] = v
	}
	if p != nil {
		req = req.WithContext(middleware.WithPrincipal(req.Context(), *p))
	}
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, req)
	return rec
}

// poll sends msg with the Principal p and decodes a 200's answer (nil
// otherwise). A description that names no group is sent naming the fleet's
// Default group (namingDefault), as every install names its group; pollAsIs
// sends a message unchanged.
func (x *harness) poll(p middleware.Principal, msg *protobufs.AgentToServer, serverName ...string) (*httptest.ResponseRecorder, *protobufs.ServerToAgent) {
	x.t.Helper()
	return x.pollAsIs(p, x.namingDefault(p, msg), serverName...)
}

// namingDefault is msg naming p's fleet's Default group when it carries a
// description without telflo.collector_group_id, else msg itself: a
// description that has the attribute (a blank one included) or no
// description at all is left as it is.
func (x *harness) namingDefault(p middleware.Principal, msg *protobufs.AgentToServer) *protobufs.AgentToServer {
	x.t.Helper()
	desc := msg.GetAgentDescription()
	if desc == nil {
		return msg
	}
	for _, kv := range desc.GetNonIdentifyingAttributes() {
		if kv.GetKey() == attributeGroupID {
			return msg
		}
	}
	groups, err := x.s.Groups.List(context.Background(), p.OrganizationID, p.FleetID)
	if err != nil || len(groups) == 0 || !groups[0].IsDefault {
		// A fleet that is gone or another organization's: sent unchanged.
		return msg
	}
	named := proto.Clone(msg).(*protobufs.AgentToServer)
	named.AgentDescription.NonIdentifyingAttributes = append(named.AgentDescription.NonIdentifyingAttributes,
		&protobufs.KeyValue{Key: attributeGroupID, Value: &protobufs.AnyValue{
			Value: &protobufs.AnyValue_StringValue{StringValue: groups[0].ID.String()}}})
	return named
}

// pollAsIs is poll without namingDefault: msg sent exactly as given.
func (x *harness) pollAsIs(p middleware.Principal, msg *protobufs.AgentToServer, serverName ...string) (*httptest.ResponseRecorder, *protobufs.ServerToAgent) {
	x.t.Helper()
	body, err := proto.Marshal(msg)
	if err != nil {
		x.t.Fatal(err)
	}
	header := http.Header{}
	if len(serverName) > 0 {
		header.Set("ServerName", serverName[0])
	}
	rec := x.send(&p, http.MethodPost, body, header)
	return rec, decode(x.t, rec)
}

// ok polls and fails the test unless the answer is 200.
func (x *harness) ok(p middleware.Principal, msg *protobufs.AgentToServer, serverName ...string) *protobufs.ServerToAgent {
	x.t.Helper()
	rec, answer := x.poll(p, msg, serverName...)
	if rec.Code != http.StatusOK {
		x.t.Fatalf("poll: %d %q; want 200", rec.Code, rec.Body.String())
	}
	return answer
}

// pollBearer sends msg with the bearer through the authentication, then
// the poll: the request as the agent stack would carry it, msg unchanged
// (a new collector's message names its group itself: withGroup).
func (x *harness) pollBearer(bearer string, msg *protobufs.AgentToServer) (*httptest.ResponseRecorder, *protobufs.ServerToAgent) {
	x.t.Helper()
	body, err := proto.Marshal(msg)
	if err != nil {
		x.t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, middleware.AgentRoute, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	x.auth.Middleware()(x.h).ServeHTTP(rec, req)
	return rec, decode(x.t, rec)
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) *protobufs.ServerToAgent {
	t.Helper()
	if rec.Code != http.StatusOK {
		return nil
	}
	answer := &protobufs.ServerToAgent{}
	if err := proto.Unmarshal(rec.Body.Bytes(), answer); err != nil {
		t.Fatalf("the answer isn't a ServerToAgent: %v", err)
	}
	return answer
}

// collector is the fleet's collector of the instance uid, with what
// resolves for it, or fails the test.
func (x *harness) collector(c fleets.Creation, uid uuid.UUID) collectors.Listed {
	x.t.Helper()
	l, ok := x.findCollector(c, uid)
	if !ok {
		x.t.Fatalf("no collector %s in the fleet", uid)
	}
	return l
}

func (x *harness) findCollector(c fleets.Creation, uid uuid.UUID) (collectors.Listed, bool) {
	x.t.Helper()
	page, err := x.s.Collectors.List(context.Background(), c.Fleet.OrganizationID, c.Fleet.ID, collectors.ListOptions{
		Sort: collectors.SortCreated, Order: collectors.Ascending, Limit: collectors.MaxListLimit})
	if err != nil {
		x.t.Fatal(err)
	}
	for _, l := range page.Collectors {
		if l.InstanceUID == uid {
			listed, err := x.s.Collectors.Get(context.Background(), c.Fleet.OrganizationID, c.Fleet.ID, l.ID)
			if err != nil {
				x.t.Fatal(err)
			}
			return listed, true
		}
	}
	return collectors.Listed{}, false
}

// errors is the collector's error history, newest first.
func (x *harness) errors(c fleets.Creation, id uuid.UUID) []collectors.CollectorError {
	x.t.Helper()
	page, err := x.s.Collectors.ListErrors(context.Background(), c.Fleet.OrganizationID, c.Fleet.ID, id, collectors.MaxListLimit, nil)
	if err != nil {
		x.t.Fatal(err)
	}
	return page.Errors
}

// logLines decodes the harness's JSON log lines.
func (x *harness) logLines() []map[string]any {
	x.t.Helper()
	var lines []map[string]any
	for _, raw := range strings.Split(x.logs.String(), "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			x.t.Fatalf("a log line isn't JSON: %v", err)
		}
		lines = append(lines, line)
	}
	return lines
}

// The messages a test sends.

type msgOption func(*protobufs.AgentToServer)

// message is a poll's AgentToServer. It carries an agent description, an
// empty one unless an option sets it, as a supervisor's full state does: a
// collector the organization doesn't hold is enrolled only by a message
// with one (withoutDescription leaves it out, as a supervisor's later
// messages do while nothing changed).
func message(uid uuid.UUID, seq uint64, opts ...msgOption) *protobufs.AgentToServer {
	m := &protobufs.AgentToServer{InstanceUid: append([]byte(nil), uid[:]...), SequenceNum: seq,
		Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_ReportsStatus |
			protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig |
			protobufs.AgentCapabilities_AgentCapabilities_ReportsEffectiveConfig |
			protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth),
		AgentDescription: &protobufs.AgentDescription{}}
	for _, o := range opts {
		o(m)
	}
	return m
}

// withoutDescription leaves the agent description out, as a supervisor's
// message after its first does while nothing changed.
func withoutDescription() msgOption {
	return func(m *protobufs.AgentToServer) { m.AgentDescription = nil }
}

// withSettings advertises AcceptsOpAMPConnectionSettings (0x100).
func withSettings() msgOption {
	return func(m *protobufs.AgentToServer) { m.Capabilities |= acceptsConnectionSettings }
}

func kvs(attributes map[string]string) []*protobufs.KeyValue {
	var out []*protobufs.KeyValue
	for k, v := range attributes {
		out = append(out, &protobufs.KeyValue{Key: k, Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: v}}})
	}
	return out
}

func withDescription(identifying, nonIdentifying map[string]string) msgOption {
	return func(m *protobufs.AgentToServer) {
		m.AgentDescription = &protobufs.AgentDescription{IdentifyingAttributes: kvs(identifying),
			NonIdentifyingAttributes: kvs(nonIdentifying)}
	}
}

// withGroup makes the description name the group id
// (telflo.collector_group_id), keeping its other attributes.
func withGroup(id uuid.UUID) msgOption {
	return func(m *protobufs.AgentToServer) {
		if m.AgentDescription == nil {
			m.AgentDescription = &protobufs.AgentDescription{}
		}
		m.AgentDescription.NonIdentifyingAttributes = append(m.AgentDescription.NonIdentifyingAttributes,
			kvs(map[string]string{attributeGroupID: id.String()})...)
	}
}

func withStatus(yaml string, status protobufs.RemoteConfigStatuses, errorMessage string) msgOption {
	return func(m *protobufs.AgentToServer) {
		sum := sha256.Sum256([]byte(yaml))
		m.RemoteConfigStatus = &protobufs.RemoteConfigStatus{LastRemoteConfigHash: sum[:], Status: status, ErrorMessage: errorMessage}
	}
}

func withHealth(healthy bool, lastError string) msgOption {
	return func(m *protobufs.AgentToServer) {
		m.Health = &protobufs.ComponentHealth{Healthy: healthy, LastError: lastError}
	}
}

func withEffective(body, contentType string) msgOption {
	return func(m *protobufs.AgentToServer) {
		m.EffectiveConfig = &protobufs.EffectiveConfig{ConfigMap: &protobufs.AgentConfigMap{
			ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: []byte(body), ContentType: contentType}}}}
	}
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// fullState reports whether the answer asks for the agent's full state.
func fullState(a *protobufs.ServerToAgent) bool {
	return a.GetFlags()&uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportFullState) != 0
}

// pushed is the pushed otelcol.yml's text, or "" when nothing was pushed.
func pushed(t *testing.T, a *protobufs.ServerToAgent) string {
	t.Helper()
	rc := a.GetRemoteConfig()
	if rc == nil {
		return ""
	}
	files := rc.GetConfig().GetConfigMap()
	file := files["otelcol.yml"]
	if len(files) != 1 || file == nil || file.GetContentType() != "text/yaml" {
		t.Fatalf("the push isn't one text/yaml otelcol.yml: %v", files)
	}
	sum := sha256.Sum256(file.GetBody())
	if !bytes.Equal(rc.GetConfigHash(), sum[:]) {
		t.Fatalf("the push's ConfigHash isn't the SHA-256 of its bytes")
	}
	return string(file.GetBody())
}

// settingsHeader is the named header of the answer's pushed connection
// settings, or "".
func settingsHeader(a *protobufs.ServerToAgent, key string) string {
	for _, h := range a.GetConnectionSettings().GetOpamp().GetHeaders().GetHeaders() {
		if h.GetKey() == key {
			return h.GetValue()
		}
	}
	return ""
}

// spy wraps a collectors store and the variables store, counting their
// calls and failing the ones a test sets.
type spy struct {
	collectors.Store
	vars                                   orgvariables.Store
	mu                                     sync.Mutex
	polls, yamls, reports, enrolls, errors int
	variableReads                          int
	failPoll, failReport, failYAML         error
	failVariables                          error
	// beforeVariables, when set, runs once before the next variables read
	// is delegated: writes that commit between the poll statement and its
	// cache miss's read.
	beforeVariables func()
	yamlOrgs        []uuid.UUID
}

func (s *spy) Variables(ctx context.Context, organizationID uuid.UUID, groupVariableGroupID, pinnedVariableGroupID *uuid.UUID) (orgvariables.Resolved, error) {
	s.mu.Lock()
	s.variableReads++
	fail, before := s.failVariables, s.beforeVariables
	s.beforeVariables = nil
	s.mu.Unlock()
	if before != nil {
		before()
	}
	if fail != nil {
		return orgvariables.Resolved{}, fail
	}
	return s.vars.Variables(ctx, organizationID, groupVariableGroupID, pinnedVariableGroupID)
}

func (s *spy) Poll(ctx context.Context, p collectors.PollRequest) (collectors.PollResult, error) {
	s.mu.Lock()
	s.polls++
	fail := s.failPoll
	s.mu.Unlock()
	if fail != nil {
		return collectors.PollResult{}, fail
	}
	return s.Store.Poll(ctx, p)
}

func (s *spy) YAML(ctx context.Context, organizationID uuid.UUID, ref collectors.Ref) (collectors.YAMLResult, error) {
	s.mu.Lock()
	s.yamls++
	s.yamlOrgs = append(s.yamlOrgs, organizationID)
	fail := s.failYAML
	s.mu.Unlock()
	if fail != nil {
		return collectors.YAMLResult{}, fail
	}
	return s.Store.YAML(ctx, organizationID, ref)
}

func (s *spy) Report(ctx context.Context, r collectors.Report) (bool, error) {
	s.mu.Lock()
	s.reports++
	fail := s.failReport
	s.mu.Unlock()
	if fail != nil {
		return false, fail
	}
	return s.Store.Report(ctx, r)
}

func (s *spy) Enroll(ctx context.Context, e collectors.Enrollment) (collectors.Enrolled, error) {
	s.mu.Lock()
	s.enrolls++
	s.mu.Unlock()
	return s.Store.Enroll(ctx, e)
}

func (s *spy) RecordError(ctx context.Context, organizationID, fleetID, collectorID uuid.UUID, e collectors.ErrorReport) error {
	s.mu.Lock()
	s.errors++
	s.mu.Unlock()
	return s.Store.RecordError(ctx, organizationID, fleetID, collectorID, e)
}

func (s *spy) counts() (polls, yamls, reports, enrolls, errs, variableReads int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.polls, s.yamls, s.reports, s.enrolls, s.errors, s.variableReads
}

// withSpy wraps the harness's collectors and variables stores in a spy.
func withSpy(s *spy) func(*Options) {
	return func(o *Options) {
		s.Store, s.vars = o.Collectors, o.Variables
		o.Collectors, o.Variables = s, s
	}
}
