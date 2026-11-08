package apikeys

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/telflo/opamp-server/telemetry"
)

// Postgres is the Store `serve` uses, on telflo_opamp.api_keys and
// api_key_organizations, as telflo_opamp (select and insert on both, and
// update on a key's name, description, revoked_at and last_used_at only).
// Every statement names the schema.
//
// Errors are wrapped with the operation's name only: no SQL argument (no
// name, no hash) reaches an error, a log line or a span.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres returns the store on pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool}
}

var _ Store = (*Postgres)(nil)

// createLock is the transaction-level advisory lock Create holds while it
// counts the valid keys and inserts, so two Creates take turns and can't
// both pass the count.
const createLock = "telflo_opamp.api_keys create"

// columns is every column of a key as k, in the order scanKey reads them,
// its organizations last (an empty array for a key that opens every
// organization).
const columns = `k.id, k.name, k.description, k.secret_sha256, k.all_organizations,
	k.expires_at, k.revoked_at, k.last_used_at, k.created_at,
	array(select o.organization_id
	        from telflo_opamp.api_key_organizations o
	       where o.api_key_id = k.id
	       order by o.organization_id)`

// Create counts the valid keys and inserts the key and its organizations
// in one transaction, under createLock. A foreign key's refusal of an
// organization (23503) is ErrOrganizationNotFound; an organization deleted
// while it runs is one too, since its insert key-shares the organization's
// row.
func (s *Postgres) Create(ctx context.Context, k NewKey) (stored Key, err error) {
	if !k.valid() {
		return Key{}, ErrInvalid
	}
	ctx, end := telemetry.StartDBSpan(ctx, "api_keys.create")
	defer func() { end(answerOrFailure(err)) }()
	k.ExpiresAt = k.ExpiresAt.UTC().Truncate(Precision)
	k.CreatedAt = k.CreatedAt.UTC().Truncate(Precision)

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock(hashtext($1))", createLock); err != nil {
			return err
		}
		var active int
		if err := tx.QueryRow(ctx, `
			select count(*)
			  from telflo_opamp.api_keys
			 where revoked_at is null and expires_at > $1`,
			k.CreatedAt).Scan(&active); err != nil {
			return err
		}
		if active >= MaxActive {
			return ErrLimitReached
		}
		if _, err := tx.Exec(ctx, `
			insert into telflo_opamp.api_keys (id, name, description, secret_sha256, all_organizations,
			                                   expires_at, revoked_at, last_used_at, created_at)
			values ($1, $2, $3, $4, $5, $6, null, null, $7)`,
			k.ID, k.Name, k.Description, k.SecretSHA256[:], k.AllOrganizations, k.ExpiresAt, k.CreatedAt); err != nil {
			return err
		}
		if !k.AllOrganizations {
			if _, err := tx.Exec(ctx, `
				insert into telflo_opamp.api_key_organizations (organization_id, api_key_id)
				select organization_id, $2 from unnest($1::uuid[]) as organization_id`,
				k.OrganizationIDs, k.ID); err != nil {
				return err
			}
		}
		a, err := get(ctx, tx, k.ID)
		stored = a.Key
		return err
	})
	switch {
	case errors.Is(err, ErrLimitReached):
		return Key{}, err
	case isForeignKeyViolation(err):
		return Key{}, ErrOrganizationNotFound
	case err != nil:
		return Key{}, fmt.Errorf("apikeys: create: %w", err)
	}
	return stored, nil
}

// List returns every key, newest first.
func (s *Postgres) List(ctx context.Context) (keys []Key, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "api_keys.list")
	defer func() { end(err) }()
	rows, err := s.pool.Query(ctx, `
		select `+columns+`
		  from telflo_opamp.api_keys k
		 order by k.created_at desc, k.id desc`)
	if err != nil {
		return nil, fmt.Errorf("apikeys: list: %w", err)
	}
	listed, err := pgx.CollectRows(rows, scanKey)
	if err != nil {
		return nil, fmt.Errorf("apikeys: list: %w", err)
	}
	keys = make([]Key, 0, len(listed))
	for _, a := range listed {
		keys = append(keys, a.Key)
	}
	return keys, nil
}

// Get returns the key id, or ErrNotFound.
func (s *Postgres) Get(ctx context.Context, id uuid.UUID) (k Key, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "api_keys.get")
	defer func() { end(answerOrFailure(err)) }()
	a, err := get(ctx, s.pool, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return Key{}, err
	case err != nil:
		return Key{}, fmt.Errorf("apikeys: get: %w", err)
	}
	return a.Key, nil
}

// Update sets the name and the description in one statement, each only
// when the change names it.
func (s *Postgres) Update(ctx context.Context, id uuid.UUID, c Changes) (k Key, err error) {
	if !c.valid() {
		return Key{}, ErrInvalid
	}
	ctx, end := telemetry.StartDBSpan(ctx, "api_keys.update")
	defer func() { end(answerOrFailure(err)) }()
	a, err := updateReturning(ctx, s.pool, `
		set name = coalesce($2, name),
		    description = case when $3 then $4 else description end
		where id = $1`,
		id, c.Name, c.Description.Set, c.Description.Value)
	switch {
	case errors.Is(err, ErrNotFound):
		return Key{}, err
	case err != nil:
		return Key{}, fmt.Errorf("apikeys: update: %w", err)
	}
	return a.Key, nil
}

// Revoke sets revoked_at in one statement, keeping a revocation's first
// time: two Revokes of one key both answer it revoked at the first's.
func (s *Postgres) Revoke(ctx context.Context, id uuid.UUID, at time.Time) (k Key, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "api_keys.revoke")
	defer func() { end(answerOrFailure(err)) }()
	a, err := updateReturning(ctx, s.pool, `
		set revoked_at = coalesce(revoked_at, $2)
		where id = $1`,
		id, at.UTC().Truncate(Precision))
	switch {
	case errors.Is(err, ErrNotFound):
		return Key{}, err
	case err != nil:
		return Key{}, fmt.Errorf("apikeys: revoke: %w", err)
	}
	return a.Key, nil
}

// Lookup returns the key id with its secret's SHA-256, or ErrNotFound.
func (s *Postgres) Lookup(ctx context.Context, id uuid.UUID) (a Authenticated, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "api_keys.lookup")
	defer func() { end(answerOrFailure(err)) }()
	a, err = get(ctx, s.pool, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return Authenticated{}, err
	case err != nil:
		return Authenticated{}, fmt.Errorf("apikeys: lookup: %w", err)
	}
	return a, nil
}

// Touch sets last_used_at in one statement, never moving it back: two
// requests answered out of order leave the later time.
func (s *Postgres) Touch(ctx context.Context, id uuid.UUID, at time.Time) (err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "api_keys.touch")
	defer func() { end(err) }()
	if _, err := s.pool.Exec(ctx, `
		update telflo_opamp.api_keys
		   set last_used_at = $2
		 where id = $1 and (last_used_at is null or last_used_at < $2)`,
		id, at.UTC().Truncate(Precision)); err != nil {
		return fmt.Errorf("apikeys: touch: %w", err)
	}
	return nil
}

// queryer is a pool or a transaction.
type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// get reads the key id, or ErrNotFound.
func get(ctx context.Context, q queryer, id uuid.UUID) (Authenticated, error) {
	rows, err := q.Query(ctx, `
		select `+columns+`
		  from telflo_opamp.api_keys k
		 where k.id = $1`,
		id)
	if err != nil {
		return Authenticated{}, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, scanKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return Authenticated{}, ErrNotFound
	}
	return a, err
}

// updateReturning runs `update telflo_opamp.api_keys <change>` and reads the
// key it changed, or ErrNotFound when it changed none.
func updateReturning(ctx context.Context, q queryer, change string, args ...any) (Authenticated, error) {
	rows, err := q.Query(ctx, `
		with k as (
		  update telflo_opamp.api_keys `+change+`
		  returning *
		)
		select `+columns+`
		  from k`,
		args...)
	if err != nil {
		return Authenticated{}, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, scanKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return Authenticated{}, ErrNotFound
	}
	return a, err
}

// scanKey reads one row of columns, with its times in UTC.
func scanKey(row pgx.CollectableRow) (Authenticated, error) {
	var (
		a             Authenticated
		secret        []byte
		organizations []uuid.UUID
	)
	if err := row.Scan(&a.ID, &a.Name, &a.Description, &secret, &a.AllOrganizations,
		&a.ExpiresAt, &a.RevokedAt, &a.LastUsedAt, &a.CreatedAt, &organizations); err != nil {
		return Authenticated{}, err
	}
	if len(secret) != len(a.SecretSHA256) {
		return Authenticated{}, errors.New("a stored secret hash of the wrong length")
	}
	copy(a.SecretSHA256[:], secret)
	if !a.AllOrganizations {
		a.OrganizationIDs = slices.SortedFunc(slices.Values(organizations), compareIDs)
		if a.OrganizationIDs == nil {
			a.OrganizationIDs = []uuid.UUID{}
		}
	}
	a.ExpiresAt = a.ExpiresAt.UTC()
	a.CreatedAt = a.CreatedAt.UTC()
	a.RevokedAt = utc(a.RevokedAt)
	a.LastUsedAt = utc(a.LastUsedAt)
	return a, nil
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// isForeignKeyViolation reports whether err is Postgres's refusal 23503.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// answerOrFailure is what a span's end is given: ErrNotFound and the
// refusals are answers, not failures.
func answerOrFailure(err error) error {
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrLimitReached) ||
		errors.Is(err, ErrOrganizationNotFound) || errors.Is(err, ErrInvalid) {
		return nil
	}
	return err
}
