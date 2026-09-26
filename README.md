# Sunstone

**Deploy containers without deploying an orchestrator.**

Orchestrators like Kubernetes solve important problems, but many applications consist of a few stateless services and background workers that fit comfortably on a small number of VMs. For them, Kubernetes can cost more than the workloads it runs, both in infrastructure and in engineering time spent on upgrades, monitoring, and troubleshooting.

Cloud Run shows how simple container deployment can be. Its usage-based pricing keeps early costs low, but well-sized VMs can cost less as an application grows. Running containers on VMs often means falling back to shell scripts and manual Docker commands.

Sunstone deploys container images to Google Cloud VMs and manages deployments through explicit, imperative commands. The VMs and Google Cloud infrastructure remain yours.

Sunstone is built for stateless services and background workers. Cloud providers already do an excellent job running databases and other stateful systems. We believe durable state is better left to managed services such as Cloud SQL, Memorystore, and Cloud Storage, while Sunstone focuses on replaceable application containers.

> Sunstone is under development.

## How Sunstone works

Sunstone treats each web service or background worker as a workload. A workload runs one container on one or more VMs and deploys and rolls back independently.

The proxy and other host-level services run separately from application workloads.

For HTTP workloads, Sunstone starts the new container, waits until it is ready, and switches traffic through a shared proxy. You provision projects, networks, IAM, and Compute Engine instances separately.

## Configuration

One configuration file defines one workload and one container.

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

Web services and background workers use the same configuration format. Here is a worker using that format.

```yaml
name: storefront-worker

gcp:
  project: acme-prod
  instances:
    - zone: us-central1-a
      name: storefront-worker-1

container:
  image: us-central1-docker.pkg.dev/acme-prod/apps/storefront
  command: ["bin/jobs"]
```

## Inspiration

- [Kamal](https://kamal-deploy.org/) for its imperative deployment workflow.
- [Cloud Run](https://cloud.google.com/run) for its container configuration and Google Cloud integration.
