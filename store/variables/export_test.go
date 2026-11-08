package variables

// SetGroupDeleteHook makes hook run at each attempt of s's DeleteGroup: at
// "start", before its transaction, and at "delete", just before its delete
// statement. A test binds a collector group past the guard with it.
func SetGroupDeleteHook(s *Postgres, hook func(attempt int, stage string)) {
	s.groupDeleteHook = hook
}
