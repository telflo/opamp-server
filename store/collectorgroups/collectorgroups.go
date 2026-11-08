// Package collectorgroups stores a fleet's collector groups. Every fleet has
// a Default group from its creation (fleets.Store.Create makes it), and
// every collector is in a group of its own fleet; a group's assigned
// published version is served to its collectors that have no pin of their
// own.
//
// Its implementation is Postgres (what `serve` uses, on
// telflo_opamp.collector_groups), and the contract tests hold it to these
// rules:
//
//   - every method takes the organization and the fleet, and touches their
//     rows only: a group of another organization or another fleet reads as
//     not found and changes nothing;
//   - names are unique within a fleet as written (case-sensitive), trimmed
//     of whitespace, 1 to 80 characters; the Default group is named
//     "Default" and keeps that name;
//   - a group's annotations are the management client's own data, kept and
//     answered in canonical form and never read; a kubernetes-supervisor
//     fleet's install fields live there, under the platform's key;
//   - an assigned published version is the organization's own
//     (ErrPublicationNotFound otherwise), and can't then be deleted; a new
//     one is refused while it references a name no variable of the
//     organization defines (*UndefinedError);
//   - a group's variable group is the organization's own
//     (ErrVariableGroupNotFound otherwise), and can't then be deleted; its
//     set, change or clear moves the organization's counter on and stamps
//     the group's setting with it;
//   - a write that names a published version or a variable group takes the
//     organization's row `for no key update` first, chosen from the request
//     before its first lock and never upgraded from a `for key share`, which
//     would deadlock (CONTRIBUTING.md, "SQL"); every other write takes it
//     `for key share`;
//   - the Default group is never deleted (ErrIsDefault); deleting another
//     deletes its collectors (and their error history) in the same
//     transaction;
//   - nothing reads the time: every time is the caller's, kept to the
//     millisecond.
package collectorgroups

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/fleets"
	"github.com/telflo/opamp-server/store/variableuse"
)

// The bounds the table checks.
const (
	MaxNameLength = 80
	// MaxAnnotationsBytes bounds a group's annotations as the table's check
	// counts them: their jsonb text, a space after every comma and colon, so
	// the store and the table refuse alike.
	MaxAnnotationsBytes = 16384
)

// Group is one stored collector group.
type Group struct {
	ID             uuid.UUID
	OrganizationID uuid.UUID
	FleetID        uuid.UUID
	Name           string
	// IsDefault is whether it is its fleet's Default group.
	IsDefault bool
	// AssignedPublicationID is the published version served to the
	// group's collectors without a pin, or nil.
	AssignedPublicationID *uuid.UUID
	// Annotations is the management client's own data about the group, a
	// JSON object in canonical form (CanonicalAnnotations), or nil; on any
	// fleet's group, and never read by the server.
	Annotations json.RawMessage
	// VariableGroupID is the variable group its collectors' text is filled
	// in with, under their pin's, or nil.
	VariableGroupID *uuid.UUID
	// VariableGroupStamp is the organization's counter when VariableGroupID
	// was last set, changed or cleared, or nil if it never was: internal,
	// for the collectors' wanted stamp, never answered.
	VariableGroupStamp *int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// NewGroup is what Create makes.
type NewGroup struct {
	OrganizationID        uuid.UUID
	FleetID               uuid.UUID
	Name                  string
	AssignedPublicationID *uuid.UUID
	Annotations           json.RawMessage
	// VariableGroupID is the variable group it uses, or nil: one of the
	// organization's, stamped with the counter's next value.
	VariableGroupID *uuid.UUID
	// At is the caller's clock's now.
	At time.Time
}

// locksOrganization reports whether Create takes the organization's row `for
// no key update`: when it names a published version (the undefined check) or
// a variable group (a stamp).
func (g NewGroup) locksOrganization() bool {
	return g.AssignedPublicationID != nil || g.VariableGroupID != nil
}

// Change is one optional field of Changes: unset (the zero Change) leaves
// the field as it is; Set gives it a value; Clear nulls it.
type Change[T any] struct {
	Set   bool
	Value *T
}

// SetTo is a Change giving the field v.
func SetTo[T any](v T) Change[T] { return Change[T]{Set: true, Value: &v} }

// Clear is a Change nulling the field.
func Clear[T any]() Change[T] { return Change[T]{Set: true} }

// apply is the field after the change.
func (c Change[T]) apply(current *T) *T {
	if !c.Set {
		return current
	}
	return c.Value
}

// Changes is what Update changes.
type Changes struct {
	// Name, when non-nil, renames the group (not the Default group).
	Name                  *string
	AssignedPublicationID Change[uuid.UUID]
	Annotations           Change[json.RawMessage]
	// VariableGroupID sets, changes or clears the group's variable group.
	VariableGroupID Change[uuid.UUID]
}

// locksOrganization reports whether Update takes the organization's row `for
// no key update`, from the request alone, before its first lock: an
// assignment set to a published version (the undefined check), or the
// variable group set or cleared (a stamp), even when the row then shows
// nothing would change. A rename, the annotations or an assignment cleared
// take it `for key share`.
func (c Changes) locksOrganization() bool {
	return (c.AssignedPublicationID.Set && c.AssignedPublicationID.Value != nil) || c.VariableGroupID.Set
}

var (
	// ErrNotFound is the answer for a group the organization's fleet doesn't
	// hold.
	ErrNotFound = errors.New("collectorgroups: not found")
	// ErrFleetNotFound is the answer for a fleet the organization doesn't
	// hold.
	ErrFleetNotFound = errors.New("collectorgroups: fleet not found")
	// ErrNameTaken is the answer for a name another of the fleet's groups
	// has.
	ErrNameTaken = errors.New("collectorgroups: name taken")
	// ErrPublicationNotFound is the answer for an assignment naming a
	// published version the organization doesn't hold.
	ErrPublicationNotFound = errors.New("collectorgroups: publication not found")
	// ErrIsDefault is the answer for deleting or renaming the Default group.
	ErrIsDefault = errors.New("collectorgroups: the Default group")
	// ErrInvalid is the answer for a value the table would refuse; it is
	// wrapped with the field's name, never its value.
	ErrInvalid = errors.New("collectorgroups: invalid")
	// ErrInvalidAnnotations is ErrInvalid for the annotations (errors.Is
	// matches both), so a handler answers the annotations' own message.
	ErrInvalidAnnotations = fmt.Errorf("%w: annotations", ErrInvalid)
	// ErrVariableGroupNotFound is the answer for a variable group the
	// organization doesn't hold (the store's own: it never imports
	// store/variables).
	ErrVariableGroupNotFound = errors.New("collectorgroups: variable group not found")
	// ErrVariablesUndefined is what an *UndefinedError matches.
	ErrVariablesUndefined = errors.New("collectorgroups: the published version references undefined variables")
)

// UndefinedError is an assignment refused: the names the published version
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

// Store keeps collector groups. Every method is scoped to one
// organization's fleet.
type Store interface {
	// Create makes a group (never a Default one) and returns it. Its
	// refusals, in this order: ErrFleetNotFound; ErrInvalid;
	// ErrPublicationNotFound; ErrVariableGroupNotFound; *UndefinedError;
	// ErrNameTaken.
	Create(ctx context.Context, g NewGroup) (Group, error)
	// List returns the fleet's groups, the Default group first, then by
	// name and id (byte order), or ErrFleetNotFound.
	List(ctx context.Context, organizationID, fleetID uuid.UUID) ([]Group, error)
	// Get returns the fleet's group id, or ErrNotFound.
	Get(ctx context.Context, organizationID, fleetID, id uuid.UUID) (Group, error)
	// Update applies changes to the fleet's group id and returns it;
	// UpdatedAt moves to at only when something changes. Its refusals, in
	// this order: ErrNotFound; ErrInvalid or ErrIsDefault (renaming the
	// Default group); then, for what changes, ErrPublicationNotFound,
	// ErrVariableGroupNotFound, *UndefinedError (a new published version
	// only: a change of the variable group alone checks nothing, since
	// "defined" means an organization variable) and ErrNameTaken.
	Update(ctx context.Context, organizationID, fleetID, id uuid.UUID, changes Changes, at time.Time) (Group, error)
	// Delete removes the fleet's group id with its collectors and their
	// error history, and reports whether there was one. ErrIsDefault for
	// the Default group, deleting nothing.
	Delete(ctx context.Context, organizationID, fleetID, id uuid.UUID) (bool, error)
}

// ValidName reports whether name is a group name the table accepts, and,
// stricter than the table (as a collector's name), holds no control
// character (fleets.NoControl).
func ValidName(name string) bool {
	return fleets.Trimmed(name) && fleets.ValidText(name, 1, MaxNameLength) && fleets.NoControl(name)
}

// CanonicalAnnotations returns annotations as the store keeps and answers
// them, or ErrInvalid when they aren't a JSON object. The form: no
// insignificant space, object keys in sorted order (so an object is answered
// with its keys in byte order whatever order it was sent in), numbers as
// written, HTML characters unescaped.
//
// The store keeps and answers exactly these bytes: Postgres stores jsonb,
// which would rewrite some numbers (an exponent, 1e2, reads back 100; -0
// reads back 0) and can't hold U+0000 in a string, so such values are
// refused here, before the store, rather than answered rewritten. The bound
// is the platform contract's pair: at most MaxAnnotationsBytes both in this
// form and as jsonb's text (jsonbTextBytes, the table check's own count), so
// the table's check never refuses what this accepts, and the opamp-server
// refuses nothing the platform's contract takes.
func CanonicalAnnotations(annotations json.RawMessage) (json.RawMessage, error) {
	value, err := canonicalAnnotations(annotations, MaxAnnotationsBytes)
	if err != nil {
		return nil, err
	}
	if jsonbTextBytes(value) > MaxAnnotationsBytes {
		return nil, invalid("annotations")
	}
	return value, nil
}

// storedAnnotations is a stored row's annotations in canonical form, with no
// bound: a row the table's check admitted always reads back, though its
// canonical form may be longer than its jsonb text (each U+2028 or U+2029 is
// six bytes escaped, three as jsonb writes it) and so past the bound a write
// checks.
func storedAnnotations(annotations json.RawMessage) (json.RawMessage, error) {
	return canonicalAnnotations(annotations, math.MaxInt)
}

func canonicalAnnotations(annotations json.RawMessage, limit int) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(annotations))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil || !atEnd(decoder) {
		return nil, invalid("annotations")
	}
	return canonical(object, limit, "annotations")
}

// atEnd reports whether nothing but space follows the value decoded: a
// second value or a stray `}` or `]` (which More doesn't see) is refused.
func atEnd(decoder *json.Decoder) bool {
	_, err := decoder.Token()
	return err == io.EOF
}

// jsonbTextBytes is how long a canonical value's jsonb text is, exactly as
// the table's check counts it (octet_length of annotations::text): Postgres
// writes a space after every comma and colon outside a string, and escapes a
// string as encoding/json does but for the \uXXXX escapes of a character at
// or above U+0020 (encoding/json's U+2028, U+2029 and U+FFFD for bytes that
// aren't UTF-8), which it writes as the character itself: three bytes for
// each of these, not six.
func jsonbTextBytes(canonical []byte) int {
	length := len(canonical)
	inString := false
	for i := 0; i < len(canonical); i++ {
		b := canonical[i]
		switch {
		case inString && b == '\\':
			if i+5 < len(canonical) && canonical[i+1] == 'u' {
				if code, err := strconv.ParseUint(string(canonical[i+2:i+6]), 16, 32); err == nil && code >= 0x20 {
					length -= 6 - utf8.RuneLen(rune(code))
				}
			}
			i++ // the escaped character (or the u) is never a quote that ends the string
		case b == '"':
			inString = !inString
		case !inString && (b == ',' || b == ':'):
			length++
		}
	}
	return length
}

// canonical is value in the stores' form, or ErrInvalid naming field when
// jsonb wouldn't keep it as written or it is over limit bytes.
func canonical(value any, limit int, field string) (json.RawMessage, error) {
	if !jsonbKeepsAsWritten(value) {
		return nil, invalid(field)
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, invalid(field)
	}
	canonical := bytes.TrimSuffix(out.Bytes(), []byte("\n"))
	if len(canonical) > limit {
		return nil, invalid(field)
	}
	return json.RawMessage(canonical), nil
}

// jsonbKeepsAsWritten reports whether jsonb stores value (decoded with
// UseNumber) as it reads: no number with an exponent or a negative zero,
// which numeric rewrites, and no U+0000 in a string or a key, which jsonb
// refuses.
func jsonbKeepsAsWritten(value any) bool {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			if !jsonbKeepsAsWritten(item) {
				return false
			}
		}
	case map[string]any:
		for key, item := range v {
			if strings.ContainsRune(key, 0) || !jsonbKeepsAsWritten(item) {
				return false
			}
		}
	case string:
		return !strings.ContainsRune(v, 0)
	case json.Number:
		number := string(v)
		if strings.ContainsAny(number, "eE") {
			return false
		}
		if strings.HasPrefix(number, "-") && strings.Trim(number, "-0.") == "" {
			return false
		}
	}
	return true
}

// sameCanonical reports whether two canonical values (annotations) are the
// same value.
func sameCanonical(a, b json.RawMessage) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return bytes.Equal(a, b)
}

// prepare checks a new group as the table would, and returns the row
// Create stores (without its id).
func (g NewGroup) prepare() (Group, error) {
	if !ValidName(g.Name) {
		return Group{}, invalid("name")
	}
	var annotations json.RawMessage
	if g.Annotations != nil {
		var err error
		if annotations, err = CanonicalAnnotations(g.Annotations); err != nil {
			return Group{}, err
		}
	}
	now := at(g.At)
	return Group{
		OrganizationID: g.OrganizationID, FleetID: g.FleetID, Name: g.Name,
		AssignedPublicationID: copyOf(g.AssignedPublicationID), Annotations: annotations,
		VariableGroupID: copyOf(g.VariableGroupID), CreatedAt: now, UpdatedAt: now,
	}, nil
}

// apply is current after changes, and whether anything changed (UpdatedAt
// then moves to now). The store's Update writes what it answers.
func (c Changes) apply(current Group, now time.Time) (Group, bool, error) {
	next := current
	if c.Name != nil {
		if !ValidName(*c.Name) {
			return Group{}, false, invalid("name")
		}
		if current.IsDefault && *c.Name != current.Name {
			return Group{}, false, ErrIsDefault
		}
		next.Name = *c.Name
	}
	next.AssignedPublicationID = copyOf(c.AssignedPublicationID.apply(current.AssignedPublicationID))
	next.VariableGroupID = copyOf(c.VariableGroupID.apply(current.VariableGroupID))
	// Annotations on any fleet's group: an object replaces them whole, a
	// clear nulls them.
	if c.Annotations.Set {
		next.Annotations = nil
		if c.Annotations.Value != nil {
			var err error
			if next.Annotations, err = CanonicalAnnotations(*c.Annotations.Value); err != nil {
				return Group{}, false, err
			}
		}
	}

	changed := next.Name != current.Name ||
		!reflect.DeepEqual(next.AssignedPublicationID, current.AssignedPublicationID) ||
		!sameCanonical(next.Annotations, current.Annotations) ||
		!reflect.DeepEqual(next.VariableGroupID, current.VariableGroupID)
	if changed {
		next.UpdatedAt = at(now)
	}
	return next, changed, nil
}

// newPublication is the published version next is newly assigned, the one
// the undefined check reads, or nil. A re-assignment of the version the
// group already has isn't checked again (a collector's same-version re-pin
// is): harmless, since a variable a used version references can't be deleted
// or renamed meanwhile, so its names are still defined.
func newPublication(current, next Group) *uuid.UUID {
	if next.AssignedPublicationID == nil || reflect.DeepEqual(next.AssignedPublicationID, current.AssignedPublicationID) {
		return nil
	}
	return next.AssignedPublicationID
}

// newVariableGroup is the variable group next newly uses, the one checked
// to be the organization's, or nil.
func newVariableGroup(current, next Group) *uuid.UUID {
	if next.VariableGroupID == nil || reflect.DeepEqual(next.VariableGroupID, current.VariableGroupID) {
		return nil
	}
	return next.VariableGroupID
}

// variableGroupChanged reports whether next's variable group is set,
// changed or cleared from current's: the change Update stamps.
func variableGroupChanged(current, next Group) bool {
	return !reflect.DeepEqual(next.VariableGroupID, current.VariableGroupID)
}

func invalid(field string) error {
	if field == "annotations" {
		return ErrInvalidAnnotations
	}
	return fmt.Errorf("%w: %s", ErrInvalid, field)
}

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
