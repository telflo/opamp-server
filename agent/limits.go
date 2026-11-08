package agent

import (
	"fmt"

	"github.com/telflo/opamp-server/internal/envconfig"
	"github.com/telflo/opamp-server/middleware"
)

// The agent side's limits' settings, each counted per pod. serve reads them
// with LoadLimits, part of LoadSettings, into the agent listener's
// middleware.AgentStack.
const (
	DefaultRequestsPerMinuteVariable   = "OPAMP_DEFAULT_REQUESTS_PER_MINUTE"
	CollectorRequestsPerMinuteVariable = "OPAMP_COLLECTOR_REQUESTS_PER_MINUTE"
	MaxConcurrentRequestsVariable      = "OPAMP_MAX_CONCURRENT_REQUESTS"
	// MaxConcurrentRequestsPerOrganizationVariable is one organization's
	// share of the pod's concurrent requests, so no organization can hold
	// every slot (middleware.OrganizationConcurrency): from 1 to
	// OPAMP_MAX_CONCURRENT_REQUESTS; unset, 25 or the pod's cap if lower.
	MaxConcurrentRequestsPerOrganizationVariable = "OPAMP_MAX_CONCURRENT_REQUESTS_PER_ORGANIZATION"
)

// The settings' bounds: each a whole number, at least 1.
const (
	// MaxDefaultRequestsPerMinute bounds an organization's default rate: a
	// thousand times the default, far past what one pod serves.
	MaxDefaultRequestsPerMinute = 12_000_000
	// MaxCollectorRequestsPerMinute bounds the per-collector guard: ten a
	// second.
	MaxCollectorRequestsPerMinute = 600
	// MaxConcurrentRequests bounds the concurrency cap.
	MaxConcurrentRequests = 10_000
)

// Limits are the agent side's limits as serve reads them.
type Limits struct {
	// DefaultRequestsPerMinute is an organization's requests a minute to
	// each pod when its requests_per_minute is null.
	DefaultRequestsPerMinute int
	// CollectorRequestsPerMinute is one collector's requests a minute (with
	// a burst of middleware.CollectorBurst).
	CollectorRequestsPerMinute int
	// MaxConcurrentRequests is the most agent requests a pod handles at once.
	MaxConcurrentRequests int
	// MaxConcurrentRequestsPerOrganization is the most of one
	// organization's agent requests a pod handles at once (at most
	// MaxConcurrentRequests).
	MaxConcurrentRequestsPerOrganization int
}

// LoadLimits reads the four settings, each its default when unset,
// failing on the first that isn't a whole number within its bounds (an
// organization's share of the concurrent requests is bounded by the pod's). Its
// errors name the variable and the bounds, never the value.
func LoadLimits(getenv func(string) string) (Limits, error) {
	var limits Limits
	for _, setting := range []struct {
		variable      string
		into          *int
		fallback, max int
	}{
		{DefaultRequestsPerMinuteVariable, &limits.DefaultRequestsPerMinute, middleware.DefaultRequestsPerMinute, MaxDefaultRequestsPerMinute},
		{CollectorRequestsPerMinuteVariable, &limits.CollectorRequestsPerMinute, middleware.DefaultCollectorRequestsPerMinute, MaxCollectorRequestsPerMinute},
		{MaxConcurrentRequestsVariable, &limits.MaxConcurrentRequests, middleware.DefaultMaxConcurrentRequests, MaxConcurrentRequests},
	} {
		value, err := boundedInt(getenv, setting.variable, setting.fallback, setting.max)
		if err != nil {
			return Limits{}, err
		}
		*setting.into = value
	}
	// An organization's share: at most the pod's cap, which bounds it, and
	// by default the lower of 25 and that cap.
	perOrganization, err := boundedInt(getenv, MaxConcurrentRequestsPerOrganizationVariable,
		min(middleware.DefaultMaxConcurrentRequestsPerOrganization, limits.MaxConcurrentRequests), limits.MaxConcurrentRequests)
	if err != nil {
		return Limits{}, fmt.Errorf("%s must be a whole number from 1 to %s's value, %d",
			MaxConcurrentRequestsPerOrganizationVariable, MaxConcurrentRequestsVariable, limits.MaxConcurrentRequests)
	}
	limits.MaxConcurrentRequestsPerOrganization = perOrganization
	return limits, nil
}

// boundedInt is variable's whole number from 1 to max (decimal digits only:
// no sign, space or leading zero), or fallback when it is unset.
func boundedInt(getenv func(string) string, variable string, fallback, max int) (int, error) {
	return envconfig.RangedInt(getenv, variable, fallback, 1, max)
}
