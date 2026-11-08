package agent

import (
	"strings"
	"testing"

	"github.com/telflo/opamp-server/middleware"
)

// The agent side's limits default to 12,000 requests a minute per
// organization, 60 per collector, 100 requests at once, and 25 of one
// organization's at once.
func TestTheAgentLimitsDefault(t *testing.T) {
	limits, err := LoadLimits(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	want := Limits{DefaultRequestsPerMinute: 12_000, CollectorRequestsPerMinute: 60, MaxConcurrentRequests: 100,
		MaxConcurrentRequestsPerOrganization: 25}
	if limits != want || middleware.DefaultRequestsPerMinute != 12_000 || middleware.DefaultCollectorRequestsPerMinute != 60 ||
		middleware.DefaultMaxConcurrentRequests != 100 {
		t.Fatalf("the defaults = %+v, want %+v", limits, want)
	}
}

func TestTheAgentLimitsAreReadAndBounded(t *testing.T) {
	env := map[string]string{
		DefaultRequestsPerMinuteVariable:             "12000000",
		CollectorRequestsPerMinuteVariable:           "1",
		MaxConcurrentRequestsVariable:                "250",
		MaxConcurrentRequestsPerOrganizationVariable: "250",
	}
	limits, err := LoadLimits(func(name string) string { return env[name] })
	if err != nil || limits != (Limits{DefaultRequestsPerMinute: 12_000_000, CollectorRequestsPerMinute: 1, MaxConcurrentRequests: 250,
		MaxConcurrentRequestsPerOrganization: 250}) {
		t.Fatalf("LoadLimits = %+v, %v", limits, err)
	}
	for _, variable := range []string{DefaultRequestsPerMinuteVariable, CollectorRequestsPerMinuteVariable, MaxConcurrentRequestsVariable,
		MaxConcurrentRequestsPerOrganizationVariable} {
		for _, value := range []string{"0", "-1", "+5", " 5", "5 ", "05", "1e3", "5.0", "ten", "99999999999999999999",
			map[string]string{DefaultRequestsPerMinuteVariable: "12000001", CollectorRequestsPerMinuteVariable: "601",
				MaxConcurrentRequestsVariable: "10001", MaxConcurrentRequestsPerOrganizationVariable: "101"}[variable]} {
			_, err := LoadLimits(func(name string) string {
				if name == variable {
					return value
				}
				return ""
			})
			if err == nil {
				t.Errorf("%s=%q was accepted", variable, value)
				continue
			}
			if !strings.Contains(err.Error(), variable) || (strings.TrimSpace(value) != "" && strings.Contains(err.Error(), "\""+value+"\"")) {
				t.Errorf("%s=%q: the error %q must name the variable, not quote the value", variable, value, err)
			}
		}
	}
}
