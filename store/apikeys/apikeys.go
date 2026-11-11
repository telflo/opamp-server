// Package apikeys stores the management API's keys: credentials the secret
// holder mints, each opening the routes of the organizations it names, or
// of every organization, until it expires or is revoked. The key itself is
// never stored, only the SHA-256 of its secret part.
//
// Its implementation is Postgres (what `serve` uses, on
// telflo_opamp.api_keys and api_key_organizations), and the contract tests
// hold it to these rules:
//
//   - keys belong to the server, not to an organization: no method takes
//     an organization, and Lookup is the management API's authentication,
//     by the key's id;
//   - Create refuses a key past MaxActive keys still valid (neither
//     revoked nor expired), under a lock, so two Creates can't both pass
//     the count; and a key naming an organization that isn't registered;
//   - a key's secret, its organizations and its expiry never change;
//     Revoke sets RevokedAt once, and no key is ever deleted;
//   - an organization's delete takes it from every key that names it, so
//     a key never opens an organization registered again under the same
//     id;
//   - nothing reads the time: every time is the caller's, kept to the
//     millisecond.
package apikeys

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/telflo-opamp-server/store/collectorgroups"
	"github.com/telflo/telflo-opamp-server/store/fleets"
)

// The bounds of a key.
const (
	// MaxActive is the most keys that may be valid at once: neither revoked
	// nor expired.
	MaxActive = 100
	// MaxNameLength and MaxDescriptionLength are the most characters a
	// key's name and description may have.
	MaxNameLength        = 80
	MaxDescriptionLength = 1000
	// MaxOrganizations is the most organizations one key may name.
	MaxOrganizations = 1000
)

// MaxLifetimeYears is how far ahead a key's expiry may be: two years from
// its creation.
const MaxLifetimeYears = 2

// LatestExpiry is the latest expiry a key made at now may have.
func LatestExpiry(now time.Time) time.Time {
	return now.AddDate(MaxLifetimeYears, 0, 0)
}

// Precision is the precision the store keeps times at: the millisecond, the
// precision the API's replies carry.
const Precision = time.Millisecond

// Key is one stored key, without its secret.
type Key struct {
	ID          uuid.UUID
	Name        string
	Description *string
	// AllOrganizations is true for a key that opens every organization,
	// registered now or later; OrganizationIDs is then nil.
	AllOrganizations bool
	// OrganizationIDs are the organizations a key limited to some opens,
	// in byte order: those it was made with that are still registered.
	OrganizationIDs []uuid.UUID
	ExpiresAt       time.Time
	// RevokedAt is when it was first revoked, or nil.
	RevokedAt *time.Time
	// LastUsedAt is when it last authenticated a request, or nil.
	LastUsedAt *time.Time
	CreatedAt  time.Time
}

// ValidAt reports whether the key authenticates at now: not revoked, and
// not yet expired.
func (k Key) ValidAt(now time.Time) bool {
	return k.RevokedAt == nil && now.Before(k.ExpiresAt)
}

// Opens reports whether the key opens the organization's routes.
func (k Key) Opens(organizationID uuid.UUID) bool {
	if k.AllOrganizations {
		return true
	}
	_, found := slices.BinarySearchFunc(k.OrganizationIDs, organizationID, compareIDs)
	return found
}

// compareIDs orders ids by their bytes, as Postgres orders uuids.
func compareIDs(a, b uuid.UUID) int {
	return slices.Compare(a[:], b[:])
}

// NewKey is a key to make.
type NewKey struct {
	ID          uuid.UUID
	Name        string
	Description *string
	// SecretSHA256 is the SHA-256 of the key's secret part.
	SecretSHA256 [sha256.Size]byte
	// AllOrganizations, or the organizations it opens (at least one, each
	// once, each registered).
	AllOrganizations bool
	OrganizationIDs  []uuid.UUID
	ExpiresAt        time.Time
	CreatedAt        time.Time
}

// Changes are a key's fields a PATCH may change: its name (nil leaves it)
// and its description (unset leaves it; Clear nulls it).
type Changes struct {
	Name        *string
	Description collectorgroups.Change[string]
}

// Authenticated is a key as Lookup reads it: with its secret's SHA-256, for
// the caller to compare.
type Authenticated struct {
	Key
	SecretSHA256 [sha256.Size]byte
}

var (
	// ErrNotFound is the answer for an id no key has.
	ErrNotFound = errors.New("apikeys: not found")
	// ErrLimitReached is Create's answer when MaxActive keys are valid.
	ErrLimitReached = errors.New("apikeys: active key limit reached")
	// ErrOrganizationNotFound is Create's answer for a key naming an
	// organization that isn't registered.
	ErrOrganizationNotFound = errors.New("apikeys: organization not found")
	// ErrInvalid is the answer for a key or a change whose fields the
	// table's checks refuse (ValidName, ValidDescription, an expiry not
	// after its creation, no organization), storing nothing.
	ErrInvalid = errors.New("apikeys: invalid key")
)

// ValidName reports whether name is a key's name: trimmed, 1 to
// MaxNameLength characters, with no control character.
func ValidName(name string) bool {
	return validTrimmedText(name, MaxNameLength)
}

// ValidDescription reports whether description is a key's description:
// none (nil), or trimmed, 1 to MaxDescriptionLength characters, with no
// control character.
func ValidDescription(description *string) bool {
	return description == nil || validTrimmedText(*description, MaxDescriptionLength)
}

func validTrimmedText(s string, max int) bool {
	return fleets.Trimmed(s) && fleets.ValidText(s, 1, max) && fleets.NoControl(s)
}

// valid reports whether k passes the table's checks, and names at least one
// organization, each once, unless it opens all of them (and then none).
func (k NewKey) valid() bool {
	if !ValidName(k.Name) || !ValidDescription(k.Description) || !k.ExpiresAt.After(k.CreatedAt) {
		return false
	}
	if k.AllOrganizations {
		return len(k.OrganizationIDs) == 0
	}
	sorted := slices.SortedFunc(slices.Values(k.OrganizationIDs), compareIDs)
	return len(sorted) > 0 && len(sorted) <= MaxOrganizations &&
		len(slices.CompactFunc(sorted, func(a, b uuid.UUID) bool { return a == b })) == len(k.OrganizationIDs)
}

// valid reports whether c's fields pass the table's checks.
func (c Changes) valid() bool {
	return (c.Name == nil || ValidName(*c.Name)) &&
		(!c.Description.Set || ValidDescription(c.Description.Value))
}

// Store keeps the keys.
type Store interface {
	// Create makes k, unless MaxActive keys are valid at k.CreatedAt
	// (ErrLimitReached), or it names an organization that isn't
	// registered (ErrOrganizationNotFound). It returns the stored key.
	Create(ctx context.Context, k NewKey) (Key, error)
	// List returns every key, revoked and expired ones too, newest first
	// (ties by id, descending).
	List(ctx context.Context) ([]Key, error)
	// Get returns the key id, or ErrNotFound.
	Get(ctx context.Context, id uuid.UUID) (Key, error)
	// Update applies c to the key id and returns it, or ErrNotFound. A
	// revoked or expired key may still be renamed.
	Update(ctx context.Context, id uuid.UUID, c Changes) (Key, error)
	// Revoke sets the key's RevokedAt to at, unless it is revoked already,
	// which keeps the first time; it returns the key, or ErrNotFound.
	Revoke(ctx context.Context, id uuid.UUID, at time.Time) (Key, error)
	// Lookup returns the key id with its secret's SHA-256, or ErrNotFound:
	// the management API's authentication.
	Lookup(ctx context.Context, id uuid.UUID) (Authenticated, error)
	// Touch sets the key's LastUsedAt to at, unless it is later already.
	Touch(ctx context.Context, id uuid.UUID, at time.Time) error
}
