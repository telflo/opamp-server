package collectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	petname "github.com/dustinkirkland/golang-petname"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/telflo/opamp-server/store/variableuse"
	"github.com/telflo/opamp-server/telemetry"
)

// Postgres is the Store `serve` uses, on telflo_opamp.collector_instances
// and collector_errors (reading organizations, fleets, collector_groups and
// publications), as telflo_opamp with its grants. Every
// statement names the schema, and every query filters on organization_id
// first, but Cleanup's, which runs across organizations by its nature.
//
// Errors are wrapped with the operation's name only: no SQL argument (no
// name, no message, no attribute) reaches an error, a log line or a span.
//
// Locks, in the order every transaction here takes them (the lock order:
// organization, fleet, group, then collector or token, a fleet always
// before any row it owns): Enroll the organization's row `for no key
// update`, then the fleet's `for key share`, then the group's `for key
// share`; Move, Pin and BulkPin, and Update when it pins, moves or sets a
// pinned variable group, the organization's row `for no key update`
// (variableuse.LockOrganization: they serialize with the variable writes,
// whose guards read what they change, and they stamp), RecordError, Delete
// and a rename's Update the organization's row `for key share`; then the
// fleet's (lockFleet), then the group and the collectors they change
// (BulkPin in id order). Each write chooses its organization's mode from
// what it was asked to change, before its first lock, and never takes the
// row again in a stronger mode: an upgrade from `for key share` deadlocks
// three ways, with an enrollment and a variable write. An enrollment and a
// variable write serialize on the organization's row: contention, not a
// cycle. An organization's delete locks the organization's row and cascades
// from there, and a fleet's delete takes the organization's and then the
// fleet's, so each queues behind them, or they behind it, and never
// deadlocks with them. Poll and Report are single statements that lock the
// collector's row and then key-share what a changed key names (a token, a
// published version). An organization's or a fleet's delete locks its
// collectors (in id order) before its cascade deletes any of those, so it
// waits for a poll or a report, or they for it; one racing a published
// version's delete may still be refused 40P01, which the agent side answers
// 503.
type Postgres struct {
	pool    *pgxpool.Pool
	petname func(words int) string
}

// Option configures a store.
type Option func(*options)

type options struct {
	petname func(words int) string
}

// Petnames makes the store name collectors with generate (a number of
// words, joined by "-") in place of golang-petname: for tests that need a
// name to collide.
func Petnames(generate func(words int) string) Option {
	return func(o *options) { o.petname = generate }
}

func optionsOf(opts []Option) options {
	o := options{petname: func(words int) string { return petname.Generate(words, "-") }}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// NewPostgres returns the store on pool.
func NewPostgres(pool *pgxpool.Pool, opts ...Option) *Postgres {
	o := optionsOf(opts)
	return &Postgres{pool: pool, petname: o.petname}
}

var _ Store = (*Postgres)(nil)

// The constraints whose refusals are answers, and their SQLSTATEs.
const (
	nameUniqueKey            = "uq_collector_instances_organization_id_name"
	groupForeignKey          = "fk_collector_instances_collector_group"
	fleetForeignKey          = "fk_collector_instances_fleet"
	tokenForeignKey          = "fk_collector_instances_enrollment_token"
	pinnedPublicationKey     = "fk_collector_instances_pinned_publication"
	servedPublicationKey     = "fk_collector_instances_served_publication"
	pinnedVariableGroupKey   = "fk_collector_instances_pinned_variable_group"
	errorCollectorForeignKey = "fk_collector_errors_collector_instance"
	attributesCheck          = "ck_collector_instances_reported_attributes_bounded"
	foreignKeyViolation      = "23503"
	uniqueViolation          = "23505"
	checkViolation           = "23514"
)

// How many names Enroll tries: petnames of two words, then of three.
const (
	maxNameAttempts  = 8
	twoWordAttempts  = 4
	enrollmentRounds = 3
)

// rowColumns is every column of collector_instances, in the order
// scanCollector reads them.
var rowColumns = []string{"id", "organization_id", "fleet_id", "collector_group_id", "instance_uid", "name",
	"pinned_publication_id", "served_publication_id",
	"served_sha256", "served_at", "effective_sha256", "effective_status", "effective_error", "effective_at",
	"full_state_requested_at", "reported_collector_version", "reported_ip", "reported_host_ip",
	"reported_attributes", "enrollment_token_id",
	"pinned_variable_group_id", "pinned_variable_group_stamp", "served_variables_stamp",
	"last_seen_at", "created_at", "updated_at"}

// selectList is rowColumns of the alias, each named as its column; lean
// leaves effective_error and reported_attributes null (the poll's hot path
// reads neither), and reported_attributes is read as text.
func selectList(alias string, lean bool) string {
	parts := make([]string, len(rowColumns))
	for i, column := range rowColumns {
		expression := alias + "." + column
		switch {
		case lean && (column == "effective_error" || column == "reported_attributes"):
			expression = "null::text"
		case column == "reported_attributes":
			expression += "::text"
		}
		parts[i] = expression + " as " + column
	}
	return strings.Join(parts, ", ")
}

// resolutionJoins joins what resolves for the collector the alias names:
// its pinned published version, else its group's assigned published
// version. The group is joined for every collector, since its variable
// group applies under a pin too; its published version only for a collector
// without a pin.
func resolutionJoins(alias string) string {
	return strings.ReplaceAll(`
		left join telflo_opamp.publications pp
		       on pp.organization_id = A.organization_id and pp.id = A.pinned_publication_id
		left join telflo_opamp.collector_groups g
		       on g.organization_id = A.organization_id and g.fleet_id = A.fleet_id
		      and g.id = A.collector_group_id
		left join telflo_opamp.publications gp
		       on gp.organization_id = A.organization_id and gp.id = g.assigned_publication_id
		      and A.pinned_publication_id is null`, "A.", alias+".")
}

// resolutionColumns are what resolutionJoins resolve: where from, the id,
// the SHA-256 (never the YAML), and the group's variable group with its
// setting's stamp.
func resolutionColumns(alias string) string {
	return strings.ReplaceAll(`
		case when A.pinned_publication_id is not null then 'pinned_publication'
		     when gp.id is not null then 'group_publication'
		     else 'none' end,
		coalesce(pp.id, gp.id),
		coalesce(pp.sha256, gp.sha256),
		g.variable_group_id,
		coalesce(g.variable_group_stamp, 0)`, "A.", alias+".")
}

// nilKey is a missing variable group's key in combos: the nil UUID, which
// no variable group has, so keys compare with = (never `is not distinct
// from`, which no index or hash serves).
const nilKey = `'00000000-0000-0000-0000-000000000000'::uuid`

// wantedSQL is the wanted stamp's SQL over a source of collectors
// (wantedStamp).
type wantedSQL struct {
	// combos is the common table expression combos, to follow `with`.
	combos string
	// join joins combos to the collector the alias names; it follows
	// resolutionJoins of that alias.
	join string
	// stamp is the collector's wanted stamp.
	stamp string
}

// wantedStamp is the one SQL fragment of the wanted stamp, shared by
// getListed, List and summariesStatement, each passing its source of
// collectors: from (with the alias c) and where, the fleets' rows, the
// page, or one collector. $1 is the organization in each.
//
// combos is set-based, never a query per collector: each distinct (resolved
// published version, pin's variable group, group's variable group) of the
// source once, its keys coalesced to nilKey, and its names_stamp: over the
// names the published version references, the greatest of each name's
// stamp, orgvariables.Choose in SQL: the greatest stamp among the rows read
// from the top down to the first level with a value (the pin's variable
// group's override po, the group's go, the variable v), the source
// included, so a reset row above the source counts and a row below it
// doesn't (Postgres's greatest ignores nulls); a name no variable defines
// counts 0. names_stamp is null when the published version has no names
// (one published before migration 00012). Materialized, so each triple's
// subquery runs once.
//
// The collector's wanted stamp is then orgvariables.WantedStamp: 0 when its
// published version has no names or nothing resolves, else the greatest of
// names_stamp and the stamps of its pin's and its group's variable-group
// settings. No variable group's row is read for a stamp.
func wantedStamp(from, where, alias string) wantedSQL {
	return wantedSQL{
		combos: `combos as materialized (
		  select k.resolved_id,
		         coalesce(k.pin_vg, ` + nilKey + `) as pin_key,
		         coalesce(k.group_vg, ` + nilKey + `) as group_key,
		         (select max(coalesce(greatest(po.stamp,
		                                       case when po.value is null then go.stamp end,
		                                       case when po.value is null and go.value is null then v.stamp end), 0))
		            from telflo_opamp.publication_variable_names n
		            left join telflo_opamp.variables v
		                   on v.organization_id = n.organization_id and v.name = n.name
		            left join telflo_opamp.variable_group_overrides po
		                   on po.organization_id = n.organization_id and po.variable_group_id = k.pin_vg
		                  and po.variable_id = v.id
		            left join telflo_opamp.variable_group_overrides go
		                   on go.organization_id = n.organization_id and go.variable_group_id = k.group_vg
		                  and go.variable_id = v.id
		           where n.organization_id = $1 and n.publication_id = k.resolved_id) as names_stamp
		    from (select distinct coalesce(pp.id, gp.id) as resolved_id,
		                 c.pinned_variable_group_id as pin_vg, g.variable_group_id as group_vg
		            from ` + from + resolutionJoins("c") + `
		           where ` + where + ` and coalesce(pp.id, gp.id) is not null) k
		)`,
		join: strings.ReplaceAll(`
		left join combos w
		       on w.resolved_id = coalesce(pp.id, gp.id)
		      and w.pin_key = coalesce(A.pinned_variable_group_id, `+nilKey+`)
		      and w.group_key = coalesce(g.variable_group_id, `+nilKey+`)`, "A.", alias+"."),
		stamp: strings.ReplaceAll(`
		case when w.names_stamp is null then 0
		     else greatest(w.names_stamp, coalesce(A.pinned_variable_group_stamp, 0),
		                   coalesce(g.variable_group_stamp, 0)) end`, "A.", alias+"."),
	}
}

// The wanted stamp's three sources.
var (
	// oneWanted is one collector's: getListed's, $2 its fleet and $3 its id.
	oneWanted = wantedStamp("telflo_opamp.collector_instances c",
		"c.organization_id = $1 and c.fleet_id = $2 and c.id = $3", "c")
	// pageWanted is a page's: List's, over its common table expression page.
	pageWanted = wantedStamp("page c", "c.organization_id = $1", "c")
	// fleetsWanted is the fleets' rows: summariesStatement's, $2 the fleets.
	fleetsWanted = wantedStamp("telflo_opamp.collector_instances c",
		"c.organization_id = $1 and c.fleet_id = any ($2)", "c")
)

// Enroll stores the collector in one transaction, retried (at most
// enrollmentRounds times) only when the row its insert conflicted with was
// deleted before it could read it.
func (s *Postgres) Enroll(ctx context.Context, e Enrollment) (r Enrolled, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "collectors.enroll")
	defer func() { end(answerOrFailure(err)) }()
	var desired *string
	if ValidName(e.DesiredName) {
		desired = &e.DesiredName
	}
	for range enrollmentRounds {
		var found bool
		err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			var err error
			r, found, err = s.enroll(ctx, tx, e, desired)
			return err
		})
		if err != nil || found {
			break
		}
	}
	var pgErr *pgconn.PgError
	switch {
	case isAnswer(err):
		return Enrolled{}, err
	case errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation && pgErr.ConstraintName == groupForeignKey:
		// The group was deleted between its lock and the insert's check.
		return Enrolled{}, ErrForeignGroup
	case errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation && pgErr.ConstraintName == tokenForeignKey:
		return Enrolled{}, ErrTokenNotFound
	case errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation && pgErr.ConstraintName == fleetForeignKey:
		return Enrolled{}, ErrFleetNotFound
	case err != nil:
		return Enrolled{}, fmt.Errorf("collectors: enroll: %w", err)
	case r.Collector.ID == uuid.Nil:
		return Enrolled{}, errors.New("collectors: enroll: the conflicting row kept vanishing")
	}
	return r, nil
}

// enroll is one round of Enroll in tx; found is false when the insert
// inserted nothing and no row was there to read (deleted meanwhile).
func (s *Postgres) enroll(ctx context.Context, tx pgx.Tx, e Enrollment, desired *string) (r Enrolled, found bool, err error) {
	// The organization's row, in its own statement: enrollments of one
	// organization serialize here, so the next statement's snapshot, taken
	// after the wait, sees every collector enrolled before it. `for no key
	// update` doesn't block a foreign key's key-share lock on the row.
	var max *int32
	err = tx.QueryRow(ctx, `
		select max_collectors
		  from telflo_opamp.organizations
		 where id = $1
		   for no key update`,
		e.OrganizationID).Scan(&max)
	if errors.Is(err, pgx.ErrNoRows) {
		return Enrolled{}, false, ErrFleetNotFound
	}
	if err != nil {
		return Enrolled{}, false, err
	}
	// The fleet before any row it owns (the lock order on Postgres); the
	// organization's row is held already.
	if ok, err := keyShareFleet(ctx, tx, e.OrganizationID, e.FleetID); err != nil || !ok {
		if err == nil {
			err = ErrFleetNotFound
		}
		return Enrolled{}, false, err
	}
	// The group it names, of its fleet, or the Default group; held so a
	// group's delete can't remove it before the insert.
	var group uuid.UUID
	err = tx.QueryRow(ctx, `
		select id
		  from telflo_opamp.collector_groups
		 where organization_id = $1 and fleet_id = $2
		   and case when $3::uuid is null then is_default else id = $3::uuid end
		   for key share`,
		e.OrganizationID, e.FleetID, e.GroupID).Scan(&group)
	switch {
	case errors.Is(err, pgx.ErrNoRows) && e.GroupID != nil:
		return Enrolled{}, false, ErrForeignGroup
	case errors.Is(err, pgx.ErrNoRows):
		return Enrolled{}, false, errors.New("the fleet has no Default group")
	case err != nil:
		return Enrolled{}, false, err
	}

	// The cap's count and the insert, in one statement; the name chosen in
	// it: the desired name if free, else the petname. A racing rename that
	// takes the name after this statement's snapshot is a 23505 on the
	// name, retried in a savepoint with a fresh petname.
	now := at(e.At)
	id := uuid.New()
	var inserted, capped bool
	for attempt := 0; ; attempt++ {
		words := 2
		if attempt >= twoWordAttempts {
			words = 3
		}
		savepoint, err := tx.Begin(ctx)
		if err != nil {
			return Enrolled{}, false, err
		}
		err = savepoint.QueryRow(ctx, `
			with counted as (
			  select case when $8::int is null then null::bigint
			              else (select count(*)
			                      from telflo_opamp.collector_instances
			                     where organization_id = $1::uuid) end as n
			), inserted as (
			  insert into telflo_opamp.collector_instances
			    (id, organization_id, fleet_id, collector_group_id, instance_uid, name,
			     enrollment_token_id, last_seen_at, created_at, updated_at)
			  select $2::uuid, $1::uuid, $3::uuid, $4::uuid, $5::uuid,
			         case when $6::text is not null and not exists (
			                select 1
			                  from telflo_opamp.collector_instances x
			                 where x.organization_id = $1::uuid and x.name = $6::text)
			              then $6::text else $7::text end,
			         $9::uuid, $10::timestamptz, $10::timestamptz, $10::timestamptz
			    from counted
			   where counted.n is null or counted.n < $8::int
			  on conflict (organization_id, instance_uid) do nothing
			  returning id
			)
			select exists (select 1 from inserted),
			       coalesce((select n from counted) >= $8::int, false)`,
			e.OrganizationID, id, e.FleetID, group, e.InstanceUID, desired, s.petname(words),
			max, e.TokenID, now).Scan(&inserted, &capped)
		if isViolation(err, uniqueViolation, nameUniqueKey) && attempt+1 < maxNameAttempts {
			if err := savepoint.Rollback(ctx); err != nil {
				return Enrolled{}, false, err
			}
			continue
		}
		if isViolation(err, uniqueViolation, nameUniqueKey) {
			_ = savepoint.Rollback(ctx)
			return Enrolled{}, false, errNoFreeName
		}
		if err != nil {
			_ = savepoint.Rollback(ctx)
			return Enrolled{}, false, err
		}
		if err := savepoint.Commit(ctx); err != nil {
			return Enrolled{}, false, err
		}
		break
	}

	// The row, in a statement of its own: a conflict's waiter sees the
	// winner's row only in a new snapshot.
	rows, err := tx.Query(ctx, `
		select `+selectList("c", false)+`
		  from telflo_opamp.collector_instances c
		 where c.organization_id = $1 and c.instance_uid = $2`,
		e.OrganizationID, e.InstanceUID)
	if err != nil {
		return Enrolled{}, false, err
	}
	c, err := pgx.CollectExactlyOneRow(rows, scanRow)
	switch {
	case errors.Is(err, pgx.ErrNoRows) && capped:
		return Enrolled{}, false, ErrCapReached
	case errors.Is(err, pgx.ErrNoRows):
		return Enrolled{}, false, nil
	case err != nil:
		return Enrolled{}, false, err
	case c.FleetID != e.FleetID:
		return Enrolled{}, true, ErrFleetMismatch
	}
	return Enrolled{Collector: c, Created: inserted}, true, nil
}

// pollStatement is the poll in one statement. Every part reads the
// statement's one snapshot, so `before` is the row before the heartbeat.
// The heartbeat is written only for the token's fleet, and only when the
// token changed or the last one is at or before $6 (now less
// HeartbeatCoalesce); it never moves last_seen_at back (a token change from
// a replica whose clock is behind keeps the later time). The resolution
// answers the SHA-256, never the YAML (not detoasted), and the group's
// variable group; the organization's row answers its counter and its
// generation (the variables cache's key), all in the same one statement.
//
//	$1 organization, $2 instance uid, $3 token, $4 the token's fleet,
//	$5 now, $6 now less HeartbeatCoalesce
var pollStatement = `
	with before as (
	  select ` + selectList("c", true) + `
	    from telflo_opamp.collector_instances c
	   where c.organization_id = $1 and c.instance_uid = $2
	), touched as (
	  update telflo_opamp.collector_instances c
	     set last_seen_at = greatest(c.last_seen_at, $5), enrollment_token_id = $3
	    from before b
	   where c.organization_id = $1 and c.id = b.id and c.fleet_id = $4
	     and (c.enrollment_token_id is distinct from $3 or c.last_seen_at <= $6)
	  returning c.id
	)
	select ` + selectList("b", false) + `,
	       exists (select 1 from touched),` + resolutionColumns("b") + `,
	       o.variables_revision, o.generation
	  from before b
	  join telflo_opamp.organizations o on o.id = b.organization_id` + resolutionJoins("b")

// Poll runs pollStatement.
func (s *Postgres) Poll(ctx context.Context, p PollRequest) (r PollResult, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "collectors.poll")
	defer func() { end(err) }()
	now := at(p.At)
	rows, err := s.pool.Query(ctx, pollStatement,
		p.OrganizationID, p.InstanceUID, p.TokenID, p.FleetID, now, now.Add(-HeartbeatCoalesce))
	if err != nil {
		return PollResult{}, fmt.Errorf("collectors: poll: %w", err)
	}
	r, err = pgx.CollectExactlyOneRow(rows, func(row pgx.CollectableRow) (PollResult, error) {
		var r PollResult
		resolution := resolutionScan{}
		c, err := scanCollector(row, append(append([]any{&r.Touched}, resolution.dest()...), &r.VariablesRevision, &r.OrganizationGeneration)...)
		if err != nil {
			return PollResult{}, err
		}
		r.Found, r.Collector, r.Resolution = true, c, resolution.result()
		return r, nil
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return PollResult{}, nil
	case err != nil:
		return PollResult{}, fmt.Errorf("collectors: poll: %w", err)
	}
	return r, nil
}

// YAML reads the bytes a cache miss needs, by the organization.
func (s *Postgres) YAML(ctx context.Context, organizationID uuid.UUID, ref Ref) (y YAMLResult, err error) {
	if !ref.valid() {
		return YAMLResult{}, invalid("ref")
	}
	ctx, end := telemetry.StartDBSpan(ctx, "collectors.yaml")
	defer func() { end(answerOrFailure(err)) }()
	err = s.pool.QueryRow(ctx, `
		select yaml, sha256
		  from telflo_opamp.publications
		 where organization_id = $1 and id = $2`,
		organizationID, ref.ID).Scan(&y.YAML, &y.SHA256)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return YAMLResult{}, ErrNotFound
	case err != nil:
		return YAMLResult{}, fmt.Errorf("collectors: yaml: %w", err)
	}
	return y, nil
}

// update builds Report's one statement: each column set, and guarded by
// `is distinct from`, so the update matches no row when every value is as
// stored.
type update struct {
	sets, guards []string
	args         []any
}

func (u *update) set(column, cast string, value any) {
	u.args = append(u.args, value)
	parameter := fmt.Sprintf("$%d::%s", len(u.args), cast)
	u.sets = append(u.sets, column+" = "+parameter)
	u.guards = append(u.guards, column+" is distinct from "+parameter)
}

// Report writes the parts in one update.
func (s *Postgres) Report(ctx context.Context, r Report) (written bool, err error) {
	if err := r.validate(); err != nil {
		return false, err
	}
	u := update{args: []any{r.OrganizationID, r.FleetID, r.CollectorID}}
	if r.Served != nil {
		u.set("served_publication_id", "uuid", r.Served.Ref.column())
		u.set("served_sha256", "text", r.Served.SHA256)
		u.set("served_at", "timestamptz", at(r.Served.At))
		u.set("served_variables_stamp", "bigint", r.Served.VariablesStamp)
	}
	if r.Reconciled != nil {
		u.set("served_publication_id", "uuid", r.Reconciled.Ref.column())
		u.set("served_sha256", "text", r.Reconciled.SHA256)
		u.set("served_variables_stamp", "bigint", r.Reconciled.VariablesStamp)
		// served_at only when it was never set: a collector found running the
		// bytes it was never served has them from At on.
		u.args = append(u.args, at(r.Reconciled.At))
		u.sets = append(u.sets, fmt.Sprintf("served_at = coalesce(served_at, $%d::timestamptz)", len(u.args)))
		u.guards = append(u.guards, "served_at is null")
	}
	if r.Effective != nil {
		u.set("effective_sha256", "text", r.Effective.SHA256)
		u.set("effective_status", "text", string(r.Effective.Status))
		u.set("effective_error", "text", r.Effective.effectiveError())
		u.set("effective_at", "timestamptz", at(r.Effective.At))
	}
	if r.Reported.CollectorVersion != nil {
		u.set("reported_collector_version", "text", *r.Reported.CollectorVersion)
	}
	if r.Reported.IP != nil {
		u.set("reported_ip", "text", *r.Reported.IP)
	}
	if r.Reported.HostIP != nil {
		u.set("reported_host_ip", "text", *r.Reported.HostIP)
	}
	if r.Reported.Attributes != nil {
		encoded, err := json.Marshal(r.Reported.Attributes)
		if err != nil {
			return false, invalid("reported attributes")
		}
		u.set("reported_attributes", "jsonb", string(encoded))
	}
	if r.FullStateRequestedAt != nil {
		u.set("full_state_requested_at", "timestamptz", at(*r.FullStateRequestedAt))
	}
	if r.VariablesStamp != nil {
		// The marker alone: guarded like every part, so an unchanged poll
		// still writes nothing.
		u.set("served_variables_stamp", "bigint", *r.VariablesStamp)
	}
	if len(u.sets) == 0 {
		return false, nil
	}

	ctx, end := telemetry.StartDBSpan(ctx, "collectors.report")
	defer func() { end(answerOrFailure(err)) }()
	tag, err := s.pool.Exec(ctx, `
		update telflo_opamp.collector_instances
		   set `+strings.Join(u.sets, ", ")+`
		 where organization_id = $1 and fleet_id = $2 and id = $3
		   and (`+strings.Join(u.guards, " or ")+`)`,
		u.args...)
	switch {
	case isViolation(err, foreignKeyViolation, servedPublicationKey):
		return false, ErrPublicationNotFound
	case isViolation(err, checkViolation, attributesCheck):
		return false, invalid("reported attributes")
	case err != nil:
		return false, fmt.Errorf("collectors: report: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RecordError inserts the row and trims the history, in one transaction,
// after the organization's and the fleet's locks (lockFleet) and the
// collector's row `for no key update`: two RecordErrors of one collector
// serialize there, so each one's insert and trim see the other's rows, and
// the history never holds more than MaxErrors (and a OncePerHash row is
// written once).
func (s *Postgres) RecordError(ctx context.Context, organizationID, fleetID, collectorID uuid.UUID, e ErrorReport) (err error) {
	message, err := e.validate()
	if err != nil {
		return err
	}
	ctx, end := telemetry.StartDBSpan(ctx, "collectors.record_error")
	defer func() { end(answerOrFailure(err)) }()
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if ok, err := lockFleet(ctx, tx, organizationID, fleetID, false); err != nil || !ok {
			if err == nil {
				err = ErrNotFound
			}
			return err
		}
		err := tx.QueryRow(ctx, `
			select 1
			  from telflo_opamp.collector_instances
			 where organization_id = $1 and fleet_id = $2 and id = $3
			   for no key update`,
			organizationID, fleetID, collectorID).Scan(new(int))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		// With OncePerHash, a row of the same hash and message already in
		// the history (under the collector's lock above, so a racing poll
		// sees this one's) writes nothing.
		if _, err := tx.Exec(ctx, `
			insert into telflo_opamp.collector_errors
				(id, organization_id, collector_instance_id, reported_at, config_sha256, message)
			select $3, $1, $2, $4, $5, $6
			 where not $7::boolean
			    or not exists (
			         select 1
			           from telflo_opamp.collector_errors x
			          where x.organization_id = $1 and x.collector_instance_id = $2
			            and x.config_sha256 = $5 and x.message = $6)`,
			organizationID, collectorID, uuid.New(), at(e.At), e.ConfigSHA256, message, e.OncePerHash); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			delete from telflo_opamp.collector_errors e
			 where e.organization_id = $1 and e.collector_instance_id = $2
			   and e.id in (
			     select x.id
			       from telflo_opamp.collector_errors x
			      where x.organization_id = $1 and x.collector_instance_id = $2
			      order by x.reported_at desc, x.id desc
			     offset $3)`,
			organizationID, collectorID, MaxErrors)
		return err
	})
	switch {
	case isAnswer(err):
		return err
	case isViolation(err, foreignKeyViolation, errorCollectorForeignKey):
		// The collector was deleted between the insert's read and its check.
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("collectors: record error: %w", err)
	}
	return nil
}

// ListErrors returns a page of the collector's errors, newest first, by
// keyset on (reported_at, id).
func (s *Postgres) ListErrors(ctx context.Context, organizationID, fleetID, collectorID uuid.UUID, limit int, after *ErrorCursor) (page ErrorPage, err error) {
	if limit < 1 || limit > MaxListLimit {
		return ErrorPage{}, invalid("limit")
	}
	ctx, end := telemetry.StartDBSpan(ctx, "collectors.list_errors")
	defer func() { end(answerOrFailure(err)) }()
	if err := s.collectorExists(ctx, s.pool, organizationID, fleetID, collectorID); err != nil {
		return ErrorPage{}, err
	}
	query := `
		select id, collector_instance_id, reported_at, config_sha256, message
		  from telflo_opamp.collector_errors
		 where organization_id = $1 and collector_instance_id = $2`
	args := []any{organizationID, collectorID, limit + 1}
	if after != nil {
		args = append(args, after.ReportedAt, after.ID)
		query += ` and (reported_at, id) < ($4, $5)`
	}
	query += ` order by reported_at desc, id desc limit $3`
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return ErrorPage{}, fmt.Errorf("collectors: list errors: %w", err)
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (CollectorError, error) {
		var e CollectorError
		err := row.Scan(&e.ID, &e.CollectorID, &e.ReportedAt, &e.ConfigSHA256, &e.Message)
		e.ReportedAt = e.ReportedAt.UTC()
		return e, err
	})
	if err != nil {
		return ErrorPage{}, fmt.Errorf("collectors: list errors: %w", err)
	}
	page = ErrorPage{Errors: found}
	if len(found) > limit {
		page.Errors = found[:limit]
		last := found[limit-1]
		page.Next = &ErrorCursor{ReportedAt: last.ReportedAt, ID: last.ID}
	}
	if page.Errors == nil {
		page.Errors = []CollectorError{}
	}
	return page, nil
}

// collectorExists answers ErrNotFound unless the organization's fleet holds
// the collector.
func (s *Postgres) collectorExists(ctx context.Context, q queryer, organizationID, fleetID, id uuid.UUID) error {
	var exists bool
	if err := q.QueryRow(ctx, `
		select exists (
		  select 1
		    from telflo_opamp.collector_instances
		   where organization_id = $1 and fleet_id = $2 and id = $3)`,
		organizationID, fleetID, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Cleanup deletes expired error rows CleanupBatchSize at a time, each batch
// a transaction that first takes the advisory lock with
// pg_try_advisory_xact_lock (released at its end); it stops at the first
// batch that can't take it, or that deletes less than a batch. It reads
// every organization's rows: the cleanup is the server's, not an
// organization's (the index on (organization_id, reported_at) serves it by
// a skip scan).
func (s *Postgres) Cleanup(ctx context.Context, cutoff time.Time) (result CleanupResult, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "collectors.cleanup")
	defer func() { end(err) }()
	cutoff = at(cutoff)
	for {
		var held bool
		var deleted int64
		err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `select pg_try_advisory_xact_lock($1)`, CleanupLockKey).Scan(&held); err != nil || !held {
				return err
			}
			tag, err := tx.Exec(ctx, `
				delete from telflo_opamp.collector_errors
				 where ctid = any (array (
				         select ctid
				           from telflo_opamp.collector_errors
				          where reported_at < $1
				          limit $2))`,
				cutoff, CleanupBatchSize)
			deleted = tag.RowsAffected()
			return err
		})
		if err != nil {
			return result, fmt.Errorf("collectors: cleanup: %w", err)
		}
		if !held {
			return result, nil
		}
		result.Held = true
		result.Deleted += int(deleted)
		if deleted < CleanupBatchSize {
			return result, nil
		}
	}
}

// List returns a page of the fleet's collectors in o's order, by keyset on
// (the sort's column, id), in one statement: the page first (its common
// table expression), then what resolves for each and its wanted stamp
// (pageWanted, over the page's collectors only). An empty first page is
// checked to be the organization's fleet's.
func (s *Postgres) List(ctx context.Context, organizationID, fleetID uuid.UUID, o ListOptions) (page Page, err error) {
	if err := checkOptions(o); err != nil {
		return Page{}, err
	}
	ctx, end := telemetry.StartDBSpan(ctx, "collectors.list")
	defer func() { end(answerOrFailure(err)) }()

	query := `
		with page as (
		  select c.*
		    from telflo_opamp.collector_instances c
		   where c.organization_id = $1 and c.fleet_id = $2`
	// One row more than the page, to know whether another page follows.
	args := []any{organizationID, fleetID, o.Limit + 1}
	if o.GroupID != nil {
		args = append(args, *o.GroupID)
		query += fmt.Sprintf(` and c.collector_group_id = $%d`, len(args))
	}
	if o.Q != "" {
		args = append(args, likePattern(o.Q))
		n := len(args)
		query += fmt.Sprintf(` and (c.name ilike $%[1]d escape '\' or c.reported_ip ilike $%[1]d escape '\'
			or c.reported_host_ip ilike $%[1]d escape '\')`, n)
	}
	key, cast := "c.created_at", "timestamptz"
	switch o.Sort {
	case SortLastSeen:
		key = "c.last_seen_at"
	case SortName:
		key, cast = `c.name collate "C"`, `text collate "C"`
	}
	direction, comparison := "asc", ">"
	if o.Order == Descending {
		direction, comparison = "desc", "<"
	}
	if o.After != nil {
		var value any = o.After.Time
		if o.Sort == SortName {
			value = o.After.Name
		}
		args = append(args, value, o.After.ID)
		query += fmt.Sprintf(` and (%s, c.id) %s ($%d::%s, $%d::uuid)`, key, comparison, len(args)-1, cast, len(args))
	}
	order := fmt.Sprintf(` order by %[1]s %[2]s, c.id %[2]s`, key, direction)
	query += order + ` limit $3
		), ` + pageWanted.combos + `
		select ` + selectList("c", false) + `,` + resolutionColumns("c") + `,` + pageWanted.stamp + `
		  from page c` + resolutionJoins("c") + pageWanted.join + `
		` + order

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return Page{}, fmt.Errorf("collectors: list: %w", err)
	}
	found, err := pgx.CollectRows(rows, scanListed)
	if err != nil {
		return Page{}, fmt.Errorf("collectors: list: %w", err)
	}
	page = Page{Collectors: found}
	if len(found) > o.Limit {
		page.Collectors = found[:o.Limit]
		page.Next = cursorOf(found[o.Limit-1], o)
	}
	if len(page.Collectors) == 0 {
		page.Collectors = []Listed{}
		if o.After == nil {
			if err := s.fleetExists(ctx, organizationID, fleetID); err != nil {
				return Page{}, err
			}
		}
	}
	return page, nil
}

// fleetExists answers ErrFleetNotFound unless the organization holds the
// fleet.
func (s *Postgres) fleetExists(ctx context.Context, organizationID, fleetID uuid.UUID) error {
	var exists bool
	if err := s.pool.QueryRow(ctx, `
		select exists (
		  select 1 from telflo_opamp.fleets where organization_id = $1 and id = $2)`,
		organizationID, fleetID).Scan(&exists); err != nil {
		return fmt.Errorf("collectors: fleet exists: %w", err)
	}
	if !exists {
		return ErrFleetNotFound
	}
	return nil
}

// Get returns the collector with what resolves for it.
func (s *Postgres) Get(ctx context.Context, organizationID, fleetID, id uuid.UUID) (l Listed, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "collectors.get")
	defer func() { end(answerOrFailure(err)) }()
	l, err = getListed(ctx, s.pool, organizationID, fleetID, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return Listed{}, ErrNotFound
	case err != nil:
		return Listed{}, fmt.Errorf("collectors: get: %w", err)
	}
	return l, nil
}

// rowsQueryer is what getListed reads through: the pool, or a transaction.
type rowsQueryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// getStatement reads one collector with what resolves for it and its
// wanted stamp (oneWanted).
var getStatement = `
	with ` + oneWanted.combos + `
	select ` + selectList("c", false) + `,` + resolutionColumns("c") + `,` + oneWanted.stamp + `
	  from telflo_opamp.collector_instances c` + resolutionJoins("c") + oneWanted.join + `
	 where c.organization_id = $1 and c.fleet_id = $2 and c.id = $3`

// getListed reads the fleet's collector with what resolves for it, or
// ErrNotFound.
func getListed(ctx context.Context, q rowsQueryer, organizationID, fleetID, id uuid.UUID) (Listed, error) {
	rows, err := q.Query(ctx, getStatement, organizationID, fleetID, id)
	if err != nil {
		return Listed{}, err
	}
	l, err := pgx.CollectExactlyOneRow(rows, scanListed)
	if errors.Is(err, pgx.ErrNoRows) {
		return Listed{}, ErrNotFound
	}
	return l, err
}

// Rename renames in one statement, which matches nothing when another of
// the organization's collectors has the name: a name taken is found without
// raising the unique key's violation, whose DETAIL Postgres would write to
// its log with the name in it. Matching nothing, the collector's existence
// tells not found from taken. Only a rename racing another's write of the
// name (after this statement's snapshot) reaches the unique key, still
// answered ErrNameTaken.
func (s *Postgres) Rename(ctx context.Context, organizationID, fleetID, id uuid.UUID, name string, now time.Time) (c Collector, err error) {
	if !ValidName(name) {
		return Collector{}, invalid("name")
	}
	ctx, end := telemetry.StartDBSpan(ctx, "collectors.rename")
	defer func() { end(answerOrFailure(err)) }()
	rows, err := s.pool.Query(ctx, `
		update telflo_opamp.collector_instances
		   set name = $4,
		       updated_at = case when name is distinct from $4 then $5 else updated_at end
		 where organization_id = $1 and fleet_id = $2 and id = $3
		   and not exists (
		         select 1
		           from telflo_opamp.collector_instances x
		          where x.organization_id = $1 and x.name = $4 and x.id <> $3)
		returning `+strings.Join(returningColumns(), ", "),
		organizationID, fleetID, id, name, at(now))
	if err != nil {
		return Collector{}, fmt.Errorf("collectors: rename: %w", err)
	}
	c, err = pgx.CollectExactlyOneRow(rows, scanRow)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if err := s.collectorExists(ctx, s.pool, organizationID, fleetID, id); err != nil {
			if errors.Is(err, ErrNotFound) {
				return Collector{}, ErrNotFound
			}
			return Collector{}, fmt.Errorf("collectors: rename: %w", err)
		}
		return Collector{}, ErrNameTaken
	case isViolation(err, uniqueViolation, nameUniqueKey):
		return Collector{}, ErrNameTaken
	case err != nil:
		return Collector{}, fmt.Errorf("collectors: rename: %w", err)
	}
	return c, nil
}

// returningColumns is rowColumns as an update's returning list.
func returningColumns() []string {
	out := slices.Clone(rowColumns)
	for i, column := range out {
		if column == "reported_attributes" {
			out[i] = "reported_attributes::text"
		}
	}
	return out
}

// bindings is what a collector's write reads of its row, under the locks it
// holds: its group, that group's variable group and its setting's stamp,
// and its pinned variable group with its stamp.
type bindings struct {
	groupID             uuid.UUID
	groupVariableGroup  *uuid.UUID
	groupStamp          *int64
	pinnedVariableGroup *uuid.UUID
	pinnedStamp         *int64
}

// readBindings reads the fleet's collector's bindings, or answers
// ErrNotFound. Under the organization's row `for no key update` they can't
// change before the transaction ends: every write of a binding takes it
// first.
func readBindings(ctx context.Context, tx pgx.Tx, organizationID, fleetID, id uuid.UUID) (bindings, error) {
	var b bindings
	err := tx.QueryRow(ctx, `
		select c.collector_group_id, g.variable_group_id, g.variable_group_stamp,
		       c.pinned_variable_group_id, c.pinned_variable_group_stamp
		  from telflo_opamp.collector_instances c
		  join telflo_opamp.collector_groups g
		    on g.organization_id = c.organization_id and g.fleet_id = c.fleet_id and g.id = c.collector_group_id
		 where c.organization_id = $1 and c.fleet_id = $2 and c.id = $3`,
		organizationID, fleetID, id).Scan(&b.groupID, &b.groupVariableGroup, &b.groupStamp,
		&b.pinnedVariableGroup, &b.pinnedStamp)
	if errors.Is(err, pgx.ErrNoRows) {
		return bindings{}, ErrNotFound
	}
	return b, err
}

// targetGroup takes the fleet's group groupID `for key share`, a move's
// target, and answers its variable group, or ErrGroupNotFound.
func targetGroup(ctx context.Context, tx pgx.Tx, organizationID, fleetID, groupID uuid.UUID) (*uuid.UUID, error) {
	var variableGroup *uuid.UUID
	err := tx.QueryRow(ctx, `
		select variable_group_id
		  from telflo_opamp.collector_groups
		 where organization_id = $1 and fleet_id = $2 and id = $3
		   for key share`,
		organizationID, fleetID, groupID).Scan(&variableGroup)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrGroupNotFound
	}
	return variableGroup, err
}

// checkVariableGroup answers ErrVariableGroupNotFound unless the
// organization holds the variable group id (nil: none, nothing to check).
// It takes no lock: the caller holds the organization's row `for no key
// update`, which a variable group's delete takes first too; the pin's own
// foreign key is the backstop (wrapPin).
func checkVariableGroup(ctx context.Context, tx pgx.Tx, organizationID uuid.UUID, id *uuid.UUID) error {
	if id == nil {
		return nil
	}
	var exists bool
	if err := tx.QueryRow(ctx, `
		select exists (
		  select 1
		    from telflo_opamp.variable_groups
		   where organization_id = $1 and id = $2)`,
		organizationID, *id).Scan(&exists); err != nil || exists {
		return err
	}
	return ErrVariableGroupNotFound
}

// checkDefined answers *UndefinedError when ref's published version
// references a name no variable of the organization defines, under the
// organization's row the caller holds `for no key update` (a variable's
// delete or rename takes it first too, so neither can slip between this
// read and the commit). A nil ref (an unpin) checks nothing.
func checkDefined(ctx context.Context, tx pgx.Tx, organizationID uuid.UUID, ref *Ref) error {
	if ref == nil {
		return nil
	}
	names, err := variableuse.Undefined(ctx, tx, organizationID, ref.ID)
	if err != nil {
		return err
	}
	return undefined(names)
}

// bump moves the organization's counter on, under its row the caller
// holds, and answers the stamp.
func bump(ctx context.Context, tx pgx.Tx, organizationID uuid.UUID) (int64, error) {
	stamp, err := variableuse.Bump(ctx, tx, organizationID)
	if errors.Is(err, variableuse.ErrOrganizationNotFound) {
		// Unreachable: the organization's row is held.
		return 0, ErrNotFound
	}
	return stamp, err
}

// Move moves the collector in one transaction: the organization's row `for
// no key update` and the fleet's (lockFleet), the collector's bindings, the
// target group's `for key share`, the counter moved on when the two groups'
// variable groups differ (stampsCollector), else, into another group, the
// old group's setting's stamp carried (carriedStamp), then the update, the
// stamp written on the collector's row.
func (s *Postgres) Move(ctx context.Context, organizationID, fleetID, id, groupID uuid.UUID, now time.Time) (c Collector, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "collectors.move")
	defer func() { end(answerOrFailure(err)) }()
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if ok, err := lockFleet(ctx, tx, organizationID, fleetID, true); err != nil || !ok {
			if err == nil {
				err = ErrNotFound
			}
			return err
		}
		current, err := readBindings(ctx, tx, organizationID, fleetID, id)
		if err != nil {
			return err
		}
		targetVariableGroup, err := targetGroup(ctx, tx, organizationID, fleetID, groupID)
		if err != nil {
			return err
		}
		stamp := current.pinnedStamp
		if stampsCollector(current.groupID, current.groupVariableGroup, current.pinnedVariableGroup,
			groupID, targetVariableGroup, current.pinnedVariableGroup) {
			next, err := bump(ctx, tx, organizationID)
			if err != nil {
				return err
			}
			stamp = &next
		} else if groupID != current.groupID {
			stamp = carriedStamp(current.pinnedStamp, current.groupStamp)
		}
		rows, err := tx.Query(ctx, `
			update telflo_opamp.collector_instances
			   set collector_group_id = $4,
			       updated_at = case when collector_group_id is distinct from $4 then $5 else updated_at end,
			       pinned_variable_group_stamp = $6
			 where organization_id = $1 and fleet_id = $2 and id = $3
			returning `+strings.Join(returningColumns(), ", "),
			organizationID, fleetID, id, groupID, at(now), stamp)
		if err != nil {
			return err
		}
		c, err = pgx.CollectExactlyOneRow(rows, scanRow)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	switch {
	case isAnswer(err):
		return Collector{}, err
	case isViolation(err, foreignKeyViolation, groupForeignKey):
		return Collector{}, ErrGroupNotFound
	case err != nil:
		return Collector{}, fmt.Errorf("collectors: move: %w", err)
	}
	return c, nil
}

// checkRef answers ErrPublicationNotFound unless the organization holds
// ref's published version. It takes no lock (the runtime role can't lock
// publications, the table being immutable to it): the pin's own foreign
// key check key-shares the row, and its refusal is mapped to the same
// answer (wrapPin).
func checkRef(ctx context.Context, tx pgx.Tx, organizationID uuid.UUID, ref *Ref) error {
	if ref == nil {
		return nil
	}
	var exists bool
	if err := tx.QueryRow(ctx, `
		select exists (
		  select 1
		    from telflo_opamp.publications
		   where organization_id = $1 and id = $2)`,
		organizationID, ref.ID).Scan(&exists); err != nil || exists {
		return err
	}
	return ErrPublicationNotFound
}

// wrapPin maps a pin's refusals to their answers.
func wrapPin(operation string, err error) error {
	switch {
	case isAnswer(err):
		return err
	case isViolation(err, foreignKeyViolation, pinnedPublicationKey):
		return ErrPublicationNotFound
	case isViolation(err, foreignKeyViolation, pinnedVariableGroupKey):
		// A backstop: the variable group is checked under the
		// organization's row, which its delete takes too.
		return ErrVariableGroupNotFound
	case err != nil:
		return fmt.Errorf("collectors: %s: %w", operation, err)
	}
	return nil
}

// Pin sets or clears the pin in one transaction: the organization's row
// `for no key update` and the fleet's (lockFleet), the collector, the
// pin's target, its names defined (checkDefined), then the update.
func (s *Postgres) Pin(ctx context.Context, organizationID, fleetID, id uuid.UUID, ref *Ref, now time.Time) (c Collector, err error) {
	if ref != nil && !ref.valid() {
		return Collector{}, invalid("ref")
	}
	ctx, end := telemetry.StartDBSpan(ctx, "collectors.pin")
	defer func() { end(answerOrFailure(err)) }()
	publication := ref.column()
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if ok, err := lockFleet(ctx, tx, organizationID, fleetID, true); err != nil || !ok {
			if err == nil {
				err = ErrNotFound
			}
			return err
		}
		if err := s.collectorExists(ctx, tx, organizationID, fleetID, id); err != nil {
			return err
		}
		if err := checkRef(ctx, tx, organizationID, ref); err != nil {
			return err
		}
		if err := checkDefined(ctx, tx, organizationID, ref); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			update telflo_opamp.collector_instances
			   set pinned_publication_id = $4,
			       updated_at = case when pinned_publication_id is distinct from $4 then $5
			                         else updated_at end
			 where organization_id = $1 and fleet_id = $2 and id = $3
			returning `+strings.Join(returningColumns(), ", "),
			organizationID, fleetID, id, publication, at(now))
		if err != nil {
			return err
		}
		c, err = pgx.CollectExactlyOneRow(rows, scanRow)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	if err := wrapPin("pin", err); err != nil {
		return Collector{}, err
	}
	return c, nil
}

// Update applies the changes in one transaction: the organization's row
// first, `for no key update` when the changes pin, move or set a pinned
// variable group (Changes.locksOrganization, chosen before the first lock
// and never upgraded: the lock order on Postgres), else `for key share`,
// and the fleet's (lockFleet); the collector's bindings; the group's `for
// key share` when it moves; the pin's target; the pinned variable group;
// the pin's names defined; the counter moved on when the pinned variable
// group changes or the move is between groups whose variable groups differ
// (stampsCollector), else, into another group, the old group's setting's
// stamp carried (carriedStamp); then one update of every column changed,
// and the collector read back with what resolves for it. A new name another
// of the organization's collectors has makes the update match nothing
// (found without raising the unique key's violation, whose DETAIL Postgres
// would log with the name); only a race with another's write of the name
// reaches the unique key, still ErrNameTaken. A refusal after the counter
// moved rolls it back with the rest.
func (s *Postgres) Update(ctx context.Context, organizationID, fleetID, id uuid.UUID, c Changes, now time.Time) (l Listed, err error) {
	if err := c.validate(); err != nil {
		return Listed{}, err
	}
	ctx, end := telemetry.StartDBSpan(ctx, "collectors.update")
	defer func() { end(answerOrFailure(err)) }()
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if ok, err := lockFleet(ctx, tx, organizationID, fleetID, c.locksOrganization()); err != nil || !ok {
			if err == nil {
				err = ErrNotFound
			}
			return err
		}
		current, err := readBindings(ctx, tx, organizationID, fleetID, id)
		if err != nil {
			return err
		}
		nextGroup, nextGroupVariableGroup := current.groupID, current.groupVariableGroup
		if c.GroupID != nil {
			if nextGroupVariableGroup, err = targetGroup(ctx, tx, organizationID, fleetID, *c.GroupID); err != nil {
				return err
			}
			nextGroup = *c.GroupID
		}
		if c.Pin != nil {
			if err := checkRef(ctx, tx, organizationID, c.Pin.Ref); err != nil {
				return err
			}
		}
		nextPinned := current.pinnedVariableGroup
		if c.PinnedVariableGroup != nil {
			if err := checkVariableGroup(ctx, tx, organizationID, c.PinnedVariableGroup.ID); err != nil {
				return err
			}
			nextPinned = c.PinnedVariableGroup.ID
		}
		if c.Pin != nil {
			if err := checkDefined(ctx, tx, organizationID, c.Pin.Ref); err != nil {
				return err
			}
		}

		args := []any{organizationID, fleetID, id}
		var sets, differs []string
		set := func(column string, value any) {
			args = append(args, value)
			sets = append(sets, fmt.Sprintf("%s = $%d", column, len(args)))
			differs = append(differs, fmt.Sprintf("%s is distinct from $%d", column, len(args)))
		}
		if c.Name != nil {
			set("name", *c.Name) // $4, which the name's guard below names
		}
		if c.GroupID != nil {
			set("collector_group_id", *c.GroupID)
		}
		if c.Pin != nil {
			set("pinned_publication_id", c.Pin.Ref.column())
		}
		if c.PinnedVariableGroup != nil {
			set("pinned_variable_group_id", c.PinnedVariableGroup.ID)
		}
		if stampsCollector(current.groupID, current.groupVariableGroup, current.pinnedVariableGroup,
			nextGroup, nextGroupVariableGroup, nextPinned) {
			stamp, err := bump(ctx, tx, organizationID)
			if err != nil {
				return err
			}
			// What it stamps changed, so updated_at moves by that column.
			args = append(args, stamp)
			sets = append(sets, fmt.Sprintf("pinned_variable_group_stamp = $%d", len(args)))
		} else if nextGroup != current.groupID {
			// A move between groups of the same variable group, or none: the
			// old group's setting's stamp carried, no bump (carriedStamp).
			args = append(args, carriedStamp(current.pinnedStamp, current.groupStamp))
			sets = append(sets, fmt.Sprintf("pinned_variable_group_stamp = $%d", len(args)))
		}
		args = append(args, at(now))
		statement := fmt.Sprintf(`
			update telflo_opamp.collector_instances
			   set %s,
			       updated_at = case when %s then $%d else updated_at end
			 where organization_id = $1 and fleet_id = $2 and id = $3`,
			strings.Join(sets, ", "), strings.Join(differs, " or "), len(args))
		if c.Name != nil {
			statement += `
			   and not exists (
			         select 1
			           from telflo_opamp.collector_instances x
			          where x.organization_id = $1 and x.name = $4 and x.id <> $3)`
		}
		tag, err := tx.Exec(ctx, statement, args...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 && c.Name != nil {
			// The name is taken, unless the collector went meanwhile.
			if err := s.collectorExists(ctx, tx, organizationID, fleetID, id); err != nil {
				return err
			}
			return ErrNameTaken
		}
		l, err = getListed(ctx, tx, organizationID, fleetID, id)
		return err
	})
	switch {
	case isViolation(err, uniqueViolation, nameUniqueKey):
		return Listed{}, ErrNameTaken
	case isViolation(err, foreignKeyViolation, groupForeignKey):
		return Listed{}, ErrGroupNotFound
	}
	if err := wrapPin("update", err); err != nil {
		return Listed{}, err
	}
	return l, nil
}

// BulkPin changes the matching collectors in one transaction: the
// organization's row `for no key update` always (it names a pin, a
// variable group or both) and the fleet's (lockFleet); the pin's target,
// the variable group and the pin's names defined, once for the call; the
// matching collectors `for no key update` in id order (so two BulkPins
// can't deadlock); the counter moved on once when any of them changes its
// pinned variable group; then one update of those that differ, the stamp
// on each whose pinned variable group changes.
func (s *Postgres) BulkPin(ctx context.Context, organizationID, fleetID uuid.UUID, ids []uuid.UUID, pin *PinChange, vg *VariableGroupChange, now time.Time) (result BulkPinResult, err error) {
	if err := validBulkPin(ids, pin, vg); err != nil {
		return BulkPinResult{}, err
	}
	ctx, end := telemetry.StartDBSpan(ctx, "collectors.bulk_pin")
	defer func() { end(answerOrFailure(err)) }()
	wanted := sortedIDs(ids)
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if ok, err := lockFleet(ctx, tx, organizationID, fleetID, true); err != nil || !ok {
			if err == nil {
				err = ErrFleetNotFound
			}
			return err
		}
		if pin != nil {
			if err := checkRef(ctx, tx, organizationID, pin.Ref); err != nil {
				return err
			}
		}
		if vg != nil {
			if err := checkVariableGroup(ctx, tx, organizationID, vg.ID); err != nil {
				return err
			}
		}
		if pin != nil {
			if err := checkDefined(ctx, tx, organizationID, pin.Ref); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, `
			select id, pinned_variable_group_id
			  from telflo_opamp.collector_instances
			 where organization_id = $1 and fleet_id = $2 and id = any ($3)
			 order by id
			   for no key update`,
			organizationID, fleetID, wanted)
		if err != nil {
			return err
		}
		type matchedRow struct {
			id            uuid.UUID
			variableGroup *uuid.UUID
		}
		found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (matchedRow, error) {
			var m matchedRow
			return m, row.Scan(&m.id, &m.variableGroup)
		})
		if err != nil {
			return err
		}
		matched := make([]uuid.UUID, len(found))
		restamp := false
		for i, m := range found {
			matched[i] = m.id
			restamp = restamp || (vg != nil && !sameID(m.variableGroup, vg.ID))
		}

		args := []any{organizationID, fleetID, matched, at(now)}
		var sets, differs []string
		if pin != nil {
			args = append(args, pin.Ref.column())
			sets = append(sets, fmt.Sprintf("pinned_publication_id = $%d", len(args)))
			differs = append(differs, fmt.Sprintf("pinned_publication_id is distinct from $%d", len(args)))
		}
		if vg != nil {
			args = append(args, vg.ID)
			n := len(args)
			if restamp {
				stamp, err := bump(ctx, tx, organizationID)
				if err != nil {
					return err
				}
				args = append(args, stamp)
				// Every set reads the row as it was before the update, so
				// this compares the old pinned variable group.
				sets = append(sets, fmt.Sprintf(`pinned_variable_group_stamp = case
				           when pinned_variable_group_id is distinct from $%d then $%d
				           else pinned_variable_group_stamp end`, n, len(args)))
			}
			sets = append(sets, fmt.Sprintf("pinned_variable_group_id = $%d", n))
			differs = append(differs, fmt.Sprintf("pinned_variable_group_id is distinct from $%d", n))
		}
		tag, err := tx.Exec(ctx, fmt.Sprintf(`
			update telflo_opamp.collector_instances
			   set %s, updated_at = $4
			 where organization_id = $1 and fleet_id = $2 and id = any ($3)
			   and (%s)`, strings.Join(sets, ", "), strings.Join(differs, " or ")),
			args...)
		if err != nil {
			return err
		}
		result = BulkPinResult{Updated: sortedIDs(matched), Changed: int(tag.RowsAffected())}
		return nil
	})
	if err := wrapPin("bulk pin", err); err != nil {
		return BulkPinResult{}, err
	}
	result.NotFound = missing(wanted, result.Updated)
	return result, nil
}

// missing is the ids of wanted not in found, both sorted.
func missing(wanted, found []uuid.UUID) []uuid.UUID {
	out := []uuid.UUID{}
	for _, id := range wanted {
		if _, ok := slices.BinarySearchFunc(found, id, compareIDs); !ok {
			out = append(out, id)
		}
	}
	return out
}

// Delete deletes the collector, after the organization's and the fleet's
// locks (lockFleet); its errors go by the cascade.
func (s *Postgres) Delete(ctx context.Context, organizationID, fleetID, id uuid.UUID) (deleted bool, err error) {
	ctx, end := telemetry.StartDBSpan(ctx, "collectors.delete")
	defer func() { end(err) }()
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if ok, err := lockFleet(ctx, tx, organizationID, fleetID, false); err != nil || !ok {
			return err
		}
		tag, err := tx.Exec(ctx, `
			delete from telflo_opamp.collector_instances
			 where organization_id = $1 and fleet_id = $2 and id = $3`,
			organizationID, fleetID, id)
		deleted = tag.RowsAffected() == 1
		return err
	})
	if err != nil {
		return false, fmt.Errorf("collectors: delete: %w", err)
	}
	return deleted, nil
}

// summariesStatement counts each fleet's collectors in one grouped query,
// classifying each by LivenessAt's edges and SyncOf's steps, in their
// order (the served published version against the resolved one, and the
// served-variables marker against the wanted stamp, fleetsWanted), and
// takes their latest last_seen_at (no index holds last_seen_at, so
// heartbeats stay HOT updates; the fleet's rows are read for the counts
// anyway). Its rows come in the fleets' id order, which
// TestSummariesCountAKnownFixture leans on to put a fleet without
// collectors after one with them.
//
//	$1 organization, $2 the fleets, $3 to $5 now less OnlineWithin,
//	WarningWithin and UnknownWithin
var summariesStatement = `
	with ` + fleetsWanted.combos + `, classified as (
	  select c.fleet_id, c.last_seen_at,
	         case when c.last_seen_at > $3 then 'online'
	              when c.last_seen_at > $4 then 'warning'
	              when c.last_seen_at > $5 then 'unknown'
	              else 'offline' end as liveness,
	         case when coalesce(pp.sha256, gp.sha256) is null then 'unassigned'
	              when c.served_publication_id is distinct from coalesce(pp.id, gp.id)
	                or coalesce(c.served_variables_stamp, 0) <` + fleetsWanted.stamp + `
	                or c.served_at is null or c.effective_at is null
	                or c.effective_at < c.served_at then 'pending'
	              when c.effective_status = 'failed' then 'failed'
	              when c.effective_status = 'applied' and c.effective_sha256 = c.served_sha256 then 'in_sync'
	              when c.effective_status = 'applied' then 'drifted'
	              else 'pending' end as sync
	    from telflo_opamp.collector_instances c` + resolutionJoins("c") + fleetsWanted.join + `
	   where c.organization_id = $1 and c.fleet_id = any ($2)
	)
	select f.id,
	       count(k.fleet_id),
	       count(*) filter (where k.liveness = 'online'),
	       count(*) filter (where k.liveness = 'warning'),
	       count(*) filter (where k.liveness = 'unknown'),
	       count(*) filter (where k.liveness = 'offline'),
	       count(*) filter (where k.sync = 'in_sync'),
	       count(*) filter (where k.sync = 'pending'),
	       count(*) filter (where k.sync = 'failed'),
	       count(*) filter (where k.sync = 'drifted'),
	       count(*) filter (where k.sync = 'unassigned'),
	       max(k.last_seen_at)
	  from telflo_opamp.fleets f
	  left join classified k on k.fleet_id = f.id
	 where f.organization_id = $1 and f.id = any ($2)
	 group by f.id
	 order by f.id`

// SummariesForFleets runs summariesStatement.
func (s *Postgres) SummariesForFleets(ctx context.Context, organizationID uuid.UUID, fleetIDs []uuid.UUID, now time.Time) (summaries map[uuid.UUID]Summary, err error) {
	if len(fleetIDs) > MaxSummaryFleets {
		return nil, invalid("fleet ids")
	}
	summaries = map[uuid.UUID]Summary{}
	if len(fleetIDs) == 0 {
		return summaries, nil
	}
	ctx, end := telemetry.StartDBSpan(ctx, "collectors.summaries")
	defer func() { end(err) }()
	now = now.UTC()
	rows, err := s.pool.Query(ctx, summariesStatement, organizationID, sortedIDs(fleetIDs),
		now.Add(-OnlineWithin), now.Add(-WarningWithin), now.Add(-UnknownWithin))
	if err != nil {
		return nil, fmt.Errorf("collectors: summaries: %w", err)
	}
	// Each row scans into a fresh Summary: a fleet without collectors (a null
	// last seen) never keeps the LastSeenAt of the fleet before it.
	type fleetSummary struct {
		id uuid.UUID
		m  Summary
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (fleetSummary, error) {
		var f fleetSummary
		var lastSeen *time.Time
		if err := row.Scan(&f.id, &f.m.Collectors, &f.m.Online, &f.m.Warning, &f.m.Unknown, &f.m.Offline,
			&f.m.InSync, &f.m.Pending, &f.m.Failed, &f.m.Drifted, &f.m.Unassigned, &lastSeen); err != nil {
			return f, err
		}
		if lastSeen != nil {
			v := lastSeen.UTC()
			f.m.LastSeenAt = &v
		}
		return f, nil
	})
	if err != nil {
		return nil, fmt.Errorf("collectors: summaries: %w", err)
	}
	for _, f := range found {
		summaries[f.id] = f.m
	}
	return summaries, nil
}

// lockFleet takes the organization's row, then its fleet's row `for key
// share`: the first locks of a transaction that then locks, inserts into or
// deletes rows the fleet owns (the lock order on Postgres). An
// organization's delete holds its row and cascades into the fleet and
// everything else it holds; a transaction that took the fleet first and the
// organization only at a foreign key's check deadlocked with it (40P01).
// The organization's row is taken `for no key update`
// (variableuse.LockOrganization) when binding, for a write that pins, moves
// or sets a pinned variable group, else `for key share`; the caller chooses
// from the request, before this first lock, and never takes the row again
// in a stronger mode (an upgrade deadlocks; see the lock order on
// Postgres). It reports whether the organization holds the fleet.
func lockFleet(ctx context.Context, tx pgx.Tx, organizationID, fleetID uuid.UUID, binding bool) (bool, error) {
	if binding {
		found, err := variableuse.LockOrganization(ctx, tx, organizationID)
		if err != nil || !found {
			return false, err
		}
		return keyShareFleet(ctx, tx, organizationID, fleetID)
	}
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
	return keyShareFleet(ctx, tx, organizationID, fleetID)
}

// keyShareFleet takes the organization's fleet's row `for key share`, for a
// transaction that holds the organization's row already (Enroll), and
// reports whether the fleet is there.
func keyShareFleet(ctx context.Context, tx pgx.Tx, organizationID, fleetID uuid.UUID) (bool, error) {
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

// isViolation reports whether err is Postgres refusing with code on the
// constraint named.
func isViolation(err error, code, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code && pgErr.ConstraintName == constraint
}

// scanRow reads one row of rowColumns.
func scanRow(row pgx.CollectableRow) (Collector, error) {
	return scanCollector(row)
}

// scanListed reads one row of rowColumns, the resolution's columns and the
// wanted stamp.
func scanListed(row pgx.CollectableRow) (Listed, error) {
	resolution := resolutionScan{}
	var wanted int64
	c, err := scanCollector(row, append(resolution.dest(), &wanted)...)
	if err != nil {
		return Listed{}, err
	}
	l := Listed{Collector: c, Resolution: resolution.result()}
	if l.Resolution.From != FromNone {
		l.Resolution.VariablesStamp = wanted
	}
	return l, nil
}

// resolutionScan reads resolutionColumns.
type resolutionScan struct {
	from          string
	id            *uuid.UUID
	sha256        *string
	variableGroup *uuid.UUID
	bindingStamp  int64
}

func (r *resolutionScan) dest() []any {
	return []any{&r.from, &r.id, &r.sha256, &r.variableGroup, &r.bindingStamp}
}

func (r *resolutionScan) result() Resolution {
	resolution := Resolution{From: From(r.from)}
	if r.id != nil && r.sha256 != nil && resolution.From != FromNone {
		resolution.ID, resolution.SHA256 = *r.id, *r.sha256
	} else {
		resolution = Resolution{From: FromNone}
	}
	resolution.GroupVariableGroupID, resolution.GroupBindingStamp = r.variableGroup, r.bindingStamp
	return resolution
}

// scanCollector reads one row of rowColumns, then the trailing values,
// with its times in UTC.
func scanCollector(row pgx.CollectableRow, trailing ...any) (Collector, error) {
	var (
		c                  Collector
		pinned, served     *uuid.UUID
		status, attributes *string
	)
	dest := append([]any{&c.ID, &c.OrganizationID, &c.FleetID, &c.GroupID, &c.InstanceUID, &c.Name,
		&pinned, &served,
		&c.ServedSHA256, &c.ServedAt, &c.EffectiveSHA256, &status, &c.EffectiveError, &c.EffectiveAt,
		&c.FullStateRequestedAt, &c.ReportedCollectorVersion, &c.ReportedIP, &c.ReportedHostIP,
		&attributes, &c.EnrollmentTokenID,
		&c.PinnedVariableGroupID, &c.PinnedVariableGroupStamp, &c.ServedVariablesStamp,
		&c.LastSeenAt, &c.CreatedAt, &c.UpdatedAt}, trailing...)
	if err := row.Scan(dest...); err != nil {
		return Collector{}, err
	}
	c.Pinned = refOf(pinned)
	c.Served = refOf(served)
	if status != nil {
		s := EffectiveStatus(*status)
		c.EffectiveStatus = &s
	}
	if attributes != nil {
		if err := json.Unmarshal([]byte(*attributes), &c.ReportedAttributes); err != nil {
			return Collector{}, errors.New("a stored collector's attributes don't read back")
		}
	}
	for _, t := range []*time.Time{c.ServedAt, c.EffectiveAt, c.FullStateRequestedAt} {
		if t != nil {
			*t = t.UTC()
		}
	}
	c.LastSeenAt, c.CreatedAt, c.UpdatedAt = c.LastSeenAt.UTC(), c.CreatedAt.UTC(), c.UpdatedAt.UTC()
	return c, nil
}
