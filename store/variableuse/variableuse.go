// Package variableuse is what the organization variables' guards read: the
// organization's row locked first, its counter bumped, the names a published
// version references that no variable defines, what uses the published
// versions that reference a name, and what uses a variable group.
//
// The names a published version references are written at publish only, by
// publications.Put, into telflo_opamp.publication_variable_names. A version
// published before migration 00012 has none until it is republished, so
// these helpers never count it: no backfill, no extraction later.
//
// Each Postgres function (postgres.go) takes the caller's pgx.Tx, in which
// the caller holds the organization's row (LockOrganization, the lock
// order's first step: organization, fleet, group, collector). The variables
// store and the fleet stores call these; the fleet stores never import
// store/variables, which imports this package.
//
// Every query filters on organization_id first, and every answer is the
// organization's own: another organization's published version, name or
// variable group has no use here. What these answer holds ids, counts and
// variable names, never a value; a name is configuration text, so no error
// here quotes one.
package variableuse

import (
	"errors"

	"github.com/google/uuid"
)

// MaxListed is the most entries a list answers: Undefined's names, UsedBy's
// entries and GroupUse's collector groups. Each comes with its exact total.
const MaxListed = 100

// ErrOrganizationNotFound is Bump's answer when the organization has no
// row: the caller locks it first (LockOrganization), so this is reached
// only by a caller that didn't.
var ErrOrganizationNotFound = errors.New("variableuse: the organization isn't registered")

// UndefinedNames is what Undefined answers: the names a published version
// references that no variable of its organization has.
type UndefinedNames struct {
	// Names holds at most MaxListed of them, in byte order.
	Names []string
	// Total is how many there are.
	Total int
}

// GroupRef names a collector group: its fleet and its id.
type GroupRef struct {
	FleetID uuid.UUID
	GroupID uuid.UUID
}

// Use is one used published version that references a name: the collector
// groups assigned it, by fleet and then group, and how many collectors are
// pinned to it.
type Use struct {
	PublicationID uuid.UUID
	// Groups may be cut short when the list reaches MaxListed entries.
	Groups []GroupRef
	// PinnedCollectors is exact, whether or not Groups was cut short.
	PinnedCollectors int
}

// Uses is what UsedBy answers.
type Uses struct {
	// List holds at most MaxListed entries in all, in publication id order:
	// an entry is one of a Use's groups, or one Use with no group (pinned
	// only). It stops at the MaxListed-th entry, so its last Use may be cut
	// short.
	List []Use
	// The totals are exact, over every use: the used published versions,
	// their collector groups, and the collectors pinned to them.
	Publications, Groups, PinnedCollectors int
}

// InUse reports whether anything uses a published version that references
// the name.
func (u Uses) InUse() bool {
	return u.Publications > 0
}

// GroupUses is what GroupUse answers: what uses a variable group, the
// collector groups bound to it and the collectors it is pinned on.
type GroupUses struct {
	// CollectorGroups holds at most MaxListed, by fleet and then group.
	CollectorGroups []GroupRef
	// TotalCollectorGroups and PinnedCollectors are exact.
	TotalCollectorGroups, PinnedCollectors int
}

// InUse reports whether a collector group or a collector uses the
// variable group.
func (g GroupUses) InUse() bool {
	return g.TotalCollectorGroups > 0 || g.PinnedCollectors > 0
}
