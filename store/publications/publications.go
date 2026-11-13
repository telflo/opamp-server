// Package publications stores publications: immutable, self-describing
// snapshots of one version of a configuration, as the platform published it.
//
// Its implementation is Postgres (what `serve` uses, on the
// opamp_server.publications table), and the contract tests hold it to these
// rules:
//
//   - every method is scoped to one organization, the opaque tenant key, and
//     never sees or changes another organization's rows;
//   - a publication belongs to a registered organization: Put under one the
//     registry doesn't hold is ErrOrganizationNotFound, and deleting the
//     organization deletes its publications (the table's foreign key);
//   - a publication's id is unique across every organization, as the
//     table's primary key is, so an id another organization holds is a
//     conflict, never a second row;
//   - nothing is updated: Put creates, or finds the identical row, or
//     reports a conflict; Delete and DeleteForConfiguration hard-delete,
//     except a publication a collector group is assigned or a collector is
//     pinned to, which neither deletes (ErrInUse, deleting nothing);
//   - nothing reads the time: ReceivedAt is set by the caller from its
//     injected clock, and times are kept to the millisecond, the precision
//     the API answers in (Precision);
//   - a Put that creates also stores the NAMEs of its YAML's live
//     ${org:NAME} references (orgvariables.References), each once, in the
//     same statement: written at publish only, and gone with the
//     publication. A Put that stores nothing (Unchanged, Conflict) stores no
//     names. A reference to a name over orgvariables.MaxNameLength
//     characters, which no variable can have, is refused before anything is
//     written (ErrReferenceTooLong).
package publications

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/orgvariables"
)

// Publication is one stored publication. List leaves YAML empty.
type Publication struct {
	ID                uuid.UUID
	OrganizationID    uuid.UUID
	ConfigurationID   uuid.UUID
	ConfigurationName string
	VersionID         uuid.UUID
	VersionNumber     int32
	// VersionLabel is nil for an unnamed version.
	VersionLabel *string
	YAML         string
	// SHA256 is YAML's SHA-256, 64 lowercase hex characters.
	SHA256 string
	// PublishedBy is nil for a publish no person made.
	PublishedBy *Actor
	PublishedAt time.Time
	// ReceivedAt is when this server stored it, by the caller's clock.
	ReceivedAt time.Time
	// Collector is the collector the configuration was published for. Every
	// publication has one.
	Collector Collector
}

// Actor is who published, as the platform knew them at the time.
type Actor struct {
	ID    uuid.UUID
	Email string
}

// Collector is the collector distribution and version a publication was
// published for: the platform's catalogue's ids, kept as plain values (the
// server holds no catalogue). Nothing here resolves an image or binaries
// from them.
type Collector struct {
	// Distribution is the distribution's id, e.g. "otelcol-contrib".
	Distribution string
	// Version is the collector version, major.minor.patch, e.g. "0.162.0".
	Version string
}

// Precision is the precision the stores keep times at: the millisecond, the
// precision the API's replies carry (RFC 3339 with milliseconds), so a
// client that POSTs back the publishedAt it was answered gets the identical
// publication, never a conflict. The store truncates a publication's times
// to it (normalized), so it holds, compares and orders the instants it
// answers; the handler truncates too, so what it answers is exactly what is
// kept.
const Precision = time.Millisecond

// normalized is p with its times in UTC at Precision, as a store keeps them.
func (p Publication) normalized() Publication {
	p.PublishedAt = p.PublishedAt.UTC().Truncate(Precision)
	p.ReceivedAt = p.ReceivedAt.UTC().Truncate(Precision)
	return p
}

// SameAs reports whether p and q describe the same publication: every field
// equal but ReceivedAt (when each was received), the collector included,
// with PublishedAt compared as an instant at millisecond precision. Put's
// "identical" is SameAs, within one organization.
func (p Publication) SameAs(q Publication) bool {
	return p.ID == q.ID &&
		p.OrganizationID == q.OrganizationID &&
		p.ConfigurationID == q.ConfigurationID &&
		p.ConfigurationName == q.ConfigurationName &&
		p.VersionID == q.VersionID &&
		p.VersionNumber == q.VersionNumber &&
		equalPointers(p.VersionLabel, q.VersionLabel) &&
		p.YAML == q.YAML &&
		p.SHA256 == q.SHA256 &&
		equalPointers(p.PublishedBy, q.PublishedBy) &&
		p.PublishedAt.Truncate(Precision).Equal(q.PublishedAt.Truncate(Precision)) &&
		p.Collector == q.Collector
}

func equalPointers[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// PutResult is what Put did.
type PutResult int

const (
	// Created: no publication had the id; this one is stored.
	Created PutResult = iota + 1
	// Unchanged: the organization already holds an identical publication
	// under the id (SameAs); nothing changed, and Put returns the stored one.
	Unchanged
	// Conflict: the id is taken by a different publication, or by another
	// organization's. Put returns nothing about it.
	Conflict
)

func (r PutResult) String() string {
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

// ErrNotFound is Get's answer for an id the organization doesn't hold,
// whether or not another organization does.
var ErrNotFound = errors.New("publications: not found")

// ErrOrganizationNotFound is Put's answer for a publication whose
// organization isn't registered: the table's foreign key refuses it. The
// handler checks the organization first, so Put meets it only when the
// organization is deleted in between.
var ErrOrganizationNotFound = errors.New("publications: the organization isn't registered")

// ErrReferenceTooLong is Put's answer for a publication whose YAML has a
// live ${org:NAME} reference to a name over orgvariables.MaxNameLength (128)
// characters. No variable can have such a name, and the names table's check
// would refuse it (an incompressible one could even overflow its index's
// entry limit), so Put writes nothing. Its text names no name.
var ErrReferenceTooLong = errors.New("publications: the yaml references an organization variable name over 128 characters")

// variableNames is the names p's YAML references, each once, in the order
// first seen (orgvariables.References), or ErrReferenceTooLong when one is
// longer than a variable's name may be. Names are ASCII, so their length in
// bytes is their length in characters.
func variableNames(p Publication) ([]string, error) {
	names := orgvariables.References(p.YAML)
	for _, name := range names {
		if len(name) > orgvariables.MaxNameLength {
			return nil, ErrReferenceTooLong
		}
	}
	return names, nil
}

// ErrInUse is Delete's and DeleteForConfiguration's answer while a collector
// group is assigned, or a collector pinned to, a publication they would
// delete: the two NO ACTION keys refuse it, and nothing is deleted.
var ErrInUse = errors.New("publications: in use by a collector group or a collector")

// MaxListLimit is the most publications one List call returns.
const MaxListLimit = 100

// Store keeps publications. Every method is scoped to one organization: Put
// by its publication's OrganizationID, the others by their organizationID.
type Store interface {
	// Put stores p unless its id is taken (see PutResult), with the names
	// its YAML references when it creates it. It returns the stored
	// publication for Created and Unchanged, and a zero one for Conflict. A
	// new id under an organization that isn't registered is
	// ErrOrganizationNotFound. A YAML referencing a name over 128
	// characters is ErrReferenceTooLong, and nothing is written.
	Put(ctx context.Context, p Publication) (PutResult, Publication, error)
	// Get returns the organization's publication id, or ErrNotFound.
	Get(ctx context.Context, organizationID, id uuid.UUID) (Publication, error)
	// List returns up to limit (1 to MaxListLimit) of the organization's
	// publications of configurationID, newest first by PublishedAt and then
	// by id, both descending, starting after the cursor when it is non-nil.
	// Their YAML is left empty. Page.Next is set when more may follow.
	List(ctx context.Context, organizationID, configurationID uuid.UUID, limit int, after *Cursor) (Page, error)
	// Delete removes the organization's publication id, reporting whether
	// there was one, or answers ErrInUse, deleting nothing, while a
	// collector group or a collector uses it.
	Delete(ctx context.Context, organizationID, id uuid.UUID) (bool, error)
	// DeleteForConfiguration removes every one of the organization's
	// publications of configurationID, returning how many there were, or
	// answers ErrInUse, deleting none, while a collector group or a
	// collector uses any of them.
	DeleteForConfiguration(ctx context.Context, organizationID, configurationID uuid.UUID) (int, error)
	// Usage returns each of the organization's publications of
	// configurationID, in List's order and without their YAML, with what
	// uses it: the collector groups assigned it and how many collectors are
	// pinned to it. A configuration with no publication has none.
	Usage(ctx context.Context, organizationID, configurationID uuid.UUID) ([]Usage, error)
}

// Usage is one publication and what uses it: what keeps it, alone or with
// its configuration's, from being deleted (ErrInUse).
type Usage struct {
	// Publication is the publication, without its YAML.
	Publication Publication
	// Groups are the collector groups assigned it, by fleet and then group
	// id (ids compared as bytes, as Postgres compares uuids).
	Groups []GroupRef
	// PinnedCollectors is how many collectors are pinned to it.
	PinnedCollectors int
}

// InUse reports whether a group or a collector uses the publication.
func (u Usage) InUse() bool {
	return len(u.Groups) > 0 || u.PinnedCollectors > 0
}

// GroupRef names a collector group: its fleet and its id.
type GroupRef struct {
	FleetID uuid.UUID
	GroupID uuid.UUID
}

// Page is one page of List.
type Page struct {
	Publications []Publication
	// Next is where the next page starts, or nil on the last page.
	Next *Cursor
}

// Cursor is a position in a List: the last row of a page. Clients see it
// only as an opaque string (String, ParseCursor).
type Cursor struct {
	PublishedAt time.Time
	ID          uuid.UUID
}

// cursorBytes is a cursor's length before encoding: the instant in
// microseconds since the Unix epoch (8 bytes, big-endian) and the id. A
// cursor holds a stored row's PublishedAt, a whole millisecond, which
// microseconds carry exactly.
const cursorBytes = 8 + 16

// ErrInvalidCursor is ParseCursor's answer to a string String didn't make.
var ErrInvalidCursor = errors.New("publications: invalid cursor")

// cursorOf is the cursor just after p.
func cursorOf(p Publication) *Cursor {
	return &Cursor{PublishedAt: p.PublishedAt, ID: p.ID}
}

// String encodes the cursor as unpadded base64url: opaque to clients, and
// safe in a query string as it is.
func (c Cursor) String() string {
	var raw [cursorBytes]byte
	binary.BigEndian.PutUint64(raw[:8], uint64(c.PublishedAt.UnixMicro()))
	copy(raw[8:], c.ID[:])
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

// ParseCursor decodes a cursor String made, or answers ErrInvalidCursor:
// also for a time no stored publication can have, outside the years 0001 to
// 9999 or not a whole millisecond, which would otherwise reach the database
// as a time it can't hold.
func ParseCursor(s string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || len(raw) != cursorBytes {
		return Cursor{}, ErrInvalidCursor
	}
	micros := int64(binary.BigEndian.Uint64(raw[:8]))
	publishedAt := time.UnixMicro(micros).UTC()
	if micros%int64(Precision/time.Microsecond) != 0 || !InRange(publishedAt) {
		return Cursor{}, ErrInvalidCursor
	}
	var id uuid.UUID
	copy(id[:], raw[8:])
	return Cursor{PublishedAt: publishedAt, ID: id}, nil
}

// The instants a publication's times may hold: the years 0001 to 9999 in
// UTC, the range RFC 3339 can write.
var (
	earliest = time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)
	latest   = time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC)
)

// InRange reports whether t falls in the years 0001 to 9999, in UTC.
func InRange(t time.Time) bool {
	return !t.Before(earliest) && t.Before(latest)
}

// checkLimit refuses a List limit outside 1 to MaxListLimit: the handler
// validates it, so this is a programming error.
func checkLimit(limit int) error {
	if limit < 1 || limit > MaxListLimit {
		return errors.New("publications: list limit out of range")
	}
	return nil
}
