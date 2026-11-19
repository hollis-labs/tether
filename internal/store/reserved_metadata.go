package store

import (
	"context"
	"strings"
)

// OperatorAttestationKey is the reply metadata that records how an
// operator-attested reply was proven (CW-20261114-0152). It, and every key
// under its prefix, is reserved: only the attested-reply path may store it.
const OperatorAttestationKey = "operator_attestation"

// IsOperatorAttestationKey reports whether k is reserved for the attested-reply
// path (any case, any suffix).
func IsOperatorAttestationKey(k string) bool {
	return strings.HasPrefix(strings.ToLower(k), OperatorAttestationKey)
}

type attestedReplyKey struct{}

// WithAttestedReply marks ctx as the attested-reply path: the only sends
// whose operator_attestation metadata is kept. Every other send has it
// dropped here, whichever ingress it came through (#586 security review).
func WithAttestedReply(ctx context.Context) context.Context {
	return context.WithValue(ctx, attestedReplyKey{}, true)
}

// withoutForgedAttestation returns meta without operator_attestation keys
// unless ctx is the attested-reply path. It copies rather than edits meta.
func withoutForgedAttestation(ctx context.Context, meta map[string]string) map[string]string {
	if ok, _ := ctx.Value(attestedReplyKey{}).(bool); ok {
		return meta
	}
	var out map[string]string
	for k := range meta {
		if IsOperatorAttestationKey(k) {
			if out == nil {
				out = make(map[string]string, len(meta))
				for kk, vv := range meta {
					out[kk] = vv
				}
			}
			delete(out, k)
		}
	}
	if out == nil {
		return meta
	}
	return out
}
