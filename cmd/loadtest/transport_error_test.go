package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"testing"
)

// timeoutErr is a net.Error whose Timeout() reports true, matching what the HTTP
// stack surfaces on an i/o deadline.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

// classifyTransportError buckets the failure so the metrics can tell a
// generator/OS limit (refused/reset) apart from a slow server (timeout).
func TestClassifyTransportError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"refused", fmt.Errorf("dial tcp: %w", syscall.ECONNREFUSED), "refused"},
		{"reset", fmt.Errorf("read: %w", syscall.ECONNRESET), "reset"},
		{"broken pipe", fmt.Errorf("write: %w", syscall.EPIPE), "reset"},
		{"eof", fmt.Errorf("read: %w", io.EOF), "reset"},
		{"deadline", fmt.Errorf("Do: %w", context.DeadlineExceeded), "timeout"},
		{"os deadline", fmt.Errorf("read: %w", os.ErrDeadlineExceeded), "timeout"},
		{"net timeout", fmt.Errorf("Do: %w", timeoutErr{}), "timeout"},
		{"unknown", errors.New("something else"), "other"},
		{"nil", nil, "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyTransportError(tc.err); got != tc.want {
				t.Errorf("classifyTransportError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
