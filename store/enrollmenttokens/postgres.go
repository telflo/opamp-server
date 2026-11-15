package enrollmenttokens

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/telflo/opamp-server/store/fleets"
	"github.com/telflo/opamp-server/telemetry"
)

// Postgres is the Store `serve` uses, on opamp_server.enrollment_tokens and
// enrollment_token_events (and fleets, for Rotate's lock and Lookup's
// join; collector_instances, for List's counts), as telflo_opamp_server with its
// grants: tokens are never deleted but with their fleet, and of a token
// only expires_at, revoked_at and do_refresh change. Every statement names
// the schema, and every query but Lookup's filters on organization_id
// first.
//
// Errors are wrapped with the operation's name only: no SQL argument (no
// email) reaches an error, a log line or a span.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres returns the store on pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool}
}

var _ Store = (*Postgres)(nil)

// columns is every column of enrollment_tokens, in the order scanToken
// reads them.
const columns = `id, organization_id, fleet_id, kind, expires_at, revoked_at,
	do_refresh, created_by_id, created_by_email, created_at`

// prefixed is columns qualified by the alias t.
const prefixed = `t.id, t.organization_id, t.fleet_id, t.kind, t.expires_at, t.revoked_at,
	t.do_refresh, t.created_by_id, t.created_by_email, t.created_at`

// currentOrder is the order of a fleet's valid rotate tokens, the current
// one first (current, in enrollmenttokens.go).
const currentOrder = `order by do_refresh, created_at desc, id desc`

// List returns the fleet's tokens with their active collectors, counted by
// idx_collector_instances_fleet_id_enrollment_token_id. A fleet always has
// its two first tokens (none is ever deleted but with it), so an empty
// answer means the fleet isn't the organization's, which a second read
// confirms.
func (s *Postgres) List(ctx context.Context, organizationID, fleetID uuid.UUID, now time.Time) (listed []Listed, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "enrollment_tokens.list")
	defer func() { end(answerOrFailure(err)) }()
	rows, err := s.pool.Query(ctx, `
		select `+prefixed+`,
		       (select count(*)
		          from opamp_server.collector_instances c
		         where c.organization_id = t.organization_id and c.fleet_id = t.fleet_id
		           and c.enrollment_token_id = t.id and c.last_seen_at > $3)
		  from opamp_server.enrollment_tokens t
		 where t.organization_id = $1 and t.fleet_id = $2
		 order by t.created_at desc, t.id desc`,
		organizationID, fleetID, at(now).Add(-ActiveWindow))
	if err != nil {
		return nil, fmt.Errorf("enrollmenttokens: list: %w", err)
	}
	listed, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Listed, error) {
		var l Listed
		var count int64
		token, err := scanTokenThen(row, &count)
		l.Token, l.ActiveCollectors = token, int(count)
		return l, err
	})
	if err != nil {
		return nil, fmt.Errorf("enrollmenttokens: list: %w", err)
	}
	if len(listed) == 0 {
		if err := s.fleetExists(ctx, organizationID, fleetID); err != nil {
			return nil, err
		}
		return []Listed{}, nil
	}
	return listed, nil
}

// fleetExists answers nil for the organization's fleet, else
// ErrFleetNotFound.
func (s *Postgres) fleetExists(ctx context.Context, organizationID, fleetID uuid.UUID) error {
	var exists bool
	if err := s.pool.QueryRow(ctx, `
		select exists (
			select 1 from opamp_server.fleets
			 where organization_id = $1 and id = $2)`,
		organizationID, fleetID).Scan(&exists); err != nil {
		return fmt.Errorf("enrollmenttokens: fleet: %w", err)
	}
	if !exists {
		return ErrFleetNotFound
	}
	return nil
}

// Get returns the fleet's token id, or ErrNotFound.
func (s *Postgres) Get(ctx context.Context, organizationID, fleetID, id uuid.UUID) (t Token, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "enrollment_tokens.get")
	defer func() { end(answerOrFailure(err)) }()
	t, err = s.get(ctx, s.pool, organizationID, fleetID, id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Token{}, fmt.Errorf("enrollmenttokens: get: %w", err)
	}
	return t, err
}

type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func (s *Postgres) get(ctx context.Context, q queryer, organizationID, fleetID, id uuid.UUID) (Token, error) {
	rows, err := q.Query(ctx, `
		select `+columns+`
		  from opamp_server.enrollment_tokens
		 where organization_id = $1 and fleet_id = $2 and id = $3`,
		organizationID, fleetID, id)
	if err != nil {
		return Token{}, err
	}
	t, err := pgx.CollectExactlyOneRow(rows, scanToken)
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	return t, err
}

// Rotate runs in one transaction, after the organization's row `for key
// share`. The predecessor's expiry becomes the earlier of its own and the
// grace end (least ignores a null, "never"): a rotation never lengthens a
// credential's life. Its first statement takes the fleet's row `for no key
// update`, which two rotations of one fleet can't both hold (a collector's
// or a token's key-share lock on the fleet isn't blocked by it): the second
// waits for the first to commit, then reads the first's new token as the
// current one. Without the lock both would put the same predecessor in its
// grace and leave two current tokens. The update's own where clause repeats
// `revoked_at is null`: a revoke racing the rotation holds the current
// token's row, the update waits for it and then re-checks only its own where
// clause against the row the revoke left; without the repeat it put the
// token just revoked in a grace as the predecessor. With it the update
// matches nothing and the rotation answers as when no rotate token is valid.
func (s *Postgres) Rotate(ctx context.Context, r Rotation) (result RotateResult, err error) {
	if err := r.validate(); err != nil {
		return RotateResult{}, err
	}
	ctx, end := telemetry.StartDBSpan(ctx, "enrollment_tokens.rotate")
	defer func() { end(answerOrFailure(err)) }()
	now := at(r.At)
	var expiresAt *time.Time
	if r.ExpiresAt != nil {
		t := at(*r.ExpiresAt)
		expiresAt = &t
	}
	actorID, actorEmail := actorColumns(r.Actor)

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// The organization's row first (the lock order rule): the token's
		// insert and the events' key-share it at their foreign keys' checks,
		// after the fleet, and an organization's delete holding it and
		// cascading into the fleet deadlocked with that (40P01).
		if found, err := lockOrganization(ctx, tx, r.OrganizationID); err != nil {
			return err
		} else if !found {
			return ErrFleetNotFound
		}
		var status fleets.Status
		err := tx.QueryRow(ctx, `
			select status
			  from opamp_server.fleets
			 where organization_id = $1 and id = $2
			   for no key update`,
			r.OrganizationID, r.FleetID).Scan(&status)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrFleetNotFound
		case err != nil:
			return err
		case status != fleets.StatusActive:
			return ErrFleetArchived
		}

		var predecessor uuid.UUID
		err = tx.QueryRow(ctx, `
			update opamp_server.enrollment_tokens
			   set expires_at = least(expires_at, $4::timestamptz), do_refresh = true
			 where organization_id = $1 and fleet_id = $2 and revoked_at is null and id = (
			         select id
			           from opamp_server.enrollment_tokens
			          where organization_id = $1 and fleet_id = $2 and kind = 'rotate'
			            and revoked_at is null and (expires_at is null or expires_at > $3)
			          `+currentOrder+`
			          limit 1)
			returning id`,
			r.OrganizationID, r.FleetID, now, at(r.GraceEndsAt)).Scan(&predecessor)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		default:
			result.PredecessorID = &predecessor
		}

		rows, err := tx.Query(ctx, `
			insert into opamp_server.enrollment_tokens (`+columns+`)
			values ($1, $2, $3, 'rotate', $4, null, false, $5, $6, $7)
			returning `+columns,
			uuid.New(), r.OrganizationID, r.FleetID, expiresAt, actorID, actorEmail, now)
		if err != nil {
			return err
		}
		if result.Token, err = pgx.CollectExactlyOneRow(rows, scanToken); err != nil {
			return err
		}

		if result.PredecessorID != nil {
			if err := insertEvent(ctx, tx, r.OrganizationID, r.FleetID, *result.PredecessorID, EventRotated, actorID, actorEmail, now); err != nil {
				return err
			}
		}
		return insertEvent(ctx, tx, r.OrganizationID, r.FleetID, result.Token.ID, EventIssued, actorID, actorEmail, now)
	})
	if err != nil {
		return RotateResult{}, wrap("rotate", err)
	}
	return result, nil
}

// IssueDefault runs in one transaction: the organization's row `for key
// share`, then the fleet's `for no key update` (as Rotate: two issues of one
// fleet, and an archive, take turns), the fleet active, then the insert `on
// conflict` the partial unique index `do nothing`, so a fleet that has a
// valid default token inserts nothing and answers ErrDefaultExists without
// the index's violation (whose DETAIL Postgres would log). A revoke of the
// valid default token racing the issue holds the token's row: the insert's
// conflict check waits for it, and once it commits the revoked row is out of
// the index and the insert goes in.
func (s *Postgres) IssueDefault(ctx context.Context, organizationID, fleetID uuid.UUID, actor *fleets.Actor, now time.Time) (t Token, err error) {
	if !fleets.ValidActor(actor) {
		return Token{}, invalid("actor")
	}
	ctx, end := telemetry.StartDBSpan(ctx, "enrollment_tokens.issue_default")
	defer func() { end(answerOrFailure(err)) }()
	issuedAt := at(now)
	actorID, actorEmail := actorColumns(actor)
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if found, err := lockOrganization(ctx, tx, organizationID); err != nil {
			return err
		} else if !found {
			return ErrFleetNotFound
		}
		var status fleets.Status
		err := tx.QueryRow(ctx, `
			select status
			  from opamp_server.fleets
			 where organization_id = $1 and id = $2
			   for no key update`,
			organizationID, fleetID).Scan(&status)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrFleetNotFound
		case err != nil:
			return err
		case status != fleets.StatusActive:
			return ErrFleetArchived
		}
		rows, err := tx.Query(ctx, `
			insert into opamp_server.enrollment_tokens (`+columns+`)
			values ($1, $2, $3, 'default', null, null, false, $4, $5, $6)
			on conflict (organization_id, fleet_id) where kind = 'default' and revoked_at is null do nothing
			returning `+columns,
			uuid.New(), organizationID, fleetID, actorID, actorEmail, issuedAt)
		if err != nil {
			return err
		}
		issued, err := pgx.CollectRows(rows, scanToken)
		if err != nil {
			return err
		}
		if len(issued) == 0 {
			return ErrDefaultExists
		}
		t = issued[0]
		return insertEvent(ctx, tx, organizationID, fleetID, t.ID, EventIssued, actorID, actorEmail, issuedAt)
	})
	if err != nil {
		return Token{}, wrap("issue default", err)
	}
	return t, nil
}

func insertEvent(ctx context.Context, tx pgx.Tx, organizationID, fleetID, tokenID uuid.UUID, kind EventKind,
	actorID *uuid.UUID, actorEmail *string, now time.Time) error {
	_, err := tx.Exec(ctx, `
		insert into opamp_server.enrollment_token_events
			(id, organization_id, fleet_id, enrollment_token_id, event, actor_id, actor_email, occurred_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8)`,
		uuid.New(), organizationID, fleetID, tokenID, string(kind), actorID, actorEmail, now)
	return err
}

// Revoke sets revoked_at only while it is null, so two revokes can't both
// revoke: the second waits for the first's row lock, then finds it set. It
// first takes the organization's row and the fleet's `for key share`
// (lockFleet): a fleet's delete locks the fleet and then cascades through
// its tokens, and a revoke that locked the token first, then key-shared the
// fleet for its event, deadlocked with it (40P01).
func (s *Postgres) Revoke(ctx context.Context, organizationID, fleetID, id uuid.UUID, actor *fleets.Actor, now time.Time) (result RevokeResult, t Token, err error) {
	if !fleets.ValidActor(actor) {
		return 0, Token{}, invalid("actor")
	}
	ctx, end := telemetry.StartDBSpan(ctx, "enrollment_tokens.revoke")
	defer func() { end(answerOrFailure(err)) }()
	actorID, actorEmail := actorColumns(actor)
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockFleet(ctx, tx, organizationID, fleetID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			update opamp_server.enrollment_tokens
			   set revoked_at = $4
			 where organization_id = $1 and fleet_id = $2 and id = $3 and revoked_at is null
			returning `+columns,
			organizationID, fleetID, id, at(now))
		if err != nil {
			return err
		}
		revoked, err := pgx.CollectRows(rows, scanToken)
		if err != nil {
			return err
		}
		if len(revoked) == 0 {
			result = AlreadyRevoked
			t, err = s.get(ctx, tx, organizationID, fleetID, id)
			return err
		}
		result, t = Revoked, revoked[0]
		return insertEvent(ctx, tx, organizationID, fleetID, id, EventRevoked, actorID, actorEmail, at(now))
	})
	if err != nil {
		return 0, Token{}, wrap("revoke", err)
	}
	return result, t, nil
}

// RecordViewed reads the token and, when it is valid at now, writes the
// event, in one statement, after taking the organization's row and the
// fleet's `for key share` in the same transaction (lockFleet): a fleet or
// organization deleted meanwhile is waited for and then answers ErrNotFound,
// never a foreign key's failure or a deadlock.
func (s *Postgres) RecordViewed(ctx context.Context, organizationID, fleetID, id uuid.UUID, actor *fleets.Actor, now time.Time) (t Token, err error) {
	if !fleets.ValidActor(actor) {
		return Token{}, invalid("actor")
	}
	ctx, end := telemetry.StartDBSpan(ctx, "enrollment_tokens.record_viewed")
	defer func() { end(answerOrFailure(err)) }()
	actorID, actorEmail := actorColumns(actor)
	var written int64
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockFleet(ctx, tx, organizationID, fleetID); err != nil {
			return err
		}
		t, err = s.recordViewed(ctx, tx, organizationID, fleetID, id, actorID, actorEmail, now, &written)
		return err
	})
	switch {
	case errors.Is(err, ErrNotFound):
		return Token{}, ErrNotFound
	case err != nil:
		return Token{}, fmt.Errorf("enrollmenttokens: record viewed: %w", err)
	case written == 1:
		return t, nil
	case t.RevokedAt != nil:
		return Token{}, ErrRevoked
	default:
		return Token{}, ErrExpired
	}
}

// recordViewed is RecordViewed's statement: the token, and the `viewed`
// event when the token is valid at now (written is then 1).
func (s *Postgres) recordViewed(ctx context.Context, tx pgx.Tx, organizationID, fleetID, id uuid.UUID,
	actorID *uuid.UUID, actorEmail *string, now time.Time, written *int64) (Token, error) {
	rows, err := tx.Query(ctx, `
		with t as (
			select `+columns+`
			  from opamp_server.enrollment_tokens
			 where organization_id = $1 and fleet_id = $2 and id = $3),
		viewed as (
			insert into opamp_server.enrollment_token_events
				(id, organization_id, fleet_id, enrollment_token_id, event, actor_id, actor_email, occurred_at)
			select $4, organization_id, fleet_id, id, 'viewed', $5, $6, $7
			  from t
			 where revoked_at is null and (expires_at is null or expires_at > $7)
			returning 1)
		select `+prefixed+`, (select count(*) from viewed)
		  from t`,
		organizationID, fleetID, id, uuid.New(), actorID, actorEmail, at(now))
	if err != nil {
		return Token{}, err
	}
	t, err := pgx.CollectExactlyOneRow(rows, func(row pgx.CollectableRow) (Token, error) {
		return scanTokenThen(row, written)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	return t, err
}

// lockFleet takes the organization's row `for key share`, then its fleet's
// `for key share`: the first locks of a transaction that then locks or links
// rows the fleet owns (the lock order rule: organization, fleet, then its
// rows). ErrNotFound when the organization holds no such fleet (so no such
// token either).
func lockFleet(ctx context.Context, tx pgx.Tx, organizationID, fleetID uuid.UUID) error {
	if found, err := lockOrganization(ctx, tx, organizationID); err != nil {
		return err
	} else if !found {
		return ErrNotFound
	}
	err := tx.QueryRow(ctx, `
		select 1
		  from opamp_server.fleets
		 where organization_id = $1 and id = $2
		   for key share`,
		organizationID, fleetID).Scan(new(int))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// lockOrganization takes the organization's row `for key share`: an
// organization's delete (which takes it `for update`) waits for the
// transaction, or the transaction for the delete; it reports whether the
// organization is registered.
func lockOrganization(ctx context.Context, tx pgx.Tx, organizationID uuid.UUID) (bool, error) {
	err := tx.QueryRow(ctx, `
		select 1
		  from opamp_server.organizations
		 where id = $1
		   for key share`,
		organizationID).Scan(new(int))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// ListEvents returns a page of the fleet's events, newest first, by keyset
// on (occurred_at, id) along idx_enrollment_token_events_fleet_id_occurred_at.
// A fleet always has its two first `issued` events (none is ever deleted
// but with it), so only an empty page asks whether the fleet is the
// organization's.
func (s *Postgres) ListEvents(ctx context.Context, organizationID, fleetID uuid.UUID, limit int, after *EventCursor) (page EventPage, err error) {
	if limit < 1 || limit > MaxListLimit {
		return EventPage{}, invalid("limit")
	}
	ctx, end := telemetry.StartDBSpan(ctx, "enrollment_token_events.list")
	defer func() { end(answerOrFailure(err)) }()
	query := `
		select id, organization_id, fleet_id, enrollment_token_id, event, actor_id, actor_email, occurred_at
		  from opamp_server.enrollment_token_events
		 where organization_id = $1 and fleet_id = $2`
	args := []any{organizationID, fleetID, limit + 1}
	if after != nil {
		args = append(args, after.OccurredAt, after.ID)
		query += ` and (occurred_at, id) < ($4, $5)`
	}
	query += ` order by occurred_at desc, id desc limit $3`
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return EventPage{}, fmt.Errorf("enrollmenttokens: list events: %w", err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var (
			e          Event
			actorID    *uuid.UUID
			actorEmail *string
		)
		err := row.Scan(&e.ID, &e.OrganizationID, &e.FleetID, &e.TokenID, &e.Kind, &actorID, &actorEmail, &e.OccurredAt)
		e.Actor = actorOf(actorID, actorEmail)
		e.OccurredAt = e.OccurredAt.UTC()
		return e, err
	})
	if err != nil {
		return EventPage{}, fmt.Errorf("enrollmenttokens: list events: %w", err)
	}
	if len(found) == 0 {
		if err := s.fleetExists(ctx, organizationID, fleetID); err != nil {
			return EventPage{}, err
		}
		return EventPage{Events: []Event{}}, nil
	}
	page = EventPage{Events: found}
	if len(found) > limit {
		page.Events = found[:limit]
		last := found[limit-1]
		page.Next = &EventCursor{OccurredAt: last.OccurredAt, ID: last.ID}
	}
	return page, nil
}

// GetActiveRotate returns the fleet's current valid rotate token.
func (s *Postgres) GetActiveRotate(ctx context.Context, organizationID, fleetID uuid.UUID, now time.Time) (t Token, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "enrollment_tokens.get_active_rotate")
	defer func() { end(answerOrFailure(err)) }()
	rows, err := s.pool.Query(ctx, `
		select `+columns+`
		  from opamp_server.enrollment_tokens
		 where organization_id = $1 and fleet_id = $2 and kind = 'rotate'
		   and revoked_at is null and (expires_at is null or expires_at > $3)
		 `+currentOrder+`
		 limit 1`,
		organizationID, fleetID, at(now))
	if err != nil {
		return Token{}, fmt.Errorf("enrollmenttokens: get active rotate: %w", err)
	}
	t, err = pgx.CollectExactlyOneRow(rows, scanToken)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Token{}, ErrNotFound
	case err != nil:
		return Token{}, fmt.Errorf("enrollmenttokens: get active rotate: %w", err)
	}
	return t, nil
}

// rotateStatesStatement is RotateStates' one grouped query: each fleet's
// valid rotate tokens, the current one first by currentOrder (as
// GetActiveRotate and Rotate pick it), counted; the current one's expiry;
// and the latest expiry among those in their grace but the current one. It
// reads by the indexes leading with organization_id, at the caller's clock.
//
//	$1 organization, $2 the fleets, $3 now
const rotateStatesStatement = `
	select fleet_id, count(*),
	       (array_agg(expires_at ` + currentOrder + `))[1],
	       max(expires_at) filter (where do_refresh and not is_current)
	  from (select fleet_id, expires_at, do_refresh, created_at, id,
	               row_number() over (partition by fleet_id ` + currentOrder + `) = 1 as is_current
	          from opamp_server.enrollment_tokens
	         where organization_id = $1 and fleet_id = any ($2) and kind = 'rotate'
	           and revoked_at is null and (expires_at is null or expires_at > $3)) v
	 group by fleet_id`

// RotateStates runs rotateStatesStatement.
func (s *Postgres) RotateStates(ctx context.Context, organizationID uuid.UUID, fleetIDs []uuid.UUID, now time.Time) (states map[uuid.UUID]RotateState, err error) {
	if len(fleetIDs) > fleets.MaxListLimit {
		return nil, invalid("fleet ids")
	}
	states = map[uuid.UUID]RotateState{}
	if len(fleetIDs) == 0 {
		return states, nil
	}
	ctx, end := telemetry.StartDBSpan(ctx, "enrollment_tokens.rotate_states")
	defer func() { end(answerOrFailure(err)) }()
	rows, err := s.pool.Query(ctx, rotateStatesStatement, organizationID, fleetIDs, at(now))
	if err != nil {
		return nil, fmt.Errorf("enrollmenttokens: rotate states: %w", err)
	}
	var (
		id                     uuid.UUID
		count                  int
		expiresAt, graceEndsAt *time.Time
	)
	_, err = pgx.ForEachRow(rows, []any{&id, &count, &expiresAt, &graceEndsAt}, func() error {
		states[id] = RotateState{ExpiresAt: utcCopy(expiresAt), GraceEndsAt: utcCopy(graceEndsAt), ValidCount: count}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("enrollmenttokens: rotate states: %w", err)
	}
	return states, nil
}

// utcCopy is a copy of t in UTC, sharing nothing with it, or nil.
func utcCopy(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.UTC()
	return &v
}

// Lookup returns the token id with its fleet's type and status.
//
// THE EXCEPTION to CONTRIBUTING.md's "organization_id is every query's first
// filter": this query is led by the token's id alone, the primary key,
// because a bearer names no organization: the agent learns the organization
// from this row. It reads one row by a unique key, so it can't reach a
// second organization's rows, and the caller then holds the bearer's fleet
// to the row's. It is the only such query in the stores.
func (s *Postgres) Lookup(ctx context.Context, id uuid.UUID) (a Authenticated, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "enrollment_tokens.lookup")
	defer func() { end(answerOrFailure(err)) }()
	rows, err := s.pool.Query(ctx, `
		select `+prefixed+`, f.type, f.status
		  from opamp_server.enrollment_tokens t
		  join opamp_server.fleets f
		    on f.organization_id = t.organization_id and f.id = t.fleet_id
		 where t.id = $1`,
		id)
	if err != nil {
		return Authenticated{}, fmt.Errorf("enrollmenttokens: lookup: %w", err)
	}
	a, err = pgx.CollectExactlyOneRow(rows, func(row pgx.CollectableRow) (Authenticated, error) {
		var a Authenticated
		token, err := scanTokenThen(row, &a.FleetType, &a.FleetStatus)
		a.Token = token
		return a, err
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Authenticated{}, ErrNotFound
	case err != nil:
		return Authenticated{}, fmt.Errorf("enrollmenttokens: lookup: %w", err)
	}
	return a, nil
}

// wrap returns an answer as it is, and wraps any other error with the
// operation's name.
func wrap(operation string, err error) error {
	if isAnswer(err) {
		return err
	}
	return fmt.Errorf("enrollmenttokens: %s: %w", operation, err)
}

func isAnswer(err error) bool {
	for _, answer := range []error{ErrNotFound, ErrFleetNotFound, ErrFleetArchived, ErrDefaultExists, ErrRevoked, ErrExpired, ErrInvalid} {
		if errors.Is(err, answer) {
			return true
		}
	}
	return false
}

// answerOrFailure is what a span's end is given: the store's answers are
// not failures.
func answerOrFailure(err error) error {
	if isAnswer(err) {
		return nil
	}
	return err
}

func scanToken(row pgx.CollectableRow) (Token, error) {
	return scanTokenThen(row)
}

// scanTokenThen reads one row of columns, then the trailing values given,
// with its times in UTC.
func scanTokenThen(row pgx.CollectableRow, trailing ...any) (Token, error) {
	var (
		t          Token
		actorID    *uuid.UUID
		actorEmail *string
	)
	dest := append([]any{&t.ID, &t.OrganizationID, &t.FleetID, &t.Kind, &t.ExpiresAt, &t.RevokedAt,
		&t.DoRefresh, &actorID, &actorEmail, &t.CreatedAt}, trailing...)
	if err := row.Scan(dest...); err != nil {
		return Token{}, err
	}
	t.CreatedBy = actorOf(actorID, actorEmail)
	t.CreatedAt = t.CreatedAt.UTC()
	if t.ExpiresAt != nil {
		v := t.ExpiresAt.UTC()
		t.ExpiresAt = &v
	}
	if t.RevokedAt != nil {
		v := t.RevokedAt.UTC()
		t.RevokedAt = &v
	}
	return t, nil
}
