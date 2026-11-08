package variableuse

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The Postgres functions run in the caller's transaction, as telflo_opamp
// (the runtime role: select on the tables they read, update on
// organizations.variables_revision). Every statement names the schema and
// filters on organization_id first. Errors are wrapped with the operation's
// name only: no argument (a name is configuration text) reaches an error.

// LockOrganization takes the organization's row `for no key update`: the
// first lock of a transaction that changes what a collector's text is filled
// in with, or checks a guard against such a change (the lock order,
// organization first; a write chooses this mode before its first lock and
// never upgrades a `for key share` to it). It reports whether the
// organization is registered.
func LockOrganization(ctx context.Context, tx pgx.Tx, organizationID uuid.UUID) (bool, error) {
	err := tx.QueryRow(ctx, `
		select 1
		  from telflo_opamp.organizations
		 where id = $1
		   for no key update`,
		organizationID).Scan(new(int))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("variableuse: lock organization: %w", err)
	}
	return true, nil
}

// Bump moves the organization's counter on by one and answers its new value,
// the stamp of the change the transaction makes: one statement, under the
// row lock LockOrganization took, so stamps are distinct and in commit
// order.
func Bump(ctx context.Context, tx pgx.Tx, organizationID uuid.UUID) (int64, error) {
	var revision int64
	err := tx.QueryRow(ctx, `
		update telflo_opamp.organizations
		   set variables_revision = variables_revision + 1
		 where id = $1
		returning variables_revision`,
		organizationID).Scan(&revision)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, ErrOrganizationNotFound
	case err != nil:
		return 0, fmt.Errorf("variableuse: bump: %w", err)
	}
	return revision, nil
}

// Undefined answers the names the organization's published version
// publicationID references that no variable of the organization has, at most
// MaxListed in byte order, with the exact total, in one statement: the total
// is counted before the limit. A version published before migration 00012,
// or another organization's, has no names here, so none is undefined.
func Undefined(ctx context.Context, tx pgx.Tx, organizationID, publicationID uuid.UUID) (UndefinedNames, error) {
	rows, err := tx.Query(ctx, `
		select n.name, count(*) over ()
		  from telflo_opamp.publication_variable_names n
		 where n.organization_id = $1 and n.publication_id = $2
		   and not exists (
		         select 1
		           from telflo_opamp.variables v
		          where v.organization_id = $1 and v.name = n.name)
		 order by n.name collate "C"
		 limit $3`,
		organizationID, publicationID, MaxListed)
	if err != nil {
		return UndefinedNames{}, fmt.Errorf("variableuse: undefined: %w", err)
	}
	undefined := UndefinedNames{Names: []string{}}
	var name string
	var total int64
	if _, err := pgx.ForEachRow(rows, []any{&name, &total}, func() error {
		undefined.Names = append(undefined.Names, name)
		undefined.Total = int(total)
		return nil
	}); err != nil {
		return UndefinedNames{}, fmt.Errorf("variableuse: undefined: %w", err)
	}
	return undefined, nil
}

// UsedBy answers the organization's used published versions whose names hold
// name: used means a collector group is assigned it or a collector pinned to
// it, so an unassigned version doesn't count. In one statement: the totals
// over every use, and at most MaxListed entries (a group row, or a version
// pinned only) by publication, fleet and group, each with its version's
// exact pinned count.
func UsedBy(ctx context.Context, tx pgx.Tx, organizationID uuid.UUID, name string) (Uses, error) {
	rows, err := tx.Query(ctx, `
		with used as (
		  select n.publication_id as id,
		         (select count(*)
		            from telflo_opamp.collector_groups g
		           where g.organization_id = $1 and g.assigned_publication_id = n.publication_id) as groups,
		         (select count(*)
		            from telflo_opamp.collector_instances c
		           where c.organization_id = $1 and c.pinned_publication_id = n.publication_id) as pinned
		    from telflo_opamp.publication_variable_names n
		   where n.organization_id = $1 and n.name = $2
		), totals as (
		  select count(*) filter (where groups > 0 or pinned > 0) as publications,
		         coalesce(sum(groups), 0)::bigint as groups,
		         coalesce(sum(pinned), 0)::bigint as pinned
		    from used
		)
		select t.publications, t.groups, t.pinned, e.id, e.fleet_id, e.group_id, e.pinned
		  from totals t
		  left join lateral (
		         select u.id, g.fleet_id, g.id as group_id, u.pinned
		           from used u
		           left join telflo_opamp.collector_groups g
		             on g.organization_id = $1 and g.assigned_publication_id = u.id
		          where u.groups > 0 or u.pinned > 0
		          order by u.id, g.fleet_id, g.id
		          limit $3) e on true
		 order by e.id, e.fleet_id, e.group_id`,
		organizationID, name, MaxListed)
	if err != nil {
		return Uses{}, fmt.Errorf("variableuse: used by: %w", err)
	}
	uses := Uses{List: []Use{}}
	var (
		publications, groups, pinned int64
		id, fleetID, groupID         *uuid.UUID
		usePinned                    *int64
	)
	if _, err := pgx.ForEachRow(rows, []any{&publications, &groups, &pinned, &id, &fleetID, &groupID, &usePinned}, func() error {
		uses.Publications, uses.Groups, uses.PinnedCollectors = int(publications), int(groups), int(pinned)
		if id == nil {
			return nil // nothing uses it: the totals' row alone
		}
		if n := len(uses.List); n == 0 || uses.List[n-1].PublicationID != *id {
			uses.List = append(uses.List, Use{PublicationID: *id, Groups: []GroupRef{}, PinnedCollectors: int(*usePinned)})
		}
		if groupID != nil {
			last := &uses.List[len(uses.List)-1]
			last.Groups = append(last.Groups, GroupRef{FleetID: *fleetID, GroupID: *groupID})
		}
		return nil
	}); err != nil {
		return Uses{}, fmt.Errorf("variableuse: used by: %w", err)
	}
	return uses, nil
}

// GroupUse answers what uses the organization's variable group
// variableGroupID, for its delete: the collector groups bound to it, at most
// MaxListed by fleet and then group, with their exact total, and how many
// collectors it is pinned on, in one statement.
func GroupUse(ctx context.Context, tx pgx.Tx, organizationID, variableGroupID uuid.UUID) (GroupUses, error) {
	rows, err := tx.Query(ctx, `
		select t.groups, t.pinned, g.fleet_id, g.id
		  from (select (select count(*)
		                  from telflo_opamp.collector_groups g
		                 where g.organization_id = $1 and g.variable_group_id = $2) as groups,
		               (select count(*)
		                  from telflo_opamp.collector_instances c
		                 where c.organization_id = $1 and c.pinned_variable_group_id = $2) as pinned) t
		  left join lateral (
		         select g.fleet_id, g.id
		           from telflo_opamp.collector_groups g
		          where g.organization_id = $1 and g.variable_group_id = $2
		          order by g.fleet_id, g.id
		          limit $3) g on true
		 order by g.fleet_id, g.id`,
		organizationID, variableGroupID, MaxListed)
	if err != nil {
		return GroupUses{}, fmt.Errorf("variableuse: group use: %w", err)
	}
	use := GroupUses{CollectorGroups: []GroupRef{}}
	var (
		groups, pinned  int64
		fleetID, someID *uuid.UUID
	)
	if _, err := pgx.ForEachRow(rows, []any{&groups, &pinned, &fleetID, &someID}, func() error {
		use.TotalCollectorGroups, use.PinnedCollectors = int(groups), int(pinned)
		if someID != nil {
			use.CollectorGroups = append(use.CollectorGroups, GroupRef{FleetID: *fleetID, GroupID: *someID})
		}
		return nil
	}); err != nil {
		return GroupUses{}, fmt.Errorf("variableuse: group use: %w", err)
	}
	return use, nil
}
