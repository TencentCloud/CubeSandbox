# Java Spring Boot Dev/Test Sandbox

[中文文档](README_zh.md)

This example builds a reusable CubeSandbox template for enterprise Java backend
development and testing. The template warms the Maven dependency cache during a
controlled build step, then the demo runs an offline Spring Boot build, starts a
real HTTP service, snapshots that workspace, forks a fresh sandbox from the
checkpoint, and proves that the fork can reuse both the compiled jar and
stateful task data. Both runtime sandboxes use default-deny public egress.

## What It Demonstrates

1. **Spring Boot web service in an isolated MicroVM**: the app exposes
   `/health`, `/api/info`, `POST /api/tasks`, and `GET /api/tasks/{id}` on
   port `8080`.
2. **Template-warmed Maven cache**: the Dockerfile runs
   `mvn dependency:go-offline`; the demo then uses `mvn --offline package`, and
   the checkpoint preserves `/workspace/.m2/repository` and `target/*.jar`.
3. **Stateful workspace fork**: task state is persisted to
   `/tmp/cubesandbox-spring/state/tasks.json` and read back from the forked
   sandbox.
4. **Web service routing**: the demo calls the Spring Boot service through
   CubeSandbox routing with `sandbox.get_host(8080)`.
5. **Verified default-deny egress**: the demo creates sandboxes with
   `allow_internet_access=False`, proves a public HTTPS request is blocked, and
   still completes the Maven build with `--offline`.

## Why This Matters For CubeSandbox

Java backend projects often spend the first run downloading Maven dependencies
and compiling code before any test can start. CubeSandbox templates can capture
the dependency cache from a controlled build step, and snapshots can preserve
the follow-up build output and service state. The pattern is useful for
agent-driven backend debugging, API contract tests, regression reproduction,
and disposable feature branches that still need a realistic JVM service.

## Files

- `Dockerfile`: Java 21 + Maven image built from digest-pinned base images, with
  a pre-warmed Maven dependency cache.
- `pom.xml` and `src/main/...`: minimal Spring Boot backend service.
- `scripts/run_demo.py`: end-to-end CubeSandbox demo using the E2B-compatible
  SDK and local dev sidecar.
- `env.example`: local CubeSandbox API/proxy settings.
- `output/`: downloaded manifest from a successful run.

## Step 1: Build The Image

Build the image where your Cube node runtime can access it:

```bash
docker build -t cubesandbox-java-springboot-web:latest .
```

## Step 2: Register A Template

Register the image with envd on `49983` and the Spring Boot service port on
`8080`:

```bash
cubemastercli tpl create-from-image \
    --image               cubesandbox-java-springboot-web:latest \
    --writable-layer-size 2G \
    --expose-port         49983 \
    --expose-port         8080 \
    --probe               49983 \
    --probe-path          /health
```

Copy the returned template ID, for example `tpl-xxxxxxxxxxxxxxxxxxxxxxxx`.

## Step 3: Configure The Client

Install the local client dependencies:

```bash
pip3 install -r requirements.txt
```

Run the 20 focused Spring Boot and task-state tests:

```bash
mvn test
```

Create `.env` and set the template ID:

```bash
cp env.example .env
```

For the local dev VM, `.env` usually looks like this:

```bash
E2B_API_URL="http://127.0.0.1:13000"
CUBE_REMOTE_PROXY_BASE="https://127.0.0.1:11443"
CUBE_TEMPLATE_ID="tpl-xxxxxxxxxxxxxxxxxxxxxxxx"
E2B_API_KEY=e2b_dummyapikeyforlocaltest
```

## Step 4: Run The Demo

```bash
python3 scripts/run_demo.py
```

The script will:

1. Create a default-deny sandbox from the Java/Spring Boot template.
2. Prove direct public HTTPS egress is blocked.
3. Upload the Spring Boot project into `/workspace/java-springboot-web`.
4. Run `mvn --offline -DskipTests package` using the template-warmed Maven
   cache and build the jar.
5. Start the service and call `/health`, `/api/info`, and `POST /api/tasks`
   through CubeSandbox routing.
6. Verify that task state was written to
   `/tmp/cubesandbox-spring/state/tasks.json`.
7. Stop the JVM, wait for process exit, flush state, and create a checkpoint.
8. Start a fresh default-deny sandbox from the snapshot.
9. Verify that `/workspace/.m2/repository`, `target/*.jar`, and the task state
   file were inherited.
10. Start Spring Boot directly from the inherited jar and read the original task.
11. Download `output/manifest.json` with a machine-readable proof of the run.

The manifest includes:

```json
{
  "scenario": "java_springboot_devtest_sandbox",
  "features": [
    "spring_boot_web_service",
    "snapshot_warmed_maven_cache",
    "stateful_workspace_fork",
    "default_deny_egress_offline_build"
  ],
  "state_inherited": true,
  "artifact_inherited": true,
  "maven_cache_inherited": true,
  "internet_access_denied": true,
  "state_flushed_before_snapshot": true
}
```

## Restricted-Egress Notes

The Dockerfile performs Maven dependency warmup during the controlled template
build. At runtime, both sandboxes use `allow_internet_access=False`; the demo
first verifies that a public HTTPS request fails, then builds with Maven offline
mode. For production or regulated clusters, use one of these patterns:

- Prebuild the template image with dependencies already downloaded.
- Point Maven at an internal repository mirror through `settings.xml`.
- Run a controlled network build step, preserve the warmed Maven cache in the
  template, and use snapshot/fork for repeated test runs.

The forked part of this demo starts from the inherited jar and does not need to
redownload dependencies.

## Resource Recommendations

- Minimum: 2 vCPU and 2 GiB memory.
- Recommended writable layer: at least 2 GiB for Maven cache and build output.
- JVM container hint:

```bash
SPRING_BOOT_JAVA_OPTS="-XX:+UseContainerSupport -XX:MaxRAMPercentage=75"
```

## Known Limitations

- This example intentionally avoids Redis, MySQL, Kafka, and multi-service
  orchestration so the snapshot/fork workflow stays easy to run.
- Building the template image needs controlled network access unless its base
  images and Maven dependencies are supplied by internal mirrors.
- Snapshot timing depends on the local CubeSandbox deployment and storage
  backend.
