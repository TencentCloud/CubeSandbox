---
title: Distributed Tracing
lang: en-US
---

# Distributed Tracing

Logs and metrics tell you *that* a sandbox operation was slow or failed; they rarely tell you *which stage* was responsible. Distributed tracing records one trace per operation, with a span for each instrumented stage across the components it touches.

Creating a sandbox, building a template from an image, or pausing and resuming a sandbox runs through several components in sequence, and tracing shows where the time goes across those stages.

## What is traced

Sandbox lifecycle operations are traced end to end:

| Workflow | Entry | What the trace shows |
|---|---|---|
| Sandbox creation (including creation from a snapshot) | `POST /sandboxes` | Queue and schedule, node selection, container create, and on restore the snapshot prepare and VM restore |
| Image-to-template | `POST /templates` | Image build, rootfs/ext4, artifact publish, distribution |
| Sandbox-to-template (commit) | `POST /cube/sandbox/commit` | Across CubeMaster and Cubelet broadly: submit, run, snapshot and register on Master, capture and publish on Cubelet |
| Snapshot creation | `POST /sandboxes/:sandboxID/snapshots` | Prepare, capture, register |
| Rollback | `POST /sandboxes/:sandboxID/rollback` | Prepare, restore, register |
| Pause / resume | `POST /sandboxes/:sandboxID/pause`, `.../resume` | Prepare, capture or runtime, finalize |

Spans come from five exporting services:

| Service | Role in the trace |
|---|---|
| `cube-api` | HTTP entry span for requests through CubeAPI |
| `cubemaster` | Orchestration: schedule, dispatch, async job lifecycle |
| `cubetemplatecenter` | Template image build and artifact handling |
| `cubelet` | Per-node runtime workflow and lifecycle lock |
| `cube-shim` | Per-sandbox VMM launch, boot or restore, container start |

Containerd instrumentation adds Task, RPC, and transfer spans under the service that performs those calls.

A few boundaries are worth knowing:

- When CubeAPI is bypassed, a trace can start at CubeMaster; the entry span is then CubeMaster's own HTTP `POST /cube/...` span, which may not carry a named `cubemaster.*` business name.
- Propagation is W3C Trace Context. A request from an SDK that does not propagate context is still traced server-side — you get the server-side trace, just without the client's own spans.
- Clone and other SDK-composed operations are not merged into a single trace. The guest agent has separate tracing code, but this integration does not connect its spans to the trace backend; guest work and hypervisor work appear in the duration of the surrounding `cube-shim` spans.

## Enabling tracing

Tracing is off by default. Point each component's `OTEL_EXPORTER_OTLP_ENDPOINT` at an externally supplied OTLP/**gRPC** receiver — an OpenTelemetry Collector or a Tempo OTLP/gRPC endpoint — using `http://<receiver-host>:<OTLP-gRPC-port>`.

Alternatively set `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`; it takes precedence when both are set. Tracing is off only when both are empty.

When tracing is enabled, this integration currently uses the SDK default parent-based, always-on sampler. Root requests and locally started lifecycle traces are sampled at 100%, and sampled parents remain sampled across services. There is no supported sampling-rate setting in this PR. Plan collector and backend capacity accordingly: a canary sandbox creation produced 52 spans, so 1,000 similar creations would produce roughly 52,000 spans before pause, resume, snapshot, and template traffic. Actual counts depend on the workflow and deployment. To reduce volume today, disable tracing on the relevant components; changing the sampling policy requires a separate implementation and cross-service validation.

Deploy the receiver and query backend separately. Each component exports under its own service name (`cube-api`, `cubemaster`, `cubetemplatecenter`, `cubelet`, `cube-shim`), so do not set a shared `OTEL_SERVICE_NAME`.

Configuration is environment-based and follows the install method:

- **Bare metal / one-click**: set the variable before install — see [Bare-Metal Deployment](./bare-metal-deploy.md).
- **Kubernetes**: add it to the per-component `env` arrays — see [Helm Install · Tracing configuration](./kubernetes/install.md#_8-4-tracing-configuration).

Cubelet launches a fresh CubeShim process per sandbox; the process lives alongside its sandbox and inherits Cubelet's environment at process start, so an already-running shim keeps its previous setting.

Cubelet registers containerd's `task.Wait` before `task.Start`. After the task starts, that wait intentionally outlives the sandbox creation request and is completed by task exit, so cancellation or timeout of the create request does not discard the exit event. The wait retains only the containerd namespace; it does not inherit request cancellation, logging values, or the completed creation span. A startup failure cancels the wait.

## Querying and interpreting durations

Find a trace by trace ID, or by an ID your API returned. The identifier in a response is not always the same string as the span attribute, and none of them is the trace ID:

| Identifier you have | Span attribute |
|---|---|
| Sandbox creation response `clientID` (the business request ID for that creation) | `cube.request_id` (creation only) |
| Sandbox ID | `cube.sandbox_id` |
| Template ID | `cube.template_id` |
| Snapshot ID | `cube.snapshot_id` |
| Async job ID (`jobID` / `job_id`) | `cube.job_id` |

Only the creation response maps this way: `clientID` in detail or list responses can be a host ID instead, so do not generalize. The HTTP request header `x-request-id` is a separate thing — it maps to `cube.http_request_id` on CubeAPI entry spans.

An attribute needs to be on *some* spans to find the whole trace: the trace is returned as soon as any span in it matches. The examples below use Tempo's TraceQL; other backends have their own query language.

```traceql
{ span.cube.request_id = "<creation-client-id>" }
```

```traceql
{ span.cube.sandbox_id = "<sandbox-id>" }
```

```traceql
{ span.cube.job_id = "<job-id>" }
```

One sandbox ID may match several traced operations within the backend retention period; narrow the time window to the operation you need.

If a lookup returns no results, check that the time window includes the operation. A failed request may not have generated a sandbox or snapshot ID yet; use its `cube.request_id` when available.

Once you have the trace, read durations with these rules in mind:

- A parent span's duration generally includes its nested work, so nested durations are not added on top of it. A detached async child can extend past its parent, and parent–child overlap is normal nesting — only sibling tasks that overlap in time suggest parallel work.
- Child span durations do not necessarily add up to the parent duration; gaps between stages may include waiting or work without a separate span.
- Async template jobs outlive the API span that submitted them. The submit span measures the submission, and the build and run spans record the background execution, but the whole submit-to-ready job is not necessarily a single span — confirm success from the job status, not from the spans alone.
- A business failure can still return HTTP 200. Look at the span status and business attributes (`cube.ret_code`) as well as the HTTP status rather than trusting the latter alone.

Tracing answers "which stage", not "why". Once a slow or failing span points at a component and a stage, that component's logs are usually where the reason is — see [Sandbox Logs](./sandbox-logs.md) and [Service Management & Logs](./service-management.md).
