package management

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/telflo/opamp-server/internal/diagnostics"
)

// The flow's markers: what must never be written down.
const (
	yamlMarker  = "receivers:\n  otlp: # yaml-marker-6c0d\n    protocols: {grpc: {}}\nexporters:\n  debug: {}\nservice:\n  pipelines:\n    traces: {receivers: [otlp], exporters: [debug]}\n"
	labelMarker = "fleet-label-marker-2f8a"
)

// replicaFleet is a fleet made through a replica's management API: its
// organization, its 201, and the published version assigned to its Default
// group.
type replicaFleet struct {
	organization string
	created      created
	publication  string
}

// fleetThrough registers an organization, publishes yamlMarker, creates a
// server fleet labelled labelMarker and assigns the published version to
// its Default group, all through s.
func fleetThrough(t *testing.T, s *Server) replicaFleet {
	t.Helper()
	organization := uuid.NewString()
	registerOrganization(t, s, organization, "an organization")
	publication := uuid.NewString()
	expectStatus(t, "publishing", publish(s, organization, publication, validBody(yamlMarker)), http.StatusCreated)
	c := createFleet(t, s, organization, uuid.NewString(), labelMarker, "server")
	assigned := call(s, http.MethodPatch, groupTarget(organization, c.Fleet.ID, c.DefaultGroup.ID),
		body{"assignedPublicationId": publication}.bytes(), jsonType)
	expectStatus(t, "assigning", assigned, http.StatusOK)
	return replicaFleet{organization: organization, created: c, publication: publication}
}

// Two replicas built as serve builds them (each its own caches and
// limiters), on one database and one injected clock. A collector enrolls
// through A with the fleet's default token and is handed the rotate token;
// it polls through B with it and is served the assigned configuration; B's
// management API lists it. A revocation made through A's management API is
// refused by both replicas once the token cache's 10 s pass, and not
// before. Through the whole flow no bearer, key, secret, YAML or label
// reaches a log line or a span.
func TestTwoReplicasOnOneDatabase(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	d := testDB(t)
	keys := testKeys(t)
	settings := replicaSettings(t, keys)
	clock := newTestClock()
	logA, logB := &syncBuffer{}, &syncBuffer{}
	a := testReplica(t, settings, postgresStores(d.Runtime), clock, logA)
	b := testReplica(t, settings, postgresStores(d.Runtime), clock, logB)
	f := fleetThrough(t, a.management)
	uid := uuid.New()

	rec, answer := pollThrough(t, a.agent, f.created.Tokens.Default.Bearer, agentMessage(t, uid, 1, f.created.DefaultGroup.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("the default token's poll through A: %d %s", rec.Code, rec.Body.String())
	}
	opamp := answer.GetConnectionSettings().GetOpamp()
	handed := pushedBearer(answer)
	if handed == "" || opamp.GetDestinationEndpoint() != testServerURL || opamp.GetHeartbeatIntervalSeconds() != 45 {
		t.Fatalf("A's hand-off: %v", opamp)
	}
	rec, answer = pollThrough(t, b.agent, handed, agentMessage(t, uid, 2, f.created.DefaultGroup.ID))
	if rec.Code != http.StatusOK || pushedYAML(answer) != yamlMarker {
		t.Fatalf("the handed token's poll through B: %d, pushed %q", rec.Code, pushedYAML(answer))
	}
	// Each replica's admission view is its own gate's, at the setting.
	admission := call(b.management, http.MethodGet, diagnostics.AdmissionPath, nil, "")
	var view map[string]any
	if decodeInto(t, admission.Body.Bytes(), &view); view["max_in_flight"] != float64(7) || view["enabled"] != true {
		t.Fatalf("B's admission view: %s", admission.Body.String())
	}
	listed := call(b.management, http.MethodGet, collectorsTarget(f.organization, f.created.Fleet.ID), nil, "")
	var page struct {
		Collectors []struct {
			ID          string `json:"id"`
			InstanceUID string `json:"instanceUid"`
		} `json:"collectors"`
	}
	expectStatus(t, "B's list", listed, http.StatusOK)
	if decodeInto(t, listed.Body.Bytes(), &page); len(page.Collectors) != 1 {
		t.Fatalf("B lists %d collectors: %s", len(page.Collectors), listed.Body.String())
	}

	// Both replicas hold the rotate token's row now; the revocation
	// commits through A at the same instant.
	for name, r := range map[string]*replica{"A": a, "B": b} {
		if rec, _ := pollThrough(t, r.agent, handed, agentMessage(t, uid, 3, f.created.DefaultGroup.ID)); rec.Code != http.StatusOK {
			t.Fatalf("%s before the revocation: %d", name, rec.Code)
		}
	}
	revoked := call(a.management, http.MethodPost, revokeTarget(f.organization, f.created.Fleet.ID, f.created.Tokens.Rotate.ID),
		actorBody().bytes(), jsonType)
	expectStatus(t, "revoking through A", revoked, http.StatusOK)
	clock.Advance(10*time.Second - time.Nanosecond) // the TTL the docs promise
	for name, r := range map[string]*replica{"A": a, "B": b} {
		if rec, _ := pollThrough(t, r.agent, handed, agentMessage(t, uid, 4, f.created.DefaultGroup.ID)); rec.Code != http.StatusOK {
			t.Fatalf("%s just under 10 s after the revocation: %d (its cache wasn't used)", name, rec.Code)
		}
	}
	clock.Advance(time.Nanosecond)
	for name, r := range map[string]*replica{"A": a, "B": b} {
		if rec, _ := pollThrough(t, r.agent, handed, agentMessage(t, uid, 5, f.created.DefaultGroup.ID)); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s 10 s after the revocation: %d, want 401", name, rec.Code)
		}
	}

	logs := logA.String() + logB.String()
	var traced strings.Builder
	for _, span := range spans.Ended() {
		fmt.Fprintf(&traced, "%s %v %v %v\n", span.Name(), span.Attributes(), span.Status(), span.Events())
	}
	if !strings.Contains(logs, "token_revoked") || !strings.Contains(traced.String(), "opamp.outcome") {
		t.Fatalf("the capture holds no refusal or outcome: logs %s; spans %s", logs, traced.String())
	}
	for _, written := range []struct{ where, text string }{{"the logs", logs}, {"the spans", traced.String()}} {
		for _, secret := range []string{f.created.Tokens.Default.Bearer, f.created.Tokens.Rotate.Bearer, handed,
			testEncryptionKey, testPreviousEncryptionKey, testSecret, "yaml-marker", labelMarker, "Bearer", "Authorization"} {
			if strings.Contains(written.text, secret) {
				t.Errorf("%s hold %q", written.where, secret)
			}
		}
	}
}
