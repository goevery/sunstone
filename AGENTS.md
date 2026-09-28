# AGENTS.md

Sunstone deploys containerized applications directly to Google Cloud VMs without Kubernetes or a persistent control plane. Users retain ownership of their network, machines, IAM, load balancing, and managed data services. A workload is one container deployed to one or more VMs. HTTP workloads support zero-downtime replacement, while background workloads use graceful stop-then-start replacement.

`cmd/sunstone` is the operator-facing CLI used by developers and CI. It reads workload definitions and coordinates deployments across their target VMs. It reaches private machines through IAP, OS Login, and SSH, then manages the container lifecycle and rolls out changes one VM at a time. Sunstone runs only for the duration of an operation; it is not a central server.

`cmd/sunbeam` is the lightweight HTTP proxy that runs on each VM. It checks replacement containers for readiness, switches traffic, and drains previous containers. Sunstone controls it through a loopback-only ConnectRPC API reached over SSH. Sunbeam persists its routing state as a JSON file in a Docker volume so traffic configuration survives restarts. Background workloads do not use Sunbeam.

## Project structure

Follow `golang-standards/project-layout`, with binaries in `cmd/` and application modules in `internal/modules/`. Each module exposes a small API from its root package and keeps its implementation in a nested `internal/`, preventing sibling modules from importing it. Organize vertical slices under `features/` and external integrations under `adapters/`.

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
│       │   ├── module.go          # module's public API
│       │   └── internal/
│       │       ├── features/
│       │       │   └── <feature>/ # one vertical slice
│       │       │       ├── feature.go
│       │       │       ├── ports.go
│       │       │       └── feature_test.go
│       │       └── adapters/
│       │           └── <adapter>/
│       │               └── adapter.go
│       └── <module-b>/
│           ├── module.go
│           └── internal/
│               ├── features/
│               │   └── <feature>/
│               └── adapters/
├── docs/
├── go.mod
└── go.sum
```
