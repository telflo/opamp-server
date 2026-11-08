package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/telflo/opamp-server/store/publications"
)

// The publication routes. api/openapi.yaml documents each, and the
// conformance test fails on a route registered here that it doesn't. Each
// answers 404 organization_not_found for an organization that isn't
// registered, once its path ids are read and before anything else.
const (
	publicationPath               = apiPrefix + "v1/organizations/{organizationId}/publications/{publicationId}"
	publicationsPath              = apiPrefix + "v1/organizations/{organizationId}/publications"
	configurationPublicationsPath = apiPrefix + "v1/organizations/{organizationId}/configurations/{configurationId}/publications"
)

// The list's page size.
const (
	defaultListLimit = 50
	maxListLimit     = publications.MaxListLimit
)

// messageConflict is the one answer to a POST whose id is taken, whether by
// a different publication of the caller's organization or by another
// organization's: it says nothing about the row.
const messageConflict = "A different publication already has this id."

// messageReferenceTooLong answers a POST whose yaml has a live ${org:NAME}
// reference to a name over 128 characters, which no variable can have: it
// names the field, never the name.
const messageReferenceTooLong = "yaml references an organization variable name over 128 characters, which no variable can have."

// registerPublications registers the five publication routes on the API's
// router, behind authentication and the body cap.
func (s *Server) registerPublications() {
	s.api.handle("POST "+publicationsPath, http.HandlerFunc(s.postPublication))
	s.api.handle("GET "+publicationPath, getOnly(s.getPublication))
	s.api.handle("DELETE "+publicationPath, http.HandlerFunc(s.deletePublication))
	s.api.handle("GET "+publicationsPath, getOnly(s.listPublications))
	s.api.handle("DELETE "+configurationPublicationsPath, http.HandlerFunc(s.deleteConfigurationPublications))
}

// getOnly answers HEAD with 404: ServeMux routes HEAD to a GET pattern, but
// the API documents no HEAD, and an unmatched method is a 404.
func getOnly(handler http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeNotFound(w)
			return
		}
		handler(w, r)
	})
}

// postPublication is POST …/publications, with or without an id: 201
// stored, 200 an identical publication already stored under the id, 400 (a
// yaml referencing a name over 128 characters among them), 404
// organization_not_found, 409 the id taken by a different publication, 413
// or 422. Without an id the server makes one, so such a POST can't be
// safely retried (as an organization's or a fleet's).
func (s *Server) postPublication(w http.ResponseWriter, r *http.Request) {
	organizationID, rerr := pathUUID(r, "organizationId")
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	if !s.requireOrganization(w, r, organizationID) {
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
	publication, hasID, rerr := decodePublication(body)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	if !hasID {
		if publication.ID, ok = s.newID(w, r); !ok {
			return
		}
	}
	publication.OrganizationID = organizationID
	// When this server received it, by its injected clock (CONTRIBUTING.md,
	// "Time"), at the precision the database keeps.
	publication.ReceivedAt = s.clock().UTC().Truncate(publications.Precision)

	result, stored, err := s.publications.Put(r.Context(), publication)
	switch {
	case errors.Is(err, publications.ErrOrganizationNotFound):
		// The organization was deleted since requireOrganization asked.
		writeOrganizationNotFound(w)
	case errors.Is(err, publications.ErrReferenceTooLong):
		writeError(w, http.StatusBadRequest, codeInvalidRequest, messageReferenceTooLong)
	case err != nil:
		s.storeFailed(w, r, storePublications, err)
	case result == publications.Created:
		writeJSON(w, http.StatusCreated, publicationJSON(stored, true))
	case result == publications.Unchanged:
		writeJSON(w, http.StatusOK, publicationJSON(stored, true))
	default:
		writeError(w, http.StatusConflict, codePublicationConflict, messageConflict)
	}
}

// getPublication is GET …/publications/{publicationId}: 200 with the YAML,
// or 404, also for another organization's id.
func (s *Server) getPublication(w http.ResponseWriter, r *http.Request) {
	organizationID, publicationID, rerr := organizationAndPublication(r)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	if !s.requireOrganization(w, r, organizationID) {
		return
	}
	stored, err := s.publications.Get(r.Context(), organizationID, publicationID)
	switch {
	case errors.Is(err, publications.ErrNotFound):
		writeNotFound(w)
	case err != nil:
		s.storeFailed(w, r, storePublications, err)
	default:
		writeJSON(w, http.StatusOK, publicationJSON(stored, true))
	}
}

// The in-use refusals' fixed messages.
const (
	messagePublicationInUse              = "A collector group or a collector uses this published version."
	messageConfigurationPublicationInUse = "A collector group or a collector uses one of the configuration's published versions."
)

// deletePublication is DELETE …/publications/{publicationId}: 204, or 404,
// also for another organization's id (which survives); or 409
// publication_in_use, deleting nothing, while a collector group is
// assigned it or a collector pinned to it.
func (s *Server) deletePublication(w http.ResponseWriter, r *http.Request) {
	organizationID, publicationID, rerr := organizationAndPublication(r)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	if !s.requireOrganization(w, r, organizationID) {
		return
	}
	deleted, err := s.publications.Delete(r.Context(), organizationID, publicationID)
	switch {
	case errors.Is(err, publications.ErrInUse):
		writeError(w, http.StatusConflict, codePublicationInUse, messagePublicationInUse)
	case err != nil:
		s.storeFailed(w, r, storePublications, err)
	case !deleted:
		writeNotFound(w)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// deleteConfigurationPublications is DELETE
// …/configurations/{configurationId}/publications: 204 whether or not any
// existed, once the organization is registered; or 409 publication_in_use,
// deleting none, while a collector group or a collector uses any of them.
func (s *Server) deleteConfigurationPublications(w http.ResponseWriter, r *http.Request) {
	organizationID, rerr := pathUUID(r, "organizationId")
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	configurationID, rerr := pathUUID(r, "configurationId")
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	if !s.requireOrganization(w, r, organizationID) {
		return
	}
	_, err := s.publications.DeleteForConfiguration(r.Context(), organizationID, configurationID)
	switch {
	case errors.Is(err, publications.ErrInUse):
		writeError(w, http.StatusConflict, codePublicationInUse, messageConfigurationPublicationInUse)
		return
	case err != nil:
		s.storeFailed(w, r, storePublications, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listPublications is GET …/publications?configurationId=…&limit=…&cursor=…:
// 200 with a page, newest first, without the YAML.
func (s *Server) listPublications(w http.ResponseWriter, r *http.Request) {
	organizationID, rerr := pathUUID(r, "organizationId")
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	if !s.requireOrganization(w, r, organizationID) {
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeRequestError(w, invalid("The query string couldn't be read."))
		return
	}

	rawConfigurationID, rerr := queryValue(query, "configurationId")
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	configurationID, ok := parseUUID(rawConfigurationID)
	if !ok {
		// Also when it is missing: it is required.
		writeRequestError(w, invalid("configurationId must be a UUID."))
		return
	}

	limit := defaultListLimit
	rawLimit, rerr := queryValue(query, "limit")
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	if _, present := query["limit"]; present {
		if limit, ok = parseLimit(rawLimit); !ok {
			writeRequestError(w, invalid("limit must be an integer from 1 to %d.", maxListLimit))
			return
		}
	}

	var after *publications.Cursor
	rawCursor, rerr := queryValue(query, "cursor")
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	if _, present := query["cursor"]; present {
		cursor, err := publications.ParseCursor(rawCursor)
		if err != nil {
			writeRequestError(w, invalid("cursor isn't one this API gave."))
			return
		}
		after = &cursor
	}

	page, err := s.publications.List(r.Context(), organizationID, configurationID, limit, after)
	if err != nil {
		s.storeFailed(w, r, storePublications, err)
		return
	}
	reply := listJSON{Publications: make([]publicationReply, 0, len(page.Publications))}
	for _, p := range page.Publications {
		reply.Publications = append(reply.Publications, publicationJSON(p, false))
	}
	if page.Next != nil {
		next := page.Next.String()
		reply.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, reply)
}

// organizationAndPublication reads the two path ids, or answers 400.
func organizationAndPublication(r *http.Request) (organizationID, publicationID uuid.UUID, err *requestError) {
	if organizationID, err = pathUUID(r, "organizationId"); err != nil {
		return
	}
	publicationID, err = pathUUID(r, "publicationId")
	return
}

// queryValue reads a query parameter given at most once. A missing one is
// "", for the caller to decide on; a repeated one is a 400.
func queryValue(query url.Values, name string) (string, *requestError) {
	values := query[name]
	if len(values) > 1 {
		return "", invalid("%s must be given once.", name)
	}
	if len(values) == 0 {
		return "", nil
	}
	return values[0], nil
}

var digits = regexp.MustCompile(`^[0-9]{1,3}$`)

func parseLimit(raw string) (int, bool) {
	if !digits.MatchString(raw) {
		return 0, false
	}
	limit, err := strconv.Atoi(raw)
	return limit, err == nil && limit >= 1 && limit <= maxListLimit
}

// The stores storeFailed names.
const (
	storeOrganizations = "organizations"
	storePublications  = "publications"
)

// storeFailed answers 500 for a store that failed. The log line names the
// store, the route and the SQLSTATE, and nothing of the error's text, which
// a driver could fill with anything (the store's own errors quote no value,
// but this doesn't rely on it).
func (s *Server) storeFailed(w http.ResponseWriter, r *http.Request, store string, err error) {
	attributes := []any{"route", s.routePattern(r)}
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr):
		attributes = append(attributes, "sqlstate", pgErr.Code)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		attributes = append(attributes, "cause", "the request's context ended")
	}
	s.logger.Error("the "+store+" store failed", attributes...)
	writeError(w, http.StatusInternalServerError, codeInternalError, messageInternalError)
}

// publicationReply is a publication as the API answers it (openapi.yaml's
// Publication, or PublicationSummary without yaml).
type publicationReply struct {
	ID             string             `json:"id"`
	OrganizationID string             `json:"organizationId"`
	Configuration  configurationReply `json:"configuration"`
	Version        versionReply       `json:"version"`
	YAML           *string            `json:"yaml,omitempty"`
	SHA256         string             `json:"sha256"`
	PublishedBy    *actorReply        `json:"publishedBy"`
	PublishedAt    string             `json:"publishedAt"`
	ReceivedAt     string             `json:"receivedAt"`
	// Collector is the collector it was published for, in every reply, the
	// list's summaries included.
	Collector collectorReply `json:"collector"`
}

type configurationReply struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type versionReply struct {
	ID     string  `json:"id"`
	Number int32   `json:"number"`
	Label  *string `json:"label"`
}

type actorReply struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

type collectorReply struct {
	Distribution string `json:"distribution"`
	Version      string `json:"version"`
}

type listJSON struct {
	Publications []publicationReply `json:"publications"`
	NextCursor   *string            `json:"nextCursor"`
}

// timeLayout is RFC 3339 with milliseconds, in UTC.
const timeLayout = "2006-01-02T15:04:05.000Z07:00"

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func publicationJSON(p publications.Publication, withYAML bool) publicationReply {
	reply := publicationReply{
		ID:             p.ID.String(),
		OrganizationID: p.OrganizationID.String(),
		Configuration:  configurationReply{ID: p.ConfigurationID.String(), Name: p.ConfigurationName},
		Version:        versionReply{ID: p.VersionID.String(), Number: p.VersionNumber, Label: p.VersionLabel},
		SHA256:         p.SHA256,
		PublishedAt:    formatTime(p.PublishedAt),
		ReceivedAt:     formatTime(p.ReceivedAt),
		Collector:      collectorReply{Distribution: p.Collector.Distribution, Version: p.Collector.Version},
	}
	if withYAML {
		yaml := p.YAML
		reply.YAML = &yaml
	}
	if p.PublishedBy != nil {
		reply.PublishedBy = &actorReply{ID: p.PublishedBy.ID.String(), Email: p.PublishedBy.Email}
	}
	return reply
}

// writeJSON answers status with value as JSON. HTML characters are left
// as they are: the YAML comes back exactly as it was put.
func writeJSON(w http.ResponseWriter, status int, value any) {
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		writeError(w, http.StatusInternalServerError, codeInternalError, messageInternalError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(bytes.TrimSuffix(body.Bytes(), []byte("\n")))
}
