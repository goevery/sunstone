# AGENTS.md

Sunstone deploys containerized applications directly to Google Cloud VMs without Kubernetes or a persistent control plane. Users retain ownership of their network, machines, IAM, load balancing, and managed data services. A workload is one container deployed to one or more VMs. HTTP workloads support zero-downtime replacement, while background workloads use graceful stop-then-start replacement.

`cmd/sunstone` is the operator-facing CLI used by developers and CI. It reads workload definitions and coordinates deployments across their target VMs. It reaches private machines through IAP, OS Login, and SSH, then manages the container lifecycle and rolls out changes one VM at a time. Sunstone runs only for the duration of an operation; it is not a central server.

`cmd/sunbeam` is the lightweight HTTP proxy that runs on each VM. It checks replacement containers for readiness, switches traffic, and drains previous containers. Sunstone controls it through a loopback-only ConnectRPC API reached over SSH. Sunbeam persists its routing state as a JSON file in a Docker volume so traffic configuration survives restarts. Background workloads do not use Sunbeam.

## Project structure

Follow `golang-standards/project-layout`, with binaries in `cmd/` and application modules in `internal/modules/`. Each module exposes one small interface from its root package and hides its implementation in a nested `internal/` directory, preventing callers and sibling modules from bypassing that interface.

Design modules for depth: keep interfaces small while concentrating meaningful behavior behind them. Organize cohesive behavior under `features/` and place external integrations under `adapters/`. Introduce an internal seam and adapter only when behavior genuinely varies; do not create interfaces or pass-through packages speculatively.

## Code organization

Write each file so it can be read from overview to detail. Follow the language’s conventions. Put shared constants and types near the top. Show the public API and its orchestration before the private functions that support them. Place new code where it fits this flow and improve the code you touch rather than carry obsolete patterns forward.

Keep related statements together. Use blank lines to separate independent steps, including when a new step follows a control-flow block. When a block returns control or signals an error after other work, leave a blank line before that step.

Require collaborators explicitly and provide them at the composition root. Do not use optional dependencies with fallback implementations.

```text
.
├── api/
│   └── <service>/v1/
│       └── service.proto
├── cmd/
│   └── <binary>/
│       └── main.go
├── internal/
│   ├── gen/                       # generated API code
│   └── modules/
│       ├── <module-a>/
│       │   ├── module.go          # module's public interface
│       │   ├── module_test.go     # tests through the public interface
│       │   └── internal/
│       │       ├── features/
│       │       │   └── <feature>/ # cohesive behavior
│       │       │       ├── feature.go
│       │       │       ├── ports.go        # only when an internal seam is needed
│       │       │       └── feature_test.go # focused internal tests when useful
│       │       └── adapters/
│       │           └── <adapter>/
│       │               └── adapter.go
│       └── <module-b>/
│           ├── module.go
│           ├── module_test.go
│           └── internal/
│               ├── features/
│               │   └── <feature>/
│               └── adapters/
├── docs/
├── go.mod
└── go.sum
```

## Agent skills

### Issue tracker

Issues are tracked in GitHub Issues for `goevery/sunstone`. See `docs/agents/issue-tracker.md`.

### Triage labels

Issue triage uses the five canonical engineering-skill labels. See `docs/agents/triage-labels.md`.

### Pull requests

Use the `visual-pr` skill whenever creating or updating a pull request.

### Domain docs

This repository uses a single-context domain documentation layout. See `docs/agents/domain.md`.
