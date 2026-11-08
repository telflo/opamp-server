package middleware

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/enrollmenttokens"
	"github.com/telflo/opamp-server/store/fleets"
	"github.com/telflo/opamp-server/tokens"
)

// activeRow is a valid rotate token's row of a server fleet.
func activeRow(token, organization, fleet uuid.UUID) enrollmenttokens.Authenticated {
	return enrollmenttokens.Authenticated{
		Token:     enrollmenttokens.Token{ID: token, OrganizationID: organization, FleetID: fleet, Kind: enrollmenttokens.KindRotate},
		FleetType: fleets.TypeServer, FleetStatus: fleets.StatusActive,
	}
}

// testStack is an AgentStack over lookup, with maxInFlight slots.
func testStack(k *tokens.Keyring, lookup TokenLookup, maxInFlight int, clock *testClock) AgentStack {
	return AgentStack{
		Concurrency:   NewConcurrencyLimiter(maxInFlight, clock.Now, discard()),
		Authenticator: NewAuthenticator(k, lookup, clock.Now, discard()),
		OrganizationConcurrency: NewOrganizationConcurrency(min(DefaultMaxConcurrentRequestsPerOrganization, maxInFlight),
			clock.Now, discard()),
		Organizations: NewOrganizationLimiter(&limitsStub{rpm: map[uuid.UUID]*int32{}}, DefaultRequestsPerMinute, clock.Now, discard()),
		Now:           clock.Now,
		Logger:        discard(),
	}
}

// countedBody counts the bytes read from a request's body.
type countedBody struct {
	io.ReadCloser
	read *atomic.Int64
}

func (b countedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.read.Add(int64(n))
	return n, err
}

// A hundred unauthenticated uploads that stall (a body promised and never
// sent, with no credential or a forged one; or 4 MiB less a byte sent and
// the last byte withheld) leave every concurrency slot free and none of
// their body read, and a valid poll still answers 200. Were the body read
// inside the slot before the authentication, the hundred would hold all
// 100 slots, the poll would be shed (429), and the server would hold their
// bodies (about 400 MiB).
func TestStalledUnauthenticatedUploadsLeaveAValidPollItsSlot(t *testing.T) {
	k := keyring(t, currentKeySetting)
	token, organization, fleet := uuid.New(), uuid.New(), uuid.New()
	clock := newTestClock()
	stack := testStack(k, staticLookup{row: activeRow(token, organization, fleet)}, DefaultMaxConcurrentRequests, clock)

	var entered, left, unauthenticatedRead atomic.Int64
	inner := stack.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered.Add(1)
		defer left.Add(1)
		if r.Header.Get("X-Test-Valid") == "" {
			r.Body = countedBody{ReadCloser: r.Body, read: &unauthenticatedRead}
		}
		inner.ServeHTTP(w, r)
	}))
	// The agent listener's timeouts.
	srv.Config.ReadHeaderTimeout = 10 * time.Second
	srv.Config.ReadTimeout = 30 * time.Second
	srv.Start()
	t.Cleanup(srv.Close)

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	payload := bytes.Repeat([]byte("y"), MaxBodyBytes-1)
	forged := "Bearer " + mint(t, keyring(t, strangerKeySetting), token, fleet)
	var conns []net.Conn
	var writers sync.WaitGroup
	// Closed before the server (cleanups run last first), so its
	// connections end.
	t.Cleanup(func() {
		for _, c := range conns {
			_ = c.Close()
		}
		writers.Wait()
	})
	const stalled = DefaultMaxConcurrentRequests
	for i := range stalled {
		c, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		switch i % 3 {
		case 0: // no credential, a body promised and never sent
			_, _ = fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\nx", AgentRoute)
		case 1: // a forged credential, likewise
			_, _ = fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: x\r\nAuthorization: %s\r\nContent-Length: 1000\r\n\r\nx", AgentRoute, forged)
		default: // no credential, 4 MiB less a byte, the last withheld
			writers.Add(1)
			go func() {
				defer writers.Done()
				_, _ = fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n", AgentRoute, MaxBodyBytes)
				_, _ = c.Write(payload) // fails once the server closes the connection
			}()
		}
	}

	// Bounded by attempts: every upload has entered the stack, and either
	// every one has left it or every one holds a slot.
	for attempt := 0; ; attempt++ {
		if attempt == 3000 {
			t.Fatalf("the uploads never settled: %d entered, %d left, %+v", entered.Load(), left.Load(), stack.Concurrency.Snapshot())
		}
		if entered.Load() == stalled && (left.Load() == stalled || stack.Concurrency.Snapshot().InFlight == stalled) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if snap := stack.Concurrency.Snapshot(); snap.InFlight != 0 {
		t.Fatalf("%d stalled unauthenticated uploads hold %d of %d slots", stalled, snap.InFlight, snap.MaxInFlight)
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+AgentRoute, strings.NewReader("an AgentToServer"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+mint(t, k, token, fleet))
	req.Header.Set("X-Test-Valid", "1")
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a valid poll beside the stalled uploads answered %d (Retry-After %q)", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if n := unauthenticatedRead.Load(); n != 0 {
		t.Fatalf("the stack read %d bytes of unauthenticated bodies", n)
	}
	if shed := stack.Concurrency.Snapshot().ShedTotal; shed != 0 {
		t.Fatalf("%d requests shed", shed)
	}

	// The heap: what the hundred left behind is their connections, not
	// their bodies (about 400 MiB).
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if grew := int64(after.HeapInuse) - int64(before.HeapInuse); grew > 64<<20 {
		t.Fatalf("the heap in use grew by %d MiB beside %d stalled uploads", grew>>20, stalled)
	}
}

// A Content-Length over the cap answers 413 before a concurrency slot is
// taken or the credential read: with the one slot held by a poll, a 5 MiB
// declared body is 413 (not shed, and no token lookup), while a small
// request is shed.
func TestAnOversizedDeclaredLengthIsRefusedBeforeASlotOrACredential(t *testing.T) {
	k := keyring(t, currentKeySetting)
	token, organization, fleet := uuid.New(), uuid.New(), uuid.New()
	clock := newTestClock()
	lookups := &countingLookup{TokenLookup: staticLookup{row: activeRow(token, organization, fleet)}}
	stack := testStack(k, lookups, 1, clock)
	started, release := make(chan struct{}), make(chan struct{})
	h := stack.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	bearer := "Bearer " + mint(t, k, token, fleet)
	held := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, AgentRoute, strings.NewReader("x"))
		req.Header.Set("Authorization", bearer)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		held <- rec.Code
	}()
	<-started

	oversized := httptest.NewRequest(http.MethodPost, AgentRoute, strings.NewReader("x"))
	oversized.ContentLength = 5 << 20
	oversized.Header.Set("Authorization", bearer)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, oversized)
	if rec.Code != http.StatusRequestEntityTooLarge || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("a declared 5 MiB body with the slot held: %d %v", rec.Code, rec.Header())
	}
	small := httptest.NewRequest(http.MethodPost, AgentRoute, strings.NewReader("x"))
	small.Header.Set("Authorization", bearer)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, small)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a small request with the slot held: %d, want shed", rec.Code)
	}
	close(release)
	if code := <-held; code != http.StatusOK {
		t.Fatalf("the held poll: %d", code)
	}
	if n := lookups.count(); n != 1 {
		t.Fatalf("%d token lookups; want 1 (the held poll's)", n)
	}
}
