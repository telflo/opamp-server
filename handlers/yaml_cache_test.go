package handlers

import (
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

var (
	cacheOrgA = uuid.MustParse("0a0a0a0a-0000-4000-8000-00000000000a")
	cacheOrgB = uuid.MustParse("0b0b0b0b-0000-4000-8000-00000000000b")
)

func TestYamlCacheRoundTripsWithinAnOrganization(t *testing.T) {
	c := NewYamlCache()
	if _, ok := c.Get(cacheOrgA, "hash-1"); ok {
		t.Fatal("a hit on an empty cache")
	}
	if !c.Put(cacheOrgA, "hash-1", "receivers: {}") {
		t.Fatal("a small YAML wasn't kept")
	}
	if got, ok := c.Get(cacheOrgA, "hash-1"); !ok || got != "receivers: {}" {
		t.Fatalf("Get = %q, %v", got, ok)
	}
}

// The cache never answers one organization's bytes to another asking for
// the same SHA-256.
func TestYamlCacheNeverAnswersAnotherOrganizationsBytesForTheSameHash(t *testing.T) {
	c := NewYamlCache()
	c.Put(cacheOrgA, "same-sha", "# A's bytes\n")
	if got, ok := c.Get(cacheOrgB, "same-sha"); ok {
		t.Fatalf("B was answered A's bytes for the same hash: %q", got)
	}
	c.Put(cacheOrgB, "same-sha", "# B's bytes\n")
	if got, _ := c.Get(cacheOrgA, "same-sha"); got != "# A's bytes\n" {
		t.Fatalf("A reads %q after B's Put", got)
	}
	if got, _ := c.Get(cacheOrgB, "same-sha"); got != "# B's bytes\n" {
		t.Fatalf("B reads %q", got)
	}
}

// A miss is never cached: an empty YAML or hash is refused.
func TestYamlCacheNeverKeepsAMissOrAnEmptyHash(t *testing.T) {
	c := NewYamlCache()
	if c.Put(cacheOrgA, "hash-1", "") || c.Put(cacheOrgA, "", "receivers: {}") {
		t.Fatal("a miss or an empty hash was kept")
	}
	if _, ok := c.Get(cacheOrgA, "hash-1"); ok {
		t.Fatal("a miss was answered")
	}
	if c.Len() != 0 {
		t.Fatalf("%d entries after refused Puts", c.Len())
	}
}

// 64 MiB in all, none over 4 MiB.
func TestYamlCacheIsBoundedInBytesAndKeepsNoEntryOver4MiB(t *testing.T) {
	c := NewYamlCache()
	over := strings.Repeat("x", YamlCacheMaxEntryBytes+1)
	if c.Put(cacheOrgA, "big", over) {
		t.Fatal("an entry over 4 MiB was kept")
	}
	mib := strings.Repeat("y", 1<<20)
	for i := range 100 {
		c.Put(cacheOrgA, strconv.Itoa(i), mib)
	}
	if c.Bytes() > YamlCacheMaxBytes {
		t.Fatalf("%d bytes kept under a bound of %d", c.Bytes(), YamlCacheMaxBytes)
	}
	if _, ok := c.Get(cacheOrgA, "0"); ok {
		t.Fatal("the least recently used entry survived the bound")
	}
	if _, ok := c.Get(cacheOrgA, "99"); !ok {
		t.Fatal("the newest entry went")
	}
}

func TestYamlCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := newYamlCacheSized(2*(1+yamlCacheEntryOverhead), 1<<20)
	c.Put(cacheOrgA, "a", "A")
	c.Put(cacheOrgA, "b", "B")
	if _, ok := c.Get(cacheOrgA, "a"); !ok {
		t.Fatal("expected a hit on a")
	}
	c.Put(cacheOrgA, "c", "C")
	if _, ok := c.Get(cacheOrgA, "b"); ok {
		t.Fatal("expected b to be evicted as least recently used")
	}
	if _, ok := c.Get(cacheOrgA, "a"); !ok {
		t.Fatal("expected a to survive")
	}
}

func TestYamlCacheIsSafeForConcurrentUse(t *testing.T) {
	c := newYamlCacheSized(64*(8+yamlCacheEntryOverhead), 1<<20)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			org := uuid.New()
			for i := range 500 {
				key := strconv.Itoa(i % 100)
				c.Put(org, key, "yaml-"+key)
				if got, ok := c.Get(org, key); ok && got != "yaml-"+key {
					t.Errorf("goroutine %d read %q for %s", g, got, key)
				}
			}
		})
	}
	wg.Wait()
	if c.Len() > 64 {
		t.Fatalf("%d entries under a bound of 64", c.Len())
	}
}
