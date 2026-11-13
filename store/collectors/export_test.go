package collectors

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WantedStampsForFleets answers the wanted stamp of each of the
// organization's fleets' collectors computed over the fleets' rows, the
// source summariesStatement classifies (fleetsWanted): the parity test's
// third source, beside Get's and List's.
func WantedStampsForFleets(ctx context.Context, pool *pgxpool.Pool, organizationID uuid.UUID, fleetIDs []uuid.UUID) (map[uuid.UUID]int64, error) {
	rows, err := pool.Query(ctx, `
		with `+fleetsWanted.combos+`
		select c.id,`+fleetsWanted.stamp+`
		  from opamp_server.collector_instances c`+resolutionJoins("c")+fleetsWanted.join+`
		 where c.organization_id = $1 and c.fleet_id = any ($2)`,
		organizationID, fleetIDs)
	if err != nil {
		return nil, err
	}
	stamps := map[uuid.UUID]int64{}
	var (
		id    uuid.UUID
		stamp int64
	)
	_, err = pgx.ForEachRow(rows, []any{&id, &stamp}, func() error {
		stamps[id] = stamp
		return nil
	})
	return stamps, err
}
