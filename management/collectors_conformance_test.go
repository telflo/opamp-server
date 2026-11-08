package management

import (
	"net/http"
	"strings"
	"testing"
)

// collectorConformanceCases are the conformance cases of the collector,
// limits and usage routes, and of the publication deletes' in-use
// refusal: every answer api/openapi.yaml documents for each, on the
// Postgres stores. 500s come from a registry that fails.
func collectorConformanceCases() []conformanceCase {
	tooLarge := []byte(`{"name":"` + strings.Repeat("n", MaxBodyBytes) + `"}`)
	withFleet := seedFleet(orgA, fleetA, "edge-prod", "server")
	withCollectors := seeds(withFleet, seedCollector(orgA, fleetA, "edge-01"), seedCollector(orgA, fleetA, "edge-02"))
	withPublication := seeds(withCollectors, seedPublication(orgA, publicationA, validBody(sampleYAML)))
	withReported := seeds(withPublication, seedReported(orgA, fleetA, "edge-03", publicationA, sampleYAML))
	inUse := seeds(withPublication, seedAssignedGroup(orgA, fleetA, "gateways", publicationA))
	pinnedInUse := seeds(withPublication, seedPin(orgA, fleetA, "edge-01", publicationA))
	edge01 := collectorTargetOf(orgA, fleetA, "edge-01", collectorTarget)
	edge01Errors := collectorTargetOf(orgA, fleetA, "edge-01", collectorErrorsTarget)
	pinPublication := []byte(`{"pin":{"kind":"publication","id":"` + publicationA + `"}}`)
	bulk := func(pin string, ids ...string) []byte {
		return []byte(`{"collectorIds":["` + strings.Join(ids, `","`) + `"],"pin":` + pin + `}`)
	}
	limits := []byte(`{"maxCollectors":500,"requestsPerMinute":null}`)
	post, get, patch, put, del := http.MethodPost, http.MethodGet, http.MethodPatch, http.MethodPut, http.MethodDelete

	return []conformanceCase{
		// GET …/collectors.
		{name: "list collectors, a page", method: get, target: collectorsTarget(orgA, fleetA) + "?limit=1", seed: withCollectors, status: 200, conforms: true},
		{name: "list collectors, searched, by name", method: get,
			target: collectorsTarget(orgA, fleetA) + "?q=EDGE&sort=name&order=asc&limit=100", seed: withCollectors, status: 200, conforms: true},
		{name: "list collectors, a group's, by last seen", method: get, seed: withCollectors, status: 200, conforms: true,
			targetOf: func(t *testing.T, s *Server) string {
				return collectorsTarget(orgA, fleetA) + "?sort=lastSeen&groupId=" + groupID(t, s, orgA, fleetA, "Default")
			}},
		{name: "list collectors, a served one with its error and attributes", method: get, target: collectorsTarget(orgA, fleetA),
			seed: seeds(withReported, seedCollectorError(orgA, fleetA, "edge-03", "failed")), status: 200, conforms: true},
		{name: "list collectors, none", method: get, target: collectorsTarget(orgA, fleetA), seed: withFleet, status: 200, conforms: true},
		{name: "list collectors, q of 101 characters", method: get, target: collectorsTarget(orgA, fleetA) + "?q=" + strings.Repeat("q", 101),
			seed: withFleet, status: 400, conforms: false},
		{name: "list collectors, an unknown sort", method: get, target: collectorsTarget(orgA, fleetA) + "?sort=ip", seed: withFleet, status: 400, conforms: false},
		{name: "list collectors, an unknown order", method: get, target: collectorsTarget(orgA, fleetA) + "?order=up", seed: withFleet, status: 400, conforms: false},
		{name: "list collectors, a groupId that isn't a UUID", method: get, target: collectorsTarget(orgA, fleetA) + "?groupId=x",
			seed: withFleet, status: 400, conforms: false},
		{name: "list collectors, limit 0", method: get, target: collectorsTarget(orgA, fleetA) + "?limit=0", seed: withFleet, status: 400, conforms: false},
		{name: "list collectors, a cursor this API didn't give", method: get, target: collectorsTarget(orgA, fleetA) + "?cursor=x",
			seed: withFleet, status: 400, conforms: true},
		{name: "list collectors, another order's cursor", method: get, seed: withCollectors, status: 400, conforms: true,
			targetOf: func(t *testing.T, s *Server) string {
				var page collectorListReply
				decodeInto(t, call(s, http.MethodGet, collectorsTarget(orgA, fleetA)+"?limit=1", nil, "").Body.Bytes(), &page)
				return collectorsTarget(orgA, fleetA) + "?limit=1&sort=name&cursor=" + *page.NextCursor
			}},
		{name: "list collectors without the secret", method: get, target: collectorsTarget(orgA, fleetA), unauthenticated: true, status: 401, conforms: false},
		{name: "list collectors, an unknown fleet", method: get, target: collectorsTarget(orgA, fleetA), status: 404, conforms: true},
		{name: "list collectors under an unregistered organization", method: get, target: collectorsTarget(orgC, fleetA), status: 404, conforms: true},
		{name: "list collectors registry down", method: get, target: collectorsTarget(orgA, fleetA), failingOrganizations: true, status: 500, conforms: true},

		// POST …/collectors/pin.
		{name: "pin collectors", method: post, contentType: jsonType, seed: withPublication, status: 200, conforms: true,
			target: collectorsPinTarget(orgA, fleetA),
			payloadOf: func(t *testing.T, s *Server) []byte {
				return bulk(`{"kind":"publication","id":"`+publicationA+`"}`, collectorID(t, s, orgA, fleetA, "edge-01"), unknownID)
			}},
		{name: "unpin collectors", method: post, target: collectorsPinTarget(orgA, fleetA), contentType: jsonType, payload: bulk("null", unknownID),
			seed: withFleet, status: 200, conforms: true},
		{name: "pin collectors, no ids", method: post, target: collectorsPinTarget(orgA, fleetA), contentType: jsonType,
			payload: []byte(`{"collectorIds":[],"pin":null}`), seed: withFleet, status: 400, conforms: false},
		{name: "pin collectors, 501 ids", method: post, target: collectorsPinTarget(orgA, fleetA), contentType: jsonType,
			payload: bulk("null", strings.Split(strings.TrimSuffix(strings.Repeat(unknownID+",", 501), ","), ",")...), seed: withFleet, status: 400, conforms: false},
		{name: "pin collectors, an unknown kind", method: post, target: collectorsPinTarget(orgA, fleetA), contentType: jsonType,
			payload: bulk(`{"kind":"configuration","id":"`+publicationA+`"}`, unknownID), seed: withFleet, status: 400, conforms: false},
		{name: "pin collectors, no pin", method: post, target: collectorsPinTarget(orgA, fleetA), contentType: jsonType,
			payload: []byte(`{"collectorIds":["` + unknownID + `"]}`), seed: withFleet, status: 400, conforms: false},
		{name: "pin collectors without the secret", method: post, target: collectorsPinTarget(orgA, fleetA), contentType: jsonType,
			payload: bulk("null", unknownID), unauthenticated: true, status: 401, conforms: false},
		{name: "pin collectors, an unknown fleet", method: post, target: collectorsPinTarget(orgA, fleetA), contentType: jsonType,
			payload: bulk("null", unknownID), status: 404, conforms: true},
		{name: "pin collectors too large", method: post, target: collectorsPinTarget(orgA, fleetA), contentType: jsonType, payload: tooLarge,
			status: 413, conforms: false},
		{name: "pin collectors, a published version the organization doesn't hold", method: post, target: collectorsPinTarget(orgA, fleetA),
			contentType: jsonType, payload: bulk(`{"kind":"publication","id":"`+unknownID+`"}`, unknownID), seed: withFleet, status: 422, conforms: true},
		// A pin names a published version only; an import's kind is refused
		// as any other kind.
		{name: "pin collectors, an import", method: post, target: collectorsPinTarget(orgA, fleetA),
			contentType: jsonType, payload: bulk(`{"kind":"import","id":"`+unknownID+`"}`, unknownID), seed: withFleet, status: 400, conforms: false},
		{name: "pin collectors registry down", method: post, target: collectorsPinTarget(orgA, fleetA), contentType: jsonType,
			payload: bulk("null", unknownID), failingOrganizations: true, status: 500, conforms: true},

		// GET, PATCH, DELETE …/collectors/{collectorId}.
		{name: "get collector", method: get, seed: withCollectors, targetOf: edge01, status: 200, conforms: true},
		{name: "get collector, pinned and resolved", method: get, seed: pinnedInUse, targetOf: edge01, status: 200, conforms: true},
		{name: "get collector, an invalid id", method: get, target: collectorTarget(orgA, fleetA, "x"), status: 400, conforms: false},
		{name: "get collector without the secret", method: get, target: collectorTarget(orgA, fleetA, unknownID), unauthenticated: true, status: 401, conforms: false},
		{name: "get collector unknown", method: get, target: collectorTarget(orgA, fleetA, unknownID), seed: withFleet, status: 404, conforms: true},
		{name: "get collector, an unknown fleet", method: get, target: collectorTarget(orgA, fleetA, unknownID), status: 404, conforms: true},
		{name: "get collector under an unregistered organization", method: get, target: collectorTarget(orgC, fleetA, unknownID), status: 404, conforms: true},
		{name: "get collector registry down", method: get, target: collectorTarget(orgA, fleetA, unknownID), failingOrganizations: true, status: 500, conforms: true},

		{name: "rename and move collector", method: patch, contentType: jsonType, seed: seeds(withCollectors, seedGroup(orgA, fleetA, "gateways")),
			status: 200, conforms: true, targetOf: edge01,
			payloadOf: func(t *testing.T, s *Server) []byte {
				return []byte(`{"name":"edge-01a","groupId":"` + groupID(t, s, orgA, fleetA, "gateways") + `"}`)
			}},
		{name: "pin collector", method: patch, contentType: jsonType, payload: pinPublication, seed: withPublication, targetOf: edge01, status: 200, conforms: true},
		{name: "unpin collector", method: patch, contentType: jsonType, payload: []byte(`{"pin":null}`), seed: pinnedInUse, targetOf: edge01, status: 200, conforms: true},
		{name: "patch collector, nothing to change", method: patch, contentType: jsonType, payload: []byte(`{}`), seed: withCollectors, targetOf: edge01,
			status: 400, conforms: false},
		{name: "patch collector, a null name", method: patch, contentType: jsonType, payload: []byte(`{"name":null}`), seed: withCollectors, targetOf: edge01,
			status: 400, conforms: false},
		{name: "patch collector, a name with a trailing space", method: patch, contentType: jsonType, payload: []byte(`{"name":"edge "}`),
			seed: withCollectors, targetOf: edge01, status: 400, conforms: false},
		{name: "patch collector, a name of 201 characters", method: patch, contentType: jsonType,
			payload: []byte(`{"name":"` + strings.Repeat("n", 201) + `"}`), seed: withCollectors, targetOf: edge01, status: 400, conforms: false},
		{name: "patch collector, a pin without an id", method: patch, contentType: jsonType, payload: []byte(`{"pin":{"kind":"publication"}}`),
			seed: withCollectors, targetOf: edge01, status: 400, conforms: false},
		{name: "patch collector without the secret", method: patch, target: collectorTarget(orgA, fleetA, unknownID), contentType: jsonType,
			payload: []byte(`{"name":"x"}`), unauthenticated: true, status: 401, conforms: false},
		{name: "patch collector unknown", method: patch, target: collectorTarget(orgA, fleetA, unknownID), contentType: jsonType,
			payload: []byte(`{"name":"x"}`), seed: withFleet, status: 404, conforms: true},
		{name: "patch collector, a group the fleet doesn't hold", method: patch, contentType: jsonType,
			payload: []byte(`{"groupId":"` + unknownID + `"}`), seed: withCollectors, targetOf: edge01, status: 404, conforms: true},
		{name: "patch collector, the name taken", method: patch, contentType: jsonType, payload: []byte(`{"name":"edge-02"}`),
			seed: withCollectors, targetOf: edge01, status: 409, conforms: true},
		{name: "patch collector too large", method: patch, target: collectorTarget(orgA, fleetA, unknownID), contentType: jsonType, payload: tooLarge,
			status: 413, conforms: false},
		{name: "patch collector, a published version the organization doesn't hold", method: patch, contentType: jsonType,
			payload: []byte(`{"pin":{"kind":"publication","id":"` + unknownID + `"}}`), seed: withCollectors, targetOf: edge01, status: 422, conforms: true},
		{name: "patch collector, an import", method: patch, contentType: jsonType,
			payload: []byte(`{"pin":{"kind":"import","id":"` + unknownID + `"}}`), seed: withCollectors, targetOf: edge01, status: 400, conforms: false},
		{name: "patch collector registry down", method: patch, target: collectorTarget(orgA, fleetA, unknownID), contentType: jsonType,
			payload: []byte(`{"name":"x"}`), failingOrganizations: true, status: 500, conforms: true},

		{name: "delete collector", method: del, seed: withCollectors, targetOf: edge01, status: 204, conforms: true},
		{name: "delete collector, an invalid id", method: del, target: collectorTarget(orgA, fleetA, "1"), status: 400, conforms: false},
		{name: "delete collector without the secret", method: del, target: collectorTarget(orgA, fleetA, unknownID), unauthenticated: true, status: 401, conforms: false},
		{name: "delete collector unknown", method: del, target: collectorTarget(orgA, fleetA, unknownID), seed: withFleet, status: 404, conforms: true},
		{name: "delete collector registry down", method: del, target: collectorTarget(orgA, fleetA, unknownID), failingOrganizations: true, status: 500, conforms: true},

		// GET …/collectors/{collectorId}/errors.
		{name: "list collector errors", method: get, seed: seeds(withCollectors, seedCollectorError(orgA, fleetA, "edge-01", "boom")),
			targetOf: edge01Errors, status: 200, conforms: true},
		{name: "list collector errors, limit 101", method: get, seed: withCollectors, status: 400, conforms: false,
			targetOf: func(t *testing.T, s *Server) string { return edge01Errors(t, s) + "?limit=101" }},
		{name: "list collector errors, a cursor this API didn't give", method: get, seed: withCollectors, status: 400, conforms: true,
			targetOf: func(t *testing.T, s *Server) string { return edge01Errors(t, s) + "?cursor=x" }},
		{name: "list collector errors without the secret", method: get, target: collectorErrorsTarget(orgA, fleetA, unknownID), unauthenticated: true,
			status: 401, conforms: false},
		{name: "list collector errors, an unknown collector", method: get, target: collectorErrorsTarget(orgA, fleetA, unknownID), seed: withFleet,
			status: 404, conforms: true},
		{name: "list collector errors registry down", method: get, target: collectorErrorsTarget(orgA, fleetA, unknownID), failingOrganizations: true,
			status: 500, conforms: true},

		// GET, PUT …/limits.
		{name: "get limits", method: get, target: limitsTarget(orgA), status: 200, conforms: true},
		{name: "get limits, an invalid organization id", method: get, target: limitsTarget("x"), status: 400, conforms: false},
		{name: "get limits without the secret", method: get, target: limitsTarget(orgA), unauthenticated: true, status: 401, conforms: false},
		{name: "get limits under an unregistered organization", method: get, target: limitsTarget(orgC), status: 404, conforms: true},
		{name: "get limits registry down", method: get, target: limitsTarget(orgA), failingOrganizations: true, status: 500, conforms: true},

		{name: "set limits", method: put, target: limitsTarget(orgA), contentType: jsonType, payload: limits, status: 200, conforms: true},
		{name: "set limits, both unset, a whole 3.0", method: put, target: limitsTarget(orgA), contentType: jsonType,
			payload: []byte(`{"maxCollectors":0,"requestsPerMinute":3.0}`), status: 200, conforms: true},
		{name: "set limits, a negative cap", method: put, target: limitsTarget(orgA), contentType: jsonType,
			payload: []byte(`{"maxCollectors":-1,"requestsPerMinute":null}`), status: 400, conforms: false},
		{name: "set limits, zero requests a minute", method: put, target: limitsTarget(orgA), contentType: jsonType,
			payload: []byte(`{"maxCollectors":null,"requestsPerMinute":0}`), status: 400, conforms: false},
		{name: "set limits, a fraction", method: put, target: limitsTarget(orgA), contentType: jsonType,
			payload: []byte(`{"maxCollectors":1.5,"requestsPerMinute":null}`), status: 400, conforms: false},
		{name: "set limits, past an integer column", method: put, target: limitsTarget(orgA), contentType: jsonType,
			payload: []byte(`{"maxCollectors":2147483648,"requestsPerMinute":null}`), status: 400, conforms: false},
		{name: "set limits, one field", method: put, target: limitsTarget(orgA), contentType: jsonType,
			payload: []byte(`{"maxCollectors":5}`), status: 400, conforms: false},
		{name: "set limits without the secret", method: put, target: limitsTarget(orgA), contentType: jsonType, payload: limits,
			unauthenticated: true, status: 401, conforms: false},
		{name: "set limits under an unregistered organization", method: put, target: limitsTarget(orgC), contentType: jsonType, payload: limits,
			status: 404, conforms: true},
		{name: "set limits too large", method: put, target: limitsTarget(orgA), contentType: jsonType, payload: tooLarge, status: 413, conforms: false},
		{name: "set limits registry down", method: put, target: limitsTarget(orgA), contentType: jsonType, payload: limits,
			failingOrganizations: true, status: 500, conforms: true},

		// GET …/configurations/{configurationId}/usage.
		{name: "usage, in use", method: get, target: usageTarget(orgA, configurationA), seed: seeds(inUse, seedPin(orgA, fleetA, "edge-02", publicationA)),
			status: 200, conforms: true},
		{name: "usage, nothing published", method: get, target: usageTarget(orgA, configurationB), status: 200, conforms: true},
		{name: "usage, an invalid configuration id", method: get, target: usageTarget(orgA, "x"), status: 400, conforms: false},
		{name: "usage without the secret", method: get, target: usageTarget(orgA, configurationA), unauthenticated: true, status: 401, conforms: false},
		{name: "usage under an unregistered organization", method: get, target: usageTarget(orgC, configurationA), status: 404, conforms: true},
		{name: "usage registry down", method: get, target: usageTarget(orgA, configurationA), failingOrganizations: true, status: 500, conforms: true},

		// The publication deletes' in-use refusal.
		{name: "delete a published version a group uses", method: del, target: publicationTarget(orgA, publicationA), seed: inUse, status: 409, conforms: true},
		{name: "delete a published version a collector is pinned to", method: del, target: publicationTarget(orgA, publicationA), seed: pinnedInUse,
			status: 409, conforms: true},
		{name: "delete a configuration's published versions, one in use", method: del, target: configurationTarget(orgA, configurationA),
			seed: inUse, status: 409, conforms: true},
	}
}
