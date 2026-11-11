package serve

import (
	"fmt"
	"time"

	"github.com/telflo/telflo-opamp-server/agent"
	"github.com/telflo/telflo-opamp-server/internal/envconfig"
	"github.com/telflo/telflo-opamp-server/management"
	"github.com/telflo/telflo-opamp-server/tokens"
)

// The process's own settings. Each component reads its own
// (management.LoadSettings, agent.LoadSettings); the OTEL_* variables are
// read by the telemetry package.
const (
	DatabaseURLVariable = "OPAMP_DATABASE_URL"
	// ShutdownDrainVariable is how long, in seconds, serve keeps its
	// listeners open once it is told to stop and /ready answers 503, before
	// it stops them: time for a load balancer's readiness checks to see the
	// 503 and send no new request, which a listener closed at once never
	// shows them. 0 stops at once (a preStop hook doing the waiting
	// instead).
	ShutdownDrainVariable = "OPAMP_SHUTDOWN_DRAIN_SECONDS"

	// The shutdown drain's default and bounds, in seconds.
	DefaultShutdownDrainSeconds = 5
	MaxShutdownDrainSeconds     = 30
)

// Settings are what `serve` reads from the environment: the process's own,
// and those of each component its role runs.
type Settings struct {
	// Role is which components run; the others' settings are left zero.
	Role        Role
	DatabaseURL string
	// Keys are the enrollment tokens' encryption keys, in locked buffers;
	// serve destroys them on its way out.
	Keys *tokens.Keyring
	// ShutdownDrain is OPAMP_SHUTDOWN_DRAIN_SECONDS.
	ShutdownDrain time.Duration
	// Management is the management API's settings.
	Management management.Settings
	// Agent is the agent endpoint's settings.
	Agent agent.Settings
	// UnusedManagementSecret is whether a management secret is set for a
	// role that runs no management API (`serve agent`): serve warns, since
	// an agent pod need not hold it.
	UnusedManagementSecret bool
}

// LoadSettings reads serve's settings for role, failing on the first one
// that is missing or wrong: the process's, then the management API's, then
// the agent endpoint's, each only when role runs it. Its errors name the variable, never its value. The
// encryption keys are read first, and their value variables unset with
// unsetenv whatever comes after, so no key stays in the environment even
// when a later setting is refused (whose refusal then destroys the keys).
func LoadSettings(role Role, getenv func(string) string, unsetenv func(string) error) (Settings, error) {
	return loadSettings(role, getenv, unsetenv, destroyKeyring)
}

// destroyKeyring is how serve destroys the keys: tests pass a recorder in
// its place (serveWith, loadSettings) to see every way out destroys them.
func destroyKeyring(keys *tokens.Keyring) {
	keys.Destroy()
}

// loadSettings is LoadSettings, destroying the keys with destroy when a
// later setting is refused.
func loadSettings(role Role, getenv func(string) string, unsetenv func(string) error, destroy func(*tokens.Keyring)) (settings Settings, err error) {
	keys, err := LoadEncryptionKeys(getenv, unsetenv)
	if err != nil {
		return Settings{}, err
	}
	defer func() {
		if err != nil {
			destroy(keys)
		}
	}()
	databaseURL := getenv(DatabaseURLVariable)
	if databaseURL == "" {
		return Settings{}, fmt.Errorf("%s is not set", DatabaseURLVariable)
	}
	drain, err := envconfig.RangedInt(getenv, ShutdownDrainVariable, DefaultShutdownDrainSeconds, 0, MaxShutdownDrainSeconds)
	if err != nil {
		return Settings{}, err
	}
	settings = Settings{Role: role, DatabaseURL: databaseURL, Keys: keys, ShutdownDrain: time.Duration(drain) * time.Second}
	if role.runsManagement() {
		if settings.Management, err = management.LoadSettings(getenv); err != nil {
			return Settings{}, err
		}
		if err = refuseDevelopmentManagementSecrets(getenv); err != nil {
			return Settings{}, err
		}
	} else {
		settings.UnusedManagementSecret = getenv(management.SecretVariable) != "" || getenv(management.PreviousSecretsVariable) != ""
	}
	if role.runsAgent() {
		if settings.Agent, err = agent.LoadSettings(getenv); err != nil {
			return Settings{}, err
		}
	}
	return settings, nil
}
