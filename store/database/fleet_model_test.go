package database_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/telflo/telflo-opamp-server/store/database"
	"github.com/telflo/telflo-opamp-server/store/database/databasetest"
	"github.com/telflo/telflo-opamp-server/store/organizations"
)

// The fleet model's constraints: every link between an organization's rows
// a composite key carrying organization_id, the in-use keys, the unique
// keys and the bounds, each shown refusing what it must as the server's
// role, and each shown to be what refuses it
// (TestEachRefusalNeedsItsConstraint, the permanent detector: the harness
// remakes the database for every test, so a break made by hand wouldn't
// survive: CONTRIBUTING.md, "Tests").

// The fixture's ids: two organizations, A with two fleets and B with one.
const (
	orgA            = "aaaaaaaa-0000-4000-8000-000000000001"
	orgB            = "bbbbbbbb-0000-4000-8000-000000000001"
	orgUnregistered = "cccccccc-0000-4000-8000-000000000001"
	pubA            = "aaaaaaaa-1111-4000-8000-000000000001"
	pubB            = "bbbbbbbb-1111-4000-8000-000000000001"
	fleetA1         = "aaaaaaaa-2222-4000-8000-000000000001"
	fleetA2         = "aaaaaaaa-2222-4000-8000-000000000002"
	fleetB          = "bbbbbbbb-2222-4000-8000-000000000001"
	groupA1         = "aaaaaaaa-3333-4000-8000-000000000001" // fleet A1's Default
	groupA1Gateways = "aaaaaaaa-3333-4000-8000-000000000002"
	groupA2         = "aaaaaaaa-3333-4000-8000-000000000003" // fleet A2's Default
	groupB          = "bbbbbbbb-3333-4000-8000-000000000001" // fleet B's Default
	tokenA1Default  = "aaaaaaaa-5555-4000-8000-000000000001"
	tokenA1Rotate   = "aaaaaaaa-5555-4000-8000-000000000002"
	tokenA2Default  = "aaaaaaaa-5555-4000-8000-000000000003"
	tokenA2Rotate   = "aaaaaaaa-5555-4000-8000-000000000004"
	tokenBDefault   = "bbbbbbbb-5555-4000-8000-000000000001"
	tokenBRotate    = "bbbbbbbb-5555-4000-8000-000000000002"
	collectorA1     = "aaaaaaaa-6666-4000-8000-000000000001"
	collectorB      = "bbbbbbbb-6666-4000-8000-000000000001"
	uidA1           = "aaaaaaaa-7777-4000-8000-000000000001"
	uidB            = "bbbbbbbb-7777-4000-8000-000000000001"
	variableA       = "aaaaaaaa-8888-4000-8000-000000000001" // REGION
	variableA2      = "aaaaaaaa-8888-4000-8000-000000000002" // TIER, made by linkVariables
	variableB       = "bbbbbbbb-8888-4000-8000-000000000001" // REGION
	variableGroupA  = "aaaaaaaa-9999-4000-8000-000000000001" // eu
	variableGroupA2 = "aaaaaaaa-9999-4000-8000-000000000002"
	variableGroupB  = "bbbbbbbb-9999-4000-8000-000000000001" // us
	apiKey          = "abababab-aaaa-4000-8000-000000000001" // limited to A and B, made by linkAPIKeys
	at              = "2026-10-12T09:00:00.000Z"
)

// fill writes the fixture's ids into a statement: {orgA} and so on.
var fill = strings.NewReplacer(
	"{orgA}", orgA, "{orgB}", orgB, "{orgUnregistered}", orgUnregistered,
	"{pubA}", pubA, "{pubB}", pubB,
	"{fleetA1}", fleetA1, "{fleetA2}", fleetA2, "{fleetB}", fleetB,
	"{groupA1}", groupA1, "{groupA1Gateways}", groupA1Gateways, "{groupA2}", groupA2, "{groupB}", groupB,
	"{tokenA1Default}", tokenA1Default, "{tokenA1Rotate}", tokenA1Rotate,
	"{tokenA2Default}", tokenA2Default, "{tokenA2Rotate}", tokenA2Rotate,
	"{tokenBDefault}", tokenBDefault, "{tokenBRotate}", tokenBRotate,
	"{collectorA1}", collectorA1, "{collectorB}", collectorB,
	"{uidA1}", uidA1, "{uidB}", uidB,
	"{variableA}", variableA, "{variableA2}", variableA2, "{variableB}", variableB,
	"{variableGroupA}", variableGroupA, "{variableGroupA2}", variableGroupA2, "{variableGroupB}", variableGroupB,
	"{apiKey}", apiKey,
	"{at}", at,
).Replace

// fixture is two organizations' fleet models, nothing linked yet but each
// collector's group and token: A has a server fleet A1 (its Default group
// and "gateways", collector edge-01) and a kubernetes-supervisor fleet A2;
// B has one server fleet with a collector. Every fleet has its Default
// group and a default and a rotate token.
const fixture = `
insert into telflo_opamp.organizations (id, name, created_at, updated_at) values
  ('{orgA}', 'Acme', '{at}', '{at}'),
  ('{orgB}', 'Globex', '{at}', '{at}');
insert into telflo_opamp.publications (id, organization_id, configuration_id, configuration_name,
  version_id, version_number, version_label, yaml, sha256, published_by_id, published_by_email,
  published_at, received_at, collector_distribution, collector_version) values
  ('{pubA}', '{orgA}', gen_random_uuid(), 'payments-gateway', gen_random_uuid(), 3, null,
   'receivers: {}', repeat('a', 64), null, null, '{at}', '{at}', 'otelcol-contrib', '0.162.0'),
  ('{pubB}', '{orgB}', gen_random_uuid(), 'payments-gateway', gen_random_uuid(), 1, null,
   'receivers: {}', repeat('b', 64), null, null, '{at}', '{at}', 'otelcol-contrib', '0.162.0');
insert into telflo_opamp.fleets (id, organization_id, label, type, status, created_by_id,
  created_by_email, created_at, updated_at) values
  ('{fleetA1}', '{orgA}', 'edge-prod', 'server', 'active', null, null, '{at}', '{at}'),
  ('{fleetA2}', '{orgA}', 'edge-k8s', 'kubernetes-supervisor', 'active', null, null, '{at}', '{at}'),
  ('{fleetB}', '{orgB}', 'globex-prod', 'server', 'active', null, null, '{at}', '{at}');
insert into telflo_opamp.collector_groups (id, organization_id, fleet_id, name, is_default,
  assigned_publication_id, created_at, updated_at) values
  ('{groupA1}', '{orgA}', '{fleetA1}', 'Default', true, null, '{at}', '{at}'),
  ('{groupA1Gateways}', '{orgA}', '{fleetA1}', 'gateways', false, null, '{at}', '{at}'),
  ('{groupA2}', '{orgA}', '{fleetA2}', 'Default', true, null, '{at}', '{at}'),
  ('{groupB}', '{orgB}', '{fleetB}', 'Default', true, null, '{at}', '{at}');
insert into telflo_opamp.enrollment_tokens (id, organization_id, fleet_id, kind, expires_at,
  revoked_at, do_refresh, created_by_id, created_by_email, created_at) values
  ('{tokenA1Default}', '{orgA}', '{fleetA1}', 'default', null, null, false, null, null, '{at}'),
  ('{tokenA1Rotate}', '{orgA}', '{fleetA1}', 'rotate', null, null, false, null, null, '{at}'),
  ('{tokenA2Default}', '{orgA}', '{fleetA2}', 'default', null, null, false, null, null, '{at}'),
  ('{tokenA2Rotate}', '{orgA}', '{fleetA2}', 'rotate', null, null, false, null, null, '{at}'),
  ('{tokenBDefault}', '{orgB}', '{fleetB}', 'default', null, null, false, null, null, '{at}'),
  ('{tokenBRotate}', '{orgB}', '{fleetB}', 'rotate', null, null, false, null, null, '{at}');
insert into telflo_opamp.collector_instances (id, organization_id, fleet_id, collector_group_id,
  instance_uid, name, enrollment_token_id, last_seen_at, created_at, updated_at) values
  ('{collectorA1}', '{orgA}', '{fleetA1}', '{groupA1}', '{uidA1}', 'edge-01', '{tokenA1Rotate}', '{at}', '{at}', '{at}'),
  ('{collectorB}', '{orgB}', '{fleetB}', '{groupB}', '{uidB}', 'globex-01', '{tokenBRotate}', '{at}', '{at}', '{at}');
`

// variablesFixture is each organization's variable REGION and variable
// group, nothing linked yet (migration 00012): A's eu and B's us. A schema
// before 00012 takes the fixture alone.
const variablesFixture = `
insert into telflo_opamp.variables (id, organization_id, name, description, value, changed_by_id,
  changed_by_email, changed_at, stamp) values
  ('{variableA}', '{orgA}', 'REGION', null, 'eu-west-1', null, null, '{at}', 1),
  ('{variableB}', '{orgB}', 'REGION', null, 'us-east-1', null, null, '{at}', 1);
insert into telflo_opamp.variable_groups (id, organization_id, name, description, changed_by_id,
  changed_by_email, changed_at) values
  ('{variableGroupA}', '{orgA}', 'eu', null, null, null, '{at}'),
  ('{variableGroupB}', '{orgB}', 'us', null, null, null, '{at}');
`

// fixtureOrganizations are the fixture's organizations as migrations before
// 00012 take them; fixtureOrganizationsAt12 as 00012 does, with the
// generation it added without a default.
const (
	fixtureOrganizations = `insert into telflo_opamp.organizations (id, name, created_at, updated_at) values
  ('{orgA}', 'Acme', '{at}', '{at}'),
  ('{orgB}', 'Globex', '{at}', '{at}');`
	fixtureOrganizationsAt12 = `insert into telflo_opamp.organizations (id, name, created_at, updated_at, generation) values
  ('{orgA}', 'Acme', '{at}', '{at}', gen_random_uuid()),
  ('{orgB}', 'Globex', '{at}', '{at}', gen_random_uuid());`
)

// latestFixture is the fixture as the newest migration's columns take it.
func latestFixture(t *testing.T) string {
	t.Helper()
	if strings.Count(fixture, fixtureOrganizations) != 1 {
		t.Fatal("the fixture's organizations aren't fixtureOrganizations")
	}
	return strings.Replace(fixture, fixtureOrganizations, fixtureOrganizationsAt12, 1)
}

// newFleetModel is a migrated database holding the fixture and the
// variables' fixture, written as the owner.
func newFleetModel(t *testing.T) *databasetest.Databases {
	t.Helper()
	d := databasetest.New(t, testDatabase)
	if _, err := d.Owner.Exec(context.Background(), fill(latestFixture(t)+variablesFixture)); err != nil {
		t.Fatalf("the fixture: %v", err)
	}
	return d
}

// Statements adding one row, the fixture's ids written in by fill. Each
// leaves every column but those it names valid.
func newFleet(organization, label, fleetType string) string {
	return fmt.Sprintf(`insert into telflo_opamp.fleets (id, organization_id, label, type, status,
	  created_by_id, created_by_email, created_at, updated_at)
	  values (gen_random_uuid(), '%s', %s, '%s', 'active', null, null, '{at}', '{at}')`, organization, label, fleetType)
}

func newGroup(organization, fleet, name string, isDefault bool) string {
	return fmt.Sprintf(`insert into telflo_opamp.collector_groups (id, organization_id, fleet_id, name,
	  is_default, assigned_publication_id, created_at, updated_at)
	  values (gen_random_uuid(), '%s', '%s', %s, %t, null, '{at}', '{at}')`,
		organization, fleet, name, isDefault)
}

func newToken(id, organization, fleet, kind, expiresAt string) string {
	return fmt.Sprintf(`insert into telflo_opamp.enrollment_tokens (id, organization_id, fleet_id, kind,
	  expires_at, revoked_at, do_refresh, created_by_id, created_by_email, created_at)
	  values (%s, '%s', '%s', '%s', %s, null, false, null, null, '{at}')`, id, organization, fleet, kind, expiresAt)
}

func newEvent(organization, fleet, token, event string) string {
	return fmt.Sprintf(`insert into telflo_opamp.enrollment_token_events (id, organization_id, fleet_id,
	  enrollment_token_id, event, actor_id, actor_email, occurred_at)
	  values (gen_random_uuid(), '%s', '%s', '%s', %s, null, null, '{at}')`, organization, fleet, token, event)
}

func newCollector(organization, fleet, group, uid, name string) string {
	return fmt.Sprintf(`insert into telflo_opamp.collector_instances (id, organization_id, fleet_id,
	  collector_group_id, instance_uid, name, last_seen_at, created_at, updated_at)
	  values (gen_random_uuid(), '%s', '%s', '%s', %s, %s, '{at}', '{at}', '{at}')`, organization, fleet, group, uid, name)
}

func newError(organization, collector, message string) string {
	return fmt.Sprintf(`insert into telflo_opamp.collector_errors (id, organization_id,
	  collector_instance_id, reported_at, config_sha256, message)
	  values (gen_random_uuid(), '%s', '%s', '{at}', null, %s)`, organization, collector, message)
}

// updateCollectorA1 sets columns of A's collector edge-01.
func updateCollectorA1(set string) string {
	return "update telflo_opamp.collector_instances set " + set +
		" where organization_id = '{orgA}' and id = '{collectorA1}'"
}

// updateGroupA1 sets columns of fleet A1's Default group.
func updateGroupA1(set string) string {
	return "update telflo_opamp.collector_groups set " + set +
		" where organization_id = '{orgA}' and id = '{groupA1}'"
}

// Migration 00012's rows: a variable, a variable group, an override and a
// published version's referenced name, each with every column but those
// named valid.
func newVariable(id, organization, name, value string) string {
	return fmt.Sprintf(`insert into telflo_opamp.variables (id, organization_id, name, description, value,
	  changed_by_id, changed_by_email, changed_at, stamp)
	  values (%s, '%s', %s, null, %s, null, null, '{at}', 1)`, id, organization, name, value)
}

func newVariableGroup(id, organization, name string) string {
	return fmt.Sprintf(`insert into telflo_opamp.variable_groups (id, organization_id, name, description,
	  changed_by_id, changed_by_email, changed_at)
	  values (%s, '%s', %s, null, null, null, '{at}')`, id, organization, name)
}

func newOverride(organization, variableGroup, variable, value string) string {
	return fmt.Sprintf(`insert into telflo_opamp.variable_group_overrides (organization_id, variable_group_id,
	  variable_id, value, changed_by_id, changed_by_email, changed_at, stamp)
	  values ('%s', '%s', '%s', %s, null, null, '{at}', 1)`, organization, variableGroup, variable, value)
}

func newVariableName(organization, publication, name string) string {
	return fmt.Sprintf(`insert into telflo_opamp.publication_variable_names (organization_id, publication_id, name)
	  values ('%s', '%s', %s)`, organization, publication, name)
}

// overrideA is A's eu overriding REGION.
var overrideA = newOverride("{orgA}", "{variableGroupA}", "{variableA}", "'eu-central-1'")

// updateVariableA sets columns of A's variable REGION.
func updateVariableA(set string) string {
	return "update telflo_opamp.variables set " + set +
		" where organization_id = '{orgA}' and id = '{variableA}'"
}

// updateVariableGroupA sets columns of A's variable group eu.
func updateVariableGroupA(set string) string {
	return "update telflo_opamp.variable_groups set " + set +
		" where organization_id = '{orgA}' and id = '{variableGroupA}'"
}

// updateOverrideA sets columns of overrideA, which the caller inserts first.
func updateOverrideA(set string) string {
	return "update telflo_opamp.variable_group_overrides set " + set +
		" where organization_id = '{orgA}' and variable_group_id = '{variableGroupA}' and variable_id = '{variableA}'"
}

// refusal is a statement the server's role must be refused: setup runs
// first in the same transaction, then statement fails with code, naming
// constraint. Dropping drop (constraint by default) lets it through.
type refusal struct {
	name       string
	setup      []string
	statement  string
	code       string
	constraint string
	drop       []string
}

// Every link carries the organization, so a row can't name another
// organization's (or, for a collector's group and token, another fleet's).
var linkRefusals = []refusal{
	{name: "a group naming another organization's fleet",
		statement: newGroup("{orgA}", "{fleetB}", "'gateways-b'", false), constraint: "fk_collector_groups_fleet"},
	{name: "a group assigned another organization's published version",
		statement: updateGroupA1("assigned_publication_id = '{pubB}'"), constraint: "fk_collector_groups_assigned_publication"},
	{name: "a token naming another organization's fleet",
		statement: newToken("gen_random_uuid()", "{orgA}", "{fleetB}", "rotate", "null"), constraint: "fk_enrollment_tokens_fleet"},
	// The token's key carries the fleet too, so it refuses as well once the
	// fleet's key is gone.
	{name: "a token event naming another organization's fleet",
		statement:  newEvent("{orgA}", "{fleetB}", "{tokenBRotate}", "'viewed'"),
		constraint: "fk_enrollment_token_events_fleet",
		drop:       []string{"fk_enrollment_token_events_fleet", "fk_enrollment_token_events_enrollment_token"}},
	{name: "a token event naming a token of another fleet",
		statement: newEvent("{orgA}", "{fleetA1}", "{tokenA2Rotate}", "'viewed'"), constraint: "fk_enrollment_token_events_enrollment_token"},
	{name: "a token event naming another organization's token",
		statement: newEvent("{orgA}", "{fleetA1}", "{tokenBRotate}", "'viewed'"), constraint: "fk_enrollment_token_events_enrollment_token"},
	// The group's key carries the fleet too, so it refuses as well once the
	// fleet's key is gone.
	{name: "a collector naming another organization's fleet",
		statement:  newCollector("{orgA}", "{fleetB}", "{groupB}", "gen_random_uuid()", "'edge-09'"),
		constraint: "fk_collector_instances_fleet",
		drop:       []string{"fk_collector_instances_fleet", "fk_collector_instances_collector_group"}},
	{name: "a collector in a group of another fleet",
		statement:  newCollector("{orgA}", "{fleetA1}", "{groupA2}", "gen_random_uuid()", "'edge-09'"),
		constraint: "fk_collector_instances_collector_group"},
	{name: "a collector in a group of another organization",
		statement:  newCollector("{orgA}", "{fleetA1}", "{groupB}", "gen_random_uuid()", "'edge-09'"),
		constraint: "fk_collector_instances_collector_group"},
	{name: "a collector moved to a group of another fleet",
		statement: updateCollectorA1("collector_group_id = '{groupA2}'"), constraint: "fk_collector_instances_collector_group"},
	{name: "a collector pinned to another organization's published version",
		statement: updateCollectorA1("pinned_publication_id = '{pubB}'"), constraint: "fk_collector_instances_pinned_publication"},
	{name: "a collector served another organization's published version",
		statement: updateCollectorA1("served_publication_id = '{pubB}'"), constraint: "fk_collector_instances_served_publication"},
	{name: "a collector on a token of another fleet",
		statement: updateCollectorA1("enrollment_token_id = '{tokenA2Rotate}'"), constraint: "fk_collector_instances_enrollment_token"},
	{name: "a collector on another organization's token",
		statement: updateCollectorA1("enrollment_token_id = '{tokenBRotate}'"), constraint: "fk_collector_instances_enrollment_token"},
	{name: "an error row naming another organization's collector",
		statement: newError("{orgB}", "{collectorA1}", "'failed'"), constraint: "fk_collector_errors_collector_instance"},
	{name: "a fleet of an unregistered organization",
		statement: newFleet("{orgUnregistered}", "'edge-prod'", "server"), constraint: "fk_fleets_organization"},
	// Migration 00012: a binding, an override and a referenced name can only
	// name the same organization's rows.
	{name: "a group bound to another organization's variable group",
		statement:  updateGroupA1("variable_group_id = '{variableGroupB}', variable_group_stamp = 1"),
		constraint: "fk_collector_groups_variable_group"},
	{name: "a collector pinned to another organization's variable group",
		statement:  updateCollectorA1("pinned_variable_group_id = '{variableGroupB}', pinned_variable_group_stamp = 1"),
		constraint: "fk_collector_instances_pinned_variable_group"},
	{name: "an override in another organization's variable group",
		statement:  newOverride("{orgA}", "{variableGroupB}", "{variableA}", "'x'"),
		constraint: "fk_variable_group_overrides_variable_group"},
	{name: "an override of another organization's variable",
		statement:  newOverride("{orgA}", "{variableGroupA}", "{variableB}", "'x'"),
		constraint: "fk_variable_group_overrides_variable"},
	{name: "names for another organization's published version",
		statement:  newVariableName("{orgA}", "{pubB}", "'REGION'"),
		constraint: "fk_publication_variable_names_publication"},
	{name: "a variable of an unregistered organization",
		statement:  newVariable("gen_random_uuid()", "{orgUnregistered}", "'REGION'", "'x'"),
		constraint: "fk_variables_organization"},
	{name: "a variable group of an unregistered organization",
		statement:  newVariableGroup("gen_random_uuid()", "{orgUnregistered}", "'eu'"),
		constraint: "fk_variable_groups_organization"},
	// The two composite keys refuse it as well once the organization's is
	// gone.
	{name: "an override of an unregistered organization",
		statement:  newOverride("{orgUnregistered}", "{variableGroupA}", "{variableA}", "'x'"),
		constraint: "fk_variable_group_overrides_organization",
		drop: []string{"fk_variable_group_overrides_organization", "fk_variable_group_overrides_variable_group",
			"fk_variable_group_overrides_variable"}},
	{name: "names of an unregistered organization",
		statement:  newVariableName("{orgUnregistered}", "{pubA}", "'REGION'"),
		constraint: "fk_publication_variable_names_organization",
		drop:       []string{"fk_publication_variable_names_organization", "fk_publication_variable_names_publication"}},
}

// A published version a group or collector uses can't be deleted
// (answered 409 publication_in_use), and a group can't be deleted while it
// holds a collector (the store deletes them first).
var inUseRefusals = []refusal{
	{name: "deleting a published version a group is assigned",
		setup:     []string{updateGroupA1("assigned_publication_id = '{pubA}'")},
		statement: "delete from telflo_opamp.publications where organization_id = '{orgA}' and id = '{pubA}'",
		code:      "23503", constraint: "fk_collector_groups_assigned_publication"},
	{name: "deleting a published version a collector is pinned to",
		setup:     []string{updateCollectorA1("pinned_publication_id = '{pubA}'")},
		statement: "delete from telflo_opamp.publications where organization_id = '{orgA}' and id = '{pubA}'",
		code:      "23503", constraint: "fk_collector_instances_pinned_publication"},
	{name: "deleting a group that holds a collector",
		setup:     []string{updateCollectorA1("collector_group_id = '{groupA1Gateways}'")},
		statement: "delete from telflo_opamp.collector_groups where organization_id = '{orgA}' and id = '{groupA1Gateways}'",
		code:      "23503", constraint: "fk_collector_instances_collector_group"},
	// A variable group a group or a pin uses can't be deleted (answered 409
	// variable_group_in_use).
	{name: "deleting a variable group a group is bound to",
		setup:     []string{updateGroupA1("variable_group_id = '{variableGroupA}', variable_group_stamp = 1"), overrideA},
		statement: "delete from telflo_opamp.variable_groups where organization_id = '{orgA}' and id = '{variableGroupA}'",
		code:      "23503", constraint: "fk_collector_groups_variable_group"},
	{name: "deleting a variable group a collector is pinned to",
		setup:     []string{updateCollectorA1("pinned_variable_group_id = '{variableGroupA}', pinned_variable_group_stamp = 1"), overrideA},
		statement: "delete from telflo_opamp.variable_groups where organization_id = '{orgA}' and id = '{variableGroupA}'",
		code:      "23503", constraint: "fk_collector_instances_pinned_variable_group"},
}

// One Default group and one default token per fleet; a token's id unique on
// its own (the agent looks it up by id alone); labels, names and instance
// uids unique where the spec says, compared as written.
var uniqueRefusals = []refusal{
	{name: "a second Default group in a fleet",
		statement: newGroup("{orgA}", "{fleetA1}", "'Default 2'", true), constraint: "uq_collector_groups_fleet_id_where_is_default"},
	{name: "a second default token in a fleet",
		statement: newToken("gen_random_uuid()", "{orgA}", "{fleetA1}", "default", "null"), constraint: "uq_enrollment_tokens_fleet_id_where_default_not_revoked"},
	{name: "a token id another organization's fleet has",
		statement: newToken("'{tokenA1Rotate}'", "{orgB}", "{fleetB}", "rotate", "null"), constraint: "enrollment_tokens_pkey"},
	{name: "a fleet label taken in the organization",
		statement: newFleet("{orgA}", "'edge-prod'", "server"), constraint: "uq_fleets_organization_id_label"},
	{name: "a group name taken in the fleet",
		statement: newGroup("{orgA}", "{fleetA1}", "'gateways'", false), constraint: "uq_collector_groups_organization_id_fleet_id_name"},
	{name: "a collector name taken in the organization",
		statement:  newCollector("{orgA}", "{fleetA2}", "{groupA2}", "gen_random_uuid()", "'edge-01'"),
		constraint: "uq_collector_instances_organization_id_name"},
	{name: "an instance uid enrolled in the organization",
		statement:  newCollector("{orgA}", "{fleetA2}", "{groupA2}", "'{uidA1}'", "'edge-02'"),
		constraint: "uq_collector_instances_organization_id_instance_uid"},
	// Migration 00012: names compared as written; one override per variable
	// group and variable; each name once per published version.
	{name: "a variable name taken in the organization",
		statement:  newVariable("gen_random_uuid()", "{orgA}", "'REGION'", "'x'"),
		constraint: "uq_variables_organization_id_name"},
	{name: "a variable id another organization has",
		statement:  newVariable("'{variableA}'", "{orgB}", "'ZONE'", "'x'"),
		constraint: "variables_pkey"},
	{name: "a variable group name taken in the organization",
		statement:  newVariableGroup("gen_random_uuid()", "{orgA}", "'eu'"),
		constraint: "uq_variable_groups_organization_id_name"},
	{name: "a variable group id another organization has",
		statement:  newVariableGroup("'{variableGroupA}'", "{orgB}", "'zone'"),
		constraint: "variable_groups_pkey"},
	{name: "a second override of a variable in a variable group",
		setup:      []string{overrideA},
		statement:  newOverride("{orgA}", "{variableGroupA}", "{variableA}", "'eu-north-1'"),
		constraint: "variable_group_overrides_pkey"},
	{name: "a name listed twice for a published version",
		setup:      []string{newVariableName("{orgA}", "{pubA}", "'REGION'")},
		statement:  newVariableName("{orgA}", "{pubA}", "'REGION'"),
		constraint: "publication_variable_names_pkey"},
}

// The bounds and the at-most-one checks.
var checkRefusals = []refusal{
	{name: "a negative collector cap",
		statement: "update telflo_opamp.organizations set max_collectors = -1 where id = '{orgA}'", constraint: "ck_organizations_max_collectors_non_negative"},
	{name: "a limit of no requests a minute",
		statement: "update telflo_opamp.organizations set requests_per_minute = 0 where id = '{orgA}'", constraint: "ck_organizations_requests_per_minute_positive"},
	{name: "a fleet label with a leading space",
		statement: newFleet("{orgA}", "' edge-x'", "server"), constraint: "ck_fleets_label_trimmed_length"},
	{name: "an empty fleet label",
		statement: newFleet("{orgA}", "''", "server"), constraint: "ck_fleets_label_trimmed_length"},
	{name: "a fleet label with a leading tab",
		statement: newFleet("{orgA}", `E'\tedge-x'`, "server"), constraint: "ck_fleets_label_trimmed_length"},
	{name: "a fleet label with a trailing line break",
		statement: newFleet("{orgA}", `E'edge-x\n'`, "server"), constraint: "ck_fleets_label_trimmed_length"},
	{name: "a fleet label of a space alone",
		statement: newFleet("{orgA}", "' '", "server"), constraint: "ck_fleets_label_trimmed_length"},
	{name: "a fleet label of whitespace alone",
		statement: newFleet("{orgA}", `E'\t\r\n'`, "server"), constraint: "ck_fleets_label_trimmed_length"},
	{name: "a fleet label of 81 characters",
		statement: newFleet("{orgA}", "repeat('x', 81)", "server"), constraint: "ck_fleets_label_trimmed_length"},
	{name: "a kubernetes fleet (the operator bridge's)",
		statement: newFleet("{orgA}", "'edge-x'", "kubernetes"), constraint: "ck_fleets_type"},
	{name: "a fleet neither active nor archived",
		statement: "update telflo_opamp.fleets set status = 'deleted' where organization_id = '{orgA}' and id = '{fleetA1}'", constraint: "ck_fleets_status"},
	{name: "a fleet's creator with an id and no email",
		statement:  strings.Replace(newFleet("{orgA}", "'edge-x'", "server"), "'active', null, null", "'active', gen_random_uuid(), null", 1),
		constraint: "ck_fleets_created_by_both_or_neither"},
	{name: "a fleet's creator with an email of 321 characters",
		statement:  strings.Replace(newFleet("{orgA}", "'edge-x'", "server"), "'active', null, null", "'active', gen_random_uuid(), repeat('e', 321)", 1),
		constraint: "ck_fleets_created_by_email_length"},
	{name: "a group name with a trailing space",
		statement: newGroup("{orgA}", "{fleetA1}", "'edge '", false), constraint: "ck_collector_groups_name_trimmed_length"},
	{name: "a group name with a trailing tab",
		statement: newGroup("{orgA}", "{fleetA1}", `E'gw\t'`, false), constraint: "ck_collector_groups_name_trimmed_length"},
	{name: "a group name of a line break alone",
		statement: newGroup("{orgA}", "{fleetA1}", `E'\n'`, false), constraint: "ck_collector_groups_name_trimmed_length"},
	{name: "a group name of 81 characters",
		statement: newGroup("{orgA}", "{fleetA1}", "repeat('g', 81)", false), constraint: "ck_collector_groups_name_trimmed_length"},
	// Migration 00011: a group's annotations.
	{name: "annotations that are an array",
		statement: updateGroupA1(`annotations = '[{"a": 1}]'`), constraint: "ck_collector_groups_annotations_bounded"},
	{name: "annotations that are a string",
		statement: updateGroupA1(`annotations = '"telflo"'`), constraint: "ck_collector_groups_annotations_bounded"},
	{name: "annotations of more than 16 KiB",
		statement: updateGroupA1(`annotations = jsonb_build_object('telflo', repeat('v', 16400))`), constraint: "ck_collector_groups_annotations_bounded"},
	{name: "a token neither default nor rotate",
		statement: newToken("gen_random_uuid()", "{orgA}", "{fleetA1}", "refresh", "null"), constraint: "ck_enrollment_tokens_kind"},
	{name: "a default token that expires",
		statement:  "update telflo_opamp.enrollment_tokens set expires_at = '{at}' where organization_id = '{orgA}' and id = '{tokenA1Default}'",
		constraint: "ck_enrollment_tokens_default_never_expires"},
	{name: "a token's maker with an email and no id",
		statement:  strings.Replace(newToken("gen_random_uuid()", "{orgA}", "{fleetA1}", "rotate", "null"), "false, null, null", "false, null, 'ana@acme.io'", 1),
		constraint: "ck_enrollment_tokens_created_by_both_or_neither"},
	{name: "a token's maker with an email of 321 characters",
		statement:  strings.Replace(newToken("gen_random_uuid()", "{orgA}", "{fleetA1}", "rotate", "null"), "false, null, null", "false, gen_random_uuid(), repeat('e', 321)", 1),
		constraint: "ck_enrollment_tokens_created_by_email_length"},
	{name: "a token event that isn't issued, viewed, rotated or revoked",
		statement: newEvent("{orgA}", "{fleetA1}", "{tokenA1Rotate}", "'regenerated'"), constraint: "ck_enrollment_token_events_event"},
	{name: "a token event's actor with an id and no email",
		statement:  strings.Replace(newEvent("{orgA}", "{fleetA1}", "{tokenA1Rotate}", "'viewed'"), "'viewed', null, null", "'viewed', gen_random_uuid(), null", 1),
		constraint: "ck_enrollment_token_events_actor_both_or_neither"},
	{name: "a token event's actor with an email of 321 characters",
		statement:  strings.Replace(newEvent("{orgA}", "{fleetA1}", "{tokenA1Rotate}", "'viewed'"), "'viewed', null, null", "'viewed', gen_random_uuid(), repeat('e', 321)", 1),
		constraint: "ck_enrollment_token_events_actor_email_length"},
	{name: "a collector name with a leading space",
		statement:  newCollector("{orgA}", "{fleetA1}", "{groupA1}", "gen_random_uuid()", "' edge-09'"),
		constraint: "ck_collector_instances_name_trimmed_length"},
	{name: "a collector name with a trailing tab",
		statement:  newCollector("{orgA}", "{fleetA1}", "{groupA1}", "gen_random_uuid()", `E'edge-09\t'`),
		constraint: "ck_collector_instances_name_trimmed_length"},
	{name: "a collector name with a leading line break",
		statement:  newCollector("{orgA}", "{fleetA1}", "{groupA1}", "gen_random_uuid()", `E'\nedge-09'`),
		constraint: "ck_collector_instances_name_trimmed_length"},
	{name: "a collector name of whitespace alone",
		statement:  newCollector("{orgA}", "{fleetA1}", "{groupA1}", "gen_random_uuid()", `E' \t'`),
		constraint: "ck_collector_instances_name_trimmed_length"},
	{name: "a collector name of 201 characters",
		statement:  newCollector("{orgA}", "{fleetA1}", "{groupA1}", "gen_random_uuid()", "repeat('c', 201)"),
		constraint: "ck_collector_instances_name_trimmed_length"},
	{name: "a served hash that isn't lowercase hex",
		statement: updateCollectorA1("served_sha256 = repeat('g', 64)"), constraint: "ck_collector_instances_served_sha256_hex"},
	{name: "an effective hash that isn't 64 characters",
		statement: updateCollectorA1("effective_sha256 = repeat('a', 63)"), constraint: "ck_collector_instances_effective_sha256_hex"},
	{name: "an effective status that isn't applied, applying or failed",
		statement: updateCollectorA1("effective_status = 'unknown'"), constraint: "ck_collector_instances_effective_status"},
	{name: "an empty effective error",
		statement: updateCollectorA1("effective_error = ''"), constraint: "ck_collector_instances_effective_error_size"},
	{name: "an effective error of 8 KiB and a byte",
		statement: updateCollectorA1("effective_error = repeat('e', 8193)"), constraint: "ck_collector_instances_effective_error_size"},
	{name: "a reported collector version of 201 characters",
		statement: updateCollectorA1("reported_collector_version = repeat('1', 201)"), constraint: "ck_collector_instances_reported_collector_version_length"},
	{name: "a reported address of 101 characters",
		statement: updateCollectorA1("reported_ip = repeat('1', 101)"), constraint: "ck_collector_instances_reported_ip_length"},
	{name: "a reported host address of 101 characters",
		statement: updateCollectorA1("reported_host_ip = repeat('1', 101)"), constraint: "ck_collector_instances_reported_host_ip_length"},
	{name: "reported attributes that aren't an object",
		statement: updateCollectorA1(`reported_attributes = '["host.name"]'`), constraint: "ck_collector_instances_reported_attributes_bounded"},
	{name: "reported attributes of more than 16 KiB",
		statement: updateCollectorA1(`reported_attributes = jsonb_build_object('host.name', repeat('h', 16400))`), constraint: "ck_collector_instances_reported_attributes_bounded"},
	{name: "an empty error message",
		statement: newError("{orgA}", "{collectorA1}", "''"), constraint: "ck_collector_errors_message_size"},
	{name: "an error message of 8 KiB and a byte",
		statement: newError("{orgA}", "{collectorA1}", "repeat('m', 8193)"), constraint: "ck_collector_errors_message_size"},
	{name: "an error's hash that isn't lowercase hex",
		statement:  strings.Replace(newError("{orgA}", "{collectorA1}", "'failed'"), "'{at}', null", "'{at}', 'abc'", 1),
		constraint: "ck_collector_errors_config_sha256_hex"},
	// Migration 00012: a variable's name and a referenced name are the
	// renderer's grammar in ASCII, at most 128 characters; a value is at most
	// 1,024 characters on one line; descriptions and a variable group's name
	// trimmed and bounded; who changed it both or neither; stamps after a
	// bump, the counter and the marker never negative.
	{name: "a variable name starting with a digit",
		statement: newVariable("gen_random_uuid()", "{orgA}", "'1A'", "'x'"), constraint: "ck_variables_name_format"},
	{name: "a variable name with a hyphen",
		statement: newVariable("gen_random_uuid()", "{orgA}", "'A-B'", "'x'"), constraint: "ck_variables_name_format"},
	{name: "an empty variable name",
		statement: newVariable("gen_random_uuid()", "{orgA}", "''", "'x'"), constraint: "ck_variables_name_format"},
	{name: "a variable name with a letter outside ASCII",
		statement: newVariable("gen_random_uuid()", "{orgA}", "'R' || chr(201) || 'GION'", "'x'"), constraint: "ck_variables_name_format"},
	{name: "a variable name of 129 characters",
		statement: newVariable("gen_random_uuid()", "{orgA}", "'A' || repeat('B', 128)", "'x'"), constraint: "ck_variables_name_format"},
	{name: "a referenced name starting with a digit",
		statement: newVariableName("{orgA}", "{pubA}", "'1A'"), constraint: "ck_publication_variable_names_name_format"},
	{name: "a referenced name with a hyphen",
		statement: newVariableName("{orgA}", "{pubA}", "'A-B'"), constraint: "ck_publication_variable_names_name_format"},
	{name: "an empty referenced name",
		statement: newVariableName("{orgA}", "{pubA}", "''"), constraint: "ck_publication_variable_names_name_format"},
	{name: "a referenced name of 129 characters",
		statement: newVariableName("{orgA}", "{pubA}", "'a' || repeat('b', 128)"), constraint: "ck_publication_variable_names_name_format"},
	{name: "a value of 1,025 characters",
		statement: updateVariableA("value = repeat('v', 1025)"), constraint: "ck_variables_value_single_line"},
	{name: "a value holding a line feed",
		statement: updateVariableA(`value = E'a\nb'`), constraint: "ck_variables_value_single_line"},
	{name: "a value holding a carriage return",
		statement: updateVariableA(`value = E'a\rb'`), constraint: "ck_variables_value_single_line"},
	{name: "a value holding a tab",
		statement: updateVariableA(`value = E'a\tb'`), constraint: "ck_variables_value_single_line"},
	{name: "a value holding U+0001",
		statement: updateVariableA("value = 'a' || chr(1) || 'b'"), constraint: "ck_variables_value_single_line"},
	{name: "a value holding DEL",
		statement: updateVariableA("value = 'a' || chr(127) || 'b'"), constraint: "ck_variables_value_single_line"},
	{name: "a value holding U+0085 (next line)",
		statement: updateVariableA("value = 'a' || chr(133) || 'b'"), constraint: "ck_variables_value_single_line"},
	{name: "a value holding U+009F",
		statement: updateVariableA("value = 'a' || chr(159) || 'b'"), constraint: "ck_variables_value_single_line"},
	{name: "a value holding U+2028 (line separator)",
		statement: updateVariableA("value = 'a' || chr(8232) || 'b'"), constraint: "ck_variables_value_single_line"},
	{name: "a value holding U+2029 (paragraph separator)",
		statement: updateVariableA("value = 'a' || chr(8233) || 'b'"), constraint: "ck_variables_value_single_line"},
	{name: "an override's value of 1,025 characters",
		statement:  newOverride("{orgA}", "{variableGroupA}", "{variableA}", "repeat('v', 1025)"),
		constraint: "ck_variable_group_overrides_value_single_line"},
	{name: "an override's value holding a line feed",
		statement:  newOverride("{orgA}", "{variableGroupA}", "{variableA}", `E'a\nb'`),
		constraint: "ck_variable_group_overrides_value_single_line"},
	{name: "an override's value holding U+2028 (line separator)",
		statement:  newOverride("{orgA}", "{variableGroupA}", "{variableA}", "'a' || chr(8232) || 'b'"),
		constraint: "ck_variable_group_overrides_value_single_line"},
	{name: "a variable's description with a leading space",
		statement: updateVariableA("description = ' x'"), constraint: "ck_variables_description_trimmed_length"},
	{name: "a variable's description with a trailing line break",
		statement: updateVariableA(`description = E'x\n'`), constraint: "ck_variables_description_trimmed_length"},
	{name: "an empty variable description",
		statement: updateVariableA("description = ''"), constraint: "ck_variables_description_trimmed_length"},
	{name: "a variable's description of 1,001 characters",
		statement: updateVariableA("description = repeat('d', 1001)"), constraint: "ck_variables_description_trimmed_length"},
	{name: "a variable group's description with a leading space",
		statement: updateVariableGroupA("description = ' x'"), constraint: "ck_variable_groups_description_trimmed_length"},
	{name: "a variable group's description with a trailing line break",
		statement: updateVariableGroupA(`description = E'x\n'`), constraint: "ck_variable_groups_description_trimmed_length"},
	{name: "a variable group's description of 1,001 characters",
		statement: updateVariableGroupA("description = repeat('d', 1001)"), constraint: "ck_variable_groups_description_trimmed_length"},
	{name: "a variable group's name of a space alone",
		statement: updateVariableGroupA("name = ' '"), constraint: "ck_variable_groups_name_trimmed_length"},
	{name: "an empty variable group name",
		statement: updateVariableGroupA("name = ''"), constraint: "ck_variable_groups_name_trimmed_length"},
	{name: "a variable group's name of 201 characters",
		statement: updateVariableGroupA("name = repeat('g', 201)"), constraint: "ck_variable_groups_name_trimmed_length"},
	{name: "a variable's changer with an id and no email",
		statement: updateVariableA("changed_by_id = gen_random_uuid()"), constraint: "ck_variables_changed_by_both_or_neither"},
	{name: "a variable's changer with an email and no id",
		statement: updateVariableA("changed_by_email = 'ana@acme.io'"), constraint: "ck_variables_changed_by_both_or_neither"},
	{name: "a variable's changer with an email of 321 characters",
		statement:  updateVariableA("changed_by_id = gen_random_uuid(), changed_by_email = repeat('e', 321)"),
		constraint: "ck_variables_changed_by_email_length"},
	{name: "a variable group's changer with an id and no email",
		statement: updateVariableGroupA("changed_by_id = gen_random_uuid()"), constraint: "ck_variable_groups_changed_by_both_or_neither"},
	{name: "a variable group's changer with an email of 321 characters",
		statement:  updateVariableGroupA("changed_by_id = gen_random_uuid(), changed_by_email = repeat('e', 321)"),
		constraint: "ck_variable_groups_changed_by_email_length"},
	{name: "an override's changer with an email and no id",
		setup:     []string{overrideA},
		statement: updateOverrideA("changed_by_email = 'ana@acme.io'"), constraint: "ck_variable_group_overrides_changed_by_both_or_neither"},
	{name: "an override's changer with an email of 321 characters",
		setup:      []string{overrideA},
		statement:  updateOverrideA("changed_by_id = gen_random_uuid(), changed_by_email = repeat('e', 321)"),
		constraint: "ck_variable_group_overrides_changed_by_email_length"},
	{name: "a variable's stamp of 0",
		statement: updateVariableA("stamp = 0"), constraint: "ck_variables_stamp_positive"},
	{name: "a variable's stamp of -1",
		statement: updateVariableA("stamp = -1"), constraint: "ck_variables_stamp_positive"},
	{name: "an override's stamp of 0",
		setup:     []string{overrideA},
		statement: updateOverrideA("stamp = 0"), constraint: "ck_variable_group_overrides_stamp_positive"},
	{name: "a group's variable-group stamp of 0",
		statement:  updateGroupA1("variable_group_id = '{variableGroupA}', variable_group_stamp = 0"),
		constraint: "ck_collector_groups_variable_group_stamp_positive"},
	{name: "a pin's variable-group stamp of 0",
		statement:  updateCollectorA1("pinned_variable_group_id = '{variableGroupA}', pinned_variable_group_stamp = 0"),
		constraint: "ck_collector_instances_pinned_variable_group_stamp_positive"},
	{name: "a served-variables marker of -1",
		statement: updateCollectorA1("served_variables_stamp = -1"), constraint: "ck_collector_instances_served_variables_stamp_non_negative"},
	{name: "a counter of -1",
		statement:  "update telflo_opamp.organizations set variables_revision = -1 where id = '{orgA}'",
		constraint: "ck_organizations_variables_revision_non_negative"},
}

// assertRefusals runs each refusal as the server's role, each in its own
// transaction rolled back after, and checks it fails with its code naming
// its constraint.
func assertRefusals(t *testing.T, pool *pgxpool.Pool, code string, refusals []refusal) {
	t.Helper()
	ctx := context.Background()
	for _, r := range refusals {
		want := r.code
		if want == "" {
			want = code
		}
		t.Run(r.name, func(t *testing.T) {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			for _, statement := range r.setup {
				if _, err := tx.Exec(ctx, fill(statement)); err != nil {
					t.Fatalf("setup %q: %v", statement, err)
				}
			}
			_, err = tx.Exec(ctx, fill(r.statement))
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != want || pgErr.ConstraintName != r.constraint {
				t.Fatalf("err = %v, want %s naming %s", err, want, r.constraint)
			}
		})
	}
}

func TestEveryLinkRefusesAnotherOrganizationsOrFleetsRow(t *testing.T) {
	d := newFleetModel(t)
	assertRefusals(t, d.Runtime, "23503", linkRefusals)
}

func TestDeletingAPublishedVersionOrAGroupInUseIsRefused(t *testing.T) {
	d := newFleetModel(t)
	assertRefusals(t, d.Runtime, "23503", inUseRefusals)
}

func TestEveryUniqueKeyRefusesADuplicate(t *testing.T) {
	d := newFleetModel(t)
	assertRefusals(t, d.Runtime, "23505", uniqueRefusals)
}

func TestEveryCheckRefusesWhatItBounds(t *testing.T) {
	d := newFleetModel(t)
	assertRefusals(t, d.Runtime, "23514", checkRefusals)
}

// What the keys and checks must still let through, as the server's role:
// the same label, name, uid or configuration elsewhere (another fleet,
// another organization, another case), and every bound at its limit.
func TestTheKeysAndChecksAllowWhatTheSpecAllows(t *testing.T) {
	d := newFleetModel(t)
	ctx := context.Background()
	for _, c := range []struct{ name, statement string }{
		{"a second group, not the Default, in a fleet", newGroup("{orgA}", "{fleetA1}", "'edge'", false)},
		{"a second rotate token in a fleet", newToken("gen_random_uuid()", "{orgA}", "{fleetA1}", "rotate", "'{at}'")},
		{"a fleet's label in another organization", newFleet("{orgB}", "'edge-prod'", "server")},
		{"a fleet's label in another case", newFleet("{orgA}", "'Edge-Prod'", "server")},
		{"a group's name in another fleet", newGroup("{orgA}", "{fleetA2}", "'gateways'", false)},
		{"a collector's name and uid in another organization", newCollector("{orgB}", "{fleetB}", "{groupB}", "'{uidA1}'", "'edge-01'")},
		{"a collector's name in another case", newCollector("{orgA}", "{fleetA1}", "{groupA1}", "gen_random_uuid()", "'Edge-01'")},
		{"a collector of its own fleet's group", newCollector("{orgA}", "{fleetA1}", "{groupA1Gateways}", "gen_random_uuid()", "'edge-02'")},
		{"a collector pinned to its organization's published version", updateCollectorA1("pinned_publication_id = '{pubA}'")},
		{"a collector served its organization's published version", updateCollectorA1("served_publication_id = '{pubA}', served_sha256 = repeat('a', 64)")},
		{"a group assigned its organization's published version", updateGroupA1("assigned_publication_id = '{pubA}'")},
		{"a collector cap of zero", "update telflo_opamp.organizations set max_collectors = 0 where id = '{orgA}'"},
		{"one request a minute", "update telflo_opamp.organizations set requests_per_minute = 1 where id = '{orgA}'"},
		{"a fleet label with spaces and a tab inside", newFleet("{orgA}", `E'edge prod\tEU'`, "server")},
		{"a group name with a space inside", newGroup("{orgA}", "{fleetA1}", "'edge gateways'", false)},
		{"a fleet label of 80 characters", newFleet("{orgA}", "repeat('x', 80)", "server")},
		{"a fleet label of 80 two-byte characters", newFleet("{orgA}", "repeat('é', 80)", "kubernetes-supervisor")},
		{"a group name of 80 characters", newGroup("{orgA}", "{fleetA1}", "repeat('g', 80)", false)},
		// An object on any fleet's group, at its bound as jsonb writes it.
		{"annotations of 16 KiB", updateGroupA1(`annotations = jsonb_build_object('telflo', repeat('v', 16384 - 14))`)},
		{"annotations as an empty object", updateGroupA1(`annotations = '{}'`)},
		{"a collector name of 200 characters", newCollector("{orgA}", "{fleetA1}", "{groupA1}", "gen_random_uuid()", "repeat('c', 200)")},
		{"an effective error of 8 KiB", updateCollectorA1("effective_status = 'failed', effective_error = repeat('e', 8192)")},
		{"reported attributes as an object", updateCollectorA1(`reported_attributes = '{"host.name": "edge-01"}'`)},
		{"an error message of 8 KiB", newError("{orgA}", "{collectorA1}", "repeat('m', 8192)")},
		{"a token event with its actor", strings.Replace(newEvent("{orgA}", "{fleetA1}", "{tokenA1Rotate}", "'revoked'"),
			"'revoked', null, null", "'revoked', gen_random_uuid(), 'ana@acme.io'", 1)},
		// Migration 00012.
		{"an empty value", updateVariableA("value = ''")},
		{"a value of 1,024 characters", updateVariableA("value = repeat('v', 1024)")},
		{"a value of 1,024 four-byte characters", updateVariableA("value = repeat(chr(128512), 1024)")},
		{"a value holding a dollar", updateVariableA("value = '$x'")},
		{"a value holding an env reference", updateVariableA("value = '${env:X}'")},
		{"a value holding an org reference", updateVariableA("value = '${org:REGION}'")},
		{"a value outside ASCII", updateVariableA("value = 'é'")},
		{"a value with spaces around and inside it", updateVariableA("value = ' eu west '")},
		{"a value holding U+00A0 (no-break space)", updateVariableA("value = 'a' || chr(160) || 'b'")},
		{"a variable name of 128 lowercase characters", newVariable("gen_random_uuid()", "{orgA}", "'a_' || repeat('b', 126)", "'x'")},
		{"a variable name starting with an underscore", newVariable("gen_random_uuid()", "{orgA}", "'_A1'", "'x'")},
		{"a variable's name in another case", newVariable("gen_random_uuid()", "{orgA}", "'Region'", "'x'")},
		{"a variable's name in another organization", newVariable("gen_random_uuid()", "{orgB}", "'ZONE'", "'x'")},
		{"a variable's description of 1,000 characters", updateVariableA("description = repeat('d', 1000)")},
		{"a variable's description with spaces inside", updateVariableA("description = 'the AWS region'")},
		{"a variable's changer with an email of 320 characters",
			updateVariableA("changed_by_id = gen_random_uuid(), changed_by_email = repeat('e', 320)")},
		{"a variable group's name of 200 characters", updateVariableGroupA("name = repeat('g', 200)")},
		{"a variable group's name in another organization", newVariableGroup("gen_random_uuid()", "{orgB}", "'eu'")},
		{"a variable group's description of 1,000 characters", updateVariableGroupA("description = repeat('d', 1000)")},
		{"a referenced name of 128 characters", newVariableName("{orgA}", "{pubA}", "'a_' || repeat('b', 126)")},
		{"a referenced name in another case", newVariableName("{orgA}", "{pubA}", "'region'")},
		{"an override's empty value", newOverride("{orgA}", "{variableGroupA}", "{variableA}", "''")},
		{"an override's value of 1,024 characters", newOverride("{orgA}", "{variableGroupA}", "{variableA}", "repeat('v', 1024)")},
		{"an override's null value (a reset row)", newOverride("{orgA}", "{variableGroupA}", "{variableA}", "null")},
		{"a group bound to its organization's variable group",
			updateGroupA1("variable_group_id = '{variableGroupA}', variable_group_stamp = 1")},
		{"a group's variable group cleared, its stamp kept",
			updateGroupA1("variable_group_id = null, variable_group_stamp = 2")},
		{"a collector pinned to its organization's variable group",
			updateCollectorA1("pinned_variable_group_id = '{variableGroupA}', pinned_variable_group_stamp = 1")},
		{"a served-variables marker of 0", updateCollectorA1("served_variables_stamp = 0")},
		{"the counter at its largest", "update telflo_opamp.organizations set variables_revision = 9223372036854775807 where id = '{orgA}'"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tx, err := d.Runtime.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, fill(c.statement)); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
}

// A fleet holds at most one valid default token, but a revoked one may be
// followed by a new one, as the server's role; a second valid one is still
// refused (uniqueRefusals).
func TestANewDefaultTokenMayFollowARevokedOne(t *testing.T) {
	d := newFleetModel(t)
	ctx := context.Background()
	tx, err := d.Runtime.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, fill(`update telflo_opamp.enrollment_tokens set revoked_at = '{at}'
		 where organization_id = '{orgA}' and fleet_id = '{fleetA1}' and id = '{tokenA1Default}'`)); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if _, err := tx.Exec(ctx, fill(newToken("gen_random_uuid()", "{orgA}", "{fleetA1}", "default", "null"))); err != nil {
			t.Fatalf("a new default token after a revoke: %v", err)
		}
		if i == 0 {
			if _, err := tx.Exec(ctx, fill(`update telflo_opamp.enrollment_tokens set revoked_at = '{at}'
				 where organization_id = '{orgA}' and fleet_id = '{fleetA1}' and kind = 'default' and revoked_at is null`)); err != nil {
				t.Fatal(err)
			}
		}
	}
	var defaults int
	if err := tx.QueryRow(ctx, fill(`select count(*) from telflo_opamp.enrollment_tokens
		 where organization_id = '{orgA}' and fleet_id = '{fleetA1}' and kind = 'default'`)).Scan(&defaults); err != nil {
		t.Fatal(err)
	}
	if defaults != 3 {
		t.Fatalf("%d default tokens, want 3 (two revoked, one valid)", defaults)
	}
}

// Migration 00010 swaps 00008's index (one default token per fleet ever)
// for one over the valid default tokens only, keeping every row; its down
// restores 00008's index while no fleet holds a revoked default token and a
// new one, and fails (23505, naming 00008's index, changing nothing) once
// one does; up again narrows it again.
func TestMigration10NarrowsTheDefaultTokenIndexToValidTokens(t *testing.T) {
	d := databasetest.New(t, testDatabase, databasetest.Unmigrated())
	ctx := context.Background()
	migrator, err := database.OpenMigrator(ctx, d.OwnerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = migrator.Close() }()
	if _, err := database.MigrateUpTo(ctx, migrator, 9); err != nil {
		t.Fatalf("up to 9: %v", err)
	}
	if _, err := d.Owner.Exec(ctx, fill(fixture)); err != nil {
		t.Fatalf("the fixture: %v", err)
	}
	const (
		everIndex = "uq_enrollment_tokens_fleet_id_where_kind_default"
		everDef   = "CREATE UNIQUE INDEX " + everIndex + " ON telflo_opamp.enrollment_tokens USING btree " +
			"(organization_id, fleet_id) WHERE (kind = 'default'::text)"
		validIndex = "uq_enrollment_tokens_fleet_id_where_default_not_revoked"
		validDef   = "CREATE UNIQUE INDEX " + validIndex + " ON telflo_opamp.enrollment_tokens USING btree " +
			"(organization_id, fleet_id) WHERE ((kind = 'default'::text) AND (revoked_at IS NULL))"
	)
	defaultTokenIndexes := func() []string {
		t.Helper()
		rows, err := d.Admin.Query(ctx, `
			select indexdef from pg_indexes
			 where schemaname = 'telflo_opamp' and tablename = 'enrollment_tokens'
			   and indexname like 'uq_enrollment_tokens_fleet_id_where_%'
			 order by indexname`)
		if err != nil {
			t.Fatal(err)
		}
		defs, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return defs
	}
	if got := defaultTokenIndexes(); !slices.Equal(got, []string{everDef}) {
		t.Fatalf("at 9, the default token's indexes are %v, want 00008's", got)
	}
	before := tableRows(t, d.Admin, "enrollment_tokens")
	revokeA1 := fill(`update telflo_opamp.enrollment_tokens set revoked_at = '{at}'
		 where organization_id = '{orgA}' and fleet_id = '{fleetA1}' and kind = 'default' and revoked_at is null`)
	secondDefault := fill(newToken("gen_random_uuid()", "{orgA}", "{fleetA1}", "default", "null"))
	refusedBy := func(err error, index string) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == index
	}

	for round := 1; round <= 2; round++ {
		if _, err := database.MigrateUpTo(ctx, migrator, 10); err != nil {
			t.Fatalf("round %d: up to 10: %v", round, err)
		}
		if got := defaultTokenIndexes(); !slices.Equal(got, []string{validDef}) {
			t.Fatalf("round %d: at 10, the default token's indexes are %v, want only %s", round, got, validDef)
		}
		if got := tableRows(t, d.Admin, "enrollment_tokens"); !slices.Equal(got, before) {
			t.Fatalf("round %d: up changed the tokens:\n before %v\n after  %v", round, before, got)
		}
		var comment string
		if err := d.Admin.QueryRow(ctx, `select coalesce(obj_description(('telflo_opamp.' || $1)::regclass, 'pg_class'), '')`,
			validIndex).Scan(&comment); err != nil || !strings.Contains(comment, "at most one valid default token per fleet") {
			t.Fatalf("round %d: the index's comment is %q, %v", round, comment, err)
		}
		// A second valid default token is still refused, as the server's role.
		if _, err := d.Runtime.Exec(ctx, secondDefault); !refusedBy(err, validIndex) {
			t.Fatalf("round %d: a second valid default token: %v, want 23505 on %s", round, err, validIndex)
		}
		// Revoked, it may be followed by a new one; and then down can't go.
		if _, err := d.Runtime.Exec(ctx, revokeA1); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Runtime.Exec(ctx, secondDefault); err != nil {
			t.Fatalf("round %d: a new default token after the revoke: %v", round, err)
		}
		afterIssue := tableRows(t, d.Admin, "enrollment_tokens")
		if _, err := migrator.Down(ctx); !refusedBy(err, everIndex) {
			t.Fatalf("round %d: down with a revoked and a new default token: %v, want 23505 on %s", round, err, everIndex)
		}
		assertStatus(t, migrator, 10)
		if got := defaultTokenIndexes(); !slices.Equal(got, []string{validDef}) {
			t.Fatalf("round %d: the refused down left the indexes %v", round, got)
		}
		if got := tableRows(t, d.Admin, "enrollment_tokens"); !slices.Equal(got, afterIssue) {
			t.Fatalf("round %d: the refused down changed the tokens", round)
		}

		// Put the fixture back by hand (the owner may delete a token), and
		// down restores 00008's index.
		if _, err := d.Owner.Exec(ctx, fill(`delete from telflo_opamp.enrollment_tokens
			 where organization_id = '{orgA}' and fleet_id = '{fleetA1}' and kind = 'default' and id <> '{tokenA1Default}'`)); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Owner.Exec(ctx, fill(`update telflo_opamp.enrollment_tokens set revoked_at = null
			 where organization_id = '{orgA}' and id = '{tokenA1Default}'`)); err != nil {
			t.Fatal(err)
		}
		result, err := migrator.Down(ctx)
		if err != nil || result.Source.Version != 10 {
			t.Fatalf("round %d: down = %v, %v; want 10 rolled back", round, result, err)
		}
		assertStatus(t, migrator, 9)
		if got := defaultTokenIndexes(); !slices.Equal(got, []string{everDef}) {
			t.Fatalf("round %d: after down, the default token's indexes are %v, want 00008's", round, got)
		}
		if got := tableRows(t, d.Admin, "enrollment_tokens"); !slices.Equal(got, before) {
			t.Fatalf("round %d: down changed the tokens", round)
		}
		if got := runtimeGrants(t, d.Admin, database.RuntimeRole); !sameGrants(got, grantsAt(9)) {
			t.Fatalf("round %d: after down, %s holds %v", round, database.RuntimeRole, got)
		}
		// 00008's index refuses a new default token even after a revoke.
		if _, err := d.Runtime.Exec(ctx, revokeA1); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Runtime.Exec(ctx, secondDefault); !refusedBy(err, everIndex) {
			t.Fatalf("round %d: at 9, a default token after a revoke: %v, want 23505 on %s", round, err, everIndex)
		}
		if _, err := d.Owner.Exec(ctx, fill(`update telflo_opamp.enrollment_tokens set revoked_at = null
			 where organization_id = '{orgA}' and id = '{tokenA1Default}'`)); err != nil {
			t.Fatal(err)
		}
	}
}

// dropConstraint drops the constraint or index name in telflo_opamp, inside
// tx.
func dropConstraint(t *testing.T, tx pgx.Tx, name string) {
	t.Helper()
	ctx := context.Background()
	var table string
	err := tx.QueryRow(ctx, `
		select c.conrelid::regclass::text from pg_constraint c
		 where c.conname = $1 and c.connamespace = 'telflo_opamp'::regnamespace`, name).Scan(&table)
	switch {
	case err == nil:
		_, err = tx.Exec(ctx, "alter table "+table+" drop constraint "+pgx.Identifier{name}.Sanitize())
	case errors.Is(err, pgx.ErrNoRows):
		_, err = tx.Exec(ctx, "drop index "+pgx.Identifier{database.Schema, name}.Sanitize())
	}
	if err != nil {
		t.Fatalf("dropping %s: %v", name, err)
	}
}

// The deliberate break, run every time (the permanent detector): with its
// constraint dropped, each refusal above goes through as the server's role,
// so each test above fails when the key, unique index or check it names is
// missing.
func TestEachRefusalNeedsItsConstraint(t *testing.T) {
	d := newFleetModel(t)
	ctx := context.Background()
	for _, r := range slices.Concat(linkRefusals, inUseRefusals, uniqueRefusals, checkRefusals) {
		t.Run(r.name, func(t *testing.T) {
			tx, err := d.Admin.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			drop := r.drop
			if drop == nil {
				drop = []string{r.constraint}
			}
			for _, name := range drop {
				dropConstraint(t, tx, name)
			}
			if _, err := tx.Exec(ctx, "set local role "+database.ServerRole); err != nil {
				t.Fatal(err)
			}
			for _, statement := range r.setup {
				if _, err := tx.Exec(ctx, fill(statement)); err != nil {
					t.Fatalf("setup %q: %v", statement, err)
				}
			}
			if _, err := tx.Exec(ctx, fill(r.statement)); err != nil {
				t.Fatalf("without %v, still refused: %v", drop, err)
			}
		})
	}
}

// A pointer to what a collector was served (or the token it used) is
// cleared alone when its target goes: on delete set null of that column
// (Postgres 15's column list), so organization_id, fleet_id and
// served_sha256 stay. A published version is deleted by the server; a token
// only ever goes with its fleet, so the owner deletes it here.
var pointerDeletes = []struct {
	name, constraint, column, setup, role, statement string
}{
	{"a published version it was served", "fk_collector_instances_served_publication", "served_publication_id",
		"served_publication_id = '{pubA}', served_sha256 = repeat('a', 64), served_at = '{at}'", database.ServerRole,
		"delete from telflo_opamp.publications where organization_id = '{orgA}' and id = '{pubA}'"},
	{"the token it used", "fk_collector_instances_enrollment_token", "enrollment_token_id",
		"served_sha256 = repeat('a', 64)", database.OwnerRole,
		"delete from telflo_opamp.enrollment_tokens where organization_id = '{orgA}' and id = '{tokenA1Rotate}'"},
}

func TestDeletingWhatACollectorWasServedClearsOnlyThatPointer(t *testing.T) {
	d := newFleetModel(t)
	ctx := context.Background()
	for _, c := range pointerDeletes {
		t.Run(c.name, func(t *testing.T) {
			tx, err := d.Admin.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, "set local role "+c.role); err != nil {
				t.Fatal(err)
			}
			for _, statement := range []string{updateCollectorA1(c.setup), c.statement} {
				if _, err := tx.Exec(ctx, fill(statement)); err != nil {
					t.Fatalf("%q: %v", statement, err)
				}
			}
			var organization, fleet, sha256 string
			var cleared bool
			if err := tx.QueryRow(ctx, `
				select organization_id::text, fleet_id::text, served_sha256, `+c.column+` is null
				  from telflo_opamp.collector_instances where id = $1`, collectorA1,
			).Scan(&organization, &fleet, &sha256, &cleared); err != nil {
				t.Fatal(err)
			}
			if !cleared || organization != orgA || fleet != fleetA1 || len(sha256) != 64 {
				t.Fatalf("after the delete: %s cleared %v, organization %s, fleet %s, served_sha256 %q; "+
					"want only %s cleared", c.column, cleared, organization, fleet, sha256, c.column)
			}
		})
	}
}

// The deliberate break, run every time: each key above with a plain on
// delete set null, which nulls every column of the key, organization_id
// among them, is refused (23502): the column list is what the test above
// relies on.
func TestThePointerCheckSeesASetNullOfEveryColumn(t *testing.T) {
	d := newFleetModel(t)
	ctx := context.Background()
	for _, c := range pointerDeletes {
		t.Run(c.name, func(t *testing.T) {
			tx, err := d.Admin.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			everyColumn := strings.TrimSuffix(wantForeignKeys[c.constraint], " ("+c.column+")")
			if everyColumn == wantForeignKeys[c.constraint] {
				t.Fatalf("%s isn't a set null of %s alone: %s", c.constraint, c.column, everyColumn)
			}
			if _, err := tx.Exec(ctx, "alter table telflo_opamp.collector_instances drop constraint "+c.constraint+
				", add constraint "+c.constraint+" "+everyColumn); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, "set local role "+c.role); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, fill(updateCollectorA1(c.setup))); err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(ctx, fill(c.statement))
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23502" || pgErr.ColumnName != "organization_id" {
				t.Fatalf("with every column set null, err = %v; want 23502 on organization_id", err)
			}
		})
	}
}

// linkEverything links each organization's rows every way the model allows,
// so an organization's delete meets every key: linkPublications, then
// linkVariables, then linkAPIKeys.
const linkEverything = linkPublications + linkVariables + linkAPIKeys

// linkAPIKeys is a key limited to both organizations (migration 00013): an
// organization's delete takes its link and leaves the key, the server's own
// row.
const linkAPIKeys = `
insert into telflo_opamp.api_keys (id, name, description, secret_sha256, all_organizations, expires_at,
  revoked_at, last_used_at, created_at) values
  ('{apiKey}', 'deploy pipeline', null, sha256('secret'), false, '{at}'::timestamptz + interval '1 day',
   null, null, '{at}');
insert into telflo_opamp.api_key_organizations (organization_id, api_key_id) values
  ('{orgA}', '{apiKey}'), ('{orgB}', '{apiKey}');
`

// linkPublications is the links a schema before 00012 holds: a group
// assigned a published version, a collector pinned to and served it, token
// events and error rows.
const linkPublications = `
update telflo_opamp.collector_groups set assigned_publication_id = '{pubA}'
 where organization_id = '{orgA}' and id = '{groupA1}';
update telflo_opamp.collector_instances
   set pinned_publication_id = '{pubA}', served_publication_id = '{pubA}', served_sha256 = repeat('a', 64)
 where organization_id = '{orgA}' and id = '{collectorA1}';
insert into telflo_opamp.collector_instances (id, organization_id, fleet_id, collector_group_id,
  instance_uid, name, served_sha256, effective_sha256,
  effective_status, enrollment_token_id, last_seen_at, created_at, updated_at) values
  (gen_random_uuid(), '{orgA}', '{fleetA1}', '{groupA1Gateways}', gen_random_uuid(), 'edge-02',
   repeat('d', 64), repeat('d', 64), 'applied', '{tokenA1Default}', '{at}', '{at}', '{at}');
insert into telflo_opamp.enrollment_token_events (id, organization_id, fleet_id, enrollment_token_id,
  event, actor_id, actor_email, occurred_at) values
  (gen_random_uuid(), '{orgA}', '{fleetA1}', '{tokenA1Default}', 'issued', null, null, '{at}'),
  (gen_random_uuid(), '{orgB}', '{fleetB}', '{tokenBDefault}', 'issued', null, null, '{at}');
insert into telflo_opamp.collector_errors (id, organization_id, collector_instance_id, reported_at,
  config_sha256, message) values
  (gen_random_uuid(), '{orgA}', '{collectorA1}', '{at}', repeat('a', 64), 'failed'),
  (gen_random_uuid(), '{orgB}', '{collectorB}', '{at}', repeat('b', 64), 'failed');
update telflo_opamp.collector_groups set assigned_publication_id = '{pubB}'
 where organization_id = '{orgB}' and id = '{groupB}';
update telflo_opamp.collector_instances set pinned_publication_id = '{pubB}', served_publication_id = '{pubB}'
 where organization_id = '{orgB}' and id = '{collectorB}';
`

// linkVariables is the variables' links (migration 00012), over the
// variables' fixture: a second variable of A's, overrides (one of A's a
// reset row), the names each published version references, a group bound to
// a variable group and a collector pinned to one, with their stamps, the
// marker and the counter.
const linkVariables = `
insert into telflo_opamp.variables (id, organization_id, name, description, value, changed_by_id,
  changed_by_email, changed_at, stamp) values
  ('{variableA2}', '{orgA}', 'TIER', 'the service tier', 'gold', gen_random_uuid(), 'ana@acme.io', '{at}', 3);
insert into telflo_opamp.variable_group_overrides (organization_id, variable_group_id, variable_id, value,
  changed_by_id, changed_by_email, changed_at, stamp) values
  ('{orgA}', '{variableGroupA}', '{variableA}', 'eu-central-1', null, null, '{at}', 4),
  ('{orgA}', '{variableGroupA}', '{variableA2}', null, null, null, '{at}', 5),
  ('{orgB}', '{variableGroupB}', '{variableB}', 'us-west-2', null, null, '{at}', 2);
insert into telflo_opamp.publication_variable_names (organization_id, publication_id, name) values
  ('{orgA}', '{pubA}', 'REGION'), ('{orgA}', '{pubA}', 'TIER'), ('{orgB}', '{pubB}', 'REGION');
update telflo_opamp.collector_groups set variable_group_id = '{variableGroupA}', variable_group_stamp = 6
 where organization_id = '{orgA}' and id = '{groupA1}';
update telflo_opamp.collector_instances
   set pinned_variable_group_id = '{variableGroupA}', pinned_variable_group_stamp = 7, served_variables_stamp = 7
 where organization_id = '{orgA}' and id = '{collectorA1}';
update telflo_opamp.collector_groups set variable_group_id = '{variableGroupB}', variable_group_stamp = 3
 where organization_id = '{orgB}' and id = '{groupB}';
update telflo_opamp.collector_instances
   set pinned_variable_group_id = '{variableGroupB}', pinned_variable_group_stamp = 3, served_variables_stamp = 3
 where organization_id = '{orgB}' and id = '{collectorB}';
update telflo_opamp.organizations set variables_revision = 7 where id = '{orgA}';
update telflo_opamp.organizations set variables_revision = 3 where id = '{orgB}';
`

// organizationRowsByTable answers, for every table in telflo_opamp but
// goose's and the server's own, the rows of organization as text, in order: its own row in
// organizations, and every row naming it elsewhere. A table without
// organization_id fails the test.
func organizationRowsByTable(t *testing.T, q *pgxpool.Pool, organization string) map[string][]string {
	t.Helper()
	ctx := context.Background()
	rows, err := q.Query(ctx, `
		select c.relname,
		       exists (select 1 from pg_attribute a
		                where a.attrelid = c.oid and a.attname = 'organization_id' and not a.attisdropped)
		  from pg_class c
		 where c.relnamespace = 'telflo_opamp'::regnamespace and c.relkind = 'r'
		   and c.relname not in ('goose_db_version', '`+database.ServerTable+`')
		 order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	type table struct {
		name              string
		hasOrganizationID bool
	}
	tables, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (table, error) {
		var tb table
		err := row.Scan(&tb.name, &tb.hasOrganizationID)
		return tb, err
	})
	if err != nil {
		t.Fatal(err)
	}
	byTable := map[string][]string{}
	for _, tb := range tables {
		column := "organization_id"
		if tb.name == "organizations" {
			column = "id"
		} else if !tb.hasOrganizationID {
			t.Fatalf("telflo_opamp.%s has no organization_id", tb.name)
		}
		rows, err := q.Query(ctx, "select r::text from telflo_opamp."+pgx.Identifier{tb.name}.Sanitize()+
			" r where "+column+" = $1 order by 1", organization)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		byTable[tb.name] = got
	}
	return byTable
}

// querier is what forceAdversarialCascadeOrder runs its statements on: a
// pool or a transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// The keys onto organizations whose cascade, fired first, meets a NO ACTION
// check: the publications' (a group's assignment, a pin) and the variable
// groups' (a group's and a pin's variable group).
const (
	publicationsFirst   = "fk_publications_organization"
	variableGroupsFirst = "fk_variable_groups_organization"
)

// forceAdversarialCascadeOrder recreates every foreign key onto
// organizations, first first, so that its table's cascade fires before
// every other table's: with publicationsFirst, the order that breaks the
// one-statement delete when the NO ACTION keys stand without each table's
// own cascade. Postgres fires a table's triggers in the order of their
// names, RI_ConstraintTrigger_a_<oid> compared as text, so the order would
// otherwise be the OIDs' luck. It checks the order it made, and tries again
// (at most three times) should the OIDs have crossed into another number of
// digits.
func forceAdversarialCascadeOrder(t *testing.T, q querier, first string) {
	t.Helper()
	ctx := context.Background()
	rows, err := q.Query(ctx, `
		select c.conname, c.conrelid::regclass::text, pg_get_constraintdef(c.oid)
		  from pg_constraint c
		 where c.contype = 'f' and c.confrelid = 'telflo_opamp.organizations'::regclass
		 order by c.conname <> $1, c.conname`, first)
	if err != nil {
		t.Fatal(err)
	}
	type key struct{ name, table, definition string }
	keys, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (key, error) {
		var k key
		err := row.Scan(&k.name, &k.table, &k.definition)
		return k, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) < 2 || keys[0].name != first {
		t.Fatalf("the keys onto organizations: %v", keys)
	}
	var order []string
	for range 3 {
		for _, k := range keys {
			if _, err := q.Exec(ctx, "alter table "+k.table+" drop constraint "+k.name+
				", add constraint "+k.name+" "+k.definition); err != nil {
				t.Fatalf("recreating %s: %v", k.name, err)
			}
		}
		rows, err := q.Query(ctx, `
			select c.conname
			  from pg_trigger t join pg_constraint c on c.oid = t.tgconstraint
			 where t.tgrelid = 'telflo_opamp.organizations'::regclass
			   and t.tgfoid = '"RI_FKey_cascade_del"'::regproc
			 order by t.tgname collate "C"`)
		if err != nil {
			t.Fatal(err)
		}
		order, err = pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		if len(order) == len(keys) && order[0] == first {
			return
		}
	}
	t.Fatalf("the cascades onto organizations fire in the order %v, want %s first", order, first)
}

// The one-statement delete (store/organizations' Delete, which the
// platform's hook calls) removes an organization with everything it holds,
// every key above in play, the published versions and variable groups in
// use included: each table's own cascade from its organization runs before
// any NO ACTION check, even in the orders that need them (forced): the
// publications' cascade first, then the variable groups'. The other
// organization keeps every row.
func TestDeletingAnOrganizationRemovesEveryRowItOwns(t *testing.T) {
	for _, first := range []string{publicationsFirst, variableGroupsFirst} {
		t.Run(first+" first", func(t *testing.T) {
			d := newFleetModel(t)
			ctx := context.Background()
			if _, err := d.Owner.Exec(ctx, fill(linkEverything)); err != nil {
				t.Fatal(err)
			}
			forceAdversarialCascadeOrder(t, d.Admin, first)
			before := organizationRowsByTable(t, d.Admin, orgA)
			wantTables := []string{"api_key_organizations", "collector_errors", "collector_groups", "collector_instances", "enrollment_token_events",
				"enrollment_tokens", "fleets", "organizations", "publication_variable_names", "publications",
				"variable_group_overrides", "variable_groups", "variables"}
			for _, table := range wantTables {
				if len(before[table]) == 0 {
					t.Fatalf("the fixture holds no row of A's in %s", table)
				}
			}
			keptB := organizationRowsByTable(t, d.Admin, orgB)

			deleted, err := organizations.NewPostgres(d.Runtime).Delete(ctx, uuid.MustParse(orgA))
			if err != nil || !deleted {
				t.Fatalf("Delete = %v, %v; want deleted", deleted, err)
			}

			after := organizationRowsByTable(t, d.Admin, orgA)
			tables := make([]string, 0, len(after))
			for table, rows := range after {
				tables = append(tables, table)
				if len(rows) != 0 {
					t.Errorf("%s still holds %d of A's rows", table, len(rows))
				}
			}
			slices.Sort(tables)
			if !slices.Equal(tables, wantTables) {
				t.Errorf("checked the tables %v, want %v", tables, wantTables)
			}
			for table, rows := range organizationRowsByTable(t, d.Admin, orgB) {
				if !slices.Equal(rows, keptB[table]) {
					t.Errorf("B's rows in %s changed:\n before %v\n after  %v", table, keptB[table], rows)
				}
			}
			var keys int
			if err := d.Admin.QueryRow(ctx, "select count(*) from telflo_opamp.api_keys").Scan(&keys); err != nil || keys != 1 {
				t.Errorf("%d keys after A's delete, %v; want the key kept", keys, err)
			}
		})
	}
}

// The deliberate break, run every time: without the groups' and the
// collectors' own cascades from their organization (the two tables whose
// NO ACTION keys name published versions and variable groups), the
// cascade forced to fire first (the publications', then the variable
// groups') queues its check before the fleets' cascade has removed the
// group, and the check finds it still there, so the organization's delete
// fails. The cascades are what the test above relies on.
func TestTheOrganizationsDeleteNeedsEveryTablesOwnCascade(t *testing.T) {
	for _, c := range []struct {
		first string
		keys  []string
	}{
		{publicationsFirst, []string{"fk_collector_groups_assigned_publication", "fk_collector_instances_pinned_publication"}},
		{variableGroupsFirst, []string{"fk_collector_groups_assigned_publication", "fk_collector_instances_pinned_publication",
			"fk_collector_groups_variable_group", "fk_collector_instances_pinned_variable_group"}},
	} {
		t.Run(c.first+" first", func(t *testing.T) {
			d := newFleetModel(t)
			ctx := context.Background()
			if _, err := d.Owner.Exec(ctx, fill(linkEverything)); err != nil {
				t.Fatal(err)
			}
			tx, err := d.Admin.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			dropConstraint(t, tx, "fk_collector_groups_organization")
			dropConstraint(t, tx, "fk_collector_instances_organization")
			forceAdversarialCascadeOrder(t, tx, c.first)
			if _, err := tx.Exec(ctx, "set local role "+database.ServerRole); err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(ctx, "delete from telflo_opamp.organizations where id = $1", orgA)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23503" || !slices.Contains(c.keys, pgErr.ConstraintName) {
				t.Fatalf("without the direct cascades, the delete = %v; want 23503 naming one of %v", err, c.keys)
			}
		})
	}
}

// Deleting a variable deletes its overrides, a variable group its
// overrides, reset rows included, and a published version the names it
// references, by the cascades, as the server's role, which has no delete on
// the names. The other organization keeps its rows.
func TestDeletingAVariableOrAGroupDeletesItsOverridesAndAPublicationItsNames(t *testing.T) {
	d := newFleetModel(t)
	ctx := context.Background()
	tx, err := d.Runtime.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, statement := range []string{
		newVariable("'{variableA2}'", "{orgA}", "'TIER'", "'gold'"),
		newVariableGroup("'{variableGroupA2}'", "{orgA}", "'eu-2'"),
		newOverride("{orgA}", "{variableGroupA}", "{variableA}", "'eu-central-1'"),
		newOverride("{orgA}", "{variableGroupA}", "{variableA2}", "null"),
		newOverride("{orgA}", "{variableGroupA2}", "{variableA}", "null"),
		newOverride("{orgA}", "{variableGroupA2}", "{variableA2}", "'silver'"),
		newOverride("{orgB}", "{variableGroupB}", "{variableB}", "'us-west-2'"),
		newVariableName("{orgA}", "{pubA}", "'REGION'"),
		newVariableName("{orgA}", "{pubA}", "'TIER'"),
		newVariableName("{orgB}", "{pubB}", "'REGION'"),
	} {
		if _, err := tx.Exec(ctx, fill(statement)); err != nil {
			t.Fatalf("%q: %v", statement, err)
		}
	}
	rowsOf := func(query string) []string {
		t.Helper()
		rows, err := tx.Query(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	overrides := func() []string {
		t.Helper()
		return rowsOf(`select variable_group_id::text || ' ' || variable_id::text || ' ' || coalesce(value, '(reset)')
			from telflo_opamp.variable_group_overrides order by 1`)
	}
	names := func() []string {
		t.Helper()
		return rowsOf(`select publication_id::text || ' ' || name from telflo_opamp.publication_variable_names order by 1`)
	}
	sorted := func(rows ...string) []string { return slices.Sorted(slices.Values(rows)) }
	exec := func(statement string) {
		t.Helper()
		tag, err := tx.Exec(ctx, fill(statement))
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("%q = %v, %v; want one row", statement, tag, err)
		}
	}

	exec("delete from telflo_opamp.variables where organization_id = '{orgA}' and id = '{variableA}'")
	if got, want := overrides(), sorted(
		variableGroupA+" "+variableA2+" (reset)",
		variableGroupA2+" "+variableA2+" silver",
		variableGroupB+" "+variableB+" us-west-2",
	); !slices.Equal(got, want) {
		t.Fatalf("after the variable's delete, the overrides are %v, want %v", got, want)
	}

	exec("delete from telflo_opamp.variable_groups where organization_id = '{orgA}' and id = '{variableGroupA}'")
	if got, want := overrides(), sorted(
		variableGroupA2+" "+variableA2+" silver",
		variableGroupB+" "+variableB+" us-west-2",
	); !slices.Equal(got, want) {
		t.Fatalf("after the variable group's delete, the overrides are %v, want %v", got, want)
	}

	exec("delete from telflo_opamp.publications where organization_id = '{orgA}' and id = '{pubA}'")
	if got, want := names(), []string{pubB + " REGION"}; !slices.Equal(got, want) {
		t.Fatalf("after the published version's delete, the names are %v, want %v", got, want)
	}
}

// The uncommented objects: 00001 and 00002 commented their tables, columns,
// index and foreign key, not their primary keys or checks. Every object
// since has a comment.
var uncommentedIn00001And00002 = []string{
	"constraint organizations.ck_organizations_name_length",
	"constraint organizations.organizations_pkey",
	"constraint publications.ck_publications_configuration_name_length",
	"constraint publications.ck_publications_published_by_both_or_neither",
	"constraint publications.ck_publications_sha256_hex",
	"constraint publications.ck_publications_version_label_length",
	"constraint publications.ck_publications_version_number_positive",
	"constraint publications.publications_pkey",
}

// Every table, column, index and constraint says why it is there.
func TestEveryTableColumnIndexAndConstraintHasAComment(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	rows, err := d.Admin.Query(context.Background(), `
		select 'table ' || c.relname
		  from pg_class c
		 where c.relnamespace = 'telflo_opamp'::regnamespace and c.relkind = 'r'
		   and c.relname <> 'goose_db_version' and obj_description(c.oid, 'pg_class') is null
		union all
		select 'column ' || c.relname || '.' || a.attname
		  from pg_class c join pg_attribute a on a.attrelid = c.oid
		 where c.relnamespace = 'telflo_opamp'::regnamespace and c.relkind = 'r'
		   and c.relname <> 'goose_db_version' and a.attnum > 0 and not a.attisdropped
		   and col_description(c.oid, a.attnum) is null
		union all
		select 'index ' || i.relname
		  from pg_index x join pg_class i on i.oid = x.indexrelid join pg_class c on c.oid = x.indrelid
		 where i.relnamespace = 'telflo_opamp'::regnamespace and c.relname <> 'goose_db_version'
		   and not exists (select 1 from pg_constraint k where k.conindid = i.oid and k.conrelid = c.oid)
		   and obj_description(i.oid, 'pg_class') is null
		union all
		select 'constraint ' || c.relname || '.' || k.conname
		  from pg_constraint k join pg_class c on c.oid = k.conrelid
		 where k.connamespace = 'telflo_opamp'::regnamespace and c.relname <> 'goose_db_version'
		   and k.contype <> 'n' and obj_description(k.oid, 'pg_constraint') is null
		order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(missing, uncommentedIn00001And00002) {
		t.Fatalf("objects without a comment:\n  %s\nwant only 00001's and 00002's:\n  %s",
			strings.Join(missing, "\n  "), strings.Join(uncommentedIn00001And00002, "\n  "))
	}
}

// CONTRIBUTING.md, "SQL": every index on an organization's rows leads with
// organization_id (each table's primary key on id alone aside: a row's id
// is unique on its own; and the server's own table, of no organization). A
// name says the columns after it, in at most 63 bytes.
func TestEveryIndexLeadsWithTheOrganization(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	rows, err := d.Admin.Query(context.Background(), `
		select i.relname || ' on ' || c.relname || ' leads with ' || a.attname
		  from pg_index x
		  join pg_class i on i.oid = x.indexrelid
		  join pg_class c on c.oid = x.indrelid
		  join pg_attribute a on a.attrelid = c.oid and a.attnum = x.indkey[0]
		 where c.relnamespace = 'telflo_opamp'::regnamespace
		   and c.relname not in ('goose_db_version', 'organizations', '`+database.ServerTable+`')
		   and not x.indisprimary and a.attname <> 'organization_id'
		order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	notLed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(notLed) != 0 {
		t.Fatalf("indexes not led by organization_id: %v", notLed)
	}
}
