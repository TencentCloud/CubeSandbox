# Introduction

Welcome to the Cube Security Sandbox documentation.

Cube Sandbox is a production-grade, multi-component security sandbox system designed for serverless computing and secure code execution environments. It implements a VM-based container isolation architecture using KVM hypervisor technology.

## Key Advantages

* **Exceptional Performance**: Fully optimized end-to-end for sandbox workloads. Delivers each sandbox instance in under 100ms with less than 5MB of additional memory overhead — even at 100 concurrent sandboxes on a single machine.

* **Secure by Design**: Hardware-level isolation combined with comprehensive network security policies, enforcing strict security boundaries between sandbox instances in multi-tenant environments.

* **Ready Out of the Box**: No complex dependencies. A minimal deployment script gets a Cube Sandbox environment running locally in minutes, letting you experience its core capabilities immediately.

* **E2B Compatible**: Drop-in compatible with the E2B sandbox protocol — existing Agent applications and workflows gain significantly stronger security without any business logic changes.

## Next Steps

* [Quick Start](./quickstart.md) — the fastest path from zero to a running sandbox.
* [Self-Build Deployment](./self-build-deploy.md) — single-machine deployment reference with prerequisites, configuration, and troubleshooting.
* [Multi-Node Cluster Deployment](./multi-node-deploy.md) — add compute nodes to scale beyond a single machine.
* [Creating Templates from OCI Images](./tutorials/template-from-image.md) — step-by-step CLI guide for template creation, monitoring, and management.
