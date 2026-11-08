package handlers

import (
	"github.com/google/uuid"

	"github.com/telflo/opamp-server/internal/lru"
)

// The YAML cache's bounds: 64 MiB of YAML in all, and no entry over 4 MiB
// (the request cap; a published version is far smaller).
const (
	YamlCacheMaxBytes      = 64 << 20
	YamlCacheMaxEntryBytes = 4 << 20
	// yamlCacheEntryOverhead is what an entry costs beyond its YAML (the key
	// and the list's bookkeeping), counted so the bound in bytes bounds the
	// entries too.
	yamlCacheEntryOverhead = 256
	// yamlCacheMaxEntries is the entry bound the byte bound implies.
	yamlCacheMaxEntries = YamlCacheMaxBytes / yamlCacheEntryOverhead
)

// yamlCacheKey is an entry's key: the organization AND the SHA-256. Keyed
// by the hash alone, one organization could be served the bytes another
// stored under the same hash; so an organization only ever reads what was
// stored under its own id.
type yamlCacheKey struct {
	organizationID uuid.UUID
	sha256         string
}

// YamlCache keeps configurations' bytes by (organization, SHA-256) so the
// steady-state poll never reads YAML it already has. A SHA-256 is a content
// address: the bytes under it can't change, so the cache needs no time to
// live. It is least recently used, bounded at YamlCacheMaxBytes in all,
// keeps no entry over YamlCacheMaxEntryBytes, and never keeps a miss: Put
// refuses an empty YAML (a failed read cached as "" would be pushed as an
// empty otelcol.yml). The caller puts only bytes the store read for the
// organization, after checking their SHA-256 against the one stored with
// them.
type YamlCache struct {
	cache *lru.Cache[yamlCacheKey, string]
}

// NewYamlCache returns an empty cache with the bounds above.
func NewYamlCache() *YamlCache {
	return newYamlCacheSized(YamlCacheMaxBytes, YamlCacheMaxEntryBytes)
}

// newYamlCacheSized is NewYamlCache with other bounds, for tests.
func newYamlCacheSized(maxBytes, maxEntryBytes int64) *YamlCache {
	return &YamlCache{cache: lru.New(lru.Options[yamlCacheKey, string]{
		MaxEntries:    yamlCacheMaxEntries,
		MaxBytes:      maxBytes,
		MaxEntryBytes: maxEntryBytes,
		Size: func(_ yamlCacheKey, yaml string) int64 {
			return int64(len(yaml)) + yamlCacheEntryOverhead
		},
	})}
}

// Get returns the organization's bytes stored under sha256, marking them
// most recently used.
func (c *YamlCache) Get(organizationID uuid.UUID, sha256 string) (string, bool) {
	if sha256 == "" {
		return "", false
	}
	return c.cache.Get(yamlCacheKey{organizationID, sha256})
}

// Put keeps the organization's yaml under sha256, and reports whether it
// did: an empty hash or YAML (a miss) is never kept, nor YAML over
// YamlCacheMaxEntryBytes.
func (c *YamlCache) Put(organizationID uuid.UUID, sha256, yaml string) bool {
	if sha256 == "" || yaml == "" {
		return false
	}
	return c.cache.Put(yamlCacheKey{organizationID, sha256}, yaml)
}

// Len is how many entries are kept: for tests and diagnostics.
func (c *YamlCache) Len() int {
	return c.cache.Len()
}

// Bytes is how many bytes are kept, overhead included.
func (c *YamlCache) Bytes() int64 {
	return c.cache.Bytes()
}
