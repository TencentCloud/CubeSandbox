# Templates Overview

A template is a reusable runtime environment from which CubeSandbox creates sandboxes. It contains not only the root filesystem derived from an OCI image, but also the sandbox configuration and a pre-warmed MicroVM snapshot. The same template can create isolated sandboxes with consistent environments, while snapshot restoration keeps startup fast.

## Core Concepts

### OCI Images

OCI (Open Container Initiative) defines open standards for container images. The Docker images used in everyday workflows are generally OCI images: they contain read-only filesystem layers plus image configuration describing the operating system files, application and dependencies, and startup settings such as `ENTRYPOINT`, `CMD`, and environment variables. An image is commonly referenced as `registry/name:tag`, for example `docker.io/library/python:3.11`, or pinned to exact content with a digest.

CubeSandbox does not run an OCI image directly as a sandbox. It pulls and unpacks the image, converts it into a rootfs suitable for a MicroVM, starts the configured services, and creates a snapshot that can be restored quickly. The OCI image is therefore an input to template creation; the template is the artifact used to create sandboxes.

### When the Template Snapshot Is Taken

After pulling the OCI image, CubeSandbox boots it in a temporary MicroVM. The image entrypoint, system services, and application start just as they would in a normal sandbox. Cube does not take the template snapshot as soon as the processes start. It **waits for the configured HTTP probe to return 2xx**, which signals that the environment has reached the state worth preserving. Cube then freezes the filesystem and memory state and creates the template snapshot.

Choosing a probe therefore means defining when startup is complete. If it succeeds too early, the snapshot may be taken while the application is still initializing or its dependencies are not yet warm. If it never succeeds, the template build times out. Ideally, the endpoint should return 2xx only after every service that should be pre-warmed in the template is ready.

### Using Ports and Probes to Define Readiness

When building a template, Cube starts the container and probes it over HTTP to determine whether it is ready. Therefore:

1. The container image **must** start an HTTP server on a fixed port and provide a readiness endpoint that returns 2xx. The endpoint may be served by `envd` or by the application.
2. The following options **must** be specified when creating the template:
   - `--expose-port <port>` — declare the port on which the HTTP service listens
   - `--probe <port>` — tell Cube which port to probe
   - `--probe-path <path>` — set the HTTP path that Cube requests with `GET`, such as `/` or `/health`
3. The selected endpoint should return HTTP 2xx **only after the services that need to be pre-warmed are fully ready**. Cube uses this response as the signal to take the snapshot. When probing `envd`, the signal only confirms that `envd` itself is ready.

A template build selects one probe port and path. Cube repeatedly requests `http://<sandbox>:<probe-port><probe-path>` until it receives an HTTP 2xx response, then takes the snapshot.

`envd` is a background service preinstalled in CubeSandbox base images. After a sandbox is created, SDK operations such as running commands, reading and writing files, and opening a terminal send requests to `envd` inside the sandbox. By default, `envd` listens on port `49983`, and its `/health` endpoint returns 204 when it can accept those requests.

The following common configuration exposes port 49983 for `envd`, probes that port, and takes the template snapshot after `/health` succeeds:

```bash
--expose-port 49983 --probe 49983 --probe-path /health
```

However, `envd` readiness only proves that the SDK data plane is ready; it does not necessarily mean that the application has finished starting. If the template should pre-warm a web application, Jupyter, or another service, point the probe at a port and path that reflect that application's readiness. See [Pre-warm a Service in a Template](./tutorials/prewarm-template-service.md).

> `envd` is not required merely to build a template. An image that only serves its own web application and does not use command, file, or terminal APIs can omit `envd` and probe the application's own health endpoint. If those SDK capabilities are needed, use a base image that includes `envd` or inject it while building the template. See [Custom Template Images](./tutorials/bring-your-own-image.md) for the available integration approaches.

### Sandbox Readiness Semantics

The probed port is also the sandbox readiness contract. Because a template is marked ready only after the probe returns HTTP 2xx, that port is expected to be available as soon as creation of a **running** sandbox from the template returns. Clients do not need an additional wait or retry after `create` returns. The same applies to sandboxes brought back by resume or auto-resume.

Two limits are worth noting:

- **The guarantee covers only the probed port.** A service on a port that is exposed but not probed may still be starting when the sandbox begins accepting traffic. Probe that port instead if it needs the same readiness guarantee.
- **The service itself must remain healthy.** The guarantee covers platform-level reachability. If a service in a running sandbox crashes or stops listening, it remains unavailable until it recovers.

## How to Create a Template

CubeSandbox supports two approaches:

- **Create from an OCI image**: Prepare an OCI image containing the operating system, tools, and application dependencies, then let CubeSandbox convert it into a template. This is the usual choice for reproducible templates. See [Create Templates from OCI Image](./tutorials/template-from-image.md).
- **Create from a running sandbox**: Install software or adjust the environment interactively, then commit the sandbox's current filesystem and memory state as a new template. This is useful for iterative development and quickly preserving a working environment. See [Commit a Running Sandbox as a Template](./tutorials/template-from-sandbox.md).

In either case, template creation consists of preparing the root filesystem, booting a MicroVM and waiting for the environment to become ready, taking a snapshot, and registering the resulting template. Once ready, the template can be used to create new sandboxes quickly.

## Templates and Images

An OCI image is one possible input for building a template; it is not itself a directly bootable CubeSandbox template. The key differences are:

| | OCI image | CubeSandbox template |
| --- | --- | --- |
| Contents | Root filesystem and container metadata | Root filesystem, MicroVM snapshot, and sandbox runtime configuration |
| Primary purpose | Distribute and reproduce a software environment | Create CubeSandbox sandboxes quickly |
| Usage | Must first be converted into a template | Can be used directly to create sandboxes |

The same OCI image can therefore produce different templates depending on CPU, memory, network, startup command, and other settings. A template committed from a running sandbox preserves that sandbox's filesystem and memory state at commit time.

## Next Steps

- [Create Templates from OCI Image](./tutorials/template-from-image.md) — complete CLI workflow, including probes, progress monitoring, and troubleshooting.
- [Commit a Running Sandbox as a Template](./tutorials/template-from-sandbox.md) — preserve a sandbox environment and optionally override its create request.
- [Template Inspection and Request Preview](./template-inspection-and-preview.md) — inspect template state and preview the effective request.
