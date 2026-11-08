package fleets

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

// Postgres is the Store `serve` uses, on telflo_opamp.fleets (and, for
// Create, collector_groups, enrollment_tokens and enrollment_token_events),
// as telflo_opamp with its grants. Every statement names the schema, and
// every query filters on organization_id first.
//
// Errors are wrapped with the operation's name only: no SQL argument (no
// label, no email) reaches an error, a log line or a span.
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

// The constraints whose refusals are answers, and their SQLSTATEs.
const (
	organizationForeignKey = "fk_fleets_organization"
	labelUniqueKey         = "uq_fleets_organization_id_label"
	foreignKeyViolation    = "23503"
	uniqueViolation        = "23505"
)

// columns is every column of fleets, in the order scanFleet reads them.
const columns = `id, organization_id, label, type, status,
	created_by_id, created_by_email, created_at, updated_at`

// Create makes the fleet and what it starts with in one transaction. It
// needs no lock of its own before its first insert (the lock order rule):
// that insert's foreign key check key-shares the organization's row before
// the transaction holds any lock on an existing row, and the rows it inserts
// are its own (an organization's delete never waits on an uncommitted
// fleet). The fleet's insert is `on conflict do nothing`, so a second Create
// of the same id or label waits for the first's transaction, then inserts
// nothing, and a read within the organization classifies it (Unchanged,
// Conflict or LabelTaken), as organizations.Create does.
func (s *Postgres) Create(ctx context.Context, f NewFleet) (result CreateResult, c Creation, err error) {
	if err := f.validate(); err != nil {
		return 0, Creation{}, err
	}
	ctx, end := telemetry.StartDBSpan(ctx, "fleets.create")
	defer func() { end(answerOrFailure(err)) }()

	now := at(f.At)
	var rotateExpiresAt *time.Time
	if f.RotateExpiresAt != nil {
		t := at(*f.RotateExpiresAt)
		rotateExpiresAt = &t
	}
	actorID, actorEmail := actorColumns(f.CreatedBy)
	c = Creation{
		DefaultGroupID:  uuid.New(),
		DefaultTokenID:  uuid.New(),
		RotateTokenID:   uuid.New(),
		RotateExpiresAt: rotateExpiresAt,
	}

	inserted := false
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			insert into telflo_opamp.fleets (`+columns+`)
			values ($1, $2, $3, $4, 'active', $5, $6, $7, $7)
			on conflict do nothing
			returning `+columns,
			f.ID, f.OrganizationID, f.Label, string(f.Type), actorID, actorEmail, now)
		if err != nil {
			return err
		}
		fleets, err := pgx.CollectRows(rows, scanFleet)
		if err != nil || len(fleets) == 0 {
			return err
		}
		inserted, c.Fleet = true, fleets[0]

		if _, err := tx.Exec(ctx, `
			insert into telflo_opamp.collector_groups
				(id, organization_id, fleet_id, name, is_default,
				 assigned_publication_id, annotations, created_at, updated_at)
			values ($1, $2, $3, $4, true, null, null, $5, $5)`,
			c.DefaultGroupID, f.OrganizationID, f.ID, DefaultGroupName, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			insert into telflo_opamp.enrollment_tokens
				(id, organization_id, fleet_id, kind, expires_at, revoked_at,
				 do_refresh, created_by_id, created_by_email, created_at)
			values ($1, $3, $4, 'default', null, null, false, $6, $7, $8),
			       ($2, $3, $4, 'rotate', $5, null, false, $6, $7, $8)`,
			c.DefaultTokenID, c.RotateTokenID, f.OrganizationID, f.ID, rotateExpiresAt,
			actorID, actorEmail, now); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			insert into telflo_opamp.enrollment_token_events
				(id, organization_id, fleet_id, enrollment_token_id, event,
				 actor_id, actor_email, occurred_at)
			values ($1, $3, $4, $5, 'issued', $7, $8, $9),
			       ($2, $3, $4, $6, 'issued', $7, $8, $9)`,
			uuid.New(), uuid.New(), f.OrganizationID, f.ID, c.DefaultTokenID, c.RotateTokenID,
			actorID, actorEmail, now)
		return err
	})
	switch {
	case isViolation(err, foreignKeyViolation, organizationForeignKey):
		return 0, Creation{}, ErrOrganizationNotFound
	case err != nil:
		return 0, Creation{}, fmt.Errorf("fleets: create: %w", err)
	case inserted:
		return Created, c, nil
	}

	existing, err := s.get(ctx, f.OrganizationID, f.ID)
	switch {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		return 0, Creation{}, fmt.Errorf("fleets: create: %w", err)
	case existing.Label == f.Label && existing.Type == f.Type:
		return Unchanged, Creation{Fleet: existing}, nil
	default:
		return Conflict, Creation{}, nil
	}
	var labelTaken bool
	if err := s.pool.QueryRow(ctx, `
		select exists (
			select 1 from telflo_opamp.fleets
			 where organization_id = $1 and label = $2)`,
		f.OrganizationID, f.Label).Scan(&labelTaken); err != nil {
		return 0, Creation{}, fmt.Errorf("fleets: create: %w", err)
	}
	if labelTaken {
		return LabelTaken, Creation{}, nil
	}
	// The id is another organization's (or was deleted since).
	return Conflict, Creation{}, nil
}

// List returns a page of the organization's fleets, newest first, by
// keyset on (created_at, id), which idx_fleets_created_at serves.
func (s *Postgres) List(ctx context.Context, organizationID uuid.UUID, o ListOptions) (page Page, err error) {
	if err := checkOptions(o); err != nil {
		return Page{}, err
	}
	ctx, end := telemetry.StartDBSpan(ctx, "fleets.list")
	defer func() { end(err) }()

	query := `
		select ` + columns + `
		  from telflo_opamp.fleets
		 where organization_id = $1`
	// One row more than the page, to know whether another page follows.
	args := []any{organizationID, o.Limit + 1}
	if o.Status != nil {
		args = append(args, string(*o.Status))
		query += fmt.Sprintf(` and status = $%d`, len(args))
	}
	if o.After != nil {
		args = append(args, o.After.CreatedAt, o.After.ID)
		query += fmt.Sprintf(` and (created_at, id) < ($%d, $%d)`, len(args)-1, len(args))
	}
	query += ` order by created_at desc, id desc limit $2`

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return Page{}, fmt.Errorf("fleets: list: %w", err)
	}
	found, err := pgx.CollectRows(rows, scanFleet)
	if err != nil {
		return Page{}, fmt.Errorf("fleets: list: %w", err)
	}
	page = Page{Fleets: found}
	if len(found) > o.Limit {
		page.Fleets = found[:o.Limit]
		page.Next = cursorOf(found[o.Limit-1])
	}
	if page.Fleets == nil {
		page.Fleets = []Fleet{}
	}
	return page, nil
}

// Get returns the organization's fleet id, or ErrNotFound.
func (s *Postgres) Get(ctx context.Context, organizationID, id uuid.UUID) (f Fleet, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "fleets.get")
	defer func() { end(answerOrFailure(err)) }()
	f, err = s.get(ctx, organizationID, id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Fleet{}, fmt.Errorf("fleets: get: %w", err)
	}
	return f, err
}

func (s *Postgres) get(ctx context.Context, organizationID, id uuid.UUID) (Fleet, error) {
	rows, err := s.pool.Query(ctx, `
		select `+columns+`
		  from telflo_opamp.fleets
		 where organization_id = $1 and id = $2`,
		organizationID, id)
	if err != nil {
		return Fleet{}, err
	}
	f, err := pgx.CollectExactlyOneRow(rows, scanFleet)
	if errors.Is(err, pgx.ErrNoRows) {
		return Fleet{}, ErrNotFound
	}
	return f, err
}

// Update applies changes in one statement; updated_at moves only when the
// label or the status changes. The statement matches nothing when another of
// the organization's fleets has the new label: a label taken is found
// without raising the unique key's violation, whose DETAIL Postgres would
// write to its log with the label in it, and a read then tells a fleet not
// found from a label taken. Only a race with another's write of the label,
// after the statement's snapshot, reaches the unique key, still
// ErrLabelTaken.
func (s *Postgres) Update(ctx context.Context, organizationID, id uuid.UUID, changes Changes, now time.Time) (f Fleet, err error) {
	if err := changes.validate(); err != nil {
		return Fleet{}, err
	}
	ctx, end := telemetry.StartDBSpan(ctx, "fleets.update")
	defer func() { end(answerOrFailure(err)) }()
	var status *string
	if changes.Status != nil {
		v := string(*changes.Status)
		status = &v
	}
	rows, err := s.pool.Query(ctx, `
		update telflo_opamp.fleets
		   set label = coalesce($3, label),
		       status = coalesce($4, status),
		       updated_at = case
		         when label is distinct from coalesce($3, label)
		           or status is distinct from coalesce($4, status) then $5
		         else updated_at end
		 where organization_id = $1 and id = $2
		   and ($3::text is null or not exists (
		         select 1
		           from telflo_opamp.fleets x
		          where x.organization_id = $1 and x.label = $3::text and x.id <> $2))
		returning `+columns,
		organizationID, id, changes.Label, status, at(now))
	if err != nil {
		return Fleet{}, fmt.Errorf("fleets: update: %w", err)
	}
	f, err = pgx.CollectExactlyOneRow(rows, scanFleet)
	switch {
	case errors.Is(err, pgx.ErrNoRows) && changes.Label != nil:
		switch _, err := s.get(ctx, organizationID, id); {
		case errors.Is(err, ErrNotFound):
			return Fleet{}, ErrNotFound
		case err != nil:
			return Fleet{}, fmt.Errorf("fleets: update: %w", err)
		}
		return Fleet{}, ErrLabelTaken
	case errors.Is(err, pgx.ErrNoRows):
		return Fleet{}, ErrNotFound
	case isViolation(err, uniqueViolation, labelUniqueKey):
		return Fleet{}, ErrLabelTaken
	case err != nil:
		return Fleet{}, fmt.Errorf("fleets: update: %w", err)
	}
	return f, nil
}

// Delete removes the organization's fleet id; the foreign keys' on delete
// cascade removes its groups, tokens, token events and collectors in the
// same statement. In one transaction it first takes the organization's row
// `for key share` (the lock order rule), so it never deadlocks with the
// organization's delete, which cascades into the same rows; then the fleet's
// row `for update`, then the fleet's collectors' rows `for update` in id
// order, and only then deletes: a poll locks a collector's row and then
// key-shares the token its changed key names, and a cascade that deleted the
// fleet's tokens before it reached the collector deadlocked with it (40P01).
// Holding the collectors first, the delete waits for the poll, or the poll
// for the delete. A 40P01 is still retried, at most DeadlockRetries times.
func (s *Postgres) Delete(ctx context.Context, organizationID, id uuid.UUID) (deleted bool, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "fleets.delete")
	defer func() { end(err) }()
	for attempt := 0; ; attempt++ {
		deleted, err = s.delete(ctx, organizationID, id)
		if !isDeadlock(err) || attempt >= s.deadlockRetries {
			break
		}
	}
	if err != nil {
		return false, fmt.Errorf("fleets: delete: %w", err)
	}
	return deleted, nil
}

func (s *Postgres) delete(ctx context.Context, organizationID, id uuid.UUID) (deleted bool, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if found, err := lockOrganization(ctx, tx, organizationID); err != nil || !found {
			return err
		}
		err := tx.QueryRow(ctx, `
			select 1
			  from telflo_opamp.fleets
			 where organization_id = $1 and id = $2
			   for update`,
			organizationID, id).Scan(new(int))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		if _, err := tx.Exec(ctx, `
			select 1
			  from telflo_opamp.collector_instances
			 where organization_id = $1 and fleet_id = $2
			 order by id
			   for update`,
			organizationID, id); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			delete from telflo_opamp.fleets
			 where organization_id = $1 and id = $2`,
			organizationID, id)
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

// lockOrganization takes the organization's row `for key share`, the first
// lock of a transaction that then locks, inserts into or deletes the
// organization's fleet model (the lock order rule: organization, fleet,
// group, then collector or token): an organization's delete, which holds its
// row `for update` and cascades into everything, queues behind it, or it
// behind the delete. It reports whether the organization is registered.
func lockOrganization(ctx context.Context, tx pgx.Tx, organizationID uuid.UUID) (bool, error) {
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
	return true, nil
}

// TypeAndStatus returns the fleet's type and status.
func (s *Postgres) TypeAndStatus(ctx context.Context, organizationID, id uuid.UUID) (t Type, st Status, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "fleets.type_and_status")
	defer func() { end(answerOrFailure(err)) }()
	err = s.pool.QueryRow(ctx, `
		select type, status
		  from telflo_opamp.fleets
		 where organization_id = $1 and id = $2`,
		organizationID, id).Scan(&t, &st)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", "", ErrNotFound
	case err != nil:
		return "", "", fmt.Errorf("fleets: type and status: %w", err)
	}
	return t, st, nil
}

// actorColumns is an actor as its two nullable columns.
func actorColumns(a *Actor) (*uuid.UUID, *string) {
	if a == nil {
		return nil, nil
	}
	id, email := a.ID, a.Email
	return &id, &email
}

// isViolation reports whether err is Postgres refusing with code on the
// constraint named.
func isViolation(err error, code, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code && pgErr.ConstraintName == constraint
}

// answerOrFailure is what a span's end is given: the store's answers are
// not failures.
func answerOrFailure(err error) error {
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrLabelTaken) ||
		errors.Is(err, ErrOrganizationNotFound) || errors.Is(err, ErrInvalid) {
		return nil
	}
	return err
}

// scanFleet reads one row of columns, with its times in UTC.
func scanFleet(row pgx.CollectableRow) (Fleet, error) {
	var (
		f          Fleet
		actorID    *uuid.UUID
		actorEmail *string
	)
	if err := row.Scan(&f.ID, &f.OrganizationID, &f.Label, &f.Type, &f.Status,
		&actorID, &actorEmail, &f.CreatedAt, &f.UpdatedAt); err != nil {
		return Fleet{}, err
	}
	if actorID != nil && actorEmail != nil {
		f.CreatedBy = &Actor{ID: *actorID, Email: *actorEmail}
	}
	f.CreatedAt = f.CreatedAt.UTC()
	f.UpdatedAt = f.UpdatedAt.UTC()
	return f, nil
}
