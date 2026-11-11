package collectors_test

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/telflo-opamp-server/store/collectorgroups"
	"github.com/telflo/telflo-opamp-server/store/collectors"
	"github.com/telflo/telflo-opamp-server/store/fleets"
	"github.com/telflo/telflo-opamp-server/store/orgvariables"
	. "github.com/telflo/telflo-opamp-server/store/storetest"
	"github.com/telflo/telflo-opamp-server/store/variables"
)

// The ladder's edges by the caller's clock: under 60 s online, 60 s exactly
// a warning, 120 s unknown, 300 s offline; a last seen ahead of the clock
// is online.
func TestLivenessAtTheLaddersEdges(t *testing.T) {
	for age, want := range map[time.Duration]collectors.Liveness{
		-time.Second:                           collectors.Online,
		0:                                      collectors.Online,
		59*time.Second + 999*time.Millisecond:  collectors.Online,
		60 * time.Second:                       collectors.Warning,
		119*time.Second + 999*time.Millisecond: collectors.Warning,
		120 * time.Second:                      collectors.Unknown,
		299*time.Second + 999*time.Millisecond: collectors.Unknown,
		300 * time.Second:                      collectors.Offline,
		24 * time.Hour:                         collectors.Offline,
	} {
		if got := collectors.LivenessAt(Now.Add(-age), Now); got != want {
			t.Errorf("last seen %v ago: %s, want %s", age, got, want)
		}
	}
}

// Sync in its steps' order. Every case's collector was served the resolved
// published version unless it says otherwise (the served pointer, not the
// hash, says what it was served: the hash is the filled-in bytes'); the
// served-variables marker below the wanted stamp reads pending, at or above
// it the hash steps decide.
func TestSyncComesFromHashesInItsOrder(t *testing.T) {
	sha, other := pubSHA256, sumOf("other")
	served, before, after := Now, Now.Add(-time.Second), Now.Add(time.Second)
	resolved := collectors.Resolution{From: collectors.FromGroupPublication, ID: PubA, SHA256: sha}
	wanting := resolved
	wanting.VariablesStamp = 7
	status := func(s collectors.EffectiveStatus) *collectors.EffectiveStatus { return &s }
	pubA := &collectors.Ref{Source: collectors.SourcePublication, ID: PubA}
	another := &collectors.Ref{Source: collectors.SourcePublication, ID: uuid.New()}
	applied := func(version *collectors.Ref, marker *int64) collectors.Collector {
		return collectors.Collector{Served: version, ServedSHA256: &sha, ServedAt: &served, EffectiveSHA256: &sha,
			EffectiveStatus: status(collectors.StatusApplied), EffectiveAt: &after, ServedVariablesStamp: marker}
	}
	for label, want := range map[string]struct {
		resolution collectors.Resolution
		collector  collectors.Collector
		sync       collectors.Sync
	}{
		"nothing resolves, whatever runs":                  {collectors.Resolution{From: collectors.FromNone}, applied(pubA, nil), collectors.Unassigned},
		"never served":                                     {resolved, collectors.Collector{}, collectors.Pending},
		"served another published version, the same bytes": {resolved, applied(another, nil), collectors.Pending},
		"served another published version and hash": {resolved,
			collectors.Collector{Served: another, ServedSHA256: &other, ServedAt: &served, EffectiveSHA256: &other, EffectiveStatus: status(collectors.StatusApplied), EffectiveAt: &after},
			collectors.Pending},
		"served it, its version since deleted (no pointer)": {resolved,
			collectors.Collector{ServedSHA256: &sha, ServedAt: &served, EffectiveSHA256: &sha, EffectiveStatus: status(collectors.StatusApplied), EffectiveAt: &after},
			collectors.Pending},
		"no report since served": {resolved,
			collectors.Collector{Served: pubA, ServedSHA256: &sha, ServedAt: &served, EffectiveSHA256: &sha, EffectiveStatus: status(collectors.StatusApplied), EffectiveAt: &before},
			collectors.Pending},
		"no report at all": {resolved, collectors.Collector{Served: pubA, ServedSHA256: &sha, ServedAt: &served}, collectors.Pending},
		"applying": {resolved,
			collectors.Collector{Served: pubA, ServedSHA256: &sha, ServedAt: &served, EffectiveSHA256: &sha, EffectiveStatus: status(collectors.StatusApplying), EffectiveAt: &after},
			collectors.Pending},
		"failed, reported at the served instant": {resolved,
			collectors.Collector{Served: pubA, ServedSHA256: &sha, ServedAt: &served, EffectiveSHA256: &sha, EffectiveStatus: status(collectors.StatusFailed), EffectiveAt: &served},
			collectors.Failed},
		"applied on the served hash": {resolved, applied(pubA, nil), collectors.InSync},
		"applied on another hash": {resolved,
			collectors.Collector{Served: pubA, ServedSHA256: &sha, ServedAt: &served, EffectiveSHA256: &other, EffectiveStatus: status(collectors.StatusApplied), EffectiveAt: &after},
			collectors.Drifted},
		"a marker below the wanted stamp": {wanting, applied(pubA, Ptr(int64(6))), collectors.Pending},
		"no marker, a wanted stamp":       {wanting, applied(pubA, nil), collectors.Pending},
		"a marker at the wanted stamp":    {wanting, applied(pubA, Ptr(int64(7))), collectors.InSync},
		"a marker past the wanted stamp":  {wanting, applied(pubA, Ptr(int64(9))), collectors.InSync},
		"a marker at the wanted stamp, then failed": {wanting, func() collectors.Collector {
			c := applied(pubA, Ptr(int64(7)))
			c.EffectiveStatus = status(collectors.StatusFailed)
			return c
		}(), collectors.Failed},
		"a marker at the wanted stamp, another hash": {wanting, func() collectors.Collector { c := applied(pubA, Ptr(int64(7))); c.EffectiveSHA256 = &other; return c }(), collectors.Drifted},
	} {
		if got := collectors.SyncOf(want.resolution, want.collector); got != want.sync {
			t.Errorf("%s: %s, want %s", label, got, want.sync)
		}
	}
}

// SummariesForFleets counts each fleet's collectors by liveness and sync
// in one query, by the same edges and steps as LivenessAt and SyncOf, on a
// known fixture (served-other served another published version, and two
// collectors' markers against the wanted stamp; the counts agree with the
// grid); a fleet without collectors is zero, another organization's fleet
// has no entry. Five fleets in id order (the order the rows come in) are
// empty, the fixture's, empty, small's, empty: a fleet without collectors
// follows each fleet with them, so a summary that kept the row before's
// LastSeenAt always shows.
func TestSummariesCountAKnownFixture(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		var made []fleets.Creation
		for _, label := range []string{"edge-1", "edge-2", "edge-3", "edge-4", "edge-5"} {
			made = append(made, s.CreateFleet(t, OrgA, label, fleets.TypeServer))
		}
		slices.SortFunc(made, func(a, b fleets.Creation) int { return bytes.Compare(a.Fleet.ID[:], b.Fleet.ID[:]) })
		c, small := made[1], made[3]
		empties := []fleets.Creation{made[0], made[2], made[4]}
		theirs := s.CreateFleet(t, OrgB, "globex", fleets.TypeServer)
		enroll(t, s, theirs, "globex-01", Now)
		if _, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, c.DefaultGroupID,
			collectorgroups.Changes{AssignedPublicationID: collectorgroups.SetTo(PubA)}, Now); err != nil {
			t.Fatal(err)
		}
		bare, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID, Name: "bare", At: Now})
		if err != nil {
			t.Fatal(err)
		}
		now := Now.Add(24 * time.Hour)
		ms := time.Millisecond
		served := now.Add(-2 * time.Hour)
		applied, applying, failed := collectors.StatusApplied, collectors.StatusApplying, collectors.StatusFailed
		other := sumOf("other")
		otherVersion := publish(t, s, OrgA, "other")
		fixture := []struct {
			name     string
			age      time.Duration
			bare     bool
			served   *string
			status   collectors.EffectiveStatus
			sha      string
			reported time.Time
			liveness collectors.Liveness
			sync     collectors.Sync
		}{
			{"unassigned", 0, true, nil, "", "", time.Time{}, collectors.Online, collectors.Unassigned},
			{"never-served", 60*time.Second - ms, false, nil, "", "", time.Time{}, collectors.Online, collectors.Pending},
			{"served-other", 60 * time.Second, false, &other, applied, other, served.Add(time.Minute), collectors.Warning, collectors.Pending},
			{"applying", 120*time.Second - ms, false, &pubSHA256, applying, pubSHA256, served.Add(time.Minute), collectors.Warning, collectors.Pending},
			{"stale-report", 120 * time.Second, false, &pubSHA256, applied, pubSHA256, served.Add(-time.Minute), collectors.Unknown, collectors.Pending},
			{"failed", 300*time.Second - ms, false, &pubSHA256, failed, pubSHA256, served, collectors.Unknown, collectors.Failed},
			{"in-sync", 300 * time.Second, false, &pubSHA256, applied, pubSHA256, served.Add(time.Minute), collectors.Offline, collectors.InSync},
			{"drifted", time.Hour, false, &pubSHA256, applied, other, served.Add(time.Minute), collectors.Offline, collectors.Drifted},
		}
		for _, f := range fixture {
			e := enrollment(c, f.name, now.Add(-f.age))
			if f.bare {
				e.GroupID = &bare.ID
			}
			r, err := s.Collectors.Enroll(ctx, e)
			if err != nil {
				t.Fatal(err)
			}
			report := collectors.Report{OrganizationID: OrgA, FleetID: c.Fleet.ID, CollectorID: r.Collector.ID}
			if f.served != nil {
				// What it was served is its served published version;
				// served-other was served another.
				version := PubA
				if *f.served == other {
					version = otherVersion
				}
				report.Served = &collectors.Served{Ref: collectors.Ref{Source: collectors.SourcePublication, ID: version},
					SHA256: *f.served, At: served}
			}
			if f.status != "" {
				report.Effective = &collectors.Effective{SHA256: f.sha, Status: f.status, Error: "boom", At: f.reported}
			}
			if _, err := s.Collectors.Report(ctx, report); err != nil {
				t.Fatal(err)
			}
			got := get(t, s, r.Collector)
			if l := collectors.LivenessAt(got.LastSeenAt, now); l != f.liveness {
				t.Errorf("%s: liveness %s, want %s", f.name, l, f.liveness)
			}
			if d := got.Sync(); d != f.sync {
				t.Errorf("%s: sync %s, want %s", f.name, d, f.sync)
			}
		}
		// The wanted stamp in the counts. A group whose published version
		// references REGION: served it with the marker at REGION's stamp, in
		// sync; with one below it (served before REGION's last change),
		// pending. Both offline.
		defineVariable(t, s, OrgA, "REGION", "eu")
		regionStamp := revision(t, s, OrgA)
		withNames := publish(t, s, OrgA, "a: ${org:REGION}\n")
		vars, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID, Name: "vars",
			AssignedPublicationID: &withNames, At: Now})
		if err != nil {
			t.Fatal(err)
		}
		for name, marker := range map[string]int64{"marker-current": regionStamp, "marker-behind": regionStamp - 1} {
			e := enrollment(c, name, now.Add(-time.Hour))
			e.GroupID = &vars.ID
			r, err := s.Collectors.Enroll(ctx, e)
			if err != nil {
				t.Fatal(err)
			}
			rendered := sumOf("a: eu\n")
			if _, err := s.Collectors.Report(ctx, collectors.Report{OrganizationID: OrgA, FleetID: c.Fleet.ID, CollectorID: r.Collector.ID,
				Served:    &collectors.Served{Ref: collectors.Ref{Source: collectors.SourcePublication, ID: withNames}, SHA256: rendered, At: served, VariablesStamp: marker},
				Effective: &collectors.Effective{SHA256: rendered, Status: applied, At: served.Add(time.Minute)}}); err != nil {
				t.Fatal(err)
			}
			want := collectors.InSync
			if name == "marker-behind" {
				want = collectors.Pending
			}
			if got := get(t, s, r.Collector); got.Sync() != want || got.Resolution.VariablesStamp != regionStamp {
				t.Errorf("%s: sync %s, wanted stamp %d; want %s, %d", name, got.Sync(), got.Resolution.VariablesStamp, want, regionStamp)
			}
		}
		agree(t, s, c.Fleet.ID, now)
		enroll(t, s, small, "small-01", now.Add(-10*time.Minute))

		summaries, err := s.Collectors.SummariesForFleets(ctx, OrgA, []uuid.UUID{c.Fleet.ID, empties[0].Fleet.ID, small.Fleet.ID,
			empties[1].Fleet.ID, theirs.Fleet.ID, uuid.New(), empties[2].Fleet.ID, c.Fleet.ID}, now)
		if err != nil {
			t.Fatal(err)
		}
		want := map[uuid.UUID]collectors.Summary{
			c.Fleet.ID: {Collectors: 10, Online: 2, Warning: 2, Unknown: 2, Offline: 4,
				InSync: 2, Pending: 5, Failed: 1, Drifted: 1, Unassigned: 1, LastSeenAt: Ptr(Millisecond(now))},
			empties[0].Fleet.ID: {},
			empties[1].Fleet.ID: {},
			empties[2].Fleet.ID: {},
			small.Fleet.ID:      {Collectors: 1, Offline: 1, Unassigned: 1, LastSeenAt: Ptr(Millisecond(now.Add(-10 * time.Minute)))},
		}
		if summariesText(summaries) != summariesText(want) {
			t.Fatalf("Summaries =\n%s\nwant\n%s", summariesText(summaries), summariesText(want))
		}
		if seen := summaries[c.Fleet.ID].LastSeenAt; seen == nil || seen.Location() != time.UTC {
			t.Fatalf("the last seen %v isn't in UTC", seen)
		}
		if got, err := s.Collectors.SummariesForFleets(ctx, OrgA, nil, now); err != nil || len(got) != 0 {
			t.Fatalf("no fleets = %+v, %v", got, err)
		}
		if got, err := s.Collectors.SummariesForFleets(ctx, OrgB, []uuid.UUID{c.Fleet.ID}, now); err != nil || len(got) != 0 {
			t.Fatalf("A's fleet under B = %+v, %v", got, err)
		}
		many := make([]uuid.UUID, collectors.MaxSummaryFleets+1)
		if _, err := s.Collectors.SummariesForFleets(ctx, OrgA, many, now); !errors.Is(err, collectors.ErrInvalid) {
			t.Fatalf("101 fleets = %v", err)
		}
	})
}

// summariesText is summaries as text, sorted, LastSeenAt as its instant in
// UTC: Describe would print the pointer.
func summariesText(summaries map[uuid.UUID]collectors.Summary) string {
	lines := make([]string, 0, len(summaries))
	for id, m := range summaries {
		seen := "null"
		if m.LastSeenAt != nil {
			seen = m.LastSeenAt.UTC().Format(time.RFC3339Nano)
		}
		copied := m
		copied.LastSeenAt = nil
		lines = append(lines, id.String()+" "+Describe(copied)+" last seen "+seen)
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

// regionYAML is the sync tests' published version: it references REGION,
// never OTHER.
const regionYAML = "exporters:\n  otlp:\n    endpoint: ${org:REGION}\n"

// syncSetup is a sync test's start: the variables REGION (which the
// published version uses) and OTHER (which it doesn't), the version
// assigned to a fleet's Default group, and its collectors enrolled there
// and polled once each: in sync.
type syncSetup struct {
	c             fleets.Creation
	region, other variables.Variable
	version       uuid.UUID
	p             *poller
}

func newSyncSetup(t *testing.T, s *Stores, names ...string) (*syncSetup, []collectors.Collector) {
	t.Helper()
	d := &syncSetup{c: s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer), p: newPoller(t, s)}
	d.region = defineVariable(t, s, OrgA, "REGION", "eu-west")
	d.other = defineVariable(t, s, OrgA, "OTHER", "o")
	d.version = publish(t, s, OrgA, regionYAML)
	assignGroup(t, s, d.c, d.c.DefaultGroupID, d.version)
	var cs []collectors.Collector
	for _, name := range names {
		cs = append(cs, enroll(t, s, d.c, name, Now))
	}
	d.pollAll(t, s, cs...)
	return d, cs
}

// pollAll polls each collector and checks it then reads in sync.
func (d *syncSetup) pollAll(t *testing.T, s *Stores, cs ...collectors.Collector) {
	t.Helper()
	for _, c := range cs {
		d.p.poll(c)
		syncIs(t, s, c, collectors.InSync, "polled")
	}
}

// A value the collector's text uses changed reads pending, and in sync
// again once the poll has served the new bytes.
func TestSyncAUsedValueChangedReadsPendingUntilServed(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		d, cs := newSyncSetup(t, s, "edge-01")
		setValue(t, s, OrgA, d.region.ID, "us-east")
		syncIs(t, s, cs[0], collectors.Pending, "REGION changed")
		syncIs(t, s, cs[0], collectors.Pending, "REGION changed, read again")
		if !d.p.poll(cs[0]) {
			t.Fatal("a used value's change wasn't pushed")
		}
		syncIs(t, s, cs[0], collectors.InSync, "served")
	})
}

// A value the text doesn't use changed, or a new variable, leaves the
// collector in sync, and its poll pushes nothing.
func TestSyncAnUnusedValueChangedStaysInSync(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		d, cs := newSyncSetup(t, s, "edge-01")
		setValue(t, s, OrgA, d.other.ID, "changed")
		defineVariable(t, s, OrgA, "NEW", "n")
		syncIs(t, s, cs[0], collectors.InSync, "OTHER changed")
		if d.p.poll(cs[0]) {
			t.Fatal("an unused value's change was pushed")
		}
		syncIs(t, s, cs[0], collectors.InSync, "polled")
	})
}

// An override set in a variable group reads pending only for the collectors
// whose group (or pin) uses that variable group; an override in a variable
// group nobody uses moves nobody.
func TestSyncAnOverrideSetReadsPendingOnlyForItsVariableGroupsCollectors(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		d, _ := newSyncSetup(t, s)
		eu, unused := newVariableGroup(t, s, OrgA, "eu"), newVariableGroup(t, s, OrgA, "unused")
		bindGroup(t, s, d.c, d.c.DefaultGroupID, &eu)
		gateways, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: d.c.Fleet.ID,
			Name: "gateways", AssignedPublicationID: &d.version, At: Now})
		if err != nil {
			t.Fatal(err)
		}
		bound := enroll(t, s, d.c, "bound", Now)
		e := enrollment(d.c, "unbound", Now)
		e.GroupID = &gateways.ID
		r, err := s.Collectors.Enroll(ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		unbound := r.Collector
		d.pollAll(t, s, bound, unbound)

		setOverride(t, s, OrgA, unused, d.region.ID, "nobody")
		syncIs(t, s, bound, collectors.InSync, "an unused variable group's override")
		setOverride(t, s, OrgA, eu, d.region.ID, "eu-central")
		syncIs(t, s, bound, collectors.Pending, "its variable group's override set")
		syncIs(t, s, unbound, collectors.InSync, "another group's override set")
		if !d.p.poll(bound) {
			t.Fatal("the override wasn't pushed")
		}
		syncIs(t, s, bound, collectors.InSync, "served")
	})
}

// An override's Reset reads pending (the row kept with no value and the
// Reset's stamp) until polled, then in sync; set again, pending again.
func TestSyncAnOverrideResetReadsPendingUntilPolled(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		d, cs := newSyncSetup(t, s, "edge-01")
		eu := newVariableGroup(t, s, OrgA, "eu")
		bindGroup(t, s, d.c, d.c.DefaultGroupID, &eu)
		setOverride(t, s, OrgA, eu, d.region.ID, "eu-central")
		d.pollAll(t, s, cs[0])

		resetOverride(t, s, OrgA, eu, d.region.ID)
		syncIs(t, s, cs[0], collectors.Pending, "reset")
		if !d.p.poll(cs[0]) {
			t.Fatal("the Reset (back to the organization's value) wasn't pushed")
		}
		syncIs(t, s, cs[0], collectors.InSync, "polled after the Reset")
		setOverride(t, s, OrgA, eu, d.region.ID, "eu-north")
		syncIs(t, s, cs[0], collectors.Pending, "set again")
		d.pollAll(t, s, cs[0])
	})
}

// The Reset of an override of a name the text doesn't use leaves it in
// sync.
func TestSyncAResetOfANameTheTextDoesntUseStaysInSync(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		d, cs := newSyncSetup(t, s, "edge-01")
		eu := newVariableGroup(t, s, OrgA, "eu")
		bindGroup(t, s, d.c, d.c.DefaultGroupID, &eu)
		setOverride(t, s, OrgA, eu, d.other.ID, "o2")
		d.pollAll(t, s, cs[0])
		resetOverride(t, s, OrgA, eu, d.other.ID)
		syncIs(t, s, cs[0], collectors.InSync, "OTHER's override reset")
	})
}

// pinnedOverGroup is the two-level start: the Default group's variable
// group overrides REGION, and the collector's pinned variable group
// overrides it too, its value the one filled in.
func pinnedOverGroup(t *testing.T, s *Stores) (d *syncSetup, c collectors.Collector, group, pin uuid.UUID) {
	t.Helper()
	d, cs := newSyncSetup(t, s, "edge-01")
	group, pin = newVariableGroup(t, s, OrgA, "group-level"), newVariableGroup(t, s, OrgA, "pin-level")
	setOverride(t, s, OrgA, group, d.region.ID, "from-group")
	setOverride(t, s, OrgA, pin, d.region.ID, "from-pin")
	bindGroup(t, s, d.c, d.c.DefaultGroupID, &group)
	pinVariableGroup(t, s, cs[0], &pin)
	d.pollAll(t, s, cs[0])
	return d, cs[0], group, pin
}

// The group's override Reset while the pin's variable group overrides that
// name with a value: the pin's value hides it, so the collector stays in
// sync and its poll pushes nothing.
func TestSyncAResetUnderThePinsOverrideStaysInSync(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		d, c, group, _ := pinnedOverGroup(t, s)
		resetOverride(t, s, OrgA, group, d.region.ID)
		syncIs(t, s, c, collectors.InSync, "the group's override reset under the pin's")
		if d.p.poll(c) {
			t.Fatal("a Reset the pin's override hides was pushed")
		}
	})
}

// The pin's override Reset falls back to the group's override: pending
// until served.
func TestSyncAResetAtThePinFallsBackToTheGroupsOverrideAndReadsPending(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		d, c, _, pin := pinnedOverGroup(t, s)
		resetOverride(t, s, OrgA, pin, d.region.ID)
		syncIs(t, s, c, collectors.Pending, "the pin's override reset")
		if !d.p.poll(c) {
			t.Fatal("the fall back to the group's override wasn't pushed")
		}
		syncIs(t, s, c, collectors.InSync, "served")
	})
}

// A group's variable group set, or cleared, reads pending for its
// collectors (the setting's stamp) until polled, whether or not the bytes
// change.
func TestSyncAGroupsVariableGroupChangedReadsPending(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		d, cs := newSyncSetup(t, s, "edge-01")
		empty := newVariableGroup(t, s, OrgA, "empty")
		bindGroup(t, s, d.c, d.c.DefaultGroupID, &empty)
		syncIs(t, s, cs[0], collectors.Pending, "the group's variable group set")
		if d.p.poll(cs[0]) {
			t.Fatal("a variable group without overrides changed the bytes")
		}
		syncIs(t, s, cs[0], collectors.InSync, "polled")
		bindGroup(t, s, d.c, d.c.DefaultGroupID, nil)
		syncIs(t, s, cs[0], collectors.Pending, "the group's variable group cleared")
		d.pollAll(t, s, cs[0])
	})
}

// A pin's variable group set reads pending for that collector only; its
// group's other collectors stay in sync.
func TestSyncAPinsVariableGroupChangedReadsPendingForThatCollectorOnly(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		d, cs := newSyncSetup(t, s, "edge-01", "edge-02")
		pin := newVariableGroup(t, s, OrgA, "pin")
		setOverride(t, s, OrgA, pin, d.region.ID, "pinned")
		pinVariableGroup(t, s, cs[0], &pin)
		syncIs(t, s, cs[0], collectors.Pending, "its pin's variable group set")
		syncIs(t, s, cs[1], collectors.InSync, "the other collector")
		d.pollAll(t, s, cs[0])
		pinVariableGroup(t, s, cs[0], nil)
		syncIs(t, s, cs[0], collectors.Pending, "its pin's variable group cleared")
		syncIs(t, s, cs[1], collectors.InSync, "the other collector")
	})
}

// An offline collector whose used value changed reads pending (and offline)
// for as long as it doesn't poll, then in sync.
func TestSyncAnOfflineCollectorReadsPendingUntilItPollsAgain(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		d, cs := newSyncSetup(t, s, "edge-01")
		setValue(t, s, OrgA, d.region.ID, "us-east")
		for _, later := range []time.Duration{time.Hour, 24 * time.Hour} {
			now := Now.Add(later)
			if got := agree(t, s, d.c.Fleet.ID, now)[cs[0].ID]; got != collectors.Pending {
				t.Fatalf("%v on, offline: %s, want pending", later, got)
			}
			summaries, err := s.Collectors.SummariesForFleets(ctx, OrgA, []uuid.UUID{d.c.Fleet.ID}, now)
			if err != nil || summaries[d.c.Fleet.ID].Offline != 1 || summaries[d.c.Fleet.ID].Pending != 1 {
				t.Fatalf("%v on: %+v, %v; want one offline and pending", later, summaries, err)
			}
		}
		d.pollAll(t, s, cs[0])
	})
}

// Three levels: per variable, the pin's variable group, else the group's,
// else the organization. The organization's change moves only a collector
// that reads the organization's value; the group's override's change only
// one without a pin's override; the pin's only the pinned collector.
func TestSyncThreeLevelsAPinsOverrideHidesTheOrganizationsChange(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		d, cs := newSyncSetup(t, s, "pinned", "grouped")
		pinned, grouped := cs[0], cs[1]
		groupLevel, pinLevel := newVariableGroup(t, s, OrgA, "group-level"), newVariableGroup(t, s, OrgA, "pin-level")
		setOverride(t, s, OrgA, groupLevel, d.region.ID, "from-group")
		setOverride(t, s, OrgA, pinLevel, d.region.ID, "from-pin")
		bindGroup(t, s, d.c, d.c.DefaultGroupID, &groupLevel)
		pinVariableGroup(t, s, pinned, &pinLevel)
		plain, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: d.c.Fleet.ID,
			Name: "plain", AssignedPublicationID: &d.version, At: Now})
		if err != nil {
			t.Fatal(err)
		}
		e := enrollment(d.c, "organization", Now)
		e.GroupID = &plain.ID
		r, err := s.Collectors.Enroll(ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		organization := r.Collector
		d.pollAll(t, s, pinned, grouped, organization)

		setValue(t, s, OrgA, d.region.ID, "us-east")
		syncIs(t, s, pinned, collectors.InSync, "the organization's change, under the pin's override")
		syncIs(t, s, grouped, collectors.InSync, "the organization's change, under the group's override")
		syncIs(t, s, organization, collectors.Pending, "the organization's change, read at the organization")
		d.pollAll(t, s, organization)

		setOverride(t, s, OrgA, groupLevel, d.region.ID, "from-group-2")
		syncIs(t, s, pinned, collectors.InSync, "the group's override changed, under the pin's")
		syncIs(t, s, grouped, collectors.Pending, "the group's override changed")
		d.pollAll(t, s, grouped)

		setOverride(t, s, OrgA, pinLevel, d.region.ID, "from-pin-2")
		syncIs(t, s, pinned, collectors.Pending, "the pin's override changed")
		syncIs(t, s, grouped, collectors.InSync, "the pin's override changed, not pinned")
		syncIs(t, s, organization, collectors.InSync, "the pin's override changed, at the organization")
	})
}

// A published version that references no name ignores variables: its
// collectors stay in sync whatever changes, the bindings included (its
// wanted stamp is 0).
func TestSyncAVersionWithoutNamesIgnoresVariables(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		region := defineVariable(t, s, OrgA, "REGION", "eu")
		plain := publish(t, s, OrgA, "receivers: {}\nliteral: $${org:REGION}\n")
		assignGroup(t, s, c, c.DefaultGroupID, plain)
		got := enroll(t, s, c, "edge-01", Now)
		p := newPoller(t, s)
		p.poll(got)
		syncIs(t, s, got, collectors.InSync, "polled")
		vg := newVariableGroup(t, s, OrgA, "eu")
		setOverride(t, s, OrgA, vg, region.ID, "x")
		setValue(t, s, OrgA, region.ID, "us")
		bindGroup(t, s, c, c.DefaultGroupID, &vg)
		pinVariableGroup(t, s, got, &vg)
		syncIs(t, s, got, collectors.InSync, "every kind of change")
		if l := get(t, s, got); l.Resolution.VariablesStamp != 0 {
			t.Fatalf("the wanted stamp = %d, want 0", l.Resolution.VariablesStamp)
		}
	})
}

// A group retargeted to another published version of identical bytes reads
// pending (the served version isn't the resolved one) until it polls; that
// poll pushes nothing (a reconcile: the pointer follows, the served time
// stays), and it then reads in sync.
func TestSyncAnIdenticalRetargetReadsPendingUntilReconciledWithoutAPush(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		d, cs := newSyncSetup(t, s, "edge-01")
		servedAt := *get(t, s, cs[0]).ServedAt
		copyOf := publish(t, s, OrgA, regionYAML)
		assignGroup(t, s, d.c, d.c.DefaultGroupID, copyOf)
		syncIs(t, s, cs[0], collectors.Pending, "retargeted to identical bytes")
		if d.p.poll(cs[0]) {
			t.Fatal("identical bytes were pushed again")
		}
		l := get(t, s, cs[0])
		if l.Served == nil || l.Served.ID != copyOf || !l.ServedAt.Equal(servedAt) {
			t.Fatalf("after the reconcile: served %+v at %v; want the copy, at %v", l.Served, l.ServedAt, servedAt)
		}
		syncIs(t, s, cs[0], collectors.InSync, "reconciled")
	})
}

// The wanted stamp is the same from every source (a name overridden at both
// the pin's and the group's level among the cases): Get (one collector),
// List (the page, several pages of it), the fleets' rows
// (SummariesForFleets' source, on Postgres) and the poll's own rule,
// orgvariables.WantedStamp over what the variables store resolves (parity),
// for every combination of a pin, a pinned and a group's variable group,
// overrides, a reset row, an undefined name and a version without names.
func TestTheWantedStampIsTheSameFromEverySource(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		a := defineVariable(t, s, OrgA, "A", "a")
		b := defineVariable(t, s, OrgA, "B", "b")
		cv := defineVariable(t, s, OrgA, "C", "c")
		x, y := newVariableGroup(t, s, OrgA, "x"), newVariableGroup(t, s, OrgA, "y")
		// A is overridden at both levels: in y, early, and in x (again after
		// the collectors are bound, below), so a group's row counted under the
		// pin's source would show.
		setOverride(t, s, OrgA, y, a.ID, "ya")
		setOverride(t, s, OrgA, x, a.ID, "xa")
		setOverride(t, s, OrgA, y, b.ID, "yb")
		setOverride(t, s, OrgA, y, cv.ID, "yc")
		resetOverride(t, s, OrgA, y, cv.ID)
		setValue(t, s, OrgA, b.ID, "b2")
		ab := publish(t, s, OrgA, "a: ${org:A}\nb: ${org:B}\n")
		onlyC := publish(t, s, OrgA, "c: ${org:C}\n")
		none := publish(t, s, OrgA, "receivers: {}\n")
		withUndefined := publish(t, s, OrgA, "a: ${org:A}\nu: ${org:UNDEFINED}\n")

		type group struct {
			name     string
			assigned *uuid.UUID
			variable *uuid.UUID
		}
		var groups []collectorgroups.Group
		for _, g := range []group{{"g-ab-x", &ab, &x}, {"g-c-y", &onlyC, &y}, {"g-ab", &ab, nil}, {"g-none", nil, &x}, {"g-plain", &none, &y}} {
			created, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID, Name: g.name,
				AssignedPublicationID: g.assigned, VariableGroupID: g.variable, At: Now})
			if err != nil {
				t.Fatal(err)
			}
			groups = append(groups, created)
		}
		var all []collectors.Collector
		i := 0
		for _, g := range groups {
			for _, pin := range []*uuid.UUID{nil, &ab, &onlyC} {
				for _, pinnedGroup := range []*uuid.UUID{nil, &x, &y} {
					e := enrollment(c, fmt.Sprintf("c-%02d", i), Now)
					e.GroupID = &g.ID
					i++
					r, err := s.Collectors.Enroll(ctx, e)
					if err != nil {
						t.Fatal(err)
					}
					changes := collectors.Changes{PinnedVariableGroup: &collectors.VariableGroupChange{ID: pinnedGroup}}
					if pin != nil {
						changes.Pin = &collectors.PinChange{Ref: &collectors.Ref{Source: collectors.SourcePublication, ID: *pin}}
					}
					if _, err := s.Collectors.Update(ctx, OrgA, c.Fleet.ID, r.Collector.ID, changes, Now); err != nil {
						t.Fatal(err)
					}
					all = append(all, r.Collector)
				}
			}
		}
		// One pinned to a version referencing an undefined name, as only a
		// test can (the pin's check refuses it).
		undefinedPinned := all[len(all)-1]
		pinPastTheCheck(t, s, undefinedPinned.ID, withUndefined)
		// The group level's override of A changed after every binding, so its
		// stamp is the newest but for the value's change below; a later
		// change of a value, so the stamps differ by name.
		setOverride(t, s, OrgA, x, a.ID, "xa2")
		setValue(t, s, OrgA, a.ID, "a2")

		fromList := map[uuid.UUID]int64{}
		o := collectors.ListOptions{Sort: collectors.SortName, Order: collectors.Descending, Limit: 7}
		for {
			page, err := s.Collectors.List(ctx, OrgA, c.Fleet.ID, o)
			if err != nil {
				t.Fatal(err)
			}
			for _, l := range page.Collectors {
				fromList[l.ID] = l.Resolution.VariablesStamp
			}
			if page.Next == nil {
				break
			}
			o.After = page.Next
		}
		var fromFleets map[uuid.UUID]int64
		var err error
		if fromFleets, err = collectors.WantedStampsForFleets(ctx, s.Databases.Runtime, OrgA, []uuid.UUID{c.Fleet.ID}); err != nil {
			t.Fatal(err)
		}
		nonZero := 0
		for _, got := range all {
			l := get(t, s, got)
			want := int64(0)
			if l.Resolution.From != collectors.FromNone {
				y, err := s.Collectors.YAML(ctx, OrgA, *l.Resolution.Ref())
				if err != nil {
					t.Fatal(err)
				}
				resolved, err := s.Variables.Variables(ctx, OrgA, l.Resolution.GroupVariableGroupID, l.PinnedVariableGroupID)
				if err != nil {
					t.Fatal(err)
				}
				want = orgvariables.WantedStamp(orgvariables.References(y.YAML), resolved, l.Resolution.GroupBindingStamp,
					stampOf(l.PinnedVariableGroupStamp))
			}
			if want != 0 {
				nonZero++
			}
			if l.Resolution.VariablesStamp != want || fromList[got.ID] != want || (fromFleets != nil && fromFleets[got.ID] != want) {
				t.Errorf("%s: Get %d, List %d, the fleets' rows %d; orgvariables.WantedStamp %d", got.Name,
					l.Resolution.VariablesStamp, fromList[got.ID], fromFleets[got.ID], want)
			}
		}
		if nonZero < len(all)/2 {
			t.Fatalf("only %d of %d collectors want a stamp: the fixture doesn't exercise the rule", nonZero, len(all))
		}
		if fromFleets != nil && len(fromFleets) != len(all) {
			t.Fatalf("the fleets' rows answered %d collectors, want %d", len(fromFleets), len(all))
		}
	})
}

// pinPastTheCheck pins the published version under the store, as only a
// test can.
func pinPastTheCheck(t *testing.T, s *Stores, collectorID, publicationID uuid.UUID) {
	t.Helper()
	if _, err := s.Databases.Owner.Exec(ctx, `update telflo_opamp.collector_instances set pinned_publication_id = $2
		where id = $1`, collectorID, publicationID); err != nil {
		t.Fatal(err)
	}
}

// moveTo moves the collector into the group, by Move or by Update.
func moveTo(t *testing.T, s *Stores, c collectors.Collector, group uuid.UUID, viaUpdate bool) {
	t.Helper()
	var err error
	if viaUpdate {
		_, err = s.Collectors.Update(ctx, c.OrganizationID, c.FleetID, c.ID, collectors.Changes{GroupID: &group}, Now)
	} else {
		_, err = s.Collectors.Move(ctx, c.OrganizationID, c.FleetID, c.ID, group, Now)
	}
	if err != nil {
		t.Fatalf("moving: %v", err)
	}
}

// groupOf creates a group of the fleet assigned the published version,
// using the variable group (none when nil).
func groupOf(t *testing.T, s *Stores, c fleets.Creation, name string, version uuid.UUID, variableGroup *uuid.UUID) uuid.UUID {
	t.Helper()
	g, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID, Name: name,
		AssignedPublicationID: &version, VariableGroupID: variableGroup, At: Now})
	if err != nil {
		t.Fatal(err)
	}
	return g.ID
}

// enrollInto enrolls a collector into the fleet's group.
func enrollInto(t *testing.T, s *Stores, c fleets.Creation, name string, group uuid.UUID) collectors.Collector {
	t.Helper()
	e := enrollment(c, name, Now)
	e.GroupID = &group
	r, err := s.Collectors.Enroll(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	return r.Collector
}

// A collector whose group's variable group changed reads pending until
// polled, and a move before that poll into a group of the same variable
// group (whose own setting's stamp is older than the collector's marker),
// or, after the old group's variable group was cleared, into a group of
// none, keeps it pending: the move carries the old group's setting's stamp,
// with no bump. Its next poll pushes the new values, and it reads in sync.
// By Move and by Update.
func TestSyncAMoveAfterAnUnpolledBindingChangeReadsPending(t *testing.T) {
	for _, viaUpdate := range []bool{false, true} {
		for _, noneToNone := range []bool{false, true} {
			Each(t, testDatabase, func(t *testing.T, s *Stores) {
				label := fmt.Sprintf("(update %v, none to none %v)", viaUpdate, noneToNone)
				d, _ := newSyncSetup(t, s)
				x, f := newVariableGroup(t, s, OrgA, "x"), newVariableGroup(t, s, OrgA, "f")
				setOverride(t, s, OrgA, x, d.region.ID, "from-x")
				setOverride(t, s, OrgA, f, d.region.ID, "from-f")
				var target *uuid.UUID // the new group's variable group, set long before
				if !noneToNone {
					target = &f
				}
				g2 := groupOf(t, s, d.c, "g2", d.version, target)
				g1 := groupOf(t, s, d.c, "g1", d.version, &x)
				c := enrollInto(t, s, d.c, "edge-01", g1)
				d.pollAll(t, s, c)
				before := revision(t, s, OrgA)

				bindGroup(t, s, d.c, g1, target) // f, or cleared
				syncIs(t, s, c, collectors.Pending, "g1's variable group changed "+label)
				moveTo(t, s, c, g2, viaUpdate)
				if r := revision(t, s, OrgA); r != before+1 {
					t.Fatalf("%s: the counter moved %d times; want once, the binding's (the move carries, never bumps)", label, r-before)
				}
				if l := get(t, s, c); stampOf(l.PinnedVariableGroupStamp) != before+1 || l.GroupID != g2 {
					t.Fatalf("%s: after the move the collector's stamp is %d, want the old group's %d", label,
						stampOf(l.PinnedVariableGroupStamp), before+1)
				}
				syncIs(t, s, c, collectors.Pending, "moved before its poll "+label)
				if !d.p.poll(c) {
					t.Fatalf("%s: the new values weren't pushed", label)
				}
				syncIs(t, s, c, collectors.InSync, "polled after the move "+label)
			})
		}
	}
}

// A move reads pending only when what the collector's text is filled in
// with changed. Between groups of the same variable group, or both none,
// with nothing unserved, it stays in sync and its poll pushes nothing;
// between groups whose variable groups differ, it reads pending until
// polled. By Move and by Update.
func TestSyncAMoveReadsPendingOnlyWhenWhatItUsesChanged(t *testing.T) {
	for _, viaUpdate := range []bool{false, true} {
		Each(t, testDatabase, func(t *testing.T, s *Stores) {
			d, _ := newSyncSetup(t, s)
			x, f := newVariableGroup(t, s, OrgA, "x"), newVariableGroup(t, s, OrgA, "f")
			setOverride(t, s, OrgA, x, d.region.ID, "from-x")
			setOverride(t, s, OrgA, f, d.region.ID, "from-f")
			// f-b's setting is stamped before f-a's, so the collector served
			// in f-a was served past it (a move into a group whose setting
			// is newer than the marker reads pending until a poll that moves
			// the marker alone, without a push).
			fB := groupOf(t, s, d.c, "f-b", d.version, &f)
			fA := groupOf(t, s, d.c, "f-a", d.version, &f)
			xA := groupOf(t, s, d.c, "x-a", d.version, &x)
			noneA, noneB := groupOf(t, s, d.c, "none-a", d.version, nil), groupOf(t, s, d.c, "none-b", d.version, nil)
			c := enrollInto(t, s, d.c, "edge-01", fA)
			d.pollAll(t, s, c)
			for _, step := range []struct {
				label   string
				group   uuid.UUID
				pending bool
			}{
				{"f to f", fB, false},
				{"f to x", xA, true},
				{"x to none", noneA, true},
				{"none to none", noneB, false},
				{"none to f", fA, true},
			} {
				moveTo(t, s, c, step.group, viaUpdate)
				label := fmt.Sprintf("%s (update %v)", step.label, viaUpdate)
				if !step.pending {
					syncIs(t, s, c, collectors.InSync, label)
					if d.p.poll(c) {
						t.Fatalf("%s: a move that changed nothing it uses was pushed", label)
					}
					continue
				}
				syncIs(t, s, c, collectors.Pending, label)
				if !d.p.poll(c) {
					t.Fatalf("%s: the new values weren't pushed", label)
				}
				syncIs(t, s, c, collectors.InSync, label+", polled")
			}
		})
	}
}

// A version published before migration 00012 has no names in
// publication_variable_names though its text holds a live ${org:NAME} (here
// deleted under the store, as such a version's are): its wanted stamp is 0
// from every source, so its collectors' status ignores variables (the poll
// still fills them in).
func TestSyncAVersionWithoutStoredNamesIgnoresVariables(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		region := defineVariable(t, s, OrgA, "REGION", "eu")
		old := publish(t, s, OrgA, regionYAML)
		forgetNames(t, s, old)
		assignGroup(t, s, c, c.DefaultGroupID, old)
		got := enroll(t, s, c, "edge-01", Now)
		p := newPoller(t, s)
		p.poll(got)
		syncIs(t, s, got, collectors.InSync, "polled")
		vg := newVariableGroup(t, s, OrgA, "eu")
		setOverride(t, s, OrgA, vg, region.ID, "x")
		setValue(t, s, OrgA, region.ID, "us")
		bindGroup(t, s, c, c.DefaultGroupID, &vg)
		pinVariableGroup(t, s, got, &vg)
		syncIs(t, s, got, collectors.InSync, "every kind of change, no names written")
		page, err := s.Collectors.List(ctx, OrgA, c.Fleet.ID, collectors.ListOptions{Sort: collectors.SortName,
			Order: collectors.Ascending, Limit: 10})
		if err != nil || len(page.Collectors) != 1 || page.Collectors[0].Resolution.VariablesStamp != 0 ||
			get(t, s, got).Resolution.VariablesStamp != 0 {
			t.Fatalf("the wanted stamp from Get or List isn't 0: %+v, %v", page.Collectors, err)
		}
		stamps, err := collectors.WantedStampsForFleets(ctx, s.Databases.Runtime, OrgA, []uuid.UUID{c.Fleet.ID})
		if err != nil || stamps[got.ID] != 0 || len(stamps) != 1 {
			t.Fatalf("the fleets' rows' wanted stamps = %v, %v; want 0", stamps, err)
		}
	})
}

// forgetNames deletes the names a published version references, under the
// store: what a version published before migration 00012 holds.
func forgetNames(t *testing.T, s *Stores, publicationID uuid.UUID) {
	t.Helper()
	if tag, err := s.Databases.Owner.Exec(ctx, `delete from telflo_opamp.publication_variable_names
		where publication_id = $1`, publicationID); err != nil || tag.RowsAffected() == 0 {
		t.Fatalf("deleting the names: %v, %v", tag, err)
	}
}
