# Sunstone

Sunstone models direct deployment of containerized applications onto user-owned Google Cloud virtual machines.

## Language

**Workload**:
One container, defined once and deployed to one or more target VMs.
_Avoid_: Application, service

**Background workload**:
A workload that does not receive HTTP traffic through Sunbeam and whose replacement stops the current container before starting the new one.
_Avoid_: Worker, job

**HTTP workload**:
A workload whose HTTP traffic is managed by Sunbeam so a ready replacement can receive traffic before the previous container is drained.
_Avoid_: Web service

**Deployment**:
An attempt to make a workload definition current on its target VMs, replacing containers one VM at a time.
_Avoid_: Release

**Rollback**:
Restoration of the previous container on a VM after its replacement fails to start. A rollback ends the deployment before any remaining VMs are changed.
_Avoid_: Retry
