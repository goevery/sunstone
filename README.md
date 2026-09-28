# Sunstone

**Deploy containers without deploying an orchestrator.**

Sunstone deploys container images directly to Google Cloud VMs and provides zero-downtime deployments for HTTP workloads without requiring a control plane, while the VMs and surrounding infrastructure remain yours.

> Sunstone is under development.

## Why Sunstone

Orchestrators like Kubernetes solve important problems, but many applications consist of a few stateless services and background workloads that fit comfortably on a small number of VMs. For them, Kubernetes can cost more than the workloads it runs, both in infrastructure and in engineering time spent on upgrades, monitoring, and troubleshooting.

Cloud Run shows how simple container deployment can be. Its usage-based pricing keeps early costs low, but well-sized VMs can cost less as an application grows. Running containers on VMs often means falling back to shell scripts and manual Docker commands.

## Where it fits

Sunstone only speaks GCP. Its choices are informed by years of operating applications on the platform. Security defaults are built in, and complexity has to earn its place.

Sunstone is built for stateless services and background workloads. Cloud providers already do an excellent job running databases and other stateful systems. We believe durable state is better left to managed services such as Cloud SQL, Memorystore, and Cloud Storage, while Sunstone focuses on replaceable application containers.

## How Sunstone works

Sunstone supports HTTP workloads and background workloads. A workload runs one container on one or more VMs and is deployed independently. Containers are replaced one VM at a time.

HTTP workloads use a zero-downtime rolling deployment. When deploying a new version of an HTTP workload, Sunstone starts the replacement container alongside the one currently serving traffic. Once the replacement is ready, the proxy on that VM sends new requests to it while the old container drains and stops.

Background workloads use stop-then-start replacement. When deploying a new version of a background workload, Sunstone lets the current container shut down cleanly before starting its replacement.

Sunstone runs one proxy on each VM, shared by the HTTP workloads deployed there. You provision projects, networks, IAM, and Compute Engine instances separately.

## Commands

Use `-f` to pass a workload file or directory.

```text
suns deploy  -f FILE_OR_DIRECTORY
suns status  -f FILE_OR_DIRECTORY
suns restart -f FILE_OR_DIRECTORY
suns remove  -f FILE_OR_DIRECTORY
```

- `deploy` validates and deploys the workload.
- `status` reports its state on each VM.
- `restart` restarts it one VM at a time.
- `remove` drains traffic and removes it while leaving the VMs and proxy running.

Run `suns COMMAND --help` for complete usage and options.

## Configuration

Each YAML document defines one workload and one container.

```yaml
name: storefront-web

gcp:
  project: acme-prod
  instances:
    - zone: us-central1-a
      name: storefront-1
    - zone: us-central1-b
      name: storefront-2

container:
  image: us-central1-docker.pkg.dev/acme-prod/apps/storefront
  command: ["bin/web"]

  env:
    APP_ENV: production
    DATABASE_URL:
      secret: projects/acme-prod/secrets/database-url/versions/latest

  resources:
    cpu:
      shares: 1024
    memory:
      limit: 512MiB

  readinessProbe:
    httpGet:
      path: /up
      port: 3000

http:
  port: 3000
  routes:
    - host: shop.example.com
```

Sunstone configures CPU and memory differently because they behave differently when containers share a VM. CPU is compressible. A container can receive less CPU and keep running, only more slowly. CPU shares only matter when the VM is busy. Containers with more shares get more CPU. When the VM has spare CPU, any container can use it.

Memory is incompressible. A container cannot adapt to memory pressure merely by running more slowly. A hard memory limit protects other workloads on the host, and exceeding it can cause an out-of-memory kill.

HTTP workloads and background workloads use the same configuration format. Here is a background workload using that format.

```yaml
name: storefront-jobs

gcp:
  project: acme-prod
  instances:
    - zone: us-central1-a
      name: storefront-jobs-1

container:
  image: us-central1-docker.pkg.dev/acme-prod/apps/storefront
  command: ["bin/jobs"]
```

## Inspiration

- [Kamal](https://kamal-deploy.org/) for its imperative deployment workflow.
- [Knative](https://knative.dev/) for its workload and traffic model.
- [Cloud Run](https://cloud.google.com/run) for its container configuration and Google Cloud integration.
