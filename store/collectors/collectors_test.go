package collectors_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/telflo/opamp-server/store/collectorgroups"
	"github.com/telflo/opamp-server/store/collectors"
	"github.com/telflo/opamp-server/store/fleets"
	"github.com/telflo/opamp-server/store/organizations"
	"github.com/telflo/opamp-server/store/orgvariables"
	"github.com/telflo/opamp-server/store/publications"
	. "github.com/telflo/opamp-server/store/storetest"
	"github.com/telflo/opamp-server/store/variables"
)

// This package's own test database (CONTRIBUTING.md, "Tests").
const testDatabase = "telflo_opamp_server_collectors_test"

var ctx = context.Background()

// PubASHA256 is the fixture's published versions' SHA-256 (storetest's
// seed).
var pubSHA256 = strings.Repeat("ab", 32)

func sumOf(yaml string) string {
	sum := sha256.Sum256([]byte(yaml))
	return hex.EncodeToString(sum[:])
}

// enrollment is an enrollment into the fleet with its rotate token, at at.
func enrollment(c fleets.Creation, name string, at time.Time) collectors.Enrollment {
	return collectors.Enrollment{OrganizationID: c.Fleet.OrganizationID, FleetID: c.Fleet.ID,
		InstanceUID: uuid.New(), TokenID: c.RotateTokenID, DesiredName: name, At: at}
}

// enroll enrolls a collector named name into the fleet at at, and fails
// the test unless it is created.
func enroll(t *testing.T, s *Stores, c fleets.Creation, name string, at time.Time) collectors.Collector {
	t.Helper()
	r, err := s.Collectors.Enroll(ctx, enrollment(c, name, at))
	if err != nil || !r.Created {
		t.Fatalf("enrolling %q: %+v, %v", name, r, err)
	}
	return r.Collector
}

// publish stores another published version of the organization, of yaml.
func publish(t *testing.T, s *Stores, org uuid.UUID, yaml string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if result, _, err := s.Publications.Put(ctx, publications.Publication{
		ID: id, OrganizationID: org, ConfigurationID: uuid.New(), ConfigurationName: "edge", VersionID: uuid.New(),
		VersionNumber: 1, YAML: yaml, SHA256: sumOf(yaml), PublishedAt: Now, ReceivedAt: Now,
		Collector: publications.Collector{Distribution: "otelcol-contrib", Version: "0.162.0"},
	}); err != nil || result != publications.Created {
		t.Fatalf("publishing: %v, %v", result, err)
	}
	return id
}

func get(t *testing.T, s *Stores, c collectors.Collector) collectors.Listed {
	t.Helper()
	got, err := s.Collectors.Get(ctx, c.OrganizationID, c.FleetID, c.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return got
}

func setCap(t *testing.T, s *Stores, org uuid.UUID, max int32) {
	t.Helper()
	if _, err := s.Organizations.SetLimits(ctx, org, organizations.Limits{MaxCollectors: &max}); err != nil {
		t.Fatal(err)
	}
}

// A collector is enrolled into its fleet's Default group with its desired
// name, its token and the caller's time as its created, updated and
// last-seen times; a second Enroll of the instance uid answers the stored
// collector, changing nothing.
func TestEnrollStoresTheCollectorInTheDefaultGroupWithItsName(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		e := enrollment(c, "edge-01 ☃", Now)
		r, err := s.Collectors.Enroll(ctx, e)
		at := Millisecond(Now)
		got := r.Collector
		if err != nil || !r.Created || got.Name != "edge-01 ☃" || got.OrganizationID != OrgA || got.FleetID != c.Fleet.ID ||
			got.GroupID != c.DefaultGroupID || got.InstanceUID != e.InstanceUID || got.EnrollmentTokenID == nil ||
			*got.EnrollmentTokenID != c.RotateTokenID || !got.LastSeenAt.Equal(at) || !got.CreatedAt.Equal(at) ||
			!got.UpdatedAt.Equal(at) || got.CreatedAt.Location() != time.UTC || got.Pinned != nil || got.Served != nil ||
			got.EffectiveStatus != nil || got.ReportedAttributes != nil {
			t.Fatalf("Enroll = %+v, %v", r, err)
		}
		before := s.Dump(t, OrgA)
		again := e
		again.At, again.DesiredName, again.TokenID = Now.Add(time.Hour), "another", c.DefaultTokenID
		r, err = s.Collectors.Enroll(ctx, again)
		if err != nil || r.Created || r.Collector.ID != got.ID || r.Collector.Name != "edge-01 ☃" ||
			!r.Collector.LastSeenAt.Equal(at) {
			t.Fatalf("a second Enroll = %+v, %v; want the stored collector", r, err)
		}
		s.Unchanged(t, "a second Enroll", OrgA, before)
	})
}

// The group the collector names, when it is its fleet's; a taken or invalid
// desired name gives a petname instead.
func TestEnrollNamesTheGroupAndFallsBackToAPetname(t *testing.T) {
	// The petnames: queued ones first, then pet-1, pet-2, ...
	var queue []string
	generated := 0
	petnames := collectors.Petnames(func(words int) string {
		if len(queue) > 0 {
			name := queue[0]
			queue = queue[1:]
			return name
		}
		generated++
		return fmt.Sprintf("pet-%d-%d", words, generated)
	})
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		queue, generated = nil, 0
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		g, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID, Name: "gateways", At: Now})
		if err != nil {
			t.Fatal(err)
		}
		e := enrollment(c, "taken", Now)
		e.GroupID = &g.ID
		r, err := s.Collectors.Enroll(ctx, e)
		if err != nil || r.Collector.GroupID != g.ID || r.Collector.Name != "taken" {
			t.Fatalf("Enroll into the named group = %+v, %v", r, err)
		}
		queue = []string{"brave-otter"}
		if got := enroll(t, s, c, "taken", Now); got.Name != "brave-otter" {
			t.Fatalf("a taken name gave %q, want the petname", got.Name)
		}
		for _, desired := range []string{"", " edge", "edge\t", "\u3000edge", strings.Repeat("e", 201), "ed\x00ge", "edge\xff",
			"edge\t01", "edge-01\nrack-7", "edge\x0701", "edge\u009b01"} {
			got := enroll(t, s, c, desired, Now)
			if got.Name == desired || !strings.HasPrefix(got.Name, "pet-2-") {
				t.Fatalf("the desired name %q gave %q, want a petname", desired, got.Name)
			}
		}
		// Taken petnames are passed over for the next; from the fifth
		// attempt they have three words.
		queue = []string{"taken", "brave-otter", "taken", "brave-otter"}
		if got := enroll(t, s, c, "", Now); got.Name != "pet-3-"+fmt.Sprint(generated) {
			t.Fatalf("after four taken petnames: %q", got.Name)
		}
	}, WithCollectors(petnames))
}

// What Enroll refuses, changing nothing: a group of another fleet or
// organization (ErrForeignGroup), an instance uid enrolled in another fleet
// (ErrFleetMismatch: never rebound), a token of another fleet, another
// organization's fleet, an unregistered organization.
func TestEnrollRefusesAForeignGroupAnotherFleetsCollectorAndForeignIDs(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		mine := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		other := s.CreateFleet(t, OrgA, "edge-dev", fleets.TypeServer)
		theirs := s.CreateFleet(t, OrgB, "globex", fleets.TypeServer)
		enrolled := enroll(t, s, other, "edge-dev-01", Now)
		beforeA, beforeB := s.Dump(t, OrgA), s.Dump(t, OrgB)

		for label, change := range map[string]func(e *collectors.Enrollment){
			"another fleet's group":        func(e *collectors.Enrollment) { e.GroupID = &other.DefaultGroupID },
			"another organization's group": func(e *collectors.Enrollment) { e.GroupID = &theirs.DefaultGroupID },
			"an unknown group":             func(e *collectors.Enrollment) { e.GroupID = Ptr(uuid.New()) },
		} {
			e := enrollment(mine, "edge-01", Now)
			change(&e)
			if _, err := s.Collectors.Enroll(ctx, e); !errors.Is(err, collectors.ErrForeignGroup) {
				t.Errorf("%s: Enroll = %v, want ErrForeignGroup", label, err)
			}
		}
		e := enrollment(mine, "edge-01", Now)
		e.InstanceUID = enrolled.InstanceUID
		if _, err := s.Collectors.Enroll(ctx, e); !errors.Is(err, collectors.ErrFleetMismatch) {
			t.Errorf("another fleet's instance uid: Enroll = %v, want ErrFleetMismatch", err)
		}
		e = enrollment(mine, "edge-01", Now)
		e.TokenID = other.RotateTokenID
		if _, err := s.Collectors.Enroll(ctx, e); !errors.Is(err, collectors.ErrTokenNotFound) {
			t.Errorf("another fleet's token: Enroll = %v, want ErrTokenNotFound", err)
		}
		e = enrollment(theirs, "edge-01", Now)
		e.OrganizationID = OrgA
		if _, err := s.Collectors.Enroll(ctx, e); !errors.Is(err, collectors.ErrFleetNotFound) {
			t.Errorf("another organization's fleet: Enroll = %v, want ErrFleetNotFound", err)
		}
		e = enrollment(mine, "edge-01", Now)
		e.OrganizationID = OrgUnregistered
		if _, err := s.Collectors.Enroll(ctx, e); !errors.Is(err, collectors.ErrFleetNotFound) {
			t.Errorf("an unregistered organization: Enroll = %v, want ErrFleetNotFound", err)
		}
		s.Unchanged(t, "the refused enrollments", OrgA, beforeA)
		s.Unchanged(t, "the refused enrollments", OrgB, beforeB)
		// The same instance uid in another organization is another collector.
		e = enrollment(theirs, "edge-dev-01", Now)
		e.InstanceUID = enrolled.InstanceUID
		if r, err := s.Collectors.Enroll(ctx, e); err != nil || !r.Created || r.Collector.Name != "edge-dev-01" {
			t.Fatalf("A's uid and name under B = %+v, %v; want created", r, err)
		}
	})
}

// The cap is counted under the organization's lock: at max_collectors, a
// new collector is ErrCapReached (naming no id or count), a collector
// already enrolled is still answered, a delete frees its slot, an archived
// fleet's collectors count, and lowering the cap below the count trims
// nothing.
func TestEnrollCountsTheCapAndADeleteFreesASlot(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		archived := s.CreateFleet(t, OrgA, "edge-old", fleets.TypeServer)
		old := enroll(t, s, archived, "old-01", Now)
		if _, err := s.Fleets.Update(ctx, OrgA, archived.Fleet.ID, fleets.Changes{Status: Ptr(fleets.StatusArchived)}, Now); err != nil {
			t.Fatal(err)
		}
		setCap(t, s, OrgA, 2)
		first := enroll(t, s, c, "edge-01", Now)
		before := s.Dump(t, OrgA)
		_, err := s.Collectors.Enroll(ctx, enrollment(c, "edge-02", Now))
		if !errors.Is(err, collectors.ErrCapReached) {
			t.Fatalf("over the cap: Enroll = %v, want ErrCapReached", err)
		}
		if text := err.Error(); strings.Contains(text, OrgA.String()) || strings.ContainsAny(text, "0123456789") {
			t.Fatalf("the cap's error names an id or a count: %q", text)
		}
		s.Unchanged(t, "the refused enrollment", OrgA, before)
		again := enrollment(c, "edge-01", Now.Add(time.Minute))
		again.InstanceUID = first.InstanceUID
		if r, err := s.Collectors.Enroll(ctx, again); err != nil || r.Created || r.Collector.ID != first.ID {
			t.Fatalf("an enrolled collector at the cap = %+v, %v", r, err)
		}
		// B's cap is its own.
		setCap(t, s, OrgB, 0)
		if deleted, err := s.Collectors.Delete(ctx, OrgA, archived.Fleet.ID, old.ID); err != nil || !deleted {
			t.Fatalf("Delete = %v, %v", deleted, err)
		}
		enroll(t, s, c, "edge-02", Now)
		setCap(t, s, OrgA, 1)
		if _, err := s.Collectors.Enroll(ctx, enrollment(c, "edge-03", Now)); !errors.Is(err, collectors.ErrCapReached) {
			t.Fatalf("under a lowered cap: Enroll = %v", err)
		}
		page, err := s.Collectors.List(ctx, OrgA, c.Fleet.ID, collectors.ListOptions{Sort: collectors.SortCreated,
			Order: collectors.Descending, Limit: 10})
		if err != nil || len(page.Collectors) != 2 {
			t.Fatalf("lowering the cap trimmed: %+v, %v", page, err)
		}
		if _, err := s.Organizations.SetLimits(ctx, OrgA, organizations.Limits{}); err != nil {
			t.Fatal(err)
		}
		enroll(t, s, c, "edge-03", Now)
		if _, err := s.Collectors.Enroll(ctx, enrollment(s.CreateFleet(t, OrgB, "globex", fleets.TypeServer), "x", Now)); !errors.Is(err, collectors.ErrCapReached) {
			t.Fatalf("B at a cap of 0: %v", err)
		}
	})
}

// Poll answers the collector as it was before the heartbeat, writes the
// heartbeat only when the last one is at least HeartbeatCoalesce old by the
// caller's clock or the token changed, and for a token of another fleet
// answers the row but writes nothing; another organization's instance uid
// finds nothing.
func TestPollAnswersTheRowBeforeItsHeartbeatAndCoalescesIt(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		other := s.CreateFleet(t, OrgA, "edge-dev", fleets.TypeServer)
		got := enroll(t, s, c, "edge-01", Now)
		poll := func(at time.Time, fleet fleets.Creation, token uuid.UUID) collectors.PollResult {
			t.Helper()
			r, err := s.Collectors.Poll(ctx, collectors.PollRequest{OrganizationID: OrgA, FleetID: fleet.Fleet.ID,
				InstanceUID: got.InstanceUID, TokenID: token, At: at})
			if err != nil || !r.Found || r.Collector.ID != got.ID {
				t.Fatalf("Poll = %+v, %v", r, err)
			}
			return r
		}
		at := Millisecond(Now)
		if r := poll(Now.Add(HeartbeatEdge-time.Millisecond), c, c.RotateTokenID); r.Touched || !r.Collector.LastSeenAt.Equal(at) {
			t.Fatalf("a poll 14.999 s on = %+v; want untouched", r)
		}
		r := poll(Now.Add(HeartbeatEdge), c, c.RotateTokenID)
		if !r.Touched || !r.Collector.LastSeenAt.Equal(at) {
			t.Fatalf("a poll 15 s on = %+v; want touched, answering the row before", r)
		}
		if r := poll(Now.Add(HeartbeatEdge+time.Second), c, c.RotateTokenID); r.Touched ||
			!r.Collector.LastSeenAt.Equal(Millisecond(Now.Add(HeartbeatEdge))) {
			t.Fatalf("a poll a second after a heartbeat = %+v; want untouched", r)
		}
		r = poll(Now.Add(HeartbeatEdge+2*time.Second), c, c.DefaultTokenID)
		if !r.Touched || *r.Collector.EnrollmentTokenID != c.RotateTokenID {
			t.Fatalf("a poll on another token = %+v; want touched, answering the old token", r)
		}
		listed := get(t, s, got)
		if !listed.LastSeenAt.Equal(Millisecond(Now.Add(HeartbeatEdge+2*time.Second))) || *listed.EnrollmentTokenID != c.DefaultTokenID {
			t.Fatalf("after the token's change: %+v", listed.Collector)
		}

		before := s.Dump(t, OrgA)
		r = poll(Now.Add(time.Hour), other, other.RotateTokenID)
		if r.Touched || r.Collector.FleetID != c.Fleet.ID {
			t.Fatalf("a token of another fleet = %+v; want the row, untouched", r)
		}
		s.Unchanged(t, "a poll with another fleet's token", OrgA, before)
		theirs, err := s.Collectors.Poll(ctx, collectors.PollRequest{OrganizationID: OrgB, FleetID: c.Fleet.ID,
			InstanceUID: got.InstanceUID, TokenID: c.RotateTokenID, At: Now.Add(time.Hour)})
		if err != nil || theirs.Found || theirs.Touched || theirs.Collector.ID != uuid.Nil {
			t.Fatalf("another organization's poll = %+v, %v; want nothing", theirs, err)
		}
		s.Unchanged(t, "another organization's poll", OrgA, before)
		if r, err := s.Collectors.Poll(ctx, collectors.PollRequest{OrganizationID: OrgA, FleetID: c.Fleet.ID,
			InstanceUID: uuid.New(), TokenID: c.RotateTokenID, At: Now}); err != nil || r.Found {
			t.Fatalf("an unknown uid = %+v, %v", r, err)
		}
	})
}

// HeartbeatEdge is collectors.HeartbeatCoalesce.
const HeartbeatEdge = collectors.HeartbeatCoalesce

// What resolves, and where from: nothing; the group's assigned published
// version; a pinned published version over it; nothing again once unpinned
// and the group's assignment cleared; each by its SHA-256, and YAML reads
// the bytes by the organization.
func TestPollResolvesThePinElseTheGroupsPublishedVersion(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		got := enroll(t, s, c, "edge-01", Now)
		resolution := func() collectors.Resolution {
			t.Helper()
			r, err := s.Collectors.Poll(ctx, collectors.PollRequest{OrganizationID: OrgA, FleetID: c.Fleet.ID,
				InstanceUID: got.InstanceUID, TokenID: c.RotateTokenID, At: Now})
			if err != nil || !r.Found {
				t.Fatalf("Poll = %+v, %v", r, err)
			}
			if listed := get(t, s, got); listed.Resolution != r.Resolution {
				t.Fatalf("Get resolves %+v, Poll %+v", listed.Resolution, r.Resolution)
			}
			return r.Resolution
		}
		if r := resolution(); r != (collectors.Resolution{From: collectors.FromNone}) || r.Ref() != nil {
			t.Fatalf("nothing assigned resolves %+v", r)
		}
		if _, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, c.DefaultGroupID,
			collectorgroups.Changes{AssignedPublicationID: collectorgroups.SetTo(PubA)}, Now); err != nil {
			t.Fatal(err)
		}
		if r := resolution(); r != (collectors.Resolution{From: collectors.FromGroupPublication, ID: PubA, SHA256: pubSHA256}) ||
			*r.Ref() != (collectors.Ref{Source: collectors.SourcePublication, ID: PubA}) {
			t.Fatalf("the group's published version resolves %+v", r)
		}
		pinnedYAML := "exporters: {debug: {}}\n"
		pinned := publish(t, s, OrgA, pinnedYAML)
		if _, err := s.Collectors.Pin(ctx, OrgA, c.Fleet.ID, got.ID,
			&collectors.Ref{Source: collectors.SourcePublication, ID: pinned}, Now); err != nil {
			t.Fatal(err)
		}
		if r := resolution(); r != (collectors.Resolution{From: collectors.FromPinnedPublication, ID: pinned, SHA256: sumOf(pinnedYAML)}) {
			t.Fatalf("a pinned published version resolves %+v", r)
		}
		y, err := s.Collectors.YAML(ctx, OrgA, collectors.Ref{Source: collectors.SourcePublication, ID: pinned})
		if err != nil || y.YAML != pinnedYAML || y.SHA256 != sumOf(pinnedYAML) {
			t.Fatalf("YAML of the published version = %+v, %v", y, err)
		}

		if _, err := s.Collectors.Pin(ctx, OrgA, c.Fleet.ID, got.ID, nil, Now); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, c.DefaultGroupID,
			collectorgroups.Changes{AssignedPublicationID: collectorgroups.Clear[uuid.UUID]()}, Now); err != nil {
			t.Fatal(err)
		}
		if r := resolution(); r != (collectors.Resolution{From: collectors.FromNone}) || r.Ref() != nil {
			t.Fatalf("unpinned with nothing assigned resolves %+v", r)
		}

		// YAML reads only the organization's published versions.
		for label, call := range map[string]func() error{
			"B's published version": func() error {
				_, err := s.Collectors.YAML(ctx, OrgA, collectors.Ref{Source: collectors.SourcePublication, ID: PubB})
				return err
			},
			"A's published version under B": func() error {
				_, err := s.Collectors.YAML(ctx, OrgB, collectors.Ref{Source: collectors.SourcePublication, ID: pinned})
				return err
			},
		} {
			if err := call(); !errors.Is(err, collectors.ErrNotFound) {
				t.Errorf("YAML of %s = %v, want ErrNotFound", label, err)
			}
		}
		// A published version is the only kind; "import" is as unknown as any
		// other.
		for _, source := range []collectors.Source{"config", "import"} {
			if _, err := s.Collectors.YAML(ctx, OrgA, collectors.Ref{Source: source, ID: pinned}); !errors.Is(err, collectors.ErrInvalid) {
				t.Errorf("YAML of the source %q = %v", source, err)
			}
		}
	})
}

// Report writes each part given in one update, and writes nothing when
// every part is as stored; Reconciled moves the served pointer without
// moving ServedAt; the effective error is kept only with failed, bounded;
// the reported fields and attributes; full state asked.
func TestReportWritesOnlyWhatChanged(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		got := enroll(t, s, c, "edge-01", Now)
		report := func(r collectors.Report) bool {
			t.Helper()
			r.OrganizationID, r.FleetID, r.CollectorID = OrgA, c.Fleet.ID, got.ID
			written, err := s.Collectors.Report(ctx, r)
			if err != nil {
				t.Fatalf("Report = %v", err)
			}
			return written
		}
		served := Now.Add(time.Minute)
		delivery := collectors.Report{Served: &collectors.Served{Ref: collectors.Ref{Source: collectors.SourcePublication, ID: PubA},
			SHA256: pubSHA256, At: served}}
		if !report(delivery) {
			t.Fatal("a delivery wrote nothing")
		}
		if report(delivery) {
			t.Fatal("the same delivery again wrote")
		}
		if report(collectors.Report{}) {
			t.Fatal("an empty report wrote")
		}
		row := get(t, s, got)
		if row.Served == nil || *row.Served != (collectors.Ref{Source: collectors.SourcePublication, ID: PubA}) ||
			*row.ServedSHA256 != pubSHA256 || !row.ServedAt.Equal(Millisecond(served)) || !row.UpdatedAt.Equal(Millisecond(Now)) {
			t.Fatalf("after the delivery: %+v", row.Collector)
		}

		other := publish(t, s, OrgA, "exporters: {}\n")
		if !report(collectors.Report{Reconciled: &collectors.Reconciled{Ref: collectors.Ref{Source: collectors.SourcePublication, ID: other},
			SHA256: sumOf("exporters: {}\n"), At: Now.Add(90 * time.Second)}}) {
			t.Fatal("a reconcile wrote nothing")
		}
		row = get(t, s, got)
		if *row.Served != (collectors.Ref{Source: collectors.SourcePublication, ID: other}) ||
			*row.ServedSHA256 != sumOf("exporters: {}\n") || !row.ServedAt.Equal(Millisecond(served)) {
			t.Fatalf("after the reconcile: %+v", row.Collector)
		}

		// A failure's message is kept, bounded; applying clears it.
		long := strings.Repeat("é", collectors.MaxMessageBytes) // two bytes each
		failed := collectors.Effective{SHA256: pubSHA256, Status: collectors.StatusFailed, Error: long, At: Now.Add(2 * time.Minute)}
		if !report(collectors.Report{Effective: &failed}) {
			t.Fatal("a failure wrote nothing")
		}
		row = get(t, s, got)
		if *row.EffectiveStatus != collectors.StatusFailed || *row.EffectiveSHA256 != pubSHA256 ||
			*row.EffectiveError != strings.Repeat("é", collectors.MaxMessageBytes/2) || !row.EffectiveAt.Equal(Millisecond(failed.At)) {
			t.Fatalf("after the failure: %+v (%d bytes of error)", row.Collector, len(*row.EffectiveError))
		}
		if report(collectors.Report{Effective: &failed}) {
			t.Fatal("the same failure again wrote")
		}
		failed.At = failed.At.Add(time.Minute)
		if !report(collectors.Report{Effective: &failed}) {
			t.Fatal("a re-report at a later time wrote nothing: it must refresh EffectiveAt")
		}
		applying := collectors.Effective{SHA256: pubSHA256, Status: collectors.StatusApplying, Error: "kept?", At: Now.Add(4 * time.Minute)}
		report(collectors.Report{Effective: &applying})
		if row = get(t, s, got); row.EffectiveError != nil || *row.EffectiveStatus != collectors.StatusApplying {
			t.Fatalf("applying kept an error: %+v", row.Collector)
		}

		reported := collectors.Reported{CollectorVersion: Ptr("0.162.0"), IP: Ptr("10.0.0.7"), HostIP: Ptr("192.168.1.7"),
			Attributes: map[string]string{"host.name": "edge-01", "os.type": "linux", "quote\"": "tab\tline\n\x01"}}
		asked := Now.Add(5 * time.Minute)
		if !report(collectors.Report{Reported: reported, FullStateRequestedAt: &asked}) {
			t.Fatal("the reported fields wrote nothing")
		}
		if report(collectors.Report{Reported: reported, FullStateRequestedAt: &asked}) {
			t.Fatal("the same reported fields again wrote")
		}
		row = get(t, s, got)
		if *row.ReportedCollectorVersion != "0.162.0" || *row.ReportedIP != "10.0.0.7" || *row.ReportedHostIP != "192.168.1.7" ||
			Describe(row.ReportedAttributes) != Describe(reported.Attributes) || !row.FullStateRequestedAt.Equal(Millisecond(asked)) {
			t.Fatalf("after the reported fields: %+v", row.Collector)
		}
		// Each field alone; nil leaves the rest.
		if !report(collectors.Report{Reported: collectors.Reported{IP: Ptr("10.0.0.8")}}) {
			t.Fatal("a new IP wrote nothing")
		}
		if !report(collectors.Report{Reported: collectors.Reported{Attributes: map[string]string{}}}) {
			t.Fatal("emptied attributes wrote nothing")
		}
		row = get(t, s, got)
		if *row.ReportedIP != "10.0.0.8" || *row.ReportedHostIP != "192.168.1.7" || row.ReportedAttributes == nil ||
			len(row.ReportedAttributes) != 0 {
			t.Fatalf("after the IP and the attributes: %+v", row.Collector)
		}
	})
}

// What Report refuses, writing nothing: values out of bounds (a served ref
// of another kind than a published version among them), a served
// published version the organization doesn't hold; another
// organization's or fleet's collector is not written.
func TestReportRefusesWhatTheTableRefusesAndForeignRows(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		other := s.CreateFleet(t, OrgA, "edge-dev", fleets.TypeServer)
		got := enroll(t, s, c, "edge-01", Now)
		enroll(t, s, other, "edge-dev-01", Now)
		before := s.Dump(t, OrgA)
		ref := collectors.Ref{Source: collectors.SourcePublication, ID: PubA}
		big := map[string]string{}
		for i := range 200 {
			big[uuid.NewString()] = strings.Repeat("v", 50) + string(rune('a'+i%26))
		}
		for label, r := range map[string]collectors.Report{
			"served and reconciled": {Served: &collectors.Served{Ref: ref, SHA256: pubSHA256, At: Now},
				Reconciled: &collectors.Reconciled{Ref: ref, SHA256: pubSHA256, At: Now}},
			"a reconcile without a time": {Reconciled: &collectors.Reconciled{Ref: ref, SHA256: pubSHA256}},
			"a served hash in capitals":  {Served: &collectors.Served{Ref: ref, SHA256: strings.ToUpper(pubSHA256), At: Now}},
			"a served ref of no source":  {Served: &collectors.Served{Ref: collectors.Ref{ID: PubA}, SHA256: pubSHA256, At: Now}},
			"a served import":            {Served: &collectors.Served{Ref: collectors.Ref{Source: "import", ID: PubA}, SHA256: pubSHA256, At: Now}},
			"an effective unknown state": {Effective: &collectors.Effective{SHA256: pubSHA256, Status: "unset", At: Now}},
			"an effective short hash":    {Effective: &collectors.Effective{SHA256: "abc", Status: collectors.StatusApplied, At: Now}},
			"an empty version":           {Reported: collectors.Reported{CollectorVersion: Ptr("")}},
			"a version of 201":           {Reported: collectors.Reported{CollectorVersion: Ptr(strings.Repeat("1", 201))}},
			"an IP of 101":               {Reported: collectors.Reported{IP: Ptr(strings.Repeat("1", 101))}},
			"a host IP with a NUL":       {Reported: collectors.Reported{HostIP: Ptr("10.0.0.1\x00")}},
			"attributes over 16 KiB":     {Reported: collectors.Reported{Attributes: big}},
			"an attribute with a NUL":    {Reported: collectors.Reported{Attributes: map[string]string{"a": "\x00"}}},
			"an attribute not UTF-8":     {Reported: collectors.Reported{Attributes: map[string]string{"a\xff": "b"}}},
		} {
			r.OrganizationID, r.FleetID, r.CollectorID = OrgA, c.Fleet.ID, got.ID
			if _, err := s.Collectors.Report(ctx, r); !errors.Is(err, collectors.ErrInvalid) {
				t.Errorf("%s: Report = %v, want ErrInvalid", label, err)
			}
		}
		for label, want := range map[string]struct {
			ref collectors.Ref
			err error
		}{
			"B's published version": {collectors.Ref{Source: collectors.SourcePublication, ID: PubB}, collectors.ErrPublicationNotFound},
		} {
			_, err := s.Collectors.Report(ctx, collectors.Report{OrganizationID: OrgA, FleetID: c.Fleet.ID, CollectorID: got.ID,
				Served: &collectors.Served{Ref: want.ref, SHA256: pubSHA256, At: Now}})
			if !errors.Is(err, want.err) {
				t.Errorf("serving %s: Report = %v, want %v", label, err, want.err)
			}
		}
		write := collectors.Report{Effective: &collectors.Effective{SHA256: pubSHA256, Status: collectors.StatusApplied, At: Now},
			Reported: collectors.Reported{IP: Ptr("10.0.0.1")}}
		for label, ids := range map[string][3]uuid.UUID{
			"through another fleet":        {OrgA, other.Fleet.ID, got.ID},
			"through another organization": {OrgB, c.Fleet.ID, got.ID},
			"an unknown collector":         {OrgA, c.Fleet.ID, uuid.New()},
		} {
			r := write
			r.OrganizationID, r.FleetID, r.CollectorID = ids[0], ids[1], ids[2]
			if written, err := s.Collectors.Report(ctx, r); err != nil || written {
				t.Errorf("%s: Report = %v, %v; want nothing written", label, written, err)
			}
		}
		s.Unchanged(t, "the refused reports", OrgA, before)
		// Just under the attributes' bound is accepted.
		edge := map[string]string{"k": strings.Repeat("v", collectors.MaxAttributesBytes-len(`{"k": ""}`))}
		if written, err := s.Collectors.Report(ctx, collectors.Report{OrganizationID: OrgA, FleetID: c.Fleet.ID, CollectorID: got.ID,
			Reported: collectors.Reported{Attributes: edge}}); err != nil || !written {
			t.Fatalf("attributes of exactly 16 KiB: %v, %v", written, err)
		}
		edge["k"] += "v"
		if _, err := s.Collectors.Report(ctx, collectors.Report{OrganizationID: OrgA, FleetID: c.Fleet.ID, CollectorID: got.ID,
			Reported: collectors.Reported{Attributes: edge}}); !errors.Is(err, collectors.ErrInvalid) {
			t.Fatalf("attributes of 16 KiB and a byte: %v", err)
		}
	})
}

// Each error row trims its collector's history to its newest MaxErrors;
// ListErrors pages it newest first; the message is bounded on a character's
// boundary; another fleet's or organization's collector is not found and
// unchanged.
func TestRecordErrorKeepsTheNewestHundredAndListErrorsPagesThem(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		other := s.CreateFleet(t, OrgA, "edge-dev", fleets.TypeServer)
		got := enroll(t, s, c, "edge-01", Now)
		neighbour := enroll(t, s, c, "edge-02", Now)
		if err := s.Collectors.RecordError(ctx, OrgA, c.Fleet.ID, neighbour.ID, collectors.ErrorReport{Message: "theirs", At: Now}); err != nil {
			t.Fatal(err)
		}
		const reported = collectors.MaxErrors + 5
		for i := range reported {
			if err := s.Collectors.RecordError(ctx, OrgA, c.Fleet.ID, got.ID, collectors.ErrorReport{
				ConfigSHA256: Ptr(pubSHA256), Message: "failure " + string(rune('A'+i%26)), At: Now.Add(time.Duration(i) * time.Second),
			}); err != nil {
				t.Fatalf("RecordError %d: %v", i, err)
			}
		}
		var listed []collectors.CollectorError
		var after *collectors.ErrorCursor
		for pages := 0; ; pages++ {
			page, err := s.Collectors.ListErrors(ctx, OrgA, c.Fleet.ID, got.ID, 30, after)
			if err != nil || pages > 5 {
				t.Fatalf("ListErrors: %v (page %d)", err, pages)
			}
			listed = append(listed, page.Errors...)
			if page.Next == nil {
				break
			}
			parsed, err := collectors.ParseErrorCursor(page.Next.String())
			if err != nil {
				t.Fatal(err)
			}
			after = &parsed
		}
		if len(listed) != collectors.MaxErrors {
			t.Fatalf("kept %d errors, want %d", len(listed), collectors.MaxErrors)
		}
		for i, e := range listed {
			want := Millisecond(Now.Add(time.Duration(reported-1-i) * time.Second))
			if !e.ReportedAt.Equal(want) || e.CollectorID != got.ID || *e.ConfigSHA256 != pubSHA256 {
				t.Fatalf("error %d = %+v, want reported at %v", i, e, want)
			}
		}
		page, err := s.Collectors.ListErrors(ctx, OrgA, c.Fleet.ID, neighbour.ID, 100, nil)
		if err != nil || len(page.Errors) != 1 || page.Errors[0].Message != "theirs" || page.Errors[0].ConfigSHA256 != nil {
			t.Fatalf("the neighbour's history = %+v, %v", page, err)
		}

		// The message, bounded: a NUL and invalid UTF-8 replaced, cut at
		// MaxMessageBytes on a character's boundary.
		long := "a\x00b\xff" + strings.Repeat("€", collectors.MaxMessageBytes)
		if err := s.Collectors.RecordError(ctx, OrgA, c.Fleet.ID, neighbour.ID, collectors.ErrorReport{Message: long, At: Now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		page, err = s.Collectors.ListErrors(ctx, OrgA, c.Fleet.ID, neighbour.ID, 1, nil)
		if err != nil || page.Errors[0].Message != collectors.BoundMessage(long) {
			t.Fatalf("the long message = %v", err)
		}
		if m := page.Errors[0].Message; len(m) > collectors.MaxMessageBytes || !strings.HasPrefix(m, "a�b�€") {
			t.Fatalf("the long message was stored as %d bytes starting %q", len(m), m[:12])
		}

		before := s.Dump(t, OrgA)
		for label, err := range map[string]error{
			"an empty message":     s.Collectors.RecordError(ctx, OrgA, c.Fleet.ID, got.ID, collectors.ErrorReport{At: Now}),
			"a hash in capitals":   s.Collectors.RecordError(ctx, OrgA, c.Fleet.ID, got.ID, collectors.ErrorReport{Message: "x", ConfigSHA256: Ptr(strings.ToUpper(pubSHA256)), At: Now}),
			"an unknown collector": s.Collectors.RecordError(ctx, OrgA, c.Fleet.ID, uuid.New(), collectors.ErrorReport{Message: "x", At: Now}),
		} {
			if !errors.Is(err, collectors.ErrInvalid) && !errors.Is(err, collectors.ErrNotFound) {
				t.Errorf("%s: RecordError = %v", label, err)
			}
		}
		for label, ids := range map[string][3]uuid.UUID{
			"through another fleet":        {OrgA, other.Fleet.ID, got.ID},
			"through another organization": {OrgB, c.Fleet.ID, got.ID},
		} {
			if err := s.Collectors.RecordError(ctx, ids[0], ids[1], ids[2], collectors.ErrorReport{Message: "x", At: Now}); !errors.Is(err, collectors.ErrNotFound) {
				t.Errorf("%s: RecordError = %v, want ErrNotFound", label, err)
			}
			if _, err := s.Collectors.ListErrors(ctx, ids[0], ids[1], ids[2], 10, nil); !errors.Is(err, collectors.ErrNotFound) {
				t.Errorf("%s: ListErrors = %v, want ErrNotFound", label, err)
			}
		}
		s.Unchanged(t, "the refused error rows", OrgA, before)
		for _, limit := range []int{0, 101} {
			if _, err := s.Collectors.ListErrors(ctx, OrgA, c.Fleet.ID, got.ID, limit, nil); !errors.Is(err, collectors.ErrInvalid) {
				t.Errorf("a limit of %d: %v", limit, err)
			}
		}
		// A deleted collector takes its errors.
		if deleted, err := s.Collectors.Delete(ctx, OrgA, c.Fleet.ID, got.ID); err != nil || !deleted {
			t.Fatal(deleted, err)
		}
		if n := strings.Count(s.Dump(t, OrgA), "collector_errors "); n != 2 {
			t.Fatalf("%d error rows left, want the neighbour's 2", n)
		}
	})
}

// A OncePerHash row is written once per hash and message per collector: the
// same hash and message again write nothing, a new hash or another message
// a row, another collector's row doesn't count, and the message is compared
// bounded. Without OncePerHash a row is written each time; OncePerHash
// without a hash is ErrInvalid.
func TestRecordErrorOncePerHashWritesOneRowPerHash(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		got := enroll(t, s, c, "edge-01", Now)
		neighbour := enroll(t, s, c, "edge-02", Now)
		const message = "The configuration with its organization variables filled in is over 1 MiB."
		first, second := sumOf("first"), sumOf("second")
		at := Now
		record := func(id uuid.UUID, hash *string, message string, once bool) {
			t.Helper()
			at = at.Add(time.Second)
			if err := s.Collectors.RecordError(ctx, OrgA, c.Fleet.ID, id, collectors.ErrorReport{
				ConfigSHA256: hash, Message: message, At: at, OncePerHash: once}); err != nil {
				t.Fatal(err)
			}
		}
		rows := func(id uuid.UUID) int {
			t.Helper()
			page, err := s.Collectors.ListErrors(ctx, OrgA, c.Fleet.ID, id, collectors.MaxListLimit, nil)
			if err != nil {
				t.Fatal(err)
			}
			return len(page.Errors)
		}
		record(neighbour.ID, &first, message, true)
		for range 3 {
			record(got.ID, &first, message, true)
		}
		if n := rows(got.ID); n != 1 {
			t.Fatalf("the same hash and message three times: %d rows; want 1 (another collector's doesn't count)", n)
		}
		record(got.ID, &second, message, true)
		record(got.ID, &second, message, true)
		if n := rows(got.ID); n != 2 {
			t.Fatalf("a new hash: %d rows; want 2", n)
		}
		record(got.ID, &first, "another message", true)
		if n := rows(got.ID); n != 3 {
			t.Fatalf("another message of the same hash: %d rows; want 3", n)
		}
		long := strings.Repeat("€", collectors.MaxMessageBytes)
		record(got.ID, &first, long, true)
		record(got.ID, &first, long+" and more", true)
		if n := rows(got.ID); n != 4 {
			t.Fatalf("two messages bounded alike: %d rows; want 4", n)
		}
		for range 2 {
			record(got.ID, &first, message, false)
		}
		if n := rows(got.ID); n != 6 {
			t.Fatalf("without OncePerHash: %d rows; want one each time (6)", n)
		}
		before := s.Dump(t, OrgA)
		if err := s.Collectors.RecordError(ctx, OrgA, c.Fleet.ID, got.ID, collectors.ErrorReport{
			Message: message, At: at, OncePerHash: true}); !errors.Is(err, collectors.ErrInvalid) {
			t.Fatalf("OncePerHash without a hash: %v; want ErrInvalid", err)
		}
		s.Unchanged(t, "the refused row", OrgA, before)
		if n := rows(neighbour.ID); n != 1 {
			t.Fatalf("the neighbour holds %d rows; want its 1", n)
		}
	})
}

// Cleanup deletes every organization's error rows reported before the
// cutoff, in batches, and keeps the rest; it holds the advisory lock while
// it does.
func TestCleanupDeletesErrorsBeforeTheCutoffAcrossOrganizations(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		a := enroll(t, s, s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer), "edge-01", Now)
		b := enroll(t, s, s.CreateFleet(t, OrgB, "globex", fleets.TypeServer), "globex-01", Now)
		cutoff := Now.Add(-collectors.ErrorRetention)
		old := collectors.CleanupBatchSize + 150
		insertErrors(t, s, a, old/2, cutoff.Add(-time.Hour))
		insertErrors(t, s, b, old-old/2, cutoff.Add(-time.Millisecond))
		insertErrors(t, s, a, 3, cutoff)
		insertErrors(t, s, b, 2, Now)

		result, err := s.Collectors.Cleanup(ctx, cutoff)
		if err != nil || !result.Held || result.Deleted != old {
			t.Fatalf("Cleanup = %+v, %v; want %d deleted under the lock", result, err, old)
		}
		if n := strings.Count(s.Dump(t, OrgA), "collector_errors "); n != 3 {
			t.Fatalf("A keeps %d errors, want 3 (the cutoff's own instant is kept)", n)
		}
		if n := strings.Count(s.Dump(t, OrgB), "collector_errors "); n != 2 {
			t.Fatalf("B keeps %d errors, want 2", n)
		}
		if result, err := s.Collectors.Cleanup(ctx, cutoff); err != nil || !result.Held || result.Deleted != 0 {
			t.Fatalf("a second Cleanup = %+v, %v", result, err)
		}
	})
}

// A Cleanup whose context has ended fails, holding no lock, and deletes
// nothing: the next run deletes what it didn't.
func TestACleanupOnAnEndedContextFailsAndDeletesNothing(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		a := enroll(t, s, s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer), "edge-01", Now)
		cutoff := Now.Add(-collectors.ErrorRetention)
		insertErrors(t, s, a, 3, cutoff.Add(-time.Hour))

		ended, cancel := context.WithCancel(ctx)
		cancel()
		if result, err := s.Collectors.Cleanup(ended, cutoff); err == nil || result.Held || result.Deleted != 0 {
			t.Fatalf("a Cleanup on an ended context = %+v, %v; want an error, nothing deleted", result, err)
		}
		if n := strings.Count(s.Dump(t, OrgA), "collector_errors "); n != 3 {
			t.Fatalf("A keeps %d errors, want all 3", n)
		}
		if result, err := s.Collectors.Cleanup(ctx, cutoff); err != nil || result.Deleted != 3 {
			t.Fatalf("the next Cleanup = %+v, %v; want the 3 deleted", result, err)
		}
	})
}

// insertErrors inserts n error rows for c at at directly, past the store's
// trim: the cleanup's fixture.
func insertErrors(t *testing.T, s *Stores, c collectors.Collector, n int, at time.Time) {
	t.Helper()
	if _, err := s.Databases.Owner.Exec(ctx, `
		insert into opamp_server.collector_errors (id, organization_id, collector_instance_id, reported_at, message)
		select gen_random_uuid(), $1, $2, $3, 'old' from generate_series(1, $4)`,
		c.OrganizationID, c.ID, Millisecond(at), n); err != nil {
		t.Fatal(err)
	}
}

// Helpers for organization variables: variables, variable groups and their
// overrides (the variables store), the bindings (the fleet stores), the
// organization's counter, and a poll as the agent side makes it.

// defineVariable creates the organization's variable name = value.
func defineVariable(t *testing.T, s *Stores, org uuid.UUID, name, value string) variables.Variable {
	t.Helper()
	result, v, err := s.Variables.CreateVariable(ctx, variables.NewVariable{ID: uuid.New(), OrganizationID: org,
		Name: name, Value: value, At: Now})
	if err != nil || result != variables.Created {
		t.Fatalf("creating variable %s: %v, %v", name, result, err)
	}
	return v
}

// setValue changes the organization's variable's value.
func setValue(t *testing.T, s *Stores, org, id uuid.UUID, value string) {
	t.Helper()
	if _, err := s.Variables.UpdateVariable(ctx, org, id, variables.VariableChanges{Value: &value}, nil, Now); err != nil {
		t.Fatalf("changing a value: %v", err)
	}
}

// newVariableGroup creates the organization's variable group.
func newVariableGroup(t *testing.T, s *Stores, org uuid.UUID, name string) uuid.UUID {
	t.Helper()
	result, g, err := s.Variables.CreateGroup(ctx, variables.NewGroup{ID: uuid.New(), OrganizationID: org, Name: name, At: Now})
	if err != nil || result != variables.Created {
		t.Fatalf("creating variable group %s: %v, %v", name, result, err)
	}
	return g.ID
}

// setOverride gives the variable group's override of the variable value.
func setOverride(t *testing.T, s *Stores, org, variableGroup, variable uuid.UUID, value string) {
	t.Helper()
	if _, _, err := s.Variables.SetOverride(ctx, org, variableGroup, variable, value, nil, Now); err != nil {
		t.Fatalf("setting an override: %v", err)
	}
}

// resetOverride resets the variable group's override of the variable.
func resetOverride(t *testing.T, s *Stores, org, variableGroup, variable uuid.UUID) {
	t.Helper()
	if reset, err := s.Variables.ResetOverride(ctx, org, variableGroup, variable, nil, Now); err != nil || !reset {
		t.Fatalf("resetting an override: %v, %v", reset, err)
	}
}

// bindGroup sets (nil: clears) the fleet's group's variable group.
func bindGroup(t *testing.T, s *Stores, c fleets.Creation, group uuid.UUID, variableGroup *uuid.UUID) {
	t.Helper()
	change := collectorgroups.Clear[uuid.UUID]()
	if variableGroup != nil {
		change = collectorgroups.SetTo(*variableGroup)
	}
	if _, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, group, collectorgroups.Changes{VariableGroupID: change}, Now); err != nil {
		t.Fatalf("binding the group: %v", err)
	}
}

// pinVariableGroup pins (nil: clears) the collector's variable group.
func pinVariableGroup(t *testing.T, s *Stores, c collectors.Collector, variableGroup *uuid.UUID) collectors.Listed {
	t.Helper()
	l, err := s.Collectors.Update(ctx, c.OrganizationID, c.FleetID, c.ID,
		collectors.Changes{PinnedVariableGroup: &collectors.VariableGroupChange{ID: variableGroup}}, Now)
	if err != nil {
		t.Fatalf("pinning the variable group: %v", err)
	}
	return l
}

// assignGroup assigns the published version to the fleet's group.
func assignGroup(t *testing.T, s *Stores, c fleets.Creation, group, publication uuid.UUID) {
	t.Helper()
	if _, err := s.Groups.Update(ctx, OrgA, c.Fleet.ID, group,
		collectorgroups.Changes{AssignedPublicationID: collectorgroups.SetTo(publication)}, Now); err != nil {
		t.Fatalf("assigning: %v", err)
	}
}

// revision is the organization's counter, variables_revision.
func revision(t *testing.T, s *Stores, org uuid.UUID) int64 {
	t.Helper()
	var r int64
	if err := s.Databases.Owner.QueryRow(ctx, `select variables_revision from opamp_server.organizations where id = $1`,
		org).Scan(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

// stampOf is a nullable stamp's value, 0 for none.
func stampOf(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// poller polls as the agent side does (in short), on its own clock, a
// second on at every poll.
type poller struct {
	t  *testing.T
	s  *Stores
	at time.Time
}

func newPoller(t *testing.T, s *Stores) *poller {
	return &poller{t: t, s: s, at: Now}
}

// poll fills the collector's resolved published version in with what its
// bindings resolve (the variables store), computes the marker as the poll
// does (orgvariables.WantedStamp over the names filled in), and records
// what the agent side records: bytes it doesn't run pushed (Served with
// the marker, then the collector's applied report), the same bytes under
// another published version reconciled without a push, or the marker
// alone. It reports whether it pushed.
func (p *poller) poll(c collectors.Collector) bool {
	t, s := p.t, p.s
	t.Helper()
	l := get(t, s, c)
	if l.Resolution.From == collectors.FromNone {
		return false
	}
	ref := *l.Resolution.Ref()
	y, err := s.Collectors.YAML(ctx, c.OrganizationID, ref)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := s.Variables.Variables(ctx, c.OrganizationID, l.Resolution.GroupVariableGroupID, l.PinnedVariableGroupID)
	if err != nil {
		t.Fatal(err)
	}
	rendered, names := orgvariables.Render(y.YAML, resolved.Values)
	marker := orgvariables.WantedStamp(names, resolved, l.Resolution.GroupBindingStamp, stampOf(l.PinnedVariableGroupStamp))
	sha := sumOf(rendered)
	p.at = p.at.Add(time.Second)
	report := collectors.Report{OrganizationID: c.OrganizationID, FleetID: c.FleetID, CollectorID: c.ID}
	pushed := false
	switch {
	case l.ServedSHA256 == nil || *l.ServedSHA256 != sha:
		report.Served = &collectors.Served{Ref: ref, SHA256: sha, At: p.at, VariablesStamp: marker}
		pushed = true
	case l.Served == nil || *l.Served != ref:
		report.Reconciled = &collectors.Reconciled{Ref: ref, SHA256: sha, At: p.at, VariablesStamp: marker}
	case stampOf(l.ServedVariablesStamp) != marker:
		report.VariablesStamp = &marker
	}
	if _, err := s.Collectors.Report(ctx, report); err != nil {
		t.Fatal(err)
	}
	if pushed {
		if _, err := s.Collectors.Report(ctx, collectors.Report{OrganizationID: c.OrganizationID, FleetID: c.FleetID,
			CollectorID: c.ID, Effective: &collectors.Effective{SHA256: sha, Status: collectors.StatusApplied,
				At: p.at.Add(time.Millisecond)}}); err != nil {
			t.Fatal(err)
		}
	}
	return pushed
}

// agree lists the fleet's collectors, checks Get answers each as List does
// (its resolution and wanted stamp included), and checks SummariesForFleets
// counts the fleet's syncs as the grid's Sync() reads them; it answers
// each collector's sync.
func agree(t *testing.T, s *Stores, fleetID uuid.UUID, now time.Time) map[uuid.UUID]collectors.Sync {
	t.Helper()
	page, err := s.Collectors.List(ctx, OrgA, fleetID, collectors.ListOptions{Sort: collectors.SortName,
		Order: collectors.Ascending, Limit: collectors.MaxListLimit})
	if err != nil {
		t.Fatal(err)
	}
	syncs := map[uuid.UUID]collectors.Sync{}
	var grid collectors.Summary
	for _, l := range page.Collectors {
		got := get(t, s, l.Collector)
		if !reflect.DeepEqual(got.Resolution, l.Resolution) {
			t.Fatalf("%s: Get resolves %+v, List %+v", l.Name, got.Resolution, l.Resolution)
		}
		d := l.Sync()
		if got.Sync() != d {
			t.Fatalf("%s: Get's sync %s, List's %s", l.Name, got.Sync(), d)
		}
		syncs[l.ID] = d
		switch d {
		case collectors.InSync:
			grid.InSync++
		case collectors.Pending:
			grid.Pending++
		case collectors.Failed:
			grid.Failed++
		case collectors.Drifted:
			grid.Drifted++
		case collectors.Unassigned:
			grid.Unassigned++
		}
	}
	summaries, err := s.Collectors.SummariesForFleets(ctx, OrgA, []uuid.UUID{fleetID}, now)
	if err != nil {
		t.Fatal(err)
	}
	m := summaries[fleetID]
	if m.InSync != grid.InSync || m.Pending != grid.Pending || m.Failed != grid.Failed || m.Drifted != grid.Drifted ||
		m.Unassigned != grid.Unassigned || m.Collectors != len(page.Collectors) {
		t.Fatalf("the fleet's counts %+v disagree with the grid's syncs %+v", m, grid)
	}
	return syncs
}

// syncIs checks the collector's sync reads want, Get, List and the
// fleet's counts agreeing.
func syncIs(t *testing.T, s *Stores, c collectors.Collector, want collectors.Sync, label string) {
	t.Helper()
	if got := agree(t, s, c.FleetID, Now)[c.ID]; got != want {
		t.Fatalf("%s: %s reads %s, want %s", label, c.Name, got, want)
	}
}

// counting counts the statements a pool runs.
type counting struct {
	mu sync.Mutex
	n  int
}

func (c *counting) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return ctx
}

func (c *counting) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *counting) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// The poll answers the organization's counter, the collector's pinned
// variable group and its group's variable group with that setting's stamp
// (under a pin too), in its one statement; it leaves the wanted stamp 0
// (the poll computes the marker itself).
func TestPollAnswersTheRevisionAndBothBindingsInOneStatement(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		got := enroll(t, s, c, "edge-01", Now)
		defineVariable(t, s, OrgA, "REGION", "eu")
		assignGroup(t, s, c, c.DefaultGroupID, publish(t, s, OrgA, "a: ${org:REGION}\n"))
		groupLevel, pinLevel := newVariableGroup(t, s, OrgA, "group-level"), newVariableGroup(t, s, OrgA, "pin-level")
		bindGroup(t, s, c, c.DefaultGroupID, &groupLevel)
		groupStamp := revision(t, s, OrgA)
		pinVariableGroup(t, s, got, &pinLevel)
		if _, err := s.Collectors.Pin(ctx, OrgA, c.Fleet.ID, got.ID, &collectors.Ref{Source: collectors.SourcePublication,
			ID: publish(t, s, OrgA, "b: ${org:REGION}\n")}, Now); err != nil {
			t.Fatal(err)
		}
		statements := &counting{}
		config := s.Databases.Runtime.Config()
		config.ConnConfig.Tracer = statements
		pool, err := pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		store := collectors.NewPostgres(pool)
		for _, at := range []time.Time{Now, Now.Add(time.Second)} { // a heartbeat, then a coalesced one
			before := statements.count()
			r, err := store.Poll(ctx, collectors.PollRequest{OrganizationID: OrgA, FleetID: c.Fleet.ID,
				InstanceUID: got.InstanceUID, TokenID: c.RotateTokenID, At: at})
			if err != nil || !r.Found {
				t.Fatalf("Poll = %+v, %v", r, err)
			}
			if statements.count()-before != 1 {
				t.Fatalf("the poll ran %d statements, want 1", statements.count()-before)
			}
			if r.VariablesRevision != revision(t, s, OrgA) || r.VariablesRevision == 0 ||
				!sameVariableGroup(r.Collector.PinnedVariableGroupID, &pinLevel) || r.Collector.PinnedVariableGroupStamp == nil ||
				!sameVariableGroup(r.Resolution.GroupVariableGroupID, &groupLevel) || r.Resolution.GroupBindingStamp != groupStamp ||
				r.Resolution.From != collectors.FromPinnedPublication || r.Resolution.VariablesStamp != 0 {
				t.Fatalf("Poll answered revision %d (want %d), %+v, resolution %+v", r.VariablesRevision, revision(t, s, OrgA),
					r.Collector, r.Resolution)
			}
		}
		// An organization whose counter never moved answers 0.
		theirs := s.CreateFleet(t, OrgB, "globex", fleets.TypeServer)
		b := enroll(t, s, theirs, "globex-01", Now)
		if r, err := s.Collectors.Poll(ctx, collectors.PollRequest{OrganizationID: OrgB, FleetID: theirs.Fleet.ID,
			InstanceUID: b.InstanceUID, TokenID: theirs.RotateTokenID, At: Now}); err != nil || r.VariablesRevision != 0 ||
			r.Resolution.GroupVariableGroupID != nil {
			t.Fatalf("B's Poll = %+v, %v", r, err)
		}
	})
}

// The served-variables marker is written with a delivery and a reconcile,
// and alone by its own part, guarded like every part (an unchanged poll
// writes nothing); a stamp below 0, or the marker alone beside a delivery
// or a reconcile, is ErrInvalid.
func TestTheMarkerIsWrittenOnlyWhenItDiffers(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		got := enroll(t, s, c, "edge-01", Now)
		report := func(r collectors.Report) (bool, error) {
			r.OrganizationID, r.FleetID, r.CollectorID = OrgA, c.Fleet.ID, got.ID
			return s.Collectors.Report(ctx, r)
		}
		marker := func() *int64 { return get(t, s, got).ServedVariablesStamp }
		ref := collectors.Ref{Source: collectors.SourcePublication, ID: PubA}
		if written, err := report(collectors.Report{Served: &collectors.Served{Ref: ref, SHA256: pubSHA256, At: Now, VariablesStamp: 4}}); err != nil ||
			!written || stampOf(marker()) != 4 {
			t.Fatalf("a delivery with marker 4 = %v, %v; marker %v", written, err, marker())
		}
		for _, stamp := range []int64{4, 6, 6, 0} {
			want := stamp != stampOf(marker())
			wrote, err := report(collectors.Report{VariablesStamp: &stamp})
			if err != nil || wrote != want || stampOf(marker()) != stamp {
				t.Fatalf("the marker %d alone = %v, %v; want written %v, the marker %d", stamp, wrote, err, want, stampOf(marker()))
			}
		}
		reconcile := collectors.Reconciled{Ref: ref, SHA256: pubSHA256, At: Now, VariablesStamp: 9}
		if written, err := report(collectors.Report{Reconciled: &reconcile}); err != nil || !written || stampOf(marker()) != 9 {
			t.Fatalf("a reconcile moving the marker alone = %v, %v; marker %v", written, err, marker())
		}
		if written, err := report(collectors.Report{Reconciled: &reconcile}); err != nil || written {
			t.Fatalf("the same reconcile again = %v, %v", written, err)
		}
		before := s.Dump(t, OrgA)
		minus := int64(-1)
		for label, r := range map[string]collectors.Report{
			"a marker below 0":          {VariablesStamp: &minus},
			"a delivery's below 0":      {Served: &collectors.Served{Ref: ref, SHA256: pubSHA256, At: Now, VariablesStamp: -1}},
			"a reconcile's below 0":     {Reconciled: &collectors.Reconciled{Ref: ref, SHA256: pubSHA256, At: Now, VariablesStamp: -1}},
			"the marker and a delivery": {Served: &collectors.Served{Ref: ref, SHA256: pubSHA256, At: Now}, VariablesStamp: Ptr(int64(3))},
			"the marker and a reconcile": {Reconciled: &collectors.Reconciled{Ref: ref, SHA256: pubSHA256, At: Now},
				VariablesStamp: Ptr(int64(3))},
		} {
			if _, err := report(r); !errors.Is(err, collectors.ErrInvalid) {
				t.Errorf("%s: Report = %v, want ErrInvalid", label, err)
			}
		}
		s.Unchanged(t, "the refused markers", OrgA, before)
	})
}
