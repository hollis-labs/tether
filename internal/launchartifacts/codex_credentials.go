package launchartifacts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hollis-labs/substrate/harness/agentlaunch"
	"github.com/hollis-labs/substrate/harness/agentlaunch/matrix"
	providerplant "github.com/hollis-labs/substrate/harness/agentlaunch/planting"
	"github.com/hollis-labs/substrate/harness/workspace"
	"github.com/hollis-labs/substrate/harness/workspace/credentials"
	"github.com/hollis-labs/substrate/harness/workspace/credentials/localfs"
	"github.com/hollis-labs/substrate/harness/workspace/effects"
	"github.com/hollis-labs/substrate/harness/workspace/local"
	"github.com/hollis-labs/substrate/harness/workspace/materialize"
	"github.com/hollis-labs/substrate/harness/workspace/materialize/artifact"
)

// CodexHome is an immutable, explicit pre-redirect capture, never an ambient
// discovery result. Only directory identity and nonsecret authority metadata
// are retained. The auth file may be atomically replaced during reauthentication.
type CodexHome struct {
	path, captureID string
	directory       *os.File
}

// CaptureCodexHome accepts the trusted host's explicit CODEX_HOME input. It
// refuses missing/noncanonical homes and absent, symlink or nonregular auth
// sources without opening or reading credential contents.
func CaptureCodexHome(path string) (*CodexHome, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || !filepath.IsAbs(path) || path != filepath.Clean(path) || path != canonical {
		return nil, refusal("codex_explicit_provider_home_required")
	}
	f, err := openDirectory(path)
	if err != nil {
		return nil, refusal("codex_provider_home_unavailable")
	}
	h := &CodexHome{path: path, captureID: uuid.NewString(), directory: f}
	if err := h.validate(); err != nil {
		_ = h.Close()
		return nil, err
	}
	return h, nil
}

func (h *CodexHome) validate() error {
	if h == nil || h.directory == nil {
		return refusal("codex_provider_home_uncaptured")
	}
	held, err := h.directory.Stat()
	if err != nil {
		return refusal("codex_provider_home_custody_changed")
	}
	current, err := os.Lstat(h.path)
	if err != nil || !current.IsDir() || !os.SameFile(held, current) {
		return refusal("codex_provider_home_custody_changed")
	}
	canonical, err := filepath.EvalSymlinks(h.path)
	if err != nil || canonical != h.path {
		return refusal("codex_provider_home_custody_changed")
	}
	info, err := os.Lstat(filepath.Join(h.path, "auth.json"))
	if err != nil || !info.Mode().IsRegular() {
		return refusal("codex_existing_auth_source_required")
	}
	return nil
}

func (h *CodexHome) Close() error {
	if h == nil || h.directory == nil {
		return nil
	}
	err := h.directory.Close()
	h.directory = nil
	return err
}

// PlantCodex implements DEC-036's single mapping. Artifacts still use the sole
// shared managed-tree engine; credentials use the typed link leaf with real
// held parent/candidate locks and that engine's same operation receipt store.
// No credential file is overwritten, copied or rendered as a managed artifact.
func (c *Custody) PlantCodex(ctx context.Context, prepared *agentlaunch.PreparedLaunch, home *CodexHome, options ...providerplant.Option) (err error) {
	if c == nil || c.compiled == nil || c.compiled.Plan == nil || prepared == nil || prepared.Compiled != c.compiled || prepared.PlantedBootDir != c.root.Path {
		return refusal("codex_credential_launch_changed")
	}
	descriptor, err := matrix.Lookup(c.compiled.Plan.Provider, c.compiled.Plan.Runtime)
	if err != nil || string(descriptor.ProviderID) != "codex" {
		return refusal("codex_credential_launch_changed")
	}
	if err := home.validate(); err != nil {
		return err
	}
	parent := filepath.Dir(c.root.Path)
	parentFile, err := openDirectory(parent)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, parentFile.Close()) }()
	validate := func(ctx context.Context) error {
		if err := c.validate(ctx); err != nil {
			return err
		}
		if err := home.validate(); err != nil {
			return err
		}
		held, e := parentFile.Stat()
		if e != nil {
			return refusal("codex_candidate_parent_changed")
		}
		current, e := os.Lstat(parent)
		if e != nil || !current.IsDir() || current.Mode().Perm()&0022 != 0 || !os.SameFile(held, current) {
			return refusal("codex_candidate_parent_changed")
		}
		return nil
	}
	if err := validate(ctx); err != nil {
		return err
	}
	resolvedHome, err := credentials.ResolveRealHome(credentials.CapturedProviderHome{
		Provider: "codex", Path: home.path, AllowedBase: home.path,
		Provenance: "tether.explicit-CODEX_HOME:DEC-036", CaptureID: home.captureID,
		Revision: home.captureID, BeforeRedirect: true, PlantedRoots: []string{c.root.Path},
	}, credentials.HomeObservations{CanonicalHome: home.path, CanonicalBase: home.path, CanonicalPlantedRoots: []string{c.root.Path}})
	if err != nil {
		return refusal("codex_captured_home_refused")
	}
	group := credentials.Group{Layer: credentials.BootLayer, Home: resolvedHome,
		Candidate: effects.RootInput{ID: c.root.ID, Path: c.root.Path, AllowedBase: parent,
			Owner: c.root.Owner, Provenance: c.root.Provenance, MutationIdentity: parent, Inactive: true, PrivateCustody: true},
		Bindings: []credentials.Binding{{Source: "auth.json", Destination: "auth.json", Required: true, SourceRead: true,
			AuthorizationID: "DEC-036:14122:source-read:" + c.admission.OperationID, AuthorizationVersion: "1",
			SourceWrite: true, SourceWriteAuthorizationID: "DEC-036:14122:refresh-source-write:" + c.admission.OperationID,
			SourceWriteAuthorizationVersion: "1"}},
	}
	// Freeze the entire typed read/write mapping into the artifact operation's
	// existing grant binding, without extending that grant beyond managed files.
	groupDigest, err := Digest(group)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.claimed || c.closed {
		c.mu.Unlock()
		return refusal("artifact_custody_unavailable")
	}
	c.resources.Grants[0].AuthorizationID += ":credential-group:" + groupDigest
	c.mu.Unlock()
	authority, err := c.Authorize(ctx, c.root.Path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, authority.Close()) }()
	if err := agentlaunch.ValidateArtifactAuthority(ctx, authority, c.root.Path); err != nil {
		return err
	}
	execution, err := providerplant.ProjectExecution(ctx, prepared, options...)
	if err != nil {
		return err
	}
	if execution.Roots.BootRoot != c.root.Path || execution.Bindings.Env["CODEX_HOME"].Value != c.root.Path || execution.Bindings.ConfigRoot != c.root.Path {
		return refusal("codex_credential_destination_changed")
	}
	if len(execution.Effects) != 1 || execution.Effects[0].Kind != agentlaunch.RuntimeEffectCredential ||
		execution.Effects[0].ProviderEffect != "codex-auth-json" || execution.Effects[0].Destination != "auth.json" {
		return refusal("codex_credential_effect_changed")
	}
	tree, err := codexArtifactTree(execution.Artifacts)
	if err != nil {
		return err
	}

	// This second port view grants no effects or directory creation. It exists
	// solely to acquire the credential leaf's required containing-directory lock.
	parentRef := workspace.RootRef{ID: "boot-parent:" + c.admission.OperationID, Path: parent,
		AllowedBase: filepath.Dir(parent), Owner: c.root.Owner, Provenance: c.root.Provenance}
	lockResources := workspace.Resources{Roots: []workspace.RootRef{parentRef}, LockRoot: c.control, LockNamespace: c.control.Path,
		Capabilities: slices.Clone(c.resources.Capabilities)}
	parentPorts, closeParent, err := local.New(local.Options{OperationID: c.admission.OperationID, ControlRoot: c.control,
		Resources: lockResources, LocalFilesystem: c.admission.LocalFilesystem,
		ValidateAuthority: func(ctx context.Context, spec workspace.Spec, _ workspace.Resources) error {
			if spec.OperationID != c.admission.OperationID || len(spec.Effects) != 0 {
				return refusal("codex_parent_lock_authority_changed")
			}
			return validate(ctx)
		}, Evidence: func(ctx context.Context) (workspace.Observations, error) {
			if err := validate(ctx); err != nil {
				return workspace.Observations{}, err
			}
			now := time.Now().UTC()
			observed := workspace.Observations{At: now, ExpiresAt: now.Add(time.Minute), Capabilities: slices.Clone(lockResources.Capabilities)}
			for _, ref := range []workspace.RootRef{parentRef, c.control} {
				root, err := workspace.InspectRoot(ref)
				if err != nil {
					return workspace.Observations{}, err
				}
				observed.Roots = append(observed.Roots, root)
			}
			return observed, nil
		}})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeParent()) }()
	parentKey := workspace.LockKey{Namespace: c.control.Path, CanonicalID: parent}
	parentLock, err := parentPorts.Locks.Acquire(ctx, parentKey)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, parentLock.Release()) }()
	rootKey := workspace.LockKey{Namespace: c.control.Path, CanonicalID: c.root.Path}
	rootLock, err := authority.Ports.Locks.Acquire(ctx, rootKey)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rootLock.Release()) }()

	linkPort := localfs.New()
	group.Header = effects.Header{Version: effects.SchemaVersion, OperationID: c.admission.OperationID, InputDigest: groupDigest}
	preflight := effects.PreflightContext{Header: group.Header,
		HeldLocks: []effects.LockIdentity{{Namespace: parentKey.Namespace, CanonicalID: parentKey.CanonicalID}, {Namespace: rootKey.Namespace, CanonicalID: rootKey.CanonicalID}}, Validate: validate}
	if err := validate(ctx); err != nil {
		return err
	}
	if _, result := credentials.Preflight(ctx, group, preflight, linkPort); result.Outcome != effects.Prepared {
		return credentialFailure(result)
	}
	// Borrow the actually held candidate lock for ApplyTree. The engine releases
	// only the borrow; both original locks stay held through credential receipts.
	borrowed := authority
	borrowed.Close = nil
	borrowed.Ports.Locks = heldCandidateLock{key: rootKey, validate: validate}
	borrowed.Input.CredentialDestinations = []string{"auth.json"}
	result, err := agentlaunch.MaterializeArtifactsResult(ctx, agentlaunch.ArtifactMaterializationRequest{
		TargetRoot: c.root.Path, Roots: execution.Roots, Artifacts: tree, Operation: materialize.OperationReconcile,
		Generation: prepared.Compiled.Provenance.PlanHash,
		Authorize: func(ctx context.Context, target string) (agentlaunch.ArtifactAuthority, error) {
			if target != c.root.Path {
				return agentlaunch.ArtifactAuthority{}, refusal("codex_credential_destination_changed")
			}
			if err := validate(ctx); err != nil {
				return agentlaunch.ArtifactAuthority{}, err
			}
			return borrowed, nil
		}})
	if err != nil {
		return err
	}
	if !result.ArtifactsComplete() || len(result.Handles) != 1 || len(result.Receipt.Roots) != 1 {
		return refusal("codex_artifact_completion_unproved")
	}
	// The concrete engine replaces the allocator's empty directory with its
	// verified staged tree. Transfer the held inode only from this earned result,
	// under both locks, before the credential leaf observes the committed root.
	if err := c.acceptMaterializedRoot(ctx, result); err != nil {
		return err
	}
	// The committed artifact input digest includes the frozen group binding.
	// Re-preflight that same mapping against the earned operation receipt before
	// linking; neither a synthetic generation nor a caller-built receipt suffices.
	group.Header.InputDigest = result.Receipt.InputDigest
	preflight.Header = group.Header
	if err := validate(ctx); err != nil {
		return err
	}
	credentialPlan, inspected := credentials.Preflight(ctx, group, preflight, linkPort)
	if inspected.Outcome != effects.Prepared {
		return credentialFailure(inspected)
	}
	sink := &codexCredentialSink{store: authority.Ports.ReceiptStore, receipt: result.Receipt, header: group.Header, rootID: c.root.ID, binding: group.Bindings[0], target: filepath.Join(home.path, "auth.json")}
	linked := credentials.Apply(ctx, credentialPlan, effects.ApplyContext{PreflightContext: preflight,
		ArtifactRootID: c.root.ID, ArtifactGeneration: result.Receipt.Roots[0].Generation, Receipts: sink}, linkPort)
	if linked.Outcome != effects.Applied && linked.Outcome != effects.AlreadyPresent {
		return credentialFailure(linked)
	}
	if err := validate(ctx); err != nil {
		return err
	}
	// This records artifact/credential completion only, never launch readiness.
	sink.receipt.Phase = workspace.ArtifactsCommitted
	if err := sink.store.Record(ctx, sink.receipt); err != nil {
		return err
	}
	prepared.Argv = slices.Clone(execution.Bindings.Argv)
	prepared.Launch = execution.Bindings.Launch
	prepared.Env = make(map[string]string, len(execution.Bindings.Env))
	for k, v := range execution.Bindings.Env {
		prepared.Env[k] = v.Value
	}
	prepared.Workdir = execution.Bindings.CWD
	return nil
}

// Only the provider's exact empty 0600 auth slot is separated into the approved
// link effect. Overlays, alternate destinations and credential payloads refuse.
func codexArtifactTree(tree artifact.Tree) (artifact.Tree, error) {
	out := tree
	out.Entries = nil
	found := false
	for _, entry := range tree.Entries {
		if entry.Path == "auth.json" {
			if found || entry.Kind != artifact.EntryFile || entry.Mode != 0600 || len(entry.Bytes) != 0 || entry.ContentRef != nil ||
				entry.Ownership.EntryID != "provider:codex:auth.json" || !strings.HasPrefix(entry.Ownership.GroupID, "provider:codex:") ||
				entry.Provenance.Source != "go-providers" || entry.Provenance.SourcePath != "credential-placeholder" {
				return artifact.Tree{}, refusal("codex_auth_projection_changed")
			}
			found = true
			continue
		}
		out.Entries = append(out.Entries, entry)
	}
	if !found {
		return artifact.Tree{}, refusal("codex_auth_projection_missing")
	}
	return out, workspace.ValidateManagedTree(out, []string{"auth.json"})
}

type heldCandidateLock struct {
	key      workspace.LockKey
	validate func(context.Context) error
}
type borrowedLock struct{}

func (borrowedLock) Release() error { return nil }
func (h heldCandidateLock) Acquire(ctx context.Context, key workspace.LockKey) (workspace.HeldLock, error) {
	if key != h.key {
		return nil, refusal("codex_held_lock_changed")
	}
	if err := h.validate(ctx); err != nil {
		return nil, err
	}
	return borrowedLock{}, nil
}

// A narrow view of the very same durable operation receipt store. Every link
// intent/progress record is bound to the earned artifact digest and exact slot.
type codexCredentialSink struct {
	store   workspace.ReceiptStore
	receipt workspace.Receipt
	header  effects.Header
	rootID  string
	binding credentials.Binding
	target  string
}

func (s *codexCredentialSink) Record(ctx context.Context, evidence effects.Evidence) error {
	if evidence.Header != s.header || evidence.Kind != effects.CredentialLinks || evidence.RootID != s.rootID || len(evidence.Trust) != 0 || len(evidence.Attachments) != 0 || len(evidence.Links) != 1 {
		return refusal("codex_credential_receipt_changed")
	}
	for _, link := range evidence.Links {
		if link.Destination != s.binding.Destination || link.Source != s.target || link.Target != s.target || link.AuthorizationID != s.binding.AuthorizationID || link.AuthorizationVersion != s.binding.AuthorizationVersion {
			return refusal("codex_credential_receipt_changed")
		}
	}
	next := s.receipt
	next.Phase = workspace.Interrupted
	next.EffectEvidence = append(slices.Clone(s.receipt.EffectEvidence), evidence.Clone())
	if err := s.store.Record(ctx, next); err != nil {
		return err
	}
	s.receipt = next
	return nil
}

func credentialFailure(result effects.Result) error {
	return &workspace.Refusal{Code: "codex_credential_" + result.Code, Concern: "tether.codex-credentials", Status: workspace.Partial}
}

func (c *Custody) acceptMaterializedRoot(ctx context.Context, result workspace.ApplyResult) error {
	if !result.ArtifactsComplete() || result.Receipt.OperationID != c.admission.OperationID || len(result.Receipt.Roots) != 1 ||
		result.Receipt.Roots[0].Root != c.root || result.Receipt.Roots[0].Generation != c.compiled.Provenance.PlanHash ||
		len(result.Handles) != 1 || result.Handles[0].TargetRoot != c.root.Path {
		return refusal("codex_artifact_completion_unproved")
	}
	observed, err := workspace.InspectRoot(c.root)
	if err != nil || observed.Manifest == nil {
		return refusal("codex_committed_root_unverified")
	}
	actual, err := Digest(observed.Manifest)
	if err != nil {
		return err
	}
	expected, err := Digest(result.Handles[0].Manifest)
	if err != nil || actual != expected {
		return refusal("codex_committed_root_unverified")
	}
	next, err := openDirectory(c.root.Path)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || !c.claimed {
		_ = next.Close()
		return refusal("artifact_custody_unavailable")
	}
	prior := c.rootFile
	c.rootFile = next
	if err := c.validate(ctx); err != nil {
		c.rootFile = prior
		_ = next.Close()
		return err
	}
	return prior.Close()
}
