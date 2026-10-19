package identity

import "strings"

// EnrollmentAudiencePrefix and IssuerSubjectPrefix are reserved for internal
// issuer protocols. Ordinary principals, exchanges and session grants may not
// acquire these identities, even with an operator or wildcard parent.
const (
	EnrollmentAudiencePrefix = "tachyon-enrollment"
	IssuerSubjectPrefix      = "urn:tether:issuer:"
)

func ReservedIssuerSubject(id string) bool {
	return strings.HasPrefix(id, IssuerSubjectPrefix)
}

func ReservedEnrollmentAudience(audience string) bool {
	return strings.HasPrefix(audience, EnrollmentAudiencePrefix)
}
