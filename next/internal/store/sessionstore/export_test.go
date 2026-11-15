package sessionstore

// SetTeamCommitHold installs the CommitGoneTeamShimRecovery test hold and returns its restore.
func SetTeamCommitHold(hold func()) (restore func()) {
	old := teamCommitHold
	teamCommitHold = hold
	return func() { teamCommitHold = old }
}
