package agent

// envOf is a getenv over values.
func envOf(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// testServerURL is a valid OPAMP_SERVER_URL.
const testServerURL = "https://opamp.test.invalid/opamp/api/v1"
