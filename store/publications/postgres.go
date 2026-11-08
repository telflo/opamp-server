package publications

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/telflo/opamp-server/telemetry"
)

// Postgres is the Store `serve` uses, on telflo_opamp.publications, as
// telflo_opamp (select, insert and delete; no update), and on
// telflo_opamp.publication_variable_names (select and insert: its rows go
// with their publication by the cascade). Every statement names
// the schema, and every query filters on organization_id first.
//
// Errors are wrapped with the operation's name only: no SQL argument (no
// YAML, no email) reaches an error, a log line or a span.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres returns the store on pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool}
}

var _ Store = (*Postgres)(nil)

// The foreign key from a publication to its organization (migration 00002),
// the names' (00012), the two that keep a publication in use from being
// deleted (migrations 00007 and 00009, NO ACTION), and the SQLSTATE of
// their refusal.
const (
	organizationForeignKey = "fk_publications_organization"
	// namesOrganizationForeignKey is the names' key to their organization
	// (migration 00012): Put's one statement inserts into both tables.
	namesOrganizationForeignKey = "fk_publication_variable_names_organization"
	assignedForeignKey          = "fk_collector_groups_assigned_publication"
	pinnedForeignKey            = "fk_collector_instances_pinned_publication"
	foreignKeyViolation         = "23503"
)

// columns is every column, in the order scanPublication reads them. Put
// writes every one: the collector's two have had no default since migration
// 00004 (00003's, for a server of the release before, are dropped).
const columns = `id, organization_id, configuration_id, configuration_name,
	version_id, version_number, version_label, yaml, sha256,
	published_by_id, published_by_email, published_at, received_at,
	collector_distribution, collector_version`

// summaryColumns is columns without yaml, which List leaves out.
const summaryColumns = `id, organization_id, configuration_id, configuration_name,
	version_id, version_number, version_label, '' as yaml, sha256,
	published_by_id, published_by_email, published_at, received_at,
	collector_distribution, collector_version`

// Put inserts p in one statement, `insert … on conflict do nothing`, which
// the foreign key refuses for an organization that isn't registered, so two
// Puts of the same new id can't both insert or fail: the second waits for
// the first's transaction, then inserts nothing. The conflict has no target:
// the id is unique twice, by the primary key and by
// uq_publications_organization_id_id (migration 00005), and a target naming
// one would let the loser of a race pass it and raise 23505 on the other.
// Inserting nothing means the id is taken, and a read within p's
// organization classifies it: the identical publication is Unchanged; a
// different one, or none (the id is another organization's, or was deleted
// since), is a Conflict.
//
// The names its YAML references go in the same statement: a data-modifying
// CTE inserts them into publication_variable_names from the publication
// insert's `returning`, so a Put that inserts nothing inserts no names. The
// names come from References, which already lists each once; the `select
// distinct` over the array is only a safeguard, so a duplicate could never
// raise 23505. Either table's key to the organization may be the one that
// finds it missing. A reference to a name over 128 characters is refused
// before the statement runs, and before its span starts, so a refusal
// records no database span (ErrReferenceTooLong).
func (s *Postgres) Put(ctx context.Context, p Publication) (result PutResult, stored Publication, err error) {
	names, err := variableNames(p)
	if err != nil {
		return 0, Publication{}, err
	}
	if names == nil {
		names = []string{}
	}
	ctx, end := telemetry.StartDBSpan(ctx, "publications.put")
	defer func() {
		if errors.Is(err, ErrOrganizationNotFound) {
			end(nil) // an answer, not a failure
		} else {
			end(err)
		}
	}()
	p = p.normalized()

	var actorID *uuid.UUID
	var actorEmail *string
	if p.PublishedBy != nil {
		actorID, actorEmail = &p.PublishedBy.ID, &p.PublishedBy.Email
	}
	rows, err := s.pool.Query(ctx, `
		with inserted as (
		  insert into telflo_opamp.publications (`+columns+`)
		  values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		  on conflict do nothing
		  returning `+columns+`
		), names as (
		  insert into telflo_opamp.publication_variable_names (organization_id, publication_id, name)
		  select i.organization_id, i.id, n.name
		    from inserted i
		   cross join (select distinct name from unnest($16::text[]) as u (name)) n
		)
		select `+columns+` from inserted`,
		p.ID, p.OrganizationID, p.ConfigurationID, p.ConfigurationName,
		p.VersionID, p.VersionNumber, p.VersionLabel, p.YAML, p.SHA256,
		actorID, actorEmail, p.PublishedAt, p.ReceivedAt,
		p.Collector.Distribution, p.Collector.Version, names)
	if organizationMissing(err) {
		return 0, Publication{}, ErrOrganizationNotFound
	}
	if err != nil {
		return 0, Publication{}, fmt.Errorf("publications: put: %w", err)
	}
	inserted, err := pgx.CollectRows(rows, scanPublication)
	if organizationMissing(err) {
		return 0, Publication{}, ErrOrganizationNotFound
	}
	if err != nil {
		return 0, Publication{}, fmt.Errorf("publications: put: %w", err)
	}
	if len(inserted) == 1 {
		return Created, inserted[0], nil
	}

	existing, err := s.get(ctx, p.OrganizationID, p.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return Conflict, Publication{}, nil
	case err != nil:
		return 0, Publication{}, fmt.Errorf("publications: put: %w", err)
	case existing.SameAs(p):
		return Unchanged, existing, nil
	default:
		return Conflict, Publication{}, nil
	}
}

// organizationMissing reports whether err is a foreign key refusing a
// publication, or the names it references, because the organization isn't
// registered: either key may fire first.
func organizationMissing(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation &&
		(pgErr.ConstraintName == organizationForeignKey || pgErr.ConstraintName == namesOrganizationForeignKey)
}

// inUse reports whether err is one of the in-use keys refusing a delete.
func inUse(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation &&
		(pgErr.ConstraintName == assignedForeignKey || pgErr.ConstraintName == pinnedForeignKey)
}

// inUseIsAnAnswer is what a delete's span ends with: ErrInUse is an
// answer, not a failure.
func inUseIsAnAnswer(err error) error {
	if errors.Is(err, ErrInUse) {
		return nil
	}
	return err
}

// Get returns the organization's publication id, or ErrNotFound.
func (s *Postgres) Get(ctx context.Context, organizationID, id uuid.UUID) (p Publication, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "publications.get")
	defer func() {
		if errors.Is(err, ErrNotFound) {
			end(nil) // an answer, not a failure
		} else {
			end(err)
		}
	}()
	p, err = s.get(ctx, organizationID, id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Publication{}, fmt.Errorf("publications: get: %w", err)
	}
	return p, err
}

func (s *Postgres) get(ctx context.Context, organizationID, id uuid.UUID) (Publication, error) {
	rows, err := s.pool.Query(ctx, `
		select `+columns+`
		  from telflo_opamp.publications
		 where organization_id = $1 and id = $2`,
		organizationID, id)
	if err != nil {
		return Publication{}, err
	}
	p, err := pgx.CollectExactlyOneRow(rows, scanPublication)
	if errors.Is(err, pgx.ErrNoRows) {
		return Publication{}, ErrNotFound
	}
	return p, err
}

// List returns a page of the organization's publications of a
// configuration, newest first, by keyset: the rows after the cursor in
// (published_at, id) order, which the index serves.
func (s *Postgres) List(ctx context.Context, organizationID, configurationID uuid.UUID, limit int, after *Cursor) (page Page, err error) {
	if err := checkLimit(limit); err != nil {
		return Page{}, err
	}
	ctx, end := telemetry.StartDBSpan(ctx, "publications.list")
	defer func() { end(err) }()

	// One row more than the page, to know whether another page follows.
	query := `
		select ` + summaryColumns + `
		  from telflo_opamp.publications
		 where organization_id = $1 and configuration_id = $2`
	args := []any{organizationID, configurationID}
	if after != nil {
		query += ` and (published_at, id) < ($4, $5)`
		args = append(args, limit+1, after.PublishedAt, after.ID)
	} else {
		args = append(args, limit+1)
	}
	query += ` order by published_at desc, id desc limit $3`

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return Page{}, fmt.Errorf("publications: list: %w", err)
	}
	found, err := pgx.CollectRows(rows, scanPublication)
	if err != nil {
		return Page{}, fmt.Errorf("publications: list: %w", err)
	}
	page = Page{Publications: found}
	if len(found) > limit {
		page.Publications = found[:limit]
		page.Next = cursorOf(found[limit-1])
	}
	if page.Publications == nil {
		page.Publications = []Publication{}
	}
	return page, nil
}

// Delete removes the organization's publication id in one statement, which
// the in-use keys refuse whole (ErrInUse) while a group or a collector uses
// it.
func (s *Postgres) Delete(ctx context.Context, organizationID, id uuid.UUID) (deleted bool, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "publications.delete")
	defer func() { end(inUseIsAnAnswer(err)) }()
	tag, err := s.pool.Exec(ctx, `
		delete from telflo_opamp.publications
		 where organization_id = $1 and id = $2`,
		organizationID, id)
	if inUse(err) {
		return false, ErrInUse
	}
	if err != nil {
		return false, fmt.Errorf("publications: delete: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// DeleteForConfiguration removes the organization's publications of a
// configuration in one statement, which the in-use keys refuse whole
// (ErrInUse, deleting none) while a group or a collector uses any of them.
func (s *Postgres) DeleteForConfiguration(ctx context.Context, organizationID, configurationID uuid.UUID) (deleted int, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "publications.delete_for_configuration")
	defer func() { end(inUseIsAnAnswer(err)) }()
	tag, err := s.pool.Exec(ctx, `
		delete from telflo_opamp.publications
		 where organization_id = $1 and configuration_id = $2`,
		organizationID, configurationID)
	if inUse(err) {
		return 0, ErrInUse
	}
	if err != nil {
		return 0, fmt.Errorf("publications: delete for configuration: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Usage reads the configuration's publications, newest first, each with
// the groups assigned it and the count of collectors pinned to it, in one
// statement (the two partial indexes on the in-use keys serve both
// subqueries).
func (s *Postgres) Usage(ctx context.Context, organizationID, configurationID uuid.UUID) (usage []Usage, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "publications.usage")
	defer func() { end(err) }()
	rows, err := s.pool.Query(ctx, `
		select `+summaryColumns+`,
		       coalesce(g.fleet_ids, '{}'), coalesce(g.group_ids, '{}'),
		       (select count(*)
		          from telflo_opamp.collector_instances c
		         where c.organization_id = $1 and c.pinned_publication_id = p.id)
		  from telflo_opamp.publications p
		  left join lateral (
		         select array_agg(cg.fleet_id order by cg.fleet_id, cg.id) as fleet_ids,
		                array_agg(cg.id order by cg.fleet_id, cg.id) as group_ids
		           from telflo_opamp.collector_groups cg
		          where cg.organization_id = $1 and cg.assigned_publication_id = p.id) g on true
		 where p.organization_id = $1 and p.configuration_id = $2
		 order by p.published_at desc, p.id desc`,
		organizationID, configurationID)
	if err != nil {
		return nil, fmt.Errorf("publications: usage: %w", err)
	}
	usage, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Usage, error) {
		var fleetIDs, groupIDs []uuid.UUID
		var pinned int64
		p, err := scanPublicationThen(row, &fleetIDs, &groupIDs, &pinned)
		if err != nil {
			return Usage{}, err
		}
		u := Usage{Publication: p, Groups: []GroupRef{}, PinnedCollectors: int(pinned)}
		for i := range fleetIDs {
			u.Groups = append(u.Groups, GroupRef{FleetID: fleetIDs[i], GroupID: groupIDs[i]})
		}
		return u, nil
	})
	if err != nil {
		return nil, fmt.Errorf("publications: usage: %w", err)
	}
	if usage == nil {
		usage = []Usage{}
	}
	return usage, nil
}

// scanPublication reads one row of columns (or summaryColumns), with its
// times in UTC.
func scanPublication(row pgx.CollectableRow) (Publication, error) {
	return scanPublicationThen(row)
}

// scanPublicationThen reads one row of columns (or summaryColumns), then the
// trailing values.
func scanPublicationThen(row pgx.CollectableRow, trailing ...any) (Publication, error) {
	var (
		p          Publication
		actorID    *uuid.UUID
		actorEmail *string
	)
	if err := row.Scan(append([]any{
		&p.ID, &p.OrganizationID, &p.ConfigurationID, &p.ConfigurationName,
		&p.VersionID, &p.VersionNumber, &p.VersionLabel, &p.YAML, &p.SHA256,
		&actorID, &actorEmail, &p.PublishedAt, &p.ReceivedAt,
		&p.Collector.Distribution, &p.Collector.Version,
	}, trailing...)...); err != nil {
		return Publication{}, err
	}
	switch {
	case actorID != nil && actorEmail != nil:
		p.PublishedBy = &Actor{ID: *actorID, Email: *actorEmail}
	case actorID != nil || actorEmail != nil:
		// The table's check makes this unreachable.
		return Publication{}, errors.New("a row has only one of published_by_id and published_by_email")
	}
	p.PublishedAt = p.PublishedAt.UTC()
	p.ReceivedAt = p.ReceivedAt.UTC()
	return p, nil
}
