package handlers

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"

	"github.com/telflo/telflo-opamp-server/store/collectors"
	"github.com/telflo/telflo-opamp-server/store/fleets"
	"github.com/telflo/telflo-opamp-server/store/organizations"
	"github.com/telflo/telflo-opamp-server/store/storetest"
	"github.com/telflo/telflo-opamp-server/tokens"
)

// The poll's main flows end to end, through the authentication, on the
// Postgres stores as the server's role: a collector enrolls with the
// default bearer, is handed the rotate token, is served its group's
// version on it and reports it applied (in sync), then fails it (an error
// row); a collector of a fleet with nothing assigned, reporting its own
// file, is sent nothing and reads unassigned; a revoked rotate token
// suspends the hand-off; the cap and another fleet's token are refused
// 403.
func TestTheMainFlowsEndToEnd(t *testing.T) {
	storetest.Each(t, testDatabase, func(t *testing.T, s *storetest.Stores) {
		x := newHarness(t, s)
		c, pub := assigned(x, gatewayYAML)
		uid := uuid.New()
		defaultBearer, err := x.keyring.Mint(tokens.Claims{TokenID: c.DefaultTokenID, FleetID: c.Fleet.ID})
		if err != nil {
			t.Fatal(err)
		}

		rec, answer := x.pollBearer(defaultBearer, message(uid, 0, withSettings(), withGroup(c.DefaultGroupID)))
		if rec.Code != http.StatusOK || answer.GetRemoteConfig() != nil || !fullState(answer) {
			t.Fatalf("the default bearer: %d %v", rec.Code, answer)
		}
		claims, rotated := x.pushedClaims(answer)
		if claims.TokenID != c.RotateTokenID || claims.FleetID != c.Fleet.ID {
			t.Fatalf("pushed %+v", claims)
		}
		rec, answer = x.pollBearer(rotated, message(uid, 0, withSettings()))
		if rec.Code != http.StatusOK || pushed(t, answer) != gatewayYAML {
			t.Fatalf("the rotate bearer: %d, push %q", rec.Code, pushed(t, answer))
		}
		x.clock.Advance(5 * time.Second)
		x.pollBearer(rotated, message(uid, 1, withStatus(gatewayYAML, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, "")))
		l := x.collector(c, uid)
		if l.Served == nil || l.Served.ID != pub || l.Sync() != collectors.InSync || *l.EnrollmentTokenID != c.RotateTokenID {
			t.Fatalf("after APPLIED: %+v sync %s", l.Collector, l.Sync())
		}
		x.clock.Advance(5 * time.Second)
		x.pollBearer(rotated, message(uid, 2, withStatus(gatewayYAML, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, "")))
		if rows := x.errors(c, l.ID); len(rows) != 1 || rows[0].Message != emptyFailureMessage {
			t.Fatalf("error rows: %+v", rows)
		}

		// A collector with nothing assigned, on its own file: sent nothing,
		// unassigned (the sync's SQL on Postgres), nothing pinned or
		// served.
		local := x.fleet(storetest.OrgA, "edge-local", fleets.TypeServer)
		id := uuid.New()
		for seq := range uint64(3) {
			if answer := x.ok(rotate(local), message(id, seq, withEffective(localYAML, ""))); answer.GetRemoteConfig() != nil {
				t.Fatalf("sequence %d: an unassigned collector was sent a configuration", seq)
			}
		}
		if l := x.collector(local, id); l.Pinned != nil || l.Served != nil || l.ServedSHA256 != nil ||
			l.Resolution.From != collectors.FromNone || l.Sync() != collectors.Unassigned {
			t.Fatalf("unassigned: %+v resolution %+v sync %s", l.Collector, l.Resolution, l.Sync())
		}

		// Another fleet's token claiming the collector: 403.
		if rec, _ := x.poll(rotate(local), message(uid, 3)); rec.Code != http.StatusForbidden {
			t.Fatalf("another fleet's token: %d", rec.Code)
		}
		// The rotate token revoked: the hand-off pushes nothing.
		if _, _, err := s.Tokens.Revoke(t.Context(), storetest.OrgA, c.Fleet.ID, c.RotateTokenID, storetest.Ana, x.clock.Now()); err != nil {
			t.Fatal(err)
		}
		if _, answer := x.pollBearer(defaultBearer, message(uuid.New(), 0, withSettings(), withGroup(c.DefaultGroupID))); answer.GetConnectionSettings() != nil {
			t.Fatalf("a suspended fleet pushed settings")
		}
		// The cap.
		if _, err := s.Organizations.SetLimits(t.Context(), storetest.OrgA, organizations.Limits{MaxCollectors: storetest.Ptr(int32(1))}); err != nil {
			t.Fatal(err)
		}
		if rec, _ := x.pollBearer(defaultBearer, message(uuid.New(), 0, withGroup(c.DefaultGroupID))); rec.Code != http.StatusForbidden ||
			rec.Body.String() != "Forbidden: collector limit reached\n" {
			t.Fatalf("the cap: %d %q", rec.Code, rec.Body.String())
		}
	})
}
