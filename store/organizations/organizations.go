// Package organizations keeps the registry of organizations: the opaque
// tenant ids the integrator supplies, registered before anything is stored
// under them, each with a display name.
//
// Its implementation is Postgres (what `serve` uses, on the
// opamp_server.organizations table), and the contract tests hold it to these
// rules:
//
//   - every method takes the organization's id, and touches that
//     organization's row only;
//   - Create never renames: the id taken under the same name is Unchanged
//     (a safe retry), under another name a Conflict;
//   - Delete deletes everything the organization owns: the table's foreign
//     key cascades to its publications;
//   - Limits and SetLimits read and set the organization's limits: the most
//     collectors it may enroll, and its agent requests a minute per pod,
//     each null for none or the server's default;
//   - nothing reads the time: CreatedAt and UpdatedAt are set by the caller
//     from its injected clock, and kept to the millisecond, the precision
//     the API answers in (Precision).
package organizations

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Organization is one registered organization.
type Organization struct {
	ID uuid.UUID
	// Name is a display name, 1 to 400 characters; the platform owns the
	// real one.
	Name string
	// CreatedAt is when this server registered it, by the caller's clock.
	CreatedAt time.Time
	// UpdatedAt is when its name last changed: CreatedAt until it is
	// renamed.
	UpdatedAt time.Time
}

// Precision is the precision the stores keep times at: the millisecond, the
// precision the API's replies carry (as publications.Precision).
const Precision = time.Millisecond

// normalized is o with its times in UTC at Precision, as a store keeps them.
func (o Organization) normalized() Organization {
	o.CreatedAt = o.CreatedAt.UTC().Truncate(Precision)
	o.UpdatedAt = o.UpdatedAt.UTC().Truncate(Precision)
	return o
}

// CreateResult is what Create did.
type CreateResult int

const (
	// Created: no organization had the id; this one is registered.
	Created CreateResult = iota + 1
	// Unchanged: the id is registered under exactly this name already;
	// nothing changed, and Create returns the stored organization.
	Unchanged
	// Conflict: the id is registered under another name. Create never
	// renames, and returns nothing about it.
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

// ErrNotFound is the answer for an id no organization has.
var ErrNotFound = errors.New("organizations: not found")

// Limits are an organization's limits. A nil field is unset.
type Limits struct {
	// MaxCollectors is the most collectors the organization may have
	// enrolled, zero or more, or nil for no cap.
	MaxCollectors *int32
	// RequestsPerMinute is the agent requests a minute the organization may
	// make to each pod, one or more, or nil for the server's default
	// (OPAMP_DEFAULT_REQUESTS_PER_MINUTE).
	RequestsPerMinute *int32
}

// ErrInvalidLimits is SetLimits' answer for a cap below zero or a request
// limit below one (the table's checks), setting nothing.
var ErrInvalidLimits = errors.New("organizations: invalid limits")

// valid reports whether the limits pass the table's checks.
func (l Limits) valid() bool {
	return (l.MaxCollectors == nil || *l.MaxCollectors >= 0) &&
		(l.RequestsPerMinute == nil || *l.RequestsPerMinute >= 1)
}

// Store keeps the registry.
type Store interface {
	// Create registers o unless its id is taken (see CreateResult). o's
	// CreatedAt and UpdatedAt are the caller's clock's now. It returns the
	// stored organization for Created and Unchanged, and a zero one for
	// Conflict.
	Create(ctx context.Context, o Organization) (CreateResult, Organization, error)
	// Get returns the organization id, or ErrNotFound.
	Get(ctx context.Context, id uuid.UUID) (Organization, error)
	// Exists reports whether the organization id is registered: what every
	// publication route asks first.
	Exists(ctx context.Context, id uuid.UUID) (bool, error)
	// Rename sets the organization's name, and its UpdatedAt to at, unless
	// the name is already name, which changes nothing. It returns the
	// stored organization, or ErrNotFound.
	Rename(ctx context.Context, id uuid.UUID, name string, at time.Time) (Organization, error)
	// Delete removes the organization and everything it owns, reporting
	// whether there was one.
	Delete(ctx context.Context, id uuid.UUID) (bool, error)
	// Limits returns the organization's limits, or ErrNotFound.
	Limits(ctx context.Context, id uuid.UUID) (Limits, error)
	// SetLimits replaces the organization's limits with limits, both fields
	// (a nil one unsets it), and returns what is stored; ErrNotFound for an
	// unregistered organization, ErrInvalidLimits for a value the table's
	// checks refuse. Neither moves UpdatedAt, which is the name's.
	SetLimits(ctx context.Context, id uuid.UUID, limits Limits) (Limits, error)
}
