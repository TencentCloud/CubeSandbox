# cubesandbox-base-java demo

A minimal image that stacks a **Java 17 LTS + Maven** toolchain and a tiny
HTTP server on top of
[`cubesandbox-base`](../../docker/Dockerfile.cube-base), so you can test the
"Bring Your Own Image" flow end-to-end with a real Java workload — and use
it as a ready-made starting point for Java-based sandboxes.

- envd listens on `:49983` (Cube readiness probe) — inherited from the base image.
- A JDK-built HTTP server listens on `:8080` and serves a tiny landing page
  that echoes the JVM version, so you can eyeball that Java really served
  the request.

See [Bring Your Own Image (envd)](../../docs/guide/tutorials/bring-your-own-image.md)
for the full tutorial, and [`../cubesandbox-base-nginx/`](../cubesandbox-base-nginx/)
for a sibling example that follows the exact same pattern with nginx.

## What's inside

- **OpenJDK 17 LTS** (`openjdk-17-jdk-headless`) — `java` / `javac` on `$PATH`.
- **Maven** — for building Java projects inside the sandbox.
- **`HelloWorldServer.java`** — a single-file HTTP server built on the JDK's
  built-in `com.sun.net.httpserver` API, so the image needs **no third-party
  dependencies**. Compiled to `/app/HelloWorldServer.class` at build time and
  launched as the foreground process via `CMD ["java", "HelloWorldServer"]`.

## Build

```bash
docker build -t cubesandbox-demo-java:latest .
```

The Dockerfile compiles and runs `HelloWorldServerTest` during the build,
so a failing test will abort the image build.

## Run unit tests locally

```bash
javac HelloWorldServer.java HelloWorldServerTest.java
java HelloWorldServerTest
```

Tests use only JDK built-in APIs (`java.net.http.HttpClient`,
`java.util.concurrent`) — no JUnit or other external dependencies required.
The concurrent-request test fires 32 simultaneous GET requests to verify
the thread-pool executor handles parallel load correctly.

`javac` leaves `*.class` files in this directory; they are covered by the
repo's `.gitignore`, or you can remove them afterwards with `rm -f *.class`.

## Run & verify locally

```bash
docker run --rm -d \
    -p 8080:8080 \
    -p 49983:49983 \
    --name cube-demo-java \
    cubesandbox-demo-java:latest

# Java server: should print the demo landing page (with the JVM version)
curl -s http://127.0.0.1:8080/

# envd readiness probe: should return 204
curl -s -o /dev/null -w "envd /health => %{http_code}\n" \
    http://127.0.0.1:49983/health

# Sanity-check the toolchain inside the container
docker exec cube-demo-java java -version
docker exec cube-demo-java mvn -version

docker rm -f cube-demo-java
```

## Register as a Cube template

```bash
cubemastercli tpl create-from-image \
    --image       <your-registry>/cubesandbox-demo-java:latest \
    --writable-layer-size 1G \
    --expose-port 49983 \
    --expose-port 8080 \
    --probe       49983 \
    --probe-path  /health
```

`--probe 49983 --probe-path /health` points Cube at envd (guaranteed to
return `204` within ~1s); the Java server's `:8080` stays exposed for your
actual traffic. See
[Creating Templates from OCI Images](../../docs/guide/tutorials/template-from-image.md)
for monitoring (`tpl watch`) and troubleshooting.

## Try it with the E2B SDK

After registering the template, [`test_sandbox.py`](./test_sandbox.py) boots
a sandbox from it and verifies four things:

1. `java -version` runs inside the sandbox (JDK is on `$PATH`)
2. `mvn -version` runs inside the sandbox (Maven is available)
3. `/app/HelloWorldServer.java` is readable via `sandbox.files.read(...)`
4. An HTTPS request to the sandbox's port `8080` returns the Java server's
   landing page

```bash
pip install -r requirements.txt

cp env.example .env
# fill in E2B_API_URL and CUBE_TEMPLATE_ID

python3 test_sandbox.py
```

## Files

```
cubesandbox-base-java/
├── Dockerfile              # FROM cubesandbox-base, installs JDK 17 + Maven, compiles & runs the server
├── HelloWorldServer.java   # JDK-only HTTP server (com.sun.net.httpserver), no third-party deps
├── HelloWorldServerTest.java # Unit tests incl. concurrent-request test (JDK-only, no JUnit)
├── test_sandbox.py         # E2B SDK smoke test: java/mvn version, file read, HTTP GET :8080
├── env.example             # E2B_API_URL / E2B_API_KEY / CUBE_TEMPLATE_ID
├── requirements.txt        # e2b + python-dotenv
└── README.md               # This file
```

## Use cases

- **Java code execution sandbox** — boot a sandbox and run `java`/`javac`/`mvn`
  via `sandbox.commands.run(...)` for isolated build or test jobs.
- **Java Web service base** — replace `HelloWorldServer` with your own
  Spring Boot / Quarkus / vanilla servlet app and expose its port with
  `--expose-port`.
- **Maven build runner** — clone a repo into the sandbox and run `mvn
  clean package` against an isolated, disposable environment.

## Known limitations

- The image ships **JDK 17 LTS** only. For a different LTS (11 / 21) change
  the `apt-get install` line in the `Dockerfile` and the `JAVA_HOME` env var.
- **Gradle is not preinstalled**; add `gradle` to the `apt-get` line if your
  project needs it, or use the [Gradle wrapper](https://docs.gradle.org/current/userguide/gradle_wrapper.html)
  (`./gradlew`) which needs no system install.
- `HelloWorldServer` is a demo, not a production server — it has no TLS,
  no graceful shutdown, and serves a single route. It uses a fixed thread
  pool sized from `Runtime.getRuntime().availableProcessors()` so concurrent
  requests are handled in parallel. Swap it for your real application's
  `CMD` and tune its executor for the workload.
- The Java server runs as `root` (the container's default user, inherited
  from the base image's `CMD` execution). For a hardened template, drop
  privileges to the base image's `user` (uid 1000) account before starting
  the JVM.

## Related

- [`../cubesandbox-base-nginx/`](../cubesandbox-base-nginx/) — the sibling
  example this template is modelled on (nginx instead of Java).
- [Bring Your Own Image (envd)](../../docs/guide/tutorials/bring-your-own-image.md)
  — the entrypoint contract and `envd` requirements.
- [Creating Templates from OCI Images](../../docs/guide/tutorials/template-from-image.md)
  — full `cubemastercli tpl create-from-image` reference.
