package teamspolicy

import "github.com/hollis-labs/substrate/mesh/agentdef"

// PinOfBytes parses an authored agent definition and returns its exact pin
// (id, revision, semantic digest). It is offline: it does not resolve pinned
// content references.
func PinOfBytes(raw []byte) (id, revision, digest string, err error) {
	def, err := agentdef.Parse(raw, agentdef.WithCapabilities(KnownCapability))
	if err != nil {
		return "", "", "", err
	}
	digest, err = agentdef.Digest(def)
	if err != nil {
		return "", "", "", err
	}
	return def.DefinitionID, def.Revision, digest, nil
}
