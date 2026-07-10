# C/C++ CMake Sandbox

[中文文档](README_zh.md)

A ready-to-use **C/C++ development template** for Cube Sandbox: build a minimal
C++17 project with **CMake + Ninja** inside an isolated MicroVM, and use
**snapshots to persist and restore the `ccache` compile cache** for fast
incremental builds.

This template is intentionally independent of the Node / Python / Go / Rust /
Java templates and focuses on the C/C++ toolchain.

## 1. What you get

| File | Purpose |
|------|---------|
| `Dockerfile` | C/C++ dev image on `cubesandbox-base` (gcc/g++/clang, CMake, Ninja, ccache, gdb) |
| `project/` | Minimal C++17 project: a `greeter` static lib, an `app` executable, and a CTest test — **zero third-party dependencies** |
| `01_build_in_sandbox.py` | E2B SDK: push the project, build with CMake+Ninja, run `./app` |
| `02_run_ctest.py` | E2B SDK: build and run the CTest suite |
| `03_ccache_snapshot.py` | Native SDK: cold build → snapshot (with ccache) → clone → warm incremental build, with timings + speedup |
| `04_ccache_rollback.py` | Native SDK: warm-cache snapshot → mutate workspace → `rollback()` → rebuild hits the cache |

> **Two SDKs on purpose.** `01`/`02` use the E2B-compatible SDK
> (`e2b_code_interpreter`) to mirror the [Code Sandbox Quickstart](../code-sandbox-quickstart).
> `03`/`04` use the native `cubesandbox` SDK because its snapshot / rollback API
> is more direct. See [Environment variables](#4-environment-variables).

## 2. When to use this template

- C/C++ CI: compile and test a project in a clean, isolated environment.
- Incremental-build acceleration: warm a `ccache`, snapshot it, and fan out or
  resume builds that hit the cache instead of recompiling from scratch.
- Checkpoint / resume long builds via snapshot + rollback.

**Resource suggestion:** `--writable-layer-size 4G` (C/C++ artifacts + ccache
are larger than scripting-language templates), ccache capped at 2G.

**Known limitations:** linux/amd64 only; no third-party package manager
(vcpkg / Conan) is wired in — the sample project is dependency-free by design.

## 3. Quick start

### Step 1 — Build the image and register a template

```bash
# Build locally (run from the repository root)
docker build -t cubesandbox-cpp-cmake:latest examples/cpp-cmake-sandbox

# Optional local sanity check: envd readiness probe should return 204
docker run --rm -d -p 49983:49983 --name cube-cpp cubesandbox-cpp-cmake:latest
curl -s -o /dev/null -w "envd /health => %{http_code}\n" http://127.0.0.1:49983/health
docker rm -f cube-cpp

# Push to your registry, then register as a Cube template
cubemastercli tpl create-from-image \
    --image <your-registry>/cubesandbox-cpp-cmake:latest \
    --writable-layer-size 4G \
    --expose-port 49983 \
    --probe       49983 \
    --probe-path  /health
```

Note the `template_id` printed on success.

### Step 2 — Install dependencies and configure the environment

```bash
pip install -r requirements.txt

cp .env.example .env
# edit .env and fill in the values below
```

### Step 3 — Run the demos

```bash
# E2B SDK: build + run
python 01_build_in_sandbox.py

# E2B SDK: build + ctest
python 02_run_ctest.py

# Native SDK: ccache persistence via snapshot + clone (the highlight)
python 03_ccache_snapshot.py

# Native SDK: in-place rollback to a warm-cache snapshot
python 04_ccache_rollback.py
```

Expected highlights:

- `01` prints `Hello, CubeSandbox!`
- `02` prints `100% tests passed`
- `03` prints a `first build` vs `rebuild after snapshot` comparison and a
  `speedup: Nx` line
- `04` prints `ccache` stats showing cache hits > 0 after rollback

## 4. Environment variables

| Variable | Used by | Meaning |
|----------|---------|---------|
| `E2B_API_URL` | `01`, `02` | Cube API address, e.g. `http://<node-ip>:3000` |
| `E2B_API_KEY` | `01`, `02` | Any non-empty value satisfies the E2B SDK |
| `CUBE_API_URL` | `03`, `04` | Cube API address (read by the native SDK `Config`) |
| `CUBE_TEMPLATE_ID` | all | Template ID from `create-from-image` |
| `SSL_CERT_FILE` | optional | Root CA path when CubeAPI is served over HTTPS |

## 5. Try the project locally (optional)

The `project/` tree is a normal CMake project, so you can verify it on any
machine with a C++ toolchain before pushing it into a sandbox:

```bash
cd project
cmake -G Ninja -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build
./build/app                 # -> Hello, CubeSandbox!
ctest --test-dir build --output-on-failure
```

## 6. Troubleshooting

| Symptom | Likely cause | Fix |
|---------|-------------|-----|
| `SSL: CERTIFICATE_VERIFY_FAILED` | HTTPS without CA cert | Set `SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem` |
| `Template not found` | Wrong template ID | Re-run `cubemastercli tpl list` |
| `Connection refused` | CubeAPI not reachable | Check `E2B_API_URL` / `CUBE_API_URL` and port 3000 |
| Build times out | Default command timeout too low | The scripts already pass `timeout=300`; raise it if needed |
| `speedup` close to `1x` | Cache not reused | Confirm `CCACHE_DIR=/workspace/.ccache` (set in the Dockerfile) is inside the snapshot |

## 7. Directory structure

```
cpp-cmake-sandbox/
├── README.md                 # English documentation (this file)
├── README_zh.md              # Chinese documentation
├── Dockerfile                # C/C++ dev image on cubesandbox-base
├── requirements.txt          # Python dependencies (both SDKs)
├── .env.example              # Environment variable template
├── env_utils.py              # Shared .env loader (E2B scripts)
├── env.py                    # TEMPLATE_ID helper (native scripts)
├── seed.py                   # Pushes project/ into a sandbox
├── project/                  # Minimal C++17 CMake project
│   ├── CMakeLists.txt
│   ├── include/greeter.hpp
│   ├── src/greeter.cpp
│   ├── src/main.cpp
│   └── tests/test_greeter.cpp
├── 01_build_in_sandbox.py    # E2B: build + run
├── 02_run_ctest.py           # E2B: build + ctest
├── 03_ccache_snapshot.py     # Native: snapshot-persisted ccache + clone
└── 04_ccache_rollback.py     # Native: rollback to a warm-cache snapshot
```

## 8. See also

- [Bring Your Own Image (envd)](../../docs/guide/tutorials/bring-your-own-image.md)
- [Snapshot · Rollback · Clone](../snapshot-rollback-clone) — native SDK snapshot APIs
- [Code Sandbox Quickstart](../code-sandbox-quickstart) — the basic E2B flow
