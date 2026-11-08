package management

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/orgvariables"
	"github.com/telflo/opamp-server/store/publications"
)

// The limits of a publication. openapi.yaml states each.
const (
	maxNameCharacters  = 200
	maxLabelCharacters = 80
	maxEmailCharacters = 320
	// maxYAMLBytes is 1 MiB of UTF-8: the cap a text filled in with its
	// organization variables is held to as well.
	maxYAMLBytes     = orgvariables.MaxRenderedBytes
	maxVersionNumber = math.MaxInt32 // the column is an integer
	// maxCollectorCharacters bounds a collector's distribution and version,
	// as openapi.yaml and migration 00003 do.
	maxCollectorCharacters = 64
)

// requestError is a 400 or 422 the handler answers. Its message names the
// field and never quotes a value from the request.
type requestError struct {
	status  int
	code    errorCode
	message string
}

func (e *requestError) Error() string { return e.message }

func invalid(format string, args ...any) *requestError {
	return &requestError{status: http.StatusBadRequest, code: codeInvalidRequest, message: fmt.Sprintf(format, args...)}
}

func writeRequestError(w http.ResponseWriter, err *requestError) {
	writeError(w, err.status, err.code, err.message)
}

// canonicalUUID is the only form of id the API takes: 8-4-4-4-12 hex digits
// (either case), as JSON Schema's "uuid" format has it. uuid.Parse alone
// would also take braces, a urn: prefix, or no hyphens.
var canonicalUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func parseUUID(s string) (uuid.UUID, bool) {
	if !canonicalUUID.MatchString(s) {
		return uuid.UUID{}, false
	}
	id, err := uuid.Parse(s)
	return id, err == nil
}

// pathUUID reads a path id, or answers 400 naming the parameter.
func pathUUID(r *http.Request, name string) (uuid.UUID, *requestError) {
	id, ok := parseUUID(r.PathValue(name))
	if !ok {
		return uuid.UUID{}, invalid("%s must be a UUID.", name)
	}
	return id, nil
}

// checkJSONContentType answers 400 unless the request says its body is
// JSON: application/json, with no parameter but charset=utf-8.
func checkJSONContentType(r *http.Request) *requestError {
	const message = "Content-Type must be application/json."
	values := r.Header.Values("Content-Type")
	if len(values) != 1 {
		return invalid(message)
	}
	mediaType, params, err := mime.ParseMediaType(values[0])
	if err != nil || mediaType != "application/json" {
		return invalid(message)
	}
	for name, value := range params {
		if name != "charset" || !strings.EqualFold(value, "utf-8") {
			return invalid(message)
		}
	}
	return nil
}

// jsonObject is one JSON object of the body, read field by field so a
// missing field, a null, a wrong type and an unknown field each get their
// own message. path is where it sits ("" for the body, else "version.").
type jsonObject struct {
	path   string
	fields map[string]json.RawMessage
}

// jsonNull is a JSON null as json.RawMessage holds it.
const jsonNull = "null"

// decodeObject reads raw as an object with exactly the given fields: every
// one present (null where allowed), and no other.
func decodeObject(raw json.RawMessage, name, path string, fields ...string) (*jsonObject, *requestError) {
	return decodeObjectWithOptional(raw, name, path, fields, nil)
}

// decodeObjectWithOptional reads raw as an object with every required field
// present (null where allowed), any of the optional ones, and no other.
func decodeObjectWithOptional(raw json.RawMessage, name, path string, required, optional []string) (*jsonObject, *requestError) {
	var decoded map[string]json.RawMessage
	if string(raw) == jsonNull || json.Unmarshal(raw, &decoded) != nil || decoded == nil {
		if name == "" {
			return nil, invalid("The request body must be a JSON object.")
		}
		return nil, invalid("%s must be an object.", name)
	}
	object := &jsonObject{path: path, fields: decoded}
	for _, field := range required {
		if _, present := decoded[field]; !present {
			return nil, invalid("%s is required.", object.name(field))
		}
	}
	known := len(required)
	for _, field := range optional {
		if object.has(field) {
			known++
		}
	}
	if len(decoded) != known {
		if name == "" {
			return nil, invalid("The request body has a field this API doesn't know.")
		}
		return nil, invalid("%s has a field this API doesn't know.", name)
	}
	return object, nil
}

func (o *jsonObject) name(field string) string {
	return o.path + field
}

func (o *jsonObject) has(field string) bool {
	_, present := o.fields[field]
	return present
}

func (o *jsonObject) isNull(field string) bool {
	return string(o.fields[field]) == jsonNull
}

// string reads a string field that may not hold U+0000 (Postgres's text
// can't).
func (o *jsonObject) string(field string) (string, *requestError) {
	var value string
	if o.isNull(field) || json.Unmarshal(o.fields[field], &value) != nil {
		return "", invalid("%s must be a string.", o.name(field))
	}
	if strings.ContainsRune(value, 0) {
		return "", invalid("%s must not contain U+0000.", o.name(field))
	}
	return value, nil
}

// boundedString reads a string of min to max characters (code points).
func (o *jsonObject) boundedString(field string, min, max int) (string, *requestError) {
	value, err := o.string(field)
	if err != nil {
		return "", err
	}
	if n := utf8.RuneCountInString(value); n < min || n > max {
		return "", invalid("%s must be %d to %d characters.", o.name(field), min, max)
	}
	return value, nil
}

func (o *jsonObject) uuid(field string) (uuid.UUID, *requestError) {
	var value string
	if o.isNull(field) || json.Unmarshal(o.fields[field], &value) != nil {
		return uuid.UUID{}, invalid("%s must be a UUID.", o.name(field))
	}
	id, ok := parseUUID(value)
	if !ok {
		return uuid.UUID{}, invalid("%s must be a UUID.", o.name(field))
	}
	return id, nil
}

func (o *jsonObject) object(field string, fields ...string) (*jsonObject, *requestError) {
	return decodeObject(o.fields[field], o.name(field), o.name(field)+".", fields...)
}

// numericOffset is an RFC 3339 numeric offset, as it ends a date-time.
var numericOffset = regexp.MustCompile(`[+-]([0-9]{2}):([0-9]{2})$`)

// parseDateTime reads an RFC 3339 date-time, refusing what time.Parse lets
// through but RFC 3339 doesn't (an offset of 24 hours or more, or of 60
// minutes, or a comma before the fraction of a second), and an instant
// outside the years 0001 to 9999 in UTC, which no reply could write in RFC
// 3339. RFC 3339 allows a lowercase "t" and "z", which time.Parse doesn't,
// so they are upper-cased first; it allows a leap second (second 60), which
// time.Parse refuses, and so does the API. Its errors name the field, never
// the value.
func parseDateTime(field, value string) (time.Time, *requestError) {
	value = strings.Map(func(r rune) rune {
		switch r {
		case 't':
			return 'T'
		case 'z':
			return 'Z'
		}
		return r
	}, value)
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || strings.ContainsRune(value, ',') {
		return time.Time{}, invalid("%s must be an RFC 3339 date-time.", field)
	}
	if offset := numericOffset.FindStringSubmatch(value); offset != nil && (offset[1] > "23" || offset[2] > "59") {
		return time.Time{}, invalid("%s must be an RFC 3339 date-time.", field)
	}
	if !publications.InRange(parsed) {
		return time.Time{}, invalid("%s must be from the year 0001 to 9999, in UTC.", field)
	}
	return parsed, nil
}

// sha256Pattern is a SHA-256 as the API takes it: 64 lowercase hex digits.
var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// The formats of a collector's fields, as openapi.yaml and migration
// 00003's checks write them: [0-9], never \d, so the server, the
// platform's fake (ajv) and Postgres agree on every input.
var (
	// collectorDistributionPattern is a distribution's id: lowercase letters
	// and digits in words joined by single hyphens ("otelcol-contrib").
	collectorDistributionPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	// collectorVersionPattern is major.minor.patch ("0.162.0").
	collectorVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
)

// decodeBody reads a request body as one JSON value of UTF-8, with nothing
// but whitespace after it, for decodeObject to check it is an object. Every
// body the API takes is read through it.
func decodeBody(body []byte) (json.RawMessage, *requestError) {
	if !utf8.Valid(body) {
		return nil, invalid("The request body must be UTF-8.")
	}
	var raw json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(body))
	if decoder.Decode(&raw) != nil {
		return nil, invalid("The request body must be a JSON object.")
	}
	// Only whitespace may follow the object: a second value, or anything
	// that isn't JSON at all, is refused (Token answers io.EOF only for
	// nothing left).
	if _, err := decoder.Token(); err != io.EOF {
		return nil, invalid("The request body must be one JSON object.")
	}
	return raw, nil
}

// decodePublication reads a POST's body into the publication it describes,
// checking every field and its bound, then the digest; hasID says whether
// the body named the publication's id. The organization's id, a missing
// publication id, ReceivedAt and nothing else are left to the caller.
func decodePublication(body []byte) (p publications.Publication, hasID bool, err *requestError) {
	raw, err := decodeBody(body)
	if err != nil {
		return p, false, err
	}

	top, err := decodeObjectWithOptional(raw, "", "",
		[]string{"configuration", "version", "yaml", "sha256", "publishedBy", "publishedAt", "collector"}, []string{"id"})
	if err != nil {
		return p, false, err
	}
	if hasID = top.has("id"); hasID {
		if p.ID, err = top.uuid("id"); err != nil {
			return p, false, err
		}
	}
	p, err = decodePublicationFields(top, p)
	return p, hasID, err
}

// decodePublicationFields reads every field of a publication's body but its
// id into p.
func decodePublicationFields(top *jsonObject, p publications.Publication) (publications.Publication, *requestError) {
	var err *requestError

	configuration, err := top.object("configuration", "id", "name")
	if err != nil {
		return p, err
	}
	if p.ConfigurationID, err = configuration.uuid("id"); err != nil {
		return p, err
	}
	if p.ConfigurationName, err = configuration.boundedString("name", 1, maxNameCharacters); err != nil {
		return p, err
	}

	version, err := top.object("version", "id", "number", "label")
	if err != nil {
		return p, err
	}
	if p.VersionID, err = version.uuid("id"); err != nil {
		return p, err
	}
	// Any JSON number whose value is a whole number counts, 3.0 and 3e0 as
	// well as 3, as JSON Schema's "integer" has it (openapi.yaml): read as a
	// double, as JSON numbers usually are, the platform's included.
	var number float64
	if version.isNull("number") || json.Unmarshal(version.fields["number"], &number) != nil ||
		number != math.Trunc(number) || number < 1 || number > maxVersionNumber {
		return p, invalid("version.number must be an integer from 1 to %d.", maxVersionNumber)
	}
	p.VersionNumber = int32(number)
	if !version.isNull("label") {
		label, err := version.boundedString("label", 1, maxLabelCharacters)
		if err != nil {
			return p, err
		}
		p.VersionLabel = &label
	}

	if p.YAML, err = top.string("yaml"); err != nil {
		return p, err
	}
	if len(p.YAML) < 1 || len(p.YAML) > maxYAMLBytes {
		return p, invalid("yaml must be 1 to %d bytes of UTF-8.", maxYAMLBytes)
	}

	if p.SHA256, err = top.string("sha256"); err != nil {
		return p, err
	}
	if !sha256Pattern.MatchString(p.SHA256) {
		return p, invalid("sha256 must be 64 lowercase hexadecimal characters.")
	}

	if !top.isNull("publishedBy") {
		actor, err := top.object("publishedBy", "id", "email")
		if err != nil {
			return p, err
		}
		var a publications.Actor
		if a.ID, err = actor.uuid("id"); err != nil {
			return p, err
		}
		if a.Email, err = actor.boundedString("email", 1, maxEmailCharacters); err != nil {
			return p, err
		}
		p.PublishedBy = &a
	}

	publishedAt, err := top.string("publishedAt")
	if err != nil {
		return p, invalid("publishedAt must be an RFC 3339 date-time.")
	}
	parsed, err := parseDateTime("publishedAt", publishedAt)
	if err != nil {
		return p, err
	}
	// Kept to the millisecond, the precision replies carry: the store
	// holds, and answers, the same instant.
	p.PublishedAt = parsed.UTC().Truncate(publications.Precision)

	if p.Collector, err = decodeCollector(top); err != nil {
		return p, err
	}

	// Last, once the body is otherwise valid: the digest must be of yaml's
	// UTF-8 bytes (422, not 400).
	sum := sha256.Sum256([]byte(p.YAML))
	if hex.EncodeToString(sum[:]) != p.SHA256 {
		return p, &requestError{
			status:  http.StatusUnprocessableEntity,
			code:    codeSHA256Mismatch,
			message: "sha256 isn't the SHA-256 of yaml's UTF-8 bytes.",
		}
	}
	return p, nil
}

// decodeCollector reads the body's collector: an object with exactly a
// distribution and a version, each in its format and at most 64 characters.
// Null is refused: every publication has a collector.
func decodeCollector(top *jsonObject) (publications.Collector, *requestError) {
	var c publications.Collector
	collector, err := top.object("collector", "distribution", "version")
	if err != nil {
		return c, err
	}
	if c.Distribution, err = collector.string("distribution"); err != nil {
		return c, err
	}
	if len(c.Distribution) > maxCollectorCharacters || !collectorDistributionPattern.MatchString(c.Distribution) {
		return c, invalid("collector.distribution must be lowercase letters and digits in words joined by single hyphens, at most %d characters.", maxCollectorCharacters)
	}
	if c.Version, err = collector.string("version"); err != nil {
		return c, err
	}
	if len(c.Version) > maxCollectorCharacters || !collectorVersionPattern.MatchString(c.Version) {
		return c, invalid("collector.version must be major.minor.patch, at most %d characters.", maxCollectorCharacters)
	}
	return c, nil
}
