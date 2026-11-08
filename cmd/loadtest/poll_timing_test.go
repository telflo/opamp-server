package main

import (
	"context"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

// With --poll-timing on, EVERY poll (not just the first) prints one timing line
// reporting elapsed-since-launch and the request's round-trip duration.
func TestPollTiming_PrintsEveryPoll(t *testing.T) {
	empty, err := proto.Marshal(&protobufs.ServerToAgent{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Write(empty)
	}))
	defer srv.Close()

	buf := captureLog(t) // from logging_test.go; also resets verbose
	pollTiming = true
	t.Cleanup(func() { pollTiming = false })
	runStart = time.Now()

	vs := &VirtualSupervisor{
		name:         "col-0007",
		token:        "Bearer t",
		instanceUID:  newInstanceUID(),
		endpoint:     srv.URL,
		pollInterval: time.Second,
		client:       srv.Client(),
		metrics:      &Collector{},
		rng:          rand.New(rand.NewSource(1)),
	}
	ctx := context.Background()
	vs.poll(ctx, true)
	vs.poll(ctx, false)
	vs.poll(ctx, false)

	out := buf.String()
	if n := strings.Count(out, "took"); n != 3 {
		t.Errorf("want a timing line per poll (3), got %d: %q", n, out)
	}
	if !strings.Contains(out, "col-0007") {
		t.Errorf("timing line missing client name: %q", out)
	}
}

// With the flag off (the default), nothing is printed.
func TestPollTiming_SilentByDefault(t *testing.T) {
	empty, _ := proto.Marshal(&protobufs.ServerToAgent{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(empty)
	}))
	defer srv.Close()

	buf := captureLog(t)
	pollTiming = false
	runStart = time.Now()

	vs := &VirtualSupervisor{
		name: "col-0008", token: "Bearer t", instanceUID: newInstanceUID(),
		endpoint: srv.URL, pollInterval: time.Second, client: srv.Client(),
		metrics: &Collector{}, rng: rand.New(rand.NewSource(1)),
	}
	vs.poll(context.Background(), true)

	if strings.Contains(buf.String(), "took") {
		t.Errorf("timing line printed while flag off: %q", buf.String())
	}
}
