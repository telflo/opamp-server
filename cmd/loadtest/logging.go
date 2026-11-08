package main

import (
	"log"
	"time"
)

// verbose gates all non-error output. When false (the default) the tool prints
// only errors — transport failures and non-2xx responses — so a large run stays
// quiet unless something is wrong. --verbose turns on the startup banner, the
// periodic metrics summaries, and the per-poll lifecycle logs. A package global
// keeps the gate out of every VirtualSupervisor and call site.
var verbose bool

// pollTiming, when set by --poll-timing, makes each poll print one line with its
// elapsed-since-runStart and round-trip duration (independent of --verbose).
// runStart is stamped in main when the launch loop begins.
var (
	pollTiming bool
	runStart   time.Time
)

// logInfo prints a non-error message, but only when --verbose is set.
func logInfo(format string, args ...any) {
	if verbose {
		log.Printf(format, args...)
	}
}

// logError prints an error message unconditionally — it fires regardless of
// --verbose so failures (transport errors, non-2xx responses, parse failures)
// are never silently swallowed.
func logError(format string, args ...any) {
	log.Printf(format, args...)
}
