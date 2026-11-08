package organizations

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/telflo/opamp-server/telemetry"
)

// Postgres is the Store `serve` uses, on telflo_opamp.organizations, as
// telflo_opamp (select, insert, delete, and update on name and updated_at
// only). Every statement names the schema, and every query filters on the
// organization's id first.
//
// Errors are wrapped with the operation's name only: no SQL argument (no
// name) reaches an error, a log line or a span.
type Postgres struct {
	pool            *pgxpool.Pool
	deadlockRetries int
}

// DeadlockRetries is how many times Delete runs again after Postgres refuses
// it 40P01: a backstop, since Delete's locks leave no known cycle.
const DeadlockRetries = 3

// Option configures a store.
type Option func(*Postgres)

// WithDeadlockRetries makes Delete run again at most n times after a 40P01
// in place of DeadlockRetries: zero for the tests that show Delete's own
// locks leave no cycle.
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

// columns is every column, in the order scanOrganization reads them.
const columns = `id, name, created_at, updated_at`

// Create inserts o in one statement, `insert … on conflict (id) do nothing`,
// so two Creates of the same new id can't both insert or fail: the second
// waits for the first's transaction, then inserts nothing. Inserting nothing
// means the id is taken, and a read classifies it: the same name is
// Unchanged; another name, or none (deleted since), is a Conflict, as a
// publication's Put answers. The insert names the variables' counter, at 0:
// its column default is only for the rows that existed when migration 00012
// added it; and a new random generation, which has no default.
func (s *Postgres) Create(ctx context.Context, o Organization) (result CreateResult, stored Organization, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "organizations.create")
	defer func() { end(err) }()
	o = o.normalized()

	rows, err := s.pool.Query(ctx, `
		insert into telflo_opamp.organizations (`+columns+`, variables_revision, generation)
		values ($1, $2, $3, $4, 0, $5)
		on conflict (id) do nothing
		returning `+columns,
		o.ID, o.Name, o.CreatedAt, o.UpdatedAt, uuid.New())
	if err != nil {
		return 0, Organization{}, fmt.Errorf("organizations: create: %w", err)
	}
	inserted, err := pgx.CollectRows(rows, scanOrganization)
	if err != nil {
		return 0, Organization{}, fmt.Errorf("organizations: create: %w", err)
	}
	if len(inserted) == 1 {
		return Created, inserted[0], nil
	}

	existing, err := s.get(ctx, o.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return Conflict, Organization{}, nil
	case err != nil:
		return 0, Organization{}, fmt.Errorf("organizations: create: %w", err)
	case existing.Name == o.Name:
		return Unchanged, existing, nil
	default:
		return Conflict, Organization{}, nil
	}
}

// Get returns the organization id, or ErrNotFound.
func (s *Postgres) Get(ctx context.Context, id uuid.UUID) (o Organization, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "organizations.get")
	defer func() { end(answerOrFailure(err)) }()
	o, err = s.get(ctx, id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Organization{}, fmt.Errorf("organizations: get: %w", err)
	}
	return o, err
}

func (s *Postgres) get(ctx context.Context, id uuid.UUID) (Organization, error) {
	rows, err := s.pool.Query(ctx, `
		select `+columns+`
		  from telflo_opamp.organizations
		 where id = $1`,
		id)
	if err != nil {
		return Organization{}, err
	}
	o, err := pgx.CollectExactlyOneRow(rows, scanOrganization)
	if errors.Is(err, pgx.ErrNoRows) {
		return Organization{}, ErrNotFound
	}
	return o, err
}

// Exists reports whether the organization id is registered.
func (s *Postgres) Exists(ctx context.Context, id uuid.UUID) (exists bool, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "organizations.exists")
	defer func() { end(err) }()
	if err := s.pool.QueryRow(ctx, `
		select exists (select 1 from telflo_opamp.organizations where id = $1)`,
		id).Scan(&exists); err != nil {
		return false, fmt.Errorf("organizations: exists: %w", err)
	}
	return exists, nil
}

// Rename sets the organization's name in one statement. updated_at moves
// only when the name changes, so renaming to the same name is a no-op.
func (s *Postgres) Rename(ctx context.Context, id uuid.UUID, name string, at time.Time) (o Organization, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "organizations.rename")
	defer func() { end(answerOrFailure(err)) }()
	rows, err := s.pool.Query(ctx, `
		update telflo_opamp.organizations
		   set name = $2,
		       updated_at = case when name = $2 then updated_at else $3 end
		 where id = $1
		returning `+columns,
		id, name, at.UTC().Truncate(Precision))
	if err != nil {
		return Organization{}, fmt.Errorf("organizations: rename: %w", err)
	}
	o, err = pgx.CollectExactlyOneRow(rows, scanOrganization)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Organization{}, ErrNotFound
	case err != nil:
		return Organization{}, fmt.Errorf("organizations: rename: %w", err)
	}
	return o, nil
}

// Delete removes the organization; every table's foreign key's on delete
// cascade removes what it owns (its publications and its whole fleet model)
// in the same statement. In one transaction it first takes the
// organization's row `for update`, then its collectors' rows `for update` in
// id order, and only then deletes: a poll or a report locks a collector's
// row and then key-shares the token or published version its changed key
// names, and a cascade that deleted those before it reached the collector
// deadlocked with it (40P01). Holding the collectors first, the delete waits
// for such a statement to finish, or it for the delete. A 40P01 is still
// retried, at most DeadlockRetries times.
func (s *Postgres) Delete(ctx context.Context, id uuid.UUID) (deleted bool, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "organizations.delete")
	defer func() { end(err) }()
	for attempt := 0; ; attempt++ {
		deleted, err = s.delete(ctx, id)
		if !isDeadlock(err) || attempt >= s.deadlockRetries {
			break
		}
	}
	if err != nil {
		return false, fmt.Errorf("organizations: delete: %w", err)
	}
	return deleted, nil
}

func (s *Postgres) delete(ctx context.Context, id uuid.UUID) (deleted bool, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			select 1
			  from telflo_opamp.organizations
			 where id = $1
			   for update`,
			id).Scan(new(int))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		if _, err := tx.Exec(ctx, `
			select 1
			  from telflo_opamp.collector_instances
			 where organization_id = $1
			 order by id
			   for update`,
			id); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			delete from telflo_opamp.organizations
			 where id = $1`,
			id)
		deleted = tag.RowsAffected() == 1
		return err
	})
	return deleted, err
}

// isDeadlock reports whether err is Postgres's refusal 40P01.
func isDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

// Limits returns the organization's limits.
func (s *Postgres) Limits(ctx context.Context, id uuid.UUID) (l Limits, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "organizations.limits")
	defer func() { end(answerOrFailure(err)) }()
	err = s.pool.QueryRow(ctx, `
		select max_collectors, requests_per_minute
		  from telflo_opamp.organizations
		 where id = $1`,
		id).Scan(&l.MaxCollectors, &l.RequestsPerMinute)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Limits{}, ErrNotFound
	case err != nil:
		return Limits{}, fmt.Errorf("organizations: limits: %w", err)
	}
	return l, nil
}

// SetLimits sets both limits in one statement, on the two columns the
// runtime role may update.
func (s *Postgres) SetLimits(ctx context.Context, id uuid.UUID, limits Limits) (l Limits, err error) {
	if !limits.valid() {
		return Limits{}, ErrInvalidLimits
	}
	ctx, end := telemetry.StartDBSpan(ctx, "organizations.set_limits")
	defer func() { end(answerOrFailure(err)) }()
	err = s.pool.QueryRow(ctx, `
		update telflo_opamp.organizations
		   set max_collectors = $2,
		       requests_per_minute = $3
		 where id = $1
		returning max_collectors, requests_per_minute`,
		id, limits.MaxCollectors, limits.RequestsPerMinute).Scan(&l.MaxCollectors, &l.RequestsPerMinute)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Limits{}, ErrNotFound
	case err != nil:
		return Limits{}, fmt.Errorf("organizations: set limits: %w", err)
	}
	return l, nil
}

// answerOrFailure is what a span's end is given: ErrNotFound is an answer,
// not a failure.
func answerOrFailure(err error) error {
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// scanOrganization reads one row of columns, with its times in UTC.
func scanOrganization(row pgx.CollectableRow) (Organization, error) {
	var o Organization
	if err := row.Scan(&o.ID, &o.Name, &o.CreatedAt, &o.UpdatedAt); err != nil {
		return Organization{}, err
	}
	o.CreatedAt = o.CreatedAt.UTC()
	o.UpdatedAt = o.UpdatedAt.UTC()
	return o, nil
}
