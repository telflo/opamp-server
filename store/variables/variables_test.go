package variables_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/telflo/opamp-server/store/apikeys"
	"github.com/telflo/opamp-server/store/collectorgroups"
	"github.com/telflo/opamp-server/store/collectors"
	"github.com/telflo/opamp-server/store/database"
	"github.com/telflo/opamp-server/store/fleets"
	"github.com/telflo/opamp-server/store/organizations"
	"github.com/telflo/opamp-server/store/orgvariables"
	"github.com/telflo/opamp-server/store/publications"
	. "github.com/telflo/opamp-server/store/storetest"
	"github.com/telflo/opamp-server/store/variables"
	"github.com/telflo/opamp-server/store/variableuse"
)

// This package's own test database (CONTRIBUTING.md, "Tests").
const testDatabase = "telflo_opamp_server_variables_test"

var ctx = context.Background()

// Bob is a second actor, for the changes that must move ChangedBy.
var Bob = &fleets.Actor{ID: uuid.MustParse("b0b0b0b0-0000-4000-8000-0000000000b0"), Email: "bob@acme.io"}

func newVariable(org uuid.UUID, name, value string) variables.NewVariable {
	return variables.NewVariable{ID: uuid.New(), OrganizationID: org, Name: name, Value: value, Actor: Ana, At: Now}
}

// create makes the organization's variable and fails the test unless it is
// Created.
func create(t *testing.T, s *Stores, org uuid.UUID, name, value string) variables.Variable {
	t.Helper()
	result, v, err := s.Variables.CreateVariable(ctx, newVariable(org, name, value))
	if err != nil || result != variables.Created {
		t.Fatalf("creating variable %s: %v, %v", name, result, err)
	}
	return v
}

func newGroup(org uuid.UUID, name string) variables.NewGroup {
	return variables.NewGroup{ID: uuid.New(), OrganizationID: org, Name: name, Actor: Ana, At: Now}
}

// group makes the organization's variable group and fails the test unless
// it is Created.
func group(t *testing.T, s *Stores, org uuid.UUID, name string) variables.Group {
	t.Helper()
	result, g, err := s.Variables.CreateGroup(ctx, newGroup(org, name))
	if err != nil || result != variables.Created {
		t.Fatalf("creating variable group %s: %v, %v", name, result, err)
	}
	return g
}

func setValue(t *testing.T, s *Stores, org, id uuid.UUID, value string) variables.Variable {
	t.Helper()
	v, err := s.Variables.UpdateVariable(ctx, org, id, variables.VariableChanges{Value: &value}, Ana, Now)
	if err != nil {
		t.Fatalf("changing a value: %v", err)
	}
	return v
}

func override(t *testing.T, s *Stores, org, groupID, variableID uuid.UUID, value string) bool {
	t.Helper()
	_, created, err := s.Variables.SetOverride(ctx, org, groupID, variableID, value, Ana, Now)
	if err != nil {
		t.Fatalf("setting an override: %v", err)
	}
	return created
}

func reset(t *testing.T, s *Stores, org, groupID, variableID uuid.UUID) bool {
	t.Helper()
	done, err := s.Variables.ResetOverride(ctx, org, groupID, variableID, Ana, Now)
	if err != nil {
		t.Fatalf("resetting an override: %v", err)
	}
	return done
}

func resolve(t *testing.T, s *Stores, org uuid.UUID, groupVG, pinVG *uuid.UUID) orgvariables.Resolved {
	t.Helper()
	r, err := s.Variables.Variables(ctx, org, groupVG, pinVG)
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	return r
}

// revision reads the organization's counter under the store.
func revision(t *testing.T, s *Stores, org uuid.UUID) int64 {
	t.Helper()
	var n int64
	if err := s.Databases.Owner.QueryRow(ctx, `
		select variables_revision from opamp_server.organizations where id = $1`, org).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// stampOf reads a variable's stamp under the store.
func stampOf(t *testing.T, s *Stores, org, id uuid.UUID) int64 {
	t.Helper()
	var n int64
	if err := s.Databases.Owner.QueryRow(ctx, `
		select stamp from opamp_server.variables where organization_id = $1 and id = $2`, org, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// overrideRow reads an override's row under the store: its value (nil for a
// reset row) and stamp, and whether there is one.
func overrideRow(t *testing.T, s *Stores, org, groupID, variableID uuid.UUID) (value *string, stamp int64, found bool) {
	t.Helper()
	err := s.Databases.Owner.QueryRow(ctx, `
		select value, stamp from opamp_server.variable_group_overrides
		 where organization_id = $1 and variable_group_id = $2 and variable_id = $3`,
		org, groupID, variableID).Scan(&value, &stamp)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return value, stamp, true
}

// overrideRows counts the variable group's override rows, reset rows
// included, under the store.
func overrideRows(t *testing.T, s *Stores, org uuid.UUID) int {
	t.Helper()
	n := 0
	if err := s.Databases.Owner.QueryRow(ctx, `
		select count(*) from opamp_server.variable_group_overrides where organization_id = $1`, org).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// insertVariables writes n variables of the organization straight into the
// table (V_1 … V_n, stamp 1), past the store: a fixture at a limit.
func insertVariables(t *testing.T, s *Stores, org uuid.UUID, n int) {
	t.Helper()
	if _, err := s.Databases.Owner.Exec(ctx, `
		insert into opamp_server.variables (id, organization_id, name, description, value,
		  changed_by_id, changed_by_email, changed_at, stamp)
		select gen_random_uuid(), $1, 'V_' || g, null, 'v', null, null, $2, 1
		  from generate_series(1, $3::int) g`, org, Now, n); err != nil {
		t.Fatal(err)
	}
}

// insertGroups writes n variable groups of the organization straight into
// the table, past the store.
func insertGroups(t *testing.T, s *Stores, org uuid.UUID, n int) {
	t.Helper()
	if _, err := s.Databases.Owner.Exec(ctx, `
		insert into opamp_server.variable_groups (id, organization_id, name, description,
		  changed_by_id, changed_by_email, changed_at)
		select gen_random_uuid(), $1, 'group ' || g, null, null, null, $2
		  from generate_series(1, $3::int) g`, org, Now, n); err != nil {
		t.Fatal(err)
	}
}

// bindGroup binds the organization's collector group to a variable group,
// past the store (the write is the fleet stores').
func bindGroup(t *testing.T, s *Stores, org, collectorGroupID, variableGroupID uuid.UUID) {
	t.Helper()
	if _, err := s.Databases.Runtime.Exec(ctx, `
		update opamp_server.collector_groups set variable_group_id = $3, variable_group_stamp = 1
		 where organization_id = $1 and id = $2`, org, collectorGroupID, variableGroupID); err != nil {
		t.Fatal(err)
	}
}

// publish publishes yaml as the organization's version id; the store
// writes the names it references.
func publish(t *testing.T, s *Stores, org, id uuid.UUID, yaml string) {
	t.Helper()
	if result, _, err := s.Publications.Put(ctx, publications.Publication{
		ID: id, OrganizationID: org, ConfigurationID: uuid.New(),
		ConfigurationName: "payments-gateway", VersionID: uuid.New(), VersionNumber: 3,
		YAML: yaml, SHA256: strings.Repeat("ab", 32), PublishedAt: Now, ReceivedAt: Now,
		Collector: publications.Collector{Distribution: "otelcol-contrib", Version: "0.162.0"},
	}); err != nil || result != publications.Created {
		t.Fatalf("publishing %s: %v, %v", id, result, err)
	}
}

// assign creates a collector group of the fleet assigned the published
// version.
func assign(t *testing.T, s *Stores, org, fleet, publication uuid.UUID, name string) collectorgroups.Group {
	t.Helper()
	g, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: org, FleetID: fleet, Name: name,
		AssignedPublicationID: Ptr(publication), At: Now})
	if err != nil {
		t.Fatalf("creating group %s: %v", name, err)
	}
	return g
}

// blockOn has the owner's session hold the organizations' rows `for no key
// update`, as a variable write does, and returns its release: once n
// sessions wait on a lock, it rolls back.
func blockOn(t *testing.T, s *Stores, n int, orgs ...uuid.UUID) (release func()) {
	t.Helper()
	blocker, err := s.Databases.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Rollback(ctx) })
	for _, org := range orgs {
		if _, err := blocker.Exec(ctx, `
			select 1 from opamp_server.organizations where id = $1 for no key update`, org); err != nil {
			t.Fatal(err)
		}
	}
	return func() {
		s.WaitForLockWaiters(t, n)
		if err := blocker.Rollback(ctx); err != nil {
			t.Error(err)
		}
	}
}

func same(a, b any) bool { return reflect.DeepEqual(a, b) }

// A create with the caller's id is safe to retry: the same id with the same
// content is Unchanged, whoever sends it when, and moves nothing; other
// content under that id, or another organization's id, is a Conflict.
// Variable groups the same.
func TestACreateWithItsOwnIdIsSafeToRetry(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		v := newVariable(OrgA, "OTLP_ENDPOINT", "http://otel:4318")
		v.Description = Ptr("where traces go")
		result, first, err := s.Variables.CreateVariable(ctx, v)
		at := Millisecond(Now)
		want := variables.Variable{ID: v.ID, OrganizationID: OrgA, Name: v.Name, Value: v.Value,
			Description: Ptr("where traces go"), ChangedBy: Ana, ChangedAt: at}
		if err != nil || result != variables.Created || !same(first, want) {
			t.Fatalf("CreateVariable = %v, %+v, %v; want created %+v", result, first, err, want)
		}
		before, beforeB := s.Dump(t, OrgA), s.Dump(t, OrgB)

		retry := v
		retry.Actor, retry.At = Bob, Now.Add(time.Hour)
		if result, again, err := s.Variables.CreateVariable(ctx, retry); err != nil || result != variables.Unchanged || !same(again, first) {
			t.Fatalf("a retry = %v, %+v, %v; want unchanged %+v", result, again, err, first)
		}
		for name, change := range map[string]func(*variables.NewVariable){
			"another name":        func(n *variables.NewVariable) { n.Name = "OTLP_EP" },
			"another value":       func(n *variables.NewVariable) { n.Value = "http://otel:4317" },
			"another description": func(n *variables.NewVariable) { n.Description = nil },
		} {
			other := v
			change(&other)
			if result, got, err := s.Variables.CreateVariable(ctx, other); err != nil || result != variables.Conflict || !same(got, variables.Variable{}) {
				t.Errorf("%s under the id = %v, %+v, %v; want a conflict, nothing answered", name, result, got, err)
			}
		}
		theirs := newVariable(OrgB, "ELSEWHERE", "x")
		theirs.ID = v.ID
		if result, got, err := s.Variables.CreateVariable(ctx, theirs); err != nil || result != variables.Conflict || !same(got, variables.Variable{}) {
			t.Fatalf("another organization's id = %v, %+v, %v; want a conflict", result, got, err)
		}
		s.Unchanged(t, "the retries", OrgA, before)
		s.Unchanged(t, "the retries", OrgB, beforeB)

		g := newGroup(OrgA, "eu-edge")
		g.Description = Ptr("Frankfurt")
		result, firstGroup, err := s.Variables.CreateGroup(ctx, g)
		if err != nil || result != variables.Created || firstGroup.ID != g.ID || firstGroup.Name != "eu-edge" ||
			*firstGroup.Description != "Frankfurt" || !same(firstGroup.ChangedBy, Ana) || !firstGroup.ChangedAt.Equal(at) {
			t.Fatalf("CreateGroup = %v, %+v, %v", result, firstGroup, err)
		}
		before = s.Dump(t, OrgA)
		retryGroup := g
		retryGroup.Actor, retryGroup.At = nil, Now.Add(time.Hour)
		if result, again, err := s.Variables.CreateGroup(ctx, retryGroup); err != nil || result != variables.Unchanged || !same(again, firstGroup) {
			t.Fatalf("a group's retry = %v, %+v, %v; want unchanged %+v", result, again, err, firstGroup)
		}
		otherGroup := g
		otherGroup.Description = Ptr("Paris")
		if result, got, err := s.Variables.CreateGroup(ctx, otherGroup); err != nil || result != variables.Conflict || !same(got, variables.Group{}) {
			t.Fatalf("a group's other content = %v, %+v, %v; want a conflict", result, got, err)
		}
		theirGroup := newGroup(OrgB, "elsewhere")
		theirGroup.ID = g.ID
		if result, _, err := s.Variables.CreateGroup(ctx, theirGroup); err != nil || result != variables.Conflict {
			t.Fatalf("another organization's group id = %v, %v; want a conflict", result, err)
		}
		s.Unchanged(t, "the groups' retries", OrgA, before)
		s.Unchanged(t, "the groups' retries", OrgB, beforeB)
	})
}

// Creates racing on one id or one name, another organization's create of
// that id among them: the id is Created once, the organization ends with one
// row of the name, and each other racer answers Unchanged, Conflict or the
// name taken; none raises the unique keys' 23505. On Postgres the racers are
// lined up behind both organizations' rows and let go together.
func TestRacingCreatesOfOneIdOrOneNameRaiseNo23505(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		id := uuid.New()
		racers := []variables.NewVariable{
			{ID: id, OrganizationID: OrgA, Name: "RACE", Value: "a", At: Now},
			{ID: id, OrganizationID: OrgA, Name: "RACE", Value: "a", At: Now},
			{ID: id, OrganizationID: OrgA, Name: "RACE", Value: "b", At: Now},
			{ID: uuid.New(), OrganizationID: OrgA, Name: "RACE", Value: "a", At: Now},
			{ID: id, OrganizationID: OrgB, Name: "RACE", Value: "a", At: Now},
		}
		ids := make([]uuid.UUID, len(racers))
		for i, v := range racers {
			ids[i] = v.ID
		}
		race(t, s, ids, variables.ErrNameTaken, func(i int) (variables.CreateResult, error) {
			result, _, err := s.Variables.CreateVariable(ctx, racers[i])
			return result, err
		})
		list, err := s.Variables.ListVariables(ctx, OrgA)
		if err != nil || len(list) != 1 || list[0].Name != "RACE" {
			t.Fatalf("A's variables after the race = %+v, %v; want one RACE", list, err)
		}

		groupID := uuid.New()
		groupRacers := []variables.NewGroup{
			{ID: groupID, OrganizationID: OrgA, Name: "race", At: Now},
			{ID: groupID, OrganizationID: OrgA, Name: "race", At: Now},
			{ID: groupID, OrganizationID: OrgA, Name: "race", Description: Ptr("other"), At: Now},
			{ID: uuid.New(), OrganizationID: OrgA, Name: "race", At: Now},
			{ID: groupID, OrganizationID: OrgB, Name: "race", At: Now},
		}
		for i, g := range groupRacers {
			ids[i] = g.ID
		}
		race(t, s, ids, variables.ErrGroupNameTaken, func(i int) (variables.CreateResult, error) {
			result, _, err := s.Variables.CreateGroup(ctx, groupRacers[i])
			return result, err
		})
		groups, err := s.Variables.ListGroups(ctx, OrgA)
		if err != nil || len(groups) != 1 || groups[0].Name != "race" {
			t.Fatalf("A's groups after the race = %+v, %v; want one race", groups, err)
		}
	})
}

// race runs one create per id at once (on Postgres, behind A's and B's
// rows): the first id is Created exactly once among the racers that name
// it, and every answer is Created, Unchanged, Conflict or nameTaken.
func race(t *testing.T, s *Stores, ids []uuid.UUID, nameTaken error, create func(i int) (variables.CreateResult, error)) {
	t.Helper()
	results := make([]variables.CreateResult, len(ids))
	errs := make([]error, len(ids))
	release := blockOn(t, s, len(ids), OrgA, OrgB)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = create(i)
		}()
	}
	release()
	wg.Wait()
	created := 0
	for i := range ids {
		switch {
		case errs[i] == nil && results[i] == variables.Created:
			if ids[i] == ids[0] {
				created++
			}
		case errs[i] == nil && (results[i] == variables.Unchanged || results[i] == variables.Conflict):
		case errors.Is(errs[i], nameTaken):
		default:
			t.Errorf("racer %d = %v, %v", i, results[i], errs[i])
		}
	}
	if created != 1 {
		t.Fatalf("the id was created %d times; want 1", created)
	}
}

// A name is unique within its organization, compared as written: case
// tells names apart, a create or a rename to a name taken is refused, and
// another organization's names are free. Variable groups the same.
func TestAVariablesNameIsUniqueInItsOrganizationAsWritten(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		create(t, s, OrgA, "PATH", "1")
		lower := create(t, s, OrgA, "Path", "2")
		before := s.Dump(t, OrgA)
		if _, _, err := s.Variables.CreateVariable(ctx, newVariable(OrgA, "PATH", "3")); !errors.Is(err, variables.ErrNameTaken) {
			t.Fatalf("a name taken = %v", err)
		}
		if _, err := s.Variables.UpdateVariable(ctx, OrgA, lower.ID, variables.VariableChanges{Name: Ptr("PATH")}, Ana, Now); !errors.Is(err, variables.ErrNameTaken) {
			t.Fatalf("a rename to a name taken = %v", err)
		}
		s.Unchanged(t, "the refusals", OrgA, before)
		create(t, s, OrgB, "PATH", "4")

		group(t, s, OrgA, "Edge")
		edge := group(t, s, OrgA, "edge")
		before = s.Dump(t, OrgA)
		if _, _, err := s.Variables.CreateGroup(ctx, newGroup(OrgA, "Edge")); !errors.Is(err, variables.ErrGroupNameTaken) {
			t.Fatalf("a group's name taken = %v", err)
		}
		if _, err := s.Variables.UpdateGroup(ctx, OrgA, edge.ID, variables.GroupChanges{Name: Ptr("Edge")}, Ana, Now); !errors.Is(err, variables.ErrGroupNameTaken) {
			t.Fatalf("a group's rename to a name taken = %v", err)
		}
		s.Unchanged(t, "the groups' refusals", OrgA, before)
		group(t, s, OrgB, "Edge")
	})
}

// limitRace has two creates race for the last place under a limit: exactly
// one is created and the other refused with limit.
func limitRace(t *testing.T, s *Stores, create func(i int) error, limit error) {
	t.Helper()
	errs := make([]error, 2)
	release := blockOn(t, s, 2, OrgA)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = create(i)
		}()
	}
	release()
	wg.Wait()
	if !(errs[0] == nil && errors.Is(errs[1], limit) || errs[1] == nil && errors.Is(errs[0], limit)) {
		t.Fatalf("the two racing for the last place = %v, %v; want one created, one refused", errs[0], errs[1])
	}
}

// The 151st variable is refused, counted under the organization's lock: two
// creates racing at 149 give exactly one. A retry of a variable already
// there still answers Unchanged, and the limit is the organization's own.
func TestTheHundredAndFiftyFirstVariableIsRefused(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		first := create(t, s, OrgA, "FIRST", "1")
		insertVariables(t, s, OrgA, variables.MaxVariables-2)
		limitRace(t, s, func(i int) error {
			_, _, err := s.Variables.CreateVariable(ctx, newVariable(OrgA, fmt.Sprintf("RACER_%d", i), "x"))
			return err
		}, variables.ErrLimitReached)
		list, err := s.Variables.ListVariables(ctx, OrgA)
		if err != nil || len(list) != variables.MaxVariables {
			t.Fatalf("listed %d, %v; want %d", len(list), err, variables.MaxVariables)
		}
		before := s.Dump(t, OrgA)
		if _, _, err := s.Variables.CreateVariable(ctx, newVariable(OrgA, "ONE_MORE", "x")); !errors.Is(err, variables.ErrLimitReached) {
			t.Fatalf("the 151st = %v", err)
		}
		retry := newVariable(OrgA, "FIRST", "1")
		retry.ID = first.ID
		if result, _, err := s.Variables.CreateVariable(ctx, retry); err != nil || result != variables.Unchanged {
			t.Fatalf("a retry at the limit = %v, %v; want unchanged", result, err)
		}
		s.Unchanged(t, "the refused 151st", OrgA, before)
		create(t, s, OrgB, "ONE_MORE", "x")

		if err := s.Variables.DeleteVariable(ctx, OrgA, first.ID); err != nil {
			t.Fatal(err)
		}
		create(t, s, OrgA, "ONE_MORE", "x")
	})
}

// The 101st variable group is refused, counted under the organization's
// lock: two creates racing at 99 give exactly one.
func TestTheHundredAndFirstGroupIsRefused(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		first := group(t, s, OrgA, "first")
		insertGroups(t, s, OrgA, variables.MaxGroups-2)
		limitRace(t, s, func(i int) error {
			_, _, err := s.Variables.CreateGroup(ctx, newGroup(OrgA, fmt.Sprintf("racer %d", i)))
			return err
		}, variables.ErrGroupLimitReached)
		list, err := s.Variables.ListGroups(ctx, OrgA)
		if err != nil || len(list) != variables.MaxGroups {
			t.Fatalf("listed %d, %v; want %d", len(list), err, variables.MaxGroups)
		}
		before := s.Dump(t, OrgA)
		if _, _, err := s.Variables.CreateGroup(ctx, newGroup(OrgA, "one more")); !errors.Is(err, variables.ErrGroupLimitReached) {
			t.Fatalf("the 101st = %v", err)
		}
		s.Unchanged(t, "the refused 101st", OrgA, before)
		group(t, s, OrgB, "one more")

		if err := s.Variables.DeleteGroup(ctx, OrgA, first.ID); err != nil {
			t.Fatal(err)
		}
		group(t, s, OrgA, "one more")
	})
}

// canary stands for configuration text that must never reach an error.
const canary = "CANARY_d20_task4"

// Every rule of a name, a value, a description, a group's name and an actor
// is refused with ErrInvalid naming the field, never quoting what was sent,
// and changes nothing; what the rules allow at their limits is kept.
func TestEveryValueRuleIsRefusedNamingTheFieldNeverTheValue(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		v := create(t, s, OrgA, "PRESENT", "x")
		g := group(t, s, OrgA, "present")
		before, beforeB := s.Dump(t, OrgA), s.Dump(t, OrgB)
		long := func(n int) string { return strings.Repeat("x", n-len(canary)) + canary }
		badActor := &fleets.Actor{ID: uuid.New(), Email: strings.Repeat("e", 321-len(canary)) + canary}

		for _, c := range []struct {
			name, field string
			call        func() error
		}{
			{"a name with a hyphen", "name", func() error {
				_, _, err := s.Variables.CreateVariable(ctx, newVariable(OrgA, canary+"-X", "x"))
				return err
			}},
			{"a name with a leading digit", "name", func() error {
				_, _, err := s.Variables.CreateVariable(ctx, newVariable(OrgA, "1"+canary, "x"))
				return err
			}},
			{"a name of 129 characters", "name", func() error {
				_, _, err := s.Variables.CreateVariable(ctx, newVariable(OrgA, long(129), "x"))
				return err
			}},
			{"an empty name", "name", func() error {
				_, _, err := s.Variables.CreateVariable(ctx, newVariable(OrgA, "", "x"))
				return err
			}},
			{"a value with a line feed", "value", func() error {
				_, _, err := s.Variables.CreateVariable(ctx, newVariable(OrgA, "NEW", canary+"\n"))
				return err
			}},
			{"a value with NUL", "value", func() error {
				_, _, err := s.Variables.CreateVariable(ctx, newVariable(OrgA, "NEW", canary+"\x00"))
				return err
			}},
			{"a value with DEL", "value", func() error {
				_, _, err := s.Variables.CreateVariable(ctx, newVariable(OrgA, "NEW", canary+"\x7f"))
				return err
			}},
			{"a name that isn't ASCII", "name", func() error {
				_, _, err := s.Variables.CreateVariable(ctx, newVariable(OrgA, "\u00c9"+canary, "x"))
				return err
			}},
			{"a value with a carriage return", "value", func() error {
				_, _, err := s.Variables.CreateVariable(ctx, newVariable(OrgA, "NEW", canary+"\r"))
				return err
			}},
			{"a value with a tab", "value", func() error {
				_, _, err := s.Variables.CreateVariable(ctx, newVariable(OrgA, "NEW", canary+"\t"))
				return err
			}},
			{"a value with U+0085", "value", func() error {
				_, _, err := s.Variables.CreateVariable(ctx, newVariable(OrgA, "NEW", canary+string(rune(0x85))))
				return err
			}},
			{"a value with U+2028", "value", func() error {
				_, _, err := s.Variables.CreateVariable(ctx, newVariable(OrgA, "NEW", canary+string(rune(0x2028))))
				return err
			}},
			{"a value with U+2029", "value", func() error {
				_, _, err := s.Variables.CreateVariable(ctx, newVariable(OrgA, "NEW", canary+string(rune(0x2029))))
				return err
			}},
			{"a value of 1,025 characters", "value", func() error {
				_, _, err := s.Variables.CreateVariable(ctx, newVariable(OrgA, "NEW", long(1025)))
				return err
			}},
			{"a description with a leading space", "description", func() error {
				n := newVariable(OrgA, "NEW", "x")
				n.Description = Ptr(" " + canary)
				_, _, err := s.Variables.CreateVariable(ctx, n)
				return err
			}},
			{"an empty description", "description", func() error {
				n := newVariable(OrgA, "NEW", "x")
				n.Description = Ptr("")
				_, _, err := s.Variables.CreateVariable(ctx, n)
				return err
			}},
			{"a description of 1,001 characters", "description", func() error {
				n := newVariable(OrgA, "NEW", "x")
				n.Description = Ptr(long(1001))
				_, _, err := s.Variables.CreateVariable(ctx, n)
				return err
			}},
			{"an actor's email of 321 characters", "actor", func() error {
				n := newVariable(OrgA, "NEW", "x")
				n.Actor = badActor
				_, _, err := s.Variables.CreateVariable(ctx, n)
				return err
			}},
			{"a rename to a bad name", "name", func() error {
				_, err := s.Variables.UpdateVariable(ctx, OrgA, v.ID, variables.VariableChanges{Name: Ptr(canary + " ")}, Ana, Now)
				return err
			}},
			{"a value changed to two lines", "value", func() error {
				_, err := s.Variables.UpdateVariable(ctx, OrgA, v.ID, variables.VariableChanges{Value: Ptr(canary + "\n2")}, Ana, Now)
				return err
			}},
			{"a description changed to a trailing newline", "description", func() error {
				_, err := s.Variables.UpdateVariable(ctx, OrgA, v.ID, variables.VariableChanges{
					Description: collectorgroups.SetTo(canary + "\n")}, Ana, Now)
				return err
			}},
			{"an update by a bad actor", "actor", func() error {
				_, err := s.Variables.UpdateVariable(ctx, OrgA, v.ID, variables.VariableChanges{Value: Ptr("y")}, badActor, Now)
				return err
			}},
			{"a group's blank name", "name", func() error {
				_, _, err := s.Variables.CreateGroup(ctx, newGroup(OrgA, " "))
				return err
			}},
			{"a group's name with a tab", "name", func() error {
				_, _, err := s.Variables.CreateGroup(ctx, newGroup(OrgA, canary+"\tx"))
				return err
			}},
			{"a group's name of 201 characters", "name", func() error {
				_, _, err := s.Variables.CreateGroup(ctx, newGroup(OrgA, long(201)))
				return err
			}},
			{"a group's description with a trailing space", "description", func() error {
				n := newGroup(OrgA, "new")
				n.Description = Ptr(canary + " ")
				_, _, err := s.Variables.CreateGroup(ctx, n)
				return err
			}},
			{"a group renamed with a trailing newline", "name", func() error {
				_, err := s.Variables.UpdateGroup(ctx, OrgA, g.ID, variables.GroupChanges{Name: Ptr(canary + "\n")}, Ana, Now)
				return err
			}},
			{"a group's description of 1,001 characters", "description", func() error {
				_, err := s.Variables.UpdateGroup(ctx, OrgA, g.ID, variables.GroupChanges{Description: collectorgroups.SetTo(long(1001))}, Ana, Now)
				return err
			}},
			{"an override with a line feed", "value", func() error {
				_, _, err := s.Variables.SetOverride(ctx, OrgA, g.ID, v.ID, canary+"\n", Ana, Now)
				return err
			}},
			{"an override of 1,025 characters", "value", func() error {
				_, _, err := s.Variables.SetOverride(ctx, OrgA, g.ID, v.ID, long(1025), Ana, Now)
				return err
			}},
			{"a Reset by a bad actor", "actor", func() error {
				_, err := s.Variables.ResetOverride(ctx, OrgA, g.ID, v.ID, badActor, Now)
				return err
			}},
		} {
			err := c.call()
			if !errors.Is(err, variables.ErrInvalid) {
				t.Errorf("%s = %v; want ErrInvalid", c.name, err)
				continue
			}
			if msg := err.Error(); !strings.HasSuffix(msg, ": "+c.field) || strings.Contains(msg, canary) || strings.Contains(msg, "CANARY") {
				t.Errorf("%s: the error %q must name %q and nothing sent", c.name, msg, c.field)
			}
		}
		s.Unchanged(t, "the refused writes", OrgA, before)
		s.Unchanged(t, "the refused writes", OrgB, beforeB)

		// The limits themselves are kept.
		atLimit := newVariable(OrgA, "A"+strings.Repeat("_b", 63)+"c", strings.Repeat("é", 1024))
		atLimit.Description = Ptr(strings.Repeat("d", 1000))
		if _, got, err := s.Variables.CreateVariable(ctx, atLimit); err != nil || len(got.Name) != 128 || got.Value != atLimit.Value {
			t.Fatalf("a variable at every limit = %v", err)
		}
		for _, value := range []string{"", "$x", "${env:X}", "a$b", "${org:OTHER}"} {
			if _, err := s.Variables.UpdateVariable(ctx, OrgA, v.ID, variables.VariableChanges{Value: Ptr(value)}, Ana, Now); err != nil {
				t.Fatalf("the value %q = %v", value, err)
			}
		}
		if _, _, err := s.Variables.CreateGroup(ctx, newGroup(OrgA, strings.Repeat("g", 200))); err != nil {
			t.Fatalf("a group's name of 200 characters = %v", err)
		}
	})
}

// A value's change moves the organization's counter on once and stamps that
// variable only; the other variables keep their stamps, another
// organization's counter doesn't move, and the same value again changes
// nothing.
func TestAValueChangeBumpsTheCounterOnceAndStampsOnlyThatVariable(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		one := create(t, s, OrgA, "ONE", "1")
		two := create(t, s, OrgA, "TWO", "2")
		create(t, s, OrgB, "ONE", "1")
		if n := revision(t, s, OrgA); n != 2 {
			t.Fatalf("two creates: the counter is %d; want 2", n)
		}
		setValue(t, s, OrgA, one.ID, "1b")
		if n := revision(t, s, OrgA); n != 3 {
			t.Fatalf("a value change: the counter is %d; want 3", n)
		}
		if a, b := stampOf(t, s, OrgA, one.ID), stampOf(t, s, OrgA, two.ID); a != 3 || b != 2 {
			t.Fatalf("the stamps are %d and %d; want 3 and 2", a, b)
		}
		r := resolve(t, s, OrgA, nil, nil)
		if r.Revision != 3 || !same(r.Values, map[string]string{"ONE": "1b", "TWO": "2"}) ||
			!same(r.Stamps, map[string]int64{"ONE": 3, "TWO": 2}) {
			t.Fatalf("resolved %+v", r)
		}
		if n := revision(t, s, OrgB); n != 1 {
			t.Fatalf("the other organization's counter is %d; want 1", n)
		}
		before := s.Dump(t, OrgA)
		setValue(t, s, OrgA, one.ID, "1b")
		s.Unchanged(t, "the same value again", OrgA, before)
	})
}

// A description's change, set or cleared, moves no counter and no stamp:
// nothing filled in changes. It still moves ChangedAt.
func TestADescriptionChangeStampsNothing(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		v := create(t, s, OrgA, "ONE", "1")
		later := Now.Add(time.Minute)
		got, err := s.Variables.UpdateVariable(ctx, OrgA, v.ID, variables.VariableChanges{Description: collectorgroups.SetTo("the first")}, Ana, later)
		if err != nil || *got.Description != "the first" || !got.ChangedAt.Equal(Millisecond(later)) {
			t.Fatalf("a description set = %+v, %v", got, err)
		}
		got, err = s.Variables.UpdateVariable(ctx, OrgA, v.ID, variables.VariableChanges{Description: collectorgroups.Clear[string]()}, Ana, later.Add(time.Minute))
		if err != nil || got.Description != nil {
			t.Fatalf("a description cleared = %+v, %v", got, err)
		}
		if n, stamp := revision(t, s, OrgA), stampOf(t, s, OrgA, v.ID); n != 1 || stamp != 1 {
			t.Fatalf("the counter is %d and the stamp %d; want 1 and 1", n, stamp)
		}
	})
}

// A create and a rename stamp the variable: the renamed name resolves with
// the rename's stamp, and the old name no longer resolves.
func TestCreateAndRenameStampTheVariable(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		create(t, s, OrgA, "OTHER", "o")
		v := create(t, s, OrgA, "OLD", "1")
		if stamp := stampOf(t, s, OrgA, v.ID); stamp != 2 {
			t.Fatalf("the create's stamp is %d; want 2", stamp)
		}
		renamed, err := s.Variables.UpdateVariable(ctx, OrgA, v.ID, variables.VariableChanges{Name: Ptr("NEW")}, Ana, Now)
		if err != nil || renamed.Name != "NEW" || renamed.Value != "1" {
			t.Fatalf("the rename = %+v, %v", renamed, err)
		}
		r := resolve(t, s, OrgA, nil, nil)
		if r.Revision != 3 || !same(r.Stamps, map[string]int64{"OTHER": 1, "NEW": 3}) || r.Values["NEW"] != "1" {
			t.Fatalf("resolved %+v", r)
		}
	})
}

// The fixed ids of the in-use fixture, so its order is known: the
// published versions are listed by id.
var (
	usedFirst  = uuid.MustParse("11111111-0000-4000-8000-000000000001")
	usedSecond = uuid.MustParse("22222222-0000-4000-8000-000000000002")
)

// A variable's rename or delete is refused while a published version that a
// collector group or a collector uses references its name, with what uses
// it: at most 100 entries, the totals exact. Nothing changes; once nothing
// uses those versions, both pass.
func TestARenameOrDeleteIsRefusedWhileAUsedVersionReferencesTheName(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		v := create(t, s, OrgA, "OTLP_EP", "http://otel:4318")
		free := create(t, s, OrgA, "FREE", "f")
		publish(t, s, OrgA, usedFirst, "exporters:\n  otlp:\n    endpoint: ${org:OTLP_EP}\n")
		publish(t, s, OrgA, usedSecond, "a: ${org:OTLP_EP}\nb: ${org:OTLP_EP}\n")
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		for i := range variableuse.MaxListed + 1 {
			assign(t, s, OrgA, c.Fleet.ID, usedFirst, fmt.Sprintf("group %03d", i))
		}
		for i, pin := range []uuid.UUID{usedFirst, usedSecond} {
			s.InsertCollector(t, Collector{ID: uuid.New(), OrganizationID: OrgA, FleetID: c.Fleet.ID, GroupID: c.DefaultGroupID,
				Name: fmt.Sprintf("edge-%d", i), PinnedPublicationID: Ptr(pin), LastSeenAt: Now})
		}
		before := s.Dump(t, OrgA)

		check := func(call string, err error) {
			t.Helper()
			var inUse *variables.InUseError
			if !errors.As(err, &inUse) || !errors.Is(err, variables.ErrInUse) {
				t.Fatalf("%s = %v; want *InUseError", call, err)
			}
			u := inUse.Uses
			if u.Publications != 2 || u.Groups != variableuse.MaxListed+1 || u.PinnedCollectors != 2 ||
				len(u.List) != 1 || u.List[0].PublicationID != usedFirst || len(u.List[0].Groups) != variableuse.MaxListed ||
				u.List[0].PinnedCollectors != 1 {
				t.Fatalf("%s: the uses are %d publications, %d groups, %d pinned, %d listed", call,
					u.Publications, u.Groups, u.PinnedCollectors, len(u.List))
			}
			if !slices.IsSortedFunc(u.List[0].Groups, func(a, b variableuse.GroupRef) int {
				return bytes.Compare(a.GroupID[:], b.GroupID[:])
			}) {
				t.Fatalf("%s: the groups aren't in order", call)
			}
			if strings.Contains(err.Error(), "OTLP_EP") {
				t.Fatalf("%s: the error %q quotes the name", call, err)
			}
		}
		_, err := s.Variables.UpdateVariable(ctx, OrgA, v.ID, variables.VariableChanges{Name: Ptr("OTLP_ENDPOINT")}, Ana, Now)
		check("the rename", err)
		_, err = s.Variables.UpdateVariable(ctx, OrgA, v.ID, variables.VariableChanges{Name: Ptr("OTLP_ENDPOINT"), Value: Ptr("x")}, Ana, Now)
		check("a rename with a value change", err)
		check("the delete", s.Variables.DeleteVariable(ctx, OrgA, v.ID))
		s.Unchanged(t, "the refused rename and delete", OrgA, before)

		// A variable no used version references is free.
		if _, err := s.Variables.UpdateVariable(ctx, OrgA, free.ID, variables.VariableChanges{Name: Ptr("FREED")}, Ana, Now); err != nil {
			t.Fatalf("an unused variable's rename = %v", err)
		}
		// Once nothing uses the versions, the name is free.
		if _, err := s.Fleets.Delete(ctx, OrgA, c.Fleet.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Variables.UpdateVariable(ctx, OrgA, v.ID, variables.VariableChanges{Name: Ptr("OTLP_ENDPOINT")}, Ana, Now); err != nil {
			t.Fatalf("the rename once unused = %v", err)
		}
		if err := s.Variables.DeleteVariable(ctx, OrgA, v.ID); err != nil {
			t.Fatalf("the delete once unused = %v", err)
		}
	})
}

// A published version no collector group is assigned and no collector is
// pinned to holds no name: the variable's rename and delete pass.
func TestAnUnassignedVersionDoesntHoldAName(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		v := create(t, s, OrgA, "OTLP_EP", "x")
		publish(t, s, OrgA, uuid.New(), "endpoint: ${org:OTLP_EP}\n")
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		if _, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID, Name: "unassigned", At: Now}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Variables.UpdateVariable(ctx, OrgA, v.ID, variables.VariableChanges{Name: Ptr("RENAMED")}, Ana, Now); err != nil {
			t.Fatalf("the rename = %v", err)
		}
		publish(t, s, OrgA, uuid.New(), "endpoint: ${org:RENAMED}\n")
		if err := s.Variables.DeleteVariable(ctx, OrgA, v.ID); err != nil {
			t.Fatalf("the delete = %v", err)
		}
	})
}

// A value change is never refused, even while its name is in use: it is
// stamped as any value change.
func TestAValueChangeIsNeverRefused(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		v := create(t, s, OrgA, "OTLP_EP", "x")
		pub := uuid.New()
		publish(t, s, OrgA, pub, "endpoint: ${org:OTLP_EP}\n")
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		assign(t, s, OrgA, c.Fleet.ID, pub, "gateways")
		got, err := s.Variables.UpdateVariable(ctx, OrgA, v.ID, variables.VariableChanges{
			Value: Ptr("y"), Description: collectorgroups.SetTo("in use")}, Ana, Now)
		if err != nil || got.Value != "y" || stampOf(t, s, OrgA, v.ID) != 2 {
			t.Fatalf("a value change in use = %+v, %v", got, err)
		}
	})
}

// Deleting a variable deletes its overrides in every variable group, reset
// rows included.
func TestDeletingAVariableDeletesItsOverrides(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		x := create(t, s, OrgA, "X", "x")
		y := create(t, s, OrgA, "Y", "y")
		g1, g2 := group(t, s, OrgA, "one"), group(t, s, OrgA, "two")
		override(t, s, OrgA, g1.ID, x.ID, "x1")
		override(t, s, OrgA, g2.ID, x.ID, "x2")
		override(t, s, OrgA, g1.ID, y.ID, "y1")
		override(t, s, OrgA, g2.ID, y.ID, "y2")
		reset(t, s, OrgA, g2.ID, y.ID)
		if err := s.Variables.DeleteVariable(ctx, OrgA, x.ID); err != nil {
			t.Fatal(err)
		}
		got, overrides, err := s.Variables.GetGroup(ctx, OrgA, g1.ID)
		if err != nil || got.OverrideCount != 1 || len(overrides) != 1 || overrides[0].VariableID != y.ID {
			t.Fatalf("group one after X's delete = %+v, %+v, %v", got, overrides, err)
		}
		if n := overrideRows(t, s, OrgA); n != 2 {
			t.Fatalf("%d override rows; want Y's two", n)
		}
		if err := s.Variables.DeleteVariable(ctx, OrgA, y.ID); err != nil {
			t.Fatal(err)
		}
		if n := overrideRows(t, s, OrgA); n != 0 {
			t.Fatalf("%d override rows; want none, the reset row too", n)
		}
	})
}

// A variable group bound to a collector group or pinned on a collector isn't
// deleted: the details list at most 100 collector groups by fleet and then
// group, with the exact total and the pinned count. Nothing changes; once
// nothing uses it, it is deleted with its overrides.
func TestAVariableGroupBoundToAGroupOrPinnedOnACollectorIsntDeleted(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		x := create(t, s, OrgA, "X", "x")
		vg := group(t, s, OrgA, "eu-edge")
		override(t, s, OrgA, vg.ID, x.ID, "eu")
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		s.InsertCollector(t, Collector{ID: uuid.New(), OrganizationID: OrgA, FleetID: c.Fleet.ID, GroupID: c.DefaultGroupID,
			Name: "edge-0", PinnedVariableGroupID: Ptr(vg.ID), LastSeenAt: Now})
		before := s.Dump(t, OrgA)
		err := s.Variables.DeleteGroup(ctx, OrgA, vg.ID)
		var inUse *variables.GroupInUseError
		if !errors.As(err, &inUse) || !errors.Is(err, variables.ErrGroupInUse) ||
			inUse.Use.TotalCollectorGroups != 0 || inUse.Use.PinnedCollectors != 1 || len(inUse.Use.CollectorGroups) != 0 {
			t.Fatalf("pinned only = %v", err)
		}
		s.Unchanged(t, "the refused delete", OrgA, before)

		other := s.CreateFleet(t, OrgA, "edge-dev", fleets.TypeServer)
		var bound []variableuse.GroupRef
		for i := range variableuse.MaxListed + 1 {
			fleet := c.Fleet.ID
			if i%2 == 1 {
				fleet = other.Fleet.ID
			}
			g, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: fleet, Name: fmt.Sprintf("group %03d", i), At: Now})
			if err != nil {
				t.Fatal(err)
			}
			bindGroup(t, s, OrgA, g.ID, vg.ID)
			bound = append(bound, variableuse.GroupRef{FleetID: fleet, GroupID: g.ID})
		}
		s.InsertCollector(t, Collector{ID: uuid.New(), OrganizationID: OrgA, FleetID: other.Fleet.ID, GroupID: other.DefaultGroupID,
			Name: "edge-1", PinnedVariableGroupID: Ptr(vg.ID), LastSeenAt: Now})
		slices.SortFunc(bound, func(a, b variableuse.GroupRef) int {
			if c := bytes.Compare(a.FleetID[:], b.FleetID[:]); c != 0 {
				return c
			}
			return bytes.Compare(a.GroupID[:], b.GroupID[:])
		})
		listed, _, err := s.Variables.GetGroup(ctx, OrgA, vg.ID)
		if err != nil || listed.Usage != (variables.Usage{CollectorGroups: variableuse.MaxListed + 1, PinnedCollectors: 2}) {
			t.Fatalf("the group's usage = %+v, %v", listed.Usage, err)
		}
		before = s.Dump(t, OrgA)
		err = s.Variables.DeleteGroup(ctx, OrgA, vg.ID)
		if !errors.As(err, &inUse) || inUse.Use.TotalCollectorGroups != variableuse.MaxListed+1 || inUse.Use.PinnedCollectors != 2 ||
			!same(inUse.Use.CollectorGroups, bound[:variableuse.MaxListed]) {
			t.Fatalf("bound and pinned = %v", err)
		}
		if strings.Contains(err.Error(), "eu-edge") {
			t.Fatalf("the error %q quotes the name", err)
		}
		s.Unchanged(t, "the refused delete", OrgA, before)

		for _, fleet := range []uuid.UUID{c.Fleet.ID, other.Fleet.ID} {
			if _, err := s.Fleets.Delete(ctx, OrgA, fleet); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Variables.DeleteGroup(ctx, OrgA, vg.ID); err != nil {
			t.Fatalf("the delete once unused = %v", err)
		}
		if n := overrideRows(t, s, OrgA); n != 0 {
			t.Fatalf("%d override rows left", n)
		}
		if _, _, err := s.Variables.GetGroup(ctx, OrgA, vg.ID); !errors.Is(err, variables.ErrGroupNotFound) {
			t.Fatalf("GetGroup after the delete = %v", err)
		}
	})
}

// An override's set, change and Reset each move the counter on once and
// stamp the override's row: the Reset keeps the row with no value and its
// stamp. The variable group has no stamp; its ChangedBy and ChangedAt move
// with each.
func TestAnOverrideIsStampedWhenSetChangedAndReset(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		x := create(t, s, OrgA, "X", "org")
		g := group(t, s, OrgA, "eu-edge")
		for i, step := range []struct {
			name       string
			do         func(at time.Time, actor *fleets.Actor)
			wantValue  *string
			wantStamp  int64
			wantFilled string
		}{
			{"set", func(at time.Time, actor *fleets.Actor) {
				if _, created, err := s.Variables.SetOverride(ctx, OrgA, g.ID, x.ID, "a", actor, at); err != nil || !created {
					t.Fatalf("set = %v, %v", created, err)
				}
			}, Ptr("a"), 2, "a"},
			{"changed", func(at time.Time, actor *fleets.Actor) {
				if _, created, err := s.Variables.SetOverride(ctx, OrgA, g.ID, x.ID, "b", actor, at); err != nil || created {
					t.Fatalf("changed = %v, %v", created, err)
				}
			}, Ptr("b"), 3, "b"},
			{"reset", func(at time.Time, actor *fleets.Actor) {
				if done, err := s.Variables.ResetOverride(ctx, OrgA, g.ID, x.ID, actor, at); err != nil || !done {
					t.Fatalf("reset = %v, %v", done, err)
				}
			}, nil, 4, "org"},
		} {
			at, actor := Now.Add(time.Duration(i+1)*time.Minute), []*fleets.Actor{Bob, Ana, Bob}[i]
			step.do(at, actor)
			if n := revision(t, s, OrgA); n != step.wantStamp {
				t.Fatalf("%s: the counter is %d; want %d", step.name, n, step.wantStamp)
			}
			value, stamp, found := overrideRow(t, s, OrgA, g.ID, x.ID)
			if !found || !same(value, step.wantValue) || stamp != step.wantStamp {
				t.Fatalf("%s: the row is %v, %v, %d", step.name, found, value, stamp)
			}
			r := resolve(t, s, OrgA, &g.ID, nil)
			if r.Values["X"] != step.wantFilled || r.Stamps["X"] != step.wantStamp {
				t.Fatalf("%s: resolved %+v", step.name, r)
			}
			if stampOf(t, s, OrgA, x.ID) != 1 {
				t.Fatalf("%s: the variable's own stamp moved", step.name)
			}
			got, _, err := s.Variables.GetGroup(ctx, OrgA, g.ID)
			if err != nil || !same(got.ChangedBy, actor) || !got.ChangedAt.Equal(Millisecond(at)) {
				t.Fatalf("%s: the group's changed by and at = %+v, %v, %v", step.name, got.ChangedBy, got.ChangedAt, err)
			}
		}
	})
}

// A reset row is ignored when filling in, never counted or answered, and a
// later override of that variable reuses it, answering created.
func TestAResetRowIsIgnoredUncountedAndReused(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		x := create(t, s, OrgA, "X", "org")
		y := create(t, s, OrgA, "Y", "y")
		g := group(t, s, OrgA, "eu-edge")
		override(t, s, OrgA, g.ID, x.ID, "a")
		override(t, s, OrgA, g.ID, y.ID, "b")
		reset(t, s, OrgA, g.ID, x.ID)

		got, overrides, err := s.Variables.GetGroup(ctx, OrgA, g.ID)
		if err != nil || got.OverrideCount != 1 || len(overrides) != 1 || overrides[0].VariableID != y.ID {
			t.Fatalf("GetGroup = %+v, %+v, %v", got, overrides, err)
		}
		list, err := s.Variables.ListGroups(ctx, OrgA)
		if err != nil || len(list) != 1 || list[0].OverrideCount != 1 {
			t.Fatalf("ListGroups = %+v, %v", list, err)
		}
		if r := resolve(t, s, OrgA, &g.ID, nil); r.Values["X"] != "org" || r.Values["Y"] != "b" {
			t.Fatalf("resolved %+v", r)
		}

		o, created, err := s.Variables.SetOverride(ctx, OrgA, g.ID, x.ID, "c", Bob, Now.Add(time.Hour))
		if err != nil || !created || o.Value != "c" || o.VariableID != x.ID || !same(o.ChangedBy, Bob) {
			t.Fatalf("the reset row reused = %+v, %v, %v", o, created, err)
		}
		if n := overrideRows(t, s, OrgA); n != 2 {
			t.Fatalf("%d override rows; want 2", n)
		}
		if got, _, _ := s.Variables.GetGroup(ctx, OrgA, g.ID); got.OverrideCount != 2 {
			t.Fatalf("OverrideCount = %d; want 2", got.OverrideCount)
		}
	})
}

// The same value again changes nothing: no bump, neither the override's nor
// its group's ChangedBy and ChangedAt move, and it answers the override as
// it was, not created.
func TestSettingTheSameValueAgainChangesNothing(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		x := create(t, s, OrgA, "X", "org")
		g := group(t, s, OrgA, "eu-edge")
		first, _, err := s.Variables.SetOverride(ctx, OrgA, g.ID, x.ID, "a", Ana, Now)
		if err != nil {
			t.Fatal(err)
		}
		before := s.Dump(t, OrgA)
		again, created, err := s.Variables.SetOverride(ctx, OrgA, g.ID, x.ID, "a", Bob, Now.Add(time.Hour))
		if err != nil || created || !same(again, first) {
			t.Fatalf("the same value again = %+v, %v, %v; want %+v, not created", again, created, err, first)
		}
		s.Unchanged(t, "the same value again", OrgA, before)
	})
}

// Every write that fails any of its checks changes nothing (its transaction
// rolls back): each method, each check in its order.
func TestAWriteThatFailsAnyCheckChangesNothing(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		used := create(t, s, OrgA, "USED", "u")
		free := create(t, s, OrgA, "FREE", "f")
		g := group(t, s, OrgA, "eu-edge")
		bound := group(t, s, OrgA, "bound")
		override(t, s, OrgA, g.ID, used.ID, "o")
		override(t, s, OrgA, g.ID, free.ID, "r")
		reset(t, s, OrgA, g.ID, free.ID)
		pub := uuid.New()
		publish(t, s, OrgA, pub, "x: ${org:USED}\n")
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		cg := assign(t, s, OrgA, c.Fleet.ID, pub, "gateways")
		bindGroup(t, s, OrgA, cg.ID, bound.ID)
		theirs := create(t, s, OrgB, "THEIRS", "t")
		theirGroup := group(t, s, OrgB, "theirs")

		retry := newVariable(OrgA, "USED", "u")
		retry.ID = used.ID
		retryGroup := newGroup(OrgA, "eu-edge")
		retryGroup.ID = g.ID
		cases := []struct {
			name string
			call func() error
			want error
		}{
			{"CreateVariable: a field", func() error {
				_, _, err := s.Variables.CreateVariable(ctx, newVariable(OrgA, "BAD-NAME", "x"))
				return err
			}, variables.ErrInvalid},
			{"CreateVariable: the organization", func() error {
				_, _, err := s.Variables.CreateVariable(ctx, newVariable(OrgUnregistered, "NEW", "x"))
				return err
			}, variables.ErrOrganizationNotFound},
			{"CreateVariable: the id, the same content", func() error {
				if result, _, err := s.Variables.CreateVariable(ctx, retry); err != nil || result != variables.Unchanged {
					return fmt.Errorf("%v, %v", result, err)
				}
				return nil
			}, nil},
			{"CreateVariable: the id, other content", func() error {
				other := retry
				other.Value = "other"
				if result, _, err := s.Variables.CreateVariable(ctx, other); err != nil || result != variables.Conflict {
					return fmt.Errorf("%v, %v", result, err)
				}
				return nil
			}, nil},
			{"CreateVariable: another organization's id", func() error {
				other := newVariable(OrgA, "NEW", "x")
				other.ID = theirs.ID
				if result, _, err := s.Variables.CreateVariable(ctx, other); err != nil || result != variables.Conflict {
					return fmt.Errorf("%v, %v", result, err)
				}
				return nil
			}, nil},
			{"CreateVariable: the name taken", func() error {
				_, _, err := s.Variables.CreateVariable(ctx, newVariable(OrgA, "USED", "x"))
				return err
			}, variables.ErrNameTaken},
			{"UpdateVariable: the changes", func() error {
				_, err := s.Variables.UpdateVariable(ctx, OrgA, used.ID, variables.VariableChanges{Value: Ptr("a\nb")}, Ana, Now)
				return err
			}, variables.ErrInvalid},
			{"UpdateVariable: the organization", func() error {
				_, err := s.Variables.UpdateVariable(ctx, OrgUnregistered, used.ID, variables.VariableChanges{Value: Ptr("y")}, Ana, Now)
				return err
			}, variables.ErrOrganizationNotFound},
			{"UpdateVariable: the variable", func() error {
				_, err := s.Variables.UpdateVariable(ctx, OrgA, theirs.ID, variables.VariableChanges{Value: Ptr("y")}, Ana, Now)
				return err
			}, variables.ErrNotFound},
			{"UpdateVariable: a new name taken", func() error {
				_, err := s.Variables.UpdateVariable(ctx, OrgA, free.ID, variables.VariableChanges{Name: Ptr("USED"), Value: Ptr("y")}, Ana, Now)
				return err
			}, variables.ErrNameTaken},
			{"UpdateVariable: a rename in use", func() error {
				_, err := s.Variables.UpdateVariable(ctx, OrgA, used.ID, variables.VariableChanges{Name: Ptr("RENAMED"), Value: Ptr("y")}, Ana, Now)
				return err
			}, variables.ErrInUse},
			{"UpdateVariable: nothing differing", func() error {
				_, err := s.Variables.UpdateVariable(ctx, OrgA, used.ID, variables.VariableChanges{Name: Ptr("USED"), Value: Ptr("u"),
					Description: collectorgroups.Clear[string]()}, Bob, Now.Add(time.Hour))
				return err
			}, nil},
			{"DeleteVariable: the organization", func() error {
				return s.Variables.DeleteVariable(ctx, OrgUnregistered, free.ID)
			}, variables.ErrOrganizationNotFound},
			{"DeleteVariable: the variable", func() error {
				return s.Variables.DeleteVariable(ctx, OrgA, theirs.ID)
			}, variables.ErrNotFound},
			{"DeleteVariable: in use", func() error {
				return s.Variables.DeleteVariable(ctx, OrgA, used.ID)
			}, variables.ErrInUse},
			{"CreateGroup: a field", func() error {
				_, _, err := s.Variables.CreateGroup(ctx, newGroup(OrgA, " bad"))
				return err
			}, variables.ErrInvalid},
			{"CreateGroup: the organization", func() error {
				_, _, err := s.Variables.CreateGroup(ctx, newGroup(OrgUnregistered, "new"))
				return err
			}, variables.ErrOrganizationNotFound},
			{"CreateGroup: the id, the same content", func() error {
				if result, _, err := s.Variables.CreateGroup(ctx, retryGroup); err != nil || result != variables.Unchanged {
					return fmt.Errorf("%v, %v", result, err)
				}
				return nil
			}, nil},
			{"CreateGroup: the id, other content", func() error {
				other := retryGroup
				other.Name = "other"
				if result, _, err := s.Variables.CreateGroup(ctx, other); err != nil || result != variables.Conflict {
					return fmt.Errorf("%v, %v", result, err)
				}
				return nil
			}, nil},
			{"CreateGroup: another organization's id", func() error {
				other := newGroup(OrgA, "new")
				other.ID = theirGroup.ID
				if result, _, err := s.Variables.CreateGroup(ctx, other); err != nil || result != variables.Conflict {
					return fmt.Errorf("%v, %v", result, err)
				}
				return nil
			}, nil},
			{"CreateGroup: the name taken", func() error {
				_, _, err := s.Variables.CreateGroup(ctx, newGroup(OrgA, "bound"))
				return err
			}, variables.ErrGroupNameTaken},
			{"UpdateGroup: the changes", func() error {
				_, err := s.Variables.UpdateGroup(ctx, OrgA, g.ID, variables.GroupChanges{Name: Ptr("")}, Ana, Now)
				return err
			}, variables.ErrInvalid},
			{"UpdateGroup: the organization", func() error {
				_, err := s.Variables.UpdateGroup(ctx, OrgUnregistered, g.ID, variables.GroupChanges{Name: Ptr("x")}, Ana, Now)
				return err
			}, variables.ErrOrganizationNotFound},
			{"UpdateGroup: the group", func() error {
				_, err := s.Variables.UpdateGroup(ctx, OrgA, theirGroup.ID, variables.GroupChanges{Name: Ptr("x")}, Ana, Now)
				return err
			}, variables.ErrGroupNotFound},
			{"UpdateGroup: a new name taken", func() error {
				_, err := s.Variables.UpdateGroup(ctx, OrgA, g.ID, variables.GroupChanges{Name: Ptr("bound"),
					Description: collectorgroups.SetTo("d")}, Ana, Now)
				return err
			}, variables.ErrGroupNameTaken},
			{"UpdateGroup: nothing differing", func() error {
				_, err := s.Variables.UpdateGroup(ctx, OrgA, g.ID, variables.GroupChanges{Name: Ptr("eu-edge")}, Bob, Now.Add(time.Hour))
				return err
			}, nil},
			{"DeleteGroup: the organization", func() error {
				return s.Variables.DeleteGroup(ctx, OrgUnregistered, g.ID)
			}, variables.ErrOrganizationNotFound},
			{"DeleteGroup: the group", func() error {
				return s.Variables.DeleteGroup(ctx, OrgA, theirGroup.ID)
			}, variables.ErrGroupNotFound},
			{"DeleteGroup: in use", func() error {
				return s.Variables.DeleteGroup(ctx, OrgA, bound.ID)
			}, variables.ErrGroupInUse},
			{"SetOverride: the value", func() error {
				_, _, err := s.Variables.SetOverride(ctx, OrgA, g.ID, used.ID, "a\x7fb", Ana, Now)
				return err
			}, variables.ErrInvalid},
			{"SetOverride: the organization", func() error {
				_, _, err := s.Variables.SetOverride(ctx, OrgUnregistered, g.ID, used.ID, "y", Ana, Now)
				return err
			}, variables.ErrOrganizationNotFound},
			{"SetOverride: the group", func() error {
				_, _, err := s.Variables.SetOverride(ctx, OrgA, theirGroup.ID, used.ID, "y", Ana, Now)
				return err
			}, variables.ErrGroupNotFound},
			{"SetOverride: the variable", func() error {
				_, _, err := s.Variables.SetOverride(ctx, OrgA, g.ID, theirs.ID, "y", Ana, Now)
				return err
			}, variables.ErrNotFound},
			{"SetOverride: the same value", func() error {
				_, created, err := s.Variables.SetOverride(ctx, OrgA, g.ID, used.ID, "o", Bob, Now.Add(time.Hour))
				if created {
					return errors.New("created")
				}
				return err
			}, nil},
			{"ResetOverride: the organization", func() error {
				_, err := s.Variables.ResetOverride(ctx, OrgUnregistered, g.ID, used.ID, Ana, Now)
				return err
			}, variables.ErrOrganizationNotFound},
			{"ResetOverride: the group", func() error {
				_, err := s.Variables.ResetOverride(ctx, OrgA, theirGroup.ID, used.ID, Ana, Now)
				return err
			}, variables.ErrGroupNotFound},
			{"ResetOverride: the variable", func() error {
				_, err := s.Variables.ResetOverride(ctx, OrgA, g.ID, theirs.ID, Ana, Now)
				return err
			}, variables.ErrNotFound},
			{"ResetOverride: no row", func() error {
				done, err := s.Variables.ResetOverride(ctx, OrgA, bound.ID, used.ID, Ana, Now)
				if done {
					return errors.New("reset")
				}
				return err
			}, nil},
			{"ResetOverride: a reset row", func() error {
				done, err := s.Variables.ResetOverride(ctx, OrgA, g.ID, free.ID, Bob, Now.Add(time.Hour))
				if done {
					return errors.New("reset")
				}
				return err
			}, nil},
		}
		for _, c := range cases {
			before, beforeB := s.Dump(t, OrgA), s.Dump(t, OrgB)
			if err := c.call(); c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
				t.Errorf("%s = %v; want %v", c.name, err, c.want)
			}
			s.Unchanged(t, c.name, OrgA, before)
			s.Unchanged(t, c.name, OrgB, beforeB)
		}

		// The limits, at the limit (each organization's own).
		insertVariables(t, s, OrgB, variables.MaxVariables-1)
		insertGroups(t, s, OrgB, variables.MaxGroups-1)
		before, beforeA := s.Dump(t, OrgB), s.Dump(t, OrgA)
		if _, _, err := s.Variables.CreateVariable(ctx, newVariable(OrgB, "NEW", "x")); !errors.Is(err, variables.ErrLimitReached) {
			t.Errorf("CreateVariable: the count = %v", err)
		}
		if _, _, err := s.Variables.CreateGroup(ctx, newGroup(OrgB, "new")); !errors.Is(err, variables.ErrGroupLimitReached) {
			t.Errorf("CreateGroup: the count = %v", err)
		}
		s.Unchanged(t, "the creates at the limits", OrgB, before)
		s.Unchanged(t, "the creates at the limits", OrgA, beforeA)
	})
}

// A Reset with no override, or of a reset row, changes nothing and
// answers false.
func TestResettingNoOverrideOrAResetRowChangesNothing(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		x := create(t, s, OrgA, "X", "org")
		g := group(t, s, OrgA, "eu-edge")
		before := s.Dump(t, OrgA)
		if done := reset(t, s, OrgA, g.ID, x.ID); done {
			t.Fatal("a Reset with no override answered true")
		}
		s.Unchanged(t, "a Reset with no override", OrgA, before)
		override(t, s, OrgA, g.ID, x.ID, "a")
		if done := reset(t, s, OrgA, g.ID, x.ID); !done {
			t.Fatal("a Reset of an override answered false")
		}
		before = s.Dump(t, OrgA)
		if done, err := s.Variables.ResetOverride(ctx, OrgA, g.ID, x.ID, Bob, Now.Add(time.Hour)); err != nil || done {
			t.Fatalf("a Reset of a reset row = %v, %v", done, err)
		}
		s.Unchanged(t, "a Reset of a reset row", OrgA, before)
	})
}

// ChangedBy and ChangedAt follow every change: a variable's create, value,
// description and rename; a group's create and change; an override's set and
// Reset, which move its group's too and never the variable's.
func TestChangedByAndChangedAtFollowEveryChange(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		minute := func(n int) time.Time { return Now.Add(time.Duration(n) * time.Minute) }
		v := newVariable(OrgA, "X", "1")
		_, created, err := s.Variables.CreateVariable(ctx, v)
		if err != nil || !same(created.ChangedBy, Ana) || !created.ChangedAt.Equal(Millisecond(Now)) {
			t.Fatalf("created = %+v, %v", created, err)
		}
		for i, step := range []struct {
			changes variables.VariableChanges
			actor   *fleets.Actor
		}{
			{variables.VariableChanges{Value: Ptr("2")}, Bob},
			{variables.VariableChanges{Description: collectorgroups.SetTo("d")}, nil},
			{variables.VariableChanges{Name: Ptr("Y")}, Ana},
		} {
			got, err := s.Variables.UpdateVariable(ctx, OrgA, v.ID, step.changes, step.actor, minute(i+1))
			if err != nil || !same(got.ChangedBy, step.actor) || !got.ChangedAt.Equal(Millisecond(minute(i+1))) {
				t.Fatalf("step %d = %+v, %v", i, got, err)
			}
		}
		list, _ := s.Variables.ListVariables(ctx, OrgA)
		if len(list) != 1 || !same(list[0].ChangedBy, Ana) || !list[0].ChangedAt.Equal(Millisecond(minute(3))) {
			t.Fatalf("listed %+v", list)
		}

		_, g, err := s.Variables.CreateGroup(ctx, newGroup(OrgA, "eu-edge"))
		if err != nil || !same(g.ChangedBy, Ana) || !g.ChangedAt.Equal(Millisecond(Now)) {
			t.Fatalf("a group created = %+v, %v", g, err)
		}
		if g, err = s.Variables.UpdateGroup(ctx, OrgA, g.ID, variables.GroupChanges{Description: collectorgroups.SetTo("d")}, Bob, minute(4)); err != nil ||
			!same(g.ChangedBy, Bob) || !g.ChangedAt.Equal(Millisecond(minute(4))) {
			t.Fatalf("a group changed = %+v, %v", g, err)
		}
		o, _, err := s.Variables.SetOverride(ctx, OrgA, g.ID, v.ID, "o", nil, minute(5))
		if err != nil || o.ChangedBy != nil || !o.ChangedAt.Equal(Millisecond(minute(5))) {
			t.Fatalf("an override set = %+v, %v", o, err)
		}
		got, overrides, _ := s.Variables.GetGroup(ctx, OrgA, g.ID)
		if got.ChangedBy != nil || !got.ChangedAt.Equal(Millisecond(minute(5))) || len(overrides) != 1 || !same(overrides[0], o) {
			t.Fatalf("the group after the set = %+v, %+v", got, overrides)
		}
		if _, err := s.Variables.ResetOverride(ctx, OrgA, g.ID, v.ID, Bob, minute(6)); err != nil {
			t.Fatal(err)
		}
		if got, _, _ = s.Variables.GetGroup(ctx, OrgA, g.ID); !same(got.ChangedBy, Bob) || !got.ChangedAt.Equal(Millisecond(minute(6))) {
			t.Fatalf("the group after the Reset = %+v", got)
		}
		if list, _ = s.Variables.ListVariables(ctx, OrgA); !same(list[0].ChangedBy, Ana) || !list[0].ChangedAt.Equal(Millisecond(minute(3))) {
			t.Fatalf("the variable moved with its override: %+v", list[0])
		}
	})
}

// Each variable resolves on its own: the pin's variable group's override,
// else the collector group's, else the organization's value; an
// organization's change hidden by the pin's override changes neither the
// value nor the stamp.
func TestVariablesResolvesPerVariablePinThenGroupThenOrganization(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		for _, name := range []string{"A", "B", "C", "D"} {
			create(t, s, OrgA, name, strings.ToLower(name)+"-org")
		}
		list, _ := s.Variables.ListVariables(ctx, OrgA)
		id := map[string]uuid.UUID{}
		for _, v := range list {
			id[v.Name] = v.ID
		}
		g, p := group(t, s, OrgA, "group's"), group(t, s, OrgA, "pin's")
		override(t, s, OrgA, g.ID, id["B"], "b-group")
		override(t, s, OrgA, g.ID, id["C"], "c-group")
		override(t, s, OrgA, p.ID, id["C"], "c-pin")
		override(t, s, OrgA, p.ID, id["D"], "d-pin")
		for _, c := range []struct {
			name       string
			group, pin *uuid.UUID
			want       map[string]string
		}{
			{"both", &g.ID, &p.ID, map[string]string{"A": "a-org", "B": "b-group", "C": "c-pin", "D": "d-pin"}},
			{"the group's only", &g.ID, nil, map[string]string{"A": "a-org", "B": "b-group", "C": "c-group", "D": "d-org"}},
			{"the pin's only", nil, &p.ID, map[string]string{"A": "a-org", "B": "b-org", "C": "c-pin", "D": "d-pin"}},
			{"neither", nil, nil, map[string]string{"A": "a-org", "B": "b-org", "C": "c-org", "D": "d-org"}},
			{"an unknown variable group", Ptr(uuid.New()), Ptr(uuid.New()), map[string]string{"A": "a-org", "B": "b-org", "C": "c-org", "D": "d-org"}},
		} {
			if r := resolve(t, s, OrgA, c.group, c.pin); !same(r.Values, c.want) || r.Revision != 8 {
				t.Errorf("%s: resolved %+v; want %v", c.name, r, c.want)
			}
		}
		hidden := resolve(t, s, OrgA, &g.ID, &p.ID)
		setValue(t, s, OrgA, id["C"], "c-org-2")
		after := resolve(t, s, OrgA, &g.ID, &p.ID)
		if after.Values["C"] != "c-pin" || after.Stamps["C"] != hidden.Stamps["C"] || after.Revision != hidden.Revision+1 {
			t.Fatalf("an organization's change under the pin's override: %+v, before %+v", after, hidden)
		}
	})
}

// orgvariables.Choose's cases through the store: each name's stamp is the
// greatest from the top down to its source. And on every combination of the
// three levels' rows (none, a reset row, a value; their stamps in each
// order), the store answers what orgvariables.Choose does.
func TestVariablesStampsEachNameFromTheTopDownToItsSource(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		x := create(t, s, OrgA, "X", "org") // 1
		g, p := group(t, s, OrgA, "group's"), group(t, s, OrgA, "pin's")
		check := func(step, value string, stamp int64) {
			t.Helper()
			r := resolve(t, s, OrgA, &g.ID, &p.ID)
			if r.Values["X"] != value || r.Stamps["X"] != stamp {
				t.Fatalf("%s: resolved %q at %d; want %q at %d", step, r.Values["X"], r.Stamps["X"], value, stamp)
			}
		}
		override(t, s, OrgA, g.ID, x.ID, "g1") // 2
		check("an override over the organization's value", "g1", 2)
		override(t, s, OrgA, g.ID, x.ID, "g2") // 3
		check("that override changed", "g2", 3)
		reset(t, s, OrgA, g.ID, x.ID) // 4
		check("that override Reset", "org", 4)
		override(t, s, OrgA, g.ID, x.ID, "g3") // 5
		check("set again on the reset row", "g3", 5)
		override(t, s, OrgA, p.ID, x.ID, "p1") // 6
		check("the pin's override", "p1", 6)
		reset(t, s, OrgA, p.ID, x.ID) // 7
		check("a Reset at the pin's level over a live group override", "g3", 7)
		override(t, s, OrgA, g.ID, x.ID, "g4") // 8
		check("the group's override changed under the pin's reset row", "g4", 8)
		setValue(t, s, OrgA, x.ID, "org2") // 9
		check("the organization's value changed under a live override", "g4", 8)
		override(t, s, OrgA, p.ID, x.ID, "p2") // 10
		reset(t, s, OrgA, g.ID, x.ID)          // 11
		check("the group's override Reset under a live pin override", "p2", 10)

		// Every combination, written straight into the tables: each a
		// variable of its own, under the same two variable groups.
		type level struct {
			present, value bool
			stamp          int64
		}
		levels := []level{{}}
		for stamp := int64(1); stamp <= 3; stamp++ {
			levels = append(levels, level{true, false, stamp}, level{true, true, stamp})
		}
		type combination struct {
			name       string
			pin, group level
			stamp      int64
		}
		var combinations []combination
		for _, pin := range levels {
			for _, grp := range levels {
				for stamp := int64(1); stamp <= 3; stamp++ {
					combinations = append(combinations, combination{fmt.Sprintf("C_%d", len(combinations)), pin, grp, stamp})
				}
			}
		}
		write := func(c combination) {
			id := uuid.New()
			rows := []struct {
				group uuid.UUID
				l     level
				value string
			}{{g.ID, c.group, "group-" + c.name}, {p.ID, c.pin, "pin-" + c.name}}
			if _, err := s.Databases.Owner.Exec(ctx, `
				insert into opamp_server.variables (id, organization_id, name, description, value,
				  changed_by_id, changed_by_email, changed_at, stamp)
				values ($1, $2, $3, null, $4, null, null, $5, $6)`,
				id, OrgA, c.name, "org-"+c.name, Now, c.stamp); err != nil {
				t.Fatal(err)
			}
			for _, r := range rows {
				if !r.l.present {
					continue
				}
				var value *string
				if r.l.value {
					value = Ptr(r.value)
				}
				if _, err := s.Databases.Owner.Exec(ctx, `
					insert into opamp_server.variable_group_overrides (organization_id, variable_group_id, variable_id,
					  value, changed_by_id, changed_by_email, changed_at, stamp)
					values ($1, $2, $3, $4, null, null, $5, $6)`,
					OrgA, r.group, id, value, Now, r.l.stamp); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, c := range combinations {
			write(c)
		}
		r := resolve(t, s, OrgA, &g.ID, &p.ID)
		asLevel := func(l level, value string) *orgvariables.Level {
			if !l.present {
				return nil
			}
			out := &orgvariables.Level{Stamp: l.stamp}
			if l.value {
				out.Value = Ptr(value)
			}
			return out
		}
		for _, c := range combinations {
			wantValue, wantStamp := orgvariables.Choose(asLevel(c.pin, "pin-"+c.name), asLevel(c.group, "group-"+c.name),
				orgvariables.Level{Value: Ptr("org-" + c.name), Stamp: c.stamp})
			if r.Values[c.name] != wantValue || r.Stamps[c.name] != wantStamp {
				t.Errorf("%s (pin %+v, group %+v, variable %d): resolved %q at %d; Choose says %q at %d", c.name, c.pin, c.group, c.stamp,
					r.Values[c.name], r.Stamps[c.name], wantValue, wantStamp)
			}
		}
		if len(combinations) != 147 {
			t.Fatalf("%d combinations", len(combinations))
		}
	})
}

// An organization whose counter never moved resolves to Resolved{} (the poll
// reads nothing for it), and so does one that isn't registered; a variable
// group's create doesn't move the counter. Once it resolves, it answers its
// generation: the same on every read, another once it is deleted and
// registered again.
func TestAnOrganizationThatNeverChangedResolvesNothing(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		g := group(t, s, OrgA, "eu-edge")
		for _, org := range []uuid.UUID{OrgA, OrgUnregistered} {
			if r := resolve(t, s, org, &g.ID, &g.ID); !same(r, orgvariables.Resolved{}) {
				t.Fatalf("%s resolved %+v", org, r)
			}
		}
		create(t, s, OrgA, "X", "x")
		first := resolve(t, s, OrgA, nil, nil)
		if first.Revision != 1 || !same(first.Values, map[string]string{"X": "x"}) || first.Generation == uuid.Nil {
			t.Fatalf("after a create: %+v", first)
		}
		if again := resolve(t, s, OrgA, &g.ID, nil); again.Generation != first.Generation {
			t.Fatalf("a second read answered the generation %s; want %s", again.Generation, first.Generation)
		}
		if deleted, err := s.Organizations.Delete(ctx, OrgA); err != nil || !deleted {
			t.Fatalf("deleting the organization: %v, %v", deleted, err)
		}
		if result, _, err := s.Organizations.Create(ctx, organizations.Organization{ID: OrgA, Name: "again", CreatedAt: Now, UpdatedAt: Now}); err != nil || result != organizations.Created {
			t.Fatalf("registering it again: %v, %v", result, err)
		}
		create(t, s, OrgA, "X", "y")
		if r := resolve(t, s, OrgA, nil, nil); r.Revision != 1 || r.Generation == uuid.Nil || r.Generation == first.Generation {
			t.Fatalf("registered again: %+v; want revision 1 under a new generation (was %s)", r, first.Generation)
		}
	})
}

// Stamps are the counter's values in commit order: concurrent writers get
// distinct stamps, and a reader that sees revision n sees every write
// stamped n or less, and none stamped after.
func TestStampsAreCommitOrdered(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		const writers, writes = 4, 12
		ids := make([]uuid.UUID, writers)
		for i := range ids {
			ids[i] = create(t, s, OrgA, fmt.Sprintf("W%d", i), "0").ID
		}
		stamps := make([][]int64, writers) // stamps[i][j]: writer i's write j (0: the create)
		for i := range stamps {
			stamps[i] = []int64{int64(i + 1)}
		}
		var (
			wg       sync.WaitGroup
			done     = make(chan struct{})
			mu       sync.Mutex
			snapshot []orgvariables.Resolved
		)
		readerDone := make(chan struct{})
		go func() {
			defer close(readerDone)
			for {
				select {
				case <-done:
					return
				default:
				}
				r, err := s.Variables.Variables(ctx, OrgA, nil, nil)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				snapshot = append(snapshot, r)
				mu.Unlock()
			}
		}()
		for i := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 1; j <= writes; j++ {
					if _, err := s.Variables.UpdateVariable(ctx, OrgA, ids[i], variables.VariableChanges{Value: Ptr(strconv.Itoa(j))}, Ana, Now); err != nil {
						t.Error(err)
						return
					}
					r, err := s.Variables.Variables(ctx, OrgA, nil, nil)
					if err != nil {
						t.Error(err)
						return
					}
					stamps[i] = append(stamps[i], r.Stamps[fmt.Sprintf("W%d", i)]) // only writer i writes Wi
				}
			}()
		}
		wg.Wait()
		close(done)
		<-readerDone
		if t.Failed() {
			return
		}

		seen := map[int64]bool{}
		for i := range writers {
			for _, stamp := range stamps[i] {
				if seen[stamp] {
					t.Fatalf("the stamp %d was given twice", stamp)
				}
				seen[stamp] = true
			}
		}
		if n := revision(t, s, OrgA); n != writers*(writes+1) || len(seen) != int(n) {
			t.Fatalf("the counter is %d with %d stamps; want %d", n, len(seen), writers*(writes+1))
		}
		snapshot = append(snapshot, resolve(t, s, OrgA, nil, nil))
		for _, r := range snapshot {
			for i := range writers {
				name := fmt.Sprintf("W%d", i)
				j, err := strconv.Atoi(r.Values[name])
				if err != nil {
					t.Fatalf("revision %d: %s's value %q", r.Revision, name, r.Values[name])
				}
				if r.Stamps[name] != stamps[i][j] || stamps[i][j] > r.Revision {
					t.Fatalf("revision %d: %s's write %d stamped %d, read at %d", r.Revision, name, j, stamps[i][j], r.Stamps[name])
				}
				if j < writes && stamps[i][j+1] <= r.Revision {
					t.Fatalf("revision %d misses %s's write %d, stamped %d", r.Revision, name, j+1, stamps[i][j+1])
				}
			}
		}
	})
}

// One organization never reads or changes another's variables, variable
// groups or overrides: another's ids read as not found, its overrides never
// fill in, its names and ids are its own, and its rows and counter never
// move.
func TestOneOrganizationNeverReadsOrChangesAnothers(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		mine := create(t, s, OrgA, "SHARED", "mine")
		myGroup := group(t, s, OrgA, "edge")
		theirs := create(t, s, OrgB, "SHARED", "theirs")
		create(t, s, OrgB, "ONLY_THEIRS", "theirs")
		theirGroup := group(t, s, OrgB, "edge")
		override(t, s, OrgB, theirGroup.ID, theirs.ID, "their override")
		pub := uuid.New()
		publish(t, s, OrgB, pub, "x: ${org:SHARED}\n")
		c := s.CreateFleet(t, OrgB, "globex", fleets.TypeServer)
		cg := assign(t, s, OrgB, c.Fleet.ID, pub, "gateways")
		bindGroup(t, s, OrgB, cg.ID, theirGroup.ID)
		beforeB, revisionB := s.Dump(t, OrgB), revision(t, s, OrgB)

		if list, err := s.Variables.ListVariables(ctx, OrgA); err != nil || len(list) != 1 || list[0].ID != mine.ID {
			t.Fatalf("A's variables = %+v, %v", list, err)
		}
		if list, err := s.Variables.ListGroups(ctx, OrgA); err != nil || len(list) != 1 || list[0].ID != myGroup.ID {
			t.Fatalf("A's groups = %+v, %v", list, err)
		}
		for name, err := range map[string]error{
			"UpdateVariable": func() error {
				_, err := s.Variables.UpdateVariable(ctx, OrgA, theirs.ID, variables.VariableChanges{Value: Ptr("x"), Name: Ptr("TAKEN_OVER")}, Ana, Now)
				return err
			}(),
			"DeleteVariable": s.Variables.DeleteVariable(ctx, OrgA, theirs.ID),
			"SetOverride of their variable": func() error {
				_, _, err := s.Variables.SetOverride(ctx, OrgA, myGroup.ID, theirs.ID, "x", Ana, Now)
				return err
			}(),
			"ResetOverride of their variable": func() error {
				_, err := s.Variables.ResetOverride(ctx, OrgA, myGroup.ID, theirs.ID, Ana, Now)
				return err
			}(),
		} {
			if !errors.Is(err, variables.ErrNotFound) {
				t.Errorf("%s = %v; want ErrNotFound", name, err)
			}
		}
		for name, err := range map[string]error{
			"GetGroup": func() error { _, _, err := s.Variables.GetGroup(ctx, OrgA, theirGroup.ID); return err }(),
			"UpdateGroup": func() error {
				_, err := s.Variables.UpdateGroup(ctx, OrgA, theirGroup.ID, variables.GroupChanges{Name: Ptr("taken over")}, Ana, Now)
				return err
			}(),
			"DeleteGroup": s.Variables.DeleteGroup(ctx, OrgA, theirGroup.ID),
			"SetOverride in their group": func() error {
				_, _, err := s.Variables.SetOverride(ctx, OrgA, theirGroup.ID, mine.ID, "x", Ana, Now)
				return err
			}(),
			"ResetOverride in their group": func() error {
				_, err := s.Variables.ResetOverride(ctx, OrgA, theirGroup.ID, theirs.ID, Ana, Now)
				return err
			}(),
		} {
			if !errors.Is(err, variables.ErrGroupNotFound) {
				t.Errorf("%s = %v; want ErrGroupNotFound", name, err)
			}
		}
		taken := newVariable(OrgA, "NEW", "x")
		taken.ID = theirs.ID
		if result, got, err := s.Variables.CreateVariable(ctx, taken); err != nil || result != variables.Conflict || !same(got, variables.Variable{}) {
			t.Errorf("A's create with B's id = %v, %+v, %v", result, got, err)
		}
		takenGroup := newGroup(OrgA, "new")
		takenGroup.ID = theirGroup.ID
		if result, _, err := s.Variables.CreateGroup(ctx, takenGroup); err != nil || result != variables.Conflict {
			t.Errorf("A's group create with B's id = %v, %v", result, err)
		}
		if r := resolve(t, s, OrgA, &theirGroup.ID, &theirGroup.ID); !same(r.Values, map[string]string{"SHARED": "mine"}) {
			t.Errorf("A resolving with B's variable group = %+v", r)
		}
		// B's used version referencing the name doesn't hold A's.
		if _, err := s.Variables.UpdateVariable(ctx, OrgA, mine.ID, variables.VariableChanges{Name: Ptr("MINE")}, Ana, Now); err != nil {
			t.Errorf("A's rename of a name B uses = %v", err)
		}
		s.Unchanged(t, "A's calls", OrgB, beforeB)
		if n := revision(t, s, OrgB); n != revisionB {
			t.Errorf("B's counter moved from %d to %d", revisionB, n)
		}
		if r := resolve(t, s, OrgB, &theirGroup.ID, nil); !same(r.Values, map[string]string{"SHARED": "their override", "ONLY_THEIRS": "theirs"}) {
			t.Errorf("B resolved %+v", r)
		}
	})
}

// The Dump the isolation and no-change tests compare sees every table an
// organization has rows in: migration 00012's four tables included, and
// every table of the catalogue but the server's own (database.ServerTable,
// of no organization).
func TestTheDumpSeesEveryTableOfTheOrganization(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		x := create(t, s, OrgA, "X", "x")
		g := group(t, s, OrgA, "eu-edge")
		override(t, s, OrgA, g.ID, x.ID, "o")
		publish(t, s, OrgA, uuid.New(), "x: ${org:X}\n")
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		collector := uuid.New()
		s.InsertCollector(t, Collector{ID: collector, OrganizationID: OrgA, FleetID: c.Fleet.ID, GroupID: c.DefaultGroupID,
			Name: "edge-0", PinnedVariableGroupID: Ptr(g.ID), LastSeenAt: Now})
		if err := s.Collectors.RecordError(ctx, OrgA, c.Fleet.ID, collector, collectors.ErrorReport{Message: "failed", At: Now}); err != nil {
			t.Fatal(err)
		}
		key := uuid.New()
		if _, err := s.APIKeys.Create(ctx, apikeys.NewKey{ID: key, Name: "deploy", SecretSHA256: sha256.Sum256(key[:]),
			OrganizationIDs: []uuid.UUID{OrgA}, ExpiresAt: Now.Add(time.Hour), CreatedAt: Now}); err != nil {
			t.Fatal(err)
		}
		// Every table of the schema: the catalogue must list exactly these.
		want := []string{"api_key_organizations", "collector_errors", "collector_groups", "collector_instances", "enrollment_token_events",
			"enrollment_tokens", "fleets", "organizations", "publication_variable_names", "publications",
			"variable_group_overrides", "variable_groups", "variables"}
		rows, err := s.Databases.Owner.Query(ctx, `
			select relname from pg_class
			 where relnamespace = 'opamp_server'::regnamespace and relkind = 'r'
			   and relname not in ('goose_db_version', '`+database.ServerTable+`')
			 order by 1`)
		if err != nil {
			t.Fatal(err)
		}
		catalogue, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || !same(catalogue, want) {
			t.Fatalf("the catalogue's tables are %v, %v; want %v", catalogue, err, want)
		}
		// A row's text may hold a line break (a published version's YAML),
		// so each table is looked for at a line's start.
		dump := "\n" + s.Dump(t, OrgA)
		for _, table := range want {
			if !strings.Contains(dump, "\n"+table+" ") {
				t.Errorf("the dump has no row of %s", table)
			}
		}
		if strings.Contains(s.Dump(t, OrgB), x.ID.String()) {
			t.Error("B's dump holds A's variable")
		}
	})
}

// Another organization's id is found last on a create, by the insert that
// inserts nothing (each store reads its own organization's rows only): it
// is a Conflict only when the name is free and the limit isn't reached;
// otherwise the name taken or the limit is answered. Nothing changes.
func TestAnotherOrganizationsIdIsAConflictOnlyWhenTheNameIsFreeAndTheLimitIsntReached(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		theirs := create(t, s, OrgB, "THEIRS", "t")
		theirGroup := group(t, s, OrgB, "theirs")
		create(t, s, OrgA, "TAKEN", "x")
		group(t, s, OrgA, "taken")
		withTheirID := func(name string) variables.NewVariable {
			v := newVariable(OrgA, name, "x")
			v.ID = theirs.ID
			return v
		}
		withTheirGroupID := func(name string) variables.NewGroup {
			g := newGroup(OrgA, name)
			g.ID = theirGroup.ID
			return g
		}
		check := func(call string, result variables.CreateResult, err error, wantResult variables.CreateResult, wantErr error) {
			t.Helper()
			if wantErr != nil && !errors.Is(err, wantErr) || wantErr == nil && (err != nil || result != wantResult) {
				t.Errorf("%s = %v, %v; want %v, %v", call, result, err, wantResult, wantErr)
			}
		}
		before, beforeB := s.Dump(t, OrgA), s.Dump(t, OrgB)
		result, _, err := s.Variables.CreateVariable(ctx, withTheirID("TAKEN"))
		check("their id with a name taken", result, err, 0, variables.ErrNameTaken)
		result, _, err = s.Variables.CreateVariable(ctx, withTheirID("FREE"))
		check("their id with a free name", result, err, variables.Conflict, nil)
		gResult, _, err := s.Variables.CreateGroup(ctx, withTheirGroupID("taken"))
		check("their group id with a name taken", gResult, err, 0, variables.ErrGroupNameTaken)
		gResult, _, err = s.Variables.CreateGroup(ctx, withTheirGroupID("free"))
		check("their group id with a free name", gResult, err, variables.Conflict, nil)
		s.Unchanged(t, "the creates with B's ids", OrgA, before)
		s.Unchanged(t, "the creates with B's ids", OrgB, beforeB)

		insertVariables(t, s, OrgA, variables.MaxVariables-1)
		insertGroups(t, s, OrgA, variables.MaxGroups-1)
		before = s.Dump(t, OrgA)
		result, _, err = s.Variables.CreateVariable(ctx, withTheirID("FREE"))
		check("their id at the limit", result, err, 0, variables.ErrLimitReached)
		gResult, _, err = s.Variables.CreateGroup(ctx, withTheirGroupID("free"))
		check("their group id at the limit", gResult, err, 0, variables.ErrGroupLimitReached)
		s.Unchanged(t, "the creates with B's ids at the limits", OrgA, before)
		s.Unchanged(t, "the creates with B's ids at the limits", OrgB, beforeB)
	})
}

// A collector group bound past DeleteGroup's guard (a write that didn't
// take the organization's row first) is refused by the binding's NO ACTION
// key; the delete reads what uses it again, once, and answers
// *GroupInUseError. A second violation is a failure, never an answer.
func TestADeleteGroupRacingABindingPastTheGuardAnswersInUse(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		vg := group(t, s, OrgA, "eu-edge")
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		gateways, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID, Name: "gateways", At: Now})
		if err != nil {
			t.Fatal(err)
		}
		binder, err := s.Databases.Runtime.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = binder.Rollback(ctx) })
		if _, err := binder.Exec(ctx, `
			update opamp_server.collector_groups set variable_group_id = $3, variable_group_stamp = 1
			 where organization_id = $1 and id = $2`, OrgA, gateways.ID, vg.ID); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- s.Variables.DeleteGroup(ctx, OrgA, vg.ID) }()
		s.WaitForLockWaiters(t, 1)
		if err := binder.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		err = <-done
		var inUse *variables.GroupInUseError
		if !errors.As(err, &inUse) || inUse.Use.TotalCollectorGroups != 1 || inUse.Use.PinnedCollectors != 0 ||
			!same(inUse.Use.CollectorGroups, []variableuse.GroupRef{{FleetID: c.Fleet.ID, GroupID: gateways.ID}}) {
			t.Fatalf("the delete racing the binding = %v; want *GroupInUseError naming the group", err)
		}

		// Twice past the guard: the retry's read sees nothing, and its
		// delete meets a binding again.
		other, err := s.Groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: OrgA, FleetID: c.Fleet.ID, Name: "others", At: Now})
		if err != nil {
			t.Fatal(err)
		}
		bind := func(groupID uuid.UUID, variableGroupID *uuid.UUID) {
			if _, err := s.Databases.Owner.Exec(ctx, `
				update opamp_server.collector_groups set variable_group_id = $3
				 where organization_id = $1 and id = $2`, OrgA, groupID, variableGroupID); err != nil {
				t.Error(err)
			}
		}
		bind(gateways.ID, nil)
		store := variables.NewPostgres(s.Databases.Runtime)
		var stages []string
		variables.SetGroupDeleteHook(store, func(attempt int, stage string) {
			stages = append(stages, fmt.Sprintf("%d %s", attempt, stage))
			switch {
			case attempt == 0 && stage == "delete":
				bind(gateways.ID, &vg.ID)
			case attempt == 1 && stage == "start":
				bind(gateways.ID, nil)
			case attempt == 1 && stage == "delete":
				bind(other.ID, &vg.ID)
			}
		})
		err = store.DeleteGroup(ctx, OrgA, vg.ID)
		var pgErr *pgconn.PgError
		if err == nil || errors.Is(err, variables.ErrGroupInUse) || !errors.As(err, &pgErr) || pgErr.Code != "23503" ||
			!strings.HasPrefix(err.Error(), "variables: delete group: ") {
			t.Fatalf("a second violation = %v; want a wrapped failure", err)
		}
		if !same(stages, []string{"0 start", "0 delete", "1 start", "1 delete"}) {
			t.Fatalf("the attempts were %v; want two", stages)
		}
	})
}

// A variable's delete moves the counter on by one (every write that can
// change a collector's filled-in text does, since the poll's cache is keyed
// by it), with no stamp left behind; a refused delete, and a variable
// group's delete, move nothing.
func TestAVariablesDeleteMovesTheCounterByOne(t *testing.T) {
	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		x := create(t, s, OrgA, "X", "x")
		y := create(t, s, OrgA, "Y", "y")
		g := group(t, s, OrgA, "eu-edge")
		override(t, s, OrgA, g.ID, x.ID, "o") // 3
		pub := uuid.New()
		publish(t, s, OrgA, pub, "y: ${org:Y}\n")
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		assign(t, s, OrgA, c.Fleet.ID, pub, "gateways")
		revisionB := revision(t, s, OrgB)

		if err := s.Variables.DeleteVariable(ctx, OrgA, x.ID); err != nil {
			t.Fatal(err)
		}
		if n := revision(t, s, OrgA); n != 4 {
			t.Fatalf("after the delete the counter is %d; want 4", n)
		}
		r := resolve(t, s, OrgA, &g.ID, nil)
		if r.Revision != 4 || !same(r.Values, map[string]string{"Y": "y"}) || !same(r.Stamps, map[string]int64{"Y": 2}) {
			t.Fatalf("resolved after the delete: %+v", r)
		}
		if err := s.Variables.DeleteVariable(ctx, OrgA, y.ID); !errors.Is(err, variables.ErrInUse) {
			t.Fatalf("the delete in use = %v", err)
		}
		if err := s.Variables.DeleteGroup(ctx, OrgA, g.ID); err != nil {
			t.Fatal(err)
		}
		if n := revision(t, s, OrgA); n != 4 {
			t.Fatalf("after a refused delete and a group's delete the counter is %d; want 4", n)
		}
		if n := revision(t, s, OrgB); n != revisionB {
			t.Fatalf("the other organization's counter moved from %d to %d", revisionB, n)
		}
	})
}
