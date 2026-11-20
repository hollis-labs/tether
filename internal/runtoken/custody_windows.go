package runtoken

import (
	"crypto/ed25519"
	"errors"
)

var ErrCustody = errors.New("private run signing key custody unavailable")

type Authority struct{}

func (*Authority) Key(string) (ed25519.PrivateKey, error) { return nil, ErrCustody }
