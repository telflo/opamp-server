package collectors_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/telflo/telflo-opamp-server/store/collectorgroups"
	"github.com/telflo/telflo-opamp-server/store/collectors"
	"github.com/telflo/telflo-opamp-server/store/fleets"
	. "github.com/telflo/telflo-opamp-server/store/storetest"
)

// refusals records the SQLSTATE of every statement Postgres refused on a
// pool: a unique key's violation (23505) is what writes a taken name into
// Postgres's own log (its DETAIL quotes the value).
type refusals struct {
	mu    sync.Mutex
	codes []string
}

func (r *refusals) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (r *refusals) TraceQueryEnd(_ context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	var pgErr *pgconn.PgError
	if errors.As(data.Err, &pgErr) {
		r.mu.Lock()
		r.codes = append(r.codes, pgErr.Code)
		r.mu.Unlock()
	}
}

func (r *refusals) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.codes...)
}

// tracedPool is the runtime role's pool again, every statement's refusal
// recorded.
func tracedPool(t *testing.T, s *Stores) (*pgxpool.Pool, *refusals) {
	t.Helper()
	r := &refusals{}
	config := s.Databases.Runtime.Config()
	config.ConnConfig.Tracer = r
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, r
}

// A fleet's label, a group's name or a collector's name already taken is
// answered taken without Postgres refusing a statement: its unique key's
// violation would write the name, in its DETAIL, to Postgres's own server
// log. A relabel, a group's create and rename, and a collector's rename and
// update, each to a name taken, are refused, change nothing, and no
// statement of theirs is refused (on Postgres, a pool records every
// refusal).
func TestATakenNameIsRefusedWithoutAUniqueViolation(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		prod := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		dev := s.CreateFleet(t, OrgA, "edge-dev", fleets.TypeServer)
		pool, traced := tracedPool(t, s)
		fleetStore, groups, store := fleets.NewPostgres(pool), collectorgroups.NewPostgres(pool), collectors.NewPostgres(pool)
		gateways, err := groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: prod.Fleet.ID, Name: "gateways", At: Now})
		if err != nil {
			t.Fatal(err)
		}
		agents, err := groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: prod.Fleet.ID, Name: "agents", At: Now})
		if err != nil {
			t.Fatal(err)
		}
		enroll(t, s, prod, "edge-01", Now)
		second := enroll(t, s, dev, "edge-02", Now)
		before := s.Dump(t, OrgA)

		if _, err := fleetStore.Update(ctx, OrgA, dev.Fleet.ID, fleets.Changes{Label: Ptr("edge-prod")}, Now); !errors.Is(err, fleets.ErrLabelTaken) {
			t.Errorf("a relabel to a label taken = %v, want ErrLabelTaken", err)
		}
		if _, err := fleetStore.Update(ctx, OrgA, uuid.New(), fleets.Changes{Label: Ptr("edge-prod")}, Now); !errors.Is(err, fleets.ErrNotFound) {
			t.Errorf("an unknown fleet's relabel to a label taken = %v, want ErrNotFound", err)
		}
		if _, err := groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: prod.Fleet.ID, Name: "gateways", At: Now}); !errors.Is(err, collectorgroups.ErrNameTaken) {
			t.Errorf("a group created with a name taken = %v, want ErrNameTaken", err)
		}
		if _, err := groups.Update(ctx, OrgA, prod.Fleet.ID, agents.ID, collectorgroups.Changes{Name: Ptr(gateways.Name)}, Now); !errors.Is(err, collectorgroups.ErrNameTaken) {
			t.Errorf("a group renamed to a name taken = %v, want ErrNameTaken", err)
		}
		if _, err := store.Rename(ctx, OrgA, dev.Fleet.ID, second.ID, "edge-01", Now); !errors.Is(err, collectors.ErrNameTaken) {
			t.Errorf("a collector renamed to a name taken = %v, want ErrNameTaken", err)
		}
		if _, err := store.Rename(ctx, OrgA, dev.Fleet.ID, uuid.New(), "edge-01", Now); !errors.Is(err, collectors.ErrNotFound) {
			t.Errorf("an unknown collector renamed to a name taken = %v, want ErrNotFound", err)
		}
		if _, err := store.Update(ctx, OrgA, dev.Fleet.ID, second.ID, collectors.Changes{Name: Ptr("edge-01")}, Now); !errors.Is(err, collectors.ErrNameTaken) {
			t.Errorf("a collector updated to a name taken = %v, want ErrNameTaken", err)
		}
		s.Unchanged(t, "the refusals", OrgA, before)
		if traced != nil {
			if codes := traced.seen(); len(codes) != 0 {
				t.Fatalf("Postgres refused statements %v; want none (a unique key's violation logs the name)", codes)
			}
		}

		// The names free, each change goes through, still with no refusal.
		if _, err := fleetStore.Update(ctx, OrgA, dev.Fleet.ID, fleets.Changes{Label: Ptr("edge-staging")}, Now); err != nil {
			t.Errorf("a relabel: %v", err)
		}
		if _, err := groups.Update(ctx, OrgA, prod.Fleet.ID, agents.ID, collectorgroups.Changes{Name: Ptr("nodes")}, Now); err != nil {
			t.Errorf("a group's rename: %v", err)
		}
		if _, err := store.Rename(ctx, OrgA, dev.Fleet.ID, second.ID, "edge-03", Now); err != nil {
			t.Errorf("a collector's rename: %v", err)
		}
		if _, err := store.Update(ctx, OrgA, dev.Fleet.ID, second.ID, collectors.Changes{Name: Ptr("edge-04")}, Now); err != nil {
			t.Errorf("a collector's update: %v", err)
		}
		if traced != nil {
			if codes := traced.seen(); len(codes) != 0 {
				t.Fatalf("Postgres refused statements %v", codes)
			}
		}
	})
}

// Only a true race reaches the unique key, still answered as a name taken.
// Two collectors renamed to one free name at once: on Postgres a third
// session holds both rows, so both renames read the name free before either
// writes it; released, one renames and the other is ErrNameTaken.
func TestTwoRenamesToOneFreeNameAtOnceAreOneRenameAndOneTaken(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		first := enroll(t, s, c, "edge-01", Now)
		second := enroll(t, s, c, "edge-02", Now)
		release := blockOn(t, s, 2, `select 1 from telflo_opamp.collector_instances
			where organization_id = $1 and id in ($2, $3) for update`, OrgA, first.ID, second.ID)
		errs := both(release,
			func() error {
				_, err := s.Collectors.Rename(ctx, OrgA, c.Fleet.ID, first.ID, "edge-99", Now)
				return err
			},
			func() error {
				_, err := s.Collectors.Rename(ctx, OrgA, c.Fleet.ID, second.ID, "edge-99", Now)
				return err
			})
		taken := 0
		for _, err := range errs {
			switch {
			case errors.Is(err, collectors.ErrNameTaken):
				taken++
			case err != nil:
				t.Fatalf("the renames = %v", errs)
			}
		}
		if taken != 1 {
			t.Fatalf("the renames = %v; want one renamed, one ErrNameTaken", errs)
		}
	})
}
