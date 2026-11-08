package orgvariables

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// A value is one line. Over every code point, exactly C0 (NUL among them,
// which Postgres can't hold), DEL, C1, U+2028 and U+2029 are refused,
// wherever they stand; a value is 0 to 1,024 characters of valid UTF-8.
func TestValidValueRefusesEveryLineBreakAndControlCharacter(t *testing.T) {
	refusedHere := func(r rune) bool {
		return r <= 0x1f || (r >= 0x7f && r <= 0x9f) || r == 0x2028 || r == 0x2029
	}
	refused := 0
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue // a surrogate: no string holds one
		}
		s := string(r)
		for _, value := range []string{s, "a" + s, s + "b", "a" + s + "b"} {
			if ValidValue(value) == refusedHere(r) {
				t.Fatalf("ValidValue(%q) = %v; want %v", value, ValidValue(value), !refusedHere(r))
			}
		}
		if refusedHere(r) {
			refused++
		}
	}
	if refused != 67 {
		t.Fatalf("%d code points refused; want 67 (32 C0, DEL, 32 C1, U+2028, U+2029)", refused)
	}
	for _, line := range []string{"a\nb", "a\r\nb", "a\rb", "a\tb", "a\u0085b", "a\u2028b", "a\u2029b", "a\x00b", "\x7f"} {
		if ValidValue(line) {
			t.Errorf("ValidValue(%q) = true; want a line break or control character refused", line)
		}
	}
	for _, ok := range []string{"", " ", " padded ", "${env:X}", "a$b", strings.Repeat("x", MaxValueLength), strings.Repeat("€", MaxValueLength), strings.Repeat("😀", MaxValueLength)} {
		if !ValidValue(ok) {
			t.Errorf("ValidValue(%.20q…) = false; want true", ok)
		}
	}
	for _, bad := range []string{strings.Repeat("x", MaxValueLength+1), strings.Repeat("€", MaxValueLength+1), "a\xffb", "\xc3"} {
		if ValidValue(bad) {
			t.Errorf("ValidValue(%.20q…) = true; want too long or invalid UTF-8 refused", bad)
		}
	}
}

// A variable's name is exactly what the renderer reads as a reference's
// NAME, at most 128 characters: every valid name is a live reference of
// itself, and a candidate the renderer doesn't read as a NAME is refused.
func TestValidNameIsTheRenderersGrammar(t *testing.T) {
	candidates := []string{
		"A", "a", "_", "_1", "OTLP_EP", "otlp_ep", "Tenant9", strings.Repeat("N", MaxNameLength),
		"", "1A", "9", "A-B", "A.B", "A B", " A", "A ", "A}", "A:", "Ä", "É_X", "Aİ", "A\n",
		"${org:A}", "a$b", strings.Repeat("N", MaxNameLength+1),
	}
	for c := 0; c < 128; c++ { // every ASCII character, alone and after a letter
		candidates = append(candidates, string(rune(c)), "A"+string(rune(c)))
	}
	for _, name := range candidates {
		live := slices.Equal(References("${org:"+name+"}"), []string{name})
		want := live && utf8.RuneCountInString(name) <= MaxNameLength
		if ValidName(name) != want {
			t.Errorf("ValidName(%q) = %v; the renderer reads it as a NAME: %v", name, !want, live)
		}
	}
	if long := strings.Repeat("N", MaxNameLength+1); ValidName(long) || !slices.Equal(References("${org:"+long+"}"), []string{long}) {
		t.Error("a 129-character NAME must be a live reference that no variable can be named")
	}
}

// A description is none, or trimmed text of 1 to 1,000 characters with no
// control character; a variable group's name is trimmed text of 1 to 200.
func TestValidDescriptionAndValidGroupName(t *testing.T) {
	ptr := func(s string) *string { return &s }
	if !ValidDescription(nil) || !ValidDescription(ptr("Where the gateway sends")) || !ValidDescription(ptr(strings.Repeat("é", MaxDescriptionLength))) {
		t.Error("a valid description was refused")
	}
	for _, bad := range []string{"", " lead", "trail ", "a\nb", "a\tb", "a\x00b", "a\u0085b", strings.Repeat("é", MaxDescriptionLength+1), "\xff"} {
		if ValidDescription(ptr(bad)) {
			t.Errorf("ValidDescription(%.20q) = true; want false", bad)
		}
	}
	if !ValidGroupName("Production EU") || !ValidGroupName(strings.Repeat("g", MaxGroupNameLength)) {
		t.Error("a valid group name was refused")
	}
	for _, bad := range []string{"", " lead", "trail ", "a\nb", "a\x1bb", strings.Repeat("g", MaxGroupNameLength+1)} {
		if ValidGroupName(bad) {
			t.Errorf("ValidGroupName(%.20q) = true; want false", bad)
		}
	}
}
