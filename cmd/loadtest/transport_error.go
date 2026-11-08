package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"syscall"
)

// Transport-error buckets. A request that never produced an HTTP response fails
// for one of these reasons; splitting them tells a generator/OS limit apart from
// a genuinely slow server:
//
//	refused — the connection was refused (accept backlog overflow, listener down)
//	reset   — the connection dropped mid-flight (reset/EOF/broken pipe; keep-alive
//	          race or the server closing connections under load)
//	timeout — no response within the client deadline (the server is too slow)
//	other   — anything else, including request-build/marshal failures
const (
	errKindRefused = "refused"
	errKindReset   = "reset"
	errKindTimeout = "timeout"
	errKindOther   = "other"
)

// classifyTransportError buckets a transport failure by cause. errors.Is/As
// unwrap the *url.Error the HTTP client wraps around the underlying syscall or
// deadline error. Timeout is checked first because a deadline can surface as a
// net.Error rather than a bare syscall.
func classifyTransportError(err error) string {
	if err == nil {
		return errKindOther
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errKindTimeout
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return errKindTimeout
	case errors.Is(err, syscall.ECONNREFUSED):
		return errKindRefused
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE), errors.Is(err, io.EOF):
		return errKindReset
	default:
		return errKindOther
	}
}
