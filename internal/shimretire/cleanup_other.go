//go:build !linux && !darwin

package shimretire

import "os"

func fileIdentity(os.FileInfo) FileIdentity { return FileIdentity{} }
func parentPath(string) string              { return "." }
func confinedParents(*os.Root, string) bool { return false }
