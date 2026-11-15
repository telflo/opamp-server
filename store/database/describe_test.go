package database_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"

	"github.com/telflo/opamp-server/internal/testcert"
	"github.com/telflo/opamp-server/store/database"
	"github.com/telflo/opamp-server/store/database/databasetest"
)

// canary is a distinctive random string to search an error's text for.
func canary(t *testing.T) string {
	t.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "canary" + hex.EncodeToString(b)
}

// connectError is the error pgx answers for url, which must fail.
func connectError(t *testing.T, connString string) error {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), connString)
	if err == nil {
		_ = conn.Close(context.Background())
		t.Fatal("the connection succeeded; the test needs it to fail")
	}
	return err
}

// serverURL is the test server's superuser URL with its user, password,
// database and query replaced.
func serverURL(t *testing.T, user, password, databaseName, query string) string {
	t.Helper()
	u, err := url.Parse(databasetest.AdminURL(t))
	if err != nil {
		t.Fatal("the admin URL doesn't parse")
	}
	u.User = url.UserPassword(user, password)
	u.Path = "/" + databaseName
	u.RawQuery = query
	return u.String()
}

// assertDescribed checks that err is described as want and that the
// description holds none of the secrets (by boolean: a failure never
// prints them).
func assertDescribed(t *testing.T, err error, want string, secrets ...string) {
	t.Helper()
	got := database.DescribeError(err)
	for _, secret := range secrets {
		if strings.Contains(got, secret) {
			t.Fatal("the description quotes a part of the error's text")
		}
	}
	if got != want {
		t.Fatalf("DescribeError() = %q, want %q", got, want)
	}
}

func TestDescribeErrorKeepsTheTextOfAnErrorWithNoDriverErrorInside(t *testing.T) {
	assertDescribed(t, nil, "")
	assertDescribed(t, errors.New("refusing to migrate: a reason"), "refusing to migrate: a reason")
	assertDescribed(t, fmt.Errorf("version 3: %w", goose.ErrNotApplied), "version 3: migration not applied")
	assertDescribed(t, fmt.Errorf("waiting: %w", context.Canceled), "waiting: context canceled")
}

func TestDescribeErrorNamesAFailedAuthentication(t *testing.T) {
	user, password := canary(t), canary(t)
	err := connectError(t, serverURL(t, user, password, "postgres", ""))
	assertDescribed(t, err, "authentication failed (SQLSTATE 28P01)", user, password)

	message := canary(t)
	assertDescribed(t, fmt.Errorf("wrapped: %w", &pgconn.PgError{Severity: "FATAL", Code: "28000", Message: message}),
		"authentication failed (SQLSTATE 28000)", message)
}

func TestDescribeErrorNamesAHostThatCantBeReachedOrResolved(t *testing.T) {
	user, password := canary(t), canary(t)
	// Port 1 on the loopback: nothing listens there.
	err := connectError(t, "postgresql://"+user+":"+password+"@127.0.0.1:1/postgres?connect_timeout=5")
	assertDescribed(t, err, "the database's host can't be reached or its name resolved", user, password)

	host := canary(t)
	assertDescribed(t, fmt.Errorf("hostname resolving error: %w", &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}),
		"the database's host can't be reached or its name resolved", host)
}

func TestDescribeErrorNamesAFailedTLSConnection(t *testing.T) {
	// verify-full against a root that signed nothing the server holds: the
	// server either refuses TLS (pgconn's "tls error: server refused TLS
	// connection") or presents a certificate the root doesn't verify.
	files := testcert.New(t)
	user, password := canary(t), canary(t)
	err := connectError(t, serverURL(t, user, password, "postgres",
		"sslmode=verify-full&sslrootcert="+url.QueryEscape(files.CertFile)))
	assertDescribed(t, err, "the TLS connection to the database failed", user, password)

	message := canary(t)
	assertDescribed(t, fmt.Errorf("wrapped: %w", tls.RecordHeaderError{Msg: message}),
		"the TLS connection to the database failed", message)
}

func TestDescribeErrorGivesAnyOtherDatabaseErrorItsSQLStateOnly(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	value := canary(t)
	// 22P02, whose message quotes the value refused.
	_, err := d.Runtime.Exec(context.Background(), "select $1::text::int", value)
	assertDescribed(t, err, "the database reported an error (SQLSTATE 22P02)", value)

	// A connection the server ends with an error of its own: here, a
	// database that doesn't exist (3D000), which the message names.
	missing := "telflo_opamp_server_" + canary(t)
	err = connectError(t, strings.Replace(d.RuntimeURL, "/"+testDatabase, "/"+missing, 1))
	assertDescribed(t, err, "the connection to the database failed (SQLSTATE 3D000)", missing, databasetest.RuntimePassword)
}

func TestDescribeErrorGivesADriverErrorWithoutASQLStateNoText(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn, err := pgx.Connect(ctx, d.RuntimeURL)
	if err == nil {
		_ = conn.Close(context.Background())
		t.Fatal("a connection with a cancelled context succeeded")
	}
	assertDescribed(t, err, "the connection to the database failed", database.ServerRole, testDatabase, databasetest.RuntimePassword)
}

// The driver's error is found anywhere in the chain: inside goose's errors
// (a failed migration wraps it in its own text, which quotes the statement)
// and inside a joined error.
func TestDescribeErrorFindsTheDriversErrorInsideGoosesErrors(t *testing.T) {
	value, statement := canary(t), canary(t)
	pgErr := &pgconn.PgError{Severity: "ERROR", Code: "23505", Message: "duplicate key", Detail: "Key (name)=(" + value + ") already exists."}
	failed := fmt.Errorf("failed to execute SQL query %q: %w", statement, pgErr)
	partial := &goose.PartialError{
		Failed: &goose.MigrationResult{Source: &goose.Source{Type: goose.TypeSQL, Version: 3}, Error: failed},
		Err:    failed,
	}
	want := "the database reported an error (SQLSTATE 23505)"
	assertDescribed(t, partial, want, value, statement)
	assertDescribed(t, fmt.Errorf("failed to initialize: %w", errors.Join(errors.New("first"), failed)), want, value, statement)
}
