package lru_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/telflo/opamp-server/internal/lru"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

var start = time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)

func TestAnEntryIsAnsweredUntilItsTTLByTheInjectedClock(t *testing.T) {
	c := &clock{now: start}
	cache := lru.New(lru.Options[string, int]{MaxEntries: 4, TTL: 10 * time.Second, Now: c.Now})
	cache.Put("a", 1)
	c.Advance(10*time.Second - time.Nanosecond)
	if v, ok := cache.Get("a"); !ok || v != 1 {
		t.Fatalf("just under the TTL: %v, %v", v, ok)
	}
	c.Advance(time.Nanosecond)
	if _, ok := cache.Get("a"); ok {
		t.Fatal("at the TTL the entry was still answered")
	}
	if cache.Len() != 0 {
		t.Fatalf("an expired entry stayed: %d", cache.Len())
	}
	// A clock set back past an entry's store time misses it too.
	cache.Put("b", 2)
	c.Advance(-time.Second)
	if _, ok := cache.Get("b"); ok {
		t.Fatal("an entry stored in the clock's future was answered")
	}
}

// PutAt counts an entry's age from the time its value was read, not from
// the Put: a value read 4 s before it was stored is a miss 6 s after the
// Put.
func TestPutAtCountsTheAgeFromTheReadNotTheStore(t *testing.T) {
	c := &clock{now: start}
	cache := lru.New(lru.Options[string, int]{MaxEntries: 4, TTL: 10 * time.Second, Now: c.Now})
	readAt := c.Now()
	c.Advance(4 * time.Second) // the read took 4 s
	cache.PutAt("a", 1, readAt)
	c.Advance(6*time.Second - time.Nanosecond)
	if v, ok := cache.Get("a"); !ok || v != 1 {
		t.Fatalf("just under the TTL from the read: %v, %v", v, ok)
	}
	c.Advance(time.Nanosecond)
	if _, ok := cache.Get("a"); ok {
		t.Fatal("answered at the TTL from the read: its age counted from the store")
	}
}

// A slow read that started before a newer one finished never overwrites the
// newer entry: PutAt of a value read at t0, after one read at t0+2s was
// stored, keeps the newer value and reports false; a value read later than
// the stored one replaces it.
func TestPutAtKeepsTheEntryReadLater(t *testing.T) {
	c := &clock{now: start}
	cache := lru.New(lru.Options[string, string]{MaxEntries: 4, TTL: 10 * time.Second, Now: c.Now})
	older := c.Now()
	c.Advance(2 * time.Second)
	if !cache.PutAt("token", "revoked", c.Now()) {
		t.Fatal("the newer read wasn't stored")
	}
	c.Advance(2 * time.Second)
	if cache.PutAt("token", "valid", older) {
		t.Fatal("the older read was reported stored")
	}
	if v, ok := cache.Get("token"); !ok || v != "revoked" {
		t.Fatalf("the older read overwrote the newer: %q, %v", v, ok)
	}
	if !cache.PutAt("token", "rotated", c.Now()) {
		t.Fatal("a later read wasn't stored")
	}
	if v, _ := cache.Get("token"); v != "rotated" {
		t.Fatalf("a later read didn't replace the entry: %q", v)
	}
}

func TestTheLeastRecentlyUsedEntryGoesFirstAndTheEntriesStayBounded(t *testing.T) {
	cache := lru.New(lru.Options[string, int]{MaxEntries: 2})
	cache.Put("a", 1)
	cache.Put("b", 2)
	cache.Get("a")
	cache.Put("c", 3)
	if _, ok := cache.Get("b"); ok {
		t.Fatal("the least recently used entry stayed")
	}
	if _, ok := cache.Get("a"); !ok {
		t.Fatal("a recently used entry went")
	}
	for i := range 1000 {
		cache.Put(fmt.Sprint(i), i)
	}
	if cache.Len() != 2 {
		t.Fatalf("%d entries kept under a bound of 2", cache.Len())
	}
}

func TestTheBytesStayBoundedAndALargeEntryIsNeverKept(t *testing.T) {
	cache := lru.New(lru.Options[string, string]{MaxEntries: 1000, MaxBytes: 10, MaxEntryBytes: 4,
		Size: func(_ string, v string) int64 { return int64(len(v)) }})
	for i := range 5 {
		if !cache.Put(fmt.Sprint(i), "abc") {
			t.Fatalf("a 3-byte entry refused")
		}
	}
	if cache.Bytes() > 10 || cache.Len() != 3 {
		t.Fatalf("%d bytes in %d entries under a bound of 10", cache.Bytes(), cache.Len())
	}
	cache.Put("big", "abcd")
	if cache.Put("big", "abcde") {
		t.Fatal("an entry over MaxEntryBytes was kept")
	}
	if _, ok := cache.Get("big"); ok {
		t.Fatal("the key's older value survived a refused Put")
	}
	cache.Remove("4")
	if _, ok := cache.Get("4"); ok {
		t.Fatal("a removed entry was answered")
	}
}

func TestTheCacheIsSafeForConcurrentUse(t *testing.T) {
	c := &clock{now: start}
	cache := lru.New(lru.Options[int, int]{MaxEntries: 64, MaxBytes: 1 << 20, TTL: time.Second, Now: c.Now,
		Size: func(int, int) int64 { return 8 }})
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 1000 {
				cache.Put(g*1000+i%100, i)
				cache.Get(g*1000 + i%50)
				if i%100 == 0 {
					c.Advance(time.Millisecond)
				}
			}
		})
	}
	wg.Wait()
	if cache.Len() > 64 {
		t.Fatalf("%d entries under a bound of 64", cache.Len())
	}
}
