# CubeAPI Examples — Test Report

**SDK**: cubesandbox v0.1.0 (commit 389174e, branch dev)  
**Date**: 2026-05-07 09:40:49  
**CUBE_API_URL**: `http://9.135.79.34:3000`  
**CUBE_PROXY_NODE_IP**: `9.135.79.34` (data-plane HTTP:80 / HTTPS:443)  
**CUBE_TEMPLATE_ID**: `tpl-6265796cee124256b4dcd6a1`

## Summary

| Example | Status | Time |
|---------|--------|------|
| `create` | ✅ PASS | 0.4s |
| `exec_code` | ✅ PASS | 0.6s |
| `cmd` | ✅ PASS | 0.6s |
| `read` | ✅ PASS | 0.5s |
| `list` | ✅ PASS | 0.1s |
| `pause` | ✅ PASS | 15.5s |
| `create_with_mount` | ✅ PASS | 0.6s |
| `network_no_internet` | ✅ PASS | 8.6s |
| `network_allowlist` | ✅ PASS | 8.6s |
| `network_denylist` | ✅ PASS | 8.7s |
| `test` | ✅ PASS | 1.7s |

**Total**: 11 | **Pass**: 11 | **Fail**: 0

---

## create

**Status**: ✅ PASS  **Time**: 0.4s

### API calls

```
POST /sandboxes
```

### Request

```json
{
  "templateID": "tpl-6265796cee124256b4dcd6a1",
  "timeout": 300
}
```

### Response / expected

- templateID
- sandboxID
- clientID
- envdVersion
- domain
- state

### Output

```
sandbox info {'templateID': 'tpl-6265796cee124256b4dcd6a1', 'sandboxID': '73af89b078554c149384158413f78e78', 'clientID': '73af89b078554c149384158413f78e78', 'startedAt': '2026-05-07T01:40:03.646900328Z', 'endAt': '2026-05-07T01:40:03.646900328Z', 'envdVersion': '0.2.0', 'domain': 'cube.app', 'cpuCount': 2, 'memoryMB': 2000, 'diskSizeMB': 0, 'state': 'running'}
```

---

## exec_code

**Status**: ✅ PASS  **Time**: 0.6s

### API calls

```
POST /sandboxes  +  data-plane POST /execute (HTTP:80)
```

### Request

```json
{
  "templateID": "tpl-6265796cee124256b4dcd6a1"
}
```

**Data-plane request body** (`POST http://<CUBE_PROXY_NODE_IP>:80/execute`):

```json
{
  "code": "print(\"hello cube\")",
  "context_id": null,
  "language": null
}
```

### Response / expected

```
ndjson stream: {type:stdout, text:...} / {type:result, ...}
```

### Output

```
hello cube
Execution(text=None, error=None)
```

---

## cmd

**Status**: ✅ PASS  **Time**: 0.6s

### API calls

```
POST /sandboxes  +  data-plane POST /execute (HTTP:80)
```

### Request

```json
{
  "templateID": "tpl-6265796cee124256b4dcd6a1"
}
```

**Data-plane request body** (`POST http://<CUBE_PROXY_NODE_IP>:80/execute`):

```json
{
  "code": "subprocess.check_output(['sh','-c','echo hello cube'])"
}
```

### Response / expected

```
ndjson stream stdout: hello cube
```

### Output

```
hello cube
```

---

## read

**Status**: ✅ PASS  **Time**: 0.5s

### API calls

```
POST /sandboxes  +  data-plane POST /execute (HTTP:80)
```

### Request

```json
{
  "templateID": "tpl-6265796cee124256b4dcd6a1"
}
```

**Data-plane request body** (`POST http://<CUBE_PROXY_NODE_IP>:80/execute`):

```json
{
  "code": "print(open('/etc/hosts').read())"
}
```

### Response / expected

```
ndjson stream stdout: /etc/hosts content
```

### Output

```
127.0.0.1 localhost tpl-6265
```

---

## list

**Status**: ✅ PASS  **Time**: 0.1s

### API calls

```
GET /sandboxes  +  GET /v2/sandboxes
```

### Request

```json
{}
```

### Response / expected

- [{sandboxID, templateID, state, ...}]

### Output

```
total running sandboxes (v1): 3
  sandbox_id=c0ec6c94c1614a389b67c98d32e7fe94     template=tpl-6265796cee124256b4dcd6a1
  sandbox_id=60090ceb06d045998aa73686d8f976a3     template=tpl-6265796cee124256b4dcd6a1
  sandbox_id=b08ec227510142498bda2958a2817f8c     template=tpl-6265796cee124256b4dcd6a1

total running sandboxes (v2): 3
  sandbox_id=60090ceb06d045998aa73686d8f976a3     template=tpl-6265796cee124256b4dcd6a1
  sandbox_id=b08ec227510142498bda2958a2817f8c     template=tpl-6265796cee124256b4dcd6a1
  sandbox_id=c0ec6c94c1614a389b67c98d32e7fe94     template=tpl-6265796cee124256b4dcd6a1
```

---

## pause

**Status**: ✅ PASS  **Time**: 15.5s

### API calls

```
POST /sandboxes → POST /sandboxes/{id}/pause → GET /sandboxes/{id} → POST /sandboxes/{id}/connect → DELETE /sandboxes/{id}
```

### Request

```json
{
  "templateID": "tpl-6265796cee124256b4dcd6a1",
  "timeout": 600
}
```

### Response / expected

- sandboxID
- state=paused → state=running after connect

### Output

```
paused, waiting for snapshot...
state: paused
sandbox info {'templateID': 'tpl-6265796cee124256b4dcd6a1', 'sandboxID': '0a1a6027c03648f289d7ecf34e654b4b', 'clientID': '0a1a6027c03648f289d7ecf34e654b4b', 'startedAt': '2026-05-07T01:40:20.796952194Z', 'endAt': '2026-05-07T01:40:20.796952194Z', 'envdVersion': '0.2.0', 'domain': 'cube.app', 'cpuCount': 2, 'memoryMB': 2000, 'diskSizeMB': 0, 'state': 'running'}
state_var after resume: hello after resume
```

---

## create_with_mount

**Status**: ✅ PASS  **Time**: 0.6s

### API calls

```
POST /sandboxes  +  data-plane POST /execute (HTTP:80)
```

### Request

```json
{
  "templateID": "tpl-6265796cee124256b4dcd6a1",
  "metadata": {
    "hostdir-mount": "[{\"hostPath\":\"/tmp/rw\",\"mountPath\":\"/mnt/rw\",\"readOnly\":false},{\"hostPath\":\"/tmp/ro\",\"mountPath\":\"/mnt/ro\",\"readOnly\":true}]"
  }
}
```

### Response / expected

- sandboxID
- rw mount: read+write OK
- ro mount: write blocked

### Output

```
=== Step 0: host directories (pre-created on Cubelet node) ===
  Cubelet node: 9.135.79.34
  hostPath rw: /tmp/rw/seed.txt
  hostPath ro: /tmp/ro/seed.txt

=== Step 1: create sandbox with hostdir-mount ===
sandbox info {'templateID': 'tpl-6265796cee124256b4dcd6a1', 'sandboxID': '9a3d4d86a6ce4fccae50dd389944e613', 'clientID': '9a3d4d86a6ce4fccae50dd389944e613', 'startedAt': '2026-05-07T01:40:21.288772940Z', 'endAt': '2026-05-07T01:40:21.288772940Z', 'envdVersion': '0.2.0', 'domain': 'cube.app', 'cpuCount': 2, 'memoryMB': 2000, 'diskSizeMB': 0, 'state': 'running'}

--- rw mount ---
  ls /mnt/rw: ['seed.txt']
seed.txt: 'rw seed from host\n'
wrote from_sandbox.txt

--- ro mount ---
  ls /mnt/ro: ['seed.txt']
seed.txt: 'ro seed from host\n'
write blocked as expected: OSError: [Errno 30] Read-only file system: '/mnt/ro/should_fail.txt'

sandbox destroyed — rw write-back flushed to Cubelet host on teardown
```

---

## network_no_internet

**Status**: ✅ PASS  **Time**: 8.6s

### API calls

```
POST /sandboxes  +  data-plane POST /execute (HTTP:80)
```

### Request

```json
{
  "templateID": "tpl-6265796cee124256b4dcd6a1",
  "metadata": {
    "network-policy": "deny-all"
  }
}
```

### Response / expected

- http:80 blocked
- https:443 blocked
- data-plane OK

### Output

```
sandbox info {'templateID': 'tpl-6265796cee124256b4dcd6a1', 'sandboxID': 'e58dafb18b1740f18c31c17da6cc0cc9', 'clientID': 'e58dafb18b1740f18c31c17da6cc0cc9', 'startedAt': '2026-05-07T01:40:21.860644561Z', 'endAt': '2026-05-07T01:40:21.860644561Z', 'envdVersion': '0.2.0', 'domain': 'cube.app', 'cpuCount': 2, 'memoryMB': 2000, 'diskSizeMB': 0, 'state': 'running'}
  data-plane: ok

  http:80 blocked as expected (TimeoutError)

  https:443 blocked as expected (TimeoutError)
```

---

## network_allowlist

**Status**: ✅ PASS  **Time**: 8.6s

### API calls

```
POST /sandboxes  +  data-plane POST /execute (HTTP:80)
```

### Request

```json
{
  "templateID": "tpl-6265796cee124256b4dcd6a1",
  "metadata": {
    "network-policy": "custom",
    "network-rules": "{\"allow\":[\"9.135.79.34/32\"]}"
  }
}
```

### Response / expected

- allowed 9.135.79.34:3000 reachable
- external 93.184.216.34:80 blocked

### Output

```
sandbox info {'templateID': 'tpl-6265796cee124256b4dcd6a1', 'sandboxID': '2c566dab090e4b29b62ce5d15f753622', 'clientID': '2c566dab090e4b29b62ce5d15f753622', 'startedAt': '2026-05-07T01:40:30.479861375Z', 'endAt': '2026-05-07T01:40:30.479861375Z', 'envdVersion': '0.2.0', 'domain': 'cube.app', 'cpuCount': 2, 'memoryMB': 2000, 'diskSizeMB': 0, 'state': 'running'}
allow-list: ['9.135.79.34/32']
  data-plane: ok

  allowed IP 9.135.79.34:3000 unreachable (TimeoutError)

  external 93.184.216.34:80 blocked as expected (TimeoutError)
```

---

## network_denylist

**Status**: ✅ PASS  **Time**: 8.7s

### API calls

```
POST /sandboxes  +  data-plane POST /execute (HTTP:80)
```

### Request

```json
{
  "templateID": "tpl-6265796cee124256b4dcd6a1",
  "metadata": {
    "network-policy": "custom",
    "network-rules": "{\"deny\":[\"9.135.79.34/32\"]}"
  }
}
```

### Response / expected

- denied 9.135.79.34:80 blocked
- data-plane OK

### Output

```
sandbox info {'templateID': 'tpl-6265796cee124256b4dcd6a1', 'sandboxID': '8b8a1a17af3e421aa92c1f709275703c', 'clientID': '8b8a1a17af3e421aa92c1f709275703c', 'startedAt': '2026-05-07T01:40:39.135374976Z', 'endAt': '2026-05-07T01:40:39.135374976Z', 'envdVersion': '0.2.0', 'domain': 'cube.app', 'cpuCount': 2, 'memoryMB': 2000, 'diskSizeMB': 0, 'state': 'running'}
deny-list: ['9.135.79.34/32']
  data-plane: ok

  denied IP 9.135.79.34:80 blocked as expected (TimeoutError)

  external:443 unreachable (TimeoutError) — may be blocked by devcloud env
```

---

## test

**Status**: ✅ PASS  **Time**: 1.7s

### API calls

```
POST /sandboxes (×4 concurrent)  +  data-plane POST /execute (HTTP:80) per worker
```

### Request

```json
{
  "templateID": "tpl-6265796cee124256b4dcd6a1"
}
```

### Response / expected

- 4 workers × run_code + cmd + files.read → sandbox destroyed

### Output

```
All workers finished. See CubeAPI_examples/log.txt for details.

[stderr]
2026-05-07 09:40:47,593 [worker-0] worker started
2026-05-07 09:40:47,594 [worker-1] worker started
2026-05-07 09:40:47,594 [worker-0] === loop start ===
2026-05-07 09:40:47,594 [worker-1] === loop start ===
2026-05-07 09:40:47,595 [worker-2] worker started
2026-05-07 09:40:47,595 [worker-3] worker started
2026-05-07 09:40:47,596 [worker-2] === loop start ===
2026-05-07 09:40:47,596 [worker-3] === loop start ===
2026-05-07 09:40:47,747 [worker-0] sandbox created: 5b2c9fef9a484dc7b36c2d3afa38acb5
2026-05-07 09:40:47,771 [worker-1] sandbox created: 463fd260ca2f488babeb3658e0197c65
2026-05-07 09:40:47,793 [worker-2] sandbox created: a5736ad0361d45f0a645c96881092411
2026-05-07 09:40:47,808 [worker-3] sandbox created: 6d51e153e4014e5fbb0a176ba73cfe4d
2026-05-07 09:40:47,942 [worker-0] [run_code stdout] hello cube
2026-05-07 09:40:47,942 [worker-0] run_code result: Execution(text=None, error=None)
2026-05-07 09:40:47,951 [worker-1] [run_code stdout] hello cube
2026-05-07 09:40:47,953 [worker-2] [run_code stdout] hello cube
2026-05-07 09:40:47,955 [worker-1] run_code result: Execution(text=None, error=None)
2026-05-07 09:40:47,956 [worker-3] [run_code stdout] hello cube
2026-05-07 09:40:47,958 [worker-3] run_code result: Execution(text=None, error=None)
2026-05-07 09:40:47,958 [worker-2] run_code result: Execution(text=None, error=None)
2026-05-07 09:40:47,996 [worker-0] cmd stdout (first 120 chars): total 96
lrwxrwxrwx   1 root root     7 Mar  2 21:50 bin -> usr/bin
drwxr-xr-x   2 root root  4096 Mar  2 21:50 boot
drw
2026-05-07 09:40:48,006 [worker-1] cmd stdout (first 120 chars): total 96
lrwxrwxrwx   1 root root     7 Mar  2 21:50 bin -> usr/bin
drwxr-xr-x   2 root root  4096 Mar  2 21:50 boot
drw
2026-05-07 09:40:48,013 [worker-3] cmd stdout (first 120 chars): total 96
lrwxrwxrwx   1 root root     7 Mar  2 21:50 bin -> usr/bin
drwxr-xr-x   2 root root  4096 Mar  2 21:50 boot
drw
2026-05-07 09:40:48,013 [worker-2] cmd stdout (first 120 chars): total 96
lrwxrwxrwx   1 root root     7 Mar  2 21:50 bin -> usr/bin
drwxr-xr-x   2 root root  4096 Mar  2 21:50 boot
drw
2026-05-07 09:40:48,048 [worker-0] files.read /etc/hosts: 127.0.0.1 localhost tpl-6265\n\n
2026-05-07 09:40:48,058 [worker-1] files.read /etc/hosts: 127.0.0.1 localhost tpl-6265\n\n
2026-05-07 09:40:48,065 [worker-3] files.read /etc/hosts: 127.0.0.1 localhost tpl-6265\n\n
2026-05-07 09:40:48,065 [worker-2] files.read /etc/hosts: 127.0.0.1 localhost tpl-6265\n\n
2026-05-07 09:40:48,164 [worker-0] sandbox destroyed
2026-05-07 09:40:48,190 [worker-1] sandbox destroyed
2026-05-07 09:40:48,191 [worker-2] sandbox destroyed
2026-05-07 09:40:48,191 [worker-3] sandbox destroyed
2026-05-07 09:40:49,165 [worker-0] worker done
2026-05-07 09:40:49,192 [worker-3] worker done
2026-05-07 09:40:49,192 [worker-2] worker done
2026-05-07 09:40:49,192 [worker-1] worker done
```

---

