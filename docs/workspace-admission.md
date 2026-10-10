# Workspace launch admission

External runtimes receive a daemon-generated `TMPDIR` beneath the canonical
session workspace. The runtime environment contains this binding; the accepted
launch plan, caller environment, `CODEX_HOME`, native resume home and original
work root retain their existing meaning. In-process API runtimes do not allocate
an OS scratch directory.

The allocation root is `.tether-scratch-<operation UUID>`. It is distinct from
older workspace temporaries. An existing directory with that name is refused
unless an admitted receipt for the same operation and physical identity exists.
A reserved partial allocation cannot adopt an existing directory, even if empty.

## Admission phases

1. The canonical session launch gate captures a fresh holder token. A closure
   from a previous acquisition cannot act through the same gate pointer after
   release or reassignment.
2. Before preparation, Tether captures the full session row and exact stored
   launch-plan JSON. Unknown JSON fields remain part of the predicate.
3. Successful preparation may record only its computed `ref_attribution` and
   `native_state_root`. The transaction compares the original full row, including
   `updated_at`, and raw JSON. It returns the exact resulting snapshot. Attribution
   remains best effort; native-state recording remains mandatory. An admitted
   retry retains its original snapshot and refuses changed computed stamps.
4. The allocation receipt binds the canonical session, operation UUID, snapshot
   digests and generated root. Exclusive creation and held directory handles
   establish the observed parent/child identity. A reserved-to-admitted CAS checks
   the unchanged decision and physical custody. Its filesystem callback never
   reenters Store.
5. Runtime entry checks the receipt, exact plan, permitted launching transition,
   holder token, physical identity, permissions and protected roots. Hosted
   placement uses a separately fenced created-to-launching transition and checks
   its exact returned timestamp and snapshot after preparation callbacks,
   immediately before `placeProvider`.

Trusted daemon control paths may use configured symlink aliases. Admission
captures their resolved physical identity and retains the configured name for
revalidation; retargeting the alias refuses. Scratch parents and children retain
their separate no-symlink requirement. An unrelated project layer beneath a
regular file cannot exist and is omitted only after the workspace overlap check.

No database transaction spans a provider or host callback. The existing launch
gate spans preparation and Start; it is not extended through execution. The
checks do not make filesystem paths immutable against noncooperating processes.
An external actor can still mutate a path after the final observation, including
inside a provider callback. Held handles preserve the observed directories but
do not convert a pathname into an atomic provider execution capability.

## Retention and completion

The Service retains scratch handles under the exact session and allocation
operation. A retry cannot overwrite an existing custody entry. The runtime
wrapper records actual entry into the underlying `Runtime.Start` and returns
the original Session, including optional RPC interfaces.

A confirmed pre-entry refusal with no tracked shim placement releases handles.
An entered Start failure is unknown and retains handles and its receipt. A
tracked placement or lookup error also retains custody. A successful direct
execution releases handles only after the existing Manager's authoritative
`WaitSession` completes; no second `Session.Wait` is introduced. Observer
cancellation and missing results are not completion. Hosted bridge completion
does not establish the provider child's completion, so tracked shim custody
remains retained.

`Store.WorkspaceScratchAllocation` exposes retained receipt facts for exact
inspection. The private Service map preserves live handles; there is no public
reconciliation or deletion command in this change. Reconciliation requires
matching the operation and resource identities with an authoritative provider
completion receipt. A process exit loses in-memory handles, while allocation
records and directories remain. Unknown partials continue to refuse adoption.
There is no automatic stop, reaper, deletion, death proof or Ready permission.

## Branch templates and remaining scope

Worktree branches accept literal names and the supported `ProjectID`,
`SessionID` and `LogicalAgentID` substitutions. Rendering and Git ref validation
happen before directory allocation or Git worktree creation. Missing inputs,
unknown variables, malformed templates and invalid refs explicitly refuse.
An explicitly empty branch retains the existing detached-worktree behavior.

This is bounded CW-20261003-0009 coverage. It does not complete general per-launch
cwd selection, multirepo and extra-directory admission, provider trust setup,
Torque task-bundle contents, declared output admission, broader worktree
reattachment proof, or adoption of a separate worktree library. Scratch
allocation alone does not prove that a provider confinement policy permits all
required writes. Strict native resume and credential continuity retain their
existing separate checks. Team cleanup parity remains a separate unfinished
CW-20261003-0017 requirement.
