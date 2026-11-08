package management

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"strings"
)

// Secrets are the management API's accepted shared secrets:
// OPAMP_MANAGEMENT_SECRET and every entry of
// OPAMP_MANAGEMENT_SECRETS_PREVIOUS, still accepted during a rotation.
//
// Only each secret's SHA-256 digest is kept, so the text never sits in a
// field a log line or an error could reach, and every comparison is between
// two 32-byte digests, so its time doesn't depend on the secret's length.
//
// Accepts is the only place a presented token meets a secret, and
// subtle.ConstantTimeCompare is the only comparison it uses (a test reads
// this package's source to hold that).
type Secrets struct {
	digests [][sha256.Size]byte
}

// NewSecrets checks and keeps the current secret and the previous ones.
// Surrounding whitespace is trimmed from each (a secret file's trailing
// newline; HTTP trims it from a header value anyway). It refuses a missing
// current secret and any secret shorter than MinSecretBytes, naming the
// variable and never the secret. The caller drops empty previous entries
// first (splitPrevious).
func NewSecrets(current string, previous []string) (*Secrets, error) {
	current = strings.TrimSpace(current)
	if current == "" {
		return nil, fmt.Errorf("%s is not set", SecretVariable)
	}
	if len(current) < MinSecretBytes {
		return nil, fmt.Errorf("%s must be at least %d bytes", SecretVariable, MinSecretBytes)
	}
	s := &Secrets{digests: [][sha256.Size]byte{sha256.Sum256([]byte(current))}}
	for i, secret := range previous {
		secret = strings.TrimSpace(secret)
		if len(secret) < MinSecretBytes {
			return nil, fmt.Errorf("%s: entry %d of %d must be at least %d bytes",
				PreviousSecretsVariable, i+1, len(previous), MinSecretBytes)
		}
		s.digests = append(s.digests, sha256.Sum256([]byte(secret)))
	}
	return s, nil
}

// Accepts reports whether token is one of the secrets. It compares the
// token with every secret, never stopping at a match, so the time it takes
// doesn't say which one matched or whether one did.
func (s *Secrets) Accepts(token string) bool {
	presented := sha256.Sum256([]byte(token))
	matched := 0
	for i := range s.digests {
		matched |= subtle.ConstantTimeCompare(presented[:], s.digests[i][:])
	}
	return matched == 1
}

// Count is how many secrets are accepted (the current one and the previous
// ones); serve logs it at startup.
func (s *Secrets) Count() int {
	return len(s.digests)
}
