package management

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/collectorgroups"
	"github.com/telflo/opamp-server/store/variableuse"
)

// The collector group routes: a fleet's groups, the Default group first. A
// group's assigned published version is served to its collectors without a
// pin of their own.
const (
	collectorGroupsPath = fleetPath + "/groups"
	collectorGroupPath  = collectorGroupsPath + "/{groupId}"
)

// The group routes' fixed messages.
const (
	messageCollectorGroupNotFound  = "The fleet has no collector group with this id."
	messageCollectorGroupNameTaken = "Another of the fleet's collector groups has this name."
	messageCollectorGroupIsDefault = "The fleet's Default group can't be renamed or deleted."
	messagePublicationNotFound     = "The organization holds no published version with this id."
	// messageUnstorable answers a value a store refused that the handler's
	// own checks let through: it shouldn't happen, and says no more.
	messageUnstorable = "The request has a value the server can't store."
)

func (s *Server) registerCollectorGroups() {
	s.api.handle("GET "+collectorGroupsPath, getOnly(s.listCollectorGroups))
	s.api.handle("POST "+collectorGroupsPath, http.HandlerFunc(s.createCollectorGroup))
	s.api.handle("GET "+collectorGroupPath, getOnly(s.getCollectorGroup))
	s.api.handle("PATCH "+collectorGroupPath, http.HandlerFunc(s.updateCollectorGroup))
	s.api.handle("DELETE "+collectorGroupPath, http.HandlerFunc(s.deleteCollectorGroup))
}

// listCollectorGroups is GET …/groups: 200 with the fleet's groups, the
// Default group first, then by name; or 404 fleet_not_found.
func (s *Server) listCollectorGroups(w http.ResponseWriter, r *http.Request) {
	organizationID, fleetID, ok := s.organizationAndFleet(w, r)
	if !ok {
		return
	}
	groups, err := s.groups.List(r.Context(), organizationID, fleetID)
	switch {
	case errors.Is(err, collectorgroups.ErrFleetNotFound):
		writeFleetNotFound(w)
		return
	case err != nil:
		s.storeFailed(w, r, storeCollectorGroups, err)
		return
	}
	reply := groupListReply{Groups: make([]groupReply, 0, len(groups))}
	for _, g := range groups {
		reply.Groups = append(reply.Groups, groupJSON(g))
	}
	writeJSON(w, http.StatusOK, reply)
}

// createCollectorGroup is POST …/groups with {"name", and optionally
// "assignedPublicationId", "annotations", "variableGroupId"} (annotations
// on any fleet's group): 201 with the group; 404 fleet_not_found; 409
// collector_group_name_taken; 422 publication_not_found for a published
// version the organization doesn't hold, variable_group_not_found for a
// variable group it doesn't hold, and variables_undefined for a published
// version referencing a name no variable of the organization has (with
// the names in its details).
func (s *Server) createCollectorGroup(w http.ResponseWriter, r *http.Request) {
	organizationID, fleetID, ok := s.organizationAndFleet(w, r)
	if !ok {
		return
	}
	body, ok := s.readJSON(w, r)
	if !ok {
		return
	}
	group, rerr := decodeCollectorGroupCreate(body)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	group.OrganizationID, group.FleetID, group.At = organizationID, fleetID, s.clock()
	stored, err := s.groups.Create(r.Context(), group)
	if errors.Is(err, collectorgroups.ErrFleetNotFound) {
		writeFleetNotFound(w)
		return
	}
	if s.writeGroupError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusCreated, groupJSON(stored))
}

// getCollectorGroup is GET …/groups/{groupId}: 200 with the group, or 404.
func (s *Server) getCollectorGroup(w http.ResponseWriter, r *http.Request) {
	organizationID, fleetID, groupID, ok := s.organizationFleetAndGroup(w, r)
	if !ok {
		return
	}
	stored, err := s.groups.Get(r.Context(), organizationID, fleetID, groupID)
	switch {
	case errors.Is(err, collectorgroups.ErrNotFound):
		s.notFoundOrFleetNotFound(w, r, organizationID, fleetID, codeCollectorGroupNotFound, messageCollectorGroupNotFound)
	case err != nil:
		s.storeFailed(w, r, storeCollectorGroups, err)
	default:
		writeJSON(w, http.StatusOK, groupJSON(stored))
	}
}

// updateCollectorGroup is PATCH …/groups/{groupId} with any of "name",
// "assignedPublicationId", "annotations" and "variableGroupId" (null clears
// all but the name), at least one: 200 with the group; 404;
// 409 collector_group_name_taken, or collector_group_is_default for a new
// name on the Default group; 422 publication_not_found,
// variable_group_not_found or variables_undefined (a newly assigned
// published version only).
func (s *Server) updateCollectorGroup(w http.ResponseWriter, r *http.Request) {
	organizationID, fleetID, groupID, ok := s.organizationFleetAndGroup(w, r)
	if !ok {
		return
	}
	body, ok := s.readJSON(w, r)
	if !ok {
		return
	}
	changes, rerr := decodeCollectorGroupUpdate(body)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	stored, err := s.groups.Update(r.Context(), organizationID, fleetID, groupID, changes, s.clock())
	if errors.Is(err, collectorgroups.ErrNotFound) {
		s.notFoundOrFleetNotFound(w, r, organizationID, fleetID, codeCollectorGroupNotFound, messageCollectorGroupNotFound)
		return
	}
	if s.writeGroupError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, groupJSON(stored))
}

// deleteCollectorGroup is DELETE …/groups/{groupId}: 204, its collectors
// (and their error history) deleted with it in the same transaction; 404;
// or 409 collector_group_is_default for the Default group, deleting
// nothing. Like a fleet's delete it runs on deleteContext (DeleteTimeout),
// finishing once started even if the caller hangs up.
func (s *Server) deleteCollectorGroup(w http.ResponseWriter, r *http.Request) {
	organizationID, fleetID, groupID, ok := s.organizationFleetAndGroup(w, r)
	if !ok {
		return
	}
	ctx, cancel := s.deleteContext(r)
	defer cancel()
	deleted, err := s.groups.Delete(ctx, organizationID, fleetID, groupID)
	switch {
	case errors.Is(err, collectorgroups.ErrIsDefault):
		writeError(w, http.StatusConflict, codeCollectorGroupIsDefault, messageCollectorGroupIsDefault)
	case err != nil:
		s.deleteFailed(w, r, storeCollectorGroups, ctx, err)
	case !deleted:
		// On the delete's context too: a caller that hung up meanwhile is
		// answered (if still there) without a failure logged for it.
		if err := s.writeNotFoundOrFleetNotFound(ctx, w, organizationID, fleetID, codeCollectorGroupNotFound, messageCollectorGroupNotFound); err != nil {
			s.deleteFailed(w, r, storeFleets, ctx, err)
		}
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// writeGroupError answers a create's or an update's refusal, or a failure,
// and reports whether it answered (false: err is nil).
func (s *Server) writeGroupError(w http.ResponseWriter, r *http.Request, err error) bool {
	var undefined *collectorgroups.UndefinedError
	switch {
	case err == nil:
		return false
	case errors.Is(err, collectorgroups.ErrNameTaken):
		writeError(w, http.StatusConflict, codeCollectorGroupNameTaken, messageCollectorGroupNameTaken)
	case errors.Is(err, collectorgroups.ErrIsDefault):
		writeError(w, http.StatusConflict, codeCollectorGroupIsDefault, messageCollectorGroupIsDefault)
	case errors.Is(err, collectorgroups.ErrPublicationNotFound):
		writeError(w, http.StatusUnprocessableEntity, codePublicationNotFound, messagePublicationNotFound)
	case errors.Is(err, collectorgroups.ErrVariableGroupNotFound):
		writeVariableGroupTargetNotFound(w)
	case errors.As(err, &undefined):
		writeVariablesUndefined(w, undefined.Undefined)
	case errors.Is(err, collectorgroups.ErrInvalidAnnotations):
		writeRequestError(w, invalid(messageAnnotations))
	case errors.Is(err, collectorgroups.ErrInvalid):
		writeRequestError(w, invalid(messageUnstorable))
	default:
		s.storeFailed(w, r, storeCollectorGroups, err)
	}
	return true
}

// organizationFleetAndGroup reads the three path ids (400), then requires
// the organization (404 organization_not_found). On failure it has
// answered.
func (s *Server) organizationFleetAndGroup(w http.ResponseWriter, r *http.Request) (organizationID, fleetID, groupID uuid.UUID, ok bool) {
	ids, ok := s.underOrganization(w, r, "fleetId", "groupId")
	if !ok {
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	return ids[0], ids[1], ids[2], true
}

// writeVariableGroupTargetNotFound answers 422 for a variable group named
// in a body (a group's variableGroupId, a pin's pinnedVariableGroupId) that
// the organization doesn't hold, as a published version it doesn't hold is
// answered.
func writeVariableGroupTargetNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusUnprocessableEntity, codeVariableGroupNotFound, messageVariableGroupNotFound)
}

// writeVariablesUndefined answers 422 variables_undefined with the names:
// an assignment or a pin of a published version that references names no
// variable of the organization has.
func writeVariablesUndefined(w http.ResponseWriter, names variableuse.UndefinedNames) {
	writeErrorDetails(w, http.StatusUnprocessableEntity, codeVariablesUndefined, messageVariablesUndefined, undefinedJSON(names))
}
