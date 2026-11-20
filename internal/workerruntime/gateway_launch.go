package workerruntime

import (
	"context"
	"errors"
	"github.com/hollis-labs/tether/internal/gatewaylaunch"
	"github.com/hollis-labs/tether/internal/runtoken"
	"github.com/hollis-labs/tether/internal/workerwire"
)

func (e *Executor) startGatewayCredential(spec workerwire.LaunchSpec) (string, func(), error) {
	endpoint := spec.Env[gatewaylaunch.URLenv]
	if err := gatewaylaunch.ValidateURL(endpoint); err != nil {
		return "", nil, err
	}
	if e.Tokens == nil {
		return "", nil, errors.New("gateway requires a session agent token grant")
	}
	var scopes []string
	for _, grant := range spec.AgentTokens {
		if grant.Aud == "tether" {
			scopes = grant.Scopes
		}
	}
	if len(scopes) == 0 {
		return "", nil, errors.New("gateway requires a session agent token grant")
	}
	path, stop, err := gatewaylaunch.Start(e.gatewayMint(spec.Session, scopes))
	if err != nil {
		return "", nil, err
	}
	if err := gatewaylaunch.BindRunToken(path, spec.Env[runtoken.TokenEnv]); err != nil {
		stop()
		return "", nil, err
	}
	return path, stop, nil
}

func (e *Executor) gatewayMint(sid string, scopes []string) gatewaylaunch.Mint {
	return func(ctx context.Context) (gatewaylaunch.Credential, error) {
		tok, err := e.Tokens.Mint(ctx, "tether", scopes, sid)
		return gatewaylaunch.Credential{Token: tok.Token, ExpiresAt: tok.ExpiresAt}, err
	}
}
