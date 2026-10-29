# AGENTS.md

Sunstone deploys containerized applications directly to Google Cloud VMs without Kubernetes or a persistent control plane, leaving users in control of their network, machines, IAM, load balancing and managed data services. Each workload is one container deployed to one or more VMs, with zero-downtime replacement for HTTP workloads and graceful stop-then-start replacement for background workloads.

`cmd/sunstone` is the CLI used by developers and CI to read workload definitions and coordinate deployments across their target VMs. It reaches private machines through IAP, OS Login and SSH, then manages the container lifecycle one VM at a time. It runs only for the duration of an operation rather than acting as a central server.

`cmd/sunbeam` is the lightweight HTTP proxy on each VM that checks replacement containers for readiness, switches traffic and drains previous containers. Sunstone controls it through a loopback-only ConnectRPC API reached over SSH. Routing state is stored as JSON in a Docker volume so it survives restarts, while background workloads run without Sunbeam.

## Project structure

Packages live directly under `internal/` and are organized by responsibility rather than nested module, `features/` or `adapters/` directories. Each binary in `cmd/` wires its dependencies and manages their lifetime, including session cleanup.

```text
.
├── api/                  # Protobuf service definitions
│   └── sunbeam/v1/
│       └── sunbeam.proto
├── cmd/
│   ├── sunstone/         # Sunstone CLI and dependency wiring
│   └── sunbeam/          # Sunbeam startup and dependency wiring
├── docs/
├── internal/
│   ├── cli/              # Progress, status and result formatting
│   ├── deployment/       # Host group checks, ordering, failure handling and results
│   ├── gen/              # Generated ConnectRPC code that must not be edited
│   ├── host/             # Workload operations and runtime access on one VM
│   ├── routing/          # HTTP proxy, readiness, switching, draining and durable state
│   ├── session/          # SSH sessions with IAP, OS Login and impersonation
│   └── workload/         # Workload definitions, YAML loading and validation
├── go.mod
└── go.sum
```

## Code organization

Follow the language's conventions and arrange each file so it reads from overview to detail, with shared constants and types near the top and public operations before their private helpers. Place new code within that flow and improve the code you touch rather than carrying obsolete patterns forward.

Keep related statements together and separate independent steps with blank lines, including when a new step follows a control-flow block. Leave a blank line before a return or error signal when other work precedes it in the same block.

Wire dependencies in `cmd/` and pass them explicitly through constructors, with no optional dependencies or fallback implementations. Prefer concrete types unless callers need interchangeable implementations, and avoid layers or wrappers that add no useful behavior.

Keep tests beside the code in a matching `*_test.go` file, focusing on public behavior and testing private functions directly when that makes the tests clearer.

## Documentation

Keep `README.md` concise and user-facing, covering Sunstone's purpose, main concepts and basic usage with examples rather than implementation details.

## Git

Use Conventional Commits for commit messages and pull request titles so Release Please can determine semantic versions and generate release notes.

## Agent skills

Track issues in GitHub Issues for `goevery/sunstone` following `docs/agents/issue-tracker.md`, and use the five canonical engineering-skill labels described in `docs/agents/triage-labels.md` for triage.

Use the `visual-pr` skill whenever creating or updating a pull request, and follow the single-context domain documentation layout described in `docs/agents/domain.md` when working with domain docs.
