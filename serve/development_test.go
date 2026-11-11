package serve

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/telflo/telflo-opamp-server/management"
)

// composeValues are the development management secret and encryption key
// compose.yaml sets, read from it, so the refusal is checked against the
// values the repository actually publishes.
func composeValues(t *testing.T) (secret, key string) {
	t.Helper()
	compose, err := os.ReadFile("../compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	value := func(variable string) string {
		values := map[string]bool{}
		for _, match := range regexp.MustCompile(`(?m)^\s+`+variable+`: (\S+)$`).FindAllStringSubmatch(string(compose), -1) {
			values[match[1]] = true
		}
		if len(values) != 1 {
			t.Fatalf("compose.yaml sets %s to %d values, want one", variable, len(values))
		}
		for v := range values {
			return v
		}
		return ""
	}
	return value(management.SecretVariable), value(EncryptionKeyVariable)
}

// compose.yaml's development secret and key, which everyone holds once the
// repository is public, are refused as the current value or as an entry of
// the previous list, unless OPAMP_ALLOW_DEVELOPMENT_SECRETS is true. Each
// refusal names the setting, never the value.
func TestServeRefusesComposesDevelopmentSecretsUnlessAllowed(t *testing.T) {
	secret, key := composeValues(t)
	ownSecret := strings.Repeat("s", management.MinSecretBytes)
	ownKey := strings.Repeat("k", MinEncryptionKeyBytes)
	allowed := "set OPAMP_ALLOW_DEVELOPMENT_SECRETS=true for local development only"
	for _, c := range []struct {
		name   string
		values map[string]string
		want   string
	}{
		{"the secret", map[string]string{management.SecretVariable: secret},
			"OPAMP_MANAGEMENT_SECRET holds compose.yaml's development value"},
		{"the secret with spaces at its ends", map[string]string{management.SecretVariable: " " + secret + " "},
			"OPAMP_MANAGEMENT_SECRET holds compose.yaml's development value"},
		{"the secret as a previous one", map[string]string{management.PreviousSecretsVariable: ownSecret + ", ," + secret},
			"OPAMP_MANAGEMENT_SECRETS_PREVIOUS: entry 2 of 2 holds compose.yaml's development value"},
		{"the key", map[string]string{EncryptionKeyVariable: key},
			"OPAMP_ENCRYPTION_KEY holds compose.yaml's development value"},
		{"the key as a previous one", map[string]string{PreviousEncryptionKeysVariable: ownKey + "," + key},
			"OPAMP_ENCRYPTION_KEYS_PREVIOUS: entry 2 of 2 holds compose.yaml's development value"},
	} {
		t.Run(c.name, func(t *testing.T) {
			values := map[string]string{DatabaseURLVariable: "postgresql://telflo_opamp@db/telflo_opamp",
				management.SecretVariable: ownSecret, EncryptionKeyVariable: ownKey}
			for name, value := range c.values {
				values[name] = value
			}
			_, err := LoadSettings(RoleAll, keyed(values), unsetNothing)
			if err == nil || !strings.HasPrefix(err.Error(), c.want+", which everyone holds") || !strings.Contains(err.Error(), allowed) {
				t.Fatalf("error = %v, want %q", err, c.want)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), key) {
				t.Fatalf("the refusal quotes the value: %q", err)
			}

			values[AllowDevelopmentSecretsVariable] = "true"
			settings, err := LoadSettings(RoleAll, keyed(values), unsetNothing)
			if err != nil {
				t.Fatalf("allowed: %v", err)
			}
			settings.Keys.Destroy()
		})
	}
}

// OPAMP_ALLOW_DEVELOPMENT_SECRETS is true, false or unset; anything else is
// refused, even when no development value is set.
func TestTheDevelopmentSecretsSwitchIsTrueOrFalse(t *testing.T) {
	for raw, want := range map[string]string{
		"":      "",
		"false": "",
		"true":  "",
		"yes":   "OPAMP_ALLOW_DEVELOPMENT_SECRETS must be true or false",
		"TRUE":  "OPAMP_ALLOW_DEVELOPMENT_SECRETS must be true or false",
		"1":     "OPAMP_ALLOW_DEVELOPMENT_SECRETS must be true or false",
	} {
		settings, err := LoadSettings(RoleAll, keyed(map[string]string{
			DatabaseURLVariable:             "postgresql://telflo_opamp@db/telflo_opamp",
			management.SecretVariable:       strings.Repeat("s", management.MinSecretBytes),
			AllowDevelopmentSecretsVariable: raw,
		}), unsetNothing)
		got := ""
		if err != nil {
			got = err.Error()
		} else {
			settings.Keys.Destroy()
		}
		if got != want {
			t.Errorf("%q: error %q, want %q", raw, got, want)
		}
	}
}

// compose.yaml runs both servers with the switch on, since it holds the
// development values; nothing else in the repository's deployments does.
func TestOnlyComposeTurnsOnTheDevelopmentSecrets(t *testing.T) {
	compose, err := os.ReadFile("../compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(compose), AllowDevelopmentSecretsVariable+`: "true"`); n != 2 {
		t.Fatalf("compose.yaml turns it on %d times, want 2 (the agent and the management API)", n)
	}
	for _, path := range []string{"../charts/telflo-opamp-server/values.yaml", "../charts/telflo-opamp-server/templates/_server.tpl",
		"../deploy/quickstart-collector/kubernetes.yaml", "../.env.example"} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), AllowDevelopmentSecretsVariable+"=true") ||
			strings.Contains(string(content), AllowDevelopmentSecretsVariable+`: "true"`) {
			t.Errorf("%s turns on the development secrets", path)
		}
	}
}
