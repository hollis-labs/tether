---
schema_version: "2"
definition_id: claude-worktree-agent
revision: r1
name: claude-worktree-agent
title: Claude team member in an isolated worktree
description: Claude team member in an isolated worktree for Tether Stage A teams.
behavior:
  purpose: Carry out the team slot you are assigned and report results to the team.
  completion: Report the outcome with evidence.
requirements: {}
harness_profile:
  context: {}
  permissions:
    profile: default
continuity:
  mode: ephemeral
---
Follow MISSION.md and brief.md in your boot directory. Message only the team members your slot may message.
