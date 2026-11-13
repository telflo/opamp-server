// Package fleets stores fleets: sets of collectors that enroll with the same
// tokens and are managed together, server-class only (`server` and
// `kubernetes-supervisor`).
//
// Its implementation is Postgres (what `serve` uses, on opamp_server.fleets
// and the tables a fleet's creation writes), and the contract tests hold it
// to these rules:
//
//   - every method takes the organization, and touches its rows only: a
//     fleet of another organization reads as not found and changes nothing;
//   - Create makes, in one transaction, the fleet, its Default group (named
//     "Default"), its default token (never expires) and its rotate token,
//     and an `issued` event for each token. The caller's id makes it
//     idempotent: the same id with the same label and type is Unchanged,
//     anything else under that id a Conflict, and a label the organization
//     already uses is LabelTaken;
//   - Delete deletes the fleet and everything it holds (its groups,
//     tokens, token events and collectors), by the cascades;
//   - labels are unique within an organization as written (case-sensitive),
//     trimmed of whitespace, 1 to 80 characters;
//   - nothing reads the time: every time is the caller's, from its injected
//     clock, kept to the millisecond (Precision).
//
// A fleet's summaries (collector counts, liveness, sync) are the
// collectors' store's, not this one's.
package fleets

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Precision is the precision the stores keep times at: the millisecond, the
// precision the API's replies carry (as publications.Precision).
const Precision = time.Millisecond

// Type is a fleet's type. It never changes.
type Type string

// The fleet types, both server-class: the only ones Valid accepts.
const (
	TypeServer               Type = "server"
	TypeKubernetesSupervisor Type = "kubernetes-supervisor"
)

// Valid reports whether t is a fleet type.
func (t Type) Valid() bool {
	return t == TypeServer || t == TypeKubernetesSupervisor
}

// Status is whether a fleet is active or archived; an archived fleet's
// collectors are refused.
type Status string

const (
	StatusActive   Status = "active"
	StatusArchived Status = "archived"
)

// Valid reports whether s is a fleet status.
func (s Status) Valid() bool {
	return s == StatusActive || s == StatusArchived
}

// Actor is a person the platform names as making a change: its id and their
// email at the time, a snapshot. The stores keep both or neither.
type Actor struct {
	ID    uuid.UUID
	Email string
}

// MaxEmailLength is the most characters an actor's email may have (the
// tables' checks).
const MaxEmailLength = 320

// ValidActor reports whether a is nil or an actor the tables accept.
func ValidActor(a *Actor) bool {
	return a == nil || ValidText(a.Email, 1, MaxEmailLength)
}

// MaxLabelLength is the most characters a fleet's label may have.
const MaxLabelLength = 80

// ValidLabel reports whether label is a label the table accepts: trimmed
// (Trimmed), 1 to MaxLabelLength characters; and, stricter than the table
// (as a collector's name), no control character anywhere (NoControl).
func ValidLabel(label string) bool {
	return Trimmed(label) && ValidText(label, 1, MaxLabelLength) && NoControl(label)
}

// NoControl reports whether s holds no control character (unicode.IsControl:
// C0, DEL and C1, a tab and a newline among them): what a fleet's label, a
// group's name and a collector's name never hold, so none is a line break or
// an escape where it is shown.
func NoControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) < 0
}

// Trimmed reports whether s has no leading or trailing whitespace, as
// Unicode counts it: at least what the tables' checks refuse (`[[:space:]]`
// at either end), so a value accepted here is one the table stores.
func Trimmed(s string) bool {
	return strings.TrimFunc(s, unicode.IsSpace) == s
}

// ValidText reports whether s is text Postgres stores as written (valid
// UTF-8, no NUL) of min to max characters, as char_length counts them.
func ValidText(s string, min, max int) bool {
	if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
		return false
	}
	n := utf8.RuneCountInString(s)
	return n >= min && n <= max
}

// Fleet is one stored fleet.
type Fleet struct {
	ID             uuid.UUID
	OrganizationID uuid.UUID
	Label          string
	Type           Type
	Status         Status
	// CreatedBy is nil for a fleet no person made.
	CreatedBy *Actor
	CreatedAt time.Time
	// UpdatedAt is when its label or status last changed: CreatedAt until
	// then.
	UpdatedAt time.Time
}

// NewFleet is the input to Store.Create: the fleet to create.
type NewFleet struct {
	// ID is the caller's: the same id again makes Create idempotent.
	ID             uuid.UUID
	OrganizationID uuid.UUID
	Label          string
	Type           Type
	// CreatedBy is the person creating it, or nil; also the actor of the
	// tokens' `issued` events.
	CreatedBy *Actor
	// RotateExpiresAt is when the rotate token expires, or nil for never.
	// It is after At.
	RotateExpiresAt *time.Time
	// At is the caller's clock's now: the fleet's, group's and tokens'
	// CreatedAt and the events' time.
	At time.Time
}

// DefaultGroupName is the name of the group every fleet is made with.
const DefaultGroupName = "Default"

// Creation is returned by Store.Create: the new fleet, plus the IDs of the
// Default group and the two enrollment tokens created with it. The API
// needs the token IDs to build the bearers it returns once, in the
// create's reply. When the result is Unchanged (the fleet already
// existed), only Fleet is set and the IDs are empty.
type Creation struct {
	Fleet           Fleet
	DefaultGroupID  uuid.UUID
	DefaultTokenID  uuid.UUID
	RotateTokenID   uuid.UUID
	RotateExpiresAt *time.Time
}

// CreateResult is returned by Store.Create: whether it created the fleet,
// found the same fleet already there, or refused it.
type CreateResult int

const (
	// Created: the fleet, its Default group, its two tokens and their
	// `issued` events are stored.
	Created CreateResult = iota + 1
	// Unchanged: the organization already holds a fleet with this id, label
	// and type; nothing changed, and Create returns the stored fleet.
	Unchanged
	// Conflict: the id is taken by a different fleet, or by another
	// organization's. Create returns nothing about it.
	Conflict
	// LabelTaken: another of the organization's fleets has the label.
	LabelTaken
)

func (r CreateResult) String() string {
	switch r {
	case Created:
		return "created"
	case Unchanged:
		return "unchanged"
	case Conflict:
		return "conflict"
	case LabelTaken:
		return "label taken"
	}
	return "unknown"
}

// Changes is what Update changes: each non-nil field.
type Changes struct {
	Label  *string
	Status *Status
}

var (
	// ErrNotFound is the answer for a fleet the organization doesn't hold,
	// whether or not another organization does.
	ErrNotFound = errors.New("fleets: not found")
	// ErrOrganizationNotFound is Create's answer for an organization that
	// isn't registered.
	ErrOrganizationNotFound = errors.New("fleets: the organization isn't registered")
	// ErrLabelTaken is Update's answer for a label another of the
	// organization's fleets has.
	ErrLabelTaken = errors.New("fleets: label taken")
	// ErrInvalid is the answer for a value the tables would refuse (a
	// label, a type, a status, an actor, a rotate token's expiry); it is
	// wrapped with the field's name, never its value.
	ErrInvalid = errors.New("fleets: invalid")
)

// MaxListLimit is the most fleets one List call returns.
const MaxListLimit = 100

// ListOptions select a page of List.
type ListOptions struct {
	// Limit is 1 to MaxListLimit.
	Limit int
	// After is where the page starts (the previous page's Next), or nil.
	After *Cursor
	// Status, when non-nil, lists only the fleets with that status.
	Status *Status
}

// Page is one page of List.
type Page struct {
	Fleets []Fleet
	// Next is where the next page starts, or nil on the last page.
	Next *Cursor
}

// Store keeps fleets. Every method is scoped to one organization.
type Store interface {
	// Create makes the fleet and what it starts with (see the package
	// comment and CreateResult). ErrOrganizationNotFound for an
	// unregistered organization; ErrInvalid for a value the tables refuse.
	Create(ctx context.Context, f NewFleet) (CreateResult, Creation, error)
	// List returns a page of the organization's fleets, newest first by
	// CreatedAt and then by id, both descending.
	List(ctx context.Context, organizationID uuid.UUID, options ListOptions) (Page, error)
	// Get returns the organization's fleet id, or ErrNotFound.
	Get(ctx context.Context, organizationID, id uuid.UUID) (Fleet, error)
	// Update applies changes to the organization's fleet id and returns it.
	// UpdatedAt moves to at only when something changes. ErrNotFound,
	// ErrLabelTaken, or ErrInvalid.
	Update(ctx context.Context, organizationID, id uuid.UUID, changes Changes, at time.Time) (Fleet, error)
	// Delete removes the organization's fleet id and everything it holds,
	// reporting whether there was one.
	Delete(ctx context.Context, organizationID, id uuid.UUID) (bool, error)
	// TypeAndStatus returns the type and status of the organization's fleet
	// id, or ErrNotFound: what the agent side asks of a fleet.
	TypeAndStatus(ctx context.Context, organizationID, id uuid.UUID) (Type, Status, error)
}

// validate checks a new fleet as the tables would.
func (f NewFleet) validate() error {
	switch {
	case !ValidLabel(f.Label):
		return invalid("label")
	case !f.Type.Valid():
		return invalid("type")
	case !ValidActor(f.CreatedBy):
		return invalid("created by")
	case f.RotateExpiresAt != nil && !f.RotateExpiresAt.UTC().Truncate(Precision).After(f.At.UTC().Truncate(Precision)):
		return invalid("rotate token expiry")
	}
	return nil
}

// validate checks changes as the table would.
func (c Changes) validate() error {
	switch {
	case c.Label != nil && !ValidLabel(*c.Label):
		return invalid("label")
	case c.Status != nil && !c.Status.Valid():
		return invalid("status")
	}
	return nil
}

func invalid(field string) error {
	return fmt.Errorf("%w: %s", ErrInvalid, field)
}

// at is t as a store keeps it: UTC, at Precision.
func at(t time.Time) time.Time {
	return t.UTC().Truncate(Precision)
}

// Cursor is a position in a List: the last row of a page. Clients see it
// only as an opaque string (String, ParseCursor).
type Cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// cursorBytes is a cursor's length before encoding: the instant in
// microseconds since the Unix epoch (8 bytes, big-endian) and the id.
const cursorBytes = 8 + 16

// ErrInvalidCursor is ParseCursor's answer to a string String didn't make.
var ErrInvalidCursor = errors.New("fleets: invalid cursor")

func cursorOf(f Fleet) *Cursor {
	return &Cursor{CreatedAt: f.CreatedAt, ID: f.ID}
}

// String encodes the cursor as unpadded base64url.
func (c Cursor) String() string {
	var raw [cursorBytes]byte
	binary.BigEndian.PutUint64(raw[:8], uint64(c.CreatedAt.UnixMicro()))
	copy(raw[8:], c.ID[:])
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

// The instants a stored time may hold: the years 0001 to 9999 in UTC.
var (
	earliest = time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)
	latest   = time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC)
)

// ParseCursor decodes a cursor String made, or answers ErrInvalidCursor,
// also for a time no stored fleet can have (outside the years 0001 to 9999,
// or not a whole millisecond).
func ParseCursor(s string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || len(raw) != cursorBytes {
		return Cursor{}, ErrInvalidCursor
	}
	micros := int64(binary.BigEndian.Uint64(raw[:8]))
	createdAt := time.UnixMicro(micros).UTC()
	if micros%int64(Precision/time.Microsecond) != 0 || createdAt.Before(earliest) || !createdAt.Before(latest) {
		return Cursor{}, ErrInvalidCursor
	}
	var id uuid.UUID
	copy(id[:], raw[8:])
	return Cursor{CreatedAt: createdAt, ID: id}, nil
}

func checkOptions(o ListOptions) error {
	if o.Limit < 1 || o.Limit > MaxListLimit {
		return errors.New("fleets: list limit out of range")
	}
	if o.Status != nil && !o.Status.Valid() {
		return invalid("status")
	}
	return nil
}
