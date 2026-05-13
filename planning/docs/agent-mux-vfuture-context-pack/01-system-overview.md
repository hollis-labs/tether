# System Overview

## Problem being solved

The user is building multiple agent-first systems and wants a single runtime substrate for launching, isolating, managing, and interacting with agent sessions.

Current pain points include:

- remembering where agents, prompts, and configs live
- managing multiple provider launch modes
- session collision and workspace isolation
- lack of durable attach/detach semantics
- difficulty coordinating multiple sibling sessions
- desire for warm, reusable, long-running logical agents
- desire to avoid vendor CLI lock-in by also supporting API-backed runtimes
- confusion between runtime/session concerns and asset/install/catalog concerns

## Core idea

Agent Mux becomes the **local runtime and session control plane**.

Other apps and surfaces become clients:

- CLI
- TUI
- HTTP/local RPC
- MCP
- Nanite
- Clockwork
- future desktop GUI

## What Agent Mux is

- runtime host
- session registry
- PTY/API runtime manager
- attach/detach host
- workspace materializer
- sandbox/policy wrapper
- broker/mailbox owner
- checkpoint/handoff owner
- event source

## What Agent Mux is not

- primary work planner
- sprint/task scheduler
- chat UI
- global asset installer/catalog owner
- memory database itself
- service manager for all system daemons

## Adjacent systems

### Nanite
Interactive operator console and chat UX.

### Clockwork
Project management, scheduling, queue policies, escalation, logical-agent work policies.

### Agent Ops
Manages agent assets, skills, tools, providers, installers, registries, and format adapters.

### Vanta memory
Shared or embedded memory primitive. Supports service mode and embedded library mode. Namespaced memory scopes already conceptually align with this architecture.

### Cerberus
Service/process manager for apps, dev servers, builds, and deployment/devops concerns. It may later manage Agent Mux itself as a system service, but it should not absorb Agent Mux runtime semantics.

## Design rule

The UI is always a client of the runtime, never the source of runtime truth.
