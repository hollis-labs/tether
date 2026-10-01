package main

import (
	"errors"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/identity"
	"os"
	"path/filepath"
)

func callerToken() (string, error) {
	if tokenFilePath != "" {
		return identity.ReadTokenFile(tokenFilePath)
	}
	if token := os.Getenv("TETHER_TOKEN"); token != "" {
		return token, nil
	}
	token, err := identity.ReadTokenFile(filepath.Join(filepath.Dir(expandCatalogPath()), "run", "operator.token"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return token, err
}
func daemonClient(addr string) *client.Client {
	if tokenFilePath != "" {
		return client.New(addr, client.WithTokenFile(tokenFilePath))
	}
	if token := os.Getenv("TETHER_TOKEN"); token != "" {
		return client.New(addr, client.WithToken(token))
	}
	return client.New(addr, client.WithTokenFileDefault(filepath.Join(filepath.Dir(expandCatalogPath()), "run", "operator.token")))
}
