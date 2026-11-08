package telemetry

import "syscall"

// dupOnto makes newfd a copy of oldfd.
func dupOnto(oldfd, newfd int) error { return syscall.Dup2(oldfd, newfd) }
