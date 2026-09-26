# Sunstone

**Deploy containers without deploying an orchestrator.**

Orchestrators like Kubernetes solve important problems, but many applications consist of a few stateless services and background workers that fit comfortably on a small number of VMs. For those applications, Kubernetes can cost more to run than the workloads it hosts once infrastructure and the engineering time for upgrades, monitoring, and troubleshooting are counted.

Cloud Run shows how simple container deployment can be. Its usage-based pricing keeps early costs low, but well-sized VMs can cost less as an application grows. The usual tradeoff is a return to shell scripts and manual Docker commands.

Sunstone deploys container images to Google Cloud VMs and manages deployments through explicit, imperative commands. The VMs and Google Cloud infrastructure remain yours.

Sunstone is built for stateless services and background workers. Cloud providers already do an excellent job running databases and other stateful systems. We believe durable state is better left to managed services such as Cloud SQL, Memorystore, and Cloud Storage, while Sunstone focuses on replaceable application containers.

> Sunstone is under development.

## Model

- A config defines one application and one container image.
- Every workload in the config uses the same image version and deploys or rolls back with the others.
- A workload with `http` serves requests; one without it is a worker.
- Each HTTP workload registers its routes with the shared proxy on its hosts.
- Projects, networks, IAM, and Compute Engine instances are provisioned outside Sunstone.

## Configuration

```yaml
schema: 1

service: storefront
image: us-central1-docker.pkg.dev/acme-prod/apps/storefront

google:
  project: acme-prod

workloads:
  web:
    instances:
      - zone: us-central1-a
        name: storefront-1
      - zone: us-central1-b
        name: storefront-2

    command: ["bin/web"]

    http:
      port: 3000
      healthcheck:
        path: /up
      routes:
        - host: shop.example.com

  worker:
    instances:
      - zone: us-central1-a
        name: storefront-worker-1

    command: ["bin/jobs"]

resources:
  cpu:
    shares: 1024
  memory:
    limit: 512MiB

env:
  APP_ENV: production
  DATABASE_URL:
    secret: projects/acme-prod/secrets/database-url/versions/latest

deploy:
  batch: 1
  wait: 5s
  timeout: 60s
  retain: 5
```

CPU shares are relative weights used only during contention; they are neither reservations nor caps. Memory limits are hard container limits.

Applications that deploy independently use separate configs. The same applies to workloads that need independent deployment or rollback.

## Inspiration

- [Kamal](https://kamal-deploy.org/) for simple, imperative container deployments.
- [Cloud Run](https://cloud.google.com/run) for its container model and Google Cloud integration, without its declarative resource format.
