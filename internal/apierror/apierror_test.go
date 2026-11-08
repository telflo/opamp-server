package apierror

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The envelope is compact JSON, HTML unescaped, with no final newline, and
// details only where given.
func TestTheEnvelopeIsCompactJSONWithDetailsOnlyWhereGiven(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, http.StatusBadRequest, CodeInvalidRequest, "a <b> & c.")
	if rec.Code != http.StatusBadRequest || rec.Header().Get("Content-Type") != "application/json" ||
		rec.Body.String() != `{"error":{"code":"invalid_request","message":"a <b> & c."}}` {
		t.Fatalf("Write: %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	rec = httptest.NewRecorder()
	WriteDetails(rec, http.StatusConflict, "in_use", "In use.", map[string]int{"count": 2})
	if rec.Body.String() != `{"error":{"code":"in_use","message":"In use.","details":{"count":2}}}` {
		t.Fatalf("WriteDetails: %q", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	WriteNotFound(rec)
	if rec.Code != http.StatusNotFound || rec.Body.String() != `{"error":{"code":"not_found","message":"Not found."}}` {
		t.Fatalf("WriteNotFound: %d %q", rec.Code, rec.Body.String())
	}
}
