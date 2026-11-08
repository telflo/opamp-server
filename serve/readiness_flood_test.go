//go:build flood

// TestAReadyFloodLeavesTheProbeReady is off by default: on a developer's Mac
// (a listen backlog of 128) its 2,000 local clients make some connections
// fail and some answers 503, whatever the server does, so it fails most
// runs, before and after the memorydb removal alike. Run it on purpose
// with the flood tag:
//
//	go test -tags flood -run '^TestAReadyFloodLeavesTheProbeReady$' ./management/
package serve

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/telflo/opamp-server/store/database"
)

// A flood of /ready on the public agent port leaves the pod ready. On a
// real database, through the real one-connection readiness pool, over
// HTTP: 2,000 clients at once ask /ready 5 times each while a separate
// probe (the kubelet's) asks 10 times, the injected clock moving a
// ReadyCacheTTL every 500 answers. Every answer, the probe's included, is
// 200, and the database was pinged at most once per TTL (a ping per
// request would queue 10,010 pings on one connection, and the probe would
// time out 503).
func TestAReadyFloodLeavesTheProbeReady(t *testing.T) {
	d := testDB(t)
	pool, err := database.OpenReadinessPool(context.Background(), d.RuntimeURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var pings atomic.Int64
	clock := newTestClock()
	r := newReadiness(func(ctx context.Context) error {
		pings.Add(1)
		return pool.Ping(ctx)
	}, clock.Now)
	r.serving()
	var answered, advances atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.ServeHTTP(w, req)
		if answered.Add(1)%500 == 0 {
			clock.Advance(ReadyCacheTTL)
			advances.Add(1)
		}
	}))
	t.Cleanup(server.Close)

	const clients, asks = 2000, 5
	transport := &http.Transport{MaxIdleConnsPerHost: clients}
	t.Cleanup(transport.CloseIdleConnections)
	flood := &http.Client{Transport: transport}
	get := func(client *http.Client) int {
		resp, err := client.Get(server.URL + "/ready")
		if err != nil {
			return -1
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	var wg sync.WaitGroup
	var refused atomic.Int64
	for range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range asks {
				if get(flood) != http.StatusOK {
					refused.Add(1)
				}
			}
		}()
	}
	probe := &http.Client{Transport: &http.Transport{}}
	var probeCodes []int
	for range 10 {
		probeCodes = append(probeCodes, get(probe))
	}
	wg.Wait()
	for _, code := range probeCodes {
		if code != http.StatusOK {
			t.Fatalf("the probe during the flood: %v", probeCodes)
		}
	}
	if n := refused.Load(); n != 0 {
		t.Fatalf("%d of the flood's %d asks weren't 200", n, clients*asks)
	}
	if n, most := pings.Load(), advances.Load()+1; n > most {
		t.Fatalf("%d pings for %d asks; want at most %d (one per TTL)", n, answered.Load(), most)
	}
}
