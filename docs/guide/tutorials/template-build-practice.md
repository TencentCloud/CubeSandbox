# Local and Remote Image Template Practice

This guide walks through two complete workflows for creating custom CubeSandbox templates:

- **Option 1: Build a local image** (the image remains on the current machine and does not need to be pushed)
- **Option 2: Use a remote image** (the image is pushed to a registry and pulled by the cluster)

See also [Create Templates from OCI Image](./template-from-image.md) and [Custom Template Images](./bring-your-own-image.md).


## Prerequisites

- `cubemastercli` is installed and available in `$PATH`.
- Docker is installed.
- CubeMaster is running (`cubemastercli tpl list` returns successfully).
- You know the path to the mkcert CA certificate required by the SDK when connecting to a sandbox:

```bash
# Usually located here
ls ~/.local/share/mkcert/rootCA.pem
```


## Option 1: Create a template from a locally built image

This option is suitable for local development and debugging on the **machine running CubeMaster**. The image does not need to be pushed to a remote registry.

### Step 1: Write a Dockerfile

This guide operates the sandbox through the CubeSandbox SDK and E2B SDK and uses the `envd` endpoint at `49983/health` as the template probe. The example therefore builds on the official `cubesandbox-base` image, which includes `envd`.

```dockerfile
FROM ghcr.io/tencentcloud/cubesandbox-base:latest

# Install the tools and dependencies you need
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 python3-pip curl wget git vim jq \
    && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir numpy pandas requests httpx
```

Save it as `/tmp/Dockerfile.cube-test`.

### Step 2: Build the image

```bash
docker build -f /tmp/Dockerfile.cube-test -t my-sandbox:v1 .
```

### Step 3: Verify that envd is healthy

Before creating the template, run the image locally and verify that `envd` responds on `/health`:

```bash
cid=$(docker run -d my-sandbox:v1)
sleep 2
docker exec "$cid" curl -s -o /dev/null -w "envd /health => %{http_code}\n" http://127.0.0.1:49983/health
docker rm -f "$cid"
```

**Expected output: `envd /health => 204`**

If the status is not 204, check that the Dockerfile has the correct `ENTRYPOINT` (see [Troubleshooting](#troubleshooting)).

### Step 4: Create the template

```bash
cubemastercli tpl create-from-image \
  --image my-sandbox:v1 \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

The command immediately returns a `job_id` and `template_id`:

```text
job_id:      718b7ebd-5a2c-4f33-85d0-1c36f0d1b3ee
template_id: tpl-01adfa335c03460cb4a09225
status:      PENDING
phase:       PULLING
```

### Step 5: Wait for the template to become ready

```bash
cubemastercli tpl watch --job-id <job_id>
```

The template is ready when the command reports `status: READY`:

```text
status:       READY
phase:        READY
progress:     100%
distribution: 1/1 ready, 0 failed
```

### Step 6: Verify the template

Use either SDK to verify the template.

#### Option A: e2b_code_interpreter (requires an SSL certificate)

```bash
export CUBE_TEMPLATE_ID=<template_id>
export E2B_API_URL=http://127.0.0.1:3000
export E2B_API_KEY=e2b_000000
export SSL_CERT_FILE=~/.local/share/mkcert/rootCA.pem

python3 - << 'EOF'
import os
from e2b_code_interpreter import Sandbox
with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"]) as sb:
    r = sb.commands.run("python3 --version && echo hello-cube")
    print(r.stdout)
EOF
```

**Expected output:**

```text
Python 3.x.x
hello-cube
```

#### Option B: CubeSandbox SDK (recommended; no SSL certificate required)

If the CubeSandbox SDK is not installed or installation fails, see [Troubleshooting](#troubleshooting).

```bash
export CUBE_API_URL=http://127.0.0.1:3000
export CUBE_TEMPLATE_ID=<template_id>
export CUBE_PROXY_NODE_IP=127.0.0.1   # Local machine; use the CubeProxy node IP for remote access

python3 - << 'EOF'
import os, time
from cubesandbox import Sandbox, Config
from cubesandbox._exceptions import ApiError

cfg = Config(
    api_url=os.environ["CUBE_API_URL"],
    template_id=os.environ["CUBE_TEMPLATE_ID"],
    proxy_node_ip=os.environ.get("CUBE_PROXY_NODE_IP", ""),
)

def run_with_retry(sb, code, max_retries=10, interval=1.0):
    for i in range(max_retries):
        try:
            return sb.run_code(code)
        except ApiError as e:
            if e.status_code == 502 and i < max_retries - 1:
                time.sleep(interval)
            else:
                raise

with Sandbox.create(config=cfg) as sb:
    r = run_with_retry(sb, 'import sys; print(sys.version); print("hello-cube")')
    for line in r.logs.stdout:
        print(line, end="")
EOF
```

**Expected output:**

```text
Python 3.x.x
hello-cube
```

> [!WARNING]
> The CubeSandbox SDK `run_code` method depends on a Jupyter kernel on port 49999. Install `jupyter_kernel_gateway ipykernel` in the custom image and expose both ports 49983 and 49999 when creating the template.


## Option 2: Create a template from a remote image

This option is suitable for **sharing images across a team** or running a **multi-node cluster**. After the image is pushed to a registry, every cluster node can pull it.

### Step 1: Write a Dockerfile

Use the same Dockerfile as in Option 1.

### Step 2: Build the image with a registry prefix

```bash
docker build -f /tmp/Dockerfile.cube-test \
  -t ccr.ccs.tencentyun.com/<namespace>/<image-name>:v1 .
```

### Step 3: Verify that envd is healthy

Follow Step 3 in Option 1.

### Step 4: Log in and push the image

```bash
# Log in if the registry requires authentication
docker login ccr.ccs.tencentyun.com

# Push the image
docker push ccr.ccs.tencentyun.com/<namespace>/<image-name>:v1
```

### Step 5: Create the template

```bash
cubemastercli tpl create-from-image \
  --image ccr.ccs.tencentyun.com/<namespace>/<image-name>:v1 \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

For a private registry, provide credentials:

```bash
cubemastercli tpl create-from-image \
  --image ccr.ccs.tencentyun.com/<namespace>/<image-name>:v1 \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health \
  --registry-username <username> \
  --registry-password <password>
```

### Steps 6 and 7: Wait and verify

Follow Steps 5 and 6 in Option 1.


## Comparison

| | Local image | Remote image |
|---|---|---|
| **Push required** | No | Yes |
| **Best for** | Single-node development and debugging | Team sharing and multi-node clusters |
| **Image name format** | `my-sandbox:v1` | `registry/ns/image:tag` |
| **Private registry authentication** | Not required | May require `--registry-username/password` |
| **Speed** | Fast (read directly from the local machine) | Depends on network speed and image size |


## Troubleshooting

### 1. apt-get reports `Temporary failure resolving`

**Symptom:** `apt-get` cannot resolve domain names during `docker build`.

**Cause:** Docker containers use `8.8.8.8` as DNS by default, which may be inaccessible from an internal network. The `--dns` option is not supported by legacy `docker build` (without buildx), and Docker overwrites `/etc/resolv.conf`.

**Solution:** Resolve the internal apt mirror IP and put it directly in `sources.list`:

```bash
# 1. Resolve the mirror IP on the host
nslookup mirrors.tencent.com 9.218.233.130 | grep Address | tail -1
# => Address: 30.163.240.137

# 2. Replace the apt sources in the Dockerfile
RUN sed -i 's|http://archive.ubuntu.com/ubuntu|http://30.163.240.137/ubuntu|g' /etc/apt/sources.list && \
    sed -i 's|http://security.ubuntu.com/ubuntu|http://30.163.240.137/ubuntu|g' /etc/apt/sources.list
```

The same approach applies to pip:

```dockerfile
RUN pip install --no-cache-dir \
    -i http://30.163.240.137/pypi/simple/ \
    --trusted-host 30.163.240.137 \
    numpy pandas
```

### 2. envd `/health` does not return 204

**Symptom:** The Step 3 `curl` command returns a status other than 204 or refuses the connection.

**Cause:** The image's `ENTRYPOINT` or `CMD` overrides `cube-entrypoint.sh`, so `envd` does not start.

**Solution:** Make sure the Dockerfile uses `cube-entrypoint.sh`:

```dockerfile
ENTRYPOINT ["/usr/local/bin/cube-entrypoint.sh"]
CMD ["your-app-command"]
```

Alternatively, start envd from a custom entrypoint:

```bash
/usr/bin/envd -port 49983 >/var/log/envd.log 2>&1 &
exec "$@"
```

### 3. The SDK reports `SSL: CERTIFICATE_VERIFY_FAILED`

**Symptom:** The Python SDK reports a certificate error when calling the sandbox.

**Cause:** The SDK accesses a sandbox hostname under `*.cube.app` over HTTPS, but the machine does not trust Cube's built-in mkcert CA.

**Solution:** Point `SSL_CERT_FILE` to the CA certificate:

```bash
export SSL_CERT_FILE=~/.local/share/mkcert/rootCA.pem
```

Or temporarily disable verification in a test environment only:

```python
import ssl, warnings
warnings.filterwarnings('ignore')
ssl._create_default_https_context = ssl._create_unverified_context
```

### 4. The template remains in `phase: PULLING`

**Symptom:** `tpl watch` remains in the PULLING phase.

**Cause:** The CubeMaster node cannot pull the image because of network or registry authentication issues.

**Diagnosis:**

```bash
# Inspect last_error
cubemastercli tpl status --job-id <job_id> --json | jq '.last_error'

# Verify the pull directly on the CubeMaster node
docker pull <image-address>
```

For a local image that was not pushed, CubeMaster reads it directly from local Docker and does not need network access. Check the image name:

```bash
docker images | grep <image-name>
```

### 5. The template enters `status: FAILED` during BUILDING

**Symptom:** The template fails after entering the BUILDING phase.

**Diagnosis:**

```bash
cubemastercli tpl status --job-id <job_id> --json | jq '.last_error'
```

Common causes include:

- `--writable-layer-size` is too small and the build runs out of space.
- The node is low on disk space; check with `df -h`.
