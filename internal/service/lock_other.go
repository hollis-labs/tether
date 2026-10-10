//go:build !linux

package service

import (
	"context"
	"fmt"
)

func processIdentity(int) (lockOwner, error) {
	return lockOwner{}, fmt.Errorf("worker service supports Linux only")
}
func ownerAlive(lockOwner) (bool, error) {
	return false, fmt.Errorf("worker service supports Linux only")
}
func lockGuard(context.Context, string) (func(), error) {
	return nil, fmt.Errorf("worker service supports Linux only")
}
