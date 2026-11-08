// Package lru is the agent side's one cache shape: least recently used,
// bounded in entries and, optionally, in bytes, with an optional time to
// live read from an injected clock. Each replica keeps its own caches for speed only; the database is
// the only source of truth, and each cache's bound on staleness is its time
// to live.
//
// A Cache is safe for concurrent use. It never reads the time itself: the
// clock is the caller's (CONTRIBUTING.md, "Time").
package lru

import (
	"container/list"
	"sync"
	"time"
)

// Options configure a Cache.
type Options[K comparable, V any] struct {
	// MaxEntries is the most entries kept (at least 1).
	MaxEntries int
	// MaxBytes, when above zero, is the most bytes kept, as Size counts them.
	MaxBytes int64
	// MaxEntryBytes, when above zero, is the largest entry kept: Put refuses
	// a larger one (and drops any older value under its key).
	MaxEntryBytes int64
	// Size is an entry's bytes; nil counts every entry as 1.
	Size func(K, V) int64
	// TTL, when above zero, is how long an entry is answered after its Put,
	// by Now: at TTL it is a miss.
	TTL time.Duration
	// Now is the clock; required when TTL is above zero.
	Now func() time.Time
}

// Cache is a least recently used cache (see the package comment).
type Cache[K comparable, V any] struct {
	o Options[K, V]

	mu      sync.Mutex
	order   *list.List // front = most recently used
	entries map[K]*list.Element
	bytes   int64
}

type entry[K comparable, V any] struct {
	key      K
	value    V
	size     int64
	storedAt time.Time
}

// New returns an empty cache. It panics on options that can't bound it
// (MaxEntries below 1, or a TTL without a clock): a programming error.
func New[K comparable, V any](o Options[K, V]) *Cache[K, V] {
	if o.MaxEntries < 1 {
		panic("lru: MaxEntries must be at least 1")
	}
	if o.TTL > 0 && o.Now == nil {
		panic("lru: a TTL needs a clock")
	}
	return &Cache[K, V]{o: o, order: list.New(), entries: map[K]*list.Element{}}
}

// Get answers the value under key and marks it most recently used. An entry
// TTL old or older by the clock (or stored at a time after the clock's now,
// which a clock set back would show) is removed and missed.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	var zero V
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return zero, false
	}
	e := el.Value.(*entry[K, V])
	if c.o.TTL > 0 {
		age := c.o.Now().Sub(e.storedAt)
		if age < 0 || age >= c.o.TTL {
			c.remove(el)
			return zero, false
		}
	}
	c.order.MoveToFront(el)
	return e.value, true
}

// Put stores value under key, replacing any value there (one stored at a
// later time than the clock's now stays: see PutAt), and evicts the
// least recently used entries until the bounds hold. It reports whether the
// value is kept: an entry over MaxEntryBytes isn't (and the key's older
// value is dropped). The entry's age counts from the clock's now.
func (c *Cache[K, V]) Put(key K, value V) bool {
	var storedAt time.Time
	if c.o.Now != nil {
		storedAt = c.o.Now()
	}
	return c.PutAt(key, value, storedAt)
}

// PutAt is Put with the entry's age counted from readAt instead of the
// clock's now: a caller that read value from the source of truth at readAt
// passes that time, so the time the read took counts against the TTL and
// the entry's staleness stays within the TTL of the read. An entry under
// key stored at a later time than readAt is kept, and PutAt reports false:
// a slow read that started before a newer one finished never overwrites the
// newer value (a revocation, say) with the older.
func (c *Cache[K, V]) PutAt(key K, value V, readAt time.Time) bool {
	size := int64(1)
	if c.o.Size != nil {
		size = c.o.Size(key, value)
	}
	storedAt := readAt
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		if el.Value.(*entry[K, V]).storedAt.After(readAt) {
			return false
		}
		c.remove(el)
	}
	if (c.o.MaxEntryBytes > 0 && size > c.o.MaxEntryBytes) || (c.o.MaxBytes > 0 && size > c.o.MaxBytes) {
		return false
	}
	c.entries[key] = c.order.PushFront(&entry[K, V]{key: key, value: value, size: size, storedAt: storedAt})
	c.bytes += size
	for c.order.Len() > c.o.MaxEntries || (c.o.MaxBytes > 0 && c.bytes > c.o.MaxBytes) {
		c.remove(c.order.Back())
	}
	return true
}

// Remove drops the entry under key, if any.
func (c *Cache[K, V]) Remove(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		c.remove(el)
	}
}

// Len is how many entries are kept.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// Bytes is how many bytes are kept, as Size counts them.
func (c *Cache[K, V]) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

// remove drops el; the caller holds mu.
func (c *Cache[K, V]) remove(el *list.Element) {
	e := el.Value.(*entry[K, V])
	c.order.Remove(el)
	delete(c.entries, e.key)
	c.bytes -= e.size
}
