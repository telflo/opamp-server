// Package collectors stores collectors (openapi.yaml's CollectorInstance) and
// their error history: what each collector is, which fleet and group it is
// in, what it was served, what it reports it runs, and when it was last
// seen.
//
// Its implementation is Postgres (what `serve` uses, on
// opamp_server.collector_instances and collector_errors), and the contract
// tests hold it to these rules:
//
//   - every method takes the organization, and the fleet where the row is a
//     fleet's, and touches their rows only: a collector of another
//     organization or another fleet reads as not found and changes nothing.
//     Two exceptions, by their nature: Poll finds a collector by its
//     organization and instance uid whatever its fleet (the caller compares
//     Collector.FleetID with its token's, and Poll writes nothing for a
//     token of another fleet), and Cleanup deletes every organization's
//     expired error rows;
//   - Enroll is atomic: the organization's row locked in its own
//     statement, then the fleet's `for key share` (the fleet before any row
//     it owns), then the group's, then the cap's count and the insert (`on
//     conflict (organization_id, instance_uid) do nothing`) in one
//     statement, then the row read in another. It never rebinds a collector
//     to another fleet (ErrFleetMismatch);
//   - Poll is one statement: the collector before its heartbeat, the
//     heartbeat coalesced to one write per HeartbeatCoalesce unless the
//     token changed, and what resolves for it by its SHA-256, never its
//     YAML;
//   - the agent's writes are one guarded update per poll (Report), writing
//     nothing when nothing changed;
//   - an error row trims its collector's history to its newest MaxErrors
//     in the same transaction; Cleanup deletes rows past their retention
//     under an advisory lock, so one replica at a time runs it;
//   - liveness and sync are computed from the caller's clock by
//     LivenessAt and Listed.Sync, and SummariesForFleets counts them the
//     same way in one grouped query; sync knows about organization
//     variables: a collector was served what it should run when its served
//     published version is the resolved one and its served-variables marker
//     is at least the wanted stamp, the newest change among the values its
//     text uses, which List, Get and SummariesForFleets compute with one
//     shared SQL fragment, set-based (wantedStamp);
//   - a pin and a pinned variable group are independent; a pin to a
//     published version is refused while it references a name no variable
//     of the organization defines (*UndefinedError); a pinned variable
//     group's change, and a move between groups whose variable groups
//     differ, move the organization's counter on and stamp the collector's
//     row with it; a move between different groups whose variable groups
//     are the same, or both none, carries the old group's setting's stamp
//     onto the collector's row, with no bump (carriedStamp: else a change
//     of the old group's variable group it wasn't yet served would read In
//     sync after the move);
//   - a write that pins, moves or sets a pinned variable group takes the
//     organization's row `for no key update` first, chosen from the request
//     before its first lock and never upgraded from a `for key share` (the
//     lock order on Postgres); a rename takes it `for key share`;
//   - nothing reads the time: every time is the caller's, from its injected
//     clock, kept to the millisecond (Precision).
//
// No method's error carries an argument's value: a name, a message, an
// attribute or a YAML never reaches an error, a log line or a span.
package collectors

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/fleets"
	"github.com/telflo/opamp-server/store/variableuse"
)

// Precision is the precision the stores keep times at (fleets.Precision).
const Precision = fleets.Precision

// HeartbeatCoalesce is how long a heartbeat is skipped after the last one
// when the token is unchanged: Poll writes last_seen_at only when it is at
// or before the poll's time less this.
const HeartbeatCoalesce = 15 * time.Second

// The bounds the tables check.
const (
	MaxNameLength             = 200
	MaxCollectorVersionLength = 200
	MaxAddressLength          = 100
	// MaxAttributesBytes bounds reported attributes as jsonb writes them out
	// (ck_collector_instances_reported_attributes_bounded).
	MaxAttributesBytes = 16384
	// MaxMessageBytes bounds an error message, in UTF-8 bytes: the store
	// truncates longer ones (BoundMessage).
	MaxMessageBytes = 8192
	// MaxErrors is how many error rows a collector keeps: each RecordError
	// trims its history to its newest MaxErrors.
	MaxErrors = 100
	// ErrorRetention is how long an error row is kept: the caller's cleanup
	// job passes its clock's now less this to Cleanup.
	ErrorRetention = 30 * 24 * time.Hour
	// MaxListLimit is the most rows one List or ListErrors call returns.
	MaxListLimit = 100
	// MaxQueryLength is the most characters List's Q may have.
	MaxQueryLength = 100
	// MaxBulkPin is the most ids one BulkPin call takes.
	MaxBulkPin = 500
	// MaxSummaryFleets is the most fleets one SummariesForFleets call takes: a page
	// of the fleet list.
	MaxSummaryFleets = fleets.MaxListLimit
	// CleanupBatchSize is how many error rows one of Cleanup's transactions
	// deletes.
	CleanupBatchSize = 1000
)

// CleanupLockKey is the transaction-level advisory lock Cleanup takes with
// pg_try_advisory_xact_lock before each batch, so one replica at a time
// deletes expired error rows: the bytes "telflo" followed by 0x0001, fixed
// here so every replica asks for the same key. No other code takes it.
const CleanupLockKey int64 = 0x74656c666c6f0001

// The liveness ladder, by how long ago a collector was last seen: online
// under OnlineWithin, warning under WarningWithin, unknown under
// UnknownWithin, offline from then on.
const (
	OnlineWithin  = 60 * time.Second
	WarningWithin = 120 * time.Second
	UnknownWithin = 300 * time.Second
)

// Liveness is a collector's place on the ladder.
type Liveness string

const (
	Online  Liveness = "online"
	Warning Liveness = "warning"
	Unknown Liveness = "unknown"
	Offline Liveness = "offline"
)

// LivenessAt is the liveness of a collector last seen at lastSeen, at now:
// the ladder's step its age falls in, each step's lower edge included (60
// seconds exactly is a warning, 120 unknown, 300 offline). A last seen
// after now (another replica's clock ahead) is online. SummariesForFleets counts by
// the same edges.
func LivenessAt(lastSeen, now time.Time) Liveness {
	age := now.Sub(lastSeen)
	switch {
	case age < OnlineWithin:
		return Online
	case age < WarningWithin:
		return Warning
	case age < UnknownWithin:
		return Unknown
	}
	return Offline
}

// Sync is how what a collector runs compares with what resolves for it
// (docs/STATE_MODEL.md's states), from hashes only: effective_sha256 is the
// hash the collector's RemoteConfigStatus reports, never the effective
// configuration's bytes.
type Sync string

const (
	// InSync: what resolves was served, and the collector reports it
	// applied, on the served hash, since it was served.
	InSync Sync = "in_sync"
	// Pending: what resolves isn't what was served, or the collector hasn't
	// reported since it was served, or it reports applying.
	Pending Sync = "pending"
	// Failed: what resolves was served, and the collector reports failing to
	// apply it since.
	Failed Sync = "failed"
	// Drifted: what resolves was served, and the collector reports applied
	// on another hash since.
	Drifted Sync = "drifted"
	// Unassigned: nothing resolves for the collector (no pin, and its group
	// has no assigned published version).
	Unassigned Sync = "unassigned"
)

// SyncOf is the sync of c, for which resolved resolves.
// SummariesForFleets classifies every collector by the same steps, in the
// same order:
//
//  1. nothing resolves: Unassigned;
//  2. Pending when it was never served, or served another published version
//     than the resolved one; when its served-variables marker is below the
//     wanted stamp, resolved.VariablesStamp (no marker counts as 0); or
//     when it hasn't reported since it was served;
//  3. it reports failing: Failed;
//  4. it reports applied: InSync on the served hash (the filled-in bytes'),
//     else Drifted;
//  5. else Pending.
//
// The served hash is the filled-in text's, so it is never compared with
// the published version's own.
func SyncOf(resolved Resolution, c Collector) Sync {
	switch {
	case resolved.From == FromNone:
		return Unassigned
	case c.Served == nil || c.Served.ID != resolved.ID || marker(c) < resolved.VariablesStamp ||
		c.ServedAt == nil || c.EffectiveAt == nil || c.EffectiveAt.Before(*c.ServedAt):
		return Pending
	case c.EffectiveStatus != nil && *c.EffectiveStatus == StatusFailed:
		return Failed
	case c.EffectiveStatus != nil && *c.EffectiveStatus == StatusApplied:
		if c.EffectiveSHA256 != nil && c.ServedSHA256 != nil && *c.EffectiveSHA256 == *c.ServedSHA256 {
			return InSync
		}
		return Drifted
	}
	return Pending
}

// marker is c's served-variables marker, 0 when it has none.
func marker(c Collector) int64 {
	if c.ServedVariablesStamp == nil {
		return 0
	}
	return *c.ServedVariablesStamp
}

// EffectiveStatus is what a collector last reported of the configuration it
// was served.
type EffectiveStatus string

const (
	StatusApplied  EffectiveStatus = "applied"
	StatusApplying EffectiveStatus = "applying"
	StatusFailed   EffectiveStatus = "failed"
)

// Valid reports whether s is a status.
func (s EffectiveStatus) Valid() bool {
	return s == StatusApplied || s == StatusApplying || s == StatusFailed
}

// Source is what a pinned or served configuration is: a published version
// of the organization, the only kind (a Ref keeps its Source, as the
// management API's pin keeps its kind).
type Source string

const (
	SourcePublication Source = "publication"
)

// Ref names a published version.
type Ref struct {
	Source Source
	ID     uuid.UUID
}

func (r Ref) valid() bool {
	return r.Source == SourcePublication && r.ID != uuid.Nil
}

// column is a ref as the table's nullable publication column.
func (r *Ref) column() *uuid.UUID {
	if r == nil {
		return nil
	}
	id := r.ID
	return &id
}

// refOf is the ref a nullable publication column holds, or nil.
func refOf(publication *uuid.UUID) *Ref {
	if publication == nil {
		return nil
	}
	return &Ref{Source: SourcePublication, ID: *publication}
}

// From is where a collector's resolution came from: its pinned published
// version, else its group's assigned published version, else nothing.
type From string

const (
	FromNone              From = "none"
	FromPinnedPublication From = "pinned_publication"
	FromGroupPublication  From = "group_publication"
)

// Resolution is what resolves for a collector: never its YAML (YAML reads
// that on a cache miss).
type Resolution struct {
	From From
	// ID and SHA256 are the published version's; zero for FromNone.
	ID     uuid.UUID
	SHA256 string
	// GroupVariableGroupID is the collector's group's variable group, or
	// nil, whatever From is: it applies under a pin too.
	// GroupBindingStamp is the group's setting's stamp, 0 for none.
	GroupVariableGroupID *uuid.UUID
	GroupBindingStamp    int64
	// VariablesStamp is the wanted stamp: the marker the collector must
	// have been served at least, 0 when its published version references
	// no name or nothing resolves. List, Get and SummariesForFleets compute
	// it; Poll leaves it 0 (the poll computes the marker from the values it
	// fills in with, orgvariables.WantedStamp).
	VariablesStamp int64
}

// Ref names what resolved, or is nil for FromNone.
func (r Resolution) Ref() *Ref {
	switch r.From {
	case FromPinnedPublication, FromGroupPublication:
		return &Ref{Source: SourcePublication, ID: r.ID}
	}
	return nil
}

// Collector is one stored collector. Each pointer is nil for SQL null.
type Collector struct {
	ID             uuid.UUID
	OrganizationID uuid.UUID
	FleetID        uuid.UUID
	GroupID        uuid.UUID
	InstanceUID    uuid.UUID
	Name           string
	// Pinned is the published version pinned on it, or nil.
	Pinned *Ref
	// Served is what it was last served (or reconciled to), or nil (also
	// when that published version was deleted since: ServedSHA256 stays).
	Served       *Ref
	ServedSHA256 *string
	ServedAt     *time.Time
	// EffectiveSHA256, EffectiveStatus, EffectiveError and EffectiveAt are
	// its last RemoteConfigStatus. Poll leaves EffectiveError nil.
	EffectiveSHA256      *string
	EffectiveStatus      *EffectiveStatus
	EffectiveError       *string
	EffectiveAt          *time.Time
	FullStateRequestedAt *time.Time
	// The reported fields: display only. Poll leaves ReportedAttributes nil.
	ReportedCollectorVersion *string
	ReportedIP               *string
	ReportedHostIP           *string
	ReportedAttributes       map[string]string
	// EnrollmentTokenID is the token of its fleet it last authenticated
	// with, or nil.
	EnrollmentTokenID *uuid.UUID
	// PinnedVariableGroupID is a variable group pinned on it, or nil,
	// independent of Pinned. PinnedVariableGroupStamp is the stamp of its
	// last change, or of a move between groups whose variable groups
	// differ, or, after a move between groups whose variable groups are the
	// same, at least the old group's setting's stamp (carriedStamp); nil if
	// none of these happened (internal: never answered).
	PinnedVariableGroupID    *uuid.UUID
	PinnedVariableGroupStamp *int64
	// ServedVariablesStamp is the marker: the newest stamp it was served
	// with, or nil before any.
	ServedVariablesStamp *int64
	LastSeenAt           time.Time
	CreatedAt            time.Time
	// UpdatedAt is when an operator last changed it (rename, move, pin):
	// CreatedAt until then.
	UpdatedAt time.Time
}

// Listed is a collector as List and Get answer it: the row and what
// resolves for it, from which the caller computes its liveness
// (LivenessAt) and sync (Sync).
type Listed struct {
	Collector
	Resolution Resolution
}

// Sync is SyncOf(l.Resolution, l.Collector).
func (l Listed) Sync() Sync {
	return SyncOf(l.Resolution, l.Collector)
}

// Enrollment is what Enroll stores for a collector's first poll.
type Enrollment struct {
	OrganizationID uuid.UUID
	// FleetID is its token's fleet (the token row's).
	FleetID     uuid.UUID
	InstanceUID uuid.UUID
	// TokenID is the token it authenticated with, of FleetID.
	TokenID uuid.UUID
	// DesiredName is its ServerName header, trimmed by the caller, or "":
	// its name when free and valid (ValidName), else a petname.
	DesiredName string
	// GroupID is the group it names (telflo.collector_group_id), or nil for
	// the fleet's Default group. Nil is for seeding and tests only: the
	// agent's door refuses a collector naming no group before Enroll, so a
	// poll always passes one.
	GroupID *uuid.UUID
	// At is the caller's clock's now: its CreatedAt, UpdatedAt and
	// LastSeenAt.
	At time.Time
}

// Enrolled is what Enroll answers.
type Enrolled struct {
	Collector Collector
	// Created is whether this call stored it; false when a collector of the
	// same instance uid in the same fleet was there first (a racing
	// enrollment), which is then answered as stored.
	Created bool
}

// PollRequest is a poll's identity: the organization and fleet from the
// token's row, the collector's instance uid, the token, and the caller's
// clock's now.
type PollRequest struct {
	OrganizationID uuid.UUID
	FleetID        uuid.UUID
	InstanceUID    uuid.UUID
	TokenID        uuid.UUID
	At             time.Time
}

// PollResult is what Poll answers.
type PollResult struct {
	// Found is whether the organization holds a collector of the instance
	// uid, whatever its fleet: the caller refuses a FleetID other than its
	// token's (Poll wrote nothing then).
	Found bool
	// Collector is the collector as it was before this poll's heartbeat,
	// without EffectiveError and ReportedAttributes.
	Collector Collector
	// Touched is whether this poll wrote the heartbeat (LastSeenAt = At,
	// EnrollmentTokenID = the token).
	Touched bool
	// Resolution is what resolves for it (its VariablesStamp left 0).
	Resolution Resolution
	// VariablesRevision is the organization's counter
	// (organizations.variables_revision), and OrganizationGeneration its
	// generation (organizations.generation), read in the same statement:
	// the variables cache's key.
	VariablesRevision      int64
	OrganizationGeneration uuid.UUID
}

// YAMLResult is a configuration's bytes and the SHA-256 stored with them.
type YAMLResult struct {
	YAML   string
	SHA256 string
}

// Report is one poll's writes to its collector: each part non-nil is
// written, all in one update, and the update writes nothing when every
// part is as stored.
type Report struct {
	OrganizationID uuid.UUID
	FleetID        uuid.UUID
	CollectorID    uuid.UUID
	// Served records a delivery: what was served, its SHA-256 and when.
	Served *Served
	// Reconciled moves what was served and its SHA-256 without a delivery:
	// the collector already runs those bytes (the bytes-match reconcile).
	// ServedAt moves only when it was never set. At most one of Served and
	// Reconciled.
	Reconciled *Reconciled
	// Effective records a RemoteConfigStatus; its At is written whenever
	// the part is given, so a re-report refreshes it.
	Effective *Effective
	// Reported records the fields the agent reported, each only when non-nil.
	Reported Reported
	// VariablesStamp, when non-nil, moves the served-variables marker
	// alone: a poll that pushed nothing, its bytes unchanged, but filled in
	// with newer values. Not with Served or Reconciled, which carry their
	// own.
	VariablesStamp *int64
	// FullStateRequestedAt records asking it for its full state.
	FullStateRequestedAt *time.Time
}

// Served is a delivery.
type Served struct {
	Ref    Ref
	SHA256 string
	At     time.Time
	// VariablesStamp is the marker written with it: the newest stamp the
	// text was filled in with (0 for none).
	VariablesStamp int64
}

// Reconciled is a served pointer moved without a delivery.
type Reconciled struct {
	Ref    Ref
	SHA256 string
	// At is ServedAt when the collector was never served (ServedAt null):
	// without it sync would read "pending" for ever. The agent side passes
	// the earlier of its clock's now and the collector's EffectiveAt. A
	// ServedAt already set is kept. The zero time is ErrInvalid.
	At time.Time
	// VariablesStamp is the marker written with it, as Served's.
	VariablesStamp int64
}

// Effective is a RemoteConfigStatus.
type Effective struct {
	SHA256 string
	Status EffectiveStatus
	// Error is its message, kept only with StatusFailed (else cleared),
	// sanitized and truncated to MaxMessageBytes by the store
	// (BoundMessage); "" for none.
	Error string
	At    time.Time
}

// Reported is what the agent reported of itself; each nil field is left
// as stored. The caller passes only values it read (an agent that sent no
// description leaves them nil); a value out of bounds is ErrInvalid.
type Reported struct {
	CollectorVersion *string
	IP               *string
	HostIP           *string
	// Attributes is its description's attributes; nil leaves them.
	Attributes map[string]string
}

// ErrorReport is one error row.
type ErrorReport struct {
	// ConfigSHA256 is the hash the collector failed to apply, or nil.
	ConfigSHA256 *string
	// Message is sanitized and truncated to MaxMessageBytes by the store
	// (BoundMessage); empty after that is ErrInvalid.
	Message string
	At      time.Time
	// OncePerHash writes the row only when the collector's history holds
	// no row of the same ConfigSHA256 and the same (bounded) Message: the
	// poll's "over 1 MiB" row, which every poll of that filled-in text would
	// otherwise repeat. It needs a ConfigSHA256.
	// Every other row is written each time.
	OncePerHash bool
}

// CollectorError is one stored error row.
type CollectorError struct {
	ID           uuid.UUID
	CollectorID  uuid.UUID
	ReportedAt   time.Time
	ConfigSHA256 *string
	Message      string
}

// ErrorPage is one page of ListErrors.
type ErrorPage struct {
	Errors []CollectorError
	// Next is where the next page starts, or nil on the last page.
	Next *ErrorCursor
}

// CleanupResult is what Cleanup did.
type CleanupResult struct {
	// Deleted is how many error rows it deleted.
	Deleted int
	// Held is whether it held the advisory lock; false when another
	// replica's Cleanup held it, and Deleted is then 0.
	Held bool
}

// Summary is a fleet's counts: its collectors, by liveness (Online +
// Warning + Unknown + Offline = Collectors) and by sync (InSync + Pending
// + Failed + Drifted + Unassigned = Collectors); and the latest LastSeenAt
// among them, nil when it has none.
type Summary struct {
	Collectors int
	Online     int
	Warning    int
	Unknown    int
	Offline    int
	InSync     int
	Pending    int
	Failed     int
	Drifted    int
	Unassigned int
	// LastSeenAt is the greatest LastSeenAt of the fleet's collectors, in
	// UTC, or nil when it has none.
	LastSeenAt *time.Time
}

// Sort is a List order's key.
type Sort string

const (
	SortCreated  Sort = "created"
	SortLastSeen Sort = "lastSeen"
	SortName     Sort = "name"
)

// Order is a List order's direction.
type Order string

const (
	Ascending  Order = "asc"
	Descending Order = "desc"
)

// ListOptions select a page of List.
type ListOptions struct {
	// Q, when not empty, keeps the collectors whose name, reported IP or
	// reported host IP contains it, ignoring case (ILIKE, with %, _ and \
	// taken literally); at most MaxQueryLength characters.
	Q string
	// GroupID, when non-nil, keeps the group's collectors.
	GroupID *uuid.UUID
	// Sort and Order are the order (ties by id in the same direction);
	// names compare byte by byte (collate "C").
	Sort  Sort
	Order Order
	// Limit is 1 to MaxListLimit.
	Limit int
	// After is where the page starts (the previous page's Next, of the same
	// Sort and Order), or nil.
	After *Cursor
}

// Page is one page of List.
type Page struct {
	Collectors []Listed
	// Next is where the next page starts, or nil on the last page.
	Next *Cursor
}

// Changes are an operator's changes to one collector (the management API's
// PATCH): each non-nil field is set, all of them in one transaction or
// none.
type Changes struct {
	// Name renames it: a name ValidName accepts, unique in the organization.
	Name *string
	// GroupID moves it into another of its fleet's groups.
	GroupID *uuid.UUID
	// Pin, when non-nil, pins Pin.Ref on it, or unpins it when Pin.Ref is
	// nil.
	Pin *PinChange
	// PinnedVariableGroup, when non-nil, pins its ID's variable group on
	// it, or clears its pinned variable group when ID is nil: independent
	// of Pin.
	PinnedVariableGroup *VariableGroupChange
}

// PinChange is a pin to set: Ref, or none.
type PinChange struct {
	Ref *Ref
}

// VariableGroupChange is a pinned variable group to set: ID, or none.
type VariableGroupChange struct {
	ID *uuid.UUID
}

// locksOrganization reports whether Update takes the organization's row
// `for no key update`, from the request alone, before its first lock (the
// lock order on Postgres): a pin, a pinned variable group or a move, even
// when the row then shows nothing would change. A rename alone takes it
// `for key share`.
func (c Changes) locksOrganization() bool {
	return c.Pin != nil || c.PinnedVariableGroup != nil || c.GroupID != nil
}

// validate checks the changes: at least one, each a value the table takes.
func (c Changes) validate() error {
	switch {
	case c.Name == nil && c.GroupID == nil && c.Pin == nil && c.PinnedVariableGroup == nil:
		return invalid("changes")
	case c.Name != nil && !ValidName(*c.Name):
		return invalid("name")
	case c.Pin != nil && c.Pin.Ref != nil && !c.Pin.Ref.valid():
		return invalid("ref")
	}
	return nil
}

// BulkPinResult is what BulkPin did, which the management API answers as
// `{"updated": [ids], "notFound": [ids]}`, each sorted.
type BulkPinResult struct {
	// Updated is the ids that are the fleet's collectors, each now pinned
	// as asked (whether or not it already was), sorted, without duplicates.
	Updated []uuid.UUID
	// Changed is how many of them changed.
	Changed int
	// NotFound is the ids the fleet doesn't hold, sorted, without
	// duplicates: reported, not an error.
	NotFound []uuid.UUID
}

var (
	// ErrNotFound is the answer for a collector the organization's fleet
	// doesn't hold, or (YAML) a published version the organization
	// doesn't.
	ErrNotFound = errors.New("collectors: not found")
	// ErrFleetNotFound is the answer for a fleet the organization doesn't
	// hold (or, from Enroll, an organization that isn't registered).
	ErrFleetNotFound = errors.New("collectors: fleet not found")
	// ErrForeignGroup is Enroll's answer for a group the collector names
	// that isn't its fleet's (or was deleted meanwhile): the agent's 403.
	ErrForeignGroup = errors.New("collectors: the group isn't the fleet's")
	// ErrFleetMismatch is Enroll's answer when the organization's collector
	// of that instance uid is in another fleet: it is never rebound.
	ErrFleetMismatch = errors.New("collectors: enrolled in another fleet")
	// ErrCapReached is Enroll's answer when the organization holds
	// max_collectors collectors already. It carries no id and no count.
	ErrCapReached = errors.New("collectors: collector limit reached")
	// ErrTokenNotFound is Enroll's answer for a token that isn't its
	// fleet's.
	ErrTokenNotFound = errors.New("collectors: token not found")
	// ErrNameTaken is Rename's answer for a name another of the
	// organization's collectors has.
	ErrNameTaken = errors.New("collectors: name taken")
	// ErrGroupNotFound is Move's answer for a group the fleet doesn't hold.
	ErrGroupNotFound = errors.New("collectors: group not found")
	// ErrPublicationNotFound is the answer for a published version the
	// organization doesn't hold (a pin, a served write).
	ErrPublicationNotFound = errors.New("collectors: published version not found")
	// ErrInvalid is the answer for a value refused; it is wrapped with the
	// field's name, never its value.
	ErrInvalid = errors.New("collectors: invalid")
	// ErrInvalidCursor is the answer for a cursor its String didn't make,
	// or one of another order.
	ErrInvalidCursor = errors.New("collectors: invalid cursor")
	// ErrVariableGroupNotFound is the answer for a pinned variable group the
	// organization doesn't hold (the store's own: it never imports
	// store/variables).
	ErrVariableGroupNotFound = errors.New("collectors: variable group not found")
	// ErrVariablesUndefined is what an *UndefinedError matches.
	ErrVariablesUndefined = errors.New("collectors: the published version references undefined variables")
	// errNoFreeName is Enroll's failure when every petname tried was taken
	// (wrapped with the operation's name).
	errNoFreeName = errors.New("no free name")
)

// UndefinedError is a pin refused: the names the published version
// references that no variable of the organization defines, at most
// variableuse.MaxListed in byte order, with their total. The names are
// configuration text: Error never quotes one.
type UndefinedError struct {
	Undefined variableuse.UndefinedNames
}

func (e *UndefinedError) Error() string        { return ErrVariablesUndefined.Error() }
func (e *UndefinedError) Is(target error) bool { return target == ErrVariablesUndefined }

// undefined is the refusal for names, or nil when there are none.
func undefined(names variableuse.UndefinedNames) error {
	if names.Total == 0 {
		return nil
	}
	return &UndefinedError{Undefined: names}
}

// Store keeps collectors and their error history. See the package comment
// for the rules every method keeps.
type Store interface {
	// Enroll stores a collector on its first poll (see Enrollment): its
	// group the one it names when that is its fleet's (ErrForeignGroup
	// otherwise), else the fleet's Default group; its name its desired name
	// when free, else a petname. A collector of the same instance uid
	// already in the fleet is answered as stored (Created false); one in
	// another fleet is ErrFleetMismatch. ErrCapReached at the
	// organization's max_collectors; ErrFleetNotFound; ErrTokenNotFound.
	Enroll(ctx context.Context, e Enrollment) (Enrolled, error)
	// Poll reads the collector of the poll's instance uid, writes its
	// heartbeat when due (see PollResult), and resolves what it is served,
	// in one statement.
	Poll(ctx context.Context, p PollRequest) (PollResult, error)
	// YAML returns what ref names, with its stored SHA-256: a published
	// version of the organization. ErrNotFound.
	YAML(ctx context.Context, organizationID uuid.UUID, ref Ref) (YAMLResult, error)
	// Report writes a poll's parts to the fleet's collector in one update,
	// and reports whether it wrote (false when nothing changed, or there is
	// no such collector). ErrPublicationNotFound for a served ref the
	// organization doesn't hold; ErrInvalid.
	Report(ctx context.Context, r Report) (bool, error)
	// RecordError stores an error row for the fleet's collector and trims
	// its history to its newest MaxErrors, in one transaction; with
	// OncePerHash, only when its history holds no row of the same hash and
	// message. ErrNotFound; ErrInvalid.
	RecordError(ctx context.Context, organizationID, fleetID, collectorID uuid.UUID, e ErrorReport) error
	// ListErrors returns a page of the fleet's collector's errors, newest
	// first (by ReportedAt, then id, both descending). ErrNotFound;
	// ErrInvalid for a limit outside 1 to MaxListLimit.
	ListErrors(ctx context.Context, organizationID, fleetID, collectorID uuid.UUID, limit int, after *ErrorCursor) (ErrorPage, error)
	// Cleanup deletes every organization's error rows reported before
	// cutoff, in batches, each under the advisory lock CleanupLockKey; it
	// stops when another Cleanup holds the lock.
	Cleanup(ctx context.Context, cutoff time.Time) (CleanupResult, error)
	// List returns a page of the fleet's collectors (see ListOptions).
	// ErrFleetNotFound; ErrInvalid; ErrInvalidCursor.
	List(ctx context.Context, organizationID, fleetID uuid.UUID, o ListOptions) (Page, error)
	// Get returns the fleet's collector id with what resolves for it.
	// ErrNotFound.
	Get(ctx context.Context, organizationID, fleetID, id uuid.UUID) (Listed, error)
	// Rename renames the fleet's collector id; UpdatedAt moves to at only
	// when the name changes. ErrNotFound, ErrNameTaken, ErrInvalid.
	Rename(ctx context.Context, organizationID, fleetID, id uuid.UUID, name string, at time.Time) (Collector, error)
	// Move moves the fleet's collector id into the fleet's group groupID;
	// UpdatedAt moves only on a change, and a move between groups whose
	// variable groups differ stamps the collector, one between groups of
	// the same variable group carries the old group's setting's stamp
	// (carriedStamp). ErrNotFound, ErrGroupNotFound.
	Move(ctx context.Context, organizationID, fleetID, id, groupID uuid.UUID, at time.Time) (Collector, error)
	// Pin pins ref on the fleet's collector id, replacing any pin, or, with
	// a nil ref, unpins it; UpdatedAt moves only on a change. ErrNotFound,
	// ErrPublicationNotFound (not the organization's), *UndefinedError,
	// ErrInvalid.
	Pin(ctx context.Context, organizationID, fleetID, id uuid.UUID, ref *Ref, at time.Time) (Collector, error)
	// Update applies changes to the fleet's collector id in one transaction,
	// all or nothing, and returns it with what resolves for it; UpdatedAt
	// moves to at only when something changes. Its refusals, in this order:
	// ErrInvalid (no change, or a value refused); ErrNotFound;
	// ErrGroupNotFound (a group the fleet doesn't hold);
	// ErrPublicationNotFound (not the organization's);
	// ErrVariableGroupNotFound (not the organization's); *UndefinedError
	// (a pin to a published version only); ErrNameTaken.
	Update(ctx context.Context, organizationID, fleetID, id uuid.UUID, c Changes, at time.Time) (Listed, error)
	// BulkPin changes the fleet's collectors among ids, 1 to MaxBulkPin of
	// them, in one transaction: a nil pin leaves each collector's pin as it
	// is, &PinChange{} unpins ("follow each collector's group"), and a Ref
	// pins it; a nil vg leaves each pinned variable group as it is,
	// &VariableGroupChange{} clears it, and an ID pins it. At least one of pin
	// and vg (ErrInvalid otherwise). The ids the fleet doesn't hold are
	// reported back. Its refusals, in this order: ErrInvalid;
	// ErrFleetNotFound; ErrPublicationNotFound; ErrVariableGroupNotFound;
	// *UndefinedError (a pin to a published version). One stamp for the call,
	// on every collector whose pinned variable group changes.
	BulkPin(ctx context.Context, organizationID, fleetID uuid.UUID, ids []uuid.UUID, pin *PinChange, vg *VariableGroupChange, at time.Time) (BulkPinResult, error)
	// Delete deletes the fleet's collector id and its errors, freeing its
	// slot under the cap; on its next poll it is asked for its full state,
	// and it enrolls again with the answer. It reports whether there was
	// one.
	Delete(ctx context.Context, organizationID, fleetID, id uuid.UUID) (bool, error)
	// SummariesForFleets counts the collectors of each of the organization's fleets
	// among fleetIDs (at most MaxSummaryFleets) by liveness at now and by
	// sync, with their latest last seen, in one grouped query. A fleet the
	// organization doesn't hold has no entry; one without collectors has a
	// zero Summary.
	SummariesForFleets(ctx context.Context, organizationID uuid.UUID, fleetIDs []uuid.UUID, now time.Time) (map[uuid.UUID]Summary, error)
}

// isAnswer reports whether err is one of the store's answers, not a
// failure.
func isAnswer(err error) bool {
	for _, answer := range []error{ErrNotFound, ErrFleetNotFound, ErrForeignGroup, ErrFleetMismatch,
		ErrCapReached, ErrTokenNotFound, ErrNameTaken, ErrGroupNotFound, ErrPublicationNotFound,
		ErrInvalid, ErrInvalidCursor, ErrVariableGroupNotFound, ErrVariablesUndefined} {
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

func invalid(field string) error {
	return fmt.Errorf("%w: %s", ErrInvalid, field)
}

// at is t as a store keeps it: UTC, at Precision.
func at(t time.Time) time.Time {
	return t.UTC().Truncate(Precision)
}

// ValidName reports whether name is a name the table accepts: no leading or
// trailing whitespace as Unicode counts it (fleets.Trimmed), 1 to
// MaxNameLength characters of valid UTF-8 without NUL; and, stricter than
// the table, no control character anywhere (unicode.IsControl: C0, DEL and
// C1, a tab and a newline among them). A collector's name is pushed back to
// it as its ServerName header (handlers' connection settings), and an HTTP
// header's value can't hold a control character (the client refuses the
// request), so a name that does is never stored.
func ValidName(name string) bool {
	return fleets.Trimmed(name) && fleets.ValidText(name, 1, MaxNameLength) && fleets.NoControl(name)
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidSHA256 reports whether s is a SHA-256 in lowercase hex, as the
// tables store them.
func ValidSHA256(s string) bool {
	return sha256Hex.MatchString(s)
}

// SHA256Hex is the lowercase hex of a raw hash, as the tables store it.
func SHA256Hex(sum []byte) string {
	return hex.EncodeToString(sum)
}

// BoundMessage is an error message as the store keeps it: invalid UTF-8 and
// NUL replaced with U+FFFD, then truncated to at most MaxMessageBytes on a
// character's boundary. The store applies it to Effective.Error and
// ErrorReport.Message; callers needn't.
func BoundMessage(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.ReplaceAll(s, "\x00", "�")
	if len(s) <= MaxMessageBytes {
		return s
	}
	cut := MaxMessageBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// attributesTextLength is how long jsonb writes attributes out, the length
// ck_collector_instances_reported_attributes_bounded checks: `{"k": "v",
// "k2": "v2"}`, each string escaped as jsonb escapes it.
func attributesTextLength(attributes map[string]string) int {
	if len(attributes) == 0 {
		return 2
	}
	n := 2 + 2*(len(attributes)-1) // the braces, and ", " between pairs
	for k, v := range attributes {
		n += jsonbStringLength(k) + 2 + jsonbStringLength(v) // ": "
	}
	return n
}

// jsonbStringLength is how many bytes jsonb's output gives s: its quotes,
// and each byte escaped as Postgres's escape_json does.
func jsonbStringLength(s string) int {
	n := 2
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' || c == '\\' || c == '\b' || c == '\f' || c == '\n' || c == '\r' || c == '\t':
			n += 2
		case c < 0x20:
			n += 6
		default:
			n++
		}
	}
	return n
}

// validAttributes reports whether attributes are what the table accepts:
// valid UTF-8 without NUL (jsonb refuses \u0000), within MaxAttributesBytes.
func validAttributes(attributes map[string]string) bool {
	for k, v := range attributes {
		for _, s := range []string{k, v} {
			if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
				return false
			}
		}
	}
	return attributesTextLength(attributes) <= MaxAttributesBytes
}

// ValidAttributes reports whether Report takes attributes as
// Reported.Attributes: valid UTF-8 without NUL, within MaxAttributesBytes
// as jsonb writes them out. The agent side bounds what a collector reports
// with it before writing.
func ValidAttributes(attributes map[string]string) bool {
	return validAttributes(attributes)
}

func validOptionalText(s *string, max int) bool {
	return s == nil || fleets.ValidText(*s, 1, max)
}

// validate checks a report's parts.
func (r Report) validate() error {
	switch {
	case r.Served != nil && r.Reconciled != nil:
		return invalid("served and reconciled")
	case r.Served != nil && (!r.Served.Ref.valid() || !ValidSHA256(r.Served.SHA256) || r.Served.VariablesStamp < 0):
		return invalid("served")
	case r.Reconciled != nil && (!r.Reconciled.Ref.valid() || !ValidSHA256(r.Reconciled.SHA256) || r.Reconciled.At.IsZero() ||
		r.Reconciled.VariablesStamp < 0):
		return invalid("reconciled")
	case r.VariablesStamp != nil && (*r.VariablesStamp < 0 || r.Served != nil || r.Reconciled != nil):
		return invalid("variables stamp")
	case r.Effective != nil && (!ValidSHA256(r.Effective.SHA256) || !r.Effective.Status.Valid()):
		return invalid("effective")
	case !validOptionalText(r.Reported.CollectorVersion, MaxCollectorVersionLength):
		return invalid("reported collector version")
	case !validOptionalText(r.Reported.IP, MaxAddressLength):
		return invalid("reported ip")
	case !validOptionalText(r.Reported.HostIP, MaxAddressLength):
		return invalid("reported host ip")
	case r.Reported.Attributes != nil && !validAttributes(r.Reported.Attributes):
		return invalid("reported attributes")
	}
	return nil
}

// effectiveError is the error column an Effective part writes: its bounded
// message with StatusFailed, else nil.
func (e Effective) effectiveError() *string {
	if e.Status != StatusFailed {
		return nil
	}
	if m := BoundMessage(e.Error); m != "" {
		return &m
	}
	return nil
}

// validate checks an error report, and returns its bounded message.
func (e ErrorReport) validate() (string, error) {
	if (e.ConfigSHA256 != nil && !ValidSHA256(*e.ConfigSHA256)) || (e.OncePerHash && e.ConfigSHA256 == nil) {
		return "", invalid("config sha256")
	}
	m := BoundMessage(e.Message)
	if m == "" {
		return "", invalid("message")
	}
	return m, nil
}

// checkOptions checks List's options.
func checkOptions(o ListOptions) error {
	switch {
	case o.Limit < 1 || o.Limit > MaxListLimit:
		return invalid("limit")
	case o.Sort != SortCreated && o.Sort != SortLastSeen && o.Sort != SortName:
		return invalid("sort")
	case o.Order != Ascending && o.Order != Descending:
		return invalid("order")
	case !utf8.ValidString(o.Q) || strings.ContainsRune(o.Q, 0) || utf8.RuneCountInString(o.Q) > MaxQueryLength:
		return invalid("q")
	case o.After != nil && (o.After.Sort != o.Sort || o.After.Order != o.Order):
		return ErrInvalidCursor
	}
	return nil
}

// likePattern is q as an ILIKE pattern matching it anywhere, with %, _ and
// \ (the escape) taken literally.
func likePattern(q string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
	return "%" + escaped + "%"
}

// Cursor is a position in a List: the last row of a page, in its order.
// Clients see it only as an opaque string (String, ParseCursor).
type Cursor struct {
	Sort  Sort
	Order Order
	// Time is the row's CreatedAt or LastSeenAt (SortCreated, SortLastSeen).
	Time time.Time
	// Name is the row's name (SortName).
	Name string
	ID   uuid.UUID
}

const cursorVersion = 1

var (
	sortCodes  = []Sort{SortCreated, SortLastSeen, SortName}
	orderCodes = []Order{Ascending, Descending}
)

// cursorOf is the cursor after l in the order o.
func cursorOf(l Listed, o ListOptions) *Cursor {
	c := &Cursor{Sort: o.Sort, Order: o.Order, ID: l.ID}
	switch o.Sort {
	case SortCreated:
		c.Time = l.CreatedAt
	case SortLastSeen:
		c.Time = l.LastSeenAt
	case SortName:
		c.Name = l.Name
	}
	return c
}

// String encodes the cursor as unpadded base64url: a version byte, the
// sort's and the order's codes, the key (the instant in microseconds since
// the Unix epoch, 8 bytes big-endian, or the name's bytes), then the id.
func (c Cursor) String() string {
	raw := []byte{cursorVersion, byte(slices.Index(sortCodes, c.Sort)), byte(slices.Index(orderCodes, c.Order))}
	if c.Sort == SortName {
		raw = append(raw, c.Name...)
	} else {
		raw = binary.BigEndian.AppendUint64(raw, uint64(c.Time.UnixMicro()))
	}
	raw = append(raw, c.ID[:]...)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// The instants a stored time may hold: the years 0001 to 9999 in UTC.
var (
	earliest = time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)
	latest   = time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC)
)

// parseInstant reads 8 bytes of microseconds, refusing what no stored time
// can be (outside the years 0001 to 9999, or not a whole millisecond).
func parseInstant(raw []byte) (time.Time, bool) {
	micros := int64(binary.BigEndian.Uint64(raw))
	t := time.UnixMicro(micros).UTC()
	if micros%int64(Precision/time.Microsecond) != 0 || t.Before(earliest) || !t.Before(latest) {
		return time.Time{}, false
	}
	return t, true
}

// ParseCursor decodes a cursor String made, or answers ErrInvalidCursor.
func ParseCursor(s string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || len(raw) < 3+16 || raw[0] != cursorVersion ||
		int(raw[1]) >= len(sortCodes) || int(raw[2]) >= len(orderCodes) {
		return Cursor{}, ErrInvalidCursor
	}
	c := Cursor{Sort: sortCodes[raw[1]], Order: orderCodes[raw[2]]}
	key, id := raw[3:len(raw)-16], raw[len(raw)-16:]
	copy(c.ID[:], id)
	if c.Sort == SortName {
		if !ValidName(string(key)) {
			return Cursor{}, ErrInvalidCursor
		}
		c.Name = string(key)
		return c, nil
	}
	if len(key) != 8 {
		return Cursor{}, ErrInvalidCursor
	}
	t, ok := parseInstant(key)
	if !ok {
		return Cursor{}, ErrInvalidCursor
	}
	c.Time = t
	return c, nil
}

// compareIDs compares ids as bytes, as Postgres compares uuids.
func compareIDs(a, b uuid.UUID) int {
	return strings.Compare(string(a[:]), string(b[:]))
}

// ErrorCursor is a position in ListErrors: the last row of a page. Clients
// see it only as an opaque string.
type ErrorCursor struct {
	ReportedAt time.Time
	ID         uuid.UUID
}

// String encodes the cursor as unpadded base64url: the instant in
// microseconds (8 bytes, big-endian) and the id.
func (c ErrorCursor) String() string {
	raw := binary.BigEndian.AppendUint64(nil, uint64(c.ReportedAt.UnixMicro()))
	return base64.RawURLEncoding.EncodeToString(append(raw, c.ID[:]...))
}

// ParseErrorCursor decodes a cursor ErrorCursor.String made, or answers
// ErrInvalidCursor.
func ParseErrorCursor(s string) (ErrorCursor, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || len(raw) != 8+16 {
		return ErrorCursor{}, ErrInvalidCursor
	}
	t, ok := parseInstant(raw[:8])
	if !ok {
		return ErrorCursor{}, ErrInvalidCursor
	}
	var id uuid.UUID
	copy(id[:], raw[8:])
	return ErrorCursor{ReportedAt: t, ID: id}, nil
}

// validBulkPin checks BulkPin's arguments: 1 to MaxBulkPin ids, at least
// one of pin and vg, and a valid ref.
func validBulkPin(ids []uuid.UUID, pin *PinChange, vg *VariableGroupChange) error {
	switch {
	case len(ids) < 1 || len(ids) > MaxBulkPin:
		return invalid("ids")
	case pin == nil && vg == nil:
		return invalid("changes")
	case pin != nil && pin.Ref != nil && !pin.Ref.valid():
		return invalid("ref")
	}
	return nil
}

// stampsCollector reports whether a collector's change is stamped on its
// row with the counter's next value: its pinned variable group set,
// changed or cleared, or a move between groups whose variable groups
// differ. A move between different groups whose variable groups are the
// same (or both none) moves no counter but carries the old group's
// setting's stamp (carriedStamp). The store decides by these.
func stampsCollector(group uuid.UUID, groupVariableGroup, pinned *uuid.UUID,
	nextGroup uuid.UUID, nextGroupVariableGroup, nextPinned *uuid.UUID) bool {
	return !sameID(pinned, nextPinned) || (group != nextGroup && !sameID(groupVariableGroup, nextGroupVariableGroup))
}

// carriedStamp is a collector's pinned_variable_group_stamp after a move
// between different groups whose variable groups are the same, or both
// none: the greater of its own stamp and the old group's setting's stamp,
// with no bump. The old group's variable group may have changed (stamped on
// that group) after the collector was last served: without the carry it
// would read In sync in the new group, whose own setting's stamp may be
// older, while it runs the values it was served before that change. Nil
// when both are nil (the column's null).
func carriedStamp(own, oldGroup *int64) *int64 {
	if own == nil && oldGroup == nil {
		return nil
	}
	carried := max(stampValue(own), stampValue(oldGroup))
	return &carried
}

// stampValue is a nullable stamp's value, 0 for none.
func stampValue(stamp *int64) int64 {
	if stamp == nil {
		return 0
	}
	return *stamp
}

// sameID reports whether two nullable ids are the same, both null
// included.
func sameID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// sortedIDs is ids sorted as Postgres sorts uuids, without duplicates.
func sortedIDs(ids []uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	slices.SortFunc(out, compareIDs)
	return slices.Compact(out)
}
