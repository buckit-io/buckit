# Buckit Manager (`bm`) High-Level Design

## Overview

This document describes the long-term high-level design for `bm`, the Buckit
Manager. The intent is for `bm` to become the primary operational entrypoint
for Buckit deployments:

- In the near term, `bm` provides cluster lifecycle management for Buckit
deployments.
- In the long term, `bm` replaces `mc` for Buckit-specific administration and,
over time, for a broader set of object and bucket operations.

The guiding principle is to ship **one product with multiple frontends**:

- a CLI for operators and automation
- an HTTP API for browser and programmatic use
- a web UI for interactive cluster management

`bm` should be distributed as a **single binary** in production.

## Goals

- Simplify creation and deployment of multi-node Buckit clusters.
- Reduce operational friction compared to manually managing distributed startup
arguments, config files, and service units.
- Provide a clear path from "cluster manager" to "Buckit operational client"
without building unrelated products.
- Keep production deployment simple: one manager binary, minimal moving parts.
- Preserve a good developer experience for the web UI and CLI.

## Non-Goals

- Replacing all `mc` functionality in the first release.
- Building a general-purpose infrastructure orchestrator.
- Requiring a node-side agent in the initial design.
- Making Kubernetes the primary deployment target for the first version.

## Product Model

`bm` is a single binary with two execution modes:

1. `bm` as a direct CLI
2. `bm server` as a long-running manager service

Examples:

```sh
bm alias set prod https://s3.example.com AK SK
bm ls prod/mybucket
bm admin info prod

bm server --listen :9443
```

The central requirement is that CLI and server mode share the same application
core. The web UI is a frontend on top of the server API, not a separate
product.

## Architecture

### High-Level Components

```text
bm
  - shared application core
  - CLI frontend
  - HTTP API server
  - web UI static asset server
  - SSH orchestration layer
  - task/job engine
  - cluster state store
```

### Shared Core

The shared core owns real business logic:

- cluster inventory
- topology planning
- Buckit install/deploy/upgrade workflows
- node discovery
- state persistence
- audit and task tracking

This logic must not be duplicated across CLI handlers and HTTP handlers.

### Frontends

#### CLI frontend

The CLI parses flags and arguments, then invokes shared application services.
It should be thin and mostly free of cluster logic.

#### HTTP API frontend

The HTTP API exposes the manager for the web UI and future automation.
REST+JSON is the default interface for the first version.

#### Web UI frontend

The web UI talks only to the HTTP API. It should not implement deployment or
cluster logic itself.

## Packaging Model

`bm` should be released as a single binary that embeds the web UI static
assets.

Recommended model:

- Development:
  - web UI built as a conventional frontend app using Node tooling
  - frontend dev server talks to local `bm` API during development
- Release:
  - frontend compiled to static assets
  - static assets embedded into the `bm` binary using Go `embed`

This keeps local frontend development sane while preserving a simple production
artifact.

## Distribution Model

The first distribution target should optimize for low-friction installation and
predictable release mechanics, not package-manager breadth.

### Primary Distribution

The initial release model should be:

- versioned release binaries for supported OS/architecture pairs
- checksums and, ideally, signatures for release artifacts
- thin install scripts that download and install the correct binary

Recommended installation entrypoints:

```sh
curl -fsSL https://get.buckit.io/bm/install.sh | sh
```

```powershell
iwr https://get.buckit.io/bm/install.ps1 -useb | iex
```

These convenience commands should be backed by a safer documented path that
downloads the script before execution.

Safer Unix path:

```sh
curl -fsSL https://get.buckit.io/bm/install.sh -o install.sh
sh install.sh
```

Safer Windows path:

```powershell
iwr https://get.buckit.io/bm/install.ps1 -OutFile install.ps1
powershell -ExecutionPolicy Bypass -File .\install.ps1
```

### Platform Coverage

- `install.sh`
  - primary install path for Linux, macOS, and WSL
- `install.ps1`
  - primary install path for native Windows
- direct binary download
  - available on all supported platforms as the lowest-level install path

The Unix shell installer is not the primary path for native Windows. Windows
should be treated as a first-class install target with its own script and
release artifact flow.

### Installer Responsibilities

Install scripts should stay thin and predictable. They should:

- detect OS and architecture
- select the correct release artifact
- download the binary
- verify checksum and, ideally, signature
- install to a reasonable prefix
- print the installed version

Install scripts should not:

- build from source
- silently mutate shell startup files
- silently require privileged installation when a user-local install works
- auto-install a service unless explicitly requested

### Packaging Roadmap

Distribution should expand in stages:

1. release binaries + `install.sh` + `install.ps1`
2. signed and checksummed releases as the standard trust model
3. Homebrew tap for macOS/Linux users
4. additional package-manager integration as needed, such as `winget`, `scoop`,
  or Linux package repositories

This keeps early release engineering proportional to product maturity while
still supporting an easy installation story.

## Cluster Management Model

### Agentless by Default

The initial design uses **SSH-based orchestration** from the manager instead of
a required node-side agent.

Manager responsibilities:

- discover hosts and disks
- install or upgrade Buckit binaries
- write config and environment files
- create and manage service units
- start, stop, and restart Buckit
- run preflight validation
- perform rolling upgrades
- collect logs for deployment tasks

Rationale:

- fewer moving parts
- easier recovery when something fails
- easier adoption in VM and bare-metal environments
- no extra daemon to install, upgrade, or debug



### Desired State

The manager should own desired state, not just execute ad hoc commands.

Examples of desired state:

- known nodes and credentials
- selected disks
- Buckit version
- cluster topology
- service state

The manager computes plans and executes them, rather than storing only a
history of shell commands.

## Phased Delivery Plan

### Phase 1: Manager foundation

Deliver the manager as a server-oriented control plane:

- `bm server`
- embedded web UI
- node inventory
- SSH-based discovery
- cluster planning
- Buckit deployment
- service management
- health/status views

This phase establishes the control-plane model and the shared core.

### Phase 2: Manager CLI

Expand the CLI around the same shared services:

- CLI commands that drive cluster lifecycle workflows directly
- task inspection and operational workflows from the CLI

This phase makes the manager usable without the UI.

### Phase 3: Selected `mc` admin replacement

Move Buckit-specific admin capabilities into `bm`:

- admin info
- policy/user/admin flows
- alias/profile management
- cluster-aware administration

This phase should target administrative parity before broad object-operation
parity.

### Phase 4: Broader object client replacement

Add more `mc`-style data-path commands where it makes sense:

- `ls`
- `cp`
- `mirror`
- bucket/object inspection helpers

This phase should happen only after the internal command model and client
abstractions are stable.

## Open Questions

- Should manager state start on SQLite only, or should Postgres be supported
from the first release?
- Which subset of `mc` admin commands should move first into `bm`?
- How should credentials be stored and rotated for SSH, S3 aliases, and Buckit
admin access?
- What level of multi-user/RBAC support is needed in the first manager release?

## Summary

`bm` should be designed as a single Buckit operational product with:

- one shared application core
- one binary for release
- multiple frontends: CLI and web/API
- agentless SSH orchestration for initial cluster management
- a phased path from manager-first workflows to gradual `mc` replacement

The first release should prioritize cluster lifecycle management and stable
application boundaries over broad command-surface expansion.

## Appendix — Implementation Notes

The sections below capture implementation-level details that an engineer
will need when building `bm`, but which are not necessary to understand the
high-level design above. They are directional, not contractual.

### Backend Design

The web UI backend is the `bm server` process itself, not a separate Node.js
backend.

Recommended backend subsystems:

- `api` — REST handlers, request validation, response models
- `app` — service wiring and shared use cases
- `store` — persistent state for clusters, nodes, tasks, credentials, audit events
- `tasks` — long-running asynchronous workflows
- `ssh` — SSH execution and file transfer
- `deploy` — install, upgrade, config, systemd workflows
- `cluster` — domain models and topology planning
- `health` — polling and status aggregation

### Task Model

Long-running operations must be task-backed. Examples: node discovery,
cluster planning, cluster deployment, rolling restart, rolling upgrade.

Preferred flow:

```text
CLI or HTTP request
  -> create task
  -> task runner executes workflow
  -> progress and logs stored
  -> caller polls or subscribes for updates
```

This applies especially to server mode and any long-running orchestration
workflow.

### Command and API Modes

`bm` has two execution styles.

**Direct operations** — short-lived request/response operations that run
directly in-process:

- `bm ls`
- `bm cp`
- `bm admin info`
- `bm user list`

**Manager-backed operations** — long-running orchestration flows that are
task-backed and own durable workflow state in the manager:

- `bm cluster add`
- `bm node discover`
- `bm cluster plan`
- `bm cluster deploy`
- `bm cluster upgrade`

This split keeps direct CLI commands responsive while letting the manager
own durable workflow state for everything that takes time.

### Storage and Persistence

The manager needs persistent state for:

- clusters
- nodes
- node facts
- cluster specs
- tasks
- task logs
- deployments
- SSH credentials or credential references
- audit events

Initial recommendation: start with SQLite for single-node deployment
simplicity, and design the storage layer so Postgres can be added later
without changing the higher-level application model.

### Repository Shape

One possible layout:

```text
cmd/bm/
internal/app/
internal/api/
internal/auth/
internal/store/
internal/tasks/
internal/ssh/
internal/deploy/
internal/cluster/
internal/health/
web/
```

This is intended as a directional layout, not a strict commitment.