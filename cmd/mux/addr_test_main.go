//go:build addr_test

package main

import (
	"fmt"
	"os"
)

func init() {
	// This is never called - just to test compilation
	_ = os.Getenv
	_ = fmt.Println
}
