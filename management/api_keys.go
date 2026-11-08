package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/apikeys"
	"github.com/telflo/opamp-server/store/collectorgroups"
)

// The API key routes: the secret holder mints, reads, renames and revokes
// keys. They take the secret only: a key calling them is answered 403
// (authorize). A key is answered once, by its create, with `Cache-Control:
// no-store`; the server keeps only its secret's hash.
const (
	apiKeysPath      = apiPrefix + "v1/api-keys"
	apiKeyPath       = apiKeysPath + "/{apiKeyId}"
	revokeAPIKeyPath = apiKeyPath + "/revoke"
)

// The store storeFailed names.
const storeAPIKeys = "API keys"

// The API key routes' messages.
const (
	messageAPIKeyName            = "name must be 1 to 80 characters, with no leading or trailing whitespace and no control character."
	messageAPIKeyDescription     = "description must be 1 to 1000 characters, with no leading or trailing whitespace and no control character, or null."
	messageAPIKeyOrganizationIDs = "organizationIds must be an array of 1 to 1000 distinct UUIDs, or null for every organization."
	messageAPIKeyExpiresAt       = "expiresAt must be in the future, and at most 2 years away."
	messageNoAPIKeyChange        = "The request body must name name or description."
	messageAPIKeyNotFound        = "No API key has this id."
	messageAPIKeyLimitReached    = "100 API keys are valid already: revoke one first."
	messageAPIKeysUnregistered   = "An organization in organizationIds isn't registered: register it with POST /api/v1/organizations first."
)

func (s *Server) registerAPIKeys() {
	s.api.handle("POST "+apiKeysPath, http.HandlerFunc(s.createAPIKey))
	s.api.handle("GET "+apiKeysPath, getOnly(s.listAPIKeys))
	s.api.handle("GET "+apiKeyPath, getOnly(s.getAPIKey))
	s.api.handle("PATCH "+apiKeyPath, http.HandlerFunc(s.updateAPIKey))
	s.api.handle("POST "+revokeAPIKeyPath, http.HandlerFunc(s.revokeAPIKey))
}

// createAPIKey is POST /api/v1/api-keys with {"name", "description",
// "organizationIds", "expiresAt"}: 201 with the key, answered this once;
// 404 organization_not_found for an organization that isn't registered;
// 409 api_key_limit_reached while 100 keys are valid.
func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readJSON(w, r)
	if !ok {
		return
	}
	now := s.clock()
	k, rerr := decodeAPIKeyCreate(body, now)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	id, key, sum, err := apikeys.Generate()
	if err != nil {
		s.storeFailed(w, r, storeAPIKeys, err)
		return
	}
	k.ID, k.SecretSHA256, k.CreatedAt = id, sum, now
	stored, err := s.apiKeys.Create(r.Context(), k)
	switch {
	case errors.Is(err, apikeys.ErrLimitReached):
		writeError(w, http.StatusConflict, codeAPIKeyLimitReached, messageAPIKeyLimitReached)
		return
	case errors.Is(err, apikeys.ErrOrganizationNotFound):
		writeError(w, http.StatusNotFound, codeOrganizationNotFound, messageAPIKeysUnregistered)
		return
	case err != nil:
		s.storeFailed(w, r, storeAPIKeys, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, createdAPIKeyReply{apiKeyReply: apiKeyJSON(stored, now), Key: key})
}

// listAPIKeys is GET /api/v1/api-keys: 200 with every key, revoked and
// expired ones too, newest first. Never a key's secret.
func (s *Server) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.apiKeys.List(r.Context())
	if err != nil {
		s.storeFailed(w, r, storeAPIKeys, err)
		return
	}
	now := s.clock()
	reply := apiKeyListReply{APIKeys: make([]apiKeyReply, 0, len(keys))}
	for _, k := range keys {
		reply.APIKeys = append(reply.APIKeys, apiKeyJSON(k, now))
	}
	writeJSON(w, http.StatusOK, reply)
}

// getAPIKey is GET /api/v1/api-keys/{apiKeyId}: 200 with the key; 404
// api_key_not_found.
func (s *Server) getAPIKey(w http.ResponseWriter, r *http.Request) {
	id, ok := apiKeyID(w, r)
	if !ok {
		return
	}
	k, err := s.apiKeys.Get(r.Context(), id)
	s.answerAPIKey(w, r, k, err)
}

// updateAPIKey is PATCH /api/v1/api-keys/{apiKeyId} with "name" (not null)
// and "description" (null clears), at least one: 200 with the key; 404
// api_key_not_found. A revoked or expired key may still be renamed.
func (s *Server) updateAPIKey(w http.ResponseWriter, r *http.Request) {
	id, ok := apiKeyID(w, r)
	if !ok {
		return
	}
	body, ok := s.readJSON(w, r)
	if !ok {
		return
	}
	c, rerr := decodeAPIKeyUpdate(body)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	k, err := s.apiKeys.Update(r.Context(), id, c)
	s.answerAPIKey(w, r, k, err)
}

// revokeAPIKey is POST /api/v1/api-keys/{apiKeyId}/revoke, with no body:
// 200 with the key, revoked (a second revoke keeps the first's time); 404
// api_key_not_found. The next request with it is refused, on every pod.
func (s *Server) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	id, ok := apiKeyID(w, r)
	if !ok {
		return
	}
	k, err := s.apiKeys.Revoke(r.Context(), id, s.clock())
	s.answerAPIKey(w, r, k, err)
}

// answerAPIKey answers a key's read or change: 200 with it, 404
// api_key_not_found, or the store's failure.
func (s *Server) answerAPIKey(w http.ResponseWriter, r *http.Request, k apikeys.Key, err error) {
	switch {
	case errors.Is(err, apikeys.ErrNotFound):
		writeError(w, http.StatusNotFound, codeAPIKeyNotFound, messageAPIKeyNotFound)
	case err != nil:
		s.storeFailed(w, r, storeAPIKeys, err)
	default:
		writeJSON(w, http.StatusOK, apiKeyJSON(k, s.clock()))
	}
}

// apiKeyID reads the key's path id, or answers 400.
func apiKeyID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, rerr := pathUUID(r, "apiKeyId")
	if rerr != nil {
		writeRequestError(w, rerr)
		return uuid.Nil, false
	}
	return id, true
}

// decodeAPIKeyCreate reads POST /api/v1/api-keys's body: {"name",
// "description" (nullable), "organizationIds" (null: every organization),
// "expiresAt" (after now, at most apikeys.MaxLifetimeYears later)}.
func decodeAPIKeyCreate(body []byte, now time.Time) (apikeys.NewKey, *requestError) {
	var k apikeys.NewKey
	raw, rerr := decodeBody(body)
	if rerr != nil {
		return k, rerr
	}
	top, rerr := decodeObject(raw, "", "", "name", "description", "organizationIds", "expiresAt")
	if rerr != nil {
		return k, rerr
	}
	if k.Name, rerr = apiKeyName(top); rerr != nil {
		return k, rerr
	}
	if k.Description, rerr = apiKeyDescription(top); rerr != nil {
		return k, rerr
	}
	if top.isNull("organizationIds") {
		k.AllOrganizations = true
	} else if k.OrganizationIDs, rerr = apiKeyOrganizations(top); rerr != nil {
		return k, rerr
	}
	value, rerr := top.string("expiresAt")
	if rerr != nil {
		return k, invalid("expiresAt must be an RFC 3339 date-time.")
	}
	expiresAt, rerr := parseDateTime("expiresAt", value)
	if rerr != nil {
		return k, rerr
	}
	k.ExpiresAt = millisecond(expiresAt)
	if !k.ExpiresAt.After(millisecond(now)) || k.ExpiresAt.After(apikeys.LatestExpiry(millisecond(now))) {
		return k, invalid(messageAPIKeyExpiresAt)
	}
	return k, nil
}

// decodeAPIKeyUpdate reads PATCH …/api-keys/{apiKeyId}'s body: any of
// "name" (not null) and "description" (null clears), at least one.
func decodeAPIKeyUpdate(body []byte) (apikeys.Changes, *requestError) {
	var c apikeys.Changes
	raw, rerr := decodeBody(body)
	if rerr != nil {
		return c, rerr
	}
	top, rerr := decodeObjectWithOptional(raw, "", "", nil, []string{"name", "description"})
	if rerr != nil {
		return c, rerr
	}
	if !top.has("name") && !top.has("description") {
		return c, invalid(messageNoAPIKeyChange)
	}
	if top.has("name") {
		name, rerr := apiKeyName(top)
		if rerr != nil {
			return c, rerr
		}
		c.Name = &name
	}
	if top.has("description") {
		description, rerr := apiKeyDescription(top)
		if rerr != nil {
			return c, rerr
		}
		if description == nil {
			c.Description = collectorgroups.Clear[string]()
		} else {
			c.Description = collectorgroups.SetTo(*description)
		}
	}
	return c, nil
}

// apiKeyName reads a key's name: trimmed, 1 to 80 characters, no control
// character.
func apiKeyName(top *jsonObject) (string, *requestError) {
	name, rerr := top.string("name")
	if rerr != nil || !apikeys.ValidName(name) {
		return "", invalid(messageAPIKeyName)
	}
	return name, nil
}

// apiKeyDescription reads a key's nullable description: trimmed, 1 to
// 1,000 characters, no control character.
func apiKeyDescription(top *jsonObject) (*string, *requestError) {
	if top.isNull("description") {
		return nil, nil
	}
	description, rerr := top.string("description")
	if rerr != nil || !apikeys.ValidDescription(&description) {
		return nil, invalid(messageAPIKeyDescription)
	}
	return &description, nil
}

// apiKeyOrganizations reads a key's organizations: 1 to 1,000 UUIDs, each
// once.
func apiKeyOrganizations(top *jsonObject) ([]uuid.UUID, *requestError) {
	var items []json.RawMessage
	if json.Unmarshal(top.fields["organizationIds"], &items) != nil ||
		len(items) < 1 || len(items) > apikeys.MaxOrganizations {
		return nil, invalid(messageAPIKeyOrganizationIDs)
	}
	ids := make([]uuid.UUID, 0, len(items))
	seen := make(map[uuid.UUID]bool, len(items))
	for _, item := range items {
		var value string
		if json.Unmarshal(item, &value) != nil {
			return nil, invalid(messageAPIKeyOrganizationIDs)
		}
		id, ok := parseUUID(value)
		if !ok || seen[id] {
			return nil, invalid(messageAPIKeyOrganizationIDs)
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

// apiKeyReply is a key as the API answers it (openapi.yaml's ApiKey): never
// its secret.
type apiKeyReply struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
	// OrganizationIDs is null for a key that opens every organization.
	OrganizationIDs []string `json:"organizationIds"`
	// Status is active, expired or revoked, at the server's clock.
	Status     string  `json:"status"`
	ExpiresAt  string  `json:"expiresAt"`
	RevokedAt  *string `json:"revokedAt"`
	LastUsedAt *string `json:"lastUsedAt"`
	CreatedAt  string  `json:"createdAt"`
}

// createdAPIKeyReply is a new key (openapi.yaml's ApiKeyCreated): the key
// and, this once, the key itself.
type createdAPIKeyReply struct {
	apiKeyReply
	Key string `json:"key"`
}

type apiKeyListReply struct {
	APIKeys []apiKeyReply `json:"apiKeys"`
}

// apiKeyJSON is k as the API answers it, its status at now.
func apiKeyJSON(k apikeys.Key, now time.Time) apiKeyReply {
	reply := apiKeyReply{
		ID: k.ID.String(), Name: k.Name, Description: k.Description,
		Status: "active", ExpiresAt: formatTime(k.ExpiresAt), RevokedAt: formatOptionalTime(k.RevokedAt),
		LastUsedAt: formatOptionalTime(k.LastUsedAt), CreatedAt: formatTime(k.CreatedAt),
	}
	switch {
	case k.RevokedAt != nil:
		reply.Status = "revoked"
	case !k.ValidAt(now):
		reply.Status = "expired"
	}
	if !k.AllOrganizations {
		reply.OrganizationIDs = make([]string, 0, len(k.OrganizationIDs))
		for _, id := range k.OrganizationIDs {
			reply.OrganizationIDs = append(reply.OrganizationIDs, id.String())
		}
	}
	return reply
}
