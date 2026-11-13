// Package variables stores an organization's variables, its variable groups
// and their overrides: what the poll fills a published version's
// `${org:NAME}` references in with.
//
// Its implementation is Postgres (what `serve` uses, on
// opamp_server.variables, variable_groups and variable_group_overrides,
// migration 00012), and the contract tests hold it to these rules:
//
//   - every method takes the organization, and touches its rows only: a
//     variable, a variable group or an override of another organization's
//     reads as not found and changes nothing;
//   - a variable is a name (the renderer's grammar, at most 128 characters,
//     unique within its organization as written), a value (0 to 1,024
//     characters on one line) and an optional description; a variable group
//     is a name and a description and holds overrides only: at most one row
//     per variable group and variable. An override's Reset keeps its row
//     with no value, a reset row: ignored when filling in, never counted or
//     answered, and reused by a later override;
//   - at most MaxVariables variables and MaxGroups variable groups per
//     organization, counted under the organization's lock;
//   - a create takes the caller's id, so a retry answers Unchanged; a change
//     of anything is last write wins;
//   - a variable's rename or delete is refused while a published version
//     that a collector group or a collector uses references its name
//     (*InUseError), and a variable group's delete while a collector group
//     or a collector uses it (*GroupInUseError); a value change is never
//     refused;
//   - every write is one transaction that takes the organization's row first
//     (`for no key update`, variableuse.LockOrganization: the lock order's
//     first step), then its rows; every write that can change any
//     collector's filled-in text moves the organization's counter on once
//     (variableuse.Bump: the poll's cache is keyed by it and is exact) and
//     writes that value as the stamp of the row that changed, never the
//     variable group's. They are a variable's create, value change, rename
//     and delete (the delete stamps nothing: its row is gone), and an
//     override's set, change and Reset. A description, a variable group's
//     create, name or description, and a variable group's delete (refused
//     while a collector group or a collector uses it, so no collector is
//     filled in with it) move nothing; nor does a write that changes
//     nothing. The delete relies on the fleet stores: every write that binds
//     a collector group or pins a collector to a variable group, or clears
//     one, moves the counter, so a cache entry keyed by a deleted group's id
//     (even an id used again) is never read at a later revision;
//   - every change moves its row's ChangedBy and ChangedAt, and an
//     override's set, change or Reset its variable group's too, for display
//     only;
//   - nothing reads the time: every time is the caller's, from its injected
//     clock, kept to the millisecond (fleets.Precision).
//
// A variable's value, name and description, and a variable group's name and
// description, are configuration text: no error here quotes one; an invalid
// field is ErrInvalid wrapped with the field's name. There are no secrets:
// nothing is sealed or masked.
package variables

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/collectorgroups"
	"github.com/telflo/opamp-server/store/fleets"
	"github.com/telflo/opamp-server/store/orgvariables"
	"github.com/telflo/opamp-server/store/variableuse"
)

// The bounds. The lengths are orgvariables', counted in characters (code
// points), as Postgres's char_length counts them.
const (
	// MaxVariables is the most variables an organization may have.
	MaxVariables = 150
	// MaxGroups is the most variable groups an organization may have.
	MaxGroups = 100

	MaxNameLength        = orgvariables.MaxNameLength
	MaxValueLength       = orgvariables.MaxValueLength
	MaxDescriptionLength = orgvariables.MaxDescriptionLength
	MaxGroupNameLength   = orgvariables.MaxGroupNameLength
)

// Variable is one of an organization's variables.
type Variable struct {
	ID             uuid.UUID
	OrganizationID uuid.UUID
	// Name is the reference's NAME, `${org:NAME}`.
	Name  string
	Value string
	// Description is nil for none.
	Description *string
	// ChangedBy is nil for a change no person made.
	ChangedBy *fleets.Actor
	// ChangedAt is when anything of it last changed.
	ChangedAt time.Time
}

// Usage is what uses a variable group: the collector groups bound to it and
// the collectors it is pinned on.
type Usage struct {
	CollectorGroups  int
	PinnedCollectors int
}

// Group is one of an organization's variable groups.
type Group struct {
	ID             uuid.UUID
	OrganizationID uuid.UUID
	Name           string
	// Description is nil for none.
	Description *string
	// ChangedBy and ChangedAt move on a change of the group or of an
	// override in it (for display only: nothing on the group feeds a
	// collector's status).
	ChangedBy *fleets.Actor
	ChangedAt time.Time
	// OverrideCount counts its overrides with a value, never a reset row.
	OverrideCount int
	Usage         Usage
}

// Override is a variable group's override of a variable, with a value: a
// reset row is never answered.
type Override struct {
	VariableGroupID uuid.UUID
	VariableID      uuid.UUID
	Value           string
	ChangedBy       *fleets.Actor
	ChangedAt       time.Time
}

// NewVariable is what CreateVariable makes.
type NewVariable struct {
	// ID is the caller's: the same id again makes CreateVariable idempotent.
	ID             uuid.UUID
	OrganizationID uuid.UUID
	Name           string
	Value          string
	// Description is nil for none.
	Description *string
	// Actor is the person creating it, or nil.
	Actor *fleets.Actor
	// At is the caller's clock's now.
	At time.Time
}

// VariableChanges is what UpdateVariable changes: each non-nil (or Set)
// field. Description's Clear removes the description.
type VariableChanges struct {
	Name        *string
	Value       *string
	Description collectorgroups.Change[string]
}

// NewGroup is what CreateGroup makes.
type NewGroup struct {
	// ID is the caller's: the same id again makes CreateGroup idempotent.
	ID             uuid.UUID
	OrganizationID uuid.UUID
	Name           string
	// Description is nil for none.
	Description *string
	// Actor is the person creating it, or nil.
	Actor *fleets.Actor
	// At is the caller's clock's now.
	At time.Time
}

// GroupChanges is what UpdateGroup changes: each non-nil (or Set) field.
type GroupChanges struct {
	Name        *string
	Description collectorgroups.Change[string]
}

// CreateResult is what a create did.
type CreateResult int

const (
	// Created: the row is stored.
	Created CreateResult = iota + 1
	// Unchanged: the organization already holds a row with this id and the
	// same name, value and description (a group: name and description);
	// nothing changed, and the stored row is answered.
	Unchanged
	// Conflict: the id is taken otherwise, by different content or by
	// another organization's row. Nothing about it is answered. Another
	// organization's id is found last, by the insert that inserts nothing
	// (each store reads its own organization's rows only): it is a Conflict
	// only when the name is free and the limit isn't reached; otherwise the
	// create answers ErrNameTaken (ErrGroupNameTaken) or ErrLimitReached
	// (ErrGroupLimitReached).
	Conflict
)

func (r CreateResult) String() string {
	switch r {
	case Created:
		return "created"
	case Unchanged:
		return "unchanged"
	case Conflict:
		return "conflict"
	}
	return "unknown"
}

var (
	// ErrNotFound is the answer for a variable the organization doesn't
	// hold, whether or not another organization does.
	ErrNotFound = errors.New("variables: variable not found")
	// ErrGroupNotFound is the answer for a variable group the organization
	// doesn't hold.
	ErrGroupNotFound = errors.New("variables: variable group not found")
	// ErrNameTaken is the answer for a name another of the organization's
	// variables has, compared as written.
	ErrNameTaken = errors.New("variables: variable name taken")
	// ErrGroupNameTaken is the answer for a name another of the
	// organization's variable groups has, compared as written.
	ErrGroupNameTaken = errors.New("variables: variable group name taken")
	// ErrLimitReached is CreateVariable's answer when the organization has
	// MaxVariables already.
	ErrLimitReached = errors.New("variables: the organization has the most variables it may have")
	// ErrGroupLimitReached is CreateGroup's answer when the organization has
	// MaxGroups already.
	ErrGroupLimitReached = errors.New("variables: the organization has the most variable groups it may have")
	// ErrOrganizationNotFound is a write's answer for an organization that
	// isn't registered.
	ErrOrganizationNotFound = errors.New("variables: the organization isn't registered")
	// ErrInvalid is the answer for a field the tables would refuse; it is
	// wrapped with the field's name, never its value.
	ErrInvalid = errors.New("variables: invalid")
	// ErrInUse is what an *InUseError matches.
	ErrInUse = errors.New("variables: a used published version references the variable")
	// ErrGroupInUse is what a *GroupInUseError matches.
	ErrGroupInUse = errors.New("variables: a collector group or a collector uses the variable group")
)

// InUseError is a variable's rename or delete refused: what uses the
// published versions that reference its name, at most variableuse.MaxListed
// entries with exact totals.
type InUseError struct {
	Uses variableuse.Uses
}

func (e *InUseError) Error() string        { return ErrInUse.Error() }
func (e *InUseError) Is(target error) bool { return target == ErrInUse }

// GroupInUseError is a variable group's delete refused: the collector groups
// bound to it, at most variableuse.MaxListed with the exact total, and the
// collectors it is pinned on.
type GroupInUseError struct {
	Use variableuse.GroupUses
}

func (e *GroupInUseError) Error() string        { return ErrGroupInUse.Error() }
func (e *GroupInUseError) Is(target error) bool { return target == ErrGroupInUse }

// Store keeps an organization's variables, variable groups and overrides,
// and resolves them for a collector (orgvariables.Store). Every method is
// scoped to one organization.
type Store interface {
	orgvariables.Store

	// ListVariables returns the organization's variables by name, in byte
	// order.
	ListVariables(ctx context.Context, organizationID uuid.UUID) ([]Variable, error)
	// CreateVariable makes v, stamped with the counter's next value; the
	// variable is answered unless the result is Conflict. It checks the id
	// in the organization, then the name, then the limit, then another
	// organization's id (Conflict only when the name is free and the limit
	// isn't reached).
	CreateVariable(ctx context.Context, v NewVariable) (CreateResult, Variable, error)
	// UpdateVariable applies changes to the organization's variable id; a
	// value change or a rename is stamped, a rename refused while the old
	// name is in use (*InUseError). Changes that change nothing answer the
	// variable as it is.
	UpdateVariable(ctx context.Context, organizationID, id uuid.UUID, changes VariableChanges, actor *fleets.Actor, at time.Time) (Variable, error)
	// DeleteVariable deletes the organization's variable id with its
	// overrides (reset rows included), unless its name is in use
	// (*InUseError).
	DeleteVariable(ctx context.Context, organizationID, id uuid.UUID) error

	// ListGroups returns the organization's variable groups by name, in
	// byte order.
	ListGroups(ctx context.Context, organizationID uuid.UUID) ([]Group, error)
	// CreateGroup makes g; the group is answered unless the result is
	// Conflict. Its checks are CreateVariable's, in the same order.
	CreateGroup(ctx context.Context, g NewGroup) (CreateResult, Group, error)
	// GetGroup returns the organization's variable group id and its
	// overrides with a value, by their variable's name in byte order.
	GetGroup(ctx context.Context, organizationID, id uuid.UUID) (Group, []Override, error)
	// UpdateGroup applies changes to the organization's variable group id.
	UpdateGroup(ctx context.Context, organizationID, id uuid.UUID, changes GroupChanges, actor *fleets.Actor, at time.Time) (Group, error)
	// DeleteGroup deletes the organization's variable group id with its
	// overrides, unless it is in use (*GroupInUseError).
	DeleteGroup(ctx context.Context, organizationID, id uuid.UUID) error

	// SetOverride gives the variable group's override of the variable
	// value. created is true when there was no override with a value (no
	// row, or a reset row revived). The same value again changes nothing
	// and answers the override as it was.
	SetOverride(ctx context.Context, organizationID, groupID, variableID uuid.UUID, value string, actor *fleets.Actor, at time.Time) (o Override, created bool, err error)
	// ResetOverride resets the variable group's override of the variable:
	// the row is kept with no value and the Reset's stamp. It reports
	// whether there was an override with a value to reset.
	ResetOverride(ctx context.Context, organizationID, groupID, variableID uuid.UUID, actor *fleets.Actor, at time.Time) (bool, error)
}

// validate checks a new variable as the table would.
func (v NewVariable) validate() error {
	switch {
	case !orgvariables.ValidName(v.Name):
		return invalid("name")
	case !orgvariables.ValidValue(v.Value):
		return invalid("value")
	case !orgvariables.ValidDescription(v.Description):
		return invalid("description")
	case !fleets.ValidActor(v.Actor):
		return invalid("actor")
	}
	return nil
}

// validate checks the changes and the actor as the table would.
func (c VariableChanges) validate(actor *fleets.Actor) error {
	switch {
	case c.Name != nil && !orgvariables.ValidName(*c.Name):
		return invalid("name")
	case c.Value != nil && !orgvariables.ValidValue(*c.Value):
		return invalid("value")
	case !validDescriptionChange(c.Description):
		return invalid("description")
	case !fleets.ValidActor(actor):
		return invalid("actor")
	}
	return nil
}

// validate checks a new variable group as the table would.
func (g NewGroup) validate() error {
	switch {
	case !orgvariables.ValidGroupName(g.Name):
		return invalid("name")
	case !orgvariables.ValidDescription(g.Description):
		return invalid("description")
	case !fleets.ValidActor(g.Actor):
		return invalid("actor")
	}
	return nil
}

// validate checks the changes and the actor as the table would.
func (c GroupChanges) validate(actor *fleets.Actor) error {
	switch {
	case c.Name != nil && !orgvariables.ValidGroupName(*c.Name):
		return invalid("name")
	case !validDescriptionChange(c.Description):
		return invalid("description")
	case !fleets.ValidActor(actor):
		return invalid("actor")
	}
	return nil
}

// validOverride checks an override's value and actor as the table would.
func validOverride(value string, actor *fleets.Actor) error {
	switch {
	case !orgvariables.ValidValue(value):
		return invalid("value")
	case !fleets.ValidActor(actor):
		return invalid("actor")
	}
	return nil
}

func validDescriptionChange(c collectorgroups.Change[string]) bool {
	return !c.Set || orgvariables.ValidDescription(c.Value)
}

// bumpFailed is a bump's failure as the store answers it: the
// organization's row gone (variableuse.ErrOrganizationNotFound, which the
// lock taken first makes unreachable) is ErrOrganizationNotFound.
func bumpFailed(err error) error {
	if errors.Is(err, variableuse.ErrOrganizationNotFound) {
		return ErrOrganizationNotFound
	}
	return err
}

func invalid(field string) error {
	return fmt.Errorf("%w: %s", ErrInvalid, field)
}

// describe is a description after the change.
func describe(current *string, c collectorgroups.Change[string]) *string {
	if !c.Set {
		return current
	}
	return copyOf(c.Value)
}

// variableAfter is current after changes, and what of it changed: filled
// says the value or the name did (the change is stamped), changed that
// anything did.
func variableAfter(current Variable, c VariableChanges) (next Variable, filled, changed bool) {
	next = current
	if c.Name != nil {
		next.Name = *c.Name
	}
	if c.Value != nil {
		next.Value = *c.Value
	}
	next.Description = describe(current.Description, c.Description)
	filled = next.Name != current.Name || next.Value != current.Value
	changed = filled || !equalText(next.Description, current.Description)
	return next, filled, changed
}

// groupAfter is current after changes, and whether anything changed.
func groupAfter(current Group, c GroupChanges) (next Group, changed bool) {
	next = current
	if c.Name != nil {
		next.Name = *c.Name
	}
	next.Description = describe(current.Description, c.Description)
	return next, next.Name != current.Name || !equalText(next.Description, current.Description)
}

func equalText(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// at is a time as the stores keep it.
func at(t time.Time) time.Time {
	return t.UTC().Truncate(fleets.Precision)
}

func copyOf[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// actorColumns is an actor as its two nullable columns.
func actorColumns(a *fleets.Actor) (*uuid.UUID, *string) {
	if a == nil {
		return nil, nil
	}
	id, email := a.ID, a.Email
	return &id, &email
}

// actorOf is the actor of its two nullable columns.
func actorOf(id *uuid.UUID, email *string) *fleets.Actor {
	if id == nil || email == nil {
		return nil
	}
	return &fleets.Actor{ID: *id, Email: *email}
}
