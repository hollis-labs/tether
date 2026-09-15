# Compositional Launch Resolution and Precedence Rules

**Status:** Approved & Implemented (CW-20260904-0092)  
**Package:** `internal/launchprofile`  

---

## 1. Overview and Problem Statement

Prior to this architecture, Tether's launch configuration suffered from a combinatorial cross-product explosion across projects, providers, and workspace modes. Catalog directories contained:
- **`launches/` (64+ files):** Every combination of `(Project × Provider × WorkspaceMode)` was authored as a separate launch YAML file (e.g. `agent-mux-claude-stream-worktree.yaml`, `agent-mux-claude-stream-direct.yaml`, `agent-mux-codex-worktree.yaml`).
- **`boot-profiles/` (62+ files):** Because Tether profiles lacked inheritance (`extends`), every combination restated all slots, roles, and guidelines in full.

### Root Cause
Project scope, provider harness, and workspace mode were treated as **identity coordinates** rather than **launch inputs**.

### The Solution: Adapter + Retire Authoring
1. **Compositional Launch Input:** Scope (`LaunchContext` / repository path), provider harness, and workspace mode become inputs to the launch request, not baked into profile identity.
2. **`Extends` Cascade:** Profiles support inheritance chains (`extends`). Ancestor profiles define shared behaviors and guidelines; descendants customize or add specific skills and overrides.
3. **Pluggable Sources (`launchprofile.Source`):** A clean adapter interface with built-in offline implementations (`FileSource`, `MemorySource`, `CairnBundleSource`, `MultiSource`). Tether operates completely standalone with zero external runtime dependencies on Cairn or external registries.
4. **Deterministic Snapshots:** Every resolved launch produces an immutable `Snapshot` with a deterministic SHA-256 digest (`sha256:<hex>`), persisted with the session to guarantee auditability and reproducible resumes.
5. **Legacy Compatibility:** Existing launch IDs (e.g. `agent-mux-claude-stream-worktree`) resolve seamlessly through `CatalogSource` without breaking historical sessions or API clients.

---

## 2. Architecture and Data Flow

```text
               +----------------------------------+
               |  CompositionInput                |
               |  Target: "backend-engineer"      |
               |  Scope:  "tether"                |
               |  Provider: "claude-code"         |
               |  WorkspaceMode: "worktree"       |
               +----------------+-----------------+
                                |
                                v
               +----------------------------------+
               |  launchprofile.Resolver          |
               |  (walks extends chain on Source) |
               +----------------+-----------------+
                                |
       +------------------------+------------------------+
       |                        |                        |
       v                        v                        v
 [base-profile]       [engineer-profile]       [backend-engineer]
 (root ancestor)        (intermediate)               (leaf)
       |                        |                        |
       +------------------------+------------------------+
                                |
                   (Root-to-Leaf Cascade Fold)
                                |
                                v
               +----------------------------------+
               |  ResolvedComposition             |
               |  - Keyed collections merged      |
               |  - Closest-wins scalars applied  |
               |  - Instructions concatenated     |
               |  - Scope & inputs folded in      |
               +----------------+-----------------+
                                |
                   +------------+------------+
                   |                         |
                   v                         v
        +---------------------+   +---------------------+
        |  Snapshot           |   |  launch.Plan        |
        |  Digest: sha256:... |   |  (persisted to DB)  |
        +---------------------+   +---------------------+
```

---

## 3. Composition and Precedence Rules

Resolution follows a strict 5-tier precedence order:

### Tier 1: Ancestor Profiles (Root-to-Leaf Cascade)
The inheritance chain is resolved leaf-to-root, checked for cycles (`ErrCycle`), and folded root-to-leaf:
- **Scalars (Closest-Wins):** Non-empty strings or declared values in descendant profiles replace ancestor values:
  - `Name`, `Description`, `Provider`, `Model`
  - `Permissions` (`PermissionMode`, `DefaultSandbox`, `Network`)
  - `SystemPrompt`, `AgentPrompt`
- **Keyed Collections (Merged by Key):**
  - `Skills`: Union of all declared skill IDs, preserving order, deduplicated.
  - `Roles`: Union of role tags, deduplicated.
  - `Prompts`: Union of slash-prompt command names, deduplicated.
  - `ContextFiles`: Union of referenced context files, deduplicated.
  - `BootFragments`: Appended in chain order (`root -> descendant -> leaf`).
  - `ProviderOverrides`: Merged by provider ID. If an override for a provider exists in both ancestor and descendant:
    - Nested `Env` maps are merged key-by-key (descendant key wins).
    - `ExtraArgs` are appended and deduplicated.
  - `Env`: Merged key-by-key (descendant variable wins).
  - `Spec`: Keyed collections merged by key, unkeyed collections replaced.
- **Instruction Bodies:** Markdown bodies (`Body`) are concatenated in chain order (`root\n\nparent\n\nleaf`), ensuring foundation rules precede specialized instructions.

### Tier 2: Leaf Profile
The leaf profile represents the concrete target requested. Its declarations take precedence over all ancestor defaults.

### Tier 3: Launch Context / Project Defaults
When a `Scope` or `Context` is supplied:
- `RepoRoot`, `TrackingRoot`, and `WorktreeBase` provide the workspace placement.
- `Workspace.DefaultMode` provides the default workspace mode if not overridden.
- Project `MCP.Servers` are injected into `MUX_MCP_SERVERS` in the environment if not already defined.
- Project `KnowledgeBase` and `BootFragments` are included in prompt assembly.

### Tier 4: Launch-Time Inputs (Request Parameters)
Caller-provided parameters supplied at launch time override profile and context defaults:
- `Provider`: Overrides the profile's declared provider (e.g. launching an agent under `codex` instead of `claude-code`).
- `WorkspaceMode`: Overrides default workspace mode (`worktree`, `direct`, `hybrid`).
- `WorktreeName`: Specifies the exact branch/worktree name.
- `Skills`: Additional ad-hoc skills unioned with profile skills.
- `Prompts`: Additional ad-hoc prompts unioned with profile prompts.

### Tier 5: Explicit Overrides
Highest-precedence parameters applied last:
- `Env`: Explicit key-value environment variables merged over all layers.
- `SystemPromptOverride`: Replaces the composed system prompt.
- `PromptAppend`: Appended to the very end of the final assembled prompt.
- `NativeFiles` & `BootDirOverlay`: Caller file injections applied to the plan.

---

## 4. Deterministic Snapshots and Auditability

To ensure reproducibility across session resumes, crashes, and audit reviews:
- `ResolvedComposition.ComputeDigest()` computes a canonical SHA-256 hash over the resolved configuration.
- `Snapshot` captures `{Digest, ResolvedAt, Composition}`.
- `Plan.Shared.PlanHash` records the digest directly in the persisted `launch_plans` table.
- A session can resume or be audited against its recorded digest to detect any drift in external files or catalog declarations.

---

## 5. Source Adapters (`launchprofile.Source`)

Tether defines a clean interface for profile retrieval:
```go
type Source interface {
    GetProfile(ctx context.Context, id string) (*LaunchProfile, error)
    GetContext(ctx context.Context, id string) (*LaunchContext, error)
}
```

Built-in implementations:
- **`FileSource`:** Reads YAML/JSON profiles and contexts from a directory tree.
- **`MemorySource`:** In-memory source for tests and programmatic definitions.
- **`CairnBundleSource`:** Reads profiles from Cairn bundles (`profiles/*.md` or `profiles/*.yaml` with YAML frontmatter) without requiring Cairn as a runtime dependency.
- **`CatalogSource` (`*config.Catalog`):** Bridges Tether's catalog, automatically resolving legacy launch entries (`launches/*.yaml`) into compositional targets.
- **`MultiSource`:** Chains multiple sources in priority order.

---

## 6. Compatibility & Migration

- **Zero Breaking Changes:** Existing catalog files (`agents/*.yaml`, `projects/*.yaml`, `launches/*.yaml`) continue to work without modification.
- **Legacy Launch IDs:** Calling `Resolve` with an old launch ID (e.g. `agent-mux-claude-stream-worktree`) resolves the underlying agent profile, folds the launch provider and workspace mode, and returns a valid `Plan`.
- **Phased Catalog Deprecation:** New launches do not require creating `launches/*.yaml` files; operators invoke profiles directly with scope and provider arguments.
