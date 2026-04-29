# cube-e2b Example Run Results

**Date**: 2026-04-29 23:55:42

**Env**: `CUBE_API_URL=http://9.135.79.34:3000` `CUBE_PROXY_NODE_IP=9.135.79.34`

| Example | Status | Time |
|---------|--------|------|
| `create_and_run` | ✅ PASS | 1.5s |
| `lifecycle` | ✅ PASS | 19.8s |
| `volume` | ⚠️ UNIMPLEMENTED | 2.0s |
| `context` | ✅ PASS | 9.9s |
| `network_policy` | ✅ PASS | 41.4s |

---

## create_and_run

**Status**: ✅ PASS  **Time**: 1.5s

```
Created: Sandbox(id='d4265c4ebca24f19ab654fae9b5439ca', domain='cube.app')
result.text   = '6.2832'
  stdout: item 0
item 1
item 2
logs.stdout   = ['item 0\nitem 1\nitem 2\n']
error.name    = ZeroDivisionError
error.value   = division by zero
Sandbox destroyed.
sandbox_id = 7c08052326b1439391964e23661ff25e
sum(1..100) = 5050
```

## lifecycle

**Status**: ✅ PASS  **Time**: 19.8s

```
created  : 5dcaad83390a495c9cd1de7072b9cdde
paused
state after resume = 42
destroyed
```

## volume

**Status**: ⚠️ UNIMPLEMENTED  **Time**: 2.0s

> **Note**: `host-mount` is forwarded by CubeAPI as an annotation to CubeMaster,
> but CubeMaster's `injectHostDirMounts()` is not yet implemented.
> The sandbox starts normally, but `/mnt/data` is empty. This is a planned feature.

```
Preparing host directory on cube-devcloud …
  wrote hello.txt on cube-devcloud:/tmp/cube_volume_demo
Created: Sandbox(id='908ddbe9bd484c1e8fadd9f7ae69663c', domain='cube.app')
file content  = None   (mount not yet active)
host sees     = '__MISSING__'  (write-back also not active)
ls /mnt/data  = None
Sandbox destroyed.
```

## context

**Status**: ✅ PASS  **Time**: 9.9s

```
Created: Sandbox(id='f4a521d05dd94ce49ed6cfe9a5df1544', domain='cube.app')

--- without context ---
result.text     = '100'

--- with shared context ---
context id      = '6a4df409-5ace-4a1e-a131-23a515e2ee00'
x=100, y=x*2, x+y = '300'
sum(1..5)         = '15'

--- two independent contexts ---
ctx_a value = 'Alice'
ctx_b value = 'Bob'

--- streaming with context ---
  stdout: item 0
item 1
item 2
item 3
logs.stdout = ['item 0\nitem 1\nitem 2\nitem 3\n']

--- cleanup ---
contexts deleted

Sandbox destroyed.
```

## network_policy

**Status**: ✅ PASS  **Time**: 41.4s

```
=== allow-all ===
Created: Sandbox(id='9a5994bbf49b45abb7d219eab3dbff95', domain='cube.app')
  outbound: blocked (<urlopen error [Errno -3] Temporary failure in name resolution>)
Sandbox destroyed.

=== deny-all ===
Created: Sandbox(id='dc5857ff4c074911b3a234e987a7b377', domain='cube.app')
  outbound: blocked as expected (URLError)
Sandbox destroyed.

=== custom allow-list ===
Created: Sandbox(id='bc820e7286ff4304b0c64c9d6cd2fb97', domain='cube.app')
  pypi.org: blocked (<urlopen error [Errno -3] Temporary failure in name resolution>)
  example.com: blocked as expected (URLError)
Sandbox destroyed.
```

