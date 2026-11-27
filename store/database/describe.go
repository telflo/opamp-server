package database

import (
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// The categories DescribeError answers. None quotes anything: the driver's
// own text can hold a part of the URL (a password holding `@` puts its tail
// in the host pgx fails to resolve), the role and the database it connected
// as, or a value the database refused.
const (
	describedUnreachable    = "the database's host can't be reached or its name resolved"
	describedTimeout        = "the database didn't answer in time"
	describedTLS            = "the TLS connection to the database failed"
	describedAuthentication = "authentication failed"
	describedConnect        = "the connection to the database failed"
	describedOther          = "the database reported an error"
)

// DescribeError describes an error on the startup and migrate paths without
// quoting the database driver's text: no log line or error message there
// carries it. An error with a driver error anywhere in its chain (pgx's, a
// network error, a TLS error, inside goose's errors too) is described by its
// category alone:
//   - authentication failed (SQLSTATE 28P01 or 28000);
//   - the database didn't answer in time (a deadline or connect_timeout
//     ran out, or the network timed out);
//   - the TLS connection failed;
//   - the host can't be reached or its name resolved;
//   - anything else: that the connection failed, or that the database
//     reported an error, with its SQLSTATE when it has one.
//
// pgx joins the errors of every attempt a connection makes (each address a
// host resolves to; with sslmode=prefer, TLS and then plain), so an answer
// from the database (its SQLSTATE) wins over a TLS failure, and a TLS
// failure over a network one: each says more about where it went wrong.
//
// Only an error with no driver error inside keeps its own text (a refusal of
// this package's, goose's "no migrations", a cancelled context), except a
// deadline that ran out, which is described as a timeout.
func DescribeError(err error) string {
	if err == nil {
		return ""
	}
	if !anyInChain(err, isDriverError) {
		if errors.Is(err, context.DeadlineExceeded) {
			return describedTimeout
		}
		return err.Error()
	}
	var connectErr *pgconn.ConnectError
	connecting := errors.As(err, &connectErr)
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && (pgErr.Code == "28P01" || pgErr.Code == "28000"):
		return describedAuthentication + sqlStateSuffix(pgErr.Code)
	case pgErr != nil && connecting:
		return describedConnect + sqlStateSuffix(pgErr.Code)
	case pgErr != nil:
		return describedOther + sqlStateSuffix(pgErr.Code)
	case errors.Is(err, context.Canceled):
		// Cancelled (serve stopped while it started): the network error a
		// cancelled dial answers says nothing of the host.
	case pgconn.Timeout(err) || errors.Is(err, context.DeadlineExceeded):
		return describedTimeout
	case anyInChain(err, isTLSError):
		return describedTLS
	case anyInChain(err, isNetworkError):
		return describedUnreachable
	}
	if connecting {
		return describedConnect
	}
	return describedOther
}

// unavailable reports whether err says the database can't be reached or
// can't answer yet, which a later attempt may not meet: a network error (no
// route, nothing listening, a name not resolved yet), a timeout, a
// connection that ended mid-way, or a server starting up, shutting down,
// out of connections or reporting a connection exception (SQLSTATE class
// 08). It is false for an answer no retry changes (a failed authentication,
// a missing database, a certificate that doesn't verify, any other
// SQLSTATE) and for a cancelled context (the process is stopping).
func unavailable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "57P01", "57P02", "57P03", "53300":
			// admin_shutdown, crash_shutdown, cannot_connect_now (starting
			// up), too_many_connections.
			return true
		}
		return strings.HasPrefix(pgErr.Code, "08")
	}
	switch {
	case pgconn.Timeout(err) || errors.Is(err, context.DeadlineExceeded):
		return true
	case anyInChain(err, isCertificateOrTLSError):
		return false
	case anyInChain(err, isNetworkError), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return true
	}
	return false
}

// isCertificateOrTLSError reports whether err is the TLS package's own or a
// certificate's: an answer from the server's TLS, not a network failure.
func isCertificateOrTLSError(err error) bool {
	switch packageOf(err) {
	case "crypto/tls", "crypto/x509":
		return true
	}
	return false
}

// sqlStateSuffix is " (SQLSTATE code)", or "" when code isn't a SQLSTATE
// (five digits or upper-case letters).
func sqlStateSuffix(code string) string {
	if len(code) != 5 {
		return ""
	}
	for _, c := range code {
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') {
			return ""
		}
	}
	return " (SQLSTATE " + code + ")"
}

// anyInChain reports whether match holds for err or any error it wraps,
// following both Unwrap() error and Unwrap() []error (errors.Join, goose's
// multierr).
func anyInChain(err error, match func(error) bool) bool {
	if err == nil {
		return false
	}
	if match(err) {
		return true
	}
	switch wrapper := err.(type) {
	case interface{ Unwrap() error }:
		return anyInChain(wrapper.Unwrap(), match)
	case interface{ Unwrap() []error }:
		for _, inner := range wrapper.Unwrap() {
			if anyInChain(inner, match) {
				return true
			}
		}
	}
	return false
}

// driverPackages are the packages whose errors are the driver's: pgx's own
// (many of pgconn's error types are unexported, so they are known by their
// package), the network's and TLS's.
var driverPackages = []string{"net", "crypto/tls", "crypto/x509"}

// isDriverError reports whether err itself, not what it wraps, is an error
// of the database driver, the network or TLS.
func isDriverError(err error) bool {
	pkg := packageOf(err)
	return strings.HasPrefix(pkg, "github.com/jackc/") || slices.Contains(driverPackages, pkg)
}

// isTLSError reports whether err is a TLS or certificate error, or pgconn's
// TLS stage of a connection attempt: an fmt.Errorf whose message starts "tls
// error: " (pgconn's "server refused TLS connection", for one, has no type
// of its own).
func isTLSError(err error) bool {
	switch packageOf(err) {
	case "crypto/tls", "crypto/x509":
		return true
	case "fmt":
		return strings.HasPrefix(err.Error(), "tls error: ")
	}
	return false
}

// isNetworkError reports whether err is a failure to resolve or reach the
// host.
func isNetworkError(err error) bool {
	switch err.(type) {
	case *net.OpError, *net.DNSError, *net.AddrError:
		return true
	}
	return false
}

// packageOf is the import path of err's type (through one pointer), or ""
// for a type with none.
func packageOf(err error) string {
	t := reflect.TypeOf(err)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.PkgPath()
}
