package management

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/publications"
)

// trickyYAML must come back byte for byte: HTML characters (which Go's JSON
// escapes by default), multi-byte text, a carriage return, a tab, trailing
// spaces, U+2028, a backslash.
const trickyYAML = "receivers:\r\n  otlp: # <tls> & \"quotes\" 'single' \\path\n\tname: café ☃ 日本 🚀  \n  note: \u2028end\n"

const conflictBody = `{"error":{"code":"publication_conflict","message":"A different publication already has this id."}}`

// The reply to a valid POST of validBody(sampleYAML) as orgA's publicationA,
// received at the test clock's start.
func expectedReply(yaml string) reply {
	return reply{
		"id":             publicationA,
		"organizationId": orgA,
		"configuration":  map[string]any{"id": configurationA, "name": "payments-gateway"},
		"version":        map[string]any{"id": versionA, "number": float64(3), "label": "TLS on"},
		"yaml":           yaml,
		"sha256":         sha256Hex(yaml),
		"publishedBy":    map[string]any{"id": actorA, "email": "ana@acme.io"},
		"publishedAt":    "2026-10-03T14:02:11.000Z",
		"receivedAt":     "2026-10-03T15:00:00.123Z",
		"collector":      map[string]any{"distribution": sampleDistribution, "version": sampleCollectorVersion},
	}
}

func assertReply(t *testing.T, label string, got, want reply) {
	t.Helper()
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("%s:\n got  %s\n want %s", label, gotJSON, wantJSON)
	}
}

func TestPostPublicationIsCreatedThenIdenticalThenConflicting(t *testing.T) {
	store := publications.NewPostgres(testDB(t).Runtime)
	s, clock, _ := publicationsServer(t, store)
	valid := validBody(sampleYAML)

	created := publish(s, orgA, publicationA, valid)
	expectStatus(t, "the first POST", created, http.StatusCreated)
	assertReply(t, "the first POST", decodeReply(t, created), expectedReply(sampleYAML))

	// The same body later: 200, the stored publication, received_at the
	// first POST's, and the row unchanged.
	stored, err := store.Get(context.Background(), uuid.MustParse(orgA), uuid.MustParse(publicationA))
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour)
	again := publish(s, orgA, publicationA, valid)
	expectStatus(t, "the identical POST", again, http.StatusOK)
	if again.Body.String() != created.Body.String() {
		t.Fatalf("the identical POST answered %s, want the stored %s", again.Body.String(), created.Body.String())
	}
	after, err := store.Get(context.Background(), uuid.MustParse(orgA), uuid.MustParse(publicationA))
	if err != nil || !after.SameAs(stored) || !after.ReceivedAt.Equal(stored.ReceivedAt) {
		t.Fatalf("the identical POST changed the row: %+v, %v", after, err)
	}

	// The same instant, written differently or below a millisecond, is
	// identical.
	for _, publishedAt := range []string{"2026-10-03T16:02:11+02:00", "2026-10-03T14:02:11.000999999Z", "2026-10-03T14:02:11.0005Z", "2026-10-03T14:02:11Z"} {
		expectStatus(t, "publishedAt "+publishedAt, publish(s, orgA, publicationA, valid.with("publishedAt", publishedAt)), http.StatusOK)
	}

	// Anything different under the same id: 409, naming nothing.
	for label, different := range map[string]body{
		"another yaml":            valid.withYAML(sampleYAML + "# changed\n"),
		"another name":            valid.with("configuration.name", "renamed"),
		"another configuration":   valid.with("configuration.id", configurationB),
		"another version id":      valid.with("version.id", uuid.NewString()),
		"another version number":  valid.with("version.number", 4),
		"another label":           valid.with("version.label", "TLS off"),
		"no label":                valid.with("version.label", nil),
		"another actor's email":   valid.with("publishedBy.email", "bo@acme.io"),
		"another actor's id":      valid.with("publishedBy.id", uuid.NewString()),
		"no actor":                valid.with("publishedBy", nil),
		"publishedAt a ms later":  valid.with("publishedAt", "2026-10-03T14:02:11.001Z"),
		"publishedAt a day later": valid.with("publishedAt", "2026-10-04T14:02:11.000Z"),
		// The collector is part of the publication.
		"another distribution":      valid.with("collector.distribution", "otelcol-k8s"),
		"another collector version": valid.with("collector.version", "0.155.4"),
		"the default collector":     valid.with("collector", map[string]any{"distribution": "otelcol-contrib", "version": "0.162.0"}),
	} {
		got := publish(s, orgA, publicationA, different)
		expectError(t, label, got, http.StatusConflict, codePublicationConflict, "A different publication already has this id.")
	}
	after, err = store.Get(context.Background(), uuid.MustParse(orgA), uuid.MustParse(publicationA))
	if err != nil || !after.SameAs(stored) || !after.ReceivedAt.Equal(stored.ReceivedAt) {
		t.Fatalf("a conflicting POST changed the row: %+v, %v", after, err)
	}
}

// A POST without an id stores the body under an id the server makes,
// answered 201 with it and readable under it; every such POST, even of the
// same body, is a new publication. It refuses what a POST with an id
// refuses.
func TestPostPublicationWithoutAnIdMakesOneEachTime(t *testing.T) {
	store := publications.NewPostgres(testDB(t).Runtime)
	s, _, _ := publicationsServer(t, store)
	valid := validBody(sampleYAML)

	first := post(s, orgA, valid)
	expectStatus(t, "the first POST", first, http.StatusCreated)
	got := decodeReply(t, first)
	id, ok := got["id"].(string)
	if !ok || uuid.Validate(id) != nil || id == publicationA {
		t.Fatalf("the POST answered the id %v, want a new UUID", got["id"])
	}
	want := expectedReply(sampleYAML)
	want["id"] = id
	assertReply(t, "the first POST", got, want)
	read := call(s, http.MethodGet, publicationTarget(orgA, id), nil, "")
	expectStatus(t, "the GET of the POST's id", read, http.StatusOK)
	if read.Body.String() != first.Body.String() {
		t.Fatalf("the GET answered %s, want the POST's %s", read.Body.String(), first.Body.String())
	}

	second := post(s, orgA, valid)
	expectStatus(t, "the same POST again", second, http.StatusCreated)
	if other := decodeReply(t, second)["id"]; other == id {
		t.Fatalf("the second POST reused the id %v", other)
	}
	page, err := store.List(context.Background(), uuid.MustParse(orgA), uuid.MustParse(configurationA), 10, nil)
	if err != nil || len(page.Publications) != 2 {
		t.Fatalf("after two POSTs the configuration has %d publications (%v), want 2", len(page.Publications), err)
	}

	// What a POST with an id refuses, one without refuses, storing nothing.
	expectError(t, "a sha256 mismatch", post(s, orgA, valid.with("sha256", sha256Hex("something else"))),
		http.StatusUnprocessableEntity, codeSHA256Mismatch, "sha256 isn't the SHA-256 of yaml's UTF-8 bytes.")
	expectStatus(t, "a name over 200 characters", post(s, orgA, valid.with("configuration.name", strings.Repeat("n", 201))), http.StatusBadRequest)
	expectStatus(t, "an unregistered organization", post(s, orgC, valid), http.StatusNotFound)
	if page, err = store.List(context.Background(), uuid.MustParse(orgA), uuid.MustParse(configurationA), 10, nil); err != nil || len(page.Publications) != 2 {
		t.Fatalf("a refused POST stored a publication: %d (%v), want 2", len(page.Publications), err)
	}
}

// publishedAt is kept, compared and answered to the millisecond: a client
// that sends finer digits, or POSTs back the publishedAt it was answered,
// gets the identical publication, not a 409; a millisecond's difference is
// a conflict.
func TestPublishedAtIsKeptComparedAndAnsweredToTheMillisecond(t *testing.T) {
	for name, server := range map[string]func(t *testing.T) *Server{
		"postgres": func(t *testing.T) *Server {
			s, _, _ := postgresServer(t)
			return s
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := server(t)
			valid := validBody(sampleYAML)

			// Milliseconds first, then the same id with microseconds below
			// them: the same publication.
			first := uuid.NewString()
			created := publish(s, orgA, first, valid.with("publishedAt", "2026-10-03T14:02:11.123Z"))
			expectStatus(t, "the POST at .123", created, http.StatusCreated)
			finer := publish(s, orgA, first, valid.with("publishedAt", "2026-10-03T14:02:11.123456Z"))
			expectStatus(t, "the same id at .123456", finer, http.StatusOK)
			if finer.Body.String() != created.Body.String() {
				t.Fatalf("the POST at .123456 answered %s, want the stored %s", finer.Body.String(), created.Body.String())
			}

			// Microseconds first: the reply holds milliseconds, and PUTting
			// the reply's publishedAt back is the same publication.
			second := uuid.NewString()
			created = publish(s, orgA, second, valid.with("publishedAt", "2026-10-03T14:02:11.123456Z"))
			expectStatus(t, "the POST at .123456", created, http.StatusCreated)
			answered := decodeReply(t, created)["publishedAt"]
			if answered != "2026-10-03T14:02:11.123Z" {
				t.Fatalf("the reply's publishedAt is %v, want 2026-10-03T14:02:11.123Z", answered)
			}
			again := publish(s, orgA, second, valid.with("publishedAt", answered))
			expectStatus(t, "the POST of the reply's publishedAt", again, http.StatusOK)
			if again.Body.String() != created.Body.String() {
				t.Fatalf("the re-POST answered %s, want the stored %s", again.Body.String(), created.Body.String())
			}
			got := call(s, http.MethodGet, publicationTarget(orgA, second), nil, "")
			expectStatus(t, "the GET", got, http.StatusOK)
			if stored := decodeReply(t, got)["publishedAt"]; stored != answered {
				t.Fatalf("the stored publishedAt is %v, want %v", stored, answered)
			}

			// A millisecond either way is another publication.
			for _, publishedAt := range []string{"2026-10-03T14:02:11.124Z", "2026-10-03T14:02:11.122999Z"} {
				expectError(t, "publishedAt "+publishedAt, publish(s, orgA, second, valid.with("publishedAt", publishedAt)),
					http.StatusConflict, codePublicationConflict, "A different publication already has this id.")
			}
		})
	}
}

// The collector is stored, answered by the POST, the GET and the list
// exactly as it was sent, and compared for the same id: the same collector
// again is the identical publication (200), and another distribution or
// another version under the id is a 409 that changes nothing. The body's
// collector isn't migration 00003's defaults, so a store leaving its
// columns to them answers another one.
func TestTheCollectorIsStoredAnsweredAndComparedForTheSameIdOverBothStores(t *testing.T) {
	for name, server := range map[string]func(t *testing.T) *Server{
		"postgres": func(t *testing.T) *Server {
			s, _, _ := postgresServer(t)
			return s
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := server(t)
			valid := validBody(sampleYAML)
			want := map[string]any{"distribution": sampleDistribution, "version": sampleCollectorVersion}
			collectorOf := func(label string, r reply) {
				t.Helper()
				if got, _ := json.Marshal(r["collector"]); string(got) != `{"distribution":"otelcol-sample","version":"0.155.3"}` {
					t.Fatalf("%s: collector %s, want %v", label, got, want)
				}
			}

			created := publish(s, orgA, publicationA, valid)
			expectStatus(t, "the POST", created, http.StatusCreated)
			collectorOf("the POST", decodeReply(t, created))

			again := publish(s, orgA, publicationA, valid)
			expectStatus(t, "the same id and collector", again, http.StatusOK)
			if again.Body.String() != created.Body.String() {
				t.Fatalf("the same POST answered %s, want the stored %s", again.Body.String(), created.Body.String())
			}

			for label, different := range map[string]body{
				"another distribution":        valid.with("collector.distribution", "otelcol-contrib"),
				"another version":             valid.with("collector.version", "0.162.0"),
				"migration 00003's defaults":  valid.with("collector", map[string]any{"distribution": "otelcol-contrib", "version": "0.162.0"}),
				"the version a patch earlier": valid.with("collector.version", "0.155.2"),
			} {
				expectError(t, label, publish(s, orgA, publicationA, different),
					http.StatusConflict, codePublicationConflict, "A different publication already has this id.")
			}

			got := call(s, http.MethodGet, publicationTarget(orgA, publicationA), nil, "")
			expectStatus(t, "the GET", got, http.StatusOK)
			if got.Body.String() != created.Body.String() {
				t.Fatalf("the GET answered %s, want the stored %s", got.Body.String(), created.Body.String())
			}
			listed := call(s, http.MethodGet, listTarget(orgA, "?configurationId="+configurationA), nil, "")
			expectStatus(t, "the list", listed, http.StatusOK)
			var page struct{ Publications []reply }
			if err := json.Unmarshal(listed.Body.Bytes(), &page); err != nil || len(page.Publications) != 1 {
				t.Fatalf("the list: %v, %s", err, listed.Body.String())
			}
			collectorOf("the list's summary", page.Publications[0])
		})
	}
}

// A publishedAt outside the years 0001 to 9999 in UTC is a 400 naming the
// field and stores nothing; one at either end is stored and answered as it
// was given. A whole-number version.number written as 3.0 is stored as 3,
// and a lowercase t and z are read.
func TestPublishedAtIsKeptInTheYears0001To9999OverBothStores(t *testing.T) {
	for name, server := range map[string]func(t *testing.T) *Server{
		"postgres": func(t *testing.T) *Server {
			s, _, _ := postgresServer(t)
			return s
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := server(t)
			valid := validBody(sampleYAML)
			for _, publishedAt := range []string{
				"9999-12-31T23:59:59-01:00", "0000-01-01T00:00:00+01:00", "0000-12-31T23:59:59.999Z", "10000-01-01T00:00:00Z",
			} {
				id := uuid.NewString()
				got := publish(s, orgA, id, valid.with("publishedAt", publishedAt))
				message := "publishedAt must be from the year 0001 to 9999, in UTC."
				if publishedAt == "10000-01-01T00:00:00Z" {
					message = "publishedAt must be an RFC 3339 date-time."
				}
				expectError(t, "publishedAt "+publishedAt, got, http.StatusBadRequest, codeInvalidRequest, message)
				expectStatus(t, "the GET after "+publishedAt, call(s, http.MethodGet, publicationTarget(orgA, id), nil, ""), http.StatusNotFound)
			}
			expectError(t, "publishedAt +24:00", publish(s, orgA, uuid.NewString(), valid.with("publishedAt", "2026-10-03T14:02:11+24:00")),
				http.StatusBadRequest, codeInvalidRequest, "publishedAt must be an RFC 3339 date-time.")

			for given, answered := range map[string]string{
				"0001-01-01T00:00:00Z":      "0001-01-01T00:00:00.000Z",
				"9999-12-31T23:59:59.999Z":  "9999-12-31T23:59:59.999Z",
				"0001-01-01t01:00:00+01:00": "0001-01-01T00:00:00.000Z",
				"2026-10-03t14:02:11.5z":    "2026-10-03T14:02:11.500Z",
			} {
				id := uuid.NewString()
				created := publish(s, orgA, id, valid.with("publishedAt", given))
				expectStatus(t, "publishedAt "+given, created, http.StatusCreated)
				if got := decodeReply(t, created)["publishedAt"]; got != answered {
					t.Fatalf("publishedAt %s was answered as %v, want %s", given, got, answered)
				}
				got := call(s, http.MethodGet, publicationTarget(orgA, id), nil, "")
				if stored := decodeReply(t, got)["publishedAt"]; stored != answered {
					t.Fatalf("publishedAt %s was stored as %v, want %s", given, stored, answered)
				}
				// The reply's value POST back is the same publication.
				expectStatus(t, "the re-POST of "+answered, publish(s, orgA, id, valid.with("publishedAt", answered)), http.StatusOK)
			}

			id := uuid.NewString()
			created := publish(s, orgA, id, valid.with("version.number", json.Number("3.0")))
			expectStatus(t, "version.number 3.0", created, http.StatusCreated)
			if number := decodeReply(t, created)["version"].(map[string]any)["number"]; number != float64(3) {
				t.Fatalf("version.number 3.0 was stored as %v, want 3", number)
			}
			expectStatus(t, "the same publication with 3", publish(s, orgA, id, valid.with("version.number", 3)), http.StatusOK)
		})
	}
}

// A cursor whose time no stored publication can have is a 400, never a 500
// from the database: a time in the year -7537, and one finer than a
// millisecond.
func TestACursorOutOfRangeIsA400OverBothStores(t *testing.T) {
	for name, server := range map[string]func(t *testing.T) *Server{
		"postgres": func(t *testing.T) *Server {
			s, _, _ := postgresServer(t)
			return s
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := server(t)
			expectStatus(t, "POST", publish(s, orgA, publicationA, validBody(sampleYAML)), http.StatusCreated)
			finer := publications.Cursor{
				PublishedAt: time.Date(2026, 10, 3, 14, 2, 11, 123456000, time.UTC),
				ID:          uuid.MustParse(publicationA),
			}.String()
			for _, cursor := range []string{"-9YvludiAAAAAAAAAAAAAAAAAAAAAAAA", finer} {
				got := call(s, http.MethodGet, listTarget(orgA, "?configurationId="+configurationA+"&cursor="+cursor), nil, "")
				expectError(t, "cursor "+cursor, got, http.StatusBadRequest, codeInvalidRequest, "cursor isn't one this API gave.")
			}
			// The last possible time is a cursor like any other.
			last := publications.Cursor{
				PublishedAt: time.Date(9999, 12, 31, 23, 59, 59, 999000000, time.UTC),
				ID:          uuid.MustParse(publicationA),
			}.String()
			page := list(t, s, orgA, "?configurationId="+configurationA+"&cursor="+last)
			if len(page.Publications) != 1 {
				t.Fatalf("after the last cursor, %d publications, want 1", len(page.Publications))
			}
		})
	}
}

// Another organization putting A's id gets the same 409 as any conflict,
// identical body or not: nothing in it names A or the row, and A's row is
// unchanged.
func TestPostOfAnotherOrganizationsPublicationIdIsAConflictThatNamesNothing(t *testing.T) {
	store := publications.NewPostgres(testDB(t).Runtime)
	s, _, _ := publicationsServer(t, store)
	valid := validBody(sampleYAML)
	expectStatus(t, "A's POST", publish(s, orgA, publicationA, valid), http.StatusCreated)

	for label, b := range map[string]body{
		"the identical body": valid,
		"another body":       valid.withYAML("exporters: {}\n"),
	} {
		got := publish(s, orgB, publicationA, b)
		if got.Code != http.StatusConflict || got.Body.String() != conflictBody {
			t.Fatalf("%s as B: %d %s, want 409 %s", label, got.Code, got.Body.String(), conflictBody)
		}
	}
	expectStatus(t, "B's GET", call(s, http.MethodGet, publicationTarget(orgB, publicationA), nil, ""), http.StatusNotFound)
	got := call(s, http.MethodGet, publicationTarget(orgA, publicationA), nil, "")
	expectStatus(t, "A's GET", got, http.StatusOK)
	assertReply(t, "A's publication", decodeReply(t, got), expectedReply(sampleYAML))
}

// One validation failure: what to send and the exact 400 (or 422) answer.
type validationCase struct {
	name        string
	payload     []byte
	contentType string
	target      string
	status      int
	message     string
	// value must not appear in the answer (the message names the field,
	// never its value).
	value string
}

func TestPostPublicationValidatesEveryFieldAndItsBounds(t *testing.T) {
	valid := validBody(sampleYAML)
	accent := "é" // two bytes, one character
	ok := func(name string, b body) validationCase {
		return validationCase{name: name, payload: b.bytes(), status: http.StatusCreated}
	}
	bad := func(name string, b body, message string) validationCase {
		return validationCase{name: name, payload: b.bytes(), status: http.StatusBadRequest, message: message}
	}
	marked := func(name string, b body, message, value string) validationCase {
		c := bad(name, b, message)
		c.value = value
		return c
	}
	maxNumber := strconv.Itoa(maxVersionNumber)
	const (
		distributionFormat = "collector.distribution must be lowercase letters and digits in words joined by single hyphens, at most 64 characters."
		versionFormat      = "collector.version must be major.minor.patch, at most 64 characters."
	)
	yamlAtLimit := "a: " + strings.Repeat("x", maxYAMLBytes-3)
	// Fewer characters than the limit, more bytes: the limit is in bytes.
	threeByteYAML := strings.Repeat("€", maxYAMLBytes/3+1)

	cases := []validationCase{
		ok("a valid body", valid),

		bad("configuration missing", valid.without("configuration"), "configuration is required."),
		bad("configuration null", valid.with("configuration", nil), "configuration must be an object."),
		bad("configuration.id missing", valid.without("configuration.id"), "configuration.id is required."),
		marked("configuration.id not a UUID", valid.with("configuration.id", "id-marker-71f"), "configuration.id must be a UUID.", "id-marker-71f"),
		bad("configuration.id with braces", valid.with("configuration.id", "{"+configurationA+"}"), "configuration.id must be a UUID."),
		bad("configuration.id without hyphens", valid.with("configuration.id", strings.ReplaceAll(configurationA, "-", "")), "configuration.id must be a UUID."),
		bad("configuration.name empty", valid.with("configuration.name", ""), "configuration.name must be 1 to 200 characters."),
		ok("configuration.name 1 character", valid.with("configuration.name", "n")),
		ok("configuration.name 200 characters", valid.with("configuration.name", strings.Repeat(accent, 200))),
		marked("configuration.name 201 characters", valid.with("configuration.name", strings.Repeat("name-marker", 18)+"abc"),
			"configuration.name must be 1 to 200 characters.", "name-marker"),
		bad("configuration.name a number", valid.with("configuration.name", 7), "configuration.name must be a string."),
		bad("configuration.name with U+0000", valid.with("configuration.name", "a\x00b"), "configuration.name must not contain U+0000."),
		bad("configuration with an unknown field", valid.with("configuration.nodes", []any{}), "configuration has a field this API doesn't know."),

		bad("version.id not a UUID", valid.with("version.id", "3"), "version.id must be a UUID."),
		bad("version.number 0", valid.with("version.number", 0), "version.number must be an integer from 1 to "+maxNumber+"."),
		ok("version.number 1", valid.with("version.number", 1)),
		ok("version.number the largest integer", valid.with("version.number", maxVersionNumber)),
		bad("version.number one over the largest", valid.with("version.number", maxVersionNumber+1), "version.number must be an integer from 1 to "+maxNumber+"."),
		bad("version.number a fraction", valid.with("version.number", 2.5), "version.number must be an integer from 1 to "+maxNumber+"."),
		// A whole number written as a fraction or with an exponent is an
		// integer, as JSON Schema has it.
		ok("version.number 3.0", valid.with("version.number", json.Number("3.0"))),
		ok("version.number 3e0", valid.with("version.number", json.Number("3e0"))),
		ok("version.number the largest, as 2.147483647e9", valid.with("version.number", json.Number("2.147483647e9"))),
		bad("version.number 3.5", valid.with("version.number", json.Number("3.5")), "version.number must be an integer from 1 to "+maxNumber+"."),
		bad("version.number 0.0", valid.with("version.number", json.Number("0.0")), "version.number must be an integer from 1 to "+maxNumber+"."),
		bad("version.number 1e400", valid.with("version.number", json.Number("1e400")), "version.number must be an integer from 1 to "+maxNumber+"."),
		bad("version.number one over the largest, as 2.147483648e9", valid.with("version.number", json.Number("2.147483648e9")),
			"version.number must be an integer from 1 to "+maxNumber+"."),
		bad("version.number a string", valid.with("version.number", "3"), "version.number must be an integer from 1 to "+maxNumber+"."),
		bad("version.number null", valid.with("version.number", nil), "version.number must be an integer from 1 to "+maxNumber+"."),
		bad("version.label missing", valid.without("version.label"), "version.label is required."),
		ok("version.label null", valid.with("version.label", nil)),
		bad("version.label empty", valid.with("version.label", ""), "version.label must be 1 to 80 characters."),
		ok("version.label 1 character", valid.with("version.label", "x")),
		ok("version.label 80 characters", valid.with("version.label", strings.Repeat(accent, 80))),
		bad("version.label 81 characters", valid.with("version.label", strings.Repeat(accent, 81)), "version.label must be 1 to 80 characters."),
		bad("version.label with U+0000", valid.with("version.label", "\x00"), "version.label must not contain U+0000."),

		bad("yaml empty", valid.withYAML(""), "yaml must be 1 to 1048576 bytes of UTF-8."),
		ok("yaml 1 byte", valid.withYAML("a")),
		ok("yaml 1 MiB", valid.withYAML(yamlAtLimit)),
		bad("yaml 1 MiB + 1 byte", valid.withYAML(yamlAtLimit+"x"), "yaml must be 1 to 1048576 bytes of UTF-8."),
		bad("yaml under 1 Mi characters but over 1 MiB", valid.withYAML(threeByteYAML), "yaml must be 1 to 1048576 bytes of UTF-8."),
		bad("yaml with U+0000", valid.withYAML("a: \x00"), "yaml must not contain U+0000."),
		bad("yaml null", valid.with("yaml", nil), "yaml must be a string."),
		bad("yaml an object", valid.with("yaml", map[string]any{"receivers": nil}), "yaml must be a string."),

		bad("sha256 in capitals", valid.with("sha256", strings.ToUpper(sha256Hex(sampleYAML))), "sha256 must be 64 lowercase hexadecimal characters."),
		bad("sha256 63 characters", valid.with("sha256", sha256Hex(sampleYAML)[:63]), "sha256 must be 64 lowercase hexadecimal characters."),
		bad("sha256 65 characters", valid.with("sha256", sha256Hex(sampleYAML)+"0"), "sha256 must be 64 lowercase hexadecimal characters."),

		ok("publishedBy null", valid.with("publishedBy", nil)),
		bad("publishedBy missing", valid.without("publishedBy"), "publishedBy is required."),
		bad("publishedBy with only an id", valid.without("publishedBy.email"), "publishedBy.email is required."),
		bad("publishedBy with only an email", valid.without("publishedBy.id"), "publishedBy.id is required."),
		bad("publishedBy an array", valid.with("publishedBy", []any{}), "publishedBy must be an object."),
		bad("publishedBy with an unknown field", valid.with("publishedBy.name", "Ana"), "publishedBy has a field this API doesn't know."),
		bad("publishedBy.email empty", valid.with("publishedBy.email", ""), "publishedBy.email must be 1 to 320 characters."),
		ok("publishedBy.email 320 characters", valid.with("publishedBy.email", strings.Repeat("e", 310)+"@acme.test")),
		marked("publishedBy.email 321 characters", valid.with("publishedBy.email", strings.Repeat("e", 311)+"@acme.test"),
			"publishedBy.email must be 1 to 320 characters.", "eeeeeeeeee@acme.test"),
		bad("publishedBy.email null", valid.with("publishedBy.email", nil), "publishedBy.email must be a string."),

		marked("publishedAt not a time", valid.with("publishedAt", "time-marker-0c3"), "publishedAt must be an RFC 3339 date-time.", "time-marker-0c3"),
		bad("publishedAt without a zone", valid.with("publishedAt", "2026-10-03T14:02:11"), "publishedAt must be an RFC 3339 date-time."),
		bad("publishedAt a date", valid.with("publishedAt", "2026-10-03"), "publishedAt must be an RFC 3339 date-time."),
		bad("publishedAt a number", valid.with("publishedAt", 1759500131), "publishedAt must be an RFC 3339 date-time."),
		ok("publishedAt with an offset", valid.with("publishedAt", "2026-10-03T16:02:11.5+02:00")),
		// RFC 3339 allows a lowercase t and z.
		ok("publishedAt with a lowercase t and z", valid.with("publishedAt", "2026-10-03t14:02:11.000z")),
		ok("publishedAt with a lowercase t and an offset", valid.with("publishedAt", "2026-10-03t16:02:11+02:00")),
		ok("publishedAt at 23:59 of offset", valid.with("publishedAt", "2026-10-03T14:02:11+23:59")),
		ok("publishedAt in the year 0001", valid.with("publishedAt", "0001-01-01T00:00:00Z")),
		ok("publishedAt at the end of 9999", valid.with("publishedAt", "9999-12-31T23:59:59.999Z")),
		// Each a date-time in the text, but an instant out of 0001 to 9999
		// in UTC, or an offset RFC 3339 has no room for.
		bad("publishedAt in 10000 once in UTC", valid.with("publishedAt", "9999-12-31T23:59:59-01:00"),
			"publishedAt must be from the year 0001 to 9999, in UTC."),
		bad("publishedAt in 0000 once in UTC", valid.with("publishedAt", "0000-01-01T00:00:00+01:00"),
			"publishedAt must be from the year 0001 to 9999, in UTC."),
		bad("publishedAt in the year 0000", valid.with("publishedAt", "0000-12-31T23:59:59Z"),
			"publishedAt must be from the year 0001 to 9999, in UTC."),
		bad("publishedAt with an offset of +24:00", valid.with("publishedAt", "2026-10-03T14:02:11+24:00"), "publishedAt must be an RFC 3339 date-time."),
		bad("publishedAt with an offset of -24:00", valid.with("publishedAt", "2026-10-03T14:02:11-24:00"), "publishedAt must be an RFC 3339 date-time."),
		bad("publishedAt with an offset of 60 minutes", valid.with("publishedAt", "2026-10-03T14:02:11+01:60"), "publishedAt must be an RFC 3339 date-time."),
		// time.Parse takes a comma before the fraction, RFC 3339 doesn't;
		// RFC 3339 has a leap second, time.Parse doesn't, and the API
		// documents its refusal.
		bad("publishedAt with a comma before the fraction", valid.with("publishedAt", "2026-10-03T14:02:11,5Z"), "publishedAt must be an RFC 3339 date-time."),
		bad("publishedAt with a leap second", valid.with("publishedAt", "2016-12-31T23:59:60Z"), "publishedAt must be an RFC 3339 date-time."),

		// The collector, an object with exactly a distribution and a
		// version, each in its format; never null.
		ok("collector Contrib 0.162.0", valid.with("collector", map[string]any{"distribution": "otelcol-contrib", "version": "0.162.0"})),
		ok("collector.distribution one word", valid.with("collector.distribution", "otelcol")),
		ok("collector.distribution 64 characters", valid.with("collector.distribution", strings.Repeat("a", 31)+"-"+strings.Repeat("b", 32))),
		ok("collector.version 64 characters", valid.with("collector.version", strings.Repeat("1", 60)+".2.3")),
		bad("collector null", valid.with("collector", nil), "collector must be an object."),
		bad("collector missing", valid.without("collector"), "collector is required."),
		bad("collector a string", valid.with("collector", "otelcol-contrib"), "collector must be an object."),
		bad("collector an empty object", valid.with("collector", map[string]any{}), "collector.distribution is required."),
		bad("collector without distribution", valid.without("collector.distribution"), "collector.distribution is required."),
		bad("collector without version", valid.without("collector.version"), "collector.version is required."),
		bad("collector with an unknown field", valid.with("collector.image", "otel/opentelemetry-collector-contrib:0.162.0"),
			"collector has a field this API doesn't know."),
		bad("collector.distribution null", valid.with("collector.distribution", nil), "collector.distribution must be a string."),
		bad("collector.distribution a number", valid.with("collector.distribution", 7), "collector.distribution must be a string."),
		bad("collector.distribution with U+0000", valid.with("collector.distribution", "otelcol\x00"), "collector.distribution must not contain U+0000."),
		bad("collector.distribution empty", valid.with("collector.distribution", ""), distributionFormat),
		marked("collector.distribution in capitals", valid.with("collector.distribution", "OTELCOL-MARKER"), distributionFormat, "OTELCOL-MARKER"),
		bad("collector.distribution with a leading hyphen", valid.with("collector.distribution", "-otelcol"), distributionFormat),
		bad("collector.distribution with a trailing hyphen", valid.with("collector.distribution", "otelcol-"), distributionFormat),
		bad("collector.distribution with two hyphens in a row", valid.with("collector.distribution", "otelcol--contrib"), distributionFormat),
		bad("collector.distribution with an underscore", valid.with("collector.distribution", "otelcol_contrib"), distributionFormat),
		bad("collector.distribution with a space", valid.with("collector.distribution", "otelcol contrib"), distributionFormat),
		bad("collector.distribution with a trailing newline", valid.with("collector.distribution", "otelcol\n"), distributionFormat),
		bad("collector.distribution with an Arabic-Indic digit", valid.with("collector.distribution", "otelcol-\u0661"), distributionFormat),
		bad("collector.distribution 65 characters", valid.with("collector.distribution", strings.Repeat("a", 32)+"-"+strings.Repeat("b", 32)), distributionFormat),
		bad("collector.version null", valid.with("collector.version", nil), "collector.version must be a string."),
		bad("collector.version a number", valid.with("collector.version", 0.162), "collector.version must be a string."),
		bad("collector.version empty", valid.with("collector.version", ""), versionFormat),
		marked("collector.version two parts", valid.with("collector.version", "0.162"), versionFormat, "0.162"),
		bad("collector.version four parts", valid.with("collector.version", "0.162.0.1"), versionFormat),
		bad("collector.version with a v", valid.with("collector.version", "v0.162.0"), versionFormat),
		bad("collector.version with a pre-release", valid.with("collector.version", "0.162.0-rc.1"), versionFormat),
		bad("collector.version with a letter", valid.with("collector.version", "0.16a.0"), versionFormat),
		bad("collector.version with an empty part", valid.with("collector.version", "0..0"), versionFormat),
		bad("collector.version with a trailing newline", valid.with("collector.version", "0.162.0\n"), versionFormat),
		bad("collector.version with an Arabic-Indic digit", valid.with("collector.version", "0.\u0661.0"), versionFormat),
		bad("collector.version 65 characters", valid.with("collector.version", strings.Repeat("1", 61)+".2.3"), versionFormat),

		bad("an unknown field", valid.with("nodes", []any{}), "The request body has a field this API doesn't know."),
		{name: "not JSON", payload: []byte("receivers: {}"), status: 400, message: "The request body must be a JSON object."},
		{name: "JSON null", payload: []byte("null"), status: 400, message: "The request body must be a JSON object."},
		{name: "a JSON array", payload: []byte("[]"), status: 400, message: "The request body must be a JSON object."},
		{name: "no body", payload: []byte{}, status: 400, message: "The request body must be a JSON object."},
		{name: "two JSON objects", payload: append(valid.bytes(), valid.bytes()...), status: 400, message: "The request body must be one JSON object."},
		{name: "whitespace after the object", payload: append(valid.bytes(), " \r\n\t"...), status: 201},
		{name: "invalid UTF-8", payload: []byte(`{"yaml":"` + "\xff" + `"}`), status: 400, message: "The request body must be UTF-8."},

		{name: "Content-Type text/plain", payload: valid.bytes(), contentType: "text/plain", status: 400, message: "Content-Type must be application/json."},
		{name: "no Content-Type", payload: valid.bytes(), contentType: "-", status: 400, message: "Content-Type must be application/json."},
		{name: "Content-Type with another charset", payload: valid.bytes(), contentType: "application/json; charset=latin1", status: 400, message: "Content-Type must be application/json."},
		{name: "Content-Type with charset=utf-8", payload: valid.bytes(), contentType: "application/json; charset=UTF-8", status: 201},
		{name: "Content-Type application/json-patch+json", payload: valid.bytes(), contentType: "application/json-patch+json", status: 400, message: "Content-Type must be application/json."},

		{name: "a non-UUID organization id", payload: valid.bytes(), target: listTarget("org-marker-5aa", ""), status: 400,
			message: "organizationId must be a UUID.", value: "org-marker-5aa"},
		{name: "a non-UUID publication id", payload: valid.with("id", "pub-marker-6bb").bytes(), status: 400,
			message: "id must be a UUID.", value: "pub-marker-6bb"},
		{name: "a publication id in braces", payload: valid.with("id", "{"+publicationA+"}").bytes(), status: 400,
			message: "id must be a UUID."},
		{name: "a null publication id", payload: valid.with("id", nil).bytes(), status: 400, message: "id must be a UUID."},

		{name: "a sha256 of something else", payload: valid.with("sha256", sha256Hex("something else")).bytes(), status: 422,
			message: "sha256 isn't the SHA-256 of yaml's UTF-8 bytes."},
	}

	// Anything after the object, JSON or not, is refused.
	for _, trailing := range []string{"xyz", "]", "}", `"`, "\x00", "{", "1", "null", " x", "\n[]"} {
		cases = append(cases, validationCase{
			name:    "the object followed by " + strconv.Quote(trailing),
			payload: append(valid.bytes(), trailing...),
			status:  http.StatusBadRequest,
			message: "The request body must be one JSON object.",
		})
	}

	for _, c := range cases {
		emptyTestDB(t)
		s, _, _ := apiServer(t)
		target := c.target
		if target == "" {
			target = listTarget(orgA, "")
		}
		contentType := c.contentType
		switch contentType {
		case "":
			contentType = jsonType
		case "-":
			contentType = ""
		}
		got := call(s, http.MethodPost, target, c.payload, contentType)
		if c.status == http.StatusCreated {
			expectStatus(t, c.name, got, http.StatusCreated)
			continue
		}
		code := codeInvalidRequest
		if c.status == http.StatusUnprocessableEntity {
			code = codeSHA256Mismatch
		}
		expectError(t, c.name, got, c.status, code, c.message)
		if c.value != "" && strings.Contains(got.Body.String(), c.value) {
			t.Fatalf("%s: the answer quotes the value: %s", c.name, got.Body.String())
		}
	}
}

// sha256 is of yaml's UTF-8 bytes: a multi-byte YAML's own digest is
// accepted, and the digest of the same text in another encoding is a 422.
func TestTheSHA256IsOfTheYAMLsUTF8Bytes(t *testing.T) {
	s, _, _ := apiServer(t)
	yaml := "name: café ☃ 日本 🚀\n"
	expectStatus(t, "the UTF-8 digest", publish(s, orgA, uuid.NewString(), validBody(yaml)), http.StatusCreated)

	var utf16Bytes []byte
	for _, unit := range utf16.Encode([]rune(yaml)) {
		utf16Bytes = append(utf16Bytes, byte(unit), byte(unit>>8))
	}
	for label, digest := range map[string]string{
		"the UTF-16 digest":         sha256Hex(string(utf16Bytes)),
		"the digest of other bytes": sha256Hex(strings.Map(func(r rune) rune { return r & 0x7f }, yaml)),
	} {
		got := publish(s, orgA, uuid.NewString(), validBody(yaml).with("sha256", digest))
		expectError(t, label, got, http.StatusUnprocessableEntity, codeSHA256Mismatch, "sha256 isn't the SHA-256 of yaml's UTF-8 bytes.")
	}
}

func TestGetReturnsTheYAMLByteForByteAndOnlyToItsOrganization(t *testing.T) {
	s, _, _ := apiServer(t)
	expectStatus(t, "POST", publish(s, orgA, publicationA, validBody(trickyYAML)), http.StatusCreated)

	got := call(s, http.MethodGet, publicationTarget(orgA, publicationA), nil, "")
	expectStatus(t, "GET", got, http.StatusOK)
	assertReply(t, "GET", decodeReply(t, got), expectedReply(trickyYAML))
	var decoded struct{ YAML string }
	if err := json.Unmarshal(got.Body.Bytes(), &decoded); err != nil || decoded.YAML != trickyYAML {
		t.Fatalf("the YAML came back as %q, want %q", decoded.YAML, trickyYAML)
	}
	if !strings.Contains(got.Body.String(), "<tls> &") {
		t.Fatalf("the reply escapes HTML characters: %s", got.Body.String())
	}

	notFound := `{"error":{"code":"not_found","message":"Not found."}}`
	for label, target := range map[string]string{
		"an unknown id":             publicationTarget(orgA, uuid.NewString()),
		"another organization's id": publicationTarget(orgB, publicationA),
	} {
		got := call(s, http.MethodGet, target, nil, "")
		if got.Code != http.StatusNotFound || got.Body.String() != notFound {
			t.Fatalf("%s: %d %s, want 404", label, got.Code, got.Body.String())
		}
	}
	expectError(t, "a non-UUID id", call(s, http.MethodGet, publicationTarget(orgA, "x"), nil, ""),
		http.StatusBadRequest, codeInvalidRequest, "publicationId must be a UUID.")
	expectError(t, "a non-UUID organization", call(s, http.MethodGet, publicationTarget("x", publicationA), nil, ""),
		http.StatusBadRequest, codeInvalidRequest, "organizationId must be a UUID.")
	// HEAD isn't documented: like any unmatched method, a 404.
	for _, target := range []string{publicationTarget(orgA, publicationA), listTarget(orgA, "?configurationId="+configurationA)} {
		got := call(s, http.MethodHead, target, nil, "")
		expectStatus(t, "HEAD "+target, got, http.StatusNotFound)
	}
}

// listed is one GET of the list, decoded.
type listed struct {
	Publications []map[string]any `json:"publications"`
	NextCursor   *string          `json:"nextCursor"`
}

func list(t *testing.T, s *Server, organizationID, query string) listed {
	t.Helper()
	got := call(s, http.MethodGet, listTarget(organizationID, query), nil, "")
	expectStatus(t, "list "+query, got, http.StatusOK)
	var page listed
	if err := json.Unmarshal(got.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Body.String(), `"publications":[`) || !strings.Contains(got.Body.String(), `"nextCursor":`) {
		t.Fatalf("the list lacks a field: %s", got.Body.String())
	}
	return page
}

func TestListIsNewestFirstPagedScopedAndWithoutYAML(t *testing.T) {
	s, _, _ := apiServer(t)
	// Five of A's publications of configurationA: two at one instant.
	type row struct{ id, publishedAt string }
	rows := []row{
		{uuid.NewString(), "2026-10-01T00:00:00.000Z"},
		{uuid.NewString(), "2026-10-02T00:00:00.000Z"},
		{uuid.NewString(), "2026-10-02T00:00:00.000Z"},
		{uuid.NewString(), "2026-10-03T00:00:00.000Z"},
		{uuid.NewString(), "2026-10-03T12:00:00.000Z"},
	}
	for _, r := range rows {
		expectStatus(t, "POST", publish(s, orgA, r.id, validBody(sampleYAML).with("publishedAt", r.publishedAt)), http.StatusCreated)
	}
	// What must never appear: B's of the same configuration id, and A's of
	// another configuration.
	expectStatus(t, "B's POST", publish(s, orgB, uuid.NewString(), validBody(sampleYAML)), http.StatusCreated)
	expectStatus(t, "A's other POST", publish(s, orgA, uuid.NewString(), validBody(sampleYAML).with("configuration.id", configurationB)), http.StatusCreated)

	slices.SortFunc(rows, func(a, b row) int {
		if c := strings.Compare(b.publishedAt, a.publishedAt); c != 0 {
			return c
		}
		return strings.Compare(b.id, a.id)
	})

	var seen []string
	query := "?configurationId=" + configurationA + "&limit=2"
	for page := 1; ; page++ {
		got := list(t, s, orgA, query)
		for _, p := range got.Publications {
			if _, hasYAML := p["yaml"]; hasYAML {
				t.Fatalf("a listed publication has yaml: %v", p)
			}
			if p["organizationId"] != orgA || p["configuration"].(map[string]any)["id"] != configurationA {
				t.Fatalf("the list holds another organization's or configuration's row: %v", p)
			}
			seen = append(seen, p["id"].(string))
		}
		if got.NextCursor == nil {
			if page != 3 {
				t.Fatalf("the list ended on page %d, want 3", page)
			}
			break
		}
		if page == 3 {
			t.Fatal("page 3 has a next cursor")
		}
		query = "?configurationId=" + configurationA + "&limit=2&cursor=" + *got.NextCursor
	}
	var want []string
	for _, r := range rows {
		want = append(want, r.id)
	}
	if !slices.Equal(seen, want) {
		t.Fatalf("listed %v, want %v (newest first, ties by id descending)", seen, want)
	}

	if got := list(t, s, orgB, "?configurationId="+configurationA); len(got.Publications) != 1 {
		t.Fatalf("B lists %d publications, want its own 1", len(got.Publications))
	}
	if got := list(t, s, orgA, "?configurationId="+uuid.NewString()); len(got.Publications) != 0 || got.NextCursor != nil {
		t.Fatalf("an unknown configuration lists %v", got)
	}
}

func TestListsLimitDefaultsTo50AndHasBounds(t *testing.T) {
	s, _, _ := apiServer(t)
	for range 101 {
		expectStatus(t, "POST", publish(s, orgA, uuid.NewString(), validBody(sampleYAML)), http.StatusCreated)
	}
	base := "?configurationId=" + configurationA
	for query, want := range map[string]int{base: 50, base + "&limit=1": 1, base + "&limit=100": 100} {
		got := list(t, s, orgA, query)
		if len(got.Publications) != want || got.NextCursor == nil {
			t.Fatalf("%s: %d publications, next %v; want %d and a next cursor", query, len(got.Publications), got.NextCursor, want)
		}
	}

	limitMessage := "limit must be an integer from 1 to 100."
	for query, message := range map[string]string{
		base + "&limit=0":                  limitMessage,
		base + "&limit=101":                limitMessage,
		base + "&limit=-1":                 limitMessage,
		base + "&limit=+5":                 limitMessage,
		base + "&limit=2.0":                limitMessage,
		base + "&limit=abc":                limitMessage,
		base + "&limit=":                   limitMessage,
		base + "&limit=1&limit=2":          "limit must be given once.",
		"":                                 "configurationId must be a UUID.",
		"?configurationId=":                "configurationId must be a UUID.",
		"?configurationId=config-marker-1": "configurationId must be a UUID.",
		base + "&configurationId=" + configurationA: "configurationId must be given once.",
		base + "&cursor=":                "cursor isn't one this API gave.",
		base + "&cursor=cursor-marker-2": "cursor isn't one this API gave.",
		base + "&cursor=a&cursor=b":      "cursor must be given once.",
		base + "&limit=%zz":              "The query string couldn't be read.",
	} {
		got := call(s, http.MethodGet, listTarget(orgA, query), nil, "")
		expectError(t, query, got, http.StatusBadRequest, codeInvalidRequest, message)
		if strings.Contains(got.Body.String(), "marker") {
			t.Fatalf("%s: the answer quotes the value: %s", query, got.Body.String())
		}
	}
	expectError(t, "a non-UUID organization", call(s, http.MethodGet, listTarget("x", base), nil, ""),
		http.StatusBadRequest, codeInvalidRequest, "organizationId must be a UUID.")
}

func TestDeleteOneIs204Then404AndNeverAnotherOrganizations(t *testing.T) {
	s, _, _ := apiServer(t)
	expectStatus(t, "POST", publish(s, orgA, publicationA, validBody(sampleYAML)), http.StatusCreated)

	expectError(t, "B's DELETE", call(s, http.MethodDelete, publicationTarget(orgB, publicationA), nil, ""),
		http.StatusNotFound, codeNotFound, messageNotFound)
	expectStatus(t, "A's GET after B's DELETE", call(s, http.MethodGet, publicationTarget(orgA, publicationA), nil, ""), http.StatusOK)

	got := call(s, http.MethodDelete, publicationTarget(orgA, publicationA), nil, "")
	if got.Code != http.StatusNoContent || got.Body.Len() != 0 {
		t.Fatalf("DELETE: %d %q, want 204 and no body", got.Code, got.Body.String())
	}
	expectError(t, "a second DELETE", call(s, http.MethodDelete, publicationTarget(orgA, publicationA), nil, ""),
		http.StatusNotFound, codeNotFound, messageNotFound)
	expectStatus(t, "GET after DELETE", call(s, http.MethodGet, publicationTarget(orgA, publicationA), nil, ""), http.StatusNotFound)
	expectError(t, "a non-UUID id", call(s, http.MethodDelete, publicationTarget(orgA, "x"), nil, ""),
		http.StatusBadRequest, codeInvalidRequest, "publicationId must be a UUID.")
}

func TestDeleteConfigurationRemovesExactlyItsPublicationsInTheOrganization(t *testing.T) {
	s, _, _ := apiServer(t)
	var ours []string
	for range 3 {
		id := uuid.NewString()
		ours = append(ours, id)
		expectStatus(t, "POST", publish(s, orgA, id, validBody(sampleYAML)), http.StatusCreated)
	}
	otherConfiguration, theirs := uuid.NewString(), uuid.NewString()
	expectStatus(t, "POST", publish(s, orgA, otherConfiguration, validBody(sampleYAML).with("configuration.id", configurationB)), http.StatusCreated)
	expectStatus(t, "POST", publish(s, orgB, theirs, validBody(sampleYAML)), http.StatusCreated)

	for range 2 { // the second time there are none: 204 still
		got := call(s, http.MethodDelete, configurationTarget(orgA, configurationA), nil, "")
		if got.Code != http.StatusNoContent || got.Body.Len() != 0 {
			t.Fatalf("DELETE: %d %q, want 204 and no body", got.Code, got.Body.String())
		}
	}
	for _, id := range ours {
		expectStatus(t, "GET a deleted one", call(s, http.MethodGet, publicationTarget(orgA, id), nil, ""), http.StatusNotFound)
	}
	expectStatus(t, "GET A's other configuration's", call(s, http.MethodGet, publicationTarget(orgA, otherConfiguration), nil, ""), http.StatusOK)
	expectStatus(t, "GET B's", call(s, http.MethodGet, publicationTarget(orgB, theirs), nil, ""), http.StatusOK)
	expectError(t, "a non-UUID configuration", call(s, http.MethodDelete, configurationTarget(orgA, "x"), nil, ""),
		http.StatusBadRequest, codeInvalidRequest, "configurationId must be a UUID.")
	expectError(t, "a non-UUID organization", call(s, http.MethodDelete, configurationTarget("x", configurationA), nil, ""),
		http.StatusBadRequest, codeInvalidRequest, "organizationId must be a UUID.")
}

// A failing store answers 500 internal_error, and the log line says which
// route failed without the error's text.
func TestAFailingStoreIs500WithoutDetail(t *testing.T) {
	s, _, log := publicationsServer(t, failingStore{})
	for _, request := range []struct{ method, target string }{
		{http.MethodPost, listTarget(orgA, "")},
		{http.MethodGet, publicationTarget(orgA, publicationA)},
		{http.MethodDelete, publicationTarget(orgA, publicationA)},
		{http.MethodGet, listTarget(orgA, "?configurationId="+configurationA)},
		{http.MethodDelete, configurationTarget(orgA, configurationA)},
	} {
		var payload []byte
		if request.method == http.MethodPost {
			payload = validBody(sampleYAML).bytes()
		}
		got := call(s, request.method, request.target, payload, jsonType)
		expectError(t, request.method+" "+request.target, got, http.StatusInternalServerError, codeInternalError, messageInternalError)
	}
	logged := log.String()
	if strings.Count(logged, `"msg":"the publications store failed"`) != 5 {
		t.Fatalf("the failures weren't each logged: %s", logged)
	}
	if strings.Contains(logged, errStoreDown.Error()) {
		t.Fatalf("the log holds the store's error: %s", logged)
	}
}

// Through the real routes: the log holds none of a publication's contents
// (YAML, names, email), no id from a path, and no secret.
func TestThePublicationRoutesLogNoContents(t *testing.T) {
	s, _, log := apiServer(t)
	marked := validBody("receivers: # yaml-marker-8e2\n").
		with("configuration.name", "name-marker-3c1").
		with("version.label", "label-marker-9d0").
		with("publishedBy.email", "email-marker-4f7@acme.io")
	publish(s, orgA, publicationA, marked)
	publish(s, orgA, publicationA, marked.withYAML("receivers: # yaml-marker-8e2 changed\n"))
	publish(s, orgA, publicationA, marked.with("version.number", 0))
	post(s, orgA, marked)
	post(s, orgA, marked.with("version.number", 0))
	call(s, http.MethodGet, publicationTarget(orgA, publicationA), nil, "")
	call(s, http.MethodGet, listTarget(orgA, "?configurationId="+configurationA), nil, "")
	call(s, http.MethodDelete, configurationTarget(orgA, configurationA), nil, "")

	logged := log.String()
	for _, leaked := range []string{
		"yaml-marker", "name-marker", "label-marker", "email-marker", testSecret,
		orgA, publicationA, configurationA,
	} {
		if strings.Contains(logged, leaked) {
			t.Errorf("the log holds %q: %s", leaked, logged)
		}
	}
	if !strings.Contains(logged, `"route":"`+publicationPath+`"`) {
		t.Errorf("the log doesn't name the route pattern: %s", logged)
	}
}

// A yaml with a live ${org:NAME} reference to a name over 128 characters,
// which no variable can have, is a 400 invalid_request naming yaml with a
// fixed message, and stores nothing; a 128-character name publishes. The
// name is never written down.
func TestAVersionReferencingANameOver128CharactersIsRefused(t *testing.T) {
	long := "N_" + strings.Repeat("A", 127) // 129 characters
	for name, server := range map[string]func(t *testing.T) (*Server, *syncBuffer){
		"postgres": func(t *testing.T) (*Server, *syncBuffer) {
			s, _, log := apiServer(t)
			return s, log
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, log := server(t)
			refused := publish(s, orgA, publicationA, validBody("a: ${org:"+long+"}\n"))
			expectError(t, "a 129-character name", refused, http.StatusBadRequest, codeInvalidRequest,
				"yaml references an organization variable name over 128 characters, which no variable can have.")
			expectStatus(t, "the GET after the refusal", call(s, http.MethodGet, publicationTarget(orgA, publicationA), nil, ""), http.StatusNotFound)

			// Escaped, the same text is no reference, and publishes; so does a
			// 128-character name.
			expectStatus(t, "the escaped reference", publish(s, orgA, publicationA, validBody("a: $${org:"+long+"}\n")), http.StatusCreated)
			expectStatus(t, "a 128-character name", publish(s, orgA, uuid.New().String(), validBody("a: ${org:"+long[:128]+"}\n")), http.StatusCreated)
			if strings.Contains(log.String(), long[:128]) {
				t.Errorf("the log holds the name: %s", log.String())
			}
		})
	}
}
