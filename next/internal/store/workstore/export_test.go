package workstore

// SetLocks switches the Postgres locks the race tests show the need for; it returns a restore func.
func SetLocks(lineage, environmentRequest bool) func() {
	l, e := lockLineage, lockEnvironmentRequest
	lockLineage, lockEnvironmentRequest = lineage, environmentRequest
	return func() { lockLineage, lockEnvironmentRequest = l, e }
}
