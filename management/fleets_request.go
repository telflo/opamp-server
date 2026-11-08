package management

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/collectorgroups"
	"github.com/telflo/opamp-server/store/collectors"
	"github.com/telflo/opamp-server/store/enrollmenttokens"
	"github.com/telflo/opamp-server/store/fleets"
)

// The request bodies and replies of the fleet, group and token routes. Each
// body is read field by field, as a publication's is: a missing field, a
// null where none is allowed, a wrong type and an unknown field each get a
// message naming the field, never its value. openapi.yaml states every
// bound.

// The stores storeFailed names.
const (
	storeFleets           = "fleets"
	storeCollectorGroups  = "collector groups"
	storeEnrollmentTokens = "enrollment tokens"
	storeCollectors       = "collectors"
)

// The fields' messages.
const (
	messageLabel         = "label must be 1 to 80 characters, with no leading or trailing whitespace and no control character."
	messageGroupName     = "name must be 1 to 80 characters, with no leading or trailing whitespace and no control character."
	messageFleetType     = "type must be server or kubernetes-supervisor."
	messageFleetStatus   = "status must be active or archived."
	messageAnnotations   = "annotations must be a JSON object of at most 16384 bytes, or null."
	messageNoFleetChange = "The request body must name label or status."
	messageNoGroupChange = "The request body must name at least one field."
)

// decodeFleetCreate reads POST …/fleets's body: {"label", "type",
// "rotateExpiresAt", "actor"}, the last two nullable, and an optional "id"
// (a UUID; null is refused). rotateExpiresAt, when set, must be after now.
// It reports whether the body named an id.
func decodeFleetCreate(body []byte, now time.Time) (fleets.NewFleet, bool, *requestError) {
	var f fleets.NewFleet
	raw, rerr := decodeBody(body)
	if rerr != nil {
		return f, false, rerr
	}
	top, rerr := decodeObjectWithOptional(raw, "", "", []string{"label", "type", "rotateExpiresAt", "actor"}, []string{"id"})
	if rerr != nil {
		return f, false, rerr
	}
	hasID := top.has("id")
	if hasID {
		if f.ID, rerr = top.uuid("id"); rerr != nil {
			return f, false, rerr
		}
	}
	if f.Label, rerr = fleetLabel(top); rerr != nil {
		return f, false, rerr
	}
	kind, rerr := top.string("type")
	if rerr != nil || !fleets.Type(kind).Valid() {
		return f, false, invalid(messageFleetType)
	}
	f.Type = fleets.Type(kind)
	if f.RotateExpiresAt, rerr = optionalDateTime(top, "rotateExpiresAt"); rerr != nil {
		return f, false, rerr
	}
	if f.RotateExpiresAt != nil && !millisecond(*f.RotateExpiresAt).After(millisecond(now)) {
		return f, false, invalid("rotateExpiresAt must be in the future, or null.")
	}
	if f.CreatedBy, rerr = decodeActor(top, "actor"); rerr != nil {
		return f, false, rerr
	}
	return f, hasID, nil
}

// decodeFleetUpdate reads PATCH …/fleets/{fleetId}'s body: "label" and
// "status", at least one, neither null.
func decodeFleetUpdate(body []byte) (fleets.Changes, *requestError) {
	var c fleets.Changes
	raw, rerr := decodeBody(body)
	if rerr != nil {
		return c, rerr
	}
	top, rerr := decodeObjectWithOptional(raw, "", "", nil, []string{"label", "status"})
	if rerr != nil {
		return c, rerr
	}
	if !top.has("label") && !top.has("status") {
		return c, invalid(messageNoFleetChange)
	}
	if top.has("label") {
		label, rerr := fleetLabel(top)
		if rerr != nil {
			return c, rerr
		}
		c.Label = &label
	}
	if top.has("status") {
		raw, rerr := top.string("status")
		status := fleets.Status(raw)
		if rerr != nil || !status.Valid() {
			return c, invalid(messageFleetStatus)
		}
		c.Status = &status
	}
	return c, nil
}

// fleetLabel reads a label: 1 to 80 characters, trimmed (no leading or
// trailing whitespace, as Unicode counts it), no control character (U+0000
// among them); kept as written.
func fleetLabel(top *jsonObject) (string, *requestError) {
	label, rerr := top.string("label")
	if rerr != nil || !fleets.ValidLabel(label) {
		return "", invalid(messageLabel)
	}
	return label, nil
}

// decodeCollectorGroupCreate reads POST …/groups's body: {"name"}, and any
// of "assignedPublicationId", "annotations" and "variableGroupId",
// each nullable (null and absent are the same: unset).
func decodeCollectorGroupCreate(body []byte) (collectorgroups.NewGroup, *requestError) {
	var g collectorgroups.NewGroup
	raw, rerr := decodeBody(body)
	if rerr != nil {
		return g, rerr
	}
	top, rerr := decodeObjectWithOptional(raw, "", "", []string{"name"}, groupOptionalFields)
	if rerr != nil {
		return g, rerr
	}
	if g.Name, rerr = groupName(top); rerr != nil {
		return g, rerr
	}
	fields, rerr := decodeGroupFields(top)
	if rerr != nil {
		return g, rerr
	}
	g.AssignedPublicationID = fields.assignedPublicationID.Value
	g.VariableGroupID = fields.variableGroupID.Value
	if fields.annotations.Value != nil {
		g.Annotations = *fields.annotations.Value
	}
	return g, nil
}

// decodeCollectorGroupUpdate reads PATCH …/groups/{groupId}'s body: any of
// "name" (not null), "assignedPublicationId", "annotations" and
// "variableGroupId" (null clears; an object replaces the annotations
// whole), at least one.
func decodeCollectorGroupUpdate(body []byte) (collectorgroups.Changes, *requestError) {
	var c collectorgroups.Changes
	raw, rerr := decodeBody(body)
	if rerr != nil {
		return c, rerr
	}
	top, rerr := decodeObjectWithOptional(raw, "", "", nil, append([]string{"name"}, groupOptionalFields...))
	if rerr != nil {
		return c, rerr
	}
	if len(top.fields) == 0 {
		return c, invalid(messageNoGroupChange)
	}
	if top.has("name") {
		name, rerr := groupName(top)
		if rerr != nil {
			return c, rerr
		}
		c.Name = &name
	}
	fields, rerr := decodeGroupFields(top)
	if rerr != nil {
		return c, rerr
	}
	c.AssignedPublicationID, c.Annotations, c.VariableGroupID = fields.assignedPublicationID, fields.annotations, fields.variableGroupID
	return c, nil
}

// groupOptionalFields are a group's fields besides its name.
var groupOptionalFields = []string{"assignedPublicationId", "annotations", "variableGroupId"}

// groupFields are a body's optional group fields: each unset when absent,
// cleared when null, set otherwise.
type groupFields struct {
	assignedPublicationID collectorgroups.Change[uuid.UUID]
	annotations           collectorgroups.Change[json.RawMessage]
	// variableGroupID is the variable group its collectors' text is filled
	// in with.
	variableGroupID collectorgroups.Change[uuid.UUID]
}

func decodeGroupFields(top *jsonObject) (groupFields, *requestError) {
	var f groupFields
	if top.has("assignedPublicationId") {
		f.assignedPublicationID = collectorgroups.Clear[uuid.UUID]()
		if !top.isNull("assignedPublicationId") {
			id, rerr := top.uuid("assignedPublicationId")
			if rerr != nil {
				return f, invalid("assignedPublicationId must be a UUID, or null.")
			}
			f.assignedPublicationID = collectorgroups.SetTo(id)
		}
	}
	if top.has("annotations") {
		f.annotations = collectorgroups.Clear[json.RawMessage]()
		if !top.isNull("annotations") {
			annotations, err := collectorgroups.CanonicalAnnotations(top.fields["annotations"])
			if err != nil {
				return f, invalid(messageAnnotations)
			}
			f.annotations = collectorgroups.SetTo(annotations)
		}
	}
	if top.has("variableGroupId") {
		id, rerr := optionalUUID(top, "variableGroupId", messageVariableGroupID)
		if rerr != nil {
			return f, rerr
		}
		f.variableGroupID = collectorgroups.Clear[uuid.UUID]()
		if id != nil {
			f.variableGroupID = collectorgroups.SetTo(*id)
		}
	}
	return f, nil
}

// groupName reads a group's name: as a label, 1 to 80 characters, trimmed.
func groupName(top *jsonObject) (string, *requestError) {
	name, rerr := top.string("name")
	if rerr != nil || !collectorgroups.ValidName(name) {
		return "", invalid(messageGroupName)
	}
	return name, nil
}

// decodeRotation reads POST …/tokens/rotate's body: {"expiresAt" (the new
// token's, null for never), "graceEndsAt", "actor"}. The grace end must be
// after now and not after the new token's expiry.
func decodeRotation(body []byte, now time.Time) (enrollmenttokens.Rotation, *requestError) {
	var rotation enrollmenttokens.Rotation
	raw, rerr := decodeBody(body)
	if rerr != nil {
		return rotation, rerr
	}
	top, rerr := decodeObject(raw, "", "", "expiresAt", "graceEndsAt", "actor")
	if rerr != nil {
		return rotation, rerr
	}
	if rotation.ExpiresAt, rerr = optionalDateTime(top, "expiresAt"); rerr != nil {
		return rotation, rerr
	}
	graceEndsAt, rerr := top.string("graceEndsAt")
	if rerr != nil {
		return rotation, invalid("graceEndsAt must be an RFC 3339 date-time.")
	}
	if rotation.GraceEndsAt, rerr = parseDateTime("graceEndsAt", graceEndsAt); rerr != nil {
		return rotation, rerr
	}
	if !millisecond(rotation.GraceEndsAt).After(millisecond(now)) {
		return rotation, invalid("graceEndsAt must be in the future.")
	}
	if rotation.ExpiresAt != nil && millisecond(rotation.GraceEndsAt).After(millisecond(*rotation.ExpiresAt)) {
		return rotation, invalid("graceEndsAt must not be after expiresAt.")
	}
	if rotation.Actor, rerr = decodeActor(top, "actor"); rerr != nil {
		return rotation, rerr
	}
	return rotation, nil
}

// decodeActorOnly reads a reveal's or a revoke's body: {"actor"}.
func decodeActorOnly(body []byte) (*fleets.Actor, *requestError) {
	raw, rerr := decodeBody(body)
	if rerr != nil {
		return nil, rerr
	}
	top, rerr := decodeObject(raw, "", "", "actor")
	if rerr != nil {
		return nil, rerr
	}
	return decodeActor(top, "actor")
}

// decodeActor reads who acts: {"id", "email"}, both, or null for a change no
// person made (as a publication's publishedBy).
func decodeActor(top *jsonObject, field string) (*fleets.Actor, *requestError) {
	if top.isNull(field) {
		return nil, nil
	}
	actor, rerr := top.object(field, "id", "email")
	if rerr != nil {
		return nil, rerr
	}
	var a fleets.Actor
	if a.ID, rerr = actor.uuid("id"); rerr != nil {
		return nil, rerr
	}
	if a.Email, rerr = actor.boundedString("email", 1, maxEmailCharacters); rerr != nil {
		return nil, rerr
	}
	return &a, nil
}

// optionalDateTime reads a nullable RFC 3339 date-time, kept to the
// millisecond.
func optionalDateTime(top *jsonObject, field string) (*time.Time, *requestError) {
	if top.isNull(field) {
		return nil, nil
	}
	value, rerr := top.string(field)
	if rerr != nil {
		return nil, invalid("%s must be an RFC 3339 date-time, or null.", field)
	}
	parsed, rerr := parseDateTime(field, value)
	if rerr != nil {
		return nil, rerr
	}
	parsed = millisecond(parsed)
	return &parsed, nil
}

// millisecond is t as the stores keep it.
func millisecond(t time.Time) time.Time {
	return t.UTC().Truncate(fleets.Precision)
}

// fleetReply is a fleet as the API answers it (openapi.yaml's Fleet).
type fleetReply struct {
	ID             string      `json:"id"`
	OrganizationID string      `json:"organizationId"`
	Label          string      `json:"label"`
	Type           string      `json:"type"`
	Status         string      `json:"status"`
	CreatedBy      *actorReply `json:"createdBy"`
	CreatedAt      string      `json:"createdAt"`
	UpdatedAt      string      `json:"updatedAt"`
}

func fleetJSON(f fleets.Fleet) fleetReply {
	return fleetReply{
		ID: f.ID.String(), OrganizationID: f.OrganizationID.String(), Label: f.Label,
		Type: string(f.Type), Status: string(f.Status), CreatedBy: actorJSON(f.CreatedBy),
		CreatedAt: formatTime(f.CreatedAt), UpdatedAt: formatTime(f.UpdatedAt),
	}
}

// fleetDetailReply is a fleet as its reads answer it (openapi.yaml's
// FleetDetail): the fleet and its summary.
type fleetDetailReply struct {
	fleetReply
	Summary summaryReply `json:"summary"`
}

// summaryReply is a fleet's collectors counted (openapi.yaml's
// FleetSummary): by liveness, and by sync, each adding up to Collectors;
// their latest last seen; and its rotate tokens' state.
type summaryReply struct {
	Collectors int `json:"collectors"`
	Online     int `json:"online"`
	Warning    int `json:"warning"`
	Unknown    int `json:"unknown"`
	Offline    int `json:"offline"`
	InSync     int `json:"inSync"`
	Pending    int `json:"pending"`
	Failed     int `json:"failed"`
	Drifted    int `json:"drifted"`
	Unassigned int `json:"unassigned"`
	// LastSeenAt is null when the fleet has no collector.
	LastSeenAt *string `json:"lastSeenAt"`
	// RotateToken is null when the fleet has no valid rotate token.
	RotateToken *rotateTokenReply `json:"rotateToken"`
}

// rotateTokenReply is a fleet's rotate tokens' state (openapi.yaml's
// FleetRotateToken).
type rotateTokenReply struct {
	ExpiresAt   *string `json:"expiresAt"`
	GraceEndsAt *string `json:"graceEndsAt"`
	ValidCount  int     `json:"validCount"`
}

// fleetDetailJSON is f with its summary m and its rotate tokens' state, the
// fleet's entry in states (none: null).
func fleetDetailJSON(f fleets.Fleet, m collectors.Summary, states map[uuid.UUID]enrollmenttokens.RotateState) fleetDetailReply {
	reply := fleetDetailReply{fleetReply: fleetJSON(f), Summary: summaryReply{
		Collectors: m.Collectors, Online: m.Online, Warning: m.Warning, Unknown: m.Unknown, Offline: m.Offline,
		InSync: m.InSync, Pending: m.Pending, Failed: m.Failed, Drifted: m.Drifted, Unassigned: m.Unassigned,
		LastSeenAt: formatOptionalTime(m.LastSeenAt),
	}}
	if state, found := states[f.ID]; found {
		reply.Summary.RotateToken = &rotateTokenReply{
			ExpiresAt: formatOptionalTime(state.ExpiresAt), GraceEndsAt: formatOptionalTime(state.GraceEndsAt), ValidCount: state.ValidCount,
		}
	}
	return reply
}

type fleetListReply struct {
	Fleets     []fleetDetailReply `json:"fleets"`
	NextCursor *string            `json:"nextCursor"`
}

// fleetUnchangedReply is a create's 200: the fleet, and no bearer.
type fleetUnchangedReply struct {
	Fleet fleetReply `json:"fleet"`
}

// fleetCreatedReply is a create's 201: the fleet, its Default group and
// both tokens' bearers, the only time both are answered at once.
type fleetCreatedReply struct {
	Fleet        fleetReply `json:"fleet"`
	DefaultGroup groupReply `json:"defaultGroup"`
	Tokens       struct {
		Default bearerReply       `json:"default"`
		Rotate  rotateBearerReply `json:"rotate"`
	} `json:"tokens"`
}

type bearerReply struct {
	ID     string `json:"id"`
	Bearer string `json:"bearer"`
}

type rotateBearerReply struct {
	ID        string  `json:"id"`
	Bearer    string  `json:"bearer"`
	ExpiresAt *string `json:"expiresAt"`
}

// groupReply is a collector group as the API answers it (openapi.yaml's
// CollectorGroup).
type groupReply struct {
	ID                    string          `json:"id"`
	FleetID               string          `json:"fleetId"`
	Name                  string          `json:"name"`
	IsDefault             bool            `json:"isDefault"`
	AssignedPublicationID *string         `json:"assignedPublicationId"`
	Annotations           json.RawMessage `json:"annotations"`
	// VariableGroupID is the variable group its collectors' text is filled
	// in with, under their pin's, or null.
	VariableGroupID *string `json:"variableGroupId"`
	CreatedAt       string  `json:"createdAt"`
	UpdatedAt       string  `json:"updatedAt"`
}

func groupJSON(g collectorgroups.Group) groupReply {
	reply := groupReply{
		ID: g.ID.String(), FleetID: g.FleetID.String(), Name: g.Name, IsDefault: g.IsDefault,
		Annotations: g.Annotations,
		CreatedAt:   formatTime(g.CreatedAt), UpdatedAt: formatTime(g.UpdatedAt),
	}
	if g.AssignedPublicationID != nil {
		id := g.AssignedPublicationID.String()
		reply.AssignedPublicationID = &id
	}
	reply.VariableGroupID = optionalIDString(g.VariableGroupID)
	return reply
}

type groupListReply struct {
	Groups []groupReply `json:"groups"`
}

// enrollmentTokenReply is a token as the API answers it (openapi.yaml's
// EnrollmentToken): never its bearer.
type enrollmentTokenReply struct {
	ID        string      `json:"id"`
	FleetID   string      `json:"fleetId"`
	Kind      string      `json:"kind"`
	ExpiresAt *string     `json:"expiresAt"`
	RevokedAt *string     `json:"revokedAt"`
	DoRefresh bool        `json:"doRefresh"`
	CreatedBy *actorReply `json:"createdBy"`
	CreatedAt string      `json:"createdAt"`
}

func enrollmentTokenJSON(t enrollmenttokens.Token) enrollmentTokenReply {
	return enrollmentTokenReply{
		ID: t.ID.String(), FleetID: t.FleetID.String(), Kind: string(t.Kind),
		ExpiresAt: formatOptionalTime(t.ExpiresAt), RevokedAt: formatOptionalTime(t.RevokedAt),
		DoRefresh: t.DoRefresh, CreatedBy: actorJSON(t.CreatedBy), CreatedAt: formatTime(t.CreatedAt),
	}
}

// listedEnrollmentTokenReply is a token in the list, with its collectors
// seen on it in the last 5 minutes.
type listedEnrollmentTokenReply struct {
	enrollmentTokenReply
	ActiveCollectors int `json:"activeCollectors"`
}

type enrollmentTokenListReply struct {
	Tokens []listedEnrollmentTokenReply `json:"tokens"`
}

// tokenEventReply is one token event as the API answers it (openapi.yaml's
// EnrollmentTokenEvent): ids, the actor and the time only.
type tokenEventReply struct {
	ID         string      `json:"id"`
	TokenID    string      `json:"tokenId"`
	Event      string      `json:"event"`
	Actor      *actorReply `json:"actor"`
	OccurredAt string      `json:"occurredAt"`
}

type tokenEventListReply struct {
	Events     []tokenEventReply `json:"events"`
	NextCursor *string           `json:"nextCursor"`
}

// rotatedReply is a rotation's 201: the new rotate token's bearer, and the
// token put in its grace, if any.
type rotatedReply struct {
	ID            string  `json:"id"`
	Bearer        string  `json:"bearer"`
	ExpiresAt     *string `json:"expiresAt"`
	PredecessorID *string `json:"predecessorId"`
}

// revealedReply is a reveal's 200: a fresh bearer for the token.
type revealedReply struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"`
	Bearer    string  `json:"bearer"`
	ExpiresAt *string `json:"expiresAt"`
}

func actorJSON(a *fleets.Actor) *actorReply {
	if a == nil {
		return nil
	}
	return &actorReply{ID: a.ID.String(), Email: a.Email}
}

func formatOptionalTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	formatted := formatTime(*t)
	return &formatted
}

// optionalIDString is a nullable id as the API writes it.
func optionalIDString(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	formatted := id.String()
	return &formatted
}
