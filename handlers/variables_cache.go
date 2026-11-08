package handlers

import (
	"github.com/google/uuid"

	"github.com/telflo/opamp-server/internal/lru"
	"github.com/telflo/opamp-server/store/orgvariables"
)

// The variables cache's bounds: 64 MiB of resolved variables in all, and no
// entry over 16 MiB (150 values of 1,024 characters of up to 4 bytes, with
// their names, are about 0.65 MB).
const (
	VariablesCacheMaxBytes      = 64 << 20
	VariablesCacheMaxEntryBytes = 16 << 20
	// variablesCacheEntryOverhead is what an entry costs beyond its
	// variables (the key and the list's bookkeeping), and
	// variablesCacheVariableOverhead what each variable costs beyond its
	// name and value (its stamp and the maps' bookkeeping), counted so the
	// bound in bytes bounds the entries too.
	variablesCacheEntryOverhead    = 256
	variablesCacheVariableOverhead = 64
	// variablesCacheMaxEntries is the entry bound the byte bound implies.
	variablesCacheMaxEntries = VariablesCacheMaxBytes / variablesCacheEntryOverhead
)

// variablesCacheKey is an entry's key: the organization, its generation
// (organizations.generation), its counter
// (organizations.variables_revision) and the collector's two variable
// groups, uuid.Nil for none. An organization only ever reads what was
// stored under its own id.
type variablesCacheKey struct {
	organizationID        uuid.UUID
	generation            uuid.UUID
	revision              int64
	groupVariableGroupID  uuid.UUID
	pinnedVariableGroupID uuid.UUID
}

func newVariablesCacheKey(organizationID, generation uuid.UUID, revision int64, groupVariableGroupID, pinnedVariableGroupID *uuid.UUID) variablesCacheKey {
	k := variablesCacheKey{organizationID: organizationID, generation: generation, revision: revision}
	if groupVariableGroupID != nil {
		k.groupVariableGroupID = *groupVariableGroupID
	}
	if pinnedVariableGroupID != nil {
		k.pinnedVariableGroupID = *pinnedVariableGroupID
	}
	return k
}

// VariablesCache keeps the variables a collector's text is filled in with
// (orgvariables.Resolved) by (organization, generation, revision, the
// collector group's variable group, the pin's variable group), so a steady
// poll never reads them. It is per pod, least recently used, bounded at
// VariablesCacheMaxBytes in all, keeps no entry over
// VariablesCacheMaxEntryBytes, and has no time to live.
//
// Its staleness bound is exact: keyed by (organization, generation,
// counter, the two variable groups). Every write that can change any
// collector's filled-in text moves the organization's counter on in the
// write's transaction (variableuse.Bump), and the poll reads the counter
// and the generation in its own statement
// (collectors.PollResult.VariablesRevision and OrganizationGeneration), so
// no entry is ever served after a change: the next poll looks up the new
// revision and misses. The generation is new each time an organization is
// registered: one deleted and registered again under its id starts its
// counter at 0 again, and never reads the previous incarnation's entries.
// An entry is stored under the generation the poll statement read and the
// revision its read answered (orgvariables.Resolved.Revision), never the
// poll's revision, and only when the read answered that generation too
// (Resolved.Generation), so the values under one key never change.
type VariablesCache struct {
	cache *lru.Cache[variablesCacheKey, orgvariables.Resolved]
}

// NewVariablesCache returns an empty cache with the bounds above.
func NewVariablesCache() *VariablesCache {
	return newVariablesCacheSized(VariablesCacheMaxBytes, VariablesCacheMaxEntryBytes)
}

// newVariablesCacheSized is NewVariablesCache with other bounds, for tests.
func newVariablesCacheSized(maxBytes, maxEntryBytes int64) *VariablesCache {
	return &VariablesCache{cache: lru.New(lru.Options[variablesCacheKey, orgvariables.Resolved]{
		MaxEntries:    variablesCacheMaxEntries,
		MaxBytes:      maxBytes,
		MaxEntryBytes: maxEntryBytes,
		Size: func(_ variablesCacheKey, r orgvariables.Resolved) int64 {
			return variablesSize(r)
		},
	})}
}

// variablesSize is what an entry of r counts: Σ(len(name) + len(value) +
// 64) + 256.
func variablesSize(r orgvariables.Resolved) int64 {
	size := int64(variablesCacheEntryOverhead)
	for name, value := range r.Values {
		size += int64(len(name) + len(value) + variablesCacheVariableOverhead)
	}
	return size
}

// Get returns the variables of the organization's generation at revision
// for a collector of those variable groups (nil: none), marking them most
// recently used. The caller answers the variables a returned Resolved
// holds and never changes them.
func (c *VariablesCache) Get(organizationID, generation uuid.UUID, revision int64, groupVariableGroupID, pinnedVariableGroupID *uuid.UUID) (orgvariables.Resolved, bool) {
	if revision == 0 {
		return orgvariables.Resolved{}, false
	}
	return c.cache.Get(newVariablesCacheKey(organizationID, generation, revision, groupVariableGroupID, pinnedVariableGroupID))
}

// Put keeps r, read for the organization of that generation (as the poll
// statement read it) and those variable groups, under r.Revision, and
// reports whether it did: a Resolved of revision 0 (an organization that
// never had a variable, which the poll never reads) is never kept, nor one
// read at another generation than the poll statement's (r.Generation: the
// organization deleted and registered again between the two reads), nor
// one over VariablesCacheMaxEntryBytes.
func (c *VariablesCache) Put(organizationID, generation uuid.UUID, groupVariableGroupID, pinnedVariableGroupID *uuid.UUID, r orgvariables.Resolved) bool {
	if r.Revision == 0 || r.Generation != generation {
		return false
	}
	return c.cache.Put(newVariablesCacheKey(organizationID, generation, r.Revision, groupVariableGroupID, pinnedVariableGroupID), r)
}

// Len is how many entries are kept: for tests and diagnostics.
func (c *VariablesCache) Len() int {
	return c.cache.Len()
}

// Bytes is how many bytes are kept, overhead included.
func (c *VariablesCache) Bytes() int64 {
	return c.cache.Bytes()
}
