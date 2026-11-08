//go:build !darwin && !linux

package telemetry

import "errors"

// dupOnto isn't written for this platform: the stderr tests skip.
func dupOnto(int, int) error { return errors.ErrUnsupported }
