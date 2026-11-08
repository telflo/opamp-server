package main

import (
	"fmt"
	"os"
	"strings"
)

// loadTokens resolves the fleet tokens the run will use. Exactly one of
// inline (comma-separated --tokens) or filePath (--tokens-file, one per line)
// must be non-empty. Blank entries and lines beginning with '#' are skipped so
// a tokens file can carry comments.
func loadTokens(inline, filePath string) ([]string, error) {
	if inline != "" && filePath != "" {
		return nil, fmt.Errorf("pass only one of --tokens or --tokens-file")
	}

	var raw []string
	switch {
	case inline != "":
		raw = strings.Split(inline, ",")
	case filePath != "":
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("reading tokens file: %w", err)
		}
		raw = strings.Split(string(data), "\n")
	default:
		return nil, fmt.Errorf("provide --tokens or --tokens-file")
	}

	tokens := make([]string, 0, len(raw))
	for _, t := range raw {
		t = strings.TrimSpace(t)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		tokens = append(tokens, normalizeToken(t))
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("no tokens found")
	}
	return tokens, nil
}

// normalizeToken returns the full Authorization header value. The server
// expects "Bearer <ciphertext>"; operators may paste the value with or without
// the prefix (the download bundle bakes it in, but the view-token UI shows the
// bare ciphertext), so we accept either and emit the canonical form.
func normalizeToken(t string) string {
	t = strings.TrimSpace(t)
	if strings.HasPrefix(strings.ToLower(t), "bearer ") {
		// Re-normalize casing/spacing of the prefix while preserving the blob.
		return "Bearer " + strings.TrimSpace(t[len("bearer "):])
	}
	return "Bearer " + t
}

// tokenFor assigns virtual supervisor index i a token round-robin across the
// list, so N clients spread evenly over the available fleets/orgs.
func tokenFor(tokens []string, i int) string {
	return tokens[i%len(tokens)]
}
