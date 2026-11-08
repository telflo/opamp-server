package middleware

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/enrollmenttokens"
)

// rowsLookup answers each token's row from a map.
type rowsLookup map[uuid.UUID]enrollmenttokens.Authenticated

func (m rowsLookup) Lookup(_ context.Context, id uuid.UUID) (enrollmenttokens.Authenticated, error) {
	row, ok := m[id]
	if !ok {
		return enrollmenttokens.Authenticated{}, enrollmenttokens.ErrNotFound
	}
	return row, nil
}

// statusWriter records the status a request was answered, and unwraps so
// http.ResponseController still reaches the connection.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// stallServer is a real listener serving stack around a handler that reads
// the body, counting the requests that entered and left it, and the
// statuses they were answered.
type stallServer struct {
	srv           *httptest.Server
	entered, left atomic.Int64
	mu            sync.Mutex
	statuses      map[int]int
	conns         []net.Conn
	writers       sync.WaitGroup
}

func newStallServer(t *testing.T, stack AgentStack) *stallServer {
	t.Helper()
	s := &stallServer{statuses: map[int]int{}}
	inner := stack.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.entered.Add(1)
		recorder := &statusWriter{ResponseWriter: w}
		defer func() {
			s.mu.Lock()
			s.statuses[recorder.status]++
			s.mu.Unlock()
			s.left.Add(1)
		}()
		inner.ServeHTTP(recorder, r)
	}))
	// The agent listener's header timeout; no ReadTimeout of note, so only
	// the stack's body deadline can end a stalled body.
	s.srv.Config.ReadHeaderTimeout = 10 * time.Second
	s.srv.Config.ReadTimeout = 10 * time.Minute
	s.srv.Start()
	// Cleanups run last first: the connections close before the server.
	t.Cleanup(s.srv.Close)
	t.Cleanup(s.closeAll)
	return s
}

// stall opens a connection and sends a request with bearer whose body is
// never finished: kind 0 promises 4 MiB and withholds its last byte, kind 1
// promises 1,000 bytes and sends one, kind 2 sends one chunk of a chunked
// body.
func (s *stallServer) stall(t *testing.T, bearer string, kind int) {
	t.Helper()
	c, err := net.Dial("tcp", s.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.conns = append(s.conns, c)
	s.mu.Unlock()
	head := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: x\r\nAuthorization: %s\r\n", AgentRoute, bearer)
	switch kind {
	case 0:
		s.writers.Add(1)
		go func() {
			defer s.writers.Done()
			_, _ = fmt.Fprintf(c, "%sContent-Length: %d\r\n\r\n", head, MaxBodyBytes)
			_, _ = c.Write(bytes.Repeat([]byte("y"), MaxBodyBytes-1)) // fails once the server closes it
		}()
	case 1:
		_, _ = fmt.Fprintf(c, "%sContent-Length: 1000\r\n\r\nx", head)
	default:
		_, _ = fmt.Fprintf(c, "%sTransfer-Encoding: chunked\r\n\r\n1\r\nx\r\n", head)
	}
}

// closeAll closes every stalled connection: its client goes away.
func (s *stallServer) closeAll() {
	s.mu.Lock()
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	s.writers.Wait()
}

// statusCount is how many requests were answered status.
func (s *stallServer) statusCount(status int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statuses[status]
}

// waitFor waits, bounded by attempts, for cond; the label says what didn't
// happen.
func waitFor(t *testing.T, label string, cond func() bool) {
	t.Helper()
	for attempt := 0; !cond(); attempt++ {
		if attempt == 2000 {
			t.Fatalf("never: %s", label)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// poll sends one small authenticated request to the server and answers its
// status and Retry-After.
func (s *stallServer) poll(t *testing.T, bearer string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.srv.URL+AgentRoute, strings.NewReader("an AgentToServer"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", bearer)
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Retry-After")
}

// One organization's valid credential sends a hundred bodies and never
// finishes them. It holds only its share of the pod's slots (25 of 100);
// the other 75 are shed at once (429, Retry-After 3 to 8 s) with none of
// their bodies read; another organization's poll still answers 200. When
// the stalled clients go away, their requests end and are logged at Info as
// request_cancelled, never as a warning or an error. Without the share the
// hundred would hold all 100 slots (another organization's poll shed) for
// the listener's whole ReadTimeout.
func TestOneOrganizationsStalledBodiesHoldOnlyItsShareOfTheSlots(t *testing.T) {
	k := keyring(t, currentKeySetting)
	tokenA, orgA, fleetA := uuid.New(), uuid.New(), uuid.New()
	tokenB, orgB, fleetB := uuid.New(), uuid.New(), uuid.New()
	clock := newTestClock()
	logs := &strings.Builder{}
	stack := testStack(k, rowsLookup{tokenA: activeRow(tokenA, orgA, fleetA), tokenB: activeRow(tokenB, orgB, fleetB)},
		DefaultMaxConcurrentRequests, clock)
	stack.Logger = captureLogs(logs)
	// Long enough that no stall ends by its deadline during the test.
	stack.BodyReadTimeout = time.Hour
	s := newStallServer(t, stack)

	bearerA := "Bearer " + mint(t, k, tokenA, fleetA)
	const stalled = DefaultMaxConcurrentRequests
	for i := range stalled {
		s.stall(t, bearerA, i%3)
	}
	share := DefaultMaxConcurrentRequestsPerOrganization
	waitFor(t, "the stalls settled at the organization's share", func() bool {
		return s.entered.Load() == stalled && s.left.Load() == stalled-int64(share) &&
			stack.OrganizationConcurrency.InFlight(orgA) == share
	})
	if snap := stack.Concurrency.Snapshot(); snap.InFlight != share {
		t.Fatalf("organization A's stalls hold %d of the pod's %d slots; want its share, %d", snap.InFlight, snap.MaxInFlight, share)
	}
	if shed := s.statusCount(http.StatusTooManyRequests); shed != stalled-share {
		t.Fatalf("%d of A's requests shed; want %d", shed, stalled-share)
	}

	status, retryAfter := s.poll(t, "Bearer "+mint(t, k, tokenB, fleetB))
	if status != http.StatusOK {
		t.Fatalf("organization B's poll beside A's stalls: %d (Retry-After %q)", status, retryAfter)
	}
	status, retryAfter = s.poll(t, bearerA)
	if seconds, _ := strconv.Atoi(retryAfter); status != http.StatusTooManyRequests || seconds < 3 || seconds > 8 {
		t.Fatalf("organization A's next poll: %d, Retry-After %q; want 429 within 3-8 s", status, retryAfter)
	}

	// The stalled clients go away.
	s.closeAll()
	waitFor(t, "the stalled requests ended once their clients went away", func() bool {
		return s.left.Load() == stalled+2 && stack.Concurrency.Snapshot().InFlight == 0
	})
	cancelled := 0
	for _, line := range logLines(t, logs) {
		switch line["outcome"] {
		case outcomeCancelled:
			cancelled++
			if line["level"] != "INFO" || line["organization_id"] != orgA.String() {
				t.Errorf("a cancelled body's line: %v", line)
			}
		case outcomeOrganizationShed:
			if line["level"] != "WARN" || line["organization_id"] != orgA.String() {
				t.Errorf("a shed line: %v", line)
			}
		default:
			t.Errorf("an unexpected line: %v", line)
		}
	}
	if cancelled != share {
		t.Fatalf("%d request_cancelled lines; want %d, one per stalled body its client abandoned", cancelled, share)
	}
	if strings.Contains(logs.String(), bearerA[len("Bearer "):]) {
		t.Fatal("a bearer in the logs")
	}
}

// An authenticated body not finished within the read deadline
// (BodyReadTimeout; a short one here) is refused 400, its slot given back,
// and logged as body_read_timeout, one line each with its ids: the stalls
// end by the deadline, not by the listener's ReadTimeout (10 minutes here).
// Without the deadline nothing would end them but that ReadTimeout.
func TestAnAuthenticatedBodyStalledPastItsReadDeadlineIsRefused(t *testing.T) {
	k := keyring(t, currentKeySetting)
	token, organization, fleet := uuid.New(), uuid.New(), uuid.New()
	clock := newTestClock()
	logs := &strings.Builder{}
	stack := testStack(k, rowsLookup{token: activeRow(token, organization, fleet)}, DefaultMaxConcurrentRequests, clock)
	stack.Logger = captureLogs(logs)
	stack.BodyReadTimeout = 200 * time.Millisecond
	s := newStallServer(t, stack)

	bearer := "Bearer " + mint(t, k, token, fleet)
	const stalled = 9
	for i := range stalled {
		s.stall(t, bearer, i%3)
	}
	waitFor(t, "the stalled bodies were refused at their read deadline", func() bool {
		return s.left.Load() == stalled && stack.Concurrency.Snapshot().InFlight == 0
	})
	if refused := s.statusCount(http.StatusBadRequest); refused != stalled {
		t.Fatalf("%d of %d stalled bodies refused 400 (%v)", refused, stalled, s.statuses)
	}
	// What each stalled client was answered.
	s.mu.Lock()
	conns := append([]net.Conn(nil), s.conns...)
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		if line, _ := bufio.NewReader(c).ReadString('\n'); strings.TrimSpace(line) != "HTTP/1.1 400 Bad Request" {
			t.Errorf("a stalled client was answered %q", line)
		}
	}
	lines := logLines(t, logs)
	if len(lines) != stalled {
		t.Fatalf("%d log lines; want one per stalled body: %v", len(lines), lines)
	}
	for _, line := range lines {
		if line["level"] != "WARN" || line["outcome"] != outcomeBodyTimeout || line["organization_id"] != organization.String() ||
			line["token_id"] != token.String() {
			t.Errorf("a timed-out body's line: %v", line)
		}
	}
	// A poll after them is read and answered.
	if status, _ := s.poll(t, bearer); status != http.StatusOK {
		t.Fatalf("a poll after the stalls: %d", status)
	}
}

// The organization's share counts its requests in flight, frees a slot as
// each returns, and keeps no entry for an organization with none.
func TestTheOrganizationsShareIsGivenBackAsRequestsReturn(t *testing.T) {
	c := NewOrganizationConcurrency(2, newTestClock().Now, discard())
	a, b := uuid.New(), uuid.New()
	first, second, third, other := c.acquire(a), c.acquire(a), c.acquire(a), c.acquire(b)
	if !first || !second || third || !other {
		t.Fatal("the share of 2 wasn't kept per organization")
	}
	c.release(a)
	if !c.acquire(a) || c.InFlight(a) != 2 {
		t.Fatalf("a released slot wasn't given back: %d", c.InFlight(a))
	}
	c.release(a)
	c.release(a)
	c.release(b)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.inFlight) != 0 {
		t.Fatalf("entries kept for organizations with nothing in flight: %v", c.inFlight)
	}
}

// The organization's rate limit comes before the body is read (it needs
// only the Principal): an organization at its requests a minute is refused
// 429 with none of its body read, and its slot given back.
func TestAnOrganizationOverItsRateIsRefusedBeforeItsBodyIsRead(t *testing.T) {
	k := keyring(t, currentKeySetting)
	token, organization, fleet := uuid.New(), uuid.New(), uuid.New()
	clock := newTestClock()
	stack := testStack(k, rowsLookup{token: activeRow(token, organization, fleet)}, DefaultMaxConcurrentRequests, clock)
	limits := &limitsStub{rpm: map[uuid.UUID]*int32{}}
	limits.set(organization, ptr32(1))
	stack.Organizations = NewOrganizationLimiter(limits, DefaultRequestsPerMinute, clock.Now, discard())
	h := stack.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	bearer := "Bearer " + mint(t, k, token, fleet)
	for i, want := range []int{http.StatusOK, http.StatusTooManyRequests} {
		var read atomic.Int64
		req := httptest.NewRequest(http.MethodPost, AgentRoute, strings.NewReader("an AgentToServer"))
		req.Body = countedBody{ReadCloser: req.Body, read: &read}
		req.Header.Set("Authorization", bearer)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want || (want == http.StatusTooManyRequests) != (read.Load() == 0) {
			t.Fatalf("request %d: %d with %d bytes of its body read; want %d", i+1, rec.Code, read.Load(), want)
		}
	}
	if n := stack.OrganizationConcurrency.InFlight(organization); n != 0 {
		t.Fatalf("%d slots still held", n)
	}
}
