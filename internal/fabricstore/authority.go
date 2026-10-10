package fabricstore

// EnrollmentAuthority is stored metadata only. It grants no delegation,
// launches, publication, lease fencing or migration authority (ADR 0061).
type EnrollmentAuthority string

const (
	AuthorityEnvironment EnrollmentAuthority = "environment"
	AuthorityHub         EnrollmentAuthority = "hub"
)

func (a EnrollmentAuthority) Effective() EnrollmentAuthority {
	if a == "" {
		return AuthorityEnvironment
	}
	return a
}

func (a EnrollmentAuthority) Validate() error {
	if a.Effective() != AuthorityEnvironment && a.Effective() != AuthorityHub {
		return invalid("enrollment authority must be environment or hub")
	}
	return nil
}
