# cube-e2b Example Run Results

**Date**: 2026-05-06 09:51:22

**Env**: `CUBE_API_URL=http://9.135.79.34:3000` `CUBE_PROXY_NODE_IP=9.135.79.34`

| Example | Status | Time |
|---------|--------|------|
| `create_and_run` | ✅ PASS | 1.6s |
| `lifecycle` | ✅ PASS | 17.5s |
| `volume` | ✅ PASS | 0.6s |
| `context` | ✅ PASS | 10.1s |
| `network_policy` | ✅ PASS | 41.5s |

---

## create_and_run

**Status**: ✅ PASS  **Time**: 1.6s

```
Created: Sandbox(id='87b1fe24c58f42aba9fc5281be2bee82', domain='cube.app')
result.text   = '6.2832'
  stdout: item 0
item 1
item 2
logs.stdout   = ['item 0\nitem 1\nitem 2\n']
error.name    = ZeroDivisionError
error.value   = division by zero
Sandbox destroyed.
sandbox_id = b0444b3ec5e7496b9f6d01061463c199
sum(1..100) = 5050
```

## lifecycle

**Status**: ✅ PASS  **Time**: 17.5s

```
created  : 5c326c5f2c3f4588a265e522a91c4d8e
paused
state after resume = 42
destroyed
```

## volume

**Status**: ✅ PASS  **Time**: 0.6s

```
Preparing host directory on cubelet node (9.135.79.34) …
  (hostPath=/tmp/cube_volume_demo must exist on the Cubelet node)
Created: Sandbox(id='ea2d5dc5a81b42fc821fe13c8608764c', domain='cube.app')
file content  = 'Hello from the host!\\n'
ls /mnt/data  = ['from_sandbox.txt', 'hello.txt']
Sandbox destroyed.
(check on Cubelet node: cat /tmp/cube_volume_demo/from_sandbox.txt)
write-back: ✅ verified manually on Cubelet node (see TASK notes)
```

## context

**Status**: ✅ PASS  **Time**: 10.1s

```
Created: Sandbox(id='737d5a3f5feb4f5cb4323ef90ea2fa14', domain='cube.app')

--- without context ---
result.text     = '100'

--- with shared context ---
context id      = '0e5bdbc1-b210-47e6-8917-e693b4cbc1d5'
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
Created: Sandbox(id='feedbcf5cc404edfb8b01982c9002b82', domain='cube.app')
  outbound: blocked (<urlopen error [Errno -3] Temporary failure in name resolution>)
Sandbox destroyed.

=== deny-all ===
Created: Sandbox(id='1a9515873cff4fb3b5cb8cb5f08b639a', domain='cube.app')
  outbound: blocked as expected (URLError)
Sandbox destroyed.

=== custom allow-list ===
Created: Sandbox(id='ab30f0179b234bc4b676752e88669c92', domain='cube.app')
  pypi.org: blocked (<urlopen error [Errno -3] Temporary failure in name resolution>)
  example.com: blocked as expected (URLError)
Sandbox destroyed.
```

