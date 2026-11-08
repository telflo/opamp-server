package main

import (
	"context"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

// A shared --max-inflight semaphore must hard-cap how many polls are in flight
// at once across all clients, no matter how many clients are running. The stub
// server tracks live concurrency; with a cap of 2 and 8 clients polling fast it
// must never observe more than 2 requests in flight simultaneously.
func TestVirtualSupervisor_MaxInflight(t *testing.T) {
	const cap = 2

	empty, err := proto.Marshal(&protobufs.ServerToAgent{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var inflight, maxSeen atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inflight.Add(1)
		for {
			m := maxSeen.Load()
			if cur <= m || maxSeen.CompareAndSwap(m, cur) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond) // hold the slot so polls overlap
		inflight.Add(-1)
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Write(empty)
	}))
	defer srv.Close()

	sem := make(chan struct{}, cap)
	metrics := &Collector{}
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()

	for i := 0; i < 8; i++ {
		vs := &VirtualSupervisor{
			name:           "col",
			token:          "Bearer t",
			instanceUID:    newInstanceUID(),
			endpoint:       srv.URL,
			deployInterval: 5 * time.Millisecond,
			pollInterval:   5 * time.Millisecond,
			jitter:         0.2,
			client:         srv.Client(),
			metrics:        metrics,
			rng:            rand.New(rand.NewSource(int64(i))),
			sem:            sem,
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			vs.Run(ctx)
		}()
	}
	wg.Wait()

	if got := maxSeen.Load(); got > cap {
		t.Errorf("max concurrent in-flight = %d, exceeds cap %d", got, cap)
	}
	if metrics.Snapshot().Total < 2 {
		t.Fatalf("test exercised too few polls (%d) to be meaningful", metrics.Snapshot().Total)
	}
}
