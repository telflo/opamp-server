package serve

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"unicode"
	"unicode/utf8"

	"github.com/awnumar/memguard"

	"github.com/telflo/opamp-server/internal/envconfig"
	"github.com/telflo/opamp-server/tokens"
)

// The enrollment tokens' encryption keys, which both components hold: the
// management API mints every bearer, and the agent endpoint reads the
// bearers collectors present. The current key, which mints every bearer,
// is a value or a file, exactly one of the two; the previous keys, still
// accepted for reading bearers during a key rotation, are a value or a
// file, at most one of the two. serve unsets both value variables from its
// environment once it has read them.
const (
	EncryptionKeyVariable              = "OPAMP_ENCRYPTION_KEY"
	EncryptionKeyFileVariable          = "OPAMP_ENCRYPTION_KEY_FILE"
	PreviousEncryptionKeysVariable     = "OPAMP_ENCRYPTION_KEYS_PREVIOUS"
	PreviousEncryptionKeysFileVariable = "OPAMP_ENCRYPTION_KEYS_PREVIOUS_FILE"

	// MinEncryptionKeyBytes is the shortest key `serve` accepts.
	MinEncryptionKeyBytes = 32
	// MaxPreviousEncryptionKeys is the most previous keys `serve` accepts:
	// each is a locked buffer, and a container's 64 KiB of locked memory
	// holds 16.
	MaxPreviousEncryptionKeys = 8
)

// MaxEncryptionKeyFileBytes is the largest key file serve reads: a key file
// is one line of text, and a larger one is a wrong path.
const MaxEncryptionKeyFileBytes = 64 << 10

// LoadEncryptionKeys reads the enrollment tokens' keys, and first unsets
// OPAMP_ENCRYPTION_KEY and OPAMP_ENCRYPTION_KEYS_PREVIOUS from the
// process's environment, both of them even when unsetting one fails, so a
// key given as a value doesn't stay where a child process or a crash report
// could read it (it may still be in /proc/<pid>/environ, which is why the
// keys may come from files).
//
//   - The current key: OPAMP_ENCRYPTION_KEY, or OPAMP_ENCRYPTION_KEY_FILE
//     (the file's content without its final line ending); exactly one. Its
//     SHA-256 is the key.
//   - The previous keys: OPAMP_ENCRYPTION_KEYS_PREVIOUS, or
//     OPAMP_ENCRYPTION_KEYS_PREVIOUS_FILE (likewise); at most one. Comma
//     separated; the spaces (U+0020) next to each comma are dropped, and
//     empty entries ignored; at most MaxPreviousEncryptionKeys.
//
// Every key is UTF-8 of at least MinEncryptionKeyBytes bytes, with no
// whitespace at either end (as Unicode counts it): a key is used exactly as
// written, so a key moved from the current setting to the previous list
// derives the same AES key there, and an all-whitespace key is refused.
// compose.yaml's development key is refused too, as either setting, unless
// OPAMP_ALLOW_DEVELOPMENT_SECRETS is true. A key file is at most
// MaxEncryptionKeyFileBytes. The bytes read are zeroed
// once the keys are derived (tokens.NewKeyringFromBytes); the environment's
// strings can't be. Errors name the variable, never a key.
func LoadEncryptionKeys(getenv func(string) string, unsetenv func(string) error) (*tokens.Keyring, error) {
	primaryValue, primaryFile := getenv(EncryptionKeyVariable), getenv(EncryptionKeyFileVariable)
	olderValue, olderFile := getenv(PreviousEncryptionKeysVariable), getenv(PreviousEncryptionKeysFileVariable)
	notUnset := ""
	for _, variable := range []string{EncryptionKeyVariable, PreviousEncryptionKeysVariable} {
		if err := unsetenv(variable); err != nil && notUnset == "" {
			notUnset = variable
		}
	}
	if notUnset != "" {
		return nil, fmt.Errorf("%s couldn't be unset from the environment", notUnset)
	}
	allowDevelopment, err := allowDevelopmentSecrets(getenv)
	if err != nil {
		return nil, err
	}

	primaryRead, primary, variable, err := keySetting(EncryptionKeyVariable, primaryValue, EncryptionKeyFileVariable, primaryFile)
	defer memguard.WipeBytes(primaryRead)
	if err != nil {
		return nil, err
	}
	if variable == "" {
		return nil, fmt.Errorf("%s or %s must be set", EncryptionKeyVariable, EncryptionKeyFileVariable)
	}
	if problem := keyProblem(primary); problem != "" {
		return nil, fmt.Errorf("%s must %s", variable, problem)
	}
	if !allowDevelopment && isDevelopmentSecret(primary) {
		return nil, developmentSecretError(variable)
	}

	olderRead, rawOlder, variable, err := keySetting(PreviousEncryptionKeysVariable, olderValue, PreviousEncryptionKeysFileVariable, olderFile)
	defer memguard.WipeBytes(olderRead)
	if err != nil {
		return nil, err
	}
	older := splitPreviousKeys(rawOlder)
	if len(older) > MaxPreviousEncryptionKeys {
		return nil, fmt.Errorf("%s holds %d keys; at most %d are accepted", variable, len(older), MaxPreviousEncryptionKeys)
	}
	for i, entry := range older {
		if problem := keyProblem(entry); problem != "" {
			return nil, fmt.Errorf("%s: entry %d of %d must %s", variable, i+1, len(older), problem)
		}
		if !allowDevelopment && isDevelopmentSecret(entry) {
			return nil, developmentSecretError(fmt.Sprintf("%s: entry %d of %d", variable, i+1, len(older)))
		}
	}
	return tokens.NewKeyringFromBytes(primary, older)
}

// keyProblem is the rule a key breaks, as an error's end ("be UTF-8 of at
// least 32 bytes"), or "" for a key serve accepts.
func keyProblem(key []byte) string {
	if !utf8.Valid(key) || len(key) < MinEncryptionKeyBytes {
		return fmt.Sprintf("be UTF-8 of at least %d bytes", MinEncryptionKeyBytes)
	}
	first, _ := utf8.DecodeRune(key)
	last, _ := utf8.DecodeLastRune(key)
	if unicode.IsSpace(first) || unicode.IsSpace(last) {
		return "have no whitespace at either end"
	}
	return ""
}

// splitPreviousKeys splits the previous keys' setting on commas, drops the
// spaces (U+0020) next to each comma, and leaves out the entries that are
// then empty. Each entry is a slice of raw (no copy), so wiping raw wipes
// them. Whitespace at the setting's own ends, or any other whitespace next
// to a comma, stays for keyProblem to refuse.
func splitPreviousKeys(raw []byte) [][]byte {
	parts := bytes.Split(raw, []byte(","))
	var entries [][]byte
	for i, part := range parts {
		if i > 0 {
			part = bytes.TrimLeft(part, " ")
		}
		if i < len(parts)-1 {
			part = bytes.TrimRight(part, " ")
		}
		if len(part) > 0 {
			entries = append(entries, part)
		}
	}
	return entries
}

// keySetting is a key setting's bytes: a copy of the value variable's, or
// the content of the file the file variable names (keyFile); at most one of
// the two may be set. It answers what was read, for the caller to wipe, and
// the setting itself, a slice of it without a file's final line ending; and
// the variable that was set ("" for neither), for an error to name.
func keySetting(valueVariable, value, fileVariable, file string) (read, setting []byte, variable string, err error) {
	switch {
	case value != "" && file != "":
		return nil, nil, "", fmt.Errorf("%s and %s are both set; set one", valueVariable, fileVariable)
	case value != "":
		read = []byte(value)
		return read, read, valueVariable, nil
	case file == "":
		return nil, nil, "", nil
	}
	read, err = readKeyFile(fileVariable, file)
	if err != nil {
		return nil, nil, "", err
	}
	setting = bytes.TrimSuffix(bytes.TrimSuffix(read, []byte("\n")), []byte("\r"))
	return read, setting, fileVariable, nil
}

// readKeyFile reads a key file of at most MaxEncryptionKeyFileBytes into one
// buffer (no copy left behind by a growing read), refusing a larger one,
// whose bytes it wipes. Its errors name the variable, never the content.
func readKeyFile(variable, path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, envconfig.FileError(variable, err)
	}
	defer func() { _ = f.Close() }()
	buffer := make([]byte, MaxEncryptionKeyFileBytes+1)
	n, err := io.ReadFull(f, buffer)
	switch {
	case n > MaxEncryptionKeyFileBytes:
		memguard.WipeBytes(buffer)
		return nil, fmt.Errorf("%s names a file larger than %d KiB", variable, MaxEncryptionKeyFileBytes>>10)
	case err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF):
		memguard.WipeBytes(buffer)
		return nil, fmt.Errorf("%s names a file that can't be read", variable)
	}
	return buffer[:n], nil
}
