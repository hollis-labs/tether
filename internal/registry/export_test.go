package registry

// export_test.go exposes package-private constructors to the _test package
// so service_test.go can wire a Service against a stubbed storageBackend
// for URN-collision-retry coverage. The production constructor remains
// NewService(*Storage) — there is no way to inject a stub via the public
// surface, by design.

import "time"

// NewServiceFromBackendForTest constructs a Service against any
// storageBackend. Only available to *_test.go files in this package's
// black-box tests.
func NewServiceFromBackendForTest(b StorageBackendForTest) *Service {
	return &Service{
		storage:   b,
		resolvers: map[string]Resolver{},
	}
}

// StorageBackendForTest re-exports the package-private storageBackend
// interface under a test-only name. _test.go files in package
// registry_test depend on this name to define stub types that satisfy
// the contract.
type StorageBackendForTest = storageBackend

// SetFileResolverMaxBytesForTest overrides the 1 MiB default cap on a
// FileResolver so oversize-rejection tests can use small fixtures.
func SetFileResolverMaxBytesForTest(r *FileResolver, n int64) {
	r.maxBytes = n
}

// SetCLIResolverMaxBytesForTest overrides the 1 MiB default cap on a
// CLIResolver so oversize-rejection tests can use small fixtures.
func SetCLIResolverMaxBytesForTest(r *CLIResolver, n int) {
	r.maxBytes = n
}

// SetCLIResolverTimeoutForTest overrides the default 5 s timeout on a
// CLIResolver so timeout tests can run in tens of milliseconds.
func SetCLIResolverTimeoutForTest(r *CLIResolver, d time.Duration) {
	r.timeout = d
}
