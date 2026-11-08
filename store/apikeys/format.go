package apikeys

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"

	"github.com/google/uuid"
)

// A key as its holder sends it: "opk_", the key's id as 32 lowercase hex
// digits, "_", and its secret, 32 random bytes in unpadded base64url (43
// characters): 80 characters in all. The prefix lets a secret scanner
// find a leaked key, and the id lets the server and its log lines name a
// key without its secret.
const (
	// Prefix starts every key.
	Prefix = "opk_"
	// secretBytes is the secret part's length before encoding.
	secretBytes = 32
	// Length is every key's length.
	Length = len(Prefix) + 32 + 1 + 43
)

// secretEncoding is unpadded base64url that refuses non-zero trailing bits,
// so each secret has exactly one spelling.
var secretEncoding = base64.RawURLEncoding.Strict()

// Generate makes a new key: its id, the key as its holder sends it, and its
// secret's SHA-256, what the store keeps.
func Generate() (id uuid.UUID, key string, sum [sha256.Size]byte, err error) {
	id, err = uuid.NewRandom()
	if err != nil {
		return uuid.Nil, "", sum, err
	}
	secret := make([]byte, secretBytes)
	if _, err := rand.Read(secret); err != nil {
		return uuid.Nil, "", sum, err
	}
	key = Prefix + hex.EncodeToString(id[:]) + "_" + secretEncoding.EncodeToString(secret)
	return id, key, sha256.Sum256(secret), nil
}

// Parse reads a presented bearer as a key: its id and its secret's SHA-256,
// for Matches. ok is false for anything not spelled exactly as Generate
// spells a key.
func Parse(bearer string) (id uuid.UUID, sum [sha256.Size]byte, ok bool) {
	if len(bearer) != Length {
		return uuid.Nil, sum, false
	}
	rest, found := strings.CutPrefix(bearer, Prefix)
	if !found {
		return uuid.Nil, sum, false
	}
	hexID, encoded, found := strings.Cut(rest, "_")
	if !found || len(hexID) != 32 || strings.ToLower(hexID) != hexID {
		return uuid.Nil, sum, false
	}
	if _, err := hex.Decode(id[:], []byte(hexID)); err != nil {
		return uuid.Nil, sum, false
	}
	secret, err := secretEncoding.DecodeString(encoded)
	if err != nil || len(secret) != secretBytes {
		return uuid.Nil, sum, false
	}
	return id, sha256.Sum256(secret), true
}

// Matches reports whether sum, a presented key's secret's SHA-256 (Parse),
// is the key's, comparing in constant time.
func (a Authenticated) Matches(sum [sha256.Size]byte) bool {
	return subtle.ConstantTimeCompare(a.SecretSHA256[:], sum[:]) == 1
}
