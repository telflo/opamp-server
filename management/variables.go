package management

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/variables"
)

// The organization variables' routes: an organization's variables, its
// variable groups and their overrides, one route per change.
// api/openapi.yaml documents each, and the conformance test fails on a
// route registered here that it doesn't. Every route checks, in order: its
// path ids (400), the organization (404 organization_not_found), the body
// (400, naming the field), then the store's answers. Another
// organization's variable or variable group is the same 404 as one that
// doesn't exist. A create takes the caller's id, so a retry answers 200;
// every change is last write wins. The three guards (a variable's rename
// or delete in use, a variable group's delete in use) answer 409 with
// bounded details: ids, counts and names, never a value.
const (
	variablesPath      = apiPrefix + "v1/organizations/{organizationId}/variables"
	variablePath       = variablesPath + "/{variableId}"
	variableGroupsPath = apiPrefix + "v1/organizations/{organizationId}/variable-groups"
	variableGroupPath  = variableGroupsPath + "/{variableGroupId}"
	overridePath       = variableGroupPath + "/overrides/{variableId}"
)

// The variables routes' fixed messages. None quotes a value: no name, no
// value, no description, no id.
const (
	messageVariableNotFound          = "The organization holds no variable with this id."
	messageVariableGroupNotFound     = "The organization holds no variable group with this id."
	messageVariableNameTaken         = "Another of the organization's variables has this name."
	messageVariableGroupNameTaken    = "Another of the organization's variable groups has this name."
	messageVariableInUse             = "A published version that a collector group or a collector uses references this variable: assign or pin another version first."
	messageVariableGroupInUse        = "A collector group or a collector uses this variable group: choose another for them first."
	messageVariablesUndefined        = "The published version references organization variables that aren't defined."
	messageVariableLimitReached      = "The organization has 150 variables, the most it may have."
	messageVariableGroupLimitReached = "The organization has 100 variable groups, the most it may have."
	messageVariableConflict          = "A different variable already has this id."
	messageVariableGroupConflict     = "A different variable group already has this id."
)

func (s *Server) registerVariables() {
	s.api.handle("GET "+variablesPath, getOnly(s.listVariables))
	s.api.handle("POST "+variablesPath, http.HandlerFunc(s.createVariable))
	s.api.handle("PATCH "+variablePath, http.HandlerFunc(s.updateVariable))
	s.api.handle("DELETE "+variablePath, http.HandlerFunc(s.deleteVariable))
	s.api.handle("GET "+variableGroupsPath, getOnly(s.listVariableGroups))
	s.api.handle("POST "+variableGroupsPath, http.HandlerFunc(s.createVariableGroup))
	s.api.handle("GET "+variableGroupPath, getOnly(s.getVariableGroup))
	s.api.handle("PATCH "+variableGroupPath, http.HandlerFunc(s.updateVariableGroup))
	s.api.handle("DELETE "+variableGroupPath, http.HandlerFunc(s.deleteVariableGroup))
	s.api.handle("PUT "+overridePath, http.HandlerFunc(s.setVariableGroupOverride))
	s.api.handle("DELETE "+overridePath, http.HandlerFunc(s.resetVariableGroupOverride))
}

// listVariables is GET …/variables: 200 with every variable of the
// organization, by name (byte order), values included (none is a secret).
func (s *Server) listVariables(w http.ResponseWriter, r *http.Request) {
	ids, ok := s.underOrganization(w, r)
	if !ok {
		return
	}
	list, err := s.variables.ListVariables(r.Context(), ids[0])
	if err != nil {
		s.storeFailed(w, r, storeVariables, err)
		return
	}
	reply := variableListReply{Variables: make([]variableReply, 0, len(list))}
	for _, v := range list {
		reply.Variables = append(reply.Variables, variableJSON(v))
	}
	writeJSON(w, http.StatusOK, reply)
}

// createVariable is POST …/variables with {"id"?, "name", "value",
// "description", "actor"}: 201 with the variable; 200 with it when the
// organization already holds this id with this name, value and description
// (a safe retry); 409 variable_conflict for an id taken otherwise,
// variable_name_taken, variable_limit_reached.
func (s *Server) createVariable(w http.ResponseWriter, r *http.Request) {
	ids, ok := s.underOrganization(w, r)
	if !ok {
		return
	}
	body, ok := s.readJSON(w, r)
	if !ok {
		return
	}
	v, hasID, rerr := decodeVariableCreate(body)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	if !hasID {
		// Without an id the server makes one, so such a POST can't be
		// safely retried (as a fleet's).
		if v.ID, ok = s.newID(w, r); !ok {
			return
		}
	}
	v.OrganizationID, v.At = ids[0], s.clock()
	result, stored, err := s.variables.CreateVariable(r.Context(), v)
	if s.writeVariableError(w, r, storeVariables, err) {
		return
	}
	switch result {
	case variables.Created:
		writeJSON(w, http.StatusCreated, variableJSON(stored))
	case variables.Unchanged:
		writeJSON(w, http.StatusOK, variableJSON(stored))
	default:
		writeError(w, http.StatusConflict, codeVariableConflict, messageVariableConflict)
	}
}

// updateVariable is PATCH …/variables/{variableId} with any of "name",
// "value" and "description" (null clears it), at least one, and "actor":
// 200 with the variable; 404 variable_not_found; 409 variable_name_taken,
// or variable_in_use for a rename while a used published version references
// the old name (with its details). A value change is never refused.
func (s *Server) updateVariable(w http.ResponseWriter, r *http.Request) {
	ids, ok := s.underOrganization(w, r, "variableId")
	if !ok {
		return
	}
	body, ok := s.readJSON(w, r)
	if !ok {
		return
	}
	changes, actor, rerr := decodeVariableUpdate(body)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	stored, err := s.variables.UpdateVariable(r.Context(), ids[0], ids[1], changes, actor, s.clock())
	if s.writeVariableError(w, r, storeVariables, err) {
		return
	}
	writeJSON(w, http.StatusOK, variableJSON(stored))
}

// deleteVariable is DELETE …/variables/{variableId}: 204, the variable gone
// with its overrides; 404; or 409 variable_in_use (with its details).
func (s *Server) deleteVariable(w http.ResponseWriter, r *http.Request) {
	ids, ok := s.underOrganization(w, r, "variableId")
	if !ok {
		return
	}
	err := s.variables.DeleteVariable(r.Context(), ids[0], ids[1])
	if s.writeVariableError(w, r, storeVariables, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listVariableGroups is GET …/variable-groups: 200 with every variable
// group of the organization, by name, each with its override count and what
// uses it.
func (s *Server) listVariableGroups(w http.ResponseWriter, r *http.Request) {
	ids, ok := s.underOrganization(w, r)
	if !ok {
		return
	}
	list, err := s.variables.ListGroups(r.Context(), ids[0])
	if err != nil {
		s.storeFailed(w, r, storeVariableGroups, err)
		return
	}
	reply := variableGroupListReply{VariableGroups: make([]variableGroupReply, 0, len(list))}
	for _, g := range list {
		reply.VariableGroups = append(reply.VariableGroups, variableGroupJSON(g))
	}
	writeJSON(w, http.StatusOK, reply)
}

// createVariableGroup is POST …/variable-groups with {"id"?, "name",
// "description", "actor"}: 201; 200 for the same id, name and description;
// 409 variable_group_conflict, variable_group_name_taken,
// variable_group_limit_reached.
func (s *Server) createVariableGroup(w http.ResponseWriter, r *http.Request) {
	ids, ok := s.underOrganization(w, r)
	if !ok {
		return
	}
	body, ok := s.readJSON(w, r)
	if !ok {
		return
	}
	g, hasID, rerr := decodeVariableGroupCreate(body)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	if !hasID {
		if g.ID, ok = s.newID(w, r); !ok {
			return
		}
	}
	g.OrganizationID, g.At = ids[0], s.clock()
	result, stored, err := s.variables.CreateGroup(r.Context(), g)
	if s.writeVariableError(w, r, storeVariableGroups, err) {
		return
	}
	switch result {
	case variables.Created:
		writeJSON(w, http.StatusCreated, variableGroupJSON(stored))
	case variables.Unchanged:
		writeJSON(w, http.StatusOK, variableGroupJSON(stored))
	default:
		writeError(w, http.StatusConflict, codeVariableGroupConflict, messageVariableGroupConflict)
	}
}

// getVariableGroup is GET …/variable-groups/{variableGroupId}: 200 with the
// group and its overrides with a value (never a reset row), by their
// variable's name; or 404 variable_group_not_found.
func (s *Server) getVariableGroup(w http.ResponseWriter, r *http.Request) {
	ids, ok := s.underOrganization(w, r, "variableGroupId")
	if !ok {
		return
	}
	g, overrides, err := s.variables.GetGroup(r.Context(), ids[0], ids[1])
	if s.writeVariableError(w, r, storeVariableGroups, err) {
		return
	}
	reply := variableGroupDetailReply{VariableGroup: variableGroupJSON(g), Overrides: make([]overrideReply, 0, len(overrides))}
	for _, o := range overrides {
		reply.Overrides = append(reply.Overrides, overrideJSON(o))
	}
	writeJSON(w, http.StatusOK, reply)
}

// updateVariableGroup is PATCH …/variable-groups/{variableGroupId} with any
// of "name" and "description" (null clears it), at least one, and "actor":
// 200; 404; 409 variable_group_name_taken.
func (s *Server) updateVariableGroup(w http.ResponseWriter, r *http.Request) {
	ids, ok := s.underOrganization(w, r, "variableGroupId")
	if !ok {
		return
	}
	body, ok := s.readJSON(w, r)
	if !ok {
		return
	}
	changes, actor, rerr := decodeVariableGroupUpdate(body)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	stored, err := s.variables.UpdateGroup(r.Context(), ids[0], ids[1], changes, actor, s.clock())
	if s.writeVariableError(w, r, storeVariableGroups, err) {
		return
	}
	writeJSON(w, http.StatusOK, variableGroupJSON(stored))
}

// deleteVariableGroup is DELETE …/variable-groups/{variableGroupId}: 204,
// the group gone with its overrides; 404; or 409 variable_group_in_use
// while a collector group or a collector uses it (with its details).
func (s *Server) deleteVariableGroup(w http.ResponseWriter, r *http.Request) {
	ids, ok := s.underOrganization(w, r, "variableGroupId")
	if !ok {
		return
	}
	err := s.variables.DeleteGroup(r.Context(), ids[0], ids[1])
	if s.writeVariableError(w, r, storeVariableGroups, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setVariableGroupOverride is PUT …/variable-groups/{variableGroupId}/
// overrides/{variableId} with {"value", "actor"}: 201 with the override
// when the group had none of the variable (a reset row revived included);
// 200 when it replaced one, or when the value was the same (then nothing
// changed); 404 variable_group_not_found or variable_not_found.
func (s *Server) setVariableGroupOverride(w http.ResponseWriter, r *http.Request) {
	ids, ok := s.underOrganization(w, r, "variableGroupId", "variableId")
	if !ok {
		return
	}
	body, ok := s.readJSON(w, r)
	if !ok {
		return
	}
	value, actor, rerr := decodeOverride(body)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	o, created, err := s.variables.SetOverride(r.Context(), ids[0], ids[1], ids[2], value, actor, s.clock())
	if s.writeVariableError(w, r, storeVariableGroups, err) {
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, overrideJSON(o))
}

// resetVariableGroupOverride is DELETE …/variable-groups/{variableGroupId}/
// overrides/{variableId} with {"actor"} (a reset records who made it, as
// the reset row's and its variable group's ChangedBy; in the body, never
// the URL): 204, the override reset (its row kept with no value) or none
// to reset; 400 for a missing or malformed actor; 404
// variable_group_not_found or variable_not_found.
func (s *Server) resetVariableGroupOverride(w http.ResponseWriter, r *http.Request) {
	ids, ok := s.underOrganization(w, r, "variableGroupId", "variableId")
	if !ok {
		return
	}
	body, ok := s.readJSON(w, r)
	if !ok {
		return
	}
	actor, rerr := decodeActorOnly(body)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	_, err := s.variables.ResetOverride(r.Context(), ids[0], ids[1], ids[2], actor, s.clock())
	if s.writeVariableError(w, r, storeVariableGroups, err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeVariableError answers a variables store's refusal, or a failure
// (logged as store's), and reports whether it answered (false: err is nil).
func (s *Server) writeVariableError(w http.ResponseWriter, r *http.Request, store string, err error) bool {
	var inUse *variables.InUseError
	var groupInUse *variables.GroupInUseError
	switch {
	case err == nil:
		return false
	case errors.Is(err, variables.ErrOrganizationNotFound):
		// The organization was deleted since requireOrganization asked.
		writeOrganizationNotFound(w)
	case errors.Is(err, variables.ErrNotFound):
		writeError(w, http.StatusNotFound, codeVariableNotFound, messageVariableNotFound)
	case errors.Is(err, variables.ErrGroupNotFound):
		writeError(w, http.StatusNotFound, codeVariableGroupNotFound, messageVariableGroupNotFound)
	case errors.Is(err, variables.ErrNameTaken):
		writeError(w, http.StatusConflict, codeVariableNameTaken, messageVariableNameTaken)
	case errors.Is(err, variables.ErrGroupNameTaken):
		writeError(w, http.StatusConflict, codeVariableGroupNameTaken, messageVariableGroupNameTaken)
	case errors.Is(err, variables.ErrLimitReached):
		writeError(w, http.StatusConflict, codeVariableLimitReached, messageVariableLimitReached)
	case errors.Is(err, variables.ErrGroupLimitReached):
		writeError(w, http.StatusConflict, codeVariableGroupLimitReached, messageVariableGroupLimitReached)
	case errors.As(err, &inUse):
		writeErrorDetails(w, http.StatusConflict, codeVariableInUse, messageVariableInUse, variableInUseJSON(inUse.Uses))
	case errors.As(err, &groupInUse):
		writeErrorDetails(w, http.StatusConflict, codeVariableGroupInUse, messageVariableGroupInUse, variableGroupInUseJSON(groupInUse.Use))
	case errors.Is(err, variables.ErrInvalid):
		writeRequestError(w, invalid(messageUnstorable))
	default:
		s.storeFailed(w, r, store, err)
	}
	return true
}

// newID makes an id for a create that named none; on failure it has
// answered 500.
func (s *Server) newID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.NewRandom()
	if err != nil {
		s.logger.Error("couldn't generate an id", "route", s.routePattern(r))
		writeError(w, http.StatusInternalServerError, codeInternalError, messageInternalError)
		return uuid.Nil, false
	}
	return id, true
}
