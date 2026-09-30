# Sunray

Sunray is an opt-in, Linux runc-compatible OCI runtime. Sunstone supplies Secret
Manager references; Sunray fetches values on the VM using its attached service
account. Neither Sunstone nor Sunbeam fetches the payloads. There is no workload
entrypoint wrapper or persistent secret-fetching server.

## Workload configuration

```yaml
name: storefront
gcp:
  project: acme-prod
  instances:
    - zone: us-central1-a
      name: storefront-1
container:
  image: example/storefront:v1
  env:
    - name: MODE
      value: production
    - name: API_KEY
      valueFrom:
        secretKeyRef:
          name: api-key
          key: "3"
    - name: DATABASE_PASSWORD
      valueFrom:
        secretKeyRef:
          name: projects/acme-data/secrets/database-password
          key: latest
```

`name` identifies a secret, not a version resource; `key` is an explicit numeric
version or alias. Quote numeric keys. Short secret IDs use `gcp.project`.
Environment entries require exactly one value source, and names must be unique.
Empty literal strings are valid. Existing literal `env` mappings remain supported.

Sunstone selects Docker's registered `sunray` runtime only when a workload has
secret references, passing a JSON map in the `dev.sunstone.secret-env` OCI
annotation. Image-default variables of the same name are replaced. Payloads are
not added to Docker's stored environment configuration.

Enable Secret Manager and grant the VM service account access to the referenced
secrets, preferably using secret-level `roles/secretmanager.secretAccessor`.
Cross-project references require IAM on the target secret. Sunray explicitly uses
the metadata token endpoint, not ADC files or the operator's identity. Access uses
CRC32C verification, up to four concurrent unique references, at most three
attempts for transport/429/5xx failures, and a 30-second preparation deadline.
Payloads must be UTF-8 without NUL bytes; empty and multiline values are preserved.

## Rotation and lifecycle

A container start resolves aliases to numeric version resources. The snapshot
stores references only, in private tmpfs state. Docker exec and Docker health
checks refetch those same numeric versions; they do not independently resolve
`latest`. Children spawned by the workload inherit its environment normally.

Restarting starts a new snapshot. HTTP restart creates a replacement and switches
traffic after readiness; background restart stops/starts the existing container.
Changing a reference changes desired configuration. Updating an alias behind an
unchanged reference does not force an otherwise-current `deploy` to replace the
container. Numeric versions avoid different VM starts resolving different alias
values during sequential rollout.

Referenced environment names are authoritative, including exec. Per-exec
`--env` overrides of secret-backed names are not supported. Other exec variables
are retained. A revoked/disabled snapshot version prevents a new exec or health
check from receiving credentials, but does not stop the existing main process.

Fetch failures prevent process startup. HTTP candidate failure preserves the
previous route/container and removes the candidate. Background replacement keeps
existing stop/start and rollback eligibility: only a previously running container
stopped by that deployment is eligible for rollback. If its refetch also fails,
rollback cannot restore it. Status and removal do not fetch secrets.

## Build and install on COS

Build from a reviewed, pinned source revision:

```sh
make sunray
GOARCH=arm64 make sunray BUILD_DIR=build/arm64
sha256sum build/sunray
```

The build is static Linux Go code. A future release workflow attaches amd64 and
arm64 executables and their SHA-256 manifest; implementation alone does not
publish a release. Verify the downloaded artifact against the manifest from a
trusted, pinned release before installing it.

Sunray is a host executable, not a scratch container. On COS, `/var` is generally
`noexec`; do not assume an arbitrary persistent directory can execute binaries.
A verified executable persistent location is a dedicated directory beneath
`/var/lib/docker`. Install the pinned artifact root-owned, mode `0755`, using an
atomic replacement. Preserve `/usr/bin/runc`, the trusted real runtime path.

Merge this entry into the existing Docker daemon configuration:

```json
{
  "runtimes": {
    "sunray": {"path": "/var/lib/docker/sunray/sunray"}
  }
}
```

Do not replace the entire daemon configuration with that fragment. Preserve
existing runtimes, `live-restore`, storage, logging, MTU, and the default runtime.
With Terraform, decode the existing configuration, merge its `runtimes` map with
this entry, then encode the full resulting configuration into provisioning data.
On the tested COS image, Docker's systemd unit copies persistent
`/var/lib/docker/daemon.json` into `/etc/docker/daemon.json` when the former exists.
Verify that behavior on the chosen COS image; `/etc` is reconstructed each boot.

Validate the merged configuration with `dockerd --validate --config-file=...`.
Plan Docker restart as maintenance rather than assuming it is disruption-free.
Registration belongs in infrastructure provisioning, not an automatic deployment
fallback. Before accepting workloads, verify `docker info` reports Sunray and
that `/run`, the OCI bundle filesystem, and runc's state root are tmpfs. Sunray
refuses secret preparation on durable filesystems. Its private state is under
`/run/sunray` and is intentionally lost on reboot; Docker restart policies
recreate containers and fetch fresh snapshots.

Sunray's supported integration is rootful Docker/containerd invoking runc-style
`create`, `start`, `exec --process`, and `delete`. Other ordinary lifecycle and
feature-discovery commands are delegated. Secret-enabled direct `run`,
checkpoint, and restore are unsupported. The initial validated stack is COS,
Docker 27.5.1/API 1.47, containerd 2.0.10, and runc 1.2.9. Unknown preparation
options fail closed. Secret-enabled debug logging is rejected to avoid verbose
runtime output disclosing values. Runtime upgrades require checking this contract
again, including exec arguments and runtime-owned state.

For an upgrade, verify the new artifact, plan maintenance, atomically replace the
executable, and validate create/exec/delete against existing snapshots. Don't
silently upgrade running hosts during a Sunstone deployment. Keep the previous
artifact available for an operator-controlled rollback. Snapshot format changes
require a compatible reader or container recreation during maintenance.

## Diagnostics and security limits

Preparation failures write a safe error-level JSON entry into the runc log passed
by containerd, as well as returning nonzero. Docker forwards a diagnostic containing
the reference and category/status, not payloads, tokens, or response bodies. The
tested Docker stack classifies these as internal failures (HTTP 500), including
Secret Manager 403/404; Docker's typed not-found classification does not represent
secret absence. A 403 does not prove that a secret exists.

The workload receives plaintext in its environment. Host root, Docker
administrators, and the workload itself can inspect it. This is not a sandbox or
per-container IAM boundary. Workloads sharing a VM normally can reach its metadata
server and share its identity. Isolate trust domains on separate VMs or enforce
metadata-network restrictions separately when needed.

Create injection temporarily modifies the OCI specification in tmpfs. A private
journal contains only the original specification, never injected payloads.
Success and delegated failure restore the original bytes and permissions. Linux
parent-death signaling prevents the directly delegated runtime from continuing
if Sunray is killed. A later create/exec/delete restores interrupted bundles and
cleans orphaned preparation files while holding container and bundle locks.

SIGKILL, a crash, or machine failure can bypass immediate cleanup. Plaintext can
remain in tmpfs until recovery or reboot; userspace code cannot guarantee instant
erasure. Recovery must precede regenerating/reusing an interrupted bundle. The
journal cannot erase a workload's environment, kernel copies, or external logs
written by the workload. Avoid swap/core dumps or account for them in the host
security policy; tmpfs alone is not a forensic-erasure guarantee. Small reference-free
lock files remain until reboot to preserve cross-process lock identity.

Inspect runtime-owned state on the supported stack after successful and failed
starts. Absence of known markers in inspected files is evidence for that stack,
not protection against privileged access or an assurance about arbitrary runtimes.

## Tests

Normal `go test ./...` runs strict YAML, deployment policy, and hermetic executable
tests with metadata/Secret Manager HTTP servers and a controlled delegated runtime.
The executable tests use private `/dev/shm` directories and have test-only
integration settings; the shipped executable has no environment hook for changing
the trusted runtime or credential endpoints.

For opt-in real Docker tests, register the built Sunray executable, pull
`busybox:1.37.0`, and create an accessible disposable secret. Use its reference and
the SHA-256 digest of the payload, not the payload itself:

```sh
SUNRAY_DOCKER_TEST=1 \
SUNRAY_TEST_REFERENCE=projects/PROJECT/secrets/TEST_SECRET/versions/latest \
SUNRAY_TEST_SHA256=DIGEST \
go test ./cmd/sunray -run TestDockerRuntime -v
```

The test creates/removes its own non-root containers and exercises the Docker Go
client, create/start, exec, Docker health checks, metadata inspection, and a missing
version diagnostic. Runtime registration, API/IAM setup, and infrastructure cleanup
remain the test operator's responsibility.
