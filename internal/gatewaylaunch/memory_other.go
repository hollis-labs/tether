//go:build !linux

package gatewaylaunch

import "errors"

func memoryDir() (string, error) {
	return "", errors.New("gateway credential tmpfs is unavailable on this platform")
}

func validateMemoryPath(string) error {
	return errors.New("gateway credential tmpfs is unavailable on this platform")
}
