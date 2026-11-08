package management

import (
	"strings"
	"testing"
)

func TestNewSecretsRefusesMissingAndShortSecrets(t *testing.T) {
	ok := strings.Repeat("k", MinSecretBytes)
	short := strings.Repeat("s", MinSecretBytes-1)
	cases := []struct {
		name     string
		current  string
		previous []string
		want     string
	}{
		{"missing", "", nil, "OPAMP_MANAGEMENT_SECRET is not set"},
		{"whitespace only", " \t\n", nil, "OPAMP_MANAGEMENT_SECRET is not set"},
		{"31 bytes", short, nil, "OPAMP_MANAGEMENT_SECRET must be at least 32 bytes"},
		{"31 bytes once trimmed", short + "\n", nil, "OPAMP_MANAGEMENT_SECRET must be at least 32 bytes"},
		{"a short previous one", ok, []string{ok, short}, "OPAMP_MANAGEMENT_SECRETS_PREVIOUS: entry 2 of 2 must be at least 32 bytes"},
	}
	for _, c := range cases {
		_, err := NewSecrets(c.current, c.previous)
		if err == nil || err.Error() != c.want {
			t.Errorf("%s: error = %v, want %q", c.name, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), "sss") {
			t.Errorf("%s: the error quotes the secret: %v", c.name, err)
		}
	}
	if _, err := NewSecrets(ok, nil); err != nil {
		t.Fatalf("a 32-byte secret was refused: %v", err)
	}
}

// A rotation: the new secret is current, the old one previous; both are
// accepted until the old one is dropped.
func TestRotationAcceptsTheCurrentAndEveryPreviousSecret(t *testing.T) {
	current := "the-current-secret-0123456789abcdef"
	older := "an-older-secret-0123456789abcdefgh"
	oldest := "the-oldest-secret-0123456789abcdef"
	dropped := "a-dropped-secret-0123456789abcdefg"

	secrets, err := NewSecrets(current, []string{older, oldest})
	if err != nil {
		t.Fatal(err)
	}
	for _, accepted := range []string{current, older, oldest} {
		if !secrets.Accepts(accepted) {
			t.Errorf("%q isn't accepted", accepted)
		}
	}
	for _, refused := range []string{dropped, "", current[:len(current)-1], current + "x", strings.ToUpper(current)} {
		if secrets.Accepts(refused) {
			t.Errorf("%q is accepted", refused)
		}
	}
}

func TestASecretIsTrimmedAsAHeaderWouldBe(t *testing.T) {
	secret := "a-secret-from-a-file-0123456789abcdef"
	secrets, err := NewSecrets(secret+"\n", []string{" " + secret + "-old "})
	if err != nil {
		t.Fatal(err)
	}
	if !secrets.Accepts(secret) || !secrets.Accepts(secret+"-old") {
		t.Fatal("trimmed secrets aren't accepted")
	}
}
