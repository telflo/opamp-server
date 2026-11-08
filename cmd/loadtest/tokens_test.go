package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeToken(t *testing.T) {
	cases := map[string]string{
		"abc":         "Bearer abc",
		"Bearer abc":  "Bearer abc",
		"bearer abc":  "Bearer abc",
		"BEARER  abc": "Bearer abc",
		"  abc  ":     "Bearer abc",
	}
	for in, want := range cases {
		if got := normalizeToken(in); got != want {
			t.Errorf("normalizeToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadTokens_Inline(t *testing.T) {
	got, err := loadTokens("a, b ,,c", "")
	if err != nil {
		t.Fatalf("loadTokens: %v", err)
	}
	want := []string{"Bearer a", "Bearer b", "Bearer c"}
	assertTokens(t, got, want)
}

func TestLoadTokens_File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.txt")
	content := "# a comment\nBearer one\n\ntwo\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := loadTokens("", path)
	if err != nil {
		t.Fatalf("loadTokens: %v", err)
	}
	assertTokens(t, got, []string{"Bearer one", "Bearer two"})
}

func TestLoadTokens_Errors(t *testing.T) {
	if _, err := loadTokens("a", "some-file"); err == nil {
		t.Error("expected error when both --tokens and --tokens-file set")
	}
	if _, err := loadTokens("", ""); err == nil {
		t.Error("expected error when neither source set")
	}
	if _, err := loadTokens("  ,  ", ""); err == nil {
		t.Error("expected error when all tokens blank")
	}
}

func TestTokenFor_RoundRobin(t *testing.T) {
	tokens := []string{"A", "B", "C"}
	want := []string{"A", "B", "C", "A", "B"}
	for i, w := range want {
		if got := tokenFor(tokens, i); got != w {
			t.Errorf("tokenFor(%d) = %q, want %q", i, got, w)
		}
	}
}

func assertTokens(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v (%d), want %v (%d)", got, len(got), want, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("token[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
