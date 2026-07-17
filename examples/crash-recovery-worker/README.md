# Crash Recovery Worker

[中文文档](README_zh.md)

This example demonstrates checkpoint-based continuation for a long-running
stateful worker. A deterministic workload evolves the same worker across three
epochs. Every epoch creates a checkpoint, commits additional work, deliberately
aborts halfway through another transfer, rolls the same sandbox back, replays
the lost work, and continues from the recovered state.

CubeSandbox does not make application transactions atomic. The application
must create its checkpoint at a stable boundary. CubeSandbox then restores the
captured process memory and writable filesystem when the later execution
becomes corrupted.

## Scenario

The worker keeps these structures in memory:

- initial and current account balances;
- pending transfers;
- a committed ledger;
- seen request IDs for idempotency;
- committed, duplicate, and fault counters.

It also mirrors state transitions to
`/workspace/crash-recovery/audit.jsonl`. Each update is flushed through an
atomic file replacement before the request completes or the fault is raised.

The default workload runs three epochs. Each epoch follows this flow:

```text
seeded transfers x4 -- verify each --> checkpoint Cn
                                            |
                                            v
                          seeded transfers x2 -- verify each
                                            |
                                            v
                          debit the next transfer and abort
                                            |
                                            v
                           verify the incomplete durable audit
                                            |
                                            v
                                    rollback(Cn)
                                            |
                                            v
                     verify exact memory + workspace equality
                                            |
                                            v
                  replay lost transfers + retry faulted transfer
                                            |
                                            v
                           verify duplicate handling, continue
```

Transfers are pseudo-random but use a fixed seed. The generator tracks a host
side balance model and only produces transfers whose source account can pay the
selected amount. The post-checkpoint transfers are intentionally committed
before the crash; rollback removes them, and the driver submits the same
request IDs again to demonstrate checkpoint continuation.

## Verified invariants

`run_demo.py` independently checks:

- every successful or duplicate request exactly matches a host-side reference
  model before the workload advances;
- the total balance is unchanged;
- replaying the ledger from the initial balances reproduces current balances;
- ledger request IDs are unique;
- `seen` equals the committed ledger ID set;
- pending and committed request IDs do not overlap;
- committed count equals ledger length;
- audit initialization matches the in-memory initial balances;
- audit committed transfers equal the in-memory ledger;
- stable state has no incomplete or faulted audit entry;
- each rollback restores the exact checkpoint state and the byte-for-byte audit
  file;
- post-checkpoint request IDs disappear after rollback and commit again during
  replay;
- duplicate retry changes neither balances nor ledger;
- the final commit, duplicate, and fault counters match the complete
  multi-epoch workload.

Before every rollback, it also verifies that the epoch's faulted transfer has a
`started` and `fault_injected` event but no `committed` event, that all earlier
committed history is still present, and that the recorded balance total is
lower by exactly the debited amount.

## Files

| File | Purpose |
| --- | --- |
| `src/domain.rs` | Transfer state machine and atomic audit persistence |
| `src/http.rs` | HTTP API and fault terminator |
| `src/lib.rs` | Public module exports |
| `src/main.rs` | Worker process entry point |
| `run_demo.py` | CubeSandbox lifecycle and independent consistency verification |
| `test_run_demo.py` | Host-side invariant tests |
| `tests/` | Rust domain, HTTP, binary, and process-termination tests |
| `Dockerfile` | Multi-stage CubeSandbox template image |

## Build and test locally

```bash
cargo test
python3 -m unittest -v test_run_demo.py
docker build -t cubesandbox-crash-recovery-worker:latest .
```

To smoke-test the worker in Docker:

```bash
docker run --rm -d \
  --name cubesandbox-crash-recovery-worker \
  -p 18080:8080 \
  cubesandbox-crash-recovery-worker:latest \
  /usr/local/bin/crash-recovery-worker

curl http://127.0.0.1:18080/health
curl http://127.0.0.1:18080/state

docker rm -f cubesandbox-crash-recovery-worker
```

## Register the template

Push the image to a registry reachable by your CubeSandbox nodes, then run:

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/cubesandbox-crash-recovery-worker:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --expose-port 8080 \
  --probe 49983 \
  --probe-path /health
```

The readiness probe targets `envd` on port `49983`. `run_demo.py` starts the
worker as a child process after sandbox creation, so aborting the worker does
not stop `envd` or destroy the sandbox before rollback.

The driver also creates the sandbox with public traffic disabled and attaches
the per-sandbox traffic token to Worker requests. The fault-injection endpoint
is therefore not anonymously reachable through CubeProxy.

## Run against CubeSandbox

```bash
python3 -m venv .venv
. .venv/bin/activate
pip install -r requirements.txt

cp .env.example .env
# Set CUBE_TEMPLATE_ID and adjust the other CubeSandbox values if needed.

python3 run_demo.py
```

If the machine running the driver cannot resolve the sandbox wildcard domain,
set `CUBE_PROXY_NODE_IP` and optionally `CUBE_PROXY_PORT_HTTP`. Worker requests
will connect directly to that HTTP endpoint while preserving the virtual
sandbox hostname in the `Host` header for CubeProxy routing.

The workload can be extended without changing the code:

```bash
python3 run_demo.py \
  --cycles 5 \
  --transfers-before-checkpoint 8 \
  --transfers-after-checkpoint 3 \
  --seed 42
```

| Option | Default | Purpose |
| --- | ---: | --- |
| `--cycles` | `3` | Number of checkpoint, fault, rollback, and replay epochs |
| `--transfers-before-checkpoint` | `4` | Verified transfers accumulated before each checkpoint |
| `--transfers-after-checkpoint` | `2` | Successful transfers deliberately lost and replayed after rollback |
| `--seed` | `20260717` | Seed for the reproducible legal-transfer generator |

Expected final output:

```text
[epoch 3/3] Rollback verified: snapshot=... discarded=2 state=... audit=...
[epoch 3/3] Fault retry verified: id=tx-fault-03 ledger=21 state=...
[epoch 3/3] Duplicate verified: id=tx-fault-03 duplicates=3
[epoch 3/3] PASS: committed=21 state=...
[summary] PASS: cycles=3 checkpoints=3 committed=21 duplicates=3 seed=20260717
```

## HTTP API

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/health` | Worker readiness |
| `GET` | `/state` | Complete in-memory state used by the verifier |
| `POST` | `/transfers` | Commit or deduplicate a transfer |

A transfer body has this form:

```json
{
  "id": "tx-001",
  "from": "alice",
  "to": "bob",
  "amount": 100
}
```

## Scope and limitations

Rollback covers state captured inside the sandbox, including process memory
and the writable filesystem. It does not undo effects already sent to an
external database, remote service, message broker, or TCP peer. Existing TCP
connections are invalid after rollback, so the driver creates a fresh HTTP
session before reconnecting to the restored worker.
