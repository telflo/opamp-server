package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// testClock is an injected clock a test moves by hand.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 10, 12, 9, 30, 0, 123456789, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// syncWriter serializes writes to w: handlers log from several goroutines.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// captureLogs is a JSON logger writing into buf, as serve's logs are.
func captureLogs(buf *strings.Builder) *slog.Logger {
	return slog.New(slog.NewJSONHandler(&syncWriter{w: buf}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// logLines decodes each JSON line of buf.
func logLines(t *testing.T, buf *strings.Builder) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, raw := range bytes.Split([]byte(buf.String()), []byte("\n")) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal(raw, &line); err != nil {
			t.Fatalf("a log line isn't JSON: %v", err)
		}
		lines = append(lines, line)
	}
	return lines
}

// discard is a logger writing nowhere.
func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
