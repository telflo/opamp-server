package database_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/telflo/opamp-server/store/database"
)

// silentServer listens on the loopback and takes every connection without
// ever answering: a database whose process is frozen, as a cold start can
// leave one. It returns its address; the connections close when the test
// ends.
func silentServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})
	return listener.Addr().String()
}

// openSilently runs OpenMigrator against a server that never answers and
// checks that it gives up, with an error marked ErrUnavailable that says
// the database didn't answer in time. The bounds under test are a fraction
// of a second; a minute without an answer is a hang, not a slow machine.
func openSilently(t *testing.T, query string) {
	t.Helper()
	url := "postgresql://nobody:not-a-real-password@" + silentServer(t) + "/telflo_opamp_server" + query
	done := make(chan error, 1)
	go func() {
		migrator, err := database.OpenMigrator(context.Background(), url)
		if err == nil {
			_ = migrator.Close()
		}
		done <- err
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(time.Minute):
		t.Fatal("OpenMigrator hadn't given up on a server that never answers after a minute")
	}
	if err == nil {
		t.Fatal("OpenMigrator succeeded against a server that never answers")
	}
	if !errors.Is(err, database.ErrUnavailable) {
		t.Errorf("the error isn't marked ErrUnavailable: %v", err)
	}
	if want := "can't connect to the database: the database didn't answer in time"; err.Error() != want {
		t.Errorf("the error is %q, want %q", err.Error(), want)
	}
}

// A URL with no connect_timeout gets OpenMigrator's own: the handshake
// with a server that takes the connection but never answers ends.
func TestOpenMigratorGivesUpOnAServerThatNeverAnswers(t *testing.T) {
	database.ShortenOpenTimeouts(t, 300*time.Millisecond, time.Minute)
	openSilently(t, "")
}

// A URL's own connect_timeout is kept, and the deadline on the open bounds
// it however long it is.
func TestOpenMigratorsDeadlineBoundsALongConnectTimeout(t *testing.T) {
	database.ShortenOpenTimeouts(t, time.Minute, 300*time.Millisecond)
	openSilently(t, "?connect_timeout=600")
}

func TestUnavailableIsTrueOnlyForWhatAnotherAttemptMayNotMeet(t *testing.T) {
	refused := connectError(t, "postgresql://nobody:not-a-real-password@127.0.0.1:1/postgres?connect_timeout=5")
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"nothing listening (a real connection)", refused, true},
		{"connection refused", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, true},
		{"a name not resolved", fmt.Errorf("hostname resolving error: %w", &net.DNSError{Err: "no such host", IsNotFound: true}), true},
		{"a deadline", fmt.Errorf("ping: %w", context.DeadlineExceeded), true},
		{"a connection ended mid-way", fmt.Errorf("receive message: %w", io.ErrUnexpectedEOF), true},
		{"starting up (57P03)", &pgconn.PgError{Code: "57P03"}, true},
		{"shutting down (57P01)", &pgconn.PgError{Code: "57P01"}, true},
		{"too many connections (53300)", &pgconn.PgError{Code: "53300"}, true},
		{"a connection exception (08006)", &pgconn.PgError{Code: "08006"}, true},
		{"failed authentication (28P01)", &pgconn.PgError{Code: "28P01"}, false},
		{"no such role (28000)", &pgconn.PgError{Code: "28000"}, false},
		{"no such database (3D000)", &pgconn.PgError{Code: "3D000"}, false},
		{"permission denied (42501)", &pgconn.PgError{Code: "42501"}, false},
		{"a TLS failure", fmt.Errorf("tls error: %w", tls.RecordHeaderError{Msg: "bad"}), false},
		{"a certificate that doesn't verify", fmt.Errorf("tls error: %w", x509.UnknownAuthorityError{}), false},
		{"a cancelled context", fmt.Errorf("dial: %w", errors.Join(context.Canceled, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED})), false},
		{"an error of this package's", errors.New("refusing to migrate"), false},
		{"none", nil, false},
	} {
		if got := database.Unavailable(c.err); got != c.want {
			t.Errorf("%s: unavailable = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDescribeErrorNamesATimeout(t *testing.T) {
	user, password := canary(t), canary(t)
	err := connectError(t, "postgresql://"+user+":"+password+"@"+silentServer(t)+"/postgres?connect_timeout=1")
	assertDescribed(t, err, "the database didn't answer in time", user, password)

	// database/sql answers a ping whose deadline ran out before it reached
	// the driver with the bare context error.
	assertDescribed(t, fmt.Errorf("ping: %w", context.DeadlineExceeded), "the database didn't answer in time")
	if got := database.DescribeError(context.Canceled); !strings.Contains(got, "canceled") {
		t.Errorf("DescribeError(context.Canceled) = %q, want its own text", got)
	}
}
