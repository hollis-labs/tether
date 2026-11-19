//go:build windows

package app

func (*Service) agyChatControlSupported(string) bool { return false }
