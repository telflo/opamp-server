// Package apierror writes the platform's error envelope,
// {"error":{"code":"…","message":"…"}}, with "details" where a caller
// gives some. The management API answers every error with it, and so do
// the diagnostics wherever they are mounted. Each caller owns its codes and
// its fixed messages; a message never quotes a value from the request.
package apierror

import (
	"bytes"
	"encoding/json"
	"net/http"
)

// The codes the shared handlers answer with (package diagnostics); the
// management API's own codes are its own.
const (
	CodeInvalidRequest = "invalid_request"
	CodeNotFound       = "not_found"
)

// MessageNotFound is CodeNotFound's fixed message.
const MessageNotFound = "Not found."

// Envelope is the error body.
type Envelope struct {
	Error Error `json:"error"`
}

// Error is the envelope's content.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Details is present only where a caller gives some (WriteDetails):
	// ids, counts and names, never a value; the message stays fixed.
	Details any `json:"details,omitempty"`
}

// Write answers status with the envelope.
func Write(w http.ResponseWriter, status int, code, message string) {
	write(w, status, Error{Code: code, Message: message})
}

// WriteDetails answers status with the envelope and its details, through
// the same encoder as Write.
func WriteDetails(w http.ResponseWriter, status int, code, message string, details any) {
	write(w, status, Error{Code: code, Message: message, Details: details})
}

// WriteNotFound answers 404 not_found.
func WriteNotFound(w http.ResponseWriter) {
	Write(w, http.StatusNotFound, CodeNotFound, MessageNotFound)
}

// write writes the envelope with HTML escaping off, so a reply's size is
// what its text is, and without the encoder's final newline.
func write(w http.ResponseWriter, status int, e Error) {
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(Envelope{Error: e}); err != nil {
		// Strings, ids and counts always marshal; this is unreachable.
		http.Error(w, "internal_error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(bytes.TrimSuffix(body.Bytes(), []byte("\n")))
}
