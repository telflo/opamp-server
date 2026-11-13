package variables

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/telflo/opamp-server/store/fleets"
	"github.com/telflo/opamp-server/store/orgvariables"
	"github.com/telflo/opamp-server/store/variableuse"
	"github.com/telflo/opamp-server/telemetry"
)

// Postgres is the Store `serve` uses, on opamp_server.variables,
// variable_groups and variable_group_overrides, as telflo_opamp_server with its
// grants (migration 00012), and on organizations.variables_revision, the
// counter. Every statement names the schema, and every query filters on
// organization_id first.
//
// Every write is one transaction whose first lock is the organization's row
// `for no key update` (variableuse.LockOrganization): the variable writes of
// an organization, and the fleet stores' writes that check or change what
// its collectors are filled in with, serialize there, so the counter's bump,
// the limits' counts and the guards' reads see every write before them, and
// stamps are distinct and in commit order. An enrollment takes the same row
// the same way: contention, not a cycle. An organization's delete takes the
// row `for update`, so it waits for the write, or the write for it.
//
// A create's insert is a targetless `on conflict do nothing`: the id is
// unique twice (the primary key and uq_…_id), and a conflict on either
// inserts nothing rather than raising 23505. A name taken is looked for,
// never raised: read under the organization's lock before the write, and the
// rename's update is guarded `where not exists`, so no unique key's
// violation, whose DETAIL Postgres logs with the name, is reached.
//
// Errors are wrapped with the operation's name only: no SQL argument (no
// name, value, description or email) reaches an error, a log line or a span.
type Postgres struct {
	pool *pgxpool.Pool

	// groupDeleteHook, when a test sets it (export_test.go), runs at each
	// attempt of DeleteGroup: at "start", before its transaction, and at
	// "delete", inside it, just before the delete statement. Nil in serve.
	groupDeleteHook func(attempt int, stage string)
}

// NewPostgres returns the store on pool.
func NewPostgres(pool *pgxpool.Pool) *Postgres {
	return &Postgres{pool: pool}
}

var _ Store = (*Postgres)(nil)

// The keys whose refusal of a variable group's delete is its being in use
// (NO ACTION, migration 00012), and their SQLSTATE: a backstop, since the
// delete reads what uses it under the organization's lock first.
const (
	groupBindingForeignKey = "fk_collector_groups_variable_group"
	pinBindingForeignKey   = "fk_collector_instances_pinned_variable_group"
	foreignKeyViolation    = "23503"
)

// errIDElsewhere rolls a create's transaction back when its insert inserted
// nothing: the id is another organization's (Conflict).
var errIDElsewhere = errors.New("variables: the id is another organization's")

// variableColumns is every column of variables Variable answers, in the
// order scanVariable reads them.
const variableColumns = `id, organization_id, name, value, description,
	changed_by_id, changed_by_email, changed_at`

// groupColumns is a variable group's columns and its counts, in the order
// scanGroup reads them, from the variable_groups row g of the organization
// $1: its overrides with a value (never a reset row), the collector groups
// bound to it and the collectors it is pinned on.
const groupColumns = `g.id, g.organization_id, g.name, g.description,
	g.changed_by_id, g.changed_by_email, g.changed_at,
	(select count(*)
	   from opamp_server.variable_group_overrides o
	  where o.organization_id = $1 and o.variable_group_id = g.id and o.value is not null),
	(select count(*)
	   from opamp_server.collector_groups c
	  where c.organization_id = $1 and c.variable_group_id = g.id),
	(select count(*)
	   from opamp_server.collector_instances i
	  where i.organization_id = $1 and i.pinned_variable_group_id = g.id)`

// write runs fn in one transaction after the organization's row, its first
// lock (`for no key update`); ErrOrganizationNotFound when it has no row.
func (s *Postgres) write(ctx context.Context, organizationID uuid.UUID, fn func(tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		found, err := variableuse.LockOrganization(ctx, tx, organizationID)
		if err != nil {
			return err
		}
		if !found {
			return ErrOrganizationNotFound
		}
		return fn(tx)
	})
}

// ListVariables returns the organization's variables by name.
func (s *Postgres) ListVariables(ctx context.Context, organizationID uuid.UUID) (list []Variable, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "variables.list_variables")
	defer func() { end(err) }()
	rows, err := s.pool.Query(ctx, `
		select `+variableColumns+`
		  from opamp_server.variables
		 where organization_id = $1
		 order by name collate "C"`,
		organizationID)
	if err != nil {
		return nil, fmt.Errorf("variables: list variables: %w", err)
	}
	list, err = pgx.CollectRows(rows, scanVariable)
	if err != nil {
		return nil, fmt.Errorf("variables: list variables: %w", err)
	}
	if list == nil {
		list = []Variable{}
	}
	return list, nil
}

// CreateVariable makes v in one transaction: the organization's row; the
// id's row in the organization (the same content is Unchanged, other
// content Conflict); the name taken; the count at MaxVariables; then the
// bump, and the insert with its stamp, which inserts nothing when the id is
// another organization's (Conflict, and the bump rolls back). So another
// organization's id is a Conflict only when the name is free and the limit
// isn't reached: the store reads its own organization's rows only.
func (s *Postgres) CreateVariable(ctx context.Context, v NewVariable) (result CreateResult, created Variable, err error) {
	if err := v.validate(); err != nil {
		return 0, Variable{}, err
	}
	ctx, end := telemetry.StartDBSpan(ctx, "variables.create_variable")
	defer func() { end(answerOrFailure(err)) }()

	actorID, actorEmail := actorColumns(v.Actor)
	err = s.write(ctx, v.OrganizationID, func(tx pgx.Tx) error {
		existing, found, err := getVariable(ctx, tx, v.OrganizationID, v.ID)
		switch {
		case err != nil:
			return err
		case found:
			result = Conflict
			if existing.Name == v.Name && existing.Value == v.Value && equalText(existing.Description, v.Description) {
				result, created = Unchanged, existing
			}
			return nil
		}
		var taken bool
		var count int
		if err := tx.QueryRow(ctx, `
			select exists (
			         select 1
			           from opamp_server.variables
			          where organization_id = $1 and name = $2),
			       (select count(*)
			          from opamp_server.variables
			         where organization_id = $1)`,
			v.OrganizationID, v.Name).Scan(&taken, &count); err != nil {
			return err
		}
		switch {
		case taken:
			return ErrNameTaken
		case count >= MaxVariables:
			return ErrLimitReached
		}

		stamp, err := variableuse.Bump(ctx, tx, v.OrganizationID)
		if err != nil {
			return bumpFailed(err)
		}
		rows, err := tx.Query(ctx, `
			insert into opamp_server.variables (id, organization_id, name, value, description,
			  changed_by_id, changed_by_email, changed_at, stamp)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			on conflict do nothing
			returning `+variableColumns,
			v.ID, v.OrganizationID, v.Name, v.Value, v.Description, actorID, actorEmail, at(v.At), stamp)
		if err != nil {
			return err
		}
		inserted, err := pgx.CollectRows(rows, scanVariable)
		switch {
		case err != nil:
			return err
		case len(inserted) == 0:
			return errIDElsewhere
		}
		result, created = Created, inserted[0]
		return nil
	})
	switch {
	case errors.Is(err, errIDElsewhere):
		return Conflict, Variable{}, nil
	case isAnswer(err):
		return 0, Variable{}, err
	case err != nil:
		return 0, Variable{}, fmt.Errorf("variables: create variable: %w", err)
	}
	return result, created, nil
}

// UpdateVariable applies changes in one transaction: the organization's
// row; the variable; a new name taken; a rename's guard on the old name
// (variableuse.UsedBy); nothing differing answers it as it is; then a bump
// when the value or the name changes (its stamp on the row), and the
// update.
func (s *Postgres) UpdateVariable(ctx context.Context, organizationID, id uuid.UUID, changes VariableChanges, actor *fleets.Actor, now time.Time) (updated Variable, err error) {
	if err := changes.validate(actor); err != nil {
		return Variable{}, err
	}
	ctx, end := telemetry.StartDBSpan(ctx, "variables.update_variable")
	defer func() { end(answerOrFailure(err)) }()

	actorID, actorEmail := actorColumns(actor)
	err = s.write(ctx, organizationID, func(tx pgx.Tx) error {
		current, found, err := getVariable(ctx, tx, organizationID, id)
		switch {
		case err != nil:
			return err
		case !found:
			return ErrNotFound
		}
		next, filled, changed := variableAfter(current, changes)
		renamed := next.Name != current.Name
		if renamed {
			taken, err := nameTaken(ctx, tx, organizationID, next.Name)
			switch {
			case err != nil:
				return err
			case taken:
				return ErrNameTaken
			}
			uses, err := variableuse.UsedBy(ctx, tx, organizationID, current.Name)
			switch {
			case err != nil:
				return err
			case uses.InUse():
				return &InUseError{Uses: uses}
			}
		}
		if !changed {
			updated = current
			return nil
		}

		var stamp *int64
		if filled {
			n, err := variableuse.Bump(ctx, tx, organizationID)
			if err != nil {
				return bumpFailed(err)
			}
			stamp = &n
		}
		rows, err := tx.Query(ctx, `
			update opamp_server.variables
			   set name = $3, value = $4, description = $5,
			       changed_by_id = $6, changed_by_email = $7, changed_at = $8,
			       stamp = coalesce($9, stamp)
			 where organization_id = $1 and id = $2
			   and not exists (
			         select 1
			           from opamp_server.variables x
			          where x.organization_id = $1 and x.name = $3 and x.id <> $2)
			returning `+variableColumns,
			organizationID, id, next.Name, next.Value, next.Description, actorID, actorEmail, at(now), stamp)
		if err != nil {
			return err
		}
		updated, err = pgx.CollectExactlyOneRow(rows, scanVariable)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNameTaken // only another's write of the name could, and the lock orders those
		}
		return err
	})
	switch {
	case isAnswer(err):
		return Variable{}, err
	case err != nil:
		return Variable{}, fmt.Errorf("variables: update variable: %w", err)
	}
	return updated, nil
}

// DeleteVariable deletes the variable in one transaction: the organization's
// row; the variable; its name's guard (variableuse.UsedBy); then a bump, and
// the delete, whose cascade takes its overrides, reset rows included. The
// bump writes no stamp (the row is gone), but moves the counter that the
// poll's cache is keyed by: a version published before migration 00012 (no
// names) may still reference the name, and its collectors must stop being
// filled in with the deleted value.
func (s *Postgres) DeleteVariable(ctx context.Context, organizationID, id uuid.UUID) (err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "variables.delete_variable")
	defer func() { end(answerOrFailure(err)) }()
	err = s.write(ctx, organizationID, func(tx pgx.Tx) error {
		current, found, err := getVariable(ctx, tx, organizationID, id)
		switch {
		case err != nil:
			return err
		case !found:
			return ErrNotFound
		}
		uses, err := variableuse.UsedBy(ctx, tx, organizationID, current.Name)
		switch {
		case err != nil:
			return err
		case uses.InUse():
			return &InUseError{Uses: uses}
		}
		if _, err := variableuse.Bump(ctx, tx, organizationID); err != nil {
			return bumpFailed(err)
		}
		_, err = tx.Exec(ctx, `
			delete from opamp_server.variables
			 where organization_id = $1 and id = $2`,
			organizationID, id)
		return err
	})
	switch {
	case isAnswer(err):
		return err
	case err != nil:
		return fmt.Errorf("variables: delete variable: %w", err)
	}
	return nil
}

// ListGroups returns the organization's variable groups by name, with
// their counts, in one statement.
func (s *Postgres) ListGroups(ctx context.Context, organizationID uuid.UUID) (list []Group, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "variables.list_groups")
	defer func() { end(err) }()
	rows, err := s.pool.Query(ctx, `
		select `+groupColumns+`
		  from opamp_server.variable_groups g
		 where g.organization_id = $1
		 order by g.name collate "C"`,
		organizationID)
	if err != nil {
		return nil, fmt.Errorf("variables: list groups: %w", err)
	}
	list, err = pgx.CollectRows(rows, scanGroup)
	if err != nil {
		return nil, fmt.Errorf("variables: list groups: %w", err)
	}
	if list == nil {
		list = []Group{}
	}
	return list, nil
}

// CreateGroup makes g in one transaction: the organization's row; the id's
// row in the organization; the name taken; the count at MaxGroups; then the
// insert, which inserts nothing when the id is another organization's
// (Conflict: only when the name is free and the limit isn't reached). No
// bump: a new variable group is bound to nothing, so no collector is filled
// in with it until a binding's write, which bumps.
func (s *Postgres) CreateGroup(ctx context.Context, g NewGroup) (result CreateResult, created Group, err error) {
	if err := g.validate(); err != nil {
		return 0, Group{}, err
	}
	ctx, end := telemetry.StartDBSpan(ctx, "variables.create_group")
	defer func() { end(answerOrFailure(err)) }()

	actorID, actorEmail := actorColumns(g.Actor)
	err = s.write(ctx, g.OrganizationID, func(tx pgx.Tx) error {
		existing, found, err := getGroup(ctx, tx, g.OrganizationID, g.ID)
		switch {
		case err != nil:
			return err
		case found:
			result = Conflict
			if existing.Name == g.Name && equalText(existing.Description, g.Description) {
				result, created = Unchanged, existing
			}
			return nil
		}
		var taken bool
		var count int
		if err := tx.QueryRow(ctx, `
			select exists (
			         select 1
			           from opamp_server.variable_groups
			          where organization_id = $1 and name = $2),
			       (select count(*)
			          from opamp_server.variable_groups
			         where organization_id = $1)`,
			g.OrganizationID, g.Name).Scan(&taken, &count); err != nil {
			return err
		}
		switch {
		case taken:
			return ErrGroupNameTaken
		case count >= MaxGroups:
			return ErrGroupLimitReached
		}

		tag, err := tx.Exec(ctx, `
			insert into opamp_server.variable_groups (id, organization_id, name, description,
			  changed_by_id, changed_by_email, changed_at)
			values ($1, $2, $3, $4, $5, $6, $7)
			on conflict do nothing`,
			g.ID, g.OrganizationID, g.Name, g.Description, actorID, actorEmail, at(g.At))
		switch {
		case err != nil:
			return err
		case tag.RowsAffected() == 0:
			return errIDElsewhere
		}
		created, _, err = getGroup(ctx, tx, g.OrganizationID, g.ID)
		result = Created
		return err
	})
	switch {
	case errors.Is(err, errIDElsewhere):
		return Conflict, Group{}, nil
	case isAnswer(err):
		return 0, Group{}, err
	case err != nil:
		return 0, Group{}, fmt.Errorf("variables: create group: %w", err)
	}
	return result, created, nil
}

// GetGroup returns the variable group and its overrides with a value, by
// their variable's name, from one snapshot (a read-only repeatable read).
func (s *Postgres) GetGroup(ctx context.Context, organizationID, id uuid.UUID) (g Group, overrides []Override, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "variables.get_group")
	defer func() { end(answerOrFailure(err)) }()
	err = pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var found bool
		g, found, err = getGroup(ctx, tx, organizationID, id)
		switch {
		case err != nil:
			return err
		case !found:
			return ErrGroupNotFound
		}
		rows, err := tx.Query(ctx, `
			select o.variable_group_id, o.variable_id, o.value, o.changed_by_id, o.changed_by_email, o.changed_at
			  from opamp_server.variable_group_overrides o
			  join opamp_server.variables v
			    on v.organization_id = $1 and v.id = o.variable_id
			 where o.organization_id = $1 and o.variable_group_id = $2 and o.value is not null
			 order by v.name collate "C"`,
			organizationID, id)
		if err != nil {
			return err
		}
		overrides, err = pgx.CollectRows(rows, scanOverride)
		return err
	})
	switch {
	case isAnswer(err):
		return Group{}, nil, err
	case err != nil:
		return Group{}, nil, fmt.Errorf("variables: get group: %w", err)
	}
	if overrides == nil {
		overrides = []Override{}
	}
	return g, overrides, nil
}

// UpdateGroup applies changes in one transaction: the organization's row;
// the group; a new name taken; nothing differing answers it as it is; then
// the update. No bump: nothing on a variable group's row fills anything in.
func (s *Postgres) UpdateGroup(ctx context.Context, organizationID, id uuid.UUID, changes GroupChanges, actor *fleets.Actor, now time.Time) (updated Group, err error) {
	if err := changes.validate(actor); err != nil {
		return Group{}, err
	}
	ctx, end := telemetry.StartDBSpan(ctx, "variables.update_group")
	defer func() { end(answerOrFailure(err)) }()

	actorID, actorEmail := actorColumns(actor)
	err = s.write(ctx, organizationID, func(tx pgx.Tx) error {
		current, found, err := getGroup(ctx, tx, organizationID, id)
		switch {
		case err != nil:
			return err
		case !found:
			return ErrGroupNotFound
		}
		next, changed := groupAfter(current, changes)
		if next.Name != current.Name {
			var taken bool
			if err := tx.QueryRow(ctx, `
				select exists (
				         select 1
				           from opamp_server.variable_groups
				          where organization_id = $1 and name = $2)`,
				organizationID, next.Name).Scan(&taken); err != nil {
				return err
			}
			if taken {
				return ErrGroupNameTaken
			}
		}
		if !changed {
			updated = current
			return nil
		}
		tag, err := tx.Exec(ctx, `
			update opamp_server.variable_groups
			   set name = $3, description = $4,
			       changed_by_id = $5, changed_by_email = $6, changed_at = $7
			 where organization_id = $1 and id = $2
			   and not exists (
			         select 1
			           from opamp_server.variable_groups x
			          where x.organization_id = $1 and x.name = $3 and x.id <> $2)`,
			organizationID, id, next.Name, next.Description, actorID, actorEmail, at(now))
		switch {
		case err != nil:
			return err
		case tag.RowsAffected() == 0:
			return ErrGroupNameTaken // only another's write of the name could, and the lock orders those
		}
		updated, _, err = getGroup(ctx, tx, organizationID, id)
		return err
	})
	switch {
	case isAnswer(err):
		return Group{}, err
	case err != nil:
		return Group{}, fmt.Errorf("variables: update group: %w", err)
	}
	return updated, nil
}

// DeleteGroup deletes the variable group in one transaction: the
// organization's row; the group; what uses it (variableuse.GroupUse); then
// the delete, whose cascade takes its overrides. The bindings' NO ACTION
// keys refuse it too, a backstop answered the same way. No bump: refused
// while a collector group or a collector uses it, so no collector is
// filled in with it.
func (s *Postgres) DeleteGroup(ctx context.Context, organizationID, id uuid.UUID) (err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "variables.delete_group")
	defer func() { end(answerOrFailure(err)) }()
	for attempt := 0; ; attempt++ {
		s.hook(attempt, "start")
		err = s.write(ctx, organizationID, func(tx pgx.Tx) error {
			_, found, err := getGroup(ctx, tx, organizationID, id)
			switch {
			case err != nil:
				return err
			case !found:
				return ErrGroupNotFound
			}
			use, err := variableuse.GroupUse(ctx, tx, organizationID, id)
			switch {
			case err != nil:
				return err
			case use.InUse():
				return &GroupInUseError{Use: use}
			}
			s.hook(attempt, "delete")
			_, err = tx.Exec(ctx, `
				delete from opamp_server.variable_groups
				 where organization_id = $1 and id = $2`,
				organizationID, id)
			return err
		})
		// A binding written past the guard (a writer that didn't take the
		// organization's row first): read what uses it again, once. A
		// second violation is a failure.
		if !isBindingViolation(err) || attempt > 0 {
			break
		}
	}
	switch {
	case isAnswer(err):
		return err
	case err != nil:
		return fmt.Errorf("variables: delete group: %w", err)
	}
	return nil
}

func (s *Postgres) hook(attempt int, stage string) {
	if s.groupDeleteHook != nil {
		s.groupDeleteHook(attempt, stage)
	}
}

// SetOverride sets the override in one transaction: the organization's row;
// the group; the variable; the existing row, read under the organization's
// lock (none, or a reset row: created; the same value: unchanged, and
// nothing is written); then the bump, the row with its stamp (inserted, or
// the existing one updated), and the group's ChangedBy and ChangedAt.
func (s *Postgres) SetOverride(ctx context.Context, organizationID, groupID, variableID uuid.UUID, value string, actor *fleets.Actor, now time.Time) (o Override, created bool, err error) {
	if err := validOverride(value, actor); err != nil {
		return Override{}, false, err
	}
	ctx, end := telemetry.StartDBSpan(ctx, "variables.set_override")
	defer func() { end(answerOrFailure(err)) }()

	actorID, actorEmail := actorColumns(actor)
	err = s.write(ctx, organizationID, func(tx pgx.Tx) error {
		existing, found, err := s.overrideTarget(ctx, tx, organizationID, groupID, variableID)
		if err != nil {
			return err
		}
		created = !found || existing.value == nil
		if !created && *existing.value == value {
			o = existing.Override(value)
			return nil
		}

		stamp, err := variableuse.Bump(ctx, tx, organizationID)
		if err != nil {
			return bumpFailed(err)
		}
		statement := `
			insert into opamp_server.variable_group_overrides (organization_id, variable_group_id, variable_id,
			  value, changed_by_id, changed_by_email, changed_at, stamp)
			values ($1, $2, $3, $4, $5, $6, $7, $8)
			returning variable_group_id, variable_id, value, changed_by_id, changed_by_email, changed_at`
		if found {
			statement = `
			update opamp_server.variable_group_overrides
			   set value = $4, changed_by_id = $5, changed_by_email = $6, changed_at = $7, stamp = $8
			 where organization_id = $1 and variable_group_id = $2 and variable_id = $3
			returning variable_group_id, variable_id, value, changed_by_id, changed_by_email, changed_at`
		}
		rows, err := tx.Query(ctx, statement, organizationID, groupID, variableID, value, actorID, actorEmail, at(now), stamp)
		if err != nil {
			return err
		}
		if o, err = pgx.CollectExactlyOneRow(rows, scanOverride); err != nil {
			return err
		}
		return touchGroup(ctx, tx, organizationID, groupID, actorID, actorEmail, now)
	})
	switch {
	case isAnswer(err):
		return Override{}, false, err
	case err != nil:
		return Override{}, false, fmt.Errorf("variables: set override: %w", err)
	}
	return o, created, nil
}

// ResetOverride resets the override in one transaction: the organization's
// row; the group; the variable; no row or a reset row answers false, and
// nothing is written; then the bump, the row kept with no value and its
// stamp, and the group's ChangedBy and ChangedAt.
func (s *Postgres) ResetOverride(ctx context.Context, organizationID, groupID, variableID uuid.UUID, actor *fleets.Actor, now time.Time) (reset bool, err error) {
	if !fleets.ValidActor(actor) {
		return false, invalid("actor")
	}
	ctx, end := telemetry.StartDBSpan(ctx, "variables.reset_override")
	defer func() { end(answerOrFailure(err)) }()

	actorID, actorEmail := actorColumns(actor)
	err = s.write(ctx, organizationID, func(tx pgx.Tx) error {
		existing, found, err := s.overrideTarget(ctx, tx, organizationID, groupID, variableID)
		if err != nil || !found || existing.value == nil {
			return err
		}
		stamp, err := variableuse.Bump(ctx, tx, organizationID)
		if err != nil {
			return bumpFailed(err)
		}
		if _, err := tx.Exec(ctx, `
			update opamp_server.variable_group_overrides
			   set value = null, changed_by_id = $4, changed_by_email = $5, changed_at = $6, stamp = $7
			 where organization_id = $1 and variable_group_id = $2 and variable_id = $3`,
			organizationID, groupID, variableID, actorID, actorEmail, at(now), stamp); err != nil {
			return err
		}
		reset = true
		return touchGroup(ctx, tx, organizationID, groupID, actorID, actorEmail, now)
	})
	switch {
	case isAnswer(err):
		return false, err
	case err != nil:
		return false, fmt.Errorf("variables: reset override: %w", err)
	}
	return reset, nil
}

// Variables resolves the organization's variables for a collector in one
// statement, so its revision is the snapshot's: the organization's row, left
// joined to each of its variables, each left joined to the override rows of
// the collector group's variable group and of the pin's, by (organization,
// variable group, variable). Each value is the first level with one, from
// the top down (a reset row's null passes over it), and each stamp the
// greatest stamp from the top down to that source, the source included:
// orgvariables.Choose in SQL (Postgres's greatest ignores nulls). The
// organization's generation comes from the same row as its counter. An
// organization whose counter never moved, or that isn't registered,
// resolves to Resolved{}.
func (s *Postgres) Variables(ctx context.Context, organizationID uuid.UUID, groupVariableGroupID, pinnedVariableGroupID *uuid.UUID) (r orgvariables.Resolved, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "variables.resolve")
	defer func() { end(err) }()
	rows, err := s.pool.Query(ctx, `
		select o.variables_revision, o.generation, v.name,
		       coalesce(po.value, gro.value, v.value),
		       greatest(po.stamp,
		                case when po.value is null then gro.stamp end,
		                case when po.value is null and gro.value is null then v.stamp end)
		  from opamp_server.organizations o
		  left join opamp_server.variables v
		    on v.organization_id = $1
		  left join opamp_server.variable_group_overrides gro
		    on gro.organization_id = $1 and gro.variable_group_id = $2::uuid and gro.variable_id = v.id
		  left join opamp_server.variable_group_overrides po
		    on po.organization_id = $1 and po.variable_group_id = $3::uuid and po.variable_id = v.id
		 where o.id = $1`,
		organizationID, groupVariableGroupID, pinnedVariableGroupID)
	if err != nil {
		return orgvariables.Resolved{}, fmt.Errorf("variables: resolve: %w", err)
	}
	var (
		revision   int64
		generation uuid.UUID
		name       *string
		value      *string
		stamp      *int64
	)
	if _, err := pgx.ForEachRow(rows, []any{&revision, &generation, &name, &value, &stamp}, func() error {
		if revision == 0 {
			return nil
		}
		if r.Revision == 0 {
			r = orgvariables.Resolved{Revision: revision, Generation: generation, Values: map[string]string{}, Stamps: map[string]int64{}}
		}
		if name != nil && value != nil && stamp != nil {
			r.Values[*name], r.Stamps[*name] = *value, *stamp
		}
		return nil
	}); err != nil {
		return orgvariables.Resolved{}, fmt.Errorf("variables: resolve: %w", err)
	}
	return r, nil
}

// overrideRow is an override's row as SetOverride and ResetOverride read
// it: value nil for a reset row.
type overrideRow struct {
	value          *string
	changedByID    *uuid.UUID
	changedByEmail *string
	changedAt      time.Time
	groupID        uuid.UUID
	variableID     uuid.UUID
}

// Override is the row answered as an override with value.
func (r overrideRow) Override(value string) Override {
	return Override{
		VariableGroupID: r.groupID, VariableID: r.variableID, Value: value,
		ChangedBy: actorOf(r.changedByID, r.changedByEmail), ChangedAt: r.changedAt.UTC(),
	}
}

// overrideTarget checks, under the organization's lock, that its variable
// group and variable exist (ErrGroupNotFound, ErrNotFound), and reads their
// override's row, if any.
func (s *Postgres) overrideTarget(ctx context.Context, tx pgx.Tx, organizationID, groupID, variableID uuid.UUID) (row overrideRow, found bool, err error) {
	var groupFound, variableFound bool
	if err := tx.QueryRow(ctx, `
		select exists (
		         select 1
		           from opamp_server.variable_groups
		          where organization_id = $1 and id = $2),
		       exists (
		         select 1
		           from opamp_server.variables
		          where organization_id = $1 and id = $3)`,
		organizationID, groupID, variableID).Scan(&groupFound, &variableFound); err != nil {
		return overrideRow{}, false, err
	}
	switch {
	case !groupFound:
		return overrideRow{}, false, ErrGroupNotFound
	case !variableFound:
		return overrideRow{}, false, ErrNotFound
	}
	row = overrideRow{groupID: groupID, variableID: variableID}
	err = tx.QueryRow(ctx, `
		select value, changed_by_id, changed_by_email, changed_at
		  from opamp_server.variable_group_overrides
		 where organization_id = $1 and variable_group_id = $2 and variable_id = $3`,
		organizationID, groupID, variableID).Scan(&row.value, &row.changedByID, &row.changedByEmail, &row.changedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return overrideRow{}, false, nil
	case err != nil:
		return overrideRow{}, false, err
	}
	return row, true, nil
}

// touchGroup moves the variable group's ChangedBy and ChangedAt, for an
// override's change in it (display only).
func touchGroup(ctx context.Context, tx pgx.Tx, organizationID, groupID uuid.UUID, actorID *uuid.UUID, actorEmail *string, now time.Time) error {
	_, err := tx.Exec(ctx, `
		update opamp_server.variable_groups
		   set changed_by_id = $3, changed_by_email = $4, changed_at = $5
		 where organization_id = $1 and id = $2`,
		organizationID, groupID, actorID, actorEmail, at(now))
	return err
}

// getVariable reads the organization's variable id.
func getVariable(ctx context.Context, tx pgx.Tx, organizationID, id uuid.UUID) (Variable, bool, error) {
	rows, err := tx.Query(ctx, `
		select `+variableColumns+`
		  from opamp_server.variables
		 where organization_id = $1 and id = $2`,
		organizationID, id)
	if err != nil {
		return Variable{}, false, err
	}
	v, err := pgx.CollectExactlyOneRow(rows, scanVariable)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Variable{}, false, nil
	case err != nil:
		return Variable{}, false, err
	}
	return v, true, nil
}

// getGroup reads the organization's variable group id, with its counts.
func getGroup(ctx context.Context, tx pgx.Tx, organizationID, id uuid.UUID) (Group, bool, error) {
	rows, err := tx.Query(ctx, `
		select `+groupColumns+`
		  from opamp_server.variable_groups g
		 where g.organization_id = $1 and g.id = $2`,
		organizationID, id)
	if err != nil {
		return Group{}, false, err
	}
	g, err := pgx.CollectExactlyOneRow(rows, scanGroup)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Group{}, false, nil
	case err != nil:
		return Group{}, false, err
	}
	return g, true, nil
}

// nameTaken reports whether one of the organization's variables has name.
func nameTaken(ctx context.Context, tx pgx.Tx, organizationID uuid.UUID, name string) (bool, error) {
	var taken bool
	err := tx.QueryRow(ctx, `
		select exists (
		         select 1
		           from opamp_server.variables
		          where organization_id = $1 and name = $2)`,
		organizationID, name).Scan(&taken)
	return taken, err
}

// isAnswer reports whether err is one of the store's answers, returned as
// it is rather than wrapped as a failure.
func isAnswer(err error) bool {
	for _, answer := range []error{ErrNotFound, ErrGroupNotFound, ErrNameTaken, ErrGroupNameTaken,
		ErrLimitReached, ErrGroupLimitReached, ErrOrganizationNotFound, ErrInvalid, ErrInUse, ErrGroupInUse} {
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

// isBindingViolation reports whether err is a binding's NO ACTION key
// refusing a variable group's delete.
func isBindingViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation &&
		(pgErr.ConstraintName == groupBindingForeignKey || pgErr.ConstraintName == pinBindingForeignKey)
}

// scanVariable reads one row of variableColumns, its time in UTC.
func scanVariable(row pgx.CollectableRow) (Variable, error) {
	var (
		v          Variable
		actorID    *uuid.UUID
		actorEmail *string
	)
	if err := row.Scan(&v.ID, &v.OrganizationID, &v.Name, &v.Value, &v.Description,
		&actorID, &actorEmail, &v.ChangedAt); err != nil {
		return Variable{}, err
	}
	v.ChangedBy = actorOf(actorID, actorEmail)
	v.ChangedAt = v.ChangedAt.UTC()
	return v, nil
}

// scanGroup reads one row of groupColumns, its time in UTC.
func scanGroup(row pgx.CollectableRow) (Group, error) {
	var (
		g                        Group
		actorID                  *uuid.UUID
		actorEmail               *string
		overrides, bound, pinned int64
	)
	if err := row.Scan(&g.ID, &g.OrganizationID, &g.Name, &g.Description,
		&actorID, &actorEmail, &g.ChangedAt, &overrides, &bound, &pinned); err != nil {
		return Group{}, err
	}
	g.ChangedBy = actorOf(actorID, actorEmail)
	g.ChangedAt = g.ChangedAt.UTC()
	g.OverrideCount = int(overrides)
	g.Usage = Usage{CollectorGroups: int(bound), PinnedCollectors: int(pinned)}
	return g, nil
}

// scanOverride reads one override's row with a value, its time in UTC.
func scanOverride(row pgx.CollectableRow) (Override, error) {
	var (
		o          Override
		actorID    *uuid.UUID
		actorEmail *string
	)
	if err := row.Scan(&o.VariableGroupID, &o.VariableID, &o.Value, &actorID, &actorEmail, &o.ChangedAt); err != nil {
		return Override{}, err
	}
	o.ChangedBy = actorOf(actorID, actorEmail)
	o.ChangedAt = o.ChangedAt.UTC()
	return o, nil
}
