//go:build !unix

package definitionresolve

import "os"

func openRegular(_ *os.Root, _ string) (*os.File, error) {
	return nil, contentError("local provider requires no-follow file opening on this platform")
}
