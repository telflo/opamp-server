package management

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
)

// variableConformanceCases are the conformance cases of the organization
// variables' routes and of the fleet routes that take a variable group:
// every (method, path, status) api/openapi.yaml documents for each, and
// one case for each code a documented status names, on the Postgres
// stores. 500s come from a registry that fails.
func variableConformanceCases() []conformanceCase {
	tooLarge := []byte(`{"name":"` + strings.Repeat("n", MaxBodyBytes) + `"}`)
	post, get, patch, put, del := http.MethodPost, http.MethodGet, http.MethodPatch, http.MethodPut, http.MethodDelete

	withVariable := seedVariable(orgA, variableA, "REGION", "eu-west-1")
	withGroup := seedVariableGroup(orgA, variableGroupA, "production")
	withOverride := seeds(withVariable, withGroup, seedOverride(orgA, variableGroupA, variableA, "eu-central-1"))
	withFleet := seedFleet(orgA, fleetA, "edge-prod", "server")
	// A published version referencing ${org:REGION}, assigned to a group of
	// fleet A: the variable is in use.
	inUse := seeds(withVariable, withFleet, seedPublication(orgA, publicationA, referencing("REGION")),
		seedAssignedGroup(orgA, fleetA, "gateways", publicationA))
	// A published version referencing a name no variable has.
	withUndefined := seeds(withFleet, seedCollector(orgA, fleetA, "edge-01"), seedPublication(orgA, publicationA, referencing("API_KEY_HEADER")))
	// The variable group bound to a collector group of fleet A.
	bound := seeds(withFleet, seedBoundGroup(orgA, fleetA, "gateways", variableGroupA))
	groupInUse := seeds(withGroup, bound)
	withCollector := seeds(withFleet, seedCollector(orgA, fleetA, "edge-01"))
	edge01 := collectorTargetOf(orgA, fleetA, "edge-01", collectorTarget)
	gateways := groupTargetOf(orgA, fleetA, "gateways")
	defaultGroup := groupTargetOf(orgA, fleetA, "Default")
	bulkOf := func(fields string) func(t *testing.T, s *Server) []byte {
		return func(t *testing.T, s *Server) []byte {
			return []byte(`{"collectorIds":["` + collectorID(t, s, orgA, fleetA, "edge-01") + `"],` + fields + `}`)
		}
	}
	pinA := `{"kind":"publication","id":"` + publicationA + `"}`

	created := variableBody(variableA, "REGION", "eu-west-1").bytes()
	createdGroup := variableGroupBody(variableGroupA, "production").bytes()
	rename := body{"name": "AWS_REGION", "actor": actorJSONBody()}.bytes()
	override := body{"value": "eu-central-1", "actor": actorJSONBody()}.bytes()
	resetBody := body{"actor": actorJSONBody()}.bytes()

	return []conformanceCase{
		// GET, POST …/variables.
		{name: "list variables", method: get, target: variablesTarget(orgA), seed: withOverride, status: 200, conforms: true},
		{name: "list variables, an invalid organization id", method: get, target: variablesTarget("x"), status: 400, conforms: false},
		{name: "list variables without the secret", method: get, target: variablesTarget(orgA), unauthenticated: true, status: 401, conforms: false},
		{name: "list variables under an unregistered organization", method: get, target: variablesTarget(orgC), status: 404, conforms: true},
		{name: "list variables registry down", method: get, target: variablesTarget(orgA), failingOrganizations: true, status: 500, conforms: true},

		{name: "create variable", method: post, target: variablesTarget(orgA), contentType: jsonType, payload: created, status: 201, conforms: true},
		{name: "create variable without an id, with a description, an empty value", method: post, target: variablesTarget(orgA), contentType: jsonType,
			payload: body{"name": "_lower_ok", "value": "", "description": "Where it runs", "actor": nil}.bytes(), status: 201, conforms: true},
		{name: "create variable, a value of 1024 characters, two bytes each, with $", method: post, target: variablesTarget(orgA),
			contentType: jsonType, payload: variableBody(variableA, "LONG", strings.Repeat("$"+string(rune(0xe9)), 512)).bytes(), status: 201, conforms: true},
		{name: "create variable, the same again", method: post, target: variablesTarget(orgA), contentType: jsonType, payload: created,
			seed: withVariable, status: 200, conforms: true},
		{name: "create variable, the id taken otherwise", method: post, target: variablesTarget(orgA), contentType: jsonType,
			payload: variableBody(variableA, "REGION", "us-east-1").bytes(), seed: withVariable, status: 409, conforms: true},
		{name: "create variable, the name taken", method: post, target: variablesTarget(orgA), contentType: jsonType,
			payload: variableBody(variableA2, "REGION", "us-east-1").bytes(), seed: withVariable, status: 409, conforms: true},
		{name: "create variable, the 151st", method: post, target: variablesTarget(orgA), contentType: jsonType,
			payload: variableBody(variableA2, "ONE_TOO_MANY", "x").bytes(), seed: seedVariables(orgA, 150), status: 409, conforms: true},
		{name: "create variable, a name with a hyphen", method: post, target: variablesTarget(orgA), contentType: jsonType,
			payload: variableBody(variableA, "AWS-REGION", "x").bytes(), status: 400, conforms: false},
		{name: "create variable, a name of 129 characters", method: post, target: variablesTarget(orgA), contentType: jsonType,
			payload: variableBody(variableA, "N"+strings.Repeat("A", 128), "x").bytes(), status: 400, conforms: false},
		{name: "create variable, a value with a line feed", method: post, target: variablesTarget(orgA), contentType: jsonType,
			payload: variableBody(variableA, "REGION", "eu\nwest").bytes(), status: 400, conforms: false},
		{name: "create variable, a value with U+2028", method: post, target: variablesTarget(orgA), contentType: jsonType,
			payload: variableBody(variableA, "REGION", "eu"+string(rune(0x2028))+"west").bytes(), status: 400, conforms: false},
		{name: "create variable, a value of 1025 characters", method: post, target: variablesTarget(orgA), contentType: jsonType,
			payload: variableBody(variableA, "REGION", strings.Repeat("v", 1025)).bytes(), status: 400, conforms: false},
		{name: "create variable, a description with a trailing space", method: post, target: variablesTarget(orgA), contentType: jsonType,
			payload: variableBody(variableA, "REGION", "x").with("description", "where ").bytes(), status: 400, conforms: false},
		{name: "create variable, no actor", method: post, target: variablesTarget(orgA), contentType: jsonType,
			payload: variableBody(variableA, "REGION", "x").without("actor").bytes(), status: 400, conforms: false},
		{name: "create variable, an unknown field", method: post, target: variablesTarget(orgA), contentType: jsonType,
			payload: variableBody(variableA, "REGION", "x").with("secret", true).bytes(), status: 400, conforms: false},
		{name: "create variable without the secret", method: post, target: variablesTarget(orgA), contentType: jsonType, payload: created,
			unauthenticated: true, status: 401, conforms: false},
		{name: "create variable under an unregistered organization", method: post, target: variablesTarget(orgC), contentType: jsonType,
			payload: created, status: 404, conforms: true},
		{name: "create variable too large", method: post, target: variablesTarget(orgA), contentType: jsonType, payload: tooLarge, status: 413, conforms: false},
		{name: "create variable registry down", method: post, target: variablesTarget(orgA), contentType: jsonType, payload: created,
			failingOrganizations: true, status: 500, conforms: true},

		// PATCH, DELETE …/variables/{variableId}.
		{name: "rename variable", method: patch, target: variableTarget(orgA, variableA), contentType: jsonType, payload: rename,
			seed: withVariable, status: 200, conforms: true},
		{name: "change a used variable's value and description", method: patch, target: variableTarget(orgA, variableA), contentType: jsonType,
			payload: body{"value": "us-east-1", "description": nil, "actor": nil}.bytes(), seed: inUse, status: 200, conforms: true},
		{name: "patch variable, nothing to change", method: patch, target: variableTarget(orgA, variableA), contentType: jsonType,
			payload: body{"actor": nil}.bytes(), seed: withVariable, status: 400, conforms: false},
		{name: "patch variable, a null name", method: patch, target: variableTarget(orgA, variableA), contentType: jsonType,
			payload: body{"name": nil, "actor": nil}.bytes(), seed: withVariable, status: 400, conforms: false},
		{name: "patch variable, an invalid id", method: patch, target: variableTarget(orgA, "x"), contentType: jsonType, payload: rename,
			status: 400, conforms: false},
		{name: "patch variable without the secret", method: patch, target: variableTarget(orgA, variableA), contentType: jsonType, payload: rename,
			unauthenticated: true, status: 401, conforms: false},
		{name: "patch variable unknown", method: patch, target: variableTarget(orgA, variableA), contentType: jsonType, payload: rename,
			status: 404, conforms: true},
		{name: "patch variable under an unregistered organization", method: patch, target: variableTarget(orgC, variableA), contentType: jsonType,
			payload: rename, status: 404, conforms: true},
		{name: "patch variable, the name taken", method: patch, target: variableTarget(orgA, variableA), contentType: jsonType, payload: rename,
			seed: seeds(withVariable, seedVariable(orgA, variableA2, "AWS_REGION", "x")), status: 409, conforms: true},
		{name: "rename a variable in use", method: patch, target: variableTarget(orgA, variableA), contentType: jsonType, payload: rename,
			seed: inUse, status: 409, conforms: true},
		{name: "patch variable too large", method: patch, target: variableTarget(orgA, variableA), contentType: jsonType, payload: tooLarge,
			status: 413, conforms: false},
		{name: "patch variable registry down", method: patch, target: variableTarget(orgA, variableA), contentType: jsonType, payload: rename,
			failingOrganizations: true, status: 500, conforms: true},

		{name: "delete variable", method: del, target: variableTarget(orgA, variableA), seed: withOverride, status: 204, conforms: true},
		{name: "delete variable, an invalid id", method: del, target: variableTarget(orgA, "1"), status: 400, conforms: false},
		{name: "delete variable without the secret", method: del, target: variableTarget(orgA, variableA), unauthenticated: true, status: 401, conforms: false},
		{name: "delete variable unknown", method: del, target: variableTarget(orgA, variableA), status: 404, conforms: true},
		{name: "delete variable under an unregistered organization", method: del, target: variableTarget(orgC, variableA), status: 404, conforms: true},
		{name: "delete a variable in use", method: del, target: variableTarget(orgA, variableA), seed: inUse, status: 409, conforms: true},
		{name: "delete variable registry down", method: del, target: variableTarget(orgA, variableA), failingOrganizations: true, status: 500, conforms: true},

		// GET, POST …/variable-groups.
		{name: "list variable groups", method: get, target: variableGroupsTarget(orgA), seed: seeds(withOverride, bound), status: 200, conforms: true},
		{name: "list variable groups, an invalid organization id", method: get, target: variableGroupsTarget("x"), status: 400, conforms: false},
		{name: "list variable groups without the secret", method: get, target: variableGroupsTarget(orgA), unauthenticated: true, status: 401, conforms: false},
		{name: "list variable groups under an unregistered organization", method: get, target: variableGroupsTarget(orgC), status: 404, conforms: true},
		{name: "list variable groups registry down", method: get, target: variableGroupsTarget(orgA), failingOrganizations: true, status: 500, conforms: true},

		{name: "create variable group", method: post, target: variableGroupsTarget(orgA), contentType: jsonType, payload: createdGroup, status: 201, conforms: true},
		{name: "create variable group without an id, with a description", method: post, target: variableGroupsTarget(orgA), contentType: jsonType,
			payload: body{"name": "Staging - eu", "description": "Pre-production", "actor": nil}.bytes(), status: 201, conforms: true},
		{name: "create variable group, the same again", method: post, target: variableGroupsTarget(orgA), contentType: jsonType, payload: createdGroup,
			seed: withGroup, status: 200, conforms: true},
		{name: "create variable group, the id taken otherwise", method: post, target: variableGroupsTarget(orgA), contentType: jsonType,
			payload: variableGroupBody(variableGroupA, "staging").bytes(), seed: withGroup, status: 409, conforms: true},
		{name: "create variable group, the name taken", method: post, target: variableGroupsTarget(orgA), contentType: jsonType,
			payload: variableGroupBody(variableGroupA2, "production").bytes(), seed: withGroup, status: 409, conforms: true},
		{name: "create variable group, the 101st", method: post, target: variableGroupsTarget(orgA), contentType: jsonType,
			payload: variableGroupBody(variableGroupA2, "one too many").bytes(), seed: seedVariableGroups(orgA, 100), status: 409, conforms: true},
		{name: "create variable group, a name of 201 characters", method: post, target: variableGroupsTarget(orgA), contentType: jsonType,
			payload: variableGroupBody(variableGroupA, strings.Repeat("g", 201)).bytes(), status: 400, conforms: false},
		{name: "create variable group, a name with a leading space", method: post, target: variableGroupsTarget(orgA), contentType: jsonType,
			payload: variableGroupBody(variableGroupA, " production").bytes(), status: 400, conforms: false},
		{name: "create variable group without the secret", method: post, target: variableGroupsTarget(orgA), contentType: jsonType,
			payload: createdGroup, unauthenticated: true, status: 401, conforms: false},
		{name: "create variable group under an unregistered organization", method: post, target: variableGroupsTarget(orgC), contentType: jsonType,
			payload: createdGroup, status: 404, conforms: true},
		{name: "create variable group too large", method: post, target: variableGroupsTarget(orgA), contentType: jsonType, payload: tooLarge,
			status: 413, conforms: false},
		{name: "create variable group registry down", method: post, target: variableGroupsTarget(orgA), contentType: jsonType, payload: createdGroup,
			failingOrganizations: true, status: 500, conforms: true},

		// GET, PATCH, DELETE …/variable-groups/{variableGroupId}.
		{name: "get variable group", method: get, target: variableGroupTarget(orgA, variableGroupA), seed: seeds(withOverride, bound),
			status: 200, conforms: true},
		{name: "get variable group, an invalid id", method: get, target: variableGroupTarget(orgA, "x"), status: 400, conforms: false},
		{name: "get variable group without the secret", method: get, target: variableGroupTarget(orgA, variableGroupA), unauthenticated: true,
			status: 401, conforms: false},
		{name: "get variable group unknown", method: get, target: variableGroupTarget(orgA, variableGroupA), status: 404, conforms: true},
		{name: "get variable group under an unregistered organization", method: get, target: variableGroupTarget(orgC, variableGroupA),
			status: 404, conforms: true},
		{name: "get variable group registry down", method: get, target: variableGroupTarget(orgA, variableGroupA), failingOrganizations: true,
			status: 500, conforms: true},

		{name: "patch variable group", method: patch, target: variableGroupTarget(orgA, variableGroupA), contentType: jsonType,
			payload: body{"name": "prod", "description": "Live", "actor": actorJSONBody()}.bytes(), seed: withGroup, status: 200, conforms: true},
		{name: "patch variable group, nothing to change", method: patch, target: variableGroupTarget(orgA, variableGroupA), contentType: jsonType,
			payload: body{"actor": nil}.bytes(), seed: withGroup, status: 400, conforms: false},
		{name: "patch variable group without the secret", method: patch, target: variableGroupTarget(orgA, variableGroupA), contentType: jsonType,
			payload: body{"name": "prod", "actor": nil}.bytes(), unauthenticated: true, status: 401, conforms: false},
		{name: "patch variable group unknown", method: patch, target: variableGroupTarget(orgA, variableGroupA), contentType: jsonType,
			payload: body{"name": "prod", "actor": nil}.bytes(), status: 404, conforms: true},
		{name: "patch variable group under an unregistered organization", method: patch, target: variableGroupTarget(orgC, variableGroupA),
			contentType: jsonType, payload: body{"name": "prod", "actor": nil}.bytes(), status: 404, conforms: true},
		{name: "patch variable group, the name taken", method: patch, target: variableGroupTarget(orgA, variableGroupA), contentType: jsonType,
			payload: body{"name": "staging", "actor": nil}.bytes(), seed: seeds(withGroup, seedVariableGroup(orgA, variableGroupA2, "staging")),
			status: 409, conforms: true},
		{name: "patch variable group too large", method: patch, target: variableGroupTarget(orgA, variableGroupA), contentType: jsonType,
			payload: tooLarge, status: 413, conforms: false},
		{name: "patch variable group registry down", method: patch, target: variableGroupTarget(orgA, variableGroupA), contentType: jsonType,
			payload: body{"name": "prod", "actor": nil}.bytes(), failingOrganizations: true, status: 500, conforms: true},

		{name: "delete variable group", method: del, target: variableGroupTarget(orgA, variableGroupA), seed: withOverride, status: 204, conforms: true},
		{name: "delete variable group, an invalid id", method: del, target: variableGroupTarget(orgA, "1"), status: 400, conforms: false},
		{name: "delete variable group without the secret", method: del, target: variableGroupTarget(orgA, variableGroupA), unauthenticated: true,
			status: 401, conforms: false},
		{name: "delete variable group unknown", method: del, target: variableGroupTarget(orgA, variableGroupA), status: 404, conforms: true},
		{name: "delete variable group under an unregistered organization", method: del, target: variableGroupTarget(orgC, variableGroupA),
			status: 404, conforms: true},
		{name: "delete a variable group in use", method: del, target: variableGroupTarget(orgA, variableGroupA), seed: groupInUse, status: 409, conforms: true},
		{name: "delete variable group registry down", method: del, target: variableGroupTarget(orgA, variableGroupA), failingOrganizations: true,
			status: 500, conforms: true},

		// PUT, DELETE …/variable-groups/{variableGroupId}/overrides/{variableId}.
		{name: "set an override", method: put, target: overrideTarget(orgA, variableGroupA, variableA), contentType: jsonType, payload: override,
			seed: seeds(withVariable, withGroup), status: 201, conforms: true},
		{name: "replace an override", method: put, target: overrideTarget(orgA, variableGroupA, variableA), contentType: jsonType,
			payload: body{"value": "", "actor": nil}.bytes(), seed: withOverride, status: 200, conforms: true},
		{name: "set an override to its value", method: put, target: overrideTarget(orgA, variableGroupA, variableA), contentType: jsonType,
			payload: override, seed: withOverride, status: 200, conforms: true},
		{name: "set an override, a value with a tab", method: put, target: overrideTarget(orgA, variableGroupA, variableA), contentType: jsonType,
			payload: body{"value": "a\tb", "actor": nil}.bytes(), seed: seeds(withVariable, withGroup), status: 400, conforms: false},
		{name: "set an override, an invalid variable id", method: put, target: overrideTarget(orgA, variableGroupA, "x"), contentType: jsonType,
			payload: override, status: 400, conforms: false},
		{name: "set an override without the secret", method: put, target: overrideTarget(orgA, variableGroupA, variableA), contentType: jsonType,
			payload: override, unauthenticated: true, status: 401, conforms: false},
		{name: "set an override, an unknown variable group", method: put, target: overrideTarget(orgA, variableGroupA, variableA), contentType: jsonType,
			payload: override, seed: withVariable, status: 404, conforms: true},
		{name: "set an override, an unknown variable", method: put, target: overrideTarget(orgA, variableGroupA, variableA), contentType: jsonType,
			payload: override, seed: withGroup, status: 404, conforms: true},
		{name: "set an override under an unregistered organization", method: put, target: overrideTarget(orgC, variableGroupA, variableA),
			contentType: jsonType, payload: override, status: 404, conforms: true},
		{name: "set an override too large", method: put, target: overrideTarget(orgA, variableGroupA, variableA), contentType: jsonType,
			payload: tooLarge, status: 413, conforms: false},
		{name: "set an override registry down", method: put, target: overrideTarget(orgA, variableGroupA, variableA), contentType: jsonType,
			payload: override, failingOrganizations: true, status: 500, conforms: true},

		{name: "reset an override", method: del, target: overrideTarget(orgA, variableGroupA, variableA), contentType: jsonType, payload: resetBody,
			seed: withOverride, status: 204, conforms: true},
		{name: "reset no override, as no person", method: del, target: overrideTarget(orgA, variableGroupA, variableA), contentType: jsonType,
			payload: []byte(`{"actor":null}`), seed: seeds(withVariable, withGroup), status: 204, conforms: true},
		{name: "reset an override, an invalid variable group id", method: del, target: overrideTarget(orgA, "x", variableA), contentType: jsonType,
			payload: resetBody, status: 400, conforms: false},
		{name: "reset an override without a body", method: del, target: overrideTarget(orgA, variableGroupA, variableA), seed: withOverride,
			status: 400, conforms: false},
		{name: "reset an override without an actor", method: del, target: overrideTarget(orgA, variableGroupA, variableA), contentType: jsonType,
			payload: []byte(`{}`), seed: withOverride, status: 400, conforms: false},
		{name: "reset an override, an actor without an email", method: del, target: overrideTarget(orgA, variableGroupA, variableA),
			contentType: jsonType, payload: []byte(`{"actor":{"id":"` + sampleActorID + `"}}`), seed: withOverride, status: 400, conforms: false},
		{name: "reset an override without the secret", method: del, target: overrideTarget(orgA, variableGroupA, variableA), contentType: jsonType,
			payload: resetBody, unauthenticated: true, status: 401, conforms: false},
		{name: "reset an override, an unknown variable group", method: del, target: overrideTarget(orgA, variableGroupA, variableA),
			contentType: jsonType, payload: resetBody, seed: withVariable, status: 404, conforms: true},
		{name: "reset an override, an unknown variable", method: del, target: overrideTarget(orgA, variableGroupA, variableA),
			contentType: jsonType, payload: resetBody, seed: withGroup, status: 404, conforms: true},
		{name: "reset an override under an unregistered organization", method: del, target: overrideTarget(orgC, variableGroupA, variableA),
			contentType: jsonType, payload: resetBody, status: 404, conforms: true},
		{name: "reset an override too large", method: del, target: overrideTarget(orgA, variableGroupA, variableA), contentType: jsonType,
			payload: tooLarge, status: 413, conforms: false},
		{name: "reset an override registry down", method: del, target: overrideTarget(orgA, variableGroupA, variableA), contentType: jsonType,
			payload: resetBody, failingOrganizations: true, status: 500, conforms: true},

		// The fleet routes that take a variable group: a group's.
		{name: "create group with a variable group", method: post, target: groupsTarget(orgA, fleetA), contentType: jsonType,
			payload: body{"name": "gateways", "variableGroupId": variableGroupA}.bytes(), seed: seeds(withFleet, withGroup), status: 201, conforms: true},
		{name: "create group, a variable group the organization doesn't hold", method: post, target: groupsTarget(orgA, fleetA), contentType: jsonType,
			payload: body{"name": "gateways", "variableGroupId": unknownID}.bytes(), seed: withFleet, status: 422, conforms: true},
		{name: "create group, assigned a version referencing an undefined variable", method: post, target: groupsTarget(orgA, fleetA),
			contentType: jsonType, payload: body{"name": "gateways", "assignedPublicationId": publicationA}.bytes(), seed: withUndefined,
			status: 422, conforms: true},
		{name: "create group, a variableGroupId that isn't a UUID", method: post, target: groupsTarget(orgA, fleetA), contentType: jsonType,
			payload: body{"name": "gateways", "variableGroupId": "x"}.bytes(), seed: withFleet, status: 400, conforms: false},
		{name: "patch group, a variable group set", method: patch, contentType: jsonType, payload: body{"variableGroupId": variableGroupA}.bytes(),
			seed: seeds(withFleet, withGroup), targetOf: defaultGroup, status: 200, conforms: true},
		{name: "patch group, its variable group cleared", method: patch, contentType: jsonType, payload: body{"variableGroupId": nil}.bytes(),
			seed: groupInUse, targetOf: gateways, status: 200, conforms: true},
		{name: "patch group, a variable group the organization doesn't hold", method: patch, contentType: jsonType,
			payload: body{"variableGroupId": unknownID}.bytes(), seed: withFleet, targetOf: defaultGroup, status: 422, conforms: true},
		{name: "patch group, assigned a version referencing an undefined variable", method: patch, contentType: jsonType,
			payload: body{"assignedPublicationId": publicationA}.bytes(), seed: withUndefined, targetOf: defaultGroup, status: 422, conforms: true},

		// A collector's pinned variable group.
		{name: "patch collector, a variable group pinned", method: patch, contentType: jsonType,
			payload: body{"pinnedVariableGroupId": variableGroupA}.bytes(), seed: seeds(withCollector, withGroup), targetOf: edge01, status: 200, conforms: true},
		{name: "patch collector, its pinned variable group cleared", method: patch, contentType: jsonType,
			payload: body{"pinnedVariableGroupId": nil}.bytes(), seed: withCollector, targetOf: edge01, status: 200, conforms: true},
		{name: "patch collector, a variable group the organization doesn't hold", method: patch, contentType: jsonType,
			payload: body{"pinnedVariableGroupId": unknownID}.bytes(), seed: withCollector, targetOf: edge01, status: 422, conforms: true},
		{name: "patch collector, pinned to a version referencing an undefined variable", method: patch, contentType: jsonType,
			payload: []byte(`{"pin":` + pinA + `}`), seed: withUndefined, targetOf: edge01, status: 422, conforms: true},
		{name: "patch collector, a pinnedVariableGroupId that isn't a UUID", method: patch, contentType: jsonType,
			payload: body{"pinnedVariableGroupId": 7}.bytes(), seed: withCollector, targetOf: edge01, status: 400, conforms: false},

		// The bulk pin's two choices.
		{name: "pin collectors, a variable group only", method: post, target: collectorsPinTarget(orgA, fleetA), contentType: jsonType,
			seed: seeds(withCollector, withGroup), payloadOf: bulkOf(`"pinnedVariableGroupId":"` + variableGroupA + `"`), status: 200, conforms: true},
		{name: "pin collectors, both, their variable group cleared", method: post, target: collectorsPinTarget(orgA, fleetA), contentType: jsonType,
			seed: withCollector, payloadOf: bulkOf(`"pin":null,"pinnedVariableGroupId":null`), status: 200, conforms: true},
		{name: "pin collectors, a variable group the organization doesn't hold", method: post, target: collectorsPinTarget(orgA, fleetA),
			contentType: jsonType, seed: withCollector, payloadOf: bulkOf(`"pinnedVariableGroupId":"` + unknownID + `"`), status: 422, conforms: true},
		{name: "pin collectors, a version referencing an undefined variable", method: post, target: collectorsPinTarget(orgA, fleetA),
			contentType: jsonType, seed: withUndefined, payloadOf: bulkOf(`"pin":` + pinA), status: 422, conforms: true},
	}
}

// codeConformanceCases are a case for each 404 code a fleet route
// documents that no other case gets (one case per code): an unregistered
// organization, and an unknown fleet.
func codeConformanceCases() []conformanceCase {
	post, get, patch, del := http.MethodPost, http.MethodGet, http.MethodPatch, http.MethodDelete
	var cases []conformanceCase
	for _, r := range []struct {
		method, label string
		target        func(organizationID string) string
		payload       []byte
		fleetNotFound bool
	}{
		{patch, "patch fleet", func(o string) string { return fleetTarget(o, fleetA) }, []byte(`{"label":"x"}`), false},
		{del, "delete fleet", func(o string) string { return fleetTarget(o, fleetA) }, nil, false},
		{get, "list groups", func(o string) string { return groupsTarget(o, fleetA) }, nil, false},
		{patch, "patch group", func(o string) string { return groupTarget(o, fleetA, unknownID) }, []byte(`{"name":"x"}`), true},
		{del, "delete group", func(o string) string { return groupTarget(o, fleetA, unknownID) }, nil, true},
		{get, "list tokens", func(o string) string { return tokensTarget(o, fleetA) }, nil, false},
		{get, "list token events", func(o string) string { return tokenEventsTarget(o, fleetA) }, nil, false},
		{post, "rotate", func(o string) string { return rotateTarget(o, fleetA) }, rotationBody(nil, "2026-10-04T00:00:00.000Z").bytes(), false},
		{post, "issue default", func(o string) string { return issueDefaultTarget(o, fleetA) }, actorBody().bytes(), false},
		{post, "revoke", func(o string) string { return revokeTarget(o, fleetA, unknownID) }, actorBody().bytes(), true},
		{post, "pin collectors", func(o string) string { return collectorsPinTarget(o, fleetA) },
			[]byte(`{"collectorIds":["` + unknownID + `"],"pin":null}`), false},
		{patch, "patch collector", func(o string) string { return collectorTarget(o, fleetA, unknownID) }, []byte(`{"name":"x"}`), true},
		{del, "delete collector", func(o string) string { return collectorTarget(o, fleetA, unknownID) }, nil, true},
		{get, "list collector errors", func(o string) string { return collectorErrorsTarget(o, fleetA, unknownID) }, nil, true},
	} {
		contentType := ""
		if r.payload != nil {
			contentType = jsonType
		}
		cases = append(cases, conformanceCase{name: r.label + " under an unregistered organization", method: r.method, target: r.target(orgC),
			contentType: contentType, payload: r.payload, status: 404, conforms: true})
		if r.fleetNotFound {
			cases = append(cases, conformanceCase{name: r.label + ", an unknown fleet", method: r.method, target: r.target(orgA),
				contentType: contentType, payload: r.payload, status: 404, conforms: true})
		}
	}
	return cases
}

// referencing is a publication body whose YAML references each name as
// ${org:NAME}.
func referencing(names ...string) body {
	var yaml strings.Builder
	yaml.WriteString("processors:\n  attributes/d20:\n    actions:\n")
	for _, name := range names {
		fmt.Fprintf(&yaml, "      - key: %s\n        value: \"${org:%s}\"\n        action: upsert\n", strings.ToLower(name), name)
	}
	return validBody(yaml.String())
}

// The validation the conformance test relies on holds a guard's details to
// the file: a details object with a field too many (a value among them), a
// list past 100, a negative total, or one schema's fields under another's
// fails; the documented shape passes.
func TestTheReplyValidationCatchesMalformedDetails(t *testing.T) {
	doc := loadSpec(t)
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	c := conformanceCase{method: http.MethodDelete, target: variableTarget(orgA, variableA), status: 409}
	route, pathParams, err := router.FindRoute(newRequest(c))
	if err != nil {
		t.Fatal(err)
	}
	use := map[string]any{"publicationId": publicationA, "groups": []any{map[string]any{"fleetId": fleetA, "groupId": unknownID}}, "pinnedCollectors": 1}
	details := func(change func(d map[string]any)) []byte {
		d := map[string]any{"usedBy": []any{use}, "totalPublications": 1, "totalGroups": 1, "totalPinnedCollectors": 1}
		change(d)
		return body{"error": map[string]any{"code": "variable_in_use", "message": messageVariableInUse, "details": d}}.bytes()
	}
	validate := func(raw []byte) error {
		return openapi3filter.ValidateResponse(context.Background(), &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{
				Request: newRequest(c), PathParams: pathParams, Route: route, Options: validationOptions(),
			},
			Status: http.StatusConflict, Header: http.Header{"Content-Type": {jsonType}},
			Body: io.NopCloser(bytes.NewReader(raw)), Options: validationOptions(),
		})
	}
	if err := validate(details(func(map[string]any) {})); err != nil {
		t.Fatalf("the documented details fail validation: %v", err)
	}
	hundredAndOne := make([]any, 101)
	for i := range hundredAndOne {
		hundredAndOne[i] = use
	}
	for label, change := range map[string]func(d map[string]any){
		"a value in the details":    func(d map[string]any) { d["value"] = "eu-west-1" },
		"101 uses":                  func(d map[string]any) { d["usedBy"] = hundredAndOne },
		"a negative total":          func(d map[string]any) { d["totalGroups"] = -1 },
		"a total missing":           func(d map[string]any) { delete(d, "totalPublications") },
		"a group's fields on a use": func(d map[string]any) { d["collectorGroups"] = []any{} },
		"a name in a use": func(d map[string]any) {
			d["usedBy"] = []any{map[string]any{"publicationId": publicationA, "groups": []any{}, "pinnedCollectors": 0, "name": "REGION"}}
		},
	} {
		if err := validate(details(change)); err == nil {
			t.Errorf("details with %s passed validation", label)
		}
	}
}
