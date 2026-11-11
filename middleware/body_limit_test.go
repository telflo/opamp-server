package middleware

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/telflo/telflo-opamp-server/tokens"
)

// readBack runs body through BodyLimit(max) and returns the answer and what
// the next handler read (nil when it wasn't reached).
func readBack(t *testing.T, max int64, body []byte, encoding string, chunked bool) (*httptest.ResponseRecorder, []byte, http.Header) {
	t.Helper()
	var got []byte
	var header http.Header
	h := BodyLimit(max, BodyReadTimeout, discard())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		read, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("the handler's read: %v", err)
		}
		got, header = read, r.Header
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, AgentRoute, bytes.NewReader(body))
	if chunked {
		req.ContentLength = -1
	}
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, got, header
}

func gzipped(t *testing.T, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A body of exactly 4 MiB is read; one byte more is 413, whether the length
// is declared or the body streams.
func TestABodyOver4MiBIs413(t *testing.T) {
	exact := bytes.Repeat([]byte("a"), MaxBodyBytes)
	if rec, got, _ := readBack(t, MaxBodyBytes, exact, "", false); rec.Code != http.StatusOK || len(got) != MaxBodyBytes {
		t.Fatalf("exactly 4 MiB: %d, %d bytes read", rec.Code, len(got))
	}
	over := append(exact, 'b')
	for _, chunked := range []bool{false, true} {
		rec, got, _ := readBack(t, MaxBodyBytes, over, "", chunked)
		if rec.Code != http.StatusRequestEntityTooLarge || rec.Body.String() != "Request Entity Too Large\n" || got != nil {
			t.Fatalf("4 MiB and a byte (chunked %v): %d %q, handler reached %v", chunked, rec.Code, rec.Body.String(), got != nil)
		}
	}
}

// A `Content-Encoding: gzip` body is decompressed through the same cap: the
// handler reads the plain bytes without the header; a small body that
// inflates past 4 MiB (a gzip bomb) is 413; gzip that doesn't decompress,
// or another encoding, is 400.
func TestAGzipBodyIsReadThroughTheSameCapAfterDecompression(t *testing.T) {
	plain := []byte("an AgentToServer, as bytes")
	rec, got, header := readBack(t, MaxBodyBytes, gzipped(t, plain), "gzip", false)
	if rec.Code != http.StatusOK || !bytes.Equal(got, plain) || header.Get("Content-Encoding") != "" {
		t.Fatalf("a gzip body: %d, read %q, Content-Encoding %q", rec.Code, got, header.Get("Content-Encoding"))
	}
	bomb := gzipped(t, make([]byte, MaxBodyBytes+1))
	if len(bomb) > 64<<10 {
		t.Fatalf("the bomb is %d bytes on the wire", len(bomb))
	}
	if rec, got, _ := readBack(t, MaxBodyBytes, bomb, "GZip", false); rec.Code != http.StatusRequestEntityTooLarge || got != nil {
		t.Fatalf("a gzip bomb: %d, handler reached %v", rec.Code, got != nil)
	}
	exact := gzipped(t, make([]byte, MaxBodyBytes))
	if rec, got, _ := readBack(t, MaxBodyBytes, exact, "gzip", false); rec.Code != http.StatusOK || len(got) != MaxBodyBytes {
		t.Fatalf("4 MiB inflated: %d, %d bytes", rec.Code, len(got))
	}
	for label, c := range map[string]struct {
		body     []byte
		encoding string
	}{
		"gzip that isn't":  {[]byte("not gzip at all"), "gzip"},
		"a truncated gzip": {gzipped(t, plain)[:12], "gzip"},
		"another encoding": {plain, "br"},
		"two encodings":    {gzipped(t, plain), "gzip, gzip"},
	} {
		if rec, got, _ := readBack(t, MaxBodyBytes, c.body, c.encoding, false); rec.Code != http.StatusBadRequest || got != nil {
			t.Errorf("%s: %d, handler reached %v", label, rec.Code, got != nil)
		}
	}
}

// The stack's order (see AgentStack): a declared length over the cap is 413
// before anything else (no lookup); otherwise the credential is checked
// before the body is read, so a 5 MiB chunked body without a credential, or
// with a forged one, is 401 with none of it read, while a valid one's is
// read through the cap and refused 413. The security headers are on every
// answer; there is no CORS header.
func TestTheStackChecksTheDeclaredLengthThenTheCredentialThenReadsTheBody(t *testing.T) {
	k, err := tokens.NewKeyring(currentKeySetting, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Destroy()
	token, organization, fleet := uuid.New(), uuid.New(), uuid.New()
	clock := newTestClock()
	lookups := &countingLookup{TokenLookup: staticLookup{row: activeRow(token, organization, fleet)}}
	stack := testStack(k, lookups, DefaultMaxConcurrentRequests, clock)
	h := stack.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "x" {
			t.Errorf("the handler read %d bytes, %v", len(body), err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	valid := "Bearer " + mint(t, k, token, fleet)
	for _, c := range []struct {
		label         string
		body          string
		declared      bool
		authorization string
		status        int
		read          bool
		lookups       int
	}{
		{"a declared 5 MiB body without credentials", strings.Repeat("x", 5<<20), true, "", http.StatusRequestEntityTooLarge, false, 0},
		{"a declared 5 MiB body with a valid bearer", strings.Repeat("x", 5<<20), true, valid, http.StatusRequestEntityTooLarge, false, 0},
		{"a chunked 5 MiB body without credentials", strings.Repeat("x", 5<<20), false, "", http.StatusUnauthorized, false, 0},
		{"a chunked 5 MiB body with a forged bearer", strings.Repeat("x", 5<<20), false, "Bearer forged", http.StatusUnauthorized, false, 0},
		{"a chunked 5 MiB body with a valid bearer", strings.Repeat("x", 5<<20), false, valid, http.StatusRequestEntityTooLarge, true, 1},
		{"a small body without credentials", "x", true, "", http.StatusUnauthorized, false, 1},
		{"a small body with a valid bearer", "x", false, valid, http.StatusOK, true, 1},
	} {
		var read atomic.Int64
		req := httptest.NewRequest(http.MethodPost, AgentRoute, strings.NewReader(c.body))
		req.Body = countedBody{ReadCloser: req.Body, read: &read}
		if !c.declared {
			req.ContentLength = -1
		}
		if c.authorization != "" {
			req.Header.Set("Authorization", c.authorization)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.status {
			t.Errorf("%s: %d, want %d", c.label, rec.Code, c.status)
		}
		if got := read.Load() > 0; got != c.read {
			t.Errorf("%s: %d bytes of the body read", c.label, read.Load())
		}
		if n := lookups.count(); n != c.lookups {
			t.Errorf("%s: %d token lookups so far, want %d", c.label, n, c.lookups)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: no security headers: %v", c.label, rec.Header())
		}
		for name := range rec.Header() {
			if strings.HasPrefix(strings.ToLower(name), "access-control-") {
				t.Errorf("%s: a CORS header %s", c.label, name)
			}
		}
	}
}
