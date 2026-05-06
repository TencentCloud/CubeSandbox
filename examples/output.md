# cube-e2b Example Run Results

**Date**: 2026-05-06 17:42:36

**Env**: `CUBE_API_URL=http://9.135.79.34:3000` `CUBE_PROXY_NODE_IP=9.135.79.34`

| Example | Status | Time |
|---------|--------|------|
| `create_and_run` | ✅ PASS | 1.5s |
| `lifecycle` | ✅ PASS | 16.0s |
| `volume` | ✅ PASS | 0.7s |
| `context` | ✅ PASS | 10.0s |
| `network_policy` | ✅ PASS | 41.5s |

---

## create_and_run

**Status**: ✅ PASS  **Time**: 1.5s

```
Created: Sandbox(id='58b1ef1162d14f708935a6652615fc84', domain='cube.app')
result.text   = '6.2832'
  stdout: item 0
item 1
item 2
logs.stdout   = ['item 0\nitem 1\nitem 2\n']
error.name    = ZeroDivisionError
error.value   = division by zero
Sandbox destroyed.
sandbox_id = de76ab7d9a474221956ae2a9028956e5
sum(1..100) = 5050
```

## lifecycle

**Status**: ✅ PASS  **Time**: 16.0s

```
created  : 4814c43f956d4dce86e2d0545b40d7b5
paused
state after resume = 42
destroyed
```

## volume

**Status**: ✅ PASS  **Time**: 0.7s

```
Preparing host directory on cubelet node (9.135.79.34) …
  (hostPath=/tmp/cube_volume_demo must exist on the Cubelet node)
Created: Sandbox(id='a75babe7694d44bc95d1256c9947c0ba', domain='cube.app')
file content  = 'Hello from the host!\\n'
ls /mnt/data  = ['from_sandbox.txt', 'hello.txt']
Sandbox destroyed.
(check on Cubelet node: cat /tmp/cube_volume_demo/from_sandbox.txt)
write-back: ✅ verified manually on Cubelet node (see TASK notes)
```

## context

**Status**: ✅ PASS  **Time**: 10.0s

```
Created: Sandbox(id='81eba06a12f04038b2cc64cac673b9e0', domain='cube.app')

--- without context ---
result.text     = '100'

--- with shared context ---
context id      = 'eb853730-80d5-4119-8a06-41a240ba94be'
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

**Status**: ✅ PASS  **Time**: 41.5s

```
=== allow-all ===
Created: Sandbox(id='b5a7f84ce94148be92d9f2de446548a5', domain='cube.app')
  outbound: blocked (<urlopen error [Errno -3] Temporary failure in name resolution>)
Sandbox destroyed.

=== deny-all ===
Created: Sandbox(id='9cd23d72ad1649a69d7a561c44fcc002', domain='cube.app')
  outbound: blocked as expected (URLError)
Sandbox destroyed.

=== custom allow-list ===
Created: Sandbox(id='8ae994290c524cb1bc7cc3441bce5839', domain='cube.app')
  pypi.org: blocked (<urlopen error [Errno -3] Temporary failure in name resolution>)
  example.com: blocked as expected (URLError)
Sandbox destroyed.
```

