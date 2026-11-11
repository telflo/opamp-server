package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/telflo-opamp-server/store/enrollmenttokens"
)

// fleetConformanceCases are the conformance cases of the fleet, group and
// token routes: every answer api/openapi.yaml documents for each, on the
// Postgres stores. 500s come from a registry that fails (every route asks
// it first).
func fleetConformanceCases() []conformanceCase {
	tooLarge := []byte(`{"label":"` + strings.Repeat("l", MaxBodyBytes) + `"}`)
	server := fleetBody(fleetA, "edge-prod", "server")
	withFleet := seedFleet(orgA, fleetA, "edge-prod", "server")
	withK8s := seedFleet(orgA, fleetA, "edge-k8s", "kubernetes-supervisor")
	withGateways := seeds(withFleet, seedGroup(orgA, fleetA, "gateways"))
	defaultGroup := groupTargetOf(orgA, fleetA, "Default")
	gateways := groupTargetOf(orgA, fleetA, "gateways")
	revealRotate := tokenTargetOf(orgA, fleetA, enrollmenttokens.KindRotate, revealTarget)
	revokeRotate := tokenTargetOf(orgA, fleetA, enrollmenttokens.KindRotate, revokeTarget)
	withPublication := seeds(withFleet, seedPublication(orgA, publicationA, validBody(sampleYAML)))
	rotation := rotationBody(futureTime, "2026-10-04T00:00:00.000Z")
	// A cursor this API gives: after the newest event a fleet can hold (the last whole millisecond of 9999, the greatest id).
	farCursor := enrollmenttokens.EventCursor{
		OccurredAt: time.Date(9999, time.December, 31, 23, 59, 59, 999_000_000, time.UTC),
		ID:         uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff"),
	}.String()
	withCollector := seeds(withFleet, seedCollector(orgA, fleetA, "edge-01"))
	rotateNever := seeds(withFleet, rotateSeed(orgA, fleetA, nil, "2026-10-04T00:00:00.000Z"))
	post, get, patch, del := http.MethodPost, http.MethodGet, http.MethodPatch, http.MethodDelete

	return []conformanceCase{
		// POST …/fleets.
		{name: "create fleet", method: post, target: fleetsTarget(orgA), contentType: jsonType, payload: server.bytes(), status: 201, conforms: true},
		{name: "create fleet without an id, never expiring, no actor", method: post, target: fleetsTarget(orgA), contentType: jsonType,
			payload: fleetBody("", "edge-prod", "kubernetes-supervisor").with("rotateExpiresAt", nil).with("actor", nil).bytes(), status: 201, conforms: true},
		{name: "create fleet again, identical", method: post, target: fleetsTarget(orgA), contentType: jsonType, payload: server.bytes(),
			seed: withFleet, status: 200, conforms: true},
		{name: "create fleet, the id taken by another label", method: post, target: fleetsTarget(orgA), contentType: jsonType,
			payload: server.with("label", "edge-dev").bytes(), seed: withFleet, status: 409, conforms: true},
		{name: "create fleet, the id taken by another organization", method: post, target: fleetsTarget(orgB), contentType: jsonType,
			payload: server.bytes(), seed: withFleet, status: 409, conforms: true},
		{name: "create fleet, the label taken", method: post, target: fleetsTarget(orgA), contentType: jsonType,
			payload: server.with("id", fleetA2).bytes(), seed: withFleet, status: 409, conforms: true},
		{name: "create fleet, a label with a leading space", method: post, target: fleetsTarget(orgA), contentType: jsonType,
			payload: server.with("label", " edge").bytes(), status: 400, conforms: false},
		{name: "create fleet, a label with a trailing no-break space", method: post, target: fleetsTarget(orgA), contentType: jsonType,
			payload: server.with("label", "edge ").bytes(), status: 400, conforms: false},
		{name: "create fleet, a label with a leading ideographic space", method: post, target: fleetsTarget(orgA), contentType: jsonType,
			payload: server.with("label", "　edge").bytes(), status: 400, conforms: false},
		{name: "create fleet, a label of 81 characters", method: post, target: fleetsTarget(orgA), contentType: jsonType,
			payload: server.with("label", strings.Repeat("l", 81)).bytes(), status: 400, conforms: false},
		{name: "create fleet, created, a label of 80 characters with inner spaces", method: post, target: fleetsTarget(orgA), contentType: jsonType,
			payload: server.with("label", "edge　prod "+strings.Repeat("l", 70)).bytes(), status: 201, conforms: true},
		{name: "create fleet, the bridge's type", method: post, target: fleetsTarget(orgA), contentType: jsonType,
			payload: server.with("type", "kubernetes").bytes(), status: 400, conforms: false},
		{name: "create fleet, no actor field", method: post, target: fleetsTarget(orgA), contentType: jsonType,
			payload: server.without("actor").bytes(), status: 400, conforms: false},
		{name: "create fleet, an unknown field", method: post, target: fleetsTarget(orgA), contentType: jsonType,
			payload: server.with("labels", []string{}).bytes(), status: 400, conforms: false},
		{name: "create fleet, a rotate token expiring in the past", method: post, target: fleetsTarget(orgA), contentType: jsonType,
			payload: server.with("rotateExpiresAt", "2026-10-01T00:00:00.000Z").bytes(), status: 400, conforms: true},
		{name: "create fleet without the secret", method: post, target: fleetsTarget(orgA), contentType: jsonType, payload: server.bytes(),
			unauthenticated: true, status: 401, conforms: false},
		{name: "create fleet under an unregistered organization", method: post, target: fleetsTarget(orgC), contentType: jsonType,
			payload: server.bytes(), status: 404, conforms: true},
		{name: "create fleet too large", method: post, target: fleetsTarget(orgA), contentType: jsonType, payload: tooLarge, status: 413, conforms: false},
		{name: "create fleet registry down", method: post, target: fleetsTarget(orgA), contentType: jsonType, payload: server.bytes(),
			failingOrganizations: true, status: 500, conforms: true},

		// GET …/fleets.
		{name: "list fleets, a page", method: get, target: fleetsTarget(orgA) + "?limit=1", status: 200, conforms: true,
			seed: seeds(withFleet, seedFleet(orgA, fleetA2, "edge-dev", "server"))},
		{name: "list fleets, the archived", method: get, target: fleetsTarget(orgA) + "?status=archived", status: 200, conforms: true,
			seed: seeds(withFleet, archiveSeed(orgA, fleetA))},
		{name: "list fleets, none", method: get, target: fleetsTarget(orgA), status: 200, conforms: true},
		{name: "list fleets, a collector seen, a grace, no valid rotate token", method: get, target: fleetsTarget(orgA), status: 200, conforms: true,
			seed: seeds(withCollector, rotateSeed(orgA, fleetA, nil, "2026-10-04T00:00:00.000Z"),
				seedFleet(orgA, fleetA2, "edge-dev", "server"), revokeSeed(orgA, fleetA2, enrollmenttokens.KindRotate))},
		{name: "list fleets, limit 101", method: get, target: fleetsTarget(orgA) + "?limit=101", status: 400, conforms: false},
		{name: "list fleets, an unknown status", method: get, target: fleetsTarget(orgA) + "?status=deleted", status: 400, conforms: false},
		{name: "list fleets, a cursor this API didn't give", method: get, target: fleetsTarget(orgA) + "?cursor=x", status: 400, conforms: true},
		{name: "list fleets, the status twice", method: get, target: fleetsTarget(orgA) + "?status=active&status=active", status: 400, conforms: true},
		{name: "list fleets without the secret", method: get, target: fleetsTarget(orgA), unauthenticated: true, status: 401, conforms: false},
		{name: "list fleets under an unregistered organization", method: get, target: fleetsTarget(orgC), status: 404, conforms: true},
		{name: "list fleets registry down", method: get, target: fleetsTarget(orgA), failingOrganizations: true, status: 500, conforms: true},

		// GET, PATCH, DELETE …/fleets/{fleetId}.
		// The summary's nullable values, each null and set: a new fleet has
		// no collector seen, its rotate token expiring and in no grace.
		{name: "get fleet", method: get, target: fleetTarget(orgA, fleetA), seed: withFleet, status: 200, conforms: true},
		{name: "get fleet, a collector seen", method: get, target: fleetTarget(orgA, fleetA), seed: withCollector, status: 200, conforms: true},
		{name: "get fleet, in a grace, the new token never expiring", method: get, target: fleetTarget(orgA, fleetA), seed: rotateNever,
			status: 200, conforms: true},
		{name: "get fleet, no valid rotate token", method: get, target: fleetTarget(orgA, fleetA),
			seed: seeds(withFleet, revokeSeed(orgA, fleetA, enrollmenttokens.KindRotate)), status: 200, conforms: true},
		{name: "get fleet, an invalid id", method: get, target: fleetTarget(orgA, "x"), status: 400, conforms: false},
		{name: "get fleet without the secret", method: get, target: fleetTarget(orgA, fleetA), unauthenticated: true, status: 401, conforms: false},
		{name: "get fleet unknown", method: get, target: fleetTarget(orgA, fleetA), status: 404, conforms: true},
		{name: "get fleet under an unregistered organization", method: get, target: fleetTarget(orgC, fleetA), status: 404, conforms: true},
		{name: "get fleet registry down", method: get, target: fleetTarget(orgA, fleetA), failingOrganizations: true, status: 500, conforms: true},

		{name: "rename fleet", method: patch, target: fleetTarget(orgA, fleetA), contentType: jsonType, payload: []byte(`{"label":"edge-eu"}`),
			seed: withFleet, status: 200, conforms: true},
		{name: "archive fleet", method: patch, target: fleetTarget(orgA, fleetA), contentType: jsonType, payload: []byte(`{"status":"archived"}`),
			seed: withFleet, status: 200, conforms: true},
		{name: "patch fleet, nothing to change", method: patch, target: fleetTarget(orgA, fleetA), contentType: jsonType, payload: []byte(`{}`),
			seed: withFleet, status: 400, conforms: false},
		{name: "patch fleet, a type", method: patch, target: fleetTarget(orgA, fleetA), contentType: jsonType, payload: []byte(`{"type":"server"}`),
			seed: withFleet, status: 400, conforms: false},
		{name: "patch fleet, a null label", method: patch, target: fleetTarget(orgA, fleetA), contentType: jsonType, payload: []byte(`{"label":null}`),
			seed: withFleet, status: 400, conforms: false},
		{name: "patch fleet without the secret", method: patch, target: fleetTarget(orgA, fleetA), contentType: jsonType,
			payload: []byte(`{"label":"edge-eu"}`), unauthenticated: true, status: 401, conforms: false},
		{name: "patch fleet unknown", method: patch, target: fleetTarget(orgA, fleetA), contentType: jsonType, payload: []byte(`{"label":"edge-eu"}`),
			status: 404, conforms: true},
		{name: "patch fleet, the label taken", method: patch, target: fleetTarget(orgA, fleetA), contentType: jsonType, payload: []byte(`{"label":"edge-dev"}`),
			seed: seeds(withFleet, seedFleet(orgA, fleetA2, "edge-dev", "server")), status: 409, conforms: true},
		{name: "patch fleet too large", method: patch, target: fleetTarget(orgA, fleetA), contentType: jsonType, payload: tooLarge, status: 413, conforms: false},
		{name: "patch fleet registry down", method: patch, target: fleetTarget(orgA, fleetA), contentType: jsonType, payload: []byte(`{"label":"edge-eu"}`),
			failingOrganizations: true, status: 500, conforms: true},

		{name: "delete fleet", method: del, target: fleetTarget(orgA, fleetA), seed: withGateways, status: 204, conforms: true},
		{name: "delete fleet, an invalid id", method: del, target: fleetTarget(orgA, "1"), status: 400, conforms: false},
		{name: "delete fleet without the secret", method: del, target: fleetTarget(orgA, fleetA), unauthenticated: true, status: 401, conforms: false},
		{name: "delete fleet unknown", method: del, target: fleetTarget(orgA, fleetA), status: 404, conforms: true},
		{name: "delete fleet registry down", method: del, target: fleetTarget(orgA, fleetA), failingOrganizations: true, status: 500, conforms: true},
		{name: "delete fleet, already being deleted", method: del, target: fleetTarget(orgA, fleetA),
			seed: seeds(withFleet, seedFleetDeleteRunning(orgA, fleetA)), status: 503, conforms: true},
		{name: "delete fleet, its organization being deleted", method: del, target: fleetTarget(orgA, fleetA),
			seed: seeds(withFleet, seedOrganizationDeleteRunning(orgA)), status: 503, conforms: true},

		// GET, POST …/groups.
		{name: "list groups", method: get, target: groupsTarget(orgA, fleetA), seed: withGateways, status: 200, conforms: true},
		{name: "list groups, an invalid fleet id", method: get, target: groupsTarget(orgA, "x"), status: 400, conforms: false},
		{name: "list groups without the secret", method: get, target: groupsTarget(orgA, fleetA), unauthenticated: true, status: 401, conforms: false},
		{name: "list groups, an unknown fleet", method: get, target: groupsTarget(orgA, fleetA), status: 404, conforms: true},
		{name: "list groups registry down", method: get, target: groupsTarget(orgA, fleetA), failingOrganizations: true, status: 500, conforms: true},

		{name: "create group", method: post, target: groupsTarget(orgA, fleetA), contentType: jsonType,
			payload: []byte(`{"name":"gateways","assignedPublicationId":"` + publicationA + `"}`), seed: withPublication, status: 201, conforms: true},
		// A Kubernetes group's install fields are the platform's, in its
		// annotations.
		{name: "create group on a kubernetes-supervisor fleet with its fields in annotations", method: post, target: groupsTarget(orgA, fleetA), contentType: jsonType,
			payload: []byte(`{"name":"traces-sampling","annotations":{"telflo":{"kubernetes":{"role":"gateway","namespace":"telemetry","replicas":3,"volumes":[{"hostPath":"/var/log/journal","hostPathType":"Directory","mountPath":"/var/log/journal","readOnly":true}]}}},"assignedPublicationId":null}`),
			seed:    withK8s, status: 201, conforms: true},
		// Annotations on any fleet's group.
		{name: "create group with annotations on a server fleet", method: post, target: groupsTarget(orgA, fleetA), contentType: jsonType,
			payload: []byte(`{"name":"edge","annotations":{"telflo":{"dockerMounts":[{"hostPath":"/var/log","containerPath":"/var/log","readOnly":true}]}}}`),
			seed:    withFleet, status: 201, conforms: true},
		{name: "create group, annotations that aren't an object", method: post, target: groupsTarget(orgA, fleetA), contentType: jsonType,
			payload: []byte(`{"name":"edge","annotations":["telflo"]}`), seed: withFleet, status: 400, conforms: false},
		{name: "create group, annotations with an exponent", method: post, target: groupsTarget(orgA, fleetA), contentType: jsonType,
			payload: []byte(`{"name":"edge","annotations":{"n":1e3}}`), seed: withFleet, status: 400, conforms: true},
		{name: "patch group, annotations cleared", method: patch, contentType: jsonType, payload: []byte(`{"annotations":null}`),
			seed: withFleet, targetOf: groupTargetOf(orgA, fleetA, "Default"), status: 200, conforms: true},
		// A group's role, namespace, replicas and volumes are unknown fields,
		// on either fleet type.
		{name: "create group, a role", method: post, target: groupsTarget(orgA, fleetA), contentType: jsonType,
			payload: []byte(`{"name":"nodes","role":"node"}`), seed: withK8s, status: 400, conforms: false},
		{name: "create group, a namespace", method: post, target: groupsTarget(orgA, fleetA), contentType: jsonType,
			payload: []byte(`{"name":"nodes","namespace":"telemetry"}`), seed: withK8s, status: 400, conforms: false},
		{name: "create group, replicas", method: post, target: groupsTarget(orgA, fleetA), contentType: jsonType,
			payload: []byte(`{"name":"nodes","replicas":3}`), seed: withK8s, status: 400, conforms: false},
		{name: "create group, volumes", method: post, target: groupsTarget(orgA, fleetA), contentType: jsonType,
			payload: []byte(`{"name":"nodes","volumes":[]}`), seed: withK8s, status: 400, conforms: false},
		{name: "create group, a null role on a server fleet", method: post, target: groupsTarget(orgA, fleetA), contentType: jsonType,
			payload: []byte(`{"name":"nodes","role":null}`), seed: withFleet, status: 400, conforms: false},
		{name: "create group, a name with a trailing tab", method: post, target: groupsTarget(orgA, fleetA), contentType: jsonType,
			payload: []byte(`{"name":"gateways\t"}`), seed: withFleet, status: 400, conforms: false},
		{name: "create group, no name", method: post, target: groupsTarget(orgA, fleetA), contentType: jsonType,
			payload: []byte(`{"assignedPublicationId":null}`), seed: withFleet, status: 400, conforms: false},
		{name: "create group without the secret", method: post, target: groupsTarget(orgA, fleetA), contentType: jsonType,
			payload: []byte(`{"name":"gateways"}`), unauthenticated: true, status: 401, conforms: false},
		{name: "create group, an unknown fleet", method: post, target: groupsTarget(orgA, fleetA), contentType: jsonType,
			payload: []byte(`{"name":"gateways"}`), status: 404, conforms: true},
		{name: "create group under an unregistered organization", method: post, target: groupsTarget(orgC, fleetA), contentType: jsonType,
			payload: []byte(`{"name":"gateways"}`), status: 404, conforms: true},
		{name: "create group, the Default group's name", method: post, target: groupsTarget(orgA, fleetA), contentType: jsonType,
			payload: []byte(`{"name":"Default"}`), seed: withFleet, status: 409, conforms: true},
		{name: "create group too large", method: post, target: groupsTarget(orgA, fleetA), contentType: jsonType, payload: tooLarge, status: 413, conforms: false},
		{name: "create group, a published version the organization doesn't hold", method: post, target: groupsTarget(orgA, fleetA), contentType: jsonType,
			payload: []byte(`{"name":"gateways","assignedPublicationId":"` + unknownID + `"}`), seed: withFleet, status: 422, conforms: true},
		{name: "create group registry down", method: post, target: groupsTarget(orgA, fleetA), contentType: jsonType,
			payload: []byte(`{"name":"gateways"}`), failingOrganizations: true, status: 500, conforms: true},

		// GET, PATCH, DELETE …/groups/{groupId}.
		{name: "get group", method: get, seed: withFleet, targetOf: defaultGroup, status: 200, conforms: true},
		{name: "get group, an invalid id", method: get, target: groupTarget(orgA, fleetA, "x"), status: 400, conforms: false},
		{name: "get group without the secret", method: get, target: groupTarget(orgA, fleetA, unknownID), unauthenticated: true, status: 401, conforms: false},
		{name: "get group unknown", method: get, target: groupTarget(orgA, fleetA, unknownID), seed: withFleet, status: 404, conforms: true},
		{name: "get group, an unknown fleet", method: get, target: groupTarget(orgA, fleetA, unknownID), status: 404, conforms: true},
		{name: "get group under an unregistered organization", method: get, target: groupTarget(orgC, fleetA, unknownID), status: 404, conforms: true},
		{name: "get group registry down", method: get, target: groupTarget(orgA, fleetA, unknownID), failingOrganizations: true, status: 500, conforms: true},

		{name: "update group", method: patch, contentType: jsonType, payload: []byte(`{"name":"edges","assignedPublicationId":"` + publicationA + `"}`),
			seed: seeds(withPublication, seedGroup(orgA, fleetA, "gateways")), targetOf: gateways, status: 200, conforms: true},
		{name: "update group, clearing", method: patch, contentType: jsonType, payload: []byte(`{"assignedPublicationId":null}`),
			seed: withFleet, targetOf: defaultGroup, status: 200, conforms: true},
		{name: "update group, nothing to change", method: patch, contentType: jsonType, payload: []byte(`{}`),
			seed: withFleet, targetOf: defaultGroup, status: 400, conforms: false},
		{name: "update group, a null name", method: patch, contentType: jsonType, payload: []byte(`{"name":null}`),
			seed: withFleet, targetOf: defaultGroup, status: 400, conforms: false},
		{name: "update group, replicas", method: patch, contentType: jsonType, payload: []byte(`{"replicas":2}`),
			seed: withK8s, targetOf: defaultGroup, status: 400, conforms: false},
		{name: "update group, volumes cleared", method: patch, contentType: jsonType, payload: []byte(`{"volumes":null}`),
			seed: withK8s, targetOf: defaultGroup, status: 400, conforms: false},
		{name: "update group without the secret", method: patch, target: groupTarget(orgA, fleetA, unknownID), contentType: jsonType,
			payload: []byte(`{"name":"edges"}`), unauthenticated: true, status: 401, conforms: false},
		{name: "update group unknown", method: patch, target: groupTarget(orgA, fleetA, unknownID), contentType: jsonType,
			payload: []byte(`{"name":"edges"}`), seed: withFleet, status: 404, conforms: true},
		{name: "update group, the name taken", method: patch, contentType: jsonType, payload: []byte(`{"name":"Default"}`),
			seed: withGateways, targetOf: gateways, status: 409, conforms: true},
		{name: "update group, renaming the Default group", method: patch, contentType: jsonType, payload: []byte(`{"name":"Main"}`),
			seed: withFleet, targetOf: defaultGroup, status: 409, conforms: true},
		{name: "update group too large", method: patch, target: groupTarget(orgA, fleetA, unknownID), contentType: jsonType, payload: tooLarge,
			status: 413, conforms: false},
		{name: "update group, a published version the organization doesn't hold", method: patch, contentType: jsonType,
			payload: []byte(`{"assignedPublicationId":"` + unknownID + `"}`), seed: withFleet, targetOf: defaultGroup, status: 422, conforms: true},
		{name: "update group registry down", method: patch, target: groupTarget(orgA, fleetA, unknownID), contentType: jsonType,
			payload: []byte(`{"name":"edges"}`), failingOrganizations: true, status: 500, conforms: true},

		{name: "delete group", method: del, seed: withGateways, targetOf: gateways, status: 204, conforms: true},
		{name: "delete group, an invalid id", method: del, target: groupTarget(orgA, fleetA, "1"), status: 400, conforms: false},
		{name: "delete group without the secret", method: del, target: groupTarget(orgA, fleetA, unknownID), unauthenticated: true, status: 401, conforms: false},
		{name: "delete group unknown", method: del, target: groupTarget(orgA, fleetA, unknownID), seed: withFleet, status: 404, conforms: true},
		{name: "delete group, the Default group", method: del, seed: withFleet, targetOf: defaultGroup, status: 409, conforms: true},
		{name: "delete group registry down", method: del, target: groupTarget(orgA, fleetA, unknownID), failingOrganizations: true, status: 500, conforms: true},

		// GET …/tokens.
		{name: "list tokens", method: get, target: tokensTarget(orgA, fleetA), seed: withFleet, status: 200, conforms: true},
		{name: "list tokens, an invalid fleet id", method: get, target: tokensTarget(orgA, "x"), status: 400, conforms: false},
		{name: "list tokens without the secret", method: get, target: tokensTarget(orgA, fleetA), unauthenticated: true, status: 401, conforms: false},
		{name: "list tokens, an unknown fleet", method: get, target: tokensTarget(orgA, fleetA), status: 404, conforms: true},
		{name: "list tokens registry down", method: get, target: tokensTarget(orgA, fleetA), failingOrganizations: true, status: 500, conforms: true},

		// GET …/tokens/events.
		{name: "list token events", method: get, target: tokenEventsTarget(orgA, fleetA), seed: withFleet, status: 200, conforms: true},
		{name: "list token events, a page of one", method: get, target: tokenEventsTarget(orgA, fleetA) + "?limit=1",
			seed: withFleet, status: 200, conforms: true},
		{name: "list token events, a valid cursor", method: get, target: tokenEventsTarget(orgA, fleetA) + "?cursor=" + farCursor,
			seed: withFleet, status: 200, conforms: true},
		{name: "list token events, limit 101", method: get, target: tokenEventsTarget(orgA, fleetA) + "?limit=101", seed: withFleet,
			status: 400, conforms: false},
		{name: "list token events, a cursor this API didn't give", method: get, target: tokenEventsTarget(orgA, fleetA) + "?cursor=x",
			seed: withFleet, status: 400, conforms: true},
		{name: "list token events, an invalid fleet id", method: get, target: tokenEventsTarget(orgA, "x"), status: 400, conforms: false},
		{name: "list token events without the secret", method: get, target: tokenEventsTarget(orgA, fleetA), unauthenticated: true,
			status: 401, conforms: false},
		{name: "list token events, an unknown fleet", method: get, target: tokenEventsTarget(orgA, fleetA), status: 404, conforms: true},
		{name: "list token events, another organization's fleet", method: get, target: tokenEventsTarget(orgB, fleetA), seed: withFleet,
			status: 404, conforms: true},
		{name: "list token events registry down", method: get, target: tokenEventsTarget(orgA, fleetA), failingOrganizations: true,
			status: 500, conforms: true},

		// POST …/tokens/rotate.
		{name: "rotate", method: post, target: rotateTarget(orgA, fleetA), contentType: jsonType, payload: rotation.bytes(), seed: withFleet, status: 201, conforms: true},
		{name: "rotate, the new token never expiring, no actor", method: post, target: rotateTarget(orgA, fleetA), contentType: jsonType,
			payload: rotation.with("expiresAt", nil).with("actor", nil).bytes(), seed: withFleet, status: 201, conforms: true},
		{name: "rotate, a grace end in the past", method: post, target: rotateTarget(orgA, fleetA), contentType: jsonType,
			payload: rotationBody(futureTime, "2026-10-01T00:00:00.000Z").bytes(), seed: withFleet, status: 400, conforms: true},
		{name: "rotate, a grace end after the expiry", method: post, target: rotateTarget(orgA, fleetA), contentType: jsonType,
			payload: rotationBody("2026-10-05T00:00:00.000Z", "2026-10-06T00:00:00.000Z").bytes(), seed: withFleet, status: 400, conforms: true},
		{name: "rotate, no grace end", method: post, target: rotateTarget(orgA, fleetA), contentType: jsonType,
			payload: rotation.without("graceEndsAt").bytes(), seed: withFleet, status: 400, conforms: false},
		{name: "rotate without the secret", method: post, target: rotateTarget(orgA, fleetA), contentType: jsonType, payload: rotation.bytes(),
			unauthenticated: true, status: 401, conforms: false},
		{name: "rotate, an unknown fleet", method: post, target: rotateTarget(orgA, fleetA), contentType: jsonType, payload: rotation.bytes(),
			status: 404, conforms: true},
		{name: "rotate, an archived fleet", method: post, target: rotateTarget(orgA, fleetA), contentType: jsonType, payload: rotation.bytes(),
			seed: seeds(withFleet, archiveSeed(orgA, fleetA)), status: 409, conforms: true},
		{name: "rotate too large", method: post, target: rotateTarget(orgA, fleetA), contentType: jsonType, payload: tooLarge, status: 413, conforms: false},
		{name: "rotate registry down", method: post, target: rotateTarget(orgA, fleetA), contentType: jsonType, payload: rotation.bytes(),
			failingOrganizations: true, status: 500, conforms: true},

		// POST …/tokens/default.
		{name: "issue default after a revoke", method: post, target: issueDefaultTarget(orgA, fleetA), contentType: jsonType,
			payload: actorBody().bytes(), seed: seeds(withFleet, revokeSeed(orgA, fleetA, enrollmenttokens.KindDefault)), status: 201, conforms: true},
		{name: "issue default, no actor", method: post, target: issueDefaultTarget(orgA, fleetA), contentType: jsonType,
			payload: []byte(`{"actor":null}`), seed: seeds(withFleet, revokeSeed(orgA, fleetA, enrollmenttokens.KindDefault)), status: 201, conforms: true},
		{name: "issue default, a valid default token", method: post, target: issueDefaultTarget(orgA, fleetA), contentType: jsonType,
			payload: actorBody().bytes(), seed: withFleet, status: 409, conforms: true},
		{name: "issue default, an archived fleet", method: post, target: issueDefaultTarget(orgA, fleetA), contentType: jsonType,
			payload: actorBody().bytes(), seed: seeds(withFleet, revokeSeed(orgA, fleetA, enrollmenttokens.KindDefault), archiveSeed(orgA, fleetA)),
			status: 409, conforms: true},
		{name: "issue default, no body", method: post, target: issueDefaultTarget(orgA, fleetA), contentType: jsonType,
			payload: []byte(`{}`), seed: withFleet, status: 400, conforms: false},
		{name: "issue default without the secret", method: post, target: issueDefaultTarget(orgA, fleetA), contentType: jsonType,
			payload: actorBody().bytes(), unauthenticated: true, status: 401, conforms: false},
		{name: "issue default, an unknown fleet", method: post, target: issueDefaultTarget(orgA, fleetA), contentType: jsonType,
			payload: actorBody().bytes(), status: 404, conforms: true},
		{name: "issue default too large", method: post, target: issueDefaultTarget(orgA, fleetA), contentType: jsonType, payload: tooLarge,
			status: 413, conforms: false},
		{name: "issue default registry down", method: post, target: issueDefaultTarget(orgA, fleetA), contentType: jsonType,
			payload: actorBody().bytes(), failingOrganizations: true, status: 500, conforms: true},

		// POST …/tokens/{tokenId}/reveal.
		{name: "reveal", method: post, contentType: jsonType, payload: actorBody().bytes(), seed: withFleet, targetOf: revealRotate, status: 200, conforms: true},
		{name: "reveal the default token, no actor", method: post, contentType: jsonType, payload: []byte(`{"actor":null}`), seed: withFleet,
			targetOf: tokenTargetOf(orgA, fleetA, enrollmenttokens.KindDefault, revealTarget), status: 200, conforms: true},
		{name: "reveal, an actor without an email", method: post, contentType: jsonType, payload: []byte(`{"actor":{"id":"` + actorA + `"}}`),
			seed: withFleet, targetOf: revealRotate, status: 400, conforms: false},
		{name: "reveal, no body", method: post, contentType: jsonType, payload: []byte(`{}`), seed: withFleet, targetOf: revealRotate, status: 400, conforms: false},
		{name: "reveal without the secret", method: post, target: revealTarget(orgA, fleetA, unknownID), contentType: jsonType,
			payload: actorBody().bytes(), unauthenticated: true, status: 401, conforms: false},
		{name: "reveal unknown", method: post, target: revealTarget(orgA, fleetA, unknownID), contentType: jsonType, payload: actorBody().bytes(),
			seed: withFleet, status: 404, conforms: true},
		{name: "reveal, an unknown fleet", method: post, target: revealTarget(orgA, fleetA, unknownID), contentType: jsonType, payload: actorBody().bytes(),
			status: 404, conforms: true},
		{name: "reveal under an unregistered organization", method: post, target: revealTarget(orgC, fleetA, unknownID), contentType: jsonType,
			payload: actorBody().bytes(), status: 404, conforms: true},
		{name: "reveal, revoked", method: post, contentType: jsonType, payload: actorBody().bytes(),
			seed: seeds(withFleet, revokeSeed(orgA, fleetA, enrollmenttokens.KindRotate)), targetOf: revealRotate, status: 409, conforms: true},
		{name: "reveal, expired", method: post, contentType: jsonType, payload: actorBody().bytes(),
			seed: seeds(withFleet, laterSeed(2*365*24*time.Hour)), targetOf: revealRotate, status: 409, conforms: true},
		{name: "reveal too large", method: post, target: revealTarget(orgA, fleetA, unknownID), contentType: jsonType, payload: tooLarge,
			status: 413, conforms: false},
		{name: "reveal registry down", method: post, target: revealTarget(orgA, fleetA, unknownID), contentType: jsonType, payload: actorBody().bytes(),
			failingOrganizations: true, status: 500, conforms: true},

		// POST …/tokens/{tokenId}/revoke.
		{name: "revoke", method: post, contentType: jsonType, payload: actorBody().bytes(), seed: withFleet, targetOf: revokeRotate, status: 200, conforms: true},
		{name: "revoke, already revoked", method: post, contentType: jsonType, payload: actorBody().bytes(),
			seed: seeds(withFleet, revokeSeed(orgA, fleetA, enrollmenttokens.KindRotate)), targetOf: revokeRotate, status: 200, conforms: true},
		{name: "revoke, an unknown field", method: post, contentType: jsonType, payload: actorBody().with("reason", "x").bytes(),
			seed: withFleet, targetOf: revokeRotate, status: 400, conforms: false},
		{name: "revoke without the secret", method: post, target: revokeTarget(orgA, fleetA, unknownID), contentType: jsonType,
			payload: actorBody().bytes(), unauthenticated: true, status: 401, conforms: false},
		{name: "revoke unknown", method: post, target: revokeTarget(orgA, fleetA, unknownID), contentType: jsonType, payload: actorBody().bytes(),
			seed: withFleet, status: 404, conforms: true},
		{name: "revoke too large", method: post, target: revokeTarget(orgA, fleetA, unknownID), contentType: jsonType, payload: tooLarge,
			status: 413, conforms: false},
		{name: "revoke registry down", method: post, target: revokeTarget(orgA, fleetA, unknownID), contentType: jsonType, payload: actorBody().bytes(),
			failingOrganizations: true, status: 500, conforms: true},
	}
}
