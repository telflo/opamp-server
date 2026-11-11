package serve

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/telflo/telflo-opamp-server/agent"
	"github.com/telflo/telflo-opamp-server/internal/diagnostics"
	"github.com/telflo/telflo-opamp-server/management"
)

// serve's argument names the role: none for both components, agent or
// management for one; anything else is no role.
func TestParseRoleNamesOneOfThreeRoles(t *testing.T) {
	for _, c := range []struct {
		args []string
		want Role
	}{{nil, RoleAll}, {[]string{"agent"}, RoleAgent}, {[]string{"management"}, RoleManagement}} {
		if got, ok := ParseRole(c.args); !ok || got != c.want {
			t.Errorf("ParseRole(%q) = %v, %v; want %v", c.args, got, ok, c.want)
		}
	}
	for _, args := range [][]string{{"all"}, {"Agent"}, {"agent", "management"}, {""}, {"api"}} {
		if _, ok := ParseRole(args); ok {
			t.Errorf("ParseRole(%q) accepted it", args)
		}
	}
	if RoleAll.String() != "all" || RoleAgent.String() != "agent" || RoleManagement.String() != "management" {
		t.Fatal("a role's name isn't its command's")
	}
}

// Each role reads only its components' settings: the agent role needs no
// management secret (and notes one set), the management role no server
// URL; both components' settings are needed only for both.
func TestEachRoleReadsOnlyItsComponentsSettings(t *testing.T) {
	process := map[string]string{
		EncryptionKeyVariable: testEncryptionKey,
		DatabaseURLVariable:   "postgresql://telflo_opamp@db/telflo_opamp",
	}
	with := func(more map[string]string) func(string) string {
		values := map[string]string{}
		for _, m := range []map[string]string{process, more} {
			for name, value := range m {
				values[name] = value
			}
		}
		return envOf(values)
	}
	secret := map[string]string{management.SecretVariable: testSecret}
	serverURL := map[string]string{agent.ServerURLVariable: testServerURL}

	agentOnly, err := LoadSettings(RoleAgent, with(serverURL), unsetNothing)
	if err != nil || agentOnly.Role != RoleAgent || agentOnly.Agent.ServerURL != testServerURL ||
		agentOnly.Management.Secrets != nil || agentOnly.UnusedManagementSecret {
		t.Fatalf("the agent role without a secret: %+v, %v", agentOnly, err)
	}
	agentOnly, err = LoadSettings(RoleAgent, with(map[string]string{agent.ServerURLVariable: testServerURL,
		management.PreviousSecretsVariable: testSecret}), unsetNothing)
	if err != nil || !agentOnly.UnusedManagementSecret || agentOnly.Management.Secrets != nil {
		t.Fatalf("the agent role with a previous secret set: %+v, %v", agentOnly, err)
	}
	if _, err := LoadSettings(RoleAgent, with(secret), unsetNothing); err == nil ||
		!strings.HasPrefix(err.Error(), agent.ServerURLVariable+" is not set") {
		t.Fatalf("the agent role without a server URL: %v", err)
	}

	managementOnly, err := LoadSettings(RoleManagement, with(secret), unsetNothing)
	if err != nil || managementOnly.Role != RoleManagement || managementOnly.Management.Secrets == nil ||
		managementOnly.Agent.ServerURL != "" || managementOnly.UnusedManagementSecret {
		t.Fatalf("the management role without a server URL: %+v, %v", managementOnly, err)
	}
	if _, err := LoadSettings(RoleManagement, with(serverURL), unsetNothing); err == nil ||
		err.Error() != management.SecretVariable+" is not set" {
		t.Fatalf("the management role without a secret: %v", err)
	}

	for _, missing := range []map[string]string{secret, serverURL} {
		if _, err := LoadSettings(RoleAll, with(missing), unsetNothing); err == nil {
			t.Fatalf("both components with only %v: accepted", missing)
		}
	}
}

// startRole runs serveWith for role on the test's database, with the
// management API's and the agent endpoint's settings (whatever the role
// reads) on free ports, until the test ends; it answers the log.
func startRole(t *testing.T, role Role, more map[string]string) *syncBuffer {
	t.Helper()
	d := testDB(t)
	values := map[string]string{
		DatabaseURLVariable: d.RuntimeURL, EncryptionKeyVariable: testEncryptionKey, ShutdownDrainVariable: "0",
		management.HostVariable: "127.0.0.1", management.PortVariable: "0",
		agent.HostVariable: "127.0.0.1", agent.PortVariable: "0",
	}
	for name, value := range more {
		values[name] = value
	}
	log := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan int, 1)
	go func() {
		exited <- serveWith(ctx, role, envOf(values), unsetNothing, slog.New(slog.NewJSONHandler(log, nil)), (&destroyRecorder{}).destroy, nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-exited:
			if code != 0 {
				t.Errorf("serve %v exited %d; log %s", role, code, log.String())
			}
		case <-time.After(ShutdownTimeout + 30*time.Second):
			t.Errorf("serve %v didn't stop", role)
		}
	})
	return log
}

// messages are the messages of every line logged so far.
func messages(t *testing.T, log *syncBuffer) []string {
	t.Helper()
	var msgs []string
	for _, line := range logLines(t, log.String()) {
		msgs = append(msgs, line["msg"].(string))
	}
	return msgs
}

// `serve agent` opens the agent listener and, with its port set, the
// diagnostics listener on loopback, and no management listener; it reads
// no management secret, warns when one is set anyway, and runs the error
// history's cleanup. The diagnostics answer the agent endpoint's own gate.
func TestServeAgentRunsTheAgentEndpointAlone(t *testing.T) {
	log := startRole(t, RoleAgent, map[string]string{
		agent.ServerURLVariable: testServerURL, agent.MaxConcurrentRequestsVariable: "7",
		agent.DiagnosticsPortVariable: "0", management.SecretVariable: testSecret,
	})
	agentURL := "http://" + waitForLine(t, log, "serving the agent endpoint")["address"].(string)
	diagnosticsAddress := waitForLine(t, log, "serving the agent's diagnostics")["address"].(string)
	waitForLine(t, log, "the error history's cleanup ran")
	warned := waitForLine(t, log, management.SecretVariable+" is set, but `serve agent` runs no management API: this process needn't hold the secret")
	if warned["level"] != "WARN" {
		t.Fatalf("the unused secret's line: %v", warned)
	}
	for _, msg := range messages(t, log) {
		if strings.Contains(msg, "the management API") {
			t.Fatalf("serve agent logged %q", msg)
		}
	}
	if strings.Contains(log.String(), testSecret) {
		t.Fatal("the log holds the secret")
	}
	if !strings.HasPrefix(diagnosticsAddress, "127.0.0.1:") {
		t.Fatalf("the diagnostics listen on %s, want loopback", diagnosticsAddress)
	}

	if status, reply := over(t, http.MethodGet, agentURL+"/ready", "", nil); status != http.StatusOK || string(reply) != "ready\n" {
		t.Fatalf("the agent port's /ready: %d %q", status, reply)
	}
	status, reply := over(t, http.MethodGet, "http://"+diagnosticsAddress+diagnostics.AdmissionPath, "", nil)
	var view map[string]any
	if status != http.StatusOK || json.Unmarshal(reply, &view) != nil || view["enabled"] != true || view["max_in_flight"] != float64(7) {
		t.Fatalf("the admission view: %d %s", status, reply)
	}
	if status, reply := over(t, http.MethodGet, "http://"+diagnosticsAddress+diagnostics.PprofPrefix, "", nil); status != http.StatusOK ||
		!strings.Contains(string(reply), "goroutine") {
		t.Fatalf("the profiles' index: %d", status)
	}
	if status, _ := over(t, http.MethodGet, agentURL+diagnostics.AdmissionPath, "", nil); status != http.StatusNotFound {
		t.Fatalf("the agent port serves the diagnostics: %d", status)
	}
}

// `serve management` opens the management listener alone: it needs no
// server URL, answers /health and /ready on its own port, reports a
// disabled admission gate (no agent endpoint in this process), and runs no
// cleanup.
func TestServeManagementRunsTheManagementAPIAlone(t *testing.T) {
	log := startRole(t, RoleManagement, map[string]string{management.SecretVariable: testSecret})
	managementURL := "http://" + waitForLine(t, log, "serving the management API")["address"].(string)
	for _, path := range []string{"/health", "/ready"} {
		if status, _ := over(t, http.MethodGet, managementURL+path, "", nil); status != http.StatusOK {
			t.Fatalf("the management port's %s: %d", path, status)
		}
	}
	status, reply := over(t, http.MethodGet, managementURL+diagnostics.AdmissionPath, "Bearer "+testSecret, nil)
	var view map[string]any
	if status != http.StatusOK || json.Unmarshal(reply, &view) != nil || view["enabled"] != false {
		t.Fatalf("the admission view: %d %s", status, reply)
	}
	if status, reply := over(t, http.MethodPost, managementURL+organizationsPath, "Bearer "+testSecret,
		body{"id": "5e5e5e5e-0000-4000-8000-00000000005e", "name": "an organization"}.bytes()); status != http.StatusCreated {
		t.Fatalf("registering an organization: %d %s", status, reply)
	}
	for _, msg := range messages(t, log) {
		if strings.Contains(msg, "agent") || strings.Contains(msg, "cleanup") {
			t.Fatalf("serve management logged %q", msg)
		}
	}
}

// Both components in one process answer /ready on both ports, from one
// readiness: told to stop, both turn 503 together while the drain holds the
// listeners open.
func TestBothPortsAnswerTheOneReadiness(t *testing.T) {
	d := testDB(t)
	env := envOf(map[string]string{
		DatabaseURLVariable: d.RuntimeURL, EncryptionKeyVariable: testEncryptionKey, ShutdownDrainVariable: "3",
		management.HostVariable: "127.0.0.1", management.PortVariable: "0", management.SecretVariable: testSecret,
		agent.HostVariable: "127.0.0.1", agent.PortVariable: "0", agent.ServerURLVariable: testServerURL,
	})
	asked, drained := make(chan time.Duration, 1), make(chan time.Time)
	adjust := func(r *replica) {
		r.drainTimer = func(d time.Duration) <-chan time.Time {
			asked <- d
			return drained
		}
	}
	log := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exited := make(chan int, 1)
	go func() {
		exited <- serveWith(ctx, RoleAll, env, unsetNothing, slog.New(slog.NewJSONHandler(log, nil)), (&destroyRecorder{}).destroy, adjust)
	}()
	agentURL := "http://" + waitForLine(t, log, "serving the agent endpoint")["address"].(string)
	managementURL := "http://" + waitForLine(t, log, "serving the management API")["address"].(string)
	waitForLine(t, log, "the error history's cleanup ran")
	for _, base := range []string{agentURL, managementURL} {
		if status, _ := over(t, http.MethodGet, base+"/ready", "", nil); status != http.StatusOK {
			t.Fatalf("%s/ready while serving: %d", base, status)
		}
	}
	cancel()
	select {
	case <-asked:
	case <-time.After(time.Minute):
		t.Fatal("serve never drained")
	}
	for _, base := range []string{agentURL, managementURL} {
		if status, _ := over(t, http.MethodGet, base+"/ready", "", nil); status != http.StatusServiceUnavailable {
			t.Fatalf("%s/ready while draining: %d", base, status)
		}
	}
	close(drained)
	if code := <-exited; code != 0 {
		t.Fatalf("serve exited %d", code)
	}
}
