//go:build windows

package app

import (
	"context"
	"errors"
)

// Shim hosting remains unsupported on Windows; direct runtime routing stays.
type unsupportedHostedCodex struct{}

func (*unsupportedHostedCodex) SendTurn(context.Context, string, string, string, string) error {
	return errors.New("shim hosting unsupported on Windows")
}
func (*Service) hostedCodexSession(context.Context, string) (*unsupportedHostedCodex, bool, error) {
	return nil, false, nil
}
func (*Service) shouldFlushSessionOutput(context.Context, string) bool { return true }
