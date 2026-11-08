package fleets

// DeadlockRetriesOf is how many times s's Delete runs again after a 40P01.
func DeadlockRetriesOf(s *Postgres) int { return s.deadlockRetries }
