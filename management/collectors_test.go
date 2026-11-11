package management

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/telflo-opamp-server/store/collectors"
	"github.com/telflo/telflo-opamp-server/store/organizations"
	"github.com/telflo/telflo-opamp-server/store/publications"
)

// collectorRoute is one of the collector routes, named for a test, with a
// body it accepts for the collector it names.
type collectorRoute struct {
	name, method string
	target       func(organizationID, fleetID, collectorID string) string
	body         func(collectorID string) body
}

// collectorRoutes are the six routes that name a fleet (the limits and
// usage routes name none: their own tests cover them).
func collectorRoutes() []collectorRoute {
	none := func(string) body { return nil }
	return []collectorRoute{
		{"list collectors", http.MethodGet, func(o, f, _ string) string { return collectorsTarget(o, f) }, none},
		{"pin collectors", http.MethodPost, func(o, f, _ string) string { return collectorsPinTarget(o, f) },
			func(c string) body { return body{"collectorIds": []string{c}, "pin": nil} }},
		{"get collector", http.MethodGet, collectorTarget, none},
		{"patch collector", http.MethodPatch, collectorTarget,
			func(string) body { return body{"name": "intruder", "pin": nil} }},
		{"delete collector", http.MethodDelete, collectorTarget, none},
		{"list collector errors", http.MethodGet, collectorErrorsTarget, none},
	}
}

// namesCollector reports whether the route names a collector in its path.
func namesCollector(route string) bool {
	switch route {
	case "get collector", "patch collector", "delete collector", "list collector errors":
		return true
	}
	return false
}

// sendRoute sends the route with the ids given.
func sendRoute(s *Server, r collectorRoute, organizationID, fleetID, collectorID string) (int, envelope, string) {
	got := sendWith(s, r.method, r.target(organizationID, fleetID, collectorID), r.body(collectorID))
	var reply envelope
	_ = json.Unmarshal(got.Body.Bytes(), &reply)
	return got.Code, reply, got.Body.String()
}

// fleetWithCollector makes the organization's fleet, a collector in it
// with an error row, and another collector beside it; it answers the
// first's id.
func fleetWithCollector(t *testing.T, s *Server, organizationID, fleetID, label string) (collectorID string) {
	t.Helper()
	createFleet(t, s, organizationID, fleetID, label, "server")
	c := enrollCollector(t, s, organizationID, fleetID, label+"-01")
	seedCollectorError(organizationID, fleetID, label+"-01", "boom")(t, s)
	enrollCollector(t, s, organizationID, fleetID, label+"-02")
	return c.ID.String()
}

// Every collector route, called with another organization's ids (its
// fleet, or this organization's fleet with its collector), answers 404 and
// changes neither organization's rows; a bulk pin of another
// organization's collectors through this organization's fleet pins none of
// them and reports them not found.
func TestAnotherOrganizationsCollectorsAreNotFoundAndUnchanged(t *testing.T) {
	s, _, _ := fleetServer(t)
	theirCollector := fleetWithCollector(t, s, orgA, fleetA, "edge-prod")
	createFleet(t, s, orgB, fleetB, "edge-b", "server")
	beforeA, beforeB := dump(t, s, orgA), dump(t, s, orgB)
	assertAnotherOrganizationsIdsAreNotFound(t, s, theirCollector)
	if after := dump(t, s, orgA); after != beforeA {
		t.Fatalf("B's calls changed A's rows:\n%s\n%s", beforeA, after)
	}
	if after := dump(t, s, orgB); after != beforeB {
		t.Fatalf("the calls changed B's rows:\n%s\n%s", beforeB, after)
	}
}

func assertAnotherOrganizationsIdsAreNotFound(t *testing.T, s *Server, theirCollector string) {
	t.Helper()
	for _, r := range collectorRoutes() {
		status, reply, raw := sendRoute(s, r, orgB, fleetA, theirCollector)
		if status != http.StatusNotFound || reply.Error.Code != string(codeFleetNotFound) {
			t.Errorf("%s with A's fleet under B: %d %s, want 404 fleet_not_found", r.name, status, raw)
		}
		if !namesCollector(r.name) {
			continue
		}
		status, reply, raw = sendRoute(s, r, orgB, fleetB, theirCollector)
		if status != http.StatusNotFound || reply.Error.Code != string(codeCollectorNotFound) {
			t.Errorf("%s with A's collector under B's fleet: %d %s, want 404 collector_not_found", r.name, status, raw)
		}
	}
	got := sendWith(s, http.MethodPost, collectorsPinTarget(orgB, fleetB), body{"collectorIds": []string{theirCollector}, "pin": nil})
	var pinned bulkPinReply
	decodeInto(t, got.Body.Bytes(), &pinned)
	if got.Code != http.StatusOK || len(pinned.Updated) != 0 || !slices.Equal(pinned.NotFound, []string{theirCollector}) {
		t.Errorf("B's bulk pin of A's collector = %d %+v, want it not found", got.Code, pinned)
	}
	var page collectorListReply
	decodeInto(t, call(s, http.MethodGet, collectorsTarget(orgB, fleetB), nil, "").Body.Bytes(), &page)
	if len(page.Collectors) != 0 {
		t.Errorf("B's fleet lists %+v", page.Collectors)
	}
	// B asking what uses A's configuration learns nothing of A's.
	var usage usageReply
	decodeInto(t, call(s, http.MethodGet, usageTarget(orgB, configurationA), nil, "").Body.Bytes(), &usage)
	if usage.InUse || len(usage.Publications) != 0 {
		t.Errorf("B's usage of A's configuration = %+v", usage)
	}
}

// Within one organization, another fleet's collector is the same 404
// through this fleet, on every route that names one, and a bulk pin
// through this fleet reports the other fleet's collector not found; nothing
// changes (the fleet filter, held at the API).
func TestAnotherFleetsCollectorsAreNotFoundThroughThisFleet(t *testing.T) {
	s, _, _ := fleetServer(t)
	theirCollector := fleetWithCollector(t, s, orgA, fleetA, "edge-prod")
	createFleet(t, s, orgA, fleetA2, "edge-dev", "server")
	before := dump(t, s, orgA)
	assertAnotherFleetsIdsAreNotFound(t, s, theirCollector)
	if after := dump(t, s, orgA); after != before {
		t.Fatalf("the calls through another fleet changed the organization's rows:\n%s\n%s", before, after)
	}
}

func assertAnotherFleetsIdsAreNotFound(t *testing.T, s *Server, theirCollector string) {
	t.Helper()
	for _, r := range collectorRoutes() {
		if !namesCollector(r.name) {
			continue
		}
		status, reply, raw := sendRoute(s, r, orgA, fleetA2, theirCollector)
		if status != http.StatusNotFound || reply.Error.Code != string(codeCollectorNotFound) {
			t.Errorf("%s with fleet A's collector through fleet A2: %d %s, want 404 collector_not_found", r.name, status, raw)
		}
	}
	got := sendWith(s, http.MethodPost, collectorsPinTarget(orgA, fleetA2), body{"collectorIds": []string{theirCollector}, "pin": nil})
	var pinned bulkPinReply
	decodeInto(t, got.Body.Bytes(), &pinned)
	if got.Code != http.StatusOK || len(pinned.Updated) != 0 || !slices.Equal(pinned.NotFound, []string{theirCollector}) {
		t.Errorf("a bulk pin of fleet A's collector through fleet A2 = %d %+v", got.Code, pinned)
	}
	if listed := call(s, http.MethodGet, collectorsTarget(orgA, fleetA2), nil, ""); strings.Contains(listed.Body.String(), theirCollector) {
		t.Errorf("fleet A2's list holds fleet A's collector: %s", listed.Body.String())
	}
}

// Every new route answers 404 organization_not_found under an unregistered
// organization, before it reads anything else (a body that isn't JSON
// doesn't change the answer), and stores nothing.
func TestEveryCollectorRouteAnswersOrganizationNotFoundFirst(t *testing.T) {
	s, _, _ := fleetServer(t)
	type route struct {
		name, method, target string
		b                    body
	}
	routes := []route{
		{"get limits", http.MethodGet, limitsTarget(orgC), nil},
		{"put limits", http.MethodPut, limitsTarget(orgC), body{"maxCollectors": 1, "requestsPerMinute": nil}},
		{"usage", http.MethodGet, usageTarget(orgC, configurationA), nil},
	}
	for _, r := range collectorRoutes() {
		routes = append(routes, route{r.name, r.method, r.target(orgC, fleetA, unknownID), r.body(unknownID)})
	}
	for _, r := range routes {
		expectError(t, r.name, sendWith(s, r.method, r.target, r.b), http.StatusNotFound, codeOrganizationNotFound, messageOrganizationNotFound)
		if r.b != nil {
			expectError(t, r.name+" with a bad body", call(s, r.method, r.target, []byte("not json"), jsonType),
				http.StatusNotFound, codeOrganizationNotFound, messageOrganizationNotFound)
		}
	}
	if dumped := dump(t, s, orgC); dumped != "" {
		t.Fatalf("something was stored under an unregistered organization: %s", dumped)
	}
}

// A collectors store that fails answers 500 internal_error on every route
// that reads it (the fleet reads' summaries included), and the log line
// names the route's pattern and the store, nothing else.
func TestAFailingCollectorsStoreAnswers500AndLogsNoDetail(t *testing.T) {
	d := testDB(t)
	registry := organizations.NewPostgres(d.Runtime)
	register(t, registry, orgA, orgB)
	stores := postgresFleetStores(d.Runtime)
	stores.collectors = failingCollectors{}
	s, _, log := newServerWith(t, registry, publications.NewPostgres(d.Runtime), stores)
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	for _, r := range collectorRoutes() {
		got := sendWith(s, r.method, r.target(orgA, fleetA, unknownID), r.body(unknownID))
		expectError(t, r.name, got, http.StatusInternalServerError, codeInternalError, messageInternalError)
	}
	for _, target := range []string{fleetsTarget(orgA), fleetTarget(orgA, fleetA)} {
		expectError(t, "a fleet read's summary", call(s, http.MethodGet, target, nil, ""), http.StatusInternalServerError, codeInternalError, messageInternalError)
	}
	failures := 0
	for _, line := range logLines(t, log.String()) {
		if line["msg"] == "request" {
			continue
		}
		failures++
		if line["msg"] != "the collectors store failed" {
			t.Fatalf("an unexpected log line: %v", line)
		}
		for key := range line {
			if !slices.Contains([]string{"time", "level", "msg", "route"}, key) {
				t.Fatalf("the failure's log line has %q: %v", key, line)
			}
		}
	}
	if failures != len(collectorRoutes())+2 || strings.Contains(log.String(), errStoreDown.Error()) {
		t.Fatalf("%d failure lines, or the store's text in the log: %s", failures, log.String())
	}
}

// collectorReplyOf is GET …/collectors/{collectorId}'s reply, decoded.
func collectorReplyOf(t *testing.T, s *Server, organizationID, fleetID, collectorID string) collectorInstanceReply {
	t.Helper()
	got := call(s, http.MethodGet, collectorTarget(organizationID, fleetID, collectorID), nil, "")
	expectStatus(t, "reading a collector", got, http.StatusOK)
	var reply collectorInstanceReply
	decodeInto(t, got.Body.Bytes(), &reply)
	return reply
}

// fleetSummaryOf is the fleet's summary as GET …/fleets/{fleetId} answers
// it, checked to be the one the list answers too.
func fleetSummaryOf(t *testing.T, s *Server, organizationID, fleetID string) summaryReply {
	t.Helper()
	var one fleetDetailReply
	decodeInto(t, call(s, http.MethodGet, fleetTarget(organizationID, fleetID), nil, "").Body.Bytes(), &one)
	var page fleetListReply
	decodeInto(t, call(s, http.MethodGet, fleetsTarget(organizationID), nil, "").Body.Bytes(), &page)
	for _, f := range page.Fleets {
		if f.ID == fleetID && !reflect.DeepEqual(f.Summary, one.Summary) {
			t.Fatalf("the list's summary %+v isn't the fleet's %+v", f.Summary, one.Summary)
		}
	}
	return one.Summary
}

// A collector's liveness is read at the server's injected clock, each
// step's lower edge included (59.999 s online, 60 s warning, 119.999 s
// warning, 120 s unknown, 299.999 s unknown, 300 s offline), in the
// collector's reply, the list's and the fleet's summary alike.
func TestLivenessAtTheLaddersEdgesByTheInjectedClock(t *testing.T) {
	s, _, _ := fleetServer(t)
	assertLivenessEdges(t, s)
}

func assertLivenessEdges(t *testing.T, s *Server) {
	t.Helper()
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	c := enrollCollector(t, s, orgA, fleetA, "edge-01")
	lastSeen := c.LastSeenAt
	for _, edge := range []struct {
		age  time.Duration
		want collectors.Liveness
	}{
		{0, collectors.Online},
		{59999 * time.Millisecond, collectors.Online},
		{60 * time.Second, collectors.Warning},
		{119999 * time.Millisecond, collectors.Warning},
		{120 * time.Second, collectors.Unknown},
		{299999 * time.Millisecond, collectors.Unknown},
		{300 * time.Second, collectors.Offline},
		{24 * time.Hour, collectors.Offline},
	} {
		now := lastSeen.Add(edge.age)
		s.clock = func() time.Time { return now }
		if got := collectorReplyOf(t, s, orgA, fleetA, c.ID.String()); got.Liveness != edge.want || got.LastSeenAt != formatTime(lastSeen) {
			t.Errorf("at %s: %s (last seen %s), want %s", edge.age, got.Liveness, got.LastSeenAt, edge.want)
		}
		var page collectorListReply
		decodeInto(t, call(s, http.MethodGet, collectorsTarget(orgA, fleetA), nil, "").Body.Bytes(), &page)
		if len(page.Collectors) != 1 || page.Collectors[0].Liveness != edge.want {
			t.Errorf("at %s: the list says %+v, want %s", edge.age, page.Collectors, edge.want)
		}
		summary := fleetSummaryOf(t, s, orgA, fleetA)
		counted := map[collectors.Liveness]int{collectors.Online: summary.Online, collectors.Warning: summary.Warning,
			collectors.Unknown: summary.Unknown, collectors.Offline: summary.Offline}
		if summary.Collectors != 1 || counted[edge.want] != 1 {
			t.Errorf("at %s: the summary is %+v, want one %s", edge.age, summary, edge.want)
		}
	}
}

// Each collector answers what resolves for it, what it was served, what it
// reports and its sync, from hashes; the fleet's summary counts the same.
// Five collectors, one per sync, last seen at different steps of the
// ladder.
func TestCollectorRepliesAndTheFleetSummaryCountSyncAndLiveness(t *testing.T) {
	s, clock, _ := fleetServer(t)
	assertSyncAndLivenessReplies(t, s, clock)
}

func assertSyncAndLivenessReplies(t *testing.T, s *Server, clock *testClock) {
	t.Helper()
	ctx := context.Background()
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	seedPublication(orgA, publicationA, validBody(sampleYAML))(t, s)
	seedAssignedGroup(orgA, fleetA, "gateways", publicationA)(t, s)
	gateways := uuid.MustParse(groupID(t, s, orgA, fleetA, "gateways"))
	published := sha256Hex(sampleYAML)
	org, fleet := uuid.MustParse(orgA), uuid.MustParse(fleetA)

	// Enrolled 301 s, 150 s, 61 s, 10 s and 0 s before the reading.
	ages := []time.Duration{301 * time.Second, 150 * time.Second, 61 * time.Second, 10 * time.Second, 0}
	names := []string{"offline-drifted", "unknown-failed", "warning-in-sync", "online-pending", "online-unassigned"}
	start := clock.Now()
	ids := map[string]uuid.UUID{}
	for i, name := range names {
		clock.Advance(start.Add(ages[0] - ages[i]).Sub(clock.Now()))
		c := enrollCollector(t, s, orgA, fleetA, name)
		ids[name] = c.ID
		if name == "online-unassigned" {
			continue
		}
		if _, err := s.collectors.Update(ctx, org, fleet, c.ID, collectors.Changes{GroupID: &gateways}, clock.Now()); err != nil {
			t.Fatal(err)
		}
	}
	at := clock.Now()
	pub := collectors.Ref{Source: collectors.SourcePublication, ID: uuid.MustParse(publicationA)}
	report := func(name string, effective *collectors.Effective) {
		t.Helper()
		if _, err := s.collectors.Report(ctx, collectors.Report{OrganizationID: org, FleetID: fleet, CollectorID: ids[name],
			Served: &collectors.Served{Ref: pub, SHA256: published, At: at}, Effective: effective,
			Reported: collectors.Reported{CollectorVersion: ptr("0.162.0"), IP: ptr("10.0.0.7"), HostIP: ptr("192.168.0.7"),
				Attributes: map[string]string{"host.name": name}}}); err != nil {
			t.Fatal(err)
		}
	}
	other := strings.Repeat("cd", 32)
	report("offline-drifted", &collectors.Effective{SHA256: other, Status: collectors.StatusApplied, At: at})
	report("unknown-failed", &collectors.Effective{SHA256: published, Status: collectors.StatusFailed, Error: "it broke", At: at})
	report("warning-in-sync", &collectors.Effective{SHA256: published, Status: collectors.StatusApplied, At: at})

	for name, want := range map[string]struct {
		liveness collectors.Liveness
		sync     string
	}{
		"offline-drifted":   {collectors.Offline, "drifted"},
		"unknown-failed":    {collectors.Unknown, "failed"},
		"warning-in-sync":   {collectors.Warning, "inSync"},
		"online-pending":    {collectors.Online, "pending"},
		"online-unassigned": {collectors.Online, "unassigned"},
	} {
		got := collectorReplyOf(t, s, orgA, fleetA, ids[name].String())
		if got.Liveness != want.liveness || got.Sync != want.sync || got.Name != name {
			t.Errorf("%s: %s and %s, want %s and %s", name, got.Liveness, got.Sync, want.liveness, want.sync)
		}
		switch name {
		case "online-unassigned":
			if got.Resolution != nil || got.Served != nil || got.Effective != nil || got.Pin != nil {
				t.Errorf("%s: %+v", name, got)
			}
		case "online-pending":
			if got.Resolution == nil || *got.Resolution != (resolutionReply{From: "groupPublication", ID: publicationA, SHA256: published}) ||
				got.Served != nil {
				t.Errorf("%s: resolution %+v, served %+v", name, got.Resolution, got.Served)
			}
		default:
			if got.Served == nil || *got.Served.Kind != "publication" || *got.Served.ID != publicationA || got.Served.SHA256 != published ||
				*got.Served.At != formatTime(at) || got.Effective == nil || got.Effective.At != formatTime(at) ||
				got.Reported.Attributes["host.name"] != name || *got.Reported.IP != "10.0.0.7" || *got.Reported.HostIP != "192.168.0.7" ||
				*got.Reported.CollectorVersion != "0.162.0" || got.EnrollmentTokenID == nil {
				t.Errorf("%s: %+v", name, got)
			}
			if wantError := name == "unknown-failed"; (got.Effective.Error != nil) != wantError ||
				(wantError && *got.Effective.Error != "it broke") {
				t.Errorf("%s: the effective error is %v", name, got.Effective.Error)
			}
		}
	}
	// The latest last seen is the collector enrolled last, at the reading.
	fresh := &rotateTokenReply{ExpiresAt: ptr(futureTime), ValidCount: 1}
	want := summaryReply{Collectors: 5, Online: 2, Warning: 1, Unknown: 1, Offline: 1,
		InSync: 1, Pending: 1, Failed: 1, Drifted: 1, Unassigned: 1, LastSeenAt: ptr(formatTime(at)), RotateToken: fresh}
	if got := fleetSummaryOf(t, s, orgA, fleetA); !reflect.DeepEqual(got, want) {
		t.Fatalf("the summary is %s, want %s", describeSummary(got), describeSummary(want))
	}
	// A fleet with no collector has a zero summary, no last seen, and its
	// rotate token; another fleet's collectors aren't counted.
	createFleet(t, s, orgA, fleetA2, "edge-dev", "server")
	if got := fleetSummaryOf(t, s, orgA, fleetA2); !reflect.DeepEqual(got, summaryReply{RotateToken: fresh}) {
		t.Fatalf("an empty fleet's summary is %s", describeSummary(got))
	}
}

// The list: searched, filtered by group, sorted and paged by the cursor
// it answers; a cursor of another sort is a 400.
func TestTheCollectorListSearchesSortsAndPages(t *testing.T) {
	s, clock, _ := fleetServer(t)
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	seedGroup(orgA, fleetA, "gateways")(t, s)
	for _, name := range []string{"bravo", "Alpha", "charlie"} {
		c := enrollCollector(t, s, orgA, fleetA, name)
		if name == "Alpha" {
			expectStatus(t, "moving", sendWith(s, http.MethodPatch, collectorTarget(orgA, fleetA, c.ID.String()),
				body{"groupId": groupID(t, s, orgA, fleetA, "gateways")}), http.StatusOK)
		}
		clock.Advance(time.Second)
	}
	namesOf := func(target string) ([]string, *string) {
		t.Helper()
		got := call(s, http.MethodGet, target, nil, "")
		expectStatus(t, target, got, http.StatusOK)
		var page collectorListReply
		decodeInto(t, got.Body.Bytes(), &page)
		var names []string
		for _, c := range page.Collectors {
			names = append(names, c.Name)
		}
		return names, page.NextCursor
	}
	base := collectorsTarget(orgA, fleetA)
	for query, want := range map[string][]string{
		"":                          {"charlie", "Alpha", "bravo"},
		"?order=asc":                {"bravo", "Alpha", "charlie"},
		"?sort=name&order=asc":      {"Alpha", "bravo", "charlie"},
		"?sort=lastSeen&order=desc": {"charlie", "Alpha", "bravo"},
		"?q=ALPH":                   {"Alpha"},
		"?q=%25":                    nil,
		"?groupId=" + unknownID:     nil,
		"?q=a&sort=name&order=desc": {"charlie", "bravo", "Alpha"},
	} {
		if got, _ := namesOf(base + query); !slices.Equal(got, want) {
			t.Errorf("%q: %v, want %v", query, got, want)
		}
	}
	first, next := namesOf(base + "?sort=name&order=asc&limit=2")
	if next == nil || !slices.Equal(first, []string{"Alpha", "bravo"}) {
		t.Fatalf("the first page = %v, %v", first, next)
	}
	if rest, last := namesOf(base + "?sort=name&order=asc&limit=2&cursor=" + *next); !slices.Equal(rest, []string{"charlie"}) || last != nil {
		t.Fatalf("the second page = %v, %v", rest, last)
	}
	expectError(t, "another sort's cursor", call(s, http.MethodGet, base+"?sort=created&order=asc&cursor="+*next, nil, ""),
		http.StatusBadRequest, codeInvalidRequest, messageCursor)
	expectError(t, "a q of 101 characters", call(s, http.MethodGet, base+"?q="+strings.Repeat("é", 101), nil, ""),
		http.StatusBadRequest, codeInvalidRequest, "q must be at most 100 characters, with no U+0000.")
	expectError(t, "the sort twice", call(s, http.MethodGet, base+"?sort=name&sort=name", nil, ""),
		http.StatusBadRequest, codeInvalidRequest, "sort must be given once.")
}

// A collector's PATCH: a rename, a move and a pin, together or apart; a
// pin wins over the group's assignment, and null unpins; each refusal (a
// name taken, a group the fleet doesn't hold, a published version the
// organization doesn't hold, a pin of another kind, an import's among
// them) changes nothing, not even the fields that would have passed.
func TestUpdateCollectorRenamesMovesAndPinsAllOrNothing(t *testing.T) {
	s, clock, _ := fleetServer(t)
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	seedPublication(orgA, publicationA, validBody(sampleYAML))(t, s)
	seedAssignedGroup(orgA, fleetA, "gateways", publicationA)(t, s)
	c := enrollCollector(t, s, orgA, fleetA, "edge-01")
	enrollCollector(t, s, orgA, fleetA, "edge-02")
	target := collectorTarget(orgA, fleetA, c.ID.String())
	gateways := groupID(t, s, orgA, fleetA, "gateways")

	clock.Advance(time.Minute)
	got := sendWith(s, http.MethodPatch, target, body{"name": "edge-01a", "groupId": gateways})
	expectStatus(t, "rename and move", got, http.StatusOK)
	var updated collectorInstanceReply
	decodeInto(t, got.Body.Bytes(), &updated)
	if updated.Name != "edge-01a" || updated.GroupID != gateways || updated.UpdatedAt != formatTime(clock.Now()) ||
		updated.Resolution == nil || updated.Resolution.From != "groupPublication" || updated.Sync != "pending" {
		t.Fatalf("after the rename and move: %+v", updated)
	}
	got = sendWith(s, http.MethodPatch, target, body{"pin": map[string]any{"kind": "publication", "id": publicationA}})
	decodeInto(t, got.Body.Bytes(), &updated)
	if updated.Pin == nil || *updated.Pin != (pinReply{Kind: "publication", ID: publicationA}) || updated.Resolution.From != "pinnedPublication" ||
		updated.Resolution.SHA256 != sha256Hex(sampleYAML) {
		t.Fatalf("after the pin: %+v", updated)
	}
	got = sendWith(s, http.MethodPatch, target, body{"pin": nil})
	decodeInto(t, got.Body.Bytes(), &updated)
	if updated.Pin != nil || updated.Resolution.From != "groupPublication" {
		t.Fatalf("after the unpin: %+v", updated)
	}

	before := dump(t, s, orgA)
	for _, refusal := range []struct {
		label   string
		b       body
		status  int
		code    errorCode
		message string
	}{
		{"a name taken, with a move", body{"name": "edge-02", "groupId": gateways}, http.StatusConflict, codeCollectorNameTaken, messageCollectorNameTaken},
		{"an unknown group, with a rename", body{"name": "edge-01b", "groupId": unknownID}, http.StatusNotFound, codeCollectorGroupNotFound,
			messageCollectorGroupNotFound},
		{"another organization's published version, with a rename", body{"name": "edge-01b", "pin": map[string]any{"kind": "publication", "id": unknownID}},
			http.StatusUnprocessableEntity, codePublicationNotFound, messagePublicationNotFound},
		// A pin names a published version only; an import's kind is refused
		// as any other kind.
		{"an import pin", body{"pin": map[string]any{"kind": "import", "id": unknownID}},
			http.StatusBadRequest, codeInvalidRequest, messagePin},
		{"nothing", body{}, http.StatusBadRequest, codeInvalidRequest, messageNoCollectorChange},
		// A name is pushed back as the ServerName header, which can't hold a
		// control character.
		{"a name with a newline", body{"name": "edge-01\nrack-7"}, http.StatusBadRequest, codeInvalidRequest, messageCollectorName},
		{"a name with a tab", body{"name": "edge\t01"}, http.StatusBadRequest, codeInvalidRequest, messageCollectorName},
		{"a null group", body{"groupId": nil}, http.StatusBadRequest, codeInvalidRequest, "groupId must be a UUID."},
		{"a pin of an unknown kind", body{"pin": map[string]any{"kind": "configuration", "id": unknownID}}, http.StatusBadRequest,
			codeInvalidRequest, messagePin},
		{"an unknown field", body{"name": "x", "labels": nil}, http.StatusBadRequest, codeInvalidRequest,
			"The request body has a field this API doesn't know."},
	} {
		expectError(t, refusal.label, sendWith(s, http.MethodPatch, target, refusal.b), refusal.status, refusal.code, refusal.message)
	}
	expectError(t, "an unknown collector", sendWith(s, http.MethodPatch, collectorTarget(orgA, fleetA, unknownID), body{"name": "x"}),
		http.StatusNotFound, codeCollectorNotFound, messageCollectorNotFound)
	expectError(t, "an unknown fleet", sendWith(s, http.MethodPatch, collectorTarget(orgA, fleetA2, c.ID.String()), body{"name": "x"}),
		http.StatusNotFound, codeFleetNotFound, messageFleetNotFound)
	if after := dump(t, s, orgA); after != before {
		t.Fatalf("a refused PATCH changed something:\n%s\n%s", before, after)
	}
}

// A bulk pin answers the ids it pinned and those the fleet doesn't hold,
// each sorted and once; null unpins; a pin target the organization doesn't
// hold pins nothing.
func TestBulkPinAnswersWhatItPinnedAndWhatItDidNotFind(t *testing.T) {
	s, _, _ := fleetServer(t)
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	seedPublication(orgA, publicationA, validBody(sampleYAML))(t, s)
	var mine []string
	for _, name := range []string{"edge-01", "edge-02", "edge-03"} {
		mine = append(mine, enrollCollector(t, s, orgA, fleetA, name).ID.String())
	}
	createFleet(t, s, orgA, fleetA2, "edge-dev", "server")
	sibling := enrollCollector(t, s, orgA, fleetA2, "edge-dev-01").ID.String()
	unknown := uuid.NewString()
	pin := map[string]any{"kind": "publication", "id": publicationA}

	got := sendWith(s, http.MethodPost, collectorsPinTarget(orgA, fleetA),
		body{"collectorIds": []string{mine[2], unknown, mine[0], sibling, mine[0]}, "pin": pin})
	expectStatus(t, "the bulk pin", got, http.StatusOK)
	var reply bulkPinReply
	decodeInto(t, got.Body.Bytes(), &reply)
	wantUpdated, wantNotFound := []string{mine[2], mine[0]}, []string{unknown, sibling}
	slices.Sort(wantUpdated)
	slices.Sort(wantNotFound)
	if !slices.Equal(reply.Updated, wantUpdated) || !slices.Equal(reply.NotFound, wantNotFound) {
		t.Fatalf("the bulk pin = %+v, want updated %v and not found %v", reply, wantUpdated, wantNotFound)
	}
	for i, id := range mine {
		got := collectorReplyOf(t, s, orgA, fleetA, id)
		if pinned := got.Pin != nil && got.Pin.ID == publicationA; pinned != (i != 1) {
			t.Errorf("collector %d pinned = %v", i, pinned)
		}
	}
	if got := collectorReplyOf(t, s, orgA, fleetA2, sibling); got.Pin != nil {
		t.Fatal("the sibling fleet's collector was pinned")
	}
	before := dump(t, s, orgA)
	expectError(t, "a published version the organization doesn't hold",
		sendWith(s, http.MethodPost, collectorsPinTarget(orgA, fleetA), body{"collectorIds": mine, "pin": map[string]any{"kind": "publication", "id": unknownID}}),
		http.StatusUnprocessableEntity, codePublicationNotFound, messagePublicationNotFound)
	expectError(t, "501 ids", sendWith(s, http.MethodPost, collectorsPinTarget(orgA, fleetA),
		body{"collectorIds": slices.Repeat([]string{unknown}, 501), "pin": nil}), http.StatusBadRequest, codeInvalidRequest, messageCollectorIDs)
	if after := dump(t, s, orgA); after != before {
		t.Fatal("a refused bulk pin changed something")
	}
	got = sendWith(s, http.MethodPost, collectorsPinTarget(orgA, fleetA), body{"collectorIds": mine, "pin": nil})
	decodeInto(t, got.Body.Bytes(), &reply)
	if len(reply.Updated) != 3 || len(reply.NotFound) != 0 || collectorReplyOf(t, s, orgA, fleetA, mine[0]).Pin != nil {
		t.Fatalf("the bulk unpin = %+v", reply)
	}
}

// A collector's error history pages newest first; a delete takes the
// collector and its history, freeing its name.
func TestCollectorErrorsPageNewestFirstAndADeleteTakesThem(t *testing.T) {
	s, clock, _ := fleetServer(t)
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	c := enrollCollector(t, s, orgA, fleetA, "edge-01")
	for _, message := range []string{"first", "second", "third"} {
		clock.Advance(time.Second)
		seedCollectorError(orgA, fleetA, "edge-01", message)(t, s)
	}
	target := collectorErrorsTarget(orgA, fleetA, c.ID.String())
	var page collectorErrorListReply
	decodeInto(t, call(s, http.MethodGet, target+"?limit=2", nil, "").Body.Bytes(), &page)
	if len(page.Errors) != 2 || page.Errors[0].Message != "third" || page.Errors[1].Message != "second" || page.NextCursor == nil {
		t.Fatalf("the first page = %+v", page)
	}
	decodeInto(t, call(s, http.MethodGet, target+"?limit=2&cursor="+*page.NextCursor, nil, "").Body.Bytes(), &page)
	if len(page.Errors) != 1 || page.Errors[0].Message != "first" || page.NextCursor != nil || page.Errors[0].ConfigSHA256 != nil {
		t.Fatalf("the second page = %+v", page)
	}
	expectStatus(t, "delete", call(s, http.MethodDelete, collectorTarget(orgA, fleetA, c.ID.String()), nil, ""), http.StatusNoContent)
	expectError(t, "its errors after the delete", call(s, http.MethodGet, target, nil, ""), http.StatusNotFound, codeCollectorNotFound,
		messageCollectorNotFound)
	expectError(t, "a second delete", call(s, http.MethodDelete, collectorTarget(orgA, fleetA, c.ID.String()), nil, ""), http.StatusNotFound,
		codeCollectorNotFound, messageCollectorNotFound)
	if strings.Contains(dump(t, s, orgA), "first") {
		t.Fatal("the collector's errors survived its delete")
	}
	enrollCollector(t, s, orgA, fleetA, "edge-01")
}

// The limits read back as set, null unsetting each.
func TestLimitsReadBackAsSet(t *testing.T) {
	s, _, _ := fleetServer(t)
	read := func() limitsJSON {
		t.Helper()
		var l limitsJSON
		decodeInto(t, call(s, http.MethodGet, limitsTarget(orgA), nil, "").Body.Bytes(), &l)
		return l
	}
	if l := read(); l.MaxCollectors != nil || l.RequestsPerMinute != nil {
		t.Fatalf("a new organization's limits = %+v", l)
	}
	got := sendWith(s, http.MethodPut, limitsTarget(orgA), body{"maxCollectors": 0, "requestsPerMinute": 120})
	var l limitsJSON
	decodeInto(t, got.Body.Bytes(), &l)
	if got.Code != http.StatusOK || l.MaxCollectors == nil || *l.MaxCollectors != 0 || l.RequestsPerMinute == nil || *l.RequestsPerMinute != 120 {
		t.Fatalf("the limits set = %d %+v", got.Code, l)
	}
	if again := read(); again.MaxCollectors == nil || *again.MaxCollectors != 0 || again.RequestsPerMinute == nil || *again.RequestsPerMinute != 120 {
		t.Fatalf("the limits read back = %+v", again)
	}
	expectStatus(t, "unsetting", sendWith(s, http.MethodPut, limitsTarget(orgA), body{"maxCollectors": nil, "requestsPerMinute": nil}), http.StatusOK)
	if again := read(); again.MaxCollectors != nil || again.RequestsPerMinute != nil {
		t.Fatalf("the limits unset = %+v", again)
	}
	expectError(t, "a negative cap", sendWith(s, http.MethodPut, limitsTarget(orgA), body{"maxCollectors": -1, "requestsPerMinute": nil}),
		http.StatusBadRequest, codeInvalidRequest, messageMaxCollectors)
	expectError(t, "zero requests", sendWith(s, http.MethodPut, limitsTarget(orgA), body{"maxCollectors": nil, "requestsPerMinute": 0}),
		http.StatusBadRequest, codeInvalidRequest, messageRequestsPerMinute)
	if again := read(); again.MaxCollectors != nil {
		t.Fatal("a refused PUT set the cap")
	}
	var b limitsJSON
	decodeInto(t, call(s, http.MethodGet, limitsTarget(orgB), nil, "").Body.Bytes(), &b)
	if b.MaxCollectors != nil || b.RequestsPerMinute != nil {
		t.Fatalf("B's limits changed: %+v", b)
	}
}

// A configuration's usage answers each published version with the groups
// assigned it and a count of the collectors pinned to it; while one is
// used, deleting it, or the configuration's, answers 409
// publication_in_use and deletes nothing. Unused, both delete.
func TestAPublishedVersionInUseIsNotDeletedAndItsUsageSaysWhy(t *testing.T) {
	s, _, _ := fleetServer(t)
	assertPublishedVersionsInUseAreKept(t, s, func() string { return dump(t, s, orgA) })
}

func assertPublishedVersionsInUseAreKept(t *testing.T, s *Server, snapshot func() string) {
	t.Helper()
	createFleet(t, s, orgA, fleetA, "edge-prod", "server")
	seedPublication(orgA, publicationA, validBody(sampleYAML))(t, s)
	seedAssignedGroup(orgA, fleetA, "gateways", publicationA)(t, s)
	edge := enrollCollector(t, s, orgA, fleetA, "edge-01")
	seedPin(orgA, fleetA, "edge-01", publicationA)(t, s)
	gateways := groupID(t, s, orgA, fleetA, "gateways")

	var usage usageReply
	decodeInto(t, call(s, http.MethodGet, usageTarget(orgA, configurationA), nil, "").Body.Bytes(), &usage)
	if !usage.InUse || len(usage.Publications) != 1 || usage.Publications[0].ID != publicationA || usage.Publications[0].PinnedCollectors != 1 ||
		len(usage.Publications[0].Groups) != 1 || usage.Publications[0].Groups[0] != (groupRefReply{FleetID: fleetA, GroupID: gateways}) ||
		usage.Publications[0].Version.Number != 3 {
		t.Fatalf("the usage = %+v", usage)
	}
	before := snapshot()
	expectError(t, "delete the published version", call(s, http.MethodDelete, publicationTarget(orgA, publicationA), nil, ""),
		http.StatusConflict, codePublicationInUse, messagePublicationInUse)
	expectError(t, "delete the configuration's", call(s, http.MethodDelete, configurationTarget(orgA, configurationA), nil, ""),
		http.StatusConflict, codePublicationInUse, messageConfigurationPublicationInUse)
	if after := snapshot(); after != before {
		t.Fatalf("a refused delete changed something:\n%s\n%s", before, after)
	}
	expectStatus(t, "the published version is still there", call(s, http.MethodGet, publicationTarget(orgA, publicationA), nil, ""), http.StatusOK)

	// The group alone still holds it.
	expectStatus(t, "unpin", sendWith(s, http.MethodPatch, collectorTarget(orgA, fleetA, edge.ID.String()), body{"pin": nil}), http.StatusOK)
	expectError(t, "delete, the group's still", call(s, http.MethodDelete, publicationTarget(orgA, publicationA), nil, ""),
		http.StatusConflict, codePublicationInUse, messagePublicationInUse)
	expectStatus(t, "unassign", sendWith(s, http.MethodPatch, groupTarget(orgA, fleetA, gateways), body{"assignedPublicationId": nil}), http.StatusOK)
	decodeInto(t, call(s, http.MethodGet, usageTarget(orgA, configurationA), nil, "").Body.Bytes(), &usage)
	if usage.InUse || len(usage.Publications) != 1 || usage.Publications[0].PinnedCollectors != 0 || len(usage.Publications[0].Groups) != 0 {
		t.Fatalf("the usage once unused = %+v", usage)
	}
	expectStatus(t, "delete the configuration's, unused", call(s, http.MethodDelete, configurationTarget(orgA, configurationA), nil, ""), http.StatusNoContent)
	expectError(t, "the published version is gone", call(s, http.MethodGet, publicationTarget(orgA, publicationA), nil, ""),
		http.StatusNotFound, codeNotFound, messageNotFound)
	decodeInto(t, call(s, http.MethodGet, usageTarget(orgA, configurationA), nil, "").Body.Bytes(), &usage)
	if usage.InUse || len(usage.Publications) != 0 || usage.ConfigurationID != configurationA {
		t.Fatalf("the usage once deleted = %+v", usage)
	}
}

func ptr[T any](v T) *T { return &v }
