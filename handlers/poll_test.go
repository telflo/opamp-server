package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"

	"github.com/telflo/telflo-opamp-server/middleware"
	"github.com/telflo/telflo-opamp-server/store/collectorgroups"
	"github.com/telflo/telflo-opamp-server/store/collectors"
	"github.com/telflo/telflo-opamp-server/store/fleets"
	"github.com/telflo/telflo-opamp-server/store/organizations"
	"github.com/telflo/telflo-opamp-server/store/orgvariables"
	"github.com/telflo/telflo-opamp-server/store/storetest"
)

const gatewayYAML = "receivers:\n  otlp:\n    protocols:\n      grpc:\nservice:\n  pipelines:\n    traces:\n      receivers: [otlp]\n      exporters: [debug]\n"

// assigned is a fleet whose Default group is assigned a published version
// of yaml.
func assigned(x *harness, yaml string) (fleets.Creation, uuid.UUID) {
	c := x.fleet(storetest.OrgA, "edge-prod", fleets.TypeServer)
	pub := x.publish(storetest.OrgA, yaml)
	x.assign(c, c.DefaultGroupID, pub)
	return c, pub
}

// A request the server can't read is 400 with the fixed body, one log line
// each with its outcome; without the Principal, 401.
func TestThePollRefusesWhatItCantRead(t *testing.T) {
	x := newHarness(t, testStores(t))
	c := x.fleet(storetest.OrgA, "edge-prod", fleets.TypeServer)
	p := rotate(c)
	asText := []byte(uuid.New().String())
	cases := []struct {
		name    string
		p       *middleware.Principal
		method  string
		body    []byte
		status  int
		outcome string
	}{
		{"a GET", &p, http.MethodGet, nil, http.StatusBadRequest, outcomeMethod},
		{"no Principal", nil, http.MethodPost, mustMarshal(t, message(uuid.New(), 1)), http.StatusUnauthorized, outcomeNoPrincipal},
		{"not a protobuf", &p, http.MethodPost, []byte("\xff\xff\xff not an AgentToServer"), http.StatusBadRequest, outcomeInvalidMessage},
		{"no instance uid", &p, http.MethodPost, mustMarshal(t, &protobufs.AgentToServer{SequenceNum: 1}), http.StatusBadRequest, outcomeInvalidUID},
		{"a uid as 36 ASCII bytes", &p, http.MethodPost, mustMarshal(t, &protobufs.AgentToServer{InstanceUid: asText}), http.StatusBadRequest, outcomeInvalidUID},
		{"a uid of 15 bytes", &p, http.MethodPost, mustMarshal(t, &protobufs.AgentToServer{InstanceUid: make([]byte, 15)}), http.StatusBadRequest, outcomeInvalidUID},
		{"a body over the cap", &p, http.MethodPost, bytes.Repeat([]byte{0}, middleware.MaxBodyBytes+1), http.StatusRequestEntityTooLarge, outcomeTooLarge},
		{"a body of millions of empty fields", &p, http.MethodPost, emptyAttributes((middleware.MaxBodyBytes - 64) / 2), http.StatusRequestEntityTooLarge, outcomeOverBudget},
	}
	for _, tc := range cases {
		before := len(x.logLines())
		rec := x.send(tc.p, tc.method, tc.body, nil)
		want := map[int]string{http.StatusBadRequest: "Bad Request\n", http.StatusUnauthorized: "Unauthorized\n",
			http.StatusRequestEntityTooLarge: "Request Entity Too Large\n"}[tc.status]
		if rec.Code != tc.status || rec.Body.String() != want {
			t.Errorf("%s: %d %q; want %d %q", tc.name, rec.Code, rec.Body.String(), tc.status, want)
		}
		lines := x.logLines()
		if len(lines) != before+1 || lines[len(lines)-1]["outcome"] != tc.outcome {
			t.Errorf("%s: want one log line with outcome %s; got %v", tc.name, tc.outcome, lines[before:])
		}
	}
	if page, _ := x.s.Collectors.List(t.Context(), storetest.OrgA, c.Fleet.ID, collectors.ListOptions{
		Sort: collectors.SortCreated, Order: collectors.Ascending, Limit: 10}); len(page.Collectors) != 0 {
		t.Fatalf("a refused poll enrolled %d collectors", len(page.Collectors))
	}
}

func mustMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The instance uid is answered as the OpAMP specification's raw 16 bytes,
// echoed. Every successful answer is no-store (it may carry a bearer).
func TestTheAnswersInstanceUidIsTheRawSixteenBytes(t *testing.T) {
	x := newHarness(t, testStores(t))
	c := x.fleet(storetest.OrgA, "edge-prod", fleets.TypeServer)
	uid := uuid.New()
	for _, p := range []middleware.Principal{defaultToken(c), rotate(c)} {
		rec, answer := x.poll(p, message(uid, 1, withSettings()))
		if rec.Code != http.StatusOK || !bytes.Equal(answer.GetInstanceUid(), uid[:]) {
			t.Fatalf("%s: %d, InstanceUid %x; want 200 and %x", p.TokenKind, rec.Code, answer.GetInstanceUid(), uid[:])
		}
		if rec.Header().Get("Content-Type") != "application/x-protobuf" || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: headers %v", p.TokenKind, rec.Header())
		}
	}
}

// The per-collector guard, keyed by the organization and the uid the body
// names, refuses past its burst with 429 and Retry-After 60, before any
// store is read.
func TestTheCollectorGuardRefusesPastItsBurst(t *testing.T) {
	sp := &spy{}
	x := newHarness(t, testStores(t), withSpy(sp))
	c := x.fleet(storetest.OrgA, "edge-prod", fleets.TypeServer)
	uid := uuid.New()
	for i := range middleware.CollectorBurst {
		if rec, _ := x.poll(rotate(c), message(uid, uint64(i))); rec.Code != http.StatusOK {
			t.Fatalf("poll %d inside the burst: %d", i, rec.Code)
		}
	}
	polls, _, _, _, _, _ := sp.counts()
	rec, _ := x.poll(rotate(c), message(uid, 99))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != strconv.Itoa(middleware.CollectorRetryAfter) {
		t.Fatalf("past the burst: %d Retry-After %q; want 429 and %d", rec.Code, rec.Header().Get("Retry-After"), middleware.CollectorRetryAfter)
	}
	if after, _, _, _, _, _ := sp.counts(); after != polls {
		t.Fatalf("a guarded poll read the store")
	}
	// Another collector of the organization isn't held back.
	if rec, _ := x.poll(rotate(c), message(uuid.New(), 1)); rec.Code != http.StatusOK {
		t.Fatalf("another collector: %d", rec.Code)
	}
}

// A rotate token's collector is served its group's assigned version as one
// otelcol.yml, its hash the SHA-256 of its bytes, recorded as served; its
// APPLIED report of that hash reads in sync; the next poll pushes nothing.
func TestAGroupsAssignedVersionIsPushedThenAppliedReadsInSync(t *testing.T) {
	x := newHarness(t, testStores(t))
	c, pub := assigned(x, gatewayYAML)
	uid := uuid.New()

	answer := x.ok(rotate(c), message(uid, 1))
	if got := pushed(t, answer); got != gatewayYAML {
		t.Fatalf("pushed %q; want the assigned version's YAML", got)
	}
	l := x.collector(c, uid)
	if l.Served == nil || *l.Served != (collectors.Ref{Source: collectors.SourcePublication, ID: pub}) ||
		l.ServedSHA256 == nil || *l.ServedSHA256 != sha(gatewayYAML) || l.ServedAt == nil || l.GroupID != c.DefaultGroupID || l.Pinned != nil {
		// A delivery never pins: the group's assignment stays the source.
		t.Fatalf("served not recorded, or pinned: %+v", l.Collector)
	}
	if l.Sync() != collectors.Pending {
		t.Fatalf("sync before a report: %s; want pending", l.Sync())
	}

	x.clock.Advance(5 * time.Second)
	answer = x.ok(rotate(c), message(uid, 2, withStatus(gatewayYAML, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, "")))
	if answer.GetRemoteConfig() != nil {
		t.Fatalf("the served version was pushed again")
	}
	l = x.collector(c, uid)
	if l.Sync() != collectors.InSync || *l.EffectiveSHA256 != sha(gatewayYAML) {
		t.Fatalf("after APPLIED: sync %s; want in_sync (%+v)", l.Sync(), l.Collector)
	}
	if again := x.ok(rotate(c), message(uid, 3)); again.GetRemoteConfig() != nil || fullState(again) {
		t.Fatalf("a steady poll was pushed or asked: %v", again)
	}
}

// A pinned published version wins over the group's assignment; unpinning
// lets the group's apply again.
func TestAPinWinsOverTheGroupsAssignment(t *testing.T) {
	x := newHarness(t, testStores(t))
	c, _ := assigned(x, gatewayYAML)
	uid := uuid.New()
	x.ok(rotate(c), message(uid, 1))
	id := x.collector(c, uid).ID

	canary := "receivers:\n  otlp:\n# the canary version\n"
	pinned := x.publish(storetest.OrgA, canary)
	x.pin(c, id, &collectors.Ref{Source: collectors.SourcePublication, ID: pinned})
	if got := pushed(t, x.ok(rotate(c), message(uid, 2))); got != canary {
		t.Fatalf("pinned: pushed %q; want the pinned version", got)
	}
	x.pin(c, id, nil)
	if got := pushed(t, x.ok(rotate(c), message(uid, 3))); got != gatewayYAML {
		t.Fatalf("unpinned: pushed %q; want the group's version again", got)
	}
}

// The bytes-match reconcile: a collector already running what resolves (its
// row deleted and re-enrolled) isn't pushed; the served pointer moves,
// served_at at or before its report, and it reads in sync.
func TestBytesAlreadyRunningAreReconciledNotPushed(t *testing.T) {
	x := newHarness(t, testStores(t))
	c, pub := assigned(x, gatewayYAML)
	uid := uuid.New()
	answer := x.ok(rotate(c), message(uid, 1, withStatus(gatewayYAML, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, "")))
	if answer.GetRemoteConfig() != nil {
		t.Fatalf("bytes already running were pushed")
	}
	l := x.collector(c, uid)
	if l.Served == nil || l.Served.ID != pub || l.ServedAt == nil || l.EffectiveAt == nil || l.ServedAt.After(*l.EffectiveAt) {
		t.Fatalf("not reconciled: %+v", l.Collector)
	}
	if l.Sync() != collectors.InSync {
		t.Fatalf("sync %s; want in_sync", l.Sync())
	}
}

// An identical retarget: retargeted to another published version of
// identical bytes, the served pointer follows without a push and served_at
// stays. Until that poll it reads pending (the served published version
// isn't the resolved one).
func TestAnIdenticalRetargetMovesTheServedPointerOnly(t *testing.T) {
	x := newHarness(t, testStores(t))
	c, _ := assigned(x, gatewayYAML)
	uid := uuid.New()
	x.ok(rotate(c), message(uid, 1))
	servedAt := *x.collector(c, uid).ServedAt

	x.clock.Advance(time.Minute)
	copyOf := x.publish(storetest.OrgA, gatewayYAML)
	x.assign(c, c.DefaultGroupID, copyOf)
	if d := x.collector(c, uid).Sync(); d != collectors.Pending {
		t.Fatalf("retargeted, before its poll: sync %s; want pending", d)
	}
	answer := x.ok(rotate(c), message(uid, 2, withStatus(gatewayYAML, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, "")))
	if answer.GetRemoteConfig() != nil {
		t.Fatalf("identical bytes were pushed again")
	}
	l := x.collector(c, uid)
	if l.Served == nil || l.Served.ID != copyOf || !l.ServedAt.Equal(servedAt) || l.Sync() != collectors.InSync {
		t.Fatalf("after the retarget: %+v sync %s", l.Collector, l.Sync())
	}
}

// A FAILED report records the effective status and an error row with its
// message and hash; one without a message records the fixed text.
func TestAFailedReportRecordsAnErrorRow(t *testing.T) {
	x := newHarness(t, testStores(t))
	c, _ := assigned(x, gatewayYAML)
	uid := uuid.New()
	x.ok(rotate(c), message(uid, 1))
	x.ok(rotate(c), message(uid, 2, withStatus(gatewayYAML, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, "port 4317 already in use")))
	l := x.collector(c, uid)
	if *l.EffectiveStatus != collectors.StatusFailed || l.EffectiveError == nil || *l.EffectiveError != "port 4317 already in use" || l.Sync() != collectors.Failed {
		t.Fatalf("after FAILED: %+v sync %s", l.Collector, l.Sync())
	}
	rows := x.errors(c, l.ID)
	if len(rows) != 1 || rows[0].Message != "port 4317 already in use" || rows[0].ConfigSHA256 == nil || *rows[0].ConfigSHA256 != sha(gatewayYAML) {
		t.Fatalf("error rows: %+v", rows)
	}
	x.clock.Advance(time.Second)
	x.ok(rotate(c), message(uid, 3, withStatus(gatewayYAML, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, "")))
	if rows := x.errors(c, l.ID); len(rows) != 2 || rows[0].Message != emptyFailureMessage {
		t.Fatalf("an empty message's row: %+v", rows)
	}
}

// The health gate: APPLIED with an unhealthy Health carrying a last_error
// is failed with that error; healthy, an unhealthy one without an error
// (the supervisor before it starts the collector), or no Health at all, is
// applied.
func TestTheHealthGateOnAnAppliedReport(t *testing.T) {
	crash := "Agent process PID=4409 exited unexpectedly, exit code=1.\nError: cannot start pipelines: lookup <VM_IP>: no such host"
	cases := []struct {
		name   string
		health msgOption
		want   collectors.EffectiveStatus
		errs   int
	}{
		{"unhealthy with an error", withHealth(false, crash), collectors.StatusFailed, 1},
		{"healthy", withHealth(true, ""), collectors.StatusApplied, 0},
		{"unhealthy before the collector starts", withHealth(false, ""), collectors.StatusApplied, 0},
		{"no health", func(*protobufs.AgentToServer) {}, collectors.StatusApplied, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x := newHarness(t, testStores(t))
			c, _ := assigned(x, gatewayYAML)
			uid := uuid.New()
			x.ok(rotate(c), message(uid, 1))
			x.ok(rotate(c), message(uid, 2, withStatus(gatewayYAML, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, ""), tc.health))
			l := x.collector(c, uid)
			if *l.EffectiveStatus != tc.want {
				t.Fatalf("status %s; want %s", *l.EffectiveStatus, tc.want)
			}
			rows := x.errors(c, l.ID)
			if len(rows) != tc.errs || (tc.errs == 1 && (rows[0].Message != crash || *l.EffectiveError != crash)) {
				t.Fatalf("error rows %+v, error %v", rows, l.EffectiveError)
			}
		})
	}
}

// mapRemoteConfigStatus: the three reported statuses; UNSET and unknown
// values record nothing.
func TestMapRemoteConfigStatusMapsTheThreeReportedStatusesAndIgnoresTheRest(t *testing.T) {
	cases := []struct {
		in   protobufs.RemoteConfigStatuses
		want collectors.EffectiveStatus
		ok   bool
	}{
		{protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, collectors.StatusApplied, true},
		{protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLYING, collectors.StatusApplying, true},
		{protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, collectors.StatusFailed, true},
		{protobufs.RemoteConfigStatuses_RemoteConfigStatuses_UNSET, "", false},
		{protobufs.RemoteConfigStatuses(99), "", false},
		{protobufs.RemoteConfigStatuses(-1), "", false},
	}
	for _, tc := range cases {
		if got, ok := mapRemoteConfigStatus(tc.in); got != tc.want || ok != tc.ok {
			t.Errorf("mapRemoteConfigStatus(%v) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	// UNSET records nothing on the collector.
	x := newHarness(t, testStores(t))
	c, _ := assigned(x, gatewayYAML)
	uid := uuid.New()
	x.ok(rotate(c), message(uid, 1))
	x.ok(rotate(c), message(uid, 2, withStatus(gatewayYAML, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_UNSET, "")))
	if l := x.collector(c, uid); l.EffectiveStatus != nil {
		t.Fatalf("UNSET recorded %s", *l.EffectiveStatus)
	}
}

// failedFor is a fleet whose collector was served gatewayYAML and reported
// it FAILED, then the clock moved age on.
func failedFor(t *testing.T, x *harness, status protobufs.RemoteConfigStatuses, age time.Duration) (fleets.Creation, uuid.UUID) {
	t.Helper()
	c, _ := assigned(x, gatewayYAML)
	uid := uuid.New()
	x.ok(rotate(c), message(uid, 1))
	x.ok(rotate(c), message(uid, 2, withStatus(gatewayYAML, status, "exporter failed")))
	x.clock.Advance(age)
	return c, uid
}

// A re-delivery that really re-delivers: a collector failed past
// StuckThreshold is pushed the same bytes again, recorded as a delivery at
// now, and asked for its full state; at most once every FullStateInterval,
// on any replica.
func TestAStuckFailedCollectorIsRedeliveredTheSameBytesAtMostOnceEveryThreeMinutes(t *testing.T) {
	x := newHarness(t, testStores(t))
	c, uid := failedFor(t, x, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, StuckThreshold+time.Second)
	firstServed := *x.collector(c, uid).ServedAt

	answer := x.ok(rotate(c), message(uid, 10))
	if got := pushed(t, answer); got != gatewayYAML || !fullState(answer) {
		t.Fatalf("stuck failed: pushed %q, full state %v; want the same bytes and the ask", got, fullState(answer))
	}
	l := x.collector(c, uid)
	if !l.ServedAt.After(firstServed) || !l.ServedAt.Equal(storetest.Millisecond(x.clock.Now())) || l.FullStateRequestedAt == nil {
		t.Fatalf("the re-delivery wasn't recorded: %+v", l.Collector)
	}
	// Another replica, a minute later, the collector still failed and quiet:
	// neither asked nor re-delivered.
	x.clock.Advance(time.Minute)
	if again := x.replica().ok(rotate(c), message(uid, 11)); again.GetRemoteConfig() != nil || fullState(again) {
		t.Fatalf("asked again within %s", FullStateInterval)
	}
	x.clock.Advance(FullStateInterval)
	if again := x.ok(rotate(c), message(uid, 12)); again.GetRemoteConfig() == nil || !fullState(again) {
		t.Fatalf("not asked again after %s", FullStateInterval)
	}
}

// A collector stuck applying is asked for its full state, not re-delivered;
// one failed within the threshold is neither.
func TestStuckApplyingIsAskedAndARecentFailureIsLeftAlone(t *testing.T) {
	x := newHarness(t, testStores(t))
	c, uid := failedFor(t, x, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLYING, StuckThreshold+time.Second)
	if answer := x.ok(rotate(c), message(uid, 10)); answer.GetRemoteConfig() != nil || !fullState(answer) {
		t.Fatalf("stuck applying: push %v, full state %v; want the ask only", answer.GetRemoteConfig() != nil, fullState(answer))
	}

	y := newHarness(t, testStores(t))
	c, uid = failedFor(t, y, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, 10*time.Second)
	if answer := y.ok(rotate(c), message(uid, 10)); answer.GetRemoteConfig() != nil || fullState(answer) {
		t.Fatalf("a recent failure was re-delivered or asked")
	}
}

// Lost-state recovery: a collector that has polled before (sequence past 1)
// with no report is asked for its full state; a brand-new one (sequence 1)
// isn't, nor one whose report is known.
func TestAMissingReportIsAskedForOnlyFromACollectorThatPolledBefore(t *testing.T) {
	x := newHarness(t, testStores(t))
	c, _ := assigned(x, gatewayYAML)
	uid := uuid.New()
	if answer := x.ok(rotate(c), message(uid, 1)); fullState(answer) {
		t.Fatalf("a new collector was asked for its full state")
	}
	if answer := x.ok(rotate(c), message(uid, 47)); !fullState(answer) {
		t.Fatalf("a collector without a report wasn't asked")
	}
	x.clock.Advance(FullStateInterval)
	x.ok(rotate(c), message(uid, 48, withStatus(gatewayYAML, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, "")))
	x.clock.Advance(FullStateInterval)
	if answer := x.ok(rotate(c), message(uid, 99)); fullState(answer) {
		t.Fatalf("a collector whose report is known was asked")
	}
}

// localYAML is a collector's own file, as its supervisor reports it: what
// an unassigned collector runs, and is never sent back.
const localYAML = "receivers:\n  hostmetrics:\nservice:\n  pipelines:\n    metrics:\n      receivers: [hostmetrics]\n"

// No hot loop: an unassigned collector on a rotate token is sent no
// configuration, and is asked for its full state at most once every
// FullStateInterval, across replicas, from its first poll, never on every
// poll.
func TestAnUnassignedCollectorIsAskedForItsFullStateAtMostOnceEveryThreeMinutes(t *testing.T) {
	x := newHarness(t, testStores(t))
	c := x.fleet(storetest.OrgA, "edge-prod", fleets.TypeServer)
	uid := uuid.New()
	other := x.replica()
	asked := 0
	for i := range 20 {
		h := x
		if i%2 == 1 {
			h = other
		}
		answer := h.ok(rotate(c), message(uid, uint64(i), withEffective(localYAML, "")))
		if answer.GetRemoteConfig() != nil {
			t.Fatalf("poll %d was sent a configuration", i)
		}
		if fullState(answer) {
			asked++
		}
		x.clock.Advance(15 * time.Second)
	}
	// 20 polls over 5 minutes: asked at 0 and at 3 minutes.
	if asked != 2 {
		t.Fatalf("asked %d times in 5 minutes; want 2", asked)
	}
}

// A server fleet's and a kubernetes-supervisor fleet's unassigned
// collectors are polled alike, each reporting its own file and naming its
// fleet's Default group: on a first connection (sequences 0 to 9) asked for
// their full state at 0 only, the ask recorded; on a reconnection through
// another replica (its sequence restarting at 0) not asked again within
// FullStateInterval; asked again after it. Never sent a configuration;
// unassigned, nothing pinned or served.
func TestUnassignedCollectorsOfEitherFleetTypeArePolledAlike(t *testing.T) {
	x := newHarness(t, testStores(t))
	for _, c := range []fleets.Creation{
		x.fleet(storetest.OrgA, "edge-prod", fleets.TypeServer),
		x.fleet(storetest.OrgA, "edge-k8s", fleets.TypeKubernetesSupervisor),
	} {
		uid := uuid.New()
		connect := func(label string, through *harness, askedFirst bool) {
			t.Helper()
			for seq := range uint64(10) {
				answer := through.ok(rotate(c), message(uid, seq, withEffective(localYAML, ""), withGroup(c.DefaultGroupID)))
				if answer.GetRemoteConfig() != nil {
					t.Fatalf("%s, %s: sequence %d was sent a configuration", c.Fleet.Type, label, seq)
				}
				if want := askedFirst && seq == 0; fullState(answer) != want {
					t.Fatalf("%s, %s: sequence %d asked %v; want %v", c.Fleet.Type, label, seq, fullState(answer), want)
				}
				x.clock.Advance(10 * time.Millisecond)
			}
		}
		asked := storetest.Millisecond(x.clock.Now())
		connect("the first connection", x, true)
		l := x.collector(c, uid)
		if l.FullStateRequestedAt == nil || !l.FullStateRequestedAt.Equal(asked) {
			t.Fatalf("%s: the ask at %s wasn't recorded: %v", c.Fleet.Type, asked, l.FullStateRequestedAt)
		}
		if l.Sync() != collectors.Unassigned || l.Pinned != nil || l.Served != nil || l.ServedSHA256 != nil {
			t.Fatalf("%s: %+v; want unassigned, nothing pinned or served", c.Fleet.Type, l.Collector)
		}
		connect("a reconnection, through another replica", x.replica(), false)
		x.clock.Advance(FullStateInterval)
		if answer := x.ok(rotate(c), message(uid, 0)); !fullState(answer) || answer.GetRemoteConfig() != nil {
			t.Fatalf("%s: %s after the last ask, not asked, or sent a configuration", c.Fleet.Type, FullStateInterval)
		}
	}
}

// A collector served its group's published version, once the group's
// assignment is cleared, is unassigned and keeps running what it was sent:
// it is sent no configuration (the last served bytes are never pushed
// again), asked for its full state only at most once every
// FullStateInterval, and keeps its served pointer, SHA-256 and time as the
// store reports them.
func TestACollectorWhoseGroupsAssignmentIsClearedKeepsWhatItWasServed(t *testing.T) {
	storetest.Each(t, testDatabase, func(t *testing.T, s *storetest.Stores) {
		x := newHarness(t, s)
		c, pub := assigned(x, gatewayYAML)
		uid := uuid.New()
		if got := pushed(t, x.ok(rotate(c), message(uid, 0))); got != gatewayYAML {
			t.Fatalf("pushed %q; want the group's version", got)
		}
		x.clock.Advance(5 * time.Second)
		x.ok(rotate(c), message(uid, 1, withStatus(gatewayYAML, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, "")))
		before := x.collector(c, uid)
		if before.Sync() != collectors.InSync || before.ServedAt == nil {
			t.Fatalf("before the clear: %+v sync %s; want in_sync", before.Collector, before.Sync())
		}

		if _, err := s.Groups.Update(t.Context(), c.Fleet.OrganizationID, c.Fleet.ID, c.DefaultGroupID,
			collectorgroups.Changes{AssignedPublicationID: collectorgroups.Clear[uuid.UUID]()}, x.clock.Now()); err != nil {
			t.Fatalf("clearing the assignment: %v", err)
		}

		var asked []int
		for i := range 20 {
			x.clock.Advance(15 * time.Second)
			answer := x.ok(rotate(c), message(uid, uint64(i+2), withStatus(gatewayYAML, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, "")))
			if answer.GetRemoteConfig() != nil {
				t.Fatalf("poll %d after the clear was sent a configuration", i)
			}
			if fullState(answer) {
				asked = append(asked, i)
			}
		}
		// 20 polls 15 seconds apart: asked at the first and 3 minutes later.
		if fmt.Sprint(asked) != "[0 12]" {
			t.Fatalf("asked on polls %v after the clear; want [0 12]", asked)
		}

		l := x.collector(c, uid)
		want := collectors.Ref{Source: collectors.SourcePublication, ID: pub}
		if l.Resolution.From != collectors.FromNone || l.Sync() != collectors.Unassigned || l.Pinned != nil {
			t.Fatalf("after the clear: resolution %+v sync %s pinned %v; want unassigned", l.Resolution, l.Sync(), l.Pinned)
		}
		if l.Served == nil || *l.Served != want || l.ServedSHA256 == nil || *l.ServedSHA256 != sha(gatewayYAML) ||
			l.ServedAt == nil || !l.ServedAt.Equal(*before.ServedAt) {
			t.Fatalf("the served pointer moved: %+v; want %v, %s at %s", l.Collector, want, sha(gatewayYAML), before.ServedAt)
		}
	})
}

// An empty YAML is never pushed, nor recorded served.
func TestAnEmptyConfigurationIsNeverPushed(t *testing.T) {
	x := newHarness(t, testStores(t))
	c, _ := assigned(x, "")
	uid := uuid.New()
	for seq := range uint64(3) {
		if answer := x.ok(rotate(c), message(uid, seq)); answer.GetRemoteConfig() != nil {
			t.Fatalf("an empty configuration was pushed")
		}
	}
	if l := x.collector(c, uid); l.ServedSHA256 != nil {
		t.Fatalf("an empty configuration was recorded served")
	}
}

// A steady poll is one store call, Poll; the YAML and the variables come
// from the caches, and nothing is written. With variables, the first poll
// reads them once and fills the cache (the warming poll); after it, 5 polls
// are 5 Poll calls, 0 variable reads, 0 YAML reads and 0 reports.
func TestASteadyPollIsOneStoreCall(t *testing.T) {
	for _, withVariables := range []bool{false, true} {
		t.Run(map[bool]string{false: "without variables", true: "with variables"}[withVariables], func(t *testing.T) {
			sp := &spy{}
			x := newHarness(t, testStores(t), withSpy(sp))
			yaml := gatewayYAML
			if withVariables {
				x.variable(storetest.OrgA, "OTLP_EP", "us:4317")
				x.variable(storetest.OrgA, "UNUSED", "1")
				yaml += "exporters:\n  otlp:\n    endpoint: ${org:OTLP_EP}\n"
			}
			c, _ := assigned(x, yaml)
			uid := uuid.New()
			running := pushed(t, x.ok(rotate(c), message(uid, 1, withDescription(map[string]string{"service.version": "0.162.0"}, nil))))
			x.ok(rotate(c), message(uid, 2, withStatus(running, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, "")))
			polls, yamls, reports, _, _, reads := sp.counts()
			if wantReads := map[bool]int{false: 0, true: 1}[withVariables]; reads != wantReads {
				t.Fatalf("the first two polls read the variables %d times; want %d", reads, wantReads)
			}
			for seq := uint64(3); seq < 8; seq++ {
				x.clock.Advance(30 * time.Second)
				x.ok(rotate(c), message(uid, seq, withoutDescription()))
			}
			p2, y2, r2, _, _, v2 := sp.counts()
			if p2-polls != 5 || y2 != yamls || r2 != reports || v2 != reads {
				t.Fatalf("5 steady polls: %d polls, %d YAML reads, %d reports, %d variable reads; want 5, 0, 0, 0",
					p2-polls, y2-yamls, r2-reports, v2-reads)
			}
			if d := x.collector(c, uid).Sync(); d != collectors.InSync {
				t.Fatalf("sync %s; want in_sync", d)
			}
		})
	}
}

// The YAML cache is read by organization; another organization's poll for
// the same SHA-256 reads its own from the store.
func TestTheYAMLCacheIsReadPerOrganization(t *testing.T) {
	sp := &spy{}
	x := newHarness(t, testStores(t), withSpy(sp))
	a, _ := assigned(x, gatewayYAML)
	b := x.fleet(storetest.OrgB, "edge-prod", fleets.TypeServer)
	x.assign(b, b.DefaultGroupID, x.publish(storetest.OrgB, gatewayYAML))
	x.ok(rotate(a), message(uuid.New(), 1))
	x.ok(rotate(b), message(uuid.New(), 1))
	_, yamls, _, _, _, _ := sp.counts()
	if yamls != 2 || sp.yamlOrgs[0] != storetest.OrgA || sp.yamlOrgs[1] != storetest.OrgB {
		t.Fatalf("YAML reads %d by %v; want one per organization", yamls, sp.yamlOrgs)
	}
}

// A database failure is 503 with a Retry-After of 10 to 30 s (never 400),
// one log line with its SQLSTATE and no error text; a failing write sends
// no push.
func TestAStoreFailureAnswers503(t *testing.T) {
	deadlock := fmt.Errorf("collectors: poll: %w", &pgconn.PgError{Code: "40P01", Message: "deadlock detected SECRET-DETAIL"})
	for _, which := range []string{"poll", "report", "yaml"} {
		t.Run(which, func(t *testing.T) {
			sp := &spy{}
			x := newHarness(t, testStores(t), withSpy(sp))
			c, _ := assigned(x, gatewayYAML)
			switch which {
			case "poll":
				sp.failPoll = deadlock
			case "report":
				sp.failReport = deadlock
			case "yaml":
				sp.failYAML = deadlock
			}
			rec, _ := x.poll(rotate(c), message(uuid.New(), 1))
			retry, _ := strconv.Atoi(rec.Header().Get("Retry-After"))
			if rec.Code != http.StatusServiceUnavailable || retry < 10 || retry > 30 || rec.Body.String() != "Service Unavailable\n" {
				t.Fatalf("%d %q Retry-After %q; want 503 within 10-30 s", rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"))
			}
			lines := x.logLines()
			last := lines[len(lines)-1]
			if last["sqlstate"] != "40P01" || last["outcome"] != outcomeStoreFailed || strings.Contains(x.logs.String(), "SECRET-DETAIL") {
				t.Fatalf("log: %v", last)
			}
		})
	}
}

// A poll whose client went away mid-request (the supervisor cancels its
// in-flight default-token poll when it applies the hand-off), so that its
// store call failed on the cancelled context, is one Info line, outcome
// request_cancelled, for either token kind; never an ERROR store_failed. A
// store call that ran out of the database deadline is still one.
func TestAPollWhoseClientWentAwayIsNoErrorLine(t *testing.T) {
	for _, kind := range []string{"default", "rotate"} {
		t.Run(kind, func(t *testing.T) {
			sp := &spy{failPoll: fmt.Errorf("collectors: poll: %w", context.Canceled)}
			x := newHarness(t, testStores(t), withSpy(sp))
			c, _ := assigned(x, gatewayYAML)
			p := rotate(c)
			if kind == "default" {
				p = defaultToken(c)
			}
			body, err := proto.Marshal(message(uuid.New(), 0))
			if err != nil {
				t.Fatal(err)
			}
			send := func(ctx context.Context) *httptest.ResponseRecorder {
				req := httptest.NewRequestWithContext(middleware.WithPrincipal(ctx, p), http.MethodPost, middleware.AgentRoute,
					bytes.NewReader(body))
				rec := httptest.NewRecorder()
				x.h.ServeHTTP(rec, req)
				return rec
			}
			gone, cancel := context.WithCancel(context.Background())
			cancel()
			if rec := send(gone); rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("its client gone: %d", rec.Code)
			}
			lines := x.logLines()
			if len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["outcome"] != middleware.OutcomeCancelled ||
				lines[0]["organization_id"] != c.Fleet.OrganizationID.String() {
				t.Fatalf("its client gone, the lines: %v", lines)
			}

			x.logs.Reset()
			sp.failPoll = fmt.Errorf("collectors: poll: %w", context.DeadlineExceeded)
			late, stop := context.WithTimeout(context.Background(), 0)
			defer stop()
			<-late.Done()
			if rec := send(late); rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("past the database deadline: %d", rec.Code)
			}
			if lines := x.logLines(); len(lines) != 1 || lines[0]["level"] != "ERROR" || lines[0]["outcome"] != outcomeStoreFailed {
				t.Fatalf("past the database deadline, the lines: %v", lines)
			}
		})
	}
}

// On either fleet type and either token, a collector the organization
// doesn't hold whose description names no group (telflo.collector_group_id
// absent or blank) is refused 403 with the fixed body, outcome no_group,
// and nothing enrolled; never put in the Default group. Its log line is a
// 403's, sampled (one line for the organization's eight refusals within the
// minute), with ids only. A message without a description is still asked
// for its full state first, and a collector already enrolled isn't checked
// again.
func TestACollectorNamingNoGroupIsRefused(t *testing.T) {
	x := newHarness(t, testStores(t))
	server := x.fleet(storetest.OrgA, "edge-prod", fleets.TypeServer)
	for _, c := range []fleets.Creation{server, x.fleet(storetest.OrgA, "edge-k8s", fleets.TypeKubernetesSupervisor)} {
		for _, p := range []middleware.Principal{defaultToken(c), rotate(c)} {
			uid := uuid.New()
			for _, m := range []struct {
				name string
				msg  *protobufs.AgentToServer
			}{
				{"an empty description", message(uid, 0, withSettings())},
				{"a blank group", message(uid, 1, withDescription(map[string]string{"service.version": "0.162.0"},
					map[string]string{attributeGroupID: " \t "}))},
			} {
				rec, _ := x.pollAsIs(p, m.msg, "edge-groupless")
				if rec.Code != http.StatusForbidden || rec.Body.String() != "Forbidden\n" {
					t.Fatalf("%s, %s, %s: %d %q; want 403 Forbidden", c.Fleet.Type, p.TokenKind, m.name, rec.Code, rec.Body.String())
				}
				if _, found := x.findCollector(c, uid); found {
					t.Fatalf("%s, %s, %s: the collector was enrolled", c.Fleet.Type, p.TokenKind, m.name)
				}
			}
			// Without a description: asked for its full state, nothing enrolled.
			rec, answer := x.pollAsIs(p, message(uid, 2, withoutDescription()))
			if rec.Code != http.StatusOK || !fullState(answer) {
				t.Fatalf("%s, %s: without a description %d; want 200 asking for the full state", c.Fleet.Type, p.TokenKind, rec.Code)
			}
			if _, found := x.findCollector(c, uid); found {
				t.Fatalf("%s, %s: a poll without a description enrolled the collector", c.Fleet.Type, p.TokenKind)
			}
		}
		// Enrolled naming its group, then a description naming none: not
		// checked again.
		uid := uuid.New()
		x.ok(rotate(c), message(uid, 0, withGroup(c.DefaultGroupID)))
		if rec, _ := x.pollAsIs(rotate(c), message(uid, 1)); rec.Code != http.StatusOK {
			t.Fatalf("%s: an enrolled collector's description without a group: %d; want 200", c.Fleet.Type, rec.Code)
		}
	}
	var refusals []map[string]any
	for _, line := range x.logLines() {
		if line["outcome"] == outcomeNoGroup {
			refusals = append(refusals, line)
		}
	}
	if len(refusals) != 1 || refusals[0]["status"] != float64(http.StatusForbidden) ||
		refusals[0]["fleet_id"] != server.Fleet.ID.String() || refusals[0]["token_id"] != server.DefaultTokenID.String() ||
		refusals[0]["organization_id"] != storetest.OrgA.String() {
		t.Fatalf("the no_group lines are %v; want one, the first refusal's, with its ids", refusals)
	}
	if strings.Contains(x.logs.String(), "edge-groupless") {
		t.Fatalf("a refusal logged the collector's name: %s", x.logs.String())
	}
}

// A group's delete deletes its collectors; one still running polls on: its
// next poll finds no record and, without a description, is asked for its
// full state (nothing enrolled); the full state, naming the deleted group,
// is refused 403 foreign_group, and it stays unenrolled.
func TestADeletedGroupsCollectorIsRefusedAtItsFullState(t *testing.T) {
	x := newHarness(t, testStores(t))
	c := x.fleet(storetest.OrgA, "edge-prod", fleets.TypeServer)
	g, err := x.s.Groups.Create(t.Context(), collectorgroups.NewGroup{OrganizationID: storetest.OrgA, FleetID: c.Fleet.ID,
		Name: "doomed", At: x.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	uid := uuid.New()
	x.ok(rotate(c), message(uid, 0, withGroup(g.ID)))
	if l := x.collector(c, uid); l.GroupID != g.ID {
		t.Fatalf("enrolled in %s; want the group %s", l.GroupID, g.ID)
	}
	if deleted, err := x.s.Groups.Delete(t.Context(), storetest.OrgA, c.Fleet.ID, g.ID); err != nil || !deleted {
		t.Fatalf("the group's delete = %v, %v", deleted, err)
	}
	if _, found := x.findCollector(c, uid); found {
		t.Fatal("the group's collector survived its delete")
	}
	rec, answer := x.pollAsIs(rotate(c), message(uid, 1, withoutDescription()))
	if rec.Code != http.StatusOK || !fullState(answer) {
		t.Fatalf("the next poll: %d; want 200 asking for the full state", rec.Code)
	}
	rec, _ = x.pollAsIs(rotate(c), message(uid, 2, withGroup(g.ID)))
	if rec.Code != http.StatusForbidden || rec.Body.String() != "Forbidden\n" {
		t.Fatalf("the full state naming the deleted group: %d %q; want 403 Forbidden", rec.Code, rec.Body.String())
	}
	if lines := x.logLines(); lines[len(lines)-1]["outcome"] != outcomeForeignGroup {
		t.Fatalf("the refusal's line: %v", lines[len(lines)-1])
	}
	if _, found := x.findCollector(c, uid); found {
		t.Fatal("the deleted group's collector enrolled again")
	}
}

// What Poll resolved, deleted before its YAML is read, is a 503 retry; the
// next poll resolves again.
func TestAConfigurationGoneBetweenPollAndYAMLIsARetry(t *testing.T) {
	sp := &spy{failYAML: collectors.ErrNotFound}
	x := newHarness(t, testStores(t), withSpy(sp))
	c, _ := assigned(x, gatewayYAML)
	rec, _ := x.poll(rotate(c), message(uuid.New(), 1))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("%d; want 503 with Retry-After", rec.Code)
	}
	if lines := x.logLines(); lines[len(lines)-1]["outcome"] != outcomeConfigurationGone {
		t.Fatalf("outcome %v", lines[len(lines)-1])
	}
}

// A fleet deleted mid-poll (its tokens with it) is a credential lost: 401,
// as the Authenticator answers it.
func TestAFleetDeletedMidPollAnswers401(t *testing.T) {
	x := newHarness(t, testStores(t))
	c := x.fleet(storetest.OrgA, "edge-prod", fleets.TypeServer)
	p := rotate(c)
	if deleted, err := x.s.Fleets.Delete(t.Context(), storetest.OrgA, c.Fleet.ID); err != nil || !deleted {
		t.Fatal(err)
	}
	rec, _ := x.poll(p, message(uuid.New(), 1, withGroup(c.DefaultGroupID)))
	if rec.Code != http.StatusUnauthorized || rec.Body.String() != "Unauthorized\n" {
		t.Fatalf("%d %q; want 401", rec.Code, rec.Body.String())
	}
}

// The reported fields (collector version, client address, host.ip,
// attributes) are written when changed, in the one Report; an unchanged
// poll writes nothing.
func TestReportedFieldsAreWrittenOnlyWhenChanged(t *testing.T) {
	sp := &spy{}
	x := newHarness(t, testStores(t), withSpy(sp))
	c := x.fleet(storetest.OrgA, "edge-prod", fleets.TypeServer)
	uid := uuid.New()
	desc := withDescription(map[string]string{"service.version": "0.162.0", "service.name": "otelcol-contrib"},
		map[string]string{"host.ip": "10.1.2.3", "os.type": "linux"})
	x.ok(rotate(c), message(uid, 1, desc))
	l := x.collector(c, uid)
	if *l.ReportedCollectorVersion != "0.162.0" || *l.ReportedHostIP != "10.1.2.3" || *l.ReportedIP != clientIP ||
		l.ReportedAttributes["os.type"] != "linux" || l.ReportedAttributes["service.name"] != "otelcol-contrib" {
		t.Fatalf("reported: %+v", l.Collector)
	}
	// Past the unassigned ask, written once.
	x.clock.Advance(FullStateInterval)
	x.ok(rotate(c), message(uid, 2, withoutDescription()))
	_, _, reports, _, _, _ := sp.counts()
	x.ok(rotate(c), message(uid, 3, withoutDescription()))
	if _, _, after, _, _, _ := sp.counts(); after != reports {
		t.Fatalf("an unchanged poll called Report")
	}
	// A message without a description leaves the reported fields as they are.
	if l2 := x.collector(c, uid); *l2.ReportedCollectorVersion != "0.162.0" || l2.ReportedAttributes["os.type"] != "linux" {
		t.Fatalf("a poll without a description cleared fields: %+v", l2.Collector)
	}
}

// host.ip: a string, or an array's first non-empty string; absent, nothing.
func TestHostIPIsAStringOrAnArraysFirstString(t *testing.T) {
	array := &protobufs.AgentDescription{NonIdentifyingAttributes: []*protobufs.KeyValue{{Key: "host.ip", Value: &protobufs.AnyValue{
		Value: &protobufs.AnyValue_ArrayValue{ArrayValue: &protobufs.ArrayValue{Values: []*protobufs.AnyValue{
			{Value: &protobufs.AnyValue_StringValue{StringValue: " "}},
			{Value: &protobufs.AnyValue_StringValue{StringValue: "10.0.0.9"}},
		}}}}}}}
	str := &protobufs.AgentDescription{NonIdentifyingAttributes: kvs(map[string]string{"host.ip": " 10.0.0.8 "})}
	for _, tc := range []struct {
		desc *protobufs.AgentDescription
		want string
	}{{array, "10.0.0.9"}, {str, "10.0.0.8"}, {&protobufs.AgentDescription{}, ""}} {
		if got := hostIP(tc.desc); got != tc.want {
			t.Errorf("hostIP = %q; want %q", got, tc.want)
		}
	}
}

// The attributes stored are bounded by the store's limit, by name order.
func TestReportedAttributesAreBoundedForTheStore(t *testing.T) {
	big := map[string]string{}
	for i := range 400 {
		big[fmt.Sprintf("attr.%03d", i)] = strings.Repeat("v", 100)
	}
	big["bad"] = "nul\x00"
	got := boundedAttributes(big)
	if !collectors.ValidAttributes(got) || len(got) == 0 || len(got) >= 400 {
		t.Fatalf("bounded to %d attributes (valid %v)", len(got), collectors.ValidAttributes(got))
	}
	if _, ok := got["attr.000"]; !ok {
		t.Fatalf("the first names in order weren't kept")
	}
	if _, ok := got["bad"]; ok {
		t.Fatalf("a NUL value was kept")
	}
}

// An agent that doesn't advertise AcceptsRemoteConfig (0x2) is pushed no
// configuration and nothing is recorded served; once it does, it is.
func TestNoConfigurationIsPushedToAnAgentThatDoesntAcceptOne(t *testing.T) {
	x := newHarness(t, testStores(t))
	c, _ := assigned(x, gatewayYAML)
	uid := uuid.New()
	refusing := func(m *protobufs.AgentToServer) {
		m.Capabilities &^= uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig)
	}
	for seq := range uint64(3) {
		if answer := x.ok(rotate(c), message(uid, seq, refusing)); answer.GetRemoteConfig() != nil {
			t.Fatalf("poll %d pushed to an agent that accepts no configuration", seq)
		}
	}
	if l := x.collector(c, uid); l.Served != nil || l.ServedSHA256 != nil || l.ServedAt != nil {
		t.Fatalf("recorded served: %+v", l.Collector)
	}
	if got := pushed(t, x.ok(rotate(c), message(uid, 3))); got != gatewayYAML {
		t.Fatalf("once it accepts one: pushed %q", got)
	}
}

// The organization variables in the poll.

// applied is the option of a message reporting the text applied.
func applied(text string) msgOption {
	return withStatus(text, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, "")
}

// A value is written literally, each `$` doubled, so the collector's
// confmap pass, which collapses `$$` to `$`, reads the value exactly:
// `${env:SECRET}` in a value stays that text, never a provider; a reference
// escaped as `$${org:NAME}` stays as written. The served hash is the
// filled-in bytes'.
func TestAValueReachesTheCollectorWrittenLiterally(t *testing.T) {
	x := newHarness(t, testStores(t))
	x.variable(storetest.OrgA, "OTLP_EP", "us:4317/$path")
	x.variable(storetest.OrgA, "TENANT", "${env:SECRET}")
	template := "exporters:\n  otlp:\n    endpoint: ${org:OTLP_EP}\n    headers: {tenant: \"${org:TENANT}\"}\n    escaped: $${org:TENANT}\n"
	c, _ := assigned(x, template)
	uid := uuid.New()
	got := pushed(t, x.ok(rotate(c), message(uid, 1)))
	want := "exporters:\n  otlp:\n    endpoint: us:4317/$$path\n    headers: {tenant: \"$${env:SECRET}\"}\n    escaped: $${org:TENANT}\n"
	if got != want {
		t.Fatalf("pushed %q; want %q", got, want)
	}
	read := strings.ReplaceAll(got, "$$", "$")
	if !strings.Contains(read, "endpoint: us:4317/$path\n") || !strings.Contains(read, "tenant: \"${env:SECRET}\"") {
		t.Fatalf("after confmap's pass the collector reads %q; want each value exactly", read)
	}
	if l := x.collector(c, uid); *l.ServedSHA256 != sha(got) {
		t.Fatalf("the served hash isn't the filled-in bytes'")
	}
}

// Three levels end to end: each variable comes from the pin's variable
// group, else the collector group's, else the organization; clearing a
// level lets the next one show through.
func TestEachVariableComesFromThePinsGroupThenTheGroupsThenTheOrganization(t *testing.T) {
	storetest.Each(t, testDatabase, func(t *testing.T, s *storetest.Stores) {
		x := newHarness(t, s)
		x.variable(storetest.OrgA, "A", "org-a")
		b := x.variable(storetest.OrgA, "B", "org-b")
		cv := x.variable(storetest.OrgA, "C", "org-c")
		group := x.variableGroup(storetest.OrgA, "eu")
		x.override(storetest.OrgA, group, b, "group-b")
		x.override(storetest.OrgA, group, cv, "group-c")
		pin := x.variableGroup(storetest.OrgA, "canary")
		x.override(storetest.OrgA, pin, cv, "pin-c")
		c, _ := assigned(x, "a: ${org:A}\nb: ${org:B}\nc: ${org:C}\n")
		x.bind(c, c.DefaultGroupID, &group)
		pinned, follower := uuid.New(), uuid.New()
		seq := uint64(0)
		expect := func(step string, uid uuid.UUID, want string) {
			t.Helper()
			seq++
			if got := pushed(t, x.ok(rotate(c), message(uid, seq))); got != want {
				t.Fatalf("%s: pushed %q; want %q", step, got, want)
			}
		}
		expect("the follower", follower, "a: org-a\nb: group-b\nc: group-c\n")
		expect("before its pin", pinned, "a: org-a\nb: group-b\nc: group-c\n")
		x.pinVariableGroup(c, x.collector(c, pinned).ID, &pin)
		expect("pinned", pinned, "a: org-a\nb: group-b\nc: pin-c\n")
		x.bind(c, c.DefaultGroupID, nil)
		expect("pinned, the group unbound", pinned, "a: org-a\nb: org-b\nc: pin-c\n")
		expect("the follower, the group unbound", follower, "a: org-a\nb: org-b\nc: org-c\n")
		x.bind(c, c.DefaultGroupID, &group)
		x.pinVariableGroup(c, x.collector(c, pinned).ID, nil)
		expect("unpinned", pinned, "a: org-a\nb: group-b\nc: group-c\n")
	})
}

// A change of a value the text uses is pushed on the next poll, once; the
// polls after it push nothing.
func TestAUsedValueChangeIsPushedOnTheNextPollOnly(t *testing.T) {
	x := newHarness(t, testStores(t))
	host := x.variable(storetest.OrgA, "HOST", "a.example")
	c, _ := assigned(x, "endpoint: ${org:HOST}\n")
	uid := uuid.New()
	running := pushed(t, x.ok(rotate(c), message(uid, 1)))
	if running != "endpoint: a.example\n" {
		t.Fatalf("pushed %q", running)
	}
	if again := x.ok(rotate(c), message(uid, 2, applied(running))); again.GetRemoteConfig() != nil {
		t.Fatalf("a steady poll was pushed")
	}
	x.setValue(storetest.OrgA, host, "b.example")
	if d := x.collector(c, uid).Sync(); d != collectors.Pending {
		t.Fatalf("after the change, before a poll: sync %s; want pending", d)
	}
	if got := pushed(t, x.ok(rotate(c), message(uid, 3, applied(running)))); got != "endpoint: b.example\n" {
		t.Fatalf("the next poll pushed %q; want the new value", got)
	}
	// The push records its marker: the new value's stamp.
	resolved, err := x.s.Variables.Variables(t.Context(), storetest.OrgA, nil, nil)
	if l := x.collector(c, uid); err != nil || l.ServedVariablesStamp == nil || *l.ServedVariablesStamp != resolved.Stamps["HOST"] {
		t.Fatalf("the push's marker is %d; want the new value's stamp %d (%v)", stampOf(l.ServedVariablesStamp), resolved.Stamps["HOST"], err)
	}
	running = "endpoint: b.example\n"
	for seq := uint64(4); seq < 7; seq++ {
		if again := x.ok(rotate(c), message(uid, seq, applied(running))); again.GetRemoteConfig() != nil {
			t.Fatalf("poll %d pushed again", seq)
		}
	}
	if d := x.collector(c, uid).Sync(); d != collectors.InSync {
		t.Fatalf("sync %s; want in_sync", d)
	}
}

// A change of a value the text doesn't use pushes nothing and writes
// nothing: the collector stays In sync, before and after its polls, and the
// polls read the variables once (the change moved the counter) and write no
// report.
func TestAnUnusedValueChangePushesNothingAndWritesNothing(t *testing.T) {
	sp := &spy{}
	x := newHarness(t, testStores(t), withSpy(sp))
	x.variable(storetest.OrgA, "HOST", "a.example")
	unused := x.variable(storetest.OrgA, "UNUSED", "1")
	c, _ := assigned(x, "endpoint: ${org:HOST}\n")
	uid := uuid.New()
	running := pushed(t, x.ok(rotate(c), message(uid, 1)))
	x.ok(rotate(c), message(uid, 2, applied(running)))
	if d := x.collector(c, uid).Sync(); d != collectors.InSync {
		t.Fatalf("sync %s; want in_sync", d)
	}
	x.setValue(storetest.OrgA, unused, "2")
	if d := x.collector(c, uid).Sync(); d != collectors.InSync {
		t.Fatalf("after an unused change: sync %s; want in_sync", d)
	}
	_, _, reports, _, _, reads := sp.counts()
	for seq := uint64(3); seq < 6; seq++ {
		x.clock.Advance(30 * time.Second)
		if answer := x.ok(rotate(c), message(uid, seq, withoutDescription())); answer.GetRemoteConfig() != nil {
			t.Fatalf("an unused change was pushed")
		}
	}
	_, _, r2, _, _, v2 := sp.counts()
	if r2 != reports || v2-reads != 1 {
		t.Fatalf("after an unused change: %d reports, %d variable reads; want 0 and 1", r2-reports, v2-reads)
	}
	if d := x.collector(c, uid).Sync(); d != collectors.InSync {
		t.Fatalf("after its polls: sync %s; want in_sync", d)
	}
}

// An override equal to the organization's value it hides changes no byte,
// so nothing is pushed, but its stamp is a change the collector uses: it
// reads pending until its next poll, which moves the marker alone (one
// report), and then In sync.
func TestAnOverrideEqualToTheOrganizationsValueMovesTheMarkerWithoutAPush(t *testing.T) {
	storetest.Each(t, testDatabase, func(t *testing.T, s *storetest.Stores) {
		sp := &spy{}
		x := newHarness(t, s, withSpy(sp))
		host := x.variable(storetest.OrgA, "HOST", "a.example")
		group := x.variableGroup(storetest.OrgA, "eu")
		c, _ := assigned(x, "endpoint: ${org:HOST}\n")
		x.bind(c, c.DefaultGroupID, &group)
		uid := uuid.New()
		running := pushed(t, x.ok(rotate(c), message(uid, 1)))
		x.ok(rotate(c), message(uid, 2, applied(running)))
		before := x.collector(c, uid)
		if before.Sync() != collectors.InSync {
			t.Fatalf("sync %s; want in_sync", before.Sync())
		}
		x.override(storetest.OrgA, group, host, "a.example")
		if d := x.collector(c, uid).Sync(); d != collectors.Pending {
			t.Fatalf("after the override, before a poll: sync %s; want pending", d)
		}
		_, _, reports, _, _, _ := sp.counts()
		if answer := x.ok(rotate(c), message(uid, 3, withoutDescription())); answer.GetRemoteConfig() != nil {
			t.Fatalf("an override of the same value was pushed")
		}
		after := x.collector(c, uid)
		if _, _, r2, _, _, _ := sp.counts(); r2-reports != 1 {
			t.Fatalf("%d reports; want the marker's one", r2-reports)
		}
		if after.Sync() != collectors.InSync || after.ServedVariablesStamp == nil ||
			*after.ServedVariablesStamp <= *before.ServedVariablesStamp || !after.ServedAt.Equal(*before.ServedAt) {
			t.Fatalf("after the poll: sync %s, marker %d (was %d), served at %v (was %v); want in_sync, a newer marker, the same served time",
				after.Sync(), stampOf(after.ServedVariablesStamp), stampOf(before.ServedVariablesStamp), after.ServedAt, before.ServedAt)
		}
		if again := x.ok(rotate(c), message(uid, 4, withoutDescription())); again.GetRemoteConfig() != nil {
			t.Fatalf("a steady poll was pushed")
		}
		if _, _, r3, _, _, _ := sp.counts(); r3-reports != 1 {
			t.Fatalf("the marker was written again")
		}
	})
}

// The variables are read once per combination of a collector's two variable
// groups at the organization's revision; the other polls of that
// combination are served from the cache, and after a change each
// combination misses once again.
func TestTheCacheMissesOncePerCombinationAfterAChange(t *testing.T) {
	sp := &spy{}
	x := newHarness(t, testStores(t), withSpy(sp))
	host := x.variable(storetest.OrgA, "HOST", "org")
	eu := x.variableGroup(storetest.OrgA, "eu")
	x.override(storetest.OrgA, eu, host, "eu")
	canary := x.variableGroup(storetest.OrgA, "canary")
	x.override(storetest.OrgA, canary, host, "canary")
	c, pub := assigned(x, "endpoint: ${org:HOST}\n")
	bound, err := x.s.Groups.Create(t.Context(), collectorgroups.NewGroup{OrganizationID: storetest.OrgA, FleetID: c.Fleet.ID,
		Name: "bound", At: x.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	x.assign(c, bound.ID, pub)
	x.bind(c, bound.ID, &eu)

	// Two collectors of each combination: (none, none), (eu, none),
	// (eu, canary) and (none, canary).
	type member struct {
		uid    uuid.UUID
		group  uuid.UUID
		pinned bool
		want   string
	}
	var members []member
	for _, m := range []member{
		{group: c.DefaultGroupID, want: "org"}, {group: bound.ID, want: "eu"},
		{group: bound.ID, pinned: true, want: "canary"}, {group: c.DefaultGroupID, pinned: true, want: "canary"},
	} {
		for range 2 {
			m.uid = uuid.New()
			members = append(members, m)
		}
	}
	seq := uint64(0)
	pollAll := func() {
		t.Helper()
		seq++
		for _, m := range members {
			x.ok(rotate(c), message(m.uid, seq, withGroup(m.group)))
		}
	}
	pollAll() // enrolls them all
	for _, m := range members {
		if m.pinned {
			x.pinVariableGroup(c, x.collector(c, m.uid).ID, &canary)
		}
	}
	for round, change := range []func(){
		func() {},
		func() { x.setValue(storetest.OrgA, host, "org-2") },
	} {
		change()
		_, _, _, _, _, reads := sp.counts()
		pollAll()
		_, _, _, _, _, warm := sp.counts()
		pollAll()
		_, _, _, _, _, steady := sp.counts()
		if warm-reads != 4 || steady != warm {
			t.Fatalf("round %d: %d variable reads, then %d; want one per combination (4), then 0", round, warm-reads, steady-warm)
		}
	}
	for _, m := range members {
		want := m.want
		if want == "org" {
			want = "org-2"
		}
		if l := x.collector(c, m.uid); *l.ServedSHA256 != sha("endpoint: "+want+"\n") {
			t.Fatalf("a collector of (%v, pinned %v) wasn't served %q", m.group, m.pinned, want)
		}
	}
}

// An organization that never had a variable (its counter at 0) never reads
// them, while another organization's variables are read for its own polls
// only.
func TestAnOrganizationThatNeverHadVariablesNeverReadsThem(t *testing.T) {
	sp := &spy{}
	x := newHarness(t, testStores(t), withSpy(sp))
	x.variable(storetest.OrgB, "HOST", "b.example")
	c, _ := assigned(x, gatewayYAML)
	uid := uuid.New()
	for seq := range uint64(4) {
		x.clock.Advance(30 * time.Second)
		x.ok(rotate(c), message(uid, seq))
	}
	if _, _, _, _, _, reads := sp.counts(); reads != 0 || x.vars.Len() != 0 {
		t.Fatalf("%d variable reads, %d cached; want none", reads, x.vars.Len())
	}
}

// A configuration whose filled-in text is over 1 MiB is never built and
// never pushed. The collector keeps what it runs, reads pending, and gets
// one error row with the exact message and the streamed hash of the
// filled-in text; a second poll adds no row. The capped poll allocates
// under 16 MiB, though the text would be 32 MiB.
func TestARenderOverOneMiBIsNotPushedAndRecordsOneErrorRow(t *testing.T) {
	sp := &spy{}
	x := newHarness(t, testStores(t), withSpy(sp))
	big := x.variable(storetest.OrgA, "V", "small")
	const references = 16384
	text := strings.Repeat("${org:V}\n", references)
	c, pub := assigned(x, text)
	uid := uuid.New()
	running := pushed(t, x.ok(rotate(c), message(uid, 1)))
	x.ok(rotate(c), message(uid, 2, applied(running)))
	servedBefore := x.collector(c, uid)

	value := strings.Repeat("$", orgvariables.MaxValueLength)
	x.setValue(storetest.OrgA, big, value)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	rec, answer := x.poll(rotate(c), message(uid, 3, applied(running)))
	runtime.ReadMemStats(&after)
	if rec.Code != http.StatusOK || answer.GetRemoteConfig() != nil {
		t.Fatalf("over the cap: %d, pushed %v; want 200 and no push", rec.Code, answer.GetRemoteConfig() != nil)
	}
	if grown := after.TotalAlloc - before.TotalAlloc; grown >= 16<<20 {
		t.Fatalf("the capped poll allocated %d bytes; want under 16 MiB (the text was built?)", grown)
	}
	independent := sha256.New()
	line := []byte(strings.Repeat("$$", orgvariables.MaxValueLength) + "\n")
	for range references {
		independent.Write(line)
	}
	streamed := hex.EncodeToString(independent.Sum(nil))
	l := x.collector(c, uid)
	rows := x.errors(c, l.ID)
	if len(rows) != 1 || rows[0].Message != "The configuration with its organization variables filled in is over 1 MiB." ||
		rows[0].ConfigSHA256 == nil || *rows[0].ConfigSHA256 != streamed {
		t.Fatalf("error rows %+v; want one with the exact message and the streamed hash %s", rows, streamed)
	}
	if l.Sync() != collectors.Pending || l.Served == nil || l.Served.ID != pub || *l.ServedSHA256 != *servedBefore.ServedSHA256 ||
		!l.ServedAt.Equal(*servedBefore.ServedAt) || *l.ServedVariablesStamp != *servedBefore.ServedVariablesStamp {
		t.Fatalf("after the capped poll: sync %s, %+v; want pending, served as before", l.Sync(), l.Collector)
	}
	_, _, reports, _, errs, _ := sp.counts()
	if answer := x.ok(rotate(c), message(uid, 4, applied(running))); answer.GetRemoteConfig() != nil {
		t.Fatalf("the second capped poll pushed")
	}
	if rows := x.errors(c, l.ID); len(rows) != 1 {
		t.Fatalf("a second poll of the same text: %d error rows; want 1", len(rows))
	}
	if _, _, r2, _, e2, _ := sp.counts(); r2-reports != 1 || e2-errs != 1 {
		// Its report (APPLIED again) is written; RecordError is called and
		// writes nothing.
		t.Fatalf("the second capped poll: %d reports, %d RecordErrors; want 1 and 1", r2-reports, e2-errs)
	}
	// Back under the cap, it is pushed again.
	x.setValue(storetest.OrgA, big, "back")
	if got := pushed(t, x.ok(rotate(c), message(uid, 5, applied(running)))); got != strings.Repeat("back\n", references) {
		t.Fatalf("back under the cap: pushed %d bytes", len(got))
	}
}

// Each pod keeps its own variables cache: a replica misses once after a
// change, as the first pod does, and is served the new values.
func TestAReplicaHasItsOwnVariablesCache(t *testing.T) {
	first, second := &spy{}, &spy{}
	x := newHarness(t, testStores(t), withSpy(first))
	y := x.replica(withSpy(second))
	host := x.variable(storetest.OrgA, "HOST", "a.example")
	c, _ := assigned(x, "endpoint: ${org:HOST}\n")
	one, two := uuid.New(), uuid.New()
	reads := func() (int, int) {
		_, _, _, _, _, a := first.counts()
		_, _, _, _, _, b := second.counts()
		return a, b
	}
	for seq := range uint64(3) {
		x.ok(rotate(c), message(one, seq))
		y.ok(rotate(c), message(two, seq))
	}
	if a, b := reads(); a != 1 || b != 1 {
		t.Fatalf("variable reads: %d and %d; want one each", a, b)
	}
	x.setValue(storetest.OrgA, host, "b.example")
	for seq := uint64(3); seq < 6; seq++ {
		x.ok(rotate(c), message(one, seq))
		y.ok(rotate(c), message(two, seq))
	}
	if a, b := reads(); a != 2 || b != 2 {
		t.Fatalf("after a change, variable reads: %d and %d; want one more each", a, b)
	}
	if x.vars == y.vars || y.vars.Len() == 0 {
		t.Fatalf("the replica shares the first pod's cache, or keeps nothing")
	}
	for _, uid := range []uuid.UUID{one, two} {
		if l := x.collector(c, uid); *l.ServedSHA256 != sha("endpoint: b.example\n") {
			t.Fatalf("collector %s wasn't served the new value", uid)
		}
	}
}

// A failed read of the variables is a 503 retry, its log line the SQLSTATE
// and no error text; nothing is pushed, recorded or cached, and the next
// poll reads them again.
func TestAVariablesReadFailureAnswers503(t *testing.T) {
	sp := &spy{}
	x := newHarness(t, testStores(t), withSpy(sp))
	x.variable(storetest.OrgA, "HOST", "a.example")
	c, _ := assigned(x, "endpoint: ${org:HOST}\n")
	uid := uuid.New()
	sp.failVariables = fmt.Errorf("variables: resolve: %w", &pgconn.PgError{Code: "57014", Message: "canceling statement SECRET-DETAIL"})
	rec, _ := x.poll(rotate(c), message(uid, 1))
	retry, _ := strconv.Atoi(rec.Header().Get("Retry-After"))
	if rec.Code != http.StatusServiceUnavailable || retry < 10 || retry > 30 || rec.Body.String() != "Service Unavailable\n" {
		t.Fatalf("%d %q Retry-After %q; want 503 within 10-30 s", rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"))
	}
	lines := x.logLines()
	last := lines[len(lines)-1]
	if last["sqlstate"] != "57014" || last["outcome"] != outcomeStoreFailed || strings.Contains(x.logs.String(), "SECRET-DETAIL") {
		t.Fatalf("log: %v", last)
	}
	if l := x.collector(c, uid); l.ServedSHA256 != nil || x.vars.Len() != 0 {
		t.Fatalf("a failed read recorded %v served, or cached %d", l.ServedSHA256, x.vars.Len())
	}
	sp.failVariables = nil
	if got := pushed(t, x.ok(rotate(c), message(uid, 2))); got != "endpoint: a.example\n" {
		t.Fatalf("the retry pushed %q", got)
	}
}

// The legacy `$orgvar:NAME` is plain text, served as written beside a
// filled-in `${org:NAME}` of the same name.
func TestTheLegacyFormIsServedAsWritten(t *testing.T) {
	x := newHarness(t, testStores(t))
	x.variable(storetest.OrgA, "SAMPLE", "0.5")
	c, _ := assigned(x, "legacy: $orgvar:SAMPLE\nlive: ${org:SAMPLE}\n")
	if got := pushed(t, x.ok(rotate(c), message(uuid.New(), 1))); got != "legacy: $orgvar:SAMPLE\nlive: 0.5\n" {
		t.Fatalf("pushed %q; want the legacy form as written", got)
	}
}

// The poll fills in by the text's own references, never by the names stored
// at publish, so a version published before migration 00012 (no names
// stored) is filled in too, and its marker is the values' stamp.
func TestAVersionWithoutStoredNamesIsFilledInToo(t *testing.T) {
	x := newHarness(t, testStores(t))
	x.variable(storetest.OrgA, "HOST", "a.example")
	c := x.fleet(storetest.OrgA, "edge-prod", fleets.TypeServer)
	pub := x.publish(storetest.OrgA, "endpoint: ${org:HOST}\n")
	x.forgetNames(pub)
	x.assign(c, c.DefaultGroupID, pub)
	uid := uuid.New()
	if got := pushed(t, x.ok(rotate(c), message(uid, 1))); got != "endpoint: a.example\n" {
		t.Fatalf("pushed %q; want the value filled in", got)
	}
	if l := x.collector(c, uid); l.ServedVariablesStamp == nil || *l.ServedVariablesStamp == 0 {
		t.Fatalf("the marker is %d; want the value's stamp", stampOf(l.ServedVariablesStamp))
	}
}

// The served-variables marker through the poll (the store's sync is its
// SQL): the marker the poll writes in Go agrees with the stores' wanted
// stamp at each step. A used value changed, an unused one, a binding to a
// group overriding nothing, an override set and Reset, a pin's variable
// group, an organization value its pin hides, an offline collector, a move
// after an unpolled rebinding (pending, pushed, In sync) and a move into a
// group of the same variable group set since the last poll (pending, then
// one poll moves the marker alone).
func TestSyncThroughThePoll(t *testing.T) {
	storetest.Each(t, testDatabase, func(t *testing.T, s *storetest.Stores) {
		x := newHarness(t, s)
		host := x.variable(storetest.OrgA, "HOST", "a.example")
		unused := x.variable(storetest.OrgA, "UNUSED", "1")
		c, pub := assigned(x, "endpoint: ${org:HOST}\n")
		uid := uuid.New()
		seq := uint64(0)
		running := ""
		check := func(step string, want collectors.Sync) {
			t.Helper()
			if d := x.collector(c, uid).Sync(); d != want {
				t.Fatalf("%s: sync %s; want %s", step, d, want)
			}
		}
		// poll polls as a supervisor does: its status only in the message
		// after it applied a push (report).
		poll := func(report bool) string {
			t.Helper()
			var opts []msgOption
			if seq > 0 {
				opts = append(opts, withoutDescription())
			}
			seq++
			x.clock.Advance(time.Second)
			if report {
				opts = append(opts, applied(running))
			}
			return pushed(t, x.ok(rotate(c), message(uid, seq, opts...)))
		}
		deliver := func(step, want string) {
			t.Helper()
			if seq > 0 {
				check(step+", before its poll", collectors.Pending)
			}
			if got := poll(false); got != "endpoint: "+want+"\n" {
				t.Fatalf("%s: pushed %q; want %s", step, got, want)
			}
			check(step+", pushed", collectors.Pending)
			running = "endpoint: " + want + "\n"
			if again := poll(true); again != "" {
				t.Fatalf("%s: pushed again", step)
			}
			check(step+", applied", collectors.InSync)
		}
		quiet := func(step string, before collectors.Sync) {
			t.Helper()
			check(step+", before its poll", before)
			if got := poll(false); got != "" {
				t.Fatalf("%s: pushed %q", step, got)
			}
			check(step+", polled", collectors.InSync)
		}

		deliver("the first push", "a.example")
		x.setValue(storetest.OrgA, unused, "2")
		quiet("an unused value changed", collectors.InSync)
		x.setValue(storetest.OrgA, host, "b.example")
		deliver("a used value changed", "b.example")
		eu := x.variableGroup(storetest.OrgA, "eu")
		x.bind(c, c.DefaultGroupID, &eu)
		quiet("bound to a group overriding nothing", collectors.Pending)
		x.override(storetest.OrgA, eu, host, "c.example")
		deliver("an override set", "c.example")
		x.override(storetest.OrgA, eu, unused, "3")
		quiet("an unused override set", collectors.InSync)
		x.resetOverride(storetest.OrgA, eu, host)
		deliver("an override Reset", "b.example")
		canary := x.variableGroup(storetest.OrgA, "canary")
		x.override(storetest.OrgA, canary, host, "d.example")
		check("an override in a group nothing uses", collectors.InSync)
		x.pinVariableGroup(c, x.collector(c, uid).ID, &canary)
		deliver("a pin's variable group", "d.example")
		x.setValue(storetest.OrgA, host, "e.example")
		check("an organization value the pin hides", collectors.InSync)
		x.override(storetest.OrgA, canary, host, "f.example")
		x.clock.Advance(time.Hour)
		check("an offline collector after a change it uses", collectors.Pending)
		deliver("back online", "f.example")

		// Moves. Another group, g2, uses the variable
		// group us; a change the collector uses then puts its marker past
		// g2's setting's stamp.
		id := x.collector(c, uid).ID
		x.pinVariableGroup(c, id, nil)
		deliver("the pin's variable group cleared", "e.example")
		us := x.variableGroup(storetest.OrgA, "us")
		x.override(storetest.OrgA, us, host, "us.example")
		g2, err := s.Groups.Create(t.Context(), collectorgroups.NewGroup{OrganizationID: storetest.OrgA, FleetID: c.Fleet.ID,
			Name: "g2", At: x.clock.Now()})
		if err != nil {
			t.Fatal(err)
		}
		x.assign(c, g2.ID, pub)
		x.bind(c, g2.ID, &us)
		check("another group bound", collectors.InSync)
		x.setValue(storetest.OrgA, host, "g.example")
		deliver("a used value changed again", "g.example")
		// Its group rebound to us, unpolled, then a move to g2, also us:
		// the move carries the rebinding's stamp.
		x.bind(c, c.DefaultGroupID, &us)
		if _, err := s.Collectors.Move(t.Context(), storetest.OrgA, c.Fleet.ID, id, g2.ID, x.clock.Now()); err != nil {
			t.Fatal(err)
		}
		deliver("a move after an unpolled rebind", "us.example")
		// The Default group rebound away and back to us after the last
		// poll, then the collector moved back into it: pending until one
		// poll moves the marker without a push.
		x.bind(c, c.DefaultGroupID, &eu)
		x.bind(c, c.DefaultGroupID, &us)
		if _, err := s.Collectors.Move(t.Context(), storetest.OrgA, c.Fleet.ID, id, c.DefaultGroupID, x.clock.Now()); err != nil {
			t.Fatal(err)
		}
		quiet("a move into a group of the same variable group, stamped since", collectors.Pending)
	})
}

// The marker moves only with the bytes it stands for. A collector served
// the resolved version, whose next poll after a change it uses is pushed
// nothing (its agent stopped accepting a configuration), keeps its marker
// and reads pending; served the new bytes, it reads In sync.
func TestTheMarkerMovesOnlyWithTheBytesItWasServed(t *testing.T) {
	storetest.Each(t, testDatabase, func(t *testing.T, s *storetest.Stores) {
		x := newHarness(t, s)
		host := x.variable(storetest.OrgA, "HOST", "a.example")
		c, _ := assigned(x, "endpoint: ${org:HOST}\n")
		uid := uuid.New()
		running := pushed(t, x.ok(rotate(c), message(uid, 1)))
		x.ok(rotate(c), message(uid, 2, applied(running)))
		before := x.collector(c, uid)
		x.setValue(storetest.OrgA, host, "b.example")
		refusing := func(m *protobufs.AgentToServer) {
			m.Capabilities &^= uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig)
		}
		if answer := x.ok(rotate(c), message(uid, 3, refusing, withoutDescription())); answer.GetRemoteConfig() != nil {
			t.Fatalf("pushed to an agent that accepts no configuration")
		}
		l := x.collector(c, uid)
		if l.Sync() != collectors.Pending || *l.ServedVariablesStamp != *before.ServedVariablesStamp {
			t.Fatalf("not served the new bytes: sync %s, marker %d (was %d); want pending, the marker unmoved",
				l.Sync(), *l.ServedVariablesStamp, *before.ServedVariablesStamp)
		}
		running = pushed(t, x.ok(rotate(c), message(uid, 4, withoutDescription())))
		x.ok(rotate(c), message(uid, 5, applied(running), withoutDescription()))
		if d := x.collector(c, uid).Sync(); running != "endpoint: b.example\n" || d != collectors.InSync {
			t.Fatalf("served %q: sync %s; want b.example, in_sync", running, d)
		}
	})
}

// An organization deleted and registered again under its id starts its
// counter at 0 again, but under a new generation, which the variables
// cache's key holds: a pod that cached the previous incarnation's values at
// the same revision never serves them to the new one.
func TestARecreatedOrganizationIsNeverServedThePreviousIncarnationsValues(t *testing.T) {
	storetest.Each(t, testDatabase, func(t *testing.T, s *storetest.Stores) {
		x := newHarness(t, s)
		x.variable(storetest.OrgA, "HOST", "old.example")
		c, _ := assigned(x, "endpoint: ${org:HOST}\n")
		if got := pushed(t, x.ok(rotate(c), message(uuid.New(), 1))); got != "endpoint: old.example\n" {
			t.Fatalf("the first incarnation was pushed %q", got)
		}
		if x.vars.Len() != 1 {
			t.Fatalf("%d entries cached; want the first incarnation's", x.vars.Len())
		}
		if deleted, err := s.Organizations.Delete(t.Context(), storetest.OrgA); err != nil || !deleted {
			t.Fatalf("deleting the organization: %v, %v", deleted, err)
		}
		if result, _, err := s.Organizations.Create(t.Context(), organizations.Organization{ID: storetest.OrgA, Name: "again",
			CreatedAt: x.clock.Now(), UpdatedAt: x.clock.Now()}); err != nil || result != organizations.Created {
			t.Fatalf("registering it again: %v, %v", result, err)
		}
		x.variable(storetest.OrgA, "HOST", "new.example")
		again, _ := assigned(x, "endpoint: ${org:HOST}\n")
		uid := uuid.New()
		for seq := range uint64(2) {
			if got := pushed(t, x.ok(rotate(again), message(uid, seq))); seq == 0 && got != "endpoint: new.example\n" {
				t.Fatalf("the new incarnation was pushed %q; want its own value", got)
			}
		}
		if l := x.collector(again, uid); *l.ServedSHA256 != sha("endpoint: new.example\n") {
			t.Fatalf("the new incarnation's collector wasn't served its own value")
		}
	})
}

// The marker never covers a change the poll statement didn't see. Two
// writes land between the poll statement (which read the collector group's
// binding at revision R) and its cache miss's read (at a later revision):
// the group rebinds from eu to us, then eu overrides PORT with the value it
// hides, leaving eu's bytes unchanged. The poll fills in eu's values and
// finds the bytes it served; its marker is capped at R, so the collector
// reads pending (never In sync on eu's values), and the next poll pushes
// us's.
func TestAMarkerNeverCoversAChangeThePollStatementDidntSee(t *testing.T) {
	storetest.Each(t, testDatabase, func(t *testing.T, s *storetest.Stores) {
		sp := &spy{}
		x := newHarness(t, s, withSpy(sp))
		host := x.variable(storetest.OrgA, "HOST", "org")
		port := x.variable(storetest.OrgA, "PORT", "4317")
		unused := x.variable(storetest.OrgA, "UNUSED", "1")
		eu := x.variableGroup(storetest.OrgA, "eu")
		us := x.variableGroup(storetest.OrgA, "us")
		x.override(storetest.OrgA, eu, host, "eu")
		x.override(storetest.OrgA, us, host, "us")
		c, _ := assigned(x, "endpoint: ${org:HOST}:${org:PORT}\n")
		x.bind(c, c.DefaultGroupID, &eu)
		uid := uuid.New()
		running := pushed(t, x.ok(rotate(c), message(uid, 1)))
		x.clock.Advance(time.Second)
		x.ok(rotate(c), message(uid, 2, applied(running), withoutDescription()))
		if d := x.collector(c, uid).Sync(); d != collectors.InSync || running != "endpoint: eu:4317\n" {
			t.Fatalf("before the window: sync %s, running %q", d, running)
		}
		// The counter moves, so the next poll misses; the two writes land
		// inside it.
		x.setValue(storetest.OrgA, unused, "2")
		sp.mu.Lock()
		sp.beforeVariables = func() {
			x.bind(c, c.DefaultGroupID, &us)
			x.override(storetest.OrgA, eu, port, "4317")
		}
		sp.mu.Unlock()
		x.clock.Advance(time.Second)
		if got := pushed(t, x.ok(rotate(c), message(uid, 3, withoutDescription()))); got != "" {
			t.Fatalf("the window's poll pushed %q; want eu's bytes, unchanged", got)
		}
		if d := x.collector(c, uid).Sync(); d != collectors.Pending {
			t.Fatalf("after the window's poll: sync %s; want pending (it runs eu's values while us is bound)", d)
		}
		x.clock.Advance(time.Second)
		if got := pushed(t, x.ok(rotate(c), message(uid, 4, withoutDescription()))); got != "endpoint: us:4317\n" {
			t.Fatalf("the next poll pushed %q; want us's values", got)
		}
		x.clock.Advance(time.Second)
		x.ok(rotate(c), message(uid, 5, applied("endpoint: us:4317\n"), withoutDescription()))
		if d := x.collector(c, uid).Sync(); d != collectors.InSync {
			t.Fatalf("after us's values applied: sync %s; want in_sync", d)
		}
	})
}

// Through the poll, a variable's delete moves the counter, so a version the
// in-use guard can't see (one without names, published before migration
// 00012) that a poll filled in is, on the next poll, read again and pushed
// with the reference as written.
func TestADeletedVariableIsLeftAsWrittenOnTheNextPoll(t *testing.T) {
	storetest.Each(t, testDatabase, func(t *testing.T, s *storetest.Stores) {
		sp := &spy{}
		x := newHarness(t, s, withSpy(sp))
		host := x.variable(storetest.OrgA, "HOST", "a.example")
		c := x.fleet(storetest.OrgA, "edge-prod", fleets.TypeServer)
		pub := x.publish(storetest.OrgA, "endpoint: ${org:HOST}\n")
		x.forgetNames(pub)
		x.assign(c, c.DefaultGroupID, pub)
		uid := uuid.New()
		if got := pushed(t, x.ok(rotate(c), message(uid, 1))); got != "endpoint: a.example\n" {
			t.Fatalf("pushed %q; want the value filled in", got)
		}
		x.deleteVariable(storetest.OrgA, host)
		_, _, _, _, _, reads := sp.counts()
		if got := pushed(t, x.ok(rotate(c), message(uid, 2, withoutDescription()))); got != "endpoint: ${org:HOST}\n" {
			t.Fatalf("after the delete, pushed %q; want the reference as written", got)
		}
		if _, _, _, _, _, after := sp.counts(); after != reads+1 {
			t.Fatalf("the poll after the delete read the variables %d times; want once (a miss)", after-reads)
		}
	})
}

// Through the poll, a name no variable defines (in a version without names,
// which the undefined check can't see) stays as written beside a filled-in
// one.
func TestAnUndefinedNameIsServedAsWritten(t *testing.T) {
	storetest.Each(t, testDatabase, func(t *testing.T, s *storetest.Stores) {
		x := newHarness(t, s)
		x.variable(storetest.OrgA, "HOST", "a.example")
		c := x.fleet(storetest.OrgA, "edge-prod", fleets.TypeServer)
		pub := x.publish(storetest.OrgA, "endpoint: ${org:HOST}\nregion: ${org:NEVER_DEFINED}\n")
		x.forgetNames(pub)
		x.assign(c, c.DefaultGroupID, pub)
		if got := pushed(t, x.ok(rotate(c), message(uuid.New(), 1))); got != "endpoint: a.example\nregion: ${org:NEVER_DEFINED}\n" {
			t.Fatalf("pushed %q; want the undefined name as written", got)
		}
	})
}

// A reconcile records its marker. A collector re-enrolled while running the
// filled-in bytes, and one retargeted to an identical copy, are each
// reconciled without a push, In sync on that poll with their marker at the
// wanted stamp.
func TestAReconcileRecordsItsMarker(t *testing.T) {
	storetest.Each(t, testDatabase, func(t *testing.T, s *storetest.Stores) {
		x := newHarness(t, s)
		host := x.variable(storetest.OrgA, "HOST", "a.example")
		x.setValue(storetest.OrgA, host, "b.example")
		const text = "endpoint: ${org:HOST}\n"
		const filled = "endpoint: b.example\n"
		c, _ := assigned(x, text)
		expect := func(step string, uid uuid.UUID) {
			t.Helper()
			l := x.collector(c, uid)
			if l.Sync() != collectors.InSync || l.ServedVariablesStamp == nil || l.Resolution.VariablesStamp == 0 ||
				*l.ServedVariablesStamp != l.Resolution.VariablesStamp {
				t.Fatalf("%s: sync %s, marker %d, wanted %d; want in_sync, the marker at the wanted stamp",
					step, l.Sync(), stampOf(l.ServedVariablesStamp), l.Resolution.VariablesStamp)
			}
		}
		// Re-enrolled, already running the filled-in bytes.
		rejoined := uuid.New()
		if answer := x.ok(rotate(c), message(rejoined, 1, applied(filled))); answer.GetRemoteConfig() != nil {
			t.Fatalf("bytes already running were pushed")
		}
		expect("re-enrolled", rejoined)
		// Retargeted to an identical copy.
		x.clock.Advance(time.Minute)
		copyOf := x.publish(storetest.OrgA, text)
		x.assign(c, c.DefaultGroupID, copyOf)
		if answer := x.ok(rotate(c), message(rejoined, 2, applied(filled), withoutDescription())); answer.GetRemoteConfig() != nil {
			t.Fatalf("an identical copy was pushed")
		}
		if l := x.collector(c, rejoined); l.Served == nil || l.Served.ID != copyOf {
			t.Fatalf("the served pointer didn't follow the retarget")
		}
		expect("retargeted", rejoined)
	})
}

// The cap at its boundary: a filled-in text of exactly MaxRenderedBytes is
// pushed; one byte more is not.
func TestTheCapsBoundaryThroughThePoll(t *testing.T) {
	x := newHarness(t, testStores(t))
	value := strings.Repeat("x", orgvariables.MaxValueLength)
	v := x.variable(storetest.OrgA, "V", value)
	head := "v: ${org:V}\n#"
	pad := orgvariables.MaxRenderedBytes - (len(head) - len("${org:V}") + len(value)) - 1
	text := head + strings.Repeat("p", pad) + "\n"
	c, _ := assigned(x, text)
	uid := uuid.New()
	got := pushed(t, x.ok(rotate(c), message(uid, 1)))
	if len(got) != orgvariables.MaxRenderedBytes {
		t.Fatalf("a filled-in text of %d bytes: pushed %d; want it pushed whole", orgvariables.MaxRenderedBytes, len(got))
	}
	// One `$` in the value is written `$$`: one byte more.
	x.setValue(storetest.OrgA, v, "$"+value[1:])
	if answer := x.ok(rotate(c), message(uid, 2, withoutDescription())); answer.GetRemoteConfig() != nil {
		t.Fatalf("a filled-in text of %d bytes was pushed", orgvariables.MaxRenderedBytes+1)
	}
	if rows := x.errors(c, x.collector(c, uid).ID); len(rows) != 1 {
		t.Fatalf("over the cap by one byte: %d error rows; want 1", len(rows))
	}
}
