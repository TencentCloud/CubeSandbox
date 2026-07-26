# ivshmem

Minimal examples for using `ivshmem` as an optional host/guest shared-memory
channel in CubeSandbox.

These examples are meant to answer three practical questions:

1. How do I enable `ivshmem` for a template?
2. Where does the shared-memory region appear on the host and in the guest?
3. How can I build a simple protocol on top of that shared memory?

## What you get

| File | What it shows |
| --- | --- |
| `ivshmem_ring_demo.py` | A minimal host/guest request-reply flow over two small ring buffers |
| `ivshmem_benchmark.py` | Host-side mmap throughput against `/dev/shm/ivshmem-{sandbox_id}` |

## 1. Background

CubeSandbox keeps `ivshmem` as a template-level opt-in.

When a template is built with `enableIvshmem=true`, each sandbox created from
that template gets its own host-side backend file:

```text
/dev/shm/ivshmem-{sandbox_id}
```

Inside the guest, the same shared-memory region is exposed as an `ivshmem` PCI
device. A guest process can open the BAR resource file and use `mmap()` to
exchange data with the host.

This makes `ivshmem` a good fit for custom data paths where you want to define
your own in-memory layout or ring-buffer protocol between host and guest.

## 2. Prerequisites

- A running CubeSandbox deployment
- Python 3.8+
- An image that can be used to build a template

Install dependencies:

```bash
pip install "cubesandbox>=0.5.0"
```

Or install from the repository helper files if you prefer:

```bash
pip install -r ../code-sandbox-quickstart/requirements.txt
```

## 3. Quick Start

### Step 1 — Build an ivshmem-enabled Template

Use the Python SDK:

```python
from cubesandbox import Template

job = Template.build(
    image="cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest",
    enable_ivshmem=True,
)

template_id = job.template_id
```

Or use `cubemastercli`:

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --enable-ivshmem
```

Note the `template_id` printed on success.

### Step 2 — Run the Ring Demo

Create a temporary sandbox from the template and run the end-to-end demo:

```bash
python examples/ivshmem/ivshmem_ring_demo.py \
  --template <ivshmem_template_id> \
  --message "ping from host" \
  --cleanup
```

Expected output:

```text
sandbox_id: ...
host shm:   /dev/shm/ivshmem-...
host sent:  ping from host
guest received: ping from host
host recv:  hello-from-guest: ping from host
```

### Step 3 — Run the Host-side mmap Benchmark

```bash
python examples/ivshmem/ivshmem_benchmark.py \
  --template <ivshmem_template_id> \
  --count 3 \
  --cleanup
```

This benchmark measures host access to `/dev/shm/ivshmem-{sandbox_id}`. It is
useful for checking host-side backend behavior across repeated sandbox runs.

## 4. How the Shared Memory Appears

### On the host

Each sandbox gets its own backend file:

```text
/dev/shm/ivshmem-{sandbox_id}
```

The host can open that file and map it directly:

```python
import mmap

shm_path = f"/dev/shm/ivshmem-{sandbox_id}"

with open(shm_path, "r+b", buffering=0) as f:
    mm = mmap.mmap(f.fileno(), 1024 * 1024)
    mm[:16] = b"hello-from-host"
    mm.flush()
    mm.close()
```

### In the guest

The guest sees an `ivshmem` PCI device with vendor/device `0x1af4/0x1110`.
The shared-memory BAR is exposed as `resource2`.

This snippet finds the device and maps the BAR:

```python
import mmap
import os

resource = None
for name in os.listdir("/sys/bus/pci/devices"):
    d = f"/sys/bus/pci/devices/{name}"
    try:
        vendor = open(f"{d}/vendor").read().strip()
        device = open(f"{d}/device").read().strip()
    except OSError:
        continue
    if vendor == "0x1af4" and device == "0x1110":
        resource = f"{d}/resource2"
        break

if resource is None:
    raise RuntimeError("ivshmem PCI device not found")

with open(resource, "r+b", buffering=0) as f:
    mm = mmap.mmap(f.fileno(), 1024 * 1024)
    print(bytes(mm[:16]))
    mm.close()
```

## 5. Example Design

### `ivshmem_ring_demo.py`

This script shows a minimal protocol shape for application code:

- one host -> guest ring buffer
- one guest -> host ring buffer
- host writes one message
- guest reads it from `resource2`
- guest writes one reply
- host reads the reply from `/dev/shm/ivshmem-{sandbox_id}`

Use this example when you want to see how to:

- create or connect to a sandbox
- wait for the host shm file
- map the same memory on both sides
- define offsets and slot layout
- exchange messages over a small shared-memory protocol

### `ivshmem_benchmark.py`

This script focuses only on the host-side backend file. It does not run a
guest protocol. Use it when you want to:

- benchmark repeated host-side mmap writes
- compare multiple temporary sandboxes
- run several host-side benchmarks in parallel

## 6. When to Build on This Example

These examples are intentionally small and readable. They are a starting point
for building your own shared-memory data path, for example:

- request-reply control messages
- ring-buffered streaming between host and guest
- shared descriptors, indexes, or frame metadata
- custom zero-copy application protocols

For production guest-side hot paths, you will usually want to move the guest
logic into a lower-overhead implementation such as C, C++, or Rust and choose
your own synchronization strategy.

## 7. Troubleshooting

| Symptom | Likely Cause | What to check |
| --- | --- | --- |
| `/dev/shm/ivshmem-{sandbox_id}` does not appear | The template was not built with `enableIvshmem=true` | Rebuild the template with ivshmem enabled |
| Guest cannot find vendor/device `0x1af4/0x1110` | The sandbox was created from a non-ivshmem template | Confirm the sandbox template ID |
| `resource2` is missing | The guest did not expose the ivshmem BAR as expected | Check the PCI device under `/sys/bus/pci/devices/*` |
| Ring demo times out | Host and guest are not using the same layout or offsets | Check ring offsets, slot size, and payload size assumptions |

## See also

- [examples/snapshot-rollback-clone](../snapshot-rollback-clone) for lifecycle-oriented Python SDK examples
- [examples/code-sandbox-quickstart](../code-sandbox-quickstart) for the basic SDK flow
- SDK source: [`sdk/python/cubesandbox`](../../sdk/python/cubesandbox)
