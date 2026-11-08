package serve

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/telflo/opamp-server/management"
)

// AllowDevelopmentSecretsVariable lets serve start on the development
// stack's management secret and encryption key (compose.yaml), which are
// committed to the repository and so held by everyone: "true" allows them,
// and only compose.yaml sets it. Unset or "false", serve refuses them, so a
// copy of that stack moved onto a network, or its values reused in a
// deployment, never starts with credentials anyone can read.
const AllowDevelopmentSecretsVariable = "OPAMP_ALLOW_DEVELOPMENT_SECRETS"

// developmentDigests are the SHA-256 digests of compose.yaml's
// OPAMP_MANAGEMENT_SECRET and OPAMP_ENCRYPTION_KEY: kept as digests, so the
// binary never carries the values.
var developmentDigests = [][sha256.Size]byte{
	mustDigest("04d9110ee3ee9b5cf1e1c09b8f3ab6fc496656acec5273f14dc7adf964fc0be0"),
	mustDigest("8840912ac9df7e08e9c22b085df53aae4f6757de04185f8f1b730e9368f98560"),
}

// mustDigest is a SHA-256 digest written in hex.
func mustDigest(hexDigest string) [sha256.Size]byte {
	var digest [sha256.Size]byte
	if n, err := hex.Decode(digest[:], []byte(hexDigest)); err != nil || n != sha256.Size {
		panic("serve: a development digest isn't 32 bytes of hex")
	}
	return digest
}

// allowDevelopmentSecrets reads OPAMP_ALLOW_DEVELOPMENT_SECRETS: true for
// "true", false when unset or "false", and refused otherwise.
func allowDevelopmentSecrets(getenv func(string) string) (bool, error) {
	switch getenv(AllowDevelopmentSecretsVariable) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	}
	return false, fmt.Errorf("%s must be true or false", AllowDevelopmentSecretsVariable)
}

// isDevelopmentSecret reports whether value is one of the development
// stack's committed credentials.
func isDevelopmentSecret(value []byte) bool {
	digest := sha256.Sum256(value)
	for _, development := range developmentDigests {
		if digest == development {
			return true
		}
	}
	return false
}

// developmentSecretError is the refusal of a development credential found
// in setting (a variable, or an entry of one), never quoting it.
func developmentSecretError(setting string) error {
	return fmt.Errorf("%s holds compose.yaml's development value, which everyone holds: generate your own "+
		"(openssl rand -base64 48), or set %s=true for local development only", setting, AllowDevelopmentSecretsVariable)
}

// refuseDevelopmentManagementSecrets refuses compose.yaml's development
// secret as OPAMP_MANAGEMENT_SECRET or as an entry of
// OPAMP_MANAGEMENT_SECRETS_PREVIOUS, each read as management.NewSecrets
// reads it (spaces at its ends dropped, empty entries ignored), unless
// OPAMP_ALLOW_DEVELOPMENT_SECRETS is true.
func refuseDevelopmentManagementSecrets(getenv func(string) string) error {
	allowed, err := allowDevelopmentSecrets(getenv)
	if err != nil || allowed {
		return err
	}
	if isDevelopmentSecret([]byte(strings.TrimSpace(getenv(management.SecretVariable)))) {
		return developmentSecretError(management.SecretVariable)
	}
	var previous []string
	for _, entry := range strings.Split(getenv(management.PreviousSecretsVariable), ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			previous = append(previous, entry)
		}
	}
	for i, entry := range previous {
		if isDevelopmentSecret([]byte(entry)) {
			return developmentSecretError(fmt.Sprintf("%s: entry %d of %d", management.PreviousSecretsVariable, i+1, len(previous)))
		}
	}
	return nil
}
