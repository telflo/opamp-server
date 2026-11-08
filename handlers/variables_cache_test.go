package handlers

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/fleets"
	"github.com/telflo/opamp-server/store/orgvariables"
	"github.com/telflo/opamp-server/store/storetest"
)

// cacheGeneration is the cache tests' organizations' generation.
var cacheGeneration = uuid.MustParse("9e9e9e9e-0000-4000-8000-00000000009e")

// resolvedAt is a Resolved of one variable at revision, read at
// cacheGeneration.
func resolvedAt(revision int64, name, value string) orgvariables.Resolved {
	return orgvariables.Resolved{Revision: revision, Generation: cacheGeneration,
		Values: map[string]string{name: value}, Stamps: map[string]int64{name: revision}}
}

// The variables cache is keyed by the organization, its generation, its
// revision and the collector's two variable groups; an organization never
// reads another's entry, nor an entry of another generation, revision or
// other variable groups. It is bounded in bytes (Σ(name + value + 64) + 256
// an entry), keeps no entry over 16 MiB, nor one of revision 0, nor one
// read at another generation than the poll statement's, and evicts the
// least recently used. Through the poll, two organizations at the same
// revision, without variable groups, are each served their own values.
func TestTheVariablesCacheIsBoundedAndKeyedByOrganization(t *testing.T) {
	t.Run("the key", func(t *testing.T) {
		c := NewVariablesCache()
		eu, canary := uuid.New(), uuid.New()
		if !c.Put(cacheOrgA, cacheGeneration, &eu, nil, resolvedAt(3, "HOST", "a")) {
			t.Fatal("a small entry wasn't kept")
		}
		if got, ok := c.Get(cacheOrgA, cacheGeneration, 3, &eu, nil); !ok || got.Values["HOST"] != "a" {
			t.Fatalf("Get = %+v, %v; want A's entry", got, ok)
		}
		for label, miss := range map[string]func() bool{
			"another organization":           func() bool { _, ok := c.Get(cacheOrgB, cacheGeneration, 3, &eu, nil); return ok },
			"another generation":             func() bool { _, ok := c.Get(cacheOrgA, uuid.New(), 3, &eu, nil); return ok },
			"another revision":               func() bool { _, ok := c.Get(cacheOrgA, cacheGeneration, 4, &eu, nil); return ok },
			"no group's variable group":      func() bool { _, ok := c.Get(cacheOrgA, cacheGeneration, 3, nil, nil); return ok },
			"another group's variable group": func() bool { _, ok := c.Get(cacheOrgA, cacheGeneration, 3, &canary, nil); return ok },
			"the group's as the pin's":       func() bool { _, ok := c.Get(cacheOrgA, cacheGeneration, 3, nil, &eu); return ok },
			"a pin's variable group too":     func() bool { _, ok := c.Get(cacheOrgA, cacheGeneration, 3, &eu, &canary); return ok },
		} {
			if miss() {
				t.Errorf("%s: a hit on A's entry", label)
			}
		}
		c.Put(cacheOrgB, cacheGeneration, &eu, nil, resolvedAt(3, "HOST", "b"))
		if a, _ := c.Get(cacheOrgA, cacheGeneration, 3, &eu, nil); a.Values["HOST"] != "a" {
			t.Fatalf("A reads %q after B's Put", a.Values["HOST"])
		}
		if b, _ := c.Get(cacheOrgB, cacheGeneration, 3, &eu, nil); b.Values["HOST"] != "b" {
			t.Fatalf("B reads %q", b.Values["HOST"])
		}
	})

	t.Run("revision 0 is never kept", func(t *testing.T) {
		c := NewVariablesCache()
		if c.Put(cacheOrgA, cacheGeneration, nil, nil, orgvariables.Resolved{}) || c.Len() != 0 {
			t.Fatal("an organization that never had a variable was cached")
		}
		if _, ok := c.Get(cacheOrgA, cacheGeneration, 0, nil, nil); ok {
			t.Fatal("revision 0 was answered")
		}
	})

	t.Run("a read at another generation is never kept", func(t *testing.T) {
		c := NewVariablesCache()
		// The organization deleted and registered again between the poll
		// statement (cacheGeneration) and its miss's read (another).
		r := resolvedAt(1, "HOST", "the next incarnation's")
		r.Generation = uuid.New()
		if c.Put(cacheOrgA, cacheGeneration, nil, nil, r) || c.Len() != 0 {
			t.Fatal("a read of another generation was kept under the poll statement's")
		}
		if _, ok := c.Get(cacheOrgA, cacheGeneration, 1, nil, nil); ok {
			t.Fatal("the poll statement's generation answered another's values")
		}
		if !c.Put(cacheOrgA, r.Generation, nil, nil, r) || c.Len() != 1 {
			t.Fatal("the same read under its own generation wasn't kept")
		}
	})

	t.Run("the bounds", func(t *testing.T) {
		c := NewVariablesCache()
		if size := variablesSize(orgvariables.Resolved{Revision: 1, Values: map[string]string{"AB": "xyz", "C": ""}}); size != 2+3+64+1+64+256 {
			t.Fatalf("an entry counts %d bytes; want Σ(name + value + 64) + 256", size)
		}
		over := orgvariables.Resolved{Revision: 1, Generation: cacheGeneration,
			Values: map[string]string{"V": strings.Repeat("x", VariablesCacheMaxEntryBytes)}}
		if c.Put(cacheOrgA, cacheGeneration, nil, nil, over) {
			t.Fatal("an entry over 16 MiB was kept")
		}
		mib := strings.Repeat("y", 1<<20)
		for revision := int64(1); revision <= 100; revision++ {
			c.Put(cacheOrgA, cacheGeneration, nil, nil, resolvedAt(revision, "V", mib))
		}
		if c.Bytes() > VariablesCacheMaxBytes {
			t.Fatalf("%d bytes kept under a bound of %d", c.Bytes(), VariablesCacheMaxBytes)
		}
		if _, ok := c.Get(cacheOrgA, cacheGeneration, 1, nil, nil); ok {
			t.Fatal("the least recently used entry survived the bound")
		}
		if _, ok := c.Get(cacheOrgA, cacheGeneration, 100, nil, nil); !ok {
			t.Fatal("the newest entry went")
		}
		small := newVariablesCacheSized(2*(1+1+variablesCacheVariableOverhead+variablesCacheEntryOverhead), 1<<20)
		small.Put(cacheOrgA, cacheGeneration, nil, nil, resolvedAt(1, "V", "a"))
		small.Put(cacheOrgA, cacheGeneration, nil, nil, resolvedAt(2, "V", "b"))
		if _, ok := small.Get(cacheOrgA, cacheGeneration, 1, nil, nil); !ok {
			t.Fatal("expected a hit on revision 1")
		}
		small.Put(cacheOrgA, cacheGeneration, nil, nil, resolvedAt(3, "V", "c"))
		if _, ok := small.Get(cacheOrgA, cacheGeneration, 2, nil, nil); ok {
			t.Fatal("expected revision 2 evicted as least recently used")
		}
		if _, ok := small.Get(cacheOrgA, cacheGeneration, 1, nil, nil); !ok {
			t.Fatal("expected revision 1 to survive")
		}
	})

	t.Run("through the poll", func(t *testing.T) {
		x := newHarness(t, testStores(t))
		x.variable(storetest.OrgA, "HOST", "a.example")
		x.variable(storetest.OrgB, "HOST", "b.example")
		a, _ := assigned(x, "endpoint: ${org:HOST}\n")
		b := x.fleet(storetest.OrgB, "edge-prod", fleets.TypeServer)
		x.assign(b, b.DefaultGroupID, x.publish(storetest.OrgB, "endpoint: ${org:HOST}\n"))
		for seq := range uint64(2) {
			if got := pushed(t, x.ok(rotate(a), message(uuid.New(), seq))); got != "endpoint: a.example\n" {
				t.Fatalf("A was pushed %q", got)
			}
			if got := pushed(t, x.ok(rotate(b), message(uuid.New(), seq))); got != "endpoint: b.example\n" {
				t.Fatalf("B was pushed %q; want its own value", got)
			}
		}
		if x.vars.Len() != 2 {
			t.Fatalf("%d entries; want one per organization", x.vars.Len())
		}
	})
}
