package management

import (
	"net/http"

	"github.com/telflo/telflo-opamp-server/internal/apierror"
)

// errorCode is one of the management API's error codes.
type errorCode string

const (
	codeUnauthorized        errorCode = "unauthorized"
	codeInvalidRequest      errorCode = "invalid_request"
	codeNotFound            errorCode = "not_found"
	codePublicationConflict errorCode = "publication_conflict"
	// codeOrganizationNotFound and codeOrganizationConflict are the
	// organization registry's.
	codeOrganizationNotFound errorCode = "organization_not_found"
	codeOrganizationConflict errorCode = "organization_conflict"
	codePayloadTooLarge      errorCode = "payload_too_large"
	codeSHA256Mismatch       errorCode = "sha256_mismatch"
	codeInternalError        errorCode = "internal_error"

	// The fleet model's codes.
	codeFleetNotFound           errorCode = "fleet_not_found"
	codeFleetConflict           errorCode = "fleet_conflict"
	codeFleetLabelTaken         errorCode = "fleet_label_taken"
	codeFleetArchived           errorCode = "fleet_archived"
	codeCollectorGroupNotFound  errorCode = "collector_group_not_found"
	codeCollectorGroupNameTaken errorCode = "collector_group_name_taken"
	codeCollectorGroupIsDefault errorCode = "collector_group_is_default"
	codeEnrollmentTokenNotFound errorCode = "enrollment_token_not_found"
	codeEnrollmentTokenRevoked  errorCode = "enrollment_token_revoked"
	codeEnrollmentTokenExpired  errorCode = "enrollment_token_expired"
	// Issuing a default token while the fleet has a valid one.
	codeEnrollmentTokenDefaultExists errorCode = "enrollment_token_default_exists"
	codePublicationNotFound          errorCode = "publication_not_found"
	codeCollectorNotFound            errorCode = "collector_not_found"
	codeCollectorNameTaken           errorCode = "collector_name_taken"
	codePublicationInUse             errorCode = "publication_in_use"

	// codeDeleteInProgress is an organization's or a fleet's delete this
	// replica is already running (deletes.go).
	codeDeleteInProgress errorCode = "delete_in_progress"

	// The organization variables' codes.
	codeVariableNotFound          errorCode = "variable_not_found"
	codeVariableGroupNotFound     errorCode = "variable_group_not_found"
	codeVariableNameTaken         errorCode = "variable_name_taken"
	codeVariableGroupNameTaken    errorCode = "variable_group_name_taken"
	codeVariableInUse             errorCode = "variable_in_use"
	codeVariableGroupInUse        errorCode = "variable_group_in_use"
	codeVariablesUndefined        errorCode = "variables_undefined"
	codeVariableLimitReached      errorCode = "variable_limit_reached"
	codeVariableGroupLimitReached errorCode = "variable_group_limit_reached"
	codeVariableConflict          errorCode = "variable_conflict"
	codeVariableGroupConflict     errorCode = "variable_group_conflict"

	// The API keys' codes.
	codeAPIKeyNotFound     errorCode = "api_key_not_found"
	codeAPIKeyLimitReached errorCode = "api_key_limit_reached"
)

// The fixed messages. A message never quotes a value from the request: the
// handlers' own messages name the field, never its value.
const (
	messageUnauthorized    = "Missing or invalid credentials."
	messageNotFound        = "Not found."
	messagePayloadTooLarge = "The request body is larger than 2 MiB."
	messageInternalError   = "Internal server error."
	messageUnreadableBody  = "The request body couldn't be read."
	// messageOrganizationNotFound answers every publication route under an
	// organization that isn't registered.
	messageOrganizationNotFound = "No organization with this id is registered: register it with POST /api/v1/organizations first."
	// messageDeleteInProgress answers a delete of an organization, or of a
	// fleet, that this server is already deleting (deletes.go).
	messageDeleteInProgress = "This delete is already running: retry later; once it has finished, a retry answers 404."
)

// envelope is the platform's error body: {"error":{"code","message"}},
// and "details" on the three guard codes (package apierror writes it).
type envelope = apierror.Envelope

// envelopeError is the envelope's content. Details is present only on
// variable_in_use, variable_group_in_use and variables_undefined
// (writeErrorDetails): ids, counts and variable names, never a value; the
// message stays fixed.
type envelopeError = apierror.Error

// writeError answers status with the error envelope.
func writeError(w http.ResponseWriter, status int, code errorCode, message string) {
	apierror.Write(w, status, string(code), message)
}

// writeErrorDetails answers status with the error envelope and its details,
// through the same encoder as writeError.
func writeErrorDetails(w http.ResponseWriter, status int, code errorCode, message string, details any) {
	apierror.WriteDetails(w, status, string(code), message, details)
}

// writeUnauthorized is the one answer to a missing, malformed or wrong
// Authorization header: the same body for each.
func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, codeUnauthorized, messageUnauthorized)
}

func writeNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, codeNotFound, messageNotFound)
}

func writeOrganizationNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, codeOrganizationNotFound, messageOrganizationNotFound)
}
