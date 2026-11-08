package management

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/agent/agenttest"
	"github.com/telflo/opamp-server/middleware"
)

// pollAs sends one poll with bearer through an agent listener's handler,
// its OpAMP-Instance-Uid header header ("" for none), as the supervisor
// sends every request.
func pollAs(t *testing.T, agent http.Handler, bearer, header string, message []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, middleware.AgentRoute, bytes.NewReader(message))
	request.Header.Set("Content-Type", "application/x-protobuf")
	request.Header.Set("Authorization", "Bearer "+bearer)
	if header != "" {
		request.Header.Set(middleware.InstanceUIDHeader, header)
	}
	recorder := httptest.NewRecorder()
	agent.ServeHTTP(recorder, request)
	return recorder
}

// describedMessage is a poll naming the group in its agent description.
func describedMessage(t *testing.T, uid uuid.UUID, sequence uint64, group string) []byte {
	t.Helper()
	return agenttest.Message(t, uid, sequence, group)
}

// handedOff enrolls a collector through the fleet's default token and
// answers the rotate token's bearer it was handed.
func handedOff(t *testing.T, r *replica, f replicaFleet) string {
	t.Helper()
	_, answer := pollThrough(t, r.agent, f.created.Tokens.Default.Bearer, agentMessage(t, uuid.New(), 0, f.created.DefaultGroup.ID))
	rotate := pushedBearer(answer)
	if rotate == "" {
		t.Fatal("no hand-off")
	}
	return rotate
}

// One collector polling in a tight loop is refused by its guard before the
// organization's rate counts it, so its organization's other collectors on
// the pod are still answered. Its requests carry the supervisor's
// OpAMP-Instance-Uid header, which the stack checks the guard by; past its
// burst (CollectorBurst) it is refused 429 with the guard's Retry-After,
// and its organization's other collector, polling after more runaway
// requests than its organization's whole rate, is answered 200, with the
// header and without.
func TestARunawayCollectorDoesNotSpendItsOrganizationsRate(t *testing.T) {
	keys := testKeys(t)
	settings := replicaSettings(t, keys)
	clock := newTestClock()
	r := testReplica(t, settings, testReplicaStores(t), clock, &syncBuffer{})
	f := fleetThrough(t, r.management)
	rotate := handedOff(t, r, f)
	runaway, other := uuid.New(), uuid.New()
	if rec := pollAs(t, r.agent, rotate, other.String(), agentMessage(t, other, 0, f.created.DefaultGroup.ID)); rec.Code != http.StatusOK {
		t.Fatalf("the other collector's first poll: %d", rec.Code)
	}
	codes := map[int]int{}
	for i := range settings.Agent.Limits.DefaultRequestsPerMinute + 10 {
		rec := pollAs(t, r.agent, rotate, runaway.String(), agentMessage(t, runaway, uint64(i), f.created.DefaultGroup.ID))
		codes[rec.Code]++
		if rec.Code == http.StatusTooManyRequests && rec.Header().Get("Retry-After") != "60" {
			t.Fatalf("the runaway's refusal: Retry-After %q; want the guard's 60", rec.Header().Get("Retry-After"))
		}
	}
	if codes[http.StatusOK] != middleware.CollectorBurst || codes[http.StatusTooManyRequests] != settings.Agent.Limits.DefaultRequestsPerMinute+10-middleware.CollectorBurst {
		t.Fatalf("the runaway's answers: %v; want its burst of %d answered, the rest 429", codes, middleware.CollectorBurst)
	}
	if rec := pollAs(t, r.agent, rotate, other.String(), agentMessage(t, other, 1, f.created.DefaultGroup.ID)); rec.Code != http.StatusOK {
		t.Fatalf("the other collector after the runaway: %d; want 200", rec.Code)
	}
	if rec, _ := pollThrough(t, r.agent, rotate, agentMessage(t, other, 2, f.created.DefaultGroup.ID)); rec.Code != http.StatusOK {
		t.Fatalf("the other collector after the runaway, without the header: %d; want 200", rec.Code)
	}
}

// The guard checked by the header, the body must be the same collector's:
// a body whose instance uid isn't the header's is refused 400
// (instance_uid_mismatch) and enrolls nothing; a header that isn't a UUID
// is refused 400 before the body is read. Without the header the handler
// checks the guard by the body's instance uid: past its burst, 429.
func TestTheInstanceUIDHeaderMustBeTheBodysCollector(t *testing.T) {
	keys := testKeys(t)
	settings := replicaSettings(t, keys)
	clock := newTestClock()
	log := &syncBuffer{}
	r := testReplica(t, settings, testReplicaStores(t), clock, log)
	f := fleetThrough(t, r.management)
	rotate := handedOff(t, r, f)
	body, header := uuid.New(), uuid.New()
	if rec := pollAs(t, r.agent, rotate, header.String(), agentMessage(t, body, 0, f.created.DefaultGroup.ID)); rec.Code != http.StatusBadRequest {
		t.Fatalf("a body of another collector than the header's: %d; want 400", rec.Code)
	}
	for _, bad := range []string{"not-a-uuid", body.String() + "x"} {
		if rec := pollAs(t, r.agent, rotate, bad, agentMessage(t, body, 0, f.created.DefaultGroup.ID)); rec.Code != http.StatusBadRequest {
			t.Fatalf("the header %q: %d; want 400", bad, rec.Code)
		}
	}
	listed := call(r.management, http.MethodGet, collectorsTarget(f.organization, f.created.Fleet.ID), nil, "")
	if strings.Contains(listed.Body.String(), body.String()) {
		t.Fatalf("a refused poll enrolled its collector: %s", listed.Body.String())
	}
	if !strings.Contains(log.String(), `"outcome":"instance_uid_mismatch"`) || !strings.Contains(log.String(), `"outcome":"invalid_instance_uid_header"`) {
		t.Fatalf("the refusals' lines are missing: %s", log.String())
	}
	if rec := pollAs(t, r.agent, rotate, strings.ToUpper(body.String()), agentMessage(t, body, 0, f.created.DefaultGroup.ID)); rec.Code != http.StatusOK {
		t.Fatalf("the header in capitals, the body's collector: %d; want 200", rec.Code)
	}
	codes := map[int]int{}
	for i := range middleware.CollectorBurst + 5 {
		rec, _ := pollThrough(t, r.agent, rotate, agentMessage(t, body, uint64(i+1), f.created.DefaultGroup.ID))
		codes[rec.Code]++
	}
	if codes[http.StatusTooManyRequests] == 0 {
		t.Fatalf("without the header, past the burst: %v; want the handler's guard to refuse", codes)
	}
}

// refusalLines are the log's lines of outcome.
func refusalLines(t *testing.T, log *syncBuffer, outcome string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range logLines(t, log.String()) {
		if line["outcome"] == outcome {
			lines = append(lines, line)
		}
	}
	return lines
}

// A supervisor keeps polling through a 403, so the 403s are logged a line
// a minute per outcome, with the count since the last, by the injected
// clock: an archived fleet's (the authentication's) and a foreign group's
// (the poll's). Fifty polls of each are one line; a minute on, the next is
// a line counting fifty.
func TestTheForbiddenRefusalsAreLoggedOnceAMinutePerOutcome(t *testing.T) {
	keys := testKeys(t)
	settings := replicaSettings(t, keys)
	clock := newTestClock()
	log := &syncBuffer{}
	r := testReplica(t, settings, testReplicaStores(t), clock, log)
	f := fleetThrough(t, r.management)
	other := fleetThrough(t, r.management)

	foreign := other.created.DefaultGroup.ID
	poll := func() int {
		rec, _ := pollThrough(t, r.agent, f.created.Tokens.Default.Bearer, describedMessage(t, uuid.New(), 0, foreign))
		return rec.Code
	}
	archivedRotate := handedOffArchived(t, r, other)
	pollArchived := func() int {
		rec, _ := pollThrough(t, r.agent, archivedRotate, agentMessage(t, uuid.New(), 1, other.created.DefaultGroup.ID))
		return rec.Code
	}
	for _, c := range []struct {
		outcome string
		poll    func() int
	}{{"foreign_group", poll}, {"fleet_archived", pollArchived}} {
		for i := range 50 {
			if code := c.poll(); code != http.StatusForbidden {
				t.Fatalf("%s, poll %d: %d; want 403", c.outcome, i, code)
			}
		}
		lines := refusalLines(t, log, c.outcome)
		if len(lines) != 1 || lines[0]["refused_since_last_log"] != float64(1) {
			t.Fatalf("%s: %d lines after 50 refusals (%v); want one, counting 1", c.outcome, len(lines), lines)
		}
		clock.Advance(time.Minute)
		if code := c.poll(); code != http.StatusForbidden {
			t.Fatalf("%s a minute on: %d", c.outcome, code)
		}
		lines = refusalLines(t, log, c.outcome)
		if len(lines) != 2 || lines[1]["refused_since_last_log"] != float64(50) {
			t.Fatalf("%s: a minute on, lines %v; want a second counting 50", c.outcome, lines)
		}
	}
}

// The 403s are sampled per outcome and organization, so one
// organization's flood never hides another's. Two organizations' 403s of
// one outcome in the same minute are two lines, one naming each; more of
// them in that minute add none. Both the poll's (a foreign group) and the
// authentication's (an archived fleet).
func TestTheForbiddenRefusalsAreLoggedPerOrganization(t *testing.T) {
	keys := testKeys(t)
	settings := replicaSettings(t, keys)
	clock := newTestClock()
	log := &syncBuffer{}
	r := testReplica(t, settings, testReplicaStores(t), clock, log)
	first, second, other := fleetThrough(t, r.management), fleetThrough(t, r.management), fleetThrough(t, r.management)
	foreign := other.created.DefaultGroup.ID
	perOrganization := func(outcome string, polls map[string]func() int) {
		t.Helper()
		for range 3 {
			for organization, poll := range polls {
				if code := poll(); code != http.StatusForbidden {
					t.Fatalf("%s, %s: %d; want 403", outcome, organization, code)
				}
			}
		}
		lines := refusalLines(t, log, outcome)
		named := map[any]int{}
		for _, line := range lines {
			named[line["organization_id"]]++
		}
		if len(lines) != 2 || named[first.organization] != 1 || named[second.organization] != 1 {
			t.Fatalf("%s: two organizations' 403s in one minute logged %v; want a line naming each", outcome, lines)
		}
	}
	foreignGroup := func(f replicaFleet) func() int {
		return func() int {
			rec, _ := pollThrough(t, r.agent, f.created.Tokens.Default.Bearer, describedMessage(t, uuid.New(), 0, foreign))
			return rec.Code
		}
	}
	perOrganization("foreign_group", map[string]func() int{
		first.organization: foreignGroup(first), second.organization: foreignGroup(second)})
	archived := func(f replicaFleet) func() int {
		rotate := handedOffArchived(t, r, f)
		return func() int {
			rec, _ := pollThrough(t, r.agent, rotate, agentMessage(t, uuid.New(), 1, f.created.DefaultGroup.ID))
			return rec.Code
		}
	}
	perOrganization("fleet_archived", map[string]func() int{
		first.organization: archived(first), second.organization: archived(second)})
}

// The production wiring logs the trailing counts. One organization's 403s
// of an outcome are a line and two counted; a minute on, another
// organization's 403 sweeps, and the poll's (a foreign group) and the
// authentication's (an archived fleet) samplers each write a line naming
// the first organization, its outcome and its two trailing refusals,
// beside the second organization's own line.
func TestTheForbiddenRefusalsTrailingCountsAreLogged(t *testing.T) {
	keys := testKeys(t)
	settings := replicaSettings(t, keys)
	clock := newTestClock()
	log := &syncBuffer{}
	r := testReplica(t, settings, testReplicaStores(t), clock, log)
	foreign := fleetThrough(t, r.management).created.DefaultGroup.ID
	foreignGroup := func(f replicaFleet) func() int {
		return func() int {
			rec, _ := pollThrough(t, r.agent, f.created.Tokens.Default.Bearer, describedMessage(t, uuid.New(), 0, foreign))
			return rec.Code
		}
	}
	archived := func(f replicaFleet) func() int {
		rotate := handedOffArchived(t, r, f)
		return func() int {
			rec, _ := pollThrough(t, r.agent, rotate, agentMessage(t, uuid.New(), 1, f.created.DefaultGroup.ID))
			return rec.Code
		}
	}
	// Each case its own two organizations' fleets (an archived fleet's
	// default token is refused fleet_archived, not foreign_group).
	type pair struct{ first, second replicaFleet }
	groups := pair{fleetThrough(t, r.management), fleetThrough(t, r.management)}
	archives := pair{fleetThrough(t, r.management), fleetThrough(t, r.management)}
	for _, c := range []struct {
		outcome, message      string
		organizations         pair
		firstPoll, secondPoll func() int
	}{
		{"foreign_group", "agent poll refused", groups, foreignGroup(groups.first), foreignGroup(groups.second)},
		{"fleet_archived", "agent request refused", archives, archived(archives.first), archived(archives.second)},
	} {
		first, second := c.organizations.first, c.organizations.second
		for i := range 3 {
			if code := c.firstPoll(); code != http.StatusForbidden {
				t.Fatalf("%s, the first organization's poll %d: %d; want 403", c.outcome, i, code)
			}
		}
		if lines := refusalLines(t, log, c.outcome); len(lines) != 1 {
			t.Fatalf("%s: three 403s of one organization logged %v; want one line", c.outcome, lines)
		}
		clock.Advance(time.Minute)
		if code := c.secondPoll(); code != http.StatusForbidden {
			t.Fatalf("%s, the second organization's poll a minute on: %d; want 403", c.outcome, code)
		}
		lines := refusalLines(t, log, c.outcome)
		if len(lines) != 3 {
			t.Fatalf("%s: a minute on, lines %v; want the first organization's trailing count and the second's line", c.outcome, lines)
		}
		flushed, own := lines[1], lines[2]
		if flushed["msg"] != c.message || flushed["organization_id"] != first.organization ||
			flushed["refused_since_last_log"] != float64(2) || flushed["status"] != float64(http.StatusForbidden) {
			t.Fatalf("%s: the trailing line %v; want the first organization's 2 refusals", c.outcome, flushed)
		}
		if own["organization_id"] != second.organization || own["refused_since_last_log"] != float64(1) {
			t.Fatalf("%s: the second organization's line %v", c.outcome, own)
		}
	}
}

// handedOffArchived is the rotate token's bearer of f's fleet, handed off
// while the fleet was active, the fleet then archived (the bearer's token
// row not yet read, so not cached as active).
func handedOffArchived(t *testing.T, r *replica, f replicaFleet) string {
	t.Helper()
	rotate := handedOff(t, r, f)
	archived := call(r.management, http.MethodPatch, fleetTarget(f.organization, f.created.Fleet.ID),
		body{"status": "archived"}.bytes(), jsonType)
	expectStatus(t, "archiving", archived, http.StatusOK)
	return rotate
}

// Once the fleet's default token is revoked, a
// supervisor presenting it is refused 401; the new default token issued
// after it authenticates, enrolls a collector and hands it the fleet's
// rotate token.
func TestANewDefaultTokenEnrollsWhereTheRevokedOneIsRefused(t *testing.T) {
	keys := testKeys(t)
	settings := replicaSettings(t, keys)
	clock := newTestClock()
	r := testReplica(t, settings, testReplicaStores(t), clock, &syncBuffer{})
	f := fleetThrough(t, r.management)
	expectStatus(t, "revoke the default token", call(r.management, http.MethodPost,
		revokeTarget(f.organization, f.created.Fleet.ID, f.created.Tokens.Default.ID), actorBody().bytes(), jsonType), http.StatusOK)
	if rec, _ := pollThrough(t, r.agent, f.created.Tokens.Default.Bearer, agentMessage(t, uuid.New(), 0, f.created.DefaultGroup.ID)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a poll on the revoked default token: %d; want 401", rec.Code)
	}
	issued := call(r.management, http.MethodPost, issueDefaultTarget(f.organization, f.created.Fleet.ID), actorBody().bytes(), jsonType)
	expectStatus(t, "a new default token", issued, http.StatusCreated)
	var n revealedReply
	decodeInto(t, issued.Body.Bytes(), &n)
	rec, answer := pollThrough(t, r.agent, n.Bearer, agentMessage(t, uuid.New(), 0, f.created.DefaultGroup.ID))
	if rec.Code != http.StatusOK || pushedBearer(answer) == "" {
		t.Fatalf("a poll on the new default token: %d; want 200 and the rotate token handed off", rec.Code)
	}
}
