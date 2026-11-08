package collectorgroups

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/telflo/opamp-server/store/variableuse"
	"github.com/telflo/opamp-server/telemetry"
)

// Postgres is the Store `serve` uses, on telflo_opamp.collector_groups (and
// collector_instances, which a group's Delete deletes), as telflo_opamp with its
// grants. Every statement names the schema, and every query filters on
// organization_id first.
//
// Errors are wrapped with the operation's name only: no SQL argument (no
// name, no annotations) reaches an error, a log line or a span.
type Postgres struct {
	pool            *pgxpool.Pool
	deadlockRetries int
}

// DeadlockRetries is how many times Delete runs again after Postgres refuses
// it 40P01, as the fleets' and organizations' deletes do: a group's delete
// cascades through its collectors' error history, whose cleanup
// (collectors.Store.Cleanup) locks rows in another order.
const DeadlockRetries = 3

// Option configures a store.
type Option func(*Postgres)

// WithDeadlockRetries makes Delete run again at most n times after a 40P01
// in place of DeadlockRetries: zero for the tests that show the cycle.
func WithDeadlockRetries(n int) Option {
	return func(s *Postgres) { s.deadlockRetries = n }
}

// NewPostgres returns the store on pool.
func NewPostgres(pool *pgxpool.Pool, opts ...Option) *Postgres {
	s := &Postgres{pool: pool, deadlockRetries: DeadlockRetries}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

var _ Store = (*Postgres)(nil)

// The constraints whose refusals are answers, and their SQLSTATEs.
const (
	nameUniqueKey           = "uq_collector_groups_organization_id_fleet_id_name"
	fleetForeignKey         = "fk_collector_groups_fleet"
	publicationForeignKey   = "fk_collector_groups_assigned_publication"
	variableGroupForeignKey = "fk_collector_groups_variable_group"
	annotationsCheck        = "ck_collector_groups_annotations_bounded"
	foreignKeyViolation     = "23503"
	uniqueViolation         = "23505"
	checkViolation          = "23514"
)

// columns is every column, in the order scanGroup reads them; the
// annotations as their text, which scanGroup puts in canonical form.
const columns = `id, organization_id, fleet_id, name, is_default,
	assigned_publication_id, annotations::text, variable_group_id, variable_group_stamp, created_at, updated_at`

// Create takes the organization's row (`for no key update` when the group
// names a published version or a variable group, else `for key share`:
// chosen before the first lock) and the fleet's `for key share` (the lock
// order rule); checks the published version and the variable group are the
// organization's, and the published version's names defined; moves the
// counter on for a variable group (its stamp); and inserts the group, in one
// transaction. The insert is `on conflict` the name's unique key `do
// nothing`: a name taken inserts nothing, ErrNameTaken, never the unique
// key's violation, whose DETAIL Postgres would write to its log with the
// name in it; a racing create of the name is waited for, then inserts
// nothing too. A refusal rolls the counter's move back with the rest.
func (s *Postgres) Create(ctx context.Context, g NewGroup) (group Group, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "collector_groups.create")
	defer func() { end(answerOrFailure(err)) }()
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		found, err := lockFleet(ctx, tx, g.OrganizationID, g.FleetID, g.locksOrganization())
		if err != nil {
			return err
		}
		if !found {
			return ErrFleetNotFound
		}
		row, err := g.prepare()
		if err != nil {
			return err
		}
		stamp, err := checkAndStamp(ctx, tx, Group{OrganizationID: g.OrganizationID}, row)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			insert into telflo_opamp.collector_groups
				(id, organization_id, fleet_id, name, is_default,
				 assigned_publication_id, annotations, variable_group_id, variable_group_stamp, created_at, updated_at)
			values ($1, $2, $3, $4, false, $5, $6::jsonb, $8, $9, $7, $7)
			on conflict on constraint `+nameUniqueKey+` do nothing
			returning `+columns,
			uuid.New(), row.OrganizationID, row.FleetID, row.Name,
			row.AssignedPublicationID, jsonColumn(row.Annotations), row.CreatedAt, row.VariableGroupID, stamp)
		if err != nil {
			return err
		}
		group, err = pgx.CollectExactlyOneRow(rows, scanGroup)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNameTaken
		}
		return err
	})
	if err != nil {
		return Group{}, s.wrap("create", err)
	}
	return group, nil
}

// fleetExists returns nil when the organization holds the fleet, or
// ErrFleetNotFound.
func (s *Postgres) fleetExists(ctx context.Context, organizationID, fleetID uuid.UUID) error {
	err := s.pool.QueryRow(ctx, `
		select 1
		  from telflo_opamp.fleets
		 where organization_id = $1 and id = $2`,
		organizationID, fleetID).Scan(new(int))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrFleetNotFound
	case err != nil:
		return fmt.Errorf("collectorgroups: fleet exists: %w", err)
	}
	return nil
}

// List returns the fleet's groups. Every fleet has its Default group, so an
// empty answer means the fleet isn't the organization's, which a second
// read confirms.
func (s *Postgres) List(ctx context.Context, organizationID, fleetID uuid.UUID) (groups []Group, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "collector_groups.list")
	defer func() { end(answerOrFailure(err)) }()
	rows, err := s.pool.Query(ctx, `
		select `+columns+`
		  from telflo_opamp.collector_groups
		 where organization_id = $1 and fleet_id = $2
		 order by is_default desc, name collate "C", id`,
		organizationID, fleetID)
	if err != nil {
		return nil, fmt.Errorf("collectorgroups: list: %w", err)
	}
	groups, err = pgx.CollectRows(rows, scanGroup)
	if err != nil {
		return nil, fmt.Errorf("collectorgroups: list: %w", err)
	}
	if len(groups) == 0 {
		if err := s.fleetExists(ctx, organizationID, fleetID); err != nil {
			return nil, err
		}
		return []Group{}, nil
	}
	return groups, nil
}

// Get returns the fleet's group id, or ErrNotFound.
func (s *Postgres) Get(ctx context.Context, organizationID, fleetID, id uuid.UUID) (g Group, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "collector_groups.get")
	defer func() { end(answerOrFailure(err)) }()
	rows, err := s.pool.Query(ctx, `
		select `+columns+`
		  from telflo_opamp.collector_groups
		 where organization_id = $1 and fleet_id = $2 and id = $3`,
		organizationID, fleetID, id)
	if err != nil {
		return Group{}, fmt.Errorf("collectorgroups: get: %w", err)
	}
	g, err = pgx.CollectExactlyOneRow(rows, scanGroup)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Group{}, ErrNotFound
	case err != nil:
		return Group{}, fmt.Errorf("collectorgroups: get: %w", err)
	}
	return g, nil
}

// Update locks the group's row, applies changes to it in Go (apply), and
// writes the result only when something changed, in one transaction: a new
// published version and a new variable group checked to be the
// organization's, the new published version's names defined, and a variable
// group set, changed or cleared stamped with the counter's next value. The
// organization's row is taken first, `for no key update` when the request
// names a published version or a variable group (Changes.locksOrganization,
// chosen before the first lock and never upgraded), else `for key share`.
// Its update matches nothing when another of the fleet's groups has the new
// name: a name taken is found without raising the unique key's violation,
// whose DETAIL Postgres would log with the name; only a race with another's
// write of the name, after the statement's snapshot, reaches the unique key,
// still ErrNameTaken.
func (s *Postgres) Update(ctx context.Context, organizationID, fleetID, id uuid.UUID, changes Changes, now time.Time) (g Group, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "collector_groups.update")
	defer func() { end(answerOrFailure(err)) }()
	// A rename changes a key (the unique name), so it takes the row `for
	// update`; any other change takes it `for no key update`, which doesn't
	// block an enrollment's or a move's key-share lock on the group.
	lock := "for no key update"
	if changes.Name != nil {
		lock = "for update"
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// The organization and the fleet first (the lock order rule).
		found, err := lockFleet(ctx, tx, organizationID, fleetID, changes.locksOrganization())
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		rows, err := tx.Query(ctx, `
			select `+columns+`
			  from telflo_opamp.collector_groups
			 where organization_id = $1 and fleet_id = $2 and id = $3
			   `+lock,
			organizationID, fleetID, id)
		if err != nil {
			return err
		}
		current, err := pgx.CollectExactlyOneRow(rows, scanGroup)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		next, changed, err := changes.apply(current, now)
		if err != nil || !changed {
			g = current
			return err
		}
		stamp, err := checkAndStamp(ctx, tx, current, next)
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `
			update telflo_opamp.collector_groups
			   set name = $4, assigned_publication_id = $5, annotations = $6::jsonb, updated_at = $7,
			       variable_group_id = $8, variable_group_stamp = $9
			 where organization_id = $1 and fleet_id = $2 and id = $3
			   and not exists (
			         select 1
			           from telflo_opamp.collector_groups x
			          where x.organization_id = $1 and x.fleet_id = $2 and x.name = $4 and x.id <> $3)
			returning `+columns,
			organizationID, fleetID, id, next.Name, next.AssignedPublicationID, jsonColumn(next.Annotations),
			next.UpdatedAt, next.VariableGroupID, stamp)
		if err != nil {
			return err
		}
		g, err = pgx.CollectExactlyOneRow(rows, scanGroup)
		if errors.Is(err, pgx.ErrNoRows) {
			// The group's row is held: the name is taken.
			return ErrNameTaken
		}
		return err
	})
	if err != nil {
		return Group{}, s.wrap("update", err)
	}
	return g, nil
}

// Delete locks the group's row, deletes its collectors (their error history
// by its cascade) and then the group, in one transaction. Locking the row
// first means a collector enrolling into the group, or moved into it,
// meanwhile either finishes first (and is deleted with it) or finds the
// group gone (ErrForeignGroup, ErrGroupNotFound).
//
// Before the group, it takes the organization's row and its fleet's `for key
// share` (the lock order rule): a fleet's delete locks the fleet first and
// then cascades through its groups, so a group's delete that locked the
// group first deadlocked with it (40P01). Fleet first, the two queue on the
// fleet's row: a group delete that comes second finds the group gone. After
// the group, it locks the group's collectors `for update` in id order (as
// BulkPin locks them) before deleting them, so the two can't deadlock. The
// collectors' key to their group stays NO ACTION, not a cascade: a cascade
// would lock them in heap order. A 40P01 is still retried, at most
// DeadlockRetries times.
func (s *Postgres) Delete(ctx context.Context, organizationID, fleetID, id uuid.UUID) (deleted bool, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "collector_groups.delete")
	defer func() { end(answerOrFailure(err)) }()
	for attempt := 0; ; attempt++ {
		deleted, err = s.delete(ctx, organizationID, fleetID, id)
		if !isDeadlock(err) || attempt >= s.deadlockRetries {
			break
		}
	}
	if err != nil {
		return false, s.wrap("delete", err)
	}
	return deleted, nil
}

// delete is one attempt of Delete, in one transaction.
func (s *Postgres) delete(ctx context.Context, organizationID, fleetID, id uuid.UUID) (deleted bool, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if found, err := lockFleet(ctx, tx, organizationID, fleetID, false); err != nil || !found {
			return err
		}
		var isDefault bool
		err := tx.QueryRow(ctx, `
			select is_default
			  from telflo_opamp.collector_groups
			 where organization_id = $1 and fleet_id = $2 and id = $3
			   for update`,
			organizationID, fleetID, id).Scan(&isDefault)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil
		case err != nil:
			return err
		case isDefault:
			return ErrIsDefault
		}
		if _, err := tx.Exec(ctx, `
			select 1
			  from telflo_opamp.collector_instances
			 where organization_id = $1 and fleet_id = $2 and collector_group_id = $3
			 order by id
			   for update`,
			organizationID, fleetID, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			delete from telflo_opamp.collector_instances
			 where organization_id = $1 and fleet_id = $2 and collector_group_id = $3`,
			organizationID, fleetID, id); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			delete from telflo_opamp.collector_groups
			 where organization_id = $1 and fleet_id = $2 and id = $3`,
			organizationID, fleetID, id)
		deleted = tag.RowsAffected() == 1
		return err
	})
	return deleted && err == nil, err
}

// lockFleet takes the organization's row, then its fleet's row `for key
// share`: the first locks of a transaction that then locks, inserts into or
// deletes rows the fleet owns (the lock order rule: organization, fleet,
// group, collector). An organization's delete holds its row and cascades
// into everything it holds; a transaction that took the fleet first and the
// organization only at a foreign key's check deadlocked with it (40P01). The
// organization's row is taken `for no key update`
// (variableuse.LockOrganization) when binding, for a write that names a
// published version or a variable group: it then serializes with the
// variable writes, whose guards read what such a write changes; else `for
// key share`. The caller chooses binding from the request, before this first
// lock, and never takes the row again in a stronger mode (an upgrade
// deadlocks with an enrollment and a variable write). It reports whether the
// organization holds the fleet.
func lockFleet(ctx context.Context, tx pgx.Tx, organizationID, fleetID uuid.UUID, binding bool) (bool, error) {
	if binding {
		found, err := variableuse.LockOrganization(ctx, tx, organizationID)
		if err != nil || !found {
			return false, err
		}
	} else {
		err := tx.QueryRow(ctx, `
			select 1
			  from telflo_opamp.organizations
			 where id = $1
			   for key share`,
			organizationID).Scan(new(int))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return false, nil
		case err != nil:
			return false, err
		}
	}
	err := tx.QueryRow(ctx, `
		select 1
		  from telflo_opamp.fleets
		 where organization_id = $1 and id = $2
		   for key share`,
		organizationID, fleetID).Scan(new(int))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// checkAndStamp makes the checks of what next newly names, against current
// (a Create's current is the zero group of its organization), in this
// order: a new published version is the organization's
// (ErrPublicationNotFound), a new variable group is
// (ErrVariableGroupNotFound), and the new published version's names are
// defined (*UndefinedError); then it answers the stamp next is written
// with: the counter moved on (variableuse.Bump) when the variable group is
// set, changed or cleared, else current's. The caller holds the
// organization's row `for no key update` whenever any of these applies
// (locksOrganization); the variable groups' deletes take it too, so what
// it read stands until the transaction ends.
func checkAndStamp(ctx context.Context, tx pgx.Tx, current, next Group) (*int64, error) {
	organizationID := current.OrganizationID
	publication := newPublication(current, next)
	if publication != nil {
		if err := exists(ctx, tx, `
			select exists (
			  select 1
			    from telflo_opamp.publications
			   where organization_id = $1 and id = $2)`, organizationID, *publication, ErrPublicationNotFound); err != nil {
			return nil, err
		}
	}
	if variableGroup := newVariableGroup(current, next); variableGroup != nil {
		if err := exists(ctx, tx, `
			select exists (
			  select 1
			    from telflo_opamp.variable_groups
			   where organization_id = $1 and id = $2)`, organizationID, *variableGroup, ErrVariableGroupNotFound); err != nil {
			return nil, err
		}
	}
	if publication != nil {
		names, err := variableuse.Undefined(ctx, tx, organizationID, *publication)
		if err != nil {
			return nil, err
		}
		if err := undefined(names); err != nil {
			return nil, err
		}
	}
	if !variableGroupChanged(current, next) {
		return current.VariableGroupStamp, nil
	}
	stamp, err := variableuse.Bump(ctx, tx, organizationID)
	if errors.Is(err, variableuse.ErrOrganizationNotFound) {
		// Unreachable: the organization's row is held.
		return nil, ErrFleetNotFound
	}
	if err != nil {
		return nil, err
	}
	return &stamp, nil
}

// exists runs query, a `select exists` of the organization's row id, and
// answers missing when it is false.
func exists(ctx context.Context, tx pgx.Tx, query string, organizationID, id uuid.UUID, missing error) error {
	var found bool
	if err := tx.QueryRow(ctx, query, organizationID, id).Scan(&found); err != nil {
		return err
	}
	if !found {
		return missing
	}
	return nil
}

// wrap maps a refusal that is an answer to its error, and wraps any other
// error with the operation's name.
func (s *Postgres) wrap(operation string, err error) error {
	var pgErr *pgconn.PgError
	switch {
	case isAnswer(err):
		return err
	case !errors.As(err, &pgErr):
	case pgErr.Code == uniqueViolation && pgErr.ConstraintName == nameUniqueKey:
		return ErrNameTaken
	case pgErr.Code == foreignKeyViolation && pgErr.ConstraintName == publicationForeignKey:
		return ErrPublicationNotFound
	case pgErr.Code == foreignKeyViolation && pgErr.ConstraintName == fleetForeignKey:
		return ErrFleetNotFound
	case pgErr.Code == foreignKeyViolation && pgErr.ConstraintName == variableGroupForeignKey:
		// A backstop: the variable group is checked under the organization's
		// row, which its delete takes too.
		return ErrVariableGroupNotFound
	case pgErr.Code == checkViolation && pgErr.ConstraintName == annotationsCheck:
		// CanonicalAnnotations bounds jsonb's own text, as this check does,
		// so this answers only what that let through.
		return invalid("annotations")
	}
	return fmt.Errorf("collectorgroups: %s: %w", operation, err)
}

func isAnswer(err error) bool {
	for _, answer := range []error{ErrNotFound, ErrFleetNotFound, ErrNameTaken, ErrPublicationNotFound,
		ErrIsDefault, ErrInvalid, ErrVariableGroupNotFound, ErrVariablesUndefined} {
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

// isDeadlock reports whether err is Postgres's refusal 40P01.
func isDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

// jsonColumn is a canonical JSON value (the annotations) as the text
// a jsonb parameter takes, or nil for null.
func jsonColumn(v json.RawMessage) *string {
	if v == nil {
		return nil
	}
	s := string(v)
	return &s
}

// scanGroup reads one row of columns.
func scanGroup(row pgx.CollectableRow) (Group, error) {
	return scanGroupAfter(row)
}

// scanGroupAfter reads one row of columns after the leading values given,
// with its times in UTC and its annotations in canonical form.
func scanGroupAfter(row pgx.CollectableRow, leading ...any) (Group, error) {
	var (
		g           Group
		annotations *string
	)
	dest := append(leading, &g.ID, &g.OrganizationID, &g.FleetID, &g.Name, &g.IsDefault,
		&g.AssignedPublicationID, &annotations, &g.VariableGroupID, &g.VariableGroupStamp, &g.CreatedAt, &g.UpdatedAt)
	if err := row.Scan(dest...); err != nil {
		return Group{}, err
	}
	if annotations != nil {
		// Without the write's bound: a row the table admitted reads back.
		canonical, err := storedAnnotations(json.RawMessage(*annotations))
		if err != nil {
			return Group{}, errors.New("a stored group's annotations don't read back")
		}
		g.Annotations = canonical
	}
	g.CreatedAt = g.CreatedAt.UTC()
	g.UpdatedAt = g.UpdatedAt.UTC()
	return g, nil
}
