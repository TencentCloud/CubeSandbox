# CubeSandbox Example Run Results

**SDK**: cubesandbox v0.1.0  
**Date**: 2026-05-07 09:26:29  
**Env**: `CUBE_API_URL=http://9.135.79.34:3000`  `CUBE_PROXY_NODE_IP=9.135.79.34`

## Summary

| Example | Status | Time |
|---------|--------|------|
| `list_and_health` | ✅ PASS | 0.4s |
| `create_and_run` | ❌ FAIL (exit 1) | 1.7s |
| `lifecycle` | ❌ FAIL (exit 1) | 19.4s |
| `context` | ❌ FAIL (exit 1) | 10.5s |
| `volume` | ❌ FAIL (exit 1) | 7.3s |
| `network_policy` | ❌ FAIL (exit 1) | 62.3s |

**Total**: 6 | **Pass**: 1 | **Fail**: 5

---

## list_and_health

**Status**: ✅ PASS  **Time**: 0.4s

```
=== Sandbox.health() ===
  result: {'status': 'ok', 'sandboxes': 0}
  ✅ health() returns dict
  ✅ health() has 'status' key

=== Sandbox.create() ===
  created sandbox_id='b9d7f7c1a1e44da7946e9c8ffdc615ea'
  ✅ create() sandbox_id not empty

=== Sandbox.list() [v1] ===
  returned 3 sandbox(es)
  ✅ list() is a list
  ✅ list() contains 'b9d7f7c1a1e44da7946e9c8ffdc615ea'

=== Sandbox.list_v2() [v2] ===
  returned 3 sandbox(es)
  ✅ list_v2() is a list
  ✅ list_v2() contains 'b9d7f7c1a1e44da7946e9c8ffdc615ea'

=== sb.kill() ===
  destroyed sandbox_id='b9d7f7c1a1e44da7946e9c8ffdc615ea'
  ✅ kill() succeeded

=== Sandbox.list() after kill [v1] ===
  ✅ list(): 'b9d7f7c1a1e44da7946e9c8ffdc615ea' absent after kill

=== Sandbox.list_v2() after kill [v2] ===
  ✅ list_v2(): 'b9d7f7c1a1e44da7946e9c8ffdc615ea' absent after kill

========================================
PASS
```

## create_and_run

**Status**: ❌ FAIL (exit 1)  **Time**: 1.7s

```
=== create + run_code ===
  created: Sandbox(id='09495131820b47eda5fa4ad76637a0d3', domain='cube.app')
  ✅ sandbox_id not empty
  ✅ math result
  ❌ stdout lines: got ['item 0\nitem 1\nitem 2\n']
  ✅ stderr callback
  ✅ error.name
  ✅ env_vars
Sandbox destroyed.

=== explicit Config ===
  ✅ sum(1..100)
  sandbox killed

========================================
FAIL
  - stdout lines: got ['item 0\nitem 1\nitem 2\n']
```

## lifecycle

**Status**: ❌ FAIL (exit 1)  **Time**: 19.4s

```
=== create ===
  created: 60090ceb06d045998aa73686d8f976a3
  ✅ sandbox_id not empty

=== get_info (GET /sandboxes/{id}) ===
  info: {'templateID': 'tpl-6265796cee124256b4dcd6a1', 'sandboxID': '60090ceb06d045998aa73686d8f976a3', 'clientID': '60090ceb06d045998aa73686d8f976a3', 'startedAt': '2026-05-07T01:24:49.880051649Z', 'endAt': '2026-05-07T01:24:49.880051649Z', 'envdVersion': '0.2.0', 'domain': 'cube.app', 'cpuCount': 2, 'memoryMB': 2000, 'diskSizeMB': 0, 'state': 'running'}
  ✅ get_info returns dict
  ✅ get_info sandboxID matches

=== set state before pause ===
  ✅ state set

=== pause (POST /sandboxes/{id}/pause) ===
  pause requested
  state after pause: 'paused'
  ✅ state == paused

=== connect (POST /sandboxes/{id}/connect) ===
  persistent_value = '42'
  ✅ state persisted across pause/connect

=== resume deprecated (POST /sandboxes/{id}/resume) ===

[stderr]
Traceback (most recent call last):
  File "/root/.openclaw/workspace/cube-e2b-v2/examples/lifecycle.py", line 87, in <module>
    sb2.pause()
  File "/root/.openclaw/workspace/cube-e2b-v2/examples/../cubesandbox/sandbox.py", line 294, in pause
    _check_response(resp)
  File "/root/.openclaw/workspace/cube-e2b-v2/examples/../cubesandbox/sandbox.py", line 29, in _check_response
    raise ApiError(msg, code)
cubesandbox._exceptions.ApiError: CubeMaster returned error code 130490: sandbox is terminating
```

## context

**Status**: ❌ FAIL (exit 1)  **Time**: 10.5s

```
Created: Sandbox(id='5901057405184067b00d2d1842aaef5f', domain='cube.app')

--- A: without context (no state persistence) ---
  result.text = '100' (envd may share global state)
  ✅ no-context: result returned

--- B: with shared context ---
  context id = '84d6a871-30b4-4fad-b179-027ffc19cf43'
  ✅ create_context returns id
  x=100, y=x*2, x+y = '300'
  ✅ context: x + y == 300
  sum(1..5) = '15'
  ✅ context: sum(1..5) == 15

--- C: two independent contexts ---
  ctx_a.value = 'Alice'
  ctx_b.value = 'Bob'
  ✅ ctx_a isolated
  ✅ ctx_b isolated

--- D: streaming with context ---
  stdout captured: ['item 0\nitem 1\nitem 2\nitem 3\n']
  ❌ streaming: 4 stdout lines: got ['item 0\nitem 1\nitem 2\nitem 3\n']

--- E: delete contexts ---
  all contexts deleted
  ✅ delete_context no error

Sandbox destroyed.

========================================
FAIL
  - streaming: 4 stdout lines: got ['item 0\nitem 1\nitem 2\nitem 3\n']
```

## volume

**Status**: ❌ FAIL (exit 1)  **Time**: 7.3s

```
=== Preparing host directories on Cubelet node ===
  ssh setup: 'Permission denied, please try again.\nReceived disconnect from 9.135.79.34 port 36000:2: Too many authentication failures\nDisconnected from 9.135.79.34 port 36000' (rc=255)
  ❌ host dir setup: Permission denied, please try again.
Received disconnect from 9.135.79.34 port 36000:2: Too many authentication failures
Disconnected from 9.135.79.34 port 36000

=== readOnly=False (read + write) ===

[stderr]
Traceback (most recent call last):
  File "/root/.openclaw/workspace/cube-e2b-v2/examples/volume.py", line 72, in <module>
    with Sandbox.create(metadata={"hostdir-mount": mounts_rw}) as sb:
         ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
  File "/root/.openclaw/workspace/cube-e2b-v2/examples/../cubesandbox/sandbox.py", line 107, in create
    _check_response(resp)
  File "/root/.openclaw/workspace/cube-e2b-v2/examples/../cubesandbox/sandbox.py", line 29, in _check_response
    raise ApiError(msg, code)
cubesandbox._exceptions.ApiError: CubeMaster returned error code 130545: prepareHostDirVolume: bind mount /tmp/cube_volume_rw -> /data/cubelet/hostdir/6ee0e97cd2d345059018bd9941958ff5/rw/hostdir-0: no such file or directory
```

## network_policy

**Status**: ❌ FAIL (exit 1)  **Time**: 62.3s

```
=== allow-all (outbound HTTP port 80) ===
  Created: Sandbox(id='fc5d80e94ee04016b67daaf4ce942835', domain='cube.app')
  http: blocked (URLError: <urlopen error [Errno -3] Temporary failure in name resolution>)
  ❌ allow-all HTTP port 80 reachable: stdout=['http: blocked (URLError: <urlopen error [Errno -3] Temporary failure in name resolution>)\n']

=== allow-all (outbound HTTPS port 443) ===
  Created: Sandbox(id='8951b40bf1b84d5381cf665f28b08bc2', domain='cube.app')
  https: blocked (URLError: <urlopen error [Errno -3] Temporary failure in name resolution>)
  ❌ allow-all HTTPS port 443 reachable: stdout=['https: blocked (URLError: <urlopen error [Errno -3] Temporary failure in name resolution>)\n']

=== deny-all (outbound HTTP port 80 blocked) ===
  Created: Sandbox(id='5966ba062316476983ed4626ad6accfa', domain='cube.app')
  http: blocked as expected (URLError)
  ✅ deny-all HTTP port 80 blocked

=== deny-all (outbound HTTPS port 443 blocked) ===
  Created: Sandbox(id='3044dbc29b3e4d568e880146d159170d', domain='cube.app')
  https: blocked as expected (URLError)
  ✅ deny-all HTTPS port 443 blocked

=== custom allow-list (pypi.org allowed, example.com blocked) ===
  Created: Sandbox(id='a10995a2836041dabb4585890920a163', domain='cube.app')
  pypi.org: blocked (URLError)
  example.com: blocked as expected (URLError)
  ❌ custom: pypi.org reachable: stdout=['pypi.org: blocked (URLError)\n', 'example.com: blocked as expected (URLError)\n']
  ✅ custom: example.com blocked

All sandboxes destroyed.

========================================
FAIL
  - allow-all HTTP port 80 reachable: stdout=['http: blocked (URLError: <urlopen error [Errno -3] Temporary failure in name resolution>)\n']
  - allow-all HTTPS port 443 reachable: stdout=['https: blocked (URLError: <urlopen error [Errno -3] Temporary failure in name resolution>)\n']
  - custom: pypi.org reachable: stdout=['pypi.org: blocked (URLError)\n', 'example.com: blocked as expected (URLError)\n']
```

