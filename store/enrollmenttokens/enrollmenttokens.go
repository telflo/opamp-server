// Package enrollmenttokens stores enrollment tokens and their audit events:
// a collector's credential to this server. The bearer itself is never stored
// (a reveal encrypts afresh); a token's row is its id, its fleet, its kind,
// its expiry and its revocation.
//
// A fleet is made with a default token (enrollment; never expires) and a
// rotate token (fleets.Store.Create). A Store's implementation is Postgres
// (what `serve` uses, on telflo_opamp.enrollment_tokens and
// enrollment_token_events), and the contract tests hold it to these rules:
//
//   - every method but Lookup takes the organization and the fleet (or,
//     RotateStates, a page of fleets), and touches their rows only: a token
//     of another organization or another fleet reads as not found and
//     changes nothing. Lookup is the agent's authentication, by the token's
//     id alone (a bearer names no organization; the caller then compares the
//     row's fleet with the bearer's);
//   - Rotate runs its steps in one transaction, under the fleet's row lock,
//     so rotations of one fleet serialize;
//   - Revoke sets RevokedAt, once; the default token may be revoked too, and
//     IssueDefault then makes a new one: a fleet holds at most one valid
//     default token;
//   - events carry ids and the actor only, never a bearer;
//   - nothing reads the time: every time is the caller's, kept to the
//     millisecond.
package enrollmenttokens

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/telflo-opamp-server/store/fleets"
)

// Kind is a token's kind.
type Kind string

const (
	// KindDefault is a fleet's enrollment token: at most one valid per
	// fleet, never expires, served no configuration; its collectors are
	// handed the fleet's rotate token.
	KindDefault Kind = "default"
	// KindRotate is a token collectors are served configuration on; it may
	// expire, and a rotation replaces it.
	KindRotate Kind = "rotate"
)

// ActiveWindow is how recently a collector must have been seen on a token to
// count in List's ActiveCollectors: the last 5 minutes, the online ladder's
// last step.
const ActiveWindow = 5 * time.Minute

// Token is one stored token.
type Token struct {
	// ID is the bearer's token_id.
	ID             uuid.UUID
	OrganizationID uuid.UUID
	FleetID        uuid.UUID
	Kind           Kind
	// ExpiresAt is when it stops authenticating, or nil for never.
	ExpiresAt *time.Time
	// RevokedAt is when it was revoked, or nil.
	RevokedAt *time.Time
	// DoRefresh is whether a collector authenticating with it is pushed the
	// fleet's newest rotate token: a rotated token in its grace. Never
	// cleared.
	DoRefresh bool
	// CreatedBy is nil for a token no person made.
	CreatedBy *fleets.Actor
	CreatedAt time.Time
}

// ValidAt reports whether the token authenticates at now: not revoked, and
// not past its expiry.
func (t Token) ValidAt(now time.Time) bool {
	return t.RevokedAt == nil && (t.ExpiresAt == nil || t.ExpiresAt.After(at(now)))
}

// RotateState is a fleet's rotate tokens at a moment, as its summary answers
// them: a fleet with no valid rotate token has none.
type RotateState struct {
	// ExpiresAt is the current rotate token's expiry (GetActiveRotate's),
	// or nil for never.
	ExpiresAt *time.Time
	// GraceEndsAt is the latest expiry among the valid rotate tokens in
	// their grace (DoRefresh) other than the current one, or nil when there
	// is none. The current one is left out: when the current token is
	// revoked mid-grace, a token in its grace becomes current, and nothing
	// is rotating towards it.
	GraceEndsAt *time.Time
	// ValidCount is how many of the fleet's rotate tokens are valid (at
	// least 1); a default token never counts.
	ValidCount int
}

// Listed is a token as List answers it.
type Listed struct {
	Token
	// ActiveCollectors is how many of the fleet's collectors were last seen
	// on the token within ActiveWindow of List's now.
	ActiveCollectors int
}

// Authenticated is a token as Lookup answers it: the row, with its fleet's
// type and status.
type Authenticated struct {
	Token
	FleetType   fleets.Type
	FleetStatus fleets.Status
}

// EventKind is what an event records.
type EventKind string

const (
	EventIssued  EventKind = "issued"
	EventViewed  EventKind = "viewed"
	EventRotated EventKind = "rotated"
	EventRevoked EventKind = "revoked"
)

// Event is one audit event: ids and the actor only.
type Event struct {
	ID             uuid.UUID
	OrganizationID uuid.UUID
	FleetID        uuid.UUID
	TokenID        uuid.UUID
	Kind           EventKind
	// Actor is nil for an event no person caused.
	Actor      *fleets.Actor
	OccurredAt time.Time
}

// MaxListLimit is the most events one ListEvents call returns.
const MaxListLimit = 100

// EventPage is one page of ListEvents.
type EventPage struct {
	Events []Event
	// Next is where the next page starts, or nil on the last page.
	Next *EventCursor
}

// EventCursor is a position in ListEvents: the last event of a page.
// Clients see it only as an opaque string (String, ParseEventCursor).
type EventCursor struct {
	OccurredAt time.Time
	ID         uuid.UUID
}

// eventCursorBytes is a cursor's length before encoding: the instant in
// microseconds since the Unix epoch (8 bytes, big-endian) and the id, as
// the fleets' and the collector errors' cursors.
const eventCursorBytes = 8 + 16

// The instants a stored time may hold: the years 0001 to 9999 in UTC.
var (
	earliest = time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)
	latest   = time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC)
)

// String encodes the cursor as unpadded base64url.
func (c EventCursor) String() string {
	var raw [eventCursorBytes]byte
	binary.BigEndian.PutUint64(raw[:8], uint64(c.OccurredAt.UnixMicro()))
	copy(raw[8:], c.ID[:])
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

// ParseEventCursor decodes a cursor String made, or answers
// ErrInvalidCursor, also for a time no stored event can have (outside the
// years 0001 to 9999, or not a whole millisecond).
func ParseEventCursor(s string) (EventCursor, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || len(raw) != eventCursorBytes {
		return EventCursor{}, ErrInvalidCursor
	}
	micros := int64(binary.BigEndian.Uint64(raw[:8]))
	occurredAt := time.UnixMicro(micros).UTC()
	if micros%int64(fleets.Precision/time.Microsecond) != 0 || occurredAt.Before(earliest) || !occurredAt.Before(latest) {
		return EventCursor{}, ErrInvalidCursor
	}
	var id uuid.UUID
	copy(id[:], raw[8:])
	return EventCursor{OccurredAt: occurredAt, ID: id}, nil
}

// Rotation is what Rotate does.
type Rotation struct {
	OrganizationID uuid.UUID
	FleetID        uuid.UUID
	// ExpiresAt is when the new rotate token expires, or nil for never.
	ExpiresAt *time.Time
	// GraceEndsAt is when the predecessor stops authenticating, unless it
	// expires sooner on its own: after At, and not after ExpiresAt.
	GraceEndsAt time.Time
	// Actor is who rotates, or nil; the new token's creator and both
	// events' actor.
	Actor *fleets.Actor
	// At is the caller's clock's now.
	At time.Time
}

// RotateResult is what Rotate made.
type RotateResult struct {
	// Token is the new rotate token.
	Token Token
	// PredecessorID is the token put in its grace, or nil when the fleet
	// had no valid rotate token.
	PredecessorID *uuid.UUID
}

// RevokeResult is what Revoke did.
type RevokeResult int

const (
	// Revoked: the token is revoked now, and a `revoked` event written.
	Revoked RevokeResult = iota + 1
	// AlreadyRevoked: it was revoked before; nothing changed, no event.
	AlreadyRevoked
)

func (r RevokeResult) String() string {
	switch r {
	case Revoked:
		return "revoked"
	case AlreadyRevoked:
		return "already revoked"
	}
	return "unknown"
}

var (
	// ErrNotFound is the answer for a token the organization's fleet
	// doesn't hold (or, from Lookup, no token).
	ErrNotFound = errors.New("enrollmenttokens: not found")
	// ErrFleetNotFound is the answer for a fleet the organization doesn't
	// hold.
	ErrFleetNotFound = errors.New("enrollmenttokens: fleet not found")
	// ErrFleetArchived is Rotate's and IssueDefault's answer for an
	// archived fleet.
	ErrFleetArchived = errors.New("enrollmenttokens: fleet archived")
	// ErrDefaultExists is IssueDefault's answer for a fleet that has a
	// valid (not revoked) default token.
	ErrDefaultExists = errors.New("enrollmenttokens: a valid default token exists")
	// ErrRevoked is RecordViewed's answer for a revoked token.
	ErrRevoked = errors.New("enrollmenttokens: revoked")
	// ErrExpired is RecordViewed's answer for a token past its expiry.
	ErrExpired = errors.New("enrollmenttokens: expired")
	// ErrInvalidCursor is ParseEventCursor's answer to a string
	// EventCursor.String didn't make.
	ErrInvalidCursor = errors.New("enrollmenttokens: invalid cursor")
	// ErrInvalid is the answer for a value refused (a grace end not in the
	// future or after the new token's expiry, an actor); it is wrapped with
	// the field's name, never its value.
	ErrInvalid = errors.New("enrollmenttokens: invalid")
)

// Store keeps enrollment tokens and their events.
type Store interface {
	// List returns the fleet's tokens, newest first (by CreatedAt, then id,
	// both descending), each with its collectors seen within ActiveWindow
	// of now; ErrFleetNotFound.
	List(ctx context.Context, organizationID, fleetID uuid.UUID, now time.Time) ([]Listed, error)
	// Get returns the fleet's token id, or ErrNotFound.
	Get(ctx context.Context, organizationID, fleetID, id uuid.UUID) (Token, error)
	// Rotate, under the fleet's row lock: the fleet must be active
	// (ErrFleetArchived); its current rotate token (the newest valid one,
	// see the implementations) gets ExpiresAt = the earlier of its own
	// expiry (none: never) and GraceEndsAt, and DoRefresh, so a rotation
	// never lengthens its life; a new rotate token is made; events `rotated`
	// (the predecessor, if any) and `issued` (the new one).
	// ErrFleetNotFound, ErrInvalid.
	Rotate(ctx context.Context, r Rotation) (RotateResult, error)
	// IssueDefault makes a new default token for the fleet when it has no
	// valid one (every default token revoked; a default token never
	// expires), and an `issued` event naming actor, in one transaction under
	// the fleet's row lock. ErrFleetNotFound, then ErrFleetArchived, then
	// ErrDefaultExists (also for a second issue racing the first, which the
	// unique index refuses), ErrInvalid.
	IssueDefault(ctx context.Context, organizationID, fleetID uuid.UUID, actor *fleets.Actor, now time.Time) (Token, error)
	// Revoke sets the fleet's token id's RevokedAt to now and writes a
	// `revoked` event, or answers AlreadyRevoked, changing nothing. It
	// returns the stored token; ErrNotFound.
	Revoke(ctx context.Context, organizationID, fleetID, id uuid.UUID, actor *fleets.Actor, now time.Time) (RevokeResult, Token, error)
	// RecordViewed writes a `viewed` event for the fleet's token id, valid
	// at now, and returns the token: what a reveal does. ErrNotFound,
	// ErrRevoked or ErrExpired, writing nothing.
	RecordViewed(ctx context.Context, organizationID, fleetID, id uuid.UUID, actor *fleets.Actor, now time.Time) (Token, error)
	// ListEvents returns a page of the fleet's token events, newest first
	// (by OccurredAt, then id, both descending), of at most limit (1 to
	// MaxListLimit, else ErrInvalid) after the cursor (nil: from the
	// newest); ErrFleetNotFound, also for an empty page of a fleet the
	// organization doesn't hold.
	ListEvents(ctx context.Context, organizationID, fleetID uuid.UUID, limit int, after *EventCursor) (EventPage, error)
	// GetActiveRotate returns the fleet's current rotate token valid at
	// now (the one a collector is pushed), or ErrNotFound when it has none.
	GetActiveRotate(ctx context.Context, organizationID, fleetID uuid.UUID, now time.Time) (Token, error)
	// RotateStates returns, for each of the organization's fleets among
	// fleetIDs (at most fleets.MaxListLimit; more is ErrInvalid), its rotate
	// tokens' state at now, in one grouped query: the current one as
	// GetActiveRotate picks it, the grace, the count. A fleet with no valid
	// rotate token, or one the organization doesn't hold, has no entry.
	RotateStates(ctx context.Context, organizationID uuid.UUID, fleetIDs []uuid.UUID, now time.Time) (map[uuid.UUID]RotateState, error)
	// Lookup returns token id, with its fleet's type and status, whatever
	// its organization, or ErrNotFound: the agent's authentication, which
	// learns the organization from the row. The caller checks validity
	// (Token.ValidAt), the fleet's type and status, and that the bearer's
	// fleet is the row's.
	Lookup(ctx context.Context, id uuid.UUID) (Authenticated, error)
}

// validate checks a rotation's values.
func (r Rotation) validate() error {
	now, grace := at(r.At), at(r.GraceEndsAt)
	switch {
	case !fleets.ValidActor(r.Actor):
		return invalid("actor")
	case !grace.After(now):
		return invalid("grace end")
	case r.ExpiresAt != nil && grace.After(at(*r.ExpiresAt)):
		return invalid("grace end")
	}
	return nil
}

func invalid(field string) error {
	return fmt.Errorf("%w: %s", ErrInvalid, field)
}

func at(t time.Time) time.Time {
	return t.UTC().Truncate(fleets.Precision)
}

func actorColumns(a *fleets.Actor) (*uuid.UUID, *string) {
	if a == nil {
		return nil, nil
	}
	id, email := a.ID, a.Email
	return &id, &email
}

func actorOf(id *uuid.UUID, email *string) *fleets.Actor {
	if id == nil || email == nil {
		return nil
	}
	return &fleets.Actor{ID: *id, Email: *email}
}
