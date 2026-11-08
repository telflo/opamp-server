package telemetry

import "syscall"

// dupOnto makes newfd a copy of oldfd. Dup3, since linux/arm64 has no Dup2.
func dupOnto(oldfd, newfd int) error { return syscall.Dup3(oldfd, newfd, 0) }
