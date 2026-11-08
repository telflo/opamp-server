package main

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Each virtual supervisor must report a distinct, valid host.ip so the fleet's
// collector list shows N different machines rather than collapsing them onto
// one IP.
func TestHostIPForIndex_DistinctAndValid(t *testing.T) {
	seen := make(map[string]int, 1000)
	for i := 0; i < 1000; i++ {
		ip := hostIPForIndex(i)
		if net.ParseIP(ip) == nil {
			t.Fatalf("index %d produced invalid IP %q", i, ip)
		}
		if prev, dup := seen[ip]; dup {
			t.Fatalf("index %d and %d both produced %q", prev, i, ip)
		}
		seen[ip] = i
	}
}

// The generated address must fall in the 10.0.0.0/8 private range so it never
// collides with a routable host and reads as an internal collector.
func TestHostIPForIndex_PrivateRange(t *testing.T) {
	ip := net.ParseIP(hostIPForIndex(0))
	if ip == nil || ip.To4() == nil || ip.To4()[0] != 10 {
		t.Errorf("hostIPForIndex(0) = %q, want a 10.x.x.x address", hostIPForIndex(0))
	}
}

// --collector_group_id is required, as --endpoint is and a count of at
// least one; a blank one is no group.
func TestConfigProblemRequiresTheCollectorGroup(t *testing.T) {
	valid := config{endpoint: "http://127.0.0.1:1/opamp/api/v1", collectorGroup: "8f3c0000-0000-4000-8000-000000000001", count: 1}
	if problem := configProblem(valid); problem != "" {
		t.Fatalf("a valid config: %q", problem)
	}
	for name, change := range map[string]func(c *config){
		"no endpoint":           func(c *config) { c.endpoint = "" },
		"no collector group":    func(c *config) { c.collectorGroup = "" },
		"a blank group":         func(c *config) { c.collectorGroup = " \t" },
		"no virtual supervisor": func(c *config) { c.count = 0 },
	} {
		c := valid
		change(&c)
		if configProblem(c) == "" {
			t.Errorf("%s: no problem", name)
		}
	}
}

// The tool exits 2, saying the group is required, when --collector_group_id
// is left out (main runs in a child process, which exits). The child runs
// on a context with a generous deadline (the hang guard only): a tool that
// didn't stop would run its load for ever, so it is killed then (and its
// output's pipes given up WaitDelay later), failing the test, with no
// child left behind.
func TestWithoutTheCollectorGroupTheToolExits2(t *testing.T) {
	if os.Getenv("LOADTEST_RUN_MAIN") == "1" {
		os.Args = []string{"loadtest", "--endpoint", "http://127.0.0.1:1/opamp/api/v1", "--tokens", "a"}
		main()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWithoutTheCollectorGroupTheToolExits2$")
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = append(os.Environ(), "LOADTEST_RUN_MAIN=1")
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 ||
		!strings.Contains(string(output), "error: --collector_group_id is required") {
		t.Fatalf("the tool without --collector_group_id: %v, %s; want exit 2 saying it is required", err, output)
	}
}
