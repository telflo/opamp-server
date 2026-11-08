package collectorgroups

// JSONBTextBytes is jsonbTextBytes, so a test can compare its count with
// Postgres' own octet_length of the same value's jsonb text.
var JSONBTextBytes = jsonbTextBytes

// DeadlockRetriesOf is how many times s's Delete runs again after a 40P01.
func DeadlockRetriesOf(s *Postgres) int { return s.deadlockRetries }
