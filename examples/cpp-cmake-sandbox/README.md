# C/C++ CMake Sandbox

[Chinese README](README_zh.md)

A CubeSandbox template for isolated C/C++ builds. It extends
`cubesandbox-base` with GCC, CMake, Ninja, and ccache, then demonstrates that
both the CMake build directory and compiler cache survive a pause/resume cycle.

## Included

```text
cpp-cmake-sandbox/
|-- Dockerfile            # Cube-ready build image
|-- sample/               # Minimal CMake project and CTest target
|-- build_and_resume.py   # SDK build, snapshot, resume, and rebuild demo
|-- env_utils.py          # Local .env loading and validation
|-- .env.example          # Cube connection settings
`-- requirements.txt      # Host-side SDK dependencies
```

## Prerequisites

- A CubeSandbox deployment and `cubemastercli`.
- Docker on a build workstation and a registry reachable from Cube nodes.
- Python 3.9+ on the machine that runs the host-side script.

## Build the template

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/cubesandbox-cpp-cmake:latest \
  examples/cpp-cmake-sandbox
docker push <your-registry>/cubesandbox-cpp-cmake:latest
```

The image uses the pinned `ghcr.io/tencentcloud/cubesandbox-base:2026.16`
base, so `envd` already handles Cube SDK commands on port `49983`.

## Register it in CubeSandbox

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/cubesandbox-cpp-cmake:latest \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health

cubemastercli tpl watch --job-id <job-id>
```

Record the `template_id` after the job reaches `READY`.

## Run the snapshot cache demo

```bash
cd examples/cpp-cmake-sandbox
cp .env.example .env
# Set E2B_API_URL, E2B_API_KEY, and CUBE_TEMPLATE_ID.
pip install -r requirements.txt
python build_and_resume.py
```

The script performs these steps:

1. Copies `sample/` into the sandbox workspace and builds it with
   `cmake -G Ninja` and `ccache`.
2. Runs CTest and the resulting binary.
3. Calls `sandbox.pause()` to save the MicroVM state and release compute.
4. Reconnects with `Sandbox.connect(sandbox_id)`, touches one source file, and
   rebuilds. The persisted CMake tree and `~/.cache/ccache` are displayed by
   `ccache --show-stats`.

The script removes its sandbox in a `finally` block, including when a build
fails. It does not create a durable snapshot; use the
[`snapshot-rollback-clone`](../snapshot-rollback-clone) examples when you want
to retain or fan out snapshots.

## Local image smoke test

This check validates the toolchain without requiring a Cube cluster:

```bash
docker build -t cubesandbox-cpp-cmake:local .
docker run --rm cubesandbox-cpp-cmake:local sh -lc \
  'cp -a /opt/cpp-cmake-sandbox/sample /tmp/demo && \
   cmake -S /tmp/demo -B /tmp/demo/build -G Ninja \
     -DCMAKE_CXX_COMPILER_LAUNCHER=ccache && \
   cmake --build /tmp/demo/build && \
   ctest --test-dir /tmp/demo/build --output-on-failure'
```

## Resource and security notes

- Start with a `2G` writable layer; increase it for larger source trees or
  generated build artifacts.
- The bundled toolchain needs no outbound network access. Keep
  `allow_internet_access=False` for untrusted builds that do not download
  dependencies.
- `ccache` only speeds repeated compilations when source and compiler inputs
  are compatible. A base-image, compiler, or build-flag change can legitimately
  cause misses.
- The demo is intentionally small. For package managers such as Conan or
  vcpkg, preinstall dependencies in a derived image or allow only the required
  registry hosts through CubeEgress.

## References

- [Bring Your Own Image](../../docs/guide/tutorials/bring-your-own-image.md)
- [Snapshot, Rollback, and Clone](../snapshot-rollback-clone)
- [Code Sandbox Quickstart](../code-sandbox-quickstart)
