package handlers

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-telemetry/opamp-go/protobufs"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	"github.com/telflo/opamp-server/middleware"
	"github.com/telflo/opamp-server/store/collectorgroups"
	"github.com/telflo/opamp-server/store/fleets"
	"github.com/telflo/opamp-server/store/organizations"
	"github.com/telflo/opamp-server/store/storetest"
	"github.com/telflo/opamp-server/store/variables"
	"github.com/telflo/opamp-server/tokens"
)

// The privacy canaries.
const (
	canaryYAML      = "CANARY-PUBLISHED-YAML-7f3a"
	canaryAttribute = "CANARY-ATTRIBUTE-VALUE-91c2"
	canaryError     = "CANARY-COLLECTOR-ERROR-5d8e"
	canaryHealth    = "CANARY-HEALTH-LAST-ERROR-0b6f"
	canaryName      = "canary-collector-name-4e1d"
	canaryLabel     = "canary-fleet-label-c09a"
	canaryGroup     = "canary-group-name-2b77"
	canaryVariable  = "CANARY-VARIABLE-VALUE-a5e0"
	canaryDBDetail  = "CANARY-DATABASE-DETAIL-66d1"
	// The variables': a variable's name and description, an override's
	// value, a variable group's name and description, and a value only a
	// text over the cap holds.
	canaryVariableName  = "CANARY_VARIABLE_NAME_e71b"
	canaryDescription   = "canary-variable-description-08f4"
	canaryOverride      = "CANARY-OVERRIDE-VALUE-3d9c"
	canaryVariableGroup = "canary-variable-group-name-71aa"
	canaryGroupNote     = "canary-variable-group-description-c4e2"
	canaryOversized     = "CANARY-OVERSIZED-VALUE-9b05"
	canaryPinnedGroup   = "canary-pinned-variable-group-5a1e"
	canaryPinned        = "CANARY-PINNED-OVERRIDE-VALUE-e2d7"
)

// Through every way the poll answers (the hand-off, a push, a reconcile, a
// failure and the health gate, an unassigned collector reporting its own
// file, each refusal, a store failure), no YAML, attribute value, error
// text, name, label, variable value or bearer reaches a log line or a span
// (its name, attributes, status or events); the ids and outcomes do, so the
// capture isn't empty. On Postgres too, so the stores' spans are searched.
// The variables are covered too, all through the variables store: a
// variable's value, name and description, an override's value, a variable
// group's name and description, a pinned variable group's name and
// override, a text over the cap (its error row) and a failed read of the
// variables.
func TestThePollLogsAndTracesNoConfigurationTextNameOrBearer(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous) })
	tracer := otel.Tracer("handlers-privacy-test")

	storetest.Each(t, testDatabase, func(t *testing.T, s *storetest.Stores) {
		sp := &spy{}
		x := newHarness(t, s, withSpy(sp))
		if _, _, err := s.Variables.CreateVariable(t.Context(), variables.NewVariable{ID: uuid.New(), OrganizationID: storetest.OrgA,
			Name: canaryVariableName, Value: canaryVariable, Description: storetest.Ptr(canaryDescription), Actor: storetest.Ana,
			At: x.clock.Now()}); err != nil {
			t.Fatal(err)
		}
		region := x.variable(storetest.OrgA, "REGION", "eu-west")
		variableGroup := uuid.New()
		if _, _, err := s.Variables.CreateGroup(t.Context(), variables.NewGroup{ID: variableGroup, OrganizationID: storetest.OrgA,
			Name: canaryVariableGroup, Description: storetest.Ptr(canaryGroupNote), Actor: storetest.Ana, At: x.clock.Now()}); err != nil {
			t.Fatal(err)
		}
		x.override(storetest.OrgA, variableGroup, region, canaryOverride)
		x.variable(storetest.OrgA, "OVERSIZED", canaryOversized+strings.Repeat("x", 1024-len(canaryOversized)))
		c := x.fleet(storetest.OrgA, canaryLabel, fleets.TypeServer)
		group, err := s.Groups.Create(t.Context(), collectorgroups.NewGroup{OrganizationID: storetest.OrgA, FleetID: c.Fleet.ID,
			Name: canaryGroup, At: x.clock.Now()})
		if err != nil {
			t.Fatal(err)
		}
		yaml := "# " + canaryYAML + "\nexporters:\n  otlp:\n    headers: {tenant: ${org:" + canaryVariableName + "}, region: ${org:REGION}}\n"
		x.assign(c, group.ID, x.publish(storetest.OrgA, yaml))
		x.bind(c, group.ID, &variableGroup)
		local := x.fleet(storetest.OrgA, "edge-local", fleets.TypeServer)
		// A fleet whose text, filled in, is over the cap: 1,200 references to
		// a 1,024-character value.
		oversized := x.fleet(storetest.OrgA, "edge-oversized", fleets.TypeServer)
		x.assign(oversized, oversized.DefaultGroupID, x.publish(storetest.OrgA, strings.Repeat("- ${org:OVERSIZED}\n", 1200)))

		var bearers []string
		send := func(p *middleware.Principal, bearer string, msg *protobufs.AgentToServer, serverName string) *protobufs.ServerToAgent {
			body, err := proto.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, middleware.AgentRoute, bytes.NewReader(body))
			req.Header.Set("ServerName", serverName)
			var handler http.Handler = x.h
			if bearer != "" {
				req.Header.Set("Authorization", "Bearer "+bearer)
				handler = x.auth.Middleware()(x.h)
			} else if p != nil {
				req = req.WithContext(middleware.WithPrincipal(req.Context(), *p))
			}
			ctx, span := tracer.Start(req.Context(), "POST "+middleware.AgentRoute, trace.WithSpanKind(trace.SpanKindServer))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req.WithContext(ctx))
			span.End()
			for _, leak := range []string{canaryYAML, canaryAttribute, canaryError, canaryLabel, canaryDBDetail, canaryVariable,
				canaryVariableName, canaryOverride, canaryOversized, canaryPinned} {
				if rec.Code != http.StatusOK && strings.Contains(rec.Body.String(), leak) {
					t.Errorf("a %d answer holds %q", rec.Code, leak)
				}
			}
			answer := decode(t, rec)
			if auth := settingsHeader(answer, "Authorization"); auth != "" {
				bearers = append(bearers, strings.TrimPrefix(auth, "Bearer "))
			}
			return answer
		}
		desc := withDescription(map[string]string{"service.version": "0.162.0"},
			map[string]string{"canary.attr": canaryAttribute, "telflo.collector_group_id": group.ID.String()})

		// The hand-off through the authentication, then the pushed bearer.
		defaultBearer, err := x.keyring.Mint(tokens.Claims{TokenID: c.DefaultTokenID, FleetID: c.Fleet.ID})
		if err != nil {
			t.Fatal(err)
		}
		bearers = append(bearers, defaultBearer)
		uid := uuid.New()
		send(nil, defaultBearer, message(uid, 0, withSettings(), desc), canaryName)
		pushedAnswer := send(nil, bearers[len(bearers)-1], message(uid, 1, withSettings(), desc), canaryName)
		if push := pushed(t, pushedAnswer); !strings.Contains(push, canaryVariable) || !strings.Contains(push, canaryOverride) {
			t.Fatalf("the capture's push isn't filled in with the value and the override")
		}
		// A failure, then the health gate, then a stuck re-delivery.
		p := rotate(c)
		send(&p, "", message(uid, 2, withStatus(pushed(t, pushedAnswer), protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, canaryError)), canaryName)
		send(&p, "", message(uid, 3, withStatus(pushed(t, pushedAnswer), protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, ""),
			withHealth(false, canaryHealth)), canaryName)
		x.clock.Advance(StuckThreshold + time.Second)
		send(&p, "", message(uid, 4), canaryName)
		// A variable group pinned on the collector, its override filled in.
		pinnedGroup := uuid.New()
		if _, _, err := s.Variables.CreateGroup(t.Context(), variables.NewGroup{ID: pinnedGroup, OrganizationID: storetest.OrgA,
			Name: canaryPinnedGroup, Actor: storetest.Ana, At: x.clock.Now()}); err != nil {
			t.Fatal(err)
		}
		x.override(storetest.OrgA, pinnedGroup, region, canaryPinned)
		x.pinVariableGroup(c, x.collector(c, uid).ID, &pinnedGroup)
		if push := pushed(t, send(&p, "", message(uid, 5), canaryName)); !strings.Contains(push, canaryPinned) {
			t.Fatalf("the pinned variable group's override wasn't filled in")
		}
		// An unassigned collector reporting its own file, the canaries in
		// it: asked for its full state, then acknowledged, sent nothing.
		loc := rotate(local)
		other := uuid.New()
		own := "# " + canaryYAML + "\n" + localYAML
		send(&loc, "", message(other, 0, withEffective(own, ""), withDescription(map[string]string{"service.version": "0.162.0"},
			map[string]string{"canary.attr": canaryAttribute, "telflo.collector_group_id": local.DefaultGroupID.String()})), canaryName+"-2")
		send(&loc, "", message(other, 1, withEffective(own, "text/yaml")), canaryName+"-2")
		// Refusals: a foreign group, no group, another fleet's collector,
		// the cap, a store failure carrying a detail, a lost credential.
		send(&p, "", message(uuid.New(), 1, withDescription(nil, map[string]string{"telflo.collector_group_id": "x-" + canaryAttribute})), canaryName)
		send(&p, "", message(uuid.New(), 1, withDescription(nil, map[string]string{"canary.attr": canaryAttribute})), canaryName)
		send(&loc, "", message(uid, 9, desc), canaryName)
		if _, err := s.Organizations.SetLimits(t.Context(), storetest.OrgA, organizations.Limits{MaxCollectors: storetest.Ptr(int32(0))}); err != nil {
			t.Fatal(err)
		}
		send(&p, "", message(uuid.New(), 1, desc), canaryName)
		if _, err := s.Organizations.SetLimits(t.Context(), storetest.OrgA, organizations.Limits{MaxCollectors: storetest.Ptr(int32(100))}); err != nil {
			t.Fatal(err)
		}
		sp.failPoll = fmt.Errorf("collectors: poll: %w", &pgconn.PgError{Code: "57014", Message: canaryDBDetail, Detail: canaryYAML})
		send(&p, "", message(uid, 10), canaryName)
		sp.failPoll = nil
		// The cap's error row, twice (once per hash), then a failed read of
		// the variables: a change moves the counter, so the poll misses.
		big := rotate(oversized)
		huge := uuid.New()
		send(&big, "", message(huge, 0, withGroup(oversized.DefaultGroupID)), canaryName+"-3")
		send(&big, "", message(huge, 1), canaryName+"-3")
		if rows := x.errors(oversized, x.collector(oversized, huge).ID); len(rows) != 1 {
			t.Fatalf("the oversized text: %d error rows; want 1", len(rows))
		}
		x.override(storetest.OrgA, variableGroup, region, canaryOverride+"-2")
		_, _, _, _, _, reads := sp.counts()
		sp.failVariables = fmt.Errorf("variables: resolve: %w", &pgconn.PgError{Code: "57014", Message: canaryDBDetail, Detail: canaryOverride})
		send(&p, "", message(uid, 11), canaryName)
		sp.failVariables = nil
		if _, _, _, _, _, after := sp.counts(); after != reads+1 {
			t.Fatalf("the failing read wasn't reached")
		}
		if _, err := s.Fleets.Delete(t.Context(), storetest.OrgA, local.Fleet.ID); err != nil {
			t.Fatal(err)
		}
		send(&loc, "", message(uuid.New(), 1, withGroup(local.DefaultGroupID)), canaryName)

		logs := x.logs.String()
		for _, outcome := range []string{outcomeForeignGroup, outcomeNoGroup, outcomeFleetMismatch, outcomeCollectorLimit, outcomeStoreFailed, "credential_lost"} {
			if !strings.Contains(logs, outcome) {
				t.Fatalf("the logs hold no %s refusal: the capture is broken:\n%s", outcome, logs)
			}
		}
		var spans strings.Builder
		for _, span := range recorder.Ended() {
			fmt.Fprintf(&spans, "%s %v %v %v\n", span.Name(), span.Attributes(), span.Status(), span.Events())
		}
		for _, outcome := range []string{outcomeHandOff, outcomePushed, outcomeRedelivered, outcomeFullState, outcomeRenderedTooLarge} {
			if !strings.Contains(spans.String(), outcome) {
				t.Fatalf("no span holds %s: the capture is broken:\n%s", outcome, spans.String())
			}
		}
		if len(bearers) < 2 {
			t.Fatalf("no bearer was pushed")
		}
		forbidden := append([]string{canaryYAML, canaryAttribute, canaryError, canaryHealth, canaryName, canaryLabel,
			canaryGroup, canaryVariable, canaryDBDetail, canaryVariableName, canaryDescription, canaryOverride,
			canaryVariableGroup, canaryGroupNote, canaryOversized, canaryPinnedGroup, canaryPinned, keySetting, "Bearer", "otelcol.yml", "exporters"}, bearers...)
		for _, written := range []struct{ where, text string }{{"the logs", logs}, {"the spans", spans.String()}} {
			for _, secret := range forbidden {
				if strings.Contains(written.text, secret) {
					t.Errorf("%s hold %q", written.where, secret)
				}
			}
		}
	})
}
