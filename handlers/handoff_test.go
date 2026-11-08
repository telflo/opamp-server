package handlers

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"

	"github.com/telflo/opamp-server/middleware"
	"github.com/telflo/opamp-server/store/collectorgroups"
	"github.com/telflo/opamp-server/store/collectors"
	"github.com/telflo/opamp-server/store/enrollmenttokens"
	"github.com/telflo/opamp-server/store/fleets"
	"github.com/telflo/opamp-server/store/organizations"
	"github.com/telflo/opamp-server/store/storetest"
	"github.com/telflo/opamp-server/tokens"
)

// pushedClaims opens the answer's pushed bearer with the harness's keyring.
func (x *harness) pushedClaims(a *protobufs.ServerToAgent) (tokens.Claims, string) {
	x.t.Helper()
	auth := settingsHeader(a, "Authorization")
	bearer, found := strings.CutPrefix(auth, "Bearer ")
	if !found {
		x.t.Fatalf("no bearer pushed: %v", a.GetConnectionSettings())
	}
	claims, err := x.keyring.Read(bearer)
	if err != nil {
		x.t.Fatalf("the pushed bearer doesn't open: %v", err)
	}
	return claims, bearer
}

// The hand-off: a default token's poll enrolls the collector in the group
// its description names (always one; here the fleet's Default group), named
// by its ServerName, records it, serves nothing (its own file, reported, is
// left as it is), asks for its full state, and pushes a fresh bearer that
// opens to the fleet's rotate token and this fleet, with the server's URL,
// the heartbeat and the collector's name.
func TestADefaultTokensPollEnrollsTheCollectorAndHandsItTheRotateToken(t *testing.T) {
	x := newHarness(t, testStores(t))
	c, _ := assigned(x, gatewayYAML)
	uid := uuid.New()
	rec, answer := x.pollAsIs(defaultToken(c), message(uid, 1, withSettings(),
		withEffective(localYAML, ""), withGroup(c.DefaultGroupID)), "edge-01")
	if rec.Code != http.StatusOK {
		t.Fatalf("the default token's poll: %d %q", rec.Code, rec.Body.String())
	}

	if answer.GetRemoteConfig() != nil || !fullState(answer) {
		t.Fatalf("a default token was served a configuration, or not asked for its full state")
	}
	claims, _ := x.pushedClaims(answer)
	if claims.TokenID != c.RotateTokenID || claims.FleetID != c.Fleet.ID {
		t.Fatalf("pushed %+v; want the rotate token %s of fleet %s", claims, c.RotateTokenID, c.Fleet.ID)
	}
	opamp := answer.GetConnectionSettings().GetOpamp()
	if opamp.GetDestinationEndpoint() != serverURL || opamp.GetHeartbeatIntervalSeconds() != 30 ||
		settingsHeader(answer, "ServerName") != "edge-01" {
		t.Fatalf("connection settings: %v", opamp)
	}
	l := x.collector(c, uid)
	if l.Name != "edge-01" || l.GroupID != c.DefaultGroupID || l.ServedSHA256 != nil ||
		l.EnrollmentTokenID == nil || *l.EnrollmentTokenID != c.DefaultTokenID {
		t.Fatalf("enrolled: %+v", l.Collector)
	}
}

// The hand-off end to end through the authentication: the pushed bearer
// authenticates, and that poll is served the configuration (the
// collector's token moves to the rotate token).
func TestTheHandedOffBearerIsServedTheConfiguration(t *testing.T) {
	x := newHarness(t, testStores(t))
	c, _ := assigned(x, gatewayYAML)
	uid := uuid.New()
	defaultBearer, err := x.keyring.Mint(tokens.Claims{TokenID: c.DefaultTokenID, FleetID: c.Fleet.ID})
	if err != nil {
		t.Fatal(err)
	}
	rec, answer := x.pollBearer(defaultBearer, message(uid, 0, withSettings(), withGroup(c.DefaultGroupID)))
	if rec.Code != http.StatusOK {
		t.Fatalf("the default bearer: %d", rec.Code)
	}
	_, rotated := x.pushedClaims(answer)
	rec, answer = x.pollBearer(rotated, message(uid, 0, withSettings()))
	if rec.Code != http.StatusOK || pushed(t, answer) != gatewayYAML || answer.GetConnectionSettings() != nil {
		t.Fatalf("the pushed bearer: %d, push %q, settings %v", rec.Code, pushed(t, answer), answer.GetConnectionSettings())
	}
	if l := x.collector(c, uid); *l.EnrollmentTokenID != c.RotateTokenID {
		t.Fatalf("the collector's token is %s; want the rotate token", *l.EnrollmentTokenID)
	}
}

// An agent that doesn't advertise AcceptsOpAMPConnectionSettings is pushed
// nothing (and the fleet's tokens aren't read).
func TestNoSettingsArePushedWithoutTheCapability(t *testing.T) {
	x := newHarness(t, testStores(t))
	c := x.fleet(storetest.OrgA, "edge-prod", fleets.TypeServer)
	answer := x.ok(defaultToken(c), message(uuid.New(), 1))
	if answer.GetConnectionSettings() != nil || !fullState(answer) {
		t.Fatalf("without the capability: settings %v, full state %v", answer.GetConnectionSettings(), fullState(answer))
	}
}

// A fleet with no valid rotate token is suspended: its
// collectors' default token is acknowledged, pushed nothing, served
// nothing.
func TestASuspendedFleetsDefaultTokenIsPushedNothing(t *testing.T) {
	x := newHarness(t, testStores(t))
	c, _ := assigned(x, gatewayYAML)
	if _, _, err := x.s.Tokens.Revoke(t.Context(), storetest.OrgA, c.Fleet.ID, c.RotateTokenID, storetest.Ana, x.clock.Now()); err != nil {
		t.Fatal(err)
	}
	answer := x.ok(defaultToken(c), message(uuid.New(), 1, withSettings()))
	if answer.GetConnectionSettings() != nil || answer.GetRemoteConfig() != nil || !fullState(answer) {
		t.Fatalf("suspended: %v", answer)
	}
}

// A rotate token in its grace (do_refresh) is served and
// pushed the fleet's newest rotate token; the newest token isn't pushed to
// itself; a token without do_refresh is pushed nothing.
func TestARotatedTokenInItsGraceIsPushedTheNewestOne(t *testing.T) {
	x := newHarness(t, testStores(t))
	c, _ := assigned(x, gatewayYAML)
	uid := uuid.New()
	if answer := x.ok(rotate(c), message(uid, 1, withSettings())); answer.GetConnectionSettings() != nil {
		t.Fatalf("a token without do_refresh was pushed settings")
	}
	rotation, err := x.s.Tokens.Rotate(t.Context(), enrollmenttokens.Rotation{OrganizationID: storetest.OrgA, FleetID: c.Fleet.ID,
		GraceEndsAt: x.clock.Now().Add(24 * time.Hour), Actor: storetest.Ana, At: x.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	old := rotate(c)
	old.DoRefresh = true
	answer := x.ok(old, message(uid, 2, withSettings()))
	if claims, _ := x.pushedClaims(answer); claims.TokenID != rotation.Token.ID || claims.FleetID != c.Fleet.ID {
		t.Fatalf("pushed %+v; want the new rotate token", claims)
	}
	// The new token revoked, the old one is the newest valid one: it isn't
	// pushed to itself.
	if _, _, err := x.s.Tokens.Revoke(t.Context(), storetest.OrgA, c.Fleet.ID, rotation.Token.ID, storetest.Ana, x.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if answer := x.ok(old, message(uid, 3, withSettings())); answer.GetConnectionSettings() != nil {
		t.Fatalf("a token was pushed to itself")
	}
}

// The ServerName header names a new collector (trimmed); an existing one
// keeps its name; none, or whitespace, gives a petname.
func TestTheServerNameHeaderNamesANewCollectorOnly(t *testing.T) {
	x := newHarness(t, testStores(t))
	c := x.fleet(storetest.OrgA, "edge-prod", fleets.TypeServer)
	uid := uuid.New()
	x.ok(rotate(c), message(uid, 1), "  prod-collector-east-1 ")
	if l := x.collector(c, uid); l.Name != "prod-collector-east-1" {
		t.Fatalf("named %q", l.Name)
	}
	x.ok(rotate(c), message(uid, 2), "agent-reported-host")
	if l := x.collector(c, uid); l.Name != "prod-collector-east-1" {
		t.Fatalf("renamed to %q", l.Name)
	}
	other := uuid.New()
	x.ok(rotate(c), message(other, 1), "   ")
	if l := x.collector(c, other); strings.TrimSpace(l.Name) == "" || !strings.Contains(l.Name, "-") {
		t.Fatalf("whitespace gave %q; want a petname", l.Name)
	}
}

// A collector naming a group of its fleet enrolls in
// it; one naming another fleet's group, or something that isn't a group's
// id, is refused 403 and enrolls nowhere.
func TestACollectorEnrollsInTheGroupItNamesOnlyWhenItIsItsFleets(t *testing.T) {
	x := newHarness(t, testStores(t))
	c := x.fleet(storetest.OrgA, "edge-prod", fleets.TypeServer)
	other := x.fleet(storetest.OrgA, "edge-dev", fleets.TypeServer)
	gateways, err := x.s.Groups.Create(t.Context(), collectorgroups.NewGroup{OrganizationID: storetest.OrgA, FleetID: c.Fleet.ID,
		Name: "gateways", At: x.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	uid := uuid.New()
	x.ok(defaultToken(c), message(uid, 1, withDescription(nil, map[string]string{"telflo.collector_group_id": gateways.ID.String()})))
	if l := x.collector(c, uid); l.GroupID != gateways.ID {
		t.Fatalf("enrolled in %s; want gateways", l.GroupID)
	}
	for _, named := range []string{other.DefaultGroupID.String(), "not-a-group-id"} {
		stranger := uuid.New()
		rec, _ := x.poll(rotate(c), message(stranger, 1, withDescription(nil, map[string]string{"telflo.collector_group_id": named})))
		if rec.Code != http.StatusForbidden || rec.Body.String() != "Forbidden\n" {
			t.Fatalf("naming %q: %d %q; want 403", named, rec.Code, rec.Body.String())
		}
		if _, found := x.findCollector(c, stranger); found {
			t.Fatalf("naming %q enrolled the collector", named)
		}
	}
}

// The cap's refusal is 403 with a fixed body saying "collector
// limit reached", naming no organization, id or count, and its log line
// none either beyond the ids.
func TestTheCapsRefusalNamesNoIdOrCount(t *testing.T) {
	x := newHarness(t, testStores(t))
	c := x.fleet(storetest.OrgA, "edge-prod", fleets.TypeServer)
	if _, err := x.s.Organizations.SetLimits(t.Context(), storetest.OrgA, organizations.Limits{MaxCollectors: storetest.Ptr(int32(1))}); err != nil {
		t.Fatal(err)
	}
	x.ok(rotate(c), message(uuid.New(), 1))
	rec, _ := x.poll(defaultToken(c), message(uuid.New(), 1))
	if rec.Code != http.StatusForbidden || rec.Body.String() != "Forbidden: collector limit reached\n" {
		t.Fatalf("%d %q; want the cap's 403", rec.Code, rec.Body.String())
	}
	lines := x.logLines()
	last := lines[len(lines)-1]
	if last["outcome"] != outcomeCollectorLimit {
		t.Fatalf("log: %v", last)
	}
	for _, leak := range []string{"max", "count", "\"1\""} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Fatalf("the body holds %q", leak)
		}
	}
}

// A cross-fleet claim: a token of another fleet of the organization
// claiming an enrolled collector's uid is refused 403, and the collector is
// untouched.
func TestACollectorOfAnotherFleetIsRefusedAndUntouched(t *testing.T) {
	x := newHarness(t, testStores(t))
	c, _ := assigned(x, gatewayYAML)
	attacker := x.fleet(storetest.OrgA, "edge-dev", fleets.TypeServer)
	uid := uuid.New()
	x.ok(rotate(c), message(uid, 1))
	before := x.collector(c, uid)
	x.clock.Advance(time.Minute)
	for _, p := range []string{"rotate", "default"} {
		principal := rotate(attacker)
		if p == "default" {
			principal = defaultToken(attacker)
		}
		rec, _ := x.poll(principal, message(uid, 2, withStatus("forged", protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, "")))
		if rec.Code != http.StatusForbidden || rec.Body.String() != "Forbidden\n" {
			t.Fatalf("%s token of another fleet: %d %q; want 403", p, rec.Code, rec.Body.String())
		}
	}
	after := x.collector(c, uid)
	if after.FleetID != c.Fleet.ID || !after.LastSeenAt.Equal(before.LastSeenAt) || after.EffectiveSHA256 != nil {
		t.Fatalf("the refused polls changed the collector: %+v", after.Collector)
	}
	if lines := x.logLines(); lines[len(lines)-1]["outcome"] != outcomeFleetMismatch {
		t.Fatalf("log %v", lines[len(lines)-1])
	}
}

// A default token's poll is asked for its full state alongside a push
// only; without one (a suspended fleet, or an agent that takes no
// connection settings) at most once every FullStateInterval per collector,
// across replicas, not on every such poll.
func TestADefaultTokenIsAskedForItsFullStateOnlyWithAPushElseEveryThreeMinutes(t *testing.T) {
	x := newHarness(t, testStores(t))
	c, _ := assigned(x, gatewayYAML)
	pushing := uuid.New()
	for seq := range uint64(3) {
		if answer := x.ok(defaultToken(c), message(pushing, seq, withSettings())); answer.GetConnectionSettings() == nil || !fullState(answer) {
			t.Fatalf("poll %d with a push: settings %v, asked %v", seq, answer.GetConnectionSettings() != nil, fullState(answer))
		}
	}
	asks := func(label string, msg func(seq uint64) *protobufs.AgentToServer) {
		t.Helper()
		asked := 0
		replica := x.replica()
		for i := range 20 {
			h := x
			if i%2 == 1 {
				h = replica
			}
			answer := h.ok(defaultToken(c), msg(uint64(i)))
			if answer.GetConnectionSettings() != nil {
				t.Fatalf("%s: pushed settings", label)
			}
			if fullState(answer) {
				asked++
			}
			x.clock.Advance(15 * time.Second)
		}
		// 20 polls over 5 minutes: asked at 0 and at 3 minutes.
		if asked != 2 {
			t.Fatalf("%s: asked %d times in 5 minutes; want 2", label, asked)
		}
	}
	plain := uuid.New()
	asks("no connection settings taken", func(seq uint64) *protobufs.AgentToServer { return message(plain, seq) })
	if _, _, err := x.s.Tokens.Revoke(t.Context(), storetest.OrgA, c.Fleet.ID, c.RotateTokenID, storetest.Ana, x.clock.Now()); err != nil {
		t.Fatal(err)
	}
	suspended := uuid.New()
	asks("a suspended fleet", func(seq uint64) *protobufs.AgentToServer { return message(suspended, seq, withSettings()) })
}

// renamingCollectors answers every Poll's collector under name: a name
// stored before ValidName refused it.
type renamingCollectors struct {
	collectors.Store
	name string
}

func (r *renamingCollectors) Poll(ctx context.Context, p collectors.PollRequest) (collectors.PollResult, error) {
	found, err := r.Store.Poll(ctx, p)
	if found.Found && r.name != "" {
		found.Collector.Name = r.name
	}
	return found, err
}

// A ServerName with a control character never names a collector (it gets
// a petname), and a name an HTTP header can't carry is never pushed back
// as its ServerName (the supervisor sends it on every request, and Go's
// HTTP client refuses such a header): the push carries the bearer alone.
func TestANameWithAControlCharacterIsNeitherTakenNorPushed(t *testing.T) {
	stub := &renamingCollectors{}
	x := newHarness(t, testStores(t), func(o *Options) { stub.Store = o.Collectors; o.Collectors = stub })
	c := x.fleet(storetest.OrgA, "edge-prod", fleets.TypeServer)
	for _, name := range []string{"edge\t01", "edge-01\x1b[31m"} {
		uid := uuid.New()
		answer := x.ok(defaultToken(c), message(uid, 0, withSettings()), name)
		l := x.collector(c, uid)
		if l.Name == name || strings.ContainsFunc(l.Name, unicode.IsControl) || settingsHeader(answer, "ServerName") != l.Name {
			t.Fatalf("ServerName %q: named %q, pushed %q", name, l.Name, settingsHeader(answer, "ServerName"))
		}
	}
	stub.name = "edge-01\nrack-7"
	answer := x.ok(defaultToken(c), message(uuid.New(), 0, withSettings()), "edge-01")
	var keys []string
	for _, h := range answer.GetConnectionSettings().GetOpamp().GetHeaders().GetHeaders() {
		keys = append(keys, h.GetKey())
	}
	if settingsHeader(answer, "Authorization") == "" || len(keys) != 1 {
		t.Fatalf("a name with a newline: pushed the headers %q; want the bearer alone", keys)
	}
}

// A collector refused for naming a group not of its fleet stays refused.
// Its supervisor sends its agent description only in its full state, so a
// later message carries none; enrolling it then into the Default group
// would undo the refusal. A collector the organization doesn't hold is
// enrolled only by a message with a description: one without is asked for
// the full state (outcome full_state_before_enrollment), records nothing,
// and the full state that answers it is checked, and refused, again. On
// either token.
func TestAForeignGroupsRefusalHoldsWhenALaterMessageCarriesNoDescription(t *testing.T) {
	x := newHarness(t, testStores(t))
	c := x.fleet(storetest.OrgA, "edge-prod", fleets.TypeServer)
	other := x.fleet(storetest.OrgA, "edge-dev", fleets.TypeServer)
	foreign := withDescription(nil, map[string]string{"telflo.collector_group_id": other.DefaultGroupID.String()})
	for name, p := range map[string]middleware.Principal{"default": defaultToken(c), "rotate": rotate(c)} {
		uid := uuid.New()
		for round := range 3 {
			if rec, _ := x.poll(p, message(uid, uint64(2*round), foreign, withSettings())); rec.Code != http.StatusForbidden {
				t.Fatalf("%s token, round %d: the foreign group answered %d; want 403", name, round, rec.Code)
			}
			answer := x.ok(p, message(uid, uint64(2*round+1), withoutDescription(), withSettings()))
			if !fullState(answer) || answer.GetConnectionSettings() != nil || answer.GetRemoteConfig() != nil {
				t.Fatalf("%s token, round %d: without a description: %v; want a bare ask for the full state", name, round, answer)
			}
			if _, found := x.findCollector(c, uid); found {
				t.Fatalf("%s token, round %d: enrolled without a description", name, round)
			}
		}
	}
}
