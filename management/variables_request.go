package management

import (
	"github.com/google/uuid"

	"github.com/telflo/telflo-opamp-server/store/collectorgroups"
	"github.com/telflo/telflo-opamp-server/store/fleets"
	"github.com/telflo/telflo-opamp-server/store/orgvariables"
	"github.com/telflo/telflo-opamp-server/store/variables"
	"github.com/telflo/telflo-opamp-server/store/variableuse"
)

// The request bodies, replies and details of the organization variables'
// routes. Each body is read field by field, as a fleet's is: a missing
// field, a null where none is allowed, a wrong type, an unknown field and a
// value out of bounds each get a message naming the field, never its
// value. A variable's value, name and description, and a variable group's
// name and description, are configuration text: they are answered by these
// routes and nowhere else, and never logged; a guard's details hold ids,
// counts and variable names only.

// The stores storeFailed names.
const (
	storeVariables      = "variables"
	storeVariableGroups = "variable groups"
)

// The fields' messages.
const (
	messageVariableName  = "name must be 1 to 128 characters: a letter or an underscore, then letters, digits or underscores."
	messageVariableValue = "value must be a string of at most 1024 characters on one line: no control character " +
		"(a tab, a line feed or a carriage return among them), and neither U+2028 nor U+2029."
	messageVariableDescription = "description must be 1 to 1000 characters, with no leading or trailing whitespace " +
		"and no control character, or null."
	messageVariableGroupName     = "name must be 1 to 200 characters, with no leading or trailing whitespace and no control character."
	messageVariableGroupID       = "variableGroupId must be a UUID, or null."
	messagePinnedVariableGroupID = "pinnedVariableGroupId must be a UUID, or null."
	messageNoVariableChange      = "The request body must name name, value or description."
	messageNoVariableGroupChange = "The request body must name name or description."
)

// decodeVariableCreate reads POST …/variables's body: {"name", "value",
// "description", "actor"}, the last two nullable, and an optional "id" (a
// UUID; null is refused). It reports whether the body named an id.
func decodeVariableCreate(body []byte) (variables.NewVariable, bool, *requestError) {
	var v variables.NewVariable
	raw, rerr := decodeBody(body)
	if rerr != nil {
		return v, false, rerr
	}
	top, rerr := decodeObjectWithOptional(raw, "", "", []string{"name", "value", "description", "actor"}, []string{"id"})
	if rerr != nil {
		return v, false, rerr
	}
	hasID := top.has("id")
	if hasID {
		if v.ID, rerr = top.uuid("id"); rerr != nil {
			return v, false, rerr
		}
	}
	if v.Name, rerr = variableName(top); rerr != nil {
		return v, false, rerr
	}
	if v.Value, rerr = variableValue(top); rerr != nil {
		return v, false, rerr
	}
	if v.Description, rerr = variableDescription(top); rerr != nil {
		return v, false, rerr
	}
	if v.Actor, rerr = decodeActor(top, "actor"); rerr != nil {
		return v, false, rerr
	}
	return v, hasID, nil
}

// decodeVariableUpdate reads PATCH …/variables/{variableId}'s body: any of
// "name" and "value" (neither null) and "description" (null clears), at
// least one, and "actor" (nullable), required.
func decodeVariableUpdate(body []byte) (variables.VariableChanges, *fleets.Actor, *requestError) {
	var c variables.VariableChanges
	raw, rerr := decodeBody(body)
	if rerr != nil {
		return c, nil, rerr
	}
	top, rerr := decodeObjectWithOptional(raw, "", "", []string{"actor"}, []string{"name", "value", "description"})
	if rerr != nil {
		return c, nil, rerr
	}
	if !top.has("name") && !top.has("value") && !top.has("description") {
		return c, nil, invalid(messageNoVariableChange)
	}
	if top.has("name") {
		name, rerr := variableName(top)
		if rerr != nil {
			return c, nil, rerr
		}
		c.Name = &name
	}
	if top.has("value") {
		value, rerr := variableValue(top)
		if rerr != nil {
			return c, nil, rerr
		}
		c.Value = &value
	}
	if top.has("description") {
		if c.Description, rerr = descriptionChange(top); rerr != nil {
			return c, nil, rerr
		}
	}
	actor, rerr := decodeActor(top, "actor")
	if rerr != nil {
		return c, nil, rerr
	}
	return c, actor, nil
}

// decodeVariableGroupCreate reads POST …/variable-groups's body: {"name",
// "description", "actor"}, the last two nullable, and an optional "id".
func decodeVariableGroupCreate(body []byte) (variables.NewGroup, bool, *requestError) {
	var g variables.NewGroup
	raw, rerr := decodeBody(body)
	if rerr != nil {
		return g, false, rerr
	}
	top, rerr := decodeObjectWithOptional(raw, "", "", []string{"name", "description", "actor"}, []string{"id"})
	if rerr != nil {
		return g, false, rerr
	}
	hasID := top.has("id")
	if hasID {
		if g.ID, rerr = top.uuid("id"); rerr != nil {
			return g, false, rerr
		}
	}
	if g.Name, rerr = variableGroupName(top); rerr != nil {
		return g, false, rerr
	}
	if g.Description, rerr = variableDescription(top); rerr != nil {
		return g, false, rerr
	}
	if g.Actor, rerr = decodeActor(top, "actor"); rerr != nil {
		return g, false, rerr
	}
	return g, hasID, nil
}

// decodeVariableGroupUpdate reads PATCH …/variable-groups/{variableGroupId}'s
// body: any of "name" (not null) and "description" (null clears), at least
// one, and "actor" (nullable), required.
func decodeVariableGroupUpdate(body []byte) (variables.GroupChanges, *fleets.Actor, *requestError) {
	var c variables.GroupChanges
	raw, rerr := decodeBody(body)
	if rerr != nil {
		return c, nil, rerr
	}
	top, rerr := decodeObjectWithOptional(raw, "", "", []string{"actor"}, []string{"name", "description"})
	if rerr != nil {
		return c, nil, rerr
	}
	if !top.has("name") && !top.has("description") {
		return c, nil, invalid(messageNoVariableGroupChange)
	}
	if top.has("name") {
		name, rerr := variableGroupName(top)
		if rerr != nil {
			return c, nil, rerr
		}
		c.Name = &name
	}
	if top.has("description") {
		if c.Description, rerr = descriptionChange(top); rerr != nil {
			return c, nil, rerr
		}
	}
	actor, rerr := decodeActor(top, "actor")
	if rerr != nil {
		return c, nil, rerr
	}
	return c, actor, nil
}

// decodeOverride reads PUT …/overrides/{variableId}'s body: {"value",
// "actor"}, the actor nullable.
func decodeOverride(body []byte) (string, *fleets.Actor, *requestError) {
	raw, rerr := decodeBody(body)
	if rerr != nil {
		return "", nil, rerr
	}
	top, rerr := decodeObject(raw, "", "", "value", "actor")
	if rerr != nil {
		return "", nil, rerr
	}
	value, rerr := variableValue(top)
	if rerr != nil {
		return "", nil, rerr
	}
	actor, rerr := decodeActor(top, "actor")
	if rerr != nil {
		return "", nil, rerr
	}
	return value, actor, nil
}

// variableName reads a variable's name: the renderer's grammar, at most 128
// characters (orgvariables.ValidName).
func variableName(top *jsonObject) (string, *requestError) {
	name, rerr := top.string("name")
	if rerr != nil || !orgvariables.ValidName(name) {
		return "", invalid(messageVariableName)
	}
	return name, nil
}

// variableValue reads a value: 0 to 1,024 characters on one line
// (orgvariables.ValidValue), else a 400 naming the field.
func variableValue(top *jsonObject) (string, *requestError) {
	value, rerr := top.string("value")
	if rerr != nil || !orgvariables.ValidValue(value) {
		return "", invalid(messageVariableValue)
	}
	return value, nil
}

// variableDescription reads a nullable description: trimmed, 1 to 1,000
// characters, no control character.
func variableDescription(top *jsonObject) (*string, *requestError) {
	if top.isNull("description") {
		return nil, nil
	}
	description, rerr := top.string("description")
	if rerr != nil || !orgvariables.ValidDescription(&description) {
		return nil, invalid(messageVariableDescription)
	}
	return &description, nil
}

// descriptionChange is a present "description" as a change: null clears it.
func descriptionChange(top *jsonObject) (collectorgroups.Change[string], *requestError) {
	description, rerr := variableDescription(top)
	if rerr != nil {
		return collectorgroups.Change[string]{}, rerr
	}
	if description == nil {
		return collectorgroups.Clear[string](), nil
	}
	return collectorgroups.SetTo(*description), nil
}

// variableGroupName reads a variable group's name: trimmed, 1 to 200
// characters, no control character.
func variableGroupName(top *jsonObject) (string, *requestError) {
	name, rerr := top.string("name")
	if rerr != nil || !orgvariables.ValidGroupName(name) {
		return "", invalid(messageVariableGroupName)
	}
	return name, nil
}

// optionalUUID reads a present nullable UUID field: nil for null; any other
// value but a UUID is message's 400.
func optionalUUID(top *jsonObject, field, message string) (*uuid.UUID, *requestError) {
	if top.isNull(field) {
		return nil, nil
	}
	id, rerr := top.uuid(field)
	if rerr != nil {
		return nil, invalid("%s", message)
	}
	return &id, nil
}

// variableReply is a variable as the API answers it (openapi.yaml's
// Variable).
type variableReply struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Value       string      `json:"value"`
	Description *string     `json:"description"`
	ChangedBy   *actorReply `json:"changedBy"`
	ChangedAt   string      `json:"changedAt"`
}

func variableJSON(v variables.Variable) variableReply {
	return variableReply{
		ID: v.ID.String(), Name: v.Name, Value: v.Value, Description: v.Description,
		ChangedBy: actorJSON(v.ChangedBy), ChangedAt: formatTime(v.ChangedAt),
	}
}

type variableListReply struct {
	Variables []variableReply `json:"variables"`
}

// variableGroupReply is a variable group as the API answers it
// (openapi.yaml's VariableGroup).
type variableGroupReply struct {
	ID            string                  `json:"id"`
	Name          string                  `json:"name"`
	Description   *string                 `json:"description"`
	OverrideCount int                     `json:"overrideCount"`
	Usage         variableGroupUsageReply `json:"usage"`
	ChangedBy     *actorReply             `json:"changedBy"`
	ChangedAt     string                  `json:"changedAt"`
}

type variableGroupUsageReply struct {
	CollectorGroups  int `json:"collectorGroups"`
	PinnedCollectors int `json:"pinnedCollectors"`
}

func variableGroupJSON(g variables.Group) variableGroupReply {
	return variableGroupReply{
		ID: g.ID.String(), Name: g.Name, Description: g.Description, OverrideCount: g.OverrideCount,
		Usage:     variableGroupUsageReply{CollectorGroups: g.Usage.CollectorGroups, PinnedCollectors: g.Usage.PinnedCollectors},
		ChangedBy: actorJSON(g.ChangedBy), ChangedAt: formatTime(g.ChangedAt),
	}
}

type variableGroupListReply struct {
	VariableGroups []variableGroupReply `json:"variableGroups"`
}

// overrideReply is an override as the API answers it (openapi.yaml's
// VariableGroupOverride): always with a value, since a reset row is never
// answered.
type overrideReply struct {
	VariableID string      `json:"variableId"`
	Value      string      `json:"value"`
	ChangedBy  *actorReply `json:"changedBy"`
	ChangedAt  string      `json:"changedAt"`
}

func overrideJSON(o variables.Override) overrideReply {
	return overrideReply{VariableID: o.VariableID.String(), Value: o.Value, ChangedBy: actorJSON(o.ChangedBy), ChangedAt: formatTime(o.ChangedAt)}
}

// variableGroupDetailReply is a variable group's read: the group and its
// overrides with a value, by their variable's name.
type variableGroupDetailReply struct {
	VariableGroup variableGroupReply `json:"variableGroup"`
	Overrides     []overrideReply    `json:"overrides"`
}

// The three guards' details (openapi.yaml's VariableInUseDetails,
// VariableGroupInUseDetails and VariablesUndefinedDetails), bounded: at
// most variableuse.MaxListed entries, with exact totals. Ids, counts and
// names only. A group is usage.go's
// groupRefReply.

type variableUseReply struct {
	PublicationID    string          `json:"publicationId"`
	Groups           []groupRefReply `json:"groups"`
	PinnedCollectors int             `json:"pinnedCollectors"`
}

type variableInUseDetails struct {
	UsedBy                []variableUseReply `json:"usedBy"`
	TotalPublications     int                `json:"totalPublications"`
	TotalGroups           int                `json:"totalGroups"`
	TotalPinnedCollectors int                `json:"totalPinnedCollectors"`
}

type variableGroupInUseDetails struct {
	CollectorGroups      []groupRefReply `json:"collectorGroups"`
	TotalCollectorGroups int             `json:"totalCollectorGroups"`
	PinnedCollectors     int             `json:"pinnedCollectors"`
}

type variablesUndefinedDetails struct {
	Names      []string `json:"names"`
	TotalNames int      `json:"totalNames"`
}

func groupRefsJSON(refs []variableuse.GroupRef) []groupRefReply {
	out := make([]groupRefReply, 0, len(refs))
	for _, g := range refs {
		out = append(out, groupRefReply{FleetID: g.FleetID.String(), GroupID: g.GroupID.String()})
	}
	return out
}

func variableInUseJSON(u variableuse.Uses) variableInUseDetails {
	details := variableInUseDetails{
		UsedBy:            make([]variableUseReply, 0, len(u.List)),
		TotalPublications: u.Publications, TotalGroups: u.Groups, TotalPinnedCollectors: u.PinnedCollectors,
	}
	for _, use := range u.List {
		details.UsedBy = append(details.UsedBy, variableUseReply{
			PublicationID: use.PublicationID.String(), Groups: groupRefsJSON(use.Groups), PinnedCollectors: use.PinnedCollectors,
		})
	}
	return details
}

func variableGroupInUseJSON(g variableuse.GroupUses) variableGroupInUseDetails {
	return variableGroupInUseDetails{
		CollectorGroups: groupRefsJSON(g.CollectorGroups), TotalCollectorGroups: g.TotalCollectorGroups, PinnedCollectors: g.PinnedCollectors,
	}
}

func undefinedJSON(n variableuse.UndefinedNames) variablesUndefinedDetails {
	return variablesUndefinedDetails{Names: append(make([]string, 0, len(n.Names)), n.Names...), TotalNames: n.Total}
}
