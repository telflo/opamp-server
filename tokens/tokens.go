// Package tokens makes and reads enrollment tokens' bearers: the credential
// a collector presents to the agent endpoint, minted by the management API
// when a fleet is created, rotated or revealed. It is this server's only
// implementation of the bearer's cryptography: the agent side's
// authentication (middleware.Authenticator) uses it too.
//
// The bearer's format is fixed, so a bearer minted by another
// implementation of it (testdata's, under Node's WebCrypto) decrypts here
// with the same key, and one minted here decrypts there:
//
//	base64std( nonce[12] ‖ AES-256-GCM ciphertext ‖ tag[16] )
//
// with no associated data, the key being the SHA-256 of the setting's UTF-8
// bytes, and the plaintext exactly {"token_id":"…","deployment_id":"…"}:
// the token's id and its fleet's (the key is deployment_id, not fleet_id,
// so outstanding tokens stay valid). The ciphertext is never stored:
// every reveal encrypts afresh, with a fresh nonce.
//
// What is protected, and how far:
//
//   - memguard holds each derived key at rest: one buffer per key, mlock'd,
//     guard-paged and frozen read-only for the life of the process, shared
//     by every request (a buffer per request runs out of locked memory
//     under load), and wiped by Destroy;
//   - each Seal and Open briefly expands the key in Go's heap: crypto/aes
//     builds its key schedule (and GCM its tables) from the locked bytes
//     for that one call, and nothing here keeps it (no AEAD is cached, so
//     the expansion lives only until the garbage collector reclaims the
//     call's cipher). Go offers no way to keep it in locked memory;
//   - the setting a key is derived from is wiped where Go allows:
//     NewKeyringFromBytes and DeriveKeyFromBytes zero the bytes they are
//     given once they are hashed, so a key file's content read into a byte
//     slice doesn't outlive the derivation. A value read from the
//     environment is a Go string, which can't be zeroed: it stays in the
//     heap until it is collected (and in /proc/<pid>/environ, which is why
//     the keys may come from files).
//
// Nothing here logs, and no error quotes a bearer, a key or a plaintext.
package tokens

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/awnumar/memguard"
	"github.com/google/uuid"
)

// nonceBytes is AES-GCM's standard nonce, as WebCrypto's caller chose it.
const nonceBytes = 12

// tagBytes is AES-GCM's tag, appended to the ciphertext by both sides.
const tagBytes = 16

var (
	// ErrInvalidBearer is Open's and Keyring.Read's one answer to a bearer
	// that isn't base64 of a sealed payload under an accepted key, or whose
	// plaintext isn't a token's claims. It says nothing more, and never
	// quotes the bearer.
	ErrInvalidBearer = errors.New("tokens: invalid bearer")
	// ErrNoKey is the answer when no key is configured, or a key's buffer
	// has been destroyed (the keys are purged on exit).
	ErrNoKey = errors.New("tokens: no usable key")
)

// NewKey places 32 bytes of AES-256 key material (a setting's SHA-256) in a
// frozen, locked buffer for the life of the process. material is wiped as
// it is copied in.
func NewKey(material []byte) *memguard.LockedBuffer {
	key := memguard.NewBufferFromBytes(material)
	key.Freeze()
	return key
}

// DeriveKey is the key a setting names: the SHA-256 of its UTF-8 bytes, in
// a frozen, locked buffer (NewKey): the bearer format's derivation, so a
// setting shared with another implementation of it opens its bearers. The
// string itself can't be wiped; DeriveKeyFromBytes wipes its bytes.
func DeriveKey(setting string) *memguard.LockedBuffer {
	return DeriveKeyFromBytes([]byte(setting))
}

// DeriveKeyFromBytes is DeriveKey of a setting's bytes, which it zeroes once
// they are hashed. The digest is wiped as NewKey copies it in.
func DeriveKeyFromBytes(setting []byte) *memguard.LockedBuffer {
	digest := sha256.Sum256(setting)
	memguard.WipeBytes(setting)
	return NewKey(digest[:])
}

// Seal encrypts plaintext under key with a fresh random nonce, as a bearer:
// base64std(nonce ‖ ciphertext ‖ tag).
func Seal(key *memguard.LockedBuffer, plaintext []byte) (string, error) {
	if key == nil || !key.IsAlive() {
		return "", ErrNoKey
	}
	aead, err := newAEAD(key.Bytes())
	if err != nil {
		return "", err
	}
	sealed := make([]byte, nonceBytes, nonceBytes+len(plaintext)+tagBytes)
	if _, err := rand.Read(sealed); err != nil {
		return "", errors.New("tokens: no randomness for a nonce")
	}
	sealed = aead.Seal(sealed, sealed[:nonceBytes], plaintext, nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Open decrypts a bearer with each key in turn, the current one first, and
// returns the plaintext from the first whose tag verifies: AES-GCM's tag
// tells a wrong key apart without a key id. ErrInvalidBearer when none
// does; ErrNoKey when there is no live key to try.
func Open(keys []*memguard.LockedBuffer, bearer string) ([]byte, error) {
	sealed, err := base64.StdEncoding.Strict().DecodeString(bearer)
	if err != nil || len(sealed) < nonceBytes+tagBytes {
		return nil, ErrInvalidBearer
	}
	tried := 0
	for _, key := range keys {
		if key == nil || !key.IsAlive() {
			continue
		}
		tried++
		aead, err := newAEAD(key.Bytes())
		if err != nil {
			return nil, err
		}
		if plaintext, err := aead.Open(nil, sealed[:nonceBytes], sealed[nonceBytes:], nil); err == nil {
			return plaintext, nil
		}
	}
	if tried == 0 {
		return nil, ErrNoKey
	}
	return nil, ErrInvalidBearer
}

// newAEAD is AES-256-GCM under key, made afresh for each Seal and Open and
// never cached (a cached AEAD would keep the expanded key in the
// heap for the life of the process). A key that isn't 32 bytes (a
// destroyed buffer reads as empty) is ErrNoKey, never an AES of another
// size.
func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != sha256.Size {
		return nil, ErrNoKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrNoKey
	}
	aead, err := cipher.NewGCMWithNonceSize(block, nonceBytes)
	if err != nil {
		return nil, ErrNoKey
	}
	return aead, nil
}

// Claims are what a bearer carries: the token's id and its fleet's.
// Authentication reads the rest (the organization, the kind, validity) from
// the token's row, and compares the row's fleet with these.
type Claims struct {
	TokenID uuid.UUID
	FleetID uuid.UUID
}

// wireClaims is the plaintext's JSON, in the bearer format's field names
// and order.
type wireClaims struct {
	TokenID string `json:"token_id"`
	FleetID string `json:"deployment_id"`
}

// canonicalUUID is the only form an id takes in a bearer: 8-4-4-4-12 hex
// digits, as uuid's String and Postgres's uuid write them.
var canonicalUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Marshal is the plaintext of claims: {"token_id":"…","deployment_id":"…"},
// the ids in lower case.
func (c Claims) Marshal() []byte {
	plaintext, err := json.Marshal(wireClaims{TokenID: c.TokenID.String(), FleetID: c.FleetID.String()})
	if err != nil {
		// Two strings always marshal.
		panic("tokens: marshalling claims: " + err.Error())
	}
	return plaintext
}

// ParseClaims reads a plaintext: a JSON object with exactly token_id and
// deployment_id, each a UUID string. Anything else (a missing, empty, null
// or extra field, an id that isn't a UUID) is ErrInvalidBearer.
func ParseClaims(plaintext []byte) (Claims, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(plaintext, &fields); err != nil || len(fields) != 2 {
		return Claims{}, ErrInvalidBearer
	}
	tokenID, ok := uuidField(fields, "token_id")
	if !ok {
		return Claims{}, ErrInvalidBearer
	}
	fleetID, ok := uuidField(fields, "deployment_id")
	if !ok {
		return Claims{}, ErrInvalidBearer
	}
	return Claims{TokenID: tokenID, FleetID: fleetID}, nil
}

func uuidField(fields map[string]json.RawMessage, name string) (uuid.UUID, bool) {
	raw, present := fields[name]
	if !present {
		return uuid.UUID{}, false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || !canonicalUUID.MatchString(value) {
		return uuid.UUID{}, false
	}
	id, err := uuid.Parse(value)
	return id, err == nil
}

// Keyring is the accepted keys: the current one, which mints every bearer,
// and the previous ones, still accepted while collectors move off them (a
// key rotation). Safe for concurrent use: the buffers are only read.
type Keyring struct {
	keys []*memguard.LockedBuffer
}

// NewKeyring derives the current key and each previous one from their
// settings (DeriveKey). The settings' policy (lengths, whitespace, how
// many) is the caller's; an empty setting is refused here too. Strings
// can't be wiped: serve uses NewKeyringFromBytes.
func NewKeyring(current string, previous []string) (*Keyring, error) {
	older := make([][]byte, len(previous))
	for i, setting := range previous {
		older[i] = []byte(setting)
	}
	return NewKeyringFromBytes([]byte(current), older)
}

// NewKeyringFromBytes derives the current key and each previous one from
// their settings' bytes (DeriveKeyFromBytes), and zeroes every one of
// those bytes before it returns, whether or not it refuses them. An empty
// setting is refused.
func NewKeyringFromBytes(current []byte, previous [][]byte) (*Keyring, error) {
	defer func() {
		memguard.WipeBytes(current)
		for _, setting := range previous {
			memguard.WipeBytes(setting)
		}
	}()
	if len(current) == 0 {
		return nil, errors.New("tokens: no current key")
	}
	for _, setting := range previous {
		if len(setting) == 0 {
			return nil, errors.New("tokens: an empty previous key")
		}
	}
	keys := make([]*memguard.LockedBuffer, 0, 1+len(previous))
	keys = append(keys, DeriveKeyFromBytes(current))
	for _, setting := range previous {
		keys = append(keys, DeriveKeyFromBytes(setting))
	}
	return &Keyring{keys: keys}, nil
}

// Mint is a fresh bearer for claims, under the current key.
func (k *Keyring) Mint(claims Claims) (string, error) {
	if k == nil || len(k.keys) == 0 {
		return "", ErrNoKey
	}
	return Seal(k.keys[0], claims.Marshal())
}

// Read opens a bearer with every accepted key and returns its claims, or
// ErrInvalidBearer (or ErrNoKey once the keys are destroyed).
func (k *Keyring) Read(bearer string) (Claims, error) {
	if k == nil {
		return Claims{}, ErrNoKey
	}
	plaintext, err := Open(k.keys, bearer)
	if err != nil {
		return Claims{}, err
	}
	return ParseClaims(plaintext)
}

// Keys are the accepted keys' buffers, the current one first, for the agent
// side's authentication, which opens bearers itself.
func (k *Keyring) Keys() []*memguard.LockedBuffer {
	return append([]*memguard.LockedBuffer(nil), k.keys...)
}

// Count is how many keys are accepted; serve logs it at startup.
func (k *Keyring) Count() int {
	return len(k.keys)
}

// Destroy wipes and unlocks every key's buffer. The keyring mints and reads
// nothing afterwards (ErrNoKey).
func (k *Keyring) Destroy() {
	for _, key := range k.keys {
		key.Destroy()
	}
}
