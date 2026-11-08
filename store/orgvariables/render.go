package orgvariables

import (
	"bufio"
	"io"
	"strings"
)

// The fill-in. A live reference is `${org:NAME}`, NAME the grammar
// [A-Za-z_][A-Za-z0-9_]* of any length, opened by a `$` that an even run of
// `$` precedes (precedingDollarRunEven). The legacy `$orgvar:NAME` form is
// plain text, filled in by nothing, and so is `${org:NAME:-default}`. A
// known NAME's value is written with every `$` doubled, so the collector's
// own confmap pass, which collapses `$$` to `$`, yields the value exactly: a
// value `${env:X}` stays that text. An unknown NAME is left as written.
// There is no YAML round trip: the bytes around each reference are the bytes
// published.
//
// One scanner (scan) serves References, RenderedLength, RenderTo and Render,
// so they can't disagree. The length comes first: a 1 MiB text of about
// 100,000 references to a value of 1,024 `$` fills in to about 200 MB, so
// the poll measures it, and over MaxRenderedBytes hashes it through
// RenderTo, never building it.

const opener = "${org:"

// precedingDollarRunEven reports whether an EVEN number of '$' immediately
// precede index i. confmap collapses each "$$" to a literal "$" in one
// left-to-right pass, so a reference opened by the '$' at i is a LIVE
// expansion iff the total run ending at i is odd — i.e. the run BEFORE i is
// even. An odd preceding run means this '$' is the trailing half of a "$$"
// escape pair and the reference is inert. The ports keep the same rule
// (the corpus's escape rows hold them to it).
func precedingDollarRunEven(text string, i int) bool {
	count := 0
	for k := i - 1; k >= 0 && text[k] == '$'; k-- {
		count++
	}
	return count%2 == 0
}

// nameLength is the length of the longest NAME of the grammar at text[from:],
// 0 when none starts there.
func nameLength(text string, from int) int {
	n := 0
	for i := from; i < len(text); i++ {
		c := text[i]
		letter := c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		if !letter && (n == 0 || c < '0' || c > '9') {
			break
		}
		n++
	}
	return n
}

// scan calls visit for each live reference in text, in order, with the
// bounds of its `${org:NAME}` and its NAME; it stops when visit answers
// false.
func scan(text string, visit func(start, end int, name string) bool) {
	for i := 0; i < len(text); {
		k := strings.Index(text[i:], opener)
		if k < 0 {
			return
		}
		start := i + k
		nameStart := start + len(opener)
		nameEnd := nameStart + nameLength(text, nameStart)
		if nameEnd == nameStart || nameEnd >= len(text) || text[nameEnd] != '}' {
			i = start + 1
			continue
		}
		end := nameEnd + 1
		if precedingDollarRunEven(text, start) && !visit(start, end, text[nameStart:nameEnd]) {
			return
		}
		i = end
	}
}

// escape writes every `$` of a value as `$$`.
func escape(value string) string {
	return strings.ReplaceAll(value, "$", "$$")
}

// measure is RenderedLength, and whether any reference is filled in.
func measure(text string, values map[string]string) (n int, names []string, filled bool) {
	n = len(text)
	// Each name's escaped value's length, or -1 for a name with no value:
	// counted once per name, however many times the text references it.
	var lengths map[string]int
	scan(text, func(start, end int, name string) bool {
		length, seen := lengths[name]
		if !seen {
			names = append(names, name)
			length = -1
			if v, ok := values[name]; ok {
				length = len(v) + strings.Count(v, "$")
			}
			if lengths == nil {
				lengths = map[string]int{}
			}
			lengths[name] = length
		}
		if length >= 0 {
			n += length - (end - start)
			filled = true
		}
		return true
	})
	return n, names, filled
}

// References is every live reference's NAME in text, defined or not, each
// once, in the order first seen; nil when there is none. It is the one
// extraction: publish stores it, the poll fills in by it, and every port is
// held to it.
func References(text string) []string {
	_, names, _ := measure(text, nil)
	return names
}

// RenderedLength is the length in bytes of text filled in with values,
// counted without building it, and References(text).
func RenderedLength(text string, values map[string]string) (n int, names []string) {
	n, names, _ = measure(text, values)
	return n, names
}

// RenderTo writes text filled in with values to w, without building it: the
// poll hashes a text over MaxRenderedBytes this way. It writes exactly
// RenderedLength bytes, or answers w's error.
func RenderTo(w io.Writer, text string, values map[string]string) error {
	bw := bufio.NewWriterSize(w, 64<<10)
	if err := fill(bw, text, values); err != nil {
		return err
	}
	return bw.Flush()
}

// Render is text filled in with values, built into a buffer of exactly
// RenderedLength bytes, and References(text). A text in which no reference
// is filled in (none live, or none known) comes back as it is. The caller
// measures first: Render builds whatever length the fill-in comes to.
func Render(text string, values map[string]string) (rendered string, names []string) {
	n, names, filled := measure(text, values)
	if !filled {
		return text, names
	}
	var b strings.Builder
	b.Grow(n)
	_ = fill(&b, text, values) // a strings.Builder's writes never fail
	return b.String(), names
}

// fill writes text filled in with values to w.
func fill(w io.StringWriter, text string, values map[string]string) error {
	escaped := map[string]string{}
	cursor := 0
	var err error
	scan(text, func(start, end int, name string) bool {
		e, ok := escaped[name]
		if !ok {
			v, known := values[name]
			if !known {
				return true // an unknown name is left as written
			}
			e = escape(v)
			escaped[name] = e
		}
		if _, err = w.WriteString(text[cursor:start]); err != nil {
			return false
		}
		if _, err = w.WriteString(e); err != nil {
			return false
		}
		cursor = end
		return true
	})
	if err != nil {
		return err
	}
	_, err = w.WriteString(text[cursor:])
	return err
}
