# S5 — Tether launch corpus (LIVE var sources)

This directory is the **S5-cutover** parameterized re-expression of the
legacy `~/.tether/catalog/launches/` (64 files) and
`~/.tether/catalog/boot-profiles/` (55 non-`.bak` files) directories.

It is the in-repo sibling of the S4.4 reference corpus in
`libs/go-agent-launch/agentlaunch/testdata/specs/`. The S4.4 corpus uses
LITERAL placeholder var sources so its spec renders standalone in unit
tests. **This corpus carries the LIVE var sources** — `file`, `cmd`, and
`call` — mapped one-to-one from the legacy boot-profile `slots:` block.
It is the input to the S4.5 parity harness.

It does **not** replace or mutate the live catalog — `~/.tether/catalog/`
is untouched. This is a parallel, additive artifact set.

## Layout

```
launch-assembly.yaml     The ONE canonical LaunchSpec, with LIVE var
                         sources. Every legacy launch collapses to a bag
                         handed to this spec. Folds the boot-profile
                         slots into vars + a merge-tag template.

templates/               Common-setup templates: the recurring
                         runner + isolation combinations, named once.

launches/                One LaunchBag per concrete legacy launch — 64
                         bags reproducing every ~/.tether/catalog/
                         launches/*.yaml, plus tether-minimum.yaml (the
                         minimum-config demo bag).
```

## What collapsed

| Legacy | New model |
|---|---|
| 64 `launches/*.yaml` (2 modes x N runners x M projects) | 1 `LaunchSpec` + 64 `LaunchBag` files |
| `<launch>.worktree` TWIN file per launch | `isolation` input value — **twins deleted** |
| `<project>-<provider>` launch file per provider | `runner` input value (D3: provider feeds runtime-binding) |
| `boot-profiles/*.yaml` `slots:` block | `vars:` (LIVE sources) + merge-tag `template:` body |

## Live var-source mapping (boot-profile slot -> S4.2 VarSpec)

| Slot | Legacy slot type | S4.2 var source kind | Live source |
|---|---|---|---|
| `agent`   | `role_summary` | `file` | `~/.nanite/roles/{{ inputs.agent_role }}.md` |
| `recap`   | `http`         | `call` (http) | `${TESSERACT_URL}/v1/recall` — session-close namespace, project tags |
| `history` | `cmd`          | `cmd`  | `git -C {{ inputs.work_dir }} log --oneline -15` |
| `status`  | `cmd`          | `cmd`  | `git status --short -b` (workdir = `{{ inputs.work_dir }}`) |
| `memory`  | `http`         | `call` (http) | `${TESSERACT_URL}/v1/recall` — memory namespace, project tags |
| `skills`  | `skill_index`  | `cmd`  | `mux skills index ...` — see GAP below |

### `skills` GAP

S4.2's `VarSourceKind` union is `literal | file | call | cmd` only —
there is **no native `skill_index` source kind**. The legacy
`skill_index` slot ran an in-process layered-discovery pass with
profile-aware ranking (`internal/bootgen/profile.go` `resolveSkillIndex`
-> `skills.DiscoverLayered`). No off-the-shelf S4.2 source reproduces the
ranking. The closest faithful S4.2 source is `cmd`: this corpus models
the slot as a `cmd` source invoking a `mux skills index` subcommand. The
var name and template wiring are preserved; the ranking-fidelity gap is
a known limitation surfaced for S5.

## Minimum valid config

Two knobs: `work_dir` + `runner`. `isolation` and `bus` default;
everything else is a defaulted convenience input.
`launches/tether-minimum.yaml` is the smallest legal bag.

## Loading

`agentlaunch.LoadLaunchSpec`, `agentlaunch.LoadLaunchBag`, and
`agentlaunch.ValidateMinimumConfig` parse/validate these files. The
corpus validation test (`internal/launchspec_corpus_test.go`) walks the
whole tree through exactly those entry points; the S4.5 parity harness
drives the same.

## Out of scope

`projects/` and `sandbox-profiles/` are NOT re-expressed here — they have
no registry kind (a known, deferred follow-up). Only `launches/` +
`boot-profiles/` are in scope.
