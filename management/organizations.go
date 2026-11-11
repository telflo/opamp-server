package management

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/telflo/telflo-opamp-server/store/organizations"
)

// The organization routes. api/openapi.yaml documents each, and the
// conformance test fails on a route registered here that it doesn't.
const (
	organizationsPath = apiPrefix + "v1/organizations"
	organizationPath  = apiPrefix + "v1/organizations/{organizationId}"
)

// maxOrganizationNameCharacters bounds an organization's display name: a
// personal organization's name carries its owner's email.
const maxOrganizationNameCharacters = 400

// messageOrganizationConflict is the one answer to a POST of an id already
// registered under another name: a create never renames, and the answer says
// nothing more about the stored organization.
const messageOrganizationConflict = "An organization with this id is already registered under another name."

// registerOrganizations registers the four organization routes on the API's
// router, behind authentication and the body cap.
func (s *Server) registerOrganizations() {
	s.api.handle("POST "+organizationsPath, http.HandlerFunc(s.createOrganization))
	s.api.handle("GET "+organizationPath, getOnly(s.getOrganization))
	s.api.handle("PATCH "+organizationPath, http.HandlerFunc(s.renameOrganization))
	s.api.handle("DELETE "+organizationPath, http.HandlerFunc(s.deleteOrganization))
}

// createOrganization is POST /api/v1/organizations with {"id"?, "name"}:
// 201 registered, 200 the id is registered under exactly this name already
// (a safe retry), 409 it is registered under another name; each success
// answers {"organizationId"}. Without an id, the server generates one, so
// such a POST can't be safely retried.
func (s *Server) createOrganization(w http.ResponseWriter, r *http.Request) {
	if rerr := checkJSONContentType(r); rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	id, name, rerr := decodeOrganizationCreate(body)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	if id == nil {
		generated, err := uuid.NewRandom()
		if err != nil {
			s.logger.Error("couldn't generate an organization id", "route", s.routePattern(r))
			writeError(w, http.StatusInternalServerError, codeInternalError, messageInternalError)
			return
		}
		id = &generated
	}
	// Registered now, by the injected clock (CONTRIBUTING.md, "Time"), at
	// the precision the database keeps.
	now := s.clock().UTC().Truncate(organizations.Precision)

	result, stored, err := s.organizations.Create(r.Context(), organizations.Organization{
		ID: *id, Name: name, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		s.storeFailed(w, r, storeOrganizations, err)
		return
	}
	switch result {
	case organizations.Created:
		writeJSON(w, http.StatusCreated, organizationIDReply{OrganizationID: stored.ID.String()})
	case organizations.Unchanged:
		writeJSON(w, http.StatusOK, organizationIDReply{OrganizationID: stored.ID.String()})
	default:
		writeError(w, http.StatusConflict, codeOrganizationConflict, messageOrganizationConflict)
	}
}

// getOrganization is GET /api/v1/organizations/{organizationId}: 200 with
// the organization, or 404.
func (s *Server) getOrganization(w http.ResponseWriter, r *http.Request) {
	organizationID, rerr := pathUUID(r, "organizationId")
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	stored, err := s.organizations.Get(r.Context(), organizationID)
	switch {
	case errors.Is(err, organizations.ErrNotFound):
		writeNotFound(w)
	case err != nil:
		s.storeFailed(w, r, storeOrganizations, err)
	default:
		writeJSON(w, http.StatusOK, organizationJSON(stored))
	}
}

// renameOrganization is PATCH /api/v1/organizations/{organizationId} with
// {"name"}: 200 with the organization, or 404. Renaming to the name it has
// changes nothing, updatedAt included.
func (s *Server) renameOrganization(w http.ResponseWriter, r *http.Request) {
	organizationID, rerr := pathUUID(r, "organizationId")
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	if rerr := checkJSONContentType(r); rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	name, rerr := decodeOrganizationRename(body)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	stored, err := s.organizations.Rename(r.Context(), organizationID, name, s.clock())
	switch {
	case errors.Is(err, organizations.ErrNotFound):
		writeNotFound(w)
	case err != nil:
		s.storeFailed(w, r, storeOrganizations, err)
	default:
		writeJSON(w, http.StatusOK, organizationJSON(stored))
	}
}

// deleteOrganization is DELETE /api/v1/organizations/{organizationId}: 204,
// the organization and everything it owns gone (a hard delete, cascading to
// its publications), or 404. Once started it finishes even if the caller
// hangs up, within DeleteTimeout (deletes.go): a retry then answers 404.
// While this replica is already deleting the organization, a second delete
// answers 503 delete_in_progress at once, before any query (deletes.go).
func (s *Server) deleteOrganization(w http.ResponseWriter, r *http.Request) {
	organizationID, rerr := pathUUID(r, "organizationId")
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	done, started := s.deleting.startOrganization(organizationID)
	if !started {
		s.writeDeleteInProgress(w, r)
		return
	}
	defer done()
	ctx, cancel := s.deleteContext(r)
	defer cancel()
	deleted, err := s.organizations.Delete(ctx, organizationID)
	switch {
	case err != nil:
		s.deleteFailed(w, r, storeOrganizations, ctx, err)
	case !deleted:
		writeNotFound(w)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// requireOrganization answers 404 organization_not_found unless the
// organization is registered, and reports whether the request may go on.
// Every publication route asks it once its path ids are read, before it
// reads anything else.
func (s *Server) requireOrganization(w http.ResponseWriter, r *http.Request, organizationID uuid.UUID) bool {
	exists, err := s.organizations.Exists(r.Context(), organizationID)
	switch {
	case err != nil:
		s.storeFailed(w, r, storeOrganizations, err)
		return false
	case !exists:
		writeOrganizationNotFound(w)
		return false
	}
	return true
}

// decodeOrganizationCreate reads a POST's body: {"name"}, with an optional
// "id" (a UUID when present; null is refused), and no other field.
func decodeOrganizationCreate(body []byte) (*uuid.UUID, string, *requestError) {
	raw, err := decodeBody(body)
	if err != nil {
		return nil, "", err
	}
	top, err := decodeObjectWithOptional(raw, "", "", []string{"name"}, []string{"id"})
	if err != nil {
		return nil, "", err
	}
	var id *uuid.UUID
	if top.has("id") {
		parsed, err := top.uuid("id")
		if err != nil {
			return nil, "", err
		}
		id = &parsed
	}
	name, err := organizationName(top)
	if err != nil {
		return nil, "", err
	}
	return id, name, nil
}

// decodeOrganizationRename reads a PATCH's body: {"name"}, and no other
// field.
func decodeOrganizationRename(body []byte) (string, *requestError) {
	raw, err := decodeBody(body)
	if err != nil {
		return "", err
	}
	top, err := decodeObject(raw, "", "", "name")
	if err != nil {
		return "", err
	}
	return organizationName(top)
}

// organizationName reads the name: 1 to 400 characters (code points), no
// U+0000. It is kept exactly as sent, neither trimmed nor refused for
// surrounding whitespace: it is display-only, the platform owns the real
// one, and a name the server rewrote would make a retried POST compare
// against something its caller never sent.
func organizationName(top *jsonObject) (string, *requestError) {
	return top.boundedString("name", 1, maxOrganizationNameCharacters)
}

// organizationIDReply is POST's answer.
type organizationIDReply struct {
	OrganizationID string `json:"organizationId"`
}

// organizationReply is an organization as GET and PATCH answer it
// (openapi.yaml's Organization).
type organizationReply struct {
	OrganizationID string `json:"organizationId"`
	Name           string `json:"name"`
	CreatedAt      string `json:"createdAt"`
	UpdatedAt      string `json:"updatedAt"`
}

func organizationJSON(o organizations.Organization) organizationReply {
	return organizationReply{
		OrganizationID: o.ID.String(),
		Name:           o.Name,
		CreatedAt:      formatTime(o.CreatedAt),
		UpdatedAt:      formatTime(o.UpdatedAt),
	}
}
