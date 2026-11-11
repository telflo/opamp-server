// Package orgvariables is the fill-in of organization variables: the rules a
// variable's name, value and description follow, the one scanner that finds
// a published version's live `${org:NAME}` references and fills them in
// (render.go), and the rule that says which level a collector's value comes
// from and which change it must have seen (stamps.go).
//
// The opamp-server's Go here is the reference: the API's test-run fill-in
// (TypeScript), the validator's (Python) and the web's reference extraction
// (TypeScript) are ports of it, and each reads the shared corpus,
// handlers/testdata/org-token-corpus.json, in its own tests.
//
// A variable's value, name and description are configuration text
// (CONTRIBUTING.md, "Secrets and what is never written down"): they never
// reach a log line, an error, a span or an audit row.
package orgvariables

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/telflo/telflo-opamp-server/store/fleets"
)

// The limits. Lengths are counted in characters (code points), as Postgres's
// char_length counts them.
const (
	// MaxNameLength bounds a variable's name. A reference's NAME may be
	// longer: it is still a live reference, which no variable can define, so
	// publishing it is refused.
	MaxNameLength = 128
	// MaxValueLength bounds a value: 0 to 1,024 characters on one line.
	MaxValueLength = 1024
	// MaxDescriptionLength bounds a variable's or a variable group's
	// description.
	MaxDescriptionLength = 1000
	// MaxGroupNameLength bounds a variable group's name.
	MaxGroupNameLength = 200
	// MaxRenderedBytes is 1 MiB of UTF-8: what a published version's YAML
	// may hold (management's maxYAMLBytes is this constant), and what a text
	// filled in with its variables may hold to be pushed. Over it the text
	// is never built.
	MaxRenderedBytes = 1 << 20
)

// ValidName reports whether name is a variable's name: the renderer's
// grammar, [A-Za-z_][A-Za-z0-9_]* (ASCII only, case-sensitive), at most
// MaxNameLength characters.
func ValidName(name string) bool {
	return len(name) <= MaxNameLength && nameLength(name, 0) == len(name) && name != ""
}

// ValidValue reports whether value is a value the variables store keeps:
// valid UTF-8 with no NUL, 0 to MaxValueLength characters, on one line: no
// C0 control (a tab, a line feed and a carriage return among them), no DEL,
// no C1 control (U+0085 among them), and neither U+2028 nor U+2029
// (migration 00012's ck_variables_value_single_line refuses the same set,
// NUL aside, which Postgres's text can't hold).
func ValidValue(value string) bool {
	return fleets.ValidText(value, 0, MaxValueLength) && fleets.NoControl(value) &&
		!strings.ContainsRune(value, '\u2028') && !strings.ContainsRune(value, '\u2029')
}

// ValidDescription reports whether description is a variable's or a
// variable group's description: none (nil), or trimmed, 1 to
// MaxDescriptionLength characters, with no control character.
func ValidDescription(description *string) bool {
	return description == nil || validTrimmedText(*description, MaxDescriptionLength)
}

// ValidGroupName reports whether name is a variable group's name: trimmed,
// 1 to MaxGroupNameLength characters, with no control character.
func ValidGroupName(name string) bool {
	return validTrimmedText(name, MaxGroupNameLength)
}

func validTrimmedText(s string, max int) bool {
	return fleets.Trimmed(s) && fleets.ValidText(s, 1, max) && fleets.NoControl(s)
}

// Resolved is what a collector's text is filled in with: for each
// organization variable, its value from the first level that has one, read
// from the top down (the pin's variable group's override, the collector
// group's variable group's override, the variable itself), and the stamp of
// the change that value must have been served after (Choose). It is never
// changed after it is built, so a cache may hand the same one to every
// reader.
type Resolved struct {
	// Revision is the organization's variables_revision the values were
	// read at; 0 when the organization never had a variable.
	Revision int64
	// Generation is the organization's generation (organizations.generation)
	// the values were read at, in the same statement as Revision; uuid.Nil
	// with Revision 0. A cache stores them only under this generation.
	Generation uuid.UUID
	// Values holds each variable's value, by name.
	Values map[string]string
	// Stamps holds each variable's stamp, by name (Choose): the row that
	// changed, never the variable group.
	Stamps map[string]int64
}

// Store reads an organization's variables.
type Store interface {
	// Variables resolves the organization's variables for a collector whose
	// collector group uses groupVariableGroupID and whose pin uses
	// pinnedVariableGroupID (nil: none).
	Variables(ctx context.Context, organizationID uuid.UUID, groupVariableGroupID, pinnedVariableGroupID *uuid.UUID) (Resolved, error)
}
