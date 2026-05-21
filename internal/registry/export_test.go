package registry

// export_test.go exposes package-private constructors to the _test package
// so service_test.go can wire a Service against a stubbed storageBackend
// for URN-collision-retry coverage. The production constructor remains
// NewService(*Storage) — there is no way to inject a stub via the public
// surface, by design.

// NewServiceFromBackendForTest constructs a Service against any
// storageBackend. Only available to *_test.go files in this package's
// black-box tests.
func NewServiceFromBackendForTest(b StorageBackendForTest) *Service {
	return &Service{storage: b}
}

// StorageBackendForTest re-exports the package-private storageBackend
// interface under a test-only name. _test.go files in package
// registry_test depend on this name to define stub types that satisfy
// the contract.
type StorageBackendForTest = storageBackend
