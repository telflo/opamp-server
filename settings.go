package main

import "fmt"

// migrationDatabaseURLVariable is `migrate`'s setting. serve's settings are
// package serve's (serve.LoadSettings).
const migrationDatabaseURLVariable = "OPAMP_MIGRATION_DATABASE_URL"

// requiredURL reads a database URL setting, failing when it is unset.
func requiredURL(getenv func(string) string, variable string) (string, error) {
	value := getenv(variable)
	if value == "" {
		return "", fmt.Errorf("%s is not set", variable)
	}
	return value, nil
}
