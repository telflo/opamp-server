package orgvariables

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unsafe"
)

// The shared corpus: the Go fill-in is the reference, and the API's, the
// validator's and the web's ports read the same file.
type renderCase struct {
	Name     string            `json:"name"`
	Text     string            `json:"text"`
	Vars     map[string]string `json:"vars"`
	Expected string            `json:"expected"`
	Names    []string          `json:"names"`
}

type extractCase struct {
	Name     string   `json:"name"`
	Text     string   `json:"text"`
	Expected []string `json:"expected"`
}

type corpus struct {
	Comment string        `json:"_comment"`
	Render  []renderCase  `json:"render"`
	Extract []extractCase `json:"extract"`
}

func loadCorpus(t *testing.T) corpus {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "handlers", "testdata", "org-token-corpus.json"))
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var c corpus
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	if len(c.Render) == 0 || len(c.Extract) == 0 {
		t.Fatal("the corpus has no render or no extract rows")
	}
	for _, r := range c.Render {
		if r.Names == nil {
			t.Fatalf("render row %s has no names", r.Name)
		}
	}
	return c
}

// sameNames compares two name lists, nil and empty alike.
func sameNames(a, b []string) bool {
	return len(a) == 0 && len(b) == 0 || slices.Equal(a, b)
}

func TestRenderMatchesTheCorpus(t *testing.T) {
	for _, c := range loadCorpus(t).Render {
		t.Run(c.Name, func(t *testing.T) {
			got, names := Render(c.Text, c.Vars)
			if got != c.Expected {
				t.Errorf("Render(%q) = %q; want %q", c.Text, got, c.Expected)
			}
			if !sameNames(names, c.Names) {
				t.Errorf("Render(%q)'s names = %q; want %q", c.Text, names, c.Names)
			}
		})
	}
}

// The extract rows, read by Go for the first time, and every render row's
// names: References is Render's names.
func TestReferencesMatchTheCorpus(t *testing.T) {
	c := loadCorpus(t)
	for _, e := range c.Extract {
		t.Run(e.Name, func(t *testing.T) {
			if got := References(e.Text); !sameNames(got, e.Expected) {
				t.Errorf("References(%q) = %q; want %q", e.Text, got, e.Expected)
			}
		})
	}
	for _, r := range c.Render {
		t.Run("render/"+r.Name, func(t *testing.T) {
			if got := References(r.Text); !sameNames(got, r.Names) {
				t.Errorf("References(%q) = %q; want %q", r.Text, got, r.Names)
			}
		})
	}
}

// confmapCollapse is the collector's own pass over a scalar: each `$$`,
// left to right, becomes `$`.
func confmapCollapse(s string) string {
	return strings.ReplaceAll(s, "$$", "$")
}

// A value is written literally. After the collector's confmap pass collapses
// `$$`, each filled-in scalar is its value exactly, whatever `$`, `${…}` or
// `$${…}` the value holds.
func TestRenderWritesAValuesDollarsDoubled(t *testing.T) {
	values := []string{
		"plain", "", "a$b", "a$$b", "$", "$$", "$$$", "${env:HOME}", "$${env:HOME}",
		"$$${env:HOME}", "${org:OTHER}", "${HOME}", "trailing$", "$leading", "é$€$😀",
	}
	for _, v := range values {
		t.Run(v, func(t *testing.T) {
			vars := map[string]string{"V": v, "OTHER": "other"}
			for _, text := range []string{"a: ${org:V}", "a: $$${org:V}", "a: x${org:V}y", "a: ${org:V}${org:V}"} {
				got, _ := Render(text, vars)
				want := confmapCollapse(strings.ReplaceAll(text, "${org:V}", "\x00")) // the text around it, collapsed
				want = strings.ReplaceAll(want, "\x00", v)
				if collapsed := confmapCollapse(got); collapsed != want {
					t.Errorf("Render(%q) with V=%q = %q, which confmap reads as %q; want %q", text, v, got, collapsed, want)
				}
				if strings.Contains(got, "${org:OTHER}") && !strings.Contains(got, "$${org:OTHER}") {
					t.Errorf("Render(%q) with V=%q = %q: a reference inside a value was left live", text, v, got)
				}
			}
		})
	}
}

// `$orgvar:NAME` is plain text, filled in by nothing and referencing
// nothing.
func TestTheLegacyFormIsLeftAsWritten(t *testing.T) {
	vars := map[string]string{"HOST": "x.example", "SAMPLE": "0.5"}
	for _, text := range []string{
		"a: $orgvar:HOST\n", "a: $$orgvar:HOST\n", "a: $$$orgvar:HOST\n", "a: https://$orgvar:HOST:4317\n",
		"a: $orgvar:HOST $orgvar:SAMPLE\n",
	} {
		got, names := Render(text, vars)
		if got != text || len(names) != 0 || len(References(text)) != 0 {
			t.Errorf("Render(%q) = %q, %q; want it as written, with no name", text, got, names)
		}
	}
	got, names := Render("a: ${org:HOST}\nb: $orgvar:SAMPLE\n", vars)
	if got != "a: x.example\nb: $orgvar:SAMPLE\n" || !slices.Equal(names, []string{"HOST"}) {
		t.Errorf("mixed forms = %q, %q; want only the braced one filled in and named", got, names)
	}
}

// names is every live reference's NAME, defined or not, once each, in the
// order first seen; escaped, defaulted, legacy and malformed ones are not
// references.
func TestRenderNamesEveryLiveReferenceOnceInOrder(t *testing.T) {
	text := "a: ${org:B}\nb: ${org:A}\nc: ${org:B}\nd: $${org:C}\ne: ${org:D}\n" +
		"f: ${org:E:-x}\ng: $orgvar:F\nh: ${org:9G}\ni: $$${org:H}\nj: ${org:A}${org:I}\n"
	want := []string{"B", "A", "D", "H", "I"}
	for _, vars := range []map[string]string{nil, {}, {"A": "1", "B": "2"}, {"C": "3", "D": "4", "E": "5", "F": "6", "G": "7"}} {
		if _, names := Render(text, vars); !slices.Equal(names, want) {
			t.Errorf("Render with %v named %q; want %q", vars, names, want)
		}
		if _, names := RenderedLength(text, vars); !slices.Equal(names, want) {
			t.Errorf("RenderedLength with %v named %q; want %q", vars, names, want)
		}
	}
	if got := References(text); !slices.Equal(got, want) {
		t.Errorf("References = %q; want %q", got, want)
	}
}

// No YAML round trip: a text in which nothing is filled in comes back as
// the very string given, not a copy, and costs no allocation when it
// references nothing.
func TestAnUnchangedTextIsReturnedAsIs(t *testing.T) {
	for _, c := range []struct {
		text string
		vars map[string]string
	}{
		{"a: plain\n", map[string]string{"A": "1"}},
		{"a: $${org:A}\nb: $orgvar:A\nc: ${env:A}\n", map[string]string{"A": "1"}},
		{"a: ${org:A}\n", nil},
		{"a: ${org:MISSING}\n", map[string]string{"A": "1"}},
		{"", map[string]string{"A": "1"}},
	} {
		got, _ := Render(c.text, c.vars)
		if got != c.text || unsafe.StringData(got) != unsafe.StringData(c.text) {
			t.Errorf("Render(%q) didn't return the text as it is", c.text)
		}
	}
	text := "receivers:\n  otlp: {}\n# $${org:A} $orgvar:A\n"
	vars := map[string]string{"A": "1"}
	if allocs := testing.AllocsPerRun(100, func() { Render(text, vars) }); allocs != 0 {
		t.Errorf("a text with no live reference allocated %v times; want 0", allocs)
	}
}

// countingWriter counts what it is written, and passes it on.
type countingWriter struct {
	n int
	w io.Writer
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += len(p)
	return c.w.Write(p)
}

// The three share one scanner, so they agree on every corpus row and on the
// escape cases.
func TestRenderedLengthAndRenderToAgreeWithRender(t *testing.T) {
	type row struct {
		name, text string
		vars       map[string]string
	}
	var rows []row
	for _, c := range loadCorpus(t).Render {
		rows = append(rows, row{c.Name, c.Text, c.Vars})
	}
	escapes := map[string]string{"A": "a$b", "B": "$$", "C": "", "D": "${env:X}", "E": "€$😀", "F": "${org:A}"}
	for i, text := range []string{
		"${org:A}", "$${org:A}", "$$${org:A}", "$$$${org:A}", "$$$$${org:A}", "${org:A}${org:B}",
		"$${org:A}${org:B}", "x$${org:C}$$${org:C}y", "${org:D} ${org:E} ${org:F} ${org:MISSING}",
		"${org:A", "${org:}", "${org:A-B}", "$", "$$", "${org:A}$", "${org:B}${org:B}${org:B}",
		strings.Repeat("$", 7) + "{org:E}" + strings.Repeat("${org:A}", 50),
	} {
		rows = append(rows, row{"escape_" + string(rune('a'+i)), text, escapes})
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			rendered, names := Render(r.text, r.vars)
			n, lengthNames := RenderedLength(r.text, r.vars)
			if n != len(rendered) || !sameNames(names, lengthNames) {
				t.Errorf("RenderedLength(%q) = %d, %q; Render's text is %d bytes, named %q", r.text, n, lengthNames, len(rendered), names)
			}
			var streamed bytes.Buffer
			if err := RenderTo(&streamed, r.text, r.vars); err != nil || streamed.String() != rendered {
				t.Errorf("RenderTo(%q) = %q, %v; want Render's %q", r.text, streamed.String(), err, rendered)
			}
		})
	}
}

// RenderTo answers its writer's error.
func TestRenderToAnswersTheWritersError(t *testing.T) {
	broken := errors.New("broken writer")
	text := strings.Repeat("a: ${org:A}\n", 20000)
	err := RenderTo(failingWriter{broken}, text, map[string]string{"A": strings.Repeat("x", 100)})
	if !errors.Is(err, broken) {
		t.Fatalf("RenderTo into a failing writer = %v; want its error", err)
	}
}

type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

// A 1 MiB text of 100,000 references to a value of 1,024 `$` fills in to
// about 200 MB. RenderedLength answers that length, and the length and then
// a hash streamed through RenderTo allocate under 16 MiB: the text is never
// built.
func TestAnOversizedFillInIsMeasuredWithoutBeingBuilt(t *testing.T) {
	const references = 100_000
	ref := "${org:V}\n"
	text := strings.Repeat(ref, references)
	text += "#" + strings.Repeat("p", MaxRenderedBytes-len(text)-2) + "\n"
	if len(text) != MaxRenderedBytes {
		t.Fatalf("the text is %d bytes; want %d", len(text), MaxRenderedBytes)
	}
	value := strings.Repeat("$", MaxValueLength)
	if !ValidValue(value) {
		t.Fatal("the value isn't one the store keeps")
	}
	vars := map[string]string{"V": value}
	want := MaxRenderedBytes + references*(2*MaxValueLength-len("${org:V}"))

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	n, names := RenderedLength(text, vars)
	hash := sha256.New()
	counted := &countingWriter{w: hash}
	err := RenderTo(counted, text, vars)
	runtime.ReadMemStats(&after)

	if n != want || !slices.Equal(names, []string{"V"}) {
		t.Fatalf("RenderedLength = %d, %q; want %d, [V]", n, names, want)
	}
	if err != nil || counted.n != n {
		t.Fatalf("RenderTo wrote %d bytes, %v; want %d", counted.n, err, n)
	}
	if grown := after.TotalAlloc - before.TotalAlloc; grown >= 16<<20 {
		t.Fatalf("measuring and hashing allocated %d bytes; want under 16 MiB (the text is %d)", grown, n)
	}

	// The streamed bytes are the filled-in text: each reference its value
	// with every `$` doubled, the rest as written.
	independent := sha256.New()
	doubled := []byte(strings.Repeat("$$", MaxValueLength) + "\n")
	for range references {
		independent.Write(doubled)
	}
	independent.Write([]byte(text[references*len(ref):]))
	if !bytes.Equal(hash.Sum(nil), independent.Sum(nil)) {
		t.Fatal("RenderTo's bytes aren't the filled-in text")
	}
}
