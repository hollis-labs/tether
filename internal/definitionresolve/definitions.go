package definitionresolve

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/agentdef"
	"github.com/hollis-labs/tether/internal/fabricstore"
)

var ErrPinMismatch = errors.New("definition pin mismatch")
var ErrConfiguration = errors.New("definition resolver configuration is incomplete")

type ExtensionKey struct {
	Namespace string
	Version   string
}

// ExtensionHandler validates an owned schema and enumerates its typed content
// references. Merely declaring a namespace supported is not enough. Unknown
// optional extensions remain uninterpreted and cannot grant executable semantics.
type ExtensionHandler struct {
	Validate func(agentdef.Extension) error
	Refs     func(agentdef.Extension) ([]agentdef.Ref, error)
}
type Policy struct {
	KnownCapability func(string) bool
	Extensions      map[ExtensionKey]ExtensionHandler
}
type DefinitionStore struct {
	repository *fabricstore.Repository
	content    ContentProvider
	policy     Policy
}

func NewDefinitionStore(repository *fabricstore.Repository, content ContentProvider, policy Policy) (*DefinitionStore, error) {
	if repository == nil || content == nil || policy.KnownCapability == nil {
		return nil, ErrConfiguration
	}
	copyPolicy := Policy{KnownCapability: policy.KnownCapability, Extensions: map[ExtensionKey]ExtensionHandler{}}
	for key, handler := range policy.Extensions {
		if key.Namespace == "" || key.Version == "" || handler.Validate == nil || handler.Refs == nil {
			return nil, ErrConfiguration
		}
		copyPolicy.Extensions[key] = handler
	}
	return &DefinitionStore{repository: repository, content: content, policy: copyPolicy}, nil
}
func (s *DefinitionStore) options() []agentdef.Option {
	return []agentdef.Option{agentdef.WithCapabilities(s.policy.KnownCapability), agentdef.WithExtensions(func(namespace, version string) (func(agentdef.Extension) error, bool) {
		handler, ok := s.policy.Extensions[ExtensionKey{namespace, version}]
		return handler.Validate, ok
	})}
}

type VerifiedDefinition struct {
	Pin            mesh.DefinitionRef
	ArtifactDigest string
	Definition     *agentdef.Definition
}

func (s *DefinitionStore) parse(ctx context.Context, source string) (VerifiedDefinition, error) {
	if err := ctx.Err(); err != nil {
		return VerifiedDefinition{}, err
	}
	raw, err := s.content.ReadDefinition(ctx, source)
	if err != nil {
		return VerifiedDefinition{}, err
	}
	definition, err := agentdef.Parse(raw, s.options()...)
	if err != nil {
		return VerifiedDefinition{}, fmt.Errorf("%w: %w", ErrContent, err)
	}
	digest, err := agentdef.Digest(definition)
	if err != nil {
		return VerifiedDefinition{}, fmt.Errorf("%w: %w", ErrContent, err)
	}
	if err := s.verifyRefs(ctx, definition); err != nil {
		return VerifiedDefinition{}, err
	}
	if err := ctx.Err(); err != nil {
		return VerifiedDefinition{}, err
	}
	return VerifiedDefinition{Pin: mesh.DefinitionRef{ID: definition.DefinitionID, Revision: definition.Revision, Digest: digest}, ArtifactDigest: agentdef.ArtifactDigest(raw), Definition: definition}, nil
}

// Index verifies content before reserving the writer. Identical retries are
// idempotent; an existing ID/revision cannot acquire a different semantic digest.
func (s *DefinitionStore) Index(ctx context.Context, source string) (VerifiedDefinition, error) {
	verified, err := s.parse(ctx, source)
	if err != nil {
		return VerifiedDefinition{}, err
	}
	err = s.repository.Write(ctx, func(tx *fabricstore.Tx) error {
		revision, err := tx.Definition(verified.Pin.ID, verified.Pin.Revision)
		switch {
		case errors.Is(err, fabricstore.ErrNotFound):
			if err := tx.AddDefinition(fabricstore.DefinitionRevision{Definition: verified.Pin, SourceRef: source}); err != nil {
				return err
			}
		case err != nil:
			return err
		case revision.Value.Definition != verified.Pin:
			return ErrPinMismatch
		}
		artifact, err := tx.Artifact(verified.Pin, verified.ArtifactDigest)
		record := fabricstore.DefinitionArtifact{Definition: verified.Pin, Digest: verified.ArtifactDigest, SourceRef: source, VerificationRef: "agentdef:2/" + verified.ArtifactDigest}
		if errors.Is(err, fabricstore.ErrNotFound) {
			return tx.AddArtifact(record)
		}
		if err != nil {
			return err
		}
		if artifact.Value != record {
			return ErrPinMismatch
		}
		return nil
	})
	if err != nil {
		return VerifiedDefinition{}, err
	}
	return verified, nil
}

// Load verifies the original indexed source every time. It never follows latest,
// silently indexes changed bytes or falls back to another location.
func (s *DefinitionStore) Load(ctx context.Context, pin mesh.DefinitionRef) (VerifiedDefinition, error) {
	if pin.ID == "" || pin.Revision == "" || pin.Digest == "" {
		return VerifiedDefinition{}, ErrPinMismatch
	}
	revision, err := s.repository.Definition(ctx, pin.ID, pin.Revision)
	if err != nil {
		return VerifiedDefinition{}, err
	}
	if revision.Value.Definition != pin {
		return VerifiedDefinition{}, ErrPinMismatch
	}
	verified, err := s.parse(ctx, revision.Value.SourceRef)
	if err != nil {
		return VerifiedDefinition{}, err
	}
	if verified.Pin != pin {
		return VerifiedDefinition{}, ErrPinMismatch
	}
	artifact, err := s.repository.Artifact(ctx, pin, verified.ArtifactDigest)
	if err != nil {
		return VerifiedDefinition{}, err
	}
	if artifact.Value.Definition != pin || artifact.Value.Digest != verified.ArtifactDigest || artifact.Value.SourceRef != revision.Value.SourceRef || artifact.Value.VerificationRef != "agentdef:2/"+verified.ArtifactDigest {
		return VerifiedDefinition{}, ErrPinMismatch
	}
	return verified, nil
}

type namedRef struct {
	field string
	ref   agentdef.Ref
	tree  bool
}

func (s *DefinitionStore) verifyRefs(ctx context.Context, d *agentdef.Definition) error {
	refs := []namedRef{}
	add := func(field string, list []agentdef.Ref) {
		for i, ref := range list {
			refs = append(refs, namedRef{field: fmt.Sprintf("%s[%d]", field, i), ref: ref})
		}
	}
	pointer := func(field string, ref *agentdef.Ref) {
		if ref != nil {
			refs = append(refs, namedRef{field: field, ref: *ref})
		}
	}
	add("behavior.instructions", d.Behavior.Instructions)
	add("behavior.sops", d.Behavior.SOPs)
	for i, capability := range d.Capabilities {
		pointer(fmt.Sprintf("capabilities[%d].input", i), capability.Input)
		pointer(fmt.Sprintf("capabilities[%d].output", i), capability.Output)
	}
	for i, skill := range d.Requirements.Skills {
		refs = append(refs, namedRef{field: fmt.Sprintf("requirements.skills[%d].content", i), ref: skill.Content, tree: true})
	}
	add("requirements.resources", d.Requirements.Resources)
	add("harness_profile.steering", d.HarnessProfile.Steering)
	add("harness_profile.context.sources", d.HarnessProfile.Context.Sources)
	pointer("harness_profile.context.policy", d.HarnessProfile.Context.Policy)
	add("harness_profile.approvals", d.HarnessProfile.Approvals)
	add("harness_profile.escalation", d.HarnessProfile.Escalation)
	pointer("continuity.memory_policy", d.Continuity.MemoryPolicy)
	pointer("continuity.recovery_strategy", d.Continuity.RecoveryStrategy)
	keys := make([]string, 0, len(d.Extensions))
	for key := range d.Extensions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, namespace := range keys {
		extension := d.Extensions[namespace]
		handler, ok := s.policy.Extensions[ExtensionKey{namespace, extension.Version}]
		if !ok {
			continue
		}
		extra, err := handler.Refs(extension)
		if err != nil {
			return err
		}
		add("extensions["+namespace+"]", extra)
	}
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return err
		}
		pin, err := s.content.Pin(ctx, ref.ref.URI)
		if err != nil {
			return fmt.Errorf("%s: %w", ref.field, err)
		}
		if pin.Digest != ref.ref.Digest || (pin.Kind != FileContent && pin.Kind != TreeContent) || (ref.tree && pin.Kind != TreeContent) {
			return fmt.Errorf("%w: %s", ErrPinMismatch, ref.field)
		}
	}
	return nil
}
