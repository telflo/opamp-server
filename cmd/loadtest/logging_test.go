package main

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

// captureLog redirects the standard logger to a buffer for the duration of the
// test and restores it (and the verbose flag) afterwards.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		verbose = false
	})
	return &buf
}

// logInfo is non-error output: silent unless --verbose is set.
func TestLogInfo_GatedByVerbose(t *testing.T) {
	buf := captureLog(t)

	verbose = false
	logInfo("quiet %d", 1)
	if buf.Len() != 0 {
		t.Errorf("logInfo printed while not verbose: %q", buf.String())
	}

	verbose = true
	logInfo("loud %d", 2)
	if !strings.Contains(buf.String(), "loud 2") {
		t.Errorf("logInfo did not print while verbose: %q", buf.String())
	}
}

// logError always prints — errors must never be swallowed by the verbose gate.
func TestLogError_AlwaysPrints(t *testing.T) {
	buf := captureLog(t)

	verbose = false
	logError("boom %d", 1)
	if !strings.Contains(buf.String(), "boom 1") {
		t.Errorf("logError did not print while not verbose: %q", buf.String())
	}
}
