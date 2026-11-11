package serve

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/telflo-opamp-server/agent"
	"github.com/telflo/telflo-opamp-server/management"
	"github.com/telflo/telflo-opamp-server/tokens"
)

// keyed is envOf with an encryption key and the server's URL set, as serve
// needs.
func keyed(values map[string]string) func(string) string {
	with := map[string]string{EncryptionKeyVariable: testEncryptionKey, agent.ServerURLVariable: testServerURL}
	for name, value := range values {
		with[name] = value
	}
	return envOf(with)
}

func TestLoadSettingsReadsEverythingServeNeeds(t *testing.T) {
	current := strings.Repeat("c", management.MinSecretBytes)
	previous := strings.Repeat("p", management.MinSecretBytes)
	settings, err := LoadSettings(RoleAll, keyed(map[string]string{
		management.PortVariable:            "40001",
		DatabaseURLVariable:                "postgresql://telflo_opamp@db/telflo_opamp",
		management.SecretVariable:          current,
		management.PreviousSecretsVariable: previous,
	}), unsetNothing)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Management.Port != 40001 || settings.DatabaseURL != "postgresql://telflo_opamp@db/telflo_opamp" {
		t.Fatalf("settings = %+v", settings)
	}
	if !settings.Management.Secrets.Accepts(current) || !settings.Management.Secrets.Accepts(previous) || settings.Management.Secrets.Count() != 2 {
		t.Fatal("the settings' secrets aren't the current and the previous one")
	}
}

func TestLoadSettingsRefusesAMissingDatabaseURL(t *testing.T) {
	_, err := LoadSettings(RoleAll, keyed(map[string]string{management.SecretVariable: strings.Repeat("c", management.MinSecretBytes)}), unsetNothing)
	if err == nil || err.Error() != "OPAMP_DATABASE_URL is not set" {
		t.Fatalf("LoadSettings() error = %v", err)
	}
}

// recordingUnset is an unsetenv that records the names it was given.
type recordingUnset struct{ names []string }

func (u *recordingUnset) unsetenv(name string) error {
	u.names = append(u.names, name)
	return nil
}

// The current key is used exactly as written, the previous ones with the
// spaces next to their commas dropped and empty entries ignored, each
// key's file without its final line ending; both value variables are unset
// first, even when the keys are then refused.
func TestLoadEncryptionKeysReadsUnsetsAndRefuses(t *testing.T) {
	current := "a key with inner spaces is used as written, " + testEncryptionKey
	older := strings.Repeat("o", MinEncryptionKeyBytes)
	unset := &recordingUnset{}
	keys, err := LoadEncryptionKeys(envOf(map[string]string{
		EncryptionKeyVariable:          current,
		PreviousEncryptionKeysVariable: " ," + older + " ,,",
	}), unset.unsetenv)
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Destroy()
	if keys.Count() != 2 || !reflect.DeepEqual(unset.names, []string{EncryptionKeyVariable, PreviousEncryptionKeysVariable}) {
		t.Fatalf("%d keys, unset %v", keys.Count(), unset.names)
	}
	bearer, err := keys.Mint(tokens.Claims{TokenID: uuid.New(), FleetID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	exact, err := tokens.NewKeyring(current, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer exact.Destroy()
	if _, err := exact.Read(bearer); err != nil {
		t.Fatal("the current key wasn't used exactly as written")
	}
	trimmed, err := tokens.NewKeyring(older, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer trimmed.Destroy()
	if _, err := keys.Read(mintWith(t, trimmed)); err != nil {
		t.Fatal("the previous key wasn't accepted trimmed")
	}

	dir := t.TempDir()
	file := filepath.Join(dir, "key")
	if err := os.WriteFile(file, []byte(testEncryptionKey+"\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fromFile, err := LoadEncryptionKeys(envOf(map[string]string{EncryptionKeyFileVariable: file}), unsetNothing)
	if err != nil {
		t.Fatal(err)
	}
	defer fromFile.Destroy()
	plain, err := tokens.NewKeyring(testEncryptionKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Destroy()
	if _, err := plain.Read(mintWith(t, fromFile)); err != nil {
		t.Fatal("the key file's final line ending was taken as part of the key")
	}

	refused := &recordingUnset{}
	_, err = LoadEncryptionKeys(envOf(map[string]string{EncryptionKeyVariable: "short-key-marker"}), refused.unsetenv)
	if err == nil || err.Error() != "OPAMP_ENCRYPTION_KEY must be UTF-8 of at least 32 bytes" || len(refused.names) != 2 {
		t.Fatalf("a short key: %v, unset %v", err, refused.names)
	}
	if _, err := LoadEncryptionKeys(envOf(map[string]string{EncryptionKeyVariable: string([]byte{0xff}) + testEncryptionKey}), unsetNothing); err == nil {
		t.Fatal("a key that isn't UTF-8 was accepted")
	}
	failing := func(string) error { return errors.New("no") }
	if _, err := LoadEncryptionKeys(envOf(map[string]string{EncryptionKeyVariable: testEncryptionKey}), failing); err == nil ||
		err.Error() != "OPAMP_ENCRYPTION_KEY couldn't be unset from the environment" {
		t.Fatalf("an unset that fails: %v", err)
	}
}

// An unset that fails doesn't stop the other variable's: both are asked,
// and the refusal names the first that failed.
func TestLoadEncryptionKeysUnsetsEveryKeyVariableEvenWhenOneFails(t *testing.T) {
	for _, failing := range []string{EncryptionKeyVariable, PreviousEncryptionKeysVariable} {
		var asked []string
		unsetenv := func(name string) error {
			asked = append(asked, name)
			if name == failing {
				return errors.New("no")
			}
			return nil
		}
		_, err := LoadEncryptionKeys(envOf(map[string]string{EncryptionKeyVariable: testEncryptionKey}), unsetenv)
		if err == nil || err.Error() != failing+" couldn't be unset from the environment" {
			t.Fatalf("unsetting %s fails: %v", failing, err)
		}
		if !reflect.DeepEqual(asked, []string{EncryptionKeyVariable, PreviousEncryptionKeysVariable}) {
			t.Fatalf("unsetting %s fails: asked %v, want both", failing, asked)
		}
	}
}

// No key, current or previous, may have whitespace at either end (after a
// file's final line ending is dropped), so a key moved from the current
// setting to the previous list derives the same AES key; an all-whitespace
// key is refused with it. The spaces next to the previous list's commas
// are separators, not part of a key. Each refusal names the variable (and
// the entry), never the key.
func TestLoadEncryptionKeysRefusesWhitespaceAtAKeysEnds(t *testing.T) {
	key := "edge-marker-" + testEncryptionKey
	older := "older-marker-" + strings.Repeat("o", MinEncryptionKeyBytes)
	dir := t.TempDir()
	file := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	const edge = "must have no whitespace at either end"
	for _, c := range []struct {
		name     string
		settings map[string]string
		want     string
	}{
		{"a leading space", map[string]string{EncryptionKeyVariable: " " + key}, "OPAMP_ENCRYPTION_KEY " + edge},
		{"a trailing space", map[string]string{EncryptionKeyVariable: key + " "}, "OPAMP_ENCRYPTION_KEY " + edge},
		{"a trailing tab", map[string]string{EncryptionKeyVariable: key + "\t"}, "OPAMP_ENCRYPTION_KEY " + edge},
		{"a leading no-break space", map[string]string{EncryptionKeyVariable: "\u00a0" + key}, "OPAMP_ENCRYPTION_KEY " + edge},
		{"a trailing ideographic space", map[string]string{EncryptionKeyVariable: key + "\u3000"}, "OPAMP_ENCRYPTION_KEY " + edge},
		{"all whitespace", map[string]string{EncryptionKeyVariable: strings.Repeat(" \t", MinEncryptionKeyBytes)}, "OPAMP_ENCRYPTION_KEY " + edge},
		{"a file's key with a trailing space before its line ending",
			map[string]string{EncryptionKeyFileVariable: file("trailing", key+" \n")}, "OPAMP_ENCRYPTION_KEY_FILE " + edge},
		{"a file's key with two line endings", map[string]string{EncryptionKeyFileVariable: file("two", key+"\n\n")},
			"OPAMP_ENCRYPTION_KEY_FILE " + edge},
		{"a file's key with a leading line ending", map[string]string{EncryptionKeyFileVariable: file("leading", "\n"+key)},
			"OPAMP_ENCRYPTION_KEY_FILE " + edge},
		{"the previous list's first key with a leading space",
			map[string]string{EncryptionKeyVariable: key, PreviousEncryptionKeysVariable: " " + older},
			"OPAMP_ENCRYPTION_KEYS_PREVIOUS: entry 1 of 1 " + edge},
		{"the previous list's last key with a trailing space",
			map[string]string{EncryptionKeyVariable: key, PreviousEncryptionKeysVariable: older + "," + older + " "},
			"OPAMP_ENCRYPTION_KEYS_PREVIOUS: entry 2 of 2 " + edge},
		{"a tab next to a comma", map[string]string{EncryptionKeyVariable: key, PreviousEncryptionKeysVariable: older + "\t," + older},
			"OPAMP_ENCRYPTION_KEYS_PREVIOUS: entry 1 of 2 " + edge},
		{"an all-whitespace previous entry", map[string]string{EncryptionKeyVariable: key, PreviousEncryptionKeysVariable: older + ", \t ," + older},
			"OPAMP_ENCRYPTION_KEYS_PREVIOUS: entry 2 of 3 must be UTF-8 of at least 32 bytes"},
		{"a previous file's last key with a trailing space",
			map[string]string{EncryptionKeyVariable: key, PreviousEncryptionKeysFileVariable: file("previous", older+" ,"+older+" \n")},
			"OPAMP_ENCRYPTION_KEYS_PREVIOUS_FILE: entry 2 of 2 " + edge},
	} {
		_, err := LoadEncryptionKeys(envOf(c.settings), unsetNothing)
		if err == nil || err.Error() != c.want {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
			continue
		}
		if strings.Contains(err.Error(), "marker") {
			t.Errorf("%s: the refusal quotes a key: %v", c.name, err)
		}
	}

	// The spaces next to the commas are dropped: the list's keys are the
	// keys as written, so a current key moved into it still reads its
	// bearers.
	keys, err := LoadEncryptionKeys(envOf(map[string]string{
		EncryptionKeyVariable:          testEncryptionKey,
		PreviousEncryptionKeysVariable: older + " ,  " + key + " , ",
	}), unsetNothing)
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Destroy()
	moved, err := tokens.NewKeyring(key, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer moved.Destroy()
	if keys.Count() != 3 {
		t.Fatalf("%d keys, want the current one and two previous", keys.Count())
	}
	if _, err := keys.Read(mintWith(t, moved)); err != nil {
		t.Fatal("a key moved into the previous list doesn't read its bearers")
	}
}

// A key file is at most 64 KiB; a larger one is refused, naming the
// variable and never the content.
func TestLoadEncryptionKeysRefusesAKeyFileOver64KiB(t *testing.T) {
	dir := t.TempDir()
	atLimit := filepath.Join(dir, "at-limit")
	if err := os.WriteFile(atLimit, []byte(strings.Repeat("k", MaxEncryptionKeyFileBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadEncryptionKeys(envOf(map[string]string{EncryptionKeyFileVariable: atLimit}), unsetNothing)
	if err != nil {
		t.Fatalf("a key file of exactly 64 KiB: %v", err)
	}
	keys.Destroy()
	over := filepath.Join(dir, "over")
	if err := os.WriteFile(over, []byte(strings.Repeat("k", MaxEncryptionKeyFileBytes)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ variable, current string }{
		{EncryptionKeyFileVariable, ""},
		{PreviousEncryptionKeysFileVariable, testEncryptionKey},
	} {
		settings := map[string]string{c.variable: over}
		if c.current != "" {
			settings[EncryptionKeyVariable] = c.current
		}
		_, err := LoadEncryptionKeys(envOf(settings), unsetNothing)
		if want := c.variable + " names a file larger than 64 KiB"; err == nil || err.Error() != want {
			t.Errorf("%s over 64 KiB: %v, want %q", c.variable, err, want)
		}
	}
}

func mintWith(t *testing.T, keys *tokens.Keyring) string {
	t.Helper()
	bearer, err := keys.Mint(tokens.Claims{TokenID: uuid.New(), FleetID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	return bearer
}

// OPAMP_SHUTDOWN_DRAIN_SECONDS is 5 unset, a whole number from 0 (no
// drain) to 30; anything else is refused naming the variable.
func TestTheShutdownDrainIsFiveSecondsByDefaultFromZeroToThirty(t *testing.T) {
	load := func(raw string) (Settings, error) {
		return LoadSettings(RoleAll, keyed(map[string]string{
			DatabaseURLVariable: "postgresql://telflo_opamp@db/telflo_opamp", management.SecretVariable: strings.Repeat("c", management.MinSecretBytes),
			ShutdownDrainVariable: raw,
		}), unsetNothing)
	}
	for raw, want := range map[string]time.Duration{"": 5 * time.Second, "0": 0, "1": time.Second, "30": 30 * time.Second} {
		settings, err := load(raw)
		if err != nil || settings.ShutdownDrain != want {
			t.Errorf("%q: %v, %v; want %v", raw, settings.ShutdownDrain, err, want)
		}
	}
	for _, raw := range []string{"31", "-1", "05", "00", "5s", " 5", "1e1"} {
		if _, err := load(raw); err == nil || err.Error() != "OPAMP_SHUTDOWN_DRAIN_SECONDS must be a whole number from 0 to 30" {
			t.Errorf("%q: %v; want the refusal naming the variable", raw, err)
		}
	}
}
