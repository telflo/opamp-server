package apikeys_test

import (
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/telflo/opamp-server/store/apikeys"
)

// A generated key parses back to its id and its secret's hash, and only
// the key it is matches it.
func TestAGeneratedKeyParsesBackAndMatchesOnlyItself(t *testing.T) {
	id, key, sum, err := apikeys.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != apikeys.Length || !strings.HasPrefix(key, apikeys.Prefix) {
		t.Fatalf("a key of %d characters, %q…; want %d starting %q", len(key), key[:8], apikeys.Length, apikeys.Prefix)
	}
	gotID, gotSum, ok := apikeys.Parse(key)
	if !ok || gotID != id || gotSum != sum {
		t.Fatalf("Parse = %s, %v; want the key's id and hash", gotID, ok)
	}
	stored := apikeys.Authenticated{SecretSHA256: sum}
	if !stored.Matches(gotSum) {
		t.Error("the key doesn't match its own hash")
	}
	_, other, _, err := apikeys.Generate()
	if err != nil {
		t.Fatal(err)
	}
	_, otherSum, _ := apikeys.Parse(other)
	if stored.Matches(otherSum) {
		t.Error("another key matches")
	}
	if _, again, _, _ := apikeys.Generate(); again == key {
		t.Error("two keys are the same")
	}
}

// Anything not spelled exactly as Generate spells a key is refused.
func TestParseRefusesAnythingButAKeysExactSpelling(t *testing.T) {
	_, key, _, err := apikeys.Generate()
	if err != nil {
		t.Fatal(err)
	}
	hexID, secret := key[4:36], key[37:]
	// A secret's last character carries 2 unused bits, always zero in a
	// key: "B" sets one.
	lastBits := secret[:42] + "B"
	for name, bearer := range map[string]string{
		"empty":                    "",
		"the management secret":    strings.Repeat("s", 48),
		"another prefix":           "opx_" + hexID + "_" + secret,
		"no prefix":                hexID + "_" + secret + "xxxx",
		"an upper-case id":         "opk_" + strings.ToUpper(hexID) + "_" + secret,
		"a hyphenated id":          "opk_" + hexID[:8] + "-" + hexID[9:] + "_" + secret,
		"another separator":        "opk_" + hexID + "." + secret,
		"a padded secret":          "opk_" + hexID + "_" + secret[:42] + "=",
		"standard base64":          "opk_" + hexID + "_" + secret[:42] + "+",
		"a secret too short":       "opk_" + hexID + "_" + secret[:42],
		"a secret too long":        key + "A",
		"trailing bits set":        "opk_" + hexID + "_" + lastBits,
		"a space":                  " " + key[1:],
		"a non-hex id":             "opk_" + "g" + hexID[1:] + "_" + secret,
		"the right length, no key": strings.Repeat("a", apikeys.Length),
	} {
		if _, _, ok := apikeys.Parse(bearer); ok {
			t.Errorf("%s: Parse accepted it", name)
		}
	}
	// The hash is of the secret alone, as the store keeps it.
	if _, sum, _ := apikeys.Parse(key); sum == sha256.Sum256([]byte(key)) {
		t.Error("the hash is of the whole key, want the secret's")
	}
}
