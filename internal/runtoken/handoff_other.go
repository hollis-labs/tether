//go:build !linux

package runtoken

func memoryHandoff([]byte) (string, func(), error) { return "", nil, ErrHandoff }
func readMemoryHandoff(string) ([]byte, error)     { return nil, ErrHandoff }
