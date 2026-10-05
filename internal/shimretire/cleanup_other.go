//go:build !linux && !darwin

package shimretire

import "os"

func fileIdentity(os.FileInfo) FileIdentity { return FileIdentity{} }
