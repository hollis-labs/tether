package identity

// SetRotateAfterReplace installs the rotation failure seam for tests.
func SetRotateAfterReplace(f func() error) { rotateAfterReplace = f }
